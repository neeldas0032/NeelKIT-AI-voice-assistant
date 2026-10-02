package vad

import (
	"os"
	"testing"

	"github.com/neeldas0032/NeelKIT-AI-voice-assistant/internal/audio"
	ort "github.com/yalue/onnxruntime_go"
)

// speechFixture returns real recorded speech. Synthetic tones are not a valid
// probe for Silero: it is trained to separate speech from other sound, so a
// sine stack scores as low as silence. The fixture path is configurable so the
// repository need not vendor a recording.
func speechFixture(t *testing.T) []float32 {
	t.Helper()
	p := os.Getenv("NEELKIT_SPEECH_FIXTURE")
	if p == "" {
		t.Skip("set NEELKIT_SPEECH_FIXTURE to a 16 kHz mono WAV of speech")
	}
	raw, err := os.ReadFile(p)
	if err != nil {
		t.Skipf("fixture unreadable: %v", err)
	}
	samples, err := audio.DecodeWAV(raw)
	if err != nil {
		t.Fatalf("decode fixture: %v", err)
	}
	return samples
}

// This is the test that actually proves the VAD works: real speech must score
// far above silence, and the endpointer driven by those probabilities must
// find a sane number of utterances in 30 seconds of continuous narration.
func TestSileroOnRealSpeech(t *testing.T) {
	p := modelPath(t)
	defer ort.DestroyEnvironment()
	samples := speechFixture(t)

	det, err := NewDetector(p)
	if err != nil {
		t.Fatalf("NewDetector: %v", err)
	}
	defer det.Close()

	st := det.NewStream()
	probs, err := st.Probs(samples)
	if err != nil {
		t.Fatalf("Probs: %v", err)
	}

	var speech int
	var sum float32
	for _, pr := range probs {
		sum += pr
		if pr >= 0.5 {
			speech++
		}
	}
	frac := float64(speech) / float64(len(probs))
	t.Logf("%d windows, mean P=%.3f, %.1f%% above 0.5", len(probs), sum/float32(len(probs)), frac*100)

	if frac < 0.5 {
		t.Fatalf("only %.1f%% of windows scored as speech in a narration clip", frac*100)
	}

	e := NewEndpointer(DefaultConfig())
	var starts, ends int
	for _, pr := range probs {
		switch e.Push(pr) {
		case EventSpeechStart:
			starts++
		case EventEndpoint:
			ends++
		}
	}
	t.Logf("endpointer: %d utterance starts, %d endpoints", starts, ends)
	if starts == 0 {
		t.Fatal("endpointer never triggered on continuous speech")
	}
	// Continuous narration should not shatter into dozens of fragments.
	if starts > 12 {
		t.Fatalf("%d utterances in 30s of narration: hangover is too short", starts)
	}
}
