package storage

import "github.com/vaibhavkapur/Trading-Software/projects/17-multi-broker-trading-gateway/src/domain"

// Store defines the persistence contract for orders, fills, webhook deduplication,
// reconciliation reports, and replay cursors (PostgreSQL-compatible in production).
type Store interface {
	SaveOrder(order domain.Order) error
	GetOrder(orderID string) (domain.Order, bool)
	GetOrderByClientID(clientOrderID string) (domain.Order, bool)
	GetOrderByBrokerOrderID(brokerID, brokerOrderID string) (domain.Order, bool)
	ListOrders(brokerID string) []domain.Order

	// RecordFill stores a fill idempotently by (broker_id, broker_fill_id). Returns true if newly inserted.
	RecordFill(fill domain.Fill) (inserted bool, err error)
	ListFillsByOrder(orderID string) []domain.Fill

	// MarkWebhookProcessed records a webhook EventID idempotently. Returns true if first time seen.
	MarkWebhookProcessed(brokerID, eventID string) (firstSeen bool, err error)

	SaveReconciliationReport(report domain.ReconciliationReport) error
	ListReconciliationReports() []domain.ReconciliationReport

	GetReplayCursor(brokerID string) int64
	SetReplayCursor(brokerID string, cursor int64) error
}
