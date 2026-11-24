// Package connectors implements exchange-specific order book adapters,
// reconnecting feed state machines with sequence validation, and recorded feed readers.
package connectors

import (
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"arbmonitor/src/normalizer"
)

var (
	ErrUnsupportedVenue = errors.New("unsupported exchange adapter")
	ErrInvalidPayload   = errors.New("invalid exchange wire payload")
)

// ExchangeAdapter parses exchange-native wire JSON into a normalizer.RawBookUpdate.
type ExchangeAdapter interface {
	Venue() string
	Decode(payload []byte, receivedAt time.Time) (normalizer.RawBookUpdate, error)
}

// AdapterRegistry maps venue names to their native wire decoders.
type AdapterRegistry struct {
	adapters map[string]ExchangeAdapter
}

// NewDefaultAdapters returns adapters for Binance, Coinbase, and Kraken public book formats.
func NewDefaultAdapters() *AdapterRegistry {
	reg := &AdapterRegistry{adapters: make(map[string]ExchangeAdapter)}
	reg.Register(&BinanceAdapter{})
	reg.Register(&CoinbaseAdapter{})
	reg.Register(&KrakenAdapter{})
	return reg
}

// Register registers an exchange adapter.
func (r *AdapterRegistry) Register(a ExchangeAdapter) {
	r.adapters[a.Venue()] = a
}

// Decode dispatches raw bytes to the adapter registered for venue.
func (r *AdapterRegistry) Decode(venue string, payload []byte, receivedAt time.Time) (normalizer.RawBookUpdate, error) {
	a, ok := r.adapters[venue]
	if !ok {
		return normalizer.RawBookUpdate{}, fmt.Errorf("%w: %s", ErrUnsupportedVenue, venue)
	}
	return a.Decode(payload, receivedAt)
}

// --- Binance Adapter ---
// Wire shape inspired by Binance depth streams:
// {"s":"BTCUSDT","E":1764000000000,"u":1001,"snapshot":true,"b":[["65000.10","1.5"]],"a":[["65001.00","2.0"]]}

type binanceDepthMsg struct {
	Symbol     string      `json:"s"`
	EventMs    int64       `json:"E"`
	UpdateID   uint64      `json:"u"`
	IsSnapshot *bool       `json:"snapshot,omitempty"`
	Bids       [][2]string `json:"b"`
	Asks       [][2]string `json:"a"`
}

type BinanceAdapter struct{}

func (a *BinanceAdapter) Venue() string { return "binance" }

func (a *BinanceAdapter) Decode(payload []byte, receivedAt time.Time) (normalizer.RawBookUpdate, error) {
	var msg binanceDepthMsg
	if err := json.Unmarshal(payload, &msg); err != nil {
		return normalizer.RawBookUpdate{}, fmt.Errorf("%w: binance: %v", ErrInvalidPayload, err)
	}
	if msg.Symbol == "" || msg.EventMs <= 0 {
		return normalizer.RawBookUpdate{}, fmt.Errorf("%w: binance missing symbol or event time", ErrInvalidPayload)
	}
	isSnap := true
	if msg.IsSnapshot != nil {
		isSnap = *msg.IsSnapshot
	}
	return normalizer.RawBookUpdate{
		Venue:       a.Venue(),
		VenueSymbol: msg.Symbol,
		Bids:        tuplesToRawLevels(msg.Bids),
		Asks:        tuplesToRawLevels(msg.Asks),
		Asof:        time.UnixMilli(msg.EventMs).UTC(),
		ReceivedAt:  receivedAt.UTC(),
		Seq:         msg.UpdateID,
		IsSnapshot:  isSnap,
	}, nil
}

// --- Coinbase Adapter ---
// Wire shape inspired by Coinbase Exchange WebSocket l2/snapshot payloads:
// {"product_id":"BTC-USD","time":"2026-11-24T05:00:00.100Z","sequence":2001,"type":"snapshot","bids":[["65080.00","1.2"]],"asks":[["65082.00","1.8"]]}

type coinbaseBookMsg struct {
	ProductID string      `json:"product_id"`
	Time      string      `json:"time"`
	Sequence  uint64      `json:"sequence"`
	Type      string      `json:"type"`
	Bids      [][2]string `json:"bids"`
	Asks      [][2]string `json:"asks"`
}

type CoinbaseAdapter struct{}

func (a *CoinbaseAdapter) Venue() string { return "coinbase" }

func (a *CoinbaseAdapter) Decode(payload []byte, receivedAt time.Time) (normalizer.RawBookUpdate, error) {
	var msg coinbaseBookMsg
	if err := json.Unmarshal(payload, &msg); err != nil {
		return normalizer.RawBookUpdate{}, fmt.Errorf("%w: coinbase: %v", ErrInvalidPayload, err)
	}
	if msg.ProductID == "" || msg.Time == "" {
		return normalizer.RawBookUpdate{}, fmt.Errorf("%w: coinbase missing product_id or time", ErrInvalidPayload)
	}
	asof, err := time.Parse(time.RFC3339Nano, msg.Time)
	if err != nil {
		return normalizer.RawBookUpdate{}, fmt.Errorf("%w: coinbase invalid time: %v", ErrInvalidPayload, err)
	}
	isSnap := msg.Type == "" || msg.Type == "snapshot"
	return normalizer.RawBookUpdate{
		Venue:       a.Venue(),
		VenueSymbol: msg.ProductID,
		Bids:        tuplesToRawLevels(msg.Bids),
		Asks:        tuplesToRawLevels(msg.Asks),
		Asof:        asof.UTC(),
		ReceivedAt:  receivedAt.UTC(),
		Seq:         msg.Sequence,
		IsSnapshot:  isSnap,
	}, nil
}

// --- Kraken Adapter ---
// Wire shape inspired by Kraken v2 book feed:
// {"pair":"XBT/USDT","timestamp_ms":1764000000050,"seq":3001,"snapshot":true,"bids":[["65050.00","0.9"]],"asks":[["65055.00","1.1"]]}

type krakenBookMsg struct {
	Pair        string      `json:"pair"`
	TimestampMs int64       `json:"timestamp_ms"`
	Seq         uint64      `json:"seq"`
	IsSnapshot  *bool       `json:"snapshot,omitempty"`
	Bids        [][2]string `json:"bids"`
	Asks        [][2]string `json:"asks"`
}

type KrakenAdapter struct{}

func (a *KrakenAdapter) Venue() string { return "kraken" }

func (a *KrakenAdapter) Decode(payload []byte, receivedAt time.Time) (normalizer.RawBookUpdate, error) {
	var msg krakenBookMsg
	if err := json.Unmarshal(payload, &msg); err != nil {
		return normalizer.RawBookUpdate{}, fmt.Errorf("%w: kraken: %v", ErrInvalidPayload, err)
	}
	if msg.Pair == "" || msg.TimestampMs <= 0 {
		return normalizer.RawBookUpdate{}, fmt.Errorf("%w: kraken missing pair or timestamp_ms", ErrInvalidPayload)
	}
	isSnap := true
	if msg.IsSnapshot != nil {
		isSnap = *msg.IsSnapshot
	}
	return normalizer.RawBookUpdate{
		Venue:       a.Venue(),
		VenueSymbol: msg.Pair,
		Bids:        tuplesToRawLevels(msg.Bids),
		Asks:        tuplesToRawLevels(msg.Asks),
		Asof:        time.UnixMilli(msg.TimestampMs).UTC(),
		ReceivedAt:  receivedAt.UTC(),
		Seq:         msg.Seq,
		IsSnapshot:  isSnap,
	}, nil
}

func tuplesToRawLevels(tuples [][2]string) []normalizer.RawLevel {
	out := make([]normalizer.RawLevel, len(tuples))
	for i, t := range tuples {
		out[i] = normalizer.RawLevel{Price: t[0], Qty: t[1]}
	}
	return out
}
