package unit

import (
	"encoding/json"
	"testing"
	"time"

	"surveillance/src/evaluation"
	"surveillance/src/normalize"
	"surveillance/src/sim"
	"surveillance/tests/harness"
)

func generate(t *testing.T, name string, seed uint64, borderline bool) sim.Scenario {
	t.Helper()
	sc, err := sim.Generate(name, sim.Params{Seed: seed, Start: harness.T0, Tag: "U", Borderline: borderline})
	if err != nil {
		t.Fatal(err)
	}
	return sc
}

func TestScenariosAreDeterministic(t *testing.T) {
	for _, name := range sim.Names {
		a, _ := json.Marshal(generate(t, name, 7, false).Events)
		b, _ := json.Marshal(generate(t, name, 7, false).Events)
		c, _ := json.Marshal(generate(t, name, 8, false).Events)
		if string(a) != string(b) {
			t.Errorf("%s: the same seed produced different events", name)
		}
		if string(a) == string(c) {
			t.Errorf("%s: different seeds produced identical events", name)
		}
	}
	if _, err := sim.Generate("spoofing", sim.Params{Start: harness.T0}); err == nil {
		t.Error("an unknown scenario name was accepted")
	}
	if _, err := sim.Generate("normal_flow", sim.Params{}); err == nil {
		t.Error("a scenario with no start time was accepted")
	}
}

// The generator must not drift between Go versions or machines: pin the
// first events of one scenario.
func TestScenarioOutputIsPinned(t *testing.T) {
	sc := generate(t, "layering_like", 1, false)
	got, _ := json.Marshal(sc.Events[:2])
	const want = `[{"type":"order","event_id":"U-E2","venue":"XSIM","symbol":"SIMA","ts":"2026-01-05T15:00:03.486Z","account_id":"U-LAY","order_id":"U-O1","action":"NEW","side":"SELL","qty":"1300","price":"33.7300"},` +
		`{"type":"order","event_id":"U-E4","venue":"XSIM","symbol":"SIMA","ts":"2026-01-05T15:00:03.735Z","account_id":"U-LAY","order_id":"U-O3","action":"NEW","side":"SELL","qty":"1300","price":"33.7400"}]`
	if string(got) != want {
		t.Errorf("generator output changed:\n got %s\nwant %s", got, want)
	}
}

func TestEveryScenarioIsWellFormed(t *testing.T) {
	n, err := normalize.New(normalize.RefData{})
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range sim.Names {
		for _, borderline := range []bool{false, true} {
			for seed := uint64(1); seed <= 25; seed++ {
				sc := generate(t, name, seed, borderline)
				if len(sc.Events) == 0 || len(sc.Accounts) == 0 {
					t.Fatalf("%s seed %d: empty scenario", name, seed)
				}
				if (sc.Label == sim.Positive) != (len(sc.Expected) == 1) {
					t.Errorf("%s: label %s with expected rules %v", name, sc.Label, sc.Expected)
				}
				seen := map[string]bool{}
				last := time.Time{}
				for _, raw := range sc.Events {
					ev, rej := n.Normalize(raw, harness.T0)
					if rej != nil {
						t.Fatalf("%s seed %d: the normalizer refused %+v: %v", name, seed, raw, rej)
					}
					if seen[ev.ID] {
						t.Fatalf("%s seed %d: duplicate event ID %s", name, seed, ev.ID)
					}
					seen[ev.ID] = true
					if ev.EventTime.Before(last) || ev.EventTime.Before(sc.Start) || ev.EventTime.After(sc.End) {
						t.Fatalf("%s seed %d: event at %v out of order or outside [%v, %v]", name, seed, ev.EventTime, sc.Start, sc.End)
					}
					last = ev.EventTime
				}
				// Window rules need a burst to fit inside one window.
				if d := sc.End.Sub(sc.Start); d > 5*time.Minute {
					t.Errorf("%s seed %d: scenario lasts %v", name, seed, d)
				}
			}
		}
	}
}

func TestEvaluationIsReproducibleAndHonest(t *testing.T) {
	o := evaluation.Options{Seed: 42, Scenarios: 70, RulesPath: "../../examples/rules.json", DisorderPct: 20}
	a, err := evaluation.Run(o)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := evaluation.Run(o)
	ja, _ := json.Marshal(a)
	jb, _ := json.Marshal(b)
	if string(ja) != string(jb) {
		t.Fatal("the same seed produced two different reports")
	}
	if a.Positives+a.Negatives != 70 || a.Negatives == 0 || a.Positives == 0 {
		t.Fatalf("scenario mix: %d positive, %d negative", a.Positives, a.Negatives)
	}
	// The simulator stays within the allowed lateness, so nothing is late
	// even though a fifth of neighbouring events are swapped in delivery.
	if a.LateEvents != 0 {
		t.Errorf("%d events were late", a.LateEvents)
	}
	// Clear positives are generated well inside the thresholds: a miss
	// there is a regression in a detector or in the generator.
	for _, r := range a.Rules {
		if r.ClearExpected == 0 {
			t.Errorf("%s has no clear positive in the set", r.Rule)
		}
		if r.ClearCaught != r.ClearExpected {
			t.Errorf("%s caught %d of %d clear positives", r.Rule, r.ClearCaught, r.ClearExpected)
		}
	}
	// The controls must be able to estimate a false-positive rate: there
	// are negatives, the report says how many were flagged, and the
	// limitations travel with it.
	if a.FalsePositiveRate != float64(a.NegativesFlagged)/float64(a.Negatives) || len(a.Limitations) == 0 {
		t.Errorf("false-positive accounting wrong: %d of %d, rate %v", a.NegativesFlagged, a.Negatives, a.FalsePositiveRate)
	}
	// Ordinary two-way flow among many accounts should essentially never
	// alert. Hard negatives are expected to; they are why this is a
	// lead generator and not a verdict.
	for _, out := range a.Outcomes {
		if out.Name == "normal_flow" && len(out.Fired) > 0 && out.Fired[0] != "WASH_TRADE_LIKE" {
			t.Errorf("normal flow (seed %d) fired %v", out.Seed, out.Fired)
		}
	}
}
