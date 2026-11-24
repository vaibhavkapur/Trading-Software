package integration_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"arbmonitor/src/alertengine"
	"arbmonitor/src/api"
	"arbmonitor/src/connectors"
	"arbmonitor/src/domain"
	"arbmonitor/src/edgecalculator"
	"arbmonitor/src/metrics"
	"arbmonitor/src/normalizer"
)

func TestHTTPAPIAndDurableAlertStore(t *testing.T) {
	reg, err := normalizer.LoadRegistryFile("../../examples/config/instruments.json")
	if err != nil {
		t.Fatalf("LoadRegistryFile: %v", err)
	}
	fees, err := edgecalculator.LoadFeeScheduleFile("../../examples/config/fees.json")
	if err != nil {
		t.Fatalf("LoadFeeScheduleFile: %v", err)
	}
	balances, err := edgecalculator.LoadBalanceBookFile("../../examples/config/balances.json")
	if err != nil {
		t.Fatalf("LoadBalanceBookFile: %v", err)
	}
	events, err := connectors.ReadFeedJSONL("../../examples/fixtures/recorded_books.jsonl")
	if err != nil {
		t.Fatalf("ReadFeedJSONL: %v", err)
	}

	archivePath := filepath.Join(t.TempDir(), "alerts.jsonl")
	store, err := alertengine.NewJSONLAlertStore(archivePath)
	if err != nil {
		t.Fatalf("NewJSONLAlertStore: %v", err)
	}

	cfg := domain.DefaultAlertConfig()
	cfg.Instruments = []string{"BTC/USDT", "BTC/USD", "ETH/USDT"}
	norm := normalizer.New(reg, cfg.MaxDepthLevels)
	feeds := connectors.NewFeedManager(connectors.NewDefaultAdapters(), norm)
	calc := edgecalculator.New(fees, balances, feeds, edgecalculator.DefaultTransferSchedule())
	collector := metrics.New()
	engine := alertengine.New(feeds, reg, calc, store, collector, cfg)

	var currentTime time.Time
	for _, ev := range events {
		currentTime = ev.ReceivedAt.UTC()
		out := feeds.Ingest(ev)
		if out.Accepted || out.SequenceGap {
			if _, err := engine.EvaluateAll(currentTime); err != nil {
				t.Fatalf("EvaluateAll: %v", err)
			}
		}
	}

	// Verify durable archive persistence across a fresh store handle (simulated restart).
	reloadedStore, err := alertengine.NewJSONLAlertStore(archivePath)
	if err != nil {
		t.Fatalf("reopen JSONLAlertStore: %v", err)
	}
	persisted, err := reloadedStore.List()
	if err != nil {
		t.Fatalf("reloadedStore.List: %v", err)
	}
	if len(persisted) < 3 {
		t.Fatalf("expected at least 3 persisted alerts, got %d", len(persisted))
	}
	for _, a := range persisted {
		if a.Disclaimer != domain.PaperDisclaimer {
			t.Fatalf("missing paper disclaimer on alert %s", a.ID)
		}
		if a.Evidence.BuyBookSnapshot.Asof.IsZero() || a.Evidence.SellBookSnapshot.Asof.IsZero() {
			t.Fatalf("alert %s missing synchronized book timestamps", a.ID)
		}
	}

	// Exercise HTTP API endpoints directly through the http.Handler (no TCP port binding required).
	srv := api.NewServer(feeds, engine, balances, collector, func() time.Time { return currentTime })
	handler := srv.Handler()
	doReq := func(method, target, body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, target, strings.NewReader(body))
		if body != "" {
			req.Header.Set("Content-Type", "application/json")
		}
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		return rec
	}

	// 1. GET /healthz
	rec := doReq(http.MethodGet, "/healthz", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /healthz failed: status=%d body=%s", rec.Code, rec.Body.String())
	}

	// 2. GET /v1/opportunities?min_net_bps=5
	rec = doReq(http.MethodGet, "/v1/opportunities?min_net_bps=5", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /v1/opportunities failed: status=%d body=%s", rec.Code, rec.Body.String())
	}
	var oppBody struct {
		Count         int                  `json:"count"`
		Opportunities []domain.Opportunity `json:"opportunities"`
		Disclaimer    string               `json:"disclaimer"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &oppBody); err != nil {
		t.Fatalf("decode opportunities: %v", err)
	}
	if oppBody.Count == 0 {
		t.Fatal("expected at least 1 active opportunity at end of recorded_books.jsonl")
	}
	if oppBody.Disclaimer != domain.PaperDisclaimer {
		t.Fatalf("expected paper disclaimer, got %q", oppBody.Disclaimer)
	}

	// 3. GET /v1/venues/health
	rec = doReq(http.MethodGet, "/v1/venues/health", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /v1/venues/health failed: status=%d", rec.Code)
	}
	var healthBody struct {
		Venues []domain.VenueHealth `json:"venues"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &healthBody); err != nil {
		t.Fatalf("decode venues health: %v", err)
	}
	if len(healthBody.Venues) != 3 {
		t.Fatalf("expected 3 venues in health response, got %d", len(healthBody.Venues))
	}

	// 4. POST /v1/alerts/config (contract from README)
	rec = doReq(http.MethodPost, "/v1/alerts/config",
		`{"instruments":["BTC/USDT"],"min_net_bps":10,"max_quote_age_ms":750}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("POST /v1/alerts/config failed: status=%d body=%s", rec.Code, rec.Body.String())
	}
	var cfgResp struct {
		Config domain.AlertConfig `json:"config"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &cfgResp); err != nil {
		t.Fatalf("decode config response: %v", err)
	}
	if cfgResp.Config.Version != 2 || cfgResp.Config.MinNetBps.String() != "10" {
		t.Fatalf("unexpected updated config: %+v", cfgResp.Config)
	}

	// Invalid config update should return 400.
	rec = doReq(http.MethodPost, "/v1/alerts/config", `{"min_net_bps":-5}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 on negative min_net_bps, got status=%d", rec.Code)
	}

	// 5. GET /metrics Prometheus exposition format.
	rec = doReq(http.MethodGet, "/metrics", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /metrics failed: status=%d", rec.Code)
	}
	promText := rec.Body.String()
	for _, metricName := range []string{"arb_alerts_emitted_total", "arb_gross_edge_bps_avg", "arb_net_edge_bps_avg", "arb_venue_disconnects_total"} {
		if !strings.Contains(promText, metricName) {
			t.Fatalf("expected Prometheus metric %s in /metrics output", metricName)
		}
	}

	// 6. Alert expiry when simulated clock advances past TTL.
	currentTime = currentTime.Add(10 * time.Second)
	rec = doReq(http.MethodGet, "/v1/alerts?active_only=true", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /v1/alerts failed: status=%d", rec.Code)
	}
	var alertsBody struct {
		Count int `json:"count"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &alertsBody); err != nil {
		t.Fatalf("decode alerts: %v", err)
	}
	if alertsBody.Count != 0 {
		t.Fatalf("expected 0 active alerts after TTL expiry, got %d", alertsBody.Count)
	}
}
