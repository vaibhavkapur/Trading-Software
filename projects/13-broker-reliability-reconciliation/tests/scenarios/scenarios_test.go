package scenarios_test

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/trading-software-projects/broker-reliability-reconciliation/src/connectors"
	"github.com/trading-software-projects/broker-reliability-reconciliation/src/domain"
	"github.com/trading-software-projects/broker-reliability-reconciliation/src/identity"
	"github.com/trading-software-projects/broker-reliability-reconciliation/src/issues"
	"github.com/trading-software-projects/broker-reliability-reconciliation/src/metrics"
	"github.com/trading-software-projects/broker-reliability-reconciliation/src/reconcile"
	"github.com/trading-software-projects/broker-reliability-reconciliation/src/scheduler"
)

func loadPaperFixture(t *testing.T) *connectors.FixtureDataset {
	t.Helper()
	ds, err := connectors.LoadFixture(filepath.Join("..", "..", "examples", "fixtures", "paper_day.json"))
	if err != nil {
		t.Fatalf("load fixture: %v", err)
	}
	return ds
}

// Definition of Done #1: Known injected discrepancies each generate the expected typed issue.
func TestAllInjectedDiscrepanciesProduceExpectedTypedIssues(t *testing.T) {
	ds := loadPaperFixture(t)
	store, _ := issues.NewStore("")
	runner := scheduler.NewRunner(
		connectors.NewSimulatedInternalConnector(ds, 3),
		connectors.NewSimulatedBrokerConnector(ds, 3),
		reconcile.NewEngine(identity.NewMapper(ds.SymbolAliases)),
		store,
		metrics.NewCollector(),
	)

	_, found, err := runner.Execute(context.Background(), scheduler.RunRequest{
		AccountID: ds.AccountID,
		AsOf:      ds.DefaultAsOf,
	})
	if err != nil {
		t.Fatalf("execute failed: %v", err)
	}

	counts := make(map[domain.IssueKind]int)
	for _, iss := range found {
		counts[iss.Kind]++
	}

	expected := []domain.IssueKind{
		domain.IssueKindMissingBrokerFill,
		domain.IssueKindMissingLedgerFill,
		domain.IssueKindDuplicateBrokerFill,
		domain.IssueKindLateFill,
		domain.IssueKindFillMismatch,
		domain.IssueKindMissingBrokerOrder,
		domain.IssueKindPositionBreak,
		domain.IssueKindSettledCashBreak,
		domain.IssueKindPendingCashBreak,
	}
	for _, kind := range expected {
		if counts[kind] != 1 {
			t.Errorf("expected exactly 1 issue of kind %s, got %d", kind, counts[kind])
		}
	}
}

// Definition of Done #2: A rerun with the same cutoff/policy/input snapshots gives the same results.
func TestDeterministicReplayEquivalence(t *testing.T) {
	ds := loadPaperFixture(t)
	fixedClock := func() time.Time { return time.Date(2026, 1, 31, 16, 1, 0, 0, time.UTC) }

	runOnce := func() (domain.ReconRun, []domain.ReconIssue) {
		store, _ := issues.NewStore("")
		engine := reconcile.NewEngine(identity.NewMapper(ds.SymbolAliases)).WithClock(fixedClock)
		runner := scheduler.NewRunner(
			connectors.NewSimulatedInternalConnector(ds, 2),
			connectors.NewSimulatedBrokerConnector(ds, 2),
			engine,
			store,
			metrics.NewCollector(),
		)
		run, list, err := runner.Execute(context.Background(), scheduler.RunRequest{
			AccountID: ds.AccountID,
			AsOf:      ds.DefaultAsOf,
		})
		if err != nil {
			t.Fatalf("execute failed: %v", err)
		}
		return run, list
	}

	runA, issuesA := runOnce()
	runB, issuesB := runOnce()

	if runA.ID != runB.ID || runA.InputDigest != runB.InputDigest {
		t.Fatalf("non-deterministic run identity: (%s,%s) vs (%s,%s)", runA.ID, runA.InputDigest, runB.ID, runB.InputDigest)
	}
	if !runA.Summary.MonetaryExposure.Equal(runB.Summary.MonetaryExposure) {
		t.Fatalf("non-deterministic monetary exposure: %s vs %s", runA.Summary.MonetaryExposure, runB.Summary.MonetaryExposure)
	}
	if len(issuesA) != len(issuesB) {
		t.Fatalf("issue count diverged: %d vs %d", len(issuesA), len(issuesB))
	}
	for i := range issuesA {
		if issuesA[i].ID != issuesB[i].ID || issuesA[i].Kind != issuesB[i].Kind || !issuesA[i].MonetaryExposure.Equal(issuesB[i].MonetaryExposure) {
			t.Fatalf("issue[%d] diverged between replays: %+v vs %+v", i, issuesA[i], issuesB[i])
		}
	}
}

// Scenario: Connection loss fails safely without corrupting existing state.
func TestScenarioConnectionLoss(t *testing.T) {
	ds := loadPaperFixture(t)
	store, _ := issues.NewStore("")
	brokerConn := connectors.NewSimulatedBrokerConnector(ds, 5)
	brokerConn.SetFaults(connectors.FaultProfile{Disconnect: true})

	runner := scheduler.NewRunner(
		connectors.NewSimulatedInternalConnector(ds, 5),
		brokerConn,
		reconcile.NewEngine(identity.NewMapper(ds.SymbolAliases)),
		store,
		metrics.NewCollector(),
	).WithRetryConfig(connectors.RetryConfig{MaxAttempts: 2, BaseBackoff: 1 * time.Millisecond})

	run, _, err := runner.Execute(context.Background(), scheduler.RunRequest{
		AccountID: ds.AccountID,
		AsOf:      ds.DefaultAsOf,
	})
	if !errors.Is(err, connectors.ErrDisconnected) {
		t.Fatalf("expected ErrDisconnected, got %v", err)
	}
	if run.Status != domain.RunStatusFailed {
		t.Fatalf("expected RunStatusFailed, got %s", run.Status)
	}
}

// Scenario: Stale broker snapshots trigger stale_snapshot alerts.
func TestScenarioStaleSnapshots(t *testing.T) {
	ds := loadPaperFixture(t)
	store, _ := issues.NewStore("")
	brokerConn := connectors.NewSimulatedBrokerConnector(ds, 10)
	brokerConn.SetFaults(connectors.FaultProfile{StaleSnapshotShift: 20 * time.Minute})

	runner := scheduler.NewRunner(
		connectors.NewSimulatedInternalConnector(ds, 10),
		brokerConn,
		reconcile.NewEngine(identity.NewMapper(ds.SymbolAliases)),
		store,
		metrics.NewCollector(),
	)

	_, found, err := runner.Execute(context.Background(), scheduler.RunRequest{
		AccountID: ds.AccountID,
		AsOf:      ds.DefaultAsOf,
	})
	if err != nil {
		t.Fatalf("execute failed: %v", err)
	}
	staleCount := 0
	for _, iss := range found {
		if iss.Kind == domain.IssueKindStaleSnapshot {
			staleCount++
		}
	}
	if staleCount < 2 {
		t.Fatalf("expected multiple stale_snapshot issues for broker positions & cash, got %d", staleCount)
	}
}

// Scenario: Clock skew anomaly where venue event_time is ahead of local receive_time.
func TestScenarioClockSkew(t *testing.T) {
	ds := loadPaperFixture(t)
	store, _ := issues.NewStore("")
	brokerConn := connectors.NewSimulatedBrokerConnector(ds, 10)
	// Shift broker EventTime forward by +45s (beyond 5s policy clock skew tolerance)
	brokerConn.SetFaults(connectors.FaultProfile{ClockSkewShift: 45 * time.Second})

	runner := scheduler.NewRunner(
		connectors.NewSimulatedInternalConnector(ds, 10),
		brokerConn,
		reconcile.NewEngine(identity.NewMapper(ds.SymbolAliases)),
		store,
		metrics.NewCollector(),
	)

	_, found, err := runner.Execute(context.Background(), scheduler.RunRequest{
		AccountID: ds.AccountID,
		AsOf:      ds.DefaultAsOf.Add(2 * time.Minute),
	})
	if err != nil {
		t.Fatalf("execute failed: %v", err)
	}
	skewCount := 0
	for _, iss := range found {
		if iss.Kind == domain.IssueKindClockSkewAnomaly {
			skewCount++
		}
	}
	if skewCount == 0 {
		t.Fatal("expected clock_skew_anomaly issues when broker EventTime leads ReceiveTime")
	}
}

// Scenario: Fault-injected lost webhook + duplicate delivery.
func TestScenarioLostWebhookAndDuplicateDelivery(t *testing.T) {
	ds := loadPaperFixture(t)
	store, _ := issues.NewStore("")
	brokerConn := connectors.NewSimulatedBrokerConnector(ds, 10)
	brokerConn.SetFaults(connectors.FaultProfile{
		DropBrokerFillIDs:  []string{"exec-aapl-1"},
		DuplicateBrokerIDs: []string{"exec-msft-1"},
	})

	runner := scheduler.NewRunner(
		connectors.NewSimulatedInternalConnector(ds, 10),
		brokerConn,
		reconcile.NewEngine(identity.NewMapper(ds.SymbolAliases)),
		store,
		metrics.NewCollector(),
	)

	_, found, err := runner.Execute(context.Background(), scheduler.RunRequest{
		AccountID: ds.AccountID,
		AsOf:      ds.DefaultAsOf,
	})
	if err != nil {
		t.Fatalf("execute failed: %v", err)
	}

	var sawDroppedAAPL, sawDupMSFT bool
	for _, iss := range found {
		if iss.Kind == domain.IssueKindMissingBrokerFill && iss.Symbol == "AAPL" {
			sawDroppedAAPL = true
		}
		if iss.Kind == domain.IssueKindDuplicateBrokerFill && iss.Symbol == "MSFT" {
			sawDupMSFT = true
		}
	}
	if !sawDroppedAAPL {
		t.Fatal("expected missing_broker_fill for dropped webhook exec-aapl-1")
	}
	if !sawDupMSFT {
		t.Fatal("expected duplicate_broker_fill for duplicated exec-msft-1")
	}
}

// Scenario: Rejected storage writes surface ErrWriteRejected cleanly.
func TestScenarioRejectedWrites(t *testing.T) {
	ds := loadPaperFixture(t)
	store, _ := issues.NewStore("")
	store.SetRejectWrites(true)

	runner := scheduler.NewRunner(
		connectors.NewSimulatedInternalConnector(ds, 10),
		connectors.NewSimulatedBrokerConnector(ds, 10),
		reconcile.NewEngine(identity.NewMapper(ds.SymbolAliases)),
		store,
		metrics.NewCollector(),
	)

	_, _, err := runner.Execute(context.Background(), scheduler.RunRequest{
		AccountID: ds.AccountID,
		AsOf:      ds.DefaultAsOf,
	})
	if !errors.Is(err, issues.ErrWriteRejected) {
		t.Fatalf("expected ErrWriteRejected, got %v", err)
	}
	if len(store.ListIssues(issues.IssueFilter{})) != 0 {
		t.Fatal("expected zero partial issues stored when writes are rejected")
	}
}
