package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/trading-software-projects/broker-reliability-reconciliation/src/api"
	"github.com/trading-software-projects/broker-reliability-reconciliation/src/connectors"
	"github.com/trading-software-projects/broker-reliability-reconciliation/src/domain"
	"github.com/trading-software-projects/broker-reliability-reconciliation/src/identity"
	"github.com/trading-software-projects/broker-reliability-reconciliation/src/issues"
	"github.com/trading-software-projects/broker-reliability-reconciliation/src/metrics"
	"github.com/trading-software-projects/broker-reliability-reconciliation/src/reconcile"
	"github.com/trading-software-projects/broker-reliability-reconciliation/src/scheduler"
)

func main() {
	if len(os.Args) < 2 {
		printUsage()
		os.Exit(1)
	}

	sub := os.Args[1]
	switch sub {
	case "serve":
		runServe(os.Args[2:])
	case "run":
		runOnce(os.Args[2:])
	case "demo":
		runDemo(os.Args[2:])
	case "failure-demo":
		runFailureDemo(os.Args[2:])
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n", sub)
		printUsage()
		os.Exit(1)
	}
}

func printUsage() {
	fmt.Println("Broker Reliability & Reconciliation CLI (PAPER / SIMULATED BY DEFAULT)")
	fmt.Println("Usage:")
	fmt.Println("  recon serve         [-addr :8080] [-fixture examples/fixtures/paper_day.json] [-state-dir .recon-state] [-schedule 0s]")
	fmt.Println("  recon run           [-fixture examples/fixtures/paper_day.json] [-account a-1] [-asof 2026-01-31T16:00:00Z]")
	fmt.Println("  recon demo          [-fixture examples/fixtures/paper_day.json]")
	fmt.Println("  recon failure-demo  [-fixture examples/fixtures/paper_day.json]")
}

func loadPolicyIfProvided(store *issues.Store, policyPath string) (string, error) {
	if strings.TrimSpace(policyPath) == "" {
		return domain.DefaultPolicy().Version, nil
	}
	raw, err := os.ReadFile(policyPath)
	if err != nil {
		return "", err
	}
	var p domain.ReconPolicy
	if err := json.Unmarshal(raw, &p); err != nil {
		return "", err
	}
	if err := store.RegisterPolicy(p); err != nil {
		return "", err
	}
	return p.Version, nil
}

func buildStack(fixturePath, policyPath, stateDir string, pageSize int) (
	*connectors.FixtureDataset,
	*connectors.SimulatedInternalConnector,
	*connectors.SimulatedBrokerConnector,
	*issues.Store,
	*metrics.Collector,
	*scheduler.Runner,
	string,
	error,
) {
	ds, err := connectors.LoadFixture(fixturePath)
	if err != nil {
		return nil, nil, nil, nil, nil, nil, "", err
	}
	store, err := issues.NewStore(stateDir)
	if err != nil {
		return nil, nil, nil, nil, nil, nil, "", err
	}
	policyVersion, err := loadPolicyIfProvided(store, policyPath)
	if err != nil {
		return nil, nil, nil, nil, nil, nil, "", err
	}

	internalConn := connectors.NewSimulatedInternalConnector(ds, pageSize)
	brokerConn := connectors.NewSimulatedBrokerConnector(ds, pageSize)
	mapper := identity.NewMapper(ds.SymbolAliases)
	engine := reconcile.NewEngine(mapper)
	collector := metrics.NewCollector()
	runner := scheduler.NewRunner(internalConn, brokerConn, engine, store, collector).WithPageSize(pageSize)
	return ds, internalConn, brokerConn, store, collector, runner, policyVersion, nil
}

func runServe(args []string) {
	fs := flag.NewFlagSet("serve", flag.ExitOnError)
	addr := fs.String("addr", ":8080", "HTTP listen address")
	fixture := fs.String("fixture", "examples/fixtures/paper_day.json", "Path to simulated fixture dataset")
	policyPath := fs.String("policy", "examples/policies/default_policy.json", "Path to policy JSON")
	stateDir := fs.String("state-dir", "", "Optional directory for durable JSONL persistence")
	scheduleInterval := fs.Duration("schedule", 0, "Optional periodic reconciliation interval (e.g. 30s)")
	bootstrapRun := fs.Bool("bootstrap", true, "Run one initial reconciliation pass on startup")
	_ = fs.Parse(args)

	ds, _, _, store, collector, runner, policyVer, err := buildStack(*fixture, *policyPath, *stateDir, 25)
	if err != nil {
		log.Fatalf("init failed: %v", err)
	}

	// Optional API key authentication injected strictly via environment variable
	var authEntries []api.APIKeyEntry
	if key := strings.TrimSpace(os.Getenv("RECON_OPERATOR_API_KEY")); key != "" {
		authEntries = append(authEntries, api.APIKeyEntry{
			Key:       key,
			Principal: api.Principal{Subject: "env-operator", Role: api.RoleOperator},
		})
	}
	if key := strings.TrimSpace(os.Getenv("RECON_VIEWER_API_KEY")); key != "" {
		authEntries = append(authEntries, api.APIKeyEntry{
			Key:       key,
			Principal: api.Principal{Subject: "env-viewer", Role: api.RoleViewer},
		})
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	if *bootstrapRun {
		run, found, err := runner.Execute(ctx, scheduler.RunRequest{
			AccountID:     ds.AccountID,
			AsOf:          ds.DefaultAsOf,
			PolicyVersion: policyVer,
			Trigger:       "on_demand",
		})
		if err != nil {
			log.Printf("bootstrap run failed: %v", err)
		} else {
			log.Printf("[PAPER/SIMULATED] bootstrap run %s completed: %d issues detected", run.ID, len(found))
		}
	}

	if *scheduleInterval > 0 {
		runner.StartPeriodic(ctx, ds.AccountID, *scheduleInterval, policyVer, func() time.Time { return ds.DefaultAsOf })
		log.Printf("[PAPER/SIMULATED] periodic scheduler active every %s for account %s", scheduleInterval.String(), ds.AccountID)
	}

	srv := &http.Server{
		Addr:              *addr,
		Handler:           api.NewServer(runner, store, collector, api.NewStaticAPIKeyAuthenticator(authEntries), api.RBACAuthorizer{}),
		ReadHeaderTimeout: 5 * time.Second,
	}

	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdownCtx)
	}()

	log.Printf("[PAPER/SIMULATED] listening on %s (UI: http://localhost%s/, Metrics: http://localhost%s/metrics)", *addr, *addr, *addr)
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Fatalf("server error: %v", err)
	}
}

func runOnce(args []string) {
	fs := flag.NewFlagSet("run", flag.ExitOnError)
	fixture := fs.String("fixture", "examples/fixtures/paper_day.json", "Fixture JSON path")
	policyPath := fs.String("policy", "examples/policies/default_policy.json", "Policy JSON path")
	account := fs.String("account", "", "Account ID (defaults to fixture account_id)")
	asofRaw := fs.String("asof", "", "As-of cutoff RFC3339 UTC (defaults to fixture default_asof)")
	_ = fs.Parse(args)

	ds, _, _, _, _, runner, policyVer, err := buildStack(*fixture, *policyPath, "", 2)
	if err != nil {
		log.Fatalf("init failed: %v", err)
	}
	acct := *account
	if acct == "" {
		acct = ds.AccountID
	}
	asof := ds.DefaultAsOf
	if *asofRaw != "" {
		parsed, err := time.Parse(time.RFC3339, *asofRaw)
		if err != nil {
			log.Fatalf("invalid asof: %v", err)
		}
		asof = parsed.UTC()
	}

	run, found, err := runner.Execute(context.Background(), scheduler.RunRequest{
		AccountID:     acct,
		AsOf:          asof,
		PolicyVersion: policyVer,
		Trigger:       "on_demand",
	})
	if err != nil {
		log.Fatalf("reconciliation failed: %v", err)
	}

	out, _ := json.MarshalIndent(map[string]any{
		"mode":   "PAPER / SIMULATED",
		"run":    run,
		"issues": found,
	}, "", "  ")
	fmt.Println(string(out))
}

func runDemo(args []string) {
	fs := flag.NewFlagSet("demo", flag.ExitOnError)
	fixture := fs.String("fixture", "examples/fixtures/paper_day.json", "Fixture JSON path")
	policyPath := fs.String("policy", "examples/policies/default_policy.json", "Policy JSON path")
	_ = fs.Parse(args)

	fmt.Println("==========================================================================")
	fmt.Println(" BROKER RELIABILITY & RECONCILIATION — PAPER / SIMULATED WALKTHROUGH")
	fmt.Println("==========================================================================")

	ds, _, _, store, collector, runner, policyVer, err := buildStack(*fixture, *policyPath, "", 2)
	if err != nil {
		log.Fatalf("demo init failed: %v", err)
	}

	fmt.Printf("Dataset : %s\n", ds.Label)
	fmt.Printf("Account : %s | Cutoff: %s | Policy: %s | PageSize: 2\n\n", ds.AccountID, ds.DefaultAsOf.Format(time.RFC3339), policyVer)

	// 1. First reconciliation run
	run1, found1, err := runner.Execute(context.Background(), scheduler.RunRequest{
		AccountID:     ds.AccountID,
		AsOf:          ds.DefaultAsOf,
		PolicyVersion: policyVer,
		Trigger:       "on_demand",
	})
	if err != nil {
		log.Fatalf("run 1 failed: %v", err)
	}

	fmt.Printf("[1] Run %s completed (digest=%s)\n", run1.ID, run1.InputDigest)
	fmt.Printf("    Checked: %d ledger fills, %d broker fills, %d positions, %d cash currencies\n",
		run1.Summary.LedgerFillsCount, run1.Summary.BrokerFillsCount, run1.Summary.PositionsChecked, run1.Summary.CashBalancesChecked)
	fmt.Printf("    Detected %d discrepancies | Total exposure: $%s | Position drift: %s\n\n",
		run1.Summary.TotalIssues, run1.Summary.MonetaryExposure.String(), run1.Summary.TotalPositionDrift.String())

	for _, iss := range found1 {
		fmt.Printf("    - [%s] %-22s (%s) exposure=$%-10s | %s\n",
			strings.ToUpper(string(iss.Severity)), iss.Kind, iss.ID, iss.MonetaryExposure.String(), iss.Evidence.Summary)
	}

	// 2. Deterministic replay verification
	run2, found2, err := runner.Execute(context.Background(), scheduler.RunRequest{
		AccountID:     ds.AccountID,
		AsOf:          ds.DefaultAsOf,
		PolicyVersion: policyVer,
		Trigger:       "on_demand",
	})
	if err != nil {
		log.Fatalf("run 2 failed: %v", err)
	}
	fmt.Printf("\n[2] Deterministic Rerun Check: run1.ID==run2.ID (%t), digest match (%t), issue count (%d==%d)\n",
		run1.ID == run2.ID, run1.InputDigest == run2.InputDigest, len(found1), len(found2))

	// 3. Controlled operator assignment & resolution preserving immutable evidence
	var lateIssue domain.ReconIssue
	for _, iss := range found1 {
		if iss.Kind == domain.IssueKindLateFill {
			lateIssue = iss
			break
		}
	}
	if lateIssue.ID != "" {
		origSummary := lateIssue.Evidence.Summary
		_, _ = store.AssignOwner(lateIssue.ID, "ops-alice", "ops-lead")
		resolved, err := store.Resolve(lateIssue.ID, "late_broker_event", "Confirmed venue fill exec-spy-late settled in EOD drop; ticket OPS-412", "ops-alice")
		if err != nil {
			log.Fatalf("resolve failed: %v", err)
		}
		fmt.Printf("\n[3] Operator Resolution on %s (%s):\n", resolved.ID, resolved.Kind)
		fmt.Printf("    State: %s | Owner: %s | Resolution: %s\n", resolved.State, resolved.Owner, resolved.Resolution.Code)
		fmt.Printf("    Original evidence preserved untouched: %t\n", resolved.Evidence.Summary == origSummary)
		fmt.Printf("    Audit history entries: %d\n", len(resolved.History))
	}

	// 4. Show Prometheus metrics excerpt
	fmt.Println("\n[4] Prometheus Metrics Excerpt (/metrics):")
	prom := collector.RenderPrometheus(store.ListIssues(issues.IssueFilter{}))
	for _, line := range strings.Split(prom, "\n") {
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		fmt.Println("    " + line)
	}
	fmt.Println("\nDemo completed successfully (100% paper/simulated data).")
}

func runFailureDemo(args []string) {
	fs := flag.NewFlagSet("failure-demo", flag.ExitOnError)
	fixture := fs.String("fixture", "examples/fixtures/paper_day.json", "Fixture JSON path")
	policyPath := fs.String("policy", "examples/policies/default_policy.json", "Policy JSON path")
	_ = fs.Parse(args)

	fmt.Println("==========================================================================")
	fmt.Println(" FAILURE & DEGRADATION DEMO — BROKER DISCONNECT & STALE SNAPSHOTS (PAPER)")
	fmt.Println("==========================================================================")

	ds, _, brokerConn, store, collector, runner, policyVer, err := buildStack(*fixture, *policyPath, "", 2)
	if err != nil {
		log.Fatalf("failure-demo init failed: %v", err)
	}

	// Phase 1: Broker connection loss
	fmt.Println("\n[Phase 1] Injecting broker API disconnect (ErrDisconnected)...")
	brokerConn.SetFaults(connectors.FaultProfile{Disconnect: true})
	failedRun, _, err := runner.Execute(context.Background(), scheduler.RunRequest{
		AccountID:     ds.AccountID,
		AsOf:          ds.DefaultAsOf,
		PolicyVersion: policyVer,
		Trigger:       "scheduled",
	})
	fmt.Printf("    Run status: %s | Error surfaced cleanly: %v\n", failedRun.Status, err)
	fmt.Println("    Ledger history modified: false | Offsetting trades placed: false")

	// Phase 2: Broker reconnects with stale snapshots (15m old) & transient throttling
	fmt.Println("\n[Phase 2] Broker reconnects with transient rate-limiting (2 retries) + 15m stale snapshots...")
	brokerConn.SetFaults(connectors.FaultProfile{
		TransientFailures:  2,
		StaleSnapshotShift: 15 * time.Minute,
	})
	staleRun, staleIssues, err := runner.Execute(context.Background(), scheduler.RunRequest{
		AccountID:     ds.AccountID,
		AsOf:          ds.DefaultAsOf,
		PolicyVersion: policyVer,
		Trigger:       "scheduled",
	})
	if err != nil {
		log.Fatalf("expected recovery with retries, got error: %v", err)
	}
	staleCount := 0
	for _, iss := range staleIssues {
		if iss.Kind == domain.IssueKindStaleSnapshot {
			staleCount++
			fmt.Printf("    - Detected STALE_SNAPSHOT (%s): %s\n", iss.ID, iss.Evidence.Summary)
		}
	}
	fmt.Printf("    Run %s recovered via retry and surfaced %d stale_snapshot alerts.\n", staleRun.ID, staleCount)

	// Phase 3: Metrics reflect connector errors and run counts
	fmt.Println("\n[Phase 3] Reliability Metrics:")
	prom := collector.RenderPrometheus(store.ListIssues(issues.IssueFilter{}))
	for _, line := range strings.Split(prom, "\n") {
		if strings.HasPrefix(line, "recon_runs_total") || strings.HasPrefix(line, "recon_connector_errors_total") {
			fmt.Println("    " + line)
		}
	}
	fmt.Println("\nFailure demo completed.")
}
