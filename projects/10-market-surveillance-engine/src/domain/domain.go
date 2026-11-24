// Package domain holds the canonical records shared by every stage:
// normalized events, feature windows, alerts and cases.
//
// All timestamps are UTC. Two clocks are kept apart everywhere:
// event time (stamped by the venue) drives windows, ordering and rules;
// receive time (stamped locally) is only used for latency and audit.
package domain

import (
	"encoding/json"
	"time"

	"surveillance/src/fixed"
)

// Disclaimer travels with every alert and every evidence export.
const Disclaimer = "This is an investigation lead produced by explicit rules over simulated data. " +
	"It is not a finding, and not proof of manipulation or misconduct by anyone."

type Kind string

const (
	KindOrder Kind = "order"
	KindTrade Kind = "trade"
)

type Action string

const (
	ActionNew    Action = "NEW"
	ActionAmend  Action = "AMEND"
	ActionCancel Action = "CANCEL"
)

type Side string

const (
	Buy  Side = "BUY"
	Sell Side = "SELL"
)

func (s Side) Opposite() Side {
	if s == Buy {
		return Sell
	}
	return Buy
}

// Event is one normalized order message or execution. The spec's
// OrderEvent and TradeEvent are folded into a single record with a Kind
// discriminator so the journal and evidence snapshots hold one shape.
type Event struct {
	ID         string    `json:"event_id"`
	Seq        uint64    `json:"seq"`
	Kind       Kind      `json:"kind"`
	Venue      string    `json:"venue"`
	Symbol     string    `json:"symbol"`
	EventTime  time.Time `json:"event_time"`
	ReceivedAt time.Time `json:"received_at"`
	// Late marks an event that arrived behind the watermark. It is kept
	// for the record but was never shown to a detector.
	Late bool `json:"late,omitempty"`

	// Order messages.
	AccountID string `json:"account_id,omitempty"`
	Owner     string `json:"owner,omitempty"`
	OrderID   string `json:"order_id,omitempty"`
	Action    Action `json:"action,omitempty"`
	Side      Side   `json:"side,omitempty"`

	// Executions.
	TradeID     string `json:"trade_id,omitempty"`
	BuyAccount  string `json:"buy_account,omitempty"`
	SellAccount string `json:"sell_account,omitempty"`
	BuyOwner    string `json:"buy_owner,omitempty"`
	SellOwner   string `json:"sell_owner,omitempty"`
	BuyOrderID  string `json:"buy_order_id,omitempty"`
	SellOrderID string `json:"sell_order_id,omitempty"`

	Qty   int64       `json:"qty,omitempty"`
	Price fixed.Price `json:"price,omitempty"`
}

// TS is the event time in nanoseconds since the epoch.
func (e Event) TS() int64 { return e.EventTime.UnixNano() }

// FeatureWindow is the activity of one account in one symbol over one
// event-time window [Start, End).
type FeatureWindow struct {
	AccountID string    `json:"account_id"`
	Symbol    string    `json:"symbol"`
	Start     time.Time `json:"start"`
	End       time.Time `json:"end"`

	NewOrders int   `json:"new_orders"`
	Amends    int   `json:"amends"`
	Cancels   int   `json:"cancels"`
	Trades    int   `json:"trades"`
	Messages  int   `json:"messages"` // new + amend + cancel
	TradedQty int64 `json:"traded_qty"`

	// CancelRate is cancels / new orders, absent when the window has no
	// new orders. OrderTradeRatio is messages / trades; with no trades it
	// is messages / 1 and NoTrades is set. Both are for display: rules
	// compare the integer counts.
	CancelRate      *float64 `json:"cancel_rate"`
	OrderTradeRatio float64  `json:"order_trade_ratio"`
	NoTrades        bool     `json:"no_trades"`

	EventRefs      []string  `json:"event_refs,omitempty"`
	RefsTruncated  bool      `json:"refs_truncated,omitempty"`
	LastEventID    string    `json:"-"`
	LastEventTime  time.Time `json:"-"`
	LastReceivedAt time.Time `json:"-"`
}

const (
	SeverityLow    = "LOW"
	SeverityMedium = "MEDIUM"
	SeverityHigh   = "HIGH"
)

func SeverityRank(s string) int {
	switch s {
	case SeverityHigh:
		return 3
	case SeverityMedium:
		return 2
	case SeverityLow:
		return 1
	}
	return 0
}

const (
	AlertOpen   = "OPEN"
	AlertInCase = "IN_CASE"
	AlertClosed = "CLOSED"
)

// Alert is an explainable lead. Everything needed to re-check it by hand
// is on the record: which rule and version fired, under which ruleset,
// the thresholds in force, the values observed, the event-time bounds and
// the IDs of the events that make up the evidence.
type Alert struct {
	ID             string `json:"id"`
	RuleID         string `json:"rule_id"`
	RuleVersion    int    `json:"rule_version"`
	RulesetVersion string `json:"ruleset_version"`
	RulesetHash    string `json:"ruleset_hash"`
	Severity       string `json:"severity"`
	Status         string `json:"status"`

	Symbol   string   `json:"symbol"`
	Accounts []string `json:"accounts"`
	DedupKey string   `json:"dedup_key"`

	WindowStart time.Time `json:"window_start"`
	WindowEnd   time.Time `json:"window_end"`

	Thresholds map[string]float64 `json:"thresholds"`
	Observed   map[string]float64 `json:"observed"`

	EventRefs         []string `json:"event_refs"`
	EvidenceTruncated bool     `json:"evidence_truncated,omitempty"`

	Explanation string `json:"explanation"`
	Disclaimer  string `json:"disclaimer"`

	// Occurrences counts detections folded into this alert: 1 plus every
	// repeat that suppression merged instead of raising a new alert.
	Occurrences int `json:"occurrences"`

	TriggerEventID    string    `json:"trigger_event_id"`
	TriggerEventTime  time.Time `json:"trigger_event_time"`
	TriggerReceivedAt time.Time `json:"trigger_received_at"`
	FirstDetectedAt   time.Time `json:"first_detected_at"`
	LastDetectedAt    time.Time `json:"last_detected_at"`

	CaseIDs []string `json:"case_ids,omitempty"`
}

const (
	CaseOpen      = "OPEN"
	CaseInReview  = "IN_REVIEW"
	CaseEscalated = "ESCALATED"
	CaseResolved  = "RESOLVED"
)

// Dispositions an analyst may record. None of them says "manipulation
// confirmed": the strongest outcome here is a referral to people with the
// authority and the data to decide.
const (
	DispFalsePositive   = "FALSE_POSITIVE"
	DispBenignExplained = "BENIGN_EXPLAINED"
	DispInconclusive    = "INCONCLUSIVE"
	DispReferred        = "REFERRED_FOR_REVIEW"
)

func ValidDisposition(d string) bool {
	switch d {
	case DispFalsePositive, DispBenignExplained, DispInconclusive, DispReferred:
		return true
	}
	return false
}

type Note struct {
	At     time.Time `json:"at"`
	Author string    `json:"author"`
	Text   string    `json:"text"`
}

// Snapshot freezes an alert and the full text of every event it cites at
// the moment it was attached to a case. Later changes to the live alert
// do not alter it.
type Snapshot struct {
	TakenAt          time.Time `json:"taken_at"`
	Alert            Alert     `json:"alert"`
	Events           []Event   `json:"events"`
	MissingEventRefs []string  `json:"missing_event_refs,omitempty"`
	Hash             string    `json:"snapshot_hash"`
}

type Case struct {
	ID          string     `json:"id"`
	Title       string     `json:"title"`
	Status      string     `json:"status"`
	AlertIDs    []string   `json:"alert_ids"`
	Priority    string     `json:"priority,omitempty"`
	Assignee    string     `json:"assignee,omitempty"`
	Disposition string     `json:"disposition,omitempty"`
	Summary     string     `json:"summary,omitempty"`
	CreatedBy   string     `json:"created_by"`
	CreatedAt   time.Time  `json:"created_at"`
	UpdatedAt   time.Time  `json:"updated_at"`
	ResolvedAt  *time.Time `json:"resolved_at,omitempty"`
	Notes       []Note     `json:"analyst_notes"`
	Evidence    []Snapshot `json:"evidence,omitempty"`
	// AuditEvents is filled in on single-case reads.
	AuditEvents []AuditEvent `json:"audit_events,omitempty"`
}

// AuditEvent is one link of the append-only, hash-chained case log.
type AuditEvent struct {
	Seq      uint64          `json:"seq"`
	Time     time.Time       `json:"time"`
	Actor    string          `json:"actor"`
	Action   string          `json:"action"`
	CaseID   string          `json:"case_id"`
	Details  json.RawMessage `json:"details"`
	PrevHash string          `json:"prev_hash"`
	Hash     string          `json:"hash"`
}
