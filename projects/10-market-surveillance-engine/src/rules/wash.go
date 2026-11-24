package rules

import (
	"fmt"
	"math/big"
	"sort"
	"strings"
	"time"

	"surveillance/src/domain"
	"surveillance/src/fixed"
)

// WASH_TRADE_LIKE, version 1. Two patterns, both over trades in one symbol
// within lookback_s, both keyed by beneficial owner (an account with no
// owner in the reference data is its own owner):
//
// SELF: the same owner is on both sides of at least min_self_trades
// trades. One self-match never fires: the ruleset refuses a minimum
// below 2, because a single one is routinely an accident.
//
// PAIR: two different owners A and B traded at least min_pair_trades
// times, in both directions, such that
//   - the smaller of (A bought from B, B bought from A) is at least
//     min_offset_pct of the larger, so positions end up roughly where
//     they started, and
//   - the volume-weighted prices of the two directions are within
//     max_price_diff_bps of each other, so little money changed hands.
//
// Two busy participants can meet this by chance, more easily the fewer
// participants there are. The price comparison is done in exact integer
// arithmetic.

type washTrade struct {
	ts       int64
	ref      string
	recv     time.Time
	qty      int64
	price    fixed.Price
	buyOwner string
	buyAcct  string
	sellAcct string
}

type washKey struct{ a, b, symbol string }

type wash struct {
	self map[washKey][]washTrade
	pair map[washKey][]washTrade
}

func newWash() *wash {
	return &wash{self: map[washKey][]washTrade{}, pair: map[washKey][]washTrade{}}
}

func trim(ts []washTrade, horizon int64) []washTrade {
	i := 0
	for i < len(ts) && ts[i].ts < horizon {
		i++
	}
	if i == 0 {
		return ts
	}
	return append(ts[:0], ts[i:]...)
}

func (w *wash) observe(ev domain.Event, r WashTradeLikeRule) []Detection {
	t := washTrade{ts: ev.TS(), ref: ev.ID, recv: ev.ReceivedAt, qty: ev.Qty, price: ev.Price,
		buyOwner: ev.BuyOwner, buyAcct: ev.BuyAccount, sellAcct: ev.SellAccount}
	horizon := ev.TS() - int64(r.LookbackS)*int64(time.Second)

	if ev.BuyOwner == ev.SellOwner {
		k := washKey{a: ev.BuyOwner, symbol: ev.Symbol}
		list := append(trim(w.self[k], horizon), t)
		w.self[k] = list
		if len(list) < r.MinSelfTrades {
			return nil
		}
		return []Detection{selfDetection(k, list, r)}
	}

	k := washKey{a: ev.BuyOwner, b: ev.SellOwner, symbol: ev.Symbol}
	if k.b < k.a {
		k.a, k.b = k.b, k.a
	}
	list := append(trim(w.pair[k], horizon), t)
	w.pair[k] = list
	if len(list) < r.MinPairTrades {
		return nil
	}
	if d, ok := pairDetection(k, list, r); ok {
		return []Detection{d}
	}
	return nil
}

func washBase(k washKey, list []washTrade, r WashTradeLikeRule, variant string) Detection {
	accounts := map[string]bool{}
	refs := make([]string, 0, len(list))
	for _, t := range list {
		accounts[t.buyAcct] = true
		accounts[t.sellAcct] = true
		refs = append(refs, t.ref)
	}
	names := make([]string, 0, len(accounts))
	for a := range accounts {
		names = append(names, a)
	}
	sort.Strings(names)
	last := list[len(list)-1]
	key := WashTradeLike + "|" + variant + "|" + k.a
	if k.b != "" {
		key += "|" + k.b
	}
	return Detection{
		RuleID: WashTradeLike, RuleVersion: r.Version,
		Severity: severity(int64(len(list)), int64(r.MediumMinTrades), int64(r.HighMinTrades)),
		Symbol:   k.symbol, Accounts: names, DedupKey: key + "|" + k.symbol,
		WindowStart: time.Unix(0, list[0].ts).UTC(), WindowEnd: time.Unix(0, last.ts).UTC(),
		EventRefs:      refs,
		TriggerEventID: last.ref, TriggerEventTime: time.Unix(0, last.ts).UTC(), TriggerReceivedAt: last.recv,
	}
}

func selfDetection(k washKey, list []washTrade, r WashTradeLikeRule) Detection {
	var qty int64
	for _, t := range list {
		qty += t.qty
	}
	d := washBase(k, list, r, "SELF")
	d.Thresholds = map[string]float64{
		"lookback_s":        float64(r.LookbackS),
		"min_self_trades":   float64(r.MinSelfTrades),
		"medium_min_trades": float64(r.MediumMinTrades),
		"high_min_trades":   float64(r.HighMinTrades),
	}
	d.Observed = map[string]float64{"self_trades": float64(len(list)), "self_traded_qty": float64(qty)}
	who := "Account " + d.Accounts[0] + " was"
	if len(d.Accounts) > 1 {
		who = fmt.Sprintf("Accounts %s, which share the beneficial owner %s, were", strings.Join(d.Accounts, " and "), k.a)
	}
	d.Explanation = fmt.Sprintf(
		"%s on both the buy and the sell side of %d trades in %s (%d units) between %s and %s. "+
			"The rule fires at %d or more such trades within %ds. Self-matching can be accidental "+
			"(for example two strategies of one firm crossing); this is a lead for review, not a conclusion.",
		who, len(list), k.symbol, qty, clock(d.WindowStart), clock(d.WindowEnd), r.MinSelfTrades, r.LookbackS)
	return d
}

func pairDetection(k washKey, list []washTrade, r WashTradeLikeRule) (Detection, bool) {
	// Direction 1: owner a buys from owner b. Direction 2: the reverse.
	var q1, q2 int64
	n1, n2 := new(big.Int), new(big.Int) // sum of price*qty per direction
	for _, t := range list {
		pq := new(big.Int).Mul(big.NewInt(int64(t.price)), big.NewInt(t.qty))
		if t.buyOwner == k.a {
			q1 += t.qty
			n1.Add(n1, pq)
		} else {
			q2 += t.qty
			n2.Add(n2, pq)
		}
	}
	if q1 == 0 || q2 == 0 {
		return Detection{}, false
	}
	lo, hi := q1, q2
	if lo > hi {
		lo, hi = hi, lo
	}
	if lo*100 < int64(r.MinOffsetPct)*hi {
		return Detection{}, false
	}
	// VWAPs are n1/q1 and n2/q2. Compare them without dividing:
	// |n1*q2 - n2*q1| * 10000 <= bps * min(n1*q2, n2*q1).
	x := new(big.Int).Mul(n1, big.NewInt(q2))
	y := new(big.Int).Mul(n2, big.NewInt(q1))
	diff := new(big.Int).Sub(x, y)
	diff.Abs(diff)
	base := x
	if y.Cmp(x) < 0 {
		base = y
	}
	lhs := new(big.Int).Mul(diff, big.NewInt(10000))
	rhs := new(big.Int).Mul(base, big.NewInt(int64(r.MaxPriceDiffBps)))
	if lhs.Cmp(rhs) > 0 {
		return Detection{}, false
	}

	offset := 100 * float64(lo) / float64(hi)
	bps, _ := new(big.Rat).SetFrac(lhs, base).Float64()
	d := washBase(k, list, r, "PAIR")
	d.Thresholds = map[string]float64{
		"lookback_s":         float64(r.LookbackS),
		"min_pair_trades":    float64(r.MinPairTrades),
		"min_offset_pct":     float64(r.MinOffsetPct),
		"max_price_diff_bps": float64(r.MaxPriceDiffBps),
		"medium_min_trades":  float64(r.MediumMinTrades),
		"high_min_trades":    float64(r.HighMinTrades),
	}
	d.Observed = map[string]float64{
		"pair_trades":    float64(len(list)),
		"qty_a_buys":     float64(q1),
		"qty_b_buys":     float64(q2),
		"offset_pct":     round2(offset),
		"price_diff_bps": round2(bps),
	}
	d.Explanation = fmt.Sprintf(
		"%s and %s traded %s with each other %d times between %s and %s: %s bought %d units from %s and %s bought %d units back "+
			"(%.1f%% offsetting) at average prices %.2f bps apart. The rule fires at %d or more trades, %d%% offsetting and prices within %d bps. "+
			"Two active participants can meet repeatedly by chance; this is a lead for review, not a conclusion.",
		k.a, k.b, k.symbol, len(list), clock(d.WindowStart), clock(d.WindowEnd),
		k.a, q1, k.b, k.b, q2, offset, bps, r.MinPairTrades, r.MinOffsetPct, r.MaxPriceDiffBps)
	return d, true
}
