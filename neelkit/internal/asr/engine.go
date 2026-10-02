// Package asr defines the speech-recognition contract the rest of NeelKIT
// depends on.
//
// Everything above this package talks to the Engine interface, never to ONNX
// Runtime. That boundary is what lets the HTTP layer, the assistant and the
// tool system be tested on a machine with no models and no native runtime
// installed, which in turn is what keeps CI honest and fast.
package asr

import (
	"context"
	"errors"
	"time"
)

// Errors the transport layer maps to specific status codes.
var (
	// ErrOverloaded means every worker is busy and the queue is full. It maps
	// to 503 with Retry-After, never to a silent wait: a caller that cannot
	// tell "slow" from "saturated" cannot back off correctly.
	ErrOverloaded = errors.New("asr: engine overloaded")
	// ErrClosed means the engine is shutting down.
	ErrClosed = errors.New("asr: engine closed")
	// ErrAudioTooLong means the input exceeds what the engine will process.
	ErrAudioTooLong = errors.New("asr: audio too long")
)

// Priority decides what gets shed first when the engine is saturated.
type Priority int

const (
	// PriorityFinal is a completed utterance from a live session. It queues,
	// and it is the last thing to be shed.
	//
	// It is deliberately the zero value. A caller that forgets to set a
	// priority gets the conservative behaviour — wait for a worker — rather
	// than having its request silently dropped under load.
	PriorityFinal Priority = iota
	// PriorityBatch is an uploaded file. It queues but yields to finals: a
	// human waiting on a live answer beats a file upload every time.
	PriorityBatch
	// PriorityPartial is a live preview transcript. It is never queued: if no
	// worker is free right now the partial is dropped, because a preview that
	// arrives late is worse than one that never arrives.
	PriorityPartial
)

func (p Priority) String() string {
	switch p {
	case PriorityPartial:
		return "partial"
	case PriorityFinal:
		return "final"
	case PriorityBatch:
		return "batch"
	}
	return "unknown"
}

// Request is one transcription job.
type Request struct {
	// Samples is mono 16 kHz audio in [-1, 1].
	Samples []float32
	// Language is an ISO-639-1 hint. The model is multilingual but the rest of
	// the pipeline is English-only, so this is currently advisory.
	Language string
	// Priority controls admission under load.
	Priority Priority
	// WantWords requests per-word timings. They cost almost nothing to produce
	// (the decoder already knows each token's timestep) but they make the
	// response larger, so callers opt in.
	WantWords bool
	// Causal marks audio from a live session. It is informational: every
	// request is a complete slice, so the engine always normalises with
	// whole-slice statistics, matching how the model was trained.
	Causal bool
}

// Word is one word with its span in the audio.
//
// Resolution is one encoder frame: 8x subsampling over a 10 ms hop, so 80 ms.
type Word struct {
	Word       string  `json:"word"`
	StartMS    int     `json:"-"`
	EndMS      int     `json:"-"`
	Start      float64 `json:"start"`
	End        float64 `json:"end"`
	Confidence float64 `json:"confidence"`
}

// Result is a completed transcription.
type Result struct {
	// Text is the decoder output with whitespace collapsed. It has NOT been
	// through the normalizer; that is a separate, testable stage.
	Text string `json:"text"`
	// Words is populated when Request.WantWords is set.
	Words []Word `json:"words,omitempty"`
	// Confidence is the geometric mean of each emitted token's softmax
	// probability. It is a real measurement, not a placeholder: a low value
	// genuinely indicates the model was unsure.
	Confidence float64 `json:"confidence"`
	// Duration is the length of the audio.
	Duration time.Duration `json:"-"`
	// Inference is wall-clock time spent in the model.
	Inference time.Duration `json:"-"`
	// QueueWait is how long the request waited for a worker.
	QueueWait time.Duration `json:"-"`
	// RTF is Inference divided by Duration. Below 1.0 means faster than real
	// time; it is the number that decides whether live transcription is viable.
	RTF float64 `json:"rtf"`
}

// Info describes the loaded engine, for /v1/models and /readyz.
type Info struct {
	Model     string `json:"model"`
	Precision string `json:"precision"`
	Device    string `json:"device"`
	Workers   int    `json:"workers"`
	Ready     bool   `json:"ready"`
}

// Engine transcribes audio. Implementations must be safe for concurrent use.
type Engine interface {
	// Transcribe decodes the request and returns the complete result.
	Transcribe(ctx context.Context, req Request) (Result, error)
	// TranscribeStream is Transcribe with incremental delivery: emit is called
	// with each newly decoded fragment of text, from the calling goroutine, so
	// implementations need no internal synchronisation around it.
	TranscribeStream(ctx context.Context, req Request, emit func(delta string)) (Result, error)
	// Info reports what is loaded.
	Info() Info
	// Close releases models and workers. It must wait for in-flight work.
	Close() error
}
