package scenarios_test

import (
	"errors"
	"testing"
	"time"

	"arbmonitor/src/alertengine"
	"arbmonitor/src/connectors"
	"arbmonitor/src/domain"
	"arbmonitor/src/edgecalculator"
	"arbmonitor/src/fixed"
	"arbmonitor/src/metrics"
	"arbmonitor/src/normalizer"
	"arbmonitor/src/replay"
)

func TestFailureScenariosFixture(t *testing.T) {
	reg, err := normalizer.LoadRegistryFile("../../examples/config/instruments.json")
	if err != nil {
		t.Fatalf("load instruments: %v", err)
	}
	fees, err := edgecalculator.LoadFeeScheduleFile("../../examples/config/fees.json")
	if err != nil {
		t.Fatalf("load fees: %v", err)
	}
	balances, err := edgecalculator.LoadBalanceBookFile("../../examples/config/balances.json")
	if err != nil {
		t.Fatalf("load balances: %v", err)
	}
	events, err := connectors.ReadFeedJSONL("../../examples/fixtures/failure_scenarios.jsonl")
	if err != nil {
		t.Fatalf("read failure_scenarios.jsonl: %v", err)
	}

	cfg := domain.DefaultAlertConfig()
	cfg.Instruments = []string{"BTC/USDT"}

	rep, err := replay.Run(replay.Options{
		Events:   events,
		Registry: reg,
		Fees:     fees,
		Balances: balances,
		Config:   cfg,
	})
	if err != nil {
		t.Fatalf("replay.Run: %v", err)
	}

	// Verify all anomalous conditions occurred and were caught cleanly.
	if rep.DuplicatesIgnored != 1 {
		t.Fatalf("expected 1 duplicate ignored, got %d", rep.DuplicatesIgnored)
	}
	if rep.SequenceGaps != 1 {
		t.Fatalf("expected 1 sequence gap, got %d", rep.SequenceGaps)
	}
	if rep.Disconnects != 1 || rep.Reconnects != 1 {
		t.Fatalf("expected 1 disconnect and 1 reconnect, got disc=%d reconn=%d", rep.Disconnects, rep.Reconnects)
	}

	// Verify suppressions for missing side, stale quote, clock skew, and disconnect/resync.
	for _, want := range []domain.SuppressionReason{
		domain.SuppressMissingSide,
		domain.SuppressStaleQuote,
		domain.SuppressClockSkew,
		domain.SuppressDisconnected,
	} {
		if rep.Metrics.Suppressions[want] == 0 {
			t.Fatalf("expected suppression reason %q to be recorded, got map=%+v", want, rep.Metrics.Suppressions)
		}
	}

	// Exactly 1 alert should be emitted — ONLY after reconnect + fresh snapshot recovery!
	if len(rep.Alerts) != 1 {
		t.Fatalf("expected exactly 1 alert after full recovery, got %d", len(rep.Alerts))
	}
	if rep.Alerts[0].Opportunity.SellBookSeq != 30 {
		t.Fatalf("expected recovered alert to use post-reconnect kraken snapshot seq 30, got %d", rep.Alerts[0].Opportunity.SellBookSeq)
	}
}

func TestDeterministicReplayReproducibilityAndInvariants(t *testing.T) {
	reg, err := normalizer.LoadRegistryFile("../../examples/config/instruments.json")
	if err != nil {
		t.Fatalf("load instruments: %v", err)
	}
	fees, err := edgecalculator.LoadFeeScheduleFile("../../examples/config/fees.json")
	if err != nil {
		t.Fatalf("load fees: %v", err)
	}
	balances, err := edgecalculator.LoadBalanceBookFile("../../examples/config/balances.json")
	if err != nil {
		t.Fatalf("load balances: %v", err)
	}
	events, err := connectors.ReadFeedJSONL("../../examples/fixtures/recorded_books.jsonl")
	if err != nil {
		t.Fatalf("read recorded_books.jsonl: %v", err)
	}

	cfg := domain.DefaultAlertConfig()
	cfg.Instruments = []string{"BTC/USDT", "BTC/USD", "ETH/USDT"}

	opts := replay.Options{
		Events:             events,
		Registry:           reg,
		Fees:               fees,
		Balances:           balances,
		Config:             cfg,
		AdverseLookaheadMs: 500,
	}

	r1, err := replay.Run(opts)
	if err != nil {
		t.Fatalf("run 1 failed: %v", err)
	}
	r2, err := replay.Run(opts)
	if err != nil {
		t.Fatalf("run 2 failed: %v", err)
	}

	if r1.DigestSHA256 == "" || r1.DigestSHA256 != r2.DigestSHA256 {
		t.Fatalf("replay not deterministic: digest1=%s digest2=%s", r1.DigestSHA256, r2.DigestSHA256)
	}
	if len(r1.Alerts) < 3 {
		t.Fatalf("expected at least 3 alerts from recorded_books.jsonl, got %d", len(r1.Alerts))
	}

	// Definition of done invariant checks on every emitted alert.
	for _, a := range r1.Alerts {
		opp := a.Opportunity
		if opp.Qty.LessThanOrEqual(fixed.Zero) {
			t.Fatalf("alert %s has non-positive qty", a.ID)
		}
		// Executable qty must never exceed quoted depth on either leg.
		buyQuoted := fixed.Zero
		for _, lvl := range a.Evidence.BuyBookSnapshot.Asks {
			buyQuoted = buyQuoted.Add(lvl.Qty)
		}
		sellQuoted := fixed.Zero
		for _, lvl := range a.Evidence.SellBookSnapshot.Bids {
			sellQuoted = sellQuoted.Add(lvl.Qty)
		}
		if opp.Qty.GreaterThan(buyQuoted) || opp.Qty.GreaterThan(sellQuoted) {
			t.Fatalf("alert %s qty %s exceeds quoted depth buy=%s sell=%s", a.ID, opp.Qty, buyQuoted, sellQuoted)
		}
		if opp.LegSkewMs > cfg.MaxLegSkewMs {
			t.Fatalf("alert %s violated max_leg_skew_ms: %d > %d", a.ID, opp.LegSkewMs, cfg.MaxLegSkewMs)
		}
		if opp.BuyAgeMs > cfg.MaxQuoteAgeMs || opp.SellAgeMs > cfg.MaxQuoteAgeMs {
			t.Fatalf("alert %s violated max_quote_age_ms: buy=%d sell=%d", a.ID, opp.BuyAgeMs, opp.SellAgeMs)
		}
	}
}

type flakyStore struct {
	failNext bool
	inner    *alertengine.MemoryAlertStore
}

func (f *flakyStore) Append(a domain.Alert) error {
	if f.failNext {
		return errors.New("simulated disk full / rejected write")
	}
	return f.inner.Append(a)
}

func (f *flakyStore) List() ([]domain.Alert, error) {
	return f.inner.List()
}

func TestRejectedStoreWriteDoesNotSwallowErrorOrArmCooldown(t *testing.T) {
	reg, err := normalizer.LoadRegistryFile("../../examples/config/instruments.json")
	if err != nil {
		t.Fatalf("load instruments: %v", err)
	}
	fees, err := edgecalculator.LoadFeeScheduleFile("../../examples/config/fees.json")
	if err != nil {
		t.Fatalf("load fees: %v", err)
	}
	balances, err := edgecalculator.LoadBalanceBookFile("../../examples/config/balances.json")
	if err != nil {
		t.Fatalf("load balances: %v", err)
	}

	cfg := domain.DefaultAlertConfig()
	cfg.Instruments = []string{"BTC/USDT"}
	cfg.CooldownMs = 60_000 // long cooldown to verify failed write does NOT arm cooldown

	norm := normalizer.New(reg, cfg.MaxDepthLevels)
	feeds := connectors.NewFeedManager(connectors.NewDefaultAdapters(), norm)
	calc := edgecalculator.New(fees, balances, feeds, nil)
	collector := metrics.New()
	store := &flakyStore{failNext: true, inner: alertengine.NewMemoryAlertStore()}
	engine := alertengine.New(feeds, reg, calc, store, collector, cfg)

	now := time.Date(2026, 11, 24, 5, 0, 0, 0, time.UTC)
	feeds.Ingest(connectors.FeedEvent{
		Type:       connectors.EventBookUpdate,
		Venue:      "binance",
		ReceivedAt: now,
		BookUpdate: &normalizer.RawBookUpdate{
			Venue: "binance", VenueSymbol: "BTCUSDT", Asof: now, ReceivedAt: now, Seq: 1, IsSnapshot: true,
			Bids: []normalizer.RawLevel{{Price: "64980", Qty: "1"}},
			Asks: []normalizer.RawLevel{{Price: "65000", Qty: "1"}},
		},
	})
	feeds.Ingest(connectors.FeedEvent{
		Type:       connectors.EventBookUpdate,
		Venue:      "kraken",
		ReceivedAt: now,
		BookUpdate: &normalizer.RawBookUpdate{
			Venue: "kraken", VenueSymbol: "XBT/USDT", Asof: now, ReceivedAt: now, Seq: 1, IsSnapshot: true,
			Bids: []normalizer.RawLevel{{Price: "65300", Qty: "1"}},
			Asks: []normalizer.RawLevel{{Price: "65320", Qty: "1"}},
		},
	})

	// First evaluation should fail persistence and increment StoreWriteErrors.
	emitted, err := engine.EvaluateAll(now)
	if err == nil {
		t.Fatal("expected error on rejected store write")
	}
	if len(emitted) != 0 {
		t.Fatalf("expected 0 emitted alerts on rejected write, got %d", len(emitted))
	}
	if collector.Snapshot().StoreWriteErrors != 1 {
		t.Fatalf("expected StoreWriteErrors=1, got %d", collector.Snapshot().StoreWriteErrors)
	}

	// Once storage recovers 10ms later (well inside 60s cooldown), retry MUST succeed!
	store.failNext = false
	emitted, err = engine.EvaluateAll(now.Add(10 * time.Millisecond))
	if err != nil {
		t.Fatalf("unexpected error after store recovery: %v", err)
	}
	if len(emitted) != 1 {
		t.Fatalf("expected 1 emitted alert after store recovery, got %d", len(emitted))
	}
}
