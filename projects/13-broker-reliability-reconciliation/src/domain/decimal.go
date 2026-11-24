package domain

import (
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"strings"
)

// DecimalScale is the fixed number of fractional decimal places (10^-8).
const DecimalScale = 8

var (
	scaleFactor = big.NewInt(100_000_000)
	halfScale   = big.NewInt(50_000_000)
	zeroBig     = big.NewInt(0)
)

// Decimal is an immutable fixed-point decimal with 8 fractional digits (scale 10^8).
// All money, quantity, price, and fee arithmetic uses integer math via big.Int —
// never binary floating point — avoiding rounding drift and int64 multiplication overflow.
type Decimal struct {
	scaled *big.Int
}

// Zero returns the zero Decimal.
func Zero() Decimal {
	return Decimal{scaled: big.NewInt(0)}
}

// DecimalFromInt creates a Decimal from an integer value.
func DecimalFromInt(v int64) Decimal {
	s := new(big.Int).Mul(big.NewInt(v), scaleFactor)
	return Decimal{scaled: s}
}

// ParseDecimal parses a base-10 decimal string (e.g. "123.45", "-0.0050", "100")
// into a fixed-point Decimal with scale 8. Uses half-up rounding if >8 fractional digits.
func ParseDecimal(raw string) (Decimal, error) {
	s := strings.TrimSpace(raw)
	if s == "" {
		return Zero(), errors.New("empty decimal string")
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
		return Zero(), fmt.Errorf("invalid decimal %q", raw)
	}

	parts := strings.Split(s, ".")
	if len(parts) > 2 {
		return Zero(), fmt.Errorf("invalid decimal %q: multiple decimal points", raw)
	}

	intPart := parts[0]
	fracPart := ""
	if len(parts) == 2 {
		fracPart = parts[1]
	}
	if intPart == "" && fracPart == "" {
		return Zero(), fmt.Errorf("invalid decimal %q", raw)
	}
	if intPart == "" {
		intPart = "0"
	}

	for i := 0; i < len(intPart); i++ {
		if intPart[i] < '0' || intPart[i] > '9' {
			return Zero(), fmt.Errorf("invalid decimal %q: non-digit in integer part", raw)
		}
	}
	for i := 0; i < len(fracPart); i++ {
		if fracPart[i] < '0' || fracPart[i] > '9' {
			return Zero(), fmt.Errorf("invalid decimal %q: non-digit in fractional part", raw)
		}
	}

	roundUp := false
	if len(fracPart) > DecimalScale {
		if fracPart[DecimalScale] >= '5' {
			roundUp = true
		}
		fracPart = fracPart[:DecimalScale]
	} else if len(fracPart) < DecimalScale {
		fracPart = fracPart + strings.Repeat("0", DecimalScale-len(fracPart))
	}

	combined := intPart + fracPart
	val, ok := new(big.Int).SetString(combined, 10)
	if !ok {
		return Zero(), fmt.Errorf("invalid decimal %q", raw)
	}
	if roundUp {
		val.Add(val, big.NewInt(1))
	}
	if neg && val.Sign() != 0 {
		val.Neg(val)
	}
	return Decimal{scaled: val}, nil
}

// MustParseDecimal parses a decimal string and panics on invalid input (intended for constants/tests).
func MustParseDecimal(raw string) Decimal {
	d, err := ParseDecimal(raw)
	if err != nil {
		panic(err)
	}
	return d
}

func (d Decimal) val() *big.Int {
	if d.scaled == nil {
		return zeroBig
	}
	return d.scaled
}

// Add returns d + other.
func (d Decimal) Add(other Decimal) Decimal {
	return Decimal{scaled: new(big.Int).Add(d.val(), other.val())}
}

// Sub returns d - other.
func (d Decimal) Sub(other Decimal) Decimal {
	return Decimal{scaled: new(big.Int).Sub(d.val(), other.val())}
}

// Mul returns d * other rounded half-up to DecimalScale.
func (d Decimal) Mul(other Decimal) Decimal {
	prod := new(big.Int).Mul(d.val(), other.val())
	neg := prod.Sign() < 0
	absProd := new(big.Int).Abs(prod)
	q, r := new(big.Int).QuoRem(absProd, scaleFactor, new(big.Int))
	if r.Cmp(halfScale) >= 0 {
		q.Add(q, big.NewInt(1))
	}
	if neg && q.Sign() != 0 {
		q.Neg(q)
	}
	return Decimal{scaled: q}
}

// Neg returns -d.
func (d Decimal) Neg() Decimal {
	if d.val().Sign() == 0 {
		return Zero()
	}
	return Decimal{scaled: new(big.Int).Neg(d.val())}
}

// Abs returns |d|.
func (d Decimal) Abs() Decimal {
	return Decimal{scaled: new(big.Int).Abs(d.val())}
}

// Cmp compares d and other (-1 if d < other, 0 if equal, +1 if d > other).
func (d Decimal) Cmp(other Decimal) int {
	return d.val().Cmp(other.val())
}

// Equal returns true if d == other.
func (d Decimal) Equal(other Decimal) bool {
	return d.Cmp(other) == 0
}

// IsZero returns true if d == 0.
func (d Decimal) IsZero() bool {
	return d.val().Sign() == 0
}

// Sign returns -1, 0, or +1.
func (d Decimal) Sign() int {
	return d.val().Sign()
}

// String renders a normalized decimal string with minimal trailing fractional zeros (at least 0 decimals if integer, or trimmed).
func (d Decimal) String() string {
	v := d.val()
	if v.Sign() == 0 {
		return "0"
	}
	neg := v.Sign() < 0
	absV := new(big.Int).Abs(v)
	q, r := new(big.Int).QuoRem(absV, scaleFactor, new(big.Int))
	prefix := ""
	if neg {
		prefix = "-"
	}
	if r.Sign() == 0 {
		return prefix + q.String()
	}
	frac := fmt.Sprintf("%08s", r.String())
	frac = strings.TrimRight(frac, "0")
	return prefix + q.String() + "." + frac
}

// Float64ForMetrics converts to float64 solely for Prometheus gauge/histogram exposition.
// Never use this in reconciliation or financial arithmetic.
func (d Decimal) Float64ForMetrics() float64 {
	r := new(big.Rat).SetFrac(d.val(), scaleFactor)
	f, _ := r.Float64()
	return f
}

// MarshalJSON encodes Decimal as a JSON string to preserve exact fixed-point precision across wire boundaries.
func (d Decimal) MarshalJSON() ([]byte, error) {
	return json.Marshal(d.String())
}

// UnmarshalJSON decodes Decimal from either a JSON string ("12.34") or JSON number (12.34).
func (d *Decimal) UnmarshalJSON(data []byte) error {
	raw := strings.TrimSpace(string(data))
	if raw == "" || raw == "null" {
		*d = Zero()
		return nil
	}
	if strings.HasPrefix(raw, "\"") {
		var s string
		if err := json.Unmarshal(data, &s); err != nil {
			return err
		}
		parsed, err := ParseDecimal(s)
		if err != nil {
			return err
		}
		*d = parsed
		return nil
	}
	// Accept JSON number tokens by parsing their exact lexical representation without float64.
	parsed, err := ParseDecimal(raw)
	if err != nil {
		return err
	}
	*d = parsed
	return nil
}
