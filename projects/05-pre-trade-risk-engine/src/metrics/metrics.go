// Package metrics is a small dependency-free registry that renders the
// Prometheus text exposition format.
package metrics

import (
	"fmt"
	"io"
	"sort"
	"strings"
	"sync"
)

// Histogram bucket upper bounds in seconds, 5µs to ~10s.
var bounds = func() []float64 {
	b := []float64{}
	for v := 5e-6; v < 11; v *= 2.5 {
		b = append(b, v)
	}
	return b
}()

type hist struct {
	buckets []uint64
	sum     float64
	count   uint64
}

type Registry struct {
	mu       sync.Mutex
	counters map[string]float64
	gauges   map[string]float64
	hists    map[string]*hist
}

func New() *Registry {
	return &Registry{counters: map[string]float64{}, gauges: map[string]float64{}, hists: map[string]*hist{}}
}

// key renders name{k="v",...} from alternating label keys and values.
func key(name string, labels []string) string {
	if len(labels) == 0 {
		return name
	}
	var sb strings.Builder
	sb.WriteString(name + "{")
	for i := 0; i+1 < len(labels); i += 2 {
		if i > 0 {
			sb.WriteByte(',')
		}
		fmt.Fprintf(&sb, "%s=%q", labels[i], labels[i+1])
	}
	sb.WriteByte('}')
	return sb.String()
}

func (r *Registry) Inc(name string, labels ...string) {
	r.mu.Lock()
	r.counters[key(name, labels)]++
	r.mu.Unlock()
}

func (r *Registry) Set(name string, v float64, labels ...string) {
	r.mu.Lock()
	r.gauges[key(name, labels)] = v
	r.mu.Unlock()
}

func (r *Registry) Observe(name string, seconds float64) {
	r.mu.Lock()
	h := r.hists[name]
	if h == nil {
		h = &hist{buckets: make([]uint64, len(bounds))}
		r.hists[name] = h
	}
	for i, b := range bounds {
		if seconds <= b {
			h.buckets[i]++
			break
		}
	}
	h.sum += seconds
	h.count++
	r.mu.Unlock()
}

// Counter and Gauge read a single series; used by tests and the demo.
func (r *Registry) Counter(name string, labels ...string) float64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.counters[key(name, labels)]
}

func (r *Registry) Gauge(name string, labels ...string) float64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.gauges[key(name, labels)]
}

func (r *Registry) HistCount(name string) uint64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	if h := r.hists[name]; h != nil {
		return h.count
	}
	return 0
}

func (r *Registry) Write(w io.Writer) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, m := range []map[string]float64{r.counters, r.gauges} {
		keys := make([]string, 0, len(m))
		for k := range m {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			fmt.Fprintf(w, "%s %g\n", k, m[k])
		}
	}
	names := make([]string, 0, len(r.hists))
	for n := range r.hists {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		h := r.hists[n]
		var cum uint64
		for i, b := range bounds {
			cum += h.buckets[i]
			fmt.Fprintf(w, "%s_bucket{le=\"%g\"} %d\n", n, b, cum)
		}
		fmt.Fprintf(w, "%s_bucket{le=\"+Inf\"} %d\n%s_sum %g\n%s_count %d\n", n, h.count, n, h.sum, n, h.count)
	}
}
