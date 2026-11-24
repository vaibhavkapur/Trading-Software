package store

import (
	"context"
	"fmt"
	"reflect"
	"sort"

	"github.com/jackc/pgx/v5"

	"oms/src/domain"
	"oms/src/fixed"
)

// VerifyReport is the result of replaying the event log against the
// projection tables.
type VerifyReport struct {
	Orders     int      `json:"orders"`
	Events     int      `json:"events"`
	Executions int      `json:"executions"`
	Positions  int      `json:"positions"`
	Problems   []string `json:"problems"`
}

// OK reports whether every projection matched the event log.
func (r VerifyReport) OK() bool { return len(r.Problems) == 0 }

// Verify replays every order event from the beginning and checks that the
// result is exactly what the orders, executions and positions tables hold.
// It also cross-checks each order's totals against its effective executions
// (fills that have not been corrected or busted), which is an independent
// derivation of the same numbers.
//
// It reads one consistent snapshot and holds no locks, so it is safe to run
// against a live system. It loads the whole log into memory.
func (s *Store) Verify(ctx context.Context) (VerifyReport, error) {
	var rep VerifyReport
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return rep, err
	}
	defer tx.Rollback(context.WithoutCancel(ctx))

	events, err := listEvents(ctx, tx, `ORDER BY seq`)
	if err != nil {
		return rep, err
	}
	orderRows, err := tx.Query(ctx, `SELECT `+orderCols+` FROM orders ORDER BY id`)
	if err != nil {
		return rep, err
	}
	orders, err := collectOrders(orderRows)
	if err != nil {
		return rep, err
	}
	execs, err := listExecs(ctx, tx, `ORDER BY event_seq`)
	if err != nil {
		return rep, err
	}
	posRows, err := tx.Query(ctx, `SELECT account_id, symbol, net_qty, net_cash_micros, fees_micros, updated_at FROM positions`)
	if err != nil {
		return rep, err
	}
	positions := map[[2]string]PositionRow{}
	for posRows.Next() {
		var p PositionRow
		if err := posRows.Scan(&p.AccountID, &p.Symbol, &p.NetQty, &p.NetCash, &p.Fees, &p.UpdatedAt); err != nil {
			posRows.Close()
			return rep, err
		}
		p.UpdatedAt = utc(p.UpdatedAt)
		positions[[2]string{p.AccountID, p.Symbol}] = p
	}
	posRows.Close()
	if err := posRows.Err(); err != nil {
		return rep, err
	}

	rep.Orders, rep.Events, rep.Executions, rep.Positions = len(orders), len(events), len(execs), len(positions)
	problem := func(format string, args ...any) { rep.Problems = append(rep.Problems, fmt.Sprintf(format, args...)) }

	// Replay.
	folded := map[string]domain.Order{}
	broken := map[string]bool{}
	var wantExecs []ExecutionRow
	wantPos := map[[2]string]PositionRow{}
	for _, e := range events {
		if broken[e.OrderID] {
			continue
		}
		before := folded[e.OrderID]
		after, err := domain.Apply(before, e)
		if err != nil {
			problem("order %s: event log does not replay: %v", e.OrderID, err)
			broken[e.OrderID] = true
			continue
		}
		if after.State != e.StateAfter {
			problem("order %s v%d: stored state_after %s but replay gives %s", e.OrderID, e.Version, e.StateAfter, after.State)
		}
		folded[e.OrderID] = after
		pr, err := Project(after, e)
		if err != nil {
			problem("order %s v%d: %v", e.OrderID, e.Version, err)
			continue
		}
		if pr.Execution != nil {
			wantExecs = append(wantExecs, *pr.Execution)
			key := [2]string{pr.Position.AccountID, pr.Position.Symbol}
			p := wantPos[key]
			p.AccountID, p.Symbol = key[0], key[1]
			p.NetQty += pr.Position.NetQty
			p.NetCash += pr.Position.NetCash
			p.Fees += pr.Position.Fees
			if pr.Position.UpdatedAt.After(p.UpdatedAt) {
				p.UpdatedAt = pr.Position.UpdatedAt
			}
			wantPos[key] = p
		}
	}

	// Orders.
	seen := map[string]bool{}
	for _, got := range orders {
		seen[got.ID] = true
		want, ok := folded[got.ID]
		switch {
		case broken[got.ID]:
		case !ok:
			problem("order %s: has a projection but no events", got.ID)
		case !reflect.DeepEqual(got, want):
			problem("order %s: projection differs from replay\n  table:  %+v\n  replay: %+v", got.ID, got, want)
		}
		if err := domain.CheckInvariants(got); err != nil {
			problem("order %s: invariant broken: %v", got.ID, err)
		}
	}
	for id := range folded {
		if !seen[id] {
			problem("order %s: has events but no projection", id)
		}
	}

	// Executions.
	if len(execs) != len(wantExecs) {
		problem("executions: table has %d rows but replay gives %d", len(execs), len(wantExecs))
	} else {
		for i := range execs {
			got := execs[i]
			got.ID = 0
			if !reflect.DeepEqual(got, wantExecs[i]) {
				problem("execution %s: projection differs from replay\n  table:  %+v\n  replay: %+v",
					got.BrokerExecID, got, wantExecs[i])
			}
		}
	}

	// Positions.
	keys := map[[2]string]bool{}
	for k := range positions {
		keys[k] = true
	}
	for k := range wantPos {
		keys[k] = true
	}
	for k := range keys {
		if got, want := positions[k], wantPos[k]; got != want {
			problem("position %s/%s: projection differs from replay\n  table:  %+v\n  replay: %+v", k[0], k[1], got, want)
		}
	}

	// Independent cross-check: order totals equal the sum of effective
	// executions, computed from the records' own terms rather than the deltas.
	referenced := map[string]bool{}
	for _, x := range execs {
		if x.RefBrokerExecID != "" {
			referenced[x.RefBrokerExecID] = true
		}
	}
	type totals struct {
		qty      int64
		notional fixed.Micros
		fee      fixed.Micros
	}
	sum := map[string]totals{}
	for _, x := range execs {
		if x.Kind == domain.ExecBust || referenced[x.BrokerExecID] {
			continue
		}
		t := sum[x.OrderID]
		n, _ := fixed.MulQty(x.Price, x.Qty)
		t.qty, t.notional, t.fee = t.qty+x.Qty, t.notional+n, t.fee+x.Fee
		sum[x.OrderID] = t
	}
	for _, o := range orders {
		t := sum[o.ID]
		if t.qty != o.FilledQty || t.notional != o.FilledNotional || t.fee != o.Fees {
			problem("order %s: totals (qty %d, notional %s, fees %s) differ from its effective executions (qty %d, notional %s, fees %s)",
				o.ID, o.FilledQty, o.FilledNotional, o.Fees, t.qty, t.notional, t.fee)
		}
	}

	sort.Strings(rep.Problems)
	return rep, nil
}
