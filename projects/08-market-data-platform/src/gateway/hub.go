package gateway

import (
	"context"
	"encoding/json"
	"log/slog"
	"sort"
	"strconv"
	"sync"
	"time"

	"mdp/src/domain"
	"mdp/src/metrics"
	"mdp/src/refdata"
	"mdp/src/stream"
)

// HubConfig tunes live delivery.
type HubConfig struct {
	QueueSize  int           // events buffered per subscriber before dropping
	StaleAfter time.Duration // an instrument silent this long is reported stale
	LagEvery   time.Duration
	BackoffMin time.Duration
	BackoffMax time.Duration
	Now        func() time.Time
}

func (c HubConfig) withDefaults() HubConfig {
	if c.QueueSize <= 0 {
		c.QueueSize = 1024
	}
	if c.StaleAfter <= 0 {
		c.StaleAfter = 3 * time.Second
	}
	if c.LagEvery <= 0 {
		c.LagEvery = time.Second
	}
	if c.BackoffMin <= 0 {
		c.BackoffMin = 100 * time.Millisecond
	}
	if c.BackoffMax <= 0 {
		c.BackoffMax = 5 * time.Second
	}
	if c.Now == nil {
		c.Now = time.Now
	}
	return c
}

// Hub fans canonical events out to live subscribers.
//
// Each subscriber has a bounded queue. The hub never blocks on a subscriber:
// one slow reader must not delay the others. When a subscriber's queue is
// full the event is not delivered to that subscriber, and that fact is
// counted and reported to it as a gap message as soon as its connection
// moves again. Nothing is dropped without the subscriber being told.
type Hub struct {
	bus stream.Bus
	reg *refdata.Registry
	m   *metrics.Set
	cfg HubConfig
	log *slog.Logger

	mu     sync.Mutex
	subs   map[string]map[*Subscription]struct{} // instrument → subscribers
	inst   map[string]*instrumentState
	seen   map[string]seqMark // topic + instrument + source → last forwarded sequence
	health map[string]domain.IngestHealth

	progress stream.Progress
	ready    chan struct{} // closed once the live consumer is positioned
}

// Progress returns, per canonical partition, the next offset not yet dispatched.
func (h *Hub) Progress() map[stream.TP]int64 { return h.progress.Snapshot() }

// Ready is closed once the hub is positioned at the end of the log; events
// published after that are delivered to subscribers.
func (h *Hub) Ready() <-chan struct{} { return h.ready }

type instrumentState struct {
	lastEventTime   time.Time // venue clock
	lastReceiveTime time.Time // ingestor clock
	lastArrival     time.Time // gateway clock
	feedGaps        uint64
}

type seqMark struct {
	session string
	seq     uint64
}

func NewHub(bus stream.Bus, reg *refdata.Registry, m *metrics.Set, cfg HubConfig, log *slog.Logger) *Hub {
	if log == nil {
		log = slog.Default()
	}
	return &Hub{
		bus: bus, reg: reg, m: m, cfg: cfg.withDefaults(), log: log.With("component", "gateway"),
		subs: map[string]map[*Subscription]struct{}{}, inst: map[string]*instrumentState{},
		seen: map[string]seqMark{}, health: map[string]domain.IngestHealth{},
		ready: make(chan struct{}),
	}
}

// Subscription is one live subscriber's queue and delivery accounting.
type Subscription struct {
	hub         *Hub
	Instruments []string
	channels    map[string]bool
	out         chan []byte

	mu      sync.Mutex
	pending map[string]uint64 // instrument → events dropped since the last notice
	dropped map[string]uint64 // instrument → events dropped in total
}

// Subscribe registers a subscriber for instruments and event kinds.
func (h *Hub) Subscribe(instruments, kinds []string) *Subscription {
	s := &Subscription{
		hub: h, Instruments: instruments, channels: map[string]bool{},
		out:     make(chan []byte, h.cfg.QueueSize),
		pending: map[string]uint64{}, dropped: map[string]uint64{},
	}
	for _, k := range kinds {
		s.channels[k] = true
	}
	h.mu.Lock()
	for _, id := range instruments {
		if h.subs[id] == nil {
			h.subs[id] = map[*Subscription]struct{}{}
		}
		h.subs[id][s] = struct{}{}
	}
	h.mu.Unlock()
	h.m.WSClients.Inc()
	return s
}

// Close unregisters the subscriber.
func (s *Subscription) Close() {
	h := s.hub
	h.mu.Lock()
	for _, id := range s.Instruments {
		delete(h.subs[id], s)
	}
	h.mu.Unlock()
	h.m.WSClients.Dec()
}

// Out is the subscriber's queue of encoded messages.
func (s *Subscription) Out() <-chan []byte { return s.out }

// offer queues a message without blocking and reports whether it fit.
func (s *Subscription) offer(msg []byte) bool {
	select {
	case s.out <- msg:
		return true
	default:
		return false
	}
}

func (s *Subscription) drop(instrument string) {
	s.mu.Lock()
	s.pending[instrument]++
	s.dropped[instrument]++
	s.mu.Unlock()
	s.hub.m.WSDropped.Inc()
}

// DropNotice tells a subscriber it missed events.
type DropNotice struct {
	Type         string `json:"type"`   // "gap"
	Reason       string `json:"reason"` // "slow_consumer"
	InstrumentID string `json:"instrument_id"`
	Dropped      uint64 `json:"dropped"`
}

// TakeDrops returns a notice for every instrument with undelivered events
// since the previous call. The writer sends these directly, bypassing the
// queue, so a full queue cannot suppress its own drop report.
func (s *Subscription) TakeDrops() []DropNotice {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.pending) == 0 {
		return nil
	}
	out := make([]DropNotice, 0, len(s.pending))
	for id, n := range s.pending {
		out = append(out, DropNotice{Type: domain.KindGap, Reason: "slow_consumer", InstrumentID: id, Dropped: n})
	}
	s.pending = map[string]uint64{}
	sort.Slice(out, func(i, j int) bool { return out[i].InstrumentID < out[j].InstrumentID })
	return out
}

// FeedGapNotice tells subscribers the upstream feed lost sequence numbers.
type FeedGapNotice struct {
	Type         string    `json:"type"`   // "gap"
	Reason       string    `json:"reason"` // "feed_gap"
	Provider     string    `json:"provider"`
	Venue        string    `json:"venue"`
	InstrumentID string    `json:"instrument_id,omitempty"` // empty: any instrument of the venue may be affected
	SeqDomain    string    `json:"seq_domain"`
	FromSeq      uint64    `json:"from_seq"`
	ToSeq        uint64    `json:"to_seq"`
	Missing      uint64    `json:"missing"`
	ReceiveTime  time.Time `json:"receive_time"`
}

// InstrumentStatus is the freshness of one instrument as seen by one subscriber.
type InstrumentStatus struct {
	InstrumentID    string     `json:"instrument_id"`
	LastEventTime   *time.Time `json:"last_event_time"`   // venue clock
	LastReceiveTime *time.Time `json:"last_receive_time"` // ingestor clock
	// AgeMs is gateway time since the gateway last saw an event for the
	// instrument. It is measured on one clock, so skew cannot distort it.
	AgeMs    *int64 `json:"age_ms"`
	Stale    bool   `json:"stale"`
	FeedGaps uint64 `json:"feed_gaps"`
	Dropped  uint64 `json:"dropped"`
}

// Status is the periodic heartbeat sent to a subscriber.
type Status struct {
	Type         string             `json:"type"` // "status"
	ServerTime   time.Time          `json:"server_time"`
	StaleAfterMs int64              `json:"stale_after_ms"`
	Instruments  []InstrumentStatus `json:"instruments"`
}

// Status reports freshness, feed gaps and drops for the subscriber's instruments.
func (s *Subscription) Status() Status {
	st := s.hub.Freshness(s.Instruments)
	s.mu.Lock()
	for i := range st.Instruments {
		st.Instruments[i].Dropped = s.dropped[st.Instruments[i].InstrumentID]
	}
	s.mu.Unlock()
	return st
}

// Freshness reports the state of the given instruments.
func (h *Hub) Freshness(instruments []string) Status {
	now := h.cfg.Now().UTC()
	st := Status{Type: "status", ServerTime: now, StaleAfterMs: h.cfg.StaleAfter.Milliseconds()}
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, id := range instruments {
		is := InstrumentStatus{InstrumentID: id, Stale: true}
		if s, ok := h.inst[id]; ok {
			is.FeedGaps = s.feedGaps
			if !s.lastArrival.IsZero() {
				age := now.Sub(s.lastArrival).Milliseconds()
				et, rt := s.lastEventTime, s.lastReceiveTime
				is.AgeMs, is.LastEventTime, is.LastReceiveTime = &age, &et, &rt
				is.Stale = now.Sub(s.lastArrival) > h.cfg.StaleAfter
			}
		}
		st.Instruments = append(st.Instruments, is)
	}
	return st
}

// IngestHealth returns the latest health record of every ingestor.
func (h *Hub) IngestHealth() []domain.IngestHealth {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := make([]domain.IngestHealth, 0, len(h.health))
	for _, v := range h.health {
		out = append(out, v)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Provider != out[j].Provider {
			return out[i].Provider < out[j].Provider
		}
		return out[i].Source < out[j].Source
	})
	return out
}

// Run consumes canonical events and ingest health until ctx ends. Live
// delivery starts at the end of the log: subscribers get new events, and
// history comes from the query API.
func (h *Hub) Run(ctx context.Context) error {
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		h.consume(ctx, "gateway", stream.ConsumerConfig{Topics: stream.CanonicalTopics, Start: stream.FromLatest}, h.Dispatch)
	}()
	go func() {
		defer wg.Done()
		h.consume(ctx, "", stream.ConsumerConfig{Topics: []string{stream.TopicHealth}, Start: stream.FromEarliest}, func(r stream.Record) {
			var ih domain.IngestHealth
			if json.Unmarshal(r.Value, &ih) == nil {
				h.mu.Lock()
				h.health[string(r.Key)] = ih
				h.mu.Unlock()
			}
		})
	}()
	wg.Wait()
	return nil
}

func (h *Hub) consume(ctx context.Context, lagName string, cfg stream.ConsumerConfig, handle func(stream.Record)) {
	backoff := h.cfg.BackoffMin
	var cons stream.Consumer
	for cons == nil {
		c, err := h.bus.NewConsumer(ctx, cfg)
		if err == nil {
			cons = c
			break
		}
		h.log.Warn("consumer unavailable; retrying", "topics", cfg.Topics, "err", err)
		if !sleepCtx(ctx, backoff) {
			return
		}
		backoff = min(backoff*2, h.cfg.BackoffMax)
	}
	defer cons.Close()
	if lagName != "" {
		h.progress.Init(cons.Positions())
		close(h.ready)
		go stream.WatchLag(ctx, h.bus, h.Progress, h.cfg.LagEvery, stream.LagObserver{
			OnLag: func(tp stream.TP, lag int64) {
				h.m.ConsumerLag.WithLabelValues(lagName, tp.Topic, strconv.Itoa(int(tp.Partition))).Set(float64(lag))
			},
		})
	}
	backoff = h.cfg.BackoffMin
	for {
		recs, err := cons.Poll(ctx)
		if ctx.Err() != nil {
			return
		}
		if err != nil {
			h.log.Warn("poll failed; retrying", "err", err)
			if !sleepCtx(ctx, backoff) {
				return
			}
			backoff = min(backoff*2, h.cfg.BackoffMax)
			continue
		}
		backoff = h.cfg.BackoffMin
		for _, r := range recs {
			handle(r)
		}
		if lagName != "" {
			h.progress.Advance(recs)
		}
	}
}

// Dispatch routes one canonical record to its subscribers.
func (h *Hub) Dispatch(r stream.Record) {
	if r.Topic == stream.TopicGaps {
		h.dispatchGap(r)
		return
	}
	var hd struct {
		Type         string    `json:"type"`
		InstrumentID string    `json:"instrument_id"`
		Session      string    `json:"session"`
		Seq          uint64    `json:"seq"`
		Source       string    `json:"source"`
		EventTime    time.Time `json:"event_time"`
		ReceiveTime  time.Time `json:"receive_time"`
	}
	if err := json.Unmarshal(r.Value, &hd); err != nil || hd.InstrumentID == "" {
		return // the sink dead-letters undecodable records; nothing to deliver
	}
	now := h.cfg.Now()
	h.mu.Lock()
	// The canonical log is at-least-once: after a normalizer restart it can
	// repeat events. Per topic and instrument, sequence numbers only grow
	// within a session, so anything at or below the mark is a repeat.
	key := r.Topic + "\x00" + hd.InstrumentID + "\x00" + hd.Source
	if mark, ok := h.seen[key]; ok && mark.session == hd.Session && hd.Seq <= mark.seq {
		h.mu.Unlock()
		h.m.WSDuplicates.Inc()
		return
	}
	h.seen[key] = seqMark{session: hd.Session, seq: hd.Seq}
	st := h.state(hd.InstrumentID)
	st.lastEventTime, st.lastReceiveTime, st.lastArrival = hd.EventTime, hd.ReceiveTime, now
	targets := make([]*Subscription, 0, len(h.subs[hd.InstrumentID]))
	for s := range h.subs[hd.InstrumentID] {
		if s.channels[hd.Type] {
			targets = append(targets, s)
		}
	}
	h.mu.Unlock()

	for _, s := range targets {
		if s.offer(r.Value) {
			h.m.WSSent.WithLabelValues(hd.Type).Inc()
		} else {
			s.drop(hd.InstrumentID)
		}
	}
	if hd.Source == domain.SourceLive {
		h.m.PipelineLatency.WithLabelValues("delivered").Observe(now.Sub(hd.ReceiveTime).Seconds())
	}
}

// state returns an instrument's state, creating it. Callers hold h.mu.
func (h *Hub) state(id string) *instrumentState {
	st := h.inst[id]
	if st == nil {
		st = &instrumentState{}
		h.inst[id] = st
	}
	return st
}

func (h *Hub) dispatchGap(r stream.Record) {
	var g domain.Gap
	if err := json.Unmarshal(r.Value, &g); err != nil {
		return
	}
	affected := []string{g.InstrumentID}
	if g.InstrumentID == "" {
		affected = h.reg.InVenue(g.Venue)
	}
	msg, _ := json.Marshal(FeedGapNotice{
		Type: domain.KindGap, Reason: "feed_gap", Provider: g.Provider, Venue: g.Venue, InstrumentID: g.InstrumentID,
		SeqDomain: g.SeqDomain, FromSeq: g.FromSeq, ToSeq: g.ToSeq, Missing: g.Missing(), ReceiveTime: g.ReceiveTime,
	})
	h.mu.Lock()
	targets := map[*Subscription]string{} // one notice per subscriber, however many instruments it covers
	for _, id := range affected {
		h.state(id).feedGaps++
		for s := range h.subs[id] {
			if _, ok := targets[s]; !ok {
				targets[s] = id
			}
		}
	}
	h.mu.Unlock()
	for s, id := range targets {
		if s.offer(msg) {
			h.m.WSSent.WithLabelValues(domain.KindGap).Inc()
		} else {
			s.drop(id)
		}
	}
}

func sleepCtx(ctx context.Context, d time.Duration) bool {
	select {
	case <-time.After(d):
		return true
	case <-ctx.Done():
		return false
	}
}
