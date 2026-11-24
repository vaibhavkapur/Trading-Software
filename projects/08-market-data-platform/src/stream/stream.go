// Package stream is the durable, partitioned log between services.
//
// Bus is the port. Two adapters implement it: KafkaBus (Kafka or Redpanda,
// the deployed transport) and MemBus (in-process, for hermetic tests and the
// standalone demo). Both use the same explicit partitioning, so a key lands
// on the same partition number on either.
package stream

import (
	"context"
	"errors"
	"hash/fnv"
	"time"
)

// Topic names.
const (
	TopicRaw         = "md.raw"         // parsed provider events, keyed by provider/sequence-domain
	TopicTrades      = "md.trades"      // canonical trades, keyed by instrument_id
	TopicQuotes      = "md.quotes"      // canonical quotes, keyed by instrument_id
	TopicDepth       = "md.depth"       // canonical depth snapshots, keyed by instrument_id
	TopicGaps        = "md.gaps"        // declared sequence gaps
	TopicRejects     = "md.rejects"     // dead letters from every stage
	TopicHealth      = "md.health"      // latest IngestHealth per provider (compacted)
	TopicCheckpoints = "md.checkpoints" // feed offsets and normalizer state (compacted)
)

// CanonicalTopics are the topics the sink persists and the gateway fans out.
var CanonicalTopics = []string{TopicTrades, TopicQuotes, TopicDepth, TopicGaps}

var (
	ErrUnavailable = errors.New("stream: broker unavailable")
	ErrClosed      = errors.New("stream: closed")
)

// TP identifies one partition of a topic.
type TP struct {
	Topic     string
	Partition int32
}

// Record is one log entry. Producers must set Topic, Partition, Key and
// Value; Offset and Time are assigned by the log.
type Record struct {
	Topic     string
	Partition int32
	Offset    int64
	Key       []byte
	Value     []byte
	Time      time.Time
}

// TopicSpec describes a topic to create.
type TopicSpec struct {
	Name       string
	Partitions int32
	Retention  time.Duration // 0 keeps the broker default
	Compact    bool
}

// Start says where a consumer begins on a partition that has neither an
// explicit nor a committed offset.
type Start int

const (
	FromEarliest Start = iota
	FromLatest
)

// ConsumerConfig configures a consumer that reads every partition of Topics.
type ConsumerConfig struct {
	// Group, if set, is where Commit stores offsets and where the consumer
	// resumes from. Consumers are assigned all partitions directly; there is
	// no group rebalancing (see docs/architecture.md).
	Group  string
	Topics []string
	Start  Start
	// Offsets, if set for a partition, overrides both the committed offset
	// and Start. Used by services that store offsets with their own state.
	Offsets    map[TP]int64
	MaxRecords int // per Poll; default 1000
}

// Consumer reads records in offset order per partition.
type Consumer interface {
	// Poll blocks until at least one record is available or ctx ends.
	Poll(ctx context.Context) ([]Record, error)
	// Commit stores, per partition, the offset of the next record to read.
	Commit(ctx context.Context, next map[TP]int64) error
	// Positions returns the next offset Poll will return per partition.
	Positions() map[TP]int64
	Close()
}

// Bus is the log port.
type Bus interface {
	// EnsureTopics creates missing topics and fails if an existing topic has
	// a different partition count, because key routing depends on it.
	EnsureTopics(ctx context.Context, specs ...TopicSpec) error
	// Produce appends records and returns once every record is acknowledged
	// as durable. It blocks while the broker is slow: that is the
	// backpressure signal producers propagate upstream.
	Produce(ctx context.Context, recs ...Record) error
	NewConsumer(ctx context.Context, cfg ConsumerConfig) (Consumer, error)
	// EndOffsets returns, per partition, the offset the next record will get.
	EndOffsets(ctx context.Context, topics ...string) (map[TP]int64, error)
	Close()
}

// PartitionFor maps a key to a partition with FNV-1a. It is deliberately not
// the client library's default partitioner: routing must be identical across
// adapters and stable across library upgrades.
func PartitionFor(key []byte, partitions int32) int32 {
	if partitions <= 1 {
		return 0
	}
	h := fnv.New32a()
	h.Write(key)
	return int32(h.Sum32() % uint32(partitions))
}

// Layout is the topic configuration shared by all services.
type Layout struct {
	Partitions         int32         // partitions of the raw and canonical topics
	RawRetention       time.Duration // how long parsed provider events can be re-normalized
	CanonicalRetention time.Duration // how long consumers can replay canonical events
	RejectRetention    time.Duration
}

// DefaultLayout is the retention policy documented in docs/architecture.md.
func DefaultLayout() Layout {
	return Layout{
		Partitions:         4,
		RawRetention:       24 * time.Hour,
		CanonicalRetention: 7 * 24 * time.Hour,
		RejectRetention:    14 * 24 * time.Hour,
	}
}

// Specs returns the topic specs for a layout.
func (l Layout) Specs() []TopicSpec {
	return []TopicSpec{
		{Name: TopicRaw, Partitions: l.Partitions, Retention: l.RawRetention},
		{Name: TopicTrades, Partitions: l.Partitions, Retention: l.CanonicalRetention},
		{Name: TopicQuotes, Partitions: l.Partitions, Retention: l.CanonicalRetention},
		{Name: TopicDepth, Partitions: l.Partitions, Retention: l.CanonicalRetention},
		{Name: TopicGaps, Partitions: l.Partitions, Retention: l.CanonicalRetention},
		{Name: TopicRejects, Partitions: 1, Retention: l.RejectRetention},
		{Name: TopicHealth, Partitions: 1, Compact: true},
		{Name: TopicCheckpoints, Partitions: 1, Compact: true},
	}
}

// Keyed builds a record routed by key.
func Keyed(topic string, partitions int32, key string, value []byte) Record {
	k := []byte(key)
	return Record{Topic: topic, Partition: PartitionFor(k, partitions), Key: k, Value: value}
}

// Lag returns, per partition, how many records lie between positions and the
// end of the log.
func Lag(ctx context.Context, bus Bus, positions map[TP]int64) (map[TP]int64, error) {
	topicSet := map[string]bool{}
	var topics []string
	for tp := range positions {
		if !topicSet[tp.Topic] {
			topicSet[tp.Topic] = true
			topics = append(topics, tp.Topic)
		}
	}
	ends, err := bus.EndOffsets(ctx, topics...)
	if err != nil {
		return nil, err
	}
	out := make(map[TP]int64, len(positions))
	for tp, pos := range positions {
		lag := ends[tp] - pos
		if lag < 0 {
			lag = 0
		}
		out[tp] = lag
	}
	return out, nil
}
