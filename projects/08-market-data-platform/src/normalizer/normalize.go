// Package normalizer turns provider events into canonical events: it
// resolves instrument IDs, converts decimal prices and sizes to exact integer
// ticks and lots, unifies event semantics, and attaches provenance.
//
// The rules here are versioned by domain.NormalizerVersion, which is stamped
// on every event. Change a rule, bump the version.
package normalizer

import (
	"errors"
	"fmt"
	"time"

	"mdp/src/domain"
	"mdp/src/fixed"
	"mdp/src/ingestors"
	"mdp/src/refdata"
)

// Reject reasons.
const (
	ReasonUnknownProvider   = "UNKNOWN_PROVIDER"
	ReasonUnknownInstrument = "UNKNOWN_INSTRUMENT"
	ReasonUnknownKind       = "UNKNOWN_KIND"
	ReasonMissingEventTime  = "MISSING_EVENT_TIME"
	ReasonEventTimeRange    = "EVENT_TIME_OUT_OF_RANGE"
	ReasonBadPrice          = "BAD_PRICE"
	ReasonPriceNotOnTick    = "PRICE_NOT_ON_TICK"
	ReasonNonPositivePrice  = "NON_POSITIVE_PRICE"
	ReasonBadQuantity       = "BAD_QUANTITY"
	ReasonQuantityNotOnLot  = "QUANTITY_NOT_ON_LOT"
	ReasonNonPositiveQty    = "NON_POSITIVE_QUANTITY"
	ReasonNegativeSize      = "NEGATIVE_SIZE"
	ReasonMissingTradeID    = "MISSING_TRADE_ID"
	ReasonEmptyQuote        = "EMPTY_QUOTE"
	ReasonCrossedQuote      = "CROSSED_QUOTE"
	ReasonEmptyDepth        = "EMPTY_DEPTH"
	ReasonUnsortedDepth     = "UNSORTED_DEPTH"
	ReasonCrossedBook       = "CROSSED_BOOK"
	ReasonUndecodable       = "UNDECODABLE_RECORD"
)

// Rejection explains why an event has no canonical form.
type Rejection struct {
	Reason string
	Detail string
}

func (r *Rejection) Error() string { return r.Reason + ": " + r.Detail }

func reject(reason, format string, args ...any) *Rejection {
	return &Rejection{Reason: reason, Detail: fmt.Sprintf(format, args...)}
}

// Options are the data-quality thresholds. Events past a threshold are kept
// and flagged; their timestamps are never altered.
type Options struct {
	// SkewTolerance: event_time may lead receive_time by this much before the
	// event is flagged clock_skew.
	SkewTolerance time.Duration
	// StaleAfter: an event whose event_time trails receive_time by more than
	// this is flagged stale.
	StaleAfter time.Duration
}

func DefaultOptions() Options {
	return Options{SkewTolerance: 250 * time.Millisecond, StaleAfter: 5 * time.Second}
}

// Normalizer is stateless and safe for concurrent use.
type Normalizer struct {
	reg *refdata.Registry
	opt Options
}

func New(reg *refdata.Registry, opt Options) *Normalizer {
	return &Normalizer{reg: reg, opt: opt}
}

// Normalize returns a domain.Trade, domain.Quote or domain.Depth, or the
// reason the event was rejected.
func (n *Normalizer) Normalize(ev ingestors.Event) (any, *Rejection) {
	prov, ok := n.reg.Provider(ev.Provider)
	if !ok {
		return nil, reject(ReasonUnknownProvider, "provider %q", ev.Provider)
	}
	inst, ok := n.reg.Lookup(prov.Venue, ev.Symbol)
	if !ok {
		return nil, reject(ReasonUnknownInstrument, "%s symbol %q", prov.Venue, ev.Symbol)
	}
	if ev.EventTime.Unix() <= 0 {
		return nil, reject(ReasonMissingEventTime, "event time %s", ev.EventTime.Format(time.RFC3339Nano))
	}
	// Nanosecond timestamps, here and in the store, end in the year 2262.
	// A venue clock that far out is broken, not merely skewed.
	if !domain.TimeInRange(ev.EventTime) {
		return nil, reject(ReasonEventTimeRange, "event time %s", ev.EventTime.Format(time.RFC3339))
	}
	meta := domain.Meta{
		Provider: ev.Provider, Session: ev.Session, SeqDomain: ev.Domain, Seq: ev.Seq,
		Source: ev.Source, NormalizerVersion: domain.NormalizerVersion,
	}
	if ev.EventTime.Sub(ev.RecvTime) > n.opt.SkewTolerance {
		meta.Flags = append(meta.Flags, domain.FlagClockSkew)
	}
	if ev.RecvTime.Sub(ev.EventTime) > n.opt.StaleAfter {
		meta.Flags = append(meta.Flags, domain.FlagStale)
	}
	eventTime, recvTime := ev.EventTime.UTC(), ev.RecvTime.UTC()

	switch ev.Kind {
	case domain.KindTrade:
		px, rej := price(inst, ev.Price)
		if rej != nil {
			return nil, rej
		}
		qty, rej := quantity(inst, ev.Qty, false)
		if rej != nil {
			return nil, rej
		}
		if ev.TradeID == "" {
			return nil, reject(ReasonMissingTradeID, "trade without a venue trade id cannot be deduplicated")
		}
		return domain.Trade{
			Type: domain.KindTrade, InstrumentID: inst.ID, TradeID: ev.TradeID, PriceTicks: px, Quantity: qty,
			Aggressor: aggressor(prov, ev.Side), EventTime: eventTime, ReceiveTime: recvTime, Meta: meta,
		}, nil

	case domain.KindQuote:
		q := domain.Quote{Type: domain.KindQuote, InstrumentID: inst.ID, EventTime: eventTime, ReceiveTime: recvTime}
		var rej *Rejection
		if ev.Bid == "" {
			meta.Flags = append(meta.Flags, domain.FlagNoBid)
		} else if q.BidTicks, q.BidSize, rej = side(inst, ev.Bid, ev.BidQty); rej != nil {
			return nil, rej
		}
		if ev.Ask == "" {
			meta.Flags = append(meta.Flags, domain.FlagNoAsk)
		} else if q.AskTicks, q.AskSize, rej = side(inst, ev.Ask, ev.AskQty); rej != nil {
			return nil, rej
		}
		switch {
		case ev.Bid == "" && ev.Ask == "":
			return nil, reject(ReasonEmptyQuote, "quote with neither bid nor ask")
		case ev.Bid != "" && ev.Ask != "" && q.BidTicks > q.AskTicks:
			return nil, reject(ReasonCrossedQuote, "bid %s > ask %s", ev.Bid, ev.Ask)
		case ev.Bid != "" && ev.Ask != "" && q.BidTicks == q.AskTicks:
			meta.Flags = append(meta.Flags, domain.FlagLocked)
		}
		q.Meta = meta
		return q, nil

	case domain.KindDepth:
		bids, rej := levels(inst, ev.Bids, true)
		if rej != nil {
			return nil, rej
		}
		asks, rej := levels(inst, ev.Asks, false)
		if rej != nil {
			return nil, rej
		}
		if len(bids) == 0 && len(asks) == 0 {
			return nil, reject(ReasonEmptyDepth, "depth with no levels")
		}
		if len(bids) > 0 && len(asks) > 0 && bids[0].PriceTicks >= asks[0].PriceTicks {
			return nil, reject(ReasonCrossedBook, "best bid %d >= best ask %d ticks", bids[0].PriceTicks, asks[0].PriceTicks)
		}
		return domain.Depth{
			Type: domain.KindDepth, InstrumentID: inst.ID, Bids: bids, Asks: asks,
			EventTime: eventTime, ReceiveTime: recvTime, Meta: meta,
		}, nil
	}
	return nil, reject(ReasonUnknownKind, "kind %q", ev.Kind)
}

// aggressor maps a provider's trade side to the canonical aggressor side.
func aggressor(p refdata.Provider, side string) string {
	if side == "" || p.TradeSide == "aggressor" {
		return side
	}
	// The provider reports the resting order's side; the aggressor took the other.
	if side == "buy" {
		return "sell"
	}
	return "buy"
}

func price(inst *refdata.Instrument, s string) (int64, *Rejection) {
	ticks, err := inst.Tick.Units(s)
	switch {
	case errors.Is(err, fixed.ErrNotMultiple):
		return 0, reject(ReasonPriceNotOnTick, "price %s is not a multiple of tick %s", s, inst.TickSize)
	case err != nil:
		return 0, reject(ReasonBadPrice, "%v", err)
	case ticks <= 0:
		return 0, reject(ReasonNonPositivePrice, "price %s", s)
	}
	return ticks, nil
}

// quantity converts a size to lots. Trades need a positive quantity; a
// quote or depth level may rest with zero displayed size only if allowZero.
func quantity(inst *refdata.Instrument, s string, allowZero bool) (int64, *Rejection) {
	lots, err := inst.Lot.Units(s)
	switch {
	case errors.Is(err, fixed.ErrNotMultiple):
		return 0, reject(ReasonQuantityNotOnLot, "quantity %s is not a multiple of lot %s", s, inst.LotSize)
	case err != nil:
		return 0, reject(ReasonBadQuantity, "%v", err)
	case lots <= 0 && !allowZero:
		return 0, reject(ReasonNonPositiveQty, "quantity %s", s)
	case lots < 0:
		return 0, reject(ReasonNegativeSize, "size %s", s)
	}
	return lots, nil
}

func side(inst *refdata.Instrument, px, qty string) (int64, int64, *Rejection) {
	ticks, rej := price(inst, px)
	if rej != nil {
		return 0, 0, rej
	}
	lots, rej := quantity(inst, qty, true)
	if rej != nil {
		return 0, 0, rej
	}
	return ticks, lots, nil
}

func levels(inst *refdata.Instrument, raw [][2]string, descending bool) ([]domain.Level, *Rejection) {
	out := make([]domain.Level, 0, len(raw))
	for i, l := range raw {
		ticks, rej := price(inst, l[0])
		if rej != nil {
			return nil, rej
		}
		lots, rej := quantity(inst, l[1], false)
		if rej != nil {
			return nil, rej
		}
		if i > 0 {
			prev := out[i-1].PriceTicks
			if (descending && ticks >= prev) || (!descending && ticks <= prev) {
				return nil, reject(ReasonUnsortedDepth, "level %d price %s out of order", i, l[0])
			}
		}
		out = append(out, domain.Level{PriceTicks: ticks, Quantity: lots})
	}
	return out, nil
}
