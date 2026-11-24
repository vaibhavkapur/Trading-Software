# Options Trading Infrastructure

> Option-chain analytics, Greeks, portfolio exposure, and defined-risk paper orders.

**Difficulty:** Advanced  
**Estimated build window:** 6–10 weeks (illustrative, solo development)  
**Suggested stack:** Python, NumPy/SciPy, FastAPI, PostgreSQL, React, optional Rust pricing kernels  
The Python implementation prices simulated option chains, calculates portfolio Greeks, and records paper spread orders. The original design and milestones appear below.

## Quick start

With Python 3.12, uv, and Make installed, run the following commands from this directory.

```bash
make setup UV=uv
make demo
make test-all
```

Run `make serve` to open the API and web interface at `http://127.0.0.1:8011`. The runbook covers service settings and recovery. [Operational runbook](docs/runbook.md)

## Problem and outcome

Build a defensible options analytics and order-preview system. Price European vanilla options using a documented model, compute sensitivities, aggregate portfolio risk, and simulate multi-leg defined-risk strategies before paper execution.

## System architecture

```text
Underlying + options chain
           |
Quotes / instrument validation
           |
IV solver + pricing / Greeks
           |
Portfolio scenario engine
           |
Strategy builder + risk preview
           |
Paper multi-leg order adapter
```

## Minimum viable product (MVP)

- Instrument/option-chain schema with strike, expiration, call/put and multiplier.
- Black–Scholes pricing and implied-volatility solver for European options.
- Delta, Gamma, Vega, Theta and scenario P&L visualization.
- Portfolio aggregated Greeks with contract multipliers.
- Defined-risk vertical spread builder and paper-order preview.

### Explicit non-goals

- Treating European Black–Scholes as universally accurate for American options.
- Ignoring dividends, interest, early exercise and assignment risks.
- Sending multi-leg orders without checking broker support and leg constraints.

## Services and responsibilities

- `instruments`: metadata, expiries and contract specifications.
- `pricing`: analytic models, volatility inversion and tests.
- `risk`: Greek aggregation and shock scenarios.
- `strategy`: spread construction and max-risk preview.
- `paper-adapter`: multi-leg simulation and order lifecycle.

## Domain model (starting point)

```text
OptionContract(id, underlying, expiry, strike, right, multiplier, style)
OptionQuote(contract_id, bid, ask, underlying_px, asof)
GreekSnapshot(contract_id, iv, delta, gamma, vega, theta, model)
OptionPosition(contract_id, quantity, avg_price)
MultiLegOrder(id, legs[], net_debit_credit, max_loss_estimate)
```

Adjust schemas as requirements sharpen. Record timestamps in UTC with venue/event time versus local receive time clearly distinguished, and version decisions/rules where possible.

## API or protocol sketch

These are **illustrative contracts**, not working endpoints.

```text
GET  /v1/options/chain?symbol=XYZ&expiry=2027-01-15
POST /v1/options/price
{"right":"CALL","spot":100,"strike":105,"years":0.25,
 "rate":0.04,"dividend_yield":0.0,"volatility":0.30}

POST /v1/portfolio/greeks
POST /v1/strategies/vertical-spread/preview
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

> This README is a **build specification**, not a claim that the implementation already exists. Replace commands, benchmarks, screenshots and integrations with verified ones as code is added.

## Getting started (implementation checklist)

1. Create the repository using the layout above, choose a package/module name, and pin runtime versions.
2. Add a `Makefile` or task runner with `setup`, `lint`, `test`, `integration-test`, and `demo` targets.
3. Create a local simulator/fixture data source; keep credentials out of source control.
4. Implement the smallest end-to-end vertical slice before optimizing throughput or adding AI.
5. Add CI checks, deterministic replay fixtures, service-level metrics and an architecture decision record.

Example target interface (the targets are **proposed**, not implemented):

```bash
make setup
make lint
make test
make integration-test
make demo
```

## Milestone roadmap

1. Week 1–2: contracts, Black–Scholes/Greeks and numerical verification.
2. Week 3–4: IV solver, quotes and chain UI.
3. Week 5–6: position aggregation, shocks and vertical spread payoff.
4. Week 7+: order preview, model warnings, simulation and documentation.

## Testing strategy

- **Unit:** validation, state transitions, numeric boundaries and all documented invariants.
- **Integration:** API, state store, message transport and simulator under retries/restarts.
- **Scenario:** missing data, clock skew, stale events, connection loss, duplicate delivery and rejected writes.
- **Replay:** fixed fixtures and seeds produce reproducible outputs; pin versions of dependencies and datasets.
- **Performance:** report realistic load shape, machine details, p50/p99 where relevant, errors and memory usage (do not report fabricated metrics).

## Definition of done

- Put-call parity tests pass within numerical tolerance for model assumptions.
- Greeks match finite-difference numerical approximations.
- Position Greeks correctly include direction, quantity and multiplier.
- Every payoff graph identifies pricing assumptions and expiry/early-exercise limitations.

## Metrics to expose

- Pricing error against reference values
- IV solver convergence rate
- Risk recalculation latency
- Scenario P&L and Greek exposure

## Stretch goals

- Binomial tree for American-style exercise.
- Volatility surface interpolation and arbitrage checks.
- Delta-hedging simulation with realistic transaction costs.

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
