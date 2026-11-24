package domain

import (
	"errors"
	"fmt"
	"strings"
	"time"
)

// SourceSystem identifies the origin of a trading record or snapshot.
type SourceSystem string

const (
	SourceInternal SourceSystem = "internal"
	SourceBroker   SourceSystem = "broker"
)

// Side represents order/fill direction.
type Side string

const (
	SideBuy  Side = "BUY"
	SideSell Side = "SELL"
)

// OrderStatus represents normalized lifecycle state for orders.
type OrderStatus string

const (
	OrderStatusNew             OrderStatus = "NEW"
	OrderStatusPartiallyFilled OrderStatus = "PARTIALLY_FILLED"
	OrderStatusFilled          OrderStatus = "FILLED"
	OrderStatusCanceled        OrderStatus = "CANCELED"
	OrderStatusRejected        OrderStatus = "REJECTED"
)

// LedgerOrder represents an internal OMS/ledger order record.
type LedgerOrder struct {
	ID            string      `json:"id"`
	AccountID     string      `json:"account_id"`
	ClientOrderID string      `json:"client_order_id"`
	BrokerOrderID string      `json:"broker_order_id,omitempty"`
	Symbol        string      `json:"symbol"`
	Side          Side        `json:"side"`
	Qty           Decimal     `json:"qty"`
	FilledQty     Decimal     `json:"filled_qty"`
	LimitPrice    Decimal     `json:"limit_price"`
	Currency      string      `json:"currency"`
	Status        OrderStatus `json:"status"`
	EventTime     time.Time   `json:"event_time"`   // Venue / OMS event timestamp (UTC)
	ReceiveTime   time.Time   `json:"receive_time"` // Local ingestion timestamp (UTC)
}

// BrokerOrder represents a broker-reported order record.
type BrokerOrder struct {
	BrokerOrderID string      `json:"broker_order_id"`
	AccountID     string      `json:"account_id"`
	ClientOrderID string      `json:"client_order_id,omitempty"`
	Symbol        string      `json:"symbol"`
	Side          Side        `json:"side"`
	Qty           Decimal     `json:"qty"`
	FilledQty     Decimal     `json:"filled_qty"`
	LimitPrice    Decimal     `json:"limit_price"`
	Currency      string      `json:"currency"`
	Status        OrderStatus `json:"status"`
	EventTime     time.Time   `json:"event_time"`
	ReceiveTime   time.Time   `json:"receive_time"`
}

// LedgerFill represents an internal ledger execution fill.
type LedgerFill struct {
	ID            string    `json:"id"`
	AccountID     string    `json:"account_id"`
	ClientOrderID string    `json:"client_order_id"`
	BrokerOrderID string    `json:"broker_order_id,omitempty"`
	BrokerExecID  string    `json:"broker_exec_id"`
	Symbol        string    `json:"symbol"`
	Side          Side      `json:"side"`
	Qty           Decimal   `json:"qty"`
	Price         Decimal   `json:"price"`
	Fee           Decimal   `json:"fee"`
	Currency      string    `json:"currency"`
	EventTime     time.Time `json:"event_time"`   // Venue execution time (UTC)
	ReceiveTime   time.Time `json:"receive_time"` // Local OMS receive time (UTC)
}

// BrokerFill represents a broker statement or streaming execution fill.
type BrokerFill struct {
	BrokerExecID  string    `json:"broker_exec_id"`
	BrokerOrderID string    `json:"broker_order_id"`
	ClientOrderID string    `json:"client_order_id,omitempty"`
	AccountID     string    `json:"account_id"`
	Symbol        string    `json:"symbol"`
	Side          Side      `json:"side"`
	Qty           Decimal   `json:"qty"`
	Price         Decimal   `json:"price"`
	Fee           Decimal   `json:"fee"`
	Currency      string    `json:"currency"`
	EventTime     time.Time `json:"event_time"`   // Venue execution time (UTC)
	ReceiveTime   time.Time `json:"receive_time"` // Local feed/statement ingestion time (UTC)
}

// PositionSnapshot represents a point-in-time security position from either internal or broker source.
type PositionSnapshot struct {
	Source      SourceSystem `json:"source"`
	AccountID   string       `json:"account_id"`
	Symbol      string       `json:"symbol"`
	Qty         Decimal      `json:"qty"`
	AvgPrice    Decimal      `json:"avg_price"`
	Currency    string       `json:"currency"`
	AsOf        time.Time    `json:"asof"`
	ReceiveTime time.Time    `json:"receive_time"`
}

// CashSnapshot represents point-in-time cash balances.
// SettledCash and PendingCash are explicitly separated; pending/settling balances
// must never be merged into settled cash during reconciliation.
type CashSnapshot struct {
	Source      SourceSystem `json:"source"`
	AccountID   string       `json:"account_id"`
	Currency    string       `json:"currency"`
	SettledCash Decimal      `json:"settled_cash"`
	PendingCash Decimal      `json:"pending_cash"`
	AsOf        time.Time    `json:"asof"`
	ReceiveTime time.Time    `json:"receive_time"`
}

// ReconPolicy defines versioned cutoff, tolerance, and freshness rules.
type ReconPolicy struct {
	Version                   string  `json:"version"`
	Description               string  `json:"description"`
	PriceTolerance            Decimal `json:"price_tolerance"`              // Max allowed abs price delta
	FeeTolerance              Decimal `json:"fee_tolerance"`                // Max allowed abs fee delta
	QtyTolerance              Decimal `json:"qty_tolerance"`                // Max allowed abs qty delta
	CashTolerance             Decimal `json:"cash_tolerance"`               // Max allowed abs settled cash break
	PendingCashTolerance      Decimal `json:"pending_cash_tolerance"`       // Max allowed abs pending cash break
	LateFillThresholdSec      int64   `json:"late_fill_threshold_sec"`      // ReceiveTime - EventTime threshold for late fill
	StaleSnapshotThresholdSec int64   `json:"stale_snapshot_threshold_sec"` // Cutoff - Snapshot.AsOf threshold for staleness
	ClockSkewToleranceSec     int64   `json:"clock_skew_tolerance_sec"`     // Allowed clock skew between event/receive times
}

// DefaultPolicy returns the baseline v1 reconciliation policy.
func DefaultPolicy() ReconPolicy {
	return ReconPolicy{
		Version:                   "v1.0.0",
		Description:               "Default paper reconciliation policy with strict qty, 0.01 cash/fee tolerance",
		PriceTolerance:            MustParseDecimal("0.0001"),
		FeeTolerance:              MustParseDecimal("0.01"),
		QtyTolerance:              Zero(),
		CashTolerance:             MustParseDecimal("0.01"),
		PendingCashTolerance:      MustParseDecimal("0.01"),
		LateFillThresholdSec:      30,
		StaleSnapshotThresholdSec: 300,
		ClockSkewToleranceSec:     5,
	}
}

// Validate enforces numeric and boundary invariants on a ReconPolicy.
func (p ReconPolicy) Validate() error {
	if strings.TrimSpace(p.Version) == "" {
		return errors.New("policy version is required")
	}
	if p.PriceTolerance.Sign() < 0 || p.FeeTolerance.Sign() < 0 || p.QtyTolerance.Sign() < 0 ||
		p.CashTolerance.Sign() < 0 || p.PendingCashTolerance.Sign() < 0 {
		return errors.New("policy tolerances must be non-negative")
	}
	if p.LateFillThresholdSec < 0 || p.StaleSnapshotThresholdSec < 0 || p.ClockSkewToleranceSec < 0 {
		return errors.New("policy time thresholds must be non-negative")
	}
	return nil
}

// RunStatus represents the execution state of a reconciliation run.
type RunStatus string

const (
	RunStatusPending   RunStatus = "pending"
	RunStatusRunning   RunStatus = "running"
	RunStatusCompleted RunStatus = "completed"
	RunStatusFailed    RunStatus = "failed"
)

// RunSummary summarizes findings of a single reconciliation run.
type RunSummary struct {
	LedgerOrdersCount   int            `json:"ledger_orders_count"`
	BrokerOrdersCount   int            `json:"broker_orders_count"`
	LedgerFillsCount    int            `json:"ledger_fills_count"`
	BrokerFillsCount    int            `json:"broker_fills_count"`
	PositionsChecked    int            `json:"positions_checked"`
	CashBalancesChecked int            `json:"cash_balances_checked"`
	TotalIssues         int            `json:"total_issues"`
	IssuesByKind        map[string]int `json:"issues_by_kind"`
	IssuesBySeverity    map[string]int `json:"issues_by_severity"`
	MonetaryExposure    Decimal        `json:"monetary_exposure"`
	TotalPositionDrift  Decimal        `json:"total_position_drift"`
}

// ReconRun records a single scheduled or on-demand reconciliation execution.
type ReconRun struct {
	ID            string     `json:"id"`
	AccountID     string     `json:"account_id"`
	Cutoff        time.Time  `json:"cutoff"`
	PolicyVersion string     `json:"policy_version"`
	Trigger       string     `json:"trigger"` // "on_demand" or "scheduled"
	Status        RunStatus  `json:"status"`
	InputDigest   string     `json:"input_digest"`
	IssueIDs      []string   `json:"issue_ids"`
	Summary       RunSummary `json:"summary"`
	StartedAt     time.Time  `json:"started_at"`
	CompletedAt   *time.Time `json:"completed_at,omitempty"`
	Error         string     `json:"error,omitempty"`
}

// IssueKind classifies the discrepancy type detected by reconcile-core.
type IssueKind string

const (
	IssueKindMissingBrokerFill   IssueKind = "missing_broker_fill"
	IssueKindMissingLedgerFill   IssueKind = "missing_ledger_fill"
	IssueKindDuplicateBrokerFill IssueKind = "duplicate_broker_fill"
	IssueKindDuplicateLedgerFill IssueKind = "duplicate_ledger_fill"
	IssueKindLateFill            IssueKind = "late_fill"
	IssueKindFillMismatch        IssueKind = "fill_mismatch"
	IssueKindMissingBrokerOrder  IssueKind = "missing_broker_order"
	IssueKindMissingLedgerOrder  IssueKind = "missing_ledger_order"
	IssueKindOrderMismatch       IssueKind = "order_mismatch"
	IssueKindPositionBreak       IssueKind = "position_break"
	IssueKindSettledCashBreak    IssueKind = "settled_cash_break"
	IssueKindPendingCashBreak    IssueKind = "pending_cash_break"
	IssueKindStaleSnapshot       IssueKind = "stale_snapshot"
	IssueKindClockSkewAnomaly    IssueKind = "clock_skew_anomaly"
)

// IssueSeverity ranks operational urgency.
type IssueSeverity string

const (
	SeverityCritical IssueSeverity = "critical"
	SeverityHigh     IssueSeverity = "high"
	SeverityMedium   IssueSeverity = "medium"
	SeverityLow      IssueSeverity = "low"
)

// IssueState tracks exception work-queue lifecycle.
type IssueState string

const (
	IssueStateOpen          IssueState = "open"
	IssueStateInvestigating IssueState = "investigating"
	IssueStateEscalated     IssueState = "escalated"
	IssueStateResolved      IssueState = "resolved"
	IssueStateWaived        IssueState = "waived"
)

// IssueReference links an issue to underlying internal/broker entities.
type IssueReference struct {
	System string `json:"system"` // "internal" or "broker"
	Entity string `json:"entity"` // "order", "fill", "position", "cash"
	ID     string `json:"id"`
}

// IssueEvidence holds the immutable snapshot captured when the discrepancy was detected.
// Operators and resolutions must NEVER mutate or erase OriginalEvidence.
type IssueEvidence struct {
	Summary       string            `json:"summary"`
	InternalData  map[string]string `json:"internal_data,omitempty"`
	BrokerData    map[string]string `json:"broker_data,omitempty"`
	Deltas        map[string]string `json:"deltas,omitempty"`
	PolicyVersion string            `json:"policy_version"`
	Cutoff        time.Time         `json:"cutoff"`
	EventTime     *time.Time        `json:"event_time,omitempty"`
	ReceiveTime   *time.Time        `json:"receive_time,omitempty"`
	CapturedAt    time.Time         `json:"captured_at"`
}

// IssueResolution records an operator-approved resolution action without altering ledger history.
type IssueResolution struct {
	Code       string    `json:"code"`     // e.g. "late_broker_event", "duplicate_ignored", "journal_adjustment_proposed"
	Evidence   string    `json:"evidence"` // Operator justification / ticket reference
	Actor      string    `json:"actor"`
	ResolvedAt time.Time `json:"resolved_at"`
}

// IssueHistoryEntry is an append-only audit log event on an issue.
type IssueHistoryEntry struct {
	Timestamp time.Time  `json:"timestamp"`
	Actor     string     `json:"actor"`
	Action    string     `json:"action"` // "created", "assigned", "state_changed", "resolved", "reopened", "reobserved"
	FromState IssueState `json:"from_state,omitempty"`
	ToState   IssueState `json:"to_state,omitempty"`
	Note      string     `json:"note,omitempty"`
}

// ReconIssue represents a tracked discrepancy in the exception work queue.
type ReconIssue struct {
	ID               string              `json:"id"`
	RunID            string              `json:"run_id"`
	AccountID        string              `json:"account_id"`
	Kind             IssueKind           `json:"kind"`
	Severity         IssueSeverity       `json:"severity"`
	Symbol           string              `json:"symbol,omitempty"`
	Currency         string              `json:"currency,omitempty"`
	MonetaryExposure Decimal             `json:"monetary_exposure"`
	PositionDrift    Decimal             `json:"position_drift"`
	References       []IssueReference    `json:"references"`
	State            IssueState          `json:"state"`
	Owner            string              `json:"owner,omitempty"`
	Evidence         IssueEvidence       `json:"evidence"` // Immutable original evidence
	Resolution       *IssueResolution    `json:"resolution,omitempty"`
	History          []IssueHistoryEntry `json:"history"`
	CreatedAt        time.Time           `json:"created_at"`
	UpdatedAt        time.Time           `json:"updated_at"`
}

// ValidateLedgerFill enforces basic invariants on a LedgerFill.
func (f LedgerFill) Validate() error {
	if strings.TrimSpace(f.ID) == "" {
		return errors.New("ledger fill id is required")
	}
	if strings.TrimSpace(f.AccountID) == "" {
		return errors.New("ledger fill account_id is required")
	}
	if f.Qty.Sign() <= 0 {
		return fmt.Errorf("ledger fill %s qty must be positive, got %s", f.ID, f.Qty.String())
	}
	if f.Price.Sign() < 0 {
		return fmt.Errorf("ledger fill %s price must be non-negative, got %s", f.ID, f.Price.String())
	}
	if f.EventTime.IsZero() {
		return fmt.Errorf("ledger fill %s event_time is required", f.ID)
	}
	return nil
}

// ValidateBrokerFill enforces basic invariants on a BrokerFill.
func (f BrokerFill) Validate() error {
	if strings.TrimSpace(f.BrokerExecID) == "" {
		return errors.New("broker fill broker_exec_id is required")
	}
	if strings.TrimSpace(f.AccountID) == "" {
		return errors.New("broker fill account_id is required")
	}
	if f.Qty.Sign() <= 0 {
		return fmt.Errorf("broker fill %s qty must be positive, got %s", f.BrokerExecID, f.Qty.String())
	}
	if f.Price.Sign() < 0 {
		return fmt.Errorf("broker fill %s price must be non-negative, got %s", f.BrokerExecID, f.Price.String())
	}
	if f.EventTime.IsZero() {
		return fmt.Errorf("broker fill %s event_time is required", f.BrokerExecID)
	}
	return nil
}

// CloneEvidence returns a deep copy of IssueEvidence to enforce immutability across callers.
func CloneEvidence(e IssueEvidence) IssueEvidence {
	out := e
	if e.InternalData != nil {
		out.InternalData = make(map[string]string, len(e.InternalData))
		for k, v := range e.InternalData {
			out.InternalData[k] = v
		}
	}
	if e.BrokerData != nil {
		out.BrokerData = make(map[string]string, len(e.BrokerData))
		for k, v := range e.BrokerData {
			out.BrokerData[k] = v
		}
	}
	if e.Deltas != nil {
		out.Deltas = make(map[string]string, len(e.Deltas))
		for k, v := range e.Deltas {
			out.Deltas[k] = v
		}
	}
	if e.EventTime != nil {
		t := *e.EventTime
		out.EventTime = &t
	}
	if e.ReceiveTime != nil {
		t := *e.ReceiveTime
		out.ReceiveTime = &t
	}
	return out
}
