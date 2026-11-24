package venuesim

import (
	"reflect"
	"testing"

	"sor/src/domain"
)

var vcfg = Config{Name: "A", TakerFeeMills: 30, LatencyMs: 5, HalfSpreadTicks: 1, Levels: 3, BaseDepth: 1000, RefillPct: 20, NoiseTicks: 1}

func newVenue(seed uint64) *Venue {
	return New(vcfg, "XYZ", 100, seed, NewMidPath(seed, 10000, 10))
}

// The market a venue generates must not depend on the orders it receives,
// otherwise router and baseline runs would not see an identical simulation.
func TestMarketIsIndependentOfOrderFlow(t *testing.T) {
	quiet, busy := newVenue(42), newVenue(42)
	for k := 0; k < 300; k++ {
		qv, qp := quiet.Step(k)
		bv, bp := busy.Step(k)
		if qv != bv || qp != bp || !reflect.DeepEqual(quiet.bids, busy.bids) || !reflect.DeepEqual(quiet.asks, busy.asks) {
			t.Fatalf("step %d diverged", k)
		}
		busy.Submit(int64(k), Order{ChildID: string(rune('a' + k)), Side: domain.Buy, Qty: 500, LimitPx: 1 << 40})
	}
	other := newVenue(43)
	same := true
	for k := 0; k < 300; k++ {
		same = same && quiet.path.At(k) == other.path.At(k)
	}
	if same {
		t.Fatal("different seeds produced the same path")
	}
}

func TestIOCMatching(t *testing.T) {
	v := newVenue(1)
	v.Step(0)
	best, depth := v.asks[0].Px, v.asks[0].Qty
	// Limit at best ask: fills at most the displayed size, remainder is cancelled.
	rep, _ := v.Submit(0, Order{ChildID: "c1", Side: domain.Buy, Qty: depth + 500, LimitPx: best})
	if len(rep.Fills) != 1 || rep.Fills[0].Qty != depth || rep.Fills[0].Px != best || rep.Fills[0].FeeMills != depth*30 {
		t.Fatalf("fills %+v (depth %d @ %d)", rep.Fills, depth, best)
	}
	// The level is now depleted until it refills.
	if rep, _ := v.Submit(0, Order{ChildID: "c2", Side: domain.Buy, Qty: 100, LimitPx: best}); len(rep.Fills) != 0 {
		t.Fatalf("filled against depleted level: %+v", rep.Fills)
	}
	if q := v.Quote(0); len(q.Asks) != 2 || q.Asks[0].Px != best+1 {
		t.Fatalf("depleted level still displayed: %+v", q.Asks)
	}
	// A sell never trades below its limit.
	bid := v.bids[0].Px
	if rep, _ := v.Submit(0, Order{ChildID: "c3", Side: domain.Sell, Qty: 100, LimitPx: bid + 1}); len(rep.Fills) != 0 {
		t.Fatalf("sold below limit: %+v", rep.Fills)
	}
	rep, _ = v.Submit(0, Order{ChildID: "c4", Side: domain.Sell, Qty: 1 << 30, LimitPx: 0})
	var total int64
	for _, f := range rep.Fills {
		total += f.Qty
	}
	if want := v.bids[0].Qty + v.bids[1].Qty + v.bids[2].Qty; total != want || len(rep.Fills) != 3 {
		t.Fatalf("sweep filled %d want %d", total, want)
	}
	// Resubmitting a known child id returns the stored result and trades nothing.
	if again, _ := v.Submit(1, Order{ChildID: "c1", Side: domain.Buy, Qty: 1, LimitPx: best}); !reflect.DeepEqual(again, mustResult(t, v, "c1")) {
		t.Fatal("resubmit not idempotent")
	}
}

func mustResult(t *testing.T, v *Venue, id string) domain.ExecReport {
	t.Helper()
	r, ok := v.Result(id)
	if !ok {
		t.Fatalf("no stored result for %s", id)
	}
	return r
}

func TestFaults(t *testing.T) {
	v := newVenue(1)
	v.Step(0)
	v.SetFault(Fault{RejectRate: 1, DuplicateRate: 1, ClockSkewMs: 5000, LatencyMs: 200})
	rep, dup := v.Submit(1000, Order{ChildID: "c1", Side: domain.Buy, Qty: 100, LimitPx: 1 << 40})
	if rep.Status != domain.ReportRejected || len(rep.Fills) != 0 || !dup || rep.VenueMs != 6000 {
		t.Fatalf("rep %+v dup %v", rep, dup)
	}
	if v.Latency() != 205 || v.Quote(1000).AsOfMs != 6000 {
		t.Fatal("latency / skew fault not applied")
	}
}

func TestDepletedDepthRefills(t *testing.T) {
	v := newVenue(1)
	v.Step(0)
	v.Submit(0, Order{ChildID: "c1", Side: domain.Buy, Qty: 1 << 30, LimitPx: 1 << 40})
	if len(v.Quote(0).Asks) != 0 {
		t.Fatal("book should be swept")
	}
	for k := 1; k < 60; k++ {
		v.Step(k)
	}
	if len(v.consumed[1]) != 0 || len(v.Quote(0).Asks) != 3 {
		t.Fatalf("depth did not refill: %v", v.consumed[1])
	}
}
