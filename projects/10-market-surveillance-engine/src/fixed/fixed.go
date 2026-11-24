// Package fixed holds prices as integers so that no price ever passes
// through a float. One unit is 1/10000 of the quote currency.
package fixed

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

// Price is a positive price in units of 0.0001.
type Price int64

const (
	Scale        = 10000
	decimals     = 4
	maxIntDigits = 10
)

var (
	ErrEmpty     = errors.New("price is empty")
	ErrFormat    = errors.New("price must be a plain decimal such as 101.25 (no sign, exponent or spaces)")
	ErrPrecision = errors.New("price has more than 4 decimal places")
	ErrRange     = errors.New("price is out of range")
	ErrZero      = errors.New("price must be greater than zero")
)

// Parse reads a decimal literal exactly. Anything that would need rounding
// or interpretation (exponents, signs, more than four decimals) is refused.
func Parse(s string) (Price, error) {
	if s == "" {
		return 0, ErrEmpty
	}
	whole, frac, hasDot := strings.Cut(s, ".")
	if whole == "" || (hasDot && frac == "") {
		return 0, ErrFormat
	}
	if !digits(whole) || !digits(frac) {
		return 0, ErrFormat
	}
	if len(frac) > decimals {
		return 0, ErrPrecision
	}
	if len(whole) > maxIntDigits {
		return 0, ErrRange
	}
	var v int64
	for _, c := range whole {
		v = v*10 + int64(c-'0')
	}
	for i := 0; i < decimals; i++ {
		v *= 10
		if i < len(frac) {
			v += int64(frac[i] - '0')
		}
	}
	if v == 0 {
		return 0, ErrZero
	}
	return Price(v), nil
}

// MustParse is for constants in tests and generators.
func MustParse(s string) Price {
	p, err := Parse(s)
	if err != nil {
		panic(fmt.Sprintf("fixed.MustParse(%q): %v", s, err))
	}
	return p
}

func digits(s string) bool {
	for _, c := range s {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}

func (p Price) String() string {
	return fmt.Sprintf("%d.%04d", int64(p)/Scale, int64(p)%Scale)
}

func (p Price) MarshalJSON() ([]byte, error) {
	return json.Marshal(p.String())
}

// UnmarshalJSON accepts "101.25" or the bare literal 101.25. The literal
// text is parsed directly, never through a float.
func (p *Price) UnmarshalJSON(b []byte) error {
	s := string(b)
	if len(s) > 0 && s[0] == '"' {
		if err := json.Unmarshal(b, &s); err != nil {
			return err
		}
	}
	v, err := Parse(s)
	if err != nil {
		return err
	}
	*p = v
	return nil
}
