// Command demo walks through the system end to end against the simulated
// broker, including the failure cases, and checks every claim it prints. It
// exits non-zero if any check fails, so it doubles as a smoke test.
//
// SIMULATED / PAPER ONLY: no real venue, no real money. Everything runs in
// this process: a throwaway PostgreSQL (or a scratch database on the server at
// OMS_DEMO_DATABASE_URL), the simulated broker and the OMS, talking to each
// other over loopback HTTP.
//
// The demo runs on a simulated clock and with sequential ids so that its
// output is the same on every run. Latencies are therefore not meaningful
// here; `make bench` measures those on the real clock.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"oms/src/broker"
	"oms/src/brokersim"
	"oms/src/clock"
	"oms/src/domain"
	"oms/src/fixed"
	"oms/src/ids"
	"oms/src/localstack"
	"oms/src/outbox"
	"oms/src/reconcile"
	"oms/src/service"
	"oms/src/store"
)

type demo struct {
	ctx    context.Context
	s      *localstack.Stack
	clock  *clock.Fake
	checks int
}

func main() {
	ctx := context.Background()
	clk := clock.NewFake(time.Date(2026, 1, 5, 14, 30, 0, 0, time.UTC))
	outboxCfg := outbox.Config{
		WorkerID: "worker-1", MaxAttempts: 4, BaseBackoff: time.Second, MaxBackoff: 8 * time.Second,
		Lease: 30 * time.Second, Jitter: func() float64 { return 1 },
	}

	fmt.Println("Trading OMS demo: SIMULATED broker, PAPER orders only. No real venue is contacted.")
	fmt.Println("Starting PostgreSQL, the simulated broker and the OMS in this process...")
	s, err := localstack.Start(ctx, localstack.Options{
		DatabaseURL: os.Getenv("OMS_DEMO_DATABASE_URL"),
		Clock:       clk,
		IDs:         &ids.Sequence{},
		Outbox:      outboxCfg,
		Reconcile:   reconcile.Config{Grace: 5 * time.Second},
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, "demo:", err)
		os.Exit(1)
	}
	d := &demo{ctx: ctx, s: s, clock: clk}

	code := 0
	func() {
		defer func() {
			if r := recover(); r != nil {
				if _, failed := r.(checkFailed); !failed {
					panic(r)
				}
				code = 1
			}
		}()
		d.lifecycle()
		d.duplicates()
		d.overfill()
		nvda, execID := d.outageAndRestart(outboxCfg)
		d.reconciliation(nvda, execID)
		d.corrections(nvda, execID)
		d.cancel(nvda)
		d.wrapUp()
	}()
	s.Close()
	if code != 0 {
		fmt.Println("\nDEMO FAILED")
		os.Exit(code)
	}
	fmt.Printf("\nDemo complete: %d checks passed.\n", d.checks)
}

// --- scenes ---

// lifecycle: an idempotent create, then the order's whole life.
func (d *demo) lifecycle() {
	d.scene("1. Idempotent order entry and the order lifecycle")

	req := service.CreateOrderRequest{AccountID: "ACC-1", ClientOrderID: "demo-aapl-1", Symbol: "AAPL",
		Side: domain.Buy, Type: domain.Limit, TimeInForce: domain.Day, Qty: 100, LimitPrice: price("150.00")}
	d.step("alice: POST /v1/orders  (Idempotency-Key: demo-key-1)  BUY 100 AAPL @ 150.00")
	first := d.call(d.s.Trader, "POST", "/v1/orders", req, "Idempotency-Key", "demo-key-1")
	o := first.order()
	d.check(first.status == 201 && o.State == domain.StateValidated, "201 Created; order %s is %s; the submission is queued in the outbox in the same transaction", o.ID, o.State)

	d.step("alice: the same request again (a client retry after a timeout)")
	again := d.call(d.s.Trader, "POST", "/v1/orders", req, "Idempotency-Key", "demo-key-1")
	d.check(again.status == 201 && bytes.Equal(again.body, first.body) && again.header.Get("Idempotent-Replayed") == "true",
		"the stored response is replayed byte for byte (Idempotent-Replayed: true)")
	var list struct{ Orders []service.OrderView }
	d.call(d.s.Trader, "GET", "/v1/orders", nil).decode(&list)
	d.check(len(list.Orders) == 1, "there is still exactly one order")

	d.step("alice: the same key with a different body (a client bug)")
	other := req
	other.Qty = 500
	reused := d.call(d.s.Trader, "POST", "/v1/orders", other, "Idempotency-Key", "demo-key-1")
	d.check(reused.status == 422 && reused.errorCode() == "IDEMPOTENCY_KEY_REUSED", "refused: %d %s", reused.status, reused.errorCode())

	d.step("vera (viewer): tries to place an order")
	denied := d.call(d.s.Viewer, "POST", "/v1/orders", req, "Idempotency-Key", "vera-key-1")
	d.check(denied.status == 403, "refused: %d %s", denied.status, denied.errorCode())

	d.step("the outbox worker delivers the submission to the broker")
	d.drain(d.s.App.Worker)
	d.wantState(o.ID, domain.StateSubmitting)

	d.step("broker: acknowledges, fills 40 @ 149.90, then fills 60 @ 150.00 (signed webhooks)")
	d.must(d.s.Sim.Ack(o.ID))
	d.flush()
	d.wantState(o.ID, domain.StateAcknowledged)
	d.fill(o.ID, 40, "149.90")
	d.flush()
	d.wantState(o.ID, domain.StatePartiallyFilled)
	d.fill(o.ID, 60, "150.00")
	d.flush()
	done := d.wantState(o.ID, domain.StateFilled)
	d.check(done.FilledQty == 100 && done.FilledNotional == fixed.MustParse("14996"),
		"filled 100 for %s (40 x 149.90 + 60 x 150.00), fees %s, exact integer micros", done.FilledNotional, done.Fees)

	d.step("GET /v1/orders/%s/events: the order's history, which is the source of truth", o.ID)
	d.printEvents(o.ID)
	d.wantPosition("ACC-1", "AAPL", 100, "-14996.50")
}

// duplicates: at-least-once and out-of-order delivery from the broker.
func (d *demo) duplicates() {
	d.scene("2. Duplicate and out-of-order broker reports")

	o := d.newOrder("demo-key-2", "MSFT", 100, "400.00")
	d.drain(d.s.App.Worker)
	d.step("broker: acknowledges and fills 100 @ 400.00, but the fill report arrives before the acknowledgement")
	d.must(d.s.Sim.Ack(o.ID))
	execID := d.fill(o.ID, 100, "400.00")
	outcomes := d.outcomes(d.s.Sim.FlushReversed(d.ctx))
	d.check(fmt.Sprint(outcomes) == "[APPLIED STALE]", "outcomes %v: the fill implied the acknowledgement; the late acknowledgement is recorded as stale", outcomes)
	d.wantState(o.ID, domain.StateFilled)

	d.step("broker: redelivers every webhook it ever sent (at-least-once delivery)")
	outcomes = d.outcomes(d.s.Sim.RedeliverAll(d.ctx))
	allDup := len(outcomes) > 0
	for _, oc := range outcomes {
		allDup = allDup && oc == "DUPLICATE"
	}
	d.check(allDup, "%d redeliveries, every one answered DUPLICATE", len(outcomes))

	d.step("broker: reports the same execution %s again under a new event id", execID)
	dup := d.send(broker.WebhookEvent{EventID: "evt-demo-resend", Type: broker.EventFill, ClientOrderID: o.ID,
		ExecID: execID, Qty: 100, Price: price("400.00"), Fee: price("0.50")})
	d.check(dup == "DUPLICATE", "outcome %s: executions are deduplicated by the broker's execution id, not only by event id", dup)

	after := d.order(o.ID)
	d.check(after.FilledQty == 100 && after.FilledNotional == fixed.MustParse("40000"), "filled quantity is still 100 and notional still %s", after.FilledNotional)
	d.wantPosition("ACC-1", "MSFT", 100, "-40000.50")
}

// overfill: a fill that would exceed the order is never booked silently.
func (d *demo) overfill() {
	d.scene("3. An impossible fill is quarantined, not booked")

	o := d.newOrder("demo-key-3", "TSLA", 100, "250.00")
	d.drain(d.s.App.Worker)
	d.must(d.s.Sim.Ack(o.ID))
	d.fill(o.ID, 100, "250.00")
	d.flush()
	d.wantState(o.ID, domain.StateFilled)

	d.step("broker: reports a further fill of 50 on the fully filled order")
	oc := d.send(broker.WebhookEvent{EventID: "evt-demo-overfill", Type: broker.EventFill, ClientOrderID: o.ID,
		ExecID: "X-DEMO-OVERFILL", Qty: 50, Price: price("250.00"), Fee: price("0.25")})
	after := d.order(o.ID)
	d.check(oc == "QUARANTINED" && after.FilledQty == 100, "outcome %s; filled quantity stays 100 of 100", oc)
	issue := d.openIssue("OVERFILL")
	d.check(issue.Severity == store.SeverityCritical, "issue %s opened: %s, severity %s", issue.ID, issue.Kind, issue.Severity)
	d.wantPosition("ACC-1", "TSLA", 100, "-25000.50")

	d.step("alice (trader): tries to close the issue")
	body := map[string]string{"action": service.ActionAcknowledge, "note": "broker confirmed the report was sent in error"}
	denied := d.call(d.s.Trader, "POST", "/v1/reconciliation/issues/"+issue.ID+"/resolve", body)
	d.check(denied.status == 403, "refused: %d %s (repairs need the operator role)", denied.status, denied.errorCode())

	d.step("olivia (operator): acknowledges it with a note, after checking with the broker")
	resolved := d.call(d.s.Operator, "POST", "/v1/reconciliation/issues/"+issue.ID+"/resolve", body)
	var closed store.Issue
	resolved.decode(&closed)
	d.check(resolved.status == 200 && closed.Status == store.IssueResolved && closed.ResolvedBy == "olivia",
		"issue %s by %s; the execution was never booked", closed.Status, closed.ResolvedBy)
}

// outageAndRestart: the failure demo. The broker goes away, a response is
// lost and a worker dies holding a lease; the order still reaches the broker
// exactly once.
func (d *demo) outageAndRestart(cfg outbox.Config) (service.OrderView, string) {
	d.scene("4. Failure demo: broker outage, a lost response and a worker crash")

	d.step("broker: goes down (connections are dropped)")
	d.s.Sim.SetFaults(brokersim.Faults{Down: true})
	o := d.newOrder("demo-key-4", "NVDA", 100, "900.00")
	d.drain(d.s.App.Worker)
	row := d.outboxRow(o.ID)
	d.check(row.Status == store.OutboxPending && row.Attempts == 1, "the order is accepted and durable; delivery attempt 1 failed and is scheduled for retry")
	d.wantState(o.ID, domain.StateSubmitting)

	d.step("broker: comes back, accepts the retry, but the response is lost on the way")
	d.s.Sim.SetFaults(brokersim.Faults{LoseResponses: 1})
	d.advance(time.Second)
	d.drain(d.s.App.Worker)
	row = d.outboxRow(o.ID)
	_, atBroker := d.s.Sim.Order(o.ID)
	d.check(atBroker && row.Status == store.OutboxPending && row.Attempts == 2,
		"the broker has the order, but the OMS cannot know that: attempt 2 is recorded as failed")

	d.step("worker-1 leases the message for attempt 3 and is killed before it can send")
	d.advance(2 * time.Second)
	claimed, err := d.s.App.Store.ClaimOutbox(d.ctx, "worker-1", 10, cfg.Lease)
	d.check(err == nil && len(claimed) == 1, "worker-1 holds a %s lease on the message and is gone", cfg.Lease)

	d.step("worker-2 starts (a restart)")
	cfg.WorkerID = "worker-2"
	worker2 := outbox.New(cfg, d.s.App.Store, d.s.Broker, &ids.Sequence{}, d.s.App.Metrics, d.s.App.Log)
	n := d.drainQuiet(worker2)
	d.check(n == 0, "worker-2 finds nothing to do: the dead worker's lease is still respected")
	d.advance(cfg.Lease + time.Second)
	n = d.drainQuiet(worker2)
	row = d.outboxRow(o.ID)
	d.check(n == 1 && row.Status == store.OutboxDelivered, "after the lease expires worker-2 takes over and delivers the submission")

	stats := d.s.Sim.Stats()
	orders := 0
	for _, bo := range d.s.Sim.Snapshot().Orders {
		if bo.ClientOrderID == o.ID {
			orders++
		}
	}
	d.check(stats.DuplicateSubmits == 1 && orders == 1,
		"the broker saw the order twice and deduplicated it by client order id: exactly one order at the broker")

	d.must(d.s.Sim.Ack(o.ID))
	d.flush()
	d.wantState(o.ID, domain.StateAcknowledged)

	d.step("broker: fills 70 @ 900.00 but its webhooks are lost")
	d.s.Sim.SetFaults(brokersim.Faults{DropWebhooks: true})
	execID := d.fill(o.ID, 70, "900.00")
	d.s.Sim.SetFaults(brokersim.Faults{})
	d.flush()
	stale := d.order(o.ID)
	d.check(stale.State == domain.StateAcknowledged && stale.FilledQty == 0, "the OMS has not heard: %s, filled 0 (the broker has 70)", stale.State)
	return o, execID
}

// reconciliation: drift is found, repaired from the broker's books where
// that is safe, and escalated where it is not.
func (d *demo) reconciliation(o service.OrderView, execID string) {
	d.scene("5. Reconciliation finds and repairs the drift")

	d.step("alice (trader): tries to start a reconciliation run")
	denied := d.call(d.s.Trader, "POST", "/v1/reconciliation/runs", nil)
	d.check(denied.status == 403, "refused: %d %s", denied.status, denied.errorCode())

	d.advance(10 * time.Second)
	d.step("olivia (operator): POST /v1/reconciliation/runs")
	run := d.reconcile()
	d.check(run.DriftFound == 1 && run.AutoRepaired == 1 && run.IssuesOpened == 0,
		"compared %d orders and %d executions with the broker snapshot: drift %d, repaired automatically %d",
		run.OrdersChecked, run.ExecutionsChecked, run.DriftFound, run.AutoRepaired)
	repaired := d.wantState(o.ID, domain.StatePartiallyFilled)
	d.check(repaired.FilledQty == 70, "the missing execution %s was booked through the normal ingest path: filled 70", execID)
	var recorded domain.Event
	for _, ev := range d.events(o.ID) {
		if ev.Type == domain.EvExecutionRecorded {
			recorded = ev
		}
	}
	d.check(recorded.Source == domain.SourceReconciler, "its event is attributed to source %s, causation %s", recorded.Source, recorded.CausationID)

	d.step("the broker's books now show that execution at 899.00; the OMS booked it at 900.00")
	d.must2(d.s.Sim.TamperExecution(execID, fixed.MustParse("899.00")))
	d.advance(10 * time.Second)
	run = d.reconcile()
	issue := d.openIssue(store.KindExecutionMismatch)
	x := d.executions(o.ID)[0]
	d.check(run.IssuesOpened == 1 && x.Price == fixed.MustParse("900"),
		"issue %s opened (%s, %s); the booked trade is NOT rewritten to match: still %s", issue.ID, issue.Kind, issue.Severity, x.Price)

	d.step("the broker corrects its own books; the next run sees agreement")
	d.must2(d.s.Sim.TamperExecution(execID, fixed.MustParse("900.00")))
	d.advance(10 * time.Second)
	run = d.reconcile()
	var open struct{ Issues []store.Issue }
	d.call(d.s.Operator, "GET", "/v1/reconciliation/issues?status=OPEN", nil).decode(&open)
	d.check(run.IssuesCleared == 1 && len(open.Issues) == 0, "the issue is closed automatically; the exception queue is empty")
}

// corrections: the only way an order moves backwards.
func (d *demo) corrections(o service.OrderView, execID string) {
	d.scene("6. Broker corrections are the only way back")

	d.step("broker: corrects execution %s from 70 @ 900.00 to 60 @ 899.50", execID)
	corrID, _, err := d.s.Sim.Correct(execID, 60, fixed.MustParse("899.50"))
	d.must2(err)
	d.flush()
	after := d.wantState(o.ID, domain.StatePartiallyFilled)
	d.check(after.FilledQty == 60 && after.FilledNotional == fixed.MustParse("53970"), "filled 60 for %s", after.FilledNotional)

	d.step("broker: busts the corrected execution entirely")
	_, _, err = d.s.Sim.Bust(corrID)
	d.must2(err)
	d.flush()
	back := d.wantState(o.ID, domain.StateAcknowledged)
	d.check(back.FilledQty == 0 && back.FilledNotional == 0 && back.Fees == 0,
		"PARTIALLY_FILLED -> ACKNOWLEDGED, through an ExecutionBusted event; nothing was edited or deleted")
	d.printExecutions(o.ID)
	d.wantPosition("ACC-1", "NVDA", 0, "0.00")
}

func (d *demo) cancel(o service.OrderView) {
	d.scene("7. Cancel")

	d.step("alice: POST /v1/orders/%s/cancel", o.ID)
	resp := d.call(d.s.Trader, "POST", "/v1/orders/"+o.ID+"/cancel", nil)
	pending := resp.order()
	d.check(resp.status == 202 && pending.PendingCancel && pending.State == domain.StateAcknowledged,
		"202 Accepted: the cancel is a request; the order stays %s until the broker confirms", pending.State)
	again := d.call(d.s.Trader, "POST", "/v1/orders/"+o.ID+"/cancel", nil)
	d.drain(d.s.App.Worker)
	d.check(again.status < 300 && d.s.Sim.Stats().CancelRequests == 1, "a repeated cancel does not send a second request to the broker")

	d.step("broker: confirms the cancel")
	d.must(d.s.Sim.ConfirmCancel(o.ID))
	d.flush()
	d.wantState(o.ID, domain.StateCancelled)
}

func (d *demo) wrapUp() {
	d.scene("8. The books still add up")

	d.step("replay every order's event log and compare it with the orders, executions and positions tables")
	rep, err := d.s.App.Store.Verify(d.ctx)
	d.must2(err)
	d.check(rep.OK(), "%d orders, %d events, %d execution records, %d positions: every projection matches the log",
		rep.Orders, rep.Events, rep.Executions, rep.Positions)

	d.step("final reconciliation against the broker")
	d.advance(10 * time.Second)
	run := d.reconcile()
	d.check(run.DriftFound == 0, "no drift across %d orders and %d executions", run.OrdersChecked, run.ExecutionsChecked)

	d.step("GET /metrics (excerpt; counters only, since the demo clock is simulated)")
	metrics := d.call("", "GET", "/metrics", nil)
	for _, line := range strings.Split(string(metrics.body), "\n") {
		for _, name := range []string{"oms_idempotent_replays_total", "oms_outbox_dispatch_total", "oms_broker_events_total",
			"oms_reconciliation_drift_total", "oms_reconciliation_auto_repaired_total", "oms_outbox_pending", "oms_reconciliation_open_issues"} {
			if strings.HasPrefix(line, name+"{") || strings.HasPrefix(line, name+" ") {
				fmt.Println("      " + line)
			}
		}
	}
}

// --- helpers ---

type checkFailed struct{}

func (d *demo) scene(title string) { fmt.Printf("\n=== %s ===\n", title) }

func (d *demo) step(format string, args ...any) { fmt.Printf("\n  > "+format+"\n", args...) }

// check prints a claim and stops the demo if it is not true.
func (d *demo) check(ok bool, format string, args ...any) {
	if !ok {
		fmt.Printf("    FAILED  "+format+"\n", args...)
		panic(checkFailed{})
	}
	d.checks++
	fmt.Printf("    ok  "+format+"\n", args...)
}

func (d *demo) must(_ brokersim.Event, err error) { d.must2(err) }

func (d *demo) must2(err error) {
	if err != nil {
		fmt.Println("    FAILED ", err)
		panic(checkFailed{})
	}
}

func price(s string) *fixed.Micros {
	p := fixed.MustParse(s)
	return &p
}

func (d *demo) advance(by time.Duration) {
	d.clock.Advance(by)
	fmt.Printf("    (simulated clock +%s)\n", by)
}

type response struct {
	status int
	header http.Header
	body   []byte
	d      *demo
}

func (r response) decode(v any) {
	if err := json.Unmarshal(r.body, v); err != nil {
		r.d.must2(fmt.Errorf("decode %q: %w", r.body, err))
	}
}

func (r response) order() service.OrderView {
	var o service.OrderView
	r.decode(&o)
	return o
}

func (r response) errorCode() string {
	var body struct {
		Error struct{ Code string }
	}
	json.Unmarshal(r.body, &body)
	return body.Error.Code
}

// call makes a real HTTP request to the OMS API.
func (d *demo) call(token, method, path string, body any, headers ...string) response {
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(d.ctx, method, d.s.URL+path, rd)
	d.must2(err)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	for i := 0; i+1 < len(headers); i += 2 {
		req.Header.Set(headers[i], headers[i+1])
	}
	resp, err := http.DefaultClient.Do(req)
	d.must2(err)
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	d.must2(err)
	return response{status: resp.StatusCode, header: resp.Header, body: bytes.TrimRight(data, "\n"), d: d}
}

func (d *demo) newOrder(key, symbol string, qty int64, limit string) service.OrderView {
	d.step("alice: POST /v1/orders  (Idempotency-Key: %s)  BUY %d %s @ %s", key, qty, symbol, limit)
	resp := d.call(d.s.Trader, "POST", "/v1/orders", service.CreateOrderRequest{AccountID: "ACC-1", Symbol: symbol,
		Side: domain.Buy, Type: domain.Limit, TimeInForce: domain.Day, Qty: qty, LimitPrice: price(limit)}, "Idempotency-Key", key)
	o := resp.order()
	d.check(resp.status == 201 && o.State == domain.StateValidated, "order %s is %s", o.ID, o.State)
	return o
}

func (d *demo) order(id string) service.OrderView {
	resp := d.call(d.s.Trader, "GET", "/v1/orders/"+id, nil)
	if resp.status != 200 {
		d.must2(fmt.Errorf("GET order %s: %d %s", id, resp.status, resp.body))
	}
	return resp.order()
}

func (d *demo) wantState(id string, want domain.State) service.OrderView {
	o := d.order(id)
	d.check(o.State == want, "order %s is %s", id, o.State)
	return o
}

func (d *demo) events(id string) []domain.Event {
	var out struct{ Events []domain.Event }
	d.call(d.s.Trader, "GET", "/v1/orders/"+id+"/events", nil).decode(&out)
	return out.Events
}

func (d *demo) printEvents(id string) {
	for _, ev := range d.events(id) {
		fmt.Printf("      v%-2d %-18s -> %-16s source=%-7s cause=%s\n", ev.Version, ev.Type, ev.StateAfter, ev.Source, ev.CausationID)
	}
}

func (d *demo) executions(id string) []store.ExecutionRow {
	var out struct{ Executions []store.ExecutionRow }
	d.call(d.s.Trader, "GET", "/v1/orders/"+id+"/executions", nil).decode(&out)
	return out.Executions
}

func (d *demo) printExecutions(id string) {
	fmt.Println("      execution records (append-only):")
	for _, x := range d.executions(id) {
		ref := ""
		if x.RefBrokerExecID != "" {
			ref = " ref=" + x.RefBrokerExecID
		}
		fmt.Printf("      %-8s %-8s qty=%-3d price=%-7s delta_qty=%+-4d delta_notional=%s%s\n",
			x.BrokerExecID, x.Kind, x.Qty, x.Price, x.DeltaQty, x.DeltaNotional, ref)
	}
}

func (d *demo) wantPosition(account, symbol string, qty int64, cash string) {
	var out struct{ Positions []store.PositionRow }
	d.call(d.s.Trader, "GET", "/v1/positions?account_id="+account, nil).decode(&out)
	var p store.PositionRow
	for _, row := range out.Positions {
		if row.Symbol == symbol {
			p = row
		}
	}
	// Cash includes fees: what the account actually paid or received.
	total, _ := fixed.Add(p.NetCash, -p.Fees)
	d.check(p.NetQty == qty && total == fixed.MustParse(cash), "position %s %s: %d shares, cash %s after %s fees", account, symbol, p.NetQty, total, p.Fees)
}

func (d *demo) outboxRow(orderID string) store.OutboxRow {
	rows, err := d.s.App.Store.ListOutbox(d.ctx, orderID)
	d.must2(err)
	if len(rows) == 0 {
		d.must2(fmt.Errorf("order %s has no outbox messages", orderID))
	}
	return rows[0]
}

func (d *demo) drain(w *outbox.Worker) { d.drainQuiet(w) }

func (d *demo) drainQuiet(w *outbox.Worker) int {
	n, err := w.Drain(d.ctx)
	d.must2(err)
	return n
}

// flush delivers the broker's queued webhooks and requires the OMS to
// accept each one.
func (d *demo) flush() { d.outcomes(d.s.Sim.Flush(d.ctx)) }

func (d *demo) outcomes(ds []brokersim.Delivery) []string {
	out := make([]string, len(ds))
	for i, dl := range ds {
		if !dl.OK() {
			d.must2(fmt.Errorf("webhook %s was not accepted: %d %s %v", dl.EventID, dl.Status, dl.Body, dl.Err))
		}
		var body struct{ Outcome string }
		json.Unmarshal([]byte(dl.Body), &body)
		out[i] = body.Outcome
	}
	return out
}

func (d *demo) fill(orderID string, qty int64, at string) string {
	execID, _, err := d.s.Sim.Fill(orderID, qty, fixed.MustParse(at))
	d.must2(err)
	return execID
}

// send delivers a hand-built, correctly signed webhook: a report a
// well-behaved broker would not send.
func (d *demo) send(ev broker.WebhookEvent) string {
	ev.EventTime = d.clock.Now()
	body, _ := json.Marshal(ev)
	return d.outcomes([]brokersim.Delivery{d.s.Sim.SendRaw(d.ctx, body, d.s.WebhookSecret)})[0]
}

func (d *demo) openIssue(kind string) store.Issue {
	var out struct{ Issues []store.Issue }
	d.call(d.s.Operator, "GET", "/v1/reconciliation/issues?status=OPEN", nil).decode(&out)
	for _, is := range out.Issues {
		if is.Kind == kind {
			return is
		}
	}
	d.must2(fmt.Errorf("no open %s issue (open: %d)", kind, len(out.Issues)))
	return store.Issue{}
}

func (d *demo) reconcile() store.Run {
	resp := d.call(d.s.Operator, "POST", "/v1/reconciliation/runs", nil)
	var run store.Run
	resp.decode(&run)
	if resp.status != 201 || run.Status != store.RunCompleted {
		d.must2(fmt.Errorf("reconciliation run: %d %s", resp.status, resp.body))
	}
	return run
}
