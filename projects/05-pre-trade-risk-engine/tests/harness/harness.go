// Package harness builds a fully wired engine on a temp directory with a
// controllable clock and a fixed signing key, so every test run is
// reproducible down to the audit hashes.
package harness

import (
	"bytes"
	"crypto/ed25519"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"riskengine/src/app"
	"riskengine/src/domain"
	"riskengine/src/fixed"
	"riskengine/src/market"
)

// T0 is a Monday, 15:00 UTC.
var T0 = time.Date(2026, 1, 5, 15, 0, 0, 0, time.UTC)

const (
	AdminToken = "test-admin-token"
	FeedToken  = "test-feed-token"
)

// DefaultPolicy: a-1 is typical, a-2 is tight, a-big has no review
// threshold so limit boundaries can be probed without REVIEW getting in
// the way.
const DefaultPolicy = `{
  "version": "test-1",
  "defaults": {
    "max_order_qty": 1000,
    "max_order_notional": "100000.00",
    "review_order_notional": "50000.00",
    "max_symbol_exposure": "150000.00",
    "max_gross_exposure": "250000.00",
    "max_daily_loss": "5000.00",
    "rate_limit": {"max_orders": 20, "window_ms": 1000},
    "quote_max_age_ms": 2000,
    "max_clock_skew_ms": 500,
    "price_collar_bps": 500,
    "reservation_ttl_ms": 30000,
    "review_ttl_ms": 300000
  },
  "accounts": {
    "a-1": {},
    "a-2": {"max_order_qty": 200, "max_symbol_exposure": "40000.00", "rate_limit": {"max_orders": 5, "window_ms": 1000}},
    "a-big": {"review_order_notional": "0"}
  },
  "symbols": {
    "XYZ": {"tradable": true, "lot_size": 1, "tick_size": "0.01"},
    "ABC": {"tradable": true, "lot_size": 10, "tick_size": "0.05", "price_collar_bps": 200},
    "HALT": {"tradable": false, "lot_size": 1, "tick_size": "0.01"}
  }
}`

type Clock struct {
	mu sync.Mutex
	t  time.Time
}

func NewClock() *Clock { return &Clock{t: T0} }

func (c *Clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *Clock) Advance(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

func (c *Clock) Set(t time.Time) {
	c.mu.Lock()
	c.t = t
	c.mu.Unlock()
}

// Key is a fixed, test-only signing key.
func Key() ed25519.PrivateKey {
	return ed25519.NewKeyFromSeed(bytes.Repeat([]byte{7}, ed25519.SeedSize))
}

type Env struct {
	T        testing.TB
	Dir      string
	Clock    *Clock
	App      *app.App
	Webhooks []string
}

func New(t testing.TB) *Env { return NewWith(t, DefaultPolicy) }

func NewWith(t testing.TB, policyJSON string, webhooks ...string) *Env {
	t.Helper()
	e := &Env{T: t, Dir: t.TempDir(), Clock: NewClock(), Webhooks: webhooks}
	e.WritePolicy(policyJSON)
	e.open()
	t.Cleanup(func() { e.App.Close() })
	return e
}

func (e *Env) PolicyPath() string { return filepath.Join(e.Dir, "policy.json") }
func (e *Env) AuditPath() string  { return filepath.Join(e.Dir, "audit.log") }

func (e *Env) WritePolicy(policyJSON string) {
	if err := os.WriteFile(e.PolicyPath(), []byte(policyJSON), 0o600); err != nil {
		e.T.Fatal(err)
	}
}

func (e *Env) open() {
	a, err := app.Open(app.Config{PolicyPath: e.PolicyPath(), DataDir: e.Dir, Key: Key(), Clock: e.Clock,
		AdminToken: AdminToken, FeedToken: FeedToken, Webhooks: e.Webhooks})
	if err != nil {
		e.T.Fatalf("open engine: %v", err)
	}
	e.App = a
}

// Restart closes the engine and reopens it from the audit log alone.
// Quotes are deliberately not persisted, so they are gone afterwards.
func (e *Env) Restart() {
	e.T.Helper()
	if err := e.App.Close(); err != nil {
		e.T.Fatal(err)
	}
	e.open()
}

// Quote publishes a quote stamped with the current fake time.
func (e *Env) Quote(symbol, bid, ask string) {
	e.T.Helper()
	now := e.Clock.Now()
	q := market.Quote{Symbol: symbol, Bid: fixed.MustParse(bid), Ask: fixed.MustParse(ask), EventTime: now}
	if err := e.App.Market.Update(q, now); err != nil {
		e.T.Fatalf("quote %s: %v", symbol, err)
	}
}

func Req(id, account, symbol, side string, qty int64, price string) domain.RiskRequest {
	p := fixed.MustParse(price)
	return domain.RiskRequest{OrderID: id, AccountID: account, Symbol: symbol, Side: domain.Side(side), Qty: qty, LimitPrice: &p}
}

func (e *Env) Eval(id, account, symbol, side string, qty int64, price string) domain.RiskDecision {
	return e.App.Engine.Evaluate(Req(id, account, symbol, side, qty, price))
}

// Expect evaluates an order and asserts the decision and exact reason codes.
func (e *Env) Expect(want domain.Result, reasons []string, id, account, symbol, side string, qty int64, price string) domain.RiskDecision {
	e.T.Helper()
	d := e.Eval(id, account, symbol, side, qty, price)
	AssertDecision(e.T, d, want, reasons...)
	return d
}

func AssertDecision(t testing.TB, d domain.RiskDecision, want domain.Result, reasons ...string) {
	t.Helper()
	if reasons == nil {
		reasons = []string{}
	}
	if d.Decision != want || !equal(d.ReasonCodes, reasons) {
		t.Fatalf("order %s: got %s %v, want %s %v\ntrace: %s", d.OrderID, d.Decision, d.ReasonCodes, want, reasons, JSON(d.Trace))
	}
}

func equal(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// Fill commits and fully or partly fills an allowed order.
func (e *Env) Fill(orderID, fillID string, qty int64, price string) {
	e.T.Helper()
	if _, err := e.App.Engine.Fill(orderID, fillID, qty, fixed.MustParse(price)); err != nil {
		e.T.Fatalf("fill %s/%s: %v", orderID, fillID, err)
	}
}

func JSON(v any) string {
	b, _ := json.MarshalIndent(v, "", "  ")
	return string(b)
}

// HTTP is a tiny JSON client for an httptest server.
type HTTP struct {
	T     testing.TB
	Base  string
	Token string
}

func (e *Env) Serve() (*httptest.Server, *HTTP) {
	srv := httptest.NewServer(e.App.Handler)
	e.T.Cleanup(srv.Close)
	return srv, &HTTP{T: e.T, Base: srv.URL}
}

func (h *HTTP) As(token string) *HTTP { return &HTTP{T: h.T, Base: h.Base, Token: token} }

// Do sends body (a string of JSON, or any value to marshal) and decodes the
// response into out when out is non-nil. It returns the status code.
func (h *HTTP) Do(method, path string, body any, out any) int {
	h.T.Helper()
	var rd *bytes.Reader
	switch b := body.(type) {
	case nil:
		rd = bytes.NewReader(nil)
	case string:
		rd = bytes.NewReader([]byte(b))
	default:
		raw, err := json.Marshal(b)
		if err != nil {
			h.T.Fatal(err)
		}
		rd = bytes.NewReader(raw)
	}
	req, err := http.NewRequest(method, h.Base+path, rd)
	if err != nil {
		h.T.Fatal(err)
	}
	if h.Token != "" {
		req.Header.Set("Authorization", "Bearer "+h.Token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		h.T.Fatal(err)
	}
	defer resp.Body.Close()
	if out != nil {
		if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
			h.T.Fatalf("%s %s: decoding %d response: %v", method, path, resp.StatusCode, err)
		}
	}
	return resp.StatusCode
}
