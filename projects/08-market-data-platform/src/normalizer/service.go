package normalizer

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"time"

	"mdp/src/domain"
	"mdp/src/ingestors"
	"mdp/src/metrics"
	"mdp/src/refdata"
	"mdp/src/seqtrack"
	"mdp/src/stream"
)

// Group is the consumer group the normalizer commits to. The commits are for
// lag tooling only; the normalizer resumes from its own checkpoint.
const Group = "mdp-normalizer"

// ServiceConfig tunes the normalizer service.
type ServiceConfig struct {
	Partitions      int32         // partitions of the raw and canonical topics
	MaxRecords      int           // raw records per poll
	CheckpointEvery time.Duration // how often state is checkpointed; bounds reprocessing after a crash
	BackoffMin      time.Duration
	BackoffMax      time.Duration
	LagEvery        time.Duration
	Now             func() time.Time
}

func (c ServiceConfig) withDefaults() ServiceConfig {
	if c.Partitions <= 0 {
		c.Partitions = 1
	}
	if c.MaxRecords <= 0 {
		c.MaxRecords = 1000
	}
	if c.CheckpointEvery <= 0 {
		c.CheckpointEvery = 500 * time.Millisecond
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

// partitionState is what the service checkpoints per raw partition: the next
// offset to read and the sequence tracker as of that offset. Storing them in
// one record makes them atomic, so after a restart the tracker is exactly as
// it was when the last covered record was processed. A gap can therefore
// never slip through a restart, and a redelivered event is always recognised.
type partitionState struct {
	NextOffset int64                     `json:"next_offset"`
	Domains    map[string]seqtrack.State `json:"domains"`
}

// Service consumes the raw topic and produces canonical events at least once.
//
// For each batch it publishes every output, then checkpoints, in that order.
// A crash between the two reprocesses the batch and republishes the same
// events with the same deduplication keys.
type Service struct {
	bus  stream.Bus
	ckpt *stream.Checkpoints
	norm *Normalizer
	reg  *refdata.Registry
	m    *metrics.Set
	cfg  ServiceConfig
	log  *slog.Logger

	progress stream.Progress
}

// Progress returns, per raw partition, the next offset whose outputs have
// not yet been published.
func (s *Service) Progress() map[stream.TP]int64 { return s.progress.Snapshot() }

func NewService(bus stream.Bus, reg *refdata.Registry, opt Options, m *metrics.Set, cfg ServiceConfig, log *slog.Logger) *Service {
	if log == nil {
		log = slog.Default()
	}
	return &Service{
		bus: bus, ckpt: stream.NewCheckpoints(bus), norm: New(reg, opt), reg: reg, m: m,
		cfg: cfg.withDefaults(), log: log.With("component", "normalizer"),
	}
}

func checkpointKey(partition int32) string {
	return "normalizer/" + stream.TopicRaw + "/" + strconv.Itoa(int(partition))
}

// Run processes raw events until ctx ends.
func (s *Service) Run(ctx context.Context) error {
	saved, err := s.ckpt.Load(ctx)
	if err != nil {
		return fmt.Errorf("normalizer: load checkpoints: %w", err)
	}
	trackers := map[int32]*seqtrack.Tracker{}
	next := map[stream.TP]int64{}
	for p := int32(0); p < s.cfg.Partitions; p++ {
		trackers[p] = seqtrack.New()
		raw, ok := saved[checkpointKey(p)]
		if !ok {
			continue
		}
		var st partitionState
		if err := json.Unmarshal(raw, &st); err != nil {
			return fmt.Errorf("normalizer: checkpoint for partition %d: %w", p, err)
		}
		trackers[p].Restore(st.Domains)
		next[stream.TP{Topic: stream.TopicRaw, Partition: p}] = st.NextOffset
	}
	cons, err := s.bus.NewConsumer(ctx, stream.ConsumerConfig{
		Group: Group, Topics: []string{stream.TopicRaw}, Start: stream.FromEarliest,
		Offsets: next, MaxRecords: s.cfg.MaxRecords,
	})
	if err != nil {
		return fmt.Errorf("normalizer: consumer: %w", err)
	}
	defer cons.Close()
	s.progress.Init(cons.Positions())
	s.log.Info("started", "restored_partitions", len(next))

	go stream.WatchLag(ctx, s.bus, s.Progress, s.cfg.LagEvery, stream.LagObserver{
		OnLag: func(tp stream.TP, lag int64) {
			s.m.ConsumerLag.WithLabelValues("normalizer", tp.Topic, strconv.Itoa(int(tp.Partition))).Set(float64(lag))
		},
		OnCaughtUp: func(d time.Duration) { s.m.ConsumerCatchup.WithLabelValues("normalizer").Set(d.Seconds()) },
	})

	dirty := map[int32]int64{} // partition → next offset not yet checkpointed
	lastCkpt := s.cfg.Now()
	// unpublished is set when a batch has been run through the trackers but
	// could not be published. The trackers are then ahead of the log, and a
	// checkpoint taken from them would mark those events as seen: they would
	// be dropped as duplicates after a restart. So no checkpoint is written.
	unpublished := false
	checkpoint := func(ctx context.Context) {
		// Counted from the attempt, so a failing checkpoint is retried at
		// the same interval rather than in a tight loop.
		lastCkpt = s.cfg.Now()
		if len(dirty) == 0 || unpublished {
			return
		}
		commits := map[stream.TP]int64{}
		for p, off := range dirty {
			st := partitionState{NextOffset: off, Domains: trackers[p].Snapshot()}
			b, _ := json.Marshal(st)
			if err := s.ckpt.Put(ctx, checkpointKey(p), b); err != nil {
				s.log.Warn("checkpoint failed; will retry", "partition", p, "err", err)
				return
			}
			commits[stream.TP{Topic: stream.TopicRaw, Partition: p}] = off
			delete(dirty, p)
		}
		if err := cons.Commit(ctx, commits); err != nil {
			s.log.Warn("offset commit failed", "err", err)
		}
	}
	defer func() {
		grace, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Second)
		defer cancel()
		checkpoint(grace)
	}()

	backoff := s.cfg.BackoffMin
	for {
		// With progress still to save, wait for input only until the next
		// checkpoint is due. Otherwise the tail of a feed that goes quiet
		// would stay unsaved until its next event: reprocessed after a
		// restart, and shown as lag by anything reading the group's offsets.
		pctx, cancel := ctx, context.CancelFunc(func() {})
		if len(dirty) > 0 {
			pctx, cancel = context.WithTimeout(ctx, max(s.cfg.CheckpointEvery-s.cfg.Now().Sub(lastCkpt), time.Millisecond))
		}
		recs, err := cons.Poll(pctx)
		cancel()
		if ctx.Err() != nil {
			return nil
		}
		if len(recs) == 0 && errors.Is(err, context.DeadlineExceeded) {
			checkpoint(ctx)
			continue
		}
		if err != nil {
			s.log.Warn("poll failed; retrying", "err", err)
			if !sleep(ctx, backoff) {
				return nil
			}
			backoff = min(backoff*2, s.cfg.BackoffMax)
			continue
		}
		out := make([]stream.Record, 0, len(recs))
		var stats batchStats
		for _, rec := range recs {
			out = s.process(rec, trackers[rec.Partition], out, &stats)
		}
		// Publish before recording progress: at-least-once.
		for len(out) > 0 {
			err := s.bus.Produce(ctx, out...)
			if err == nil {
				break
			}
			if ctx.Err() == nil {
				s.log.Warn("publish failed; retrying", "err", err, "records", len(out))
			}
			if !sleep(ctx, backoff) {
				unpublished = true
				return nil
			}
			backoff = min(backoff*2, s.cfg.BackoffMax)
		}
		backoff = s.cfg.BackoffMin
		stats.record(s.m, s.cfg.Now())
		s.progress.Advance(recs)
		for _, rec := range recs {
			dirty[rec.Partition] = rec.Offset + 1
		}
		if s.cfg.Now().Sub(lastCkpt) >= s.cfg.CheckpointEvery {
			checkpoint(ctx)
		}
	}
}

// batchStats defers metric updates until a batch is published, so counters
// describe what actually reached the log.
type batchStats struct {
	kinds, rejects, flags             map[string]int
	gaps, missing, duplicates, resets map[string]int
	liveRecv                          []time.Time
}

func bump(m *map[string]int, k string, n int) {
	if *m == nil {
		*m = map[string]int{}
	}
	(*m)[k] += n
}

func (b *batchStats) record(m *metrics.Set, now time.Time) {
	for k, n := range b.kinds {
		m.NormalizedEvents.WithLabelValues(k).Add(float64(n))
	}
	for k, n := range b.rejects {
		m.NormalizerRejects.WithLabelValues(k).Add(float64(n))
	}
	for k, n := range b.flags {
		m.Flagged.WithLabelValues(k).Add(float64(n))
	}
	for k, n := range b.gaps {
		m.Gaps.WithLabelValues(k).Add(float64(n))
	}
	for k, n := range b.missing {
		m.GapMissing.WithLabelValues(k).Add(float64(n))
	}
	for k, n := range b.duplicates {
		m.Duplicates.WithLabelValues(k).Add(float64(n))
	}
	for k, n := range b.resets {
		m.SessionResets.WithLabelValues(k).Add(float64(n))
	}
	for _, recv := range b.liveRecv {
		m.PipelineLatency.WithLabelValues("normalized").Observe(now.Sub(recv).Seconds())
	}
}

// process handles one raw record and appends its outputs.
func (s *Service) process(rec stream.Record, tr *seqtrack.Tracker, out []stream.Record, stats *batchStats) []stream.Record {
	var ev ingestors.Event
	if err := json.Unmarshal(rec.Value, &ev); err != nil || ev.Provider == "" {
		detail := "missing provider"
		if err != nil {
			detail = err.Error()
		}
		bump(&stats.rejects, ReasonUndecodable, 1)
		return append(out, rejectRecord(domain.Reject{Stage: "normalize", Reason: ReasonUndecodable,
			Detail: detail, Raw: truncate(rec.Value), ReceiveTime: rec.Time.UTC()}))
	}

	res := tr.Observe(ev.TrackKey(), ev.Session, ev.Seq)
	switch res.Verdict {
	case seqtrack.Duplicate:
		bump(&stats.duplicates, ev.Provider, 1)
		return out
	case seqtrack.Reset:
		bump(&stats.resets, ev.Provider, 1)
	case seqtrack.Gap:
		out = s.declareGap(ev, res.MissingFrom, res.MissingTo, out, stats)
	}
	// A session joined after its first number has already lost its
	// beginning, whether it is a new session (reset) or the first one this
	// pipeline ever saw of the domain. Neither has a previous number to
	// compare against, so without this the loss would be invisible.
	if res.Verdict == seqtrack.Reset || res.First {
		if prov, ok := s.reg.Provider(ev.Provider); ok && prov.SeqStart > 0 && ev.Seq > prov.SeqStart {
			out = s.declareGap(ev, prov.SeqStart, ev.Seq-1, out, stats)
		}
	}

	msg, rej := s.norm.Normalize(ev)
	if rej != nil {
		bump(&stats.rejects, rej.Reason, 1)
		return append(out, rejectRecord(domain.Reject{Stage: "normalize", Reason: rej.Reason, Detail: rej.Detail,
			Provider: ev.Provider, Raw: truncate(rec.Value), ReceiveTime: ev.RecvTime.UTC()}))
	}
	var (
		topic, key string
		meta       domain.Meta
	)
	switch v := msg.(type) {
	case domain.Trade:
		topic, key, meta = stream.TopicTrades, v.InstrumentID, v.Meta
	case domain.Quote:
		topic, key, meta = stream.TopicQuotes, v.InstrumentID, v.Meta
	case domain.Depth:
		topic, key, meta = stream.TopicDepth, v.InstrumentID, v.Meta
	}
	bump(&stats.kinds, ev.Kind, 1)
	for _, f := range meta.Flags {
		bump(&stats.flags, f, 1)
	}
	if ev.Source == domain.SourceLive {
		stats.liveRecv = append(stats.liveRecv, ev.RecvTime)
	}
	return append(out, stream.Keyed(topic, s.cfg.Partitions, key, domain.Encode(msg)))
}

// declareGap appends a gap record for sequence numbers [from, to] revealed by ev.
func (s *Service) declareGap(ev ingestors.Event, from, to uint64, out []stream.Record, stats *batchStats) []stream.Record {
	gap := domain.Gap{
		Type: domain.KindGap, Provider: ev.Provider, Session: ev.Session, SeqDomain: ev.Domain,
		FromSeq: from, ToSeq: to, ReceiveTime: ev.RecvTime.UTC(), Source: ev.Source,
	}
	key := ev.Provider
	if prov, ok := s.reg.Provider(ev.Provider); ok {
		gap.Venue = prov.Venue
		key = prov.Venue
		if prov.SeqScope == "symbol" {
			if inst, ok := s.reg.Lookup(prov.Venue, ev.Symbol); ok {
				gap.InstrumentID = inst.ID
				key = inst.ID
			}
		}
	}
	bump(&stats.gaps, ev.Provider, 1)
	bump(&stats.missing, ev.Provider, int(gap.Missing()))
	return append(out, stream.Keyed(stream.TopicGaps, s.cfg.Partitions, key, domain.Encode(gap)))
}

func rejectRecord(r domain.Reject) stream.Record {
	return stream.Record{Topic: stream.TopicRejects, Key: []byte(r.Reason), Value: domain.Encode(r)}
}

func truncate(b []byte) string {
	if len(b) > 512 {
		b = b[:512]
	}
	return string(b)
}

func sleep(ctx context.Context, d time.Duration) bool {
	select {
	case <-time.After(d):
		return true
	case <-ctx.Done():
		return false
	}
}
