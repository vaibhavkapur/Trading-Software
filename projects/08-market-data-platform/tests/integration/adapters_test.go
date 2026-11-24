package integration

import (
	"context"
	"strconv"
	"strings"
	"testing"
	"time"

	"mdp/src/domain"
	"mdp/src/storage"
	"mdp/src/stream"
	"mdp/tests/contract"
)

func TestKafkaBusHonoursTheBusContract(t *testing.T) {
	integration(t)
	bus, _ := newBus(t)
	contract.RunBus(t, bus)
}

func TestClickHouseHonoursTheStoreContract(t *testing.T) {
	integration(t)
	store, _ := newStore(t)
	contract.RunStore(t, store)
}

func TestKafkaNamespacesAreIsolated(t *testing.T) {
	integration(t)
	a, nsA := newBus(t)
	b, nsB := newBus(t)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	spec := stream.TopicSpec{Name: "shared.name", Partitions: 1, Retention: time.Hour}
	for _, bus := range []*stream.KafkaBus{a, b} {
		if err := bus.EnsureTopics(ctx, spec); err != nil {
			t.Fatal(err)
		}
	}
	if err := a.Produce(ctx, stream.Record{Topic: "shared.name", Value: []byte("from-a")}); err != nil {
		t.Fatal(err)
	}
	endsA, _ := a.EndOffsets(ctx, "shared.name")
	endsB, _ := b.EndOffsets(ctx, "shared.name")
	tp := stream.TP{Topic: "shared.name"}
	if endsA[tp] != 1 || endsB[tp] != 0 {
		t.Errorf("end offsets: a=%d b=%d; a record produced in one namespace must not appear in the other", endsA[tp], endsB[tp])
	}

	// Callers see logical names; the broker holds the prefixed ones.
	cons, err := a.NewConsumer(ctx, stream.ConsumerConfig{Group: "g", Topics: []string{"shared.name"}})
	if err != nil {
		t.Fatal(err)
	}
	recs, err := cons.Poll(ctx)
	if err != nil || len(recs) != 1 || recs[0].Topic != "shared.name" || string(recs[0].Value) != "from-a" {
		t.Fatalf("poll: %v %v", recs, err)
	}
	if err := cons.Commit(ctx, map[stream.TP]int64{tp: 1}); err != nil {
		t.Fatal(err)
	}
	adm := admin(t)
	topics, err := adm.ListTopics(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !topics.Has(nsA+"shared.name") || !topics.Has(nsB+"shared.name") || topics.Has("shared.name") {
		t.Errorf("the broker should hold %sshared.name and %sshared.name only", nsA, nsB)
	}
	offs, err := adm.FetchOffsets(ctx, nsA+"g")
	if err != nil {
		t.Fatal(err)
	}
	if o, ok := offs.Lookup(nsA+"shared.name", 0); !ok || o.At != 1 {
		t.Errorf("group %sg should have committed offset 1 on the broker: %+v", nsA, o)
	}
	// The same group name in the other namespace starts from scratch.
	other, err := b.NewConsumer(ctx, stream.ConsumerConfig{Group: "g", Topics: []string{"shared.name"}})
	if err != nil {
		t.Fatal(err)
	}
	if pos := other.Positions()[tp]; pos != 0 {
		t.Errorf("namespace b's group g is at %d", pos)
	}

	if _, err := stream.NewKafkaBus(brokers(), "x", "bad namespace/"); err == nil {
		t.Error("a namespace with characters Kafka topic names do not allow must be refused")
	}
}

func TestKafkaTopicsAreCreatedWithTheDocumentedLayout(t *testing.T) {
	integration(t)
	bus, ns := newBus(t)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	layout := stream.DefaultLayout()
	if err := bus.EnsureTopics(ctx, layout.Specs()...); err != nil {
		t.Fatal(err)
	}
	if err := bus.EnsureTopics(ctx, layout.Specs()...); err != nil {
		t.Fatalf("a second service creating the same layout: %v", err)
	}
	adm := admin(t)
	var names []string
	for _, s := range layout.Specs() {
		names = append(names, ns+s.Name)
	}
	topics, err := adm.ListTopics(ctx, names...)
	if err != nil {
		t.Fatal(err)
	}
	configs, err := adm.DescribeTopicConfigs(ctx, names...)
	if err != nil {
		t.Fatal(err)
	}
	value := func(topic, key string) string {
		rc, err := configs.On(topic, nil)
		if err != nil {
			t.Fatalf("config of %s: %v", topic, err)
		}
		for _, c := range rc.Configs {
			if c.Key == key && c.Value != nil {
				return *c.Value
			}
		}
		return ""
	}
	for _, s := range layout.Specs() {
		name := ns + s.Name
		if got := len(topics[name].Partitions); got != int(s.Partitions) {
			t.Errorf("%s: %d partitions, want %d", s.Name, got, s.Partitions)
		}
		if s.Compact {
			if got := value(name, "cleanup.policy"); got != "compact" {
				t.Errorf("%s: cleanup.policy %q, want compact", s.Name, got)
			}
		} else if got, want := value(name, "retention.ms"), s.Retention.Milliseconds(); got != itoa(want) {
			t.Errorf("%s: retention.ms %q, want %d", s.Name, got, want)
		}
	}

	// A service configured with a different partition count must not start:
	// it would route keys to different partitions than everyone else.
	other := layout
	other.Partitions = 8
	if err := bus.EnsureTopics(ctx, other.Specs()...); err == nil || !strings.Contains(err.Error(), "partitions") {
		t.Errorf("mismatched partition count: %v", err)
	}
}

func itoa(n int64) string { return strconv.FormatInt(n, 10) }

func TestClickHouseSchemaAndDeduplication(t *testing.T) {
	integration(t)
	store, db := newStore(t)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()

	for table, ttl := range map[string]string{"trades": "toIntervalDay(90)", "quotes": "toIntervalDay(30)", "depth": "toIntervalDay(7)", "feed_gaps": "toIntervalDay(90)"} {
		ddl := chExec(t, clickhouseURL(), db, "SELECT create_table_query FROM system.tables WHERE database = currentDatabase() AND name = '"+table+"'")
		if !strings.Contains(ddl, "ReplacingMergeTree") {
			t.Errorf("%s is not a ReplacingMergeTree: %s", table, ddl)
		}
		if !strings.Contains(ddl, "TTL") || !strings.Contains(ddl, ttl) {
			t.Errorf("%s: TTL %s not found in: %s", table, ttl, ddl)
		}
	}
	// Each timestamp column says which clock it is.
	comments := chExec(t, clickhouseURL(), db, "SELECT concat(name, '=', comment) FROM system.columns WHERE database = currentDatabase() AND table = 'trades' AND name IN ('event_ts','recv_ts','persist_ts') ORDER BY name")
	for _, want := range []string{"event_ts=venue clock", "recv_ts=ingestor host clock", "persist_ts=store clock"} {
		if !strings.Contains(comments, want) {
			t.Errorf("column comments %q lack %q", comments, want)
		}
	}

	// The same fact written three times in separate inserts: three physical
	// rows until ClickHouse merges them, one row through the adapter always.
	at := time.Now().UTC().Add(-time.Minute)
	tr := contract.Trade("venueA:XYZ", at, "T1", 1)
	for i := 0; i < 3; i++ {
		if err := store.InsertTrades(ctx, []domain.Trade{tr}); err != nil {
			t.Fatal(err)
		}
	}
	physical := chExec(t, clickhouseURL(), db, "SELECT count() FROM trades")
	if n := count(t, store, "trades"); n != 1 {
		t.Errorf("adapter count = %d, want 1 (physical rows before merge: %s)", n, physical)
	}
	got, err := store.Trades(ctx, storage.RangeQuery{InstrumentID: "venueA:XYZ", From: at.Add(-time.Hour), To: at.Add(time.Hour)})
	if err != nil || len(got) != 1 {
		t.Fatalf("query returned %d rows, %v; want 1", len(got), err)
	}
	t.Logf("physical rows before merge: %s; rows returned with FINAL: 1", physical)
	chExec(t, clickhouseURL(), db, "OPTIMIZE TABLE trades FINAL")
	if after := chExec(t, clickhouseURL(), db, "SELECT count() FROM trades"); after != "1" {
		t.Errorf("after a merge the table physically holds %s rows, want 1", after)
	}
}

// Expiry must be computed on the stored 64-bit timestamp. An earlier schema
// computed it through ClickHouse's 32-bit DateTime, which wraps after 2106:
// the server accepted such a row and deleted it as expired. The adapter
// refuses those timestamps anyway; this writes around the adapter to check
// the table itself no longer loses an acknowledged row.
func TestClickHouseExpiryDoesNotWrap(t *testing.T) {
	integration(t)
	_, db := newStore(t)
	for _, table := range []string{"trades", "quotes", "depth", "feed_gaps"} {
		ddl := chExec(t, clickhouseURL(), db, "SELECT create_table_query FROM system.tables WHERE database = currentDatabase() AND name = '"+table+"'")
		if strings.Contains(ddl, "toDateTime(") {
			t.Errorf("%s computes its TTL through a 32-bit DateTime: %s", table, ddl)
		}
	}
	chExec(t, clickhouseURL(), db, `INSERT INTO trades (instrument_id, trade_id, price_ticks, quantity, event_ts, recv_ts) SETTINGS async_insert=0 VALUES
		('venueA:XYZ', 'far-future', 1, 1, '2107-01-01 00:00:00', '2107-01-01 00:00:00'),
		('venueA:XYZ', 'recent', 1, 1, now64(9) - INTERVAL 1 DAY, now64(9) - INTERVAL 1 DAY),
		('venueA:XYZ', 'expired', 1, 1, now64(9) - INTERVAL 200 DAY, now64(9) - INTERVAL 200 DAY)`)
	chExec(t, clickhouseURL(), db, "OPTIMIZE TABLE trades FINAL")
	got := chExec(t, clickhouseURL(), db, "SELECT arrayStringConcat(arraySort(groupArray(trade_id)), ',') FROM trades FINAL")
	if got != "far-future,recent" {
		t.Errorf("rows left after a merge: %q; want the far-future and recent rows kept and only the row past its 90-day retention removed", got)
	}
}
