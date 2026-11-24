package unit

import (
	"fmt"
	"testing"
	"time"

	"surveillance/src/domain"
	"surveillance/src/pipeline"
	"surveillance/tests/harness"
)

var seq uint64

// order builds a normalized order event at T0+ms.
func order(acct, id string, action domain.Action, ms int) domain.Event {
	seq++
	return domain.Event{ID: fmt.Sprintf("e%d", seq), Seq: seq, Kind: domain.KindOrder, Venue: "XSIM", Symbol: "SIMA",
		EventTime: harness.At(ms), ReceivedAt: harness.At(ms), AccountID: acct, Owner: acct, OrderID: id,
		Action: action, Side: domain.Buy, Qty: 100, Price: 1000000}
}

func trade(buy, sell string, qty int64, ms int) domain.Event {
	seq++
	return domain.Event{ID: fmt.Sprintf("e%d", seq), Seq: seq, Kind: domain.KindTrade, Venue: "XSIM", Symbol: "SIMA",
		EventTime: harness.At(ms), ReceivedAt: harness.At(ms), TradeID: fmt.Sprintf("t%d", seq),
		BuyAccount: buy, SellAccount: sell, BuyOwner: buy, SellOwner: sell, Qty: qty, Price: 1000000}
}

func ids(evs []domain.Event) []string {
	out := make([]string, len(evs))
	for i, e := range evs {
		out[i] = e.ID
	}
	return out
}

func TestReorderReleasesInEventTimeOrder(t *testing.T) {
	r := pipeline.NewReorder(5 * time.Second)
	a := order("A", "1", domain.ActionNew, 1000)
	b := order("A", "2", domain.ActionNew, 3000)
	c := order("A", "3", domain.ActionNew, 2000) // arrives after b, belongs before it
	d := order("A", "4", domain.ActionNew, 2000) // same instant as c, arrived later
	for _, ev := range []domain.Event{a, b, c, d} {
		if r.IsLate(ev.TS()) {
			t.Fatalf("%s wrongly late", ev.ID)
		}
		r.Push(ev)
	}
	if got := r.Release(); len(got) != 0 {
		t.Fatalf("released %v before the watermark passed anything", ids(got))
	}
	// Newest event at 9s puts the watermark at 4s: everything above is released.
	r.Push(order("A", "5", domain.ActionNew, 9000))
	got := ids(r.Release())
	want := []string{a.ID, c.ID, d.ID, b.ID}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("released %v, want %v", got, want)
	}
	if r.Buffered() != 1 {
		t.Errorf("buffered = %d, want 1", r.Buffered())
	}
}

func TestWatermarkBoundary(t *testing.T) {
	r := pipeline.NewReorder(5 * time.Second)
	r.Push(order("A", "1", domain.ActionNew, 10000))
	wm, ok := r.Watermark()
	if !ok || wm != harness.At(5000).UnixNano() {
		t.Fatalf("watermark = %v, want T0+5s", time.Unix(0, wm).UTC())
	}
	// Exactly on the watermark is on time; one nanosecond older is late.
	if r.IsLate(wm) {
		t.Error("an event exactly at the watermark was late")
	}
	if !r.IsLate(wm - 1) {
		t.Error("an event just behind the watermark was not late")
	}
	// The watermark never moves backwards: not with an older event, not
	// with a longer allowed lateness, not with an explicit lower target.
	r.Push(order("A", "2", domain.ActionNew, 6000))
	r.SetLateness(time.Minute)
	r.Push(order("A", "3", domain.ActionNew, 11000))
	r.AdvanceTo(wm - int64(time.Second))
	if now, _ := r.Watermark(); now != wm {
		t.Errorf("watermark moved from %d to %d", wm, now)
	}
	// An explicit advance releases what it passes, and only that.
	r.AdvanceTo(harness.At(10500).UnixNano())
	if got := len(r.Release()); got != 2 {
		t.Errorf("released %d events, want the two at 6s and 10s", got)
	}
}

func TestReorderBeforeAnyEvent(t *testing.T) {
	r := pipeline.NewReorder(time.Second)
	if _, ok := r.Watermark(); ok {
		t.Error("a watermark exists before any event")
	}
	if r.IsLate(0) {
		t.Error("nothing can be late before the first event")
	}
}

func window(fws []domain.FeatureWindow, acct string, endMs int) *domain.FeatureWindow {
	for i := range fws {
		if fws[i].AccountID == acct && fws[i].End.Equal(harness.At(endMs)) {
			return &fws[i]
		}
	}
	return nil
}

func TestHoppingWindows(t *testing.T) {
	w := pipeline.NewWindower(60*time.Second, 10*time.Second, 100)
	// Account A: 4 new, 3 cancels and 1 amend between 12s and 19s.
	for i := 0; i < 4; i++ {
		w.Add(order("A", fmt.Sprint(i), domain.ActionNew, 12000+i*1000))
	}
	for i := 0; i < 3; i++ {
		w.Add(order("A", fmt.Sprint(i), domain.ActionCancel, 16000+i*1000))
	}
	w.Add(order("A", "3", domain.ActionAmend, 19000))
	// One trade between A and B at 19.999s: last instant of the bucket.
	w.Add(trade("A", "B", 70, 19999))
	// An event exactly on a bucket boundary belongs to the later bucket.
	w.Add(order("A", "9", domain.ActionNew, 20000))

	if got := w.Close(harness.At(19999).UnixNano()); len(got) != 0 {
		t.Fatalf("a window closed before the watermark reached its end: %+v", got)
	}
	got := w.Close(harness.At(25000).UnixNano())
	// The only window that has ended is [-40s, 20s).
	if len(got) != 2 {
		t.Fatalf("got %d windows, want one each for A and B: %+v", len(got), got)
	}
	a := window(got, "A", 20000)
	if a == nil || !a.Start.Equal(harness.At(-40000)) {
		t.Fatalf("no window [-40s,20s) for A: %+v", got)
	}
	if a.NewOrders != 4 || a.Cancels != 3 || a.Amends != 1 || a.Messages != 8 || a.Trades != 1 || a.TradedQty != 70 {
		t.Errorf("A counts wrong: %+v", a)
	}
	if a.CancelRate == nil || *a.CancelRate != 0.75 || a.OrderTradeRatio != 8 || a.NoTrades {
		t.Errorf("A ratios wrong: rate=%v otr=%v", a.CancelRate, a.OrderTradeRatio)
	}
	if len(a.EventRefs) != 9 {
		t.Errorf("A has %d evidence refs, want 9", len(a.EventRefs))
	}
	b := window(got, "B", 20000)
	if b == nil || b.Trades != 1 || b.Messages != 0 || b.CancelRate != nil || b.OrderTradeRatio != 0 {
		t.Errorf("B window wrong: %+v", b)
	}

	// Closing again at the same watermark emits nothing twice.
	if again := w.Close(harness.At(25000).UnixNano()); len(again) != 0 {
		t.Errorf("windows emitted twice: %+v", again)
	}

	// Each event sits in size/hop = 6 windows. Run the watermark far
	// ahead: A's burst at 12-19s must appear in exactly windows ending
	// 20s..70s, and the boundary event at 20s in windows ending 30s..80s.
	rest := w.Close(harness.At(600000).UnixNano())
	var burst, boundary int
	for _, fw := range rest {
		if fw.AccountID != "A" {
			continue
		}
		if fw.Cancels == 3 {
			burst++
		}
		if fw.NewOrders == 5 || (fw.NewOrders == 1 && fw.Cancels == 0) {
			boundary++
		}
	}
	if burst != 5 { // five more after the one already emitted
		t.Errorf("burst appeared in %d further windows, want 5", burst)
	}
	if boundary != 6 {
		t.Errorf("boundary event appeared in %d windows, want 6", boundary)
	}
	if w.Pending() {
		t.Error("buckets left behind after every window closed")
	}
}

func TestSelfTradeCountsOnceAndNoTradesRatio(t *testing.T) {
	w := pipeline.NewWindower(10*time.Second, 10*time.Second, 100)
	w.Add(trade("A", "A", 50, 1000))
	for i := 0; i < 7; i++ {
		w.Add(order("C", fmt.Sprint(i), domain.ActionNew, 2000))
	}
	got := w.Close(harness.At(10000).UnixNano())
	a := window(got, "A", 10000)
	if a == nil || a.Trades != 1 || a.TradedQty != 50 {
		t.Errorf("self trade counted wrongly: %+v", a)
	}
	c := window(got, "C", 10000)
	// No trades: the ratio is messages over one, and says so.
	if c == nil || !c.NoTrades || c.OrderTradeRatio != 7 || *c.CancelRate != 0 {
		t.Errorf("no-trade window wrong: %+v", c)
	}
}

func TestWindowEvidenceIsCapped(t *testing.T) {
	w := pipeline.NewWindower(10*time.Second, 10*time.Second, 5)
	for i := 0; i < 12; i++ {
		w.Add(order("A", fmt.Sprint(i), domain.ActionNew, 1000+i))
	}
	got := w.Close(harness.At(10000).UnixNano())
	if len(got) != 1 || len(got[0].EventRefs) != 5 || !got[0].RefsTruncated || got[0].NewOrders != 12 {
		t.Errorf("cap not applied: %+v", got)
	}
}

func TestWindowsBeforeTheEpoch(t *testing.T) {
	// Flooring must round down, not towards zero, for negative times.
	w := pipeline.NewWindower(10*time.Second, 10*time.Second, 10)
	ev := order("A", "1", domain.ActionNew, 0)
	ev.EventTime = time.Unix(-15, 0).UTC()
	w.Add(ev)
	got := w.Close(0)
	if len(got) != 1 || !got[0].Start.Equal(time.Unix(-20, 0)) || !got[0].End.Equal(time.Unix(-10, 0)) {
		t.Errorf("window for t=-15s is %+v, want [-20s,-10s)", got)
	}
}
