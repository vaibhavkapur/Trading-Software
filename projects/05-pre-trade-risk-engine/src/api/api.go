// Package api is the HTTP/JSON transport. It contains no risk logic: it
// parses, authenticates the privileged routes, calls the core and maps
// errors to status codes.
package api

import (
	"bytes"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"

	"riskengine/src/core"
	"riskengine/src/domain"
	"riskengine/src/fixed"
	"riskengine/src/market"
	"riskengine/src/metrics"
	"riskengine/src/notify"
	"riskengine/src/policy"
)

const maxBody = 64 << 10

type Server struct {
	Engine   *core.Engine
	Market   *market.Store
	Metrics  *metrics.Registry
	Clock    core.Clock
	Notifier *notify.Notifier

	// AdminToken guards kill switches, approvals, policy reload and audit
	// verification; FeedToken guards quote ingest. A route whose token is
	// not configured is disabled, never open.
	AdminToken string
	FeedToken  string

	PublicKey   string
	ReloadFn    func() (*policy.Policy, error)
	AuditHead   func() (uint64, string)
	AuditVerify func() (uint64, string, error)
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	admin := func(h http.HandlerFunc) http.HandlerFunc { return s.auth(s.AdminToken, h) }

	mux.HandleFunc("POST /v1/risk/evaluate", s.evaluate)
	mux.HandleFunc("GET /v1/risk/decisions/{order_id}", s.decision)
	mux.HandleFunc("POST /v1/risk/reservations/{order_id}/commit", s.commit)
	mux.HandleFunc("POST /v1/risk/reservations/{order_id}/release", s.release)
	mux.HandleFunc("POST /v1/risk/reservations/{order_id}/fills", s.fill)
	mux.HandleFunc("POST /v1/risk/reservations/{order_id}/approve", admin(s.approve))
	mux.HandleFunc("GET /v1/risk/accounts/{account_id}", s.account)
	mux.HandleFunc("POST /v1/risk/reconcile", s.reconcile)

	mux.HandleFunc("GET /v1/risk/kill-switches", s.listKill)
	mux.HandleFunc("POST /v1/risk/kill-switches/global/{action}", admin(s.kill(domain.ScopeGlobal)))
	mux.HandleFunc("POST /v1/risk/kill-switches/accounts/{target}/{action}", admin(s.kill(domain.ScopeAccount)))
	mux.HandleFunc("POST /v1/risk/kill-switches/symbols/{target}/{action}", admin(s.kill(domain.ScopeSymbol)))

	mux.HandleFunc("POST /v1/market/quotes", s.auth(s.FeedToken, s.quote))
	mux.HandleFunc("POST /v1/policy/reload", admin(s.reload))
	mux.HandleFunc("GET /v1/audit/head", s.auditHead)
	mux.HandleFunc("GET /v1/audit/verify", admin(s.auditVerify))

	mux.HandleFunc("GET /metrics", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain; version=0.0.4")
		s.Metrics.Write(w)
	})
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		if s.Engine.Policy() == nil {
			writeJSON(w, http.StatusServiceUnavailable, map[string]string{"status": "no policy loaded"})
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok", "policy_version": s.Engine.Policy().Version})
	})
	return mux
}

func (s *Server) auth(token string, h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if token == "" {
			writeErr(w, http.StatusForbidden, "this route is disabled: no token is configured for it")
			return
		}
		got, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		if !ok || subtle.ConstantTimeCompare([]byte(got), []byte(token)) != 1 {
			writeErr(w, http.StatusUnauthorized, "missing or wrong bearer token")
			return
		}
		h(w, r)
	}
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]string{"error": msg})
}

// decode reads a strict JSON body: unknown fields and trailing data are
// errors. An empty body is allowed when optional is true.
func decode(w http.ResponseWriter, r *http.Request, v any, optional bool) ([]byte, error) {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxBody))
	if err != nil {
		return body, err
	}
	if optional && len(bytes.TrimSpace(body)) == 0 {
		return body, nil
	}
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return body, err
	}
	if dec.More() {
		return body, errors.New("unexpected data after JSON object")
	}
	return body, nil
}

func status(err error) int {
	switch {
	case errors.Is(err, core.ErrNotFound):
		return http.StatusNotFound
	case errors.Is(err, core.ErrInvalid):
		return http.StatusBadRequest
	case errors.Is(err, core.ErrAudit):
		return http.StatusServiceUnavailable
	case errors.Is(err, core.ErrNoReservation), errors.Is(err, core.ErrReleased), errors.Is(err, core.ErrExpired),
		errors.Is(err, core.ErrPendingReview), errors.Is(err, core.ErrNotPending), errors.Is(err, core.ErrKillSwitch):
		return http.StatusConflict
	}
	return http.StatusInternalServerError
}

func errCode(err error) string {
	for e, c := range map[error]string{
		core.ErrNotFound: "ORDER_NOT_FOUND", core.ErrNoReservation: "NO_RESERVATION", core.ErrReleased: "RESERVATION_RELEASED",
		core.ErrExpired: "RESERVATION_EXPIRED", core.ErrPendingReview: "PENDING_REVIEW", core.ErrNotPending: "NOT_PENDING_REVIEW",
		core.ErrKillSwitch: "KILL_SWITCH_ACTIVE", core.ErrInvalid: "INVALID_ARGUMENT", core.ErrAudit: domain.AuditUnavailable,
	} {
		if errors.Is(err, e) {
			return c
		}
	}
	return "INTERNAL"
}

// reservationReply answers a reservation transition. On failure the body
// still carries the reservation's current state where one exists.
func reservationReply(w http.ResponseWriter, r domain.RiskReservation, err error) {
	if err != nil {
		body := map[string]any{"error": err.Error(), "code": errCode(err)}
		if r.OrderID != "" {
			body["reservation"] = r
		}
		writeJSON(w, status(err), body)
		return
	}
	writeJSON(w, http.StatusOK, r)
}

func (s *Server) evaluate(w http.ResponseWriter, r *http.Request) {
	var req domain.RiskRequest
	body, err := decode(w, r, &req, false)
	if err != nil {
		// Unparseable input is still a decision: REJECT, with evidence.
		sum := sha256.Sum256(body)
		s.Engine.RecordMalformed(hex.EncodeToString(sum[:]), err.Error())
		version := ""
		if p := s.Engine.Policy(); p != nil {
			version = p.Version
		}
		writeJSON(w, http.StatusBadRequest, map[string]any{"decision": domain.Reject,
			"reason_codes": []string{domain.MalformedRequest}, "policy_version": version, "error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, s.Engine.Evaluate(req))
}

func (s *Server) decision(w http.ResponseWriter, r *http.Request) {
	d, ok := s.Engine.Decision(r.PathValue("order_id"))
	if !ok {
		writeErr(w, http.StatusNotFound, "no recorded decision for that order_id")
		return
	}
	writeJSON(w, http.StatusOK, d)
}

func (s *Server) commit(w http.ResponseWriter, r *http.Request) {
	res, err := s.Engine.Commit(r.PathValue("order_id"))
	reservationReply(w, res, err)
}

func (s *Server) release(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Reason string `json:"reason"`
	}
	if _, err := decode(w, r, &in, true); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	res, err := s.Engine.Release(r.PathValue("order_id"), in.Reason)
	reservationReply(w, res, err)
}

func (s *Server) approve(w http.ResponseWriter, r *http.Request) {
	var in struct {
		ApprovedBy string `json:"approved_by"`
	}
	if _, err := decode(w, r, &in, false); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	res, err := s.Engine.Approve(r.PathValue("order_id"), in.ApprovedBy)
	reservationReply(w, res, err)
}

func (s *Server) fill(w http.ResponseWriter, r *http.Request) {
	var in struct {
		FillID string        `json:"fill_id"`
		Qty    int64         `json:"qty"`
		Price  fixed.Decimal `json:"price"`
	}
	if _, err := decode(w, r, &in, false); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	res, err := s.Engine.Fill(r.PathValue("order_id"), in.FillID, in.Qty, in.Price)
	if err != nil {
		writeJSON(w, status(err), map[string]string{"error": err.Error(), "code": errCode(err)})
		return
	}
	writeJSON(w, http.StatusOK, res)
}

func (s *Server) account(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.Engine.AccountState(r.PathValue("account_id")))
}

func (s *Server) reconcile(w http.ResponseWriter, r *http.Request) {
	var in struct {
		AccountID string           `json:"account_id"`
		Positions map[string]int64 `json:"positions"`
	}
	if _, err := decode(w, r, &in, false); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	rec, err := s.Engine.Reconcile(in.AccountID, in.Positions)
	if err != nil {
		writeJSON(w, status(err), map[string]string{"error": err.Error(), "code": errCode(err)})
		return
	}
	writeJSON(w, http.StatusOK, rec)
}

func (s *Server) kill(scope domain.KillScope) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		action := r.PathValue("action")
		if action != "activate" && action != "deactivate" {
			writeErr(w, http.StatusNotFound, "action must be activate or deactivate")
			return
		}
		var in struct {
			By     string `json:"activated_by"`
			Reason string `json:"reason"`
		}
		if _, err := decode(w, r, &in, false); err != nil {
			writeErr(w, http.StatusBadRequest, err.Error())
			return
		}
		ks, seal, durable, err := s.Engine.SetKillSwitch(scope, r.PathValue("target"), action == "activate", in.By, in.Reason)
		body := map[string]any{"kill_switch": ks, "durable": durable}
		switch {
		case err == nil:
			body["audit_seq"], body["record_hash"] = seal.Seq, seal.Hash
			writeJSON(w, http.StatusOK, body)
		case ks.Enabled && errors.Is(err, core.ErrAudit):
			// Active in memory but not recorded: tell the operator loudly.
			body["warning"] = "kill switch is ACTIVE but could not be written to the audit log; it will not survive a restart"
			writeJSON(w, http.StatusOK, body)
		default:
			writeJSON(w, status(err), map[string]string{"error": err.Error(), "code": errCode(err)})
		}
	}
}

func (s *Server) listKill(w http.ResponseWriter, r *http.Request) {
	out := map[string]any{"active": s.Engine.KillSwitches()}
	if s.Notifier != nil {
		out["propagation"] = s.Notifier.Status()
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) quote(w http.ResponseWriter, r *http.Request) {
	var q market.Quote
	if _, err := decode(w, r, &q, false); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := s.Market.Update(q, s.Clock.Now()); err != nil {
		reason := map[error]string{market.ErrInvalidQuote: "invalid", market.ErrOutOfOrder: "out_of_order", market.ErrFutureQuote: "future"}[err]
		s.Metrics.Inc("risk_quotes_rejected_total", "reason", reason)
		writeErr(w, http.StatusUnprocessableEntity, err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) reload(w http.ResponseWriter, r *http.Request) {
	p, err := s.ReloadFn()
	if err != nil {
		// The previous policy stays in force.
		writeErr(w, http.StatusUnprocessableEntity, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"policy_version": p.Version, "policy_hash": p.Hash})
}

func (s *Server) auditHead(w http.ResponseWriter, r *http.Request) {
	seq, hash := s.AuditHead()
	writeJSON(w, http.StatusOK, map[string]any{"seq": seq, "head_hash": hash, "public_key": s.PublicKey})
}

func (s *Server) auditVerify(w http.ResponseWriter, r *http.Request) {
	n, head, err := s.AuditVerify()
	if err != nil {
		writeJSON(w, http.StatusConflict, map[string]any{"valid": false, "records_verified": n, "error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"valid": true, "records": n, "head_hash": head})
}
