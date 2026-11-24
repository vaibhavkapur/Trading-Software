# Pre-Trade Risk Engine

> Deterministic order guardrails with configurable limits, circuit breakers, and audit evidence.

**Difficulty:** Advanced  
**Estimated build window:** 5–8 weeks (illustrative, solo development)  
**Suggested stack:** Rust or Go, PostgreSQL, Redis, gRPC, OpenTelemetry, optional OPA/Rego for non-hot-path policy  
**Status:** Project blueprint — not yet implemented

## Problem and outcome

Create a risk decision service that accepts every proposed order and responds ALLOW, REJECT, or REVIEW with stable reason codes. Separate stateful trading limits from read-only reference data, and fail closed when critical inputs are missing.

## System architecture

```text
Order proposal
      |
Input validation + market-data freshness
      |
Account limits + exposure + rate limits
      |
Instrument and session policies
      |
ALLOW / REJECT / REVIEW
      |
Signed decision + append-only audit
```

## Minimum viable product (MVP)

- Per-account max order quantity/notional and symbol concentration.
- Max daily realized loss and open-position exposure.
- Price collars, stale quote checks, duplicate request detection.
- Rate limits, kill switch and kill-switch propagation to OMS.
- Explainable decisions with policy version, input snapshot and reason codes.

### Explicit non-goals

- Guaranteeing safety solely with an LLM.
- Relying on client-side checks for enforcement.
- Treating cached balances as authoritative after they become stale.

## Services and responsibilities

- `policy-loader`: versioned static limits.
- `risk-core`: deterministic evaluation and reason codes.
- `state-store`: reservation/commit model for concurrent orders.
- `market-check`: quote staleness and price collars.
- `audit`: decision inputs, hashes and policy versions.

## Domain model (starting point)

```text
RiskRequest(order_id, account_id, symbol, side, qty, price)
RiskLimit(scope, name, threshold, version)
RiskReservation(order_id, quantity, notional, expires_at)
RiskDecision(order_id, result, reasons[], snapshot_hash)
KillSwitch(scope, enabled, activated_by, activated_at)
```

Adjust schemas as requirements sharpen. Record timestamps in UTC with venue/event time versus local receive time clearly distinguished, and version decisions/rules where possible.

## API or protocol sketch

These are **illustrative contracts**, not working endpoints.

```text
POST /v1/risk/evaluate
{"order_id":"o-123","account_id":"a-1","symbol":"XYZ",
 "side":"BUY","qty":100,"limit_price":105.20}

# Example response
{"decision":"REJECT","reason_codes":["MAX_SYMBOL_EXPOSURE"],
 "policy_version":"2026-01"}

POST /v1/risk/reservations/{order_id}/commit
POST /v1/risk/kill-switches/accounts/a-1/activate
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

1. Week 1: decision model, policies and boundary-case test vectors.
2. Week 2–3: exposure, rate, duplicate, price and freshness checks.
3. Week 4: concurrent order reservations and kill switches.
4. Week 5+: benchmarks, fault handling, replay/audits and OMS integration.

## Testing strategy

- **Unit:** validation, state transitions, numeric boundaries and all documented invariants.
- **Integration:** API, state store, message transport and simulator under retries/restarts.
- **Scenario:** missing data, clock skew, stale events, connection loss, duplicate delivery and rejected writes.
- **Replay:** fixed fixtures and seeds produce reproducible outputs; pin versions of dependencies and datasets.
- **Performance:** report realistic load shape, machine details, p50/p99 where relevant, errors and memory usage (do not report fabricated metrics).

## Definition of done

- Concurrent orders cannot oversubscribe a shared notional limit.
- No decision without a recorded policy version and reason trace.
- Critical missing/stale inputs produce REJECT or REVIEW, not ALLOW.
- Kill switch blocks new orders across all execution entry points.

## Metrics to expose

- Decision p50/p99 latency
- Policy rejects by rule
- Risk-state drift against reconciled broker positions
- Kill-switch propagation delay

## Stretch goals

- Rules-as-code test harness and shadow policy rollouts.
- Hierarchical limits across account, strategy and firm.
- Real-time risk dashboard with what-if previews.

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
