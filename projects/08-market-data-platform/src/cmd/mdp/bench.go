package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"

	"mdp/src/refdata"
	"mdp/src/storage"
	"mdp/src/stream"
)

// benchResult is what `mdp bench` measured. Nothing in it is estimated
// except where a field says so.
type benchResult struct {
	Note     string    `json:"note"`
	When     time.Time `json:"when"`
	Infra    string    `json:"infra"`
	Host     string    `json:"host"`
	CPUs     int       `json:"cpus"`
	Go       string    `json:"go"`
	RateArg  int       `json:"requested_events_per_sec_per_venue"`
	Duration string    `json:"paced_duration"`

	Paced struct {
		Published        int64   `json:"published_events"`
		Ingested         int64   `json:"ingested_events"`
		IngestedBytes    int64   `json:"ingested_bytes"`
		Seconds          float64 `json:"seconds"`
		EventsPerSec     float64 `json:"events_per_sec"`
		BytesPerSec      float64 `json:"bytes_per_sec"`
		StoredRows       int64   `json:"stored_rows"`
		Gaps             int64   `json:"gaps"`
		Duplicates       int64   `json:"duplicates"`
		Reconnects       int64   `json:"reconnects"`
		Rejects          int64   `json:"rejects"`
		WSDelivered      int     `json:"ws_delivered"`
		WSDropped        int64   `json:"ws_dropped"`
		MaxNormalizerLag int64   `json:"max_normalizer_lag_records"`
		MaxSinkLag       int64   `json:"max_sink_lag_records"`
		// Exact percentiles over every event the WebSocket client received:
		// client receive time minus the ingestor's receive_time (one clock).
		WSLatencyMs struct {
			P50 float64 `json:"p50"`
			P99 float64 `json:"p99"`
			Max float64 `json:"max"`
		} `json:"receive_to_ws_client_ms_exact"`
		// Upper bounds: the edge of the histogram bucket holding the quantile.
		NormalizedMs bound `json:"receive_to_normalized_ms_upper_bound"`
		StoredMs     bound `json:"receive_to_stored_ms_upper_bound"`
	} `json:"paced"`

	CatchUp struct {
		Backlog            int64   `json:"backlog_events"`
		IngestSeconds      float64 `json:"ingest_seconds"`
		IngestEventsPerSec float64 `json:"ingest_events_per_sec"`
		Reconnects         int64   `json:"reconnects_during_burst"`
		Seconds            float64 `json:"catch_up_seconds"`
		EventsPerSec       float64 `json:"catch_up_events_per_sec"`
		StoredRows         int64   `json:"stored_rows_total"`
		ExpectedRows       int64   `json:"expected_rows_total"`
	} `json:"catch_up"`
}

type bound struct {
	P50 float64 `json:"p50"`
	P99 float64 `json:"p99"`
	N   uint64  `json:"n"`
}

func percentile(sorted []float64, q float64) float64 {
	if len(sorted) == 0 {
		return 0
	}
	i := int(float64(len(sorted))*q+0.999999) - 1
	if i < 0 {
		i = 0
	}
	if i >= len(sorted) {
		i = len(sorted) - 1
	}
	return sorted[i]
}

func cmdBench(ctx context.Context, args []string) error {
	c := newFlags("bench")
	rate := c.fs.Int("rate", 2000, "paced phase: simulated events per second per venue")
	dur := c.fs.Duration("duration", 10*time.Second, "paced phase: how long to run the feed")
	backlog := c.fs.Int("backlog", 50000, "catch-up phase: events to accumulate while normalizer and sink are down")
	infra := c.fs.String("infra", "memory", "memory: in-process log and store; real: the Kafka/Redpanda and ClickHouse given by the flags, in a throwaway namespace and database")
	seed := c.fs.Uint64("seed", 1, "random seed")
	jsonOut := c.fs.String("json", "", "also write the result to this file as JSON")
	if err := c.fs.Parse(args); err != nil {
		return err
	}
	if *rate < 1 || *dur <= 0 || *backlog < 2 {
		return errors.New("--rate, --duration and --backlog must be positive")
	}
	reg, err := refdata.Load(c.refdata)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	var res benchResult
	res.Note = "SIMULATED load. Measured on one run on one machine; not a capacity claim."
	res.When = time.Now().UTC().Truncate(time.Second)
	res.Host, res.CPUs, res.Go = runtime.GOOS+"/"+runtime.GOARCH, runtime.NumCPU(), runtime.Version()
	res.RateArg, res.Duration = *rate, dur.String()

	var (
		bus   stream.Bus
		store storage.Store
	)
	switch *infra {
	case "memory":
		res.Infra = "in-memory log and store (measures the services, not Kafka or ClickHouse)"
	case "real":
		pw := os.Getenv("MDP_CLICKHOUSE_PASSWORD")
		if pw == "" {
			return errors.New("MDP_CLICKHOUSE_PASSWORD is not set")
		}
		tag := fmt.Sprint(time.Now().UnixNano())
		kb, err := stream.NewKafkaBus(strings.Split(c.brokers, ","), "mdp-bench", "bench"+tag+".")
		if err != nil {
			return err
		}
		ch := storage.NewClickHouse(storage.ClickHouseConfig{URL: c.chURL, Database: storage.TemporaryPrefix + "bench_" + tag,
			User: envOr("MDP_CLICKHOUSE_USER", "mdp"), Password: pw})
		bus, store = kb, ch
		res.Infra = "Kafka-protocol broker at " + c.brokers + " and ClickHouse at " + c.chURL + " (single-node development containers unless you pointed it elsewhere)"
		defer func() {
			// Remove the throwaway namespace and database, even after ^C.
			cctx, ccancel := context.WithTimeout(context.Background(), 60*time.Second)
			defer ccancel()
			if err := kb.DeleteNamespace(cctx); err != nil {
				fmt.Fprintln(os.Stderr, "bench: cleanup:", err)
			}
			kb.Close()
			if err := ch.DropDatabase(cctx); err != nil {
				fmt.Fprintln(os.Stderr, "bench: cleanup:", err)
			}
		}()
	default:
		return fmt.Errorf("--infra must be memory or real, not %q", *infra)
	}

	fmt.Println("mdp bench:", res.Note)
	fmt.Printf("  infra  %s\n  host   %s, %d CPUs, %s\n", res.Infra, res.Host, res.CPUs, res.Go)

	r, err := newRig(ctx, reg, bus, store, c.options(), *seed, nil)
	if err != nil {
		return err
	}
	if err := r.startIngestors(); err != nil {
		return err
	}
	r.startNormalizer()
	r.startSink()
	m := r.st.M

	// A real WebSocket client subscribed to everything times each event.
	var ids []string
	for _, in := range reg.All() {
		ids = append(ids, in.ID)
	}
	var (
		mu      sync.Mutex
		samples []float64
	)
	ws, err := r.subscribe("instrument_id="+strings.Join(ids, ",")+"&channels=trade,quote,depth", func(msg map[string]any, at time.Time) {
		s, _ := msg["receive_time"].(string)
		if recv, err := time.Parse(time.RFC3339Nano, s); err == nil {
			mu.Lock()
			samples = append(samples, float64(at.Sub(recv))/float64(time.Millisecond))
			mu.Unlock()
		}
	})
	if err != nil {
		return err
	}
	defer ws.close()

	published := func() int64 { return int64(r.sim.Alpha.Published() + r.sim.Beta.Published()) }
	timeout := 5*time.Minute + 2**dur

	// ---- Phase 1: paced feed
	fmt.Printf("\nPhase 1: paced feed, 2 venues x %d events/s for %s\n", *rate, *dur)
	lagCtx, stopLag := context.WithCancel(ctx)
	defer stopLag()
	var lagDone sync.WaitGroup
	lagDone.Add(1)
	go func() {
		defer lagDone.Done()
		t := time.NewTicker(100 * time.Millisecond)
		defer t.Stop()
		for {
			select {
			case <-lagCtx.Done():
				return
			case <-t.C:
				if l := r.lag(r.norm.Progress); l > res.Paced.MaxNormalizerLag {
					res.Paced.MaxNormalizerLag = l
				}
				if l := r.lag(r.sink.Progress); l > res.Paced.MaxSinkLag {
					res.Paced.MaxSinkLag = l
				}
			}
		}
	}()
	feedCtx, stopFeed := context.WithTimeout(ctx, *dur)
	began := time.Now()
	r.sim.Run(feedCtx, float64(*rate))
	stopFeed()
	if ctx.Err() != nil {
		return ctx.Err()
	}
	res.Paced.Published = published()
	if err := r.waitRaw(res.Paced.Published, timeout); err != nil {
		return err
	}
	// Ingest is complete once the last published event is in the raw topic.
	res.Paced.Seconds = time.Since(began).Seconds()
	if err := r.settle(timeout); err != nil {
		return err
	}
	stopLag()
	lagDone.Wait()
	if err := r.wait(10*time.Second, "the WebSocket client to receive everything", func() bool {
		n, _, _ := ws.snapshot()
		return int64(n)+int64(m.Sum("mdp_ws_dropped_total")) >= res.Paced.Published-int64(m.Sum("mdp_normalizer_rejects_total"))
	}); err != nil {
		fmt.Println("  note: the WebSocket client did not receive every event:", err)
	}

	p := &res.Paced
	p.Ingested = int64(m.Sum("mdp_ingest_events_total"))
	p.IngestedBytes = int64(m.Sum("mdp_ingest_bytes_total"))
	p.EventsPerSec = float64(p.Ingested) / p.Seconds
	p.BytesPerSec = float64(p.IngestedBytes) / p.Seconds
	if p.StoredRows, err = r.stored(); err != nil {
		return err
	}
	p.Gaps = int64(m.Sum("mdp_gaps_total"))
	p.Duplicates = int64(m.Sum("mdp_duplicates_total") + m.Sum("mdp_ingest_duplicates_total"))
	p.Reconnects = int64(m.Sum("mdp_ingest_reconnects_total"))
	p.Rejects = int64(m.Sum("mdp_normalizer_rejects_total") + m.Sum("mdp_sink_rejected_rows_total"))
	p.WSDropped = int64(m.Sum("mdp_ws_dropped_total"))
	mu.Lock()
	sorted := append([]float64(nil), samples...)
	mu.Unlock()
	sort.Float64s(sorted)
	p.WSDelivered = len(sorted)
	p.WSLatencyMs.P50, p.WSLatencyMs.P99 = percentile(sorted, 0.50), percentile(sorted, 0.99)
	if len(sorted) > 0 {
		p.WSLatencyMs.Max = sorted[len(sorted)-1]
	}
	stage := func(name string) bound {
		p50, n := m.Quantile("mdp_pipeline_latency_seconds", 0.50, "stage", name)
		p99, _ := m.Quantile("mdp_pipeline_latency_seconds", 0.99, "stage", name)
		return bound{P50: p50 * 1000, P99: p99 * 1000, N: n}
	}
	p.NormalizedMs, p.StoredMs = stage("normalized"), stage("stored")

	fmt.Printf("  published                %d events\n", p.Published)
	fmt.Printf("  ingested                 %d events, %d bytes in %.2fs = %.0f events/s, %.2f MB/s\n", p.Ingested, p.IngestedBytes, p.Seconds,
		p.EventsPerSec, p.BytesPerSec/1e6)
	fmt.Printf("  stored                   %d rows   (gaps %d, duplicates dropped %d, reconnects %d, rejects %d)\n", p.StoredRows, p.Gaps, p.Duplicates,
		p.Reconnects, p.Rejects)
	fmt.Printf("  receive -> normalized    p50 <= %.2f ms, p99 <= %.2f ms   (histogram bucket upper bounds, n=%d)\n", p.NormalizedMs.P50, p.NormalizedMs.P99, p.NormalizedMs.N)
	fmt.Printf("  receive -> stored        p50 <= %.2f ms, p99 <= %.2f ms   (histogram bucket upper bounds, n=%d)\n", p.StoredMs.P50, p.StoredMs.P99, p.StoredMs.N)
	fmt.Printf("  receive -> WS client     p50 %.2f ms, p99 %.2f ms, max %.2f ms   (exact, n=%d, dropped for slow consumer: %d)\n", p.WSLatencyMs.P50,
		p.WSLatencyMs.P99, p.WSLatencyMs.Max, p.WSDelivered, p.WSDropped)
	fmt.Printf("  max consumer lag seen    normalizer %d records, sink %d records   (sampled every 100 ms)\n", p.MaxNormalizerLag, p.MaxSinkLag)
	if p.StoredRows != p.Published-p.Rejects {
		fmt.Printf("  WARNING: stored rows (%d) differ from published minus rejected (%d)\n", p.StoredRows, p.Published-p.Rejects)
	}

	// ---- Phase 2: catch-up
	fmt.Printf("\nPhase 2: catch-up. Normalizer and sink are stopped while %d events arrive, then restarted\n", *backlog)
	r.normSvc.stop()
	r.sinkSvc.stop()
	cu := &res.CatchUp
	reconnectsBefore := int64(m.Sum("mdp_ingest_reconnects_total"))
	began = time.Now()
	for sent := 0; sent < *backlog/2; {
		n := min(100, *backlog/2-sent)
		r.sim.Step(n)
		sent += n
		// Stay within what the ingestors accept: a venue disconnects a
		// subscriber that falls too far behind, as real venues do.
		if err := r.waitRaw(published()-2000, timeout); err != nil {
			return err
		}
	}
	total := published()
	cu.Backlog = total - p.Published
	if err := r.waitRaw(total, timeout); err != nil {
		return err
	}
	cu.IngestSeconds = time.Since(began).Seconds()
	cu.IngestEventsPerSec = float64(cu.Backlog) / cu.IngestSeconds
	cu.Reconnects = int64(m.Sum("mdp_ingest_reconnects_total")) - reconnectsBefore

	began = time.Now()
	r.startNormalizer()
	r.startSink()
	if err := r.settle(timeout); err != nil {
		return err
	}
	cu.Seconds = time.Since(began).Seconds()
	cu.EventsPerSec = float64(cu.Backlog) / cu.Seconds
	if cu.StoredRows, err = r.stored(); err != nil {
		return err
	}
	cu.ExpectedRows = total - int64(m.Sum("mdp_normalizer_rejects_total")+m.Sum("mdp_sink_rejected_rows_total"))
	fmt.Printf("  ingest (unpaced)         %d events in %.2fs = %.0f events/s   (reconnects %d)\n", cu.Backlog, cu.IngestSeconds, cu.IngestEventsPerSec, cu.Reconnects)
	fmt.Printf("  catch-up                 %d raw events normalized and stored in %.2fs = %.0f events/s\n", cu.Backlog, cu.Seconds, cu.EventsPerSec)
	fmt.Printf("  store                    %d rows, expected %d\n", cu.StoredRows, cu.ExpectedRows)

	if *jsonOut != "" {
		if err := writeJSON(*jsonOut, res); err != nil {
			return err
		}
		fmt.Println("\nwrote", *jsonOut)
	}
	if cu.StoredRows != cu.ExpectedRows {
		return fmt.Errorf("store holds %d rows, expected %d", cu.StoredRows, cu.ExpectedRows)
	}
	return nil
}
