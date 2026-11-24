package api

import (
	"context"
	"crypto/subtle"
	"net/http"
	"strings"
)

// Role defines authorization tiers separated from authentication and execution.
type Role string

const (
	RoleViewer   Role = "viewer"
	RoleOperator Role = "operator"
	RoleAdmin    Role = "admin"
)

// Principal represents an authenticated caller.
type Principal struct {
	Subject string
	Role    Role
}

type principalCtxKey struct{}

// PrincipalFromContext extracts the authenticated Principal from request context.
func PrincipalFromContext(ctx context.Context) (Principal, bool) {
	p, ok := ctx.Value(principalCtxKey{}).(Principal)
	return p, ok
}

// Authenticator verifies caller credentials independently of authorization rules.
type Authenticator interface {
	Authenticate(r *http.Request) (Principal, bool)
}

// Authorizer checks whether a Principal is permitted to perform an action.
type Authorizer interface {
	Authorize(p Principal, action string) bool
}

// APIKeyEntry maps a secret API key (injected via environment/secret manager, never logged) to a Principal.
type APIKeyEntry struct {
	Key       string
	Principal Principal
}

// StaticAPIKeyAuthenticator validates X-API-Key headers using constant-time comparison.
// If no keys are configured, it runs in local paper-demo mode granting operator access.
type StaticAPIKeyAuthenticator struct {
	entries []APIKeyEntry
}

// NewStaticAPIKeyAuthenticator constructs an authenticator. Passing nil/empty enables local paper-demo mode.
func NewStaticAPIKeyAuthenticator(entries []APIKeyEntry) *StaticAPIKeyAuthenticator {
	cp := make([]APIKeyEntry, len(entries))
	copy(cp, entries)
	return &StaticAPIKeyAuthenticator{entries: cp}
}

func (a *StaticAPIKeyAuthenticator) Authenticate(r *http.Request) (Principal, bool) {
	if len(a.entries) == 0 {
		actor := strings.TrimSpace(r.Header.Get("X-Actor"))
		if actor == "" {
			actor = "paper-operator"
		}
		return Principal{Subject: actor, Role: RoleOperator}, true
	}

	provided := strings.TrimSpace(r.Header.Get("X-API-Key"))
	if provided == "" {
		authz := strings.TrimSpace(r.Header.Get("Authorization"))
		if strings.HasPrefix(authz, "Bearer ") {
			provided = strings.TrimSpace(strings.TrimPrefix(authz, "Bearer "))
		}
	}
	if provided == "" {
		return Principal{}, false
	}
	for _, e := range a.entries {
		if subtle.ConstantTimeCompare([]byte(provided), []byte(e.Key)) == 1 {
			return e.Principal, true
		}
	}
	return Principal{}, false
}

// RBACAuthorizer enforces action-level permissions per Role.
type RBACAuthorizer struct{}

func (RBACAuthorizer) Authorize(p Principal, action string) bool {
	switch p.Role {
	case RoleAdmin:
		return true
	case RoleOperator:
		switch action {
		case "runs:read", "runs:create", "issues:read", "issues:mutate", "policies:read":
			return true
		default:
			return false
		}
	case RoleViewer:
		switch action {
		case "runs:read", "issues:read", "policies:read":
			return true
		default:
			return false
		}
	default:
		return false
	}
}
