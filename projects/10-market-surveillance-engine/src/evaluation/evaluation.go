// Package evaluation runs labelled synthetic scenarios through a real
// engine and counts what the rules caught, missed and wrongly flagged.
//
// What the numbers mean, and what they do not, is spelled out in
// Limitations, which is printed with every report.
package evaluation

import (
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"time"

	"surveillance/src/engine"
	"surveillance/src/normalize"
	"surveillance/src/rules"
	"surveillance/src/sim"
)

// Limitations travels with every report.
var Limitations = []string{
	"Every event is synthetic. These figures say nothing about performance on real order flow.",
	"The scenarios and the rules were written by the same author, so 'clear' positives are detected largely by construction. Recall on them is a regression check, not a measure of skill.",
	"A scenario's label records what the generator set out to imitate. It is not ground truth about intent; no such thing exists in this data.",
	"'Borderline' positives are deliberately generated below or near thresholds. Their recall shows how sharply the thresholds cut, not how often real cases are missed.",
	"The false-positive rate depends entirely on the mix of negative scenarios chosen here (how many are 'hard'). A different mix gives a different rate.",
	"Each scenario uses its own accounts, so interactions between unrelated participants in a crowded book are under-represented.",
	"Precision and recall are counted per scenario (did the expected rule fire on it at all), not per alert.",
}

type Options struct {
	Seed      uint64
	Scenarios int // total; spread evenly over the scenario types
	RulesPath string
	// BorderlineEvery makes every n-th positive of each type a borderline one.
	BorderlineEvery int
	// Disorder swaps this percentage of neighbouring events before
	// delivery, to exercise reordering.
	DisorderPct int
}

type RuleScore struct {
	Rule            string  `json:"rule"`
	TruePositives   int     `json:"true_positives"`
	FalseNegatives  int     `json:"false_negatives"`
	FalsePositives  int     `json:"false_positives"`
	Tolerated       int     `json:"tolerated_co_firings"`
	Precision       float64 `json:"precision"`
	Recall          float64 `json:"recall"`
	ClearExpected   int     `json:"clear_expected"`
	ClearCaught     int     `json:"clear_caught"`
	BorderExpected  int     `json:"borderline_expected"`
	BorderCaught    int     `json:"borderline_caught"`
	HasExpectations bool    `json:"-"`
}

type ScenarioScore struct {
	Name      string `json:"name"`
	Label     string `json:"label"`
	Intensity string `json:"intensity"`
	Count     int    `json:"count"`
	// For positives: scenarios where the expected rule fired.
	// For negatives: scenarios where any rule fired (false positives).
	Flagged int `json:"flagged"`
}

type Outcome struct {
	sim.Scenario
	Events int      `json:"events"`
	Fired  []string `json:"fired_rules"`
	// Verdict is TP, FN, TN or FP at scenario level.
	Verdict string `json:"verdict"`
}

type Report struct {
	Seed           uint64 `json:"seed"`
	RulesetVersion string `json:"ruleset_version"`
	RulesetHash    string `json:"ruleset_hash"`

	Scenarios int `json:"scenarios"`
	Positives int `json:"positives"`
	Negatives int `json:"negatives"`

	Events           uint64  `json:"events"`
	LateEvents       uint64  `json:"late_events"`
	Alerts           int     `json:"alerts"`
	Detections       uint64  `json:"detections"`
	Suppressed       uint64  `json:"suppressed_detections"`
	DuplicateRate    float64 `json:"duplicate_alert_rate"`
	PerMillionEvents float64 `json:"alerts_per_million_events"`

	Rules      []RuleScore     `json:"rules"`
	ByScenario []ScenarioScore `json:"by_scenario"`

	NegativesFlagged  int     `json:"negatives_flagged"`
	FalsePositiveRate float64 `json:"false_positive_rate"`
	OrdinaryNegatives int     `json:"ordinary_negatives"`
	OrdinaryFlagged   int     `json:"ordinary_negatives_flagged"`
	HardNegatives     int     `json:"hard_negatives"`
	HardFlagged       int     `json:"hard_negatives_flagged"`

	Limitations []string  `json:"limitations"`
	Outcomes    []Outcome `json:"outcomes"`
}

// Base is the fixed event-time origin of every evaluation run.
var Base = time.Date(2026, 1, 5, 14, 0, 0, 0, time.UTC)

// Build generates the labelled scenario set for a seed.
func Build(o Options) ([]sim.Scenario, error) {
	if o.Scenarios <= 0 {
		o.Scenarios = 210
	}
	if o.BorderlineEvery <= 0 {
		o.BorderlineEvery = 3
	}
	symbols := []string{"SIMA", "SIMB", "SIMC"}
	out := make([]sim.Scenario, 0, o.Scenarios)
	for i := 0; i < o.Scenarios; i++ {
		name := sim.Names[i%len(sim.Names)]
		round := i / len(sim.Names)
		sc, err := sim.Generate(name, sim.Params{
			// Scenarios overlap in time, as independent participants do.
			Seed: o.Seed*1_000_003 + uint64(i), Start: Base.Add(time.Duration(i) * 7 * time.Second),
			Symbol: symbols[i%len(symbols)], Tag: fmt.Sprintf("E%04d", i),
			Borderline: round%o.BorderlineEvery == o.BorderlineEvery-1,
		})
		if err != nil {
			return nil, err
		}
		out = append(out, sc)
	}
	return out, nil
}

// MaxDisorder bounds how far apart in event time two events may be and
// still be delivered in the wrong order. It is well inside the example
// ruleset's allowed lateness (5s), so disorder alone never makes an event
// late; lateness is exercised separately, by the scenario tests.
const MaxDisorder = 2 * time.Second

// Stream merges scenarios into one event-time-ordered feed, then swaps a
// share of neighbours so that delivery order differs from event order.
func Stream(scs []sim.Scenario, seed uint64, disorderPct int) []normalize.Raw {
	var all []normalize.Raw
	for _, sc := range scs {
		all = append(all, sc.Events...)
	}
	sort.SliceStable(all, func(i, j int) bool { return all[i].TS < all[j].TS })
	s := seed ^ 0x5eed
	for i := 0; i+1 < len(all); i++ {
		s = s*6364136223846793005 + 1442695040888963407
		if int((s>>33)%100) >= disorderPct {
			continue
		}
		a, errA := normalize.ParseTime(all[i].TS)
		b, errB := normalize.ParseTime(all[i+1].TS)
		if errA == nil && errB == nil && b.Sub(a) <= MaxDisorder {
			all[i], all[i+1] = all[i+1], all[i]
			i++
		}
	}
	return all
}

// Run executes the evaluation in a scratch directory.
func Run(o Options) (Report, error) {
	scs, err := Build(o)
	if err != nil {
		return Report{}, err
	}
	dir, err := os.MkdirTemp("", "surv-eval-")
	if err != nil {
		return Report{}, err
	}
	defer os.RemoveAll(dir)

	// A deterministic clock, later than every event, so that no event is
	// "from the future" and the run is repeatable.
	now := Base.Add(24 * time.Hour)
	eng, err := engine.Open(engine.Config{DataDir: dir, RulesPath: o.RulesPath, Clock: func() time.Time {
		now = now.Add(time.Millisecond)
		return now
	}})
	if err != nil {
		return Report{}, err
	}
	defer eng.Close()

	stream := Stream(scs, o.Seed, o.DisorderPct)
	for len(stream) > 0 {
		n := min(500, len(stream))
		res, err := eng.Ingest(stream[:n], false)
		if err != nil {
			return Report{}, err
		}
		if len(res.Rejected) > 0 {
			return Report{}, fmt.Errorf("simulator produced an event the normalizer refused: %+v", res.Rejected[0])
		}
		stream = stream[n:]
	}
	if _, err := eng.Flush(); err != nil {
		return Report{}, err
	}
	return score(o, scs, eng), nil
}

func score(o Options, scs []sim.Scenario, eng *engine.Engine) Report {
	st := eng.Stats()
	rep := Report{Seed: o.Seed, RulesetVersion: st.Ruleset.Version, RulesetHash: st.Ruleset.Hash,
		Scenarios: len(scs), Events: st.Events.Total, LateEvents: st.Events.Late,
		Alerts: st.Alerts.Total, Detections: st.Alerts.Detections, Suppressed: st.Alerts.Suppressed,
		DuplicateRate: st.Alerts.DuplicateRate, PerMillionEvents: st.Alerts.PerMillionEvents,
		Limitations: Limitations}

	owner := map[string]int{}
	for i, sc := range scs {
		for _, a := range sc.Accounts {
			owner[a] = i
		}
	}
	fired := make([]map[string]bool, len(scs))
	for i := range fired {
		fired[i] = map[string]bool{}
	}
	for _, a := range eng.Alerts(engine.AlertFilter{}) {
		for _, acct := range a.Accounts {
			if i, ok := owner[acct]; ok {
				fired[i][a.RuleID] = true
			}
		}
	}

	scores := map[string]*RuleScore{}
	for _, id := range rules.RuleIDs {
		scores[id] = &RuleScore{Rule: id}
	}
	type groupKey struct{ name, intensity string }
	groups := map[groupKey]*ScenarioScore{}

	for i, sc := range scs {
		g := groups[groupKey{sc.Name, sc.Intensity}]
		if g == nil {
			g = &ScenarioScore{Name: sc.Name, Label: sc.Label, Intensity: sc.Intensity}
			groups[groupKey{sc.Name, sc.Intensity}] = g
		}
		g.Count++

		out := Outcome{Scenario: sc, Events: len(sc.Events), Fired: []string{}}
		for _, id := range rules.RuleIDs {
			if fired[i][id] {
				out.Fired = append(out.Fired, id)
			}
		}
		caught := false
		for _, id := range rules.RuleIDs {
			s := scores[id]
			expected, tolerated := in(sc.Expected, id), in(sc.Tolerated, id)
			if expected {
				s.HasExpectations = true
				if sc.Intensity == sim.Borderline {
					s.BorderExpected++
				} else {
					s.ClearExpected++
				}
			}
			switch {
			case expected && fired[i][id]:
				s.TruePositives++
				caught = true
				if sc.Intensity == sim.Borderline {
					s.BorderCaught++
				} else {
					s.ClearCaught++
				}
			case expected:
				s.FalseNegatives++
			case fired[i][id] && tolerated:
				s.Tolerated++
			case fired[i][id]:
				s.FalsePositives++
			}
		}

		if sc.Label == sim.Positive {
			rep.Positives++
			out.Verdict = "FN"
			if caught {
				out.Verdict = "TP"
				g.Flagged++
			}
		} else {
			rep.Negatives++
			flagged := len(out.Fired) > 0
			out.Verdict = "TN"
			if flagged {
				out.Verdict = "FP"
				g.Flagged++
				rep.NegativesFlagged++
			}
			if sc.Intensity == sim.Hard {
				rep.HardNegatives++
				if flagged {
					rep.HardFlagged++
				}
			} else {
				rep.OrdinaryNegatives++
				if flagged {
					rep.OrdinaryFlagged++
				}
			}
		}
		rep.Outcomes = append(rep.Outcomes, out)
	}

	for _, id := range rules.RuleIDs {
		s := scores[id]
		s.Precision = ratio(s.TruePositives, s.TruePositives+s.FalsePositives)
		s.Recall = ratio(s.TruePositives, s.TruePositives+s.FalseNegatives)
		rep.Rules = append(rep.Rules, *s)
	}
	rep.FalsePositiveRate = ratio(rep.NegativesFlagged, rep.Negatives)
	for _, g := range groups {
		rep.ByScenario = append(rep.ByScenario, *g)
	}
	sort.Slice(rep.ByScenario, func(i, j int) bool {
		a, b := rep.ByScenario[i], rep.ByScenario[j]
		if a.Label != b.Label {
			return a.Label > b.Label // positive first
		}
		if a.Name != b.Name {
			return a.Name < b.Name
		}
		return a.Intensity < b.Intensity
	})
	return rep
}

func in(xs []string, x string) bool {
	for _, v := range xs {
		if v == x {
			return true
		}
	}
	return false
}

func ratio(a, b int) float64 {
	if b == 0 {
		return 0
	}
	return float64(a) / float64(b)
}

func pct(a, b int) string {
	if b == 0 {
		return "   n/a"
	}
	return fmt.Sprintf("%5.1f%%", 100*float64(a)/float64(b))
}

// WriteText renders the report for a terminal or a markdown code block.
func (r Report) WriteText(w io.Writer) {
	fmt.Fprintf(w, "SYNTHETIC EVALUATION (simulated data only)\n")
	fmt.Fprintf(w, "seed %d, ruleset %s (%s)\n\n", r.Seed, r.RulesetVersion, r.RulesetHash[:12])
	fmt.Fprintf(w, "scenarios %d (%d positive, %d negative), events %d, late events %d\n",
		r.Scenarios, r.Positives, r.Negatives, r.Events, r.LateEvents)
	fmt.Fprintf(w, "alerts %d (%.0f per million events), detections %d, folded into existing alerts %d (duplicate-alert rate %.1f%%)\n\n",
		r.Alerts, r.PerMillionEvents, r.Detections, r.Suppressed, 100*r.DuplicateRate)

	fmt.Fprintf(w, "Per rule, counted per scenario:\n")
	fmt.Fprintf(w, "  %-24s %4s %4s %4s  %9s %9s  %13s %13s  %s\n",
		"rule", "TP", "FN", "FP", "precision", "recall", "clear caught", "border caught", "tolerated")
	for _, s := range r.Rules {
		fmt.Fprintf(w, "  %-24s %4d %4d %4d  %9s %9s  %6d of %-3d %6d of %-3d  %d\n",
			s.Rule, s.TruePositives, s.FalseNegatives, s.FalsePositives,
			pct(s.TruePositives, s.TruePositives+s.FalsePositives), pct(s.TruePositives, s.TruePositives+s.FalseNegatives),
			s.ClearCaught, s.ClearExpected, s.BorderCaught, s.BorderExpected, s.Tolerated)
	}

	fmt.Fprintf(w, "\nPer scenario type:\n")
	fmt.Fprintf(w, "  %-22s %-9s %-11s %6s %8s\n", "scenario", "label", "intensity", "count", "flagged")
	for _, g := range r.ByScenario {
		fmt.Fprintf(w, "  %-22s %-9s %-11s %6d %8d\n", g.Name, g.Label, g.Intensity, g.Count, g.Flagged)
	}

	fmt.Fprintf(w, "\nNegative controls flagged by any rule (false-positive rate): %d of %d (%s)\n",
		r.NegativesFlagged, r.Negatives, strings.TrimSpace(pct(r.NegativesFlagged, r.Negatives)))
	fmt.Fprintf(w, "  ordinary: %d of %d    hard: %d of %d\n",
		r.OrdinaryFlagged, r.OrdinaryNegatives, r.HardFlagged, r.HardNegatives)

	fmt.Fprintf(w, "\nLimitations:\n")
	for _, l := range r.Limitations {
		fmt.Fprintf(w, "  - %s\n", l)
	}
}
