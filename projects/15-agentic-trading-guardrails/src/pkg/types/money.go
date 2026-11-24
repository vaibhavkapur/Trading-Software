package types

import (
	"errors"
	"fmt"
	"math"
	"math/big"
	"strings"
)

const (
	// TicksPerDollar represents $0.0001 (1/10,000th of a USD) fixed-point precision.
	TicksPerDollar int64 = 10_000
	// TicksPerCent is the number of price ticks per USD cent ($0.01).
	TicksPerCent int64 = 100
	// CentsPerDollar is 100 cents per USD.
	CentsPerDollar int64 = 100
)

var (
	ErrInvalidDecimal   = errors.New("invalid decimal monetary string")
	ErrNegativeAmount   = errors.New("monetary amount or quantity must be non-negative")
	ErrNonPositiveValue = errors.New("quantity and price must be strictly positive")
	ErrArithmeticOver   = errors.New("fixed-point arithmetic overflow")
)

// PriceTicks represents a fixed-point price in $0.0001 increments (no float64).
type PriceTicks int64

// MoneyCents represents a fixed-point USD monetary value in cents ($0.01 increments).
type MoneyCents int64

// FormatUSD formats MoneyCents cleanly as e.g. "$1234.56".
func (m MoneyCents) FormatUSD() string {
	neg := m < 0
	abs := int64(m)
	if neg {
		abs = -abs
	}
	dollars := abs / CentsPerDollar
	cents := abs % CentsPerDollar
	if neg {
		return fmt.Sprintf("-$%d.%02d", dollars, cents)
	}
	return fmt.Sprintf("$%d.%02d", dollars, cents)
}

// FormatUSD formats PriceTicks cleanly as e.g. "$512.3400".
func (p PriceTicks) FormatUSD() string {
	neg := p < 0
	abs := int64(p)
	if neg {
		abs = -abs
	}
	dollars := abs / TicksPerDollar
	rem := abs % TicksPerDollar
	if neg {
		return fmt.Sprintf("-$%d.%04d", dollars, rem)
	}
	return fmt.Sprintf("$%d.%04d", dollars, rem)
}

// DollarsToCents converts whole USD dollars to MoneyCents safely.
func DollarsToCents(dollars int64) (MoneyCents, error) {
	if dollars > math.MaxInt64/CentsPerDollar || dollars < math.MinInt64/CentsPerDollar {
		return 0, ErrArithmeticOver
	}
	return MoneyCents(dollars * CentsPerDollar), nil
}

// ParseMoneyCents parses a decimal USD string (e.g. "500", "500.25") into exact MoneyCents without float64.
func ParseMoneyCents(raw string) (MoneyCents, error) {
	v, err := parseFixedDecimal(raw, 2)
	if err != nil {
		return 0, err
	}
	return MoneyCents(v), nil
}

// ParsePriceTicks parses a decimal USD price string (up to 4 decimal places) into exact PriceTicks without float64.
func ParsePriceTicks(raw string) (PriceTicks, error) {
	v, err := parseFixedDecimal(raw, 4)
	if err != nil {
		return 0, err
	}
	return PriceTicks(v), nil
}

func parseFixedDecimal(raw string, scale int) (int64, error) {
	s := strings.TrimSpace(raw)
	s = strings.TrimPrefix(s, "$")
	if s == "" {
		return 0, ErrInvalidDecimal
	}
	neg := false
	if strings.HasPrefix(s, "-") {
		neg = true
		s = strings.TrimPrefix(s, "-")
	} else if strings.HasPrefix(s, "+") {
		s = strings.TrimPrefix(s, "+")
	}
	if s == "" {
		return 0, ErrInvalidDecimal
	}
	parts := strings.Split(s, ".")
	if len(parts) > 2 {
		return 0, ErrInvalidDecimal
	}
	wholeStr := parts[0]
	fracStr := ""
	if len(parts) == 2 {
		fracStr = parts[1]
	}
	if wholeStr == "" && fracStr == "" {
		return 0, ErrInvalidDecimal
	}
	if len(fracStr) > scale {
		return 0, fmt.Errorf("%w: exceeds %d decimal places", ErrInvalidDecimal, scale)
	}
	if wholeStr == "" {
		wholeStr = "0"
	}
	for _, ch := range wholeStr {
		if ch < '0' || ch > '9' {
			return 0, ErrInvalidDecimal
		}
	}
	for _, ch := range fracStr {
		if ch < '0' || ch > '9' {
			return 0, ErrInvalidDecimal
		}
	}
	fracPadded := fracStr + strings.Repeat("0", scale-len(fracStr))
	combined := wholeStr + fracPadded
	n, ok := new(big.Int).SetString(combined, 10)
	if !ok {
		return 0, ErrInvalidDecimal
	}
	if neg {
		n.Neg(n)
	}
	if !n.IsInt64() {
		return 0, ErrArithmeticOver
	}
	return n.Int64(), nil
}

// ComputeNotionalCents computes ceil(quantity * priceTicks / TicksPerCent) using exact big.Int math.
// Rounding up conservatively ensures risk limits are never bypassed by sub-cent truncation.
func ComputeNotionalCents(quantity int64, priceTicks PriceTicks) (MoneyCents, error) {
	if quantity <= 0 || priceTicks <= 0 {
		return 0, ErrNonPositiveValue
	}
	q := big.NewInt(quantity)
	p := big.NewInt(int64(priceTicks))
	prod := new(big.Int).Mul(q, p)
	div := big.NewInt(TicksPerCent)
	quot, rem := new(big.Int).QuoRem(prod, div, new(big.Int))
	if rem.Sign() > 0 {
		quot.Add(quot, big.NewInt(1))
	}
	if !quot.IsInt64() {
		return 0, ErrArithmeticOver
	}
	return MoneyCents(quot.Int64()), nil
}

// SafeAddCents adds two MoneyCents values with overflow checking.
func SafeAddCents(a, b MoneyCents) (MoneyCents, error) {
	sum := new(big.Int).Add(big.NewInt(int64(a)), big.NewInt(int64(b)))
	if !sum.IsInt64() {
		return 0, ErrArithmeticOver
	}
	return MoneyCents(sum.Int64()), nil
}
