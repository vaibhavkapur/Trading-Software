// Command arb-demo executes a self-contained terminal walkthrough of the
// Cross-Exchange Arbitrage Monitor using recorded simulated data:
// 1. Multi-venue arbitrage detection with depth walking, FX conversion, fees, and API checks.
// 2. Failure handling: disconnect, stale quotes, clock skew, sequence gap & snapshot recovery.
// 3. Deterministic replay verification & paper adverse-selection summary.
package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"time"

	"arbmonitor/src/alertengine"
	"arbmonitor/src/api"
	"arbmonitor/src/connectors"
	"arbmonitor/src/domain"
	"arbmonitor/src/edgecalculator"
	"arbmonitor/src/metrics"
	"arbmonitor/src/normalizer"
	"arbmonitor/src/replay"
)

func main() {
	fmt.Println("========================================================================")
	fmt.Println("  CROSS-EXCHANGE ARBITRAGE MONITOR — SIMULATED PAPER-ONLY DEMO")
	fmt.Println("  Notice:", domain.PaperDisclaimer)
	fmt.Println("========================================================================")

	reg := must(normalizer.LoadRegistryFile("examples/config/instruments.json"))
	fees := must(edgecalculator.LoadFeeScheduleFile("examples/config/fees.json"))
	balances := must(edgecalculator.LoadBalanceBookFile("examples/config/balances.json"))
	cfg := loadConfig("examples/config/alert_config.json")

	// --- PART 1: Normal multi-venue scanning + live REST API demonstration ---
	fmt.Println("\n[1/3] Multi-Venue Scanning (Binance, Coinbase, Kraken) + Live API")
	fmt.Println("------------------------------------------------------------------------")
	events := must(connectors.ReadFeedJSONL("examples/fixtures/recorded_books.jsonl"))

	norm := normalizer.New(reg, cfg.MaxDepthLevels)
	feeds := connectors.NewFeedManager(connectors.NewDefaultAdapters(), norm)
	calc := edgecalculator.New(fees, balances, feeds, edgecalculator.DefaultTransferSchedule())
	collector := metrics.New()
	store := alertengine.NewMemoryAlertStore()
	engine := alertengine.New(feeds, reg, calc, store, collector, cfg)

	var lastTime time.Time
	for _, ev := range events {
		lastTime = ev.ReceivedAt.UTC()
		out := feeds.Ingest(ev)
		if out.Accepted || out.SequenceGap {
			emitted, err := engine.EvaluateAll(lastTime)
			if err != nil {
				fmt.Fprintf(os.Stderr, "evaluate error: %v\n", err)
				os.Exit(1)
			}
			for _, a := range emitted {
				opp := a.Opportunity
				fmt.Printf("  [PAPER ALERT %s] %s | BUY %s (%s) @ %s -> SELL %s (%s) @ %s\n",
					a.ID, opp.Instrument,
					opp.BuyVenue, opp.BuyInstrument, opp.BuyVWAP.FixedString(2),
					opp.SellVenue, opp.SellInstrument, opp.SellVWAP.FixedString(2))
				fmt.Printf("    qty=%s | notional=%s %s | gross=%s bps | net=%s bps | net_profit=%s %s\n",
					opp.Qty.String(), opp.ExecutableNotional.FixedString(2), opp.CommonQuote,
					opp.GrossEdgeBps.FixedString(2), opp.NetEdgeBps.FixedString(2),
					opp.NetProfit.FixedString(2), opp.CommonQuote)
				fmt.Printf("    fees=(buy:%s sell:%s) fx=%s slip_buf=%s depth_slip=%s bps | levels=(buy:%d sell:%d) | inv_capped=%v\n",
					opp.BuyFee.FixedString(2), opp.SellFee.FixedString(2),
					opp.FXCost.FixedString(2), opp.ModeledSlippageCost.FixedString(2),
					opp.DepthSlippageBps.FixedString(2),
					opp.BuyLevelsConsumed, opp.SellLevelsConsumed, opp.InventoryCapped)
				fmt.Printf("    timestamps: buy_asof=%s sell_asof=%s leg_skew=%dms buy_age=%dms sell_age=%dms\n",
					opp.BuyBookAsof.Format("15:04:05.000Z"), opp.SellBookAsof.Format("15:04:05.000Z"),
					opp.LegSkewMs, opp.BuyAgeMs, opp.SellAgeMs)
				if opp.TransferCost != nil {
					fmt.Printf("    off-band rebalance estimate (NOT in edge): %s %s (~%s %s, ~%ds confirm)\n",
						opp.TransferCost.WithdrawalFee.String(), opp.TransferCost.Asset,
						opp.TransferCost.EstimatedQuote.FixedString(2), opp.CommonQuote,
						opp.TransferCost.EstConfirmSec)
				}
			}
		}
	}

	// Query the HTTP API directly via httptest.ResponseRecorder (zero port binding required).
	srv := api.NewServer(feeds, engine, balances, collector, func() time.Time { return lastTime })
	handler := srv.Handler()

	fmt.Println("\n  API Check -> GET /v1/opportunities?min_net_bps=5")
	printJSONBody(handler, http.MethodGet, "/v1/opportunities?min_net_bps=5", "")

	fmt.Println("\n  API Check -> GET /v1/venues/health")
	printJSONBody(handler, http.MethodGet, "/v1/venues/health", "")

	fmt.Println("\n  API Check -> POST /v1/alerts/config (raise min_net_bps to 10)")
	printJSONBody(handler, http.MethodPost, "/v1/alerts/config",
		`{"instruments":["BTC/USDT","BTC/USD","ETH/USDT"],"min_net_bps":"10","max_quote_age_ms":750}`)

	// --- PART 2: Failure Scenario Demo ---
	fmt.Println("\n[2/3] Failure Demo (Stale Feed, Clock Skew, Disconnect, Sequence Gap, Duplicate)")
	fmt.Println("------------------------------------------------------------------------")
	failEvents := must(connectors.ReadFeedJSONL("examples/fixtures/failure_scenarios.jsonl"))
	failReport := must(replay.Run(replay.Options{
		Events:   failEvents,
		Registry: reg,
		Fees:     fees,
		Balances: balances,
		Config:   cfg,
	}))
	fmt.Printf("  Events processed: %d | Duplicates dropped: %d | Sequence gaps: %d | Disconnects: %d | Reconnects: %d\n",
		failReport.EventsProcessed, failReport.DuplicatesIgnored, failReport.SequenceGaps,
		failReport.Disconnects, failReport.Reconnects)
	fmt.Printf("  Suppressions recorded:\n")
	for reason, count := range failReport.Metrics.Suppressions {
		fmt.Printf("    - %-28s %d\n", reason, count)
	}
	fmt.Printf("  Alerts emitted ONLY after healthy resynced recovery: %d\n", len(failReport.Alerts))
	for _, a := range failReport.Alerts {
		fmt.Printf("    recovered alert: %s (%s %s->%s net=%s bps)\n",
			a.ID, a.Opportunity.Instrument, a.Opportunity.BuyVenue, a.Opportunity.SellVenue, a.Opportunity.NetEdgeBps.FixedString(2))
	}

	// --- PART 3: Deterministic Replay Verification ---
	fmt.Println("\n[3/3] Deterministic Replay & Paper Adverse-Selection Check")
	fmt.Println("------------------------------------------------------------------------")
	rep1 := must(replay.Run(replay.Options{
		Events:             events,
		Registry:           reg,
		Fees:               fees,
		Balances:           balances,
		Config:             cfg,
		AdverseLookaheadMs: 500,
	}))
	rep2 := must(replay.Run(replay.Options{
		Events:             events,
		Registry:           reg,
		Fees:               fees,
		Balances:           balances,
		Config:             cfg,
		AdverseLookaheadMs: 500,
	}))
	fmt.Printf("  Run 1 Digest SHA-256: %s (%d alerts)\n", rep1.DigestSHA256, len(rep1.Alerts))
	fmt.Printf("  Run 2 Digest SHA-256: %s (%d alerts)\n", rep2.DigestSHA256, len(rep2.Alerts))
	if rep1.DigestSHA256 != rep2.DigestSHA256 {
		fmt.Fprintln(os.Stderr, "  ERROR: replay digest mismatch!")
		os.Exit(1)
	}
	fmt.Println("  Determinism Status:   VERIFIED (identical alerts & SHA-256 digest)")
	for _, chk := range rep1.PaperChecks {
		fmt.Printf("  Paper Lookahead (%dms) for %s: trigger=%s bps -> after=%s bps | still_profitable=%v adverse_selection=%v\n",
			chk.LookaheadMs, chk.AlertID, chk.TriggerNetBps.FixedString(2), chk.LookaheadNetBps.FixedString(2),
			chk.StillProfitable, chk.AdverseSelection)
	}
	fmt.Println("\n========================================================================")
	fmt.Println("  Demo complete. All activity above was simulated paper-only scanning.")
	fmt.Println("========================================================================")
}

func printJSONBody(handler http.Handler, method, target, body string) {
	req := httptest.NewRequest(method, target, strings.NewReader(body))
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	var payload map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &payload)
	b, _ := json.MarshalIndent(payload, "    ", "  ")
	lines := strings.Split(string(b), "\n")
	if len(lines) > 22 {
		lines = append(lines[:22], "    ... (truncated for terminal readability)")
	}
	fmt.Printf("    HTTP %d:\n    %s\n", rec.Code, strings.Join(lines, "\n"))
}

func loadConfig(path string) domain.AlertConfig {
	data := must(os.ReadFile(path))
	cfg := domain.DefaultAlertConfig()
	if err := json.Unmarshal(data, &cfg); err != nil {
		panic(err)
	}
	return cfg
}

func must[T any](v T, err error) T {
	if err != nil {
		fmt.Fprintf(os.Stderr, "demo setup error: %v\n", err)
		os.Exit(1)
	}
	return v
}
