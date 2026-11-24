# Agentic Trading Platform

> A risk-gated, auditable AI trading assistant that turns natural-language intents into paper orders.

**Status:** MVP implemented and tested. Paper trading against a built-in simulator only.  
**Stack:** Python 3.12, FastAPI, Pydantic v2, SQLite, Prometheus client, optional Claude planner (Anthropic SDK), vanilla-JS web UI  
**Original build specification:** [docs/blueprint.md](docs/blueprint.md)

The language model **proposes** actions; deterministic services validate and execute them.
There is no real-money connectivity anywhere in this repository, and no strategy
profitability is implied. All prices are invented fixture values.

## Quickstart

Requires Python 3.12+.

```bash
make setup              # create .venv, install dependencies
make demo               # deterministic terminal walkthrough, including failure cases
make run                # API + web UI at http://127.0.0.1:8000  (OpenAPI at /docs)
```

In the UI, paste the dev API key `dev-local-key` into the key field, type
`Buy $500 of SPY using a limit order`, and step through propose, risk check, approve and
submit. The "Simulator faults" panel lets you stall quotes or take the broker down before
submitting. `examples/walkthrough.sh` and `examples/failure_demo.sh` do the same over curl.

```bash
make lint               # ruff check + format check
make test               # unit tests
make integration-test   # API, restart/replay and failure-scenario tests
make report             # full run with pass/fail evidence in reports/
```

The Makefile sets `PYTHONPATH=src`. If you call `pytest` or `python -m atp` directly, the
editable install normally covers that; on macOS folders synced by iCloud the `.pth` file
can be flagged hidden and skipped by Python, in which case set `PYTHONPATH=src` yourself.

### Planner

With no credentials the planner is a deterministic rule parser, so everything works
offline. Set `ANTHROPIC_API_KEY` to have Claude (`claude-opus-5-5`, structured output)
extract the intent instead; it falls back to the rule parser if the API call fails.
Either way the planner's output is untrusted input to the same deterministic pipeline.

## How it works

```text
User / Web UI
      |  intent + identity (API key)
      v
Planner (Claude or rules) ---> typed ParsedIntent, nothing else
      |
Deterministic validation + sizing, quotes via the read-only tool gateway
      v
TradeProposal + sha256 hash of every order parameter
      |
Policy gate (pure function) ---> RiskDecision with structured reasons
      |
Human approval: HMAC-signed, expires in 120s, bound to the proposal hash
      v
OMS: verify approval -> policy gate again on fresh data -> idempotent dispatch
      |
Paper broker -> fills -> portfolio
      |
Append-only, hash-chained audit log / Prometheus metrics
```

More detail, trust boundaries and the invariant-to-test map are in
[docs/architecture.md](docs/architecture.md). The hardest trade-off, what to do when a
broker submit fails without telling you whether it landed, is written up in
[ADR 0001](docs/decisions/0001-idempotent-dispatch-under-ambiguous-failure.md).
Operations are in [docs/runbook.md](docs/runbook.md).

## API

All `/v1` endpoints require `Authorization: Bearer <key>`.

```text
POST /v1/intents                       {"text": "Buy $500 of SPY using a limit order", "mode": "paper"}
GET  /v1/proposals/{proposal_id}
POST /v1/proposals/{proposal_id}/evaluate
POST /v1/proposals/{proposal_id}/approve   {"proposal_hash": "sha256:...", "decision": "approve" | "reject"}
POST /v1/orders/{order_id}/submit      idempotent; 503 + retryable when the broker is unreachable
GET  /v1/orders/{order_id}             also pulls new fills from the broker
POST /v1/orders/{order_id}/cancel
GET  /v1/orders
GET  /v1/portfolio | /v1/quotes/{symbol} | /v1/tools
GET  /v1/audit?correlation_id=...      GET /v1/audit/verify
GET|POST /v1/sim/faults                dev only: simulator fault injection
GET  /metrics | /healthz
```

A submit that is refused returns `200` with `order.state = "rejected"` and a
`reject_reason` such as `APPROVAL_EXPIRED` or `RISK:QUOTE_STALE`. Money is serialised as
decimal strings.

## Definition of done

| Requirement | Evidence |
|---|---|
| An unapproved or changed order can never execute | [tests/scenarios/test_definition_of_done.py](tests/scenarios/test_definition_of_done.py), section 1: forged approvals, nine direct database tamper cases, cross-user access |
| Every executed order links to a user intent, policy decision and approval | `test_executed_order_links_intent_decision_and_approval` joins all four and checks audit ordering |
| Duplicate submits resolve to one logical broker order | Section 3: repeats, 24 concurrent submits, lost acknowledgement, crash mid-dispatch |
| A stale quote, expired approval or disconnected broker produces a safe rejection | Section 4, plus the same faults over HTTP in [tests/integration/test_api.py](tests/integration/test_api.py) |

Latest local run: 133 tests passing (62 unit, 71 integration and scenario) on Python 3.12,
macOS. `make report` regenerates the evidence. No performance benchmarks have been run, so
none are reported.

## Implemented vs. Planned

### Implemented

| Area | Where |
|---|---|
| Typed intent-to-proposal pipeline with untrusted-planner validation and deterministic sizing | [src/atp/service.py](src/atp/service.py), [src/atp/planner.py](src/atp/planner.py) |
| Read-only tool gateway (quotes, balances, positions, open orders) with scopes and audited calls | [src/atp/tools.py](src/atp/tools.py) |
| Policy gate: size, exposure, buying power, no shorting, stale or skewed quotes, price band, duplicates, trading hours, paper-only | [src/atp/policy.py](src/atp/policy.py), [tests/unit/test_policy.py](tests/unit/test_policy.py) |
| Preview / approve / reject with signed, expiring approvals bound to exact parameters | [src/atp/approvals.py](src/atp/approvals.py) |
| Order state machine, idempotent dispatch with retry and reconciliation, fill ingestion, cancel | [src/atp/oms.py](src/atp/oms.py) |
| Paper broker with partial fills, resting orders and fault injection | [src/atp/broker/paper.py](src/atp/broker/paper.py) |
| Deterministic simulated market data with stall, outage, skew and shock faults | [src/atp/marketdata.py](src/atp/marketdata.py) |
| Append-only hash-chained audit log with correlation IDs | [src/atp/audit.py](src/atp/audit.py) |
| The four metrics groups from the spec | [src/atp/metrics.py](src/atp/metrics.py) |
| HTTP API, API-key auth, web UI | [src/atp/api.py](src/atp/api.py), [src/atp/web/index.html](src/atp/web/index.html) |
| Deterministic replay (fixed clock, seed and IDs give an identical audit log) | [tests/integration/test_restart_and_replay.py](tests/integration/test_restart_and_replay.py) |
| Prompt-injection tests using a scripted hostile planner | [tests/scenarios/test_lifecycle_and_injection.py](tests/scenarios/test_lifecycle_and_injection.py) |
| CI workflow, Dockerfile, compose file | [.github/workflows/ci.yml](.github/workflows/ci.yml) |

### Differences from the original blueprint

- **SQLite, not PostgreSQL and Redis.** One process, one file, no services to run. The
  idempotency guarantees rest on uniqueness constraints, not on the in-process lock.
- **One process, not five deployables.** The five services are separate modules with
  explicit interfaces.
- **Plain HTML/JS UI, not React/Next.js.**
- **Prometheus metrics only.** No OpenTelemetry tracing.
- **Tools are in-process functions**, not an MCP server.

### Planned / not done

- Background reconciler for orders left in `submitting` (today a read or retry triggers it).
- A Claude planner that researches through the tool gateway before proposing. Today it
  makes one extraction call and writes no analysis.
- Live-model prompt-injection evaluation. Current tests script the planner's output.
- PostgreSQL store, multi-process deployment, real authentication and approver roles.
- Additional broker adapters, market holiday calendar, fractional shares, order types
  beyond day limit and market.
- Policy authoring DSL and a policy simulation mode.
- Load testing and a benchmark methodology.

### Not verified

- The Claude planner path has not been exercised against the live API in this
  environment (no credentials were available); it is covered only by the fallback logic.
- The Docker image and compose file have not been built here.
- The web UI is served and its script parses, but it has not been click-tested in a browser.
- The CI workflow has not run on GitHub.

## Explicit non-goals

- Autonomous real-money trading.
- Promising profits or using unverified LLM price predictions.
- Routing LLM-generated raw API calls directly to the broker.

## Safety, data and operational boundaries

- **Paper/simulated by default and only.** `mode` values other than `paper` are refused at
  the API and again by the policy gate.
- API keys and the approval-signing secret come from the environment. Outside `ATP_ENV=dev`
  the server refuses to start without them, and the fault-injection endpoints do not exist.
  The dev key `dev-local-key` is a public placeholder; do not expose a dev server.
- Authentication, pre-trade risk, approval and execution are separate modules, each tested
  on its own.
- Money and prices use `Decimal` end to end; quantities are whole shares.
- Before connecting any real feed or broker: comply with market-data licences, broker terms
  and local trading rules, and replace the authentication model.

## Repository layout

```text
src/atp/            application package (api, service, planner, tools, policy, approvals, oms, broker/, audit, web/)
tests/unit/         pure-logic tests: policy boundaries, hashing, state machine, approvals, audit
tests/integration/  HTTP contract, restart and replay tests
tests/scenarios/    definition-of-done and failure-injection scenarios
docs/               architecture, ADRs, runbook, original blueprint
examples/           curl walkthroughs against a running server
scripts/            offline audit-chain verifier
```

## License

Not yet chosen. Pick one before publishing; MIT or Apache-2.0 are common, subject to
dependency and data-licence compatibility.
