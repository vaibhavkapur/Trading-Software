package integration_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/trading-software-projects/broker-reliability-reconciliation/src/api"
	"github.com/trading-software-projects/broker-reliability-reconciliation/src/connectors"
	"github.com/trading-software-projects/broker-reliability-reconciliation/src/domain"
	"github.com/trading-software-projects/broker-reliability-reconciliation/src/identity"
	"github.com/trading-software-projects/broker-reliability-reconciliation/src/issues"
	"github.com/trading-software-projects/broker-reliability-reconciliation/src/metrics"
	"github.com/trading-software-projects/broker-reliability-reconciliation/src/reconcile"
	"github.com/trading-software-projects/broker-reliability-reconciliation/src/scheduler"
)

func fixturePath(t *testing.T) string {
	t.Helper()
	return filepath.Join("..", "..", "examples", "fixtures", "paper_day.json")
}

func doReq(h http.Handler, method, target, body string, headers map[string]string) *httptest.ResponseRecorder {
	var bodyReader io.Reader
	if body != "" {
		bodyReader = strings.NewReader(body)
	}
	req := httptest.NewRequest(method, target, bodyReader)
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestEndToEndRESTAPIAndRestartPersistence(t *testing.T) {
	ds, err := connectors.LoadFixture(fixturePath(t))
	if err != nil {
		t.Fatalf("load fixture: %v", err)
	}

	stateDir := t.TempDir()
	store, err := issues.NewStore(stateDir)
	if err != nil {
		t.Fatalf("create store: %v", err)
	}

	// PageSize=1 verifies cursor pagination across all internal and broker endpoints
	internalConn := connectors.NewSimulatedInternalConnector(ds, 1)
	brokerConn := connectors.NewSimulatedBrokerConnector(ds, 1)
	// Inject 1 transient rate limit to test connector retry recovery
	brokerConn.SetFaults(connectors.FaultProfile{TransientFailures: 1})

	engine := reconcile.NewEngine(identity.NewMapper(ds.SymbolAliases))
	collector := metrics.NewCollector()
	runner := scheduler.NewRunner(internalConn, brokerConn, engine, store, collector).WithPageSize(1)
	handler := api.NewServer(runner, store, collector, nil, nil)

	// 1. Trigger POST /v1/reconciliation/runs
	rec := doReq(handler, http.MethodPost, "/v1/reconciliation/runs", `{"account_id":"a-1","asof":"2026-01-31T16:00:00Z"}`, nil)
	if rec.Code != http.StatusCreated {
		t.Fatalf("expected 201 Created, got %d: %s", rec.Code, rec.Body.String())
	}

	var runResp struct {
		Run    domain.ReconRun     `json:"run"`
		Issues []domain.ReconIssue `json:"issues"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &runResp); err != nil {
		t.Fatalf("decode run response: %v", err)
	}
	if runResp.Run.Status != domain.RunStatusCompleted || len(runResp.Issues) == 0 {
		t.Fatalf("expected completed run with issues, got status=%s count=%d", runResp.Run.Status, len(runResp.Issues))
	}

	// 2. GET /v1/reconciliation/runs/{id}
	rec = doReq(handler, http.MethodGet, "/v1/reconciliation/runs/"+runResp.Run.ID, "", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 OK, got %d", rec.Code)
	}

	// 3. GET /v1/reconciliation/issues?status=open
	rec = doReq(handler, http.MethodGet, "/v1/reconciliation/issues?status=open", "", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 OK, got %d", rec.Code)
	}
	var listPayload struct {
		Count  int                 `json:"count"`
		Issues []domain.ReconIssue `json:"issues"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &listPayload); err != nil {
		t.Fatalf("decode issues: %v", err)
	}
	if listPayload.Count != len(runResp.Issues) {
		t.Fatalf("expected %d open issues, got %d", len(runResp.Issues), listPayload.Count)
	}

	target := listPayload.Issues[0]

	// 4. Assign & Resolve an issue
	rec = doReq(handler, http.MethodPost, "/v1/reconciliation/issues/"+target.ID+"/assign",
		`{"owner":"ops-claire","actor":"ops-lead"}`, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("assign failed: status=%d body=%s", rec.Code, rec.Body.String())
	}

	rec = doReq(handler, http.MethodPost, "/v1/reconciliation/issues/"+target.ID+"/resolve",
		`{"resolution":"late_broker_event","evidence":"Confirmed with broker desk ticket #771","actor":"ops-claire"}`, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 on resolve, got %d: %s", rec.Code, rec.Body.String())
	}
	var resolvedIssue domain.ReconIssue
	if err := json.Unmarshal(rec.Body.Bytes(), &resolvedIssue); err != nil {
		t.Fatalf("decode resolved issue: %v", err)
	}
	if resolvedIssue.State != domain.IssueStateResolved || resolvedIssue.Evidence.Summary != target.Evidence.Summary {
		t.Fatalf("resolved issue state or immutable evidence mismatch")
	}

	// 5. Verify Prometheus /metrics exposition contains all required metrics
	rec = doReq(handler, http.MethodGet, "/metrics", "", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 on /metrics, got %d", rec.Code)
	}
	metricsText := rec.Body.String()
	for _, metricName := range []string{
		"recon_unreconciled_issues",
		"recon_monetary_exposure",
		"recon_position_drift_abs",
		"recon_mttd_seconds",
		"recon_mttr_seconds",
		"recon_late_fill_arrival_seconds_bucket",
		"recon_runs_total",
	} {
		if !strings.Contains(metricsText, metricName) {
			t.Errorf("expected /metrics to contain %q", metricName)
		}
	}

	// 6. Verify Operator UI loads with PAPER / SIMULATED banner
	rec = doReq(handler, http.MethodGet, "/", "", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 on /, got %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "PAPER / SIMULATED") {
		t.Fatalf("expected operator UI to display PAPER / SIMULATED banner")
	}

	// 7. Simulate process restart: open a brand new Store pointing at the same stateDir
	reloadedStore, err := issues.NewStore(stateDir)
	if err != nil {
		t.Fatalf("reload store after restart: %v", err)
	}
	reloadedRun, err := reloadedStore.GetRun(runResp.Run.ID)
	if err != nil {
		t.Fatalf("run missing after restart: %v", err)
	}
	if reloadedRun.InputDigest != runResp.Run.InputDigest {
		t.Fatalf("run digest changed after restart")
	}
	reloadedIssue, err := reloadedStore.GetIssue(target.ID)
	if err != nil {
		t.Fatalf("issue missing after restart: %v", err)
	}
	if reloadedIssue.State != domain.IssueStateResolved || reloadedIssue.Owner != "ops-claire" {
		t.Fatalf("issue state/owner lost after restart: state=%s owner=%s", reloadedIssue.State, reloadedIssue.Owner)
	}
	if reloadedIssue.Evidence.Summary != target.Evidence.Summary || len(reloadedIssue.History) < 3 {
		t.Fatalf("evidence or audit history lost after restart")
	}
}

func TestAuthenticationAndAuthorizationBoundary(t *testing.T) {
	ds, err := connectors.LoadFixture(fixturePath(t))
	if err != nil {
		t.Fatalf("load fixture: %v", err)
	}
	store, _ := issues.NewStore("")
	runner := scheduler.NewRunner(
		connectors.NewSimulatedInternalConnector(ds, 10),
		connectors.NewSimulatedBrokerConnector(ds, 10),
		reconcile.NewEngine(identity.NewMapper(ds.SymbolAliases)),
		store,
		metrics.NewCollector(),
	)

	authn := api.NewStaticAPIKeyAuthenticator([]api.APIKeyEntry{
		{Key: "test-viewer-token", Principal: api.Principal{Subject: "auditor-1", Role: api.RoleViewer}},
		{Key: "test-operator-token", Principal: api.Principal{Subject: "ops-1", Role: api.RoleOperator}},
	})
	handler := api.NewServer(runner, store, metrics.NewCollector(), authn, api.RBACAuthorizer{})

	// Unauthenticated request -> 401
	rec := doReq(handler, http.MethodGet, "/v1/reconciliation/issues", "", nil)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 Unauthorized, got %d", rec.Code)
	}

	// Viewer can read issues -> 200
	rec = doReq(handler, http.MethodGet, "/v1/reconciliation/issues", "", map[string]string{"X-API-Key": "test-viewer-token"})
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 for viewer read, got %d", rec.Code)
	}

	// Viewer cannot trigger reconciliation runs -> 403
	rec = doReq(handler, http.MethodPost, "/v1/reconciliation/runs", `{"account_id":"a-1","asof":"2026-01-31T16:00:00Z"}`,
		map[string]string{"X-API-Key": "test-viewer-token"})
	if rec.Code != http.StatusForbidden {
		t.Fatalf("expected 403 Forbidden for viewer run creation, got %d", rec.Code)
	}

	// Operator can trigger runs -> 201
	rec = doReq(handler, http.MethodPost, "/v1/reconciliation/runs", `{"account_id":"a-1","asof":"2026-01-31T16:00:00Z"}`,
		map[string]string{"X-API-Key": "test-operator-token"})
	if rec.Code != http.StatusCreated {
		t.Fatalf("expected 201 Created for operator run creation, got %d", rec.Code)
	}
}

func TestPeriodicSchedulerExecutesAndStopsCleanly(t *testing.T) {
	ds, err := connectors.LoadFixture(fixturePath(t))
	if err != nil {
		t.Fatalf("load fixture: %v", err)
	}
	store, _ := issues.NewStore("")
	runner := scheduler.NewRunner(
		connectors.NewSimulatedInternalConnector(ds, 10),
		connectors.NewSimulatedBrokerConnector(ds, 10),
		reconcile.NewEngine(identity.NewMapper(ds.SymbolAliases)),
		store,
		metrics.NewCollector(),
	)

	ctx, cancel := context.WithCancel(context.Background())
	runner.StartPeriodic(ctx, ds.AccountID, 15*time.Millisecond, "", func() time.Time { return ds.DefaultAsOf })

	deadline := time.Now().Add(500 * time.Millisecond)
	for time.Now().Before(deadline) {
		if len(store.ListRuns()) > 0 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	cancel()

	runs := store.ListRuns()
	if len(runs) == 0 {
		t.Fatal("expected periodic scheduler to execute at least one run")
	}
	if runs[0].Trigger != "scheduled" {
		t.Fatalf("expected trigger 'scheduled', got %q", runs[0].Trigger)
	}
}
