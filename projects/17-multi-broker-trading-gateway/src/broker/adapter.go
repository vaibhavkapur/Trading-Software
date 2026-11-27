package broker

import (
	"context"
	"time"

	"github.com/vaibhavkapur/Trading-Software/projects/17-multi-broker-trading-gateway/src/domain"
)

// BrokerOrderSubmit is the broker-bound order payload after canonical validation and symbol mapping.
type BrokerOrderSubmit struct {
	OrderID       string               `json:"order_id"`
	ClientOrderID string               `json:"client_order_id"`
	Mode          domain.ExecutionMode `json:"mode"`
	BrokerSymbol  string               `json:"broker_symbol"`
	Side          domain.Side          `json:"side"`
	OrderType     domain.OrderType     `json:"order_type"`
	TimeInForce   domain.TimeInForce   `json:"time_in_force"`
	Quantity      float64              `json:"quantity"`
	LimitPrice    float64              `json:"limit_price,omitempty"`
	StopPrice     float64              `json:"stop_price,omitempty"`
	Currency      string               `json:"currency"`
}

// BrokerSubmitAck is the normalized adapter acknowledgement to an order submission.
type BrokerSubmitAck struct {
	BrokerOrderID string             `json:"broker_order_id"`
	Status        domain.OrderStatus `json:"status"`
	Fills         []BrokerFillRecord `json:"fills,omitempty"`
	RejectReason  string             `json:"reject_reason,omitempty"`
	AckTimeUTC    time.Time          `json:"ack_time_utc"`
}

// BrokerCancelAck is the normalized adapter acknowledgement to a cancel request.
type BrokerCancelAck struct {
	BrokerOrderID string             `json:"broker_order_id"`
	Status        domain.OrderStatus `json:"status"`
	CancelTimeUTC time.Time          `json:"cancel_time_utc"`
}

// BrokerOrderSnapshot is the broker's authoritative view of an order used for reconciliation.
type BrokerOrderSnapshot struct {
	BrokerOrderID  string             `json:"broker_order_id"`
	ClientOrderID  string             `json:"client_order_id"`
	BrokerSymbol   string             `json:"broker_symbol"`
	Side           domain.Side        `json:"side"`
	OrderType      domain.OrderType   `json:"order_type"`
	Quantity       float64            `json:"quantity"`
	FilledQuantity float64            `json:"filled_quantity"`
	AvgFillPrice   float64            `json:"avg_fill_price"`
	Status         domain.OrderStatus `json:"status"`
	UpdatedAtUTC   time.Time          `json:"updated_at_utc"`
}

// BrokerFillRecord is the broker's normalized fill representation before canonical mapping.
type BrokerFillRecord struct {
	EventID       string    `json:"event_id"`
	BrokerOrderID string    `json:"broker_order_id"`
	ClientOrderID string    `json:"client_order_id"`
	BrokerFillID  string    `json:"broker_fill_id"`
	BrokerSymbol  string    `json:"broker_symbol"`
	Side          domain.Side `json:"side"`
	Sequence      int64     `json:"sequence"`
	Quantity      float64   `json:"quantity"`
	Price         float64   `json:"price"`
	Fee           float64   `json:"fee"`
	Currency      string    `json:"currency"`
	ExecutedAtUTC time.Time `json:"executed_at_utc"`
}

// Adapter is the unified broker adapter contract implemented by mock and sandbox providers.
type Adapter interface {
	BrokerID() string
	Capabilities() domain.BrokerCapabilities
	SubmitOrder(ctx context.Context, req BrokerOrderSubmit) (BrokerSubmitAck, error)
	CancelOrder(ctx context.Context, brokerOrderID, clientOrderID string) (BrokerCancelAck, error)
	GetOrderByClientID(ctx context.Context, clientOrderID string) (BrokerOrderSnapshot, bool, error)
	ListOrders(ctx context.Context) ([]BrokerOrderSnapshot, error)
	ListFills(ctx context.Context, clientOrderID string) ([]BrokerFillRecord, error)
	ReplayEvents(ctx context.Context, afterCursor int64) ([]domain.WebhookEvent, int64, error)
	HealthCheck(ctx context.Context) error
	DispatchCount() int64
}
