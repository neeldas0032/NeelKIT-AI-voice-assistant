// Package metrics implements the Prometheus text exposition format with no
// dependencies.
//
// The official client is an excellent library, and for fifteen instruments it
// brings six modules (protobuf, procfs, xxhash, perks, common, client_model)
// into the build. This package implements exactly the three instrument types
// NeelKIT uses and the text format Prometheus scrapes, in a few hundred lines
// that can be read in one sitting. If the project ever needs exemplars, native
// histograms or the pushgateway, swapping the library back in is a contained
// change behind this API.
package metrics

import (
	"fmt"
	"io"
	"math"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
)

// Registry collects instruments and renders them.
type Registry struct {
	mu   sync.RWMutex
	cols []collector
}

type collector interface {
	write(w io.Writer)
}

// NewRegistry returns an empty registry.
func NewRegistry() *Registry { return &Registry{} }

func (r *Registry) add(c collector) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.cols = append(r.cols, c)
}

// WritePrometheus renders every instrument in the text exposition format.
//
// Deliberately not named WriteTo: that name belongs to io.WriterTo, whose
// contract returns the byte count and an error, and go vet flags the mismatch.
// A method that looks like a standard interface but is not one is a trap for
// the next reader.
func (r *Registry) WritePrometheus(w io.Writer) {
	r.mu.RLock()
	cols := make([]collector, len(r.cols))
	copy(cols, r.cols)
	r.mu.RUnlock()
	for _, c := range cols {
		c.write(w)
	}
}

// ContentType is the exposition format's media type.
const ContentType = "text/plain; version=0.0.4; charset=utf-8"

// --------------------------------------------------------------- helpers

// labelKey renders a label set into a stable series key. Sorting the names is
// what makes the key stable: map iteration order is random in Go, so an
// unsorted key would create a new series on every call.
func labelKey(names, values []string) string {
	if len(names) == 0 {
		return ""
	}
	type pair struct{ n, v string }
	ps := make([]pair, len(names))
	for i := range names {
		v := ""
		if i < len(values) {
			v = values[i]
		}
		ps[i] = pair{names[i], v}
	}
	sort.Slice(ps, func(i, j int) bool { return ps[i].n < ps[j].n })

	var b strings.Builder
	b.WriteByte('{')
	for i, p := range ps {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(p.n)
		b.WriteString(`="`)
		b.WriteString(escapeLabel(p.v))
		b.WriteByte('"')
	}
	b.WriteByte('}')
	return b.String()
}

// escapeLabel escapes the three characters the exposition format reserves.
// A tool name or error string carrying a quote would otherwise produce a
// malformed scrape that breaks every other metric in the response.
func escapeLabel(s string) string {
	r := strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`)
	return r.Replace(s)
}

func header(w io.Writer, name, help, typ string) {
	fmt.Fprintf(w, "# HELP %s %s\n# TYPE %s %s\n", name, help, name, typ)
}

func formatFloat(f float64) string {
	if math.IsInf(f, 1) {
		return "+Inf"
	}
	return strconv.FormatFloat(f, 'g', -1, 64)
}

// ------------------------------------------------------------- counters

// Counter is a monotonically increasing value.
type Counter struct {
	name, help string
	bits       atomic.Uint64 // float64 bits, so Add stays lock-free
}

// NewCounter registers a counter.
func (r *Registry) NewCounter(name, help string) *Counter {
	c := &Counter{name: name, help: help}
	r.add(c)
	return c
}

// Inc adds one.
func (c *Counter) Inc() { c.Add(1) }

// Add adds a non-negative delta.
func (c *Counter) Add(v float64) {
	if v < 0 {
		return // a counter that can go down is not a counter
	}
	for {
		old := c.bits.Load()
		next := math.Float64bits(math.Float64frombits(old) + v)
		if c.bits.CompareAndSwap(old, next) {
			return
		}
	}
}

// Value returns the current total.
func (c *Counter) Value() float64 { return math.Float64frombits(c.bits.Load()) }

func (c *Counter) write(w io.Writer) {
	header(w, c.name, c.help, "counter")
	fmt.Fprintf(w, "%s %s\n", c.name, formatFloat(c.Value()))
}

// CounterVec is a counter partitioned by labels.
type CounterVec struct {
	name, help string
	names      []string

	mu     sync.RWMutex
	series map[string]*Counter
}

// NewCounterVec registers a labelled counter.
func (r *Registry) NewCounterVec(name, help string, labels []string) *CounterVec {
	c := &CounterVec{name: name, help: help, names: labels, series: map[string]*Counter{}}
	r.add(c)
	return c
}

// With returns the counter for a label set, creating it on first use.
func (c *CounterVec) With(values ...string) *Counter {
	key := labelKey(c.names, values)
	c.mu.RLock()
	s, ok := c.series[key]
	c.mu.RUnlock()
	if ok {
		return s
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if s, ok = c.series[key]; ok {
		return s
	}
	s = &Counter{name: c.name + key}
	c.series[key] = s
	return s
}

// Inc increments the series for a label set.
func (c *CounterVec) Inc(values ...string) { c.With(values...).Inc() }

func (c *CounterVec) write(w io.Writer) {
	header(w, c.name, c.help, "counter")
	c.mu.RLock()
	defer c.mu.RUnlock()
	for _, k := range sortedKeys(c.series) {
		fmt.Fprintf(w, "%s%s %s\n", c.name, k, formatFloat(c.series[k].Value()))
	}
}

// ---------------------------------------------------------------- gauges

// Gauge is a value that can go up or down.
type Gauge struct {
	name, help string
	bits       atomic.Uint64
}

// NewGauge registers a gauge.
func (r *Registry) NewGauge(name, help string) *Gauge {
	g := &Gauge{name: name, help: help}
	r.add(g)
	return g
}

// Set replaces the value.
func (g *Gauge) Set(v float64) { g.bits.Store(math.Float64bits(v)) }

// Add adds a delta, which may be negative.
func (g *Gauge) Add(v float64) {
	for {
		old := g.bits.Load()
		next := math.Float64bits(math.Float64frombits(old) + v)
		if g.bits.CompareAndSwap(old, next) {
			return
		}
	}
}

// Inc adds one; Dec subtracts one.
func (g *Gauge) Inc() { g.Add(1) }
func (g *Gauge) Dec() { g.Add(-1) }

// Value returns the current value.
func (g *Gauge) Value() float64 { return math.Float64frombits(g.bits.Load()) }

func (g *Gauge) write(w io.Writer) {
	header(w, g.name, g.help, "gauge")
	fmt.Fprintf(w, "%s %s\n", g.name, formatFloat(g.Value()))
}

// ------------------------------------------------------------ histograms

// Histogram counts observations into cumulative buckets.
type Histogram struct {
	name, help string
	bounds     []float64

	mu       sync.Mutex
	counts   []uint64
	sum      float64
	total    uint64
	labels   string
	noHeader bool
}

// NewHistogram registers a histogram. Bounds must be sorted ascending; the
// implicit +Inf bucket is added automatically.
func (r *Registry) NewHistogram(name, help string, bounds []float64) *Histogram {
	h := newHistogram(name, help, bounds, "")
	r.add(h)
	return h
}

func newHistogram(name, help string, bounds []float64, labels string) *Histogram {
	b := append([]float64(nil), bounds...)
	sort.Float64s(b)
	return &Histogram{name: name, help: help, bounds: b, counts: make([]uint64, len(b)+1), labels: labels}
}

// Observe records a value.
func (h *Histogram) Observe(v float64) {
	// A linear scan beats a binary search for the bucket counts used here
	// (fewer than a dozen bounds) and keeps the code obvious.
	i := 0
	for i < len(h.bounds) && v > h.bounds[i] {
		i++
	}
	h.mu.Lock()
	h.counts[i]++
	h.sum += v
	h.total++
	h.mu.Unlock()
}

// Count returns how many observations were recorded.
func (h *Histogram) Count() uint64 {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.total
}

func (h *Histogram) write(w io.Writer) {
	if !h.noHeader {
		header(w, h.name, h.help, "histogram")
	}
	h.writeSeries(w)
}

// writeSeries emits the cumulative buckets, sum and count.
func (h *Histogram) writeSeries(w io.Writer) {
	h.mu.Lock()
	counts := append([]uint64(nil), h.counts...)
	sum, total := h.sum, h.total
	h.mu.Unlock()

	inner := strings.TrimSuffix(strings.TrimPrefix(h.labels, "{"), "}")
	sep := ""
	if inner != "" {
		sep = ","
	}

	var cum uint64
	for i, b := range h.bounds {
		cum += counts[i]
		fmt.Fprintf(w, "%s_bucket{%s%sle=\"%s\"} %d\n", h.name, inner, sep, formatFloat(b), cum)
	}
	cum += counts[len(counts)-1]
	fmt.Fprintf(w, "%s_bucket{%s%sle=\"+Inf\"} %d\n", h.name, inner, sep, cum)
	fmt.Fprintf(w, "%s_sum%s %s\n", h.name, h.labels, formatFloat(sum))
	fmt.Fprintf(w, "%s_count%s %d\n", h.name, h.labels, total)
}

// HistogramVec is a histogram partitioned by labels.
type HistogramVec struct {
	name, help string
	names      []string
	bounds     []float64

	mu     sync.RWMutex
	series map[string]*Histogram
}

// NewHistogramVec registers a labelled histogram.
func (r *Registry) NewHistogramVec(name, help string, labels []string, bounds []float64) *HistogramVec {
	h := &HistogramVec{name: name, help: help, names: labels, bounds: bounds, series: map[string]*Histogram{}}
	r.add(h)
	return h
}

// With returns the histogram for a label set.
func (h *HistogramVec) With(values ...string) *Histogram {
	key := labelKey(h.names, values)
	h.mu.RLock()
	s, ok := h.series[key]
	h.mu.RUnlock()
	if ok {
		return s
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if s, ok = h.series[key]; ok {
		return s
	}
	s = newHistogram(h.name, h.help, h.bounds, key)
	s.noHeader = true
	h.series[key] = s
	return s
}

// Observe records a value for a label set.
func (h *HistogramVec) Observe(v float64, values ...string) { h.With(values...).Observe(v) }

func (h *HistogramVec) write(w io.Writer) {
	header(w, h.name, h.help, "histogram")
	h.mu.RLock()
	defer h.mu.RUnlock()
	for _, k := range sortedKeys(h.series) {
		h.series[k].writeSeries(w)
	}
}

func sortedKeys[T any](m map[string]T) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
