package unit

import (
	"errors"
	"testing"
	"time"

	"riskengine/src/domain"
	"riskengine/src/fixed"
	"riskengine/src/market"
	"riskengine/tests/harness"
)

func quote(bid, ask string, at time.Time) market.Quote {
	return market.Quote{Symbol: "XYZ", Bid: fixed.MustParse(bid), Ask: fixed.MustParse(ask), EventTime: at}
}

func TestFreshnessBoundaries(t *testing.T) {
	now := harness.T0
	maxAge, skew := 2*time.Second, 500*time.Millisecond
	for _, c := range []struct {
		name  string
		event time.Duration // relative to now
		have  bool
		want  string
	}{
		{"missing", 0, false, domain.NoMarketData},
		{"current", 0, true, ""},
		{"exactly max age", -2 * time.Second, true, ""},
		{"1ms past max age", -2001 * time.Millisecond, true, domain.StaleQuote},
		{"ahead within skew", 500 * time.Millisecond, true, ""},
		{"ahead beyond skew", 501 * time.Millisecond, true, domain.QuoteClockSkew},
	} {
		if got := market.Freshness(quote("100", "100", now.Add(c.event)), c.have, now, maxAge, skew); got != c.want {
			t.Errorf("%s: got %q, want %q", c.name, got, c.want)
		}
	}
}

func TestCollarBoundaries(t *testing.T) {
	q := quote("99.99", "100.02", harness.T0) // mid 100.005: not representable, must still be exact
	for price, outside := range map[string]bool{
		"105.0052": false, // exactly mid * 1.05 = 105.00525, truncated input is inside
		"105.0053": true,
		"95.0048":  false, // mid * 0.95 = 95.00475
		"95.0047":  true,
		"100.00":   false,
	} {
		if got := market.OutsideCollar(fixed.MustParse(price), q, 500); got != outside {
			t.Errorf("price %s: outside=%v, want %v", price, got, outside)
		}
	}
}

func TestQuoteIngestRefusesBadWrites(t *testing.T) {
	s := market.NewStore(5 * time.Second)
	now := harness.T0
	if err := s.Update(quote("100", "100.02", now), now); err != nil {
		t.Fatal(err)
	}
	for name, c := range map[string]struct {
		q    market.Quote
		want error
	}{
		"crossed":      {quote("100.03", "100.02", now), market.ErrInvalidQuote},
		"zero bid":     {quote("0", "100.02", now), market.ErrInvalidQuote},
		"no timestamp": {quote("100", "100.02", time.Time{}), market.ErrInvalidQuote},
		"absurd price": {quote("100", "1000000.01", now), market.ErrInvalidQuote},
		"out of order": {quote("90", "90.02", now.Add(-time.Millisecond)), market.ErrOutOfOrder},
		"from future":  {quote("90", "90.02", now.Add(5001*time.Millisecond)), market.ErrFutureQuote},
	} {
		if err := s.Update(c.q, now); !errors.Is(err, c.want) {
			t.Errorf("%s: err = %v, want %v", name, err, c.want)
		}
	}
	// None of the refused writes may have displaced the good quote.
	if q, _ := s.Get("XYZ"); q.Bid != fixed.MustParse("100") || !q.ReceiveTime.Equal(now) {
		t.Errorf("good quote was displaced: %+v", q)
	}
}
