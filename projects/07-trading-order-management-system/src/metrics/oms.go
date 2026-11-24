package metrics

import "time"

// latencyBuckets covers 1ms to 60s.
var latencyBuckets = []float64{0.001, 0.0025, 0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30, 60}

// repairBuckets covers 1s to 1 day.
var repairBuckets = []float64{1, 5, 15, 60, 300, 900, 3600, 4 * 3600, 24 * 3600}

// Set is every metric the OMS records as it works. Gauges that describe the
// database (outbox depth and age, open issues) are registered separately by
// the process that owns the store; see app.RegisterGauges.
type Set struct {
	Registry *Registry

	// HTTP
	HTTPRequests *Counter   // route, status
	HTTPDuration *Histogram // route

	// Commands
	OrdersCreated     *Counter // result: validated | rejected
	IdempotentReplays *Counter // operation

	// Outbox
	OutboxDispatches *Counter   // type, result: delivered | retry | rejected | skipped | dead
	OutboxDelay      *Histogram // type; enqueue to accepted by the broker

	// Broker reports
	BrokerEvents     *Counter   // kind, source, outcome
	SubmitToAck      *Histogram // first submission attempt to acknowledgement applied
	OrderStateLag    *Histogram // kind; broker event time to applied locally
	FutureTimestamps *Counter   // broker reports stamped later than the local clock
	WebhookRejects   *Counter   // reason: signature | malformed

	// Reconciliation
	ReconRuns     *Counter   // status
	ReconDrift    *Counter   // kind
	ReconRepairs  *Counter   // kind; drift repaired automatically
	ReconDuration *Histogram // run duration
	RepairTime    *Histogram // kind; issue detected to resolved
}

// New builds the OMS metric set on a fresh registry.
func New() *Set {
	r := &Registry{}
	return &Set{
		Registry: r,
		HTTPRequests: r.Counter("oms_http_requests_total",
			"HTTP requests served, by route and status code.", "route", "status"),
		HTTPDuration: r.Histogram("oms_http_request_duration_seconds",
			"HTTP request duration by route.", latencyBuckets, "route"),

		OrdersCreated: r.Counter("oms_orders_created_total",
			"Orders accepted by the command API, by pre-trade result.", "result"),
		IdempotentReplays: r.Counter("oms_idempotent_replays_total",
			"Requests answered from a stored idempotency record instead of being executed again.", "operation"),

		OutboxDispatches: r.Counter("oms_outbox_dispatch_total",
			"Outbox delivery attempts by message type and result.", "type", "result"),
		OutboxDelay: r.Histogram("oms_outbox_delivery_seconds",
			"Time from a broker call being queued to the broker accepting it.", latencyBuckets, "type"),

		BrokerEvents: r.Counter("oms_broker_events_total",
			"Broker reports received, by kind, transport and outcome. "+
				"The duplicate rate is outcome=DUPLICATE over the total.", "kind", "source", "outcome"),
		SubmitToAck: r.Histogram("oms_submit_to_ack_seconds",
			"Time from the first submission attempt to the broker acknowledgement being applied.", latencyBuckets),
		OrderStateLag: r.Histogram("oms_order_state_lag_seconds",
			"Time from the broker's event timestamp to the report being applied here, by report kind. "+
				"Depends on the broker's clock; negative lags are recorded as zero.", latencyBuckets, "kind"),
		FutureTimestamps: r.Counter("oms_broker_future_timestamps_total",
			"Broker reports whose event time was more than a second ahead of the local clock (clock skew)."),
		WebhookRejects: r.Counter("oms_webhook_rejected_total",
			"Webhook deliveries refused before processing, by reason.", "reason"),

		ReconRuns: r.Counter("oms_reconciliation_runs_total",
			"Reconciliation runs by final status.", "status"),
		ReconDrift: r.Counter("oms_reconciliation_drift_total",
			"Discrepancies between the OMS and the broker found by reconciliation, by kind.", "kind"),
		ReconRepairs: r.Counter("oms_reconciliation_auto_repaired_total",
			"Discrepancies repaired automatically from the broker snapshot, by kind.", "kind"),
		ReconDuration: r.Histogram("oms_reconciliation_run_duration_seconds",
			"Duration of reconciliation runs.", latencyBuckets),
		RepairTime: r.Histogram("oms_reconciliation_repair_seconds",
			"Time from an issue being detected to it being resolved, by kind.", repairBuckets, "kind"),
	}
}

// Seconds converts a duration for a histogram, clamping negatives to zero.
func Seconds(d time.Duration) float64 {
	if d < 0 {
		return 0
	}
	return d.Seconds()
}
