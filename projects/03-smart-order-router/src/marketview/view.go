// Package marketview keeps normalized quotes and venue health, and produces
// immutable snapshots that are the complete market input to the route planner.
package marketview

import (
	"fmt"
	"sort"

	"sor/src/domain"
)

type Status string

const (
	OK      Status = "OK"
	NoQuote Status = "NO_QUOTE"
	Stale   Status = "STALE"   // venue event time too old
	Skewed  Status = "SKEWED"  // venue event time is in our future
	Down    Status = "DOWN"    // link lost
	Tripped Status = "TRIPPED" // circuit breaker open after repeated failures
)

type Config struct {
	StaleMs           int64 `json:"stale_ms"`
	SkewToleranceMs   int64 `json:"skew_tolerance_ms"`
	BreakerThreshold  int   `json:"breaker_threshold"` // consecutive failures to trip
	BreakerCooldownMs int64 `json:"breaker_cooldown_ms"`
}

type VenueState struct {
	Venue         string         `json:"venue"`
	Status        Status         `json:"status"`
	Bids          []domain.Level `json:"bids"`
	Asks          []domain.Level `json:"asks"`
	AsOfMs        int64          `json:"asof_ms"`
	RecvMs        int64          `json:"recv_ms"`
	TakerFeeMills int64          `json:"taker_fee_mills"`
	LatencyMs     int64          `json:"latency_ms"`     // one-way estimate
	FillRatioBps  int64          `json:"fill_ratio_bps"` // EWMA of filled/sent
}

type Snapshot struct {
	ID      string       `json:"id"`
	Symbol  string       `json:"symbol"`
	TakenMs int64        `json:"taken_ms"`
	Venues  []VenueState `json:"venues"` // sorted by venue name
}

// BBO returns the consolidated best bid/ask across OK venues.
func (s Snapshot) BBO() (bid, ask int64, ok bool) {
	var haveBid, haveAsk bool
	for _, v := range s.Venues {
		if v.Status != OK {
			continue
		}
		if len(v.Bids) > 0 && (!haveBid || v.Bids[0].Px > bid) {
			bid, haveBid = v.Bids[0].Px, true
		}
		if len(v.Asks) > 0 && (!haveAsk || v.Asks[0].Px < ask) {
			ask, haveAsk = v.Asks[0].Px, true
		}
	}
	return bid, ask, haveBid && haveAsk
}

// MidMills is the consolidated mid in mills (exact for half-tick mids).
func (s Snapshot) MidMills() (int64, bool) {
	bid, ask, ok := s.BBO()
	return (bid + ask) * domain.MillsPerTick / 2, ok
}

type venue struct {
	fee       int64
	quote     *domain.VenueQuote
	linkUp    bool
	consec    int
	openUntil int64
	latMs     int64
	fillBps   int64
}

type View struct {
	cfg    Config
	symbol string
	names  []string
	venues map[string]*venue
	nSnap  int
}

func New(cfg Config, symbol string) *View {
	return &View{cfg: cfg, symbol: symbol, venues: map[string]*venue{}}
}

func (v *View) AddVenue(name string, takerFeeMills, nominalLatencyMs int64) {
	v.venues[name] = &venue{fee: takerFeeMills, linkUp: true, latMs: nominalLatencyMs, fillBps: 10000}
	v.names = append(v.names, name)
	sort.Strings(v.names)
}

// OnQuote stores a quote. Duplicate and out-of-order quotes (by venue
// sequence) are dropped; it reports whether the quote was accepted.
func (v *View) OnQuote(q domain.VenueQuote) bool {
	s, ok := v.venues[q.Venue]
	if !ok || q.Symbol != v.symbol || (s.quote != nil && q.Seq <= s.quote.Seq) {
		return false
	}
	s.quote = &q
	return true
}

func (v *View) SetLink(name string, up bool) {
	if s, ok := v.venues[name]; ok {
		s.linkUp = up
	}
}

// RecordOutcome feeds child-order results into venue health. A failure is a
// reject or a timeout; BreakerThreshold consecutive failures open the breaker
// for BreakerCooldownMs, after which a single trial order is allowed
// (half-open): one more failure re-opens it, a success closes it.
func (v *View) RecordOutcome(name string, ok bool, rttMs, filledQty, qty, nowMs int64) {
	s, found := v.venues[name]
	if !found {
		return
	}
	if !ok {
		s.fillBps = s.fillBps * 3 / 4
		s.consec++
		if v.cfg.BreakerThreshold > 0 && s.consec >= v.cfg.BreakerThreshold {
			s.openUntil = nowMs + v.cfg.BreakerCooldownMs
			s.consec = v.cfg.BreakerThreshold - 1
		}
		return
	}
	s.consec = 0
	s.latMs = (s.latMs*3 + rttMs/2) / 4
	if qty > 0 {
		s.fillBps = (s.fillBps*3 + filledQty*10000/qty) / 4
	}
}

func (v *View) status(s *venue, nowMs int64) Status {
	switch {
	case !s.linkUp:
		return Down
	case nowMs < s.openUntil:
		return Tripped
	case s.quote == nil:
		return NoQuote
	case s.quote.AsOfMs > s.quote.RecvMs+v.cfg.SkewToleranceMs:
		return Skewed
	case nowMs-s.quote.AsOfMs > v.cfg.StaleMs:
		return Stale
	}
	return OK
}

// Peek builds a snapshot without consuming a snapshot id (for read-only views).
func (v *View) Peek(nowMs int64) Snapshot {
	snap := Snapshot{Symbol: v.symbol, TakenMs: nowMs, Venues: make([]VenueState, 0, len(v.names))}
	for _, name := range v.names {
		s := v.venues[name]
		vs := VenueState{Venue: name, Status: v.status(s, nowMs), Bids: []domain.Level{}, Asks: []domain.Level{},
			TakerFeeMills: s.fee, LatencyMs: s.latMs, FillRatioBps: s.fillBps}
		if s.quote != nil {
			vs.Bids = append(vs.Bids, s.quote.Bids...)
			vs.Asks = append(vs.Asks, s.quote.Asks...)
			vs.AsOfMs, vs.RecvMs = s.quote.AsOfMs, s.quote.RecvMs
		}
		snap.Venues = append(snap.Venues, vs)
	}
	return snap
}

// Snapshot freezes the market view for a routing decision and assigns it an id.
func (v *View) Snapshot(nowMs int64) Snapshot {
	snap := v.Peek(nowMs)
	v.nSnap++
	snap.ID = fmt.Sprintf("S-%06d", v.nSnap)
	return snap
}
