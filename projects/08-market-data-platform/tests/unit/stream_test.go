package unit

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"mdp/src/stream"
)

func TestPartitionRoutingIsPinned(t *testing.T) {
	// FNV-1a, modulo the partition count. These values must never change:
	// a key that moved partitions would lose its ordering against history.
	// The empty key is the FNV-1a offset basis 2166136261, checkable by hand.
	pins := []struct {
		key       string
		of4, of12 int32
	}{
		{"", 1, 1},
		{"venueA:XYZ", 2, 2}, {"venueA:ABC", 1, 5}, {"venueA:QRS", 1, 1},
		{"venueB:XYZ.B", 3, 3}, {"venueB:ABC.B", 0, 0}, {"venueB:LMN.B", 3, 3},
		{"venueB", 2, 10}, {"alpha/live/XYZ", 2, 6}, {"beta/live/main", 2, 2},
	}
	for _, p := range pins {
		if got := stream.PartitionFor([]byte(p.key), 4); got != p.of4 {
			t.Errorf("PartitionFor(%q, 4) = %d, pinned %d", p.key, got, p.of4)
		}
		if got := stream.PartitionFor([]byte(p.key), 12); got != p.of12 {
			t.Errorf("PartitionFor(%q, 12) = %d, pinned %d", p.key, got, p.of12)
		}
	}
	for _, n := range []int32{1, 0, -3} {
		if got := stream.PartitionFor([]byte("venueA:XYZ"), n); got != 0 {
			t.Errorf("PartitionFor with %d partitions = %d, want 0", n, got)
		}
	}
	rec := stream.Keyed(stream.TopicTrades, 4, "venueA:XYZ", []byte("v"))
	if rec.Topic != stream.TopicTrades || rec.Partition != 2 || string(rec.Key) != "venueA:XYZ" || string(rec.Value) != "v" {
		t.Errorf("Keyed built %+v", rec)
	}
}

func TestDefaultLayout(t *testing.T) {
	day := 24 * time.Hour
	want := map[string]stream.TopicSpec{
		"md.raw":         {Partitions: 4, Retention: day},
		"md.trades":      {Partitions: 4, Retention: 7 * day},
		"md.quotes":      {Partitions: 4, Retention: 7 * day},
		"md.depth":       {Partitions: 4, Retention: 7 * day},
		"md.gaps":        {Partitions: 4, Retention: 7 * day},
		"md.rejects":     {Partitions: 1, Retention: 14 * day},
		"md.health":      {Partitions: 1, Compact: true},
		"md.checkpoints": {Partitions: 1, Compact: true},
	}
	specs := stream.DefaultLayout().Specs()
	if len(specs) != len(want) {
		t.Fatalf("%d topics, want %d", len(specs), len(want))
	}
	for _, s := range specs {
		w, ok := want[s.Name]
		w.Name = s.Name
		if !ok || s != w {
			t.Errorf("topic %s: %+v, want %+v", s.Name, s, w)
		}
	}
}

func memTopic(t *testing.T, partitions int32) (*stream.MemBus, context.Context) {
	t.Helper()
	bus := stream.NewMemBus()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)
	if err := bus.EnsureTopics(ctx, stream.TopicSpec{Name: "t", Partitions: partitions}); err != nil {
		t.Fatal(err)
	}
	return bus, ctx
}

func TestMemBusOutageFailsEverythingAndKeepsRecords(t *testing.T) {
	bus, ctx := memTopic(t, 1)
	rec := stream.Record{Topic: "t", Value: []byte("a")}
	if err := bus.Produce(ctx, rec); err != nil {
		t.Fatal(err)
	}
	cons, err := bus.NewConsumer(ctx, stream.ConsumerConfig{Group: "g", Topics: []string{"t"}})
	if err != nil {
		t.Fatal(err)
	}

	bus.SetDown(true)
	if err := bus.Produce(ctx, rec); !errors.Is(err, stream.ErrUnavailable) {
		t.Errorf("produce during outage: %v", err)
	}
	if _, err := cons.Poll(ctx); !errors.Is(err, stream.ErrUnavailable) {
		t.Errorf("poll during outage: %v", err)
	}
	if err := cons.Commit(ctx, map[stream.TP]int64{{Topic: "t"}: 1}); !errors.Is(err, stream.ErrUnavailable) {
		t.Errorf("commit during outage: %v", err)
	}
	if _, err := bus.EndOffsets(ctx, "t"); !errors.Is(err, stream.ErrUnavailable) {
		t.Errorf("end offsets during outage: %v", err)
	}
	if _, err := bus.NewConsumer(ctx, stream.ConsumerConfig{Topics: []string{"t"}}); !errors.Is(err, stream.ErrUnavailable) {
		t.Errorf("new consumer during outage: %v", err)
	}

	bus.SetDown(false)
	recs, err := cons.Poll(ctx)
	if err != nil || len(recs) != 1 || string(recs[0].Value) != "a" {
		t.Errorf("after the outage the record is still there: %v %v", recs, err)
	}
	if len(bus.Committed("g")) != 0 {
		t.Error("a failed commit must not be recorded")
	}
}

func TestMemBusBatchIsAllOrNothing(t *testing.T) {
	bus, ctx := memTopic(t, 2)
	good := stream.Record{Topic: "t", Partition: 1, Value: []byte("ok")}
	for name, bad := range map[string]stream.Record{
		"unknown topic":      {Topic: "nope", Value: []byte("x")},
		"unknown partition":  {Topic: "t", Partition: 2, Value: []byte("x")},
		"negative partition": {Topic: "t", Partition: -1, Value: []byte("x")},
	} {
		if err := bus.Produce(ctx, good, bad, good); err == nil {
			t.Errorf("%s: produce should fail", name)
		}
	}
	if n := len(bus.All("t")); n != 0 {
		t.Errorf("failed batches left %d records behind", n)
	}
	bus.SetProduceHook(func(recs []stream.Record) error { return errors.New("injected") })
	if err := bus.Produce(ctx, good); err == nil || len(bus.All("t")) != 0 {
		t.Errorf("a failing hook must fail the produce and append nothing: %v", err)
	}
	bus.SetProduceHook(nil)
	if err := bus.Produce(ctx, good, good); err != nil || len(bus.Records("t", 1)) != 2 || len(bus.Records("t", 0)) != 0 {
		t.Errorf("produce after clearing the hook: %v", err)
	}
	cctx, cancel := context.WithCancel(ctx)
	cancel()
	if err := bus.Produce(cctx, good); !errors.Is(err, context.Canceled) {
		t.Errorf("produce on a cancelled context: %v", err)
	}
}

func TestMemBusPollWakesOnProduceAndStopsOnClose(t *testing.T) {
	bus, ctx := memTopic(t, 1)
	cons, _ := bus.NewConsumer(ctx, stream.ConsumerConfig{Topics: []string{"t"}})
	got := make(chan []stream.Record, 1)
	go func() {
		recs, _ := cons.Poll(ctx)
		got <- recs
	}()
	time.Sleep(20 * time.Millisecond) // let the poll block
	if err := bus.Produce(ctx, stream.Record{Topic: "t", Value: []byte("late")}); err != nil {
		t.Fatal(err)
	}
	select {
	case recs := <-got:
		if len(recs) != 1 || string(recs[0].Value) != "late" {
			t.Errorf("blocked poll returned %v", recs)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("a blocked poll was not woken by a produce")
	}

	errc := make(chan error, 1)
	go func() { _, err := cons.Poll(ctx); errc <- err }()
	time.Sleep(20 * time.Millisecond)
	bus.Close()
	select {
	case err := <-errc:
		if !errors.Is(err, stream.ErrClosed) {
			t.Errorf("poll after close: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("a blocked poll was not released by Close")
	}
	if err := bus.Produce(ctx, stream.Record{Topic: "t"}); !errors.Is(err, stream.ErrClosed) {
		t.Errorf("produce after close: %v", err)
	}
}

func TestMemBusPollRespectsMaxRecordsWithoutStarvingPartitions(t *testing.T) {
	bus, ctx := memTopic(t, 3)
	for p := int32(0); p < 3; p++ {
		for i := 0; i < 10; i++ {
			bus.Produce(ctx, stream.Record{Topic: "t", Partition: p, Value: []byte{byte(i)}})
		}
	}
	cons, _ := bus.NewConsumer(ctx, stream.ConsumerConfig{Topics: []string{"t"}, MaxRecords: 4})
	firstSeen := map[int32]int{}
	total := 0
	for poll := 0; total < 30; poll++ {
		recs, err := cons.Poll(ctx)
		if err != nil || len(recs) == 0 || len(recs) > 4 {
			t.Fatalf("poll %d: %d records, %v", poll, len(recs), err)
		}
		for _, r := range recs {
			if _, ok := firstSeen[r.Partition]; !ok {
				firstSeen[r.Partition] = poll
			}
		}
		total += len(recs)
	}
	// With rotation every partition is reached within the first few polls,
	// not only after the earlier partitions are drained.
	for p, poll := range firstSeen {
		if poll > 2 {
			t.Errorf("partition %d was first served on poll %d", p, poll)
		}
	}
}

func TestProgressAndWatchLag(t *testing.T) {
	bus, ctx := memTopic(t, 2)
	for i := 0; i < 5; i++ {
		bus.Produce(ctx, stream.Record{Topic: "t", Partition: int32(i % 2), Value: []byte{byte(i)}})
	}
	cons, _ := bus.NewConsumer(ctx, stream.ConsumerConfig{Topics: []string{"t"}})
	var progress stream.Progress
	progress.Init(cons.Positions())

	var mu sync.Mutex
	lags := map[stream.TP]int64{}
	caught := make(chan time.Duration, 4)
	wctx, stop := context.WithCancel(ctx)
	defer stop()
	go stream.WatchLag(wctx, bus, progress.Snapshot, 5*time.Millisecond, stream.LagObserver{
		OnLag:      func(tp stream.TP, lag int64) { mu.Lock(); lags[tp] = lag; mu.Unlock() },
		OnCaughtUp: func(d time.Duration) { caught <- d },
	})
	lagOf := func(p int32) int64 { mu.Lock(); defer mu.Unlock(); return lags[stream.TP{Topic: "t", Partition: p}] }
	wait := func(what string, cond func() bool) {
		t.Helper()
		for deadline := time.Now().Add(5 * time.Second); !cond(); time.Sleep(time.Millisecond) {
			if time.Now().After(deadline) {
				t.Fatalf("timed out waiting for %s", what)
			}
		}
	}
	wait("the backlog to be measured", func() bool { return lagOf(0) == 3 && lagOf(1) == 2 })
	select {
	case <-caught:
		t.Fatal("caught up reported while records were unprocessed")
	default:
	}

	// Reading is not processing: progress, and so lag, moves only on Advance.
	recs, _ := cons.Poll(ctx)
	if len(recs) != 5 {
		t.Fatalf("polled %d", len(recs))
	}
	time.Sleep(20 * time.Millisecond)
	if lagOf(0) != 3 || lagOf(1) != 2 {
		t.Errorf("lag fell to %d,%d before the records were processed", lagOf(0), lagOf(1))
	}
	progress.Advance(recs)
	wait("lag to reach zero", func() bool { return lagOf(0) == 0 && lagOf(1) == 0 })
	select {
	case d := <-caught:
		if d <= 0 {
			t.Errorf("catch-up time %s", d)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("catch-up was not reported")
	}

	// Catch-up is reported once, even if the consumer falls behind and recovers again.
	bus.Produce(ctx, stream.Record{Topic: "t", Value: []byte("more")})
	wait("new lag", func() bool { return lagOf(0) == 1 })
	recs, _ = cons.Poll(ctx)
	progress.Advance(recs)
	wait("lag to reach zero again", func() bool { return lagOf(0) == 0 })
	time.Sleep(20 * time.Millisecond)
	select {
	case <-caught:
		t.Error("catch-up reported twice")
	default:
	}
}
