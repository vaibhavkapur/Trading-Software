package metrics

import (
	"fmt"
	"sort"
	"strings"
	"sync"

	"github.com/trading-software-projects/agentic-trading-guardrails/src/pkg/types"
)

// Snapshot is a JSON-serializable summary of all guardrail telemetry.
type Snapshot struct {
	AuthorizationTotal       uint64            `json:"authorization_total"`
	AuthorizationAllow       uint64            `json:"authorization_allow"`
	AuthorizationDeny        uint64            `json:"authorization_deny"`
	AuthLatencyP50Micros     int64             `json:"auth_latency_p50_micros"`
	AuthLatencyP95Micros     int64             `json:"auth_latency_p95_micros"`
	AuthLatencyP99Micros     int64             `json:"auth_latency_p99_micros"`
	AuthLatencyMaxMicros     int64             `json:"auth_latency_max_micros"`
	DenialReasons            map[string]uint64 `json:"denial_reasons"`
	ApprovalViolations       map[string]uint64 `json:"approval_violations"`
	BrokerWritesTotal        uint64            `json:"broker_writes_total"`
	BrokerWritesViaPEP       uint64            `json:"broker_writes_via_pep"`
	BrokerWritePEPCoverage   float64           `json:"broker_write_pep_coverage_pct"`
	AuditEventsTotal         uint64            `json:"audit_events_total"`
	DecisionsAuditedTotal    uint64            `json:"decisions_audited_total"`
	AuditCompletenessPct     float64           `json:"audit_completeness_pct"`
	PolicyVersionTraceCounts map[string]uint64 `json:"policy_version_trace_counts"`
}

// Collector tracks authorization latencies, denial reasons, approval violations,
// broker write PEP coverage, and audit completeness.
type Collector struct {
	mu                 sync.RWMutex
	authTotal          uint64
	authAllow          uint64
	authDeny           uint64
	latenciesMicros    []int64
	maxLatencyMicros   int64
	denialReasons      map[string]uint64
	approvalViolations map[string]uint64
	brokerWritesTotal  uint64
	brokerWritesViaPEP uint64
	auditEventsTotal   uint64
	decisionsAudited   uint64
	policyVersions     map[string]uint64
}

func NewCollector() *Collector {
	return &Collector{
		latenciesMicros:    make([]int64, 0, 1024),
		denialReasons:      make(map[string]uint64),
		approvalViolations: make(map[string]uint64),
		policyVersions:     make(map[string]uint64),
	}
}

func (c *Collector) RecordDecision(d types.EnforcementDecision, audited bool) {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.authTotal++
	if d.Result == types.DecisionAllow {
		c.authAllow++
	} else {
		c.authDeny++
		for _, r := range d.Reasons {
			key := normalizeReasonKey(r)
			c.denialReasons[key]++
		}
	}
	if len(c.latenciesMicros) < 50_000 {
		c.latenciesMicros = append(c.latenciesMicros, d.LatencyMicros)
	}
	if d.LatencyMicros > c.maxLatencyMicros {
		c.maxLatencyMicros = d.LatencyMicros
	}
	if d.PolicyVersion != "" {
		c.policyVersions[d.PolicyVersion]++
	}
	if audited {
		c.decisionsAudited++
	}
}

func (c *Collector) RecordApprovalViolation(kind string) {
	if kind == "" {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.approvalViolations[kind]++
}

func (c *Collector) RecordBrokerWrite(viaPEP bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.brokerWritesTotal++
	if viaPEP {
		c.brokerWritesViaPEP++
	}
}

func (c *Collector) RecordAuditEvent() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.auditEventsTotal++
}

func (c *Collector) Snapshot() Snapshot {
	c.mu.RLock()
	defer c.mu.RUnlock()

	sorted := append([]int64(nil), c.latenciesMicros...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })

	pepCoverage := 100.0
	if c.brokerWritesTotal > 0 {
		pepCoverage = float64(c.brokerWritesViaPEP) * 100.0 / float64(c.brokerWritesTotal)
	}
	auditCompleteness := 100.0
	if c.authTotal > 0 {
		auditCompleteness = float64(c.decisionsAudited) * 100.0 / float64(c.authTotal)
	}

	return Snapshot{
		AuthorizationTotal:       c.authTotal,
		AuthorizationAllow:       c.authAllow,
		AuthorizationDeny:        c.authDeny,
		AuthLatencyP50Micros:     percentile(sorted, 50),
		AuthLatencyP95Micros:     percentile(sorted, 95),
		AuthLatencyP99Micros:     percentile(sorted, 99),
		AuthLatencyMaxMicros:     c.maxLatencyMicros,
		DenialReasons:            copyMap(c.denialReasons),
		ApprovalViolations:       copyMap(c.approvalViolations),
		BrokerWritesTotal:        c.brokerWritesTotal,
		BrokerWritesViaPEP:       c.brokerWritesViaPEP,
		BrokerWritePEPCoverage:   pepCoverage,
		AuditEventsTotal:         c.auditEventsTotal,
		DecisionsAuditedTotal:    c.decisionsAudited,
		AuditCompletenessPct:     auditCompleteness,
		PolicyVersionTraceCounts: copyMap(c.policyVersions),
	}
}

// PrometheusText renders metrics in standard Prometheus text exposition format.
func (c *Collector) PrometheusText() string {
	s := c.Snapshot()
	var b strings.Builder
	fmt.Fprintf(&b, "# TYPE guardrails_authorization_total counter\n")
	fmt.Fprintf(&b, "guardrails_authorization_total{result=\"ALLOW\"} %d\n", s.AuthorizationAllow)
	fmt.Fprintf(&b, "guardrails_authorization_total{result=\"DENY\"} %d\n", s.AuthorizationDeny)
	fmt.Fprintf(&b, "# TYPE guardrails_authorization_latency_micros gauge\n")
	fmt.Fprintf(&b, "guardrails_authorization_latency_micros{quantile=\"0.50\"} %d\n", s.AuthLatencyP50Micros)
	fmt.Fprintf(&b, "guardrails_authorization_latency_micros{quantile=\"0.95\"} %d\n", s.AuthLatencyP95Micros)
	fmt.Fprintf(&b, "guardrails_authorization_latency_micros{quantile=\"0.99\"} %d\n", s.AuthLatencyP99Micros)
	fmt.Fprintf(&b, "# TYPE guardrails_broker_write_pep_coverage_pct gauge\n")
	fmt.Fprintf(&b, "guardrails_broker_write_pep_coverage_pct %.2f\n", s.BrokerWritePEPCoverage)
	fmt.Fprintf(&b, "# TYPE guardrails_audit_completeness_pct gauge\n")
	fmt.Fprintf(&b, "guardrails_audit_completeness_pct %.2f\n", s.AuditCompletenessPct)
	for kind, cnt := range s.ApprovalViolations {
		fmt.Fprintf(&b, "guardrails_approval_violations_total{kind=%q} %d\n", kind, cnt)
	}
	for ver, cnt := range s.PolicyVersionTraceCounts {
		fmt.Fprintf(&b, "guardrails_policy_decisions_by_version_total{version=%q} %d\n", ver, cnt)
	}
	return b.String()
}

func percentile(sorted []int64, p int) int64 {
	if len(sorted) == 0 {
		return 0
	}
	idx := (len(sorted) - 1) * p / 100
	return sorted[idx]
}

func copyMap(m map[string]uint64) map[string]uint64 {
	cp := make(map[string]uint64, len(m))
	for k, v := range m {
		cp[k] = v
	}
	return cp
}

func normalizeReasonKey(reason string) string {
	lower := strings.ToLower(reason)
	switch {
	case strings.Contains(lower, "kill switch"):
		return "kill_switch_active"
	case strings.Contains(lower, "lacks required scope"):
		return "missing_scope"
	case strings.Contains(lower, "not authorized for account"):
		return "wrong_account"
	case strings.Contains(lower, "explicitly denied"):
		return "symbol_explicit_deny"
	case strings.Contains(lower, "not in allowlist"):
		return "symbol_not_allowlisted"
	case strings.Contains(lower, "order type") || strings.Contains(lower, "market orders"):
		return "order_type_denied"
	case strings.Contains(lower, "daily notional"):
		return "daily_notional_limit"
	case strings.Contains(lower, "order notional"):
		return "order_notional_limit"
	case strings.Contains(lower, "order quantity"):
		return "order_quantity_limit"
	case strings.Contains(lower, "trading window"):
		return "outside_trading_window"
	case strings.Contains(lower, "quote is stale"):
		return "stale_market_quote"
	case strings.Contains(lower, "ahead of") || strings.Contains(lower, "stale by"):
		return "clock_skew_or_stale_event"
	case strings.Contains(lower, "approval capability"):
		return "missing_approval"
	default:
		return "policy_denied"
	}
}
