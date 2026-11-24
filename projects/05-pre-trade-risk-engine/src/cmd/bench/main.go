// bench drives the engine in-process with a mixed, seeded workload and
// reports measured latency, throughput, decision mix and memory.
// It measures the decision path only: no network, no HTTP parsing.
package main

import (
	"flag"
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"

	"riskengine/src/app"
	"riskengine/src/domain"
	"riskengine/src/fixed"
	"riskengine/src/market"
)

func main() {
	n := flag.Int("n", 200_000, "evaluations")
	workers := flag.Int("workers", 8, "concurrent clients")
	accounts := flag.Int("accounts", 50, "accounts")
	syncEach := flag.Bool("audit-sync", false, "fsync every audit record")
	seed := flag.Int64("seed", 1, "workload seed")
	rate := flag.Int("rate", 0, "target evaluations/s across all workers; 0 = closed loop, as fast as possible")
	flag.Parse()

	dir, err := os.MkdirTemp("", "risk-bench-")
	if err != nil {
		panic(err)
	}
	defer os.RemoveAll(dir)

	symbols := []string{"AAA", "BBB", "CCC", "DDD", "EEE", "FFF", "GGG", "HHH", "III", "JJJ"}
	var accts, syms []string
	for i := 0; i < *accounts; i++ {
		accts = append(accts, fmt.Sprintf(`"acct-%d": {}`, i))
	}
	for _, s := range symbols {
		syms = append(syms, fmt.Sprintf(`%q: {"tradable": true, "lot_size": 1, "tick_size": "0.01"}`, s))
	}
	policy := fmt.Sprintf(`{"version":"bench","defaults":{
  "max_order_qty":500,"max_order_notional":"40000","review_order_notional":"30000",
  "max_symbol_exposure":"150000","max_gross_exposure":"600000","max_daily_loss":"1000000",
  "rate_limit":{"max_orders":1000000,"window_ms":1000},"quote_max_age_ms":2000,"max_clock_skew_ms":500,
  "price_collar_bps":300,"reservation_ttl_ms":30000,"review_ttl_ms":30000},
  "accounts":{%s},"symbols":{%s}}`, strings.Join(accts, ","), strings.Join(syms, ","))
	policyPath := filepath.Join(dir, "policy.json")
	os.WriteFile(policyPath, []byte(policy), 0o600)

	a, err := app.Open(app.Config{PolicyPath: policyPath, DataDir: dir, AuditSync: *syncEach})
	if err != nil {
		panic(err)
	}
	defer a.Close()

	// Simulated feed: every symbol requoted around 100.00 every 50ms.
	px := fixed.MustParse("100")
	feed := func() {
		now := time.Now().Round(0).UTC()
		for _, s := range symbols {
			a.Market.Update(market.Quote{Symbol: s, Bid: px - 100, Ask: px + 100, EventTime: now}, now)
		}
	}
	feed()
	stop := make(chan struct{})
	go func() {
		t := time.NewTicker(50 * time.Millisecond)
		defer t.Stop()
		for {
			select {
			case <-stop:
				return
			case <-t.C:
				feed()
				a.Engine.Sweep()
			}
		}
	}()

	var before runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)

	lat := make([][]time.Duration, *workers)
	mix := make([]map[string]int, *workers)
	var wg sync.WaitGroup
	began := time.Now()
	for w := 0; w < *workers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			rng := rand.New(rand.NewSource(*seed + int64(w)))
			mix[w] = map[string]int{}
			per := *n / *workers
			lat[w] = make([]time.Duration, 0, per)
			var gap time.Duration
			if *rate > 0 {
				gap = time.Duration(float64(time.Second) * float64(*workers) / float64(*rate))
			}
			for i := 0; i < per; i++ {
				side := domain.Buy
				if rng.Intn(2) == 0 {
					side = domain.Sell
				}
				// Mostly near-touch orders; ~4% priced through the collar
				// and ~3% oversized, so rejects are part of the mix.
				price := px + fixed.Decimal(rng.Intn(41)-20)*100
				if rng.Intn(25) == 0 {
					price = px + 50000
				}
				qty := int64(1 + rng.Intn(200))
				if rng.Intn(33) == 0 {
					qty = 600
				}
				req := domain.RiskRequest{OrderID: fmt.Sprintf("w%d-%d", w, i), AccountID: fmt.Sprintf("acct-%d", rng.Intn(*accounts)),
					Symbol: symbols[rng.Intn(len(symbols))], Side: side, Qty: qty, LimitPrice: &price}
				t0 := time.Now()
				if gap > 0 {
					// Open loop: time from the scheduled send, so a slow
					// call delays (and is charged for) the ones behind it.
					// Sleep most of the way, then spin: the OS timer alone
					// overshoots by up to a millisecond, which would be
					// billed to the engine.
					// Workers are staggered so arrivals are evenly spaced
					// rather than landing in bursts of one per worker.
					t0 = began.Add(time.Duration(i)*gap + time.Duration(w)*gap/time.Duration(*workers))
					if d := time.Until(t0) - 2*time.Millisecond; d > 0 {
						time.Sleep(d)
					}
					for time.Now().Before(t0) {
					}
				}
				d := a.Engine.Evaluate(req)
				lat[w] = append(lat[w], time.Since(t0))
				mix[w][string(d.Decision)]++
				if d.Decision != domain.Allow {
					continue
				}
				// Lifecycle outside the timed section: half fill, most of
				// the rest cancel, a few are abandoned to expire.
				switch r := rng.Intn(10); {
				case r < 5:
					a.Engine.Commit(req.OrderID)
					a.Engine.Fill(req.OrderID, "f", qty, price)
				case r < 9:
					a.Engine.Release(req.OrderID, "cancel")
				}
			}
		}(w)
	}
	wg.Wait()
	elapsed := time.Since(began)
	close(stop)

	var after runtime.MemStats
	runtime.ReadMemStats(&after)
	var all []time.Duration
	total := map[string]int{}
	for w := range lat {
		all = append(all, lat[w]...)
		for k, v := range mix[w] {
			total[k] += v
		}
	}
	sort.Slice(all, func(i, j int) bool { return all[i] < all[j] })
	q := func(p float64) time.Duration { return all[int(float64(len(all)-1)*p)] }
	seq, _ := a.Log.Head()
	fi, _ := os.Stat(filepath.Join(dir, "audit.log"))

	fmt.Printf("machine      %s/%s, %d logical CPUs, %s\n", runtime.GOOS, runtime.GOARCH, runtime.NumCPU(), runtime.Version())
	shape := "closed loop (each worker sends its next order as soon as the last returns)"
	if *rate > 0 {
		shape = fmt.Sprintf("open loop at %d/s (latency measured from scheduled send time)", *rate)
	}
	fmt.Printf("workload     %d evaluations, %d workers, %d accounts, %d symbols, seed %d, audit fsync per record: %v\n",
		len(all), *workers, *accounts, len(symbols), *seed, *syncEach)
	fmt.Printf("load shape   %s\n", shape)
	fmt.Printf("decisions    ALLOW %d  REJECT %d  REVIEW %d  (errors: 0 by construction; every call returns a decision)\n",
		total["ALLOW"], total["REJECT"], total["REVIEW"])
	fmt.Printf("latency      p50 %v  p90 %v  p99 %v  p99.9 %v  max %v   (Evaluate call, includes lock wait and audit write)\n",
		q(0.50), q(0.90), q(0.99), q(0.999), all[len(all)-1])
	fmt.Printf("throughput   %.0f evaluations/s wall clock (plus lifecycle calls not counted here)\n", float64(len(all))/elapsed.Seconds())
	fmt.Printf("audit        %d records, %.1f MB\n", seq, float64(fi.Size())/1e6)
	fmt.Printf("memory       heap in use %.1f MB -> %.1f MB, %d GC cycles\n",
		float64(before.HeapInuse)/1e6, float64(after.HeapInuse)/1e6, after.NumGC-before.NumGC)
}
