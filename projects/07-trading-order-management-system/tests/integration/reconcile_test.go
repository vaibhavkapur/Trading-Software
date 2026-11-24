package integration

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"oms/src/broker"
	"oms/src/brokersim"
	"oms/src/domain"
	"oms/src/fixed"
	"oms/src/reconcile"
	"oms/src/service"
	"oms/src/store"
	"oms/tests/harness"
)

// past moves the clock beyond the reconciliation grace window (5s in the
// harness), so that what just happened is old enough to be judged.
func past(e *harness.Env) { e.Advance(6 * time.Second) }

func issuesOfKind(e *harness.Env, status, kind string) []store.Issue {
	var out []store.Issue
	for _, is := range e.Issues(status) {
		if is.Kind == kind {
			out = append(out, is)
		}
	}
	return out
}

func TestReconcile_NothingToDoWhenBooksAgree(t *testing.T) {
	e := harness.New(t)
	o := e.Working()
	e.Fill(o.ID, 40, "150")
	rejected := e.Submitted()
	e.Must(e.Sim.Reject(rejected.ID, "no"))
	e.Flush()
	past(e)

	run := e.Reconcile()
	if run.DriftFound != 0 || run.AutoRepaired != 0 || run.IssuesOpened != 0 || run.OrdersChecked != 2 || run.ExecutionsChecked != 1 {
		t.Fatalf("run = %+v", run)
	}
	if run.BrokerAsOf == nil || run.FinishedAt == nil {
		t.Errorf("run timestamps missing: %+v", run)
	}
	if n := len(e.Issues("")); n != 0 {
		t.Errorf("%d issues recorded on a clean run", n)
	}
}

// Definition of done: reconciliation identifies discrepancies. Here the
// broker filled an order and every webhook about it was lost.
func TestReconcile_BooksAFillWhoseWebhookWasLost(t *testing.T) {
	e := harness.New(t)
	o := e.Working()
	e.Sim.SetFaults(brokersim.Faults{DropWebhooks: true})
	x1 := e.FillHeld(o.ID, 60, "150")
	e.FillHeld(o.ID, 40, "150.50")
	e.Sim.SetFaults(brokersim.Faults{})
	e.WantState(o.ID, domain.StateAcknowledged) // the OMS has heard nothing
	past(e)

	run := e.Reconcile()
	if run.AutoRepaired != 2 || run.DriftFound != 2 || run.IssuesOpened != 0 {
		t.Fatalf("run = %+v", run)
	}
	e.WantState(o.ID, domain.StateFilled)
	e.WantFilled(o.ID, 100, "15020")
	e.WantPosition(harness.Account1, "AAPL", 100, "-15020")

	// The repair is attributed and auditable.
	xs := e.Executions(o.ID)
	if len(xs) != 2 || xs[0].Source != domain.SourceReconciler || xs[0].BrokerExecID != x1 {
		t.Fatalf("executions = %+v", xs)
	}
	repairs := issuesOfKind(e, store.IssueResolved, store.KindMissingExecution)
	if len(repairs) != 2 || repairs[0].Resolution != "AUTO_REPAIRED" || repairs[0].ResolvedBy != "reconciler" || repairs[0].RunID != run.ID {
		t.Fatalf("repair records = %+v", repairs)
	}
	e.WantNoOpenIssues()

	// A second run finds nothing, and a webhook that finally turns up is a
	// duplicate of what reconciliation already booked.
	if again := e.Reconcile(); again.DriftFound != 0 {
		t.Fatalf("second run = %+v", again)
	}
	fee := fixed.MustParse("0.30")
	d := e.Send(broker.WebhookEvent{EventID: "evt-late", Type: broker.EventFill, ClientOrderID: o.ID,
		ExecID: x1, Qty: 60, Price: harness.Price("150"), Fee: &fee})
	if harness.Outcome(d) != "DUPLICATE" {
		t.Fatalf("late webhook: %s", d.Body)
	}
	e.WantFilled(o.ID, 100, "15020")
}

func TestReconcile_AppliesMissedStatusReports(t *testing.T) {
	e := harness.New(t)
	e.Sim.SetFaults(brokersim.Faults{DropWebhooks: true})

	acked := e.Submitted()
	e.Must(e.Sim.Ack(acked.ID))

	cancelled := e.Submitted()
	e.Must(e.Sim.Ack(cancelled.ID))
	e.Must(e.Sim.ConfirmCancel(cancelled.ID))

	rejected := e.Submitted()
	e.Must(e.Sim.Reject(rejected.ID, "NO_BUYING_POWER"))

	expired := e.Submitted()
	e.Must(e.Sim.Ack(expired.ID))
	e.Must(e.Sim.Expire(expired.ID, "session closed"))

	replaced := e.Submitted()
	e.Must(e.Sim.Ack(replaced.ID))
	past(e)
	// First run: the OMS learns the order is working, so that it can be
	// replaced; the replace confirmation is then lost as well.
	e.Reconcile()
	e.Replace(replaced.ID, 150, "").Want(http.StatusAccepted)
	e.Drain()
	e.Must(e.Sim.ConfirmReplace(replaced.ID))
	past(e)

	run := e.Reconcile()
	if run.IssuesOpened != 0 {
		t.Fatalf("run opened issues: %+v", run)
	}
	e.WantState(acked.ID, domain.StateAcknowledged)
	e.WantState(cancelled.ID, domain.StateCancelled)
	if got := e.WantState(rejected.ID, domain.StateRejected); got.RejectReason != "NO_BUYING_POWER" {
		t.Errorf("reject reason = %q", got.RejectReason)
	}
	e.WantState(expired.ID, domain.StateExpired)
	if got := e.WantState(replaced.ID, domain.StateAcknowledged); got.Qty != 150 || got.PendingReplace != nil {
		t.Errorf("replaced order = %+v", got)
	}

	// Every repaired event is attributed to the reconciler.
	evs := e.Events(cancelled.ID)
	last := evs[len(evs)-1]
	if last.Type != domain.EvOrderCancelled || last.Source != domain.SourceReconciler || !strings.HasPrefix(last.CausationID, "recon:") {
		t.Errorf("repair event = %+v", last)
	}
	if n := len(issuesOfKind(e, store.IssueResolved, store.KindMissedReport)); n < 6 {
		t.Errorf("%d MISSED_REPORT repair records, want at least 6", n)
	}
	e.WantNoOpenIssues()
}

// Activity inside the grace window is left alone: its webhooks may simply
// still be on their way.
func TestReconcile_LeavesRecentActivityAlone(t *testing.T) {
	e := harness.New(t)
	o := e.Working()
	e.Sim.SetFaults(brokersim.Faults{DropWebhooks: true})
	e.FillHeld(o.ID, 60, "150")
	e.Advance(2 * time.Second) // inside the 5s grace

	run := e.Reconcile()
	if run.DriftFound != 0 || run.AutoRepaired != 0 || run.ExecutionsChecked != 0 {
		t.Fatalf("run acted on fresh activity: %+v", run)
	}
	e.WantFilled(o.ID, 0, "0")

	e.Advance(4 * time.Second)
	if run = e.Reconcile(); run.AutoRepaired != 1 {
		t.Fatalf("run after the grace window = %+v", run)
	}
	e.WantFilled(o.ID, 60, "9000")
}

// Non-goal: executed trades are never mutated to hide a discrepancy. When
// the two sides hold different terms for a trade, an issue is opened and the
// OMS's record stays exactly as it was.
func TestReconcile_ExecutionMismatchIsReportedNotOverwritten(t *testing.T) {
	e := harness.New(t)
	o := e.Working()
	x := e.Fill(o.ID, 100, "150")
	if err := e.Sim.TamperExecution(x, fixed.MustParse("149")); err != nil {
		t.Fatal(err)
	}
	past(e)

	run := e.Reconcile()
	if run.IssuesOpened != 1 || run.AutoRepaired != 0 {
		t.Fatalf("run = %+v", run)
	}
	issue := e.OpenIssue(store.KindExecutionMismatch)
	if issue.Severity != store.SeverityCritical || issue.OrderID != o.ID || issue.Occurrences != 1 {
		t.Fatalf("issue = %+v", issue)
	}
	e.WantFilled(o.ID, 100, "15000")
	if xs := e.Executions(o.ID); len(xs) != 1 || xs[0].Price != fixed.MustParse("150") {
		t.Fatalf("the booked execution changed: %+v", xs)
	}

	// Seeing it again does not open a second issue.
	past(e)
	if run = e.Reconcile(); run.IssuesOpened != 0 || run.DriftFound != 1 {
		t.Fatalf("second run = %+v", run)
	}
	if issue = e.OpenIssue(store.KindExecutionMismatch); issue.Occurrences != 2 {
		t.Errorf("occurrences = %d, want 2", issue.Occurrences)
	}
	if got := e.Metric(); !strings.Contains(got, `oms_reconciliation_open_issues{severity="CRITICAL"} 1`) {
		t.Errorf("open issue gauge missing from metrics")
	}

	// The broker fixes its side; the next run sees agreement and closes it.
	if err := e.Sim.TamperExecution(x, fixed.MustParse("150")); err != nil {
		t.Fatal(err)
	}
	past(e)
	if run = e.Reconcile(); run.IssuesCleared != 1 {
		t.Fatalf("third run = %+v", run)
	}
	e.WantNoOpenIssues()
	cleared := issuesOfKind(e, store.IssueResolved, store.KindExecutionMismatch)
	if len(cleared) != 1 || cleared[0].ResolvedBy != "reconciler" {
		t.Fatalf("cleared issue = %+v", cleared)
	}
}

func TestReconcile_PhantomExecutionAndMissingOrder(t *testing.T) {
	e := harness.New(t)
	o := e.Working()
	e.Fill(o.ID, 40, "150")
	e.Sim.Forget(o.ID) // the broker's books no longer have the order or its trade
	past(e)

	run := e.Reconcile()
	if run.IssuesOpened != 2 {
		t.Fatalf("run = %+v", run)
	}
	if is := e.OpenIssue(store.KindPhantomExecution); is.Severity != store.SeverityCritical {
		t.Errorf("phantom severity = %s", is.Severity)
	}
	missing := e.OpenIssue(store.KindMissingAtBroker)
	// The OMS's books are untouched.
	e.WantState(o.ID, domain.StatePartiallyFilled)
	e.WantFilled(o.ID, 40, "6000")
	// An order that has traded cannot be written off as never submitted.
	e.Resolve(missing.ID, service.ActionRejectUnsubmitted, "try").WantError(http.StatusConflict, "ORDER_NOT_STUCK")
	e.OpenIssue(store.KindMissingAtBroker)
}

func TestReconcile_OrderMissingAtBrokerCanBeRejectedByAnOperator(t *testing.T) {
	e := harness.New(t)
	o := e.Submitted()
	e.Sim.Forget(o.ID)
	past(e)
	e.Reconcile()
	issue := e.OpenIssue(store.KindMissingAtBroker)
	e.Resolve(issue.ID, service.ActionRejectUnsubmitted, "broker lost it in their failover; client informed").Want(http.StatusOK)
	e.WantState(o.ID, domain.StateRejected)
	past(e)
	if run := e.Reconcile(); run.DriftFound != 0 {
		t.Fatalf("run after repair = %+v", run)
	}
	e.WantNoOpenIssues()
}

func TestReconcile_StateAndQuantityMismatches(t *testing.T) {
	e := harness.New(t)
	filled := e.Working()
	e.Fill(filled.ID, 100, "150")
	if err := e.Sim.SetStatus(filled.ID, broker.StatusWorking); err != nil {
		t.Fatal(err)
	}

	// The broker shows a different quantity than the OMS agreed to.
	qty := e.Working()
	e.Sim.InjectOrder(broker.OrderSnapshot{ClientOrderID: "ord_from_nowhere", BrokerOrderID: "B-X", Symbol: "TSLA",
		Side: domain.Sell, Status: broker.StatusWorking, Qty: 500})
	bo, _ := e.Sim.Order(qty.ID)
	e.Sim.Forget(qty.ID)
	bo.Qty = 90
	e.Sim.InjectOrder(bo)
	past(e)

	run := e.Reconcile()
	if run.IssuesOpened != 3 || run.AutoRepaired != 0 {
		t.Fatalf("run = %+v", run)
	}
	if is := e.OpenIssue(store.KindStateMismatch); is.OrderID != filled.ID {
		t.Errorf("state mismatch issue = %+v", is)
	}
	if is := e.OpenIssue(store.KindQtyMismatch); is.OrderID != qty.ID {
		t.Errorf("quantity mismatch issue = %+v", is)
	}
	if is := e.OpenIssue(store.KindOrphanOrder); is.OrderID != "ord_from_nowhere" || is.Severity != store.SeverityCritical {
		t.Errorf("orphan order issue = %+v", is)
	}
	e.WantState(filled.ID, domain.StateFilled)
	if got := e.Order(qty.ID); got.Qty != 100 {
		t.Errorf("order quantity was changed to %d", got.Qty)
	}
}

// An overfill discovered through reconciliation is quarantined exactly as
// one arriving by webhook, and repeated runs do not multiply the issue.
func TestReconcile_OverfillFoundInSnapshotIsQuarantined(t *testing.T) {
	e := harness.New(t)
	o := e.Working()
	e.Fill(o.ID, 80, "150")
	e.Sim.SetFaults(brokersim.Faults{DropWebhooks: true})
	e.FillHeld(o.ID, 50, "150")
	past(e)

	run := e.Reconcile()
	if run.IssuesOpened != 1 || run.AutoRepaired != 0 {
		t.Fatalf("run = %+v", run)
	}
	e.WantFilled(o.ID, 80, "12000")
	first := e.OpenIssue(domain.ReasonOverfill)

	past(e)
	run = e.Reconcile()
	again := e.OpenIssue(domain.ReasonOverfill)
	if run.IssuesOpened != 0 || again.ID != first.ID || again.Occurrences != 2 {
		t.Fatalf("second run = %+v, issue = %+v", run, again)
	}

	e.Resolve(first.ID, service.ActionAcceptExecution, "broker confirms the extra 30 are ours").Want(http.StatusOK)
	e.WantFilled(o.ID, 130, "19500")
	past(e)
	// Both sides now agree on 130 of 100.
	if run = e.Reconcile(); run.DriftFound != 0 {
		t.Fatalf("run after acceptance = %+v", run)
	}
	e.WantNoOpenIssues()
}

// When the broker's books cannot be read, the run fails loudly and judges
// nothing: no issue is opened and none is closed.
func TestReconcile_SnapshotUnavailable(t *testing.T) {
	e := harness.New(t)
	o := e.Working()
	x := e.Fill(o.ID, 100, "150")
	e.Sim.TamperExecution(x, fixed.MustParse("140"))
	past(e)
	e.Reconcile()
	e.OpenIssue(store.KindExecutionMismatch)

	e.Sim.SetFaults(brokersim.Faults{SnapshotDown: true})
	resp := e.Call(e.Olivia, http.MethodPost, "/v1/reconciliation/runs", nil).WantError(http.StatusBadGateway, "RECONCILIATION_FAILED")
	var body struct {
		Run store.Run `json:"run"`
	}
	resp.Decode(&body)
	if body.Run.Status != store.RunFailed || body.Run.Error == "" || body.Run.IssuesCleared != 0 {
		t.Fatalf("failed run = %+v", body.Run)
	}
	e.OpenIssue(store.KindExecutionMismatch) // still open
	if got := e.App.Metrics.ReconRuns.Value(store.RunFailed); got != 1 {
		t.Errorf("failed run counter = %v", got)
	}

	// The failed run is on record.
	var stored store.Run
	e.Call(e.Olivia, http.MethodGet, "/v1/reconciliation/runs/"+body.Run.ID, nil).Want(http.StatusOK).Decode(&stored)
	if stored.Status != store.RunFailed {
		t.Errorf("stored run = %+v", stored)
	}
}

func TestReconcile_API(t *testing.T) {
	e := harness.New(t)
	o := e.Working()
	e.Sim.SetFaults(brokersim.Faults{DropWebhooks: true})
	e.FillHeld(o.ID, 100, "150")
	past(e)

	var run store.Run
	e.Call(e.Olivia, http.MethodPost, "/v1/reconciliation/runs", nil).Want(http.StatusCreated).Decode(&run)
	if run.Status != store.RunCompleted || run.AutoRepaired != 1 || run.Trigger != "api:olivia" {
		t.Fatalf("run = %+v", run)
	}
	e.WantState(o.ID, domain.StateFilled)

	var list struct {
		Issues []store.Issue `json:"issues"`
	}
	e.Call(e.Olivia, http.MethodGet, "/v1/reconciliation/issues?status=resolved&order_id="+o.ID, nil).Want(http.StatusOK).Decode(&list)
	if len(list.Issues) != 1 || list.Issues[0].Kind != store.KindMissingExecution {
		t.Fatalf("issues = %+v", list.Issues)
	}
	var one store.Issue
	e.Call(e.Olivia, http.MethodGet, "/v1/reconciliation/issues/"+list.Issues[0].ID, nil).Want(http.StatusOK).Decode(&one)
	if one.ID != list.Issues[0].ID {
		t.Errorf("issue = %+v", one)
	}
	e.Call(e.Olivia, http.MethodGet, "/v1/reconciliation/issues?status=bogus", nil).WantError(http.StatusBadRequest, "BAD_REQUEST")
	e.Call(e.Olivia, http.MethodGet, "/v1/reconciliation/issues/iss_nope", nil).WantError(http.StatusNotFound, "ISSUE_NOT_FOUND")
	e.Call(e.Olivia, http.MethodGet, "/v1/reconciliation/runs/run_nope", nil).WantError(http.StatusNotFound, "RUN_NOT_FOUND")

	// Reconciliation is an operator function.
	for _, token := range []string{e.Alice, e.Vera} {
		e.Call(token, http.MethodPost, "/v1/reconciliation/runs", nil).WantError(http.StatusForbidden, "FORBIDDEN")
		e.Call(token, http.MethodGet, "/v1/reconciliation/issues", nil).WantError(http.StatusForbidden, "FORBIDDEN")
	}
}

// Only one run at a time, across every process sharing the database.
func TestReconcile_OneRunAtATime(t *testing.T) {
	e := harness.New(t)
	ctx := context.Background()
	other, err := e.App.Store.Pool().Acquire(ctx) // stands in for another OMS process
	if err != nil {
		t.Fatal(err)
	}
	defer other.Release()
	if _, err := other.Exec(ctx, `SELECT pg_advisory_lock($1)`, int64(reconcile.LockKey)); err != nil {
		t.Fatal(err)
	}
	e.Call(e.Olivia, http.MethodPost, "/v1/reconciliation/runs", nil).WantError(http.StatusConflict, "RUN_IN_PROGRESS")
	if _, err := other.Exec(ctx, `SELECT pg_advisory_unlock($1)`, int64(reconcile.LockKey)); err != nil {
		t.Fatal(err)
	}
	e.Call(e.Olivia, http.MethodPost, "/v1/reconciliation/runs", nil).Want(http.StatusCreated)
}

// Scheduled reconciliation repairs drift without anyone asking.
func TestReconcile_Scheduled(t *testing.T) {
	e := harness.New(t, func(o *harness.Options) {
		o.RealClock = true
		o.Reconcile = reconcile.Config{Grace: 50 * time.Millisecond, Interval: 40 * time.Millisecond}
	})
	o := e.Working()
	e.Sim.SetFaults(brokersim.Faults{DropWebhooks: true})
	e.FillHeld(o.ID, 100, "150")

	ctx, stop := context.WithCancel(context.Background())
	defer stop()
	go e.App.Reconciler.Schedule(ctx)
	e.Eventually(10*time.Second, "scheduled reconciliation to book the lost fill", func() bool {
		return e.Order(o.ID).State == domain.StateFilled
	})
	e.WantFilled(o.ID, 100, "15000")
}
