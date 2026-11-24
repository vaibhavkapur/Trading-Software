package perf_test

import (
	"fmt"
	"runtime"
	"sort"
	"testing"
	"time"

	"github.com/trading-software-projects/agentic-trading-guardrails/src/pkg/gateway"
	"github.com/trading-software-projects/agentic-trading-guardrails/src/pkg/types"
)

func TestAuthorizePerformanceProfile(t *testing.T) {
	clk := types.NewFixedClock(time.Date(2026, 6, 15, 15, 0, 0, 0, time.UTC))
	gw, err := gateway.New(gateway.Config{Clock: clk})
	if err != nil {
		t.Fatalf("new gw: %v", err)
	}
	_ = gw.SeedDemoIdentities()
	agentTok, _, _ := gw.Identity.IssueToken("agent-1", nil, 15*time.Minute)

	const totalOps = 2000
	latencies := make([]time.Duration, totalOps)
	var memBefore, memAfter runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&memBefore)

	req := gateway.AuthorizeRequest{
		AgentID: "agent-1",
		Action:  types.ScopeOrderPropose,
		Resource: types.ResourceDescriptor{
			AccountID:       "a-1",
			Symbol:          "SPY",
			Side:            types.SideBuy,
			OrderType:       types.OrderTypeLimit,
			Quantity:        1,
			LimitPriceTicks: 510_0000,
			NotionalCents:   51_000,
		},
	}

	var errCount int
	wallStart := time.Now()
	for i := 0; i < totalOps; i++ {
		s := time.Now()
		dec, err := gw.Authorize(agentTok, req)
		latencies[i] = time.Since(s)
		if err != nil || dec.Result != types.DecisionAllow {
			errCount++
		}
	}
	wallElapsed := time.Since(wallStart)
	runtime.ReadMemStats(&memAfter)

	sort.Slice(latencies, func(i, j int) bool { return latencies[i] < latencies[j] })
	p50 := latencies[totalOps*50/100]
	p95 := latencies[totalOps*95/100]
	p99 := latencies[totalOps*99/100]
	throughput := float64(totalOps) / wallElapsed.Seconds()
	allocBytes := memAfter.TotalAlloc - memBefore.TotalAlloc

	if errCount != 0 {
		t.Fatalf("expected 0 errors in perf run, got %d", errCount)
	}
	if err := gw.Audit.VerifyChain(); err != nil {
		t.Fatalf("audit chain broken after perf run: %v", err)
	}

	t.Logf("PERF_REPORT machine=%s/%s cpus=%d go=%s ops=%d wall=%s throughput=%.1f_ops/s p50=%s p95=%s p99=%s errors=%d total_alloc_mb=%.2f",
		runtime.GOOS, runtime.GOARCH, runtime.NumCPU(), runtime.Version(),
		totalOps, wallElapsed.Round(time.Millisecond), throughput, p50, p95, p99, errCount, float64(allocBytes)/(1024*1024))
}

func BenchmarkAuthorizeEndToEnd(b *testing.B) {
	clk := types.NewFixedClock(time.Date(2026, 6, 15, 15, 0, 0, 0, time.UTC))
	gw, err := gateway.New(gateway.Config{Clock: clk})
	if err != nil {
		b.Fatalf("new gw: %v", err)
	}
	_ = gw.SeedDemoIdentities()
	agentTok, _, _ := gw.Identity.IssueToken("agent-1", nil, 15*time.Minute)

	req := gateway.AuthorizeRequest{
		AgentID: "agent-1",
		Action:  types.ScopeOrderPropose,
		Resource: types.ResourceDescriptor{
			AccountID:       "a-1",
			Symbol:          "SPY",
			Side:            types.SideBuy,
			OrderType:       types.OrderTypeLimit,
			Quantity:        1,
			LimitPriceTicks: 510_0000,
			NotionalCents:   51_000,
		},
	}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		req.CorrelationID = fmt.Sprintf("bench-%d", i)
		if _, err := gw.Authorize(agentTok, req); err != nil {
			b.Fatalf("authorize error: %v", err)
		}
	}
}
