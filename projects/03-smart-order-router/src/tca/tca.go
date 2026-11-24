// Package tca computes execution-quality reports. It is analytics, not the
// execution path, so derived ratios (bps, averages) are floats; all the
// underlying sums are exact integers in mills.
package tca

import (
	"math"
	"sort"

	"sor/src/domain"
)

type VenueStat struct {
	Venue      string  `json:"venue"`
	Children   int     `json:"children"`
	Rejects    int     `json:"rejects"`
	Timeouts   int     `json:"timeouts"`
	RejectRate float64 `json:"reject_rate"`
	FilledQty  int64   `json:"filled_qty"`
	AllocPct   float64 `json:"alloc_pct"` // share of filled quantity
	FeesMills  int64   `json:"fees_mills"`
}

// Baseline compares the router against the same order run through the same
// seeded simulation with a naive single-venue policy.
type Baseline struct {
	Venue                string  `json:"venue"`
	FilledQty            int64   `json:"filled_qty"`
	FillRatePct          float64 `json:"fill_rate_pct"`
	AvgFillPx            float64 `json:"avg_fill_px"`
	ShortfallBps         float64 `json:"implementation_shortfall_bps"`
	ShortfallMillsPerShr float64 `json:"shortfall_mills_per_share"`
	// Positive savings = the router was cheaper than the baseline.
	SavingsMillsPerShr float64 `json:"router_savings_mills_per_share"`
	SavingsBps         float64 `json:"router_savings_bps"`
}

type Report struct {
	Label              string              `json:"label"`
	ParentID           string              `json:"parent_id"`
	Symbol             string              `json:"symbol"`
	Side               domain.Side         `json:"side"`
	Strategy           string              `json:"strategy"`
	Status             domain.ParentStatus `json:"status"`
	TargetQty          int64               `json:"target_qty"`
	FilledQty          int64               `json:"filled_qty"`
	UnfilledQty        int64               `json:"unfilled_qty"`
	UnresolvedQty      int64               `json:"unresolved_qty"` // reserved by children with unknown outcome
	FillRatePct        float64             `json:"fill_rate_pct"`
	TimeToCompletionMs int64               `json:"time_to_completion_ms"` // 0 while working
	AvgFillPx          float64             `json:"avg_fill_px"`           // ticks
	NotionalMills      int64               `json:"notional_mills"`
	FeesMills          int64               `json:"fees_mills"`
	ArrivalMidMills    int64               `json:"arrival_mid_mills"`
	MarketVWAPMills    int64               `json:"market_vwap_mills"`
	// Implementation shortfall vs arrival mid, fees included; positive = cost.
	// Opportunity cost of unfilled quantity is not included.
	ShortfallMills       int64       `json:"implementation_shortfall_mills"`
	ShortfallBps         float64     `json:"implementation_shortfall_bps"`
	ShortfallMillsPerShr float64     `json:"shortfall_mills_per_share"`
	SlippageArrivalBps   float64     `json:"slippage_vs_arrival_bps"` // price only
	SlippageVWAPBps      float64     `json:"slippage_vs_vwap_bps"`
	EffectiveSpreadBps   float64     `json:"effective_spread_bps"`
	Children             int         `json:"children"`
	Venues               []VenueStat `json:"venues"`
	Baseline             *Baseline   `json:"baseline,omitempty"`
}

func round(x float64) float64 {
	if math.IsNaN(x) || math.IsInf(x, 0) {
		return 0
	}
	return math.Round(x*1e4) / 1e4
}

func ratio(a, b float64) float64 {
	if b == 0 {
		return 0
	}
	return a / b
}

// Build assembles the report for one parent. marketVWAPMills is the
// simulated market VWAP over the order's life (0 if no volume).
func Build(p domain.ParentOrder, children []domain.ChildOrder, fills []domain.Fill, marketVWAPMills int64) Report {
	r := Report{Label: "SIMULATED / PAPER", ParentID: p.ID, Symbol: p.Symbol, Side: p.Side, Strategy: p.Strategy,
		Status: p.Status, TargetQty: p.TargetQty, FilledQty: p.FilledQty, UnfilledQty: p.TargetQty - p.FilledQty,
		UnresolvedQty: p.ReservedQty, ArrivalMidMills: p.ArrivalMidMills, MarketVWAPMills: marketVWAPMills,
		Children: len(children), Venues: []VenueStat{}}
	if p.DoneMs > 0 {
		r.TimeToCompletionMs = p.DoneMs - p.StartMs
	}
	sign := p.Side.Sign()
	stats := map[string]*VenueStat{}
	stat := func(v string) *VenueStat {
		if stats[v] == nil {
			stats[v] = &VenueStat{Venue: v}
		}
		return stats[v]
	}
	refMid := map[string]int64{}
	for _, c := range children {
		s := stat(c.Venue)
		s.Children++
		if c.Status == domain.ChildRejected {
			s.Rejects++
		}
		if c.TimedOut {
			s.Timeouts++
		}
		refMid[c.ID] = c.RefMidMills
	}
	var spreadNum, spreadDen int64
	for _, f := range fills {
		pxMills := f.Px * domain.MillsPerTick
		r.NotionalMills += f.Qty * pxMills
		r.FeesMills += f.FeeMills
		s := stat(f.Venue)
		s.FilledQty += f.Qty
		s.FeesMills += f.FeeMills
		spreadNum += 2 * f.Qty * sign * (pxMills - refMid[f.ChildID])
		spreadDen += f.Qty * refMid[f.ChildID]
	}
	for _, s := range stats {
		s.RejectRate = round(ratio(float64(s.Rejects), float64(s.Children)))
		s.AllocPct = round(100 * ratio(float64(s.FilledQty), float64(p.FilledQty)))
		r.Venues = append(r.Venues, *s)
	}
	sort.Slice(r.Venues, func(i, j int) bool { return r.Venues[i].Venue < r.Venues[j].Venue })

	q := float64(p.FilledQty)
	arrivalNotional := p.FilledQty * p.ArrivalMidMills
	priceSlip := sign * (r.NotionalMills - arrivalNotional)
	r.ShortfallMills = priceSlip + r.FeesMills
	r.FillRatePct = round(100 * ratio(q, float64(p.TargetQty)))
	r.AvgFillPx = round(ratio(float64(r.NotionalMills), q*domain.MillsPerTick))
	r.ShortfallBps = round(1e4 * ratio(float64(r.ShortfallMills), float64(arrivalNotional)))
	r.ShortfallMillsPerShr = round(ratio(float64(r.ShortfallMills), q))
	r.SlippageArrivalBps = round(1e4 * ratio(float64(priceSlip), float64(arrivalNotional)))
	r.SlippageVWAPBps = round(1e4 * ratio(float64(sign*(r.NotionalMills-p.FilledQty*marketVWAPMills)), q*float64(marketVWAPMills)))
	r.EffectiveSpreadBps = round(1e4 * ratio(float64(spreadNum), float64(spreadDen)))
	return r
}

// Compare summarises a baseline run relative to the router's run of the same order.
func Compare(router, base Report, venue string) *Baseline {
	return &Baseline{Venue: venue, FilledQty: base.FilledQty, FillRatePct: base.FillRatePct, AvgFillPx: base.AvgFillPx,
		ShortfallBps: base.ShortfallBps, ShortfallMillsPerShr: base.ShortfallMillsPerShr,
		SavingsMillsPerShr: round(base.ShortfallMillsPerShr - router.ShortfallMillsPerShr),
		SavingsBps:         round(base.ShortfallBps - router.ShortfallBps)}
}
