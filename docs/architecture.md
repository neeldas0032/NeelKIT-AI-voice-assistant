# NeelKIT — Architecture

**NeelKIT** — *Neel's Knowledge & Intent Toolkit*. A local-first voice
assistant for developers: speak, and it does the thing.

- Server binary: `neelkitd`
- CLI client: `nk`
- Benchmark harness: `nkbench`
- Go SDK: `github.com/neeldas0032/NeelKIT-AI-voice-assistant/pkg/neelkit`

---

## 0. Product thesis

> Dictation turns speech into text. NeelKIT turns speech into **verified,
> reversible actions on your machine**, with every risky step gated by an
> explicit approval.

Three properties define it and every decision below serves one of them:

1. **Local-first.** With Ollama installed, nothing leaves the machine — audio,
   transcripts, code context, or commands. No API key is required to run it.
2. **Low latency.** A partial transcript on screen inside ~400 ms of speaking;
   a final transcript within ~300 ms of the endpoint being detected.
3. **Safe by construction.** Nothing that writes, deletes or executes runs
   without passing a policy gate, and every attempt is in an audit log whether
   it was allowed or not.

---

## 1. System architecture

```
┌─────────────────────────────────────────────────────────────────────────┐
│  BROWSER  (Next.js 15 · TypeScript · Tailwind)                          │
│                                                                         │
│   getUserMedia ─► AudioWorklet ─► 16 kHz mono Int16 ─► 20 ms frames     │
│        ▲                                                   │            │
│        │ events (JSON text frames)      audio (binary frames)           │
└────────┼───────────────────────────────────────────────────┼────────────┘
         │                    WebSocket /v1/realtime         ▼
┌────────┴───────────────────────────────────────────────────────────────┐
│  neelkitd  (single Go binary)                                          │
│                                                                        │
│  ┌── EDGE ────────────────────────────────────────────────────────┐    │
│  │  recover → requestID → otel → logging → CORS → authn →         │    │
│  │  rate-limit → body-limit → timeout → route                     │    │
│  └───────────────────┬──────────────────────────┬─────────────────┘    │
│                      │ REST                     │ WS upgrade           │
│                      ▼                          ▼                      │
│              ┌───────────────┐        ┌──────────────────────┐         │
│              │ httpapi       │        │ realtime.Session     │         │
│              │ (batch paths) │        │  reader│vad│asr│wtr  │         │
│              └───────┬───────┘        └──────────┬───────────┘         │
│                      │                           │                     │
│  ┌───────────────────▼───────────────────────────▼─────────────────┐   │
│  │  AUDIO   ring buffer · incremental mel · per-slice normaliser   │   │
│  └───────────────────┬─────────────────────────────────────────────┘   │
│                      ▼                                                 │
│  ┌─────────────────────────────────────────────────────────────────┐   │
│  │  VAD  Silero 512-sample windows → endpointing state machine     │   │
│  │       idle → speech → hangover → endpoint                       │   │
│  └───────────────────┬─────────────────────────────────────────────┘   │
│                      ▼                                                 │
│  ┌─────────────────────────────────────────────────────────────────┐   │
│  │  ASR ENGINE                                                     │   │
│  │   admission queue (bounded) ─► encoder session (shared, warm)   │   │
│  │                             ─► decoder worker pool (N, pinned)  │   │
│  │   emits: partial (unstable) · final (stable) · confidence       │   │
│  │          · word timestamps                                      │   │
│  └───────────────────┬─────────────────────────────────────────────┘   │
│                      ▼                                                 │
│  ┌─────────────────────────────────────────────────────────────────┐   │
│  │  NORMALIZE  whitespace → numbers/units → dev lexicon →          │   │
│  │             identifier repair ("feature dash auth"→feature-auth)│   │
│  │             → punctuation & capitalisation                      │   │
│  └───────────────────┬─────────────────────────────────────────────┘   │
│                      ▼                                                 │
│  ┌─────────────────────────────────────────────────────────────────┐   │
│  │  INTENT ROUTER                                                  │   │
│  │   1. grammar.Matcher  — compiled patterns, slot extraction, 0ms │   │
│  │   2. llm.Planner      — Provider iface: ollama│anthropic│openai │   │
│  │                         emits a ToolPlan (JSON, schema-checked) │   │
│  └───────────────────┬─────────────────────────────────────────────┘   │
│                      ▼                                                 │
│  ┌─────────────────────────────────────────────────────────────────┐   │
│  │  POLICY GATE   risk(tool) × settings → allow │ confirm │ deny   │   │
│  │                confirm ⇒ tool.permission_required over WS       │   │
│  └───────────────────┬─────────────────────────────────────────────┘   │
│                      ▼                                                 │
│  ┌─────────────────────────────────────────────────────────────────┐   │
│  │  TOOL RUNNER   registry lookup → schema validate → sandbox →    │   │
│  │                exec with timeout → capture → audit              │   │
│  │   builtin: fs · git · shell · http · search · notes · sql · docs│   │
│  └───────────────────┬─────────────────────────────────────────────┘   │
│                      ▼                                                 │
│  ┌─────────────────────────────────────────────────────────────────┐   │
│  │  COMPOSER  tool results + transcript → assistant message        │   │
│  └───────────────────┬─────────────────────────────────────────────┘   │
│                      ▼                                                 │
│  ┌─────────────────────────────────────────────────────────────────┐   │
│  │  STORAGE  SQLite (WAL) · FTS5 transcript search · migrations    │   │
│  │  OBSERVABILITY  slog+traceID · Prometheus · OTel spans          │   │
│  └─────────────────────────────────────────────────────────────────┘   │
└────────────────────────────────────────────────────────────────────────┘
         │                                    │
         ▼                                    ▼
   Ollama (localhost:11434)            Workspace root (sandboxed)
```

### Why this shape

The pipeline is **eight stages with one direction of data flow and one
direction of control flow**. Each stage has a single input type, a single
output type, and no knowledge of its neighbours beyond those types. That is
what makes it testable: `normalize` is a pure function, `grammar.Match` is a
pure function, `policy.Decide` is a pure function. The only stages that touch
the outside world are `asr` (ONNX), `llm` (HTTP), `tools` (exec/fs) and
`storage` (SQLite) — and each of those is behind an interface with a fake.

---

## 2. Key architectural decisions

Every one of these follows the same template, because every one of these is an
interview question.

### AD-001 — Incremental decoding over an offline model

**Problem.** Parakeet TDT is full-context. It cannot be fed frames as they
arrive; it needs a window, and it attends over all of it. But a voice assistant
must show the user *something* while they are still speaking, or it feels
broken.

**Decision.** Do not fake streaming ASR. Build a scheduler around the offline
model:

- Mel features are computed once, incrementally, per 160-sample hop, into a
  ring buffer. Nothing is ever recomputed.
- VAD drives an endpointing state machine. Speech start requires
  `P(speech) > 0.5` for 3 consecutive 32 ms windows (debounces clicks); speech
  end requires `P(speech) < 0.35` for a `hangover` of 600 ms (default,
  configurable — this is the single biggest lever on perceived latency).
- While speech is active, a **partial decode** runs at most every 400 ms over
  the trailing `min(utterance, 12 s)` of mel frames. Partials are marked
  unstable and may be rewritten.
- On endpoint, a **final decode** runs over the full utterance. Only finals go
  to `normalize` → `intent`.

**Alternatives considered.**
*Re-export Parakeet with cache-aware streaming* — correct long-term answer,
requires NeMo + a GPU + model surgery, out of scope and unverifiable on a
CPU laptop. *Swap to a natively streaming model (e.g. a small Conformer-CTC)* —
loses the accuracy that motivated Parakeet. *Fixed-interval chunk decode with
no VAD* — cuts words in half and burns CPU during silence.

**Trade-offs.** Partial decodes are redundant compute: an utterance of length
*T* costs roughly `T/0.4` partial passes over a growing window, capped by the
12 s trailing limit. Measured cost is bounded and it is the price of
responsiveness. It is disclosed in `docs/performance.md`, not hidden.

**What can fail.** Endpointing too eagerly truncates a thinking pause
mid-sentence. Mitigation: hangover is configurable, and a `session.commit`
control message lets the client force an endpoint (push-to-talk mode).

**How it is tested.** The endpointing state machine is a pure function over a
probability sequence — table-driven tests with synthetic VAD traces covering
clipped starts, breath pauses, and continuous speech. The scheduler is tested
with a fake ASR engine that records the windows it was asked to decode.

**How it scales.** Partial decodes are the first thing shed under load: the
admission queue drops partials before finals, so a saturated server degrades to
final-only transcription rather than failing.

---

### AD-002 — Whole-slice feature normalisation (revised)

**Problem.** Parakeet was trained with per-feature mel normalisation computed
over the whole utterance. A live stream never has "the whole utterance" while
the user is still speaking.

**First decision, now reversed.** Live audio used a running, exponentially
decaying normaliser so features could be normalised before the utterance
ended. Running it on a CPU laptop showed the flaw: by the time a final decode
ran, the running statistics were dominated by the last second of audio — the
hangover silence that ends every utterance — so speech was scaled against
silence and the encoder saw input unlike its training data.

**Decision.** Every decode request already carries a complete slice of audio:
an endpointed utterance, the trailing window of a preview, or an uploaded file.
So every request is normalised with whole-slice per-feature statistics, exactly
as the model was trained. Nothing needs the future, because nothing is decoded
before its slice is cut.

**Trade-off.** A preview's statistics cover only its trailing window, so a
preview can differ slightly from the final. Previews are labelled unstable and
are never acted on; only the final reaches the assistant.

---

### AD-003 — WebSocket for realtime, REST for everything else

**Problem.** The realtime path needs audio flowing up *and* events flowing
down — including a permission prompt that blocks execution until the user
answers.

**Decision.** One WebSocket at `/v1/realtime`, binary frames for PCM, text
frames for JSON control/events. Everything stateless stays REST.

**Alternatives.** *SSE + a separate POST channel* (what upstream implies) —
two connections, no correlation, and the permission round-trip becomes a poll.
*gRPC streaming* — better typed, but needs grpc-web + a proxy for browsers,
which is infrastructure for a local-first tool. *WebTransport* — genuinely
better for lossy audio, not yet universal in Safari.

**Concurrency rule.** One goroutine per connection owns *all* writes. Every
other goroutine sends on a buffered `chan outbound`. This is the idiomatic
answer to "WebSocket connections do not support concurrent writers" and it also
gives a natural place to drop low-priority events (partials) when the client is
slow, instead of blocking the decoder.

**Backpressure.** The outbound channel is bounded. On overflow, partials are
dropped and a `stream.lagged` event is sent; finals, permission prompts and
errors are never dropped.

---

### AD-004 — Grammar first, LLM second

**Problem.** Sending every utterance to an LLM costs 300–2000 ms and makes the
assistant non-deterministic for commands that are not ambiguous at all.

**Decision.** `intent.Router` tries a compiled grammar first. Rules are
declarative, live in `internal/intent/grammar/rules.go`, and extract typed
slots:

```
create (a )?(git )?branch (called |named )?<name:identifier>
  → tool: git.branch_create   {name: $name}

what changed (since|in) <ref:gitref>
  → tool: git.diff            {ref: $ref}
```

A miss falls through to `llm.Planner`, which sees the tool descriptors as a
JSON schema and returns a `ToolPlan`.

**Rationale.** The ~20 commands actually used daily become deterministic,
zero-token, sub-millisecond and unit-testable. The long tail still works.
It also degrades gracefully: with no LLM configured at all, NeelKIT is still a
useful voice command runner.

**Trade-off.** Two code paths to maintain, and a grammar that silently
mis-matches is worse than a miss. Mitigation: every rule has positive *and*
negative test cases, and the resolved intent is always shown in the UI with its
source (`grammar` or `llm`) and its confidence before anything executes.

---

### AD-005 — Tools are registered, not wired

**Problem.** Adding a capability must not mean editing the assistant.

**Decision.** A `Tool` interface + a registry. A tool package registers itself
in `init()`; `cmd/neelkitd` blank-imports the set it ships with. The planner
builds its schema *from the registry at runtime*, so a newly registered tool is
immediately callable by both the grammar layer and the LLM with zero changes to
the orchestrator. Full detail in `docs/tool-system.md`.

**Why this pattern.** It is exactly how `database/sql` drivers and
`image` decoders work in the standard library — a Go reviewer recognises it
instantly, and it keeps the core free of a `switch` over tool names.

---

### AD-006 — Every effect is policy-gated and audited

**Problem.** "Run the test suite" and "delete the branch" are the same kind of
sentence to a speech recogniser, and ASR makes mistakes.

**Decision.** Every tool declares a `RiskLevel`
(`Read` → `Write` → `Execute` → `Network`). A policy table maps
(risk × user settings × argument inspection) to `Allow | Confirm | Deny`.
`Confirm` suspends the turn, emits `tool.permission_required`, and resumes only
on an explicit approval carrying the same `call_id`. An `audit_events` row is
written **before** execution and updated after — so a crash mid-execution still
leaves evidence. Details and the destructive-operation list in
`docs/security.md`.

---

### AD-007 — SQLite, not Postgres

**Problem.** The system needs durable conversations, transcripts, audit logs
and searchable history.

**Decision.** SQLite in WAL mode via `mattn/go-sqlite3`, behind repository
methods, with numbered SQL migrations embedded by `//go:embed` and applied in a
transaction each. FTS5 provides transcript search with no extra service.

A pure-Go driver was the original intent, to avoid a second C toolchain — but
ONNX Runtime already forces CGO, so the constraint does not exist, and the CGO
driver ships FTS5 behind a build tag (`sqlite_fts5`, wired into every Make
target so nobody has to remember it).

**Rationale.** This is a single-user local-first tool. A Postgres dependency
would mean "install Docker before you can try my project", which kills the
demo. WAL mode handles one writer + many readers, which is exactly this
workload. FTS5 gives full-text transcript search for free — a feature that
would otherwise justify Elasticsearch.

**Alternatives.** *Postgres* — needed only for multi-writer or network access;
the repository interface means swapping it is a new implementation, not a
rewrite. *BoltDB/Badger* — no ad-hoc queries, no FTS. *Flat JSON files* —
no transactions on the audit log, which is the one place that must not lose
writes.

**Trade-off / failure mode.** SQLite will not survive two `neelkitd` processes
writing concurrently. Mitigated by a PID lockfile on the data directory and a
clear error. Documented, not discovered.

---

### AD-008 — Server-side, workspace-rooted tool execution

**Decision.** Tools execute in the `neelkitd` process, confined to one
configured `NEELKIT_WORKSPACE_ROOT`. Filesystem access goes through Go 1.24+
`os.Root`, which resolves paths *inside the kernel* relative to a directory
handle and refuses traversal — a stronger guarantee than string-prefix checks,
which symlinks defeat.

**Why not a remote agent daemon.** A separate agent talking to a hosted server
is a better story and a much larger attack surface: a second auth boundary, a
command channel to a developer machine, replay concerns. It is not needed for a
local-first tool. But the seam is designed in: `tools.Runner` is an interface,
and `LocalRunner` is one implementation. A `RemoteRunner` speaking the same
`ToolCall`/`ToolResult` protocol over the existing WebSocket is a roadmap item,
not a refactor.

---

### AD-009 — CPU-first, GPU as a configured provider

**Decision.** int8 quantised models on the CPU execution provider are the
default and the benchmarked path. CUDA exists behind `NEELKIT_ASR_DEVICE=cuda`
and a separate image, and **fails loudly at startup if it cannot initialise**
— never silently falling back to CPU, because a GPU box quietly running CPU
inference is the worst possible failure mode.

**Consequence.** Every latency number published in `docs/performance.md` is a
CPU number, measured on named hardware, with the RTF methodology stated.

---

### AD-010 — Go for the runtime, TypeScript for the UI, no Python

**Why Go here specifically** (not "because I like Go"):

- **CGO to ONNX Runtime** — the C API binding already exists and is
  maintained; the audio path can write directly into ORT tensor memory with no
  marshalling.
- **Goroutines map onto the pipeline** — reader / VAD / scheduler / writer per
  session is four goroutines and two channels, not a thread pool and a future
  library.
- **Predictable memory** — audio buffers are preallocated and reused; the
  decode loop allocates nothing per timestep. A GC'd language with arenas the
  programmer controls is the right middle ground here.
- **One binary** — `neelkitd` plus a models directory is the whole deployment.
  No virtualenv, no CUDA-matched wheels.

Python would be the obvious choice for the ML side and is deliberately absent:
there is no training here, only inference against a fixed ONNX graph, and the
hard problems are concurrency, latency and safety.

---

## 3. Technology stack

| Layer | Choice | Why not the obvious alternative |
|---|---|---|
| Server language | **Go 1.25** | See AD-010 |
| HTTP | **net/http** + Go 1.22 routing patterns | chi/gin add a dependency for `GET /v1/x/{id}` routing the stdlib now does |
| WebSocket | **`github.com/coder/websocket`** | Context-aware API, no `SetReadDeadline` juggling; gorilla is archived-then-revived and heavier |
| Inference | **ONNX Runtime** via `yalue/onnxruntime_go` | Same as upstream; the only maintained Go binding |
| ASR model | **Parakeet TDT 0.6B v3 (int8)** | Upstream's choice, and correct: 0.6B, strong English WER, TDT skips frames |
| VAD | **Silero VAD v6.2.1 (ONNX, MIT)** | 1.8 MB, no training, already ONNX — reused from upstream's Docker pin |
| Datastore | **SQLite (WAL) + FTS5**, `mattn/go-sqlite3` (built with `-tags sqlite_fts5`) | See AD-007. CGO is already required by the ONNX binding, so a CGO driver costs nothing extra and brings FTS5 |
| Migrations | numbered SQL + embedded `//go:embed` | No runtime migration tool to install |
| LLM | **Provider interface**; Ollama default | See AD-004; Anthropic/OpenAI are config, not code changes |
| Metrics | **Prometheus text format, implemented directly** (`internal/observability/metrics`) | The official client brings six modules for the fifteen instruments used here. Counters, gauges and labelled histograms are a few hundred readable lines; swapping the library back in is contained behind this package |
| Tracing | **Not implemented.** Request correlation is carried by `request_id` through logs, metrics and audit rows | Distributed tracing earns its keep across service boundaries; this is one process. The context plumbing a tracer needs is already in place |
| Logging | **`log/slog`** JSON, request/session/trace IDs | stdlib |
| Frontend | **Next.js 16 (App Router) + React 19 + TypeScript + Tailwind 4** | React 19 `use` + server components for history pages, client components for the live session |
| Audio capture | **AudioWorklet** (not ScriptProcessor) | ScriptProcessor is deprecated and runs on the main thread — audible glitches |
| FE state | React state plus an imperative session client | Audio capture and a socket are long-lived and imperative; expressing them as effects produces reconnect loops on every render. No state library was needed |
| FE tests | **Vitest** + Testing Library + **Playwright** | |
| CI | GitHub Actions: gofmt, vet, `go test -race`, build, `govulncheck`, web typecheck and build, docker build | |

---

## 4. Repository structure

```
neelkit/
├── cmd/
│   ├── neelkitd/            # server: flags → config → wire → serve. ~150 LOC.
│   │   └── plugins.go       # blank imports that decide which tools ship
│   ├── nk/                  # CLI. Talks HTTP only; imports pkg/neelkit.
│   └── nkbench/             # benchmark + WER harness
│
├── internal/
│   ├── config/              # typed Config, env+flag binding, Validate(), redacted String()
│   ├── version/             # ldflags-injected version/commit/date
│   │
│   ├── httpapi/             # TRANSPORT ONLY — no business logic
│   │   ├── router.go        # route table
│   │   ├── dto.go           # wire types; never reused as domain types
│   │   ├── errors.go        # problem+json mapping from domain errors
│   │   ├── middleware/      # recover, reqid, otel, log, cors, authn, ratelimit, timeout
│   │   └── realtime/        # WS session: reader, writer, event protocol
│   │
│   ├── audio/               # ring buffer, WAV codec, anti-aliased resample,
│   │   │                    # streaming mel (precomputed twiddles), per-slice normaliser
│   │   └── testdata/
│   ├── vad/                 # Silero session + endpointing state machine (pure)
│   ├── asr/
│   │   ├── engine.go        # Engine interface — the seam every layer above uses
│   │   ├── onnx/            # sessions, worker pool, warm-up, TDT decode
│   │   ├── chunk/           # long-audio planner + boundary oracles + seam dedup (batch)
│   │   └── schedule/        # realtime partial/final decode scheduler
│   │
│   ├── normalize/           # pure: rules pipeline, dev lexicon, identifier repair
│   ├── intent/
│   │   ├── router.go        # grammar → llm fallback
│   │   ├── grammar/         # compiled patterns + slot extraction
│   │   └── planner/         # LLM-backed planner, schema-validated output
│   ├── llm/                 # Provider interface + ollama/, anthropic/, openai/, fake/
│   │
│   ├── tools/
│   │   ├── tool.go          # Tool, Descriptor, RiskLevel, Result
│   │   ├── registry/        # Register/Lookup/Descriptors, duplicate detection
│   │   ├── policy/          # Allow|Confirm|Deny decision (pure)
│   │   ├── sandbox/         # os.Root fs, argv exec, SSRF-guarded HTTP client
│   │   ├── runner/          # Runner interface + LocalRunner
│   │   └── builtin/         # fs/ git/ shell/ http/ search/ notes/ sql/ docs/
│   │
│   ├── assistant/           # turn orchestrator: the state machine that ties it together
│   ├── storage/
│   │   ├── migrations/      # 0001_init.sql … (embedded)
│   │   ├── sqlite/          # repository implementations
│   │   └── repo.go          # interfaces
│   ├── auth/                # key generation, argon2id hashing, ticket exchange
│   └── observability/       # metrics registry, tracer, slog handler with context IDs
│
├── pkg/
│   └── neelkit/             # PUBLIC typed SDK: client, options, event types
│
├── web/                     # Next.js app
│   ├── app/                 # /(session) /history /tools /settings
│   ├── components/
│   ├── lib/                 # generated API types, WS client, audio worklet
│   └── e2e/                 # Playwright
│
├── deploy/
│   ├── Dockerfile           # CPU, multi-stage, non-root, distroless-ish
│   ├── Dockerfile.cuda
│   ├── docker-compose.yml
│   └── nginx/               # reverse-proxy reference (WS upgrade, buffering off)
│
├── docs/                    # this directory
├── scripts/                 # fetch-models.sh, gen-openapi.sh, seed-fixtures.sh
├── testdata/                # golden audio + expected transcripts + WER references
├── .github/workflows/
├── ATTRIBUTION.md           # what is upstream-derived and what is mine
├── CHANGELOG.md
├── Makefile
└── .env.example
```

### Why each boundary exists

- **`cmd/` is thin.** Binaries parse flags, build a config, wire dependencies,
  and hand off. Nothing testable lives there.
- **`internal/` is everything.** Nothing is importable by outsiders unless it
  was a deliberate promotion to `pkg/`. Compiler-enforced API surface.
- **`httpapi` never contains logic.** Handlers decode a DTO, call one
  application method, and encode. That is what makes the CLI and the browser
  provably equivalent clients.
- **`pkg/neelkit` is the only public package** and it is *used by `nk`*. If the
  CLI can do it, the SDK can do it, therefore the API can do it. A CLI that
  reaches into `internal/` proves nothing about the API.
- **`asr.Engine` is an interface in the parent package**, implemented by
  `asr/onnx`. Everything above `asr` is testable without ONNX Runtime
  installed — which is why CI can run the whole suite on a plain ubuntu runner.
- **`normalize`, `grammar`, `policy`, `vad`'s state machine and `chunk`'s
  planner are pure.** No I/O, no clock, no randomness. They carry most of the
  interesting logic and therefore most of the tests.
- **`sandbox` is separate from `builtin`** so the containment rules are tested
  once, adversarially, rather than re-implemented per tool.

---

## 5. Data flow for one spoken command

Tracing `"create a git branch called feature auth"` end to end:

| # | Stage | Output | Budget |
|---|---|---|---|
| 1 | AudioWorklet | 20 ms Int16 frames | — |
| 2 | WS reader → ring buffer | samples | <1 ms |
| 3 | Incremental mel | 2 new frames per 20 ms | ~0.1 ms |
| 4 | Silero VAD | `P(speech)` per 32 ms | ~1 ms |
| 5 | Endpointer | `speech_started`, later `endpoint` | — |
| 6 | Partial decode (×N) | `transcript.partial` (unstable) | ≤400 ms cadence |
| 7 | Final decode | `"create a git branch called feature auth"` + word times + confidence | ~250 ms |
| 8 | Normalize | `"create a git branch called feature-auth"` | <1 ms |
| 9 | Grammar match | `git.branch_create{name:"feature-auth"}`, source=grammar | <1 ms |
| 10 | Policy | risk=Write → **Confirm** → `tool.permission_required` | <1 ms |
| 11 | User approves | `tool.approve{call_id}` | human |
| 12 | Runner | `git switch -c feature-auth` in workspace root, argv-exec, 10 s timeout | ~50 ms |
| 13 | Audit + persist | `tool_executions` + `audit_events` rows | <5 ms |
| 14 | Composer | `"Created and switched to feature-auth."` | — |

**Step 8 is the one that makes this work at all.** Without identifier repair,
the branch would be named `feature auth`, and the whole feature is useless.
That is why `normalize` is a first-class package and not a `strings.TrimSpace`
call.

---

## 6. Concurrency model

| Unit | Goroutines | Bound |
|---|---|---|
| HTTP request | 1 (stdlib) | `MaxHeaderBytes`, body limit, per-request `context.WithTimeout` |
| WS session | 4: reader, VAD, scheduler, writer | one session per connection; connections capped |
| Encoder | shared ORT session, N concurrent `Run` | ASR admission semaphore |
| Decoder | pool of N workers | `NEELKIT_ASR_WORKERS` (default `min(4, NumCPU/2)`) |
| Tool exec | 1 per call | per-tool timeout + global concurrent-exec semaphore |
| LLM call | 1 per turn | provider timeout + circuit breaker |

**The admission queue** is the piece upstream lacks. All ASR work — batch and
realtime, partial and final — enters through
`engine.Acquire(ctx, priority)`. It is a bounded queue, not an unbounded
channel:

- `final` and `batch` priority: queue up to `2 × workers`, then reject with
  `ErrOverloaded` → HTTP 503 + `Retry-After`, or a `stream.overloaded` event.
- `partial` priority: never queues. If no worker is free *right now*, the
  partial is skipped. Skipping a preview is free; delaying it is not.

Every wait is instrumented: `neelkit_asr_queue_wait_seconds` and
`neelkit_asr_queue_depth` make saturation visible instead of inferable.

**Shutdown** is ordered and race-free, fixing the upstream leak:
`SIGTERM` → stop accepting connections → `httpServer.Shutdown(ctx)` → close WS
sessions with code 1001 → wait for the exec semaphore to drain → `engine.Close()`
destroys workers (all of which are now provably returned) → close SQLite →
flush traces. Each step has its own deadline; the total is bounded by
`NEELKIT_SHUTDOWN_TIMEOUT` (default 30 s).

---

## 7. What this project deliberately does *not* do

Stating these matters as much as the feature list — an unbounded scope is the
tell of a project that has not been thought about.

- **No model training or fine-tuning.** Inference against a fixed checkpoint.
- **No speaker diarisation.** Single speaker assumed; multi-speaker is a
  different model, not a feature flag.
- **No wake-word in v1.** Push-to-talk and VAD-gated always-on are enough;
  a real wake word needs a trained keyword-spotting model, and a bad one that
  false-fires is worse than no wake word. Listed in the roadmap with that
  reasoning.
- **No multi-user tenancy in v1**, but `user_id` is on every table from
  migration 0001, so it is not a rewrite later.
- **No TTS in v1.** Browser `SpeechSynthesis` covers the demo; a local TTS
  model is roadmap.
- **No arbitrary shell.** There is a `shell` tool, and it takes an argv against
  an allowlist. `sh -c "$user_input"` does not appear anywhere in this codebase.
