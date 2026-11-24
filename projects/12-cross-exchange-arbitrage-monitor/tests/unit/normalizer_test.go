package unit_test

import (
	"errors"
	"testing"
	"time"

	"arbmonitor/src/domain"
	"arbmonitor/src/fixed"
	"arbmonitor/src/normalizer"
)

func testRegistry(t *testing.T) *normalizer.Registry {
	t.Helper()
	reg, err := normalizer.NewRegistry([]domain.InstrumentSpec{
		{
			Canonical:   "BTC/USDT",
			Base:        "BTC",
			Quote:       "USDT",
			TickSize:    fixed.MustParse("0.10"),
			LotSize:     fixed.MustParse("0.01"),
			MinNotional: fixed.MustParse("100"),
			MaxDepth:    3,
			VenueSymbols: map[string]string{
				"binance": "BTCUSDT",
				"kraken":  "XBT/USDT",
			},
		},
		{
			Canonical:   "BTC/USD",
			Base:        "BTC",
			Quote:       "USD",
			TickSize:    fixed.MustParse("0.10"),
			LotSize:     fixed.MustParse("0.01"),
			MinNotional: fixed.MustParse("100"),
			MaxDepth:    3,
			VenueSymbols: map[string]string{
				"coinbase": "BTC-USD",
			},
		},
	})
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	return reg
}

func TestNormalizerConservativeRoundingSortingAndTopN(t *testing.T) {
	reg := testRegistry(t)
	norm := normalizer.New(reg, 2)
	now := time.Date(2026, 11, 24, 5, 0, 0, 0, time.UTC)

	book, err := norm.Normalize(normalizer.RawBookUpdate{
		Venue:       "kraken",
		VenueSymbol: "XBT/USDT",
		Asof:        now,
		ReceivedAt:  now.Add(10 * time.Millisecond),
		Seq:         42,
		IsSnapshot:  true,
		Bids: []normalizer.RawLevel{
			{Price: "65000.09", Qty: "0.509"}, // floors to 65000.00, qty 0.50
			{Price: "65000.02", Qty: "0.255"}, // floors to 65000.00, qty 0.25 -> aggregates to 0.75
			{Price: "64999.95", Qty: "1.00"},  // floors to 64999.90
			{Price: "64990.00", Qty: "2.00"},  // truncated by topN=2
		},
		Asks: []normalizer.RawLevel{
			{Price: "65001.01", Qty: "0.409"}, // ceils to 65001.10, qty 0.40
			{Price: "65002.00", Qty: "0.80"},
			{Price: "65005.00", Qty: "1.00"}, // truncated by topN=2
		},
	})
	if err != nil {
		t.Fatalf("Normalize failed: %v", err)
	}

	if book.Instrument != "BTC/USDT" {
		t.Fatalf("expected canonical BTC/USDT, got %s", book.Instrument)
	}
	if len(book.Bids) != 2 || len(book.Asks) != 2 {
		t.Fatalf("expected topN=2 on both sides, got bids=%d asks=%d", len(book.Bids), len(book.Asks))
	}
	if book.Bids[0].Price.String() != "65000" || book.Bids[0].Qty.String() != "0.75" {
		t.Fatalf("unexpected top bid: %+v", book.Bids[0])
	}
	if book.Asks[0].Price.String() != "65001.1" || book.Asks[0].Qty.String() != "0.4" {
		t.Fatalf("unexpected top ask: %+v", book.Asks[0])
	}
}

func TestNormalizerRejectsCrossedBookAndUnknownSymbol(t *testing.T) {
	reg := testRegistry(t)
	norm := normalizer.New(reg, 5)
	now := time.Date(2026, 11, 24, 5, 0, 0, 0, time.UTC)

	_, err := norm.Normalize(normalizer.RawBookUpdate{
		Venue:       "binance",
		VenueSymbol: "BTCUSDT",
		Asof:        now,
		ReceivedAt:  now,
		Bids:        []normalizer.RawLevel{{Price: "65010.00", Qty: "1.0"}},
		Asks:        []normalizer.RawLevel{{Price: "65000.00", Qty: "1.0"}},
	})
	if !errors.Is(err, normalizer.ErrCrossedVenueBook) {
		t.Fatalf("expected ErrCrossedVenueBook, got %v", err)
	}

	_, err = norm.Normalize(normalizer.RawBookUpdate{
		Venue:       "binance",
		VenueSymbol: "DOGEUSDT",
		Asof:        now,
		ReceivedAt:  now,
	})
	if !errors.Is(err, normalizer.ErrUnknownInstrument) {
		t.Fatalf("expected ErrUnknownInstrument, got %v", err)
	}
}
