package vad

import "testing"

// seq builds a probability sequence from (value, repeat) pairs.
func seq(pairs ...any) []float32 {
	var out []float32
	for i := 0; i < len(pairs); i += 2 {
		v := float32(pairs[i].(float64))
		n := pairs[i+1].(int)
		for j := 0; j < n; j++ {
			out = append(out, v)
		}
	}
	return out
}

func run(e *Endpointer, probs []float32) []Event {
	var events []Event
	for _, p := range probs {
		if ev := e.Push(p); ev != EventNone {
			events = append(events, ev)
		}
	}
	return events
}

func TestEndpointerBasicUtterance(t *testing.T) {
	e := NewEndpointer(Config{StartWindows: 3, HangoverWindows: 5})
	events := run(e, seq(0.0, 10, 0.9, 20, 0.0, 10))

	if len(events) != 2 || events[0] != EventSpeechStart || events[1] != EventEndpoint {
		t.Fatalf("events = %v, want [SpeechStart Endpoint]", events)
	}
	if e.State() != StateIdle {
		t.Fatalf("state = %v after endpoint, want idle", e.State())
	}
}

// The whole point of the hangover: a pause between words must not split one
// sentence into two utterances.
func TestEndpointerIgnoresShortPause(t *testing.T) {
	e := NewEndpointer(Config{StartWindows: 3, HangoverWindows: 10})
	events := run(e, seq(0.9, 10, 0.0, 4, 0.9, 10, 0.0, 15))

	if len(events) != 2 {
		t.Fatalf("events = %v, want exactly one start and one endpoint", events)
	}
	if events[0] != EventSpeechStart || events[1] != EventEndpoint {
		t.Fatalf("events = %v", events)
	}
}

func TestEndpointerDebouncesSingleSpike(t *testing.T) {
	e := NewEndpointer(Config{StartWindows: 3, HangoverWindows: 5})
	// A door knock: two loud windows, then quiet. Must not start an utterance.
	if events := run(e, seq(0.0, 5, 0.95, 2, 0.0, 20)); len(events) != 0 {
		t.Fatalf("events = %v, want none for a 2-window spike", events)
	}
}

// Hysteresis: probabilities oscillating between the two thresholds must not
// flip the state machine back and forth.
func TestEndpointerHysteresis(t *testing.T) {
	e := NewEndpointer(Config{StartThreshold: 0.5, StopThreshold: 0.35, StartWindows: 3, HangoverWindows: 5})
	run(e, seq(0.9, 5))
	if e.State() != StateSpeech {
		t.Fatalf("state = %v, want speech", e.State())
	}
	// 0.4 is between the thresholds: neither speech nor silence.
	if events := run(e, seq(0.4, 20)); len(events) != 0 {
		t.Fatalf("events = %v inside the hysteresis band, want none", events)
	}
	if e.State() != StateSpeech {
		t.Fatalf("state = %v after ambiguous windows, want speech", e.State())
	}
}

func TestEndpointerMaxUtterance(t *testing.T) {
	e := NewEndpointer(Config{StartWindows: 2, HangoverWindows: 100, MaxUtteranceWindows: 20})
	events := run(e, seq(0.9, 60))

	if len(events) < 2 {
		t.Fatalf("events = %v, want a start and at least one forced endpoint", events)
	}
	if events[1] != EventEndpoint {
		t.Fatalf("second event = %v, want a forced Endpoint at the cap", events[1])
	}
}

func TestEndpointerForceEndpoint(t *testing.T) {
	e := NewEndpointer(Config{StartWindows: 2})
	if e.ForceEndpoint() {
		t.Fatal("ForceEndpoint reported true while idle")
	}
	run(e, seq(0.9, 5))
	if !e.ForceEndpoint() {
		t.Fatal("ForceEndpoint reported false during speech")
	}
	if e.Active() {
		t.Fatal("still active after ForceEndpoint")
	}
}

// The windows that proved speech started belong to the utterance. Clipping
// them removes the first phoneme, which is exactly where ASR errors are worst.
func TestEndpointerBackDatesStart(t *testing.T) {
	e := NewEndpointer(Config{StartWindows: 3, HangoverWindows: 5, PreRollWindows: -1})
	run(e, seq(0.0, 10, 0.9, 3))

	from, _ := e.UtteranceRange()
	wantFrom := int64(10 * WindowSamples) // first speech window, not the third
	if from != wantFrom {
		t.Fatalf("utterance starts at sample %d, want %d (start not back-dated)", from, wantFrom)
	}
}

// Silero's probability rises after a word has already begun, so the audio
// just before the first confident window is kept too.
func TestEndpointerKeepsPreRoll(t *testing.T) {
	e := NewEndpointer(Config{StartWindows: 3, HangoverWindows: 5, PreRollWindows: 4})
	run(e, seq(0.0, 10, 0.9, 3))

	from, _ := e.UtteranceRange()
	if want := int64(6 * WindowSamples); from != want {
		t.Fatalf("utterance starts at sample %d, want %d (4 windows of pre-roll)", from, want)
	}
}

// Pre-roll must not reach back into the previous utterance, or its last word
// is transcribed a second time at the start of this one.
func TestPreRollStopsAtThePreviousUtterance(t *testing.T) {
	e := NewEndpointer(Config{StartWindows: 1, HangoverWindows: 2, PreRollWindows: 10, PostRollWindows: 1})
	run(e, seq(0.9, 5, 0.0, 2)) // first utterance: windows 0-4, ends at 6
	firstFrom, firstEnd := e.UtteranceRange()
	// Read after the endpoint has fired, which is exactly when the session
	// slices the audio for the final decode. It must not be empty.
	if firstEnd-firstFrom != int64(6*WindowSamples) {
		t.Fatalf("finished utterance spans %d samples, want %d", firstEnd-firstFrom, 6*WindowSamples)
	}

	run(e, seq(0.0, 2, 0.9, 3)) // second begins at window 9
	from, _ := e.UtteranceRange()
	if from < firstEnd {
		t.Fatalf("second utterance starts at %d, inside the first (ended at %d)", from, firstEnd)
	}
}

func TestEndpointerTrimsTrailingSilence(t *testing.T) {
	e := NewEndpointer(Config{StartWindows: 2, HangoverWindows: 6, PostRollWindows: 1})
	run(e, seq(0.9, 10, 0.0, 6))

	_, to := e.UtteranceRange()
	// 10 speech + 6 silence windows seen; keep one window of padding.
	if want := int64(11 * WindowSamples); to != want {
		t.Fatalf("utterance ends at sample %d, want %d", to, want)
	}
}

// The default post-roll keeps enough of the tail for a fading consonant, but
// never more than was actually heard.
func TestEndpointerDefaultPostRoll(t *testing.T) {
	e := NewEndpointer(Config{StartWindows: 2, HangoverWindows: 19})
	run(e, seq(0.9, 10, 0.0, 19))

	_, to := e.UtteranceRange()
	want := int64((10 + DefaultConfig().PostRollWindows) * WindowSamples)
	if to != want {
		t.Fatalf("utterance ends at sample %d, want %d", to, want)
	}
}

func TestEndpointerReset(t *testing.T) {
	e := NewEndpointer(DefaultConfig())
	run(e, seq(0.9, 20))
	e.Reset()
	if e.Active() || e.WindowIndex() != 0 {
		t.Fatalf("after Reset: active=%v index=%d", e.Active(), e.WindowIndex())
	}
}

func TestDefaultConfigTimings(t *testing.T) {
	c := DefaultConfig()
	if got := float64(c.HangoverWindows) * WindowDuration.Seconds(); got < 0.55 || got > 0.65 {
		t.Fatalf("default hangover is %.3fs, want ~0.6s", got)
	}
	if c.StopThreshold >= c.StartThreshold {
		t.Fatal("stop threshold must be below start threshold for hysteresis")
	}
}
