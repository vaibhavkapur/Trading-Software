package integration

import (
	"context"
	"encoding/json"
	"net/http"
	"sync"
	"testing"
	"time"

	"oms/src/broker"
	"oms/src/brokersim"
	"oms/src/domain"
	"oms/src/service"
	"oms/src/store"
	"oms/tests/harness"
)

func submitRow(t *testing.T, e *harness.Env, orderID string) store.OutboxRow {
	t.Helper()
	for _, row := range e.Outbox(orderID) {
		if row.EventType == store.OutboxSubmit {
			return row
		}
	}
	t.Fatalf("order %s has no submission in the outbox", orderID)
	return store.OutboxRow{}
}

func TestOutbox_OrderAndSubmissionCommitTogether(t *testing.T) {
	e := harness.New(t)
	o := e.Created()
	row := submitRow(t, e, o.ID)
	if row.Status != store.OutboxPending || row.Attempts != 0 {
		t.Fatalf("queued submission = %+v", row)
	}
	e.Drain()
	row = submitRow(t, e, o.ID)
	if row.Status != store.OutboxDelivered || row.Attempts != 1 || row.DeliveredAt == nil {
		t.Fatalf("delivered submission = %+v", row)
	}
	// SUBMITTING was recorded with a link to the outbox message.
	evs := e.Events(o.ID)
	last := evs[len(evs)-1]
	if last.Type != domain.EvSubmissionStarted || last.Source != domain.SourceOutbox {
		t.Fatalf("last event = %+v", last)
	}
	// Nothing left to do.
	if n := e.Drain(); n != 0 {
		t.Errorf("a second drain handled %d messages", n)
	}
}

// The broker is briefly unavailable: the submission is retried with backoff
// and arrives exactly once.
func TestOutbox_RetriesWithBackoffUntilTheBrokerAccepts(t *testing.T) {
	e := harness.New(t)
	e.Sim.SetFaults(brokersim.Faults{FailRequests: 2})
	o := e.Created()

	e.Drain()
	row := submitRow(t, e, o.ID)
	if row.Status != store.OutboxPending || row.Attempts != 1 || row.LastError == "" {
		t.Fatalf("after first failure: %+v", row)
	}
	// The order says a submission may be in flight, which is the truth.
	e.WantState(o.ID, domain.StateSubmitting)

	// Not due yet: the retry waits out its backoff (1s, then 2s).
	if n := e.Drain(); n != 0 {
		t.Fatalf("retried before the backoff elapsed (%d messages)", n)
	}
	e.Advance(time.Second)
	e.Drain()
	if row = submitRow(t, e, o.ID); row.Attempts != 2 || row.Status != store.OutboxPending {
		t.Fatalf("after second failure: %+v", row)
	}
	e.Advance(time.Second)
	if n := e.Drain(); n != 0 {
		t.Fatalf("second retry ran after 1s; backoff should have doubled (%d messages)", n)
	}
	e.Advance(time.Second)
	e.Drain()
	if row = submitRow(t, e, o.ID); row.Status != store.OutboxDelivered || row.Attempts != 3 {
		t.Fatalf("after recovery: %+v", row)
	}

	if _, known := e.Sim.Order(o.ID); !known {
		t.Fatal("the broker never received the order")
	}
	if n := e.CountEvents(o.ID, domain.EvSubmissionStarted); n != 1 {
		t.Errorf("%d SubmissionStarted events, want 1", n)
	}
	if got := e.App.Metrics.OutboxDispatches.Value(store.OutboxSubmit, "retry"); got != 2 {
		t.Errorf("retry counter = %v, want 2", got)
	}
}

// The broker processed the submission but its response was lost. The retry
// carries the same client order id, so the broker does not create a second
// order.
func TestOutbox_LostResponseDoesNotDuplicateTheOrderAtTheBroker(t *testing.T) {
	e := harness.New(t)
	e.Sim.SetFaults(brokersim.Faults{LoseResponses: 1})
	o := e.Created()
	e.Drain()
	if _, known := e.Sim.Order(o.ID); !known {
		t.Fatal("the broker should have processed the first attempt")
	}
	if row := submitRow(t, e, o.ID); row.Status != store.OutboxPending {
		t.Fatalf("the OMS believes the lost attempt succeeded: %+v", row)
	}

	e.Advance(time.Second)
	e.Drain()
	if row := submitRow(t, e, o.ID); row.Status != store.OutboxDelivered {
		t.Fatalf("after retry: %+v", row)
	}
	st := e.Sim.Stats()
	if st.SubmitRequests != 2 || st.DuplicateSubmits != 1 {
		t.Fatalf("broker stats = %+v, want 2 submissions of which 1 duplicate", st)
	}
	if n := len(e.Sim.Snapshot().Orders); n != 1 {
		t.Fatalf("the broker has %d orders, want 1", n)
	}
}

// Definition of done: restarting workers drains the outbox safely. A worker
// leases a message, the broker accepts the call, and the worker dies before
// recording that. Another worker takes over when the lease runs out.
func TestOutbox_WorkerDiesAfterTheBrokerAccepted(t *testing.T) {
	e := harness.New(t)
	ctx := context.Background()
	o := e.Created()

	// The doomed worker: lease the message and make the call, then vanish.
	rows, err := e.App.Store.ClaimOutbox(ctx, "doomed", 10, 30*time.Second)
	if err != nil || len(rows) != 1 {
		t.Fatalf("claim: %v, %d rows", err, len(rows))
	}
	if err := e.Broker.Submit(ctx, broker.SubmitRequest{
		ClientOrderID: o.ID, Symbol: o.Symbol, Side: o.Side, Type: o.Type,
		TimeInForce: o.TimeInForce, Qty: o.Qty, LimitPrice: o.LimitPrice,
	}); err != nil {
		t.Fatal(err)
	}

	// While the lease is held nobody else touches the message.
	survivor := e.NewWorker("survivor")
	if n, _ := survivor.Drain(ctx); n != 0 {
		t.Fatalf("a second worker took a leased message (%d)", n)
	}
	e.Advance(29 * time.Second)
	if n, _ := survivor.Drain(ctx); n != 0 {
		t.Fatalf("a second worker took the message before the lease expired (%d)", n)
	}

	e.Advance(2 * time.Second)
	if n, err := survivor.Drain(ctx); err != nil || n != 1 {
		t.Fatalf("survivor drained %d (%v), want 1", n, err)
	}
	row := submitRow(t, e, o.ID)
	if row.Status != store.OutboxDelivered || row.Attempts != 2 {
		t.Fatalf("after takeover: %+v", row)
	}
	e.WantState(o.ID, domain.StateSubmitting)
	st := e.Sim.Stats()
	if st.SubmitRequests != 2 || st.DuplicateSubmits != 1 || len(e.Sim.Snapshot().Orders) != 1 {
		t.Fatalf("broker stats = %+v with %d orders; want the resend deduplicated", st, len(e.Sim.Snapshot().Orders))
	}

	// The order carries on normally.
	e.Must(e.Sim.Ack(o.ID))
	e.Flush()
	e.Fill(o.ID, 100, "150")
	e.WantState(o.ID, domain.StateFilled)
}

// A worker that lost its lease must not overwrite the outcome recorded by
// the worker that took over.
func TestOutbox_StaleWorkerCannotFinishAMessageItNoLongerOwns(t *testing.T) {
	e := harness.New(t)
	ctx := context.Background()
	o := e.Created()
	rows, err := e.App.Store.ClaimOutbox(ctx, "slow", 10, 30*time.Second)
	if err != nil || len(rows) != 1 {
		t.Fatalf("claim: %v, %d rows", err, len(rows))
	}
	e.Advance(31 * time.Second)
	e.Drain() // worker-1 takes over and delivers

	var ok bool
	err = e.App.Store.Tx(ctx, func(tx *store.Tx) error {
		var err error
		ok, err = tx.FinishOutbox(ctx, rows[0].ID, "slow", store.OutboxDead, "late and wrong")
		return err
	})
	if err != nil || ok {
		t.Fatalf("the stale worker finished the message: ok=%v err=%v", ok, err)
	}
	if row := submitRow(t, e, o.ID); row.Status != store.OutboxDelivered {
		t.Fatalf("message status = %s, want DELIVERED", row.Status)
	}
}

func TestOutbox_ManyOrdersTwoWorkersEachSubmittedOnce(t *testing.T) {
	e := harness.New(t)
	const n = 60
	orderIDs := make([]string, n)
	for i := range orderIDs {
		orderIDs[i] = e.Created().ID
	}

	second := e.NewWorker("worker-2")
	var wg sync.WaitGroup
	for _, w := range []interface {
		Drain(context.Context) (int, error)
	}{e.App.Worker, second} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := w.Drain(context.Background()); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	e.Drain()

	st := e.Sim.Stats()
	if st.SubmitRequests != n || st.DuplicateSubmits != 0 {
		t.Fatalf("broker stats = %+v, want %d submissions and no duplicates", st, n)
	}
	for _, id := range orderIDs {
		e.WantState(id, domain.StateSubmitting)
		if row := submitRow(t, e, id); row.Status != store.OutboxDelivered || row.Attempts != 1 {
			t.Fatalf("order %s submission = %+v", id, row)
		}
	}
}

// Calls for one order are delivered in the order they were queued: a cancel
// never overtakes the replace queued before it, even when the replace has
// to be retried.
func TestOutbox_CallsForOneOrderStayInOrder(t *testing.T) {
	e := harness.New(t)
	o := e.Working()
	e.Replace(o.ID, 150, "").Want(http.StatusAccepted)
	e.Cancel(o.ID).Want(http.StatusAccepted)

	e.Sim.SetFaults(brokersim.Faults{FailRequests: 1})
	e.Drain()
	st := e.Sim.Stats()
	if st.ReplaceRequests != 0 || st.CancelRequests != 0 {
		t.Fatalf("after the failed replace the broker saw %+v; the cancel must wait behind it", st)
	}
	e.Advance(time.Second)
	e.Drain()
	st = e.Sim.Stats()
	if st.ReplaceRequests != 1 || st.CancelRequests != 1 {
		t.Fatalf("broker stats = %+v, want one replace and one cancel", st)
	}
	rows := e.Outbox(o.ID)
	if len(rows) != 3 || rows[1].EventType != store.OutboxReplace || rows[2].EventType != store.OutboxCancel ||
		!rows[1].DeliveredAt.Before(*rows[2].DeliveredAt) && !rows[1].DeliveredAt.Equal(*rows[2].DeliveredAt) {
		t.Fatalf("outbox rows = %+v", rows)
	}
}

func TestOutbox_CancelBeforeDispatchNeverReachesTheBroker(t *testing.T) {
	e := harness.New(t)
	o := e.Created()
	got := e.Cancel(o.ID).Want(http.StatusOK).Order()
	if got.State != domain.StateCancelled || got.PendingCancel {
		t.Fatalf("cancelled order = %+v", got)
	}
	e.Drain()
	if row := submitRow(t, e, o.ID); row.Status != store.OutboxSkipped {
		t.Fatalf("submission = %+v, want SKIPPED", row)
	}
	if st := e.Sim.Stats(); st.SubmitRequests != 0 {
		t.Fatalf("the broker received %d submissions for a cancelled order", st.SubmitRequests)
	}
	if n := e.CountEvents(o.ID, domain.EvSubmissionStarted); n != 0 {
		t.Error("a cancelled order was marked SUBMITTING")
	}
}

// A broker outage longer than the retry budget: the submission is
// dead-lettered and an operator decides. The order stays SUBMITTING because
// nobody knows whether the broker has it.
func TestOutbox_DeadLetterThenRequeue(t *testing.T) {
	e := harness.New(t)
	e.Sim.SetFaults(brokersim.Faults{Down: true})
	o := e.Created()

	for i := 0; i < 4; i++ { // MaxAttempts is 4 in the harness
		e.Drain()
		e.Advance(10 * time.Second)
	}
	row := submitRow(t, e, o.ID)
	if row.Status != store.OutboxDead || row.Attempts != 4 {
		t.Fatalf("submission = %+v, want DEAD after 4 attempts", row)
	}
	e.WantState(o.ID, domain.StateSubmitting)

	issue := e.OpenIssue(store.KindOutboxDead)
	var dl service.DeadLetter
	if err := json.Unmarshal(issue.Details, &dl); err != nil {
		t.Fatal(err)
	}
	if issue.Severity != store.SeverityHigh || issue.OrderID != o.ID || dl.OutboxID != row.ID || dl.Attempts != 4 || dl.LastError == "" {
		t.Fatalf("issue = %+v details = %+v", issue, dl)
	}
	// Dead messages are not retried on their own.
	e.Advance(time.Hour)
	if n := e.Drain(); n != 0 {
		t.Fatalf("a dead message was retried (%d)", n)
	}

	// The wrong repairs are refused.
	e.Resolve(issue.ID, service.ActionAcceptExecution, "no").WantError(http.StatusConflict, "ACTION_NOT_APPLICABLE")
	e.Resolve(issue.ID, "MAKE_IT_GO_AWAY", "no").WantError(http.StatusUnprocessableEntity, "UNKNOWN_ACTION")

	// The broker comes back; the operator requeues.
	e.Sim.SetFaults(brokersim.Faults{})
	e.Resolve(issue.ID, service.ActionRequeueOutbox, "broker outage INC-77 is over").Want(http.StatusOK)
	e.Drain()
	if row = submitRow(t, e, o.ID); row.Status != store.OutboxDelivered {
		t.Fatalf("after requeue: %+v", row)
	}
	if _, known := e.Sim.Order(o.ID); !known {
		t.Fatal("the broker did not receive the requeued order")
	}
	e.WantNoOpenIssues()
}

func TestOutbox_DeadLetterThenRejectUnsubmitted(t *testing.T) {
	e := harness.New(t)
	e.Sim.SetFaults(brokersim.Faults{Down: true})
	o := e.Created()
	for i := 0; i < 4; i++ {
		e.Drain()
		e.Advance(10 * time.Second)
	}
	issue := e.OpenIssue(store.KindOutboxDead)

	// While the broker cannot be asked, the order cannot be written off.
	e.Resolve(issue.ID, service.ActionRejectUnsubmitted, "give up").WantError(http.StatusServiceUnavailable, "BROKER_UNAVAILABLE")
	e.WantState(o.ID, domain.StateSubmitting)

	e.Sim.SetFaults(brokersim.Faults{})
	e.Resolve(issue.ID, service.ActionRejectUnsubmitted, "broker confirms it never arrived; client re-entered").Want(http.StatusOK)
	rejected := e.WantState(o.ID, domain.StateRejected)
	if rejected.ClosedAt == nil {
		t.Error("closed_at is not set")
	}
	evs := e.Events(o.ID)
	last := evs[len(evs)-1]
	if last.Type != domain.EvOrderRejected || last.Source != domain.SourceOperator || last.Actor != "olivia" {
		t.Fatalf("last event = %+v", last)
	}
	e.WantNoOpenIssues()
}

// The same repair is refused when the broker does have the order: rejecting
// it would leave a live order that nobody tracks.
func TestOutbox_RejectUnsubmittedIsRefusedWhenTheBrokerHasTheOrder(t *testing.T) {
	e := harness.New(t)
	e.Sim.SetFaults(brokersim.Faults{LoseResponses: 4})
	o := e.Created()
	for i := 0; i < 4; i++ {
		e.Drain()
		e.Advance(10 * time.Second)
	}
	issue := e.OpenIssue(store.KindOutboxDead)
	e.Resolve(issue.ID, service.ActionRejectUnsubmitted, "give up").WantError(http.StatusConflict, "BROKER_HAS_ORDER")
	e.WantState(o.ID, domain.StateSubmitting)

	// Reconciliation is the right tool: the broker has it as RECEIVED, the
	// OMS as SUBMITTING, and those agree. The dead-letter issue is then
	// acknowledged by the operator.
	e.Advance(10 * time.Second)
	e.Reconcile()
	e.Resolve(issue.ID, service.ActionAcknowledge, "broker has the order; verified by reconciliation").Want(http.StatusOK)
	e.Must(e.Sim.Ack(o.ID))
	e.Flush()
	e.WantState(o.ID, domain.StateAcknowledged)
	e.WantNoOpenIssues()
}

// refusingBroker refuses every call the way a broker that understood the
// request and said no would.
type refusingBroker struct{ broker.Client }

func (refusingBroker) Submit(context.Context, broker.SubmitRequest) error {
	return &broker.Error{Op: "submit", Status: 422, Code: "ACCOUNT_RESTRICTED", Message: "account is restricted"}
}

func (refusingBroker) Cancel(context.Context, string, string) error {
	return &broker.Error{Op: "cancel", Status: 409, Code: "TOO_LATE", Message: "order is closed"}
}

func TestOutbox_PermanentRefusalIsRecordedNotRetried(t *testing.T) {
	e := harness.New(t)
	ctx := context.Background()
	refuser := e.NewWorkerWith("refuser", refusingBroker{e.Broker})

	o := e.Created()
	if n, err := refuser.Drain(ctx); err != nil || n != 1 {
		t.Fatalf("drain: %d, %v", n, err)
	}
	rejected := e.WantState(o.ID, domain.StateRejected)
	if rejected.RejectReason != "ACCOUNT_RESTRICTED: account is restricted" {
		t.Errorf("reject_reason = %q", rejected.RejectReason)
	}
	if row := submitRow(t, e, o.ID); row.Status != store.OutboxRejected || row.Attempts != 1 {
		t.Fatalf("submission = %+v", row)
	}
	e.Advance(time.Hour)
	if n, _ := refuser.Drain(ctx); n != 0 {
		t.Fatal("a refused submission was retried")
	}

	// A refused cancel clears the pending flag and leaves the order working.
	w := e.Working()
	e.Cancel(w.ID).Want(http.StatusAccepted)
	if n, err := refuser.Drain(ctx); err != nil || n != 1 {
		t.Fatalf("drain: %d, %v", n, err)
	}
	after := e.WantState(w.ID, domain.StateAcknowledged)
	if after.PendingCancel {
		t.Error("pending_cancel is still set after the broker refused the cancel")
	}
	if n := e.CountEvents(w.ID, domain.EvCancelRejected); n != 1 {
		t.Errorf("%d CancelRejected events, want 1", n)
	}
}

// The background worker picks up new orders without being asked, and a
// worker started later finishes what an earlier one left.
func TestOutbox_BackgroundWorkerDeliversAndSurvivesRestart(t *testing.T) {
	e := harness.New(t, func(o *harness.Options) { o.RealClock = true })
	ctx, stop := context.WithCancel(context.Background())
	first := e.NewWorker("bg-1")
	done := make(chan struct{})
	go func() { first.Run(ctx); close(done) }()

	a := e.Created()
	first.Nudge()
	e.Eventually(5*time.Second, "the first order to be submitted", func() bool {
		return e.Order(a.ID).State == domain.StateSubmitting
	})

	stop() // the worker process goes away
	<-done
	b := e.Created()
	c := e.Created()
	time.Sleep(50 * time.Millisecond)
	if e.Order(b.ID).State != domain.StateValidated {
		t.Fatal("an order was submitted with no worker running")
	}

	ctx2, stop2 := context.WithCancel(context.Background())
	defer stop2()
	second := e.NewWorker("bg-2")
	go second.Run(ctx2)
	e.Eventually(5*time.Second, "the queued orders to be submitted after restart", func() bool {
		return e.Order(b.ID).State == domain.StateSubmitting && e.Order(c.ID).State == domain.StateSubmitting
	})
	if st := e.Sim.Stats(); st.SubmitRequests != 3 || st.DuplicateSubmits != 0 {
		t.Fatalf("broker stats = %+v", st)
	}
}
