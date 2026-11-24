// Command sor is the simulated multi-venue smart order router.
//
//	sor run     [-log events.jsonl] [-json] scenario.json   run a scenario, print execution quality
//	sor replay  events.jsonl                                 re-verify every recorded routing decision
//	sor compare [-seeds 1,2,3] scenario.json...              strategies x seeds vs single-venue baseline
//	sor serve   [-addr :8080] [-speed 1] [-config scenario.json] [-log events.jsonl]
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"text/tabwriter"

	"sor/src/api"
	"sor/src/replay"
	"sor/src/scenario"
	"sor/src/tca"
)

func die(err error) {
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func openLog(path string) (io.Writer, func()) {
	if path == "" {
		return nil, func() {}
	}
	f, err := os.Create(path)
	die(err)
	return f, func() { die(f.Close()) }
}

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: sor <run|replay|compare|serve> [flags] [args]")
		os.Exit(2)
	}
	fs := flag.NewFlagSet(os.Args[1], flag.ExitOnError)
	switch os.Args[1] {
	case "run":
		logPath := fs.String("log", "", "write the router's event log (JSON lines) to this file")
		asJSON := fs.Bool("json", false, "print reports as JSON")
		fs.Parse(os.Args[2:])
		if fs.NArg() != 1 {
			die(fmt.Errorf("run needs exactly one scenario file"))
		}
		sc, err := scenario.Load(fs.Arg(0))
		die(err)
		log, closeLog := openLog(*logPath)
		reports, err := scenario.RunWithBaseline(sc, log)
		die(err)
		closeLog()
		if *asJSON {
			enc := json.NewEncoder(os.Stdout)
			enc.SetIndent("", "  ")
			die(enc.Encode(reports))
			return
		}
		fmt.Printf("=== SIMULATED / PAPER TRADING — scenario %q (seed %d) ===\n", sc.Name, sc.Config.WithDefaults().Seed)
		if sc.Description != "" {
			fmt.Println(sc.Description)
		}
		for _, r := range reports {
			printReport(r)
		}
	case "replay":
		fs.Parse(os.Args[2:])
		if fs.NArg() != 1 {
			die(fmt.Errorf("replay needs exactly one event log"))
		}
		f, err := os.Open(fs.Arg(0))
		die(err)
		res, err := replay.Verify(f)
		die(err)
		for _, m := range res.Mismatches {
			fmt.Println("MISMATCH", m)
		}
		fmt.Printf("replayed %d routing decisions against frozen snapshots: %d mismatches\n", res.Decisions, len(res.Mismatches))
		if len(res.Mismatches) > 0 || res.Decisions == 0 {
			os.Exit(1)
		}
	case "compare":
		seeds := fs.String("seeds", "1,2,3,4,5", "comma-separated seeds")
		fs.Parse(os.Args[2:])
		compare(fs.Args(), *seeds)
	case "serve":
		addr := fs.String("addr", "127.0.0.1:8080", "listen address")
		speed := fs.Float64("speed", 1, "simulated seconds per wall second; 0 = manual clock via POST /sim/advance")
		cfgPath := fs.String("config", "", "scenario file to take venue/policy/risk config from (its parents and faults are ignored)")
		logPath := fs.String("log", "", "write the event log (JSON lines) to this file")
		fs.Parse(os.Args[2:])
		var sc scenario.Scenario
		if *cfgPath != "" {
			var err error
			sc, err = scenario.Load(*cfgPath)
			die(err)
		}
		log, closeLog := openLog(*logPath)
		defer closeLog()
		srv := api.New(sc.Config, sc.Baseline(), *speed, log)
		ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
		defer stop()
		go srv.Run(ctx)
		hs := &http.Server{Addr: *addr, Handler: srv.Handler()}
		go func() { <-ctx.Done(); hs.Close() }()
		fmt.Printf("SIMULATED / PAPER smart order router listening on http://%s (speed x%g, baseline venue %s)\n", *addr, *speed, sc.Baseline())
		if err := hs.ListenAndServe(); err != http.ErrServerClosed {
			die(err)
		}
	default:
		die(fmt.Errorf("unknown command %q", os.Args[1]))
	}
}

func printReport(r tca.Report) {
	px := func(mills int64) string { return fmt.Sprintf("%.4f", float64(mills)/100) }
	fmt.Printf("\nParent %s  %s %d %s  strategy=%s  status=%s\n", r.ParentID, r.Side, r.TargetQty, r.Symbol, r.Strategy, r.Status)
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintf(w, "  filled / unfilled\t%d / %d  (%.2f%% fill rate, %d unresolved)\n", r.FilledQty, r.UnfilledQty, r.FillRatePct, r.UnresolvedQty)
	fmt.Fprintf(w, "  time to completion\t%.1fs\n", float64(r.TimeToCompletionMs)/1000)
	fmt.Fprintf(w, "  avg fill px (ticks)\t%.4f\n", r.AvgFillPx)
	fmt.Fprintf(w, "  arrival mid / market VWAP\t%s / %s\n", px(r.ArrivalMidMills), px(r.MarketVWAPMills))
	fmt.Fprintf(w, "  fees\t%s ticks\n", px(r.FeesMills))
	fmt.Fprintf(w, "  implementation shortfall\t%.2f bps  (%.2f mills/share, fees included)\n", r.ShortfallBps, r.ShortfallMillsPerShr)
	fmt.Fprintf(w, "  slippage vs arrival / VWAP\t%.2f / %.2f bps\n", r.SlippageArrivalBps, r.SlippageVWAPBps)
	fmt.Fprintf(w, "  effective spread\t%.2f bps\n", r.EffectiveSpreadBps)
	w.Flush()
	w = tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "  VENUE\tCHILDREN\tREJECTS\tTIMEOUTS\tREJECT RATE\tFILLED\tALLOC %\tFEES (ticks)")
	for _, v := range r.Venues {
		fmt.Fprintf(w, "  %s\t%d\t%d\t%d\t%.2f\t%d\t%.1f\t%s\n", v.Venue, v.Children, v.Rejects, v.Timeouts, v.RejectRate, v.FilledQty, v.AllocPct, px(v.FeesMills))
	}
	w.Flush()
	if b := r.Baseline; b != nil {
		fmt.Printf("  vs naive single-venue baseline (%s, identical simulation): baseline filled %d (%.2f%%), shortfall %.2f bps\n",
			b.Venue, b.FilledQty, b.FillRatePct, b.ShortfallBps)
		fmt.Printf("  router savings: %.2f mills/share (%.2f bps); positive = router cheaper\n", b.SavingsMillsPerShr, b.SavingsBps)
	}
}

func compare(files []string, seedList string) {
	if len(files) == 0 {
		die(fmt.Errorf("compare needs at least one scenario file"))
	}
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Println("=== SIMULATED / PAPER TRADING — strategy comparison (first parent of each scenario) ===")
	fmt.Fprintln(w, "SCENARIO\tSEED\tSTRATEGY\tFILL %\tTIME (s)\tIS (bps)\tBASELINE FILL %\tBASELINE IS (bps)\tSAVINGS (bps)")
	for _, file := range files {
		sc, err := scenario.Load(file)
		die(err)
		if len(sc.Parents) == 0 {
			continue
		}
		for _, s := range strings.Split(seedList, ",") {
			seed, err := strconv.ParseUint(strings.TrimSpace(s), 10, 64)
			die(err)
			for _, strat := range []string{"TWAP", "LIQUIDITY"} {
				run := sc
				run.Seed = seed
				run.Parents = append([]scenario.ParentSpec(nil), sc.Parents...)
				for i := range run.Parents {
					run.Parents[i].Strategy = strat
					run.Parents[i].IntervalSeconds = 0
				}
				reports, err := scenario.RunWithBaseline(run, nil)
				die(err)
				r := reports[0]
				fmt.Fprintf(w, "%s\t%d\t%s\t%.1f\t%.1f\t%.2f\t%.1f\t%.2f\t%.2f\n", sc.Name, seed, strat, r.FillRatePct,
					float64(r.TimeToCompletionMs)/1000, r.ShortfallBps, r.Baseline.FillRatePct, r.Baseline.ShortfallBps, r.Baseline.SavingsBps)
			}
		}
	}
	w.Flush()
}
