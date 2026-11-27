package integration_test

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/vaibhavkapur/Trading-Software/projects/17-multi-broker-trading-gateway/src/adapters/mock"
	"github.com/vaibhavkapur/Trading-Software/projects/17-multi-broker-trading-gateway/src/api"
	"github.com/vaibhavkapur/Trading-Software/projects/17-multi-broker-trading-gateway/src/broker"
	"github.com/vaibhavkapur/Trading-Software/projects/17-multi-broker-trading-gateway/src/domain"
	"github.com/vaibhavkapur/Trading-Software/projects/17-multi-broker-trading-gateway/src/gateway"
	"github.com/vaibhavkapur/Trading-Software/projects/17-multi-broker-trading-gateway/src/ratelimit"
	"github.com/vaibhavkapur/Trading-Software/projects/17-multi-broker-trading-gateway/src/reconciler"
	"github.com/vaibhavkapur/Trading-Software/projects/17-multi-broker-trading-gateway/src/security"
	"github.com/vaibhavkapur/Trading-Software/projects/17-multi-broker-trading-gateway/src/storage"
)

type hijackRecorder struct {
	header http.Header
	code   int
	conn   net.Conn
}

func (h *hijackRecorder) Header() http.Header         { return h.header }
func (h *hijackRecorder) Write(b []byte) (int, error) { return len(b), nil }
func (h *hijackRecorder) WriteHeader(code int)        { h.code = code }
func (h *hijackRecorder) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	return h.conn, bufio.NewReadWriter(bufio.NewReader(h.conn), bufio.NewWriter(h.conn)), nil
}

func TestRESTAPIAndWebSocketEventStream(t *testing.T) {
	caps, _ := domain.LoadCapabilityMatrix(filepath.Join("..", "..", "config", "capabilities.json"))
	insts, _ := domain.LoadInstrumentMapper(filepath.Join("..", "..", "config", "instruments.json"))
	policy, _ := security.LoadExecutionPolicy(filepath.Join("..", "..", "config", "policy.json"))

	clk := ratelimit.NewManualClock(time.Date(2026, 11, 27, 13, 0, 0, 0, time.UTC))
	sm := security.NewMemorySecretManager()
	sm.PutCredential(security.Credential{Ref: "mock/api_key", Scheme: "API_KEY", Secret: security.NewSecretString("WS_API_SECRET_1234"), ExpiresAtUTC: clk.Now().Add(time.Hour)})
	mockCaps, _ := caps.Get("mock")
	mockAdp := mock.New(mockCaps, sm, "mock/api_key", 7, clk)

	reg := broker.NewRegistry()
	_ = reg.Register(mockAdp)

	gw := gateway.New(gateway.Config{
		DefaultMode:  domain.ModePaper,
		Policy:       policy,
		Capabilities: caps,
		Instruments:  insts,
		Registry:     reg,
		Store:        storage.NewMemoryStore(),
		Logger:       security.NewRedactingLogger(&bytes.Buffer{}, sm),
		Clock:        clk,
	})
	recon := reconciler.New(gw)
	srv := api.NewServer(gw, recon)

	// 1. Health, Capabilities, and Instruments REST endpoints
	for _, ep := range []string{"/health", "/v1/capabilities", "/v1/instruments", "/v1/metrics"} {
		rec := httptest.NewRecorder()
		srv.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, ep, nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("GET %s failed: status=%d body=%s", ep, rec.Code, rec.Body.String())
		}
	}

	// 2. Connect RFC 6455 WebSocket subscriber to /v1/ws over in-memory net.Pipe() (no TCP port required)
	clientConn, serverConn := net.Pipe()
	defer clientConn.Close()
	defer serverConn.Close()

	wsReq := httptest.NewRequest(http.MethodGet, "/v1/ws", nil)
	wsReq.Header.Set("Upgrade", "websocket")
	wsReq.Header.Set("Connection", "Upgrade")
	wsReq.Header.Set("Sec-WebSocket-Key", "dGhlIHNhbXBsZSBub25jZQ==")
	wsReq.Header.Set("Sec-WebSocket-Version", "13")

	doneUpgrade := make(chan struct{})
	go func() {
		srv.ServeHTTP(&hijackRecorder{header: make(http.Header), conn: serverConn}, wsReq)
		close(doneUpgrade)
	}()

	br := bufio.NewReader(clientConn)
	statusLine, err := br.ReadString('\n')
	if err != nil || !strings.Contains(statusLine, "101 Switching Protocols") {
		t.Fatalf("expected 101 Switching Protocols, got %q err=%v", statusLine, err)
	}
	for {
		line, err := br.ReadString('\n')
		if err != nil {
			t.Fatalf("read ws headers: %v", err)
		}
		if line == "\r\n" {
			break
		}
	}
	<-doneUpgrade

	// 3. Submit order via REST POST /v1/orders while reading pushed WebSocket frames concurrently
	frameCh := make(chan []byte, 8)
	go func() {
		for {
			f, err := api.ReadTextFrame(br)
			if err != nil {
				return
			}
			frameCh <- f
		}
	}()

	body, _ := json.Marshal(domain.OrderRequest{
		ClientOrderID: "rest-ws-1",
		BrokerID:      "mock",
		InstrumentID:  "EQ:US:AAPL",
		Side:          domain.SideBuy,
		OrderType:     domain.OrderTypeMarket,
		TimeInForce:   domain.TIFDay,
		Quantity:      3,
		Currency:      "USD",
	})
	postRec := httptest.NewRecorder()
	srv.ServeHTTP(postRec, httptest.NewRequest(http.MethodPost, "/v1/orders", bytes.NewReader(body)))
	if postRec.Code != http.StatusCreated {
		t.Fatalf("POST /v1/orders failed: status=%d body=%s", postRec.Code, postRec.Body.String())
	}
	var created domain.Order
	_ = json.Unmarshal(postRec.Body.Bytes(), &created)
	if created.Status != domain.StatusFilled {
		t.Fatalf("expected FILLED via REST, got %s", created.Status)
	}

	// 4. Verify WebSocket frames (FILL_RECORDED + ORDER_UPDATED) were pushed in real time
	seenTypes := map[domain.GatewayEventType]bool{}
	for i := 0; i < 2; i++ {
		select {
		case frame := <-frameCh:
			var evt domain.GatewayEvent
			if err := json.Unmarshal(frame, &evt); err != nil {
				t.Fatalf("decode ws event: %v", err)
			}
			if evt.OrderID != created.OrderID {
				t.Fatalf("expected ws event for order %s, got %+v", created.OrderID, evt)
			}
			seenTypes[evt.Type] = true
		case <-time.After(2 * time.Second):
			t.Fatalf("timed out waiting for websocket event frame %d", i+1)
		}
	}
	if !seenTypes[domain.EventFillRecorded] || !seenTypes[domain.EventOrderUpdated] {
		t.Fatalf("expected both FILL_RECORDED and ORDER_UPDATED ws frames, got %+v", seenTypes)
	}

	// Close WS client before further broadcasts to avoid unbuffered net.Pipe blocking
	_ = clientConn.Close()

	// 5. GET /v1/orders/{id} returns order + fills
	getRec := httptest.NewRecorder()
	srv.ServeHTTP(getRec, httptest.NewRequest(http.MethodGet, "/v1/orders/"+created.OrderID, nil))
	if getRec.Code != http.StatusOK || !strings.Contains(getRec.Body.String(), created.OrderID) {
		t.Fatalf("GET /v1/orders/{id} failed: %d %s", getRec.Code, getRec.Body.String())
	}

	// 6. Operator halt -> POST /v1/orders returns 503 -> operator resume
	haltRec := httptest.NewRecorder()
	srv.ServeHTTP(haltRec, httptest.NewRequest(http.MethodPost, "/v1/operator/halt", strings.NewReader(`{"reason":"test drill"}`)))
	if haltRec.Code != http.StatusOK {
		t.Fatalf("expected 200 on halt, got %d", haltRec.Code)
	}
	blockedRec := httptest.NewRecorder()
	srv.ServeHTTP(blockedRec, httptest.NewRequest(http.MethodPost, "/v1/orders", strings.NewReader(`{"client_order_id":"rest-ws-2","broker_id":"mock","instrument_id":"EQ:US:AAPL","side":"BUY","order_type":"MARKET","time_in_force":"DAY","quantity":1,"currency":"USD"}`)))
	if blockedRec.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503 while halted, got %d", blockedRec.Code)
	}
	resumeRec := httptest.NewRecorder()
	srv.ServeHTTP(resumeRec, httptest.NewRequest(http.MethodPost, "/v1/operator/resume", strings.NewReader(`{"operator_id":"ops-1","note":"cleared"}`)))
	if resumeRec.Code != http.StatusOK {
		t.Fatalf("expected 200 on resume, got %d", resumeRec.Code)
	}
}

func TestJournalStoreCrashRecoveryAndIdempotencyPersistence(t *testing.T) {
	journalPath := filepath.Join(t.TempDir(), "gateway_journal.jsonl")
	caps, _ := domain.LoadCapabilityMatrix(filepath.Join("..", "..", "config", "capabilities.json"))
	insts, _ := domain.LoadInstrumentMapper(filepath.Join("..", "..", "config", "instruments.json"))
	policy, _ := security.LoadExecutionPolicy(filepath.Join("..", "..", "config", "policy.json"))
	clk := ratelimit.NewManualClock(time.Date(2026, 11, 27, 14, 0, 0, 0, time.UTC))

	mockCaps, _ := caps.Get("mock")
	mockAdp := mock.New(mockCaps, nil, "", 42, clk)
	reg := broker.NewRegistry()
	_ = reg.Register(mockAdp)

	// Phase 1: write order, fill, and replay cursor to JournalStore
	js1, err := storage.OpenJournalStore(journalPath)
	if err != nil {
		t.Fatalf("open journal 1: %v", err)
	}
	gw1 := gateway.New(gateway.Config{
		Policy: policy, Capabilities: caps, Instruments: insts, Registry: reg, Store: js1, Clock: clk,
	})
	req := domain.OrderRequest{
		ClientOrderID: "crash-rec-1", BrokerID: "mock", Mode: domain.ModePaper,
		InstrumentID: "EQ:US:AAPL", Side: domain.SideBuy, OrderType: domain.OrderTypeMarket,
		TimeInForce: domain.TIFDay, Quantity: 5, Currency: "USD",
	}
	ord1, err := gw1.SubmitOrder(context.Background(), req)
	if err != nil {
		t.Fatalf("submit before crash: %v", err)
	}
	_ = js1.SetReplayCursor("mock", 12)

	// Phase 2: simulate restart by opening a fresh JournalStore from the same file
	js2, err := storage.OpenJournalStore(journalPath)
	if err != nil {
		t.Fatalf("open journal 2: %v", err)
	}
	gw2 := gateway.New(gateway.Config{
		Policy: policy, Capabilities: caps, Instruments: insts, Registry: reg, Store: js2, Clock: clk,
	})
	recovered, ok := js2.GetOrderByClientID("crash-rec-1")
	if !ok || recovered.OrderID != ord1.OrderID || recovered.Status != domain.StatusFilled {
		t.Fatalf("recovered order mismatch: ok=%v ord=%+v", ok, recovered)
	}
	if len(js2.ListFillsByOrder(ord1.OrderID)) != 1 {
		t.Fatalf("expected 1 recovered fill")
	}
	if js2.GetReplayCursor("mock") != 12 {
		t.Fatalf("expected replay cursor 12, got %d", js2.GetReplayCursor("mock"))
	}

	// Re-submitting same ClientOrderID after crash MUST be idempotent (0 broker dispatches)
	dispatchesBefore := mockAdp.DispatchCount()
	ord2, err := gw2.SubmitOrder(context.Background(), req)
	if err != nil || ord2.OrderID != ord1.OrderID {
		t.Fatalf("post-crash idempotent submit failed: %+v err=%v", ord2, err)
	}
	if mockAdp.DispatchCount() != dispatchesBefore {
		t.Fatalf("post-crash idempotent replay dispatched to broker!")
	}
}
