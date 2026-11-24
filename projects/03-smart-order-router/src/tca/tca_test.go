package tca

import (
	"testing"

	"sor/src/domain"
)

func TestBuildBuy(t *testing.T) {
	p := domain.ParentOrder{ID: "P-1", Side: domain.Buy, TargetQty: 1000, FilledQty: 600, ReservedQty: 100,
		ArrivalMidMills: 10000, StartMs: 1000, DoneMs: 61000, Status: domain.ParentExpired}
	children := []domain.ChildOrder{
		{ID: "C-1", Venue: "A", RefMidMills: 10000, Status: domain.ChildFilled},
		{ID: "C-2", Venue: "B", RefMidMills: 10100, Status: domain.ChildPartial},
		{ID: "C-3", Venue: "B", Status: domain.ChildRejected},
		{ID: "C-4", Venue: "B", Status: domain.ChildTimedOut, TimedOut: true},
	}
	fills := []domain.Fill{
		{ChildID: "C-1", Venue: "A", Qty: 400, Px: 101, FeeMills: 400 * 30},
		{ChildID: "C-2", Venue: "B", Qty: 200, Px: 102, FeeMills: 200 * 10},
	}
	r := Build(p, children, fills, 10050)
	// notional = 400*10100 + 200*10200 = 6,080,000; fees = 14,000
	// shortfall = 6,080,000 - 600*10,000 + 14,000 = 94,000 mills
	checks := []struct {
		name      string
		got, want float64
	}{
		{"notional", float64(r.NotionalMills), 6_080_000},
		{"fees", float64(r.FeesMills), 14_000},
		{"shortfall", float64(r.ShortfallMills), 94_000},
		{"shortfall bps", r.ShortfallBps, 156.6667},
		{"shortfall/share", r.ShortfallMillsPerShr, 156.6667},
		{"slippage arrival bps", r.SlippageArrivalBps, 133.3333},
		{"slippage vwap bps", r.SlippageVWAPBps, 82.9187}, // (6,080,000 - 600*10050) / (600*10050)
		{"avg px", r.AvgFillPx, 101.3333},
		{"fill rate", r.FillRatePct, 60},
		// 2 * (400*(10100-10000) + 200*(10200-10100)) / (400*10000 + 200*10100)
		{"effective spread bps", r.EffectiveSpreadBps, 199.3355},
		{"unfilled", float64(r.UnfilledQty), 400},
		{"unresolved", float64(r.UnresolvedQty), 100},
		{"ttc", float64(r.TimeToCompletionMs), 60000},
	}
	for _, c := range checks {
		if c.got != c.want {
			t.Errorf("%s = %v, want %v", c.name, c.got, c.want)
		}
	}
	if len(r.Venues) != 2 || r.Venues[1].Venue != "B" {
		t.Fatalf("venues %+v", r.Venues)
	}
	if b := r.Venues[1]; b.Children != 3 || b.Rejects != 1 || b.Timeouts != 1 || b.RejectRate != 0.3333 || b.AllocPct != 33.3333 {
		t.Errorf("venue B stats %+v", b)
	}
}

func TestSellSignsAndEmptyReport(t *testing.T) {
	p := domain.ParentOrder{Side: domain.Sell, TargetQty: 100, FilledQty: 100, ArrivalMidMills: 10000}
	r := Build(p, []domain.ChildOrder{{ID: "C-1", Venue: "A", RefMidMills: 10000}},
		[]domain.Fill{{ChildID: "C-1", Venue: "A", Qty: 100, Px: 99, FeeMills: -500}}, 10000)
	// Sold 1 tick below arrival (cost 10,000 mills) less a 500 mill rebate.
	if r.ShortfallMills != 9500 || r.SlippageArrivalBps != 100 || r.EffectiveSpreadBps != 200 {
		t.Errorf("sell report %+v", r)
	}
	empty := Build(domain.ParentOrder{Side: domain.Buy, TargetQty: 100}, nil, nil, 0)
	if empty.ShortfallBps != 0 || empty.AvgFillPx != 0 || empty.FillRatePct != 0 {
		t.Errorf("empty report has non-zero ratios: %+v", empty)
	}
	base := r
	base.ShortfallBps, base.ShortfallMillsPerShr = 120, 120
	if c := Compare(r, base, "A"); c.SavingsBps != 25 || c.SavingsMillsPerShr != 25 {
		t.Errorf("compare %+v", c)
	}
}
