package domain

import (
	"encoding/json"
	"errors"
	"fmt"

	"oms/src/fixed"
)

// ErrInvalidEvent is wrapped by every error Apply returns. Seeing it means a
// decision function produced an event the state machine forbids, which is a
// bug: nothing is written.
var ErrInvalidEvent = errors.New("invalid event for order")

var fillEvents = []EventType{EvExecutionRecorded, EvExecutionCorrected, EvExecutionAccepted}
var correctionEvents = []EventType{EvExecutionCorrected, EvExecutionBusted}

// Transitions is the complete state machine: from state → to state → the
// event types that may cause that move. A move that is not listed cannot be
// persisted. The only moves to an earlier state are caused by the two
// execution correction events.
var Transitions = map[State]map[State][]EventType{
	"": {
		StateNew: {EvOrderCreated},
	},
	StateNew: {
		StateValidated: {EvOrderValidated},
		StateRejected:  {EvOrderRejected},
	},
	StateValidated: {
		StateSubmitting: {EvSubmissionStarted},
		StateCancelled:  {EvOrderCancelled},
	},
	StateSubmitting: {
		StateAcknowledged:    {EvOrderAcknowledged},
		StatePartiallyFilled: fillEvents,
		StateFilled:          fillEvents,
		StateRejected:        {EvOrderRejected},
		StateCancelled:       {EvOrderCancelled},
		StateExpired:         {EvOrderExpired},
	},
	StateAcknowledged: {
		StatePartiallyFilled: fillEvents,
		StateFilled:          fillEvents,
		StateRejected:        {EvOrderRejected},
		StateCancelled:       {EvOrderCancelled},
		StateExpired:         {EvOrderExpired},
	},
	StatePartiallyFilled: {
		StateFilled:       fillEvents,
		StateCancelled:    {EvOrderCancelled},
		StateExpired:      {EvOrderExpired},
		StateAcknowledged: correctionEvents, // backward
	},
	StateFilled: {
		StatePartiallyFilled: correctionEvents, // backward
		StateAcknowledged:    correctionEvents, // backward
	},
	StateCancelled: {},
	StateRejected:  {},
	StateExpired:   {},
}

// TransitionAllowed reports whether event type t may move an order from one
// state to another. Staying in the same state is always allowed here; whether
// a given event is meaningful in that state is checked by Apply.
func TransitionAllowed(from, to State, t EventType) bool {
	if from == to && from != "" {
		return true
	}
	for _, allowed := range Transitions[from][to] {
		if allowed == t {
			return true
		}
	}
	return false
}

// Apply folds one event into an order and returns the new order. It is the
// only code that changes order state. It validates the event against the
// state machine and the aggregate's invariants and never mutates its input.
func Apply(o Order, e Event) (Order, error) {
	fail := func(format string, args ...any) (Order, error) {
		return Order{}, fmt.Errorf("%w: %s v%d on %s order %s: %s",
			ErrInvalidEvent, e.Type, e.Version, o.State, e.OrderID, fmt.Sprintf(format, args...))
	}
	if e.Version != o.Version+1 {
		return fail("expected version %d", o.Version+1)
	}
	if (o.Version == 0) != (e.Type == EvOrderCreated) {
		return fail("OrderCreated must be the first event and only the first")
	}
	if e.OrderID == "" || (o.Version > 0 && e.OrderID != o.ID) {
		return fail("event belongs to another order")
	}

	n := o
	at := e.RecordedAt

	switch e.Type {
	case EvOrderCreated:
		p, err := decode[OrderCreated](e)
		if err != nil {
			return fail("%v", err)
		}
		n = Order{
			ID: e.OrderID, AccountID: p.AccountID, ClientOrderID: p.ClientOrderID,
			Symbol: p.Symbol, Side: p.Side, Type: p.Type, TimeInForce: p.TimeInForce,
			Qty: p.Qty, LimitPrice: p.LimitPrice, CreatedBy: p.CreatedBy,
			State: StateNew, CreatedAt: at,
		}

	case EvOrderValidated:
		n.State = StateValidated

	case EvOrderRejected:
		p, err := decode[OrderRejected](e)
		if err != nil {
			return fail("%v", err)
		}
		if o.FilledQty != 0 {
			return fail("order has executions")
		}
		n.State = StateRejected
		n.RejectReason = p.Reason

	case EvSubmissionStarted:
		n.State = StateSubmitting
		n.SubmittedAt = &at

	case EvOrderAcknowledged:
		p, err := decode[OrderAcknowledged](e)
		if err != nil {
			return fail("%v", err)
		}
		n.State = StateAcknowledged
		n.AcknowledgedAt = &at
		if p.BrokerOrderID != "" {
			n.BrokerOrderID = p.BrokerOrderID
		}

	case EvCancelRequested:
		if !o.State.Live() || o.PendingCancel {
			return fail("no cancel can be requested")
		}
		n.PendingCancel = true

	case EvCancelRejected:
		if !o.PendingCancel {
			return fail("no cancel is pending")
		}
		n.PendingCancel = false

	case EvOrderCancelled:
		n.State = StateCancelled

	case EvReplaceRequested:
		p, err := decode[ReplaceRequested](e)
		if err != nil {
			return fail("%v", err)
		}
		if !o.State.Live() || o.PendingCancel || o.PendingReplace != nil {
			return fail("no replace can be requested")
		}
		if err := checkReplace(o, p.Replace); err != nil {
			return fail("%v", err)
		}
		r := p.Replace
		n.PendingReplace = &r

	case EvReplaceRejected:
		if o.PendingReplace == nil {
			return fail("no replace is pending")
		}
		n.PendingReplace = nil

	case EvOrderReplaced:
		p, err := decode[OrderReplaced](e)
		if err != nil {
			return fail("%v", err)
		}
		if p.Local {
			if o.State != StateValidated {
				return fail("a local replace needs an unsent order")
			}
		} else if !o.State.Live() || o.PendingReplace == nil {
			return fail("no replace is pending")
		}
		if err := checkReplace(o, p.Replace); err != nil {
			return fail("%v", err)
		}
		n.Qty = p.Qty
		n.LimitPrice = p.LimitPrice
		n.PendingReplace = nil

	case EvOrderExpired:
		n.State = StateExpired

	case EvExecutionRecorded, EvExecutionCorrected, EvExecutionBusted, EvExecutionAccepted:
		p, err := decode[Execution](e)
		if err != nil {
			return fail("%v", err)
		}
		if !AcceptsExecutions(o) {
			return fail("order cannot take executions")
		}
		n.FilledQty = o.FilledQty + p.DeltaQty
		var ok1, ok2 bool
		n.FilledNotional, ok1 = fixed.Add(o.FilledNotional, p.DeltaNotional)
		n.Fees, ok2 = fixed.Add(o.Fees, p.DeltaFee)
		if !ok1 || !ok2 {
			return fail("totals overflow")
		}
		if n.FilledQty > n.Qty {
			// Only an explicit operator acceptance may take an order above its
			// quantity. Once it has, later corrections may still lower the total.
			switch {
			case e.Type == EvExecutionAccepted:
				n.OverfillAccepted = true
			case o.OverfillAccepted && p.DeltaQty <= 0:
			default:
				return fail("filled %d would exceed order quantity %d", n.FilledQty, n.Qty)
			}
		}
		if n.BrokerOrderID == "" {
			n.BrokerOrderID = p.BrokerOrderID
		}
		n.State = stateAfterExecution(o.State, n.FilledQty, n.Qty)
		if o.State == StateSubmitting && n.State != StateSubmitting && n.AcknowledgedAt == nil {
			// A fill that beats the acknowledgement implies it.
			n.AcknowledgedAt = &at
		}

	default:
		return fail("unknown event type")
	}

	if !TransitionAllowed(o.State, n.State, e.Type) {
		return fail("transition to %s is not allowed", n.State)
	}
	if Backward(o.State, n.State) && !e.Type.IsCorrection() {
		return fail("backward transition to %s without a correction event", n.State)
	}

	n.Version = e.Version
	n.UpdatedAt = at
	switch {
	case !n.State.Terminal():
		n.ClosedAt = nil
	case !o.State.Terminal():
		n.ClosedAt = &at
		// Nothing can be pending on a closed order.
		n.PendingCancel = false
		n.PendingReplace = nil
	}

	if err := CheckInvariants(n); err != nil {
		return fail("%v", err)
	}
	return n, nil
}

// AcceptsExecutions reports whether a broker execution can be booked against
// the order: it must have been sent, and must not have been rejected.
func AcceptsExecutions(o Order) bool {
	switch o.State {
	case StateSubmitting, StateAcknowledged, StatePartiallyFilled, StateFilled, StateExpired:
		return true
	case StateCancelled:
		return o.Submitted() // a locally cancelled order never reached the broker
	}
	return false
}

// stateAfterExecution derives the state from the filled quantity. Closed
// CANCELLED and EXPIRED orders keep their state when a late fill or a
// correction arrives; the broker closed them and only the totals change.
func stateAfterExecution(from State, filled, qty int64) State {
	if from == StateCancelled || from == StateExpired {
		return from
	}
	switch {
	case filled >= qty:
		return StateFilled
	case filled > 0:
		return StatePartiallyFilled
	case from == StateSubmitting:
		return StateSubmitting
	default:
		return StateAcknowledged
	}
}

func checkReplace(o Order, r Replace) error {
	if r.Qty <= o.FilledQty {
		return fmt.Errorf("new quantity %d must exceed filled quantity %d", r.Qty, o.FilledQty)
	}
	if (o.Type == Limit) != (r.LimitPrice != nil) {
		return errors.New("limit price must be set for LIMIT orders and only for them")
	}
	if r.LimitPrice != nil && *r.LimitPrice <= 0 {
		return errors.New("limit price must be positive")
	}
	return nil
}

// CheckInvariants verifies the rules that hold for every stored order.
func CheckInvariants(o Order) error {
	switch {
	case !o.State.Valid():
		return fmt.Errorf("unknown state %q", o.State)
	case o.Qty <= 0:
		return errors.New("order quantity must be positive")
	case o.FilledQty < 0:
		return errors.New("filled quantity is negative")
	case o.FilledQty > o.Qty && !o.OverfillAccepted:
		return fmt.Errorf("filled %d exceeds order quantity %d without an accepted overfill", o.FilledQty, o.Qty)
	case (o.Type == Limit) != (o.LimitPrice != nil):
		return errors.New("limit price must be set for LIMIT orders and only for them")
	case o.LimitPrice != nil && *o.LimitPrice <= 0:
		return errors.New("limit price must be positive")
	}
	switch o.State {
	case StateNew, StateValidated, StateSubmitting, StateAcknowledged, StateRejected:
		if o.FilledQty != 0 {
			return fmt.Errorf("%s order has filled quantity %d", o.State, o.FilledQty)
		}
	case StatePartiallyFilled:
		if o.FilledQty <= 0 || o.FilledQty >= o.Qty {
			return fmt.Errorf("PARTIALLY_FILLED order has filled %d of %d", o.FilledQty, o.Qty)
		}
	case StateFilled:
		if o.FilledQty < o.Qty {
			return fmt.Errorf("FILLED order has filled %d of %d", o.FilledQty, o.Qty)
		}
	}
	if !o.State.Live() && (o.PendingCancel || o.PendingReplace != nil) {
		return fmt.Errorf("%s order has a pending cancel or replace", o.State)
	}
	if o.PendingReplace != nil && o.PendingReplace.Qty <= 0 {
		return errors.New("pending replace quantity must be positive")
	}
	return nil
}

// Fold rebuilds an order from its full event history.
func Fold(events []Event) (Order, error) {
	var o Order
	for _, e := range events {
		var err error
		if o, err = Apply(o, e); err != nil {
			return Order{}, err
		}
	}
	return o, nil
}

func decode[T any](e Event) (T, error) {
	var p T
	if err := json.Unmarshal(e.Payload, &p); err != nil {
		return p, fmt.Errorf("payload: %w", err)
	}
	return p, nil
}
