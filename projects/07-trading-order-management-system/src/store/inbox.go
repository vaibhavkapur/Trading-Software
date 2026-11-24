package store

import (
	"context"
	"encoding/json"
	"time"

	"github.com/jackc/pgx/v5"

	"oms/src/domain"
)

// BrokerEventRow is one inbox entry: a report the broker sent, with what the
// OMS did about it.
type BrokerEventRow struct {
	ID         int64           `json:"id"`
	EventID    string          `json:"event_id"`
	Kind       string          `json:"kind"`
	OrderID    string          `json:"order_id,omitempty"`
	Source     string          `json:"source"`
	Payload    json.RawMessage `json:"payload"`
	Outcome    string          `json:"outcome"`
	Detail     string          `json:"detail,omitempty"`
	Deliveries int             `json:"deliveries"`
	// EventTime is the broker's clock; ReceivedAt is the first local receipt.
	EventTime  *time.Time `json:"event_time,omitempty"`
	ReceivedAt time.Time  `json:"received_at"`
}

// RecordBrokerEvent files a broker report in the inbox. first is false when
// the same event id has been seen before; the redelivery is counted and the
// earlier outcome returned, and the caller must not process it again.
//
// A concurrent delivery of the same event id blocks here until the first
// transaction finishes, so each event id is processed exactly once.
func (t *Tx) RecordBrokerEvent(ctx context.Context, r domain.BrokerReport, source domain.Source, payload []byte) (id int64, first bool, outcome domain.Outcome, err error) {
	var eventTime *time.Time
	if !r.EventTime.IsZero() {
		et := r.EventTime.UTC()
		eventTime = &et
	}
	err = t.tx.QueryRow(ctx, `INSERT INTO broker_events
		(event_id, kind, order_id, source, payload, event_time, received_at, last_received_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $7)
		ON CONFLICT (event_id) DO UPDATE
			SET deliveries = broker_events.deliveries + 1, last_received_at = EXCLUDED.last_received_at
		RETURNING id, (xmax = 0), outcome`,
		r.EventID, r.Kind, r.OrderID, source, payload, eventTime, t.Now).Scan(&id, &first, &outcome)
	return id, first, outcome, err
}

// FinishBrokerEvent records what was done with an inbox entry.
func (t *Tx) FinishBrokerEvent(ctx context.Context, id int64, outcome domain.Outcome, detail string) error {
	_, err := t.tx.Exec(ctx, `UPDATE broker_events SET outcome = $2, detail = $3 WHERE id = $1`, id, outcome, detail)
	return err
}

// ListBrokerEvents returns the inbox entries for an order, oldest first.
func (s *Store) ListBrokerEvents(ctx context.Context, orderID string) ([]BrokerEventRow, error) {
	rows, err := s.pool.Query(ctx, `SELECT id, event_id, kind, order_id, source, payload, outcome, detail,
		deliveries, event_time, received_at FROM broker_events WHERE order_id = $1 ORDER BY id`, orderID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []BrokerEventRow{}
	for rows.Next() {
		var r BrokerEventRow
		var payload []byte
		if err := rows.Scan(&r.ID, &r.EventID, &r.Kind, &r.OrderID, &r.Source, &payload, &r.Outcome, &r.Detail,
			&r.Deliveries, &r.EventTime, &r.ReceivedAt); err != nil {
			return nil, err
		}
		r.Payload, r.EventTime, r.ReceivedAt = payload, utcPtr(r.EventTime), utc(r.ReceivedAt)
		out = append(out, r)
	}
	return out, rows.Err()
}

// IdempotencyRecord is the stored result of a keyed command.
type IdempotencyRecord struct {
	Operation   string
	RequestHash string
	OrderID     string
	// Status is the HTTP status of the first response, or 0 if the first
	// request has not committed (only visible inside its own transaction).
	Status int
	Body   []byte
}

// LookupIdempotency reads a completed idempotency record.
func (s *Store) LookupIdempotency(ctx context.Context, principal, key string) (IdempotencyRecord, error) {
	return scanIdempotency(s.pool.QueryRow(ctx, `SELECT operation, request_hash, COALESCE(order_id, ''),
		response_status, response_body FROM idempotency_keys WHERE principal = $1 AND key = $2`, principal, key))
}

func scanIdempotency(row pgx.Row) (IdempotencyRecord, error) {
	var r IdempotencyRecord
	if err := row.Scan(&r.Operation, &r.RequestHash, &r.OrderID, &r.Status, &r.Body); err != nil {
		return IdempotencyRecord{}, notFound(err)
	}
	return r, nil
}

// ClaimIdempotency reserves a key for the current transaction. claimed is
// false when the key already belongs to an earlier request, whose record is
// returned. A concurrent request with the same key waits here until the
// first one commits or rolls back, so exactly one of them does the work.
func (t *Tx) ClaimIdempotency(ctx context.Context, principal, key, operation, requestHash string) (claimed bool, existing IdempotencyRecord, err error) {
	tag, err := t.tx.Exec(ctx, `INSERT INTO idempotency_keys (principal, key, operation, request_hash, created_at)
		VALUES ($1, $2, $3, $4, $5) ON CONFLICT (principal, key) DO NOTHING`, principal, key, operation, requestHash, t.Now)
	if err != nil {
		return false, existing, err
	}
	if tag.RowsAffected() == 1 {
		return true, existing, nil
	}
	existing, err = scanIdempotency(t.tx.QueryRow(ctx, `SELECT operation, request_hash, COALESCE(order_id, ''),
		response_status, response_body FROM idempotency_keys WHERE principal = $1 AND key = $2`, principal, key))
	return false, existing, err
}

// CompleteIdempotency stores the response for a claimed key.
func (t *Tx) CompleteIdempotency(ctx context.Context, principal, key, orderID string, status int, body []byte) error {
	_, err := t.tx.Exec(ctx, `UPDATE idempotency_keys SET order_id = $3, response_status = $4, response_body = $5
		WHERE principal = $1 AND key = $2`, principal, key, orderID, status, body)
	return err
}
