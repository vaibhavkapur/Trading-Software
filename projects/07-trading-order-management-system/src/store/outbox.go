package store

import (
	"context"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// Outbox message types. Each is one call to the broker.
const (
	OutboxSubmit  = "SUBMIT_ORDER"
	OutboxCancel  = "CANCEL_ORDER"
	OutboxReplace = "REPLACE_ORDER"
)

// Outbox row statuses.
const (
	OutboxPending   = "PENDING"   // waiting to be sent, or being retried
	OutboxDelivered = "DELIVERED" // the broker accepted the request
	OutboxSkipped   = "SKIPPED"   // no longer needed when its turn came
	OutboxRejected  = "REJECTED"  // the broker permanently refused the request
	OutboxDead      = "DEAD"      // gave up after too many attempts; needs an operator
)

// OutboxRow is one queued broker call.
type OutboxRow struct {
	ID          int64      `json:"id"`
	AggregateID string     `json:"aggregate_id"`
	EventType   string     `json:"event_type"`
	Status      string     `json:"status"`
	Attempts    int        `json:"attempts"`
	AvailableAt time.Time  `json:"available_at"`
	LockedBy    string     `json:"locked_by,omitempty"`
	LastError   string     `json:"last_error,omitempty"`
	CreatedAt   time.Time  `json:"created_at"`
	DeliveredAt *time.Time `json:"delivered_at,omitempty"`
}

const outboxCols = `id, aggregate_id, event_type, status, attempts, available_at, locked_by, last_error, created_at, delivered_at`

func scanOutbox(row pgx.Row) (OutboxRow, error) {
	var r OutboxRow
	if err := row.Scan(&r.ID, &r.AggregateID, &r.EventType, &r.Status, &r.Attempts, &r.AvailableAt,
		&r.LockedBy, &r.LastError, &r.CreatedAt, &r.DeliveredAt); err != nil {
		return OutboxRow{}, notFound(err)
	}
	r.AvailableAt, r.CreatedAt, r.DeliveredAt = utc(r.AvailableAt), utc(r.CreatedAt), utcPtr(r.DeliveredAt)
	return r, nil
}

// prefixCols qualifies a comma-separated column list with a table name.
func prefixCols(table, cols string) string {
	parts := strings.Split(cols, ",")
	for i, c := range parts {
		parts[i] = table + "." + strings.TrimSpace(c)
	}
	return strings.Join(parts, ", ")
}

// Enqueue adds a broker call to the outbox. Because it runs in the same
// transaction as the events that caused it, a call is queued if and only if
// those events were committed.
func (t *Tx) Enqueue(ctx context.Context, aggregateID, eventType string) (int64, error) {
	var id int64
	err := t.tx.QueryRow(ctx, `INSERT INTO outbox (aggregate_id, event_type, available_at, created_at)
		VALUES ($1, $2, $3, $3) RETURNING id`, aggregateID, eventType, t.Now).Scan(&id)
	return id, err
}

// ClaimOutbox leases up to limit due messages to a worker for the lease
// duration and returns them, oldest first.
//
// Two rules make delivery safe with any number of workers:
//   - A message is only eligible when no older message for the same order is
//     still pending, so a cancel is never sent ahead of its submit.
//   - SKIP LOCKED plus the lease means two workers never hold the same message,
//     and a message whose worker died becomes eligible again when the lease
//     runs out. Delivery is therefore at-least-once; the broker deduplicates.
func (s *Store) ClaimOutbox(ctx context.Context, worker string, limit int, lease time.Duration) ([]OutboxRow, error) {
	now := s.clock.Now()
	rows, err := s.pool.Query(ctx, `
		WITH due AS (
			SELECT o.id FROM outbox o
			WHERE o.status = 'PENDING'
			  AND o.available_at <= $1
			  AND (o.locked_until IS NULL OR o.locked_until <= $1)
			  AND NOT EXISTS (
				SELECT 1 FROM outbox older
				WHERE older.aggregate_id = o.aggregate_id AND older.status = 'PENDING' AND older.id < o.id)
			ORDER BY o.id
			LIMIT $2
			FOR UPDATE OF o SKIP LOCKED
		)
		UPDATE outbox SET locked_until = $3, locked_by = $4, attempts = attempts + 1
		FROM due WHERE outbox.id = due.id
		RETURNING `+prefixCols("outbox", outboxCols), now, limit, now.Add(lease), worker)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []OutboxRow
	for rows.Next() {
		r, err := scanOutbox(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	// RETURNING does not guarantee order.
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

// FinishOutbox moves a leased message to a final status. It reports false if
// the lease was lost to another worker in the meantime, in which case nothing
// is changed and that worker owns the outcome.
func (t *Tx) FinishOutbox(ctx context.Context, id int64, worker, status, lastError string) (bool, error) {
	var deliveredAt *time.Time
	if status == OutboxDelivered {
		deliveredAt = &t.Now
	}
	tag, err := t.tx.Exec(ctx, `UPDATE outbox
		SET status = $3, last_error = $4, delivered_at = $5, locked_until = NULL
		WHERE id = $1 AND locked_by = $2 AND status = 'PENDING'`, id, worker, status, lastError, deliveredAt)
	return tag.RowsAffected() == 1, err
}

// RetryOutbox releases a leased message to be tried again at availableAt.
func (t *Tx) RetryOutbox(ctx context.Context, id int64, worker string, availableAt time.Time, lastError string) (bool, error) {
	tag, err := t.tx.Exec(ctx, `UPDATE outbox
		SET available_at = $3, last_error = $4, locked_until = NULL
		WHERE id = $1 AND locked_by = $2 AND status = 'PENDING'`, id, worker, availableAt, lastError)
	return tag.RowsAffected() == 1, err
}

// RequeueOutbox puts a DEAD message back in the queue with a fresh attempt
// budget. It is an operator repair.
func (t *Tx) RequeueOutbox(ctx context.Context, id int64) (bool, error) {
	tag, err := t.tx.Exec(ctx, `UPDATE outbox
		SET status = 'PENDING', attempts = 0, available_at = $2, locked_until = NULL, locked_by = ''
		WHERE id = $1 AND status = 'DEAD'`, id, t.Now)
	return tag.RowsAffected() == 1, err
}

// GetOutbox reads one message.
func (s *Store) GetOutbox(ctx context.Context, id int64) (OutboxRow, error) {
	return scanOutbox(s.pool.QueryRow(ctx, `SELECT `+outboxCols+` FROM outbox WHERE id = $1`, id))
}

// ListOutbox returns an order's messages, oldest first.
func (s *Store) ListOutbox(ctx context.Context, aggregateID string) ([]OutboxRow, error) {
	rows, err := s.pool.Query(ctx, `SELECT `+outboxCols+` FROM outbox WHERE aggregate_id = $1 ORDER BY id`, aggregateID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []OutboxRow{}
	for rows.Next() {
		r, err := scanOutbox(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// OutboxStats summarises the queue for metrics.
type OutboxStats struct {
	Pending int64
	Dead    int64
	// OldestPendingAge is how long the oldest undelivered message has waited.
	OldestPendingAge time.Duration
}

func (s *Store) OutboxStats(ctx context.Context) (OutboxStats, error) {
	var st OutboxStats
	var oldest *time.Time
	err := s.pool.QueryRow(ctx, `SELECT
		count(*) FILTER (WHERE status = 'PENDING'),
		count(*) FILTER (WHERE status = 'DEAD'),
		min(created_at) FILTER (WHERE status = 'PENDING')
		FROM outbox WHERE status IN ('PENDING', 'DEAD')`).Scan(&st.Pending, &st.Dead, &oldest)
	if err != nil {
		return st, err
	}
	if oldest != nil {
		if age := s.clock.Now().Sub(*oldest); age > 0 {
			st.OldestPendingAge = age
		}
	}
	return st, nil
}

// LockSubmitStatus returns the status of an order's submission message and
// locks that message, so it cannot be claimed or requeued until the
// transaction ends.
func (t *Tx) LockSubmitStatus(ctx context.Context, orderID string) (string, error) {
	var status string
	err := t.tx.QueryRow(ctx, `SELECT status FROM outbox WHERE aggregate_id = $1 AND event_type = 'SUBMIT_ORDER'
		ORDER BY id LIMIT 1 FOR UPDATE`, orderID).Scan(&status)
	return status, notFound(err)
}

// SkipOutboxFor retires every message of an order that has not been
// delivered, so nothing more is sent for it.
func (t *Tx) SkipOutboxFor(ctx context.Context, orderID, reason string) error {
	_, err := t.tx.Exec(ctx, `UPDATE outbox SET status = 'SKIPPED', last_error = $2, locked_until = NULL
		WHERE aggregate_id = $1 AND status IN ('PENDING', 'DEAD')`, orderID, reason)
	return err
}
