// Package state is the state-store: positions, reservations, rate windows,
// order history and kill switches. It has exactly one mutation path, Apply,
// which consumes audit events. The engine appends an event to the audit log
// and then applies it here; on restart the same events are replayed, so live
// state and recovered state cannot diverge.
//
// Store is not safe for concurrent use. The risk core serialises access.
package state

import (
	"fmt"
	"math/big"
	"sort"
	"time"

	"riskengine/src/domain"
	"riskengine/src/fixed"
)

type Order struct {
	Request     domain.RiskRequest
	Fingerprint string
	// Decision is the verdict as issued, with its audit seal but without
	// the reason trace: the trace is several kilobytes per order and lives
	// in the audit record that Seal points to.
	Decision    domain.RiskDecision
	Reservation *domain.RiskReservation // live reservation; nil if the order was rejected
	Fills       map[string]struct{}     // fill ids already applied; nil until the first fill
}

type Position struct {
	Qty       int64         `json:"qty"`        // signed
	CostBasis fixed.Decimal `json:"cost_basis"` // signed cost of the open quantity
	LastMark  fixed.Decimal `json:"last_mark"`  // fallback valuation when quotes are stale
}

type Account struct {
	Positions   map[string]*Position
	Open        map[string]*domain.RiskReservation // every reservation that is not RELEASED
	Ticks       []time.Time                        // evaluation times, ascending, for rate limiting
	PnLDay      string                             // UTC date RealizedPnL belongs to
	RealizedPnL fixed.Decimal
}

// DefaultRetention is how long a finished order is remembered for duplicate
// detection and late fills.
const DefaultRetention = 24 * time.Hour

type aged struct {
	id string
	at time.Time
}

type Store struct {
	// Retention bounds memory: an order with no live reservation is
	// forgotten once it is this old, after which its order_id would be
	// accepted as new and a fill for it would be refused as unknown. It
	// must be the same value on every replay of a given log.
	Retention time.Duration
	queue     []aged // orders in decision order, for expiry

	Orders   map[string]*Order
	Accounts map[string]*Account
	Kill     map[string]domain.KillSwitch // enabled switches only, by KillSwitch.Key()
	// UnreservedFills counts fills that arrived for orders holding no live
	// reservation (rejected, expired or over-filled): a sign the OMS and the
	// engine disagree.
	UnreservedFills int
}

func New() *Store {
	return &Store{Retention: DefaultRetention, Orders: map[string]*Order{}, Accounts: map[string]*Account{}, Kill: map[string]domain.KillSwitch{}}
}

var empty = &Account{}

// Account returns the account's state, or an empty read-only account if it
// has none yet. Reading nil maps is safe in Go.
func (s *Store) Account(id string) *Account {
	if a, ok := s.Accounts[id]; ok {
		return a
	}
	return empty
}

func (s *Store) account(id string) *Account {
	a, ok := s.Accounts[id]
	if !ok {
		a = &Account{Positions: map[string]*Position{}, Open: map[string]*domain.RiskReservation{}}
		s.Accounts[id] = a
	}
	return a
}

// ActiveKill returns the enabled switches that cover this account and
// symbol, widest scope first.
func (s *Store) ActiveKill(account, symbol string) []domain.KillSwitch {
	var out []domain.KillSwitch
	for _, key := range []string{
		string(domain.ScopeGlobal) + ":",
		string(domain.ScopeAccount) + ":" + account,
		string(domain.ScopeSymbol) + ":" + symbol,
	} {
		if k, ok := s.Kill[key]; ok {
			out = append(out, k)
		}
	}
	return out
}

func (s *Store) OpenReservations() int {
	n := 0
	for _, a := range s.Accounts {
		n += len(a.Open)
	}
	return n
}

// Day is the UTC trading-day key used for the daily loss limit.
func Day(t time.Time) string { return t.UTC().Format("2006-01-02") }

// PnLToday is realized P&L for the UTC day containing now.
func (a *Account) PnLToday(now time.Time) fixed.Decimal {
	if a.PnLDay != Day(now) {
		return 0
	}
	return a.RealizedPnL
}

// OrdersSince counts evaluations strictly after t.
func (a *Account) OrdersSince(t time.Time) int {
	i := sort.Search(len(a.Ticks), func(i int) bool { return a.Ticks[i].After(t) })
	return len(a.Ticks) - i
}

// Exposure is the worst-case absolute notional in one symbol: the position
// valued at mark, plus either every open buy filling or every open sell
// filling, whichever leaves the larger position. extraBuy/extraSell add a
// hypothetical order.
func (a *Account) Exposure(symbol string, mark, extraBuy, extraSell fixed.Decimal) fixed.Decimal {
	var pos fixed.Decimal
	if p := a.Positions[symbol]; p != nil {
		pos = fixed.MulQty(mark, p.Qty)
	}
	buys, sells := extraBuy, extraSell
	for _, r := range a.Open {
		if r.Symbol != symbol {
			continue
		}
		if r.Side == domain.Buy {
			buys = fixed.Add(buys, r.Notional)
		} else {
			sells = fixed.Add(sells, r.Notional)
		}
	}
	return max(fixed.Abs(fixed.Add(pos, buys)), fixed.Abs(fixed.Sub(pos, sells)))
}

// Symbols lists every symbol with a non-zero position or an open
// reservation, sorted.
func (a *Account) Symbols() []string {
	seen := map[string]struct{}{}
	for s, p := range a.Positions {
		if p.Qty != 0 {
			seen[s] = struct{}{}
		}
	}
	for _, r := range a.Open {
		seen[r.Symbol] = struct{}{}
	}
	out := make([]string, 0, len(seen))
	for s := range seen {
		out = append(out, s)
	}
	sort.Strings(out)
	return out
}

// tickRetention bounds the rate-limit history kept per account.
const tickRetention = time.Hour

// Apply mutates the store according to one audit event. It must stay a pure
// function of (state, event): no clocks, no randomness, no external reads.
func (s *Store) Apply(ev domain.Event, seal domain.Seal) error {
	switch ev.Type {
	case domain.EvDecision:
		d := ev.Decision
		if d == nil {
			return fmt.Errorf("state: %s event without body", ev.Type)
		}
		if !d.Register {
			return nil
		}
		req := d.Snapshot.Request
		dec := d.Decision
		dec.Seal, dec.Trace = seal, nil
		ord := &Order{Request: req, Fingerprint: d.Fingerprint, Decision: dec}
		s.Orders[req.OrderID] = ord
		s.queue = append(s.queue, aged{req.OrderID, ev.At})
		s.forget(ev.At)
		a := s.account(req.AccountID)
		a.Ticks = append(a.Ticks, ev.At)
		if cut := a.OrdersSince(ev.At.Add(-tickRetention)); cut < len(a.Ticks) {
			a.Ticks = append(a.Ticks[:0], a.Ticks[len(a.Ticks)-cut:]...)
		}
		if dec.Reservation != nil {
			r := *dec.Reservation
			ord.Reservation = &r
			a.Open[r.OrderID] = &r
		}
		if p := a.Positions[req.Symbol]; p != nil && d.Mark > 0 {
			p.LastMark = d.Mark
		}
	case domain.EvApprove:
		r, err := s.live(ev.Approve.OrderID)
		if err != nil {
			return err
		}
		r.Status, r.ExpiresAt = domain.Reserved, ev.Approve.ExpiresAt
	case domain.EvCommit:
		r, err := s.live(ev.Commit.OrderID)
		if err != nil {
			return err
		}
		r.Status = domain.Committed
	case domain.EvRelease:
		r, err := s.live(ev.Release.OrderID)
		if err != nil {
			return err
		}
		s.release(r, ev.Release.Reason)
	case domain.EvFill:
		return s.applyFill(ev.Fill, ev.At)
	case domain.EvKill:
		if ev.Kill.Enabled {
			s.Kill[ev.Kill.Key()] = *ev.Kill
		} else {
			delete(s.Kill, ev.Kill.Key())
		}
	case domain.EvPolicy, domain.EvReconcile, domain.EvMalformed:
		// Evidence only; no state.
	default:
		return fmt.Errorf("state: unknown event type %q", ev.Type)
	}
	return nil
}

// forget drops orders older than Retention. It is driven by event time
// only, so replaying the log forgets exactly the same orders. An order that
// still holds a reservation is kept and re-examined a retention period later.
func (s *Store) forget(now time.Time) {
	n := 0
	for n < len(s.queue) && now.Sub(s.queue[n].at) > s.Retention {
		n++
	}
	if n == 0 {
		return
	}
	old := s.queue[:n:n]
	s.queue = s.queue[n:]
	for _, o := range old {
		if ord := s.Orders[o.id]; ord.Reservation != nil && ord.Reservation.Status != domain.Released {
			s.queue = append(s.queue, aged{o.id, now})
		} else {
			delete(s.Orders, o.id)
		}
	}
}

func (s *Store) live(orderID string) (*domain.RiskReservation, error) {
	ord, ok := s.Orders[orderID]
	if !ok || ord.Reservation == nil || ord.Reservation.Status == domain.Released {
		return nil, fmt.Errorf("state: order %q has no live reservation", orderID)
	}
	return ord.Reservation, nil
}

func (s *Store) release(r *domain.RiskReservation, reason string) {
	r.Status, r.ReleaseReason = domain.Released, reason
	delete(s.account(r.AccountID).Open, r.OrderID)
}

func (s *Store) applyFill(f *domain.FillEvent, at time.Time) error {
	ord, ok := s.Orders[f.OrderID]
	if !ok {
		return fmt.Errorf("state: fill for unknown order %q", f.OrderID)
	}
	if _, dup := ord.Fills[f.FillID]; dup {
		return nil
	}
	if ord.Fills == nil {
		ord.Fills = map[string]struct{}{}
	}
	ord.Fills[f.FillID] = struct{}{}
	a := s.account(ord.Request.AccountID)

	// A fill is a fact: the position moves whether or not capacity was held.
	p := a.Positions[ord.Request.Symbol]
	if p == nil {
		p = &Position{}
		a.Positions[ord.Request.Symbol] = p
	}
	delta := f.Qty
	if ord.Request.Side == domain.Sell {
		delta = -delta
	}
	realized := p.fill(delta, f.Price)
	if day := Day(at); a.PnLDay != day {
		a.PnLDay, a.RealizedPnL = day, 0
	}
	a.RealizedPnL = fixed.Add(a.RealizedPnL, realized)

	r := ord.Reservation
	if r == nil || r.Status == domain.Released || r.Status == domain.PendingReview || f.Qty > r.Remaining {
		s.UnreservedFills++
	}
	if r == nil || r.Status == domain.Released {
		return nil
	}
	r.Remaining -= min(f.Qty, r.Remaining)
	r.Notional = fixed.MulQty(r.Price, r.Remaining)
	r.Status = domain.Committed // a fill proves the order is live
	if r.Remaining == 0 {
		s.release(r, "FILLED")
	}
	return nil
}

// fill applies a signed quantity at price using average cost and returns the
// realized P&L of whatever part of the position it closed.
func (p *Position) fill(delta int64, price fixed.Decimal) fixed.Decimal {
	p.LastMark = price
	if p.Qty == 0 || (p.Qty > 0) == (delta > 0) {
		p.Qty += delta
		p.CostBasis = fixed.Add(p.CostBasis, fixed.MulQty(price, delta))
		return 0
	}
	sign := int64(1)
	if p.Qty < 0 {
		sign = -1
	}
	closing := min(abs(delta), abs(p.Qty))
	// Cost of the closed slice, pro rata. Exact when the position closes
	// fully; otherwise truncated to 1e-4, which the remaining basis absorbs.
	closedCost := p.CostBasis
	if closing != abs(p.Qty) {
		c := new(big.Int).Mul(big.NewInt(int64(p.CostBasis)), big.NewInt(closing))
		closedCost = fixed.Decimal(c.Quo(c, big.NewInt(abs(p.Qty))).Int64())
	}
	realized := fixed.Sub(fixed.MulQty(price, sign*closing), closedCost)
	p.CostBasis = fixed.Sub(p.CostBasis, closedCost)
	p.Qty -= sign * closing
	if rest := abs(delta) - closing; rest > 0 { // flipped through zero
		p.Qty = -sign * rest
		p.CostBasis = fixed.MulQty(price, p.Qty)
	}
	return realized
}

func abs(v int64) int64 {
	if v < 0 {
		return -v
	}
	return v
}
