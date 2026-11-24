// Package api is the HTTP surface of the OMS: the command API for clients,
// the reconciliation API for operators, the broker webhook receiver, and the
// health and metrics endpoints.
package api

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgconn"

	"oms/src/auth"
	"oms/src/broker"
	"oms/src/domain"
	"oms/src/metrics"
	"oms/src/reconcile"
	"oms/src/service"
	"oms/src/store"
)

// maxBody bounds every request body.
const maxBody = 1 << 20

// Server holds what the handlers need.
type Server struct {
	Service    *service.Service
	Reconciler *reconcile.Reconciler
	Store      *store.Store
	// Keys authenticates API callers. With no keys every API route answers
	// 401: the API is closed, not open.
	Keys *auth.Keys
	// WebhookSecret authenticates the broker. With no secret the webhook
	// route is disabled.
	WebhookSecret string
	Metrics       *metrics.Set
	Log           *slog.Logger
}

// Handler returns the routed, instrumented HTTP handler.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("POST /v1/orders", s.authed(s.createOrder))
	mux.HandleFunc("GET /v1/orders", s.authed(s.listOrders))
	mux.HandleFunc("GET /v1/orders/{id}", s.authed(s.getOrder))
	mux.HandleFunc("GET /v1/orders/{id}/events", s.authed(s.listEvents))
	mux.HandleFunc("GET /v1/orders/{id}/executions", s.authed(s.listExecutions))
	mux.HandleFunc("POST /v1/orders/{id}/cancel", s.authed(s.cancelOrder))
	mux.HandleFunc("POST /v1/orders/{id}/replace", s.authed(s.replaceOrder))
	mux.HandleFunc("GET /v1/positions", s.authed(s.listPositions))

	mux.HandleFunc("POST /v1/reconciliation/runs", s.authed(s.operator(s.startRun)))
	mux.HandleFunc("GET /v1/reconciliation/runs/{id}", s.authed(s.operator(s.getRun)))
	mux.HandleFunc("GET /v1/reconciliation/issues", s.authed(s.operator(s.listIssues)))
	mux.HandleFunc("GET /v1/reconciliation/issues/{id}", s.authed(s.operator(s.getIssue)))
	mux.HandleFunc("POST /v1/reconciliation/issues/{id}/resolve", s.authed(s.operator(s.resolveIssue)))

	mux.HandleFunc("POST /v1/broker/webhooks", s.webhook)

	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})
	mux.HandleFunc("GET /readyz", s.ready)
	mux.HandleFunc("GET /metrics", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
		s.Metrics.Registry.Write(w)
	})

	return s.instrument(mux)
}

// instrument records metrics, bounds request bodies and turns a handler
// panic into a 500 instead of a dropped connection.
func (s *Server) instrument(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		r.Body = http.MaxBytesReader(rec, r.Body, maxBody)
		defer func() {
			if p := recover(); p != nil {
				s.Log.Error("handler panic", "panic", p, "path", r.URL.Path)
				if !rec.wrote {
					writeError(rec, http.StatusInternalServerError, "INTERNAL", "internal error")
				}
			}
			route := r.Pattern // set by the mux; empty when nothing matched
			if route == "" {
				route = "unmatched"
			}
			s.Metrics.HTTPRequests.Inc(route, strconv.Itoa(rec.status))
			s.Metrics.HTTPDuration.Observe(time.Since(start).Seconds(), route)
		}()
		next.ServeHTTP(rec, r)
	})
}

type statusRecorder struct {
	http.ResponseWriter
	status int
	wrote  bool
}

func (r *statusRecorder) WriteHeader(code int) {
	if !r.wrote {
		r.status, r.wrote = code, true
	}
	r.ResponseWriter.WriteHeader(code)
}

func (r *statusRecorder) Write(b []byte) (int, error) {
	r.wrote = true
	return r.ResponseWriter.Write(b)
}

type principalHandler func(w http.ResponseWriter, r *http.Request, p auth.Principal)

// authed resolves the bearer token to a principal or answers 401.
func (s *Server) authed(next principalHandler) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		token, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		p, known := s.Keys.Authenticate(strings.TrimSpace(token))
		if !ok || !known {
			w.Header().Set("WWW-Authenticate", `Bearer realm="oms"`)
			writeError(w, http.StatusUnauthorized, "UNAUTHORIZED", "a valid bearer token is required")
			return
		}
		next(w, r, p)
	}
}

// operator restricts a route to the operator role.
func (s *Server) operator(next principalHandler) principalHandler {
	return func(w http.ResponseWriter, r *http.Request, p auth.Principal) {
		if !p.CanOperate() {
			writeError(w, http.StatusForbidden, "FORBIDDEN", "this route is for operators")
			return
		}
		next(w, r, p)
	}
}

func (s *Server) createOrder(w http.ResponseWriter, r *http.Request, p auth.Principal) {
	var req service.CreateOrderRequest
	if !s.decode(w, r, &req) {
		return
	}
	res, err := s.Service.CreateOrder(r.Context(), p, r.Header.Get("Idempotency-Key"), req)
	s.writeResult(w, r, res, err)
}

func (s *Server) cancelOrder(w http.ResponseWriter, r *http.Request, p auth.Principal) {
	res, err := s.Service.CancelOrder(r.Context(), p, r.PathValue("id"))
	s.writeResult(w, r, res, err)
}

func (s *Server) replaceOrder(w http.ResponseWriter, r *http.Request, p auth.Principal) {
	var req service.ReplaceOrderRequest
	if !s.decode(w, r, &req) {
		return
	}
	res, err := s.Service.ReplaceOrder(r.Context(), p, r.Header.Get("Idempotency-Key"), r.PathValue("id"), req)
	s.writeResult(w, r, res, err)
}

func (s *Server) writeResult(w http.ResponseWriter, r *http.Request, res service.Result, err error) {
	if err != nil {
		s.fail(w, r, err)
		return
	}
	if res.Replayed {
		w.Header().Set("Idempotent-Replayed", "true")
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(res.Status)
	w.Write(res.Body)
	w.Write([]byte("\n"))
}

func (s *Server) getOrder(w http.ResponseWriter, r *http.Request, p auth.Principal) {
	o, err := s.Service.GetOrder(r.Context(), p, r.PathValue("id"))
	s.respond(w, r, http.StatusOK, o, err)
}

func (s *Server) listOrders(w http.ResponseWriter, r *http.Request, p auth.Principal) {
	q := r.URL.Query()
	limit, _ := strconv.Atoi(q.Get("limit"))
	orders, err := s.Service.ListOrders(r.Context(), p, q.Get("account_id"), domain.State(q.Get("state")), limit)
	s.respond(w, r, http.StatusOK, map[string]any{"orders": orders}, err)
}

func (s *Server) listEvents(w http.ResponseWriter, r *http.Request, p auth.Principal) {
	events, err := s.Service.ListEvents(r.Context(), p, r.PathValue("id"))
	s.respond(w, r, http.StatusOK, map[string]any{"events": events}, err)
}

func (s *Server) listExecutions(w http.ResponseWriter, r *http.Request, p auth.Principal) {
	execs, err := s.Service.ListExecutions(r.Context(), p, r.PathValue("id"))
	s.respond(w, r, http.StatusOK, map[string]any{"executions": execs}, err)
}

func (s *Server) listPositions(w http.ResponseWriter, r *http.Request, p auth.Principal) {
	positions, err := s.Service.ListPositions(r.Context(), p, r.URL.Query().Get("account_id"))
	s.respond(w, r, http.StatusOK, map[string]any{"positions": positions}, err)
}

func (s *Server) startRun(w http.ResponseWriter, r *http.Request, p auth.Principal) {
	run, err := s.Reconciler.Run(r.Context(), "api:"+p.Name)
	switch {
	case errors.Is(err, reconcile.ErrRunInProgress):
		writeError(w, http.StatusConflict, "RUN_IN_PROGRESS", err.Error())
	case err != nil && run.ID != "":
		// The run was recorded as FAILED, typically because the broker
		// snapshot was unavailable. Nothing was opened or closed.
		s.Log.Error("reconciliation run failed", "run_id", run.ID, "err", err)
		writeJSON(w, http.StatusBadGateway, map[string]any{
			"error": errorBody{Code: "RECONCILIATION_FAILED", Message: "the run failed; no issues were opened or closed"},
			"run":   run,
		})
	default:
		s.respond(w, r, http.StatusCreated, run, err)
	}
}

func (s *Server) getRun(w http.ResponseWriter, r *http.Request, _ auth.Principal) {
	run, err := s.Store.GetRun(r.Context(), r.PathValue("id"))
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "RUN_NOT_FOUND", "no such reconciliation run")
		return
	}
	s.respond(w, r, http.StatusOK, run, err)
}

func (s *Server) listIssues(w http.ResponseWriter, r *http.Request, _ auth.Principal) {
	q := r.URL.Query()
	status := strings.ToUpper(q.Get("status"))
	if status != "" && status != store.IssueOpen && status != store.IssueResolved {
		writeError(w, http.StatusBadRequest, "BAD_REQUEST", "status must be OPEN or RESOLVED")
		return
	}
	limit, _ := strconv.Atoi(q.Get("limit"))
	issues, err := s.Store.ListIssues(r.Context(), status, q.Get("order_id"), limit)
	s.respond(w, r, http.StatusOK, map[string]any{"issues": issues}, err)
}

func (s *Server) getIssue(w http.ResponseWriter, r *http.Request, _ auth.Principal) {
	issue, err := s.Store.GetIssue(r.Context(), r.PathValue("id"))
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "ISSUE_NOT_FOUND", "no such issue")
		return
	}
	s.respond(w, r, http.StatusOK, issue, err)
}

func (s *Server) resolveIssue(w http.ResponseWriter, r *http.Request, p auth.Principal) {
	var req struct {
		Action string `json:"action"`
		Note   string `json:"note"`
	}
	if !s.decode(w, r, &req) {
		return
	}
	issue, err := s.Service.ResolveIssue(r.Context(), p, r.PathValue("id"), req.Action, req.Note)
	s.respond(w, r, http.StatusOK, issue, err)
}

// webhook receives broker reports. The status code is the contract with the
// broker's retry logic:
//
//	200  the report is durably recorded (applied, duplicate, stale or
//	     quarantined); do not send it again
//	400  the body is not a usable report; sending it again will not help
//	401  the signature does not verify
//	503  the report could not be recorded; send it again
func (s *Server) webhook(w http.ResponseWriter, r *http.Request) {
	if s.WebhookSecret == "" {
		writeError(w, http.StatusForbidden, "WEBHOOKS_DISABLED", "no webhook secret is configured")
		return
	}
	body, err := io.ReadAll(r.Body)
	if err != nil {
		writeError(w, http.StatusBadRequest, "BAD_REQUEST", "could not read the request body")
		return
	}
	if !broker.VerifySignature(s.WebhookSecret, r.Header.Get(broker.SignatureHeader), body) {
		s.Metrics.WebhookRejects.Inc("signature")
		writeError(w, http.StatusUnauthorized, "BAD_SIGNATURE", "the webhook signature does not verify")
		return
	}
	report, err := broker.NormalizeWebhook(body)
	if err != nil {
		s.Metrics.WebhookRejects.Inc("malformed")
		s.Log.Warn("malformed webhook", "err", err)
		writeError(w, http.StatusBadRequest, "MALFORMED_EVENT", err.Error())
		return
	}
	res, err := s.Service.IngestBrokerReport(r.Context(), report, domain.SourceBroker)
	if err != nil {
		s.Log.Error("webhook could not be recorded", "event_id", report.EventID, "err", err)
		w.Header().Set("Retry-After", "1")
		writeError(w, http.StatusServiceUnavailable, "UNAVAILABLE", "the event could not be recorded; retry")
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"outcome": string(res.Outcome), "detail": res.Detail})
}

func (s *Server) ready(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
	defer cancel()
	if err := s.Store.Ping(ctx); err != nil {
		writeError(w, http.StatusServiceUnavailable, "NOT_READY", "database is not reachable")
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ready"})
}

// decode reads a JSON body strictly: unknown fields and trailing data are
// errors, so a misspelled field cannot be silently ignored on an order.
func (s *Server) decode(w http.ResponseWriter, r *http.Request, into any) bool {
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	err := dec.Decode(into)
	if err == nil && dec.More() {
		err = errors.New("unexpected data after the JSON value")
	}
	if err != nil {
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) {
			writeError(w, http.StatusRequestEntityTooLarge, "BODY_TOO_LARGE", "the request body is too large")
			return false
		}
		writeError(w, http.StatusBadRequest, "MALFORMED_REQUEST", "the request body is not valid: "+err.Error())
		return false
	}
	return true
}

func (s *Server) respond(w http.ResponseWriter, r *http.Request, status int, body any, err error) {
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, status, body)
}

// fail maps an error to a response. Only service errors carry a message
// meant for the caller; anything else is logged and reported generically.
func (s *Server) fail(w http.ResponseWriter, r *http.Request, err error) {
	var se *service.Error
	switch {
	case errors.As(err, &se):
		writeJSON(w, se.Status, map[string]any{"error": errorBody{
			Code: se.Code, Message: se.Message, Fields: se.Fields, OrderID: se.OrderID, Codes: se.Codes}})
	case errors.Is(err, store.ErrNotFound):
		writeError(w, http.StatusNotFound, "ORDER_NOT_FOUND", "no such order")
	case temporarilyUnavailable(err):
		s.Log.Warn("request failed on a temporary condition", "path", r.URL.Path, "err", err)
		w.Header().Set("Retry-After", "1")
		writeError(w, http.StatusServiceUnavailable, "UNAVAILABLE", "temporarily unavailable; retry with the same Idempotency-Key")
	default:
		s.Log.Error("request failed", "path", r.URL.Path, "err", err)
		writeError(w, http.StatusInternalServerError, "INTERNAL", "internal error")
	}
}

// temporarilyUnavailable recognises database conditions that a retry can
// get past: lock and statement timeouts, deadlocks and lost connections.
func temporarilyUnavailable(err error) bool {
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		return true
	}
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		switch pgErr.Code {
		case "55P03", // lock_not_available
			"57014", // query_canceled (statement timeout)
			"40001", // serialization_failure
			"40P01", // deadlock_detected
			"53300": // too_many_connections
			return true
		}
		return strings.HasPrefix(pgErr.Code, "08") || strings.HasPrefix(pgErr.Code, "57P") // connection, shutdown
	}
	return pgconn.SafeToRetry(err) || pgconn.Timeout(err)
}

type errorBody struct {
	Code    string              `json:"code"`
	Message string              `json:"message"`
	Fields  []domain.FieldError `json:"fields,omitempty"`
	OrderID string              `json:"order_id,omitempty"`
	Codes   []string            `json:"codes,omitempty"`
}

func writeError(w http.ResponseWriter, status int, code, message string) {
	writeJSON(w, status, map[string]any{"error": errorBody{Code: code, Message: message}})
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	b, err := json.Marshal(body)
	if err != nil {
		http.Error(w, `{"error":{"code":"INTERNAL","message":"internal error"}}`, http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	w.Write(b)
	w.Write([]byte("\n"))
}
