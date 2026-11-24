package storage

import (
	"context"
	"sort"
	"sync"
	"time"

	"mdp/src/domain"
)

// MemStore is an in-process Store. It mirrors the ClickHouse adapter's
// semantics: one row per deduplication key, last write wins, half-open time
// ranges, keyset pagination. Retention is reported but not enforced.
type MemStore struct {
	mu          sync.Mutex
	instruments map[string]domain.Instrument
	trades      map[string]domain.Trade
	quotes      map[string]domain.Quote
	depth       map[string]domain.Depth
	gaps        map[string]domain.Gap
	writes      map[string]int // rows received per table, including duplicates
	fault       func(table string, rows int) error
	rowFault    func(table, dedupKey string) error
	retention   Retention
}

func NewMemStore() *MemStore {
	return &MemStore{
		instruments: map[string]domain.Instrument{},
		trades:      map[string]domain.Trade{},
		quotes:      map[string]domain.Quote{},
		depth:       map[string]domain.Depth{},
		gaps:        map[string]domain.Gap{},
		writes:      map[string]int{},
		retention:   DefaultRetention(),
	}
}

// SetRetention overrides the retention the store reports.
func (s *MemStore) SetRetention(r Retention) {
	s.mu.Lock()
	s.retention = r
	s.mu.Unlock()
}

// SetFault installs a hook that can fail any write (table, row count), Ping
// (table "ping") or query (table "query"). Pass nil to clear it.
func (s *MemStore) SetFault(f func(table string, rows int) error) {
	s.mu.Lock()
	s.fault = f
	s.mu.Unlock()
}

// SetRowFault installs a hook consulted for every row of a write. If it
// returns an error for any row the whole write fails and nothing is stored,
// which is how a real store treats a batch containing a bad row.
func (s *MemStore) SetRowFault(f func(table, dedupKey string) error) {
	s.mu.Lock()
	s.rowFault = f
	s.mu.Unlock()
}

// check applies the row fault hook to a batch. Callers hold s.mu.
func check[T interface{ DedupKey() string }](s *MemStore, table string, rows []T) error {
	if s.rowFault == nil {
		return nil
	}
	for _, r := range rows {
		if err := s.rowFault(table, r.DedupKey()); err != nil {
			return err
		}
	}
	return nil
}

// queryFault applies the fault hook to a read. Callers hold s.mu.
func (s *MemStore) queryFault() error {
	if s.fault == nil {
		return nil
	}
	return s.fault("query", 0)
}

// begin locks the store and applies the fault hook.
func (s *MemStore) begin(table string, rows int) error {
	s.mu.Lock()
	if s.fault != nil {
		if err := s.fault(table, rows); err != nil {
			s.mu.Unlock()
			return err
		}
	}
	s.writes[table] += rows
	return nil
}

func (s *MemStore) Migrate(context.Context) error { return nil }
func (s *MemStore) Retention() Retention {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.retention
}

func (s *MemStore) Ping(context.Context) error {
	if err := s.begin("ping", 0); err != nil {
		return err
	}
	s.mu.Unlock()
	return nil
}

func (s *MemStore) UpsertInstruments(_ context.Context, rows []domain.Instrument) error {
	if err := s.begin("instruments", len(rows)); err != nil {
		return err
	}
	defer s.mu.Unlock()
	for _, r := range rows {
		s.instruments[r.ID] = r
	}
	return nil
}

func (s *MemStore) InsertTrades(_ context.Context, rows []domain.Trade) error {
	if err := checkTimes("trades", rows, tradeTimes); err != nil {
		return err
	}
	if err := s.begin("trades", len(rows)); err != nil {
		return err
	}
	defer s.mu.Unlock()
	if err := check(s, "trades", rows); err != nil {
		return err
	}
	for _, r := range rows {
		s.trades[r.DedupKey()] = r
	}
	return nil
}

func (s *MemStore) InsertQuotes(_ context.Context, rows []domain.Quote) error {
	if err := checkTimes("quotes", rows, quoteTimes); err != nil {
		return err
	}
	if err := s.begin("quotes", len(rows)); err != nil {
		return err
	}
	defer s.mu.Unlock()
	if err := check(s, "quotes", rows); err != nil {
		return err
	}
	for _, r := range rows {
		s.quotes[r.DedupKey()] = r
	}
	return nil
}

func (s *MemStore) InsertDepth(_ context.Context, rows []domain.Depth) error {
	if err := checkTimes("depth", rows, depthTimes); err != nil {
		return err
	}
	if err := s.begin("depth", len(rows)); err != nil {
		return err
	}
	defer s.mu.Unlock()
	if err := check(s, "depth", rows); err != nil {
		return err
	}
	for _, r := range rows {
		s.depth[r.DedupKey()] = r
	}
	return nil
}

func (s *MemStore) InsertGaps(_ context.Context, rows []domain.Gap) error {
	if err := checkTimes("gaps", rows, gapTimes); err != nil {
		return err
	}
	if err := s.begin("gaps", len(rows)); err != nil {
		return err
	}
	defer s.mu.Unlock()
	if err := check(s, "gaps", rows); err != nil {
		return err
	}
	for _, r := range rows {
		s.gaps[r.DedupKey()] = r
	}
	return nil
}

// Rows returns the number of distinct rows in a table.
func (s *MemStore) Rows(table string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	switch table {
	case "instruments":
		return len(s.instruments)
	case "trades":
		return len(s.trades)
	case "quotes":
		return len(s.quotes)
	case "depth":
		return len(s.depth)
	case "gaps":
		return len(s.gaps)
	}
	return 0
}

// Writes returns how many rows were written to a table, duplicates included.
func (s *MemStore) Writes(table string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.writes[table]
}

func inRange(t time.Time, q RangeQuery) bool {
	return !t.Before(q.From) && t.Before(q.To)
}

func page[T any](rows []T, limit int) []T {
	if limit > 0 && len(rows) > limit {
		rows = rows[:limit]
	}
	return rows
}

func (s *MemStore) Trades(_ context.Context, q RangeQuery) ([]domain.Trade, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.queryFault(); err != nil {
		return nil, err
	}
	out := []domain.Trade{}
	for _, r := range s.trades {
		if r.InstrumentID != q.InstrumentID || !inRange(r.EventTime, q) {
			continue
		}
		if c := q.After; c != nil && (r.EventTime.Before(c.EventTime) ||
			(r.EventTime.Equal(c.EventTime) && r.TradeID <= c.TradeID)) {
			continue
		}
		out = append(out, r)
	}
	sort.Slice(out, func(i, j int) bool {
		if !out[i].EventTime.Equal(out[j].EventTime) {
			return out[i].EventTime.Before(out[j].EventTime)
		}
		return out[i].TradeID < out[j].TradeID
	})
	return page(out, q.Limit), nil
}

func afterSeq(t time.Time, seq uint64, c *Cursor) bool {
	return c == nil || t.After(c.EventTime) || (t.Equal(c.EventTime) && seq > c.Seq)
}

func (s *MemStore) Quotes(_ context.Context, q RangeQuery) ([]domain.Quote, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.queryFault(); err != nil {
		return nil, err
	}
	out := []domain.Quote{}
	for _, r := range s.quotes {
		if r.InstrumentID == q.InstrumentID && inRange(r.EventTime, q) && afterSeq(r.EventTime, r.Seq, q.After) {
			out = append(out, r)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if !out[i].EventTime.Equal(out[j].EventTime) {
			return out[i].EventTime.Before(out[j].EventTime)
		}
		return out[i].Seq < out[j].Seq
	})
	return page(out, q.Limit), nil
}

func (s *MemStore) Depth(_ context.Context, q RangeQuery) ([]domain.Depth, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.queryFault(); err != nil {
		return nil, err
	}
	out := []domain.Depth{}
	for _, r := range s.depth {
		if r.InstrumentID == q.InstrumentID && inRange(r.EventTime, q) && afterSeq(r.EventTime, r.Seq, q.After) {
			out = append(out, r)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if !out[i].EventTime.Equal(out[j].EventTime) {
			return out[i].EventTime.Before(out[j].EventTime)
		}
		return out[i].Seq < out[j].Seq
	})
	return page(out, q.Limit), nil
}

func (s *MemStore) Gaps(_ context.Context, q GapQuery) ([]domain.Gap, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.queryFault(); err != nil {
		return nil, err
	}
	out := []domain.Gap{}
	for _, g := range s.gaps {
		if g.ReceiveTime.Before(q.From) || !g.ReceiveTime.Before(q.To) {
			continue
		}
		if !gapMatches(g, q) {
			continue
		}
		out = append(out, g)
	}
	sort.Slice(out, func(i, j int) bool {
		if !out[i].ReceiveTime.Equal(out[j].ReceiveTime) {
			return out[i].ReceiveTime.Before(out[j].ReceiveTime)
		}
		return out[i].DedupKey() < out[j].DedupKey()
	})
	return page(out, q.Limit), nil
}

// gapMatches applies GapQuery's scope: with an instrument, its own gaps and
// its venue's feed-wide gaps; with only a venue, every gap of that venue.
func gapMatches(g domain.Gap, q GapQuery) bool {
	if q.Venue != "" && g.Venue != q.Venue {
		return false
	}
	if q.InstrumentID != "" && g.InstrumentID != "" && g.InstrumentID != q.InstrumentID {
		return false
	}
	return true
}
