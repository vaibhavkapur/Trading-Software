package policy

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/trading-software-projects/agentic-trading-guardrails/src/pkg/types"
)

var (
	ErrNoActivePolicy     = errors.New("no active policy bundle configured (fail-closed)")
	ErrInvalidPolicy      = errors.New("invalid policy bundle")
	ErrDuplicateVersion   = errors.New("policy version already exists")
	ErrPolicyVersionFound = errors.New("policy version not found")
)

// TimeWindowUTC defines an allowed trading window in UTC minutes from midnight.
type TimeWindowUTC struct {
	StartMinuteUTC  int            `json:"start_minute_utc"` // e.g., 13*60+30 = 810 (13:30 UTC)
	EndMinuteUTC    int            `json:"end_minute_utc"`   // e.g., 20*60 = 1200 (20:00 UTC)
	AllowedWeekdays []time.Weekday `json:"allowed_weekdays"` // empty = every day
}

func (w *TimeWindowUTC) Contains(t time.Time) bool {
	if w == nil {
		return true
	}
	utc := t.UTC()
	if len(w.AllowedWeekdays) > 0 {
		matchedDay := false
		for _, d := range w.AllowedWeekdays {
			if utc.Weekday() == d {
				matchedDay = true
				break
			}
		}
		if !matchedDay {
			return false
		}
	}
	mins := utc.Hour()*60 + utc.Minute()
	return mins >= w.StartMinuteUTC && mins <= w.EndMinuteUTC
}

// ConstraintSet represents composable limits at Organization, Team, or Agent level.
// More specific scopes can only NARROW constraints (smaller notional/qty, subset of symbols/order types).
type ConstraintSet struct {
	AllowedSymbols            []string          `json:"allowed_symbols,omitempty"`
	DeniedSymbols             []string          `json:"denied_symbols,omitempty"`
	AllowedOrderTypes         []types.OrderType `json:"allowed_order_types,omitempty"`
	AllowedSides              []types.Side      `json:"allowed_sides,omitempty"`
	AllowedAccounts           []string          `json:"allowed_accounts,omitempty"`
	MaxOrderNotionalCents     types.MoneyCents  `json:"max_order_notional_cents,omitempty"`
	MaxDailyNotionalCents     types.MoneyCents  `json:"max_daily_notional_cents,omitempty"`
	MaxOrderQuantity          int64             `json:"max_order_quantity,omitempty"`
	TradingWindow             *TimeWindowUTC    `json:"trading_window,omitempty"`
	RequireApprovalAboveCents *types.MoneyCents `json:"require_approval_above_cents,omitempty"`
}

// PolicyRule represents an individual versioned policy-as-code rule (allow or explicit deny).
type PolicyRule struct {
	ID         string      `json:"id"`
	Scope      types.Scope `json:"scope"`
	Effect     string      `json:"effect"`     // "ALLOW" or "DENY"
	Expression string      `json:"expression"` // declarative predicate e.g. `symbol == "GME"` or `notional_cents > 100000`
	Reason     string      `json:"reason"`
}

// PolicyBundle is an immutable, versioned policy snapshot.
type PolicyBundle struct {
	ID                  string                   `json:"id"`
	Version             string                   `json:"version"`
	Description         string                   `json:"description"`
	KillSwitchActive    bool                     `json:"kill_switch_active"`
	MaxQuoteAgeSeconds  int64                    `json:"max_quote_age_seconds"`
	MaxClockSkewSeconds int64                    `json:"max_clock_skew_seconds"`
	Org                 ConstraintSet            `json:"org"`
	Teams               map[string]ConstraintSet `json:"teams,omitempty"`
	Agents              map[string]ConstraintSet `json:"agents,omitempty"`
	Rules               []PolicyRule             `json:"rules"`
	CreatedAtUTC        time.Time                `json:"created_at_utc"`
	Hash                string                   `json:"hash"`
}

// ComputeHash computes a deterministic SHA-256 fingerprint of the bundle's executable policy content.
func (b PolicyBundle) ComputeHash() string {
	cp := b
	cp.Hash = ""
	cp.CreatedAtUTC = time.Time{}
	raw, _ := json.Marshal(cp)
	sum := sha256.Sum256(raw)
	return "sha256:" + hex.EncodeToString(sum[:])
}

func (b PolicyBundle) Validate() error {
	if strings.TrimSpace(b.ID) == "" || strings.TrimSpace(b.Version) == "" {
		return fmt.Errorf("%w: id and version are required", ErrInvalidPolicy)
	}
	if b.MaxQuoteAgeSeconds < 0 || b.MaxClockSkewSeconds < 0 {
		return fmt.Errorf("%w: timing bounds must be non-negative", ErrInvalidPolicy)
	}
	for _, r := range b.Rules {
		if strings.TrimSpace(r.ID) == "" {
			return fmt.Errorf("%w: rule id required", ErrInvalidPolicy)
		}
		if r.Effect != "ALLOW" && r.Effect != "DENY" {
			return fmt.Errorf("%w: rule %s effect must be ALLOW or DENY", ErrInvalidPolicy, r.ID)
		}
		if !types.ValidScope(r.Scope) {
			return fmt.Errorf("%w: rule %s has invalid scope %q", ErrInvalidPolicy, r.ID, r.Scope)
		}
		if err := validateExpression(r.Expression); err != nil {
			return fmt.Errorf("%w: rule %s invalid expression: %v", ErrInvalidPolicy, r.ID, err)
		}
	}
	return nil
}

// EvalInput holds all inputs needed for a deterministic, fail-closed policy decision.
type EvalInput struct {
	CorrelationID           string
	Identity                types.AgentIdentity
	Action                  types.Scope
	Resource                types.ResourceDescriptor
	DailySpentNotionalCents types.MoneyCents
	EventTimeUTC            time.Time
	QuoteVenueTimeUTC       *time.Time
	HasValidApproval        bool
}

// SimulationComparison captures active vs candidate policy outcomes for shadow testing.
type SimulationComparison struct {
	RequestIndex      int                       `json:"request_index"`
	ActiveResult      types.DecisionResult      `json:"active_result"`
	CandidateResult   types.DecisionResult      `json:"candidate_result"`
	Changed           bool                      `json:"changed"`
	ActiveReasons     []string                  `json:"active_reasons"`
	CandidateReasons  []string                  `json:"candidate_reasons"`
	CandidateDecision types.EnforcementDecision `json:"candidate_decision"`
}

// Engine is the Policy Decision Point (PDP) storing immutable policy versions and evaluating fail-closed.
type Engine struct {
	mu            sync.RWMutex
	clock         types.Clock
	activeVersion string
	versions      map[string]PolicyBundle
	versionOrder  []string
	decisionSeq   uint64
}

func NewEngine(clock types.Clock) *Engine {
	if clock == nil {
		clock = types.SystemClock{}
	}
	return &Engine{
		clock:    clock,
		versions: make(map[string]PolicyBundle),
	}
}

// Publish validates, hashes, stores, and activates a new immutable policy version.
func (e *Engine) Publish(bundle PolicyBundle) (PolicyBundle, error) {
	if err := bundle.Validate(); err != nil {
		return PolicyBundle{}, err
	}
	if bundle.CreatedAtUTC.IsZero() {
		bundle.CreatedAtUTC = e.clock.Now()
	} else {
		bundle.CreatedAtUTC = bundle.CreatedAtUTC.UTC()
	}
	bundle.Hash = bundle.ComputeHash()

	e.mu.Lock()
	defer e.mu.Unlock()
	if _, exists := e.versions[bundle.Version]; exists {
		return PolicyBundle{}, fmt.Errorf("%w: %s", ErrDuplicateVersion, bundle.Version)
	}
	e.versions[bundle.Version] = bundle
	e.versionOrder = append(e.versionOrder, bundle.Version)
	e.activeVersion = bundle.Version
	return bundle, nil
}

func (e *Engine) Active() (PolicyBundle, bool) {
	e.mu.RLock()
	defer e.mu.RUnlock()
	b, ok := e.versions[e.activeVersion]
	return b, ok
}

func (e *Engine) GetVersion(version string) (PolicyBundle, bool) {
	e.mu.RLock()
	defer e.mu.RUnlock()
	b, ok := e.versions[version]
	return b, ok
}

func (e *Engine) ListVersions() []PolicyBundle {
	e.mu.RLock()
	defer e.mu.RUnlock()
	out := make([]PolicyBundle, 0, len(e.versionOrder))
	for _, v := range e.versionOrder {
		out = append(out, e.versions[v])
	}
	return out
}

// Evaluate evaluates EvalInput against the active policy bundle (fail-closed if none active).
func (e *Engine) Evaluate(in EvalInput) types.EnforcementDecision {
	e.mu.RLock()
	bundle, hasActive := e.versions[e.activeVersion]
	e.mu.RUnlock()
	if !hasActive {
		now := e.clock.Now()
		return types.EnforcementDecision{
			ID:             e.nextDecisionID(),
			CorrelationID:  in.CorrelationID,
			ActorID:        in.Identity.ID,
			PrincipalID:    in.Identity.PrincipalID,
			Action:         in.Action,
			Resource:       in.Resource,
			PolicyID:       "none",
			PolicyVersion:  "none",
			PolicyHash:     "none",
			Result:         types.DecisionDeny,
			Reasons:        []string{ErrNoActivePolicy.Error()},
			EventTimeUTC:   normalizeEventTime(in.EventTimeUTC, now),
			EvaluatedAtUTC: now,
		}
	}
	return e.EvaluateWithBundle(bundle, in)
}

// EvaluateWithBundle evaluates EvalInput deterministically against a specific PolicyBundle.
func (e *Engine) EvaluateWithBundle(bundle PolicyBundle, in EvalInput) types.EnforcementDecision {
	start := time.Now()
	now := e.clock.Now()
	eventTime := normalizeEventTime(in.EventTimeUTC, now)

	decision := types.EnforcementDecision{
		ID:             e.nextDecisionID(),
		CorrelationID:  in.CorrelationID,
		ActorID:        in.Identity.ID,
		PrincipalID:    in.Identity.PrincipalID,
		Action:         in.Action,
		Resource:       in.Resource,
		PolicyID:       bundle.ID,
		PolicyVersion:  bundle.Version,
		PolicyHash:     bundle.Hash,
		Result:         types.DecisionDeny, // FAIL-CLOSED DEFAULT
		Reasons:        []string{},
		Trace:          []types.RuleTrace{},
		EventTimeUTC:   eventTime,
		EvaluatedAtUTC: now,
	}

	deny := func(ruleID, scope, reason string) {
		decision.Reasons = append(decision.Reasons, reason)
		decision.Trace = append(decision.Trace, types.RuleTrace{
			RuleID:  ruleID,
			Scope:   scope,
			Effect:  "DENY",
			Matched: true,
			Result:  types.DecisionDeny,
			Reason:  reason,
		})
	}
	pass := func(ruleID, scope, reason string) {
		decision.Trace = append(decision.Trace, types.RuleTrace{
			RuleID:  ruleID,
			Scope:   scope,
			Effect:  "ALLOW",
			Matched: true,
			Result:  types.DecisionAllow,
			Reason:  reason,
		})
	}

	// 1. Global kill switch check (fail-closed)
	if bundle.KillSwitchActive && isWriteAction(in.Action) {
		deny("global.kill_switch", "global", "trading kill switch is active")
	}

	// 2. Identity validity, revocation, expiry, scope, and account checks
	if in.Identity.ID == "" {
		deny("identity.present", "identity", "missing authenticated agent identity")
	}
	if in.Identity.Revoked {
		deny("identity.revoked", "identity", "agent identity is revoked")
	}
	if !in.Identity.ExpiresAtUTC.IsZero() && now.After(in.Identity.ExpiresAtUTC) {
		deny("identity.expired", "identity", "agent identity has expired")
	}
	if !in.Identity.HasScope(in.Action) {
		deny("identity.scope", "identity", fmt.Sprintf("identity %q lacks required scope %q", in.Identity.ID, in.Action))
	} else {
		pass("identity.scope", "identity", fmt.Sprintf("identity holds scope %q", in.Action))
	}

	if in.Resource.AccountID != "" && !in.Identity.CanAccessAccount(in.Resource.AccountID) {
		deny("identity.account", "identity", fmt.Sprintf("identity %q is not authorized for account %q", in.Identity.ID, in.Resource.AccountID))
	}

	// 3. Clock skew check on event timestamp
	maxSkew := time.Duration(bundle.MaxClockSkewSeconds) * time.Second
	if maxSkew == 0 {
		maxSkew = 30 * time.Second
	}
	if !in.EventTimeUTC.IsZero() {
		diff := now.Sub(in.EventTimeUTC.UTC())
		if diff < -maxSkew {
			deny("clock.skew_future", "safety", fmt.Sprintf("event timestamp is %s ahead of gateway UTC clock (max skew %s)", (-diff).Round(time.Millisecond), maxSkew))
		}
		if isWriteAction(in.Action) && diff > maxSkew*4 {
			deny("clock.stale_event", "safety", fmt.Sprintf("write event timestamp is stale by %s", diff.Round(time.Millisecond)))
		}
	}

	// 4. Write-action deterministic checks (instrument allowlist, denylist, notional, quantity, order type, time window, quote freshness)
	if isWriteAction(in.Action) {
		if in.Resource.AccountID == "" {
			deny("resource.account", "write", "account is required for order write actions")
		}
		if strings.TrimSpace(in.Resource.Symbol) == "" {
			deny("resource.symbol", "write", "symbol is required for order write actions")
		}
		if in.Resource.NotionalCents <= 0 {
			deny("resource.notional_positive", "write", "notional_cents must be strictly positive")
		}

		// Market quote staleness check (fail-closed if quote provided and stale)
		if in.QuoteVenueTimeUTC != nil && bundle.MaxQuoteAgeSeconds > 0 {
			age := now.Sub(in.QuoteVenueTimeUTC.UTC())
			maxAge := time.Duration(bundle.MaxQuoteAgeSeconds) * time.Second
			if age > maxAge {
				deny("market.quote_stale", "safety", fmt.Sprintf("market quote is stale (%s old > max %s)", age.Round(time.Millisecond), maxAge))
			} else if age < -maxSkew {
				deny("market.quote_clock_skew", "safety", fmt.Sprintf("market quote venue time is ahead of UTC clock by %s", (-age).Round(time.Millisecond)))
			} else {
				pass("market.quote_fresh", "safety", "market quote within freshness bound")
			}
		}

		// Evaluate hierarchical constraints: Org -> Team -> Agent (every level must pass; child can only tighten!)
		levels := []struct {
			name string
			cs   ConstraintSet
		}{
			{name: "org:" + in.Identity.OrgID, cs: bundle.Org},
		}
		if teamCS, ok := bundle.Teams[in.Identity.TeamID]; ok && in.Identity.TeamID != "" {
			levels = append(levels, struct {
				name string
				cs   ConstraintSet
			}{name: "team:" + in.Identity.TeamID, cs: teamCS})
		}
		if agentCS, ok := bundle.Agents[in.Identity.ID]; ok {
			levels = append(levels, struct {
				name string
				cs   ConstraintSet
			}{name: "agent:" + in.Identity.ID, cs: agentCS})
		}

		effectiveRequireApproval := types.MoneyCents(0)
		hasApprovalThreshold := false

		for _, lvl := range levels {
			evalConstraints(lvl.name, lvl.cs, in, now, deny, pass)
			if lvl.cs.RequireApprovalAboveCents != nil {
				if !hasApprovalThreshold || *lvl.cs.RequireApprovalAboveCents < effectiveRequireApproval {
					effectiveRequireApproval = *lvl.cs.RequireApprovalAboveCents
					hasApprovalThreshold = true
				}
			}
		}

		// For order:submit, require signed approval capability whenever notional >= threshold (default threshold 0 = always required)
		if in.Action == types.ScopeOrderSubmit {
			if !hasApprovalThreshold || in.Resource.NotionalCents > effectiveRequireApproval || effectiveRequireApproval == 0 {
				if !in.HasValidApproval {
					deny("approval.required", "write", "signed one-use principal approval capability is required for order:submit")
				} else {
					pass("approval.verified", "write", "signed one-use principal approval capability verified")
				}
			}
		}
	}

	// 5. Evaluate declarative PolicyRules (explicit DENY overrides ALLOW; must have at least one ALLOW rule matching scope)
	matchedAllow := false
	matchedExplicitDeny := false

	for _, rule := range bundle.Rules {
		if rule.Scope != in.Action {
			continue
		}
		matched, err := evalExpression(rule.Expression, in)
		if err != nil {
			// Fail-closed on any expression evaluation error
			deny("rule."+rule.ID, string(rule.Scope), fmt.Sprintf("policy rule %s evaluation error (fail-closed): %v", rule.ID, err))
			matchedExplicitDeny = true
			continue
		}
		if !matched {
			decision.Trace = append(decision.Trace, types.RuleTrace{
				RuleID:  "rule." + rule.ID,
				Scope:   string(rule.Scope),
				Effect:  rule.Effect,
				Matched: false,
				Result:  types.DecisionAllow,
				Reason:  "rule predicate did not match",
			})
			continue
		}
		if rule.Effect == "DENY" {
			matchedExplicitDeny = true
			reason := rule.Reason
			if reason == "" {
				reason = fmt.Sprintf("explicit deny rule %s matched", rule.ID)
			}
			deny("rule."+rule.ID, string(rule.Scope), reason)
		} else if rule.Effect == "ALLOW" {
			matchedAllow = true
			reason := rule.Reason
			if reason == "" {
				reason = fmt.Sprintf("allow rule %s matched", rule.ID)
			}
			pass("rule."+rule.ID, string(rule.Scope), reason)
		}
	}

	if !matchedAllow {
		deny("policy.default_deny", string(in.Action), fmt.Sprintf("default deny: no ALLOW rule matched action %q", in.Action))
	}

	if !matchedExplicitDeny && matchedAllow && len(decision.Reasons) == 0 {
		decision.Result = types.DecisionAllow
	} else {
		decision.Result = types.DecisionDeny
	}

	decision.LatencyMicros = time.Since(start).Microseconds()
	return decision
}

// Simulate compares the active policy against a candidate policy bundle across a batch of EvalInputs.
func (e *Engine) Simulate(candidate PolicyBundle, inputs []EvalInput) ([]SimulationComparison, error) {
	if err := candidate.Validate(); err != nil {
		return nil, err
	}
	candidate.Hash = candidate.ComputeHash()
	comparisons := make([]SimulationComparison, 0, len(inputs))
	for idx, in := range inputs {
		activeDec := e.Evaluate(in)
		candDec := e.EvaluateWithBundle(candidate, in)
		comparisons = append(comparisons, SimulationComparison{
			RequestIndex:      idx,
			ActiveResult:      activeDec.Result,
			CandidateResult:   candDec.Result,
			Changed:           activeDec.Result != candDec.Result,
			ActiveReasons:     activeDec.Reasons,
			CandidateReasons:  candDec.Reasons,
			CandidateDecision: candDec,
		})
	}
	return comparisons, nil
}

func (e *Engine) nextDecisionID() string {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.decisionSeq++
	return fmt.Sprintf("dec-%06d", e.decisionSeq)
}

func evalConstraints(
	scopeName string,
	cs ConstraintSet,
	in EvalInput,
	now time.Time,
	deny func(ruleID, scope, reason string),
	pass func(ruleID, scope, reason string),
) {
	sym := strings.ToUpper(strings.TrimSpace(in.Resource.Symbol))

	// Explicit symbol denylist
	for _, d := range cs.DeniedSymbols {
		if strings.EqualFold(d, sym) {
			deny(scopeName+".denied_symbol", scopeName, fmt.Sprintf("symbol %q is explicitly denied at %s", sym, scopeName))
			break
		}
	}

	// Instrument allowlist
	if len(cs.AllowedSymbols) > 0 {
		allowed := false
		for _, a := range cs.AllowedSymbols {
			if strings.EqualFold(a, sym) {
				allowed = true
				break
			}
		}
		if !allowed {
			deny(scopeName+".symbol_allowlist", scopeName, fmt.Sprintf("symbol %q is not in allowlist at %s", sym, scopeName))
		} else {
			pass(scopeName+".symbol_allowlist", scopeName, fmt.Sprintf("symbol %q permitted at %s", sym, scopeName))
		}
	}

	// Account allowlist
	if len(cs.AllowedAccounts) > 0 {
		allowed := false
		for _, a := range cs.AllowedAccounts {
			if a == in.Resource.AccountID {
				allowed = true
				break
			}
		}
		if !allowed {
			deny(scopeName+".account_allowlist", scopeName, fmt.Sprintf("account %q is not permitted at %s", in.Resource.AccountID, scopeName))
		}
	}

	// Order type restriction
	if len(cs.AllowedOrderTypes) > 0 && in.Resource.OrderType != "" {
		allowed := false
		for _, ot := range cs.AllowedOrderTypes {
			if ot == in.Resource.OrderType {
				allowed = true
				break
			}
		}
		if !allowed {
			deny(scopeName+".order_type", scopeName, fmt.Sprintf("order type %q is not permitted at %s", in.Resource.OrderType, scopeName))
		} else {
			pass(scopeName+".order_type", scopeName, fmt.Sprintf("order type %q permitted at %s", in.Resource.OrderType, scopeName))
		}
	}

	// Side restriction
	if len(cs.AllowedSides) > 0 && in.Resource.Side != "" {
		allowed := false
		for _, sd := range cs.AllowedSides {
			if sd == in.Resource.Side {
				allowed = true
				break
			}
		}
		if !allowed {
			deny(scopeName+".side", scopeName, fmt.Sprintf("order side %q is not permitted at %s", in.Resource.Side, scopeName))
		}
	}

	// Per-order fixed-point notional limit
	if cs.MaxOrderNotionalCents > 0 {
		if in.Resource.NotionalCents > cs.MaxOrderNotionalCents {
			deny(scopeName+".max_order_notional", scopeName,
				fmt.Sprintf("order notional %s exceeds limit %s at %s",
					in.Resource.NotionalCents.FormatUSD(), cs.MaxOrderNotionalCents.FormatUSD(), scopeName))
		} else {
			pass(scopeName+".max_order_notional", scopeName,
				fmt.Sprintf("order notional %s within limit %s at %s",
					in.Resource.NotionalCents.FormatUSD(), cs.MaxOrderNotionalCents.FormatUSD(), scopeName))
		}
	}

	// Daily rolling notional limit (overflow-safe fixed-point addition)
	if cs.MaxDailyNotionalCents > 0 {
		projected, err := types.SafeAddCents(in.DailySpentNotionalCents, in.Resource.NotionalCents)
		if err != nil {
			deny(scopeName+".max_daily_notional_overflow", scopeName, "daily notional arithmetic overflow (fail-closed)")
		} else if projected > cs.MaxDailyNotionalCents {
			deny(scopeName+".max_daily_notional", scopeName,
				fmt.Sprintf("projected daily notional %s exceeds daily limit %s at %s",
					projected.FormatUSD(), cs.MaxDailyNotionalCents.FormatUSD(), scopeName))
		} else {
			pass(scopeName+".max_daily_notional", scopeName,
				fmt.Sprintf("projected daily notional %s within limit %s at %s",
					projected.FormatUSD(), cs.MaxDailyNotionalCents.FormatUSD(), scopeName))
		}
	}

	// Order quantity limit
	if cs.MaxOrderQuantity > 0 && in.Resource.Quantity > 0 {
		if in.Resource.Quantity > cs.MaxOrderQuantity {
			deny(scopeName+".max_order_quantity", scopeName,
				fmt.Sprintf("order quantity %d exceeds limit %d at %s", in.Resource.Quantity, cs.MaxOrderQuantity, scopeName))
		}
	}

	// UTC trading time window
	if cs.TradingWindow != nil {
		if !cs.TradingWindow.Contains(now) {
			deny(scopeName+".trading_window", scopeName,
				fmt.Sprintf("current UTC time %s is outside permitted trading window at %s", now.UTC().Format(time.RFC3339), scopeName))
		} else {
			pass(scopeName+".trading_window", scopeName, fmt.Sprintf("within trading window at %s", scopeName))
		}
	}
}

func isWriteAction(a types.Scope) bool {
	return a == types.ScopeOrderPropose || a == types.ScopeOrderSubmit
}

func normalizeEventTime(eventTime, fallback time.Time) time.Time {
	if eventTime.IsZero() {
		return fallback.UTC()
	}
	return eventTime.UTC()
}

// --- Safe Deterministic Expression Evaluator ---
// Supports conjunctions (&&) of atomic predicates:
//   - "true" / "false"
//   - symbol == "SPY" | symbol != "GME" | symbol in ["SPY","QQQ"]
//   - order_type == "LIMIT" | order_type != "MARKET" | order_type in ["LIMIT"]
//   - side == "BUY" | account == "a-1" | team_id == "desk-1" | agent_id == "agent-1"
//   - notional_cents <= 100000 | notional_cents > 50000 | quantity <= 500

func validateExpression(expr string) error {
	e := strings.TrimSpace(expr)
	if e == "" {
		return errors.New("empty expression")
	}
	clauses := strings.Split(e, "&&")
	for _, c := range clauses {
		if err := validateClause(strings.TrimSpace(c)); err != nil {
			return err
		}
	}
	return nil
}

func validateClause(c string) error {
	if c == "true" || c == "false" {
		return nil
	}
	for _, op := range []string{" in ", " == ", " != ", " <= ", " >= ", " < ", " > "} {
		if idx := strings.Index(c, op); idx > 0 {
			lhs := strings.TrimSpace(c[:idx])
			rhs := strings.TrimSpace(c[idx+len(op):])
			if lhs == "" || rhs == "" {
				return fmt.Errorf("incomplete clause %q", c)
			}
			switch lhs {
			case "symbol", "order_type", "side", "account", "team_id", "agent_id", "org_id", "notional_cents", "quantity":
				return nil
			default:
				return fmt.Errorf("unknown field %q", lhs)
			}
		}
	}
	return fmt.Errorf("unsupported predicate syntax %q", c)
}

func evalExpression(expr string, in EvalInput) (bool, error) {
	clauses := strings.Split(strings.TrimSpace(expr), "&&")
	for _, raw := range clauses {
		c := strings.TrimSpace(raw)
		ok, err := evalClause(c, in)
		if err != nil {
			return false, err
		}
		if !ok {
			return false, nil
		}
	}
	return true, nil
}

func evalClause(c string, in EvalInput) (bool, error) {
	if c == "true" {
		return true, nil
	}
	if c == "false" {
		return false, nil
	}
	for _, op := range []string{" in ", " == ", " != ", " <= ", " >= ", " < ", " > "} {
		if idx := strings.Index(c, op); idx > 0 {
			lhs := strings.TrimSpace(c[:idx])
			rhs := strings.TrimSpace(c[idx+len(op):])
			return compareField(lhs, strings.TrimSpace(op), rhs, in)
		}
	}
	return false, fmt.Errorf("cannot parse clause %q", c)
}

func compareField(field, op, rhs string, in EvalInput) (bool, error) {
	switch field {
	case "notional_cents", "quantity":
		var actual int64
		if field == "notional_cents" {
			actual = int64(in.Resource.NotionalCents)
		} else {
			actual = in.Resource.Quantity
		}
		var target int64
		if _, err := fmt.Sscanf(rhs, "%d", &target); err != nil {
			return false, fmt.Errorf("invalid integer %q for %s", rhs, field)
		}
		switch op {
		case "==":
			return actual == target, nil
		case "!=":
			return actual != target, nil
		case "<":
			return actual < target, nil
		case "<=":
			return actual <= target, nil
		case ">":
			return actual > target, nil
		case ">=":
			return actual >= target, nil
		default:
			return false, fmt.Errorf("unsupported numeric operator %q", op)
		}
	case "symbol", "order_type", "side", "account", "team_id", "agent_id", "org_id":
		var actual string
		switch field {
		case "symbol":
			actual = strings.ToUpper(in.Resource.Symbol)
		case "order_type":
			actual = string(in.Resource.OrderType)
		case "side":
			actual = string(in.Resource.Side)
		case "account":
			actual = in.Resource.AccountID
		case "team_id":
			actual = in.Identity.TeamID
		case "agent_id":
			actual = in.Identity.ID
		case "org_id":
			actual = in.Identity.OrgID
		}
		if op == "in" {
			var list []string
			if err := json.Unmarshal([]byte(rhs), &list); err != nil {
				return false, fmt.Errorf("invalid string list %q", rhs)
			}
			for _, item := range list {
				if strings.EqualFold(actual, item) {
					return true, nil
				}
			}
			return false, nil
		}
		target := strings.Trim(rhs, `"'`)
		switch op {
		case "==":
			return strings.EqualFold(actual, target), nil
		case "!=":
			return !strings.EqualFold(actual, target), nil
		default:
			return false, fmt.Errorf("unsupported string operator %q", op)
		}
	default:
		return false, fmt.Errorf("unknown field %q", field)
	}
}

// DefaultPolicyBundle returns a strict, production-style paper-trading guardrail policy bundle.
func DefaultPolicyBundle() PolicyBundle {
	zeroApproval := types.MoneyCents(0) // Require signed approval for every order:submit
	b := PolicyBundle{
		ID:                  "guardrails-default",
		Version:             "v1.0.0",
		Description:         "Fail-closed paper trading guardrails with instrument allowlist, notional caps, LIMIT-only orders, and mandatory signed approval for execution.",
		KillSwitchActive:    false,
		MaxQuoteAgeSeconds:  30,
		MaxClockSkewSeconds: 30,
		Org: ConstraintSet{
			AllowedSymbols:            []string{"SPY", "QQQ", "AAPL", "MSFT", "NVDA", "TLT"},
			DeniedSymbols:             []string{"MEME", "PENNY", "UNLISTED"},
			AllowedOrderTypes:         []types.OrderType{types.OrderTypeLimit},
			AllowedSides:              []types.Side{types.SideBuy, types.SideSell},
			MaxOrderNotionalCents:     types.MoneyCents(250_000),   // $2,500.00 per order
			MaxDailyNotionalCents:     types.MoneyCents(1_000_000), // $10,000.00 daily cap
			MaxOrderQuantity:          100,
			RequireApprovalAboveCents: &zeroApproval,
			TradingWindow: &TimeWindowUTC{
				StartMinuteUTC:  0,
				EndMinuteUTC:    24*60 - 1, // 24h UTC for paper simulator; overrideable for US cash session 13:30-20:00 UTC
				AllowedWeekdays: []time.Weekday{time.Sunday, time.Monday, time.Tuesday, time.Wednesday, time.Thursday, time.Friday, time.Saturday},
			},
		},
		Teams: map[string]ConstraintSet{
			"equities-research": {
				AllowedSymbols:        []string{"SPY", "QQQ", "AAPL", "MSFT"},
				MaxOrderNotionalCents: types.MoneyCents(100_000), // $1,000.00 team cap (narrower than org)
				MaxDailyNotionalCents: types.MoneyCents(500_000), // $5,000.00 team daily cap
			},
		},
		Agents: map[string]ConstraintSet{},
		Rules: []PolicyRule{
			{ID: "allow-market-read", Scope: types.ScopeMarketRead, Effect: "ALLOW", Expression: "true", Reason: "allow read-only market data"},
			{ID: "allow-portfolio-read", Scope: types.ScopePortfolioRead, Effect: "ALLOW", Expression: "true", Reason: "allow read-only portfolio state"},
			{ID: "allow-order-propose", Scope: types.ScopeOrderPropose, Effect: "ALLOW", Expression: `order_type == "LIMIT"`, Reason: "allow LIMIT order proposals subject to hierarchy limits"},
			{ID: "allow-order-submit", Scope: types.ScopeOrderSubmit, Effect: "ALLOW", Expression: `order_type == "LIMIT"`, Reason: "allow approved LIMIT order submission subject to hierarchy limits"},
			{ID: "deny-market-orders", Scope: types.ScopeOrderSubmit, Effect: "DENY", Expression: `order_type == "MARKET"`, Reason: "explicit deny: MARKET orders are prohibited for autonomous agents"},
			{ID: "deny-market-propose", Scope: types.ScopeOrderPropose, Effect: "DENY", Expression: `order_type == "MARKET"`, Reason: "explicit deny: MARKET orders cannot be proposed"},
		},
	}
	// Deterministic sorted order in JSON
	sort.Strings(b.Org.AllowedSymbols)
	return b
}
