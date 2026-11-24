package unit_test

import (
	"errors"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/trading-software-projects/agentic-trading-guardrails/src/pkg/approval"
	"github.com/trading-software-projects/agentic-trading-guardrails/src/pkg/audit"
	"github.com/trading-software-projects/agentic-trading-guardrails/src/pkg/broker"
	"github.com/trading-software-projects/agentic-trading-guardrails/src/pkg/identity"
	"github.com/trading-software-projects/agentic-trading-guardrails/src/pkg/policy"
	"github.com/trading-software-projects/agentic-trading-guardrails/src/pkg/types"
)

func TestFixedPointMoneyAndTicks(t *testing.T) {
	c, err := types.ParseMoneyCents("500.25")
	if err != nil || c != 50025 {
		t.Fatalf("expected 50025 cents, got %d (err=%v)", c, err)
	}
	if c.FormatUSD() != "$500.25" {
		t.Fatalf("unexpected format: %s", c.FormatUSD())
	}
	ticks, err := types.ParsePriceTicks("510.125")
	if err != nil || ticks != 5101250 {
		t.Fatalf("expected 5101250 ticks, got %d (err=%v)", ticks, err)
	}

	// Ceiling rounding for conservative risk checks: 1 share * $1.0001 = 10001 ticks -> 101 cents ($1.01)
	notional, err := types.ComputeNotionalCents(1, 10001)
	if err != nil || notional != 101 {
		t.Fatalf("expected conservative ceil 101 cents, got %d (err=%v)", notional, err)
	}

	// Overflow protection
	if _, err := types.ComputeNotionalCents(math.MaxInt64, 100000); !errors.Is(err, types.ErrArithmeticOver) {
		t.Fatalf("expected overflow error, got %v", err)
	}
	if _, err := types.SafeAddCents(types.MoneyCents(math.MaxInt64), 1); !errors.Is(err, types.ErrArithmeticOver) {
		t.Fatalf("expected SafeAddCents overflow error, got %v", err)
	}
	if _, err := types.ParseMoneyCents("12.345"); err == nil {
		t.Fatalf("expected error for >2 decimal places in cents")
	}
}

func TestIdentityDelegationJWTAndRevocation(t *testing.T) {
	clk := types.NewFixedClock(time.Date(2026, 6, 15, 15, 0, 0, 0, time.UTC))
	svc, err := identity.NewService(clk)
	if err != nil {
		t.Fatalf("new identity svc: %v", err)
	}

	err = svc.RegisterIdentity(types.AgentIdentity{
		ID:              "agent-1",
		PrincipalID:     "principal-alice",
		OrgID:           "org-1",
		TeamID:          "equities-research",
		Scopes:          []types.Scope{types.ScopeMarketRead, types.ScopeOrderPropose},
		AllowedAccounts: []string{"a-1"},
		ExpiresAtUTC:    clk.Now().Add(1 * time.Hour),
	})
	if err != nil {
		t.Fatalf("register identity: %v", err)
	}

	// Scope escalation attempt must fail
	if _, _, err := svc.IssueToken("agent-1", []types.Scope{types.ScopeOrderSubmit}, 5*time.Minute); !errors.Is(err, identity.ErrScopeEscalation) {
		t.Fatalf("expected scope escalation error, got %v", err)
	}

	// Valid narrowed token
	tok, claims, err := svc.IssueToken("agent-1", []types.Scope{types.ScopeMarketRead}, 5*time.Minute)
	if err != nil {
		t.Fatalf("issue token: %v", err)
	}

	// Rotate keys and verify old token still verifies via retained JWKS key
	if _, err := svc.RotateKey(); err != nil {
		t.Fatalf("rotate key: %v", err)
	}
	if len(svc.JWKS().Keys) != 2 {
		t.Fatalf("expected 2 JWKS keys after rotation, got %d", len(svc.JWKS().Keys))
	}

	ident, _, err := svc.VerifyToken("Bearer " + tok)
	if err != nil {
		t.Fatalf("verify token after rotation: %v", err)
	}
	if ident.ID != "agent-1" || ident.PrincipalID != "principal-alice" || len(ident.Scopes) != 1 || ident.Scopes[0] != types.ScopeMarketRead {
		t.Fatalf("unexpected resolved identity: %+v", ident)
	}

	// Tampered signature must fail
	badTok := tok[:len(tok)-2] + "AA"
	if _, _, err := svc.VerifyToken(badTok); !errors.Is(err, identity.ErrInvalidSignature) {
		t.Fatalf("expected invalid signature, got %v", err)
	}

	// Token revocation must take effect immediately
	svc.RevokeToken(claims.JTI)
	if _, _, err := svc.VerifyToken(tok); !errors.Is(err, identity.ErrTokenRevoked) {
		t.Fatalf("expected token revoked, got %v", err)
	}

	// Expiry check
	tok2, _, err := svc.IssueToken("agent-1", nil, 1*time.Minute)
	if err != nil {
		t.Fatalf("issue token 2: %v", err)
	}
	clk.Advance(2 * time.Minute)
	if _, _, err := svc.VerifyToken(tok2); !errors.Is(err, identity.ErrTokenExpired) {
		t.Fatalf("expected token expired, got %v", err)
	}
}

func TestPolicyEngineFailClosedAndHierarchy(t *testing.T) {
	clk := types.NewFixedClock(time.Date(2026, 6, 15, 15, 0, 0, 0, time.UTC))
	eng := policy.NewEngine(clk)

	ident := types.AgentIdentity{
		ID:              "agent-1",
		PrincipalID:     "principal-alice",
		OrgID:           "org-1",
		TeamID:          "equities-research",
		Scopes:          []types.Scope{types.ScopeOrderPropose, types.ScopeOrderSubmit},
		AllowedAccounts: []string{"a-1"},
		ExpiresAtUTC:    clk.Now().Add(2 * time.Hour),
	}

	// Fail-closed when no active policy exists
	decNoPol := eng.Evaluate(policy.EvalInput{
		Identity: ident,
		Action:   types.ScopeOrderPropose,
		Resource: types.ResourceDescriptor{AccountID: "a-1", Symbol: "SPY", OrderType: types.OrderTypeLimit, NotionalCents: 50_000},
	})
	if decNoPol.Result != types.DecisionDeny {
		t.Fatalf("expected fail-closed DENY with no active policy")
	}

	// Publish default bundle
	b := policy.DefaultPolicyBundle()
	b.Org.TradingWindow = &policy.TimeWindowUTC{
		StartMinuteUTC:  13*60 + 30, // 13:30 UTC
		EndMinuteUTC:    20 * 60,    // 20:00 UTC
		AllowedWeekdays: []time.Weekday{time.Monday, time.Tuesday, time.Wednesday, time.Thursday, time.Friday},
	}
	published, err := eng.Publish(b)
	if err != nil {
		t.Fatalf("publish policy: %v", err)
	}
	if published.Hash == "" {
		t.Fatalf("expected non-empty policy hash")
	}

	// Valid LIMIT order within team cap ($1000) and market hours (15:00 UTC Mon)
	okDec := eng.Evaluate(policy.EvalInput{
		Identity: ident,
		Action:   types.ScopeOrderPropose,
		Resource: types.ResourceDescriptor{AccountID: "a-1", Symbol: "SPY", Side: types.SideBuy, OrderType: types.OrderTypeLimit, Quantity: 1, NotionalCents: 51_000},
	})
	if okDec.Result != types.DecisionAllow {
		t.Fatalf("expected ALLOW, got DENY: %v", okDec.Reasons)
	}

	// Hierarchy narrowing: NVDA is allowed at Org level, NOT at team equities-research level -> DENY
	teamDeny := eng.Evaluate(policy.EvalInput{
		Identity: ident,
		Action:   types.ScopeOrderPropose,
		Resource: types.ResourceDescriptor{AccountID: "a-1", Symbol: "NVDA", Side: types.SideBuy, OrderType: types.OrderTypeLimit, Quantity: 1, NotionalCents: 13_000},
	})
	if teamDeny.Result != types.DecisionDeny {
		t.Fatalf("expected team hierarchy DENY for NVDA")
	}

	// Outside trading window (22:00 UTC) -> DENY
	clk.Set(time.Date(2026, 6, 15, 22, 0, 0, 0, time.UTC))
	winDeny := eng.Evaluate(policy.EvalInput{
		Identity: ident,
		Action:   types.ScopeOrderPropose,
		Resource: types.ResourceDescriptor{AccountID: "a-1", Symbol: "SPY", Side: types.SideBuy, OrderType: types.OrderTypeLimit, Quantity: 1, NotionalCents: 51_000},
	})
	if winDeny.Result != types.DecisionDeny {
		t.Fatalf("expected trading window DENY outside market hours")
	}

	// Shadow simulation: test stricter candidate bundle ($400 cap) without changing active policy
	clk.Set(time.Date(2026, 6, 15, 15, 0, 0, 0, time.UTC))
	cand := b
	cand.Version = "v2.0.0-shadow"
	cand.Org.MaxOrderNotionalCents = 40_000
	sims, err := eng.Simulate(cand, []policy.EvalInput{{
		Identity: ident,
		Action:   types.ScopeOrderPropose,
		Resource: types.ResourceDescriptor{AccountID: "a-1", Symbol: "SPY", Side: types.SideBuy, OrderType: types.OrderTypeLimit, Quantity: 1, NotionalCents: 51_000},
	}})
	if err != nil {
		t.Fatalf("simulate: %v", err)
	}
	if len(sims) != 1 || !sims[0].Changed || sims[0].ActiveResult != types.DecisionAllow || sims[0].CandidateResult != types.DecisionDeny {
		t.Fatalf("unexpected simulation diff: %+v", sims)
	}
}

func TestApprovalOneUseAndHashBinding(t *testing.T) {
	clk := types.NewFixedClock(time.Date(2026, 6, 15, 15, 0, 0, 0, time.UTC))
	svc, err := approval.NewService(clk)
	if err != nil {
		t.Fatalf("new approval svc: %v", err)
	}

	prop := types.OrderProposal{
		ID:              "p-1",
		AgentID:         "agent-1",
		PrincipalID:     "principal-alice",
		AccountID:       "a-1",
		Symbol:          "SPY",
		Side:            types.SideBuy,
		OrderType:       types.OrderTypeLimit,
		Quantity:        1,
		LimitPriceTicks: 510_0000,
		NotionalCents:   51_000,
		TimeInForce:     types.TIFDay,
	}
	prop.TermsHash = approval.ComputeProposalHash(prop)

	grant, err := svc.IssueGrant(prop, prop.TermsHash, "principal-alice", 60*time.Second)
	if err != nil {
		t.Fatalf("issue grant: %v", err)
	}

	// Tampered proposal quantity invalidates grant BEFORE consuming
	tampered := prop
	tampered.Quantity = 2
	if _, viol, err := svc.VerifyAndConsume(grant.Token, tampered, "a-1"); !errors.Is(err, approval.ErrGrantHashMismatch) || viol != approval.ViolationModifiedProposal {
		t.Fatalf("expected modified proposal hash mismatch, got viol=%s err=%v", viol, err)
	}

	// Wrong account rejected BEFORE consuming
	if _, viol, err := svc.VerifyAndConsume(grant.Token, prop, "a-2"); !errors.Is(err, approval.ErrGrantWrongAccount) || viol != approval.ViolationWrongAccount {
		t.Fatalf("expected wrong account error, got viol=%s err=%v", viol, err)
	}

	// First valid consumption succeeds
	consumed, viol, err := svc.VerifyAndConsume(grant.Token, prop, "a-1")
	if err != nil || viol != approval.ViolationNone || consumed.ConsumedAtUTC == nil {
		t.Fatalf("expected valid consume, got viol=%s err=%v", viol, err)
	}

	// Second consumption (replay) must be denied
	if _, viol, err := svc.VerifyAndConsume(grant.Token, prop, "a-1"); !errors.Is(err, approval.ErrGrantReplayed) || viol != approval.ViolationReplayed {
		t.Fatalf("expected replay denied, got viol=%s err=%v", viol, err)
	}
}

func TestBrokerBlocksDirectUnverifiedWrites(t *testing.T) {
	brk, err := broker.NewPaperAdapter(nil)
	if err != nil {
		t.Fatalf("new broker: %v", err)
	}
	// Attempting to submit with zero-value ExecutionPermit MUST fail closed
	_, err = brk.SubmitOrder(broker.ExecutionPermit{}, types.OrderProposal{
		ID:              "p-bypass",
		AccountID:       "a-1",
		Symbol:          "SPY",
		Side:            types.SideBuy,
		OrderType:       types.OrderTypeLimit,
		Quantity:        1,
		LimitPriceTicks: 510_0000,
		NotionalCents:   51_000,
	})
	if !errors.Is(err, broker.ErrUnverifiedWrite) {
		t.Fatalf("expected ErrUnverifiedWrite when bypassing PEP, got %v", err)
	}
}

func TestAuditHashChainAndRedaction(t *testing.T) {
	clk := types.NewFixedClock(time.Date(2026, 6, 15, 15, 0, 0, 0, time.UTC))
	aud, err := audit.NewLogger(clk, "")
	if err != nil {
		t.Fatalf("new audit logger: %v", err)
	}
	ev, err := aud.Append(audit.Event{
		CorrelationID: "corr-1",
		ProposalID:    "p-1",
		ActorID:       "agent-1",
		Category:      audit.CategoryApproval,
		Action:        "approval:grant",
		Outcome:       "APPROVED",
		Details: map[string]any{
			"approval_grant": "secret.signed.token",
			"api_key":        "super-secret-key",
			"auth_header":    "Bearer eyJhbGciOiJFZERTQSJ9.abc.def",
			"symbol":         "SPY",
		},
	})
	if err != nil {
		t.Fatalf("append audit: %v", err)
	}
	if ev.Details["approval_grant"] != "[REDACTED]" || ev.Details["api_key"] != "[REDACTED]" {
		t.Fatalf("expected secrets redacted, got %+v", ev.Details)
	}
	if !strings.Contains(ev.Details["auth_header"].(string), "[REDACTED]") {
		t.Fatalf("expected bearer header redacted, got %v", ev.Details["auth_header"])
	}
	if ev.Details["symbol"] != "SPY" {
		t.Fatalf("expected non-secret field preserved")
	}
	if err := aud.VerifyChain(); err != nil {
		t.Fatalf("expected valid hash chain, got %v", err)
	}
}
