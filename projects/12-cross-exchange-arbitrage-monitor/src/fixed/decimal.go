// Package fixed provides exact fixed-point decimal arithmetic with 8 decimal places
// (1e8 scale, satoshi precision) to eliminate floating-point rounding drift in price,
// quantity, fee, FX conversion, and arbitrage edge calculations.
package fixed

import (
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"strings"
)

const (
	// ScaleDigits is the number of fractional decimal digits represented by Decimal.
	ScaleDigits = 8
	// Scale is 10^8.
	Scale int64 = 100_000_000
	// BpsDenom is 10,000 (1 bp = 0.0001).
	BpsDenom int64 = 10_000
)

var (
	bigScale    = big.NewInt(Scale)
	bigBpsDenom = big.NewInt(BpsDenom)
	Zero        = Decimal{raw: 0}
	One         = Decimal{raw: Scale}

	ErrInvalidDecimal = errors.New("invalid fixed-point decimal string")
	ErrDivisionByZero = errors.New("fixed-point division by zero")
	ErrOverflow       = errors.New("fixed-point int64 overflow")
)

// Decimal is a signed fixed-point number scaled by 1e8.
// Its zero value represents 0.00000000.
type Decimal struct {
	raw int64
}

// FromRaw creates a Decimal directly from a pre-scaled int64 value.
func FromRaw(raw int64) Decimal {
	return Decimal{raw: raw}
}

// FromInt creates a Decimal from an integer value.
func FromInt(v int64) Decimal {
	return Decimal{raw: v * Scale}
}

// FromBps converts an integer basis-point count (e.g. 10 = 10 bps = 0.001) to a Decimal ratio.
func FromBps(bps int64) Decimal {
	// 1 bp = 1e-4 = 10,000 in 1e8 scale.
	return Decimal{raw: bps * (Scale / BpsDenom)}
}

// MustParse parses a decimal string or panics. Intended for constants and tests.
func MustParse(s string) Decimal {
	d, err := Parse(s)
	if err != nil {
		panic(fmt.Sprintf("fixed.MustParse(%q): %v", s, err))
	}
	return d
}

// Parse converts a base-10 decimal string (e.g. "65000.50", "-0.00125") into a Decimal.
// Rejects scientific notation, NaN, Inf, and non-numeric strings.
func Parse(s string) (Decimal, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return Zero, fmt.Errorf("%w: empty string", ErrInvalidDecimal)
	}

	neg := false
	switch s[0] {
	case '-':
		neg = true
		s = s[1:]
	case '+':
		s = s[1:]
	}
	if s == "" {
		return Zero, fmt.Errorf("%w: sign without digits", ErrInvalidDecimal)
	}

	parts := strings.Split(s, ".")
	if len(parts) > 2 {
		return Zero, fmt.Errorf("%w: multiple decimal points in %q", ErrInvalidDecimal, s)
	}

	intPartStr := parts[0]
	fracPartStr := ""
	if len(parts) == 2 {
		fracPartStr = parts[1]
	}
	if intPartStr == "" && fracPartStr == "" {
		return Zero, fmt.Errorf("%w: no digits", ErrInvalidDecimal)
	}

	intVal := new(big.Int)
	if intPartStr != "" {
		for i := 0; i < len(intPartStr); i++ {
			if intPartStr[i] < '0' || intPartStr[i] > '9' {
				return Zero, fmt.Errorf("%w: non-digit in integer part %q", ErrInvalidDecimal, s)
			}
		}
		if _, ok := intVal.SetString(intPartStr, 10); !ok {
			return Zero, fmt.Errorf("%w: invalid integer part %q", ErrInvalidDecimal, s)
		}
	}

	// Truncate beyond ScaleDigits conservatively after validating all characters are digits.
	for i := 0; i < len(fracPartStr); i++ {
		if fracPartStr[i] < '0' || fracPartStr[i] > '9' {
			return Zero, fmt.Errorf("%w: non-digit in fractional part %q", ErrInvalidDecimal, s)
		}
	}
	if len(fracPartStr) > ScaleDigits {
		fracPartStr = fracPartStr[:ScaleDigits]
	}
	for len(fracPartStr) < ScaleDigits {
		fracPartStr += "0"
	}

	fracVal := new(big.Int)
	if _, ok := fracVal.SetString(fracPartStr, 10); !ok {
		return Zero, fmt.Errorf("%w: invalid fractional part %q", ErrInvalidDecimal, s)
	}

	total := new(big.Int).Mul(intVal, bigScale)
	total.Add(total, fracVal)
	if neg {
		total.Neg(total)
	}
	if !total.IsInt64() {
		return Zero, fmt.Errorf("%w: %q exceeds int64 fixed-point range", ErrOverflow, s)
	}
	return Decimal{raw: total.Int64()}, nil
}

// Raw returns the underlying scaled int64 value.
func (d Decimal) Raw() int64 { return d.raw }

// IsZero reports whether d == 0.
func (d Decimal) IsZero() bool { return d.raw == 0 }

// IsPositive reports whether d > 0.
func (d Decimal) IsPositive() bool { return d.raw > 0 }

// IsNegative reports whether d < 0.
func (d Decimal) IsNegative() bool { return d.raw < 0 }

// Sign returns -1, 0, or +1.
func (d Decimal) Sign() int {
	if d.raw < 0 {
		return -1
	}
	if d.raw > 0 {
		return 1
	}
	return 0
}

// Cmp compares d and o, returning -1, 0, or +1.
func (d Decimal) Cmp(o Decimal) int {
	if d.raw < o.raw {
		return -1
	}
	if d.raw > o.raw {
		return 1
	}
	return 0
}

// Equal reports whether d == o.
func (d Decimal) Equal(o Decimal) bool { return d.raw == o.raw }

// LessThan reports whether d < o.
func (d Decimal) LessThan(o Decimal) bool { return d.raw < o.raw }

// LessThanOrEqual reports whether d <= o.
func (d Decimal) LessThanOrEqual(o Decimal) bool { return d.raw <= o.raw }

// GreaterThan reports whether d > o.
func (d Decimal) GreaterThan(o Decimal) bool { return d.raw > o.raw }

// GreaterThanOrEqual reports whether d >= o.
func (d Decimal) GreaterThanOrEqual(o Decimal) bool { return d.raw >= o.raw }

// Neg returns -d.
func (d Decimal) Neg() Decimal { return Decimal{raw: -d.raw} }

// Abs returns |d|.
func (d Decimal) Abs() Decimal {
	if d.raw < 0 {
		return Decimal{raw: -d.raw}
	}
	return d
}

// Add returns d + o.
func (d Decimal) Add(o Decimal) Decimal {
	return Decimal{raw: d.raw + o.raw}
}

// Sub returns d - o.
func (d Decimal) Sub(o Decimal) Decimal {
	return Decimal{raw: d.raw - o.raw}
}

// Mul returns d * o truncated toward zero using overflow-safe 128-bit/big.Int intermediates.
func (d Decimal) Mul(o Decimal) Decimal {
	if d.raw == 0 || o.raw == 0 {
		return Zero
	}
	prod := new(big.Int).Mul(big.NewInt(d.raw), big.NewInt(o.raw))
	prod.Quo(prod, bigScale)
	return Decimal{raw: prod.Int64()}
}

// MulRoundHalfUp returns d * o rounded half-away-from-zero.
func (d Decimal) MulRoundHalfUp(o Decimal) Decimal {
	if d.raw == 0 || o.raw == 0 {
		return Zero
	}
	prod := new(big.Int).Mul(big.NewInt(d.raw), big.NewInt(o.raw))
	half := big.NewInt(Scale / 2)
	if prod.Sign() >= 0 {
		prod.Add(prod, half)
	} else {
		prod.Sub(prod, half)
	}
	prod.Quo(prod, bigScale)
	return Decimal{raw: prod.Int64()}
}

// Div returns d / o truncated toward zero.
func (d Decimal) Div(o Decimal) (Decimal, error) {
	if o.raw == 0 {
		return Zero, ErrDivisionByZero
	}
	if d.raw == 0 {
		return Zero, nil
	}
	num := new(big.Int).Mul(big.NewInt(d.raw), bigScale)
	quo := num.Quo(num, big.NewInt(o.raw))
	if !quo.IsInt64() {
		return Zero, ErrOverflow
	}
	return Decimal{raw: quo.Int64()}, nil
}

// MulBps returns d * (bps / 10000), rounded up conservatively if roundUp is true (e.g. for fees).
func (d Decimal) MulBps(bps Decimal, roundUp bool) Decimal {
	if d.raw == 0 || bps.raw == 0 {
		return Zero
	}
	// bps is a Decimal representing basis points (e.g. 10.0 = 10 bps = 10/10000).
	// result = d.raw * bps.raw / (Scale * 10000)
	denom := new(big.Int).Mul(bigScale, bigBpsDenom)
	num := new(big.Int).Mul(big.NewInt(d.raw), big.NewInt(bps.raw))
	q, r := new(big.Int).QuoRem(num, denom, new(big.Int))
	if roundUp && r.Sign() > 0 {
		q.Add(q, big.NewInt(1))
	}
	return Decimal{raw: q.Int64()}
}

// RatioToBps computes (numerator / denominator) * 10000 as a Decimal in basis points.
func RatioToBps(numerator, denominator Decimal) (Decimal, error) {
	if denominator.raw == 0 {
		return Zero, ErrDivisionByZero
	}
	if numerator.raw == 0 {
		return Zero, nil
	}
	// raw_bps = (numerator.raw * Scale * 10000) / denominator.raw
	num := new(big.Int).Mul(big.NewInt(numerator.raw), bigScale)
	num.Mul(num, bigBpsDenom)
	quo := num.Quo(num, big.NewInt(denominator.raw))
	if !quo.IsInt64() {
		return Zero, ErrOverflow
	}
	return Decimal{raw: quo.Int64()}, nil
}

// FloorToStep floors a non-negative Decimal down to the nearest multiple of step.
// If step <= 0, returns d unchanged.
func (d Decimal) FloorToStep(step Decimal) Decimal {
	if step.raw <= 0 || d.raw <= 0 {
		return d
	}
	return Decimal{raw: (d.raw / step.raw) * step.raw}
}

// CeilToStep rounds a non-negative Decimal up to the nearest multiple of step.
// If step <= 0, returns d unchanged.
func (d Decimal) CeilToStep(step Decimal) Decimal {
	if step.raw <= 0 || d.raw <= 0 {
		return d
	}
	rem := d.raw % step.raw
	if rem == 0 {
		return d
	}
	return Decimal{raw: d.raw + (step.raw - rem)}
}

// IsMultipleOf reports whether d is an exact multiple of step.
func (d Decimal) IsMultipleOf(step Decimal) bool {
	if step.raw <= 0 {
		return true
	}
	return d.raw%step.raw == 0
}

// Min returns the smaller of a and b.
func Min(a, b Decimal) Decimal {
	if a.raw <= b.raw {
		return a
	}
	return b
}

// Max returns the larger of a and b.
func Max(a, b Decimal) Decimal {
	if a.raw >= b.raw {
		return a
	}
	return b
}

// String formats the Decimal cleanly by trimming trailing fractional zeros.
func (d Decimal) String() string {
	if d.raw == 0 {
		return "0"
	}
	neg := d.raw < 0
	var mag uint64
	if neg {
		mag = uint64(-d.raw)
	} else {
		mag = uint64(d.raw)
	}
	intPart := mag / uint64(Scale)
	fracPart := mag % uint64(Scale)
	var b strings.Builder
	if neg {
		b.WriteByte('-')
	}
	fmt.Fprintf(&b, "%d", intPart)
	if fracPart != 0 {
		fracStr := fmt.Sprintf("%08d", fracPart)
		fracStr = strings.TrimRight(fracStr, "0")
		b.WriteByte('.')
		b.WriteString(fracStr)
	}
	return b.String()
}

// FixedString formats the Decimal with exactly decimals fractional digits (0..8).
func (d Decimal) FixedString(decimals int) string {
	if decimals < 0 {
		decimals = 0
	}
	if decimals > ScaleDigits {
		decimals = ScaleDigits
	}
	neg := d.raw < 0
	var mag uint64
	if neg {
		mag = uint64(-d.raw)
	} else {
		mag = uint64(d.raw)
	}
	intPart := mag / uint64(Scale)
	fracPart := mag % uint64(Scale)
	var b strings.Builder
	if neg {
		b.WriteByte('-')
	}
	fmt.Fprintf(&b, "%d", intPart)
	if decimals > 0 {
		fracStr := fmt.Sprintf("%08d", fracPart)[:decimals]
		b.WriteByte('.')
		b.WriteString(fracStr)
	}
	return b.String()
}

// MarshalJSON encodes Decimal as a JSON string to preserve exact precision across languages.
func (d Decimal) MarshalJSON() ([]byte, error) {
	return json.Marshal(d.String())
}

// UnmarshalJSON decodes a Decimal from either a JSON string ("65000.5") or JSON number (65000.5).
func (d *Decimal) UnmarshalJSON(data []byte) error {
	trimmed := strings.TrimSpace(string(data))
	if trimmed == "null" {
		*d = Zero
		return nil
	}
	if len(trimmed) >= 2 && trimmed[0] == '"' && trimmed[len(trimmed)-1] == '"' {
		var s string
		if err := json.Unmarshal(data, &s); err != nil {
			return err
		}
		parsed, err := Parse(s)
		if err != nil {
			return err
		}
		*d = parsed
		return nil
	}
	// Accept bare JSON numeric tokens without going through float64.
	parsed, err := Parse(trimmed)
	if err != nil {
		return err
	}
	*d = parsed
	return nil
}
