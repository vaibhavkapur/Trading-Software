package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/coder/websocket"

	"mdp/src/app"
	"mdp/src/gateway"
	"mdp/src/ingestors"
	"mdp/src/metrics"
	"mdp/src/normalizer"
	"mdp/src/refdata"
	"mdp/src/simulator"
	"mdp/src/storage"
	"mdp/src/stream"
)

// rig is a complete pipeline in one process whose services can be stopped
// and started one by one: simulated venues on loopback sockets, the real
// services, and the gateway on a real socket. The demo and bench commands
// script it. The log and store are in memory unless the caller supplies others.
type rig struct {
	ctx   context.Context
	st    *app.Stack
	mem   *stream.MemBus    // nil on a real broker
	memDB *storage.MemStore // nil on a real store
	sim   *simulator.Sim
	url   string // gateway base URL

	alpha, beta *ingestors.Runner
	ingest      []*svc
	norm        *normalizer.Service
	normSvc     *svc
	sink        *storage.Sink
	sinkSvc     *svc
}

// svc is one running service.
type svc struct {
	cancel context.CancelFunc
	done   chan struct{}
	err    error
}

func (s *svc) stop() {
	s.cancel()
	<-s.done
}

func (r *rig) start(run func(context.Context) error) *svc {
	ctx, cancel := context.WithCancel(r.ctx)
	s := &svc{cancel: cancel, done: make(chan struct{})}
	go func() {
		defer close(s.done)
		s.err = run(ctx)
	}()
	return s
}

// newRig builds the pipeline and starts the gateway. bus and store may be
// nil, meaning in-memory.
func newRig(ctx context.Context, reg *refdata.Registry, bus stream.Bus, store storage.Store, opt app.Options, seed uint64, log *slog.Logger) (*rig, error) {
	r := &rig{ctx: ctx}
	if bus == nil {
		r.mem = stream.NewMemBus()
		bus = r.mem
	}
	if store == nil {
		r.memDB = storage.NewMemStore()
		store = r.memDB
	}
	if log == nil {
		log = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	r.st = &app.Stack{Reg: reg, Bus: bus, Store: store, M: metrics.New(), Opt: opt, Log: log}
	ictx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	if err := r.st.Init(ictx); err != nil {
		return nil, err
	}
	var err error
	session := time.Now().UTC().Format("20060102T150405.000")
	if r.sim, err = simulator.Start(ctx, reg, simulator.Config{Seed: seed, AlphaAddr: "127.0.0.1:0", BetaAddr: "127.0.0.1:0", Session: session}); err != nil {
		return nil, err
	}
	hub, srv := r.st.NewGateway(gateway.OpenEntitlements())
	bound, wait, err := serveHTTP(ctx, "127.0.0.1:0", srv.Handler())
	if err != nil {
		return nil, err
	}
	r.url = "http://" + bound.String()
	go wait()
	r.start(hub.Run)
	select {
	case <-hub.Ready():
	case <-time.After(30 * time.Second):
		return nil, fmt.Errorf("gateway did not become ready")
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	return r, nil
}

func (r *rig) startIngestors() error {
	r.alpha = r.st.NewIngestor(&ingestors.Alpha{URL: r.sim.AlphaURL(), Symbols: r.st.Reg.Symbols(ingestors.ProviderAlpha)})
	r.beta = r.st.NewIngestor(&ingestors.Beta{Addr: r.sim.BetaAddr()})
	r.ingest = []*svc{r.start(r.alpha.Run), r.start(r.beta.Run)}
	return r.wait(15*time.Second, "ingestors to connect", func() bool {
		return r.sim.Alpha.Subscribers() == 1 && r.sim.Beta.Subscribers() == 1
	})
}

func (r *rig) startNormalizer() {
	r.norm = r.st.NewNormalizer()
	r.normSvc = r.start(r.norm.Run)
}

func (r *rig) startSink() {
	r.sink = r.st.NewSink()
	r.sinkSvc = r.start(r.sink.Run)
}

// wait polls cond until it holds.
func (r *rig) wait(timeout time.Duration, what string, cond func() bool) error {
	deadline := time.Now().Add(timeout)
	for !cond() {
		if err := r.ctx.Err(); err != nil {
			return err
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("timed out waiting for %s", what)
		}
		time.Sleep(2 * time.Millisecond)
	}
	return nil
}

// rawCount is the number of records in the raw topic.
func (r *rig) rawCount() (int64, error) {
	ends, err := r.st.Bus.EndOffsets(r.ctx, stream.TopicRaw)
	var n int64
	for _, e := range ends {
		n += e
	}
	return n, err
}

// waitRaw waits until the raw topic holds n records.
func (r *rig) waitRaw(n int64, timeout time.Duration) error {
	var have int64
	err := r.wait(timeout, "raw events", func() bool {
		have, _ = r.rawCount()
		return have >= n
	})
	if err != nil {
		return fmt.Errorf("%w: raw topic holds %d of %d expected events", err, have, n)
	}
	return nil
}

// settle waits until everything in the log has been normalized and stored.
func (r *rig) settle(timeout time.Duration) error {
	ctx, cancel := context.WithTimeout(r.ctx, timeout)
	defer cancel()
	if err := app.WaitCaughtUp(ctx, r.st.Bus, r.norm.Progress, stream.TopicRaw); err != nil {
		return err
	}
	return app.WaitCaughtUp(ctx, r.st.Bus, r.sink.Progress, stream.CanonicalTopics...)
}

// lag is how many records a consumer is behind the end of the log.
func (r *rig) lag(progress func() map[stream.TP]int64) int64 {
	lags, err := stream.Lag(r.ctx, r.st.Bus, progress())
	if err != nil {
		return -1
	}
	var total int64
	for _, l := range lags {
		total += l
	}
	return total
}

// stored is the number of distinct trade, quote and depth rows in the store.
func (r *rig) stored() (int64, error) {
	if r.memDB != nil {
		return int64(r.memDB.Rows("trades") + r.memDB.Rows("quotes") + r.memDB.Rows("depth")), nil
	}
	counter, ok := r.st.Store.(interface {
		Count(ctx context.Context, table string) (int64, error)
	})
	if !ok {
		return 0, fmt.Errorf("store %T cannot count rows", r.st.Store)
	}
	var total int64
	for _, table := range []string{"trades", "quotes", "depth"} {
		n, err := counter.Count(r.ctx, table)
		if err != nil {
			return 0, err
		}
		total += n
	}
	return total, nil
}

func (r *rig) get(path string) (int, map[string]any, error) {
	req, err := http.NewRequestWithContext(r.ctx, http.MethodGet, r.url+path, nil)
	if err != nil {
		return 0, nil, err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	var body map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return resp.StatusCode, nil, fmt.Errorf("GET %s: %w", path, err)
	}
	return resp.StatusCode, body, nil
}

// wsClient is a live subscriber that records what it is sent.
type wsClient struct {
	conn *websocket.Conn

	mu       sync.Mutex
	events   int
	firstRaw []string         // the first few events as received
	gaps     []map[string]any // in-band gap notices
	status   map[string]any   // latest status message
	onEvent  func(m map[string]any, at time.Time)
	readErr  error
}

func (r *rig) subscribe(query string, onEvent func(map[string]any, time.Time)) (*wsClient, error) {
	ctx, cancel := context.WithTimeout(r.ctx, 10*time.Second)
	defer cancel()
	conn, _, err := websocket.Dial(ctx, "ws"+r.url[len("http"):]+"/v1/stream?"+query, nil)
	if err != nil {
		return nil, err
	}
	conn.SetReadLimit(1 << 20)
	c := &wsClient{conn: conn, onEvent: onEvent}
	go func() {
		for {
			_, data, err := conn.Read(r.ctx)
			at := time.Now()
			if err != nil {
				c.mu.Lock()
				c.readErr = err
				c.mu.Unlock()
				return
			}
			var m map[string]any
			if json.Unmarshal(data, &m) != nil {
				continue
			}
			c.mu.Lock()
			switch m["type"] {
			case "trade", "quote", "depth":
				c.events++
				if len(c.firstRaw) < 2 {
					c.firstRaw = append(c.firstRaw, string(data))
				}
				if c.onEvent != nil {
					c.onEvent(m, at)
				}
			case "gap":
				c.gaps = append(c.gaps, m)
			case "status":
				c.status = m
			}
			c.mu.Unlock()
		}
	}()
	return c, nil
}

func (c *wsClient) snapshot() (events int, gaps []map[string]any, status map[string]any) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.events, append([]map[string]any(nil), c.gaps...), c.status
}

func (c *wsClient) close() { c.conn.CloseNow() }
