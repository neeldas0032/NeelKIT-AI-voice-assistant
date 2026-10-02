// Derived from achetronic/parakeet (Apache-2.0), Copyright 2026 Alby Hernández:
// the Silero VAD ONNX contract and state handling.
// Reimplemented here with the changes described in ATTRIBUTION.md.
// SPDX-License-Identifier: Apache-2.0

package vad

import (
	"fmt"
	"os"
	"sync"

	ort "github.com/yalue/onnxruntime_go"
)

// Silero VAD v6.2.1 ONNX contract at 16 kHz.
const (
	// WindowSamples is the fixed analysis window: 512 samples = 32 ms.
	WindowSamples = 512
	// ContextSamples is the left context the model prepends to each window;
	// the trailing ContextSamples of one window become the context of the next.
	ContextSamples = 64
	// stateElements is the flattened recurrent state, shaped [2, 1, 128].
	stateElements = 2 * 1 * 128
	// SampleRate is the only rate this integration feeds the model.
	SampleRate = 16000
)

// Detector wraps the shared Silero ONNX session.
//
// The model is stateful, but its recurrent state travels through the input and
// output tensors rather than living inside the session, so one session serves
// any number of concurrent streams as long as each keeps its own Stream.
type Detector struct {
	mu      sync.Mutex
	session *ort.DynamicAdvancedSession
	srData  []int64
}

// NewDetector loads the Silero model from path.
//
// A missing file returns os.ErrNotExist so the caller can decide: the realtime
// path cannot work without VAD and must fail, while the batch path can fall
// back to energy-based boundaries.
func NewDetector(path string) (*Detector, error) {
	if _, err := os.Stat(path); err != nil {
		if os.IsNotExist(err) {
			return nil, os.ErrNotExist
		}
		return nil, fmt.Errorf("stat silero model: %w", err)
	}
	sess, err := ort.NewDynamicAdvancedSession(
		path,
		[]string{"input", "state", "sr"},
		[]string{"output", "stateN"},
		nil,
	)
	if err != nil {
		return nil, fmt.Errorf("create silero session: %w", err)
	}
	return &Detector{session: sess, srData: []int64{SampleRate}}, nil
}

// Close releases the ONNX session.
func (d *Detector) Close() {
	if d == nil {
		return
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.session != nil {
		d.session.Destroy()
		d.session = nil
	}
}

// Stream holds one audio stream's recurrent state and left context. Each
// concurrent stream needs its own; a Stream is not safe for concurrent use.
type Stream struct {
	det   *Detector
	state [stateElements]float32
	ctx   [ContextSamples]float32
	input []float32 // reused across calls
}

// NewStream returns per-stream VAD state bound to this detector.
func (d *Detector) NewStream() *Stream {
	return &Stream{det: d, input: make([]float32, ContextSamples+WindowSamples)}
}

// Reset clears the recurrent state and left context.
func (s *Stream) Reset() {
	for i := range s.state {
		s.state[i] = 0
	}
	for i := range s.ctx {
		s.ctx[i] = 0
	}
}

// Prob runs one window through the model and returns its speech probability.
// window must be exactly WindowSamples long.
func (s *Stream) Prob(window []float32) (float32, error) {
	if len(window) != WindowSamples {
		return 0, fmt.Errorf("silero window must be %d samples, got %d", WindowSamples, len(window))
	}
	copy(s.input, s.ctx[:])
	copy(s.input[ContextSamples:], window)
	copy(s.ctx[:], window[WindowSamples-ContextSamples:])

	in, err := ort.NewTensor(ort.NewShape(1, int64(len(s.input))), s.input)
	if err != nil {
		return 0, fmt.Errorf("vad input tensor: %w", err)
	}
	defer in.Destroy()

	stateData := make([]float32, stateElements)
	copy(stateData, s.state[:])
	stateIn, err := ort.NewTensor(ort.NewShape(2, 1, 128), stateData)
	if err != nil {
		return 0, fmt.Errorf("vad state tensor: %w", err)
	}
	defer stateIn.Destroy()

	sr, err := ort.NewTensor(ort.NewShape(1), s.det.srData)
	if err != nil {
		return 0, fmt.Errorf("vad sr tensor: %w", err)
	}
	defer sr.Destroy()

	out, err := ort.NewEmptyTensor[float32](ort.NewShape(1, 1))
	if err != nil {
		return 0, fmt.Errorf("vad output tensor: %w", err)
	}
	defer out.Destroy()

	stateOut, err := ort.NewEmptyTensor[float32](ort.NewShape(2, 1, 128))
	if err != nil {
		return 0, fmt.Errorf("vad state output tensor: %w", err)
	}
	defer stateOut.Destroy()

	// ORT Run is thread-safe on a shared session, but this model is small and
	// called once per 32 ms per stream; serialising keeps the tensor lifetime
	// reasoning trivial and costs nothing measurable.
	s.det.mu.Lock()
	err = s.det.session.Run(
		[]ort.Value{in, stateIn, sr},
		[]ort.Value{out, stateOut},
	)
	s.det.mu.Unlock()
	if err != nil {
		return 0, fmt.Errorf("vad run: %w", err)
	}

	copy(s.state[:], stateOut.GetData())
	return out.GetData()[0], nil
}

// Probs runs the model over a run of samples, returning one probability per
// complete window. Trailing samples that do not fill a window are ignored.
func (s *Stream) Probs(samples []float32) ([]float32, error) {
	n := len(samples) / WindowSamples
	if n == 0 {
		return nil, nil
	}
	out := make([]float32, 0, n)
	for i := 0; i < n; i++ {
		p, err := s.Prob(samples[i*WindowSamples : (i+1)*WindowSamples])
		if err != nil {
			return out, err
		}
		out = append(out, p)
	}
	return out, nil
}
