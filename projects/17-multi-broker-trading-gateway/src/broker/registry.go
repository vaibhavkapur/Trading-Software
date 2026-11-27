package broker

import (
	"fmt"
	"sort"
	"sync"

	"github.com/vaibhavkapur/Trading-Software/projects/17-multi-broker-trading-gateway/src/domain"
)

// Registry manages registered broker adapters safely by BrokerID.
type Registry struct {
	mu       sync.RWMutex
	adapters map[string]Adapter
}

// NewRegistry initializes an empty adapter registry.
func NewRegistry() *Registry {
	return &Registry{
		adapters: make(map[string]Adapter),
	}
}

// Register adds a broker adapter to the registry.
func (r *Registry) Register(a Adapter) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	id := a.BrokerID()
	if id == "" {
		return domain.NewError(domain.ErrCodeValidation, "adapter broker_id cannot be empty", false)
	}
	r.adapters[id] = a
	return nil
}

// Get returns the adapter registered under brokerID.
func (r *Registry) Get(brokerID string) (Adapter, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	a, ok := r.adapters[brokerID]
	if !ok {
		return nil, domain.NewError(domain.ErrCodeBrokerNotFound, fmt.Sprintf("broker adapter %q is not registered", brokerID), false).
			WithContext(brokerID, "")
	}
	return a, nil
}

// ListIDs returns sorted broker IDs for deterministic iteration.
func (r *Registry) ListIDs() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	ids := make([]string, 0, len(r.adapters))
	for id := range r.adapters {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}

// All returns adapters sorted by BrokerID.
func (r *Registry) All() []Adapter {
	ids := r.ListIDs()
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]Adapter, 0, len(ids))
	for _, id := range ids {
		out = append(out, r.adapters[id])
	}
	return out
}
