// Package broker is the adapter between the OMS and a broker. It defines the
// outbound calls the dispatcher makes, normalizes inbound reports from either
// transport (pushed webhooks, polled snapshots) into domain.BrokerReport, and
// implements both against the simulated broker's HTTP API.
//
// The broker is treated as an external, eventually consistent system. An
// outbound call that succeeds only means the broker accepted the request for
// processing; what actually happened arrives later as reports.
package broker

import (
	"context"
	"errors"
	"fmt"
	"time"

	"oms/src/domain"
	"oms/src/fixed"
)

// SubmitRequest is a new order for the broker. ClientOrderID is the OMS order
// id: it is stable across retries, and the broker uses it to deduplicate.
type SubmitRequest struct {
	ClientOrderID string             `json:"client_order_id"`
	Symbol        string             `json:"symbol"`
	Side          domain.Side        `json:"side"`
	Type          domain.OrderType   `json:"type"`
	TimeInForce   domain.TimeInForce `json:"time_in_force"`
	Qty           int64              `json:"qty"`
	LimitPrice    *fixed.Micros      `json:"limit_price,omitempty"`
}

// ReplaceRequest carries new terms for a working order.
type ReplaceRequest struct {
	Qty        int64         `json:"qty"`
	LimitPrice *fixed.Micros `json:"limit_price,omitempty"`
}

// Client is everything the OMS asks of a broker.
//
// Submit, Cancel and Replace must be safe to repeat: the dispatcher delivers
// at least once. Submit is deduplicated by ClientOrderID; Cancel and Replace
// by requestID.
type Client interface {
	Submit(ctx context.Context, r SubmitRequest) error
	Cancel(ctx context.Context, clientOrderID, requestID string) error
	Replace(ctx context.Context, clientOrderID, requestID string, r ReplaceRequest) error
	// GetOrder returns the broker's current view of one order, or an error
	// matching ErrUnknownOrder if the broker has never seen it.
	GetOrder(ctx context.Context, clientOrderID string) (OrderSnapshot, error)
	// Snapshot returns the broker's orders and executions for reconciliation.
	Snapshot(ctx context.Context) (Snapshot, error)
}

// ErrUnknownOrder means the broker has no record of the order.
var ErrUnknownOrder = errors.New("broker does not know the order")

// Error is a failed broker call.
type Error struct {
	Op string
	// Status is the HTTP status, or 0 if no response was received.
	Status  int
	Code    string
	Message string
	// Retryable is true when the request may not have been processed or the
	// failure is temporary: timeouts, connection errors, 5xx, 408 and 429.
	// False means the broker understood the request and refused it.
	Retryable bool
	Err       error
}

func (e *Error) Error() string {
	switch {
	case e.Status == 0:
		return fmt.Sprintf("broker %s: %v", e.Op, e.Err)
	case e.Code != "":
		return fmt.Sprintf("broker %s: HTTP %d %s: %s", e.Op, e.Status, e.Code, e.Message)
	}
	return fmt.Sprintf("broker %s: HTTP %d", e.Op, e.Status)
}

func (e *Error) Unwrap() error { return e.Err }

// IsRetryable reports whether a failed call should be tried again. Errors the
// adapter did not classify are retried: not knowing is not a refusal.
func IsRetryable(err error) bool {
	var be *Error
	if errors.As(err, &be) {
		return be.Retryable
	}
	return true
}

// Broker order statuses, as reported in snapshots.
const (
	StatusReceived        = "RECEIVED" // accepted for processing, not yet acknowledged
	StatusWorking         = "WORKING"
	StatusPartiallyFilled = "PARTIALLY_FILLED"
	StatusFilled          = "FILLED"
	StatusCancelled       = "CANCELLED"
	StatusRejected        = "REJECTED"
	StatusExpired         = "EXPIRED"
)

// OrderSnapshot is the broker's current view of one order.
type OrderSnapshot struct {
	ClientOrderID string        `json:"client_order_id"`
	BrokerOrderID string        `json:"broker_order_id"`
	Symbol        string        `json:"symbol"`
	Side          domain.Side   `json:"side"`
	Status        string        `json:"status"`
	Qty           int64         `json:"qty"`
	CumQty        int64         `json:"cum_qty"`
	LimitPrice    *fixed.Micros `json:"limit_price,omitempty"`
	Reason        string        `json:"reason,omitempty"`
	// UpdatedAt is the broker's clock.
	UpdatedAt time.Time `json:"updated_at"`
}

// ExecutionSnapshot is one execution record on the broker's books.
type ExecutionSnapshot struct {
	ExecID        string          `json:"exec_id"`
	Kind          domain.ExecKind `json:"kind"`
	RefExecID     string          `json:"ref_exec_id,omitempty"`
	ClientOrderID string          `json:"client_order_id"`
	BrokerOrderID string          `json:"broker_order_id,omitempty"`
	Qty           int64           `json:"qty"`
	Price         fixed.Micros    `json:"price"`
	Fee           fixed.Micros    `json:"fee"`
	// ExecutedAt is the broker's clock.
	ExecutedAt time.Time `json:"executed_at"`
}

// Snapshot is the broker's books at one instant of its own clock.
type Snapshot struct {
	AsOf       time.Time           `json:"as_of"`
	Orders     []OrderSnapshot     `json:"orders"`
	Executions []ExecutionSnapshot `json:"executions"`
}

// Report converts a snapshot execution into the same normalized report a
// webhook would have produced, so both transports take one code path.
func (x ExecutionSnapshot) Report(eventID string) domain.BrokerReport {
	r := domain.BrokerReport{
		EventID: eventID, OrderID: x.ClientOrderID, BrokerOrderID: x.BrokerOrderID,
		ExecID: x.ExecID, RefExecID: x.RefExecID, Qty: x.Qty, Price: x.Price, Fee: x.Fee,
		EventTime: x.ExecutedAt,
	}
	switch x.Kind {
	case domain.ExecCorrect:
		r.Kind = domain.ReportCorrect
	case domain.ExecBust:
		r.Kind = domain.ReportBust
	default:
		r.Kind = domain.ReportFill
	}
	return r
}
