package integration

import (
	"context"
	"net/http"
	"sync"
	"testing"

	"oms/src/domain"
	"oms/src/service"
	"oms/src/store"
	"oms/tests/harness"
)

func countOrders(t *testing.T, e *harness.Env) int {
	t.Helper()
	orders, err := e.App.Store.ListOrders(context.Background(), store.OrderFilter{Limit: 1000})
	if err != nil {
		t.Fatal(err)
	}
	return len(orders)
}

// Definition of done: repeated requests with identical idempotency keys do
// not create multiple orders.
func TestIdempotency_RepeatedCreateReturnsTheFirstOrder(t *testing.T) {
	e := harness.New(t)
	req := harness.OrderReq()

	first := e.Create(req, "retry-me").Want(http.StatusCreated)
	if first.Header.Get("Idempotent-Replayed") != "" {
		t.Error("the first response is marked as a replay")
	}
	id := first.Order().ID

	for i := 0; i < 5; i++ {
		again := e.Create(req, "retry-me").Want(http.StatusCreated)
		if again.Header.Get("Idempotent-Replayed") != "true" {
			t.Errorf("retry %d is not marked as a replay", i)
		}
		if got := again.Order().ID; got != id {
			t.Fatalf("retry %d returned order %s, want %s", i, got, id)
		}
		if string(again.Body) != string(first.Body) {
			t.Errorf("retry %d body differs from the first response", i)
		}
	}

	if n := countOrders(t, e); n != 1 {
		t.Fatalf("%d orders exist, want 1", n)
	}
	if rows := e.Outbox(id); len(rows) != 1 {
		t.Fatalf("%d submissions queued, want 1", len(rows))
	}
	e.Drain()
	// A retry after the order has moved on still returns the original
	// response, not a second order.
	if got := e.Create(req, "retry-me").Want(http.StatusCreated).Order(); got.ID != id {
		t.Fatalf("late retry returned %s", got.ID)
	}
	if st := e.Sim.Stats(); st.SubmitRequests != 1 {
		t.Errorf("broker received %d submissions, want 1", st.SubmitRequests)
	}
	if got := e.App.Metrics.IdempotentReplays.Value("create"); got != 6 {
		t.Errorf("replay counter = %v, want 6", got)
	}
}

func TestIdempotency_ConcurrentCreatesWithOneKey(t *testing.T) {
	e := harness.New(t)
	req := harness.OrderReq()

	const n = 24
	ids := make([]string, n)
	statuses := make([]int, n)
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			resp := e.Create(req, "same-key")
			statuses[i] = resp.Status
			if resp.Status == http.StatusCreated {
				ids[i] = resp.Order().ID
			}
		}()
	}
	close(start)
	wg.Wait()

	for i := range ids {
		if statuses[i] != http.StatusCreated || ids[i] != ids[0] {
			t.Fatalf("request %d: status %d order %q; first order %q", i, statuses[i], ids[i], ids[0])
		}
	}
	if got := countOrders(t, e); got != 1 {
		t.Fatalf("%d orders exist, want 1", got)
	}
	e.Drain()
	if st := e.Sim.Stats(); st.SubmitRequests != 1 {
		t.Errorf("broker received %d submissions, want 1", st.SubmitRequests)
	}
}

func TestIdempotency_KeyReusedForADifferentRequest(t *testing.T) {
	e := harness.New(t)
	id := e.Create(harness.OrderReq(), "k").Want(http.StatusCreated).Order().ID

	other := harness.OrderReq(func(r *service.CreateOrderRequest) { r.Qty = 101 })
	e.Create(other, "k").WantError(http.StatusUnprocessableEntity, "IDEMPOTENCY_KEY_REUSED")
	if n := countOrders(t, e); n != 1 {
		t.Fatalf("%d orders exist, want 1", n)
	}
	if e.Order(id).Qty != 100 {
		t.Error("the original order was changed")
	}
}

// The request fingerprint is taken from the parsed request, so the same
// order written differently is still the same request.
func TestIdempotency_EquivalentBodiesAreTheSameRequest(t *testing.T) {
	e := harness.New(t)
	a := `{"account_id":"ACC-1","symbol":"AAPL","side":"BUY","type":"LIMIT","time_in_force":"DAY","qty":100,"limit_price":"150.00"}`
	b := `{ "qty": 100, "limit_price": 150, "type": "LIMIT", "side": "BUY", "symbol": "AAPL", "account_id": "ACC-1" }`
	first := e.Call(e.Alice, http.MethodPost, "/v1/orders", a, "Idempotency-Key", "fmt").Want(http.StatusCreated).Order()
	second := e.Call(e.Alice, http.MethodPost, "/v1/orders", b, "Idempotency-Key", "fmt").Want(http.StatusCreated)
	if second.Order().ID != first.ID || second.Header.Get("Idempotent-Replayed") != "true" {
		t.Fatalf("equivalent body was not treated as a retry: %s", second.Body)
	}
}

func TestIdempotency_KeyIsRequiredAndValidated(t *testing.T) {
	e := harness.New(t)
	e.Call(e.Alice, http.MethodPost, "/v1/orders", harness.OrderReq()).
		WantError(http.StatusBadRequest, "IDEMPOTENCY_KEY_REQUIRED")
	e.Create(harness.OrderReq(), "has space").WantError(http.StatusBadRequest, "IDEMPOTENCY_KEY_INVALID")
	if n := countOrders(t, e); n != 0 {
		t.Fatalf("%d orders exist, want 0", n)
	}
}

func TestIdempotency_KeysAreScopedToThePrincipal(t *testing.T) {
	e := harness.New(t)
	a := e.Create(harness.OrderReq(), "shared").Want(http.StatusCreated).Order()
	bobReq := harness.OrderReq(func(r *service.CreateOrderRequest) { r.AccountID = harness.Account3 })
	b := e.Call(e.Bob, http.MethodPost, "/v1/orders", bobReq, "Idempotency-Key", "shared").Want(http.StatusCreated).Order()
	if a.ID == b.ID || b.AccountID != harness.Account3 {
		t.Fatalf("bob's request was answered with alice's order: %+v", b)
	}
}

// A stable client order id is a second line of defence: a client that loses
// its idempotency key cannot create the same order twice.
func TestIdempotency_DuplicateClientOrderID(t *testing.T) {
	e := harness.New(t)
	req := harness.OrderReq(func(r *service.CreateOrderRequest) { r.ClientOrderID = "desk-42" })
	first := e.Create(req, e.Key()).Want(http.StatusCreated).Order()
	if first.ClientOrderID != "desk-42" {
		t.Fatalf("client_order_id = %q", first.ClientOrderID)
	}

	dup := e.Create(req, e.Key()).WantError(http.StatusConflict, "DUPLICATE_CLIENT_ORDER_ID")
	var body struct {
		Error struct {
			OrderID string `json:"order_id"`
		} `json:"error"`
	}
	dup.Decode(&body)
	if body.Error.OrderID != first.ID {
		t.Errorf("the conflict names order %q, want %q", body.Error.OrderID, first.ID)
	}

	// The same client order id in another account is a different order.
	other := harness.OrderReq(func(r *service.CreateOrderRequest) { r.ClientOrderID = "desk-42"; r.AccountID = harness.Account2 })
	e.Create(other, e.Key()).Want(http.StatusCreated)
	if n := countOrders(t, e); n != 2 {
		t.Fatalf("%d orders exist, want 2", n)
	}

	// The refused request left nothing behind: its key can be used again.
	key := e.Key()
	e.Create(req, key).WantError(http.StatusConflict, "DUPLICATE_CLIENT_ORDER_ID")
	fresh := harness.OrderReq(func(r *service.CreateOrderRequest) { r.ClientOrderID = "desk-43" })
	e.Create(fresh, key).Want(http.StatusCreated)
}

func TestIdempotency_RiskRejectionIsReplayedToo(t *testing.T) {
	e := harness.New(t)
	req := harness.OrderReq(func(r *service.CreateOrderRequest) { r.Qty = 50_000 })
	first := e.Create(req, "big").Want(http.StatusCreated).Order()
	if first.State != domain.StateRejected {
		t.Fatalf("state = %s, want REJECTED", first.State)
	}
	again := e.Create(req, "big").Want(http.StatusCreated)
	if again.Order().ID != first.ID || again.Header.Get("Idempotent-Replayed") != "true" {
		t.Fatal("the rejected order was not replayed")
	}
	if n := countOrders(t, e); n != 1 {
		t.Fatalf("%d orders exist, want 1", n)
	}
	if rows := e.Outbox(first.ID); len(rows) != 0 {
		t.Errorf("a rejected order queued %d broker calls", len(rows))
	}
}

func TestIdempotency_RepeatedCancelSendsOneCancel(t *testing.T) {
	e := harness.New(t)
	o := e.Working()
	for i := 0; i < 4; i++ {
		got := e.Cancel(o.ID).Want(http.StatusAccepted).Order()
		if !got.PendingCancel || got.State != domain.StateAcknowledged {
			t.Fatalf("cancel %d: %+v", i, got)
		}
	}
	if n := e.CountEvents(o.ID, domain.EvCancelRequested); n != 1 {
		t.Fatalf("%d CancelRequested events, want 1", n)
	}
	e.Drain()
	if st := e.Sim.Stats(); st.CancelRequests != 1 {
		t.Fatalf("broker received %d cancels, want 1", st.CancelRequests)
	}
	e.Must(e.Sim.ConfirmCancel(o.ID))
	e.Flush()
	e.WantState(o.ID, domain.StateCancelled)
	// Cancelling a cancelled order is still a success and still a no-op.
	e.Cancel(o.ID).Want(http.StatusOK)
	if n := len(e.Events(o.ID)); n != 6 {
		t.Errorf("%d events, want 6: %v", n, e.EventTypes(o.ID))
	}
}

func TestIdempotency_RepeatedReplaceIsReplayed(t *testing.T) {
	e := harness.New(t)
	o := e.Working()
	qty := int64(150)
	body := service.ReplaceOrderRequest{Qty: &qty}
	path := "/v1/orders/" + o.ID + "/replace"

	first := e.Call(e.Alice, http.MethodPost, path, body, "Idempotency-Key", "r1").Want(http.StatusAccepted)
	again := e.Call(e.Alice, http.MethodPost, path, body, "Idempotency-Key", "r1").Want(http.StatusAccepted)
	if again.Header.Get("Idempotent-Replayed") != "true" || string(again.Body) != string(first.Body) {
		t.Fatal("the repeated replace was not replayed")
	}
	if n := e.CountEvents(o.ID, domain.EvReplaceRequested); n != 1 {
		t.Fatalf("%d ReplaceRequested events, want 1", n)
	}
	e.Call(e.Alice, http.MethodPost, path, body).WantError(http.StatusBadRequest, "IDEMPOTENCY_KEY_REQUIRED")
	// A create key cannot be replayed as a replace, or the other way round.
	e.Create(harness.OrderReq(), "r1").WantError(http.StatusUnprocessableEntity, "IDEMPOTENCY_KEY_REUSED")
}
