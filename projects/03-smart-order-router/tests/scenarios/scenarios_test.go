// Package scenarios runs seeded end-to-end simulations and checks the
// invariants in the README's definition of done.
package scenarios

import (
	"bytes"
	"math/rand/v2"
	"path/filepath"
	"testing"

	"sor/src/domain"
	"sor/src/engine"
	"sor/src/replay"
	"sor/src/scenario"
	"sor/src/venuesim"
)

func load(t *testing.T, name string) scenario.Scenario {
	t.Helper()
	sc, err := scenario.Load(filepath.Join("..", "..", "examples", name+".json"))
	if err != nil {
		t.Fatal(err)
	}
	return sc
}

func run(t *testing.T, sc scenario.Scenario, singleVenue string) (*engine.Engine, []byte) {
	t.Helper()
	var log bytes.Buffer
	e, err := scenario.Run(sc, singleVenue, &log)
	if err != nil {
		t.Fatal(err)
	}
	return e, log.Bytes()
}

// checkInvariants: no overfill at any level, fills reconcile exactly with
// order state, no fill id counted twice, every fill inside its child's limit.
func checkInvariants(t *testing.T, e *engine.Engine) {
	t.Helper()
	seen := map[string]bool{}
	for _, p := range e.Parents() {
		limit := map[string]domain.ChildOrder{}
		var childSum int64
		for _, c := range e.Children(p.ID) {
			limit[c.ID] = c
			childSum += c.FilledQty
			if c.FilledQty > c.Qty || c.Qty%100 != 0 {
				t.Fatalf("bad child %+v", c)
			}
			if c.Status == domain.ChildSent {
				t.Fatalf("child still in flight after run: %+v", c)
			}
		}
		var fillSum int64
		for _, f := range e.Fills(p.ID) {
			fillSum += f.Qty
			if seen[f.Venue+f.ID] {
				t.Fatalf("fill %s counted twice", f.ID)
			}
			seen[f.Venue+f.ID] = true
			if c := limit[f.ChildID]; p.Side.Sign()*(f.Px-c.LimitPx) > 0 || f.Venue != c.Venue {
				t.Fatalf("fill %+v violates child %+v", f, c)
			}
		}
		if p.FilledQty > p.TargetQty || fillSum != p.FilledQty || childSum != p.FilledQty {
			t.Fatalf("parent %s: target %d filled %d fills %d children %d", p.ID, p.TargetQty, p.FilledQty, fillSum, childSum)
		}
		if p.Status == domain.ParentWorking {
			t.Fatalf("parent %s still working after run", p.ID)
		}
	}
}

func TestExampleScenarios(t *testing.T) {
	for _, name := range []string{"calm-twap", "venue-outage", "stale-data", "liquidity-seek"} {
		t.Run(name, func(t *testing.T) {
			sc := load(t, name)
			e, log := run(t, sc, "")
			checkInvariants(t, e)
			for _, p := range e.Parents() {
				if p.Status != domain.ParentFilled {
					t.Errorf("router did not complete %s: %+v", p.ID, p)
				}
			}
			// Replay: same scenario, byte-identical event log.
			if _, again := run(t, sc, ""); !bytes.Equal(log, again) {
				t.Fatal("event log is not reproducible")
			}
			// Replay: every decision reproduced from its frozen snapshot.
			res, err := replay.Verify(bytes.NewReader(log))
			if err != nil || res.Decisions == 0 || len(res.Mismatches) != 0 {
				t.Fatalf("replay: %d decisions, mismatches %v, err %v", res.Decisions, res.Mismatches, err)
			}
			base, _ := run(t, sc, sc.Baseline())
			checkInvariants(t, base)
			for _, p := range base.Parents() {
				for _, c := range base.Children(p.ID) {
					if c.Venue != sc.Baseline() {
						t.Fatalf("baseline routed to %s", c.Venue)
					}
				}
			}
			// Identical simulation: both runs saw the same market tape.
			rp, bp := e.Parents()[0], base.Parents()[0]
			if rp.ArrivalMidMills != bp.ArrivalMidMills ||
				e.MarketVWAPMills(rp.StartMs, rp.DeadlineMs) != base.MarketVWAPMills(bp.StartMs, bp.DeadlineMs) {
				t.Fatal("router and baseline did not see the same market")
			}
			reports, err := scenario.RunWithBaseline(sc, nil)
			if err != nil || len(reports) != 1 || reports[0].Baseline == nil || reports[0].Baseline.Venue != sc.Baseline() {
				t.Fatalf("reports %+v err %v", reports, err)
			}
		})
	}
}

func TestTamperedLogFailsReplay(t *testing.T) {
	_, log := run(t, load(t, "calm-twap"), "")
	// Change a recorded fee in a frozen snapshot: the replayed decision must differ somewhere.
	tampered := bytes.Replace(log, []byte(`"venue":"BRAVO","status":"OK"`), []byte(`"venue":"BRAVO","status":"STALE"`), -1)
	res, err := replay.Verify(bytes.NewReader(tampered))
	if err != nil || len(res.Mismatches) == 0 {
		t.Fatalf("tampering not detected: %+v %v", res, err)
	}
}

func TestOutageVenueIsNotRoutedWhileDown(t *testing.T) {
	sc := load(t, "venue-outage")
	e, _ := run(t, sc, "")
	start := e.Config().StartMs
	var before, during, after int
	for _, c := range e.Children("P-000001") {
		if c.Venue != "ALPHA" {
			continue
		}
		switch t := c.SentMs - start; {
		case t < 120_000:
			before++
		case t < 300_000:
			during++
		default:
			after++
		}
	}
	if before == 0 || during != 0 || after == 0 {
		t.Fatalf("ALPHA children before/during/after outage: %d/%d/%d", before, during, after)
	}
	// The naive baseline can only wait for its one venue.
	base, _ := run(t, sc, "ALPHA")
	for _, f := range base.Fills("P-000001") {
		if at := f.RecvMs - start; at > 121_000 && at < 300_000 {
			t.Fatalf("baseline filled during outage at +%dms", at)
		}
	}
}

func TestStaleAndSkewedVenuesAreNotRouted(t *testing.T) {
	e, _ := run(t, load(t, "stale-data"), "")
	start := e.Config().StartMs
	for _, c := range e.Children("P-000001") {
		at := c.SentMs - start
		// Allow stale_ms (1s) for detection after the fault starts.
		if c.Venue == "BRAVO" && at > 61_500 && at < 240_000 {
			t.Fatalf("routed to BRAVO on stale data at +%dms", at)
		}
		if c.Venue == "CHARLIE" && at > 120_500 && at < 300_000 {
			t.Fatalf("routed to CHARLIE with skewed clock at +%dms", at)
		}
	}
}

// Fuzz the simulator: random seeds, sides, sizes, strategies and fault
// timelines (including link loss, duplicates and a mid-run kill switch).
// Whatever happens, the accounting invariants must hold and decisions replay.
func TestRandomFaultTimelinesKeepInvariants(t *testing.T) {
	rng := rand.New(rand.NewPCG(99, 7))
	venues := []string{"ALPHA", "BRAVO", "CHARLIE"}
	n := 60
	if testing.Short() {
		n = 10
	}
	for i := 0; i < n; i++ {
		sc := scenario.Scenario{Name: "fuzz"}
		sc.Seed = rng.Uint64N(1 << 32)
		for j := 0; j < 1+rng.IntN(2); j++ {
			req := engine.ParentRequest{Symbol: "XYZ", Side: domain.Buy, Qty: 100 * (1 + rng.Int64N(300)),
				Strategy: "TWAP", DurationSeconds: 30 + rng.Int64N(200)}
			if rng.IntN(2) == 0 {
				req.Side = domain.Sell
			}
			if rng.IntN(3) == 0 {
				req.Strategy = "LIQUIDITY"
			}
			sc.Parents = append(sc.Parents, scenario.ParentSpec{AtMs: 1000 + rng.Int64N(20_000), ParentRequest: req})
		}
		for j := 0; j < rng.IntN(12); j++ {
			f := venuesim.Fault{}
			switch rng.IntN(6) {
			case 0:
				f.Down = true
			case 1:
				f.LatencyMs = rng.Int64N(3000)
			case 2:
				f.RejectRate = rng.Float64()
			case 3:
				f.StopQuotes = true
			case 4:
				f.ClockSkewMs = rng.Int64N(10_000) - 5000
			case 5:
				f.DuplicateRate, f.RejectRate, f.LatencyMs = 1, rng.Float64()/2, rng.Int64N(800)
			}
			if rng.IntN(4) == 0 {
				f = venuesim.Fault{} // heal
			}
			sc.Faults = append(sc.Faults, scenario.FaultSpec{AtMs: rng.Int64N(200_000), Venue: venues[rng.IntN(3)], Fault: f})
		}
		if rng.IntN(5) == 0 {
			sc.KillAtMs = 1000 + rng.Int64N(100_000)
		}
		e, log := run(t, sc, "")
		checkInvariants(t, e)
		if res, err := replay.Verify(bytes.NewReader(log)); err != nil || len(res.Mismatches) != 0 {
			t.Fatalf("seed %d: replay mismatches %v err %v", sc.Seed, res.Mismatches, err)
		}
		base, _ := run(t, sc, "BRAVO")
		checkInvariants(t, base)
	}
}
