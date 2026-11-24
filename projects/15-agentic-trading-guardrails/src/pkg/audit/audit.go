package audit

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/trading-software-projects/agentic-trading-guardrails/src/pkg/types"
)

const GenesisHash = "sha256:0000000000000000000000000000000000000000000000000000000000000000"

type Category string

const (
	CategoryAuth              Category = "AUTH"
	CategoryToolCall          Category = "TOOL_CALL"
	CategoryPolicyDecision    Category = "POLICY_DECISION"
	CategoryProposal          Category = "PROPOSAL"
	CategoryApproval          Category = "APPROVAL"
	CategoryExecution         Category = "EXECUTION"
	CategoryBrokerResponse    Category = "BROKER_RESPONSE"
	CategorySecurityViolation Category = "SECURITY_VIOLATION"
	CategoryPolicyUpdate      Category = "POLICY_UPDATE"
)

// Event is an immutable, hash-chained audit record with redacted sensitive fields.
type Event struct {
	Seq           uint64         `json:"seq"`
	EventID       string         `json:"event_id"`
	CorrelationID string         `json:"correlation_id"`
	ProposalID    string         `json:"proposal_id,omitempty"`
	ActorID       string         `json:"actor_id,omitempty"`
	PrincipalID   string         `json:"principal_id,omitempty"`
	AccountID     string         `json:"account_id,omitempty"`
	Category      Category       `json:"category"`
	Action        string         `json:"action"`
	Outcome       string         `json:"outcome"`
	PolicyVersion string         `json:"policy_version,omitempty"`
	PolicyHash    string         `json:"policy_hash,omitempty"`
	EventTimeUTC  time.Time      `json:"event_time_utc"`
	ReceivedAtUTC time.Time      `json:"received_at_utc"`
	Details       map[string]any `json:"details,omitempty"`
	PrevHash      string         `json:"prev_hash"`
	EntryHash     string         `json:"entry_hash"`
}

// Query filters immutable audit records.
type Query struct {
	ProposalID    string
	CorrelationID string
	ActorID       string
	AccountID     string
	Category      Category
	Limit         int
}

// ReplaySummary aggregates an end-to-end auditable replay for a proposal or correlation ID.
type ReplaySummary struct {
	ProposalID     string   `json:"proposal_id,omitempty"`
	CorrelationID  string   `json:"correlation_id,omitempty"`
	ActorID        string   `json:"actor_id,omitempty"`
	PrincipalID    string   `json:"principal_id,omitempty"`
	AccountID      string   `json:"account_id,omitempty"`
	PolicyVersions []string `json:"policy_versions"`
	ChainVerified  bool     `json:"chain_verified"`
	EventCount     int      `json:"event_count"`
	Events         []Event  `json:"events"`
}

var sensitiveKeyFragments = []string{
	"secret", "password", "passwd", "api_key", "apikey", "authorization",
	"bearer", "private_key", "broker_key", "broker_secret", "token", "approval_grant",
}

// RedactMap recursively redacts secret keys and credential-like values before writing to audit logs.
func RedactMap(in map[string]any) map[string]any {
	if in == nil {
		return nil
	}
	out := make(map[string]any, len(in))
	for k, v := range in {
		if isSensitiveKey(k) {
			out[k] = "[REDACTED]"
			continue
		}
		out[k] = redactValue(v)
	}
	return out
}

func isSensitiveKey(k string) bool {
	lower := strings.ToLower(k)
	for _, frag := range sensitiveKeyFragments {
		if strings.Contains(lower, frag) {
			return true
		}
	}
	return false
}

func redactValue(v any) any {
	switch val := v.(type) {
	case map[string]any:
		return RedactMap(val)
	case []any:
		cp := make([]any, len(val))
		for i, item := range val {
			cp[i] = redactValue(item)
		}
		return cp
	case string:
		return RedactString(val)
	default:
		return v
	}
}

func RedactString(s string) string {
	lower := strings.ToLower(s)
	if strings.HasPrefix(lower, "bearer ") {
		return "Bearer [REDACTED]"
	}
	if strings.Contains(lower, "sk_live_") || strings.Contains(lower, "sk_test_") || strings.Contains(lower, "paper-sim-local-secret") {
		return "[REDACTED_SECRET]"
	}
	return s
}

func computeEntryHash(e Event) string {
	cp := e
	cp.EntryHash = ""
	raw, _ := json.Marshal(cp)
	sum := sha256.Sum256(raw)
	return "sha256:" + hex.EncodeToString(sum[:])
}

// Logger is an append-only, hash-chained audit log supporting optional JSONL persistence.
type Logger struct {
	mu       sync.RWMutex
	clock    types.Clock
	filePath string
	events   []Event
	lastHash string
}

// NewLogger creates an audit logger. If filePath is non-empty, events are durably appended to JSONL
// and loaded + verified on startup.
func NewLogger(clock types.Clock, filePath string) (*Logger, error) {
	if clock == nil {
		clock = types.SystemClock{}
	}
	l := &Logger{
		clock:    clock,
		filePath: filePath,
		events:   make([]Event, 0, 128),
		lastHash: GenesisHash,
	}
	if filePath != "" {
		if err := os.MkdirAll(filepath.Dir(filePath), 0o750); err != nil {
			return nil, fmt.Errorf("create audit directory: %w", err)
		}
		if err := l.loadFromFile(); err != nil {
			return nil, err
		}
	}
	return l, nil
}

func (l *Logger) loadFromFile() error {
	f, err := os.Open(l.filePath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	// Allow larger audit lines up to 1MB
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		var ev Event
		if err := json.Unmarshal([]byte(line), &ev); err != nil {
			return fmt.Errorf("corrupt audit log line: %w", err)
		}
		l.events = append(l.events, ev)
		l.lastHash = ev.EntryHash
	}
	if err := scanner.Err(); err != nil {
		return err
	}
	return l.VerifyChain()
}

// Append redacts sensitive data, chains the SHA-256 hash, persists to disk if configured, and stores immutably.
func (l *Logger) Append(ev Event) (Event, error) {
	l.mu.Lock()
	defer l.mu.Unlock()

	now := l.clock.Now()
	if ev.ReceivedAtUTC.IsZero() {
		ev.ReceivedAtUTC = now
	} else {
		ev.ReceivedAtUTC = ev.ReceivedAtUTC.UTC()
	}
	if ev.EventTimeUTC.IsZero() {
		ev.EventTimeUTC = ev.ReceivedAtUTC
	} else {
		ev.EventTimeUTC = ev.EventTimeUTC.UTC()
	}
	ev.Seq = uint64(len(l.events) + 1)
	if ev.EventID == "" {
		ev.EventID = fmt.Sprintf("aud-%06d", ev.Seq)
	}
	ev.Details = RedactMap(ev.Details)
	ev.PrevHash = l.lastHash
	ev.EntryHash = computeEntryHash(ev)

	if l.filePath != "" {
		f, err := os.OpenFile(l.filePath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
		if err != nil {
			return Event{}, fmt.Errorf("open audit file: %w", err)
		}
		line, err := json.Marshal(ev)
		if err != nil {
			_ = f.Close()
			return Event{}, err
		}
		if _, err := f.Write(append(line, '\n')); err != nil {
			_ = f.Close()
			return Event{}, fmt.Errorf("write audit record: %w", err)
		}
		if err := f.Sync(); err != nil {
			_ = f.Close()
			return Event{}, fmt.Errorf("fsync audit record: %w", err)
		}
		_ = f.Close()
	}

	l.events = append(l.events, ev)
	l.lastHash = ev.EntryHash
	return ev, nil
}

// VerifyChain validates the cryptographic hash chain across every stored audit record.
func (l *Logger) VerifyChain() error {
	l.mu.RLock()
	defer l.mu.RUnlock()

	expectedPrev := GenesisHash
	for i, ev := range l.events {
		if ev.PrevHash != expectedPrev {
			return fmt.Errorf("audit chain break at seq %d (index %d): expected prev_hash %s, got %s", ev.Seq, i, expectedPrev, ev.PrevHash)
		}
		recomputed := computeEntryHash(ev)
		if ev.EntryHash != recomputed {
			return fmt.Errorf("audit entry tampered at seq %d: expected entry_hash %s, got %s", ev.Seq, recomputed, ev.EntryHash)
		}
		expectedPrev = ev.EntryHash
	}
	return nil
}

// QueryEvents returns immutable copies matching the filter.
func (l *Logger) QueryEvents(q Query) []Event {
	l.mu.RLock()
	defer l.mu.RUnlock()

	results := make([]Event, 0)
	for _, ev := range l.events {
		if q.ProposalID != "" && ev.ProposalID != q.ProposalID {
			continue
		}
		if q.CorrelationID != "" && ev.CorrelationID != q.CorrelationID {
			continue
		}
		if q.ActorID != "" && ev.ActorID != q.ActorID {
			continue
		}
		if q.AccountID != "" && ev.AccountID != q.AccountID {
			continue
		}
		if q.Category != "" && ev.Category != q.Category {
			continue
		}
		results = append(results, ev)
	}
	if q.Limit > 0 && len(results) > q.Limit {
		results = results[len(results)-q.Limit:]
	}
	return results
}

func (l *Logger) Count() int {
	l.mu.RLock()
	defer l.mu.RUnlock()
	return len(l.events)
}

// BuildReplay builds a complete audit/replay timeline for a proposal or correlation ID.
func (l *Logger) BuildReplay(proposalID, correlationID string) ReplaySummary {
	events := l.QueryEvents(Query{ProposalID: proposalID, CorrelationID: correlationID})
	verSet := make(map[string]struct{})
	summary := ReplaySummary{
		ProposalID:     proposalID,
		CorrelationID:  correlationID,
		PolicyVersions: []string{},
		ChainVerified:  l.VerifyChain() == nil,
		EventCount:     len(events),
		Events:         events,
	}
	for _, ev := range events {
		if summary.ActorID == "" && ev.ActorID != "" {
			summary.ActorID = ev.ActorID
		}
		if summary.PrincipalID == "" && ev.PrincipalID != "" {
			summary.PrincipalID = ev.PrincipalID
		}
		if summary.AccountID == "" && ev.AccountID != "" {
			summary.AccountID = ev.AccountID
		}
		if ev.PolicyVersion != "" {
			if _, seen := verSet[ev.PolicyVersion]; !seen {
				verSet[ev.PolicyVersion] = struct{}{}
				summary.PolicyVersions = append(summary.PolicyVersions, ev.PolicyVersion)
			}
		}
	}
	return summary
}
