// Package app wires the services together. riskd, the demo and the tests
// all start the engine through Open, so they exercise the same recovery path.
package app

import (
	"context"
	"crypto/ed25519"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"riskengine/src/api"
	"riskengine/src/audit"
	"riskengine/src/core"
	"riskengine/src/domain"
	"riskengine/src/market"
	"riskengine/src/metrics"
	"riskengine/src/notify"
	"riskengine/src/policy"
	"riskengine/src/state"
)

type Config struct {
	PolicyPath string
	DataDir    string             // holds audit.log and, unless Key is set, audit.key
	Key        ed25519.PrivateKey // optional; tests pass a fixed key for reproducible signatures
	Clock      core.Clock         // defaults to the system clock
	AuditSync  bool               // fsync every audit record
	AdminToken string
	FeedToken  string
	Webhooks   []string // kill-switch subscribers
	// MaxQuoteFuture bounds how far ahead of local time a quote may be
	// stamped at ingest. Defaults to 5s.
	MaxQuoteFuture time.Duration
}

type App struct {
	Engine   *core.Engine
	Market   *market.Store
	Metrics  *metrics.Registry
	Notifier *notify.Notifier
	Log      *audit.Log
	Handler  http.Handler
	// Recovered is the number of audit records replayed at startup.
	Recovered uint64

	cfg    Config
	cancel context.CancelFunc
}

func (c Config) AuditPath() string { return filepath.Join(c.DataDir, "audit.log") }

// Open verifies and replays the audit log, loads the policy and returns a
// ready engine. It fails rather than start on a corrupt log or bad policy.
func Open(cfg Config) (*App, error) {
	if cfg.Clock == nil {
		cfg.Clock = core.SystemClock{}
	}
	if cfg.MaxQuoteFuture == 0 {
		cfg.MaxQuoteFuture = 5 * time.Second
	}
	if err := os.MkdirAll(cfg.DataDir, 0o700); err != nil {
		return nil, err
	}
	key := cfg.Key
	if key == nil {
		var err error
		if key, err = audit.LoadOrCreateKey(filepath.Join(cfg.DataDir, "audit.key")); err != nil {
			return nil, err
		}
	}
	pub := key.Public().(ed25519.PublicKey)
	pol, err := policy.Load(cfg.PolicyPath)
	if err != nil {
		return nil, err
	}

	st := state.New()
	var recovered uint64
	log, err := audit.Open(cfg.AuditPath(), key, cfg.AuditSync, func(rec audit.Record) error {
		var ev domain.Event
		if err := json.Unmarshal(rec.Event, &ev); err != nil {
			return fmt.Errorf("audit seq %d: %w", rec.Seq, err)
		}
		recovered++
		return st.Apply(ev, rec.Seal())
	})
	if err != nil {
		return nil, err
	}

	ctx, cancel := context.WithCancel(context.Background())
	a := &App{Market: market.NewStore(cfg.MaxQuoteFuture), Metrics: metrics.New(), Log: log, Recovered: recovered, cfg: cfg, cancel: cancel}
	a.Engine = core.New(st, a.Market, log, cfg.Clock, a.Metrics)
	a.Notifier = notify.New(ctx, cfg.Webhooks, a.Metrics)
	a.Engine.OnKillSwitch(a.Notifier.Publish)
	if err := a.Engine.SetPolicy(pol, "startup"); err != nil {
		a.Close()
		return nil, err
	}
	a.Handler = (&api.Server{
		Engine: a.Engine, Market: a.Market, Metrics: a.Metrics, Clock: cfg.Clock, Notifier: a.Notifier,
		AdminToken: cfg.AdminToken, FeedToken: cfg.FeedToken,
		PublicKey:   hex.EncodeToString(pub),
		ReloadFn:    a.ReloadPolicy,
		AuditHead:   log.Head,
		AuditVerify: func() (uint64, string, error) { return audit.Verify(cfg.AuditPath(), pub) },
	}).Handler()
	return a, nil
}

// ReloadPolicy re-reads the policy file. An invalid file changes nothing.
func (a *App) ReloadPolicy() (*policy.Policy, error) {
	p, err := policy.Load(a.cfg.PolicyPath)
	if err != nil {
		return nil, err
	}
	return p, a.Engine.SetPolicy(p, "reload")
}

// Run sweeps expired reservations and syncs the audit log until ctx ends.
func (a *App) Run(ctx context.Context) {
	sweep := time.NewTicker(250 * time.Millisecond)
	sync := time.NewTicker(100 * time.Millisecond)
	defer sweep.Stop()
	defer sync.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-sweep.C:
			a.Engine.Sweep()
		case <-sync.C:
			a.Log.Sync()
		}
	}
}

func (a *App) Close() error {
	a.cancel()
	return a.Log.Close()
}
