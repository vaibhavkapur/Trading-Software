// Package api exposes the router over HTTP. All trading is simulated.
//
// The server drives two engines in lockstep on the same seed: the live router
// and a shadow running the naive single-venue policy. Every order, fault and
// kill-switch command is applied to both, which gives /execution-quality an
// identical simulated baseline to compare against.
package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"sor/src/engine"
	"sor/src/risk"
	"sor/src/tca"
	"sor/src/venuesim"
)

type Server struct {
	mu            sync.Mutex
	live, shadow  *engine.Engine
	baselineVenue string
	speed         float64 // simulated ms per wall ms; 0 = manual clock (POST /sim/advance)
	wallStart     time.Time
	simStart      int64
}

// New creates a server. speed is how fast simulated time runs relative to
// wall time; 0 gives a manual clock advanced only through POST /sim/advance.
func New(cfg engine.Config, baselineVenue string, speed float64, log io.Writer) *Server {
	cfg = cfg.WithDefaults()
	shadowCfg := cfg
	shadowCfg.Policy.SingleVenue = baselineVenue
	return &Server{live: engine.New(cfg, log), shadow: engine.New(shadowCfg, nil), baselineVenue: baselineVenue,
		speed: speed, wallStart: time.Now(), simStart: cfg.StartMs}
}

// lock takes the mutex and brings both engines up to the current simulated time.
func (s *Server) lock() {
	s.mu.Lock()
	if s.speed > 0 {
		s.advanceTo(s.simStart + int64(float64(time.Since(s.wallStart).Milliseconds())*s.speed))
	}
}

func (s *Server) advanceTo(t int64) {
	s.live.AdvanceTo(t)
	s.shadow.AdvanceTo(t)
}

// Run keeps simulated time moving until ctx is cancelled (no-op for a manual clock).
func (s *Server) Run(ctx context.Context) {
	if s.speed <= 0 {
		return
	}
	t := time.NewTicker(50 * time.Millisecond)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			s.lock()
			s.mu.Unlock()
		}
	}
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(v)
}

func fail(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]string{"error": msg})
}

func decode(w http.ResponseWriter, r *http.Request, v any) bool {
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		fail(w, http.StatusBadRequest, "invalid JSON body: "+err.Error())
		return false
	}
	return true
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	// with wraps a handler so it runs under the lock at current simulated time.
	with := func(h func(w http.ResponseWriter, r *http.Request)) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			s.lock()
			defer s.mu.Unlock()
			h(w, r)
		}
	}
	mux.HandleFunc("GET /healthz", with(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, map[string]any{"status": "ok", "mode": "SIMULATED / PAPER", "sim_time_ms": s.live.Now(),
			"kill_switch": s.live.Killed()})
	}))
	mux.HandleFunc("POST /parents", with(func(w http.ResponseWriter, r *http.Request) {
		var req engine.ParentRequest
		if !decode(w, r, &req) {
			return
		}
		p, err := s.live.SubmitParent(req)
		s.shadow.SubmitParent(req)
		var rej *risk.Rejection
		if errors.As(err, &rej) {
			writeJSON(w, http.StatusUnprocessableEntity, map[string]any{"error": rej.Error(), "code": rej.Code, "parent": p})
			return
		}
		writeJSON(w, http.StatusCreated, p)
	}))
	mux.HandleFunc("GET /parents", with(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, s.live.Parents())
	}))
	parent := func(h func(w http.ResponseWriter, id string)) http.HandlerFunc {
		return with(func(w http.ResponseWriter, r *http.Request) {
			id := r.PathValue("id")
			if _, ok := s.live.Parent(id); !ok {
				fail(w, http.StatusNotFound, "unknown parent "+id)
				return
			}
			h(w, id)
		})
	}
	mux.HandleFunc("GET /parents/{id}", parent(func(w http.ResponseWriter, id string) {
		p, _ := s.live.Parent(id)
		writeJSON(w, 200, p)
	}))
	mux.HandleFunc("GET /parents/{id}/children", parent(func(w http.ResponseWriter, id string) {
		writeJSON(w, 200, s.live.Children(id))
	}))
	mux.HandleFunc("GET /parents/{id}/fills", parent(func(w http.ResponseWriter, id string) {
		writeJSON(w, 200, s.live.Fills(id))
	}))
	mux.HandleFunc("GET /parents/{id}/execution-quality", parent(func(w http.ResponseWriter, id string) {
		rep, _ := s.live.Report(id)
		if base, ok := s.shadow.Report(id); ok {
			rep.Baseline = tca.Compare(rep, base, s.baselineVenue)
		}
		writeJSON(w, 200, rep)
	}))
	mux.HandleFunc("POST /parents/{id}/cancel", parent(func(w http.ResponseWriter, id string) {
		if !s.live.CancelParent(id) {
			fail(w, http.StatusConflict, "parent is not working")
			return
		}
		s.shadow.CancelParent(id)
		p, _ := s.live.Parent(id)
		writeJSON(w, 200, p)
	}))
	mux.HandleFunc("GET /venues", with(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, s.live.MarketView())
	}))
	mux.HandleFunc("POST /sim/venues/{venue}/faults", with(func(w http.ResponseWriter, r *http.Request) {
		var f venuesim.Fault
		if !decode(w, r, &f) {
			return
		}
		venue := r.PathValue("venue")
		if err := s.live.SetFault(venue, f); err != nil {
			code := http.StatusBadRequest
			if strings.HasPrefix(err.Error(), "unknown venue") {
				code = http.StatusNotFound
			}
			fail(w, code, err.Error())
			return
		}
		s.shadow.SetFault(venue, f)
		writeJSON(w, 200, map[string]any{"venue": venue, "fault": f})
	}))
	mux.HandleFunc("POST /sim/advance", with(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Ms int64 `json:"ms"`
		}
		if !decode(w, r, &req) {
			return
		}
		if s.speed > 0 || req.Ms <= 0 || req.Ms > 24*3600*1000 {
			fail(w, http.StatusBadRequest, "advance needs a manual clock (-speed 0) and 0 < ms <= 86400000")
			return
		}
		s.advanceTo(s.live.Now() + req.Ms)
		writeJSON(w, 200, map[string]int64{"sim_time_ms": s.live.Now()})
	}))
	mux.HandleFunc("POST /kill-switch", with(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Engaged bool `json:"engaged"`
		}
		if !decode(w, r, &req) {
			return
		}
		s.live.SetKill(req.Engaged)
		s.shadow.SetKill(req.Engaged)
		writeJSON(w, 200, map[string]bool{"engaged": req.Engaged})
	}))
	mux.HandleFunc("GET /metrics", with(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain; version=0.0.4")
		c := s.live.Counters()
		keys := make([]string, 0, len(c))
		for k := range c {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			fmt.Fprintf(w, "%s %d\n", k, c[k])
		}
		kill := 0
		if s.live.Killed() {
			kill = 1
		}
		fmt.Fprintf(w, "sor_kill_switch_engaged %d\nsor_sim_time_ms %d\n", kill, s.live.Now())
		for _, p := range s.live.Parents() {
			fmt.Fprintf(w, "sor_parent_filled_qty{parent=%q,status=%q} %d\n", p.ID, p.Status, p.FilledQty)
		}
	}))
	return mux
}
