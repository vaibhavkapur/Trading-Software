package integration_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/trading-software-projects/agentic-trading-guardrails/src/pkg/gateway"
	"github.com/trading-software-projects/agentic-trading-guardrails/src/pkg/types"
)

func TestFullHTTPLifecycleAndAuditReplay(t *testing.T) {
	dir := t.TempDir()
	clk := types.NewFixedClock(time.Date(2026, 6, 15, 15, 0, 0, 0, time.UTC))

	gw, err := gateway.New(gateway.Config{
		Clock:          clk,
		AuditLogPath:   filepath.Join(dir, "audit.jsonl"),
		StateStorePath: filepath.Join(dir, "state.json"),
	})
	if err != nil {
		t.Fatalf("new gateway: %v", err)
	}
	if err := gw.SeedDemoIdentities(); err != nil {
		t.Fatalf("seed identities: %v", err)
	}

	h := gw.NewHTTPHandler()

	// 1. Check JWKS & Audit UI
	jwksRec := doHTTP(t, h, http.MethodGet, "/.well-known/jwks.json", "", "")
	if jwksRec.Code != http.StatusOK || !strings.Contains(jwksRec.Body.String(), `"Ed25519"`) {
		t.Fatalf("jwks failed: code=%d body=%s", jwksRec.Code, jwksRec.Body.String())
	}

	uiRec := doHTTP(t, h, http.MethodGet, "/", "", "")
	if uiRec.Code != http.StatusOK || !strings.Contains(uiRec.Body.String(), "PAPER / SIMULATED ONLY") {
		t.Fatalf("ui failed: code=%d", uiRec.Code)
	}

	// 2. Issue tokens for agent-1 and principal-alice
	agentTok := issueTokenHTTP(t, h, "agent-1")
	principalTok := issueTokenHTTP(t, h, "principal-alice")

	// 3. Standalone /v1/authorize matching README contract (with notional_usd)
	authBody := `{"agent_id":"agent-1","action":"order:propose","resource":{"account":"a-1","symbol":"SPY","notional_usd":500}}`
	authRec := doHTTP(t, h, http.MethodPost, "/v1/authorize", agentTok, authBody)
	if authRec.Code != http.StatusOK {
		t.Fatalf("expected 200 on authorize, got %d: %s", authRec.Code, authRec.Body.String())
	}

	// 4. Tool permission gateway call (market.quote)
	toolRec := doHTTP(t, h, http.MethodPost, "/v1/tools/call", agentTok, `{"tool":"market.quote","arguments":{"symbol":"SPY"}}`)
	if toolRec.Code != http.StatusOK {
		t.Fatalf("expected 200 on market.quote tool, got %d: %s", toolRec.Code, toolRec.Body.String())
	}

	// 5. Propose order via /v1/proposals
	propBody := `{"proposal_id":"p-1","account_id":"a-1","symbol":"SPY","side":"BUY","order_type":"LIMIT","quantity":1,"limit_price_ticks":5100000,"correlation_id":"corr-e2e-1"}`
	propRec := doHTTP(t, h, http.MethodPost, "/v1/proposals", agentTok, propBody)
	if propRec.Code != http.StatusCreated {
		t.Fatalf("expected 201 on proposal, got %d: %s", propRec.Code, propRec.Body.String())
	}
	var propResult gateway.ProposeOrderResult
	_ = json.Unmarshal(propRec.Body.Bytes(), &propResult)
	if propResult.Proposal.TermsHash == "" {
		t.Fatalf("expected canonical terms_hash on proposal")
	}

	// 6. Approve via /v1/approvals as principal-alice
	aprBody, _ := json.Marshal(map[string]any{
		"proposal_id":    "p-1",
		"proposal_hash":  propResult.Proposal.TermsHash,
		"correlation_id": "corr-e2e-1",
	})
	aprRec := doHTTP(t, h, http.MethodPost, "/v1/approvals", principalTok, string(aprBody))
	if aprRec.Code != http.StatusCreated {
		t.Fatalf("expected 201 on approval, got %d: %s", aprRec.Code, aprRec.Body.String())
	}
	var grant types.ApprovalGrant
	_ = json.Unmarshal(aprRec.Body.Bytes(), &grant)

	// 7. Execute via /v1/execute as agent-1
	execBody, _ := json.Marshal(map[string]any{
		"proposal_id":    "p-1",
		"approval_grant": grant.Token,
		"correlation_id": "corr-e2e-1",
	})
	execRec := doHTTP(t, h, http.MethodPost, "/v1/execute", agentTok, string(execBody))
	if execRec.Code != http.StatusOK {
		t.Fatalf("expected 200 on execute, got %d: %s", execRec.Code, execRec.Body.String())
	}
	var execResult gateway.ExecuteResult
	_ = json.Unmarshal(execRec.Body.Bytes(), &execResult)
	if execResult.Receipt.Environment != "PAPER_SIMULATED" || execResult.Receipt.Status != "FILLED_PAPER" {
		t.Fatalf("unexpected paper receipt: %+v", execResult.Receipt)
	}

	// 8. Query /v1/audit?proposal_id=p-1 and /v1/audit/replay
	audRec := doHTTP(t, h, http.MethodGet, "/v1/audit?proposal_id=p-1", "", "")
	if audRec.Code != http.StatusOK {
		t.Fatalf("audit query failed: %d", audRec.Code)
	}
	var audData struct {
		ChainVerified bool `json:"chain_verified"`
		Count         int  `json:"count"`
	}
	_ = json.Unmarshal(audRec.Body.Bytes(), &audData)
	if !audData.ChainVerified || audData.Count < 5 {
		t.Fatalf("expected verified audit chain with >=5 events, got verified=%v count=%d", audData.ChainVerified, audData.Count)
	}

	replayRec := doHTTP(t, h, http.MethodGet, "/v1/audit/replay?proposal_id=p-1", "", "")
	if replayRec.Code != http.StatusOK || !strings.Contains(replayRec.Body.String(), `"principal-alice"`) {
		t.Fatalf("audit replay failed: code=%d body=%s", replayRec.Code, replayRec.Body.String())
	}

	// 9. Verify Prometheus metrics endpoint
	metRec := doHTTP(t, h, http.MethodGet, "/v1/metrics?format=prometheus", "", "")
	if metRec.Code != http.StatusOK || !strings.Contains(metRec.Body.String(), "guardrails_broker_write_pep_coverage_pct 100.00") {
		t.Fatalf("expected 100%% PEP coverage metric, got code=%d body=%s", metRec.Code, metRec.Body.String())
	}

	// 10. Simulate service RESTART with same data dir:
	// Verify audit chain reloads cleanly, daily notional persists, AND consumed grant cannot be replayed after restart!
	gw2, err := gateway.New(gateway.Config{
		Clock:          clk,
		AuditLogPath:   filepath.Join(dir, "audit.jsonl"),
		StateStorePath: filepath.Join(dir, "state.json"),
	})
	if err != nil {
		t.Fatalf("restart gateway: %v", err)
	}
	if err := gw2.Audit.VerifyChain(); err != nil {
		t.Fatalf("audit chain verification after restart failed: %v", err)
	}
	if gw2.Store.GetDailyNotional("a-1", clk.Now()) != 51_000 {
		t.Fatalf("expected persisted daily notional 51000, got %d", gw2.Store.GetDailyNotional("a-1", clk.Now()))
	}
	if _, ok := gw2.Store.ConsumedGrants()[grant.ID]; !ok {
		t.Fatalf("expected consumed grant %s persisted across restart", grant.ID)
	}

	// Tamper with audit.jsonl on disk and verify restart detects corruption!
	rawAudit, _ := os.ReadFile(filepath.Join(dir, "audit.jsonl"))
	tamperedAudit := strings.Replace(string(rawAudit), "FILLED_PAPER", "TAMPERED_FILL", 1)
	_ = os.WriteFile(filepath.Join(dir, "audit.jsonl"), []byte(tamperedAudit), 0o600)
	if _, err := gateway.New(gateway.Config{
		Clock:          clk,
		AuditLogPath:   filepath.Join(dir, "audit.jsonl"),
		StateStorePath: filepath.Join(dir, "state.json"),
	}); err == nil {
		t.Fatalf("expected tampered audit.jsonl to fail hash-chain verification on startup")
	}
}

func TestConcurrentApprovalReplayRace(t *testing.T) {
	clk := types.NewFixedClock(time.Date(2026, 6, 15, 15, 0, 0, 0, time.UTC))
	gw, err := gateway.New(gateway.Config{Clock: clk})
	if err != nil {
		t.Fatalf("new gw: %v", err)
	}
	_ = gw.SeedDemoIdentities()

	agentTok, _, _ := gw.Identity.IssueToken("agent-1", nil, 10*time.Minute)
	principalTok, _, _ := gw.Identity.IssueToken("principal-alice", nil, 10*time.Minute)

	propRes, err := gw.ProposeOrder(agentTok, gateway.ProposeOrderRequest{
		ProposalID:      "p-race",
		AccountID:       "a-1",
		Symbol:          "SPY",
		Side:            types.SideBuy,
		OrderType:       types.OrderTypeLimit,
		Quantity:        1,
		LimitPriceTicks: 510_0000,
	})
	if err != nil {
		t.Fatalf("propose: %v", err)
	}
	grant, err := gw.ApproveProposal(principalTok, gateway.ApproveRequest{
		ProposalID:   propRes.Proposal.ID,
		ProposalHash: propRes.Proposal.TermsHash,
	})
	if err != nil {
		t.Fatalf("approve: %v", err)
	}

	const workers = 20
	var successes atomic.Int32
	var failures atomic.Int32
	var wg sync.WaitGroup
	start := make(chan struct{})

	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			_, execErr := gw.ExecuteProposal(agentTok, gateway.ExecuteRequest{
				ProposalID:    propRes.Proposal.ID,
				ApprovalGrant: grant.Token,
			})
			if execErr == nil {
				successes.Add(1)
			} else {
				failures.Add(1)
			}
		}()
	}
	close(start)
	wg.Wait()

	if successes.Load() != 1 || failures.Load() != workers-1 {
		t.Fatalf("expected strictly 1 execution and %d replay denials, got successes=%d failures=%d",
			workers-1, successes.Load(), failures.Load())
	}
	snap := gw.Metrics.Snapshot()
	if snap.ApprovalViolations["approval_replayed"] != workers-1 {
		t.Fatalf("expected %d approval_replayed metrics, got %d", workers-1, snap.ApprovalViolations["approval_replayed"])
	}
}

func issueTokenHTTP(t *testing.T, h http.Handler, agentID string) string {
	t.Helper()
	body, _ := json.Marshal(map[string]any{"agent_id": agentID, "ttl_seconds": 600})
	rec := doHTTP(t, h, http.MethodPost, "/v1/identities/tokens", "", string(body))
	if rec.Code != http.StatusOK {
		t.Fatalf("issue token for %s failed: %d %s", agentID, rec.Code, rec.Body.String())
	}
	var out struct {
		Token string `json:"token"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	return out.Token
}

func doHTTP(t *testing.T, h http.Handler, method, path, token, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}
