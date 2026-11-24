# Trading Order Management System (OMS)

> An event-sourced, idempotent order-lifecycle service with broker reconciliation.

**Difficulty:** Advanced  
**Estimated build window:** 6–9 weeks (illustrative, solo development)  
**Suggested stack:** Go, PostgreSQL, NATS/Kafka, Redis, gRPC/REST, OpenTelemetry, Docker  
**Status:** Project blueprint — not yet implemented

## Problem and outcome

Build an OMS that tracks order state, handles asynchronous acknowledgements and fills, and survives duplicate/out-of-order broker events. Treat the broker as an external system with eventually consistent status rather than a synchronous function call.

## System architecture

```text
Client / Strategy
       |
Risk authorization
       |
OMS command API ---> Postgres outbox
       |                    |
Order aggregate        Dispatcher
       |                    |
       +<---- Broker adapter+----> Broker simulator
       |
Events -> position projections / reconciliation
```

## Minimum viable product (MVP)

- State machine for NEW, VALIDATED, SUBMITTING, ACKNOWLEDGED, PARTIALLY_FILLED, FILLED, CANCELLED, REJECTED and EXPIRED.
- Client idempotency keys and stable client order IDs.
- Transactional outbox + asynchronous broker dispatch.
- Fill deduplication, correction handling and order projections.
- Scheduled reconciliation against broker order/fill snapshots.

### Explicit non-goals

- Assuming the broker is exactly-once.
- Transitioning backwards without a documented correction event.
- Mutating executed trades to hide discrepancies.

## Services and responsibilities

- `command-api`: create/cancel/replace with access controls.
- `order-aggregate`: versioned state transitions and invariants.
- `outbox-worker`: safe dispatch and retries.
- `broker-adapter`: webhook/poll normalization.
- `reconciler`: drift detection, exception queues and repair workflow.

## Domain model (starting point)

```text
Order(id, account_id, client_order_id, symbol, side, qty, state, version)
OrderEvent(id, order_id, source, type, payload, occurred_at)
Execution(id, broker_exec_id, order_id, qty, px, fee)
Outbox(id, aggregate_id, event_type, delivered_at)
ReconciliationIssue(id, order_id, kind, severity, status)
```

Adjust schemas as requirements sharpen. Record timestamps in UTC with venue/event time versus local receive time clearly distinguished, and version decisions/rules where possible.

## API or protocol sketch

These are **illustrative contracts**, not working endpoints.

```text
POST /v1/orders
Idempotency-Key: 9f5e...
{"account_id":"a-1","symbol":"XYZ","side":"BUY",
 "type":"LIMIT","qty":10,"limit_price":99.50}

POST /v1/orders/{id}/cancel
GET  /v1/orders/{id}
GET  /v1/orders/{id}/events
POST /v1/reconciliation/runs
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

1. Week 1: state machine, domain events and Postgres schema.
2. Week 2–3: command API, transactional outbox and simulated broker.
3. Week 4–5: asynchronous fill processing, idempotency, retries and cancels.
4. Week 6+: reconciliation, metrics, operator repair runbook and chaos tests.

## Testing strategy

- **Unit:** validation, state transitions, numeric boundaries and all documented invariants.
- **Integration:** API, state store, message transport and simulator under retries/restarts.
- **Scenario:** missing data, clock skew, stale events, connection loss, duplicate delivery and rejected writes.
- **Replay:** fixed fixtures and seeds produce reproducible outputs; pin versions of dependencies and datasets.
- **Performance:** report realistic load shape, machine details, p50/p99 where relevant, errors and memory usage (do not report fabricated metrics).

## Definition of done

- Repeated API requests with identical idempotency keys do not create multiple orders.
- Duplicate fills do not double-count quantity or cash.
- Total filled quantity never exceeds accepted order quantity absent explicit corrections.
- Restarting workers drains outbox safely and reconciliation identifies discrepancies.

## Metrics to expose

- Submit-to-ack latency
- Order-state lag and outbox age
- Duplicate webhook rate
- Reconciliation drift and repair time

## Stretch goals

- FIX 4.4 adapter against a local simulator.
- Multi-account permissions and trade allocation.
- Replace/cancel race-condition test suite.

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
