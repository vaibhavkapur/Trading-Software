package gateway

import (
	"context"
	"errors"
	"fmt"
	"math"
	"sync"
	"sync/atomic"
	"time"

	"github.com/vaibhavkapur/Trading-Software/projects/17-multi-broker-trading-gateway/src/broker"
	"github.com/vaibhavkapur/Trading-Software/projects/17-multi-broker-trading-gateway/src/domain"
	"github.com/vaibhavkapur/Trading-Software/projects/17-multi-broker-trading-gateway/src/ratelimit"
	"github.com/vaibhavkapur/Trading-Software/projects/17-multi-broker-trading-gateway/src/security"
	"github.com/vaibhavkapur/Trading-Software/projects/17-multi-broker-trading-gateway/src/storage"
)

// EventSink receives real-time gateway events (e.g., WebSocket broadcaster).
type EventSink func(evt domain.GatewayEvent)

// HaltState captures fail-closed circuit breaker status.
type HaltState struct {
	Halted      bool      `json:"halted"`
	Reason      string    `json:"reason,omitempty"`
	HaltedAtUTC time.Time `json:"halted_at_utc,omitempty"`
}

// Config configures a Gateway instance.
type Config struct {
	DefaultMode              domain.ExecutionMode
	MaxUnresolvedBeforeHalt  int
	Policy                   security.ExecutionPolicy
	Capabilities             *domain.CapabilityMatrix
	Instruments              *domain.InstrumentMapper
	Registry                 *broker.Registry
	Store                    storage.Store
	Logger                   *security.RedactingLogger
	Clock                    ratelimit.Clock
}

// Gateway is the fail-closed multi-broker order routing, normalization, and recovery coordinator.
type Gateway struct {
	cfg      Config
	clock    ratelimit.Clock
	limiters map[string]*ratelimit.TokenBucket
	limMu    sync.RWMutex
	haltMu   sync.RWMutex
	halt     HaltState
	sinkMu   sync.RWMutex
	sinks    []EventSink
	orderSeq atomic.Int64
	eventSeq atomic.Int64
	Metrics  Metrics
}

// New constructs a Gateway with safe paper/sandbox defaults.
func New(cfg Config) *Gateway {
	if cfg.DefaultMode == "" {
		cfg.DefaultMode = domain.ModePaper
	}
	if cfg.MaxUnresolvedBeforeHalt <= 0 {
		cfg.MaxUnresolvedBeforeHalt = 1
	}
	if cfg.Clock == nil {
		cfg.Clock = ratelimit.RealClock()
	}
	if cfg.Store == nil {
		cfg.Store = storage.NewMemoryStore()
	}
	if cfg.Registry == nil {
		cfg.Registry = broker.NewRegistry()
	}
	return &Gateway{
		cfg:      cfg,
		clock:    cfg.Clock,
		limiters: make(map[string]*ratelimit.TokenBucket),
	}
}

// SetRateLimiter registers a per-broker client-side rate limiter.
func (g *Gateway) SetRateLimiter(brokerID string, tb *ratelimit.TokenBucket) {
	g.limMu.Lock()
	defer g.limMu.Unlock()
	g.limiters[brokerID] = tb
}

func (g *Gateway) getRateLimiter(brokerID string) *ratelimit.TokenBucket {
	g.limMu.RLock()
	defer g.limMu.RUnlock()
	return g.limiters[brokerID]
}

// Subscribe attaches an event listener for real-time gateway events.
func (g *Gateway) Subscribe(sink EventSink) {
	g.sinkMu.Lock()
	defer g.sinkMu.Unlock()
	g.sinks = append(g.sinks, sink)
}

func (g *Gateway) publish(evtType domain.GatewayEventType, brokerID, orderID string, payload any) {
	id := g.eventSeq.Add(1)
	evt := domain.GatewayEvent{
		EventID:      fmt.Sprintf("gw-evt-%08d", id),
		Type:         evtType,
		BrokerID:     brokerID,
		OrderID:      orderID,
		TimestampUTC: g.clock.Now(),
		Payload:      payload,
	}
	g.sinkMu.RLock()
	sinks := append([]EventSink(nil), g.sinks...)
	g.sinkMu.RUnlock()
	for _, s := range sinks {
		s(evt)
	}
}

// Halt trips the fail-closed execution gate, preventing any further outbound order dispatch.
func (g *Gateway) Halt(reason string) {
	g.haltMu.Lock()
	if !g.halt.Halted {
		g.halt = HaltState{
			Halted:      true,
			Reason:      reason,
			HaltedAtUTC: g.clock.Now(),
		}
		g.Metrics.FailClosedTrips.Add(1)
	}
	state := g.halt
	g.haltMu.Unlock()

	if g.cfg.Logger != nil {
		g.cfg.Logger.Error("gateway", "fail-closed halt triggered", map[string]any{
			"reason": reason,
		})
	}
	g.publish(domain.EventGatewayHalted, "", "", state)
}

// Resume lifts a fail-closed halt after explicit operator acknowledgment and remediation.
func (g *Gateway) Resume(operatorID, note string) error {
	if operatorID == "" {
		return domain.NewError(domain.ErrCodeValidation, "operator_id is required to resume halted gateway", false)
	}
	g.haltMu.Lock()
	g.halt = HaltState{Halted: false}
	g.haltMu.Unlock()

	if g.cfg.Logger != nil {
		g.cfg.Logger.Info("gateway", "gateway resumed by operator", map[string]any{
			"operator_id": operatorID,
			"note":        note,
		})
	}
	g.publish(domain.EventGatewayResumed, "", "", map[string]string{
		"operator_id": operatorID,
		"note":        note,
	})
	return nil
}

// HaltStatus returns current fail-closed status.
func (g *Gateway) HaltStatus() HaltState {
	g.haltMu.RLock()
	defer g.haltMu.RUnlock()
	return g.halt
}

// Store exposes the gateway's persistence store for read-only inspection and reconciliation.
func (g *Gateway) Store() storage.Store { return g.cfg.Store }

// Registry exposes the adapter registry.
func (g *Gateway) Registry() *broker.Registry { return g.cfg.Registry }

// Capabilities exposes the versioned capability matrix.
func (g *Gateway) Capabilities() *domain.CapabilityMatrix { return g.cfg.Capabilities }

// Instruments exposes the versioned instrument mapper.
func (g *Gateway) Instruments() *domain.InstrumentMapper { return g.cfg.Instruments }

// Policy exposes the active execution policy.
func (g *Gateway) Policy() security.ExecutionPolicy { return g.cfg.Policy }

// Logger exposes the redacting logger.
func (g *Gateway) Logger() *security.RedactingLogger { return g.cfg.Logger }

// Clock exposes the gateway clock.
func (g *Gateway) Clock() ratelimit.Clock { return g.clock }

// MaxUnresolvedBeforeHalt returns the reconciliation drift halt threshold.
func (g *Gateway) MaxUnresolvedBeforeHalt() int { return g.cfg.MaxUnresolvedBeforeHalt }

// SubmitOrder validates policy, capabilities, symbol mapping, rate limits, and idempotency before dispatching.
// Unsupported order types, symbol mismatches, and policy denials are rejected strictly BEFORE broker dispatch.
// Unknown submit outcomes transition to SUBMIT_UNKNOWN and are NEVER blindly retried.
func (g *Gateway) SubmitOrder(ctx context.Context, req domain.OrderRequest) (domain.Order, error) {
	if req.Mode == "" {
		req.Mode = g.cfg.DefaultMode
	}
	if req.SubmittedAt.IsZero() {
		req.SubmittedAt = g.clock.Now()
	}

	// 1. Structural validation
	if err := req.ValidateBasic(); err != nil {
		g.Metrics.OrdersRejectedPreDispatch.Add(1)
		return domain.Order{}, err
	}

	// 2. Idempotency check BEFORE halt/dispatch so safe duplicate replays return existing order without double-execution.
	reqHash := req.PayloadHash()
	if existing, ok := g.cfg.Store.GetOrderByClientID(req.ClientOrderID); ok {
		if existing.PayloadHash != reqHash {
			g.Metrics.OrdersRejectedPreDispatch.Add(1)
			return domain.Order{}, domain.NewError(
				domain.ErrCodeIdempotencyConflict,
				fmt.Sprintf("client_order_id %q already used with a different payload", req.ClientOrderID),
				false,
			).WithContext(req.BrokerID, existing.OrderID)
		}
		g.Metrics.IdempotentHits.Add(1)
		if g.cfg.Logger != nil {
			g.cfg.Logger.Info("gateway", "idempotent submit replay returned existing order", map[string]any{
				"client_order_id": req.ClientOrderID,
				"order_id":        existing.OrderID,
				"status":          existing.Status,
			})
		}
		if existing.Status == domain.StatusSubmitUnknown {
			// Never blindly retry an order with unknown outcome!
			return existing, domain.NewError(
				domain.ErrCodeUnknownOutcome,
				"order has unknown submit outcome; run reconciliation instead of retrying",
				false,
			).WithContext(existing.BrokerID, existing.OrderID)
		}
		return existing, nil
	}

	// 3. Fail-closed gate
	if hs := g.HaltStatus(); hs.Halted {
		g.Metrics.OrdersRejectedPreDispatch.Add(1)
		return domain.Order{}, domain.NewError(
			domain.ErrCodeGatewayHalted,
			fmt.Sprintf("gateway is halted (fail-closed): %s", hs.Reason),
			false,
		).WithContext(req.BrokerID, "")
	}

	// 4. Execution policy authorization (paper/sandbox default; live denied)
	if err := g.cfg.Policy.AuthorizeOrder(req); err != nil {
		g.Metrics.OrdersRejectedPreDispatch.Add(1)
		if g.cfg.Logger != nil {
			g.cfg.Logger.Warn("policy", "order rejected by execution policy", map[string]any{
				"client_order_id": req.ClientOrderID,
				"broker_id":       req.BrokerID,
				"mode":            req.Mode,
				"error":           err.Error(),
			})
		}
		return domain.Order{}, err
	}

	// 5. Adapter lookup
	adapter, err := g.cfg.Registry.Get(req.BrokerID)
	if err != nil {
		g.Metrics.OrdersRejectedPreDispatch.Add(1)
		return domain.Order{}, err
	}

	// 6. Canonical instrument mapping & validation BEFORE dispatch
	instSpec, brokerSymbol, err := g.cfg.Instruments.ResolveToBroker(req.BrokerID, req)
	if err != nil {
		g.Metrics.OrdersRejectedPreDispatch.Add(1)
		if g.cfg.Logger != nil {
			g.cfg.Logger.Warn("mapping", "order rejected due to instrument/symbol validation", map[string]any{
				"client_order_id": req.ClientOrderID,
				"broker_id":       req.BrokerID,
				"instrument_id":   req.InstrumentID,
				"error":           err.Error(),
			})
		}
		return domain.Order{}, err
	}

	// 7. Broker capability validation BEFORE dispatch
	if err := g.cfg.Capabilities.ValidateOrder(req, instSpec); err != nil {
		g.Metrics.OrdersRejectedPreDispatch.Add(1)
		if g.cfg.Logger != nil {
			g.cfg.Logger.Warn("capabilities", "order rejected due to unsupported broker capability", map[string]any{
				"client_order_id": req.ClientOrderID,
				"broker_id":       req.BrokerID,
				"order_type":      req.OrderType,
				"time_in_force":   req.TimeInForce,
				"error":           err.Error(),
			})
		}
		return domain.Order{}, err
	}

	// 8. Client-side per-broker rate limit check BEFORE dispatch
	if lim := g.getRateLimiter(req.BrokerID); lim != nil {
		if err := lim.Allow(); err != nil {
			g.Metrics.RateLimitRejections.Add(1)
			if g.cfg.Logger != nil {
				g.cfg.Logger.Warn("ratelimit", "order rejected by client rate limiter", map[string]any{
					"client_order_id": req.ClientOrderID,
					"broker_id":       req.BrokerID,
					"error":           err.Error(),
				})
			}
			return domain.Order{}, err
		}
	}

	// 9. Write-ahead order creation in PENDING_SUBMIT
	now := g.clock.Now()
	oid := fmt.Sprintf("gw-ord-%06d", g.orderSeq.Add(1))
	order := domain.Order{
		OrderID:       oid,
		ClientOrderID: req.ClientOrderID,
		BrokerID:      req.BrokerID,
		Mode:          req.Mode,
		InstrumentID:  req.InstrumentID,
		BrokerSymbol:  brokerSymbol,
		Side:          req.Side,
		OrderType:     req.OrderType,
		TimeInForce:   req.TimeInForce,
		Quantity:      req.Quantity,
		RemainingQty:  req.Quantity,
		LimitPrice:    req.LimitPrice,
		StopPrice:     req.StopPrice,
		Currency:      req.Currency,
		Status:        domain.StatusPendingSubmit,
		PayloadHash:   reqHash,
		CreatedAtUTC:  now,
		UpdatedAtUTC:  now,
	}
	if err := g.cfg.Store.SaveOrder(order); err != nil {
		return domain.Order{}, domain.NewError(domain.ErrCodeInternal, "failed to persist write-ahead order", false).WithCause(err)
	}
	g.Metrics.OrdersSubmitted.Add(1)

	// 10. Dispatch to broker adapter
	ack, err := adapter.SubmitOrder(ctx, broker.BrokerOrderSubmit{
		OrderID:       order.OrderID,
		ClientOrderID: order.ClientOrderID,
		Mode:          order.Mode,
		BrokerSymbol:  order.BrokerSymbol,
		Side:          order.Side,
		OrderType:     order.OrderType,
		TimeInForce:   order.TimeInForce,
		Quantity:      order.Quantity,
		LimitPrice:    order.LimitPrice,
		StopPrice:     order.StopPrice,
		Currency:      order.Currency,
	})
	if err != nil {
		return g.handleSubmitError(order, err)
	}

	// 11. Normalize ack & any synchronous fills
	// Re-fetch in case an asynchronous webhook/fill arrived concurrently.
	if latest, ok := g.cfg.Store.GetOrder(order.OrderID); ok {
		order = latest
	}
	order.BrokerOrderID = ack.BrokerOrderID
	order.RejectReason = ack.RejectReason
	order.UpdatedAtUTC = g.clock.Now()

	if ack.Status.Rank() > order.Status.Rank() {
		order.Status = ack.Status
	}

	for _, bf := range ack.Fills {
		_ = g.recordBrokerFillLocked(&order, bf)
	}
	fills := g.cfg.Store.ListFillsByOrder(order.OrderID)
	if len(fills) > 0 {
		domain.RecomputeOrderFromFills(&order, fills, g.clock.Now())
	}

	if err := g.cfg.Store.SaveOrder(order); err != nil {
		return order, domain.NewError(domain.ErrCodeInternal, "failed to save acknowledged order", false).WithCause(err)
	}

	switch order.Status {
	case domain.StatusAccepted, domain.StatusPendingNew:
		g.Metrics.OrdersAccepted.Add(1)
	case domain.StatusFilled:
		g.Metrics.OrdersFilled.Add(1)
	case domain.StatusRejected:
		g.Metrics.OrdersRejectedByBroker.Add(1)
	}

	if g.cfg.Logger != nil {
		g.cfg.Logger.Info("gateway", "order dispatched and normalized", map[string]any{
			"order_id":        order.OrderID,
			"client_order_id": order.ClientOrderID,
			"broker_id":       order.BrokerID,
			"broker_order_id": order.BrokerOrderID,
			"status":          order.Status,
			"filled_qty":      order.FilledQuantity,
		})
	}
	g.publish(domain.EventOrderUpdated, order.BrokerID, order.OrderID, order)
	return order, nil
}

func (g *Gateway) handleSubmitError(order domain.Order, err error) (domain.Order, error) {
	code := domain.ErrorCodeOf(err)
	var ge *domain.GatewayError
	_ = errors.As(err, &ge)

	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		code = domain.ErrCodeUnknownOutcome
		err = domain.NewError(domain.ErrCodeUnknownOutcome, "submit context expired; outcome uncertain", false).
			WithContext(order.BrokerID, order.OrderID).WithCause(err)
	}

	order.LastErrorCode = code
	order.UpdatedAtUTC = g.clock.Now()

	switch code {
	case domain.ErrCodeUnknownOutcome:
		// CRITICAL INVARIANT: Unknown submit result -> mark SUBMIT_UNKNOWN, NEVER blindly retry!
		order.Status = domain.StatusSubmitUnknown
		g.Metrics.UnknownOutcomes.Add(1)
	case domain.ErrCodeRateLimited:
		order.Status = domain.StatusRejected
		order.RejectReason = err.Error()
		g.Metrics.RateLimitRejections.Add(1)
		if ge != nil && ge.RetryAfter > 0 {
			if lim := g.getRateLimiter(order.BrokerID); lim != nil {
				lim.ApplyRetryAfter(ge.RetryAfter)
			}
		}
	case domain.ErrCodeAuthExpired, domain.ErrCodeAuthInvalid:
		order.Status = domain.StatusRejected
		order.RejectReason = err.Error()
		g.Metrics.AuthFailures.Add(1)
		if g.cfg.Policy.FailClosedOnAuthError {
			g.Halt(fmt.Sprintf("broker %s authentication failure: %s", order.BrokerID, code))
		}
	default:
		order.Status = domain.StatusRejected
		order.RejectReason = err.Error()
		g.Metrics.OrdersRejectedByBroker.Add(1)
	}

	_ = g.cfg.Store.SaveOrder(order)
	if g.cfg.Logger != nil {
		g.cfg.Logger.Warn("gateway", "order dispatch returned error", map[string]any{
			"order_id":        order.OrderID,
			"client_order_id": order.ClientOrderID,
			"broker_id":       order.BrokerID,
			"error_code":      code,
			"status":          order.Status,
			"error":           err.Error(),
		})
	}
	g.publish(domain.EventOrderUpdated, order.BrokerID, order.OrderID, order)
	return order, err
}

// CancelOrder dispatches a cancel to the owning broker adapter and handles cancel timeouts safely.
func (g *Gateway) CancelOrder(ctx context.Context, orderID string) (domain.Order, error) {
	order, ok := g.cfg.Store.GetOrder(orderID)
	if !ok {
		order, ok = g.cfg.Store.GetOrderByClientID(orderID)
	}
	if !ok {
		return domain.Order{}, domain.NewError(domain.ErrCodeOrderNotFound, fmt.Sprintf("order %q not found", orderID), false)
	}
	if order.Status.IsTerminal() {
		return order, nil
	}

	adapter, err := g.cfg.Registry.Get(order.BrokerID)
	if err != nil {
		return order, err
	}

	if lim := g.getRateLimiter(order.BrokerID); lim != nil {
		if err := lim.Allow(); err != nil {
			g.Metrics.RateLimitRejections.Add(1)
			return order, err
		}
	}

	ack, err := adapter.CancelOrder(ctx, order.BrokerOrderID, order.ClientOrderID)
	if err != nil {
		code := domain.ErrorCodeOf(err)
		if errors.Is(err, context.DeadlineExceeded) {
			code = domain.ErrCodeCancelTimeout
			err = domain.NewError(domain.ErrCodeCancelTimeout, "cancel context deadline exceeded", false).
				WithContext(order.BrokerID, order.OrderID).WithCause(err)
		}
		if code == domain.ErrCodeCancelTimeout {
			order.Status = domain.StatusCancelPending
			order.LastErrorCode = domain.ErrCodeCancelTimeout
			order.UpdatedAtUTC = g.clock.Now()
			_ = g.cfg.Store.SaveOrder(order)
			g.Metrics.CancelTimeouts.Add(1)
			if g.cfg.Logger != nil {
				g.cfg.Logger.Warn("gateway", "cancel timed out; marked CANCEL_PENDING for reconciliation", map[string]any{
					"order_id":        order.OrderID,
					"broker_id":       order.BrokerID,
					"broker_order_id": order.BrokerOrderID,
				})
			}
			g.publish(domain.EventOrderUpdated, order.BrokerID, order.OrderID, order)
			return order, err
		}
		return order, err
	}

	if ack.Status.Rank() >= order.Status.Rank() {
		order.Status = ack.Status
	}
	order.UpdatedAtUTC = g.clock.Now()
	_ = g.cfg.Store.SaveOrder(order)
	if order.Status == domain.StatusCancelled {
		g.Metrics.OrdersCancelled.Add(1)
	}
	g.publish(domain.EventOrderUpdated, order.BrokerID, order.OrderID, order)
	return order, nil
}

// IngestWebhook processes broker webhook/push events idempotently, handling duplicates and out-of-order fills.
func (g *Gateway) IngestWebhook(evt domain.WebhookEvent) (domain.Order, bool, error) {
	firstSeen, err := g.cfg.Store.MarkWebhookProcessed(evt.BrokerID, evt.EventID)
	if err != nil {
		return domain.Order{}, false, err
	}
	if !firstSeen {
		g.Metrics.DuplicateWebhooksIgnored.Add(1)
		if g.cfg.Logger != nil {
			g.cfg.Logger.Info("webhook", "duplicate webhook ignored", map[string]any{
				"broker_id": evt.BrokerID,
				"event_id":  evt.EventID,
			})
		}
		g.publish(domain.EventWebhookDeduplicated, evt.BrokerID, "", evt)
		if ord, ok := g.findOrderForWebhook(evt); ok {
			return ord, false, nil
		}
		return domain.Order{}, false, nil
	}

	order, ok := g.findOrderForWebhook(evt)
	if !ok {
		return domain.Order{}, false, domain.NewError(
			domain.ErrCodeOrderNotFound,
			fmt.Sprintf("webhook %q references unknown order (client=%q broker=%q)", evt.EventID, evt.ClientOrderID, evt.BrokerOrderID),
			false,
		).WithContext(evt.BrokerID, evt.BrokerOrderID)
	}

	if order.BrokerOrderID == "" && evt.BrokerOrderID != "" {
		order.BrokerOrderID = evt.BrokerOrderID
	}

	switch evt.EventType {
	case "FILL":
		existingFills := g.cfg.Store.ListFillsByOrder(order.OrderID)
		// Detect out-of-order arrival: sequence smaller than highest already recorded, OR fill arrived before ACK.
		var maxSeq int64
		for _, ef := range existingFills {
			if ef.Sequence > maxSeq {
				maxSeq = ef.Sequence
			}
		}
		if (maxSeq > 0 && evt.Sequence > 0 && evt.Sequence < maxSeq) ||
			order.Status == domain.StatusPendingSubmit ||
			order.Status == domain.StatusSubmitUnknown ||
			order.Status == domain.StatusPendingNew {
			g.Metrics.OutOfOrderFillsHandled.Add(1)
		}

		currency := evt.Currency
		if currency == "" {
			currency = order.Currency
		}
		fill := domain.Fill{
			FillID:        fmt.Sprintf("fill-%s-%s", evt.BrokerID, evt.BrokerFillID),
			EventID:       evt.EventID,
			OrderID:       order.OrderID,
			ClientOrderID: order.ClientOrderID,
			BrokerID:      evt.BrokerID,
			BrokerOrderID: order.BrokerOrderID,
			BrokerFillID:  evt.BrokerFillID,
			InstrumentID:  order.InstrumentID,
			BrokerSymbol:  order.BrokerSymbol,
			Side:          order.Side,
			Sequence:      evt.Sequence,
			Quantity:      evt.FillQty,
			Price:         evt.FillPrice,
			Fee:           evt.Fee,
			Currency:      currency,
			ExecutedAtUTC: evt.EventTimeUTC.UTC(),
			ReceivedAtUTC: g.clock.Now(),
		}
		inserted, err := g.cfg.Store.RecordFill(fill)
		if err != nil {
			return order, false, err
		}
		if inserted {
			g.Metrics.FillsRecorded.Add(1)
			allFills := g.cfg.Store.ListFillsByOrder(order.OrderID)
			prevStatus := order.Status
			domain.RecomputeOrderFromFills(&order, allFills, g.clock.Now())
			if order.Status == domain.StatusFilled && prevStatus != domain.StatusFilled {
				g.Metrics.OrdersFilled.Add(1)
			}
			_ = g.cfg.Store.SaveOrder(order)
			g.publish(domain.EventFillRecorded, order.BrokerID, order.OrderID, fill)
			g.publish(domain.EventOrderUpdated, order.BrokerID, order.OrderID, order)
			return order, true, nil
		}
		// Duplicate fill ID inside a new webhook event ID: still deduplicated!
		g.Metrics.DuplicateWebhooksIgnored.Add(1)
		return order, false, nil

	case "STATUS", "CANCELLED", "REJECTED":
		target := evt.Status
		if target == "" {
			switch evt.EventType {
			case "CANCELLED":
				target = domain.StatusCancelled
			case "REJECTED":
				target = domain.StatusRejected
			}
		}
		// Monotonic status protection: never regress terminal or further-progressed states due to out-of-order status events.
		if !order.Status.IsTerminal() && target.Rank() > order.Status.Rank() {
			order.Status = target
			if evt.Reason != "" {
				order.RejectReason = evt.Reason
			}
			order.UpdatedAtUTC = g.clock.Now()
			_ = g.cfg.Store.SaveOrder(order)
			if order.Status == domain.StatusCancelled {
				g.Metrics.OrdersCancelled.Add(1)
			}
			g.publish(domain.EventOrderUpdated, order.BrokerID, order.OrderID, order)
			return order, true, nil
		}
		return order, false, nil

	default:
		return order, false, domain.NewError(domain.ErrCodeValidation, fmt.Sprintf("unsupported webhook event_type %q", evt.EventType), false)
	}
}

// ReplayBrokerEvents replays cursor-based events from adapters that support event replay (e.g., sandbox-beta).
func (g *Gateway) ReplayBrokerEvents(ctx context.Context, brokerID string) (int, int64, error) {
	adapter, err := g.cfg.Registry.Get(brokerID)
	if err != nil {
		return 0, 0, err
	}
	cursor := g.cfg.Store.GetReplayCursor(brokerID)
	events, nextCursor, err := adapter.ReplayEvents(ctx, cursor)
	if err != nil {
		return 0, cursor, err
	}
	applied := 0
	for _, ev := range events {
		_, ok, err := g.IngestWebhook(ev)
		if err != nil {
			continue
		}
		if ok {
			applied++
		}
	}
	if nextCursor > cursor {
		_ = g.cfg.Store.SetReplayCursor(brokerID, nextCursor)
	}
	return applied, nextCursor, nil
}

func (g *Gateway) findOrderForWebhook(evt domain.WebhookEvent) (domain.Order, bool) {
	if evt.ClientOrderID != "" {
		if ord, ok := g.cfg.Store.GetOrderByClientID(evt.ClientOrderID); ok {
			return ord, true
		}
	}
	if evt.BrokerOrderID != "" {
		if ord, ok := g.cfg.Store.GetOrderByBrokerOrderID(evt.BrokerID, evt.BrokerOrderID); ok {
			return ord, true
		}
	}
	return domain.Order{}, false
}

func (g *Gateway) recordBrokerFillLocked(order *domain.Order, bf broker.BrokerFillRecord) bool {
	if bf.EventID != "" {
		_, _ = g.cfg.Store.MarkWebhookProcessed(order.BrokerID, bf.EventID)
	}
	fill := domain.Fill{
		FillID:        fmt.Sprintf("fill-%s-%s", order.BrokerID, bf.BrokerFillID),
		EventID:       bf.EventID,
		OrderID:       order.OrderID,
		ClientOrderID: order.ClientOrderID,
		BrokerID:      order.BrokerID,
		BrokerOrderID: bf.BrokerOrderID,
		BrokerFillID:  bf.BrokerFillID,
		InstrumentID:  order.InstrumentID,
		BrokerSymbol:  order.BrokerSymbol,
		Side:          order.Side,
		Sequence:      bf.Sequence,
		Quantity:      math.Round(bf.Quantity*1e8) / 1e8,
		Price:         bf.Price,
		Fee:           bf.Fee,
		Currency:      bf.Currency,
		ExecutedAtUTC: bf.ExecutedAtUTC.UTC(),
		ReceivedAtUTC: g.clock.Now(),
	}
	inserted, err := g.cfg.Store.RecordFill(fill)
	if err == nil && inserted {
		g.Metrics.FillsRecorded.Add(1)
		g.publish(domain.EventFillRecorded, order.BrokerID, order.OrderID, fill)
		return true
	}
	return false
}

// ApplyReconciledFills is used by the independent reconciler to synchronize missing broker fills.
func (g *Gateway) ApplyReconciledFills(order *domain.Order, fills []broker.BrokerFillRecord) int {
	added := 0
	for _, bf := range fills {
		if g.recordBrokerFillLocked(order, bf) {
			added++
		}
	}
	all := g.cfg.Store.ListFillsByOrder(order.OrderID)
	if len(all) > 0 {
		domain.RecomputeOrderFromFills(order, all, g.clock.Now())
	}
	return added
}

// PublishEvent exposes event publishing for the independent reconciler.
func (g *Gateway) PublishEvent(evtType domain.GatewayEventType, brokerID, orderID string, payload any) {
	g.publish(evtType, brokerID, orderID, payload)
}
