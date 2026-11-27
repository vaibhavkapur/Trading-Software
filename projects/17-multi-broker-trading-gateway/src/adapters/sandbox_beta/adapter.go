package sandbox_beta

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
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

// Adapter implements Approved Sandbox Adapter #2 with distinct async queue & cursor-replay semantics:
// - HMAC-SHA256 request signing using SecretManager ("HMAC" scheme)
// - Symbol convention: "<TICKER>.US" for equities/ETFs, "<BASE>-USD" for crypto
// - Supports EQUITY, ETF, CRYPTO; MARKET, LIMIT, STOP_LIMIT; fractional quantities
// - Async queued acknowledgement (PENDING_NEW -> settle via ProcessQueue / immediate mode)
// - Async cancel semantics that can return CANCEL_PENDING or CANCEL_TIMEOUT
// - Cursor-based event replay log supporting out-of-order fill delivery and crash recovery
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
	eventLog           []domain.WebhookEvent
	pendingQueue       []string
	orderSeq           int64
	fillSeq            int64
	dispatchCnt        atomic.Int64
	asyncQueueMode     bool
	forceUnknownSubmit bool
	forceCancelTimeout string // "", "cancelled", "filled", "stuck"
}

// New constructs Sandbox Beta adapter.
func New(caps domain.BrokerCapabilities, sm security.SecretManager, secretRef string, ratePerSec float64, burst int, clock ratelimit.Clock) *Adapter {
	if clock == nil {
		clock = ratelimit.RealClock()
	}
	if caps.BrokerID == "" {
		caps.BrokerID = "sandbox-beta"
	}
	return &Adapter{
		brokerID:      caps.BrokerID,
		caps:          caps,
		secrets:       sm,
		secretRef:     secretRef,
		clock:         clock,
		serverLimiter: ratelimit.NewTokenBucket(caps.BrokerID+"-server", ratePerSec, burst, clock),
		refPrices: map[string]float64{
			"AAPL.US": 195.10,
			"MSFT.US": 412.20,
			"SPY.US":  560.05,
			"BTC-USD": 91500.00,
		},
		orders:     make(map[string]*broker.BrokerOrderSnapshot),
		byBrokerID: make(map[string]string),
		fills:      make(map[string][]broker.BrokerFillRecord),
	}
}

func (a *Adapter) BrokerID() string                        { return a.brokerID }
func (a *Adapter) Capabilities() domain.BrokerCapabilities { return a.caps }
func (a *Adapter) DispatchCount() int64                    { return a.dispatchCnt.Load() }

// SetAsyncQueueMode controls whether orders initially ack as PENDING_NEW and settle when ProcessQueue runs.
func (a *Adapter) SetAsyncQueueMode(enabled bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.asyncQueueMode = enabled
}

// InjectUnknownSubmitOnce simulates a broker submit that succeeds server-side but drops the response.
func (a *Adapter) InjectUnknownSubmitOnce() {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.forceUnknownSubmit = true
}

// InjectCancelTimeoutOnce causes the next cancel to return ErrCodeCancelTimeout while settling broker-side to outcome ("cancelled", "filled", or "stuck").
func (a *Adapter) InjectCancelTimeoutOnce(outcome string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.forceCancelTimeout = outcome
}

// signRequest computes an HMAC signature without exposing the key anywhere in logs or errors.
func (a *Adapter) authenticateAndSign(payload string) (string, error) {
	if a.secrets == nil {
		return "", domain.NewError(domain.ErrCodeAuthInvalid, "sandbox-beta secret manager missing", false).WithContext(a.brokerID, "")
	}
	cred, err := a.secrets.GetCredential(a.secretRef)
	if err != nil {
		return "", domain.NewError(domain.ErrCodeAuthInvalid, "sandbox-beta hmac secret unavailable", false).WithContext(a.brokerID, "")
	}
	if !cred.IsValidAt(a.clock.Now()) {
		return "", domain.NewError(domain.ErrCodeAuthExpired, "sandbox-beta hmac credential expired or revoked", false).WithContext(a.brokerID, "")
	}
	mac := hmac.New(sha256.New, []byte(cred.Secret.Expose()))
	_, _ = mac.Write([]byte(payload))
	return hex.EncodeToString(mac.Sum(nil)), nil
}

func (a *Adapter) SubmitOrder(_ context.Context, req broker.BrokerOrderSubmit) (broker.BrokerSubmitAck, error) {
	a.dispatchCnt.Add(1)
	a.mu.Lock()
	defer a.mu.Unlock()

	if _, err := a.authenticateAndSign(req.ClientOrderID + ":" + req.BrokerSymbol); err != nil {
		return broker.BrokerSubmitAck{}, err
	}
	if err := a.serverLimiter.Allow(); err != nil {
		return broker.BrokerSubmitAck{}, err
	}
	if !strings.HasSuffix(req.BrokerSymbol, ".US") && !strings.HasSuffix(req.BrokerSymbol, "-USD") {
		return broker.BrokerSubmitAck{}, domain.NewError(domain.ErrCodeSymbolMismatch, fmt.Sprintf("sandbox-beta invalid symbol convention %q", req.BrokerSymbol), false).
			WithContext(a.brokerID, req.OrderID)
	}
	if _, known := a.refPrices[req.BrokerSymbol]; !known {
		return broker.BrokerSubmitAck{}, domain.NewError(domain.ErrCodeSymbolMismatch, fmt.Sprintf("symbol %q not listed on sandbox-beta", req.BrokerSymbol), false).
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
	boid := fmt.Sprintf("beta-ord-%06d", a.orderSeq)
	now := a.clock.Now()
	snap := &broker.BrokerOrderSnapshot{
		BrokerOrderID: boid,
		ClientOrderID: req.ClientOrderID,
		BrokerSymbol:  req.BrokerSymbol,
		Side:          req.Side,
		OrderType:     req.OrderType,
		Quantity:      req.Quantity,
		Status:        domain.StatusPendingNew,
		UpdatedAtUTC:  now,
	}
	a.orders[req.ClientOrderID] = snap
	a.byBrokerID[boid] = req.ClientOrderID

	var fills []broker.BrokerFillRecord
	if a.asyncQueueMode {
		a.pendingQueue = append(a.pendingQueue, req.ClientOrderID)
	} else {
		fills = a.settleOrderLocked(snap, req.LimitPrice, req.Currency, now)
	}

	if a.forceUnknownSubmit {
		a.forceUnknownSubmit = false
		return broker.BrokerSubmitAck{}, domain.NewError(domain.ErrCodeUnknownOutcome, "sandbox-beta websocket/REST ack lost after queuing", false).
			WithContext(a.brokerID, req.OrderID)
	}

	return broker.BrokerSubmitAck{
		BrokerOrderID: boid,
		Status:        snap.Status,
		Fills:         fills,
		AckTimeUTC:    now,
	}, nil
}

// ProcessQueue deterministically advances queued PENDING_NEW orders to ACCEPTED or FILLED and appends replayable events.
func (a *Adapter) ProcessQueue() []domain.WebhookEvent {
	a.mu.Lock()
	defer a.mu.Unlock()
	now := a.clock.Now()
	startIdx := len(a.eventLog)
	for _, cid := range a.pendingQueue {
		snap, ok := a.orders[cid]
		if !ok || snap.Status.IsTerminal() {
			continue
		}
		a.settleOrderLocked(snap, 0, "USD", now)
	}
	a.pendingQueue = nil
	return append([]domain.WebhookEvent(nil), a.eventLog[startIdx:]...)
}

func (a *Adapter) settleOrderLocked(snap *broker.BrokerOrderSnapshot, limitPrice float64, currency string, now time.Time) []broker.BrokerFillRecord {
	refPx := a.refPrices[snap.BrokerSymbol]
	marketable := snap.OrderType == domain.OrderTypeMarket ||
		(snap.OrderType == domain.OrderTypeLimit && limitPrice > 0 && ((snap.Side == domain.SideBuy && limitPrice >= refPx) || (snap.Side == domain.SideSell && limitPrice <= refPx)))

	if !marketable {
		snap.Status = domain.StatusAccepted
		snap.UpdatedAtUTC = now
		a.eventLog = append(a.eventLog, domain.WebhookEvent{
			EventID:       fmt.Sprintf("beta-ev-acc-%d", len(a.eventLog)+1),
			BrokerID:      a.brokerID,
			EventType:     "STATUS",
			BrokerOrderID: snap.BrokerOrderID,
			ClientOrderID: snap.ClientOrderID,
			Status:        domain.StatusAccepted,
			Sequence:      0,
			EventTimeUTC:  now,
		})
		return nil
	}

	execPx := refPx
	if limitPrice > 0 {
		execPx = limitPrice
	}
	// Sandbox Beta splits orders >= 2 units into 2 fills to exercise multi-fill normalization & replay.
	var tranches []float64
	if snap.Quantity >= 2 {
		q1 := math.Round(snap.Quantity*0.3*1e6) / 1e6
		q2 := math.Round((snap.Quantity-q1)*1e6) / 1e6
		tranches = []float64{q1, q2}
	} else {
		tranches = []float64{snap.Quantity}
	}

	var out []broker.BrokerFillRecord
	var totQty, totVal float64
	for idx, q := range tranches {
		a.fillSeq++
		px := math.Round((execPx+float64(idx)*0.10)*100) / 100
		f := broker.BrokerFillRecord{
			EventID:       fmt.Sprintf("beta-ev-fill-%06d", a.fillSeq),
			BrokerOrderID: snap.BrokerOrderID,
			ClientOrderID: snap.ClientOrderID,
			BrokerFillID:  fmt.Sprintf("beta-fill-%06d", a.fillSeq),
			BrokerSymbol:  snap.BrokerSymbol,
			Side:          snap.Side,
			Sequence:      int64(idx + 1),
			Quantity:      q,
			Price:         px,
			Fee:           math.Round(q*px*0.0002*10000) / 10000,
			Currency:      currency,
			ExecutedAtUTC: now.Add(time.Duration(idx) * time.Second),
		}
		a.fills[snap.ClientOrderID] = append(a.fills[snap.ClientOrderID], f)
		out = append(out, f)
		totQty += q
		totVal += q * px
		a.eventLog = append(a.eventLog, domain.WebhookEvent{
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
	}
	snap.FilledQuantity = math.Round(totQty*1e8) / 1e8
	snap.AvgFillPrice = math.Round((totVal/totQty)*1e8) / 1e8
	snap.Status = domain.StatusFilled
	snap.UpdatedAtUTC = now
	return out
}

func (a *Adapter) CancelOrder(_ context.Context, brokerOrderID, clientOrderID string) (broker.BrokerCancelAck, error) {
	a.dispatchCnt.Add(1)
	a.mu.Lock()
	defer a.mu.Unlock()

	if _, err := a.authenticateAndSign("cancel:" + clientOrderID); err != nil {
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
		return broker.BrokerCancelAck{}, domain.NewError(domain.ErrCodeOrderNotFound, "order not found at sandbox-beta", false).
			WithContext(a.brokerID, brokerOrderID)
	}

	now := a.clock.Now()
	if a.forceCancelTimeout != "" {
		mode := a.forceCancelTimeout
		a.forceCancelTimeout = ""
		switch mode {
		case "cancelled":
			snap.Status = domain.StatusCancelled
			snap.UpdatedAtUTC = now
			a.eventLog = append(a.eventLog, domain.WebhookEvent{
				EventID:       fmt.Sprintf("beta-ev-cxl-%d", len(a.eventLog)+1),
				BrokerID:      a.brokerID,
				EventType:     "CANCELLED",
				BrokerOrderID: snap.BrokerOrderID,
				ClientOrderID: snap.ClientOrderID,
				Status:        domain.StatusCancelled,
				EventTimeUTC:  now,
			})
		case "filled":
			a.settleOrderLocked(snap, a.refPrices[snap.BrokerSymbol], "USD", now)
		default:
			snap.Status = domain.StatusCancelPending
			snap.UpdatedAtUTC = now
		}
		return broker.BrokerCancelAck{}, domain.NewError(domain.ErrCodeCancelTimeout, "sandbox-beta cancel timed out awaiting venue confirmation", false).
			WithContext(a.brokerID, snap.BrokerOrderID)
	}

	if !snap.Status.IsTerminal() {
		snap.Status = domain.StatusCancelled
		snap.UpdatedAtUTC = now
		a.eventLog = append(a.eventLog, domain.WebhookEvent{
			EventID:       fmt.Sprintf("beta-ev-cxl-%d", len(a.eventLog)+1),
			BrokerID:      a.brokerID,
			EventType:     "CANCELLED",
			BrokerOrderID: snap.BrokerOrderID,
			ClientOrderID: snap.ClientOrderID,
			Status:        domain.StatusCancelled,
			EventTimeUTC:  now,
		})
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
	if _, err := a.authenticateAndSign("get:" + clientOrderID); err != nil {
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
	if _, err := a.authenticateAndSign("list-orders"); err != nil {
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
	if _, err := a.authenticateAndSign("list-fills"); err != nil {
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
	a.mu.Lock()
	defer a.mu.Unlock()
	if _, err := a.authenticateAndSign("replay"); err != nil {
		return nil, afterCursor, err
	}
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
	_, err := a.authenticateAndSign("health")
	return err
}
