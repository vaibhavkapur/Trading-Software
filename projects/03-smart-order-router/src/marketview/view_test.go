package marketview

import (
	"testing"

	"sor/src/domain"
)

var cfg = Config{StaleMs: 1000, SkewToleranceMs: 250, BreakerThreshold: 3, BreakerCooldownMs: 5000}

func quote(venue string, seq uint64, bid, ask, asof, recv int64) domain.VenueQuote {
	return domain.VenueQuote{Venue: venue, Symbol: "XYZ", Seq: seq, AsOfMs: asof, RecvMs: recv,
		Bids: []domain.Level{{Px: bid, Qty: 100}}, Asks: []domain.Level{{Px: ask, Qty: 100}}}
}

func status(v *View, name string, now int64) Status {
	for _, s := range v.Peek(now).Venues {
		if s.Venue == name {
			return s.Status
		}
	}
	return ""
}

func TestQuoteFreshnessAndOrdering(t *testing.T) {
	v := New(cfg, "XYZ")
	v.AddVenue("A", 30, 5)
	if status(v, "A", 0) != NoQuote {
		t.Fatal("want NO_QUOTE before first quote")
	}
	if !v.OnQuote(quote("A", 2, 99, 101, 1000, 1005)) {
		t.Fatal("quote rejected")
	}
	if v.OnQuote(quote("A", 2, 90, 91, 1100, 1105)) || v.OnQuote(quote("A", 1, 90, 91, 1100, 1105)) {
		t.Fatal("duplicate / out-of-order quote accepted")
	}
	if v.OnQuote(quote("ZZ", 1, 1, 2, 0, 0)) || v.OnQuote(domain.VenueQuote{Venue: "A", Symbol: "OTHER", Seq: 9}) {
		t.Fatal("quote for unknown venue or symbol accepted")
	}
	if s := v.Peek(1500).Venues[0]; s.Status != OK || s.Bids[0].Px != 99 || s.AsOfMs != 1000 || s.RecvMs != 1005 {
		t.Fatalf("state %+v", s)
	}
	if status(v, "A", 2001) != Stale {
		t.Fatal("want STALE once event time is older than stale_ms")
	}
	// Venue clock 5s ahead of our receive time: not trustworthy.
	v.OnQuote(quote("A", 3, 99, 101, 7000, 2000))
	if status(v, "A", 2000) != Skewed {
		t.Fatal("want SKEWED for future-dated quote")
	}
	v.SetLink("A", false)
	if status(v, "A", 2000) != Down {
		t.Fatal("want DOWN")
	}
}

func TestConsolidatedBBOIgnoresUnhealthyVenues(t *testing.T) {
	v := New(cfg, "XYZ")
	v.AddVenue("B", 0, 5)
	v.AddVenue("A", 0, 5)
	v.OnQuote(quote("A", 1, 99, 102, 1000, 1000))
	v.OnQuote(quote("B", 1, 100, 103, 1000, 1000))
	s := v.Snapshot(1000)
	if bid, ask, ok := s.BBO(); !ok || bid != 100 || ask != 102 {
		t.Fatalf("bbo %d/%d %v", bid, ask, ok)
	}
	if mid, _ := s.MidMills(); mid != 10100 {
		t.Fatalf("mid %d", mid)
	}
	if s.ID != "S-000001" || s.Venues[0].Venue != "A" {
		t.Fatalf("snapshot id %q / order %q", s.ID, s.Venues[0].Venue)
	}
	v.SetLink("B", false)
	if bid, _, _ := v.Peek(1000).BBO(); bid != 99 {
		t.Fatalf("down venue still in BBO: bid %d", bid)
	}
	v.SetLink("A", false)
	if _, ok := v.Peek(1000).MidMills(); ok {
		t.Fatal("mid available with no healthy venue")
	}
}

func TestCircuitBreaker(t *testing.T) {
	v := New(cfg, "XYZ")
	v.AddVenue("A", 0, 5)
	v.OnQuote(quote("A", 1, 99, 101, 100000, 100000))
	at := func(now int64) Status { return status(v, "A", now) }
	v.RecordOutcome("A", false, 0, 0, 100, 100000)
	v.RecordOutcome("A", false, 0, 0, 100, 100000)
	if at(100000) != OK {
		t.Fatal("tripped before threshold")
	}
	v.RecordOutcome("A", true, 20, 100, 100, 100000) // success resets the streak
	v.RecordOutcome("A", false, 0, 0, 100, 100000)
	v.RecordOutcome("A", false, 0, 0, 100, 100000)
	if at(100000) != OK {
		t.Fatal("streak was not reset by success")
	}
	v.RecordOutcome("A", false, 0, 0, 100, 100000)
	if at(100000) != Tripped || at(104999) != Tripped {
		t.Fatal("want TRIPPED during cooldown")
	}
	v.OnQuote(quote("A", 2, 99, 101, 105000, 105000))
	if at(105000) != OK {
		t.Fatal("want half-open (OK) after cooldown")
	}
	v.RecordOutcome("A", false, 0, 0, 100, 105000) // one failure while half-open re-trips
	if at(105001) != Tripped {
		t.Fatal("want re-trip on half-open failure")
	}
	v.OnQuote(quote("A", 3, 99, 101, 110000, 110000))
	v.RecordOutcome("A", true, 20, 100, 100, 110000)
	v.RecordOutcome("A", false, 0, 0, 100, 110000)
	if at(110000) != OK {
		t.Fatal("success while half-open should close the breaker")
	}
	if s := v.Peek(110000).Venues[0]; s.FillRatioBps >= 10000 || s.FillRatioBps <= 0 {
		t.Fatalf("fill ratio not tracked: %d", s.FillRatioBps)
	}
}
