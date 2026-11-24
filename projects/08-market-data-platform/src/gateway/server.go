// Package gateway is the read side of the platform: historical queries over
// HTTP, live subscriptions over WebSocket, entitlements and metrics.
package gateway

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/coder/websocket"

	"mdp/src/domain"
	"mdp/src/metrics"
	"mdp/src/refdata"
	"mdp/src/storage"
)

// ServerConfig tunes the HTTP and WebSocket endpoints.
type ServerConfig struct {
	DefaultLimit   int
	MaxLimit       int
	DefaultWindow  time.Duration // query window when from is omitted
	QueryTimeout   time.Duration
	HeartbeatEvery time.Duration // WebSocket status interval
	WriteTimeout   time.Duration // a subscriber that cannot be written to for this long is disconnected
	MaxInstruments int           // instruments per subscription
	Now            func() time.Time
}

func (c ServerConfig) withDefaults() ServerConfig {
	if c.DefaultLimit <= 0 {
		c.DefaultLimit = 1000
	}
	if c.MaxLimit <= 0 {
		c.MaxLimit = 10000
	}
	if c.DefaultWindow <= 0 {
		c.DefaultWindow = time.Hour
	}
	if c.QueryTimeout <= 0 {
		c.QueryTimeout = 10 * time.Second
	}
	if c.HeartbeatEvery <= 0 {
		c.HeartbeatEvery = time.Second
	}
	if c.WriteTimeout <= 0 {
		c.WriteTimeout = 5 * time.Second
	}
	if c.MaxInstruments <= 0 {
		c.MaxInstruments = 100
	}
	if c.Now == nil {
		c.Now = time.Now
	}
	return c
}

// Server serves the public API.
type Server struct {
	reg   *refdata.Registry
	store storage.Store
	hub   *Hub
	ent   *Entitlements
	m     *metrics.Set
	cfg   ServerConfig
}

func NewServer(reg *refdata.Registry, store storage.Store, hub *Hub, ent *Entitlements, m *metrics.Set, cfg ServerConfig) *Server {
	return &Server{reg: reg, store: store, hub: hub, ent: ent, m: m, cfg: cfg.withDefaults()}
}

// clocks is attached to every response that carries timestamps.
var clocks = map[string]string{
	"event_time":   "venue clock, as supplied by the provider (UTC)",
	"receive_time": "ingestor host clock when the frame was read (UTC)",
}

// Handler returns the API routes.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { w.Write([]byte("ok\n")) })
	mux.Handle("GET /metrics", s.m.Handler())
	mux.HandleFunc("GET /v1/instruments", s.route("instruments", s.instruments))
	mux.HandleFunc("GET /v1/trades", s.route("trades", s.rangeQuery(domain.KindTrade)))
	mux.HandleFunc("GET /v1/quotes", s.route("quotes", s.rangeQuery(domain.KindQuote)))
	mux.HandleFunc("GET /v1/depth", s.route("depth", s.rangeQuery(domain.KindDepth)))
	mux.HandleFunc("GET /v1/gaps", s.route("gaps", s.gaps))
	mux.HandleFunc("GET /v1/health", s.route("health", s.health))
	mux.HandleFunc("GET /v1/stream", s.stream)
	return mux
}

type apiError struct {
	status  int
	code    string
	message string
}

func (e *apiError) Error() string { return e.message }

func badRequest(format string, args ...any) *apiError {
	return &apiError{http.StatusBadRequest, "BAD_REQUEST", fmt.Sprintf(format, args...)}
}

type handler func(p *Principal, r *http.Request) (any, error)

// route wraps a handler with authentication, JSON encoding and metrics.
func (s *Server) route(name string, h handler) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		status := http.StatusOK
		var body any
		p, ok := s.ent.Authenticate(r)
		if !ok {
			status, body = writeErr(&apiError{http.StatusUnauthorized, "UNAUTHENTICATED", "missing or invalid bearer token"})
		} else if v, err := h(p, r); err != nil {
			status, body = writeErr(err)
		} else {
			body = v
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		json.NewEncoder(w).Encode(body)
		s.m.HTTPRequests.WithLabelValues(name, strconv.Itoa(status)).Inc()
		s.m.QueryDuration.WithLabelValues(name).Observe(time.Since(start).Seconds())
	}
}

func writeErr(err error) (int, any) {
	var ae *apiError
	if !errors.As(err, &ae) {
		// Store errors can carry connection details; callers get a generic message.
		ae = &apiError{http.StatusServiceUnavailable, "STORE_UNAVAILABLE", "historical store is unavailable"}
	}
	return ae.status, map[string]any{"error": map[string]string{"code": ae.code, "message": ae.message}}
}

// instrument resolves and authorizes the instrument_id parameter.
func (s *Server) instrument(p *Principal, id string) (*refdata.Instrument, error) {
	if id == "" {
		return nil, badRequest("instrument_id is required")
	}
	inst, ok := s.reg.Get(id)
	if !ok {
		return nil, &apiError{http.StatusNotFound, "UNKNOWN_INSTRUMENT", "unknown instrument_id " + id}
	}
	if !p.Allows(inst.Venue) {
		return nil, &apiError{http.StatusForbidden, "NOT_ENTITLED", "not entitled to venue " + inst.Venue}
	}
	return inst, nil
}

func (s *Server) instruments(p *Principal, r *http.Request) (any, error) {
	out := []domain.Instrument{}
	for _, in := range s.reg.Search(r.URL.Query().Get("search")) {
		if p.Allows(in.Venue) {
			out = append(out, in)
		}
	}
	return map[string]any{"count": len(out), "instruments": out}, nil
}

// window parses from/to. Ranges are half-open [from, to) on event_time.
func (s *Server) window(r *http.Request) (from, to time.Time, err error) {
	q := r.URL.Query()
	to = s.cfg.Now().UTC()
	if v := q.Get("to"); v != "" {
		if to, err = time.Parse(time.RFC3339Nano, v); err != nil {
			return from, to, badRequest("to must be RFC 3339, e.g. 2026-01-05T09:30:00Z")
		}
	}
	from = to.Add(-s.cfg.DefaultWindow)
	if v := q.Get("from"); v != "" {
		if from, err = time.Parse(time.RFC3339Nano, v); err != nil {
			return from, to, badRequest("from must be RFC 3339, e.g. 2026-01-05T09:30:00Z")
		}
	}
	if !from.Before(to) {
		return from, to, badRequest("from must be before to")
	}
	return from.UTC(), to.UTC(), nil
}

func (s *Server) limit(r *http.Request) (int, error) {
	v := r.URL.Query().Get("limit")
	if v == "" {
		return s.cfg.DefaultLimit, nil
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < 1 || n > s.cfg.MaxLimit {
		return 0, badRequest("limit must be between 1 and %d", s.cfg.MaxLimit)
	}
	return n, nil
}

func encodeCursor(t time.Time, tie string) string {
	return base64.RawURLEncoding.EncodeToString([]byte(strconv.FormatInt(t.UnixNano(), 10) + "|" + tie))
}

func decodeCursor(kind, v string) (*storage.Cursor, error) {
	b, err := base64.RawURLEncoding.DecodeString(v)
	if err != nil {
		return nil, badRequest("invalid cursor")
	}
	ns, tie, ok := strings.Cut(string(b), "|")
	n, err := strconv.ParseInt(ns, 10, 64)
	if !ok || err != nil {
		return nil, badRequest("invalid cursor")
	}
	c := &storage.Cursor{EventTime: time.Unix(0, n).UTC()}
	if kind == domain.KindTrade {
		c.TradeID = tie
	} else if c.Seq, err = strconv.ParseUint(tie, 10, 64); err != nil {
		return nil, badRequest("invalid cursor")
	}
	return c, nil
}

func (s *Server) rangeQuery(kind string) handler {
	return func(p *Principal, r *http.Request) (any, error) {
		inst, err := s.instrument(p, r.URL.Query().Get("instrument_id"))
		if err != nil {
			return nil, err
		}
		from, to, err := s.window(r)
		if err != nil {
			return nil, err
		}
		limit, err := s.limit(r)
		if err != nil {
			return nil, err
		}
		q := storage.RangeQuery{InstrumentID: inst.ID, From: from, To: to, Limit: limit}
		if c := r.URL.Query().Get("cursor"); c != "" {
			if q.After, err = decodeCursor(kind, c); err != nil {
				return nil, err
			}
		}
		ctx, cancel := context.WithTimeout(r.Context(), s.cfg.QueryTimeout)
		defer cancel()

		ret := s.store.Retention()
		var (
			name   string
			rows   any
			count  int
			cursor string
			keep   time.Duration
		)
		switch kind {
		case domain.KindTrade:
			v, err := s.store.Trades(ctx, q)
			if err != nil {
				return nil, err
			}
			name, rows, count, keep = "trades", v, len(v), ret.Trades
			if count == limit {
				cursor = encodeCursor(v[count-1].EventTime, v[count-1].TradeID)
			}
		case domain.KindQuote:
			v, err := s.store.Quotes(ctx, q)
			if err != nil {
				return nil, err
			}
			name, rows, count, keep = "quotes", v, len(v), ret.Quotes
			if count == limit {
				cursor = encodeCursor(v[count-1].EventTime, strconv.FormatUint(v[count-1].Seq, 10))
			}
		case domain.KindDepth:
			v, err := s.store.Depth(ctx, q)
			if err != nil {
				return nil, err
			}
			name, rows, count, keep = "depth", v, len(v), ret.Depth
			if count == limit {
				cursor = encodeCursor(v[count-1].EventTime, strconv.FormatUint(v[count-1].Seq, 10))
			}
		}
		resp := map[string]any{
			"instrument_id": inst.ID,
			"tick_size":     inst.TickSize,
			"lot_size":      inst.LotSize,
			"from":          from,
			"to":            to,
			"count":         count,
			name:            rows,
			"next_cursor":   nil,
			"clocks":        clocks,
			"retention":     s.retention(keep, "event_time"),
		}
		if cursor != "" {
			resp["next_cursor"] = cursor
		}
		return resp, nil
	}
}

// retention describes how far back a table can be queried. clock names the
// timestamp rows expire on.
func (s *Server) retention(keep time.Duration, clock string) map[string]any {
	return map[string]any{
		"days":             int(keep / (24 * time.Hour)),
		"oldest_queryable": s.cfg.Now().UTC().Add(-keep).Truncate(24 * time.Hour),
		"measured_on":      clock,
	}
}

func (s *Server) gaps(p *Principal, r *http.Request) (any, error) {
	inst, err := s.instrument(p, r.URL.Query().Get("instrument_id"))
	if err != nil {
		return nil, err
	}
	from, to, err := s.window(r)
	if err != nil {
		return nil, err
	}
	limit, err := s.limit(r)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(r.Context(), s.cfg.QueryTimeout)
	defer cancel()
	rows, err := s.store.Gaps(ctx, storage.GapQuery{InstrumentID: inst.ID, Venue: inst.Venue, From: from, To: to, Limit: limit})
	if err != nil {
		return nil, err
	}
	return map[string]any{
		"instrument_id": inst.ID, "from": from, "to": to, "count": len(rows), "gaps": rows,
		"note":      "from/to filter on receive_time, the ingestor clock when the gap was revealed; a gap without instrument_id is feed-wide and may affect any instrument of the venue",
		"retention": s.retention(s.store.Retention().Gaps, "receive_time"),
	}, nil
}

func (s *Server) health(p *Principal, r *http.Request) (any, error) {
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
	defer cancel()
	storeOK := s.store.Ping(ctx) == nil
	var ids []string
	for _, in := range s.reg.All() {
		if p.Allows(in.Venue) {
			ids = append(ids, in.ID)
		}
	}
	ingest := []domain.IngestHealth{}
	for _, ih := range s.hub.IngestHealth() {
		if prov, ok := s.reg.Provider(ih.Provider); ok && p.Allows(prov.Venue) {
			ingest = append(ingest, ih)
		}
	}
	fresh := s.hub.Freshness(ids)
	return map[string]any{
		"server_time":    fresh.ServerTime,
		"store":          map[string]bool{"ok": storeOK},
		"ingest":         ingest,
		"stale_after_ms": fresh.StaleAfterMs,
		"instruments":    fresh.Instruments,
		"clocks": map[string]string{
			"lag_ms": "receive_time minus event_time of the latest event; spans the venue and ingestor clocks",
			"age_ms": "gateway clock time since the gateway last saw an event for the instrument",
		},
	}, nil
}

var allKinds = []string{domain.KindTrade, domain.KindQuote, domain.KindDepth}

// stream upgrades to a WebSocket and delivers live events.
//
//	WS /v1/stream?instrument_id=venueA:XYZ,venueB:XYZ.B&channels=trade,quote
//
// Messages are JSON text frames, each with a "type":
//
//	subscribed  sent once, first
//	trade | quote | depth   canonical events
//	gap         reason "feed_gap" (upstream sequence loss) or
//	            "slow_consumer" (this connection missed events)
//	status      periodic freshness per instrument
func (s *Server) stream(w http.ResponseWriter, r *http.Request) {
	fail := func(err error) {
		status, body := writeErr(err)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		json.NewEncoder(w).Encode(body)
		s.m.HTTPRequests.WithLabelValues("stream", strconv.Itoa(status)).Inc()
	}
	p, ok := s.ent.Authenticate(r)
	if !ok {
		fail(&apiError{http.StatusUnauthorized, "UNAUTHENTICATED", "missing or invalid bearer token"})
		return
	}
	q := r.URL.Query()
	var ids []string
	seen := map[string]bool{}
	for _, id := range strings.Split(q.Get("instrument_id"), ",") {
		if id = strings.TrimSpace(id); id == "" || seen[id] {
			continue
		}
		if _, err := s.instrument(p, id); err != nil {
			fail(err)
			return
		}
		seen[id] = true
		ids = append(ids, id)
	}
	if len(ids) == 0 {
		fail(badRequest("instrument_id is required (comma-separated for several)"))
		return
	}
	if len(ids) > s.cfg.MaxInstruments {
		fail(badRequest("at most %d instruments per subscription", s.cfg.MaxInstruments))
		return
	}
	kinds := allKinds
	if v := q.Get("channels"); v != "" {
		kinds = nil
		for _, k := range strings.Split(v, ",") {
			switch k = strings.TrimSpace(k); k {
			case domain.KindTrade, domain.KindQuote, domain.KindDepth:
				kinds = append(kinds, k)
			default:
				fail(badRequest("channels must be a comma-separated subset of trade,quote,depth"))
				return
			}
		}
	}

	conn, err := websocket.Accept(w, r, nil)
	if err != nil {
		return // Accept has already written the error response
	}
	defer conn.CloseNow()
	s.m.HTTPRequests.WithLabelValues("stream", "101").Inc()
	// Subscribers do not send data; CloseRead handles pings and close frames
	// and cancels ctx when the peer goes away.
	ctx := conn.CloseRead(r.Context())

	sub := s.hub.Subscribe(ids, kinds)
	defer sub.Close()

	write := func(v any) error {
		b, ok := v.([]byte)
		if !ok {
			var err error
			if b, err = json.Marshal(v); err != nil {
				return err
			}
		}
		wctx, cancel := context.WithTimeout(ctx, s.cfg.WriteTimeout)
		defer cancel()
		return conn.Write(wctx, websocket.MessageText, b)
	}
	if write(map[string]any{
		"type": "subscribed", "instruments": ids, "channels": kinds,
		"heartbeat_ms": s.cfg.HeartbeatEvery.Milliseconds(), "stale_after_ms": s.hub.cfg.StaleAfter.Milliseconds(),
		"queue": s.hub.cfg.QueueSize, "clocks": clocks,
	}) != nil {
		return
	}
	heartbeat := time.NewTicker(s.cfg.HeartbeatEvery)
	defer heartbeat.Stop()
	for {
		var err error
		select {
		case <-ctx.Done():
			conn.Close(websocket.StatusNormalClosure, "")
			return
		case msg := <-sub.Out():
			err = write(msg)
		case <-heartbeat.C:
			err = write(sub.Status())
		}
		// Report drops outside the queue so they are sent even when it is full.
		for _, n := range sub.TakeDrops() {
			if err == nil {
				err = write(n)
			}
		}
		if err != nil {
			return
		}
	}
}
