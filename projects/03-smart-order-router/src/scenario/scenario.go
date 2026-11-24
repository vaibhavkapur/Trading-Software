// Package scenario runs a scripted, seeded simulation: parent orders and
// venue faults applied at fixed simulated times.
package scenario

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"sort"

	"sor/src/engine"
	"sor/src/tca"
	"sor/src/venuesim"
)

type ParentSpec struct {
	AtMs int64 `json:"at_ms"` // offset from simulation start
	engine.ParentRequest
}

type FaultSpec struct {
	AtMs  int64          `json:"at_ms"`
	Venue string         `json:"venue"`
	Fault venuesim.Fault `json:"fault"`
}

type Scenario struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	engine.Config
	BaselineVenue string       `json:"baseline_venue"` // default: first venue by name
	KillAtMs      int64        `json:"kill_at_ms"`     // engage the kill switch; 0 = never
	Parents       []ParentSpec `json:"parents"`
	Faults        []FaultSpec  `json:"faults"`
}

func Load(path string) (Scenario, error) {
	var sc Scenario
	b, err := os.ReadFile(path)
	if err != nil {
		return sc, err
	}
	if err := json.Unmarshal(b, &sc); err != nil {
		return sc, fmt.Errorf("%s: %w", path, err)
	}
	return sc, nil
}

// Baseline returns the venue used by the naive single-venue baseline.
func (sc Scenario) Baseline() string {
	if sc.BaselineVenue != "" {
		return sc.BaselineVenue
	}
	names := []string{}
	for _, v := range sc.Config.WithDefaults().Venues {
		names = append(names, v.Name)
	}
	sort.Strings(names)
	return names[0]
}

type step struct {
	at int64
	fn func(*engine.Engine) error
}

// Run executes the scenario to completion. singleVenue != "" forces the naive
// single-venue policy; everything else (seed, market, faults) is identical.
func Run(sc Scenario, singleVenue string, log io.Writer) (*engine.Engine, error) {
	cfg := sc.Config.WithDefaults()
	cfg.Policy.SingleVenue = singleVenue
	eng := engine.New(cfg, log)
	var steps []step
	var end int64
	for _, f := range sc.Faults {
		steps = append(steps, step{f.AtMs, func(e *engine.Engine) error { return e.SetFault(f.Venue, f.Fault) }})
	}
	if sc.KillAtMs > 0 {
		steps = append(steps, step{sc.KillAtMs, func(e *engine.Engine) error { e.SetKill(true); return nil }})
	}
	for _, p := range sc.Parents {
		// A risk rejection is a valid scenario outcome, recorded on the parent.
		steps = append(steps, step{p.AtMs, func(e *engine.Engine) error { e.SubmitParent(p.ParentRequest); return nil }})
		end = max(end, p.AtMs+p.DurationSeconds*1000)
	}
	sort.SliceStable(steps, func(i, j int) bool { return steps[i].at < steps[j].at })
	for _, s := range steps {
		eng.AdvanceTo(cfg.StartMs + s.at)
		if err := s.fn(eng); err != nil {
			return nil, err
		}
		end = max(end, s.at)
	}
	// Run past the last deadline so late reports and reconnects can resolve.
	eng.AdvanceTo(cfg.StartMs + end + 2*cfg.Router.ChildTimeoutMs)
	return eng, nil
}

// RunWithBaseline runs the router and the single-venue baseline through the
// identical simulation and returns the router's reports with the comparison
// attached. The event log (if any) records the router run only.
func RunWithBaseline(sc Scenario, log io.Writer) ([]tca.Report, error) {
	router, err := Run(sc, "", log)
	if err != nil {
		return nil, err
	}
	venue := sc.Baseline()
	base, err := Run(sc, venue, nil)
	if err != nil {
		return nil, err
	}
	var out []tca.Report
	for _, p := range router.Parents() {
		r, _ := router.Report(p.ID)
		if b, ok := base.Report(p.ID); ok {
			r.Baseline = tca.Compare(r, b, venue)
		}
		out = append(out, r)
	}
	return out, nil
}
