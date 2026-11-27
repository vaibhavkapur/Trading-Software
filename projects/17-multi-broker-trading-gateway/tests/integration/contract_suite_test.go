package integration_test

import (
	"bytes"
	"context"
	"fmt"
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

type adapterFixture struct {
	name      string
	mode      domain.ExecutionMode
	secretRef string
	adapter   broker.Adapter
}

// TestSharedBrokerContractSuite executes the unified broker contract suite across ALL adapters
// (mock, sandbox-alpha, sandbox-beta) to prove behavioral conformance.
func TestSharedBrokerContractSuite(t *testing.T) {
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

	clk := ratelimit.NewManualClock(time.Date(2026, 11, 27, 12, 0, 0, 0, time.UTC))
	sm := security.NewMemorySecretManager()
	exp := clk.Now().Add(2 * time.Hour)
	sm.PutCredential(security.Credential{Ref: "mock/api_key", Scheme: "API_KEY", Secret: security.NewSecretString("CONTRACT_SECRET_MOCK_1111"), ExpiresAtUTC: exp})
	sm.PutCredential(security.Credential{Ref: "sandbox-alpha/oauth_token", Scheme: "BEARER", Secret: security.NewSecretString("CONTRACT_SECRET_ALPHA_2222"), ExpiresAtUTC: exp})
	sm.PutCredential(security.Credential{Ref: "sandbox-beta/hmac_key", Scheme: "HMAC", Secret: security.NewSecretString("CONTRACT_SECRET_BETA_3333"), ExpiresAtUTC: exp})

	mockCaps, _ := caps.Get("mock")
	alphaCaps, _ := caps.Get("sandbox-alpha")
	betaCaps, _ := caps.Get("sandbox-beta")

	fixtures := []adapterFixture{
		{name: "mock", mode: domain.ModePaper, secretRef: "mock/api_key", adapter: mock.New(mockCaps, sm, "mock/api_key", 42, clk)},
		{name: "sandbox-alpha", mode: domain.ModeSandbox, secretRef: "sandbox-alpha/oauth_token", adapter: sandbox_alpha.New(alphaCaps, sm, "sandbox-alpha/oauth_token", 100, 50, clk)},
		{name: "sandbox-beta", mode: domain.ModeSandbox, secretRef: "sandbox-beta/hmac_key", adapter: sandbox_beta.New(betaCaps, sm, "sandbox-beta/hmac_key", 100, 50, clk)},
	}

	for _, fx := range fixtures {
		t.Run("AdapterContract/"+fx.name, func(t *testing.T) {
			ctx := context.Background()
			logBuf := &bytes.Buffer{}
			logger := security.NewRedactingLogger(logBuf, sm)

			reg := broker.NewRegistry()
			if err := reg.Register(fx.adapter); err != nil {
				t.Fatalf("register adapter: %v", err)
			}

			gw := gateway.New(gateway.Config{
				DefaultMode:  domain.ModePaper,
				Policy:       policy,
				Capabilities: caps,
				Instruments:  insts,
				Registry:     reg,
				Store:        storage.NewMemoryStore(),
				Logger:       logger,
				Clock:        clk,
			})
			recon := reconciler.New(gw)

			// 1. HealthCheck passes with valid secret
			if err := fx.adapter.HealthCheck(ctx); err != nil {
				t.Fatalf("expected healthy adapter, got %v", err)
			}

			// 2. Marketable order fills and normalizes accurately
			mkt, err := gw.SubmitOrder(ctx, domain.OrderRequest{
				ClientOrderID: fmt.Sprintf("ct-mkt-%s", fx.name),
				BrokerID:      fx.name,
				Mode:          fx.mode,
				InstrumentID:  "EQ:US:AAPL",
				Side:          domain.SideBuy,
				OrderType:     domain.OrderTypeMarket,
				TimeInForce:   domain.TIFDay,
				Quantity:      5,
				Currency:      "USD",
			})
			if err != nil {
				t.Fatalf("market order failed: %v", err)
			}
			if mkt.Status != domain.StatusFilled || mkt.FilledQuantity != 5 || mkt.AvgFillPrice <= 0 {
				t.Fatalf("unexpected filled market order state: %+v", mkt)
			}
			fills := gw.Store().ListFillsByOrder(mkt.OrderID)
			if len(fills) == 0 {
				t.Fatalf("expected normalized fill records for filled order")
			}

			// 3. Idempotent replay returns identical order with ZERO extra adapter dispatches
			beforeReplay := fx.adapter.DispatchCount()
			mktReplay, err := gw.SubmitOrder(ctx, domain.OrderRequest{
				ClientOrderID: fmt.Sprintf("ct-mkt-%s", fx.name),
				BrokerID:      fx.name,
				Mode:          fx.mode,
				InstrumentID:  "EQ:US:AAPL",
				Side:          domain.SideBuy,
				OrderType:     domain.OrderTypeMarket,
				TimeInForce:   domain.TIFDay,
				Quantity:      5,
				Currency:      "USD",
			})
			if err != nil || mktReplay.OrderID != mkt.OrderID {
				t.Fatalf("idempotent replay failed: ord=%+v err=%v", mktReplay, err)
			}
			if fx.adapter.DispatchCount() != beforeReplay {
				t.Fatalf("idempotent replay must not re-dispatch to broker")
			}

			// 4. Conflicting payload on same ClientOrderID returns IDEMPOTENCY_CONFLICT
			_, conflictErr := gw.SubmitOrder(ctx, domain.OrderRequest{
				ClientOrderID: fmt.Sprintf("ct-mkt-%s", fx.name),
				BrokerID:      fx.name,
				Mode:          fx.mode,
				InstrumentID:  "EQ:US:AAPL",
				Side:          domain.SideBuy,
				OrderType:     domain.OrderTypeMarket,
				TimeInForce:   domain.TIFDay,
				Quantity:      99,
				Currency:      "USD",
			})
			if domain.ErrorCodeOf(conflictErr) != domain.ErrCodeIdempotencyConflict {
				t.Fatalf("expected IDEMPOTENCY_CONFLICT, got %v", conflictErr)
			}

			// 5. Passive limit order -> ACCEPTED -> Cancel -> CANCELLED
			lim, err := gw.SubmitOrder(ctx, domain.OrderRequest{
				ClientOrderID: fmt.Sprintf("ct-lim-%s", fx.name),
				BrokerID:      fx.name,
				Mode:          fx.mode,
				InstrumentID:  "EQ:US:AAPL",
				Side:          domain.SideBuy,
				OrderType:     domain.OrderTypeLimit,
				TimeInForce:   domain.TIFDay,
				Quantity:      4,
				LimitPrice:    100.00, // passive below reference price
				Currency:      "USD",
			})
			if err != nil {
				t.Fatalf("passive limit failed: %v", err)
			}
			if lim.Status != domain.StatusAccepted {
				t.Fatalf("expected ACCEPTED for passive limit, got %s", lim.Status)
			}
			cxl, err := gw.CancelOrder(ctx, lim.OrderID)
			if err != nil || cxl.Status != domain.StatusCancelled {
				t.Fatalf("cancel failed: ord=%+v err=%v", cxl, err)
			}

			// 6. Broker snapshot matches gateway state via independent reconciliation (0 drift)
			rep, err := recon.ReconcileBroker(ctx, fx.name)
			if err != nil {
				t.Fatalf("reconcile failed: %v", err)
			}
			if rep.UnresolvedCount != 0 || len(rep.Findings) != 0 {
				t.Fatalf("expected clean reconciliation, got findings=%+v", rep.Findings)
			}

			// 7. Unsupported order type (STOP) rejected pre-dispatch with ZERO adapter calls
			beforeUnsup := fx.adapter.DispatchCount()
			_, unsupErr := gw.SubmitOrder(ctx, domain.OrderRequest{
				ClientOrderID: fmt.Sprintf("ct-unsup-%s", fx.name),
				BrokerID:      fx.name,
				Mode:          fx.mode,
				InstrumentID:  "EQ:US:AAPL",
				Side:          domain.SideSell,
				OrderType:     domain.OrderTypeStop,
				TimeInForce:   domain.TIFDay,
				Quantity:      2,
				StopPrice:     180.0,
				Currency:      "USD",
			})
			if domain.ErrorCodeOf(unsupErr) != domain.ErrCodeUnsupportedCapability {
				t.Fatalf("expected UNSUPPORTED_CAPABILITY, got %v", unsupErr)
			}
			if fx.adapter.DispatchCount() != beforeUnsup {
				t.Fatalf("unsupported order type must be rejected BEFORE dispatch")
			}

			// 8. Live execution mode rejected pre-dispatch with ZERO adapter calls
			beforeLive := fx.adapter.DispatchCount()
			_, liveErr := gw.SubmitOrder(ctx, domain.OrderRequest{
				ClientOrderID: fmt.Sprintf("ct-live-%s", fx.name),
				BrokerID:      fx.name,
				Mode:          domain.ModeLive,
				InstrumentID:  "EQ:US:AAPL",
				Side:          domain.SideBuy,
				OrderType:     domain.OrderTypeMarket,
				TimeInForce:   domain.TIFDay,
				Quantity:      1,
				Currency:      "USD",
			})
			if domain.ErrorCodeOf(liveErr) != domain.ErrCodePolicyDenied {
				t.Fatalf("expected POLICY_DENIED for live mode, got %v", liveErr)
			}
			if fx.adapter.DispatchCount() != beforeLive {
				t.Fatalf("live mode must be rejected BEFORE dispatch")
			}

			// 9. Verify ZERO secrets leaked in logs
			logs := logBuf.String()
			for _, secret := range sm.RegisteredSecretValues() {
				if strings.Contains(logs, secret) {
					t.Fatalf("secret %q leaked into logs for %s", secret, fx.name)
				}
			}
		})
	}
}
