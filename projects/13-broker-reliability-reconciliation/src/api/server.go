package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/trading-software-projects/broker-reliability-reconciliation/src/domain"
	"github.com/trading-software-projects/broker-reliability-reconciliation/src/issues"
	"github.com/trading-software-projects/broker-reliability-reconciliation/src/metrics"
	"github.com/trading-software-projects/broker-reliability-reconciliation/src/operatorui"
	"github.com/trading-software-projects/broker-reliability-reconciliation/src/scheduler"
)

// Server exposes the reconciliation REST API, Prometheus /metrics, /healthz, and Operator UI.
type Server struct {
	runner  *scheduler.Runner
	store   *issues.Store
	metrics *metrics.Collector
	authn   Authenticator
	authz   Authorizer
	mux     *http.ServeMux
}

// NewServer creates a wired HTTP handler with authentication and authorization boundaries.
func NewServer(
	runner *scheduler.Runner,
	store *issues.Store,
	collector *metrics.Collector,
	authn Authenticator,
	authz Authorizer,
) *Server {
	if authn == nil {
		authn = NewStaticAPIKeyAuthenticator(nil)
	}
	if authz == nil {
		authz = RBACAuthorizer{}
	}
	s := &Server{
		runner:  runner,
		store:   store,
		metrics: collector,
		authn:   authn,
		authz:   authz,
		mux:     http.NewServeMux(),
	}
	s.registerRoutes()
	return s
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.mux.ServeHTTP(w, r)
}

func (s *Server) registerRoutes() {
	s.mux.HandleFunc("/healthz", s.handleHealthz)
	s.mux.HandleFunc("/metrics", s.handleMetrics)
	s.mux.HandleFunc("/v1/reconciliation/runs", s.handleRuns)
	s.mux.HandleFunc("/v1/reconciliation/runs/", s.handleRunByID)
	s.mux.HandleFunc("/v1/reconciliation/issues", s.handleIssues)
	s.mux.HandleFunc("/v1/reconciliation/issues/", s.handleIssueSubroutes)
	s.mux.HandleFunc("/v1/reconciliation/policies", s.handlePolicies)
	s.mux.HandleFunc("/", s.handleUI)
}

func (s *Server) guard(w http.ResponseWriter, r *http.Request, action string) (Principal, bool) {
	p, ok := s.authn.Authenticate(r)
	if !ok {
		writeError(w, http.StatusUnauthorized, "unauthorized")
		return Principal{}, false
	}
	if !s.authz.Authorize(p, action) {
		writeError(w, http.StatusForbidden, "forbidden for role "+string(p.Role))
		return Principal{}, false
	}
	return p, true
}

func (s *Server) handleHealthz(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{
		"status": "ok",
		"mode":   "paper-simulated",
		"time":   time.Now().UTC().Format(time.RFC3339),
	})
}

func (s *Server) handleMetrics(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(s.metrics.RenderPrometheus(s.store.ListIssues(issues.IssueFilter{}))))
}

func (s *Server) handleUI(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" && r.URL.Path != "/index.html" {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(operatorui.IndexHTML())
}

type createRunPayload struct {
	AccountID     string `json:"account_id"`
	AsOf          string `json:"asof"`
	PolicyVersion string `json:"policy_version,omitempty"`
}

func (s *Server) handleRuns(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		if _, ok := s.guard(w, r, "runs:read"); !ok {
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"runs": s.store.ListRuns(),
		})
	case http.MethodPost:
		p, ok := s.guard(w, r, "runs:create")
		if !ok {
			return
		}
		var req createRunPayload
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeError(w, http.StatusBadRequest, "invalid json payload: "+err.Error())
			return
		}
		if strings.TrimSpace(req.AccountID) == "" {
			writeError(w, http.StatusBadRequest, "account_id is required")
			return
		}
		var asof time.Time
		if strings.TrimSpace(req.AsOf) != "" {
			parsed, err := time.Parse(time.RFC3339, req.AsOf)
			if err != nil {
				writeError(w, http.StatusBadRequest, "invalid asof timestamp (expected RFC3339): "+err.Error())
				return
			}
			asof = parsed.UTC()
		} else {
			asof = time.Now().UTC()
		}

		ctx := context.WithValue(r.Context(), principalCtxKey{}, p)
		run, foundIssues, err := s.runner.Execute(ctx, scheduler.RunRequest{
			AccountID:     req.AccountID,
			AsOf:          asof,
			PolicyVersion: req.PolicyVersion,
			Trigger:       "on_demand",
		})
		if err != nil {
			status := http.StatusBadGateway
			if errors.Is(err, issues.ErrNotFound) {
				status = http.StatusNotFound
			} else if errors.Is(err, issues.ErrWriteRejected) {
				status = http.StatusServiceUnavailable
			}
			writeJSON(w, status, map[string]any{
				"error": err.Error(),
				"run":   run,
			})
			return
		}
		writeJSON(w, http.StatusCreated, map[string]any{
			"run":    run,
			"issues": foundIssues,
		})
	default:
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
	}
}

func (s *Server) handleRunByID(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	if _, ok := s.guard(w, r, "runs:read"); !ok {
		return
	}
	id := strings.TrimPrefix(r.URL.Path, "/v1/reconciliation/runs/")
	id = strings.Trim(id, "/")
	if id == "" {
		writeError(w, http.StatusBadRequest, "run id is required")
		return
	}
	run, err := s.store.GetRun(id)
	if err != nil {
		mapStoreError(w, err)
		return
	}
	runIssues := s.store.ListIssues(issues.IssueFilter{RunID: run.ID})
	writeJSON(w, http.StatusOK, map[string]any{
		"run":    run,
		"issues": runIssues,
	})
}

func (s *Server) handleIssues(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	if _, ok := s.guard(w, r, "issues:read"); !ok {
		return
	}
	q := r.URL.Query()
	filter := issues.IssueFilter{
		Status:    domain.IssueState(strings.TrimSpace(q.Get("status"))),
		AccountID: strings.TrimSpace(q.Get("account_id")),
		Kind:      domain.IssueKind(strings.TrimSpace(q.Get("kind"))),
		Severity:  domain.IssueSeverity(strings.TrimSpace(q.Get("severity"))),
		Owner:     strings.TrimSpace(q.Get("owner")),
		RunID:     strings.TrimSpace(q.Get("run_id")),
	}
	list := s.store.ListIssues(filter)
	writeJSON(w, http.StatusOK, map[string]any{
		"count":  len(list),
		"issues": list,
	})
}

type resolvePayload struct {
	Resolution string `json:"resolution"`
	Evidence   string `json:"evidence"`
	Actor      string `json:"actor,omitempty"`
}

type assignPayload struct {
	Owner string `json:"owner"`
	Actor string `json:"actor,omitempty"`
}

type statePayload struct {
	State string `json:"state"`
	Note  string `json:"note,omitempty"`
	Actor string `json:"actor,omitempty"`
}

func (s *Server) handleIssueSubroutes(w http.ResponseWriter, r *http.Request) {
	rest := strings.TrimPrefix(r.URL.Path, "/v1/reconciliation/issues/")
	parts := strings.Split(strings.Trim(rest, "/"), "/")
	if len(parts) == 0 || parts[0] == "" {
		writeError(w, http.StatusBadRequest, "issue id is required")
		return
	}
	issueID := parts[0]

	if len(parts) == 1 {
		if r.Method != http.MethodGet {
			writeError(w, http.StatusMethodNotAllowed, "method not allowed")
			return
		}
		if _, ok := s.guard(w, r, "issues:read"); !ok {
			return
		}
		iss, err := s.store.GetIssue(issueID)
		if err != nil {
			mapStoreError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, iss)
		return
	}

	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	p, ok := s.guard(w, r, "issues:mutate")
	if !ok {
		return
	}

	switch parts[1] {
	case "resolve":
		var req resolvePayload
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeError(w, http.StatusBadRequest, "invalid json payload: "+err.Error())
			return
		}
		actor := req.Actor
		if actor == "" {
			actor = p.Subject
		}
		updated, err := s.store.Resolve(issueID, req.Resolution, req.Evidence, actor)
		if err != nil {
			mapStoreError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, updated)
	case "assign":
		var req assignPayload
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeError(w, http.StatusBadRequest, "invalid json payload: "+err.Error())
			return
		}
		actor := req.Actor
		if actor == "" {
			actor = p.Subject
		}
		updated, err := s.store.AssignOwner(issueID, req.Owner, actor)
		if err != nil {
			mapStoreError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, updated)
	case "state":
		var req statePayload
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeError(w, http.StatusBadRequest, "invalid json payload: "+err.Error())
			return
		}
		actor := req.Actor
		if actor == "" {
			actor = p.Subject
		}
		updated, err := s.store.TransitionState(issueID, domain.IssueState(req.State), actor, req.Note)
		if err != nil {
			mapStoreError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, updated)
	default:
		http.NotFound(w, r)
	}
}

func (s *Server) handlePolicies(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	if _, ok := s.guard(w, r, "policies:read"); !ok {
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"policies": s.store.ListPolicies(),
	})
}

func mapStoreError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, issues.ErrNotFound):
		writeError(w, http.StatusNotFound, err.Error())
	case errors.Is(err, issues.ErrValidation):
		writeError(w, http.StatusBadRequest, err.Error())
	case errors.Is(err, issues.ErrInvalidTransition):
		writeError(w, http.StatusConflict, err.Error())
	case errors.Is(err, issues.ErrWriteRejected):
		writeError(w, http.StatusServiceUnavailable, err.Error())
	default:
		writeError(w, http.StatusInternalServerError, err.Error())
	}
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

func writeJSON(w http.ResponseWriter, status int, payload any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(payload)
}
