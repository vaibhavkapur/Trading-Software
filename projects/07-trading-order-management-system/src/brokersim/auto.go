package brokersim

import (
	"fmt"

	"oms/src/broker"
	"oms/src/domain"
	"oms/src/fixed"
)

// Auto mode: a deliberately tiny market model. Each tradable symbol has one
// reference price. An order is marketable when it is a market order, a buy
// at or above the reference price, or a sell at or below it. A marketable
// order fills completely at the reference price in up to MaxSlices fills; any
// other order rests until the reference price moves (SetPrice), it is
// cancelled, or the session closes. An IOC order that is not marketable
// expires at once.

// autoAccept acknowledges or rejects a newly received order.
func (s *Sim) autoAccept(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	o, ok := s.orders[id]
	if !ok || o.Status != broker.StatusReceived {
		return
	}
	if _, tradable := s.cfg.Prices[o.Symbol]; !tradable {
		o.Status, o.Reason = broker.StatusRejected, "UNKNOWN_SYMBOL: "+o.Symbol+" is not tradable"
		s.emit(o, broker.EventRejected, func(ev *broker.WebhookEvent) { ev.Reason = o.Reason })
		return
	}
	o.Status = broker.StatusWorking
	s.emit(o, broker.EventAcknowledged, nil)
	s.match(o)
}

func (s *Sim) marketable(o *order) bool {
	ref, ok := s.cfg.Prices[o.Symbol]
	if !ok {
		return false
	}
	switch {
	case o.Type == domain.Market || o.LimitPrice == nil:
		return true
	case o.Side == domain.Buy:
		return *o.LimitPrice >= ref
	default:
		return *o.LimitPrice <= ref
	}
}

// match starts filling an acknowledged order if it is marketable. The caller
// holds s.mu.
func (s *Sim) match(o *order) {
	if !o.open() || o.Status == broker.StatusReceived || o.matching {
		return
	}
	if !s.marketable(o) {
		if o.TimeInForce == domain.IOC {
			o.Status, o.Reason = broker.StatusExpired, "IOC order was not marketable"
			s.emit(o, broker.EventExpired, func(ev *broker.WebhookEvent) { ev.Reason = o.Reason })
		}
		return
	}
	o.matching = true
	id := o.ClientOrderID
	s.after(s.cfg.FillDelay, func() { s.autoFill(id) })
}

// autoFill executes the next slice of a marketable order.
func (s *Sim) autoFill(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	o, ok := s.orders[id]
	if !ok {
		return
	}
	o.matching = false
	remaining := o.Qty - o.CumQty
	if !o.open() || remaining <= 0 || !s.marketable(o) {
		return // cancelled, replaced or no longer marketable while waiting
	}
	slicesLeft := int64(s.cfg.MaxSlices - o.slices)
	qty := remaining
	if slicesLeft > 1 {
		qty = (remaining + slicesLeft - 1) / slicesLeft
	}
	s.fill(o, qty, s.cfg.Prices[o.Symbol])
	o.slices++
	s.match(o)
}

// SetPrice moves a symbol's reference price (adding the symbol if needed)
// and, in auto mode, matches the orders resting on it.
func (s *Sim) SetPrice(symbol string, price fixed.Micros) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.cfg.Prices[symbol] = price
	if !s.cfg.Auto {
		return
	}
	for _, id := range s.orderIDs {
		if o := s.orders[id]; o.Symbol == symbol {
			s.match(o)
		}
	}
}

// requestCancel records a cancel request. known is false for an unknown
// order; duplicate is true when the idempotency key was seen before.
func (s *Sim) requestCancel(id, key string) (known, duplicate bool) {
	o, ok := s.orders[id]
	if !ok {
		return false, false
	}
	if s.seenAction(key) {
		return true, true
	}
	s.stats.CancelRequests++
	o.cancelPending = true
	if s.cfg.Auto {
		s.after(s.cfg.CancelDelay, func() { s.autoCancel(id) })
	}
	return true, false
}

func (s *Sim) autoCancel(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	o, ok := s.orders[id]
	if !ok || !o.cancelPending {
		return
	}
	o.cancelPending = false
	if !o.open() {
		reason := fmt.Sprintf("TOO_LATE: order is %s", o.Status)
		s.emit(o, broker.EventCancelRejected, func(ev *broker.WebhookEvent) { ev.Reason = reason })
		return
	}
	o.Status, o.replace = broker.StatusCancelled, nil
	s.emit(o, broker.EventCancelled, nil)
}

// requestReplace records a replace request.
func (s *Sim) requestReplace(id, key string, r broker.ReplaceRequest) (known, duplicate bool) {
	o, ok := s.orders[id]
	if !ok {
		return false, false
	}
	if s.seenAction(key) {
		return true, true
	}
	s.stats.ReplaceRequests++
	o.replace = &r
	if s.cfg.Auto {
		s.after(s.cfg.CancelDelay, func() { s.autoReplace(id) })
	}
	return true, false
}

func (s *Sim) autoReplace(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	o, ok := s.orders[id]
	if !ok || o.replace == nil {
		return
	}
	r := *o.replace
	o.replace = nil
	reject := func(reason string) {
		s.emit(o, broker.EventReplaceReject, func(ev *broker.WebhookEvent) { ev.Reason = reason })
	}
	switch {
	case !o.open():
		reject(fmt.Sprintf("TOO_LATE: order is %s", o.Status))
	case r.Qty <= o.CumQty:
		reject(fmt.Sprintf("new quantity %d is not above the filled quantity %d", r.Qty, o.CumQty))
	default:
		o.Qty, o.LimitPrice = r.Qty, r.LimitPrice
		s.emit(o, broker.EventReplaced, func(ev *broker.WebhookEvent) {
			ev.OrderQty, ev.LimitPrice = o.Qty, o.LimitPrice
		})
		s.match(o)
	}
}

// seenAction reports whether a cancel or replace request with this
// idempotency key was already handled, and remembers the key.
func (s *Sim) seenAction(key string) bool {
	if key == "" {
		return false
	}
	if s.actionKeys[key] {
		s.stats.DuplicateActions++
		return true
	}
	s.actionKeys[key] = true
	return false
}
