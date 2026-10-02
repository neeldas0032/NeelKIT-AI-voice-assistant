// Package fake provides an asr.Engine that needs no models and no native
// runtime.
//
// Every layer above ASR — the HTTP API, the realtime session, the assistant,
// the tool system — is tested against this. That is what lets `go test ./...`
// pass on a clean checkout with no 670 MB download and no ONNX Runtime
// installed, which in turn is what keeps CI fast enough to run on every push.
package fake

import (
	"context"
	"strings"
	"sync"
	"time"

	"github.com/neeldas0032/NeelKIT-AI-voice-assistant/internal/asr"
	"github.com/neeldas0032/NeelKIT-AI-voice-assistant/internal/audio"
)

// Engine returns scripted transcripts.
type Engine struct {
	mu sync.Mutex
	// Script is returned in order, one entry per call; the last entry repeats.
	Script []string
	// Err, when set, is returned instead of a result.
	Err error
	// Delay simulates inference time, for testing timeouts and queueing.
	Delay time.Duration
	// Calls records every request received, for assertions.
	Calls []asr.Request

	idx    int
	closed bool
}

// New returns a fake engine that yields the given transcripts in order.
func New(script ...string) *Engine {
	if len(script) == 0 {
		script = []string{"hello world"}
	}
	return &Engine{Script: script}
}

func (e *Engine) next() string {
	if len(e.Script) == 0 {
		return ""
	}
	i := e.idx
	if i >= len(e.Script) {
		i = len(e.Script) - 1
	}
	e.idx++
	return e.Script[i]
}

// CallCount reports how many requests of the given priority have been
// received. It is safe to call while decodes are in flight.
func (e *Engine) CallCount(p asr.Priority) int {
	e.mu.Lock()
	defer e.mu.Unlock()
	n := 0
	for _, c := range e.Calls {
		if c.Priority == p {
			n++
		}
	}
	return n
}

// Snapshot returns a copy of the requests received so far. It is safe to call
// while decodes are in flight.
func (e *Engine) Snapshot() []asr.Request {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]asr.Request(nil), e.Calls...)
}

// Transcribe implements asr.Engine.
func (e *Engine) Transcribe(ctx context.Context, req asr.Request) (asr.Result, error) {
	return e.TranscribeStream(ctx, req, nil)
}

// TranscribeStream implements asr.Engine.
func (e *Engine) TranscribeStream(ctx context.Context, req asr.Request, emit func(string)) (asr.Result, error) {
	e.mu.Lock()
	if e.closed {
		e.mu.Unlock()
		return asr.Result{}, asr.ErrClosed
	}
	e.Calls = append(e.Calls, req)
	err := e.Err
	delay := e.Delay
	text := e.next()
	e.mu.Unlock()

	if err != nil {
		return asr.Result{}, err
	}
	if delay > 0 {
		select {
		case <-time.After(delay):
		case <-ctx.Done():
			return asr.Result{}, ctx.Err()
		}
	}
	if err := ctx.Err(); err != nil {
		return asr.Result{}, err
	}

	if emit != nil {
		for i, w := range strings.Fields(text) {
			if i > 0 {
				emit(" ")
			}
			emit(w)
		}
	}

	duration := time.Duration(len(req.Samples)) * time.Second / audio.SampleRate
	res := asr.Result{
		Text:       text,
		Confidence: 0.95,
		Duration:   duration,
		Inference:  delay,
		RTF:        0.1,
	}
	if req.WantWords {
		// Evenly spaced words: enough structure for a caller to exercise
		// timing code without pretending to be a real alignment.
		words := strings.Fields(text)
		if n := len(words); n > 0 {
			per := duration.Seconds() / float64(n)
			for i, w := range words {
				res.Words = append(res.Words, asr.Word{
					Word:       w,
					Start:      float64(i) * per,
					End:        float64(i+1) * per,
					StartMS:    int(float64(i) * per * 1000),
					EndMS:      int(float64(i+1) * per * 1000),
					Confidence: 0.95,
				})
			}
		}
	}
	return res, nil
}

// Info implements asr.Engine.
func (e *Engine) Info() asr.Info {
	e.mu.Lock()
	defer e.mu.Unlock()
	return asr.Info{
		Model: "fake", Precision: "none", Device: "cpu",
		Workers: 1, Ready: !e.closed,
	}
}

// Close implements asr.Engine.
func (e *Engine) Close() error {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.closed = true
	return nil
}

// Reset rewinds the script and clears recorded calls.
func (e *Engine) Reset() {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.idx = 0
	e.Calls = nil
}

var _ asr.Engine = (*Engine)(nil)
