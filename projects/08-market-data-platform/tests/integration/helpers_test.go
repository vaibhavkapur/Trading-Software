// Package integration runs the real adapters and the complete pipeline
// against Redpanda and ClickHouse. `make integration-test` starts both and
// sets MDP_INTEGRATION; without it every test here is skipped, visibly.
//
// Each test uses its own Kafka namespace and its own ClickHouse database and
// removes them afterwards, so runs do not interfere with each other or with
// a locally running stack.
package integration

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/twmb/franz-go/pkg/kadm"
	"github.com/twmb/franz-go/pkg/kgo"

	"mdp/src/storage"
	"mdp/src/stream"
)

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func brokers() []string {
	return strings.Split(envOr("MDP_KAFKA_BROKERS", "127.0.0.1:19092"), ",")
}

func clickhouseURL() string { return envOr("MDP_CLICKHOUSE_URL", "http://127.0.0.1:8123") }

// integration skips unless the suite was asked for, and fails (rather than
// skips) if it was asked for but is misconfigured.
func integration(t *testing.T) {
	t.Helper()
	if os.Getenv("MDP_INTEGRATION") == "" {
		t.Skip("needs Redpanda and ClickHouse: run `make integration-test`")
	}
	if os.Getenv("MDP_CLICKHOUSE_PASSWORD") == "" {
		t.Fatal("MDP_CLICKHOUSE_PASSWORD is not set; `make integration-test` reads it from .env")
	}
}

var runID = time.Now().UnixNano()
var seq int

func unique() string {
	seq++
	return fmt.Sprintf("%d_%d", runID, seq)
}

// admin returns a Kafka admin client for assertions about the broker itself.
func admin(t *testing.T) *kadm.Client {
	t.Helper()
	cl, err := kgo.NewClient(kgo.SeedBrokers(brokers()...))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(cl.Close)
	return kadm.NewClient(cl)
}

// newBus opens a KafkaBus in a namespace of its own and deletes the
// namespace's topics when the test ends.
func newBus(t *testing.T) (*stream.KafkaBus, string) {
	t.Helper()
	ns := "it" + unique() + "."
	bus, err := stream.NewKafkaBus(brokers(), "mdp-integration-test", ns)
	if err != nil {
		t.Fatal(err)
	}
	adm := admin(t)
	t.Cleanup(func() {
		bus.Close()
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		topics, err := adm.ListTopics(ctx)
		if err != nil {
			t.Logf("cleanup: list topics: %v", err)
			return
		}
		var mine []string
		for name := range topics {
			if strings.HasPrefix(name, ns) {
				mine = append(mine, name)
			}
		}
		if len(mine) > 0 {
			if _, err := adm.DeleteTopics(ctx, mine...); err != nil {
				t.Logf("cleanup: delete topics: %v", err)
			}
		}
	})
	return bus, ns
}

// chExec runs raw SQL against ClickHouse, for setup and for assertions that
// deliberately bypass the adapter.
func chExec(t *testing.T, baseURL, database, sql string) string {
	t.Helper()
	q := url.Values{}
	if database != "" {
		q.Set("database", database)
	}
	req, _ := http.NewRequest(http.MethodPost, baseURL+"/?"+q.Encode(), bytes.NewReader([]byte(sql)))
	req.Header.Set("X-ClickHouse-User", envOr("MDP_CLICKHOUSE_USER", "mdp"))
	req.Header.Set("X-ClickHouse-Key", os.Getenv("MDP_CLICKHOUSE_PASSWORD"))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("clickhouse: %v", err)
	}
	defer resp.Body.Close()
	out, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("clickhouse: %s: HTTP %d: %s", sql, resp.StatusCode, out)
	}
	return strings.TrimSpace(string(out))
}

// newStoreAt opens a ClickHouse store in a database of its own, reached
// through baseURL, and drops the database when the test ends.
func newStoreAt(t *testing.T, baseURL string, retention storage.Retention) (*storage.ClickHouse, string) {
	t.Helper()
	db := "mdp_it_" + unique()
	store := storage.NewClickHouse(storage.ClickHouseConfig{
		URL: baseURL, Database: db, User: envOr("MDP_CLICKHOUSE_USER", "mdp"),
		Password: os.Getenv("MDP_CLICKHOUSE_PASSWORD"), Retention: retention,
	})
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := store.Migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	t.Cleanup(func() { chExec(t, clickhouseURL(), "", "DROP DATABASE IF EXISTS "+db+" SYNC") })
	return store, db
}

func newStore(t *testing.T) (*storage.ClickHouse, string) {
	return newStoreAt(t, clickhouseURL(), storage.Retention{})
}

func count(t *testing.T, store *storage.ClickHouse, table string) int64 {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	n, err := store.Count(ctx, table)
	if err != nil {
		t.Fatal(err)
	}
	return n
}
