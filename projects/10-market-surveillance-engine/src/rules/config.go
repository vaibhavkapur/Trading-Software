// Package rules holds the versioned detectors and alert suppression.
//
// Every threshold lives in one ruleset file. An alert records the ruleset
// version and hash it was raised under plus the thresholds of its rule, so
// a reviewer can always tell what "suspicious" meant at the time.
package rules

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"time"
)

const (
	HighCancelRate      = "HIGH_CANCEL_RATE"
	HighOrderTradeRatio = "HIGH_ORDER_TRADE_RATIO"
	LayeringLike        = "LAYERING_LIKE"
	WashTradeLike       = "WASH_TRADE_LIKE"
)

// RuleIDs lists every detector, in reporting order.
var RuleIDs = []string{HighCancelRate, HighOrderTradeRatio, LayeringLike, WashTradeLike}

type Config struct {
	Version     string      `json:"version"`
	Window      Window      `json:"window"`
	Suppression Suppression `json:"suppression"`
	Rules       RuleSet     `json:"rules"`
}

type Window struct {
	SizeS             int `json:"size_s"`
	HopS              int `json:"hop_s"`
	AllowedLatenessMs int `json:"allowed_lateness_ms"`
	// MaxFutureSkewMs bounds how far ahead of the local clock an event
	// time may be. One event stamped an hour ahead would otherwise drag
	// the watermark forward and make an hour of honest events late.
	MaxFutureSkewMs int `json:"max_future_skew_ms"`
	// IdleTimeoutMs is how long the stream may be silent before the
	// watermark starts following the local clock instead of new events.
	IdleTimeoutMs int `json:"idle_timeout_ms"`
}

func (w Window) Size() time.Duration { return time.Duration(w.SizeS) * time.Second }
func (w Window) Hop() time.Duration  { return time.Duration(w.HopS) * time.Second }
func (w Window) Lateness() time.Duration {
	return time.Duration(w.AllowedLatenessMs) * time.Millisecond
}
func (w Window) FutureSkew() time.Duration {
	return time.Duration(w.MaxFutureSkewMs) * time.Millisecond
}
func (w Window) IdleTimeout() time.Duration {
	return time.Duration(w.IdleTimeoutMs) * time.Millisecond
}

type Suppression struct {
	// CooldownS: a repeat detection with the same dedup key is folded into
	// the existing open alert if its evidence ends within this many
	// seconds (event time) of that alert's evidence.
	CooldownS       int `json:"cooldown_s"`
	MaxEvidenceRefs int `json:"max_evidence_refs"`
}

type RuleSet struct {
	HighCancelRate      HighCancelRateRule      `json:"HIGH_CANCEL_RATE"`
	HighOrderTradeRatio HighOrderTradeRatioRule `json:"HIGH_ORDER_TRADE_RATIO"`
	LayeringLike        LayeringLikeRule        `json:"LAYERING_LIKE"`
	WashTradeLike       WashTradeLikeRule       `json:"WASH_TRADE_LIKE"`
}

// HighCancelRateRule fires on a window where an account entered at least
// MinNewOrders orders and cancels >= MinCancelRatePct% of new orders.
type HighCancelRateRule struct {
	Version            int  `json:"version"`
	Enabled            bool `json:"enabled"`
	MinNewOrders       int  `json:"min_new_orders"`
	MinCancelRatePct   int  `json:"min_cancel_rate_pct"`
	MediumMinNewOrders int  `json:"medium_min_new_orders"`
	HighMinNewOrders   int  `json:"high_min_new_orders"`
}

// HighOrderTradeRatioRule fires on a window with at least MinMessages
// order messages and messages >= MinRatio x trades (trades counted as 1
// when there are none).
type HighOrderTradeRatioRule struct {
	Version        int  `json:"version"`
	Enabled        bool `json:"enabled"`
	MinMessages    int  `json:"min_messages"`
	MinRatio       int  `json:"min_ratio"`
	MediumMinRatio int  `json:"medium_min_ratio"`
	HighMinRatio   int  `json:"high_min_ratio"`
}

// LayeringLikeRule: see layering.go for the exact pattern.
type LayeringLikeRule struct {
	Version                 int  `json:"version"`
	Enabled                 bool `json:"enabled"`
	LookbackS               int  `json:"lookback_s"`
	CancelWithinS           int  `json:"cancel_within_s"`
	MinLayerOrders          int  `json:"min_layer_orders"`
	MinPriceLevels          int  `json:"min_price_levels"`
	MinCancelPct            int  `json:"min_cancel_pct"`
	MinImbalanceRatio       int  `json:"min_imbalance_ratio"`
	MediumMinImbalanceRatio int  `json:"medium_min_imbalance_ratio"`
	HighMinImbalanceRatio   int  `json:"high_min_imbalance_ratio"`
}

// WashTradeLikeRule: see wash.go for the exact pattern.
type WashTradeLikeRule struct {
	Version         int  `json:"version"`
	Enabled         bool `json:"enabled"`
	LookbackS       int  `json:"lookback_s"`
	MinSelfTrades   int  `json:"min_self_trades"`
	MinPairTrades   int  `json:"min_pair_trades"`
	MinOffsetPct    int  `json:"min_offset_pct"`
	MaxPriceDiffBps int  `json:"max_price_diff_bps"`
	MediumMinTrades int  `json:"medium_min_trades"`
	HighMinTrades   int  `json:"high_min_trades"`
}

// Parse reads a ruleset strictly. Unknown fields and out-of-range values
// fail the whole file: a typo must not silently disable a threshold.
func Parse(b []byte) (Config, error) {
	var c Config
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&c); err != nil {
		return c, fmt.Errorf("ruleset: %w", err)
	}
	if dec.More() {
		return c, fmt.Errorf("ruleset: trailing data")
	}
	if err := c.Validate(); err != nil {
		return c, fmt.Errorf("ruleset: %w", err)
	}
	return c, nil
}

func Load(path string) (Config, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return Config{}, fmt.Errorf("ruleset: %w", err)
	}
	return Parse(b)
}

type check struct {
	name   string
	v      int
	lo, hi int
}

func inRange(cs ...check) error {
	for _, c := range cs {
		if c.v < c.lo || c.v > c.hi {
			return fmt.Errorf("%s = %d is outside [%d, %d]", c.name, c.v, c.lo, c.hi)
		}
	}
	return nil
}

const limit = 1_000_000_000

func (c Config) Validate() error {
	if c.Version == "" {
		return fmt.Errorf("version is required")
	}
	w := c.Window
	if err := inRange(
		check{"window.size_s", w.SizeS, 1, 86400},
		check{"window.hop_s", w.HopS, 1, 86400},
		check{"window.allowed_lateness_ms", w.AllowedLatenessMs, 0, 3_600_000},
		check{"window.max_future_skew_ms", w.MaxFutureSkewMs, 0, 3_600_000},
		check{"window.idle_timeout_ms", w.IdleTimeoutMs, 1, 3_600_000},
		check{"suppression.cooldown_s", c.Suppression.CooldownS, 0, 7 * 86400},
		check{"suppression.max_evidence_refs", c.Suppression.MaxEvidenceRefs, 10, 100_000},
	); err != nil {
		return err
	}
	if w.SizeS%w.HopS != 0 {
		return fmt.Errorf("window.size_s (%d) must be a multiple of window.hop_s (%d)", w.SizeS, w.HopS)
	}
	if w.SizeS/w.HopS > 360 {
		return fmt.Errorf("window.size_s / window.hop_s must not exceed 360")
	}

	r := c.Rules
	hc := r.HighCancelRate
	if err := inRange(
		check{"HIGH_CANCEL_RATE.version", hc.Version, 1, limit},
		// No rule may be configured to fire on a single order or trade.
		check{"HIGH_CANCEL_RATE.min_new_orders", hc.MinNewOrders, 2, limit},
		check{"HIGH_CANCEL_RATE.min_cancel_rate_pct", hc.MinCancelRatePct, 1, 10000},
		check{"HIGH_CANCEL_RATE.medium_min_new_orders", hc.MediumMinNewOrders, hc.MinNewOrders, limit},
		check{"HIGH_CANCEL_RATE.high_min_new_orders", hc.HighMinNewOrders, hc.MediumMinNewOrders, limit},
	); err != nil {
		return err
	}
	ot := r.HighOrderTradeRatio
	if err := inRange(
		check{"HIGH_ORDER_TRADE_RATIO.version", ot.Version, 1, limit},
		check{"HIGH_ORDER_TRADE_RATIO.min_messages", ot.MinMessages, 2, limit},
		check{"HIGH_ORDER_TRADE_RATIO.min_ratio", ot.MinRatio, 1, limit},
		check{"HIGH_ORDER_TRADE_RATIO.medium_min_ratio", ot.MediumMinRatio, ot.MinRatio, limit},
		check{"HIGH_ORDER_TRADE_RATIO.high_min_ratio", ot.HighMinRatio, ot.MediumMinRatio, limit},
	); err != nil {
		return err
	}
	ll := r.LayeringLike
	if err := inRange(
		check{"LAYERING_LIKE.version", ll.Version, 1, limit},
		check{"LAYERING_LIKE.lookback_s", ll.LookbackS, 1, 86400},
		check{"LAYERING_LIKE.cancel_within_s", ll.CancelWithinS, 1, 86400},
		check{"LAYERING_LIKE.min_layer_orders", ll.MinLayerOrders, 2, 10000},
		check{"LAYERING_LIKE.min_price_levels", ll.MinPriceLevels, 1, ll.MinLayerOrders},
		check{"LAYERING_LIKE.min_cancel_pct", ll.MinCancelPct, 1, 100},
		check{"LAYERING_LIKE.min_imbalance_ratio", ll.MinImbalanceRatio, 1, limit},
		check{"LAYERING_LIKE.medium_min_imbalance_ratio", ll.MediumMinImbalanceRatio, ll.MinImbalanceRatio, limit},
		check{"LAYERING_LIKE.high_min_imbalance_ratio", ll.HighMinImbalanceRatio, ll.MediumMinImbalanceRatio, limit},
	); err != nil {
		return err
	}
	wt := r.WashTradeLike
	if err := inRange(
		check{"WASH_TRADE_LIKE.version", wt.Version, 1, limit},
		check{"WASH_TRADE_LIKE.lookback_s", wt.LookbackS, 1, 86400},
		// One trade is never enough: a single self-match is routinely an accident.
		check{"WASH_TRADE_LIKE.min_self_trades", wt.MinSelfTrades, 2, limit},
		check{"WASH_TRADE_LIKE.min_pair_trades", wt.MinPairTrades, 2, limit},
		check{"WASH_TRADE_LIKE.min_offset_pct", wt.MinOffsetPct, 1, 100},
		check{"WASH_TRADE_LIKE.max_price_diff_bps", wt.MaxPriceDiffBps, 0, 10000},
		check{"WASH_TRADE_LIKE.medium_min_trades", wt.MediumMinTrades, 2, limit},
		check{"WASH_TRADE_LIKE.high_min_trades", wt.HighMinTrades, wt.MediumMinTrades, limit},
	); err != nil {
		return err
	}
	return nil
}

// Canonical is the ruleset re-encoded from the parsed struct, so the hash
// depends on the values and not on whitespace or key order in the file.
func (c Config) Canonical() []byte {
	b, err := json.Marshal(c)
	if err != nil {
		panic(err) // a struct of ints, bools and strings always encodes
	}
	return b
}

func (c Config) Hash() string {
	h := sha256.Sum256(c.Canonical())
	return hex.EncodeToString(h[:])
}
