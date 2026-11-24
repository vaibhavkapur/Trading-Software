package storage

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/parquet-go/parquet-go"
	"github.com/parquet-go/parquet-go/compress/zstd"

	"mdp/src/domain"
)

// TradeRow is the Parquet layout of an archived trade. Timestamps are UTC
// nanoseconds since the epoch; the column names say which clock.
type TradeRow struct {
	InstrumentID      string   `parquet:"instrument_id,dict"`
	TradeID           string   `parquet:"trade_id"`
	PriceTicks        int64    `parquet:"price_ticks"`
	Quantity          int64    `parquet:"quantity"`
	Aggressor         string   `parquet:"aggressor,dict"`
	EventTimeNs       int64    `parquet:"event_time_ns"`   // venue clock
	ReceiveTimeNs     int64    `parquet:"receive_time_ns"` // ingestor clock
	Provider          string   `parquet:"provider,dict"`
	Session           string   `parquet:"session,dict"`
	SeqDomain         string   `parquet:"seq_domain,dict"`
	Seq               int64    `parquet:"seq"`
	Source            string   `parquet:"source,dict"`
	NormalizerVersion int32    `parquet:"normalizer_version"`
	Flags             []string `parquet:"flags,list"`
}

// QuoteRow is the Parquet layout of an archived quote.
type QuoteRow struct {
	InstrumentID      string   `parquet:"instrument_id,dict"`
	BidTicks          int64    `parquet:"bid_ticks"`
	BidSize           int64    `parquet:"bid_size"`
	AskTicks          int64    `parquet:"ask_ticks"`
	AskSize           int64    `parquet:"ask_size"`
	EventTimeNs       int64    `parquet:"event_time_ns"`
	ReceiveTimeNs     int64    `parquet:"receive_time_ns"`
	Provider          string   `parquet:"provider,dict"`
	Session           string   `parquet:"session,dict"`
	SeqDomain         string   `parquet:"seq_domain,dict"`
	Seq               int64    `parquet:"seq"`
	Source            string   `parquet:"source,dict"`
	NormalizerVersion int32    `parquet:"normalizer_version"`
	Flags             []string `parquet:"flags,list"`
}

// ArchiveFile describes one Parquet file written by Archive.
type ArchiveFile struct {
	Path string
	Rows int
}

const archivePage = 10000

// Archive exports one UTC day of an instrument's trades and quotes from the
// store to Parquet files under dir, laid out for partition discovery:
//
//	<dir>/<table>/date=2026-01-05/instrument=venueA_XYZ/part-0.parquet
//
// Rows are in store order (event time, then tie-breaker), so archiving the
// same data twice produces the same rows. Files are written to a temporary
// name and renamed, so a partial file is never left under the final name.
// An instrument with no rows that day produces no file.
func Archive(ctx context.Context, store Store, instrumentID string, day time.Time, dir string) ([]ArchiveFile, error) {
	from := day.UTC().Truncate(24 * time.Hour)
	q := RangeQuery{InstrumentID: instrumentID, From: from, To: from.Add(24 * time.Hour), Limit: archivePage}
	part := func(table string) string {
		return filepath.Join(dir, table, "date="+from.Format("2006-01-02"),
			"instrument="+strings.NewReplacer(":", "_", "/", "_").Replace(instrumentID), "part-0.parquet")
	}
	var files []ArchiveFile

	var trades []TradeRow
	for tq := q; ; {
		rows, err := store.Trades(ctx, tq)
		if err != nil {
			return nil, fmt.Errorf("archive: trades: %w", err)
		}
		for _, r := range rows {
			trades = append(trades, TradeRow{
				InstrumentID: r.InstrumentID, TradeID: r.TradeID, PriceTicks: r.PriceTicks, Quantity: r.Quantity,
				Aggressor: r.Aggressor, EventTimeNs: r.EventTime.UnixNano(), ReceiveTimeNs: r.ReceiveTime.UnixNano(),
				Provider: r.Provider, Session: r.Session, SeqDomain: r.SeqDomain, Seq: int64(r.Seq), Source: r.Source,
				NormalizerVersion: int32(r.NormalizerVersion), Flags: r.Flags,
			})
		}
		if len(rows) < archivePage {
			break
		}
		last := rows[len(rows)-1]
		tq.After = &Cursor{EventTime: last.EventTime, TradeID: last.TradeID}
	}
	if len(trades) > 0 {
		if err := writeParquet(part("trades"), trades); err != nil {
			return nil, err
		}
		files = append(files, ArchiveFile{Path: part("trades"), Rows: len(trades)})
	}

	var quotes []QuoteRow
	for qq := q; ; {
		rows, err := store.Quotes(ctx, qq)
		if err != nil {
			return nil, fmt.Errorf("archive: quotes: %w", err)
		}
		for _, r := range rows {
			quotes = append(quotes, QuoteRow{
				InstrumentID: r.InstrumentID, BidTicks: r.BidTicks, BidSize: r.BidSize, AskTicks: r.AskTicks, AskSize: r.AskSize,
				EventTimeNs: r.EventTime.UnixNano(), ReceiveTimeNs: r.ReceiveTime.UnixNano(),
				Provider: r.Provider, Session: r.Session, SeqDomain: r.SeqDomain, Seq: int64(r.Seq), Source: r.Source,
				NormalizerVersion: int32(r.NormalizerVersion), Flags: r.Flags,
			})
		}
		if len(rows) < archivePage {
			break
		}
		last := rows[len(rows)-1]
		qq.After = &Cursor{EventTime: last.EventTime, Seq: last.Seq}
	}
	if len(quotes) > 0 {
		if err := writeParquet(part("quotes"), quotes); err != nil {
			return nil, err
		}
		files = append(files, ArchiveFile{Path: part("quotes"), Rows: len(quotes)})
	}
	return files, nil
}

func writeParquet[T any](path string, rows []T) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("archive: %w", err)
	}
	tmp := path + ".tmp"
	f, err := os.Create(tmp)
	if err != nil {
		return fmt.Errorf("archive: %w", err)
	}
	w := parquet.NewGenericWriter[T](f, parquet.Compression(&zstd.Codec{}))
	_, err = w.Write(rows)
	if cerr := w.Close(); err == nil {
		err = cerr
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		os.Remove(tmp)
		return fmt.Errorf("archive: write %s: %w", path, err)
	}
	return os.Rename(tmp, path)
}

// ReadParquet loads every row of a Parquet file written by Archive.
func ReadParquet[T any](path string) ([]T, error) {
	return parquet.ReadFile[T](path)
}

// TradeFromRow converts an archived row back to a canonical trade.
func TradeFromRow(r TradeRow) domain.Trade {
	t := domain.Trade{
		Type: domain.KindTrade, InstrumentID: r.InstrumentID, TradeID: r.TradeID, PriceTicks: r.PriceTicks,
		Quantity: r.Quantity, Aggressor: r.Aggressor,
		EventTime: time.Unix(0, r.EventTimeNs).UTC(), ReceiveTime: time.Unix(0, r.ReceiveTimeNs).UTC(),
		Meta: domain.Meta{Provider: r.Provider, Session: r.Session, SeqDomain: r.SeqDomain, Seq: uint64(r.Seq),
			Source: r.Source, NormalizerVersion: int(r.NormalizerVersion)},
	}
	if len(r.Flags) > 0 {
		t.Flags = r.Flags
	}
	return t
}
