package metrics

import (
	"fmt"
	"math"
	"sort"
	"strings"
	"sync"

	"github.com/trading-software-projects/broker-reliability-reconciliation/src/domain"
	"github.com/trading-software-projects/broker-reliability-reconciliation/src/reconcile"
)

var lateFillBuckets = []float64{1, 5, 15, 30, 60, 300, 900, 3600}

// Collector tracks operational reconciliation SLIs and renders Prometheus exposition format.
type Collector struct {
	mu              sync.RWMutex
	runsTotal       map[string]uint64
	connectorErrors map[string]uint64

	// Late fill arrival distribution histogram
	lateBucketCounts []uint64
	lateCount        uint64
	lateSum          float64
}

// NewCollector constructs a metrics collector.
func NewCollector() *Collector {
	return &Collector{
		runsTotal:        make(map[string]uint64),
		connectorErrors:  make(map[string]uint64),
		lateBucketCounts: make([]uint64, len(lateFillBuckets)),
	}
}

// IncRun records a completed or failed reconciliation run.
func (c *Collector) IncRun(status domain.RunStatus) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.runsTotal[string(status)]++
}

// IncConnectorError records a connector fetch failure by source and reason.
func (c *Collector) IncConnectorError(source, reason string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	key := source + "|" + reason
	c.connectorErrors[key]++
}

// ObserveLateFills records arrival delay samples into the late-fill histogram.
func (c *Collector) ObserveLateFills(obs []reconcile.LateFillObservation) {
	if len(obs) == 0 {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, o := range obs {
		v := o.ArrivalSeconds
		if v < 0 || math.IsNaN(v) || math.IsInf(v, 0) {
			continue
		}
		c.lateCount++
		c.lateSum += v
		for i, bound := range lateFillBuckets {
			if v <= bound {
				c.lateBucketCounts[i]++
			}
		}
	}
}

// RenderPrometheus renders all reconciliation metrics in Prometheus text exposition format (0.0.4).
func (c *Collector) RenderPrometheus(allIssues []domain.ReconIssue) string {
	var b strings.Builder

	// Compute live gauges from current issue state
	type issueKey struct {
		account  string
		kind     string
		severity string
	}
	openCounts := make(map[issueKey]int)

	type expKey struct {
		account  string
		currency string
	}
	exposureByAcctCcy := make(map[expKey]domain.Decimal)

	type driftKey struct {
		account string
		symbol  string
	}
	driftByAcctSym := make(map[driftKey]domain.Decimal)

	var mttdSum float64
	var mttdCount int
	var mttrSum float64
	var mttrCount int

	for _, iss := range allIssues {
		if iss.State != domain.IssueStateResolved && iss.State != domain.IssueStateWaived {
			openCounts[issueKey{iss.AccountID, string(iss.Kind), string(iss.Severity)}]++
			ccy := iss.Currency
			if ccy == "" {
				ccy = "USD"
			}
			ek := expKey{iss.AccountID, ccy}
			exposureByAcctCcy[ek] = exposureByAcctCcy[ek].Add(iss.MonetaryExposure)
			if iss.Symbol != "" && iss.PositionDrift.Sign() > 0 {
				dk := driftKey{iss.AccountID, iss.Symbol}
				driftByAcctSym[dk] = driftByAcctSym[dk].Add(iss.PositionDrift)
			}
		}

		// MTTD: CreatedAt minus EventTime (if present) or Cutoff
		refTime := iss.Evidence.Cutoff
		if iss.Evidence.EventTime != nil && !iss.Evidence.EventTime.IsZero() {
			refTime = *iss.Evidence.EventTime
		}
		if !refTime.IsZero() && !iss.CreatedAt.Before(refTime) {
			mttdSum += iss.CreatedAt.Sub(refTime).Seconds()
			mttdCount++
		}

		// MTTR: ResolvedAt minus CreatedAt
		if iss.Resolution != nil && !iss.Resolution.ResolvedAt.Before(iss.CreatedAt) {
			mttrSum += iss.Resolution.ResolvedAt.Sub(iss.CreatedAt).Seconds()
			mttrCount++
		}
	}

	b.WriteString("# HELP recon_unreconciled_issues Current count of open/unresolved reconciliation issues.\n")
	b.WriteString("# TYPE recon_unreconciled_issues gauge\n")
	openKeys := make([]issueKey, 0, len(openCounts))
	for k := range openCounts {
		openKeys = append(openKeys, k)
	}
	sort.Slice(openKeys, func(i, j int) bool {
		if openKeys[i].account != openKeys[j].account {
			return openKeys[i].account < openKeys[j].account
		}
		if openKeys[i].kind != openKeys[j].kind {
			return openKeys[i].kind < openKeys[j].kind
		}
		return openKeys[i].severity < openKeys[j].severity
	})
	for _, k := range openKeys {
		fmt.Fprintf(&b, "recon_unreconciled_issues{account_id=%q,kind=%q,severity=%q} %d\n",
			k.account, k.kind, k.severity, openCounts[k])
	}

	b.WriteString("# HELP recon_monetary_exposure Open unreconciled monetary exposure by account and currency.\n")
	b.WriteString("# TYPE recon_monetary_exposure gauge\n")
	expKeys := make([]expKey, 0, len(exposureByAcctCcy))
	for k := range exposureByAcctCcy {
		expKeys = append(expKeys, k)
	}
	sort.Slice(expKeys, func(i, j int) bool {
		if expKeys[i].account != expKeys[j].account {
			return expKeys[i].account < expKeys[j].account
		}
		return expKeys[i].currency < expKeys[j].currency
	})
	for _, k := range expKeys {
		fmt.Fprintf(&b, "recon_monetary_exposure{account_id=%q,currency=%q} %.4f\n",
			k.account, k.currency, exposureByAcctCcy[k].Float64ForMetrics())
	}

	b.WriteString("# HELP recon_position_drift_abs Absolute broker-vs-internal position drift by account and symbol.\n")
	b.WriteString("# TYPE recon_position_drift_abs gauge\n")
	driftKeys := make([]driftKey, 0, len(driftByAcctSym))
	for k := range driftByAcctSym {
		driftKeys = append(driftKeys, k)
	}
	sort.Slice(driftKeys, func(i, j int) bool {
		if driftKeys[i].account != driftKeys[j].account {
			return driftKeys[i].account < driftKeys[j].account
		}
		return driftKeys[i].symbol < driftKeys[j].symbol
	})
	for _, k := range driftKeys {
		fmt.Fprintf(&b, "recon_position_drift_abs{account_id=%q,symbol=%q} %.4f\n",
			k.account, k.symbol, driftByAcctSym[k].Float64ForMetrics())
	}

	mttd := 0.0
	if mttdCount > 0 {
		mttd = mttdSum / float64(mttdCount)
	}
	mttr := 0.0
	if mttrCount > 0 {
		mttr = mttrSum / float64(mttrCount)
	}
	b.WriteString("# HELP recon_mttd_seconds Mean time to detect reconciliation discrepancies in seconds.\n")
	b.WriteString("# TYPE recon_mttd_seconds gauge\n")
	fmt.Fprintf(&b, "recon_mttd_seconds %.4f\n", mttd)

	b.WriteString("# HELP recon_mttr_seconds Mean time to resolve reconciliation discrepancies in seconds.\n")
	b.WriteString("# TYPE recon_mttr_seconds gauge\n")
	fmt.Fprintf(&b, "recon_mttr_seconds %.4f\n", mttr)

	c.mu.RLock()
	defer c.mu.RUnlock()

	b.WriteString("# HELP recon_late_fill_arrival_seconds Distribution of broker fill arrival delay (receive_time - event_time).\n")
	b.WriteString("# TYPE recon_late_fill_arrival_seconds histogram\n")
	for i, bound := range lateFillBuckets {
		fmt.Fprintf(&b, "recon_late_fill_arrival_seconds_bucket{le=\"%.0f\"} %d\n", bound, c.lateBucketCounts[i])
	}
	fmt.Fprintf(&b, "recon_late_fill_arrival_seconds_bucket{le=\"+Inf\"} %d\n", c.lateCount)
	fmt.Fprintf(&b, "recon_late_fill_arrival_seconds_sum %.4f\n", c.lateSum)
	fmt.Fprintf(&b, "recon_late_fill_arrival_seconds_count %d\n", c.lateCount)

	b.WriteString("# HELP recon_runs_total Total number of reconciliation runs by status.\n")
	b.WriteString("# TYPE recon_runs_total counter\n")
	runKeys := make([]string, 0, len(c.runsTotal))
	for k := range c.runsTotal {
		runKeys = append(runKeys, k)
	}
	sort.Strings(runKeys)
	for _, k := range runKeys {
		fmt.Fprintf(&b, "recon_runs_total{status=%q} %d\n", k, c.runsTotal[k])
	}

	b.WriteString("# HELP recon_connector_errors_total Total connector errors by source and reason.\n")
	b.WriteString("# TYPE recon_connector_errors_total counter\n")
	errKeys := make([]string, 0, len(c.connectorErrors))
	for k := range c.connectorErrors {
		errKeys = append(errKeys, k)
	}
	sort.Strings(errKeys)
	for _, k := range errKeys {
		parts := strings.SplitN(k, "|", 2)
		fmt.Fprintf(&b, "recon_connector_errors_total{source=%q,reason=%q} %d\n", parts[0], parts[1], c.connectorErrors[k])
	}

	return b.String()
}
