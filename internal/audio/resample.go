package audio

import "math"

// Resample converts samples from srcRate to dstRate.
//
// Downsampling is low-pass filtered before decimation. Bare linear
// interpolation (what the upstream reference does) folds every component above
// the new Nyquist frequency back into the audible band: resampling 44.1 kHz to
// 16 kHz without a filter mirrors 8-22 kHz content down into 0-8 kHz, which is
// exactly the band the acoustic model listens to. The artefacts are inaudible
// to a human skimming the waveform and measurably hurt word error rate.
func Resample(in []float32, srcRate, dstRate int) []float32 {
	if srcRate == dstRate || len(in) == 0 || srcRate <= 0 || dstRate <= 0 {
		return in
	}
	src := in
	if dstRate < srcRate {
		// Cut at 45% of the destination rate, comfortably below the new Nyquist
		// so the filter's transition band has somewhere to land.
		src = lowPass(in, float64(dstRate)*0.45/float64(srcRate))
	}

	ratio := float64(srcRate) / float64(dstRate)
	n := int(float64(len(src)) / ratio)
	out := make([]float32, n)
	for i := 0; i < n; i++ {
		pos := float64(i) * ratio
		lo := int(pos)
		hi := lo + 1
		if hi >= len(src) {
			hi = len(src) - 1
		}
		f := float32(pos - float64(lo))
		out[i] = src[lo]*(1-f) + src[hi]*f
	}
	return out
}

// lowPass applies a Hann-windowed sinc FIR with the given normalized cutoff
// (cycles per sample, 0 < cutoff < 0.5). The kernel is built once per call;
// resampling happens at most once per request, so caching it would optimise a
// path that is never hot.
func lowPass(in []float32, cutoff float64) []float32 {
	if cutoff <= 0 || cutoff >= 0.5 {
		return in
	}
	const halfTaps = 32 // 65-tap kernel: ~60 dB stopband, cheap enough at 16 kHz
	kernel := make([]float64, 2*halfTaps+1)
	var sum float64
	for i := -halfTaps; i <= halfTaps; i++ {
		var s float64
		if i == 0 {
			s = 2 * cutoff
		} else {
			x := math.Pi * float64(i)
			s = math.Sin(2*cutoff*x) / x
		}
		// Hann window suppresses the sidelobes the truncated sinc would leave.
		w := 0.5 * (1 - math.Cos(2*math.Pi*float64(i+halfTaps)/float64(2*halfTaps)))
		s *= w
		kernel[i+halfTaps] = s
		sum += s
	}
	for i := range kernel { // unity DC gain: the filter must not change loudness
		kernel[i] /= sum
	}

	out := make([]float32, len(in))
	for i := range in {
		var acc float64
		for k := -halfTaps; k <= halfTaps; k++ {
			j := i + k
			// Clamp at the edges (replicate) rather than zero-pad, which would
			// inject a step discontinuity and ring.
			if j < 0 {
				j = 0
			} else if j >= len(in) {
				j = len(in) - 1
			}
			acc += float64(in[j]) * kernel[k+halfTaps]
		}
		out[i] = float32(acc)
	}
	return out
}
