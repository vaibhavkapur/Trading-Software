package rules

import (
	"fmt"
	"sort"
	"time"

	"surveillance/src/domain"
	"surveillance/src/fixed"
)

// LAYERING_LIKE, version 1.
//
// An episode starts when an account receives an execution at time e0 on
// one side (the "executed side"). The check runs once event time has
// passed e0 + cancel_within_s, and fires if all of the following hold for
// the account's orders on the OTHER side (the "layered side"):
//
//  1. at least min_layer_orders orders, entered no earlier than
//     e0 - lookback_s, were resting at e0 and have never traded;
//  2. those orders sit at min_price_levels or more distinct prices;
//  3. at least min_cancel_pct of the quantity they had resting at e0 was
//     cancelled in [e0, e0 + cancel_within_s];
//  4. that resting quantity is at least min_imbalance_ratio times what the
//     account executed on the executed side in the same interval.
//
// In words: a stack of orders on one side that never trades, a fill on
// the other side, then the stack is pulled. Waiting for the cancel window
// to close gives one detection per episode with the complete evidence,
// at the cost of cancel_within_s of extra event-time delay.
//
// A market maker who cancels and replaces a deep one-sided ladder after a
// fill produces exactly this shape. The rule cannot tell intent apart.

type layKey struct{ account, symbol string }

type layCancel struct {
	ts   int64
	qty  int64
	ref  string
	recv time.Time
}

type layOrder struct {
	id        string
	ref       string
	side      domain.Side
	price     fixed.Price
	leaves    int64
	submitted int64
	end       int64 // 0 while the order is open
	filled    int64
	cancels   []layCancel
}

type layExec struct {
	ts   int64
	side domain.Side
	qty  int64
	ref  string
	recv time.Time
}

type layState struct {
	orders []*layOrder // in submission order
	byID   map[string]*layOrder
	execs  []layExec
	// open episodes: layered side -> e0
	episode map[domain.Side]int64
}

type layPending struct {
	key  layKey
	side domain.Side // the layered side
	e0   int64
}

type layering struct {
	states  map[layKey]*layState
	pending []layPending
}

func newLayering() *layering { return &layering{states: map[layKey]*layState{}} }

func (l *layering) state(k layKey) *layState {
	st := l.states[k]
	if st == nil {
		st = &layState{byID: map[string]*layOrder{}, episode: map[domain.Side]int64{}}
		l.states[k] = st
	}
	return st
}

// observe updates order and execution state. It never fires on its own.
func (l *layering) observe(ev domain.Event, r LayeringLikeRule) {
	horizon := ev.TS() - int64(r.LookbackS+r.CancelWithinS)*int64(time.Second)
	if ev.Kind == domain.KindTrade {
		l.fill(layKey{ev.BuyAccount, ev.Symbol}, domain.Buy, ev.BuyOrderID, ev, horizon)
		l.fill(layKey{ev.SellAccount, ev.Symbol}, domain.Sell, ev.SellOrderID, ev, horizon)
		return
	}
	st := l.state(layKey{ev.AccountID, ev.Symbol})
	st.prune(horizon)
	switch ev.Action {
	case domain.ActionNew:
		if _, dup := st.byID[ev.OrderID]; dup {
			return // an order ID reused while still tracked: keep the first
		}
		o := &layOrder{id: ev.OrderID, ref: ev.ID, side: ev.Side, price: ev.Price, leaves: ev.Qty, submitted: ev.TS()}
		st.orders = append(st.orders, o)
		st.byID[o.id] = o
	case domain.ActionAmend:
		// An amend states the order's new price and open quantity.
		if o := st.byID[ev.OrderID]; o != nil && o.end == 0 {
			o.price = ev.Price
			o.leaves = ev.Qty
		}
	case domain.ActionCancel:
		// A cancel for an order we never saw (its NEW was late, or older
		// than the lookback) still counts in the window features, but
		// there is nothing here to attach it to.
		o := st.byID[ev.OrderID]
		if o == nil || o.end != 0 {
			return
		}
		q := ev.Qty
		if q <= 0 || q > o.leaves {
			q = o.leaves
		}
		o.leaves -= q
		o.cancels = append(o.cancels, layCancel{ts: ev.TS(), qty: q, ref: ev.ID, recv: ev.ReceivedAt})
		if o.leaves == 0 {
			o.end = ev.TS()
		}
	}
}

func (l *layering) fill(k layKey, side domain.Side, orderID string, ev domain.Event, horizon int64) {
	st := l.state(k)
	st.prune(horizon)
	st.execs = append(st.execs, layExec{ts: ev.TS(), side: side, qty: ev.Qty, ref: ev.ID, recv: ev.ReceivedAt})
	if o := st.byID[orderID]; o != nil && orderID != "" {
		o.filled += ev.Qty
		if o.end == 0 {
			o.leaves -= ev.Qty
			if o.leaves <= 0 {
				o.leaves = 0
				o.end = ev.TS()
			}
		}
	}
	// A fill opens an episode on the opposite side unless one is running;
	// further fills inside it belong to the same episode.
	layered := side.Opposite()
	if _, open := st.episode[layered]; !open {
		st.episode[layered] = ev.TS()
		l.pending = append(l.pending, layPending{key: k, side: layered, e0: ev.TS()})
	}
}

func (st *layState) prune(horizon int64) {
	i := 0
	for i < len(st.orders) && st.orders[i].submitted < horizon {
		delete(st.byID, st.orders[i].id)
		i++
	}
	if i > 0 {
		st.orders = append(st.orders[:0], st.orders[i:]...)
	}
	j := 0
	for j < len(st.execs) && st.execs[j].ts < horizon {
		j++
	}
	if j > 0 {
		st.execs = append(st.execs[:0], st.execs[j:]...)
	}
}

// due evaluates every episode whose cancel window closed before ts.
func (l *layering) due(ts int64, r LayeringLikeRule) []Detection {
	if len(l.pending) == 0 {
		return nil
	}
	within := int64(r.CancelWithinS) * int64(time.Second)
	var ready, keep []layPending
	for _, p := range l.pending {
		if p.e0+within < ts {
			ready = append(ready, p)
		} else {
			keep = append(keep, p)
		}
	}
	if len(ready) == 0 {
		return nil
	}
	l.pending = keep
	sort.SliceStable(ready, func(i, j int) bool {
		a, b := ready[i], ready[j]
		if a.e0 != b.e0 {
			return a.e0 < b.e0
		}
		if a.key != b.key {
			if a.key.account != b.key.account {
				return a.key.account < b.key.account
			}
			return a.key.symbol < b.key.symbol
		}
		return a.side < b.side
	})
	var out []Detection
	for _, p := range ready {
		st := l.states[p.key]
		delete(st.episode, p.side)
		if !r.Enabled {
			continue
		}
		if d, ok := st.evaluate(p, r); ok {
			out = append(out, d)
		}
	}
	return out
}

func (st *layState) evaluate(p layPending, r LayeringLikeRule) (Detection, bool) {
	e0 := p.e0
	deadline := e0 + int64(r.CancelWithinS)*int64(time.Second)
	earliest := e0 - int64(r.LookbackS)*int64(time.Second)

	var layers []*layOrder
	levels := map[fixed.Price]bool{}
	var resting, cancelled int64
	var cancels []layCancel
	for _, o := range st.orders {
		if o.side != p.side || o.submitted > e0 || o.submitted < earliest || o.filled != 0 {
			continue
		}
		if o.end != 0 && o.end < e0 {
			continue // gone before the fill
		}
		// Never filled, so what rested at e0 is what is left now plus
		// whatever was cancelled from e0 on.
		atE0 := o.leaves
		for _, c := range o.cancels {
			if c.ts >= e0 {
				atE0 += c.qty
			}
			if c.ts >= e0 && c.ts <= deadline {
				cancelled += c.qty
				cancels = append(cancels, c)
			}
		}
		if atE0 == 0 {
			continue
		}
		layers = append(layers, o)
		levels[o.price] = true
		resting += atE0
	}
	if len(layers) < r.MinLayerOrders || len(levels) < r.MinPriceLevels {
		return Detection{}, false
	}
	if cancelled*100 < int64(r.MinCancelPct)*resting {
		return Detection{}, false
	}
	executed := p.side.Opposite()
	var execQty int64
	var execs []layExec
	for _, e := range st.execs {
		if e.side == executed && e.ts >= e0 && e.ts <= deadline {
			execQty += e.qty
			execs = append(execs, e)
		}
	}
	if execQty == 0 || resting < int64(r.MinImbalanceRatio)*execQty {
		return Detection{}, false
	}

	sort.SliceStable(cancels, func(i, j int) bool { return cancels[i].ts < cancels[j].ts })
	start := layers[0].submitted
	var refs []string
	for _, o := range layers {
		refs = append(refs, o.ref)
		if o.submitted < start {
			start = o.submitted
		}
	}
	end := e0
	for _, e := range execs {
		refs = append(refs, e.ref)
		if e.ts > end {
			end = e.ts
		}
	}
	for _, c := range cancels {
		refs = append(refs, c.ref)
		if c.ts > end {
			end = c.ts
		}
	}
	// The pull is what completes the pattern, so the last cancel is the trigger.
	last := cancels[len(cancels)-1]

	imbalance := float64(resting) / float64(execQty)
	cancelPct := 100 * float64(cancelled) / float64(resting)
	d := Detection{
		RuleID: LayeringLike, RuleVersion: r.Version,
		Severity: severity(resting/execQty, int64(r.MediumMinImbalanceRatio), int64(r.HighMinImbalanceRatio)),
		Symbol:   p.key.symbol, Accounts: []string{p.key.account},
		DedupKey:    LayeringLike + "|" + p.key.account + "|" + p.key.symbol + "|" + string(p.side),
		WindowStart: time.Unix(0, start).UTC(), WindowEnd: time.Unix(0, end).UTC(),
		Thresholds: map[string]float64{
			"lookback_s":                 float64(r.LookbackS),
			"cancel_within_s":            float64(r.CancelWithinS),
			"min_layer_orders":           float64(r.MinLayerOrders),
			"min_price_levels":           float64(r.MinPriceLevels),
			"min_cancel_pct":             float64(r.MinCancelPct),
			"min_imbalance_ratio":        float64(r.MinImbalanceRatio),
			"medium_min_imbalance_ratio": float64(r.MediumMinImbalanceRatio),
			"high_min_imbalance_ratio":   float64(r.HighMinImbalanceRatio),
		},
		Observed: map[string]float64{
			"layered_orders":    float64(len(layers)),
			"price_levels":      float64(len(levels)),
			"layered_qty":       float64(resting),
			"cancelled_qty":     float64(cancelled),
			"cancel_pct":        round2(cancelPct),
			"opposite_exec_qty": float64(execQty),
			"imbalance_ratio":   round2(imbalance),
		},
		EventRefs:      refs,
		TriggerEventID: last.ref, TriggerEventTime: time.Unix(0, last.ts).UTC(), TriggerReceivedAt: last.recv,
	}
	d.Explanation = fmt.Sprintf(
		"%s had %d %s orders in %s resting at %d price levels (%d units, none of it traded) when it executed %d units on the %s side at %s. "+
			"Within %ds it cancelled %d units (%.1f%%) of those %s orders. Resting quantity was %.1fx the executed quantity. "+
			"The rule needs at least %d orders at %d or more levels, %d%% cancelled, and a %dx imbalance. "+
			"This shape also arises from ordinary cancel-and-replace quoting; it is a lead for review, not a conclusion.",
		p.key.account, len(layers), p.side, p.key.symbol, len(levels), resting, execQty, executed,
		clock(time.Unix(0, e0)), r.CancelWithinS, cancelled, cancelPct, p.side, imbalance,
		r.MinLayerOrders, r.MinPriceLevels, r.MinCancelPct, r.MinImbalanceRatio)
	return d, true
}
