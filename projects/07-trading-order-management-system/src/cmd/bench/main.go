// Command bench measures the system under load and prints what it measured.
// Nothing it prints is an estimate: every number comes from this run.
//
// SIMULATED / PAPER ONLY. The whole stack runs in this process (PostgreSQL
// with fsync on, the simulated broker in auto mode, the OMS with its outbox
// worker), connected over loopback HTTP. The load generator shares the
// process and the machine with the system under test, so treat the results
// as a single-machine baseline, not a capacity plan.
//
// What is measured:
//   - POST /v1/orders latency and throughput at a fixed concurrency
//   - the same requests repeated with the same idempotency keys (replays)
//   - GET /v1/orders/{id} latency
//   - per-order pipeline times read back from the database: accepted to
//     first submission attempt, submission to broker acknowledgement, and
//     accepted to fully filled. These include the simulated broker's own
//     delays (-ack-delay, -fill-delay).
//   - one reconciliation run over everything
//   - a full event-log replay check (store.Verify)
//
// By default clients send as fast as they can (a burst: the dispatcher and
// the broker's webhook senders queue up, and the pipeline times show that
// queueing). With -rate N, new orders are released at N per second instead
// and each latency is measured from the moment the request was due, not from
// when a free client got to it.
//
// Usage: go run ./src/cmd/bench [-orders 2000] [-concurrency 16] [-rate 0] [-durable=true]
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"runtime"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"oms/src/brokersim"
	"oms/src/devpg"
	"oms/src/domain"
	"oms/src/fixed"
	"oms/src/localstack"
	"oms/src/outbox"
	"oms/src/reconcile"
	"oms/src/service"
	"oms/src/store"
)

type latency struct {
	Count      int     `json:"count"`
	Errors     int     `json:"errors"`
	P50Ms      float64 `json:"p50_ms"`
	P95Ms      float64 `json:"p95_ms"`
	P99Ms      float64 `json:"p99_ms"`
	MaxMs      float64 `json:"max_ms"`
	WallSec    float64 `json:"wall_seconds,omitempty"`
	Throughput float64 `json:"throughput_per_second,omitempty"`
}

type report struct {
	Mode        string `json:"mode"`
	GoVersion   string `json:"go_version"`
	Platform    string `json:"platform"`
	CPUs        int    `json:"cpus"`
	Postgres    string `json:"postgres"`
	Durability  string `json:"commit_durability"`
	Orders      int    `json:"orders"`
	Concurrency int    `json:"concurrency"`
	Rate        int    `json:"target_orders_per_second"` // 0: as fast as possible
	Positions   int    `json:"distinct_positions"`
	OutboxConc  int    `json:"outbox_concurrency"`
	Senders     int    `json:"sim_webhook_senders"`
	AckDelay    string `json:"sim_ack_delay"`
	FillDelay   string `json:"sim_fill_delay"`

	Create   latency `json:"create_order"`
	Replay   latency `json:"idempotent_replay"`
	Read     latency `json:"get_order"`
	Dispatch latency `json:"accepted_to_first_submit_attempt"`
	Ack      latency `json:"submit_to_ack"`
	Filled   latency `json:"accepted_to_filled"`

	SettleSec        float64  `json:"all_orders_filled_after_seconds"`
	EndToEndPerSec   float64  `json:"orders_filled_per_second"`
	OrdersFilled     int      `json:"orders_filled"`
	ReplaysDistinct  int      `json:"orders_after_replays"`
	ReconcileSec     float64  `json:"reconciliation_run_seconds"`
	ReconcileDrift   int      `json:"reconciliation_drift"`
	VerifySec        float64  `json:"verify_seconds"`
	VerifyEvents     int      `json:"verify_events"`
	VerifyProblems   int      `json:"verify_problems"`
	VerifyDetails    []string `json:"verify_problem_details,omitempty"`
	BrokerSubmits    int      `json:"broker_submit_requests"`
	BrokerDuplicates int      `json:"broker_duplicate_submits"`
	WebhooksSent     int      `json:"webhooks_sent"`
	HeapAllocMB      float64  `json:"process_heap_alloc_mb"`
	SysMB            float64  `json:"process_sys_mb"`
	TotalAllocMB     float64  `json:"process_total_alloc_mb"`
}

func main() {
	orders := flag.Int("orders", 2000, "orders to place")
	concurrency := flag.Int("concurrency", 16, "concurrent API clients")
	rate := flag.Int("rate", 0, "release new orders at this many per second (0: as fast as possible)")
	durable := flag.Bool("durable", true, "make commits in the throwaway PostgreSQL durable (fsync to stable storage)")
	ackDelay := flag.Duration("ack-delay", time.Millisecond, "simulated broker: delay before acknowledging")
	fillDelay := flag.Duration("fill-delay", time.Millisecond, "simulated broker: delay before each fill")
	spread := flag.Bool("spread", false, "spread orders over 2 accounts x 4 symbols instead of one account and symbol (one position row)")
	outboxConc := flag.Int("outbox-concurrency", 8, "broker calls the outbox worker makes in parallel")
	senders := flag.Int("webhook-senders", 4, "simulated broker: concurrent webhook connections to the OMS")
	out := flag.String("out", "", "also write the results as JSON to this file")
	flag.Parse()
	if *orders < 1 || *concurrency < 1 || *rate < 0 || *outboxConc < 1 || *senders < 1 {
		fmt.Fprintln(os.Stderr, "bench: counts must be positive and -rate not negative")
		os.Exit(2)
	}

	rep, err := run(*orders, *concurrency, *rate, *outboxConc, *senders, *spread, *durable, *ackDelay, *fillDelay)
	if err != nil {
		fmt.Fprintln(os.Stderr, "bench:", err)
		os.Exit(1)
	}
	print(rep)
	if *out != "" {
		data, _ := json.MarshalIndent(rep, "", "  ")
		if err := os.WriteFile(*out, append(data, '\n'), 0o644); err != nil {
			fmt.Fprintln(os.Stderr, "bench:", err)
			os.Exit(1)
		}
	}
	if rep.Create.Errors+rep.Replay.Errors+rep.Read.Errors > 0 || rep.OrdersFilled != rep.Orders ||
		rep.ReplaysDistinct != rep.Orders || rep.VerifyProblems > 0 || rep.ReconcileDrift > 0 {
		fmt.Fprintln(os.Stderr, "bench: the run was not clean; see the numbers above")
		os.Exit(1)
	}
}

func run(orders, concurrency, rate, outboxConc, senders int, spread, durable bool, ackDelay, fillDelay time.Duration) (report, error) {
	ctx := context.Background()
	rep := report{
		Mode: "SIMULATED", GoVersion: runtime.Version(), Platform: runtime.GOOS + "/" + runtime.GOARCH,
		CPUs: runtime.NumCPU(), Postgres: string(devpg.Version) + " (embedded, loopback TCP)",
		Durability: devpg.Durability(devpg.Options{Durable: durable}),
		Orders:     orders, Concurrency: concurrency, Rate: rate, OutboxConc: outboxConc, Senders: senders, AckDelay: ackDelay.String(), FillDelay: fillDelay.String(),
	}

	s, err := localstack.Start(ctx, localstack.Options{
		DatabaseURL: os.Getenv("OMS_BENCH_DATABASE_URL"),
		Durable:     durable,
		Auto:        true,
		Background:  true,
		Outbox:      outbox.Config{PollInterval: 50 * time.Millisecond, Concurrency: outboxConc, Batch: 4 * outboxConc},
		Reconcile:   reconcile.Config{Grace: time.Second},
		Store:       store.Options{MaxConns: 48},
		Sim: func(c *brokersim.Config) {
			c.AckDelay, c.FillDelay, c.DeliveryWorkers = ackDelay, fillDelay, senders
		},
	})
	if err != nil {
		return rep, err
	}
	defer s.Close()
	if os.Getenv("OMS_BENCH_DATABASE_URL") != "" {
		rep.Postgres = "external server (OMS_BENCH_DATABASE_URL)"
		rep.Durability = "as configured on that server"
	}

	client := &http.Client{
		Timeout:   30 * time.Second,
		Transport: &http.Transport{MaxIdleConns: concurrency * 2, MaxIdleConnsPerHost: concurrency * 2},
	}
	// Every fill for the same account and symbol updates the same position
	// row, so one account and symbol is the worst case for contention.
	type book struct {
		account, symbol string
		limit           fixed.Micros
	}
	books := []book{{"ACC-1", "AAPL", fixed.MustParse("150")}}
	if spread {
		books = nil
		for _, account := range []string{"ACC-1", "ACC-2"} {
			for symbol, px := range map[string]string{"AAPL": "150", "MSFT": "400", "TSLA": "250", "NVDA": "900"} {
				books = append(books, book{account, symbol, fixed.MustParse(px)})
			}
		}
	}
	rep.Positions = len(books)
	body := func(i int) []byte {
		bk := books[i%len(books)]
		b, _ := json.Marshal(service.CreateOrderRequest{
			AccountID: bk.account, ClientOrderID: fmt.Sprintf("bench-%06d", i), Symbol: bk.symbol,
			Side: domain.Buy, Type: domain.Limit, TimeInForce: domain.Day, Qty: 100, LimitPrice: &bk.limit,
		})
		return b
	}
	ids := make([]string, orders)
	create := func(i int) error {
		req, _ := http.NewRequest(http.MethodPost, s.URL+"/v1/orders", bytes.NewReader(body(i)))
		req.Header.Set("Authorization", "Bearer "+s.Trader)
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Idempotency-Key", fmt.Sprintf("bench-key-%06d", i))
		resp, err := client.Do(req)
		if err != nil {
			return err
		}
		defer resp.Body.Close()
		data, _ := io.ReadAll(resp.Body)
		if resp.StatusCode != http.StatusCreated {
			return fmt.Errorf("HTTP %d", resp.StatusCode)
		}
		var o struct{ ID string }
		if err := json.Unmarshal(data, &o); err != nil || o.ID == "" {
			return fmt.Errorf("unreadable response")
		}
		if ids[i] != "" && ids[i] != o.ID {
			return fmt.Errorf("replay returned a different order")
		}
		ids[i] = o.ID
		return nil
	}

	// Warm up connections and plans outside the measurement.
	warm := &http.Client{Timeout: 10 * time.Second}
	for i := 0; i < 20; i++ {
		req, _ := http.NewRequest(http.MethodGet, s.URL+"/v1/orders?limit=1", nil)
		req.Header.Set("Authorization", "Bearer "+s.Trader)
		if resp, err := warm.Do(req); err == nil {
			io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
		}
	}

	start := time.Now()
	rep.Create = measure(orders, concurrency, rate, create)

	// Wait for every order to be filled.
	deadline := time.Now().Add(5 * time.Minute)
	for {
		err := s.App.Store.Pool().QueryRow(ctx, `SELECT count(*) FROM orders WHERE state = 'FILLED'`).Scan(&rep.OrdersFilled)
		if err != nil {
			return rep, err
		}
		if rep.OrdersFilled >= orders-rep.Create.Errors || time.Now().After(deadline) {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	rep.SettleSec = time.Since(start).Seconds()
	rep.EndToEndPerSec = float64(rep.OrdersFilled) / rep.SettleSec

	rep.Replay = measure(orders, concurrency, 0, create)
	if err := s.App.Store.Pool().QueryRow(ctx, `SELECT count(*) FROM orders`).Scan(&rep.ReplaysDistinct); err != nil {
		return rep, err
	}

	rep.Read = measure(orders, concurrency, 0, func(i int) error {
		if ids[i] == "" {
			return fmt.Errorf("no order")
		}
		req, _ := http.NewRequest(http.MethodGet, s.URL+"/v1/orders/"+ids[i], nil)
		req.Header.Set("Authorization", "Bearer "+s.Trader)
		resp, err := client.Do(req)
		if err != nil {
			return err
		}
		defer resp.Body.Close()
		io.Copy(io.Discard, resp.Body)
		if resp.StatusCode != http.StatusOK {
			return fmt.Errorf("HTTP %d", resp.StatusCode)
		}
		return nil
	})

	// Pipeline times, from the timestamps the OMS itself recorded.
	rows, err := s.App.Store.Pool().Query(ctx,
		`SELECT created_at, submitted_at, acknowledged_at, closed_at FROM orders WHERE state = 'FILLED'`)
	if err != nil {
		return rep, err
	}
	var dispatch, ack, filled []time.Duration
	for rows.Next() {
		var created time.Time
		var submitted, acked, closed *time.Time
		if err := rows.Scan(&created, &submitted, &acked, &closed); err != nil {
			return rep, err
		}
		if submitted != nil {
			dispatch = append(dispatch, submitted.Sub(created))
			if acked != nil {
				ack = append(ack, acked.Sub(*submitted))
			}
		}
		if closed != nil {
			filled = append(filled, closed.Sub(created))
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return rep, err
	}
	rep.Dispatch, rep.Ack, rep.Filled = summarize(dispatch, 0), summarize(ack, 0), summarize(filled, 0)

	time.Sleep(1500 * time.Millisecond) // let the last changes age past the reconciliation grace window
	t := time.Now()
	recon, err := s.App.Reconciler.Run(ctx, "bench")
	if err != nil {
		return rep, fmt.Errorf("reconciliation: %w", err)
	}
	rep.ReconcileSec, rep.ReconcileDrift = time.Since(t).Seconds(), recon.DriftFound

	t = time.Now()
	v, err := s.App.Store.Verify(ctx)
	if err != nil {
		return rep, fmt.Errorf("verify: %w", err)
	}
	rep.VerifySec, rep.VerifyEvents, rep.VerifyProblems = time.Since(t).Seconds(), v.Events, len(v.Problems)
	rep.VerifyDetails = v.Problems

	stats := s.Sim.Stats()
	rep.BrokerSubmits, rep.BrokerDuplicates, rep.WebhooksSent = stats.SubmitRequests, stats.DuplicateSubmits, stats.WebhooksSent

	var m runtime.MemStats
	runtime.ReadMemStats(&m)
	const mb = 1 << 20
	rep.HeapAllocMB, rep.SysMB, rep.TotalAllocMB = float64(m.HeapAlloc)/mb, float64(m.Sys)/mb, float64(m.TotalAlloc)/mb
	return rep, nil
}

// measure runs fn(0..n-1) across workers and times each call. With a rate,
// call i is due at start + i/rate and its time is taken from when it was
// due, so a slow system cannot hide delay by slowing the load generator.
func measure(n, workers, rate int, fn func(i int) error) latency {
	durations := make([]time.Duration, n)
	var next, errs atomic.Int64
	var wg sync.WaitGroup
	start := time.Now()
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				i := int(next.Add(1)) - 1
				if i >= n {
					return
				}
				t := time.Now()
				if rate > 0 {
					due := start.Add(time.Duration(i) * time.Second / time.Duration(rate))
					time.Sleep(time.Until(due))
					t = due
				}
				if err := fn(i); err != nil {
					errs.Add(1)
				}
				durations[i] = time.Since(t)
			}
		}()
	}
	wg.Wait()
	l := summarize(durations, int(errs.Load()))
	l.WallSec = time.Since(start).Seconds()
	l.Throughput = float64(n) / l.WallSec
	return l
}

func summarize(d []time.Duration, errs int) latency {
	l := latency{Count: len(d), Errors: errs}
	if len(d) == 0 {
		return l
	}
	sort.Slice(d, func(i, j int) bool { return d[i] < d[j] })
	pct := func(p float64) float64 {
		// Nearest rank.
		i := int(p*float64(len(d))+0.999999) - 1
		if i < 0 {
			i = 0
		}
		if i >= len(d) {
			i = len(d) - 1
		}
		return ms(d[i])
	}
	l.P50Ms, l.P95Ms, l.P99Ms, l.MaxMs = pct(0.50), pct(0.95), pct(0.99), ms(d[len(d)-1])
	return l
}

func ms(d time.Duration) float64 { return float64(d.Microseconds()) / 1000 }

func print(r report) {
	fmt.Println("OMS benchmark: SIMULATED broker, single process, loopback HTTP. Measured, not estimated.")
	fmt.Printf("  %s %s, %d CPUs; PostgreSQL %s\n", r.GoVersion, r.Platform, r.CPUs, r.Postgres)
	fmt.Printf("  commit durability: %s\n", r.Durability)
	load := "sent as fast as possible (burst)"
	if r.Rate > 0 {
		load = fmt.Sprintf("released at %d/s", r.Rate)
	}
	fmt.Printf("  %d orders %s, %d concurrent clients, across %d position(s)\n", r.Orders, load, r.Concurrency, r.Positions)
	fmt.Printf("  outbox concurrency %d; simulated broker: %d webhook senders, ack delay %s, fill delay %s\n\n", r.OutboxConc, r.Senders, r.AckDelay, r.FillDelay)

	fmt.Printf("  %-36s %8s %8s %8s %8s %8s %10s\n", "", "p50 ms", "p95 ms", "p99 ms", "max ms", "errors", "per sec")
	row := func(name string, l latency) {
		tput := ""
		if l.Throughput > 0 {
			tput = fmt.Sprintf("%.0f", l.Throughput)
		}
		fmt.Printf("  %-36s %8.2f %8.2f %8.2f %8.2f %8d %10s\n", name, l.P50Ms, l.P95Ms, l.P99Ms, l.MaxMs, l.Errors, tput)
	}
	row("POST /v1/orders (new)", r.Create)
	row("POST /v1/orders (idempotent replay)", r.Replay)
	row("GET /v1/orders/{id}", r.Read)
	row("accepted -> first submit attempt", r.Dispatch)
	row("submit -> broker ack applied", r.Ack)
	row("accepted -> FILLED", r.Filled)

	fmt.Printf("\n  %d of %d orders FILLED %.2fs after the first request (%.0f orders/s end to end)\n",
		r.OrdersFilled, r.Orders, r.SettleSec, r.EndToEndPerSec)
	fmt.Printf("  after replaying every request: %d orders in the database (want %d)\n", r.ReplaysDistinct, r.Orders)
	fmt.Printf("  broker: %d submit requests (%d duplicates), %d webhooks delivered\n", r.BrokerSubmits, r.BrokerDuplicates, r.WebhooksSent)
	fmt.Printf("  reconciliation run over all orders: %.2fs, drift %d\n", r.ReconcileSec, r.ReconcileDrift)
	fmt.Printf("  event-log replay check: %d events in %.2fs, %d problems\n", r.VerifyEvents, r.VerifySec, r.VerifyProblems)
	for _, p := range r.VerifyDetails {
		fmt.Printf("    PROBLEM: %s\n", p)
	}
	fmt.Printf("  process memory (OMS + simulated broker + load generator; PostgreSQL excluded): heap %.1f MB, sys %.1f MB, total allocated %.1f MB\n",
		r.HeapAllocMB, r.SysMB, r.TotalAllocMB)
}
