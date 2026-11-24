package stream

import (
	"context"
	"fmt"
)

// Checkpoints is a small key/value store on a compacted topic. Services use
// it to keep the state they need to resume: ingestors store FeedOffsets, the
// normalizer stores its input offset together with its sequence tracker so
// the two can never disagree.
type Checkpoints struct {
	bus   Bus
	topic string
}

func NewCheckpoints(bus Bus) *Checkpoints {
	return &Checkpoints{bus: bus, topic: TopicCheckpoints}
}

// Put durably stores value under key.
func (c *Checkpoints) Put(ctx context.Context, key string, value []byte) error {
	return c.bus.Produce(ctx, Record{Topic: c.topic, Partition: 0, Key: []byte(key), Value: value})
}

// Load returns the latest value of every key.
func (c *Checkpoints) Load(ctx context.Context) (map[string][]byte, error) {
	return LatestByKey(ctx, c.bus, c.topic)
}

// LatestByKey reads a topic from the beginning up to its current end and
// returns the last value written for each key.
func LatestByKey(ctx context.Context, bus Bus, topic string) (map[string][]byte, error) {
	ends, err := bus.EndOffsets(ctx, topic)
	if err != nil {
		return nil, fmt.Errorf("stream: end offsets of %s: %w", topic, err)
	}
	out := map[string][]byte{}
	remaining := false
	for _, end := range ends {
		if end > 0 {
			remaining = true
		}
	}
	if !remaining {
		return out, nil
	}
	cons, err := bus.NewConsumer(ctx, ConsumerConfig{Topics: []string{topic}, Start: FromEarliest})
	if err != nil {
		return nil, err
	}
	defer cons.Close()
	for {
		done := true
		for tp, pos := range cons.Positions() {
			if pos < ends[tp] {
				done = false
			}
		}
		if done {
			return out, nil
		}
		recs, err := cons.Poll(ctx)
		if err != nil {
			return nil, fmt.Errorf("stream: reading %s: %w", topic, err)
		}
		for _, r := range recs {
			out[string(r.Key)] = r.Value
		}
	}
}
