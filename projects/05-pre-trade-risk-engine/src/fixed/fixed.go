// Package fixed implements the fixed-point decimal used for every price and
// notional in the engine. Values are int64 counts of 1e-4 currency units, so
// no floating-point arithmetic ever touches the decision path.
package fixed

import (
	"errors"
	"math"
	"math/bits"
	"strconv"
	"strings"
)

// Scale is the number of Decimal units per whole currency unit.
const Scale = 10000

const scaleDigits = 4

// Decimal is a fixed-point number with four decimal places.
type Decimal int64

// Max is the saturation value. Arithmetic helpers clamp to ±Max instead of
// wrapping, and policy thresholds are capped well below it, so a saturated
// value always breaches its limit rather than slipping under it.
const Max = Decimal(math.MaxInt64)

var ErrSyntax = errors.New("invalid decimal: want digits with at most 4 decimal places, no exponent")
var ErrRange = errors.New("decimal out of range")

// Parse converts text such as "105.20" exactly. It rejects exponents, more
// than four decimal places and anything that would need rounding.
func Parse(s string) (Decimal, error) {
	if s == "" {
		return 0, ErrSyntax
	}
	neg := false
	if s[0] == '-' {
		neg = true
		s = s[1:]
	}
	intPart, fracPart, hasDot := strings.Cut(s, ".")
	if intPart == "" || (hasDot && fracPart == "") || len(fracPart) > scaleDigits {
		return 0, ErrSyntax
	}
	var v uint64
	digits := intPart + fracPart + strings.Repeat("0", scaleDigits-len(fracPart))
	for i := 0; i < len(digits); i++ {
		c := digits[i]
		if c < '0' || c > '9' {
			return 0, ErrSyntax
		}
		if v > (math.MaxInt64-uint64(c-'0'))/10 {
			return 0, ErrRange
		}
		v = v*10 + uint64(c-'0')
	}
	if neg {
		return -Decimal(v), nil
	}
	return Decimal(v), nil
}

// MustParse is for constants in tests and examples.
func MustParse(s string) Decimal {
	d, err := Parse(s)
	if err != nil {
		panic(err)
	}
	return d
}

func (d Decimal) String() string {
	neg := d < 0
	u := uint64(d)
	if neg {
		u = -u
	}
	s := strconv.FormatUint(u/Scale, 10) + "." + pad4(u%Scale)
	if neg {
		return "-" + s
	}
	return s
}

func pad4(v uint64) string {
	s := strconv.FormatUint(v, 10)
	return strings.Repeat("0", scaleDigits-len(s)) + s
}

// MarshalJSON emits a quoted decimal string so clients never round-trip
// money through a binary float.
func (d Decimal) MarshalJSON() ([]byte, error) {
	return []byte(`"` + d.String() + `"`), nil
}

// UnmarshalJSON accepts a JSON number (105.20) or a string ("105.20"). The
// number's source text is parsed directly; it is never converted to float64.
func (d *Decimal) UnmarshalJSON(b []byte) error {
	s := string(b)
	if len(s) >= 2 && s[0] == '"' && s[len(s)-1] == '"' {
		s = s[1 : len(s)-1]
	}
	v, err := Parse(s)
	if err != nil {
		return err
	}
	*d = v
	return nil
}

// Add returns a+b, saturating at ±Max.
func Add(a, b Decimal) Decimal {
	s := a + b
	if a > 0 && b > 0 && s < 0 {
		return Max
	}
	if a < 0 && b < 0 && s >= 0 {
		return -Max
	}
	return s
}

// Sub returns a-b, saturating at ±Max.
func Sub(a, b Decimal) Decimal {
	if b == math.MinInt64 {
		return Max
	}
	return Add(a, -b)
}

// Abs returns |d|, saturating.
func Abs(d Decimal) Decimal {
	if d == math.MinInt64 {
		return Max
	}
	if d < 0 {
		return -d
	}
	return d
}

// MulQty returns price*qty (a notional), saturating at ±Max.
func MulQty(price Decimal, qty int64) Decimal {
	return Decimal(MulInt(int64(price), qty))
}

// MulInt returns a*b, saturating at ±MaxInt64.
func MulInt(a, b int64) int64 {
	neg := (a < 0) != (b < 0)
	hi, lo := bits.Mul64(absU(a), absU(b))
	if hi != 0 || lo > math.MaxInt64 {
		if neg {
			return -math.MaxInt64
		}
		return math.MaxInt64
	}
	if neg {
		return -int64(lo)
	}
	return int64(lo)
}

func absU(v int64) uint64 {
	if v < 0 {
		return -uint64(v)
	}
	return uint64(v)
}
