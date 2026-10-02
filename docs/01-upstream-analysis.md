# Upstream Analysis — `achetronic/parakeet`

> Commit analysed: shallow clone of `main`, September 2026. Apache-2.0.
> 4,646 lines of Go across 2 internal packages, 1 external dependency
> (`github.com/yalue/onnxruntime_go v1.30.1`).
>
> This document is the honest engineering assessment that NeelKIT is built on.
> Everything below was read from source, not from the README.

---

## 1. What the project actually is

A **stateless, single-model, batch speech-to-text HTTP server** that speaks
OpenAI's Whisper API dialect. You POST an audio file, you get a transcript
back. That is the whole product.

It is *not* a streaming ASR server, and it is *not* an assistant. Its
"streaming" is output streaming: the audio is fully buffered first, then
decoded tokens are pushed to the client as Server-Sent Events. This
distinction is the single most important fact for NeelKIT's design, and
§6 explains why.

### Top-level layout

```
main.go                    144  flags → env fallback, slog setup, signal handling
main_test.go               109
internal/server/
  server.go                195  Config, route table, bearer-auth middleware, lifecycle
  handlers.go              360  multipart handler, response formats, SSE writer, CORS
  streaming.go             101  raw-body (non-multipart) upload handler
  types.go                  72  OpenAI-shaped DTOs
internal/asr/
  transcriber.go           953  ONNX sessions, worker pool, TDT greedy decode loop
  mel.go                   247  Hann window, mel filterbank, radix-2 FFT, normalisation
  audio.go                 186  WAV parse (8/16/24/32-bit PCM + f32), channel mix, resample
  ffmpeg.go                208  external-binary transcode fallback
  chunker.go               198  long-audio sliding-window planner
  boundary.go              288  chunk-boundary oracle cascade
  vad.go                   190  Silero VAD ONNX session wrapper
  seam.go                   84  token-level seam deduplication
Dockerfile / Dockerfile.cuda    CPU (debian-slim) and CUDA (nvidia/cuda 12.6) images
.github/workflows/ci.yaml       vet + gofmt + `go test -race` + build
.agents/DESIGN_DECISIONS.md     14 ADRs, genuinely good ones
```

The layering is clean: `main` owns configuration and process lifecycle,
`server` owns HTTP transport, `asr` owns everything from bytes to text.
`asr` has **no import of `net/http`** — that separation is worth copying.

---

## 2. The inference pipeline, end to end

```
bytes ──► loadAudio ──► mel.Extract ──► planForAudio ──► [per window]
                                                          encoder.Run (ORT)
                                                          tdtDecode  (ORT, pooled)
                                                        ──► seam dedup ──► tokensToText
```

### 2.1 Audio decoding (`audio.go`, `ffmpeg.go`)

Format is detected **by content, not filename**: the first 12 bytes are
checked for `RIFF….WAVE`. WAV is parsed in-process — it walks the chunk list
looking for `fmt ` and `data`, supports 8/16/24/32-bit PCM and 32-bit float,
averages channels to mono, and resamples to 16 kHz. Anything else is written
to a temp file and handed to an external `ffmpeg` binary
(`-ac 1 -ar 16000 -acodec pcm_s16le -f wav`), which is resolved once at
startup via `exec.LookPath`. A missing ffmpeg is not fatal: the converter is
`nil` and non-WAV uploads get a clean HTTP 400 through the typed sentinel
`ErrUnsupportedAudio`.

That content-sniffing + typed-sentinel + startup-probe pattern is the best
single idea in the repository and NeelKIT keeps it verbatim in spirit.

### 2.2 Feature extraction (`mel.go`)

NeMo-compatible defaults: 128 mel bins, 512-point FFT, 400-sample (25 ms)
Hann window, 160-sample (10 ms) hop → **100 mel frames per second**. Per frame
it windows, runs a radix-2 Cooley–Tukey FFT, takes the power spectrum, applies
a triangular mel filterbank, and takes `log`. Afterwards it applies
**per-utterance mean/variance normalisation across the entire file**.

### 2.3 Chunking for long audio (`chunker.go`)

The exported ONNX encoder has a hard ceiling: its positional-encoding table is
`[1, 9999, 1024]` centred at 5000, so beyond `modelMaxEncoderFrames = 5000`
encoder frames the relative-position slice goes out of range and self-attention
crashes on a broadcast mismatch. At 8× subsampling and 100 mel fps that is
**~400 seconds of audio in one pass**.

With `--long-audio` off, anything longer is rejected with `ErrAudioTooLong`.
With it on, the mel sequence is tiled into windows of `chunkFrames` (default
300 s) overlapping by `overlapFrames` (default 15 s). Each window is *encoded*
over its full extent but only *emits* tokens inside `[emitStart, emitEnd)`.
The emit ranges tile the timeline exactly — `emitEnd[i] == emitStart[i+1]` —
and that invariant is enforced by clamping whatever the boundary oracle
returns, then property-tested against an adversarial oracle. Genuinely good
engineering.

### 2.4 Boundary selection (`boundary.go`, `vad.go`)

Splitting an overlap at its arithmetic midpoint lands mid-word, so the word
gets timestamped differently by each window and is emitted twice or dropped.
Their fix is a three-layer cascade behind one interface:

1. **Silero VAD** over the overlap's waveform only — picks the centre of the
   longest run of 32 ms windows with `P(speech) < 0.4`, preferring runs after
   a ~1 s warm-up because the recurrent state is reset per overlap.
2. **Mel energy** — 150 ms moving average over per-frame energy sums, pick the
   minimum. Free, because the features already exist.
3. **Midpoint** — always decides, so the cascade terminates.

Plus an always-on seam dedup: a leading token of window *i+1* is dropped if its
absolute encoder-frame timestep is within 3 frames (~240 ms) of any of window
*i*'s last 3 tokens. One rule covers both duplicates and collisions, and
window *i* wins because its decoder LSTM reaches the seam warmed up.

### 2.5 Model integration (`transcriber.go`)

Parakeet TDT 0.6B, split into two ONNX graphs:

| | shape / value |
|---|---|
| Encoder | Conformer, output dim **1024**, subsampling **8×** |
| Decoder+Joint | LSTM, **2 layers × 640** state, vocab **8193** (blank at 8192) |
| Duration head | **5** classes, concatenated after the vocab logits |
| Decoding | greedy argmax, `maxTokensPerStep = 10` |

Concurrency model:

- **Encoder**: one long-lived `ort.DynamicAdvancedSession`, shared. Shapes vary
  per request so fresh tensors are passed to each `Run`. ORT `Run` is
  thread-safe on a shared session.
- **Decoder**: a pool of `N` `decoderWorker`s in a buffered channel. Each owns a
  persistent `ort.AdvancedSession` **and pre-allocated input/output tensors**.
  The decode loop writes straight into tensor backing memory via `GetData()`
  and calls `Run()` with zero allocation per timestep. This replaced an earlier
  version that created a session *per timestep* (~150 model loads per request).
- The channel doubles as a semaphore: acquisition is `select` on the pool
  channel or `ctx.Done()`, so a disconnecting client frees the slot.

The TDT loop itself: at each encoder timestep, copy the 1024-dim frame into the
reusable tensor, set the previous token, `Run`, `argmax` the vocab logits and
the duration logits. Non-blank tokens advance the LSTM state (`state*Out` →
`state*In`) and set `prevToken`. The duration head decides the time jump:
`step > 0` advances `timestep += step`, otherwise it advances by 1. That
duration prediction is what makes TDT faster than a classic RNN-T — it skips
frames instead of grinding one at a time.

---

## 3. HTTP layer

Routes: `POST /v1/audio/transcriptions`, `POST /v1/audio/translations`
(delegates to transcription), `GET /v1/models` (advertises `whisper-1` as an
alias), `GET /health`.

`handleTranscription` branches on `Content-Type`: multipart goes to the OpenAI
path, anything else treats the raw body as audio. Multipart is capped at 25 MB
to match OpenAI; raw bodies use `http.MaxBytesReader`.

The SSE writer is the most careful code in the HTTP layer. It deliberately sets
**no `WriteTimeout`** on `http.Server` (that would kill healthy long streams)
and instead uses `http.NewResponseController` to set a fresh 30 s write
deadline **before every event**. If a write fails — disconnect, broken pipe,
stalled reader — it cancels a derived context so the decoder stops and returns
its worker instead of computing into the void. That is a correct, non-obvious
answer to slow-reader DoS and NeelKIT reuses the pattern on its WebSocket.

Auth is one static key from `PARAKEET_API_KEY`, compared against
`Authorization: Bearer …`; when unset, auth is entirely disabled.

---

## 4. What is genuinely worth learning from

| Idea | Why |
|---|---|
| Pre-warmed worker pool with **reusable tensors** | The decode loop is allocation-free. This is the difference between 0.3× and 8× real-time factor. |
| Pool channel **as semaphore** with `ctx` on acquire | Bounded memory, natural backpressure, no goroutine leak on disconnect. |
| Shared encoder session, pooled decoder sessions | Correctly distinguishes "thread-safe, expensive to build" from "stateful, must be owned". |
| Content-sniffing + typed sentinel error | Client errors are 400, server errors are 500. No guessing from file extensions. |
| Boundary oracle **interface** + clamping invariant | Three interchangeable strategies, each unit-testable from synthetic arrays, and a tiling invariant that holds for *any* oracle — property-tested. |
| Per-event write deadlines via `ResponseController` | The right fix for stalled SSE readers. |
| Flag → env var derivation in one `VisitAll` | Every flag gets `PARAKEET_<NAME>` free, with CLI > env > default precedence, and invalid env values are rejected instead of silently zeroing the flag. |
| ADR log (`DESIGN_DECISIONS.md`) | Every decision has context, rationale, consequences, and rejected alternatives. |

---

## 5. Concrete weaknesses found in the code

These are **read from the source**, not guessed. Each one is an interview
answer for NeelKIT: "here is what upstream does, here is why I changed it."

### Correctness

1. **`verbose_json` fabricates fields.** `handlers.go` hardcodes
   `AvgLogprob: -0.5`, `CompressionRatio: 1.0`, `NoSpeechProb: 0.0` and emits a
   single segment spanning the whole file. These are placeholders shaped like
   Whisper's output, not measurements.
2. **Duration is computed from the wrong bytes.** `duration :=
   float64(len(audioData)) / (16000.0 * 2)` treats *any* upload as 16 kHz
   16-bit mono PCM. For an MP3 that number is meaningless, and it propagates
   into the SRT/VTT timestamps.
3. **Timestamps are computed, then discarded.** `decodedToken` carries an
   absolute encoder-frame timestep, used for seam dedup, then
   `tokensToText` throws it away. Encoder frame × 8 subsampling × 10 ms hop =
   **80 ms resolution timing, already paid for**, never exposed.
4. **Resampling aliases.** `resample()` is bare linear interpolation with no
   anti-alias filter. Downsampling 44.1 kHz → 16 kHz folds everything above
   8 kHz back into the band the model listens to.
5. **`Close()` can leak a worker and races shutdown.** It does
   `close(t.decoderPool)` then `for w := range t.decoderPool { w.destroy() }`.
   Any worker checked out at that moment is never destroyed, and its deferred
   return-to-pool send panics on the closed channel — recovered, but the ORT
   session is leaked. Safe only because `Shutdown` is called first; nothing in
   the type enforces that ordering.

### Performance

6. **The FFT recomputes twiddle factors per butterfly.** `mel.go` calls
   `cmplx.Exp(complex(0, angle))` *inside* the innermost loop — a transcendental
   per butterfly, for every one of the 100 frames per second of audio. Twiddles
   are constant; precomputing them is a large, free win.
7. **A full complex FFT is used for a real signal.** Half the work is
   structurally redundant; a real-input FFT or the standard pack-two-reals trick
   halves it.
8. **Per-frame allocation.** Each frame allocates a `[]float64` window, a
   `[]complex128` of length 512, a `[]float64` power spectrum and a
   `[]float32` of mel energies. At 100 fps that is 400+ allocations per second
   of audio, all short-lived, all GC pressure.
9. **The whole mel spectrogram is materialised before any decoding.** For a
   3-hour file that is 1.08 M frames × 128 floats ≈ **550 MB** resident before
   the first window is encoded, on top of the waveform. Long-audio mode chunks
   the *decode*, not the *feature extraction*.
10. **Mel normalisation is non-causal.** Mean/variance are computed over the
    entire utterance, so the features for second 1 depend on second 3600. This
    is correct for batch and **impossible for streaming** — see §6.

### Robustness / security

11. **API key comparison is not constant-time.** `token != s.apiKey` is a
    length-and-content-dependent compare — a timing oracle. Keys are also
    stored in plaintext in the environment and compared in plaintext.
12. **`Access-Control-Allow-Origin: *` is unconditional** on an API that
    accepts bearer tokens. Combined with a browser-held key that is a real
    exposure; there is no origin allowlist.
13. **No per-request deadline.** Only `ReadHeaderTimeout` is set. A 25 MB
    pathological upload can occupy a worker for as long as the client keeps the
    socket open.
14. **No backpressure signal.** When all workers are busy a request blocks on
    the pool channel until its context dies. The client cannot distinguish
    "slow" from "overloaded"; there is no 503, no `Retry-After`, no queue-depth
    metric.
15. **`/health` is a liveness probe pretending to be readiness.** It returns
    `{"status":"ok"}` unconditionally — it cannot report a saturated pool, a
    dead ORT environment, or a missing model.
16. **The VAD session runs outside the worker pool.** Upstream documents this
    honestly: VAD concurrency is bounded by in-flight HTTP requests, not by
    `-workers`, so the memory guarantee the pool provides does not cover it.
17. **`asr.DebugMode` is a mutable package-level global** read inside the
    decode hot loop.

### Observability / testing

18. **No metrics, no tracing, no request IDs.** Structured logs exist
    (`slog`, text or JSON) but nothing correlates a log line to a request, and
    there is no way to answer "what is p99 encoder latency right now".
19. **Tests cover the pure logic well and the model path not at all.** 36 test
    functions, almost all on chunk planning, boundary oracles, seam dedup, WAV
    sniffing and flag parsing — the parts that need no ONNX runtime. There is
    no golden-transcript test, no WER measurement, and no benchmark target.
    That is a defensible CI trade-off, but it means accuracy regressions are
    invisible.

---

## 6. The structural limitation that drives NeelKIT's design

**Parakeet TDT 0.6B as exported is a full-context offline model.** The encoder
attends over the entire window it is given. There is no cache-aware streaming
export here, no chunked encoder state, no left-context cache.

Three consequences follow, and they are the reason NeelKIT is not a fork:

1. **Upstream cannot do real-time microphone input at all.** Its SSE stream
   emits decoder output progressively, but only after the *complete* audio has
   been uploaded, decoded to a waveform, and turned into a full mel
   spectrogram. Time-to-first-token is bounded below by the length of the
   recording.
2. **Non-causal normalisation is incompatible with streaming.** You cannot
   compute a whole-utterance mean before the utterance has ended.
3. **Latency scales with input length, not with speech length.** There is no
   endpointing: nothing decides "the user stopped talking, decode now."

NeelKIT's answer is not to pretend the model streams. It is to build an
**incremental decoding scheduler** around an offline model:

- VAD runs continuously on the live input and drives an endpointing state
  machine (speech start / speech end / hangover).
- Mel features are computed **once, incrementally**, into a ring buffer as
  audio arrives — never recomputed.
- Normalisation uses causal running statistics seeded from a warm-up window.
  *(Later reversed — see AD-002 in `architecture.md`: every decode is a
  complete slice, so whole-slice statistics are both available and correct.)*
- While speech is active, a *partial* decode runs on a debounced cadence over
  the trailing N seconds → an unstable preview transcript.
- On endpoint, one *final* decode runs over the whole utterance → the stable
  transcript that the assistant acts on.

That is an honest design: it names the model's limitation, pays a bounded
redundant-compute cost for partials, and gets correct finals. It is also the
part of NeelKIT that is entirely original.

---

## 7. Reuse / redesign verdict

| Component | Verdict | Note |
|---|---|---|
| TDT greedy decode loop + duration head | **Reuse the algorithm** | Conceptually reimplemented; it is how the model works, not an invention. Attributed. |
| Tensor-reusing decoder worker pool | **Reuse the pattern** | Rebuilt over an explicit semaphore + owned slice so shutdown is race-free. |
| Shared encoder session | **Reuse** | Correct; extended with a warm-up pass at boot. |
| Model dimensions / vocab loading | **Reuse the facts** | 1024 / 640×2 / 8193 / 5 / 8× are properties of the checkpoint. |
| WAV parser + content sniffing | **Reuse the approach** | Reimplemented with anti-aliased resampling. |
| ffmpeg fallback | **Reuse the design** | Startup probe, per-call temp files, bounded timeout, typed error — all kept. |
| Boundary oracle cascade + seam dedup | **Reuse conceptually** | Excellent design for long files; kept for the batch endpoint only. |
| Mel filterbank | **Redesign** | Same maths, precomputed twiddles, real FFT, ring-buffer incremental, causal normalisation. |
| Chunk planner | **Keep for batch, bypass for realtime** | Realtime uses VAD utterance segmentation instead. |
| HTTP layer | **Redesign** | Middleware chain, request IDs, real readiness, bounded queue with 503, origin allowlist, hashed keys, per-request deadlines. |
| SSE streaming | **Replace with WebSocket** | Need bidirectional: audio up, events + permission prompts down. SSE is one-way. |
| Auth | **Redesign** | Hashed multi-key with `subtle.ConstantTimeCompare`, WS ticket exchange. |
| Config | **Redesign** | Keep the flag→env derivation idea, add a typed config struct with validation and a `doctor` command. |
| Everything above the transcript | **100% original** | Normalisation, intent routing, tool registry, permissions, persistence, UI, CLI, SDK. Upstream has none of it. |
