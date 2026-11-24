package types

import (
	"errors"
	"strings"
	"time"
)

// Scope enumerates delegated capability scopes for AI agents and principals.
type Scope string

const (
	ScopeMarketRead    Scope = "market:read"
	ScopePortfolioRead Scope = "portfolio:read"
	ScopeOrderPropose  Scope = "order:propose"
	ScopeOrderSubmit   Scope = "order:submit"
	ScopeApprovalGrant Scope = "approval:grant"
	ScopePolicyAdmin   Scope = "policy:admin"
	ScopeAuditRead     Scope = "audit:read"
)

func ValidScope(s Scope) bool {
	switch s {
	case ScopeMarketRead, ScopePortfolioRead, ScopeOrderPropose,
		ScopeOrderSubmit, ScopeApprovalGrant, ScopePolicyAdmin, ScopeAuditRead:
		return true
	default:
		return false
	}
}

type Side string

const (
	SideBuy  Side = "BUY"
	SideSell Side = "SELL"
)

type OrderType string

const (
	OrderTypeLimit  OrderType = "LIMIT"
	OrderTypeMarket OrderType = "MARKET"
	OrderTypeStop   OrderType = "STOP"
)

type TimeInForce string

const (
	TIFDay TimeInForce = "DAY"
	TIFIOC TimeInForce = "IOC"
	TIFGTC TimeInForce = "GTC"
)

type ProposalStatus string

const (
	ProposalStatusProposed ProposalStatus = "PROPOSED"
	ProposalStatusApproved ProposalStatus = "APPROVED"
	ProposalStatusExecuted ProposalStatus = "EXECUTED"
	ProposalStatusRejected ProposalStatus = "REJECTED"
	ProposalStatusExpired  ProposalStatus = "EXPIRED"
)

type DecisionResult string

const (
	DecisionAllow DecisionResult = "ALLOW"
	DecisionDeny  DecisionResult = "DENY"
)

// AgentIdentity models a delegated agent identity bound to a human/system principal.
type AgentIdentity struct {
	ID              string    `json:"id"`
	PrincipalID     string    `json:"principal_id"`
	OrgID           string    `json:"org_id"`
	TeamID          string    `json:"team_id"`
	Scopes          []Scope   `json:"scopes"`
	AllowedAccounts []string  `json:"allowed_accounts"`
	IssuedAtUTC     time.Time `json:"issued_at_utc"`
	ExpiresAtUTC    time.Time `json:"expires_at_utc"`
	Revoked         bool      `json:"revoked"`
}

func (a AgentIdentity) HasScope(target Scope) bool {
	for _, s := range a.Scopes {
		if s == target {
			return true
		}
	}
	return false
}

func (a AgentIdentity) CanAccessAccount(accountID string) bool {
	for _, acc := range a.AllowedAccounts {
		if acc == accountID {
			return true
		}
	}
	return false
}

// ResourceDescriptor identifies the target trading resource of an authorization check.
type ResourceDescriptor struct {
	AccountID       string      `json:"account"`
	Symbol          string      `json:"symbol"`
	Side            Side        `json:"side,omitempty"`
	OrderType       OrderType   `json:"order_type,omitempty"`
	Quantity        int64       `json:"quantity,omitempty"`
	LimitPriceTicks PriceTicks  `json:"limit_price_ticks,omitempty"`
	NotionalCents   MoneyCents  `json:"notional_cents"`
	TimeInForce     TimeInForce `json:"time_in_force,omitempty"`
	ProposalID      string      `json:"proposal_id,omitempty"`
}

// OrderProposal represents a proposed order with immutable cryptographically hashed terms.
type OrderProposal struct {
	ID              string         `json:"id"`
	AgentID         string         `json:"agent_id"`
	PrincipalID     string         `json:"principal_id"`
	OrgID           string         `json:"org_id"`
	TeamID          string         `json:"team_id"`
	AccountID       string         `json:"account_id"`
	Symbol          string         `json:"symbol"`
	Side            Side           `json:"side"`
	OrderType       OrderType      `json:"order_type"`
	Quantity        int64          `json:"quantity"`
	LimitPriceTicks PriceTicks     `json:"limit_price_ticks"`
	NotionalCents   MoneyCents     `json:"notional_cents"`
	TimeInForce     TimeInForce    `json:"time_in_force"`
	TermsHash       string         `json:"terms_hash"`
	Status          ProposalStatus `json:"status"`
	Rationale       string         `json:"rationale,omitempty"`
	ClientRequestID string         `json:"client_request_id,omitempty"`
	EventTimeUTC    time.Time      `json:"event_time_utc"`
	ReceivedAtUTC   time.Time      `json:"received_at_utc"`
	ExpiresAtUTC    time.Time      `json:"expires_at_utc"`
}

func (p *OrderProposal) Validate() error {
	if strings.TrimSpace(p.AccountID) == "" {
		return errors.New("account_id is required")
	}
	if strings.TrimSpace(p.Symbol) == "" {
		return errors.New("symbol is required")
	}
	if p.Side != SideBuy && p.Side != SideSell {
		return errors.New("side must be BUY or SELL")
	}
	if p.OrderType != OrderTypeLimit && p.OrderType != OrderTypeMarket && p.OrderType != OrderTypeStop {
		return errors.New("invalid order_type")
	}
	if p.Quantity <= 0 {
		return errors.New("quantity must be positive")
	}
	if p.LimitPriceTicks <= 0 {
		return errors.New("limit_price_ticks must be positive")
	}
	if p.NotionalCents <= 0 {
		return errors.New("notional_cents must be positive")
	}
	if p.TimeInForce == "" {
		p.TimeInForce = TIFDay
	}
	return nil
}

// ApprovalGrant is a signed, short-lived, one-use capability bound to a proposal hash and account.
type ApprovalGrant struct {
	ID            string     `json:"id"`
	ProposalID    string     `json:"proposal_id"`
	ProposalHash  string     `json:"proposal_hash"`
	AccountID     string     `json:"account_id"`
	PrincipalID   string     `json:"principal_id"`
	Nonce         string     `json:"nonce"`
	KeyID         string     `json:"key_id"`
	IssuedAtUTC   time.Time  `json:"issued_at_utc"`
	ExpiresAtUTC  time.Time  `json:"expires_at_utc"`
	ConsumedAtUTC *time.Time `json:"consumed_at_utc,omitempty"`
	Token         string     `json:"token"`
}

// RuleTrace records the deterministic evaluation outcome of a single policy rule.
type RuleTrace struct {
	RuleID  string         `json:"rule_id"`
	Scope   string         `json:"scope"`
	Effect  string         `json:"effect"`
	Matched bool           `json:"matched"`
	Result  DecisionResult `json:"result"`
	Reason  string         `json:"reason"`
}

// EnforcementDecision captures a versioned, auditable authorization result.
type EnforcementDecision struct {
	ID             string             `json:"id"`
	CorrelationID  string             `json:"correlation_id"`
	ActorID        string             `json:"actor_id"`
	PrincipalID    string             `json:"principal_id"`
	Action         Scope              `json:"action"`
	Resource       ResourceDescriptor `json:"resource"`
	PolicyID       string             `json:"policy_id"`
	PolicyVersion  string             `json:"policy_version"`
	PolicyHash     string             `json:"policy_hash"`
	Result         DecisionResult     `json:"result"`
	Reasons        []string           `json:"reasons"`
	Trace          []RuleTrace        `json:"trace"`
	EventTimeUTC   time.Time          `json:"event_time_utc"`
	EvaluatedAtUTC time.Time          `json:"evaluated_at_utc"`
	LatencyMicros  int64              `json:"latency_micros"`
}

// MarketQuote represents a simulated paper market quote with distinct venue and receive timestamps.
type MarketQuote struct {
	Symbol        string     `json:"symbol"`
	BidTicks      PriceTicks `json:"bid_ticks"`
	AskTicks      PriceTicks `json:"ask_ticks"`
	LastTicks     PriceTicks `json:"last_ticks"`
	VenueTimeUTC  time.Time  `json:"venue_time_utc"`
	ReceivedAtUTC time.Time  `json:"received_at_utc"`
	Source        string     `json:"source"`
	Simulated     bool       `json:"simulated"`
}

// BrokerOrderResponse represents a paper broker execution receipt.
type BrokerOrderResponse struct {
	BrokerOrderID   string     `json:"broker_order_id"`
	ProposalID      string     `json:"proposal_id"`
	AccountID       string     `json:"account_id"`
	Symbol          string     `json:"symbol"`
	Side            Side       `json:"side"`
	OrderType       OrderType  `json:"order_type"`
	Quantity        int64      `json:"quantity"`
	FillPriceTicks  PriceTicks `json:"fill_price_ticks"`
	NotionalCents   MoneyCents `json:"notional_cents"`
	Status          string     `json:"status"`
	Environment     string     `json:"environment"` // Always "PAPER_SIMULATED"
	VenueTimeUTC    time.Time  `json:"venue_time_utc"`
	ReceivedAtUTC   time.Time  `json:"received_at_utc"`
	IdempotencyKey  string     `json:"idempotency_key"`
	DuplicateReplay bool       `json:"duplicate_replay"`
}
