package integration

import (
	"net/http"
	"reflect"
	"testing"

	"oms/src/domain"
	"oms/src/fixed"
	"oms/src/service"
	"oms/tests/harness"
)

func TestMain(m *testing.M) { harness.Main(m) }

// The whole happy path through real HTTP in both directions: create, outbox
// dispatch, broker acknowledgement, two fills, and what the API shows after.
func TestOrderLifecycle_CreateToFilled(t *testing.T) {
	e := harness.New(t)

	o := e.Create(harness.OrderReq(), "k-1").Want(http.StatusCreated).Order()
	if o.State != domain.StateValidated || o.Version != 2 || o.LeavesQty != 100 {
		t.Fatalf("created order = %+v", o)
	}
	if o.ClientOrderID != o.ID {
		t.Errorf("client_order_id defaults to the order id; got %q", o.ClientOrderID)
	}
	if o.CreatedBy != "alice" {
		t.Errorf("created_by = %q", o.CreatedBy)
	}

	// Nothing reaches the broker until the outbox worker runs.
	if _, known := e.Sim.Order(o.ID); known {
		t.Fatal("the broker has the order before the outbox was dispatched")
	}
	if n := e.Drain(); n != 1 {
		t.Fatalf("drained %d messages, want 1", n)
	}
	e.WantState(o.ID, domain.StateSubmitting)
	bo, known := e.Sim.Order(o.ID)
	if !known || bo.Qty != 100 || bo.Symbol != "AAPL" {
		t.Fatalf("broker order = %+v, known = %v", bo, known)
	}

	e.Must(e.Sim.Ack(o.ID))
	e.Flush()
	acked := e.WantState(o.ID, domain.StateAcknowledged)
	if acked.BrokerOrderID != bo.BrokerOrderID || acked.AcknowledgedAt == nil {
		t.Errorf("acknowledged order = %+v", acked)
	}

	e.Fill(o.ID, 40, "149.50")
	part := e.WantState(o.ID, domain.StatePartiallyFilled)
	if part.FilledQty != 40 || part.LeavesQty != 60 {
		t.Errorf("after first fill: filled %d leaves %d", part.FilledQty, part.LeavesQty)
	}

	e.Fill(o.ID, 60, "150.00")
	done := e.WantState(o.ID, domain.StateFilled)
	e.WantFilled(o.ID, 100, "14980") // 40 × 149.50 + 60 × 150.00
	if done.Fees != fixed.MustParse("0.50") || done.ClosedAt == nil || done.LeavesQty != 0 {
		t.Errorf("filled order: fees %s closed %v leaves %d", done.Fees, done.ClosedAt, done.LeavesQty)
	}

	// The API shows the same thing, with a derived average price.
	got := e.Call(e.Alice, http.MethodGet, "/v1/orders/"+o.ID, nil).Want(http.StatusOK).Order()
	if got.State != domain.StateFilled || got.AvgFillPrice == nil || *got.AvgFillPrice != fixed.MustParse("149.80") {
		t.Errorf("GET order = %s avg %v", got.State, got.AvgFillPrice)
	}

	var history struct {
		Events []domain.Event `json:"events"`
	}
	e.Call(e.Alice, http.MethodGet, "/v1/orders/"+o.ID+"/events", nil).Want(http.StatusOK).Decode(&history)
	var types []string
	for i, ev := range history.Events {
		types = append(types, string(ev.Type))
		if ev.Version != int64(i+1) {
			t.Errorf("event %d has version %d", i, ev.Version)
		}
	}
	want := []string{"OrderCreated", "OrderValidated", "SubmissionStarted", "OrderAcknowledged", "ExecutionRecorded", "ExecutionRecorded"}
	if !reflect.DeepEqual(types, want) {
		t.Errorf("history = %v, want %v", types, want)
	}

	// Buying 100 for 14,980.00 leaves the account long 100 and that much
	// cash lighter; fees are tracked separately.
	pos := e.WantPosition(harness.Account1, "AAPL", 100, "-14980")
	if pos.Fees != fixed.MustParse("0.50") {
		t.Errorf("position fees = %s", pos.Fees)
	}

	var execs struct {
		Executions []struct {
			Kind  string       `json:"kind"`
			Qty   int64        `json:"qty"`
			Price fixed.Micros `json:"price"`
		} `json:"executions"`
	}
	e.Call(e.Alice, http.MethodGet, "/v1/orders/"+o.ID+"/executions", nil).Want(http.StatusOK).Decode(&execs)
	if len(execs.Executions) != 2 || execs.Executions[0].Qty != 40 || execs.Executions[1].Price != fixed.MustParse("150") {
		t.Errorf("executions = %+v", execs.Executions)
	}
	e.WantNoOpenIssues()
}

func TestOrderLifecycle_SellReducesPosition(t *testing.T) {
	e := harness.New(t)
	buy := e.Working()
	e.Fill(buy.ID, 100, "150")
	sell := e.Working(func(r *service.CreateOrderRequest) {
		r.Side = domain.Sell
		r.Qty = 30
		r.LimitPrice = harness.Price("151")
	})
	e.Fill(sell.ID, 30, "151")
	e.WantState(sell.ID, domain.StateFilled)
	// −15,000.00 for the buy, +4,530.00 for the sell.
	e.WantPosition(harness.Account1, "AAPL", 70, "-10470")
}
