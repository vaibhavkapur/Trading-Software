# Test report

Recorded 2026-11-23 on the development machine. Reproduce with the commands shown; numbers will differ on other
hardware. All trading activity in these tests is simulated.

## Environment

- Apple M5 (10 cores), 16 GB RAM, macOS 26.5.1, `go1.27.1 darwin/arm64`
- No third-party dependencies (`go.mod` has no `require` block)

## Results

| Command | Result |
|---|---|
| `make lint` (gofmt, go vet) | pass |
| `make test` (`go test -race ./src/...`) | 31 tests in 7 packages, all pass |
| `make integration-test` (`go test -race ./tests/...`) | 8 tests in 2 packages, all pass |
| `make demo`, `make demo-failure` | pass; replay reports 0 mismatches |
| `docker compose build` | **not verified** — no Docker daemon was running |

Statement coverage across `./src/...` from all tests combined: 81.9%
(`go test -coverpkg=./src/... -coverprofile=c.out ./... && go tool cover -func=c.out`). The CLI `main` package
has no tests and accounts for most of the gap.

## What the tests establish

| Definition-of-done item | Evidence |
|---|---|
| No child fill can make executed quantity exceed the parent quantity | `oms`: `TestReservationBlocksOverRouting`, `TestOverfillReportRefusedAtomically`, `TestTimeoutKeepsReservationUntilLateReport`. `engine`: invariants checked every simulated second in `TestTWAPFillsOnScheduleWithoutOverfill`. `scenarios`: `TestRandomFaultTimelinesKeepInvariants` — 60 random seeds with random link loss, latency, rejects, stale data, clock skew, duplicate reports and kill-switch timing, router and baseline. |
| Routing decisions can be replayed against frozen snapshots | `scenarios`: `TestExampleScenarios` (byte-identical event log on re-run; every decision reproduced by `replay.Verify`), `TestTamperedLogFailsReplay`. `planner`: `TestPlanProperties` (500 random books; decision reproducible from JSON-serialized input). `integration`: log from the live HTTP server replays cleanly. |
| Failed venues are excluded or retried within explicit policy limits | `marketview`: `TestCircuitBreaker`, `TestQuoteFreshnessAndOrdering`. `engine`: `TestBreakerTripsOnTimeoutsAndVenueIsAvoided`, `TestLostReportIsReservedThenReconciled`. `scenarios`: `TestOutageVenueIsNotRoutedWhileDown`, `TestStaleAndSkewedVenuesAreNotRouted`. |
| TCA compares router outcomes against an identical simulated baseline | `venuesim`: `TestMarketIsIndependentOfOrderFlow`. `scenarios`: `TestExampleScenarios` asserts router and baseline saw the same arrival mid and market VWAP and that the baseline traded on one venue only. `tca`: hand-computed figures in `TestBuildBuy`, `TestSellSignsAndEmptyReport`. |

Scenario classes from the README's testing strategy that are covered: missing data (`NO_MARKET_DATA`, stopped
quotes), clock skew, stale and out-of-order events, connection loss, duplicate delivery, rejected orders.
**Not covered:** process restart and recovery (state is in memory only) and a message transport (there is none).

## Benchmarks

`make bench` — Go's `testing.B`, default benchmark time, single run each; indicative only.

| Benchmark | Result |
|---|---|
| `BenchmarkPlan` — one routing decision, 3 venues × 5 levels | 1,075 ns/op, 3,968 B/op, 22 allocs/op |
| `BenchmarkScenario` — one simulated 10-minute TWAP with faults (`examples/venue-outage.json`, router run only: ~18,000 venue book updates, 92 decisions, 117 children) | 4.6 ms/op, 9.2 MB/op, 138,290 allocs/op |

These measure the in-process planner and simulator. They are not latency figures for a networked router: no
p50/p99, network, serialization or load test has been measured, so none is reported.

## Example results (seeded, simulated)

`make compare` — BUY 10,000 by each strategy, implementation shortfall in bps including fees, router versus the
same order sent only to ALPHA through the identical simulation. Positive savings means the router was cheaper.

| Scenario | Seed | Strategy | Time (s) | Router IS | Baseline IS | Savings |
|---|---|---|---|---|---|---|
| calm-twap | 1 | TWAP | 590.0 | 15.59 | 16.11 | 0.52 |
| calm-twap | 1 | LIQUIDITY | 9.8 | 1.05 | 2.09 | 1.04 |
| calm-twap | 2 | TWAP | 590.4 | -3.21 | -2.96 | 0.25 |
| calm-twap | 2 | LIQUIDITY | 9.1 | 0.21 | 0.93 | 0.72 |
| calm-twap | 3 | TWAP | 590.4 | -30.07 | -29.64 | 0.43 |
| calm-twap | 3 | LIQUIDITY | 8.1 | -0.13 | 0.10 | 0.23 |
| calm-twap | 4 | TWAP | 590.0 | -1.33 | -0.62 | 0.71 |
| calm-twap | 4 | LIQUIDITY | 10.3 | 1.65 | 2.32 | 0.68 |
| calm-twap | 5 | TWAP | 590.0 | -10.11 | -9.64 | 0.47 |
| calm-twap | 5 | LIQUIDITY | 8.1 | -0.29 | -0.71 | -0.42 |
| venue-outage | 1 | TWAP | 590.0 | 15.67 | 17.74 | 2.07 |
| venue-outage | 2 | TWAP | 590.4 | -3.10 | -2.20 | 0.90 |
| venue-outage | 3 | TWAP | 590.4 | -29.93 | -30.65 | -0.72 |
| venue-outage | 4 | TWAP | 590.0 | -1.28 | 0.36 | 1.64 |
| venue-outage | 5 | TWAP | 590.0 | -9.90 | -7.15 | 2.75 |

All runs filled 100%. (LIQUIDITY rows for venue-outage equal the calm rows: the order completes before the first
fault at t=120s.)

### Trade-offs these runs show

- **Timing risk dominates TWAP.** Shortfall over a 10-minute TWAP ranges from −30 to +16 bps across seeds, which
  is the simulated price drifting, not routing skill. Routing shows up only in the difference against the
  baseline, which is why the baseline shares the seed.
- **Routing is worth about half a basis point here.** Savings are 0.25–0.71 bps on calm TWAP runs, roughly the
  fee difference between ALPHA (30 mills) and the cheaper venues. The simulated venues quote nearly the same
  prices, so there is little price improvement to find.
- **The router is not always cheaper.** Two of fifteen rows are negative. In the outage run with seed 3 the price fell about 30 bps over the
  order's life, so the baseline, which could not buy for three minutes and caught up later, probably bought lower
  by luck (not verified beyond the totals).
  A five-seed sample is too small to rank the strategies.
- **LIQUIDITY trades timing risk for spread.** It finishes in about 10 seconds with shortfall near 0–2 bps: less
  drift exposure, but it pays the spread on everything at once. This simulator has no permanent market impact,
  which flatters LIQUIDITY; do not read these rows as evidence that fast execution is cheap.

No strategy profitability is implied by any of these figures.
