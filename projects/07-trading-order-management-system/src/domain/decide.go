package domain

import (
	"encoding/json"
	"fmt"
	"regexp"
	"time"

	"oms/src/fixed"
)

// Hard input bounds on a single quantity and a single price. Their product
// can still exceed an int64 of micros, so every price × quantity is computed
// with an explicit overflow check and refused if it does not fit.
const (
	MaxQty   int64        = 1_000_000_000
	MaxPrice fixed.Micros = 1_000_000 * fixed.Scale
)

var (
	symbolRe   = regexp.MustCompile(`^[A-Za-z0-9._/-]{1,32}$`)
	accountRe  = regexp.MustCompile(`^[A-Za-z0-9._:-]{1,64}$`)
	clientIDRe = regexp.MustCompile(`^[A-Za-z0-9._:-]{1,64}$`)
)

// FieldError is one problem with a submitted order.
type FieldError struct {
	Field   string `json:"field"`
	Message string `json:"message"`
}

// ValidateNewOrder checks the shape and numeric bounds of a new order. These
// are hard errors: an order that fails here is never stored.
func ValidateNewOrder(p OrderCreated) []FieldError {
	var errs []FieldError
	add := func(field, msg string) { errs = append(errs, FieldError{field, msg}) }

	if !accountRe.MatchString(p.AccountID) {
		add("account_id", "required; up to 64 letters, digits or . _ : -")
	}
	if !clientIDRe.MatchString(p.ClientOrderID) {
		add("client_order_id", "up to 64 letters, digits or . _ : -")
	}
	if !symbolRe.MatchString(p.Symbol) {
		add("symbol", "required; up to 32 letters, digits or . _ / -")
	}
	if p.Side != Buy && p.Side != Sell {
		add("side", "must be BUY or SELL")
	}
	switch p.TimeInForce {
	case Day, GTC, IOC:
	default:
		add("time_in_force", "must be DAY, GTC or IOC")
	}
	if p.Qty <= 0 || p.Qty > MaxQty {
		add("qty", fmt.Sprintf("must be a whole number from 1 to %d", MaxQty))
	}
	switch p.Type {
	case Limit:
		if p.LimitPrice == nil {
			add("limit_price", "required for LIMIT orders")
		} else if *p.LimitPrice <= 0 || *p.LimitPrice > MaxPrice {
			add("limit_price", fmt.Sprintf("must be above 0 and at most %s", MaxPrice))
		}
	case Market:
		if p.LimitPrice != nil {
			add("limit_price", "must be omitted for MARKET orders")
		}
	default:
		add("type", "must be LIMIT or MARKET")
	}
	if p.LimitPrice != nil && len(errs) == 0 {
		if _, ok := fixed.MulQty(*p.LimitPrice, p.Qty); !ok {
			add("qty", "quantity times limit price is too large")
		}
	}
	return errs
}

// CommandError is a command the aggregate refuses in its current state.
type CommandError struct {
	Code    string
	Message string
}

func (e *CommandError) Error() string { return e.Code + ": " + e.Message }

const (
	CodeNotCancellable = "ORDER_NOT_CANCELLABLE"
	CodeNotReplaceable = "ORDER_NOT_REPLACEABLE"
	CodeCancelPending  = "CANCEL_PENDING"
	CodeReplacePending = "REPLACE_PENDING"
	CodeInvalidReplace = "INVALID_REPLACE"
	CodeNoChange       = "NO_CHANGE"
)

// DecideCancel decides a client cancel. dispatch is true when a cancel must be
// sent to the broker. A cancel of an order that is already cancelled or
// already has a cancel pending is a no-op, which makes the command idempotent.
func DecideCancel(o Order, actor string) (drafts []Draft, dispatch bool, err error) {
	switch {
	case o.State == StateValidated:
		// Never sent: nothing can have traded, so cancel immediately. The queued
		// submission is skipped when the dispatcher reaches it.
		return []Draft{{Type: EvOrderCancelled, Source: SourceAPI, Actor: actor,
			Payload: OrderCancelled{By: ByClient}}}, false, nil
	case o.State.Live():
		if o.PendingCancel {
			return nil, false, nil
		}
		// The order is CANCELLED only when the broker confirms: fills can still
		// race the request.
		return []Draft{{Type: EvCancelRequested, Source: SourceAPI, Actor: actor,
			Payload: CancelRequested{}}}, true, nil
	case o.State == StateCancelled:
		return nil, false, nil
	}
	return nil, false, &CommandError{CodeNotCancellable, fmt.Sprintf("order is %s", o.State)}
}

// DecideReplace decides a client change of quantity and/or limit price.
func DecideReplace(o Order, r Replace, actor string) (drafts []Draft, dispatch bool, err error) {
	if o.State != StateValidated && !o.State.Live() {
		return nil, false, &CommandError{CodeNotReplaceable, fmt.Sprintf("order is %s", o.State)}
	}
	if o.PendingCancel {
		return nil, false, &CommandError{CodeCancelPending, "a cancel is pending for this order"}
	}
	if o.PendingReplace != nil {
		return nil, false, &CommandError{CodeReplacePending, "a replace is already pending for this order"}
	}
	if r.Qty > MaxQty || (r.LimitPrice != nil && *r.LimitPrice > MaxPrice) {
		return nil, false, &CommandError{CodeInvalidReplace, "quantity or limit price is out of range"}
	}
	if err := checkReplace(o, r); err != nil {
		return nil, false, &CommandError{CodeInvalidReplace, err.Error()}
	}
	if r.LimitPrice != nil {
		if _, ok := fixed.MulQty(*r.LimitPrice, r.Qty); !ok {
			return nil, false, &CommandError{CodeInvalidReplace, "quantity times limit price is too large"}
		}
	}
	if r.Qty == o.Qty && samePrice(r.LimitPrice, o.LimitPrice) {
		return nil, false, &CommandError{CodeNoChange, "the replace changes nothing"}
	}
	if o.State == StateValidated {
		return []Draft{{Type: EvOrderReplaced, Source: SourceAPI, Actor: actor,
			Payload: OrderReplaced{Replace: r, Local: true}}}, false, nil
	}
	return []Draft{{Type: EvReplaceRequested, Source: SourceAPI, Actor: actor,
		Payload: ReplaceRequested{Replace: r}}}, true, nil
}

func samePrice(a, b *fixed.Micros) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
}

// DecideSubmit is asked by the dispatcher before it sends an order. send is
// false when the order no longer needs sending, for example because it was
// cancelled while queued.
func DecideSubmit(o Order, outboxID int64) (drafts []Draft, send bool) {
	switch o.State {
	case StateValidated:
		return []Draft{{Type: EvSubmissionStarted, Source: SourceOutbox,
			CausationID: fmt.Sprintf("outbox:%d", outboxID),
			Payload:     SubmissionStarted{OutboxID: outboxID}}}, true
	case StateSubmitting:
		return nil, true // a retry of an attempt whose outcome is unknown
	}
	return nil, false
}

// ReportKind is a normalized broker report type.
type ReportKind string

const (
	ReportAck            ReportKind = "ACK"
	ReportReject         ReportKind = "REJECT"
	ReportFill           ReportKind = "FILL"
	ReportCorrect        ReportKind = "CORRECT"
	ReportBust           ReportKind = "BUST"
	ReportCancelled      ReportKind = "CANCELLED"
	ReportCancelRejected ReportKind = "CANCEL_REJECTED"
	ReportReplaced       ReportKind = "REPLACED"
	ReportReplaceReject  ReportKind = "REPLACE_REJECTED"
	ReportExpired        ReportKind = "EXPIRED"
)

// IsExecution reports whether the report carries an execution record.
func (k ReportKind) IsExecution() bool {
	return k == ReportFill || k == ReportCorrect || k == ReportBust
}

// BrokerReport is what the broker adapter hands to the aggregate, whichever
// transport it arrived on.
type BrokerReport struct {
	// EventID is the broker's unique id for this report. Redeliveries reuse it.
	EventID       string     `json:"event_id"`
	Kind          ReportKind `json:"kind"`
	OrderID       string     `json:"order_id"`
	BrokerOrderID string     `json:"broker_order_id,omitempty"`

	// Execution fields, for FILL, CORRECT and BUST.
	ExecID    string       `json:"exec_id,omitempty"`
	RefExecID string       `json:"ref_exec_id,omitempty"`
	Qty       int64        `json:"qty,omitempty"`
	Price     fixed.Micros `json:"price,omitempty"`
	Fee       fixed.Micros `json:"fee,omitempty"`

	// Order terms confirmed by the broker, for REPLACED.
	OrderQty   int64         `json:"order_qty,omitempty"`
	LimitPrice *fixed.Micros `json:"limit_price,omitempty"`

	Reason string `json:"reason,omitempty"`
	// EventTime is the broker's clock. It is recorded but never used to order
	// or accept reports.
	EventTime time.Time `json:"event_time"`
	// Raw is the report as received, kept for audit.
	Raw json.RawMessage `json:"-"`
}

// Outcome is what happened to a broker report.
type Outcome string

const (
	// OutcomeApplied: the report produced at least one event.
	OutcomeApplied Outcome = "APPLIED"
	// OutcomeDuplicate: the same report (or execution) was already processed.
	OutcomeDuplicate Outcome = "DUPLICATE"
	// OutcomeStale: the report describes something the order is already past.
	OutcomeStale Outcome = "STALE"
	// OutcomeQuarantined: the report contradicts the order and needs an
	// operator. Nothing was applied and an issue was raised.
	OutcomeQuarantined Outcome = "QUARANTINED"
	// OutcomeUnknownOrder: the report names an order this OMS does not have.
	OutcomeUnknownOrder Outcome = "UNKNOWN_ORDER"
)

// Quarantine reasons. Each is also the kind of the issue that is raised.
const (
	ReasonOverfill            = "OVERFILL"
	ReasonUnexpectedExecution = "UNEXPECTED_EXECUTION"
	ReasonConflictingReport   = "CONFLICTING_REPORT"
	ReasonInvalidReport       = "INVALID_REPORT"
)

// ExecValues are the economic terms of one execution record.
type ExecValues struct {
	Qty   int64
	Price fixed.Micros
	Fee   fixed.Micros
}

// ExecContext is what the executions already on file say about an incoming
// execution report.
type ExecContext struct {
	// Ref holds the terms of the execution this report corrects or busts, when
	// that execution is on file and still effective. Nil otherwise.
	Ref *ExecValues
	// Superseded is true when a correction or bust naming this report's own
	// execution id is already on file, because it overtook this report.
	Superseded bool
	// RefCorrectedBy is the id of another record that already corrects or
	// busts the execution this report references. An execution can be
	// replaced only once, so such a report cannot be booked.
	RefCorrectedBy string
}

// Decision is the aggregate's verdict on a broker report.
type Decision struct {
	Outcome Outcome
	// Reason is a quarantine reason, empty otherwise.
	Reason string
	Detail string
	Drafts []Draft
}

func stale(detail string) Decision { return Decision{Outcome: OutcomeStale, Detail: detail} }
func quarantine(reason, detail string) Decision {
	return Decision{Outcome: OutcomeQuarantined, Reason: reason, Detail: detail}
}

// DecideReport decides what a broker report means for an order. It never
// trusts delivery order: reports may be duplicated, delayed or reordered, so
// each rule looks only at the order's current state and totals.
func DecideReport(o Order, r BrokerReport, x ExecContext, src Source) Decision {
	if !o.Submitted() {
		return quarantine(ReasonConflictingReport,
			fmt.Sprintf("%s report for an order that was never sent to the broker", r.Kind))
	}
	draft := func(t EventType, payload any) Decision {
		return Decision{Outcome: OutcomeApplied, Drafts: []Draft{{
			Type: t, Source: src, CausationID: r.EventID, Payload: payload, OccurredAt: r.EventTime,
		}}}
	}
	conflict := func() Decision {
		return quarantine(ReasonConflictingReport,
			fmt.Sprintf("%s report contradicts %s order", r.Kind, o.State))
	}

	switch r.Kind {
	case ReportAck:
		if o.State == StateSubmitting {
			return draft(EvOrderAcknowledged, OrderAcknowledged{BrokerOrderID: r.BrokerOrderID})
		}
		return stale("order is already past acknowledgement")

	case ReportReject:
		switch o.State {
		case StateSubmitting, StateAcknowledged:
			return draft(EvOrderRejected, OrderRejected{By: ByBroker, Reason: r.Reason})
		case StateRejected:
			return stale("order is already rejected")
		}
		return conflict()

	case ReportCancelled:
		switch {
		case o.State.Live():
			return draft(EvOrderCancelled, OrderCancelled{
				By: ByBroker, Unsolicited: !o.PendingCancel, Reason: r.Reason})
		case o.State == StateCancelled:
			return stale("order is already cancelled")
		}
		return conflict()

	case ReportExpired:
		switch {
		case o.State.Live():
			return draft(EvOrderExpired, OrderExpired{Reason: r.Reason})
		case o.State == StateExpired:
			return stale("order is already expired")
		}
		return conflict()

	case ReportCancelRejected:
		if o.State.Live() && o.PendingCancel {
			return draft(EvCancelRejected, CancelRejected{Reason: r.Reason})
		}
		return stale("no cancel is pending")

	case ReportReplaced:
		switch {
		case o.State.Live() && o.PendingReplace != nil:
			got := Replace{Qty: r.OrderQty, LimitPrice: r.LimitPrice}
			if got.Qty != o.PendingReplace.Qty || !samePrice(got.LimitPrice, o.PendingReplace.LimitPrice) {
				return quarantine(ReasonConflictingReport, "broker confirmed terms that differ from the requested replace")
			}
			if err := checkReplace(o, got); err != nil {
				return quarantine(ReasonConflictingReport, "broker confirmed an impossible replace: "+err.Error())
			}
			return draft(EvOrderReplaced, OrderReplaced{Replace: got})
		case o.State.Terminal():
			return stale("order is closed")
		}
		return quarantine(ReasonConflictingReport, "replace confirmation without a pending replace")

	case ReportReplaceReject:
		if o.State.Live() && o.PendingReplace != nil {
			return draft(EvReplaceRejected, ReplaceRejected{Reason: r.Reason})
		}
		return stale("no replace is pending")

	case ReportFill, ReportCorrect, ReportBust:
		p, d := decideExecution(o, r, x, false)
		if p == nil {
			return d
		}
		t := EvExecutionRecorded
		switch r.Kind {
		case ReportCorrect:
			t = EvExecutionCorrected
		case ReportBust:
			t = EvExecutionBusted
		}
		return draft(t, *p)
	}
	return quarantine(ReasonInvalidReport, fmt.Sprintf("unknown report kind %q", r.Kind))
}

// DecideAccept is the operator's explicit acceptance of an execution that was
// quarantined as an overfill. It is the one path that lets filled quantity
// exceed order quantity.
func DecideAccept(o Order, r BrokerReport, x ExecContext, actor, issueID string) Decision {
	if !r.Kind.IsExecution() {
		return quarantine(ReasonInvalidReport, "only executions can be accepted")
	}
	p, d := decideExecution(o, r, x, true)
	if p == nil {
		return d
	}
	return Decision{Outcome: OutcomeApplied, Drafts: []Draft{{
		Type: EvExecutionAccepted, Source: SourceOperator, Actor: actor,
		CausationID: "issue:" + issueID, Payload: *p, OccurredAt: r.EventTime,
	}}}
}

// decideExecution computes what an execution record does to the order's
// totals. It returns a nil payload and a quarantine decision when the record
// cannot be booked.
//
// Records are commutative. A correction or bust replaces the record it names:
// its delta is its own terms minus the terms of that record, if that record
// is on file. If the correction arrives first it is booked on its own, and
// the original, when it finally arrives, is stored as already superseded with
// a zero delta. Either order produces the same totals.
func decideExecution(o Order, r BrokerReport, x ExecContext, allowOverfill bool) (*Execution, Decision) {
	invalid := func(msg string) (*Execution, Decision) {
		return nil, quarantine(ReasonInvalidReport, msg)
	}
	if r.ExecID == "" {
		return invalid("execution id is missing")
	}
	switch r.Kind {
	case ReportFill:
		if r.RefExecID != "" {
			return invalid("a fill cannot reference another execution")
		}
	case ReportCorrect, ReportBust:
		if r.RefExecID == "" || r.RefExecID == r.ExecID {
			return invalid("a correction must reference a different execution")
		}
	}
	if r.Kind != ReportBust && (r.Qty <= 0 || r.Qty > MaxQty || r.Price <= 0 || r.Price > MaxPrice || r.Fee < 0) {
		return invalid("execution quantity, price or fee is out of range")
	}
	if !AcceptsExecutions(o) {
		return nil, quarantine(ReasonUnexpectedExecution,
			fmt.Sprintf("execution %s reported for %s order", r.ExecID, o.State))
	}
	if x.RefCorrectedBy != "" {
		return nil, quarantine(ReasonConflictingReport, fmt.Sprintf(
			"execution %s was already replaced by %s", r.RefExecID, x.RefCorrectedBy))
	}

	var own ExecValues
	if r.Kind != ReportBust && !x.Superseded {
		own = ExecValues{Qty: r.Qty, Price: r.Price, Fee: r.Fee}
	}
	var ref ExecValues
	if x.Ref != nil {
		ref = *x.Ref
	}
	ownNotional, ok1 := fixed.MulQty(own.Price, own.Qty)
	refNotional, ok2 := fixed.MulQty(ref.Price, ref.Qty)
	if !ok1 || !ok2 {
		return invalid("execution notional overflows")
	}

	p := &Execution{
		BrokerExecID: r.ExecID, RefBrokerExecID: r.RefExecID, BrokerOrderID: r.BrokerOrderID,
		Qty: r.Qty, Price: r.Price, Fee: r.Fee, ExecutedAt: r.EventTime.UTC(),
		DeltaQty:      own.Qty - ref.Qty,
		DeltaNotional: ownNotional - refNotional,
		DeltaFee:      own.Fee - ref.Fee,
		Late:          o.State == StateCancelled || o.State == StateExpired,
		Superseded:    x.Superseded,
	}
	switch r.Kind {
	case ReportFill:
		p.Kind = ExecFill
	case ReportCorrect:
		p.Kind = ExecCorrect
	case ReportBust:
		p.Kind = ExecBust
		p.Qty, p.Price, p.Fee = 0, 0, 0
	}

	filled := o.FilledQty + p.DeltaQty
	if filled < 0 {
		return invalid("execution would make filled quantity negative")
	}
	_, ok1 = fixed.Add(o.FilledNotional, p.DeltaNotional)
	_, ok2 = fixed.Add(o.Fees, p.DeltaFee)
	if !ok1 || !ok2 {
		return invalid("execution would overflow the order's totals")
	}
	if filled > o.Qty && p.DeltaQty > 0 && !allowOverfill {
		return nil, quarantine(ReasonOverfill, fmt.Sprintf(
			"execution %s for %d would take filled quantity to %d, above order quantity %d",
			r.ExecID, p.DeltaQty, filled, o.Qty))
	}
	return p, Decision{}
}
