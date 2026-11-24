// demo walks through the engine end to end over real HTTP, using simulated
// quotes, a simulated clock and a mock OMS. Nothing here touches a market.
package main

import (
	"bytes"
	"crypto/ed25519"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"riskengine/src/app"
	"riskengine/src/audit"
)

type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) Now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *clock) advance(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
	fmt.Printf("\n    ... simulated clock +%s ...\n", d)
}

const (
	adminToken = "demo-admin-token" // demo-only values, generated nowhere else
	feedToken  = "demo-feed-token"
)

var (
	base string
	clk  = &clock{t: time.Date(2026, 1, 5, 15, 0, 0, 0, time.UTC)}
)

func section(title string) { fmt.Printf("\n\n=== %s ===\n", title) }

// call performs one request and prints it with the chosen response fields.
func call(method, path, token, body string, show ...string) map[string]any {
	req, _ := http.NewRequest(method, base+path, strings.NewReader(body))
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		fmt.Println("request failed:", err)
		os.Exit(1)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	var out map[string]any
	json.Unmarshal(raw, &out)

	fmt.Printf("\n> %s %s", method, path)
	if body != "" {
		fmt.Printf("\n  %s", body)
	}
	fmt.Printf("\n< %d", resp.StatusCode)
	for _, k := range show {
		v := any(out)
		for _, part := range strings.Split(k, ".") {
			m, _ := v.(map[string]any)
			v = m[part]
		}
		if s, ok := v.(string); ok && len(s) == 64 {
			v = s[:16] + "..."
		}
		b, _ := json.Marshal(v)
		fmt.Printf("  %s=%s", k, b)
	}
	fmt.Println()
	return out
}

func quote(symbol, bid, ask string) {
	call("POST", "/v1/market/quotes", feedToken,
		fmt.Sprintf(`{"symbol":%q,"bid":%s,"ask":%s,"event_time":%q}`, symbol, bid, ask, clk.Now().Format(time.RFC3339Nano)))
}

func order(id, account, symbol, side string, qty int, price string) map[string]any {
	return call("POST", "/v1/risk/evaluate", "",
		fmt.Sprintf(`{"order_id":%q,"account_id":%q,"symbol":%q,"side":%q,"qty":%d,"limit_price":%s}`, id, account, symbol, side, qty, price),
		"decision", "reason_codes", "policy_version", "audit_seq")
}

func main() {
	policyPath := "examples/policy.json"
	if len(os.Args) > 1 {
		policyPath = os.Args[1]
	}
	dir, err := os.MkdirTemp("", "risk-demo-")
	if err != nil {
		panic(err)
	}
	defer os.RemoveAll(dir)

	fmt.Println("PRE-TRADE RISK ENGINE DEMO — SIMULATED DATA, PAPER ACTIVITY ONLY")
	fmt.Println("Quotes, fills, the clock and the OMS below are all simulated; no order leaves this process.")

	// A mock OMS that listens for kill-switch notifications.
	omsGot := make(chan string, 8)
	oms := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var m struct {
			KillSwitch struct {
				Scope, Target string
				Enabled       bool
			} `json:"kill_switch"`
		}
		json.NewDecoder(r.Body).Decode(&m)
		omsGot <- fmt.Sprintf("scope=%s target=%q enabled=%v", m.KillSwitch.Scope, m.KillSwitch.Target, m.KillSwitch.Enabled)
	}))
	defer oms.Close()

	cfg := app.Config{PolicyPath: policyPath, DataDir: dir, Clock: clk, AdminToken: adminToken, FeedToken: feedToken, Webhooks: []string{oms.URL}}
	start := func() (*app.App, *httptest.Server) {
		a, err := app.Open(cfg)
		if err != nil {
			fmt.Println("cannot start engine:", err)
			os.Exit(1)
		}
		srv := httptest.NewServer(a.Handler)
		base = srv.URL
		return a, srv
	}
	a, srv := start()

	section("1. An order inside every limit is ALLOWED and capacity is reserved")
	quote("XYZ", "105.18", "105.22")
	d := order("o-123", "a-1", "XYZ", "BUY", 100, "105.20")
	fmt.Println("  reason trace for o-123 (check, outcome, observed vs threshold):")
	for _, c := range d["trace"].([]any) {
		m := c.(map[string]any)
		line := fmt.Sprintf("    %-22s %-5s", m["name"], m["outcome"])
		if m["threshold"] != nil {
			line += fmt.Sprintf(" %v vs %v", m["observed"], m["threshold"])
		}
		fmt.Println(line)
	}
	call("POST", "/v1/risk/reservations/o-123/commit", "", "", "status", "remaining", "notional")
	call("POST", "/v1/risk/reservations/o-123/fills", "", `{"fill_id":"f-1","qty":100,"price":105.19}`, "position.qty", "reservation.status")

	section("2. Orders that break a rule are REJECTED with stable reason codes")
	order("o-124", "a-1", "XYZ", "BUY", 100, "112.00")  // 6.5% through the mid
	order("o-125", "a-1", "XYZ", "BUY", 5000, "105.20") // too big every way
	order("o-126", "a-2", "XYZ", "BUY", 200, "105.20")
	order("o-127", "a-2", "XYZ", "BUY", 200, "105.20") // second one would breach a-2's 40,000 symbol cap
	order("o-128", "nobody", "XYZ", "BUY", 1, "105.20")

	section("3. A large order goes to REVIEW; capacity is held until a human decides")
	order("o-130", "a-1", "XYZ", "BUY", 600, "105.20")
	call("POST", "/v1/risk/reservations/o-130/commit", "", "", "code")
	call("POST", "/v1/risk/reservations/o-130/approve", adminToken, `{"approved_by":"risk-officer"}`, "status", "expires_at")
	call("POST", "/v1/risk/reservations/o-130/release", "", `{"reason":"cancelled by trader"}`, "status", "release_reason")

	section("4. Duplicate delivery returns the original decision; id reuse is rejected")
	call("POST", "/v1/risk/evaluate", "", `{"order_id":"o-123","account_id":"a-1","symbol":"XYZ","side":"BUY","qty":100,"limit_price":105.20}`,
		"decision", "replayed", "audit_seq")
	order("o-123", "a-1", "XYZ", "BUY", 999, "105.20")

	section("5. FAILURE DEMO: the market-data feed disconnects")
	clk.advance(5 * time.Second)
	order("o-140", "a-1", "XYZ", "SELL", 100, "105.20")
	fmt.Println("  (quote is 5s old, policy allows 2s: the engine fails closed rather than trust it)")
	quote("XYZ", "105.18", "105.22")
	order("o-141", "a-1", "XYZ", "SELL", 100, "105.20")

	section("6. Kill switch: blocks evaluate and commit, and is pushed to the OMS")
	t0 := time.Now()
	call("POST", "/v1/risk/kill-switches/accounts/a-1/activate", adminToken, `{"activated_by":"risk-officer","reason":"demo"}`, "durable", "audit_seq")
	fmt.Printf("  mock OMS received: %s  (%.2fms after the request was sent)\n", <-omsGot, float64(time.Since(t0).Microseconds())/1000)
	order("o-150", "a-1", "XYZ", "BUY", 1, "105.20")
	call("POST", "/v1/risk/reservations/o-141/commit", "", "", "code", "reservation.status", "reservation.release_reason")
	order("o-151", "a-2", "XYZ", "SELL", 10, "105.20") // other accounts are unaffected
	call("POST", "/v1/risk/kill-switches/accounts/a-1/activate", "", `{"activated_by":"intruder"}`, "error")

	section("7. Restart: state is rebuilt from the audit log alone")
	before := call("GET", "/v1/risk/accounts/a-1", "", "", "positions", "realized_pnl_today")
	srv.Close()
	a.Close()
	a, srv = start()
	defer srv.Close()
	defer a.Close()
	fmt.Printf("\n    ... engine restarted, %d audit records replayed ...\n", a.Recovered)
	after := call("GET", "/v1/risk/accounts/a-1", "", "", "positions", "realized_pnl_today")
	b1, _ := json.Marshal(before)
	b2, _ := json.Marshal(after)
	fmt.Printf("  account state identical across restart: %v\n", bytes.Equal(b1, b2))
	order("o-160", "a-1", "XYZ", "BUY", 1, "105.20") // kill switch survived; quotes did not, but the switch is checked first
	order("o-161", "a-2", "XYZ", "BUY", 1, "105.20") // no quote since restart: fail closed

	section("8. Audit evidence: hash-chained, signed, tamper-evident")
	call("GET", "/v1/audit/verify", adminToken, "", "valid", "records", "head_hash")
	head := call("GET", "/v1/audit/head", "", "", "seq", "public_key")
	pubBytes, _ := hex.DecodeString(head["public_key"].(string))
	logPath := filepath.Join(dir, "audit.log")
	raw, _ := os.ReadFile(logPath)
	forged := filepath.Join(dir, "forged.log")
	os.WriteFile(forged, bytes.Replace(raw, []byte(`"decision":"REJECT"`), []byte(`"decision":"ALLOW"`), 1), 0o600)
	_, _, err = audit.Verify(forged, ed25519.PublicKey(pubBytes))
	fmt.Printf("\n  a copy of the log with one REJECT edited to ALLOW fails verification:\n    %v\n", err)

	fmt.Println("\nDemo complete. All activity above was simulated.")
}
