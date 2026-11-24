# Cross-Exchange Arbitrage Monitor

> Detect price dislocations after fees, slippage, quote age, and transfer constraints.

**Difficulty:** Advanced  
**Stack:** Go 1.27 (zero external runtime dependencies), exchange public-format adapters (Binance, Coinbase, Kraken), recorded JSONL feeds, Prometheus metrics, optional Docker stack (Redis, PostgreSQL, Grafana)  
**Status:** Implemented (paper/simulated read-only MVP + stretch inventory, off-band transfer cost, and paper adverse-selection diagnostics)

## Problem and outcome

A read-only scanner for cross-exchange crypto price differences. It normalizes exchange-specific symbols, tick/lot precision, and quote currencies using exact `1e8` fixed-point arithmetic, rejects stale/skewed/disconnected quotes, walks multi-level order-book depth to estimate executable opportunity size after taker fees, FX spread, modeled slippage, and pre-funded venue inventory constraints, and persists auditable **paper-only alerts**.

## System architecture

```text
Exchange A book (Binance)  ----+
Exchange B book (Coinbase) ----+--> Sequence validation & Normalizer (top-N fixed-point)
Exchange C book (Kraken)   ----+                    |
FX Oracle                  ----+            Quote-age & skew filter
                                                    |
                                       Volume-aware Net-Edge Calculator
                                       (fees, FX, slippage, venue balances)
                                                    |
                                     Alert Engine + JSONL Evidence Archive
                                       + REST API + Prometheus /metrics
```

See [docs/architecture.md](docs/architecture.md), [docs/decisions/0001-fixed-point-depth-walking-and-freshness.md](docs/decisions/0001-fixed-point-depth-walking-and-freshness.md), and [docs/runbook.md](docs/runbook.md).

## Implemented vs. Planned

| Capability | Status | Code & Tests |
| --- | --- | --- |
| Fixed-point `1e8` decimal arithmetic (zero floats in execution path) | Implemented | [src/fixed/decimal.go](src/fixed/decimal.go), [tests/unit/fixed_test.go](tests/unit/fixed_test.go) |
| Domain model with UTC venue `asof` vs local `received_at` & rule/config versioning | Implemented | [src/domain/types.go](src/domain/types.go) |
| Exchange adapters (Binance, Coinbase, Kraken) + sequence gap/duplicate/disconnect resync state machine | Implemented | [src/connectors/adapters.go](src/connectors/adapters.go), [src/connectors/connector.go](src/connectors/connector.go), [tests/scenarios/scenarios_test.go](tests/scenarios/scenarios_test.go) |
| Instrument registry, conservative tick/lot rounding, top-N truncation, crossed-book rejection | Implemented | [src/normalizer/normalizer.go](src/normalizer/normalizer.go), [tests/unit/normalizer_test.go](tests/unit/normalizer_test.go) |
| Depth-aware cross-book net-edge calculator (taker fees, `min_fee`, FX conversion, modeled & depth slippage, pre-funded inventory caps, separated withdrawal cost estimate) | Implemented | [src/edgecalculator/calculator.go](src/edgecalculator/calculator.go), [tests/unit/edgecalculator_test.go](tests/unit/edgecalculator_test.go) |
| Alert engine (thresholds, cooldowns, TTL expiry, immutable evidence snapshots, durable fsynced JSONL archive) | Implemented | [src/alertengine/engine.go](src/alertengine/engine.go), [src/alertengine/store.go](src/alertengine/store.go), [tests/integration/api_and_store_test.go](tests/integration/api_and_store_test.go) |
| Deterministic replay CLI with SHA-256 digest verification & paper adverse-selection lookahead | Implemented | [src/replay/replay.go](src/replay/replay.go), [src/cmd/arb-replay/main.go](src/cmd/arb-replay/main.go) |
| Read-only REST API (`/v1/opportunities`, `/v1/alerts`, `/v1/venues/health`, `/v1/alerts/config`, `/v1/balances`, `/v1/metrics`, `/metrics`, `/healthz`) | Implemented | [src/api/server.go](src/api/server.go), [src/cmd/arb-monitor/main.go](src/cmd/arb-monitor/main.go) |
| Prometheus & JSON telemetry (gross/net edge bps, histograms, alert staleness/lifetime, disconnects/recovery, executable notional) | Implemented | [src/metrics/metrics.go](src/metrics/metrics.go) |
| Live public WebSocket network transport clients | Planned | Adapter wire decoders are implemented in [src/connectors/adapters.go](src/connectors/adapters.go); live dialers remain disabled by default for offline/paper safety |
| PostgreSQL / Redis storage backends | Planned | `AlertStore` interface in [src/alertengine/store.go](src/alertengine/store.go) and [docker-compose.yml](docker-compose.yml) scaffold are ready; durable JSONL archive is the default |

### Explicit non-goals (enforced)

- Never assumes balances can be teleported between exchanges (`enforce_inventory: true` caps executable size to pre-funded balances; withdrawal cost is separated with `included_in_edge: false`).
- Never treats stale, clock-skewed, or desynchronized quotes as simultaneously executable.
- No live order placement, API signing, or leverage. Every opportunity and alert carries `"Alert: paper estimate, not an executable trade guarantee."`

## Verified API endpoints

```text
GET  /healthz
GET  /v1/opportunities?min_net_bps=5&instrument=BTC/USDT
GET  /v1/alerts?active_only=true&limit=50
GET  /v1/venues/health
GET  /v1/alerts/config
POST /v1/alerts/config
     {"instruments":["BTC/USDT"],"min_net_bps":10,"max_quote_age_ms":750}
GET  /v1/balances
GET  /v1/metrics
GET  /metrics

# Alert: paper estimate, not an executable trade guarantee.
```

## Repository layout

```text
.
├── README.md
├── Makefile
├── go.mod
├── docker-compose.yml
├── .github/workflows/ci.yml
├── docs/
│   ├── architecture.md
│   ├── decisions/
│   │   └── 0001-fixed-point-depth-walking-and-freshness.md
│   └── runbook.md
├── src/
│   ├── fixed/
│   ├── domain/
│   ├── normalizer/
│   ├── connectors/
│   ├── edgecalculator/
│   ├── alertengine/
│   ├── metrics/
│   ├── replay/
│   ├── api/
│   └── cmd/
│       ├── arb-monitor/
│       ├── arb-replay/
│       └── arb-demo/
├── tests/
│   ├── unit/
│   ├── integration/
│   └── scenarios/
├── examples/
│   ├── config/
│   └── fixtures/
└── scripts/
    └── demo.sh
```

## Quick start (verified commands)

```bash
make setup             # Verify Go 1.27 toolchain and module integrity
make lint              # Check gofmt formatting and run go vet ./...
make test              # Run unit tests with -race
make integration-test  # Run HTTP API & durable store integration tests with -race
make scenario-test     # Run failure-mode & deterministic replay scenario tests with -race
make replay            # Run deterministic replay and verify identical SHA-256 digest across 2 runs
make demo              # Run end-to-end simulated terminal walkthrough (normal + failure + replay)
make bench             # Run fixed-point cross-book evaluation benchmarks
make build             # Build bin/arb-monitor, bin/arb-replay, and bin/arb-demo
```

To start the read-only HTTP server locally against recorded fixtures:

```bash
go run ./src/cmd/arb-monitor \
  -addr :8080 \
  -feed examples/fixtures/recorded_books.jsonl \
  -archive data/alerts.jsonl
```

## Verified test & benchmark report

Verified locally on `go1.27.1 darwin/arm64` (Apple M5):

- **Lint (`make lint`):** `gofmt` clean, `go vet ./...` passed.
- **Unit tests (`make test`):** 7/7 tests passed with `-race` (fixed-point math, tick/lot conservative rounding, crossed-book rejection, depth walking, FX conversion, inventory caps, freshness/skew/disconnect/missing-side gates).
- **Integration tests (`make integration-test`):** passed with `-race` (all REST endpoints, partial config updates & validation, Prometheus `/metrics`, durable JSONL archive reload, TTL expiry).
- **Scenario tests (`make scenario-test`):** 3/3 passed with `-race` (missing side, stale events, clock skew, duplicate delivery, sequence gap resync, disconnect/reconnect recovery, rejected store writes, deterministic replay invariant verification).
- **Deterministic replay (`make replay`):** 2/2 runs produced identical SHA-256 alert digest `61b565280741ef598f9c63c00a5e5fc577aede02f0c7bf0ad933bf494e392240` across 14 recorded events (5 paper alerts).
- **Benchmark (`make bench`):**
  - `BenchmarkEvaluateCrossBook-10` (5-level cross-book fixed-point depth walk with fees, slippage, inventory, and transfer estimate): `2999 ns/op`, `6459 B/op`, `132 allocs/op`.

## Safety, data and operational boundaries

- **Paper/simulated by default.** No real-money connectivity or order placement exists in this codebase.
- Keep API keys out of source control, prompts, logs, and sample fixtures.
- All currency, quantity, fee, FX, and edge arithmetic runs in exact `1e8` fixed-point decimals (`src/fixed`), avoiding IEEE-754 floating-point drift.
- Treat all simulated fixtures and paper alerts as illustrative diagnostics; no strategy profitability is implied.

## License

MIT (see repository root before publishing externally).
