package unit

import (
	"encoding/json"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"mdp/src/domain"
	"mdp/src/gateway"
	"mdp/src/metrics"
	"mdp/src/stream"
	"mdp/tests/harness"
)

// clock is a settable time source.
type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) Now() time.Time      { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *clock) Add(d time.Duration) { c.mu.Lock(); c.t = c.t.Add(d); c.mu.Unlock() }

func newHub(t *testing.T, queue int) (*gateway.Hub, *metrics.Set, *clock) {
	t.Helper()
	clk := &clock{t: time.Date(2026, 1, 5, 9, 30, 0, 0, time.UTC)}
	m := metrics.New()
	hub := gateway.NewHub(stream.NewMemBus(), harness.Registry(t), m,
		gateway.HubConfig{QueueSize: queue, StaleAfter: 3 * time.Second, Now: clk.Now}, harness.Logger())
	return hub, m, clk
}

func tradeRec(instrument, session string, seq uint64) stream.Record {
	at := time.Date(2026, 1, 5, 9, 30, 0, int(seq), time.UTC)
	tr := domain.Trade{Type: domain.KindTrade, InstrumentID: instrument, TradeID: fmt.Sprint("t", seq), PriceTicks: 100, Quantity: 1,
		EventTime: at, ReceiveTime: at.Add(time.Millisecond),
		Meta: domain.Meta{Provider: "alpha", Session: session, SeqDomain: "XYZ", Seq: seq, Source: domain.SourceLive, NormalizerVersion: 1}}
	return stream.Record{Topic: stream.TopicTrades, Key: []byte(instrument), Value: domain.Encode(tr)}
}

func quoteRec(instrument string, seq uint64) stream.Record {
	at := time.Date(2026, 1, 5, 9, 30, 0, int(seq), time.UTC)
	q := domain.Quote{Type: domain.KindQuote, InstrumentID: instrument, BidTicks: 99, BidSize: 1, AskTicks: 101, AskSize: 1,
		EventTime: at, ReceiveTime: at, Meta: domain.Meta{Provider: "alpha", Session: "S1", Seq: seq, Source: domain.SourceLive}}
	return stream.Record{Topic: stream.TopicQuotes, Key: []byte(instrument), Value: domain.Encode(q)}
}

// drainSub returns what is queued for a subscriber right now.
func drainSub(s *gateway.Subscription) []map[string]any {
	var out []map[string]any
	for {
		select {
		case b := <-s.Out():
			var m map[string]any
			json.Unmarshal(b, &m)
			out = append(out, m)
		default:
			return out
		}
	}
}

func seqs(msgs []map[string]any) string {
	s := ""
	for _, m := range msgs {
		s += fmt.Sprint(m["seq"], " ")
	}
	return s
}

func TestHubRoutesByInstrumentAndChannel(t *testing.T) {
	hub, m, _ := newHub(t, 16)
	xyzTrades := hub.Subscribe([]string{"venueA:XYZ"}, []string{domain.KindTrade})
	xyzAll := hub.Subscribe([]string{"venueA:XYZ"}, []string{domain.KindTrade, domain.KindQuote})
	abc := hub.Subscribe([]string{"venueA:ABC"}, []string{domain.KindTrade, domain.KindQuote})
	if testutil.ToFloat64(m.WSClients) != 3 {
		t.Errorf("mdp_ws_clients = %v", testutil.ToFloat64(m.WSClients))
	}

	hub.Dispatch(tradeRec("venueA:XYZ", "S1", 1))
	hub.Dispatch(quoteRec("venueA:XYZ", 2))
	hub.Dispatch(tradeRec("venueA:ABC", "S1", 1))

	if got := drainSub(xyzTrades); len(got) != 1 || got[0]["type"] != "trade" || got[0]["instrument_id"] != "venueA:XYZ" {
		t.Errorf("trade-only subscriber got %v", got)
	}
	if got := drainSub(xyzAll); len(got) != 2 || got[1]["type"] != "quote" {
		t.Errorf("trade+quote subscriber got %v", got)
	}
	if got := drainSub(abc); len(got) != 1 || got[0]["instrument_id"] != "venueA:ABC" {
		t.Errorf("ABC subscriber got %v", got)
	}

	abc.Close()
	hub.Dispatch(tradeRec("venueA:ABC", "S1", 2))
	if got := drainSub(abc); len(got) != 0 {
		t.Errorf("a closed subscription still received %v", got)
	}
	if testutil.ToFloat64(m.WSClients) != 2 {
		t.Errorf("mdp_ws_clients after close = %v", testutil.ToFloat64(m.WSClients))
	}
	// Undecodable records are not delivered and do not disturb the hub.
	hub.Dispatch(stream.Record{Topic: stream.TopicTrades, Value: []byte("{not json")})
	hub.Dispatch(stream.Record{Topic: stream.TopicGaps, Value: []byte("{not json")})
}

func TestSlowSubscriberIsToldWhatItMissed(t *testing.T) {
	hub, m, _ := newHub(t, 3)
	slow := hub.Subscribe([]string{"venueA:XYZ", "venueA:ABC"}, []string{domain.KindTrade})
	fast := hub.Subscribe([]string{"venueA:XYZ"}, []string{domain.KindTrade})

	for seq := uint64(1); seq <= 10; seq++ {
		hub.Dispatch(tradeRec("venueA:XYZ", "S1", seq))
		drainSub(fast) // the fast subscriber keeps up
	}
	hub.Dispatch(tradeRec("venueA:ABC", "S1", 1))

	// The queue holds the first three; the rest were not delivered to this subscriber.
	if got := seqs(drainSub(slow)); got != "1 2 3 " {
		t.Errorf("slow subscriber's queue holds %q, want the first three", got)
	}
	notices := slow.TakeDrops()
	want := []gateway.DropNotice{
		{Type: "gap", Reason: "slow_consumer", InstrumentID: "venueA:ABC", Dropped: 1},
		{Type: "gap", Reason: "slow_consumer", InstrumentID: "venueA:XYZ", Dropped: 7},
	}
	if len(notices) != 2 || notices[0] != want[0] || notices[1] != want[1] {
		t.Errorf("drop notices %+v, want %+v", notices, want)
	}
	if again := slow.TakeDrops(); again != nil {
		t.Errorf("drops are reported once: %+v", again)
	}
	// The running total stays visible in every status message.
	for _, is := range slow.Status().Instruments {
		if w := map[string]uint64{"venueA:XYZ": 7, "venueA:ABC": 1}[is.InstrumentID]; is.Dropped != w {
			t.Errorf("status dropped for %s = %d, want %d", is.InstrumentID, is.Dropped, w)
		}
	}
	if got := testutil.ToFloat64(m.WSDropped); got != 8 {
		t.Errorf("mdp_ws_dropped_total = %v, want 8", got)
	}
	if len(fast.TakeDrops()) != 0 || fast.Status().Instruments[0].Dropped != 0 {
		t.Error("one slow subscriber must not cost another subscriber anything")
	}

	// Once it reads again, delivery resumes and new drops are counted afresh.
	hub.Dispatch(tradeRec("venueA:XYZ", "S1", 11))
	if got := seqs(drainSub(slow)); got != "11 " {
		t.Errorf("after draining, got %q", got)
	}
}

func TestHubDoesNotForwardRedeliveredEvents(t *testing.T) {
	hub, m, _ := newHub(t, 64)
	sub := hub.Subscribe([]string{"venueA:XYZ"}, []string{domain.KindTrade, domain.KindQuote})
	for _, seq := range []uint64{1, 2, 3} {
		hub.Dispatch(tradeRec("venueA:XYZ", "S1", seq))
	}
	hub.Dispatch(quoteRec("venueA:XYZ", 2)) // quotes are sequenced on their own topic
	for _, seq := range []uint64{2, 3, 4} { // a restarted normalizer repeats 2 and 3
		hub.Dispatch(tradeRec("venueA:XYZ", "S1", seq))
	}
	hub.Dispatch(tradeRec("venueA:XYZ", "S2", 1)) // a new session restarts numbering
	if got := seqs(drainSub(sub)); got != "1 2 3 2 4 1 " {
		t.Errorf("delivered %q, want trades 1 2 3, quote 2, trade 4, then 1 of the new session", got)
	}
	if got := testutil.ToFloat64(m.WSDuplicates); got != 2 {
		t.Errorf("mdp_ws_duplicates_suppressed_total = %v, want 2", got)
	}
}

func gapRec(g domain.Gap) stream.Record {
	g.Type = domain.KindGap
	return stream.Record{Topic: stream.TopicGaps, Value: domain.Encode(g)}
}

func TestFeedGapsReachEveryAffectedSubscriberOnce(t *testing.T) {
	hub, _, _ := newHub(t, 16)
	twoB := hub.Subscribe([]string{"venueB:XYZ.B", "venueB:ABC.B"}, []string{domain.KindTrade})
	oneA := hub.Subscribe([]string{"venueA:XYZ"}, []string{domain.KindTrade})
	otherA := hub.Subscribe([]string{"venueA:ABC"}, []string{domain.KindTrade})

	// Feed-wide: every instrument of the venue may be affected.
	hub.Dispatch(gapRec(domain.Gap{Provider: "beta", Venue: "venueB", Session: "B7", SeqDomain: "main", FromSeq: 21, ToSeq: 25}))
	got := drainSub(twoB)
	if len(got) != 1 || got[0]["type"] != "gap" || got[0]["reason"] != "feed_gap" || got[0]["missing"] != float64(5) ||
		got[0]["from_seq"] != float64(21) || got[0]["venue"] != "venueB" {
		t.Errorf("subscriber of two venueB instruments got %v, want one feed gap notice", got)
	}
	if len(drainSub(oneA)) != 0 {
		t.Error("a venueB gap was sent to a venueA subscriber")
	}
	for _, is := range hub.Freshness([]string{"venueB:XYZ.B", "venueB:ABC.B", "venueB:LMN.B", "venueA:XYZ"}).Instruments {
		want := uint64(1)
		if is.InstrumentID == "venueA:XYZ" {
			want = 0
		}
		if is.FeedGaps != want {
			t.Errorf("feed_gaps for %s = %d, want %d", is.InstrumentID, is.FeedGaps, want)
		}
	}

	// Scoped to one instrument: only its subscribers hear of it.
	hub.Dispatch(gapRec(domain.Gap{Provider: "alpha", Venue: "venueA", InstrumentID: "venueA:XYZ", Session: "A1", SeqDomain: "XYZ", FromSeq: 3, ToSeq: 3}))
	if got := drainSub(oneA); len(got) != 1 || got[0]["instrument_id"] != "venueA:XYZ" || got[0]["missing"] != float64(1) {
		t.Errorf("XYZ subscriber got %v", got)
	}
	if len(drainSub(otherA)) != 0 || len(drainSub(twoB)) != 0 {
		t.Error("an instrument-scoped gap leaked to other subscribers")
	}
}

func TestFreshnessIsMeasuredOnTheGatewayClock(t *testing.T) {
	hub, _, clk := newHub(t, 16)
	const id = "venueA:XYZ"
	st := hub.Freshness([]string{id})
	if is := st.Instruments[0]; !is.Stale || is.AgeMs != nil || is.LastEventTime != nil || st.StaleAfterMs != 3000 || st.Type != "status" {
		t.Errorf("an instrument never seen is stale with no age: %+v", is)
	}

	rec := tradeRec(id, "S1", 1)
	hub.Dispatch(rec)
	clk.Add(1200 * time.Millisecond)
	is := hub.Freshness([]string{id}).Instruments[0]
	if is.Stale || is.AgeMs == nil || *is.AgeMs != 1200 {
		t.Errorf("1.2s after an event: %+v", is)
	}
	// Both source clocks are reported as they came, separately from the age.
	if is.LastEventTime == nil || is.LastReceiveTime == nil || !is.LastReceiveTime.Equal(is.LastEventTime.Add(time.Millisecond)) {
		t.Errorf("last event/receive times: %v %v", is.LastEventTime, is.LastReceiveTime)
	}

	clk.Add(1800 * time.Millisecond) // exactly the threshold
	if hub.Freshness([]string{id}).Instruments[0].Stale {
		t.Error("at exactly stale_after the instrument is not yet stale")
	}
	clk.Add(time.Millisecond)
	if is := hub.Freshness([]string{id}).Instruments[0]; !is.Stale || *is.AgeMs != 3001 {
		t.Errorf("past stale_after: %+v", is)
	}
	hub.Dispatch(tradeRec(id, "S1", 2))
	if hub.Freshness([]string{id}).Instruments[0].Stale {
		t.Error("a new event makes the instrument fresh again")
	}
}
