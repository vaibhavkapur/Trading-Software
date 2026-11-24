// Package auth authenticates API callers by bearer token and answers what
// each one may do. Authentication (who is calling) and authorization (what
// they may touch) live here; pre-trade risk and order execution are separate
// packages.
package auth

import (
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
)

// Role is a fixed set of permissions.
type Role string

const (
	// RoleTrader may create, cancel and replace orders, and read orders and
	// positions, for its own accounts.
	RoleTrader Role = "trader"
	// RoleViewer may read orders and positions for its own accounts.
	RoleViewer Role = "viewer"
	// RoleOperator may read every account, run reconciliation and repair
	// issues. It cannot place or change orders.
	RoleOperator Role = "operator"
)

// Principal is an authenticated caller.
type Principal struct {
	Name     string
	Role     Role
	Accounts []string // "*" means every account
}

// allAccounts reports whether the principal's scope covers every account.
func (p Principal) allAccounts() bool {
	if p.Role == RoleOperator {
		return true
	}
	for _, a := range p.Accounts {
		if a == "*" {
			return true
		}
	}
	return false
}

func (p Principal) hasAccount(account string) bool {
	if p.allAccounts() {
		return true
	}
	for _, a := range p.Accounts {
		if a == account {
			return true
		}
	}
	return false
}

// CanView reports whether the principal may read the account's orders.
func (p Principal) CanView(account string) bool {
	return p.Name != "" && p.hasAccount(account)
}

// CanTrade reports whether the principal may create or change orders for the
// account.
func (p Principal) CanTrade(account string) bool {
	return p.Role == RoleTrader && p.Name != "" && p.hasAccount(account)
}

// CanOperate reports whether the principal may run reconciliation and
// repair issues.
func (p Principal) CanOperate() bool { return p.Role == RoleOperator && p.Name != "" }

// AccountScope returns the accounts a listing should be limited to. A nil
// slice means no restriction.
func (p Principal) AccountScope() []string {
	if p.allAccounts() {
		return nil
	}
	return append([]string{}, p.Accounts...)
}

// Keys maps API tokens to principals. Only token hashes are kept in memory.
type Keys struct {
	byHash map[[sha256.Size]byte]Principal
}

// keyEntry is one element of the API key configuration.
type keyEntry struct {
	Principal string   `json:"principal"`
	Role      Role     `json:"role"`
	Accounts  []string `json:"accounts"`
	Token     string   `json:"token"`
}

// minTokenLen rejects obviously guessable tokens in configuration.
const minTokenLen = 24

// ParseKeys reads the API key configuration: a JSON array of
// {"principal","role","accounts","token"}.
func ParseKeys(data []byte) (*Keys, error) {
	var entries []keyEntry
	if err := json.Unmarshal(data, &entries); err != nil {
		return nil, fmt.Errorf("auth: API keys are not a JSON array: %w", err)
	}
	if len(entries) == 0 {
		return nil, errors.New("auth: no API keys configured")
	}
	k := &Keys{byHash: map[[sha256.Size]byte]Principal{}}
	for i, e := range entries {
		switch {
		case strings.TrimSpace(e.Principal) == "":
			return nil, fmt.Errorf("auth: API key %d has no principal", i)
		case e.Role != RoleTrader && e.Role != RoleViewer && e.Role != RoleOperator:
			return nil, fmt.Errorf("auth: API key %q has unknown role %q", e.Principal, e.Role)
		case len(e.Token) < minTokenLen:
			return nil, fmt.Errorf("auth: API key %q has a token shorter than %d characters", e.Principal, minTokenLen)
		case e.Role != RoleOperator && len(e.Accounts) == 0:
			return nil, fmt.Errorf("auth: API key %q has no accounts", e.Principal)
		}
		h := sha256.Sum256([]byte(e.Token))
		if _, dup := k.byHash[h]; dup {
			return nil, fmt.Errorf("auth: API key %q reuses another key's token", e.Principal)
		}
		k.byHash[h] = Principal{Name: e.Principal, Role: e.Role, Accounts: e.Accounts}
	}
	return k, nil
}

// LoadKeys reads the API key configuration from OMS_API_KEYS (inline JSON)
// or OMS_API_KEYS_FILE (a path). Tokens therefore come from the environment
// or a mounted secret, never from source control.
func LoadKeys() (*Keys, error) {
	if inline := os.Getenv("OMS_API_KEYS"); inline != "" {
		return ParseKeys([]byte(inline))
	}
	if path := os.Getenv("OMS_API_KEYS_FILE"); path != "" {
		data, err := os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("auth: read OMS_API_KEYS_FILE: %w", err)
		}
		return ParseKeys(data)
	}
	return nil, errors.New("auth: set OMS_API_KEYS or OMS_API_KEYS_FILE")
}

// Authenticate resolves a bearer token. Looking the token up by its SHA-256
// hash means the comparison does not leak how much of a token matched.
func (k *Keys) Authenticate(token string) (Principal, bool) {
	if k == nil || token == "" {
		return Principal{}, false
	}
	p, ok := k.byHash[sha256.Sum256([]byte(token))]
	return p, ok
}
