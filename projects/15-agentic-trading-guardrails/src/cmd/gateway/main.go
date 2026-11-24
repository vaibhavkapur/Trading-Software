package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/trading-software-projects/agentic-trading-guardrails/src/pkg/gateway"
	"github.com/trading-software-projects/agentic-trading-guardrails/src/pkg/policy"
)

func main() {
	addr := flag.String("addr", envOr("GATEWAY_ADDR", ":8080"), "HTTP listen address")
	dataDir := flag.String("data-dir", envOr("GUARDRAILS_DATA_DIR", "./data"), "Directory for durable audit log and state store")
	policyFile := flag.String("policy", envOr("GUARDRAILS_POLICY_FILE", ""), "Optional path to initial JSON policy bundle")
	flag.Parse()

	var initialPolicy *policy.PolicyBundle
	if *policyFile != "" {
		raw, err := os.ReadFile(*policyFile)
		if err != nil {
			log.Fatalf("read policy file %s: %v", *policyFile, err)
		}
		var b policy.PolicyBundle
		if err := json.Unmarshal(raw, &b); err != nil {
			log.Fatalf("parse policy file %s: %v", *policyFile, err)
		}
		initialPolicy = &b
	}

	gw, err := gateway.New(gateway.Config{
		AuditLogPath:   filepath.Join(*dataDir, "audit.jsonl"),
		StateStorePath: filepath.Join(*dataDir, "state.json"),
		InitialPolicy:  initialPolicy,
	})
	if err != nil {
		log.Fatalf("initialize gateway: %v", err)
	}
	if err := gw.SeedDemoIdentities(); err != nil {
		log.Fatalf("seed demo identities: %v", err)
	}

	srv := &http.Server{
		Addr:              *addr,
		Handler:           gw.NewHTTPHandler(),
		ReadHeaderTimeout: 5 * time.Second,
	}

	fmt.Printf("[PAPER_SIMULATED] Agentic Trading Guardrails Gateway listening on http://localhost%s\n", *addr)
	fmt.Printf("  Audit & Replay UI : http://localhost%s/\n", *addr)
	fmt.Printf("  JWKS Endpoint     : http://localhost%s/.well-known/jwks.json\n", *addr)
	fmt.Printf("  Metrics (JSON)    : http://localhost%s/v1/metrics\n", *addr)
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Fatalf("server error: %v", err)
	}
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
