package unit_test

import (
	"testing"
	"time"

	"arbmonitor/src/domain"
	"arbmonitor/src/edgecalculator"
	"arbmonitor/src/fixed"
)

type staticFX map[string]domain.FXRate

func (s staticFX) GetFXRate(from, to string) (domain.FXRate, bool) {
	r, ok := s[from+"->"+to]
	return r, ok
}

func TestEdgeCalculatorDepthWalkingAndStopAtUnprofitableLevel(t *testing.T) {
	now := time.Date(2026, 11, 24, 5, 0, 0, 500_000_000, time.UTC)
	fees := edgecalculator.NewFeeSchedule([]domain.VenueFee{
		{Venue: "binance", Instrument: "BTC/USDT", TakerBps: fixed.MustParse("7.5"), MinFee: fixed.Zero},
		{Venue: "kraken", Instrument: "BTC/USDT", TakerBps: fixed.MustParse("12"), MinFee: fixed.MustParse("0.50")},
	})
	balances := edgecalculator.NewBalanceBook([]domain.VenueBalance{
		{Venue: "binance", Asset: "USDT", Available: fixed.FromInt(200000)},
		{Venue: "kraken", Asset: "BTC", Available: fixed.FromInt(5)},
	})

	calc := edgecalculator.New(fees, balances, nil, edgecalculator.DefaultTransferSchedule())
	spec := domain.InstrumentSpec{
		Canonical:   "BTC/USDT",
		Base:        "BTC",
		Quote:       "USDT",
		TickSize:    fixed.MustParse("0.01"),
		LotSize:     fixed.MustParse("0.001"),
		MinNotional: fixed.MustParse("100"),
		MaxDepth:    10,
	}

	buyBook := domain.VenueBook{
		Venue:      "binance",
		Instrument: "BTC/USDT",
		QuoteAsset: "USDT",
		Asks: []domain.PriceLevel{
			{Price: fixed.FromInt(65000), Qty: fixed.MustParse("0.40")},
			{Price: fixed.FromInt(65020), Qty: fixed.MustParse("0.60")},
			{Price: fixed.FromInt(65200), Qty: fixed.MustParse("2.00")}, // unprofitable vs 65210 after ~21bps costs
		},
		Bids:       []domain.PriceLevel{{Price: fixed.FromInt(64980), Qty: fixed.One}},
		Asof:       now.Add(-20 * time.Millisecond),
		ReceivedAt: now.Add(-10 * time.Millisecond),
		Seq:        100,
		Status:     domain.VenueStatusHealthy,
	}
	sellBook := domain.VenueBook{
		Venue:      "kraken",
		Instrument: "BTC/USDT",
		QuoteAsset: "USDT",
		Bids: []domain.PriceLevel{
			{Price: fixed.FromInt(65260), Qty: fixed.MustParse("0.50")},
			{Price: fixed.FromInt(65210), Qty: fixed.MustParse("0.50")},
			{Price: fixed.FromInt(64990), Qty: fixed.MustParse("2.00")}, // no longer crossed
		},
		Asks:       []domain.PriceLevel{{Price: fixed.FromInt(65290), Qty: fixed.One}},
		Asof:       now.Add(-15 * time.Millisecond),
		ReceivedAt: now.Add(-5 * time.Millisecond),
		Seq:        200,
		Status:     domain.VenueStatusHealthy,
	}

	cfg := domain.DefaultAlertConfig()
	res := calc.Evaluate(edgecalculator.EvalInput{
		BuyBook:  buyBook,
		SellBook: sellBook,
		BuySpec:  spec,
		SellSpec: spec,
		Config:   cfg,
		Now:      now,
	})

	if res.Suppressed != domain.SuppressNone || res.Opportunity == nil {
		t.Fatalf("expected valid opportunity, got suppressed=%s detail=%s", res.Suppressed, res.Detail)
	}
	opp := res.Opportunity
	// Should match exactly 0.40 + 0.10 + 0.50 = 1.00 BTC across 2 levels each side, stopping before level 3.
	if opp.Qty.String() != "1" {
		t.Fatalf("expected qty 1 BTC, got %s", opp.Qty)
	}
	if opp.BuyLevelsConsumed != 2 || opp.SellLevelsConsumed != 2 {
		t.Fatalf("expected 2 levels consumed on each side, got buy=%d sell=%d", opp.BuyLevelsConsumed, opp.SellLevelsConsumed)
	}
	// Executable notional = 0.4*65000 + 0.6*65020 = 26000 + 39012 = 65012.
	if opp.ExecutableNotional.String() != "65012" {
		t.Fatalf("expected executable notional 65012, got %s", opp.ExecutableNotional)
	}
	// Gross profit = (0.5*65260 + 0.5*65210) - 65012 = 65235 - 65012 = 223.
	if opp.GrossProfit.String() != "223" {
		t.Fatalf("expected gross profit 223, got %s", opp.GrossProfit)
	}
	if !opp.NetProfit.IsPositive() || !opp.NetEdgeBps.GreaterThan(cfg.MinNetBps) {
		t.Fatalf("expected positive net edge > %s bps, got net_profit=%s net_bps=%s", cfg.MinNetBps, opp.NetProfit, opp.NetEdgeBps)
	}
	if !opp.DepthSlippageBps.IsPositive() {
		t.Fatalf("expected positive depth walking slippage bps, got %s", opp.DepthSlippageBps)
	}
	if opp.TransferCost == nil || opp.TransferCost.IncludedInEdge {
		t.Fatalf("expected separated transfer cost estimate with IncludedInEdge=false, got %+v", opp.TransferCost)
	}
}

func TestEdgeCalculatorFXAndInventoryCap(t *testing.T) {
	now := time.Date(2026, 11, 24, 5, 0, 0, 500_000_000, time.UTC)
	fees := edgecalculator.NewFeeSchedule([]domain.VenueFee{
		{Venue: "binance", Instrument: "*", TakerBps: fixed.MustParse("7.5")},
		{Venue: "coinbase", Instrument: "*", TakerBps: fixed.MustParse("10")},
	})
	// Coinbase only holds 0.65 BTC pre-funded, capping the 1.5 BTC quoted depth.
	balances := edgecalculator.NewBalanceBook([]domain.VenueBalance{
		{Venue: "binance", Asset: "USDT", Available: fixed.FromInt(200000)},
		{Venue: "coinbase", Asset: "BTC", Available: fixed.MustParse("0.65")},
	})
	fx := staticFX{
		"USD->USDT": domain.FXRate{
			From:       "USD",
			To:         "USDT",
			Rate:       fixed.MustParse("1.0002"),
			SpreadBps:  fixed.MustParse("1.0"),
			Asof:       now,
			ReceivedAt: now,
		},
	}

	calc := edgecalculator.New(fees, balances, fx, edgecalculator.DefaultTransferSchedule())
	buySpec := domain.InstrumentSpec{Canonical: "BTC/USDT", Base: "BTC", Quote: "USDT", TickSize: fixed.MustParse("0.01"), LotSize: fixed.MustParse("0.001"), MinNotional: fixed.FromInt(100)}
	sellSpec := domain.InstrumentSpec{Canonical: "BTC/USD", Base: "BTC", Quote: "USD", TickSize: fixed.MustParse("0.01"), LotSize: fixed.MustParse("0.001"), MinNotional: fixed.FromInt(100)}

	buyBook := domain.VenueBook{
		Venue: "binance", Instrument: "BTC/USDT", QuoteAsset: "USDT",
		Asks: []domain.PriceLevel{{Price: fixed.FromInt(65000), Qty: fixed.MustParse("1.50")}},
		Asof: now, ReceivedAt: now, Seq: 1, Status: domain.VenueStatusHealthy,
	}
	sellBook := domain.VenueBook{
		Venue: "coinbase", Instrument: "BTC/USD", QuoteAsset: "USD",
		Bids: []domain.PriceLevel{{Price: fixed.FromInt(65250), Qty: fixed.MustParse("1.50")}},
		Asof: now, ReceivedAt: now, Seq: 1, Status: domain.VenueStatusHealthy,
	}

	res := calc.Evaluate(edgecalculator.EvalInput{
		BuyBook:  buyBook,
		SellBook: sellBook,
		BuySpec:  buySpec,
		SellSpec: sellSpec,
		Config:   domain.DefaultAlertConfig(),
		Now:      now,
	})
	if res.Suppressed != domain.SuppressNone || res.Opportunity == nil {
		t.Fatalf("expected valid FX opportunity, got suppressed=%s detail=%s", res.Suppressed, res.Detail)
	}
	if !res.Opportunity.InventoryCapped {
		t.Fatal("expected opportunity to be marked inventory_capped=true")
	}
	if res.Opportunity.Qty.String() != "0.65" {
		t.Fatalf("expected qty capped at 0.65 BTC, got %s", res.Opportunity.Qty)
	}
	if !res.Opportunity.FXCost.IsPositive() {
		t.Fatalf("expected positive FX spread cost, got %s", res.Opportunity.FXCost)
	}
}

func TestEdgeCalculatorFreshnessAndHealthSuppressions(t *testing.T) {
	now := time.Date(2026, 11, 24, 5, 0, 5, 0, time.UTC)
	fees := edgecalculator.NewFeeSchedule(nil)
	calc := edgecalculator.New(fees, nil, nil, nil)
	spec := domain.InstrumentSpec{Canonical: "BTC/USDT", Base: "BTC", Quote: "USDT", TickSize: fixed.MustParse("0.01"), LotSize: fixed.MustParse("0.001")}
	cfg := domain.DefaultAlertConfig()
	cfg.EnforceInventory = false

	baseBuy := domain.VenueBook{
		Venue: "binance", Instrument: "BTC/USDT", QuoteAsset: "USDT",
		Asks: []domain.PriceLevel{{Price: fixed.FromInt(65000), Qty: fixed.One}},
		Asof: now, ReceivedAt: now, Seq: 1, Status: domain.VenueStatusHealthy,
	}
	baseSell := domain.VenueBook{
		Venue: "kraken", Instrument: "BTC/USDT", QuoteAsset: "USDT",
		Bids: []domain.PriceLevel{{Price: fixed.FromInt(65400), Qty: fixed.One}},
		Asof: now, ReceivedAt: now, Seq: 1, Status: domain.VenueStatusHealthy,
	}

	// 1. Stale buy quote (> 750ms).
	staleBuy := baseBuy.Clone()
	staleBuy.ReceivedAt = now.Add(-900 * time.Millisecond)
	staleBuy.Asof = now.Add(-910 * time.Millisecond)
	res := calc.Evaluate(edgecalculator.EvalInput{BuyBook: staleBuy, SellBook: baseSell, BuySpec: spec, SellSpec: spec, Config: cfg, Now: now})
	if res.Suppressed != domain.SuppressStaleQuote {
		t.Fatalf("expected stale_quote suppression, got %s", res.Suppressed)
	}

	// 2. Clock skew (asof vs received_at > 1500ms).
	skewBuy := baseBuy.Clone()
	skewBuy.Asof = now.Add(-2000 * time.Millisecond)
	res = calc.Evaluate(edgecalculator.EvalInput{BuyBook: skewBuy, SellBook: baseSell, BuySpec: spec, SellSpec: spec, Config: cfg, Now: now})
	if res.Suppressed != domain.SuppressClockSkew {
		t.Fatalf("expected clock_skew suppression, got %s", res.Suppressed)
	}

	// 3. Inter-leg timestamp skew (> 500ms while both legs individually fresh).
	legBuy := baseBuy.Clone()
	legBuy.Asof = now.Add(-600 * time.Millisecond)
	legBuy.ReceivedAt = now.Add(-100 * time.Millisecond)
	res = calc.Evaluate(edgecalculator.EvalInput{BuyBook: legBuy, SellBook: baseSell, BuySpec: spec, SellSpec: spec, Config: cfg, Now: now})
	if res.Suppressed != domain.SuppressLegSkew {
		t.Fatalf("expected leg_timestamp_skew suppression, got %s", res.Suppressed)
	}

	// 4. Disconnected venue.
	discSell := baseSell.Clone()
	discSell.Status = domain.VenueStatusDisconnected
	res = calc.Evaluate(edgecalculator.EvalInput{BuyBook: baseBuy, SellBook: discSell, BuySpec: spec, SellSpec: spec, Config: cfg, Now: now})
	if res.Suppressed != domain.SuppressDisconnected {
		t.Fatalf("expected venue_disconnected suppression, got %s", res.Suppressed)
	}

	// 5. Missing bid side.
	emptySell := baseSell.Clone()
	emptySell.Bids = nil
	res = calc.Evaluate(edgecalculator.EvalInput{BuyBook: baseBuy, SellBook: emptySell, BuySpec: spec, SellSpec: spec, Config: cfg, Now: now})
	if res.Suppressed != domain.SuppressMissingSide {
		t.Fatalf("expected missing_book_side suppression, got %s", res.Suppressed)
	}
}

func BenchmarkEvaluateCrossBook(b *testing.B) {
	now := time.Date(2026, 11, 24, 5, 0, 0, 0, time.UTC)
	fees := edgecalculator.NewFeeSchedule([]domain.VenueFee{
		{Venue: "binance", Instrument: "*", TakerBps: fixed.MustParse("7.5")},
		{Venue: "kraken", Instrument: "*", TakerBps: fixed.MustParse("12")},
	})
	balances := edgecalculator.NewBalanceBook([]domain.VenueBalance{
		{Venue: "binance", Asset: "USDT", Available: fixed.FromInt(500000)},
		{Venue: "kraken", Asset: "BTC", Available: fixed.FromInt(10)},
	})
	calc := edgecalculator.New(fees, balances, nil, edgecalculator.DefaultTransferSchedule())
	spec := domain.InstrumentSpec{Canonical: "BTC/USDT", Base: "BTC", Quote: "USDT", TickSize: fixed.MustParse("0.01"), LotSize: fixed.MustParse("0.001"), MinNotional: fixed.FromInt(100)}
	cfg := domain.DefaultAlertConfig()

	buyBook := domain.VenueBook{
		Venue: "binance", Instrument: "BTC/USDT", QuoteAsset: "USDT",
		Asks: []domain.PriceLevel{
			{Price: fixed.FromInt(65000), Qty: fixed.MustParse("0.40")},
			{Price: fixed.FromInt(65015), Qty: fixed.MustParse("0.60")},
			{Price: fixed.FromInt(65040), Qty: fixed.MustParse("0.80")},
			{Price: fixed.FromInt(65100), Qty: fixed.MustParse("1.00")},
			{Price: fixed.FromInt(65200), Qty: fixed.MustParse("1.50")},
		},
		Asof: now, ReceivedAt: now, Seq: 1, Status: domain.VenueStatusHealthy,
	}
	sellBook := domain.VenueBook{
		Venue: "kraken", Instrument: "BTC/USDT", QuoteAsset: "USDT",
		Bids: []domain.PriceLevel{
			{Price: fixed.FromInt(65270), Qty: fixed.MustParse("0.50")},
			{Price: fixed.FromInt(65240), Qty: fixed.MustParse("0.50")},
			{Price: fixed.FromInt(65200), Qty: fixed.MustParse("0.70")},
			{Price: fixed.FromInt(65090), Qty: fixed.MustParse("1.00")},
			{Price: fixed.FromInt(64980), Qty: fixed.MustParse("1.50")},
		},
		Asof: now, ReceivedAt: now, Seq: 1, Status: domain.VenueStatusHealthy,
	}

	input := edgecalculator.EvalInput{
		BuyBook: buyBook, SellBook: sellBook,
		BuySpec: spec, SellSpec: spec,
		Config: cfg, Now: now,
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		res := calc.Evaluate(input)
		if res.Suppressed != domain.SuppressNone {
			b.Fatalf("unexpected suppression: %s", res.Suppressed)
		}
	}
}
