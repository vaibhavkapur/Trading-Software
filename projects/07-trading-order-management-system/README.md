# Trading Order Management System

An order management service that keeps one authoritative, replayable record of every order from request to final fill, and stays correct when clients retry, the broker repeats or reorders its messages, calls time out, and workers crash.

> **Simulated / paper trading only.** The only broker this talks to is the simulator in this repository, and `omsd` refuses to start against anything that does not identify itself as one. It is an engineering project, not a trading product and not financial advice. Do not connect it to a live account.

The original brief is kept in [docs/SPEC.md](docs/SPEC.md). What was built against it, and what was not, is listed under [Implemented vs. Planned](#implemented-vs-planned).

## What it guarantees

- **A retried request never creates a second order.** Every write takes an `Idempotency-Key`; a repeat returns the first response byte for byte.
- **A fill is counted once.** Redelivered, duplicated and reordered broker reports converge on the same quantities and cash.
- **Filled quantity never exceeds order quantity** unless an operator explicitly accepts the execution, which is recorded as such.
- **Nothing accepted is lost.** The order and the instruction to send it to the broker are committed together; workers can be killed at any point and the work is picked up again, without the broker ever holding two orders for one of ours.
- **Disagreements with the broker are found and shown.** Reconciliation repairs what a missed message explains, and opens an issue for everything else. It never rewrites a booked trade to make the two sides match.
- **History is append-only.** Order state is derived from an event log that the database refuses to update or delete, and `omsd verify` proves the tables still match it.

## Quick start

Needs Go 1.27 or later. Nothing else: the tests, the demo and the local stack start their own throwaway PostgreSQL 17 (downloaded once into `.cache/`).

```sh
make setup       # fetch and verify dependencies, build
make test-all    # lint, unit, integration and scenario tests
make demo        # scripted walkthrough, including the failure demo
```

To run the service and call it yourself:

```sh
make run            # terminal 1: OMS on 127.0.0.1:8080, simulated broker on 127.0.0.1:9090
make walkthrough    # terminal 2: curl through the API, including a failure and its repair
```

`make run` generates credentials for that run only, writes them to `.local/credentials.env` (mode 600, git-ignored, deleted on exit) and does not print them.

With Docker:

```sh
make env    # writes .env with fresh random secrets (git-ignored)
make up     # PostgreSQL 17, the simulated broker and omsd
make walkthrough
make down
```

Other targets: `make bench` (load test), `make build` (binaries into `bin/`), `make golden` (re-record the replay fixture), `make clean`.

## Demo

`make demo` runs eight scenes against a real PostgreSQL and the simulated broker over HTTP, on a scripted clock so the output is the same every time, and exits non-zero if any of its 52 checks fails:

1. An order from request to `FILLED`, with the request retried.
2. Duplicate and out-of-order broker reports.
3. An overfill: quarantined, then acknowledged by an operator.
4. **Failure demo.** The broker goes down, a response is lost, a worker dies holding a message, a second worker takes over, the broker deduplicates the resubmission, and webhooks are dropped.
5. Reconciliation repairs the lost fills, reports an execution mismatch without overwriting it, and closes the issue once the two sides agree.
6. A correction and a bust, taking an order back to `ACKNOWLEDGED` through documented events.
7. Cancel.
8. The event log replayed against the tables, a final clean reconciliation, and the metrics.

## API

All `/v1` calls need `Authorization: Bearer <token>`. Money is a decimal string (`"150.25"`), quantities are whole shares, timestamps are UTC.

| Method and path | Role | |
|---|---|---|
| `POST /v1/orders` | trader | Create. Requires `Idempotency-Key`. `201` with the order; a risk rejection is also `201` with state `REJECTED` |
| `GET /v1/orders/{id}` | viewer | Current state |
| `GET /v1/orders` | viewer | List, for the caller's accounts |
| `GET /v1/orders/{id}/events` | viewer | Full history |
| `GET /v1/orders/{id}/executions` | viewer | Fills, corrections and busts |
| `POST /v1/orders/{id}/cancel` | trader | Requires `Idempotency-Key`. `202` while the broker has yet to confirm, `200` when already final |
| `POST /v1/orders/{id}/replace` | trader | Change quantity or limit price. Requires `Idempotency-Key` |
| `GET /v1/positions` | viewer | Net quantity and cash per account and symbol |
| `POST /v1/reconciliation/runs` | operator | Compare with the broker now |
| `GET /v1/reconciliation/runs/{id}` | operator | |
| `GET /v1/reconciliation/issues`, `/issues/{id}` | operator | The exception queue |
| `POST /v1/reconciliation/issues/{id}/resolve` | operator | `ACKNOWLEDGE`, `ACCEPT_EXECUTION`, `REJECT_UNSUBMITTED`, `REQUEUE_OUTBOX`; a note is required |
| `POST /v1/broker/webhooks` | broker | Authenticated by an HMAC signature, not a token |
| `GET /healthz`, `/readyz`, `/metrics` | none | |

```sh
curl -sS -X POST "$OMS_URL/v1/orders" \
  -H "Authorization: Bearer $TRADER_TOKEN" \
  -H "Idempotency-Key: 6f1c2a9e-order-0001" \
  -H "Content-Type: application/json" \
  -d @examples/requests/create-order.json
```

Errors are `{"error":{"code":"...","message":"..."}}`. A replayed response carries `Idempotent-Replayed: true`. Reusing a key for a different request is `422 IDEMPOTENCY_KEY_REUSED`; reusing a `client_order_id` under a new key is `409 DUPLICATE_CLIENT_ORDER_ID`. An order in an account the caller cannot see is `404`.

Order types are `LIMIT` and `MARKET`; time in force is `DAY`, `GTC` or `IOC`. More request and webhook examples are in [examples/](examples/).

## Order states

```
NEW -> VALIDATED -> SUBMITTING -> ACKNOWLEDGED -> PARTIALLY_FILLED -> FILLED
```

| From | Can also go to |
|---|---|
| `NEW` | `REJECTED` (pre-trade risk) |
| `VALIDATED` | `CANCELLED` (cancelled before it was sent) |
| `SUBMITTING` | `PARTIALLY_FILLED`, `FILLED` (the fill arrived before the acknowledgement), `REJECTED`, `CANCELLED`, `EXPIRED` |
| `ACKNOWLEDGED` | `FILLED`, `REJECTED`, `CANCELLED`, `EXPIRED` |
| `PARTIALLY_FILLED` | `CANCELLED`, `EXPIRED`; back to `ACKNOWLEDGED` by a correction or bust only |
| `FILLED` | Back to `PARTIALLY_FILLED` or `ACKNOWLEDGED` by a correction or bust only |

`CANCELLED`, `REJECTED` and `EXPIRED` are final, and so is `FILLED` unless the broker corrects or busts one of its executions. That is the only backward movement there is, and it is recorded as its own event. A fill that arrives after a cancel is booked and marked late, because the trade happened. The full table, and what happens to every kind of broker report in every state, is in [docs/architecture.md](docs/architecture.md).

## How it is built

One Go binary, `omsd`, with three roles that can run together or as separate processes (`OMS_ROLES`):

| Role | Spec service | What it does |
|---|---|---|
| `api` | command-api | Authenticates, authorises, checks pre-trade risk, and commits each command with its events and outbox message in one transaction. Receives broker webhooks |
| (library) | order-aggregate | `src/domain`: pure functions that decide which events a command or broker report produces, and fold events into state |
| `outbox` | outbox-worker | Leases messages, calls the broker, retries with backoff, dead-letters after a limit |
| (library) | broker-adapter | `src/broker`: the HTTP client and webhook format for the broker; `src/brokersim` is the broker |
| `reconciler` | reconciler | Compares orders and executions with the broker's snapshot on a schedule and on request |

PostgreSQL holds everything: the event log, the tables derived from it, idempotency records, the outbox, the inbox of broker events, and the reconciliation queue.

- [docs/architecture.md](docs/architecture.md): diagrams, write paths, data model, failure behaviour.
- [docs/decisions/0001](docs/decisions/0001-event-log-and-outbox-in-postgres.md): why the event log and outbox are in PostgreSQL, and what that costs.
- [docs/decisions/0002](docs/decisions/0002-reconciliation-repairs.md): what reconciliation may repair by itself.
- [docs/runbook.md](docs/runbook.md): what to watch, what each issue means, how to repair it.
- [docs/test-report.md](docs/test-report.md): test results, requirement-to-test mapping, measured performance.

## Configuration

Everything comes from the environment. `omsd` reports every problem with its configuration at once, without echoing values, and exits.

| Variable | Default | |
|---|---|---|
| `OMS_DATABASE_URL` | required | PostgreSQL connection string |
| `OMS_API_KEYS` or `OMS_API_KEYS_FILE` | required for `api` | Principals as JSON; shape in [examples/api-keys.example.json](examples/api-keys.example.json). Tokens of at least 24 characters |
| `OMS_RISK_LIMITS_FILE` | required for `api` | [examples/risk-limits.json](examples/risk-limits.json) |
| `OMS_BROKER_URL`, `OMS_BROKER_API_KEY` | required | The simulated broker |
| `OMS_BROKER_WEBHOOK_SECRET` | required for `api` | At least 24 characters |
| `OMS_HTTP_ADDR` | `127.0.0.1:8080` | |
| `OMS_ROLES` | `api,outbox,reconciler` | |
| `OMS_BROKER_TIMEOUT` / `OMS_BROKER_WAIT` | `5s` / `30s` | Per call / how long to wait for the broker at start |
| `OMS_OUTBOX_MAX_ATTEMPTS` | `8` | Then dead-letter |
| `OMS_RECONCILE_INTERVAL` / `OMS_RECONCILE_GRACE` | `30s` / `5s` | `0` disables scheduled runs |
| `OMS_DB_MAX_CONNS` | `16` | |
| `OMS_LOG_LEVEL` | `info` | JSON logs on stderr |

`omsd migrate` applies the schema and exits; `omsd verify` checks the tables against the event log. The simulator's variables (`BROKERSIM_*`) are in [docker-compose.yml](docker-compose.yml).

## Metrics

`GET /metrics`, Prometheus text format. The four the brief asks for:

| Asked for | Metric |
|---|---|
| Submit-to-ack latency | `oms_submit_to_ack_seconds` |
| Order-state lag and outbox age | `oms_order_state_lag_seconds`, `oms_outbox_oldest_pending_age_seconds`, `oms_outbox_pending`, `oms_outbox_dead` |
| Duplicate webhook rate | `oms_broker_events_total{outcome="DUPLICATE"}` over `oms_broker_events_total` |
| Reconciliation drift and repair time | `oms_reconciliation_drift_total{kind}`, `oms_reconciliation_auto_repaired_total`, `oms_reconciliation_repair_seconds`, `oms_reconciliation_open_issues{severity}` |

Also HTTP request counts and durations, outbox dispatch results, rejected webhooks, and broker timestamps that are ahead of our clock. The runbook says what to alert on.

## Performance

Measured on one laptop (Apple M5, macOS, PostgreSQL 17.5 on loopback) with commits flushed to storage, with the load generator, OMS and simulated broker in one process. Single runs; a baseline, not a capacity plan. Method and full tables are in the [test report](docs/test-report.md).

| | |
|---|---|
| `POST /v1/orders` at 50 orders/s | p50 6.7 ms, p99 12.8 ms |
| Accepted to fully filled at 50 orders/s (two fills) | p50 22.9 ms, p99 34.8 ms |
| `GET /v1/orders/{id}` | p50 0.25 ms |
| Sustained end to end, all orders in one account and symbol | 104 to 109 orders/s |
| Sustained end to end, spread over eight positions | 483 orders/s |

One heavily traded account and symbol is the slow case, because its fills update one position row in turn; the ADR explains the trade-off.

## Safety

- **Paper only**, enforced at startup as described above.
- **No secrets in the repository.** Credentials come from the environment or a file named by it. `.env.example` and the example key file contain no usable values, `make env` and `make run` generate random ones into git-ignored files, and a test scans every text file in the repository for credentials. Tokens are held in memory only as hashes and are never logged; a test searches the debug logs of both processes for them.
- **Separate boundaries.** Authentication (`src/auth`), authorisation by role and account, pre-trade risk (`src/risk`) and execution (`src/outbox`, `src/broker`) are separate packages called in that order. Risk failing closed means no order: if limits cannot be evaluated the request is refused with 503.
- **Money is integer.** Prices and cash are 64-bit counts of millionths; every multiplication and sum is overflow-checked. No floating point.
- **Time is UTC**, and the time the broker says something happened is stored separately from the time we received it. Ordering never depends on the broker's clock.

## Repository layout

```text
README.md
Makefile
Dockerfile, docker-compose.yml, .env.example
docs/
  SPEC.md              the original brief
  architecture.md
  runbook.md
  test-report.md
  decisions/
src/
  domain/              order state machine: commands, events, decisions (pure)
  fixed/               fixed-point money
  store/               PostgreSQL: event log, projections, idempotency, outbox, inbox, issues, verify
  service/             use cases: create, cancel, replace, ingest broker reports, operator actions
  api/                 HTTP
  auth/, risk/         authentication and authorisation; pre-trade limits
  outbox/              dispatcher
  broker/              broker client and webhook format
  brokersim/           the simulated broker, with fault injection
  reconcile/           reconciliation
  metrics/, clock/, ids/
  app/                 wiring shared by the binaries
  devpg/, localstack/  throwaway PostgreSQL and an in-process stack, for tests and tools only
  cmd/omsd/            the service
  cmd/brokersim/       the simulated broker
  cmd/demo/, cmd/bench/, cmd/devstack/
tests/
  unit/                no database
  integration/         real PostgreSQL, real HTTP, real processes
  scenarios/           failure scenarios, load with faults, deterministic replay
  harness/
examples/              request bodies, webhook payloads, risk limits, key file shape
scripts/               gen-env.sh, api-walkthrough.sh
.github/workflows/ci.yml
```

## Implemented vs. Planned

### Implemented

| From the brief | Status |
|---|---|
| State machine for `NEW`, `VALIDATED`, `SUBMITTING`, `ACKNOWLEDGED`, `PARTIALLY_FILLED`, `FILLED`, `CANCELLED`, `REJECTED`, `EXPIRED` | Done, as a transition table that the tests check against the code in both directions |
| Client idempotency keys and stable client order IDs | Done, for create, cancel and replace |
| Transactional outbox and asynchronous broker dispatch | Done: leases, backoff, dead-letter, per-order ordering, several workers |
| Fill deduplication, correction handling, order projections | Done: fills, corrections, busts, late fills; orders, executions and positions |
| Scheduled reconciliation against broker order and fill snapshots | Done, scheduled and on request, with an exception queue and operator actions |
| The five API endpoints | Done, plus list, executions, replace, positions, issues and the webhook receiver |
| The four metrics | Done |
| Unit, integration, scenario and replay tests | Done; see the test report |
| Performance figures | Measured and reported with their method and limits |
| Demo, failure demo, architecture diagram, ADR, runbook, test report | Done |
| Makefile targets `setup`, `lint`, `test`, `integration-test`, `demo` | Done |

All four definition-of-done items are covered by named tests, listed in the [test report](docs/test-report.md#definition-of-done).

### Different from the suggested stack

| Suggested | Built | Why |
|---|---|---|
| NATS or Kafka | The outbox worker calls the broker over HTTP directly | The outbox table already gives durable, ordered, retried delivery. A bus between the outbox and the broker adds a hop without removing any failure case this has to handle |
| Redis | Idempotency records are in PostgreSQL | They must commit atomically with the order they protect |
| OpenTelemetry | Prometheus text metrics and structured JSON logs | No tracing. See below |
| REST/gRPC | REST only | |

### Not verified

- The container image was not built and `docker compose up` was not run: no Docker daemon was available. The compose file was validated with `docker compose config` only.
- The CI workflow has never run.
- `scripts/gen-env.sh` was exercised with a stand-in for its random generator, not with real `openssl` output.
- Everything ran on macOS/arm64 against PostgreSQL 17.5. Linux and the official `postgres:17` image are untested.

### Planned, not built

- **FIX-style adapter** (stretch goal).
- **Multi-account allocation** (stretch goal).
- **A systematic race-condition suite** (stretch goal). What exists: every suite runs under the race detector, and there are concurrency tests for idempotent creates, competing outbox workers, concurrent reconciliation and load with injected faults. There is no exhaustive interleaving or model-based testing.
- **Distributed tracing** with OpenTelemetry.
- **gRPC.**
- **A real broker adapter.** Deliberately absent; the startup guard would also have to change.
- **Positions updated outside the fill transaction**, to remove the hot-row limit measured above.
- **Event-log snapshots or archiving.** `omsd verify` reads the whole log; fine at this scale, not for years of history.
- **Rate limiting, TLS termination and API-key rotation without a restart.** Expected from a gateway or secret manager in front of the service.
- **Multi-process and multi-host load tests.**
