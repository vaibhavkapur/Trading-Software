package domain

import "time"

// GatewayEventType classifies real-time events published over WebSocket and journaled.
type GatewayEventType string

const (
	EventOrderUpdated          GatewayEventType = "ORDER_UPDATED"
	EventFillRecorded          GatewayEventType = "FILL_RECORDED"
	EventReconciliationReport  GatewayEventType = "RECONCILIATION_REPORT"
	EventGatewayHalted         GatewayEventType = "GATEWAY_HALTED"
	EventGatewayResumed        GatewayEventType = "GATEWAY_RESUMED"
	EventWebhookDeduplicated   GatewayEventType = "WEBHOOK_DEDUPLICATED"
)

// GatewayEvent is the versioned event envelope for WebSocket subscribers and audit streams.
type GatewayEvent struct {
	EventID       string           `json:"event_id"`
	Type          GatewayEventType `json:"type"`
	BrokerID      string           `json:"broker_id,omitempty"`
	OrderID       string           `json:"order_id,omitempty"`
	TimestampUTC  time.Time        `json:"timestamp_utc"`
	Payload       any              `json:"payload"`
}

// DriftType categorizes reconciliation discrepancies.
type DriftType string

const (
	DriftUnknownSubmitResolved DriftType = "UNKNOWN_SUBMIT_RESOLVED"
	DriftCancelTimeoutResolved DriftType = "CANCEL_TIMEOUT_RESOLVED"
	DriftStatusMismatch        DriftType = "STATUS_MISMATCH"
	DriftFillQtyMismatch       DriftType = "FILL_QTY_MISMATCH"
	DriftPhantomBrokerOrder    DriftType = "PHANTOM_BROKER_ORDER"
	DriftMissingBrokerOrder    DriftType = "MISSING_BROKER_ORDER"
	DriftUnresolvedOutcome     DriftType = "UNRESOLVED_OUTCOME"
)

// ReconciliationItem documents a single finding from an independent reconciliation pass.
type ReconciliationItem struct {
	BrokerID       string      `json:"broker_id"`
	OrderID        string      `json:"order_id,omitempty"`
	ClientOrderID  string      `json:"client_order_id,omitempty"`
	BrokerOrderID  string      `json:"broker_order_id,omitempty"`
	Drift          DriftType   `json:"drift"`
	LocalStatus    OrderStatus `json:"local_status,omitempty"`
	BrokerStatus   OrderStatus `json:"broker_status,omitempty"`
	LocalFilled    float64     `json:"local_filled"`
	BrokerFilled   float64     `json:"broker_filled"`
	Resolved       bool        `json:"resolved"`
	ActionTaken    string      `json:"action_taken"`
	Severity       string      `json:"severity"` // INFO, WARN, CRITICAL
}

// ReconciliationReport summarizes an independent reconciliation run across one or all brokers.
type ReconciliationReport struct {
	RunID             string               `json:"run_id"`
	StartedAtUTC      time.Time            `json:"started_at_utc"`
	CompletedAtUTC    time.Time            `json:"completed_at_utc"`
	BrokersChecked    []string             `json:"brokers_checked"`
	OrdersChecked     int                  `json:"orders_checked"`
	Findings          []ReconciliationItem `json:"findings"`
	UnresolvedCount   int                  `json:"unresolved_count"`
	TriggeredFailSafe bool                 `json:"triggered_fail_safe"`
}
