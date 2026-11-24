package gateway

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/trading-software-projects/agentic-trading-guardrails/src/pkg/audit"
	"github.com/trading-software-projects/agentic-trading-guardrails/src/pkg/broker"
	"github.com/trading-software-projects/agentic-trading-guardrails/src/pkg/policy"
	"github.com/trading-software-projects/agentic-trading-guardrails/src/pkg/store"
	"github.com/trading-software-projects/agentic-trading-guardrails/src/pkg/types"
)

// NewHTTPHandler mounts the full Gateway REST API, JWKS endpoint, Prometheus metrics, and Audit UI.
func (g *Gateway) NewHTTPHandler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/", g.handleUI)
	mux.HandleFunc("/healthz", g.handleHealthz)
	mux.HandleFunc("/.well-known/jwks.json", g.handleJWKS)

	mux.HandleFunc("/v1/identities/tokens", g.handleIssueToken)
	mux.HandleFunc("/v1/authorize", g.handleAuthorize)
	mux.HandleFunc("/v1/tools/call", g.handleToolCall)
	mux.HandleFunc("/v1/proposals", g.handleProposals)
	mux.HandleFunc("/v1/proposals/", g.handleProposalByID)
	mux.HandleFunc("/v1/approvals", g.handleApprovals)
	mux.HandleFunc("/v1/execute", g.handleExecute)
	mux.HandleFunc("/v1/policies", g.handlePolicies)
	mux.HandleFunc("/v1/policies/simulate", g.handlePolicySimulate)
	mux.HandleFunc("/v1/audit", g.handleAudit)
	mux.HandleFunc("/v1/audit/replay", g.handleAuditReplay)
	mux.HandleFunc("/v1/audit/verify", g.handleAuditVerify)
	mux.HandleFunc("/v1/metrics", g.handleMetrics)
	return mux
}

func (g *Gateway) handleHealthz(w http.ResponseWriter, r *http.Request) {
	active, ok := g.Policy.Active()
	version := "none"
	if ok {
		version = active.Version
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"status":         "ok",
		"environment":    broker.EnvironmentPaperSimulated,
		"policy_version": version,
		"chain_verified": g.Audit.VerifyChain() == nil,
		"utc_time":       g.Clock.Now().Format(time.RFC3339),
	})
}

func (g *Gateway) handleJWKS(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, g.Identity.JWKS())
}

func (g *Gateway) handleIssueToken(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	var req struct {
		AgentID    string        `json:"agent_id"`
		Scopes     []types.Scope `json:"scopes,omitempty"`
		TTLSeconds int64         `json:"ttl_seconds,omitempty"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON payload")
		return
	}
	ttl := time.Duration(req.TTLSeconds) * time.Second
	tok, claims, err := g.Identity.IssueToken(req.AgentID, req.Scopes, ttl)
	if err != nil {
		writeError(w, http.StatusForbidden, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"token":  tok,
		"claims": claims,
	})
}

func (g *Gateway) handleAuthorize(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	// Support both notional_cents and README's illustrative integer/string notional_usd without float64!
	var wire struct {
		AgentID       string      `json:"agent_id"`
		Action        types.Scope `json:"action"`
		CorrelationID string      `json:"correlation_id,omitempty"`
		EventTimeUTC  *time.Time  `json:"event_time_utc,omitempty"`
		Resource      struct {
			Account         string            `json:"account"`
			Symbol          string            `json:"symbol"`
			Side            types.Side        `json:"side,omitempty"`
			OrderType       types.OrderType   `json:"order_type,omitempty"`
			Quantity        int64             `json:"quantity,omitempty"`
			LimitPriceTicks types.PriceTicks  `json:"limit_price_ticks,omitempty"`
			NotionalCents   types.MoneyCents  `json:"notional_cents,omitempty"`
			NotionalUSD     json.RawMessage   `json:"notional_usd,omitempty"`
			TimeInForce     types.TimeInForce `json:"time_in_force,omitempty"`
			ProposalID      string            `json:"proposal_id,omitempty"`
		} `json:"resource"`
	}
	if err := json.NewDecoder(r.Body).Decode(&wire); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON payload")
		return
	}
	notional := wire.Resource.NotionalCents
	if notional == 0 && len(wire.Resource.NotionalUSD) > 0 {
		raw := strings.Trim(strings.TrimSpace(string(wire.Resource.NotionalUSD)), `"`)
		parsed, err := types.ParseMoneyCents(raw)
		if err != nil {
			writeError(w, http.StatusBadRequest, "invalid notional_usd decimal value")
			return
		}
		notional = parsed
	}
	if wire.Resource.OrderType == "" {
		wire.Resource.OrderType = types.OrderTypeLimit
	}
	dec, err := g.Authorize(r.Header.Get("Authorization"), AuthorizeRequest{
		AgentID:       wire.AgentID,
		Action:        wire.Action,
		CorrelationID: wire.CorrelationID,
		EventTimeUTC:  wire.EventTimeUTC,
		Resource: types.ResourceDescriptor{
			AccountID:       wire.Resource.Account,
			Symbol:          wire.Resource.Symbol,
			Side:            wire.Resource.Side,
			OrderType:       wire.Resource.OrderType,
			Quantity:        wire.Resource.Quantity,
			LimitPriceTicks: wire.Resource.LimitPriceTicks,
			NotionalCents:   notional,
			TimeInForce:     wire.Resource.TimeInForce,
			ProposalID:      wire.Resource.ProposalID,
		},
	})
	if err != nil {
		writeMappedError(w, err, nil)
		return
	}
	status := http.StatusOK
	if dec.Result != types.DecisionAllow {
		status = http.StatusForbidden
	}
	writeJSON(w, status, dec)
}

func (g *Gateway) handleToolCall(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	var req ToolCallRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON payload")
		return
	}
	resp, err := g.CallTool(r.Header.Get("Authorization"), req)
	if err != nil {
		writeMappedError(w, err, resp)
		return
	}
	writeJSON(w, http.StatusOK, resp)
}

func (g *Gateway) handleProposals(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		writeJSON(w, http.StatusOK, map[string]any{"proposals": g.Store.ListProposals()})
	case http.MethodPost:
		var req ProposeOrderRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeError(w, http.StatusBadRequest, "invalid JSON payload")
			return
		}
		res, err := g.ProposeOrder(r.Header.Get("Authorization"), req)
		if err != nil {
			writeMappedError(w, err, res)
			return
		}
		writeJSON(w, http.StatusCreated, res)
	default:
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
	}
}

func (g *Gateway) handleProposalByID(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	id := strings.TrimPrefix(r.URL.Path, "/v1/proposals/")
	p, err := g.Store.GetProposal(id)
	if err != nil {
		writeMappedError(w, err, nil)
		return
	}
	writeJSON(w, http.StatusOK, p)
}

func (g *Gateway) handleApprovals(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	var req ApproveRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON payload")
		return
	}
	grant, err := g.ApproveProposal(r.Header.Get("Authorization"), req)
	if err != nil {
		writeMappedError(w, err, nil)
		return
	}
	writeJSON(w, http.StatusCreated, grant)
}

func (g *Gateway) handleExecute(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	var req ExecuteRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON payload")
		return
	}
	res, err := g.ExecuteProposal(r.Header.Get("Authorization"), req)
	if err != nil {
		writeMappedError(w, err, res)
		return
	}
	writeJSON(w, http.StatusOK, res)
}

func (g *Gateway) handlePolicies(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		active, _ := g.Policy.Active()
		writeJSON(w, http.StatusOK, map[string]any{
			"active":   active,
			"versions": g.Policy.ListVersions(),
		})
	case http.MethodPost:
		ident, _, err := g.Identity.VerifyToken(r.Header.Get("Authorization"))
		if err != nil {
			writeError(w, http.StatusUnauthorized, err.Error())
			return
		}
		if !ident.HasScope(types.ScopePolicyAdmin) {
			writeError(w, http.StatusForbidden, "policy:admin scope required")
			return
		}
		var bundle policy.PolicyBundle
		if err := json.NewDecoder(r.Body).Decode(&bundle); err != nil {
			writeError(w, http.StatusBadRequest, "invalid policy bundle JSON")
			return
		}
		published, err := g.Policy.Publish(bundle)
		if err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		g.appendAudit(audit.Event{
			CorrelationID: ensureCorrelationID(""),
			ActorID:       ident.ID,
			PrincipalID:   ident.PrincipalID,
			Category:      audit.CategoryPolicyUpdate,
			Action:        "policy:publish",
			Outcome:       "PUBLISHED",
			PolicyVersion: published.Version,
			PolicyHash:    published.Hash,
			Details:       map[string]any{"description": published.Description, "kill_switch": published.KillSwitchActive},
		})
		writeJSON(w, http.StatusCreated, published)
	default:
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
	}
}

func (g *Gateway) handlePolicySimulate(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	ident, _, err := g.Identity.VerifyToken(r.Header.Get("Authorization"))
	if err != nil {
		writeError(w, http.StatusUnauthorized, err.Error())
		return
	}
	if !ident.HasScope(types.ScopePolicyAdmin) {
		writeError(w, http.StatusForbidden, "policy:admin scope required")
		return
	}
	var req struct {
		Candidate policy.PolicyBundle `json:"candidate"`
		Samples   []policy.EvalInput  `json:"samples"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON payload")
		return
	}
	results, err := g.Policy.Simulate(req.Candidate, req.Samples)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"comparisons": results})
}

func (g *Gateway) handleAudit(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	q := r.URL.Query()
	limit, _ := strconv.Atoi(q.Get("limit"))
	events := g.Audit.QueryEvents(audit.Query{
		ProposalID:    q.Get("proposal_id"),
		CorrelationID: q.Get("correlation_id"),
		ActorID:       q.Get("actor_id"),
		AccountID:     q.Get("account_id"),
		Category:      audit.Category(q.Get("category")),
		Limit:         limit,
	})
	writeJSON(w, http.StatusOK, map[string]any{
		"chain_verified": g.Audit.VerifyChain() == nil,
		"count":          len(events),
		"events":         events,
	})
}

func (g *Gateway) handleAuditReplay(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	q := r.URL.Query()
	summary := g.Audit.BuildReplay(q.Get("proposal_id"), q.Get("correlation_id"))
	writeJSON(w, http.StatusOK, summary)
}

func (g *Gateway) handleAuditVerify(w http.ResponseWriter, r *http.Request) {
	err := g.Audit.VerifyChain()
	if err != nil {
		writeJSON(w, http.StatusConflict, map[string]any{"verified": false, "error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"verified": true, "event_count": g.Audit.Count()})
}

func (g *Gateway) handleMetrics(w http.ResponseWriter, r *http.Request) {
	if r.URL.Query().Get("format") == "prometheus" {
		w.Header().Set("Content-Type", "text/plain; version=0.0.4")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(g.Metrics.PrometheusText()))
		return
	}
	writeJSON(w, http.StatusOK, g.Metrics.Snapshot())
}

func writeMappedError(w http.ResponseWriter, err error, detail any) {
	status := http.StatusInternalServerError
	switch {
	case errors.Is(err, ErrUnauthorized):
		status = http.StatusUnauthorized
	case errors.Is(err, ErrForbidden), errors.Is(err, ErrActorMismatch):
		status = http.StatusForbidden
	case errors.Is(err, store.ErrProposalNotFound), errors.Is(err, broker.ErrSymbolNotFound), errors.Is(err, broker.ErrAccountNotFound):
		status = http.StatusNotFound
	case errors.Is(err, ErrInvalidRequest), errors.Is(err, ErrUnknownTool):
		status = http.StatusBadRequest
	case errors.Is(err, ErrProposalExpired), errors.Is(err, ErrProposalNotPending):
		status = http.StatusConflict
	case errors.Is(err, broker.ErrBrokerDisconnected):
		status = http.StatusServiceUnavailable
	case errors.Is(err, broker.ErrBrokerRejected), errors.Is(err, broker.ErrInsufficientCash), errors.Is(err, broker.ErrInsufficientShares):
		status = http.StatusUnprocessableEntity
	}
	payload := map[string]any{"error": err.Error()}
	if detail != nil {
		payload["detail"] = detail
	}
	writeJSON(w, status, payload)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]any{"error": msg})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
