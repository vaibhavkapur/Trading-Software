package stream

import (
	"context"
	"fmt"
	"sort"
	"sync"
	"time"
)

// MemBus is an in-process Bus. It keeps every record in memory for the life
// of the process, ignores retention and compaction, and exists for hermetic
// tests and the standalone demo. It models a broker that outlives the
// services using it: services can be stopped and restarted against the same
// MemBus and will find their records and committed offsets intact.
type MemBus struct {
	mu     sync.Mutex
	topics map[string][][]Record
	groups map[string]map[TP]int64
	wake   chan struct{} // closed and replaced on every append
	down   bool
	closed bool
	// produceHook, if set, runs before each Produce and can fail it.
	produceHook func(recs []Record) error
	// commitHook, if set, runs before each offset commit and can fail it.
	commitHook func(group string) error
	now        func() time.Time
}

func NewMemBus() *MemBus {
	return &MemBus{
		topics: map[string][][]Record{},
		groups: map[string]map[TP]int64{},
		wake:   make(chan struct{}),
		now:    time.Now,
	}
}

// SetDown simulates a broker outage: Produce and Poll fail with
// ErrUnavailable until it is cleared. Stored records are unaffected.
func (b *MemBus) SetDown(down bool) {
	b.mu.Lock()
	b.down = down
	b.broadcastLocked()
	b.mu.Unlock()
}

// SetProduceHook installs a fault-injection hook for Produce.
func (b *MemBus) SetProduceHook(h func(recs []Record) error) {
	b.mu.Lock()
	b.produceHook = h
	b.mu.Unlock()
}

// SetCommitHook installs a fault-injection hook for offset commits.
func (b *MemBus) SetCommitHook(h func(group string) error) {
	b.mu.Lock()
	b.commitHook = h
	b.mu.Unlock()
}

func (b *MemBus) broadcastLocked() {
	close(b.wake)
	b.wake = make(chan struct{})
}

func (b *MemBus) EnsureTopics(_ context.Context, specs ...TopicSpec) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	for _, s := range specs {
		if s.Partitions < 1 {
			return fmt.Errorf("stream: topic %s: partitions must be >= 1", s.Name)
		}
		if parts, ok := b.topics[s.Name]; ok {
			if int32(len(parts)) != s.Partitions {
				return fmt.Errorf("stream: topic %s has %d partitions, want %d", s.Name, len(parts), s.Partitions)
			}
			continue
		}
		b.topics[s.Name] = make([][]Record, s.Partitions)
	}
	return nil
}

func (b *MemBus) Produce(ctx context.Context, recs ...Record) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed {
		return ErrClosed
	}
	if b.down {
		return ErrUnavailable
	}
	if b.produceHook != nil {
		if err := b.produceHook(recs); err != nil {
			return err
		}
	}
	// Validate everything first so a batch is appended entirely or not at all.
	for _, r := range recs {
		parts, ok := b.topics[r.Topic]
		if !ok {
			return fmt.Errorf("stream: unknown topic %s", r.Topic)
		}
		if r.Partition < 0 || int(r.Partition) >= len(parts) {
			return fmt.Errorf("stream: topic %s has no partition %d", r.Topic, r.Partition)
		}
	}
	now := b.now()
	for _, r := range recs {
		parts := b.topics[r.Topic]
		r.Offset = int64(len(parts[r.Partition]))
		r.Time = now
		parts[r.Partition] = append(parts[r.Partition], r)
	}
	b.broadcastLocked()
	return nil
}

func (b *MemBus) EndOffsets(_ context.Context, topics ...string) (map[TP]int64, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.down {
		return nil, ErrUnavailable
	}
	out := map[TP]int64{}
	for _, t := range topics {
		parts, ok := b.topics[t]
		if !ok {
			return nil, fmt.Errorf("stream: unknown topic %s", t)
		}
		for p := range parts {
			out[TP{t, int32(p)}] = int64(len(parts[p]))
		}
	}
	return out, nil
}

// Committed returns a group's committed offsets, for tests.
func (b *MemBus) Committed(group string) map[TP]int64 {
	b.mu.Lock()
	defer b.mu.Unlock()
	out := map[TP]int64{}
	for tp, o := range b.groups[group] {
		out[tp] = o
	}
	return out
}

// Records returns a copy of one partition's records, for tests.
func (b *MemBus) Records(topic string, partition int32) []Record {
	b.mu.Lock()
	defer b.mu.Unlock()
	parts := b.topics[topic]
	if int(partition) >= len(parts) {
		return nil
	}
	return append([]Record(nil), parts[partition]...)
}

// All returns every record of a topic across partitions, for tests.
func (b *MemBus) All(topic string) []Record {
	b.mu.Lock()
	defer b.mu.Unlock()
	var out []Record
	for _, p := range b.topics[topic] {
		out = append(out, p...)
	}
	return out
}

func (b *MemBus) Close() {
	b.mu.Lock()
	b.closed = true
	b.broadcastLocked()
	b.mu.Unlock()
}

func (b *MemBus) NewConsumer(_ context.Context, cfg ConsumerConfig) (Consumer, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.down {
		return nil, ErrUnavailable
	}
	c := &memConsumer{bus: b, group: cfg.Group, pos: map[TP]int64{}, max: cfg.MaxRecords}
	if c.max <= 0 {
		c.max = 1000
	}
	for _, t := range cfg.Topics {
		parts, ok := b.topics[t]
		if !ok {
			return nil, fmt.Errorf("stream: unknown topic %s", t)
		}
		for p := range parts {
			tp := TP{t, int32(p)}
			switch {
			case hasOffset(cfg.Offsets, tp):
				c.pos[tp] = cfg.Offsets[tp]
			case cfg.Group != "" && hasOffset(b.groups[cfg.Group], tp):
				c.pos[tp] = b.groups[cfg.Group][tp]
			case cfg.Start == FromLatest:
				c.pos[tp] = int64(len(parts[p]))
			default:
				c.pos[tp] = 0
			}
			c.order = append(c.order, tp)
		}
	}
	sort.Slice(c.order, func(i, j int) bool {
		if c.order[i].Topic != c.order[j].Topic {
			return c.order[i].Topic < c.order[j].Topic
		}
		return c.order[i].Partition < c.order[j].Partition
	})
	return c, nil
}

func hasOffset(m map[TP]int64, tp TP) bool {
	_, ok := m[tp]
	return ok
}

type memConsumer struct {
	bus    *MemBus
	group  string
	max    int
	order  []TP
	next   int // rotates the starting partition so no partition starves
	pos    map[TP]int64
	closed bool
}

func (c *memConsumer) Poll(ctx context.Context) ([]Record, error) {
	for {
		c.bus.mu.Lock()
		if c.closed || c.bus.closed {
			c.bus.mu.Unlock()
			return nil, ErrClosed
		}
		if c.bus.down {
			c.bus.mu.Unlock()
			return nil, ErrUnavailable
		}
		var out []Record
		for i := 0; i < len(c.order) && len(out) < c.max; i++ {
			tp := c.order[(c.next+i)%len(c.order)]
			log := c.bus.topics[tp.Topic][tp.Partition]
			for c.pos[tp] < int64(len(log)) && len(out) < c.max {
				out = append(out, log[c.pos[tp]])
				c.pos[tp]++
			}
		}
		if len(c.order) > 0 {
			c.next = (c.next + 1) % len(c.order)
		}
		wake := c.bus.wake
		c.bus.mu.Unlock()
		if len(out) > 0 {
			return out, nil
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-wake:
		}
	}
}

func (c *memConsumer) Commit(_ context.Context, next map[TP]int64) error {
	if c.group == "" {
		return nil
	}
	c.bus.mu.Lock()
	defer c.bus.mu.Unlock()
	if c.bus.down {
		return ErrUnavailable
	}
	if c.bus.commitHook != nil {
		if err := c.bus.commitHook(c.group); err != nil {
			return err
		}
	}
	g := c.bus.groups[c.group]
	if g == nil {
		g = map[TP]int64{}
		c.bus.groups[c.group] = g
	}
	for tp, o := range next {
		g[tp] = o
	}
	return nil
}

func (c *memConsumer) Positions() map[TP]int64 {
	c.bus.mu.Lock()
	defer c.bus.mu.Unlock()
	out := make(map[TP]int64, len(c.pos))
	for tp, o := range c.pos {
		out[tp] = o
	}
	return out
}

func (c *memConsumer) Close() {
	c.bus.mu.Lock()
	c.closed = true
	c.bus.broadcastLocked()
	c.bus.mu.Unlock()
}
