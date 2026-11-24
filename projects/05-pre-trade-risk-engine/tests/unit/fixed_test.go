package unit

import (
	"encoding/json"
	"math"
	"testing"

	"riskengine/src/fixed"
)

func TestDecimalParseAndFormat(t *testing.T) {
	for in, want := range map[string]string{
		"105.20": "105.2000", "0.0001": "0.0001", "7": "7.0000", "-3.5": "-3.5000",
		"922337203685477.5807": "922337203685477.5807",
	} {
		d, err := fixed.Parse(in)
		if err != nil || d.String() != want {
			t.Errorf("Parse(%q) = %v, %v; want %s", in, d, err, want)
		}
	}
}

func TestDecimalRejectsAnythingThatWouldRound(t *testing.T) {
	for _, in := range []string{"", ".", "1.", ".5", "1.00001", "1e2", "1,000", " 1", "+1", "--1", "0x10", "NaN",
		"922337203685477.5808", "99999999999999999999"} {
		if d, err := fixed.Parse(in); err == nil {
			t.Errorf("Parse(%q) = %v; want error", in, d)
		}
	}
}

func TestDecimalJSONNeverUsesFloat(t *testing.T) {
	var v struct{ P fixed.Decimal }
	// 0.1+0.2 style values and a number float64 cannot hold exactly.
	for in, want := range map[string]fixed.Decimal{
		`{"P":105.20}`: 1052000, `{"P":"105.20"}`: 1052000, `{"P":0.3}`: 3000, `{"P":90071992547409.93}`: 900719925474099300,
	} {
		if err := json.Unmarshal([]byte(in), &v); err != nil || v.P != want {
			t.Errorf("%s => %d, %v; want %d", in, v.P, err, want)
		}
	}
	for _, in := range []string{`{"P":1.05e2}`, `{"P":null}`, `{"P":true}`, `{"P":1.23456}`} {
		if err := json.Unmarshal([]byte(in), &v); err == nil {
			t.Errorf("%s: want error", in)
		}
	}
	out, _ := json.Marshal(fixed.Decimal(1052000))
	if string(out) != `"105.2000"` {
		t.Errorf("marshal = %s", out)
	}
}

func TestArithmeticSaturatesInsteadOfWrapping(t *testing.T) {
	if got := fixed.MulQty(fixed.Max, 2); got != fixed.Max {
		t.Errorf("MulQty overflow = %d", got)
	}
	if got := fixed.MulQty(fixed.Max, -2); got != -fixed.Max {
		t.Errorf("MulQty negative overflow = %d", got)
	}
	if got := fixed.Add(fixed.Max, 1); got != fixed.Max {
		t.Errorf("Add overflow = %d", got)
	}
	if got := fixed.Sub(-fixed.Max, 10); got != -fixed.Max {
		t.Errorf("Sub overflow = %d", got)
	}
	if got := fixed.Abs(math.MinInt64); got != fixed.Max {
		t.Errorf("Abs(min) = %d", got)
	}
	if got := fixed.MulQty(fixed.MustParse("105.20"), 100); got.String() != "10520.0000" {
		t.Errorf("MulQty = %s", got)
	}
}
