package unit

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"mdp/src/domain"
	"mdp/src/storage"
	"mdp/tests/contract"
)

func TestArchiveWritesOneDayPerInstrumentToParquet(t *testing.T) {
	ctx := context.Background()
	store := storage.NewMemStore()
	day := time.Date(2026, 1, 5, 0, 0, 0, 0, time.UTC)
	const id = "venueA:XYZ"

	var trades []domain.Trade
	var quotes []domain.Quote
	for i := 0; i < 500; i++ {
		at := day.Add(9*time.Hour + time.Duration(i)*time.Second + 123456789)
		tr := contract.Trade(id, at, fmt.Sprintf("T%04d", i), uint64(2*i+1))
		if i%100 == 0 {
			tr.Flags = []string{domain.FlagStale, domain.FlagClockSkew}
		}
		trades = append(trades, tr)
		quotes = append(quotes, contract.Quote(id, at, uint64(2*i+2)))
	}
	trades = append(trades,
		contract.Trade(id, day, "first-instant", 9001), // inside: [day, day+24h)
		contract.Trade(id, day.Add(24*time.Hour-time.Nanosecond), "last-instant", 9002),
		contract.Trade(id, day.Add(-time.Nanosecond), "previous-day", 9003), // outside
		contract.Trade(id, day.Add(24*time.Hour), "next-day", 9004),         // outside
		contract.Trade("venueA:ABC", day.Add(time.Hour), "other-instrument", 9005),
	)
	if err := store.InsertTrades(ctx, trades); err != nil {
		t.Fatal(err)
	}
	if err := store.InsertQuotes(ctx, quotes); err != nil {
		t.Fatal(err)
	}

	dir := t.TempDir()
	files, err := storage.Archive(ctx, store, id, day.Add(13*time.Hour), dir) // any time of the day selects the day
	if err != nil {
		t.Fatal(err)
	}
	tradePath := filepath.Join(dir, "trades", "date=2026-01-05", "instrument=venueA_XYZ", "part-0.parquet")
	quotePath := filepath.Join(dir, "quotes", "date=2026-01-05", "instrument=venueA_XYZ", "part-0.parquet")
	want := []storage.ArchiveFile{{Path: tradePath, Rows: 502}, {Path: quotePath, Rows: 500}}
	if !reflect.DeepEqual(files, want) {
		t.Fatalf("files %+v, want %+v", files, want)
	}

	rows, err := storage.ReadParquet[storage.TradeRow](tradePath)
	if err != nil {
		t.Fatal(err)
	}
	inStore, _ := store.Trades(ctx, storage.RangeQuery{InstrumentID: id, From: day, To: day.Add(24 * time.Hour)})
	if len(rows) != len(inStore) {
		t.Fatalf("archived %d trades, store has %d for the day", len(rows), len(inStore))
	}
	for i, r := range rows {
		if got := storage.TradeFromRow(r); !reflect.DeepEqual(got, inStore[i]) {
			t.Fatalf("row %d\n got %+v\nwant %+v", i, got, inStore[i])
		}
	}
	if rows[0].TradeID != "first-instant" || rows[len(rows)-1].TradeID != "last-instant" {
		t.Errorf("the day is [00:00, 24:00): first %s, last %s", rows[0].TradeID, rows[len(rows)-1].TradeID)
	}
	qrows, err := storage.ReadParquet[storage.QuoteRow](quotePath)
	if err != nil || len(qrows) != 500 || qrows[0].Seq != 2 || qrows[0].EventTimeNs != day.Add(9*time.Hour+123456789).UnixNano() {
		t.Errorf("quotes: %d rows, %v", len(qrows), err)
	}

	// Archiving the same data again produces the same file, and no temporary files remain.
	again := t.TempDir()
	if _, err := storage.Archive(ctx, store, id, day, again); err != nil {
		t.Fatal(err)
	}
	a, _ := os.ReadFile(tradePath)
	b, _ := os.ReadFile(strings.Replace(tradePath, dir, again, 1))
	if len(a) == 0 || !bytes.Equal(a, b) {
		t.Error("archiving the same day twice should give identical files")
	}
	filepath.WalkDir(dir, func(path string, d os.DirEntry, _ error) error {
		if strings.HasSuffix(path, ".tmp") {
			t.Errorf("temporary file left behind: %s", path)
		}
		return nil
	})

	// A day or instrument without rows writes nothing.
	empty := t.TempDir()
	files, err = storage.Archive(ctx, store, "venueA:QRS", day, empty)
	if err != nil || len(files) != 0 {
		t.Errorf("no rows: %v %v", files, err)
	}
	if entries, _ := os.ReadDir(empty); len(entries) != 0 {
		t.Errorf("no rows should create no directories: %v", entries)
	}
}

func TestArchivePagesThroughLargeDays(t *testing.T) {
	ctx := context.Background()
	store := storage.NewMemStore()
	day := time.Date(2026, 1, 5, 0, 0, 0, 0, time.UTC)
	const n = 20_050 // more than two store pages
	trades := make([]domain.Trade, n)
	for i := range trades {
		// Ten trades per timestamp, so pages break between rows that share an event time.
		trades[i] = contract.Trade("venueA:XYZ", day.Add(time.Duration(i/10)*time.Millisecond), fmt.Sprintf("T%05d", i), uint64(i+1))
	}
	if err := store.InsertTrades(ctx, trades); err != nil {
		t.Fatal(err)
	}
	files, err := storage.Archive(ctx, store, "venueA:XYZ", day, t.TempDir())
	if err != nil || len(files) != 1 || files[0].Rows != n {
		t.Fatalf("files %+v, %v", files, err)
	}
	rows, err := storage.ReadParquet[storage.TradeRow](files[0].Path)
	if err != nil || len(rows) != n {
		t.Fatalf("read %d rows, %v", len(rows), err)
	}
	for i, r := range rows {
		if r.TradeID != fmt.Sprintf("T%05d", i) {
			t.Fatalf("row %d is %s: a page boundary lost or repeated a row", i, r.TradeID)
		}
	}
}
