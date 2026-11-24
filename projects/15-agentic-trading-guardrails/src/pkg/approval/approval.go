package approval

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
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
	DefaultGrantTTL = 60 * time.Second
	MaxGrantTTL     = 5 * time.Minute
	DefaultMaxSkew  = 5 * time.Second
)

var (
	ErrInvalidGrantFormat    = errors.New("malformed approval grant token")
	ErrGrantInvalidSignature = errors.New("approval grant signature verification failed")
	ErrGrantUnknownKey       = errors.New("approval grant signed by unknown key")
	ErrGrantExpired          = errors.New("approval grant has expired")
	ErrGrantClockSkew        = errors.New("approval grant issued in the future (clock skew)")
	ErrGrantReplayed         = errors.New("approval grant has already been consumed (replay denied)")
	ErrGrantProposalMismatch = errors.New("approval grant proposal_id does not match target proposal")
	ErrGrantHashMismatch     = errors.New("approval grant proposal_hash does not match current proposal terms (proposal modified)")
	ErrGrantWrongAccount     = errors.New("approval grant account_id does not match target account")
	ErrGrantWrongPrincipal   = errors.New("approval grant principal_id does not match proposal principal")
	ErrHashMismatchOnIssue   = errors.New("supplied proposal_hash does not match canonical proposal terms hash")
)

// ViolationKind categorizes approval failures for metrics and auditing.
type ViolationKind string

const (
	ViolationNone             ViolationKind = ""
	ViolationExpired          ViolationKind = "approval_expired"
	ViolationReplayed         ViolationKind = "approval_replayed"
	ViolationModifiedProposal ViolationKind = "proposal_modified"
	ViolationWrongAccount     ViolationKind = "wrong_account"
	ViolationWrongPrincipal   ViolationKind = "wrong_principal"
	ViolationInvalidSignature ViolationKind = "invalid_signature"
	ViolationClockSkew        ViolationKind = "clock_skew"
)

// ComputeProposalHash computes a canonical SHA-256 hash over every immutable order term.
// Modifying any field (symbol, account, qty, price, side, order_type, tif, notional, actor) produces a different hash.
func ComputeProposalHash(p types.OrderProposal) string {
	canonical := fmt.Sprintf(
		"v1|id=%s|agent=%s|principal=%s|account=%s|symbol=%s|side=%s|type=%s|qty=%d|price_ticks=%d|notional_cents=%d|tif=%s",
		p.ID,
		p.AgentID,
		p.PrincipalID,
		p.AccountID,
		strings.ToUpper(strings.TrimSpace(p.Symbol)),
		p.Side,
		p.OrderType,
		p.Quantity,
		p.LimitPriceTicks,
		p.NotionalCents,
		p.TimeInForce,
	)
	sum := sha256.Sum256([]byte(canonical))
	return "sha256:" + hex.EncodeToString(sum[:])
}

type capabilityPayload struct {
	GrantID      string `json:"grant_id"`
	ProposalID   string `json:"proposal_id"`
	ProposalHash string `json:"proposal_hash"`
	AccountID    string `json:"account_id"`
	PrincipalID  string `json:"principal_id"`
	Nonce        string `json:"nonce"`
	KeyID        string `json:"kid"`
	IssuedAtUnix int64  `json:"iat"`
	ExpiresUnix  int64  `json:"exp"`
}

// ConsumedHook persists consumed grant IDs so replay protection survives process restarts.
type ConsumedHook func(grantID string, consumedAt time.Time) error

// Service issues and atomically consumes Ed25519-signed, one-use, proposal-bound approval capabilities.
type Service struct {
	mu           sync.Mutex
	clock        types.Clock
	maxSkew      time.Duration
	keyID        string
	pub          ed25519.PublicKey
	priv         ed25519.PrivateKey
	grants       map[string]types.ApprovalGrant
	consumed     map[string]time.Time
	consumedHook ConsumedHook
}

func NewService(clock types.Clock) (*Service, error) {
	if clock == nil {
		clock = types.SystemClock{}
	}
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("generate approval signing key: %w", err)
	}
	return NewServiceWithKey(clock, "approval-key-1", pub, priv), nil
}

func NewServiceWithKey(clock types.Clock, keyID string, pub ed25519.PublicKey, priv ed25519.PrivateKey) *Service {
	if clock == nil {
		clock = types.SystemClock{}
	}
	return &Service{
		clock:    clock,
		maxSkew:  DefaultMaxSkew,
		keyID:    keyID,
		pub:      pub,
		priv:     priv,
		grants:   make(map[string]types.ApprovalGrant),
		consumed: make(map[string]time.Time),
	}
}

func (s *Service) SetConsumedHook(fn ConsumedHook) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.consumedHook = fn
}

// RestoreConsumed seeds previously consumed grant IDs from durable storage on startup.
func (s *Service) RestoreConsumed(consumed map[string]time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for id, ts := range consumed {
		s.consumed[id] = ts.UTC()
	}
}

func (s *Service) PublicKey() (string, ed25519.PublicKey) {
	return s.keyID, s.pub
}

// IssueGrant mints a signed, short-lived, one-use capability bound to proposal_id, proposal_hash, account_id, and principal_id.
func (s *Service) IssueGrant(proposal types.OrderProposal, suppliedHash, approverPrincipalID string, ttl time.Duration) (types.ApprovalGrant, error) {
	canonicalHash := ComputeProposalHash(proposal)
	if suppliedHash != canonicalHash || proposal.TermsHash != canonicalHash {
		return types.ApprovalGrant{}, ErrHashMismatchOnIssue
	}
	if approverPrincipalID == "" || approverPrincipalID != proposal.PrincipalID {
		return types.ApprovalGrant{}, ErrGrantWrongPrincipal
	}
	if ttl <= 0 {
		ttl = DefaultGrantTTL
	}
	if ttl > MaxGrantTTL {
		ttl = MaxGrantTTL
	}

	now := s.clock.Now()
	exp := now.Add(ttl)

	idBytes := make([]byte, 10)
	nonceBytes := make([]byte, 12)
	if _, err := rand.Read(idBytes); err != nil {
		return types.ApprovalGrant{}, err
	}
	if _, err := rand.Read(nonceBytes); err != nil {
		return types.ApprovalGrant{}, err
	}
	grantID := "apr-" + hex.EncodeToString(idBytes)
	nonce := hex.EncodeToString(nonceBytes)

	payload := capabilityPayload{
		GrantID:      grantID,
		ProposalID:   proposal.ID,
		ProposalHash: canonicalHash,
		AccountID:    proposal.AccountID,
		PrincipalID:  approverPrincipalID,
		Nonce:        nonce,
		KeyID:        s.keyID,
		IssuedAtUnix: now.Unix(),
		ExpiresUnix:  exp.Unix(),
	}
	payloadBytes, err := json.Marshal(payload)
	if err != nil {
		return types.ApprovalGrant{}, err
	}
	encodedPayload := base64.RawURLEncoding.EncodeToString(payloadBytes)
	sig := ed25519.Sign(s.priv, []byte(encodedPayload))
	token := encodedPayload + "." + base64.RawURLEncoding.EncodeToString(sig)

	grant := types.ApprovalGrant{
		ID:           grantID,
		ProposalID:   proposal.ID,
		ProposalHash: canonicalHash,
		AccountID:    proposal.AccountID,
		PrincipalID:  approverPrincipalID,
		Nonce:        nonce,
		KeyID:        s.keyID,
		IssuedAtUTC:  now,
		ExpiresAtUTC: exp,
		Token:        token,
	}

	s.mu.Lock()
	s.grants[grantID] = grant
	s.mu.Unlock()
	return grant, nil
}

// VerifyAndConsume verifies signature, expiry, clock skew, account/principal binding, canonical proposal hash,
// and atomically consumes the grant so it can never be replayed.
func (s *Service) VerifyAndConsume(token string, currentProposal types.OrderProposal, targetAccountID string) (types.ApprovalGrant, ViolationKind, error) {
	parts := strings.Split(strings.TrimSpace(token), ".")
	if len(parts) != 2 {
		return types.ApprovalGrant{}, ViolationInvalidSignature, ErrInvalidGrantFormat
	}
	sigBytes, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil || len(sigBytes) != ed25519.SignatureSize {
		return types.ApprovalGrant{}, ViolationInvalidSignature, ErrGrantInvalidSignature
	}
	if !ed25519.Verify(s.pub, []byte(parts[0]), sigBytes) {
		return types.ApprovalGrant{}, ViolationInvalidSignature, ErrGrantInvalidSignature
	}

	payloadBytes, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		return types.ApprovalGrant{}, ViolationInvalidSignature, ErrInvalidGrantFormat
	}
	var payload capabilityPayload
	if err := json.Unmarshal(payloadBytes, &payload); err != nil {
		return types.ApprovalGrant{}, ViolationInvalidSignature, ErrInvalidGrantFormat
	}
	if payload.KeyID != s.keyID {
		return types.ApprovalGrant{}, ViolationInvalidSignature, ErrGrantUnknownKey
	}

	now := s.clock.Now()
	skewSec := int64(s.maxSkew.Seconds())
	if payload.IssuedAtUnix > now.Unix()+skewSec {
		return types.ApprovalGrant{}, ViolationClockSkew, ErrGrantClockSkew
	}
	if now.Unix() > payload.ExpiresUnix {
		return types.ApprovalGrant{}, ViolationExpired, ErrGrantExpired
	}

	if payload.ProposalID != currentProposal.ID {
		return types.ApprovalGrant{}, ViolationModifiedProposal, ErrGrantProposalMismatch
	}
	if targetAccountID != "" && payload.AccountID != targetAccountID {
		return types.ApprovalGrant{}, ViolationWrongAccount, ErrGrantWrongAccount
	}
	if payload.AccountID != currentProposal.AccountID {
		return types.ApprovalGrant{}, ViolationWrongAccount, ErrGrantWrongAccount
	}
	if payload.PrincipalID != currentProposal.PrincipalID {
		return types.ApprovalGrant{}, ViolationWrongPrincipal, ErrGrantWrongPrincipal
	}

	// Recompute canonical proposal hash from live terms to detect ANY tampering.
	liveHash := ComputeProposalHash(currentProposal)
	if payload.ProposalHash != liveHash || currentProposal.TermsHash != liveHash {
		return types.ApprovalGrant{}, ViolationModifiedProposal, ErrGrantHashMismatch
	}

	// Atomic one-use consumption under mutex.
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, alreadyUsed := s.consumed[payload.GrantID]; alreadyUsed {
		return types.ApprovalGrant{}, ViolationReplayed, ErrGrantReplayed
	}
	if s.consumedHook != nil {
		if err := s.consumedHook(payload.GrantID, now); err != nil {
			// Fail-closed if replay state cannot be persisted
			return types.ApprovalGrant{}, ViolationReplayed, fmt.Errorf("persist grant consumption (fail-closed): %w", err)
		}
	}
	s.consumed[payload.GrantID] = now

	consumedCopy := now
	grant := types.ApprovalGrant{
		ID:            payload.GrantID,
		ProposalID:    payload.ProposalID,
		ProposalHash:  payload.ProposalHash,
		AccountID:     payload.AccountID,
		PrincipalID:   payload.PrincipalID,
		Nonce:         payload.Nonce,
		KeyID:         payload.KeyID,
		IssuedAtUTC:   time.Unix(payload.IssuedAtUnix, 0).UTC(),
		ExpiresAtUTC:  time.Unix(payload.ExpiresUnix, 0).UTC(),
		ConsumedAtUTC: &consumedCopy,
		Token:         "[REDACTED]",
	}
	s.grants[payload.GrantID] = grant
	return grant, ViolationNone, nil
}
