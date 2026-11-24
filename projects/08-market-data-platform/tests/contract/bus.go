// Package contract holds the behaviour every adapter of a port must share.
// The same suites run against the in-memory adapters (unit tests) and the
// real ones (integration tests), which is what justifies testing services
// on the in-memory adapters.
package contract

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"mdp/src/stream"
)

// RunBus checks a stream.Bus. The bus may hold other topics; every topic
// the suite creates has a unique name.
func RunBus(t *testing.T, bus stream.Bus) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	t.Cleanup(cancel)
	run := fmt.Sprintf("contract.%d.", time.Now().UnixNano())
	topic := func(name string, partitions int32) string {
		t.Helper()
		n := run + name
		if err := bus.EnsureTopics(ctx, stream.TopicSpec{Name: n, Partitions: partitions, Retention: time.Hour}); err != nil {
			t.Fatalf("create topic: %v", err)
		}
		return n
	}
	rec := func(topic string, partition int32, key, value string) stream.Record {
		return stream.Record{Topic: topic, Partition: partition, Key: []byte(key), Value: []byte(value)}
	}
	produce := func(recs ...stream.Record) {
		t.Helper()
		if err := bus.Produce(ctx, recs...); err != nil {
			t.Fatalf("produce: %v", err)
		}
	}
	consumer := func(cfg stream.ConsumerConfig) stream.Consumer {
		t.Helper()
		c, err := bus.NewConsumer(ctx, cfg)
		if err != nil {
			t.Fatalf("consumer: %v", err)
		}
		t.Cleanup(c.Close)
		return c
	}
	// read polls until n records have arrived.
	read := func(c stream.Consumer, n int) []stream.Record {
		t.Helper()
		var out []stream.Record
		rctx, cancel := context.WithTimeout(ctx, 30*time.Second)
		defer cancel()
		for len(out) < n {
			recs, err := c.Poll(rctx)
			if err != nil {
				t.Fatalf("poll: got %d of %d records: %v", len(out), n, err)
			}
			out = append(out, recs...)
		}
		return out
	}
	values := func(recs []stream.Record) string {
		var s []string
		for _, r := range recs {
			s = append(s, string(r.Value))
		}
		return strings.Join(s, ",")
	}
	// idle asserts that nothing arrives within a short window.
	idle := func(c stream.Consumer) {
		t.Helper()
		ictx, cancel := context.WithTimeout(ctx, 400*time.Millisecond)
		defer cancel()
		recs, err := c.Poll(ictx)
		if len(recs) != 0 {
			t.Fatalf("expected no records, got %s", values(recs))
		}
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("an idle poll should end with its deadline, got %v", err)
		}
	}

	t.Run("creating topics is idempotent and partition counts are enforced", func(t *testing.T) {
		name := topic("layout", 3)
		spec := stream.TopicSpec{Name: name, Partitions: 3, Retention: time.Hour}
		if err := bus.EnsureTopics(ctx, spec); err != nil {
			t.Errorf("ensuring an existing topic again: %v", err)
		}
		ends, err := bus.EndOffsets(ctx, name)
		if err != nil || len(ends) != 3 {
			t.Fatalf("end offsets: %v, %d partitions, want 3", err, len(ends))
		}
		spec.Partitions = 5
		if err := bus.EnsureTopics(ctx, spec); err == nil {
			t.Error("a topic with a different partition count must be refused: key routing depends on it")
		}
	})

	t.Run("records keep order, key and value within a partition", func(t *testing.T) {
		name := topic("order", 2)
		var batch []stream.Record
		for i := 0; i < 50; i++ {
			batch = append(batch, rec(name, int32(i%2), fmt.Sprintf("k%d", i%2), fmt.Sprintf("v%d", i)))
		}
		before := time.Now().Add(-time.Minute)
		produce(batch[:20]...)
		produce(batch[20:]...)
		got := read(consumer(stream.ConsumerConfig{Topics: []string{name}}), 50)
		next := map[int32]int64{}
		last := map[int32]int{-1: 0}
		for _, r := range got {
			if r.Topic != name {
				t.Fatalf("record from topic %q", r.Topic)
			}
			if r.Offset != next[r.Partition] {
				t.Fatalf("partition %d: offset %d, want %d", r.Partition, r.Offset, next[r.Partition])
			}
			next[r.Partition]++
			var i int
			fmt.Sscanf(string(r.Value), "v%d", &i)
			if i%2 != int(r.Partition) || string(r.Key) != fmt.Sprintf("k%d", i%2) {
				t.Fatalf("record %s with key %s on partition %d", r.Value, r.Key, r.Partition)
			}
			if prev, seen := last[r.Partition]; seen && i <= prev {
				t.Fatalf("partition %d: %s after v%d", r.Partition, r.Value, prev)
			}
			last[r.Partition] = i
			if r.Time.Before(before) {
				t.Fatalf("record time %s is not a log append time", r.Time)
			}
		}
		if next[0] != 25 || next[1] != 25 {
			t.Errorf("partitions hold %d and %d records, want 25 each", next[0], next[1])
		}
	})

	t.Run("a consumer starts from explicit, then committed, then configured position", func(t *testing.T) {
		name := topic("resume", 1)
		tp := stream.TP{Topic: name, Partition: 0}
		group := run + "group"
		produce(rec(name, 0, "k", "a"), rec(name, 0, "k", "b"), rec(name, 0, "k", "c"), rec(name, 0, "k", "d"))

		c1 := consumer(stream.ConsumerConfig{Group: group, Topics: []string{name}, Start: stream.FromEarliest})
		if got := values(read(c1, 4)); got != "a,b,c,d" {
			t.Fatalf("a new group reading from earliest got %s", got)
		}
		if pos := c1.Positions()[tp]; pos != 4 {
			t.Errorf("position after 4 records = %d", pos)
		}
		if err := c1.Commit(ctx, map[stream.TP]int64{tp: 2}); err != nil {
			t.Fatalf("commit: %v", err)
		}
		c1.Close()

		c2 := consumer(stream.ConsumerConfig{Group: group, Topics: []string{name}, Start: stream.FromLatest})
		if pos := c2.Positions()[tp]; pos != 2 {
			t.Errorf("a restarted group member is positioned at %d, want the committed 2", pos)
		}
		if got := values(read(c2, 2)); got != "c,d" {
			t.Errorf("a restarted group member got %s, want c,d", got)
		}

		c3 := consumer(stream.ConsumerConfig{Group: group, Topics: []string{name}, Offsets: map[stream.TP]int64{tp: 1}})
		if got := values(read(c3, 3)); got != "b,c,d" {
			t.Errorf("an explicit offset of 1 got %s, want b,c,d", got)
		}

		other := consumer(stream.ConsumerConfig{Group: run + "other", Topics: []string{name}})
		if got := values(read(other, 4)); got != "a,b,c,d" {
			t.Errorf("another group must not share offsets: got %s", got)
		}
	})

	t.Run("a consumer from latest sees only later records", func(t *testing.T) {
		name := topic("latest", 2)
		produce(rec(name, 0, "k", "old0"), rec(name, 1, "k", "old1"))
		c := consumer(stream.ConsumerConfig{Topics: []string{name}, Start: stream.FromLatest})
		idle(c)
		produce(rec(name, 1, "k", "new"))
		if got := values(read(c, 1)); got != "new" {
			t.Errorf("got %s, want new", got)
		}
	})

	t.Run("end offsets and lag follow the log", func(t *testing.T) {
		name := topic("lag", 2)
		ends, err := bus.EndOffsets(ctx, name)
		if err != nil || ends[stream.TP{Topic: name, Partition: 0}] != 0 || ends[stream.TP{Topic: name, Partition: 1}] != 0 {
			t.Fatalf("an empty topic should end at 0: %v %v", ends, err)
		}
		produce(rec(name, 0, "k", "a"), rec(name, 0, "k", "b"), rec(name, 1, "k", "c"))
		c := consumer(stream.ConsumerConfig{Topics: []string{name}})
		lag, err := stream.Lag(ctx, bus, c.Positions())
		if err != nil || lag[stream.TP{Topic: name, Partition: 0}] != 2 || lag[stream.TP{Topic: name, Partition: 1}] != 1 {
			t.Fatalf("lag before reading = %v, %v; want 2 and 1", lag, err)
		}
		read(c, 3)
		lag, err = stream.Lag(ctx, bus, c.Positions())
		if err != nil || lag[stream.TP{Topic: name, Partition: 0}] != 0 || lag[stream.TP{Topic: name, Partition: 1}] != 0 {
			t.Fatalf("lag after reading = %v, %v; want 0", lag, err)
		}
		ends, _ = bus.EndOffsets(ctx, name)
		if !stream.CaughtUp(ends, c.Positions()) {
			t.Error("a consumer at the end of every partition is caught up")
		}
	})

	t.Run("one produce call can span topics", func(t *testing.T) {
		a, b := topic("span-a", 1), topic("span-b", 1)
		produce(rec(a, 0, "k", "1"), rec(b, 0, "k", "2"), rec(a, 0, "k", "3"))
		if got := values(read(consumer(stream.ConsumerConfig{Topics: []string{a}}), 2)); got != "1,3" {
			t.Errorf("topic a holds %s, want 1,3", got)
		}
		if got := values(read(consumer(stream.ConsumerConfig{Topics: []string{b}}), 1)); got != "2" {
			t.Errorf("topic b holds %s, want 2", got)
		}
		both := read(consumer(stream.ConsumerConfig{Topics: []string{a, b}}), 3)
		if len(both) != 3 {
			t.Errorf("a consumer of both topics got %d records", len(both))
		}
	})

	t.Run("checkpoints keep the latest value of each key", func(t *testing.T) {
		if err := bus.EnsureTopics(ctx, stream.TopicSpec{Name: stream.TopicCheckpoints, Partitions: 1, Compact: true}); err != nil {
			t.Fatal(err)
		}
		ck := stream.NewCheckpoints(bus)
		k1, k2 := run+"one", run+"two"
		for _, kv := range [][2]string{{k1, "1"}, {k2, "x"}, {k1, "2"}, {k1, "3"}} {
			if err := ck.Put(ctx, kv[0], []byte(kv[1])); err != nil {
				t.Fatal(err)
			}
		}
		all, err := ck.Load(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if string(all[k1]) != "3" || string(all[k2]) != "x" {
			t.Errorf("loaded %q and %q, want 3 and x", all[k1], all[k2])
		}
		if _, ok := all[run+"never-written"]; ok {
			t.Error("a key that was never written must be absent")
		}
	})
}
