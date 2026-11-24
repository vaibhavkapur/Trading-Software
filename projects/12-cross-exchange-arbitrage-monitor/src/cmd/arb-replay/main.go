// Command arb-replay runs deterministic replay on recorded order book fixtures
// and verifies identical alerts and SHA-256 digests across repeated runs.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"

	"arbmonitor/src/connectors"
	"arbmonitor/src/domain"
	"arbmonitor/src/edgecalculator"
	"arbmonitor/src/normalizer"
	"arbmonitor/src/replay"
)

func main() {
	instrumentsPath := flag.String("instruments", "examples/config/instruments.json", "path to instruments JSON")
	feesPath := flag.String("fees", "examples/config/fees.json", "path to fees JSON")
	balancesPath := flag.String("balances", "examples/config/balances.json", "path to balances JSON")
	alertCfgPath := flag.String("alert-config", "examples/config/alert_config.json", "path to alert config JSON")
	feedPath := flag.String("feed", "examples/fixtures/recorded_books.jsonl", "path to recorded feed JSONL")
	archivePath := flag.String("archive", "", "optional JSONL path to write replayed alerts")
	lookaheadMs := flag.Int64("lookahead-ms", 500, "paper adverse-selection lookahead window in ms")
	verify := flag.Bool("verify-determinism", true, "run twice and assert identical SHA-256 alert digests")
	asJSON := flag.Bool("json", false, "emit full JSON report")
	flag.Parse()

	reg, err := normalizer.LoadRegistryFile(*instrumentsPath)
	if err != nil {
		fatalf("load instruments: %v", err)
	}
	fees, err := edgecalculator.LoadFeeScheduleFile(*feesPath)
	if err != nil {
		fatalf("load fees: %v", err)
	}
	balances, err := edgecalculator.LoadBalanceBookFile(*balancesPath)
	if err != nil {
		fatalf("load balances: %v", err)
	}
	cfgData, err := os.ReadFile(*alertCfgPath)
	if err != nil {
		fatalf("read alert config: %v", err)
	}
	cfg := domain.DefaultAlertConfig()
	if err := json.Unmarshal(cfgData, &cfg); err != nil {
		fatalf("parse alert config: %v", err)
	}
	events, err := connectors.ReadFeedJSONL(*feedPath)
	if err != nil {
		fatalf("read feed: %v", err)
	}

	opts := replay.Options{
		Events:             events,
		Registry:           reg,
		Fees:               fees,
		Balances:           balances,
		Config:             cfg,
		ArchivePath:        *archivePath,
		AdverseLookaheadMs: *lookaheadMs,
	}

	rep1, err := replay.Run(opts)
	if err != nil {
		fatalf("replay run 1: %v", err)
	}

	if *verify {
		opts2 := opts
		opts2.ArchivePath = ""
		rep2, err := replay.Run(opts2)
		if err != nil {
			fatalf("replay run 2: %v", err)
		}
		if rep1.DigestSHA256 != rep2.DigestSHA256 {
			fatalf("determinism check FAILED: run1=%s run2=%s", rep1.DigestSHA256, rep2.DigestSHA256)
		}
	}

	if *asJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		_ = enc.Encode(rep1)
		return
	}

	fmt.Printf("=== Deterministic Replay Report (PAPER READ-ONLY) ===\n")
	fmt.Printf("Feed: %s (%d events, %d books accepted)\n", *feedPath, rep1.EventsProcessed, rep1.BooksAccepted)
	fmt.Printf("Feed anomalies: duplicates=%d out_of_order=%d seq_gaps=%d disconnects=%d reconnects=%d\n",
		rep1.DuplicatesIgnored, rep1.OutOfOrderIgnored, rep1.SequenceGaps, rep1.Disconnects, rep1.Reconnects)
	fmt.Printf("Alerts emitted: %d | Evaluations: %d | Digest SHA-256: %s\n",
		len(rep1.Alerts), rep1.Metrics.EvaluationsTotal, rep1.DigestSHA256)
	if *verify {
		fmt.Printf("Determinism check: PASSED (2/2 runs produced identical SHA-256 digest)\n")
	}
	fmt.Printf("Disclaimer: %s\n", rep1.Disclaimer)
}

func fatalf(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "arb-replay error: "+format+"\n", args...)
	os.Exit(1)
}
