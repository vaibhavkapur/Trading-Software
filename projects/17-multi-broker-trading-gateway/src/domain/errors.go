package domain

import (
	"errors"
	"fmt"
	"time"
)

// ErrorCode defines structured, machine-actionable gateway error categories.
type ErrorCode string

const (
	ErrCodeValidation            ErrorCode = "VALIDATION_ERROR"
	ErrCodePolicyDenied          ErrorCode = "POLICY_DENIED"
	ErrCodeGatewayHalted         ErrorCode = "GATEWAY_HALTED"
	ErrCodeBrokerNotFound        ErrorCode = "BROKER_NOT_FOUND"
	ErrCodeUnsupportedCapability ErrorCode = "UNSUPPORTED_CAPABILITY"
	ErrCodeSymbolMismatch        ErrorCode = "SYMBOL_MISMATCH"
	ErrCodeIdempotencyConflict   ErrorCode = "IDEMPOTENCY_CONFLICT"
	ErrCodeRateLimited           ErrorCode = "RATE_LIMITED"
	ErrCodeAuthExpired           ErrorCode = "AUTH_EXPIRED"
	ErrCodeAuthInvalid           ErrorCode = "AUTH_INVALID"
	ErrCodeUnknownOutcome        ErrorCode = "UNKNOWN_OUTCOME"
	ErrCodeCancelTimeout         ErrorCode = "CANCEL_TIMEOUT"
	ErrCodeOrderNotFound         ErrorCode = "ORDER_NOT_FOUND"
	ErrCodeInvalidTransition     ErrorCode = "INVALID_STATE_TRANSITION"
	ErrCodeBrokerRejected        ErrorCode = "BROKER_REJECTED"
	ErrCodeReconciliationDrift   ErrorCode = "RECONCILIATION_DRIFT"
	ErrCodeInternal              ErrorCode = "INTERNAL_ERROR"
)

// GatewayError is a structured error with explicit retryability and metadata.
type GatewayError struct {
	Code       ErrorCode     `json:"code"`
	Message    string        `json:"message"`
	Retryable  bool          `json:"retryable"`
	RetryAfter time.Duration `json:"retry_after_ns,omitempty"`
	BrokerID   string        `json:"broker_id,omitempty"`
	OrderID    string        `json:"order_id,omitempty"`
	Cause      error         `json:"-"`
}

func (e *GatewayError) Error() string {
	if e.BrokerID != "" && e.OrderID != "" {
		return fmt.Sprintf("[%s] broker=%s order=%s: %s", e.Code, e.BrokerID, e.OrderID, e.Message)
	}
	if e.BrokerID != "" {
		return fmt.Sprintf("[%s] broker=%s: %s", e.Code, e.BrokerID, e.Message)
	}
	return fmt.Sprintf("[%s] %s", e.Code, e.Message)
}

func (e *GatewayError) Unwrap() error {
	return e.Cause
}

// NewError creates a new GatewayError.
func NewError(code ErrorCode, msg string, retryable bool) *GatewayError {
	return &GatewayError{
		Code:      code,
		Message:   msg,
		Retryable: retryable,
	}
}

// WithContext attaches broker/order identifiers to a GatewayError.
func (e *GatewayError) WithContext(brokerID, orderID string) *GatewayError {
	cp := *e
	if brokerID != "" {
		cp.BrokerID = brokerID
	}
	if orderID != "" {
		cp.OrderID = orderID
	}
	return &cp
}

// WithRetryAfter attaches retry backoff duration.
func (e *GatewayError) WithRetryAfter(d time.Duration) *GatewayError {
	cp := *e
	cp.RetryAfter = d
	return &cp
}

// WithCause wraps an underlying error.
func (e *GatewayError) WithCause(err error) *GatewayError {
	cp := *e
	cp.Cause = err
	return &cp
}

// ErrorCodeOf extracts the structured ErrorCode from any error.
func ErrorCodeOf(err error) ErrorCode {
	if err == nil {
		return ""
	}
	var ge *GatewayError
	if errors.As(err, &ge) {
		return ge.Code
	}
	return ErrCodeInternal
}
