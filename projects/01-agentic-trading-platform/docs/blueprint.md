# Agentic Trading Platform

> A risk-gated, auditable AI trading assistant that turns natural-language intents into paper orders.

**Difficulty:** Advanced  
**Estimated build window:** 6–10 weeks (illustrative, solo development)  
**Suggested stack:** Python 3.12+, FastAPI, Pydantic, PostgreSQL, Redis, MCP-compatible tools, React/Next.js, OpenTelemetry  
**Status:** Project blueprint — not yet implemented

## Problem and outcome

Build an AI-powered trading workflow that can research a symbol, explain a proposed trade, check portfolio exposure, request explicit approval, and route an approved order to a paper broker. The language model **proposes** actions; deterministic services validate and execute them.

## System architecture

```text
User / Web UI
      |
Intent + identity
      v
LLM planner ----> Market-data + portfolio tools
      |
Structured trade proposal
      v
Deterministic risk & policy gate
      |
Human approval (signed, expiring)
      v
OMS -> paper broker adapter -> fills
      |
Audit log / metrics / notifications
```

## Minimum viable product (MVP)

- Read-only market-data, balances, positions, and open-order tools.
- Typed intent-to-order proposal with confidence-independent validation.
- Risk controls for size, exposure, stale quotes, duplicate orders, and trading hours.
- Preview/approve/reject workflow; approvals expire and bind to exact order parameters.
- Paper broker execution, fill ingestion, portfolio refresh, and immutable audit trail.

### Explicit non-goals

- Autonomous real-money trading.
- Promising profits or using unverified LLM price predictions.
- Routing LLM-generated raw API calls directly to the broker.

## Services and responsibilities

- `agent-api`: auth, chat sessions, proposals and approvals.
- `tool-gateway`: allowlisted, read-only market/portfolio tools; separate write permissions.
- `policy-service`: deterministic checks returning structured reasons.
- `oms`: order-state machine and idempotent broker dispatch.
- `audit-service`: append-only event records with correlation IDs.

## Domain model (starting point)

```text
TradeIntent(id, user_id, symbol, side, notional, rationale, created_at)
TradeProposal(id, intent_id, order_type, quantity, limit_price, quote_asof, status)
Approval(id, proposal_hash, approver_id, expires_at, decision)
RiskDecision(id, proposal_id, policy_version, passed, violations)
BrokerOrder(client_order_id, broker_order_id, state, filled_qty)
```

Adjust schemas as requirements sharpen. Record timestamps in UTC with venue/event time versus local receive time clearly distinguished, and version decisions/rules where possible.

## API or protocol sketch

These are **illustrative contracts**, not working endpoints.

```text
POST /v1/intents
{"text":"Buy $500 of SPY using a limit order","mode":"paper"}

POST /v1/proposals/{proposal_id}/evaluate
POST /v1/proposals/{proposal_id}/approve
{"proposal_hash":"sha256:...","decision":"approve"}

POST /v1/orders/{order_id}/submit
GET  /v1/orders/{order_id}
GET  /v1/audit?correlation_id=...
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

1. Week 1–2: broker simulator, tool schemas, session/auth model and quote freshness checks.
2. Week 3–4: intent extraction, proposal schema, deterministic risk/policy service.
3. Week 5–6: approvals, OMS, paper broker execution and end-to-end audit.
4. Week 7+: failure injection, observability, UI and replayable demos.

## Testing strategy

- **Unit:** validation, state transitions, numeric boundaries and all documented invariants.
- **Integration:** API, state store, message transport and simulator under retries/restarts.
- **Scenario:** missing data, clock skew, stale events, connection loss, duplicate delivery and rejected writes.
- **Replay:** fixed fixtures and seeds produce reproducible outputs; pin versions of dependencies and datasets.
- **Performance:** report realistic load shape, machine details, p50/p99 where relevant, errors and memory usage (do not report fabricated metrics).

## Definition of done

- An unapproved or changed order can never execute.
- Every executed order links to a user intent, policy decision and approval.
- Duplicate submits resolve to one logical broker order.
- A stale quote, expired approval or disconnected broker produces a safe rejection.

## Metrics to expose

- Proposal-to-approval latency
- Order-submission success and retry counts
- Policy rejection by reason
- Quote staleness and order reconciliation mismatch

## Stretch goals

- Multiple broker adapters with a canonical order model.
- Policy authoring DSL and simulation mode.
- LLM prompt-injection tests with malicious news/tool output.

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
