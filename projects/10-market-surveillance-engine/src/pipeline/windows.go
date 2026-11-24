package pipeline

import (
	"sort"
	"time"

	"surveillance/src/domain"
)

// Key identifies one account's activity in one symbol.
type Key struct{ Account, Symbol string }

type counts struct {
	newOrders, amends, cancels, trades int
	tradedQty                          int64
	refs                               []string
	truncated                          bool
	lastID                             string
	lastTS                             int64
	lastRecv                           time.Time
}

// Windower aggregates ordered events into hopping event-time windows.
// Windows are Size long and start every Hop, aligned to the epoch, so an
// event belongs to Size/Hop overlapping windows: "rolling" features at Hop
// resolution. Counts are held per Hop-sized bucket and summed on close.
type Windower struct {
	size, hop int64
	maxRefs   int
	buckets   map[int64]map[Key]*counts
	lastEnd   int64
}

// NewWindower requires size to be a whole multiple of hop.
func NewWindower(size, hop time.Duration, maxRefs int) *Windower {
	return &Windower{size: int64(size), hop: int64(hop), maxRefs: maxRefs,
		buckets: map[int64]map[Key]*counts{}, lastEnd: unset}
}

func (w *Windower) Size() time.Duration { return time.Duration(w.size) }
func (w *Windower) Hop() time.Duration  { return time.Duration(w.hop) }

// Pending reports whether any event is still waiting in an open window.
func (w *Windower) Pending() bool { return len(w.buckets) > 0 }

func floorTo(ts, step int64) int64 {
	r := ts % step
	if r < 0 {
		r += step
	}
	return ts - r
}

func (w *Windower) slot(ev domain.Event, account string) *counts {
	b := floorTo(ev.TS(), w.hop)
	m := w.buckets[b]
	if m == nil {
		m = map[Key]*counts{}
		w.buckets[b] = m
	}
	k := Key{account, ev.Symbol}
	c := m[k]
	if c == nil {
		c = &counts{}
		m[k] = c
	}
	if len(c.refs) < w.maxRefs {
		c.refs = append(c.refs, ev.ID)
	} else {
		c.truncated = true
	}
	c.lastID, c.lastTS, c.lastRecv = ev.ID, ev.TS(), ev.ReceivedAt
	return c
}

// Add counts one event. Events must arrive in event-time order, which is
// what Reorder.Release guarantees.
func (w *Windower) Add(ev domain.Event) {
	if ev.Kind == domain.KindTrade {
		c := w.slot(ev, ev.BuyAccount)
		c.trades++
		c.tradedQty += ev.Qty
		// An account on both sides of a trade took part in one trade, not two.
		if ev.SellAccount != ev.BuyAccount {
			c = w.slot(ev, ev.SellAccount)
			c.trades++
			c.tradedQty += ev.Qty
		}
		return
	}
	c := w.slot(ev, ev.AccountID)
	switch ev.Action {
	case domain.ActionNew:
		c.newOrders++
	case domain.ActionAmend:
		c.amends++
	case domain.ActionCancel:
		c.cancels++
	}
}

// Close emits every window whose end the watermark has reached, oldest
// first and then by key, and forgets buckets no open window still needs.
// Only windows that contain activity are emitted.
func (w *Windower) Close(watermark int64) []domain.FeatureWindow {
	limit := floorTo(watermark, w.hop)
	if w.lastEnd != unset && limit <= w.lastEnd {
		return nil
	}
	var out []domain.FeatureWindow
	if len(w.buckets) > 0 {
		lo, hi := int64(0), int64(0)
		first := true
		for b := range w.buckets {
			if first || b < lo {
				lo = b
			}
			if first || b > hi {
				hi = b
			}
			first = false
		}
		from := lo + w.hop
		if w.lastEnd != unset && w.lastEnd+w.hop > from {
			from = w.lastEnd + w.hop
		}
		to := hi + w.size
		if limit < to {
			to = limit
		}
		for end := from; end <= to; end += w.hop {
			out = append(out, w.window(end)...)
		}
	}
	w.lastEnd = limit
	for b := range w.buckets {
		if b+w.size <= w.lastEnd {
			delete(w.buckets, b)
		}
	}
	return out
}

func (w *Windower) window(end int64) []domain.FeatureWindow {
	start := end - w.size
	agg := map[Key]*counts{}
	for b := start; b < end; b += w.hop {
		for k, c := range w.buckets[b] {
			a := agg[k]
			if a == nil {
				a = &counts{}
				agg[k] = a
			}
			a.newOrders += c.newOrders
			a.amends += c.amends
			a.cancels += c.cancels
			a.trades += c.trades
			a.tradedQty += c.tradedQty
			a.truncated = a.truncated || c.truncated
			room := w.maxRefs - len(a.refs)
			if room >= len(c.refs) {
				a.refs = append(a.refs, c.refs...)
			} else {
				a.refs = append(a.refs, c.refs[:room]...)
				a.truncated = true
			}
			if a.lastID == "" || c.lastTS >= a.lastTS {
				a.lastID, a.lastTS, a.lastRecv = c.lastID, c.lastTS, c.lastRecv
			}
		}
	}
	keys := make([]Key, 0, len(agg))
	for k := range agg {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		if keys[i].Account != keys[j].Account {
			return keys[i].Account < keys[j].Account
		}
		return keys[i].Symbol < keys[j].Symbol
	})
	out := make([]domain.FeatureWindow, 0, len(keys))
	for _, k := range keys {
		a := agg[k]
		fw := domain.FeatureWindow{
			AccountID: k.Account, Symbol: k.Symbol,
			Start: time.Unix(0, start).UTC(), End: time.Unix(0, end).UTC(),
			NewOrders: a.newOrders, Amends: a.amends, Cancels: a.cancels, Trades: a.trades,
			Messages: a.newOrders + a.amends + a.cancels, TradedQty: a.tradedQty,
			EventRefs: a.refs, RefsTruncated: a.truncated,
			LastEventID: a.lastID, LastEventTime: time.Unix(0, a.lastTS).UTC(), LastReceivedAt: a.lastRecv,
		}
		Ratios(&fw)
		out = append(out, fw)
	}
	return out
}

// Ratios fills in the two derived features from the counts.
func Ratios(fw *domain.FeatureWindow) {
	fw.CancelRate = nil
	if fw.NewOrders > 0 {
		r := float64(fw.Cancels) / float64(fw.NewOrders)
		fw.CancelRate = &r
	}
	fw.NoTrades = fw.Trades == 0
	if fw.NoTrades {
		fw.OrderTradeRatio = float64(fw.Messages)
	} else {
		fw.OrderTradeRatio = float64(fw.Messages) / float64(fw.Trades)
	}
}
