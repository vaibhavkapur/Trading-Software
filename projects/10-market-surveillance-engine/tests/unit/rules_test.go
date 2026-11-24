package unit

import (
	"fmt"
	"math"
	"sort"
	"strings"
	"testing"

	"surveillance/src/domain"
	"surveillance/src/fixed"
	"surveillance/src/pipeline"
	"surveillance/src/rules"
	"surveillance/tests/harness"
)

// ---- builders ----

func ord(acct, id string, action domain.Action, side domain.Side, qty int64, price string, ms int) domain.Event {
	ev := order(acct, id, action, ms)
	ev.Side, ev.Qty, ev.Price = side, qty, 0
	if price != "" {
		ev.Price = fixed.MustParse(price)
	}
	return ev
}

func trd(buyAcct, buyOrder, sellAcct, sellOrder string, qty int64, price string, ms int) domain.Event {
	ev := trade(buyAcct, sellAcct, qty, ms)
	ev.BuyOrderID, ev.SellOrderID, ev.Price = buyOrder, sellOrder, fixed.MustParse(price)
	return ev
}

// feed runs events through the detectors the way the engine does: in
// event-time order, with deferred checks released as time passes.
func feed(d *rules.Detectors, evs ...domain.Event) []rules.Detection {
	sort.SliceStable(evs, func(i, j int) bool { return evs[i].TS() < evs[j].TS() })
	var out []rules.Detection
	for _, ev := range evs {
		out = append(out, d.OnWatermark(ev.TS())...)
		out = append(out, d.OnEvent(ev)...)
	}
	return append(out, d.OnWatermark(math.MaxInt64)...)
}

func detectors(edit ...func(*rules.Config)) *rules.Detectors {
	c := harness.DefaultRules()
	for _, fn := range edit {
		fn(&c)
	}
	return rules.NewDetectors(c)
}

func only(t *testing.T, dets []rules.Detection, rule string) rules.Detection {
	t.Helper()
	if len(dets) != 1 || dets[0].RuleID != rule {
		t.Fatalf("got %d detections %v, want exactly one %s", len(dets), ruleIDs(dets), rule)
	}
	return dets[0]
}

func ruleIDs(dets []rules.Detection) []string {
	out := []string{}
	for _, d := range dets {
		out = append(out, d.RuleID)
	}
	return out
}

func none(t *testing.T, name string, dets []rules.Detection) {
	t.Helper()
	if len(dets) != 0 {
		t.Errorf("%s: fired %v (%s)", name, ruleIDs(dets), dets[0].Explanation)
	}
}

// ---- window rules ----

func fw(newOrders, amends, cancels, trades int) domain.FeatureWindow {
	f := domain.FeatureWindow{AccountID: "A", Symbol: "SIMA", Start: harness.At(0), End: harness.At(60000),
		NewOrders: newOrders, Amends: amends, Cancels: cancels, Trades: trades, Messages: newOrders + amends + cancels,
		EventRefs: []string{"e1", "e2"}, LastEventID: "e2", LastEventTime: harness.At(59000)}
	pipeline.Ratios(&f)
	return f
}

func TestHighCancelRateBoundaries(t *testing.T) {
	d := detectors(func(c *rules.Config) { c.Rules.HighOrderTradeRatio.Enabled = false })
	// Thresholds: at least 20 new orders, cancels >= 90% of them.
	none(t, "19 new orders, all cancelled", d.OnWindow(fw(19, 0, 19, 0)))
	none(t, "20 new, 17 cancels (85%)", d.OnWindow(fw(20, 0, 17, 0)))
	none(t, "21 new, 18 cancels (85.7%)", d.OnWindow(fw(21, 0, 18, 0)))

	det := only(t, d.OnWindow(fw(20, 0, 18, 3)), rules.HighCancelRate) // exactly 90%
	if det.Severity != domain.SeverityLow || det.RuleVersion != 1 {
		t.Errorf("severity %s, version %d", det.Severity, det.RuleVersion)
	}
	if det.Observed["cancel_rate_pct"] != 90 || det.Observed["new_orders"] != 20 || det.Observed["cancels"] != 18 {
		t.Errorf("observed = %v", det.Observed)
	}
	if det.Thresholds["min_cancel_rate_pct"] != 90 || det.Thresholds["min_new_orders"] != 20 {
		t.Errorf("thresholds = %v", det.Thresholds)
	}
	if det.DedupKey != "HIGH_CANCEL_RATE|A|SIMA" || !det.WindowStart.Equal(harness.At(0)) || !det.WindowEnd.Equal(harness.At(60000)) {
		t.Errorf("key %q window %v..%v", det.DedupKey, det.WindowStart, det.WindowEnd)
	}
	if det.TriggerEventID != "e2" || len(det.EventRefs) != 2 {
		t.Errorf("evidence = %v trigger %s", det.EventRefs, det.TriggerEventID)
	}
	if !strings.Contains(det.Explanation, "90.0%") || !strings.Contains(det.Explanation, "nothing more") {
		t.Errorf("explanation does not state the figure and the caveat: %s", det.Explanation)
	}

	// Cancels of orders entered before the window can push the rate past 100%.
	only(t, d.OnWindow(fw(20, 0, 30, 0)), rules.HighCancelRate)

	// Severity steps on the number of new orders: 50 medium, 150 high.
	for n, want := range map[int]string{49: domain.SeverityLow, 50: domain.SeverityMedium, 149: domain.SeverityMedium, 150: domain.SeverityHigh} {
		if got := only(t, d.OnWindow(fw(n, 0, n, 0)), rules.HighCancelRate).Severity; got != want {
			t.Errorf("%d new orders: severity %s, want %s", n, got, want)
		}
	}
}

func TestHighOrderTradeRatioBoundaries(t *testing.T) {
	d := detectors(func(c *rules.Config) { c.Rules.HighCancelRate.Enabled = false })
	// Thresholds: at least 40 messages, messages >= 20 x trades.
	none(t, "39 messages, no trades", d.OnWindow(fw(10, 29, 0, 0)))
	none(t, "40 messages, 3 trades (13.3)", d.OnWindow(fw(10, 30, 0, 3)))
	none(t, "59 messages, 3 trades (19.7)", d.OnWindow(fw(10, 49, 0, 3)))

	det := only(t, d.OnWindow(fw(10, 50, 0, 3)), rules.HighOrderTradeRatio) // exactly 20
	if det.Severity != domain.SeverityLow || det.Observed["order_trade_ratio"] != 20 || det.Observed["messages"] != 60 {
		t.Errorf("severity %s observed %v", det.Severity, det.Observed)
	}
	// No trades at all: counted as one, and the explanation says so.
	det = only(t, d.OnWindow(fw(10, 30, 0, 0)), rules.HighOrderTradeRatio)
	if det.Observed["order_trade_ratio"] != 40 || !strings.Contains(det.Explanation, "no trades") {
		t.Errorf("observed %v: %s", det.Observed, det.Explanation)
	}
	// Severity steps on the whole-number ratio: 50 medium, 100 high.
	for msgs, want := range map[int]string{49: domain.SeverityLow, 50: domain.SeverityMedium, 99: domain.SeverityMedium, 100: domain.SeverityHigh} {
		if got := only(t, d.OnWindow(fw(msgs, 0, 0, 1)), rules.HighOrderTradeRatio).Severity; got != want {
			t.Errorf("%d messages per trade: severity %s, want %s", msgs, got, want)
		}
	}
	// Both window rules can fire on one window; a disabled rule never does.
	both := detectors().OnWindow(fw(30, 0, 30, 0))
	if len(both) != 2 {
		t.Errorf("expected both window rules, got %v", ruleIDs(both))
	}
	off := detectors(func(c *rules.Config) {
		c.Rules.HighCancelRate.Enabled, c.Rules.HighOrderTradeRatio.Enabled = false, false
	})
	none(t, "both disabled", off.OnWindow(fw(300, 0, 300, 0)))
}

// ---- layering ----

type layerSpec struct {
	n, levels    int   // sell orders, and how many distinct prices they use
	qty, execQty int64 // per layer, and the buy that trades
	pull         int   // how many layers are cancelled
	cancelAt     int   // ms after the fill of the first cancel
	placeAt      int   // ms before the fill of the first layer
}

func base() layerSpec {
	return layerSpec{n: 4, levels: 4, qty: 1000, execQty: 200, pull: 4, cancelAt: 1000, placeAt: 3000}
}

const fillAt = 40000

// layerEvents: account L stacks sell orders, buys a little from CP, then
// cancels. Cancels carry no side, as on many venues.
func layerEvents(s layerSpec) []domain.Event {
	var evs []domain.Event
	for i := 0; i < s.n; i++ {
		evs = append(evs, ord("L", fmt.Sprintf("s%d", i), domain.ActionNew, domain.Sell, s.qty,
			fmt.Sprintf("100.%02d", 10+i%s.levels), fillAt-s.placeAt+i*100))
	}
	evs = append(evs,
		ord("L", "b", domain.ActionNew, domain.Buy, s.execQty, "100.00", fillAt-100),
		trd("L", "b", "CP", "cp", s.execQty, "100.00", fillAt))
	for i := 0; i < s.pull; i++ {
		evs = append(evs, ord("L", fmt.Sprintf("s%d", i), domain.ActionCancel, "", 0, "", fillAt+s.cancelAt+i*50))
	}
	return evs
}

func TestLayeringLikeFires(t *testing.T) {
	det := only(t, feed(detectors(), layerEvents(base())...), rules.LayeringLike)
	if det.Symbol != "SIMA" || fmt.Sprint(det.Accounts) != "[L]" || det.DedupKey != "LAYERING_LIKE|L|SIMA|SELL" {
		t.Errorf("identity wrong: %+v", det)
	}
	want := map[string]float64{"layered_orders": 4, "price_levels": 4, "layered_qty": 4000, "cancelled_qty": 4000,
		"cancel_pct": 100, "opposite_exec_qty": 200, "imbalance_ratio": 20}
	for k, v := range want {
		if det.Observed[k] != v {
			t.Errorf("observed[%s] = %v, want %v", k, det.Observed[k], v)
		}
	}
	// 4 layers + 1 trade + 4 cancels. The genuine buy order is not evidence of layering.
	if len(det.EventRefs) != 9 {
		t.Errorf("%d evidence refs, want 9", len(det.EventRefs))
	}
	if !det.WindowStart.Equal(harness.At(fillAt-3000)) || !det.WindowEnd.Equal(harness.At(fillAt+1150)) {
		t.Errorf("window %v..%v", det.WindowStart, det.WindowEnd)
	}
	if !det.TriggerEventTime.Equal(harness.At(fillAt + 1150)) {
		t.Errorf("trigger at %v, want the last cancel", det.TriggerEventTime)
	}
	if det.Severity != domain.SeverityHigh { // 20x >= 15x
		t.Errorf("severity %s", det.Severity)
	}
	for _, k := range []string{"min_layer_orders", "min_price_levels", "min_cancel_pct", "min_imbalance_ratio", "lookback_s", "cancel_within_s"} {
		if _, ok := det.Thresholds[k]; !ok {
			t.Errorf("threshold %s not recorded", k)
		}
	}
	if !strings.Contains(det.Explanation, "not a conclusion") {
		t.Errorf("explanation lacks the caveat: %s", det.Explanation)
	}
}

func TestLayeringLikeBoundaries(t *testing.T) {
	with := func(edit func(*layerSpec)) []rules.Detection {
		s := base()
		edit(&s)
		return feed(detectors(), layerEvents(s)...)
	}
	// Each case changes one thing from the pattern that fires.
	none(t, "two layers", with(func(s *layerSpec) { s.n, s.levels, s.pull = 2, 2, 2 }))
	none(t, "three orders at two prices", with(func(s *layerSpec) { s.n, s.levels, s.pull = 3, 2, 3 }))
	none(t, "3 of 4 cancelled (75%)", with(func(s *layerSpec) { s.pull = 3 }))
	none(t, "cancelled 11s after the fill", with(func(s *layerSpec) { s.cancelAt = 11000 }))
	none(t, "last cancel 1ms after the 10s window", with(func(s *layerSpec) { s.cancelAt = 9851 }))
	none(t, "stack placed 31s before the fill", with(func(s *layerSpec) { s.placeAt = 31000 }))
	none(t, "stack 3.96x the fill", with(func(s *layerSpec) { s.qty, s.execQty = 100, 101 }))

	only(t, with(func(s *layerSpec) { s.n, s.levels, s.pull = 3, 3, 3 }), rules.LayeringLike)
	only(t, with(func(s *layerSpec) { s.n, s.levels, s.pull = 5, 5, 4 }), rules.LayeringLike) // exactly 80%
	only(t, with(func(s *layerSpec) { s.cancelAt = 9850 }), rules.LayeringLike)               // last cancel exactly at 10s
	only(t, with(func(s *layerSpec) { s.placeAt = 30000 }), rules.LayeringLike)               // exactly at the lookback

	// Severity steps on the imbalance: 4x low, 8x medium, 15x high.
	for ratio, want := range map[int64]string{4: domain.SeverityLow, 7: domain.SeverityLow, 8: domain.SeverityMedium,
		14: domain.SeverityMedium, 15: domain.SeverityHigh} {
		det := only(t, with(func(s *layerSpec) { s.qty, s.execQty = 25*ratio, 100 }), rules.LayeringLike)
		if det.Severity != want {
			t.Errorf("imbalance %dx: severity %s, want %s", ratio, det.Severity, want)
		}
	}
}

func TestLayeringLikeNeedsTheWholePattern(t *testing.T) {
	s := base()

	// A stacked order that itself traded is not part of a stack that
	// "never trades": with one of three layers filled, two remain.
	s3 := s
	s3.n, s3.levels, s3.pull = 3, 3, 3
	evs := append(layerEvents(s3), trd("X", "x", "L", "s0", 100, "100.10", fillAt-500))
	none(t, "a layer was partly filled", feed(detectors(), evs...))

	// A fill on the same side as the stack is just the stack trading.
	sameSide := []domain.Event{}
	for _, ev := range layerEvents(s) {
		if ev.Kind == domain.KindTrade {
			ev = trd("CP", "cp", "L", "other", 200, "100.00", fillAt)
		}
		sameSide = append(sameSide, ev)
	}
	none(t, "fill on the stacked side", feed(detectors(), sameSide...))

	// Pulling the stack before the fill is not this pattern.
	early := s
	early.cancelAt = -1500
	none(t, "cancelled before the fill", feed(detectors(), layerEvents(early)...))

	// No fill at all: nothing to anchor an episode on.
	var noFill []domain.Event
	for _, ev := range layerEvents(s) {
		if ev.Kind != domain.KindTrade {
			noFill = append(noFill, ev)
		}
	}
	none(t, "no fill", feed(detectors(), noFill...))

	// The same account in another symbol is a different book.
	other := layerEvents(s)
	for i := range other {
		if other[i].Kind == domain.KindTrade {
			other[i].Symbol = "SIMB"
		}
	}
	none(t, "fill in another symbol", feed(detectors(), other...))

	off := detectors(func(c *rules.Config) { c.Rules.LayeringLike.Enabled = false })
	none(t, "rule disabled", feed(off, layerEvents(s)...))
}

func TestLayeringLikeOneDetectionPerEpisode(t *testing.T) {
	// Two fills inside one cancel window are one episode; the executed
	// quantity is their sum.
	evs := append(layerEvents(base()),
		ord("L", "b2", domain.ActionNew, domain.Buy, 100, "100.00", fillAt+300),
		trd("L", "b2", "CP", "cp2", 100, "100.00", fillAt+400))
	det := only(t, feed(detectors(), evs...), rules.LayeringLike)
	if det.Observed["opposite_exec_qty"] != 300 || len(det.EventRefs) != 10 {
		t.Errorf("observed %v with %d refs", det.Observed, len(det.EventRefs))
	}

	// The same pattern repeated a minute later is a second episode.
	first := layerEvents(base())
	second := layerEvents(base())
	for i := range second {
		second[i].EventTime = second[i].EventTime.Add(60e9)
		if second[i].OrderID != "" {
			second[i].OrderID += "-again"
		}
		second[i].BuyOrderID += "-again"
	}
	dets := feed(detectors(), append(first, second...)...)
	if len(dets) != 2 || dets[0].DedupKey != dets[1].DedupKey || dets[0].TriggerEventID == dets[1].TriggerEventID {
		t.Fatalf("want two detections with one dedup key, got %+v", dets)
	}
}

func TestLayeringLikeOnTheBuySide(t *testing.T) {
	// Mirror image: stacked bids, a small sale, bids pulled.
	var evs []domain.Event
	for i := 0; i < 4; i++ {
		evs = append(evs, ord("L", fmt.Sprintf("b%d", i), domain.ActionNew, domain.Buy, 1000, fmt.Sprintf("99.%02d", 90-i), 37000+i*100))
	}
	evs = append(evs, ord("L", "s", domain.ActionNew, domain.Sell, 100, "100.00", 39900),
		trd("CP", "cp", "L", "s", 100, "100.00", 40000))
	for i := 0; i < 4; i++ {
		// A partial cancel followed by the rest: both count.
		evs = append(evs, ord("L", fmt.Sprintf("b%d", i), domain.ActionCancel, domain.Buy, 400, "", 41000+i*10),
			ord("L", fmt.Sprintf("b%d", i), domain.ActionCancel, domain.Buy, 0, "", 42000+i*10))
	}
	det := only(t, feed(detectors(), evs...), rules.LayeringLike)
	if det.DedupKey != "LAYERING_LIKE|L|SIMA|BUY" || det.Observed["cancelled_qty"] != 4000 || len(det.EventRefs) != 13 {
		t.Errorf("key %s observed %v refs %d", det.DedupKey, det.Observed, len(det.EventRefs))
	}
}

// ---- wash trades ----

func TestWashSelfTrades(t *testing.T) {
	d := detectors()
	// One self-match is never enough.
	none(t, "one self trade", feed(d, trd("W", "b1", "W", "s1", 100, "50.00", 1000)))
	det := only(t, feed(d, trd("W", "b2", "W", "s2", 300, "50.00", 2000)), rules.WashTradeLike)
	if det.DedupKey != "WASH_TRADE_LIKE|SELF|W|SIMA" || det.Observed["self_trades"] != 2 || det.Observed["self_traded_qty"] != 400 {
		t.Errorf("key %s observed %v", det.DedupKey, det.Observed)
	}
	if len(det.EventRefs) != 2 || !det.WindowStart.Equal(harness.At(1000)) || !det.WindowEnd.Equal(harness.At(2000)) {
		t.Errorf("evidence %v window %v..%v", det.EventRefs, det.WindowStart, det.WindowEnd)
	}
	if !strings.Contains(det.Explanation, "accidental") {
		t.Errorf("explanation lacks the caveat: %s", det.Explanation)
	}

	// Trades older than the lookback (300s) no longer count.
	d = detectors()
	none(t, "self trades 301s apart", feed(d,
		trd("W", "b1", "W", "s1", 100, "50.00", 0), trd("W", "b2", "W", "s2", 100, "50.00", 301000)))
	d = detectors()
	only(t, feed(d, trd("W", "b1", "W", "s1", 100, "50.00", 0), trd("W", "b2", "W", "s2", 100, "50.00", 300000)), rules.WashTradeLike)
}

func TestWashAcrossAccountsOfOneOwner(t *testing.T) {
	// Different accounts, same beneficial owner: still trading with oneself.
	mk := func(ms int) domain.Event {
		ev := trd("ACC-1001", "b", "ACC-1002", "s", 100, "50.00", ms)
		ev.BuyOwner, ev.SellOwner = "OWNER-A", "OWNER-A"
		return ev
	}
	det := only(t, feed(detectors(), mk(1000), mk(2000)), rules.WashTradeLike)
	if fmt.Sprint(det.Accounts) != "[ACC-1001 ACC-1002]" || det.DedupKey != "WASH_TRADE_LIKE|SELF|OWNER-A|SIMA" {
		t.Errorf("accounts %v key %s", det.Accounts, det.DedupKey)
	}
	if !strings.Contains(det.Explanation, "OWNER-A") {
		t.Errorf("explanation does not name the shared owner: %s", det.Explanation)
	}
}

// pair builds trades between A and B: positive qty means A buys from B.
func pairTrades(price string, qtys ...int64) []domain.Event {
	var evs []domain.Event
	for i, q := range qtys {
		if q > 0 {
			evs = append(evs, trd("A", "", "B", "", q, price, 1000*(i+1)))
		} else {
			evs = append(evs, trd("B", "", "A", "", -q, price, 1000*(i+1)))
		}
	}
	return evs
}

func TestWashPairBoundaries(t *testing.T) {
	// Thresholds: at least 4 trades, both directions, >= 90% offsetting,
	// average prices within 10 bps.
	none(t, "three trades", feed(detectors(), pairTrades("50.00", 500, -500, 500)...))
	none(t, "one direction only", feed(detectors(), pairTrades("50.00", 100, 100, 100, 100)...))
	none(t, "89% offsetting", feed(detectors(), pairTrades("50.00", 500, 500, -445, -445)...))

	det := only(t, feed(detectors(), pairTrades("50.00", 500, 500, -450, -450)...), rules.WashTradeLike) // exactly 90%
	if det.DedupKey != "WASH_TRADE_LIKE|PAIR|A|B|SIMA" || fmt.Sprint(det.Accounts) != "[A B]" {
		t.Errorf("key %s accounts %v", det.DedupKey, det.Accounts)
	}
	if det.Observed["offset_pct"] != 90 || det.Observed["qty_a_buys"] != 1000 || det.Observed["qty_b_buys"] != 900 || det.Observed["pair_trades"] != 4 {
		t.Errorf("observed %v", det.Observed)
	}

	// Price: A buys 2 x 500 at 100.00, B buys back at 100.10 (exactly
	// 10 bps) and then at 100.11 (just over).
	at := func(back string) []domain.Event {
		return append(pairTrades("100.00", 500, 500), append(
			[]domain.Event{}, trd("B", "", "A", "", 500, back, 3000), trd("B", "", "A", "", 500, back, 4000))...)
	}
	det = only(t, feed(detectors(), at("100.10")...), rules.WashTradeLike)
	if det.Observed["price_diff_bps"] != 10 {
		t.Errorf("price_diff_bps = %v, want 10", det.Observed["price_diff_bps"])
	}
	none(t, "prices 11 bps apart", feed(detectors(), at("100.11")...))

	// The direction of the first trade does not matter, nor which
	// account is named first.
	only(t, feed(detectors(), pairTrades("50.00", -500, 500, -500, 500)...), rules.WashTradeLike)

	// Each further trade re-fires; severity follows the trade count.
	dets := feed(detectors(), pairTrades("50.00", 100, -100, 100, -100, 100, -100)...)
	if len(dets) != 2 || dets[0].Severity != domain.SeverityLow || dets[1].Severity != domain.SeverityMedium {
		// Fires at trade 4 (100% offset) and trade 6; trade 5 is 2:3 = 67%.
		t.Errorf("got %d detections: %+v", len(dets), dets)
	}

	// Very large prices and sizes must not overflow the comparison.
	huge := []domain.Event{
		trd("A", "", "B", "", 900_000_000_000, "9999999999.0000", 1000), trd("B", "", "A", "", 900_000_000_000, "9999999999.0000", 2000),
		trd("A", "", "B", "", 900_000_000_000, "9999999999.0000", 3000), trd("B", "", "A", "", 900_000_000_000, "9999999999.0000", 4000)}
	only(t, feed(detectors(), huge...), rules.WashTradeLike)

	off := detectors(func(c *rules.Config) { c.Rules.WashTradeLike.Enabled = false })
	none(t, "rule disabled", feed(off, pairTrades("50.00", 500, -500, 500, -500)...))
}

// ---- suppression ----

func detection(key string, endMs int, sev string, refs ...string) rules.Detection {
	return rules.Detection{RuleID: rules.HighCancelRate, RuleVersion: 1, Severity: sev, Symbol: "SIMA",
		Accounts: []string{"A"}, DedupKey: key, WindowStart: harness.At(endMs - 60000), WindowEnd: harness.At(endMs),
		Thresholds: map[string]float64{"t": 1}, Observed: map[string]float64{"end": float64(endMs)},
		EventRefs: refs, Explanation: "at " + fmt.Sprint(endMs), TriggerEventID: refs[len(refs)-1]}
}

func TestSuppressionFoldsRepeats(t *testing.T) {
	b := rules.NewBook(rules.Suppression{CooldownS: 300, MaxEvidenceRefs: 4})
	meta := rules.Meta{RulesetVersion: "v", RulesetHash: "h"}
	now := harness.T0.Add(3600e9)

	a1, created := b.Apply(detection("k", 60000, domain.SeverityMedium, "e1", "e2"), meta, now)
	if !created || a1.Occurrences != 1 || a1.Status != domain.AlertOpen || a1.RulesetVersion != "v" || a1.RulesetHash != "h" {
		t.Fatalf("first detection: created=%v %+v", created, a1)
	}
	if a1.Disclaimer != domain.Disclaimer {
		t.Error("alert carries no disclaimer")
	}

	// An overlapping window 10s later: same alert, evidence unioned,
	// window widened. A lower severity does not downgrade it.
	a2, created := b.Apply(detection("k", 70000, domain.SeverityLow, "e2", "e3"), meta, now.Add(1e9))
	if created || a2.ID != a1.ID || a2.Occurrences != 2 {
		t.Fatalf("repeat was not folded: created=%v %+v", created, a2)
	}
	if fmt.Sprint(a2.EventRefs) != "[e1 e2 e3]" || !a2.WindowStart.Equal(harness.At(0)) || !a2.WindowEnd.Equal(harness.At(70000)) {
		t.Errorf("merged evidence %v window %v..%v", a2.EventRefs, a2.WindowStart, a2.WindowEnd)
	}
	if a2.Severity != domain.SeverityMedium || a2.Observed["end"] != 60000 {
		t.Errorf("a weaker repeat changed the alert: %s %v", a2.Severity, a2.Observed)
	}
	if !a2.FirstDetectedAt.Equal(now) || !a2.LastDetectedAt.Equal(now.Add(1e9)) {
		t.Errorf("detected-at times wrong: %v %v", a2.FirstDetectedAt, a2.LastDetectedAt)
	}

	// A stronger repeat raises severity and brings its own figures.
	a3, _ := b.Apply(detection("k", 80000, domain.SeverityHigh, "e4", "e5"), meta, now)
	if a3.Severity != domain.SeverityHigh || a3.Observed["end"] != 80000 || a3.Explanation != "at 80000" {
		t.Errorf("a stronger repeat did not update the alert: %+v", a3)
	}
	// Evidence is capped, and says so.
	if len(a3.EventRefs) != 4 || !a3.EvidenceTruncated {
		t.Errorf("evidence cap not applied: %v truncated=%v", a3.EventRefs, a3.EvidenceTruncated)
	}

	// A different key is a different alert.
	if other, created := b.Apply(detection("k2", 80000, domain.SeverityLow, "e9"), meta, now); !created || other.ID == a1.ID {
		t.Error("a different dedup key was folded in")
	}
	if b.Len() != 2 {
		t.Errorf("book holds %d alerts, want 2", b.Len())
	}
}

func TestSuppressionCooldownAndCaseBoundary(t *testing.T) {
	b := rules.NewBook(rules.Suppression{CooldownS: 300, MaxEvidenceRefs: 10})
	meta := rules.Meta{}
	a1, _ := b.Apply(detection("k", 60000, domain.SeverityLow, "e1"), meta, harness.T0)

	// Exactly at the cooldown (evidence ends 300s later): still folded.
	if a, created := b.Apply(detection("k", 360000, domain.SeverityLow, "e2"), meta, harness.T0); created || a.ID != a1.ID {
		t.Error("a repeat exactly at the cooldown raised a new alert")
	}
	// The cooldown runs from the alert's latest evidence, now at 360s.
	// 301s after that: a new alert.
	a2, created := b.Apply(detection("k", 661000, domain.SeverityLow, "e3"), meta, harness.T0)
	if !created || a2.ID == a1.ID {
		t.Fatal("a repeat past the cooldown was folded into the old alert")
	}

	// Once an analyst has the alert in a case, new activity is a new
	// alert: the snapshot they are working from is not extended.
	b.SetCase(a2.ID, domain.AlertInCase, []string{"case-000001"})
	a3, created := b.Apply(detection("k", 662000, domain.SeverityLow, "e4"), meta, harness.T0)
	if !created || a3.ID == a2.ID {
		t.Fatal("a repeat was folded into an alert that is already in a case")
	}
	if got, _ := b.Get(a2.ID); got.Occurrences != 1 || got.Status != domain.AlertInCase || fmt.Sprint(got.CaseIDs) != "[case-000001]" {
		t.Errorf("the in-case alert changed: %+v", got)
	}
	// The newest alert for the key takes later repeats.
	if a, created := b.Apply(detection("k", 663000, domain.SeverityLow, "e5"), meta, harness.T0); created || a.ID != a3.ID {
		t.Error("a repeat did not fold into the newest open alert")
	}
}

func TestAlertIDsAreDeterministic(t *testing.T) {
	mk := func() string {
		b := rules.NewBook(rules.Suppression{CooldownS: 300, MaxEvidenceRefs: 10})
		a, _ := b.Apply(detection("k", 60000, domain.SeverityLow, "e1"), rules.Meta{}, harness.T0)
		return a.ID
	}
	if a, b := mk(), mk(); a != b || !strings.HasPrefix(a, "alr-") {
		t.Errorf("IDs differ across identical runs: %s vs %s", a, b)
	}
}
