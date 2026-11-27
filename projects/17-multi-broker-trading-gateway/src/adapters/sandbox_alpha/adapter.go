package sandbox_alpha

import (
	"context"
	"fmt"
	"math"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/vaibhavkapur/Trading-Software/projects/17-multi-broker-trading-gateway/src/broker"
	"github.com/vaibhavkapur/Trading-Software/projects/17-multi-broker-trading-gateway/src/domain"
	"github.com/vaibhavkapur/Trading-Software/projects/17-multi-broker-trading-gateway/src/ratelimit"
	"github.com/vaibhavkapur/Trading-Software/projects/17-multi-broker-trading-gateway/src/security"
)

// Adapter implements Approved Sandbox Adapter #1 (synchronous REST + webhook push semantics).
// Distinguishing characteristics:
// - OAuth Bearer token authentication validated via SecretManager on every call
// - Strict "US:<TICKER>" symbol format validation
// - Whole-share equities & ETFs only; MARKET & LIMIT; DAY & IOC
// - Server-side rate limit returning Retry-After duration
type Adapter struct {
	mu                 sync.Mutex
	brokerID           string
	caps               domain.BrokerCapabilities
	secrets            security.SecretManager
	secretRef          string
	clock              ratelimit.Clock
	serverLimiter      *ratelimit.TokenBucket
	refPrices          map[string]float64
	orders             map[string]*broker.BrokerOrderSnapshot
	byBrokerID         map[string]string
	fills              map[string][]broker.BrokerFillRecord
	webhookQueue       []domain.WebhookEvent
	orderSeq           int64
	fillSeq            int64
	dispatchCnt        atomic.Int64
	splitPartialFills  bool
	forceRateLimitOnce bool
	forceUnknownSubmit bool
}

// New constructs Sandbox Alpha adapter.
func New(caps domain.BrokerCapabilities, sm security.SecretManager, secretRef string, ratePerSec float64, burst int, clock ratelimit.Clock) *Adapter {
	if clock == nil {
		clock = ratelimit.RealClock()
	}
	if caps.BrokerID == "" {
		caps.BrokerID = "sandbox-alpha"
	}
	return &Adapter{
		brokerID:      caps.BrokerID,
		caps:          caps,
		secrets:       sm,
		secretRef:     secretRef,
		clock:         clock,
		serverLimiter: ratelimit.NewTokenBucket(caps.BrokerID+"-server", ratePerSec, burst, clock),
		refPrices: map[string]float64{
			"US:AAPL": 195.25,
			"US:MSFT": 412.50,
			"US:SPY":  560.10,
		},
		orders:     make(map[string]*broker.BrokerOrderSnapshot),
		byBrokerID: make(map[string]string),
		fills:      make(map[string][]broker.BrokerFillRecord),
	}
}

func (a *Adapter) BrokerID() string                        { return a.brokerID }
func (a *Adapter) Capabilities() domain.BrokerCapabilities { return a.caps }
func (a *Adapter) DispatchCount() int64                    { return a.dispatchCnt.Load() }

// SetSplitPartialFills toggles two-tranche deterministic fill generation for webhook testing.
func (a *Adapter) SetSplitPartialFills(enabled bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.splitPartialFills = enabled
}

// InjectUpstreamRateLimitOnce forces the next submit to return HTTP 429 style Retry-After.
func (a *Adapter) InjectUpstreamRateLimitOnce() {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.forceRateLimitOnce = true
}

// InjectUnknownSubmitOnce simulates a post-acceptance network timeout.
func (a *Adapter) InjectUnknownSubmitOnce() {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.forceUnknownSubmit = true
}

// DrainWebhooks returns and clears pending sandbox webhook notifications.
func (a *Adapter) DrainWebhooks() []domain.WebhookEvent {
	a.mu.Lock()
	defer a.mu.Unlock()
	out := append([]domain.WebhookEvent(nil), a.webhookQueue...)
	a.webhookQueue = nil
	return out
}

func (a *Adapter) authenticate() error {
	if a.secrets == nil {
		return domain.NewError(domain.ErrCodeAuthInvalid, "sandbox-alpha secret manager not configured", false).WithContext(a.brokerID, "")
	}
	cred, err := a.secrets.GetCredential(a.secretRef)
	if err != nil {
		return domain.NewError(domain.ErrCodeAuthInvalid, "sandbox-alpha oauth token unavailable", false).WithContext(a.brokerID, "")
	}
	if cred.Scheme != "" && cred.Scheme != "BEARER" {
		return domain.NewError(domain.ErrCodeAuthInvalid, "sandbox-alpha requires BEARER credential scheme", false).WithContext(a.brokerID, "")
	}
	if !cred.IsValidAt(a.clock.Now()) {
		return domain.NewError(domain.ErrCodeAuthExpired, "sandbox-alpha oauth bearer token expired or revoked", false).WithContext(a.brokerID, "")
	}
	return nil
}

func (a *Adapter) SubmitOrder(_ context.Context, req broker.BrokerOrderSubmit) (broker.BrokerSubmitAck, error) {
	a.dispatchCnt.Add(1)
	a.mu.Lock()
	defer a.mu.Unlock()

	if err := a.authenticate(); err != nil {
		return broker.BrokerSubmitAck{}, err
	}
	if a.forceRateLimitOnce {
		a.forceRateLimitOnce = false
		return broker.BrokerSubmitAck{}, domain.NewError(domain.ErrCodeRateLimited, "sandbox-alpha upstream 429 Too Many Requests", true).
			WithContext(a.brokerID, req.OrderID).WithRetryAfter(300 * time.Millisecond)
	}
	if err := a.serverLimiter.Allow(); err != nil {
		return broker.BrokerSubmitAck{}, err
	}
	if !strings.HasPrefix(req.BrokerSymbol, "US:") {
		return broker.BrokerSubmitAck{}, domain.NewError(domain.ErrCodeSymbolMismatch, fmt.Sprintf("sandbox-alpha requires US:<TICKER> symbol, got %q", req.BrokerSymbol), false).
			WithContext(a.brokerID, req.OrderID)
	}
	if existing, ok := a.orders[req.ClientOrderID]; ok {
		return broker.BrokerSubmitAck{
			BrokerOrderID: existing.BrokerOrderID,
			Status:        existing.Status,
			Fills:         append([]broker.BrokerFillRecord(nil), a.fills[req.ClientOrderID]...),
			AckTimeUTC:    a.clock.Now(),
		}, nil
	}

	a.orderSeq++
	boid := fmt.Sprintf("alpha-ord-%06d", a.orderSeq)
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

	refPx, knownPx := a.refPrices[req.BrokerSymbol]
	if !knownPx {
		return broker.BrokerSubmitAck{}, domain.NewError(domain.ErrCodeSymbolMismatch, fmt.Sprintf("symbol %q not tradable on sandbox-alpha", req.BrokerSymbol), false).
			WithContext(a.brokerID, req.OrderID)
	}

	marketable := req.OrderType == domain.OrderTypeMarket ||
		(req.OrderType == domain.OrderTypeLimit && ((req.Side == domain.SideBuy && req.LimitPrice >= refPx) || (req.Side == domain.SideSell && req.LimitPrice <= refPx)))

	var ackFills []broker.BrokerFillRecord
	if marketable {
		execPx := refPx
		if req.OrderType == domain.OrderTypeLimit {
			execPx = req.LimitPrice
		}
		if a.splitPartialFills && req.Quantity >= 2 {
			q1 := math.Floor(req.Quantity * 0.4)
			if q1 < 1 {
				q1 = 1
			}
			q2 := req.Quantity - q1
			ackFills = append(ackFills, a.recordFillLocked(snap, 1, q1, execPx-0.05, req.Currency, now))
			ackFills = append(ackFills, a.recordFillLocked(snap, 2, q2, execPx+0.05, req.Currency, now.Add(time.Second)))
		} else {
			ackFills = append(ackFills, a.recordFillLocked(snap, 1, req.Quantity, execPx, req.Currency, now))
		}
		var totQty, totVal float64
		for _, f := range ackFills {
			totQty += f.Quantity
			totVal += f.Quantity * f.Price
		}
		snap.FilledQuantity = totQty
		snap.AvgFillPrice = math.Round((totVal/totQty)*1e8) / 1e8
		snap.Status = domain.StatusFilled
	} else if req.TimeInForce == domain.TIFIOC {
		snap.Status = domain.StatusExpired
	}

	a.orders[req.ClientOrderID] = snap
	a.byBrokerID[boid] = req.ClientOrderID

	if a.forceUnknownSubmit {
		a.forceUnknownSubmit = false
		return broker.BrokerSubmitAck{}, domain.NewError(domain.ErrCodeUnknownOutcome, "sandbox-alpha HTTP gateway timeout after acceptance", false).
			WithContext(a.brokerID, req.OrderID)
	}

	return broker.BrokerSubmitAck{
		BrokerOrderID: boid,
		Status:        snap.Status,
		Fills:         ackFills,
		AckTimeUTC:    now,
	}, nil
}

func (a *Adapter) recordFillLocked(snap *broker.BrokerOrderSnapshot, seq int64, qty, price float64, currency string, ts time.Time) broker.BrokerFillRecord {
	a.fillSeq++
	f := broker.BrokerFillRecord{
		EventID:       fmt.Sprintf("alpha-wh-%06d", a.fillSeq),
		BrokerOrderID: snap.BrokerOrderID,
		ClientOrderID: snap.ClientOrderID,
		BrokerFillID:  fmt.Sprintf("alpha-fill-%06d", a.fillSeq),
		BrokerSymbol:  snap.BrokerSymbol,
		Side:          snap.Side,
		Sequence:      seq,
		Quantity:      qty,
		Price:         math.Round(price*100) / 100,
		Fee:           math.Round(qty*price*0.00015*10000) / 10000,
		Currency:      currency,
		ExecutedAtUTC: ts.UTC(),
	}
	a.fills[snap.ClientOrderID] = append(a.fills[snap.ClientOrderID], f)
	a.webhookQueue = append(a.webhookQueue, domain.WebhookEvent{
		EventID:       f.EventID,
		BrokerID:      a.brokerID,
		EventType:     "FILL",
		BrokerOrderID: f.BrokerOrderID,
		ClientOrderID: f.ClientOrderID,
		BrokerFillID:  f.BrokerFillID,
		Sequence:      f.Sequence,
		FillQty:       f.Quantity,
		FillPrice:     f.Price,
		Fee:           f.Fee,
		Currency:      f.Currency,
		EventTimeUTC:  f.ExecutedAtUTC,
	})
	return f
}

func (a *Adapter) CancelOrder(_ context.Context, brokerOrderID, clientOrderID string) (broker.BrokerCancelAck, error) {
	a.dispatchCnt.Add(1)
	a.mu.Lock()
	defer a.mu.Unlock()

	if err := a.authenticate(); err != nil {
		return broker.BrokerCancelAck{}, err
	}
	if err := a.serverLimiter.Allow(); err != nil {
		return broker.BrokerCancelAck{}, err
	}

	cid := clientOrderID
	if cid == "" {
		cid = a.byBrokerID[brokerOrderID]
	}
	snap, ok := a.orders[cid]
	if !ok {
		return broker.BrokerCancelAck{}, domain.NewError(domain.ErrCodeOrderNotFound, "order not found at sandbox-alpha", false).
			WithContext(a.brokerID, brokerOrderID)
	}
	now := a.clock.Now()
	if !snap.Status.IsTerminal() {
		snap.Status = domain.StatusCancelled
		snap.UpdatedAtUTC = now
	}
	return broker.BrokerCancelAck{
		BrokerOrderID: snap.BrokerOrderID,
		Status:        snap.Status,
		CancelTimeUTC: now,
	}, nil
}

func (a *Adapter) GetOrderByClientID(_ context.Context, clientOrderID string) (broker.BrokerOrderSnapshot, bool, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if err := a.authenticate(); err != nil {
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
	if err := a.authenticate(); err != nil {
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
	if err := a.authenticate(); err != nil {
		return nil, err
	}
	if clientOrderID != "" {
		return append([]broker.BrokerFillRecord(nil), a.fills[clientOrderID]...), nil
	}
	var out []broker.BrokerFillRecord
	for _, l := range a.fills {
		out = append(out, l...)
	}
	return out, nil
}

func (a *Adapter) ReplayEvents(_ context.Context, afterCursor int64) ([]domain.WebhookEvent, int64, error) {
	// Sandbox Alpha does not support cursor event replay; callers rely on webhooks + GetOrderByClientID reconciliation.
	return nil, afterCursor, nil
}

func (a *Adapter) HealthCheck(_ context.Context) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.authenticate()
}
