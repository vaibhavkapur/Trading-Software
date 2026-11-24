package identity

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/trading-software-projects/agentic-trading-guardrails/src/pkg/types"
)

const (
	DefaultIssuer      = "agentic-trading-guardrails"
	DefaultAudience    = "trading-gateway"
	DefaultMaxSkew     = 30 * time.Second
	DefaultMaxTokenTTL = 15 * time.Minute
)

var (
	ErrUnknownIdentity    = errors.New("unknown identity")
	ErrIdentityRevoked    = errors.New("identity has been revoked")
	ErrIdentityExpired    = errors.New("identity delegation has expired")
	ErrInvalidTokenFormat = errors.New("malformed JWT token")
	ErrUnsupportedAlg     = errors.New("unsupported JWT algorithm")
	ErrUnknownKeyID       = errors.New("unknown signing key id")
	ErrInvalidSignature   = errors.New("invalid JWT signature")
	ErrTokenExpired       = errors.New("token expired")
	ErrTokenNotYetValid   = errors.New("token not yet valid (clock skew)")
	ErrInvalidIssuer      = errors.New("invalid token issuer")
	ErrInvalidAudience    = errors.New("invalid token audience")
	ErrTokenRevoked       = errors.New("token jti has been revoked")
	ErrScopeEscalation    = errors.New("requested or claimed scope exceeds delegated identity scopes")
	ErrAccountEscalation  = errors.New("claimed account exceeds delegated identity accounts")
	ErrPrincipalMismatch  = errors.New("token principal does not match registered delegation")
	ErrInvalidDelegation  = errors.New("invalid identity delegation parameters")
)

type jwtHeader struct {
	Alg string `json:"alg"`
	Typ string `json:"typ"`
	Kid string `json:"kid"`
}

// TokenClaims represents validated claims inside an EdDSA delegated JWT.
type TokenClaims struct {
	JTI             string        `json:"jti"`
	Iss             string        `json:"iss"`
	Aud             string        `json:"aud"`
	Sub             string        `json:"sub"`
	PrincipalID     string        `json:"principal_id"`
	OrgID           string        `json:"org_id"`
	TeamID          string        `json:"team_id"`
	Scopes          []types.Scope `json:"scopes"`
	AllowedAccounts []string      `json:"allowed_accounts"`
	Iat             int64         `json:"iat"`
	Nbf             int64         `json:"nbf"`
	Exp             int64         `json:"exp"`
}

// JWK represents an Ed25519 public key in JSON Web Key (RFC 8037) format.
type JWK struct {
	Kty string `json:"kty"`
	Crv string `json:"crv"`
	Kid string `json:"kid"`
	Use string `json:"use"`
	Alg string `json:"alg"`
	X   string `json:"x"`
}

// JWKS is a standard JSON Web Key Set response.
type JWKS struct {
	Keys []JWK `json:"keys"`
}

type signingKey struct {
	kid  string
	pub  ed25519.PublicKey
	priv ed25519.PrivateKey
}

// Service manages delegated agent identities, Ed25519 JWT signing, JWKS export, and revocation.
type Service struct {
	mu          sync.RWMutex
	clock       types.Clock
	issuer      string
	audience    string
	maxSkew     time.Duration
	maxTokenTTL time.Duration

	activeKeyID string
	keys        map[string]signingKey
	identities  map[string]types.AgentIdentity
	revokedJTIs map[string]time.Time
}

func NewService(clock types.Clock) (*Service, error) {
	if clock == nil {
		clock = types.SystemClock{}
	}
	s := &Service{
		clock:       clock,
		issuer:      DefaultIssuer,
		audience:    DefaultAudience,
		maxSkew:     DefaultMaxSkew,
		maxTokenTTL: DefaultMaxTokenTTL,
		keys:        make(map[string]signingKey),
		identities:  make(map[string]types.AgentIdentity),
		revokedJTIs: make(map[string]time.Time),
	}
	if _, err := s.RotateKey(); err != nil {
		return nil, err
	}
	return s, nil
}

// RotateKey generates a new Ed25519 keypair and sets it active while retaining prior public keys for verification.
func (s *Service) RotateKey() (string, error) {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return "", fmt.Errorf("generate ed25519 key: %w", err)
	}
	kidBytes := make([]byte, 8)
	if _, err := rand.Read(kidBytes); err != nil {
		return "", err
	}
	kid := "key-" + hex.EncodeToString(kidBytes)

	s.mu.Lock()
	defer s.mu.Unlock()
	s.keys[kid] = signingKey{kid: kid, pub: pub, priv: priv}
	s.activeKeyID = kid
	return kid, nil
}

// JWKS returns all active and retained public keys for external verification.
func (s *Service) JWKS() JWKS {
	s.mu.RLock()
	defer s.mu.RUnlock()
	keys := make([]JWK, 0, len(s.keys))
	for _, k := range s.keys {
		keys = append(keys, JWK{
			Kty: "OKP",
			Crv: "Ed25519",
			Kid: k.kid,
			Use: "sig",
			Alg: "EdDSA",
			X:   base64.RawURLEncoding.EncodeToString(k.pub),
		})
	}
	return JWKS{Keys: keys}
}

// RegisterIdentity registers a delegated identity bound to a principal.
func (s *Service) RegisterIdentity(id types.AgentIdentity) error {
	if strings.TrimSpace(id.ID) == "" || strings.TrimSpace(id.PrincipalID) == "" {
		return fmt.Errorf("%w: id and principal_id are required", ErrInvalidDelegation)
	}
	if len(id.Scopes) == 0 {
		return fmt.Errorf("%w: at least one scope is required", ErrInvalidDelegation)
	}
	for _, sc := range id.Scopes {
		if !types.ValidScope(sc) {
			return fmt.Errorf("%w: invalid scope %q", ErrInvalidDelegation, sc)
		}
	}
	if id.ExpiresAtUTC.IsZero() {
		return fmt.Errorf("%w: expires_at_utc is required", ErrInvalidDelegation)
	}
	if id.IssuedAtUTC.IsZero() {
		id.IssuedAtUTC = s.clock.Now()
	} else {
		id.IssuedAtUTC = id.IssuedAtUTC.UTC()
	}
	id.ExpiresAtUTC = id.ExpiresAtUTC.UTC()

	s.mu.Lock()
	defer s.mu.Unlock()
	s.identities[id.ID] = cloneIdentity(id)
	return nil
}

func (s *Service) GetIdentity(agentID string) (types.AgentIdentity, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	id, ok := s.identities[agentID]
	if !ok {
		return types.AgentIdentity{}, false
	}
	return cloneIdentity(id), true
}

func (s *Service) ListIdentities() []types.AgentIdentity {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]types.AgentIdentity, 0, len(s.identities))
	for _, id := range s.identities {
		out = append(out, cloneIdentity(id))
	}
	return out
}

func (s *Service) RevokeIdentity(agentID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	id, ok := s.identities[agentID]
	if !ok {
		return ErrUnknownIdentity
	}
	id.Revoked = true
	s.identities[agentID] = id
	return nil
}

func (s *Service) RevokeToken(jti string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.revokedJTIs[jti] = s.clock.Now()
}

// IssueToken mints a short-lived EdDSA JWT for a registered identity.
// Requested scopes may narrow (never widen) the identity's registered scopes.
func (s *Service) IssueToken(agentID string, requestedScopes []types.Scope, ttl time.Duration) (string, TokenClaims, error) {
	s.mu.RLock()
	id, ok := s.identities[agentID]
	activeKey := s.keys[s.activeKeyID]
	s.mu.RUnlock()

	if !ok {
		return "", TokenClaims{}, ErrUnknownIdentity
	}
	now := s.clock.Now()
	if id.Revoked {
		return "", TokenClaims{}, ErrIdentityRevoked
	}
	if !now.Before(id.ExpiresAtUTC) {
		return "", TokenClaims{}, ErrIdentityExpired
	}

	scopes := id.Scopes
	if len(requestedScopes) > 0 {
		for _, rs := range requestedScopes {
			if !id.HasScope(rs) {
				return "", TokenClaims{}, fmt.Errorf("%w: %s", ErrScopeEscalation, rs)
			}
		}
		scopes = requestedScopes
	}

	if ttl <= 0 || ttl > s.maxTokenTTL {
		ttl = s.maxTokenTTL
	}
	exp := now.Add(ttl)
	if exp.After(id.ExpiresAtUTC) {
		exp = id.ExpiresAtUTC
	}

	jtiBytes := make([]byte, 12)
	if _, err := rand.Read(jtiBytes); err != nil {
		return "", TokenClaims{}, err
	}

	claims := TokenClaims{
		JTI:             "jti-" + hex.EncodeToString(jtiBytes),
		Iss:             s.issuer,
		Aud:             s.audience,
		Sub:             id.ID,
		PrincipalID:     id.PrincipalID,
		OrgID:           id.OrgID,
		TeamID:          id.TeamID,
		Scopes:          append([]types.Scope(nil), scopes...),
		AllowedAccounts: append([]string(nil), id.AllowedAccounts...),
		Iat:             now.Unix(),
		Nbf:             now.Unix(),
		Exp:             exp.Unix(),
	}

	header := jwtHeader{Alg: "EdDSA", Typ: "JWT", Kid: activeKey.kid}
	headerJSON, err := json.Marshal(header)
	if err != nil {
		return "", TokenClaims{}, err
	}
	claimsJSON, err := json.Marshal(claims)
	if err != nil {
		return "", TokenClaims{}, err
	}

	signingInput := base64.RawURLEncoding.EncodeToString(headerJSON) + "." + base64.RawURLEncoding.EncodeToString(claimsJSON)
	sig := ed25519.Sign(activeKey.priv, []byte(signingInput))
	token := signingInput + "." + base64.RawURLEncoding.EncodeToString(sig)
	return token, claims, nil
}

// VerifyToken cryptographically validates a delegated JWT and cross-checks live registry state.
// Never trusts unverified claims or prompt-injected strings.
func (s *Service) VerifyToken(token string) (types.AgentIdentity, TokenClaims, error) {
	raw := strings.TrimSpace(token)
	raw = strings.TrimPrefix(raw, "Bearer ")
	raw = strings.TrimPrefix(raw, "bearer ")
	parts := strings.Split(raw, ".")
	if len(parts) != 3 {
		return types.AgentIdentity{}, TokenClaims{}, ErrInvalidTokenFormat
	}

	headerBytes, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		return types.AgentIdentity{}, TokenClaims{}, ErrInvalidTokenFormat
	}
	var hdr jwtHeader
	if err := json.Unmarshal(headerBytes, &hdr); err != nil {
		return types.AgentIdentity{}, TokenClaims{}, ErrInvalidTokenFormat
	}
	if hdr.Alg != "EdDSA" || hdr.Typ != "JWT" {
		return types.AgentIdentity{}, TokenClaims{}, ErrUnsupportedAlg
	}

	s.mu.RLock()
	key, keyOK := s.keys[hdr.Kid]
	s.mu.RUnlock()
	if !keyOK {
		return types.AgentIdentity{}, TokenClaims{}, ErrUnknownKeyID
	}

	sigBytes, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil || len(sigBytes) != ed25519.SignatureSize {
		return types.AgentIdentity{}, TokenClaims{}, ErrInvalidSignature
	}
	signingInput := parts[0] + "." + parts[1]
	if !ed25519.Verify(key.pub, []byte(signingInput), sigBytes) {
		return types.AgentIdentity{}, TokenClaims{}, ErrInvalidSignature
	}

	claimsBytes, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return types.AgentIdentity{}, TokenClaims{}, ErrInvalidTokenFormat
	}
	var claims TokenClaims
	if err := json.Unmarshal(claimsBytes, &claims); err != nil {
		return types.AgentIdentity{}, TokenClaims{}, ErrInvalidTokenFormat
	}

	if claims.Iss != s.issuer {
		return types.AgentIdentity{}, TokenClaims{}, ErrInvalidIssuer
	}
	if claims.Aud != s.audience {
		return types.AgentIdentity{}, TokenClaims{}, ErrInvalidAudience
	}

	now := s.clock.Now()
	skewSec := int64(s.maxSkew.Seconds())
	if now.Unix() > claims.Exp+skewSec {
		return types.AgentIdentity{}, TokenClaims{}, ErrTokenExpired
	}
	if claims.Nbf > now.Unix()+skewSec || claims.Iat > now.Unix()+skewSec {
		return types.AgentIdentity{}, TokenClaims{}, ErrTokenNotYetValid
	}

	s.mu.RLock()
	_, jtiRevoked := s.revokedJTIs[claims.JTI]
	regID, regOK := s.identities[claims.Sub]
	s.mu.RUnlock()

	if jtiRevoked {
		return types.AgentIdentity{}, TokenClaims{}, ErrTokenRevoked
	}
	if !regOK {
		return types.AgentIdentity{}, TokenClaims{}, ErrUnknownIdentity
	}
	if regID.Revoked {
		return types.AgentIdentity{}, TokenClaims{}, ErrIdentityRevoked
	}
	if now.After(regID.ExpiresAtUTC.Add(s.maxSkew)) {
		return types.AgentIdentity{}, TokenClaims{}, ErrIdentityExpired
	}
	if claims.PrincipalID != regID.PrincipalID {
		return types.AgentIdentity{}, TokenClaims{}, ErrPrincipalMismatch
	}

	// Defense-in-depth: token scopes and accounts must be a strict subset of registered identity.
	effectiveScopes := make([]types.Scope, 0, len(claims.Scopes))
	for _, sc := range claims.Scopes {
		if !regID.HasScope(sc) {
			return types.AgentIdentity{}, TokenClaims{}, fmt.Errorf("%w: %s", ErrScopeEscalation, sc)
		}
		effectiveScopes = append(effectiveScopes, sc)
	}
	effectiveAccounts := make([]string, 0, len(claims.AllowedAccounts))
	for _, acc := range claims.AllowedAccounts {
		if !regID.CanAccessAccount(acc) {
			return types.AgentIdentity{}, TokenClaims{}, fmt.Errorf("%w: %s", ErrAccountEscalation, acc)
		}
		effectiveAccounts = append(effectiveAccounts, acc)
	}

	resolved := cloneIdentity(regID)
	resolved.Scopes = effectiveScopes
	resolved.AllowedAccounts = effectiveAccounts
	return resolved, claims, nil
}

func cloneIdentity(id types.AgentIdentity) types.AgentIdentity {
	cp := id
	cp.Scopes = append([]types.Scope(nil), id.Scopes...)
	cp.AllowedAccounts = append([]string(nil), id.AllowedAccounts...)
	return cp
}
