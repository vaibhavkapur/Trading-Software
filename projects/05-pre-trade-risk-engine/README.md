# Pre-Trade Risk Engine

> Deterministic order guardrails with configurable limits, circuit breakers, and audit evidence.

A risk decision service that takes every proposed order and answers **ALLOW**, **REJECT** or **REVIEW** with stable reason codes, a policy version, a full reason trace and a signed, hash-chained audit record. It fails closed: missing policy, missing or stale market data, or an unavailable audit log all produce REJECT, never ALLOW.

**Paper/simulated use only.** Nothing here connects to a broker or a venue. Quotes, fills and the OMS in the demo and tests are simulated.

Written in Go 1.27 with no third-party dependencies. The original build specification is kept in [docs/SPEC.md](docs/SPEC.md).

## Quick start

```bash
make setup             # check toolchain, build
make test-all          # lint + unit + integration + scenario tests (race detector on)
make demo              # simulated walkthrough over real HTTP, including failure cases
make bench             # in-process load test, prints measured numbers
```

Run the server:

```bash
export RISK_ADMIN_TOKEN=...   # kill switches, approvals, policy reload, audit verify
export RISK_FEED_TOKEN=...    # quote ingest
make run                      # listens on 127.0.0.1:8080, state in ./data

curl -X POST localhost:8080/v1/market/quotes -H "Authorization: Bearer $RISK_FEED_TOKEN" \
  -d "{\"symbol\":\"XYZ\",\"bid\":105.18,\"ask\":105.22,\"event_time\":\"$(date -u +%Y-%m-%dT%H:%M:%SZ)\"}"

curl -X POST localhost:8080/v1/risk/evaluate \
  -d '{"order_id":"o-123","account_id":"a-1","symbol":"XYZ","side":"BUY","qty":100,"limit_price":105.20}'
```

```json
{"order_id":"o-123","decision":"ALLOW","reason_codes":[],"policy_version":"2026-01",
 "policy_hash":"…","snapshot_hash":"…","decided_at":"…","trace":[…],
 "reservation":{"status":"RESERVED","remaining":100,"notional":"10520.0000","expires_at":"…"},
 "audit_seq":2,"record_hash":"…","signature":"…"}
```

A route whose token is not configured is disabled (403), not open.

## How a decision is made

```text
Order proposal
   │  1 input validation            INVALID_*            (malformed JSON → 400 MALFORMED_REQUEST)
   │  2 duplicate detection         DUPLICATE_ORDER_ID   (identical redelivery → original decision, replayed:true)
   │  3 reference data              POLICY_UNAVAILABLE, UNKNOWN_ACCOUNT, UNKNOWN_SYMBOL
   │  4 kill switches               KILL_SWITCH_GLOBAL / _ACCOUNT / _SYMBOL
   │  5 instrument + session        INSTRUMENT_NOT_TRADABLE, LOT_SIZE_VIOLATION, TICK_SIZE_VIOLATION, SESSION_CLOSED
   │  6 market data                 NO_MARKET_DATA, STALE_QUOTE, QUOTE_CLOCK_SKEW, PRICE_COLLAR
   │  7 order size                  MAX_ORDER_QTY, MAX_ORDER_NOTIONAL
   │  8 rate limit                  RATE_LIMIT
   │  9 daily realized loss         MAX_DAILY_LOSS
   │ 10 exposure                    MAX_SYMBOL_EXPOSURE, MAX_GROSS_EXPOSURE
   │ 11 review triggers             REVIEW_ORDER_NOTIONAL, EXPOSURE_MARK_STALE
   ▼
ALLOW / REJECT / REVIEW  →  audit record appended  →  reservation applied  →  signed response
```

Stages 1–4 short-circuit; stages 5–11 are all evaluated so a reject lists every rule it broke. Any REJECT reason beats any REVIEW reason. If the audit record cannot be written the answer is `REJECT [AUDIT_UNAVAILABLE]` and nothing is reserved.

Rules worth knowing before relying on it:

- **Exposure is worst case.** Per symbol: position at the quote mid, plus *either* all open buys *or* all open sells filling, whichever is larger. Resting sells do not earn headroom for buys. Gross is the sum across symbols.
- **Reduce-only is always possible.** Exposure limits only block orders that increase exposure, so an account pushed over its limit by a price move can still trade down.
- **Daily loss blocks everything** for that account, including reducing orders, until the next UTC day. That is a deliberate simplification; see Planned.
- **The collar is symmetric** around the quote mid, per account with a per-symbol override.
- **Decisions are final per `order_id`.** Resubmitting a rejected id returns the same REJECT; use a new id.
- **Limit orders only.** `limit_price` is required.

## Reservation lifecycle

```text
evaluate ──ALLOW──▶ RESERVED ──commit──▶ COMMITTED ──fills──▶ RELEASED (FILLED)
    │                  │ lease lapses / kill switch      │ release (cancel, venue reject)
    │                  ▼                                 ▼
    └─REVIEW──▶ PENDING_REVIEW ──approve──▶ RESERVED    RELEASED
```

The OMS calls `commit` immediately **before** sending the order. Commit is the last checkpoint: it fails with 409 if the lease has expired (`reservation_ttl_ms`) or a kill switch has come on, and in both cases the capacity is released and the order must not be sent. Committed reservations never expire; they end by fills or `release`. Fills are facts and are always booked to the position, even with no reservation behind them (flagged `unreserved` and counted).

## API

| Route | Auth | Purpose |
| --- | --- | --- |
| `POST /v1/risk/evaluate` | – | Decide an order |
| `GET /v1/risk/decisions/{order_id}` | – | Recorded decision (trace is in the audit record) |
| `POST /v1/risk/reservations/{order_id}/commit` | – | Confirm the order is about to go live |
| `POST /v1/risk/reservations/{order_id}/fills` | – | `{"fill_id","qty","price"}`; idempotent per `fill_id` |
| `POST /v1/risk/reservations/{order_id}/release` | – | Free remaining capacity; idempotent |
| `POST /v1/risk/reservations/{order_id}/approve` | admin | `{"approved_by"}`; REVIEW → RESERVED |
| `POST /v1/risk/kill-switches/global/{activate\|deactivate}` | admin | `{"activated_by","reason"}` |
| `POST /v1/risk/kill-switches/accounts/{id}/{activate\|deactivate}` | admin | |
| `POST /v1/risk/kill-switches/symbols/{symbol}/{activate\|deactivate}` | admin | |
| `GET /v1/risk/kill-switches` | – | Active switches and per-subscriber propagation status |
| `GET /v1/risk/accounts/{id}` | – | Positions, realized P&L today, open reservations |
| `POST /v1/risk/reconcile` | – | `{"account_id","positions":{sym:qty}}` → drift report |
| `POST /v1/market/quotes` | feed | `{"symbol","bid","ask","event_time"}` |
| `POST /v1/policy/reload` | admin | Re-read the policy file (also `SIGHUP`) |
| `GET /v1/audit/head`, `GET /v1/audit/verify` | –, admin | Chain head + public key; full chain verification |
| `GET /metrics`, `GET /healthz` | – | Prometheus text; readiness |

Prices and notionals are fixed-point with four decimal places. Requests may send them as JSON numbers or strings; they are parsed from the literal text, never through a float, and anything needing rounding (`1e2`, five decimals) is rejected. Responses return them as strings. Unknown JSON fields are rejected.

The trader-facing routes carry no authentication of their own: authn/authz is a separate boundary, and this service is meant to sit behind it on a private network.

## Policy

[examples/policy.json](examples/policy.json): `defaults` plus per-account overlays, per-symbol reference data, an optional UTC session window, and a `version`. Every decision records the version and the SHA-256 of the file it was made under. A policy with an unknown field, a missing or non-positive limit, or an out-of-range value fails to load as a whole, and a failed reload leaves the previous policy in force. Accounts and symbols not listed are rejected.

## Implemented vs. Planned

### Implemented

| Spec item | Where | Evidence |
| --- | --- | --- |
| Max order qty / notional, symbol concentration | [src/core/engine.go](src/core/engine.go) | [tests/unit/limits_test.go](tests/unit/limits_test.go) |
| Max daily realized loss, open-position exposure | [src/core/engine.go](src/core/engine.go), [src/state/state.go](src/state/state.go) | [limits_test.go](tests/unit/limits_test.go), [state_test.go](tests/unit/state_test.go) |
| Price collars, stale-quote checks | [src/market/market.go](src/market/market.go) | [market_test.go](tests/unit/market_test.go) |
| Duplicate request detection | [src/core/engine.go](src/core/engine.go) | `TestDuplicateDelivery` |
| Rate limits | [src/core/engine.go](src/core/engine.go) | `TestRateLimitSlidingWindow` |
| Kill switch (global / account / symbol) and propagation to OMS | [src/core/engine.go](src/core/engine.go), [src/notify/notify.go](src/notify/notify.go) | `TestKillSwitchBlocksEveryEntryPoint`, `TestKillSwitchIsImmediateUnderLoad`, `TestKillSwitchPropagatesToOMSWithRetries` |
| Explainable decisions: policy version, input snapshot, reason codes | [src/domain/domain.go](src/domain/domain.go) | `TestEveryDecisionIsExplained` |
| `policy-loader`: versioned static limits | [src/policy/policy.go](src/policy/policy.go) | [policy_test.go](tests/unit/policy_test.go), `TestPolicyReloadIsAtomicAndVersioned` |
| `state-store`: reservation/commit model | [src/state/state.go](src/state/state.go) | `TestConcurrentOrdersCannotOversubscribe`, `TestReservationLeaseExpiry` |
| `audit`: inputs, hashes, policy versions; signed, append-only | [src/audit/audit.go](src/audit/audit.go) | [audit_test.go](tests/unit/audit_test.go), `TestEngineRefusesToStartOnTamperedLogOrBadPolicy` |
| Restart recovery by audit-log replay | [src/app/app.go](src/app/app.go) | `TestRestartRebuildsStateFromAuditLog` |
| Deterministic replay fixture | [tests/scenarios/testdata/](tests/scenarios/testdata/) | `TestRecordedSessionReplaysIdentically` |
| Metrics: decision latency histogram, rejects by rule, state drift vs broker, kill-switch propagation delay | [src/metrics/metrics.go](src/metrics/metrics.go) | `TestReconciliationDriftAndMetrics` |
| Fixed-point money, no floats in the decision path | [src/fixed/fixed.go](src/fixed/fixed.go) | [fixed_test.go](tests/unit/fixed_test.go) |
| `make setup / lint / test / integration-test / demo`, CI | [Makefile](Makefile), [.github/workflows/ci.yml](.github/workflows/ci.yml) | |

Definition of done, each backed by a test:

- Concurrent orders cannot oversubscribe a shared notional limit — `TestConcurrentOrdersCannotOversubscribe`, `TestReserveReleaseChurnNeverExceedsLimit`.
- No decision without a recorded policy version and reason trace — `TestEveryDecisionIsExplained`, `TestAuditOutageFailsClosed`.
- Critical missing/stale inputs produce REJECT or REVIEW, not ALLOW — `TestNoPolicyMeansNoTrading`, `TestMarketDataLossAndRecovery`, `TestStaleMarkOnAnotherSymbolForcesReview`, `TestClockSkew`.
- Kill switch blocks new orders across all execution entry points — `TestKillSwitchBlocksEveryEntryPoint`, `TestOMSUnreachableDoesNotWeakenKillSwitch`.

### Different from the suggested stack

| Spec suggested | What was built | Why |
| --- | --- | --- |
| PostgreSQL + Redis | In-process state, rebuilt from the audit log | Makes check-and-reserve atomic without distributed locking; see [ADR 0001](docs/decisions/0001-single-writer-event-sourced-state.md) |
| gRPC | HTTP/JSON | Matches the spec's own protocol sketch; zero dependencies |
| OpenTelemetry | Prometheus text metrics, no tracing | |
| `src/` only | Go packages under `src/`, black-box tests under `tests/` | As in the suggested layout |

### Planned (not built)

- High availability. This is a single process and a single point of failure; when it is down, nothing is approved. Replication or a standby is the largest gap.
- Audit log rotation, snapshots and archival. The log grows without bound (about 2.6 KB per record on average in the benchmark) and restart time grows with it.
- Market orders; reduce-only exemption from the daily-loss block; unrealized P&L in the loss limit.
- Hierarchical limits (account → strategy → firm), shadow policy rollouts, rules-as-code harness, what-if dashboard.
- Authentication and per-caller authorization on the trader-facing routes; real identity for `activated_by` / `approved_by` (currently caller-asserted behind a shared admin token).
- Automatic correction from reconciliation (today it reports drift and changes nothing).
- gRPC transport, OpenTelemetry tracing, PostgreSQL/Redis backends.
- The Docker image and compose file are written but **have not been built or run** (no Docker daemon was available when this was developed).
- A license. None has been chosen; pick one before publishing.

## Documentation

- [docs/architecture.md](docs/architecture.md) — components, data flow, invariants
- [docs/decisions/0001-single-writer-event-sourced-state.md](docs/decisions/0001-single-writer-event-sourced-state.md) — the main trade-off
- [docs/runbook.md](docs/runbook.md) — operating it, and what each failure looks like
- [docs/test-report.md](docs/test-report.md) — pass/fail evidence
- [docs/benchmarks.md](docs/benchmarks.md) — measured performance and method
- [docs/SPEC.md](docs/SPEC.md) — the original blueprint

## Safety and operational boundaries

- Paper/simulated by default; no real-money connectivity exists in this repository.
- Tokens come from the environment. The audit signing key is generated into the data directory with mode 0600; `data/`, `*.key` and `*.log` are git-ignored.
- This engine is one independent boundary. It does not replace authentication, authorization, or the venue's and broker's own controls.
- Use only market data and broker APIs you are licensed and authorized to use before any production deployment.
