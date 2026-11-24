package planner

import (
	"encoding/json"
	"math/rand/v2"
	"reflect"
	"testing"

	"sor/src/domain"
	"sor/src/marketview"
)

func venue(name string, fee int64, bid, ask, qty int64) marketview.VenueState {
	return marketview.VenueState{Venue: name, Status: marketview.OK, TakerFeeMills: fee, FillRatioBps: 10000,
		Bids: []domain.Level{{Px: bid, Qty: qty}, {Px: bid - 1, Qty: qty}},
		Asks: []domain.Level{{Px: ask, Qty: qty}, {Px: ask + 1, Qty: qty}}}
}

func input(side domain.Side, want int64, venues ...marketview.VenueState) Input {
	return Input{ParentID: "P-1", Side: side, WantQty: want,
		Policy:   Policy{Version: "t", ParticipationPct: 100, LotSize: 100},
		Snapshot: marketview.Snapshot{ID: "S-1", Venues: venues}}
}

func allocs(d domain.RouteDecision) map[string]domain.Allocation {
	m := map[string]domain.Allocation{}
	for _, a := range d.Allocations {
		m[a.Venue] = a
	}
	return m
}

func TestLowerFeeWinsAtEqualPrice(t *testing.T) {
	d := Plan(input(domain.Buy, 500, venue("A", 30, 99, 100, 1000), venue("B", 10, 99, 100, 1000)))
	if got := allocs(d); len(got) != 1 || got["B"].Qty != 500 || got["B"].LimitPx != 100 {
		t.Fatalf("want all 500 on B@100, got %+v", d.Allocations)
	}
}

func TestBetterPriceBeatsRebate(t *testing.T) {
	// A: 100 ticks + 30 mills = 10030; B: 101 ticks - 5 mills = 10095.
	d := Plan(input(domain.Buy, 500, venue("A", 30, 99, 100, 1000), venue("B", -5, 100, 101, 1000)))
	if got := allocs(d); got["A"].Qty != 500 || got["B"].Qty != 0 {
		t.Fatalf("want A, got %+v", d.Allocations)
	}
}

func TestSpillsAcrossVenuesAndLevelsInCostOrder(t *testing.T) {
	d := Plan(input(domain.Buy, 2500, venue("A", 30, 99, 100, 1000), venue("B", 10, 99, 100, 1000)))
	got := allocs(d)
	// B@100 (10010), A@100 (10030), then B@101 (10110) for the last 500.
	if got["B"].Qty != 1500 || got["B"].LimitPx != 101 || got["A"].Qty != 1000 || got["A"].LimitPx != 100 {
		t.Fatalf("got %+v", d.Allocations)
	}
}

func TestParticipationAndChildCaps(t *testing.T) {
	in := input(domain.Buy, 5000, venue("A", 0, 99, 100, 1050))
	in.Policy.ParticipationPct = 30 // 315 -> 300 per level after lot rounding
	if d := Plan(in); allocs(d)["A"].Qty != 600 {
		t.Fatalf("participation cap: got %+v", d.Allocations)
	}
	in.Policy.MaxChildQty = 400
	if d := Plan(in); allocs(d)["A"].Qty != 400 {
		t.Fatalf("child cap: got %+v", d.Allocations)
	}
}

func TestUnhealthyExcludedAndSingleVenue(t *testing.T) {
	stale := venue("A", 0, 99, 100, 1000)
	stale.Status = marketview.Stale
	in := input(domain.Buy, 500, stale, venue("B", 50, 99, 100, 1000), venue("C", 10, 99, 100, 1000))
	in.Exclude = []string{"C"}
	if got := allocs(Plan(in)); len(got) != 1 || got["B"].Qty != 500 {
		t.Fatalf("want only B, got %+v", got)
	}
	in.Exclude = nil
	in.Policy.SingleVenue = "B"
	if got := allocs(Plan(in)); len(got) != 1 || got["B"].Qty != 500 {
		t.Fatalf("single venue: got %+v", got)
	}
	in.Policy.SingleVenue = "A" // baseline venue is stale: route nothing rather than elsewhere
	if d := Plan(in); len(d.Allocations) != 0 {
		t.Fatalf("want no allocations, got %+v", d.Allocations)
	}
}

func TestSellUsesBidsAndCollar(t *testing.T) {
	in := input(domain.Sell, 1500, venue("A", 10, 100, 101, 1000), venue("B", 10, 99, 100, 1000))
	in.LimitPx = 99 // may not sell below 99
	got := allocs(Plan(in))
	// A@100, then A@99 and B@99 tie on cost and size: venue name breaks the tie.
	if got["A"].Qty != 1500 || got["A"].LimitPx != 99 || got["B"].Qty != 0 {
		t.Fatalf("got %+v", got)
	}
	in.WantQty = 5000
	for _, a := range Plan(in).Allocations {
		if a.LimitPx < 99 {
			t.Fatalf("allocation below collar: %+v", a)
		}
	}
}

func TestLatencyAndQualityPenalties(t *testing.T) {
	slow := venue("A", 0, 99, 100, 1000)
	slow.LatencyMs = 100
	in := input(domain.Buy, 500, slow, venue("B", 5, 99, 100, 1000))
	in.Policy.LatencyPenaltyMillsPer10Ms = 1 // A pays 10 mills for latency > B's 5 mill fee
	if got := allocs(Plan(in)); got["B"].Qty != 500 {
		t.Fatalf("latency: got %+v", got)
	}
	flaky := venue("A", 0, 99, 100, 1000)
	flaky.FillRatioBps = 5000
	in = input(domain.Buy, 500, flaky, venue("B", 5, 99, 100, 1000))
	in.Policy.QualityPenaltyMills = 50 // A pays 25 mills for poor fill quality
	if got := allocs(Plan(in)); got["B"].Qty != 500 {
		t.Fatalf("quality: got %+v", got)
	}
}

func TestMaxLevels(t *testing.T) {
	in := input(domain.Buy, 5000, venue("A", 0, 99, 100, 1000))
	in.MaxLevels = 1
	if got := allocs(Plan(in)); got["A"].Qty != 1000 || got["A"].LimitPx != 100 {
		t.Fatalf("got %+v", got)
	}
}

// Property: for random books the plan never exceeds want, respects lots,
// caps and collar, and is reproducible from its JSON-serialized input.
func TestPlanProperties(t *testing.T) {
	rng := rand.New(rand.NewPCG(1, 2))
	for i := 0; i < 500; i++ {
		side := domain.Buy
		if rng.IntN(2) == 0 {
			side = domain.Sell
		}
		var vs []marketview.VenueState
		for _, n := range []string{"A", "B", "C"} {
			v := venue(n, rng.Int64N(60)-20, 95+rng.Int64N(5), 100+rng.Int64N(5), 100*rng.Int64N(20))
			v.LatencyMs, v.FillRatioBps = rng.Int64N(200), rng.Int64N(10001)
			vs = append(vs, v)
		}
		in := input(side, 100*rng.Int64N(50), vs...)
		in.Policy = Policy{Version: "t", ParticipationPct: 1 + rng.Int64N(100), MaxChildQty: 100 * rng.Int64N(20),
			LatencyPenaltyMillsPer10Ms: 1, QualityPenaltyMills: 50, LotSize: 100}
		in.LimitPx = 98 + rng.Int64N(6)
		in.Exclude = []string{}
		d := Plan(in)
		var total int64
		for _, a := range d.Allocations {
			total += a.Qty
			if a.Qty <= 0 || a.Qty%100 != 0 {
				t.Fatalf("bad qty %+v", a)
			}
			if in.Policy.MaxChildQty > 0 && a.Qty > in.Policy.MaxChildQty {
				t.Fatalf("child cap exceeded %+v", a)
			}
			if side.Sign()*(a.LimitPx-in.LimitPx) > 0 {
				t.Fatalf("collar breached %+v limit %d", a, in.LimitPx)
			}
		}
		if total > in.WantQty {
			t.Fatalf("allocated %d > want %d", total, in.WantQty)
		}
		var back Input
		b, _ := json.Marshal(in)
		if err := json.Unmarshal(b, &back); err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(d, Plan(back)) {
			t.Fatalf("decision not reproducible from serialized input")
		}
	}
}

// BenchmarkPlan measures one routing decision over 3 venues x 5 levels.
func BenchmarkPlan(b *testing.B) {
	var vs []marketview.VenueState
	for i, n := range []string{"A", "B", "C"} {
		v := marketview.VenueState{Venue: n, Status: marketview.OK, TakerFeeMills: int64(10 * i), FillRatioBps: 9000, LatencyMs: int64(20 * i)}
		for l := int64(0); l < 5; l++ {
			v.Asks = append(v.Asks, domain.Level{Px: 10000 + l, Qty: 800})
		}
		vs = append(vs, v)
	}
	in := input(domain.Buy, 3000, vs...)
	in.Policy = Policy{Version: "b", ParticipationPct: 30, MaxChildQty: 2000, LatencyPenaltyMillsPer10Ms: 1, QualityPenaltyMills: 50, LotSize: 100}
	b.ReportAllocs()
	for b.Loop() {
		Plan(in)
	}
}
