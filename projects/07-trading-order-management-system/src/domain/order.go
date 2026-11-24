// Package domain is the order aggregate: its states, its events, the rules
// that decide which events a command or a broker report produces, and the
// fold that turns events back into an order.
//
// Everything here is pure. There is no I/O, no clock and no randomness, so the
// same events always fold to the same order.
package domain

import (
	"time"

	"oms/src/fixed"
)

// State is a position in the order lifecycle.
type State string

const (
	StateNew             State = "NEW"
	StateValidated       State = "VALIDATED"
	StateSubmitting      State = "SUBMITTING"
	StateAcknowledged    State = "ACKNOWLEDGED"
	StatePartiallyFilled State = "PARTIALLY_FILLED"
	StateFilled          State = "FILLED"
	StateCancelled       State = "CANCELLED"
	StateRejected        State = "REJECTED"
	StateExpired         State = "EXPIRED"
)

// States lists every state in lifecycle order.
var States = []State{
	StateNew, StateValidated, StateSubmitting, StateAcknowledged,
	StatePartiallyFilled, StateFilled, StateCancelled, StateRejected, StateExpired,
}

// Terminal reports whether the order is closed. FILLED is terminal for
// commands, but a documented execution correction can still reopen it.
func (s State) Terminal() bool {
	switch s {
	case StateFilled, StateCancelled, StateRejected, StateExpired:
		return true
	}
	return false
}

// Live reports whether the order has been handed to the broker and may still
// trade.
func (s State) Live() bool {
	switch s {
	case StateSubmitting, StateAcknowledged, StatePartiallyFilled:
		return true
	}
	return false
}

// Valid reports whether s is a known state.
func (s State) Valid() bool {
	for _, k := range States {
		if s == k {
			return true
		}
	}
	return false
}

// progress orders the open states so that a move to a lower value can be
// recognised as a backward transition.
func (s State) progress() int {
	switch s {
	case "": // before the first event
		return -1
	case StateNew:
		return 0
	case StateValidated:
		return 1
	case StateSubmitting:
		return 2
	case StateAcknowledged:
		return 3
	case StatePartiallyFilled:
		return 4
	default: // every terminal state
		return 5
	}
}

// Backward reports whether from → to undoes progress. Only execution
// correction events may cause such a transition.
func Backward(from, to State) bool {
	return to.progress() < from.progress()
}

type Side string

const (
	Buy  Side = "BUY"
	Sell Side = "SELL"
)

// Sign is +1 for a buy and -1 for a sell.
func (s Side) Sign() int64 {
	if s == Sell {
		return -1
	}
	return 1
}

type OrderType string

const (
	Limit  OrderType = "LIMIT"
	Market OrderType = "MARKET"
)

type TimeInForce string

const (
	Day TimeInForce = "DAY"
	GTC TimeInForce = "GTC"
	IOC TimeInForce = "IOC"
)

// Replace is a requested or confirmed change to a working order.
type Replace struct {
	Qty        int64         `json:"qty"`
	LimitPrice *fixed.Micros `json:"limit_price,omitempty"`
}

// Order is the aggregate. It is only ever produced by folding events with
// Apply; the orders table is a cached copy of that fold.
type Order struct {
	ID            string        `json:"id"`
	AccountID     string        `json:"account_id"`
	ClientOrderID string        `json:"client_order_id"`
	Symbol        string        `json:"symbol"`
	Side          Side          `json:"side"`
	Type          OrderType     `json:"type"`
	TimeInForce   TimeInForce   `json:"time_in_force"`
	Qty           int64         `json:"qty"`
	LimitPrice    *fixed.Micros `json:"limit_price,omitempty"`

	State   State `json:"state"`
	Version int64 `json:"version"`

	// FilledQty, FilledNotional and Fees are the totals over effective
	// executions: fills, net of corrections and busts.
	FilledQty      int64        `json:"filled_qty"`
	FilledNotional fixed.Micros `json:"filled_notional"`
	Fees           fixed.Micros `json:"fees"`
	// OverfillAccepted is set once an operator has explicitly accepted an
	// execution that took FilledQty above Qty. Without it FilledQty <= Qty.
	OverfillAccepted bool `json:"overfill_accepted,omitempty"`

	BrokerOrderID  string   `json:"broker_order_id,omitempty"`
	PendingCancel  bool     `json:"pending_cancel"`
	PendingReplace *Replace `json:"pending_replace,omitempty"`
	RejectReason   string   `json:"reject_reason,omitempty"`
	CreatedBy      string   `json:"created_by"`

	// All timestamps are UTC local receive times taken from the events that
	// caused them. Broker event times live on the events and executions.
	CreatedAt      time.Time  `json:"created_at"`
	UpdatedAt      time.Time  `json:"updated_at"`
	SubmittedAt    *time.Time `json:"submitted_at,omitempty"`
	AcknowledgedAt *time.Time `json:"acknowledged_at,omitempty"`
	ClosedAt       *time.Time `json:"closed_at,omitempty"`
}

// LeavesQty is the quantity still open to trade. It is zero for closed orders.
func (o Order) LeavesQty() int64 {
	if o.State.Terminal() || o.FilledQty >= o.Qty {
		return 0
	}
	return o.Qty - o.FilledQty
}

// Submitted reports whether a submission to the broker was ever attempted.
func (o Order) Submitted() bool { return o.SubmittedAt != nil }
