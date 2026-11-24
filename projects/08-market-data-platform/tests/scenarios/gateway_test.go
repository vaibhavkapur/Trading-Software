package scenarios

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/prometheus/client_golang/prometheus/testutil"

	"mdp/src/gateway"
	"mdp/src/storage"
	"mdp/tests/harness"
)

// get performs an API request and decodes the JSON body.
func get(t *testing.T, g *harness.Gateway, token, path string) (int, map[string]any) {
	t.Helper()
	req, _ := http.NewRequest(http.MethodGet, g.URL+path, nil)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	var body map[string]any
	if err := json.Unmarshal(raw, &body); err != nil {
		t.Fatalf("GET %s: body is not JSON: %q", path, raw)
	}
	return resp.StatusCode, body
}

func errCode(body map[string]any) string {
	e, _ := body["error"].(map[string]any)
	code, _ := e["code"].(string)
	return code
}

// wsClient reads a live subscription.
type wsClient struct {
	t    *testing.T
	conn *websocket.Conn
}

func dial(t *testing.T, g *harness.Gateway, token, query string) (*wsClient, *http.Response, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	opts := &websocket.DialOptions{HTTPHeader: http.Header{}}
	if token != "" {
		opts.HTTPHeader.Set("Authorization", "Bearer "+token)
	}
	conn, resp, err := websocket.Dial(ctx, g.WS+"/v1/stream?"+query, opts)
	if err != nil {
		return nil, resp, err
	}
	conn.SetReadLimit(1 << 20)
	t.Cleanup(func() { conn.CloseNow() })
	return &wsClient{t: t, conn: conn}, resp, nil
}

func subscribe(t *testing.T, g *harness.Gateway, query string) *wsClient {
	t.Helper()
	c, _, err := dial(t, g, "", query)
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	if first := c.next(); first["type"] != "subscribed" {
		t.Fatalf("first message should be subscribed, got %v", first)
	}
	return c
}

func (c *wsClient) next() map[string]any {
	c.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_, data, err := c.conn.Read(ctx)
	if err != nil {
		c.t.Fatalf("websocket read: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal(data, &m); err != nil {
		c.t.Fatalf("websocket message is not JSON: %q", data)
	}
	return m
}

// until reads messages until match accepts one, returning everything read.
func (c *wsClient) until(what string, match func(map[string]any) bool) []map[string]any {
	c.t.Helper()
	var seen []map[string]any
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		m := c.next()
		seen = append(seen, m)
		if match(m) {
			return seen
		}
	}
	c.t.Fatalf("timed out waiting for %s", what)
	return nil
}

func statusOf(m map[string]any, instrument string) map[string]any {
	if m["type"] != "status" {
		return nil
	}
	for _, v := range m["instruments"].([]any) {
		if is := v.(map[string]any); is["instrument_id"] == instrument {
			return is
		}
	}
	return nil
}

func TestWebSocketDeliversCanonicalEventsWithFreshness(t *testing.T) {
	e := harness.New(t)
	e.StartIngestors()
	e.StartNormalizer()
	g := e.StartGateway(gateway.OpenEntitlements())
	c := subscribe(t, g, "instrument_id=venueA:XYZ")

	e.Sim.Step(60)
	var trade map[string]any
	seen := c.until("a fresh status after a trade", func(m map[string]any) bool {
		if m["type"] == "trade" && trade == nil {
			trade = m
		}
		is := statusOf(m, "venueA:XYZ")
		return trade != nil && is != nil && is["stale"] == false
	})

	// The trade carries the fields of the specification's example message.
	for _, f := range []string{"instrument_id", "price_ticks", "quantity", "event_time", "receive_time", "trade_id", "seq", "session"} {
		if _, ok := trade[f]; !ok {
			t.Errorf("trade message lacks %q: %v", f, trade)
		}
	}
	if px, ok := trade["price_ticks"].(float64); !ok || px != float64(int64(px)) || px <= 0 {
		t.Errorf("price_ticks should be a positive integer: %v", trade["price_ticks"])
	}
	for _, m := range seen {
		if id, ok := m["instrument_id"]; ok && id != "venueA:XYZ" {
			t.Fatalf("received an event for an instrument not subscribed to: %v", m)
		}
	}

	// The feed goes quiet: the subscriber is told, with an age on one clock.
	seen = c.until("a stale status", func(m map[string]any) bool {
		is := statusOf(m, "venueA:XYZ")
		return is != nil && is["stale"] == true
	})
	is := statusOf(seen[len(seen)-1], "venueA:XYZ")
	if age, _ := is["age_ms"].(float64); age < 300 {
		t.Errorf("stale instrument should report age_ms above the 300ms threshold, got %v", is["age_ms"])
	}
	if is["last_event_time"] == nil || is["last_receive_time"] == nil {
		t.Errorf("status should carry both clocks: %v", is)
	}
}

func TestWebSocketChannelFilter(t *testing.T) {
	e := harness.New(t)
	e.StartIngestors()
	n := e.StartNormalizer()
	g := e.StartGateway(gateway.OpenEntitlements())
	c := subscribe(t, g, "instrument_id=venueB:XYZ.B,venueB:ABC.B,venueB:LMN.B&channels=depth")

	e.Sim.Step(300)
	e.WaitRaw(600)
	e.Settle(n, nil)
	depth := 0
	c.until("a status after the burst", func(m map[string]any) bool {
		switch m["type"] {
		case "depth":
			depth++
			if _, ok := m["bids"].([]any); !ok {
				t.Errorf("depth message without bids: %v", m)
			}
		case "trade", "quote":
			t.Fatalf("channels=depth delivered a %v", m["type"])
		}
		return m["type"] == "status" && depth > 0
	})
}

func TestWebSocketReportsFeedGap(t *testing.T) {
	e := harness.New(t)
	e.StartIngestors()
	e.StartNormalizer()
	g := e.StartGateway(gateway.OpenEntitlements())
	c := subscribe(t, g, "instrument_id=venueB:XYZ.B")

	e.Sim.Step(20)
	e.Sim.Beta.LoseNext(6)
	e.Sim.Step(100)

	seen := c.until("a feed gap notice", func(m map[string]any) bool { return m["type"] == "gap" })
	gap := seen[len(seen)-1]
	if gap["reason"] != "feed_gap" || gap["missing"] != float64(6) || gap["venue"] != "venueB" {
		t.Errorf("unexpected gap notice: %v", gap)
	}
	// Beta's sequence covers the whole feed, so the notice names no single instrument.
	if _, scoped := gap["instrument_id"]; scoped {
		t.Errorf("a feed-wide gap should not name an instrument: %v", gap)
	}
	c.until("a status counting the gap", func(m map[string]any) bool {
		is := statusOf(m, "venueB:XYZ.B")
		return is != nil && is["feed_gaps"] == float64(1)
	})
}

func TestWebSocketSuppressesRedeliveredEvents(t *testing.T) {
	e := harness.New(t)
	g := e.StartGateway(gateway.OpenEntitlements())
	c := subscribe(t, g, "instrument_id=venueB:XYZ.B&channels=trade")

	// The normalizer publishes but cannot checkpoint, then restarts and
	// republishes the same events.
	e.Mem.SetProduceHook(checkpointFault("normalizer/"))
	publishRaw(e, 1, 2, 3, 4, 5)
	n := e.StartNormalizer()
	e.Settle(n, nil)
	n.Stop()
	e.Mem.SetProduceHook(nil)
	n = e.StartNormalizer()
	e.Settle(n, nil)
	publishRaw(e, 6)

	var seqs []float64
	c.until("trade 6", func(m map[string]any) bool {
		if m["type"] == "trade" {
			seqs = append(seqs, m["seq"].(float64))
		}
		return m["type"] == "trade" && m["seq"] == float64(6)
	})
	if fmt.Sprint(seqs) != "[1 2 3 4 5 6]" {
		t.Errorf("subscriber saw sequence numbers %v, want each of 1..6 once", seqs)
	}
}

func entitlements(t *testing.T, clients map[string][]string) *gateway.Entitlements {
	t.Helper()
	var list []map[string]any
	for token, venues := range clients {
		sum := sha256.Sum256([]byte(token))
		list = append(list, map[string]any{"name": "client-" + venues[0], "token_sha256": hex.EncodeToString(sum[:]), "venues": venues})
	}
	b, _ := json.Marshal(map[string]any{"clients": list})
	ent, err := gateway.ParseEntitlements(b)
	if err != nil {
		t.Fatal(err)
	}
	return ent
}

func TestEntitlementsLimitWhatACallerCanRead(t *testing.T) {
	e := harness.New(t)
	g := e.StartGateway(entitlements(t, map[string][]string{"token-a": {"venueA"}, "token-all": {"*"}}))

	if code, body := get(t, g, "", "/v1/instruments"); code != 401 || errCode(body) != "UNAUTHENTICATED" {
		t.Errorf("no token: got %d %v, want 401 UNAUTHENTICATED", code, body)
	}
	if code, _ := get(t, g, "wrong", "/v1/instruments"); code != 401 {
		t.Errorf("wrong token: got %d, want 401", code)
	}
	code, body := get(t, g, "token-a", "/v1/instruments")
	if code != 200 {
		t.Fatalf("venueA client: got %d", code)
	}
	for _, v := range body["instruments"].([]any) {
		if venue := v.(map[string]any)["venue"]; venue != "venueA" {
			t.Errorf("venueA client can see an instrument of %v", venue)
		}
	}
	if _, body := get(t, g, "token-all", "/v1/instruments"); body["count"] != float64(len(e.Reg.All())) {
		t.Errorf("wildcard client sees %v instruments, want %d", body["count"], len(e.Reg.All()))
	}
	if code, body := get(t, g, "token-a", "/v1/trades?instrument_id=venueB:XYZ.B"); code != 403 || errCode(body) != "NOT_ENTITLED" {
		t.Errorf("venueA client reading venueB: got %d %v, want 403 NOT_ENTITLED", code, body)
	}
	if code, _ := get(t, g, "token-a", "/v1/trades?instrument_id=venueA:XYZ"); code != 200 {
		t.Errorf("venueA client reading venueA: got %d, want 200", code)
	}
	// A token in the URL is not accepted: it would end up in access logs.
	if code, _ := get(t, g, "", "/v1/instruments?token=token-a&access_token=token-a"); code != 401 {
		t.Errorf("token in the URL: got %d, want 401", code)
	}

	// The same rules guard the live stream, before the upgrade.
	if _, resp, err := dial(t, g, "", "instrument_id=venueA:XYZ"); err == nil || resp == nil || resp.StatusCode != 401 {
		t.Errorf("stream without token: err=%v, want HTTP 401", err)
	}
	if _, resp, err := dial(t, g, "token-a", "instrument_id=venueA:XYZ,venueB:XYZ.B"); err == nil || resp == nil || resp.StatusCode != 403 {
		t.Errorf("stream for an unentitled venue: err=%v, want HTTP 403", err)
	}
	if _, _, err := dial(t, g, "token-a", "instrument_id=venueA:XYZ"); err != nil {
		t.Errorf("stream for an entitled venue: %v", err)
	}
}

func TestHistoricalQueryPaginatesWithoutLossOrRepeat(t *testing.T) {
	e := harness.New(t)
	e.StartIngestors()
	n, s := e.StartNormalizer(), e.StartSink()
	g := e.StartGateway(gateway.OpenEntitlements())
	e.Sim.Step(400)
	e.WaitRaw(800)
	e.Settle(n, s)

	const id = "venueA:XYZ"
	from, to := time.Now().Add(-time.Hour), time.Now().Add(time.Hour)
	want, err := e.Store.Trades(e.Ctx, storage.RangeQuery{InstrumentID: id, From: from, To: to})
	if err != nil || len(want) < 20 {
		t.Fatalf("test setup: %d trades in the store, err %v", len(want), err)
	}

	var got []map[string]any
	pages, cursor := 0, ""
	for {
		path := "/v1/trades?instrument_id=" + id + "&limit=7"
		if cursor != "" {
			path += "&cursor=" + url.QueryEscape(cursor)
		}
		code, body := get(t, g, "", path)
		if code != 200 {
			t.Fatalf("page %d: %d %v", pages, code, body)
		}
		if pages == 0 {
			if body["tick_size"] != "0.01" || body["lot_size"] != "1" {
				t.Errorf("response should say how to convert ticks and lots: %v %v", body["tick_size"], body["lot_size"])
			}
			clk, _ := body["clocks"].(map[string]any)
			if clk["event_time"] == nil || clk["receive_time"] == nil {
				t.Errorf("response should identify the clock of each timestamp: %v", body["clocks"])
			}
			ret, _ := body["retention"].(map[string]any)
			if ret["days"] != float64(90) || ret["oldest_queryable"] == nil || ret["measured_on"] != "event_time" {
				t.Errorf("response should state the retention and the clock it is measured on: %v", body["retention"])
			}
		}
		for _, v := range body["trades"].([]any) {
			got = append(got, v.(map[string]any))
		}
		pages++
		next, _ := body["next_cursor"].(string)
		if next == "" {
			break
		}
		cursor = next
	}
	if len(got) != len(want) {
		t.Fatalf("paged through %d trades in %d pages, store has %d", len(got), pages, len(want))
	}
	for i, tr := range got {
		if tr["trade_id"] != want[i].TradeID {
			t.Fatalf("row %d: trade_id %v, want %s (ascending event_time, then trade_id)", i, tr["trade_id"], want[i].TradeID)
		}
	}

	// Ranges are half-open: [from, to).
	pivot := want[len(want)/2].EventTime
	rng := func(from, to time.Time) int {
		q := url.Values{"instrument_id": {id}, "from": {from.Format(time.RFC3339Nano)}, "to": {to.Format(time.RFC3339Nano)}, "limit": {"10000"}}
		code, body := get(t, g, "", "/v1/trades?"+q.Encode())
		if code != 200 {
			t.Fatalf("range query: %d %v", code, body)
		}
		return int(body["count"].(float64))
	}
	before, after := rng(from, pivot), rng(pivot, to)
	if before+after != len(want) {
		t.Errorf("[from,pivot) has %d and [pivot,to) has %d; together they should be all %d", before, after, len(want))
	}
	if before != len(want)/2 {
		t.Errorf("[from,pivot) should exclude the trade at pivot: got %d rows, want %d", before, len(want)/2)
	}

	// Quotes and depth are served the same way.
	if code, body := get(t, g, "", "/v1/quotes?instrument_id="+id+"&limit=5"); code != 200 || body["count"] != float64(5) || body["next_cursor"] == nil {
		t.Errorf("quotes: %d %v", code, body["count"])
	}
	if code, body := get(t, g, "", "/v1/depth?instrument_id=venueB:XYZ.B&limit=2"); code != 200 || body["count"] != float64(2) {
		t.Errorf("depth: %d %v", code, body["count"])
	}
}

func TestHistoricalQueryRejectsBadInput(t *testing.T) {
	e := harness.New(t)
	g := e.StartGateway(gateway.OpenEntitlements())
	cases := []struct {
		path string
		code int
		err  string
	}{
		{"/v1/trades", 400, "BAD_REQUEST"},
		{"/v1/trades?instrument_id=venueZ:NOPE", 404, "UNKNOWN_INSTRUMENT"},
		{"/v1/trades?instrument_id=venueA:XYZ&from=yesterday", 400, "BAD_REQUEST"},
		{"/v1/trades?instrument_id=venueA:XYZ&from=2026-01-02T00:00:00Z&to=2026-01-01T00:00:00Z", 400, "BAD_REQUEST"},
		{"/v1/trades?instrument_id=venueA:XYZ&limit=0", 400, "BAD_REQUEST"},
		{"/v1/trades?instrument_id=venueA:XYZ&limit=1000000", 400, "BAD_REQUEST"},
		{"/v1/trades?instrument_id=venueA:XYZ&cursor=%21%21", 400, "BAD_REQUEST"},
		{"/v1/quotes?instrument_id=venueA:XYZ&cursor=bm90LWEtY3Vyc29y", 400, "BAD_REQUEST"},
		{"/v1/gaps", 400, "BAD_REQUEST"},
	}
	for _, c := range cases {
		code, body := get(t, g, "", c.path)
		if code != c.code || errCode(body) != c.err {
			t.Errorf("GET %s: got %d %s, want %d %s", c.path, code, errCode(body), c.code, c.err)
		}
	}
	for _, q := range []string{"", "instrument_id=venueZ:NOPE", "instrument_id=venueA:XYZ&channels=candles"} {
		if _, resp, err := dial(t, g, "", q); err == nil || resp == nil || resp.StatusCode < 400 || resp.StatusCode > 404 {
			t.Errorf("stream?%s should be refused with a 4xx, err=%v", q, err)
		}
	}
	if code, body := get(t, g, "", "/v1/instruments?search=xyz"); code != 200 || body["count"] != float64(2) {
		t.Errorf("instrument search for xyz: %d %v, want 2 matches", code, body["count"])
	}
}

func TestGapsAndHealthAreQueryable(t *testing.T) {
	e := harness.New(t)
	e.StartIngestors()
	n, s := e.StartNormalizer(), e.StartSink()
	g := e.StartGateway(gateway.OpenEntitlements())
	e.Sim.Step(20)
	e.Sim.Beta.LoseNext(3)
	e.Sim.Step(100)
	e.WaitRaw(240 - 3)
	e.Settle(n, s)

	// A feed-wide gap is reported for every instrument of the venue.
	for _, id := range []string{"venueB:XYZ.B", "venueB:LMN.B"} {
		code, body := get(t, g, "", "/v1/gaps?instrument_id="+id)
		if code != 200 || body["count"] != float64(1) {
			t.Fatalf("gaps for %s: %d %v", id, code, body)
		}
		gap := body["gaps"].([]any)[0].(map[string]any)
		if gap["from_seq"] != float64(21) || gap["to_seq"] != float64(23) || gap["provider"] != "beta" {
			t.Errorf("unexpected gap: %v", gap)
		}
		// A gap has no venue timestamp; its retention runs on the ingestor clock.
		if ret, _ := body["retention"].(map[string]any); ret["measured_on"] != "receive_time" {
			t.Errorf("gap retention should be measured on receive_time: %v", body["retention"])
		}
	}
	if _, body := get(t, g, "", "/v1/gaps?instrument_id=venueA:XYZ"); body["count"] != float64(0) {
		t.Errorf("venueA has no gaps, got %v", body["count"])
	}

	var health map[string]any
	e.Eventually("ingest health to show the gap", func() bool {
		_, health = get(t, g, "", "/v1/health")
		for _, v := range health["ingest"].([]any) {
			if ih := v.(map[string]any); ih["provider"] == "beta" && ih["gaps"] == float64(1) && ih["connected"] == true {
				return true
			}
		}
		return false
	})
	if ok := health["store"].(map[string]any)["ok"]; ok != true {
		t.Errorf("store should be reported healthy: %v", health["store"])
	}
	if len(health["instruments"].([]any)) != len(e.Reg.All()) {
		t.Errorf("health should cover every instrument: %v", health["instruments"])
	}
}

func TestStoreOutageIsReportedWithoutLeakingDetails(t *testing.T) {
	e := harness.New(t)
	g := e.StartGateway(gateway.OpenEntitlements())
	e.MemDB.SetFault(func(string, int) error {
		return errors.New("dial tcp 10.1.2.3:8123: connection refused (user=mdp)")
	})
	code, body := get(t, g, "", "/v1/trades?instrument_id=venueA:XYZ")
	if code != 503 || errCode(body) != "STORE_UNAVAILABLE" {
		t.Fatalf("got %d %v, want 503 STORE_UNAVAILABLE", code, body)
	}
	if raw, _ := json.Marshal(body); strings.Contains(string(raw), "10.1.2.3") || strings.Contains(string(raw), "user=") {
		t.Errorf("error response leaks store connection details: %s", raw)
	}
	if _, health := get(t, g, "", "/v1/health"); health["store"].(map[string]any)["ok"] != false {
		t.Errorf("health should report the store down: %v", health["store"])
	}
	resp, err := http.Get(g.URL + "/metrics")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	metrics, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(metrics), `mdp_http_requests_total{code="503",route="trades"} 1`) {
		t.Errorf("the failed request should be counted in /metrics")
	}
}

func TestSlowWebSocketClientIsToldHowMuchItMissed(t *testing.T) {
	e := harness.New(t)
	e.Stack.Opt.Hub.QueueSize = 64
	e.Stack.Opt.Server.WriteTimeout = time.Minute // slow, not gone: the gateway must not hang up on it
	e.StartIngestors()
	e.StartNormalizer()
	g := e.StartGateway(gateway.OpenEntitlements())

	// Every venueB instrument, every channel: one message per beta event.
	const query = "instrument_id=venueB:XYZ.B,venueB:ABC.B,venueB:LMN.B&channels=trade,quote,depth"
	slow, fast := subscribe(t, g, query), subscribe(t, g, query)
	var fastEvents atomic.Int64
	go func() { // the fast subscriber reads everything it is sent
		for {
			_, data, err := fast.conn.Read(e.Ctx)
			if err != nil {
				return
			}
			if !bytes.Contains(data, []byte(`"type":"status"`)) && !bytes.Contains(data, []byte(`"type":"gap"`)) {
				fastEvents.Add(1)
			}
		}
	}()

	// The slow subscriber stops reading. Publish, in bursts small enough for
	// the fast subscriber's queue, until the gateway's socket to the slow one
	// is full, its queue has overflowed and events have been dropped.
	published := int64(0)
	deadline := time.Now().Add(60 * time.Second)
	for testutil.ToFloat64(e.Stack.M.WSDropped) == 0 || published%1000 != 0 {
		if time.Now().After(deadline) {
			t.Fatalf("no event was dropped for a subscriber that read nothing of %d events", published)
		}
		e.Sim.Step(8)
		published += 8
		e.Eventually("the fast subscriber to keep up", func() bool { return fastEvents.Load() == published })
	}

	// It starts reading again: what was queued arrives, then the notices.
	var events, noticed, inStatus float64
	e.Eventually("every drop to be reported in-band and in status", func() bool {
		m := slow.next()
		switch m["type"] {
		case "trade", "quote", "depth":
			events++
		case "gap":
			if m["reason"] != "slow_consumer" || m["instrument_id"] == nil || m["dropped"].(float64) < 1 {
				t.Fatalf("unexpected gap notice: %v", m)
			}
			noticed += m["dropped"].(float64)
		case "status":
			inStatus = 0
			for _, v := range m["instruments"].([]any) {
				inStatus += v.(map[string]any)["dropped"].(float64)
			}
		}
		return noticed > 0 && noticed == inStatus && events+noticed == float64(published)
	})
	if total := testutil.ToFloat64(e.Stack.M.WSDropped); total != noticed {
		t.Errorf("mdp_ws_dropped_total = %v, the subscriber was told %v", total, noticed)
	}
	t.Logf("published %d; slow subscriber received %v and was told it missed %v; fast subscriber received %d",
		published, events, noticed, fastEvents.Load())
}
