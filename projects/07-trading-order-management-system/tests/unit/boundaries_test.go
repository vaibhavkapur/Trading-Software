package unit

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"oms/src/auth"
	"oms/src/clock"
	"oms/src/domain"
	"oms/src/fixed"
	"oms/src/ids"
	"oms/src/metrics"
	"oms/src/outbox"
	"oms/src/risk"
)

// token makes a throwaway API token of valid length for these tests.
func token(name string) string { return "unit-test-token-" + name + "-0123456789" }

func keysJSON(entries ...string) []byte { return []byte("[" + strings.Join(entries, ",") + "]") }

func keyEntry(principal, role, accounts, tok string) string {
	return fmt.Sprintf(`{"principal":%q,"role":%q,"accounts":%s,"token":%q}`, principal, role, accounts, tok)
}

func TestAuth_ParseKeysFailsClosed(t *testing.T) {
	bad := map[string][]byte{
		"not JSON":            []byte(`{`),
		"an object":           []byte(`{"principal":"a"}`),
		"empty list":          []byte(`[]`),
		"null":                []byte(`null`),
		"no principal":        keysJSON(keyEntry("", "trader", `["ACC-1"]`, token("a"))),
		"blank principal":     keysJSON(keyEntry("  ", "trader", `["ACC-1"]`, token("a"))),
		"unknown role":        keysJSON(keyEntry("a", "admin", `["ACC-1"]`, token("a"))),
		"no role":             keysJSON(keyEntry("a", "", `["ACC-1"]`, token("a"))),
		"role in other case":  keysJSON(keyEntry("a", "Trader", `["ACC-1"]`, token("a"))),
		"short token":         keysJSON(keyEntry("a", "trader", `["ACC-1"]`, "short-token")),
		"empty token":         keysJSON(keyEntry("a", "trader", `["ACC-1"]`, "")),
		"trader, no accounts": keysJSON(keyEntry("a", "trader", `[]`, token("a"))),
		"viewer, no accounts": keysJSON(keyEntry("a", "viewer", `null`, token("a"))),
		"shared token":        keysJSON(keyEntry("a", "trader", `["ACC-1"]`, token("a")), keyEntry("b", "viewer", `["ACC-1"]`, token("a"))),
		"one bad among good":  keysJSON(keyEntry("a", "trader", `["ACC-1"]`, token("a")), keyEntry("b", "viewer", `["ACC-1"]`, "x")),
	}
	for name, data := range bad {
		keys, err := auth.ParseKeys(data)
		if err == nil || keys != nil {
			t.Errorf("%s: accepted", name)
			continue
		}
		// A configuration error must never echo a token back.
		if strings.Contains(err.Error(), "unit-test-token") || strings.Contains(err.Error(), "short-token") {
			t.Errorf("%s: error leaks a token: %v", name, err)
		}
	}
}

func TestAuth_AuthenticateAndPermissions(t *testing.T) {
	keys, err := auth.ParseKeys(keysJSON(
		keyEntry("alice", "trader", `["ACC-1","ACC-2"]`, token("alice")),
		keyEntry("vera", "viewer", `["ACC-1"]`, token("vera")),
		keyEntry("desk", "trader", `["*"]`, token("desk")),
		keyEntry("olivia", "operator", `[]`, token("olivia")),
	))
	if err != nil {
		t.Fatal(err)
	}
	for _, tok := range []string{"", "nope", token("mallory"), token("alice") + " ", " " + token("alice"),
		strings.ToUpper(token("alice")), token("alice")[:len(token("alice"))-1], "Bearer " + token("alice")} {
		if p, ok := keys.Authenticate(tok); ok || p.Name != "" {
			t.Errorf("token %q authenticated as %+v", tok, p)
		}
	}
	var none *auth.Keys
	if _, ok := none.Authenticate(token("alice")); ok {
		t.Error("a nil key set authenticated a caller")
	}

	who := func(name string) auth.Principal {
		p, ok := keys.Authenticate(token(name))
		if !ok || p.Name != name {
			t.Fatalf("%s did not authenticate: %+v", name, p)
		}
		return p
	}
	type perms struct {
		view1, view3, trade1, trade3, operate bool
		scope                                 string
	}
	cases := map[string]perms{
		"alice":  {view1: true, trade1: true, scope: "[ACC-1 ACC-2]"},
		"vera":   {view1: true, scope: "[ACC-1]"},
		"desk":   {view1: true, view3: true, trade1: true, trade3: true, scope: "[]"},
		"olivia": {view1: true, view3: true, operate: true, scope: "[]"}, // operators see everything and trade nothing
	}
	for name, want := range cases {
		p := who(name)
		got := perms{p.CanView("ACC-1"), p.CanView("ACC-3"), p.CanTrade("ACC-1"), p.CanTrade("ACC-3"), p.CanOperate(),
			fmt.Sprint(p.AccountScope())}
		if got != want {
			t.Errorf("%s: %+v, want %+v", name, got, want)
		}
		if (p.AccountScope() == nil) != (want.scope == "[]") {
			t.Errorf("%s: scope %v", name, p.AccountScope())
		}
	}

	// The zero principal (an unauthenticated request that slipped through)
	// can do nothing, whatever its fields say.
	for _, p := range []auth.Principal{{}, {Role: auth.RoleOperator}, {Role: auth.RoleTrader, Accounts: []string{"*"}}} {
		if p.CanView("ACC-1") || p.CanTrade("ACC-1") || p.CanOperate() {
			t.Errorf("anonymous principal %+v has permissions", p)
		}
	}
	// Scope is a copy: callers cannot widen a principal through it.
	alice := who("alice")
	alice.AccountScope()[0] = "ACC-3"
	if alice.CanView("ACC-3") {
		t.Error("AccountScope exposes the principal's own slice")
	}
}

func TestAuth_LoadKeysReadsOnlyTheEnvironment(t *testing.T) {
	t.Setenv("OMS_API_KEYS", "")
	t.Setenv("OMS_API_KEYS_FILE", "")
	if _, err := auth.LoadKeys(); err == nil {
		t.Error("no configuration: expected an error, not an open door")
	}

	path := filepath.Join(t.TempDir(), "keys.json")
	if err := os.WriteFile(path, keysJSON(keyEntry("vera", "viewer", `["ACC-1"]`, token("vera"))), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("OMS_API_KEYS_FILE", path)
	keys, err := auth.LoadKeys()
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := keys.Authenticate(token("vera")); !ok {
		t.Error("key from file does not authenticate")
	}

	// Inline configuration wins over the file.
	t.Setenv("OMS_API_KEYS", string(keysJSON(keyEntry("alice", "trader", `["ACC-1"]`, token("alice")))))
	keys, err = auth.LoadKeys()
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := keys.Authenticate(token("alice")); !ok {
		t.Error("inline key does not authenticate")
	}
	if _, ok := keys.Authenticate(token("vera")); ok {
		t.Error("file was merged into inline configuration")
	}

	t.Setenv("OMS_API_KEYS", "")
	t.Setenv("OMS_API_KEYS_FILE", filepath.Join(t.TempDir(), "missing.json"))
	if _, err := auth.LoadKeys(); err == nil {
		t.Error("missing file: expected an error")
	}
}

func TestRisk_StaticLimits(t *testing.T) {
	limits := &risk.StaticLimits{Version: "v1", AllowedSymbols: []string{"AAPL", "MSFT"}, MaxOrderQty: 1000,
		MaxOrderNotional: fixed.MustParse("100000")}
	price := func(s string) *fixed.Micros { v := fixed.MustParse(s); return &v }
	req := func(symbol string, typ domain.OrderType, qty int64, lp *fixed.Micros) risk.Request {
		return risk.Request{OrderID: "ord_1", AccountID: "ACC-1", Symbol: symbol, Side: domain.Buy, Type: typ, Qty: qty, LimitPrice: lp}
	}
	cases := []struct {
		name  string
		req   risk.Request
		codes string
	}{
		{"within limits", req("AAPL", domain.Limit, 100, price("150")), "[]"},
		{"exactly at the quantity limit", req("AAPL", domain.Limit, 1000, price("100")), "[]"},
		{"exactly at the notional limit", req("MSFT", domain.Limit, 250, price("400")), "[]"},
		{"one micro over the notional limit", req("MSFT", domain.Limit, 1, price("100000.000001")), "[MAX_ORDER_NOTIONAL]"},
		{"over the quantity limit", req("AAPL", domain.Limit, 1001, price("1")), "[MAX_ORDER_QTY]"},
		{"symbol not allowed", req("TSLA", domain.Limit, 1, price("1")), "[SYMBOL_NOT_ALLOWED]"},
		{"symbols are case-sensitive", req("aapl", domain.Limit, 1, price("1")), "[SYMBOL_NOT_ALLOWED]"},
		{"market order while disabled", req("AAPL", domain.Market, 1, nil), "[MARKET_ORDERS_DISABLED]"},
		{"every rule at once", req("TSLA", domain.Limit, 5000, price("500")), "[SYMBOL_NOT_ALLOWED MAX_ORDER_QTY MAX_ORDER_NOTIONAL]"},
		// A notional too large to represent is over the limit, not zero.
		{"notional overflow", req("AAPL", domain.Limit, 1000, price("999999999999")), "[MAX_ORDER_NOTIONAL]"},
	}
	for _, c := range cases {
		d, err := limits.Authorize(context.Background(), c.req)
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		codes := fmt.Sprint(d.Codes)
		if codes != c.codes || d.Allowed != (c.codes == "[]") || (d.Reason == "") != d.Allowed {
			t.Errorf("%s: allowed=%v codes=%s reason=%q, want %s", c.name, d.Allowed, codes, d.Reason, c.codes)
		}
		if d.Authorizer != "static-limits/v1" {
			t.Errorf("%s: authorizer %q", c.name, d.Authorizer)
		}
	}

	open := &risk.StaticLimits{Version: "v2", AllowedSymbols: []string{"*"}, MaxOrderQty: 10, MaxOrderNotional: fixed.MustParse("1000"),
		AllowMarketOrders: true}
	if d, _ := open.Authorize(context.Background(), req("ANYTHING", domain.Market, 10, nil)); !d.Allowed {
		t.Errorf("wildcard symbol with market orders enabled: %+v", d)
	}
	// Enabling market orders does not lift the quantity limit.
	if d, _ := open.Authorize(context.Background(), req("ANYTHING", domain.Market, 11, nil)); d.Allowed {
		t.Errorf("market order over the quantity limit was allowed")
	}
}

func TestRisk_LimitsThatAllowEverythingOrNothingAreRefused(t *testing.T) {
	good := risk.StaticLimits{Version: "v1", AllowedSymbols: []string{"*"}, MaxOrderQty: 1, MaxOrderNotional: 1}
	if err := good.Validate(); err != nil {
		t.Fatal(err)
	}
	for name, mutate := range map[string]func(*risk.StaticLimits){
		"no version":        func(l *risk.StaticLimits) { l.Version = "" },
		"no symbols":        func(l *risk.StaticLimits) { l.AllowedSymbols = nil },
		"zero quantity":     func(l *risk.StaticLimits) { l.MaxOrderQty = 0 },
		"negative quantity": func(l *risk.StaticLimits) { l.MaxOrderQty = -1 },
		"zero notional":     func(l *risk.StaticLimits) { l.MaxOrderNotional = 0 },
	} {
		l := good
		mutate(&l)
		if err := l.Validate(); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}

	dir := t.TempDir()
	write := func(name, content string) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	l, err := risk.LoadStaticLimits(write("ok.json",
		`{"version":"2026-01","allowed_symbols":["AAPL"],"max_order_qty":500,"max_order_notional":"250000.50","allow_market_orders":true}`))
	if err != nil {
		t.Fatal(err)
	}
	if l.MaxOrderNotional != fixed.MustParse("250000.5") || l.MaxOrderQty != 500 || !l.AllowMarketOrders {
		t.Errorf("loaded %+v", l)
	}
	for _, p := range []string{
		filepath.Join(dir, "missing.json"),
		write("syntax.json", `{"version":`),
		write("empty.json", `{}`),
		write("float.json", `{"version":"v","allowed_symbols":["*"],"max_order_qty":1,"max_order_notional":"1e6"}`),
	} {
		if _, err := risk.LoadStaticLimits(p); err == nil {
			t.Errorf("%s: loaded", filepath.Base(p))
		}
	}
}

func TestOutbox_BackoffDoublesUpToTheCap(t *testing.T) {
	w := outbox.New(outbox.Config{WorkerID: "w", BaseBackoff: 200 * time.Millisecond, MaxBackoff: 5 * time.Second,
		Jitter: func() float64 { return 1 }}, nil, nil, nil, nil, nil)
	want := []time.Duration{200, 200, 400, 800, 1600, 3200, 5000, 5000, 5000}
	for attempt, ms := range want {
		if got := w.Backoff(attempt); got != ms*time.Millisecond {
			t.Errorf("Backoff(%d) = %v, want %v", attempt, got, ms*time.Millisecond)
		}
	}
	// Very large attempt counts neither overflow nor exceed the cap.
	for _, attempt := range []int{64, 1000, 1 << 30} {
		if got := w.Backoff(attempt); got != 5*time.Second {
			t.Errorf("Backoff(%d) = %v", attempt, got)
		}
	}
	// A cap that is not a power-of-two multiple of the base is still honoured.
	odd := outbox.New(outbox.Config{WorkerID: "w", BaseBackoff: 300 * time.Millisecond, MaxBackoff: time.Second,
		Jitter: func() float64 { return 1 }}, nil, nil, nil, nil, nil)
	if got := odd.Backoff(3); got != time.Second {
		t.Errorf("Backoff(3) = %v, want the 1s cap", got)
	}

	// Jitter scales the delay; the default stays within ±20%.
	half := outbox.New(outbox.Config{WorkerID: "w", BaseBackoff: time.Second, MaxBackoff: time.Minute,
		Jitter: func() float64 { return 0.5 }}, nil, nil, nil, nil, nil)
	if got := half.Backoff(2); got != time.Second {
		t.Errorf("jittered Backoff(2) = %v, want 1s", got)
	}
	def := outbox.New(outbox.Config{WorkerID: "w"}, nil, nil, nil, nil, nil)
	distinct := map[time.Duration]bool{}
	for i := 0; i < 200; i++ {
		d := def.Backoff(1)
		if d < 160*time.Millisecond || d >= 240*time.Millisecond {
			t.Fatalf("default Backoff(1) = %v, outside [160ms, 240ms)", d)
		}
		distinct[d] = true
	}
	if len(distinct) < 50 {
		t.Errorf("default jitter produced only %d distinct delays in 200", len(distinct))
	}
}

func TestMetrics_PrometheusTextFormat(t *testing.T) {
	r := &metrics.Registry{}
	c := r.Counter("jobs_total", "Jobs by result.", "type", "result")
	plain := r.Counter("ticks_total", "Ticks.")
	h := r.Histogram("latency_seconds", "Latency.", []float64{1, 0.1, 0.5}, "op") // unsorted on purpose
	r.GaugeFunc("queue_depth", "Depth.", []string{"queue"}, func() []metrics.Sample {
		return []metrics.Sample{{LabelValues: []string{"a"}, Value: 3}, {LabelValues: []string{"b"}, Value: 0.5}}
	})
	r.GaugeFunc("empty_gauge", "Nothing to report.", nil, func() []metrics.Sample { return nil })

	c.Inc("submit", "ok")
	c.Add(2, "submit", "ok")
	c.Inc("cancel", `we"ird\la`+"\nbel")
	plain.Inc()
	for _, v := range []float64{0.05, 0.1, 0.3, 0.5, 0.7, 2} { // bounds are inclusive upper limits
		h.Observe(v, "x")
	}

	var buf bytes.Buffer
	r.Write(&buf)
	want := `# HELP jobs_total Jobs by result.
# TYPE jobs_total counter
jobs_total{type="cancel",result="we\"ird\\la\nbel"} 1
jobs_total{type="submit",result="ok"} 3
# HELP ticks_total Ticks.
# TYPE ticks_total counter
ticks_total 1
# HELP latency_seconds Latency.
# TYPE latency_seconds histogram
latency_seconds_bucket{op="x",le="0.1"} 2
latency_seconds_bucket{op="x",le="0.5"} 4
latency_seconds_bucket{op="x",le="1"} 5
latency_seconds_bucket{op="x",le="+Inf"} 6
latency_seconds_sum{op="x"} 3.65
latency_seconds_count{op="x"} 6
# HELP queue_depth Depth.
# TYPE queue_depth gauge
queue_depth{queue="a"} 3
queue_depth{queue="b"} 0.5
# HELP empty_gauge Nothing to report.
# TYPE empty_gauge gauge
`
	if buf.String() != want {
		t.Errorf("rendered:\n%s\nwant:\n%s", buf.String(), want)
	}
	if c.Value("submit", "ok") != 3 || c.Value("never", "seen") != 0 || h.Count("x") != 6 || h.Count("y") != 0 {
		t.Error("Value/Count accessors disagree with what was recorded")
	}

	// The wrong number of label values is a programming error, caught loudly.
	func() {
		defer func() {
			if recover() == nil {
				t.Error("rendering a sample with the wrong label count did not panic")
			}
		}()
		bad := &metrics.Registry{}
		bad.Counter("x_total", "x", "a", "b").Inc("only-one")
		bad.Write(&bytes.Buffer{})
	}()
}

func TestMetrics_OMSSetIsWellFormedAndSafeForConcurrentUse(t *testing.T) {
	m := metrics.New()
	var wg sync.WaitGroup
	for g := 0; g < 8; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 500; i++ {
				m.BrokerEvents.Inc("FILL", "webhook", "APPLIED")
				m.SubmitToAck.Observe(0.02)
				m.OrderStateLag.Observe(metrics.Seconds(-time.Second), "FILL")
				var buf bytes.Buffer
				if i%100 == 0 {
					m.Registry.Write(&buf)
				}
			}
		}()
	}
	wg.Wait()
	if got := m.BrokerEvents.Value("FILL", "webhook", "APPLIED"); got != 4000 {
		t.Errorf("counter = %v, want 4000", got)
	}
	if m.SubmitToAck.Count() != 4000 || m.OrderStateLag.Count("FILL") != 4000 {
		t.Error("histogram lost observations")
	}
	if metrics.Seconds(-time.Hour) != 0 || metrics.Seconds(1500*time.Millisecond) != 1.5 {
		t.Error("Seconds does not clamp or convert")
	}

	var buf bytes.Buffer
	m.Registry.Write(&buf)
	out := buf.String()
	// Every metric the spec asks for is exported under a stable name.
	for _, name := range []string{
		"oms_submit_to_ack_seconds", "oms_order_state_lag_seconds", "oms_outbox_delivery_seconds",
		"oms_broker_events_total", "oms_reconciliation_drift_total", "oms_reconciliation_repair_seconds",
		"oms_reconciliation_auto_repaired_total", "oms_idempotent_replays_total", "oms_webhook_rejected_total",
	} {
		if !strings.Contains(out, "# TYPE "+name+" ") {
			t.Errorf("%s is not exported", name)
		}
	}
	line := regexp.MustCompile(`^(# (HELP|TYPE) [a-z_]+ .+|[a-z_]+(\{([a-z_]+="[^"]*",?)+\})? [0-9.e+-]+|[a-z_]+(\{.*\})? \+Inf)$`)
	for _, l := range strings.Split(strings.TrimRight(out, "\n"), "\n") {
		if !line.MatchString(l) {
			t.Errorf("not valid exposition format: %q", l)
		}
	}
}

func TestIDs(t *testing.T) {
	seen := map[string]bool{}
	format := regexp.MustCompile(`^ord_[0-9a-f]{24}$`)
	for i := 0; i < 10000; i++ {
		id := ids.Random{}.New("ord")
		if !format.MatchString(id) {
			t.Fatalf("id %q", id)
		}
		if seen[id] {
			t.Fatalf("duplicate id %q", id)
		}
		seen[id] = true
	}

	// The sequence is predictable, shared across prefixes, and safe to share.
	var s ids.Sequence
	if a, b, c := s.New("ord"), s.New("evt"), s.New("ord"); a != "ord_000001" || b != "evt_000002" || c != "ord_000003" {
		t.Errorf("sequence: %s %s %s", a, b, c)
	}
	var wg sync.WaitGroup
	var mu sync.Mutex
	got := map[string]bool{}
	for g := 0; g < 8; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 250; i++ {
				id := s.New("x")
				mu.Lock()
				got[id] = true
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if len(got) != 2000 || !got["x_002003"] {
		t.Errorf("%d distinct sequence ids, want 2000 ending at x_002003", len(got))
	}
}

func TestClock_IsUTCAtMicrosecondPrecision(t *testing.T) {
	// Postgres timestamps hold microseconds; a clock with more precision
	// would produce values that change when they are stored and read back.
	now := clock.System{}.Now()
	if now.Location() != time.UTC || now.Nanosecond()%1000 != 0 {
		t.Errorf("system clock: %v", now)
	}
	if d := time.Since(now); d < 0 || d > time.Minute {
		t.Errorf("system clock is %v away from the wall clock", d)
	}

	local := time.FixedZone("EST", -5*3600)
	f := clock.NewFake(time.Date(2026, 1, 5, 9, 30, 0, 123456789, local))
	want := time.Date(2026, 1, 5, 14, 30, 0, 123456000, time.UTC)
	if got := f.Now(); !got.Equal(want) || got.Location() != time.UTC {
		t.Errorf("fake clock starts at %v, want %v", got, want)
	}
	if !f.Now().Equal(f.Now()) {
		t.Error("the fake clock moves on its own")
	}
	f.Advance(90 * time.Second)
	if got := f.Now(); !got.Equal(want.Add(90 * time.Second)) {
		t.Errorf("after Advance: %v", got)
	}
	var c clock.Clock = f
	var wg sync.WaitGroup
	for g := 0; g < 4; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 100; i++ {
				f.Advance(time.Millisecond)
				c.Now()
			}
		}()
	}
	wg.Wait()
	if got := f.Now(); !got.Equal(want.Add(90*time.Second + 400*time.Millisecond)) {
		t.Errorf("after concurrent advances: %v", got)
	}
}
