// Derived from achetronic/parakeet (Apache-2.0), Copyright 2026 Alby Hernández:
// the tensor-reusing decoder worker pool.
// Reimplemented here with the changes described in ATTRIBUTION.md.
// SPDX-License-Identifier: Apache-2.0

package onnx

import (
	"context"
	"errors"
	"fmt"
	"sync"

	ort "github.com/yalue/onnxruntime_go"
)

// errPoolClosed is returned when a worker is requested after shutdown began.
var errPoolClosed = errors.New("asr: decoder pool closed")

// decoderWorker owns one decoder+joint session and every tensor it needs.
//
// The tensors are allocated once and written in place through GetData(), so the
// decode loop performs no allocation per timestep. For a 30-second utterance
// that is a few thousand iterations, and allocating there would dominate the
// cost of the inference itself.
type decoderWorker struct {
	session   *ort.AdvancedSession
	encOut    *ort.Tensor[float32]
	targets   *ort.Tensor[int32]
	targetLen *ort.Tensor[int32]
	state1In  *ort.Tensor[float32]
	state2In  *ort.Tensor[float32]
	output    *ort.Tensor[float32]
	state1Out *ort.Tensor[float32]
	state2Out *ort.Tensor[float32]
}

func newDecoderWorker(path string, vocabSize int, opts *ort.SessionOptions) (w *decoderWorker, err error) {
	w = &decoderWorker{}
	// Any failure part-way through leaves already-created tensors leaked
	// unless they are released here; ORT has no finaliser to fall back on.
	defer func() {
		if err != nil {
			w.destroy()
			w = nil
		}
	}()

	mk := func(shape ...int64) (*ort.Tensor[float32], error) {
		return ort.NewEmptyTensor[float32](ort.NewShape(shape...))
	}

	if w.encOut, err = mk(1, encoderDim, 1); err != nil {
		return nil, fmt.Errorf("encoder-output tensor: %w", err)
	}
	if w.targets, err = ort.NewEmptyTensor[int32](ort.NewShape(1, 1)); err != nil {
		return nil, fmt.Errorf("targets tensor: %w", err)
	}
	if w.targetLen, err = ort.NewTensor(ort.NewShape(1), []int32{1}); err != nil {
		return nil, fmt.Errorf("target-length tensor: %w", err)
	}
	if w.state1In, err = mk(decoderLayers, 1, decoderStateDim); err != nil {
		return nil, fmt.Errorf("state1 input tensor: %w", err)
	}
	if w.state2In, err = mk(decoderLayers, 1, decoderStateDim); err != nil {
		return nil, fmt.Errorf("state2 input tensor: %w", err)
	}
	if w.output, err = mk(1, 1, 1, int64(vocabSize)+durationClasses); err != nil {
		return nil, fmt.Errorf("joint output tensor: %w", err)
	}
	if w.state1Out, err = mk(decoderLayers, 1, decoderStateDim); err != nil {
		return nil, fmt.Errorf("state1 output tensor: %w", err)
	}
	if w.state2Out, err = mk(decoderLayers, 1, decoderStateDim); err != nil {
		return nil, fmt.Errorf("state2 output tensor: %w", err)
	}

	w.session, err = ort.NewAdvancedSession(
		path,
		[]string{"encoder_outputs", "targets", "target_length", "input_states_1", "input_states_2"},
		[]string{"outputs", "output_states_1", "output_states_2"},
		[]ort.ArbitraryTensor{w.encOut, w.targets, w.targetLen, w.state1In, w.state2In},
		[]ort.ArbitraryTensor{w.output, w.state1Out, w.state2Out},
		opts,
	)
	if err != nil {
		return nil, fmt.Errorf("create decoder session: %w", err)
	}
	return w, nil
}

func (w *decoderWorker) destroy() {
	if w == nil {
		return
	}
	if w.session != nil {
		w.session.Destroy()
		w.session = nil
	}
	// Each tensor is checked as a concrete pointer. Collecting them into a
	// []interface{ Destroy() error } would box typed nils into non-nil
	// interface values and call Destroy on a nil tensor.
	destroyF32 := func(t **ort.Tensor[float32]) {
		if *t != nil {
			_ = (*t).Destroy()
			*t = nil
		}
	}
	destroyF32(&w.encOut)
	destroyF32(&w.state1In)
	destroyF32(&w.state2In)
	destroyF32(&w.output)
	destroyF32(&w.state1Out)
	destroyF32(&w.state2Out)
	if w.targets != nil {
		_ = w.targets.Destroy()
		w.targets = nil
	}
	if w.targetLen != nil {
		_ = w.targetLen.Destroy()
		w.targetLen = nil
	}
}

// resetState zeroes the recurrent state so each utterance starts clean. Leaking
// state between requests would let one caller's audio bias another's.
func (w *decoderWorker) resetState() {
	for _, s := range [][]float32{w.state1In.GetData(), w.state2In.GetData()} {
		for i := range s {
			s[i] = 0
		}
	}
}

// pool hands out decoder workers and guarantees every one is destroyed exactly
// once, after all in-flight work has finished.
//
// The reference implementation this project studied closes its worker channel
// and then ranges over it. Any worker checked out at that moment is never
// destroyed, and the deferred send that returns it panics on the closed
// channel. Tracking in-flight acquisitions explicitly removes both problems.
type pool struct {
	free     chan *decoderWorker
	all      []*decoderWorker
	inflight sync.WaitGroup

	mu     sync.RWMutex
	closed bool
}

func newPool(workers []*decoderWorker) *pool {
	p := &pool{free: make(chan *decoderWorker, len(workers)), all: workers}
	for _, w := range workers {
		p.free <- w
	}
	return p
}

// acquire takes a worker, blocking until one is free, the context is done, or
// the pool closes.
func (p *pool) acquire(ctx context.Context) (*decoderWorker, error) {
	p.mu.RLock()
	if p.closed {
		p.mu.RUnlock()
		return nil, errPoolClosed
	}
	p.inflight.Add(1)
	p.mu.RUnlock()

	select {
	case w := <-p.free:
		if w == nil { // channel drained during shutdown
			p.inflight.Done()
			return nil, errPoolClosed
		}
		return w, nil
	case <-ctx.Done():
		p.inflight.Done()
		return nil, ctx.Err()
	}
}

// tryAcquire takes a worker only if one is free right now. Used for partial
// decodes, which must never wait.
func (p *pool) tryAcquire() (*decoderWorker, bool) {
	p.mu.RLock()
	if p.closed {
		p.mu.RUnlock()
		return nil, false
	}
	p.inflight.Add(1)
	p.mu.RUnlock()

	select {
	case w := <-p.free:
		if w == nil {
			p.inflight.Done()
			return nil, false
		}
		return w, true
	default:
		p.inflight.Done()
		return nil, false
	}
}

// release returns a worker to the pool.
func (p *pool) release(w *decoderWorker) {
	p.free <- w
	p.inflight.Done()
}

// idle reports how many workers are currently free, for the queue-depth metric.
func (p *pool) idle() int { return len(p.free) }

// size reports the total worker count.
func (p *pool) size() int { return len(p.all) }

// close stops new acquisitions, waits for outstanding work to finish, then
// destroys every worker exactly once.
func (p *pool) close() {
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return
	}
	p.closed = true
	p.mu.Unlock()

	p.inflight.Wait() // every worker is now back in p.free

	for _, w := range p.all {
		w.destroy()
	}
	p.all = nil
	close(p.free)
}
