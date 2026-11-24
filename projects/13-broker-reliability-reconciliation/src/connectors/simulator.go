package connectors

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"sync"
	"time"

	"github.com/trading-software-projects/broker-reliability-reconciliation/src/domain"
)

// FixtureDataset holds deterministic paper/simulated internal and broker data.
type FixtureDataset struct {
	Label         string                    `json:"label"`
	Description   string                    `json:"description"`
	AccountID     string                    `json:"account_id"`
	DefaultAsOf   time.Time                 `json:"default_asof"`
	LedgerOrders  []domain.LedgerOrder      `json:"ledger_orders"`
	BrokerOrders  []domain.BrokerOrder      `json:"broker_orders"`
	LedgerFills   []domain.LedgerFill       `json:"ledger_fills"`
	BrokerFills   []domain.BrokerFill       `json:"broker_fills"`
	InternalPos   []domain.PositionSnapshot `json:"internal_positions"`
	BrokerPos     []domain.PositionSnapshot `json:"broker_positions"`
	InternalCash  []domain.CashSnapshot     `json:"internal_cash"`
	BrokerCash    []domain.CashSnapshot     `json:"broker_cash"`
	SymbolAliases map[string]string         `json:"symbol_aliases,omitempty"`
}

// LoadFixture reads a FixtureDataset from a JSON file on disk.
func LoadFixture(path string) (*FixtureDataset, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read fixture %s: %w", path, err)
	}
	var ds FixtureDataset
	if err := json.Unmarshal(raw, &ds); err != nil {
		return nil, fmt.Errorf("parse fixture %s: %w", path, err)
	}
	return &ds, nil
}

// FaultProfile enables controlled fault injection for failure demos and scenario tests.
type FaultProfile struct {
	Disconnect           bool          // Permanent ErrDisconnected
	TransientFailures    int           // Return ErrRateLimited for first N calls, then succeed
	DropBrokerFillIDs    []string      // Simulate lost webhook/statement fills
	DuplicateBrokerIDs   []string      // Simulate duplicate delivery of these broker_exec_ids
	StaleSnapshotShift   time.Duration // Shift broker position/cash AsOf backwards to simulate staleness
	ClockSkewShift       time.Duration // Shift broker fill EventTime forward to simulate clock skew
	LateFillDelay        time.Duration // Shift broker fill ReceiveTime forward to simulate late arrival
	LateFillTargetExecID string        // Optional single fill target for LateFillDelay (all if empty)
}

// SimulatedInternalConnector serves internal OMS/ledger fixtures with cursor pagination.
type SimulatedInternalConnector struct {
	name     string
	dataset  *FixtureDataset
	pageSize int

	mu          sync.Mutex
	faults      FaultProfile
	failedCalls int
}

// NewSimulatedInternalConnector creates a read-only simulated internal connector.
func NewSimulatedInternalConnector(ds *FixtureDataset, pageSize int) *SimulatedInternalConnector {
	if pageSize <= 0 {
		pageSize = 50
	}
	return &SimulatedInternalConnector{
		name:     "simulated-internal-oms",
		dataset:  ds,
		pageSize: pageSize,
	}
}

// SetFaults updates the active fault injection profile.
func (c *SimulatedInternalConnector) SetFaults(f FaultProfile) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.faults = f
	c.failedCalls = 0
}

func (c *SimulatedInternalConnector) Name() string { return c.name }

func (c *SimulatedInternalConnector) checkFault() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.faults.Disconnect {
		return ErrDisconnected
	}
	if c.failedCalls < c.faults.TransientFailures {
		c.failedCalls++
		return ErrRateLimited
	}
	return nil
}

func paginateSlice[T any](items []T, req PageRequest, defaultSize int) (PageResponse[T], error) {
	start := 0
	if req.Cursor != "" {
		idx, err := strconv.Atoi(req.Cursor)
		if err != nil || idx < 0 || idx > len(items) {
			return PageResponse[T]{}, fmt.Errorf("%w: %q", ErrInvalidCursor, req.Cursor)
		}
		start = idx
	}
	limit := req.Limit
	if limit <= 0 {
		limit = defaultSize
	}
	end := start + limit
	if end > len(items) {
		end = len(items)
	}
	out := make([]T, end-start)
	copy(out, items[start:end])
	hasMore := end < len(items)
	nextCursor := ""
	if hasMore {
		nextCursor = strconv.Itoa(end)
	}
	return PageResponse[T]{
		Items:      out,
		NextCursor: nextCursor,
		HasMore:    hasMore,
	}, nil
}

func (c *SimulatedInternalConnector) ListOrders(_ context.Context, req PageRequest) (PageResponse[domain.LedgerOrder], error) {
	if err := c.checkFault(); err != nil {
		return PageResponse[domain.LedgerOrder]{}, err
	}
	var filtered []domain.LedgerOrder
	for _, o := range c.dataset.LedgerOrders {
		if req.AccountID != "" && o.AccountID != req.AccountID {
			continue
		}
		filtered = append(filtered, o)
	}
	return paginateSlice(filtered, req, c.pageSize)
}

func (c *SimulatedInternalConnector) ListFills(_ context.Context, req PageRequest) (PageResponse[domain.LedgerFill], error) {
	if err := c.checkFault(); err != nil {
		return PageResponse[domain.LedgerFill]{}, err
	}
	var filtered []domain.LedgerFill
	for _, f := range c.dataset.LedgerFills {
		if req.AccountID != "" && f.AccountID != req.AccountID {
			continue
		}
		filtered = append(filtered, f)
	}
	return paginateSlice(filtered, req, c.pageSize)
}

func (c *SimulatedInternalConnector) ListPositions(_ context.Context, req PageRequest) (PageResponse[domain.PositionSnapshot], error) {
	if err := c.checkFault(); err != nil {
		return PageResponse[domain.PositionSnapshot]{}, err
	}
	var filtered []domain.PositionSnapshot
	for _, p := range c.dataset.InternalPos {
		if req.AccountID != "" && p.AccountID != req.AccountID {
			continue
		}
		filtered = append(filtered, p)
	}
	return paginateSlice(filtered, req, c.pageSize)
}

func (c *SimulatedInternalConnector) ListCashBalances(_ context.Context, req PageRequest) (PageResponse[domain.CashSnapshot], error) {
	if err := c.checkFault(); err != nil {
		return PageResponse[domain.CashSnapshot]{}, err
	}
	var filtered []domain.CashSnapshot
	for _, cs := range c.dataset.InternalCash {
		if req.AccountID != "" && cs.AccountID != req.AccountID {
			continue
		}
		filtered = append(filtered, cs)
	}
	return paginateSlice(filtered, req, c.pageSize)
}

// SimulatedBrokerConnector serves simulated broker API/statement fixtures with pagination and fault injection.
type SimulatedBrokerConnector struct {
	name     string
	dataset  *FixtureDataset
	pageSize int

	mu          sync.Mutex
	faults      FaultProfile
	failedCalls int
}

// NewSimulatedBrokerConnector creates a read-only simulated paper broker connector.
func NewSimulatedBrokerConnector(ds *FixtureDataset, pageSize int) *SimulatedBrokerConnector {
	if pageSize <= 0 {
		pageSize = 50
	}
	return &SimulatedBrokerConnector{
		name:     "simulated-paper-broker",
		dataset:  ds,
		pageSize: pageSize,
	}
}

// SetFaults updates the active fault injection profile.
func (c *SimulatedBrokerConnector) SetFaults(f FaultProfile) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.faults = f
	c.failedCalls = 0
}

// Faults returns a copy of the active fault profile.
func (c *SimulatedBrokerConnector) Faults() FaultProfile {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.faults
}

func (c *SimulatedBrokerConnector) Name() string { return c.name }

func (c *SimulatedBrokerConnector) checkFault() (FaultProfile, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.faults.Disconnect {
		return c.faults, ErrDisconnected
	}
	if c.failedCalls < c.faults.TransientFailures {
		c.failedCalls++
		return c.faults, ErrRateLimited
	}
	return c.faults, nil
}

func (c *SimulatedBrokerConnector) ListOrders(_ context.Context, req PageRequest) (PageResponse[domain.BrokerOrder], error) {
	if _, err := c.checkFault(); err != nil {
		return PageResponse[domain.BrokerOrder]{}, err
	}
	var filtered []domain.BrokerOrder
	for _, o := range c.dataset.BrokerOrders {
		if req.AccountID != "" && o.AccountID != req.AccountID {
			continue
		}
		filtered = append(filtered, o)
	}
	return paginateSlice(filtered, req, c.pageSize)
}

func (c *SimulatedBrokerConnector) ListFills(_ context.Context, req PageRequest) (PageResponse[domain.BrokerFill], error) {
	faults, err := c.checkFault()
	if err != nil {
		return PageResponse[domain.BrokerFill]{}, err
	}
	dropSet := make(map[string]bool, len(faults.DropBrokerFillIDs))
	for _, id := range faults.DropBrokerFillIDs {
		dropSet[id] = true
	}
	dupSet := make(map[string]bool, len(faults.DuplicateBrokerIDs))
	for _, id := range faults.DuplicateBrokerIDs {
		dupSet[id] = true
	}

	var filtered []domain.BrokerFill
	for _, f := range c.dataset.BrokerFills {
		if req.AccountID != "" && f.AccountID != req.AccountID {
			continue
		}
		if dropSet[f.BrokerExecID] {
			continue
		}
		item := f
		if faults.ClockSkewShift != 0 {
			item.EventTime = item.EventTime.Add(faults.ClockSkewShift)
		}
		if faults.LateFillDelay != 0 && (faults.LateFillTargetExecID == "" || faults.LateFillTargetExecID == item.BrokerExecID) {
			item.ReceiveTime = item.ReceiveTime.Add(faults.LateFillDelay)
		}
		filtered = append(filtered, item)
		if dupSet[item.BrokerExecID] {
			filtered = append(filtered, item)
		}
	}
	return paginateSlice(filtered, req, c.pageSize)
}

func (c *SimulatedBrokerConnector) ListPositions(_ context.Context, req PageRequest) (PageResponse[domain.PositionSnapshot], error) {
	faults, err := c.checkFault()
	if err != nil {
		return PageResponse[domain.PositionSnapshot]{}, err
	}
	var filtered []domain.PositionSnapshot
	for _, p := range c.dataset.BrokerPos {
		if req.AccountID != "" && p.AccountID != req.AccountID {
			continue
		}
		item := p
		if faults.StaleSnapshotShift != 0 {
			item.AsOf = item.AsOf.Add(-faults.StaleSnapshotShift)
		}
		filtered = append(filtered, item)
	}
	return paginateSlice(filtered, req, c.pageSize)
}

func (c *SimulatedBrokerConnector) ListCashBalances(_ context.Context, req PageRequest) (PageResponse[domain.CashSnapshot], error) {
	faults, err := c.checkFault()
	if err != nil {
		return PageResponse[domain.CashSnapshot]{}, err
	}
	var filtered []domain.CashSnapshot
	for _, cs := range c.dataset.BrokerCash {
		if req.AccountID != "" && cs.AccountID != req.AccountID {
			continue
		}
		item := cs
		if faults.StaleSnapshotShift != 0 {
			item.AsOf = item.AsOf.Add(-faults.StaleSnapshotShift)
		}
		filtered = append(filtered, item)
	}
	return paginateSlice(filtered, req, c.pageSize)
}
