// Command omsd is the order management service.
//
//	omsd            serve the API and run the background workers
//	omsd migrate    apply database migrations and exit
//	omsd verify     replay the event log against the projections and exit
//	                (status 1 if they disagree)
//
// PAPER TRADING ONLY. The only broker adapter speaks the protocol of the
// simulated broker in this repository (cmd/brokersim), and omsd refuses to
// start unless the broker at OMS_BROKER_URL identifies itself as simulated.
//
// Configuration comes from the environment; secrets are never flags, so they
// do not show up in process listings or shell history.
//
//	OMS_DATABASE_URL            PostgreSQL connection string (required)
//	OMS_HTTP_ADDR               listen address (default 127.0.0.1:8080)
//	OMS_ROLES                   comma list of api, outbox, reconciler
//	                            (default all three)
//	OMS_API_KEYS                API keys as inline JSON, or
//	OMS_API_KEYS_FILE           path to the same JSON (see examples/)
//	OMS_RISK_LIMITS_FILE        pre-trade limits (see examples/risk-limits.json)
//	OMS_BROKER_URL              simulated broker base URL
//	OMS_BROKER_API_KEY          bearer token for the broker
//	OMS_BROKER_WEBHOOK_SECRET   shared secret that signs broker webhooks
//	OMS_BROKER_TIMEOUT          per-call timeout (default 5s)
//	OMS_BROKER_WAIT             how long to wait for the broker at startup
//	                            (default 30s)
//	OMS_OUTBOX_MAX_ATTEMPTS     tries before a broker call is dead-lettered
//	                            (default 8)
//	OMS_RECONCILE_INTERVAL      scheduled reconciliation period (default 30s;
//	                            0 disables)
//	OMS_RECONCILE_GRACE         how recent a change must be to be left alone
//	                            (default 5s)
//	OMS_DB_MAX_CONNS            connection pool size (default 16)
//	OMS_LOG_LEVEL               debug, info, warn or error (default info)
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"oms/src/app"
	"oms/src/auth"
	"oms/src/broker"
	"oms/src/outbox"
	"oms/src/reconcile"
	"oms/src/risk"
	"oms/src/store"
)

func main() {
	cmd := "serve"
	if len(os.Args) > 1 {
		cmd = os.Args[1]
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	var err error
	switch cmd {
	case "serve":
		err = serve(ctx)
	case "migrate":
		err = migrate(ctx)
	case "verify":
		err = verify(ctx)
	case "-h", "--help", "help":
		fmt.Fprintln(os.Stderr, "usage: omsd [serve|migrate|verify]   (configuration is read from OMS_* environment variables)")
		return
	default:
		err = fmt.Errorf("unknown command %q (want serve, migrate or verify)", cmd)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "omsd:", err)
		os.Exit(1)
	}
}

// --- configuration ---

type config struct {
	databaseURL   string
	httpAddr      string
	roles         map[string]bool
	limitsFile    string
	brokerURL     string
	brokerKey     string
	webhookSecret string
	brokerTimeout time.Duration
	brokerWait    time.Duration
	maxAttempts   int
	reconInterval time.Duration
	reconGrace    time.Duration
	maxConns      int
}

// env collects configuration errors so that one run reports all of them.
type env struct{ problems []string }

func (e *env) fail(format string, args ...any) {
	e.problems = append(e.problems, fmt.Sprintf(format, args...))
}

func (e *env) required(name string) string {
	v := os.Getenv(name)
	if v == "" {
		e.fail("%s is required", name)
	}
	return v
}

func (e *env) duration(name string, def time.Duration) time.Duration {
	v := os.Getenv(name)
	if v == "" {
		return def
	}
	if v == "0" {
		return 0
	}
	d, err := time.ParseDuration(v)
	if err != nil || d < 0 {
		// The value is not echoed: a secret pasted into the wrong variable
		// should not end up in a log.
		e.fail("%s is not a valid duration (for example 5s or 2m)", name)
		return def
	}
	return d
}

func (e *env) integer(name string, def, min int) int {
	v := os.Getenv(name)
	if v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < min {
		e.fail("%s must be an integer of at least %d", name, min)
		return def
	}
	return n
}

func (e *env) err() error {
	if len(e.problems) == 0 {
		return nil
	}
	return errors.New("configuration:\n  " + strings.Join(e.problems, "\n  "))
}

func loadConfig() (config, error) {
	var e env
	c := config{
		databaseURL:   e.required("OMS_DATABASE_URL"),
		httpAddr:      os.Getenv("OMS_HTTP_ADDR"),
		limitsFile:    e.required("OMS_RISK_LIMITS_FILE"),
		brokerURL:     e.required("OMS_BROKER_URL"),
		brokerKey:     e.required("OMS_BROKER_API_KEY"),
		webhookSecret: e.required("OMS_BROKER_WEBHOOK_SECRET"),
		brokerTimeout: e.duration("OMS_BROKER_TIMEOUT", 5*time.Second),
		brokerWait:    e.duration("OMS_BROKER_WAIT", 30*time.Second),
		maxAttempts:   e.integer("OMS_OUTBOX_MAX_ATTEMPTS", 8, 1),
		reconInterval: e.duration("OMS_RECONCILE_INTERVAL", 30*time.Second),
		reconGrace:    e.duration("OMS_RECONCILE_GRACE", 5*time.Second),
		maxConns:      e.integer("OMS_DB_MAX_CONNS", 16, 2),
		roles:         map[string]bool{},
	}
	if c.httpAddr == "" {
		c.httpAddr = "127.0.0.1:8080"
	}
	if c.brokerTimeout <= 0 {
		e.fail("OMS_BROKER_TIMEOUT must be positive")
	}
	if len(c.webhookSecret) > 0 && len(c.webhookSecret) < 24 {
		e.fail("OMS_BROKER_WEBHOOK_SECRET must be at least 24 characters")
	}
	roles := os.Getenv("OMS_ROLES")
	if roles == "" {
		roles = "api,outbox,reconciler"
	}
	for _, r := range strings.Split(roles, ",") {
		switch r = strings.TrimSpace(r); r {
		case "api", "outbox", "reconciler":
			c.roles[r] = true
		case "":
		default:
			e.fail("OMS_ROLES: unknown role %q (want api, outbox, reconciler)", r)
		}
	}
	if len(c.roles) == 0 {
		e.fail("OMS_ROLES selects no role")
	}
	return c, e.err()
}

func newLogger() *slog.Logger {
	level := slog.LevelInfo
	switch strings.ToLower(os.Getenv("OMS_LOG_LEVEL")) {
	case "debug":
		level = slog.LevelDebug
	case "warn":
		level = slog.LevelWarn
	case "error":
		level = slog.LevelError
	}
	return slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{
		Level: level,
		// Log timestamps are UTC, like every timestamp the service stores.
		ReplaceAttr: func(groups []string, a slog.Attr) slog.Attr {
			if a.Key == slog.TimeKey && len(groups) == 0 {
				a.Value = slog.TimeValue(a.Value.Time().UTC())
			}
			return a
		},
	}))
}

// --- commands ---

func serve(ctx context.Context) error {
	cfg, err := loadConfig()
	if err != nil {
		return err
	}
	log := newLogger()

	keys, err := auth.LoadKeys()
	if err != nil {
		return err
	}
	limits, err := risk.LoadStaticLimits(cfg.limitsFile)
	if err != nil {
		return err
	}
	if err := requireSimulatedBroker(ctx, cfg.brokerURL, cfg.brokerWait, log); err != nil {
		return err
	}

	// The lease must outlast a broker call, or a slow call is sent twice.
	lease := 30 * time.Second
	if min := 3 * cfg.brokerTimeout; lease < min {
		lease = min
	}
	a, err := app.New(ctx, app.Config{
		DatabaseURL:   cfg.databaseURL,
		Store:         store.Options{MaxConns: int32(cfg.maxConns)},
		Keys:          keys,
		Risk:          limits,
		Broker:        broker.NewHTTPClient(cfg.brokerURL, cfg.brokerKey, cfg.brokerTimeout),
		WebhookSecret: cfg.webhookSecret,
		Outbox:        outbox.Config{MaxAttempts: cfg.maxAttempts, Lease: lease},
		Reconcile:     reconcile.Config{Interval: cfg.reconInterval, Grace: cfg.reconGrace},
		Log:           log,
	})
	if err != nil {
		return err
	}
	defer a.Close()

	handler := a.Handler()
	if !cfg.roles["api"] {
		handler = operationalOnly(handler)
	}
	srv := &http.Server{
		Handler:           handler,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      60 * time.Second, // a reconciliation run answers when it finishes
		IdleTimeout:       90 * time.Second,
		MaxHeaderBytes:    64 << 10,
		ErrorLog:          slog.NewLogLogger(log.Handler(), slog.LevelWarn),
	}
	ln, err := net.Listen("tcp", cfg.httpAddr)
	if err != nil {
		return fmt.Errorf("listen on %s: %w", cfg.httpAddr, err)
	}

	// Addresses and roles are logged; the database URL, API keys, broker key
	// and webhook secret never are.
	log.Info("omsd started", "mode", "PAPER", "addr", ln.Addr().String(), "roles", roleList(cfg.roles),
		"risk_limits", limits.Version, "reconcile_interval", cfg.reconInterval.String())

	workers := make(chan struct{})
	go func() {
		defer close(workers)
		a.Run(ctx, app.Roles{Outbox: cfg.roles["outbox"], Reconciler: cfg.roles["reconciler"] && cfg.reconInterval > 0})
	}()
	served := make(chan error, 1)
	go func() { served <- srv.Serve(ln) }()

	select {
	case err := <-served:
		return fmt.Errorf("http server: %w", err)
	case <-ctx.Done():
	}

	// Stop taking requests, let the ones in flight finish, then wait for the
	// workers. Anything not yet delivered stays in the outbox for the next
	// start; nothing is lost by stopping here.
	log.Info("omsd stopping")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		log.Warn("http shutdown", "err", err)
	}
	select {
	case <-workers:
	case <-shutdownCtx.Done():
		log.Warn("workers did not stop in time; their leases will expire and the work will be retried")
	}
	log.Info("omsd stopped")
	return nil
}

func roleList(roles map[string]bool) string {
	var out []string
	for _, r := range []string{"api", "outbox", "reconciler"} {
		if roles[r] {
			out = append(out, r)
		}
	}
	return strings.Join(out, ",")
}

// operationalOnly serves health and metrics for a process that runs workers
// but not the API.
func operationalOnly(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/healthz", "/readyz", "/metrics":
			next.ServeHTTP(w, r)
		default:
			http.NotFound(w, r)
		}
	})
}

// requireSimulatedBroker refuses to continue unless the broker says it is a
// simulator. It is the guard that keeps this paper-trading system from being
// pointed at something that executes real orders by a configuration mistake.
func requireSimulatedBroker(ctx context.Context, baseURL string, wait time.Duration, log *slog.Logger) error {
	client := &http.Client{Timeout: 3 * time.Second}
	deadline := time.Now().Add(wait)
	for attempt := 1; ; attempt++ {
		mode, err := brokerMode(ctx, client, baseURL)
		switch {
		case err == nil && mode == "SIMULATED":
			return nil
		case err == nil:
			return fmt.Errorf("the broker at OMS_BROKER_URL does not identify itself as simulated (mode %q); this system is for paper trading only", mode)
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if !time.Now().Before(deadline) {
			return fmt.Errorf("could not confirm that the broker at OMS_BROKER_URL is simulated: %w", err)
		}
		if attempt == 1 {
			log.Info("waiting for the simulated broker", "err", err.Error())
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(500 * time.Millisecond):
		}
	}
}

func brokerMode(ctx context.Context, client *http.Client, baseURL string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(baseURL, "/")+"/healthz", nil)
	if err != nil {
		return "", err
	}
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
	if err != nil {
		return "", err
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("broker health check: HTTP %d", resp.StatusCode)
	}
	var health struct {
		Mode string `json:"mode"`
	}
	if err := json.Unmarshal(body, &health); err != nil {
		// Something answered, and it is not the simulator.
		return "UNKNOWN", nil
	}
	return health.Mode, nil
}

func openStore(ctx context.Context) (*store.Store, error) {
	url := os.Getenv("OMS_DATABASE_URL")
	if url == "" {
		return nil, errors.New("OMS_DATABASE_URL is required")
	}
	return store.Open(ctx, url, store.Options{MaxConns: 4})
}

func migrate(ctx context.Context) error {
	st, err := openStore(ctx)
	if err != nil {
		return err
	}
	defer st.Close()
	if err := st.Migrate(ctx); err != nil {
		return err
	}
	fmt.Println("migrations applied")
	return nil
}

func verify(ctx context.Context) error {
	st, err := openStore(ctx)
	if err != nil {
		return err
	}
	defer st.Close()
	rep, err := st.Verify(ctx)
	if err != nil {
		return err
	}
	out, _ := json.MarshalIndent(rep, "", "  ")
	fmt.Println(string(out))
	if !rep.OK() {
		return fmt.Errorf("%d projection(s) disagree with the event log", len(rep.Problems))
	}
	return nil
}
