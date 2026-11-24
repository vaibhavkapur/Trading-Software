// Package pipeline is the feature-pipeline: it puts events back into
// event-time order behind a watermark and aggregates them into windows.
//
// Watermark policy (bounded out-of-orderness):
//
//	watermark = (largest event time seen) - (allowed lateness)
//
// and it never moves backwards. The watermark is a promise that nothing
// older will be processed any more:
//
//   - an event with event time <  watermark on arrival is LATE. It is kept
//     on the record but is never shown to a detector;
//   - an event with event time >= watermark is buffered, and handed on in
//     (event time, arrival sequence) order once the watermark passes it;
//   - a window [start, end) is final once watermark >= end.
//
// The watermark can also be advanced explicitly (end of a batch, or an
// idle stream), which is the only other way buffered events are released.
package pipeline

import (
	"container/heap"
	"math"
	"time"

	"surveillance/src/domain"
)

const unset = math.MinInt64

type Reorder struct {
	h        eventHeap
	lateness int64
	maxSeen  int64
	wm       int64
}

func NewReorder(allowedLateness time.Duration) *Reorder {
	return &Reorder{lateness: int64(allowedLateness), maxSeen: unset, wm: unset}
}

// SetLateness takes effect from the next event. Because the watermark is
// monotonic, raising the lateness does not reopen anything already final.
func (r *Reorder) SetLateness(d time.Duration) { r.lateness = int64(d) }

// Watermark reports the current watermark; ok is false before any event.
func (r *Reorder) Watermark() (wm int64, ok bool) { return r.wm, r.wm != unset }

// MaxSeen reports the largest event time accepted so far.
func (r *Reorder) MaxSeen() (ts int64, ok bool) { return r.maxSeen, r.maxSeen != unset }

func (r *Reorder) IsLate(ts int64) bool { return r.wm != unset && ts < r.wm }

func (r *Reorder) Buffered() int { return len(r.h) }

// Push buffers an on-time event and moves the watermark if this is the
// newest event time seen. The caller has already checked IsLate.
func (r *Reorder) Push(ev domain.Event) {
	heap.Push(&r.h, ev)
	ts := ev.TS()
	if r.maxSeen == unset || ts > r.maxSeen {
		r.maxSeen = ts
	}
	r.AdvanceTo(r.maxSeen - r.lateness)
}

// AdvanceTo raises the watermark; a lower value is ignored.
func (r *Reorder) AdvanceTo(ts int64) {
	if r.wm == unset || ts > r.wm {
		r.wm = ts
	}
}

// Release returns, in order, every buffered event the watermark has passed.
func (r *Reorder) Release() []domain.Event {
	var out []domain.Event
	for len(r.h) > 0 && r.h[0].TS() < r.wm {
		out = append(out, heap.Pop(&r.h).(domain.Event))
	}
	return out
}

type eventHeap []domain.Event

func (h eventHeap) Len() int { return len(h) }
func (h eventHeap) Less(i, j int) bool {
	a, b := h[i].TS(), h[j].TS()
	if a != b {
		return a < b
	}
	return h[i].Seq < h[j].Seq
}
func (h eventHeap) Swap(i, j int) { h[i], h[j] = h[j], h[i] }
func (h *eventHeap) Push(x any)   { *h = append(*h, x.(domain.Event)) }
func (h *eventHeap) Pop() any {
	old := *h
	n := len(old)
	x := old[n-1]
	old[n-1] = domain.Event{}
	*h = old[:n-1]
	return x
}
