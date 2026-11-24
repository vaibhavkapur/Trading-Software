package integration

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"oms/src/broker"
	"oms/src/domain"
	"oms/src/risk"
	"oms/src/service"
	"oms/tests/harness"
)

// waitFor bounds how long a test waits for background work.
const waitFor = 15 * time.Second

func TestAPI_AuthenticationFailsClosed(t *testing.T) {
	e := harness.New(t)
	o := e.Created()
	paths := []struct{ method, path string }{
		{http.MethodPost, "/v1/orders"},
		{http.MethodGet, "/v1/orders"},
		{http.MethodGet, "/v1/orders/" + o.ID},
		{http.MethodGet, "/v1/orders/" + o.ID + "/events"},
		{http.MethodPost, "/v1/orders/" + o.ID + "/cancel"},
		{http.MethodGet, "/v1/positions"},
		{http.MethodPost, "/v1/reconciliation/runs"},
		{http.MethodGet, "/v1/reconciliation/issues"},
	}
	for _, p := range paths {
		for name, token := range map[string]string{"no token": "", "unknown token": strings.Repeat("x", 48)} {
			resp := e.Call(token, p.method, p.path, nil)
			if resp.Status != http.StatusUnauthorized || resp.ErrorCode() != "UNAUTHORIZED" {
				t.Errorf("%s %s with %s: %d %s", p.method, p.path, name, resp.Status, resp.Body)
			}
		}
	}
	// A token in the wrong scheme is not a token.
	resp := e.Call("", http.MethodGet, "/v1/orders/"+o.ID, nil, "Authorization", "Basic "+e.Alice)
	resp.WantError(http.StatusUnauthorized, "UNAUTHORIZED")
	if resp.Header.Get("WWW-Authenticate") == "" {
		t.Error("401 without WWW-Authenticate")
	}
	e.WantState(o.ID, domain.StateValidated) // the unauthenticated cancel did nothing
}

func TestAPI_AccountScopesAndRoles(t *testing.T) {
	e := harness.New(t)
	o := e.Working() // alice, ACC-1
	e.Fill(o.ID, 10, "150")
	orderPath := "/v1/orders/" + o.ID

	// bob trades ACC-3 only: alice's order does not exist for him. 404, not
	// 403, so order ids cannot be probed.
	for _, p := range []struct{ method, path string }{
		{http.MethodGet, orderPath}, {http.MethodGet, orderPath + "/events"},
		{http.MethodGet, orderPath + "/executions"}, {http.MethodPost, orderPath + "/cancel"},
	} {
		e.Call(e.Bob, p.method, p.path, nil).WantError(http.StatusNotFound, "ORDER_NOT_FOUND")
	}
	e.Call(e.Bob, http.MethodPost, orderPath+"/replace", `{"qty": 5}`, "Idempotency-Key", "b1").
		WantError(http.StatusNotFound, "ORDER_NOT_FOUND")
	e.Call(e.Bob, http.MethodPost, "/v1/orders", harness.OrderReq(), "Idempotency-Key", "b2").
		WantError(http.StatusForbidden, "FORBIDDEN")
	e.Call(e.Bob, http.MethodGet, "/v1/orders?account_id="+harness.Account1, nil).WantError(http.StatusForbidden, "FORBIDDEN")
	e.Call(e.Bob, http.MethodGet, "/v1/positions?account_id="+harness.Account1, nil).WantError(http.StatusForbidden, "FORBIDDEN")

	var list struct {
		Orders []service.OrderView `json:"orders"`
	}
	e.Call(e.Bob, http.MethodGet, "/v1/orders", nil).Want(http.StatusOK).Decode(&list)
	if len(list.Orders) != 0 {
		t.Fatalf("bob sees %d orders", len(list.Orders))
	}

	// vera can look at ACC-1 but not touch it.
	e.Call(e.Vera, http.MethodGet, orderPath, nil).Want(http.StatusOK)
	e.Call(e.Vera, http.MethodPost, orderPath+"/cancel", nil).WantError(http.StatusForbidden, "FORBIDDEN")
	e.Call(e.Vera, http.MethodPost, "/v1/orders", harness.OrderReq(), "Idempotency-Key", "v1").
		WantError(http.StatusForbidden, "FORBIDDEN")
	var positions struct {
		Positions []struct {
			AccountID string `json:"account_id"`
			NetQty    int64  `json:"net_qty"`
		} `json:"positions"`
	}
	e.Call(e.Vera, http.MethodGet, "/v1/positions", nil).Want(http.StatusOK).Decode(&positions)
	if len(positions.Positions) != 1 || positions.Positions[0].NetQty != 10 {
		t.Fatalf("vera's positions = %+v", positions.Positions)
	}

	// olivia operates: she sees everything and trades nothing.
	e.Call(e.Olivia, http.MethodGet, orderPath, nil).Want(http.StatusOK)
	e.Call(e.Olivia, http.MethodPost, orderPath+"/cancel", nil).WantError(http.StatusForbidden, "FORBIDDEN")
	e.Call(e.Olivia, http.MethodPost, "/v1/orders", harness.OrderReq(), "Idempotency-Key", "o1").
		WantError(http.StatusForbidden, "FORBIDDEN")

	// alice's listing is filtered by state and account.
	e.Created(func(r *service.CreateOrderRequest) { r.AccountID = harness.Account2 })
	e.Call(e.Alice, http.MethodGet, "/v1/orders", nil).Want(http.StatusOK).Decode(&list)
	if len(list.Orders) != 2 {
		t.Fatalf("alice sees %d orders, want 2", len(list.Orders))
	}
	e.Call(e.Alice, http.MethodGet, "/v1/orders?state=PARTIALLY_FILLED", nil).Want(http.StatusOK).Decode(&list)
	if len(list.Orders) != 1 || list.Orders[0].ID != o.ID {
		t.Fatalf("filtered listing = %+v", list.Orders)
	}
	e.Call(e.Alice, http.MethodGet, "/v1/orders?state=NOPE", nil).WantError(http.StatusBadRequest, "BAD_REQUEST")
	e.WantState(o.ID, domain.StatePartiallyFilled)
}

func TestAPI_ValidationRejectsBeforeAnythingIsStored(t *testing.T) {
	e := harness.New(t)
	post := func(body string) harness.Resp {
		return e.Call(e.Alice, http.MethodPost, "/v1/orders", body, "Idempotency-Key", e.Key())
	}
	bad := post(`{"account_id":"ACC-1","symbol":"","side":"HOLD","type":"LIMIT","qty":0}`).
		WantError(http.StatusUnprocessableEntity, "VALIDATION_FAILED")
	var body struct {
		Error struct {
			Fields []domain.FieldError `json:"fields"`
		} `json:"error"`
	}
	bad.Decode(&body)
	got := map[string]bool{}
	for _, f := range body.Error.Fields {
		got[f.Field] = true
	}
	for _, want := range []string{"symbol", "side", "qty", "limit_price"} {
		if !got[want] {
			t.Errorf("no validation error for %s: %+v", want, body.Error.Fields)
		}
	}

	cases := map[string]string{
		"not json":               `{"account_id":`,
		"unknown field":          `{"account_id":"ACC-1","symbol":"AAPL","side":"BUY","type":"LIMIT","qty":1,"limit_price":"1","quantity":5}`,
		"trailing data":          `{"account_id":"ACC-1","symbol":"AAPL","side":"BUY","type":"LIMIT","qty":1,"limit_price":"1"} {}`,
		"fractional quantity":    `{"account_id":"ACC-1","symbol":"AAPL","side":"BUY","type":"LIMIT","qty":1.5,"limit_price":"1"}`,
		"price beyond precision": `{"account_id":"ACC-1","symbol":"AAPL","side":"BUY","type":"LIMIT","qty":1,"limit_price":"1.0000001"}`,
		"price not a number":     `{"account_id":"ACC-1","symbol":"AAPL","side":"BUY","type":"LIMIT","qty":1,"limit_price":"1e3"}`,
	}
	for name, payload := range cases {
		if resp := post(payload); resp.Status != http.StatusBadRequest || resp.ErrorCode() != "MALFORMED_REQUEST" {
			t.Errorf("%s: %d %s", name, resp.Status, resp.Body)
		}
	}
	for name, payload := range map[string]string{
		"market order with a price": `{"account_id":"ACC-1","symbol":"AAPL","side":"BUY","type":"MARKET","qty":1,"limit_price":"1"}`,
		"negative price":            `{"account_id":"ACC-1","symbol":"AAPL","side":"BUY","type":"LIMIT","qty":1,"limit_price":"-1"}`,
		"quantity too large":        `{"account_id":"ACC-1","symbol":"AAPL","side":"BUY","type":"LIMIT","qty":1000000001,"limit_price":"1"}`,
		"bad time in force":         `{"account_id":"ACC-1","symbol":"AAPL","side":"BUY","type":"LIMIT","qty":1,"limit_price":"1","time_in_force":"FOK"}`,
	} {
		if resp := post(payload); resp.Status != http.StatusUnprocessableEntity {
			t.Errorf("%s: %d %s", name, resp.Status, resp.Body)
		}
	}

	big := `{"account_id":"ACC-1","symbol":"` + strings.Repeat("A", 1<<20) + `"}`
	if resp := post(big); resp.Status != http.StatusRequestEntityTooLarge {
		t.Errorf("oversized body: %d", resp.Status)
	}
	if n := countOrders(t, e); n != 0 {
		t.Fatalf("%d orders were stored from invalid requests", n)
	}
	e.Call(e.Alice, http.MethodGet, "/v1/orders/ord_missing", nil).WantError(http.StatusNotFound, "ORDER_NOT_FOUND")
	if resp := e.Call(e.Alice, http.MethodDelete, "/v1/orders", nil); resp.Status != http.StatusMethodNotAllowed {
		t.Errorf("DELETE /v1/orders: %d", resp.Status)
	}
}

// A pre-trade risk refusal is recorded as an order that ends REJECTED, so
// there is an audit trail of what was asked and why it was refused. Nothing
// is sent to the broker.
func TestAPI_RiskRejectionIsRecordedAndNeverSent(t *testing.T) {
	e := harness.New(t)
	cases := []struct {
		name string
		mod  func(*service.CreateOrderRequest)
		code string
	}{
		{"symbol", func(r *service.CreateOrderRequest) { r.Symbol = "GME" }, risk.CodeSymbolNotAllowed},
		{"quantity", func(r *service.CreateOrderRequest) { r.Qty = 10_001 }, risk.CodeMaxOrderQty},
		{"notional", func(r *service.CreateOrderRequest) { r.Qty = 10_000; r.LimitPrice = harness.Price("900") }, risk.CodeMaxOrderNotional},
	}
	for _, c := range cases {
		o := e.Create(harness.OrderReq(c.mod), e.Key()).Want(http.StatusCreated).Order()
		if o.State != domain.StateRejected || o.RejectReason == "" || o.ClosedAt == nil {
			t.Errorf("%s: order = %+v", c.name, o)
		}
		evs := e.Events(o.ID)
		if len(evs) != 2 || evs[1].Type != domain.EvOrderRejected || !strings.Contains(string(evs[1].Payload), c.code) {
			t.Errorf("%s: history = %v payload %s", c.name, e.EventTypes(o.ID), evs[len(evs)-1].Payload)
		}
		e.Cancel(o.ID).WantError(http.StatusConflict, "ORDER_NOT_CANCELLABLE")
	}
	e.Drain()
	if st := e.Sim.Stats(); st.SubmitRequests != 0 {
		t.Fatalf("the broker received %d risk-rejected orders", st.SubmitRequests)
	}
	if got := e.App.Metrics.OrdersCreated.Value("rejected"); got != 3 {
		t.Errorf("rejected counter = %v", got)
	}
}

type brokenRisk struct{}

func (brokenRisk) Authorize(context.Context, risk.Request) (risk.Decision, error) {
	return risk.Decision{}, errors.New("risk engine unreachable")
}

// No risk decision, no order: the boundary fails closed.
func TestAPI_RiskUnavailableFailsClosed(t *testing.T) {
	e := harness.New(t, func(o *harness.Options) { o.Risk = brokenRisk{} })
	e.Create(harness.OrderReq(), "k").WantError(http.StatusServiceUnavailable, "RISK_UNAVAILABLE")
	if n := countOrders(t, e); n != 0 {
		t.Fatalf("%d orders stored without a risk decision", n)
	}
}

func TestAPI_BrokerRejectsUnknownSymbol(t *testing.T) {
	e := harness.New(t, func(o *harness.Options) { o.Auto = true; o.Background = true; o.RealClock = true })
	o := e.Created(func(r *service.CreateOrderRequest) { r.Symbol = harness.UnknownSymbol })
	e.Eventually(waitFor, "the broker to reject the order", func() bool {
		return e.Order(o.ID).State == domain.StateRejected
	})
	if got := e.Order(o.ID); !strings.Contains(got.RejectReason, "UNKNOWN_SYMBOL") {
		t.Errorf("reject_reason = %q", got.RejectReason)
	}
}

func TestAPI_WebhookAuthentication(t *testing.T) {
	e := harness.New(t)
	o := e.Submitted()
	ctx := context.Background()
	e.Must(e.Sim.Ack(o.ID))
	pending := e.Sim.Pending()
	if len(pending) != 1 {
		t.Fatalf("%d pending webhooks", len(pending))
	}
	body := pending[0].Body

	// Signed with the wrong secret.
	if d := e.Sim.SendRaw(ctx, body, "not-the-secret"); d.Status != http.StatusUnauthorized {
		t.Fatalf("wrong secret: %d %s", d.Status, d.Body)
	}
	// A valid signature for a different body.
	tampered := []byte(strings.Replace(string(body), "order.acknowledged", "order.cancelled", 1))
	req, _ := http.NewRequest(http.MethodPost, e.URL+"/v1/broker/webhooks", strings.NewReader(string(tampered)))
	req.Header.Set(broker.SignatureHeader, broker.Sign(e.WebhookSecret, body))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("tampered body: %d", resp.StatusCode)
	}
	// No signature at all; an API bearer token is not a substitute.
	e.Call(e.Olivia, http.MethodPost, "/v1/broker/webhooks", body).WantError(http.StatusUnauthorized, "BAD_SIGNATURE")

	e.WantState(o.ID, domain.StateSubmitting) // nothing got through
	if got := e.App.Metrics.WebhookRejects.Value("signature"); got != 3 {
		t.Errorf("signature reject counter = %v, want 3", got)
	}
	e.Flush()
	e.WantState(o.ID, domain.StateAcknowledged)
}

func TestAPI_HealthAndMetrics(t *testing.T) {
	e := harness.New(t)
	o := e.Working()
	e.Fill(o.ID, 100, "150")
	e.Create(harness.OrderReq(), "again")
	e.Create(harness.OrderReq(), "again")

	e.Call("", http.MethodGet, "/healthz", nil).Want(http.StatusOK)
	e.Call("", http.MethodGet, "/readyz", nil).Want(http.StatusOK)
	text := string(e.Call("", http.MethodGet, "/metrics", nil).Want(http.StatusOK).Body)
	for _, want := range []string{
		`oms_http_requests_total{route="POST /v1/orders",status="201"} 3`,
		`oms_orders_created_total{result="validated"} 2`,
		`oms_idempotent_replays_total{operation="create"} 1`,
		`oms_outbox_dispatch_total{type="SUBMIT_ORDER",result="delivered"} 1`,
		`oms_broker_events_total{kind="FILL",source="BROKER",outcome="APPLIED"} 1`,
		`oms_submit_to_ack_seconds_count 1`,
		`oms_order_state_lag_seconds_count{kind="ACK"} 1`,
		`oms_outbox_delivery_seconds_count{type="SUBMIT_ORDER"} 1`,
		`oms_outbox_pending 1`,
		`oms_outbox_dead 0`,
		`oms_outbox_oldest_pending_age_seconds 0`,
		`oms_reconciliation_open_issues{severity="CRITICAL"} 0`,
		"# TYPE oms_reconciliation_drift_total counter",
		"# TYPE oms_reconciliation_repair_seconds histogram",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("metrics do not contain %q", want)
		}
	}
	// Outbox age grows while a message waits.
	e.Advance(90 * time.Second)
	if text = e.Metric(); !strings.Contains(text, "oms_outbox_oldest_pending_age_seconds 90") {
		t.Errorf("outbox age gauge did not advance")
	}
}
