package onnx

import (
	"context"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/neeldas0032/NeelKIT-AI-voice-assistant/internal/asr"
	"github.com/neeldas0032/NeelKIT-AI-voice-assistant/internal/audio"
)

// syntheticDir holds a model with Parakeet's exact I/O signature but trivial
// weights. See scripts/gen_testmodel.py for why.
const syntheticDir = "../../../testdata/synthetic-model"

func newSyntheticEngine(t *testing.T) *Engine {
	t.Helper()
	if _, err := os.Stat(filepath.Join(syntheticDir, "encoder-model.onnx")); err != nil {
		t.Skip("synthetic model missing; run `make testmodel`")
	}
	e, err := New(Config{
		ModelsDir:  syntheticDir,
		ORTLibrary: os.Getenv("ONNXRUNTIME_LIB"),
		Workers:    2,
		Device:     "cpu",
	})
	if err != nil {
		t.Skipf("engine unavailable: %v", err)
	}
	t.Cleanup(func() { _ = e.Close() })
	return e
}

func tone(seconds float64) []float32 {
	n := int(float64(audio.SampleRate) * seconds)
	s := make([]float32, n)
	for i := range s {
		s[i] = float32(0.2 * math.Sin(2*math.Pi*220*float64(i)/audio.SampleRate))
	}
	return s
}

// The decode loop is the hardest part of the engine to get right: a strided
// gather of the encoder frame, recurrent state carried only on non-blank
// emissions, and a duration head that decides the timestep jump. The synthetic
// decoder is a deterministic state machine over the previous token, so any of
// those going wrong changes this string.
func TestDecodeLoopProducesExpectedTranscript(t *testing.T) {
	e := newSyntheticEngine(t)

	res, err := e.Transcribe(context.Background(), asr.Request{
		Samples:   tone(2),
		WantWords: true,
	})
	if err != nil {
		t.Fatalf("Transcribe: %v", err)
	}
	if res.Text != "hello world this is neelkit" {
		t.Fatalf("text = %q, want %q", res.Text, "hello world this is neelkit")
	}
	t.Logf("text=%q confidence=%.4f rtf=%.4f", res.Text, res.Confidence, res.RTF)
}

// Word assembly must join sub-word pieces ("wor"+"ld", "neel"+"kit") into whole
// words and give each a monotonically increasing span.
func TestWordTimingsAndAssembly(t *testing.T) {
	e := newSyntheticEngine(t)

	res, err := e.Transcribe(context.Background(), asr.Request{Samples: tone(2), WantWords: true})
	if err != nil {
		t.Fatalf("Transcribe: %v", err)
	}

	want := []string{"hello", "world", "this", "is", "neelkit"}
	if len(res.Words) != len(want) {
		t.Fatalf("got %d words %v, want %d %v", len(res.Words), wordStrings(res.Words), len(want), want)
	}
	for i, w := range res.Words {
		if w.Word != want[i] {
			t.Errorf("word %d = %q, want %q", i, w.Word, want[i])
		}
		if w.End <= w.Start {
			t.Errorf("word %q has end %.3f <= start %.3f", w.Word, w.End, w.Start)
		}
		if i > 0 && w.Start < res.Words[i-1].Start {
			t.Errorf("word %q starts before its predecessor", w.Word)
		}
		if w.Confidence <= 0 || w.Confidence > 1 {
			t.Errorf("word %q confidence %v outside (0,1]", w.Word, w.Confidence)
		}
	}

	// One encoder frame is 8 mel frames of 10 ms: an 80 ms quantum. Every
	// boundary must land on it, and the synthetic decoder emits one token per
	// frame, so consecutive words start exactly one frame apart.
	const frame = 0.08
	for _, w := range res.Words {
		if r := math.Mod(w.Start+1e-9, frame); r > 1e-6 && math.Abs(r-frame) > 1e-6 {
			t.Errorf("word %q starts at %.4fs, not on the 80 ms frame grid", w.Word, w.Start)
		}
	}
	if got := res.Words[1].Start - res.Words[0].Start; math.Abs(got-frame) > 1e-6 {
		t.Errorf("gap between the starts of %q and %q = %.4fs, want one 80 ms frame",
			res.Words[0].Word, res.Words[1].Word, got)
	}
	// "world" is two sub-word pieces, so it must span two frames.
	if got := res.Words[1].End - res.Words[1].Start; math.Abs(got-2*frame) > 1e-6 {
		t.Errorf("%q spans %.4fs, want two frames (it is two sub-word pieces)", res.Words[1].Word, got)
	}
}

// Confidence must be a real measurement in (0, 1]. The reference implementation
// this project studied returns a hardcoded avg_logprob of -0.5 regardless of
// the audio, which is indistinguishable from a lie.
func TestConfidenceIsRealAndBounded(t *testing.T) {
	e := newSyntheticEngine(t)

	res, err := e.Transcribe(context.Background(), asr.Request{Samples: tone(2)})
	if err != nil {
		t.Fatalf("Transcribe: %v", err)
	}
	if math.IsNaN(res.Confidence) || math.IsInf(res.Confidence, 0) {
		t.Fatalf("confidence = %v", res.Confidence)
	}
	if res.Confidence <= 0 || res.Confidence > 1 {
		t.Fatalf("confidence = %v, want (0,1]", res.Confidence)
	}
	// The synthetic table puts the winning logit 20 above every rival, so the
	// softmax probability must be close to 1. A missing max-subtraction in the
	// softmax would overflow and produce NaN; a wrong denominator would drift.
	if res.Confidence < 0.99 {
		t.Errorf("confidence = %.4f on a near-deterministic model, want > 0.99", res.Confidence)
	}
}

func TestTranscribeStreamDeltasReconstructText(t *testing.T) {
	e := newSyntheticEngine(t)

	var sb strings.Builder
	res, err := e.TranscribeStream(context.Background(), asr.Request{Samples: tone(2)},
		func(d string) { sb.WriteString(d) })
	if err != nil {
		t.Fatalf("TranscribeStream: %v", err)
	}
	if got := collapseSpaces(sb.String()); got != res.Text {
		t.Fatalf("concatenated deltas %q != final text %q", got, res.Text)
	}
	if sb.Len() == 0 {
		t.Fatal("no deltas emitted")
	}
}

func TestShortAudioReturnsEmptyNotError(t *testing.T) {
	e := newSyntheticEngine(t)

	res, err := e.Transcribe(context.Background(), asr.Request{Samples: make([]float32, 100)})
	if err != nil {
		t.Fatalf("50 ms of audio should not error: %v", err)
	}
	if res.Text != "" {
		t.Fatalf("text = %q, want empty", res.Text)
	}
}

func TestCancelledContextStopsDecode(t *testing.T) {
	e := newSyntheticEngine(t)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := e.Transcribe(ctx, asr.Request{Samples: tone(2)}); err == nil {
		t.Fatal("expected an error from a cancelled context")
	}
}

func TestAudioTooLongIsRejected(t *testing.T) {
	e := newSyntheticEngine(t)
	e.cfg.MaxAudio = time.Second

	_, err := e.Transcribe(context.Background(), asr.Request{Samples: tone(3)})
	if err == nil || !strings.Contains(err.Error(), "too long") {
		t.Fatalf("err = %v, want an audio-too-long error", err)
	}
}

// Concurrency is the point of the worker pool. With -race this catches shared
// tensor state between workers, which would silently corrupt transcripts.
func TestConcurrentTranscription(t *testing.T) {
	e := newSyntheticEngine(t)

	const n = 8
	errs := make(chan error, n)
	for i := 0; i < n; i++ {
		go func() {
			res, err := e.Transcribe(context.Background(), asr.Request{Samples: tone(1.5)})
			if err == nil && res.Text != "hello world this is neelkit" {
				err = errTextMismatch(res.Text)
			}
			errs <- err
		}()
	}
	for i := 0; i < n; i++ {
		if err := <-errs; err != nil {
			t.Fatalf("concurrent request %d: %v", i, err)
		}
	}
}

// Shutting down while requests are in flight must not free a session out from
// under a decode. The reference implementation closes its worker channel and
// ranges over it, leaking any worker that was checked out at that moment.
func TestCloseWaitsForInflightWork(t *testing.T) {
	e := newSyntheticEngine(t)

	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < 4; i++ {
			_, _ = e.Transcribe(context.Background(), asr.Request{Samples: tone(1)})
		}
	}()
	time.Sleep(20 * time.Millisecond)
	if err := e.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	select {
	case <-done:
	case <-time.After(30 * time.Second):
		t.Fatal("in-flight requests did not finish after Close")
	}
	// Close must be idempotent: the shutdown path calls it after Shutdown.
	if err := e.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
}

func TestInfoReportsLoadedModel(t *testing.T) {
	e := newSyntheticEngine(t)
	info := e.Info()
	if !info.Ready || info.Workers != 2 || info.Device != "cpu" {
		t.Fatalf("info = %+v", info)
	}
	if info.Precision != "fp32" {
		t.Errorf("precision = %q, want fp32 for the synthetic model", info.Precision)
	}
}

func TestUnsupportedDeviceFailsLoudly(t *testing.T) {
	if _, err := buildSessionOptions("tpu", 0); err == nil {
		t.Fatal("expected an error for an unknown device")
	}
	opts, err := buildSessionOptions("cpu", 0)
	if err != nil || opts != nil {
		t.Fatalf("cpu options = %v, %v; want nil, nil", opts, err)
	}
}

func wordStrings(ws []asr.Word) []string {
	out := make([]string, len(ws))
	for i, w := range ws {
		out[i] = w.Word
	}
	return out
}

type errTextMismatch string

func (e errTextMismatch) Error() string { return "unexpected transcript: " + string(e) }

// Warm-up runs through the ordinary Transcribe path, which refuses an engine
// that is not marked ready. Ordering the two wrongly made warm-up fail on every
// start, which is invisible except as a slow first request.
func TestWarmupRunsAtStartup(t *testing.T) {
	if _, err := os.Stat(filepath.Join(syntheticDir, "encoder-model.onnx")); err != nil {
		t.Skip("synthetic model missing; run `make testmodel`")
	}
	e, err := New(Config{
		ModelsDir:  syntheticDir,
		ORTLibrary: os.Getenv("ONNXRUNTIME_LIB"),
		Workers:    1,
		Device:     "cpu",
		Warmup:     true,
	})
	if err != nil {
		t.Skipf("engine unavailable: %v", err)
	}
	defer e.Close()

	if !e.Info().Ready {
		t.Fatal("engine reports not ready after a successful New")
	}
	// A warmed engine answers immediately; an unwarmed one pays graph
	// initialisation here. The assertion is only that it works at all.
	if _, err := e.Transcribe(context.Background(), asr.Request{Samples: tone(0.5)}); err != nil {
		t.Fatalf("first request after warm-up failed: %v", err)
	}
}
