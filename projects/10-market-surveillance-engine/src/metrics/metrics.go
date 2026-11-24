// Package metrics renders Prometheus text exposition without dependencies.
package metrics

import (
	"fmt"
	"io"
	"sort"
	"strings"
	"sync"
)

// Histogram is a fixed-bucket latency histogram, safe for concurrent use.
type Histogram struct {
	mu     sync.Mutex
	bounds []float64
	counts []uint64
	sum    float64
	n      uint64
}

func NewHistogram(bounds ...float64) *Histogram {
	return &Histogram{bounds: bounds, counts: make([]uint64, len(bounds))}
}

func (h *Histogram) Observe(v float64) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for i, b := range h.bounds {
		if v <= b {
			h.counts[i]++
		}
	}
	h.sum += v
	h.n++
}

func (h *Histogram) Count() uint64 {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.n
}

func (h *Histogram) Write(w io.Writer, name, help string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	fmt.Fprintf(w, "# HELP %s %s\n# TYPE %s histogram\n", name, help, name)
	for i, b := range h.bounds {
		fmt.Fprintf(w, "%s_bucket{le=\"%g\"} %d\n", name, b, h.counts[i])
	}
	fmt.Fprintf(w, "%s_bucket{le=\"+Inf\"} %d\n%s_sum %g\n%s_count %d\n", name, h.n, name, h.sum, name, h.n)
}

// Gauge writes a single unlabelled sample.
func Gauge(w io.Writer, name, help string, v float64) {
	fmt.Fprintf(w, "# HELP %s %s\n# TYPE %s gauge\n%s %g\n", name, help, name, name, v)
}

// Counter writes a single unlabelled sample.
func Counter(w io.Writer, name, help string, v uint64) {
	fmt.Fprintf(w, "# HELP %s %s\n# TYPE %s counter\n%s %d\n", name, help, name, name, v)
}

// Labelled writes one sample per label value, in sorted order.
func Labelled(w io.Writer, name, kind, help, label string, values map[string]uint64) {
	fmt.Fprintf(w, "# HELP %s %s\n# TYPE %s %s\n", name, help, name, kind)
	keys := make([]string, 0, len(values))
	for k := range values {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		fmt.Fprintf(w, "%s{%s=\"%s\"} %d\n", name, label, escape(k), values[k])
	}
}

func escape(s string) string {
	return strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`).Replace(s)
}
