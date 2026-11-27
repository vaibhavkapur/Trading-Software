package storage

import (
	"sort"
	"sync"

	"github.com/vaibhavkapur/Trading-Software/projects/17-multi-broker-trading-gateway/src/domain"
)

// MemoryStore is a concurrency-safe transactional in-memory Store.
type MemoryStore struct {
	mu          sync.RWMutex
	orders      map[string]domain.Order
	byClientID  map[string]string // clientOrderID -> orderID
	byBrokerKey map[string]string // brokerID|brokerOrderID -> orderID
	fillsByOrd  map[string][]domain.Fill
	fillDedup   map[string]struct{} // brokerID|brokerFillID
	whDedup     map[string]struct{} // brokerID|eventID
	reports     []domain.ReconciliationReport
	cursors     map[string]int64
}

// NewMemoryStore initializes a MemoryStore.
func NewMemoryStore() *MemoryStore {
	return &MemoryStore{
		orders:      make(map[string]domain.Order),
		byClientID:  make(map[string]string),
		byBrokerKey: make(map[string]string),
		fillsByOrd:  make(map[string][]domain.Fill),
		fillDedup:   make(map[string]struct{}),
		whDedup:     make(map[string]struct{}),
		cursors:     make(map[string]int64),
	}
}

func (s *MemoryStore) SaveOrder(order domain.Order) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.orders[order.OrderID] = order
	s.byClientID[order.ClientOrderID] = order.OrderID
	if order.BrokerOrderID != "" {
		s.byBrokerKey[order.BrokerID+"|"+order.BrokerOrderID] = order.OrderID
	}
	return nil
}

func (s *MemoryStore) GetOrder(orderID string) (domain.Order, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	o, ok := s.orders[orderID]
	return o, ok
}

func (s *MemoryStore) GetOrderByClientID(clientOrderID string) (domain.Order, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	oid, ok := s.byClientID[clientOrderID]
	if !ok {
		return domain.Order{}, false
	}
	o, ok := s.orders[oid]
	return o, ok
}

func (s *MemoryStore) GetOrderByBrokerOrderID(brokerID, brokerOrderID string) (domain.Order, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	oid, ok := s.byBrokerKey[brokerID+"|"+brokerOrderID]
	if !ok {
		return domain.Order{}, false
	}
	o, ok := s.orders[oid]
	return o, ok
}

func (s *MemoryStore) ListOrders(brokerID string) []domain.Order {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]domain.Order, 0, len(s.orders))
	for _, o := range s.orders {
		if brokerID == "" || o.BrokerID == brokerID {
			out = append(out, o)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if !out[i].CreatedAtUTC.Equal(out[j].CreatedAtUTC) {
			return out[i].CreatedAtUTC.Before(out[j].CreatedAtUTC)
		}
		return out[i].OrderID < out[j].OrderID
	})
	return out
}

func (s *MemoryStore) RecordFill(fill domain.Fill) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	key := fill.BrokerID + "|" + fill.BrokerFillID
	if _, exists := s.fillDedup[key]; exists {
		return false, nil
	}
	s.fillDedup[key] = struct{}{}
	list := append(s.fillsByOrd[fill.OrderID], fill)
	// Always keep fills ordered by canonical Sequence then ExecutedAtUTC for deterministic VWAP & replay.
	sort.Slice(list, func(i, j int) bool {
		if list[i].Sequence != list[j].Sequence {
			return list[i].Sequence < list[j].Sequence
		}
		if !list[i].ExecutedAtUTC.Equal(list[j].ExecutedAtUTC) {
			return list[i].ExecutedAtUTC.Before(list[j].ExecutedAtUTC)
		}
		return list[i].BrokerFillID < list[j].BrokerFillID
	})
	s.fillsByOrd[fill.OrderID] = list
	return true, nil
}

func (s *MemoryStore) ListFillsByOrder(orderID string) []domain.Fill {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return append([]domain.Fill(nil), s.fillsByOrd[orderID]...)
}

func (s *MemoryStore) MarkWebhookProcessed(brokerID, eventID string) (bool, error) {
	if eventID == "" {
		return true, nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	key := brokerID + "|" + eventID
	if _, exists := s.whDedup[key]; exists {
		return false, nil
	}
	s.whDedup[key] = struct{}{}
	return true, nil
}

func (s *MemoryStore) SaveReconciliationReport(report domain.ReconciliationReport) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.reports = append(s.reports, report)
	return nil
}

func (s *MemoryStore) ListReconciliationReports() []domain.ReconciliationReport {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return append([]domain.ReconciliationReport(nil), s.reports...)
}

func (s *MemoryStore) GetReplayCursor(brokerID string) int64 {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.cursors[brokerID]
}

func (s *MemoryStore) SetReplayCursor(brokerID string, cursor int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.cursors[brokerID] = cursor
	return nil
}
