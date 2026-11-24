# Options Trading Infrastructure

> Option-chain analytics, Greeks, portfolio exposure, and defined-risk paper orders.

**Difficulty:** Advanced  
**Stack:** Python 3.12, FastAPI, Pydantic v2, SQLite, fixed-point `Decimal` arithmetic, Prometheus client, self-contained HTML/SVG workstation  
**Status:** Implemented (paper/simulated MVP + CRR binomial tree stretch goal; 817 tests passing)

## Problem and outcome

A defensible options analytics and paper order-preview system. It prices European vanilla options with an analytic Black–Scholes–Merton model (`r`, continuous dividend yield `q`), inverts implied volatility under European no-arbitrage bounds, optionally quantifies American early-exercise premiums with a Cox–Ross–Rubinstein (CRR) binomial tree, aggregates multiplier-aware portfolio Greeks and 2D spot × vol shock scenarios, and previews/executes defined-risk vertical spreads against a strictly **PAPER / SIMULATED** multi-leg adapter.

## System architecture

```text
Simulated chain / replay fixture (UTC venue asof vs. local received_at)
                      |
     instruments (validation, clock-skew, staleness & disconnect guards)
                      |
     pricing (European BSM + safeguarded IV solver + CRR binomial tree)
                      |
     risk (signed quantity x multiplier Greeks + 2D shock scenario grid)
                      |
     strategy (defined-risk vertical spreads + payoff & assumptions)
                      |
     paper_adapter (broker rules -> pre-trade risk gate -> SQLite ledger)
                      |
          +-----------+-----------+
          |                       |
   FastAPI + Web UI      Prometheus /metrics
```

See [docs/architecture.md](docs/architecture.md), [ADR 0001](docs/decisions/0001-pricing-model-and-decimal-execution-boundary.md), and [docs/runbook.md](docs/runbook.md).

## Explicit non-goals (enforced by construction)

- **Never treats European Black–Scholes as universally accurate for American options:** Pricing an `AMERICAN` contract with European BSM always emits `AMERICAN_PRICED_WITH_EUROPEAN_BSM`; callers can invoke `model="BINOMIAL_CRR"` to estimate the early-exercise premium explicitly.
- **Never ignores dividends, interest, early exercise, or assignment risk:** Continuous risk-free rate `r` and dividend yield `q` are first-class inputs, and every vertical spread preview attaches `PricingAssumptions`, explicit expiry/early-exercise `limitations`, and `PIN_AND_ASSIGNMENT_RISK` / `AMERICAN_SPREAD_EARLY_ASSIGNMENT` warnings.
- **Never routes multi-leg orders without broker and risk checks:** Broker capability validation ([broker_rules.py](src/options_infra/paper_adapter/broker_rules.py)) and pre-trade risk limits ([pre_trade_risk.py](src/options_infra/paper_adapter/pre_trade_risk.py)) are separated from simulated execution ([adapter.py](src/options_infra/paper_adapter/adapter.py)), rejecting unsupported combos, uncovered short legs, invalid tick increments, stale quotes, and excessive clock skew.
- **No real-money execution:** Every order schema enforces `execution_mode = "PAPER_SIMULATED"`.

## Implemented vs. Planned

| Capability | Status | Code | Tests / Docs |
| --- | --- | --- | --- |
| Canonical UTC domain models (`Decimal` money/strikes, `asof` vs. `received_at`) | Implemented | [src/options_infra/domain/models.py](src/options_infra/domain/models.py) | [tests/unit/test_paper_adapter.py](tests/unit/test_paper_adapter.py) |
| Deterministic option-chain simulator, replay fixture & freshness/skew/disconnect guards | Implemented | [src/options_infra/instruments/simulator.py](src/options_infra/instruments/simulator.py), [src/options_infra/instruments/repository.py](src/options_infra/instruments/repository.py) | [examples/fixtures/xyz_chain_2027_01_15.json](examples/fixtures/xyz_chain_2027_01_15.json), [tests/scenarios/test_failure_scenarios.py](tests/scenarios/test_failure_scenarios.py) |
| Analytic European Black–Scholes–Merton pricer & Greeks (`r`, `q`) | Implemented | [src/options_infra/pricing/black_scholes.py](src/options_infra/pricing/black_scholes.py) | [tests/unit/test_pricing_bsm.py](tests/unit/test_pricing_bsm.py) |
| Safeguarded Newton–Raphson / bisection IV solver with no-arbitrage bounds | Implemented | [src/options_infra/pricing/iv_solver.py](src/options_infra/pricing/iv_solver.py) | [tests/unit/test_iv_solver.py](tests/unit/test_iv_solver.py) |
| CRR binomial tree for American early-exercise valuation (stretch) | Implemented | [src/options_infra/pricing/binomial.py](src/options_infra/pricing/binomial.py) | [tests/unit/test_binomial.py](tests/unit/test_binomial.py) |
| Multiplier-aware portfolio Greek aggregation & 2D spot × vol shock scenarios | Implemented | [src/options_infra/risk/aggregator.py](src/options_infra/risk/aggregator.py), [src/options_infra/risk/scenarios.py](src/options_infra/risk/scenarios.py) | [tests/unit/test_risk_and_strategy.py](tests/unit/test_risk_and_strategy.py) |
| Defined-risk vertical spread builder (4 structures) & payoff curves with assumptions | Implemented | [src/options_infra/strategy/vertical_spread.py](src/options_infra/strategy/vertical_spread.py) | [tests/unit/test_risk_and_strategy.py](tests/unit/test_risk_and_strategy.py) |
| Separated broker rules, pre-trade risk gate, SQLite idempotent store & paper adapter | Implemented | [broker_rules.py](src/options_infra/paper_adapter/broker_rules.py), [pre_trade_risk.py](src/options_infra/paper_adapter/pre_trade_risk.py), [store.py](src/options_infra/paper_adapter/store.py), [adapter.py](src/options_infra/paper_adapter/adapter.py) | [tests/unit/test_paper_adapter.py](tests/unit/test_paper_adapter.py), [tests/integration/test_persistence_replay.py](tests/integration/test_persistence_replay.py) |
| FastAPI REST API, Prometheus/JSON metrics & interactive SVG web workstation | Implemented | [src/options_infra/api/app.py](src/options_infra/api/app.py), [src/options_infra/metrics/registry.py](src/options_infra/metrics/registry.py), [src/options_infra/web/index.html](src/options_infra/web/index.html) | [tests/integration/test_api.py](tests/integration/test_api.py) |
| Architecture, ADR 0001, operational runbook, Docker & CI workflow | Implemented | [docs/architecture.md](docs/architecture.md), [ADR 0001](docs/decisions/0001-pricing-model-and-decimal-execution-boundary.md), [docs/runbook.md](docs/runbook.md) | [.github/workflows/ci.yml](.github/workflows/ci.yml), [docker-compose.yml](docker-compose.yml) |
| Volatility surface interpolation & static arbitrage checks | Planned | — | Stretch milestone |
| Delta-hedging simulator with realistic transaction costs | Planned | — | Stretch milestone |
| PostgreSQL persistence backend & optional Rust pricing kernels | Planned | [docker-compose.yml](docker-compose.yml) (Postgres profile scaffold) | Stretch milestone |

## Verified API endpoints

```text
GET  /                                              Interactive PAPER/SIMULATED workstation UI
GET  /health                                        Feed connectivity, symbols, execution mode
GET  /metrics                                       Prometheus text exposition
GET  /v1/metrics                                    JSON summary of pricing/IV/risk metrics
GET  /v1/options/symbols                            Available simulated symbols and expiries
GET  /v1/options/chain?symbol=XYZ&expiry=2027-01-15 Option chain with quotes, IV, Greeks, warnings

POST /v1/options/price
     {"right":"CALL","spot":100,"strike":105,"years":0.25,
      "rate":0.04,"dividend_yield":0.0,"volatility":0.30}

POST /v1/options/iv                                 Invert implied volatility from market price
POST /v1/portfolio/greeks                           Multiplier-aware Greeks + 2D scenario P&L grid
POST /v1/strategies/vertical-spread/preview         Defined-risk spread, payoff curve & limitations
POST /v1/paper/orders                               Idempotent PAPER_SIMULATED multi-leg submission
GET  /v1/paper/orders                               List paper multi-leg orders
GET  /v1/paper/orders/{order_id}                    Fetch paper order by ID
POST /v1/paper/orders/{order_id}/cancel             Cancel resting SUBMITTED paper order
GET  /v1/paper/positions                            Inspect paper portfolio positions
POST /v1/quotes                                     Ingest simulated quote update (sequence/skew checks)
```

## Repository layout

```text
.
├── README.md
├── LICENSE
├── Makefile
├── pyproject.toml
├── Dockerfile
├── docker-compose.yml
├── .github/workflows/ci.yml
├── docs/
│   ├── architecture.md
│   ├── decisions/
│   │   └── 0001-pricing-model-and-decimal-execution-boundary.md
│   └── runbook.md
├── src/options_infra/
│   ├── domain/
│   ├── instruments/
│   ├── pricing/
│   ├── risk/
│   ├── strategy/
│   ├── paper_adapter/
│   ├── metrics/
│   ├── api/
│   └── web/
├── tests/
│   ├── unit/
│   ├── integration/
│   └── scenarios/
├── examples/
│   └── fixtures/
│       └── xyz_chain_2027_01_15.json
└── scripts/
    ├── generate_fixture.py
    ├── demo.py
    └── benchmark.py
```

## Quick start (verified commands)

Requires Python 3.12, `uv`, and `make`. Run from this directory:

```bash
make setup              # Create .venv (Python 3.12) and install editable package + dev tools
make lint               # Run ruff check across src, tests, and scripts
make test               # Run unit tests (pricing, parity, FD Greeks, IV, CRR, spreads, adapter)
make integration-test   # Run API, SQLite restart persistence, replay, and failure scenario tests
make test-all           # Run the full test suite (817 tests)
make demo               # Terminal walkthrough + failure demo (stale quotes, naked short, disconnect)
make benchmark          # Reproducible latency and tracemalloc memory benchmark
make serve              # Start FastAPI server + web UI on http://127.0.0.1:8011
```

## Definition of done

| Requirement | Verification Evidence |
| --- | --- |
| Put-call parity tests pass within numerical tolerance for model assumptions | [tests/unit/test_pricing_bsm.py](tests/unit/test_pricing_bsm.py): 648 parameterized cases across spot, strike, expiry, rate, dividend yield, and volatility assert `abs(residual) < 1e-11` |
| Greeks match finite-difference numerical approximations | [tests/unit/test_pricing_bsm.py](tests/unit/test_pricing_bsm.py): analytic Delta, Gamma, Vega, Theta, and Rho verified against central finite differences across ITM/ATM/OTM calls and puts with and without dividends |
| Position Greeks correctly include direction, quantity, and multiplier | [tests/unit/test_risk_and_strategy.py](tests/unit/test_risk_and_strategy.py): checks signed short scaling (`-3x`), custom contract multipliers (`10` vs. `100`), and exact net-zero cancellation |
| Every payoff graph identifies pricing assumptions and expiry/early-exercise limitations | [src/options_infra/strategy/vertical_spread.py](src/options_infra/strategy/vertical_spread.py), [src/options_infra/web/index.html](src/options_infra/web/index.html), and [tests/unit/test_risk_and_strategy.py](tests/unit/test_risk_and_strategy.py) assert `PricingAssumptions`, `limitations`, and `PIN_AND_ASSIGNMENT_RISK` on all 4 vertical spread structures |

## Verified test and benchmark results

Measured locally on `macOS-26.5.1-arm64` with Python `3.12.15`:

- **Lint (`make lint`):** `ruff check src tests scripts` — 0 errors.
- **Test suite (`make test-all`):** **817 passed** in `0.48s` (`806` unit + `5` integration + `6` failure scenario tests covering missing data, stale quotes, clock skew, out-of-order quotes, feed disconnect/recovery, duplicate quote/order idempotency, and rejected storage writes).
- **Performance (`make benchmark`):**
  - Single-option BSM pricing (10,000 runs): **p50 = 21.75 µs**, **p99 = 28.88 µs**, 0 errors.
  - IV solver across 7 strikes (2,000 runs): **p50 = 193.29 µs**, **p99 = 247.12 µs**, **100.00% convergence**, 0 errors.
  - 10-position portfolio Greeks + 21-cell 2D shock grid (200 runs): **p50 = 14.207 ms**, **p99 = 20.749 ms**, 0 errors.
  - Python heap memory (`tracemalloc`): **544.5 KiB current**, **593.9 KiB peak**.

## Safety, data, and operational boundaries

- **Paper/simulated by default:** No real-money broker connectivity or credential storage is enabled.
- **Fixed-point money arithmetic:** Strikes, quotes, spread debit/credit, max loss/gain, and position cost basis use `Decimal` rather than binary floating-point currency math.
- **Metrics exposed:** `/metrics` and `/v1/metrics` expose pricing reference/parity error, IV solver convergence rate, risk recalculation latency, portfolio Greek exposure, scenario worst/best P&L, and paper order counts by status.
- **No strategy profitability implied:** All simulated chains, spreads, and benchmark metrics are for analytical verification and educational/engineering demonstration only.

## License

Licensed under the [MIT License](LICENSE).
