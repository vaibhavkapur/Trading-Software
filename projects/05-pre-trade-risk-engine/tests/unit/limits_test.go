package unit

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"riskengine/src/domain"
	"riskengine/tests/harness"
)

const (
	allow  = domain.Allow
	reject = domain.Reject
	review = domain.Review
)

func r(codes ...string) []string { return codes }

// Each case probes one limit at its boundary: the largest value that must
// pass and the smallest that must not. Account a-big: 1000 qty, 100,000
// order notional, 150,000 per symbol, 250,000 gross, 5,000 daily loss.
func TestOrderSizeBoundaries(t *testing.T) {
	e := harness.New(t)
	e.Quote("XYZ", "100", "100")
	e.Expect(allow, nil, "qty-at", "a-big", "XYZ", "BUY", 1000, "100.00") // both limits exactly
	e.App.Engine.Release("qty-at", "")
	e.Expect(reject, r(domain.MaxOrderQty, domain.MaxOrderNotional), "qty-over", "a-big", "XYZ", "BUY", 1001, "100.00")
	e.Expect(reject, r(domain.MaxOrderNotional), "ntl-over", "a-big", "XYZ", "BUY", 1000, "100.01")

	e.Quote("XYZ", "50", "50")
	e.Expect(reject, r(domain.MaxOrderQty), "qty-only", "a-big", "XYZ", "BUY", 1001, "50.00")
}

func TestValidationRejects(t *testing.T) {
	e := harness.New(t)
	e.Quote("XYZ", "100", "100")
	ok := harness.Req("v", "a-big", "XYZ", "BUY", 10, "100.00")
	for name, c := range map[string]struct {
		mut  func(*domain.RiskRequest)
		want []string
	}{
		"empty order id":   {func(q *domain.RiskRequest) { q.OrderID = "" }, r(domain.InvalidOrderID)},
		"order id charset": {func(q *domain.RiskRequest) { q.OrderID = "o 1\n" }, r(domain.InvalidOrderID)},
		"long order id":    {func(q *domain.RiskRequest) { q.OrderID = strings.Repeat("x", 65) }, r(domain.InvalidOrderID)},
		"empty account":    {func(q *domain.RiskRequest) { q.AccountID = "" }, r(domain.InvalidAccountID)},
		"bad side":         {func(q *domain.RiskRequest) { q.Side = "buy" }, r(domain.InvalidSide)},
		"zero qty":         {func(q *domain.RiskRequest) { q.Qty = 0 }, r(domain.InvalidQty)},
		"negative qty":     {func(q *domain.RiskRequest) { q.Qty = -5 }, r(domain.InvalidQty)},
		"huge qty":         {func(q *domain.RiskRequest) { q.Qty = 1_000_000_001 }, r(domain.InvalidQty)},
		"missing price":    {func(q *domain.RiskRequest) { q.LimitPrice = nil }, r(domain.InvalidPrice)},
		"zero price":       {func(q *domain.RiskRequest) { *q.LimitPrice = 0 }, r(domain.InvalidPrice)},
		"negative price":   {func(q *domain.RiskRequest) { *q.LimitPrice = -1 }, r(domain.InvalidPrice)},
		"everything wrong": {func(q *domain.RiskRequest) { *q = domain.RiskRequest{} },
			r(domain.InvalidOrderID, domain.InvalidAccountID, domain.InvalidSymbol, domain.InvalidSide, domain.InvalidQty, domain.InvalidPrice)},
		"unknown account": {func(q *domain.RiskRequest) { q.AccountID = "ghost" }, r(domain.UnknownAccount)},
		"unknown symbol":  {func(q *domain.RiskRequest) { q.Symbol = "NOPE" }, r(domain.UnknownSymbol)},
	} {
		req := ok
		p := *ok.LimitPrice
		req.LimitPrice = &p
		c.mut(&req)
		d := e.App.Engine.Evaluate(req)
		harness.AssertDecision(t, d, reject, c.want...)
		if d.PolicyVersion != "test-1" || d.Seq == 0 || d.SnapshotHash == "" || len(d.Trace) == 0 {
			t.Errorf("%s: reject lacks evidence: %+v", name, d)
		}
		if d.Reservation != nil {
			t.Errorf("%s: a rejected order reserved capacity", name)
		}
	}
}

func TestInstrumentRules(t *testing.T) {
	e := harness.New(t)
	e.Quote("ABC", "100", "100")
	e.Quote("HALT", "100", "100")
	e.Expect(allow, nil, "lot-ok", "a-big", "ABC", "BUY", 20, "100.05")
	e.Expect(reject, r(domain.LotSizeViolation), "lot-bad", "a-big", "ABC", "BUY", 15, "100.05")
	e.Expect(reject, r(domain.TickSizeViolation), "tick-bad", "a-big", "ABC", "BUY", 20, "100.02")
	e.Expect(reject, r(domain.InstrumentNotTrading), "halted", "a-big", "HALT", "BUY", 1, "100.00")
}

func TestPriceCollarBoundaries(t *testing.T) {
	e := harness.New(t)
	e.Quote("XYZ", "100", "100") // account collar: 500bps
	e.Quote("ABC", "100", "100") // symbol override: 200bps
	e.Expect(allow, nil, "c1", "a-big", "XYZ", "BUY", 1, "105.00")
	e.Expect(reject, r(domain.PriceCollar), "c2", "a-big", "XYZ", "BUY", 1, "105.01")
	e.Expect(allow, nil, "c3", "a-big", "XYZ", "SELL", 1, "95.00")
	e.Expect(reject, r(domain.PriceCollar), "c4", "a-big", "XYZ", "SELL", 1, "94.99")
	e.Expect(allow, nil, "c5", "a-big", "ABC", "BUY", 10, "102.00")
	e.Expect(reject, r(domain.PriceCollar), "c6", "a-big", "ABC", "BUY", 10, "102.05")
}

func TestQuoteFreshnessBoundaries(t *testing.T) {
	e := harness.New(t)
	e.Expect(reject, r(domain.NoMarketData), "q0", "a-big", "XYZ", "BUY", 1, "100.00")
	e.Quote("XYZ", "100", "100")
	e.Clock.Advance(2000 * time.Millisecond)
	e.Expect(allow, nil, "q1", "a-big", "XYZ", "BUY", 1, "100.00") // exactly max age
	e.Clock.Advance(time.Millisecond)
	d := e.Expect(reject, r(domain.StaleQuote), "q2", "a-big", "XYZ", "BUY", 1, "100.00")
	// With no usable quote the dependent checks are reported as skipped,
	// not silently passed.
	skipped := 0
	for _, c := range d.Trace {
		if c.Outcome == "SKIPPED" {
			skipped++
		}
	}
	if skipped != 3 {
		t.Errorf("want collar and both exposure checks SKIPPED, trace: %s", harness.JSON(d.Trace))
	}
}

func TestRateLimitSlidingWindow(t *testing.T) {
	e := harness.New(t)
	e.Quote("XYZ", "100", "100")
	for i := 0; i < 20; i++ { // a-big: 20 per 1000ms
		e.Expect(allow, nil, fmt.Sprint("r", i), "a-big", "XYZ", "BUY", 1, "100.00")
		e.Clock.Advance(10 * time.Millisecond)
	}
	e.Expect(reject, r(domain.RateLimit), "r-over", "a-big", "XYZ", "BUY", 1, "100.00")
	// Another account is unaffected.
	e.Expect(allow, nil, "r-other", "a-1", "XYZ", "BUY", 1, "100.00")
	// The rejected order at t=200ms counts toward the rate as well, so the
	// window only has room again once two of the early orders have left it.
	e.Clock.Advance(800 * time.Millisecond) // t=1000ms: window holds t=10..200, still 20
	e.Quote("XYZ", "100", "100")
	e.Expect(reject, r(domain.RateLimit), "r-full", "a-big", "XYZ", "BUY", 1, "100.00")
	e.Clock.Advance(20 * time.Millisecond) // t=1020ms: t=10 and t=20 have left
	e.Expect(allow, nil, "r-next", "a-big", "XYZ", "BUY", 1, "100.00")
	e.Expect(reject, r(domain.RateLimit), "r-again", "a-big", "XYZ", "BUY", 1, "100.00")
}

func TestSymbolExposureBoundary(t *testing.T) {
	e := harness.New(t)
	e.Quote("XYZ", "100", "100")
	e.Expect(allow, nil, "s1", "a-big", "XYZ", "BUY", 1000, "100.00") // 100,000 reserved
	e.Expect(allow, nil, "s2", "a-big", "XYZ", "BUY", 500, "100.00")  // 150,000: exactly the limit
	e.Expect(reject, r(domain.MaxSymbolExposure), "s3", "a-big", "XYZ", "BUY", 1, "100.00")
	// Filled quantity counts the same as reserved quantity.
	e.Fill("s1", "f", 1000, "100.00")
	e.Expect(reject, r(domain.MaxSymbolExposure), "s4", "a-big", "XYZ", "BUY", 1, "100.00")
	// Releasing unfilled capacity frees it.
	e.App.Engine.Release("s2", "cancelled")
	e.Expect(allow, nil, "s5", "a-big", "XYZ", "BUY", 500, "100.00")
}

func TestExposureIsWorstCaseAcrossOpenOrders(t *testing.T) {
	e := harness.New(t)
	e.Quote("XYZ", "100", "100")
	// Long 1000 (100,000). Resting sells do not earn headroom for buys,
	// because the sells might never fill.
	e.Expect(allow, nil, "w1", "a-big", "XYZ", "BUY", 1000, "100.00")
	e.Fill("w1", "f", 1000, "100.00")
	e.Expect(allow, nil, "w2", "a-big", "XYZ", "SELL", 1000, "100.00")
	e.Expect(allow, nil, "w3", "a-big", "XYZ", "BUY", 500, "100.00")
	e.Expect(reject, r(domain.MaxSymbolExposure), "w4", "a-big", "XYZ", "BUY", 1, "100.00")
}

func TestOverLimitAccountCanStillReduce(t *testing.T) {
	e := harness.New(t)
	e.Quote("XYZ", "100", "100")
	e.Expect(allow, nil, "d1", "a-big", "XYZ", "BUY", 1000, "100.00")
	e.Fill("d1", "f1", 1000, "100.00")
	e.Expect(allow, nil, "d2", "a-big", "XYZ", "BUY", 500, "100.00")
	e.Fill("d2", "f1", 500, "100.00")
	// The market rallies 60%: 1500 shares are now 240,000, far over 150,000.
	e.Clock.Advance(time.Second)
	e.Quote("XYZ", "160", "160")
	e.Expect(reject, r(domain.MaxSymbolExposure), "d3", "a-big", "XYZ", "BUY", 1, "160.00")
	e.Expect(allow, nil, "d4", "a-big", "XYZ", "SELL", 500, "160.00") // still over after it fills, but smaller
}

func TestGrossExposureBoundary(t *testing.T) {
	e := harness.New(t)
	e.Quote("XYZ", "100", "100")
	e.Quote("ABC", "100", "100")
	e.Expect(allow, nil, "g1", "a-big", "XYZ", "BUY", 1000, "100.00")
	e.Expect(allow, nil, "g2", "a-big", "XYZ", "BUY", 500, "100.00")   // XYZ 150,000
	e.Expect(allow, nil, "g3", "a-big", "ABC", "SELL", 1000, "100.00") // gross 250,000: exactly the limit
	e.Expect(reject, r(domain.MaxGrossExposure), "g4", "a-big", "ABC", "SELL", 10, "100.00")
	// A buy against the resting sell cannot raise the worst case: only one
	// side can end up as the position. It becomes a breach once it would.
	e.Expect(allow, nil, "g5", "a-big", "ABC", "BUY", 1000, "100.00")
	e.Expect(reject, r(domain.MaxGrossExposure), "g6", "a-big", "ABC", "BUY", 10, "100.00")
}

func TestDailyLossBoundaryAndRollover(t *testing.T) {
	lose := func(exit string) *harness.Env {
		e := harness.New(t)
		e.Quote("XYZ", "100", "100")
		e.Expect(allow, nil, "l1", "a-big", "XYZ", "BUY", 1000, "100.00")
		e.Fill("l1", "f", 1000, "100.00")
		e.Clock.Advance(time.Second)
		e.Quote("XYZ", exit, exit)
		e.Expect(allow, nil, "l2", "a-big", "XYZ", "SELL", 1000, exit)
		e.Fill("l2", "f", 1000, exit)
		return e
	}
	e := lose("95.01") // -4,990: under the 5,000 limit
	e.Expect(allow, nil, "l3", "a-big", "XYZ", "BUY", 1, "95.01")

	e = lose("95.00") // -5,000: at the limit, trading stops
	e.Expect(reject, r(domain.MaxDailyLoss), "l3", "a-big", "XYZ", "BUY", 1, "95.00")
	e.Expect(allow, nil, "l3-other", "a-1", "XYZ", "BUY", 1, "95.00") // per account

	// The limit is per UTC day.
	e.Clock.Set(harness.T0.Add(24 * time.Hour))
	e.Quote("XYZ", "95", "95")
	e.Expect(allow, nil, "l4", "a-big", "XYZ", "BUY", 1, "95.00")
}

func TestReviewThresholdHoldsCapacity(t *testing.T) {
	e := harness.New(t)
	e.Quote("XYZ", "100", "100")
	e.Expect(allow, nil, "v1", "a-1", "XYZ", "BUY", 500, "100.00") // 50,000: at threshold
	e.App.Engine.Release("v1", "")
	d := e.Expect(review, r(domain.ReviewOrderNotional), "v2", "a-1", "XYZ", "BUY", 501, "100.00")
	if d.Reservation == nil || d.Reservation.Status != domain.PendingReview {
		t.Fatalf("REVIEW must hold capacity: %+v", d.Reservation)
	}
	// A hard breach always wins over a review trigger.
	e.App.Engine.Release("v2", "review declined")
	e.Expect(reject, r(domain.MaxOrderQty, domain.MaxOrderNotional), "v3", "a-1", "XYZ", "BUY", 1001, "100.00")
}

func TestSessionClosed(t *testing.T) {
	doc := strings.Replace(harness.DefaultPolicy, `"symbols"`,
		`"session": {"open_utc":"13:30","close_utc":"20:00","weekdays":[1,2,3,4,5]}, "symbols"`, 1)
	e := harness.NewWith(t, doc)
	e.Quote("XYZ", "100", "100")
	e.Expect(allow, nil, "h1", "a-big", "XYZ", "BUY", 1, "100.00") // Monday 15:00Z
	e.Clock.Set(harness.T0.Add(5 * time.Hour))
	e.Quote("XYZ", "100", "100")
	e.Expect(reject, r(domain.SessionClosed), "h2", "a-big", "XYZ", "BUY", 1, "100.00")
}

// Every decision, whatever its outcome, names the policy it was made under
// and carries a trace in which each reason code appears.
func TestEveryDecisionIsExplained(t *testing.T) {
	e := harness.New(t)
	e.Quote("XYZ", "100", "100")
	for _, d := range []domain.RiskDecision{
		e.Eval("x1", "a-1", "XYZ", "BUY", 10, "100.00"),
		e.Eval("x2", "a-1", "XYZ", "BUY", 600, "100.00"),
		e.Eval("x3", "a-1", "XYZ", "BUY", 5000, "120.00"),
	} {
		if d.PolicyVersion != "test-1" || len(d.PolicyHash) != 64 || len(d.SnapshotHash) != 64 || d.Seq == 0 || d.Signature == "" {
			t.Errorf("%s: missing evidence fields: %s", d.OrderID, harness.JSON(d))
		}
		inTrace := map[string]bool{}
		for _, c := range d.Trace {
			inTrace[c.Code] = true
		}
		for _, code := range d.ReasonCodes {
			if !inTrace[code] {
				t.Errorf("%s: reason %s has no trace entry", d.OrderID, code)
			}
		}
		if len(d.Trace) == 0 {
			t.Errorf("%s: empty trace", d.OrderID)
		}
	}
}
