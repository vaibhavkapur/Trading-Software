package integration

import (
	"context"
	"encoding/json"
	"net/http"
	"reflect"
	"testing"
	"time"

	"oms/src/broker"
	"oms/src/domain"
	"oms/src/fixed"
	"oms/src/service"
	"oms/src/store"
	"oms/tests/harness"
)

// Definition of done: duplicate fills do not double-count quantity or cash.
func TestFills_RedeliveredWebhookIsCountedOnce(t *testing.T) {
	e := harness.New(t)
	o := e.Working()
	e.Fill(o.ID, 40, "150")

	ds := e.Accepted(e.Sim.RedeliverAll(context.Background()))
	ds = append(ds, e.Accepted(e.Sim.RedeliverAll(context.Background()))...)
	for i, outcome := range harness.Outcomes(ds) {
		if outcome != "DUPLICATE" {
			t.Errorf("redelivery %d (%s) outcome = %s, want DUPLICATE", i, ds[i].Type, outcome)
		}
	}

	e.WantState(o.ID, domain.StatePartiallyFilled)
	e.WantFilled(o.ID, 40, "6000")
	e.WantPosition(harness.Account1, "AAPL", 40, "-6000")
	if n := e.CountEvents(o.ID, domain.EvExecutionRecorded); n != 1 {
		t.Fatalf("%d ExecutionRecorded events, want 1", n)
	}
	if xs := e.Executions(o.ID); len(xs) != 1 {
		t.Fatalf("%d executions booked, want 1", len(xs))
	}

	// The inbox shows how often each report was delivered.
	inbox, err := e.App.Store.ListBrokerEvents(context.Background(), o.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(inbox) != 2 {
		t.Fatalf("%d inbox entries, want 2 (ack, fill)", len(inbox))
	}
	for _, row := range inbox {
		if row.Deliveries != 3 || row.Outcome != "APPLIED" {
			t.Errorf("inbox %s: %d deliveries, outcome %s; want 3, APPLIED", row.EventID, row.Deliveries, row.Outcome)
		}
	}
	if got := e.App.Metrics.BrokerEvents.Value("FILL", "BROKER", "DUPLICATE"); got != 2 {
		t.Errorf("duplicate fill counter = %v, want 2", got)
	}
}

// A broker that re-sends a trade under a new event id (for example after its
// own restart) is still recognised by the execution id.
func TestFills_SameExecutionUnderANewEventID(t *testing.T) {
	e := harness.New(t)
	o := e.Working()
	execID := e.Fill(o.ID, 40, "150")
	fee := fixed.MustParse("0.20")

	d := e.Send(broker.WebhookEvent{
		EventID: "evt-resent-1", Type: broker.EventFill, ClientOrderID: o.ID,
		ExecID: execID, Qty: 40, Price: harness.Price("150"), Fee: &fee,
	})
	if !d.OK() || harness.Outcome(d) != "DUPLICATE" {
		t.Fatalf("re-sent execution: %d %s", d.Status, d.Body)
	}
	e.WantFilled(o.ID, 40, "6000")
	e.WantPosition(harness.Account1, "AAPL", 40, "-6000")
	e.WantNoOpenIssues()
}

// The same execution id with different terms is a contradiction, not a
// duplicate. Nothing is applied and nothing is silently dropped.
func TestFills_SameExecutionIDWithDifferentTermsIsQuarantined(t *testing.T) {
	e := harness.New(t)
	o := e.Working()
	execID := e.Fill(o.ID, 40, "150")
	fee := fixed.MustParse("0.20")

	d := e.Send(broker.WebhookEvent{
		EventID: "evt-conflict", Type: broker.EventFill, ClientOrderID: o.ID,
		ExecID: execID, Qty: 45, Price: harness.Price("150"), Fee: &fee,
	})
	if !d.OK() || harness.Outcome(d) != "QUARANTINED" {
		t.Fatalf("conflicting execution: %d %s", d.Status, d.Body)
	}
	e.WantFilled(o.ID, 40, "6000")
	issue := e.OpenIssue(domain.ReasonConflictingReport)
	if issue.Severity != store.SeverityCritical || issue.OrderID != o.ID {
		t.Errorf("issue = %+v", issue)
	}
}

// Definition of done: total filled quantity never exceeds order quantity
// absent explicit corrections.
func TestFills_OverfillIsQuarantinedUntilAnOperatorAcceptsIt(t *testing.T) {
	e := harness.New(t)
	o := e.Working()
	e.Fill(o.ID, 60, "150")

	e.FillHeld(o.ID, 60, "151") // the broker says 120 of 100
	ds := e.Flush()
	if got := harness.Outcomes(ds); !reflect.DeepEqual(got, []string{"QUARANTINED"}) {
		t.Fatalf("overfill outcomes = %v", got)
	}

	held := e.WantState(o.ID, domain.StatePartiallyFilled)
	e.WantFilled(o.ID, 60, "9000")
	e.WantPosition(harness.Account1, "AAPL", 60, "-9000")
	if held.OverfillAccepted {
		t.Fatal("overfill was accepted without an operator")
	}

	issue := e.OpenIssue(domain.ReasonOverfill)
	if issue.Severity != store.SeverityCritical {
		t.Errorf("severity = %s, want CRITICAL", issue.Severity)
	}
	var details service.QuarantinedReport
	if err := json.Unmarshal(issue.Details, &details); err != nil {
		t.Fatal(err)
	}
	if details.Report.Qty != 60 || details.FilledQty != 60 || details.OrderQty != 100 {
		t.Errorf("issue details = %+v", details)
	}

	// Redelivering the report does not open a second issue or sneak it in.
	e.Accepted(e.Sim.RedeliverAll(context.Background()))
	e.WantFilled(o.ID, 60, "9000")
	e.OpenIssue(domain.ReasonOverfill)

	// Only an operator can accept it, and only with a reason.
	e.Call(e.Alice, http.MethodPost, "/v1/reconciliation/issues/"+issue.ID+"/resolve",
		map[string]string{"action": service.ActionAcceptExecution, "note": "x"}).WantError(http.StatusForbidden, "FORBIDDEN")
	e.Resolve(issue.ID, service.ActionAcceptExecution, "  ").WantError(http.StatusUnprocessableEntity, "VALIDATION_FAILED")

	var resolved store.Issue
	e.Resolve(issue.ID, service.ActionAcceptExecution, "confirmed with the broker desk, ticket 1234").
		Want(http.StatusOK).Decode(&resolved)
	if resolved.Status != store.IssueResolved || resolved.ResolvedBy != "olivia" || resolved.Resolution != "EXECUTION_ACCEPTED" {
		t.Errorf("resolved issue = %+v", resolved)
	}

	after := e.WantState(o.ID, domain.StateFilled)
	e.WantFilled(o.ID, 120, "18060")
	e.WantPosition(harness.Account1, "AAPL", 120, "-18060")
	if !after.OverfillAccepted {
		t.Error("overfill_accepted is not set")
	}
	evs := e.Events(o.ID)
	last := evs[len(evs)-1]
	if last.Type != domain.EvExecutionAccepted || last.Source != domain.SourceOperator || last.Actor != "olivia" ||
		last.CausationID != "issue:"+issue.ID {
		t.Errorf("acceptance event = %+v", last)
	}
	e.Resolve(issue.ID, service.ActionAcceptExecution, "again").WantError(http.StatusConflict, "ISSUE_NOT_OPEN")
	e.WantNoOpenIssues()
}

func TestFills_CorrectionAndBust(t *testing.T) {
	e := harness.New(t)
	o := e.Working()
	x1 := e.Fill(o.ID, 50, "150")
	x2 := e.Fill(o.ID, 50, "150")
	e.WantState(o.ID, domain.StateFilled)
	e.WantFilled(o.ID, 100, "15000")

	// The broker restates the first fill as 40 at 149.00.
	_, _, err := e.Sim.Correct(x1, 40, fixed.MustParse("149"))
	if err != nil {
		t.Fatal(err)
	}
	e.Flush()
	// FILLED → PARTIALLY_FILLED is a backward move, allowed only because a
	// correction event caused it.
	e.WantState(o.ID, domain.StatePartiallyFilled)
	e.WantFilled(o.ID, 90, "13460") // 40 × 149 + 50 × 150
	e.WantPosition(harness.Account1, "AAPL", 90, "-13460")

	// The broker busts the second fill.
	_, _, err = e.Sim.Bust(x2)
	if err != nil {
		t.Fatal(err)
	}
	e.Flush()
	after := e.WantState(o.ID, domain.StatePartiallyFilled)
	e.WantFilled(o.ID, 40, "5960")
	e.WantPosition(harness.Account1, "AAPL", 40, "-5960")
	if after.Fees != fixed.MustParse("0.20") {
		t.Errorf("fees = %s, want 0.20", after.Fees)
	}

	want := []string{"OrderCreated", "OrderValidated", "SubmissionStarted", "OrderAcknowledged",
		"ExecutionRecorded", "ExecutionRecorded", "ExecutionCorrected", "ExecutionBusted"}
	if got := e.EventTypes(o.ID); !reflect.DeepEqual(got, want) {
		t.Errorf("history = %v", got)
	}
	// The original executions are still on file, untouched: corrections are
	// new records, not edits.
	xs := e.Executions(o.ID)
	if len(xs) != 4 || xs[0].Qty != 50 || xs[0].Price != fixed.MustParse("150") || xs[2].RefBrokerExecID != x1 {
		t.Errorf("executions = %+v", xs)
	}
	e.WantNoOpenIssues()
}

func TestFills_BustOfTheOnlyFillReturnsToAcknowledged(t *testing.T) {
	e := harness.New(t)
	o := e.Working()
	x := e.Fill(o.ID, 100, "150")
	e.WantState(o.ID, domain.StateFilled)
	_, _, err := e.Sim.Bust(x)
	if err != nil {
		t.Fatal(err)
	}
	e.Flush()
	back := e.WantState(o.ID, domain.StateAcknowledged)
	e.WantFilled(o.ID, 0, "0")
	e.WantPosition(harness.Account1, "AAPL", 0, "0")
	if back.ClosedAt != nil || back.LeavesQty != 100 {
		t.Errorf("reopened order: closed_at %v leaves %d", back.ClosedAt, back.LeavesQty)
	}
	// The reopened order can trade again.
	e.Fill(o.ID, 100, "150.25")
	e.WantState(o.ID, domain.StateFilled)
	e.WantFilled(o.ID, 100, "15025")
}

// An execution can be replaced once. A second correction of the same record
// would make the totals depend on arrival order, so it is refused.
func TestFills_SecondCorrectionOfTheSameExecutionIsQuarantined(t *testing.T) {
	e := harness.New(t)
	o := e.Working()
	x := e.Fill(o.ID, 50, "150")
	if _, _, err := e.Sim.Correct(x, 40, fixed.MustParse("150")); err != nil {
		t.Fatal(err)
	}
	e.Flush()
	e.WantFilled(o.ID, 40, "6000")

	fee := fixed.MustParse("0.10")
	d := e.Send(broker.WebhookEvent{
		EventID: "evt-second-correction", Type: broker.EventFillCorrected, ClientOrderID: o.ID,
		ExecID: "X-second", RefExecID: x, Qty: 20, Price: harness.Price("150"), Fee: &fee,
	})
	if harness.Outcome(d) != "QUARANTINED" {
		t.Fatalf("second correction: %d %s", d.Status, d.Body)
	}
	e.WantFilled(o.ID, 40, "6000")
	e.OpenIssue(domain.ReasonConflictingReport)
}

func TestFills_LateFillAfterCancelIsBookedAndFlagged(t *testing.T) {
	e := harness.New(t)
	o := e.Working()
	e.Cancel(o.ID).Want(http.StatusAccepted)
	e.Drain()
	e.Must(e.Sim.ConfirmCancel(o.ID))
	e.Flush()
	e.WantState(o.ID, domain.StateCancelled)

	// The broker filled 30 before it processed the cancel and reports it late.
	e.Fill(o.ID, 30, "150")
	after := e.WantState(o.ID, domain.StateCancelled) // still cancelled: the broker closed it
	e.WantFilled(o.ID, 30, "4500")
	e.WantPosition(harness.Account1, "AAPL", 30, "-4500")
	if after.LeavesQty != 0 {
		t.Errorf("leaves = %d, want 0", after.LeavesQty)
	}
	evs := e.Events(o.ID)
	var p domain.Execution
	if err := json.Unmarshal(evs[len(evs)-1].Payload, &p); err != nil {
		t.Fatal(err)
	}
	if !p.Late {
		t.Error("the execution is not flagged as late")
	}
}

func TestFills_ExecutionOnARejectedOrderIsQuarantined(t *testing.T) {
	e := harness.New(t)
	o := e.Submitted()
	e.Must(e.Sim.Reject(o.ID, "NO_BUYING_POWER"))
	e.Flush()
	rejected := e.WantState(o.ID, domain.StateRejected)
	if rejected.RejectReason != "NO_BUYING_POWER" {
		t.Errorf("reject_reason = %q", rejected.RejectReason)
	}

	e.FillHeld(o.ID, 10, "150")
	if got := harness.Outcomes(e.Flush()); !reflect.DeepEqual(got, []string{"QUARANTINED"}) {
		t.Fatalf("outcomes = %v", got)
	}
	e.WantState(o.ID, domain.StateRejected)
	e.WantFilled(o.ID, 0, "0")
	if issue := e.OpenIssue(domain.ReasonUnexpectedExecution); issue.Severity != store.SeverityCritical {
		t.Errorf("severity = %s", issue.Severity)
	}
}

func TestFills_ReportForAnUnknownOrderRaisesAnIssue(t *testing.T) {
	e := harness.New(t)
	fee := fixed.MustParse("0.05")
	d := e.Send(broker.WebhookEvent{
		EventID: "evt-orphan", Type: broker.EventFill, ClientOrderID: "ord_nobody",
		ExecID: "X-orphan", Qty: 10, Price: harness.Price("150"), Fee: &fee,
	})
	if !d.OK() || harness.Outcome(d) != "UNKNOWN_ORDER" {
		t.Fatalf("orphan report: %d %s", d.Status, d.Body)
	}
	issue := e.OpenIssue(store.KindOrphanEvent)
	if issue.Severity != store.SeverityCritical || issue.OrderID != "ord_nobody" {
		t.Errorf("issue = %+v", issue)
	}
	// Redelivery is a duplicate, not a second issue.
	d = e.Send(broker.WebhookEvent{
		EventID: "evt-orphan", Type: broker.EventFill, ClientOrderID: "ord_nobody",
		ExecID: "X-orphan", Qty: 10, Price: harness.Price("150"), Fee: &fee,
	})
	if harness.Outcome(d) != "DUPLICATE" {
		t.Fatalf("redelivered orphan: %s", d.Body)
	}
	if n := len(e.Issues(store.IssueOpen)); n != 1 {
		t.Errorf("%d open issues, want 1", n)
	}
	e.Resolve(issue.ID, service.ActionAcknowledge, "broker test order, confirmed by their support").Want(http.StatusOK)
	e.WantNoOpenIssues()
}

// The database itself refuses to rewrite history: order events and
// executions are append-only.
func TestFills_EventsAndExecutionsAreAppendOnly(t *testing.T) {
	e := harness.New(t)
	o := e.Working()
	e.Fill(o.ID, 40, "150")
	ctx := context.Background()
	pool := e.App.Store.Pool()

	for _, stmt := range []string{
		`UPDATE executions SET qty = 1 WHERE order_id = $1`,
		`DELETE FROM executions WHERE order_id = $1`,
		`UPDATE order_events SET payload = '{}' WHERE order_id = $1`,
		`DELETE FROM order_events WHERE order_id = $1`,
	} {
		if _, err := pool.Exec(ctx, stmt, o.ID); err == nil {
			t.Errorf("the database allowed: %s", stmt)
		}
	}
	for _, stmt := range []string{`TRUNCATE executions`, `TRUNCATE order_events CASCADE`} {
		if _, err := pool.Exec(ctx, stmt); err == nil {
			t.Errorf("the database allowed: %s", stmt)
		}
	}
	// And it refuses a silent overfill even if application code tried one.
	if _, err := pool.Exec(ctx, `UPDATE orders SET filled_qty = qty + 1 WHERE id = $1`, o.ID); err == nil {
		t.Error("the database allowed filled_qty above qty without overfill_accepted")
	}
	e.WantFilled(o.ID, 40, "6000")
}

// Fills for different orders on the same position are booked by concurrent
// transactions, which can reach the position row in a different order from
// the one their events have in the log. The row must not depend on that
// order, or it could not be reproduced by replaying the log. Quantities and
// cash are sums; the timestamp is the latest one seen.
func TestPosition_DoesNotDependOnTheOrderFillsAreBooked(t *testing.T) {
	e := harness.New(t)
	a, b := e.Working(), e.Working()

	e.Advance(10 * time.Second)
	later := e.Now()
	e.Fill(a.ID, 100, "150")
	// The second fill is recorded by a transaction that read its clock
	// earlier but wrote later.
	e.Advance(-4 * time.Second)
	e.Fill(b.ID, 100, "151")

	p := e.WantPosition(harness.Account1, "AAPL", 200, "-30100")
	if !p.UpdatedAt.Equal(later) {
		t.Errorf("position updated_at = %v, want the latest fill time %v", p.UpdatedAt, later)
	}
	if rep := e.Verify(); rep.Positions != 1 {
		t.Errorf("verify report: %+v", rep)
	}
}
