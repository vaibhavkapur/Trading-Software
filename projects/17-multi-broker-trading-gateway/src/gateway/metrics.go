package gateway

import "sync/atomic"

// MetricsSnapshot captures point-in-time gateway operational telemetry.
type MetricsSnapshot struct {
	OrdersSubmitted           int64 `json:"orders_submitted"`
	OrdersAccepted            int64 `json:"orders_accepted"`
	OrdersFilled              int64 `json:"orders_filled"`
	OrdersCancelled           int64 `json:"orders_cancelled"`
	OrdersRejectedPreDispatch int64 `json:"orders_rejected_pre_dispatch"`
	OrdersRejectedByBroker    int64 `json:"orders_rejected_by_broker"`
	IdempotentHits            int64 `json:"idempotent_hits"`
	RateLimitRejections       int64 `json:"rate_limit_rejections"`
	AuthFailures              int64 `json:"auth_failures"`
	UnknownOutcomes           int64 `json:"unknown_outcomes"`
	CancelTimeouts            int64 `json:"cancel_timeouts"`
	FillsRecorded             int64 `json:"fills_recorded"`
	DuplicateWebhooksIgnored  int64 `json:"duplicate_webhooks_ignored"`
	OutOfOrderFillsHandled    int64 `json:"out_of_order_fills_handled"`
	ReconciliationRuns        int64 `json:"reconciliation_runs"`
	ReconciliationFindings    int64 `json:"reconciliation_findings"`
	FailClosedTrips           int64 `json:"fail_closed_trips"`
}

// Metrics tracks thread-safe atomic operational counters.
type Metrics struct {
	OrdersSubmitted           atomic.Int64
	OrdersAccepted            atomic.Int64
	OrdersFilled              atomic.Int64
	OrdersCancelled           atomic.Int64
	OrdersRejectedPreDispatch atomic.Int64
	OrdersRejectedByBroker    atomic.Int64
	IdempotentHits            atomic.Int64
	RateLimitRejections       atomic.Int64
	AuthFailures              atomic.Int64
	UnknownOutcomes           atomic.Int64
	CancelTimeouts            atomic.Int64
	FillsRecorded             atomic.Int64
	DuplicateWebhooksIgnored  atomic.Int64
	OutOfOrderFillsHandled    atomic.Int64
	ReconciliationRuns        atomic.Int64
	ReconciliationFindings    atomic.Int64
	FailClosedTrips           atomic.Int64
}

// Snapshot returns a plain copy of all counters.
func (m *Metrics) Snapshot() MetricsSnapshot {
	return MetricsSnapshot{
		OrdersSubmitted:           m.OrdersSubmitted.Load(),
		OrdersAccepted:            m.OrdersAccepted.Load(),
		OrdersFilled:              m.OrdersFilled.Load(),
		OrdersCancelled:           m.OrdersCancelled.Load(),
		OrdersRejectedPreDispatch: m.OrdersRejectedPreDispatch.Load(),
		OrdersRejectedByBroker:    m.OrdersRejectedByBroker.Load(),
		IdempotentHits:            m.IdempotentHits.Load(),
		RateLimitRejections:       m.RateLimitRejections.Load(),
		AuthFailures:              m.AuthFailures.Load(),
		UnknownOutcomes:           m.UnknownOutcomes.Load(),
		CancelTimeouts:            m.CancelTimeouts.Load(),
		FillsRecorded:             m.FillsRecorded.Load(),
		DuplicateWebhooksIgnored:  m.DuplicateWebhooksIgnored.Load(),
		OutOfOrderFillsHandled:    m.OutOfOrderFillsHandled.Load(),
		ReconciliationRuns:        m.ReconciliationRuns.Load(),
		ReconciliationFindings:    m.ReconciliationFindings.Load(),
		FailClosedTrips:           m.FailClosedTrips.Load(),
	}
}
