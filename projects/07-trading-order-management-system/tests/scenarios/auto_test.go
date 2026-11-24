package scenarios

import (
	"context"
	"net/http"
	"sync"
	"testing"
	"time"

	"oms/src/broker"
	"oms/src/brokersim"
	"oms/src/domain"
	"oms/src/fixed"
	"oms/src/outbox"
	"oms/src/reconcile"
	"oms/src/service"
	"oms/src/store"
	"oms/tests/harness"
)

// Scenario: everything at once, with nothing scripted. The broker runs on
// its own (acknowledging, filling in slices, expiring, cancelling), the
// outbox worker runs in the background, and the faults are all on: failed
// broker requests, lost responses, every webhook delivered twice, and four
// concurrent webhook senders so that reports overtake each other. Clients
// create orders concurrently, retry every create, and cancel some orders
// while they may be filling.
//
// Afterwards the OMS must agree with the broker order by order and
// execution by execution, with no reconciliation needed to get there.
func TestScenario_ConcurrentLoadWithFaults(t *testing.T) {
	e := harness.New(t, func(o *harness.Options) {
		o.Auto, o.Background, o.RealClock = true, true, true
		o.Outbox = outbox.Config{MaxAttempts: 100, BaseBackoff: 5 * time.Millisecond, MaxBackoff: 50 * time.Millisecond,
			PollInterval: 5 * time.Millisecond}
		o.Reconcile = reconcile.Config{Grace: 20 * time.Millisecond}
		o.Sim = func(c *brokersim.Config) {
			c.DeliveryWorkers = 4
			c.MaxSlices = 3
			c.FillDelay = 2 * time.Millisecond
		}
	})
	e.Sim.SetFaults(brokersim.Faults{FailRequests: 25, LoseResponses: 25, DuplicateWebhooks: 1})

	type plan struct {
		req    service.CreateOrderRequest
		cancel bool
		want   []domain.State // acceptable final states
	}
	var plans []plan
	for i := 0; i < 120; i++ {
		p := plan{req: harness.OrderReq(func(r *service.CreateOrderRequest) {
			r.Qty = int64(10 + i%7*15)
			if i%2 == 1 {
				r.Side = domain.Sell
			}
			if i%3 == 0 {
				r.AccountID = harness.Account2
			}
		})}
		switch i % 6 {
		case 0, 1, 2: // marketable limit: fills completely
			p.want = []domain.State{domain.StateFilled}
		case 3: // market order
			p.req.Type, p.req.LimitPrice = domain.Market, nil
			p.want = []domain.State{domain.StateFilled}
		case 4: // IOC that cannot trade: expires at the broker
			p.req.TimeInForce = domain.IOC
			p.req.LimitPrice = away(p.req.Side)
			p.want = []domain.State{domain.StateExpired}
		case 5: // cancelled by the client; may fill first if marketable
			p.cancel = true
			if i%12 == 5 {
				p.req.LimitPrice = away(p.req.Side) // rests: the cancel wins
				p.want = []domain.State{domain.StateCancelled}
			} else {
				p.want = []domain.State{domain.StateCancelled, domain.StateFilled}
			}
		}
		plans = append(plans, p)
	}

	ids := make([]string, len(plans))
	var wg sync.WaitGroup
	sem := make(chan struct{}, 12)
	for i, p := range plans {
		wg.Add(1)
		sem <- struct{}{}
		go func() {
			defer wg.Done()
			defer func() { <-sem }()
			key := e.Key()
			first := e.Create(p.req, key).Want(http.StatusCreated).Order()
			// Every client retries its create, as one that timed out would.
			if again := e.Create(p.req, key).Want(http.StatusCreated).Order(); again.ID != first.ID {
				t.Errorf("retry created a second order: %s and %s", first.ID, again.ID)
			}
			ids[i] = first.ID
			if p.cancel {
				if resp := e.Cancel(first.ID); resp.Status != http.StatusOK && resp.Status != http.StatusAccepted &&
					resp.ErrorCode() != "ORDER_NOT_CANCELLABLE" {
					t.Errorf("cancel %s: %d %s", first.ID, resp.Status, resp.Body)
				}
			}
		}()
	}
	wg.Wait()
	if t.Failed() {
		return
	}

	settled := func() bool {
		for i, id := range ids {
			if !oneOf(e.Order(id).State, plans[i].want) {
				return false
			}
		}
		if len(e.Sim.Pending()) != 0 {
			return false
		}
		// And nothing is left in the outbox: every broker call was either
		// delivered or found to be no longer needed.
		stats, err := e.App.Store.OutboxStats(context.Background())
		return err == nil && stats.Pending == 0 && stats.Dead == 0
	}
	e.Eventually(60*time.Second, "every order to reach its final state", settled)
	time.Sleep(100 * time.Millisecond) // let duplicate deliveries land

	// The broker has each order exactly once.
	snap := e.Sim.Snapshot()
	st := e.Sim.Stats()
	if len(snap.Orders) > len(ids) || st.SubmitRequests-st.DuplicateSubmits != len(snap.Orders) {
		t.Fatalf("broker has %d orders from %d OMS orders; stats %+v", len(snap.Orders), len(ids), st)
	}
	// A submission whose response was lost is sent again unless the broker's
	// acknowledgement arrived first, in which case the resend is skipped; so
	// the number of resends varies, and none of them may create an order.
	if st.WebhooksFailed != 0 || st.WebhooksLost != 0 {
		t.Errorf("broker stats = %+v; want no failed webhooks", st)
	}
	if got := e.App.Metrics.OutboxDispatches.Value(store.OutboxSubmit, "retry"); got < 50 {
		t.Errorf("%v submission retries, want at least the 50 injected failures", got)
	}

	// Order by order, execution by execution, the two sides agree.
	brokerOrders := map[string]broker.OrderSnapshot{}
	for _, b := range snap.Orders {
		brokerOrders[b.ClientOrderID] = b
	}
	type pos struct {
		qty  int64
		cash fixed.Micros
	}
	wantPos := map[[2]string]pos{}
	var execs int
	for _, id := range ids {
		o := e.Order(id)
		b, known := brokerOrders[id]
		if !known {
			if o.State != domain.StateCancelled || o.SubmittedAt != nil {
				t.Errorf("order %s (%s) is unknown to the broker", id, o.State)
			}
			continue
		}
		if o.FilledQty != b.CumQty || o.FilledQty > o.Qty {
			t.Errorf("order %s: OMS filled %d of %d, broker %d", id, o.FilledQty, o.Qty, b.CumQty)
		}
		var qty int64
		var notional fixed.Micros
		for _, x := range e.Executions(id) {
			qty += x.DeltaQty
			notional += x.DeltaNotional
			execs++
		}
		if qty != o.FilledQty || notional != o.FilledNotional {
			t.Errorf("order %s: executions sum to %d / %s, order says %d / %s", id, qty, notional, o.FilledQty, o.FilledNotional)
		}
		k := [2]string{o.AccountID, o.Symbol}
		p := wantPos[k]
		p.qty += o.Side.Sign() * o.FilledQty
		p.cash -= fixed.Micros(o.Side.Sign()) * o.FilledNotional
		wantPos[k] = p
	}
	if execs != len(snap.Executions) {
		t.Errorf("OMS booked %d executions, broker has %d", execs, len(snap.Executions))
	}
	for k, want := range wantPos {
		if got := e.Position(k[0], k[1]); got.NetQty != want.qty || got.NetCash != want.cash {
			t.Errorf("position %v = %d / %s, want %d / %s", k, got.NetQty, got.NetCash, want.qty, want.cash)
		}
	}

	// Reconciliation confirms it: nothing to repair, nothing to report.
	run, err := e.App.Reconciler.Run(context.Background(), "test")
	if err != nil || run.Status != store.RunCompleted {
		t.Fatalf("reconcile: %+v, %v", run, err)
	}
	if run.DriftFound != 0 || run.AutoRepaired != 0 {
		t.Errorf("reconciliation found drift after the run: %+v", run)
	}
	e.WantNoOpenIssues()
	if dup := e.App.Metrics.BrokerEvents.Value("FILL", "BROKER", "DUPLICATE"); dup == 0 {
		t.Error("no duplicate fill deliveries were seen; the scenario did not exercise deduplication")
	}
	t.Logf("%d orders, %d executions, broker stats %+v", len(ids), execs, st)
}

// away returns a limit price that cannot trade against the reference price
// of 150.00.
func away(side domain.Side) *fixed.Micros {
	if side == domain.Sell {
		return harness.Price("175")
	}
	return harness.Price("125")
}

func oneOf(s domain.State, set []domain.State) bool {
	for _, v := range set {
		if s == v {
			return true
		}
	}
	return false
}
