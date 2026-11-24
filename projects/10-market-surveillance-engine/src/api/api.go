// Package api is the HTTP surface: ingest, alerts, cases, evidence.
//
// Three kinds of caller, three kinds of credential:
//
//	ingest   - one bearer token; may send events and flush.
//	admin    - one bearer token; may reload rules and run the simulator.
//	analyst  - one bearer token per person; may read alerts and work cases.
//
// An analyst's name comes from their token, never from the request body,
// so the audit trail records who actually acted. A route whose credential
// is not configured is disabled, not open.
package api

import (
	"bytes"
	"crypto/subtle"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"

	"surveillance/src/cases"
	"surveillance/src/engine"
	"surveillance/src/sim"
)

//go:embed web/index.html web/app.js web/app.css
var web embed.FS

const (
	maxEventBody = 8 << 20
	maxBatch     = 5000
	maxCaseBody  = 64 << 10
)

type Config struct {
	Engine      *engine.Engine
	IngestToken string
	AdminToken  string
	// Analysts maps a bearer token to the analyst it identifies.
	Analysts map[string]string
	// EnableSim exposes the scenario simulator. It lets simulated time
	// run ahead of the wall clock, so it is off unless asked for.
	EnableSim bool
}

type server struct {
	Config
}

// New builds the handler.
func New(cfg Config) http.Handler {
	s := &server{cfg}
	mux := http.NewServeMux()

	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{"status": "ok", "mode": "simulated data only"})
	})
	mux.HandleFunc("GET /metrics", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain; version=0.0.4")
		s.Engine.WriteMetrics(w)
	})
	mux.HandleFunc("GET /{$}", s.static("web/index.html", "text/html; charset=utf-8"))
	mux.HandleFunc("GET /app.js", s.static("web/app.js", "text/javascript; charset=utf-8"))
	mux.HandleFunc("GET /app.css", s.static("web/app.css", "text/css; charset=utf-8"))

	mux.HandleFunc("POST /v1/events", s.ingest(s.postEvents))
	mux.HandleFunc("POST /v1/ingest/flush", s.ingest(s.postFlush))

	mux.HandleFunc("POST /v1/rules/reload", s.admin(s.postRulesReload))
	mux.HandleFunc("GET /v1/sim/scenarios", s.admin(s.getScenarios))
	mux.HandleFunc("POST /v1/sim/scenarios/{name}", s.admin(s.postScenario))

	mux.HandleFunc("GET /v1/whoami", s.analyst(func(w http.ResponseWriter, r *http.Request, who string) {
		writeJSON(w, http.StatusOK, map[string]string{"analyst": who})
	}))
	mux.HandleFunc("GET /v1/rules", s.analyst(func(w http.ResponseWriter, r *http.Request, _ string) {
		writeJSON(w, http.StatusOK, s.Engine.Ruleset())
	}))
	mux.HandleFunc("GET /v1/stats", s.analyst(func(w http.ResponseWriter, r *http.Request, _ string) {
		writeJSON(w, http.StatusOK, s.Engine.Stats())
	}))
	mux.HandleFunc("GET /v1/features", s.analyst(s.getFeatures))
	mux.HandleFunc("GET /v1/alerts", s.analyst(s.getAlerts))
	mux.HandleFunc("GET /v1/alerts/{id}", s.analyst(s.getAlert))
	mux.HandleFunc("POST /v1/alerts/{id}/cases", s.analyst(s.postAlertCase))
	mux.HandleFunc("GET /v1/cases", s.analyst(s.getCases))
	mux.HandleFunc("GET /v1/cases/{id}", s.analyst(s.getCase))
	mux.HandleFunc("POST /v1/cases/{id}/alerts", s.analyst(s.postCaseAlert))
	mux.HandleFunc("POST /v1/cases/{id}/triage", s.analyst(s.postTriage))
	mux.HandleFunc("POST /v1/cases/{id}/notes", s.analyst(s.postNote))
	mux.HandleFunc("POST /v1/cases/{id}/escalate", s.analyst(s.postEscalate))
	mux.HandleFunc("POST /v1/cases/{id}/resolve", s.analyst(s.postResolve))
	mux.HandleFunc("POST /v1/cases/{id}/reopen", s.analyst(s.postReopen))
	mux.HandleFunc("GET /v1/cases/{id}/evidence", s.analyst(s.getEvidence))
	mux.HandleFunc("GET /v1/audit/head", s.analyst(func(w http.ResponseWriter, r *http.Request, _ string) {
		writeJSON(w, http.StatusOK, s.Engine.AuditHead())
	}))
	mux.HandleFunc("GET /v1/audit/verify", s.analyst(func(w http.ResponseWriter, r *http.Request, _ string) {
		writeJSON(w, http.StatusOK, s.Engine.AuditVerify())
	}))
	return mux
}

func (s *server) static(name, contentType string) http.HandlerFunc {
	body, err := web.ReadFile(name)
	if err != nil {
		panic(err)
	}
	return func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Content-Type", contentType)
		h.Set("Content-Security-Policy", "default-src 'none'; script-src 'self'; style-src 'self'; connect-src 'self'; base-uri 'none'; form-action 'none'; frame-ancestors 'none'")
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "no-referrer")
		h.Set("Cache-Control", "no-store")
		_, _ = w.Write(body)
	}
}

// ---- responses ----

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

type apiError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

func writeErr(w http.ResponseWriter, status int, code, msg string) {
	writeJSON(w, status, map[string]apiError{"error": {Code: code, Message: msg}})
}

// fail maps a domain error onto a status code.
func fail(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, engine.ErrNotFound), errors.Is(err, cases.ErrNotFound):
		writeErr(w, http.StatusNotFound, "NOT_FOUND", err.Error())
	case errors.Is(err, engine.ErrConflict), errors.Is(err, cases.ErrConflict):
		writeErr(w, http.StatusConflict, "CONFLICT", err.Error())
	case errors.Is(err, cases.ErrInvalid):
		writeErr(w, http.StatusUnprocessableEntity, "INVALID_REQUEST", err.Error())
	case errors.Is(err, engine.ErrJournal), errors.Is(err, cases.ErrUnavailable):
		w.Header().Set("Retry-After", "1")
		writeErr(w, http.StatusServiceUnavailable, "STORAGE_UNAVAILABLE", err.Error())
	default:
		writeErr(w, http.StatusInternalServerError, "INTERNAL", err.Error())
	}
}

// ---- authentication ----

func bearer(r *http.Request) string {
	const prefix = "Bearer "
	h := r.Header.Get("Authorization")
	if len(h) > len(prefix) && strings.EqualFold(h[:len(prefix)], prefix) {
		return h[len(prefix):]
	}
	return ""
}

func same(a, b string) bool { return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1 }

func (s *server) token(want, role string, next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if want == "" {
			writeErr(w, http.StatusForbidden, "ROUTE_DISABLED", "no "+role+" token is configured, so this route is disabled")
			return
		}
		if !same(bearer(r), want) {
			w.Header().Set("WWW-Authenticate", "Bearer")
			writeErr(w, http.StatusUnauthorized, "UNAUTHORIZED", "a valid "+role+" bearer token is required")
			return
		}
		next(w, r)
	}
}

func (s *server) ingest(next http.HandlerFunc) http.HandlerFunc {
	return s.token(s.IngestToken, "ingest", next)
}
func (s *server) admin(next http.HandlerFunc) http.HandlerFunc {
	return s.token(s.AdminToken, "admin", next)
}

func (s *server) analyst(next func(http.ResponseWriter, *http.Request, string)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if len(s.Analysts) == 0 {
			writeErr(w, http.StatusForbidden, "ROUTE_DISABLED", "no analyst tokens are configured, so this route is disabled")
			return
		}
		got, who := bearer(r), ""
		// Compare against every token so timing does not reveal which matched.
		for tok, name := range s.Analysts {
			if got != "" && same(got, tok) {
				who = name
			}
		}
		if who == "" {
			w.Header().Set("WWW-Authenticate", "Bearer")
			writeErr(w, http.StatusUnauthorized, "UNAUTHORIZED", "a valid analyst bearer token is required")
			return
		}
		next(w, r, who)
	}
}

// readBody reads a request body of at most limit bytes. A body that is
// cut short (the client went away) is refused whole: nothing in it is used.
func readBody(w http.ResponseWriter, r *http.Request, limit int64) ([]byte, bool) {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, limit))
	if err == nil {
		return body, true
	}
	var tooBig *http.MaxBytesError
	if errors.As(err, &tooBig) {
		writeErr(w, http.StatusRequestEntityTooLarge, "BODY_TOO_LARGE", fmt.Sprintf("request body exceeds %d bytes", limit))
	} else {
		writeErr(w, http.StatusBadRequest, "MALFORMED_REQUEST", "request body could not be read in full")
	}
	return nil, false
}

// ---- ingest ----

func (s *server) postEvents(w http.ResponseWriter, r *http.Request) {
	body, ok := readBody(w, r, maxEventBody)
	if !ok {
		return
	}
	body = bytes.TrimSpace(body)
	var items []json.RawMessage
	switch {
	case len(body) > 0 && body[0] == '[':
		if err := json.Unmarshal(body, &items); err != nil {
			writeErr(w, http.StatusBadRequest, "MALFORMED_REQUEST", "body is not a valid JSON array: "+err.Error())
			return
		}
	case len(body) > 0 && body[0] == '{':
		if !json.Valid(body) {
			writeErr(w, http.StatusBadRequest, "MALFORMED_REQUEST", "body is not valid JSON")
			return
		}
		items = []json.RawMessage{body}
	default:
		writeErr(w, http.StatusBadRequest, "MALFORMED_REQUEST", "body must be one event object or an array of them")
		return
	}
	if len(items) > maxBatch {
		writeErr(w, http.StatusRequestEntityTooLarge, "BATCH_TOO_LARGE", fmt.Sprintf("at most %d events per request", maxBatch))
		return
	}
	res, err := s.Engine.IngestJSON(items)
	if err != nil {
		if errors.Is(err, engine.ErrJournal) {
			// Say how far the batch got. Resending all of it is safe:
			// what was already stored comes back as duplicates.
			w.Header().Set("Retry-After", "1")
			writeJSON(w, http.StatusServiceUnavailable, map[string]any{
				"error":  apiError{Code: "STORAGE_UNAVAILABLE", Message: err.Error() + "; resend the whole batch, stored events are recognised as duplicates"},
				"result": res,
			})
			return
		}
		fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, res)
}

func (s *server) postFlush(w http.ResponseWriter, r *http.Request) {
	res, err := s.Engine.Flush()
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, res)
}

// ---- admin ----

func (s *server) postRulesReload(w http.ResponseWriter, r *http.Request) {
	info, err := s.Engine.ReloadRules()
	if err != nil {
		if errors.Is(err, engine.ErrJournal) {
			fail(w, err)
			return
		}
		writeErr(w, http.StatusUnprocessableEntity, "INVALID_RULESET", err.Error()+" (the previous ruleset stays in force)")
		return
	}
	writeJSON(w, http.StatusOK, info)
}

func (s *server) simEnabled(w http.ResponseWriter) bool {
	if !s.EnableSim {
		writeErr(w, http.StatusForbidden, "SIM_DISABLED", "the simulator is disabled; start the server with -sim to enable it")
		return false
	}
	return true
}

func (s *server) getScenarios(w http.ResponseWriter, r *http.Request) {
	if s.simEnabled(w) {
		writeJSON(w, http.StatusOK, map[string]any{"scenarios": sim.Names})
	}
}

func (s *server) postScenario(w http.ResponseWriter, r *http.Request) {
	if !s.simEnabled(w) {
		return
	}
	var req struct {
		Seed       uint64 `json:"seed"`
		Symbol     string `json:"symbol"`
		Borderline bool   `json:"borderline"`
		NoFlush    bool   `json:"no_flush"`
	}
	if !decode(w, r, &req, true) {
		return
	}
	start := s.Engine.NextSimStart()
	sc, err := sim.Generate(r.PathValue("name"), sim.Params{
		Seed: req.Seed, Start: start, Symbol: strings.ToUpper(req.Symbol), Borderline: req.Borderline,
		Tag: fmt.Sprintf("SIM%d-%d", req.Seed, start.Unix()),
	})
	if err != nil {
		writeErr(w, http.StatusNotFound, "UNKNOWN_SCENARIO", err.Error())
		return
	}
	res, err := s.Engine.Ingest(sc.Events, true)
	if err != nil {
		fail(w, err)
		return
	}
	out := map[string]any{"simulated": true, "scenario": sc, "events": len(sc.Events), "ingest": res}
	if !req.NoFlush {
		fl, err := s.Engine.Flush()
		if err != nil {
			fail(w, err)
			return
		}
		out["flush"] = fl
	}
	writeJSON(w, http.StatusOK, out)
}

// ---- analyst ----

// decode reads a small JSON body strictly. With optional set, an empty
// body is accepted and leaves v untouched.
func decode(w http.ResponseWriter, r *http.Request, v any, optional bool) bool {
	body, ok := readBody(w, r, maxCaseBody)
	if !ok {
		return false
	}
	if len(bytes.TrimSpace(body)) == 0 {
		if optional {
			return true
		}
		writeErr(w, http.StatusBadRequest, "MALFORMED_REQUEST", "a JSON body is required")
		return false
	}
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		writeErr(w, http.StatusBadRequest, "MALFORMED_REQUEST", err.Error())
		return false
	}
	if dec.More() {
		writeErr(w, http.StatusBadRequest, "MALFORMED_REQUEST", "trailing data after JSON body")
		return false
	}
	return true
}

func upper(r *http.Request, key string) string {
	return strings.ToUpper(strings.TrimSpace(r.URL.Query().Get(key)))
}

func (s *server) getAlerts(w http.ResponseWriter, r *http.Request, _ string) {
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	if limit <= 0 || limit > 1000 {
		limit = 200
	}
	alerts := s.Engine.Alerts(engine.AlertFilter{
		Status: upper(r, "status"), Symbol: upper(r, "symbol"), RuleID: upper(r, "rule_id"),
		Account: upper(r, "account"), Severity: upper(r, "severity"), Limit: limit,
	})
	writeJSON(w, http.StatusOK, map[string]any{"alerts": alerts, "count": len(alerts)})
}

func (s *server) getAlert(w http.ResponseWriter, r *http.Request, _ string) {
	d, err := s.Engine.Alert(r.PathValue("id"))
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, d)
}

func (s *server) postAlertCase(w http.ResponseWriter, r *http.Request, who string) {
	var req struct {
		Title string `json:"title"`
		Note  string `json:"note"`
	}
	if !decode(w, r, &req, true) {
		return
	}
	c, err := s.Engine.OpenCase(who, r.PathValue("id"), req.Title, req.Note)
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, c)
}

func (s *server) getCases(w http.ResponseWriter, r *http.Request, _ string) {
	list := s.Engine.Cases(upper(r, "status"))
	writeJSON(w, http.StatusOK, map[string]any{"cases": list, "count": len(list)})
}

func (s *server) getCase(w http.ResponseWriter, r *http.Request, _ string) {
	c, err := s.Engine.Case(r.PathValue("id"))
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, c)
}

func (s *server) postCaseAlert(w http.ResponseWriter, r *http.Request, who string) {
	var req struct {
		AlertID string `json:"alert_id"`
	}
	if !decode(w, r, &req, false) {
		return
	}
	c, err := s.Engine.AttachAlert(who, r.PathValue("id"), req.AlertID)
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, c)
}

func (s *server) postTriage(w http.ResponseWriter, r *http.Request, who string) {
	var req struct {
		Priority string `json:"priority"`
		Assignee string `json:"assignee"`
	}
	if !decode(w, r, &req, false) {
		return
	}
	if req.Assignee == "" {
		req.Assignee = who
	}
	c, err := s.Engine.Triage(who, r.PathValue("id"), strings.ToUpper(req.Priority), req.Assignee)
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, c)
}

func (s *server) postNote(w http.ResponseWriter, r *http.Request, who string) {
	var req struct {
		Text string `json:"text"`
	}
	if !decode(w, r, &req, false) {
		return
	}
	c, err := s.Engine.AddNote(who, r.PathValue("id"), req.Text)
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, c)
}

func (s *server) postEscalate(w http.ResponseWriter, r *http.Request, who string) {
	var req struct {
		Reason string `json:"reason"`
	}
	if !decode(w, r, &req, false) {
		return
	}
	c, err := s.Engine.Escalate(who, r.PathValue("id"), req.Reason)
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, c)
}

func (s *server) postResolve(w http.ResponseWriter, r *http.Request, who string) {
	var req struct {
		Disposition string `json:"disposition"`
		Summary     string `json:"summary"`
	}
	if !decode(w, r, &req, false) {
		return
	}
	c, err := s.Engine.Resolve(who, r.PathValue("id"), strings.ToUpper(req.Disposition), req.Summary)
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, c)
}

func (s *server) postReopen(w http.ResponseWriter, r *http.Request, who string) {
	var req struct {
		Reason string `json:"reason"`
	}
	if !decode(w, r, &req, false) {
		return
	}
	c, err := s.Engine.Reopen(who, r.PathValue("id"), req.Reason)
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, c)
}

// getEvidence exports the bundle. It is a GET that leaves a trace: every
// export is itself recorded in the audit trail.
func (s *server) getEvidence(w http.ResponseWriter, r *http.Request, who string) {
	b, err := s.Engine.Evidence(who, r.PathValue("id"))
	if err != nil {
		fail(w, err)
		return
	}
	w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="%s-evidence.json"`, b.Case.ID))
	writeJSON(w, http.StatusOK, b)
}

func (s *server) getFeatures(w http.ResponseWriter, r *http.Request, _ string) {
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	fws := s.Engine.Features(upper(r, "account"), upper(r, "symbol"), limit)
	writeJSON(w, http.StatusOK, map[string]any{"windows": fws, "count": len(fws)})
}

// MinTokenLen is the shortest bearer token the server will accept as
// configuration. Shorter ones are refused at startup.
const MinTokenLen = 16

// ParseAnalysts reads "name:token,name:token" (the SURV_ANALYST_TOKENS
// format) into the token -> name table.
func ParseAnalysts(s string) (map[string]string, error) {
	out := map[string]string{}
	names := map[string]bool{}
	for _, pair := range strings.Split(s, ",") {
		pair = strings.TrimSpace(pair)
		if pair == "" {
			continue
		}
		name, tok, ok := strings.Cut(pair, ":")
		name, tok = strings.TrimSpace(name), strings.TrimSpace(tok)
		if !ok || name == "" {
			return nil, errors.New("analyst tokens must be written as name:token,name:token")
		}
		if len(tok) < MinTokenLen {
			return nil, fmt.Errorf("the token for analyst %q is shorter than %d characters", name, MinTokenLen)
		}
		if names[name] {
			return nil, fmt.Errorf("analyst %q is listed twice", name)
		}
		if _, dup := out[tok]; dup {
			return nil, errors.New("two analysts share one token; each analyst needs their own")
		}
		names[name] = true
		out[tok] = name
	}
	return out, nil
}
