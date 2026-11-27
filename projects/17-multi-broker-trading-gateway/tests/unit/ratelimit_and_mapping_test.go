package unit_test

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/vaibhavkapur/Trading-Software/projects/17-multi-broker-trading-gateway/src/domain"
	"github.com/vaibhavkapur/Trading-Software/projects/17-multi-broker-trading-gateway/src/ratelimit"
)

func TestTokenBucketRateLimitAndRetryAfter(t *testing.T) {
	clk := ratelimit.NewManualClock(time.Date(2026, 11, 27, 10, 0, 0, 0, time.UTC))
	tb := ratelimit.NewTokenBucket("sandbox-alpha", 2, 2, clk)

	if err := tb.Allow(); err != nil {
		t.Fatalf("1st token should pass: %v", err)
	}
	if err := tb.Allow(); err != nil {
		t.Fatalf("2nd token should pass: %v", err)
	}
	if err := tb.Allow(); domain.ErrorCodeOf(err) != domain.ErrCodeRateLimited {
		t.Fatalf("3rd immediate request should be RATE_LIMITED, got %v", err)
	}

	// Advance 600ms -> refills 1.2 tokens -> should allow 1 request
	clk.Advance(600 * time.Millisecond)
	if err := tb.Allow(); err != nil {
		t.Fatalf("expected refill token after 600ms: %v", err)
	}

	// Apply upstream Retry-After cooldown
	tb.ApplyRetryAfter(2 * time.Second)
	clk.Advance(1 * time.Second)
	if err := tb.Allow(); domain.ErrorCodeOf(err) != domain.ErrCodeRateLimited {
		t.Fatalf("expected cooldown active at +1s, got %v", err)
	}
	clk.Advance(1500 * time.Millisecond)
	if err := tb.Allow(); err != nil {
		t.Fatalf("expected recovery after cooldown, got %v", err)
	}
}

func TestInstrumentMappingAndCapabilityMatrix(t *testing.T) {
	insts, err := domain.LoadInstrumentMapper(filepath.Join("..", "..", "config", "instruments.json"))
	if err != nil {
		t.Fatalf("load instruments: %v", err)
	}
	caps, err := domain.LoadCapabilityMatrix(filepath.Join("..", "..", "config", "capabilities.json"))
	if err != nil {
		t.Fatalf("load capabilities: %v", err)
	}

	// Bidirectional mapping check across all 3 brokers
	reqAlpha := domain.OrderRequest{
		ClientOrderID: "c1", BrokerID: "sandbox-alpha", Mode: domain.ModeSandbox,
		InstrumentID: "EQ:US:AAPL", Side: domain.SideBuy, OrderType: domain.OrderTypeLimit,
		TimeInForce: domain.TIFDay, Quantity: 5, LimitPrice: 195.0, Currency: "USD",
	}
	spec, sym, err := insts.ResolveToBroker("sandbox-alpha", reqAlpha)
	if err != nil || sym != "US:AAPL" {
		t.Fatalf("expected US:AAPL, got %q err=%v", sym, err)
	}
	if cid, ok := insts.ResolveFromBroker("sandbox-alpha", "US:AAPL"); !ok || cid != "EQ:US:AAPL" {
		t.Fatalf("reverse mapping failed: ok=%v cid=%q", ok, cid)
	}
	if err := caps.ValidateOrder(reqAlpha, spec); err != nil {
		t.Fatalf("expected valid alpha order, got %v", err)
	}

	// Symbol mismatch: crypto not mapped on sandbox-alpha
	reqBadSym := reqAlpha
	reqBadSym.InstrumentID = "CRYPTO:SPOT:BTC-USD"
	if _, _, err := insts.ResolveToBroker("sandbox-alpha", reqBadSym); domain.ErrorCodeOf(err) != domain.ErrCodeSymbolMismatch {
		t.Fatalf("expected SYMBOL_MISMATCH, got %v", err)
	}

	// Currency mismatch
	reqBadCcy := reqAlpha
	reqBadCcy.Currency = "EUR"
	if _, _, err := insts.ResolveToBroker("sandbox-alpha", reqBadCcy); domain.ErrorCodeOf(err) != domain.ErrCodeSymbolMismatch {
		t.Fatalf("expected SYMBOL_MISMATCH on wrong currency, got %v", err)
	}

	// Unsupported order type STOP_LIMIT on sandbox-alpha
	reqStop := reqAlpha
	reqStop.OrderType = domain.OrderTypeStopLimit
	reqStop.StopPrice = 194.0
	if err := caps.ValidateOrder(reqStop, spec); domain.ErrorCodeOf(err) != domain.ErrCodeUnsupportedCapability {
		t.Fatalf("expected UNSUPPORTED_CAPABILITY for STOP_LIMIT on alpha, got %v", err)
	}

	// Unsupported fractional qty on sandbox-alpha
	reqFrac := reqAlpha
	reqFrac.Quantity = 1.5
	if err := caps.ValidateOrder(reqFrac, spec); domain.ErrorCodeOf(err) != domain.ErrCodeUnsupportedCapability {
		t.Fatalf("expected UNSUPPORTED_CAPABILITY for fractional qty on alpha, got %v", err)
	}

	// Same fractional crypto STOP_LIMIT IS supported on sandbox-beta
	reqBeta := domain.OrderRequest{
		ClientOrderID: "c2", BrokerID: "sandbox-beta", Mode: domain.ModeSandbox,
		InstrumentID: "CRYPTO:SPOT:BTC-USD", Side: domain.SideBuy, OrderType: domain.OrderTypeStopLimit,
		TimeInForce: domain.TIFGTC, Quantity: 0.25, LimitPrice: 92000, StopPrice: 91500, Currency: "USD",
	}
	betaSpec, betaSym, err := insts.ResolveToBroker("sandbox-beta", reqBeta)
	if err != nil || betaSym != "BTC-USD" {
		t.Fatalf("expected BTC-USD on beta, got %q err=%v", betaSym, err)
	}
	if err := caps.ValidateOrder(reqBeta, betaSpec); err != nil {
		t.Fatalf("expected valid beta crypto stop-limit order, got %v", err)
	}
}
