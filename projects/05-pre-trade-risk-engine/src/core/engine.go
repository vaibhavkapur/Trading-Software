// Package core is risk-core: the deterministic evaluation of a proposed
// order against policy, market data and account state, and the reservation
// lifecycle that follows an ALLOW.
//
// Everything that reads or writes account state runs under one mutex, so
// "check the limit" and "reserve the capacity" are a single atomic step and
// two concurrent orders can never both spend the same headroom.
package core

import (
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"riskengine/src/domain"
	"riskengine/src/fixed"
	"riskengine/src/market"
	"riskengine/src/metrics"
	"riskengine/src/policy"
	"riskengine/src/state"
)

type Clock interface{ Now() time.Time }

type SystemClock struct{}

func (SystemClock) Now() time.Time { return time.Now().Round(0).UTC() }

// AuditLog is the evidence sink. An event that cannot be appended is never
// applied (kill-switch activation excepted; see SetKillSwitch).
type AuditLog interface {
	Append(domain.Event) (domain.Seal, error)
}

var (
	ErrNotFound      = errors.New("order not found")
	ErrNoReservation = errors.New("order has no reservation")
	ErrReleased      = errors.New("reservation already released")
	ErrExpired       = errors.New("reservation expired before commit; do not send the order")
	ErrPendingReview = errors.New("reservation is pending review and must be approved first")
	ErrNotPending    = errors.New("reservation is not pending review")
	ErrKillSwitch    = errors.New("kill switch active; reservation released, do not send the order")
	ErrInvalid       = errors.New("invalid argument")
	ErrAudit         = errors.New("audit log unavailable; no change was made")
)

const (
	MaxQty   = 1_000_000_000
	maxIDLen = 64
)

type Engine struct {
	mu     sync.Mutex
	st     *state.Store
	pol    atomic.Pointer[policy.Policy]
	mkt    *market.Store
	log    AuditLog
	clock  Clock
	met    *metrics.Registry
	onKill func(domain.KillSwitch, domain.Seal)
}

// New builds an engine over st, which the caller has already rebuilt by
// replaying the audit log. With no policy set, every order is rejected.
func New(st *state.Store, mkt *market.Store, log AuditLog, clock Clock, met *metrics.Registry) *Engine {
	return &Engine{st: st, mkt: mkt, log: log, clock: clock, met: met}
}

// OnKillSwitch registers the propagation hook, called outside the engine
// lock after every kill-switch change.
func (e *Engine) OnKillSwitch(fn func(domain.KillSwitch, domain.Seal)) { e.onKill = fn }

func (e *Engine) Policy() *policy.Policy { return e.pol.Load() }

// SetPolicy records the policy version in the audit log and then activates
// it. If the record cannot be written the previous policy stays in force.
func (e *Engine) SetPolicy(p *policy.Policy, source string) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	ev := domain.Event{Type: domain.EvPolicy, At: e.clock.Now(),
		Policy: &domain.PolicyEvent{Version: p.Version, Hash: p.Hash, Source: source}}
	if _, err := e.emit(ev); err != nil {
		return err
	}
	e.pol.Store(p)
	return nil
}

// emit appends an event and, only if that succeeds, applies it.
func (e *Engine) emit(ev domain.Event) (domain.Seal, error) {
	seal, err := e.log.Append(ev)
	if err != nil {
		e.met.Inc("risk_audit_append_errors_total")
		return domain.Seal{}, fmt.Errorf("%w: %v", ErrAudit, err)
	}
	if err := e.st.Apply(ev, seal); err != nil {
		return seal, err
	}
	e.met.Set("risk_reservations_open", float64(e.st.OpenReservations()))
	return seal, nil
}

// verdict accumulates the reason trace of one evaluation.
type verdict struct {
	trace   []domain.Check
	rejects []string
	reviews []string
}

func (v *verdict) ok() bool { return len(v.rejects) == 0 }

func (v *verdict) pass(c domain.Check) {
	c.Outcome = "PASS"
	v.trace = append(v.trace, c)
}

func (v *verdict) skip(name, why string) {
	v.trace = append(v.trace, domain.Check{Name: name, Outcome: "SKIPPED", Detail: why})
}

func (v *verdict) reject(code string, c domain.Check) {
	c.Outcome, c.Code = "REJECT", code
	v.trace = append(v.trace, c)
	v.rejects = append(v.rejects, code)
}

func (v *verdict) review(code string, c domain.Check) {
	c.Outcome, c.Code = "REVIEW", code
	v.trace = append(v.trace, c)
	v.reviews = append(v.reviews, code)
}

// limit records a threshold comparison: breach => reject with code.
func (v *verdict) limit(breach bool, code string, c domain.Check) {
	if breach {
		v.reject(code, c)
	} else {
		v.pass(c)
	}
}

func validID(s string, maxLen int) bool {
	if s == "" || len(s) > maxLen {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || strings.IndexByte("._:-", c) >= 0) {
			return false
		}
	}
	return true
}

func validate(v *verdict, r domain.RiskRequest) {
	bad := func(code, detail string) { v.reject(code, domain.Check{Name: "input_validation", Detail: detail}) }
	if !validID(r.OrderID, maxIDLen) {
		bad(domain.InvalidOrderID, "order_id must be 1-64 chars of [A-Za-z0-9._:-]")
	}
	if !validID(r.AccountID, maxIDLen) {
		bad(domain.InvalidAccountID, "account_id must be 1-64 chars of [A-Za-z0-9._:-]")
	}
	if !validID(r.Symbol, 32) {
		bad(domain.InvalidSymbol, "symbol must be 1-32 chars of [A-Za-z0-9._:-]")
	}
	if r.Side != domain.Buy && r.Side != domain.Sell {
		bad(domain.InvalidSide, "side must be BUY or SELL")
	}
	if r.Qty <= 0 || r.Qty > MaxQty {
		bad(domain.InvalidQty, fmt.Sprintf("qty must be in [1, %d]", MaxQty))
	}
	if r.LimitPrice == nil || *r.LimitPrice <= 0 || *r.LimitPrice > market.MaxPrice {
		bad(domain.InvalidPrice, fmt.Sprintf("limit_price is required and must be in (0, %s]", market.MaxPrice))
	}
	if v.ok() {
		v.pass(domain.Check{Name: "input_validation"})
	}
}

// Evaluate decides one proposed order. It always returns a decision; the
// only decisions not backed by an audit record are AUDIT_UNAVAILABLE rejects.
func (e *Engine) Evaluate(req domain.RiskRequest) domain.RiskDecision {
	start := time.Now()
	e.mu.Lock()
	d := e.evaluate(req, e.clock.Now())
	e.mu.Unlock()

	e.met.Observe("risk_decision_duration_seconds", time.Since(start).Seconds())
	if d.Replayed {
		e.met.Inc("risk_duplicate_replays_total")
		return d
	}
	e.met.Inc("risk_decisions_total", "decision", string(d.Decision))
	for _, c := range d.ReasonCodes {
		e.met.Inc("risk_decision_reasons_total", "decision", string(d.Decision), "reason", c)
	}
	return d
}

func (e *Engine) evaluate(req domain.RiskRequest, now time.Time) domain.RiskDecision {
	v := &verdict{}
	pol := e.pol.Load()
	snap := domain.Snapshot{Request: req, EvaluatedAt: now}
	if pol != nil {
		snap.PolicyVersion, snap.PolicyHash = pol.Version, pol.Hash
	}
	fp := req.Fingerprint()
	var (
		register bool
		limits   policy.Limits
		sym      policy.Symbol
		mark     fixed.Decimal
		notional fixed.Decimal
	)

	// 1. Input validation.
	validate(v, req)

	// 2. Duplicate detection. A redelivery of the same order returns the
	// original decision and changes nothing; reuse of an order_id for a
	// different order is rejected.
	if v.ok() {
		if ord, seen := e.st.Orders[req.OrderID]; seen {
			if ord.Fingerprint == fp {
				d := ord.Decision
				d.Replayed = true
				return d
			}
			v.reject(domain.DuplicateOrderID, domain.Check{Name: "duplicate_order_id",
				Detail: "order_id was already decided with different contents"})
		} else {
			v.pass(domain.Check{Name: "duplicate_order_id"})
		}
	}

	// 3. Reference data. Unknown accounts and symbols have no limits, so
	// they are rejected rather than waved through.
	if v.ok() {
		if pol == nil {
			v.reject(domain.PolicyUnavailable, domain.Check{Name: "policy", Detail: "no policy is loaded"})
		} else {
			var okA, okS bool
			limits, okA = pol.Limits(req.AccountID)
			sym, okS = pol.Symbols[req.Symbol]
			v.limit(!okA, domain.UnknownAccount, domain.Check{Name: "account_known", Scope: "account:" + req.AccountID})
			v.limit(!okS, domain.UnknownSymbol, domain.Check{Name: "symbol_known", Scope: "symbol:" + req.Symbol})
		}
	}

	// From here on the order is a well-formed request from a known account:
	// it is remembered for duplicate detection and counts toward the rate.
	if v.ok() {
		register = true
		snap.Limits, _ = json.Marshal(limits)

		// 4. Kill switches.
		kills := e.st.ActiveKill(req.AccountID, req.Symbol)
		for _, k := range kills {
			snap.KillSwitches = append(snap.KillSwitches, k.Key())
			code := map[domain.KillScope]string{domain.ScopeGlobal: domain.KillSwitchGlobal,
				domain.ScopeAccount: domain.KillSwitchAccount, domain.ScopeSymbol: domain.KillSwitchSymbol}[k.Scope]
			v.reject(code, domain.Check{Name: "kill_switch", Scope: k.Key(),
				Detail: "activated by " + k.ActivatedBy + " at " + k.ActivatedAt.Format(time.RFC3339Nano)})
		}
		if len(kills) == 0 {
			v.pass(domain.Check{Name: "kill_switch"})
		}
	}

	if v.ok() {
		price := *req.LimitPrice
		notional = fixed.MulQty(price, req.Qty)
		acct := e.st.Account(req.AccountID)
		acctScope, symScope := "account:"+req.AccountID, "symbol:"+req.Symbol

		// 5. Instrument and session policy.
		v.limit(!sym.Tradable, domain.InstrumentNotTrading, domain.Check{Name: "instrument_tradable", Scope: symScope})
		v.limit(req.Qty%sym.LotSize != 0, domain.LotSizeViolation, domain.Check{Name: "lot_size", Scope: symScope,
			Observed: fmt.Sprint(req.Qty), Threshold: fmt.Sprint(sym.LotSize)})
		v.limit(price%sym.TickSize != 0, domain.TickSizeViolation, domain.Check{Name: "tick_size", Scope: symScope,
			Observed: price.String(), Threshold: sym.TickSize.String()})
		v.limit(!pol.Session.IsOpen(now), domain.SessionClosed, domain.Check{Name: "session_open", Scope: "policy",
			Observed: now.Format("Mon 15:04Z")})

		// 6. Market data freshness and price collar.
		maxAge := time.Duration(limits.QuoteMaxAgeMs) * time.Millisecond
		maxSkew := time.Duration(limits.MaxClockSkewMs) * time.Millisecond
		q, have := e.mkt.Get(req.Symbol)
		if have {
			snap.Quote = q.View()
		}
		fresh := market.Freshness(q, have, now, maxAge, maxSkew)
		quoteCheck := domain.Check{Name: "quote_fresh", Scope: symScope, Threshold: fmt.Sprintf("%dms", limits.QuoteMaxAgeMs)}
		if have {
			quoteCheck.Observed = fmt.Sprintf("%dms", now.Sub(q.EventTime).Milliseconds())
		}
		if fresh != "" {
			v.reject(fresh, quoteCheck)
			v.skip("price_collar", "no usable quote")
		} else {
			v.pass(quoteCheck)
			mark = q.Mid()
			bps := limits.PriceCollarBps
			if sym.PriceCollarBps > 0 {
				bps = sym.PriceCollarBps
			}
			v.limit(market.OutsideCollar(price, q, bps), domain.PriceCollar, domain.Check{Name: "price_collar", Scope: symScope,
				Observed: price.String(), Threshold: fmt.Sprintf("mid %s +/- %dbps", mark, bps)})
		}

		// 7. Per-order size limits.
		v.limit(req.Qty > limits.MaxOrderQty, domain.MaxOrderQty, domain.Check{Name: "max_order_qty", Scope: acctScope,
			Observed: fmt.Sprint(req.Qty), Threshold: fmt.Sprint(limits.MaxOrderQty)})
		v.limit(notional > limits.MaxOrderNotional, domain.MaxOrderNotional, domain.Check{Name: "max_order_notional", Scope: acctScope,
			Observed: notional.String(), Threshold: limits.MaxOrderNotional.String()})

		// 8. Rate limit over a sliding window. Rejected orders count too,
		// so a runaway client stays throttled.
		window := time.Duration(limits.RateLimit.WindowMs) * time.Millisecond
		inWindow := acct.OrdersSince(now.Add(-window))
		v.limit(inWindow >= limits.RateLimit.MaxOrders, domain.RateLimit, domain.Check{Name: "rate_limit", Scope: acctScope,
			Observed:  fmt.Sprintf("%d in last %dms", inWindow, limits.RateLimit.WindowMs),
			Threshold: fmt.Sprint(limits.RateLimit.MaxOrders)})

		// 9. Daily realized loss.
		pnl := acct.PnLToday(now)
		v.limit(pnl <= -limits.MaxDailyLoss, domain.MaxDailyLoss, domain.Check{Name: "max_daily_loss", Scope: acctScope,
			Observed: pnl.String(), Threshold: "-" + limits.MaxDailyLoss.String()})

		view := &domain.AccountView{RealizedPnLToday: pnl, OrdersInWindow: inWindow, OpenReservations: len(acct.Open)}
		if p := acct.Positions[req.Symbol]; p != nil {
			view.PositionQty = p.Qty
		}
		snap.Account = view

		// 10. Exposure, counting positions plus every open reservation. An
		// order that does not increase exposure is never blocked by these
		// limits, so an account that is over its limit can still reduce.
		if fresh != "" {
			v.skip("max_symbol_exposure", "no usable quote to value the position")
			v.skip("max_gross_exposure", "no usable quote to value the position")
		} else {
			var extraBuy, extraSell fixed.Decimal
			if req.Side == domain.Buy {
				extraBuy = notional
			} else {
				extraSell = notional
			}
			preSym := acct.Exposure(req.Symbol, mark, 0, 0)
			postSym := acct.Exposure(req.Symbol, mark, extraBuy, extraSell)
			var others fixed.Decimal
			for _, s := range acct.Symbols() {
				if s == req.Symbol {
					continue
				}
				m, isFresh := e.markFor(acct, s, now, maxAge, maxSkew)
				if !isFresh {
					view.StaleMarks = append(view.StaleMarks, s)
				}
				others = fixed.Add(others, acct.Exposure(s, m, 0, 0))
			}
			preGross, postGross := fixed.Add(others, preSym), fixed.Add(others, postSym)
			view.SymbolExposure, view.GrossExposure = preSym, preGross

			v.limit(postSym > limits.MaxSymbolExposure && postSym > preSym, domain.MaxSymbolExposure,
				domain.Check{Name: "max_symbol_exposure", Scope: acctScope + "/" + symScope,
					Observed: postSym.String(), Threshold: limits.MaxSymbolExposure.String(), Detail: "before: " + preSym.String()})
			v.limit(postGross > limits.MaxGrossExposure && postGross > preGross, domain.MaxGrossExposure,
				domain.Check{Name: "max_gross_exposure", Scope: acctScope,
					Observed: postGross.String(), Threshold: limits.MaxGrossExposure.String(), Detail: "before: " + preGross.String()})

			// A position in another symbol valued off a stale quote makes
			// the gross figure an estimate: a human decides.
			if len(view.StaleMarks) > 0 {
				v.review(domain.ExposureMarkStale, domain.Check{Name: "exposure_marks_fresh", Scope: acctScope,
					Detail: "valued at last known mark: " + strings.Join(view.StaleMarks, ",")})
			} else {
				v.pass(domain.Check{Name: "exposure_marks_fresh", Scope: acctScope})
			}
		}

		// 11. Large-order review threshold.
		if t := limits.ReviewOrderNotional; t > 0 {
			c := domain.Check{Name: "review_order_notional", Scope: acctScope, Observed: notional.String(), Threshold: t.String()}
			if notional > t {
				v.review(domain.ReviewOrderNotional, c)
			} else {
				v.pass(c)
			}
		}
	}

	d := domain.RiskDecision{
		OrderID:       req.OrderID,
		Decision:      domain.Allow,
		ReasonCodes:   []string{},
		PolicyVersion: snap.PolicyVersion,
		PolicyHash:    snap.PolicyHash,
		DecidedAt:     now,
	}
	switch {
	case !v.ok():
		d.Decision, d.ReasonCodes = domain.Reject, v.rejects
	case len(v.reviews) > 0:
		d.Decision, d.ReasonCodes = domain.Review, v.reviews
	}
	if d.Decision != domain.Reject {
		status, ttl := domain.Reserved, limits.ReservationTTLMs
		if d.Decision == domain.Review {
			status, ttl = domain.PendingReview, limits.ReviewTTLMs
		}
		d.Reservation = &domain.RiskReservation{
			OrderID: req.OrderID, AccountID: req.AccountID, Symbol: req.Symbol, Side: req.Side,
			Quantity: req.Qty, Remaining: req.Qty, Price: *req.LimitPrice, Notional: notional,
			Status: status, ExpiresAt: now.Add(time.Duration(ttl) * time.Millisecond),
		}
	}
	d.Trace = v.trace
	d.SnapshotHash = domain.HashJSON(snap)

	ev := domain.Event{Type: domain.EvDecision, At: now, Decision: &domain.DecisionEvent{
		Decision: d, Snapshot: snap, Fingerprint: fp, Register: register, Mark: mark}}
	seal, err := e.emit(ev)
	if err != nil {
		// Fail closed: no evidence, no approval, and nothing was reserved.
		// The order_id is not consumed, so the caller may retry it.
		d.Decision, d.ReasonCodes, d.Reservation = domain.Reject, []string{domain.AuditUnavailable}, nil
		d.Trace = append(d.Trace, domain.Check{Name: "audit_record", Outcome: "REJECT", Code: domain.AuditUnavailable, Detail: err.Error()})
		return d
	}
	d.Seal = seal
	return d
}

// markFor values a symbol other than the one being traded: the live mid if
// its quote is fresh, otherwise the last known mark, flagged as stale. A
// symbol with reservations but no position needs no mark at all.
func (e *Engine) markFor(a *state.Account, symbol string, now time.Time, maxAge, maxSkew time.Duration) (fixed.Decimal, bool) {
	q, have := e.mkt.Get(symbol)
	if market.Freshness(q, have, now, maxAge, maxSkew) == "" {
		return q.Mid(), true
	}
	p := a.Positions[symbol]
	if p == nil || p.Qty == 0 {
		return 0, true
	}
	return p.LastMark, false
}

// reservation returns the live reservation of an order, or why there is none.
func (e *Engine) reservation(orderID string) (*domain.RiskReservation, error) {
	ord, ok := e.st.Orders[orderID]
	switch {
	case !ok:
		return nil, ErrNotFound
	case ord.Reservation == nil:
		return nil, ErrNoReservation
	case ord.Reservation.Status == domain.Released:
		return ord.Reservation, ErrReleased
	}
	return ord.Reservation, nil
}

// gate enforces the two conditions that stop a reservation from becoming a
// live order: an active kill switch, or an expired lease. Either releases
// the reservation.
func (e *Engine) gate(r *domain.RiskReservation, now time.Time) error {
	reason, cause := "", error(nil)
	if len(e.st.ActiveKill(r.AccountID, r.Symbol)) > 0 {
		reason, cause = "KILL_SWITCH", ErrKillSwitch
	} else if now.After(r.ExpiresAt) {
		reason, cause = "EXPIRED", ErrExpired
	}
	if cause == nil {
		return nil
	}
	if _, err := e.emit(domain.Event{Type: domain.EvRelease, At: now, Release: &domain.ReleaseEvent{OrderID: r.OrderID, Reason: reason}}); err != nil {
		return err
	}
	return cause
}

// Commit is the OMS's confirmation, made immediately before it sends the
// order, that the reservation is now a live order. It is the last
// kill-switch checkpoint and is idempotent.
func (e *Engine) Commit(orderID string) (domain.RiskReservation, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	r, err := e.reservation(orderID)
	if err != nil {
		return deref(r), err
	}
	now := e.clock.Now()
	switch r.Status {
	case domain.Committed:
		return *r, nil
	case domain.PendingReview:
		return *r, ErrPendingReview
	}
	if err := e.gate(r, now); err != nil {
		return *r, err
	}
	_, err = e.emit(domain.Event{Type: domain.EvCommit, At: now, Commit: &domain.CommitEvent{OrderID: orderID}})
	return *r, err
}

// Approve turns a REVIEW decision's held capacity into a normal reservation
// with a fresh commit lease.
func (e *Engine) Approve(orderID, by string) (domain.RiskReservation, error) {
	if !validID(by, maxIDLen) {
		return domain.RiskReservation{}, fmt.Errorf("%w: approved_by is required", ErrInvalid)
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	r, err := e.reservation(orderID)
	if err != nil {
		return deref(r), err
	}
	if r.Status != domain.PendingReview {
		return *r, ErrNotPending
	}
	now := e.clock.Now()
	if err := e.gate(r, now); err != nil {
		return *r, err
	}
	ttl := int64(60_000)
	if pol := e.pol.Load(); pol != nil {
		if l, ok := pol.Limits(r.AccountID); ok {
			ttl = l.ReservationTTLMs
		}
	}
	_, err = e.emit(domain.Event{Type: domain.EvApprove, At: now, Approve: &domain.ApproveEvent{
		OrderID: orderID, ApprovedBy: by, ExpiresAt: now.Add(time.Duration(ttl) * time.Millisecond)}})
	return *r, err
}

// Release frees whatever capacity the order still holds (cancel, venue
// reject, review declined). Releasing twice is a no-op.
func (e *Engine) Release(orderID, reason string) (domain.RiskReservation, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	r, err := e.reservation(orderID)
	if errors.Is(err, ErrReleased) {
		return *r, nil
	}
	if err != nil {
		return deref(r), err
	}
	if reason == "" {
		reason = "RELEASED"
	}
	_, err = e.emit(domain.Event{Type: domain.EvRelease, At: e.clock.Now(), Release: &domain.ReleaseEvent{OrderID: orderID, Reason: reason}})
	return *r, err
}

type FillResult struct {
	Duplicate   bool                    `json:"duplicate"`
	Unreserved  bool                    `json:"unreserved"` // the fill was not covered by held capacity
	Reservation *domain.RiskReservation `json:"reservation,omitempty"`
	Position    state.Position          `json:"position"`
}

// Fill records an execution. Fills are facts and are always applied to the
// position, even for an order whose reservation is gone; that case is
// flagged and counted because it means the OMS acted without cover.
func (e *Engine) Fill(orderID, fillID string, qty int64, price fixed.Decimal) (FillResult, error) {
	if !validID(fillID, maxIDLen) || qty <= 0 || qty > MaxQty || price <= 0 || price > market.MaxPrice {
		return FillResult{}, fmt.Errorf("%w: need fill_id, qty in [1,%d] and price in (0,%s]", ErrInvalid, MaxQty, market.MaxPrice)
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	ord, ok := e.st.Orders[orderID]
	if !ok {
		return FillResult{}, ErrNotFound
	}
	res := FillResult{}
	if _, dup := ord.Fills[fillID]; dup {
		res.Duplicate = true
	} else {
		before := e.st.UnreservedFills
		if _, err := e.emit(domain.Event{Type: domain.EvFill, At: e.clock.Now(),
			Fill: &domain.FillEvent{OrderID: orderID, FillID: fillID, Qty: qty, Price: price}}); err != nil {
			return FillResult{}, err
		}
		if res.Unreserved = e.st.UnreservedFills > before; res.Unreserved {
			e.met.Inc("risk_unreserved_fills_total")
		}
	}
	if ord.Reservation != nil {
		r := *ord.Reservation
		res.Reservation = &r
	}
	if p := e.st.Account(ord.Request.AccountID).Positions[ord.Request.Symbol]; p != nil {
		res.Position = *p
	}
	return res, nil
}

// Sweep releases RESERVED and PENDING_REVIEW reservations whose lease has
// lapsed, returning how many it released. Committed orders never expire.
func (e *Engine) Sweep() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	now := e.clock.Now()
	var ids []string
	for _, a := range e.st.Accounts {
		for id, r := range a.Open {
			if r.Status != domain.Committed && now.After(r.ExpiresAt) {
				ids = append(ids, id)
			}
		}
	}
	sort.Strings(ids)
	n := 0
	for _, id := range ids {
		if _, err := e.emit(domain.Event{Type: domain.EvRelease, At: now, Release: &domain.ReleaseEvent{OrderID: id, Reason: "EXPIRED"}}); err == nil {
			n++
		}
	}
	return n
}

// SetKillSwitch turns a switch on or off. Once it returns for an
// activation, no evaluation, approval or commit in its scope can succeed.
//
// Activation is the one mutation applied even when the audit log is down:
// refusing to stop trading because the evidence store is broken would be
// failing open. In that case durable is false and the error is returned
// alongside the active switch. Deactivation always requires the record.
func (e *Engine) SetKillSwitch(scope domain.KillScope, target string, enabled bool, by, reason string) (ks domain.KillSwitch, seal domain.Seal, durable bool, err error) {
	switch scope {
	case domain.ScopeGlobal:
		if target != "" {
			return ks, seal, false, fmt.Errorf("%w: global scope takes no target", ErrInvalid)
		}
	case domain.ScopeAccount, domain.ScopeSymbol:
		if !validID(target, maxIDLen) {
			return ks, seal, false, fmt.Errorf("%w: bad %s", ErrInvalid, scope)
		}
	default:
		return ks, seal, false, fmt.Errorf("%w: unknown scope", ErrInvalid)
	}
	if !validID(by, maxIDLen) {
		return ks, seal, false, fmt.Errorf("%w: activated_by is required", ErrInvalid)
	}
	e.mu.Lock()
	ks = domain.KillSwitch{Scope: scope, Target: target, Enabled: enabled, ActivatedBy: by, ActivatedAt: e.clock.Now(), Reason: reason}
	ev := domain.Event{Type: domain.EvKill, At: ks.ActivatedAt, Kill: &ks}
	seal, err = e.emit(ev)
	durable = err == nil
	if err != nil && enabled {
		e.st.Apply(ev, domain.Seal{})
	}
	applied := err == nil || enabled
	e.met.Set("risk_kill_switches_active", float64(len(e.st.Kill)))
	e.mu.Unlock()
	if applied && e.onKill != nil {
		e.onKill(ks, seal)
	}
	return ks, seal, durable, err
}

func (e *Engine) KillSwitches() []domain.KillSwitch {
	e.mu.Lock()
	defer e.mu.Unlock()
	out := make([]domain.KillSwitch, 0, len(e.st.Kill))
	for _, k := range e.st.Kill {
		out = append(out, k)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Key() < out[j].Key() })
	return out
}

// Reconcile compares engine positions with the broker's and reports drift
// (engine minus broker) per symbol. It never adjusts positions: drift is a
// finding for a human, not something to paper over.
func (e *Engine) Reconcile(account string, broker map[string]int64) (domain.ReconcileEvent, error) {
	if !validID(account, maxIDLen) {
		return domain.ReconcileEvent{}, fmt.Errorf("%w: bad account_id", ErrInvalid)
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	rec := domain.ReconcileEvent{AccountID: account, Drift: map[string]int64{}}
	a := e.st.Account(account)
	diff := func(sym string) {
		var mine int64
		if p := a.Positions[sym]; p != nil {
			mine = p.Qty
		}
		if d := mine - broker[sym]; d != 0 {
			rec.Drift[sym] = d
		}
	}
	for s := range a.Positions {
		diff(s)
	}
	for s := range broker {
		diff(s)
	}
	for _, d := range rec.Drift {
		if d < 0 {
			d = -d
		}
		rec.AbsDrift += d
	}
	if _, err := e.emit(domain.Event{Type: domain.EvReconcile, At: e.clock.Now(), Reconcile: &rec}); err != nil {
		return rec, err
	}
	e.met.Set("risk_state_drift_abs_qty", float64(rec.AbsDrift), "account", account)
	return rec, nil
}

// Decision returns the recorded decision for an order.
func (e *Engine) Decision(orderID string) (domain.RiskDecision, bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	ord, ok := e.st.Orders[orderID]
	if !ok {
		return domain.RiskDecision{}, false
	}
	return ord.Decision, true
}

// OrderAccount returns the account an order belongs to, or "".
func (e *Engine) OrderAccount(orderID string) string {
	e.mu.Lock()
	defer e.mu.Unlock()
	if ord, ok := e.st.Orders[orderID]; ok {
		return ord.Request.AccountID
	}
	return ""
}

type AccountState struct {
	AccountID        string                    `json:"account_id"`
	RealizedPnLToday fixed.Decimal             `json:"realized_pnl_today"`
	Positions        map[string]state.Position `json:"positions"`
	Reservations     []domain.RiskReservation  `json:"open_reservations"`
}

func (e *Engine) AccountState(account string) AccountState {
	e.mu.Lock()
	defer e.mu.Unlock()
	a := e.st.Account(account)
	out := AccountState{AccountID: account, RealizedPnLToday: a.PnLToday(e.clock.Now()),
		Positions: map[string]state.Position{}, Reservations: []domain.RiskReservation{}}
	for s, p := range a.Positions {
		out.Positions[s] = *p
	}
	for _, r := range a.Open {
		out.Reservations = append(out.Reservations, *r)
	}
	sort.Slice(out.Reservations, func(i, j int) bool { return out.Reservations[i].OrderID < out.Reservations[j].OrderID })
	return out
}

// RecordMalformed leaves evidence of a request that could not be parsed.
func (e *Engine) RecordMalformed(bodyHash, problem string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.emit(domain.Event{Type: domain.EvMalformed, At: e.clock.Now(), Malformed: &domain.MalformedEvent{BodySHA256: bodyHash, Error: problem}})
	e.met.Inc("risk_decisions_total", "decision", string(domain.Reject))
	e.met.Inc("risk_decision_reasons_total", "decision", string(domain.Reject), "reason", domain.MalformedRequest)
}

func deref(r *domain.RiskReservation) domain.RiskReservation {
	if r == nil {
		return domain.RiskReservation{}
	}
	return *r
}
