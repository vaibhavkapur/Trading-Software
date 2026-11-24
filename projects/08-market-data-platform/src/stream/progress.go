package stream

import "sync"

// Progress records, per partition, the next offset a service has not yet
// fully processed. Unlike a consumer's position it only advances once the
// records' effects are durable (published or persisted), so lag computed
// from it is the true backlog.
type Progress struct {
	mu   sync.Mutex
	next map[TP]int64
}

// Init sets the starting positions.
func (p *Progress) Init(positions map[TP]int64) {
	p.mu.Lock()
	p.next = make(map[TP]int64, len(positions))
	for tp, o := range positions {
		p.next[tp] = o
	}
	p.mu.Unlock()
}

// Advance marks recs as processed.
func (p *Progress) Advance(recs []Record) {
	p.mu.Lock()
	if p.next == nil {
		p.next = map[TP]int64{}
	}
	for _, r := range recs {
		p.next[TP{Topic: r.Topic, Partition: r.Partition}] = r.Offset + 1
	}
	p.mu.Unlock()
}

// Snapshot returns a copy of the positions.
func (p *Progress) Snapshot() map[TP]int64 {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make(map[TP]int64, len(p.next))
	for tp, o := range p.next {
		out[tp] = o
	}
	return out
}

// CaughtUp reports whether positions have reached the end of every
// partition of the given topics.
func CaughtUp(ends, positions map[TP]int64) bool {
	for tp, end := range ends {
		if positions[tp] < end {
			return false
		}
	}
	return true
}
