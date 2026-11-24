// Package simulator is a pair of simulated venues. It generates synthetic
// trades, quotes and depth from a seeded random walk and serves them in the
// alpha and beta wire formats, with fault injection for tests and demos.
//
// Everything it emits is simulated. It has no connection to any real market.
package simulator

import (
	"math/rand/v2"
	"time"
)

// Level is one depth level in ticks and lots.
type Level struct{ Ticks, Lots int64 }

// Tick is one synthetic market event, before any venue encoding.
type Tick struct {
	Sym       int // index into the venue's instrument list
	Kind      string
	EventTime time.Time

	// trade
	PriceTicks   int64
	Lots         int64
	BuyAggressor bool
	TradeNo      uint64

	// quote
	BidTicks, AskTicks int64
	BidLots, AskLots   int64

	// depth
	Bids, Asks []Level
}

// Market is a deterministic generator: the same seed yields the same ticks.
type Market struct {
	rng     *rand.Rand
	mids    []int64
	tradeNo uint64
	depth   bool
}

// NewMarket creates a generator for n symbols. PCG's output for a given seed
// is fixed by the Go 1 compatibility promise, so fixtures stay reproducible.
func NewMarket(seed uint64, n int, depth bool) *Market {
	m := &Market{rng: rand.New(rand.NewPCG(seed, 0x6d6470)), mids: make([]int64, n), depth: depth}
	for i := range m.mids {
		m.mids[i] = 10000 + int64(i)*2500
	}
	return m
}

// Rand exposes the generator's random source so callers that need more
// deterministic randomness (timing, latency) draw from the same stream.
func (m *Market) Rand() *rand.Rand { return m.rng }

// Next produces the next event, stamped with the given venue time.
func (m *Market) Next(now time.Time) Tick {
	i := m.rng.IntN(len(m.mids))
	m.mids[i] += int64(m.rng.IntN(3)) - 1
	if m.mids[i] < 100 {
		m.mids[i] = 100
	}
	spread := int64(1 + m.rng.IntN(3))
	bid, ask := m.mids[i]-spread, m.mids[i]+spread
	t := Tick{Sym: i, EventTime: now}
	switch r := m.rng.Float64(); {
	case r < 0.30:
		t.Kind = "trade"
		t.BuyAggressor = m.rng.IntN(2) == 0
		t.PriceTicks = bid
		if t.BuyAggressor {
			t.PriceTicks = ask
		}
		t.Lots = int64(1+m.rng.IntN(20)) * 10
		m.tradeNo++
		t.TradeNo = m.tradeNo
	case m.depth && r < 0.40:
		t.Kind = "depth"
		for k := int64(0); k < 5; k++ {
			t.Bids = append(t.Bids, Level{bid - k, int64(1+m.rng.IntN(50)) * 10})
			t.Asks = append(t.Asks, Level{ask + k, int64(1+m.rng.IntN(50)) * 10})
		}
	default:
		t.Kind = "quote"
		t.BidTicks, t.AskTicks = bid, ask
		t.BidLots, t.AskLots = int64(1+m.rng.IntN(50))*10, int64(1+m.rng.IntN(50))*10
	}
	return t
}
