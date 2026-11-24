package unit

import (
	"strings"
	"testing"
	"time"

	"riskengine/src/fixed"
	"riskengine/src/policy"
	"riskengine/tests/harness"
)

func TestPolicyAccountOverridesOverlayDefaults(t *testing.T) {
	p, err := policy.Parse([]byte(harness.DefaultPolicy))
	if err != nil {
		t.Fatal(err)
	}
	a1, _ := p.Limits("a-1")
	a2, _ := p.Limits("a-2")
	if a1.MaxOrderQty != 1000 || a2.MaxOrderQty != 200 {
		t.Errorf("max_order_qty: a-1=%d a-2=%d", a1.MaxOrderQty, a2.MaxOrderQty)
	}
	if a2.MaxOrderNotional != a1.MaxOrderNotional || a2.MaxSymbolExposure != fixed.MustParse("40000") {
		t.Errorf("a-2 should inherit notional and override symbol exposure: %+v", a2)
	}
	if a2.RateLimit.MaxOrders != 5 {
		t.Errorf("a-2 rate limit = %+v", a2.RateLimit)
	}
	if _, ok := p.Limits("nobody"); ok {
		t.Error("unlisted account must have no limits")
	}
	if len(p.Hash) != 64 || p.Version != "test-1" {
		t.Errorf("version/hash = %q/%q", p.Version, p.Hash)
	}
}

// A policy that is wrong in any way must fail to load as a whole.
func TestPolicyRejectsBrokenDocuments(t *testing.T) {
	mutate := func(old, new string) string {
		if !strings.Contains(harness.DefaultPolicy, old) {
			t.Fatalf("fixture no longer contains %q", old)
		}
		return strings.Replace(harness.DefaultPolicy, old, new, 1)
	}
	for name, doc := range map[string]string{
		"typo in limit name":      mutate(`"max_order_qty": 1000`, `"max_order_qtty": 1000`),
		"typo in account limit":   mutate(`"a-1": {}`, `"a-1": {"max_ordr_qty": 5}`),
		"missing limit":           mutate(`"max_daily_loss": "5000.00",`, ``),
		"zero limit":              mutate(`"max_order_qty": 1000`, `"max_order_qty": 0`),
		"negative account limit":  mutate(`"a-1": {}`, `"a-1": {"max_gross_exposure": "-1"}`),
		"float-ish money":         mutate(`"100000.00"`, `"1e5"`),
		"no version":              mutate(`"version": "test-1"`, `"version": ""`),
		"zero lot size":           mutate(`"lot_size": 10`, `"lot_size": 0`),
		"collar above 100%":       mutate(`"price_collar_bps": 500`, `"price_collar_bps": 10001`),
		"threshold beyond cap":    mutate(`"250000.00"`, `"99999999999999.00"`),
		"not json":                `{"version":`,
		"no accounts":             `{"version":"x","defaults":{},"accounts":{},"symbols":{}}`,
		"session closes too soon": mutate(`"symbols"`, `"session": {"open_utc":"20:00","close_utc":"13:30"}, "symbols"`),
	} {
		if _, err := policy.Parse([]byte(doc)); err == nil {
			t.Errorf("%s: policy loaded, want error", name)
		}
	}
}

func TestSessionWindow(t *testing.T) {
	doc := strings.Replace(harness.DefaultPolicy, `"symbols"`,
		`"session": {"open_utc":"13:30","close_utc":"20:00","weekdays":[1,2,3,4,5]}, "symbols"`, 1)
	p, err := policy.Parse([]byte(doc))
	if err != nil {
		t.Fatal(err)
	}
	mon := time.Date(2026, 1, 5, 0, 0, 0, 0, time.UTC)
	for _, c := range []struct {
		at   time.Time
		open bool
	}{
		{mon.Add(13*time.Hour + 29*time.Minute + 59*time.Second), false},
		{mon.Add(13*time.Hour + 30*time.Minute), true},
		{mon.Add(19*time.Hour + 59*time.Minute + 59*time.Second), true},
		{mon.Add(20 * time.Hour), false},
		{mon.Add(-24*time.Hour + 15*time.Hour), false}, // Sunday
		// 10:00 in New York on Monday is 15:00 UTC: zone must not matter.
		{time.Date(2026, 1, 5, 10, 0, 0, 0, time.FixedZone("EST", -5*3600)), true},
	} {
		if got := p.Session.IsOpen(c.at); got != c.open {
			t.Errorf("IsOpen(%s) = %v, want %v", c.at, got, c.open)
		}
	}
	var none *policy.Session
	if !none.IsOpen(mon) {
		t.Error("no session configured means always open")
	}
}
