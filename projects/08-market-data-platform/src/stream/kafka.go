package stream

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/twmb/franz-go/pkg/kadm"
	"github.com/twmb/franz-go/pkg/kerr"
	"github.com/twmb/franz-go/pkg/kgo"
)

// KafkaBus is the Bus adapter for Kafka-protocol brokers (Kafka, Redpanda).
//
// Producing uses acks=all with the idempotent producer, so a record
// acknowledged by Produce survives the loss of a broker and client retries
// do not duplicate it. Partitions are chosen by PartitionFor, not by the
// client library.
//
// Consumers are assigned every partition of their topics directly and store
// offsets under their group id. There is no group membership and so no
// rebalancing: run one instance of each consuming service.
//
// A namespace, if set, is prefixed to every topic and group name on the
// broker and removed again on the way back, so several independent pipelines
// (environments, test runs) can share one cluster without seeing each other.
type KafkaBus struct {
	brokers  []string
	clientID string
	ns       string
	cl       *kgo.Client
	adm      *kadm.Client

	mu        sync.Mutex
	consumers map[*kafkaConsumer]struct{}
}

// NewKafkaBus connects to the brokers. It does not fail if they are down;
// operations fail or block until they are reachable. namespace may be empty.
func NewKafkaBus(brokers []string, clientID, namespace string) (*KafkaBus, error) {
	for _, r := range namespace {
		if !(r == '.' || r == '_' || r == '-' || r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9') {
			return nil, fmt.Errorf("stream: namespace %q may only contain letters, digits, '.', '_' and '-'", namespace)
		}
	}
	cl, err := kgo.NewClient(
		kgo.SeedBrokers(brokers...),
		kgo.ClientID(clientID),
		kgo.RecordPartitioner(kgo.ManualPartitioner()),
		kgo.RequiredAcks(kgo.AllISRAcks()),
		// Bound how long Produce waits so callers see a failing broker,
		// log it and retry, instead of blocking without a trace.
		kgo.RecordDeliveryTimeout(10*time.Second),
		kgo.ProducerLinger(0),
		// The client caches metadata answers, including "no such topic",
		// for this long (5s by default). EnsureTopics waits for a topic it
		// created to become visible, so a long cache is a long startup.
		kgo.MetadataMinAge(250*time.Millisecond),
	)
	if err != nil {
		return nil, fmt.Errorf("stream: kafka client: %w", err)
	}
	return &KafkaBus{brokers: brokers, clientID: clientID, ns: namespace, cl: cl, adm: kadm.NewClient(cl),
		consumers: map[*kafkaConsumer]struct{}{}}, nil
}

// wire is a topic or group name as the broker knows it.
func (b *KafkaBus) wire(name string) string { return b.ns + name }

// logical is the inverse of wire.
func (b *KafkaBus) logical(name string) string { return strings.TrimPrefix(name, b.ns) }

func (b *KafkaBus) wireAll(names []string) []string {
	out := make([]string, len(names))
	for i, n := range names {
		out[i] = b.wire(n)
	}
	return out
}

// EnsureTopics creates the topics that do not exist and returns once every
// one of them is visible with the expected partition count and a leader for
// each partition. Waiting matters: a broker acknowledges a creation before
// its metadata shows the topic, and a caller that went on to list offsets in
// that window would be told the topic does not exist.
//
// A topic that exists with a different partition count is an error, whoever
// created it: PartitionFor depends on the count, so two services that
// disagree about it would route the same key to different partitions.
func (b *KafkaBus) EnsureTopics(ctx context.Context, specs ...TopicSpec) error {
	names := make([]string, len(specs))
	for i, s := range specs {
		names[i] = b.wire(s.Name)
	}
	created := map[string]bool{}
	for {
		existing, err := b.adm.ListTopics(ctx, names...)
		if err != nil {
			return fmt.Errorf("%w: list topics: %v", ErrUnavailable, err)
		}
		ready := true
		for _, s := range specs {
			name := b.wire(s.Name)
			if d, ok := existing[name]; ok && d.Err == nil {
				if got := int32(len(d.Partitions)); got != s.Partitions {
					return fmt.Errorf("stream: topic %s has %d partitions, want %d", s.Name, got, s.Partitions)
				}
				for _, p := range d.Partitions {
					if p.Err != nil || p.Leader < 0 {
						ready = false
					}
				}
				continue
			}
			ready = false
			if created[name] {
				continue // created, not yet visible
			}
			configs := map[string]*string{}
			if s.Retention > 0 {
				configs["retention.ms"] = ptr(strconv.FormatInt(s.Retention.Milliseconds(), 10))
			}
			if s.Compact {
				configs["cleanup.policy"] = ptr("compact")
			}
			// Replication factor -1 uses the broker default.
			resp, err := b.adm.CreateTopic(ctx, s.Partitions, -1, configs, name)
			if err == nil {
				err = resp.Err
			}
			// Already exists: another service got there first. The next
			// pass checks its partition count like any other topic.
			if err != nil && !errors.Is(err, kerr.TopicAlreadyExists) {
				return fmt.Errorf("stream: create topic %s: %w", s.Name, err)
			}
			created[name] = true
		}
		if ready {
			return nil
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("%w: topics not ready: %v", ErrUnavailable, ctx.Err())
		case <-time.After(50 * time.Millisecond):
		}
	}
}

func (b *KafkaBus) Produce(ctx context.Context, recs ...Record) error {
	if len(recs) == 0 {
		return nil
	}
	krecs := make([]*kgo.Record, len(recs))
	for i, r := range recs {
		krecs[i] = &kgo.Record{Topic: b.wire(r.Topic), Partition: r.Partition, Key: r.Key, Value: r.Value}
	}
	if err := b.cl.ProduceSync(ctx, krecs...).FirstErr(); err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return fmt.Errorf("%w: produce: %v", ErrUnavailable, err)
	}
	return nil
}

func (b *KafkaBus) EndOffsets(ctx context.Context, topics ...string) (map[TP]int64, error) {
	listed, err := b.adm.ListEndOffsets(ctx, b.wireAll(topics)...)
	if err != nil {
		return nil, fmt.Errorf("%w: end offsets: %v", ErrUnavailable, err)
	}
	return b.collect(listed)
}

func (b *KafkaBus) collect(listed kadm.ListedOffsets) (map[TP]int64, error) {
	out := map[TP]int64{}
	var first error
	listed.Each(func(o kadm.ListedOffset) {
		if o.Err != nil && first == nil {
			first = fmt.Errorf("%w: offsets of %s/%d: %v", ErrUnavailable, o.Topic, o.Partition, o.Err)
		}
		out[TP{b.logical(o.Topic), o.Partition}] = o.Offset
	})
	return out, first
}

func (b *KafkaBus) NewConsumer(ctx context.Context, cfg ConsumerConfig) (Consumer, error) {
	starts, err := b.adm.ListStartOffsets(ctx, b.wireAll(cfg.Topics)...)
	if err != nil {
		return nil, fmt.Errorf("%w: start offsets: %v", ErrUnavailable, err)
	}
	earliest, err := b.collect(starts)
	if err != nil {
		return nil, err
	}
	latest, err := b.EndOffsets(ctx, cfg.Topics...)
	if err != nil {
		return nil, err
	}
	if len(latest) == 0 {
		return nil, fmt.Errorf("stream: topics %v do not exist", cfg.Topics)
	}
	committed := map[TP]int64{}
	if cfg.Group != "" {
		resp, err := b.adm.FetchOffsets(ctx, b.wire(cfg.Group))
		if err != nil && !errors.Is(err, kerr.GroupIDNotFound) {
			return nil, fmt.Errorf("%w: fetch offsets of %s: %v", ErrUnavailable, cfg.Group, err)
		}
		resp.Each(func(o kadm.OffsetResponse) {
			if o.Err == nil && o.At >= 0 {
				committed[TP{b.logical(o.Topic), o.Partition}] = o.At
			}
		})
	}
	pos := map[TP]int64{}
	assign := map[string]map[int32]kgo.Offset{}
	for tp := range latest {
		switch {
		case hasOffset(cfg.Offsets, tp):
			pos[tp] = cfg.Offsets[tp]
		case hasOffset(committed, tp):
			pos[tp] = committed[tp]
		case cfg.Start == FromLatest:
			pos[tp] = latest[tp]
		default:
			pos[tp] = earliest[tp]
		}
		topic := b.wire(tp.Topic)
		if assign[topic] == nil {
			assign[topic] = map[int32]kgo.Offset{}
		}
		assign[topic][tp.Partition] = kgo.NewOffset().At(pos[tp])
	}
	cl, err := kgo.NewClient(
		kgo.SeedBrokers(b.brokers...),
		kgo.ClientID(b.clientID),
		kgo.ConsumePartitions(assign),
		kgo.FetchMaxWait(250*time.Millisecond),
	)
	if err != nil {
		return nil, fmt.Errorf("stream: kafka consumer: %w", err)
	}
	max := cfg.MaxRecords
	if max <= 0 {
		max = 1000
	}
	c := &kafkaConsumer{bus: b, cl: cl, group: cfg.Group, max: max, pos: pos}
	b.mu.Lock()
	b.consumers[c] = struct{}{}
	b.mu.Unlock()
	return c, nil
}

func (b *KafkaBus) Close() {
	b.mu.Lock()
	cs := b.consumers
	b.consumers = map[*kafkaConsumer]struct{}{}
	b.mu.Unlock()
	for c := range cs {
		c.cl.Close()
	}
	b.cl.Close()
}

type kafkaConsumer struct {
	bus   *KafkaBus
	cl    *kgo.Client
	group string
	max   int

	mu  sync.Mutex
	pos map[TP]int64
}

func (c *kafkaConsumer) Poll(ctx context.Context) ([]Record, error) {
	for {
		fetches := c.cl.PollRecords(ctx, c.max)
		if fetches.IsClientClosed() {
			return nil, ErrClosed
		}
		var out []Record
		c.mu.Lock()
		fetches.EachRecord(func(r *kgo.Record) {
			topic := c.bus.logical(r.Topic)
			out = append(out, Record{Topic: topic, Partition: r.Partition, Offset: r.Offset,
				Key: r.Key, Value: r.Value, Time: r.Timestamp})
			c.pos[TP{topic, r.Partition}] = r.Offset + 1
		})
		c.mu.Unlock()
		if len(out) > 0 {
			return out, nil
		}
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		for _, fe := range fetches.Errors() {
			return nil, fmt.Errorf("%w: fetch %s/%d: %v", ErrUnavailable, c.bus.logical(fe.Topic), fe.Partition, fe.Err)
		}
	}
}

func (c *kafkaConsumer) Commit(ctx context.Context, next map[TP]int64) error {
	if c.group == "" || len(next) == 0 {
		return nil
	}
	offs := kadm.Offsets{}
	for tp, o := range next {
		offs.AddOffset(c.bus.wire(tp.Topic), tp.Partition, o, -1)
	}
	resp, err := c.bus.adm.CommitOffsets(ctx, c.bus.wire(c.group), offs)
	if err == nil {
		err = resp.Error()
	}
	if err != nil {
		return fmt.Errorf("%w: commit to %s: %v", ErrUnavailable, c.group, err)
	}
	return nil
}

func (c *kafkaConsumer) Positions() map[TP]int64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make(map[TP]int64, len(c.pos))
	for tp, o := range c.pos {
		out[tp] = o
	}
	return out
}

func (c *kafkaConsumer) Close() {
	c.bus.mu.Lock()
	delete(c.bus.consumers, c)
	c.bus.mu.Unlock()
	c.cl.Close()
}

func ptr[T any](v T) *T { return &v }

// DeleteNamespace deletes every topic and consumer group of this bus's
// namespace. It exists for throwaway namespaces (benchmarks, tests) and
// refuses to run without one, so it cannot delete a shared cluster's topics.
func (b *KafkaBus) DeleteNamespace(ctx context.Context) error {
	if b.ns == "" {
		return errors.New("stream: refusing to delete topics without a namespace")
	}
	topics, err := b.adm.ListTopics(ctx)
	if err != nil {
		return fmt.Errorf("%w: list topics: %v", ErrUnavailable, err)
	}
	var mine []string
	for name := range topics {
		if strings.HasPrefix(name, b.ns) {
			mine = append(mine, name)
		}
	}
	if len(mine) > 0 {
		resp, err := b.adm.DeleteTopics(ctx, mine...)
		if err == nil {
			err = resp.Error()
		}
		if err != nil {
			return fmt.Errorf("stream: delete topics: %w", err)
		}
	}
	groups, err := b.adm.ListGroups(ctx)
	if err != nil {
		return fmt.Errorf("%w: list groups: %v", ErrUnavailable, err)
	}
	var stale []string
	for _, g := range groups.Groups() {
		if strings.HasPrefix(g, b.ns) {
			stale = append(stale, g)
		}
	}
	if len(stale) > 0 {
		if _, err := b.adm.DeleteGroups(ctx, stale...); err != nil {
			return fmt.Errorf("stream: delete groups: %w", err)
		}
	}
	return nil
}
