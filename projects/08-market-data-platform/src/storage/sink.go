package storage

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strconv"
	"time"

	"mdp/src/domain"
	"mdp/src/metrics"
	"mdp/src/stream"
)

// SinkGroup is the consumer group whose committed offsets mark what has been
// persisted.
const SinkGroup = "mdp-sink"

// SinkConfig tunes the sink.
type SinkConfig struct {
	MaxBatch   int           // records per store write cycle
	FlushEvery time.Duration // longest a record waits for its batch to fill
	BackoffMin time.Duration
	BackoffMax time.Duration
	LagEvery   time.Duration
	Now        func() time.Time
}

func (c SinkConfig) withDefaults() SinkConfig {
	if c.MaxBatch <= 0 {
		c.MaxBatch = 5000
	}
	if c.FlushEvery <= 0 {
		c.FlushEvery = 200 * time.Millisecond
	}
	if c.BackoffMin <= 0 {
		c.BackoffMin = 100 * time.Millisecond
	}
	if c.BackoffMax <= 0 {
		c.BackoffMax = 5 * time.Second
	}
	if c.LagEvery <= 0 {
		c.LagEvery = time.Second
	}
	if c.Now == nil {
		c.Now = time.Now
	}
	return c
}

// Sink consumes the canonical topics and writes them to the Store.
//
// Delivery contract. Offsets are committed only after every record of a
// batch is in the store (or dead-lettered), so a crash redelivers the batch
// and the store's deduplication keys absorb it. While the store is
// unavailable the sink retries the same batch and reads nothing new: the
// backlog accumulates in the log and is visible as consumer lag. It is never
// dropped.
type Sink struct {
	bus         stream.Bus
	store       Store
	instruments []domain.Instrument
	m           *metrics.Set
	cfg         SinkConfig
	log         *slog.Logger

	progress stream.Progress
}

// Progress returns, per canonical partition, the next offset not yet persisted.
func (s *Sink) Progress() map[stream.TP]int64 { return s.progress.Snapshot() }

func NewSink(bus stream.Bus, store Store, instruments []domain.Instrument, m *metrics.Set, cfg SinkConfig, log *slog.Logger) *Sink {
	if log == nil {
		log = slog.Default()
	}
	return &Sink{bus: bus, store: store, instruments: instruments, m: m, cfg: cfg.withDefaults(),
		log: log.With("component", "sink")}
}

// Run persists canonical events until ctx ends.
func (s *Sink) Run(ctx context.Context) error {
	// Reference data first, so stored ticks can always be converted to prices.
	if err := s.retry(ctx, "instruments", func() error {
		return s.store.UpsertInstruments(ctx, s.instruments)
	}); err != nil {
		return nil
	}
	cons, err := s.bus.NewConsumer(ctx, stream.ConsumerConfig{
		Group: SinkGroup, Topics: stream.CanonicalTopics, Start: stream.FromEarliest, MaxRecords: s.cfg.MaxBatch,
	})
	if err != nil {
		return fmt.Errorf("sink: consumer: %w", err)
	}
	defer cons.Close()
	s.progress.Init(cons.Positions())

	go stream.WatchLag(ctx, s.bus, s.Progress, s.cfg.LagEvery, stream.LagObserver{
		OnLag: func(tp stream.TP, lag int64) {
			s.m.ConsumerLag.WithLabelValues("sink", tp.Topic, strconv.Itoa(int(tp.Partition))).Set(float64(lag))
		},
		OnCaughtUp: func(d time.Duration) { s.m.ConsumerCatchup.WithLabelValues("sink").Set(d.Seconds()) },
	})

	backoff := s.cfg.BackoffMin
	for {
		recs, err := cons.Poll(ctx)
		if ctx.Err() != nil {
			return nil
		}
		if err != nil {
			s.log.Warn("poll failed; retrying", "err", err)
			if !sleepCtx(ctx, backoff) {
				return nil
			}
			backoff = min(backoff*2, s.cfg.BackoffMax)
			continue
		}
		backoff = s.cfg.BackoffMin
		// Linger briefly so a trickle of events becomes a few larger inserts.
		deadline := s.cfg.Now().Add(s.cfg.FlushEvery)
		for len(recs) < s.cfg.MaxBatch {
			pctx, cancel := context.WithDeadline(ctx, deadline)
			more, err := cons.Poll(pctx)
			cancel()
			if err != nil {
				break
			}
			recs = append(recs, more...)
		}
		if err := s.persist(ctx, recs); err != nil {
			return nil // ctx ended mid-batch; the batch is redelivered on restart
		}
		s.progress.Advance(recs)
		next := map[stream.TP]int64{}
		for _, r := range recs {
			next[stream.TP{Topic: r.Topic, Partition: r.Partition}] = r.Offset + 1
		}
		if err := cons.Commit(ctx, next); err != nil {
			s.log.Warn("offset commit failed; batch may be redelivered", "err", err)
		}
	}
}

// persist writes one batch. It returns an error only if ctx ends first.
func (s *Sink) persist(ctx context.Context, recs []stream.Record) error {
	var (
		trades []domain.Trade
		quotes []domain.Quote
		depth  []domain.Depth
		gaps   []domain.Gap
	)
	for _, r := range recs {
		var err error
		switch r.Topic {
		case stream.TopicTrades:
			var v domain.Trade
			if err = json.Unmarshal(r.Value, &v); err == nil {
				trades = append(trades, v)
			}
		case stream.TopicQuotes:
			var v domain.Quote
			if err = json.Unmarshal(r.Value, &v); err == nil {
				quotes = append(quotes, v)
			}
		case stream.TopicDepth:
			var v domain.Depth
			if err = json.Unmarshal(r.Value, &v); err == nil {
				depth = append(depth, v)
			}
		case stream.TopicGaps:
			var v domain.Gap
			if err = json.Unmarshal(r.Value, &v); err == nil {
				gaps = append(gaps, v)
			}
		}
		if err != nil {
			if derr := s.deadLetter(ctx, "UNDECODABLE_RECORD", err.Error(), r.Value); derr != nil {
				return derr
			}
		}
	}
	// A row already past its table's retention would be accepted by the
	// store and then deleted by its TTL. Refuse it visibly instead.
	now := s.cfg.Now()
	ret := s.store.Retention()
	var err error
	if trades, err = expire(ctx, s, trades, now.Add(-ret.Trades), func(v domain.Trade) time.Time { return v.EventTime }); err != nil {
		return err
	}
	if quotes, err = expire(ctx, s, quotes, now.Add(-ret.Quotes), func(v domain.Quote) time.Time { return v.EventTime }); err != nil {
		return err
	}
	if depth, err = expire(ctx, s, depth, now.Add(-ret.Depth), func(v domain.Depth) time.Time { return v.EventTime }); err != nil {
		return err
	}
	if gaps, err = expire(ctx, s, gaps, now.Add(-ret.Gaps), func(v domain.Gap) time.Time { return v.ReceiveTime }); err != nil {
		return err
	}
	if err := writeRows(ctx, s, "trades", trades, s.store.InsertTrades); err != nil {
		return err
	}
	if err := writeRows(ctx, s, "quotes", quotes, s.store.InsertQuotes); err != nil {
		return err
	}
	if err := writeRows(ctx, s, "depth", depth, s.store.InsertDepth); err != nil {
		return err
	}
	if err := writeRows(ctx, s, "gaps", gaps, s.store.InsertGaps); err != nil {
		return err
	}
	now = s.cfg.Now()
	observe := func(source string, recv time.Time) {
		if source == domain.SourceLive {
			s.m.PipelineLatency.WithLabelValues("stored").Observe(now.Sub(recv).Seconds())
		}
	}
	for _, v := range trades {
		observe(v.Source, v.ReceiveTime)
	}
	for _, v := range quotes {
		observe(v.Source, v.ReceiveTime)
	}
	for _, v := range depth {
		observe(v.Source, v.ReceiveTime)
	}
	return nil
}

// expire dead-letters rows older than cutoff and returns the rest.
func expire[T any](ctx context.Context, s *Sink, rows []T, cutoff time.Time, at func(T) time.Time) ([]T, error) {
	kept := rows[:0]
	for _, r := range rows {
		if at(r).Before(cutoff) {
			s.m.SinkRejectedRows.Inc()
			raw, _ := json.Marshal(r)
			detail := "timestamp " + at(r).UTC().Format(time.RFC3339Nano) + " is before the retention horizon " + cutoff.UTC().Format(time.RFC3339)
			if err := s.deadLetter(ctx, "OLDER_THAN_RETENTION", detail, raw); err != nil {
				return nil, err
			}
			continue
		}
		kept = append(kept, r)
	}
	return kept, nil
}

// writeRows inserts rows, retrying transient failures forever. A permanent
// failure is narrowed by bisection to the rows the store refuses; those are
// dead-lettered and the rest are written.
func writeRows[T any](ctx context.Context, s *Sink, table string, rows []T, insert func(context.Context, []T) error) error {
	if len(rows) == 0 {
		return nil
	}
	var permanent error
	err := s.retry(ctx, table, func() error {
		err := insert(ctx, rows)
		if IsPermanent(err) {
			permanent = err
			return nil
		}
		return err
	})
	if err != nil {
		return err
	}
	if permanent == nil {
		s.m.SinkRows.WithLabelValues(table).Add(float64(len(rows)))
		s.m.SinkBatches.WithLabelValues(table).Inc()
		return nil
	}
	s.m.SinkInsertFailures.WithLabelValues("permanent").Inc()
	if len(rows) == 1 {
		s.m.SinkRejectedRows.Inc()
		s.log.Error("store rejected row; dead-lettering", "table", table, "err", permanent)
		raw, _ := json.Marshal(rows[0])
		return s.deadLetter(ctx, "STORE_REJECTED", permanent.Error(), raw)
	}
	mid := len(rows) / 2
	if err := writeRows(ctx, s, table, rows[:mid], insert); err != nil {
		return err
	}
	return writeRows(ctx, s, table, rows[mid:], insert)
}

// retry runs op until it succeeds or ctx ends.
func (s *Sink) retry(ctx context.Context, what string, op func() error) error {
	backoff := s.cfg.BackoffMin
	defer s.m.SinkRetrying.Set(0)
	for {
		err := op()
		if err == nil {
			return nil
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		s.m.SinkInsertFailures.WithLabelValues("transient").Inc()
		s.m.SinkRetrying.Set(1)
		s.log.Warn("store write failed; retrying", "what", what, "err", err, "backoff", backoff)
		if !sleepCtx(ctx, backoff) {
			return ctx.Err()
		}
		backoff = min(backoff*2, s.cfg.BackoffMax)
	}
}

func (s *Sink) deadLetter(ctx context.Context, reason, detail string, raw []byte) error {
	if len(raw) > 512 {
		raw = raw[:512]
	}
	rej := domain.Reject{Stage: "sink", Reason: reason, Detail: detail, Raw: string(raw), ReceiveTime: s.cfg.Now().UTC()}
	rec := stream.Record{Topic: stream.TopicRejects, Key: []byte(reason), Value: domain.Encode(rej)}
	return s.retry(ctx, "dead-letter", func() error { return s.bus.Produce(ctx, rec) })
}

func sleepCtx(ctx context.Context, d time.Duration) bool {
	select {
	case <-time.After(d):
		return true
	case <-ctx.Done():
		return false
	}
}
