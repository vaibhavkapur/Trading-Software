// Package fixed converts provider decimal strings into exact integer counts
// of an instrument's tick or lot size. No floating point is used anywhere on
// this path: a value that is not an exact multiple of its unit is an error,
// never a rounded number.
package fixed

import (
	"errors"
	"fmt"
	"math/big"
	"strings"
)

var (
	ErrSyntax      = errors.New("not a plain decimal number")
	ErrNotMultiple = errors.New("value is not a multiple of the unit")
	ErrOverflow    = errors.New("value does not fit in 64 bits")
	ErrBadUnit     = errors.New("unit must be a positive decimal")
)

// maxDigits bounds input length so hostile frames cannot force huge big.Int work.
const maxDigits = 40

// Unit is a positive decimal step such as a tick size ("0.01") or lot size ("0.001").
type Unit struct {
	mant  *big.Int
	scale int
	text  string
}

// ParseUnit parses a tick or lot size.
func ParseUnit(s string) (Unit, error) {
	m, sc, err := parse(s)
	if err != nil {
		return Unit{}, fmt.Errorf("%w: %q", ErrBadUnit, s)
	}
	if m.Sign() <= 0 {
		return Unit{}, fmt.Errorf("%w: %q", ErrBadUnit, s)
	}
	return Unit{mant: m, scale: sc, text: s}, nil
}

func (u Unit) String() string { return u.text }

// Units returns value/unit when that quotient is an exact integer.
func (u Unit) Units(value string) (int64, error) {
	m, sc, err := parse(value)
	if err != nil {
		return 0, err
	}
	// value/unit = (m * 10^u.scale) / (u.mant * 10^sc)
	num := new(big.Int).Mul(m, pow10(u.scale))
	den := new(big.Int).Mul(u.mant, pow10(sc))
	q, r := new(big.Int).QuoRem(num, den, new(big.Int))
	if r.Sign() != 0 {
		return 0, fmt.Errorf("%w: %s / %s", ErrNotMultiple, value, u.text)
	}
	if !q.IsInt64() {
		return 0, fmt.Errorf("%w: %s / %s", ErrOverflow, value, u.text)
	}
	return q.Int64(), nil
}

// Format renders units*unit as a decimal string, for display only.
func (u Unit) Format(units int64) string {
	v := new(big.Int).Mul(big.NewInt(units), u.mant)
	return render(v, u.scale)
}

// FromScaled renders an integer with an implied decimal scale exactly, e.g.
// FromScaled(1012500, 4) == "101.25". Used for feeds that send scaled integers.
func FromScaled(n int64, scale int) string {
	return render(big.NewInt(n), scale)
}

func render(v *big.Int, scale int) string {
	neg := v.Sign() < 0
	digits := new(big.Int).Abs(v).String()
	if scale > 0 {
		if len(digits) <= scale {
			digits = strings.Repeat("0", scale-len(digits)+1) + digits
		}
		digits = digits[:len(digits)-scale] + "." + digits[len(digits)-scale:]
		digits = strings.TrimRight(digits, "0")
		digits = strings.TrimSuffix(digits, ".")
	}
	if neg && digits != "0" {
		return "-" + digits
	}
	return digits
}

// parse accepts [-]digits[.digits]. Exponents, signs other than a leading
// minus, whitespace, NaN and Inf are rejected.
func parse(s string) (*big.Int, int, error) {
	if s == "" || len(s) > maxDigits {
		return nil, 0, fmt.Errorf("%w: %q", ErrSyntax, s)
	}
	body := s
	neg := false
	if body[0] == '-' {
		neg = true
		body = body[1:]
	}
	intPart, frac, hasDot := strings.Cut(body, ".")
	if intPart == "" || (hasDot && frac == "") {
		return nil, 0, fmt.Errorf("%w: %q", ErrSyntax, s)
	}
	for _, part := range []string{intPart, frac} {
		for i := 0; i < len(part); i++ {
			if part[i] < '0' || part[i] > '9' {
				return nil, 0, fmt.Errorf("%w: %q", ErrSyntax, s)
			}
		}
	}
	m, ok := new(big.Int).SetString(intPart+frac, 10)
	if !ok {
		return nil, 0, fmt.Errorf("%w: %q", ErrSyntax, s)
	}
	if neg {
		m.Neg(m)
	}
	return m, len(frac), nil
}

func pow10(n int) *big.Int {
	return new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(n)), nil)
}
