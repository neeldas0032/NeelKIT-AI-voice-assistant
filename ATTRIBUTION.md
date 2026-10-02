# Attribution & Originality

This project exists because of other people's work. This file says exactly
whose, and exactly what is mine — so that every claim I make about NeelKIT in
an interview is one I can defend.

---

## Upstream projects

### `achetronic/parakeet` — Apache License 2.0

<https://github.com/achetronic/parakeet> — © Alby Hernández.

NeelKIT's speech-recognition layer is **derived from and inspired by** this
project. It is the reference implementation I studied to understand how to run
Parakeet TDT through ONNX Runtime from Go.

**Concepts and techniques adopted** (documented in
`docs/01-upstream-analysis.md`, §7):

- The TDT greedy decode loop: how the duration head advances the timestep, how
  LSTM state is carried through `state_in`/`state_out`, how the blank index and
  `maxTokensPerStep` terminate the inner loop.
- The tensor-reusing decoder worker pool, and the insight that the encoder
  session is shareable while decoder sessions are not.
- Model facts: encoder dim 1024, 8× subsampling, 2×640 LSTM state, 8193 vocab
  with blank at 8192, 5 duration classes, the 5000-encoder-frame
  positional-encoding limit.
- Content-based audio format detection with a typed `ErrUnsupportedAudio`
  sentinel, and the ffmpeg fallback design (startup probe, per-call temp files,
  bounded timeout).
- The long-audio overlapping-window planner, the boundary-oracle cascade
  (VAD → mel energy → midpoint) and the seam token dedup, used in NeelKIT's
  batch path.
- Per-event write deadlines via `http.ResponseController` for streamed responses.
- Deriving an environment variable for every flag in one `flag.VisitAll` pass.

**Apache-2.0 obligations met:**

- `LICENSES/parakeet-APACHE-2.0.txt` contains the upstream licence verbatim.
- `NOTICE` records the upstream copyright.
- Every NeelKIT source file whose implementation is closely derived from an
  upstream file carries a header naming the upstream file and its licence.
- No upstream file is copied verbatim and presented as original. Where code is
  structurally similar because the algorithm is the same, the header says so.
- The upstream project's name is not used to imply endorsement.

### NVIDIA Parakeet TDT 0.6B v3

The speech model. **I did not create, train, fine-tune or modify this model.**
ONNX export by [`istupakov/parakeet-tdt-0.6b-v3-onnx`](https://huggingface.co/istupakov/parakeet-tdt-0.6b-v3-onnx).
Model weights are downloaded at build time and are not vendored. The model
licence is NVIDIA's; see the model card.

### Silero VAD v6.2.1 — MIT

<https://github.com/snakers4/silero-vad>. Voice activity detection. Downloaded
at build time, pinned by tag and sha256. Not vendored, not modified.

### ONNX Runtime (MIT) and `yalue/onnxruntime_go` (MIT)

Inference runtime and its Go binding.

---

## What is original to NeelKIT

Upstream is a stateless batch transcription server. Everything that makes
NeelKIT an *assistant* has no upstream counterpart:

**Realtime pipeline** — incremental mel extraction into a ring buffer; causal
feature normalisation; the VAD-driven endpointing state machine; the
partial/final decode scheduler around an offline model; the priority admission
queue that sheds partials before finals; the WebSocket event protocol.

**Language layer** — transcript normalisation with a developer lexicon and
identifier repair; grammar-first intent routing with slot extraction; the
pluggable LLM provider interface and the planner that builds its schema from the
tool registry.

**Action layer** — the `Tool`/`Descriptor`/`RiskLevel` contract; the
self-registering plugin registry; the pure policy engine with
escalate-only decisions; the approval protocol; the `os.Root` filesystem
sandbox; argv-only process execution with rlimits and process-group kill; the
SSRF-guarded HTTP client; the append-only audit log.

**Platform** — the data model and migrations; FTS5 transcript search; hashed
multi-key auth with WebSocket ticket exchange; the observability stack; the
Next.js UI; the `nk` CLI; the `pkg/neelkit` SDK; `nkbench`.

**Fixes to problems found in upstream** — each listed with the upstream
behaviour in `docs/01-upstream-analysis.md` §5: real confidence instead of
hardcoded `avg_logprob`, real duration instead of byte-count arithmetic, exposed
word timestamps instead of discarded ones, anti-aliased resampling, precomputed
FFT twiddles, race-free shutdown, constant-time key comparison, origin
allowlisting, per-request deadlines, and a bounded queue with a real 503.

---

## What I will not claim

- I did not invent the Parakeet architecture, TDT decoding, or the Conformer
  encoder.
- I did not train any model.
- I did not invent ONNX Runtime, Silero VAD, or the OpenAI API shape.
- I did not independently discover the decoder-pool or boundary-oracle designs —
  I read them in `achetronic/parakeet` and say so.
- Benchmark numbers in `docs/performance.md` are measured on stated hardware
  with a stated method, or they are not published. No number in this repository
  is estimated and presented as measured.

---

## Commit history

The git history is the real one. No backdated commits, no fabricated
contributors, no squashed "initial commit" hiding a copied tree.
`CHANGELOG.md` tracks what changed per release, and `docs/adr/` records design
decisions as they are made, with the alternatives that were rejected.
