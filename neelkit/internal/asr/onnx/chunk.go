// Derived from achetronic/parakeet (Apache-2.0), Copyright 2026 Alby Hernández:
// the overlapping-window planner and seam token deduplication.
// Reimplemented here with the changes described in ATTRIBUTION.md.
// SPDX-License-Identifier: Apache-2.0

package onnx

// Long-audio windowing.
//
// The exported encoder graph has a hard limit on sequence length: its
// positional-encoding table is centred at 5000 encoder frames, and beyond that
// the relative-position slice goes out of range and self-attention fails on a
// broadcast mismatch. At 8x subsampling over a 10 ms hop that is about 400
// seconds of audio in one pass.
//
// Anything longer is covered by overlapping windows. The overlap exists so the
// encoder has acoustic context and the decoder's recurrent state is warm before
// the region a window is responsible for begins.

const (
	// maxEncoderFrames is the model's hard sequence limit.
	maxEncoderFrames int64 = 5000
	// safetyMarginFrames keeps windows clear of that limit so rounding at the
	// subsampling boundary can never reach it.
	safetyMarginFrames int64 = 200

	// seamTokens is how many leading tokens of a window are held back and
	// compared against the previous window's tail.
	seamTokens = 3
	// seamToleranceFrames is how close two tokens must be, in encoder frames
	// of 80 ms, to count as the same acoustic position. Three frames is 240 ms:
	// wide enough to absorb the timestamp jitter between two windows that both
	// saw the word, narrow enough not to swallow a genuine neighbour.
	seamToleranceFrames = 3
)

// window is one analysis window over the mel sequence, in absolute mel frames.
//
// The encoder runs over [start, end), but only tokens landing in
// [emitStart, emitEnd) are kept. Adjacent emit ranges tile the timeline exactly:
// emitEnd of one window equals emitStart of the next, the first starts at 0 and
// the last ends at the total. Every frame is therefore transcribed by exactly
// one window: no gaps, no duplicates.
type window struct {
	start     int64
	end       int64
	emitStart int64
	emitEnd   int64
}

// boundaryFn chooses where ownership of an overlap changes hands, given the
// overlap's absolute mel-frame bounds and its arithmetic midpoint. Returning
// false falls back to the midpoint.
type boundaryFn func(start, end, midpoint int64) (int64, bool)

// planWindows tiles total mel frames into windows of at most chunkFrames, each
// sharing overlapFrames with its neighbour.
//
// Whatever boundary is chosen, it is clamped so the tiling invariant holds. A
// boundary function is a heuristic and is allowed to be wrong; it is not
// allowed to produce overlapping or missing coverage.
func planWindows(total, chunkFrames, overlapFrames int64, boundary boundaryFn) []window {
	if total <= chunkFrames {
		return []window{{start: 0, end: total, emitStart: 0, emitEnd: total}}
	}

	stride := chunkFrames - overlapFrames
	var ws []window
	for start := int64(0); start < total; start += stride {
		end := start + chunkFrames
		if end >= total {
			end = total
		}
		ws = append(ws, window{start: start, end: end})
		if end == total {
			break
		}
	}

	prev := int64(0)
	for i := range ws {
		ws[i].emitStart = prev
		if i == len(ws)-1 {
			ws[i].emitEnd = total
			break
		}

		lo, hi := ws[i+1].start, ws[i].end
		mid := (lo + hi) / 2
		b := mid
		if boundary != nil {
			if got, ok := boundary(lo, hi, mid); ok {
				b = got
			}
		}
		if b < lo {
			b = lo
		}
		if b > hi {
			b = hi
		}
		if b < ws[i].emitStart {
			b = ws[i].emitStart
		}
		ws[i].emitEnd = b
		prev = b
	}
	return ws
}

// quietestFrame returns a boundary function that splits each overlap at the
// quietest point of the already-extracted mel features.
//
// A blind midpoint frequently lands inside a word, and the two windows then
// timestamp that word differently, so it is emitted twice or by neither. Moving
// the split into a pause removes the ambiguity at its source. The energy comes
// from features that were computed anyway, so this costs one pass over the
// overlap and no extra model inference.
func quietestFrame(features [][]float32) boundaryFn {
	if len(features) == 0 {
		return nil
	}
	energy := make([]float64, len(features))
	for i, f := range features {
		var s float64
		for _, v := range f {
			s += float64(v)
		}
		energy[i] = s
	}
	// Smooth over ~150 ms so a single quiet frame inside a word (a glottal
	// closure, a stop consonant) is not mistaken for a pause between words.
	const smooth = 15
	prefix := make([]float64, len(energy)+1)
	for i, v := range energy {
		prefix[i+1] = prefix[i] + v
	}
	sm := make([]float64, len(energy))
	for i := range energy {
		lo, hi := i-smooth/2, i+smooth/2+1
		if lo < 0 {
			lo = 0
		}
		if hi > len(energy) {
			hi = len(energy)
		}
		sm[i] = (prefix[hi] - prefix[lo]) / float64(hi-lo)
	}

	return func(start, end, _ int64) (int64, bool) {
		if end <= start {
			return 0, false
		}
		lo, hi := int(start), int(end)
		if lo < 0 {
			lo = 0
		}
		if hi > len(sm) {
			hi = len(sm)
		}
		if lo >= hi {
			return 0, false
		}
		best := lo
		for i := lo + 1; i < hi; i++ {
			if sm[i] < sm[best] {
				best = i
			}
		}
		return int64(best), true
	}
}

// melToEncoderFrame maps a mel-frame offset to its encoder-frame index. The
// encoder collapses `subsampling` mel frames into one.
func melToEncoderFrame(mel, subsampling int64) int64 {
	if subsampling <= 0 {
		return mel
	}
	return mel / subsampling
}

// dedupSeam drops leading tokens of a window that repeat the previous window's
// tail.
//
// Even with the boundary on silence, a word near the seam can be timestamped a
// frame or two apart by the two windows that both saw it. One rule covers both
// failure modes: identical text at the same position is a duplicate, and
// different text at the same position is a disagreement that the earlier window
// wins, because its recurrent state reached the seam fully warmed up while the
// later window is still warming.
func dedupSeam(prevTail, head []token) []token {
	if len(prevTail) == 0 || len(head) == 0 {
		return head
	}
	tail := prevTail
	if len(tail) > seamTokens {
		tail = tail[len(tail)-seamTokens:]
	}
	out := make([]token, 0, len(head))
	for _, h := range head {
		dup := false
		for _, p := range tail {
			d := h.timestep - p.timestep
			if d < 0 {
				d = -d
			}
			if d <= seamToleranceFrames {
				dup = true
				break
			}
		}
		if !dup {
			out = append(out, h)
		}
	}
	return out
}
