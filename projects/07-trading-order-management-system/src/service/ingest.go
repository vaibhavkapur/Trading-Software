package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"oms/src/domain"
	"oms/src/metrics"
	"oms/src/store"
)

// IngestResult says what happened to one broker report.
type IngestResult struct {
	Outcome domain.Outcome
	Detail  string
	// IssueID and IssueKind are set when the report was quarantined or
	// orphaned. IssueOpened is false if that issue was already open.
	IssueID     string
	IssueKind   string
	IssueOpened bool
	// Order is the order after the report, when the order is known.
	Order *domain.Order
}

// QuarantinedReport is stored in an issue's details so that an operator can
// see exactly what the broker sent, and accept it if that is the right call.
type QuarantinedReport struct {
	Reason     string              `json:"reason"`
	Detail     string              `json:"detail"`
	Source     domain.Source       `json:"source"`
	OrderState domain.State        `json:"order_state,omitempty"`
	OrderQty   int64               `json:"order_qty,omitempty"`
	FilledQty  int64               `json:"filled_qty"`
	Report     domain.BrokerReport `json:"report"`
}

// IngestBrokerReport applies one normalized broker report, from a webhook or
// from a reconciliation snapshot. It is the only way broker data reaches an
// order.
//
// The report is filed in the inbox and applied in a single transaction, which
// gives exactly-once processing on top of at-least-once delivery:
//
//   - a redelivered event id is counted and ignored;
//   - an execution id that is already booked is ignored, whichever transport
//     and event id it arrived under;
//   - a report the order is already past is recorded as stale;
//   - a report that contradicts the order is quarantined and raises an issue
//     instead of being applied or dropped.
//
// A returned error means nothing was recorded and the sender should retry.
func (s *Service) IngestBrokerReport(ctx context.Context, r domain.BrokerReport, src domain.Source) (IngestResult, error) {
	if r.EventID == "" || r.OrderID == "" {
		return IngestResult{}, errors.New("service: broker report needs an event id and an order id")
	}
	// PostgreSQL keeps microseconds; normalise first so that what is stored in
	// event payloads and what is stored in columns are the same instant.
	r.EventTime = r.EventTime.UTC().Truncate(time.Microsecond)
	payload := r.Raw
	if len(payload) == 0 {
		var err error
		if payload, err = json.Marshal(r); err != nil {
			return IngestResult{}, err
		}
	}

	var res IngestResult
	var before domain.Order
	var now time.Time
	err := s.Store.Tx(ctx, func(tx *store.Tx) error {
		now = tx.Now
		inboxID, first, prior, err := tx.RecordBrokerEvent(ctx, r, src, payload)
		if err != nil {
			return err
		}
		if !first {
			res = IngestResult{Outcome: domain.OutcomeDuplicate, Detail: "event already processed with outcome " + string(prior)}
			return nil
		}
		finish := func(outcome domain.Outcome, detail string) error {
			res.Outcome, res.Detail = outcome, detail
			return tx.FinishBrokerEvent(ctx, inboxID, outcome, detail)
		}

		o, err := tx.LockOrder(ctx, r.OrderID)
		if errors.Is(err, store.ErrNotFound) {
			severity := store.SeverityHigh
			if r.Kind.IsExecution() {
				severity = store.SeverityCritical // a trade we have no order for
			}
			detail := "the broker reported on an order this OMS does not have"
			res.IssueKind = store.KindOrphanEvent
			res.IssueID, res.IssueOpened, err = tx.RaiseIssue(ctx, store.NewIssue{
				ID: s.IDs.New("iss"), OrderID: r.OrderID, Kind: store.KindOrphanEvent, Severity: severity,
				Fingerprint: fmt.Sprintf("%s:%s:%s", store.KindOrphanEvent, r.OrderID, reportKey(r)),
				Details:     QuarantinedReport{Reason: store.KindOrphanEvent, Detail: detail, Source: src, Report: r},
			})
			if err != nil {
				return err
			}
			return finish(domain.OutcomeUnknownOrder, detail)
		}
		if err != nil {
			return err
		}
		before = o
		res.Order = &o

		var x domain.ExecContext
		if r.Kind.IsExecution() {
			look, err := tx.LookupExecution(ctx, r.ExecID, r.RefExecID)
			if err != nil {
				return err
			}
			if look.Existing != nil {
				if sameExecution(*look.Existing, o.ID, r) {
					// The same trade under a different event id, for example
					// first from a webhook and now from a snapshot.
					return finish(domain.OutcomeDuplicate, "execution "+r.ExecID+" is already booked")
				}
				return s.quarantine(ctx, tx, &res, finish, o, r, src, domain.ReasonConflictingReport,
					"execution id "+r.ExecID+" is already booked with different terms")
			}
			if look.RefOrderID != "" && look.RefOrderID != o.ID {
				return s.quarantine(ctx, tx, &res, finish, o, r, src, domain.ReasonConflictingReport,
					"the referenced execution "+r.RefExecID+" belongs to another order")
			}
			x = look.Context
		}

		d := domain.DecideReport(o, r, x, src)
		switch d.Outcome {
		case domain.OutcomeApplied:
			after, _, err := tx.Append(ctx, o.ID, o, d.Drafts)
			if err != nil {
				return err
			}
			res.Order = &after
			return finish(domain.OutcomeApplied, "")
		case domain.OutcomeQuarantined:
			return s.quarantine(ctx, tx, &res, finish, o, r, src, d.Reason, d.Detail)
		default:
			return finish(d.Outcome, d.Detail)
		}
	})
	if err != nil {
		return IngestResult{}, err
	}

	s.Metrics.BrokerEvents.Inc(string(r.Kind), string(src), string(res.Outcome))
	if res.Outcome == domain.OutcomeApplied {
		if !r.EventTime.IsZero() {
			s.Metrics.OrderStateLag.Observe(metrics.Seconds(now.Sub(r.EventTime)), string(r.Kind))
			if r.EventTime.After(now.Add(time.Second)) {
				s.Metrics.FutureTimestamps.Inc()
			}
		}
		if after := res.Order; before.AcknowledgedAt == nil && after.AcknowledgedAt != nil && after.SubmittedAt != nil {
			s.Metrics.SubmitToAck.Observe(metrics.Seconds(after.AcknowledgedAt.Sub(*after.SubmittedAt)))
		}
	}
	level := s.Log.Debug
	if res.Outcome == domain.OutcomeQuarantined || res.Outcome == domain.OutcomeUnknownOrder {
		level = s.Log.Warn
	}
	level("broker report", "event_id", r.EventID, "kind", r.Kind, "order_id", r.OrderID,
		"source", src, "outcome", res.Outcome, "detail", res.Detail)
	return res, nil
}

// quarantine records a report that cannot be applied and raises an issue for
// an operator. The order is left untouched.
func (s *Service) quarantine(ctx context.Context, tx *store.Tx, res *IngestResult,
	finish func(domain.Outcome, string) error, o domain.Order, r domain.BrokerReport, src domain.Source,
	reason, detail string) error {

	severity := store.SeverityHigh
	switch {
	case r.Kind.IsExecution():
		severity = store.SeverityCritical // the broker says we traded and our books disagree
	case reason == domain.ReasonInvalidReport:
		severity = store.SeverityMedium
	}
	id, opened, err := tx.RaiseIssue(ctx, store.NewIssue{
		ID: s.IDs.New("iss"), OrderID: o.ID, Kind: reason, Severity: severity,
		Fingerprint: fmt.Sprintf("%s:%s:%s", reason, o.ID, reportKey(r)),
		Details: QuarantinedReport{Reason: reason, Detail: detail, Source: src,
			OrderState: o.State, OrderQty: o.Qty, FilledQty: o.FilledQty, Report: r},
	})
	if err != nil {
		return err
	}
	res.IssueID, res.IssueKind, res.IssueOpened = id, reason, opened
	return finish(domain.OutcomeQuarantined, reason+": "+detail)
}

// reportKey identifies what a report is about, independent of its event id,
// so the same problem reported again maps to the same issue.
func reportKey(r domain.BrokerReport) string {
	if r.Kind.IsExecution() {
		return "exec:" + r.ExecID
	}
	return string(r.Kind)
}

// sameExecution reports whether a booked execution matches an incoming
// report for the same execution id.
func sameExecution(x store.ExecutionRow, orderID string, r domain.BrokerReport) bool {
	if x.OrderID != orderID || x.RefBrokerExecID != r.RefExecID {
		return false
	}
	switch r.Kind {
	case domain.ReportBust:
		return x.Kind == domain.ExecBust
	case domain.ReportCorrect:
		return x.Kind == domain.ExecCorrect && x.Qty == r.Qty && x.Price == r.Price && x.Fee == r.Fee
	default:
		return x.Kind == domain.ExecFill && x.Qty == r.Qty && x.Price == r.Price && x.Fee == r.Fee
	}
}
