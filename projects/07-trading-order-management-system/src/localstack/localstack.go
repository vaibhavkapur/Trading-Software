// Package localstack runs the whole system in one process for the demo and
// the benchmark: a throwaway PostgreSQL, the simulated broker and the OMS,
// each behind a real loopback HTTP listener, wired exactly as the separate
// binaries would be.
//
// SIMULATED / PAPER ONLY. Every credential is generated at start and lives
// only in memory. The production binary (cmd/omsd) does not import this
// package.
package localstack

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"sync"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5"

	"oms/src/app"
	"oms/src/auth"
	"oms/src/broker"
	"oms/src/brokersim"
	"oms/src/clock"
	"oms/src/devpg"
	"oms/src/fixed"
	"oms/src/ids"
	"oms/src/outbox"
	"oms/src/reconcile"
	"oms/src/risk"
	"oms/src/store"
)

// Options adjusts the stack. The zero value is a manual-mode broker on the
// system clock with a non-durable throwaway database.
type Options struct {
	// DatabaseURL points at an existing PostgreSQL server. A fresh database
	// is created on it and dropped on Close. Empty starts a throwaway server.
	DatabaseURL string
	// Durable keeps fsync on in the throwaway server.
	Durable bool
	// Addr and BrokerAddr are the listen addresses of the OMS and the
	// simulated broker. Empty picks a free loopback port.
	Addr, BrokerAddr string

	// Auto makes the broker acknowledge, fill and deliver webhooks by itself.
	Auto bool
	// Background runs the outbox worker continuously.
	Background bool

	Clock clock.Clock
	IDs   ids.Generator

	Outbox        outbox.Config
	Reconcile     reconcile.Config
	Store         store.Options
	Limits        *risk.StaticLimits
	Sim           func(*brokersim.Config)
	BrokerTimeout time.Duration
	Log           *slog.Logger
}

// Stack is a running system.
type Stack struct {
	App    *app.App
	Sim    *brokersim.Sim
	Broker *broker.HTTPClient
	// URL is the OMS API; BrokerURL is the simulated broker's.
	URL, BrokerURL string

	// Bearer tokens, generated at start. The trader may trade ACC-1 and
	// ACC-2, the viewer may read ACC-1, the operator runs reconciliation.
	Trader, Viewer, Operator string
	WebhookSecret, BrokerKey string

	closers []func()
}

func secret() string {
	b := make([]byte, 24)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b)
}

// Start brings the stack up. Call Close when done.
func Start(ctx context.Context, opts Options) (s *Stack, err error) {
	s = &Stack{Trader: secret(), Viewer: secret(), Operator: secret(), WebhookSecret: secret(), BrokerKey: secret()}
	defer func() {
		if err != nil {
			s.Close()
			s = nil
		}
	}()

	if opts.Clock == nil {
		opts.Clock = clock.System{}
	}
	if opts.BrokerTimeout <= 0 {
		opts.BrokerTimeout = 3 * time.Second
	}
	if opts.Limits == nil {
		opts.Limits = &risk.StaticLimits{
			Version: "local-1", AllowedSymbols: []string{"AAPL", "MSFT", "TSLA", "NVDA"},
			MaxOrderQty: 10_000, MaxOrderNotional: fixed.MustParse("5000000"), AllowMarketOrders: true,
		}
	}

	adminURL := opts.DatabaseURL
	if adminURL == "" {
		pg, err := devpg.Start(devpg.Options{Durable: opts.Durable})
		if err != nil {
			return s, err
		}
		s.closers = append(s.closers, func() { pg.Stop() })
		adminURL = pg.URL
	}
	dbURL, err := s.createDatabase(ctx, adminURL)
	if err != nil {
		return s, err
	}

	// The OMS and the broker each need the other's address, so the OMS
	// listener starts with a handler that is filled in once the app exists.
	var handler atomic.Value
	s.URL, err = s.listen(opts.Addr, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h, _ := handler.Load().(http.Handler)
		if h == nil {
			http.Error(w, "starting", http.StatusServiceUnavailable)
			return
		}
		h.ServeHTTP(w, r)
	}))
	if err != nil {
		return s, err
	}

	simCfg := brokersim.Config{
		APIKey: s.BrokerKey, WebhookURL: s.URL + "/v1/broker/webhooks", WebhookSecret: s.WebhookSecret,
		Auto: opts.Auto, AutoDeliver: opts.Auto, Clock: opts.Clock,
		AckDelay: time.Millisecond, FillDelay: time.Millisecond, CancelDelay: time.Millisecond,
		Prices: map[string]fixed.Micros{
			"AAPL": fixed.MustParse("150"), "MSFT": fixed.MustParse("400"),
			"TSLA": fixed.MustParse("250"), "NVDA": fixed.MustParse("900"),
		},
		FeePerUnit: fixed.MustParse("0.005"),
	}
	if opts.Sim != nil {
		opts.Sim(&simCfg)
	}
	s.Sim = brokersim.New(simCfg)
	s.closers = append(s.closers, s.Sim.Close)
	if s.BrokerURL, err = s.listen(opts.BrokerAddr, s.Sim.Handler()); err != nil {
		return s, err
	}
	s.Broker = broker.NewHTTPClient(s.BrokerURL, s.BrokerKey, opts.BrokerTimeout)

	keysJSON, _ := json.Marshal([]map[string]any{
		{"principal": "alice", "role": "trader", "accounts": []string{"ACC-1", "ACC-2"}, "token": s.Trader},
		{"principal": "vera", "role": "viewer", "accounts": []string{"ACC-1"}, "token": s.Viewer},
		{"principal": "olivia", "role": "operator", "accounts": []string{"*"}, "token": s.Operator},
	})
	keys, err := auth.ParseKeys(keysJSON)
	if err != nil {
		return s, err
	}

	s.App, err = app.New(ctx, app.Config{
		DatabaseURL: dbURL, Store: opts.Store, Keys: keys, Risk: opts.Limits, Broker: s.Broker,
		WebhookSecret: s.WebhookSecret, Outbox: opts.Outbox, Reconcile: opts.Reconcile,
		Clock: opts.Clock, IDs: opts.IDs, Log: opts.Log,
	})
	if err != nil {
		return s, err
	}
	handler.Store(s.App.Handler())

	bg, stop := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		s.App.Run(bg, app.Roles{Outbox: opts.Background, Reconciler: opts.Reconcile.Interval > 0})
	}()
	// Closers run last to first: stop the workers, then the database handle.
	s.closers = append(s.closers, s.App.Close, func() { stop(); wg.Wait() })
	return s, nil
}

// Close stops everything and deletes the database.
func (s *Stack) Close() {
	for i := len(s.closers) - 1; i >= 0; i-- {
		s.closers[i]()
	}
	s.closers = nil
}

func (s *Stack) listen(addr string, h http.Handler) (string, error) {
	if addr == "" {
		addr = "127.0.0.1:0"
	}
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return "", fmt.Errorf("localstack: listen on %s: %w", addr, err)
	}
	srv := &http.Server{Handler: h, ReadHeaderTimeout: 5 * time.Second}
	go srv.Serve(ln)
	s.closers = append(s.closers, func() { srv.Close() })
	return "http://" + ln.Addr().String(), nil
}

func (s *Stack) createDatabase(ctx context.Context, adminURL string) (string, error) {
	name := "local_" + secret()[:16]
	admin, err := pgx.Connect(ctx, adminURL)
	if err != nil {
		return "", fmt.Errorf("localstack: connect: %w", err)
	}
	defer admin.Close(ctx)
	if _, err := admin.Exec(ctx, `CREATE DATABASE `+name); err != nil {
		return "", fmt.Errorf("localstack: create database: %w", err)
	}
	s.closers = append(s.closers, func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		c, err := pgx.Connect(ctx, adminURL)
		if err != nil {
			return
		}
		defer c.Close(ctx)
		c.Exec(ctx, `DROP DATABASE IF EXISTS `+name+` WITH (FORCE)`)
	})
	u, err := url.Parse(adminURL)
	if err != nil {
		return "", fmt.Errorf("localstack: database url: %w", err)
	}
	u.Path = "/" + name
	return u.String(), nil
}
