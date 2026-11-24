// Package venuesim simulates trading venues under controlled seeds.
//
// Each venue quotes a ladder around a shared seeded mid-price path. The price
// path and generated depth never depend on our own order flow (orders only
// deplete displayed depth temporarily), so two runs with the same seed see an
// identical market regardless of routing policy. That is what makes the TCA
// baseline comparison fair.
package venuesim

import (
	"fmt"
	"hash/fnv"
	"math/rand/v2"

	"sor/src/domain"
)

type Config struct {
	Name            string `json:"name"`
	TakerFeeMills   int64  `json:"taker_fee_mills"` // per share; negative = rebate
	LatencyMs       int64  `json:"latency_ms"`      // one-way
	HalfSpreadTicks int64  `json:"half_spread_ticks"`
	Levels          int    `json:"levels"`
	BaseDepth       int64  `json:"base_depth"`  // shares per level before noise
	RefillPct       int64  `json:"refill_pct"`  // % of depleted depth restored per step
	NoiseTicks      int64  `json:"noise_ticks"` // max |venue mid - shared mid|
}

// Fault is the injectable failure state of a venue (POST /sim/venues/{venue}/faults).
type Fault struct {
	LatencyMs     int64   `json:"latency_ms"`     // added to one-way latency
	RejectRate    float64 `json:"reject_rate"`    // probability an order is rejected
	Down          bool    `json:"down"`           // link lost: no quotes, no reports, sends fail
	StopQuotes    bool    `json:"stop_quotes"`    // market data goes silent, orders still work
	ClockSkewMs   int64   `json:"clock_skew_ms"`  // added to venue event timestamps
	DuplicateRate float64 `json:"duplicate_rate"` // probability a report is delivered twice
}

// MidPath is the seeded random walk all venues share.
type MidPath struct {
	rng     *rand.Rand
	mids    []int64
	movePct int
}

func NewMidPath(seed uint64, startTicks int64, movePct int) *MidPath {
	return &MidPath{rng: rand.New(rand.NewPCG(seed, 0x5eed)), mids: []int64{startTicks}, movePct: movePct}
}

// At returns the mid at step k, extending the path deterministically.
func (m *MidPath) At(k int) int64 {
	for len(m.mids) <= k {
		last := m.mids[len(m.mids)-1]
		r := m.rng.IntN(200)
		if r < m.movePct {
			last++
		} else if r < 2*m.movePct {
			last--
		}
		if last < 100 {
			last = 100
		}
		m.mids = append(m.mids, last)
	}
	return m.mids[k]
}

type Order struct {
	ChildID string
	Side    domain.Side
	Qty     int64
	LimitPx int64
}

type Venue struct {
	cfg      Config
	symbol   string
	lot      int64
	path     *MidPath
	bookRng  *rand.Rand
	ordRng   *rand.Rand
	seq      uint64
	fillSeq  int
	bids     []domain.Level
	asks     []domain.Level
	consumed [2]map[int64]int64 // [0]=bids [1]=asks, keyed by price
	fault    Fault
	results  map[string]domain.ExecReport
}

func New(cfg Config, symbol string, lot int64, seed uint64, path *MidPath) *Venue {
	h := fnv.New64a()
	h.Write([]byte(cfg.Name))
	n := h.Sum64()
	return &Venue{
		cfg: cfg, symbol: symbol, lot: lot, path: path,
		bookRng:  rand.New(rand.NewPCG(seed, n)),
		ordRng:   rand.New(rand.NewPCG(seed^0x9e3779b97f4a7c15, n)),
		consumed: [2]map[int64]int64{{}, {}},
		results:  map[string]domain.ExecReport{},
	}
}

func (v *Venue) Name() string     { return v.cfg.Name }
func (v *Venue) Config() Config   { return v.cfg }
func (v *Venue) Fault() Fault     { return v.fault }
func (v *Venue) SetFault(f Fault) { v.fault = f }
func (v *Venue) Latency() int64   { return v.cfg.LatencyMs + v.fault.LatencyMs }

// Step regenerates the book for step k and returns the simulated market
// volume and price traded at this venue during the step (the public tape).
// It always draws the same number of random values, whatever our orders did.
func (v *Venue) Step(k int) (vol, px int64) {
	mid := v.path.At(k)
	if n := v.cfg.NoiseTicks; n > 0 {
		mid += v.bookRng.Int64N(2*n+1) - n
	}
	v.bids, v.asks = v.bids[:0], v.asks[:0]
	lots := max(v.cfg.BaseDepth/v.lot, 1)
	for i := 0; i < v.cfg.Levels; i++ {
		bq := max(lots*(50+v.bookRng.Int64N(101))/100, 1) * v.lot
		aq := max(lots*(50+v.bookRng.Int64N(101))/100, 1) * v.lot
		v.bids = append(v.bids, domain.Level{Px: mid - v.cfg.HalfSpreadTicks - int64(i), Qty: bq})
		v.asks = append(v.asks, domain.Level{Px: mid + v.cfg.HalfSpreadTicks + int64(i), Qty: aq})
	}
	for _, c := range v.consumed {
		for p, q := range c {
			q -= max(q*v.cfg.RefillPct/100, v.lot)
			if q <= 0 {
				delete(c, p)
			} else {
				c[p] = q
			}
		}
	}
	return v.bookRng.Int64N(lots+1) * v.lot, mid
}

func displayed(levels []domain.Level, consumed map[int64]int64) []domain.Level {
	out := make([]domain.Level, 0, len(levels))
	for _, l := range levels {
		if q := l.Qty - consumed[l.Px]; q > 0 {
			out = append(out, domain.Level{Px: l.Px, Qty: q})
		}
	}
	return out
}

// Quote publishes the current displayed book, stamped with venue event time.
func (v *Venue) Quote(nowMs int64) domain.VenueQuote {
	v.seq++
	return domain.VenueQuote{
		Venue: v.cfg.Name, Symbol: v.symbol, Seq: v.seq,
		Bids: displayed(v.bids, v.consumed[0]), Asks: displayed(v.asks, v.consumed[1]),
		AsOfMs: nowMs + v.fault.ClockSkewMs,
	}
}

// Submit matches an IOC order against the current book. dup reports whether
// the venue will (faultily) deliver the report twice. Resubmitting a known
// child id is idempotent.
func (v *Venue) Submit(nowMs int64, o Order) (rep domain.ExecReport, dup bool) {
	if r, ok := v.results[o.ChildID]; ok {
		return r, false
	}
	rejDraw, dupDraw := v.ordRng.Float64(), v.ordRng.Float64()
	rep = domain.ExecReport{ChildID: o.ChildID, Venue: v.cfg.Name, Status: domain.ReportDone,
		Fills: []domain.VenueFill{}, VenueMs: nowMs + v.fault.ClockSkewMs}
	if rejDraw < v.fault.RejectRate {
		rep.Status, rep.Reason = domain.ReportRejected, "VENUE_REJECT"
	} else {
		levels, side := v.asks, 1
		if o.Side == domain.Sell {
			levels, side = v.bids, 0
		}
		rem := o.Qty
		for _, l := range levels {
			if rem == 0 || o.Side.Sign()*(l.Px-o.LimitPx) > 0 {
				break
			}
			take := min(l.Qty-v.consumed[side][l.Px], rem)
			if take <= 0 {
				continue
			}
			v.consumed[side][l.Px] += take
			rem -= take
			v.fillSeq++
			rep.Fills = append(rep.Fills, domain.VenueFill{
				ID: fmt.Sprintf("%s-F%06d", v.cfg.Name, v.fillSeq), Qty: take, Px: l.Px,
				FeeMills: take * v.cfg.TakerFeeMills,
			})
		}
	}
	v.results[o.ChildID] = rep
	return rep, dupDraw < v.fault.DuplicateRate
}

// Result returns the stored terminal report for a child (order-status query,
// used to reconcile after a reconnect).
func (v *Venue) Result(childID string) (domain.ExecReport, bool) {
	r, ok := v.results[childID]
	return r, ok
}
