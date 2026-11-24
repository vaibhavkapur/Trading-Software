package unit_test

import (
	"encoding/json"
	"testing"

	"arbmonitor/src/fixed"
)

func TestFixedParseAndArithmetic(t *testing.T) {
	a := fixed.MustParse("65000.25")
	b := fixed.MustParse("0.4")
	prod := a.Mul(b)
	if prod.String() != "26000.1" {
		t.Fatalf("expected 26000.1, got %s", prod.String())
	}

	quo, err := prod.Div(b)
	if err != nil {
		t.Fatalf("unexpected div error: %v", err)
	}
	if !quo.Equal(a) {
		t.Fatalf("expected %s, got %s", a, quo)
	}

	// Conservative fee rounding up on MulBps.
	notional := fixed.MustParse("100.01")
	fee := notional.MulBps(fixed.MustParse("7.5"), true)
	// Exact: 100.01 * 0.00075 = 0.0750075
	if fee.String() != "0.0750075" {
		t.Fatalf("expected 0.0750075, got %s", fee.String())
	}

	// RatioToBps: 65 / 65000 = 10 bps
	bps, err := fixed.RatioToBps(fixed.FromInt(65), fixed.FromInt(65000))
	if err != nil {
		t.Fatalf("unexpected RatioToBps error: %v", err)
	}
	if bps.String() != "10" {
		t.Fatalf("expected 10 bps, got %s", bps.String())
	}
}

func TestFixedTickLotRoundingAndJSON(t *testing.T) {
	tick := fixed.MustParse("0.05")
	p := fixed.MustParse("65000.07")
	if got := p.FloorToStep(tick); got.String() != "65000.05" {
		t.Fatalf("FloorToStep expected 65000.05, got %s", got)
	}
	if got := p.CeilToStep(tick); got.String() != "65000.1" {
		t.Fatalf("CeilToStep expected 65000.1, got %s", got)
	}

	// JSON string and bare number decoding.
	type box struct {
		A fixed.Decimal `json:"a"`
		B fixed.Decimal `json:"b"`
	}
	var dst box
	if err := json.Unmarshal([]byte(`{"a":"123.456","b":78.9}`), &dst); err != nil {
		t.Fatalf("json unmarshal failed: %v", err)
	}
	if dst.A.String() != "123.456" || dst.B.String() != "78.9" {
		t.Fatalf("unexpected decoded values: %+v", dst)
	}

	// Invalid inputs must fail.
	for _, bad := range []string{"", "NaN", "1e5", "12.3.4", "abc", "-"} {
		if _, err := fixed.Parse(bad); err == nil {
			t.Fatalf("expected error parsing %q", bad)
		}
	}
	if _, err := fixed.One.Div(fixed.Zero); err == nil {
		t.Fatal("expected division by zero error")
	}
}
