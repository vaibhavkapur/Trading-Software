package brokersim

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"math/rand/v2"
	"net/http"
	"time"

	"oms/src/broker"
)

// Delivery is the result of sending one webhook.
type Delivery struct {
	EventID string
	Type    string
	// Status is the HTTP status the receiver answered, or 0 if the request
	// failed before an answer.
	Status int
	Body   string
	Err    error
}

// OK reports whether the receiver accepted the webhook.
func (d Delivery) OK() bool { return d.Err == nil && d.Status >= 200 && d.Status < 300 }

var webhookClient = &http.Client{Timeout: 10 * time.Second}

// send posts one signed webhook, plus any configured duplicates, and returns
// the result of the first copy.
func (s *Sim) send(ctx context.Context, e Event) Delivery {
	s.mu.Lock()
	extra := s.faults.DuplicateWebhooks
	s.mu.Unlock()
	first := s.post(ctx, e, s.cfg.WebhookSecret)
	for range extra {
		s.post(ctx, e, s.cfg.WebhookSecret)
	}
	return first
}

func (s *Sim) post(ctx context.Context, e Event, secret string) Delivery {
	d := Delivery{EventID: e.ID, Type: e.Type}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.cfg.WebhookURL, bytes.NewReader(e.Body))
	if err != nil {
		d.Err = err
		return d
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(broker.SignatureHeader, broker.Sign(secret, e.Body))
	resp, err := webhookClient.Do(req)
	s.mu.Lock()
	s.stats.WebhooksSent++
	if err != nil || resp.StatusCode/100 != 2 {
		s.stats.WebhooksFailed++
	}
	s.mu.Unlock()
	if err != nil {
		d.Err = err
		return d
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
	d.Status, d.Body = resp.StatusCode, string(body)
	return d
}

// deliveryLoop sends queued webhooks as they appear (AutoDeliver mode). A
// webhook the receiver does not accept is retried with exponential backoff
// and, after WebhookAttempts tries, given up on; reconciliation is what
// recovers a report lost that way.
func (s *Sim) deliveryLoop() {
	defer s.workers.Done()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		<-s.stop
		cancel()
	}()
	for {
		e, ok := s.next()
		if !ok {
			select {
			case <-s.stop:
				return
			case <-s.wake:
				continue
			}
		}
		backoff := s.cfg.WebhookBackoff
		for attempt := 1; ; attempt++ {
			d := s.send(ctx, e)
			// 4xx other than 408/429 means the receiver will never take it.
			permanent := d.Status >= 400 && d.Status < 500 && d.Status != 408 && d.Status != 429
			if d.OK() || permanent || attempt >= s.cfg.WebhookAttempts {
				if !d.OK() {
					s.mu.Lock()
					s.stats.WebhooksLost++
					s.mu.Unlock()
					s.cfg.Log.Warn("brokersim: webhook abandoned", "event_id", e.ID, "status", d.Status, "err", d.Err)
				}
				break
			}
			select {
			case <-s.stop:
				return
			case <-time.After(backoff):
			}
			backoff *= 2
		}
	}
}

// next pops the oldest queued webhook and re-signals other senders if more
// remain.
func (s *Sim) next() (Event, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.queue) == 0 {
		return Event{}, false
	}
	e := s.events[s.queue[0]]
	s.queue = s.queue[1:]
	if len(s.queue) > 0 {
		select {
		case s.wake <- struct{}{}:
		default:
		}
	}
	return e, true
}

// --- Manual delivery, for tests. ---

// Pending returns the webhooks waiting to be sent, oldest first.
func (s *Sim) Pending() []Event {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Event, 0, len(s.queue))
	for _, i := range s.queue {
		out = append(out, s.events[i])
	}
	return out
}

// Flush sends every waiting webhook once, oldest first. A webhook the
// receiver does not accept goes back on the queue, as a broker that retries
// would keep it.
func (s *Sim) Flush(ctx context.Context) []Delivery {
	return s.flush(ctx, func(q []int) {})
}

// FlushReversed sends the waiting webhooks newest first.
func (s *Sim) FlushReversed(ctx context.Context) []Delivery {
	return s.flush(ctx, func(q []int) {
		for i, j := 0, len(q)-1; i < j; i, j = i+1, j-1 {
			q[i], q[j] = q[j], q[i]
		}
	})
}

// FlushShuffled sends the waiting webhooks in an order determined by seed.
func (s *Sim) FlushShuffled(ctx context.Context, seed uint64) []Delivery {
	return s.flush(ctx, func(q []int) {
		r := rand.New(rand.NewPCG(seed, seed^0x9e3779b97f4a7c15))
		r.Shuffle(len(q), func(i, j int) { q[i], q[j] = q[j], q[i] })
	})
}

func (s *Sim) flush(ctx context.Context, reorder func([]int)) []Delivery {
	s.mu.Lock()
	q := s.queue
	s.queue = nil
	events := make([]Event, 0, len(q))
	reorder(q)
	for _, i := range q {
		events = append(events, s.events[i])
	}
	s.mu.Unlock()

	out := make([]Delivery, 0, len(events))
	var retry []int
	for n, e := range events {
		d := s.send(ctx, e)
		out = append(out, d)
		if !d.OK() {
			retry = append(retry, q[n])
		}
	}
	if len(retry) > 0 {
		s.mu.Lock()
		s.queue = append(retry, s.queue...)
		s.mu.Unlock()
	}
	return out
}

// DropPending discards every waiting webhook, as if the broker lost them,
// and returns how many there were.
func (s *Sim) DropPending() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := len(s.queue)
	s.queue = nil
	s.stats.WebhooksLost += n
	return n
}

// Redeliver sends an already produced webhook again, with the same event id
// and body.
func (s *Sim) Redeliver(ctx context.Context, eventID string) (Delivery, error) {
	e, ok := s.event(eventID)
	if !ok {
		return Delivery{}, fmt.Errorf("%w: event %s", ErrUnknown, eventID)
	}
	return s.post(ctx, e, s.cfg.WebhookSecret), nil
}

// RedeliverAll sends every webhook produced so far again, oldest first.
func (s *Sim) RedeliverAll(ctx context.Context) []Delivery {
	events := s.Events()
	out := make([]Delivery, 0, len(events))
	for _, e := range events {
		out = append(out, s.post(ctx, e, s.cfg.WebhookSecret))
	}
	return out
}

// SendRaw posts an arbitrary body to the webhook receiver, signed with the
// given secret. Tests use it for malformed bodies and bad signatures.
func (s *Sim) SendRaw(ctx context.Context, body []byte, secret string) Delivery {
	return s.post(ctx, Event{ID: "raw", Type: "raw", Body: body}, secret)
}

func (s *Sim) event(id string) (Event, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, e := range s.events {
		if e.ID == id {
			return e, true
		}
	}
	return Event{}, false
}
