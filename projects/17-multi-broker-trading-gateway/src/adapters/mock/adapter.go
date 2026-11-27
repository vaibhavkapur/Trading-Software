package mock

import (
	"context"
	"fmt"
	"math"
	"math/rand"
	"sync"
	"sync/atomic"
	"time"

	"github.com/vaibhavkapur/Trading-Software/projects/17-multi-broker-trading-gateway/src/broker"
	"github.com/vaibhavkapur/Trading-Software/projects/17-multi-broker-trading-gateway/src/domain"
	"github.com/vaibhavkapur/Trading-Software/projects/17-multi-broker-trading-gateway/src/ratelimit"
	"github.com/vaibhavkapur/Trading-Software/projects/17-multi-broker-trading-gateway/src/security"
)

// FaultMode injects deterministic failures for contract and scenario testing.
type FaultMode string

const (
	FaultNone                    FaultMode = ""
	FaultRateLimit               FaultMode = "rate_limit"
	FaultExpiredCredential       FaultMode = "expired_credential"
	FaultUnknownSubmitAccepted   FaultMode = "unknown_submit_accepted"
	FaultUnknownSubmitDropped    FaultMode = "unknown_submit_dropped"
	FaultCancelTimeoutCancelled  FaultMode = "cancel_timeout_cancelled"
	FaultCancelTimeoutFilled     FaultMode = "cancel_timeout_filled"
	FaultCancelTimeoutUnresolved FaultMode = "cancel_timeout_unresolved"
)

// Adapter is a deterministic seeded paper broker adapter with programmable fault injection.
type Adapter struct {
	mu           sync.Mutex
	brokerID     string
	caps         domain.BrokerCapabilities
	secrets      security.SecretManager
	secretRef    string
	clock        ratelimit.Clock
	rng          *rand.Rand
	refPrices    map[string]float64
	orders       map[string]*broker.BrokerOrderSnapshot // by clientOrderID
	byBrokerID   map[string]string                      // brokerOrderID -> clientOrderID
	fills        map[string][]broker.BrokerFillRecord   // by clientOrderID
	eventLog     []domain.WebhookEvent
	orderSeq     int64
	fillSeq      int64
	dispatchCnt  atomic.Int64
	nextSubmitF  FaultMode
	nextCancelF  FaultMode
	retryAfter   time.Duration
}

// New constructs a deterministic mock broker adapter.
func New(caps domain.BrokerCapabilities, sm security.SecretManager, secretRef string, seed int64, clock ratelimit.Clock) *Adapter {
	if clock == nil {
		clock = ratelimit.RealClock()
	}
	id := caps.BrokerID
	if id == "" {
		id = "mock"
		caps.BrokerID = id
	}
	return &Adapter{
		brokerID:  id,
		caps:      caps,
		secrets:   sm,
		secretRef: secretRef,
		clock:     clock,
		rng:       rand.New(rand.NewSource(seed)),
		refPrices: map[string]float64{
			"AAPL":    195.00,
			"MSFT":    412.00,
			"SPY":     560.00,
			"BTC-USD": 91000.00,
		},
		orders:     make(map[string]*broker.BrokerOrderSnapshot),
		byBrokerID: make(map[string]string),
		fills:      make(map[string][]broker.BrokerFillRecord),
		retryAfter: 500 * time.Millisecond,
	}
}

func (a *Adapter) BrokerID() string                      { return a.brokerID }
func (a *Adapter) Capabilities() domain.BrokerCapabilities { return a.caps }
func (a *Adapter) DispatchCount() int64                  { return a.dispatchCnt.Load() }

// InjectNextSubmitFault arms a one-shot submit fault.
func (a *Adapter) InjectNextSubmitFault(f FaultMode) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.nextSubmitF = f
}

// InjectNextCancelFault arms a one-shot cancel fault.
func (a *Adapter) InjectNextCancelFault(f FaultMode) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.nextCancelF = f
}

// InjectPhantomOrder creates a broker-side order unknown to the gateway to exercise reconciliation drift.
func (a *Adapter) InjectPhantomOrder(brokerSymbol string, qty, price float64) broker.BrokerOrderSnapshot {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.orderSeq++
	boid := fmt.Sprintf("mock-phantom-%d", a.orderSeq)
	cid := fmt.Sprintf("external-phantom-%d", a.orderSeq)
	snap := &broker.BrokerOrderSnapshot{
		BrokerOrderID:  boid,
		ClientOrderID:  cid,
		BrokerSymbol:   brokerSymbol,
		Side:           domain.SideBuy,
		OrderType:      domain.OrderTypeMarket,
		Quantity:       qty,
		FilledQuantity: qty,
		AvgFillPrice:   price,
		Status:         domain.StatusFilled,
		UpdatedAtUTC:   a.clock.Now(),
	}
	a.orders[cid] = snap
	a.byBrokerID[boid] = cid
	return *snap
}

func (a *Adapter) verifyAuth() error {
	if a.secrets == nil || a.secretRef == "" {
		return nil
	}
	cred, err := a.secrets.GetCredential(a.secretRef)
	if err != nil {
		return domain.NewError(domain.ErrCodeAuthInvalid, "missing broker credential", false).WithContext(a.brokerID, "")
	}
	if !cred.IsValidAt(a.clock.Now()) {
		return domain.NewError(domain.ErrCodeAuthExpired, fmt.Sprintf("credential %q expired or revoked", a.secretRef), false).WithContext(a.brokerID, "")
	}
	return nil
}

func (a *Adapter) SubmitOrder(_ context.Context, req broker.BrokerOrderSubmit) (broker.BrokerSubmitAck, error) {
	a.dispatchCnt.Add(1)
	a.mu.Lock()
	defer a.mu.Unlock()

	fault := a.nextSubmitF
	a.nextSubmitF = FaultNone

	if fault == FaultExpiredCredential {
		return broker.BrokerSubmitAck{}, domain.NewError(domain.ErrCodeAuthExpired, "injected expired credential", false).WithContext(a.brokerID, req.OrderID)
	}
	if err := a.verifyAuth(); err != nil {
		return broker.BrokerSubmitAck{}, err
	}
	if fault == FaultRateLimit {
		return broker.BrokerSubmitAck{}, domain.NewError(domain.ErrCodeRateLimited, "mock upstream 429 rate limit", true).
			WithContext(a.brokerID, req.OrderID).WithRetryAfter(a.retryAfter)
	}
	if fault == FaultUnknownSubmitDropped {
		return broker.BrokerSubmitAck{}, domain.NewError(domain.ErrCodeUnknownOutcome, "submit connection reset before broker receipt", false).
			WithContext(a.brokerID, req.OrderID)
	}

	// Idempotent broker-side behavior if client_order_id already exists.
	if existing, ok := a.orders[req.ClientOrderID]; ok {
		return broker.BrokerSubmitAck{
			BrokerOrderID: existing.BrokerOrderID,
			Status:        existing.Status,
			Fills:         append([]broker.BrokerFillRecord(nil), a.fills[req.ClientOrderID]...),
			AckTimeUTC:    a.clock.Now(),
		}, nil
	}

	a.orderSeq++
	boid := fmt.Sprintf("mock-ord-%06d", a.orderSeq)
	now := a.clock.Now()

	snap := &broker.BrokerOrderSnapshot{
		BrokerOrderID: boid,
		ClientOrderID: req.ClientOrderID,
		BrokerSymbol:  req.BrokerSymbol,
		Side:          req.Side,
		OrderType:     req.OrderType,
		Quantity:      req.Quantity,
		Status:        domain.StatusAccepted,
		UpdatedAtUTC:  now,
	}

	var fills []broker.BrokerFillRecord
	refPx := a.refPrices[req.BrokerSymbol]
	if refPx == 0 {
		refPx = 100.0
	}

	shouldFill := req.OrderType == domain.OrderTypeMarket
	if req.OrderType == domain.OrderTypeLimit {
		if req.Side == domain.SideBuy && req.LimitPrice >= refPx {
			shouldFill = true
		}
		if req.Side == domain.SideSell && req.LimitPrice <= refPx {
			shouldFill = true
		}
	}

	if shouldFill {
		a.fillSeq++
		slip := math.Round((a.rng.Float64()-0.5)*0.04*100) / 100
		execPx := math.Round((refPx+slip)*100) / 100
		if req.OrderType == domain.OrderTypeLimit {
			execPx = req.LimitPrice
		}
		fee := math.Round(req.Quantity*execPx*0.0001*10000) / 10000
		fill := broker.BrokerFillRecord{
			EventID:       fmt.Sprintf("mock-evt-%d", a.fillSeq),
			BrokerOrderID: boid,
			ClientOrderID: req.ClientOrderID,
			BrokerFillID:  fmt.Sprintf("mock-fill-%06d", a.fillSeq),
			BrokerSymbol:  req.BrokerSymbol,
			Side:          req.Side,
			Sequence:      1,
			Quantity:      req.Quantity,
			Price:         execPx,
			Fee:           fee,
			Currency:      req.Currency,
			ExecutedAtUTC: now,
		}
		fills = append(fills, fill)
		a.fills[req.ClientOrderID] = append(a.fills[req.ClientOrderID], fill)
		snap.FilledQuantity = req.Quantity
		snap.AvgFillPrice = execPx
		snap.Status = domain.StatusFilled
		a.eventLog = append(a.eventLog, domain.WebhookEvent{
			EventID:       fill.EventID,
			BrokerID:      a.brokerID,
			EventType:     "FILL",
			BrokerOrderID: boid,
			ClientOrderID: req.ClientOrderID,
			BrokerFillID:  fill.BrokerFillID,
			Sequence:      fill.Sequence,
			FillQty:       fill.Quantity,
			FillPrice:     fill.Price,
			Fee:           fill.Fee,
			Currency:      fill.Currency,
			EventTimeUTC:  now,
		})
	}

	a.orders[req.ClientOrderID] = snap
	a.byBrokerID[boid] = req.ClientOrderID

	if fault == FaultUnknownSubmitAccepted {
		// Order IS stored at broker, but transport timed out returning the response!
		return broker.BrokerSubmitAck{}, domain.NewError(domain.ErrCodeUnknownOutcome, "submit response timeout after broker acceptance", false).
			WithContext(a.brokerID, req.OrderID)
	}

	return broker.BrokerSubmitAck{
		BrokerOrderID: boid,
		Status:        snap.Status,
		Fills:         fills,
		AckTimeUTC:    now,
	}, nil
}

func (a *Adapter) CancelOrder(_ context.Context, brokerOrderID, clientOrderID string) (broker.BrokerCancelAck, error) {
	a.dispatchCnt.Add(1)
	a.mu.Lock()
	defer a.mu.Unlock()

	if err := a.verifyAuth(); err != nil {
		return broker.BrokerCancelAck{}, err
	}

	cid := clientOrderID
	if cid == "" {
		cid = a.byBrokerID[brokerOrderID]
	}
	snap, ok := a.orders[cid]
	if !ok {
		return broker.BrokerCancelAck{}, domain.NewError(domain.ErrCodeOrderNotFound, "order not found at mock broker", false).
			WithContext(a.brokerID, brokerOrderID)
	}

	fault := a.nextCancelF
	a.nextCancelF = FaultNone
	now := a.clock.Now()

	switch fault {
	case FaultCancelTimeoutCancelled:
		snap.Status = domain.StatusCancelled
		snap.UpdatedAtUTC = now
		return broker.BrokerCancelAck{}, domain.NewError(domain.ErrCodeCancelTimeout, "cancel ack timed out (broker actually cancelled)", false).
			WithContext(a.brokerID, snap.BrokerOrderID)
	case FaultCancelTimeoutFilled:
		rem := snap.Quantity - snap.FilledQuantity
		if rem > 0 {
			a.fillSeq++
			px := a.refPrices[snap.BrokerSymbol]
			if px == 0 {
				px = 100.0
			}
			fill := broker.BrokerFillRecord{
				EventID:       fmt.Sprintf("mock-evt-%d", a.fillSeq),
				BrokerOrderID: snap.BrokerOrderID,
				ClientOrderID: snap.ClientOrderID,
				BrokerFillID:  fmt.Sprintf("mock-fill-%06d", a.fillSeq),
				BrokerSymbol:  snap.BrokerSymbol,
				Side:          snap.Side,
				Sequence:      int64(len(a.fills[cid]) + 1),
				Quantity:      rem,
				Price:         px,
				Fee:           math.Round(rem*px*0.0001*10000) / 10000,
				Currency:      "USD",
				ExecutedAtUTC: now,
			}
			a.fills[cid] = append(a.fills[cid], fill)
			snap.FilledQuantity = snap.Quantity
			snap.AvgFillPrice = px
		}
		snap.Status = domain.StatusFilled
		snap.UpdatedAtUTC = now
		return broker.BrokerCancelAck{}, domain.NewError(domain.ErrCodeCancelTimeout, "cancel ack timed out (order filled before cancel)", false).
			WithContext(a.brokerID, snap.BrokerOrderID)
	case FaultCancelTimeoutUnresolved:
		// Broker keeps order in CANCEL_PENDING to simulate stuck cancel requiring fail-closed operator intervention.
		snap.Status = domain.StatusCancelPending
		snap.UpdatedAtUTC = now
		return broker.BrokerCancelAck{}, domain.NewError(domain.ErrCodeCancelTimeout, "cancel ack timed out and broker still cancel_pending", false).
			WithContext(a.brokerID, snap.BrokerOrderID)
	}

	if snap.Status.IsTerminal() {
		return broker.BrokerCancelAck{
			BrokerOrderID: snap.BrokerOrderID,
			Status:        snap.Status,
			CancelTimeUTC: now,
		}, nil
	}

	snap.Status = domain.StatusCancelled
	snap.UpdatedAtUTC = now
	return broker.BrokerCancelAck{
		BrokerOrderID: snap.BrokerOrderID,
		Status:        domain.StatusCancelled,
		CancelTimeUTC: now,
	}, nil
}

func (a *Adapter) GetOrderByClientID(_ context.Context, clientOrderID string) (broker.BrokerOrderSnapshot, bool, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if err := a.verifyAuth(); err != nil {
		return broker.BrokerOrderSnapshot{}, false, err
	}
	snap, ok := a.orders[clientOrderID]
	if !ok {
		return broker.BrokerOrderSnapshot{}, false, nil
	}
	return *snap, true, nil
}

func (a *Adapter) ListOrders(_ context.Context) ([]broker.BrokerOrderSnapshot, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if err := a.verifyAuth(); err != nil {
		return nil, err
	}
	out := make([]broker.BrokerOrderSnapshot, 0, len(a.orders))
	for _, s := range a.orders {
		out = append(out, *s)
	}
	return out, nil
}

func (a *Adapter) ListFills(_ context.Context, clientOrderID string) ([]broker.BrokerFillRecord, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if err := a.verifyAuth(); err != nil {
		return nil, err
	}
	if clientOrderID != "" {
		return append([]broker.BrokerFillRecord(nil), a.fills[clientOrderID]...), nil
	}
	var out []broker.BrokerFillRecord
	for _, list := range a.fills {
		out = append(out, list...)
	}
	return out, nil
}

func (a *Adapter) ReplayEvents(_ context.Context, afterCursor int64) ([]domain.WebhookEvent, int64, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if afterCursor < 0 {
		afterCursor = 0
	}
	if int(afterCursor) >= len(a.eventLog) {
		return nil, int64(len(a.eventLog)), nil
	}
	return append([]domain.WebhookEvent(nil), a.eventLog[afterCursor:]...), int64(len(a.eventLog)), nil
}

func (a *Adapter) HealthCheck(_ context.Context) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.verifyAuth()
}
