package contract

import (
	"context"
	"fmt"
	"reflect"
	"testing"
	"time"

	"mdp/src/domain"
	"mdp/src/storage"
)

func meta(provider string, seq uint64, flags ...string) domain.Meta {
	return domain.Meta{Provider: provider, Session: "S1", SeqDomain: "main", Seq: seq,
		Source: domain.SourceLive, NormalizerVersion: domain.NormalizerVersion, Flags: flags}
}

// Trade builds a trade for store tests.
func Trade(id string, at time.Time, tradeID string, seq uint64) domain.Trade {
	return domain.Trade{Type: domain.KindTrade, InstrumentID: id, TradeID: tradeID, PriceTicks: 10000 + int64(seq),
		Quantity: 5, Aggressor: "buy", EventTime: at, ReceiveTime: at.Add(321 * time.Microsecond), Meta: meta("beta", seq)}
}

// Quote builds a quote for store tests.
func Quote(id string, at time.Time, seq uint64) domain.Quote {
	return domain.Quote{Type: domain.KindQuote, InstrumentID: id, BidTicks: 9999, BidSize: 3, AskTicks: 10001, AskSize: 4,
		EventTime: at, ReceiveTime: at.Add(321 * time.Microsecond), Meta: meta("beta", seq)}
}

// RunStore checks a storage.Store. The store must be empty and migrated.
func RunStore(t *testing.T, store storage.Store) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	t.Cleanup(cancel)
	// A whole second in the recent past, so nanosecond offsets are visible
	// and every row is well inside retention.
	base := time.Now().UTC().Add(-time.Hour).Truncate(time.Second)
	all := func(id string) storage.RangeQuery {
		return storage.RangeQuery{InstrumentID: id, From: base.Add(-time.Hour), To: base.Add(time.Hour)}
	}
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}

	t.Run("setup is repeatable", func(t *testing.T) {
		must(store.Migrate(ctx))
		must(store.Migrate(ctx))
		must(store.Ping(ctx))
		inst := []domain.Instrument{{ID: "venueA:XYZ", Venue: "venueA", NativeSymbol: "XYZ", TickSize: "0.01", LotSize: "1", Currency: "USD"}}
		must(store.UpsertInstruments(ctx, inst))
		must(store.UpsertInstruments(ctx, inst))
		if store.Retention() != storage.DefaultRetention() {
			t.Errorf("retention = %+v, want the documented default", store.Retention())
		}
	})

	t.Run("a fact written again is stored once", func(t *testing.T) {
		const id = "c:dedup"
		tr := Trade(id, base, "T1", 1)
		qt := Quote(id, base, 2)
		dp := domain.Depth{Type: domain.KindDepth, InstrumentID: id, Bids: []domain.Level{{PriceTicks: 99, Quantity: 1}},
			Asks: []domain.Level{{PriceTicks: 101, Quantity: 2}}, EventTime: base, ReceiveTime: base, Meta: meta("beta", 3)}
		gp := domain.Gap{Type: domain.KindGap, Provider: "dedup", Venue: "c", Session: "S1", SeqDomain: "main",
			FromSeq: 4, ToSeq: 6, ReceiveTime: base, Source: domain.SourceLive}
		for i := 0; i < 3; i++ { // separate writes, as after a redelivery
			must(store.InsertTrades(ctx, []domain.Trade{tr}))
			must(store.InsertQuotes(ctx, []domain.Quote{qt}))
			must(store.InsertDepth(ctx, []domain.Depth{dp}))
			must(store.InsertGaps(ctx, []domain.Gap{gp}))
		}
		must(store.InsertTrades(ctx, []domain.Trade{tr, tr})) // and within one write
		trades, err := store.Trades(ctx, all(id))
		must(err)
		quotes, err := store.Quotes(ctx, all(id))
		must(err)
		depth, err := store.Depth(ctx, all(id))
		must(err)
		gaps, err := store.Gaps(ctx, storage.GapQuery{Venue: "c", From: base.Add(-time.Hour), To: base.Add(time.Hour)})
		must(err)
		if len(trades) != 1 || len(quotes) != 1 || len(depth) != 1 || len(gaps) != 1 {
			t.Fatalf("rows after repeated writes: %d trades, %d quotes, %d depth, %d gaps; want 1 each",
				len(trades), len(quotes), len(depth), len(gaps))
		}

		// Same venue fact, different key parts: these are different facts.
		must(store.InsertTrades(ctx, []domain.Trade{Trade(id, base, "T2", 1), Trade(id, base.Add(time.Nanosecond), "T1", 1)}))
		trades, err = store.Trades(ctx, all(id))
		must(err)
		if len(trades) != 3 {
			t.Errorf("trades differing in trade_id or event_time are distinct: got %d rows, want 3", len(trades))
		}
	})

	t.Run("rows come back exactly as written", func(t *testing.T) {
		const id = "c:roundtrip"
		at := base.Add(123456789 * time.Nanosecond)
		tr := Trade(id, at, "A-77", 41)
		tr.Flags = []string{domain.FlagClockSkew, domain.FlagStale}
		tr.Aggressor = ""
		tr.PriceTicks, tr.Quantity = 9_007_199_254_740_993, 1<<62 // beyond what a float64 can hold exactly
		qt := Quote(id, at, 42)
		qt.BidTicks, qt.BidSize = 0, 0
		qt.Flags = []string{domain.FlagNoBid}
		dp := domain.Depth{Type: domain.KindDepth, InstrumentID: id,
			Bids:      []domain.Level{{PriceTicks: 100, Quantity: 7}, {PriceTicks: 99, Quantity: 8}, {PriceTicks: 98, Quantity: 9}},
			Asks:      []domain.Level{{PriceTicks: 101, Quantity: 1}},
			EventTime: at, ReceiveTime: at.Add(time.Millisecond), Meta: meta("beta", 43)}
		gp := domain.Gap{Type: domain.KindGap, Provider: "roundtrip", Venue: "c", InstrumentID: id, Session: "S1",
			SeqDomain: "XYZ", FromSeq: 10, ToSeq: 12, ReceiveTime: at, Source: domain.SourceReplay}
		must(store.InsertTrades(ctx, []domain.Trade{tr}))
		must(store.InsertQuotes(ctx, []domain.Quote{qt}))
		must(store.InsertDepth(ctx, []domain.Depth{dp}))
		must(store.InsertGaps(ctx, []domain.Gap{gp}))

		trades, err := store.Trades(ctx, all(id))
		must(err)
		if len(trades) != 1 || !reflect.DeepEqual(trades[0], tr) {
			t.Errorf("trade\n got %+v\nwant %+v", trades, tr)
		}
		quotes, err := store.Quotes(ctx, all(id))
		must(err)
		if len(quotes) != 1 || !reflect.DeepEqual(quotes[0], qt) {
			t.Errorf("quote\n got %+v\nwant %+v", quotes, qt)
		}
		depth, err := store.Depth(ctx, all(id))
		must(err)
		if len(depth) != 1 || !reflect.DeepEqual(depth[0], dp) {
			t.Errorf("depth\n got %+v\nwant %+v", depth, dp)
		}
		gaps, err := store.Gaps(ctx, storage.GapQuery{InstrumentID: id, Venue: "c", From: at, To: at.Add(time.Nanosecond)})
		must(err)
		if len(gaps) != 1 || !reflect.DeepEqual(gaps[0], gp) {
			t.Errorf("gap\n got %+v\nwant %+v", gaps, gp)
		}
	})

	t.Run("ranges are half-open, ordered and scoped to the instrument", func(t *testing.T) {
		const id = "c:range"
		var trades []domain.Trade
		var quotes []domain.Quote
		for i := 9; i >= 0; i-- { // written out of order
			at := base.Add(time.Duration(i) * time.Millisecond)
			trades = append(trades, Trade(id, at, fmt.Sprintf("T%02d", i), uint64(i+1)))
			quotes = append(quotes, Quote(id, at, uint64(i+1)))
		}
		must(store.InsertTrades(ctx, trades))
		must(store.InsertQuotes(ctx, quotes))
		must(store.InsertTrades(ctx, []domain.Trade{Trade("c:range-other", base, "X", 1)}))

		q := storage.RangeQuery{InstrumentID: id, From: base.Add(2 * time.Millisecond), To: base.Add(5 * time.Millisecond)}
		got, err := store.Trades(ctx, q)
		must(err)
		if len(got) != 3 || got[0].TradeID != "T02" || got[1].TradeID != "T03" || got[2].TradeID != "T04" {
			t.Errorf("[2ms,5ms) returned %d trades starting %v; want T02,T03,T04", len(got), ids(got))
		}
		gq, err := store.Quotes(ctx, q)
		must(err)
		if len(gq) != 3 || gq[0].Seq != 3 || gq[2].Seq != 5 {
			t.Errorf("[2ms,5ms) returned %d quotes; want seq 3..5", len(gq))
		}
		q = all(id)
		q.Limit = 4
		got, err = store.Trades(ctx, q)
		must(err)
		if len(got) != 4 || got[0].TradeID != "T00" || got[3].TradeID != "T03" {
			t.Errorf("limit 4 returned %v; want the first four", ids(got))
		}
		if empty, err := store.Trades(ctx, all("c:never-written")); err != nil || empty == nil || len(empty) != 0 {
			t.Errorf("an instrument without rows returns an empty, non-nil list: %v %v", empty, err)
		}
	})

	t.Run("a cursor visits every row once, even rows sharing an event time", func(t *testing.T) {
		const id = "c:cursor"
		var trades []domain.Trade
		var quotes []domain.Quote
		var depth []domain.Depth
		for i := 0; i < 23; i++ {
			at := base.Add(time.Duration(i/5) * time.Millisecond) // five rows per timestamp
			trades = append(trades, Trade(id, at, fmt.Sprintf("T%02d", 22-i), uint64(i+1)))
			quotes = append(quotes, Quote(id, at, uint64(i+1)))
			depth = append(depth, domain.Depth{Type: domain.KindDepth, InstrumentID: id,
				Bids: []domain.Level{{PriceTicks: 1, Quantity: 1}}, Asks: []domain.Level{},
				EventTime: at, ReceiveTime: at, Meta: meta("beta", uint64(i+1))})
		}
		must(store.InsertTrades(ctx, trades))
		must(store.InsertQuotes(ctx, quotes))
		must(store.InsertDepth(ctx, depth))

		seen := map[string]int{}
		q := all(id)
		q.Limit = 4
		pages := 0
		var prev *domain.Trade
		for {
			page, err := store.Trades(ctx, q)
			must(err)
			for i := range page {
				r := page[i]
				seen[r.TradeID]++
				if prev != nil && (r.EventTime.Before(prev.EventTime) || (r.EventTime.Equal(prev.EventTime) && r.TradeID <= prev.TradeID)) {
					t.Fatalf("trades out of (event_time, trade_id) order: %s after %s", r.TradeID, prev.TradeID)
				}
				prev = &page[i]
			}
			if pages++; len(page) < q.Limit {
				break
			}
			last := page[len(page)-1]
			q.After = &storage.Cursor{EventTime: last.EventTime, TradeID: last.TradeID}
		}
		if len(seen) != 23 || pages != 6 {
			t.Errorf("paged %d distinct trades in %d pages, want 23 in 6", len(seen), pages)
		}
		for id, n := range seen {
			if n != 1 {
				t.Errorf("trade %s returned %d times", id, n)
			}
		}

		var seqs []uint64
		q = all(id)
		q.Limit = 4
		for {
			page, err := store.Quotes(ctx, q)
			must(err)
			for _, r := range page {
				seqs = append(seqs, r.Seq)
			}
			if len(page) < q.Limit {
				break
			}
			last := page[len(page)-1]
			q.After = &storage.Cursor{EventTime: last.EventTime, Seq: last.Seq}
		}
		for i, s := range seqs {
			if s != uint64(i+1) {
				t.Fatalf("quotes paged as %v, want seq 1..23 in order", seqs)
			}
		}
		if len(seqs) != 23 {
			t.Errorf("paged %d quotes, want 23", len(seqs))
		}

		q = all(id)
		q.Limit = 10
		q.After = &storage.Cursor{EventTime: base.Add(3 * time.Millisecond), Seq: 18}
		dp, err := store.Depth(ctx, q)
		must(err)
		if len(dp) != 5 || dp[0].Seq != 19 || dp[4].Seq != 23 {
			t.Errorf("depth after (3ms, seq 18) returned %d rows, want seq 19..23", len(dp))
		}
	})

	t.Run("timestamps the store cannot keep are refused, not altered or dropped", func(t *testing.T) {
		const id = "venueA:RANGE"
		at := time.Now().UTC().Add(-time.Minute)
		query := storage.RangeQuery{InstrumentID: id, From: domain.MinTime, To: domain.MaxTime}
		for name, bad := range map[string]time.Time{
			"after the range":  time.Date(2110, 6, 1, 0, 0, 0, 0, time.UTC),
			"at the upper end": domain.MaxTime,
			"before 1970":      time.Date(1960, 6, 1, 0, 0, 0, 0, time.UTC),
			"zero":             {},
		} {
			// The good rows of a refused batch are not written either, so
			// the caller can retry them without the bad one.
			err := store.InsertTrades(ctx, []domain.Trade{Trade(id, at, "good-1", 1), Trade(id, bad, "bad", 2), Trade(id, at, "good-2", 3)})
			if !storage.IsPermanent(err) {
				t.Errorf("trade with event time %s: got %v, want a permanent error", name, err)
			}
			tr := Trade(id, at, "bad-receive-time", 4)
			tr.ReceiveTime = bad
			if err := store.InsertTrades(ctx, []domain.Trade{tr}); !storage.IsPermanent(err) {
				t.Errorf("trade with receive time %s: got %v, want a permanent error", name, err)
			}
			if err := store.InsertQuotes(ctx, []domain.Quote{Quote(id, bad, 1)}); !storage.IsPermanent(err) {
				t.Errorf("quote with event time %s: got %v, want a permanent error", name, err)
			}
			d := domain.Depth{Type: domain.KindDepth, InstrumentID: id, EventTime: bad, ReceiveTime: at,
				Meta: domain.Meta{Provider: "p", Session: "S1", Seq: 1, Source: domain.SourceLive}}
			if err := store.InsertDepth(ctx, []domain.Depth{d}); !storage.IsPermanent(err) {
				t.Errorf("depth with event time %s: got %v, want a permanent error", name, err)
			}
			g := domain.Gap{Type: domain.KindGap, Provider: "range", Venue: "venueA", Session: "S1", SeqDomain: "d",
				FromSeq: 1, ToSeq: 2, ReceiveTime: bad, Source: domain.SourceLive}
			if err := store.InsertGaps(ctx, []domain.Gap{g}); !storage.IsPermanent(err) {
				t.Errorf("gap with receive time %s: got %v, want a permanent error", name, err)
			}
		}
		if got, err := store.Trades(ctx, query); err != nil || len(got) != 0 {
			t.Errorf("refused batches left %d rows behind (%v)", len(got), err)
		}
		// The last instant before the bound is accepted and comes back unchanged.
		edge := domain.MaxTime.Add(-time.Nanosecond)
		last := Trade(id, edge, "edge", 9)
		last.ReceiveTime = edge
		if err := store.InsertTrades(ctx, []domain.Trade{last}); err != nil {
			t.Fatalf("the last representable instant: %v", err)
		}
		got, err := store.Trades(ctx, query)
		if err != nil || len(got) != 1 || !got[0].EventTime.Equal(edge) {
			t.Errorf("the last representable instant came back as %v (%v)", got, err)
		}
	})

	t.Run("gaps are found by instrument, by venue and by the time they were revealed", func(t *testing.T) {
		gap := func(provider, venue, instrument string, from uint64, at time.Time) domain.Gap {
			return domain.Gap{Type: domain.KindGap, Provider: provider, Venue: venue, InstrumentID: instrument, Session: "S1",
				SeqDomain: "d", FromSeq: from, ToSeq: from + 2, ReceiveTime: at, Source: domain.SourceLive}
		}
		must(store.InsertGaps(ctx, []domain.Gap{
			gap("g-alpha", "gA", "gA:X", 10, base),                    // one instrument
			gap("g-alpha", "gA", "gA:Y", 20, base.Add(time.Second)),   // another instrument
			gap("g-beta", "gB", "", 30, base.Add(2*time.Second)),      // feed-wide
			gap("g-beta", "gB", "", 40, base.Add(30*time.Minute)),     // feed-wide, later
			gap("g-other", "gC", "gC:Z", 50, base.Add(3*time.Second)), // unrelated venue
		}))
		froms := func(q storage.GapQuery) string {
			q.From, q.To = base, base.Add(10*time.Minute)
			got, err := store.Gaps(ctx, q)
			must(err)
			s := ""
			for _, g := range got {
				s += fmt.Sprint(g.FromSeq, " ")
			}
			return s
		}
		if got := froms(storage.GapQuery{InstrumentID: "gA:X", Venue: "gA"}); got != "10 " {
			t.Errorf("gaps for gA:X = %q, want its own gap only", got)
		}
		if got := froms(storage.GapQuery{InstrumentID: "gB:ANY", Venue: "gB"}); got != "30 " {
			t.Errorf("gaps for an instrument of gB = %q, want the feed-wide gap inside the window", got)
		}
		if got := froms(storage.GapQuery{Venue: "gA"}); got != "10 20 " {
			t.Errorf("gaps for venue gA = %q, want both in the order they were revealed", got)
		}
		if got, err := store.Gaps(ctx, storage.GapQuery{Venue: "gB", From: base, To: base.Add(time.Hour), Limit: 1}); err != nil || len(got) != 1 || got[0].FromSeq != 30 {
			t.Errorf("limit 1 = %v, %v", got, err)
		}
	})
}

func ids(ts []domain.Trade) []string {
	out := make([]string, len(ts))
	for i, t := range ts {
		out[i] = t.TradeID
	}
	return out
}
