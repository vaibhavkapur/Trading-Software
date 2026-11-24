// Package fixed provides the fixed-point decimal used for every price, fee
// and cash amount in the OMS. Values are integer millionths (micros), so no
// floating-point arithmetic ever touches the execution path.
package fixed

import (
	"bytes"
	"errors"
	"fmt"
	"math"
	"math/bits"
	"strconv"
	"strings"
)

// Micros is a decimal amount scaled by 1e6. 1.25 is Micros(1_250_000).
type Micros int64

// Scale is the number of micros in one whole unit.
const Scale = 1_000_000

// fractionDigits is the number of decimal places a Micros can hold.
const fractionDigits = 6

// maxIntegerDigits bounds the whole-number part accepted by Parse so the
// scaled value always fits in an int64.
const maxIntegerDigits = 12

var (
	// ErrSyntax is returned for text that is not a plain decimal number.
	ErrSyntax = errors.New("not a plain decimal number")
	// ErrPrecision is returned when text has more than six decimal places.
	// Values are rejected rather than rounded.
	ErrPrecision = errors.New("more than 6 decimal places")
	// ErrRange is returned when a value does not fit.
	ErrRange = errors.New("value out of range")
)

// Parse converts plain decimal text ("99.5", "-0.000001") exactly. It accepts
// no exponent, no leading plus sign, no thousands separators and no
// whitespace, and it refuses to round.
func Parse(s string) (Micros, error) {
	orig := s
	neg := false
	if strings.HasPrefix(s, "-") {
		neg = true
		s = s[1:]
	}
	whole, frac, hasDot := strings.Cut(s, ".")
	if whole == "" || !allDigits(whole) || (hasDot && (frac == "" || !allDigits(frac))) {
		return 0, fmt.Errorf("%q: %w", orig, ErrSyntax)
	}
	if len(frac) > fractionDigits {
		// Trailing zeros carry no information, so "1.2500000" is still exact.
		if strings.Trim(frac[fractionDigits:], "0") != "" {
			return 0, fmt.Errorf("%q: %w", orig, ErrPrecision)
		}
		frac = frac[:fractionDigits]
	}
	whole = strings.TrimLeft(whole, "0")
	if len(whole) > maxIntegerDigits {
		return 0, fmt.Errorf("%q: %w", orig, ErrRange)
	}
	var w int64
	if whole != "" {
		w, _ = strconv.ParseInt(whole, 10, 64)
	}
	var f int64
	if frac != "" {
		f, _ = strconv.ParseInt(frac+strings.Repeat("0", fractionDigits-len(frac)), 10, 64)
	}
	v := w*Scale + f
	if neg {
		v = -v
	}
	return Micros(v), nil
}

// MustParse is Parse for constants in tests and examples. It panics on error.
func MustParse(s string) Micros {
	m, err := Parse(s)
	if err != nil {
		panic(err)
	}
	return m
}

func allDigits(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}

// String renders the value exactly with at least two decimal places.
func (m Micros) String() string {
	v := int64(m)
	var u uint64
	sign := ""
	if v < 0 {
		sign = "-"
		u = uint64(-(v + 1)) + 1 // safe for math.MinInt64
	} else {
		u = uint64(v)
	}
	frac := fmt.Sprintf("%06d", u%Scale)
	frac = strings.TrimRight(frac, "0")
	for len(frac) < 2 {
		frac += "0"
	}
	return sign + strconv.FormatUint(u/Scale, 10) + "." + frac
}

// MarshalJSON emits the value as a JSON string so that consumers never parse
// money through a binary float.
func (m Micros) MarshalJSON() ([]byte, error) {
	return []byte(`"` + m.String() + `"`), nil
}

// UnmarshalJSON accepts either a JSON number (99.50) or a JSON string
// ("99.50"). The number's source text is parsed directly, never via float64.
func (m *Micros) UnmarshalJSON(b []byte) error {
	b = bytes.TrimSpace(b)
	if len(b) >= 2 && b[0] == '"' && b[len(b)-1] == '"' {
		b = b[1 : len(b)-1]
	}
	v, err := Parse(string(b))
	if err != nil {
		return err
	}
	*m = v
	return nil
}

// MulQty returns price × qty, reporting false on overflow.
func MulQty(price Micros, qty int64) (Micros, bool) {
	if price == 0 || qty == 0 {
		return 0, true
	}
	neg := (price < 0) != (qty < 0)
	hi, lo := bits.Mul64(abs(int64(price)), abs(qty))
	if hi != 0 || lo > math.MaxInt64 {
		return 0, false
	}
	if neg {
		return Micros(-int64(lo)), true
	}
	return Micros(lo), true
}

// Add returns a + b, reporting false on overflow.
func Add(a, b Micros) (Micros, bool) {
	c := a + b
	if (c > a) != (b > 0) {
		return 0, false
	}
	return c, true
}

func abs(v int64) uint64 {
	if v < 0 {
		return uint64(-(v + 1)) + 1
	}
	return uint64(v)
}
