package main

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/vaibhavkapur/Trading-Software/projects/17-multi-broker-trading-gateway/src/adapters/mock"
	"github.com/vaibhavkapur/Trading-Software/projects/17-multi-broker-trading-gateway/src/adapters/sandbox_alpha"
	"github.com/vaibhavkapur/Trading-Software/projects/17-multi-broker-trading-gateway/src/adapters/sandbox_beta"
	"github.com/vaibhavkapur/Trading-Software/projects/17-multi-broker-trading-gateway/src/api"
	"github.com/vaibhavkapur/Trading-Software/projects/17-multi-broker-trading-gateway/src/broker"
	"github.com/vaibhavkapur/Trading-Software/projects/17-multi-broker-trading-gateway/src/domain"
	"github.com/vaibhavkapur/Trading-Software/projects/17-multi-broker-trading-gateway/src/gateway"
	"github.com/vaibhavkapur/Trading-Software/projects/17-multi-broker-trading-gateway/src/ratelimit"
	"github.com/vaibhavkapur/Trading-Software/projects/17-multi-broker-trading-gateway/src/reconciler"
	"github.com/vaibhavkapur/Trading-Software/projects/17-multi-broker-trading-gateway/src/security"
	"github.com/vaibhavkapur/Trading-Software/projects/17-multi-broker-trading-gateway/src/storage"
)

type runtimeBundle struct {
	gw      *gateway.Gateway
	recon   *reconciler.Reconciler
	secrets *security.MemorySecretManager
	logBuf  *bytes.Buffer
	mockAdp *mock.Adapter
	alpha   *sandbox_alpha.Adapter
	beta    *sandbox_beta.Adapter
	clock   *ratelimit.ManualClock
}

func buildRuntime(configDir string, journalPath string, manualClock bool) (*runtimeBundle, error) {
	caps, err := domain.LoadCapabilityMatrix(filepath.Join(configDir, "capabilities.json"))
	if err != nil {
		return nil, err
	}
	insts, err := domain.LoadInstrumentMapper(filepath.Join(configDir, "instruments.json"))
	if err != nil {
		return nil, err
	}
	policy, err := security.LoadExecutionPolicy(filepath.Join(configDir, "policy.json"))
	if err != nil {
		return nil, err
	}

	var clk ratelimit.Clock = ratelimit.RealClock()
	var mclk *ratelimit.ManualClock
	if manualClock {
		mclk = ratelimit.NewManualClock(time.Date(2026, 11, 27, 10, 0, 0, 0, time.UTC))
		clk = mclk
	}

	sm := security.NewMemorySecretManager()
	// Credentials are injected at runtime outside versioned config files.
	exp := clk.Now().Add(24 * time.Hour)
	sm.PutCredential(security.Credential{Ref: "mock/api_key", Scheme: "API_KEY", Secret: security.NewSecretString("MOCK_SUPER_SECRET_KEY_9988"), ExpiresAtUTC: exp})
	sm.PutCredential(security.Credential{Ref: "sandbox-alpha/oauth_token", Scheme: "BEARER", Secret: security.NewSecretString("ALPHA_OAUTH_BEARER_SECRET_7766"), ExpiresAtUTC: exp})
	sm.PutCredential(security.Credential{Ref: "sandbox-beta/hmac_key", Scheme: "HMAC", Secret: security.NewSecretString("BETA_HMAC_SIGNING_SECRET_5544"), ExpiresAtUTC: exp})

	logBuf := &bytes.Buffer{}
	logger := security.NewRedactingLogger(logBuf, sm)

	mockCaps, _ := caps.Get("mock")
	alphaCaps, _ := caps.Get("sandbox-alpha")
	betaCaps, _ := caps.Get("sandbox-beta")

	mockAdp := mock.New(mockCaps, sm, "mock/api_key", 42, clk)
	alphaAdp := sandbox_alpha.New(alphaCaps, sm, "sandbox-alpha/oauth_token", 10, 5, clk)
	betaAdp := sandbox_beta.New(betaCaps, sm, "sandbox-beta/hmac_key", 8, 4, clk)

	reg := broker.NewRegistry()
	_ = reg.Register(mockAdp)
	_ = reg.Register(alphaAdp)
	_ = reg.Register(betaAdp)

	var st storage.Store
	if journalPath != "" {
		js, err := storage.OpenJournalStore(journalPath)
		if err != nil {
			return nil, err
		}
		st = js
	} else {
		st = storage.NewMemoryStore()
	}

	gw := gateway.New(gateway.Config{
		DefaultMode:             domain.ModePaper,
		MaxUnresolvedBeforeHalt: 1,
		Policy:                  policy,
		Capabilities:            caps,
		Instruments:             insts,
		Registry:                reg,
		Store:                   st,
		Logger:                  logger,
		Clock:                   clk,
	})
	gw.SetRateLimiter("mock", ratelimit.NewTokenBucket("mock", 50, 10, clk))
	gw.SetRateLimiter("sandbox-alpha", ratelimit.NewTokenBucket("sandbox-alpha", 10, 5, clk))
	gw.SetRateLimiter("sandbox-beta", ratelimit.NewTokenBucket("sandbox-beta", 8, 4, clk))

	return &runtimeBundle{
		gw:      gw,
		recon:   reconciler.New(gw),
		secrets: sm,
		logBuf:  logBuf,
		mockAdp: mockAdp,
		alpha:   alphaAdp,
		beta:    betaAdp,
		clock:   mclk,
	}, nil
}

func main() {
	mode := flag.String("mode", "demo", "Execution mode: demo | drill | serve")
	configDir := flag.String("config", "config", "Path to config directory")
	addr := flag.String("addr", "127.0.0.1:8080", "HTTP listen address for serve mode")
	outPath := flag.String("out", "", "Optional path to write JSON evidence output")
	flag.Parse()

	switch *mode {
	case "demo":
		if err := runDemo(*configDir, *outPath); err != nil {
			fmt.Fprintf(os.Stderr, "demo failed: %v\n", err)
			os.Exit(1)
		}
	case "drill":
		if err := runDrill(*configDir, *outPath); err != nil {
			fmt.Fprintf(os.Stderr, "drill failed: %v\n", err)
			os.Exit(1)
		}
	case "serve":
		rt, err := buildRuntime(*configDir, "data/runtime/gateway_journal.jsonl", false)
		if err != nil {
			fmt.Fprintf(os.Stderr, "startup failed: %v\n", err)
			os.Exit(1)
		}
		srv := api.NewServer(rt.gw, rt.recon)
		fmt.Printf("Multi-Broker Trading Gateway listening on http://%s (default mode: paper/sandbox; live disabled)\n", *addr)
		if err := http.ListenAndServe(*addr, srv); err != nil {
			fmt.Fprintf(os.Stderr, "server error: %v\n", err)
			os.Exit(1)
		}
	default:
		fmt.Fprintf(os.Stderr, "unknown mode: %s\n", *mode)
		os.Exit(1)
	}
}

func runDemo(configDir, outPath string) error {
	ctx := context.Background()
	rt, err := buildRuntime(configDir, "", true)
	if err != nil {
		return err
	}

	summary := map[string]any{
		"run_type":      "end_to_end_demo",
		"timestamp_utc": rt.clock.Now().Format(time.RFC3339),
	}

	// 1. Valid order on mock (paper default)
	o1, err := rt.gw.SubmitOrder(ctx, domain.OrderRequest{
		ClientOrderID: "demo-mock-1",
		BrokerID:      "mock",
		InstrumentID:  "EQ:US:AAPL",
		Side:          domain.SideBuy,
		OrderType:     domain.OrderTypeMarket,
		TimeInForce:   domain.TIFDay,
		Quantity:      10,
		Currency:      "USD",
	})
	if err != nil {
		return err
	}

	// 2. Valid limit order on sandbox-alpha (sync REST + split fills)
	rt.alpha.SetSplitPartialFills(true)
	o2, err := rt.gw.SubmitOrder(ctx, domain.OrderRequest{
		ClientOrderID: "demo-alpha-1",
		BrokerID:      "sandbox-alpha",
		Mode:          domain.ModeSandbox,
		InstrumentID:  "EQ:US:MSFT",
		Side:          domain.SideBuy,
		OrderType:     domain.OrderTypeLimit,
		TimeInForce:   domain.TIFDay,
		Quantity:      10,
		LimitPrice:    415.00,
		Currency:      "USD",
	})
	if err != nil {
		return err
	}

	// 3. Valid fractional crypto STOP_LIMIT on sandbox-beta (async queue + cursor replay)
	rt.beta.SetAsyncQueueMode(true)
	o3, err := rt.gw.SubmitOrder(ctx, domain.OrderRequest{
		ClientOrderID: "demo-beta-1",
		BrokerID:      "sandbox-beta",
		Mode:          domain.ModeSandbox,
		InstrumentID:  "CRYPTO:SPOT:BTC-USD",
		Side:          domain.SideBuy,
		OrderType:     domain.OrderTypeMarket,
		TimeInForce:   domain.TIFGTC,
		Quantity:      0.5,
		Currency:      "USD",
	})
	if err != nil {
		return err
	}
	rt.beta.ProcessQueue()
	replayed, nextCursor, err := rt.gw.ReplayBrokerEvents(ctx, "sandbox-beta")
	if err != nil {
		return err
	}
	o3Updated, _ := rt.gw.Store().GetOrder(o3.OrderID)

	// 4. Reject unsupported order type BEFORE dispatch (STOP_LIMIT on sandbox-alpha)
	alphaDispatchesBefore := rt.alpha.DispatchCount()
	_, unsupErr := rt.gw.SubmitOrder(ctx, domain.OrderRequest{
		ClientOrderID: "demo-alpha-unsup",
		BrokerID:      "sandbox-alpha",
		Mode:          domain.ModeSandbox,
		InstrumentID:  "EQ:US:AAPL",
		Side:          domain.SideSell,
		OrderType:     domain.OrderTypeStopLimit,
		TimeInForce:   domain.TIFDay,
		Quantity:      5,
		LimitPrice:    190,
		StopPrice:     191,
		Currency:      "USD",
	})
	alphaDispatchesAfter := rt.alpha.DispatchCount()

	// 5. Reject LIVE mode by policy
	_, liveErr := rt.gw.SubmitOrder(ctx, domain.OrderRequest{
		ClientOrderID: "demo-live-denied",
		BrokerID:      "sandbox-alpha",
		Mode:          domain.ModeLive,
		InstrumentID:  "EQ:US:AAPL",
		Side:          domain.SideBuy,
		OrderType:     domain.OrderTypeMarket,
		TimeInForce:   domain.TIFDay,
		Quantity:      1,
		Currency:      "USD",
	})

	// 6. Independent reconciliation across all 3 brokers
	rep, err := rt.recon.ReconcileAll(ctx)
	if err != nil {
		return err
	}

	// Verify zero secret leakage in structured logs
	logs := rt.logBuf.String()
	leaked := false
	for _, sec := range rt.secrets.RegisteredSecretValues() {
		if strings.Contains(logs, sec) {
			leaked = true
		}
	}

	summary["orders"] = []domain.Order{o1, o2, o3Updated}
	summary["beta_replayed_events"] = replayed
	summary["beta_replay_cursor"] = nextCursor
	summary["unsupported_order_error"] = string(domain.ErrorCodeOf(unsupErr))
	summary["unsupported_zero_dispatch"] = alphaDispatchesBefore == alphaDispatchesAfter
	summary["live_mode_error"] = string(domain.ErrorCodeOf(liveErr))
	summary["reconciliation"] = rep
	summary["metrics"] = rt.gw.Metrics.Snapshot()
	summary["secret_leak_detected"] = leaked

	b, _ := json.MarshalIndent(summary, "", "  ")
	if outPath != "" {
		_ = os.MkdirAll(filepath.Dir(outPath), 0o755)
		if err := os.WriteFile(outPath, b, 0o644); err != nil {
			return err
		}
	}
	fmt.Println(string(b))
	return nil
}

func runDrill(configDir, outPath string) error {
	ctx := context.Background()
	rt, err := buildRuntime(configDir, "", true)
	if err != nil {
		return err
	}

	results := make([]map[string]any, 0, 8)

	// Scenario 1: Rate limit + Retry-After recovery
	rt.alpha.InjectUpstreamRateLimitOnce()
	_, rlErr := rt.gw.SubmitOrder(ctx, domain.OrderRequest{
		ClientOrderID: "drill-rl-1", BrokerID: "sandbox-alpha", Mode: domain.ModeSandbox,
		InstrumentID: "EQ:US:AAPL", Side: domain.SideBuy, OrderType: domain.OrderTypeLimit,
		TimeInForce: domain.TIFDay, Quantity: 2, LimitPrice: 190.00, Currency: "USD",
	})
	// Immediately blocked by cooldown; advance clock past cooldown and recover
	_, rlCooldownErr := rt.gw.SubmitOrder(ctx, domain.OrderRequest{
		ClientOrderID: "drill-rl-2", BrokerID: "sandbox-alpha", Mode: domain.ModeSandbox,
		InstrumentID: "EQ:US:AAPL", Side: domain.SideBuy, OrderType: domain.OrderTypeLimit,
		TimeInForce: domain.TIFDay, Quantity: 2, LimitPrice: 190.00, Currency: "USD",
	})
	rt.clock.Advance(1 * time.Second)
	rlRecovered, rlRecErr := rt.gw.SubmitOrder(ctx, domain.OrderRequest{
		ClientOrderID: "drill-rl-3", BrokerID: "sandbox-alpha", Mode: domain.ModeSandbox,
		InstrumentID: "EQ:US:AAPL", Side: domain.SideBuy, OrderType: domain.OrderTypeLimit,
		TimeInForce: domain.TIFDay, Quantity: 2, LimitPrice: 190.00, Currency: "USD",
	})
	results = append(results, map[string]any{
		"scenario":       "rate_limit",
		"initial_error":  string(domain.ErrorCodeOf(rlErr)),
		"cooldown_error": string(domain.ErrorCodeOf(rlCooldownErr)),
		"recovered":      rlRecErr == nil && rlRecovered.Status == domain.StatusAccepted,
	})

	// Scenario 2: Expired credential + rotation recovery + zero log leak
	_ = rt.secrets.ExpireCredential("sandbox-alpha/oauth_token", rt.clock.Now().Add(-time.Minute))
	_, authErr := rt.gw.SubmitOrder(ctx, domain.OrderRequest{
		ClientOrderID: "drill-auth-1", BrokerID: "sandbox-alpha", Mode: domain.ModeSandbox,
		InstrumentID: "EQ:US:AAPL", Side: domain.SideBuy, OrderType: domain.OrderTypeLimit,
		TimeInForce: domain.TIFDay, Quantity: 2, LimitPrice: 190.00, Currency: "USD",
	})
	_, _ = rt.secrets.RotateCredential("sandbox-alpha/oauth_token", security.NewSecretString("ALPHA_ROTATED_SECRET_NEW_1122"), rt.clock.Now().Add(24*time.Hour))
	authRec, authRecErr := rt.gw.SubmitOrder(ctx, domain.OrderRequest{
		ClientOrderID: "drill-auth-2", BrokerID: "sandbox-alpha", Mode: domain.ModeSandbox,
		InstrumentID: "EQ:US:AAPL", Side: domain.SideBuy, OrderType: domain.OrderTypeLimit,
		TimeInForce: domain.TIFDay, Quantity: 2, LimitPrice: 190.00, Currency: "USD",
	})
	results = append(results, map[string]any{
		"scenario":      "expired_credential",
		"initial_error": string(domain.ErrorCodeOf(authErr)),
		"recovered":     authRecErr == nil && authRec.Status == domain.StatusAccepted,
	})

	// Scenario 3: Symbol mismatch (rejected pre-dispatch with 0 adapter calls)
	beforeSym := rt.alpha.DispatchCount()
	_, symErr := rt.gw.SubmitOrder(ctx, domain.OrderRequest{
		ClientOrderID: "drill-sym-1", BrokerID: "sandbox-alpha", Mode: domain.ModeSandbox,
		InstrumentID: "CRYPTO:SPOT:BTC-USD", Side: domain.SideBuy, OrderType: domain.OrderTypeMarket,
		TimeInForce: domain.TIFDay, Quantity: 1, Currency: "USD",
	})
	results = append(results, map[string]any{
		"scenario":        "symbol_mismatch",
		"error":           string(domain.ErrorCodeOf(symErr)),
		"zero_dispatches": beforeSym == rt.alpha.DispatchCount(),
	})

	// Scenario 4 & 5: Out-of-order fill + duplicate webhook
	oooOrd, _ := rt.gw.SubmitOrder(ctx, domain.OrderRequest{
		ClientOrderID: "drill-ooo-1", BrokerID: "sandbox-alpha", Mode: domain.ModeSandbox,
		InstrumentID: "EQ:US:AAPL", Side: domain.SideBuy, OrderType: domain.OrderTypeLimit,
		TimeInForce: domain.TIFDay, Quantity: 10, LimitPrice: 190.00, Currency: "USD",
	})
	// Deliver fill seq=2 FIRST, then seq=1, then duplicate seq=2
	_, _, _ = rt.gw.IngestWebhook(domain.WebhookEvent{
		EventID: "drill-wh-2", BrokerID: "sandbox-alpha", EventType: "FILL",
		BrokerOrderID: oooOrd.BrokerOrderID, ClientOrderID: oooOrd.ClientOrderID,
		BrokerFillID: "drill-fill-2", Sequence: 2, FillQty: 6, FillPrice: 190.40, Fee: 0.12, Currency: "USD",
		EventTimeUTC: rt.clock.Now().Add(2 * time.Second),
	})
	_, _, _ = rt.gw.IngestWebhook(domain.WebhookEvent{
		EventID: "drill-wh-1", BrokerID: "sandbox-alpha", EventType: "FILL",
		BrokerOrderID: oooOrd.BrokerOrderID, ClientOrderID: oooOrd.ClientOrderID,
		BrokerFillID: "drill-fill-1", Sequence: 1, FillQty: 4, FillPrice: 190.00, Fee: 0.08, Currency: "USD",
		EventTimeUTC: rt.clock.Now().Add(1 * time.Second),
	})
	oooFinal, dupApplied, _ := rt.gw.IngestWebhook(domain.WebhookEvent{
		EventID: "drill-wh-2", BrokerID: "sandbox-alpha", EventType: "FILL",
		BrokerOrderID: oooOrd.BrokerOrderID, ClientOrderID: oooOrd.ClientOrderID,
		BrokerFillID: "drill-fill-2", Sequence: 2, FillQty: 6, FillPrice: 190.40, Fee: 0.12, Currency: "USD",
		EventTimeUTC: rt.clock.Now().Add(2 * time.Second),
	})
	results = append(results, map[string]any{
		"scenario":          "duplicate_webhook_and_out_of_order_fill",
		"duplicate_applied": dupApplied,
		"final_status":      oooFinal.Status,
		"filled_qty":        oooFinal.FilledQuantity,
		"avg_fill_price":    oooFinal.AvgFillPrice,
	})

	// Scenario 6: Unknown submit result -> NO blind retry -> reconciled to broker truth
	rt.beta.InjectUnknownSubmitOnce()
	unkOrd, unkErr := rt.gw.SubmitOrder(ctx, domain.OrderRequest{
		ClientOrderID: "drill-unk-1", BrokerID: "sandbox-beta", Mode: domain.ModeSandbox,
		InstrumentID: "EQ:US:AAPL", Side: domain.SideBuy, OrderType: domain.OrderTypeMarket,
		TimeInForce: domain.TIFDay, Quantity: 5, Currency: "USD",
	})
	betaDispatchesBeforeReplay := rt.beta.DispatchCount()
	_, replayUnkErr := rt.gw.SubmitOrder(ctx, domain.OrderRequest{
		ClientOrderID: "drill-unk-1", BrokerID: "sandbox-beta", Mode: domain.ModeSandbox,
		InstrumentID: "EQ:US:AAPL", Side: domain.SideBuy, OrderType: domain.OrderTypeMarket,
		TimeInForce: domain.TIFDay, Quantity: 5, Currency: "USD",
	})
	noBlindRetry := betaDispatchesBeforeReplay == rt.beta.DispatchCount()
	unkRecon, _ := rt.recon.ReconcileBroker(ctx, "sandbox-beta")
	unkResolved, _ := rt.gw.Store().GetOrder(unkOrd.OrderID)
	results = append(results, map[string]any{
		"scenario":        "unknown_submit_result",
		"submit_error":    string(domain.ErrorCodeOf(unkErr)),
		"replay_error":    string(domain.ErrorCodeOf(replayUnkErr)),
		"no_blind_retry":  noBlindRetry,
		"resolved_status": unkResolved.Status,
		"resolved_filled": unkResolved.FilledQuantity,
		"recon_findings":  len(unkRecon.Findings),
	})

	// Scenario 7: Cancel timeout -> CANCEL_PENDING -> reconciled to terminal state
	cxlOrd, _ := rt.gw.SubmitOrder(ctx, domain.OrderRequest{
		ClientOrderID: "drill-cxl-1", BrokerID: "sandbox-beta", Mode: domain.ModeSandbox,
		InstrumentID: "EQ:US:AAPL", Side: domain.SideBuy, OrderType: domain.OrderTypeLimit,
		TimeInForce: domain.TIFDay, Quantity: 5, LimitPrice: 180.00, Currency: "USD",
	})
	rt.beta.InjectCancelTimeoutOnce("cancelled")
	cxlPending, cxlErr := rt.gw.CancelOrder(ctx, cxlOrd.OrderID)
	_, _ = rt.recon.ReconcileBroker(ctx, "sandbox-beta")
	cxlResolved, _ := rt.gw.Store().GetOrder(cxlOrd.OrderID)
	results = append(results, map[string]any{
		"scenario":        "cancel_timeout",
		"cancel_error":    string(domain.ErrorCodeOf(cxlErr)),
		"pending_status":  cxlPending.Status,
		"resolved_status": cxlResolved.Status,
	})

	// Scenario 8: Fail-closed reconciliation drill on phantom broker order + operator resume
	rt.mockAdp.InjectPhantomOrder("AAPL", 50, 195.00)
	phantomRep, _ := rt.recon.ReconcileBroker(ctx, "mock")
	_, haltedErr := rt.gw.SubmitOrder(ctx, domain.OrderRequest{
		ClientOrderID: "drill-while-halted", BrokerID: "mock", Mode: domain.ModePaper,
		InstrumentID: "EQ:US:AAPL", Side: domain.SideBuy, OrderType: domain.OrderTypeMarket,
		TimeInForce: domain.TIFDay, Quantity: 1, Currency: "USD",
	})
	_ = rt.gw.Resume("oncall-sre-01", "phantom order audited and cleared in drill")
	results = append(results, map[string]any{
		"scenario":            "reconciliation_drift_fail_closed_drill",
		"triggered_fail_safe": phantomRep.TriggeredFailSafe,
		"unresolved_count":    phantomRep.UnresolvedCount,
		"blocked_order_error": string(domain.ErrorCodeOf(haltedErr)),
		"resumed_cleanly":     !rt.gw.HaltStatus().Halted,
	})

	// Secret audit across full drill log stream
	logs := rt.logBuf.String()
	secretLeaked := false
	for _, sec := range rt.secrets.RegisteredSecretValues() {
		if strings.Contains(logs, sec) {
			secretLeaked = true
		}
	}

	payload := map[string]any{
		"run_type":             "reconciliation_and_fault_drill",
		"timestamp_utc":        rt.clock.Now().Format(time.RFC3339),
		"scenarios":            results,
		"metrics":              rt.gw.Metrics.Snapshot(),
		"secret_leak_detected": secretLeaked,
	}
	b, _ := json.MarshalIndent(payload, "", "  ")
	if outPath != "" {
		_ = os.MkdirAll(filepath.Dir(outPath), 0o755)
		if err := os.WriteFile(outPath, b, 0o644); err != nil {
			return err
		}
	}
	fmt.Println(string(b))
	return nil
}
