package simulator

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"strconv"
	"sync"
	"time"

	"mdp/src/ingestors"
	"mdp/src/refdata"
)

// Config configures a pair of simulated venues.
type Config struct {
	Seed         uint64
	AlphaAddr    string // listen address of the alpha WebSocket venue
	BetaAddr     string // listen address of the beta TCP venue
	ResendBuffer int    // frames each venue keeps for resume
	Session      string // session id suffix; a restart with the same id continues the session
}

// Sim runs the alpha and beta venues.
type Sim struct {
	Alpha, Beta *Venue

	mu          sync.Mutex // guards the markets
	alphaMarket *Market
	betaMarket  *Market
	alphaLn     net.Listener
	betaLn      net.Listener
	alphaSrv    *http.Server
	wg          sync.WaitGroup
}

// New builds the venues without opening any sockets.
func New(reg *refdata.Registry, cfg Config) (*Sim, error) {
	if cfg.ResendBuffer == 0 {
		cfg.ResendBuffer = 20000
	}
	if cfg.Session == "" {
		cfg.Session = "S1"
	}
	alpha, err := NewVenue(WireAlpha, "A-"+cfg.Session, reg, ingestors.ProviderAlpha, cfg.ResendBuffer)
	if err != nil {
		return nil, err
	}
	beta, err := NewVenue(WireBeta, "B-"+cfg.Session, reg, ingestors.ProviderBeta, cfg.ResendBuffer)
	if err != nil {
		return nil, err
	}
	return &Sim{
		Alpha: alpha, Beta: beta,
		alphaMarket: NewMarket(cfg.Seed, alpha.Symbols(), false),
		betaMarket:  NewMarket(cfg.Seed+1, beta.Symbols(), true),
	}, nil
}

// Start builds the venues and serves them until ctx ends.
func Start(ctx context.Context, reg *refdata.Registry, cfg Config) (*Sim, error) {
	s, err := New(reg, cfg)
	if err != nil {
		return nil, err
	}
	if s.alphaLn, err = net.Listen("tcp", cfg.AlphaAddr); err != nil {
		return nil, fmt.Errorf("simulator: alpha listen: %w", err)
	}
	if s.betaLn, err = net.Listen("tcp", cfg.BetaAddr); err != nil {
		s.alphaLn.Close()
		return nil, fmt.Errorf("simulator: beta listen: %w", err)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/feed", s.Alpha.ServeAlpha)
	s.alphaSrv = &http.Server{Handler: mux}
	s.wg.Add(3)
	go func() { defer s.wg.Done(); s.alphaSrv.Serve(s.alphaLn) }()
	go func() { defer s.wg.Done(); s.Beta.ServeBeta(ctx, s.betaLn) }()
	go func() {
		defer s.wg.Done()
		<-ctx.Done()
		s.Alpha.Disconnect()
		s.Beta.Disconnect()
		s.alphaSrv.Close()
	}()
	return s, nil
}

// Wait blocks until the servers have stopped.
func (s *Sim) Wait() { s.wg.Wait() }

// AlphaURL is the alpha venue's WebSocket URL.
func (s *Sim) AlphaURL() string { return "ws://" + s.alphaLn.Addr().String() + "/feed" }

// BetaAddr is the beta venue's TCP address.
func (s *Sim) BetaAddr() string { return s.betaLn.Addr().String() }

// Step publishes exactly n events on each venue, stamped with the current
// time. Tests use it to know precisely what was sent.
func (s *Sim) Step(n int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := 0; i < n; i++ {
		now := time.Now()
		s.Alpha.Publish(s.alphaMarket.Next(now))
		s.Beta.Publish(s.betaMarket.Next(now))
	}
}

// Run publishes rate events per second on each venue until ctx ends.
func (s *Sim) Run(ctx context.Context, rate float64) {
	const tick = 2 * time.Millisecond
	t := time.NewTicker(tick)
	defer t.Stop()
	start, sent := time.Now(), 0
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			// Publish whatever is owed since start, so the average rate
			// holds even when ticks are late.
			if owed := int(time.Since(start).Seconds()*rate) - sent; owed > 0 {
				s.Step(owed)
				sent += owed
			}
		}
	}
}

// AdminHandler exposes fault injection over HTTP for demos:
//
//	POST /faults/{venue}/{fault}?n=3&ms=500
//
// venue is alpha or beta. fault is one of disconnect, drop, lose, duplicate,
// garbage, offtick, stale, skew, pause, resume, newsession.
func (s *Sim) AdminHandler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /faults/{venue}/{fault}", func(w http.ResponseWriter, r *http.Request) {
		v := map[string]*Venue{"alpha": s.Alpha, "beta": s.Beta}[r.PathValue("venue")]
		if v == nil {
			http.Error(w, "venue must be alpha or beta", http.StatusNotFound)
			return
		}
		n, _ := strconv.Atoi(r.URL.Query().Get("n"))
		if n <= 0 {
			n = 1
		}
		ms, _ := strconv.Atoi(r.URL.Query().Get("ms"))
		d := time.Duration(ms) * time.Millisecond
		if !ApplyFault(v, r.PathValue("fault"), n, d, r.URL.Query().Get("id")) {
			http.Error(w, "unknown fault", http.StatusNotFound)
			return
		}
		fmt.Fprintf(w, "ok: %s %s\n", r.PathValue("venue"), r.PathValue("fault"))
	})
	return mux
}

// ApplyFault arms a named fault on a venue. It reports false for an unknown
// fault name.
func ApplyFault(v *Venue, fault string, n int, d time.Duration, session string) bool {
	switch fault {
	case "disconnect":
		v.Disconnect()
	case "drop":
		v.DropNext(n)
	case "lose":
		v.LoseNext(n)
	case "duplicate":
		v.DuplicateNext(n)
	case "garbage":
		v.GarbageNext(n)
	case "offtick":
		v.OffTickNext(n)
	case "stale":
		v.StaleNext(n, d)
	case "skew":
		v.SetSkew(d)
	case "pause":
		v.Pause(true)
	case "resume":
		v.Pause(false)
	case "newsession":
		v.NewSession(session)
	default:
		return false
	}
	return true
}
