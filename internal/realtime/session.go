// Package realtime turns a live audio stream into transcripts and assistant
// turns.
//
// This is the part of NeelKIT that has no counterpart upstream. Parakeet is a
// full-context offline model: it cannot be fed frames as they arrive, and its
// training-time feature normalisation needs the whole utterance. So rather than
// pretending the model streams, the session schedules it:
//
//   - mel features are computed once, incrementally, as audio arrives;
//   - voice activity drives an endpointing state machine;
//   - while speech is active, a debounced partial decode runs over the trailing
//     few seconds and produces an unstable preview;
//   - when the endpoint fires, one final decode runs over the whole utterance,
//     and only that result reaches the assistant.
//
// The cost is the redundant compute in the partials, which is bounded by the
// cadence and the trailing window. The benefit is that the user sees words
// appear while they speak instead of staring at nothing.
package realtime

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"time"

	"github.com/neeldas0032/NeelKIT-AI-voice-assistant/internal/asr"
	"github.com/neeldas0032/NeelKIT-AI-voice-assistant/internal/audio"
	"github.com/neeldas0032/NeelKIT-AI-voice-assistant/internal/id"
	"github.com/neeldas0032/NeelKIT-AI-voice-assistant/internal/observability"
	"github.com/neeldas0032/NeelKIT-AI-voice-assistant/internal/vad"
)

// Mode selects how utterance boundaries are decided.
type Mode string

const (
	// ModeVAD detects speech automatically.
	ModeVAD Mode = "vad"
	// ModePushToTalk waits for an explicit commit from the client, which is
	// the right choice in a noisy room or on a shared call.
	ModePushToTalk Mode = "push_to_talk"
)

// EventType identifies an outbound event.
type EventType string

const (
	EventSessionCreated EventType = "session.created"
	EventSpeechStarted  EventType = "vad.speech_started"
	EventSpeechStopped  EventType = "vad.speech_stopped"
	EventPartial        EventType = "transcript.partial"
	EventFinal          EventType = "transcript.final"
	EventLagged         EventType = "stream.lagged"
	EventOverloaded     EventType = "stream.overloaded"
	EventError          EventType = "error"
)

// Event is sent to the client.
type Event struct {
	Type EventType `json:"type"`

	SessionID  string     `json:"session_id,omitempty"`
	Utterance  string     `json:"utterance_id,omitempty"`
	Text       string     `json:"text,omitempty"`
	RawText    string     `json:"raw_text,omitempty"`
	Stable     bool       `json:"stable,omitempty"`
	Confidence float64    `json:"confidence,omitempty"`
	Words      []asr.Word `json:"words,omitempty"`
	DurationMS int64      `json:"duration_ms,omitempty"`
	LatencyMS  int64      `json:"latency_ms,omitempty"`
	AtMS       int64      `json:"at_ms,omitempty"`
	Dropped    int        `json:"dropped,omitempty"`
	Message    string     `json:"message,omitempty"`
	Code       string     `json:"code,omitempty"`
}

// Config configures a session.
type Config struct {
	Engine  asr.Engine
	VAD     *vad.Detector
	Metrics *observability.Metrics

	Mode Mode
	// Partials enables preview transcripts.
	Partials bool
	// PartialEvery is the minimum gap between partial decodes. Below roughly
	// 250 ms the decodes overlap and the queue does nothing but shed.
	PartialEvery time.Duration
	// PartialWindow caps how much trailing audio a partial decode sees, so the
	// cost of a preview does not grow with the length of the utterance.
	PartialWindow time.Duration
	// HangoverMS is the silence required to end an utterance.
	HangoverMS int
	// MaxUtterance force-ends an utterance that never stops.
	MaxUtterance time.Duration
	// Language is an advisory hint passed to the engine.
	Language string
}

// OnFinal is called with each completed utterance. It runs on the session's own
// goroutine, so implementations must not block for long; the assistant work it
// triggers belongs on another goroutine.
type OnFinal func(ctx context.Context, ev Event)

// Session processes one live audio stream.
//
// A Session is driven by a single goroutine calling Write and Commit. It is not
// safe for concurrent use, which is deliberate: audio is inherently ordered, and
// a mutex around the pipeline would only hide a caller bug.
type Session struct {
	ID  string
	cfg Config

	ring   *audio.Ring
	fb     *audio.Filterbank
	stream *vad.Stream
	ep     *vad.Endpointer

	pending   []float32 // samples not yet forming a whole VAD window
	speaking  bool
	utterance string

	// The endpointer counts windows from its last Reset, while the ring counts
	// samples since the session began. base is the ring position of the
	// endpointer's window zero; without it, every utterance after a commit or
	// cancel would be sliced from the wrong place in the ring and the
	// recogniser would be handed old audio.
	base int64
	// pttFrom is where the current push-to-talk utterance began in the ring.
	pttFrom int64

	lastPartial time.Time

	// Preview decodes run on their own goroutine, at most one at a time, so a
	// slow decoder can never stall audio intake or endpoint detection.
	//
	// gen identifies the current utterance. It is bumped whenever an utterance
	// ends, so a preview that finishes late can tell it is stale and drop
	// itself instead of painting an old half-sentence under the final.
	partialBusy atomic.Bool
	gen         atomic.Uint64
	partialCost atomic.Int64 // nanoseconds the last preview took

	pmu         sync.Mutex
	partialText string // guarded by pmu
	partialGen  uint64 // the gen partialText belongs to; guarded by pmu

	send    func(Event)
	onFinal OnFinal

	mu     sync.Mutex
	closed bool
}

// NewSession creates a session. send is called for every outbound event, from
// the caller's goroutine.
func NewSession(cfg Config, send func(Event), onFinal OnFinal) (*Session, error) {
	if cfg.Engine == nil {
		return nil, errors.New("realtime: no ASR engine")
	}
	if cfg.VAD == nil {
		return nil, errors.New("realtime: no VAD detector")
	}
	if cfg.PartialEvery <= 0 {
		cfg.PartialEvery = 400 * time.Millisecond
	}
	if cfg.PartialWindow <= 0 {
		cfg.PartialWindow = 12 * time.Second
	}
	if cfg.MaxUtterance <= 0 {
		cfg.MaxUtterance = 30 * time.Second
	}
	if cfg.Mode == "" {
		cfg.Mode = ModeVAD
	}
	if cfg.Language == "" {
		cfg.Language = "en"
	}

	hangover := cfg.HangoverMS
	if hangover <= 0 {
		hangover = 600
	}
	epCfg := vad.DefaultConfig()
	epCfg.HangoverWindows = int(float64(hangover) / vad.WindowDuration.Seconds() / 1000)
	if epCfg.HangoverWindows < 1 {
		epCfg.HangoverWindows = 1
	}
	epCfg.MaxUtteranceWindows = int(cfg.MaxUtterance / vad.WindowDuration)

	// The ring holds a little more than the maximum utterance, so a final
	// decode can always reach back to the start of the speech it is about to
	// transcribe even if the client burst ahead.
	capacity := int((cfg.MaxUtterance + 10*time.Second).Seconds() * audio.SampleRate)

	return &Session{
		ID:      id.New(id.PrefixSession),
		cfg:     cfg,
		ring:    audio.NewRing(capacity),
		fb:      audio.NewFilterbank(audio.NumMels, audio.SampleRate),
		stream:  cfg.VAD.NewStream(),
		ep:      vad.NewEndpointer(epCfg),
		send:    send,
		onFinal: onFinal,
	}, nil
}

// Start emits the session.created event.
func (s *Session) Start() {
	if s.cfg.Metrics != nil {
		s.cfg.Metrics.StreamSessions.Inc()
	}
	s.send(Event{Type: EventSessionCreated, SessionID: s.ID})
}

// Close releases the session.
func (s *Session) Close() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return
	}
	s.closed = true
	if s.cfg.Metrics != nil {
		s.cfg.Metrics.StreamSessions.Dec()
	}
}

// Write feeds PCM samples into the pipeline.
//
// Samples are buffered until they fill whole VAD windows; a client sending
// 20 ms frames and a client sending 100 ms frames must behave identically, so
// the window boundary is imposed here rather than assumed of the caller.
func (s *Session) Write(ctx context.Context, samples []float32) {
	if len(samples) == 0 {
		return
	}
	s.ring.Write(samples)
	s.pending = append(s.pending, samples...)

	for len(s.pending) >= vad.WindowSamples {
		window := s.pending[:vad.WindowSamples]
		s.pending = s.pending[vad.WindowSamples:]

		prob, err := s.stream.Prob(window)
		if err != nil {
			// Still advance the endpointer, as silence: skipping the window
			// would put its count out of step with the ring for the rest of
			// the session.
			observability.Logger(ctx).Warn("vad inference failed", "error", err)
			prob = 0
		}
		s.onWindow(ctx, prob, s.ring.Len()-int64(len(s.pending))-vad.WindowSamples)
	}

	// Keep the pending slice from aliasing an ever-growing backing array.
	if len(s.pending) == 0 {
		s.pending = s.pending[:0]
	}
}

// onWindow advances the endpointer and reacts to its decision. at is the ring
// position where this window begins.
func (s *Session) onWindow(ctx context.Context, prob float32, at int64) {
	if s.cfg.Mode == ModePushToTalk {
		// The client decides where utterances begin and end, so the endpointer
		// is not consulted; audio simply accumulates until Commit.
		if !s.speaking {
			s.speaking = true
			s.pttFrom = at
			s.utterance = id.New("utt")
			s.send(Event{Type: EventSpeechStarted, Utterance: s.utterance})
		}
		s.maybePartial(ctx)
		return
	}

	switch s.ep.Push(prob) {
	case vad.EventSpeechStart:
		s.speaking = true
		s.utterance = id.New("utt")
		s.send(Event{
			Type: EventSpeechStarted, Utterance: s.utterance,
			AtMS: s.ep.WindowIndex() * vad.WindowDuration.Milliseconds(),
		})

	case vad.EventEndpoint:
		from, to := s.uttRange()
		s.send(Event{
			Type: EventSpeechStopped, Utterance: s.utterance,
			DurationMS: (to - from) * 1000 / audio.SampleRate,
		})
		s.endUtterance()
		s.finalize(ctx, from, to)
		s.speaking = false

	default:
		if s.speaking {
			s.maybePartial(ctx)
		}
	}
}

// Commit ends the current utterance immediately, for push-to-talk release or an
// explicit client request. The web client sends it whenever the microphone is
// switched off, so words spoken just before the click are not lost.
func (s *Session) Commit(ctx context.Context) {
	if !s.speaking {
		return
	}
	var from, to int64
	if s.cfg.Mode == ModePushToTalk {
		from, to = s.pttFrom, s.ring.Len()
		if oldest := s.ring.Oldest(); from < oldest {
			from = oldest
		}
	} else {
		s.ep.ForceEndpoint()
		from, to = s.uttRange()
	}
	s.send(Event{Type: EventSpeechStopped, Utterance: s.utterance})
	s.endUtterance()
	s.finalize(ctx, from, to)
	s.speaking = false
	s.restart(int64(len(s.pending)))
}

// Cancel discards the current utterance without transcribing it.
func (s *Session) Cancel() {
	s.speaking = false
	s.endUtterance()
	s.pending = s.pending[:0]
	s.restart(0)
}

// restart resets the detectors and re-anchors the endpointer to the ring.
// pending is how many ring samples have not yet been fed to the VAD.
func (s *Session) restart(pending int64) {
	s.ep.Reset()
	s.stream.Reset()
	s.base = s.ring.Len() - pending
}

// uttRange is the endpointer's current utterance as ring positions.
func (s *Session) uttRange() (from, to int64) {
	from, to = s.ep.UtteranceRange()
	return s.base + from, s.base + to
}

// endUtterance invalidates any preview still in flight for the utterance that
// is ending.
func (s *Session) endUtterance() {
	s.pmu.Lock()
	s.gen.Add(1)
	s.pmu.Unlock()
}

// partialInterval is the effective gap between previews: the configured
// cadence, or twice what the last preview actually cost, whichever is longer.
//
// On a GPU a preview takes tens of milliseconds and the configured cadence
// wins. On a laptop CPU the same decode can take most of a second, and running
// previews back to back would keep the encoder permanently busy, so the final
// decode — the one the user is actually waiting for — would always queue
// behind one. Backing off leaves the encoder idle about half the time.
func (s *Session) partialInterval() time.Duration {
	every := s.cfg.PartialEvery
	if cost := time.Duration(s.partialCost.Load()); 2*cost > every {
		every = 2 * cost
	}
	return every
}

// maybePartial starts a preview decode if the cadence allows and none is
// already running. It never blocks: the decode runs on its own goroutine.
func (s *Session) maybePartial(ctx context.Context) {
	if !s.cfg.Partials {
		return
	}
	if time.Since(s.lastPartial) < s.partialInterval() {
		return
	}
	if s.partialBusy.Load() {
		return
	}

	from := s.pttFrom
	if s.cfg.Mode == ModeVAD {
		from, _ = s.uttRange()
	}
	to := s.ring.Len()

	// Cap the trailing window so the cost of a preview does not grow with the
	// length of the utterance.
	maxSamples := int64(s.cfg.PartialWindow.Seconds() * audio.SampleRate)
	if to-from > maxSamples {
		from = to - maxSamples
	}
	if oldest := s.ring.Oldest(); from < oldest {
		from = oldest
	}
	if to-from < audio.SampleRate/2 { // less than half a second is not worth a pass
		return
	}

	// Slice copies, so the goroutine owns its samples and the ring can keep
	// being written.
	samples, ok := s.ring.Slice(from, to, nil)
	if !ok {
		return
	}
	s.lastPartial = time.Now()
	s.partialBusy.Store(true)
	gen := s.gen.Load()
	utterance := s.utterance

	go s.runPartial(ctx, samples, gen, utterance)
}

// runPartial performs one preview decode off the audio path.
func (s *Session) runPartial(ctx context.Context, samples []float32, gen uint64, utterance string) {
	defer s.partialBusy.Store(false)

	start := time.Now()
	res, err := s.cfg.Engine.Transcribe(ctx, asr.Request{
		Samples:  samples,
		Language: s.cfg.Language,
		Priority: asr.PriorityPartial,
		Causal:   true,
	})
	s.partialCost.Store(int64(time.Since(start)))
	if err != nil {
		if errors.Is(err, asr.ErrOverloaded) {
			// Expected under load: a preview that cannot run right now is
			// dropped rather than queued, because it would be stale on arrival.
			if s.cfg.Metrics != nil {
				s.cfg.Metrics.ASRShed.Inc("partial")
			}
			s.send(Event{Type: EventOverloaded, Message: "skipped a preview while busy"})
		}
		return
	}

	s.pmu.Lock()
	defer s.pmu.Unlock()
	if s.gen.Load() != gen {
		return // the utterance ended while this was decoding
	}
	if s.partialGen != gen {
		s.partialGen, s.partialText = gen, ""
	}
	if res.Text == "" || res.Text == s.partialText {
		return
	}
	s.partialText = res.Text

	if s.cfg.Metrics != nil {
		s.cfg.Metrics.StreamPartialLatency.Observe(time.Since(start).Seconds())
		s.cfg.Metrics.ASRDuration.Observe(res.Inference.Seconds(), "partial", "partial")
	}
	// Sent under pmu so that the staleness check and the send are atomic with
	// respect to endUtterance: once the final is on its way, no preview for the
	// same utterance can follow it.
	s.send(Event{
		Type: EventPartial, Utterance: utterance,
		Text: res.Text, Stable: false, Confidence: res.Confidence,
		LatencyMS: time.Since(start).Milliseconds(),
	})
}

// finalize decodes the completed utterance and hands it to the assistant.
func (s *Session) finalize(ctx context.Context, from, to int64) {
	if to <= from {
		return
	}
	if oldest := s.ring.Oldest(); from < oldest {
		from = oldest
	}
	samples, ok := s.ring.Slice(from, to, nil)
	if !ok || len(samples) < audio.SampleRate/10 {
		return
	}

	start := time.Now()
	res, err := s.cfg.Engine.Transcribe(ctx, asr.Request{
		Samples:   samples,
		Language:  s.cfg.Language,
		Priority:  asr.PriorityFinal,
		WantWords: true,
		Causal:    true,
	})
	if err != nil {
		if errors.Is(err, asr.ErrOverloaded) {
			if s.cfg.Metrics != nil {
				s.cfg.Metrics.ASRShed.Inc("final")
			}
			s.send(Event{Type: EventOverloaded, Code: "overloaded",
				Message: "the recogniser is busy; that utterance was dropped"})
			return
		}
		s.send(Event{Type: EventError, Code: "asr_failed", Message: err.Error()})
		return
	}
	if res.Text == "" {
		return
	}

	if s.cfg.Metrics != nil {
		s.cfg.Metrics.StreamFinalLatency.Observe(time.Since(start).Seconds())
		s.cfg.Metrics.ASRDuration.Observe(res.Inference.Seconds(), "final", "final")
		s.cfg.Metrics.ASRRTF.Observe(res.RTF)
	}

	ev := Event{
		Type: EventFinal, Utterance: s.utterance,
		Text: res.Text, RawText: res.Text, Stable: true,
		Confidence: res.Confidence, Words: res.Words,
		DurationMS: res.Duration.Milliseconds(),
		LatencyMS:  time.Since(start).Milliseconds(),
	}
	s.send(ev)
	if s.onFinal != nil {
		s.onFinal(ctx, ev)
	}
}

// Speaking reports whether an utterance is in progress.
func (s *Session) Speaking() bool { return s.speaking }
