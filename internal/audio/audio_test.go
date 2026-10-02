package audio

import (
	"math"
	"math/cmplx"
	"testing"
)

// TestFFTMatchesNaiveDFT is the load-bearing test of this package. The mel
// features feed the acoustic model directly: a sign error or an off-by-one in
// the twiddle indexing produces a plausible-looking spectrum and a quietly
// wrong transcript, with nothing downstream to catch it. Comparing against a
// naive O(n^2) DFT makes the optimised path provably equivalent to the
// definition.
func TestFFTMatchesNaiveDFT(t *testing.T) {
	fb := NewFilterbank(NumMels, SampleRate)

	signals := map[string][]float64{
		"impulse": func() []float64 { s := make([]float64, NFFT); s[0] = 1; return s }(),
		"dc": func() []float64 {
			s := make([]float64, NFFT)
			for i := range s {
				s[i] = 1
			}
			return s
		}(),
		"sine440": func() []float64 {
			s := make([]float64, NFFT)
			for i := range s {
				s[i] = math.Sin(2 * math.Pi * 440 * float64(i) / SampleRate)
			}
			return s
		}(),
		"mixed": func() []float64 {
			s := make([]float64, NFFT)
			for i := range s {
				s[i] = 0.3*math.Sin(2*math.Pi*100*float64(i)/SampleRate) +
					0.7*math.Cos(2*math.Pi*3000*float64(i)/SampleRate)
			}
			return s
		}(),
	}

	for name, sig := range signals {
		t.Run(name, func(t *testing.T) {
			fast := make([]complex128, NFFT)
			for i, v := range sig {
				fast[i] = complex(v, 0)
			}
			fb.fft(fast)

			for k := 0; k <= NFFT/2; k++ {
				var want complex128
				for n, v := range sig {
					a := -2 * math.Pi * float64(k) * float64(n) / float64(NFFT)
					want += complex(v, 0) * cmplx.Exp(complex(0, a))
				}
				if d := cmplx.Abs(fast[k] - want); d > 1e-6 {
					t.Fatalf("bin %d: fft=%v dft=%v (|diff|=%g)", k, fast[k], want, d)
				}
			}
		})
	}
}

func TestFilterbankFrameGeometry(t *testing.T) {
	fb := NewFilterbank(NumMels, SampleRate)

	if got := fb.FramesPerSecond(); got != 100 {
		t.Fatalf("FramesPerSecond = %d, want 100 (16000/160)", got)
	}
	for _, tc := range []struct{ samples, want int }{
		{0, 0},
		{WinLength - 1, 0},
		{WinLength, 1},
		{WinLength + HopLength, 2},
		{SampleRate, (SampleRate-WinLength)/HopLength + 1},
	} {
		if got := fb.NumFrames(tc.samples); got != tc.want {
			t.Errorf("NumFrames(%d) = %d, want %d", tc.samples, got, tc.want)
		}
	}
}

// A pure tone must concentrate energy in the mel bins covering its frequency,
// not spread it evenly. This catches a filterbank built with wrong edges.
func TestMelEnergyConcentratesAtToneFrequency(t *testing.T) {
	fb := NewFilterbank(NumMels, SampleRate)
	ex := fb.NewExtractor()

	tone := make([]float32, WinLength)
	for i := range tone {
		tone[i] = float32(math.Sin(2 * math.Pi * 1000 * float64(i) / SampleRate))
	}
	out := make([]float32, NumMels)
	ex.Frame(tone, out)

	peak := 0
	for i, v := range out {
		if v > out[peak] {
			peak = i
		}
	}
	// 1 kHz on the HTK mel scale with 128 bins over 0-8 kHz lands well inside
	// the lower half of the bank.
	if peak < 15 || peak > 50 {
		t.Fatalf("1 kHz tone peaks at mel bin %d, expected roughly 15-50", peak)
	}
	mean := float32(0)
	for _, v := range out {
		mean += v
	}
	mean /= float32(len(out))
	if out[peak] <= mean {
		t.Fatal("peak bin is not above the mean: filterbank is not selective")
	}
}

func TestExtractorIsDeterministicAndAllocationFree(t *testing.T) {
	fb := NewFilterbank(NumMels, SampleRate)
	ex := fb.NewExtractor()

	sig := make([]float32, WinLength*4)
	for i := range sig {
		sig[i] = float32(math.Sin(float64(i) * 0.01))
	}
	a := ex.Extract(sig)
	b := ex.Extract(sig)
	if len(a) != len(b) {
		t.Fatalf("frame count differs between runs: %d vs %d", len(a), len(b))
	}
	for i := range a {
		for j := range a[i] {
			if a[i][j] != b[i][j] {
				t.Fatalf("frame %d bin %d differs: %v vs %v", i, j, a[i][j], b[i][j])
			}
		}
	}

	out := make([]float32, NumMels)
	allocs := testing.AllocsPerRun(50, func() { ex.Frame(sig[:WinLength], out) })
	if allocs != 0 {
		t.Fatalf("Frame allocates %.0f times per call; scratch reuse is broken", allocs)
	}
}

func TestNormalizeUtteranceProducesZeroMeanUnitVariance(t *testing.T) {
	frames := [][]float32{
		{1, 10}, {2, 20}, {3, 30}, {4, 40},
	}
	NormalizeUtterance(frames)
	for m := 0; m < 2; m++ {
		var sum, sq float64
		for _, f := range frames {
			sum += float64(f[m])
		}
		mean := sum / float64(len(frames))
		for _, f := range frames {
			sq += (float64(f[m]) - mean) * (float64(f[m]) - mean)
		}
		std := math.Sqrt(sq / float64(len(frames)))
		if math.Abs(mean) > 1e-5 {
			t.Errorf("bin %d mean = %g, want 0", m, mean)
		}
		if math.Abs(std-1) > 1e-5 {
			t.Errorf("bin %d std = %g, want 1", m, std)
		}
	}
}

func TestNormalizeUtteranceHandlesConstantBin(t *testing.T) {
	frames := [][]float32{{5}, {5}, {5}}
	NormalizeUtterance(frames) // must not divide by zero or produce NaN
	for i, f := range frames {
		if math.IsNaN(float64(f[0])) || math.IsInf(float64(f[0]), 0) {
			t.Fatalf("frame %d became %v on a constant bin", i, f[0])
		}
	}
}

func TestStreamNormalizerConverges(t *testing.T) {
	s := NewStreamNormalizer(1, 50, 0.01)
	for i := 0; i < 500; i++ {
		v := []float32{float32(10 + math.Sin(float64(i)))}
		s.Observe(v)
	}
	if !s.Ready() {
		t.Fatal("normalizer not ready after 500 frames")
	}
	if math.Abs(s.mean[0]-10) > 0.5 {
		t.Fatalf("running mean = %g, want ~10", s.mean[0])
	}
	f := []float32{10}
	s.Apply(f)
	if math.IsNaN(float64(f[0])) {
		t.Fatal("Apply produced NaN")
	}
}

func TestWAVRoundTrip(t *testing.T) {
	orig := make([]float32, SampleRate/2)
	for i := range orig {
		orig[i] = float32(0.5 * math.Sin(2*math.Pi*440*float64(i)/SampleRate))
	}
	got, err := DecodeWAV(EncodeWAV(orig, SampleRate))
	if err != nil {
		t.Fatalf("DecodeWAV: %v", err)
	}
	if len(got) != len(orig) {
		t.Fatalf("length %d, want %d", len(got), len(orig))
	}
	for i := range got {
		if math.Abs(float64(got[i]-orig[i])) > 1e-4 { // 16-bit quantisation floor
			t.Fatalf("sample %d: got %v want %v", i, got[i], orig[i])
		}
	}
}

func TestDecodeWAVRejectsGarbage(t *testing.T) {
	for name, in := range map[string][]byte{
		"empty":     {},
		"short":     []byte("RIFF"),
		"not riff":  make([]byte, 100),
		"riff only": append([]byte("RIFF0000WAVE"), make([]byte, 40)...),
	} {
		if _, err := DecodeWAV(in); err == nil {
			t.Errorf("%s: expected an error, got nil", name)
		}
	}
}

// Downsampling without a low-pass filter mirrors high-frequency content into
// the speech band. A 7 kHz tone resampled 44.1k -> 16k must not appear as a
// loud low-frequency component.
func TestResampleAttenuatesAliasing(t *testing.T) {
	const srcRate = 44100
	in := make([]float32, srcRate/2)
	for i := range in {
		in[i] = float32(math.Sin(2 * math.Pi * 15000 * float64(i) / srcRate))
	}
	out := Resample(in, srcRate, SampleRate)

	if len(out) == 0 {
		t.Fatal("resample produced nothing")
	}
	var energy float64
	for _, v := range out {
		energy += float64(v) * float64(v)
	}
	rms := math.Sqrt(energy / float64(len(out)))
	// 15 kHz is far above the 8 kHz Nyquist of the target rate: a correct
	// filter removes it almost entirely. Unfiltered linear interpolation
	// leaves an alias at roughly the input amplitude (rms ~0.7).
	if rms > 0.15 {
		t.Fatalf("aliased energy rms = %.3f, want < 0.15 (no anti-alias filter?)", rms)
	}
}

func TestResampleIdentityAndUpsample(t *testing.T) {
	in := []float32{0, 0.5, 1, 0.5, 0}
	if got := Resample(in, SampleRate, SampleRate); len(got) != len(in) {
		t.Fatalf("identity resample changed length: %d", len(got))
	}
	up := Resample(in, 8000, 16000)
	if len(up) != 10 {
		t.Fatalf("upsample 2x gave %d samples, want 10", len(up))
	}
}

func TestPCM16ToFloat32(t *testing.T) {
	// -32768, 0, 32767 little-endian
	b := []byte{0x00, 0x80, 0x00, 0x00, 0xff, 0x7f}
	got := PCM16ToFloat32(b, nil)
	want := []float32{-1, 0, 32767.0 / 32768.0}
	for i := range want {
		if math.Abs(float64(got[i]-want[i])) > 1e-6 {
			t.Errorf("sample %d = %v, want %v", i, got[i], want[i])
		}
	}
}

func TestRingAbsoluteIndexing(t *testing.T) {
	r := NewRing(10)
	r.Write([]float32{1, 2, 3, 4, 5})

	got, ok := r.Slice(1, 4, nil)
	if !ok {
		t.Fatal("Slice(1,4) reported unavailable")
	}
	if len(got) != 3 || got[0] != 2 || got[2] != 4 {
		t.Fatalf("Slice(1,4) = %v, want [2 3 4]", got)
	}

	// Overflow the ring: early samples must be reported as gone, not returned
	// as whatever happens to be in those slots now.
	r.Write([]float32{6, 7, 8, 9, 10, 11, 12})
	if r.Len() != 12 {
		t.Fatalf("Len = %d, want 12", r.Len())
	}
	if r.Oldest() != 2 {
		t.Fatalf("Oldest = %d, want 2", r.Oldest())
	}
	if _, ok := r.Slice(0, 3, nil); ok {
		t.Fatal("Slice returned overwritten samples instead of reporting the gap")
	}
	got, ok = r.Slice(9, 12, nil)
	if !ok || got[0] != 10 || got[2] != 12 {
		t.Fatalf("Slice(9,12) = %v ok=%v, want [10 11 12] true", got, ok)
	}
}

func TestRingWriteLargerThanCapacity(t *testing.T) {
	r := NewRing(4)
	r.Write([]float32{1, 2, 3, 4, 5, 6, 7, 8, 9})
	got, ok := r.Slice(5, 9, nil)
	if !ok {
		t.Fatal("tail unavailable after oversized write")
	}
	want := []float32{6, 7, 8, 9}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("got %v, want %v", got, want)
		}
	}
}

func BenchmarkMelFrame(b *testing.B) {
	fb := NewFilterbank(NumMels, SampleRate)
	ex := fb.NewExtractor()
	sig := make([]float32, WinLength)
	for i := range sig {
		sig[i] = float32(math.Sin(float64(i) * 0.1))
	}
	out := make([]float32, NumMels)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		ex.Frame(sig, out)
	}
}

func BenchmarkExtractOneSecond(b *testing.B) {
	fb := NewFilterbank(NumMels, SampleRate)
	ex := fb.NewExtractor()
	sig := make([]float32, SampleRate)
	for i := range sig {
		sig[i] = float32(math.Sin(float64(i) * 0.05))
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		ex.Extract(sig)
	}
}
