// Command arb-monitor runs the read-only Cross-Exchange Arbitrage Monitor HTTP API
// backed by a recorded/simulated feed stream. Paper/simulated mode only.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"arbmonitor/src/alertengine"
	"arbmonitor/src/api"
	"arbmonitor/src/connectors"
	"arbmonitor/src/domain"
	"arbmonitor/src/edgecalculator"
	"arbmonitor/src/metrics"
	"arbmonitor/src/normalizer"
)

func main() {
	addr := flag.String("addr", ":8080", "HTTP listen address")
	instrumentsPath := flag.String("instruments", "examples/config/instruments.json", "path to instruments JSON")
	feesPath := flag.String("fees", "examples/config/fees.json", "path to fees JSON")
	balancesPath := flag.String("balances", "examples/config/balances.json", "path to balances JSON")
	alertCfgPath := flag.String("alert-config", "examples/config/alert_config.json", "path to alert config JSON")
	feedPath := flag.String("feed", "examples/fixtures/recorded_books.jsonl", "path to recorded feed JSONL")
	archivePath := flag.String("archive", "", "optional JSONL file path to persist emitted alerts")
	realtime := flag.Bool("realtime", false, "pace recorded events by their inter-event timestamps")
	flag.Parse()

	logger := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))

	reg, err := normalizer.LoadRegistryFile(*instrumentsPath)
	if err != nil {
		logger.Error("failed to load instruments", "err", err)
		os.Exit(1)
	}
	fees, err := edgecalculator.LoadFeeScheduleFile(*feesPath)
	if err != nil {
		logger.Error("failed to load fees", "err", err)
		os.Exit(1)
	}
	balances, err := edgecalculator.LoadBalanceBookFile(*balancesPath)
	if err != nil {
		logger.Error("failed to load balances", "err", err)
		os.Exit(1)
	}
	cfg, err := loadAlertConfig(*alertCfgPath)
	if err != nil {
		logger.Error("failed to load alert config", "err", err)
		os.Exit(1)
	}
	events, err := connectors.ReadFeedJSONL(*feedPath)
	if err != nil {
		logger.Error("failed to load feed", "err", err)
		os.Exit(1)
	}

	norm := normalizer.New(reg, cfg.MaxDepthLevels)
	feeds := connectors.NewFeedManager(connectors.NewDefaultAdapters(), norm)
	calc := edgecalculator.New(fees, balances, feeds, edgecalculator.DefaultTransferSchedule())
	collector := metrics.New()

	var store alertengine.AlertStore
	if *archivePath != "" {
		store, err = alertengine.NewJSONLAlertStore(*archivePath)
		if err != nil {
			logger.Error("failed to open alert archive", "err", err)
			os.Exit(1)
		}
	} else {
		store = alertengine.NewMemoryAlertStore()
	}

	engine := alertengine.New(feeds, reg, calc, store, collector, cfg)

	var clockMu sync.RWMutex
	simNow := time.Now().UTC()
	if len(events) > 0 {
		simNow = events[0].ReceivedAt.UTC()
	}
	getClock := func() time.Time {
		clockMu.RLock()
		defer clockMu.RUnlock()
		return simNow
	}

	srv := api.NewServer(feeds, engine, balances, collector, getClock)
	httpSrv := &http.Server{
		Addr:              *addr,
		Handler:           srv.Handler(),
		ReadHeaderTimeout: 5 * time.Second,
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	go func() {
		var prevTime time.Time
		for _, ev := range events {
			if ctx.Err() != nil {
				return
			}
			if *realtime && !prevTime.IsZero() {
				delta := ev.ReceivedAt.Sub(prevTime)
				if delta > 0 {
					select {
					case <-ctx.Done():
						return
					case <-time.After(delta):
					}
				}
			}
			prevTime = ev.ReceivedAt
			clockMu.Lock()
			simNow = ev.ReceivedAt.UTC()
			clockMu.Unlock()

			outcome := feeds.Ingest(ev)
			if outcome.Accepted || outcome.SequenceGap {
				emitted, evalErr := engine.EvaluateAll(ev.ReceivedAt.UTC())
				if evalErr != nil {
					logger.Warn("alert persistence error", "err", evalErr)
				}
				for _, a := range emitted {
					logger.Info("PAPER ALERT",
						"alert_id", a.ID,
						"instrument", a.Opportunity.Instrument,
						"buy_venue", a.Opportunity.BuyVenue,
						"sell_venue", a.Opportunity.SellVenue,
						"qty", a.Opportunity.Qty.String(),
						"net_bps", a.Opportunity.NetEdgeBps.FixedString(2),
						"net_profit", a.Opportunity.NetProfit.FixedString(2),
						"disclaimer", a.Disclaimer,
					)
				}
			}
		}
		logger.Info("recorded feed ingestion complete; HTTP API remains online", "events", len(events))
	}()

	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = httpSrv.Shutdown(shutdownCtx)
	}()

	logger.Info("starting Cross-Exchange Arbitrage Monitor (PAPER READ-ONLY)",
		"addr", *addr,
		"feed", *feedPath,
	)
	if err := httpSrv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		fmt.Fprintf(os.Stderr, "server error: %v\n", err)
		os.Exit(1)
	}
}

func loadAlertConfig(path string) (domain.AlertConfig, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return domain.AlertConfig{}, err
	}
	cfg := domain.DefaultAlertConfig()
	if err := json.Unmarshal(data, &cfg); err != nil {
		return domain.AlertConfig{}, err
	}
	return cfg, nil
}
