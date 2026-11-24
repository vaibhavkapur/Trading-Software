// Package metrics is a small, dependency-free metrics registry that renders
// the Prometheus text exposition format, plus the set of metrics the OMS
// exposes.
package metrics

import (
	"fmt"
	"io"
	"math"
	"sort"
	"strconv"
	"strings"
	"sync"
)

// Registry holds metrics and renders them.
type Registry struct {
	mu      sync.Mutex
	metrics []renderer
}

type renderer interface {
	render(w io.Writer)
}

func (r *Registry) register(m renderer) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.metrics = append(r.metrics, m)
}

// Write renders every metric in registration order.
func (r *Registry) Write(w io.Writer) {
	r.mu.Lock()
	ms := append([]renderer(nil), r.metrics...)
	r.mu.Unlock()
	for _, m := range ms {
		m.render(w)
	}
}

type desc struct {
	name, help, kind string
	labels           []string
}

func (d desc) header(w io.Writer) {
	fmt.Fprintf(w, "# HELP %s %s\n# TYPE %s %s\n", d.name, d.help, d.name, d.kind)
}

func (d desc) labelString(values []string, extra ...string) string {
	if len(values) != len(d.labels) {
		panic(fmt.Sprintf("metrics: %s expects %d label values, got %d", d.name, len(d.labels), len(values)))
	}
	var parts []string
	for i, l := range d.labels {
		parts = append(parts, l+`="`+escape(values[i])+`"`)
	}
	for i := 0; i+1 < len(extra); i += 2 {
		parts = append(parts, extra[i]+`="`+escape(extra[i+1])+`"`)
	}
	if len(parts) == 0 {
		return ""
	}
	return "{" + strings.Join(parts, ",") + "}"
}

var escaper = strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`)

func escape(s string) string { return escaper.Replace(s) }

// key joins label values into a map key. \xff cannot appear in valid UTF-8.
func key(values []string) string { return strings.Join(values, "\xff") }

func unkey(k string, n int) []string {
	if n == 0 {
		return nil
	}
	return strings.Split(k, "\xff")
}

func formatFloat(v float64) string {
	if math.IsInf(v, 1) {
		return "+Inf"
	}
	return strconv.FormatFloat(v, 'g', -1, 64)
}

// Counter is a monotonically increasing value per label set.
type Counter struct {
	desc
	mu     sync.Mutex
	values map[string]float64
}

// Counter registers a counter.
func (r *Registry) Counter(name, help string, labels ...string) *Counter {
	c := &Counter{desc: desc{name, help, "counter", labels}, values: map[string]float64{}}
	if len(labels) == 0 {
		c.values[""] = 0 // an unlabelled counter is exported from the start
	}
	r.register(c)
	return c
}

// Inc adds one.
func (c *Counter) Inc(labelValues ...string) { c.Add(1, labelValues...) }

// Add adds v, which must not be negative.
func (c *Counter) Add(v float64, labelValues ...string) {
	if v < 0 {
		return
	}
	c.mu.Lock()
	c.values[key(labelValues)] += v
	c.mu.Unlock()
}

// Value returns the current value for a label set.
func (c *Counter) Value(labelValues ...string) float64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.values[key(labelValues)]
}

func (c *Counter) render(w io.Writer) {
	c.header(w)
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, k := range sortedKeys(c.values) {
		fmt.Fprintf(w, "%s%s %s\n", c.name, c.labelString(unkey(k, len(c.labels))), formatFloat(c.values[k]))
	}
}

// Sample is one value of a computed gauge.
type Sample struct {
	LabelValues []string
	Value       float64
}

// GaugeFunc is a gauge whose samples are computed at scrape time.
type GaugeFunc struct {
	desc
	fn func() []Sample
}

// GaugeFunc registers a gauge computed by fn each time metrics are rendered.
func (r *Registry) GaugeFunc(name, help string, labels []string, fn func() []Sample) {
	r.register(&GaugeFunc{desc: desc{name, help, "gauge", labels}, fn: fn})
}

func (g *GaugeFunc) render(w io.Writer) {
	g.header(w)
	for _, s := range g.fn() {
		fmt.Fprintf(w, "%s%s %s\n", g.name, g.labelString(s.LabelValues), formatFloat(s.Value))
	}
}

// Histogram counts observations into cumulative buckets per label set.
type Histogram struct {
	desc
	bounds []float64
	mu     sync.Mutex
	series map[string]*histSeries
}

type histSeries struct {
	counts []uint64 // one per bound, plus +Inf
	sum    float64
	count  uint64
}

// Histogram registers a histogram with the given upper bounds, in seconds or
// whatever unit the name says.
func (r *Registry) Histogram(name, help string, bounds []float64, labels ...string) *Histogram {
	b := append([]float64(nil), bounds...)
	sort.Float64s(b)
	h := &Histogram{desc: desc{name, help, "histogram", labels}, bounds: b, series: map[string]*histSeries{}}
	r.register(h)
	return h
}

// Observe records one value.
func (h *Histogram) Observe(v float64, labelValues ...string) {
	k := key(labelValues)
	h.mu.Lock()
	defer h.mu.Unlock()
	s := h.series[k]
	if s == nil {
		s = &histSeries{counts: make([]uint64, len(h.bounds)+1)}
		h.series[k] = s
	}
	i := sort.SearchFloat64s(h.bounds, v) // first bound >= v
	s.counts[i]++
	s.sum += v
	s.count++
}

// Count returns the number of observations for a label set.
func (h *Histogram) Count(labelValues ...string) uint64 {
	h.mu.Lock()
	defer h.mu.Unlock()
	if s := h.series[key(labelValues)]; s != nil {
		return s.count
	}
	return 0
}

func (h *Histogram) render(w io.Writer) {
	h.header(w)
	h.mu.Lock()
	defer h.mu.Unlock()
	keys := make([]string, 0, len(h.series))
	for k := range h.series {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		s := h.series[k]
		values := unkey(k, len(h.labels))
		var cum uint64
		for i, b := range h.bounds {
			cum += s.counts[i]
			fmt.Fprintf(w, "%s_bucket%s %d\n", h.name, h.labelString(values, "le", formatFloat(b)), cum)
		}
		cum += s.counts[len(h.bounds)]
		fmt.Fprintf(w, "%s_bucket%s %d\n", h.name, h.labelString(values, "le", "+Inf"), cum)
		fmt.Fprintf(w, "%s_sum%s %s\n", h.name, h.labelString(values), formatFloat(s.sum))
		fmt.Fprintf(w, "%s_count%s %d\n", h.name, h.labelString(values), s.count)
	}
}

func sortedKeys(m map[string]float64) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
