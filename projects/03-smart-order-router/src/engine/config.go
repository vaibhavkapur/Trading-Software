package engine

import (
	"sor/src/marketview"
	"sor/src/planner"
	"sor/src/risk"
	"sor/src/venuesim"
)

type RouterCfg struct {
	ChildTimeoutMs     int64 `json:"child_timeout_ms"`
	MaxReplansPerSlice int   `json:"max_replans_per_slice"`
	ReplanDelayMs      int64 `json:"replan_delay_ms"`
	DefaultIntervalMs  int64 `json:"default_interval_ms"` // TWAP slice interval
}

// Config fully determines a simulation: same Config + same inputs at the same
// simulated times = same outputs.
type Config struct {
	Symbol        string            `json:"symbol"`
	Seed          uint64            `json:"seed"`
	StartMs       int64             `json:"start_ms"` // simulated epoch, Unix ms UTC
	StepMs        int64             `json:"step_ms"`  // venue book/quote update period
	StartMidTicks int64             `json:"start_mid_ticks"`
	MovePct       int               `json:"move_pct"` // % chance the mid moves one tick per step
	LotSize       int64             `json:"lot_size"`
	Venues        []venuesim.Config `json:"venues"`
	Policy        planner.Policy    `json:"policy"`
	Risk          risk.Limits       `json:"risk"`
	Router        RouterCfg         `json:"router"`
	View          marketview.Config `json:"market_view"`
}

func def[T comparable](v *T, d T) {
	var zero T
	if *v == zero {
		*v = d
	}
}

// WithDefaults fills every unset field so scenario files can stay small.
func (c Config) WithDefaults() Config {
	def(&c.Symbol, "XYZ")
	def(&c.Seed, 1)
	def(&c.StartMs, 1767623400000) // 2026-01-05T14:30:00Z
	def(&c.StepMs, 100)
	def(&c.StartMidTicks, 10000)
	def(&c.MovePct, 10)
	def(&c.LotSize, 100)
	if len(c.Venues) == 0 {
		c.Venues = []venuesim.Config{
			{Name: "ALPHA", TakerFeeMills: 30, LatencyMs: 5, BaseDepth: 1000},
			{Name: "BRAVO", TakerFeeMills: 10, LatencyMs: 25, BaseDepth: 700},
			{Name: "CHARLIE", TakerFeeMills: -5, LatencyMs: 60, BaseDepth: 400},
		}
	}
	c.Venues = append([]venuesim.Config(nil), c.Venues...)
	for i := range c.Venues {
		v := &c.Venues[i]
		def(&v.HalfSpreadTicks, 1)
		def(&v.Levels, 5)
		def(&v.BaseDepth, 800)
		def(&v.RefillPct, 20)
		def(&v.NoiseTicks, 1)
	}
	def(&c.Policy.Version, planner.DefaultVersion)
	def(&c.Policy.ParticipationPct, 30)
	def(&c.Policy.MaxChildQty, 2000)
	def(&c.Policy.LatencyPenaltyMillsPer10Ms, 1)
	def(&c.Policy.QualityPenaltyMills, 50)
	c.Policy.LotSize = c.LotSize
	if len(c.Risk.Symbols) == 0 {
		c.Risk.Symbols = []string{c.Symbol}
	}
	def(&c.Risk.MaxParentQty, 1_000_000)
	def(&c.Risk.MaxParentNotionalMills, 100_000_000_000_000)
	def(&c.Risk.MaxChildQty, 5000)
	def(&c.Risk.PriceCollarBps, 50)
	def(&c.Risk.MaxDurationSeconds, 8*3600)
	c.Risk.LotSize = c.LotSize
	def(&c.Router.ChildTimeoutMs, 1000)
	def(&c.Router.MaxReplansPerSlice, 2)
	def(&c.Router.ReplanDelayMs, 300)
	def(&c.Router.DefaultIntervalMs, 10_000)
	def(&c.View.StaleMs, 1000)
	def(&c.View.SkewToleranceMs, 250)
	def(&c.View.BreakerThreshold, 3)
	def(&c.View.BreakerCooldownMs, 15_000)
	return c
}
