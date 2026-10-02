// Package vad detects speech in an audio stream and decides when an utterance
// has ended.
//
// Endpointing is what makes a voice assistant feel responsive. Without it the
// system either waits for the user to press a button or guesses at fixed
// intervals, cutting words in half. The detector here is a pure state machine
// over a sequence of speech probabilities, which makes it exhaustively
// testable without any model.
package vad

import "time"

// State is the endpointer's current position in an utterance.
type State int

const (
	// StateIdle: no speech detected; audio is being discarded.
	StateIdle State = iota
	// StateSpeech: speech is in progress; audio is accumulating.
	StateSpeech
	// StateHangover: speech stopped but the grace period has not elapsed. A
	// pause mid-sentence lives here and returns to StateSpeech.
	StateHangover
)

func (s State) String() string {
	switch s {
	case StateIdle:
		return "idle"
	case StateSpeech:
		return "speech"
	case StateHangover:
		return "hangover"
	}
	return "unknown"
}

// Event is emitted when the endpointer changes its mind about the utterance.
type Event int

const (
	// EventNone: no transition on this window.
	EventNone Event = iota
	// EventSpeechStart: an utterance began.
	EventSpeechStart
	// EventEndpoint: the utterance is complete and should be decoded.
	EventEndpoint
)

// Config tunes the endpointer. The defaults come from what the pipeline
// actually needs rather than from the model: Silero emits one probability per
// 32 ms window at 16 kHz, so the counts below are in those windows.
type Config struct {
	// StartThreshold is the probability above which a window counts as speech.
	StartThreshold float32
	// StopThreshold is the probability below which a window counts as silence.
	// It is deliberately lower than StartThreshold: a single hysteresis band
	// stops the state machine oscillating on a marginal window, which would
	// otherwise chop one sentence into three utterances.
	StopThreshold float32
	// StartWindows is how many consecutive speech windows begin an utterance.
	// Requiring more than one debounces keyboard clicks and door knocks.
	StartWindows int
	// HangoverWindows is how many consecutive silence windows end it. This is
	// the single biggest lever on perceived latency: too short truncates a
	// thinking pause, too long makes the assistant feel sluggish.
	HangoverWindows int
	// MaxUtteranceWindows force-ends an utterance that never stops, so a noisy
	// room cannot grow one utterance until it exceeds the model's frame limit.
	MaxUtteranceWindows int
	// PreRollWindows is audio kept from before the first window Silero was
	// confident about. Its probability rises a little after the onset of a
	// word, so without this the first consonant ("l" in "list", "sh" in
	// "show") is cut off, and the recogniser starts mid-phoneme — which is
	// exactly where it makes its worst mistakes. Zero means the default;
	// negative disables it.
	PreRollWindows int
	// PostRollWindows is audio kept after the last speech window, for the same
	// reason at the other end: a trailing fricative fades below the threshold
	// before it has actually finished. Zero means the default; negative keeps
	// only the speech itself.
	PostRollWindows int
}

// DefaultConfig returns tuned defaults: 600 ms hangover, 96 ms to trigger,
// 30 s maximum utterance, ~320 ms of pre-roll and ~190 ms of post-roll.
func DefaultConfig() Config {
	return Config{
		StartThreshold:      0.5,
		StopThreshold:       0.35,
		StartWindows:        3,
		HangoverWindows:     19,
		MaxUtteranceWindows: 937,
		PreRollWindows:      10,
		PostRollWindows:     6,
	}
}

// WindowDuration is the wall-clock length of one Silero window at 16 kHz.
const WindowDuration = time.Duration(WindowSamples) * time.Second / SampleRate

// Endpointer converts a stream of speech probabilities into utterance
// boundaries. It is a pure state machine: no I/O, no clock, no randomness.
type Endpointer struct {
	cfg Config

	state      State
	speechRun  int
	silenceRun int
	windows    int   // windows since the current utterance began
	startIdx   int64 // absolute window index where the utterance began
	endIdx     int64 // absolute window index one past the last kept window
	lastEnd    int64 // endIdx of the most recently finished utterance
	floor      int64 // lastEnd as it was when this utterance began; pre-roll stops here
	index      int64 // absolute window counter
}

// NewEndpointer creates an endpointer with the given configuration. Zero-valued
// fields fall back to the defaults so a partially filled Config is still usable.
func NewEndpointer(cfg Config) *Endpointer {
	d := DefaultConfig()
	if cfg.StartThreshold <= 0 {
		cfg.StartThreshold = d.StartThreshold
	}
	if cfg.StopThreshold <= 0 {
		cfg.StopThreshold = d.StopThreshold
	}
	if cfg.StartWindows <= 0 {
		cfg.StartWindows = d.StartWindows
	}
	if cfg.HangoverWindows <= 0 {
		cfg.HangoverWindows = d.HangoverWindows
	}
	if cfg.MaxUtteranceWindows <= 0 {
		cfg.MaxUtteranceWindows = d.MaxUtteranceWindows
	}
	switch {
	case cfg.PreRollWindows == 0:
		cfg.PreRollWindows = d.PreRollWindows
	case cfg.PreRollWindows < 0:
		cfg.PreRollWindows = 0
	}
	switch {
	case cfg.PostRollWindows == 0:
		cfg.PostRollWindows = d.PostRollWindows
	case cfg.PostRollWindows < 0:
		cfg.PostRollWindows = 0
	}
	return &Endpointer{cfg: cfg, state: StateIdle}
}

// Push feeds one window's speech probability and returns any resulting event.
func (e *Endpointer) Push(prob float32) Event {
	idx := e.index
	e.index++

	speech := prob >= e.cfg.StartThreshold
	silence := prob < e.cfg.StopThreshold

	if speech {
		e.speechRun++
		e.silenceRun = 0
	} else if silence {
		e.silenceRun++
		e.speechRun = 0
	} else {
		// Inside the hysteresis band: hold both runs. The window is neither
		// evidence for starting nor for stopping.
		return e.tickActive()
	}

	switch e.state {
	case StateIdle:
		if e.speechRun >= e.cfg.StartWindows {
			e.state = StateSpeech
			e.windows = e.speechRun
			// Back-date the start so the leading windows that proved it was
			// speech are part of the utterance, not clipped off it.
			e.startIdx = idx - int64(e.speechRun-1)
			e.floor = e.lastEnd
			return EventSpeechStart
		}

	case StateSpeech:
		e.windows++
		if e.silenceRun > 0 {
			e.state = StateHangover
		}
		if e.windows >= e.cfg.MaxUtteranceWindows {
			return e.endUtterance()
		}

	case StateHangover:
		e.windows++
		if speech {
			// A pause inside a sentence, not the end of one.
			e.state = StateSpeech
		} else if e.silenceRun >= e.cfg.HangoverWindows {
			return e.endUtterance()
		}
		if e.windows >= e.cfg.MaxUtteranceWindows {
			return e.endUtterance()
		}
	}
	return EventNone
}

// tickActive advances the utterance length for a window that was neither clear
// speech nor clear silence, applying only the hard maximum.
func (e *Endpointer) tickActive() Event {
	if e.state == StateIdle {
		return EventNone
	}
	e.windows++
	if e.windows >= e.cfg.MaxUtteranceWindows {
		return e.endUtterance()
	}
	return EventNone
}

func (e *Endpointer) endUtterance() Event {
	// Freeze where the utterance ends before the run counters are cleared.
	e.endIdx = e.paddedEnd()
	e.lastEnd = e.endIdx
	e.state = StateIdle
	e.speechRun = 0
	e.silenceRun = 0
	return EventEndpoint
}

// paddedEnd is one past the last speech window, plus post-roll, never beyond
// what has actually been seen. Trailing silence costs encoder time and adds
// nothing, so only the post-roll of it is kept.
func (e *Endpointer) paddedEnd() int64 {
	end := e.index - int64(e.silenceRun) + int64(e.cfg.PostRollWindows)
	if end > e.index {
		end = e.index
	}
	if end < e.startIdx {
		end = e.startIdx
	}
	return end
}

// ForceEndpoint ends the current utterance immediately, for push-to-talk
// release or an explicit client commit. It reports false when no utterance is
// in progress, so a stray commit does not manufacture an empty transcript.
func (e *Endpointer) ForceEndpoint() bool {
	if e.state == StateIdle {
		return false
	}
	e.endUtterance()
	return true
}

// State returns the current state.
func (e *Endpointer) State() State { return e.state }

// Active reports whether an utterance is in progress.
func (e *Endpointer) Active() bool { return e.state != StateIdle }

// UtteranceRange returns the sample range of the current or just-finished
// utterance, counted from the last Reset: the speech itself plus pre-roll and
// post-roll, with the rest of the hangover silence trimmed from the end.
func (e *Endpointer) UtteranceRange() (from, to int64) {
	start := e.startIdx - int64(e.cfg.PreRollWindows)
	if start < e.floor {
		start = e.floor // never re-transcribe the tail of the previous utterance
	}
	if start < 0 {
		start = 0
	}

	endWindow := e.endIdx
	if e.state != StateIdle {
		endWindow = e.paddedEnd()
	}

	from = start * WindowSamples
	to = endWindow * WindowSamples
	if to < from {
		to = from
	}
	return from, to
}

// WindowIndex returns how many windows have been processed.
func (e *Endpointer) WindowIndex() int64 { return e.index }

// Reset returns the endpointer to its initial state, keeping the configuration.
func (e *Endpointer) Reset() {
	e.state = StateIdle
	e.speechRun = 0
	e.silenceRun = 0
	e.windows = 0
	e.startIdx = 0
	e.endIdx = 0
	e.lastEnd = 0
	e.floor = 0
	e.index = 0
}
