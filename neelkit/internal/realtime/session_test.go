package realtime

import (
	"context"
	"math"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	ort "github.com/yalue/onnxruntime_go"

	"github.com/neeldas0032/NeelKIT-AI-voice-assistant/internal/asr"
	"github.com/neeldas0032/NeelKIT-AI-voice-assistant/internal/asr/fake"
	"github.com/neeldas0032/NeelKIT-AI-voice-assistant/internal/audio"
	"github.com/neeldas0032/NeelKIT-AI-voice-assistant/internal/vad"
)

// detector loads the real Silero model, skipping when it or ONNX Runtime is
// absent so `go test ./...` still passes on a clean checkout.
func detector(t *testing.T) *vad.Detector {
	t.Helper()
	p := filepath.Join("..", "..", "models", "silero_vad.onnx")
	if _, err := os.Stat(p); err != nil {
		t.Skip("silero_vad.onnx not present; run `make models`")
	}
	if lib := os.Getenv("ONNXRUNTIME_LIB"); lib != "" {
		ort.SetSharedLibraryPath(lib)
	}
	if !ort.IsInitialized() {
		if err := ort.InitializeEnvironment(); err != nil {
			t.Skipf("ONNX Runtime unavailable: %v", err)
		}
	}
	det, err := vad.NewDetector(p)
	if err != nil {
		t.Fatalf("NewDetector: %v", err)
	}
	t.Cleanup(det.Close)
	return det
}

type recorder struct {
	mu     sync.Mutex
	events []Event
}

func (r *recorder) send(ev Event) {
	r.mu.Lock()
	r.events = append(r.events, ev)
	r.mu.Unlock()
}

func (r *recorder) snapshot() []Event {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]Event(nil), r.events...)
}

// feed writes seconds of audio in 20 ms frames, the way the browser does.
func feed(ctx context.Context, s *Session, seconds float64) {
	frame := make([]float32, audio.SampleRate/50)
	for i := 0; i < int(seconds*50); i++ {
		s.Write(ctx, frame)
	}
}

// A preview decode that is slower than the preview cadence must not hold up
// the audio path.
//
// This is the failure that made voice unusable on a CPU laptop: partials ran
// inline in Write, so every 32 ms VAD window queued behind a full decode, and
// once a decode took longer than PartialEvery, every subsequent window started
// another one. Three seconds of speech became tens of seconds of decoding
// before the endpoint could even be seen. The fake engine is deliberately
// slower than the cadence to recreate exactly that condition.
func TestSlowPartialsDoNotStallAudioIntake(t *testing.T) {
	det := detector(t)
	eng := fake.New("list the files")
	eng.Delay = 250 * time.Millisecond

	rec := &recorder{}
	s, err := NewSession(Config{
		Engine: eng, VAD: det,
		Mode:         ModePushToTalk,
		Partials:     true,
		PartialEvery: 50 * time.Millisecond,
	}, rec.send, nil)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()

	done := make(chan time.Duration, 1)
	go func() {
		start := time.Now()
		feed(ctx, s, 3)
		done <- time.Since(start)
	}()

	select {
	case took := <-done:
		// Three seconds of audio is 150 frames; ingesting them is VAD work
		// only, which is well under a second on any machine.
		if took > 2*time.Second {
			t.Fatalf("writing 3 s of audio took %s; partial decodes are blocking intake", took)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("writing 3 s of audio did not finish in 10 s; partial decodes are blocking intake")
	}

	// At most one preview is ever in flight, so the number of decodes is
	// bounded by elapsed time / decode cost, not by the number of windows.
	time.Sleep(300 * time.Millisecond)
	partials := eng.CallCount(asr.PriorityPartial)
	if partials > 6 {
		t.Fatalf("%d partial decodes for ~1 s of wall time at 250 ms each; they are piling up", partials)
	}
}

// A preview that finishes after its utterance has ended must be discarded, or
// the interface shows a stale half-sentence under the final transcript.
func TestLatePartialIsDroppedAfterFinal(t *testing.T) {
	det := detector(t)
	eng := fake.New("list the files")
	eng.Delay = 200 * time.Millisecond

	rec := &recorder{}
	s, err := NewSession(Config{
		Engine: eng, VAD: det,
		Mode:         ModePushToTalk,
		Partials:     true,
		PartialEvery: 10 * time.Millisecond,
	}, rec.send, nil)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()

	feed(ctx, s, 1)
	s.Commit(ctx) // a partial is almost certainly still in flight here
	time.Sleep(500 * time.Millisecond)

	seenFinal := false
	for _, ev := range rec.snapshot() {
		switch ev.Type {
		case EventFinal:
			seenFinal = true
		case EventPartial:
			if seenFinal {
				t.Fatalf("a preview arrived after the final transcript: %q", ev.Text)
			}
		}
	}
	if !seenFinal {
		t.Fatal("no final transcript after commit")
	}
}

// finals returns the sample slices handed to the engine for final decodes.
func finals(e *fake.Engine) [][]float32 {
	var out [][]float32
	for _, c := range e.Snapshot() {
		if c.Priority == asr.PriorityFinal {
			out = append(out, c.Samples)
		}
	}
	return out
}

// Each push-to-talk utterance is only what was said during that press. Before
// this was anchored, every commit decoded the ring from the start, so the
// second answer was computed from the first question as well.
func TestPushToTalkUtterancesDoNotOverlap(t *testing.T) {
	det := detector(t)
	eng := fake.New("first", "second")
	s, err := NewSession(Config{Engine: eng, VAD: det, Mode: ModePushToTalk}, func(Event) {}, nil)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()

	feed(ctx, s, 1)
	s.Commit(ctx)
	feed(ctx, s, 0.5)
	s.Commit(ctx)

	f := finals(eng)
	if len(f) != 2 {
		t.Fatalf("%d final decodes, want 2", len(f))
	}
	second := float64(len(f[1])) / audio.SampleRate
	if second < 0.4 || second > 0.6 {
		t.Fatalf("second utterance is %.2fs of audio, want ~0.5s", second)
	}
}

// speech loads a real recording of "list the files in this project". It was
// synthesised with espeak-ng, so it carries no licence and no voice of a real
// person, but Silero hears it as speech, which a synthetic tone does not.
func speech(t *testing.T) []float32 {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", "list-the-files.wav"))
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	x, err := audio.DecodeWAV(raw)
	if err != nil {
		t.Fatalf("decode fixture: %v", err)
	}
	return x
}

func writeFrames(ctx context.Context, s *Session, x []float32) {
	for len(x) > 0 {
		n := min(len(x), audio.SampleRate/50)
		s.Write(ctx, x[:n])
		x = x[n:]
	}
}

func loudFraction(x []float32) float64 {
	n := 0
	for _, v := range x {
		if math.Abs(float64(v)) > 0.01 {
			n++
		}
	}
	return float64(n) / float64(len(x))
}

// After a cancel, the endpointer restarts its window count but the ring does
// not. The next utterance must still be sliced from where it actually is.
func TestUtteranceAfterCancelIsSlicedFromTheRightAudio(t *testing.T) {
	det := detector(t)
	eng := fake.New("one", "two")
	s, err := NewSession(Config{Engine: eng, VAD: det, Mode: ModeVAD}, func(Event) {}, nil)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	silence := func(sec float64) []float32 { return make([]float32, int(sec*audio.SampleRate)) }

	said := speech(t)

	writeFrames(ctx, s, silence(3))
	writeFrames(ctx, s, said)
	writeFrames(ctx, s, silence(1))
	if n := len(finals(eng)); n != 1 {
		t.Fatalf("%d final decodes after the first sentence, want 1", n)
	}

	writeFrames(ctx, s, silence(0.01)) // leave a partial window pending
	s.Cancel()

	writeFrames(ctx, s, silence(0.3))
	writeFrames(ctx, s, said)
	writeFrames(ctx, s, silence(1))

	f := finals(eng)
	if len(f) != 2 {
		t.Fatalf("%d final decodes, want 2", len(f))
	}
	if frac := loudFraction(f[1]); frac < 0.5 {
		t.Fatalf("second utterance is only %.0f%% speech; it was sliced from the wrong place in the ring", frac*100)
	}
}
