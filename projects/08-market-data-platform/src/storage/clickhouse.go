package storage

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"mdp/src/domain"
)

// ClickHouseConfig locates a ClickHouse server's HTTP interface. The
// password comes from the environment, never from a file in the repository.
type ClickHouseConfig struct {
	URL       string // e.g. http://localhost:8123
	Database  string
	User      string
	Password  string
	Retention Retention
}

// ClickHouse is the Store adapter for ClickHouse, over its HTTP interface.
//
// Idempotency. Every table is a ReplacingMergeTree whose sorting key is the
// deduplication key documented in package domain. ClickHouse collapses rows
// with equal sorting keys when it merges parts, which happens in the
// background at a time of its choosing, so every read uses FINAL to collapse
// them at query time. A fact written twice is therefore never returned twice,
// whether or not the merge has happened yet.
//
// Retention. Each table has a TTL on event time (receive time for gaps).
// Rows past it are deleted by ClickHouse; the sink refuses to write rows
// that are already past it rather than let them vanish.
type ClickHouse struct {
	cfg ClickHouseConfig
	hc  *http.Client
}

func NewClickHouse(cfg ClickHouseConfig) *ClickHouse {
	if cfg.Retention == (Retention{}) {
		cfg.Retention = DefaultRetention()
	}
	return &ClickHouse{cfg: cfg, hc: &http.Client{Timeout: 30 * time.Second}}
}

func (c *ClickHouse) Retention() Retention { return c.cfg.Retention }

// permanentCodes are ClickHouse exception codes that mean the data itself
// is unacceptable, so retrying the same rows cannot succeed.
var permanentCodes = map[int]bool{
	6:   true, // CANNOT_PARSE_TEXT
	26:  true, // CANNOT_PARSE_QUOTED_STRING
	27:  true, // CANNOT_PARSE_INPUT_ASSERTION_FAILED
	38:  true, // CANNOT_PARSE_DATE
	41:  true, // CANNOT_PARSE_DATETIME
	53:  true, // TYPE_MISMATCH
	69:  true, // ARGUMENT_OUT_OF_BOUND
	70:  true, // CANNOT_CONVERT_TYPE
	72:  true, // CANNOT_PARSE_NUMBER
	117: true, // INCORRECT_DATA
	130: true, // CANNOT_READ_ARRAY_FROM_TEXT
	349: true, // CANNOT_INSERT_NULL_IN_ORDINARY_COLUMN
	407: true, // DECIMAL_OVERFLOW: a DateTime64 beyond its range
	469: true, // VIOLATED_CONSTRAINT: a CHECK constraint added by an operator
}

// exec runs one statement. For INSERT, body carries the rows; otherwise the
// query travels in the body so it is not length-limited. params are bound
// server-side as {name:Type} placeholders, so values are never spliced into SQL.
func (c *ClickHouse) exec(ctx context.Context, database, query string, params map[string]string, body []byte) ([]byte, error) {
	q := url.Values{}
	if database != "" {
		q.Set("database", database)
	}
	for k, v := range params {
		q.Set("param_"+k, v)
	}
	q.Set("output_format_json_quote_64bit_integers", "0")
	payload := []byte(query)
	if body != nil {
		q.Set("query", query)
		payload = body
		// Synchronous inserts: a 200 means the rows are in a part on disk,
		// and a refused batch is refused as a whole. Newer servers default
		// to asynchronous inserts; the sink already batches.
		q.Set("async_insert", "0")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.cfg.URL+"/?"+q.Encode(), bytes.NewReader(payload))
	if err != nil {
		return nil, err
	}
	req.Header.Set("X-ClickHouse-User", c.cfg.User)
	req.Header.Set("X-ClickHouse-Key", c.cfg.Password)
	resp, err := c.hc.Do(req)
	if err != nil {
		// url.Error includes the URL, which carries no credentials: they are sent as headers.
		return nil, fmt.Errorf("clickhouse: %w", err)
	}
	defer resp.Body.Close()
	out, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("clickhouse: read response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		msg := strings.TrimSpace(string(out))
		if len(msg) > 400 {
			msg = msg[:400]
		}
		err := fmt.Errorf("clickhouse: HTTP %d: %s", resp.StatusCode, msg)
		if code, _ := strconv.Atoi(resp.Header.Get("X-ClickHouse-Exception-Code")); permanentCodes[code] {
			return nil, &PermanentError{Err: err}
		}
		return nil, err
	}
	return out, nil
}

func (c *ClickHouse) Ping(ctx context.Context) error {
	_, err := c.exec(ctx, c.cfg.Database, "SELECT 1", nil, nil)
	return err
}

func days(d time.Duration) int {
	n := int(d / (24 * time.Hour))
	if n < 1 {
		n = 1
	}
	return n
}

// Schema returns the DDL for the configured retention.
func (c *ClickHouse) Schema() []string {
	r := c.cfg.Retention
	provenance := `
  provider LowCardinality(String),
  session String,
  seq_domain String,
  seq UInt64,
  source LowCardinality(String),
  normalizer_version UInt16,
  flags Array(LowCardinality(String)),
  persist_ts DateTime64(3, 'UTC') DEFAULT now64(3) COMMENT 'store clock'`
	clocks := `
  event_ts DateTime64(9, 'UTC') COMMENT 'venue clock, as supplied by the provider',
  recv_ts DateTime64(9, 'UTC') COMMENT 'ingestor host clock at socket read',`
	return []string{
		`CREATE TABLE IF NOT EXISTS instruments (
  instrument_id String,
  venue LowCardinality(String),
  native_symbol String,
  tick_size String COMMENT 'decimal; price = price_ticks * tick_size',
  lot_size String COMMENT 'decimal; size = quantity * lot_size',
  currency LowCardinality(String),
  updated_at DateTime64(3, 'UTC') DEFAULT now64(3)
) ENGINE = ReplacingMergeTree(updated_at)
ORDER BY instrument_id`,

		fmt.Sprintf(`CREATE TABLE IF NOT EXISTS trades (
  instrument_id LowCardinality(String),
  trade_id String COMMENT 'venue-assigned',
  price_ticks Int64,
  quantity Int64 COMMENT 'lots',
  aggressor LowCardinality(String),%s%s
) ENGINE = ReplacingMergeTree(persist_ts)
PARTITION BY toDate(event_ts)
ORDER BY (instrument_id, event_ts, trade_id)
TTL event_ts + INTERVAL %d DAY`, clocks, provenance, days(r.Trades)),

		fmt.Sprintf(`CREATE TABLE IF NOT EXISTS quotes (
  instrument_id LowCardinality(String),
  bid_ticks Int64,
  bid_size Int64,
  ask_ticks Int64,
  ask_size Int64,%s%s
) ENGINE = ReplacingMergeTree(persist_ts)
PARTITION BY toDate(event_ts)
ORDER BY (instrument_id, event_ts, seq)
TTL event_ts + INTERVAL %d DAY`, clocks, provenance, days(r.Quotes)),

		fmt.Sprintf(`CREATE TABLE IF NOT EXISTS depth (
  instrument_id LowCardinality(String),
  bid_ticks Array(Int64) COMMENT 'best first',
  bid_qty Array(Int64),
  ask_ticks Array(Int64) COMMENT 'best first',
  ask_qty Array(Int64),%s%s
) ENGINE = ReplacingMergeTree(persist_ts)
PARTITION BY toDate(event_ts)
ORDER BY (instrument_id, event_ts, seq)
TTL event_ts + INTERVAL %d DAY`, clocks, provenance, days(r.Depth)),

		fmt.Sprintf(`CREATE TABLE IF NOT EXISTS feed_gaps (
  provider LowCardinality(String),
  venue LowCardinality(String),
  instrument_id String COMMENT 'empty: feed-wide, any instrument of the venue',
  session String,
  seq_domain String,
  from_seq UInt64,
  to_seq UInt64,
  recv_ts DateTime64(9, 'UTC') COMMENT 'ingestor clock of the event that revealed the gap',
  source LowCardinality(String),
  persist_ts DateTime64(3, 'UTC') DEFAULT now64(3)
) ENGINE = ReplacingMergeTree(persist_ts)
PARTITION BY toDate(recv_ts)
ORDER BY (provider, session, seq_domain, from_seq)
TTL recv_ts + INTERVAL %d DAY`, days(r.Gaps)),
	}
}

// Migrate creates the database and tables if they do not exist. Changing
// retention on an existing table needs ALTER TABLE ... MODIFY TTL.
func (c *ClickHouse) Migrate(ctx context.Context) error {
	if !validIdent(c.cfg.Database) {
		return fmt.Errorf("clickhouse: invalid database name %q", c.cfg.Database)
	}
	r := c.cfg.Retention
	for table, d := range map[string]time.Duration{"trades": r.Trades, "quotes": r.Quotes, "depth": r.Depth, "gaps": r.Gaps} {
		if d <= 0 || d > MaxRetention {
			return fmt.Errorf("clickhouse: %s retention %s must be between one day and %d days", table, d, days(MaxRetention))
		}
	}
	if _, err := c.exec(ctx, "", "CREATE DATABASE IF NOT EXISTS "+c.cfg.Database, nil, nil); err != nil {
		return err
	}
	for _, ddl := range c.Schema() {
		if _, err := c.exec(ctx, c.cfg.Database, ddl, nil, nil); err != nil {
			return err
		}
	}
	return nil
}

func validIdent(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if !(r == '_' || r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9') {
			return false
		}
	}
	return true
}

const chTime = "2006-01-02 15:04:05.000000000"

func ts(t time.Time) string { return t.UTC().Format(chTime) }

func nonNil(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}

// insert writes rows as JSONEachRow.
func (c *ClickHouse) insert(ctx context.Context, table string, rows []any) error {
	if len(rows) == 0 {
		return nil
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	for _, r := range rows {
		if err := enc.Encode(r); err != nil {
			return &PermanentError{Err: err}
		}
	}
	_, err := c.exec(ctx, c.cfg.Database, "INSERT INTO "+table+" FORMAT JSONEachRow", nil, buf.Bytes())
	return err
}

type chMeta struct {
	Provider          string   `json:"provider"`
	Session           string   `json:"session"`
	SeqDomain         string   `json:"seq_domain"`
	Seq               uint64   `json:"seq"`
	Source            string   `json:"source"`
	NormalizerVersion int      `json:"normalizer_version"`
	Flags             []string `json:"flags"`
}

func toMeta(m domain.Meta) chMeta {
	return chMeta{m.Provider, m.Session, m.SeqDomain, m.Seq, m.Source, m.NormalizerVersion, nonNil(m.Flags)}
}

func (m chMeta) domain() domain.Meta {
	out := domain.Meta{Provider: m.Provider, Session: m.Session, SeqDomain: m.SeqDomain, Seq: m.Seq,
		Source: m.Source, NormalizerVersion: m.NormalizerVersion}
	if len(m.Flags) > 0 {
		out.Flags = m.Flags
	}
	return out
}

func (c *ClickHouse) UpsertInstruments(ctx context.Context, rows []domain.Instrument) error {
	out := make([]any, len(rows))
	for i, r := range rows {
		out[i] = r
	}
	return c.insert(ctx, "instruments", out)
}

type chTrade struct {
	InstrumentID string `json:"instrument_id"`
	TradeID      string `json:"trade_id"`
	PriceTicks   int64  `json:"price_ticks"`
	Quantity     int64  `json:"quantity"`
	Aggressor    string `json:"aggressor"`
	EventTS      string `json:"event_ts,omitempty"`
	RecvTS       string `json:"recv_ts,omitempty"`
	EventNs      int64  `json:"event_ns,omitempty"`
	RecvNs       int64  `json:"recv_ns,omitempty"`
	chMeta
}

func (c *ClickHouse) InsertTrades(ctx context.Context, rows []domain.Trade) error {
	if err := checkTimes("trades", rows, tradeTimes); err != nil {
		return err
	}
	out := make([]any, len(rows))
	for i, r := range rows {
		out[i] = chTrade{InstrumentID: r.InstrumentID, TradeID: r.TradeID, PriceTicks: r.PriceTicks, Quantity: r.Quantity,
			Aggressor: r.Aggressor, EventTS: ts(r.EventTime), RecvTS: ts(r.ReceiveTime), chMeta: toMeta(r.Meta)}
	}
	return c.insert(ctx, "trades", out)
}

type chQuote struct {
	InstrumentID string `json:"instrument_id"`
	BidTicks     int64  `json:"bid_ticks"`
	BidSize      int64  `json:"bid_size"`
	AskTicks     int64  `json:"ask_ticks"`
	AskSize      int64  `json:"ask_size"`
	EventTS      string `json:"event_ts,omitempty"`
	RecvTS       string `json:"recv_ts,omitempty"`
	EventNs      int64  `json:"event_ns,omitempty"`
	RecvNs       int64  `json:"recv_ns,omitempty"`
	chMeta
}

func (c *ClickHouse) InsertQuotes(ctx context.Context, rows []domain.Quote) error {
	if err := checkTimes("quotes", rows, quoteTimes); err != nil {
		return err
	}
	out := make([]any, len(rows))
	for i, r := range rows {
		out[i] = chQuote{InstrumentID: r.InstrumentID, BidTicks: r.BidTicks, BidSize: r.BidSize, AskTicks: r.AskTicks,
			AskSize: r.AskSize, EventTS: ts(r.EventTime), RecvTS: ts(r.ReceiveTime), chMeta: toMeta(r.Meta)}
	}
	return c.insert(ctx, "quotes", out)
}

type chDepth struct {
	InstrumentID string  `json:"instrument_id"`
	BidTicks     []int64 `json:"bid_ticks"`
	BidQty       []int64 `json:"bid_qty"`
	AskTicks     []int64 `json:"ask_ticks"`
	AskQty       []int64 `json:"ask_qty"`
	EventTS      string  `json:"event_ts,omitempty"`
	RecvTS       string  `json:"recv_ts,omitempty"`
	EventNs      int64   `json:"event_ns,omitempty"`
	RecvNs       int64   `json:"recv_ns,omitempty"`
	chMeta
}

func splitLevels(ls []domain.Level) (px, qty []int64) {
	px, qty = make([]int64, len(ls)), make([]int64, len(ls))
	for i, l := range ls {
		px[i], qty[i] = l.PriceTicks, l.Quantity
	}
	return px, qty
}

func joinLevels(px, qty []int64) []domain.Level {
	out := make([]domain.Level, 0, len(px))
	for i := range px {
		if i < len(qty) {
			out = append(out, domain.Level{PriceTicks: px[i], Quantity: qty[i]})
		}
	}
	return out
}

func (c *ClickHouse) InsertDepth(ctx context.Context, rows []domain.Depth) error {
	if err := checkTimes("depth", rows, depthTimes); err != nil {
		return err
	}
	out := make([]any, len(rows))
	for i, r := range rows {
		d := chDepth{InstrumentID: r.InstrumentID, EventTS: ts(r.EventTime), RecvTS: ts(r.ReceiveTime), chMeta: toMeta(r.Meta)}
		d.BidTicks, d.BidQty = splitLevels(r.Bids)
		d.AskTicks, d.AskQty = splitLevels(r.Asks)
		out[i] = d
	}
	return c.insert(ctx, "depth", out)
}

type chGap struct {
	Provider     string `json:"provider"`
	Venue        string `json:"venue"`
	InstrumentID string `json:"instrument_id"`
	Session      string `json:"session"`
	SeqDomain    string `json:"seq_domain"`
	FromSeq      uint64 `json:"from_seq"`
	ToSeq        uint64 `json:"to_seq"`
	RecvTS       string `json:"recv_ts,omitempty"`
	RecvNs       int64  `json:"recv_ns,omitempty"`
	Source       string `json:"source"`
}

func (c *ClickHouse) InsertGaps(ctx context.Context, rows []domain.Gap) error {
	if err := checkTimes("feed_gaps", rows, gapTimes); err != nil {
		return err
	}
	out := make([]any, len(rows))
	for i, r := range rows {
		out[i] = chGap{Provider: r.Provider, Venue: r.Venue, InstrumentID: r.InstrumentID, Session: r.Session,
			SeqDomain: r.SeqDomain, FromSeq: r.FromSeq, ToSeq: r.ToSeq, RecvTS: ts(r.ReceiveTime), Source: r.Source}
	}
	return c.insert(ctx, "feed_gaps", out)
}

// selectRows runs a SELECT ... FORMAT JSONEachRow and decodes each row.
func selectRows[T any](ctx context.Context, c *ClickHouse, query string, params map[string]string) ([]T, error) {
	// Duplicates of a key always share a partition (the key includes the
	// partitioning timestamp), so FINAL need not merge across partitions.
	query += "\nSETTINGS do_not_merge_across_partitions_select_final = 1\nFORMAT JSONEachRow"
	out, err := c.exec(ctx, c.cfg.Database, query, params, nil)
	if err != nil {
		return nil, err
	}
	rows := []T{}
	sc := bufio.NewScanner(bytes.NewReader(out))
	sc.Buffer(make([]byte, 64<<10), 16<<20)
	for sc.Scan() {
		var row T
		if err := json.Unmarshal(sc.Bytes(), &row); err != nil {
			return nil, fmt.Errorf("clickhouse: decode row: %w", err)
		}
		rows = append(rows, row)
	}
	return rows, sc.Err()
}

const metaCols = "provider, session, seq_domain, seq, source, normalizer_version, flags"
const timeCols = "toUnixTimestamp64Nano(event_ts) AS event_ns, toUnixTimestamp64Nano(recv_ts) AS recv_ns"

// rangeSQL builds the shared WHERE / ORDER BY / LIMIT of a range query.
// tie is the tie-breaker column and tieType its ClickHouse type.
func rangeSQL(q RangeQuery, tie, tieType string) (string, map[string]string) {
	params := map[string]string{
		"id":   q.InstrumentID,
		"from": strconv.FormatInt(q.From.UnixNano(), 10),
		"to":   strconv.FormatInt(q.To.UnixNano(), 10),
	}
	sql := `
WHERE instrument_id = {id:String}
  AND event_ts >= fromUnixTimestamp64Nano({from:Int64})
  AND event_ts < fromUnixTimestamp64Nano({to:Int64})`
	if c := q.After; c != nil {
		params["cts"] = strconv.FormatInt(c.EventTime.UnixNano(), 10)
		if tie == "trade_id" {
			params["ctie"] = c.TradeID
		} else {
			params["ctie"] = strconv.FormatUint(c.Seq, 10)
		}
		sql += fmt.Sprintf(`
  AND (event_ts, %s) > (fromUnixTimestamp64Nano({cts:Int64}), {ctie:%s})`, tie, tieType)
	}
	sql += "\nORDER BY event_ts, " + tie
	if q.Limit > 0 {
		params["limit"] = strconv.Itoa(q.Limit)
		sql += "\nLIMIT {limit:UInt32}"
	}
	return sql, params
}

func (c *ClickHouse) Trades(ctx context.Context, q RangeQuery) ([]domain.Trade, error) {
	where, params := rangeSQL(q, "trade_id", "String")
	rows, err := selectRows[chTrade](ctx, c,
		"SELECT instrument_id, trade_id, price_ticks, quantity, aggressor, "+timeCols+", "+metaCols+" FROM trades FINAL"+where, params)
	if err != nil {
		return nil, err
	}
	out := make([]domain.Trade, len(rows))
	for i, r := range rows {
		out[i] = domain.Trade{Type: domain.KindTrade, InstrumentID: r.InstrumentID, TradeID: r.TradeID,
			PriceTicks: r.PriceTicks, Quantity: r.Quantity, Aggressor: r.Aggressor,
			EventTime: time.Unix(0, r.EventNs).UTC(), ReceiveTime: time.Unix(0, r.RecvNs).UTC(), Meta: r.chMeta.domain()}
	}
	return out, nil
}

func (c *ClickHouse) Quotes(ctx context.Context, q RangeQuery) ([]domain.Quote, error) {
	where, params := rangeSQL(q, "seq", "UInt64")
	rows, err := selectRows[chQuote](ctx, c,
		"SELECT instrument_id, bid_ticks, bid_size, ask_ticks, ask_size, "+timeCols+", "+metaCols+" FROM quotes FINAL"+where, params)
	if err != nil {
		return nil, err
	}
	out := make([]domain.Quote, len(rows))
	for i, r := range rows {
		out[i] = domain.Quote{Type: domain.KindQuote, InstrumentID: r.InstrumentID, BidTicks: r.BidTicks, BidSize: r.BidSize,
			AskTicks: r.AskTicks, AskSize: r.AskSize,
			EventTime: time.Unix(0, r.EventNs).UTC(), ReceiveTime: time.Unix(0, r.RecvNs).UTC(), Meta: r.chMeta.domain()}
	}
	return out, nil
}

func (c *ClickHouse) Depth(ctx context.Context, q RangeQuery) ([]domain.Depth, error) {
	where, params := rangeSQL(q, "seq", "UInt64")
	rows, err := selectRows[chDepth](ctx, c,
		"SELECT instrument_id, bid_ticks, bid_qty, ask_ticks, ask_qty, "+timeCols+", "+metaCols+" FROM depth FINAL"+where, params)
	if err != nil {
		return nil, err
	}
	out := make([]domain.Depth, len(rows))
	for i, r := range rows {
		out[i] = domain.Depth{Type: domain.KindDepth, InstrumentID: r.InstrumentID,
			Bids: joinLevels(r.BidTicks, r.BidQty), Asks: joinLevels(r.AskTicks, r.AskQty),
			EventTime: time.Unix(0, r.EventNs).UTC(), ReceiveTime: time.Unix(0, r.RecvNs).UTC(), Meta: r.chMeta.domain()}
	}
	return out, nil
}

func (c *ClickHouse) Gaps(ctx context.Context, q GapQuery) ([]domain.Gap, error) {
	params := map[string]string{
		"from": strconv.FormatInt(q.From.UnixNano(), 10),
		"to":   strconv.FormatInt(q.To.UnixNano(), 10),
	}
	sql := `SELECT provider, venue, instrument_id, session, seq_domain, from_seq, to_seq,
       toUnixTimestamp64Nano(recv_ts) AS recv_ns, source
FROM feed_gaps FINAL
WHERE recv_ts >= fromUnixTimestamp64Nano({from:Int64})
  AND recv_ts < fromUnixTimestamp64Nano({to:Int64})`
	if q.Venue != "" {
		params["venue"] = q.Venue
		sql += "\n  AND venue = {venue:String}"
	}
	if q.InstrumentID != "" {
		params["id"] = q.InstrumentID
		sql += "\n  AND (instrument_id = '' OR instrument_id = {id:String})"
	}
	sql += "\nORDER BY recv_ts, provider, session, seq_domain, from_seq"
	if q.Limit > 0 {
		params["limit"] = strconv.Itoa(q.Limit)
		sql += "\nLIMIT {limit:UInt32}"
	}
	rows, err := selectRows[chGap](ctx, c, sql, params)
	if err != nil {
		return nil, err
	}
	out := make([]domain.Gap, len(rows))
	for i, r := range rows {
		out[i] = domain.Gap{Type: domain.KindGap, Provider: r.Provider, Venue: r.Venue, InstrumentID: r.InstrumentID,
			Session: r.Session, SeqDomain: r.SeqDomain, FromSeq: r.FromSeq, ToSeq: r.ToSeq,
			ReceiveTime: time.Unix(0, r.RecvNs).UTC(), Source: r.Source}
	}
	return out, nil
}

// Count returns the number of distinct rows in a table, for tests and the
// runbook's verification steps.
func (c *ClickHouse) Count(ctx context.Context, table string) (int64, error) {
	if !validIdent(table) {
		return 0, fmt.Errorf("clickhouse: invalid table %q", table)
	}
	rows, err := selectRows[struct {
		N int64 `json:"n"`
	}](ctx, c, "SELECT count() AS n FROM "+table+" FINAL", nil)
	if err != nil || len(rows) != 1 {
		return 0, fmt.Errorf("clickhouse: count %s: %v", table, err)
	}
	return rows[0].N, nil
}

// TemporaryPrefix marks databases that DropDatabase may remove.
const TemporaryPrefix = "mdp_tmp_"

// DropDatabase removes this store's database and everything in it. It is
// for throwaway databases (benchmarks) and refuses any database whose name
// does not start with TemporaryPrefix.
func (c *ClickHouse) DropDatabase(ctx context.Context) error {
	if !validIdent(c.cfg.Database) || !strings.HasPrefix(c.cfg.Database, TemporaryPrefix) {
		return fmt.Errorf("clickhouse: refusing to drop %q: only %s* databases may be dropped", c.cfg.Database, TemporaryPrefix)
	}
	_, err := c.exec(ctx, "", "DROP DATABASE IF EXISTS "+c.cfg.Database+" SYNC", nil, nil)
	return err
}
