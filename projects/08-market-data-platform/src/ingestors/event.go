// Package ingestors holds the provider-specific side of the platform: wire
// parsers, connections, reconnect and resume behavior. Everything a provider
// sends is turned into Event, a uniform but still un-normalized record that
// keeps the provider's own symbols, decimal strings and sequence numbers.
package ingestors

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"mdp/src/domain"
)

// Event is one parsed provider message as published to the raw topic.
type Event struct {
	Provider string `json:"provider"`
	Session  string `json:"session"` // provider session; sequence numbers restart when it changes
	Domain   string `json:"domain"`  // the scope within which Seq is contiguous
	Seq      uint64 `json:"seq"`
	Kind     string `json:"kind"`
	Symbol   string `json:"symbol"` // provider-native symbol

	// Trade fields. Side is "buy" or "sell" in the provider's own convention
	// (aggressor or resting side); the normalizer resolves the convention.
	Price   string `json:"price,omitempty"`
	Qty     string `json:"qty,omitempty"`
	TradeID string `json:"trade_id,omitempty"`
	Side    string `json:"side,omitempty"`

	// Quote fields. An empty price means that side is absent.
	Bid    string `json:"bid,omitempty"`
	BidQty string `json:"bid_qty,omitempty"`
	Ask    string `json:"ask,omitempty"`
	AskQty string `json:"ask_qty,omitempty"`

	// Depth fields: [price, quantity] pairs, best level first.
	Bids [][2]string `json:"bids,omitempty"`
	Asks [][2]string `json:"asks,omitempty"`

	EventTime time.Time `json:"event_time"`   // venue clock, as supplied
	RecvTime  time.Time `json:"receive_time"` // ingestor clock, at socket read
	Source    string    `json:"source"`
}

// TrackKey is the sequence-tracking and raw-topic routing key. Live and
// replayed streams of the same provider are tracked separately.
func (e Event) TrackKey() string {
	return e.Provider + "/" + e.Source + "/" + e.Domain
}

// Parser decodes one connection's frames. It is stateful because providers
// announce the session once, at login.
type Parser interface {
	// Parse returns the data events in a frame. Control frames (welcome,
	// heartbeat) return no events and no error. Returned events have Session,
	// Domain, Seq, Kind, Symbol, EventTime and the kind's fields set.
	Parse(frame []byte) ([]Event, error)
}

// Frame is one provider message as read off a connection.
type Frame struct {
	Data []byte
	// Recv is the receive time. Live connections leave it zero and the runner
	// stamps its own clock; a replay supplies the originally captured time.
	Recv time.Time
}

// Conn is one provider connection delivering frames in order.
type Conn interface {
	// ReadFrame returns the next frame. It fails if ctx ends first.
	ReadFrame(ctx context.Context) (Frame, error)
	Close() error
}

// Adapter is a feed adapter for one provider.
type Adapter interface {
	Provider() string
	// Source is domain.SourceLive or domain.SourceReplay.
	Source() string
	// Live adapters reconnect forever and can ask the provider to resend.
	// A non-live adapter (replay) ends at EOF.
	Live() bool
	NewParser() Parser
	// Domains lists the sequence domains the adapter subscribes to, when it
	// knows them before any event arrives; nil otherwise.
	Domains() []string
	// Dial connects and asks the provider to resume after the given offsets.
	Dial(ctx context.Context, resume []domain.FeedOffset) (Conn, error)
}

// ErrParse marks a frame the parser could not decode.
var ErrParse = errors.New("unparseable frame")

func parseErr(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrParse, fmt.Sprintf(format, args...))
}

// Token builds the resume token stored in a FeedOffset.
func Token(session string, seq uint64) string {
	return session + "@" + strconv.FormatUint(seq, 10)
}

// ParseToken splits a resume token into session and sequence number.
func ParseToken(tok string) (session string, seq uint64, err error) {
	i := strings.LastIndexByte(tok, '@')
	if i < 0 {
		return "", 0, fmt.Errorf("ingestors: bad resume token %q", tok)
	}
	seq, err = strconv.ParseUint(tok[i+1:], 10, 64)
	if err != nil {
		return "", 0, fmt.Errorf("ingestors: bad resume token %q", tok)
	}
	return tok[:i], seq, nil
}
