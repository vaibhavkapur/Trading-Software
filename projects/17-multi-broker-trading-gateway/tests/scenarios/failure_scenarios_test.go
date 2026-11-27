package scenarios_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/vaibhavkapur/Trading-Software/projects/17-multi-broker-trading-gateway/src/adapters/mock"
	"github.com/vaibhavkapur/Trading-Software/projects/17-multi-broker-trading-gateway/src/adapters/sandbox_alpha"
	"github.com/vaibhavkapur/Trading-Software/projects/17-multi-broker-trading-gateway/src/adapters/sandbox_beta"
	"github.com/vaibhavkapur/Trading-Software/projects/17-multi-broker-trading-gateway/src/broker"
	"github.com/vaibhavkapur/Trading-Software/projects/17-multi-broker-trading-gateway/src/domain"
	"github.com/vaibhavkapur/Trading-Software/projects/17-multi-broker-trading-gateway/src/gateway"
	"github.com/vaibhavkapur/Trading-Software/projects/17-multi-broker-trading-gateway/src/ratelimit"
	"github.com/vaibhavkapur/Trading-Software/projects/17-multi-broker-trading-gateway/src/reconciler"
	"github.com/vaibhavkapur/Trading-Software/projects/17-multi-broker-trading-gateway/src/security"
	"github.com/vaibhavkapur/Trading-Software/projects/17-multi-broker-trading-gateway/src/storage"
)

type scenarioHarness struct {
	gw      *gateway.Gateway
	recon   *reconciler.Reconciler
	secrets *security.MemorySecretManager
	logBuf  *bytes.Buffer
	clk     *ratelimit.ManualClock
	mockAdp *mock.Adapter
	alpha   *sandbox_alpha.Adapter
	beta    *sandbox_beta.Adapter
}

func newHarness(t testing.TB) *scenarioHarness {
	t.Helper()
	caps, err := domain.LoadCapabilityMatrix(filepath.Join("..", "..", "config", "capabilities.json"))
	if err != nil {
		t.Fatalf("load caps: %v", err)
	}
	insts, err := domain.LoadInstrumentMapper(filepath.Join("..", "..", "config", "instruments.json"))
	if err != nil {
		t.Fatalf("load instruments: %v", err)
	}
	policy, err := security.LoadExecutionPolicy(filepath.Join("..", "..", "config", "policy.json"))
	if err != nil {
		t.Fatalf("load policy: %v", err)
	}

	clk := ratelimit.NewManualClock(time.Date(2026, 11, 27, 10, 0, 0, 0, time.UTC))
	sm := security.NewMemorySecretManager()
	exp := clk.Now().Add(24 * time.Hour)
	sm.PutCredential(security.Credential{Ref: "mock/api_key", Scheme: "API_KEY", Secret: security.NewSecretString("SCEN_MOCK_SECRET_A1B2"), ExpiresAtUTC: exp})
	sm.PutCredential(security.Credential{Ref: "sandbox-alpha/oauth_token", Scheme: "BEARER", Secret: security.NewSecretString("SCEN_ALPHA_SECRET_C3D4"), ExpiresAtUTC: exp})
	sm.PutCredential(security.Credential{Ref: "sandbox-beta/hmac_key", Scheme: "HMAC", Secret: security.NewSecretString("SCEN_BETA_SECRET_E5F6"), ExpiresAtUTC: exp})

	logBuf := &bytes.Buffer{}
	logger := security.NewRedactingLogger(logBuf, sm)

	mockCaps, _ := caps.Get("mock")
	alphaCaps, _ := caps.Get("sandbox-alpha")
	betaCaps, _ := caps.Get("sandbox-beta")

	mockAdp := mock.New(mockCaps, sm, "mock/api_key", 42, clk)
	alphaAdp := sandbox_alpha.New(alphaCaps, sm, "sandbox-alpha/oauth_token", 10, 5, clk)
	betaAdp := sandbox_beta.New(betaCaps, sm, "sandbox-beta/hmac_key", 10, 5, clk)

	reg := broker.NewRegistry()
	_ = reg.Register(mockAdp)
	_ = reg.Register(alphaAdp)
	_ = reg.Register(betaAdp)

	gw := gateway.New(gateway.Config{
		DefaultMode:             domain.ModePaper,
		MaxUnresolvedBeforeHalt: 1,
		Policy:                  policy,
		Capabilities:            caps,
		Instruments:             insts,
		Registry:                reg,
		Store:                   storage.NewMemoryStore(),
		Logger:                  logger,
		Clock:                   clk,
	})
	gw.SetRateLimiter("mock", ratelimit.NewTokenBucket("mock", 1000, 1000, clk))
	gw.SetRateLimiter("sandbox-alpha", ratelimit.NewTokenBucket("sandbox-alpha", 10, 5, clk))
	gw.SetRateLimiter("sandbox-beta", ratelimit.NewTokenBucket("sandbox-beta", 10, 5, clk))

	return &scenarioHarness{
		gw:      gw,
		recon:   reconciler.New(gw),
		secrets: sm,
		logBuf:  logBuf,
		clk:     clk,
		mockAdp: mockAdp,
		alpha:   alphaAdp,
		beta:    betaAdp,
	}
}

func (h *scenarioHarness) assertNoSecretLeaks(t *testing.T) {
	t.Helper()
	logs := h.logBuf.String()
	for _, sec := range h.secrets.RegisteredSecretValues() {
		if strings.Contains(logs, sec) {
			t.Fatalf("SECRET LEAKED IN LOGS: %s", sec)
		}
	}
}

// Scenario 1: Rate limit (upstream 429 Retry-After + local cooldown enforcement + window recovery)
func TestScenario_RateLimit(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	h.alpha.InjectUpstreamRateLimitOnce()
	_, err := h.gw.SubmitOrder(ctx, domain.OrderRequest{
		ClientOrderID: "sc-rl-1", BrokerID: "sandbox-alpha", Mode: domain.ModeSandbox,
		InstrumentID: "EQ:US:AAPL", Side: domain.SideBuy, OrderType: domain.OrderTypeLimit,
		TimeInForce: domain.TIFDay, Quantity: 2, LimitPrice: 190.0, Currency: "USD",
	})
	if domain.ErrorCodeOf(err) != domain.ErrCodeRateLimited {
		t.Fatalf("expected RATE_LIMITED, got %v", err)
	}

	// Next order during Retry-After window MUST be blocked locally before adapter dispatch
	beforeCooldown := h.alpha.DispatchCount()
	_, err2 := h.gw.SubmitOrder(ctx, domain.OrderRequest{
		ClientOrderID: "sc-rl-2", BrokerID: "sandbox-alpha", Mode: domain.ModeSandbox,
		InstrumentID: "EQ:US:AAPL", Side: domain.SideBuy, OrderType: domain.OrderTypeLimit,
		TimeInForce: domain.TIFDay, Quantity: 2, LimitPrice: 190.0, Currency: "USD",
	})
	if domain.ErrorCodeOf(err2) != domain.ErrCodeRateLimited {
		t.Fatalf("expected local cooldown RATE_LIMITED, got %v", err2)
	}
	if h.alpha.DispatchCount() != beforeCooldown {
		t.Fatalf("expected 0 broker dispatches during Retry-After cooldown")
	}

	// Advance past Retry-After window -> recovers cleanly
	h.clk.Advance(1 * time.Second)
	ord, err3 := h.gw.SubmitOrder(ctx, domain.OrderRequest{
		ClientOrderID: "sc-rl-3", BrokerID: "sandbox-alpha", Mode: domain.ModeSandbox,
		InstrumentID: "EQ:US:AAPL", Side: domain.SideBuy, OrderType: domain.OrderTypeLimit,
		TimeInForce: domain.TIFDay, Quantity: 2, LimitPrice: 190.0, Currency: "USD",
	})
	if err3 != nil || ord.Status != domain.StatusAccepted {
		t.Fatalf("expected recovery after rate limit window, ord=%+v err=%v", ord, err3)
	}
	h.assertNoSecretLeaks(t)
}

// Scenario 2: Expired credential (rejected with AUTH_EXPIRED, zero secret leakage, recovers on rotation)
func TestScenario_ExpiredCredential(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	_ = h.secrets.ExpireCredential("sandbox-alpha/oauth_token", h.clk.Now().Add(-time.Second))
	_, err := h.gw.SubmitOrder(ctx, domain.OrderRequest{
		ClientOrderID: "sc-auth-1", BrokerID: "sandbox-alpha", Mode: domain.ModeSandbox,
		InstrumentID: "EQ:US:AAPL", Side: domain.SideBuy, OrderType: domain.OrderTypeLimit,
		TimeInForce: domain.TIFDay, Quantity: 2, LimitPrice: 190.0, Currency: "USD",
	})
	if domain.ErrorCodeOf(err) != domain.ErrCodeAuthExpired {
		t.Fatalf("expected AUTH_EXPIRED, got %v", err)
	}

	// Rotate credential -> recovers cleanly
	_, err = h.secrets.RotateCredential("sandbox-alpha/oauth_token", security.NewSecretString("SCEN_ALPHA_ROTATED_G7H8"), h.clk.Now().Add(time.Hour))
	if err != nil {
		t.Fatalf("rotate credential: %v", err)
	}
	ord, err := h.gw.SubmitOrder(ctx, domain.OrderRequest{
		ClientOrderID: "sc-auth-2", BrokerID: "sandbox-alpha", Mode: domain.ModeSandbox,
		InstrumentID: "EQ:US:AAPL", Side: domain.SideBuy, OrderType: domain.OrderTypeLimit,
		TimeInForce: domain.TIFDay, Quantity: 2, LimitPrice: 190.0, Currency: "USD",
	})
	if err != nil || ord.Status != domain.StatusAccepted {
		t.Fatalf("expected recovery after credential rotation, ord=%+v err=%v", ord, err)
	}
	h.assertNoSecretLeaks(t)
}

// Scenario 3: Symbol mismatch (rejected before dispatch with zero adapter calls)
func TestScenario_SymbolMismatch(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	before := h.alpha.DispatchCount()
	_, err := h.gw.SubmitOrder(ctx, domain.OrderRequest{
		ClientOrderID: "sc-sym-1", BrokerID: "sandbox-alpha", Mode: domain.ModeSandbox,
		InstrumentID: "CRYPTO:SPOT:BTC-USD", Side: domain.SideBuy, OrderType: domain.OrderTypeMarket,
		TimeInForce: domain.TIFDay, Quantity: 1, Currency: "USD",
	})
	if domain.ErrorCodeOf(err) != domain.ErrCodeSymbolMismatch {
		t.Fatalf("expected SYMBOL_MISMATCH, got %v", err)
	}
	if h.alpha.DispatchCount() != before {
		t.Fatalf("symbol mismatch must reject before broker dispatch")
	}
	h.assertNoSecretLeaks(t)
}

// Scenario 4: Duplicate webhook (driven by data/fixtures/webhooks.json)
func TestScenario_DuplicateWebhook(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	ord, err := h.gw.SubmitOrder(ctx, domain.OrderRequest{
		ClientOrderID: "cli-wh-001", BrokerID: "sandbox-alpha", Mode: domain.ModeSandbox,
		InstrumentID: "EQ:US:AAPL", Side: domain.SideBuy, OrderType: domain.OrderTypeLimit,
		TimeInForce: domain.TIFDay, Quantity: 10, LimitPrice: 190.0, Currency: "USD",
	})
	if err != nil {
		t.Fatalf("submit order: %v", err)
	}

	raw, err := os.ReadFile(filepath.Join("..", "..", "data", "fixtures", "webhooks.json"))
	if err != nil {
		t.Fatalf("read webhooks fixture: %v", err)
	}
	var fixture struct {
		Webhooks []domain.WebhookEvent `json:"webhooks"`
	}
	if err := json.Unmarshal(raw, &fixture); err != nil {
		t.Fatalf("parse webhooks fixture: %v", err)
	}

	appliedCount := 0
	for _, wh := range fixture.Webhooks {
		wh.BrokerOrderID = ord.BrokerOrderID
		_, applied, err := h.gw.IngestWebhook(wh)
		if err != nil {
			t.Fatalf("ingest webhook %s: %v", wh.EventID, err)
		}
		if applied {
			appliedCount++
		}
	}
	if appliedCount != 2 {
		t.Fatalf("expected exactly 2 unique fills applied (1 duplicate ignored), got %d", appliedCount)
	}

	// Also test a NEW EventID that carries a DUPLICATE BrokerFillID
	_, dupFillApplied, err := h.gw.IngestWebhook(domain.WebhookEvent{
		EventID: "wh-evt-9999-new-id", BrokerID: "sandbox-alpha", EventType: "FILL",
		BrokerOrderID: ord.BrokerOrderID, ClientOrderID: "cli-wh-001",
		BrokerFillID: "alpha-fill-2", Sequence: 2, FillQty: 6, FillPrice: 195.40, Fee: 0.12, Currency: "USD",
		EventTimeUTC: h.clk.Now(),
	})
	if err != nil || dupFillApplied {
		t.Fatalf("expected duplicate BrokerFillID to be ignored: applied=%v err=%v", dupFillApplied, err)
	}

	finalOrd, _ := h.gw.Store().GetOrder(ord.OrderID)
	if finalOrd.Status != domain.StatusFilled || finalOrd.FilledQuantity != 10 {
		t.Fatalf("unexpected final order after duplicate webhooks: %+v", finalOrd)
	}
	expectedVWAP := (4*195.20 + 6*195.40) / 10.0
	if math.Abs(finalOrd.AvgFillPrice-expectedVWAP) > 1e-6 {
		t.Fatalf("expected VWAP %v, got %v", expectedVWAP, finalOrd.AvgFillPrice)
	}
	if h.gw.Metrics.Snapshot().DuplicateWebhooksIgnored < 2 {
		t.Fatalf("expected >=2 duplicate webhooks counted")
	}
}

// Scenario 5: Out-of-order fill (seq=2 before seq=1, AND stale ACCEPTED status arriving after FILLED)
func TestScenario_OutOfOrderFill(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	h.beta.SetAsyncQueueMode(true)
	ord, err := h.gw.SubmitOrder(ctx, domain.OrderRequest{
		ClientOrderID: "sc-ooo-1", BrokerID: "sandbox-beta", Mode: domain.ModeSandbox,
		InstrumentID: "EQ:US:MSFT", Side: domain.SideBuy, OrderType: domain.OrderTypeLimit,
		TimeInForce: domain.TIFDay, Quantity: 10, LimitPrice: 400.0, Currency: "USD",
	})
	if err != nil || ord.Status != domain.StatusPendingNew {
		t.Fatalf("expected PENDING_NEW on async beta, got ord=%+v err=%v", ord, err)
	}

	// Fill #2 arrives BEFORE ACK and BEFORE Fill #1
	oAfterF2, _, err := h.gw.IngestWebhook(domain.WebhookEvent{
		EventID: "ooo-wh-2", BrokerID: "sandbox-beta", EventType: "FILL",
		BrokerOrderID: ord.BrokerOrderID, ClientOrderID: ord.ClientOrderID,
		BrokerFillID: "beta-ooo-fill-2", Sequence: 2, FillQty: 7, FillPrice: 400.50, Fee: 0.28, Currency: "USD",
		EventTimeUTC: h.clk.Now().Add(2 * time.Second),
	})
	if err != nil || oAfterF2.Status != domain.StatusPartiallyFilled || oAfterF2.FilledQuantity != 7 {
		t.Fatalf("unexpected state after out-of-order fill #2: %+v err=%v", oAfterF2, err)
	}

	// Fill #1 arrives late
	oAfterF1, _, err := h.gw.IngestWebhook(domain.WebhookEvent{
		EventID: "ooo-wh-1", BrokerID: "sandbox-beta", EventType: "FILL",
		BrokerOrderID: ord.BrokerOrderID, ClientOrderID: ord.ClientOrderID,
		BrokerFillID: "beta-ooo-fill-1", Sequence: 1, FillQty: 3, FillPrice: 399.50, Fee: 0.12, Currency: "USD",
		EventTimeUTC: h.clk.Now().Add(1 * time.Second),
	})
	if err != nil || oAfterF1.Status != domain.StatusFilled || oAfterF1.FilledQuantity != 10 {
		t.Fatalf("expected FILLED after fill #1 arrives, got %+v err=%v", oAfterF1, err)
	}

	// Stale ACCEPTED status webhook arrives AFTER FILLED -> MUST NOT regress status!
	oAfterStale, applied, err := h.gw.IngestWebhook(domain.WebhookEvent{
		EventID: "ooo-wh-stale-acc", BrokerID: "sandbox-beta", EventType: "STATUS",
		BrokerOrderID: ord.BrokerOrderID, ClientOrderID: ord.ClientOrderID,
		Status: domain.StatusAccepted, Sequence: 0, EventTimeUTC: h.clk.Now(),
	})
	if err != nil || applied || oAfterStale.Status != domain.StatusFilled {
		t.Fatalf("stale status must not regress FILLED order: applied=%v status=%s err=%v", applied, oAfterStale.Status, err)
	}

	fills := h.gw.Store().ListFillsByOrder(ord.OrderID)
	if len(fills) != 2 || fills[0].Sequence != 1 || fills[1].Sequence != 2 {
		t.Fatalf("expected fills sorted canonically by sequence [1, 2], got %+v", fills)
	}
	expectedVWAP := (3*399.50 + 7*400.50) / 10.0
	if math.Abs(oAfterF1.AvgFillPrice-expectedVWAP) > 1e-6 {
		t.Fatalf("expected VWAP %v, got %v", expectedVWAP, oAfterF1.AvgFillPrice)
	}
	if h.gw.Metrics.Snapshot().OutOfOrderFillsHandled < 2 {
		t.Fatalf("expected out-of-order fill metric incremented")
	}
}

// Scenario 6: Unknown submit result -> NEVER blindly retried -> reconciled to broker truth (both accepted & dropped)
func TestScenario_UnknownSubmitResult(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	// Case 6A: Broker ACTUALLY accepted & filled, transport dropped response
	h.mockAdp.InjectNextSubmitFault(mock.FaultUnknownSubmitAccepted)
	reqA := domain.OrderRequest{
		ClientOrderID: "sc-unk-accepted", BrokerID: "mock", Mode: domain.ModePaper,
		InstrumentID: "EQ:US:AAPL", Side: domain.SideBuy, OrderType: domain.OrderTypeMarket,
		TimeInForce: domain.TIFDay, Quantity: 5, Currency: "USD",
	}
	ordA, errA := h.gw.SubmitOrder(ctx, reqA)
	if domain.ErrorCodeOf(errA) != domain.ErrCodeUnknownOutcome || ordA.Status != domain.StatusSubmitUnknown {
		t.Fatalf("expected SUBMIT_UNKNOWN + UNKNOWN_OUTCOME, got status=%s err=%v", ordA.Status, errA)
	}

	// Re-submitting same ClientOrderID MUST NOT blindly retry!
	dispatchesBefore := h.mockAdp.DispatchCount()
	_, replayErr := h.gw.SubmitOrder(ctx, reqA)
	if domain.ErrorCodeOf(replayErr) != domain.ErrCodeUnknownOutcome {
		t.Fatalf("expected UNKNOWN_OUTCOME instructing reconciliation, got %v", replayErr)
	}
	if h.mockAdp.DispatchCount() != dispatchesBefore {
		t.Fatalf("FATAL: gateway blindly retried an unknown submit outcome!")
	}

	// Case 6B: Broker NEVER received the order, transport dropped connection
	h.mockAdp.InjectNextSubmitFault(mock.FaultUnknownSubmitDropped)
	reqB := domain.OrderRequest{
		ClientOrderID: "sc-unk-dropped", BrokerID: "mock", Mode: domain.ModePaper,
		InstrumentID: "EQ:US:AAPL", Side: domain.SideBuy, OrderType: domain.OrderTypeMarket,
		TimeInForce: domain.TIFDay, Quantity: 3, Currency: "USD",
	}
	ordB, errB := h.gw.SubmitOrder(ctx, reqB)
	if domain.ErrorCodeOf(errB) != domain.ErrCodeUnknownOutcome || ordB.Status != domain.StatusSubmitUnknown {
		t.Fatalf("expected SUBMIT_UNKNOWN for dropped order, got %s err=%v", ordB.Status, errB)
	}

	// Independent reconciliation resolves BOTH cleanly!
	rep, err := h.recon.ReconcileBroker(ctx, "mock")
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if rep.UnresolvedCount != 0 || len(rep.Findings) != 2 {
		t.Fatalf("expected 2 resolved unknown-submit findings, got %+v", rep)
	}

	resA, _ := h.gw.Store().GetOrder(ordA.OrderID)
	if resA.Status != domain.StatusFilled || resA.FilledQuantity != 5 || !resA.Reconciled {
		t.Fatalf("expected accepted unknown order reconciled to FILLED(5), got %+v", resA)
	}
	resB, _ := h.gw.Store().GetOrder(ordB.OrderID)
	if resB.Status != domain.StatusRejected || !resB.Reconciled {
		t.Fatalf("expected dropped unknown order reconciled to REJECTED, got %+v", resB)
	}
}

// Scenario 7: Cancel timeout -> CANCEL_PENDING -> reconciled to CANCELLED, FILLED, or trips fail-closed when stuck
func TestScenario_CancelTimeout(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	// Case 7A: Cancel timed out, broker actually cancelled it
	o1, _ := h.gw.SubmitOrder(ctx, domain.OrderRequest{
		ClientOrderID: "sc-cxl-1", BrokerID: "sandbox-beta", Mode: domain.ModeSandbox,
		InstrumentID: "EQ:US:AAPL", Side: domain.SideBuy, OrderType: domain.OrderTypeLimit,
		TimeInForce: domain.TIFDay, Quantity: 5, LimitPrice: 180.0, Currency: "USD",
	})
	h.beta.InjectCancelTimeoutOnce("cancelled")
	o1Pending, err := h.gw.CancelOrder(ctx, o1.OrderID)
	if domain.ErrorCodeOf(err) != domain.ErrCodeCancelTimeout || o1Pending.Status != domain.StatusCancelPending {
		t.Fatalf("expected CANCEL_PENDING + CANCEL_TIMEOUT, got %s err=%v", o1Pending.Status, err)
	}
	rep1, _ := h.recon.ReconcileBroker(ctx, "sandbox-beta")
	o1Final, _ := h.gw.Store().GetOrder(o1.OrderID)
	if rep1.UnresolvedCount != 0 || o1Final.Status != domain.StatusCancelled {
		t.Fatalf("expected reconciled CANCELLED, rep=%+v ord=%+v", rep1, o1Final)
	}

	// Case 7B: Cancel timed out because order FILLED right before cancel arrived
	o2, _ := h.gw.SubmitOrder(ctx, domain.OrderRequest{
		ClientOrderID: "sc-cxl-2", BrokerID: "sandbox-beta", Mode: domain.ModeSandbox,
		InstrumentID: "EQ:US:AAPL", Side: domain.SideBuy, OrderType: domain.OrderTypeLimit,
		TimeInForce: domain.TIFDay, Quantity: 4, LimitPrice: 180.0, Currency: "USD",
	})
	h.beta.InjectCancelTimeoutOnce("filled")
	_, err = h.gw.CancelOrder(ctx, o2.OrderID)
	if domain.ErrorCodeOf(err) != domain.ErrCodeCancelTimeout {
		t.Fatalf("expected CANCEL_TIMEOUT, got %v", err)
	}
	rep2, _ := h.recon.ReconcileBroker(ctx, "sandbox-beta")
	o2Final, _ := h.gw.Store().GetOrder(o2.OrderID)
	if rep2.UnresolvedCount != 0 || o2Final.Status != domain.StatusFilled || o2Final.FilledQuantity != 4 {
		t.Fatalf("expected reconciled FILLED(4), rep=%+v ord=%+v", rep2, o2Final)
	}

	// Case 7C: Cancel stuck non-terminal at broker -> reconciler trips FAIL-CLOSED halt!
	h.clk.Advance(2 * time.Second) // refill sandbox-beta rate-limit bucket for sub-case 7C
	o3, err := h.gw.SubmitOrder(ctx, domain.OrderRequest{
		ClientOrderID: "sc-cxl-3", BrokerID: "sandbox-beta", Mode: domain.ModeSandbox,
		InstrumentID: "EQ:US:AAPL", Side: domain.SideBuy, OrderType: domain.OrderTypeLimit,
		TimeInForce: domain.TIFDay, Quantity: 4, LimitPrice: 180.0, Currency: "USD",
	})
	if err != nil {
		t.Fatalf("submit o3 failed: %v", err)
	}
	h.beta.InjectCancelTimeoutOnce("stuck")
	_, err = h.gw.CancelOrder(ctx, o3.OrderID)
	if domain.ErrorCodeOf(err) != domain.ErrCodeCancelTimeout {
		t.Fatalf("expected CANCEL_TIMEOUT for stuck cancel, got %v", err)
	}
	rep3, _ := h.recon.ReconcileBroker(ctx, "sandbox-beta")
	if !rep3.TriggeredFailSafe || !h.gw.HaltStatus().Halted {
		t.Fatalf("expected stuck cancel timeout to trip fail-closed gate, rep=%+v", rep3)
	}
}

// Scenario 8: Sandbox Beta async queue + cursor event replay + phantom order fail-closed drill
func TestScenario_AsyncReplayAndFailClosedDrill(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	h.beta.SetAsyncQueueMode(true)
	ord, err := h.gw.SubmitOrder(ctx, domain.OrderRequest{
		ClientOrderID: "sc-beta-async-1", BrokerID: "sandbox-beta", Mode: domain.ModeSandbox,
		InstrumentID: "CRYPTO:SPOT:BTC-USD", Side: domain.SideBuy, OrderType: domain.OrderTypeMarket,
		TimeInForce: domain.TIFGTC, Quantity: 0.5, Currency: "USD",
	})
	if err != nil || ord.Status != domain.StatusPendingNew {
		t.Fatalf("expected PENDING_NEW, got %+v err=%v", ord, err)
	}
	h.beta.ProcessQueue()

	applied, cursor, err := h.gw.ReplayBrokerEvents(ctx, "sandbox-beta")
	if err != nil || applied == 0 || cursor == 0 {
		t.Fatalf("expected replayed events, applied=%d cursor=%d err=%v", applied, cursor, err)
	}
	ordAfter, _ := h.gw.Store().GetOrder(ord.OrderID)
	if ordAfter.Status != domain.StatusFilled || ordAfter.FilledQuantity != 0.5 {
		t.Fatalf("expected FILLED(0.5) after cursor replay, got %+v", ordAfter)
	}

	// Replaying again is a no-op (cursor persisted)
	applied2, _, _ := h.gw.ReplayBrokerEvents(ctx, "sandbox-beta")
	if applied2 != 0 {
		t.Fatalf("expected 0 events on idempotent second replay, got %d", applied2)
	}

	// Phantom order on mock triggers fail-closed halt, blocks new submits, operator resumes
	h.mockAdp.InjectPhantomOrder("AAPL", 25, 195.0)
	rep, _ := h.recon.ReconcileBroker(ctx, "mock")
	if !rep.TriggeredFailSafe || !h.gw.HaltStatus().Halted {
		t.Fatalf("expected phantom order to trip fail-closed halt")
	}
	_, haltedErr := h.gw.SubmitOrder(ctx, domain.OrderRequest{
		ClientOrderID: "sc-blocked-by-halt", BrokerID: "mock", Mode: domain.ModePaper,
		InstrumentID: "EQ:US:AAPL", Side: domain.SideBuy, OrderType: domain.OrderTypeMarket,
		TimeInForce: domain.TIFDay, Quantity: 1, Currency: "USD",
	})
	if domain.ErrorCodeOf(haltedErr) != domain.ErrCodeGatewayHalted {
		t.Fatalf("expected GATEWAY_HALTED while halted, got %v", haltedErr)
	}
	if err := h.gw.Resume("sre-operator", "remediated phantom position"); err != nil {
		t.Fatalf("operator resume failed: %v", err)
	}
	if h.gw.HaltStatus().Halted {
		t.Fatalf("gateway should be resumed")
	}
	h.assertNoSecretLeaks(t)
}

func BenchmarkGatewaySubmitOrder(b *testing.B) {
	h := newHarness(b)
	ctx := context.Background()
	h.gw.SetRateLimiter("mock", ratelimit.NewTokenBucket("mock", 1e9, 1e9, h.clk))
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, err := h.gw.SubmitOrder(ctx, domain.OrderRequest{
			ClientOrderID: fmt.Sprintf("bench-ord-%d", i),
			BrokerID:      "mock",
			Mode:          domain.ModePaper,
			InstrumentID:  "EQ:US:AAPL",
			Side:          domain.SideBuy,
			OrderType:     domain.OrderTypeMarket,
			TimeInForce:   domain.TIFDay,
			Quantity:      1,
			Currency:      "USD",
		})
		if err != nil {
			b.Fatalf("bench submit failed: %v", err)
		}
	}
}
