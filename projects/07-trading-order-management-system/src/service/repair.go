package service

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"oms/src/auth"
	"oms/src/broker"
	"oms/src/domain"
	"oms/src/metrics"
	"oms/src/store"
)

// Repair actions an operator can take on an open issue.
const (
	// ActionAcknowledge closes the issue without changing anything, for
	// example after the broker corrected its own books.
	ActionAcknowledge = "ACKNOWLEDGE"
	// ActionAcceptExecution books a quarantined overfill as an explicit,
	// attributed correction. It is the only way filled quantity can exceed
	// order quantity.
	ActionAcceptExecution = "ACCEPT_EXECUTION"
	// ActionRejectUnsubmitted closes an order that is stuck in SUBMITTING
	// after confirming with the broker that the order never arrived.
	ActionRejectUnsubmitted = "REJECT_UNSUBMITTED"
	// ActionRequeueOutbox gives a dead-lettered broker call a fresh set of
	// attempts.
	ActionRequeueOutbox = "REQUEUE_OUTBOX"
)

// DeadLetter is the details of an OUTBOX_DEAD_LETTER issue.
type DeadLetter struct {
	OutboxID  int64  `json:"outbox_id"`
	EventType string `json:"event_type"`
	Attempts  int    `json:"attempts"`
	LastError string `json:"last_error"`
}

// ResolveIssue applies an operator repair and closes the issue. Every repair
// is recorded: on the issue (who, when, which action, why) and, when it
// changes an order, as an OPERATOR event in that order's history.
func (s *Service) ResolveIssue(ctx context.Context, p auth.Principal, issueID, action, note string) (store.Issue, error) {
	if !p.CanOperate() {
		return store.Issue{}, errorf(http.StatusForbidden, "FORBIDDEN", "only operators can resolve issues")
	}
	note = strings.TrimSpace(note)
	if note == "" {
		return store.Issue{}, errorf(http.StatusUnprocessableEntity, "VALIDATION_FAILED", "a note explaining the repair is required")
	}
	issue, err := s.Store.GetIssue(ctx, issueID)
	if errors.Is(err, store.ErrNotFound) {
		return store.Issue{}, errorf(http.StatusNotFound, "ISSUE_NOT_FOUND", "no such issue")
	}
	if err != nil {
		return store.Issue{}, err
	}
	if issue.Status != store.IssueOpen {
		return store.Issue{}, errorf(http.StatusConflict, "ISSUE_NOT_OPEN", "the issue is already resolved")
	}

	var resolution string
	// repair runs inside the transaction. When needsOrder is set it receives
	// the order, already locked.
	var repair func(tx *store.Tx, o domain.Order) error
	var needsOrder bool
	switch action {
	case ActionAcknowledge:
		resolution = "ACKNOWLEDGED"
		repair = func(*store.Tx, domain.Order) error { return nil }

	case ActionAcceptExecution:
		if issue.Kind != domain.ReasonOverfill {
			return store.Issue{}, wrongKind(action, issue.Kind)
		}
		var q QuarantinedReport
		if err := json.Unmarshal(issue.Details, &q); err != nil {
			return store.Issue{}, err
		}
		resolution = "EXECUTION_ACCEPTED"
		needsOrder = true
		repair = func(tx *store.Tx, o domain.Order) error {
			look, err := tx.LookupExecution(ctx, q.Report.ExecID, q.Report.RefExecID)
			if err != nil {
				return err
			}
			if look.Existing != nil {
				return errorf(http.StatusConflict, "ALREADY_BOOKED", "execution %s is already booked; acknowledge the issue instead", q.Report.ExecID)
			}
			d := domain.DecideAccept(o, q.Report, look.Context, p.Name, issue.ID)
			if d.Outcome != domain.OutcomeApplied {
				return errorf(http.StatusConflict, "CANNOT_ACCEPT", "the execution cannot be booked: %s", d.Detail)
			}
			_, _, err = tx.Append(ctx, o.ID, o, d.Drafts)
			return err
		}

	case ActionRejectUnsubmitted:
		if issue.Kind != store.KindMissingAtBroker && issue.Kind != store.KindOutboxDead {
			return store.Issue{}, wrongKind(action, issue.Kind)
		}
		if s.Broker == nil {
			return store.Issue{}, errorf(http.StatusServiceUnavailable, "BROKER_UNAVAILABLE", "no broker connection is configured")
		}
		// Ask the broker directly. Rejecting an order the broker is in fact
		// working would leave a live order nobody is tracking.
		switch snap, err := s.Broker.GetOrder(ctx, issue.OrderID); {
		case err == nil:
			return store.Issue{}, errorf(http.StatusConflict, "BROKER_HAS_ORDER",
				"the broker has this order with status %s; run reconciliation instead", snap.Status)
		case !errors.Is(err, broker.ErrUnknownOrder):
			s.Log.Error("broker lookup failed during repair", "order_id", issue.OrderID, "err", err)
			return store.Issue{}, errorf(http.StatusServiceUnavailable, "BROKER_UNAVAILABLE", "could not confirm with the broker that the order is unknown; try again")
		}
		resolution = "ORDER_REJECTED"
		needsOrder = true
		repair = func(tx *store.Tx, o domain.Order) error {
			if o.State != domain.StateSubmitting {
				return errorf(http.StatusConflict, "ORDER_NOT_STUCK", "the order is %s, not SUBMITTING", o.State)
			}
			// A submission that is still being retried could reach the broker
			// right after the check above.
			status, err := tx.LockSubmitStatus(ctx, o.ID)
			if err != nil {
				return err
			}
			if status == store.OutboxPending {
				return errorf(http.StatusConflict, "SUBMISSION_IN_PROGRESS", "the submission is still being retried; wait until it is delivered or dead-lettered")
			}
			if _, _, err := tx.Append(ctx, o.ID, o, []domain.Draft{{
				Type: domain.EvOrderRejected, Source: domain.SourceOperator, Actor: p.Name,
				CausationID: "issue:" + issue.ID,
				Payload:     domain.OrderRejected{By: domain.ByOperator, Reason: "never reached the broker: " + note},
			}}); err != nil {
				return err
			}
			return tx.SkipOutboxFor(ctx, o.ID, "order rejected by operator "+p.Name)
		}

	case ActionRequeueOutbox:
		if issue.Kind != store.KindOutboxDead {
			return store.Issue{}, wrongKind(action, issue.Kind)
		}
		var dl DeadLetter
		if err := json.Unmarshal(issue.Details, &dl); err != nil {
			return store.Issue{}, err
		}
		resolution = "REQUEUED"
		repair = func(tx *store.Tx, _ domain.Order) error {
			ok, err := tx.RequeueOutbox(ctx, dl.OutboxID)
			if err == nil && !ok {
				return errorf(http.StatusConflict, "NOT_DEAD", "outbox message %d is no longer dead-lettered", dl.OutboxID)
			}
			return err
		}

	default:
		return store.Issue{}, errorf(http.StatusUnprocessableEntity, "UNKNOWN_ACTION",
			"action must be one of %s, %s, %s, %s", ActionAcknowledge, ActionAcceptExecution, ActionRejectUnsubmitted, ActionRequeueOutbox)
	}

	err = s.Store.Tx(ctx, func(tx *store.Tx) error {
		// Lock order first, then issue: the same order the ingest path uses.
		var o domain.Order
		if needsOrder {
			var err error
			if o, err = tx.LockOrder(ctx, issue.OrderID); err != nil {
				return err
			}
		}
		locked, err := tx.LockIssue(ctx, issue.ID)
		if err != nil {
			return err
		}
		if locked.Status != store.IssueOpen {
			return errorf(http.StatusConflict, "ISSUE_NOT_OPEN", "the issue is already resolved")
		}
		if err := repair(tx, o); err != nil {
			return err
		}
		return tx.ResolveIssue(ctx, issue.ID, p.Name, resolution, note)
	})
	if err != nil {
		return store.Issue{}, err
	}
	if action == ActionRequeueOutbox {
		s.nudge()
	}

	resolved, err := s.Store.GetIssue(ctx, issue.ID)
	if err != nil {
		return store.Issue{}, err
	}
	if resolved.ResolvedAt != nil {
		s.Metrics.RepairTime.Observe(metrics.Seconds(resolved.ResolvedAt.Sub(resolved.DetectedAt)), resolved.Kind)
	}
	s.Log.Info("issue resolved", "issue_id", issue.ID, "kind", issue.Kind, "action", action, "actor", p.Name, "order_id", issue.OrderID)
	return resolved, nil
}

func wrongKind(action, kind string) *Error {
	return errorf(http.StatusConflict, "ACTION_NOT_APPLICABLE", "%s does not apply to a %s issue", action, kind)
}
