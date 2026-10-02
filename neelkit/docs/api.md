# NeelKIT — API Specification

Base URL: `http://127.0.0.1:8420` (default; bound to loopback unless
`NEELKIT_HTTP_ADDR` says otherwise).

All REST responses are JSON. All errors use one shape, modelled on RFC 9457:

```json
{
  "error": {
    "type":     "invalid_request | authentication | permission_denied | not_found |
                 rate_limited | overloaded | unsupported_media | internal",
    "code":     "workspace_escape",
    "message":  "path resolves outside the workspace root",
    "request_id": "01J9ZQ2K7X8N",
    "details":  { "path": "../../etc/passwd" }
  }
}
```

`request_id` is echoed in the `X-Request-Id` response header and appears in
every log line and trace for that request. It is the first thing to quote in a
bug report.

---

## Authentication

**Scheme.** `Authorization: Bearer nk_<key_id>_<secret>`

- `key_id` — 12 chars, base32, stored plaintext, **indexed**. Lookup is by id,
  so verification is one indexed row read, not a table scan of hashes.
- `secret` — 32 bytes of `crypto/rand`, shown **once** at creation.
- Storage — argon2id hash of the secret. Verification is
  `subtle.ConstantTimeCompare` against the recomputed hash.

**Unauthenticated endpoints:** `GET /healthz`, `GET /readyz`, and `GET /metrics`
when `NEELKIT_METRICS_PUBLIC=true` (default: authenticated).

**First run.** If no keys exist, `neelkitd` generates one, prints it once, and
writes only the hash. There is no "auth disabled" mode — upstream's
`if apiKey == "" { passthrough }` is a footgun on a service that can execute
shell commands.

### WebSocket authentication

Browsers cannot set headers on a `WebSocket` handshake. Putting the API key in
the query string puts it in access logs, proxy logs, and browser history.

Instead: exchange a ticket.

```
POST /v1/realtime/tickets     (Bearer auth)
→ 201 { "ticket": "rt_…", "expires_at": "…", "ttl_seconds": 30 }

GET /v1/realtime?ticket=rt_…  (Upgrade: websocket)
```

Tickets are single-use, 30 s TTL, bound to the issuing key and — when
`NEELKIT_STRICT_TICKETS=true` — to the client IP. The `Origin` header is
checked against the CORS allowlist on upgrade; a WebSocket handshake is not
subject to CORS preflight, so this check is mandatory, not optional.

---

## Rate limiting

Token bucket per API key, in-memory (`golang.org/x/time/rate`), separate buckets
per cost class:

| Class | Endpoints | Default |
|---|---|---|
| `asr` | transcriptions, realtime sessions | 30 / min, burst 10 |
| `assistant` | turns, approvals | 60 / min, burst 20 |
| `read` | conversations, transcripts, tools, settings | 300 / min, burst 100 |

Responses carry `RateLimit-Limit`, `RateLimit-Remaining`, `RateLimit-Reset`.
Exceeding returns **429** with `Retry-After`.

Redis-backed limiting is an interface implementation (`ratelimit.Store`), not a
rewrite — but it is not in v1, because a single-process local tool does not
need shared state.

---

# Endpoints

## 1. `POST /v1/audio/transcriptions`

Batch transcription. **Deliberately OpenAI-Whisper-compatible** so existing
tooling works unchanged; NeelKIT extensions are additive and default to off.

**Auth:** required · **Class:** `asr` · **Body:** `multipart/form-data`, ≤ 50 MB

| Field | Type | Required | Notes |
|---|---|---|---|
| `file` | file | yes | WAV parsed in-process; anything else via ffmpeg |
| `model` | string | no | accepted, one model exists; unknown values 400 (upstream silently ignores) |
| `language` | string | no | `en` default |
| `response_format` | enum | no | `json` (default), `text`, `srt`, `vtt`, `verbose_json` |
| `timestamps` | enum | no | **NeelKIT**: `none` (default), `segment`, `word` |
| `confidence` | bool | no | **NeelKIT**: include per-segment confidence |
| `normalize` | bool | no | **NeelKIT**: apply the dev-lexicon pipeline (default `false` on this endpoint) |

**Validation.** Missing `file` → 400 `invalid_request/missing_field`. Body over
limit → 413. Undecodable audio → 400 `unsupported_media`. Audio shorter than
100 ms → 200 with empty text (matches upstream; it is not an error).

**Example**

```bash
curl -sS http://127.0.0.1:8420/v1/audio/transcriptions \
  -H "Authorization: Bearer $NEELKIT_API_KEY" \
  -F file=@meeting.m4a \
  -F timestamps=word -F confidence=true
```

```json
{
  "text": "deploy the staging branch after the tests pass",
  "duration": 3.84,
  "language": "en",
  "model": "parakeet-tdt-0.6b-v3-int8",
  "confidence": 0.94,
  "words": [
    { "word": "deploy",  "start": 0.08, "end": 0.48, "confidence": 0.98 },
    { "word": "the",     "start": 0.48, "end": 0.56, "confidence": 0.91 }
  ],
  "metrics": { "rtf": 0.31, "queue_wait_ms": 0, "inference_ms": 1190 }
}
```

> Word timings come from the TDT decoder's per-token timesteps × 8 subsampling
> × 10 ms hop = **80 ms resolution**. Upstream computes these and discards them.
> `confidence` is the geometric mean of the softmax probability of each emitted
> token — a real measurement, not the hardcoded `-0.5` upstream returns.

**Errors:** 400, 401, 413, 415, 429, **503 `overloaded`** (+`Retry-After`), 500.

---

## 2. `GET /v1/realtime` — WebSocket

The realtime voice channel. Binary frames are audio; text frames are JSON.

**Handshake:** `?ticket=rt_…`, `Origin` must be allowlisted.

**Audio format:** 16 kHz, mono, signed 16-bit little-endian PCM, 20 ms frames
(640 bytes). Non-conforming sizes are accepted and buffered; the ring buffer
does not care about frame boundaries.

### Client → server (text)

```jsonc
// must be the first message
{ "type": "session.configure",
  "conversation_id": null,            // null → create new
  "mode": "vad" | "push_to_talk",
  "partials": true,
  "auto_execute": false,              // never true for Execute-risk tools
  "hangover_ms": 600,
  "language": "en" }

{ "type": "session.commit" }          // force endpoint now (PTT release)
{ "type": "session.cancel" }          // discard current utterance
{ "type": "tool.approve", "call_id": "…", "remember": false }
{ "type": "tool.deny",    "call_id": "…", "reason": "wrong branch" }
{ "type": "text.input",   "text": "…" }   // type instead of speak, same pipeline
{ "type": "ping" }
```

### Server → client (text)

| Event | Payload | Meaning |
|---|---|---|
| `session.created` | `session_id`, `conversation_id`, `model`, `limits` | ready for audio |
| `vad.speech_started` | `at_ms` | endpointer entered speech |
| `vad.speech_stopped` | `at_ms`, `duration_ms` | hangover elapsed |
| `transcript.partial` | `text`, `utterance_id`, `stable: false` | may be rewritten |
| `transcript.final` | `text`, `raw_text`, `utterance_id`, `confidence`, `words[]` | stable; `raw_text` is pre-normalisation |
| `intent.resolved` | `intent`, `source: grammar\|llm`, `confidence`, `plan[]` | what it decided to do |
| `tool.permission_required` | `call_id`, `tool`, `risk`, `args`, `preview` | **blocks until answered** |
| `tool.started` / `tool.output` / `tool.completed` | `call_id`, `chunk`/`result`, `duration_ms` | streamed stdout for long tools |
| `assistant.message` | `text`, `message_id` | the reply |
| `stream.lagged` | `dropped` | client too slow; partials were dropped |
| `stream.overloaded` | `retry_after_ms` | server shed this partial |
| `error` | standard error object | non-fatal unless `fatal: true` |

**Lifecycle.** Idle sessions close after 10 min. Server pings every 30 s; two
missed pongs close with 1001. The server never sends a permission prompt it
will not wait for: an unanswered prompt times out after
`NEELKIT_APPROVAL_TIMEOUT` (120 s) and resolves as **deny**.

---

## 3. `POST /v1/assistant/turns`

The text-in equivalent of a spoken turn — used by `nk ask`, by tests, and by
the UI's typing fallback. Same pipeline as speech from `normalize` onward.

```jsonc
// request
{ "conversation_id": "conv_…",   // optional
  "text": "create a git branch called feature-auth",
  "execute": false,              // false → plan only, nothing runs
  "context": { "cwd": "services/api" } }
```

```jsonc
// 200
{ "turn_id": "turn_…",
  "conversation_id": "conv_…",
  "intent": { "name": "git.branch_create", "source": "grammar", "confidence": 1.0 },
  "plan": [
    { "call_id": "call_…", "tool": "git.branch_create",
      "args": { "name": "feature-auth" },
      "risk": "write", "decision": "confirm",
      "preview": "git switch -c feature-auth  (in ~/dev/api)" }
  ],
  "status": "awaiting_approval",   // executed | awaiting_approval | denied | failed
  "message": null }
```

`execute: false` is the **dry-run contract**: the planner runs, the policy runs,
the preview is rendered, and nothing touches the machine. It is how the CLI's
`--dry-run` works and how the integration tests assert policy decisions without
side effects.

**Errors:** 400 (empty text, unknown conversation), 401, 422 (`plan_invalid` —
the LLM returned a tool call that fails schema validation), 429, 503, 500.

---

## 4. `POST /v1/assistant/turns/{turn_id}/approvals`

```jsonc
{ "call_id": "call_…", "decision": "approve" | "deny",
  "remember": false, "reason": null }
```

Returns the updated turn with `status: executed` and the tool results.
Idempotent on `call_id`: re-approving an already-resolved call returns **409
`already_resolved`** rather than executing twice. `remember: true` writes a
scoped policy override (tool + argument shape, never a blanket allow) to
`settings`.

---

## 5. Conversations & transcripts

| Method | Path | Purpose |
|---|---|---|
| `GET` | `/v1/conversations?limit=&cursor=&q=` | list, newest first, cursor-paginated |
| `GET` | `/v1/conversations/{id}` | metadata + counts |
| `GET` | `/v1/conversations/{id}/messages?limit=&cursor=` | full turn history incl. tool calls |
| `PATCH` | `/v1/conversations/{id}` | rename, pin |
| `DELETE` | `/v1/conversations/{id}` | **hard delete**, cascades to messages/transcripts; audit rows are retained by design and the response says so |
| `GET` | `/v1/transcripts/search?q=&from=&to=&limit=` | FTS5 over all finals, snippet-highlighted |
| `GET` | `/v1/conversations/{id}/export?format=json\|md\|srt\|vtt\|txt` | privacy: take your data out |

Pagination is **cursor-based** (opaque, encodes `created_at`+`id`), not
offset-based: offsets skip or duplicate rows when new turns land mid-scroll,
which in a live-appending log is the common case, not the edge case.

---

## 6. Tools

| Method | Path | Purpose |
|---|---|---|
| `GET` | `/v1/tools` | registry introspection: name, description, JSON schema, risk, enabled |
| `POST` | `/v1/tools/{name}:invoke` | direct invocation — **still policy-gated**; for the CLI and for testing |

`GET /v1/tools` is the same data the LLM planner receives. That is deliberate:
what the UI shows you it can do is literally what the model was told it can do.

---

## 7. Settings

`GET /v1/settings` · `PATCH /v1/settings`

```jsonc
{ "asr":       { "device": "cpu", "workers": 4, "partials": true, "hangover_ms": 600 },
  "llm":       { "provider": "ollama", "model": "qwen2.5:7b-instruct", "timeout_ms": 20000 },
  "policy":    { "read": "allow", "write": "confirm",
                 "execute": "confirm", "network": "confirm" },
  "workspace": { "root": "/home/neel/dev", "read_only": false },
  "privacy":   { "store_audio": false, "store_transcripts": true,
                 "retention_days": 90 } }
```

`llm.api_key` is **write-only**: `PATCH` accepts it, `GET` returns
`"sk-…" → "***"`. Same for any secret. A settings endpoint that echoes secrets
back is how secrets end up in browser devtools and screen recordings.

---

## 8. Operational endpoints

| Path | Auth | Semantics |
|---|---|---|
| `GET /healthz` | none | **Liveness.** 200 if the process is running. Never checks dependencies — a liveness probe that fails on a slow dependency causes a restart loop. |
| `GET /readyz` | none | **Readiness.** 200 only when models are loaded and warmed, the DB is migrated and reachable, and the queue is not saturated. Returns 503 with a per-check breakdown otherwise. |
| `GET /metrics` | key (default) | Prometheus text format |
| `GET /v1/models` | key | Loaded model + precision + device |
| `GET /version` | none | version, commit, build date, Go version |

`/readyz` body when degraded:

```json
{ "ready": false,
  "checks": { "models": "ok", "database": "ok",
              "warmup": "ok", "queue": "saturated (depth 8/8)" } }
```

---

## 9. Client tooling

- **OpenAPI 3.1** spec generated into `docs/openapi.yaml` and checked in CI
  against the router (a drifted spec fails the build).
- **Go SDK** `pkg/neelkit` — typed client for every endpoint plus a
  `RealtimeSession` with typed events. Used by `nk`.
- **TypeScript types** generated from the OpenAPI spec into
  `web/lib/api-types.ts` — the browser and the server cannot disagree about a
  field name without the build failing.
