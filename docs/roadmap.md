# NeelKIT — Feature Roadmap & Development Phases

---

## Part A — Feature roadmap

Features are grouped by what they *prove*. Anything that proves nothing was cut;
the cuts are listed at the end, with reasons, because knowing what to leave out
is the harder half.

### Tier 1 — the product (v1.0, must exist or there is no project)

| Feature | Engineering it demonstrates |
|---|---|
| Realtime mic → partial + final transcript over WebSocket | Streaming systems, VAD endpointing, scheduling around a non-streaming model |
| Batch file transcription, OpenAI-compatible | API design, interop, backwards compatibility |
| Word timestamps + per-token confidence | Reading a model's actual outputs instead of faking them (upstream discards both) |
| Transcript normalisation (dev lexicon, identifier repair) | The domain insight that makes voice commands usable at all |
| Grammar-first intent routing with LLM fallback | Latency/determinism engineering, graceful degradation |
| Tool registry + JSON-Schema descriptors | Extensibility, plugin architecture |
| Policy gate + approval flow + audit log | Security design under a genuinely adversarial condition |
| Sandboxed `fs` / `git` / `shell` / `http` / `search` tools | Systems programming, containment |
| Conversation + transcript persistence with FTS search | Data modelling, indexing |
| Web UI: mic, live transcript, approval cards, history, settings | Frontend, realtime state, accessibility |
| `nk` CLI over the public API | API completeness (proved, not claimed) |
| Prometheus metrics, OTel traces, structured logs | Observability |
| Docker + compose, CPU and CUDA images | Deployment |
| `nkbench` — RTF, latency, WER | Performance engineering with real numbers |

### Tier 2 — v1.1–1.3, each with a stated reason

| Feature | Why it earns its place | Why not in v1 |
|---|---|---|
| Session memory / context ("this file", "that error") | Makes multi-turn commands work; needs a context-resolution design | Depends on v1 turn history being real |
| Local TTS voice reply (Piper) | Closes the voice loop; browser `SpeechSynthesis` covers v1 | Another ONNX model to manage |
| MCP tool provider | Every MCP server becomes a plugin, in any language | Second trust boundary; worthless until there is a non-Go tool worth having |
| Redis-backed rate limiting + queue | Multi-process story | Single process needs no shared state |
| Postgres repository implementation | Multi-user | AD-007 |
| Remote tool runner (agent on the dev box) | Distributed systems | AD-008 — seam exists, feature does not |
| Batch/dynamic request batching in the encoder | Throughput under concurrent load | Needs a load profile to justify the complexity |

### Tier 3 — considered and deliberately deferred, with reasons

| Feature | Why deferred |
|---|---|
| **Wake word** | Needs a trained keyword-spotting model. A bad wake word that false-fires on a service that can run `go test` is worse than push-to-talk. Would ship as a separate pinned ONNX KWS model or not at all |
| **Speaker diarisation** | A different model and a different product. Not a flag |
| **Multi-language** | Parakeet v3 is multilingual, but the grammar rules, lexicon and normaliser are English-only. Shipping "supports 25 languages" while the command layer only works in one is a lie |
| **Mobile app** | The web UI is responsive and works on a phone. A native app adds no engineering story |
| **Fine-tuning on my own voice** | Real accuracy win, but it is an ML project, not this project. Honest answer: personal-vocabulary biasing in `normalize` gets most of the benefit for 1% of the work |
| **File deletion tool** | See `docs/security.md` §11 |

---

## Part B — Development phases

**Phases 1 to 11 are built.** What follows records what each phase produced and
what is still open, rather than what was planned.

---

### Phase 1 — Analysis & architecture ✅

Upstream analysis, architecture, API spec, data model, tool system, security
model, roadmap, attribution.

### Phase 2 — ASR backend ✅

`internal/audio` (WAV codec, anti-aliased resampling, mel extraction with
precomputed twiddles and a zero-allocation frame path, batch and running
normalisers, ring buffer), `internal/asr` (Engine interface) and
`internal/asr/onnx` (sessions, worker pool, warm-up, TDT decode retaining
timesteps and probabilities, long-audio windowing, seam dedup).

Verified: the FFT is proven equivalent to a naive DFT; the decode loop is
verified end to end against a synthetic ONNX model with the real checkpoint's
I/O signature; concurrency and shutdown are race-clean.

### Phase 3 — Realtime streaming ✅

`internal/vad` (Silero session plus a pure endpointing state machine) and
`internal/realtime` (incremental features, debounced partial decodes, final
decode on endpoint, priority shedding).

Verified: 30 seconds of real narration segments into four utterances; a live
WebSocket session produces streaming partials and a final within milliseconds
of the endpoint.

### Phase 4 — Assistant orchestration ✅

`internal/normalize`, `internal/intent` (grammar plus planner), `internal/llm`
(provider interface, Ollama, fake) and `internal/assistant` (the turn state
machine).

### Phase 5 — Tools ✅

`internal/tools` (contract, registry), `policy` (pure gate), `sandbox`
(`os.Root`, argv execution, allowlists), `runner`, and thirteen built-in tools
across fs, git, search, notes and shell tasks.

Verified: the adversarial sandbox suite (traversal, symlink TOCTOU,
metacharacters, credential files) passes; a property test over 5,000 randomised
inputs shows no argument can ever weaken a policy decision; the previewed
command is asserted byte-identical to the executed argv.

### Phase 6 — Persistence ✅

SQLite with embedded migrations, FTS5 search, conversations, messages,
transcripts, tool executions, append-only audit.

### Phase 7 — Frontend ✅

Next.js interface: live session with approval cards, searchable history, the
tool registry, settings. Responsive to 390 px, keyboard operable, theme-aware.

### Phase 8 — Security ✅

Hashed keys with constant-time verification, WebSocket ticket exchange, origin
allowlist on both CORS and the upgrade, rate limits, body limits, per-request
deadlines, audit log, secret redaction in config output.

### Phase 9 — Observability ✅

Dependency-free Prometheus exposition, structured JSON logs with request
correlation, `/readyz` with a per-dependency breakdown.

Not built: distributed tracing. It earns its keep across service boundaries and
this is one process; the context plumbing is already in place if that changes.

### Phase 10 — Testing ✅

Every package that carries logic has tests; the suite runs with no model and no
ONNX Runtime installed.

Done since: browser end-to-end tests (`web/e2e/`), running in CI against the
synthetic model — a read runs unprompted, a write writes nothing until approved,
denial leaves the disk untouched, and an approved path traversal is still
refused by the sandbox.

Still open: a fuzz target on the WAV parser and the WebSocket frame decoder.

### Phase 11 — Docker & deployment ✅

Multi-stage CPU image (non-root, model volume, healthcheck), web image
(standalone output), compose stack with a one-shot model downloader and an
optional Ollama profile.

Still open: the CUDA image. The execution-provider code path exists and fails
loudly rather than falling back, but it has not been run on a GPU, so it is not
claimed as working.

---

### Phase 12 — Documentation ◐

Done: README, the `docs/` set, `CHANGELOG.md` (including the bugs found by
running the system), `ATTRIBUTION.md`, `NOTICE`, `.env.example`.

Done since: `docs/openapi.yaml`, hand-written and checked against the router by
`TestOpenAPIMatchesTheRouter` — drift in either direction fails the build.
Generating it from the router was considered and rejected: a generated spec can
only describe shapes the code already has, and the descriptions are the part
worth writing. `docs/design-system.md` and `docs/performance.md` also landed.

Still open: `docs/interview-prep.md` — deliberately last, because it must be
written from the code that exists rather than from the plan, and it should cite
measured numbers rather than adjectives.

---

### Phase 13 — Performance ◐

Done as part of the build rather than after it: precomputed FFT twiddle
factors, a zero-allocation mel frame path, one backing array per spectrogram,
and a salted fast hash for API keys after PBKDF2 was measured at 20 ms per
request. The `neelkit_asr_real_time_factor` and queue-wait histograms are live.

Done since: `cmd/nkbench` with `asr`, `route`, `normalize` and `all`
subcommands, and `docs/performance.md` stating the method — discarded warm-up
pass, nearest-rank percentiles, audio decoded before timing starts.

Still open, and stated plainly: **the numbers.** The results tables in
`docs/performance.md` are empty. The harness has only ever been run against the
synthetic test model, whose figures describe the harness rather than the system.
Nothing will be published until it has been run on the real model on named
hardware. Also open: WER against a recorded corpus, and a CI benchmark
threshold.

---

## Part C — Phase 2 implementation plan (next session)

Concrete, in order:

1. `go mod init github.com/neeldas0032/NeelKIT-AI-voice-assistant`; Makefile with
   `build/run/test/lint/fmt/models`; CI workflow.
2. `internal/version` + `/version` endpoint. Smallest possible end-to-end slice:
   prove the build, the router and CI before any ML.
3. `internal/config`: typed struct, `NEELKIT_*` env + flags with CLI > env >
   default, `Validate()`, redacted `String()`.
4. `internal/audio`: WAV reader (with a fuzz target), anti-aliased resampler,
   `MelExtractor` with precomputed twiddle tables and a reusable frame scratch
   buffer, batch normaliser. **Tests first here** — this is where a silent bug
   ruins every transcript downstream.
5. `internal/asr/engine.go`: the `Engine` interface + `fake` implementation.
   Everything above ASR becomes testable at this commit.
6. `internal/asr/onnx`: model loading, vocab, shared encoder, worker pool over an
   explicit semaphore + owned slice (fixing the upstream shutdown leak), warm-up
   pass at boot, TDT decode retaining `(tokenID, timestep, prob)`.
7. `internal/asr/postprocess`: tokens → text + word spans + confidence.
8. `internal/httpapi`: middleware chain, `POST /v1/audio/transcriptions`,
   `/healthz`, `/readyz`, `/version`, problem+json errors.
9. `internal/observability`: slog JSON handler with request id, first metrics.
10. `cmd/neelkitd`: wire, serve, ordered graceful shutdown.
11. `cmd/nkbench`: `rtf` subcommand.
12. `testdata/`: 3 short WAVs + golden transcripts; a frozen mel reference matrix.
13. `docs/development.md` + README quickstart.

**Definition of done for Phase 2:** a clean clone, `make models && make run`,
one `curl`, a correct transcript with word timings — and `go test ./...` green
on a machine with no ONNX Runtime installed.
