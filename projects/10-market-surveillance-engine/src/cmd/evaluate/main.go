// evaluate runs the labelled synthetic scenario set through a fresh engine
// and prints what the rules caught, missed and wrongly flagged.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"

	"surveillance/src/evaluation"
)

func main() {
	seed := flag.Uint64("seed", 42, "scenario seed; the same seed gives the same report")
	n := flag.Int("n", 210, "number of scenarios, spread evenly over the scenario types")
	rulesPath := flag.String("rules", "examples/rules.json", "ruleset file")
	disorder := flag.Int("disorder", 20, "percentage of neighbouring events delivered out of order")
	asJSON := flag.Bool("json", false, "print the full report, with every scenario's outcome, as JSON")
	flag.Parse()

	rep, err := evaluation.Run(evaluation.Options{Seed: *seed, Scenarios: *n, RulesPath: *rulesPath, DisorderPct: *disorder})
	if err != nil {
		fmt.Fprintln(os.Stderr, "evaluate:", err)
		os.Exit(1)
	}
	if *asJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		_ = enc.Encode(rep)
		return
	}
	rep.WriteText(os.Stdout)
}
