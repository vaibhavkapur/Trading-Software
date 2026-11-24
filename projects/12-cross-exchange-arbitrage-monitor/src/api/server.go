// Package api exposes the read-only HTTP endpoints for querying arbitrage opportunities,
// alerts, venue health, balances, alert configuration, and Prometheus metrics.
package api

import (
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"time"

	"arbmonitor/src/alertengine"
	"arbmonitor/src/connectors"
	"arbmonitor/src/domain"
	"arbmonitor/src/edgecalculator"
	"arbmonitor/src/fixed"
	"arbmonitor/src/metrics"
)

// Server wires HTTP routes to the arbitrage monitor components.
type Server struct {
	feeds    *connectors.FeedManager
	engine   *alertengine.Engine
	balances *edgecalculator.BalanceBook
	metrics  *metrics.Collector
	clock    func() time.Time
	mux      *http.ServeMux
}

// NewServer constructs a new HTTP API Server.
// If clock is nil, time.Now().UTC is used.
func NewServer(
	feeds *connectors.FeedManager,
	engine *alertengine.Engine,
	balances *edgecalculator.BalanceBook,
	collector *metrics.Collector,
	clock func() time.Time,
) *Server {
	if clock == nil {
		clock = func() time.Time { return time.Now().UTC() }
	}
	s := &Server{
		feeds:    feeds,
		engine:   engine,
		balances: balances,
		metrics:  collector,
		clock:    clock,
		mux:      http.NewServeMux(),
	}
	s.registerRoutes()
	return s
}

func (s *Server) registerRoutes() {
	s.mux.HandleFunc("GET /healthz", s.handleHealthz)
	s.mux.HandleFunc("GET /v1/opportunities", s.handleOpportunities)
	s.mux.HandleFunc("GET /v1/alerts", s.handleAlerts)
	s.mux.HandleFunc("GET /v1/venues/health", s.handleVenuesHealth)
	s.mux.HandleFunc("GET /v1/alerts/config", s.handleGetAlertConfig)
	s.mux.HandleFunc("POST /v1/alerts/config", s.handlePostAlertConfig)
	s.mux.HandleFunc("GET /v1/balances", s.handleBalances)
	s.mux.HandleFunc("GET /v1/metrics", s.handleMetricsJSON)
	s.mux.HandleFunc("GET /metrics", s.handleMetricsProm)
}

// Handler returns the http.Handler for this API server.
func (s *Server) Handler() http.Handler {
	return s.mux
}

func (s *Server) handleHealthz(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"status":     "ok",
		"mode":       "paper_readonly",
		"disclaimer": domain.PaperDisclaimer,
	})
}

func (s *Server) handleOpportunities(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	minNetBps := fixed.Zero
	if raw := strings.TrimSpace(q.Get("min_net_bps")); raw != "" {
		parsed, err := fixed.Parse(raw)
		if err != nil || parsed.IsNegative() {
			writeError(w, http.StatusBadRequest, "invalid min_net_bps query parameter")
			return
		}
		minNetBps = parsed
	}
	instrument := strings.TrimSpace(q.Get("instrument"))
	now := s.clock().UTC()
	opps := s.engine.ActiveOpportunities(now, minNetBps, instrument)
	if opps == nil {
		opps = []domain.Opportunity{}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"asof":          now,
		"count":         len(opps),
		"opportunities": opps,
		"disclaimer":    domain.PaperDisclaimer,
	})
}

func (s *Server) handleAlerts(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	activeOnly := strings.EqualFold(q.Get("active_only"), "true")
	limit := 0
	if raw := strings.TrimSpace(q.Get("limit")); raw != "" {
		v, err := strconv.Atoi(raw)
		if err != nil || v < 0 {
			writeError(w, http.StatusBadRequest, "invalid limit query parameter")
			return
		}
		limit = v
	}
	s.engine.ExpireAlerts(s.clock().UTC())
	alerts := s.engine.Alerts(activeOnly, limit)
	if alerts == nil {
		alerts = []domain.Alert{}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"count":      len(alerts),
		"alerts":     alerts,
		"disclaimer": domain.PaperDisclaimer,
	})
}

func (s *Server) handleVenuesHealth(w http.ResponseWriter, _ *http.Request) {
	now := s.clock().UTC()
	cfg := s.engine.Config()
	venues := s.feeds.VenueHealth(now, cfg.MaxQuoteAgeMs)
	if venues == nil {
		venues = []domain.VenueHealth{}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"asof":             now,
		"max_quote_age_ms": cfg.MaxQuoteAgeMs,
		"venues":           venues,
	})
}

func (s *Server) handleGetAlertConfig(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, s.engine.Config())
}

type updateConfigRequest struct {
	Instruments        *[]string      `json:"instruments,omitempty"`
	MinNetBps          *fixed.Decimal `json:"min_net_bps,omitempty"`
	MaxQuoteAgeMs      *int64         `json:"max_quote_age_ms,omitempty"`
	MaxLegSkewMs       *int64         `json:"max_leg_skew_ms,omitempty"`
	MaxClockSkewMs     *int64         `json:"max_clock_skew_ms,omitempty"`
	CooldownMs         *int64         `json:"cooldown_ms,omitempty"`
	AlertTTLMs         *int64         `json:"alert_ttl_ms,omitempty"`
	MaxDepthLevels     *int           `json:"max_depth_levels,omitempty"`
	ModeledSlippageBps *fixed.Decimal `json:"modeled_slippage_bps,omitempty"`
	EnforceInventory   *bool          `json:"enforce_inventory,omitempty"`
}

func (s *Server) handlePostAlertConfig(w http.ResponseWriter, r *http.Request) {
	defer r.Body.Close()
	var req updateConfigRequest
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON payload: "+err.Error())
		return
	}

	next := s.engine.Config()
	if req.Instruments != nil {
		next.Instruments = *req.Instruments
	}
	if req.MinNetBps != nil {
		next.MinNetBps = *req.MinNetBps
	}
	if req.MaxQuoteAgeMs != nil {
		next.MaxQuoteAgeMs = *req.MaxQuoteAgeMs
	}
	if req.MaxLegSkewMs != nil {
		next.MaxLegSkewMs = *req.MaxLegSkewMs
	}
	if req.MaxClockSkewMs != nil {
		next.MaxClockSkewMs = *req.MaxClockSkewMs
	}
	if req.CooldownMs != nil {
		next.CooldownMs = *req.CooldownMs
	}
	if req.AlertTTLMs != nil {
		next.AlertTTLMs = *req.AlertTTLMs
	}
	if req.MaxDepthLevels != nil {
		next.MaxDepthLevels = *req.MaxDepthLevels
	}
	if req.ModeledSlippageBps != nil {
		next.ModeledSlippageBps = *req.ModeledSlippageBps
	}
	if req.EnforceInventory != nil {
		next.EnforceInventory = *req.EnforceInventory
	}

	updated, err := s.engine.UpdateConfig(next)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"config":     updated,
		"disclaimer": domain.PaperDisclaimer,
	})
}

func (s *Server) handleBalances(w http.ResponseWriter, _ *http.Request) {
	var items []domain.VenueBalance
	if s.balances != nil {
		items = s.balances.All()
	}
	if items == nil {
		items = []domain.VenueBalance{}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"balances": items,
		"note":     "Pre-funded balances per venue; cross-venue balance teleportation is explicitly disabled.",
	})
}

func (s *Server) handleMetricsJSON(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, s.metrics.Snapshot())
}

func (s *Server) handleMetricsProm(w http.ResponseWriter, _ *http.Request) {
	now := s.clock().UTC()
	venues := s.feeds.VenueHealth(now, s.engine.Config().MaxQuoteAgeMs)
	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(s.metrics.PrometheusText(venues)))
}

func writeJSON(w http.ResponseWriter, status int, payload any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	_ = enc.Encode(payload)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}
