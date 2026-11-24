// Package metrics tracks operational and strategy-health telemetry for the arbitrage monitor
// and exposes both JSON snapshots and Prometheus exposition text.
package metrics

import (
	"fmt"
	"sort"
	"strings"
	"sync"

	"arbmonitor/src/domain"
	"arbmonitor/src/fixed"
)

// EdgeSummary summarizes observed basis-point values using fixed-point arithmetic.
type EdgeSummary struct {
	Count int64         `json:"count"`
	Min   fixed.Decimal `json:"min_bps"`
	Max   fixed.Decimal `json:"max_bps"`
	Avg   fixed.Decimal `json:"avg_bps"`
}

// NotionalSummary summarizes estimated executable notional across opportunities.
type NotionalSummary struct {
	Count int64         `json:"count"`
	Total fixed.Decimal `json:"total_quote"`
	Max   fixed.Decimal `json:"max_quote"`
	Avg   fixed.Decimal `json:"avg_quote"`
}

// Snapshot is a point-in-time JSON-serializable view of all monitor metrics.
type Snapshot struct {
	EvaluationsTotal      int64                              `json:"evaluations_total"`
	OpportunitiesObserved int64                              `json:"opportunities_observed"`
	AlertsEmitted         int64                              `json:"alerts_emitted"`
	AlertsExpired         int64                              `json:"alerts_expired"`
	StoreWriteErrors      int64                              `json:"store_write_errors"`
	GrossEdgeBps          EdgeSummary                        `json:"gross_edge_bps"`
	NetEdgeBps            EdgeSummary                        `json:"net_edge_bps"`
	ExecutableNotional    NotionalSummary                    `json:"executable_notional"`
	AvgAlertStalenessMs   int64                              `json:"avg_alert_staleness_ms"`
	MaxAlertStalenessMs   int64                              `json:"max_alert_staleness_ms"`
	AvgAlertLifetimeMs    int64                              `json:"avg_alert_lifetime_ms"`
	NetEdgeHistogram      map[string]int64                   `json:"net_edge_bps_histogram"`
	Suppressions          map[domain.SuppressionReason]int64 `json:"suppressions"`
}

// Collector aggregates metrics safely across concurrent feed and API goroutines.
type Collector struct {
	mu                    sync.Mutex
	evaluationsTotal      int64
	opportunitiesObserved int64
	alertsEmitted         int64
	alertsExpired         int64
	storeWriteErrors      int64

	grossCount int64
	grossSum   fixed.Decimal
	grossMin   fixed.Decimal
	grossMax   fixed.Decimal

	netCount int64
	netSum   fixed.Decimal
	netMin   fixed.Decimal
	netMax   fixed.Decimal

	notionalCount int64
	notionalSum   fixed.Decimal
	notionalMax   fixed.Decimal

	stalenessSumMs int64
	stalenessMaxMs int64
	lifetimeSumMs  int64
	lifetimeCount  int64

	netBuckets   map[string]int64
	suppressions map[domain.SuppressionReason]int64
}

// New creates an initialized metrics Collector.
func New() *Collector {
	return &Collector{
		netBuckets: map[string]int64{
			"lt_0":     0,
			"0_to_5":   0,
			"5_to_10":  0,
			"10_to_25": 0,
			"25_to_50": 0,
			"gte_50":   0,
		},
		suppressions: make(map[domain.SuppressionReason]int64),
	}
}

// RecordEvaluation increments the pair evaluation counter.
func (c *Collector) RecordEvaluation() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.evaluationsTotal++
}

// RecordSuppression increments the suppression counter for a specific reason.
func (c *Collector) RecordSuppression(reason domain.SuppressionReason) {
	if reason == domain.SuppressNone {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.suppressions[reason]++
}

// RecordOpportunity records gross/net edge bps and executable notional for an evaluated opportunity.
func (c *Collector) RecordOpportunity(opp domain.Opportunity) {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.opportunitiesObserved++

	c.grossCount++
	c.grossSum = c.grossSum.Add(opp.GrossEdgeBps)
	if c.grossCount == 1 || opp.GrossEdgeBps.LessThan(c.grossMin) {
		c.grossMin = opp.GrossEdgeBps
	}
	if c.grossCount == 1 || opp.GrossEdgeBps.GreaterThan(c.grossMax) {
		c.grossMax = opp.GrossEdgeBps
	}

	c.netCount++
	c.netSum = c.netSum.Add(opp.NetEdgeBps)
	if c.netCount == 1 || opp.NetEdgeBps.LessThan(c.netMin) {
		c.netMin = opp.NetEdgeBps
	}
	if c.netCount == 1 || opp.NetEdgeBps.GreaterThan(c.netMax) {
		c.netMax = opp.NetEdgeBps
	}

	switch {
	case opp.NetEdgeBps.IsNegative():
		c.netBuckets["lt_0"]++
	case opp.NetEdgeBps.LessThan(fixed.FromInt(5)):
		c.netBuckets["0_to_5"]++
	case opp.NetEdgeBps.LessThan(fixed.FromInt(10)):
		c.netBuckets["5_to_10"]++
	case opp.NetEdgeBps.LessThan(fixed.FromInt(25)):
		c.netBuckets["10_to_25"]++
	case opp.NetEdgeBps.LessThan(fixed.FromInt(50)):
		c.netBuckets["25_to_50"]++
	default:
		c.netBuckets["gte_50"]++
	}

	c.notionalCount++
	c.notionalSum = c.notionalSum.Add(opp.ExecutableNotional)
	if c.notionalCount == 1 || opp.ExecutableNotional.GreaterThan(c.notionalMax) {
		c.notionalMax = opp.ExecutableNotional
	}
}

// RecordAlertEmitted records an emitted alert and its staleness (max leg age at trigger time).
func (c *Collector) RecordAlertEmitted(alert domain.Alert) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.alertsEmitted++
	staleness := alert.Opportunity.BuyAgeMs
	if alert.Opportunity.SellAgeMs > staleness {
		staleness = alert.Opportunity.SellAgeMs
	}
	if staleness < 0 {
		staleness = 0
	}
	c.stalenessSumMs += staleness
	if staleness > c.stalenessMaxMs {
		c.stalenessMaxMs = staleness
	}
}

// RecordAlertExpired records an expired alert and its active lifetime in milliseconds.
func (c *Collector) RecordAlertExpired(lifetimeMs int64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.alertsExpired++
	if lifetimeMs < 0 {
		lifetimeMs = 0
	}
	c.lifetimeSumMs += lifetimeMs
	c.lifetimeCount++
}

// RecordStoreWriteError increments the failed persistence counter.
func (c *Collector) RecordStoreWriteError() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.storeWriteErrors++
}

// Snapshot returns a consistent copy of all metrics.
func (c *Collector) Snapshot() Snapshot {
	c.mu.Lock()
	defer c.mu.Unlock()

	grossAvg := fixed.Zero
	if c.grossCount > 0 {
		grossAvg, _ = c.grossSum.Div(fixed.FromInt(c.grossCount))
	}
	netAvg := fixed.Zero
	if c.netCount > 0 {
		netAvg, _ = c.netSum.Div(fixed.FromInt(c.netCount))
	}
	notionalAvg := fixed.Zero
	if c.notionalCount > 0 {
		notionalAvg, _ = c.notionalSum.Div(fixed.FromInt(c.notionalCount))
	}

	var avgStale int64
	if c.alertsEmitted > 0 {
		avgStale = c.stalenessSumMs / c.alertsEmitted
	}
	var avgLife int64
	if c.lifetimeCount > 0 {
		avgLife = c.lifetimeSumMs / c.lifetimeCount
	}

	buckets := make(map[string]int64, len(c.netBuckets))
	for k, v := range c.netBuckets {
		buckets[k] = v
	}
	supps := make(map[domain.SuppressionReason]int64, len(c.suppressions))
	for k, v := range c.suppressions {
		supps[k] = v
	}

	return Snapshot{
		EvaluationsTotal:      c.evaluationsTotal,
		OpportunitiesObserved: c.opportunitiesObserved,
		AlertsEmitted:         c.alertsEmitted,
		AlertsExpired:         c.alertsExpired,
		StoreWriteErrors:      c.storeWriteErrors,
		GrossEdgeBps: EdgeSummary{
			Count: c.grossCount,
			Min:   c.grossMin,
			Max:   c.grossMax,
			Avg:   grossAvg,
		},
		NetEdgeBps: EdgeSummary{
			Count: c.netCount,
			Min:   c.netMin,
			Max:   c.netMax,
			Avg:   netAvg,
		},
		ExecutableNotional: NotionalSummary{
			Count: c.notionalCount,
			Total: c.notionalSum,
			Max:   c.notionalMax,
			Avg:   notionalAvg,
		},
		AvgAlertStalenessMs: avgStale,
		MaxAlertStalenessMs: c.stalenessMaxMs,
		AvgAlertLifetimeMs:  avgLife,
		NetEdgeHistogram:    buckets,
		Suppressions:        supps,
	}
}

// PrometheusText formats metrics and venue connectivity counters in Prometheus text format.
func (c *Collector) PrometheusText(venues []domain.VenueHealth) string {
	snap := c.Snapshot()
	var b strings.Builder

	b.WriteString("# HELP arb_evaluations_total Total cross-venue pair evaluations.\n")
	b.WriteString("# TYPE arb_evaluations_total counter\n")
	fmt.Fprintf(&b, "arb_evaluations_total %d\n", snap.EvaluationsTotal)

	b.WriteString("# HELP arb_opportunities_observed_total Total crossed opportunities evaluated.\n")
	b.WriteString("# TYPE arb_opportunities_observed_total counter\n")
	fmt.Fprintf(&b, "arb_opportunities_observed_total %d\n", snap.OpportunitiesObserved)

	b.WriteString("# HELP arb_alerts_emitted_total Total paper arbitrage alerts emitted.\n")
	b.WriteString("# TYPE arb_alerts_emitted_total counter\n")
	fmt.Fprintf(&b, "arb_alerts_emitted_total %d\n", snap.AlertsEmitted)

	b.WriteString("# HELP arb_alerts_expired_total Total alerts transitioned to expired.\n")
	b.WriteString("# TYPE arb_alerts_expired_total counter\n")
	fmt.Fprintf(&b, "arb_alerts_expired_total %d\n", snap.AlertsExpired)

	b.WriteString("# HELP arb_store_write_errors_total Total failed alert persistence writes.\n")
	b.WriteString("# TYPE arb_store_write_errors_total counter\n")
	fmt.Fprintf(&b, "arb_store_write_errors_total %d\n", snap.StoreWriteErrors)

	b.WriteString("# HELP arb_gross_edge_bps_avg Average observed gross edge in basis points.\n")
	b.WriteString("# TYPE arb_gross_edge_bps_avg gauge\n")
	fmt.Fprintf(&b, "arb_gross_edge_bps_avg %s\n", snap.GrossEdgeBps.Avg.FixedString(4))

	b.WriteString("# HELP arb_net_edge_bps_avg Average observed net edge in basis points.\n")
	b.WriteString("# TYPE arb_net_edge_bps_avg gauge\n")
	fmt.Fprintf(&b, "arb_net_edge_bps_avg %s\n", snap.NetEdgeBps.Avg.FixedString(4))

	b.WriteString("# HELP arb_executable_notional_avg Average estimated executable notional in quote currency.\n")
	b.WriteString("# TYPE arb_executable_notional_avg gauge\n")
	fmt.Fprintf(&b, "arb_executable_notional_avg %s\n", snap.ExecutableNotional.Avg.FixedString(2))

	b.WriteString("# HELP arb_alert_staleness_ms_avg Average quote age at alert trigger time in ms.\n")
	b.WriteString("# TYPE arb_alert_staleness_ms_avg gauge\n")
	fmt.Fprintf(&b, "arb_alert_staleness_ms_avg %d\n", snap.AvgAlertStalenessMs)

	b.WriteString("# HELP arb_alert_lifetime_ms_avg Average alert lifetime in ms.\n")
	b.WriteString("# TYPE arb_alert_lifetime_ms_avg gauge\n")
	fmt.Fprintf(&b, "arb_alert_lifetime_ms_avg %d\n", snap.AvgAlertLifetimeMs)

	b.WriteString("# HELP arb_suppressions_total Suppressed evaluations by reason.\n")
	b.WriteString("# TYPE arb_suppressions_total counter\n")
	reasons := make([]string, 0, len(snap.Suppressions))
	for r := range snap.Suppressions {
		reasons = append(reasons, string(r))
	}
	sort.Strings(reasons)
	for _, r := range reasons {
		fmt.Fprintf(&b, "arb_suppressions_total{reason=%q} %d\n", r, snap.Suppressions[domain.SuppressionReason(r)])
	}

	b.WriteString("# HELP arb_venue_disconnects_total Feed disconnect count per venue.\n")
	b.WriteString("# TYPE arb_venue_disconnects_total counter\n")
	b.WriteString("# HELP arb_venue_reconnects_total Feed recovery/reconnect count per venue.\n")
	b.WriteString("# TYPE arb_venue_reconnects_total counter\n")
	b.WriteString("# HELP arb_venue_sequence_gaps_total Feed sequence gap count per venue.\n")
	b.WriteString("# TYPE arb_venue_sequence_gaps_total counter\n")
	b.WriteString("# HELP arb_venue_quote_age_ms Latest quote age in ms per venue.\n")
	b.WriteString("# TYPE arb_venue_quote_age_ms gauge\n")
	for _, v := range venues {
		fmt.Fprintf(&b, "arb_venue_disconnects_total{venue=%q} %d\n", v.Venue, v.DisconnectCount)
		fmt.Fprintf(&b, "arb_venue_reconnects_total{venue=%q} %d\n", v.Venue, v.ReconnectCount)
		fmt.Fprintf(&b, "arb_venue_sequence_gaps_total{venue=%q} %d\n", v.Venue, v.SequenceGaps)
		fmt.Fprintf(&b, "arb_venue_quote_age_ms{venue=%q} %d\n", v.Venue, v.QuoteAgeMs)
	}

	return b.String()
}
