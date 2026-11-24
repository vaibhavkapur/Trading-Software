package unit

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"oms/src/broker"
	"oms/src/domain"
	"oms/src/fixed"
)

// testSecret is a throwaway value made up for these tests.
const testSecret = "unit-test-webhook-secret"

func TestWebhookSignature(t *testing.T) {
	body := []byte(`{"event_id":"evt-1"}`)
	sig := broker.Sign(testSecret, body)
	if !strings.HasPrefix(sig, "sha256=") || len(sig) != len("sha256=")+64 {
		t.Fatalf("signature format: %q", sig)
	}
	if !broker.VerifySignature(testSecret, sig, body) {
		t.Error("a correct signature does not verify")
	}
	refused := map[string]struct {
		secret, header string
		body           []byte
	}{
		"tampered body":    {testSecret, sig, []byte(`{"event_id":"evt-2"}`)},
		"wrong secret":     {"another-secret", sig, body},
		"missing header":   {testSecret, "", body},
		"missing prefix":   {testSecret, strings.TrimPrefix(sig, "sha256="), body},
		"upper-case hex":   {testSecret, "sha256=" + strings.ToUpper(strings.TrimPrefix(sig, "sha256=")), body},
		"truncated":        {testSecret, sig[:len(sig)-1], body},
		"trailing garbage": {testSecret, sig + "0", body},
		// Fails closed: with no secret configured nothing verifies, not even
		// a signature computed with the empty secret.
		"empty secret": {"", broker.Sign("", body), body},
	}
	for name, c := range refused {
		if broker.VerifySignature(c.secret, c.header, c.body) {
			t.Errorf("%s: verified", name)
		}
	}
}

func TestNormalizeWebhook(t *testing.T) {
	const at = `"event_time":"2026-01-05T09:30:00.123456-05:00"`
	wantTime := time.Date(2026, 1, 5, 14, 30, 0, 123456000, time.UTC)
	p := func(s string) fixed.Micros { return fixed.MustParse(s) }
	lp := p("149.50")

	good := []struct {
		name, body string
		want       domain.BrokerReport
	}{
		{"ack", `{"event_id":"e1","type":"order.acknowledged","client_order_id":"ord_1","broker_order_id":"B-1",` + at + `}`,
			domain.BrokerReport{EventID: "e1", Kind: domain.ReportAck, OrderID: "ord_1", BrokerOrderID: "B-1"}},
		{"reject", `{"event_id":"e2","type":"order.rejected","client_order_id":"ord_1","reason":"UNKNOWN_SYMBOL",` + at + `}`,
			domain.BrokerReport{EventID: "e2", Kind: domain.ReportReject, OrderID: "ord_1", Reason: "UNKNOWN_SYMBOL"}},
		{"fill", `{"event_id":"e3","type":"order.fill","client_order_id":"ord_1","exec_id":"X-1","qty":40,"price":"150.25","fee":"0.20",` + at + `}`,
			domain.BrokerReport{EventID: "e3", Kind: domain.ReportFill, OrderID: "ord_1", ExecID: "X-1", Qty: 40, Price: p("150.25"), Fee: p("0.2")}},
		{"fill with a zero fee and numeric prices", `{"event_id":"e4","type":"order.fill","client_order_id":"ord_1","exec_id":"X-2","qty":1,"price":150.1,"fee":0,` + at + `}`,
			domain.BrokerReport{EventID: "e4", Kind: domain.ReportFill, OrderID: "ord_1", ExecID: "X-2", Qty: 1, Price: p("150.10")}},
		{"correction", `{"event_id":"e5","type":"order.fill_corrected","client_order_id":"ord_1","exec_id":"X-3","ref_exec_id":"X-1","qty":30,"price":"150.00","fee":"0.15",` + at + `}`,
			domain.BrokerReport{EventID: "e5", Kind: domain.ReportCorrect, OrderID: "ord_1", ExecID: "X-3", RefExecID: "X-1", Qty: 30, Price: p("150"), Fee: p("0.15")}},
		{"bust", `{"event_id":"e6","type":"order.fill_busted","client_order_id":"ord_1","exec_id":"X-4","ref_exec_id":"X-1",` + at + `}`,
			domain.BrokerReport{EventID: "e6", Kind: domain.ReportBust, OrderID: "ord_1", ExecID: "X-4", RefExecID: "X-1"}},
		{"cancelled", `{"event_id":"e7","type":"order.cancelled","client_order_id":"ord_1",` + at + `}`,
			domain.BrokerReport{EventID: "e7", Kind: domain.ReportCancelled, OrderID: "ord_1"}},
		{"cancel rejected", `{"event_id":"e8","type":"order.cancel_rejected","client_order_id":"ord_1","reason":"TOO_LATE",` + at + `}`,
			domain.BrokerReport{EventID: "e8", Kind: domain.ReportCancelRejected, OrderID: "ord_1", Reason: "TOO_LATE"}},
		{"replaced", `{"event_id":"e9","type":"order.replaced","client_order_id":"ord_1","order_qty":80,"limit_price":"149.50",` + at + `}`,
			domain.BrokerReport{EventID: "e9", Kind: domain.ReportReplaced, OrderID: "ord_1", OrderQty: 80, LimitPrice: &lp}},
		{"replace rejected", `{"event_id":"e10","type":"order.replace_rejected","client_order_id":"ord_1",` + at + `}`,
			domain.BrokerReport{EventID: "e10", Kind: domain.ReportReplaceReject, OrderID: "ord_1"}},
		{"expired, with fields this version does not know", `{"event_id":"e11","type":"order.expired","client_order_id":"ord_1","venue":"SIM","nested":{"a":[1,2]},` + at + `}`,
			domain.BrokerReport{EventID: "e11", Kind: domain.ReportExpired, OrderID: "ord_1"}},
	}
	for _, c := range good {
		got, err := broker.NormalizeWebhook([]byte(c.body))
		if err != nil {
			t.Errorf("%s: %v", c.name, err)
			continue
		}
		// The broker's own timestamp is kept as event time, in UTC, and the
		// exact bytes received are kept for the audit trail.
		if !got.EventTime.Equal(wantTime) || got.EventTime.Location() != time.UTC {
			t.Errorf("%s: event time %v", c.name, got.EventTime)
		}
		if string(got.Raw) != c.body {
			t.Errorf("%s: raw body not preserved", c.name)
		}
		got.EventTime, got.Raw = time.Time{}, nil
		gotJSON, _ := json.Marshal(got)
		wantJSON, _ := json.Marshal(c.want)
		if string(gotJSON) != string(wantJSON) {
			t.Errorf("%s:\n got %s\nwant %s", c.name, gotJSON, wantJSON)
		}
	}

	bad := map[string]struct{ body, mentions string }{
		"not JSON":                  {`{"event_id":`, ""},
		"an array":                  {`[]`, ""},
		"empty object":              {`{}`, "event_id, a known type, client_order_id, event_time"},
		"unknown type":              {`{"event_id":"e","type":"order.exploded","client_order_id":"o",` + at + `}`, "a known type"},
		"no event id":               {`{"type":"order.acknowledged","client_order_id":"o",` + at + `}`, "event_id"},
		"no order id":               {`{"event_id":"e","type":"order.acknowledged",` + at + `}`, "client_order_id"},
		"no event time":             {`{"event_id":"e","type":"order.acknowledged","client_order_id":"o"}`, "event_time"},
		"unparseable event time":    {`{"event_id":"e","type":"order.acknowledged","client_order_id":"o","event_time":"yesterday"}`, ""},
		"fill without exec id":      {`{"event_id":"e","type":"order.fill","client_order_id":"o","qty":1,"price":"1","fee":"0",` + at + `}`, "exec_id"},
		"fill without qty":          {`{"event_id":"e","type":"order.fill","client_order_id":"o","exec_id":"x","price":"1","fee":"0",` + at + `}`, "qty"},
		"fill with zero qty":        {`{"event_id":"e","type":"order.fill","client_order_id":"o","exec_id":"x","qty":0,"price":"1","fee":"0",` + at + `}`, "qty"},
		"fill with negative qty":    {`{"event_id":"e","type":"order.fill","client_order_id":"o","exec_id":"x","qty":-5,"price":"1","fee":"0",` + at + `}`, "qty"},
		"fill with fractional qty":  {`{"event_id":"e","type":"order.fill","client_order_id":"o","exec_id":"x","qty":1.5,"price":"1","fee":"0",` + at + `}`, ""},
		"fill without price":        {`{"event_id":"e","type":"order.fill","client_order_id":"o","exec_id":"x","qty":1,"fee":"0",` + at + `}`, "price"},
		"fill without fee":          {`{"event_id":"e","type":"order.fill","client_order_id":"o","exec_id":"x","qty":1,"price":"1",` + at + `}`, "fee"},
		"fill with 7 decimals":      {`{"event_id":"e","type":"order.fill","client_order_id":"o","exec_id":"x","qty":1,"price":"1.0000001","fee":"0",` + at + `}`, ""},
		"fill with exponent price":  {`{"event_id":"e","type":"order.fill","client_order_id":"o","exec_id":"x","qty":1,"price":1e2,"fee":"0",` + at + `}`, ""},
		"correction without ref":    {`{"event_id":"e","type":"order.fill_corrected","client_order_id":"o","exec_id":"x","qty":1,"price":"1","fee":"0",` + at + `}`, "ref_exec_id"},
		"bust without ref":          {`{"event_id":"e","type":"order.fill_busted","client_order_id":"o","exec_id":"x",` + at + `}`, "ref_exec_id"},
		"bust without exec id":      {`{"event_id":"e","type":"order.fill_busted","client_order_id":"o","ref_exec_id":"x",` + at + `}`, "exec_id"},
		"replaced without quantity": {`{"event_id":"e","type":"order.replaced","client_order_id":"o",` + at + `}`, "order_qty"},
	}
	for name, c := range bad {
		_, err := broker.NormalizeWebhook([]byte(c.body))
		if !errors.Is(err, broker.ErrMalformed) {
			t.Errorf("%s: err = %v, want ErrMalformed", name, err)
			continue
		}
		if !strings.Contains(err.Error(), c.mentions) {
			t.Errorf("%s: %q does not mention %q", name, err, c.mentions)
		}
	}
}

func TestExecutionSnapshot_ReportMatchesTheWebhookShape(t *testing.T) {
	at := time.Date(2026, 1, 5, 14, 30, 0, 0, time.UTC)
	x := broker.ExecutionSnapshot{ExecID: "X-2", Kind: domain.ExecCorrect, RefExecID: "X-1", ClientOrderID: "ord_1",
		BrokerOrderID: "B-1", Qty: 30, Price: fixed.MustParse("150"), Fee: fixed.MustParse("0.15"), ExecutedAt: at}
	r := x.Report("recon:run_1:X-2")
	if r.EventID != "recon:run_1:X-2" || r.Kind != domain.ReportCorrect || r.OrderID != "ord_1" || r.ExecID != "X-2" ||
		r.RefExecID != "X-1" || r.Qty != 30 || r.Price != x.Price || r.Fee != x.Fee || !r.EventTime.Equal(at) {
		t.Errorf("report = %+v", r)
	}
	for kind, want := range map[domain.ExecKind]domain.ReportKind{
		domain.ExecFill: domain.ReportFill, domain.ExecCorrect: domain.ReportCorrect, domain.ExecBust: domain.ReportBust,
	} {
		x.Kind = kind
		if got := x.Report("e").Kind; got != want {
			t.Errorf("%s -> %s, want %s", kind, got, want)
		}
	}
}

// TestHTTPClient_RetryClassification pins the one decision the dispatcher
// depends on: which failures mean "the broker refused this request" (stop)
// and which mean "unknown, try again".
func TestHTTPClient_RetryClassification(t *testing.T) {
	var status int
	var body string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(status)
		io.WriteString(w, body)
	}))
	defer srv.Close()
	c := broker.NewHTTPClient(srv.URL+"/", "unit-test-broker-key", 2*time.Second)
	ctx := context.Background()

	cases := []struct {
		status    int
		retryable bool
	}{
		{400, false}, {409, false}, {422, false},
		{401, true}, {403, true}, {408, true}, {429, true},
		{500, true}, {502, true}, {503, true}, {504, true},
		{301, true}, // an unexpected redirect is not a verdict on the order
	}
	for _, tc := range cases {
		status, body = tc.status, `{"error":{"code":"SOME_CODE","message":"some message"}}`
		err := c.Cancel(ctx, "ord_1", "req-1")
		var be *broker.Error
		if !errors.As(err, &be) {
			t.Fatalf("%d: err = %v", tc.status, err)
		}
		if be.Status != tc.status || be.Retryable != tc.retryable || broker.IsRetryable(err) != tc.retryable {
			t.Errorf("%d: status %d retryable %v, want %v", tc.status, be.Status, be.Retryable, tc.retryable)
		}
		if tc.status != 301 && (be.Code != "SOME_CODE" || be.Message != "some message") {
			t.Errorf("%d: code %q message %q", tc.status, be.Code, be.Message)
		}
	}

	// 404 means different things per call: the order is unknown for cancel
	// (a refusal), unknown for a lookup (ErrUnknownOrder), but for submit the
	// endpoint itself is missing, which says nothing about the order.
	status, body = 404, `{"error":{"code":"UNKNOWN_ORDER","message":"no such order"}}`
	if err := c.Cancel(ctx, "ord_1", "req-1"); broker.IsRetryable(err) {
		t.Errorf("cancel 404 is retryable: %v", err)
	}
	if err := c.Replace(ctx, "ord_1", "req-1", broker.ReplaceRequest{Qty: 1}); broker.IsRetryable(err) {
		t.Errorf("replace 404 is retryable: %v", err)
	}
	if _, err := c.GetOrder(ctx, "ord_1"); !errors.Is(err, broker.ErrUnknownOrder) || broker.IsRetryable(err) {
		t.Errorf("get order 404: %v", err)
	}
	if err := c.Submit(ctx, broker.SubmitRequest{ClientOrderID: "ord_1"}); !broker.IsRetryable(err) {
		t.Errorf("submit 404 is not retryable: %v", err)
	}

	// A failure with an unreadable body still classifies by status.
	status, body = 400, `<html>bad request</html>`
	if err := c.Cancel(ctx, "ord_1", "req-1"); err == nil || broker.IsRetryable(err) {
		t.Errorf("400 with an HTML body: %v", err)
	}
	// A success whose body cannot be decoded tells us nothing: retry.
	status, body = 200, `not json`
	if _, err := c.Snapshot(ctx); err == nil || !broker.IsRetryable(err) {
		t.Errorf("undecodable 200: %v", err)
	}
	status, body = 202, ``
	if err := c.Cancel(ctx, "ord_1", "req-1"); err != nil {
		t.Errorf("202: %v", err)
	}

	// Errors the adapter never classified are retried; nil is not an error.
	if !broker.IsRetryable(errors.New("something else")) {
		t.Error("an unclassified error is not retryable")
	}
}

func TestHTTPClient_NoResponseIsRetryable(t *testing.T) {
	ctx := context.Background()

	// Connection refused.
	srv := httptest.NewServer(http.NotFoundHandler())
	url := srv.URL
	srv.Close()
	err := broker.NewHTTPClient(url, "unit-test-broker-key", time.Second).Submit(ctx, broker.SubmitRequest{ClientOrderID: "ord_1"})
	var be *broker.Error
	if !errors.As(err, &be) || be.Status != 0 || !be.Retryable {
		t.Errorf("connection refused: %v", err)
	}

	// Timeout: the broker may well have processed the request.
	release := make(chan struct{})
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-release:
		case <-r.Context().Done():
		}
	}))
	defer slow.Close()
	defer close(release)
	err = broker.NewHTTPClient(slow.URL, "unit-test-broker-key", 50*time.Millisecond).Submit(ctx, broker.SubmitRequest{ClientOrderID: "ord_1"})
	if !errors.As(err, &be) || be.Status != 0 || !be.Retryable {
		t.Errorf("timeout: %v", err)
	}

	// Connection dropped mid-response.
	dropped := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, _, err := w.(http.Hijacker).Hijack()
		if err == nil {
			conn.Close()
		}
	}))
	defer dropped.Close()
	err = broker.NewHTTPClient(dropped.URL, "unit-test-broker-key", time.Second).Cancel(ctx, "ord_1", "req-1")
	if !errors.As(err, &be) || !be.Retryable {
		t.Errorf("dropped connection: %v", err)
	}
}

func TestHTTPClient_RequestsCarryIdentityAndIdempotency(t *testing.T) {
	type seen struct {
		method, path, auth, idem, ctype, body string
	}
	var got []seen
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		got = append(got, seen{r.Method, r.URL.EscapedPath(), r.Header.Get("Authorization"), r.Header.Get("Idempotency-Key"),
			r.Header.Get("Content-Type"), string(b)})
		switch {
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/snapshot"):
			io.WriteString(w, `{"as_of":"2026-01-05T14:30:00Z","orders":[{"client_order_id":"ord_1","status":"WORKING","qty":100,"cum_qty":40,"limit_price":"150.25","updated_at":"2026-01-05T14:29:00Z"}],"executions":[{"exec_id":"X-1","kind":"FILL","client_order_id":"ord_1","qty":40,"price":"150.25","fee":"0.20","executed_at":"2026-01-05T14:29:00Z"}]}`)
		case r.Method == http.MethodGet:
			io.WriteString(w, `{"client_order_id":"ord 1/x","status":"FILLED","qty":100,"cum_qty":100,"updated_at":"2026-01-05T14:29:00Z"}`)
		default:
			w.WriteHeader(http.StatusAccepted)
		}
	}))
	defer srv.Close()
	c := broker.NewHTTPClient(srv.URL, "unit-test-broker-key", 2*time.Second)
	ctx := context.Background()
	lp := fixed.MustParse("150.25")

	if err := c.Submit(ctx, broker.SubmitRequest{ClientOrderID: "ord_1", Symbol: "AAPL", Side: domain.Buy, Type: domain.Limit,
		TimeInForce: domain.Day, Qty: 100, LimitPrice: &lp}); err != nil {
		t.Fatal(err)
	}
	if err := c.Cancel(ctx, "ord 1/x", "cxl_1"); err != nil {
		t.Fatal(err)
	}
	if err := c.Replace(ctx, "ord_1", "rpl_1", broker.ReplaceRequest{Qty: 80, LimitPrice: &lp}); err != nil {
		t.Fatal(err)
	}
	o, err := c.GetOrder(ctx, "ord 1/x")
	if err != nil || o.Status != broker.StatusFilled || o.CumQty != 100 {
		t.Fatalf("get order: %+v %v", o, err)
	}
	snap, err := c.Snapshot(ctx)
	if err != nil || len(snap.Orders) != 1 || len(snap.Executions) != 1 || snap.Executions[0].Price != lp ||
		*snap.Orders[0].LimitPrice != lp || snap.AsOf.IsZero() {
		t.Fatalf("snapshot: %+v %v", snap, err)
	}

	want := []seen{
		// The order id is the broker's dedupe key for submissions, so it
		// travels in the body; prices travel as exact decimal strings.
		{"POST", "/v1/orders", "", "", "application/json",
			`{"client_order_id":"ord_1","symbol":"AAPL","side":"BUY","type":"LIMIT","time_in_force":"DAY","qty":100,"limit_price":"150.25"}`},
		// Ids are escaped into the path, and the request id is the dedupe key.
		{"POST", "/v1/orders/ord%201%2Fx/cancel", "", "cxl_1", "", ""},
		{"POST", "/v1/orders/ord_1/replace", "", "rpl_1", "application/json", `{"qty":80,"limit_price":"150.25"}`},
		{"GET", "/v1/orders/ord%201%2Fx", "", "", "", ""},
		{"GET", "/v1/snapshot", "", "", "", ""},
	}
	if len(got) != len(want) {
		t.Fatalf("%d requests, want %d", len(got), len(want))
	}
	for i := range want {
		if got[i].auth != "Bearer unit-test-broker-key" {
			t.Errorf("request %d: Authorization %q", i, got[i].auth)
		}
		got[i].auth = ""
		if got[i] != want[i] {
			t.Errorf("request %d:\n got %+v\nwant %+v", i, got[i], want[i])
		}
	}
}

func TestBrokerError_MessagesNeverIncludeCredentials(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		io.WriteString(w, `{"error":{"code":"UNAUTHENTICATED","message":"bad key"}}`)
	}))
	defer srv.Close()
	const key = "unit-test-broker-key-do-not-log"
	err := broker.NewHTTPClient(srv.URL, key, time.Second).Submit(context.Background(), broker.SubmitRequest{ClientOrderID: "ord_1"})
	if err == nil || strings.Contains(err.Error(), key) {
		t.Errorf("error = %v", err)
	}
	if want := "broker submit: HTTP 401 UNAUTHENTICATED: bad key"; err.Error() != want {
		t.Errorf("error = %q, want %q", err, want)
	}
	for _, e := range []*broker.Error{
		{Op: "cancel", Status: 503},
		{Op: "cancel", Err: errors.New("dial tcp: connection refused")},
	} {
		if e.Error() == "" || strings.Contains(e.Error(), key) {
			t.Errorf("error = %q", e.Error())
		}
	}
}
