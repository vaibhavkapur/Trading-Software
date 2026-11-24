// Package service implements the OMS use cases: the client commands (create,
// cancel, replace), the ingestion of broker reports, and operator repairs.
// Each use case is one database transaction that locks the order, asks the
// domain package what should happen, and persists the resulting events with
// their projections and outbox messages.
package service

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"

	"oms/src/auth"
	"oms/src/broker"
	"oms/src/domain"
	"oms/src/fixed"
	"oms/src/ids"
	"oms/src/metrics"
	"oms/src/risk"
	"oms/src/store"
)

// Service holds the dependencies of every use case.
type Service struct {
	Store   *store.Store
	Risk    risk.Authorizer
	IDs     ids.Generator
	Metrics *metrics.Set
	Log     *slog.Logger
	// Broker is used only by the operator repair that must confirm an order
	// never reached the broker. It may be nil, which disables that repair.
	Broker broker.Client
	// Nudge, if set, is called after a transaction queued a broker call, so a
	// dispatcher in the same process can run without waiting for its poll.
	Nudge func()
}

// Error is a use-case failure with the HTTP status and stable code that the
// API reports.
type Error struct {
	Status  int
	Code    string
	Message string
	Fields  []domain.FieldError
	OrderID string
	Codes   []string
}

func (e *Error) Error() string { return e.Code + ": " + e.Message }

func errorf(status int, code, format string, args ...any) *Error {
	return &Error{Status: status, Code: code, Message: fmt.Sprintf(format, args...)}
}

var errOrderNotFound = &Error{Status: http.StatusNotFound, Code: "ORDER_NOT_FOUND", Message: "no such order"}

// Result is the outcome of a command.
type Result struct {
	// Status is the HTTP status to report.
	Status int
	// Body is the JSON order view.
	Body []byte
	// Replayed is true when the response came from a stored idempotency
	// record and nothing was executed.
	Replayed bool
}

// OrderView is an order as the API presents it.
type OrderView struct {
	domain.Order
	LeavesQty    int64         `json:"leaves_qty"`
	AvgFillPrice *fixed.Micros `json:"avg_fill_price,omitempty"`
}

// View adds the derived fields to an order.
func View(o domain.Order) OrderView {
	v := OrderView{Order: o, LeavesQty: o.LeavesQty()}
	if o.FilledQty > 0 {
		// Display only: rounded half up to the micro.
		avg := fixed.Micros((int64(o.FilledNotional) + o.FilledQty/2) / o.FilledQty)
		v.AvgFillPrice = &avg
	}
	return v
}

// CreateOrderRequest is the body of POST /v1/orders.
type CreateOrderRequest struct {
	AccountID     string             `json:"account_id"`
	ClientOrderID string             `json:"client_order_id,omitempty"`
	Symbol        string             `json:"symbol"`
	Side          domain.Side        `json:"side"`
	Type          domain.OrderType   `json:"type"`
	TimeInForce   domain.TimeInForce `json:"time_in_force,omitempty"`
	Qty           int64              `json:"qty"`
	LimitPrice    *fixed.Micros      `json:"limit_price,omitempty"`
}

// ReplaceOrderRequest is the body of POST /v1/orders/{id}/replace. An omitted
// field keeps its current value.
type ReplaceOrderRequest struct {
	Qty        *int64        `json:"qty,omitempty"`
	LimitPrice *fixed.Micros `json:"limit_price,omitempty"`
}

const (
	opCreate  = "create"
	opReplace = "replace"
)

// CreateOrder accepts a new order.
//
// The idempotency key makes the call safe to retry: the same key with the
// same body returns the first response without creating anything; the same
// key with a different body is refused. The order, its events, the queued
// broker submission and the idempotency record commit in one transaction, so
// there is no state in which an order exists but will not be sent, or will
// be sent but does not exist.
func (s *Service) CreateOrder(ctx context.Context, p auth.Principal, key string, req CreateOrderRequest) (Result, error) {
	if err := checkKey(key); err != nil {
		return Result{}, err
	}
	if req.TimeInForce == "" {
		req.TimeInForce = domain.Day
	}
	hash := hashRequest(opCreate, req)

	orderID := s.IDs.New("ord")
	created := domain.OrderCreated{
		AccountID: req.AccountID, ClientOrderID: req.ClientOrderID, Symbol: req.Symbol,
		Side: req.Side, Type: req.Type, TimeInForce: req.TimeInForce,
		Qty: req.Qty, LimitPrice: req.LimitPrice, CreatedBy: p.Name,
	}
	if created.ClientOrderID == "" {
		created.ClientOrderID = orderID
	}
	if fields := domain.ValidateNewOrder(created); len(fields) > 0 {
		return Result{}, &Error{Status: http.StatusUnprocessableEntity, Code: "VALIDATION_FAILED",
			Message: "the order is not valid", Fields: fields}
	}
	if !p.CanTrade(req.AccountID) {
		return Result{}, errorf(http.StatusForbidden, "FORBIDDEN", "not allowed to trade account %s", req.AccountID)
	}

	// Fast path for a retry, before spending a risk check on it.
	switch rec, err := s.Store.LookupIdempotency(ctx, p.Name, key); {
	case err == nil:
		return s.replay(rec, opCreate, hash)
	case !errors.Is(err, store.ErrNotFound):
		return Result{}, err
	}

	decision, err := s.Risk.Authorize(ctx, risk.Request{
		OrderID: orderID, AccountID: req.AccountID, Symbol: req.Symbol, Side: req.Side,
		Type: req.Type, Qty: req.Qty, LimitPrice: req.LimitPrice,
	})
	if err != nil {
		// Fail closed: without a risk decision nothing is stored or sent.
		s.Log.Error("risk authorization failed", "err", err)
		return Result{}, errorf(http.StatusServiceUnavailable, "RISK_UNAVAILABLE", "pre-trade risk could not be checked; retry with the same Idempotency-Key")
	}

	cause := "idempotency-key:" + key
	drafts := []domain.Draft{{Type: domain.EvOrderCreated, Source: domain.SourceAPI, Actor: p.Name, CausationID: cause, Payload: created}}
	if decision.Allowed {
		drafts = append(drafts, domain.Draft{Type: domain.EvOrderValidated, Source: domain.SourceAPI, Actor: p.Name, CausationID: cause,
			Payload: domain.OrderValidated{Authorizer: decision.Authorizer, Reference: decision.Reference}})
	} else {
		drafts = append(drafts, domain.Draft{Type: domain.EvOrderRejected, Source: domain.SourceAPI, Actor: p.Name, CausationID: cause,
			Payload: domain.OrderRejected{By: domain.ByRisk, Reason: decision.Reason, Codes: decision.Codes}})
	}

	var res Result
	var order domain.Order
	err = s.Store.Tx(ctx, func(tx *store.Tx) error {
		claimed, existing, err := tx.ClaimIdempotency(ctx, p.Name, key, opCreate, hash)
		if err != nil {
			return err
		}
		if !claimed {
			// A concurrent request with the same key committed first.
			res, err = s.replay(existing, opCreate, hash)
			return err
		}
		order, _, err = tx.Append(ctx, orderID, domain.Order{}, drafts)
		if err != nil {
			return err
		}
		if order.State == domain.StateValidated {
			if _, err := tx.Enqueue(ctx, orderID, store.OutboxSubmit); err != nil {
				return err
			}
		}
		body, err := json.Marshal(View(order))
		if err != nil {
			return err
		}
		res = Result{Status: http.StatusCreated, Body: body}
		return tx.CompleteIdempotency(ctx, p.Name, key, orderID, res.Status, body)
	})
	if store.IsUniqueViolation(err, "orders_account_id_client_order_id_key") {
		dup := errorf(http.StatusConflict, "DUPLICATE_CLIENT_ORDER_ID",
			"client_order_id %s is already used by another order in account %s", created.ClientOrderID, req.AccountID)
		if existing, lerr := s.Store.GetOrderByClientID(ctx, req.AccountID, created.ClientOrderID); lerr == nil {
			dup.OrderID = existing.ID
		}
		return Result{}, dup
	}
	if err != nil {
		return Result{}, err
	}
	if res.Replayed {
		return res, nil
	}

	result := "rejected"
	if order.State == domain.StateValidated {
		result = "validated"
		s.nudge()
	}
	s.Metrics.OrdersCreated.Inc(result)
	s.Log.Info("order created", "order_id", order.ID, "account_id", order.AccountID, "state", order.State, "actor", p.Name)
	return res, nil
}

// CancelOrder requests cancellation. It is naturally idempotent: cancelling
// an order that is already cancelled, or whose cancel is already pending,
// changes nothing and returns the order.
//
// An order that was never sent is cancelled at once (200). Otherwise the
// cancel is queued for the broker (202) and the order stays in its current
// state, with pending_cancel set, until the broker confirms, because fills
// can still arrive in the meantime.
func (s *Service) CancelOrder(ctx context.Context, p auth.Principal, orderID string) (Result, error) {
	if _, err := s.authorizeOrder(ctx, p, orderID, true); err != nil {
		return Result{}, err
	}
	var res Result
	var dispatched bool
	err := s.Store.Tx(ctx, func(tx *store.Tx) error {
		o, err := tx.LockOrder(ctx, orderID)
		if err != nil {
			return err
		}
		drafts, dispatch, err := domain.DecideCancel(o, p.Name)
		if err != nil {
			return commandError(err)
		}
		if o, _, err = tx.Append(ctx, o.ID, o, drafts); err != nil {
			return err
		}
		if dispatch {
			if _, err := tx.Enqueue(ctx, o.ID, store.OutboxCancel); err != nil {
				return err
			}
		}
		dispatched = dispatch
		res.Status = http.StatusOK
		if o.PendingCancel {
			res.Status = http.StatusAccepted
		}
		res.Body, err = json.Marshal(View(o))
		return err
	})
	if err != nil {
		return Result{}, err
	}
	if dispatched {
		s.nudge()
	}
	return res, nil
}

// ReplaceOrder changes the quantity and/or limit price of an open order. The
// new terms go through pre-trade risk again. Like CreateOrder it requires an
// idempotency key.
//
// An order that was never sent is amended at once (200). Otherwise the
// replace is queued for the broker (202) and the order keeps its current
// terms, with pending_replace set, until the broker confirms.
func (s *Service) ReplaceOrder(ctx context.Context, p auth.Principal, key, orderID string, req ReplaceOrderRequest) (Result, error) {
	if err := checkKey(key); err != nil {
		return Result{}, err
	}
	current, err := s.authorizeOrder(ctx, p, orderID, true)
	if err != nil {
		return Result{}, err
	}
	if req.Qty == nil && req.LimitPrice == nil {
		return Result{}, errorf(http.StatusUnprocessableEntity, "VALIDATION_FAILED", "provide qty, limit_price or both")
	}
	op := opReplace + ":" + orderID
	hash := hashRequest(op, req)

	switch rec, err := s.Store.LookupIdempotency(ctx, p.Name, key); {
	case err == nil:
		return s.replay(rec, op, hash)
	case !errors.Is(err, store.ErrNotFound):
		return Result{}, err
	}

	if current.Type == domain.Market && req.LimitPrice != nil {
		return Result{}, errorf(http.StatusUnprocessableEntity, "VALIDATION_FAILED", "a MARKET order has no limit price to change")
	}
	// Terms that are not being changed are taken from the order as just read.
	// They are re-checked under the row lock below.
	terms := domain.Replace{Qty: current.Qty, LimitPrice: current.LimitPrice}
	if req.Qty != nil {
		terms.Qty = *req.Qty
	}
	if req.LimitPrice != nil {
		terms.LimitPrice = req.LimitPrice
	}

	decision, err := s.Risk.Authorize(ctx, risk.Request{
		OrderID: orderID, AccountID: current.AccountID, Symbol: current.Symbol, Side: current.Side,
		Type: current.Type, Qty: terms.Qty, LimitPrice: terms.LimitPrice,
	})
	if err != nil {
		s.Log.Error("risk authorization failed", "err", err)
		return Result{}, errorf(http.StatusServiceUnavailable, "RISK_UNAVAILABLE", "pre-trade risk could not be checked; retry with the same Idempotency-Key")
	}
	if !decision.Allowed {
		return Result{}, &Error{Status: http.StatusUnprocessableEntity, Code: "RISK_REJECTED",
			Message: decision.Reason, Codes: decision.Codes}
	}

	var res Result
	var dispatched bool
	err = s.Store.Tx(ctx, func(tx *store.Tx) error {
		claimed, existing, err := tx.ClaimIdempotency(ctx, p.Name, key, op, hash)
		if err != nil {
			return err
		}
		if !claimed {
			res, err = s.replay(existing, op, hash)
			return err
		}
		o, err := tx.LockOrder(ctx, orderID)
		if err != nil {
			return err
		}
		if o.Qty != current.Qty || !samePrice(o.LimitPrice, current.LimitPrice) {
			// Another replace landed between the risk check and this lock, so
			// the terms that were authorized are no longer the terms requested.
			return errorf(http.StatusConflict, "ORDER_CHANGED", "the order's terms changed while the replace was being checked; retry with a new Idempotency-Key")
		}
		drafts, dispatch, err := domain.DecideReplace(o, terms, p.Name)
		if err != nil {
			return commandError(err)
		}
		for i := range drafts {
			drafts[i].CausationID = "idempotency-key:" + key
		}
		if o, _, err = tx.Append(ctx, o.ID, o, drafts); err != nil {
			return err
		}
		if dispatch {
			if _, err := tx.Enqueue(ctx, o.ID, store.OutboxReplace); err != nil {
				return err
			}
		}
		dispatched = dispatch
		res.Status = http.StatusOK
		if o.PendingReplace != nil {
			res.Status = http.StatusAccepted
		}
		if res.Body, err = json.Marshal(View(o)); err != nil {
			return err
		}
		return tx.CompleteIdempotency(ctx, p.Name, key, orderID, res.Status, res.Body)
	})
	if err != nil {
		return Result{}, err
	}
	if dispatched {
		s.nudge()
	}
	return res, nil
}

// GetOrder returns one order the principal may see.
func (s *Service) GetOrder(ctx context.Context, p auth.Principal, orderID string) (OrderView, error) {
	o, err := s.authorizeOrder(ctx, p, orderID, false)
	if err != nil {
		return OrderView{}, err
	}
	return View(o), nil
}

// ListOrders returns the principal's orders, newest first.
func (s *Service) ListOrders(ctx context.Context, p auth.Principal, account string, state domain.State, limit int) ([]OrderView, error) {
	f := store.OrderFilter{Accounts: p.AccountScope(), State: state, Limit: limit}
	if account != "" {
		if !p.CanView(account) {
			return nil, errorf(http.StatusForbidden, "FORBIDDEN", "not allowed to view account %s", account)
		}
		f.Accounts = []string{account}
	}
	if state != "" && !state.Valid() {
		return nil, errorf(http.StatusBadRequest, "BAD_REQUEST", "unknown state %q", state)
	}
	orders, err := s.Store.ListOrders(ctx, f)
	if err != nil {
		return nil, err
	}
	out := make([]OrderView, len(orders))
	for i, o := range orders {
		out[i] = View(o)
	}
	return out, nil
}

// ListEvents returns an order's full history.
func (s *Service) ListEvents(ctx context.Context, p auth.Principal, orderID string) ([]domain.Event, error) {
	if _, err := s.authorizeOrder(ctx, p, orderID, false); err != nil {
		return nil, err
	}
	return s.Store.ListEvents(ctx, orderID)
}

// ListExecutions returns an order's execution records.
func (s *Service) ListExecutions(ctx context.Context, p auth.Principal, orderID string) ([]store.ExecutionRow, error) {
	if _, err := s.authorizeOrder(ctx, p, orderID, false); err != nil {
		return nil, err
	}
	return s.Store.ListExecutions(ctx, orderID)
}

// ListPositions returns positions for the principal's accounts.
func (s *Service) ListPositions(ctx context.Context, p auth.Principal, account string) ([]store.PositionRow, error) {
	scope := p.AccountScope()
	if account != "" {
		if !p.CanView(account) {
			return nil, errorf(http.StatusForbidden, "FORBIDDEN", "not allowed to view account %s", account)
		}
		scope = []string{account}
	}
	return s.Store.ListPositions(ctx, scope)
}

// authorizeOrder loads an order and checks the principal's access to it. An
// order in an account the principal cannot see is reported as not found, so
// order ids cannot be probed across accounts.
func (s *Service) authorizeOrder(ctx context.Context, p auth.Principal, orderID string, write bool) (domain.Order, error) {
	o, err := s.Store.GetOrder(ctx, orderID)
	if errors.Is(err, store.ErrNotFound) || (err == nil && !p.CanView(o.AccountID)) {
		return domain.Order{}, errOrderNotFound
	}
	if err != nil {
		return domain.Order{}, err
	}
	if write && !p.CanTrade(o.AccountID) {
		return domain.Order{}, errorf(http.StatusForbidden, "FORBIDDEN", "not allowed to trade account %s", o.AccountID)
	}
	return o, nil
}

func samePrice(a, b *fixed.Micros) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
}

// replay answers a request from its stored idempotency record.
func (s *Service) replay(rec store.IdempotencyRecord, op, hash string) (Result, error) {
	if rec.Operation != op || rec.RequestHash != hash {
		return Result{}, errorf(http.StatusUnprocessableEntity, "IDEMPOTENCY_KEY_REUSED",
			"this Idempotency-Key was already used for a different request")
	}
	if rec.Status == 0 {
		return Result{}, errorf(http.StatusConflict, "REQUEST_IN_PROGRESS", "the first request with this Idempotency-Key has not finished")
	}
	s.Metrics.IdempotentReplays.Inc(opName(op))
	return Result{Status: rec.Status, Body: rec.Body, Replayed: true}, nil
}

func opName(op string) string {
	if len(op) >= len(opReplace) && op[:len(opReplace)] == opReplace {
		return opReplace
	}
	return op
}

func (s *Service) nudge() {
	if s.Nudge != nil {
		s.Nudge()
	}
}

// commandError maps an aggregate refusal to an API error.
func commandError(err error) error {
	var ce *domain.CommandError
	if !errors.As(err, &ce) {
		return err
	}
	status := http.StatusConflict
	if ce.Code == domain.CodeInvalidReplace || ce.Code == domain.CodeNoChange {
		status = http.StatusUnprocessableEntity
	}
	return &Error{Status: status, Code: ce.Code, Message: ce.Message}
}

// checkKey validates an Idempotency-Key header value.
func checkKey(key string) error {
	if key == "" {
		return errorf(http.StatusBadRequest, "IDEMPOTENCY_KEY_REQUIRED", "the Idempotency-Key header is required")
	}
	if len(key) > 128 {
		return errorf(http.StatusBadRequest, "IDEMPOTENCY_KEY_INVALID", "the Idempotency-Key header is longer than 128 characters")
	}
	for i := 0; i < len(key); i++ {
		if key[i] < 0x21 || key[i] > 0x7e {
			return errorf(http.StatusBadRequest, "IDEMPOTENCY_KEY_INVALID", "the Idempotency-Key header must be printable ASCII without spaces")
		}
	}
	return nil
}

// hashRequest fingerprints a command so that a reused idempotency key can be
// told apart from a genuine retry. The request structs marshal with a fixed
// field order and prices as exact decimal strings, so equal requests hash
// equally however the client formatted them.
func hashRequest(op string, req any) string {
	b, err := json.Marshal(req)
	if err != nil {
		panic("service: request is not marshalable: " + err.Error())
	}
	h := sha256.New()
	h.Write([]byte(op))
	h.Write([]byte{0})
	h.Write(b)
	return hex.EncodeToString(h.Sum(nil))
}
