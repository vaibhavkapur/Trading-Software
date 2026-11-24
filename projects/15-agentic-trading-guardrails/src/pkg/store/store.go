package store

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/trading-software-projects/agentic-trading-guardrails/src/pkg/types"
)

var (
	ErrProposalNotFound = errors.New("order proposal not found")
	ErrProposalExists   = errors.New("order proposal already exists")
)

type persistedState struct {
	Proposals       map[string]types.OrderProposal `json:"proposals"`
	DailyNotional   map[string]types.MoneyCents    `json:"daily_notional"`
	ConsumedGrants  map[string]time.Time           `json:"consumed_grants"`
	ClientRequestID map[string]string              `json:"client_request_id"`
}

// Store persists proposals, daily notional usage, and consumed approval grants across restarts.
type Store struct {
	mu       sync.RWMutex
	filePath string
	state    persistedState
}

func NewStore(filePath string) (*Store, error) {
	s := &Store{
		filePath: filePath,
		state: persistedState{
			Proposals:       make(map[string]types.OrderProposal),
			DailyNotional:   make(map[string]types.MoneyCents),
			ConsumedGrants:  make(map[string]time.Time),
			ClientRequestID: make(map[string]string),
		},
	}
	if filePath != "" {
		if err := os.MkdirAll(filepath.Dir(filePath), 0o750); err != nil {
			return nil, fmt.Errorf("create store dir: %w", err)
		}
		if err := s.load(); err != nil {
			return nil, err
		}
	}
	return s, nil
}

func (s *Store) load() error {
	raw, err := os.ReadFile(s.filePath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}
	if len(raw) == 0 {
		return nil
	}
	var st persistedState
	if err := json.Unmarshal(raw, &st); err != nil {
		return fmt.Errorf("decode state store: %w", err)
	}
	if st.Proposals == nil {
		st.Proposals = make(map[string]types.OrderProposal)
	}
	if st.DailyNotional == nil {
		st.DailyNotional = make(map[string]types.MoneyCents)
	}
	if st.ConsumedGrants == nil {
		st.ConsumedGrants = make(map[string]time.Time)
	}
	if st.ClientRequestID == nil {
		st.ClientRequestID = make(map[string]string)
	}
	s.state = st
	return nil
}

func (s *Store) flushLocked() error {
	if s.filePath == "" {
		return nil
	}
	raw, err := json.MarshalIndent(s.state, "", "  ")
	if err != nil {
		return err
	}
	tmp := s.filePath + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, s.filePath)
}

func (s *Store) SaveProposal(p types.OrderProposal) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, exists := s.state.Proposals[p.ID]; exists {
		return ErrProposalExists
	}
	s.state.Proposals[p.ID] = p
	if p.ClientRequestID != "" {
		s.state.ClientRequestID[p.ClientRequestID] = p.ID
	}
	return s.flushLocked()
}

func (s *Store) UpdateProposal(p types.OrderProposal) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, exists := s.state.Proposals[p.ID]; !exists {
		return ErrProposalNotFound
	}
	s.state.Proposals[p.ID] = p
	return s.flushLocked()
}

func (s *Store) GetProposal(id string) (types.OrderProposal, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	p, ok := s.state.Proposals[id]
	if !ok {
		return types.OrderProposal{}, ErrProposalNotFound
	}
	return p, nil
}

func (s *Store) ListProposals() []types.OrderProposal {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]types.OrderProposal, 0, len(s.state.Proposals))
	for _, p := range s.state.Proposals {
		out = append(out, p)
	}
	return out
}

func (s *Store) FindByClientRequestID(reqID string) (types.OrderProposal, bool) {
	if reqID == "" {
		return types.OrderProposal{}, false
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	pid, ok := s.state.ClientRequestID[reqID]
	if !ok {
		return types.OrderProposal{}, false
	}
	p, ok := s.state.Proposals[pid]
	return p, ok
}

func dailyKey(accountID string, utc time.Time) string {
	return accountID + "|" + utc.UTC().Format("2006-01-02")
}

func (s *Store) GetDailyNotional(accountID string, utc time.Time) types.MoneyCents {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.state.DailyNotional[dailyKey(accountID, utc)]
}

func (s *Store) AddDailyNotional(accountID string, utc time.Time, delta types.MoneyCents) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	k := dailyKey(accountID, utc)
	updated, err := types.SafeAddCents(s.state.DailyNotional[k], delta)
	if err != nil {
		return err
	}
	s.state.DailyNotional[k] = updated
	return s.flushLocked()
}

func (s *Store) RecordConsumedGrant(grantID string, consumedAt time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.state.ConsumedGrants[grantID] = consumedAt.UTC()
	return s.flushLocked()
}

func (s *Store) ConsumedGrants() map[string]time.Time {
	s.mu.RLock()
	defer s.mu.RUnlock()
	cp := make(map[string]time.Time, len(s.state.ConsumedGrants))
	for k, v := range s.state.ConsumedGrants {
		cp[k] = v
	}
	return cp
}
