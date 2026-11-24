# Multi-Venue Smart Order Router

> Route simulated parent orders across venues using fees, depth, latency, and execution quality.

**Difficulty:** Expert  
**Estimated build window:** 7–11 weeks (illustrative, solo development)  
**Suggested stack:** Go or Rust, PostgreSQL, Redis, Kafka/Redpanda, Python analytics, gRPC, OpenTelemetry  
**Status:** MVP implemented in Go (standard library only), simulated venues only. See [Implemented vs. Planned](#implemented-vs-planned).

> **Simulated / paper only.** Nothing here connects to a real venue, broker or market-data feed.

## Quick start

Requires Go 1.27+ (pinned in `go.mod`). No third-party dependencies, no services to start.

```bash
make setup             # verify toolchain and build
make lint              # gofmt + go vet
make test              # unit tests (race detector)
make integration-test  # HTTP API, seeded scenarios, replay, fault fuzzing
make demo              # BUY 10,000 by TWAP across 3 venues, then replay every routing decision
make demo-failure      # venue outage + degraded venue, router vs single-venue baseline
make compare           # TWAP vs LIQUIDITY across seeds
make serve             # HTTP API on 127.0.0.1:8080
```

Output of `make demo` (seed 7; prices in ticks of 0.01):

```text
=== SIMULATED / PAPER TRADING — scenario "calm-twap" (seed 7) ===
Parent P-000001  BUY 10000 XYZ  strategy=TWAP  status=FILLED
  filled / unfilled           10000 / 0  (100.00% fill rate, 0 unresolved)
  time to completion          590.4s
  avg fill px (ticks)         9988.7000
  arrival mid / market VWAP   9999.0000 / 9988.2800
  fees                        1070.0000 ticks
  implementation shortfall    -10.19 bps  (-1019.30 mills/share, fees included)
  slippage vs arrival / VWAP  -10.30 / 0.42 bps
  effective spread            0.71 bps
  VENUE    CHILDREN  REJECTS  TIMEOUTS  REJECT RATE  FILLED  ALLOC %  FEES (ticks)
  ALPHA    34        0        0         0.00         2600    26.0     780.0000
  BRAVO    52        0        0         0.00         4400    44.0     440.0000
  CHARLIE  39        0        0         0.00         3000    30.0     -150.0000
  vs naive single-venue baseline (ALPHA, identical simulation): baseline filled 10000 (100.00%), shortfall -9.37 bps
  router savings: 82.30 mills/share (0.82 bps); positive = router cheaper
replayed 96 routing decisions against frozen snapshots: 0 mismatches
```

The negative shortfall is the simulated price drifting down during the order, not routing skill; the comparison
against the baseline on the same seed is the meaningful number. No strategy profitability is implied.

Documentation: [architecture](docs/architecture.md) ·
[ADR 0001: reserve until confirmed](docs/decisions/0001-reserve-until-confirmed.md) ·
[runbook and API reference](docs/runbook.md) · [test report, benchmarks and strategy comparison](docs/test-report.md)

## Implemented vs. Planned

| Area | Implemented | Code and tests |
|---|---|---|
| Canonical quote/depth and fee schemas | Yes — integer ticks, mills and lots; venue time vs receive time | [`src/domain`](src/domain/domain.go), [`src/marketview`](src/marketview/view.go) |
| Simulated venues: partial fills, delays, rejects, changing books | Yes — seeded, plus link loss, silent data, clock skew, duplicate reports | [`src/venuesim`](src/venuesim/venue.go), [tests](src/venuesim/venue_test.go) |
| Cost-aware allocation under risk and participation limits | Yes — price + fee + latency and fill-quality penalties, with a per-candidate explanation trace | [`src/planner`](src/planner/planner.go), [tests](src/planner/planner_test.go) |
| TWAP and liquidity-aware strategies | Yes — catch-up `TWAP`; `LIQUIDITY` (top-of-book, participation-capped) | [`src/engine`](src/engine/engine.go), [tests](src/engine/engine_test.go) |
| Execution report | Yes — average fill, fees, shortfall, slippage vs arrival and VWAP, effective spread, unfilled and unresolved quantity, per-venue stats | [`src/tca`](src/tca/tca.go), [tests](src/tca/tca_test.go) |
| Child-order lifecycle, retries, timeouts | Yes — reservation accounting, idempotent reports, bounded replans, reconciliation after reconnect | [`src/oms`](src/oms/oms.go), [tests](src/oms/oms_test.go) |
| Pre-trade risk and kill switch | Yes — symbol, quantity, notional, duration, child size, price collar | [`src/risk`](src/risk/risk.go), [tests](src/risk/risk_test.go) |
| Venue-health circuit breaker (stretch) | Yes — trips on consecutive rejects/timeouts, half-open retry | [`src/marketview`](src/marketview/view.go), [tests](src/marketview/view_test.go) |
| Decision replay | Yes — `sor replay` re-derives every decision from its frozen snapshot | [`src/replay`](src/replay/replay.go), [scenario tests](tests/scenarios/scenarios_test.go) |
| Single-venue baseline on an identical simulation | Yes — CLI and API | [`src/scenario`](src/scenario/scenario.go), [`src/api`](src/api/api.go) |
| HTTP API from the protocol sketch | Yes — plus cancel, kill switch, venue view, manual clock, metrics | [`src/api`](src/api/api.go), [integration tests](tests/integration/api_test.go) |
| Metrics | Partly — Prometheus text counters at `/metrics`; quality metrics are in the TCA report | [`src/api`](src/api/api.go) |
| CI | Written, **not yet run** (this directory is not a git repository) | [`.github/workflows/ci.yml`](.github/workflows/ci.yml) |
| Docker image / compose | Written, **build not verified** | [`Dockerfile`](Dockerfile), [`docker-compose.yml`](docker-compose.yml) |

Planned, not implemented:

- Persistence and restart recovery (PostgreSQL), a message bus (Kafka/Redpanda), Redis, gRPC, OpenTelemetry
  tracing. The suggested stack lists them; the MVP is one in-memory process and needs none of them yet.
- Separate deployable services. The five services are packages in one binary.
- Participation-of-volume scheduling; a true constrained optimizer (allocation is greedy by cost).
- Passive/posted orders and maker rebates, multiple symbols per engine, permanent market impact in the simulator.
- Authentication and authorization on the API (it binds to localhost and must not be exposed).
- Python analytics notebooks; a recorded screen capture.

Deviations from the suggested layout: unit tests sit beside the code they test (`src/<pkg>/*_test.go`, the Go
convention) rather than under `tests/unit/`; `tests/` holds the integration and scenario suites.

## Problem and outcome

Build a venue-agnostic execution router that receives a large parent order and schedules child orders to simulated venues. Prioritize deterministic routing, realistic fill models, and measurable costs rather than theoretical arbitrage profits.

## System architecture

```text
Parent order -> OMS -> Risk checks
                         |
                    Routing planner
                         |
            +------------+------------+
            |            |            |
         Venue A       Venue B       Venue C
            |            |            |
            +------ fill reports -----+
                         |
                 Replanner + TCA
```

## Minimum viable product (MVP)

- Canonical venue quote/depth and fee schemas.
- Simulated venues with partial fills, delays, rejects and changing books.
- Cost-aware child-order allocation subject to risk and participation limits.
- Basic TWAP and liquidity-aware routing strategies.
- Execution report with weighted-average fill, costs, slippage and unfilled quantity.

### Explicit non-goals

- Exchange colocation or guaranteed best execution.
- Accessing fragmented equity venues without appropriate market data and permissions.
- Production routing of client funds.

## Services and responsibilities

- `market-view`: normalized quotes and venue status.
- `route-planner`: schedule/size/venue selection; deterministic inputs.
- `child-oms`: lifecycle and retries for routed child orders.
- `venue-sim`: latency, liquidity and fills under controlled seeds.
- `tca`: arrival-price, VWAP and implementation-shortfall analysis.

## Domain model (starting point)

```text
ParentOrder(id, symbol, side, target_qty, start_time, deadline)
ChildOrder(id, parent_id, venue, qty, limit_px, status)
VenueQuote(venue, symbol, bid, ask, bid_qty, ask_qty, asof)
Fill(id, child_id, qty, px, fee, timestamp)
RouteDecision(parent_id, snapshot_id, policy_version, allocations)
```

Adjust schemas as requirements sharpen. Record timestamps in UTC with venue/event time versus local receive time clearly distinguished, and version decisions/rules where possible.

## API or protocol sketch

These endpoints are implemented; the full list is in the [runbook](docs/runbook.md). `qty` must be a multiple of the lot size (100).

```text
POST /parents
{"symbol":"XYZ","side":"BUY","qty":10000,"strategy":"TWAP","duration_seconds":600}

GET /parents/{id}/children
GET /parents/{id}/fills
GET /parents/{id}/execution-quality
POST /sim/venues/{venue}/faults
{"latency_ms":200,"reject_rate":0.1}
```

## Suggested repository layout

```text
.
├── README.md
├── docs/
│   ├── architecture.md
│   ├── decisions/
│   └── runbook.md
├── src/
├── tests/
│   ├── unit/
│   ├── integration/
│   └── scenarios/
├── examples/
├── scripts/
├── docker-compose.yml
└── .github/workflows/ci.yml
```

> The sections below are the original **build specification**. Where it and the code differ, the [Implemented vs. Planned](#implemented-vs-planned) table is authoritative.

## Getting started (implementation checklist)

1. Create the repository using the layout above, choose a package/module name, and pin runtime versions.
2. Add a `Makefile` or task runner with `setup`, `lint`, `test`, `integration-test`, and `demo` targets.
3. Create a local simulator/fixture data source; keep credentials out of source control.
4. Implement the smallest end-to-end vertical slice before optimizing throughput or adding AI.
5. Add CI checks, deterministic replay fixtures, service-level metrics and an architecture decision record.

These targets are implemented in the [`Makefile`](Makefile):

```bash
make setup
make lint
make test
make integration-test
make demo
```

## Milestone roadmap

1. Week 1–2: venue simulator, canonical market data and parent/child order model.
2. Week 3–4: basic allocation, routing rules and partial fill handling.
3. Week 5–6: TWAP, replanning, risk constraints, kill switch and analytics.
4. Week 7+: compare strategies across seeded market scenarios and document trade-offs.

## Testing strategy

- **Unit:** validation, state transitions, numeric boundaries and all documented invariants.
- **Integration:** API, state store, message transport and simulator under retries/restarts.
- **Scenario:** missing data, clock skew, stale events, connection loss, duplicate delivery and rejected writes.
- **Replay:** fixed fixtures and seeds produce reproducible outputs; pin versions of dependencies and datasets.
- **Performance:** report realistic load shape, machine details, p50/p99 where relevant, errors and memory usage (do not report fabricated metrics).

## Definition of done

- No child fill can cause executed quantity to exceed the parent quantity.
- Routing decisions can be replayed against frozen market snapshots.
- Failed venues are excluded or retried within explicit policy limits.
- TCA compares router outcomes against an identical simulated baseline.

## Metrics to expose

- Fill rate and time-to-completion
- Effective spread and implementation shortfall
- Venue allocation and reject rate
- Cost versus a naive single-venue baseline

## Stretch goals

- Participation-of-volume scheduling.
- Venue-health circuit breakers and adaptive routing.
- Constrained optimizer with route-explanation traces.

## Safety, data and operational boundaries

- **Paper/simulated by default.** No real-money connectivity is enabled by this specification.
- Use only authorized market feeds and broker APIs; comply with applicable market-data licenses, broker terms, and local trading rules before any production deployment.
- Keep API keys in a secret manager or environment injection, never in prompts, logs, sample commits or browser code.
- Separate authentication, authorization, pre-trade risk and order execution into independently testable boundaries.
- Use integer ticks/lot quantities or fixed-point decimals for money and order arithmetic; avoid floating-point currency calculations in the execution path.
- Treat all examples and performance figures as illustrative until supported by test results; no strategy profitability is implied.

## Demo and portfolio deliverables

- A short screen capture or terminal walkthrough using **simulated** data and visibly labeled paper activity.
- An architecture diagram and brief ADR explaining one difficult engineering trade-off.
- A failure demo showing how the system behaves when a dependency disconnects or data is stale.
- A test report with pass/fail evidence and a reproducible benchmark methodology where applicable.
- A clear section titled **Implemented vs. Planned** once development begins, with links to code and tests.

## License

Choose a license before publishing; MIT or Apache-2.0 are common for open-source software, subject to dependency and data-license compatibility.
