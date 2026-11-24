package risk

import (
	"errors"
	"testing"

	"sor/src/domain"
)

var limits = Limits{Symbols: []string{"XYZ"}, MaxParentQty: 10000, MaxParentNotionalMills: 5_000_000_000,
	MaxChildQty: 1000, PriceCollarBps: 50, MaxDurationSeconds: 600, LotSize: 100}

func code(err error) string {
	var r *Rejection
	if errors.As(err, &r) {
		return r.Code
	}
	return ""
}

func TestCheckParent(t *testing.T) {
	ok := domain.ParentOrder{Symbol: "XYZ", Side: domain.Buy, TargetQty: 1000, StartMs: 0, DeadlineMs: 60000, IntervalMs: 1000}
	mod := func(f func(*domain.ParentOrder)) domain.ParentOrder { p := ok; f(&p); return p }
	cases := []struct {
		p    domain.ParentOrder
		want string
	}{
		{ok, ""},
		{mod(func(p *domain.ParentOrder) { p.Symbol = "ABC" }), "SYMBOL_NOT_ALLOWED"},
		{mod(func(p *domain.ParentOrder) { p.Side = "HOLD" }), "BAD_SIDE"},
		{mod(func(p *domain.ParentOrder) { p.TargetQty = 0 }), "BAD_QTY"},
		{mod(func(p *domain.ParentOrder) { p.TargetQty = -100 }), "BAD_QTY"},
		{mod(func(p *domain.ParentOrder) { p.TargetQty = 150 }), "BAD_QTY"},
		{mod(func(p *domain.ParentOrder) { p.TargetQty = 10100 }), "MAX_PARENT_QTY"},
		{mod(func(p *domain.ParentOrder) { p.TargetQty = 10000 }), "MAX_NOTIONAL"}, // 10000 * 1,000,000 mills > 5e9
		{mod(func(p *domain.ParentOrder) { p.DeadlineMs = 0 }), "BAD_DURATION"},
		{mod(func(p *domain.ParentOrder) { p.DeadlineMs = 601000 }), "MAX_DURATION"},
	}
	g := New(limits)
	for _, c := range cases {
		if got := code(g.CheckParent(c.p, 1_000_000)); got != c.want {
			t.Errorf("%+v: got %q want %q", c.p, got, c.want)
		}
	}
	g.SetKill(true)
	if got := code(g.CheckParent(ok, 1_000_000)); got != "KILL_SWITCH" {
		t.Errorf("kill switch: got %q", got)
	}
}

func TestCollarAndCheckChild(t *testing.T) {
	g := New(limits)
	mid := int64(1_000_050) // 10000.5 ticks; 50 bps = 50.0025 ticks
	if px := g.CollarPx(domain.Buy, mid); px != 10050 {
		t.Errorf("buy collar = %d", px)
	}
	if px := g.CollarPx(domain.Sell, mid); px != 9951 {
		t.Errorf("sell collar = %d", px)
	}
	cases := []struct {
		side              domain.Side
		qty, limit, avail int64
		want              string
	}{
		{domain.Buy, 500, 10050, 500, ""},
		{domain.Buy, 500, 10051, 500, "PRICE_COLLAR"},
		{domain.Sell, 500, 9951, 500, ""},
		{domain.Sell, 500, 9950, 500, "PRICE_COLLAR"},
		{domain.Buy, 1100, 10000, 5000, "MAX_CHILD_QTY"},
		{domain.Buy, 500, 10000, 400, "OVERFILL_RISK"},
		{domain.Buy, 50, 10000, 400, "BAD_QTY"},
		{domain.Buy, 0, 10000, 400, "BAD_QTY"},
	}
	for _, c := range cases {
		if got := code(g.CheckChild(c.side, c.qty, c.limit, mid, c.avail)); got != c.want {
			t.Errorf("%+v: got %q want %q", c, got, c.want)
		}
	}
	g.SetKill(true)
	if got := code(g.CheckChild(domain.Buy, 500, 10000, mid, 500)); got != "KILL_SWITCH" {
		t.Errorf("kill switch: got %q", got)
	}
}
