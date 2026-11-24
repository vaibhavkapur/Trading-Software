package unit

import (
	"reflect"
	"testing"
	"time"

	"mdp/src/domain"
	"mdp/src/ingestors"
	"mdp/src/normalizer"
	"mdp/tests/harness"
)

var (
	evTime = time.Date(2026, 1, 5, 9, 30, 0, 123456789, time.UTC)
	rcTime = evTime.Add(400 * time.Microsecond)
)

func norm(t *testing.T) *normalizer.Normalizer {
	return normalizer.New(harness.Registry(t), normalizer.DefaultOptions())
}

func alphaTrade() ingestors.Event {
	return ingestors.Event{Provider: "alpha", Session: "A1", Domain: "XYZ", Seq: 42, Kind: domain.KindTrade, Symbol: "XYZ",
		Price: "101.25", Qty: "100", TradeID: "A-7781", Side: "buy", EventTime: evTime, RecvTime: rcTime, Source: domain.SourceLive}
}

func alphaQuote() ingestors.Event {
	return ingestors.Event{Provider: "alpha", Session: "A1", Domain: "XYZ", Seq: 43, Kind: domain.KindQuote, Symbol: "XYZ",
		Bid: "101.24", BidQty: "300", Ask: "101.26", AskQty: "200", EventTime: evTime, RecvTime: rcTime, Source: domain.SourceLive}
}

func betaDepth() ingestors.Event {
	return ingestors.Event{Provider: "beta", Session: "B7", Domain: "main", Seq: 20, Kind: domain.KindDepth, Symbol: "ABC.B",
		Bids: [][2]string{{"10.01", "50"}, {"10", "70"}}, Asks: [][2]string{{"10.02", "10"}, {"10.03", "20"}},
		EventTime: evTime, RecvTime: rcTime, Source: domain.SourceLive}
}

func TestNormalizeTrade(t *testing.T) {
	got, rej := norm(t).Normalize(alphaTrade())
	if rej != nil {
		t.Fatal(rej)
	}
	want := domain.Trade{
		Type: "trade", InstrumentID: "venueA:XYZ", TradeID: "A-7781", PriceTicks: 10125, Quantity: 100, Aggressor: "buy",
		EventTime: evTime, ReceiveTime: rcTime,
		Meta: domain.Meta{Provider: "alpha", Session: "A1", SeqDomain: "XYZ", Seq: 42, Source: "live", NormalizerVersion: domain.NormalizerVersion},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("\n got %+v\nwant %+v", got, want)
	}
}

func TestNormalizeUnifiesTradeSideAcrossProviders(t *testing.T) {
	// beta reports the resting order's side, so a resting sell was hit by a buyer.
	ev := ingestors.Event{Provider: "beta", Session: "B7", Domain: "main", Seq: 17, Kind: domain.KindTrade, Symbol: "XYZ.B",
		Price: "75.015", Qty: "100", TradeID: "991", Side: "sell", EventTime: evTime, RecvTime: rcTime, Source: domain.SourceLive}
	got, rej := norm(t).Normalize(ev)
	if rej != nil {
		t.Fatal(rej)
	}
	tr := got.(domain.Trade)
	if tr.Aggressor != "buy" || tr.InstrumentID != "venueB:XYZ.B" || tr.PriceTicks != 15003 {
		t.Errorf("got aggressor %q, instrument %q, %d ticks; want buy, venueB:XYZ.B, 15003 (tick 0.005)", tr.Aggressor, tr.InstrumentID, tr.PriceTicks)
	}
	ev.Side = "buy"
	got, _ = norm(t).Normalize(ev)
	if a := got.(domain.Trade).Aggressor; a != "sell" {
		t.Errorf("resting buy should give aggressor sell, got %q", a)
	}
	ev.Side = ""
	got, _ = norm(t).Normalize(ev)
	if a := got.(domain.Trade).Aggressor; a != "" {
		t.Errorf("an unknown side stays unknown, got %q", a)
	}
}

func TestNormalizeUsesEachInstrumentsUnits(t *testing.T) {
	// venueA:QRS has tick 0.05 and lot 0.001.
	ev := alphaTrade()
	ev.Symbol, ev.Domain, ev.Price, ev.Qty = "QRS", "QRS", "150.15", "0.25"
	got, rej := norm(t).Normalize(ev)
	if rej != nil {
		t.Fatal(rej)
	}
	if tr := got.(domain.Trade); tr.PriceTicks != 3003 || tr.Quantity != 250 {
		t.Errorf("got %d ticks, %d lots; want 3003 and 250", tr.PriceTicks, tr.Quantity)
	}
}

func TestNormalizeQuoteAndDepth(t *testing.T) {
	got, rej := norm(t).Normalize(alphaQuote())
	if rej != nil {
		t.Fatal(rej)
	}
	q := got.(domain.Quote)
	if q.BidTicks != 10124 || q.BidSize != 300 || q.AskTicks != 10126 || q.AskSize != 200 || q.Flags != nil || q.Seq != 43 {
		t.Errorf("quote %+v", q)
	}

	got, rej = norm(t).Normalize(betaDepth())
	if rej != nil {
		t.Fatal(rej)
	}
	d := got.(domain.Depth)
	wantBids := []domain.Level{{PriceTicks: 1001, Quantity: 50}, {PriceTicks: 1000, Quantity: 70}}
	wantAsks := []domain.Level{{PriceTicks: 1002, Quantity: 10}, {PriceTicks: 1003, Quantity: 20}}
	if !reflect.DeepEqual(d.Bids, wantBids) || !reflect.DeepEqual(d.Asks, wantAsks) || d.InstrumentID != "venueB:ABC.B" {
		t.Errorf("depth %+v", d)
	}
}

func flagsOf(t *testing.T, ev ingestors.Event) []string {
	t.Helper()
	got, rej := norm(t).Normalize(ev)
	if rej != nil {
		t.Fatalf("unexpected rejection: %v", rej)
	}
	switch v := got.(type) {
	case domain.Trade:
		return v.Flags
	case domain.Quote:
		return v.Flags
	case domain.Depth:
		return v.Flags
	}
	return nil
}

func TestNormalizeFlagsWithoutRewriting(t *testing.T) {
	// Venue clock ahead of ours: flagged, and both timestamps kept as supplied.
	ev := alphaTrade()
	ev.EventTime = rcTime.Add(2 * time.Second)
	got, _ := norm(t).Normalize(ev)
	tr := got.(domain.Trade)
	if !reflect.DeepEqual(tr.Flags, []string{domain.FlagClockSkew}) {
		t.Errorf("flags %v, want clock_skew", tr.Flags)
	}
	if !tr.EventTime.Equal(ev.EventTime) || !tr.ReceiveTime.Equal(rcTime) {
		t.Errorf("timestamps were altered: event %s receive %s", tr.EventTime, tr.ReceiveTime)
	}

	ev = alphaTrade()
	ev.EventTime = rcTime.Add(250 * time.Millisecond) // exactly the tolerance
	if f := flagsOf(t, ev); f != nil {
		t.Errorf("at the skew tolerance: flags %v, want none", f)
	}
	ev.EventTime = rcTime.Add(-5 * time.Second) // exactly the threshold
	if f := flagsOf(t, ev); f != nil {
		t.Errorf("at the stale threshold: flags %v, want none", f)
	}
	ev.EventTime = rcTime.Add(-5*time.Second - time.Nanosecond)
	if f := flagsOf(t, ev); !reflect.DeepEqual(f, []string{domain.FlagStale}) {
		t.Errorf("past the stale threshold: flags %v, want stale", f)
	}

	q := alphaQuote()
	q.Bid, q.BidQty = "", ""
	if f := flagsOf(t, q); !reflect.DeepEqual(f, []string{domain.FlagNoBid}) {
		t.Errorf("one-sided quote: flags %v, want no_bid", f)
	}
	q = alphaQuote()
	q.Ask, q.AskQty = "", ""
	if f := flagsOf(t, q); !reflect.DeepEqual(f, []string{domain.FlagNoAsk}) {
		t.Errorf("one-sided quote: flags %v, want no_ask", f)
	}
	q = alphaQuote()
	q.Ask = q.Bid
	if f := flagsOf(t, q); !reflect.DeepEqual(f, []string{domain.FlagLocked}) {
		t.Errorf("locked quote: flags %v, want locked", f)
	}
	q = alphaQuote()
	q.BidQty = "0" // a quote may show zero size
	if f := flagsOf(t, q); f != nil {
		t.Errorf("zero-size quote: flags %v, want none", f)
	}
}

func TestNormalizeTimestampsAreUTC(t *testing.T) {
	ev := alphaTrade()
	ev.EventTime = evTime.In(time.FixedZone("EST", -5*3600))
	ev.RecvTime = rcTime.In(time.FixedZone("CET", 3600))
	got, _ := norm(t).Normalize(ev)
	tr := got.(domain.Trade)
	if tr.EventTime.Location() != time.UTC || tr.ReceiveTime.Location() != time.UTC || !tr.EventTime.Equal(evTime) {
		t.Errorf("event %v receive %v", tr.EventTime, tr.ReceiveTime)
	}
}

func TestNormalizeRejections(t *testing.T) {
	trade := func(f func(*ingestors.Event)) ingestors.Event { ev := alphaTrade(); f(&ev); return ev }
	quote := func(f func(*ingestors.Event)) ingestors.Event { ev := alphaQuote(); f(&ev); return ev }
	depth := func(f func(*ingestors.Event)) ingestors.Event { ev := betaDepth(); f(&ev); return ev }
	cases := []struct {
		name string
		ev   ingestors.Event
		want string
	}{
		{"unknown provider", trade(func(e *ingestors.Event) { e.Provider = "gamma" }), normalizer.ReasonUnknownProvider},
		{"unknown symbol", trade(func(e *ingestors.Event) { e.Symbol = "NOPE" }), normalizer.ReasonUnknownInstrument},
		{"another venue's symbol", trade(func(e *ingestors.Event) { e.Symbol = "XYZ.B" }), normalizer.ReasonUnknownInstrument},
		{"unknown kind", trade(func(e *ingestors.Event) { e.Kind = "candle" }), normalizer.ReasonUnknownKind},
		{"zero event time", trade(func(e *ingestors.Event) { e.EventTime = time.Time{} }), normalizer.ReasonMissingEventTime},
		{"epoch event time", trade(func(e *ingestors.Event) { e.EventTime = time.Unix(0, 0) }), normalizer.ReasonMissingEventTime},
		{"event time beyond nanosecond range", trade(func(e *ingestors.Event) { e.EventTime = time.Date(2300, 1, 1, 0, 0, 0, 0, time.UTC) }), normalizer.ReasonEventTimeRange},
		{"event time the store would expire wrongly", trade(func(e *ingestors.Event) { e.EventTime = time.Date(2107, 1, 1, 0, 0, 0, 0, time.UTC) }), normalizer.ReasonEventTimeRange},
		{"event time at the upper bound", trade(func(e *ingestors.Event) { e.EventTime = domain.MaxTime }), normalizer.ReasonEventTimeRange},
		{"price between ticks", trade(func(e *ingestors.Event) { e.Price = "101.255" }), normalizer.ReasonPriceNotOnTick},
		{"price with exponent", trade(func(e *ingestors.Event) { e.Price = "1e2" }), normalizer.ReasonBadPrice},
		{"empty price", trade(func(e *ingestors.Event) { e.Price = "" }), normalizer.ReasonBadPrice},
		{"overflowing price", trade(func(e *ingestors.Event) { e.Price = "999999999999999999999" }), normalizer.ReasonBadPrice},
		{"zero price", trade(func(e *ingestors.Event) { e.Price = "0" }), normalizer.ReasonNonPositivePrice},
		{"negative price", trade(func(e *ingestors.Event) { e.Price = "-101.25" }), normalizer.ReasonNonPositivePrice},
		{"fractional lot", trade(func(e *ingestors.Event) { e.Qty = "100.5" }), normalizer.ReasonQuantityNotOnLot},
		{"non-numeric quantity", trade(func(e *ingestors.Event) { e.Qty = "lots" }), normalizer.ReasonBadQuantity},
		{"zero quantity trade", trade(func(e *ingestors.Event) { e.Qty = "0" }), normalizer.ReasonNonPositiveQty},
		{"negative quantity trade", trade(func(e *ingestors.Event) { e.Qty = "-100" }), normalizer.ReasonNonPositiveQty},
		{"trade without id", trade(func(e *ingestors.Event) { e.TradeID = "" }), normalizer.ReasonMissingTradeID},
		{"quote with neither side", quote(func(e *ingestors.Event) { e.Bid, e.Ask = "", "" }), normalizer.ReasonEmptyQuote},
		{"crossed quote", quote(func(e *ingestors.Event) { e.Bid = "101.27" }), normalizer.ReasonCrossedQuote},
		{"negative quote size", quote(func(e *ingestors.Event) { e.AskQty = "-1" }), normalizer.ReasonNegativeSize},
		{"quote price between ticks", quote(func(e *ingestors.Event) { e.Ask = "101.263" }), normalizer.ReasonPriceNotOnTick},
		{"depth with no levels", depth(func(e *ingestors.Event) { e.Bids, e.Asks = nil, nil }), normalizer.ReasonEmptyDepth},
		{"bids not descending", depth(func(e *ingestors.Event) { e.Bids = [][2]string{{"10", "50"}, {"10.01", "70"}} }), normalizer.ReasonUnsortedDepth},
		{"repeated ask level", depth(func(e *ingestors.Event) { e.Asks = [][2]string{{"10.02", "10"}, {"10.02", "20"}} }), normalizer.ReasonUnsortedDepth},
		{"crossed book", depth(func(e *ingestors.Event) { e.Bids = [][2]string{{"10.02", "50"}} }), normalizer.ReasonCrossedBook},
		{"empty depth level", depth(func(e *ingestors.Event) { e.Asks = [][2]string{{"10.02", "0"}} }), normalizer.ReasonNonPositiveQty},
	}
	n := norm(t)
	for _, c := range cases {
		got, rej := n.Normalize(c.ev)
		if rej == nil {
			t.Errorf("%s: accepted as %+v, want %s", c.name, got, c.want)
			continue
		}
		if rej.Reason != c.want || rej.Detail == "" || got != nil {
			t.Errorf("%s: rejected as %s (%q), want %s", c.name, rej.Reason, rej.Detail, c.want)
		}
	}
}

func TestOneSidedDepthIsValid(t *testing.T) {
	ev := betaDepth()
	ev.Bids = nil
	if _, rej := norm(t).Normalize(ev); rej != nil {
		t.Errorf("a book with only asks is valid: %v", rej)
	}
}

func TestDedupKeysIdentifyFacts(t *testing.T) {
	a, _ := norm(t).Normalize(alphaTrade())
	redelivered := alphaTrade()
	redelivered.RecvTime = rcTime.Add(time.Minute) // same venue fact, received again later
	redelivered.Source = domain.SourceReplay
	b, _ := norm(t).Normalize(redelivered)
	if a.(domain.Trade).DedupKey() != b.(domain.Trade).DedupKey() {
		t.Error("a redelivered trade must have the same deduplication key, whenever and however it arrives")
	}
	other := alphaTrade()
	other.TradeID = "A-7782"
	c, _ := norm(t).Normalize(other)
	if a.(domain.Trade).DedupKey() == c.(domain.Trade).DedupKey() {
		t.Error("different venue trade ids are different facts")
	}
	q1, _ := norm(t).Normalize(alphaQuote())
	q2ev := alphaQuote()
	q2ev.Seq++
	q2, _ := norm(t).Normalize(q2ev)
	if q1.(domain.Quote).DedupKey() == q2.(domain.Quote).DedupKey() {
		t.Error("quotes with different sequence numbers are different facts")
	}
	g := domain.Gap{Provider: "beta", Session: "B7", SeqDomain: "main", FromSeq: 21, ToSeq: 23}
	if g.Missing() != 3 || g.DedupKey() != "beta/B7/main/21" {
		t.Errorf("gap: missing %d key %s", g.Missing(), g.DedupKey())
	}
}
