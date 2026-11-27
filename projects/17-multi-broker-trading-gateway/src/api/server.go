package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"github.com/vaibhavkapur/Trading-Software/projects/17-multi-broker-trading-gateway/src/domain"
	"github.com/vaibhavkapur/Trading-Software/projects/17-multi-broker-trading-gateway/src/gateway"
	"github.com/vaibhavkapur/Trading-Software/projects/17-multi-broker-trading-gateway/src/reconciler"
)

// Server exposes the Multi-Broker Trading Gateway over REST JSON and RFC 6455 WebSocket.
type Server struct {
	gw    *gateway.Gateway
	recon *reconciler.Reconciler
	hub   *WSHub
	mux   *http.ServeMux
}

// NewServer constructs an HTTP/WebSocket API server wired to gw and recon.
func NewServer(gw *gateway.Gateway, recon *reconciler.Reconciler) *Server {
	hub := NewWSHub()
	gw.Subscribe(hub.Broadcast)

	s := &Server{
		gw:    gw,
		recon: recon,
		hub:   hub,
		mux:   http.NewServeMux(),
	}
	s.routes()
	return s
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.mux.ServeHTTP(w, r)
}

func (s *Server) routes() {
	s.mux.HandleFunc("/health", s.handleHealth)
	s.mux.HandleFunc("/v1/capabilities", s.handleCapabilities)
	s.mux.HandleFunc("/v1/instruments", s.handleInstruments)
	s.mux.HandleFunc("/v1/metrics", s.handleMetrics)
	s.mux.HandleFunc("/v1/orders", s.handleOrders)
	s.mux.HandleFunc("/v1/orders/", s.handleOrderByID)
	s.mux.HandleFunc("/v1/webhooks", s.handleWebhook)
	s.mux.HandleFunc("/v1/reconcile", s.handleReconcile)
	s.mux.HandleFunc("/v1/operator/halt", s.handleHalt)
	s.mux.HandleFunc("/v1/operator/resume", s.handleResume)
	s.mux.HandleFunc("/v1/ws", s.hub.HandleUpgrade)
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
		return
	}
	brokersHealth := make(map[string]string)
	for _, a := range s.gw.Registry().All() {
		if err := a.HealthCheck(r.Context()); err != nil {
			brokersHealth[a.BrokerID()] = "unhealthy: " + string(domain.ErrorCodeOf(err))
		} else {
			brokersHealth[a.BrokerID()] = "ok"
		}
	}
	halt := s.gw.HaltStatus()
	status := "ok"
	code := http.StatusOK
	if halt.Halted {
		status = "halted"
		code = http.StatusServiceUnavailable
	}
	writeJSON(w, code, map[string]any{
		"status":         status,
		"default_mode":   domain.ModePaper,
		"live_enabled":   !s.gw.Policy().DenyLiveExecution,
		"halt_state":     halt,
		"broker_health":  brokersHealth,
		"timestamp_utc":  s.gw.Clock().Now(),
	})
}

func (s *Server) handleCapabilities(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"schema_version": s.gw.Capabilities().SchemaVersion,
		"brokers":        s.gw.Capabilities().All(),
	})
}

func (s *Server) handleInstruments(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"schema_version": s.gw.Instruments().SchemaVersion,
		"instruments":    s.gw.Instruments().All(),
	})
}

func (s *Server) handleMetrics(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, s.gw.Metrics.Snapshot())
}

func (s *Server) handleOrders(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		brokerID := r.URL.Query().Get("broker_id")
		writeJSON(w, http.StatusOK, map[string]any{
			"orders": s.gw.Store().ListOrders(brokerID),
		})
	case http.MethodPost:
		var req domain.OrderRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeGatewayError(w, domain.NewError(domain.ErrCodeValidation, "invalid JSON order body", false))
			return
		}
		ord, err := s.gw.SubmitOrder(r.Context(), req)
		if err != nil {
			writeGatewayErrorWithOrder(w, err, ord)
			return
		}
		writeJSON(w, http.StatusCreated, ord)
	default:
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
	}
}

func (s *Server) handleOrderByID(w http.ResponseWriter, r *http.Request) {
	trimmed := strings.TrimPrefix(r.URL.Path, "/v1/orders/")
	parts := strings.Split(strings.Trim(trimmed, "/"), "/")
	if len(parts) == 0 || parts[0] == "" {
		writeGatewayError(w, domain.NewError(domain.ErrCodeValidation, "missing order id", false))
		return
	}
	orderID := parts[0]

	if len(parts) == 2 && parts[1] == "cancel" && r.Method == http.MethodPost {
		ord, err := s.gw.CancelOrder(r.Context(), orderID)
		if err != nil {
			writeGatewayErrorWithOrder(w, err, ord)
			return
		}
		writeJSON(w, http.StatusOK, ord)
		return
	}

	if len(parts) == 1 && r.Method == http.MethodGet {
		ord, ok := s.gw.Store().GetOrder(orderID)
		if !ok {
			ord, ok = s.gw.Store().GetOrderByClientID(orderID)
		}
		if !ok {
			writeGatewayError(w, domain.NewError(domain.ErrCodeOrderNotFound, "order not found", false))
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"order": ord,
			"fills": s.gw.Store().ListFillsByOrder(ord.OrderID),
		})
		return
	}

	writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "unsupported route or method"})
}

func (s *Server) handleWebhook(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
		return
	}
	var evt domain.WebhookEvent
	if err := json.NewDecoder(r.Body).Decode(&evt); err != nil {
		writeGatewayError(w, domain.NewError(domain.ErrCodeValidation, "invalid webhook JSON", false))
		return
	}
	ord, applied, err := s.gw.IngestWebhook(evt)
	if err != nil {
		writeGatewayError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"applied": applied,
		"order":   ord,
	})
}

func (s *Server) handleReconcile(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodGet {
		writeJSON(w, http.StatusOK, map[string]any{
			"reports": s.gw.Store().ListReconciliationReports(),
		})
		return
	}
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
		return
	}
	brokerID := r.URL.Query().Get("broker_id")
	var (
		rep domain.ReconciliationReport
		err error
	)
	if brokerID != "" {
		rep, err = s.recon.ReconcileBroker(r.Context(), brokerID)
	} else {
		rep, err = s.recon.ReconcileAll(r.Context())
	}
	if err != nil {
		writeGatewayError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, rep)
}

func (s *Server) handleHalt(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
		return
	}
	var body struct {
		Reason string `json:"reason"`
	}
	_ = json.NewDecoder(r.Body).Decode(&body)
	if body.Reason == "" {
		body.Reason = "manual operator halt"
	}
	s.gw.Halt(body.Reason)
	writeJSON(w, http.StatusOK, s.gw.HaltStatus())
}

func (s *Server) handleResume(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
		return
	}
	var body struct {
		OperatorID string `json:"operator_id"`
		Note       string `json:"note"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeGatewayError(w, domain.NewError(domain.ErrCodeValidation, "invalid JSON", false))
		return
	}
	if err := s.gw.Resume(body.OperatorID, body.Note); err != nil {
		writeGatewayError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, s.gw.HaltStatus())
}

func httpStatusForError(code domain.ErrorCode) int {
	switch code {
	case domain.ErrCodeValidation, domain.ErrCodeUnsupportedCapability, domain.ErrCodeSymbolMismatch:
		return http.StatusBadRequest
	case domain.ErrCodePolicyDenied:
		return http.StatusForbidden
	case domain.ErrCodeGatewayHalted:
		return http.StatusServiceUnavailable
	case domain.ErrCodeAuthExpired, domain.ErrCodeAuthInvalid:
		return http.StatusUnauthorized
	case domain.ErrCodeRateLimited:
		return http.StatusTooManyRequests
	case domain.ErrCodeIdempotencyConflict:
		return http.StatusConflict
	case domain.ErrCodeOrderNotFound, domain.ErrCodeBrokerNotFound:
		return http.StatusNotFound
	case domain.ErrCodeUnknownOutcome, domain.ErrCodeCancelTimeout:
		return http.StatusGatewayTimeout
	default:
		return http.StatusInternalServerError
	}
}

func writeGatewayError(w http.ResponseWriter, err error) {
	writeGatewayErrorWithOrder(w, err, domain.Order{})
}

func writeGatewayErrorWithOrder(w http.ResponseWriter, err error, ord domain.Order) {
	var ge *domain.GatewayError
	if !errors.As(err, &ge) {
		ge = domain.NewError(domain.ErrCodeInternal, err.Error(), false)
	}
	resp := map[string]any{
		"error": ge,
	}
	if ord.OrderID != "" {
		resp["order"] = ord
	}
	writeJSON(w, httpStatusForError(ge.Code), resp)
}

func writeJSON(w http.ResponseWriter, status int, payload any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(payload)
}
