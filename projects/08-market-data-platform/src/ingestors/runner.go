package ingestors

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"math/rand/v2"
	"sort"
	"sync"
	"time"

	"mdp/src/domain"
	"mdp/src/metrics"
	"mdp/src/seqtrack"
	"mdp/src/stream"
)

// Config tunes a Runner. Zero values take the defaults in withDefaults.
type Config struct {
	Partitions      int32         // partitions of the raw topic
	MaxBatch        int           // events per produce call
	Linger          time.Duration // longest an event waits for its batch to fill
	Buffer          int           // frames buffered between socket and publisher
	IdleTimeout     time.Duration // a live connection silent this long is redialed
	DialTimeout     time.Duration
	BackoffMin      time.Duration
	BackoffMax      time.Duration
	CheckpointEvery time.Duration
	HealthEvery     time.Duration
	Now             func() time.Time
	Capture         *CaptureWriter // optional: record every frame read
	// SeqStart is the first sequence number of a provider session, or 0 if
	// unknown. With it, a new session joined after its first number is
	// treated like any other gap: the provider is asked to resend.
	SeqStart uint64
}

func (c Config) withDefaults() Config {
	def := func(d *time.Duration, v time.Duration) {
		if *d <= 0 {
			*d = v
		}
	}
	if c.Partitions <= 0 {
		c.Partitions = 1
	}
	if c.MaxBatch <= 0 {
		c.MaxBatch = 500
	}
	if c.Buffer <= 0 {
		c.Buffer = 2048
	}
	def(&c.Linger, 5*time.Millisecond)
	def(&c.IdleTimeout, 5*time.Second)
	def(&c.DialTimeout, 5*time.Second)
	def(&c.BackoffMin, 100*time.Millisecond)
	def(&c.BackoffMax, 5*time.Second)
	def(&c.CheckpointEvery, time.Second)
	def(&c.HealthEvery, time.Second)
	if c.Now == nil {
		c.Now = time.Now
	}
	return c
}

// Runner drives one feed adapter: it keeps the connection up, publishes
// parsed events to the raw topic at least once, and checkpoints how far it
// has published so a restart resumes instead of starting over.
//
// Delivery contract. An event is accepted only if it advances its sequence
// domain. Accepted events are published before the checkpoint that covers
// them is written, so a crash can republish events (the normalizer drops
// them by sequence number) but can never skip one.
//
// Gap recovery. If a live provider skips sequence numbers mid-stream the
// runner does not publish past the hole. It reconnects and asks the provider
// to resend from the last number it has. Only if the provider cannot supply
// the missing range is the gap accepted and left for the normalizer to
// declare.
type Runner struct {
	adapter Adapter
	bus     stream.Bus
	ckpt    *stream.Checkpoints
	m       *metrics.Set
	cfg     Config
	log     *slog.Logger

	tracker    *seqtrack.Tracker         // accepted into a batch
	durable    map[string]seqtrack.State // acknowledged by the log; what resume and checkpoints use
	resyncedAt map[string]uint64         // domain → first missing seq of the gap last resynced for
	rejoin     map[string]seqtrack.State // domain → position to resume a newly seen session from
	dirty      bool                      // durable has advanced since the last checkpoint
	lastCkpt   time.Time
	connected  int // connections established so far

	mu     sync.Mutex
	health domain.IngestHealth
}

var errResync = errors.New("resync")

func NewRunner(a Adapter, bus stream.Bus, m *metrics.Set, cfg Config, log *slog.Logger) *Runner {
	if log == nil {
		log = slog.Default()
	}
	return &Runner{
		adapter: a, bus: bus, ckpt: stream.NewCheckpoints(bus), m: m, cfg: cfg.withDefaults(),
		log:        log.With("component", "ingestor", "provider", a.Provider(), "source", a.Source()),
		tracker:    seqtrack.New(),
		durable:    map[string]seqtrack.State{},
		resyncedAt: map[string]uint64{},
		rejoin:     map[string]seqtrack.State{},
		health:     domain.IngestHealth{Provider: a.Provider(), Source: a.Source()},
	}
}

func (r *Runner) checkpointKey() string {
	return "feed/" + r.adapter.Provider() + "/" + r.adapter.Source()
}

// Health returns the runner's current health snapshot.
func (r *Runner) Health() domain.IngestHealth {
	r.mu.Lock()
	defer r.mu.Unlock()
	h := r.health
	h.UpdatedAt = r.cfg.Now().UTC()
	return h
}

// offsets returns the FeedOffset of every sequence domain, counting only
// events the log has acknowledged.
func (r *Runner) offsets() []domain.FeedOffset {
	out := make([]domain.FeedOffset, 0, len(r.durable))
	for d, st := range r.durable {
		out = append(out, domain.FeedOffset{
			Provider: r.adapter.Provider(), Partition: d, Seq: st.Last, ResumeToken: Token(st.Session, st.Last),
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Partition < out[j].Partition })
	return out
}

// Run ingests until ctx ends, or until EOF for a non-live adapter.
func (r *Runner) Run(ctx context.Context) error {
	if err := r.restore(ctx); err != nil {
		return err
	}
	hctx, stopHealth := context.WithCancel(ctx)
	var wg sync.WaitGroup
	wg.Add(1)
	go func() { defer wg.Done(); r.healthLoop(hctx) }()
	defer func() {
		stopHealth()
		wg.Wait()
		grace, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Second)
		defer cancel()
		r.checkpoint(grace, true)
		r.publishHealth(grace)
	}()

	backoff := r.cfg.BackoffMin
	for {
		progress, err := r.session(ctx)
		if ctx.Err() != nil {
			return nil
		}
		switch {
		case errors.Is(err, io.EOF) && !r.adapter.Live():
			return nil
		case errors.Is(err, errResync):
			r.m.IngestResyncs.WithLabelValues(r.adapter.Provider()).Inc()
			r.mu.Lock()
			r.health.Resyncs++
			r.mu.Unlock()
			continue
		}
		if progress {
			backoff = r.cfg.BackoffMin
		}
		r.log.Warn("connection lost; reconnecting", "err", err, "backoff", backoff)
		// Full jitter keeps a fleet of ingestors from reconnecting in step.
		sleep := time.Duration(rand.Int64N(int64(backoff)) + 1)
		select {
		case <-time.After(sleep):
		case <-ctx.Done():
			return nil
		}
		if backoff *= 2; backoff > r.cfg.BackoffMax {
			backoff = r.cfg.BackoffMax
		}
	}
}

func (r *Runner) restore(ctx context.Context) error {
	all, err := r.ckpt.Load(ctx)
	if err != nil {
		return err
	}
	raw, ok := all[r.checkpointKey()]
	if !ok {
		return nil
	}
	var offs []domain.FeedOffset
	if err := json.Unmarshal(raw, &offs); err != nil {
		return err
	}
	for _, off := range offs {
		session, seq, err := ParseToken(off.ResumeToken)
		if err != nil {
			return err
		}
		st := seqtrack.State{Session: session, Last: seq}
		r.tracker.Set(off.Partition, st)
		r.durable[off.Partition] = st
	}
	r.log.Info("restored feed offsets", "domains", len(offs))
	return nil
}

type batch struct {
	recs   []stream.Record
	events []Event
}

// session runs one connection. progress reports whether any frame arrived.
func (r *Runner) session(ctx context.Context) (progress bool, err error) {
	dctx, cancel := context.WithTimeout(ctx, r.cfg.DialTimeout)
	conn, err := r.adapter.Dial(dctx, r.offsets())
	cancel()
	if err != nil {
		return false, err
	}
	provider := r.adapter.Provider()
	if r.connected++; r.connected > 1 {
		r.m.IngestReconnects.WithLabelValues(provider).Inc()
	}
	r.setConnected(true)
	sctx, stop := context.WithCancel(ctx)
	frames := make(chan Frame, r.cfg.Buffer)
	readErr := make(chan error, 1)
	readerDone := make(chan struct{})
	go func() {
		defer close(readerDone)
		for {
			rctx, rcancel := sctx, context.CancelFunc(func() {})
			if r.adapter.Live() {
				rctx, rcancel = context.WithTimeout(sctx, r.cfg.IdleTimeout)
			}
			f, err := conn.ReadFrame(rctx)
			rcancel()
			if err != nil {
				readErr <- err
				return
			}
			if f.Recv.IsZero() {
				f.Recv = r.cfg.Now().UTC()
			}
			// A full buffer blocks here, which stops reading the socket:
			// backpressure reaches the provider instead of dropping frames.
			select {
			case frames <- f:
			case <-sctx.Done():
				readErr <- sctx.Err()
				return
			}
		}
	}()
	defer func() {
		stop()
		conn.Close()
		<-readerDone
		r.setConnected(false)
	}()

	parser := r.adapter.NewParser()
	var b batch
	// finish publishes what is batched, even during shutdown.
	finish := func() {
		fctx := ctx
		if ctx.Err() != nil {
			var c context.CancelFunc
			fctx, c = context.WithTimeout(context.WithoutCancel(ctx), 2*time.Second)
			defer c()
		}
		r.flush(fctx, &b)
		// Only now, after the old session's last events are acknowledged,
		// may the resume position move to the start of a new session.
		for d, st := range r.rejoin {
			r.durable[d] = st
			delete(r.rejoin, d)
		}
	}
	ticker := time.NewTicker(r.cfg.Linger)
	defer ticker.Stop()
	for {
		select {
		case f := <-frames:
			progress = true
			if r.handle(parser, f, &b) {
				finish()
				return progress, errResync
			}
			if len(b.recs) >= r.cfg.MaxBatch {
				if err := r.flush(ctx, &b); err != nil {
					return progress, err
				}
			}
		case <-ticker.C:
			if err := r.flush(ctx, &b); err != nil {
				return progress, err
			}
			r.checkpoint(ctx, false)
		case err := <-readErr:
			// Frames read before the error are still in the buffer.
			for drained := false; !drained; {
				select {
				case f := <-frames:
					progress = true
					if r.handle(parser, f, &b) {
						finish()
						return progress, errResync
					}
				default:
					drained = true
				}
			}
			finish()
			return progress, err
		case <-ctx.Done():
			finish()
			return progress, ctx.Err()
		}
	}
}

// handle parses one frame into the batch. It returns true when the frame
// revealed a gap the provider should be asked to resend.
func (r *Runner) handle(parser Parser, f Frame, b *batch) (resync bool) {
	provider := r.adapter.Provider()
	r.m.IngestBytes.WithLabelValues(provider).Add(float64(len(f.Data)))
	if r.cfg.Capture != nil {
		if err := r.cfg.Capture.Write(provider, f.Recv, f.Data); err != nil {
			r.log.Warn("capture write failed", "err", err)
		}
	}
	events, err := parser.Parse(f.Data)
	if err != nil {
		r.m.IngestParseErrors.WithLabelValues(provider).Inc()
		r.mu.Lock()
		r.health.ParseErrors++
		r.mu.Unlock()
		raw := f.Data
		if len(raw) > 512 {
			raw = raw[:512]
		}
		rej := domain.Reject{Stage: "ingest", Reason: "UNPARSEABLE_FRAME", Detail: err.Error(),
			Provider: provider, Raw: string(raw), ReceiveTime: f.Recv}
		b.recs = append(b.recs, stream.Record{Topic: stream.TopicRejects, Key: []byte(provider), Value: domain.Encode(rej)})
		return false
	}
	for _, ev := range events {
		ev.RecvTime = f.Recv
		ev.Source = r.adapter.Source()
		res := r.tracker.Classify(ev.Domain, ev.Session, ev.Seq)
		switch res.Verdict {
		case seqtrack.Duplicate:
			r.m.IngestDuplicates.WithLabelValues(provider).Inc()
			r.mu.Lock()
			r.health.Duplicates++
			r.mu.Unlock()
			continue
		case seqtrack.Gap:
			// Ask for a resend once per gap. If the same hole is still there
			// afterwards the provider cannot fill it, and it is accepted.
			if r.adapter.Live() && r.resyncedAt[ev.Domain] != res.MissingFrom {
				r.resyncedAt[ev.Domain] = res.MissingFrom
				r.adoptUnseen(ev.Session)
				r.log.Info("sequence gap; asking provider to resend", "domain", ev.Domain,
					"from", res.MissingFrom, "to", res.MissingTo)
				return true
			}
			r.log.Warn("unrecoverable sequence gap", "domain", ev.Domain, "from", res.MissingFrom, "to", res.MissingTo)
			r.mu.Lock()
			r.health.Gaps++
			r.mu.Unlock()
		case seqtrack.Reset:
			delete(r.resyncedAt, ev.Domain)
			// The session changed and its first numbers went by while we
			// were away. Ask for the session from its start, once. A
			// session covers the whole feed, so every domain still on the
			// old session is moved along: one resend recovers them all.
			if start := r.cfg.SeqStart; r.adapter.Live() && start > 0 && ev.Seq > start {
				st := seqtrack.State{Session: ev.Session, Last: start - 1}
				for _, d := range r.tracker.Domains() {
					if cur, _ := r.tracker.Get(d); cur.Session != ev.Session {
						r.tracker.Set(d, st)
						r.rejoin[d] = st
						r.resyncedAt[d] = start
					}
				}
				r.adoptUnseen(ev.Session)
				r.log.Info("joined a new session after its start; asking provider to resend", "domain", ev.Domain,
					"session", ev.Session, "first_seen", ev.Seq)
				return true
			}
		}
		// The first event ever seen of a domain, and not the session's first:
		// we joined late. Ask for the session from its start, once, exactly
		// as for a gap. What the provider can no longer resend is then
		// declared missing by the normalizer.
		if start := r.cfg.SeqStart; res.First && r.adapter.Live() && start > 0 && ev.Seq > start {
			st := seqtrack.State{Session: ev.Session, Last: start - 1}
			r.tracker.Set(ev.Domain, st)
			r.rejoin[ev.Domain] = st
			r.resyncedAt[ev.Domain] = start
			r.adoptUnseen(ev.Session)
			r.log.Info("joined a session after its start; asking provider to resend", "domain", ev.Domain,
				"session", ev.Session, "first_seen", ev.Seq)
			return true
		}
		r.tracker.Observe(ev.Domain, ev.Session, ev.Seq)
		b.events = append(b.events, ev)
		b.recs = append(b.recs, stream.Keyed(stream.TopicRaw, r.cfg.Partitions, ev.TrackKey(), mustJSON(ev)))
	}
	return false
}

// adoptUnseen is called when a resync is about to drop the connection. The
// frames still queued from it are discarded, and the provider resends only
// the domains it is given a position for. A domain of which nothing has been
// seen yet has no position, so its discarded events would stay lost until
// its next event revealed the hole, which on a quiet symbol can be a long
// time. Where the first number of a session is known, such domains are
// therefore positioned just before it, and the same resend covers them.
func (r *Runner) adoptUnseen(session string) {
	start := r.cfg.SeqStart
	if start == 0 {
		return
	}
	st := seqtrack.State{Session: session, Last: start - 1}
	for _, d := range r.adapter.Domains() {
		if _, seen := r.tracker.Get(d); !seen {
			r.tracker.Set(d, st)
			r.rejoin[d] = st
			r.resyncedAt[d] = start
		}
	}
}

// flush publishes the batch, retrying until it succeeds or ctx ends. While
// it retries nothing is read from the provider.
func (r *Runner) flush(ctx context.Context, b *batch) error {
	if len(b.recs) == 0 {
		return nil
	}
	backoff := r.cfg.BackoffMin
	for {
		err := r.bus.Produce(ctx, b.recs...)
		if err == nil {
			break
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		r.log.Warn("publish failed; retrying", "err", err, "records", len(b.recs))
		select {
		case <-time.After(backoff):
		case <-ctx.Done():
			return ctx.Err()
		}
		if backoff *= 2; backoff > r.cfg.BackoffMax {
			backoff = r.cfg.BackoffMax
		}
	}
	provider := r.adapter.Provider()
	for _, ev := range b.events {
		r.durable[ev.Domain] = seqtrack.State{Session: ev.Session, Last: ev.Seq}
		r.m.IngestEvents.WithLabelValues(provider, ev.Kind).Inc()
		r.m.FeedLag.WithLabelValues(provider).Observe(ev.RecvTime.Sub(ev.EventTime).Seconds())
	}
	if n := len(b.events); n > 0 {
		last := b.events[n-1]
		r.mu.Lock()
		r.health.Events += uint64(n)
		r.health.LastEventTime, r.health.LastReceiveTime = last.EventTime, last.RecvTime
		r.health.LagMs = last.RecvTime.Sub(last.EventTime).Milliseconds()
		r.mu.Unlock()
		r.dirty = true
	}
	b.recs, b.events = b.recs[:0], b.events[:0]
	return nil
}

// checkpoint stores the feed offsets. They cover acknowledged events only,
// so a checkpoint can never claim an event the log does not hold.
func (r *Runner) checkpoint(ctx context.Context, force bool) {
	if !r.dirty || (!force && r.cfg.Now().Sub(r.lastCkpt) < r.cfg.CheckpointEvery) {
		return
	}
	if err := r.ckpt.Put(ctx, r.checkpointKey(), mustJSON(r.offsets())); err != nil {
		r.log.Warn("checkpoint failed; will retry", "err", err)
		return
	}
	r.dirty, r.lastCkpt = false, r.cfg.Now()
}

func (r *Runner) setConnected(up bool) {
	v := 0.0
	if up {
		v = 1
	}
	r.m.IngestConnected.WithLabelValues(r.adapter.Provider()).Set(v)
	r.mu.Lock()
	r.health.Connected = up
	if up && r.connected > 1 {
		r.health.Reconnects++
	}
	r.mu.Unlock()
}

func (r *Runner) healthLoop(ctx context.Context) {
	t := time.NewTicker(r.cfg.HealthEvery)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			r.publishHealth(ctx)
		}
	}
}

func (r *Runner) publishHealth(ctx context.Context) {
	h := r.Health()
	key := h.Provider + "/" + h.Source
	// Health is advisory: a failed publish is retried on the next tick.
	_ = r.bus.Produce(ctx, stream.Record{Topic: stream.TopicHealth, Key: []byte(key), Value: domain.Encode(h)})
}

func mustJSON(v any) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		panic("ingestors: marshal: " + err.Error())
	}
	return b
}
