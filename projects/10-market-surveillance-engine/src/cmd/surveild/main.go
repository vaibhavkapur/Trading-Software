// surveild serves the market surveillance engine over HTTP.
//
// Simulated data only. Credentials come from the environment, never from
// flags or files in the repository:
//
//	SURV_INGEST_TOKEN    bearer token for POST /v1/events and flush
//	SURV_ADMIN_TOKEN     bearer token for rule reloads and the simulator
//	SURV_ANALYST_TOKENS  name:token,name:token, one token per analyst
//
// A route whose credential is not set is disabled.
package main

import (
	"context"
	"errors"
	"flag"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"surveillance/src/api"
	"surveillance/src/engine"
)

func main() {
	addr := flag.String("addr", "127.0.0.1:8080", "listen address")
	rulesPath := flag.String("rules", "examples/rules.json", "ruleset file")
	refPath := flag.String("refdata", "examples/refdata.json", "reference data file (empty for none)")
	dataDir := flag.String("data", "data", "directory for the journal and the case log")
	sync := flag.Bool("journal-sync", false, "fsync every journal record (durable against power loss, slower)")
	enableSim := flag.Bool("sim", false, "enable the scenario simulator routes (demo environments only)")
	flag.Parse()

	for _, name := range []string{"SURV_INGEST_TOKEN", "SURV_ADMIN_TOKEN"} {
		if v := os.Getenv(name); v != "" && len(v) < api.MinTokenLen {
			log.Fatalf("surveild: %s is shorter than %d characters", name, api.MinTokenLen)
		}
	}
	analysts, err := api.ParseAnalysts(os.Getenv("SURV_ANALYST_TOKENS"))
	if err != nil {
		log.Fatalf("surveild: SURV_ANALYST_TOKENS: %v", err)
	}

	eng, err := engine.Open(engine.Config{DataDir: *dataDir, RulesPath: *rulesPath, RefDataPath: *refPath, SyncJournal: *sync})
	if err != nil {
		log.Fatalf("surveild: refusing to start: %v", err)
	}
	defer eng.Close()

	if os.Getenv("SURV_INGEST_TOKEN") == "" {
		log.Print("surveild: SURV_INGEST_TOKEN is not set; event ingest is disabled")
	}
	if os.Getenv("SURV_ADMIN_TOKEN") == "" {
		log.Print("surveild: SURV_ADMIN_TOKEN is not set; rule reload and the simulator are disabled")
	}
	if len(analysts) == 0 {
		log.Print("surveild: SURV_ANALYST_TOKENS is not set; alerts, cases and the dashboard are disabled")
	}
	if *enableSim {
		log.Print("surveild: simulator enabled; simulated event time may run ahead of the wall clock")
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// The idle timer: lets a quiet stream's last events be evaluated.
	go func() {
		t := time.NewTicker(500 * time.Millisecond)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				if err := eng.Tick(); err != nil {
					log.Printf("surveild: tick: %v", err)
				}
			}
		}
	}()

	hup := make(chan os.Signal, 1)
	signal.Notify(hup, syscall.SIGHUP)
	go func() {
		for range hup {
			if info, err := eng.ReloadRules(); err != nil {
				log.Printf("surveild: ruleset reload failed, keeping the current ruleset: %v", err)
			} else {
				log.Printf("surveild: ruleset %s (%.12s), changed=%v", info.Version, info.Hash, info.Changed)
			}
		}
	}()

	rs := eng.Ruleset()
	srv := &http.Server{
		Addr: *addr,
		Handler: api.New(api.Config{Engine: eng, IngestToken: os.Getenv("SURV_INGEST_TOKEN"),
			AdminToken: os.Getenv("SURV_ADMIN_TOKEN"), Analysts: analysts, EnableSim: *enableSim}),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      60 * time.Second,
		IdleTimeout:       120 * time.Second,
	}
	go func() {
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdown)
	}()
	log.Printf("surveild: SIMULATED DATA ONLY. Listening on http://%s, ruleset %s (%.12s), data in %s", *addr, rs.Version, rs.Hash, *dataDir)
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Fatalf("surveild: %v", err)
	}
}
