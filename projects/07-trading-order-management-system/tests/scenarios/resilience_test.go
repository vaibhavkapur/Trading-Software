package scenarios

import (
	"context"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"oms/src/brokersim"
	"oms/src/domain"
	"oms/src/outbox"
	"oms/src/service"
	"oms/src/store"
	"oms/tests/harness"
)

// past moves the frozen clock beyond the 5s reconciliation grace window.
func past(e *harness.Env) { e.Advance(6 * time.Second) }

// Scenario: connection loss. The broker drops every connection while orders
// keep arriving. The API keeps accepting them, the outbox holds them, and
// when the broker returns each order is delivered exactly once.
func TestScenario_BrokerConnectionLoss(t *testing.T) {
	e := harness.New(t, func(o *harness.Options) { o.Outbox.MaxAttempts = 10 })
	e.Sim.SetFaults(brokersim.Faults{Down: true})

	const n = 25
	ids := make([]string, n)
	for i := range ids {
		ids[i] = e.Created().ID // accepted although the broker is unreachable
	}
	for round := 0; round < 3; round++ {
		e.Drain()
		e.Advance(10 * time.Second)
	}
	if st := e.Sim.Stats(); st.SubmitRequests != 0 {
		t.Fatalf("the broker processed %d submissions while down", st.SubmitRequests)
	}
	for _, id := range ids {
		e.WantState(id, domain.StateSubmitting)
	}
	if text := e.Metric(); !strings.Contains(text, "oms_outbox_pending 25") {
		t.Error("the outbox backlog is not visible in the metrics")
	}
	// Reconciliation cannot reach the broker either: the run fails and
	// judges nothing.
	if run, err := e.App.Reconciler.Run(context.Background(), "test"); err == nil || run.Status != store.RunFailed {
		t.Fatalf("reconciliation during the outage: %+v, %v", run, err)
	}
	e.WantNoOpenIssues()

	e.Sim.SetFaults(brokersim.Faults{})
	e.Advance(10 * time.Second)
	e.Drain()
	st := e.Sim.Stats()
	if st.SubmitRequests != n || st.DuplicateSubmits != 0 {
		t.Fatalf("after recovery the broker saw %+v, want %d submissions, none duplicated", st, n)
	}
	if text := e.Metric(); !strings.Contains(text, "oms_outbox_pending 0") {
		t.Error("the outbox did not drain")
	}
	for _, id := range ids {
		e.Must(e.Sim.Ack(id))
	}
	e.Flush()
	for _, id := range ids {
		e.WantState(id, domain.StateAcknowledged)
	}
	past(e)
	if run := e.Reconcile(); run.DriftFound != 0 {
		t.Fatalf("reconciliation after recovery = %+v", run)
	}
}

// Scenario: connection loss with rolling worker restarts. A flaky broker
// (failed requests, lost responses) and workers that are stopped and
// replaced mid-flow still deliver every order, with no duplicates created
// at the broker.
func TestScenario_FlakyBrokerAndWorkerRestarts(t *testing.T) {
	e := harness.New(t, func(o *harness.Options) {
		o.RealClock = true
		o.Outbox = outbox.Config{MaxAttempts: 50, BaseBackoff: 5 * time.Millisecond, MaxBackoff: 40 * time.Millisecond,
			PollInterval: 5 * time.Millisecond, Batch: 8, Concurrency: 4}
	})
	e.Sim.SetFaults(brokersim.Faults{FailRequests: 30, LoseResponses: 30})

	const n = 80
	ids := make([]string, n)
	for i := range ids {
		ids[i] = e.Created().ID
	}

	allSubmitted := func() bool {
		for _, id := range ids {
			if row := e.Outbox(id); row[0].Status != store.OutboxDelivered {
				return false
			}
		}
		return true
	}
	deadline := time.Now().Add(30 * time.Second)
	for gen := 0; !allSubmitted(); gen++ {
		if time.Now().After(deadline) {
			t.Fatal("the outbox did not drain")
		}
		// Two workers at a time, each generation stopped after 30ms.
		ctx, stop := context.WithTimeout(context.Background(), 30*time.Millisecond)
		var wg sync.WaitGroup
		for w := 0; w < 2; w++ {
			worker := e.NewWorker("gen-" + string(rune('a'+gen%26)) + "-" + string(rune('0'+w)))
			wg.Add(1)
			go func() { defer wg.Done(); worker.Run(ctx) }()
		}
		wg.Wait()
		stop()
	}

	st := e.Sim.Stats()
	if got := len(e.Sim.Snapshot().Orders); got != n {
		t.Fatalf("the broker has %d orders, want %d", got, n)
	}
	if st.SubmitRequests-st.DuplicateSubmits != n {
		t.Fatalf("broker stats = %+v: %d distinct submissions, want %d", st, st.SubmitRequests-st.DuplicateSubmits, n)
	}
	if st.DuplicateSubmits == 0 {
		t.Error("the lost responses caused no resends; the scenario did not exercise the retry path")
	}
	for _, id := range ids {
		e.WantState(id, domain.StateSubmitting)
		if n := e.CountEvents(id, domain.EvSubmissionStarted); n != 1 {
			t.Fatalf("order %s has %d SubmissionStarted events", id, n)
		}
	}
	e.WantNoOpenIssues()
}

// Scenario: lost reports. The broker's webhooks vanish for a while (every
// kind of report). Reconciliation alone brings the OMS back in line.
func TestScenario_LostWebhooksAreRecoveredByReconciliation(t *testing.T) {
	e := harness.New(t)
	a := e.Working()
	b := e.Working(func(r *service.CreateOrderRequest) {
		r.Side = domain.Sell
		r.Symbol = "MSFT"
		r.LimitPrice = harness.Price("400")
	})
	c := e.Submitted()
	e.Cancel(b.ID).Want(http.StatusAccepted)
	e.Drain()

	e.Sim.SetFaults(brokersim.Faults{DropWebhooks: true})
	e.FillHeld(a.ID, 60, "150")
	e.FillHeld(a.ID, 40, "149.90")
	e.FillHeld(b.ID, 30, "400.10")
	e.Must(e.Sim.ConfirmCancel(b.ID))
	e.Must(e.Sim.Ack(c.ID))
	e.Must(e.Sim.Expire(c.ID, "session closed"))
	e.Sim.SetFaults(brokersim.Faults{})
	if lost := e.Sim.Stats().WebhooksLost; lost != 6 {
		t.Fatalf("%d webhooks lost, want 6", lost)
	}
	past(e)

	run := e.Reconcile()
	if run.AutoRepaired != 5 || run.IssuesOpened != 0 {
		t.Fatalf("run = %+v, want 5 repairs (3 executions, 1 cancel, 1 expiry)", run)
	}
	e.WantState(a.ID, domain.StateFilled)
	e.WantFilled(a.ID, 100, "14996")
	e.WantState(b.ID, domain.StateCancelled)
	e.WantFilled(b.ID, 30, "12003")
	e.WantState(c.ID, domain.StateExpired)
	e.WantPosition(harness.Account1, "AAPL", 100, "-14996")
	e.WantPosition(harness.Account1, "MSFT", -30, "12003")

	if again := e.Reconcile(); again.DriftFound != 0 {
		t.Fatalf("second run = %+v", again)
	}
	if got := e.App.Metrics.ReconRepairs.Value(store.KindMissingExecution); got != 3 {
		t.Errorf("repair counter = %v", got)
	}
	e.WantNoOpenIssues()
}

// Scenario: clock skew. The broker's clock is ten minutes off in either
// direction. Processing order, deduplication and the reconciliation grace
// window do not depend on it, and both times are kept.
func TestScenario_ClockSkew(t *testing.T) {
	for name, skew := range map[string]time.Duration{"broker ahead": 10 * time.Minute, "broker behind": -10 * time.Minute} {
		t.Run(name, func(t *testing.T) {
			e := harness.New(t)
			e.Sim.SetFaults(brokersim.Faults{ClockSkew: skew})
			o := e.Working()
			e.Fill(o.ID, 40, "150")
			e.WantState(o.ID, domain.StatePartiallyFilled)

			// Event time is the broker's; receive time is ours; neither is
			// altered to agree with the other.
			evs := e.Events(o.ID)
			fill := evs[len(evs)-1]
			if got := fill.OccurredAt.Sub(fill.RecordedAt); got != skew {
				t.Errorf("event time − receive time = %s, want %s", got, skew)
			}
			if !fill.RecordedAt.Equal(harness.Start) {
				t.Errorf("recorded_at = %s, want the local clock %s", fill.RecordedAt, harness.Start)
			}
			x := e.Executions(o.ID)[0]
			if !x.ExecutedAt.Equal(harness.Start.Add(skew)) || !x.RecordedAt.Equal(harness.Start) {
				t.Errorf("execution times = %s / %s", x.ExecutedAt, x.RecordedAt)
			}
			// History order is arrival order, whatever the timestamps say.
			for i := 1; i < len(evs); i++ {
				if evs[i].Seq <= evs[i-1].Seq || evs[i].Version != evs[i-1].Version+1 {
					t.Fatalf("history is not in arrival order at %d", i)
				}
			}
			future := e.App.Metrics.FutureTimestamps.Value()
			if skew > 0 && future != 2 || skew < 0 && future != 0 {
				t.Errorf("future-timestamp counter = %v", future)
			}

			// A fill whose webhook is lost, seconds ago on the broker's own
			// clock. Judged on the local clock it would look ten minutes old
			// (or ten minutes in the future); reconciliation ages broker
			// records on the broker's clock, so it is still inside the grace
			// window.
			e.Sim.SetFaults(brokersim.Faults{ClockSkew: skew, DropWebhooks: true})
			e.FillHeld(o.ID, 60, "150")
			e.Advance(2 * time.Second)
			if run := e.Reconcile(); run.AutoRepaired != 0 || run.DriftFound != 0 {
				t.Fatalf("reconciliation acted on activity inside the grace window: %+v", run)
			}
			e.Advance(4 * time.Second)
			if run := e.Reconcile(); run.AutoRepaired != 1 {
				t.Fatalf("reconciliation after the grace window: %+v", run)
			}
			e.WantState(o.ID, domain.StateFilled)
			e.WantFilled(o.ID, 100, "15000")
			e.WantNoOpenIssues()
		})
	}
}

// Scenario: rejected writes. The database refuses writes for a while (here:
// the orders table is locked, so every write times out). Clients and the
// broker are told to retry, nothing is half-written, and the retries apply
// exactly once.
func TestScenario_RejectedWrites(t *testing.T) {
	e := harness.New(t, func(o *harness.Options) { o.Store.LockTimeout = 150 * time.Millisecond })
	ctx := context.Background()
	o := e.Working()
	e.FillHeld(o.ID, 40, "150")

	blocker, err := e.App.Store.Pool().Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer blocker.Rollback(ctx)
	if _, err := blocker.Exec(ctx, `LOCK TABLE orders IN ACCESS EXCLUSIVE MODE`); err != nil {
		t.Fatal(err)
	}

	// A new order: refused with 503 and a Retry-After, and not stored.
	req := harness.OrderReq(func(r *service.CreateOrderRequest) { r.Symbol = "MSFT"; r.LimitPrice = harness.Price("400") })
	refused := e.Create(req, "during-outage").WantError(http.StatusServiceUnavailable, "UNAVAILABLE")
	if refused.Header.Get("Retry-After") == "" {
		t.Error("503 without Retry-After")
	}
	// The broker's fill report: refused with 503, so the broker keeps it.
	ds := e.Sim.Flush(ctx)
	if len(ds) != 1 || ds[0].Status != http.StatusServiceUnavailable {
		t.Fatalf("webhook during the outage: %+v", ds)
	}
	if n := len(e.Sim.Pending()); n != 1 {
		t.Fatalf("the broker holds %d webhooks for retry, want 1", n)
	}
	// A cancel: refused as well.
	e.Cancel(o.ID).WantError(http.StatusServiceUnavailable, "UNAVAILABLE")

	if err := blocker.Rollback(ctx); err != nil {
		t.Fatal(err)
	}

	// Nothing was half-written while writes were refused.
	var orders, keys, inbox int
	pool := e.App.Store.Pool()
	pool.QueryRow(ctx, `SELECT count(*) FROM orders`).Scan(&orders)
	pool.QueryRow(ctx, `SELECT count(*) FROM idempotency_keys WHERE key = 'during-outage'`).Scan(&keys)
	pool.QueryRow(ctx, `SELECT count(*) FROM broker_events WHERE kind = 'FILL'`).Scan(&inbox)
	if orders != 1 || keys != 0 || inbox != 0 {
		t.Fatalf("after the outage: %d orders, %d idempotency keys, %d fill inbox rows; want 1, 0, 0", orders, keys, inbox)
	}
	e.WantFilled(o.ID, 0, "0")
	if e.Order(o.ID).PendingCancel {
		t.Fatal("the refused cancel left a pending flag")
	}

	// The retries succeed, once each.
	created := e.Create(req, "during-outage").Want(http.StatusCreated)
	if created.Header.Get("Idempotent-Replayed") != "" {
		t.Error("the retry was answered as a replay although the first attempt stored nothing")
	}
	e.Create(req, "during-outage").Want(http.StatusCreated)
	pool.QueryRow(ctx, `SELECT count(*) FROM orders`).Scan(&orders)
	if orders != 2 {
		t.Fatalf("%d orders after the retries, want 2", orders)
	}
	if got := harness.Outcomes(e.Flush()); len(got) != 1 || got[0] != "APPLIED" {
		t.Fatalf("webhook retry outcomes = %v", got)
	}
	e.WantFilled(o.ID, 40, "6000")
	e.WantPosition(harness.Account1, "AAPL", 40, "-6000")
}

// Scenario: the outbox worker cannot write (same lock) while it holds a
// message. It gives the message back for a retry rather than losing it or
// sending it without recording the intent.
func TestScenario_RejectedWritesInTheDispatcher(t *testing.T) {
	e := harness.New(t, func(o *harness.Options) { o.Store.LockTimeout = 150 * time.Millisecond })
	ctx := context.Background()
	o := e.Created()

	blocker, err := e.App.Store.Pool().Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer blocker.Rollback(ctx)
	if _, err := blocker.Exec(ctx, `LOCK TABLE orders IN ACCESS EXCLUSIVE MODE`); err != nil {
		t.Fatal(err)
	}
	e.App.Worker.RunOnce(ctx)
	if st := e.Sim.Stats(); st.SubmitRequests != 0 {
		t.Fatal("the order was sent although SUBMITTING could not be recorded")
	}
	blocker.Rollback(ctx)

	e.WantState(o.ID, domain.StateValidated)
	e.Advance(time.Second)
	e.Drain()
	e.WantState(o.ID, domain.StateSubmitting)
	if st := e.Sim.Stats(); st.SubmitRequests != 1 {
		t.Fatalf("broker saw %d submissions, want 1", st.SubmitRequests)
	}
}
