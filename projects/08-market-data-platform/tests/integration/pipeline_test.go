package integration

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"reflect"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/twmb/franz-go/pkg/kadm"

	"mdp/src/domain"
	"mdp/src/gateway"
	"mdp/src/storage"
	"mdp/src/stream"
	"mdp/tests/contract"
	"mdp/tests/harness"
)

// pipeline builds the real services on Redpanda and ClickHouse, with the
// simulated venues as the only stand-in. storeURL lets a test put a proxy
// in front of ClickHouse.
type pipeline struct {
	*harness.Env
	ns    string
	db    string
	store *storage.ClickHouse
}

func newPipeline(t *testing.T, storeURL string) *pipeline {
	t.Helper()
	bus, ns := newBus(t)
	store, db := newStoreAt(t, storeURL, storage.Retention{})
	opt := harness.FastOptions()
	// Batch sizes closer to a real deployment: each flush is a ClickHouse part.
	opt.Sink.FlushEvery = 50 * time.Millisecond
	opt.Sink.LagEvery, opt.Normalize.LagEvery, opt.Hub.LagEvery = 100*time.Millisecond, 100*time.Millisecond, 100*time.Millisecond
	return &pipeline{Env: harness.NewWith(t, bus, store, opt), ns: ns, db: db, store: store}
}

func (p *pipeline) get(g *harness.Gateway, path string) (int, map[string]any) {
	p.T.Helper()
	resp, err := http.Get(g.URL + path)
	if err != nil {
		p.T.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	var body map[string]any
	if err := json.Unmarshal(raw, &body); err != nil {
		p.T.Fatalf("GET %s: %d, not JSON: %q", path, resp.StatusCode, raw)
	}
	return resp.StatusCode, body
}

// physical counts rows as they lie on disk, duplicates included.
func (p *pipeline) physical() int {
	total := 0
	for _, table := range []string{"trades", "quotes", "depth"} {
		var n int
		fmt.Sscan(chExec(p.T.(*testing.T), clickhouseURL(), p.db, "SELECT count() FROM "+table), &n)
		total += n
	}
	return total
}

func missing(gaps []domain.Gap, provider string) (n uint64) {
	for _, g := range gaps {
		if g.Provider == provider {
			n += g.Missing()
		}
	}
	return n
}

func TestPipelineOnRedpandaAndClickHouse(t *testing.T) {
	integration(t)
	p := newPipeline(t, clickhouseURL())
	ing := p.StartIngestors()
	n, s := p.StartNormalizer(), p.StartSink()
	g := p.StartGateway(gateway.OpenEntitlements())

	const id = "venueA:XYZ"
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	conn, _, err := websocket.Dial(ctx, g.WS+"/v1/stream?instrument_id="+id+"&channels=trade,quote", nil)
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	defer conn.CloseNow()
	conn.SetReadLimit(1 << 20)

	// 1. Clean flow: 300 events on each venue arrive exactly once everywhere.
	p.Sim.Step(300)
	p.WaitRaw(600)
	p.Settle(n, s)
	if got := p.Canonical(); got != 600 {
		t.Errorf("canonical events in the log = %d, want 600", got)
	}
	if got := p.Stored(); got != 600 {
		t.Errorf("rows in ClickHouse = %d, want 600", got)
	}
	if gaps, rejects := p.Gaps(), p.Rejects(); len(gaps) != 0 || len(rejects) != 0 {
		t.Errorf("clean flow produced gaps %v, rejects %v", gaps, rejects)
	}
	if got := count(t, p.store, "instruments"); got != int64(len(p.Reg.All())) {
		t.Errorf("instruments in ClickHouse = %d, want %d", got, len(p.Reg.All()))
	}

	// The live subscriber received the same events, in order, with both clocks.
	want := 0
	for _, topic := range []string{stream.TopicTrades, stream.TopicQuotes} {
		for _, r := range p.Records(topic) {
			if string(r.Key) == id {
				want++
			}
		}
	}
	if want < 20 {
		t.Fatalf("test setup: only %d events for %s", want, id)
	}
	lastSeq := map[string]float64{}
	for got := 0; got < want; {
		_, data, err := conn.Read(ctx)
		if err != nil {
			t.Fatalf("websocket read after %d of %d events: %v", got, want, err)
		}
		var m map[string]any
		json.Unmarshal(data, &m)
		kind, _ := m["type"].(string)
		if kind != "trade" && kind != "quote" {
			continue // subscribed / status
		}
		got++
		if m["instrument_id"] != id || m["event_time"] == nil || m["receive_time"] == nil || m["source"] != "live" {
			t.Fatalf("live message %v", m)
		}
		if seq := m["seq"].(float64); seq <= lastSeq[kind] {
			t.Fatalf("%s seq %v after %v", kind, seq, lastSeq[kind])
		} else {
			lastSeq[kind] = seq
		}
	}

	// The historical endpoint returns what the store holds, page by page.
	inStore, err := p.store.Trades(ctx, storage.RangeQuery{InstrumentID: id, From: time.Now().Add(-time.Hour), To: time.Now().Add(time.Hour)})
	if err != nil || len(inStore) < 10 {
		t.Fatalf("test setup: %d trades in the store, %v", len(inStore), err)
	}
	var ids []string
	for cursor, pages := "", 0; ; pages++ {
		path := "/v1/trades?instrument_id=" + id + "&limit=9"
		if cursor != "" {
			path += "&cursor=" + url.QueryEscape(cursor)
		}
		code, body := p.get(g, path)
		if code != 200 {
			t.Fatalf("page %d: %d %v", pages, code, body)
		}
		for _, v := range body["trades"].([]any) {
			ids = append(ids, v.(map[string]any)["trade_id"].(string))
		}
		cursor, _ = body["next_cursor"].(string)
		if cursor == "" {
			break
		}
	}
	var wantIDs []string
	for _, tr := range inStore {
		wantIDs = append(wantIDs, tr.TradeID)
	}
	if !reflect.DeepEqual(ids, wantIDs) {
		t.Errorf("GET /v1/trades returned %d trades over pages, the store holds %d", len(ids), len(wantIDs))
	}

	// 2. Normalizer and sink restart while the feed keeps going.
	n.Stop()
	s.Stop()
	p.Sim.Step(100)
	p.WaitRaw(800)
	n, s = p.StartNormalizer(), p.StartSink()
	p.Settle(n, s)
	if got := p.Stored(); got != 800 {
		t.Errorf("after normalizer and sink restart: rows = %d, want 800", got)
	}

	// 3. Ingestors restart: they resume from their checkpoints in the log.
	ing.Stop()
	p.Sim.Step(100)
	ing = p.StartIngestors()
	p.WaitRaw(1000)
	p.Settle(n, s)
	if got := p.Stored(); got != 1000 {
		t.Errorf("after ingestor restart: rows = %d, want 1000", got)
	}
	if gaps := p.Gaps(); len(gaps) != 0 {
		t.Errorf("restarts must not look like gaps: %v", gaps)
	}

	// 4. The venue loses five events for good: declared, stored, queryable.
	p.Sim.Beta.LoseNext(5)
	p.Sim.Step(50)
	p.WaitRaw(1095)
	p.Settle(n, s)
	if got := p.Stored(); got != 1095 {
		t.Errorf("after loss: rows = %d, want 1095", got)
	}
	if got := missing(p.Gaps(), "beta"); got != 5 {
		t.Errorf("declared missing events = %d, want 5", got)
	}
	code, body := p.get(g, "/v1/gaps?instrument_id=venueB:XYZ.B")
	if code != 200 || body["count"] != float64(1) {
		t.Errorf("GET /v1/gaps: %d %v", code, body)
	}
	p.Eventually("health to report both providers connected and the gap", func() bool {
		_, health := p.get(g, "/v1/health")
		ok := 0
		for _, v := range health["ingest"].([]any) {
			ih := v.(map[string]any)
			if ih["connected"] == true && (ih["provider"] == "alpha" || ih["provider"] == "beta" && ih["gaps"] == float64(1)) {
				ok++
			}
		}
		return ok == 2 && health["store"].(map[string]any)["ok"] == true
	})
	_ = ing
}

func TestRedeliveryIntoClickHouseIsIdempotent(t *testing.T) {
	integration(t)
	p := newPipeline(t, clickhouseURL())
	p.StartIngestors()
	n, s := p.StartNormalizer(), p.StartSink()
	p.Sim.Step(200)
	p.WaitRaw(400)
	p.Settle(n, s)
	if got := p.Stored(); got != 400 {
		t.Fatalf("rows before redelivery = %d, want 400", got)
	}
	written := func() float64 {
		return testutil.ToFloat64(p.Stack.M.SinkRows.WithLabelValues("trades")) +
			testutil.ToFloat64(p.Stack.M.SinkRows.WithLabelValues("quotes")) +
			testutil.ToFloat64(p.Stack.M.SinkRows.WithLabelValues("depth"))
	}
	if w := written(); w != 400 {
		t.Fatalf("rows written before redelivery = %v, want 400", w)
	}
	before := p.physical()

	// Lose the sink's progress: rewind its committed offsets on the broker
	// to the beginning, as if every commit since the start had been lost.
	s.Stop()
	adm := admin(t)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	group := p.ns + storage.SinkGroup
	committed, err := adm.FetchOffsets(ctx, group)
	if err != nil || len(committed.Offsets()) == 0 {
		t.Fatalf("the sink should have committed offsets to group %s: %v %v", group, committed, err)
	}
	rewound := kadm.Offsets{}
	committed.Each(func(o kadm.OffsetResponse) { rewound.AddOffset(o.Topic, o.Partition, 0, -1) })
	if resp, err := adm.CommitOffsets(ctx, group, rewound); err != nil || resp.Error() != nil {
		t.Fatalf("rewind: %v %v", err, resp.Error())
	}

	s = p.StartSink()
	p.Settle(n, s)
	if w := written(); w != 800 {
		t.Errorf("rows written after redelivery = %v, want 800 (every row a second time)", w)
	}
	if got := p.Stored(); got != 400 {
		t.Errorf("rows after redelivery = %d, want 400", got)
	}
	after := p.physical()
	t.Logf("physical rows: %d before redelivery, %d after; rows visible to queries: 400", before, after)
	for _, table := range []string{"trades", "quotes", "depth"} {
		chExec(t, clickhouseURL(), p.db, "OPTIMIZE TABLE "+table+" FINAL")
	}
	if got := p.physical(); got != 400 {
		t.Errorf("physical rows after merge = %d, want 400", got)
	}
}

// proxy forwards TCP to a target and can be switched off, which resets open
// connections and refuses new ones: an unreachable database.
type proxy struct {
	ln     net.Listener
	target string
	mu     sync.Mutex
	down   bool
	conns  map[net.Conn]struct{}
}

func newProxy(t *testing.T, target string) *proxy {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	p := &proxy{ln: ln, target: target, conns: map[net.Conn]struct{}{}}
	t.Cleanup(func() { ln.Close(); p.SetDown(true) })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go p.serve(c)
		}
	}()
	return p
}

func (p *proxy) URL() string { return "http://" + p.ln.Addr().String() }

func (p *proxy) track(c net.Conn) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.down {
		c.Close()
		return false
	}
	p.conns[c] = struct{}{}
	return true
}

func (p *proxy) serve(client net.Conn) {
	if !p.track(client) {
		return
	}
	upstream, err := net.DialTimeout("tcp", p.target, 5*time.Second)
	if err != nil || !p.track(upstream) {
		client.Close()
		return
	}
	done := func() {
		client.Close()
		upstream.Close()
		p.mu.Lock()
		delete(p.conns, client)
		delete(p.conns, upstream)
		p.mu.Unlock()
	}
	go func() { io.Copy(upstream, client); done() }()
	io.Copy(client, upstream)
	done()
}

func (p *proxy) SetDown(down bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.down = down
	if down {
		for c := range p.conns {
			c.Close()
		}
	}
}

func TestClickHouseOutageBecomesLagThenDrains(t *testing.T) {
	integration(t)
	u, _ := url.Parse(clickhouseURL())
	px := newProxy(t, u.Host)
	p := newPipeline(t, px.URL())
	p.StartIngestors()
	n, s := p.StartNormalizer(), p.StartSink()
	g := p.StartGateway(gateway.OpenEntitlements())
	p.Sim.Step(100)
	p.WaitRaw(200)
	p.Settle(n, s)
	if got := p.Stored(); got != 200 {
		t.Fatalf("rows before the outage = %d, want 200", got)
	}

	px.SetDown(true)
	p.Sim.Step(200)
	p.WaitRaw(600)
	p.Settle(n, nil) // ingest and normalization are not held up by the store
	if got := p.Canonical(); got != 600 {
		t.Errorf("canonical events during the outage = %d, want 600", got)
	}
	lag := func() (total int64) {
		lags, err := stream.Lag(p.Ctx, p.Bus, s.Progress())
		if err != nil {
			t.Fatal(err)
		}
		for _, l := range lags {
			total += l
		}
		return total
	}
	p.Eventually("the sink to report it is retrying, with lag", func() bool {
		return testutil.ToFloat64(p.Stack.M.SinkRetrying) == 1 && lag() > 0
	})
	during := lag()
	if failures := testutil.ToFloat64(p.Stack.M.SinkInsertFailures.WithLabelValues("transient")); failures == 0 {
		t.Error("failed writes should be counted")
	}
	code, body := p.get(g, "/v1/trades?instrument_id=venueA:XYZ")
	if e, _ := body["error"].(map[string]any); code != 503 || e["code"] != "STORE_UNAVAILABLE" {
		t.Errorf("historical query during the outage: %d %v, want 503 STORE_UNAVAILABLE", code, body)
	}
	if raw, _ := json.Marshal(body); strings.Contains(string(raw), "127.0.0.1") {
		t.Errorf("error response leaks the store address: %s", raw)
	}
	if _, health := p.get(g, "/v1/health"); health["store"].(map[string]any)["ok"] != false {
		t.Errorf("health should report the store down: %v", health["store"])
	}

	px.SetDown(false)
	p.Settle(n, s)
	if got := p.Stored(); got != 600 {
		t.Errorf("rows after the outage = %d, want 600: nothing lost, nothing doubled", got)
	}
	if got := lag(); got != 0 {
		t.Errorf("lag after recovery = %d", got)
	}
	p.Eventually("the sink to stop reporting retries", func() bool { return testutil.ToFloat64(p.Stack.M.SinkRetrying) == 0 })
	t.Logf("sink lag during the outage: %d records; after recovery: 0", during)
}

func TestRowRefusedByClickHouseIsDeadLettered(t *testing.T) {
	integration(t)
	p := newPipeline(t, clickhouseURL())
	// An operator-added constraint: the kind of rule only the store knows about.
	chExec(t, clickhouseURL(), p.db, "ALTER TABLE trades ADD CONSTRAINT positive_quantity CHECK quantity > 0")

	const id = "venueA:XYZ"
	at := time.Now().UTC().Add(-time.Minute)
	var recs []stream.Record
	for i := 1; i <= 9; i++ {
		tr := contract.Trade(id, at.Add(time.Duration(i)*time.Millisecond), fmt.Sprintf("T%d", i), uint64(i))
		if i == 6 {
			tr.Quantity = -1
		}
		recs = append(recs, stream.Keyed(stream.TopicTrades, p.Stack.Opt.Layout.Partitions, id, domain.Encode(tr)))
	}
	if err := p.Bus.Produce(p.Ctx, recs...); err != nil {
		t.Fatal(err)
	}
	s := p.StartSink()
	p.Settle(nil, s)

	got, err := p.store.Trades(p.Ctx, storage.RangeQuery{InstrumentID: id, From: at.Add(-time.Hour), To: at.Add(time.Hour)})
	if err != nil {
		t.Fatal(err)
	}
	var ids []string
	for _, tr := range got {
		ids = append(ids, tr.TradeID)
	}
	sort.Strings(ids)
	if want := []string{"T1", "T2", "T3", "T4", "T5", "T7", "T8", "T9"}; !reflect.DeepEqual(ids, want) {
		t.Errorf("stored trades %v, want %v", ids, want)
	}
	var rejects []domain.Reject
	for _, r := range p.Records(stream.TopicRejects) {
		var rej domain.Reject
		json.Unmarshal(r.Value, &rej)
		rejects = append(rejects, rej)
	}
	if len(rejects) != 1 || rejects[0].Reason != "STORE_REJECTED" || rejects[0].Stage != "sink" ||
		!strings.Contains(rejects[0].Detail, "positive_quantity") || !strings.Contains(rejects[0].Raw, `"trade_id":"T6"`) {
		t.Errorf("dead letters: %+v", rejects)
	}
	if got := testutil.ToFloat64(p.Stack.M.SinkRejectedRows); got != 1 {
		t.Errorf("mdp_sink_rejected_rows_total = %v, want 1", got)
	}
}

func TestArchiveFromClickHouseToParquet(t *testing.T) {
	integration(t)
	store, _ := newStore(t)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	day := time.Now().UTC().Add(-24 * time.Hour).Truncate(24 * time.Hour)
	const id, n = "venueB:XYZ.B", 12_500 // more than one page of the archive reader
	trades := make([]domain.Trade, n)
	for i := range trades {
		trades[i] = contract.Trade(id, day.Add(time.Duration(i/5)*time.Millisecond+789), fmt.Sprintf("T%05d", i), uint64(i+1))
	}
	if err := store.InsertTrades(ctx, trades); err != nil {
		t.Fatal(err)
	}
	if err := store.InsertTrades(ctx, trades[:100]); err != nil { // duplicates must not reach the archive
		t.Fatal(err)
	}
	files, err := storage.Archive(ctx, store, id, day, t.TempDir())
	if err != nil || len(files) != 1 || files[0].Rows != n {
		t.Fatalf("archive: %+v %v", files, err)
	}
	rows, err := storage.ReadParquet[storage.TradeRow](files[0].Path)
	if err != nil || len(rows) != n {
		t.Fatalf("read back %d rows: %v", len(rows), err)
	}
	for i, r := range rows {
		if got := storage.TradeFromRow(r); !reflect.DeepEqual(got, trades[i]) {
			t.Fatalf("row %d\n got %+v\nwant %+v", i, got, trades[i])
		}
	}
}

func TestClickHouseConfigurationErrors(t *testing.T) {
	integration(t)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	decade := 10 * 365 * 24 * time.Hour
	long := storage.NewClickHouse(storage.ClickHouseConfig{URL: clickhouseURL(), Database: "mdp_it_never_created", User: "mdp",
		Password: "irrelevant", Retention: storage.Retention{Trades: decade, Quotes: decade, Depth: decade, Gaps: decade}})
	if err := long.Migrate(ctx); err == nil || !strings.Contains(err.Error(), "retention") {
		t.Errorf("a retention ClickHouse cannot expire correctly must be refused before anything is created: %v", err)
	}

	const secret = "wrong-password-that-must-not-be-logged"
	bad := storage.NewClickHouse(storage.ClickHouseConfig{URL: clickhouseURL(), Database: "default", User: "mdp", Password: secret})
	err := bad.Ping(ctx)
	if err == nil {
		t.Fatal("a wrong password was accepted")
	}
	if strings.Contains(err.Error(), secret) {
		t.Errorf("the error contains the password: %v", err)
	}
	if storage.IsPermanent(err) {
		t.Error("an authentication failure is not a property of the rows; it must be retried, not dead-lettered")
	}
}
