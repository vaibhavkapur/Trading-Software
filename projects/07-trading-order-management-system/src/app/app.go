// Package app assembles the OMS from its parts. The server binary, the demo,
// the benchmark and the tests all build the system through here, so they all
// run the same wiring.
package app

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"oms/src/api"
	"oms/src/auth"
	"oms/src/broker"
	"oms/src/clock"
	"oms/src/ids"
	"oms/src/metrics"
	"oms/src/outbox"
	"oms/src/reconcile"
	"oms/src/risk"
	"oms/src/service"
	"oms/src/store"
)

// Config is everything needed to build an OMS.
type Config struct {
	DatabaseURL string
	Store       store.Options

	// Keys authenticates API callers; Risk authorizes orders; Broker is where
	// orders go. All three are required: there are no permissive defaults.
	Keys   *auth.Keys
	Risk   risk.Authorizer
	Broker broker.Client
	// WebhookSecret verifies broker webhooks. Empty disables the receiver.
	WebhookSecret string

	Outbox    outbox.Config
	Reconcile reconcile.Config

	// Clock and IDs default to the system clock and random ids. Tests replace
	// them to make runs reproducible.
	Clock clock.Clock
	IDs   ids.Generator
	Log   *slog.Logger
}

// App is an assembled OMS.
type App struct {
	Store      *store.Store
	Service    *service.Service
	Worker     *outbox.Worker
	Reconciler *reconcile.Reconciler
	Metrics    *metrics.Set
	API        *api.Server
	Log        *slog.Logger
}

// New connects to the database, applies migrations and wires the components.
// Nothing runs in the background until Run is called.
func New(ctx context.Context, cfg Config) (*App, error) {
	switch {
	case cfg.DatabaseURL == "":
		return nil, errors.New("app: a database URL is required")
	case cfg.Keys == nil:
		return nil, errors.New("app: API keys are required")
	case cfg.Risk == nil:
		return nil, errors.New("app: a risk authorizer is required")
	case cfg.Broker == nil:
		return nil, errors.New("app: a broker client is required")
	}
	if cfg.Clock == nil {
		cfg.Clock = clock.System{}
	}
	if cfg.IDs == nil {
		cfg.IDs = ids.Random{}
	}
	if cfg.Log == nil {
		cfg.Log = slog.New(slog.DiscardHandler)
	}
	cfg.Store.Clock = cfg.Clock

	st, err := store.Open(ctx, cfg.DatabaseURL, cfg.Store)
	if err != nil {
		return nil, fmt.Errorf("app: open database: %w", err)
	}
	if err := st.Migrate(ctx); err != nil {
		st.Close()
		return nil, fmt.Errorf("app: migrate: %w", err)
	}

	m := metrics.New()
	RegisterGauges(m, st)

	worker := outbox.New(cfg.Outbox, st, cfg.Broker, cfg.IDs, m, cfg.Log)
	svc := &service.Service{
		Store: st, Risk: cfg.Risk, IDs: cfg.IDs, Metrics: m, Log: cfg.Log,
		Broker: cfg.Broker, Nudge: worker.Nudge,
	}
	rec := &reconcile.Reconciler{
		Config: cfg.Reconcile, Store: st, Broker: cfg.Broker, Service: svc,
		IDs: cfg.IDs, Metrics: m, Log: cfg.Log,
	}
	return &App{
		Store: st, Service: svc, Worker: worker, Reconciler: rec, Metrics: m, Log: cfg.Log,
		API: &api.Server{
			Service: svc, Reconciler: rec, Store: st, Keys: cfg.Keys,
			WebhookSecret: cfg.WebhookSecret, Metrics: m, Log: cfg.Log,
		},
	}, nil
}

// Handler returns the HTTP API.
func (a *App) Handler() http.Handler { return a.API.Handler() }

// Roles selects which background workers a process runs. The HTTP API is
// served by the caller; a process can run any combination.
type Roles struct {
	Outbox     bool
	Reconciler bool
}

// Run runs the selected background workers until ctx is cancelled, then
// waits for work in flight to finish.
func (a *App) Run(ctx context.Context, roles Roles) {
	var wg sync.WaitGroup
	if roles.Outbox {
		wg.Add(1)
		go func() {
			defer wg.Done()
			a.Worker.Run(ctx)
		}()
	}
	if roles.Reconciler {
		wg.Add(1)
		go func() {
			defer wg.Done()
			a.Reconciler.Schedule(ctx)
		}()
	}
	wg.Wait()
}

// Close releases the database connections.
func (a *App) Close() { a.Store.Close() }

// RegisterGauges adds the gauges that describe the database: outbox depth
// and age, and the size of the exception queue. They are computed when
// metrics are scraped, so they are correct no matter which process did the
// work.
func RegisterGauges(m *metrics.Set, st *store.Store) {
	query := func() (context.Context, context.CancelFunc) {
		return context.WithTimeout(context.Background(), 2*time.Second)
	}
	outboxStats := func() (store.OutboxStats, bool) {
		ctx, cancel := query()
		defer cancel()
		s, err := st.OutboxStats(ctx)
		return s, err == nil
	}
	one := func(v float64) []metrics.Sample { return []metrics.Sample{{Value: v}} }

	m.Registry.GaugeFunc("oms_outbox_pending", "Broker calls waiting to be delivered.", nil, func() []metrics.Sample {
		s, ok := outboxStats()
		if !ok {
			return nil
		}
		return one(float64(s.Pending))
	})
	m.Registry.GaugeFunc("oms_outbox_dead", "Broker calls given up on and waiting for an operator.", nil, func() []metrics.Sample {
		s, ok := outboxStats()
		if !ok {
			return nil
		}
		return one(float64(s.Dead))
	})
	m.Registry.GaugeFunc("oms_outbox_oldest_pending_age_seconds", "Age of the oldest undelivered broker call.", nil, func() []metrics.Sample {
		s, ok := outboxStats()
		if !ok {
			return nil
		}
		return one(metrics.Seconds(s.OldestPendingAge))
	})
	m.Registry.GaugeFunc("oms_reconciliation_open_issues", "Open issues in the exception queue.", []string{"severity"}, func() []metrics.Sample {
		ctx, cancel := query()
		defer cancel()
		counts, err := st.OpenIssueCounts(ctx)
		if err != nil {
			return nil
		}
		out := make([]metrics.Sample, 0, 4)
		for _, sev := range []string{store.SeverityCritical, store.SeverityHigh, store.SeverityMedium, store.SeverityLow} {
			out = append(out, metrics.Sample{LabelValues: []string{sev}, Value: float64(counts[sev])})
		}
		return out
	})
}
