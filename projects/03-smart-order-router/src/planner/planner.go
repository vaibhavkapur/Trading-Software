// Package planner turns (want quantity, frozen market snapshot, policy) into
// child-order allocations. Plan is a pure function of its Input: the same
// Input always yields the same RouteDecision, which is what makes recorded
// decisions replayable.
package planner

import (
	"slices"
	"sort"
	"strings"

	"sor/src/domain"
	"sor/src/marketview"
)

const DefaultVersion = "cost-aware-v1"

type Policy struct {
	Version string `json:"version"`
	// ParticipationPct caps each child at this % of the displayed size of
	// every price level it targets.
	ParticipationPct int64 `json:"participation_pct"`
	MaxChildQty      int64 `json:"max_child_qty"` // per venue per decision; 0 = no cap
	// Penalties convert latency and historical fill quality into mills/share
	// so they can be traded off against price and fees.
	LatencyPenaltyMillsPer10Ms int64 `json:"latency_penalty_mills_per_10ms"`
	QualityPenaltyMills        int64 `json:"quality_penalty_mills"`
	LotSize                    int64 `json:"lot_size"`
	// SingleVenue restricts routing to one venue (the naive TCA baseline).
	SingleVenue string `json:"single_venue,omitempty"`
}

type Input struct {
	ParentID  string              `json:"parent_id"`
	Side      domain.Side         `json:"side"`
	WantQty   int64               `json:"want_qty"`
	LimitPx   int64               `json:"limit_px"`   // risk price collar; 0 = none
	MaxLevels int                 `json:"max_levels"` // book levels per venue; 0 = all
	DecidedMs int64               `json:"decided_ms"`
	Exclude   []string            `json:"exclude"` // venues that failed earlier in this slice
	Policy    Policy              `json:"policy"`
	Snapshot  marketview.Snapshot `json:"snapshot"`
}

type candidate struct {
	venue           string
	px, avail, cost int64
}

func Plan(in Input) domain.RouteDecision {
	pol := in.Policy
	lot := max(pol.LotSize, 1)
	sign := in.Side.Sign()
	d := domain.RouteDecision{ParentID: in.ParentID, SnapshotID: in.Snapshot.ID, PolicyVersion: pol.Version,
		DecidedMs: in.DecidedMs, WantQty: in.WantQty, Allocations: []domain.Allocation{}, Trace: []domain.TraceEntry{}}
	skip := func(venue, reason string) {
		d.Trace = append(d.Trace, domain.TraceEntry{Venue: venue, Reason: reason})
	}

	var cands []candidate
	for _, vs := range in.Snapshot.Venues {
		switch {
		case pol.SingleVenue != "" && vs.Venue != pol.SingleVenue:
			skip(vs.Venue, "skipped: policy routes to single venue "+pol.SingleVenue)
			continue
		case slices.Contains(in.Exclude, vs.Venue):
			skip(vs.Venue, "skipped: failed earlier in this slice")
			continue
		case vs.Status != marketview.OK:
			skip(vs.Venue, "skipped: venue status "+string(vs.Status))
			continue
		}
		levels := vs.Asks
		if in.Side == domain.Sell {
			levels = vs.Bids
		}
		if in.MaxLevels > 0 && len(levels) > in.MaxLevels {
			levels = levels[:in.MaxLevels]
		}
		penalty := vs.LatencyMs*pol.LatencyPenaltyMillsPer10Ms/10 +
			(10000-vs.FillRatioBps)*pol.QualityPenaltyMills/10000
		for _, l := range levels {
			if in.LimitPx != 0 && sign*(l.Px-in.LimitPx) > 0 {
				d.Trace = append(d.Trace, domain.TraceEntry{Venue: vs.Venue, Px: l.Px, Reason: "skipped: outside risk price collar"})
				break
			}
			avail := l.Qty * pol.ParticipationPct / 100 / lot * lot
			if avail <= 0 {
				continue
			}
			cands = append(cands, candidate{venue: vs.Venue, px: l.Px, avail: avail,
				cost: sign*l.Px*domain.MillsPerTick + vs.TakerFeeMills + penalty})
		}
	}
	// Cheapest all-in cost first; ties broken deterministically.
	sort.Slice(cands, func(i, j int) bool {
		a, b := cands[i], cands[j]
		if a.cost != b.cost {
			return a.cost < b.cost
		}
		if a.avail != b.avail {
			return a.avail > b.avail
		}
		if c := strings.Compare(a.venue, b.venue); c != 0 {
			return c < 0
		}
		return sign*a.px < sign*b.px
	})

	remaining := in.WantQty / lot * lot
	alloc := map[string]*domain.Allocation{}
	for _, c := range cands {
		e := domain.TraceEntry{Venue: c.venue, Px: c.px, AvailQty: c.avail, CostMills: c.cost}
		take := min(c.avail, remaining)
		a := alloc[c.venue]
		if pol.MaxChildQty > 0 {
			used := int64(0)
			if a != nil {
				used = a.Qty
			}
			take = min(take, (pol.MaxChildQty-used)/lot*lot)
		}
		switch {
		case remaining == 0:
			e.Reason = "not needed: quantity satisfied at lower cost"
		case take <= 0:
			e.Reason = "skipped: venue child size cap reached"
		default:
			e.TakenQty, e.Reason = take, "taken: lowest remaining all-in cost"
			if take < min(c.avail, remaining) {
				e.Reason = "taken: limited by venue child size cap"
			}
			if a == nil {
				a = &domain.Allocation{Venue: c.venue}
				alloc[c.venue] = a
			}
			a.Qty += take
			a.LimitPx = c.px // candidates of one venue arrive best-first, so this is the worst level taken
			remaining -= take
		}
		d.Trace = append(d.Trace, e)
	}
	for _, a := range alloc {
		d.Allocations = append(d.Allocations, *a)
	}
	sort.Slice(d.Allocations, func(i, j int) bool { return d.Allocations[i].Venue < d.Allocations[j].Venue })
	return d
}
