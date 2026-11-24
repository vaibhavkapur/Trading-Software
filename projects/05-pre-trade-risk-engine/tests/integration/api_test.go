package integration

import (
	"crypto/ed25519"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"riskengine/src/app"
	"riskengine/src/audit"
	"riskengine/src/domain"
	"riskengine/src/fixed"
	"riskengine/tests/harness"
)

type decision = domain.RiskDecision

func quoteBody(e *harness.Env, symbol, bid, ask string) string {
	return fmt.Sprintf(`{"symbol":%q,"bid":%s,"ask":%s,"event_time":%q}`, symbol, bid, ask, e.Clock.Now().Format(time.RFC3339Nano))
}

// The request and routes shown in the README's protocol sketch, verbatim.
func TestReadmeProtocolSketch(t *testing.T) {
	e := harness.New(t)
	_, c := e.Serve()
	if code := c.As(harness.FeedToken).Do("POST", "/v1/market/quotes", quoteBody(e, "XYZ", "105.18", "105.22"), nil); code != 204 {
		t.Fatalf("quote ingest: %d", code)
	}

	var d decision
	code := c.Do("POST", "/v1/risk/evaluate",
		`{"order_id":"o-123","account_id":"a-1","symbol":"XYZ","side":"BUY","qty":100,"limit_price":105.20}`, &d)
	if code != 200 {
		t.Fatalf("evaluate: %d", code)
	}
	harness.AssertDecision(t, d, domain.Allow)
	if d.PolicyVersion != "test-1" || d.Reservation.Notional != fixed.MustParse("10520") {
		t.Fatalf("decision: %s", harness.JSON(d))
	}

	var res domain.RiskReservation
	if code := c.Do("POST", "/v1/risk/reservations/o-123/commit", nil, &res); code != 200 || res.Status != domain.Committed {
		t.Fatalf("commit: %d %+v", code, res)
	}

	var ks map[string]any
	code = c.As(harness.AdminToken).Do("POST", "/v1/risk/kill-switches/accounts/a-1/activate",
		`{"activated_by":"risk-officer","reason":"drill"}`, &ks)
	if code != 200 || ks["durable"] != true {
		t.Fatalf("kill switch: %d %v", code, ks)
	}
	c.Do("POST", "/v1/risk/evaluate",
		`{"order_id":"o-124","account_id":"a-1","symbol":"XYZ","side":"BUY","qty":100,"limit_price":105.20}`, &d)
	harness.AssertDecision(t, d, domain.Reject, domain.KillSwitchAccount)

	var got decision
	if code := c.Do("GET", "/v1/risk/decisions/o-124", nil, &got); code != 200 || got.Hash != d.Hash {
		t.Fatalf("decision lookup: %d", code)
	}
}

func TestMalformedRequestsAreRejectedWithEvidence(t *testing.T) {
	e := harness.New(t)
	_, c := e.Serve()
	before, _ := e.App.Log.Head()
	bodies := []string{
		`{"order_id":`,
		`not json`,
		``,
		`{"order_id":"o","account_id":"a-1","symbol":"XYZ","side":"BUY","qty":1.5,"limit_price":100}`,      // fractional qty
		`{"order_id":"o","account_id":"a-1","symbol":"XYZ","side":"BUY","qty":1,"limit_price":1e2}`,        // exponent price
		`{"order_id":"o","account_id":"a-1","symbol":"XYZ","side":"BUY","qty":1,"limit_price":100.001234}`, // sub-tick precision
		`{"order_id":"o","account_id":"a-1","symbol":"XYZ","side":"BUY","qty":1,"limit_price":100,"bypass_risk":true}`,
		`{"order_id":"o","account_id":"a-1","symbol":"XYZ","side":"BUY","qty":1,"limit_price":100}{"x":1}`,
		`{"order_id":"o","account_id":"a-1","symbol":"XYZ","side":"BUY","qty":99999999999999999999,"limit_price":100}`,
		`{"order_id":"` + strings.Repeat("x", 70_000) + `"}`, // over the body limit
	}
	for _, body := range bodies {
		var out struct {
			Decision      string   `json:"decision"`
			ReasonCodes   []string `json:"reason_codes"`
			PolicyVersion string   `json:"policy_version"`
		}
		code := c.Do("POST", "/v1/risk/evaluate", body, &out)
		if code != 400 || out.Decision != "REJECT" || len(out.ReasonCodes) != 1 || out.ReasonCodes[0] != domain.MalformedRequest || out.PolicyVersion != "test-1" {
			t.Errorf("body %.60q: %d %+v", body, code, out)
		}
	}
	after, _ := e.App.Log.Head()
	if int(after-before) != len(bodies) {
		t.Errorf("audit grew by %d records, want %d", after-before, len(bodies))
	}
}

func TestPrivilegedRoutesFailClosed(t *testing.T) {
	e := harness.New(t)
	_, c := e.Serve()
	routes := []string{
		"/v1/risk/kill-switches/global/activate",
		"/v1/risk/kill-switches/global/deactivate",
		"/v1/risk/kill-switches/accounts/a-1/activate",
		"/v1/risk/kill-switches/symbols/XYZ/activate",
		"/v1/risk/reservations/o1/approve",
		"/v1/policy/reload",
		"/v1/market/quotes",
	}
	body := `{"activated_by":"mallory"}`
	for _, path := range routes {
		for name, client := range map[string]*harness.HTTP{"no token": c, "wrong token": c.As("guess"), "feed token on admin route": c.As(harness.FeedToken)} {
			if path == "/v1/market/quotes" && name == "feed token on admin route" {
				client = c.As(harness.AdminToken) // and the admin token must not open the feed
			}
			if code := client.Do("POST", path, body, nil); code != 401 {
				t.Errorf("%s with %s: status %d, want 401", path, name, code)
			}
		}
	}
	if n := len(e.App.Engine.KillSwitches()); n != 0 {
		t.Fatalf("%d kill switches set by unauthenticated callers", n)
	}

	// With no tokens configured the routes are disabled, not open.
	open, err := app.Open(app.Config{PolicyPath: e.PolicyPath(), DataDir: t.TempDir(), Key: harness.Key(), Clock: e.Clock})
	if err != nil {
		t.Fatal(err)
	}
	defer open.Close()
	srv := httptest.NewServer(open.Handler)
	defer srv.Close()
	c2 := &harness.HTTP{T: t, Base: srv.URL}
	for _, path := range routes {
		for _, client := range []*harness.HTTP{c2, c2.As("")} {
			if code := client.Do("POST", path, body, nil); code != 403 {
				t.Errorf("%s with no token configured: status %d, want 403", path, code)
			}
		}
	}
}

// Definition of done: concurrent orders cannot oversubscribe a shared
// notional limit. 300 clients race for a 150,000 symbol limit in 10,000
// lots; exactly 15 may win no matter how the requests interleave.
func TestConcurrentOrdersCannotOversubscribe(t *testing.T) {
	doc := strings.Replace(harness.DefaultPolicy, `"a-big": {"review_order_notional": "0"}`,
		`"a-big": {"review_order_notional": "0", "rate_limit": {"max_orders": 100000, "window_ms": 1000}}`, 1)
	e := harness.NewWith(t, doc)
	_, c := e.Serve()
	e.Quote("XYZ", "100", "100")

	const clients = 300
	var allowed, rejected atomic.Int64
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < clients; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			side := "BUY"
			var d decision
			c.Do("POST", "/v1/risk/evaluate", harness.Req(fmt.Sprint("c", i), "a-big", "XYZ", side, 100, "100.00"), &d)
			switch {
			case d.Decision == domain.Allow:
				allowed.Add(1)
				// Half the winners commit, as a live OMS would.
				if i%2 == 0 {
					c.Do("POST", "/v1/risk/reservations/"+d.OrderID+"/commit", nil, nil)
				}
			case d.Decision == domain.Reject && len(d.ReasonCodes) == 1 && d.ReasonCodes[0] == domain.MaxSymbolExposure:
				rejected.Add(1)
			default:
				t.Errorf("unexpected decision: %s %v", d.Decision, d.ReasonCodes)
			}
		}(i)
	}
	close(start)
	wg.Wait()

	if allowed.Load() != 15 || rejected.Load() != clients-15 {
		t.Fatalf("allowed=%d rejected=%d, want 15 and %d", allowed.Load(), rejected.Load(), clients-15)
	}
	var held fixed.Decimal
	for _, r := range e.App.Engine.AccountState("a-big").Reservations {
		held += r.Notional
	}
	if held != fixed.MustParse("150000") {
		t.Fatalf("held notional = %s, want exactly the 150000 limit", held)
	}
}

// Releases racing with new orders: capacity freed by a release may be
// re-used, but the total held must never exceed the limit at any instant.
func TestReserveReleaseChurnNeverExceedsLimit(t *testing.T) {
	doc := strings.Replace(harness.DefaultPolicy, `"a-big": {"review_order_notional": "0"}`,
		`"a-big": {"review_order_notional": "0", "rate_limit": {"max_orders": 1000000, "window_ms": 1000}}`, 1)
	e := harness.NewWith(t, doc)
	e.Quote("XYZ", "100", "100")
	limit := fixed.MustParse("150000")

	stop := make(chan struct{})
	var wg sync.WaitGroup
	for w := 0; w < 8; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := 0; ; i++ {
				select {
				case <-stop:
					return
				default:
				}
				id := fmt.Sprintf("w%d-%d", w, i)
				if d := e.Eval(id, "a-big", "XYZ", "BUY", 300, "100.00"); d.Decision == domain.Allow {
					if i%3 == 0 {
						e.App.Engine.Commit(id)
					}
					e.App.Engine.Release(id, "cancel")
				}
			}
		}(w)
	}
	for i := 0; i < 500; i++ {
		var held fixed.Decimal
		for _, r := range e.App.Engine.AccountState("a-big").Reservations {
			held += r.Notional
		}
		if held > limit {
			close(stop)
			t.Fatalf("held %s exceeds limit %s", held, limit)
		}
	}
	close(stop)
	wg.Wait()
}

func TestRestartRebuildsStateFromAuditLog(t *testing.T) {
	e := harness.New(t)
	e.Quote("XYZ", "100", "100")
	e.Quote("ABC", "50", "50")
	e.Expect(domain.Allow, nil, "o1", "a-1", "XYZ", "BUY", 200, "100.00")
	e.Fill("o1", "f1", 120, "99.90") // partly filled, still working
	e.Expect(domain.Allow, nil, "o2", "a-1", "ABC", "SELL", 100, "50.00")
	e.App.Engine.Commit("o2")
	e.Expect(domain.Review, []string{domain.ReviewOrderNotional}, "o3", "a-1", "XYZ", "BUY", 600, "100.00")
	e.Expect(domain.Allow, nil, "o4", "a-2", "XYZ", "BUY", 50, "100.00") // uncommitted lease
	e.Expect(domain.Allow, nil, "o5", "a-1", "XYZ", "SELL", 20, "100.00")
	e.Fill("o5", "f1", 20, "101.00") // realizes P&L
	e.App.Engine.Release("o4", "cancelled")
	e.App.Engine.SetKillSwitch(domain.ScopeSymbol, "ABC", true, "ops", "halt")

	type picture struct {
		A1, A2 any
		Kill   []domain.KillSwitch
		O1, O3 decision
		Head   string
	}
	snap := func() picture {
		o1, _ := e.App.Engine.Decision("o1")
		o3, _ := e.App.Engine.Decision("o3")
		return picture{e.App.Engine.AccountState("a-1"), e.App.Engine.AccountState("a-2"), e.App.Engine.KillSwitches(), o1, o3, ""}
	}
	before := harness.JSON(snap())
	seqBefore, _ := e.App.Log.Head()

	e.Restart()

	if after := harness.JSON(snap()); after != before {
		t.Fatalf("state changed across restart\nbefore: %s\nafter: %s", before, after)
	}
	if e.App.Recovered != seqBefore {
		t.Fatalf("replayed %d records, want %d", e.App.Recovered, seqBefore)
	}
	// Quotes are not persisted: until the feed is back, nothing is allowed.
	e.Expect(domain.Reject, []string{domain.NoMarketData}, "o6", "a-1", "XYZ", "BUY", 1, "100.00")
	// The kill switch and duplicate detection both survived.
	e.Quote("ABC", "50", "50")
	e.Expect(domain.Reject, []string{domain.KillSwitchSymbol}, "o7", "a-1", "ABC", "BUY", 10, "50.00")
	if d := e.Eval("o1", "a-1", "XYZ", "BUY", 200, "100.00"); !d.Replayed || d.Decision != domain.Allow {
		t.Fatalf("redelivery after restart: %+v", d)
	}
	if n, _, err := audit.Verify(e.AuditPath(), harness.Key().Public().(ed25519.PublicKey)); err != nil || n <= seqBefore {
		t.Fatalf("audit after restart: n=%d err=%v", n, err)
	}
}

func TestEngineRefusesToStartOnTamperedLogOrBadPolicy(t *testing.T) {
	e := harness.New(t)
	e.Quote("XYZ", "100", "100")
	e.Expect(domain.Reject, []string{domain.MaxOrderQty, domain.MaxOrderNotional, domain.MaxSymbolExposure, domain.MaxGrossExposure}, "o1", "a-big", "XYZ", "BUY", 5000, "100.00")
	e.App.Close()
	cfg := app.Config{PolicyPath: e.PolicyPath(), DataDir: e.Dir, Key: harness.Key(), Clock: e.Clock}

	// Rewrite history: turn the recorded REJECT into an ALLOW.
	raw, _ := os.ReadFile(e.AuditPath())
	forged := strings.Replace(string(raw), `"decision":"REJECT"`, `"decision":"ALLOW"`, 1)
	if forged == string(raw) {
		t.Fatal("fixture: nothing to forge")
	}
	os.WriteFile(e.AuditPath(), []byte(forged), 0o600)
	if _, err := app.Open(cfg); err == nil {
		t.Fatal("engine started on a forged audit log")
	}
	os.WriteFile(e.AuditPath(), raw, 0o600)

	e.WritePolicy(strings.Replace(harness.DefaultPolicy, `"max_order_qty": 1000`, `"max_order_qty": -1`, 1))
	if _, err := app.Open(cfg); err == nil {
		t.Fatal("engine started with an invalid policy")
	}
	e.WritePolicy(harness.DefaultPolicy)
	a, err := app.Open(cfg) // and starts cleanly once both are sound
	if err != nil {
		t.Fatalf("engine failed to start on a sound log and policy: %v", err)
	}
	e.App = a
}

func TestPolicyReloadIsAtomicAndVersioned(t *testing.T) {
	e := harness.New(t)
	_, c := e.Serve()
	admin := c.As(harness.AdminToken)
	e.Quote("XYZ", "100", "100")
	e.Expect(domain.Allow, nil, "p1", "a-big", "XYZ", "BUY", 900, "100.00")
	e.App.Engine.Release("p1", "")

	// A broken edit is refused and the old limits stay in force.
	e.WritePolicy(strings.Replace(harness.DefaultPolicy, `"max_order_qty": 1000`, `"max_order_qty": "lots"`, 1))
	if code := admin.Do("POST", "/v1/policy/reload", nil, nil); code != 422 {
		t.Fatalf("bad reload: %d", code)
	}
	if d := e.Expect(domain.Allow, nil, "p2", "a-big", "XYZ", "BUY", 900, "100.00"); d.PolicyVersion != "test-1" {
		t.Fatalf("version after failed reload = %s", d.PolicyVersion)
	}
	e.App.Engine.Release("p2", "")

	tighter := strings.NewReplacer(`"version": "test-1"`, `"version": "test-2"`, `"max_order_qty": 1000`, `"max_order_qty": 500`).Replace(harness.DefaultPolicy)
	e.WritePolicy(tighter)
	var out map[string]string
	if code := admin.Do("POST", "/v1/policy/reload", nil, &out); code != 200 || out["policy_version"] != "test-2" {
		t.Fatalf("reload: %d %v", code, out)
	}
	d := e.Expect(domain.Reject, []string{domain.MaxOrderQty}, "p3", "a-big", "XYZ", "BUY", 900, "100.00")
	old, _ := e.App.Engine.Decision("p1")
	if d.PolicyVersion != "test-2" || d.PolicyHash == old.PolicyHash || old.PolicyVersion != "test-1" {
		t.Fatalf("decisions must record the policy they were made under: new=%s old=%s", d.PolicyVersion, old.PolicyVersion)
	}
}

func TestKillSwitchPropagatesToOMSWithRetries(t *testing.T) {
	var calls atomic.Int64
	got := make(chan string, 4)
	oms := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) <= 2 { // OMS is briefly unavailable
			http.Error(w, "busy", http.StatusServiceUnavailable)
			return
		}
		var m struct {
			KillSwitch domain.KillSwitch `json:"kill_switch"`
			Seq        uint64            `json:"audit_seq"`
		}
		if err := jsonDecode(r, &m); err != nil || m.Seq == 0 {
			t.Errorf("webhook payload: %v seq=%d", err, m.Seq)
		}
		got <- fmt.Sprintf("%s enabled=%v by=%s", m.KillSwitch.Key(), m.KillSwitch.Enabled, m.KillSwitch.ActivatedBy)
	}))
	defer oms.Close()

	e := harness.NewWith(t, harness.DefaultPolicy, oms.URL)
	_, c := e.Serve()
	if code := c.As(harness.AdminToken).Do("POST", "/v1/risk/kill-switches/global/activate", `{"activated_by":"ops"}`, nil); code != 200 {
		t.Fatalf("activate: %d", code)
	}
	select {
	case msg := <-got:
		if msg != "global: enabled=true by=ops" {
			t.Fatalf("OMS received %q", msg)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("OMS never received the kill switch")
	}
	e.App.Notifier.Wait()

	var view struct {
		Active      []domain.KillSwitch `json:"active"`
		Propagation []struct {
			Attempts int        `json:"attempts"`
			AckedAt  *time.Time `json:"acked_at"`
		} `json:"propagation"`
	}
	c.Do("GET", "/v1/risk/kill-switches", nil, &view)
	if len(view.Active) != 1 || len(view.Propagation) != 1 || view.Propagation[0].Attempts != 3 || view.Propagation[0].AckedAt == nil {
		t.Fatalf("propagation status: %+v", view)
	}
	if e.App.Metrics.HistCount("risk_kill_switch_propagation_seconds") != 1 ||
		e.App.Metrics.Counter("risk_kill_switch_propagation_failures_total") != 2 {
		t.Fatal("propagation delay/failure metrics not recorded")
	}
}

func TestReconciliationDriftAndMetrics(t *testing.T) {
	e := harness.New(t)
	_, c := e.Serve()
	e.Quote("XYZ", "100", "100")
	e.Expect(domain.Allow, nil, "m1", "a-1", "XYZ", "BUY", 100, "100.00")
	e.Fill("m1", "f", 100, "100.00")
	e.Expect(domain.Reject, []string{domain.MaxOrderQty, domain.MaxOrderNotional, domain.MaxSymbolExposure, domain.MaxGrossExposure}, "m2", "a-1", "XYZ", "BUY", 5000, "100.00")

	var rec domain.ReconcileEvent
	// Broker says 90 XYZ (we think 100) and 10 ABC we know nothing about.
	code := c.Do("POST", "/v1/risk/reconcile", `{"account_id":"a-1","positions":{"XYZ":90,"ABC":10}}`, &rec)
	if code != 200 || rec.Drift["XYZ"] != 10 || rec.Drift["ABC"] != -10 || rec.AbsDrift != 20 {
		t.Fatalf("reconcile: %d %+v", code, rec)
	}
	// Reconciliation reports; it does not rewrite positions.
	if q := e.App.Engine.AccountState("a-1").Positions["XYZ"].Qty; q != 100 {
		t.Fatalf("position changed to %d", q)
	}
	c.Do("POST", "/v1/risk/reconcile", `{"account_id":"a-1","positions":{"XYZ":100}}`, &rec)
	if rec.AbsDrift != 0 {
		t.Fatalf("drift after matching positions: %+v", rec)
	}

	resp, err := http.Get(c.Base + "/metrics")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	buf := new(strings.Builder)
	fmt.Fprint(buf, readAll(resp))
	for _, want := range []string{
		`risk_decisions_total{decision="ALLOW"} 1`,
		`risk_decisions_total{decision="REJECT"} 1`,
		`risk_decision_reasons_total{decision="REJECT",reason="MAX_ORDER_QTY"} 1`,
		`risk_state_drift_abs_qty{account="a-1"} 0`,
		`risk_decision_duration_seconds_count 2`,
		`risk_decision_duration_seconds_bucket{le="+Inf"} 2`,
	} {
		if !strings.Contains(buf.String(), want) {
			t.Errorf("metrics missing %q\n%s", want, buf.String())
		}
	}
}

func TestQuoteIngestOverHTTP(t *testing.T) {
	e := harness.New(t)
	_, c := e.Serve()
	feed := c.As(harness.FeedToken)
	if code := feed.Do("POST", "/v1/market/quotes", quoteBody(e, "XYZ", "100.00", "100.04"), nil); code != 204 {
		t.Fatalf("good quote: %d", code)
	}
	bad := map[string]string{
		"crossed":      quoteBody(e, "XYZ", "100.10", "100.04"),
		"float syntax": fmt.Sprintf(`{"symbol":"XYZ","bid":1e2,"ask":101,"event_time":%q}`, e.Clock.Now().Format(time.RFC3339)),
		"older":        fmt.Sprintf(`{"symbol":"XYZ","bid":90,"ask":91,"event_time":%q}`, e.Clock.Now().Add(-time.Second).Format(time.RFC3339)),
		"future":       fmt.Sprintf(`{"symbol":"XYZ","bid":90,"ask":91,"event_time":%q}`, e.Clock.Now().Add(time.Minute).Format(time.RFC3339)),
	}
	for name, body := range bad {
		if code := feed.Do("POST", "/v1/market/quotes", body, nil); code != 422 && code != 400 {
			t.Errorf("%s quote accepted: %d", name, code)
		}
	}
	// The collar still references the one good quote (mid 100.02).
	e.Expect(domain.Allow, nil, "h1", "a-big", "XYZ", "BUY", 1, "100.02")
	e.Expect(domain.Reject, []string{domain.PriceCollar}, "h2", "a-big", "XYZ", "BUY", 1, "90.50")
}

// The whole reservation lifecycle and its error codes, over HTTP.
func TestReservationLifecycleOverHTTP(t *testing.T) {
	e := harness.New(t)
	_, c := e.Serve()
	admin := c.As(harness.AdminToken)
	e.Quote("XYZ", "100", "100")
	type errBody struct {
		Code        string                 `json:"code"`
		Reservation domain.RiskReservation `json:"reservation"`
	}
	var res domain.RiskReservation
	var eb errBody

	// Unknown order.
	for path, body := range map[string]string{"commit": "", "release": "",
		"fills": `{"fill_id":"f","qty":1,"price":100}`, "approve": `{"approved_by":"x"}`} {
		eb = errBody{}
		if code := admin.Do("POST", "/v1/risk/reservations/nope/"+path, body, &eb); code != 404 || eb.Code != "ORDER_NOT_FOUND" {
			t.Errorf("%s on unknown order: %d %s", path, code, eb.Code)
		}
	}

	// A rejected order has nothing to commit.
	e.Expect(domain.Reject, []string{domain.PriceCollar}, "rej", "a-1", "XYZ", "BUY", 1, "150.00")
	if code := c.Do("POST", "/v1/risk/reservations/rej/commit", nil, &eb); code != 409 || eb.Code != "NO_RESERVATION" {
		t.Errorf("commit of rejected order: %d %s", code, eb.Code)
	}

	// REVIEW -> approve -> commit -> partial fill -> release remainder.
	e.Expect(domain.Review, []string{domain.ReviewOrderNotional}, "big", "a-1", "XYZ", "BUY", 600, "100.00")
	if code := c.Do("POST", "/v1/risk/reservations/big/commit", nil, &eb); code != 409 || eb.Code != "PENDING_REVIEW" {
		t.Errorf("commit before approval: %d %s", code, eb.Code)
	}
	if code := admin.Do("POST", "/v1/risk/reservations/big/approve", `{}`, &eb); code != 400 || eb.Code != "INVALID_ARGUMENT" {
		t.Errorf("approval with no approver: %d %s", code, eb.Code)
	}
	if code := admin.Do("POST", "/v1/risk/reservations/big/approve", `{"approved_by":"risk-officer"}`, &res); code != 200 || res.Status != domain.Reserved {
		t.Fatalf("approve: %d %+v", code, res)
	}
	if code := admin.Do("POST", "/v1/risk/reservations/big/approve", `{"approved_by":"risk-officer"}`, &eb); code != 409 || eb.Code != "NOT_PENDING_REVIEW" {
		t.Errorf("second approval: %d %s", code, eb.Code)
	}
	if code := c.Do("POST", "/v1/risk/reservations/big/commit", nil, &res); code != 200 || res.Status != domain.Committed {
		t.Fatalf("commit: %d %+v", code, res)
	}
	var fill struct {
		Duplicate   bool                   `json:"duplicate"`
		Reservation domain.RiskReservation `json:"reservation"`
		Position    struct {
			Qty int64 `json:"qty"`
		} `json:"position"`
	}
	for i, wantDup := range []bool{false, true} { // delivered twice
		code := c.Do("POST", "/v1/risk/reservations/big/fills", `{"fill_id":"f1","qty":250,"price":99.95}`, &fill)
		if code != 200 || fill.Duplicate != wantDup || fill.Position.Qty != 250 || fill.Reservation.Remaining != 350 {
			t.Fatalf("fill delivery %d: %d %+v", i, code, fill)
		}
	}
	for name, body := range map[string]string{"zero qty": `{"fill_id":"f2","qty":0,"price":100}`, "no id": `{"qty":1,"price":100}`,
		"no price": `{"fill_id":"f2","qty":1}`, "float price": `{"fill_id":"f2","qty":1,"price":1e2}`, "extra field": `{"fill_id":"f2","qty":1,"price":100,"x":1}`} {
		if code := c.Do("POST", "/v1/risk/reservations/big/fills", body, nil); code != 400 {
			t.Errorf("bad fill (%s): %d", name, code)
		}
	}
	if code := c.Do("POST", "/v1/risk/reservations/big/release", `{"reason":"cancelled"}`, &res); code != 200 || res.Status != domain.Released || res.ReleaseReason != "cancelled" {
		t.Fatalf("release: %d %+v", code, res)
	}
	if code := c.Do("POST", "/v1/risk/reservations/big/release", nil, &res); code != 200 || res.ReleaseReason != "cancelled" {
		t.Errorf("repeat release must be a no-op: %d %+v", code, res)
	}
	if code := c.Do("POST", "/v1/risk/reservations/big/commit", nil, &eb); code != 409 || eb.Code != "RESERVATION_RELEASED" {
		t.Errorf("commit after release: %d %s", code, eb.Code)
	}

	// A lease that lapses before the OMS commits.
	e.Expect(domain.Allow, nil, "slow", "a-1", "XYZ", "BUY", 1, "100.00")
	e.Clock.Advance(31 * time.Second)
	if code := c.Do("POST", "/v1/risk/reservations/slow/commit", nil, &eb); code != 409 || eb.Code != "RESERVATION_EXPIRED" || eb.Reservation.Status != domain.Released {
		t.Errorf("late commit: %d %+v", code, eb)
	}

	var acct struct {
		Positions map[string]struct {
			Qty int64 `json:"qty"`
		} `json:"positions"`
		Open []domain.RiskReservation `json:"open_reservations"`
	}
	if code := c.Do("GET", "/v1/risk/accounts/a-1", nil, &acct); code != 200 || acct.Positions["XYZ"].Qty != 250 || len(acct.Open) != 0 {
		t.Errorf("account view: %d %+v", code, acct)
	}

	// Kill switch lifecycle and audit endpoints.
	if code := admin.Do("POST", "/v1/risk/kill-switches/symbols/XYZ/pause", `{"activated_by":"ops"}`, nil); code != 404 {
		t.Errorf("unknown kill-switch action: %d", code)
	}
	if code := admin.Do("POST", "/v1/risk/kill-switches/symbols/XYZ/activate", `{}`, nil); code != 400 {
		t.Errorf("kill switch with no activated_by: %d", code)
	}
	admin.Do("POST", "/v1/risk/kill-switches/symbols/XYZ/activate", `{"activated_by":"ops"}`, nil)
	admin.Do("POST", "/v1/risk/kill-switches/symbols/XYZ/deactivate", `{"activated_by":"ops"}`, nil)
	if n := len(e.App.Engine.KillSwitches()); n != 0 {
		t.Errorf("%d switches active after deactivate", n)
	}
	var verify struct {
		Valid   bool   `json:"valid"`
		Records uint64 `json:"records"`
	}
	var head struct {
		Seq       uint64 `json:"seq"`
		PublicKey string `json:"public_key"`
	}
	c.Do("GET", "/v1/audit/head", nil, &head)
	if code := admin.Do("GET", "/v1/audit/verify", nil, &verify); code != 200 || !verify.Valid || verify.Records != head.Seq || len(head.PublicKey) != 64 {
		t.Errorf("audit verify: %d %+v head=%+v", code, verify, head)
	}
	if code := c.Do("GET", "/v1/audit/verify", nil, nil); code != 401 {
		t.Errorf("audit verify without token: %d", code)
	}
	var health map[string]string
	if code := c.Do("GET", "/healthz", nil, &health); code != 200 || health["policy_version"] != "test-1" {
		t.Errorf("healthz: %d %v", code, health)
	}
	if code := c.Do("GET", "/v1/risk/decisions/never-seen", nil, nil); code != 404 {
		t.Errorf("unknown decision: %d", code)
	}
}
