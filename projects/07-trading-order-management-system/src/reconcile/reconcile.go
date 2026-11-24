// Package reconcile compares the OMS with the broker's own books and deals
// with every difference it finds.
//
// Webhooks are a best-effort push: they can be lost. Reconciliation is the
// pull that makes the system converge anyway. A run fetches the broker's
// orders and executions, then for each difference does one of two things:
//
//   - If the broker is simply ahead (it has an execution or a status change
//     the OMS never heard about), the missing report is synthesized and fed
//     through the same ingest path a webhook takes. The state machine and
//     every invariant apply as usual, and the repair is recorded.
//   - If the two sides contradict each other, nothing is changed and an issue
//     is opened for an operator. Trades are never edited to make a difference
//     disappear.
package reconcile

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"time"

	"oms/src/broker"
	"oms/src/domain"
	"oms/src/ids"
	"oms/src/metrics"
	"oms/src/service"
	"oms/src/store"
)

// Config tunes the reconciler. Zero values take the defaults noted.
type Config struct {
	// Grace is how long a change is left alone before reconciliation will act
	// on it, so that the normal report flow gets the first chance and
	// in-flight activity is not mistaken for drift. Default 5s.
	Grace time.Duration
	// Lookback is how far back closed orders are still compared. Open orders
	// and orders with open issues are always compared. Default 24h.
	Lookback time.Duration
	// Interval is the period of scheduled runs. Zero disables scheduling.
	Interval time.Duration
}

// Reconciler runs reconciliation passes.
type Reconciler struct {
	Config  Config
	Store   *store.Store
	Broker  broker.Client
	Service *service.Service
	IDs     ids.Generator
	Metrics *metrics.Set
	Log     *slog.Logger
}

// ErrRunInProgress is returned when another run holds the reconciliation
// lock, in this process or any other.
var ErrRunInProgress = errors.New("a reconciliation run is already in progress")

// LockKey is the PostgreSQL advisory lock that makes runs mutually exclusive
// across every process sharing the database.
const LockKey = 0x6f6d73_0002

// conditionKinds are issues that describe a difference visible in a
// snapshot. A later run that examines the order and no longer sees the
// difference closes them.
var conditionKinds = []string{
	store.KindExecutionMismatch, store.KindPhantomExecution, store.KindStateMismatch,
	store.KindQtyMismatch, store.KindMissingAtBroker, store.KindOrphanOrder,
}

// Schedule runs reconciliation every Config.Interval until ctx is cancelled.
func (r *Reconciler) Schedule(ctx context.Context) {
	if r.Config.Interval <= 0 {
		return
	}
	t := time.NewTicker(r.Config.Interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if _, err := r.Run(ctx, "schedule"); err != nil && !errors.Is(err, ErrRunInProgress) && ctx.Err() == nil {
				r.Log.Error("scheduled reconciliation failed", "err", err)
			}
		}
	}
}

// Run performs one reconciliation pass and returns its record. If the broker
// snapshot cannot be fetched the run is recorded as FAILED and an error is
// returned; in that case no issue is opened or closed, because the absence
// of data is not evidence of anything.
func (r *Reconciler) Run(ctx context.Context, trigger string) (store.Run, error) {
	conn, err := r.Store.Pool().Acquire(ctx)
	if err != nil {
		return store.Run{}, err
	}
	defer conn.Release()
	var locked bool
	if err := conn.QueryRow(ctx, `SELECT pg_try_advisory_lock($1)`, int64(LockKey)).Scan(&locked); err != nil {
		return store.Run{}, err
	}
	if !locked {
		return store.Run{}, ErrRunInProgress
	}
	defer conn.Exec(context.WithoutCancel(ctx), `SELECT pg_advisory_unlock($1)`, int64(LockKey))

	run, err := r.Store.StartRun(ctx, r.IDs.New("run"), trigger)
	if err != nil {
		return store.Run{}, err
	}
	p := &pass{Reconciler: r, run: &run, skipped: map[string]bool{}, repaired: map[string]bool{}, held: map[string]bool{}}
	runErr := p.execute(ctx)

	finished := r.Store.Now()
	run.FinishedAt = &finished
	run.Status = store.RunCompleted
	if runErr != nil {
		run.Status, run.Error = store.RunFailed, runErr.Error()
	}
	if err := r.Store.FinishRun(context.WithoutCancel(ctx), run); err != nil {
		return run, errors.Join(runErr, err)
	}
	r.Metrics.ReconRuns.Inc(run.Status)
	r.Metrics.ReconDuration.Observe(metrics.Seconds(finished.Sub(run.StartedAt)))
	r.Log.Info("reconciliation run finished", "run_id", run.ID, "status", run.Status, "orders", run.OrdersChecked,
		"executions", run.ExecutionsChecked, "drift", run.DriftFound, "auto_repaired", run.AutoRepaired,
		"issues_opened", run.IssuesOpened, "issues_cleared", run.IssuesCleared, "err", runErr)
	return run, runErr
}

// pass is the working state of one run.
type pass struct {
	*Reconciler
	run *store.Run
	// skipped holds orders the run chose not to judge because they changed
	// too recently. Their open issues are neither confirmed nor cleared.
	skipped map[string]bool
	// repaired holds orders this run changed itself.
	repaired map[string]bool
	// held holds orders with a broker execution that could not be booked and
	// is waiting for an operator. Their totals cannot agree with the broker
	// until that issue is settled, so comparing them would only restate it.
	held map[string]bool
}

func (p *pass) execute(ctx context.Context) error {
	grace := p.Config.Grace
	if grace <= 0 {
		grace = 5 * time.Second
	}
	lookback := p.Config.Lookback
	if lookback <= 0 {
		lookback = 24 * time.Hour
	}

	snap, err := p.Broker.Snapshot(ctx)
	if err != nil {
		return fmt.Errorf("broker snapshot unavailable: %w", err)
	}
	asOf := snap.AsOf.UTC()
	p.run.BrokerAsOf = &asOf
	// Broker records are aged on the broker's clock, OMS records on ours, so
	// a skew between the two clocks cannot make fresh activity look old.
	brokerCutoff := asOf.Add(-grace)
	localStart := p.run.StartedAt
	localCutoff := localStart.Add(-grace)

	orders, err := p.Store.SubmittedOrders(ctx, localStart.Add(-lookback))
	if err != nil {
		return err
	}
	orderIDs := make([]string, len(orders))
	for i, o := range orders {
		orderIDs[i] = o.ID
	}
	omsExecs, err := p.Store.ExecutionsForOrders(ctx, orderIDs)
	if err != nil {
		return err
	}

	brokerOrders := make(map[string]broker.OrderSnapshot, len(snap.Orders))
	for _, b := range snap.Orders {
		brokerOrders[b.ClientOrderID] = b
	}
	brokerExecs := make(map[string]broker.ExecutionSnapshot, len(snap.Executions))
	for _, x := range snap.Executions {
		brokerExecs[x.ExecID] = x
	}

	// 1. Executions the broker has. Oldest first, so an execution is booked
	//    before any correction that references it.
	execs := append([]broker.ExecutionSnapshot(nil), snap.Executions...)
	sort.SliceStable(execs, func(i, j int) bool { return execs[i].ExecutedAt.Before(execs[j].ExecutedAt) })
	for _, x := range execs {
		if x.ExecutedAt.After(brokerCutoff) {
			p.skipped[x.ClientOrderID] = true
			continue
		}
		p.run.ExecutionsChecked++
		if err := p.checkBrokerExecution(ctx, x); err != nil {
			return err
		}
	}

	// 2. Executions the OMS has that the broker does not.
	for _, x := range omsExecs {
		if x.RecordedAt.After(localCutoff) {
			continue
		}
		if _, ok := brokerExecs[x.BrokerExecID]; ok {
			continue
		}
		if err := p.raise(ctx, store.KindPhantomExecution, store.SeverityCritical, x.OrderID, x.BrokerExecID, map[string]any{
			"detail":        "the OMS has booked an execution that is not in the broker's records",
			"oms_execution": x,
		}); err != nil {
			return err
		}
	}

	// 3. Order status, quantity and terms.
	for _, stale := range orders {
		p.run.OrdersChecked++
		b, known := brokerOrders[stale.ID]
		if p.skipped[stale.ID] || p.held[stale.ID] || (known && b.UpdatedAt.After(brokerCutoff)) {
			// Recent broker activity on this order (its reports may still be
			// in flight) or an execution waiting for an operator: the order
			// is not judged by this run.
			p.skipped[stale.ID] = true
			continue
		}
		if err := p.checkOrder(ctx, stale.ID, b, known, localStart, localCutoff); err != nil {
			return err
		}
	}

	// 4. Orders the broker has that the OMS does not.
	inScope := make(map[string]bool, len(orderIDs))
	for _, id := range orderIDs {
		inScope[id] = true
	}
	for _, b := range snap.Orders {
		if inScope[b.ClientOrderID] || b.UpdatedAt.After(brokerCutoff) {
			continue
		}
		// Not in this run's scope; it may still be an older order.
		if _, err := p.Store.GetOrder(ctx, b.ClientOrderID); !errors.Is(err, store.ErrNotFound) {
			if err != nil {
				return err
			}
			continue
		}
		p.run.OrdersChecked++
		if err := p.raise(ctx, store.KindOrphanOrder, store.SeverityCritical, b.ClientOrderID, "", map[string]any{
			"detail":       "the broker has an order this OMS has no record of",
			"broker_order": b,
		}); err != nil {
			return err
		}
	}

	// 5. Close issues whose discrepancy was examined and is gone.
	skip := make([]string, 0, len(p.skipped))
	for id := range p.skipped {
		skip = append(skip, id)
	}
	var cleared []store.Issue
	if err := p.Store.Tx(ctx, func(tx *store.Tx) error {
		var err error
		cleared, err = tx.ClearIssuesNotSeen(ctx, p.run.ID, conditionKinds, skip)
		return err
	}); err != nil {
		return err
	}
	p.run.IssuesCleared = len(cleared)
	for _, i := range cleared {
		if i.ResolvedAt != nil {
			p.Metrics.RepairTime.Observe(metrics.Seconds(i.ResolvedAt.Sub(i.DetectedAt)), i.Kind)
		}
	}
	return nil
}

// checkBrokerExecution makes sure one broker execution is on the OMS's books
// with the same terms.
func (p *pass) checkBrokerExecution(ctx context.Context, x broker.ExecutionSnapshot) error {
	booked, err := p.Store.GetExecution(ctx, x.ExecID)
	switch {
	case err == nil:
		if booked.OrderID == x.ClientOrderID && booked.Kind == x.Kind && booked.RefBrokerExecID == x.RefExecID &&
			(x.Kind == domain.ExecBust || (booked.Qty == x.Qty && booked.Price == x.Price && booked.Fee == x.Fee)) {
			return nil
		}
		return p.raise(ctx, store.KindExecutionMismatch, store.SeverityCritical, booked.OrderID, x.ExecID, map[string]any{
			"detail":           "the OMS and the broker hold different terms for the same execution",
			"oms_execution":    booked,
			"broker_execution": x,
		})
	case !errors.Is(err, store.ErrNotFound):
		return err
	}

	// The broker has an execution the OMS never booked: a lost webhook.
	// Feed it through the normal ingest path. The event id is scoped to this
	// run so an execution that stays unbooked is re-examined by every run.
	res, err := p.Service.IngestBrokerReport(ctx,
		x.Report(fmt.Sprintf("recon:%s:exec:%s", p.run.ID, x.ExecID)), domain.SourceReconciler)
	if err != nil {
		return err
	}
	switch res.Outcome {
	case domain.OutcomeApplied:
		p.repaired[x.ClientOrderID] = true
		return p.recordRepair(ctx, store.KindMissingExecution, x.ClientOrderID, x.ExecID, map[string]any{
			"detail":           "execution found in the broker snapshot and booked",
			"broker_execution": x,
		})
	case domain.OutcomeDuplicate:
		return nil // the webhook won the race
	default:
		// Quarantined or orphaned: ingest raised the issue. It still counts
		// as drift found by this run.
		p.held[x.ClientOrderID] = true
		p.run.DriftFound++
		p.Metrics.ReconDrift.Inc(res.IssueKind)
		if res.IssueOpened {
			p.run.IssuesOpened++
		}
		return nil
	}
}

// checkOrder compares one OMS order with the broker's view of it, repairing
// what the broker is simply ahead on and raising an issue for the rest.
func (p *pass) checkOrder(ctx context.Context, orderID string, b broker.OrderSnapshot, known bool, localStart, localCutoff time.Time) error {
	o, err := p.Store.GetOrder(ctx, orderID)
	if err != nil {
		return err
	}
	if o.UpdatedAt.After(localStart) && !p.repaired[orderID] {
		// Something other than this run changed the order after the snapshot
		// was taken. The snapshot is out of date for it.
		p.skipped[orderID] = true
		return nil
	}

	if !known {
		return p.checkMissingAtBroker(ctx, o, localCutoff)
	}

	// Apply status changes the OMS missed. Each repair goes through the state
	// machine, so the loop can only move forward and ends when nothing
	// applies.
	for range 4 {
		report, ok := missedReport(o, b)
		if !ok {
			break
		}
		report.EventID = fmt.Sprintf("recon:%s:%s:%s", p.run.ID, o.ID, report.Kind)
		res, err := p.Service.IngestBrokerReport(ctx, report, domain.SourceReconciler)
		if err != nil {
			return err
		}
		if res.Outcome != domain.OutcomeApplied {
			break
		}
		p.repaired[orderID] = true
		if err := p.recordRepair(ctx, store.KindMissedReport, o.ID, string(report.Kind), map[string]any{
			"detail":        fmt.Sprintf("the broker reports %s; applied the missing %s report", b.Status, report.Kind),
			"oms_state":     o.State,
			"broker_status": b.Status,
		}); err != nil {
			return err
		}
		o = *res.Order
	}

	details := map[string]any{
		"oms_state": o.State, "oms_qty": o.Qty, "oms_filled_qty": o.FilledQty, "oms_limit_price": o.LimitPrice,
		"broker_order": b,
	}
	if want, ok := expectedState(b.Status); !ok || want != o.State {
		details["detail"] = fmt.Sprintf("the OMS has the order as %s but the broker reports %s", o.State, b.Status)
		return p.raise(ctx, store.KindStateMismatch, store.SeverityHigh, o.ID, "", details)
	}
	if o.FilledQty != b.CumQty || o.Qty != b.Qty || !samePrice(o, b) {
		details["detail"] = "the OMS and the broker disagree on quantity, filled quantity or limit price"
		return p.raise(ctx, store.KindQtyMismatch, store.SeverityHigh, o.ID, "", details)
	}
	return nil
}

// checkMissingAtBroker handles an OMS order the broker snapshot does not
// contain.
func (p *pass) checkMissingAtBroker(ctx context.Context, o domain.Order, localCutoff time.Time) error {
	if o.State == domain.StateRejected {
		return nil // refused at submission: the broker never created it
	}
	status, err := p.Store.SubmitStatus(ctx, o.ID)
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		return err
	}
	switch status {
	case store.OutboxPending:
		// Still being sent. If it never gets there it is dead-lettered, which
		// raises its own issue.
		p.skipped[o.ID] = true
		return nil
	case store.OutboxDead:
		// The dead-letter issue covers this while it is open.
		open, err := p.Store.ListIssues(ctx, store.IssueOpen, o.ID, 0)
		if err != nil {
			return err
		}
		for _, i := range open {
			if i.Kind == store.KindOutboxDead {
				return nil
			}
		}
	}
	if o.SubmittedAt != nil && o.SubmittedAt.After(localCutoff) {
		p.skipped[o.ID] = true
		return nil
	}
	return p.raise(ctx, store.KindMissingAtBroker, store.SeverityHigh, o.ID, "", map[string]any{
		"detail":        "the OMS submitted this order but the broker has no record of it",
		"oms_state":     o.State,
		"submit_status": status,
	})
}

// missedReport returns the report that would bring an OMS order into line
// with the broker when the broker is strictly ahead of it.
func missedReport(o domain.Order, b broker.OrderSnapshot) (domain.BrokerReport, bool) {
	r := domain.BrokerReport{OrderID: o.ID, BrokerOrderID: b.BrokerOrderID, Reason: b.Reason, EventTime: b.UpdatedAt}
	if !o.State.Live() {
		return r, false
	}
	switch b.Status {
	case broker.StatusCancelled:
		r.Kind = domain.ReportCancelled
		return r, true
	case broker.StatusExpired:
		r.Kind = domain.ReportExpired
		return r, true
	case broker.StatusRejected:
		r.Kind = domain.ReportReject
		return r, o.FilledQty == 0
	}
	if o.PendingReplace != nil && o.PendingReplace.Qty == b.Qty &&
		samePricePtr(o.PendingReplace.LimitPrice, b.LimitPrice) && (o.Qty != b.Qty || !samePrice(o, b)) {
		r.Kind, r.OrderQty, r.LimitPrice = domain.ReportReplaced, b.Qty, b.LimitPrice
		return r, true
	}
	if o.State == domain.StateSubmitting && b.Status != broker.StatusReceived {
		r.Kind = domain.ReportAck
		return r, true
	}
	return r, false
}

// expectedState maps a broker status to the OMS state that matches it.
func expectedState(status string) (domain.State, bool) {
	switch status {
	case broker.StatusReceived:
		return domain.StateSubmitting, true
	case broker.StatusWorking:
		return domain.StateAcknowledged, true
	case broker.StatusPartiallyFilled:
		return domain.StatePartiallyFilled, true
	case broker.StatusFilled:
		return domain.StateFilled, true
	case broker.StatusCancelled:
		return domain.StateCancelled, true
	case broker.StatusRejected:
		return domain.StateRejected, true
	case broker.StatusExpired:
		return domain.StateExpired, true
	}
	return "", false
}

func samePrice(o domain.Order, b broker.OrderSnapshot) bool {
	return samePricePtr(o.LimitPrice, b.LimitPrice)
}

func samePricePtr[T comparable](a, b *T) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
}

// raise opens (or re-confirms) an issue found by this run.
func (p *pass) raise(ctx context.Context, kind, severity, orderID, subject string, details map[string]any) error {
	fingerprint := kind + ":" + orderID
	if subject != "" {
		fingerprint += ":" + subject
	}
	var opened bool
	err := p.Store.Tx(ctx, func(tx *store.Tx) error {
		var err error
		_, opened, err = tx.RaiseIssue(ctx, store.NewIssue{
			ID: p.IDs.New("iss"), RunID: p.run.ID, OrderID: orderID, Kind: kind,
			Severity: severity, Fingerprint: fingerprint, Details: details,
		})
		return err
	})
	if err != nil {
		return err
	}
	p.run.DriftFound++
	p.Metrics.ReconDrift.Inc(kind)
	if opened {
		p.run.IssuesOpened++
		p.Log.Warn("reconciliation issue opened", "kind", kind, "severity", severity, "order_id", orderID, "run_id", p.run.ID)
	}
	return nil
}

// recordRepair files a discrepancy that this run found and fixed.
func (p *pass) recordRepair(ctx context.Context, kind, orderID, subject string, details map[string]any) error {
	err := p.Store.Tx(ctx, func(tx *store.Tx) error {
		return tx.RecordRepair(ctx, store.NewIssue{
			ID: p.IDs.New("iss"), RunID: p.run.ID, OrderID: orderID, Kind: kind, Severity: store.SeverityMedium,
			Fingerprint: fmt.Sprintf("%s:%s:%s:%s", kind, orderID, subject, p.run.ID), Details: details,
		}, "reconciler", "AUTO_REPAIRED")
	})
	if err != nil {
		return err
	}
	p.run.DriftFound++
	p.run.AutoRepaired++
	p.Metrics.ReconDrift.Inc(kind)
	p.Metrics.ReconRepairs.Inc(kind)
	p.Log.Info("reconciliation repaired drift", "kind", kind, "order_id", orderID, "subject", subject, "run_id", p.run.ID)
	return nil
}
