// Derived from achetronic/parakeet (Apache-2.0), Copyright 2026 Alby Hernández:
// the NeMo-compatible mel filterbank parameters.
// Reimplemented here with the changes described in ATTRIBUTION.md.
// SPDX-License-Identifier: Apache-2.0

package audio

import "math"

// Feature-extraction constants. These are not tunable: they must match the
// preprocessing the Parakeet checkpoint was trained with, or the encoder sees
// out-of-distribution input and the transcript quietly degrades.
const (
	NumMels   = 128 // mel filterbank size
	NFFT      = 512 // FFT length
	WinLength = 400 // 25 ms analysis window at 16 kHz
	HopLength = 160 // 10 ms hop  -> 100 frames per second
)

// Filterbank holds the immutable, precomputed tables shared by every
// extraction: the mel weights, the analysis window, the FFT twiddle factors and
// the bit-reversal permutation.
//
// The reference implementation this project studied recomputes a complex
// exponential inside the innermost FFT butterfly loop. That is one
// transcendental per butterfly, for every one of the 100 frames per second of
// audio. Twiddle factors depend only on NFFT, so they are computed once here
// and indexed with a stride per stage.
//
// A Filterbank is read-only after construction and safe for concurrent use.
// Per-goroutine scratch lives in Extractor.
type Filterbank struct {
	nMels      int
	sampleRate int
	weights    [][]float64 // [nMels][nFFT/2+1]
	window     []float64   // Hann, WinLength taps
	twiddle    []complex128
	reverse    []int
}

// NewFilterbank builds the tables for the given mel count and sample rate.
func NewFilterbank(nMels, sampleRate int) *Filterbank {
	f := &Filterbank{nMels: nMels, sampleRate: sampleRate}

	f.window = make([]float64, WinLength)
	for i := range f.window {
		f.window[i] = 0.5 * (1 - math.Cos(2*math.Pi*float64(i)/float64(WinLength-1)))
	}

	f.weights = buildMelWeights(nMels, sampleRate, NFFT)

	// twiddle[k] = exp(-2*pi*i*k/NFFT). Stage `size` uses stride NFFT/size, so
	// one table of NFFT/2 entries serves every stage.
	f.twiddle = make([]complex128, NFFT/2)
	for k := range f.twiddle {
		a := -2 * math.Pi * float64(k) / float64(NFFT)
		f.twiddle[k] = complex(math.Cos(a), math.Sin(a))
	}

	bits := 0
	for 1<<bits < NFFT {
		bits++
	}
	f.reverse = make([]int, NFFT)
	for i := range f.reverse {
		r := 0
		for b := 0; b < bits; b++ {
			r = r<<1 | (i>>b)&1
		}
		f.reverse[i] = r
	}
	return f
}

// buildMelWeights creates triangular mel filters over the FFT bins using the
// HTK mel scale, matching the reference NeMo preprocessing.
func buildMelWeights(nMels, sampleRate, nFFT int) [][]float64 {
	hzToMel := func(hz float64) float64 { return 2595 * math.Log10(1+hz/700) }
	melToHz := func(m float64) float64 { return 700 * (math.Pow(10, m/2595) - 1) }

	numBins := nFFT/2 + 1
	melMin, melMax := hzToMel(0), hzToMel(float64(sampleRate)/2)

	edges := make([]int, nMels+2)
	for i := range edges {
		mel := melMin + float64(i)*(melMax-melMin)/float64(nMels+1)
		edges[i] = int(math.Floor(float64(nFFT+1) * melToHz(mel) / float64(sampleRate)))
	}

	w := make([][]float64, nMels)
	for i := 0; i < nMels; i++ {
		row := make([]float64, numBins)
		lo, mid, hi := edges[i], edges[i+1], edges[i+2]
		for j := lo; j < mid && j < numBins; j++ {
			if mid > lo {
				row[j] = float64(j-lo) / float64(mid-lo)
			}
		}
		for j := mid; j < hi && j < numBins; j++ {
			if hi > mid {
				row[j] = float64(hi-j) / float64(hi-mid)
			}
		}
		w[i] = row
	}
	return w
}

// FramesPerSecond is how many mel frames one second of audio produces.
func (f *Filterbank) FramesPerSecond() int { return f.sampleRate / HopLength }

// NumFrames reports how many complete frames n samples yield.
func (f *Filterbank) NumFrames(n int) int {
	if n < WinLength {
		return 0
	}
	return (n-WinLength)/HopLength + 1
}

// Extractor is per-goroutine scratch space bound to a Filterbank. It allocates
// its buffers once and reuses them for every frame, so steady-state extraction
// performs no allocation at all.
//
// An Extractor is NOT safe for concurrent use; create one per goroutine.
type Extractor struct {
	fb       *Filterbank
	windowed []complex128
	power    []float64
}

// NewExtractor returns scratch space for this filterbank.
func (f *Filterbank) NewExtractor() *Extractor {
	return &Extractor{
		fb:       f,
		windowed: make([]complex128, NFFT),
		power:    make([]float64, NFFT/2+1),
	}
}

// Frame computes one mel frame from exactly WinLength samples (short input is
// zero-padded) and writes NumMels values into out.
func (e *Extractor) Frame(samples []float32, out []float32) {
	f := e.fb

	for i := range e.windowed {
		e.windowed[i] = 0
	}
	n := len(samples)
	if n > WinLength {
		n = WinLength
	}
	for i := 0; i < n; i++ {
		e.windowed[i] = complex(float64(samples[i])*f.window[i], 0)
	}

	f.fft(e.windowed)

	for i := range e.power {
		re, im := real(e.windowed[i]), imag(e.windowed[i])
		e.power[i] = re*re + im*im
	}

	for m := 0; m < f.nMels; m++ {
		var energy float64
		row := f.weights[m]
		for j, w := range row {
			if w != 0 {
				energy += e.power[j] * w
			}
		}
		// Floor before the log: silence produces exactly zero energy, and
		// log(0) is -Inf, which propagates through normalisation and poisons
		// every other bin in the frame.
		if energy < 1e-10 {
			energy = 1e-10
		}
		out[m] = float32(math.Log(energy))
	}
}

// Extract computes every complete mel frame in samples.
func (e *Extractor) Extract(samples []float32) [][]float32 {
	n := e.fb.NumFrames(len(samples))
	if n <= 0 {
		return nil
	}
	// One backing array for all frames: n separate allocations would fragment
	// the heap and cost a GC scan per frame for a 10-minute file.
	flat := make([]float32, n*e.fb.nMels)
	frames := make([][]float32, n)
	for i := 0; i < n; i++ {
		frames[i] = flat[i*e.fb.nMels : (i+1)*e.fb.nMels]
		start := i * HopLength
		end := start + WinLength
		if end > len(samples) {
			end = len(samples)
		}
		e.Frame(samples[start:end], frames[i])
	}
	return frames
}

// fft performs an in-place iterative radix-2 Cooley-Tukey transform using the
// precomputed twiddle table and bit-reversal permutation.
func (f *Filterbank) fft(x []complex128) {
	n := len(x)
	for i := 0; i < n; i++ {
		if j := f.reverse[i]; i < j {
			x[i], x[j] = x[j], x[i]
		}
	}
	for size := 2; size <= n; size <<= 1 {
		half := size >> 1
		stride := n / size
		for base := 0; base < n; base += size {
			for j := 0; j < half; j++ {
				w := f.twiddle[j*stride]
				a := x[base+j]
				b := w * x[base+j+half]
				x[base+j] = a + b
				x[base+j+half] = a - b
			}
		}
	}
}

// NormalizeUtterance applies per-bin mean/variance normalisation across the
// whole utterance, matching NeMo's training-time preprocessing.
//
// This is the correct transform for batch transcription, where the complete
// utterance is available. It is impossible in the realtime path, which cannot
// see the future; see StreamNormalizer.
func NormalizeUtterance(frames [][]float32) {
	if len(frames) == 0 {
		return
	}
	nMels := len(frames[0])
	for m := 0; m < nMels; m++ {
		var sum float64
		for _, fr := range frames {
			sum += float64(fr[m])
		}
		mean := sum / float64(len(frames))

		var sq float64
		for _, fr := range frames {
			d := float64(fr[m]) - mean
			sq += d * d
		}
		std := math.Sqrt(sq / float64(len(frames)))
		if std < 1e-10 {
			std = 1e-10 // a constant bin would otherwise divide by zero
		}
		for _, fr := range frames {
			fr[m] = float32((float64(fr[m]) - mean) / std)
		}
	}
}

// StreamNormalizer maintains causal per-bin statistics for the realtime path.
//
// The batch normaliser needs the whole utterance before it can normalise the
// first frame, which is not available while the user is still speaking. This
// keeps an exponentially weighted running mean and variance instead: it is
// warmed from the first frames of the session and then adapts slowly, so a
// change in room noise is tracked without a single loud frame distorting the
// scale.
type StreamNormalizer struct {
	mean   []float64
	varian []float64
	alpha  float64
	warm   int
	warmUp int
}

// NewStreamNormalizer returns a causal normaliser. warmUpFrames is how many
// frames are used to seed the statistics before adaptation slows to alpha;
// 50 frames is 500 ms, enough to characterise the room without delaying speech.
func NewStreamNormalizer(nMels, warmUpFrames int, alpha float64) *StreamNormalizer {
	return &StreamNormalizer{
		mean:   make([]float64, nMels),
		varian: make([]float64, nMels),
		alpha:  alpha,
		warmUp: warmUpFrames,
	}
}

// Observe updates the running statistics with a frame, without modifying it.
func (s *StreamNormalizer) Observe(frame []float32) {
	s.warm++
	// During warm-up use the running average (1/n) so early estimates converge
	// quickly; afterwards switch to a fixed decay so the scale keeps adapting.
	a := s.alpha
	if s.warm <= s.warmUp {
		a = 1 / float64(s.warm)
	}
	for m, v := range frame {
		d := float64(v) - s.mean[m]
		s.mean[m] += a * d
		s.varian[m] = (1 - a) * (s.varian[m] + a*d*d)
	}
}

// Apply normalises a frame in place using the statistics observed so far.
func (s *StreamNormalizer) Apply(frame []float32) {
	for m := range frame {
		std := math.Sqrt(s.varian[m])
		if std < 1e-10 {
			std = 1e-10
		}
		frame[m] = float32((float64(frame[m]) - s.mean[m]) / std)
	}
}

// Ready reports whether enough frames have been seen for the statistics to be
// meaningful. Before this, normalisation is applied anyway (the alternative is
// no output at all) but the caller may choose to suppress partial transcripts.
func (s *StreamNormalizer) Ready() bool { return s.warm >= s.warmUp }
