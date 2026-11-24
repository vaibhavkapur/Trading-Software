// simgen writes a synthetic event feed as JSON lines, one event per line,
// in the format POST /v1/events accepts. The same flags always produce the
// same file. Everything in it is simulated.
package main

import (
	"bufio"
	"encoding/json"
	"flag"
	"fmt"
	"os"

	"surveillance/src/evaluation"
)

func main() {
	seed := flag.Uint64("seed", 7, "scenario seed")
	n := flag.Int("n", 14, "number of scenarios, spread evenly over the scenario types")
	disorder := flag.Int("disorder", 20, "percentage of neighbouring events delivered out of order")
	out := flag.String("out", "", "file to write events to (default: standard output)")
	labels := flag.String("labels", "", "optional file to write the scenario labels to, as JSON")
	flag.Parse()

	if err := run(*seed, *n, *disorder, *out, *labels); err != nil {
		fmt.Fprintln(os.Stderr, "simgen:", err)
		os.Exit(1)
	}
}

func run(seed uint64, n, disorder int, out, labels string) error {
	scs, err := evaluation.Build(evaluation.Options{Seed: seed, Scenarios: n})
	if err != nil {
		return err
	}
	dst := os.Stdout
	if out != "" {
		if dst, err = os.Create(out); err != nil {
			return err
		}
		defer dst.Close()
	}
	w := bufio.NewWriter(dst)
	enc := json.NewEncoder(w)
	for _, ev := range evaluation.Stream(scs, seed, disorder) {
		if err := enc.Encode(ev); err != nil {
			return err
		}
	}
	if err := w.Flush(); err != nil {
		return err
	}
	if labels != "" {
		b, err := json.MarshalIndent(scs, "", "  ")
		if err != nil {
			return err
		}
		return os.WriteFile(labels, append(b, '\n'), 0o644)
	}
	return nil
}
