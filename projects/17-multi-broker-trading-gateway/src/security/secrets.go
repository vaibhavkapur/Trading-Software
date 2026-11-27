package security

import (
	"encoding/json"
	"fmt"
	"sync"
	"time"

	"github.com/vaibhavkapur/Trading-Software/projects/17-multi-broker-trading-gateway/src/domain"
)

// SecretString wraps sensitive credential material so it cannot accidentally leak via fmt, logs, or JSON.
type SecretString struct {
	value string
}

// NewSecretString constructs a wrapped secret.
func NewSecretString(raw string) SecretString {
	return SecretString{value: raw}
}

// Expose returns the raw value exclusively for authenticated outbound wire calls.
func (s SecretString) Expose() string {
	return s.value
}

// IsEmpty reports whether the secret is unset.
func (s SecretString) IsEmpty() bool {
	return s.value == ""
}

// String always returns [REDACTED].
func (s SecretString) String() string { return "[REDACTED]" }

// GoString always returns [REDACTED].
func (s SecretString) GoString() string { return "[REDACTED]" }

// MarshalJSON always emits "[REDACTED]".
func (s SecretString) MarshalJSON() ([]byte, error) {
	return json.Marshal("[REDACTED]")
}

// Credential represents a broker authentication credential with explicit expiry and version.
type Credential struct {
	Ref          string       `json:"ref"`
	Scheme       string       `json:"scheme"` // BEARER, API_KEY, HMAC
	Secret       SecretString `json:"secret"`
	Version      string       `json:"version"`
	ExpiresAtUTC time.Time    `json:"expires_at_utc"`
	Revoked      bool         `json:"revoked"`
}

// IsValidAt verifies the credential is non-empty, not revoked, and not expired at timestamp t.
func (c Credential) IsValidAt(t time.Time) bool {
	if c.Secret.IsEmpty() || c.Revoked {
		return false
	}
	if !c.ExpiresAtUTC.IsZero() && !t.Before(c.ExpiresAtUTC) {
		return false
	}
	return true
}

// SecretManager abstracts secret storage (Vault/KMS/SM in production; isolated in-memory vault in local prototype).
type SecretManager interface {
	GetCredential(ref string) (Credential, error)
	RotateCredential(ref string, newSecret SecretString, expiresAtUTC time.Time) (Credential, error)
	RegisteredSecretValues() []string
}

// MemorySecretManager is a thread-safe in-memory secret vault that never persists secrets to config files.
type MemorySecretManager struct {
	mu    sync.RWMutex
	creds map[string]Credential
	seq   int
}

// NewMemorySecretManager creates an empty secret manager.
func NewMemorySecretManager() *MemorySecretManager {
	return &MemorySecretManager{
		creds: make(map[string]Credential),
	}
}

// PutCredential stores or updates a credential in memory.
func (m *MemorySecretManager) PutCredential(cred Credential) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if cred.Version == "" {
		m.seq++
		cred.Version = fmt.Sprintf("v%d", m.seq)
	}
	m.creds[cred.Ref] = cred
}

// GetCredential retrieves a credential by reference.
func (m *MemorySecretManager) GetCredential(ref string) (Credential, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	c, ok := m.creds[ref]
	if !ok {
		return Credential{}, domain.NewError(domain.ErrCodeAuthInvalid, fmt.Sprintf("secret ref %q not found", ref), false)
	}
	return c, nil
}

// RotateCredential atomically rotates a credential to a new secret value and expiry.
func (m *MemorySecretManager) RotateCredential(ref string, newSecret SecretString, expiresAtUTC time.Time) (Credential, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	prev, ok := m.creds[ref]
	if !ok {
		return Credential{}, domain.NewError(domain.ErrCodeAuthInvalid, fmt.Sprintf("secret ref %q not found for rotation", ref), false)
	}
	m.seq++
	prev.Secret = newSecret
	prev.ExpiresAtUTC = expiresAtUTC.UTC()
	prev.Revoked = false
	prev.Version = fmt.Sprintf("v%d", m.seq)
	m.creds[ref] = prev
	return prev, nil
}

// ExpireCredential forces a credential to expire (used by deterministic fault injection & drills).
func (m *MemorySecretManager) ExpireCredential(ref string, expiredAt time.Time) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	c, ok := m.creds[ref]
	if !ok {
		return domain.NewError(domain.ErrCodeAuthInvalid, fmt.Sprintf("secret ref %q not found", ref), false)
	}
	c.ExpiresAtUTC = expiredAt.UTC()
	m.creds[ref] = c
	return nil
}

// RegisteredSecretValues exposes raw values strictly to the RedactingLogger scrubber so any accidental echo is masked.
func (m *MemorySecretManager) RegisteredSecretValues() []string {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]string, 0, len(m.creds))
	for _, c := range m.creds {
		if !c.Secret.IsEmpty() {
			out = append(out, c.Secret.Expose())
		}
	}
	return out
}
