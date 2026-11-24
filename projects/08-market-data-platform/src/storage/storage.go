// Package storage persists canonical facts and answers historical queries.
//
// Store is the port. ClickHouse is the deployed adapter; MemStore is the
// in-process one for hermetic tests and the standalone demo. Both are
// idempotent on the deduplication keys documented in package domain: writing
// the same fact twice leaves one row.
package storage

import (
	"context"
	"errors"
	"fmt"
	"time"

	"mdp/src/domain"
)

// Retention is how long each table keeps rows, measured from event_time
// (gaps: from receive_time, the only timestamp a gap has). Queries cannot
// return anything older.
type Retention struct {
	Trades time.Duration
	Quotes time.Duration
	Depth  time.Duration
	Gaps   time.Duration
}

// DefaultRetention is the documented query retention.
func DefaultRetention() Retention {
	day := 24 * time.Hour
	return Retention{Trades: 90 * day, Quotes: 30 * day, Depth: 7 * day, Gaps: 90 * day}
}

// Cursor is an exclusive keyset position in (event_time, tie-breaker) order.
// The tie-breaker is TradeID for trades and Seq for quotes and depth.
type Cursor struct {
	EventTime time.Time
	TradeID   string
	Seq       uint64
}

// RangeQuery selects one instrument's events with event_time in [From, To),
// ordered by event_time then tie-breaker.
type RangeQuery struct {
	InstrumentID string
	From, To     time.Time
	Limit        int
	After        *Cursor
}

// GapQuery selects gaps revealed (receive_time) in [From, To) that could
// affect an instrument: gaps scoped to it, plus feed-wide gaps of its venue.
type GapQuery struct {
	InstrumentID string
	Venue        string
	From, To     time.Time
	Limit        int
}

// Store is the storage port.
type Store interface {
	Migrate(ctx context.Context) error
	Ping(ctx context.Context) error
	Retention() Retention

	UpsertInstruments(ctx context.Context, rows []domain.Instrument) error
	InsertTrades(ctx context.Context, rows []domain.Trade) error
	InsertQuotes(ctx context.Context, rows []domain.Quote) error
	InsertDepth(ctx context.Context, rows []domain.Depth) error
	InsertGaps(ctx context.Context, rows []domain.Gap) error

	Trades(ctx context.Context, q RangeQuery) ([]domain.Trade, error)
	Quotes(ctx context.Context, q RangeQuery) ([]domain.Quote, error)
	Depth(ctx context.Context, q RangeQuery) ([]domain.Depth, error)
	Gaps(ctx context.Context, q GapQuery) ([]domain.Gap, error)
}

// PermanentError marks a write the store will never accept, such as a row
// that violates the schema. Retrying it is pointless; the sink dead-letters
// the offending rows instead of blocking the pipeline behind them.
type PermanentError struct{ Err error }

func (e *PermanentError) Error() string { return "permanent: " + e.Err.Error() }
func (e *PermanentError) Unwrap() error { return e.Err }

// IsPermanent reports whether err is, or wraps, a PermanentError.
func IsPermanent(err error) bool {
	var p *PermanentError
	return errors.As(err, &p)
}

// MaxRetention is the longest retention a store accepts. It keeps every
// expiry time (timestamp plus retention) far inside what any store's date
// arithmetic can represent; longer history belongs in the Parquet archive.
const MaxRetention = 5 * 365 * 24 * time.Hour

// checkTimes refuses rows whose timestamps no store can represent. Every
// adapter applies it before writing anything, so a refused batch leaves no
// rows behind and the sink can narrow it down to the offending row.
func checkTimes[T any](table string, rows []T, times func(T) []time.Time) error {
	for i, r := range rows {
		for _, t := range times(r) {
			if !domain.TimeInRange(t) {
				return &PermanentError{Err: fmt.Errorf("%s row %d: timestamp %s is outside (%s, %s)", table, i,
					t.UTC().Format(time.RFC3339), domain.MinTime.Format("2006-01-02"), domain.MaxTime.Format("2006-01-02"))}
			}
		}
	}
	return nil
}

func tradeTimes(r domain.Trade) []time.Time { return []time.Time{r.EventTime, r.ReceiveTime} }
func quoteTimes(r domain.Quote) []time.Time { return []time.Time{r.EventTime, r.ReceiveTime} }
func depthTimes(r domain.Depth) []time.Time { return []time.Time{r.EventTime, r.ReceiveTime} }
func gapTimes(r domain.Gap) []time.Time     { return []time.Time{r.ReceiveTime} }
