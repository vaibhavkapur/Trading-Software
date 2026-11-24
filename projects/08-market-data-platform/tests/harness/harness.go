// Package harness builds complete, hermetic pipelines for tests: simulated
// venues on loopback sockets, an in-memory log and store, and the real
// services from package app. Individual services can be stopped and
// restarted while the log and store live on, like a broker and a database.
package harness

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"mdp/src/app"
	"mdp/src/domain"
	"mdp/src/gateway"
	"mdp/src/ingestors"
	"mdp/src/metrics"
	"mdp/src/normalizer"
	"mdp/src/refdata"
	"mdp/src/simulator"
	"mdp/src/storage"
	"mdp/src/stream"
)

// Root is the repository root.
func Root() string {
	_, file, _, _ := runtime.Caller(0)
	return filepath.Join(filepath.Dir(file), "..", "..")
}

// Registry loads the example reference data.
func Registry(t testing.TB) *refdata.Registry {
	t.Helper()
	reg, err := refdata.Load(filepath.Join(Root(), "examples", "instruments.json"))
	if err != nil {
		t.Fatal(err)
	}
	return reg
}

// Logger returns a logger that is silent unless MDP_TEST_LOG is set.
func Logger() *slog.Logger {
	if os.Getenv("MDP_TEST_LOG") != "" {
		return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelDebug}))
	}
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// FastOptions are service settings tuned so tests finish quickly.
func FastOptions() app.Options {
	opt := app.DefaultOptions()
	opt.Ingest = ingestors.Config{
		Linger: time.Millisecond, BackoffMin: 5 * time.Millisecond, BackoffMax: 50 * time.Millisecond,
		CheckpointEvery: 10 * time.Millisecond, HealthEvery: 20 * time.Millisecond, IdleTimeout: 2 * time.Second,
	}
	opt.Normalize = normalizer.ServiceConfig{
		CheckpointEvery: 10 * time.Millisecond, BackoffMin: 5 * time.Millisecond, BackoffMax: 50 * time.Millisecond,
		LagEvery: 20 * time.Millisecond,
	}
	opt.Sink = storage.SinkConfig{
		FlushEvery: 5 * time.Millisecond, BackoffMin: 5 * time.Millisecond, BackoffMax: 50 * time.Millisecond,
		LagEvery: 20 * time.Millisecond,
	}
	opt.Hub = gateway.HubConfig{StaleAfter: 300 * time.Millisecond, BackoffMin: 5 * time.Millisecond,
		BackoffMax: 50 * time.Millisecond, LagEvery: 20 * time.Millisecond}
	opt.Server = gateway.ServerConfig{HeartbeatEvery: 50 * time.Millisecond}
	return opt
}

// Env is one test's pipeline.
type Env struct {
	T     testing.TB
	Ctx   context.Context
	Reg   *refdata.Registry
	Bus   stream.Bus
	Mem   *stream.MemBus // nil when the pipeline runs on a real broker
	Store storage.Store
	MemDB *storage.MemStore // nil when the pipeline runs on a real store
	Stack *app.Stack
	Sim   *simulator.Sim
}

// New builds a hermetic pipeline: in-memory log and store, simulator on
// loopback. No service is started.
func New(t testing.TB) *Env {
	mem, db := stream.NewMemBus(), storage.NewMemStore()
	e := NewWith(t, mem, db, FastOptions())
	e.Mem, e.MemDB = mem, db
	return e
}

// NewWith builds a pipeline on the given log and store.
func NewWith(t testing.TB, bus stream.Bus, store storage.Store, opt app.Options) *Env {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	e := &Env{T: t, Ctx: ctx, Reg: Registry(t), Bus: bus, Store: store}
	e.Stack = &app.Stack{Reg: e.Reg, Bus: bus, Store: store, M: metrics.New(), Opt: opt, Log: Logger()}
	if err := e.Stack.Init(ctx); err != nil {
		t.Fatalf("init: %v", err)
	}
	sim, err := simulator.Start(ctx, e.Reg, simulator.Config{Seed: 7, AlphaAddr: "127.0.0.1:0", BetaAddr: "127.0.0.1:0",
		Session: time.Now().UTC().Format("150405.000000")})
	if err != nil {
		t.Fatalf("simulator: %v", err)
	}
	e.Sim = sim
	return e
}

// Proc is a running service.
type Proc struct {
	cancel context.CancelFunc
	done   chan struct{}
	err    error
}

// Stop cancels the service and waits for it to exit.
func (p *Proc) Stop() {
	p.cancel()
	<-p.done
}

// Go runs a service until Stop or the end of the test.
func (e *Env) Go(run func(context.Context) error) *Proc {
	ctx, cancel := context.WithCancel(e.Ctx)
	p := &Proc{cancel: cancel, done: make(chan struct{})}
	go func() {
		defer close(p.done)
		p.err = run(ctx)
	}()
	e.T.Cleanup(p.Stop)
	return p
}

// Ingestors is a running pair of live ingestors.
type Ingestors struct {
	Alpha, Beta *ingestors.Runner
	procs       []*Proc
}

func (i *Ingestors) Stop() {
	for _, p := range i.procs {
		p.Stop()
	}
}

// StartIngestors starts live ingestors for both simulated venues and waits
// until they are connected.
func (e *Env) StartIngestors() *Ingestors {
	e.T.Helper()
	a := e.Stack.NewIngestor(&ingestors.Alpha{URL: e.Sim.AlphaURL(), Symbols: e.Reg.Symbols(ingestors.ProviderAlpha)})
	b := e.Stack.NewIngestor(&ingestors.Beta{Addr: e.Sim.BetaAddr()})
	i := &Ingestors{Alpha: a, Beta: b, procs: []*Proc{e.Go(a.Run), e.Go(b.Run)}}
	e.WaitConnected()
	return i
}

// WaitConnected waits until each venue has a subscriber.
func (e *Env) WaitConnected() {
	e.T.Helper()
	e.Eventually("ingestors connected to both venues", func() bool {
		return e.Sim.Alpha.Subscribers() == 1 && e.Sim.Beta.Subscribers() == 1
	})
}

// Normalizer is a running normalizer.
type Normalizer struct {
	*normalizer.Service
	*Proc
}

func (e *Env) StartNormalizer() *Normalizer {
	s := e.Stack.NewNormalizer()
	return &Normalizer{Service: s, Proc: e.Go(s.Run)}
}

// Sink is a running sink.
type Sink struct {
	*storage.Sink
	*Proc
}

func (e *Env) StartSink() *Sink {
	s := e.Stack.NewSink()
	return &Sink{Sink: s, Proc: e.Go(s.Run)}
}

// Gateway is a running gateway on a loopback socket.
type Gateway struct {
	Hub  *gateway.Hub
	URL  string // http://host:port
	WS   string // ws://host:port
	Proc *Proc
}

// StartGateway serves the API and waits until live delivery is positioned.
func (e *Env) StartGateway(ent *gateway.Entitlements) *Gateway {
	e.T.Helper()
	hub, srv := e.Stack.NewGateway(ent)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		e.T.Fatal(err)
	}
	hs := &http.Server{Handler: srv.Handler()}
	go hs.Serve(ln)
	e.T.Cleanup(func() { hs.Close() })
	g := &Gateway{Hub: hub, URL: "http://" + ln.Addr().String(), WS: "ws://" + ln.Addr().String(), Proc: e.Go(hub.Run)}
	select {
	case <-hub.Ready():
	case <-time.After(10 * time.Second):
		e.T.Fatal("gateway hub not ready")
	}
	return g
}

// Records returns everything currently in a topic, on any bus.
func (e *Env) Records(topic string) []stream.Record {
	e.T.Helper()
	if e.Mem != nil {
		return e.Mem.All(topic)
	}
	ctx, cancel := context.WithTimeout(e.Ctx, 30*time.Second)
	defer cancel()
	ends, err := e.Bus.EndOffsets(ctx, topic)
	if err != nil {
		e.T.Fatalf("end offsets of %s: %v", topic, err)
	}
	c, err := e.Bus.NewConsumer(ctx, stream.ConsumerConfig{Topics: []string{topic}, Start: stream.FromEarliest})
	if err != nil {
		e.T.Fatalf("read %s: %v", topic, err)
	}
	defer c.Close()
	var out []stream.Record
	for !stream.CaughtUp(ends, c.Positions()) {
		recs, err := c.Poll(ctx)
		if err != nil {
			e.T.Fatalf("read %s: %v", topic, err)
		}
		out = append(out, recs...)
	}
	return out
}

// pollEvery is how often conditions are re-evaluated: reading a topic from
// a real broker costs a round trip, reading memory does not.
func (e *Env) pollEvery() time.Duration {
	if e.Mem != nil {
		return 2 * time.Millisecond
	}
	return 100 * time.Millisecond
}

// Eventually polls cond until it holds or 15 seconds pass (60 on real infrastructure).
func (e *Env) Eventually(what string, cond func() bool) {
	e.T.Helper()
	deadline := time.Now().Add(e.patience())
	for !cond() {
		if time.Now().After(deadline) {
			e.T.Fatalf("timed out waiting for: %s", what)
		}
		time.Sleep(e.pollEvery())
	}
}

func (e *Env) patience() time.Duration {
	if e.Mem != nil {
		return 15 * time.Second
	}
	return 60 * time.Second
}

// RawSeen returns how many distinct provider events are in the raw topic.
func (e *Env) RawSeen() int {
	seen := map[string]bool{}
	for _, r := range e.Records(stream.TopicRaw) {
		var ev ingestors.Event
		if json.Unmarshal(r.Value, &ev) == nil {
			seen[ev.TrackKey()+"/"+ev.Session+"/"+itoa(ev.Seq)] = true
		}
	}
	return len(seen)
}

func itoa(n uint64) string {
	b, _ := json.Marshal(n)
	return string(b)
}

// WaitRaw waits until n distinct provider events have reached the raw topic.
func (e *Env) WaitRaw(n int) {
	e.T.Helper()
	deadline := time.Now().Add(e.patience())
	for {
		have := e.RawSeen()
		if have >= n {
			return
		}
		if time.Now().After(deadline) {
			e.T.Fatalf("timed out waiting for %d raw events; have %d", n, have)
		}
		time.Sleep(e.pollEvery())
	}
}

// Settle waits until the normalizer has processed the whole raw topic and
// the sink has persisted every canonical topic.
func (e *Env) Settle(n *Normalizer, s *Sink) {
	e.T.Helper()
	ctx, cancel := context.WithTimeout(e.Ctx, e.patience())
	defer cancel()
	if n != nil {
		if err := app.WaitCaughtUp(ctx, e.Bus, n.Progress, stream.TopicRaw); err != nil {
			e.T.Fatal(err)
		}
	}
	if s != nil {
		if err := app.WaitCaughtUp(ctx, e.Bus, s.Progress, stream.CanonicalTopics...); err != nil {
			e.T.Fatal(err)
		}
	}
}

// Rejects returns the dead letters by reason.
func (e *Env) Rejects() map[string]int {
	out := map[string]int{}
	for _, r := range e.Records(stream.TopicRejects) {
		var rej domain.Reject
		if json.Unmarshal(r.Value, &rej) == nil {
			out[rej.Reason]++
		}
	}
	return out
}

// Gaps returns the declared gaps.
func (e *Env) Gaps() []domain.Gap {
	var out []domain.Gap
	for _, r := range e.Records(stream.TopicGaps) {
		var g domain.Gap
		if json.Unmarshal(r.Value, &g) == nil {
			out = append(out, g)
		}
	}
	return out
}

// Canonical returns how many trade, quote and depth records are in the log,
// counting redeliveries.
func (e *Env) Canonical() int {
	return len(e.Records(stream.TopicTrades)) + len(e.Records(stream.TopicQuotes)) + len(e.Records(stream.TopicDepth))
}

// Stored returns how many distinct trade, quote and depth rows are in the store.
func (e *Env) Stored() int {
	e.T.Helper()
	if e.MemDB != nil {
		return e.MemDB.Rows("trades") + e.MemDB.Rows("quotes") + e.MemDB.Rows("depth")
	}
	counter, ok := e.Store.(interface {
		Count(ctx context.Context, table string) (int64, error)
	})
	if !ok {
		e.T.Fatalf("store %T cannot count rows", e.Store)
	}
	var total int64
	for _, table := range []string{"trades", "quotes", "depth"} {
		n, err := counter.Count(e.Ctx, table)
		if err != nil {
			e.T.Fatalf("count %s: %v", table, err)
		}
		total += n
	}
	return int(total)
}
