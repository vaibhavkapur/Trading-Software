// Package outbox is the dispatcher: it delivers queued broker calls (submit,
// cancel, replace) from the transactional outbox to the broker.
//
// Delivery is at-least-once. A worker can die at any point, including after
// the broker accepted a call but before that was recorded; the message's
// lease then runs out and another worker sends it again. That is safe because
// every call carries a stable id the broker deduplicates on.
package outbox

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"sync"
	"time"

	"oms/src/broker"
	"oms/src/domain"
	"oms/src/ids"
	"oms/src/metrics"
	"oms/src/service"
	"oms/src/store"
)

// Config tunes a worker. Zero values take the defaults noted on each field.
type Config struct {
	// WorkerID names this worker in leases. Default: a random id.
	WorkerID string
	// Batch is how many messages are leased at once. Default 32.
	Batch int
	// Concurrency is how many broker calls run in parallel. Default 8.
	Concurrency int
	// Lease is how long a worker owns a message. It must be longer than the
	// broker call timeout, or a slow call is sent twice. Default 30s.
	Lease time.Duration
	// PollInterval is how often an idle worker looks for work. Default 250ms.
	PollInterval time.Duration
	// MaxAttempts is how many times a message is tried before it is
	// dead-lettered for an operator. Default 8.
	MaxAttempts int
	// BaseBackoff and MaxBackoff shape the exponential retry delay.
	// Defaults 200ms and 30s.
	BaseBackoff time.Duration
	MaxBackoff  time.Duration
	// Jitter returns a factor applied to each retry delay so that retries from
	// many messages do not arrive together. Default: uniform in [0.8, 1.2).
	Jitter func() float64
}

func (c *Config) defaults() {
	if c.WorkerID == "" {
		c.WorkerID = ids.Random{}.New("worker")
	}
	if c.Batch <= 0 {
		c.Batch = 32
	}
	if c.Concurrency <= 0 {
		c.Concurrency = 8
	}
	if c.Lease <= 0 {
		c.Lease = 30 * time.Second
	}
	if c.PollInterval <= 0 {
		c.PollInterval = 250 * time.Millisecond
	}
	if c.MaxAttempts <= 0 {
		c.MaxAttempts = 8
	}
	if c.BaseBackoff <= 0 {
		c.BaseBackoff = 200 * time.Millisecond
	}
	if c.MaxBackoff <= 0 {
		c.MaxBackoff = 30 * time.Second
	}
	if c.Jitter == nil {
		c.Jitter = func() float64 { return 0.8 + 0.4*rand.Float64() }
	}
}

// Worker delivers outbox messages. Any number of workers may run against the
// same database.
type Worker struct {
	cfg     Config
	store   *store.Store
	broker  broker.Client
	ids     ids.Generator
	metrics *metrics.Set
	log     *slog.Logger
	nudge   chan struct{}
}

// New builds a worker.
func New(cfg Config, st *store.Store, br broker.Client, gen ids.Generator, m *metrics.Set, log *slog.Logger) *Worker {
	cfg.defaults()
	return &Worker{cfg: cfg, store: st, broker: br, ids: gen, metrics: m, log: log, nudge: make(chan struct{}, 1)}
}

// Nudge wakes an idle worker without waiting for its poll interval. It never
// blocks.
func (w *Worker) Nudge() {
	select {
	case w.nudge <- struct{}{}:
	default:
	}
}

// Run delivers messages until ctx is cancelled. Calls already in flight are
// allowed to finish and have their outcome recorded before Run returns.
func (w *Worker) Run(ctx context.Context) {
	for ctx.Err() == nil {
		n, err := w.RunOnce(ctx)
		if err != nil && ctx.Err() == nil {
			w.log.Error("outbox: claim failed", "err", err)
		}
		if n > 0 && err == nil {
			continue // there may be more
		}
		select {
		case <-ctx.Done():
		case <-w.nudge:
		case <-time.After(w.cfg.PollInterval):
		}
	}
}

// Drain delivers until nothing is due and returns how many messages it
// handled. Messages waiting out a retry delay are not due.
func (w *Worker) Drain(ctx context.Context) (int, error) {
	total := 0
	for {
		n, err := w.RunOnce(ctx)
		total += n
		if err != nil || n == 0 {
			return total, err
		}
	}
}

// RunOnce leases one batch of due messages, delivers them and returns how
// many it handled.
func (w *Worker) RunOnce(ctx context.Context) (int, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	// From here on the work is seen through even if shutdown begins. A claim
	// interrupted halfway could commit its leases with nobody left to work
	// them, and the outcome of a broker call must always be recorded.
	ctx = context.WithoutCancel(ctx)
	rows, err := w.store.ClaimOutbox(ctx, w.cfg.WorkerID, w.cfg.Batch, w.cfg.Lease)
	if err != nil {
		return 0, err
	}
	sem := make(chan struct{}, w.cfg.Concurrency)
	var wg sync.WaitGroup
	for _, row := range rows {
		wg.Add(1)
		sem <- struct{}{}
		go func() {
			defer wg.Done()
			defer func() { <-sem }()
			w.deliver(ctx, row)
		}()
	}
	wg.Wait()
	return len(rows), nil
}

// errLeaseLost aborts a transaction when another worker has taken over the
// message; that worker owns the outcome.
var errLeaseLost = errors.New("outbox: lease lost")

// deliver handles one leased message in three steps: record intent, call the
// broker, record the outcome. Each step is its own transaction so that the
// broker call is never made while holding a database lock.
func (w *Worker) deliver(ctx context.Context, row store.OutboxRow) {
	log := w.log.With("outbox_id", row.ID, "type", row.EventType, "order_id", row.AggregateID, "attempt", row.Attempts)

	// Step 1. Decide, under the order lock, whether the call is still needed,
	// and for a submission record SUBMITTING before anything is sent. If the
	// process dies after this the order shows that a submission may be in
	// flight, which is the truth.
	var order domain.Order
	var send bool
	err := w.store.Tx(ctx, func(tx *store.Tx) error {
		o, err := tx.LockOrder(ctx, row.AggregateID)
		if err != nil {
			return err
		}
		switch row.EventType {
		case store.OutboxSubmit:
			var drafts []domain.Draft
			drafts, send = domain.DecideSubmit(o, row.ID)
			if o, _, err = tx.Append(ctx, o.ID, o, drafts); err != nil {
				return err
			}
		case store.OutboxCancel:
			send = o.State.Live() && o.PendingCancel
		case store.OutboxReplace:
			send = o.State.Live() && o.PendingReplace != nil
		}
		order = o
		if send {
			return nil
		}
		return w.finish(ctx, tx, row, store.OutboxSkipped, fmt.Sprintf("not needed: order is %s", o.State))
	})
	switch {
	case errors.Is(err, errLeaseLost):
		return
	case err != nil:
		log.Error("outbox: could not prepare delivery", "err", err)
		w.retryLater(ctx, row, err)
		return
	case !send:
		w.metrics.OutboxDispatches.Inc(row.EventType, "skipped")
		return
	}

	// Step 2. Call the broker. requestID is stable across retries.
	requestID := fmt.Sprintf("outbox-%d", row.ID)
	var callErr error
	switch row.EventType {
	case store.OutboxSubmit:
		callErr = w.broker.Submit(ctx, broker.SubmitRequest{
			ClientOrderID: order.ID, Symbol: order.Symbol, Side: order.Side, Type: order.Type,
			TimeInForce: order.TimeInForce, Qty: order.Qty, LimitPrice: order.LimitPrice,
		})
	case store.OutboxCancel:
		callErr = w.broker.Cancel(ctx, order.ID, requestID)
	case store.OutboxReplace:
		callErr = w.broker.Replace(ctx, order.ID, requestID, broker.ReplaceRequest{
			Qty: order.PendingReplace.Qty, LimitPrice: order.PendingReplace.LimitPrice,
		})
	}

	// Step 3. Record the outcome.
	switch {
	case callErr == nil:
		var now time.Time
		err = w.store.Tx(ctx, func(tx *store.Tx) error {
			now = tx.Now
			return w.finish(ctx, tx, row, store.OutboxDelivered, "")
		})
		if err == nil {
			w.metrics.OutboxDispatches.Inc(row.EventType, "delivered")
			w.metrics.OutboxDelay.Observe(metrics.Seconds(now.Sub(row.CreatedAt)), row.EventType)
		}

	case broker.IsRetryable(callErr):
		// The broker may or may not have acted. Try again; it deduplicates.
		if row.Attempts >= w.cfg.MaxAttempts {
			err = w.deadLetter(ctx, row, callErr)
			if err == nil {
				w.metrics.OutboxDispatches.Inc(row.EventType, "dead")
				log.Error("outbox: giving up; issue raised for an operator", "err", callErr)
			}
		} else {
			log.Warn("outbox: delivery failed, will retry", "err", callErr)
			w.retryLater(ctx, row, callErr)
			return
		}

	default:
		// The broker understood the request and refused it.
		err = w.refused(ctx, row, callErr)
		if err == nil {
			w.metrics.OutboxDispatches.Inc(row.EventType, "rejected")
			log.Warn("outbox: broker refused the request", "err", callErr)
		}
	}
	if err != nil && !errors.Is(err, errLeaseLost) {
		// The outcome could not be recorded. The lease will run out and the
		// message will be sent again, which the broker deduplicates.
		log.Error("outbox: could not record delivery outcome", "err", err)
	}
}

func (w *Worker) finish(ctx context.Context, tx *store.Tx, row store.OutboxRow, status, lastError string) error {
	ok, err := tx.FinishOutbox(ctx, row.ID, w.cfg.WorkerID, status, lastError)
	if err == nil && !ok {
		return errLeaseLost
	}
	return err
}

// retryLater schedules another attempt after an exponential, jittered delay.
func (w *Worker) retryLater(ctx context.Context, row store.OutboxRow, cause error) {
	delay := w.Backoff(row.Attempts)
	err := w.store.Tx(ctx, func(tx *store.Tx) error {
		_, err := tx.RetryOutbox(ctx, row.ID, w.cfg.WorkerID, tx.Now.Add(delay), cause.Error())
		return err
	})
	if err != nil {
		w.log.Error("outbox: could not schedule retry; the lease will expire instead", "outbox_id", row.ID, "err", err)
	}
	w.metrics.OutboxDispatches.Inc(row.EventType, "retry")
}

// Backoff returns the delay before the attempt after the given one.
func (w *Worker) Backoff(attempt int) time.Duration {
	d := w.cfg.BaseBackoff
	for i := 1; i < attempt && d < w.cfg.MaxBackoff; i++ {
		d *= 2
	}
	if d > w.cfg.MaxBackoff {
		d = w.cfg.MaxBackoff
	}
	return time.Duration(float64(d) * w.cfg.Jitter())
}

// deadLetter stops retrying and raises an issue. The order is left exactly
// as it is: for a submission that means SUBMITTING, because whether the
// broker has the order is not known. An operator, or the next
// reconciliation run, settles it.
func (w *Worker) deadLetter(ctx context.Context, row store.OutboxRow, cause error) error {
	return w.store.Tx(ctx, func(tx *store.Tx) error {
		if err := w.finish(ctx, tx, row, store.OutboxDead, cause.Error()); err != nil {
			return err
		}
		_, _, err := tx.RaiseIssue(ctx, store.NewIssue{
			ID: w.ids.New("iss"), OrderID: row.AggregateID, Kind: store.KindOutboxDead, Severity: store.SeverityHigh,
			Fingerprint: fmt.Sprintf("%s:%d", store.KindOutboxDead, row.ID),
			Details: service.DeadLetter{OutboxID: row.ID, EventType: row.EventType,
				Attempts: row.Attempts, LastError: cause.Error()},
		})
		return err
	})
}

// refused records a permanent refusal by the broker as an event on the order.
func (w *Worker) refused(ctx context.Context, row store.OutboxRow, cause error) error {
	reason := cause.Error()
	var be *broker.Error
	if errors.As(cause, &be) && be.Code != "" {
		reason = be.Code + ": " + be.Message
	}
	return w.store.Tx(ctx, func(tx *store.Tx) error {
		o, err := tx.LockOrder(ctx, row.AggregateID)
		if err != nil {
			return err
		}
		if err := w.finish(ctx, tx, row, store.OutboxRejected, cause.Error()); err != nil {
			return err
		}
		var draft *domain.Draft
		switch {
		case row.EventType == store.OutboxSubmit && o.State == domain.StateSubmitting:
			draft = &domain.Draft{Type: domain.EvOrderRejected, Payload: domain.OrderRejected{By: domain.ByBroker, Reason: reason}}
		case row.EventType == store.OutboxCancel && o.State.Live() && o.PendingCancel:
			draft = &domain.Draft{Type: domain.EvCancelRejected, Payload: domain.CancelRejected{Reason: reason}}
		case row.EventType == store.OutboxReplace && o.State.Live() && o.PendingReplace != nil:
			draft = &domain.Draft{Type: domain.EvReplaceRejected, Payload: domain.ReplaceRejected{Reason: reason}}
		}
		if draft == nil {
			return nil // the order moved on while the call was in flight
		}
		draft.Source, draft.CausationID = domain.SourceOutbox, fmt.Sprintf("outbox:%d", row.ID)
		_, _, err = tx.Append(ctx, o.ID, o, []domain.Draft{*draft})
		return err
	})
}
