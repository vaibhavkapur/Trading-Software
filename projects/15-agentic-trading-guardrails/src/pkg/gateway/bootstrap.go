package gateway

import (
	"time"

	"github.com/trading-software-projects/agentic-trading-guardrails/src/pkg/types"
)

// SeedDemoIdentities registers representative paper-trading identities:
// - agent-1: research & execution agent bound to principal-alice on account a-1
// - agent-readonly: market/portfolio read-only agent (cannot propose or submit)
// - principal-alice: human principal authorized to grant one-use approvals
// - admin-risk: risk officer authorized to publish and simulate policy versions
func (g *Gateway) SeedDemoIdentities() error {
	now := g.Clock.Now()
	exp := now.Add(24 * time.Hour)

	identities := []types.AgentIdentity{
		{
			ID:              "agent-1",
			PrincipalID:     "principal-alice",
			OrgID:           "acme-paper-desk",
			TeamID:          "equities-research",
			Scopes:          []types.Scope{types.ScopeMarketRead, types.ScopePortfolioRead, types.ScopeOrderPropose, types.ScopeOrderSubmit},
			AllowedAccounts: []string{"a-1"},
			IssuedAtUTC:     now,
			ExpiresAtUTC:    exp,
		},
		{
			ID:              "agent-readonly",
			PrincipalID:     "principal-alice",
			OrgID:           "acme-paper-desk",
			TeamID:          "equities-research",
			Scopes:          []types.Scope{types.ScopeMarketRead, types.ScopePortfolioRead},
			AllowedAccounts: []string{"a-1"},
			IssuedAtUTC:     now,
			ExpiresAtUTC:    exp,
		},
		{
			ID:              "principal-alice",
			PrincipalID:     "principal-alice",
			OrgID:           "acme-paper-desk",
			TeamID:          "equities-research",
			Scopes:          []types.Scope{types.ScopeApprovalGrant, types.ScopeAuditRead, types.ScopePortfolioRead},
			AllowedAccounts: []string{"a-1", "a-2"},
			IssuedAtUTC:     now,
			ExpiresAtUTC:    exp,
		},
		{
			ID:              "principal-bob",
			PrincipalID:     "principal-bob",
			OrgID:           "acme-paper-desk",
			TeamID:          "macro-desk",
			Scopes:          []types.Scope{types.ScopeApprovalGrant, types.ScopeAuditRead},
			AllowedAccounts: []string{"a-2"},
			IssuedAtUTC:     now,
			ExpiresAtUTC:    exp,
		},
		{
			ID:              "admin-risk",
			PrincipalID:     "admin-risk",
			OrgID:           "acme-paper-desk",
			TeamID:          "risk-governance",
			Scopes:          []types.Scope{types.ScopePolicyAdmin, types.ScopeAuditRead},
			AllowedAccounts: []string{"a-1", "a-2"},
			IssuedAtUTC:     now,
			ExpiresAtUTC:    exp,
		},
	}

	for _, id := range identities {
		if err := g.Identity.RegisterIdentity(id); err != nil {
			return err
		}
	}
	return nil
}
