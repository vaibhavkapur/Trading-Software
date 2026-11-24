// Package risk is the pre-trade risk boundary. The OMS asks an Authorizer
// before it accepts an order or a replace; it never decides risk itself.
//
// StaticLimits is a deliberately small built-in authorizer so the OMS runs on
// its own. A fuller engine (for example project 05 in this repository) can be
// plugged in behind the same interface.
package risk

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"

	"oms/src/domain"
	"oms/src/fixed"
)

// Request describes the order to authorize, with the terms it would have.
type Request struct {
	OrderID    string
	AccountID  string
	Symbol     string
	Side       domain.Side
	Type       domain.OrderType
	Qty        int64
	LimitPrice *fixed.Micros
}

// Decision is the authorizer's answer.
type Decision struct {
	Allowed bool
	// Codes are stable machine-readable reasons for a refusal.
	Codes  []string
	Reason string
	// Authorizer names the engine and rule set that decided.
	Authorizer string
	// Reference is the authorizer's own id for the decision, if it has one.
	Reference string
}

// Authorizer makes pre-trade risk decisions. An error means no decision could
// be made; callers must treat that as a refusal to proceed (fail closed).
type Authorizer interface {
	Authorize(ctx context.Context, r Request) (Decision, error)
}

// Refusal codes.
const (
	CodeSymbolNotAllowed = "SYMBOL_NOT_ALLOWED"
	CodeMaxOrderQty      = "MAX_ORDER_QTY"
	CodeMaxOrderNotional = "MAX_ORDER_NOTIONAL"
	CodeMarketOrders     = "MARKET_ORDERS_DISABLED"
)

// StaticLimits authorizes orders against fixed per-order limits.
type StaticLimits struct {
	// Version labels the rule set in every decision.
	Version string `json:"version"`
	// AllowedSymbols lists tradable symbols. "*" allows any symbol.
	AllowedSymbols []string `json:"allowed_symbols"`
	MaxOrderQty    int64    `json:"max_order_qty"`
	// MaxOrderNotional caps quantity × limit price. Market orders have no
	// price to check, which is why they must be enabled separately.
	MaxOrderNotional  fixed.Micros `json:"max_order_notional"`
	AllowMarketOrders bool         `json:"allow_market_orders"`
}

// LoadStaticLimits reads limits from a JSON file and validates them.
func LoadStaticLimits(path string) (*StaticLimits, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("risk: read limits: %w", err)
	}
	var l StaticLimits
	if err := json.Unmarshal(data, &l); err != nil {
		return nil, fmt.Errorf("risk: parse limits %s: %w", path, err)
	}
	if err := l.Validate(); err != nil {
		return nil, fmt.Errorf("risk: limits %s: %w", path, err)
	}
	return &l, nil
}

// Validate rejects limits that would silently allow everything or nothing.
func (l *StaticLimits) Validate() error {
	switch {
	case l.Version == "":
		return errors.New("version is required")
	case len(l.AllowedSymbols) == 0:
		return errors.New(`allowed_symbols is required (use ["*"] to allow any symbol)`)
	case l.MaxOrderQty <= 0:
		return errors.New("max_order_qty must be positive")
	case l.MaxOrderNotional <= 0:
		return errors.New("max_order_notional must be positive")
	}
	return nil
}

func (l *StaticLimits) Authorize(_ context.Context, r Request) (Decision, error) {
	d := Decision{Authorizer: "static-limits/" + l.Version}
	refuse := func(code, msg string) {
		d.Codes = append(d.Codes, code)
		if d.Reason != "" {
			d.Reason += "; "
		}
		d.Reason += msg
	}

	allowed := false
	for _, s := range l.AllowedSymbols {
		if s == "*" || s == r.Symbol {
			allowed = true
			break
		}
	}
	if !allowed {
		refuse(CodeSymbolNotAllowed, fmt.Sprintf("symbol %s is not tradable", r.Symbol))
	}
	if r.Qty > l.MaxOrderQty {
		refuse(CodeMaxOrderQty, fmt.Sprintf("quantity %d exceeds the per-order limit %d", r.Qty, l.MaxOrderQty))
	}
	switch {
	case r.LimitPrice != nil:
		notional, ok := fixed.MulQty(*r.LimitPrice, r.Qty)
		if !ok || notional > l.MaxOrderNotional {
			refuse(CodeMaxOrderNotional, fmt.Sprintf("order notional exceeds the per-order limit %s", l.MaxOrderNotional))
		}
	case !l.AllowMarketOrders:
		refuse(CodeMarketOrders, "market orders are disabled")
	}

	d.Allowed = len(d.Codes) == 0
	return d, nil
}
