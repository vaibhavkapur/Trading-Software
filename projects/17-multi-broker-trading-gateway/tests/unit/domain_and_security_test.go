package unit_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/vaibhavkapur/Trading-Software/projects/17-multi-broker-trading-gateway/src/domain"
	"github.com/vaibhavkapur/Trading-Software/projects/17-multi-broker-trading-gateway/src/security"
)

func TestOrderRequestValidationInvariants(t *testing.T) {
	base := domain.OrderRequest{
		ClientOrderID: "cli-1",
		BrokerID:      "mock",
		Mode:          domain.ModePaper,
		InstrumentID:  "EQ:US:AAPL",
		Side:          domain.SideBuy,
		OrderType:     domain.OrderTypeLimit,
		TimeInForce:   domain.TIFDay,
		Quantity:      10,
		LimitPrice:    195.0,
		Currency:      "USD",
	}
	if err := base.ValidateBasic(); err != nil {
		t.Fatalf("expected valid order, got %v", err)
	}

	cases := []struct {
		name   string
		mutate func(*domain.OrderRequest)
	}{
		{"empty_client_id", func(r *domain.OrderRequest) { r.ClientOrderID = "" }},
		{"empty_broker_id", func(r *domain.OrderRequest) { r.BrokerID = "" }},
		{"empty_instrument", func(r *domain.OrderRequest) { r.InstrumentID = "" }},
		{"bad_side", func(r *domain.OrderRequest) { r.Side = "HOLD" }},
		{"zero_qty", func(r *domain.OrderRequest) { r.Quantity = 0 }},
		{"neg_qty", func(r *domain.OrderRequest) { r.Quantity = -5 }},
		{"missing_limit_price", func(r *domain.OrderRequest) { r.LimitPrice = 0 }},
		{"missing_stop_price", func(r *domain.OrderRequest) { r.OrderType = domain.OrderTypeStopLimit; r.StopPrice = 0 }},
		{"bad_tif", func(r *domain.OrderRequest) { r.TimeInForce = "FOREVER" }},
		{"empty_currency", func(r *domain.OrderRequest) { r.Currency = "" }},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := base
			tc.mutate(&r)
			err := r.ValidateBasic()
			if err == nil {
				t.Fatalf("expected validation error for %s", tc.name)
			}
			if domain.ErrorCodeOf(err) != domain.ErrCodeValidation {
				t.Fatalf("expected VALIDATION_ERROR, got %s", domain.ErrorCodeOf(err))
			}
		})
	}
}

func TestRecomputeOrderFromFillsVWAPAndMonotonicity(t *testing.T) {
	now := time.Date(2026, 11, 27, 10, 0, 0, 0, time.UTC)
	ord := domain.Order{
		OrderID:  "gw-1",
		Quantity: 10,
		Status:   domain.StatusAccepted,
	}
	fills := []domain.Fill{
		{Quantity: 4, Price: 100.0, Fee: 0.04},
		{Quantity: 6, Price: 102.0, Fee: 0.06},
	}
	domain.RecomputeOrderFromFills(&ord, fills, now)
	if ord.Status != domain.StatusFilled {
		t.Fatalf("expected FILLED, got %s", ord.Status)
	}
	if ord.FilledQuantity != 10 || ord.RemainingQty != 0 {
		t.Fatalf("unexpected quantities: filled=%v rem=%v", ord.FilledQuantity, ord.RemainingQty)
	}
	expectedVWAP := (4*100.0 + 6*102.0) / 10.0
	if math.Abs(ord.AvgFillPrice-expectedVWAP) > 1e-6 {
		t.Fatalf("expected VWAP %v, got %v", expectedVWAP, ord.AvgFillPrice)
	}
	if math.Abs(ord.TotalFees-0.10) > 1e-6 {
		t.Fatalf("expected total fees 0.10, got %v", ord.TotalFees)
	}
}

func TestSecretStringAndLoggerNeverLeakSecrets(t *testing.T) {
	rawSecret := "ULTRA_SECRET_TOKEN_XYZ_987654321"
	sec := security.NewSecretString(rawSecret)

	if fmt.Sprintf("%s", sec) != "[REDACTED]" {
		t.Fatalf("String() leaked secret")
	}
	if fmt.Sprintf("%#v", sec) != "[REDACTED]" {
		t.Fatalf("GoString() leaked secret")
	}
	jb, err := json.Marshal(sec)
	if err != nil || strings.Contains(string(jb), rawSecret) {
		t.Fatalf("MarshalJSON() leaked secret: %s", string(jb))
	}

	sm := security.NewMemorySecretManager()
	sm.PutCredential(security.Credential{
		Ref:          "broker/test",
		Scheme:       "BEARER",
		Secret:       sec,
		ExpiresAtUTC: time.Now().UTC().Add(time.Hour),
	})

	buf := &bytes.Buffer{}
	logger := security.NewRedactingLogger(buf, sm)
	logger.Error("auth", "upstream failed with Bearer "+rawSecret+" and api_key=INLINE_KEY_12345", map[string]any{
		"authorization": "Bearer ANOTHER_SECRET_777",
		"token":         "TOP_SECRET_VALUE",
		"raw_echo":      "leaked: " + rawSecret,
		"secret_obj":    sec,
	})

	out := buf.String()
	for _, forbidden := range []string{rawSecret, "INLINE_KEY_12345", "ANOTHER_SECRET_777", "TOP_SECRET_VALUE"} {
		if strings.Contains(out, forbidden) {
			t.Fatalf("secret %q appeared in redacted log output: %s", forbidden, out)
		}
	}
	if !strings.Contains(out, "[REDACTED]") {
		t.Fatalf("expected [REDACTED] markers in log output: %s", out)
	}
}

func TestExecutionPolicyPaperSandboxDefaultAndLiveDenied(t *testing.T) {
	policy := security.DefaultSafePolicy()

	paperReq := domain.OrderRequest{BrokerID: "mock", Mode: domain.ModePaper, Quantity: 10, LimitPrice: 100}
	if err := policy.AuthorizeOrder(paperReq); err != nil {
		t.Fatalf("paper mode should be permitted: %v", err)
	}

	sandboxReq := domain.OrderRequest{BrokerID: "sandbox-alpha", Mode: domain.ModeSandbox, Quantity: 10, LimitPrice: 100}
	if err := policy.AuthorizeOrder(sandboxReq); err != nil {
		t.Fatalf("sandbox mode should be permitted: %v", err)
	}

	liveReq := domain.OrderRequest{BrokerID: "sandbox-alpha", Mode: domain.ModeLive, Quantity: 1, LimitPrice: 100}
	err := policy.AuthorizeOrder(liveReq)
	if domain.ErrorCodeOf(err) != domain.ErrCodePolicyDenied {
		t.Fatalf("live mode must return POLICY_DENIED, got %v", err)
	}

	hugeReq := domain.OrderRequest{BrokerID: "sandbox-alpha", Mode: domain.ModeSandbox, Quantity: 5000, LimitPrice: 100}
	err = policy.AuthorizeOrder(hugeReq)
	if domain.ErrorCodeOf(err) != domain.ErrCodePolicyDenied {
		t.Fatalf("oversized notional must return POLICY_DENIED, got %v", err)
	}
}
