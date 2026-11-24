package app

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"sort"
	"time"

	"mdp/src/domain"
	"mdp/src/ingestors"
	"mdp/src/metrics"
	"mdp/src/refdata"
	"mdp/src/storage"
	"mdp/src/stream"
)

// ReplayResult summarises what a capture turns into.
type ReplayResult struct {
	RawEvents  int            `json:"raw_events"` // provider events accepted into the raw topic
	Trades     int            `json:"trades"`
	Quotes     int            `json:"quotes"`
	Depth      int            `json:"depth"`
	Gaps       int            `json:"gaps"`
	MissingSeq uint64         `json:"missing_seq"` // sequence numbers covered by the gaps
	Duplicates uint64         `json:"duplicates"`  // provider events dropped as redelivered
	Rejects    map[string]int `json:"rejects"`     // dead letters by reason
	// Digest is the SHA-256 of every canonical event and dead letter, in an
	// order that does not depend on scheduling. Equal digests mean equal output.
	Digest string `json:"digest"`
}

// Replay runs a capture file through a complete in-memory pipeline (replay
// adapters, raw topic, normalizer, canonical topics, sink, store) and
// reports the outcome. It uses the same services as a deployment; only the
// log and the store are in-process.
func Replay(ctx context.Context, reg *refdata.Registry, capturePath string, log *slog.Logger) (ReplayResult, *storage.MemStore, error) {
	var res ReplayResult
	lines, err := ingestors.ReadCapture(capturePath)
	if err != nil {
		return res, nil, err
	}
	seen := map[string]bool{}
	var providers []string
	for _, l := range lines {
		if !seen[l.Provider] {
			seen[l.Provider] = true
			providers = append(providers, l.Provider)
		}
	}
	sort.Strings(providers)

	bus := stream.NewMemBus()
	store := storage.NewMemStore()
	// A capture is historical by nature: keep it regardless of its age.
	century := 100 * 365 * 24 * time.Hour
	store.SetRetention(storage.Retention{Trades: century, Quotes: century, Depth: century, Gaps: century})
	if log == nil {
		log = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	st := &Stack{Reg: reg, Bus: bus, Store: store, M: metrics.New(), Opt: DefaultOptions(), Log: log}
	if err := st.Init(ctx); err != nil {
		return res, nil, err
	}
	var runners []*ingestors.Runner
	for _, p := range providers {
		a, err := ingestors.NewReplay(capturePath, p, 0)
		if err != nil {
			return res, nil, err
		}
		runners = append(runners, st.NewIngestor(a))
	}
	norm, sink := st.NewNormalizer(), st.NewSink()

	sctx, stop := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() { done <- Run(sctx, norm.Run, sink.Run) }()
	defer func() { stop(); <-done }()

	ingest := make([]func(context.Context) error, len(runners))
	for i, r := range runners {
		ingest[i] = r.Run
	}
	if err := Run(ctx, ingest...); err != nil {
		return res, nil, err
	}
	if err := WaitCaughtUp(ctx, bus, norm.Progress, stream.TopicRaw); err != nil {
		return res, nil, err
	}
	if err := WaitCaughtUp(ctx, bus, sink.Progress, stream.CanonicalTopics...); err != nil {
		return res, nil, err
	}

	for _, r := range runners {
		res.Duplicates += r.Health().Duplicates
	}
	res.RawEvents = len(bus.All(stream.TopicRaw))
	res.Trades = len(bus.All(stream.TopicTrades))
	res.Quotes = len(bus.All(stream.TopicQuotes))
	res.Depth = len(bus.All(stream.TopicDepth))
	gaps := bus.All(stream.TopicGaps)
	res.Gaps = len(gaps)
	for _, r := range gaps {
		var g domain.Gap
		if json.Unmarshal(r.Value, &g) == nil {
			res.MissingSeq += g.Missing()
		}
	}
	res.Rejects = map[string]int{}
	for _, r := range bus.All(stream.TopicRejects) {
		var rej domain.Reject
		if json.Unmarshal(r.Value, &rej) == nil {
			res.Rejects[rej.Reason]++
		}
	}
	res.Digest = Digest(bus)
	return res, store, nil
}

// Digest hashes the canonical topics and the reject topic of an in-memory
// log. Records are ordered by topic, then key, then their order within the
// key. Order within a key is fixed by the provider's own sequence; order
// across keys depends on goroutine scheduling and is deliberately excluded.
// Dead letters have no meaningful order and are sorted by content.
func Digest(bus *stream.MemBus) string {
	h := sha256.New()
	for _, topic := range stream.CanonicalTopics {
		byKey := map[string][][]byte{}
		for _, r := range bus.All(topic) {
			byKey[string(r.Key)] = append(byKey[string(r.Key)], r.Value)
		}
		keys := make([]string, 0, len(byKey))
		for k := range byKey {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		fmt.Fprintf(h, "topic %s\n", topic)
		for _, k := range keys {
			for _, v := range byKey[k] {
				h.Write(v)
				h.Write([]byte{'\n'})
			}
		}
	}
	var rejects []string
	for _, r := range bus.All(stream.TopicRejects) {
		rejects = append(rejects, string(r.Value))
	}
	sort.Strings(rejects)
	fmt.Fprintf(h, "topic %s\n", stream.TopicRejects)
	for _, v := range rejects {
		io.WriteString(h, v)
		h.Write([]byte{'\n'})
	}
	return hex.EncodeToString(h.Sum(nil))
}
