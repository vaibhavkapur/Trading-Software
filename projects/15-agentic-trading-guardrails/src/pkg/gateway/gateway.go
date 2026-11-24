package gateway

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/trading-software-projects/agentic-trading-guardrails/src/pkg/approval"
	"github.com/trading-software-projects/agentic-trading-guardrails/src/pkg/audit"
	"github.com/trading-software-projects/agentic-trading-guardrails/src/pkg/broker"
	"github.com/trading-software-projects/agentic-trading-guardrails/src/pkg/identity"
	"github.com/trading-software-projects/agentic-trading-guardrails/src/pkg/metrics"
	"github.com/trading-software-projects/agentic-trading-guardrails/src/pkg/policy"
	"github.com/trading-software-projects/agentic-trading-guardrails/src/pkg/store"
	"github.com/trading-software-projects/agentic-trading-guardrails/src/pkg/types"
)

const DefaultProposalTTL = 5 * time.Minute

var (
	ErrUnauthorized       = errors.New("unauthorized")
	ErrForbidden          = errors.New("forbidden by policy")
	ErrProposalExpired    = errors.New("order proposal has expired")
	ErrProposalNotPending = errors.New("order proposal is already executed or rejected")
	ErrActorMismatch      = errors.New("authenticated agent does not match proposal actor")
	ErrUnknownTool        = errors.New("unknown tool name")
	ErrInvalidRequest     = errors.New("invalid request")
)

// Gateway is the single Policy Enforcement Point (PEP) connecting identity, policy, approval,
// isolated paper broker, immutable audit, durable store, and telemetry.
type Gateway struct {
	Clock    types.Clock
	Identity *identity.Service
	Policy   *policy.Engine
	Approval *approval.Service
	Broker   *broker.PaperAdapter
	Audit    *audit.Logger
	Store    *store.Store
	Metrics  *metrics.Collector

	propSeqMu sync.Mutex
	propSeq   uint64
}

type Config struct {
	Clock          types.Clock
	AuditLogPath   string
	StateStorePath string
	InitialPolicy  *policy.PolicyBundle
}

func New(cfg Config) (*Gateway, error) {
	clk := cfg.Clock
	if clk == nil {
		clk = types.SystemClock{}
	}
	idSvc, err := identity.NewService(clk)
	if err != nil {
		return nil, err
	}
	polEngine := policy.NewEngine(clk)
	bundle := policy.DefaultPolicyBundle()
	if cfg.InitialPolicy != nil {
		bundle = *cfg.InitialPolicy
	}
	if _, err := polEngine.Publish(bundle); err != nil {
		return nil, fmt.Errorf("publish initial policy: %w", err)
	}

	aprSvc, err := approval.NewService(clk)
	if err != nil {
		return nil, err
	}
	brk, err := broker.NewPaperAdapter(clk)
	if err != nil {
		return nil, err
	}
	aud, err := audit.NewLogger(clk, cfg.AuditLogPath)
	if err != nil {
		return nil, err
	}
	st, err := store.NewStore(cfg.StateStorePath)
	if err != nil {
		return nil, err
	}

	// Restore consumed grant IDs from durable state so replay protection survives restarts.
	aprSvc.RestoreConsumed(st.ConsumedGrants())
	aprSvc.SetConsumedHook(st.RecordConsumedGrant)

	gw := &Gateway{
		Clock:    clk,
		Identity: idSvc,
		Policy:   polEngine,
		Approval: aprSvc,
		Broker:   brk,
		Audit:    aud,
		Store:    st,
		Metrics:  metrics.NewCollector(),
	}
	return gw, nil
}

// AuthorizeRequest represents a standalone PDP authorization query.
type AuthorizeRequest struct {
	AgentID       string                   `json:"agent_id,omitempty"`
	Action        types.Scope              `json:"action"`
	Resource      types.ResourceDescriptor `json:"resource"`
	CorrelationID string                   `json:"correlation_id,omitempty"`
	EventTimeUTC  *time.Time               `json:"event_time_utc,omitempty"`
}

// Authorize authenticates the caller via bearer JWT (never trusting raw prompt or unverified agent_id)
// and evaluates the active policy bundle fail-closed.
func (g *Gateway) Authorize(bearerToken string, req AuthorizeRequest) (types.EnforcementDecision, error) {
	corrID := ensureCorrelationID(req.CorrelationID)
	ident, _, err := g.Identity.VerifyToken(bearerToken)
	if err != nil {
		g.appendAudit(audit.Event{
			CorrelationID: corrID,
			ActorID:       req.AgentID,
			Category:      audit.CategoryAuth,
			Action:        string(req.Action),
			Outcome:       "DENY",
			Details:       map[string]any{"error": err.Error()},
		})
		return types.EnforcementDecision{}, fmt.Errorf("%w: %v", ErrUnauthorized, err)
	}
	if req.AgentID != "" && req.AgentID != ident.ID {
		g.appendAudit(audit.Event{
			CorrelationID: corrID,
			ActorID:       ident.ID,
			PrincipalID:   ident.PrincipalID,
			Category:      audit.CategorySecurityViolation,
			Action:        "identity_spoofing_attempt",
			Outcome:       "DENY",
			Details:       map[string]any{"claimed_agent_id": req.AgentID, "authenticated_agent_id": ident.ID},
		})
		return types.EnforcementDecision{}, fmt.Errorf("%w: claimed agent_id %q does not match token subject %q", ErrUnauthorized, req.AgentID, ident.ID)
	}

	var eventTime time.Time
	if req.EventTimeUTC != nil {
		eventTime = req.EventTimeUTC.UTC()
	}

	dailySpent := g.Store.GetDailyNotional(req.Resource.AccountID, g.Clock.Now())
	dec := g.Policy.Evaluate(policy.EvalInput{
		CorrelationID:           corrID,
		Identity:                ident,
		Action:                  req.Action,
		Resource:                req.Resource,
		DailySpentNotionalCents: dailySpent,
		EventTimeUTC:            eventTime,
		HasValidApproval:        false,
	})

	_, audErr := g.appendAudit(audit.Event{
		CorrelationID: corrID,
		ProposalID:    req.Resource.ProposalID,
		ActorID:       ident.ID,
		PrincipalID:   ident.PrincipalID,
		AccountID:     req.Resource.AccountID,
		Category:      audit.CategoryPolicyDecision,
		Action:        string(req.Action),
		Outcome:       string(dec.Result),
		PolicyVersion: dec.PolicyVersion,
		PolicyHash:    dec.PolicyHash,
		EventTimeUTC:  dec.EventTimeUTC,
		Details: map[string]any{
			"decision_id": dec.ID,
			"reasons":     dec.Reasons,
			"symbol":      req.Resource.Symbol,
			"notional":    req.Resource.NotionalCents.FormatUSD(),
		},
	})
	g.Metrics.RecordDecision(dec, audErr == nil)
	return dec, nil
}

// ProposeOrderRequest creates an immutable hashed order proposal.
type ProposeOrderRequest struct {
	ProposalID      string            `json:"proposal_id,omitempty"`
	AccountID       string            `json:"account_id"`
	Symbol          string            `json:"symbol"`
	Side            types.Side        `json:"side"`
	OrderType       types.OrderType   `json:"order_type"`
	Quantity        int64             `json:"quantity"`
	LimitPriceTicks types.PriceTicks  `json:"limit_price_ticks"`
	TimeInForce     types.TimeInForce `json:"time_in_force,omitempty"`
	Rationale       string            `json:"rationale,omitempty"`
	ClientRequestID string            `json:"client_request_id,omitempty"`
	CorrelationID   string            `json:"correlation_id,omitempty"`
	EventTimeUTC    *time.Time        `json:"event_time_utc,omitempty"`
}

type ProposeOrderResult struct {
	Proposal        types.OrderProposal       `json:"proposal"`
	Decision        types.EnforcementDecision `json:"decision"`
	DuplicateReplay bool                      `json:"duplicate_replay"`
}

// ProposeOrder authenticates the agent, checks market freshness and policy fail-closed,
// computes fixed-point notional and canonical terms_hash, and persists the proposal.
func (g *Gateway) ProposeOrder(bearerToken string, req ProposeOrderRequest) (ProposeOrderResult, error) {
	corrID := ensureCorrelationID(req.CorrelationID)
	ident, _, err := g.Identity.VerifyToken(bearerToken)
	if err != nil {
		g.appendAudit(audit.Event{
			CorrelationID: corrID,
			Category:      audit.CategoryAuth,
			Action:        string(types.ScopeOrderPropose),
			Outcome:       "DENY",
			Details:       map[string]any{"error": err.Error()},
		})
		return ProposeOrderResult{}, fmt.Errorf("%w: %v", ErrUnauthorized, err)
	}

	// Idempotent duplicate proposal request handling
	if req.ClientRequestID != "" {
		if existing, ok := g.Store.FindByClientRequestID(req.ClientRequestID); ok {
			return ProposeOrderResult{Proposal: existing, DuplicateReplay: true}, nil
		}
	}

	notionalCents, err := types.ComputeNotionalCents(req.Quantity, req.LimitPriceTicks)
	if err != nil {
		return ProposeOrderResult{}, fmt.Errorf("%w: %v", ErrInvalidRequest, err)
	}
	if req.TimeInForce == "" {
		req.TimeInForce = types.TIFDay
	}
	sym := strings.ToUpper(strings.TrimSpace(req.Symbol))

	// Check paper market feed availability & freshness (fail-closed if broker disconnected)
	quoteSnap, qErr := g.Broker.GetQuote(sym)
	var quoteVenueTime *time.Time
	if qErr != nil {
		if errors.Is(qErr, broker.ErrBrokerDisconnected) {
			g.appendAudit(audit.Event{
				CorrelationID: corrID,
				ActorID:       ident.ID,
				PrincipalID:   ident.PrincipalID,
				AccountID:     req.AccountID,
				Category:      audit.CategoryProposal,
				Action:        string(types.ScopeOrderPropose),
				Outcome:       "DENY",
				Details:       map[string]any{"reason": qErr.Error()},
			})
			return ProposeOrderResult{}, qErr
		}
	} else {
		vt := quoteSnap.Quote.VenueTimeUTC
		quoteVenueTime = &vt
	}

	now := g.Clock.Now()
	eventTime := now
	if req.EventTimeUTC != nil {
		eventTime = req.EventTimeUTC.UTC()
	}

	res := types.ResourceDescriptor{
		AccountID:       req.AccountID,
		Symbol:          sym,
		Side:            req.Side,
		OrderType:       req.OrderType,
		Quantity:        req.Quantity,
		LimitPriceTicks: req.LimitPriceTicks,
		NotionalCents:   notionalCents,
		TimeInForce:     req.TimeInForce,
	}
	dailySpent := g.Store.GetDailyNotional(req.AccountID, now)
	dec := g.Policy.Evaluate(policy.EvalInput{
		CorrelationID:           corrID,
		Identity:                ident,
		Action:                  types.ScopeOrderPropose,
		Resource:                res,
		DailySpentNotionalCents: dailySpent,
		EventTimeUTC:            eventTime,
		QuoteVenueTimeUTC:       quoteVenueTime,
	})

	propID := req.ProposalID
	if propID == "" {
		propID = g.nextProposalID()
	}
	res.ProposalID = propID

	_, audErr := g.appendAudit(audit.Event{
		CorrelationID: corrID,
		ProposalID:    propID,
		ActorID:       ident.ID,
		PrincipalID:   ident.PrincipalID,
		AccountID:     req.AccountID,
		Category:      audit.CategoryPolicyDecision,
		Action:        string(types.ScopeOrderPropose),
		Outcome:       string(dec.Result),
		PolicyVersion: dec.PolicyVersion,
		PolicyHash:    dec.PolicyHash,
		EventTimeUTC:  eventTime,
		Details: map[string]any{
			"decision_id": dec.ID,
			"reasons":     dec.Reasons,
			"symbol":      sym,
			"notional":    notionalCents.FormatUSD(),
		},
	})
	g.Metrics.RecordDecision(dec, audErr == nil)

	if dec.Result != types.DecisionAllow {
		return ProposeOrderResult{Decision: dec}, fmt.Errorf("%w: %s", ErrForbidden, strings.Join(dec.Reasons, "; "))
	}

	proposal := types.OrderProposal{
		ID:              propID,
		AgentID:         ident.ID,
		PrincipalID:     ident.PrincipalID,
		OrgID:           ident.OrgID,
		TeamID:          ident.TeamID,
		AccountID:       req.AccountID,
		Symbol:          sym,
		Side:            req.Side,
		OrderType:       req.OrderType,
		Quantity:        req.Quantity,
		LimitPriceTicks: req.LimitPriceTicks,
		NotionalCents:   notionalCents,
		TimeInForce:     req.TimeInForce,
		Status:          types.ProposalStatusProposed,
		Rationale:       req.Rationale,
		ClientRequestID: req.ClientRequestID,
		EventTimeUTC:    eventTime,
		ReceivedAtUTC:   now,
		ExpiresAtUTC:    now.Add(DefaultProposalTTL),
	}
	if err := proposal.Validate(); err != nil {
		return ProposeOrderResult{}, fmt.Errorf("%w: %v", ErrInvalidRequest, err)
	}
	proposal.TermsHash = approval.ComputeProposalHash(proposal)

	if err := g.Store.SaveProposal(proposal); err != nil {
		return ProposeOrderResult{}, err
	}

	g.appendAudit(audit.Event{
		CorrelationID: corrID,
		ProposalID:    proposal.ID,
		ActorID:       ident.ID,
		PrincipalID:   ident.PrincipalID,
		AccountID:     proposal.AccountID,
		Category:      audit.CategoryProposal,
		Action:        string(types.ScopeOrderPropose),
		Outcome:       "PROPOSED",
		PolicyVersion: dec.PolicyVersion,
		PolicyHash:    dec.PolicyHash,
		EventTimeUTC:  proposal.EventTimeUTC,
		Details: map[string]any{
			"symbol":            proposal.Symbol,
			"side":              proposal.Side,
			"order_type":        proposal.OrderType,
			"quantity":          proposal.Quantity,
			"limit_price_ticks": proposal.LimitPriceTicks,
			"notional":          proposal.NotionalCents.FormatUSD(),
			"terms_hash":        proposal.TermsHash,
		},
	})

	return ProposeOrderResult{Proposal: proposal, Decision: dec}, nil
}

// ApproveRequest requests a signed, short-lived, one-use approval capability bound to proposal_hash.
type ApproveRequest struct {
	ProposalID    string `json:"proposal_id"`
	ProposalHash  string `json:"proposal_hash"`
	TTLSeconds    int64  `json:"ttl_seconds,omitempty"`
	CorrelationID string `json:"correlation_id,omitempty"`
}

// ApproveProposal verifies the principal's identity and scope, checks canonical proposal_hash,
// and mints a signed one-use approval capability.
func (g *Gateway) ApproveProposal(principalToken string, req ApproveRequest) (types.ApprovalGrant, error) {
	corrID := ensureCorrelationID(req.CorrelationID)
	ident, _, err := g.Identity.VerifyToken(principalToken)
	if err != nil {
		g.appendAudit(audit.Event{
			CorrelationID: corrID,
			ProposalID:    req.ProposalID,
			Category:      audit.CategoryAuth,
			Action:        string(types.ScopeApprovalGrant),
			Outcome:       "DENY",
			Details:       map[string]any{"error": err.Error()},
		})
		return types.ApprovalGrant{}, fmt.Errorf("%w: %v", ErrUnauthorized, err)
	}
	if !ident.HasScope(types.ScopeApprovalGrant) {
		g.appendAudit(audit.Event{
			CorrelationID: corrID,
			ProposalID:    req.ProposalID,
			ActorID:       ident.ID,
			PrincipalID:   ident.PrincipalID,
			Category:      audit.CategorySecurityViolation,
			Action:        string(types.ScopeApprovalGrant),
			Outcome:       "DENY",
			Details:       map[string]any{"reason": "caller lacks approval:grant scope"},
		})
		return types.ApprovalGrant{}, fmt.Errorf("%w: identity %q lacks approval:grant scope", ErrForbidden, ident.ID)
	}

	proposal, err := g.Store.GetProposal(req.ProposalID)
	if err != nil {
		return types.ApprovalGrant{}, err
	}
	if g.Clock.Now().After(proposal.ExpiresAtUTC) {
		return types.ApprovalGrant{}, ErrProposalExpired
	}
	if proposal.Status == types.ProposalStatusExecuted || proposal.Status == types.ProposalStatusRejected {
		return types.ApprovalGrant{}, ErrProposalNotPending
	}

	ttl := time.Duration(req.TTLSeconds) * time.Second
	grant, err := g.Approval.IssueGrant(proposal, req.ProposalHash, ident.PrincipalID, ttl)
	if err != nil {
		g.appendAudit(audit.Event{
			CorrelationID: corrID,
			ProposalID:    proposal.ID,
			ActorID:       ident.ID,
			PrincipalID:   ident.PrincipalID,
			AccountID:     proposal.AccountID,
			Category:      audit.CategorySecurityViolation,
			Action:        string(types.ScopeApprovalGrant),
			Outcome:       "DENY",
			Details:       map[string]any{"error": err.Error()},
		})
		return types.ApprovalGrant{}, fmt.Errorf("%w: %v", ErrForbidden, err)
	}

	proposal.Status = types.ProposalStatusApproved
	_ = g.Store.UpdateProposal(proposal)

	g.appendAudit(audit.Event{
		CorrelationID: corrID,
		ProposalID:    proposal.ID,
		ActorID:       ident.ID,
		PrincipalID:   ident.PrincipalID,
		AccountID:     proposal.AccountID,
		Category:      audit.CategoryApproval,
		Action:        string(types.ScopeApprovalGrant),
		Outcome:       "APPROVED",
		Details: map[string]any{
			"grant_id":       grant.ID,
			"proposal_hash":  grant.ProposalHash,
			"expires_at_utc": grant.ExpiresAtUTC.Format(time.RFC3339),
			"approval_grant": grant.Token, // Will be automatically redacted by audit logger
		},
	})

	return grant, nil
}

// ExecuteRequest submits an approved order proposal through the single PEP to the isolated paper broker.
type ExecuteRequest struct {
	ProposalID     string     `json:"proposal_id"`
	ApprovalGrant  string     `json:"approval_grant"`
	AccountID      string     `json:"account_id,omitempty"`
	IdempotencyKey string     `json:"idempotency_key,omitempty"`
	CorrelationID  string     `json:"correlation_id,omitempty"`
	EventTimeUTC   *time.Time `json:"event_time_utc,omitempty"`
}

type ExecuteResult struct {
	Proposal types.OrderProposal       `json:"proposal"`
	Decision types.EnforcementDecision `json:"decision"`
	Grant    types.ApprovalGrant       `json:"grant"`
	Receipt  types.BrokerOrderResponse `json:"receipt"`
}

// ExecuteProposal is the single fail-closed execution path to the isolated paper broker.
// It verifies identity, market freshness, one-use approval capability, proposal hash integrity,
// AND re-evaluates deterministic policy rules BEFORE touching the paper broker.
func (g *Gateway) ExecuteProposal(agentToken string, req ExecuteRequest) (ExecuteResult, error) {
	corrID := ensureCorrelationID(req.CorrelationID)
	ident, _, err := g.Identity.VerifyToken(agentToken)
	if err != nil {
		g.appendAudit(audit.Event{
			CorrelationID: corrID,
			ProposalID:    req.ProposalID,
			Category:      audit.CategoryAuth,
			Action:        string(types.ScopeOrderSubmit),
			Outcome:       "DENY",
			Details:       map[string]any{"error": err.Error()},
		})
		return ExecuteResult{}, fmt.Errorf("%w: %v", ErrUnauthorized, err)
	}

	proposal, err := g.Store.GetProposal(req.ProposalID)
	if err != nil {
		return ExecuteResult{}, err
	}
	if proposal.AgentID != ident.ID {
		g.appendAudit(audit.Event{
			CorrelationID: corrID,
			ProposalID:    proposal.ID,
			ActorID:       ident.ID,
			PrincipalID:   ident.PrincipalID,
			AccountID:     proposal.AccountID,
			Category:      audit.CategorySecurityViolation,
			Action:        string(types.ScopeOrderSubmit),
			Outcome:       "DENY",
			Details:       map[string]any{"reason": ErrActorMismatch.Error(), "proposal_agent": proposal.AgentID},
		})
		return ExecuteResult{}, fmt.Errorf("%w: proposal owned by %q", ErrActorMismatch, proposal.AgentID)
	}

	now := g.Clock.Now()
	if now.After(proposal.ExpiresAtUTC) {
		proposal.Status = types.ProposalStatusExpired
		_ = g.Store.UpdateProposal(proposal)
		g.appendAudit(audit.Event{
			CorrelationID: corrID,
			ProposalID:    proposal.ID,
			ActorID:       ident.ID,
			PrincipalID:   ident.PrincipalID,
			AccountID:     proposal.AccountID,
			Category:      audit.CategoryExecution,
			Action:        string(types.ScopeOrderSubmit),
			Outcome:       "DENY",
			Details:       map[string]any{"reason": ErrProposalExpired.Error()},
		})
		return ExecuteResult{}, ErrProposalExpired
	}

	// Determine target account (if explicitly passed in execute request, enforce exact match!)
	targetAccount := req.AccountID
	if targetAccount == "" {
		targetAccount = proposal.AccountID
	}

	// Verify signed one-use approval grant BEFORE executing.
	// Note: if approval fails, record violation metric & audit event and fail closed.
	grant, violation, aprErr := g.Approval.VerifyAndConsume(req.ApprovalGrant, proposal, targetAccount)
	if aprErr != nil {
		g.Metrics.RecordApprovalViolation(string(violation))
		g.appendAudit(audit.Event{
			CorrelationID: corrID,
			ProposalID:    proposal.ID,
			ActorID:       ident.ID,
			PrincipalID:   ident.PrincipalID,
			AccountID:     targetAccount,
			Category:      audit.CategorySecurityViolation,
			Action:        string(types.ScopeOrderSubmit),
			Outcome:       "DENY",
			Details: map[string]any{
				"violation":      string(violation),
				"error":          aprErr.Error(),
				"approval_grant": req.ApprovalGrant, // Automatically redacted
			},
		})
		return ExecuteResult{}, fmt.Errorf("%w: %v", ErrForbidden, aprErr)
	}

	// Check broker connectivity & market data freshness (fail-closed)
	quoteSnap, qErr := g.Broker.GetQuote(proposal.Symbol)
	if qErr != nil {
		g.appendAudit(audit.Event{
			CorrelationID: corrID,
			ProposalID:    proposal.ID,
			ActorID:       ident.ID,
			PrincipalID:   ident.PrincipalID,
			AccountID:     proposal.AccountID,
			Category:      audit.CategoryExecution,
			Action:        string(types.ScopeOrderSubmit),
			Outcome:       "DENY",
			Details:       map[string]any{"reason": qErr.Error()},
		})
		return ExecuteResult{}, qErr
	}
	quoteVenueTime := quoteSnap.Quote.VenueTimeUTC

	eventTime := now
	if req.EventTimeUTC != nil {
		eventTime = req.EventTimeUTC.UTC()
	}

	// Fail-closed deterministic policy re-evaluation at execution time!
	// Human approval NEVER substitutes for deterministic policy checks.
	dailySpent := g.Store.GetDailyNotional(proposal.AccountID, now)
	dec := g.Policy.Evaluate(policy.EvalInput{
		CorrelationID: corrID,
		Identity:      ident,
		Action:        types.ScopeOrderSubmit,
		Resource: types.ResourceDescriptor{
			AccountID:       targetAccount,
			Symbol:          proposal.Symbol,
			Side:            proposal.Side,
			OrderType:       proposal.OrderType,
			Quantity:        proposal.Quantity,
			LimitPriceTicks: proposal.LimitPriceTicks,
			NotionalCents:   proposal.NotionalCents,
			TimeInForce:     proposal.TimeInForce,
			ProposalID:      proposal.ID,
		},
		DailySpentNotionalCents: dailySpent,
		EventTimeUTC:            eventTime,
		QuoteVenueTimeUTC:       &quoteVenueTime,
		HasValidApproval:        true,
	})

	_, audErr := g.appendAudit(audit.Event{
		CorrelationID: corrID,
		ProposalID:    proposal.ID,
		ActorID:       ident.ID,
		PrincipalID:   ident.PrincipalID,
		AccountID:     proposal.AccountID,
		Category:      audit.CategoryPolicyDecision,
		Action:        string(types.ScopeOrderSubmit),
		Outcome:       string(dec.Result),
		PolicyVersion: dec.PolicyVersion,
		PolicyHash:    dec.PolicyHash,
		EventTimeUTC:  eventTime,
		Details: map[string]any{
			"decision_id": dec.ID,
			"grant_id":    grant.ID,
			"reasons":     dec.Reasons,
		},
	})
	g.Metrics.RecordDecision(dec, audErr == nil)

	if dec.Result != types.DecisionAllow {
		return ExecuteResult{Proposal: proposal, Decision: dec, Grant: grant},
			fmt.Errorf("%w: %s", ErrForbidden, strings.Join(dec.Reasons, "; "))
	}

	// Issue sealed PEP ExecutionPermit and route to isolated Paper Broker
	idemKey := req.IdempotencyKey
	if idemKey == "" {
		idemKey = "idem-" + proposal.ID
	}
	permit := broker.IssueExecutionPermit(dec.ID, dec.PolicyVersion, grant.ID, corrID, idemKey)
	g.Metrics.RecordBrokerWrite(true)

	receipt, bErr := g.Broker.SubmitOrder(permit, proposal)
	if bErr != nil {
		g.appendAudit(audit.Event{
			CorrelationID: corrID,
			ProposalID:    proposal.ID,
			ActorID:       ident.ID,
			PrincipalID:   ident.PrincipalID,
			AccountID:     proposal.AccountID,
			Category:      audit.CategoryBrokerResponse,
			Action:        string(types.ScopeOrderSubmit),
			Outcome:       "BROKER_ERROR",
			PolicyVersion: dec.PolicyVersion,
			PolicyHash:    dec.PolicyHash,
			Details:       map[string]any{"error": bErr.Error()},
		})
		return ExecuteResult{Proposal: proposal, Decision: dec, Grant: grant}, bErr
	}

	// Update daily spent notional and proposal state
	if !receipt.DuplicateReplay {
		_ = g.Store.AddDailyNotional(proposal.AccountID, now, receipt.NotionalCents)
	}
	proposal.Status = types.ProposalStatusExecuted
	_ = g.Store.UpdateProposal(proposal)

	g.appendAudit(audit.Event{
		CorrelationID: corrID,
		ProposalID:    proposal.ID,
		ActorID:       ident.ID,
		PrincipalID:   ident.PrincipalID,
		AccountID:     proposal.AccountID,
		Category:      audit.CategoryBrokerResponse,
		Action:        string(types.ScopeOrderSubmit),
		Outcome:       receipt.Status,
		PolicyVersion: dec.PolicyVersion,
		PolicyHash:    dec.PolicyHash,
		EventTimeUTC:  receipt.VenueTimeUTC,
		Details: map[string]any{
			"broker_order_id":  receipt.BrokerOrderID,
			"environment":      receipt.Environment,
			"symbol":           receipt.Symbol,
			"quantity":         receipt.Quantity,
			"fill_price_ticks": receipt.FillPriceTicks,
			"notional":         receipt.NotionalCents.FormatUSD(),
			"duplicate_replay": receipt.DuplicateReplay,
		},
	})

	return ExecuteResult{
		Proposal: proposal,
		Decision: dec,
		Grant:    grant,
		Receipt:  receipt,
	}, nil
}

// ToolCallRequest represents an MCP-style tool invocation routed through the Tool Permission Gateway.
type ToolCallRequest struct {
	Tool          string         `json:"tool"` // "market.quote", "portfolio.get", "order.propose", "order.submit"
	Arguments     map[string]any `json:"arguments"`
	CorrelationID string         `json:"correlation_id,omitempty"`
}

// ToolCallResponse wraps tool output with explicit untrusted-data tagging and prompt-injection telemetry.
type ToolCallResponse struct {
	Tool                    string                    `json:"tool"`
	CorrelationID           string                    `json:"correlation_id"`
	DataTrust               string                    `json:"data_trust"` // "UNTRUSTED_EXTERNAL" for market/external feeds
	PromptInjectionDetected bool                      `json:"prompt_injection_detected"`
	InjectionWarnings       []string                  `json:"injection_warnings,omitempty"`
	Decision                types.EnforcementDecision `json:"decision"`
	Data                    any                       `json:"data"`
}

// CallTool enforces per-tool entitlements, executes the tool, and tags/scans external outputs
// so prompt injection in tool output can NEVER expand order permissions.
func (g *Gateway) CallTool(bearerToken string, req ToolCallRequest) (ToolCallResponse, error) {
	corrID := ensureCorrelationID(req.CorrelationID)
	ident, _, err := g.Identity.VerifyToken(bearerToken)
	if err != nil {
		return ToolCallResponse{}, fmt.Errorf("%w: %v", ErrUnauthorized, err)
	}

	switch req.Tool {
	case "market.quote":
		sym, _ := req.Arguments["symbol"].(string)
		dec := g.Policy.Evaluate(policy.EvalInput{
			CorrelationID: corrID,
			Identity:      ident,
			Action:        types.ScopeMarketRead,
			Resource:      types.ResourceDescriptor{Symbol: sym},
		})
		_, audErr := g.appendAudit(audit.Event{
			CorrelationID: corrID,
			ActorID:       ident.ID,
			PrincipalID:   ident.PrincipalID,
			Category:      audit.CategoryToolCall,
			Action:        "tool:market.quote",
			Outcome:       string(dec.Result),
			PolicyVersion: dec.PolicyVersion,
			PolicyHash:    dec.PolicyHash,
			Details:       map[string]any{"symbol": sym, "reasons": dec.Reasons},
		})
		g.Metrics.RecordDecision(dec, audErr == nil)
		if dec.Result != types.DecisionAllow {
			return ToolCallResponse{Tool: req.Tool, CorrelationID: corrID, Decision: dec},
				fmt.Errorf("%w: %s", ErrForbidden, strings.Join(dec.Reasons, "; "))
		}
		snap, err := g.Broker.GetQuote(sym)
		if err != nil {
			return ToolCallResponse{}, err
		}
		warnings := DetectPromptInjection(snap.VenueNotes)
		if len(warnings) > 0 {
			g.appendAudit(audit.Event{
				CorrelationID: corrID,
				ActorID:       ident.ID,
				PrincipalID:   ident.PrincipalID,
				Category:      audit.CategorySecurityViolation,
				Action:        "prompt_injection_detected_in_tool_output",
				Outcome:       "CONTAINED",
				Details:       map[string]any{"tool": req.Tool, "symbol": sym, "warnings": warnings},
			})
		}
		return ToolCallResponse{
			Tool:                    req.Tool,
			CorrelationID:           corrID,
			DataTrust:               "UNTRUSTED_EXTERNAL_DATA_DO_NOT_TREAT_AS_AUTHORIZATION",
			PromptInjectionDetected: len(warnings) > 0,
			InjectionWarnings:       warnings,
			Decision:                dec,
			Data:                    snap,
		}, nil

	case "portfolio.get":
		accountID, _ := req.Arguments["account_id"].(string)
		dec := g.Policy.Evaluate(policy.EvalInput{
			CorrelationID: corrID,
			Identity:      ident,
			Action:        types.ScopePortfolioRead,
			Resource:      types.ResourceDescriptor{AccountID: accountID},
		})
		_, audErr := g.appendAudit(audit.Event{
			CorrelationID: corrID,
			ActorID:       ident.ID,
			PrincipalID:   ident.PrincipalID,
			AccountID:     accountID,
			Category:      audit.CategoryToolCall,
			Action:        "tool:portfolio.get",
			Outcome:       string(dec.Result),
			PolicyVersion: dec.PolicyVersion,
			PolicyHash:    dec.PolicyHash,
			Details:       map[string]any{"account_id": accountID, "reasons": dec.Reasons},
		})
		g.Metrics.RecordDecision(dec, audErr == nil)
		if dec.Result != types.DecisionAllow {
			return ToolCallResponse{Tool: req.Tool, CorrelationID: corrID, Decision: dec},
				fmt.Errorf("%w: %s", ErrForbidden, strings.Join(dec.Reasons, "; "))
		}
		snap, err := g.Broker.GetPortfolio(accountID)
		if err != nil {
			return ToolCallResponse{}, err
		}
		return ToolCallResponse{
			Tool:          req.Tool,
			CorrelationID: corrID,
			DataTrust:     "INTERNAL_PAPER_STATE",
			Decision:      dec,
			Data:          snap,
		}, nil

	default:
		return ToolCallResponse{}, fmt.Errorf("%w: %s (write operations must use /v1/proposals and /v1/execute)", ErrUnknownTool, req.Tool)
	}
}

var injectionSignatures = []string{
	"ignore previous instructions",
	"ignore all prior instructions",
	"system override",
	"grant order:submit",
	"bypass approval",
	"bypass policy",
	"disable guardrails",
	"execute immediately without approval",
	"reveal api key",
	"print broker secret",
}

// DetectPromptInjection scans untrusted tool output strings for adversarial prompt-injection directives.
func DetectPromptInjection(text string) []string {
	if strings.TrimSpace(text) == "" {
		return nil
	}
	lower := strings.ToLower(text)
	var hits []string
	for _, sig := range injectionSignatures {
		if strings.Contains(lower, sig) {
			hits = append(hits, fmt.Sprintf("matched adversarial pattern %q", sig))
		}
	}
	return hits
}

func (g *Gateway) appendAudit(ev audit.Event) (audit.Event, error) {
	stored, err := g.Audit.Append(ev)
	if err == nil {
		g.Metrics.RecordAuditEvent()
	}
	return stored, err
}

func (g *Gateway) nextProposalID() string {
	g.propSeqMu.Lock()
	defer g.propSeqMu.Unlock()
	g.propSeq++
	return fmt.Sprintf("p-%d", g.propSeq)
}

func ensureCorrelationID(id string) string {
	if strings.TrimSpace(id) != "" {
		return id
	}
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return "corr-" + hex.EncodeToString(b)
}
