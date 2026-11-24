// Package scenarios holds the failure scenarios the specification asks for:
// duplicate delivery, out-of-order and stale events, connection loss, lost
// reports, clock skew, rejected writes and missing data, plus deterministic
// replay. Each one runs the real system against the simulated broker.
package scenarios

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"reflect"
	"testing"

	"oms/src/broker"
	"oms/src/brokersim"
	"oms/src/domain"
	"oms/src/fixed"
	"oms/src/service"
	"oms/src/store"
	"oms/tests/harness"
)

func TestMain(m *testing.M) { harness.Main(m) }

// Scenario: duplicate delivery. The broker sends every webhook three times,
// across a lifecycle that includes a fill, a correction and a cancel. Every
// effect is applied exactly once.
func TestScenario_DuplicateDelivery(t *testing.T) {
	e := harness.New(t)
	e.Sim.SetFaults(brokersim.Faults{DuplicateWebhooks: 2})

	o := e.Submitted()
	e.Must(e.Sim.Ack(o.ID))
	x := e.FillHeld(o.ID, 40, "150")
	e.FillHeld(o.ID, 20, "150.50")
	if _, _, err := e.Sim.Correct(x, 30, fixed.MustParse("149.90")); err != nil {
		t.Fatal(err)
	}
	e.Flush()
	e.Cancel(o.ID).Want(http.StatusAccepted)
	e.Drain()
	e.Must(e.Sim.ConfirmCancel(o.ID))
	e.Flush()

	// 30 × 149.90 + 20 × 150.50 = 7,507.00
	e.WantState(o.ID, domain.StateCancelled)
	e.WantFilled(o.ID, 50, "7507")
	e.WantPosition(harness.Account1, "AAPL", 50, "-7507")
	want := []string{"OrderCreated", "OrderValidated", "SubmissionStarted", "OrderAcknowledged",
		"ExecutionRecorded", "ExecutionRecorded", "ExecutionCorrected", "CancelRequested", "OrderCancelled"}
	if got := e.EventTypes(o.ID); !reflect.DeepEqual(got, want) {
		t.Fatalf("history = %v", got)
	}

	inbox, err := e.App.Store.ListBrokerEvents(context.Background(), o.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(inbox) != 5 {
		t.Fatalf("%d inbox entries, want 5", len(inbox))
	}
	for _, row := range inbox {
		if row.Deliveries != 3 {
			t.Errorf("inbox %s (%s): %d deliveries, want 3", row.EventID, row.Kind, row.Deliveries)
		}
	}
	// The duplicate webhook rate is observable: 10 of 15 deliveries.
	var dup, total float64
	for _, kind := range []string{"ACK", "FILL", "CORRECT", "CANCELLED"} {
		d := e.App.Metrics.BrokerEvents.Value(kind, "BROKER", "DUPLICATE")
		dup += d
		total += d + e.App.Metrics.BrokerEvents.Value(kind, "BROKER", "APPLIED")
	}
	if dup != 10 || total != 15 {
		t.Errorf("duplicates %v of %v deliveries, want 10 of 15", dup, total)
	}
	e.WantNoOpenIssues()
}

// Scenario: out-of-order delivery. The fill overtakes the acknowledgement.
func TestScenario_FillArrivesBeforeTheAcknowledgement(t *testing.T) {
	e := harness.New(t)
	o := e.Submitted()
	e.Must(e.Sim.Ack(o.ID))
	e.FillHeld(o.ID, 40, "150")

	ds := e.Accepted(e.Sim.FlushReversed(context.Background()))
	if got := harness.Outcomes(ds); !reflect.DeepEqual(got, []string{"APPLIED", "STALE"}) {
		t.Fatalf("outcomes = %v, want the fill applied and the late ack stale", got)
	}
	after := e.WantState(o.ID, domain.StatePartiallyFilled)
	e.WantFilled(o.ID, 40, "6000")
	// The fill implied the acknowledgement and carried the broker's order id.
	if after.AcknowledgedAt == nil || after.BrokerOrderID == "" {
		t.Errorf("order after early fill = %+v", after)
	}
	if n := e.CountEvents(o.ID, domain.EvOrderAcknowledged); n != 0 {
		t.Errorf("a stale acknowledgement produced %d events", n)
	}
}

// Scenario: out-of-order delivery. The cancel confirmation overtakes a fill
// that happened before it.
func TestScenario_CancelConfirmationArrivesBeforeAnEarlierFill(t *testing.T) {
	e := harness.New(t)
	o := e.Working()
	e.Cancel(o.ID).Want(http.StatusAccepted)
	e.Drain()
	e.FillHeld(o.ID, 25, "150")
	e.Must(e.Sim.ConfirmCancel(o.ID))

	ds := e.Accepted(e.Sim.FlushReversed(context.Background()))
	if got := harness.Outcomes(ds); !reflect.DeepEqual(got, []string{"APPLIED", "APPLIED"}) {
		t.Fatalf("outcomes = %v", got)
	}
	e.WantState(o.ID, domain.StateCancelled)
	e.WantFilled(o.ID, 25, "3750")
	e.WantPosition(harness.Account1, "AAPL", 25, "-3750")
}

// Scenario: out-of-order delivery. A correction and a bust arrive before the
// executions they refer to. Totals come out the same as in order.
func TestScenario_CorrectionsArriveBeforeTheirOriginals(t *testing.T) {
	e := harness.New(t)
	o := e.Working()
	x1 := e.FillHeld(o.ID, 40, "150")
	x2 := e.FillHeld(o.ID, 30, "151")
	if _, _, err := e.Sim.Correct(x1, 35, fixed.MustParse("149")); err != nil {
		t.Fatal(err)
	}
	if _, _, err := e.Sim.Bust(x2); err != nil {
		t.Fatal(err)
	}

	ds := e.Accepted(e.Sim.FlushReversed(context.Background())) // bust, correct, fill 2, fill 1
	for i, outcome := range harness.Outcomes(ds) {
		if outcome != "APPLIED" {
			t.Fatalf("delivery %d (%s): %s", i, ds[i].Type, ds[i].Body)
		}
	}
	e.WantState(o.ID, domain.StatePartiallyFilled)
	e.WantFilled(o.ID, 35, "5215") // 35 × 149.00
	e.WantPosition(harness.Account1, "AAPL", 35, "-5215")

	// The originals are on file, marked as superseded on arrival, so the
	// audit trail is complete even though they changed nothing.
	superseded := 0
	for _, ev := range e.Events(o.ID) {
		if ev.Type != domain.EvExecutionRecorded {
			continue
		}
		var p domain.Execution
		if err := json.Unmarshal(ev.Payload, &p); err != nil {
			t.Fatal(err)
		}
		if p.Superseded && p.DeltaQty == 0 {
			superseded++
		}
	}
	if superseded != 2 || len(e.Executions(o.ID)) != 4 {
		t.Errorf("%d superseded originals among %d executions, want 2 of 4", superseded, len(e.Executions(o.ID)))
	}
	e.WantNoOpenIssues()
}

type outcome struct {
	State     domain.State
	FilledQty int64
	Notional  fixed.Micros
	Fees      fixed.Micros
	Execs     int
}

// script produces a fixed sequence of broker activity for one order: an
// acknowledgement, four fills, one correction and one bust.
func script(t *testing.T, e *harness.Env, orderID string) {
	t.Helper()
	e.Must(e.Sim.Ack(orderID))
	e.FillHeld(orderID, 100, "150")
	x2 := e.FillHeld(orderID, 200, "150.25")
	x3 := e.FillHeld(orderID, 300, "149.75")
	if _, _, err := e.Sim.Correct(x2, 150, fixed.MustParse("150.125")); err != nil {
		t.Fatal(err)
	}
	if _, _, err := e.Sim.Bust(x3); err != nil {
		t.Fatal(err)
	}
	e.FillHeld(orderID, 250, "150.50")
}

func result(e *harness.Env, orderID string) outcome {
	o := e.Order(orderID)
	return outcome{o.State, o.FilledQty, o.FilledNotional, o.Fees, len(e.Executions(orderID))}
}

// Scenario: convergence. The same seven reports, delivered in 40 different
// seeded orders with every report also delivered twice, always produce the
// same order. The seeds make any failure reproducible.
func TestScenario_AnyDeliveryOrderConverges(t *testing.T) {
	e := harness.New(t)
	ctx := context.Background()
	big := func(r *service.CreateOrderRequest) { r.Qty = 1000 }

	ref := e.Submitted(big)
	script(t, e, ref.ID)
	e.Flush()
	want := result(e, ref.ID)
	// 100 × 150 + 150 × 150.125 + 250 × 150.50 = 75,143.75
	if want.State != domain.StatePartiallyFilled || want.FilledQty != 500 || want.Notional != fixed.MustParse("75143.75") || want.Execs != 6 {
		t.Fatalf("in-order result = %+v", want)
	}

	const seeds = 40
	for seed := uint64(1); seed <= seeds; seed++ {
		o := e.Submitted(big)
		script(t, e, o.ID)
		e.Sim.SetFaults(brokersim.Faults{DuplicateWebhooks: 1})
		ds := e.Accepted(e.Sim.FlushShuffled(ctx, seed))
		e.Sim.SetFaults(brokersim.Faults{})
		if got := result(e, o.ID); got != want {
			order := make([]string, len(ds))
			for i, d := range ds {
				order[i] = fmt.Sprintf("%s=%s", d.Type, harness.Outcome(d))
			}
			t.Fatalf("seed %d: got %+v, want %+v\ndelivery order: %v\nhistory: %v", seed, got, want, order, e.EventTypes(o.ID))
		}
	}
	e.WantPosition(harness.Account1, "AAPL", 500*(seeds+1), fixed.Micros(-int64(want.Notional)*(seeds+1)).String())
	e.WantNoOpenIssues()
}

// Scenario: out-of-order delivery that cannot be applied as it arrives. A
// correction that makes room is overtaken by the fill that needs the room,
// so for a moment the fill looks like an overfill. It is quarantined, never
// dropped; reconciliation books it once the correction has landed.
func TestScenario_TransientOverfillFromReorderingIsRecoveredByReconciliation(t *testing.T) {
	e := harness.New(t)
	o := e.Working()
	x1 := e.Fill(o.ID, 80, "150")
	// At the broker: fill 1 is corrected down to 50, then 50 more fill.
	if _, _, err := e.Sim.Correct(x1, 50, fixed.MustParse("150")); err != nil {
		t.Fatal(err)
	}
	e.FillHeld(o.ID, 50, "150")

	ds := e.Accepted(e.Sim.FlushReversed(context.Background())) // the fill first, then the correction
	if got := harness.Outcomes(ds); !reflect.DeepEqual(got, []string{"QUARANTINED", "APPLIED"}) {
		t.Fatalf("outcomes = %v", got)
	}
	e.WantFilled(o.ID, 50, "7500")
	issue := e.OpenIssue(domain.ReasonOverfill)

	e.Advance(6e9)
	run := e.Reconcile()
	if run.AutoRepaired != 1 {
		t.Fatalf("run = %+v", run)
	}
	e.WantState(o.ID, domain.StateFilled)
	e.WantFilled(o.ID, 100, "15000")
	if e.Order(o.ID).OverfillAccepted {
		t.Error("the order was marked as an accepted overfill; it never exceeded its quantity")
	}

	// The quarantine issue is now moot. Accepting it is refused because the
	// execution is booked; the operator acknowledges it.
	e.Resolve(issue.ID, service.ActionAcceptExecution, "accept").WantError(http.StatusConflict, "ALREADY_BOOKED")
	e.Resolve(issue.ID, service.ActionAcknowledge, "booked by reconciliation after the correction arrived").Want(http.StatusOK)
	e.WantFilled(o.ID, 100, "15000")
	e.WantNoOpenIssues()
}

// Scenario: stale events. Reports about things the order is already past
// are recorded and change nothing.
func TestScenario_StaleEvents(t *testing.T) {
	e := harness.New(t)
	o := e.Working()
	e.Fill(o.ID, 100, "150")
	before := len(e.Events(o.ID))

	send := func(id, typ string) string {
		t.Helper()
		d := e.Send(broker.WebhookEvent{EventID: id, Type: typ, ClientOrderID: o.ID, BrokerOrderID: "B-000001", OrderQty: 100})
		if !d.OK() {
			t.Fatalf("%s: %d %s", typ, d.Status, d.Body)
		}
		return harness.Outcome(d)
	}
	for id, typ := range map[string]string{
		"s-1": broker.EventAcknowledged,   // acknowledged long ago
		"s-2": broker.EventCancelRejected, // no cancel is pending
		"s-3": broker.EventReplaceReject,  // no replace is pending
		"s-4": broker.EventReplaced,       // the order is closed
	} {
		if got := send(id, typ); got != "STALE" {
			t.Errorf("%s on a FILLED order: %s, want STALE", typ, got)
		}
	}
	// Reports that contradict a closed order are not stale, they are wrong:
	// a filled order cannot become cancelled, rejected or expired.
	for id, typ := range map[string]string{
		"c-1": broker.EventCancelled, "c-2": broker.EventRejected, "c-3": broker.EventExpired,
	} {
		if got := send(id, typ); got != "QUARANTINED" {
			t.Errorf("%s on a FILLED order: %s, want QUARANTINED", typ, got)
		}
	}
	if n := len(e.Events(o.ID)); n != before {
		t.Fatalf("stale reports added %d events", n-before)
	}
	e.WantState(o.ID, domain.StateFilled)
	if n := len(e.Issues(store.IssueOpen)); n != 3 {
		t.Errorf("%d open issues, want 3 (one per contradiction)", n)
	}

	// The same holds for a cancelled order hearing about its cancel again
	// under a new event id.
	c := e.Working()
	e.Must(e.Sim.ConfirmCancel(c.ID))
	e.Flush()
	d := e.Send(broker.WebhookEvent{EventID: "s-5", Type: broker.EventCancelled, ClientOrderID: c.ID})
	if harness.Outcome(d) != "STALE" {
		t.Errorf("second cancel report: %s", d.Body)
	}
	if n := e.CountEvents(c.ID, domain.EvOrderCancelled); n != 1 {
		t.Errorf("%d OrderCancelled events, want 1", n)
	}
}

// Scenario: missing data. Reports that lack what their type needs are
// refused at the door with 400, counted, and leave no trace on the order. A
// well-formed report afterwards is processed normally.
func TestScenario_MalformedAndIncompleteReports(t *testing.T) {
	e := harness.New(t)
	o := e.Working()
	ctx := context.Background()
	now := e.Now().Format("2006-01-02T15:04:05Z07:00")

	bodies := map[string]string{
		"not json":            `{"event_id": "m-1", `,
		"empty":               ``,
		"no event id":         fmt.Sprintf(`{"type":"order.fill","client_order_id":%q,"exec_id":"X","qty":10,"price":"150","fee":"0","event_time":%q}`, o.ID, now),
		"unknown type":        fmt.Sprintf(`{"event_id":"m-2","type":"order.exploded","client_order_id":%q,"event_time":%q}`, o.ID, now),
		"no order id":         fmt.Sprintf(`{"event_id":"m-3","type":"order.acknowledged","event_time":%q}`, now),
		"no event time":       fmt.Sprintf(`{"event_id":"m-4","type":"order.acknowledged","client_order_id":%q}`, o.ID),
		"fill without id":     fmt.Sprintf(`{"event_id":"m-5","type":"order.fill","client_order_id":%q,"qty":10,"price":"150","fee":"0","event_time":%q}`, o.ID, now),
		"fill without price":  fmt.Sprintf(`{"event_id":"m-6","type":"order.fill","client_order_id":%q,"exec_id":"X","qty":10,"fee":"0","event_time":%q}`, o.ID, now),
		"fill without fee":    fmt.Sprintf(`{"event_id":"m-7","type":"order.fill","client_order_id":%q,"exec_id":"X","qty":10,"price":"150","event_time":%q}`, o.ID, now),
		"fill of zero":        fmt.Sprintf(`{"event_id":"m-8","type":"order.fill","client_order_id":%q,"exec_id":"X","qty":0,"price":"150","fee":"0","event_time":%q}`, o.ID, now),
		"float price":         fmt.Sprintf(`{"event_id":"m-9","type":"order.fill","client_order_id":%q,"exec_id":"X","qty":10,"price":"1.5e2","fee":"0","event_time":%q}`, o.ID, now),
		"bust without ref":    fmt.Sprintf(`{"event_id":"m-10","type":"order.fill_busted","client_order_id":%q,"exec_id":"X","event_time":%q}`, o.ID, now),
		"replace without qty": fmt.Sprintf(`{"event_id":"m-11","type":"order.replaced","client_order_id":%q,"event_time":%q}`, o.ID, now),
	}
	for name, body := range bodies {
		d := e.Sim.SendRaw(ctx, []byte(body), e.WebhookSecret)
		if d.Status != http.StatusBadRequest {
			t.Errorf("%s: %d %s, want 400", name, d.Status, d.Body)
		}
	}
	if got := e.App.Metrics.WebhookRejects.Value("malformed"); got != float64(len(bodies)) {
		t.Errorf("malformed counter = %v, want %d", got, len(bodies))
	}
	if inbox, _ := e.App.Store.ListBrokerEvents(ctx, o.ID); len(inbox) != 1 {
		t.Errorf("%d inbox entries, want only the acknowledgement", len(inbox))
	}
	e.WantState(o.ID, domain.StateAcknowledged)
	e.WantFilled(o.ID, 0, "0")

	// Structurally fine but economically impossible: accepted for the
	// record, quarantined, not applied.
	fee := fixed.MustParse("-1")
	d := e.Send(broker.WebhookEvent{EventID: "m-12", Type: broker.EventFill, ClientOrderID: o.ID,
		ExecID: "X-neg", Qty: 10, Price: harness.Price("150"), Fee: &fee})
	if !d.OK() || harness.Outcome(d) != "QUARANTINED" {
		t.Fatalf("negative fee: %d %s", d.Status, d.Body)
	}
	if is := e.OpenIssue(domain.ReasonInvalidReport); is.OrderID != o.ID {
		t.Errorf("issue = %+v", is)
	}

	e.Fill(o.ID, 100, "150")
	e.WantState(o.ID, domain.StateFilled)
}
