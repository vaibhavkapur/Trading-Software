package security

import (
	"encoding/json"
	"fmt"
	"os"

	"github.com/vaibhavkapur/Trading-Software/projects/17-multi-broker-trading-gateway/src/domain"
)

// ExecutionPolicy enforces paper/sandbox defaults and fail-closed live-capital controls.
type ExecutionPolicy struct {
	SchemaVersion                  string                 `json:"schema_version"`
	PolicyID                       string                 `json:"policy_id"`
	ExecutionEnabled               bool                   `json:"execution_enabled"`
	PermittedModes                 []domain.ExecutionMode `json:"permitted_modes"`
	DenyLiveExecution              bool                   `json:"deny_live_execution"`
	RequireIndependentApprovalLive bool                   `json:"require_independent_approval_for_live"`
	MaxSingleOrderNotionalUSD      float64                `json:"max_single_order_notional_usd"`
	FailClosedOnReconcileDrift     bool                   `json:"fail_closed_on_reconciliation_drift"`
	FailClosedOnAuthError          bool                   `json:"fail_closed_on_auth_error"`
	ApprovedBy                     string                 `json:"approved_by"`
	ReviewedAtUTC                  string                 `json:"reviewed_at_utc"`
}

// DefaultSafePolicy returns a fail-closed policy that permits only paper/sandbox.
func DefaultSafePolicy() ExecutionPolicy {
	return ExecutionPolicy{
		SchemaVersion:                  "1.0.0",
		PolicyID:                       "default-safe-paper-sandbox",
		ExecutionEnabled:               true,
		PermittedModes:                 []domain.ExecutionMode{domain.ModePaper, domain.ModeSandbox},
		DenyLiveExecution:              true,
		RequireIndependentApprovalLive: true,
		MaxSingleOrderNotionalUSD:      250000.0,
		FailClosedOnReconcileDrift:     true,
		FailClosedOnAuthError:          false,
	}
}

// LoadExecutionPolicy reads config/policy.json.
func LoadExecutionPolicy(path string) (ExecutionPolicy, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return ExecutionPolicy{}, fmt.Errorf("read policy file: %w", err)
	}
	var p ExecutionPolicy
	if err := json.Unmarshal(data, &p); err != nil {
		return ExecutionPolicy{}, fmt.Errorf("parse policy file: %w", err)
	}
	return p, nil
}

// AuthorizeOrder evaluates the deterministic policy gate prior to broker dispatch.
func (p ExecutionPolicy) AuthorizeOrder(req domain.OrderRequest) error {
	if !p.ExecutionEnabled {
		return domain.NewError(domain.ErrCodePolicyDenied, "order execution is disabled by policy", false).
			WithContext(req.BrokerID, "")
	}
	mode := req.Mode
	if mode == "" {
		mode = domain.ModePaper
	}
	if mode == domain.ModeLive && p.DenyLiveExecution {
		return domain.NewError(domain.ErrCodePolicyDenied, "live execution is explicitly prohibited by policy; paper/sandbox only", false).
			WithContext(req.BrokerID, "")
	}
	allowed := false
	for _, m := range p.PermittedModes {
		if m == mode {
			allowed = true
			break
		}
	}
	if !allowed {
		return domain.NewError(domain.ErrCodePolicyDenied, fmt.Sprintf("execution mode %q is not permitted by policy %q", mode, p.PolicyID), false).
			WithContext(req.BrokerID, "")
	}
	if p.MaxSingleOrderNotionalUSD > 0 && req.EstimatedNotional(0) > p.MaxSingleOrderNotionalUSD {
		return domain.NewError(domain.ErrCodePolicyDenied, fmt.Sprintf("estimated notional %.2f exceeds policy limit %.2f", req.EstimatedNotional(0), p.MaxSingleOrderNotionalUSD), false).
			WithContext(req.BrokerID, "")
	}
	return nil
}
