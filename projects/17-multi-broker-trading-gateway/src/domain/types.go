package domain

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"math"
	"strings"
	"time"
)

// ExecutionMode enforces separation between paper/sandbox and live capital.
type ExecutionMode string

const (
	ModePaper   ExecutionMode = "paper"
	ModeSandbox ExecutionMode = "sandbox"
	ModeLive    ExecutionMode = "live"
)

// AssetClass enumerates canonical asset classes.
type AssetClass string

const (
	AssetEquity AssetClass = "EQUITY"
	AssetETF    AssetClass = "ETF"
	AssetCrypto AssetClass = "CRYPTO"
	AssetFX     AssetClass = "FX"
	AssetOption AssetClass = "OPTION"
)

// Side represents buy or sell direction.
type Side string

const (
	SideBuy  Side = "BUY"
	SideSell Side = "SELL"
)

// OrderType enumerates canonical order types.
type OrderType string

const (
	OrderTypeMarket    OrderType = "MARKET"
	OrderTypeLimit     OrderType = "LIMIT"
	OrderTypeStopLimit OrderType = "STOP_LIMIT"
	OrderTypeStop      OrderType = "STOP"
)

// TimeInForce enumerates order duration semantics.
type TimeInForce string

const (
	TIFDay TimeInForce = "DAY"
	TIFGTC TimeInForce = "GTC"
	TIFIOC TimeInForce = "IOC"
	TIFFOK TimeInForce = "FOK"
)

// OrderStatus represents the canonical order lifecycle state machine.
type OrderStatus string

const (
	StatusPendingSubmit   OrderStatus = "PENDING_SUBMIT"
	StatusSubmitUnknown   OrderStatus = "SUBMIT_UNKNOWN"
	StatusPendingNew      OrderStatus = "PENDING_NEW"
	StatusAccepted        OrderStatus = "ACCEPTED"
	StatusPartiallyFilled OrderStatus = "PARTIALLY_FILLED"
	StatusFilled          OrderStatus = "FILLED"
	StatusCancelPending   OrderStatus = "CANCEL_PENDING"
	StatusCancelled       OrderStatus = "CANCELLED"
	StatusRejected        OrderStatus = "REJECTED"
	StatusExpired         OrderStatus = "EXPIRED"
)

// IsTerminal reports whether the order status is terminal and cannot regress.
func (s OrderStatus) IsTerminal() bool {
	switch s {
	case StatusFilled, StatusCancelled, StatusRejected, StatusExpired:
		return true
	default:
		return false
	}
}

// Rank returns a monotonic precedence rank for non-regressing status resolution.
func (s OrderStatus) Rank() int {
	switch s {
	case StatusPendingSubmit:
		return 10
	case StatusSubmitUnknown:
		return 20
	case StatusPendingNew:
		return 30
	case StatusAccepted:
		return 40
	case StatusPartiallyFilled:
		return 50
	case StatusCancelPending:
		return 60
	case StatusCancelled, StatusExpired, StatusRejected:
		return 80
	case StatusFilled:
		return 90
	default:
		return 0
	}
}

// OrderRequest is the canonical client order submission payload.
type OrderRequest struct {
	ClientOrderID string        `json:"client_order_id"`
	BrokerID      string        `json:"broker_id"`
	Mode          ExecutionMode `json:"mode"`
	InstrumentID  string        `json:"instrument_id"`
	Side          Side          `json:"side"`
	OrderType     OrderType     `json:"order_type"`
	TimeInForce   TimeInForce   `json:"time_in_force"`
	Quantity      float64       `json:"quantity"`
	LimitPrice    float64       `json:"limit_price,omitempty"`
	StopPrice     float64       `json:"stop_price,omitempty"`
	Currency      string        `json:"currency"`
	SubmittedAt   time.Time     `json:"submitted_at_utc"`
}

// PayloadHash computes a deterministic canonical fingerprint for idempotency conflict checks.
func (r OrderRequest) PayloadHash() string {
	canon := fmt.Sprintf("%s|%s|%s|%s|%s|%s|%s|%.8f|%.8f|%.8f|%s",
		r.ClientOrderID,
		r.BrokerID,
		r.Mode,
		r.InstrumentID,
		r.Side,
		r.OrderType,
		r.TimeInForce,
		r.Quantity,
		r.LimitPrice,
		r.StopPrice,
		strings.ToUpper(r.Currency),
	)
	sum := sha256.Sum256([]byte(canon))
	return hex.EncodeToString(sum[:])
}

// ValidateBasic checks structural invariants independent of broker capabilities.
func (r OrderRequest) ValidateBasic() error {
	if strings.TrimSpace(r.ClientOrderID) == "" {
		return NewError(ErrCodeValidation, "client_order_id is required", false)
	}
	if strings.TrimSpace(r.BrokerID) == "" {
		return NewError(ErrCodeValidation, "broker_id is required", false)
	}
	if strings.TrimSpace(r.InstrumentID) == "" {
		return NewError(ErrCodeValidation, "instrument_id is required", false)
	}
	if r.Side != SideBuy && r.Side != SideSell {
		return NewError(ErrCodeValidation, fmt.Sprintf("invalid side: %q", r.Side), false)
	}
	if r.Quantity <= 0 || math.IsNaN(r.Quantity) || math.IsInf(r.Quantity, 0) {
		return NewError(ErrCodeValidation, "quantity must be positive and finite", false)
	}
	if strings.TrimSpace(r.Currency) == "" {
		return NewError(ErrCodeValidation, "currency is required", false)
	}
	switch r.OrderType {
	case OrderTypeMarket:
	case OrderTypeLimit:
		if r.LimitPrice <= 0 {
			return NewError(ErrCodeValidation, "limit_price must be positive for LIMIT orders", false)
		}
	case OrderTypeStopLimit:
		if r.LimitPrice <= 0 || r.StopPrice <= 0 {
			return NewError(ErrCodeValidation, "limit_price and stop_price must be positive for STOP_LIMIT orders", false)
		}
	case OrderTypeStop:
		if r.StopPrice <= 0 {
			return NewError(ErrCodeValidation, "stop_price must be positive for STOP orders", false)
		}
	default:
		return NewError(ErrCodeValidation, fmt.Sprintf("unknown order_type: %q", r.OrderType), false)
	}
	if r.TimeInForce != TIFDay && r.TimeInForce != TIFGTC && r.TimeInForce != TIFIOC && r.TimeInForce != TIFFOK {
		return NewError(ErrCodeValidation, fmt.Sprintf("unknown time_in_force: %q", r.TimeInForce), false)
	}
	return nil
}

// EstimatedNotional returns an explicit notional estimate for risk/capability checks.
func (r OrderRequest) EstimatedNotional(referencePrice float64) float64 {
	px := r.LimitPrice
	if px <= 0 {
		px = r.StopPrice
	}
	if px <= 0 {
		px = referencePrice
	}
	if px <= 0 {
		px = 100.0 // conservative fallback when no market ref price is supplied for synthetic market orders
	}
	return r.Quantity * px
}

// Order is the normalized canonical gateway order record.
type Order struct {
	OrderID        string        `json:"order_id"`
	ClientOrderID  string        `json:"client_order_id"`
	BrokerID       string        `json:"broker_id"`
	BrokerOrderID  string        `json:"broker_order_id,omitempty"`
	Mode           ExecutionMode `json:"mode"`
	InstrumentID   string        `json:"instrument_id"`
	BrokerSymbol   string        `json:"broker_symbol"`
	Side           Side          `json:"side"`
	OrderType      OrderType     `json:"order_type"`
	TimeInForce    TimeInForce   `json:"time_in_force"`
	Quantity       float64       `json:"quantity"`
	FilledQuantity float64       `json:"filled_quantity"`
	RemainingQty   float64       `json:"remaining_qty"`
	LimitPrice     float64       `json:"limit_price,omitempty"`
	StopPrice      float64       `json:"stop_price,omitempty"`
	AvgFillPrice   float64       `json:"avg_fill_price"`
	TotalFees      float64       `json:"total_fees"`
	Currency       string        `json:"currency"`
	Status         OrderStatus   `json:"status"`
	RejectReason   string        `json:"reject_reason,omitempty"`
	PayloadHash    string        `json:"payload_hash"`
	Reconciled     bool          `json:"reconciled"`
	LastErrorCode  ErrorCode     `json:"last_error_code,omitempty"`
	CreatedAtUTC   time.Time     `json:"created_at_utc"`
	UpdatedAtUTC   time.Time     `json:"updated_at_utc"`
}

// Fill is the canonical normalized execution fill record.
type Fill struct {
	FillID        string    `json:"fill_id"`
	EventID       string    `json:"event_id,omitempty"`
	OrderID       string    `json:"order_id"`
	ClientOrderID string    `json:"client_order_id"`
	BrokerID      string    `json:"broker_id"`
	BrokerOrderID string    `json:"broker_order_id"`
	BrokerFillID  string    `json:"broker_fill_id"`
	InstrumentID  string    `json:"instrument_id"`
	BrokerSymbol  string    `json:"broker_symbol"`
	Side          Side      `json:"side"`
	Sequence      int64     `json:"sequence"`
	Quantity      float64   `json:"quantity"`
	Price         float64   `json:"price"`
	Fee           float64   `json:"fee"`
	Currency      string    `json:"currency"`
	ExecutedAtUTC time.Time `json:"executed_at_utc"`
	ReceivedAtUTC time.Time `json:"received_at_utc"`
}

// WebhookEvent represents a raw broker webhook or push update before canonical ingestion.
type WebhookEvent struct {
	EventID       string      `json:"event_id"`
	BrokerID      string      `json:"broker_id"`
	EventType     string      `json:"event_type"` // FILL, STATUS, CANCELLED, REJECTED
	BrokerOrderID string      `json:"broker_order_id"`
	ClientOrderID string      `json:"client_order_id"`
	BrokerFillID  string      `json:"broker_fill_id,omitempty"`
	Sequence      int64       `json:"sequence"`
	Status        OrderStatus `json:"status,omitempty"`
	FillQty       float64     `json:"fill_qty,omitempty"`
	FillPrice     float64     `json:"fill_price,omitempty"`
	Fee           float64     `json:"fee,omitempty"`
	Currency      string      `json:"currency,omitempty"`
	Reason        string      `json:"reason,omitempty"`
	EventTimeUTC  time.Time   `json:"event_time_utc"`
}

// RecomputeOrderFromFills deterministically recomputes filled quantity, remaining quantity,
// VWAP average fill price, fees, and monotonic order status from deduplicated fills.
func RecomputeOrderFromFills(o *Order, fills []Fill, nowUTC time.Time) {
	var totalQty, totalNotional, totalFees float64
	for _, f := range fills {
		totalQty += f.Quantity
		totalNotional += f.Quantity * f.Price
		totalFees += f.Fee
	}
	// Guard against floating-point noise.
	totalQty = math.Round(totalQty*1e8) / 1e8
	o.FilledQuantity = totalQty
	rem := math.Round((o.Quantity-totalQty)*1e8) / 1e8
	if rem < 0 {
		rem = 0
	}
	o.RemainingQty = rem
	if totalQty > 0 {
		o.AvgFillPrice = math.Round((totalNotional/totalQty)*1e8) / 1e8
	}
	o.TotalFees = math.Round(totalFees*1e8) / 1e8

	if totalQty >= o.Quantity-1e-9 && o.Quantity > 0 {
		o.Status = StatusFilled
		o.RemainingQty = 0
	} else if totalQty > 0 {
		if !o.Status.IsTerminal() && o.Status != StatusCancelPending {
			o.Status = StatusPartiallyFilled
		}
	}
	o.UpdatedAtUTC = nowUTC.UTC()
}
