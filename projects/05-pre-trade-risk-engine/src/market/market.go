// Package market is the market-check service: it holds the latest quote per
// symbol and answers two questions for the risk core — is this quote fresh
// enough to trust, and is this price inside the collar around it.
package market

import (
	"errors"
	"sync"
	"time"

	"riskengine/src/domain"
	"riskengine/src/fixed"
)

type Quote struct {
	Symbol      string        `json:"symbol"`
	Bid         fixed.Decimal `json:"bid"`
	Ask         fixed.Decimal `json:"ask"`
	EventTime   time.Time     `json:"event_time"`   // venue timestamp
	ReceiveTime time.Time     `json:"receive_time"` // set locally on ingest
}

// Mid is the collar and valuation reference price (truncated to 1e-4).
func (q Quote) Mid() fixed.Decimal { return (q.Bid + q.Ask) / 2 }

func (q Quote) View() *domain.QuoteView {
	return &domain.QuoteView{Bid: q.Bid, Ask: q.Ask, EventTime: q.EventTime, ReceiveTime: q.ReceiveTime}
}

var (
	ErrInvalidQuote = errors.New("invalid quote: need symbol, 0 < bid <= ask and an event_time")
	ErrOutOfOrder   = errors.New("quote is older than the one already held")
	ErrFutureQuote  = errors.New("quote event_time is too far ahead of the local clock")
)

// MaxPrice bounds quotes and order prices (1,000,000.0000) so that
// price*qty cannot overflow for any valid quantity.
const MaxPrice = fixed.Decimal(1_000_000 * fixed.Scale)

type Store struct {
	mu        sync.RWMutex
	quotes    map[string]Quote
	maxFuture time.Duration
}

// NewStore rejects quotes stamped more than maxFuture ahead of local time.
// Without that guard one bad timestamp would shadow every later quote.
func NewStore(maxFuture time.Duration) *Store {
	return &Store{quotes: map[string]Quote{}, maxFuture: maxFuture}
}

// Update ingests a quote received at now. Invalid, future-dated and
// out-of-order quotes are refused and the previous quote is kept.
func (s *Store) Update(q Quote, now time.Time) error {
	if q.Symbol == "" || q.Bid <= 0 || q.Ask < q.Bid || q.Ask > MaxPrice || q.EventTime.IsZero() {
		return ErrInvalidQuote
	}
	q.EventTime = q.EventTime.Round(0).UTC()
	if q.EventTime.Sub(now) > s.maxFuture {
		return ErrFutureQuote
	}
	q.ReceiveTime = now
	s.mu.Lock()
	defer s.mu.Unlock()
	if cur, ok := s.quotes[q.Symbol]; ok && q.EventTime.Before(cur.EventTime) {
		return ErrOutOfOrder
	}
	s.quotes[q.Symbol] = q
	return nil
}

func (s *Store) Get(symbol string) (Quote, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	q, ok := s.quotes[symbol]
	return q, ok
}

// Freshness returns "" when the quote may be used, otherwise the reason code
// explaining why it may not. Age is measured from venue event time.
func Freshness(q Quote, ok bool, now time.Time, maxAge, maxSkew time.Duration) string {
	switch {
	case !ok:
		return domain.NoMarketData
	case q.EventTime.Sub(now) > maxSkew:
		return domain.QuoteClockSkew
	case now.Sub(q.EventTime) > maxAge:
		return domain.StaleQuote
	}
	return ""
}

// OutsideCollar reports whether price is more than bps basis points away
// from the quote mid. Computed on 2*mid = bid+ask to stay in integers.
func OutsideCollar(price fixed.Decimal, q Quote, bps int64) bool {
	twoMid := int64(q.Bid + q.Ask)
	dev := int64(fixed.Abs(fixed.Sub(price*2, fixed.Decimal(twoMid))))
	return fixed.MulInt(dev, 10000) > fixed.MulInt(twoMid, bps)
}
