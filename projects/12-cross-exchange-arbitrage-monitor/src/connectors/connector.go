package connectors

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"arbmonitor/src/domain"
	"arbmonitor/src/normalizer"
)

// EventType enumerates recorded or live feed event kinds.
type EventType string

const (
	EventBookRaw    EventType = "book_raw"    // exchange-native JSON in RawPayload
	EventBookUpdate EventType = "book_update" // pre-decoded RawBookUpdate
	EventDisconnect EventType = "disconnect"
	EventReconnect  EventType = "reconnect"
	EventFXRate     EventType = "fx_rate"
	EventHeartbeat  EventType = "heartbeat"
)

// FeedEvent is a single timestamped event from a live or recorded feed stream.
type FeedEvent struct {
	Type       EventType                 `json:"type"`
	Venue      string                    `json:"venue"`
	ReceivedAt time.Time                 `json:"received_at"`
	RawPayload json.RawMessage           `json:"raw_payload,omitempty"`
	BookUpdate *normalizer.RawBookUpdate `json:"book_update,omitempty"`
	FXRate     *domain.FXRate            `json:"fx_rate,omitempty"`
	Reason     string                    `json:"reason,omitempty"`
}

// IngestOutcome describes what happened when a FeedEvent was processed by the FeedManager.
type IngestOutcome struct {
	Accepted      bool
	Book          *domain.VenueBook
	FXRate        *domain.FXRate
	Duplicate     bool
	OutOfOrder    bool
	SequenceGap   bool
	Disconnected  bool
	Reconnected   bool
	RejectedError error
}

// ReconnectPolicy controls exponential backoff for reconnecting venue feeds.
type ReconnectPolicy struct {
	InitialBackoff time.Duration
	MaxBackoff     time.Duration
	MaxAttempts    int // 0 means unlimited
}

// NextBackoff computes deterministic exponential backoff for attempt >= 1.
func (p ReconnectPolicy) NextBackoff(attempt int) time.Duration {
	base := p.InitialBackoff
	if base <= 0 {
		base = 250 * time.Millisecond
	}
	maxB := p.MaxBackoff
	if maxB <= 0 {
		maxB = 10 * time.Second
	}
	if attempt <= 1 {
		if base > maxB {
			return maxB
		}
		return base
	}
	d := base
	for i := 1; i < attempt; i++ {
		if d >= maxB/2 {
			return maxB
		}
		d *= 2
	}
	if d > maxB {
		return maxB
	}
	return d
}

type venueState struct {
	venue            string
	status           domain.VenueStatus
	connected        bool
	needsResync      bool
	lastAsof         time.Time
	lastReceivedAt   time.Time
	lastSeq          uint64
	disconnectCount  int64
	reconnectCount   int64
	sequenceGaps     int64
	duplicateCount   int64
	outOfOrderCount  int64
	messagesAccepted int64
	books            map[string]domain.VenueBook // instrument -> latest book
	lastSeqByInst    map[string]uint64           // instrument -> last accepted seq
	resyncByInst     map[string]bool             // instrument -> awaiting snapshot
}

// FeedManager coordinates exchange adapters, sequence validation, reconnect state,
// normalized top-N books, and FX rates across all configured venues.
type FeedManager struct {
	mu         sync.RWMutex
	adapters   *AdapterRegistry
	normalizer *normalizer.Normalizer
	venues     map[string]*venueState
	fxRates    map[string]domain.FXRate // "FROM->TO" -> FXRate
}

// NewFeedManager creates a FeedManager with the given adapter registry and normalizer.
func NewFeedManager(adapters *AdapterRegistry, norm *normalizer.Normalizer) *FeedManager {
	if adapters == nil {
		adapters = NewDefaultAdapters()
	}
	return &FeedManager{
		adapters:   adapters,
		normalizer: norm,
		venues:     make(map[string]*venueState),
		fxRates:    make(map[string]domain.FXRate),
	}
}

func (fm *FeedManager) getOrCreateVenueLocked(venue string) *venueState {
	v := strings.ToLower(strings.TrimSpace(venue))
	st, ok := fm.venues[v]
	if !ok {
		st = &venueState{
			venue:         v,
			status:        domain.VenueStatusHealthy,
			connected:     true,
			books:         make(map[string]domain.VenueBook),
			lastSeqByInst: make(map[string]uint64),
			resyncByInst:  make(map[string]bool),
		}
		fm.venues[v] = st
	}
	return st
}

// Ingest processes a single FeedEvent with sequence checking, normalization, and state transitions.
func (fm *FeedManager) Ingest(ev FeedEvent) IngestOutcome {
	fm.mu.Lock()
	defer fm.mu.Unlock()

	receivedAt := ev.ReceivedAt.UTC()

	switch ev.Type {
	case EventFXRate:
		if ev.FXRate == nil {
			return IngestOutcome{RejectedError: errors.New("fx_rate event missing payload")}
		}
		fx := *ev.FXRate
		fx.From = strings.ToUpper(strings.TrimSpace(fx.From))
		fx.To = strings.ToUpper(strings.TrimSpace(fx.To))
		if fx.ReceivedAt.IsZero() {
			fx.ReceivedAt = receivedAt
		}
		fx.Asof = fx.Asof.UTC()
		fx.ReceivedAt = fx.ReceivedAt.UTC()
		if !fx.Rate.IsPositive() {
			return IngestOutcome{RejectedError: errors.New("fx rate must be positive")}
		}
		fm.fxRates[fxKey(fx.From, fx.To)] = fx
		return IngestOutcome{Accepted: true, FXRate: &fx}

	case EventDisconnect:
		st := fm.getOrCreateVenueLocked(ev.Venue)
		st.connected = false
		st.status = domain.VenueStatusDisconnected
		st.needsResync = true
		st.disconnectCount++
		st.lastReceivedAt = receivedAt
		// Invalidate existing books on disconnect so stale liquidity is never matched.
		for inst, book := range st.books {
			book.Status = domain.VenueStatusDisconnected
			st.books[inst] = book
			st.resyncByInst[inst] = true
		}
		return IngestOutcome{Accepted: true, Disconnected: true}

	case EventReconnect:
		st := fm.getOrCreateVenueLocked(ev.Venue)
		st.connected = true
		st.status = domain.VenueStatusResyncing
		st.needsResync = true
		st.reconnectCount++
		st.lastReceivedAt = receivedAt
		for inst := range st.books {
			st.resyncByInst[inst] = true
		}
		return IngestOutcome{Accepted: true, Reconnected: true}

	case EventHeartbeat:
		st := fm.getOrCreateVenueLocked(ev.Venue)
		st.lastReceivedAt = receivedAt
		return IngestOutcome{Accepted: true}

	case EventBookRaw, EventBookUpdate:
		return fm.ingestBookLocked(ev)

	default:
		return IngestOutcome{RejectedError: fmt.Errorf("unknown event type %q", ev.Type)}
	}
}

func (fm *FeedManager) ingestBookLocked(ev FeedEvent) IngestOutcome {
	var raw normalizer.RawBookUpdate
	var err error

	if ev.Type == EventBookRaw {
		raw, err = fm.adapters.Decode(strings.ToLower(strings.TrimSpace(ev.Venue)), ev.RawPayload, ev.ReceivedAt)
		if err != nil {
			return IngestOutcome{RejectedError: err}
		}
	} else {
		if ev.BookUpdate == nil {
			return IngestOutcome{RejectedError: errors.New("book_update event missing book_update payload")}
		}
		raw = *ev.BookUpdate
		if raw.Venue == "" {
			raw.Venue = ev.Venue
		}
		if raw.ReceivedAt.IsZero() {
			raw.ReceivedAt = ev.ReceivedAt
		}
	}

	st := fm.getOrCreateVenueLocked(raw.Venue)
	if !st.connected {
		// Automatically recover only on an explicit snapshot after reconnect; reject deltas while disconnected.
		if !raw.IsSnapshot {
			return IngestOutcome{RejectedError: fmt.Errorf("venue %s is disconnected", st.venue)}
		}
		st.connected = true
		st.reconnectCount++
	}

	book, err := fm.normalizer.Normalize(raw)
	if err != nil {
		return IngestOutcome{RejectedError: err}
	}

	inst := book.Instrument
	lastSeq, hasLast := st.lastSeqByInst[inst]
	awaitingSnapshot := st.resyncByInst[inst] || st.needsResync

	if awaitingSnapshot {
		if !raw.IsSnapshot {
			// Still waiting for a clean snapshot after disconnect or sequence gap.
			st.status = domain.VenueStatusResyncing
			return IngestOutcome{SequenceGap: true, RejectedError: fmt.Errorf("venue %s %s awaiting snapshot resync", st.venue, inst)}
		}
		// Snapshot clears resync gate for this instrument.
		st.resyncByInst[inst] = false
		st.needsResync = false
	} else if hasLast && book.Seq > 0 {
		switch {
		case book.Seq == lastSeq:
			st.duplicateCount++
			return IngestOutcome{Duplicate: true}
		case book.Seq < lastSeq:
			st.outOfOrderCount++
			return IngestOutcome{OutOfOrder: true}
		case !raw.IsSnapshot && book.Seq > lastSeq+1:
			// Gap detected on incremental update: mark resyncing and suppress until snapshot arrives.
			st.sequenceGaps++
			st.status = domain.VenueStatusResyncing
			st.resyncByInst[inst] = true
			if existing, ok := st.books[inst]; ok {
				existing.Status = domain.VenueStatusResyncing
				st.books[inst] = existing
			}
			return IngestOutcome{SequenceGap: true, RejectedError: fmt.Errorf("sequence gap on %s %s: expected %d got %d", st.venue, inst, lastSeq+1, book.Seq)}
		}
	}

	book.Status = domain.VenueStatusHealthy
	st.books[inst] = book
	if book.Seq > 0 {
		st.lastSeqByInst[inst] = book.Seq
		st.lastSeq = book.Seq
	}
	st.status = domain.VenueStatusHealthy
	st.lastAsof = book.Asof
	st.lastReceivedAt = book.ReceivedAt
	st.messagesAccepted++

	cloned := book.Clone()
	return IngestOutcome{Accepted: true, Book: &cloned}
}

// GetBook returns the latest normalized book for a venue and canonical instrument.
func (fm *FeedManager) GetBook(venue, instrument string) (domain.VenueBook, bool) {
	fm.mu.RLock()
	defer fm.mu.RUnlock()
	st, ok := fm.venues[strings.ToLower(strings.TrimSpace(venue))]
	if !ok {
		return domain.VenueBook{}, false
	}
	b, ok := st.books[strings.ToUpper(strings.TrimSpace(instrument))]
	if !ok {
		return domain.VenueBook{}, false
	}
	return b.Clone(), true
}

// BooksForBase returns all latest books whose base asset matches baseAsset (e.g., "BTC" matches BTC/USDT and BTC/USD).
func (fm *FeedManager) BooksForBase(baseAsset string) []domain.VenueBook {
	fm.mu.RLock()
	defer fm.mu.RUnlock()

	target := fm.normalizer.Registry().CanonicalAsset(baseAsset)
	var out []domain.VenueBook
	for _, st := range fm.venues {
		for inst, b := range st.books {
			spec, ok := fm.normalizer.Registry().LookupCanonical(inst)
			if ok && spec.Base == target {
				out = append(out, b.Clone())
			}
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Venue != out[j].Venue {
			return out[i].Venue < out[j].Venue
		}
		return out[i].Instrument < out[j].Instrument
	})
	return out
}

// AllBooks returns all current venue books deterministically sorted by (instrument, venue).
func (fm *FeedManager) AllBooks() []domain.VenueBook {
	fm.mu.RLock()
	defer fm.mu.RUnlock()

	var out []domain.VenueBook
	for _, st := range fm.venues {
		for _, b := range st.books {
			out = append(out, b.Clone())
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Instrument != out[j].Instrument {
			return out[i].Instrument < out[j].Instrument
		}
		return out[i].Venue < out[j].Venue
	})
	return out
}

// GetFXRate looks up an FX conversion rate from -> to.
func (fm *FeedManager) GetFXRate(from, to string) (domain.FXRate, bool) {
	fm.mu.RLock()
	defer fm.mu.RUnlock()
	fx, ok := fm.fxRates[fxKey(from, to)]
	return fx, ok
}

// SetFXRate directly sets an FX rate in the feed manager.
func (fm *FeedManager) SetFXRate(fx domain.FXRate) {
	fm.mu.Lock()
	defer fm.mu.Unlock()
	fx.From = strings.ToUpper(strings.TrimSpace(fx.From))
	fx.To = strings.ToUpper(strings.TrimSpace(fx.To))
	fm.fxRates[fxKey(fx.From, fx.To)] = fx
}

// VenueHealth returns deterministic health snapshots for every seen venue evaluated at `now`.
func (fm *FeedManager) VenueHealth(now time.Time, maxQuoteAgeMs int64) []domain.VenueHealth {
	fm.mu.RLock()
	defer fm.mu.RUnlock()

	now = now.UTC()
	out := make([]domain.VenueHealth, 0, len(fm.venues))
	for _, st := range fm.venues {
		var ageMs int64
		if !st.lastReceivedAt.IsZero() {
			ageMs = now.Sub(st.lastReceivedAt).Milliseconds()
			if ageMs < 0 {
				ageMs = 0
			}
		}
		var clockSkewMs int64
		if !st.lastAsof.IsZero() && !st.lastReceivedAt.IsZero() {
			clockSkewMs = st.lastReceivedAt.Sub(st.lastAsof).Milliseconds()
		}
		status := st.status
		if st.connected && status == domain.VenueStatusHealthy && maxQuoteAgeMs > 0 && ageMs > maxQuoteAgeMs {
			status = domain.VenueStatusStale
		}
		out = append(out, domain.VenueHealth{
			Venue:            st.venue,
			Status:           status,
			Connected:        st.connected,
			LastAsof:         st.lastAsof,
			LastReceivedAt:   st.lastReceivedAt,
			QuoteAgeMs:       ageMs,
			ClockSkewMs:      clockSkewMs,
			LastSeq:          st.lastSeq,
			DisconnectCount:  st.disconnectCount,
			ReconnectCount:   st.reconnectCount,
			SequenceGaps:     st.sequenceGaps,
			DuplicateCount:   st.duplicateCount,
			OutOfOrderCount:  st.outOfOrderCount,
			MessagesAccepted: st.messagesAccepted,
			ActiveBooks:      len(st.books),
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Venue < out[j].Venue })
	return out
}

func fxKey(from, to string) string {
	return strings.ToUpper(strings.TrimSpace(from)) + "->" + strings.ToUpper(strings.TrimSpace(to))
}

// ReadFeedJSONL loads a deterministic sequence of FeedEvents from a JSONL file.
func ReadFeedJSONL(path string) ([]FeedEvent, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open feed file %s: %w", path, err)
	}
	defer f.Close()

	var events []FeedEvent
	scanner := bufio.NewScanner(f)
	// Allow large order book snapshots up to 1MB per line.
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	lineNo := 0
	for scanner.Scan() {
		lineNo++
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		var ev FeedEvent
		if err := json.Unmarshal([]byte(line), &ev); err != nil {
			return nil, fmt.Errorf("parse feed line %d in %s: %w", lineNo, path, err)
		}
		events = append(events, ev)
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("scan feed file %s: %w", path, err)
	}
	return events, nil
}
