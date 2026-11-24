package domain

import (
	"encoding/json"
	"time"

	"oms/src/fixed"
)

// EventType names a fact about one order.
type EventType string

const (
	EvOrderCreated      EventType = "OrderCreated"
	EvOrderValidated    EventType = "OrderValidated"
	EvOrderRejected     EventType = "OrderRejected"
	EvSubmissionStarted EventType = "SubmissionStarted"
	EvOrderAcknowledged EventType = "OrderAcknowledged"
	EvCancelRequested   EventType = "CancelRequested"
	EvCancelRejected    EventType = "CancelRejected"
	EvOrderCancelled    EventType = "OrderCancelled"
	EvReplaceRequested  EventType = "ReplaceRequested"
	EvReplaceRejected   EventType = "ReplaceRejected"
	EvOrderReplaced     EventType = "OrderReplaced"
	EvOrderExpired      EventType = "OrderExpired"

	// EvExecutionRecorded is a new fill reported by the broker.
	EvExecutionRecorded EventType = "ExecutionRecorded"
	// EvExecutionCorrected is the broker restating an earlier execution. It is
	// one of the two documented correction events that may move an order
	// backwards.
	EvExecutionCorrected EventType = "ExecutionCorrected"
	// EvExecutionBusted is the broker cancelling an earlier execution. It is
	// the other documented correction event.
	EvExecutionBusted EventType = "ExecutionBusted"
	// EvExecutionAccepted is an operator explicitly accepting an execution
	// that the OMS had quarantined because it would overfill the order. It is
	// the only event that may take filled quantity above order quantity.
	EvExecutionAccepted EventType = "ExecutionAccepted"
)

// IsExecution reports whether the event changes the order's filled totals.
func (t EventType) IsExecution() bool {
	switch t {
	case EvExecutionRecorded, EvExecutionCorrected, EvExecutionBusted, EvExecutionAccepted:
		return true
	}
	return false
}

// IsCorrection reports whether the event is a documented correction that is
// allowed to move an order to an earlier state.
func (t EventType) IsCorrection() bool {
	return t == EvExecutionCorrected || t == EvExecutionBusted
}

// Source says which part of the system produced an event.
type Source string

const (
	SourceAPI        Source = "API"        // a client command
	SourceOutbox     Source = "OUTBOX"     // the dispatcher talking to the broker
	SourceBroker     Source = "BROKER"     // a broker webhook
	SourceReconciler Source = "RECONCILER" // a broker snapshot, during reconciliation
	SourceOperator   Source = "OPERATOR"   // a manual repair
)

// Event is one immutable entry in an order's history.
type Event struct {
	// Seq is the global append position, assigned by the store.
	Seq     int64     `json:"seq"`
	OrderID string    `json:"order_id"`
	Version int64     `json:"version"`
	Type    EventType `json:"type"`
	Source  Source    `json:"source"`
	// Actor is the authenticated principal for API and operator events.
	Actor string `json:"actor,omitempty"`
	// CausationID links the event to what triggered it: an idempotency key, an
	// outbox row, a broker event id or a reconciliation issue.
	CausationID string          `json:"causation_id,omitempty"`
	StateAfter  State           `json:"state_after"`
	Payload     json.RawMessage `json:"payload"`
	// OccurredAt is event time: the broker's timestamp for broker-sourced
	// events, otherwise the local clock.
	OccurredAt time.Time `json:"occurred_at"`
	// RecordedAt is the local receive time.
	RecordedAt time.Time `json:"recorded_at"`
}

// Draft is an event that has been decided but not yet versioned or stored.
type Draft struct {
	Type        EventType
	Source      Source
	Actor       string
	CausationID string
	Payload     any
	// OccurredAt is the event time. Zero means "use the record time".
	OccurredAt time.Time
}

// Payloads. Every event type has exactly one payload type.

type OrderCreated struct {
	AccountID     string        `json:"account_id"`
	ClientOrderID string        `json:"client_order_id"`
	Symbol        string        `json:"symbol"`
	Side          Side          `json:"side"`
	Type          OrderType     `json:"type"`
	TimeInForce   TimeInForce   `json:"time_in_force"`
	Qty           int64         `json:"qty"`
	LimitPrice    *fixed.Micros `json:"limit_price,omitempty"`
	CreatedBy     string        `json:"created_by"`
}

type OrderValidated struct {
	// Authorizer and Reference identify the pre-trade risk decision.
	Authorizer string `json:"authorizer"`
	Reference  string `json:"reference,omitempty"`
}

// Rejection origins.
const (
	ByRisk     = "RISK"
	ByBroker   = "BROKER"
	ByOperator = "OPERATOR"
	ByClient   = "CLIENT"
)

type OrderRejected struct {
	By     string   `json:"by"`
	Reason string   `json:"reason"`
	Codes  []string `json:"codes,omitempty"`
}

type SubmissionStarted struct {
	OutboxID int64 `json:"outbox_id"`
}

type OrderAcknowledged struct {
	BrokerOrderID string `json:"broker_order_id"`
}

type CancelRequested struct{}

type CancelRejected struct {
	Reason string `json:"reason,omitempty"`
}

type OrderCancelled struct {
	// By is CLIENT when the order was cancelled locally before it was ever
	// sent, BROKER when the broker confirmed or initiated the cancel.
	By string `json:"by"`
	// Unsolicited is true when the broker cancelled without a request from us.
	Unsolicited bool   `json:"unsolicited,omitempty"`
	Reason      string `json:"reason,omitempty"`
}

type ReplaceRequested struct {
	Replace
}

type ReplaceRejected struct {
	Reason string `json:"reason,omitempty"`
}

type OrderReplaced struct {
	Replace
	// Local is true when the order was amended before it was ever sent.
	Local bool `json:"local,omitempty"`
}

type OrderExpired struct {
	Reason string `json:"reason,omitempty"`
}

// ExecKind classifies a broker execution record.
type ExecKind string

const (
	ExecFill    ExecKind = "FILL"
	ExecCorrect ExecKind = "CORRECT"
	ExecBust    ExecKind = "BUST"
)

// Execution is the payload of every execution event. It carries the broker's
// record verbatim plus the deltas that record made to the order's totals, so
// the executions table and position projections can be rebuilt from events.
type Execution struct {
	BrokerExecID    string       `json:"broker_exec_id"`
	Kind            ExecKind     `json:"kind"`
	RefBrokerExecID string       `json:"ref_broker_exec_id,omitempty"`
	Qty             int64        `json:"qty"`
	Price           fixed.Micros `json:"price"`
	Fee             fixed.Micros `json:"fee"`
	// ExecutedAt is the broker's execution time.
	ExecutedAt    time.Time `json:"executed_at"`
	BrokerOrderID string    `json:"broker_order_id,omitempty"`

	// Deltas to the order's cumulative totals. A record that was already
	// superseded when it arrived has all-zero deltas.
	DeltaQty      int64        `json:"delta_qty"`
	DeltaNotional fixed.Micros `json:"delta_notional"`
	DeltaFee      fixed.Micros `json:"delta_fee"`

	// Late is true when the order had already closed.
	Late bool `json:"late,omitempty"`
	// Superseded is true when a correction for this record arrived first.
	Superseded bool `json:"superseded,omitempty"`
}
