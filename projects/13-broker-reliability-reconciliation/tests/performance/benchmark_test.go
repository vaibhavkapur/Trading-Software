package performance_test

import (
	"fmt"
	"runtime"
	"sort"
	"testing"
	"time"

	"github.com/trading-software-projects/broker-reliability-reconciliation/src/domain"
	"github.com/trading-software-projects/broker-reliability-reconciliation/src/identity"
	"github.com/trading-software-projects/broker-reliability-reconciliation/src/reconcile"
)

func buildSyntheticInput(numFills int) reconcile.SnapshotInput {
	cutoff := time.Date(2026, 1, 31, 16, 0, 0, 0, time.UTC)
	lFills := make([]domain.LedgerFill, 0, numFills)
	bFills := make([]domain.BrokerFill, 0, numFills)
	symbols := []string{"AAPL", "MSFT", "NVDA", "SPY", "QQQ", "AMZN", "META", "GOOGL", "BRK.B", "JPM"}

	for i := 0; i < numFills; i++ {
		sym := symbols[i%len(symbols)]
		execID := fmt.Sprintf("exec-%06d", i)
		ev := cutoff.Add(-time.Duration(numFills-i) * time.Second)
		rc := ev.Add(1 * time.Second)
		qty := domain.DecimalFromInt(int64(10 + (i % 90)))
		px := domain.MustParseDecimal("150.25")
		fee := domain.MustParseDecimal("0.75")

		// Inject ~2% breaks: missing broker fill, fee mismatch, and late fill
		lFills = append(lFills, domain.LedgerFill{
			ID:            fmt.Sprintf("lf-%06d", i),
			AccountID:     "bench-acct",
			ClientOrderID: fmt.Sprintf("cl-%06d", i),
			BrokerExecID:  execID,
			Symbol:        sym,
			Side:          domain.SideBuy,
			Qty:           qty,
			Price:         px,
			Fee:           fee,
			Currency:      "USD",
			EventTime:     ev,
			ReceiveTime:   rc,
		})

		if i%100 == 0 {
			continue // missing broker fill (1%)
		}
		bf := domain.BrokerFill{
			BrokerExecID:  execID,
			BrokerOrderID: fmt.Sprintf("bo-%06d", i),
			ClientOrderID: fmt.Sprintf("cl-%06d", i),
			AccountID:     "bench-acct",
			Symbol:        sym,
			Side:          domain.SideBuy,
			Qty:           qty,
			Price:         px,
			Fee:           fee,
			Currency:      "USD",
			EventTime:     ev,
			ReceiveTime:   rc,
		}
		if i%100 == 50 {
			bf.Fee = domain.MustParseDecimal("1.50") // fee mismatch (1%)
		}
		if i%200 == 25 {
			bf.ReceiveTime = cutoff.Add(45 * time.Second) // late fill (0.5%)
		}
		bFills = append(bFills, bf)
	}

	return reconcile.SnapshotInput{
		AccountID:   "bench-acct",
		Cutoff:      cutoff,
		Policy:      domain.DefaultPolicy(),
		LedgerFills: lFills,
		BrokerFills: bFills,
		InternalCash: []domain.CashSnapshot{{
			Source:      domain.SourceInternal,
			AccountID:   "bench-acct",
			Currency:    "USD",
			SettledCash: domain.MustParseDecimal("5000000.00"),
			PendingCash: domain.MustParseDecimal("-125000.00"),
			AsOf:        cutoff,
			ReceiveTime: cutoff,
		}},
		BrokerCash: []domain.CashSnapshot{{
			Source:      domain.SourceBroker,
			AccountID:   "bench-acct",
			Currency:    "USD",
			SettledCash: domain.MustParseDecimal("5000000.00"),
			PendingCash: domain.MustParseDecimal("-125000.00"),
			AsOf:        cutoff,
			ReceiveTime: cutoff,
		}},
	}
}

func BenchmarkReconcile1000Fills(b *testing.B) {
	in := buildSyntheticInput(1000)
	engine := reconcile.NewEngine(identity.NewMapper(nil))
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := engine.Reconcile("bench-1k", in); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkReconcile10000Fills(b *testing.B) {
	in := buildSyntheticInput(10000)
	engine := reconcile.NewEngine(identity.NewMapper(nil))
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := engine.Reconcile("bench-10k", in); err != nil {
			b.Fatal(err)
		}
	}
}

// TestPerformanceReport measures real p50/p99 latency and heap usage for 1,000 fills.
func TestPerformanceReport(t *testing.T) {
	const iterations = 50
	const numFills = 1000
	in := buildSyntheticInput(numFills)
	engine := reconcile.NewEngine(identity.NewMapper(nil))

	durations := make([]time.Duration, 0, iterations)
	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)

	var lastIssues int
	for i := 0; i < iterations; i++ {
		start := time.Now()
		res, err := engine.Reconcile("perf-report", in)
		if err != nil {
			t.Fatalf("reconcile error: %v", err)
		}
		durations = append(durations, time.Since(start))
		lastIssues = len(res.Issues)
	}
	runtime.ReadMemStats(&after)

	sort.Slice(durations, func(i, j int) bool { return durations[i] < durations[j] })
	p50 := durations[iterations/2]
	p99Idx := (iterations * 99) / 100
	if p99Idx >= len(durations) {
		p99Idx = len(durations) - 1
	}
	p99 := durations[p99Idx]
	totalAllocMB := float64(after.TotalAlloc-before.TotalAlloc) / (1024 * 1024)

	t.Logf("Perf Report (%s/%s, CPUs=%d, Go=%s): %d fills x %d runs | issues=%d | p50=%s | p99=%s | alloc/run=%.2f MB",
		runtime.GOOS, runtime.GOARCH, runtime.NumCPU(), runtime.Version(),
		numFills, iterations, lastIssues, p50, p99, totalAllocMB/float64(iterations))
}
