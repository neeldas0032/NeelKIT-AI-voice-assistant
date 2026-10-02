package audio

import "sync"

// Ring is a fixed-capacity circular buffer of float32 samples with a monotonic
// absolute index, used by the realtime path.
//
// The realtime pipeline needs two things a plain slice cannot give: a bound on
// memory for an arbitrarily long session, and a stable way to name a position
// in the stream after older samples have been overwritten. Ring keeps an
// absolute write counter, so a caller can ask for "samples 48000 to 64000 of
// this session" and get either the data or an explicit "that has been
// overwritten" answer, instead of silently reading the wrong audio.
type Ring struct {
	mu    sync.RWMutex
	buf   []float32
	write int64 // total samples ever written; buf index is write % len(buf)
}

// NewRing allocates a ring holding capacity samples.
func NewRing(capacity int) *Ring {
	return &Ring{buf: make([]float32, capacity)}
}

// Write appends samples, overwriting the oldest data when full.
func (r *Ring) Write(p []float32) {
	r.mu.Lock()
	defer r.mu.Unlock()
	n := len(r.buf)
	// A burst larger than the ring can only leave its own tail; copying the
	// whole thing would wrap repeatedly and write the same slots many times.
	// The absolute counter must still advance by the full burst, or every
	// index handed out afterwards would point at the wrong audio.
	if len(p) >= n {
		r.write += int64(len(p) - n)
		p = p[len(p)-n:]
	}
	for _, v := range p {
		r.buf[int(r.write%int64(n))] = v
		r.write++
	}
}

// Len returns the absolute number of samples written since the ring was created.
func (r *Ring) Len() int64 {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.write
}

// Oldest returns the lowest absolute index still resident in the ring.
func (r *Ring) Oldest() int64 {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if r.write < int64(len(r.buf)) {
		return 0
	}
	return r.write - int64(len(r.buf))
}

// Slice copies the absolute range [from, to) into dst, returning the copied
// samples. It reports false when any part of the range has already been
// overwritten, so a caller never silently receives the wrong audio.
func (r *Ring) Slice(from, to int64, dst []float32) ([]float32, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()

	n := int64(len(r.buf))
	oldest := int64(0)
	if r.write > n {
		oldest = r.write - n
	}
	if from < oldest || to > r.write || from > to {
		return nil, false
	}

	count := int(to - from)
	if cap(dst) < count {
		dst = make([]float32, count)
	}
	dst = dst[:count]
	for i := 0; i < count; i++ {
		dst[i] = r.buf[int((from+int64(i))%n)]
	}
	return dst, true
}

// Reset discards all content and rewinds the absolute counter.
func (r *Ring) Reset() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.write = 0
	for i := range r.buf {
		r.buf[i] = 0
	}
}
