// Package harness builds a complete OMS for a test: a fresh database, the
// real HTTP API, the real outbox worker and reconciler, and a simulated
// broker reached over real HTTP in both directions.
//
// By default nothing runs in the background and the clock is frozen. A test
// moves the system forward one deliberate step at a time (Drain the outbox,
// Flush the broker's webhooks, Advance the clock, Reconcile), which is what
// makes failure scenarios exact and repeatable.
//
// Every Env checks, when its test ends, that the projections in the database
// still match a replay of the event log.
package harness

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"oms/src/app"
	"oms/src/auth"
	"oms/src/broker"
	"oms/src/brokersim"
	"oms/src/clock"
	"oms/src/devpg"
	"oms/src/domain"
	"oms/src/fixed"
	"oms/src/ids"
	"oms/src/outbox"
	"oms/src/reconcile"
	"oms/src/risk"
	"oms/src/service"
	"oms/src/store"
)

// adminURL is the server on which each test creates its own database.
var adminURL string

// Main is the body of a package's TestMain. It uses the server named by
// OMS_TEST_DATABASE_URL (as CI does) or starts a throwaway one.
func Main(m *testing.M) {
	os.Exit(run(m))
}

func run(m *testing.M) int {
	if adminURL = os.Getenv("OMS_TEST_DATABASE_URL"); adminURL == "" {
		pg, err := devpg.Start(devpg.Options{})
		if err != nil {
			fmt.Fprintln(os.Stderr, "harness: cannot start PostgreSQL:", err)
			return 1
		}
		defer pg.Stop()
		adminURL = pg.URL
	}
	return m.Run()
}

// Start is the frozen clock's initial time: a Monday, mid-session, UTC.
var Start = time.Date(2026, 1, 5, 14, 30, 0, 0, time.UTC)

// Accounts and symbols the default configuration knows.
const (
	Account1 = "ACC-1" // alice (trader), vera (viewer)
	Account2 = "ACC-2" // alice (trader)
	Account3 = "ACC-3" // bob (trader)

	// UnknownSymbol passes the OMS risk limits but is not tradable at the
	// simulated broker.
	UnknownSymbol = "ZZZZ"
)

// Options adjusts an Env.
type Options struct {
	// Auto runs the simulated broker in auto mode (it acknowledges and fills
	// by itself) and delivers webhooks as they are produced.
	Auto bool
	// Background runs the outbox worker continuously.
	Background bool
	// RealClock uses the wall clock instead of the frozen one.
	RealClock bool
	// SequentialIDs makes order, issue and run ids predictable.
	SequentialIDs bool
	Outbox        outbox.Config
	Reconcile     reconcile.Config
	Limits        *risk.StaticLimits
	Risk          risk.Authorizer
	Store         store.Options
	Sim           func(*brokersim.Config)
	// BrokerTimeout bounds each call from the OMS to the broker. Default 3s.
	BrokerTimeout time.Duration
}

// Env is one assembled system.
type Env struct {
	T     testing.TB
	App   *app.App
	Sim   *brokersim.Sim
	Clock *clock.Fake // nil with RealClock
	URL   string      // the OMS API
	// BrokerURL is the simulated broker's API.
	BrokerURL string
	Broker    *broker.HTTPClient

	// Bearer tokens.
	Alice, Bob, Vera, Olivia string
	WebhookSecret            string
	BrokerKey                string

	opts   Options
	keySeq atomic.Int64
}

func secret() string {
	b := make([]byte, 24)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b)
}

// New builds an Env that is torn down when the test ends.
func New(t testing.TB, mods ...func(*Options)) *Env {
	t.Helper()
	if adminURL == "" {
		t.Fatal("harness: no database; the package needs `func TestMain(m *testing.M) { harness.Main(m) }`")
	}
	opts := Options{
		Outbox: outbox.Config{
			WorkerID: "worker-1", MaxAttempts: 4, BaseBackoff: time.Second, MaxBackoff: 8 * time.Second,
			Lease: 30 * time.Second, PollInterval: 20 * time.Millisecond, Jitter: func() float64 { return 1 },
		},
		Reconcile: reconcile.Config{Grace: 5 * time.Second, Lookback: 24 * time.Hour},
		Limits: &risk.StaticLimits{
			Version: "test-1", AllowedSymbols: []string{"AAPL", "MSFT", "TSLA", "NVDA", UnknownSymbol},
			MaxOrderQty: 10_000, MaxOrderNotional: fixed.MustParse("5000000"), AllowMarketOrders: true,
		},
		Store:         store.Options{MaxConns: 16, LockTimeout: 2 * time.Second},
		BrokerTimeout: 3 * time.Second,
	}
	for _, mod := range mods {
		mod(&opts)
	}
	ctx := context.Background()
	e := &Env{T: t, opts: opts, Alice: secret(), Bob: secret(), Vera: secret(), Olivia: secret(),
		WebhookSecret: secret(), BrokerKey: secret()}

	dbURL := createDatabase(t)

	var clk clock.Clock = clock.System{}
	if !opts.RealClock {
		e.Clock = clock.NewFake(Start)
		clk = e.Clock
	}
	var gen ids.Generator = ids.Random{}
	if opts.SequentialIDs {
		gen = &ids.Sequence{}
	}

	// The OMS and the broker each need the other's address, so the OMS
	// server starts with a handler that is filled in once the app exists.
	var handler atomic.Value
	omsSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h, _ := handler.Load().(http.Handler)
		if h == nil {
			http.Error(w, "starting", http.StatusServiceUnavailable)
			return
		}
		h.ServeHTTP(w, r)
	}))
	e.URL = omsSrv.URL

	simCfg := brokersim.Config{
		APIKey: e.BrokerKey, WebhookURL: omsSrv.URL + "/v1/broker/webhooks", WebhookSecret: e.WebhookSecret,
		Auto: opts.Auto, AutoDeliver: opts.Auto, Clock: clk,
		AckDelay: time.Millisecond, FillDelay: time.Millisecond, CancelDelay: time.Millisecond,
		WebhookBackoff: 10 * time.Millisecond,
		Prices: map[string]fixed.Micros{
			"AAPL": fixed.MustParse("150"), "MSFT": fixed.MustParse("400"),
			"TSLA": fixed.MustParse("250"), "NVDA": fixed.MustParse("900"),
		},
		FeePerUnit: fixed.MustParse("0.005"),
	}
	if opts.Sim != nil {
		opts.Sim(&simCfg)
	}
	e.Sim = brokersim.New(simCfg)
	simSrv := httptest.NewServer(e.Sim.Handler())
	e.BrokerURL = simSrv.URL
	e.Broker = broker.NewHTTPClient(simSrv.URL, e.BrokerKey, opts.BrokerTimeout)

	keys, err := auth.ParseKeys(mustJSON([]map[string]any{
		{"principal": "alice", "role": "trader", "accounts": []string{Account1, Account2}, "token": e.Alice},
		{"principal": "bob", "role": "trader", "accounts": []string{Account3}, "token": e.Bob},
		{"principal": "vera", "role": "viewer", "accounts": []string{Account1}, "token": e.Vera},
		{"principal": "olivia", "role": "operator", "accounts": []string{"*"}, "token": e.Olivia},
	}))
	if err != nil {
		t.Fatalf("harness: api keys: %v", err)
	}
	var authorizer risk.Authorizer = opts.Limits
	if opts.Risk != nil {
		authorizer = opts.Risk
	}

	logger := slog.New(slog.DiscardHandler)
	if os.Getenv("OMS_TEST_LOG") != "" {
		logger = slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelDebug}))
	}
	e.App, err = app.New(ctx, app.Config{
		DatabaseURL: dbURL, Store: opts.Store, Keys: keys, Risk: authorizer, Broker: e.Broker,
		WebhookSecret: e.WebhookSecret, Outbox: opts.Outbox, Reconcile: opts.Reconcile,
		Clock: clk, IDs: gen, Log: logger,
	})
	if err != nil {
		t.Fatalf("harness: %v", err)
	}
	handler.Store(e.App.Handler())

	bgCtx, stopBackground := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		e.App.Run(bgCtx, app.Roles{Outbox: opts.Background})
	}()

	t.Cleanup(func() {
		stopBackground()
		<-done
		e.Sim.Close()
		if !t.Failed() {
			e.Verify()
		}
		omsSrv.Close()
		simSrv.Close()
		e.App.Close()
	})
	return e
}

// NewDatabase makes an empty database for a test that runs the real binaries
// instead of an Env, and returns its URL. It is dropped when the test ends.
func NewDatabase(t testing.TB) string { return createDatabase(t) }

// Secret returns a fresh random credential.
func Secret() string { return secret() }

// createDatabase makes an empty database for one test and drops it after.
func createDatabase(t testing.TB) string {
	t.Helper()
	ctx := context.Background()
	name := "t_" + secret()[:16]
	admin, err := pgx.Connect(ctx, adminURL)
	if err != nil {
		t.Fatalf("harness: connect: %v", err)
	}
	defer admin.Close(ctx)
	if _, err := admin.Exec(ctx, `CREATE DATABASE `+name); err != nil {
		t.Fatalf("harness: create database: %v", err)
	}
	t.Cleanup(func() {
		c, err := pgx.Connect(ctx, adminURL)
		if err != nil {
			return
		}
		defer c.Close(ctx)
		c.Exec(ctx, `DROP DATABASE IF EXISTS `+name+` WITH (FORCE)`)
	})
	u, err := url.Parse(adminURL)
	if err != nil {
		t.Fatalf("harness: database url: %v", err)
	}
	u.Path = "/" + name
	return u.String()
}

func mustJSON(v any) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return b
}

// --- HTTP ---

// Resp is an API response.
type Resp struct {
	Status int
	Header http.Header
	Body   []byte
	t      testing.TB
}

// Decode unmarshals the body.
func (r Resp) Decode(v any) {
	r.t.Helper()
	if err := json.Unmarshal(r.Body, v); err != nil {
		r.t.Fatalf("decode %q: %v", r.Body, err)
	}
}

// Order decodes the body as an order.
func (r Resp) Order() service.OrderView {
	r.t.Helper()
	var o service.OrderView
	r.Decode(&o)
	return o
}

// ErrorCode returns the error code of an error response, or "".
func (r Resp) ErrorCode() string {
	var body struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	json.Unmarshal(r.Body, &body)
	return body.Error.Code
}

// Want fails the test unless the response has the given status.
func (r Resp) Want(status int) Resp {
	r.t.Helper()
	if r.Status != status {
		r.t.Fatalf("status = %d, want %d; body: %s", r.Status, status, r.Body)
	}
	return r
}

// WantError fails the test unless the response is the given error.
func (r Resp) WantError(status int, code string) Resp {
	r.t.Helper()
	if r.Status != status || r.ErrorCode() != code {
		r.t.Fatalf("got %d %q, want %d %q; body: %s", r.Status, r.ErrorCode(), status, code, r.Body)
	}
	return r
}

// Call makes an API request. body may be nil, a []byte or string sent as is,
// or anything else, which is sent as JSON. headers are name, value pairs.
func (e *Env) Call(token, method, path string, body any, headers ...string) Resp {
	e.T.Helper()
	var rd io.Reader
	switch b := body.(type) {
	case nil:
	case []byte:
		rd = bytes.NewReader(b)
	case string:
		rd = strings.NewReader(b)
	default:
		rd = bytes.NewReader(mustJSON(b))
	}
	req, err := http.NewRequest(method, e.URL+path, rd)
	if err != nil {
		e.T.Fatalf("request: %v", err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	if rd != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	for i := 0; i+1 < len(headers); i += 2 {
		req.Header.Set(headers[i], headers[i+1])
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		e.T.Fatalf("%s %s: %v", method, path, err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		e.T.Fatalf("%s %s: read: %v", method, path, err)
	}
	return Resp{Status: resp.StatusCode, Header: resp.Header, Body: data, t: e.T}
}

// Key returns a new idempotency key.
func (e *Env) Key() string { return fmt.Sprintf("key-%d", e.keySeq.Add(1)) }

// Price parses a decimal.
func Price(s string) *fixed.Micros {
	p := fixed.MustParse(s)
	return &p
}

// OrderReq returns a valid order: alice buys 100 AAPL at a 150.00 limit in
// ACC-1, good for the day. mods adjust it.
func OrderReq(mods ...func(*service.CreateOrderRequest)) service.CreateOrderRequest {
	r := service.CreateOrderRequest{
		AccountID: Account1, Symbol: "AAPL", Side: domain.Buy, Type: domain.Limit,
		TimeInForce: domain.Day, Qty: 100, LimitPrice: Price("150"),
	}
	for _, mod := range mods {
		mod(&r)
	}
	return r
}

// Create posts an order as alice with the given idempotency key.
func (e *Env) Create(req service.CreateOrderRequest, key string) Resp {
	e.T.Helper()
	return e.Call(e.Alice, http.MethodPost, "/v1/orders", req, "Idempotency-Key", key)
}

// Created creates an order (state VALIDATED, submission queued).
func (e *Env) Created(mods ...func(*service.CreateOrderRequest)) service.OrderView {
	e.T.Helper()
	o := e.Create(OrderReq(mods...), e.Key()).Want(http.StatusCreated).Order()
	if o.State != domain.StateValidated {
		e.T.Fatalf("new order is %s (%s), want VALIDATED", o.State, o.RejectReason)
	}
	return o
}

// Submitted creates an order and delivers its submission to the broker
// (state SUBMITTING).
func (e *Env) Submitted(mods ...func(*service.CreateOrderRequest)) service.OrderView {
	e.T.Helper()
	o := e.Created(mods...)
	e.Drain()
	return e.WantState(o.ID, domain.StateSubmitting)
}

// Working creates an order, submits it and has the broker acknowledge it
// (state ACKNOWLEDGED).
func (e *Env) Working(mods ...func(*service.CreateOrderRequest)) service.OrderView {
	e.T.Helper()
	o := e.Submitted(mods...)
	if _, err := e.Sim.Ack(o.ID); err != nil {
		e.T.Fatalf("ack: %v", err)
	}
	e.Flush()
	return e.WantState(o.ID, domain.StateAcknowledged)
}

// Cancel requests a cancel as alice.
func (e *Env) Cancel(orderID string) Resp {
	e.T.Helper()
	return e.Call(e.Alice, http.MethodPost, "/v1/orders/"+orderID+"/cancel", nil)
}

// Replace requests a replace as alice.
func (e *Env) Replace(orderID string, qty int64, limit string) Resp {
	e.T.Helper()
	req := service.ReplaceOrderRequest{}
	if qty != 0 {
		req.Qty = &qty
	}
	if limit != "" {
		req.LimitPrice = Price(limit)
	}
	return e.Call(e.Alice, http.MethodPost, "/v1/orders/"+orderID+"/replace", req, "Idempotency-Key", e.Key())
}

// --- Stepping the system ---

// Drain runs the outbox worker until nothing is due.
func (e *Env) Drain() int {
	e.T.Helper()
	n, err := e.App.Worker.Drain(context.Background())
	if err != nil {
		e.T.Fatalf("drain outbox: %v", err)
	}
	return n
}

// Flush delivers the broker's waiting webhooks in order and requires the OMS
// to accept each of them.
func (e *Env) Flush() []brokersim.Delivery {
	e.T.Helper()
	return e.accepted(e.Sim.Flush(context.Background()))
}

func (e *Env) accepted(ds []brokersim.Delivery) []brokersim.Delivery {
	e.T.Helper()
	for _, d := range ds {
		if !d.OK() {
			e.T.Fatalf("webhook %s (%s) was not accepted: %d %s %v", d.EventID, d.Type, d.Status, d.Body, d.Err)
		}
	}
	return ds
}

// Accepted requires every delivery to have been accepted by the OMS.
func (e *Env) Accepted(ds []brokersim.Delivery) []brokersim.Delivery {
	e.T.Helper()
	return e.accepted(ds)
}

// Outcomes returns the outcome the OMS reported for each delivery.
func Outcomes(ds []brokersim.Delivery) []string {
	out := make([]string, len(ds))
	for i, d := range ds {
		var body struct {
			Outcome string `json:"outcome"`
		}
		json.Unmarshal([]byte(d.Body), &body)
		out[i] = body.Outcome
	}
	return out
}

// Fill has the broker execute qty at price and delivers the report. It
// returns the broker's execution id.
func (e *Env) Fill(orderID string, qty int64, price string) string {
	e.T.Helper()
	execID := e.FillHeld(orderID, qty, price)
	e.Flush()
	return execID
}

// FillHeld has the broker execute qty at price without delivering the
// report yet.
func (e *Env) FillHeld(orderID string, qty int64, price string) string {
	e.T.Helper()
	execID, _, err := e.Sim.Fill(orderID, qty, fixed.MustParse(price))
	if err != nil {
		e.T.Fatalf("fill: %v", err)
	}
	return execID
}

// Send delivers a hand-built webhook, correctly signed, and returns the
// OMS's answer. Tests use it for reports a well-behaved broker would not
// send.
func (e *Env) Send(ev broker.WebhookEvent) brokersim.Delivery {
	e.T.Helper()
	if ev.EventTime.IsZero() {
		ev.EventTime = e.Now()
	}
	return e.Sim.SendRaw(context.Background(), mustJSON(ev), e.WebhookSecret)
}

// Now is the current time on the test's clock.
func (e *Env) Now() time.Time {
	if e.Clock != nil {
		return e.Clock.Now()
	}
	return clock.System{}.Now()
}

// Outcome extracts the outcome the OMS reported for one delivery.
func Outcome(d brokersim.Delivery) string { return Outcomes([]brokersim.Delivery{d})[0] }

// Must fails the test on a simulator control error.
func (e *Env) Must(_ brokersim.Event, err error) {
	e.T.Helper()
	if err != nil {
		e.T.Fatalf("broker simulator: %v", err)
	}
}

// Advance moves the frozen clock.
func (e *Env) Advance(d time.Duration) {
	e.T.Helper()
	if e.Clock == nil {
		e.T.Fatal("harness: Advance needs the frozen clock")
	}
	e.Clock.Advance(d)
}

// Reconcile runs one reconciliation pass and requires it to complete.
func (e *Env) Reconcile() store.Run {
	e.T.Helper()
	run, err := e.App.Reconciler.Run(context.Background(), "test")
	if err != nil {
		e.T.Fatalf("reconcile: %v", err)
	}
	if run.Status != store.RunCompleted {
		e.T.Fatalf("reconciliation run is %s: %s", run.Status, run.Error)
	}
	return run
}

// --- Reading state ---

// Order reads an order's current projection.
func (e *Env) Order(id string) service.OrderView {
	e.T.Helper()
	o, err := e.App.Store.GetOrder(context.Background(), id)
	if err != nil {
		e.T.Fatalf("get order %s: %v", id, err)
	}
	return service.View(o)
}

// WantState fails the test unless the order is in the given state.
func (e *Env) WantState(id string, want domain.State) service.OrderView {
	e.T.Helper()
	o := e.Order(id)
	if o.State != want {
		e.T.Fatalf("order %s is %s, want %s; history: %v", id, o.State, want, e.EventTypes(id))
	}
	return o
}

// WantFilled fails the test unless the order has the given filled quantity
// and notional.
func (e *Env) WantFilled(id string, qty int64, notional string) service.OrderView {
	e.T.Helper()
	o := e.Order(id)
	if o.FilledQty != qty || o.FilledNotional != fixed.MustParse(notional) {
		e.T.Fatalf("order %s filled %d for %s, want %d for %s; history: %v",
			id, o.FilledQty, o.FilledNotional, qty, notional, e.EventTypes(id))
	}
	return o
}

// Events returns an order's history.
func (e *Env) Events(id string) []domain.Event {
	e.T.Helper()
	evs, err := e.App.Store.ListEvents(context.Background(), id)
	if err != nil {
		e.T.Fatalf("list events: %v", err)
	}
	return evs
}

// EventTypes returns the types of an order's events, in order.
func (e *Env) EventTypes(id string) []string {
	e.T.Helper()
	evs := e.Events(id)
	out := make([]string, len(evs))
	for i, ev := range evs {
		out[i] = string(ev.Type)
	}
	return out
}

// CountEvents returns how many events of a type the order has.
func (e *Env) CountEvents(id string, typ domain.EventType) int {
	e.T.Helper()
	n := 0
	for _, ev := range e.Events(id) {
		if ev.Type == typ {
			n++
		}
	}
	return n
}

// Executions returns an order's booked execution records.
func (e *Env) Executions(id string) []store.ExecutionRow {
	e.T.Helper()
	xs, err := e.App.Store.ListExecutions(context.Background(), id)
	if err != nil {
		e.T.Fatalf("list executions: %v", err)
	}
	return xs
}

// Position returns the position for an account and symbol; the zero value
// if there is none.
func (e *Env) Position(account, symbol string) store.PositionRow {
	e.T.Helper()
	ps, err := e.App.Store.ListPositions(context.Background(), []string{account})
	if err != nil {
		e.T.Fatalf("list positions: %v", err)
	}
	for _, p := range ps {
		if p.Symbol == symbol {
			return p
		}
	}
	return store.PositionRow{AccountID: account, Symbol: symbol}
}

// WantPosition fails the test unless the position has the given quantity
// and cash.
func (e *Env) WantPosition(account, symbol string, qty int64, cash string) store.PositionRow {
	e.T.Helper()
	p := e.Position(account, symbol)
	if p.NetQty != qty || p.NetCash != fixed.MustParse(cash) {
		e.T.Fatalf("position %s/%s is %d for %s, want %d for %s", account, symbol, p.NetQty, p.NetCash, qty, cash)
	}
	return p
}

// Outbox returns an order's outbox messages.
func (e *Env) Outbox(id string) []store.OutboxRow {
	e.T.Helper()
	rows, err := e.App.Store.ListOutbox(context.Background(), id)
	if err != nil {
		e.T.Fatalf("list outbox: %v", err)
	}
	return rows
}

// Issues returns the issues with the given status ("" for all).
func (e *Env) Issues(status string) []store.Issue {
	e.T.Helper()
	issues, err := e.App.Store.ListIssues(context.Background(), status, "", 1000)
	if err != nil {
		e.T.Fatalf("list issues: %v", err)
	}
	return issues
}

// OpenIssue returns the single open issue of the given kind.
func (e *Env) OpenIssue(kind string) store.Issue {
	e.T.Helper()
	var found []store.Issue
	open := e.Issues(store.IssueOpen)
	for _, is := range open {
		if is.Kind == kind {
			found = append(found, is)
		}
	}
	if len(found) != 1 {
		kinds := make([]string, len(open))
		for i, is := range open {
			kinds[i] = is.Kind
		}
		e.T.Fatalf("want exactly one open %s issue, have %d; open issues: %v", kind, len(found), kinds)
	}
	return found[0]
}

// WantNoOpenIssues fails the test if the exception queue is not empty.
func (e *Env) WantNoOpenIssues() {
	e.T.Helper()
	if open := e.Issues(store.IssueOpen); len(open) > 0 {
		e.T.Fatalf("want no open issues, have %d; first: %s %s", len(open), open[0].Kind, open[0].Details)
	}
}

// Resolve applies an operator repair to an issue, as olivia.
func (e *Env) Resolve(issueID, action, note string) Resp {
	e.T.Helper()
	return e.Call(e.Olivia, http.MethodPost, "/v1/reconciliation/issues/"+issueID+"/resolve",
		map[string]string{"action": action, "note": note})
}

// Verify replays the event log and fails the test if any projection
// (orders, executions, positions) differs from it.
func (e *Env) Verify() store.VerifyReport {
	e.T.Helper()
	rep, err := e.App.Store.Verify(context.Background())
	if err != nil {
		e.T.Errorf("verify: %v", err)
		return rep
	}
	for _, p := range rep.Problems {
		e.T.Errorf("projection does not match the event log: %s", p)
	}
	return rep
}

// NewWorker returns another outbox worker on the same database and broker,
// as a second process would be.
func (e *Env) NewWorker(id string, mods ...func(*outbox.Config)) *outbox.Worker {
	cfg := e.opts.Outbox
	cfg.WorkerID = id
	for _, mod := range mods {
		mod(&cfg)
	}
	return outbox.New(cfg, e.App.Store, e.Broker, ids.Random{}, e.App.Metrics, e.App.Log)
}

// NewWorkerWith returns an outbox worker that talks to the given broker
// client instead of the simulated broker.
func (e *Env) NewWorkerWith(id string, client broker.Client) *outbox.Worker {
	cfg := e.opts.Outbox
	cfg.WorkerID = id
	return outbox.New(cfg, e.App.Store, client, ids.Random{}, e.App.Metrics, e.App.Log)
}

// Eventually polls cond until it is true or the timeout passes.
func (e *Env) Eventually(timeout time.Duration, what string, cond func() bool) {
	e.T.Helper()
	deadline := time.Now().Add(timeout)
	for !cond() {
		if time.Now().After(deadline) {
			e.T.Fatalf("timed out after %s waiting for %s", timeout, what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// Metric returns the rendered metrics text.
func (e *Env) Metric() string {
	var b bytes.Buffer
	e.App.Metrics.Registry.Write(&b)
	return b.String()
}
