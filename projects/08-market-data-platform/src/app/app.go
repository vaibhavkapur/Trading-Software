// Package app wires the services together. The commands and the tests both
// build their pipelines here, so what is tested is what runs.
package app

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"mdp/src/gateway"
	"mdp/src/ingestors"
	"mdp/src/metrics"
	"mdp/src/normalizer"
	"mdp/src/refdata"
	"mdp/src/storage"
	"mdp/src/stream"
)

// Options collects every service's tunables.
type Options struct {
	Layout     stream.Layout
	Ingest     ingestors.Config
	Normalize  normalizer.ServiceConfig
	Thresholds normalizer.Options
	Sink       storage.SinkConfig
	Hub        gateway.HubConfig
	Server     gateway.ServerConfig
}

// DefaultOptions are the documented defaults.
func DefaultOptions() Options {
	return Options{Layout: stream.DefaultLayout(), Thresholds: normalizer.DefaultOptions()}
}

// Stack is the shared infrastructure services are built on.
type Stack struct {
	Reg   *refdata.Registry
	Bus   stream.Bus
	Store storage.Store
	M     *metrics.Set
	Opt   Options
	Log   *slog.Logger
}

// Init creates topics and the store schema. It is idempotent.
func (s *Stack) Init(ctx context.Context) error {
	if err := s.Bus.EnsureTopics(ctx, s.Opt.Layout.Specs()...); err != nil {
		return err
	}
	if s.Store != nil {
		if err := s.Store.Migrate(ctx); err != nil {
			return fmt.Errorf("store migrate: %w", err)
		}
	}
	return nil
}

func (s *Stack) NewIngestor(a ingestors.Adapter) *ingestors.Runner {
	cfg := s.Opt.Ingest
	cfg.Partitions = s.Opt.Layout.Partitions
	if prov, ok := s.Reg.Provider(a.Provider()); ok {
		cfg.SeqStart = prov.SeqStart
	}
	return ingestors.NewRunner(a, s.Bus, s.M, cfg, s.Log)
}

func (s *Stack) NewNormalizer() *normalizer.Service {
	cfg := s.Opt.Normalize
	cfg.Partitions = s.Opt.Layout.Partitions
	return normalizer.NewService(s.Bus, s.Reg, s.Opt.Thresholds, s.M, cfg, s.Log)
}

func (s *Stack) NewSink() *storage.Sink {
	return storage.NewSink(s.Bus, s.Store, s.Reg.All(), s.M, s.Opt.Sink, s.Log)
}

func (s *Stack) NewGateway(ent *gateway.Entitlements) (*gateway.Hub, *gateway.Server) {
	hub := gateway.NewHub(s.Bus, s.Reg, s.M, s.Opt.Hub, s.Log)
	return hub, gateway.NewServer(s.Reg, s.Store, hub, ent, s.M, s.Opt.Server)
}

// Run runs fns concurrently. When one returns an error the others are
// cancelled; Run waits for all of them and returns the first error.
func Run(ctx context.Context, fns ...func(context.Context) error) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	var (
		wg    sync.WaitGroup
		once  sync.Once
		first error
	)
	for _, fn := range fns {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := fn(ctx); err != nil {
				once.Do(func() { first = err; cancel() })
			}
		}()
	}
	wg.Wait()
	return first
}

// WaitCaughtUp blocks until progress has reached the current end of every
// partition of topics, i.e. everything published so far has been processed.
func WaitCaughtUp(ctx context.Context, bus stream.Bus, progress func() map[stream.TP]int64, topics ...string) error {
	for {
		ends, err := bus.EndOffsets(ctx, topics...)
		if err == nil && stream.CaughtUp(ends, progress()) {
			return nil
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("waiting for %v to be processed: %w", topics, ctx.Err())
		case <-time.After(5 * time.Millisecond):
		}
	}
}
