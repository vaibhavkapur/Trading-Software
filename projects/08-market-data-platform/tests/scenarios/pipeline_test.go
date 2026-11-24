// Package scenarios runs complete pipelines against simulated venues and
// injects the failures the platform is specified to survive. Everything is
// hermetic: loopback sockets, an in-memory log and store, no Docker.
package scenarios

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strconv"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"mdp/src/domain"
	"mdp/src/ingestors"
	"mdp/src/normalizer"
	"mdp/src/storage"
	"mdp/src/stream"
	"mdp/tests/harness"
)

// event is the part of a canonical message these tests inspect.
type event struct {
	Type         string    `json:"type"`
	InstrumentID string    `json:"instrument_id"`
	Provider     string    `json:"provider"`
	Session      string    `json:"session"`
	Seq          uint64    `json:"seq"`
	Source       string    `json:"source"`
	Flags        []string  `json:"flags"`
	EventTime    time.Time `json:"event_time"`
	ReceiveTime  time.Time `json:"receive_time"`
	Version      int       `json:"normalizer_version"`
}

func canonical(e *harness.Env) []event {
	var out []event
	for _, topic := range []string{stream.TopicTrades, stream.TopicQuotes, stream.TopicDepth} {
		for _, r := range e.Mem.All(topic) {
			var ev event
			if err := json.Unmarshal(r.Value, &ev); err != nil {
				e.T.Fatalf("undecodable canonical record: %v", err)
			}
			out = append(out, ev)
		}
	}
	return out
}

func has(flags []string, f string) bool {
	for _, x := range flags {
		if x == f {
			return true
		}
	}
	return false
}

func missing(gaps []domain.Gap, provider string) (n uint64) {
	for _, g := range gaps {
		if g.Provider == provider {
			n += g.Missing()
		}
	}
	return n
}

func wantInt(t *testing.T, what string, got, want int) {
	t.Helper()
	if got != want {
		t.Errorf("%s = %d, want %d", what, got, want)
	}
}

func TestLiveFeedsAreNormalizedAndStoredExactlyOnce(t *testing.T) {
	e := harness.New(t)
	e.StartIngestors()
	n, s := e.StartNormalizer(), e.StartSink()

	e.Sim.Step(500) // 500 events on each of the two venues
	e.WaitRaw(1000)
	e.Settle(n, s)

	wantInt(t, "canonical events in the log", e.Canonical(), 1000)
	wantInt(t, "distinct rows in the store", e.Stored(), 1000)
	wantInt(t, "gaps", len(e.Gaps()), 0)
	wantInt(t, "dead letters", len(e.Rejects()), 0)
	wantInt(t, "instruments in the store", e.MemDB.Rows("instruments"), len(e.Reg.All()))

	seen := map[string]bool{}
	for _, ev := range canonical(e) {
		seen[ev.Provider+"/"+ev.Type] = true
		if _, ok := e.Reg.Get(ev.InstrumentID); !ok {
			t.Fatalf("event for unknown instrument %q", ev.InstrumentID)
		}
		if ev.EventTime.IsZero() || ev.ReceiveTime.IsZero() {
			t.Fatalf("event without both timestamps: %+v", ev)
		}
		if ev.EventTime.Location() != time.UTC || ev.ReceiveTime.Location() != time.UTC {
			t.Fatalf("timestamps must be UTC: %+v", ev)
		}
		if ev.Session == "" || ev.Seq == 0 || ev.Source != domain.SourceLive || ev.Version != domain.NormalizerVersion {
			t.Fatalf("incomplete provenance: %+v", ev)
		}
		if len(ev.Flags) != 0 {
			t.Fatalf("clean feed produced a flagged event: %+v", ev)
		}
	}
	for _, k := range []string{"alpha/trade", "alpha/quote", "beta/trade", "beta/quote", "beta/depth"} {
		if !seen[k] {
			t.Errorf("no %s events came through", k)
		}
	}
}

func TestConnectionLossResumesWithoutGapOrDuplicate(t *testing.T) {
	e := harness.New(t)
	ing := e.StartIngestors()
	n, s := e.StartNormalizer(), e.StartSink()

	e.Sim.Step(200)
	e.WaitRaw(400)
	e.Sim.Alpha.Disconnect()
	e.Sim.Beta.Disconnect()
	e.Sim.Step(200) // published while the ingestors are reconnecting
	e.WaitRaw(800)
	e.Settle(n, s)

	wantInt(t, "distinct rows in the store", e.Stored(), 800)
	wantInt(t, "canonical events in the log", e.Canonical(), 800)
	wantInt(t, "gaps", len(e.Gaps()), 0)
	if ing.Alpha.Health().Reconnects == 0 || ing.Beta.Health().Reconnects == 0 {
		t.Errorf("reconnects not counted: alpha=%d beta=%d", ing.Alpha.Health().Reconnects, ing.Beta.Health().Reconnects)
	}
}

func TestWireLossIsRecoveredByAskingTheProviderAgain(t *testing.T) {
	e := harness.New(t)
	ing := e.StartIngestors()
	n, s := e.StartNormalizer(), e.StartSink()

	e.Sim.Step(50)
	e.Sim.Alpha.DropNext(3) // lost on the wire, still in the venue's resend buffer
	e.Sim.Beta.DropNext(3)
	e.Sim.Step(200)
	e.WaitRaw(500)
	e.Settle(n, s)

	wantInt(t, "distinct rows in the store", e.Stored(), 500)
	wantInt(t, "gaps", len(e.Gaps()), 0)
	if ing.Alpha.Health().Resyncs == 0 || ing.Beta.Health().Resyncs == 0 {
		t.Errorf("resyncs not counted: alpha=%d beta=%d", ing.Alpha.Health().Resyncs, ing.Beta.Health().Resyncs)
	}
}

func TestUnrecoverableLossIsDeclaredAsGap(t *testing.T) {
	e := harness.New(t)
	ing := e.StartIngestors()
	n, s := e.StartNormalizer(), e.StartSink()

	e.Sim.Step(50)
	e.Sim.Alpha.LoseNext(4) // gone for good: not sent, not in the resend buffer
	e.Sim.Beta.LoseNext(5)
	e.Sim.Step(300)
	e.WaitRaw(700 - 9)
	e.Settle(n, s)

	gaps := e.Gaps()
	if got := missing(gaps, "alpha"); got != 4 {
		t.Errorf("alpha missing sequence numbers = %d, want 4", got)
	}
	if got := missing(gaps, "beta"); got != 5 {
		t.Errorf("beta missing sequence numbers = %d, want 5", got)
	}
	for _, g := range gaps {
		switch g.Provider {
		case "alpha": // per-symbol sequence: the gap names its instrument
			if g.InstrumentID == "" || g.Venue != "venueA" {
				t.Errorf("alpha gap should be scoped to an instrument: %+v", g)
			}
		case "beta": // feed-wide sequence: any instrument of the venue may be affected
			if g.InstrumentID != "" || g.Venue != "venueB" || g.Missing() != 5 {
				t.Errorf("beta gap should be one feed-wide gap of 5: %+v", g)
			}
		}
	}
	wantInt(t, "distinct rows in the store", e.Stored(), 700-9)
	wantInt(t, "gap rows in the store", e.MemDB.Rows("gaps"), len(gaps))
	if ing.Beta.Health().Gaps != 1 {
		t.Errorf("beta ingest health gaps = %d, want 1", ing.Beta.Health().Gaps)
	}
	if got := testutil.ToFloat64(e.Stack.M.GapMissing.WithLabelValues("beta")); got != 5 {
		t.Errorf("mdp_gap_missing_events_total{beta} = %v, want 5", got)
	}
}

func TestDuplicateDeliveryIsDropped(t *testing.T) {
	e := harness.New(t)
	ing := e.StartIngestors()
	n, s := e.StartNormalizer(), e.StartSink()

	e.Sim.Alpha.DuplicateNext(10)
	e.Sim.Beta.DuplicateNext(10)
	e.Sim.Step(100)
	e.WaitRaw(200)
	e.Eventually("both ingestors to count 10 duplicates", func() bool {
		return ing.Alpha.Health().Duplicates == 10 && ing.Beta.Health().Duplicates == 10
	})
	e.Settle(n, s)

	wantInt(t, "raw records", len(e.Mem.All(stream.TopicRaw)), 200)
	wantInt(t, "canonical events in the log", e.Canonical(), 200)
	wantInt(t, "distinct rows in the store", e.Stored(), 200)
}

func TestMalformedAndOffTickInputIsDeadLettered(t *testing.T) {
	e := harness.New(t)
	e.StartIngestors()
	n, s := e.StartNormalizer(), e.StartSink()

	e.Sim.Alpha.GarbageNext(2)
	e.Sim.Beta.GarbageNext(1)
	e.Sim.Alpha.OffTickNext(1)
	e.Sim.Beta.OffTickNext(1)
	e.Sim.Step(100)
	e.WaitRaw(200)
	e.Settle(n, s)

	rej := e.Rejects()
	wantInt(t, "UNPARSEABLE_FRAME dead letters", rej["UNPARSEABLE_FRAME"], 3)
	wantInt(t, "PRICE_NOT_ON_TICK dead letters", rej["PRICE_NOT_ON_TICK"], 2)
	wantInt(t, "distinct rows in the store", e.Stored(), 198)
	// A rejected event still arrived, so it must not be mistaken for a gap.
	wantInt(t, "gaps", len(e.Gaps()), 0)
}

func TestClockSkewAndStaleEventsAreFlaggedNotRewritten(t *testing.T) {
	e := harness.New(t)
	e.StartIngestors()
	n, s := e.StartNormalizer(), e.StartSink()

	e.Sim.Beta.SetSkew(2 * time.Second) // venue clock runs 2s ahead
	e.Sim.Alpha.StaleNext(5, 30*time.Second)
	e.Sim.Step(40)
	e.WaitRaw(80)
	e.Settle(n, s)

	var skewed, stale int
	for _, ev := range canonical(e) {
		lead := ev.EventTime.Sub(ev.ReceiveTime)
		switch ev.Provider {
		case "beta":
			if !has(ev.Flags, domain.FlagClockSkew) {
				t.Errorf("beta event not flagged clock_skew: %+v", ev)
			}
			// The venue timestamp is kept as supplied, ahead of our clock.
			if lead < 1900*time.Millisecond || lead > 2100*time.Millisecond {
				t.Errorf("event_time should lead receive_time by ~2s, got %s", lead)
			}
			skewed++
		case "alpha":
			if has(ev.Flags, domain.FlagStale) {
				if -lead < 29*time.Second {
					t.Errorf("stale event's age %s should be ~30s", -lead)
				}
				stale++
			}
			if has(ev.Flags, domain.FlagClockSkew) {
				t.Errorf("alpha event wrongly flagged clock_skew: %+v", ev)
			}
		}
	}
	wantInt(t, "beta events flagged clock_skew", skewed, 40)
	wantInt(t, "alpha events flagged stale", stale, 5)
	if got := testutil.ToFloat64(e.Stack.M.Flagged.WithLabelValues(domain.FlagStale)); got != 5 {
		t.Errorf("mdp_flagged_events_total{stale} = %v, want 5", got)
	}
}

func TestSessionResetRestartsSequenceTracking(t *testing.T) {
	e := harness.New(t)
	e.StartIngestors()
	n, s := e.StartNormalizer(), e.StartSink()

	e.Sim.Step(50)
	e.WaitRaw(100)
	e.Sim.Alpha.NewSession("A-next") // sequence numbers restart at 1
	e.Sim.Beta.NewSession("B-next")
	e.WaitConnected()
	e.Sim.Step(50)
	e.WaitRaw(200)
	e.Settle(n, s)

	// Low numbers of the new session must not be dropped as duplicates of the old one.
	wantInt(t, "distinct rows in the store", e.Stored(), 200)
	wantInt(t, "gaps", len(e.Gaps()), 0)
	if got := testutil.ToFloat64(e.Stack.M.SessionResets.WithLabelValues("beta")); got != 1 {
		t.Errorf("mdp_session_resets_total{beta} = %v, want 1", got)
	}
}

func TestSessionStartedWhileIngestorsWereDownIsRecovered(t *testing.T) {
	e := harness.New(t)
	ing := e.StartIngestors()
	n, s := e.StartNormalizer(), e.StartSink()
	e.Sim.Step(20)
	e.WaitRaw(40)

	ing.Stop()
	e.Sim.Alpha.NewSession("A-next")
	e.Sim.Beta.NewSession("B-next")
	e.Sim.Step(10) // the new session's first events: nobody is connected
	ing = e.StartIngestors()
	e.Sim.Step(100)
	e.WaitRaw(260)
	e.Settle(n, s)

	// The first event seen is not number 1, so the ingestors ask for the
	// session from its start and the venues still have it.
	wantInt(t, "distinct rows in the store", e.Stored(), 260)
	wantInt(t, "gaps", len(e.Gaps()), 0)
	if ing.Beta.Health().Resyncs == 0 {
		t.Error("beta ingestor should have asked for a resend")
	}
}

func TestMissedStartOfSessionIsDeclaredAsGap(t *testing.T) {
	e := harness.New(t)
	e.StartIngestors()
	n, s := e.StartNormalizer(), e.StartSink()
	e.Sim.Step(20)
	e.WaitRaw(40)

	e.Sim.Alpha.NewSession("A-next")
	e.Sim.Beta.NewSession("B-next")
	e.WaitConnected()
	e.Sim.Alpha.LoseNext(4) // the new session's first numbers never arrive
	e.Sim.Beta.LoseNext(5)
	e.Sim.Step(100)
	e.WaitRaw(240 - 9)
	e.Settle(n, s)

	gaps := e.Gaps()
	if got := missing(gaps, "beta"); got != 5 {
		t.Errorf("beta missing sequence numbers = %d, want 5 (gaps: %+v)", got, gaps)
	}
	if got := missing(gaps, "alpha"); got != 4 {
		t.Errorf("alpha missing sequence numbers = %d, want 4 (gaps: %+v)", got, gaps)
	}
	for _, g := range gaps {
		if g.FromSeq != 1 || g.Session == "" {
			t.Errorf("a missed session start begins at sequence 1 of the new session: %+v", g)
		}
	}
	wantInt(t, "distinct rows in the store", e.Stored(), 240-9)
}

// checkpointFault fails checkpoint writes whose key has the prefix,
// simulating a process that dies before it can record its progress.
func checkpointFault(prefix string) func([]stream.Record) error {
	return func(recs []stream.Record) error {
		for _, r := range recs {
			if r.Topic == stream.TopicCheckpoints && bytes.HasPrefix(r.Key, []byte(prefix)) {
				return errors.New("injected: checkpoint write lost")
			}
		}
		return nil
	}
}

func feedOffset(e *harness.Env, key string) (total uint64) {
	all, err := stream.LatestByKey(e.Ctx, e.Bus, stream.TopicCheckpoints)
	if err != nil {
		e.T.Fatal(err)
	}
	var offs []domain.FeedOffset
	json.Unmarshal(all[key], &offs)
	for _, o := range offs {
		total += o.Seq
	}
	return total
}

func TestIngestorRestartResumesFromCheckpoint(t *testing.T) {
	e := harness.New(t)
	ing := e.StartIngestors()
	n, s := e.StartNormalizer(), e.StartSink()

	e.Sim.Step(100)
	e.WaitRaw(200)
	ing.Stop() // clean shutdown writes a final checkpoint
	if got := feedOffset(e, "feed/beta/live"); got != 100 {
		t.Fatalf("beta FeedOffset after shutdown = %d, want 100", got)
	}
	e.Sim.Step(100) // the venues keep publishing while the ingestors are down
	e.StartIngestors()
	e.WaitRaw(400)
	e.Settle(n, s)

	wantInt(t, "raw records (nothing republished)", len(e.Mem.All(stream.TopicRaw)), 400)
	wantInt(t, "distinct rows in the store", e.Stored(), 400)
	wantInt(t, "gaps", len(e.Gaps()), 0)
}

func TestIngestorCrashRepublishesAndNormalizerDropsTheRepeats(t *testing.T) {
	e := harness.New(t)
	ing := e.StartIngestors()
	n, s := e.StartNormalizer(), e.StartSink()

	e.Sim.Step(50)
	e.WaitRaw(100)
	e.Eventually("first 50 events of each feed to be checkpointed", func() bool {
		return feedOffset(e, "feed/alpha/live") == 50 && feedOffset(e, "feed/beta/live") == 50
	})
	// From here the ingestors publish but can no longer checkpoint, as if
	// they were killed before the next checkpoint.
	e.Mem.SetProduceHook(checkpointFault("feed/"))
	e.Sim.Step(50)
	e.WaitRaw(200)
	ing.Stop()
	e.Mem.SetProduceHook(nil)

	e.StartIngestors() // resumes from sequence 50 and receives 51..100 again
	e.Sim.Step(50)
	e.WaitRaw(300)
	e.Settle(n, s)

	if raw := len(e.Mem.All(stream.TopicRaw)); raw != 400 {
		t.Errorf("raw records = %d, want 400 (300 events + 100 republished)", raw)
	}
	dups := testutil.ToFloat64(e.Stack.M.Duplicates.WithLabelValues("alpha")) +
		testutil.ToFloat64(e.Stack.M.Duplicates.WithLabelValues("beta"))
	if dups != 100 {
		t.Errorf("normalizer dropped %v redelivered events, want 100", dups)
	}
	wantInt(t, "canonical events in the log", e.Canonical(), 300)
	wantInt(t, "distinct rows in the store", e.Stored(), 300)
	wantInt(t, "gaps", len(e.Gaps()), 0)
}

// rawTrade builds a beta trade as an ingestor would publish it.
func rawTrade(e *harness.Env, seq uint64) stream.Record {
	at := time.Now().UTC()
	ev := ingestors.Event{
		Provider: "beta", Session: "S", Domain: ingestors.BetaDomain, Seq: seq, Kind: domain.KindTrade,
		Symbol: "XYZ.B", Price: "75.015", Qty: "10", TradeID: "m" + string(rune('a'+seq)), Side: "buy",
		EventTime: at, RecvTime: at, Source: domain.SourceLive,
	}
	b, _ := json.Marshal(ev)
	return stream.Keyed(stream.TopicRaw, e.Stack.Opt.Layout.Partitions, ev.TrackKey(), b)
}

func publishRaw(e *harness.Env, seqs ...uint64) {
	for _, seq := range seqs {
		if err := e.Bus.Produce(e.Ctx, rawTrade(e, seq)); err != nil {
			e.T.Fatal(err)
		}
	}
}

func TestGapSpanningANormalizerRestartIsStillDetected(t *testing.T) {
	e := harness.New(t)
	s := e.StartSink()

	publishRaw(e, 1, 2, 3)
	n := e.StartNormalizer()
	e.Settle(n, s)
	n.Stop()

	publishRaw(e, 5, 6) // 4 never arrives; the normalizer that saw 3 is gone
	n = e.StartNormalizer()
	e.Settle(n, s)

	gaps := e.Gaps()
	if len(gaps) != 1 || gaps[0].FromSeq != 4 || gaps[0].ToSeq != 4 {
		t.Fatalf("want exactly one gap [4,4], got %+v", gaps)
	}
	wantInt(t, "trades stored", e.MemDB.Rows("trades"), 5)
}

func TestRedeliveredRawEventsAfterRestartAreDropped(t *testing.T) {
	e := harness.New(t)
	s := e.StartSink()

	publishRaw(e, 1, 2, 3, 4, 5)
	n := e.StartNormalizer()
	e.Settle(n, s)
	n.Stop()

	publishRaw(e, 4, 5, 6) // 4 and 5 again, as after an ingestor crash
	n = e.StartNormalizer()
	e.Settle(n, s)

	wantInt(t, "canonical trades in the log", len(e.Mem.All(stream.TopicTrades)), 6)
	wantInt(t, "trades stored", e.MemDB.Rows("trades"), 6)
	wantInt(t, "gaps", len(e.Gaps()), 0)
}

func TestNormalizerCrashReprocessesWithoutDuplicatingFacts(t *testing.T) {
	e := harness.New(t)
	s := e.StartSink()

	// The normalizer publishes its output but dies before checkpointing.
	e.Mem.SetProduceHook(checkpointFault("normalizer/"))
	publishRaw(e, 1, 2, 3, 4, 5, 6, 7, 8, 9, 10)
	n := e.StartNormalizer()
	e.Settle(n, s)
	n.Stop()
	e.Mem.SetProduceHook(nil)

	n = e.StartNormalizer() // starts over from the beginning of the raw topic
	e.Settle(n, s)

	// At-least-once: the log holds each event twice...
	wantInt(t, "canonical trades in the log", len(e.Mem.All(stream.TopicTrades)), 20)
	wantInt(t, "rows written to the store", e.MemDB.Writes("trades"), 20)
	// ...and the store holds each fact once.
	wantInt(t, "trades stored", e.MemDB.Rows("trades"), 10)
	wantInt(t, "gaps", len(e.Gaps()), 0)
}

func TestSinkCrashRedeliversIdempotently(t *testing.T) {
	e := harness.New(t)
	e.StartIngestors()
	n, s := e.StartNormalizer(), e.StartSink()

	// The sink writes to the store but its offset commits are lost.
	e.Mem.SetCommitHook(func(group string) error {
		if group == storage.SinkGroup {
			return errors.New("injected: commit lost")
		}
		return nil
	})
	e.Sim.Step(100)
	e.WaitRaw(200)
	e.Settle(n, s)
	wantInt(t, "distinct rows before the restart", e.Stored(), 200)
	s.Stop()
	e.Mem.SetCommitHook(nil)
	if len(e.Mem.Committed(storage.SinkGroup)) != 0 {
		t.Fatal("test setup: sink offsets should not have been committed")
	}

	s = e.StartSink() // redelivers everything
	e.Settle(n, s)

	writes := e.MemDB.Writes("trades") + e.MemDB.Writes("quotes") + e.MemDB.Writes("depth")
	wantInt(t, "rows written to the store", writes, 400)
	wantInt(t, "distinct rows after redelivery", e.Stored(), 200)
}

func sinkLag(e *harness.Env, s *harness.Sink) (total int64) {
	lags, err := stream.Lag(e.Ctx, e.Bus, s.Progress())
	if err != nil {
		e.T.Fatal(err)
	}
	for _, l := range lags {
		total += l
	}
	return total
}

func TestStoreOutageBecomesLagThenDrainsWithoutLoss(t *testing.T) {
	e := harness.New(t)
	e.StartIngestors()
	n, s := e.StartNormalizer(), e.StartSink()
	e.Sim.Step(50)
	e.WaitRaw(100)
	e.Settle(n, s)

	e.MemDB.SetFault(func(string, int) error { return errors.New("injected: store unavailable") })
	e.Sim.Step(200)
	e.WaitRaw(500)
	e.Settle(n, nil) // the rest of the pipeline keeps flowing

	// The backlog is in the log, measurable, and not in the store.
	if lag := sinkLag(e, s); lag != 400 {
		t.Errorf("sink lag during the outage = %d records, want 400", lag)
	}
	e.Eventually("mdp_sink_retrying to report the outage", func() bool {
		return testutil.ToFloat64(e.Stack.M.SinkRetrying) == 1
	})
	e.Eventually("mdp_consumer_lag_records{sink} to report the backlog", func() bool {
		var total float64
		for p := 0; p < int(e.Stack.Opt.Layout.Partitions); p++ {
			for _, topic := range stream.CanonicalTopics {
				total += testutil.ToFloat64(e.Stack.M.ConsumerLag.WithLabelValues("sink", topic, strconv.Itoa(p)))
			}
		}
		return total == 400
	})
	wantInt(t, "rows stored during the outage", e.Stored(), 100)

	e.MemDB.SetFault(nil)
	e.Settle(n, s)
	wantInt(t, "distinct rows after recovery", e.Stored(), 500)
	wantInt(t, "dead letters", len(e.Rejects()), 0)
	if lag := sinkLag(e, s); lag != 0 {
		t.Errorf("sink lag after recovery = %d, want 0", lag)
	}
}

func TestRejectedWriteIsDeadLetteredWithoutBlockingTheRest(t *testing.T) {
	e := harness.New(t)
	e.StartIngestors()
	n, s := e.StartNormalizer(), e.StartSink()

	// The store permanently refuses one particular trade, like a row that
	// violates the schema. The batch containing it fails as a whole.
	var poison string
	e.MemDB.SetRowFault(func(table, key string) error {
		if table != "trades" {
			return nil
		}
		if poison == "" {
			poison = key
		}
		if key == poison {
			return &storage.PermanentError{Err: errors.New("injected: row refused")}
		}
		return nil
	})
	e.Sim.Step(200)
	e.WaitRaw(400)
	e.Settle(n, s)

	wantInt(t, "STORE_REJECTED dead letters", e.Rejects()["STORE_REJECTED"], 1)
	wantInt(t, "distinct rows in the store", e.Stored(), 399)
	if got := testutil.ToFloat64(e.Stack.M.SinkRejectedRows); got != 1 {
		t.Errorf("mdp_sink_rejected_rows_total = %v, want 1", got)
	}
}

func TestBrokerOutageBlocksIngestThenRecoversWithoutLoss(t *testing.T) {
	e := harness.New(t)
	e.StartIngestors()
	n, s := e.StartNormalizer(), e.StartSink()
	e.Sim.Step(50)
	e.WaitRaw(100)

	e.Mem.SetDown(true)
	e.Sim.Step(150) // nothing can be published; the ingestors hold and retry
	time.Sleep(100 * time.Millisecond)
	wantInt(t, "raw records during the outage", len(e.Mem.All(stream.TopicRaw)), 100)
	e.Mem.SetDown(false)

	e.WaitRaw(400)
	e.Settle(n, s)
	wantInt(t, "distinct rows in the store", e.Stored(), 400)
	wantInt(t, "gaps", len(e.Gaps()), 0)
}

func TestEventsOlderThanRetentionAreRefusedVisibly(t *testing.T) {
	e := harness.New(t)
	day := 24 * time.Hour
	e.MemDB.SetRetention(storage.Retention{Trades: 90 * day, Quotes: 90 * day, Depth: 90 * day, Gaps: 90 * day})
	e.StartIngestors()
	n, s := e.StartNormalizer(), e.StartSink()

	e.Sim.Alpha.StaleNext(3, 91*day) // event times beyond the retention horizon
	e.Sim.Step(50)
	e.WaitRaw(100)
	e.Settle(n, s)

	wantInt(t, "OLDER_THAN_RETENTION dead letters", e.Rejects()["OLDER_THAN_RETENTION"], 3)
	wantInt(t, "distinct rows in the store", e.Stored(), 97)
}

func TestReplayedCaptureIntoALivePipelineIsLabelledAndIdempotent(t *testing.T) {
	e := harness.New(t)
	century := 100 * 365 * 24 * time.Hour
	e.MemDB.SetRetention(storage.Retention{Trades: century, Quotes: century, Depth: century, Gaps: century})
	n, s := e.StartNormalizer(), e.StartSink()
	capture := harness.Root() + "/examples/fixtures/session.ndjson"

	replay := func() {
		for _, p := range []string{"alpha", "beta"} {
			a, err := ingestors.NewReplay(capture, p, 0)
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(e.Ctx, 30*time.Second)
			if err := e.Stack.NewIngestor(a).Run(ctx); err != nil {
				t.Fatal(err)
			}
			cancel()
		}
		e.Settle(n, s)
	}
	replay()
	rows, raw := e.Stored(), len(e.Mem.All(stream.TopicRaw))
	wantInt(t, "distinct rows after the first replay", rows, 7987)
	for _, ev := range canonical(e) {
		if ev.Source != domain.SourceReplay {
			t.Fatalf("replayed event not labelled as replay: %+v", ev)
		}
	}

	replay() // the same capture again: its feed offsets are already checkpointed
	wantInt(t, "raw records after the second replay", len(e.Mem.All(stream.TopicRaw)), raw)
	wantInt(t, "distinct rows after the second replay", e.Stored(), rows)
}

func TestQuietFeedLeavesNoUnsavedProgress(t *testing.T) {
	e := harness.New(t)
	e.StartIngestors()
	n, s := e.StartNormalizer(), e.StartSink()
	e.Sim.Step(100)
	e.WaitRaw(200)
	e.Settle(n, s)

	// Nothing more arrives. The committed offsets of both consumers must
	// still reach the end of their topics, or the tail would be reprocessed
	// after a restart and external lag monitoring would never read zero.
	for group, topics := range map[string][]string{normalizer.Group: {stream.TopicRaw}, storage.SinkGroup: stream.CanonicalTopics} {
		e.Eventually("the committed offsets of "+group+" to reach the end of the log", func() bool {
			ends, err := e.Bus.EndOffsets(e.Ctx, topics...)
			if err != nil {
				t.Fatal(err)
			}
			committed := e.Mem.Committed(group)
			for tp, end := range ends {
				if end > 0 && committed[tp] != end {
					return false
				}
			}
			return true
		})
	}

	// A restarted normalizer therefore has nothing to redo.
	n.Stop()
	before := e.Canonical()
	n = e.StartNormalizer()
	e.Settle(n, s)
	wantInt(t, "canonical events republished after a restart on a quiet feed", e.Canonical()-before, 0)
}

func TestJoiningASessionLateRecoversItsStartFromTheVenue(t *testing.T) {
	e := harness.New(t)
	// The venues have been publishing for a while before the platform exists.
	e.Sim.Step(150)
	ing := e.StartIngestors()
	n, s := e.StartNormalizer(), e.StartSink()
	e.Sim.Step(50)
	e.WaitRaw(400)
	e.Settle(n, s)

	wantInt(t, "distinct rows in the store", e.Stored(), 400)
	wantInt(t, "gaps", len(e.Gaps()), 0)
	if a, b := ing.Alpha.Health().Resyncs, ing.Beta.Health().Resyncs; a == 0 || b == 0 {
		t.Errorf("both ingestors should have asked their venue for the start of the session: alpha=%d beta=%d", a, b)
	}
	// Every sequence domain is complete from its first number.
	first := map[string]uint64{}
	for _, ev := range canonical(e) {
		key := ev.Provider + "/" + ev.InstrumentID
		if ev.Provider == "beta" {
			key = "beta"
		}
		if cur, ok := first[key]; !ok || ev.Seq < cur {
			first[key] = ev.Seq
		}
	}
	for key, seq := range first {
		if seq != 1 {
			t.Errorf("%s: first stored sequence number is %d, want 1", key, seq)
		}
	}
}

func TestJoiningASessionLateDeclaresWhatTheVenueCannotResend(t *testing.T) {
	e := harness.New(t)
	e.Sim.Beta.LoseNext(10) // gone for good before anyone was listening
	e.Sim.Step(10)
	ing := e.StartIngestors()
	n, s := e.StartNormalizer(), e.StartSink()
	e.Sim.Step(20)
	e.WaitRaw(30 + 20) // all of alpha, and beta from its 11th event
	e.Settle(n, s)

	gaps := e.Gaps()
	if len(gaps) != 1 || gaps[0].Provider != "beta" || gaps[0].FromSeq != 1 || gaps[0].ToSeq != 10 {
		t.Fatalf("gaps %+v, want beta 1..10: the store must not look complete from the start of the session", gaps)
	}
	wantInt(t, "distinct rows in the store", e.Stored(), 50)
	wantInt(t, "gap rows in the store", e.MemDB.Rows("gaps"), 1)
	if got := testutil.ToFloat64(e.Stack.M.GapMissing.WithLabelValues("beta")); got != 10 {
		t.Errorf("mdp_gap_missing_events_total{provider=beta} = %v, want 10", got)
	}
	if h := ing.Beta.Health(); h.Gaps != 1 || h.Resyncs != 1 {
		t.Errorf("beta ingest health: gaps=%d resyncs=%d, want 1 and 1 (asked once, then accepted)", h.Gaps, h.Resyncs)
	}

	// A restart must not ask again or declare it again.
	ing.Stop()
	n.Stop()
	e.Sim.Step(5)
	e.StartIngestors()
	n = e.StartNormalizer()
	e.WaitRaw(60)
	e.Settle(n, s)
	wantInt(t, "gaps after a restart", len(e.Gaps()), 1)
	wantInt(t, "distinct rows after a restart", e.Stored(), 60)
}
