package metrics

import (
	"strings"
	"sync"
	"testing"
)

func render(r *Registry) string {
	var b strings.Builder
	r.WritePrometheus(&b)
	return b.String()
}

func TestCounterOutput(t *testing.T) {
	r := NewRegistry()
	c := r.NewCounter("neelkit_things_total", "Things counted.")
	c.Inc()
	c.Add(4)

	out := render(r)
	for _, want := range []string{
		"# HELP neelkit_things_total Things counted.",
		"# TYPE neelkit_things_total counter",
		"neelkit_things_total 5",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q:\n%s", want, out)
		}
	}
}

// A counter that can decrease is not a counter, and a negative delta would
// make rate() produce nonsense.
func TestCounterRejectsNegative(t *testing.T) {
	r := NewRegistry()
	c := r.NewCounter("x_total", "x")
	c.Add(5)
	c.Add(-3)
	if c.Value() != 5 {
		t.Fatalf("value = %v after a negative Add, want 5", c.Value())
	}
}

// Map iteration order is random in Go. If the label key were built unsorted,
// the same label set would produce a different series on each call and the
// metric would fragment into thousands of near-duplicate lines.
func TestLabelOrderIsStable(t *testing.T) {
	r := NewRegistry()
	c := r.NewCounterVec("x_total", "x", []string{"b", "a", "c"})
	for i := 0; i < 100; i++ {
		c.Inc("2", "1", "3")
	}
	out := render(r)
	if n := strings.Count(out, "x_total{"); n != 1 {
		t.Fatalf("%d series for one label set, want 1:\n%s", n, out)
	}
	if !strings.Contains(out, `x_total{a="1",b="2",c="3"} 100`) {
		t.Fatalf("labels not sorted or value wrong:\n%s", out)
	}
}

// A tool name or error message containing a quote would otherwise produce a
// malformed scrape and break every other metric in the response.
func TestLabelValuesAreEscaped(t *testing.T) {
	r := NewRegistry()
	c := r.NewCounterVec("x_total", "x", []string{"msg"})
	c.Inc(`he said "hi"` + "\n" + `and \ left`)

	out := render(r)
	if strings.Count(out, "\n") != strings.Count(out, "\n") {
		t.Fatal("unreachable")
	}
	if !strings.Contains(out, `\"hi\"`) || !strings.Contains(out, `\\`) || !strings.Contains(out, `\n`) {
		t.Fatalf("label value not escaped:\n%s", out)
	}
	// The escaped value must not introduce a real newline mid-series.
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		if strings.HasPrefix(line, "x_total{") && !strings.HasSuffix(line, " 1") {
			t.Fatalf("series line is malformed: %q", line)
		}
	}
}

func TestGauge(t *testing.T) {
	r := NewRegistry()
	g := r.NewGauge("x_active", "x")
	g.Inc()
	g.Inc()
	g.Dec()
	g.Set(7)
	g.Add(-2)
	if g.Value() != 5 {
		t.Fatalf("value = %v, want 5", g.Value())
	}
	if !strings.Contains(render(r), "x_active 5") {
		t.Fatalf("output:\n%s", render(r))
	}
}

// Buckets must be cumulative: an observation in a low bucket also counts in
// every higher one, and le="+Inf" must equal the total.
func TestHistogramBucketsAreCumulative(t *testing.T) {
	r := NewRegistry()
	h := r.NewHistogram("x_seconds", "x", []float64{0.1, 0.5, 1})
	for _, v := range []float64{0.05, 0.2, 0.2, 0.7, 5} {
		h.Observe(v)
	}

	out := render(r)
	for _, want := range []string{
		`x_seconds_bucket{le="0.1"} 1`,
		`x_seconds_bucket{le="0.5"} 3`,
		`x_seconds_bucket{le="1"} 4`,
		`x_seconds_bucket{le="+Inf"} 5`,
		"x_seconds_count 5",
		"x_seconds_sum 6.15",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q:\n%s", want, out)
		}
	}
}

func TestHistogramVecEmitsOneHeaderPerMetric(t *testing.T) {
	r := NewRegistry()
	h := r.NewHistogramVec("x_seconds", "x", []string{"tool"}, []float64{0.1, 1})
	h.Observe(0.05, "fs.read")
	h.Observe(2, "git.status")

	out := render(r)
	// Prometheus rejects a scrape that repeats HELP/TYPE for one metric name.
	if n := strings.Count(out, "# TYPE x_seconds histogram"); n != 1 {
		t.Fatalf("%d TYPE lines for one metric, want 1:\n%s", n, out)
	}
	for _, want := range []string{
		`x_seconds_bucket{tool="fs.read",le="0.1"} 1`,
		`x_seconds_bucket{tool="git.status",le="+Inf"} 1`,
		`x_seconds_count{tool="fs.read"} 1`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q:\n%s", want, out)
		}
	}
}

func TestConcurrentUpdatesAreLossless(t *testing.T) {
	r := NewRegistry()
	c := r.NewCounter("x_total", "x")
	cv := r.NewCounterVec("y_total", "y", []string{"k"})
	h := r.NewHistogram("z_seconds", "z", []float64{1, 10})

	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 200; j++ {
				c.Inc()
				cv.Inc("a")
				h.Observe(0.5)
			}
		}()
	}
	wg.Wait()

	if c.Value() != 10000 {
		t.Errorf("counter = %v, want 10000 (lost updates)", c.Value())
	}
	if cv.With("a").Value() != 10000 {
		t.Errorf("counter vec = %v, want 10000", cv.With("a").Value())
	}
	if h.Count() != 10000 {
		t.Errorf("histogram count = %d, want 10000", h.Count())
	}
}

func TestOutputIsDeterministic(t *testing.T) {
	r := NewRegistry()
	c := r.NewCounterVec("x_total", "x", []string{"k"})
	for _, k := range []string{"z", "a", "m", "b"} {
		c.Inc(k)
	}
	first := render(r)
	for i := 0; i < 20; i++ {
		if got := render(r); got != first {
			t.Fatalf("render %d differs:\n%s\nvs\n%s", i, got, first)
		}
	}
}
