// Package brokersim is a simulated broker for paper trading, demos and tests.
// Nothing here connects to a real venue.
//
// It behaves like an external broker the OMS does not control: it accepts
// requests for processing, reports what happened later through signed
// webhooks, keeps its own books, and serves them as a snapshot for
// reconciliation. It can also be told to misbehave in the ways real brokers
// and networks do: drop connections, lose responses, lose, duplicate or
// reorder webhooks, run on a skewed clock, or report impossible fills.
//
// Two modes:
//   - Auto: orders are acknowledged and matched against a reference price per
//     symbol after short delays. Used by the demo, the benchmark and the
//     standalone binary.
//   - Manual: nothing happens until the caller says so (Ack, Fill, ...), and
//     webhooks are held until Flush. Used by tests that need exact control
//     over ordering.
package brokersim

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"oms/src/broker"
	"oms/src/clock"
	"oms/src/domain"
	"oms/src/fixed"
)

// Config sets up a simulator.
type Config struct {
	// APIKey is the bearer token the OMS must present.
	APIKey string
	// WebhookURL and WebhookSecret say where and how to push reports.
	WebhookURL    string
	WebhookSecret string

	// Auto turns on automatic acknowledgement and matching.
	Auto bool
	// AutoDeliver pushes webhooks as they are produced. When false they are
	// held until Flush.
	AutoDeliver bool
	// DeliveryWorkers is the number of concurrent webhook senders in
	// AutoDeliver mode. One preserves order; more can reorder. Default 1.
	DeliveryWorkers int
	// WebhookAttempts is how many times a webhook is tried before the
	// simulator gives up on it. Default 6.
	WebhookAttempts int
	// WebhookBackoff is the delay before the first retry; it doubles each
	// time. Default 100ms.
	WebhookBackoff time.Duration

	// AckDelay, FillDelay and CancelDelay pace the auto mode.
	AckDelay    time.Duration
	FillDelay   time.Duration
	CancelDelay time.Duration
	// Prices lists the tradable symbols with their reference prices. An order
	// for any other symbol is rejected.
	Prices map[string]fixed.Micros
	// FeePerUnit is charged on every filled unit.
	FeePerUnit fixed.Micros
	// MaxSlices is how many fills a marketable order is split into. Default 2.
	MaxSlices int

	// Clock is the broker's own clock. Default: the system clock.
	Clock clock.Clock
	Log   *slog.Logger
}

// Faults are the ways the simulator can be told to misbehave.
type Faults struct {
	// Down drops every API connection without a response.
	Down bool `json:"down"`
	// FailRequests makes the next N API requests fail with 503 before
	// anything is processed.
	FailRequests int `json:"fail_requests"`
	// LoseResponses makes the next N order requests be processed normally
	// and then answered with 503, as if the response were lost.
	LoseResponses int `json:"lose_responses"`
	// SnapshotDown makes the reconciliation snapshot unavailable.
	SnapshotDown bool `json:"snapshot_down"`
	// DropWebhooks makes the broker act without telling anyone: reports are
	// written to its books but never sent.
	DropWebhooks bool `json:"drop_webhooks"`
	// DuplicateWebhooks delivers each webhook this many extra times.
	DuplicateWebhooks int `json:"duplicate_webhooks"`
	// ClockSkew is added to the broker's clock.
	ClockSkew time.Duration `json:"clock_skew"`
}

// Event is one report the broker produced.
type Event struct {
	ID      string
	Type    string
	OrderID string
	Body    []byte
	// Dropped is true when the report was never queued for delivery.
	Dropped bool
}

// Stats counts what the simulator has seen.
type Stats struct {
	SubmitRequests   int `json:"submit_requests"`
	DuplicateSubmits int `json:"duplicate_submits"`
	CancelRequests   int `json:"cancel_requests"`
	ReplaceRequests  int `json:"replace_requests"`
	DuplicateActions int `json:"duplicate_actions"`
	EventsProduced   int `json:"events_produced"`
	WebhooksSent     int `json:"webhooks_sent"`
	WebhooksFailed   int `json:"webhooks_failed"`
	WebhooksLost     int `json:"webhooks_lost"`
}

type order struct {
	broker.OrderSnapshot
	Type          domain.OrderType
	TimeInForce   domain.TimeInForce
	cancelPending bool
	replace       *broker.ReplaceRequest
	// Auto mode: a fill timer is running, and how many slices have filled.
	matching bool
	slices   int
}

func (o *order) open() bool {
	switch o.Status {
	case broker.StatusReceived, broker.StatusWorking, broker.StatusPartiallyFilled:
		return true
	}
	return false
}

// Sim is a simulated broker.
type Sim struct {
	cfg Config

	mu         sync.Mutex
	faults     Faults
	orders     map[string]*order
	orderIDs   []string // insertion order, for stable snapshots
	execs      []broker.ExecutionSnapshot
	effective  map[string]int64 // execution id → quantity it currently contributes
	actionKeys map[string]bool  // idempotency keys of cancel and replace requests
	events     []Event
	queue      []int // indexes into events awaiting delivery
	stats      Stats
	seq        struct{ order, exec, event int }
	closed     bool
	timers     map[*time.Timer]struct{}

	wake    chan struct{}
	stop    chan struct{}
	workers sync.WaitGroup
}

// New builds a simulator and, in AutoDeliver mode, starts its webhook
// senders. Call Close when done.
func New(cfg Config) *Sim {
	if cfg.Clock == nil {
		cfg.Clock = clock.System{}
	}
	if cfg.Log == nil {
		cfg.Log = slog.New(slog.DiscardHandler)
	}
	if cfg.MaxSlices <= 0 {
		cfg.MaxSlices = 2
	}
	if cfg.DeliveryWorkers <= 0 {
		cfg.DeliveryWorkers = 1
	}
	if cfg.WebhookAttempts <= 0 {
		cfg.WebhookAttempts = 6
	}
	if cfg.WebhookBackoff <= 0 {
		cfg.WebhookBackoff = 100 * time.Millisecond
	}
	prices := make(map[string]fixed.Micros, len(cfg.Prices))
	for k, v := range cfg.Prices {
		prices[k] = v
	}
	cfg.Prices = prices

	s := &Sim{
		cfg:        cfg,
		orders:     map[string]*order{},
		effective:  map[string]int64{},
		actionKeys: map[string]bool{},
		timers:     map[*time.Timer]struct{}{},
		wake:       make(chan struct{}, 1),
		stop:       make(chan struct{}),
	}
	if cfg.AutoDeliver {
		for range cfg.DeliveryWorkers {
			s.workers.Add(1)
			go s.deliveryLoop()
		}
	}
	return s
}

// Close stops timers and webhook senders. Queued webhooks are abandoned.
func (s *Sim) Close() {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return
	}
	s.closed = true
	for t := range s.timers {
		t.Stop()
	}
	s.mu.Unlock()
	close(s.stop)
	s.workers.Wait()
}

// SetFaults replaces the fault settings.
func (s *Sim) SetFaults(f Faults) {
	s.mu.Lock()
	s.faults = f
	s.mu.Unlock()
}

// Faults returns the current fault settings.
func (s *Sim) Faults() Faults {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.faults
}

// Stats returns the counters.
func (s *Sim) Stats() Stats {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.stats
}

// now is the broker's clock, including any configured skew. Truncated to
// microseconds, the precision the OMS stores.
func (s *Sim) now() time.Time {
	return s.cfg.Clock.Now().Add(s.faults.ClockSkew).UTC().Truncate(time.Microsecond)
}

// ErrUnknown is returned by the manual controls for an id the simulator does
// not have.
var ErrUnknown = errors.New("brokersim: unknown order or execution")

// ErrNotOpen is returned by the manual controls when the order is closed.
var ErrNotOpen = errors.New("brokersim: order is not open")

// submit books a new order, or returns the existing one for a repeated
// client order id.
func (s *Sim) submit(r broker.SubmitRequest) (o *order, duplicate bool) {
	s.stats.SubmitRequests++
	if existing, ok := s.orders[r.ClientOrderID]; ok {
		s.stats.DuplicateSubmits++
		return existing, true
	}
	s.seq.order++
	o = &order{
		OrderSnapshot: broker.OrderSnapshot{
			ClientOrderID: r.ClientOrderID, BrokerOrderID: fmt.Sprintf("B-%06d", s.seq.order),
			Symbol: r.Symbol, Side: r.Side, Status: broker.StatusReceived,
			Qty: r.Qty, LimitPrice: r.LimitPrice, UpdatedAt: s.now(),
		},
		Type: r.Type, TimeInForce: r.TimeInForce,
	}
	s.orders[r.ClientOrderID] = o
	s.orderIDs = append(s.orderIDs, r.ClientOrderID)
	if s.cfg.Auto {
		s.after(s.cfg.AckDelay, func() { s.autoAccept(r.ClientOrderID) })
	}
	return o, false
}

// emit records a report and queues it for delivery. The caller holds s.mu.
func (s *Sim) emit(o *order, typ string, fill func(ev *broker.WebhookEvent)) Event {
	s.seq.event++
	ev := broker.WebhookEvent{
		EventID: fmt.Sprintf("evt-%06d", s.seq.event), Type: typ,
		ClientOrderID: o.ClientOrderID, BrokerOrderID: o.BrokerOrderID, EventTime: s.now(),
	}
	if fill != nil {
		fill(&ev)
	}
	body, err := json.Marshal(ev)
	if err != nil {
		panic("brokersim: " + err.Error())
	}
	o.UpdatedAt = ev.EventTime
	e := Event{ID: ev.EventID, Type: typ, OrderID: o.ClientOrderID, Body: body, Dropped: s.faults.DropWebhooks}
	s.events = append(s.events, e)
	s.stats.EventsProduced++
	if e.Dropped {
		s.stats.WebhooksLost++
		return e
	}
	s.queue = append(s.queue, len(s.events)-1)
	select {
	case s.wake <- struct{}{}:
	default:
	}
	return e
}

// --- Manual controls. Each acts on the broker's books and produces the
// report a real broker would send. ---

// Ack acknowledges a received order.
func (s *Sim) Ack(id string) (Event, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	o, ok := s.orders[id]
	if !ok {
		return Event{}, ErrUnknown
	}
	if o.Status != broker.StatusReceived {
		return Event{}, ErrNotOpen
	}
	o.Status = broker.StatusWorking
	return s.emit(o, broker.EventAcknowledged, nil), nil
}

// Reject refuses an order that has not traded.
func (s *Sim) Reject(id, reason string) (Event, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	o, ok := s.orders[id]
	if !ok {
		return Event{}, ErrUnknown
	}
	if !o.open() || o.CumQty != 0 {
		return Event{}, ErrNotOpen
	}
	o.Status, o.Reason = broker.StatusRejected, reason
	return s.emit(o, broker.EventRejected, func(ev *broker.WebhookEvent) { ev.Reason = reason }), nil
}

// Fill executes qty at price against an order and returns the execution id.
// It deliberately does not check the quantity against the order, so tests
// can make the broker report an overfill, or a fill on a closed order.
func (s *Sim) Fill(id string, qty int64, price fixed.Micros) (execID string, ev Event, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	o, ok := s.orders[id]
	if !ok {
		return "", Event{}, ErrUnknown
	}
	execID, ev = s.fill(o, qty, price)
	return execID, ev, nil
}

func (s *Sim) fill(o *order, qty int64, price fixed.Micros) (string, Event) {
	s.seq.exec++
	fee, _ := fixed.MulQty(s.cfg.FeePerUnit, qty)
	x := broker.ExecutionSnapshot{
		ExecID: fmt.Sprintf("X-%06d", s.seq.exec), Kind: domain.ExecFill,
		ClientOrderID: o.ClientOrderID, BrokerOrderID: o.BrokerOrderID,
		Qty: qty, Price: price, Fee: fee, ExecutedAt: s.now(),
	}
	s.execs = append(s.execs, x)
	s.effective[x.ExecID] = qty
	o.CumQty += qty
	s.restate(o)
	return x.ExecID, s.emit(o, broker.EventFill, func(ev *broker.WebhookEvent) {
		ev.ExecID, ev.Qty, ev.Price, ev.Fee = x.ExecID, qty, &x.Price, &x.Fee
	})
}

// Correct restates an earlier execution with a new quantity and price and
// returns the id of the correction record.
func (s *Sim) Correct(execID string, qty int64, price fixed.Micros) (string, Event, error) {
	return s.amend(execID, domain.ExecCorrect, qty, price)
}

// Bust cancels an earlier execution and returns the id of the bust record.
func (s *Sim) Bust(execID string) (string, Event, error) {
	return s.amend(execID, domain.ExecBust, 0, 0)
}

func (s *Sim) amend(execID string, kind domain.ExecKind, qty int64, price fixed.Micros) (string, Event, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var ref *broker.ExecutionSnapshot
	for i := range s.execs {
		if s.execs[i].ExecID == execID {
			ref = &s.execs[i]
		}
	}
	if ref == nil {
		return "", Event{}, ErrUnknown
	}
	o := s.orders[ref.ClientOrderID]
	s.seq.exec++
	fee, _ := fixed.MulQty(s.cfg.FeePerUnit, qty)
	x := broker.ExecutionSnapshot{
		ExecID: fmt.Sprintf("X-%06d", s.seq.exec), Kind: kind, RefExecID: execID,
		ClientOrderID: ref.ClientOrderID, BrokerOrderID: ref.BrokerOrderID,
		Qty: qty, Price: price, Fee: fee, ExecutedAt: s.now(),
	}
	s.execs = append(s.execs, x)
	o.CumQty += qty - s.effective[execID]
	s.effective[execID] = 0
	s.effective[x.ExecID] = qty
	s.restate(o)

	typ := broker.EventFillCorrected
	if kind == domain.ExecBust {
		typ = broker.EventFillBusted
	}
	return x.ExecID, s.emit(o, typ, func(ev *broker.WebhookEvent) {
		ev.ExecID, ev.RefExecID = x.ExecID, execID
		if kind == domain.ExecCorrect {
			ev.Qty, ev.Price, ev.Fee = qty, &x.Price, &x.Fee
		}
	}), nil
}

// restate derives an order's status from its cumulative quantity. An order
// the broker closed by cancel, expiry or reject stays closed.
func (s *Sim) restate(o *order) {
	switch o.Status {
	case broker.StatusCancelled, broker.StatusExpired, broker.StatusRejected:
		return
	}
	switch {
	case o.CumQty >= o.Qty:
		o.Status = broker.StatusFilled
	case o.CumQty > 0:
		o.Status = broker.StatusPartiallyFilled
	case o.Status != broker.StatusReceived:
		o.Status = broker.StatusWorking
	}
}

// ConfirmCancel cancels an open order, whether or not a cancel was requested
// (a broker can cancel on its own initiative).
func (s *Sim) ConfirmCancel(id string) (Event, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	o, ok := s.orders[id]
	if !ok {
		return Event{}, ErrUnknown
	}
	if !o.open() {
		return Event{}, ErrNotOpen
	}
	o.Status, o.cancelPending, o.replace = broker.StatusCancelled, false, nil
	return s.emit(o, broker.EventCancelled, nil), nil
}

// RejectCancel refuses a requested cancel.
func (s *Sim) RejectCancel(id, reason string) (Event, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	o, ok := s.orders[id]
	if !ok {
		return Event{}, ErrUnknown
	}
	o.cancelPending = false
	return s.emit(o, broker.EventCancelRejected, func(ev *broker.WebhookEvent) { ev.Reason = reason }), nil
}

// ConfirmReplace applies the requested replace.
func (s *Sim) ConfirmReplace(id string) (Event, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	o, ok := s.orders[id]
	if !ok || o.replace == nil {
		return Event{}, ErrUnknown
	}
	if !o.open() {
		return Event{}, ErrNotOpen
	}
	o.Qty, o.LimitPrice, o.replace = o.replace.Qty, o.replace.LimitPrice, nil
	return s.emit(o, broker.EventReplaced, func(ev *broker.WebhookEvent) {
		ev.OrderQty, ev.LimitPrice = o.Qty, o.LimitPrice
	}), nil
}

// RejectReplace refuses the requested replace.
func (s *Sim) RejectReplace(id, reason string) (Event, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	o, ok := s.orders[id]
	if !ok {
		return Event{}, ErrUnknown
	}
	o.replace = nil
	return s.emit(o, broker.EventReplaceReject, func(ev *broker.WebhookEvent) { ev.Reason = reason }), nil
}

// Expire closes an open order as expired.
func (s *Sim) Expire(id, reason string) (Event, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	o, ok := s.orders[id]
	if !ok {
		return Event{}, ErrUnknown
	}
	if !o.open() {
		return Event{}, ErrNotOpen
	}
	o.Status, o.Reason, o.cancelPending, o.replace = broker.StatusExpired, reason, false, nil
	return s.emit(o, broker.EventExpired, func(ev *broker.WebhookEvent) { ev.Reason = reason }), nil
}

// CloseSession expires every open DAY order, as the end of a trading day
// would.
func (s *Sim) CloseSession() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for _, id := range s.orderIDs {
		o := s.orders[id]
		if o.open() && o.TimeInForce == domain.Day {
			o.Status, o.Reason, o.cancelPending, o.replace = broker.StatusExpired, "session closed", false, nil
			s.emit(o, broker.EventExpired, func(ev *broker.WebhookEvent) { ev.Reason = o.Reason })
			n++
		}
	}
	return n
}

// --- Book tampering, for reconciliation tests. None of these send a
// report: they make the broker's books differ from what it told the OMS. ---

// Forget erases an order and its executions from the broker's books.
func (s *Sim) Forget(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.orders, id)
	ids := s.orderIDs[:0]
	for _, v := range s.orderIDs {
		if v != id {
			ids = append(ids, v)
		}
	}
	s.orderIDs = ids
	execs := s.execs[:0]
	for _, x := range s.execs {
		if x.ClientOrderID != id {
			execs = append(execs, x)
		}
	}
	s.execs = execs
}

// InjectOrder puts an order on the broker's books that nobody submitted.
func (s *Sim) InjectOrder(o broker.OrderSnapshot) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if o.UpdatedAt.IsZero() {
		o.UpdatedAt = s.now()
	}
	s.orders[o.ClientOrderID] = &order{OrderSnapshot: o, Type: domain.Limit, TimeInForce: domain.Day}
	s.orderIDs = append(s.orderIDs, o.ClientOrderID)
}

// TamperExecution changes the price of an execution on the broker's books.
func (s *Sim) TamperExecution(execID string, price fixed.Micros) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.execs {
		if s.execs[i].ExecID == execID {
			s.execs[i].Price = price
			return nil
		}
	}
	return ErrUnknown
}

// SetStatus overwrites an order's status on the broker's books.
func (s *Sim) SetStatus(id, status string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	o, ok := s.orders[id]
	if !ok {
		return ErrUnknown
	}
	o.Status = status
	return nil
}

// --- Views ---

// Order returns the broker's view of one order.
func (s *Sim) Order(id string) (broker.OrderSnapshot, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	o, ok := s.orders[id]
	if !ok {
		return broker.OrderSnapshot{}, false
	}
	return o.OrderSnapshot, true
}

// Snapshot returns the broker's books.
func (s *Sim) Snapshot() broker.Snapshot {
	s.mu.Lock()
	defer s.mu.Unlock()
	snap := broker.Snapshot{
		AsOf:       s.now(),
		Orders:     make([]broker.OrderSnapshot, 0, len(s.orderIDs)),
		Executions: append([]broker.ExecutionSnapshot{}, s.execs...),
	}
	for _, id := range s.orderIDs {
		snap.Orders = append(snap.Orders, s.orders[id].OrderSnapshot)
	}
	return snap
}

// Events returns every report the broker has produced, in order.
func (s *Sim) Events() []Event {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]Event{}, s.events...)
}

// after runs fn once after d, unless the simulator is closed first.
func (s *Sim) after(d time.Duration, fn func()) {
	if s.closed {
		return
	}
	var t *time.Timer
	t = time.AfterFunc(d, func() {
		s.mu.Lock()
		delete(s.timers, t)
		closed := s.closed
		s.mu.Unlock()
		if !closed {
			fn()
		}
	})
	s.timers[t] = struct{}{}
}
