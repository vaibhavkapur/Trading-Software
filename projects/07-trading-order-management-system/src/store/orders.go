package store

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"oms/src/domain"
	"oms/src/fixed"
)

const orderCols = `id, account_id, client_order_id, symbol, side, type, time_in_force, qty,
	limit_price_micros, state, version, filled_qty, filled_notional_micros, fees_micros,
	overfill_accepted, broker_order_id, pending_cancel, pending_replace, reject_reason,
	created_by, created_at, updated_at, submitted_at, acknowledged_at, closed_at`

func scanOrder(row pgx.Row) (domain.Order, error) {
	var o domain.Order
	var pendingReplace []byte
	err := row.Scan(&o.ID, &o.AccountID, &o.ClientOrderID, &o.Symbol, &o.Side, &o.Type, &o.TimeInForce, &o.Qty,
		&o.LimitPrice, &o.State, &o.Version, &o.FilledQty, &o.FilledNotional, &o.Fees,
		&o.OverfillAccepted, &o.BrokerOrderID, &o.PendingCancel, &pendingReplace, &o.RejectReason,
		&o.CreatedBy, &o.CreatedAt, &o.UpdatedAt, &o.SubmittedAt, &o.AcknowledgedAt, &o.ClosedAt)
	if err != nil {
		return domain.Order{}, notFound(err)
	}
	if pendingReplace != nil {
		o.PendingReplace = new(domain.Replace)
		if err := json.Unmarshal(pendingReplace, o.PendingReplace); err != nil {
			return domain.Order{}, fmt.Errorf("store: order %s pending_replace: %w", o.ID, err)
		}
	}
	o.CreatedAt, o.UpdatedAt = utc(o.CreatedAt), utc(o.UpdatedAt)
	o.SubmittedAt, o.AcknowledgedAt, o.ClosedAt = utcPtr(o.SubmittedAt), utcPtr(o.AcknowledgedAt), utcPtr(o.ClosedAt)
	return o, nil
}

// saveOrder writes the order projection: an insert for a new order
// (prevVersion 0), otherwise an update that only succeeds if the row is still
// at the version the caller read.
func (t *Tx) saveOrder(ctx context.Context, o domain.Order, prevVersion int64) error {
	var pendingReplace []byte
	if o.PendingReplace != nil {
		var err error
		if pendingReplace, err = json.Marshal(o.PendingReplace); err != nil {
			return err
		}
	}
	if prevVersion == 0 {
		_, err := t.tx.Exec(ctx, `INSERT INTO orders (`+orderCols+`) VALUES
			($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19,$20,$21,$22,$23,$24,$25)`,
			o.ID, o.AccountID, o.ClientOrderID, o.Symbol, o.Side, o.Type, o.TimeInForce, o.Qty,
			o.LimitPrice, o.State, o.Version, o.FilledQty, o.FilledNotional, o.Fees,
			o.OverfillAccepted, o.BrokerOrderID, o.PendingCancel, pendingReplace, o.RejectReason,
			o.CreatedBy, o.CreatedAt, o.UpdatedAt, o.SubmittedAt, o.AcknowledgedAt, o.ClosedAt)
		return err
	}
	tag, err := t.tx.Exec(ctx, `UPDATE orders SET
			qty = $3, limit_price_micros = $4, state = $5, version = $6, filled_qty = $7,
			filled_notional_micros = $8, fees_micros = $9, overfill_accepted = $10, broker_order_id = $11,
			pending_cancel = $12, pending_replace = $13, reject_reason = $14, updated_at = $15,
			submitted_at = $16, acknowledged_at = $17, closed_at = $18
		WHERE id = $1 AND version = $2`,
		o.ID, prevVersion,
		o.Qty, o.LimitPrice, o.State, o.Version, o.FilledQty,
		o.FilledNotional, o.Fees, o.OverfillAccepted, o.BrokerOrderID,
		o.PendingCancel, pendingReplace, o.RejectReason, o.UpdatedAt,
		o.SubmittedAt, o.AcknowledgedAt, o.ClosedAt)
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		return fmt.Errorf("%w: order %s is no longer at version %d", ErrVersionConflict, o.ID, prevVersion)
	}
	return nil
}

// GetOrder reads an order's current projection.
func (s *Store) GetOrder(ctx context.Context, id string) (domain.Order, error) {
	return scanOrder(s.pool.QueryRow(ctx, `SELECT `+orderCols+` FROM orders WHERE id = $1`, id))
}

// GetOrderByClientID finds an order by its account-scoped client order id.
func (s *Store) GetOrderByClientID(ctx context.Context, accountID, clientOrderID string) (domain.Order, error) {
	return scanOrder(s.pool.QueryRow(ctx,
		`SELECT `+orderCols+` FROM orders WHERE account_id = $1 AND client_order_id = $2`, accountID, clientOrderID))
}

// OrderFilter narrows ListOrders.
type OrderFilter struct {
	// Accounts restricts the result to these accounts. Nil means all accounts.
	Accounts []string
	State    domain.State
	Limit    int
}

// ListOrders returns orders newest first.
func (s *Store) ListOrders(ctx context.Context, f OrderFilter) ([]domain.Order, error) {
	if f.Limit <= 0 || f.Limit > 500 {
		f.Limit = 100
	}
	rows, err := s.pool.Query(ctx, `SELECT `+orderCols+` FROM orders
		WHERE ($1::text[] IS NULL OR account_id = ANY($1))
		  AND ($2 = '' OR state = $2)
		ORDER BY created_at DESC, id LIMIT $3`, f.Accounts, string(f.State), f.Limit)
	if err != nil {
		return nil, err
	}
	return collectOrders(rows)
}

func collectOrders(rows pgx.Rows) ([]domain.Order, error) {
	defer rows.Close()
	out := []domain.Order{}
	for rows.Next() {
		o, err := scanOrder(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, o)
	}
	return out, rows.Err()
}

// LockOrder reads an order and locks its row until the transaction ends.
// Every writer takes this lock first, so order changes are serialized per
// order and never interleave.
func (t *Tx) LockOrder(ctx context.Context, id string) (domain.Order, error) {
	return scanOrder(t.tx.QueryRow(ctx, `SELECT `+orderCols+` FROM orders WHERE id = $1 FOR UPDATE`, id))
}

const eventCols = `seq, order_id, version, type, source, actor, causation_id, state_after, payload, occurred_at, recorded_at`

func scanEvent(row pgx.Row) (domain.Event, error) {
	var e domain.Event
	var payload []byte
	if err := row.Scan(&e.Seq, &e.OrderID, &e.Version, &e.Type, &e.Source, &e.Actor, &e.CausationID,
		&e.StateAfter, &payload, &e.OccurredAt, &e.RecordedAt); err != nil {
		return domain.Event{}, notFound(err)
	}
	e.Payload = payload
	e.OccurredAt, e.RecordedAt = utc(e.OccurredAt), utc(e.RecordedAt)
	return e, nil
}

// ListEvents returns an order's history, oldest first.
func (s *Store) ListEvents(ctx context.Context, orderID string) ([]domain.Event, error) {
	return listEvents(ctx, s.pool, `WHERE order_id = $1 ORDER BY version`, orderID)
}

// AllEvents returns every event in append order. It is used to verify and
// rebuild projections.
func (s *Store) AllEvents(ctx context.Context) ([]domain.Event, error) {
	return listEvents(ctx, s.pool, `ORDER BY seq`)
}

func listEvents(ctx context.Context, q querier, where string, args ...any) ([]domain.Event, error) {
	rows, err := q.Query(ctx, `SELECT `+eventCols+` FROM order_events `+where, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []domain.Event{}
	for rows.Next() {
		e, err := scanEvent(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// Append applies drafts to an order, in order, and persists the result: the
// new events, the order projection, and the execution and position
// projections that follow from execution events. cur is the order as read
// with LockOrder, or the zero Order with orderID set to create a new one.
//
// Every draft passes through domain.Apply first, so an event the state
// machine forbids aborts the transaction before anything is written.
func (t *Tx) Append(ctx context.Context, orderID string, cur domain.Order, drafts []domain.Draft) (domain.Order, []domain.Event, error) {
	if len(drafts) == 0 {
		return cur, nil, nil
	}
	prev := cur
	events := make([]domain.Event, 0, len(drafts))
	befores := make([]domain.Order, 0, len(drafts))
	for _, d := range drafts {
		payload, err := json.Marshal(d.Payload)
		if err != nil {
			return domain.Order{}, nil, fmt.Errorf("store: marshal %s payload: %w", d.Type, err)
		}
		e := domain.Event{
			OrderID: orderID, Version: cur.Version + 1, Type: d.Type, Source: d.Source,
			Actor: d.Actor, CausationID: d.CausationID, Payload: payload,
			OccurredAt: d.OccurredAt.UTC(), RecordedAt: t.Now,
		}
		if d.OccurredAt.IsZero() {
			e.OccurredAt = t.Now
		}
		next, err := domain.Apply(cur, e)
		if err != nil {
			return domain.Order{}, nil, err
		}
		e.StateAfter = next.State
		befores = append(befores, cur)
		events = append(events, e)
		cur = next
	}

	if err := t.saveOrder(ctx, cur, prev.Version); err != nil {
		return domain.Order{}, nil, err
	}

	for i := range events {
		e := &events[i]
		if err := t.tx.QueryRow(ctx, `INSERT INTO order_events
			(order_id, version, type, source, actor, causation_id, state_after, payload, occurred_at, recorded_at)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10) RETURNING seq`,
			e.OrderID, e.Version, e.Type, e.Source, e.Actor, e.CausationID, e.StateAfter,
			[]byte(e.Payload), e.OccurredAt, e.RecordedAt).Scan(&e.Seq); err != nil {
			return domain.Order{}, nil, err
		}
		if err := t.project(ctx, befores[i], *e); err != nil {
			return domain.Order{}, nil, err
		}
	}
	return cur, events, nil
}

// ExecutionRow is one booked broker execution record.
type ExecutionRow struct {
	ID               int64           `json:"id"`
	OrderID          string          `json:"order_id"`
	EventSeq         int64           `json:"event_seq"`
	BrokerExecID     string          `json:"broker_exec_id"`
	Kind             domain.ExecKind `json:"kind"`
	RefBrokerExecID  string          `json:"ref_broker_exec_id,omitempty"`
	Qty              int64           `json:"qty"`
	Price            fixed.Micros    `json:"price"`
	Fee              fixed.Micros    `json:"fee"`
	DeltaQty         int64           `json:"delta_qty"`
	DeltaNotional    fixed.Micros    `json:"delta_notional"`
	DeltaFee         fixed.Micros    `json:"delta_fee"`
	AcceptedOverfill bool            `json:"accepted_overfill,omitempty"`
	Source           domain.Source   `json:"source"`
	// ExecutedAt is the broker's execution time; RecordedAt is local receipt.
	ExecutedAt time.Time `json:"executed_at"`
	RecordedAt time.Time `json:"recorded_at"`
}

// PositionRow is the net result of every effective execution for one
// account and symbol.
type PositionRow struct {
	AccountID string       `json:"account_id"`
	Symbol    string       `json:"symbol"`
	NetQty    int64        `json:"net_qty"`
	NetCash   fixed.Micros `json:"net_cash"`
	Fees      fixed.Micros `json:"fees"`
	UpdatedAt time.Time    `json:"updated_at"`
}

// Projection is what one event contributes to the execution and position
// projections. It is derived from the event alone (plus the order's account,
// symbol and side), so replaying events reproduces the tables exactly.
type Projection struct {
	Execution *ExecutionRow
	Position  *PositionRow // a delta, not a total
}

// Project derives the projection rows for an event. o is the order the event
// belongs to (only its identity fields are read).
func Project(o domain.Order, e domain.Event) (Projection, error) {
	if !e.Type.IsExecution() {
		return Projection{}, nil
	}
	var p domain.Execution
	if err := json.Unmarshal(e.Payload, &p); err != nil {
		return Projection{}, fmt.Errorf("store: event %d payload: %w", e.Seq, err)
	}
	sign := o.Side.Sign()
	return Projection{
		Execution: &ExecutionRow{
			OrderID: e.OrderID, EventSeq: e.Seq, BrokerExecID: p.BrokerExecID, Kind: p.Kind,
			RefBrokerExecID: p.RefBrokerExecID, Qty: p.Qty, Price: p.Price, Fee: p.Fee,
			DeltaQty: p.DeltaQty, DeltaNotional: p.DeltaNotional, DeltaFee: p.DeltaFee,
			AcceptedOverfill: e.Type == domain.EvExecutionAccepted, Source: e.Source,
			ExecutedAt: p.ExecutedAt.UTC(), RecordedAt: e.RecordedAt,
		},
		Position: &PositionRow{
			AccountID: o.AccountID, Symbol: o.Symbol,
			NetQty: sign * p.DeltaQty,
			// A buy pays cash out, a sell brings it in.
			NetCash:   fixed.Micros(-sign) * p.DeltaNotional,
			Fees:      p.DeltaFee,
			UpdatedAt: e.RecordedAt,
		},
	}, nil
}

func (t *Tx) project(ctx context.Context, o domain.Order, e domain.Event) error {
	pr, err := Project(o, e)
	if err != nil || pr.Execution == nil {
		return err
	}
	x := pr.Execution
	var ref *string
	if x.RefBrokerExecID != "" {
		ref = &x.RefBrokerExecID
	}
	if _, err := t.tx.Exec(ctx, `INSERT INTO executions
		(order_id, event_seq, broker_exec_id, kind, ref_broker_exec_id, qty, price_micros, fee_micros,
		 delta_qty, delta_notional_micros, delta_fee_micros, accepted_overfill, source, executed_at, recorded_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15)`,
		x.OrderID, x.EventSeq, x.BrokerExecID, x.Kind, ref, x.Qty, x.Price, x.Fee,
		x.DeltaQty, x.DeltaNotional, x.DeltaFee, x.AcceptedOverfill, x.Source, x.ExecutedAt, x.RecordedAt); err != nil {
		return err
	}
	// Every column is combined with a commutative operation (sums, and the
	// latest timestamp), so the row does not depend on the order in which
	// concurrent transactions for different orders reach it, and a replay of
	// the event log in sequence order reproduces it exactly.
	p := pr.Position
	_, err = t.tx.Exec(ctx, `INSERT INTO positions (account_id, symbol, net_qty, net_cash_micros, fees_micros, updated_at)
		VALUES ($1,$2,$3,$4,$5,$6)
		ON CONFLICT (account_id, symbol) DO UPDATE SET
			net_qty = positions.net_qty + EXCLUDED.net_qty,
			net_cash_micros = positions.net_cash_micros + EXCLUDED.net_cash_micros,
			fees_micros = positions.fees_micros + EXCLUDED.fees_micros,
			updated_at = GREATEST(positions.updated_at, EXCLUDED.updated_at)`,
		p.AccountID, p.Symbol, p.NetQty, p.NetCash, p.Fees, p.UpdatedAt)
	return err
}

const execCols = `id, order_id, event_seq, broker_exec_id, kind, COALESCE(ref_broker_exec_id, ''), qty, price_micros,
	fee_micros, delta_qty, delta_notional_micros, delta_fee_micros, accepted_overfill, source, executed_at, recorded_at`

func scanExec(row pgx.Row) (ExecutionRow, error) {
	var x ExecutionRow
	if err := row.Scan(&x.ID, &x.OrderID, &x.EventSeq, &x.BrokerExecID, &x.Kind, &x.RefBrokerExecID, &x.Qty, &x.Price,
		&x.Fee, &x.DeltaQty, &x.DeltaNotional, &x.DeltaFee, &x.AcceptedOverfill, &x.Source, &x.ExecutedAt, &x.RecordedAt); err != nil {
		return ExecutionRow{}, notFound(err)
	}
	x.ExecutedAt, x.RecordedAt = utc(x.ExecutedAt), utc(x.RecordedAt)
	return x, nil
}

func listExecs(ctx context.Context, q querier, where string, args ...any) ([]ExecutionRow, error) {
	rows, err := q.Query(ctx, `SELECT `+execCols+` FROM executions `+where, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []ExecutionRow{}
	for rows.Next() {
		x, err := scanExec(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, x)
	}
	return out, rows.Err()
}

// ListExecutions returns an order's execution records in booking order.
func (s *Store) ListExecutions(ctx context.Context, orderID string) ([]ExecutionRow, error) {
	return listExecs(ctx, s.pool, `WHERE order_id = $1 ORDER BY id`, orderID)
}

// GetExecution finds an execution record by the broker's execution id.
func (s *Store) GetExecution(ctx context.Context, brokerExecID string) (ExecutionRow, error) {
	return scanExec(s.pool.QueryRow(ctx, `SELECT `+execCols+` FROM executions WHERE broker_exec_id = $1`, brokerExecID))
}

// ExecutionsForOrders returns the execution records of the given orders.
func (s *Store) ExecutionsForOrders(ctx context.Context, orderIDs []string) ([]ExecutionRow, error) {
	return listExecs(ctx, s.pool, `WHERE order_id = ANY($1) ORDER BY id`, orderIDs)
}

// AllExecutions returns every execution record in booking order.
func (s *Store) AllExecutions(ctx context.Context) ([]ExecutionRow, error) {
	return listExecs(ctx, s.pool, `ORDER BY id`)
}

// ExecLookup is what the executions on file say about an incoming record.
type ExecLookup struct {
	// Existing is the record already booked under the same execution id.
	Existing *ExecutionRow
	// Context is handed to the aggregate.
	Context domain.ExecContext
	// RefOrderID is the order the referenced execution belongs to, if on file.
	RefOrderID string
}

// LookupExecution gathers the facts needed to decide an execution report.
// Call it with the order row locked.
func (t *Tx) LookupExecution(ctx context.Context, execID, refExecID string) (ExecLookup, error) {
	var out ExecLookup
	x, err := scanExec(t.tx.QueryRow(ctx, `SELECT `+execCols+` FROM executions WHERE broker_exec_id = $1`, execID))
	switch {
	case err == nil:
		out.Existing = &x
	case err != ErrNotFound:
		return out, err
	}
	if err := t.tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM executions WHERE ref_broker_exec_id = $1)`, execID).
		Scan(&out.Context.Superseded); err != nil {
		return out, err
	}
	if refExecID == "" {
		return out, nil
	}
	err = t.tx.QueryRow(ctx, `SELECT broker_exec_id FROM executions WHERE ref_broker_exec_id = $1`, refExecID).
		Scan(&out.Context.RefCorrectedBy)
	if err != nil && err != pgx.ErrNoRows {
		return out, err
	}
	ref, err := scanExec(t.tx.QueryRow(ctx, `SELECT `+execCols+` FROM executions WHERE broker_exec_id = $1`, refExecID))
	switch {
	case err == ErrNotFound:
		return out, nil
	case err != nil:
		return out, err
	}
	out.RefOrderID = ref.OrderID
	if out.Context.RefCorrectedBy == "" && ref.Kind != domain.ExecBust {
		out.Context.Ref = &domain.ExecValues{Qty: ref.Qty, Price: ref.Price, Fee: ref.Fee}
	}
	return out, nil
}

// ListPositions returns positions for the given accounts (all when nil).
func (s *Store) ListPositions(ctx context.Context, accounts []string) ([]PositionRow, error) {
	rows, err := s.pool.Query(ctx, `SELECT account_id, symbol, net_qty, net_cash_micros, fees_micros, updated_at
		FROM positions WHERE ($1::text[] IS NULL OR account_id = ANY($1)) ORDER BY account_id, symbol`, accounts)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []PositionRow{}
	for rows.Next() {
		var p PositionRow
		if err := rows.Scan(&p.AccountID, &p.Symbol, &p.NetQty, &p.NetCash, &p.Fees, &p.UpdatedAt); err != nil {
			return nil, err
		}
		p.UpdatedAt = utc(p.UpdatedAt)
		out = append(out, p)
	}
	return out, rows.Err()
}
