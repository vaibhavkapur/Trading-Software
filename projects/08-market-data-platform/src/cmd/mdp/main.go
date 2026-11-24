// Command mdp runs the market data platform's services.
//
// Everything it ingests by default is simulated. See README.md.
package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"mdp/src/app"
	"mdp/src/gateway"
	"mdp/src/ingestors"
	"mdp/src/metrics"
	"mdp/src/refdata"
	"mdp/src/simulator"
	"mdp/src/storage"
	"mdp/src/stream"
)

const usage = `mdp — market data ingestion platform (simulated data by default)

Services (each is one process in a deployment):
  sim         simulated venues: alpha (WebSocket) and beta (TCP), with fault injection
  ingest      feed adapters → raw topic        (or --replay <capture>)
  normalize   raw topic → canonical topics
  sink        canonical topics → ClickHouse
  gateway     HTTP queries, WebSocket subscriptions, metrics

One process, no external dependencies:
  standalone  simulator + all services on an in-memory log and store
  demo        scripted walkthrough of the pipeline, including failures
  bench       in-process load test; prints measured throughput and latency

Tools:
  fixture     generate a deterministic capture file
  replay      run a capture through the pipeline and verify its digest
  archive     export a day of trades and quotes from ClickHouse to Parquet

Run "mdp <command> -h" for a command's flags.
`

func main() {
	if len(os.Args) < 2 {
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	cmds := map[string]func(context.Context, []string) error{
		"sim": cmdSim, "ingest": cmdIngest, "normalize": cmdNormalize, "sink": cmdSink, "gateway": cmdGateway,
		"standalone": cmdStandalone, "demo": cmdDemo, "bench": cmdBench,
		"fixture": cmdFixture, "replay": cmdReplay, "archive": cmdArchive,
	}
	cmd, ok := cmds[os.Args[1]]
	if !ok {
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := cmd(ctx, os.Args[2:]); err != nil && !errors.Is(err, flag.ErrHelp) {
		fmt.Fprintln(os.Stderr, "mdp:", err)
		os.Exit(1)
	}
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// common holds the flags every service shares.
type common struct {
	fs         *flag.FlagSet
	refdata    string
	brokers    string
	chURL      string
	namespace  string
	partitions int
	metrics    string
	logLevel   string
}

func newFlags(name string) *common {
	c := &common{fs: flag.NewFlagSet("mdp "+name, flag.ContinueOnError)}
	c.fs.StringVar(&c.refdata, "refdata", envOr("MDP_REFDATA", "examples/instruments.json"), "instrument and provider reference data")
	c.fs.StringVar(&c.brokers, "brokers", envOr("MDP_KAFKA_BROKERS", "127.0.0.1:19092"), "Kafka/Redpanda brokers, comma-separated")
	c.fs.StringVar(&c.chURL, "clickhouse-url", envOr("MDP_CLICKHOUSE_URL", "http://127.0.0.1:8123"),
		"ClickHouse HTTP URL (user, password, database: MDP_CLICKHOUSE_USER, MDP_CLICKHOUSE_PASSWORD, MDP_CLICKHOUSE_DB)")
	c.fs.StringVar(&c.namespace, "namespace", os.Getenv("MDP_KAFKA_NAMESPACE"), "prefix for every topic and consumer group, to share a cluster")
	c.fs.IntVar(&c.partitions, "partitions", 4, "partitions of the raw and canonical topics")
	c.fs.StringVar(&c.metrics, "metrics-addr", envOr("MDP_METRICS_ADDR", ""), "serve /metrics and /healthz here (empty: off)")
	c.fs.StringVar(&c.logLevel, "log-level", envOr("MDP_LOG_LEVEL", "info"), "debug, info, warn or error")
	return c
}

func (c *common) logger() *slog.Logger {
	var lvl slog.Level
	if err := lvl.UnmarshalText([]byte(c.logLevel)); err != nil {
		lvl = slog.LevelInfo
	}
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: lvl}))
}

func (c *common) options() app.Options {
	opt := app.DefaultOptions()
	opt.Layout.Partitions = int32(c.partitions)
	return opt
}

func (c *common) kafka(clientID string) (*stream.KafkaBus, error) {
	return stream.NewKafkaBus(strings.Split(c.brokers, ","), clientID, c.namespace)
}

func (c *common) clickhouse() (*storage.ClickHouse, error) {
	pw := os.Getenv("MDP_CLICKHOUSE_PASSWORD")
	if pw == "" {
		return nil, errors.New("MDP_CLICKHOUSE_PASSWORD is not set")
	}
	return storage.NewClickHouse(storage.ClickHouseConfig{
		URL: c.chURL, Database: envOr("MDP_CLICKHOUSE_DB", "mdp"), User: envOr("MDP_CLICKHOUSE_USER", "mdp"), Password: pw,
	}), nil
}

// stack builds the shared infrastructure for a deployed service and waits
// for the broker (and store) to accept the topic and schema setup.
func (c *common) stack(ctx context.Context, clientID string, withStore bool) (*app.Stack, error) {
	reg, err := refdata.Load(c.refdata)
	if err != nil {
		return nil, err
	}
	bus, err := c.kafka(clientID)
	if err != nil {
		return nil, err
	}
	st := &app.Stack{Reg: reg, Bus: bus, M: metrics.New(), Opt: c.options(), Log: c.logger()}
	if withStore {
		ch, err := c.clickhouse()
		if err != nil {
			return nil, err
		}
		st.Store = ch
	}
	deadline := time.Now().Add(90 * time.Second)
	for {
		ictx, cancel := context.WithTimeout(ctx, 10*time.Second)
		err := st.Init(ictx)
		cancel()
		if err == nil {
			break
		}
		if ctx.Err() != nil || time.Now().After(deadline) {
			return nil, fmt.Errorf("dependencies not ready: %w", err)
		}
		st.Log.Warn("waiting for dependencies", "err", err)
		select {
		case <-time.After(2 * time.Second):
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	c.serveMetrics(ctx, st.M, st.Log)
	return st, nil
}

// serveMetrics exposes /metrics for services that have no API of their own.
func (c *common) serveMetrics(ctx context.Context, m *metrics.Set, log *slog.Logger) {
	if c.metrics == "" {
		return
	}
	mux := http.NewServeMux()
	mux.Handle("GET /metrics", m.Handler())
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { w.Write([]byte("ok\n")) })
	srv := &http.Server{Addr: c.metrics, Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	go func() { <-ctx.Done(); srv.Close() }()
	go func() {
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Error("metrics server failed", "err", err)
		}
	}()
}

// serveHTTP serves h on addr until ctx ends and returns the bound address.
func serveHTTP(ctx context.Context, addr string, h http.Handler) (net.Addr, func() error, error) {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, nil, err
	}
	srv := &http.Server{Handler: h, ReadHeaderTimeout: 5 * time.Second}
	go func() { <-ctx.Done(); srv.Close() }()
	wait := func() error {
		if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			return err
		}
		return nil
	}
	return ln.Addr(), wait, nil
}

func cmdSim(ctx context.Context, args []string) error {
	c := newFlags("sim")
	alpha := c.fs.String("alpha-addr", "127.0.0.1:9101", "alpha venue listen address (WebSocket, path /feed)")
	beta := c.fs.String("beta-addr", "127.0.0.1:9102", "beta venue listen address (TCP)")
	admin := c.fs.String("admin-addr", "127.0.0.1:9103", "fault-injection HTTP address (empty: off)")
	rate := c.fs.Float64("rate", 200, "events per second per venue")
	seed := c.fs.Uint64("seed", 1, "random seed")
	if err := c.fs.Parse(args); err != nil {
		return err
	}
	reg, err := refdata.Load(c.refdata)
	if err != nil {
		return err
	}
	// A new session per start: a restarted venue does not continue old sequence numbers.
	session := time.Now().UTC().Format("20060102T150405")
	sim, err := simulator.Start(ctx, reg, simulator.Config{Seed: *seed, AlphaAddr: *alpha, BetaAddr: *beta, Session: session})
	if err != nil {
		return err
	}
	log := c.logger()
	log.Info("SIMULATED venues up", "alpha", sim.AlphaURL(), "beta", sim.BetaAddr(), "rate", *rate, "session", session)
	if *admin != "" {
		addr, wait, err := serveHTTP(ctx, *admin, sim.AdminHandler())
		if err != nil {
			return err
		}
		log.Info("fault injection", "url", "http://"+addr.String()+"/faults/{alpha|beta}/{fault}")
		go wait()
	}
	sim.Run(ctx, *rate)
	sim.Wait()
	return nil
}

func cmdIngest(ctx context.Context, args []string) error {
	c := newFlags("ingest")
	providers := c.fs.String("providers", "alpha,beta", "live feeds to ingest")
	alphaURL := c.fs.String("alpha-url", envOr("MDP_ALPHA_URL", "ws://127.0.0.1:9101/feed"), "alpha feed URL")
	betaAddr := c.fs.String("beta-addr", envOr("MDP_BETA_ADDR", "127.0.0.1:9102"), "beta feed address")
	replay := c.fs.String("replay", "", "ingest this capture file instead of live feeds, then exit")
	speed := c.fs.Float64("speed", 0, "replay pacing: 1 is real time, 0 is as fast as possible")
	capture := c.fs.String("capture", "", "also record every frame read to this capture file")
	if err := c.fs.Parse(args); err != nil {
		return err
	}
	st, err := c.stack(ctx, "mdp-ingest", false)
	if err != nil {
		return err
	}
	defer st.Bus.Close()
	if *capture != "" {
		f, err := os.OpenFile(*capture, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
		if err != nil {
			return err
		}
		defer f.Close()
		cw := ingestors.NewCaptureWriter(f)
		defer cw.Flush()
		st.Opt.Ingest.Capture = cw
	}
	var adapters []ingestors.Adapter
	if *replay != "" {
		lines, err := ingestors.ReadCapture(*replay)
		if err != nil {
			return err
		}
		seen := map[string]bool{}
		for _, l := range lines {
			if !seen[l.Provider] {
				seen[l.Provider] = true
				a, err := ingestors.NewReplay(*replay, l.Provider, *speed)
				if err != nil {
					return err
				}
				adapters = append(adapters, a)
			}
		}
	} else {
		for _, p := range strings.Split(*providers, ",") {
			switch strings.TrimSpace(p) {
			case ingestors.ProviderAlpha:
				adapters = append(adapters, &ingestors.Alpha{URL: *alphaURL, Symbols: st.Reg.Symbols(ingestors.ProviderAlpha)})
			case ingestors.ProviderBeta:
				adapters = append(adapters, &ingestors.Beta{Addr: *betaAddr})
			default:
				return fmt.Errorf("unknown provider %q", p)
			}
		}
	}
	fns := make([]func(context.Context) error, len(adapters))
	for i, a := range adapters {
		fns[i] = st.NewIngestor(a).Run
	}
	return app.Run(ctx, fns...)
}

func cmdNormalize(ctx context.Context, args []string) error {
	c := newFlags("normalize")
	if err := c.fs.Parse(args); err != nil {
		return err
	}
	st, err := c.stack(ctx, "mdp-normalize", false)
	if err != nil {
		return err
	}
	defer st.Bus.Close()
	return st.NewNormalizer().Run(ctx)
}

func cmdSink(ctx context.Context, args []string) error {
	c := newFlags("sink")
	if err := c.fs.Parse(args); err != nil {
		return err
	}
	st, err := c.stack(ctx, "mdp-sink", true)
	if err != nil {
		return err
	}
	defer st.Bus.Close()
	return st.NewSink().Run(ctx)
}

func entitlements(path string, log *slog.Logger) (*gateway.Entitlements, error) {
	if path == "" {
		log.Warn("no entitlements file: every caller can read every venue (local, simulated use only)")
		return gateway.OpenEntitlements(), nil
	}
	return gateway.LoadEntitlements(path)
}

func cmdGateway(ctx context.Context, args []string) error {
	c := newFlags("gateway")
	addr := c.fs.String("addr", envOr("MDP_GATEWAY_ADDR", "127.0.0.1:8080"), "listen address")
	entFile := c.fs.String("entitlements", os.Getenv("MDP_ENTITLEMENTS_FILE"), "entitlements file (empty: open access)")
	if err := c.fs.Parse(args); err != nil {
		return err
	}
	st, err := c.stack(ctx, "mdp-gateway", true)
	if err != nil {
		return err
	}
	defer st.Bus.Close()
	ent, err := entitlements(*entFile, st.Log)
	if err != nil {
		return err
	}
	hub, srv := st.NewGateway(ent)
	bound, wait, err := serveHTTP(ctx, *addr, srv.Handler())
	if err != nil {
		return err
	}
	st.Log.Info("gateway listening", "addr", bound.String())
	return app.Run(ctx, hub.Run, func(context.Context) error { return wait() })
}

// local is a complete pipeline in one process: simulator, in-memory log and
// store, every service, and the gateway on a real socket.
type local struct {
	st    *app.Stack
	bus   *stream.MemBus
	store *storage.MemStore
	sim   *simulator.Sim
	url   string // gateway base URL
	run   []func(context.Context) error
}

func newLocal(ctx context.Context, c *common, addr string, seed uint64, opt app.Options, log *slog.Logger) (*local, error) {
	reg, err := refdata.Load(c.refdata)
	if err != nil {
		return nil, err
	}
	l := &local{bus: stream.NewMemBus(), store: storage.NewMemStore()}
	l.st = &app.Stack{Reg: reg, Bus: l.bus, Store: l.store, M: metrics.New(), Opt: opt, Log: log}
	if err := l.st.Init(ctx); err != nil {
		return nil, err
	}
	if l.sim, err = simulator.Start(ctx, reg, simulator.Config{Seed: seed, AlphaAddr: "127.0.0.1:0", BetaAddr: "127.0.0.1:0"}); err != nil {
		return nil, err
	}
	hub, srv := l.st.NewGateway(gateway.OpenEntitlements())
	bound, wait, err := serveHTTP(ctx, addr, srv.Handler())
	if err != nil {
		return nil, err
	}
	l.url = "http://" + bound.String()
	l.run = []func(context.Context) error{
		l.st.NewIngestor(&ingestors.Alpha{URL: l.sim.AlphaURL(), Symbols: reg.Symbols(ingestors.ProviderAlpha)}).Run,
		l.st.NewIngestor(&ingestors.Beta{Addr: l.sim.BetaAddr()}).Run,
		l.st.NewNormalizer().Run,
		l.st.NewSink().Run,
		hub.Run,
		func(context.Context) error { return wait() },
	}
	return l, nil
}

func cmdStandalone(ctx context.Context, args []string) error {
	c := newFlags("standalone")
	addr := c.fs.String("addr", "127.0.0.1:8080", "gateway listen address")
	rate := c.fs.Float64("rate", 200, "simulated events per second per venue")
	seed := c.fs.Uint64("seed", 1, "random seed")
	if err := c.fs.Parse(args); err != nil {
		return err
	}
	log := c.logger()
	l, err := newLocal(ctx, c, *addr, *seed, c.options(), log)
	if err != nil {
		return err
	}
	log.Info("standalone pipeline up: SIMULATED data, in-memory log and store, nothing persists", "gateway", l.url)
	fns := append(l.run, func(ctx context.Context) error { l.sim.Run(ctx, *rate); return nil })
	return app.Run(ctx, fns...)
}

// manifest pins a capture file and the output it must produce.
type manifest struct {
	Description   string              `json:"description"`
	Seed          uint64              `json:"seed"`
	Steps         int                 `json:"steps"`
	Start         time.Time           `json:"start"`
	Faults        []simulator.FaultAt `json:"faults"`
	Frames        int                 `json:"frames"`
	CaptureSHA256 string              `json:"capture_sha256"`
	Expected      *app.ReplayResult   `json:"expected,omitempty"`
}

func fileSHA256(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func manifestPath(capture string) string {
	return strings.TrimSuffix(capture, ".ndjson") + ".manifest.json"
}

func writeJSON(path string, v any) error {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(b, '\n'), 0o644)
}

func cmdFixture(ctx context.Context, args []string) error {
	c := newFlags("fixture")
	out := c.fs.String("out", "examples/fixtures/session.ndjson", "capture file to write")
	seed := c.fs.Uint64("seed", 42, "random seed")
	steps := c.fs.Int("steps", 4000, "steps; each publishes one event per venue")
	start := c.fs.String("start", "2026-01-05T09:30:00Z", "virtual clock start (RFC 3339)")
	noFaults := c.fs.Bool("no-faults", false, "generate a clean capture without the default fault plan")
	if err := c.fs.Parse(args); err != nil {
		return err
	}
	reg, err := refdata.Load(c.refdata)
	if err != nil {
		return err
	}
	t0, err := time.Parse(time.RFC3339, *start)
	if err != nil {
		return fmt.Errorf("start: %w", err)
	}
	faults := simulator.DefaultFaults()
	if *noFaults {
		faults = nil
	}
	f, err := os.Create(*out)
	if err != nil {
		return err
	}
	frames, err := simulator.WriteFixture(reg, *seed, *steps, t0, faults, f)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return err
	}
	sum, err := fileSHA256(*out)
	if err != nil {
		return err
	}
	res, _, err := app.Replay(ctx, reg, *out, nil)
	if err != nil {
		return err
	}
	m := manifest{
		Description: "Simulated capture generated by `mdp fixture`. Not real market data.",
		Seed:        *seed, Steps: *steps, Start: t0.UTC(), Faults: faults, Frames: frames, CaptureSHA256: sum, Expected: &res,
	}
	if err := writeJSON(manifestPath(*out), m); err != nil {
		return err
	}
	fmt.Printf("wrote %s (%d frames, sha256 %s)\nwrote %s (output digest %s)\n", *out, frames, sum, manifestPath(*out), res.Digest)
	return nil
}

func cmdReplay(ctx context.Context, args []string) error {
	c := newFlags("replay")
	file := c.fs.String("file", "examples/fixtures/session.ndjson", "capture file")
	verify := c.fs.Bool("verify", true, "compare against the capture's manifest and fail on any difference")
	if err := c.fs.Parse(args); err != nil {
		return err
	}
	reg, err := refdata.Load(c.refdata)
	if err != nil {
		return err
	}
	started := time.Now()
	res, store, err := app.Replay(ctx, reg, *file, nil)
	if err != nil {
		return err
	}
	out, _ := json.MarshalIndent(res, "", "  ")
	fmt.Printf("%s\nstored rows: trades=%d quotes=%d depth=%d gaps=%d\nreplayed in %s\n", out,
		store.Rows("trades"), store.Rows("quotes"), store.Rows("depth"), store.Rows("gaps"), time.Since(started).Round(time.Millisecond))
	if !*verify {
		return nil
	}
	b, err := os.ReadFile(manifestPath(*file))
	if err != nil {
		return fmt.Errorf("manifest: %w", err)
	}
	var m manifest
	if err := json.Unmarshal(b, &m); err != nil {
		return fmt.Errorf("manifest: %w", err)
	}
	sum, err := fileSHA256(*file)
	if err != nil {
		return err
	}
	if sum != m.CaptureSHA256 {
		return fmt.Errorf("capture sha256 %s does not match manifest %s", sum, m.CaptureSHA256)
	}
	if m.Expected == nil || res.Digest != m.Expected.Digest {
		return fmt.Errorf("output digest %s does not match manifest", res.Digest)
	}
	fmt.Println("OK: capture and output match the manifest")
	return nil
}

func cmdArchive(ctx context.Context, args []string) error {
	c := newFlags("archive")
	date := c.fs.String("date", "", "UTC day to export, YYYY-MM-DD (required)")
	out := c.fs.String("out", "data/archive", "output directory")
	instrument := c.fs.String("instrument", "", "instrument_id (default: all)")
	if err := c.fs.Parse(args); err != nil {
		return err
	}
	day, err := time.Parse("2006-01-02", *date)
	if err != nil {
		return fmt.Errorf("--date must be YYYY-MM-DD")
	}
	reg, err := refdata.Load(c.refdata)
	if err != nil {
		return err
	}
	store, err := c.clickhouse()
	if err != nil {
		return err
	}
	ids := []string{*instrument}
	if *instrument == "" {
		ids = nil
		for _, in := range reg.All() {
			ids = append(ids, in.ID)
		}
	} else if _, ok := reg.Get(*instrument); !ok {
		return fmt.Errorf("unknown instrument %q", *instrument)
	}
	total := 0
	for _, id := range ids {
		files, err := storage.Archive(ctx, store, id, day, *out)
		if err != nil {
			return err
		}
		for _, f := range files {
			fmt.Printf("%8d rows  %s\n", f.Rows, f.Path)
			total += f.Rows
		}
	}
	fmt.Printf("archived %d rows for %s\n", total, *date)
	return nil
}
