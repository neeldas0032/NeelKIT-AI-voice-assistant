package vad

import (
	"math"
	"math/rand"
	"os"
	"path/filepath"
	"testing"

	ort "github.com/yalue/onnxruntime_go"
)

// modelPath resolves the Silero model, skipping the test when it is absent so
// `go test ./...` still passes on a machine that has not run `make models`.
func modelPath(t *testing.T) string {
	t.Helper()
	p := filepath.Join("..", "..", "models", "silero_vad.onnx")
	if _, err := os.Stat(p); err != nil {
		t.Skip("silero_vad.onnx not present; run `make models`")
	}
	if lib := os.Getenv("ONNXRUNTIME_LIB"); lib != "" {
		ort.SetSharedLibraryPath(lib)
	}
	if err := ort.InitializeEnvironment(); err != nil {
		t.Skipf("ONNX Runtime unavailable: %v", err)
	}
	return p
}

func TestSileroDistinguishesSilenceFromSignal(t *testing.T) {
	p := modelPath(t)
	defer ort.DestroyEnvironment()

	det, err := NewDetector(p)
	if err != nil {
		t.Fatalf("NewDetector: %v", err)
	}
	defer det.Close()

	silence := make([]float32, WindowSamples*30)

	// Speech-like: a 150 Hz glottal pulse train shaped by formant-ish tones,
	// which is far closer to voiced speech than white noise (Silero correctly
	// rejects white noise, so noise would be a misleading probe).
	rng := rand.New(rand.NewSource(1))
	voiced := make([]float32, WindowSamples*30)
	for i := range voiced {
		tt := float64(i) / SampleRate
		env := 0.5 * (1 - math.Cos(2*math.Pi*150*tt)) // pulse train envelope
		v := env * (0.6*math.Sin(2*math.Pi*700*tt) +
			0.3*math.Sin(2*math.Pi*1220*tt) +
			0.1*math.Sin(2*math.Pi*2600*tt))
		voiced[i] = float32(v*0.5 + rng.NormFloat64()*0.002)
	}

	mean := func(samples []float32) float32 {
		s := det.NewStream()
		s.Reset()
		probs, err := s.Probs(samples)
		if err != nil {
			t.Fatalf("Probs: %v", err)
		}
		if len(probs) == 0 {
			t.Fatal("no probabilities returned")
		}
		// Skip the first second: Silero's recurrent state is warming up.
		probs = probs[len(probs)/3:]
		var sum float32
		for _, p := range probs {
			sum += p
		}
		return sum / float32(len(probs))
	}

	qs, qv := mean(silence), mean(voiced)
	t.Logf("mean P(speech): silence=%.3f voiced=%.3f", qs, qv)

	if qs > 0.2 {
		t.Errorf("silence scored %.3f, expected near zero", qs)
	}
	if qv <= qs {
		t.Errorf("voiced signal scored %.3f, not above silence %.3f", qv, qs)
	}
}

func TestSileroRejectsWrongWindowSize(t *testing.T) {
	p := modelPath(t)
	defer ort.DestroyEnvironment()

	det, err := NewDetector(p)
	if err != nil {
		t.Fatalf("NewDetector: %v", err)
	}
	defer det.Close()

	if _, err := det.NewStream().Prob(make([]float32, 100)); err == nil {
		t.Fatal("expected an error for a 100-sample window")
	}
}

func TestNewDetectorMissingFile(t *testing.T) {
	if _, err := NewDetector(filepath.Join(t.TempDir(), "nope.onnx")); !os.IsNotExist(err) {
		t.Fatalf("err = %v, want os.ErrNotExist", err)
	}
}
