package issues

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/trading-software-projects/broker-reliability-reconciliation/src/domain"
)

var (
	// ErrNotFound is returned when a requested run or issue does not exist.
	ErrNotFound = errors.New("record not found")
	// ErrInvalidTransition is returned when an issue state transition violates the lifecycle graph.
	ErrInvalidTransition = errors.New("invalid issue state transition")
	// ErrWriteRejected simulates a durable storage failure (disk full, read-only replica, constraint failure).
	ErrWriteRejected = errors.New("storage write rejected")
	// ErrValidation indicates missing required fields on a mutation request.
	ErrValidation = errors.New("validation error")
)

// IssueFilter selects issues from the work queue.
type IssueFilter struct {
	Status    domain.IssueState
	AccountID string
	Kind      domain.IssueKind
	Severity  domain.IssueSeverity
	Owner     string
	RunID     string
}

// Store manages ReconRuns, ReconIssues, Policies, and append-only audit logs.
// Original IssueEvidence is strictly immutable once persisted.
type Store struct {
	mu           sync.RWMutex
	runs         map[string]domain.ReconRun
	issues       map[string]domain.ReconIssue
	policies     map[string]domain.ReconPolicy
	stateDir     string
	rejectWrites bool
	nowFn        func() time.Time
}

// NewStore creates a store; if stateDir is non-empty, runs and issues are durably persisted
// to append-only JSONL files and reloaded automatically on restart.
func NewStore(stateDir string) (*Store, error) {
	s := &Store{
		runs:     make(map[string]domain.ReconRun),
		issues:   make(map[string]domain.ReconIssue),
		policies: make(map[string]domain.ReconPolicy),
		stateDir: stateDir,
		nowFn:    func() time.Time { return time.Now().UTC() },
	}
	def := domain.DefaultPolicy()
	s.policies[def.Version] = def

	if stateDir != "" {
		if err := os.MkdirAll(stateDir, 0o755); err != nil {
			return nil, fmt.Errorf("create state dir %s: %w", stateDir, err)
		}
		if err := s.replayFromDisk(); err != nil {
			return nil, err
		}
	}
	return s, nil
}

// SetRejectWrites toggles simulated storage write rejection for fault/scenario testing.
func (s *Store) SetRejectWrites(reject bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.rejectWrites = reject
}

// RegisterPolicy stores a versioned reconciliation policy.
func (s *Store) RegisterPolicy(p domain.ReconPolicy) error {
	if err := p.Validate(); err != nil {
		return fmt.Errorf("%w: %v", ErrValidation, err)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.policies[p.Version] = p
	return nil
}

// GetPolicy retrieves a policy by version (or the default policy if version is empty).
func (s *Store) GetPolicy(version string) (domain.ReconPolicy, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if strings.TrimSpace(version) == "" {
		return s.policies[domain.DefaultPolicy().Version], nil
	}
	p, ok := s.policies[version]
	if !ok {
		return domain.ReconPolicy{}, fmt.Errorf("%w: policy %q", ErrNotFound, version)
	}
	return p, nil
}

// ListPolicies returns all registered policy versions sorted by version string.
func (s *Store) ListPolicies() []domain.ReconPolicy {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]domain.ReconPolicy, 0, len(s.policies))
	for _, p := range s.policies {
		out = append(out, p)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Version < out[j].Version })
	return out
}

// SaveRun persists a reconciliation run record.
func (s *Store) SaveRun(run domain.ReconRun) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.rejectWrites {
		return ErrWriteRejected
	}
	cp := cloneRun(run)
	if err := s.appendJSONL("runs.jsonl", cp); err != nil {
		return err
	}
	s.runs[cp.ID] = cp
	return nil
}

// GetRun fetches a run by ID.
func (s *Store) GetRun(id string) (domain.ReconRun, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	r, ok := s.runs[id]
	if !ok {
		return domain.ReconRun{}, fmt.Errorf("%w: run %q", ErrNotFound, id)
	}
	return cloneRun(r), nil
}

// ListRuns returns all runs ordered newest first.
func (s *Store) ListRuns() []domain.ReconRun {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]domain.ReconRun, 0, len(s.runs))
	for _, r := range s.runs {
		out = append(out, cloneRun(r))
	}
	sort.Slice(out, func(i, j int) bool {
		if !out[i].StartedAt.Equal(out[j].StartedAt) {
			return out[i].StartedAt.After(out[j].StartedAt)
		}
		return out[i].ID < out[j].ID
	})
	return out
}

// UpsertIssue records a newly detected issue or links a re-observed issue without
// ever mutating or destroying original Evidence, State, Owner, or Resolution.
func (s *Store) UpsertIssue(issue domain.ReconIssue) (domain.ReconIssue, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.rejectWrites {
		return domain.ReconIssue{}, ErrWriteRejected
	}

	if existing, ok := s.issues[issue.ID]; ok {
		// Preserve immutable original evidence, state, owner, resolution, and created_at!
		cp := cloneIssue(existing)
		if existing.RunID != issue.RunID {
			now := s.nowFn().UTC()
			cp.RunID = issue.RunID
			cp.UpdatedAt = now
			cp.History = append(cp.History, domain.IssueHistoryEntry{
				Timestamp: now,
				Actor:     "reconcile-core",
				Action:    "reobserved",
				FromState: cp.State,
				ToState:   cp.State,
				Note:      fmt.Sprintf("Re-observed in run %s (original evidence preserved)", issue.RunID),
			})
			if err := s.appendJSONL("issues.jsonl", cp); err != nil {
				return domain.ReconIssue{}, err
			}
			s.issues[cp.ID] = cp
		}
		return cloneIssue(cp), nil
	}

	cp := cloneIssue(issue)
	if err := s.appendJSONL("issues.jsonl", cp); err != nil {
		return domain.ReconIssue{}, err
	}
	s.issues[cp.ID] = cp
	return cloneIssue(cp), nil
}

// GetIssue fetches an issue by ID.
func (s *Store) GetIssue(id string) (domain.ReconIssue, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	iss, ok := s.issues[id]
	if !ok {
		return domain.ReconIssue{}, fmt.Errorf("%w: issue %q", ErrNotFound, id)
	}
	return cloneIssue(iss), nil
}

// ListIssues returns issues matching the filter, ordered deterministically by severity then created_at.
func (s *Store) ListIssues(filter IssueFilter) []domain.ReconIssue {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var out []domain.ReconIssue
	for _, iss := range s.issues {
		if filter.Status != "" && iss.State != filter.Status {
			continue
		}
		if filter.AccountID != "" && iss.AccountID != filter.AccountID {
			continue
		}
		if filter.Kind != "" && iss.Kind != filter.Kind {
			continue
		}
		if filter.Severity != "" && iss.Severity != filter.Severity {
			continue
		}
		if filter.Owner != "" && iss.Owner != filter.Owner {
			continue
		}
		if filter.RunID != "" && iss.RunID != filter.RunID {
			continue
		}
		out = append(out, cloneIssue(iss))
	}
	sort.Slice(out, func(i, j int) bool {
		ri, rj := sevOrder(out[i].Severity), sevOrder(out[j].Severity)
		if ri != rj {
			return ri < rj
		}
		if out[i].Kind != out[j].Kind {
			return out[i].Kind < out[j].Kind
		}
		return out[i].ID < out[j].ID
	})
	return out
}

// AssignOwner assigns an operator to an issue and appends an audit history entry.
func (s *Store) AssignOwner(issueID, owner, actor string) (domain.ReconIssue, error) {
	owner = strings.TrimSpace(owner)
	if owner == "" {
		return domain.ReconIssue{}, fmt.Errorf("%w: owner is required", ErrValidation)
	}
	if strings.TrimSpace(actor) == "" {
		actor = "operator"
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if s.rejectWrites {
		return domain.ReconIssue{}, ErrWriteRejected
	}
	iss, ok := s.issues[issueID]
	if !ok {
		return domain.ReconIssue{}, fmt.Errorf("%w: issue %q", ErrNotFound, issueID)
	}
	cp := cloneIssue(iss)
	now := s.nowFn().UTC()
	prevOwner := cp.Owner
	cp.Owner = owner
	cp.UpdatedAt = now
	cp.History = append(cp.History, domain.IssueHistoryEntry{
		Timestamp: now,
		Actor:     actor,
		Action:    "assigned",
		FromState: cp.State,
		ToState:   cp.State,
		Note:      fmt.Sprintf("Owner changed from %q to %q", prevOwner, owner),
	})
	if err := s.appendJSONL("issues.jsonl", cp); err != nil {
		return domain.ReconIssue{}, err
	}
	s.issues[cp.ID] = cp
	return cloneIssue(cp), nil
}

// TransitionState moves an issue along valid lifecycle edges.
func (s *Store) TransitionState(issueID string, target domain.IssueState, actor, note string) (domain.ReconIssue, error) {
	if strings.TrimSpace(actor) == "" {
		actor = "operator"
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.rejectWrites {
		return domain.ReconIssue{}, ErrWriteRejected
	}
	iss, ok := s.issues[issueID]
	if !ok {
		return domain.ReconIssue{}, fmt.Errorf("%w: issue %q", ErrNotFound, issueID)
	}
	if !isValidTransition(iss.State, target) {
		return domain.ReconIssue{}, fmt.Errorf("%w: cannot transition from %s to %s", ErrInvalidTransition, iss.State, target)
	}
	cp := cloneIssue(iss)
	now := s.nowFn().UTC()
	from := cp.State
	cp.State = target
	cp.UpdatedAt = now
	action := "state_changed"
	if target == domain.IssueStateOpen && (from == domain.IssueStateResolved || from == domain.IssueStateWaived) {
		action = "reopened"
	}
	cp.History = append(cp.History, domain.IssueHistoryEntry{
		Timestamp: now,
		Actor:     actor,
		Action:    action,
		FromState: from,
		ToState:   target,
		Note:      note,
	})
	if err := s.appendJSONL("issues.jsonl", cp); err != nil {
		return domain.ReconIssue{}, err
	}
	s.issues[cp.ID] = cp
	return cloneIssue(cp), nil
}

// Resolve records an operator resolution while keeping original Evidence completely untouched.
func (s *Store) Resolve(issueID, resolutionCode, evidenceNote, actor string) (domain.ReconIssue, error) {
	resolutionCode = strings.TrimSpace(resolutionCode)
	evidenceNote = strings.TrimSpace(evidenceNote)
	if resolutionCode == "" {
		return domain.ReconIssue{}, fmt.Errorf("%w: resolution code is required", ErrValidation)
	}
	if evidenceNote == "" {
		return domain.ReconIssue{}, fmt.Errorf("%w: resolution evidence is required", ErrValidation)
	}
	if strings.TrimSpace(actor) == "" {
		actor = "operator"
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if s.rejectWrites {
		return domain.ReconIssue{}, ErrWriteRejected
	}
	iss, ok := s.issues[issueID]
	if !ok {
		return domain.ReconIssue{}, fmt.Errorf("%w: issue %q", ErrNotFound, issueID)
	}
	if !isValidTransition(iss.State, domain.IssueStateResolved) {
		return domain.ReconIssue{}, fmt.Errorf("%w: cannot resolve issue in state %s", ErrInvalidTransition, iss.State)
	}

	cp := cloneIssue(iss)
	now := s.nowFn().UTC()
	from := cp.State
	cp.State = domain.IssueStateResolved
	cp.UpdatedAt = now
	cp.Resolution = &domain.IssueResolution{
		Code:       resolutionCode,
		Evidence:   evidenceNote,
		Actor:      actor,
		ResolvedAt: now,
	}
	cp.History = append(cp.History, domain.IssueHistoryEntry{
		Timestamp: now,
		Actor:     actor,
		Action:    "resolved",
		FromState: from,
		ToState:   domain.IssueStateResolved,
		Note:      fmt.Sprintf("[%s] %s", resolutionCode, evidenceNote),
	})
	if err := s.appendJSONL("issues.jsonl", cp); err != nil {
		return domain.ReconIssue{}, err
	}
	s.issues[cp.ID] = cp
	return cloneIssue(cp), nil
}

// IsValidTransition exposes lifecycle validation for tests and callers.
func IsValidTransition(from, to domain.IssueState) bool {
	return isValidTransition(from, to)
}

func isValidTransition(from, to domain.IssueState) bool {
	if from == to {
		return false
	}
	switch from {
	case domain.IssueStateOpen:
		return to == domain.IssueStateInvestigating || to == domain.IssueStateEscalated || to == domain.IssueStateResolved || to == domain.IssueStateWaived
	case domain.IssueStateInvestigating:
		return to == domain.IssueStateOpen || to == domain.IssueStateEscalated || to == domain.IssueStateResolved || to == domain.IssueStateWaived
	case domain.IssueStateEscalated:
		return to == domain.IssueStateInvestigating || to == domain.IssueStateResolved || to == domain.IssueStateWaived
	case domain.IssueStateResolved, domain.IssueStateWaived:
		return to == domain.IssueStateOpen
	default:
		return false
	}
}

func sevOrder(s domain.IssueSeverity) int {
	switch s {
	case domain.SeverityCritical:
		return 0
	case domain.SeverityHigh:
		return 1
	case domain.SeverityMedium:
		return 2
	case domain.SeverityLow:
		return 3
	default:
		return 4
	}
}

func (s *Store) appendJSONL(filename string, v any) error {
	if s.stateDir == "" {
		return nil
	}
	line, err := json.Marshal(v)
	if err != nil {
		return err
	}
	path := filepath.Join(s.stateDir, filename)
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	if _, err := f.Write(append(line, '\n')); err != nil {
		return err
	}
	return f.Sync()
}

func (s *Store) replayFromDisk() error {
	if err := replayFile(filepath.Join(s.stateDir, "runs.jsonl"), func(raw []byte) error {
		var r domain.ReconRun
		if err := json.Unmarshal(raw, &r); err != nil {
			return err
		}
		s.runs[r.ID] = r
		return nil
	}); err != nil {
		return err
	}
	return replayFile(filepath.Join(s.stateDir, "issues.jsonl"), func(raw []byte) error {
		var iss domain.ReconIssue
		if err := json.Unmarshal(raw, &iss); err != nil {
			return err
		}
		s.issues[iss.ID] = iss
		return nil
	})
}

func replayFile(path string, onLine func([]byte) error) error {
	f, err := os.Open(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}
	defer f.Close()
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		if err := onLine([]byte(line)); err != nil {
			return fmt.Errorf("replay %s: %w", path, err)
		}
	}
	return scanner.Err()
}

func cloneRun(r domain.ReconRun) domain.ReconRun {
	cp := r
	if r.IssueIDs != nil {
		cp.IssueIDs = append([]string(nil), r.IssueIDs...)
	}
	if r.Summary.IssuesByKind != nil {
		cp.Summary.IssuesByKind = make(map[string]int, len(r.Summary.IssuesByKind))
		for k, v := range r.Summary.IssuesByKind {
			cp.Summary.IssuesByKind[k] = v
		}
	}
	if r.Summary.IssuesBySeverity != nil {
		cp.Summary.IssuesBySeverity = make(map[string]int, len(r.Summary.IssuesBySeverity))
		for k, v := range r.Summary.IssuesBySeverity {
			cp.Summary.IssuesBySeverity[k] = v
		}
	}
	if r.CompletedAt != nil {
		t := *r.CompletedAt
		cp.CompletedAt = &t
	}
	return cp
}

func cloneIssue(iss domain.ReconIssue) domain.ReconIssue {
	cp := iss
	cp.Evidence = domain.CloneEvidence(iss.Evidence)
	if iss.References != nil {
		cp.References = append([]domain.IssueReference(nil), iss.References...)
	}
	if iss.History != nil {
		cp.History = append([]domain.IssueHistoryEntry(nil), iss.History...)
	}
	if iss.Resolution != nil {
		res := *iss.Resolution
		cp.Resolution = &res
	}
	return cp
}
