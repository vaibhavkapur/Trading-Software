// riskd serves the pre-trade risk engine over HTTP.
//
// Secrets come from the environment, never from flags or files in the repo:
//
//	RISK_ADMIN_TOKEN   bearer token for kill switches, approvals, reloads
//	RISK_FEED_TOKEN    bearer token for quote ingest
//	RISK_OMS_WEBHOOKS  comma-separated URLs notified of kill-switch changes
package main

import (
	"context"
	"errors"
	"flag"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"riskengine/src/app"
)

func main() {
	addr := flag.String("addr", "127.0.0.1:8080", "listen address")
	policyPath := flag.String("policy", "examples/policy.json", "policy file")
	dataDir := flag.String("data", "data", "directory for the audit log and signing key")
	auditSync := flag.Bool("audit-sync", false, "fsync every audit record (durable against power loss, slower)")
	flag.Parse()

	var hooks []string
	if v := os.Getenv("RISK_OMS_WEBHOOKS"); v != "" {
		hooks = strings.Split(v, ",")
	}
	a, err := app.Open(app.Config{
		PolicyPath: *policyPath, DataDir: *dataDir, AuditSync: *auditSync,
		AdminToken: os.Getenv("RISK_ADMIN_TOKEN"), FeedToken: os.Getenv("RISK_FEED_TOKEN"), Webhooks: hooks,
	})
	if err != nil {
		log.Fatalf("riskd: refusing to start: %v", err)
	}
	if os.Getenv("RISK_ADMIN_TOKEN") == "" {
		log.Print("riskd: RISK_ADMIN_TOKEN is not set; kill-switch, approval and reload routes are disabled")
	}
	if os.Getenv("RISK_FEED_TOKEN") == "" {
		log.Print("riskd: RISK_FEED_TOKEN is not set; quote ingest is disabled, so every order will be rejected with NO_MARKET_DATA")
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go a.Run(ctx)

	hup := make(chan os.Signal, 1)
	signal.Notify(hup, syscall.SIGHUP)
	go func() {
		for range hup {
			if p, err := a.ReloadPolicy(); err != nil {
				log.Printf("riskd: policy reload failed, keeping current policy: %v", err)
			} else {
				log.Printf("riskd: policy %s loaded", p.Version)
			}
		}
	}()

	srv := &http.Server{Addr: *addr, Handler: a.Handler, ReadHeaderTimeout: 5 * time.Second}
	go func() {
		<-ctx.Done()
		shut, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		srv.Shutdown(shut)
	}()
	log.Printf("riskd: policy %s, %d audit records replayed, listening on %s (paper/simulated use only)",
		a.Engine.Policy().Version, a.Recovered, *addr)
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Fatal(err)
	}
	if err := a.Close(); err != nil {
		log.Printf("riskd: closing audit log: %v", err)
	}
}
