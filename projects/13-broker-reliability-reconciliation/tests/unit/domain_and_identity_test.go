package unit_test

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/trading-software-projects/broker-reliability-reconciliation/src/domain"
	"github.com/trading-software-projects/broker-reliability-reconciliation/src/identity"
	"github.com/trading-software-projects/broker-reliability-reconciliation/src/issues"
	"github.com/trading-software-projects/broker-reliability-reconciliation/src/reconcile"
)

func TestDecimalFixedPointExactnessAndLargeNotional(t *testing.T) {
	// Classic float64 failure: 0.1 + 0.2 must equal exactly 0.3
	a := domain.MustParseDecimal("0.1")
	b := domain.MustParseDecimal("0.2")
	sum := a.Add(b)
	if !sum.Equal(domain.MustParseDecimal("0.3")) {
		t.Fatalf("expected 0.1 + 0.2 == 0.3, got %s", sum.String())
	}

	// Large notional without int64 overflow: 1,000,000,000 * 500.12345678
	qty := domain.MustParseDecimal("1000000000")
	px := domain.MustParseDecimal("500.12345678")
	notional := qty.Mul(px)
	want := domain.MustParseDecimal("500123456780")
	if !notional.Equal(want) {
		t.Fatalf("expected %s, got %s", want.String(), notional.String())
	}

	// Negative arithmetic and Abs
	neg := domain.MustParseDecimal("-42.50")
	if neg.Abs().String() != "42.5" {
		t.Fatalf("expected abs 42.5, got %s", neg.Abs().String())
	}

	// JSON marshaling preserves exact string representation
	raw, err := json.Marshal(domain.MustParseDecimal("1234.56780000"))
	if err != nil {
		t.Fatalf("marshal failed: %v", err)
	}
	if string(raw) != `"1234.5678"` {
		t.Fatalf("expected quoted decimal, got %s", string(raw))
	}
	var decoded domain.Decimal
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatalf("unmarshal failed: %v", err)
	}
	if !decoded.Equal(domain.MustParseDecimal("1234.5678")) {
		t.Fatalf("roundtrip mismatch: got %s", decoded.String())
	}
}

func TestValidationBoundaries(t *testing.T) {
	now := time.Date(2026, 1, 31, 15, 0, 0, 0, time.UTC)

	badFill := domain.LedgerFill{
		ID:        "lf-bad",
		AccountID: "a-1",
		Qty:       domain.Zero(),
		Price:     domain.MustParseDecimal("10.00"),
		EventTime: now,
	}
	if err := badFill.Validate(); err == nil {
		t.Fatal("expected error for zero qty ledger fill")
	}

	badPolicy := domain.DefaultPolicy()
	badPolicy.CashTolerance = domain.MustParseDecimal("-0.01")
	if err := badPolicy.Validate(); err == nil {
		t.Fatal("expected error for negative tolerance policy")
	}

	if _, err := domain.ParseDecimal("12.34.56"); err == nil {
		t.Fatal("expected error for malformed decimal")
	}
}

func TestIdentityMapperNormalizationAndLinks(t *testing.T) {
	m := identity.NewMapper(map[string]string{"BRKB": "BRK.B"})

	if got := m.NormalizeSymbol("  aapl.us "); got != "AAPL" {
		t.Fatalf("expected AAPL, got %s", got)
	}
	if got := m.NormalizeSymbol("brk-b"); got != "BRK.B" {
		t.Fatalf("expected BRK.B, got %s", got)
	}
	if got := m.NormalizeSymbol("BRKB"); got != "BRK.B" {
		t.Fatalf("expected BRK.B via alias, got %s", got)
	}
	if got := m.NormalizeCurrency(" eur "); got != "EUR" {
		t.Fatalf("expected EUR, got %s", got)
	}

	est := time.FixedZone("EST", -5*3600)
	localEv := time.Date(2026, 1, 31, 10, 0, 0, 0, est)
	evUTC, rcUTC := identity.NormalizeUTC(localEv, time.Time{})
	if evUTC.Location() != time.UTC || evUTC.Hour() != 15 {
		t.Fatalf("expected 15:00 UTC, got %s", evUTC)
	}
	if !rcUTC.Equal(evUTC) {
		t.Fatalf("expected zero receive time to default to event time")
	}

	m.RegisterOrderLink("cl-1", "bo-1")
	if b, ok := m.BrokerOrderForClient("cl-1"); !ok || b != "bo-1" {
		t.Fatalf("expected bo-1, got %q (%t)", b, ok)
	}
	if c, ok := m.ClientOrderForBroker("bo-1"); !ok || c != "cl-1" {
		t.Fatalf("expected cl-1, got %q (%t)", c, ok)
	}
}

func TestIssueStateTransitions(t *testing.T) {
	valid := [][2]domain.IssueState{
		{domain.IssueStateOpen, domain.IssueStateInvestigating},
		{domain.IssueStateOpen, domain.IssueStateEscalated},
		{domain.IssueStateOpen, domain.IssueStateResolved},
		{domain.IssueStateInvestigating, domain.IssueStateResolved},
		{domain.IssueStateEscalated, domain.IssueStateResolved},
		{domain.IssueStateResolved, domain.IssueStateOpen},
		{domain.IssueStateWaived, domain.IssueStateOpen},
	}
	for _, pair := range valid {
		if !issues.IsValidTransition(pair[0], pair[1]) {
			t.Errorf("expected transition %s -> %s to be valid", pair[0], pair[1])
		}
	}

	invalid := [][2]domain.IssueState{
		{domain.IssueStateOpen, domain.IssueStateOpen},
		{domain.IssueStateResolved, domain.IssueStateEscalated},
		{domain.IssueStateResolved, domain.IssueStateWaived},
		{domain.IssueStateWaived, domain.IssueStateResolved},
	}
	for _, pair := range invalid {
		if issues.IsValidTransition(pair[0], pair[1]) {
			t.Errorf("expected transition %s -> %s to be invalid", pair[0], pair[1])
		}
	}
}

func TestPendingCashNeverMasksSettledCashBreak(t *testing.T) {
	cutoff := time.Date(2026, 1, 31, 16, 0, 0, 0, time.UTC)
	engine := reconcile.NewEngine(identity.NewMapper(nil))

	// Total cash (settled + pending) equals 1000.00 on BOTH sides,
	// yet settled cash differs by $200. Non-goal guard must flag settled_cash_break!
	res, err := engine.Reconcile("run-cash-guard", reconcile.SnapshotInput{
		AccountID: "a-cash",
		Cutoff:    cutoff,
		Policy:    domain.DefaultPolicy(),
		InternalCash: []domain.CashSnapshot{{
			Source:      domain.SourceInternal,
			AccountID:   "a-cash",
			Currency:    "USD",
			SettledCash: domain.MustParseDecimal("800.00"),
			PendingCash: domain.MustParseDecimal("200.00"),
			AsOf:        cutoff,
			ReceiveTime: cutoff,
		}},
		BrokerCash: []domain.CashSnapshot{{
			Source:      domain.SourceBroker,
			AccountID:   "a-cash",
			Currency:    "USD",
			SettledCash: domain.MustParseDecimal("1000.00"),
			PendingCash: domain.MustParseDecimal("0.00"),
			AsOf:        cutoff,
			ReceiveTime: cutoff,
		}},
	})
	if err != nil {
		t.Fatalf("reconcile failed: %v", err)
	}

	var sawSettled, sawPending bool
	for _, iss := range res.Issues {
		if iss.Kind == domain.IssueKindSettledCashBreak {
			sawSettled = true
			if !iss.MonetaryExposure.Equal(domain.MustParseDecimal("200")) {
				t.Fatalf("expected 200 settled exposure, got %s", iss.MonetaryExposure.String())
			}
		}
		if iss.Kind == domain.IssueKindPendingCashBreak {
			sawPending = true
		}
	}
	if !sawSettled || !sawPending {
		t.Fatalf("expected both settled_cash_break (%t) and pending_cash_break (%t)", sawSettled, sawPending)
	}
}

func TestResolutionPreservesOriginalEvidence(t *testing.T) {
	store, err := issues.NewStore("")
	if err != nil {
		t.Fatalf("new store: %v", err)
	}
	now := time.Date(2026, 1, 31, 16, 5, 0, 0, time.UTC)
	orig := domain.ReconIssue{
		ID:        "iss-immut-1",
		RunID:     "run-1",
		AccountID: "a-1",
		Kind:      domain.IssueKindLateFill,
		Severity:  domain.SeverityMedium,
		State:     domain.IssueStateOpen,
		Evidence: domain.IssueEvidence{
			Summary:       "Original immutable summary",
			BrokerData:    map[string]string{"broker_exec_id": "exec-99"},
			Deltas:        map[string]string{"arrival_delay_sec": "120.00"},
			PolicyVersion: "v1.0.0",
			Cutoff:        now,
			CapturedAt:    now,
		},
		CreatedAt: now,
		UpdatedAt: now,
	}
	if _, err := store.UpsertIssue(orig); err != nil {
		t.Fatalf("upsert failed: %v", err)
	}

	resolved, err := store.Resolve(orig.ID, "late_broker_event", "Verified on broker EOD statement", "ops-bob")
	if err != nil {
		t.Fatalf("resolve failed: %v", err)
	}
	if resolved.State != domain.IssueStateResolved {
		t.Fatalf("expected resolved state, got %s", resolved.State)
	}
	if resolved.Evidence.Summary != orig.Evidence.Summary || resolved.Evidence.BrokerData["broker_exec_id"] != "exec-99" {
		t.Fatalf("original evidence was modified during resolution!")
	}
	// Attempt caller-side mutation on returned copy and verify store remains protected
	resolved.Evidence.BrokerData["broker_exec_id"] = "tampered"
	reloaded, err := store.GetIssue(orig.ID)
	if err != nil {
		t.Fatalf("get issue: %v", err)
	}
	if reloaded.Evidence.BrokerData["broker_exec_id"] != "exec-99" {
		t.Fatalf("store evidence was mutated through returned reference!")
	}
}
