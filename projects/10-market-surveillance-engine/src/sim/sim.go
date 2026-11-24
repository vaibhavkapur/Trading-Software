// Package sim generates synthetic order flow. Everything it produces is
// simulated: no real accounts, venues or prices.
//
// Scenarios are labelled by what the generator set out to imitate, not by
// whether a rule fires on them:
//
//   - positive scenarios imitate a pattern of interest. "clear" ones are
//     generated well inside the rule thresholds; "borderline" ones are
//     deliberately weak (fewer orders, slower cancels, partial offsets)
//     and are expected to be missed some of the time;
//   - negative scenarios are benign. "ordinary" ones are plain two-way
//     flow; "hard" ones are legitimate behaviour that resembles a pattern
//     (a market maker who cancels and replaces, two firms that keep
//     meeting) and are expected to raise false positives some of the time.
//
// A scenario is a pure function of its name and parameters: the same seed
// always yields the same events, on any machine and any Go version,
// because the generator below is self-contained.
package sim

import (
	"fmt"
	"sort"
	"time"

	"surveillance/src/domain"
	"surveillance/src/fixed"
	"surveillance/src/normalize"
	"surveillance/src/rules"
)

const (
	Positive = "positive"
	Negative = "negative"

	Clear      = "clear"
	Borderline = "borderline"
	Ordinary   = "ordinary"
	Hard       = "hard"
)

// Names lists every scenario, positives first.
var Names = []string{
	"layering_like", "wash_trade_like", "high_cancel_rate", "quote_flicker_like",
	"normal_flow", "benign_market_maker", "benign_cross_trades",
}

type Params struct {
	Seed   uint64
	Start  time.Time
	Symbol string
	Venue  string
	// Tag prefixes every account and ID, keeping scenarios apart.
	Tag string
	// Borderline asks a positive scenario for its weak form.
	Borderline bool
}

type Scenario struct {
	Name      string    `json:"name"`
	Label     string    `json:"label"`
	Intensity string    `json:"intensity"`
	Variant   string    `json:"variant,omitempty"`
	Expected  []string  `json:"expected_rules"`
	Tolerated []string  `json:"tolerated_rules"`
	Accounts  []string  `json:"accounts"`
	Symbol    string    `json:"symbol"`
	Start     time.Time `json:"start"`
	End       time.Time `json:"end"`
	Seed      uint64    `json:"seed"`

	Events []normalize.Raw `json:"-"`
}

// rng is splitmix64: tiny, well distributed and fixed for all time.
type rng struct{ s uint64 }

func (r *rng) next() uint64 {
	r.s += 0x9e3779b97f4a7c15
	z := r.s
	z = (z ^ (z >> 30)) * 0xbf58476d1ce4e5b9
	z = (z ^ (z >> 27)) * 0x94d049bb133111eb
	return z ^ (z >> 31)
}

// between returns a value in [lo, hi].
func (r *rng) between(lo, hi int) int { return lo + int(r.next()%uint64(hi-lo+1)) }
func (r *rng) chance(pct int) bool    { return r.between(1, 100) <= pct }
func (r *rng) side() domain.Side {
	if r.chance(50) {
		return domain.Buy
	}
	return domain.Sell
}

const tick = fixed.Price(100) // 0.01

type builder struct {
	r        *rng
	p        Params
	t        time.Time
	n        int
	base     fixed.Price
	events   []normalize.Raw
	accounts []string
}

func ms(n int) time.Duration { return time.Duration(n) * time.Millisecond }

// step moves the scenario clock forward by a random amount and returns it.
func (b *builder) step(lo, hi int) time.Time {
	b.t = b.t.Add(ms(b.r.between(lo, hi)))
	return b.t
}

func (b *builder) acct(suffix string) string {
	name := b.p.Tag + "-" + suffix
	for _, a := range b.accounts {
		if a == name {
			return name
		}
	}
	b.accounts = append(b.accounts, name)
	return name
}

func (b *builder) id(kind string) string {
	b.n++
	return fmt.Sprintf("%s-%s%d", b.p.Tag, kind, b.n)
}

func stamp(t time.Time) string { return t.UTC().Format("2006-01-02T15:04:05.000Z") }

func (b *builder) order(acct, orderID string, action domain.Action, side domain.Side, qty int64, price fixed.Price, at time.Time) {
	r := normalize.Raw{Type: "order", EventID: b.id("E"), Venue: b.p.Venue, Symbol: b.p.Symbol, TS: stamp(at),
		AccountID: acct, OrderID: orderID, Action: string(action), Side: string(side)}
	if qty > 0 {
		r.Qty = normalize.Lit(fmt.Sprint(qty))
	}
	if price > 0 {
		r.Price = normalize.Lit(price.String())
	}
	b.events = append(b.events, r)
}

func (b *builder) newOrder(acct string, side domain.Side, qty int64, price fixed.Price, at time.Time) string {
	id := b.id("O")
	b.order(acct, id, domain.ActionNew, side, qty, price, at)
	return id
}

func (b *builder) cancel(acct, orderID string, side domain.Side, at time.Time) {
	b.order(acct, orderID, domain.ActionCancel, side, 0, 0, at)
}

func (b *builder) trade(buyAcct, buyOrder, sellAcct, sellOrder string, qty int64, price fixed.Price, at time.Time) {
	b.events = append(b.events, normalize.Raw{Type: "trade", Venue: b.p.Venue, Symbol: b.p.Symbol, TS: stamp(at),
		TradeID: b.id("T"), BuyAccount: buyAcct, SellAccount: sellAcct, BuyOrderID: buyOrder, SellOrderID: sellOrder,
		Qty: normalize.Lit(fmt.Sprint(qty)), Price: normalize.Lit(price.String())})
}

// hit has taker enter an order that trades against a resting one.
func (b *builder) hit(restAcct, restOrder string, restSide domain.Side, taker string, qty int64, price fixed.Price, at time.Time) {
	takerOrder := b.newOrder(taker, restSide.Opposite(), qty, price, at.Add(-ms(1)))
	if restSide == domain.Buy {
		b.trade(restAcct, restOrder, taker, takerOrder, qty, price, at)
	} else {
		b.trade(taker, takerOrder, restAcct, restOrder, qty, price, at)
	}
}

// Generate builds one scenario.
func Generate(name string, p Params) (Scenario, error) {
	if p.Venue == "" {
		p.Venue = "XSIM"
	}
	if p.Symbol == "" {
		p.Symbol = "SIMA"
	}
	if p.Tag == "" {
		p.Tag = fmt.Sprintf("S%d", p.Seed)
	}
	if p.Start.IsZero() {
		return Scenario{}, fmt.Errorf("scenario start time is required")
	}
	r := &rng{s: p.Seed}
	b := &builder{r: r, p: p, t: p.Start.UTC().Truncate(time.Millisecond)}
	b.base = fixed.Price(r.between(2000, 20000)) * tick
	sc := Scenario{Name: name, Symbol: p.Symbol, Start: b.t, Seed: p.Seed, Expected: []string{}, Tolerated: []string{}}

	positive := func(expected string, tolerated ...string) {
		sc.Label, sc.Intensity = Positive, Clear
		if p.Borderline {
			sc.Intensity = Borderline
		}
		sc.Expected = []string{expected}
		sc.Tolerated = append(sc.Tolerated, tolerated...)
	}
	switch name {
	case "layering_like":
		// The pattern is itself a burst of orders that are then cancelled,
		// so the two window rules firing as well is consistent with it.
		positive(rules.LayeringLike, rules.HighCancelRate, rules.HighOrderTradeRatio)
		b.layering()
	case "wash_trade_like":
		positive(rules.WashTradeLike)
		sc.Variant = b.wash()
	case "high_cancel_rate":
		positive(rules.HighCancelRate, rules.HighOrderTradeRatio)
		b.highCancel()
	case "quote_flicker_like":
		positive(rules.HighOrderTradeRatio)
		b.flicker()
	case "normal_flow":
		sc.Label, sc.Intensity = Negative, Ordinary
		b.normalFlow()
	case "benign_market_maker":
		sc.Label = Negative
		sc.Variant, sc.Intensity = b.marketMaker()
	case "benign_cross_trades":
		sc.Label, sc.Intensity = Negative, Hard
		b.crossTrades()
	default:
		return Scenario{}, fmt.Errorf("unknown scenario %q (known: %v)", name, Names)
	}

	// Order by event time; generation order breaks ties.
	sort.SliceStable(b.events, func(i, j int) bool { return b.events[i].TS < b.events[j].TS })
	sc.Events, sc.Accounts = b.events, b.accounts
	sc.End = sc.Start
	if n := len(b.events); n > 0 {
		sc.End, _ = normalize.ParseTime(b.events[n-1].TS)
	}
	return sc, nil
}

// layering: stack one side, get a small fill on the other, pull the stack.
func (b *builder) layering() {
	r := b.r
	trader, other := b.acct("LAY"), b.acct("CP")
	for cycle, cycles := 0, r.between(1, 3); cycle < cycles; cycle++ {
		side := r.side()
		layers, qty := r.between(4, 7), int64(r.between(8, 20))*100
		if b.p.Borderline {
			layers, qty = r.between(2, 4), int64(r.between(2, 6))*100
		}
		genuine := int64(r.between(1, 3)) * 100

		at := b.step(1000, 4000)
		ids := make([]string, layers)
		for i := range ids {
			price := b.base + fixed.Price(i+2)*tick
			if side == domain.Buy {
				price = b.base - fixed.Price(i+2)*tick
			}
			ids[i] = b.newOrder(trader, side, qty, price, at)
			at = at.Add(ms(r.between(150, 400)))
		}
		exec := at.Add(ms(r.between(500, 2000)))
		resting := b.newOrder(trader, side.Opposite(), genuine, b.base, exec.Add(-ms(r.between(100, 300))))
		b.hit(trader, resting, side.Opposite(), other, genuine, b.base, exec)

		// Clear: everything is pulled within about three seconds.
		// Borderline: only part is pulled, or it is pulled slowly.
		pull, delay := layers, r.between(300, 3000)
		if b.p.Borderline {
			switch r.between(1, 3) {
			case 1:
				pull = (layers + 1) / 2
			case 2:
				delay = r.between(11000, 14000)
			}
		}
		at = exec.Add(ms(delay))
		for i := 0; i < pull; i++ {
			b.cancel(trader, ids[i], side, at)
			at = at.Add(ms(r.between(50, 200)))
		}
		b.t = at.Add(ms(r.between(5000, 12000)))
	}
}

// wash: one account on both sides, or two accounts passing a position
// back and forth.
func (b *builder) wash() string {
	r := b.r
	if r.chance(50) {
		acct := b.acct("WSH")
		n := r.between(3, 6)
		if b.p.Borderline {
			n = r.between(1, 2)
		}
		for i := 0; i < n; i++ {
			at := b.step(800, 4000)
			qty := int64(r.between(1, 5)) * 100
			price := b.base + fixed.Price(r.between(-2, 2))*tick
			buy := b.newOrder(acct, domain.Buy, qty, price, at.Add(-ms(80)))
			sell := b.newOrder(acct, domain.Sell, qty, price, at.Add(-ms(40)))
			b.trade(acct, buy, acct, sell, qty, price, at)
		}
		return "self"
	}

	a, c := b.acct("WA"), b.acct("WB")
	trips, mismatch := r.between(2, 5), false
	if b.p.Borderline {
		if r.chance(50) {
			trips = 1
		} else {
			trips, mismatch = r.between(2, 3), true
		}
	}
	leg := func(buyer, seller string, qty int64, price fixed.Price, at time.Time) {
		buy := b.newOrder(buyer, domain.Buy, qty, price, at.Add(-ms(60)))
		sell := b.newOrder(seller, domain.Sell, qty, price, at.Add(-ms(30)))
		b.trade(buyer, buy, seller, sell, qty, price, at)
	}
	for i := 0; i < trips; i++ {
		qty := int64(r.between(2, 10)) * 100
		price := b.base + fixed.Price(r.between(-2, 2))*tick
		leg(a, c, qty, price, b.step(1500, 6000))
		back := qty
		if mismatch {
			back = qty * int64(r.between(55, 85)) / 1000 * 10
		}
		leg(c, a, back, price+fixed.Price(r.between(0, 1))*tick, b.step(500, 5000))
	}
	return "paired"
}

// highCancel: a burst of orders, nearly all cancelled within half a second.
func (b *builder) highCancel() {
	r := b.r
	trader, other := b.acct("QS"), b.acct("CP")
	n, cancelPct := r.between(30, 120), r.between(93, 100)
	if b.p.Borderline {
		if r.chance(50) {
			n = r.between(12, 19) // too few orders
		} else {
			n, cancelPct = r.between(25, 60), r.between(70, 88) // too many kept
		}
	}
	trades := r.between(0, 2)
	for i := 0; i < n; i++ {
		at := b.step(150, 400)
		side := r.side()
		price := b.base + fixed.Price(r.between(-3, 3))*tick
		qty := int64(r.between(1, 5)) * 100
		id := b.newOrder(trader, side, qty, price, at)
		switch {
		case r.chance(cancelPct):
			b.cancel(trader, id, side, at.Add(ms(r.between(50, 500))))
		case trades > 0:
			trades--
			b.hit(trader, id, side, other, qty, price, at.Add(ms(r.between(200, 900))))
		}
	}
}

// flicker: a few orders, each re-priced many times, almost nothing trades.
func (b *builder) flicker() {
	r := b.r
	trader, other := b.acct("FLK"), b.acct("CP")
	orders, amends := r.between(8, 14), r.between(6, 10)
	if b.p.Borderline {
		orders, amends = r.between(3, 5), r.between(3, 6)
	}
	traded := r.chance(50)
	for i := 0; i < orders; i++ {
		side := r.side()
		qty := int64(r.between(1, 5)) * 100
		price := b.base + fixed.Price(r.between(-3, 3))*tick
		id := b.newOrder(trader, side, qty, price, b.step(100, 300))
		for j := 0; j < amends; j++ {
			price = b.base + fixed.Price(r.between(-3, 3))*tick
			b.order(trader, id, domain.ActionAmend, side, qty, price, b.step(100, 300))
		}
		if traded && i == 0 {
			b.hit(trader, id, side, other, qty, price, b.step(100, 300))
		}
	}
}

// normalFlow: a crowd of accounts placing orders; most trade, some are
// amended, some are cancelled, some just rest.
func (b *builder) normalFlow() {
	r := b.r
	accts := make([]string, r.between(8, 12))
	for i := range accts {
		accts[i] = b.acct(fmt.Sprintf("N%d", i+1))
	}
	for i, n := 0, r.between(30, 80); i < n; i++ {
		at := b.step(300, 1200)
		who := r.between(0, len(accts)-1)
		side := r.side()
		qty := int64(r.between(1, 5)) * 100
		price := b.base + fixed.Price(r.between(-3, 3))*tick
		id := b.newOrder(accts[who], side, qty, price, at)
		switch roll := r.between(1, 100); {
		case roll <= 55:
			taker := (who + r.between(1, len(accts)-1)) % len(accts)
			b.hit(accts[who], id, side, accts[taker], qty, price, at.Add(ms(r.between(100, 1500))))
		case roll <= 70:
			b.order(accts[who], id, domain.ActionAmend, side, qty, price+tick, at.Add(ms(r.between(500, 3000))))
		case roll <= 88:
			b.cancel(accts[who], id, side, at.Add(ms(r.between(1000, 5000))))
		}
	}
}

// marketMaker: two-sided quotes at several levels; after each fill the
// quotes move. A maker who amends is unremarkable. A maker who cancels
// and replaces the whole ladder is legitimate too, but looks much more
// like the patterns the rules look for.
func (b *builder) marketMaker() (variant, intensity string) {
	r := b.r
	maker := b.acct("MM")
	takers := []string{b.acct("T1"), b.acct("T2"), b.acct("T3")}
	levels, size := r.between(2, 4), int64(r.between(1, 3))*100
	replace := r.chance(40)
	variant, intensity = "amend", Ordinary
	if replace {
		variant, intensity = "cancel_replace", Hard
	}

	type quote struct {
		id    string
		side  domain.Side
		level int
	}
	mid := b.base
	price := func(q quote) fixed.Price {
		if q.side == domain.Buy {
			return mid - fixed.Price(q.level)*tick
		}
		return mid + fixed.Price(q.level)*tick
	}
	var quotes []quote
	place := func(at time.Time) {
		quotes = quotes[:0]
		for _, side := range []domain.Side{domain.Buy, domain.Sell} {
			for lvl := 1; lvl <= levels; lvl++ {
				q := quote{side: side, level: lvl}
				q.id = b.newOrder(maker, side, size, price(q), at)
				quotes = append(quotes, q)
				at = at.Add(ms(10))
			}
		}
	}
	place(b.step(500, 1500))

	for cycle, cycles := 0, r.between(3, 8); cycle < cycles; cycle++ {
		at := b.step(2000, 6000)
		// A taker trades with the best quote on one side.
		side := r.side()
		best := -1
		for i, q := range quotes {
			if q.side == side && q.level == 1 {
				best = i
			}
		}
		filled := quotes[best]
		b.hit(maker, filled.id, side, takers[r.between(0, 2)], size, price(filled), at)
		quotes = append(quotes[:best], quotes[best+1:]...)

		if side == domain.Buy {
			mid -= tick
		} else {
			mid += tick
		}
		at = at.Add(ms(r.between(200, 800)))
		if replace {
			for _, q := range quotes {
				b.cancel(maker, q.id, q.side, at)
				at = at.Add(ms(10))
			}
			place(at.Add(ms(50)))
		} else {
			for _, q := range quotes {
				b.order(maker, q.id, domain.ActionAmend, q.side, size, price(q), at)
				at = at.Add(ms(10))
			}
			q := quote{side: side, level: 1}
			q.id = b.newOrder(maker, side, size, price(q), at)
			quotes = append(quotes, q)
		}
		b.t = at
	}
	at := b.step(2000, 4000)
	for _, q := range quotes {
		b.cancel(maker, q.id, q.side, at)
		at = at.Add(ms(10))
	}
	return variant, intensity
}

// crossTrades: two firms that keep trading with each other in both
// directions, in unrelated sizes, while the price drifts.
func (b *builder) crossTrades() {
	r := b.r
	a, c := b.acct("XA"), b.acct("XB")
	price := b.base
	for i, n := 0, r.between(4, 8); i < n; i++ {
		at := b.step(2000, 15000)
		price += fixed.Price(r.between(-3, 3)) * tick
		qty := int64(r.between(1, 9)) * 100
		buyer, seller := a, c
		if r.chance(50) {
			buyer, seller = c, a
		}
		buy := b.newOrder(buyer, domain.Buy, qty, price, at.Add(-ms(60)))
		sell := b.newOrder(seller, domain.Sell, qty, price, at.Add(-ms(30)))
		b.trade(buyer, buy, seller, sell, qty, price, at)
	}
}
