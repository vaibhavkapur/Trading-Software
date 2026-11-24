// Package harness builds a fully wired engine on a temp directory with a
// controllable clock, so every test run is reproducible.
package harness

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"surveillance/src/api"
	"surveillance/src/domain"
	"surveillance/src/engine"
	"surveillance/src/normalize"
	"surveillance/src/rules"
)

// T0 is the event-time origin used by tests: a Monday, 15:00 UTC.
var T0 = time.Date(2026, 1, 5, 15, 0, 0, 0, time.UTC)

const (
	IngestToken = "test-ingest-token-000001"
	AdminToken  = "test-admin-token-0000001"
	AliceToken  = "test-analyst-alice-00001"
	BobToken    = "test-analyst-bob-0000001"
)

// At returns T0 plus a number of milliseconds.
func At(ms int) time.Time { return T0.Add(time.Duration(ms) * time.Millisecond) }

// Clock is a manual clock. It starts an hour after T0 so that test events
// stamped near T0 are in the past.
type Clock struct {
	mu sync.Mutex
	t  time.Time
}

func NewClock() *Clock { return &Clock{t: T0.Add(time.Hour)} }

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

// DefaultRules mirrors examples/rules.json.
func DefaultRules() rules.Config {
	var c rules.Config
	c.Version = "test-1"
	c.Window = rules.Window{SizeS: 60, HopS: 10, AllowedLatenessMs: 5000, MaxFutureSkewMs: 2000, IdleTimeoutMs: 3000}
	c.Suppression = rules.Suppression{CooldownS: 300, MaxEvidenceRefs: 500}
	c.Rules.HighCancelRate = rules.HighCancelRateRule{Version: 1, Enabled: true, MinNewOrders: 20, MinCancelRatePct: 90,
		MediumMinNewOrders: 50, HighMinNewOrders: 150}
	c.Rules.HighOrderTradeRatio = rules.HighOrderTradeRatioRule{Version: 1, Enabled: true, MinMessages: 40, MinRatio: 20,
		MediumMinRatio: 50, HighMinRatio: 100}
	c.Rules.LayeringLike = rules.LayeringLikeRule{Version: 1, Enabled: true, LookbackS: 30, CancelWithinS: 10,
		MinLayerOrders: 3, MinPriceLevels: 3, MinCancelPct: 80, MinImbalanceRatio: 4,
		MediumMinImbalanceRatio: 8, HighMinImbalanceRatio: 15}
	c.Rules.WashTradeLike = rules.WashTradeLikeRule{Version: 1, Enabled: true, LookbackS: 300, MinSelfTrades: 2,
		MinPairTrades: 4, MinOffsetPct: 90, MaxPriceDiffBps: 10, MediumMinTrades: 6, HighMinTrades: 12}
	return c
}

// RefData used by tests: two venues, and two accounts sharing an owner.
const RefData = `{
  "venues": {"XSIM": {"aliases": ["SIM"]}, "XALT": {"aliases": []}},
  "accounts": {
    "ACC-1001": {"aliases": ["1001"], "owner": "OWNER-A"},
    "ACC-1002": {"aliases": ["1002"], "owner": "OWNER-A"}
  },
  "symbols": {"SIMA": {"aliases": ["SIMA.XSIM"]}}
}`

type Env struct {
	T         testing.TB
	Dir       string
	RulesPath string
	RefPath   string
	Clock     *Clock
	Engine    *engine.Engine
	Server    *httptest.Server
	EnableSim bool
}

type Option func(*Env, *rules.Config)

// WithRules edits the ruleset before the engine starts.
func WithRules(fn func(*rules.Config)) Option {
	return func(_ *Env, c *rules.Config) { fn(c) }
}

// WithSim enables the simulator routes.
func WithSim() Option { return func(e *Env, _ *rules.Config) { e.EnableSim = true } }

// New starts an engine and an HTTP server on a fresh temp directory.
func New(t testing.TB, opts ...Option) *Env {
	t.Helper()
	dir := t.TempDir()
	e := &Env{T: t, Dir: filepath.Join(dir, "data"), RulesPath: filepath.Join(dir, "rules.json"),
		RefPath: filepath.Join(dir, "refdata.json"), Clock: NewClock()}
	cfg := DefaultRules()
	for _, o := range opts {
		o(e, &cfg)
	}
	e.WriteRules(cfg)
	if err := os.WriteFile(e.RefPath, []byte(RefData), 0o600); err != nil {
		t.Fatal(err)
	}
	e.start()
	t.Cleanup(e.stop)
	return e
}

// WriteRules replaces the ruleset file (it takes effect on reload or restart).
func (e *Env) WriteRules(c rules.Config) {
	e.T.Helper()
	b, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		e.T.Fatal(err)
	}
	if err := os.WriteFile(e.RulesPath, b, 0o600); err != nil {
		e.T.Fatal(err)
	}
}

func (e *Env) start() {
	e.T.Helper()
	eng, err := engine.Open(engine.Config{DataDir: e.Dir, RulesPath: e.RulesPath, RefDataPath: e.RefPath, Clock: e.Clock.Now})
	if err != nil {
		e.T.Fatalf("engine.Open: %v", err)
	}
	e.Engine = eng
	e.Server = httptest.NewServer(api.New(api.Config{Engine: eng, IngestToken: IngestToken, AdminToken: AdminToken,
		Analysts: map[string]string{AliceToken: "alice", BobToken: "bob"}, EnableSim: e.EnableSim}))
}

func (e *Env) stop() {
	if e.Server != nil {
		e.Server.Close()
		e.Server = nil
	}
	if e.Engine != nil {
		e.Engine.Close()
		e.Engine = nil
	}
}

// Restart closes the engine and opens a new one on the same files, as a
// process restart would.
func (e *Env) Restart() {
	e.T.Helper()
	e.stop()
	e.start()
}

// Ingest sends events straight to the engine and fails the test on error.
func (e *Env) Ingest(raws ...normalize.Raw) engine.IngestResult {
	e.T.Helper()
	res, err := e.Engine.Ingest(raws, false)
	if err != nil {
		e.T.Fatalf("ingest: %v", err)
	}
	return res
}

// MustAccept ingests and insists that nothing was refused, late or duplicate.
func (e *Env) MustAccept(raws ...normalize.Raw) engine.IngestResult {
	e.T.Helper()
	res := e.Ingest(raws...)
	if res.Accepted != len(raws) {
		e.T.Fatalf("expected %d accepted, got %+v", len(raws), res)
	}
	return res
}

func (e *Env) Flush() engine.FlushResult {
	e.T.Helper()
	res, err := e.Engine.Flush()
	if err != nil {
		e.T.Fatalf("flush: %v", err)
	}
	return res
}

// Alerts lists every alert, oldest first.
func (e *Env) Alerts() []domain.Alert {
	list := e.Engine.Alerts(engine.AlertFilter{})
	for i, j := 0, len(list)-1; i < j; i, j = i+1, j-1 {
		list[i], list[j] = list[j], list[i]
	}
	return list
}

// AlertsOf lists alerts raised by one rule.
func (e *Env) AlertsOf(rule string) []domain.Alert {
	var out []domain.Alert
	for _, a := range e.Alerts() {
		if a.RuleID == rule {
			out = append(out, a)
		}
	}
	return out
}

// Do sends an HTTP request and returns the status and body.
func (e *Env) Do(method, path, token string, body any) (int, []byte) {
	e.T.Helper()
	var rd io.Reader
	switch b := body.(type) {
	case nil:
	case string:
		rd = bytes.NewReader([]byte(b))
	case []byte:
		rd = bytes.NewReader(b)
	default:
		raw, err := json.Marshal(b)
		if err != nil {
			e.T.Fatal(err)
		}
		rd = bytes.NewReader(raw)
	}
	req, err := http.NewRequest(method, e.Server.URL+path, rd)
	if err != nil {
		e.T.Fatal(err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		e.T.Fatal(err)
	}
	defer res.Body.Close()
	out, err := io.ReadAll(res.Body)
	if err != nil {
		e.T.Fatal(err)
	}
	return res.StatusCode, out
}

// JSON sends a request, requires the given status and decodes the reply.
func (e *Env) JSON(method, path, token string, body any, want int, out any) {
	e.T.Helper()
	status, raw := e.Do(method, path, token, body)
	if status != want {
		e.T.Fatalf("%s %s: status %d, want %d: %s", method, path, status, want, raw)
	}
	if out != nil {
		if err := json.Unmarshal(raw, out); err != nil {
			e.T.Fatalf("%s %s: decoding %s: %v", method, path, raw, err)
		}
	}
}

// ---- event builders ----

var counter struct {
	sync.Mutex
	n int
}

func next(prefix string) string {
	counter.Lock()
	defer counter.Unlock()
	counter.n++
	return fmt.Sprintf("%s%d", prefix, counter.n)
}

func stamp(t time.Time) string { return t.UTC().Format(time.RFC3339Nano) }

// Order builds an order message on venue XSIM in symbol SIMA with a
// unique source event ID.
func Order(acct, orderID, action, side string, qty int, price string, at time.Time) normalize.Raw {
	r := normalize.Raw{Type: "order", EventID: next("e"), Venue: "XSIM", Symbol: "SIMA", TS: stamp(at),
		AccountID: acct, OrderID: orderID, Action: action, Side: side}
	if qty > 0 {
		r.Qty = normalize.Lit(fmt.Sprint(qty))
	}
	if price != "" {
		r.Price = normalize.Lit(price)
	}
	return r
}

func NewOrder(acct, orderID, side string, qty int, price string, at time.Time) normalize.Raw {
	return Order(acct, orderID, "NEW", side, qty, price, at)
}

func Cancel(acct, orderID string, at time.Time) normalize.Raw {
	return Order(acct, orderID, "CANCEL", "", 0, "", at)
}

// Trade builds an execution with a unique trade ID.
func Trade(buyAcct, buyOrder, sellAcct, sellOrder string, qty int, price string, at time.Time) normalize.Raw {
	return normalize.Raw{Type: "trade", Venue: "XSIM", Symbol: "SIMA", TS: stamp(at), TradeID: next("t"),
		BuyAccount: buyAcct, SellAccount: sellAcct, BuyOrderID: buyOrder, SellOrderID: sellOrder,
		Qty: normalize.Lit(fmt.Sprint(qty)), Price: normalize.Lit(price)}
}

// Layering builds the textbook pattern for account acct starting at
// start: n sell orders of layerQty at rising prices, a buy of execQty
// that trades, then cancels of the first `pull` sell orders, the first
// one cancelAfter after the trade.
func Layering(acct string, start time.Time, n, layerQty, execQty, pull int, cancelAfter time.Duration) []normalize.Raw {
	var evs []normalize.Raw
	tag := next("L")
	for i := 0; i < n; i++ {
		evs = append(evs, NewOrder(acct, fmt.Sprintf("%s-s%d", tag, i), "SELL", layerQty,
			fmt.Sprintf("100.%02d", 10+i), start.Add(time.Duration(i)*200*time.Millisecond)))
	}
	exec := start.Add(time.Duration(n)*200*time.Millisecond + time.Second)
	evs = append(evs,
		NewOrder(acct, tag+"-b", "BUY", execQty, "100.00", exec.Add(-100*time.Millisecond)),
		NewOrder("CP-"+tag, tag+"-cp", "SELL", execQty, "100.00", exec.Add(-time.Millisecond)),
		Trade(acct, tag+"-b", "CP-"+tag, tag+"-cp", execQty, "100.00", exec))
	for i := 0; i < pull; i++ {
		evs = append(evs, Cancel(acct, fmt.Sprintf("%s-s%d", tag, i), exec.Add(cancelAfter+time.Duration(i)*100*time.Millisecond)))
	}
	return evs
}

// Burst builds n new orders for acct, 100ms apart from start, and cancels
// the first `cancelled` of them 50ms after entry.
func Burst(acct string, start time.Time, n, cancelled int) []normalize.Raw {
	var evs []normalize.Raw
	tag := next("B")
	for i := 0; i < n; i++ {
		at := start.Add(time.Duration(i) * 100 * time.Millisecond)
		id := fmt.Sprintf("%s-o%d", tag, i)
		evs = append(evs, NewOrder(acct, id, "BUY", 100, "100.00", at))
		if i < cancelled {
			evs = append(evs, Cancel(acct, id, at.Add(50*time.Millisecond)))
		}
	}
	return evs
}
