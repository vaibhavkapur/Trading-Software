package rules

import (
	"fmt"
	"time"

	"surveillance/src/domain"
)

// Detection is one rule firing, before suppression decides whether it
// becomes a new alert or is folded into an existing one.
type Detection struct {
	RuleID      string
	RuleVersion int
	Severity    string
	Symbol      string
	Accounts    []string
	DedupKey    string

	WindowStart, WindowEnd time.Time

	Thresholds map[string]float64
	Observed   map[string]float64
	EventRefs  []string
	Truncated  bool

	Explanation string

	TriggerEventID    string
	TriggerEventTime  time.Time
	TriggerReceivedAt time.Time
}

// Detectors runs every rule. It must be fed events in event-time order.
type Detectors struct {
	cfg  Config
	lay  *layering
	wash *wash
}

func NewDetectors(cfg Config) *Detectors {
	return &Detectors{cfg: cfg, lay: newLayering(), wash: newWash()}
}

// SetConfig swaps thresholds. Detector state (open orders, recent trades)
// is kept, so a reload does not blind the rules to what is in flight.
func (d *Detectors) SetConfig(cfg Config) { d.cfg = cfg }

// OnEvent feeds one ordered event to the stateful detectors.
func (d *Detectors) OnEvent(ev domain.Event) []Detection {
	var out []Detection
	d.lay.observe(ev, d.cfg.Rules.LayeringLike)
	if ev.Kind == domain.KindTrade && d.cfg.Rules.WashTradeLike.Enabled {
		out = append(out, d.wash.observe(ev, d.cfg.Rules.WashTradeLike)...)
	}
	return out
}

// OnWatermark runs checks that were waiting for event time to pass: every
// event with a timestamp before ts has been seen by now.
func (d *Detectors) OnWatermark(ts int64) []Detection {
	return d.lay.due(ts, d.cfg.Rules.LayeringLike)
}

// OnWindow evaluates the window rules on one closed feature window.
func (d *Detectors) OnWindow(fw domain.FeatureWindow) []Detection {
	var out []Detection
	if det, ok := highCancelRate(fw, d.cfg.Rules.HighCancelRate); ok {
		out = append(out, det)
	}
	if det, ok := highOrderTradeRatio(fw, d.cfg.Rules.HighOrderTradeRatio); ok {
		out = append(out, det)
	}
	return out
}

func severity(value, medium, high int64) string {
	switch {
	case value >= high:
		return domain.SeverityHigh
	case value >= medium:
		return domain.SeverityMedium
	}
	return domain.SeverityLow
}

func clock(t time.Time) string { return t.UTC().Format("15:04:05.000Z") }

func windowDetection(fw domain.FeatureWindow, rule string, version int) Detection {
	return Detection{
		RuleID: rule, RuleVersion: version,
		Symbol: fw.Symbol, Accounts: []string{fw.AccountID},
		DedupKey:    rule + "|" + fw.AccountID + "|" + fw.Symbol,
		WindowStart: fw.Start, WindowEnd: fw.End,
		EventRefs: fw.EventRefs, Truncated: fw.RefsTruncated,
		TriggerEventID: fw.LastEventID, TriggerEventTime: fw.LastEventTime, TriggerReceivedAt: fw.LastReceivedAt,
	}
}

// highCancelRate compares integer counts: cancels*100 >= pct*new_orders.
// A window exactly on the threshold fires.
func highCancelRate(fw domain.FeatureWindow, r HighCancelRateRule) (Detection, bool) {
	if !r.Enabled || fw.NewOrders < r.MinNewOrders {
		return Detection{}, false
	}
	if int64(fw.Cancels)*100 < int64(r.MinCancelRatePct)*int64(fw.NewOrders) {
		return Detection{}, false
	}
	d := windowDetection(fw, HighCancelRate, r.Version)
	rate := 100 * float64(fw.Cancels) / float64(fw.NewOrders)
	d.Severity = severity(int64(fw.NewOrders), int64(r.MediumMinNewOrders), int64(r.HighMinNewOrders))
	d.Thresholds = map[string]float64{
		"min_new_orders":        float64(r.MinNewOrders),
		"min_cancel_rate_pct":   float64(r.MinCancelRatePct),
		"medium_min_new_orders": float64(r.MediumMinNewOrders),
		"high_min_new_orders":   float64(r.HighMinNewOrders),
	}
	d.Observed = map[string]float64{
		"new_orders":      float64(fw.NewOrders),
		"cancels":         float64(fw.Cancels),
		"cancel_rate_pct": round2(rate),
		"trades":          float64(fw.Trades),
	}
	d.Explanation = fmt.Sprintf(
		"%s entered %d new orders in %s between %s and %s and sent %d cancels (%.1f%% of new orders). "+
			"The rule fires at %d%% or more with at least %d new orders in the window. "+
			"High cancellation is common in legitimate quoting; this says the pattern is worth a look, nothing more.",
		fw.AccountID, fw.NewOrders, fw.Symbol, clock(fw.Start), clock(fw.End), fw.Cancels, rate,
		r.MinCancelRatePct, r.MinNewOrders)
	return d, true
}

// highOrderTradeRatio compares messages >= ratio * max(trades, 1).
func highOrderTradeRatio(fw domain.FeatureWindow, r HighOrderTradeRatioRule) (Detection, bool) {
	if !r.Enabled || fw.Messages < r.MinMessages {
		return Detection{}, false
	}
	trades := int64(fw.Trades)
	if trades == 0 {
		trades = 1
	}
	if int64(fw.Messages) < int64(r.MinRatio)*trades {
		return Detection{}, false
	}
	d := windowDetection(fw, HighOrderTradeRatio, r.Version)
	// Severity uses the whole-number ratio, rounded down.
	d.Severity = severity(int64(fw.Messages)/trades, int64(r.MediumMinRatio), int64(r.HighMinRatio))
	d.Thresholds = map[string]float64{
		"min_messages":     float64(r.MinMessages),
		"min_ratio":        float64(r.MinRatio),
		"medium_min_ratio": float64(r.MediumMinRatio),
		"high_min_ratio":   float64(r.HighMinRatio),
	}
	d.Observed = map[string]float64{
		"messages":          float64(fw.Messages),
		"new_orders":        float64(fw.NewOrders),
		"amends":            float64(fw.Amends),
		"cancels":           float64(fw.Cancels),
		"trades":            float64(fw.Trades),
		"order_trade_ratio": round2(fw.OrderTradeRatio),
	}
	traded := fmt.Sprintf("%d trades", fw.Trades)
	if fw.Trades == 1 {
		traded = "1 trade"
	}
	if fw.Trades == 0 {
		traded = "no trades (counted as 1 for the ratio)"
	}
	d.Explanation = fmt.Sprintf(
		"%s sent %d order messages in %s between %s and %s (%d new, %d amends, %d cancels) against %s: "+
			"an order-to-trade ratio of %.1f. The rule fires at a ratio of %d or more with at least %d messages. "+
			"Many messages per trade can be ordinary re-quoting; treat this as a lead to review.",
		fw.AccountID, fw.Messages, fw.Symbol, clock(fw.Start), clock(fw.End),
		fw.NewOrders, fw.Amends, fw.Cancels, traded, fw.OrderTradeRatio, r.MinRatio, r.MinMessages)
	return d, true
}

func round2(v float64) float64 {
	if v < 0 {
		return float64(int64(v*100-0.5)) / 100
	}
	return float64(int64(v*100+0.5)) / 100
}
