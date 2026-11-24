package engine

import (
	"bytes"
	"errors"
	"testing"

	"sor/src/domain"
	"sor/src/risk"
	"sor/src/venuesim"
)

func warm(t *testing.T, cfg Config) (*Engine, int64) {
	t.Helper()
	e := New(cfg, nil)
	e.AdvanceTo(e.Now() + 1000)
	return e, e.Now()
}

func buy(qty, seconds int64) ParentRequest {
	return ParentRequest{Symbol: "XYZ", Side: domain.Buy, Qty: qty, Strategy: "TWAP", DurationSeconds: seconds}
}

func checkInvariants(t *testing.T, e *Engine) {
	t.Helper()
	for _, p := range e.Parents() {
		var fillSum int64
		for _, f := range e.Fills(p.ID) {
			fillSum += f.Qty
		}
		var childSum, reserved int64
		for _, c := range e.Children(p.ID) {
			childSum += c.FilledQty
			if c.FilledQty > c.Qty {
				t.Fatalf("child %s overfilled: %+v", c.ID, c)
			}
			if !c.Status.Terminal() {
				reserved += c.Qty
			}
		}
		if fillSum != p.FilledQty || childSum != p.FilledQty || reserved != p.ReservedQty {
			t.Fatalf("parent %s inconsistent: filled %d fills %d children %d reserved %d/%d", p.ID, p.FilledQty, fillSum, childSum, p.ReservedQty, reserved)
		}
		if p.FilledQty+p.ReservedQty > p.TargetQty {
			t.Fatalf("parent %s exceeds target: %+v", p.ID, p)
		}
	}
}

func TestTWAPFillsOnScheduleWithoutOverfill(t *testing.T) {
	e, start := warm(t, Config{Seed: 5})
	p, err := e.SubmitParent(buy(6000, 120))
	if err != nil {
		t.Fatal(err)
	}
	for s := int64(1); s <= 125; s++ {
		e.AdvanceTo(start + s*1000)
		checkInvariants(t, e)
		got, _ := e.Parent(p.ID)
		// Never ahead of the schedule: by second s at most ceil(s/10) of 12 slices are due.
		if due := min((s+9)/10, 12) * 500; got.FilledQty > due {
			t.Fatalf("t=%ds filled %d ahead of schedule %d", s, got.FilledQty, due)
		}
	}
	got, _ := e.Parent(p.ID)
	if got.Status != domain.ParentFilled || got.FilledQty != 6000 {
		t.Fatalf("parent %+v", got)
	}
	r, _ := e.Report(p.ID)
	if r.MarketVWAPMills == 0 || r.AvgFillPx == 0 || len(r.Venues) < 2 {
		t.Fatalf("report %+v", r)
	}
}

func TestParentRejections(t *testing.T) {
	cold := New(Config{}, nil) // no quotes have arrived yet
	p, err := cold.SubmitParent(buy(1000, 60))
	var rej *risk.Rejection
	if !errors.As(err, &rej) || rej.Code != "NO_MARKET_DATA" || p.Status != domain.ParentRejected {
		t.Fatalf("want NO_MARKET_DATA, got %v / %+v", err, p)
	}
	e, _ := warm(t, Config{})
	for want, req := range map[string]ParentRequest{
		"BAD_STRATEGY":       {Symbol: "XYZ", Side: domain.Buy, Qty: 1000, Strategy: "VWAP", DurationSeconds: 60},
		"SYMBOL_NOT_ALLOWED": {Symbol: "ABC", Side: domain.Buy, Qty: 1000, DurationSeconds: 60},
		"BAD_QTY":            {Symbol: "XYZ", Side: domain.Buy, Qty: 150, DurationSeconds: 60},
		"BAD_DURATION":       {Symbol: "XYZ", Side: domain.Buy, Qty: 1000},
	} {
		if _, err := e.SubmitParent(req); !errors.As(err, &rej) || rej.Code != want {
			t.Errorf("want %s, got %v", want, err)
		}
	}
	e.AdvanceTo(e.Now() + 5000)
	for _, p := range e.Parents() {
		if p.Status != domain.ParentRejected || len(e.Children(p.ID)) != 0 {
			t.Fatalf("rejected parent traded: %+v", p)
		}
	}
}

func TestKillSwitchStopsRoutingAndBlocksNewOrders(t *testing.T) {
	e, start := warm(t, Config{})
	p, _ := e.SubmitParent(buy(6000, 120))
	e.AdvanceTo(start + 30_000)
	e.SetKill(true)
	n := len(e.Children(p.ID))
	e.AdvanceTo(start + 130_000)
	got, _ := e.Parent(p.ID)
	if got.Status != domain.ParentCancelled || got.FilledQty == 0 || got.FilledQty >= 6000 || len(e.Children(p.ID)) != n {
		t.Fatalf("parent %+v children %d -> %d", got, n, len(e.Children(p.ID)))
	}
	var rej *risk.Rejection
	if _, err := e.SubmitParent(buy(1000, 60)); !errors.As(err, &rej) || rej.Code != "KILL_SWITCH" {
		t.Fatalf("want KILL_SWITCH, got %v", err)
	}
	e.SetKill(false)
	if _, err := e.SubmitParent(buy(1000, 60)); err != nil {
		t.Fatalf("after release: %v", err)
	}
	checkInvariants(t, e)
}

// A link drop while an order is in flight: the venue acts on it but the
// report is lost. The child must time out with its quantity still reserved,
// and the true outcome must be recovered by reconciliation on reconnect -
// even after the parent has expired - without ever exceeding the target.
func TestLostReportIsReservedThenReconciled(t *testing.T) {
	for _, reconnectAt := range []int64{5000, 30_000} { // before / after the 20s deadline
		recovered := 0
		for seed := uint64(1); seed <= 10; seed++ {
			cfg := Config{Seed: seed, Venues: []venuesim.Config{{Name: "SOLO", LatencyMs: 200, BaseDepth: 5000}}}
			e, start := warm(t, cfg)
			p, _ := e.SubmitParent(ParentRequest{Symbol: "XYZ", Side: domain.Sell, Qty: 1000, Strategy: "TWAP", DurationSeconds: 20, IntervalSeconds: 20})
			e.AdvanceTo(start + 100) // child sent at +0, reaches the venue at +200
			if err := e.SetFault("SOLO", venuesim.Fault{Down: true}); err != nil {
				t.Fatal(err)
			}
			e.AdvanceTo(start + reconnectAt)
			cs := e.Children(p.ID)
			got, _ := e.Parent(p.ID)
			r, _ := e.Report(p.ID)
			if len(cs) != 1 || cs[0].Status != domain.ChildTimedOut || got.FilledQty != 0 || got.ReservedQty != 1000 || r.UnresolvedQty != 1000 {
				t.Fatalf("seed %d: want one reserved TIMED_OUT child, got %+v / parent %+v", seed, cs, got)
			}
			if wantStatus := map[bool]domain.ParentStatus{false: domain.ParentWorking, true: domain.ParentExpired}[reconnectAt > 20_000]; got.Status != wantStatus {
				t.Fatalf("seed %d: parent status %s, want %s", seed, got.Status, wantStatus)
			}
			e.SetFault("SOLO", venuesim.Fault{})
			e.AdvanceTo(start + reconnectAt + 1000)
			c := e.Children(p.ID)[0]
			got, _ = e.Parent(p.ID)
			if !c.Status.Terminal() || !c.TimedOut || got.FilledQty != c.FilledQty {
				t.Fatalf("seed %d: not reconciled: %+v / parent %+v", seed, c, got)
			}
			if c.FilledQty > 0 {
				recovered++
			}
			e.AdvanceTo(start + 40_000)
			checkInvariants(t, e)
		}
		if recovered == 0 {
			t.Fatal("no seed recovered a lost fill; the test is not exercising reconciliation")
		}
	}
}

func TestBreakerTripsOnTimeoutsAndVenueIsAvoided(t *testing.T) {
	e, start := warm(t, Config{Seed: 2})
	e.SetFault("BRAVO", venuesim.Fault{LatencyMs: 2000}) // round trip 4s > 1s child timeout
	p, _ := e.SubmitParent(buy(20000, 300))
	e.AdvanceTo(start + 302_000)
	var timeouts int
	var lastBravoSent int64
	for _, c := range e.Children(p.ID) {
		if c.Venue == "BRAVO" {
			lastBravoSent = max(lastBravoSent, c.SentMs)
			if c.TimedOut {
				timeouts++
			}
			if !c.Status.Terminal() {
				t.Fatalf("late report never resolved child %+v", c)
			}
		}
	}
	// 2s-old quotes make BRAVO STALE almost immediately, so it only gets the
	// first few orders; after that it must not be routed to at all.
	if timeouts == 0 || lastBravoSent > start+10_000 {
		t.Fatalf("timeouts %d, last BRAVO child sent at +%dms", timeouts, lastBravoSent-start)
	}
	got, _ := e.Parent(p.ID)
	if got.Status != domain.ParentFilled {
		t.Fatalf("router failed to complete around the bad venue: %+v", got)
	}
	checkInvariants(t, e)
}

func TestSameSeedSameEventLog(t *testing.T) {
	run := func(seed uint64) []byte {
		var buf bytes.Buffer
		e := New(Config{Seed: seed}, &buf)
		e.AdvanceTo(e.Now() + 1000)
		e.SetFault("ALPHA", venuesim.Fault{RejectRate: 0.3, DuplicateRate: 0.3})
		e.SubmitParent(buy(8000, 90))
		e.AdvanceTo(e.Now() + 100_000)
		return buf.Bytes()
	}
	a, b := run(21), run(21)
	if len(a) == 0 || !bytes.Equal(a, b) {
		t.Fatal("same seed produced different event logs")
	}
	if bytes.Equal(a, run(22)) {
		t.Fatal("different seeds produced identical event logs")
	}
}
