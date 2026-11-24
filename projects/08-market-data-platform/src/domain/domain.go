// Package domain holds the canonical, provider-independent market data schema.
//
// Clock provenance. Every event carries two timestamps that are never mixed:
//
//	event_time    the venue's clock, exactly as the provider supplied it
//	receive_time  the ingestor host's clock when the frame was read off the socket
//
// Both are UTC. A third clock, the store's (persist_ts), exists only in storage.
package domain

import (
	"encoding/json"
	"fmt"
	"time"
)

// SchemaVersion is the version of the canonical message layout.
const SchemaVersion = 1

// NormalizerVersion identifies the normalization rule set that produced an
// event. Bump it whenever a rule in package normalizer changes meaning.
const NormalizerVersion = 1

const (
	KindTrade = "trade"
	KindQuote = "quote"
	KindDepth = "depth"
	KindGap   = "gap"
)

const (
	SourceLive   = "live"
	SourceReplay = "replay"
)

// Flags attached to events that are valid but noteworthy.
const (
	FlagClockSkew = "clock_skew" // event_time is ahead of receive_time beyond tolerance
	FlagStale     = "stale"      // event_time is older than receive_time beyond the stale threshold
	FlagLocked    = "locked"     // bid == ask
	FlagNoBid     = "no_bid"     // one-sided quote
	FlagNoAsk     = "no_ask"
)

// Instrument is reference data. ID is "<venue>:<native_symbol>".
type Instrument struct {
	ID           string `json:"instrument_id"`
	Venue        string `json:"venue"`
	NativeSymbol string `json:"native_symbol"`
	TickSize     string `json:"tick_size"`
	LotSize      string `json:"lot_size"`
	Currency     string `json:"currency"`
}

// Meta is the provenance attached to every canonical event.
type Meta struct {
	Provider          string   `json:"provider"`
	Session           string   `json:"session"`
	SeqDomain         string   `json:"seq_domain"`
	Seq               uint64   `json:"seq"`
	Source            string   `json:"source"`
	NormalizerVersion int      `json:"normalizer_version"`
	Flags             []string `json:"flags,omitempty"`
}

// Trade is MarketTrade. Prices are integer ticks and quantities integer lots
// of the instrument's tick_size / lot_size.
type Trade struct {
	Type         string    `json:"type"`
	InstrumentID string    `json:"instrument_id"`
	TradeID      string    `json:"trade_id"`
	PriceTicks   int64     `json:"price_ticks"`
	Quantity     int64     `json:"quantity"`
	Aggressor    string    `json:"aggressor,omitempty"`
	EventTime    time.Time `json:"event_time"`
	ReceiveTime  time.Time `json:"receive_time"`
	Meta
}

// Quote is MarketQuote: top of book.
type Quote struct {
	Type         string    `json:"type"`
	InstrumentID string    `json:"instrument_id"`
	BidTicks     int64     `json:"bid_ticks"`
	BidSize      int64     `json:"bid_size"`
	AskTicks     int64     `json:"ask_ticks"`
	AskSize      int64     `json:"ask_size"`
	EventTime    time.Time `json:"event_time"`
	ReceiveTime  time.Time `json:"receive_time"`
	Meta
}

// Level is one price level of a depth snapshot.
type Level struct {
	PriceTicks int64 `json:"price_ticks"`
	Quantity   int64 `json:"quantity"`
}

// Depth is a level-2 snapshot of the best N levels per side.
type Depth struct {
	Type         string    `json:"type"`
	InstrumentID string    `json:"instrument_id"`
	Bids         []Level   `json:"bids"`
	Asks         []Level   `json:"asks"`
	EventTime    time.Time `json:"event_time"`
	ReceiveTime  time.Time `json:"receive_time"`
	Meta
}

// Gap records provider sequence numbers [FromSeq, ToSeq] that never arrived.
// InstrumentID is set when the sequence domain covers one instrument;
// otherwise the gap may affect any instrument of Venue.
type Gap struct {
	Type         string    `json:"type"`
	Provider     string    `json:"provider"`
	Venue        string    `json:"venue"`
	InstrumentID string    `json:"instrument_id,omitempty"`
	Session      string    `json:"session"`
	SeqDomain    string    `json:"seq_domain"`
	FromSeq      uint64    `json:"from_seq"`
	ToSeq        uint64    `json:"to_seq"`
	ReceiveTime  time.Time `json:"receive_time"` // receive_time of the event that revealed the gap
	Source       string    `json:"source"`
}

// Missing is the number of sequence numbers the gap covers.
func (g Gap) Missing() uint64 { return g.ToSeq - g.FromSeq + 1 }

// Reject is a record that could not be processed. Rejects are published to
// the reject topic so nothing is dropped silently.
type Reject struct {
	Stage       string    `json:"stage"` // ingest | normalize | sink
	Reason      string    `json:"reason"`
	Detail      string    `json:"detail,omitempty"`
	Provider    string    `json:"provider,omitempty"`
	Raw         string    `json:"raw,omitempty"`
	ReceiveTime time.Time `json:"receive_time"`
}

// FeedOffset is how far an ingestor has durably published one provider
// sequence domain. ResumeToken is what the adapter hands back to the provider
// on reconnect.
type FeedOffset struct {
	Provider    string `json:"provider"`
	Partition   string `json:"partition"` // the provider's sequence domain
	Seq         uint64 `json:"seq"`
	ResumeToken string `json:"resume_token"`
}

// IngestHealth is the periodically published state of one ingestor.
type IngestHealth struct {
	Provider        string    `json:"provider"`
	Source          string    `json:"source"`
	Connected       bool      `json:"connected"`
	LagMs           int64     `json:"lag_ms"` // receive_time - event_time of the latest event; spans two clocks
	Gaps            uint64    `json:"gaps"`
	Duplicates      uint64    `json:"duplicates"`
	Reconnects      uint64    `json:"reconnects"`
	Resyncs         uint64    `json:"resyncs"`
	ParseErrors     uint64    `json:"parse_errors"`
	Events          uint64    `json:"events"`
	LastEventTime   time.Time `json:"last_event_time"`
	LastReceiveTime time.Time `json:"last_receive_time"`
	UpdatedAt       time.Time `json:"updated_at"`
}

// Deduplication keys. A fact redelivered by the provider, the log or a
// restarted service always produces the same key, and the store keeps one
// row per key. The keys are the store's sorting keys, so they include
// event_time; a redelivery carries the same event_time because it is the
// same venue fact.
//
//	trade        instrument_id, event_time, trade_id   (venue-assigned id)
//	quote/depth  instrument_id, event_time, seq        (provider sequence number)
//	gap          provider, session, seq_domain, from_seq

func (t Trade) DedupKey() string {
	return fmt.Sprintf("%s/%d/%s", t.InstrumentID, t.EventTime.UnixNano(), t.TradeID)
}
func (q Quote) DedupKey() string {
	return fmt.Sprintf("%s/%d/%d", q.InstrumentID, q.EventTime.UnixNano(), q.Seq)
}
func (d Depth) DedupKey() string {
	return fmt.Sprintf("%s/%d/%d", d.InstrumentID, d.EventTime.UnixNano(), d.Seq)
}
func (g Gap) DedupKey() string {
	return fmt.Sprintf("%s/%s/%s/%d", g.Provider, g.Session, g.SeqDomain, g.FromSeq)
}

// Header is the subset of fields shared by every canonical message; it lets
// consumers route a record without decoding the whole payload.
type Header struct {
	Type         string    `json:"type"`
	InstrumentID string    `json:"instrument_id"`
	Venue        string    `json:"venue"`
	Session      string    `json:"session"`
	Seq          uint64    `json:"seq"`
	EventTime    time.Time `json:"event_time"`
	ReceiveTime  time.Time `json:"receive_time"`
}

// PeekHeader decodes only the routing fields of a canonical message.
func PeekHeader(b []byte) (Header, error) {
	var h Header
	err := json.Unmarshal(b, &h)
	return h, err
}

// Encode marshals a canonical message. Canonical structs cannot fail to marshal.
func Encode(v any) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		panic("domain: marshal: " + err.Error())
	}
	return b
}

// Event and receive times are accepted only inside (MinTime, MaxTime). The
// bounds are what every store adapter can partition and expire correctly:
// ClickHouse's Date and DateTime types wrap outside 1970..2106/2149, and a
// table that derives its partition or expiry from them was observed to
// accept such rows and then misfile or silently delete them (see
// docs/decisions/0004-bounded-timestamp-range.md). A timestamp outside the
// range is a broken clock, not market data, and is rejected where it can be
// reported.
var (
	MinTime = time.Unix(0, 0).UTC()
	MaxTime = time.Date(2100, 1, 1, 0, 0, 0, 0, time.UTC)
)

// TimeInRange reports whether t can be stored.
func TimeInRange(t time.Time) bool { return t.After(MinTime) && t.Before(MaxTime) }
