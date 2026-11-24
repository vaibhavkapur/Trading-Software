package brokersim

import (
	"crypto/subtle"
	"encoding/json"
	"net/http"
	"strings"

	"oms/src/broker"
	"oms/src/fixed"
)

// Handler returns the broker's HTTP API:
//
//	POST /v1/orders                 submit; 202; idempotent on client_order_id
//	POST /v1/orders/{id}/cancel     request a cancel; 202; Idempotency-Key
//	POST /v1/orders/{id}/replace    request a replace; 202; Idempotency-Key
//	GET  /v1/orders/{id}            the broker's view of one order
//	GET  /v1/snapshot               all orders and executions
//
// and the simulator's controls, which a real broker would not have:
//
//	GET  /sim/faults, PUT /sim/faults   read or replace the fault settings
//	PUT  /sim/prices                    {"symbol": "price", ...}
//	POST /sim/session/close             expire open DAY orders
//	GET  /sim/stats
//
// Every route requires the bearer API key.
func (s *Sim) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/orders", s.handleSubmit)
	mux.HandleFunc("POST /v1/orders/{id}/cancel", s.handleCancel)
	mux.HandleFunc("POST /v1/orders/{id}/replace", s.handleReplace)
	mux.HandleFunc("GET /v1/orders/{id}", s.handleGetOrder)
	mux.HandleFunc("GET /v1/snapshot", s.handleSnapshot)

	mux.HandleFunc("GET /sim/faults", func(w http.ResponseWriter, _ *http.Request) { reply(w, http.StatusOK, s.Faults()) })
	mux.HandleFunc("PUT /sim/faults", s.handleSetFaults)
	mux.HandleFunc("PUT /sim/prices", s.handleSetPrices)
	mux.HandleFunc("POST /sim/session/close", func(w http.ResponseWriter, _ *http.Request) {
		reply(w, http.StatusOK, map[string]int{"expired": s.CloseSession()})
	})
	mux.HandleFunc("GET /sim/stats", func(w http.ResponseWriter, _ *http.Request) { reply(w, http.StatusOK, s.Stats()) })
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		reply(w, http.StatusOK, map[string]string{"status": "ok", "mode": "SIMULATED"})
	})

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/healthz" {
			mux.ServeHTTP(w, r)
			return
		}
		token, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		if !ok || s.cfg.APIKey == "" || subtle.ConstantTimeCompare([]byte(token), []byte(s.cfg.APIKey)) != 1 {
			refuse(w, http.StatusUnauthorized, "UNAUTHORIZED", "a valid bearer token is required")
			return
		}
		// Faults apply to the broker API, not to the simulator's controls.
		if strings.HasPrefix(r.URL.Path, "/v1/") {
			s.mu.Lock()
			down := s.faults.Down
			fail := !down && s.faults.FailRequests > 0
			if fail {
				s.faults.FailRequests--
			}
			s.mu.Unlock()
			if down {
				dropConnection(w)
				return
			}
			if fail {
				refuse(w, http.StatusServiceUnavailable, "UNAVAILABLE", "injected fault: request not processed")
				return
			}
		}
		r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
		mux.ServeHTTP(w, r)
	})
}

// dropConnection closes the connection without answering, as a crashed or
// unreachable broker would.
func dropConnection(w http.ResponseWriter) {
	if hj, ok := w.(http.Hijacker); ok {
		if conn, _, err := hj.Hijack(); err == nil {
			conn.Close()
			return
		}
	}
	panic(http.ErrAbortHandler)
}

// loseResponse reports whether this processed request should be answered as
// if its response were lost. The caller holds s.mu.
func (s *Sim) loseResponse() bool {
	if s.faults.LoseResponses > 0 {
		s.faults.LoseResponses--
		return true
	}
	return false
}

func (s *Sim) handleSubmit(w http.ResponseWriter, r *http.Request) {
	var req broker.SubmitRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		refuse(w, http.StatusBadRequest, "MALFORMED_REQUEST", err.Error())
		return
	}
	switch {
	case req.ClientOrderID == "" || req.Symbol == "":
		refuse(w, http.StatusUnprocessableEntity, "INVALID_ORDER", "client_order_id and symbol are required")
		return
	case req.Qty <= 0:
		refuse(w, http.StatusUnprocessableEntity, "INVALID_ORDER", "qty must be positive")
		return
	}
	s.mu.Lock()
	o, _ := s.submit(req)
	snap := o.OrderSnapshot
	lost := s.loseResponse()
	s.mu.Unlock()
	if lost {
		refuse(w, http.StatusServiceUnavailable, "UNAVAILABLE", "injected fault: processed, response lost")
		return
	}
	reply(w, http.StatusAccepted, snap)
}

func (s *Sim) handleCancel(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	known, _ := s.requestCancel(r.PathValue("id"), r.Header.Get("Idempotency-Key"))
	lost := known && s.loseResponse()
	s.mu.Unlock()
	s.answerAction(w, known, lost)
}

func (s *Sim) handleReplace(w http.ResponseWriter, r *http.Request) {
	var req broker.ReplaceRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		refuse(w, http.StatusBadRequest, "MALFORMED_REQUEST", err.Error())
		return
	}
	if req.Qty <= 0 {
		refuse(w, http.StatusUnprocessableEntity, "INVALID_REPLACE", "qty must be positive")
		return
	}
	s.mu.Lock()
	known, _ := s.requestReplace(r.PathValue("id"), r.Header.Get("Idempotency-Key"), req)
	lost := known && s.loseResponse()
	s.mu.Unlock()
	s.answerAction(w, known, lost)
}

func (s *Sim) answerAction(w http.ResponseWriter, known, lost bool) {
	switch {
	case !known:
		refuse(w, http.StatusNotFound, "UNKNOWN_ORDER", "the broker has no such order")
	case lost:
		refuse(w, http.StatusServiceUnavailable, "UNAVAILABLE", "injected fault: processed, response lost")
	default:
		reply(w, http.StatusAccepted, map[string]string{"status": "accepted"})
	}
}

func (s *Sim) handleGetOrder(w http.ResponseWriter, r *http.Request) {
	o, ok := s.Order(r.PathValue("id"))
	if !ok {
		refuse(w, http.StatusNotFound, "UNKNOWN_ORDER", "the broker has no such order")
		return
	}
	reply(w, http.StatusOK, o)
}

func (s *Sim) handleSnapshot(w http.ResponseWriter, _ *http.Request) {
	if s.Faults().SnapshotDown {
		refuse(w, http.StatusServiceUnavailable, "UNAVAILABLE", "injected fault: snapshot unavailable")
		return
	}
	reply(w, http.StatusOK, s.Snapshot())
}

func (s *Sim) handleSetFaults(w http.ResponseWriter, r *http.Request) {
	var f Faults
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&f); err != nil {
		refuse(w, http.StatusBadRequest, "MALFORMED_REQUEST", err.Error())
		return
	}
	s.SetFaults(f)
	reply(w, http.StatusOK, f)
}

func (s *Sim) handleSetPrices(w http.ResponseWriter, r *http.Request) {
	var prices map[string]fixed.Micros
	if err := json.NewDecoder(r.Body).Decode(&prices); err != nil {
		refuse(w, http.StatusBadRequest, "MALFORMED_REQUEST", err.Error())
		return
	}
	for symbol, price := range prices {
		if price <= 0 {
			refuse(w, http.StatusUnprocessableEntity, "INVALID_PRICE", "prices must be positive")
			return
		}
		s.SetPrice(symbol, price)
	}
	reply(w, http.StatusOK, prices)
}

func reply(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(body)
}

func refuse(w http.ResponseWriter, status int, code, message string) {
	reply(w, status, map[string]any{"error": map[string]string{"code": code, "message": message}})
}
