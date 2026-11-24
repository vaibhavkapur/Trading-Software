package store

import (
	"context"
	"encoding/json"
	"time"

	"github.com/jackc/pgx/v5"

	"oms/src/domain"
)

// Issue severities.
const (
	SeverityLow      = "LOW"
	SeverityMedium   = "MEDIUM"
	SeverityHigh     = "HIGH"
	SeverityCritical = "CRITICAL"
)

// Issue kinds. The quarantine reasons in the domain package (OVERFILL,
// UNEXPECTED_EXECUTION, CONFLICTING_REPORT, INVALID_REPORT) are issue kinds
// too.
const (
	// Raised while ingesting a broker report.
	KindOrphanEvent = "ORPHAN_BROKER_EVENT" // a report for an order this OMS does not have

	// Raised by the outbox dispatcher.
	KindOutboxDead = "OUTBOX_DEAD_LETTER" // a broker call was given up on

	// Found by reconciliation and repaired from the broker snapshot.
	KindMissingExecution = "MISSING_EXECUTION" // the broker has an execution the OMS had not booked
	KindMissedReport     = "MISSED_REPORT"     // the broker moved an order on and the OMS never heard

	// Found by reconciliation and left for an operator.
	KindExecutionMismatch = "EXECUTION_MISMATCH"  // both sides have the execution, with different terms
	KindPhantomExecution  = "PHANTOM_EXECUTION"   // the OMS booked an execution the broker does not have
	KindStateMismatch     = "STATE_MISMATCH"      // order status differs and the broker is not simply ahead
	KindQtyMismatch       = "QUANTITY_MISMATCH"   // order quantity, limit price or filled quantity differs
	KindMissingAtBroker   = "MISSING_AT_BROKER"   // the OMS sent an order the broker does not have
	KindOrphanOrder       = "ORPHAN_BROKER_ORDER" // the broker has an order the OMS does not have
)

// Issue statuses.
const (
	IssueOpen     = "OPEN"
	IssueResolved = "RESOLVED"
)

// Issue is one entry in the exception queue.
type Issue struct {
	ID       string `json:"id"`
	RunID    string `json:"run_id,omitempty"`
	OrderID  string `json:"order_id,omitempty"`
	Kind     string `json:"kind"`
	Severity string `json:"severity"`
	Status   string `json:"status"`
	// Fingerprint identifies the discrepancy. Seeing the same one again
	// updates the open issue rather than opening a second.
	Fingerprint string          `json:"fingerprint"`
	Details     json.RawMessage `json:"details"`
	Occurrences int             `json:"occurrences"`
	DetectedAt  time.Time       `json:"detected_at"`
	LastSeenAt  time.Time       `json:"last_seen_at"`
	ResolvedAt  *time.Time      `json:"resolved_at,omitempty"`
	ResolvedBy  string          `json:"resolved_by,omitempty"`
	Resolution  string          `json:"resolution,omitempty"`
	Note        string          `json:"note,omitempty"`
}

const issueCols = `id, COALESCE(run_id, ''), order_id, kind, severity, status, fingerprint, details, occurrences,
	detected_at, last_seen_at, resolved_at, resolved_by, resolution, note`

func scanIssue(row pgx.Row) (Issue, error) {
	var i Issue
	var details []byte
	if err := row.Scan(&i.ID, &i.RunID, &i.OrderID, &i.Kind, &i.Severity, &i.Status, &i.Fingerprint, &details,
		&i.Occurrences, &i.DetectedAt, &i.LastSeenAt, &i.ResolvedAt, &i.ResolvedBy, &i.Resolution, &i.Note); err != nil {
		return Issue{}, notFound(err)
	}
	i.Details = details
	i.DetectedAt, i.LastSeenAt, i.ResolvedAt = utc(i.DetectedAt), utc(i.LastSeenAt), utcPtr(i.ResolvedAt)
	return i, nil
}

// NewIssue describes an issue to raise.
type NewIssue struct {
	ID          string
	RunID       string
	OrderID     string
	Kind        string
	Severity    string
	Fingerprint string
	Details     any
}

// RaiseIssue opens an issue, or records another sighting of an open issue
// with the same fingerprint. It returns the issue's id and whether it is new.
func (t *Tx) RaiseIssue(ctx context.Context, n NewIssue) (id string, opened bool, err error) {
	details, err := json.Marshal(n.Details)
	if err != nil {
		return "", false, err
	}
	var runID *string
	if n.RunID != "" {
		runID = &n.RunID
	}
	err = t.tx.QueryRow(ctx, `INSERT INTO reconciliation_issues
		(id, run_id, order_id, kind, severity, status, fingerprint, details, detected_at, last_seen_at, last_seen_run)
		VALUES ($1, $2, $3, $4, $5, 'OPEN', $6, $7, $8, $8, $9)
		ON CONFLICT (fingerprint) WHERE status = 'OPEN' DO UPDATE SET
			occurrences = reconciliation_issues.occurrences + 1,
			last_seen_at = EXCLUDED.last_seen_at,
			last_seen_run = EXCLUDED.last_seen_run,
			details = EXCLUDED.details
		RETURNING id, (xmax = 0)`,
		n.ID, runID, n.OrderID, n.Kind, n.Severity, n.Fingerprint, details, t.Now, n.RunID).Scan(&id, &opened)
	return id, opened, err
}

// RecordRepair files an issue that was detected and repaired in one step, so
// that every drift the reconciler fixed is still on record.
func (t *Tx) RecordRepair(ctx context.Context, n NewIssue, resolvedBy, resolution string) error {
	details, err := json.Marshal(n.Details)
	if err != nil {
		return err
	}
	var runID *string
	if n.RunID != "" {
		runID = &n.RunID
	}
	_, err = t.tx.Exec(ctx, `INSERT INTO reconciliation_issues
		(id, run_id, order_id, kind, severity, status, fingerprint, details, detected_at, last_seen_at, last_seen_run,
		 resolved_at, resolved_by, resolution)
		VALUES ($1, $2, $3, $4, $5, 'RESOLVED', $6, $7, $8, $8, $9, $8, $10, $11)`,
		n.ID, runID, n.OrderID, n.Kind, n.Severity, n.Fingerprint, details, t.Now, n.RunID, resolvedBy, resolution)
	return err
}

// LockIssue reads an issue and locks it for the transaction.
func (t *Tx) LockIssue(ctx context.Context, id string) (Issue, error) {
	return scanIssue(t.tx.QueryRow(ctx, `SELECT `+issueCols+` FROM reconciliation_issues WHERE id = $1 FOR UPDATE`, id))
}

// ResolveIssue closes an open issue.
func (t *Tx) ResolveIssue(ctx context.Context, id, by, resolution, note string) error {
	_, err := t.tx.Exec(ctx, `UPDATE reconciliation_issues
		SET status = 'RESOLVED', resolved_at = $2, resolved_by = $3, resolution = $4, note = $5
		WHERE id = $1 AND status = 'OPEN'`, id, t.Now, by, resolution, note)
	return err
}

// ClearIssuesNotSeen closes open issues of the given kinds that the given run
// did not observe again: the discrepancy is gone. Issues for the orders in
// skipOrders are left alone, because the run did not examine those orders.
// It returns the closed issues.
func (t *Tx) ClearIssuesNotSeen(ctx context.Context, runID string, kinds, skipOrders []string) ([]Issue, error) {
	if skipOrders == nil {
		skipOrders = []string{} // a NULL array would make the NOT ... ANY test NULL for every row
	}
	rows, err := t.tx.Query(ctx, `UPDATE reconciliation_issues
		SET status = 'RESOLVED', resolved_at = $1, resolved_by = 'reconciler', resolution = 'CLEARED',
		    note = 'no longer observed by run ' || $2
		WHERE status = 'OPEN' AND kind = ANY($3) AND last_seen_run <> $2 AND NOT (order_id = ANY($4))
		RETURNING `+issueCols, t.Now, runID, kinds, skipOrders)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Issue
	for rows.Next() {
		i, err := scanIssue(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, i)
	}
	return out, rows.Err()
}

// GetIssue reads one issue.
func (s *Store) GetIssue(ctx context.Context, id string) (Issue, error) {
	return scanIssue(s.pool.QueryRow(ctx, `SELECT `+issueCols+` FROM reconciliation_issues WHERE id = $1`, id))
}

// ListIssues returns issues, most severe and oldest first. status and
// orderID filter when non-empty.
func (s *Store) ListIssues(ctx context.Context, status, orderID string, limit int) ([]Issue, error) {
	if limit <= 0 || limit > 1000 {
		limit = 200
	}
	rows, err := s.pool.Query(ctx, `SELECT `+issueCols+` FROM reconciliation_issues
		WHERE ($1 = '' OR status = $1) AND ($2 = '' OR order_id = $2)
		ORDER BY CASE severity WHEN 'CRITICAL' THEN 0 WHEN 'HIGH' THEN 1 WHEN 'MEDIUM' THEN 2 ELSE 3 END,
		         detected_at, id
		LIMIT $3`, status, orderID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Issue{}
	for rows.Next() {
		i, err := scanIssue(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, i)
	}
	return out, rows.Err()
}

// OpenIssueCounts returns the number of open issues per severity.
func (s *Store) OpenIssueCounts(ctx context.Context) (map[string]int64, error) {
	rows, err := s.pool.Query(ctx, `SELECT severity, count(*) FROM reconciliation_issues WHERE status = 'OPEN' GROUP BY severity`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]int64{}
	for rows.Next() {
		var sev string
		var n int64
		if err := rows.Scan(&sev, &n); err != nil {
			return nil, err
		}
		out[sev] = n
	}
	return out, rows.Err()
}

// Run is one reconciliation pass.
type Run struct {
	ID         string     `json:"id"`
	Trigger    string     `json:"trigger"`
	Status     string     `json:"status"`
	Error      string     `json:"error,omitempty"`
	StartedAt  time.Time  `json:"started_at"`
	FinishedAt *time.Time `json:"finished_at,omitempty"`
	// BrokerAsOf is the broker's clock when it produced the snapshot.
	BrokerAsOf        *time.Time `json:"broker_as_of,omitempty"`
	OrdersChecked     int        `json:"orders_checked"`
	ExecutionsChecked int        `json:"executions_checked"`
	DriftFound        int        `json:"drift_found"`
	AutoRepaired      int        `json:"auto_repaired"`
	IssuesOpened      int        `json:"issues_opened"`
	IssuesCleared     int        `json:"issues_cleared"`
}

// Run statuses.
const (
	RunRunning   = "RUNNING"
	RunCompleted = "COMPLETED"
	RunFailed    = "FAILED"
)

// StartRun records that a reconciliation run has begun.
func (s *Store) StartRun(ctx context.Context, id, trigger string) (Run, error) {
	r := Run{ID: id, Trigger: trigger, Status: RunRunning, StartedAt: s.clock.Now()}
	_, err := s.pool.Exec(ctx, `INSERT INTO reconciliation_runs (id, trigger, status, started_at) VALUES ($1, $2, $3, $4)`,
		r.ID, r.Trigger, r.Status, r.StartedAt)
	return r, err
}

// FinishRun stores a run's final status and counters.
func (s *Store) FinishRun(ctx context.Context, r Run) error {
	_, err := s.pool.Exec(ctx, `UPDATE reconciliation_runs SET status = $2, error = $3, finished_at = $4,
		broker_as_of = $5, orders_checked = $6, executions_checked = $7, drift_found = $8, auto_repaired = $9,
		issues_opened = $10, issues_cleared = $11 WHERE id = $1`,
		r.ID, r.Status, r.Error, r.FinishedAt, r.BrokerAsOf, r.OrdersChecked, r.ExecutionsChecked,
		r.DriftFound, r.AutoRepaired, r.IssuesOpened, r.IssuesCleared)
	return err
}

// GetRun reads one reconciliation run.
func (s *Store) GetRun(ctx context.Context, id string) (Run, error) {
	var r Run
	err := s.pool.QueryRow(ctx, `SELECT id, trigger, status, error, started_at, finished_at, broker_as_of,
		orders_checked, executions_checked, drift_found, auto_repaired, issues_opened, issues_cleared
		FROM reconciliation_runs WHERE id = $1`, id).Scan(&r.ID, &r.Trigger, &r.Status, &r.Error, &r.StartedAt,
		&r.FinishedAt, &r.BrokerAsOf, &r.OrdersChecked, &r.ExecutionsChecked, &r.DriftFound, &r.AutoRepaired,
		&r.IssuesOpened, &r.IssuesCleared)
	if err != nil {
		return Run{}, notFound(err)
	}
	r.StartedAt, r.FinishedAt, r.BrokerAsOf = utc(r.StartedAt), utcPtr(r.FinishedAt), utcPtr(r.BrokerAsOf)
	return r, nil
}

// SubmittedOrders returns the orders a reconciliation run compares: every
// order that was handed to the broker and is still open, was last updated at
// or after since, or has an open issue.
func (s *Store) SubmittedOrders(ctx context.Context, since time.Time) ([]domain.Order, error) {
	rows, err := s.pool.Query(ctx, `SELECT `+orderCols+` FROM orders
		WHERE submitted_at IS NOT NULL
		  AND (state IN ('SUBMITTING', 'ACKNOWLEDGED', 'PARTIALLY_FILLED')
		       OR updated_at >= $1
		       OR id IN (SELECT order_id FROM reconciliation_issues WHERE status = 'OPEN'))
		ORDER BY created_at, id`, since)
	if err != nil {
		return nil, err
	}
	return collectOrders(rows)
}

// SubmitStatus returns the outbox status of an order's submission message.
func (s *Store) SubmitStatus(ctx context.Context, orderID string) (status string, err error) {
	err = s.pool.QueryRow(ctx, `SELECT status FROM outbox WHERE aggregate_id = $1 AND event_type = 'SUBMIT_ORDER'
		ORDER BY id LIMIT 1`, orderID).Scan(&status)
	return status, notFound(err)
}
