package scheduler

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/trading-software-projects/broker-reliability-reconciliation/src/connectors"
	"github.com/trading-software-projects/broker-reliability-reconciliation/src/domain"
	"github.com/trading-software-projects/broker-reliability-reconciliation/src/issues"
	"github.com/trading-software-projects/broker-reliability-reconciliation/src/metrics"
	"github.com/trading-software-projects/broker-reliability-reconciliation/src/reconcile"
)

// RunRequest specifies parameters for an on-demand or scheduled reconciliation run.
type RunRequest struct {
	AccountID     string
	AsOf          time.Time
	PolicyVersion string
	Trigger       string // "on_demand" or "scheduled"
}

// Runner coordinates paginated connector ingestion, reconciliation, issue persistence, and metrics.
type Runner struct {
	internal connectors.InternalConnector
	broker   connectors.BrokerConnector
	engine   *reconcile.Engine
	store    *issues.Store
	metrics  *metrics.Collector
	pageSize int
	retryCfg connectors.RetryConfig
	nowFn    func() time.Time
}

// NewRunner wires connectors, reconciliation engine, issue store, and metrics collector.
func NewRunner(
	internal connectors.InternalConnector,
	broker connectors.BrokerConnector,
	engine *reconcile.Engine,
	store *issues.Store,
	collector *metrics.Collector,
) *Runner {
	if collector == nil {
		collector = metrics.NewCollector()
	}
	return &Runner{
		internal: internal,
		broker:   broker,
		engine:   engine,
		store:    store,
		metrics:  collector,
		pageSize: 50,
		retryCfg: connectors.DefaultRetryConfig(),
		nowFn:    func() time.Time { return time.Now().UTC() },
	}
}

// WithPageSize configures the pagination batch size used against connectors.
func (r *Runner) WithPageSize(size int) *Runner {
	if size > 0 {
		r.pageSize = size
	}
	return r
}

// WithRetryConfig configures retry behavior for transient connector errors.
func (r *Runner) WithRetryConfig(cfg connectors.RetryConfig) *Runner {
	r.retryCfg = cfg
	return r
}

// Execute runs a single end-to-end reconciliation pass and persists the run and detected issues.
func (r *Runner) Execute(ctx context.Context, req RunRequest) (domain.ReconRun, []domain.ReconIssue, error) {
	started := r.nowFn().UTC()
	trigger := req.Trigger
	if trigger == "" {
		trigger = "on_demand"
	}
	asof := req.AsOf.UTC()
	if asof.IsZero() {
		asof = started
	}

	policy, err := r.store.GetPolicy(req.PolicyVersion)
	if err != nil {
		return domain.ReconRun{}, nil, err
	}

	run := domain.ReconRun{
		ID:            fmt.Sprintf("run-pending-%d", started.UnixNano()),
		AccountID:     req.AccountID,
		Cutoff:        asof,
		PolicyVersion: policy.Version,
		Trigger:       trigger,
		Status:        domain.RunStatusRunning,
		StartedAt:     started,
	}

	failRun := func(source, reason string, cause error) (domain.ReconRun, []domain.ReconIssue, error) {
		completed := r.nowFn().UTC()
		run.ID = reconcile.DeriveRunID(req.AccountID, asof, policy.Version, fmt.Sprintf("failed-%d", completed.UnixNano()))
		run.Status = domain.RunStatusFailed
		run.CompletedAt = &completed
		run.Error = cause.Error()
		if source != "" {
			r.metrics.IncConnectorError(source, reason)
		}
		r.metrics.IncRun(domain.RunStatusFailed)
		_ = r.store.SaveRun(run)
		return run, nil, cause
	}

	classifyErr := func(err error) string {
		switch {
		case errors.Is(err, connectors.ErrDisconnected):
			return "disconnected"
		case errors.Is(err, connectors.ErrRateLimited):
			return "rate_limited"
		default:
			return "fetch_error"
		}
	}

	lOrders, _, err := connectors.FetchAllPages(ctx, req.AccountID, asof, r.pageSize, r.retryCfg, r.internal.ListOrders)
	if err != nil {
		return failRun(r.internal.Name(), classifyErr(err), fmt.Errorf("internal orders: %w", err))
	}
	lFills, _, err := connectors.FetchAllPages(ctx, req.AccountID, asof, r.pageSize, r.retryCfg, r.internal.ListFills)
	if err != nil {
		return failRun(r.internal.Name(), classifyErr(err), fmt.Errorf("internal fills: %w", err))
	}
	iPos, _, err := connectors.FetchAllPages(ctx, req.AccountID, asof, r.pageSize, r.retryCfg, r.internal.ListPositions)
	if err != nil {
		return failRun(r.internal.Name(), classifyErr(err), fmt.Errorf("internal positions: %w", err))
	}
	iCash, _, err := connectors.FetchAllPages(ctx, req.AccountID, asof, r.pageSize, r.retryCfg, r.internal.ListCashBalances)
	if err != nil {
		return failRun(r.internal.Name(), classifyErr(err), fmt.Errorf("internal cash: %w", err))
	}

	bOrders, _, err := connectors.FetchAllPages(ctx, req.AccountID, asof, r.pageSize, r.retryCfg, r.broker.ListOrders)
	if err != nil {
		return failRun(r.broker.Name(), classifyErr(err), fmt.Errorf("broker orders: %w", err))
	}
	bFills, _, err := connectors.FetchAllPages(ctx, req.AccountID, asof, r.pageSize, r.retryCfg, r.broker.ListFills)
	if err != nil {
		return failRun(r.broker.Name(), classifyErr(err), fmt.Errorf("broker fills: %w", err))
	}
	bPos, _, err := connectors.FetchAllPages(ctx, req.AccountID, asof, r.pageSize, r.retryCfg, r.broker.ListPositions)
	if err != nil {
		return failRun(r.broker.Name(), classifyErr(err), fmt.Errorf("broker positions: %w", err))
	}
	bCash, _, err := connectors.FetchAllPages(ctx, req.AccountID, asof, r.pageSize, r.retryCfg, r.broker.ListCashBalances)
	if err != nil {
		return failRun(r.broker.Name(), classifyErr(err), fmt.Errorf("broker cash: %w", err))
	}

	res, err := r.engine.Reconcile("pending", reconcile.SnapshotInput{
		AccountID:    req.AccountID,
		Cutoff:       asof,
		Policy:       policy,
		LedgerOrders: lOrders,
		BrokerOrders: bOrders,
		LedgerFills:  lFills,
		BrokerFills:  bFills,
		InternalPos:  iPos,
		BrokerPos:    bPos,
		InternalCash: iCash,
		BrokerCash:   bCash,
	})
	if err != nil {
		return failRun("", "reconcile_error", err)
	}

	run.ID = reconcile.DeriveRunID(req.AccountID, asof, policy.Version, res.InputDigest)
	run.InputDigest = res.InputDigest
	run.Summary = res.Summary

	persistedIssues := make([]domain.ReconIssue, 0, len(res.Issues))
	issueIDs := make([]string, 0, len(res.Issues))
	for _, iss := range res.Issues {
		iss.RunID = run.ID
		saved, err := r.store.UpsertIssue(iss)
		if err != nil {
			return failRun("store", "write_rejected", err)
		}
		persistedIssues = append(persistedIssues, saved)
		issueIDs = append(issueIDs, saved.ID)
	}

	completed := r.nowFn().UTC()
	run.IssueIDs = issueIDs
	run.Status = domain.RunStatusCompleted
	run.CompletedAt = &completed

	if err := r.store.SaveRun(run); err != nil {
		r.metrics.IncConnectorError("store", "write_rejected")
		r.metrics.IncRun(domain.RunStatusFailed)
		return run, persistedIssues, err
	}

	r.metrics.ObserveLateFills(res.LateObservations)
	r.metrics.IncRun(domain.RunStatusCompleted)
	return run, persistedIssues, nil
}

// StartPeriodic launches a background reconciliation loop until ctx is canceled.
func (r *Runner) StartPeriodic(ctx context.Context, accountID string, interval time.Duration, policyVersion string, asofFn func() time.Time) {
	if interval <= 0 {
		return
	}
	if asofFn == nil {
		asofFn = func() time.Time { return time.Now().UTC() }
	}
	ticker := time.NewTicker(interval)
	go func() {
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				_, _, _ = r.Execute(ctx, RunRequest{
					AccountID:     accountID,
					AsOf:          asofFn(),
					PolicyVersion: policyVersion,
					Trigger:       "scheduled",
				})
			}
		}
	}()
}
