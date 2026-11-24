package unit

import (
	"encoding/json"
	"errors"
	"math"
	"testing"

	"oms/src/fixed"
)

func TestFixed_ParseIsExact(t *testing.T) {
	good := map[string]fixed.Micros{
		"0":                   0,
		"1":                   1_000_000,
		"150":                 150_000_000,
		"150.00":              150_000_000,
		"0.000001":            1,
		"-0.000001":           -1,
		"99.5":                99_500_000,
		"0.1":                 100_000, // exact, unlike the float 0.1
		"1.2500000":           1_250_000,
		"007":                 7_000_000,
		"999999999999.999999": 999_999_999_999_999_999,
		"-12.34":              -12_340_000,
	}
	for in, want := range good {
		got, err := fixed.Parse(in)
		if err != nil || got != want {
			t.Errorf("Parse(%q) = %d, %v; want %d", in, got, err, want)
		}
	}
	bad := map[string]error{
		"":               fixed.ErrSyntax,
		".":              fixed.ErrSyntax,
		"1.":             fixed.ErrSyntax,
		".5":             fixed.ErrSyntax,
		"+1":             fixed.ErrSyntax,
		"1e3":            fixed.ErrSyntax,
		"1,000":          fixed.ErrSyntax,
		" 1":             fixed.ErrSyntax,
		"1 ":             fixed.ErrSyntax,
		"--1":            fixed.ErrSyntax,
		"NaN":            fixed.ErrSyntax,
		"0x10":           fixed.ErrSyntax,
		"1.0000001":      fixed.ErrPrecision, // refused, not rounded
		"0.0000009":      fixed.ErrPrecision,
		"1000000000000":  fixed.ErrRange, // 13 integer digits
		"-1000000000000": fixed.ErrRange,
	}
	for in, want := range bad {
		if _, err := fixed.Parse(in); !errors.Is(err, want) {
			t.Errorf("Parse(%q) error = %v, want %v", in, err, want)
		}
	}
}

func TestFixed_StringRoundTrips(t *testing.T) {
	cases := map[fixed.Micros]string{
		0:                   "0.00",
		1:                   "0.000001",
		-1:                  "-0.000001",
		1_000_000:           "1.00",
		1_500_000:           "1.50",
		1_234_560:           "1.23456",
		-150_250_000:        "-150.25",
		math.MaxInt64:       "9223372036854.775807",
		math.MinInt64:       "-9223372036854.775808",
		999_999_999_999_999: "999999999.999999",
	}
	for v, want := range cases {
		if got := v.String(); got != want {
			t.Errorf("%d.String() = %q, want %q", int64(v), got, want)
		}
	}
	for _, s := range []string{"0.00", "150.25", "-0.000001", "999999999999.999999", "12.50"} {
		v := fixed.MustParse(s)
		if back, err := fixed.Parse(v.String()); err != nil || back != v {
			t.Errorf("%q does not round-trip: %q", s, v.String())
		}
	}
}

func TestFixed_JSONNeverGoesThroughAFloat(t *testing.T) {
	// Marshals as a string.
	b, _ := json.Marshal(struct {
		P fixed.Micros `json:"p"`
	}{fixed.MustParse("150.10")})
	if string(b) != `{"p":"150.10"}` {
		t.Errorf("marshal = %s", b)
	}
	// Accepts a string or a number, both parsed from their text. 0.1 + 0.2
	// is exactly 0.3 here.
	var v struct{ A, B, C fixed.Micros }
	if err := json.Unmarshal([]byte(`{"A": 0.1, "B": "0.2", "C": 0.30}`), &v); err != nil {
		t.Fatal(err)
	}
	if sum, ok := fixed.Add(v.A, v.B); !ok || sum != v.C {
		t.Errorf("0.1 + 0.2 = %s, want %s", sum, v.C)
	}
	for _, in := range []string{`{"A": 1e2}`, `{"A": "1e2"}`, `{"A": 0.1234567}`, `{"A": true}`, `{"A": null}`, `{"A": ""}`, `{"A": "abc"}`} {
		var x struct{ A fixed.Micros }
		if err := json.Unmarshal([]byte(in), &x); err == nil {
			t.Errorf("unmarshal %s succeeded with %d", in, x.A)
		}
	}
}

func TestFixed_ArithmeticReportsOverflow(t *testing.T) {
	mul := []struct {
		p    fixed.Micros
		q    int64
		want fixed.Micros
		ok   bool
	}{
		{fixed.MustParse("150.25"), 100, fixed.MustParse("15025"), true},
		{fixed.MustParse("0.000001"), 3, 3, true},
		{fixed.MustParse("-2"), 3, fixed.MustParse("-6"), true},
		{fixed.MustParse("2"), -3, fixed.MustParse("-6"), true},
		{fixed.MustParse("-2"), -3, fixed.MustParse("6"), true},
		{0, math.MaxInt64, 0, true},
		{math.MaxInt64, 1, math.MaxInt64, true},
		{math.MaxInt64, 2, 0, false},
		{fixed.MustParse("1000000"), 1_000_000_000, 0, false}, // 1e21 micros
		{fixed.MustParse("9223372.036854"), 1_000_000, 9_223_372_036_854_000_000, true},
		{fixed.MustParse("9223372.036855"), 1_000_000, 0, false},
		{math.MinInt64, 1, 0, false}, // |MinInt64| does not fit
	}
	for _, c := range mul {
		if got, ok := fixed.MulQty(c.p, c.q); ok != c.ok || (ok && got != c.want) {
			t.Errorf("MulQty(%d, %d) = %d, %v; want %d, %v", c.p, c.q, got, ok, c.want, c.ok)
		}
	}
	add := []struct {
		a, b fixed.Micros
		ok   bool
	}{
		{1, 2, true}, {-1, 1, true}, {math.MaxInt64, 0, true}, {math.MinInt64, 0, true},
		{math.MaxInt64, 1, false}, {math.MinInt64, -1, false}, {math.MaxInt64, math.MinInt64, true},
		{math.MaxInt64 - 5, 5, true}, {math.MaxInt64 - 5, 6, false},
	}
	for _, c := range add {
		if got, ok := fixed.Add(c.a, c.b); ok != c.ok || (ok && got != c.a+c.b) {
			t.Errorf("Add(%d, %d) = %d, %v; want ok=%v", c.a, c.b, got, ok, c.ok)
		}
	}
}
