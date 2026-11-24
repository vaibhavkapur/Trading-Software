// Package notify pushes kill-switch changes to the OMS (and any other
// execution entry point) over webhooks, retrying until each subscriber
// acknowledges, and measures how long propagation took.
//
// Push is a latency optimisation, not the enforcement mechanism: the engine
// itself refuses evaluate, approve and commit while a switch is on, so an
// OMS that never hears the webhook still cannot get an order through.
package notify

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"sync"
	"time"

	"riskengine/src/domain"
	"riskengine/src/metrics"
)

type Message struct {
	KillSwitch domain.KillSwitch `json:"kill_switch"`
	domain.Seal
}

type Delivery struct {
	URL       string     `json:"url"`
	Switch    string     `json:"switch"`
	Enabled   bool       `json:"enabled"`
	Attempts  int        `json:"attempts"`
	AckedAt   *time.Time `json:"acked_at,omitempty"`
	DelayMs   float64    `json:"propagation_delay_ms,omitempty"`
	LastError string     `json:"last_error,omitempty"`
}

type Notifier struct {
	ctx    context.Context
	urls   []string
	client *http.Client
	met    *metrics.Registry

	mu   sync.Mutex
	gen  map[string]uint64 // newest publication per url+switch; older retries stop
	stat map[string]*Delivery
	wg   sync.WaitGroup
}

func New(ctx context.Context, urls []string, met *metrics.Registry) *Notifier {
	return &Notifier{ctx: ctx, urls: urls, met: met, client: &http.Client{Timeout: 2 * time.Second},
		gen: map[string]uint64{}, stat: map[string]*Delivery{}}
}

// Publish starts delivery to every subscriber and returns immediately.
func (n *Notifier) Publish(ks domain.KillSwitch, seal domain.Seal) {
	body, _ := json.Marshal(Message{KillSwitch: ks, Seal: seal})
	start := time.Now()
	for _, url := range n.urls {
		id := url + "|" + ks.Key()
		n.mu.Lock()
		n.gen[id]++
		gen := n.gen[id]
		d := &Delivery{URL: url, Switch: ks.Key(), Enabled: ks.Enabled}
		n.stat[id] = d
		n.mu.Unlock()
		n.wg.Add(1)
		go n.deliver(id, gen, url, body, d, start)
	}
}

func (n *Notifier) deliver(id string, gen uint64, url string, body []byte, d *Delivery, start time.Time) {
	defer n.wg.Done()
	backoff := 50 * time.Millisecond
	for {
		err := n.post(url, body)
		n.mu.Lock()
		if n.gen[id] != gen { // superseded by a newer change to the same switch
			n.mu.Unlock()
			return
		}
		d.Attempts++
		if err == nil {
			now := time.Now().UTC()
			delay := now.Sub(start)
			d.AckedAt, d.DelayMs, d.LastError = &now, float64(delay.Microseconds())/1000, ""
			n.mu.Unlock()
			n.met.Observe("risk_kill_switch_propagation_seconds", delay.Seconds())
			return
		}
		d.LastError = err.Error()
		n.mu.Unlock()
		n.met.Inc("risk_kill_switch_propagation_failures_total")
		select {
		case <-n.ctx.Done():
			return
		case <-time.After(backoff):
		}
		backoff = min(backoff*2, 2*time.Second)
	}
}

func (n *Notifier) post(url string, body []byte) error {
	req, err := http.NewRequestWithContext(n.ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := n.client.Do(req)
	if err != nil {
		return err
	}
	resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("subscriber answered %s", resp.Status)
	}
	return nil
}

// Status reports the latest delivery per subscriber and switch.
func (n *Notifier) Status() []Delivery {
	n.mu.Lock()
	defer n.mu.Unlock()
	out := make([]Delivery, 0, len(n.stat))
	for _, d := range n.stat {
		out = append(out, *d)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].URL+out[i].Switch < out[j].URL+out[j].Switch })
	return out
}

// Wait blocks until in-flight deliveries finish or the context is cancelled.
func (n *Notifier) Wait() { n.wg.Wait() }
