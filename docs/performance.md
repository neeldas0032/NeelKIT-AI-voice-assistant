# Performance

**Status: not yet measured on real hardware.** The harness exists; the results
tables below are empty on purpose and stay empty until someone runs them.

That is the point of this file. A benchmark that ships with its own numbers
proves nothing, because the person reading it cannot tell whether the numbers
came from a machine, from an estimate, or from a hope. Everything here is
produced by `nkbench` on the machine you are holding, and the method is written
down so the figures can be argued with.

## What is measured, and why these three

| | why it decides whether the product works |
|---|---|
| **Real-time factor** | Compute time over audio length. Above 1.0 the recogniser cannot keep up with speech and live transcription falls behind permanently. The whole incremental-decoding design in `docs/architecture.md` is a bet that this stays well under 1.0 on a CPU. |
| **Grammar routing latency** | The architectural claim is that the twenty commands a developer says daily need no language model. That claim is only worth making if the tier is genuinely free. |
| **Normalisation latency** | It runs on every utterance between the recogniser and the router, so it is on the critical path of every single turn. |

Word error rate is measured separately — see below — because it needs a
reference transcript, which the harness cannot invent.

## Running it

```bash
make build                       # builds bin/nkbench alongside neelkitd and nk

# The two that need no model and no audio:
./bin/nkbench route
./bin/nkbench normalize

# The one that needs both:
./bin/nkbench asr -models ./models -corpus ./testdata/speech

# Everything, with the raw numbers kept for comparison later:
./bin/nkbench all -models ./models -corpus ./testdata/speech -json bench.json
```

`nkbench` prints markdown. Paste it under the headings below.

### Building the corpus

Record **your own** commands — 30 to 50 clips of the sentences you actually say
to this thing, at 16 kHz mono. A number measured on speech that resembles the
workload is worth more than a number measured on read-aloud audiobooks, and it is
far more defensible in an interview than a figure copied from a paper.

```bash
# Linux / WSL, one clip at a time
arecord -f S16_LE -r 16000 -c 1 testdata/speech/01-list-files.wav

# Or convert anything you already have
ffmpeg -i input.m4a -ar 16000 -ac 1 -c:a pcm_s16le testdata/speech/01.wav
```

Keep the clips out of git — they are your voice. `testdata/speech/` is
gitignored. Write the reference transcripts in
`testdata/speech/transcripts.tsv` — one `filename<TAB>exact words spoken` per
line — or as a `.txt` file beside each clip. `nkbench asr` reads either, and
when references are present it reports word error rate and prints a table of
what the recogniser heard next to what was said.

### Method

- The first pass over the corpus is **discarded**. It pays for lazily allocated
  arenas inside ONNX Runtime and a cold page cache, and reports a number nobody
  experiences after the first sentence of a session.
- Percentiles are nearest-rank, no interpolation — it matches what most latency
  tooling reports and needs no choice defended.
- Audio is decoded into memory before timing starts. Disk I/O is not what is
  being measured, and on a Windows bind mount it would dominate the result.
- Run with the machine otherwise idle, on mains power. A laptop on battery
  throttles, and a throttled number is not wrong, it is just a different
  measurement that has to be labelled as one.

## Results

> Delete this quote block and paste the `nkbench` output when you have run it.
> Leave the "Measured on" table — the figures mean nothing without it.

### Measured on

| | |
|---|---|
| platform | |
| logical CPUs | |
| model | |
| precision | |
| decoder workers | |
| date | |

### Speech recognition

| metric | value |
|---|---|
| real-time factor, mean | |
| real-time factor, p50 | |
| real-time factor, p95 | |
| wall-clock per clip, p50 | |
| wall-clock per clip, p95 | |
| decoder confidence, mean | |

### Grammar routing

| percentile | latency |
|---|---|
| p50 | |
| p95 | |
| p99 | |

### Normalisation

| percentile | latency |
|---|---|
| p50 | |
| p95 | |
| p99 | |

## Word error rate

Not yet measured. `nkbench asr` computes it when the corpus has reference
transcripts (see above).

Method:

1. Record the corpus above and write `testdata/speech/transcripts.tsv` — one
   `filename<TAB>exact words spoken` per line.
2. Score the **raw** decoder output against the reference, before normalisation.
   `nkbench` does exactly this: it scores the engine's text, lowercased and
   with punctuation removed, and never runs the normaliser.
   Scoring after normalisation measures the normaliser's ability to hide
   recogniser errors, which is a different and much more flattering question.
3. Report the corpus size, and report it honestly: 50 clips of one speaker in one
   room is a small sample, and saying so is what makes the number credible.
4. Note separately how many of the errors the normaliser then repairs. That
   second figure is the interesting one — it is the argument for the
   normalisation layer existing at all.

An expected failure worth writing down when it happens: identifier-shaped
speech ("feature dash auth", "src slash main dot go") is where a general-purpose
recogniser is weakest, and it is exactly the input this product receives most.
The gap between raw WER and post-normalisation WER on those clips is the clearest
evidence that the repair pass earns its place.

## What is not measured yet

Named so that nobody mistakes silence for a good result:

- **Memory under sustained load.** Each decoder worker holds its own ONNX
  session. The per-worker cost is documented as roughly 20 MB in
  `internal/asr/onnx`, but that is a design estimate, not a measurement.
- **Endpoint-to-final latency end to end.** `nkbench asr` times the decode, not
  the full path from the last syllable to the transcript on screen. The
  `latency_ms` field on `transcript.final` carries the real figure at runtime and
  the telemetry rail displays it — it is just not aggregated here yet.
- **Concurrency.** The worker pool is tested for correctness under load but its
  throughput curve has not been plotted.
- **CUDA.** The image builds. It has never run on a GPU.
