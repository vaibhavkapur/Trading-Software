package unit

import (
	"testing"

	"riskengine/src/domain"
	"riskengine/src/fixed"
	"riskengine/tests/harness"
)

// position runs fills through the real engine and returns the resulting
// position and realized P&L for a-big/XYZ.
func position(t *testing.T, fills ...[3]string) (qty int64, basis, pnl string) {
	t.Helper()
	e := harness.New(t)
	for i, f := range fills {
		e.Clock.Advance(100_000_000) // 100ms: stay under the rate limit
		e.Quote("XYZ", f[2], f[2])
		id := string(rune('a' + i))
		qtyN := map[string]int64{"100": 100, "40": 40, "60": 60, "150": 150, "50": 50, "3": 3, "1": 1, "2": 2}[f[1]]
		d := e.Eval(id, "a-big", "XYZ", f[0], qtyN, f[2])
		if d.Decision != domain.Allow {
			t.Fatalf("fill %d not allowed: %v", i, d.ReasonCodes)
		}
		e.Fill(id, "f1", qtyN, f[2])
	}
	st := e.App.Engine.AccountState("a-big")
	p := st.Positions["XYZ"]
	return p.Qty, p.CostBasis.String(), st.RealizedPnLToday.String()
}

func TestPositionAndRealizedPnL(t *testing.T) {
	for _, c := range []struct {
		name  string
		fills [][3]string // side, qty, price
		qty   int64
		basis string
		pnl   string
	}{
		{"open long", [][3]string{{"BUY", "100", "10.00"}}, 100, "1000.0000", "0.0000"},
		{"add to long averages cost", [][3]string{{"BUY", "100", "10.00"}, {"BUY", "100", "12.00"}}, 200, "2200.0000", "0.0000"},
		{"partial close realizes gain", [][3]string{{"BUY", "100", "10.00"}, {"SELL", "40", "10.50"}}, 60, "600.0000", "20.0000"},
		{"full close realizes loss", [][3]string{{"BUY", "100", "10.00"}, {"SELL", "100", "9.50"}}, 0, "0.0000", "-50.0000"},
		{"flip long to short", [][3]string{{"BUY", "100", "10.00"}, {"SELL", "150", "10.20"}}, -50, "-510.0000", "20.0000"},
		{"short covered at a loss", [][3]string{{"SELL", "100", "10.00"}, {"BUY", "100", "10.40"}}, 0, "0.0000", "-40.0000"},
		{"short covered at a gain", [][3]string{{"SELL", "100", "10.00"}, {"BUY", "60", "9.80"}}, -40, "-400.0000", "12.0000"},
		// 3 @ 10.00 = 30.00 basis; selling 1 closes 10.0000 exactly, and
		// the remaining basis must still sum to the original.
		{"pro-rata basis does not leak", [][3]string{{"BUY", "3", "10.00"}, {"SELL", "1", "10.00"}, {"SELL", "2", "10.00"}}, 0, "0.0000", "0.0000"},
	} {
		qty, basis, pnl := position(t, c.fills...)
		if qty != c.qty || basis != c.basis || pnl != c.pnl {
			t.Errorf("%s: qty=%d basis=%s pnl=%s; want %d %s %s", c.name, qty, basis, pnl, c.qty, c.basis, c.pnl)
		}
	}
}

func TestFillLifecycle(t *testing.T) {
	e := harness.New(t)
	e.Quote("XYZ", "100", "100")
	d := e.Expect(domain.Allow, nil, "o1", "a-big", "XYZ", "BUY", 100, "100.00")
	if r := d.Reservation; r == nil || r.Status != domain.Reserved || r.Notional != fixed.MustParse("10000") {
		t.Fatalf("reservation = %+v", r)
	}

	// Partial fill: remaining capacity shrinks and the order is live.
	res, err := e.App.Engine.Fill("o1", "f1", 30, fixed.MustParse("99.50"))
	if err != nil || res.Reservation.Remaining != 70 || res.Reservation.Status != domain.Committed ||
		res.Reservation.Notional != fixed.MustParse("7000") || res.Position.Qty != 30 {
		t.Fatalf("after partial fill: %+v err=%v", res, err)
	}
	// The same fill delivered again must not double-count.
	res, _ = e.App.Engine.Fill("o1", "f1", 30, fixed.MustParse("99.50"))
	if !res.Duplicate || res.Position.Qty != 30 {
		t.Fatalf("duplicate fill changed state: %+v", res)
	}
	// Final fill releases the reservation.
	res, _ = e.App.Engine.Fill("o1", "f2", 70, fixed.MustParse("100"))
	if res.Reservation.Status != domain.Released || res.Reservation.ReleaseReason != "FILLED" || res.Position.Qty != 100 {
		t.Fatalf("after final fill: %+v", res)
	}
	if n := len(e.App.Engine.AccountState("a-big").Reservations); n != 0 {
		t.Fatalf("%d reservations still open", n)
	}
	// An over-fill is still booked, and flagged.
	res, _ = e.App.Engine.Fill("o1", "f3", 5, fixed.MustParse("100"))
	if !res.Unreserved || res.Position.Qty != 105 || e.App.Metrics.Counter("risk_unreserved_fills_total") != 1 {
		t.Fatalf("over-fill: %+v", res)
	}
}
