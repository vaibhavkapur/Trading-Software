# Architecture

```text
                    quotes (feed token)                 admin token
                          │                                 │
                          ▼                                 ▼
 OMS / strategy ──▶ ┌───────────────────────── api (HTTP/JSON) ─────────────────────────┐
  evaluate          │  strict JSON parsing, auth on privileged routes, error mapping    │
  commit            └───────┬───────────────────────────┬──────────────────────┬────────┘
  fills                     │                           │                      │
  release                   ▼                           ▼                      ▼
                    ┌── market-check ──┐      ┌──── risk-core ─────┐    ┌─ policy-loader ─┐
                    │ latest quote per │◀─────│ one mutex:         │───▶│ immutable,      │
                    │ symbol; freshness│ read │ evaluate, reserve, │read│ versioned,      │
                    │ and collar maths │      │ commit, fill, kill │    │ swapped whole   │
                    └──────────────────┘      └───┬────────────┬───┘    └─────────────────┘
                                         1 append │            │ 2 apply
                                                  ▼            ▼
                                          ┌──── audit ────┐  ┌── state-store ──┐
                                          │ JSONL, hash-  │  │ positions,      │
                                          │ chained,      │─▶│ reservations,   │
                                          │ Ed25519-signed│  │ rate windows,   │
                                          └───────────────┘  │ orders, kill sw.│
                                             replay on start └─────────────────┘
                                                  │
                              kill-switch events  ▼
                                          ┌──── notify ────┐
                                          │ webhooks to OMS│──▶ OMS
                                          │ retry until ack│
                                          └────────────────┘
```

| Package | Spec service | Responsibility |
| --- | --- | --- |
| `src/policy` | policy-loader | Parse and validate the versioned policy; resolve per-account limits |
| `src/core` | risk-core | The evaluation pipeline, reason codes, reservation lifecycle, kill switches |
| `src/state` | state-store | All mutable state; one mutation path (`Apply`) driven by audit events |
| `src/market` | market-check | Quote store, staleness, clock-skew and collar checks |
| `src/audit` | audit | Append-only hash-chained signed log; verification; replay |
| `src/notify` | – | Kill-switch propagation to subscribers |
| `src/api`, `src/app`, `src/metrics`, `src/fixed`, `src/domain` | – | Transport, wiring, metrics, fixed-point decimal, shared types |

## The write path

Every state change, without exception, follows the same three steps under the engine mutex:

1. Compute an **event** from current state (a decision, a commit, a fill, a release, a kill-switch change).
2. **Append** it to the audit log. If the append fails, stop: nothing changes.
3. **Apply** it to the state store.

`state.Store.Apply` is a pure function of state and event: it reads no clock and no external data. Restart is therefore just "apply every record again", and the live state and the recovered state are produced by the same code. Rate-limit windows, the daily-loss day boundary and order retention are all derived from event timestamps for the same reason.

The single exception to step 2 is kill-switch **activation**, which is applied even if the append fails (and reported as `durable: false`), because refusing to stop trading when the evidence store is broken would be failing open. Deactivation always needs the record.

## Invariants

| Invariant | Enforced by |
| --- | --- |
| Check and reserve are one atomic step | Single engine mutex around evaluate → append → apply |
| No ALLOW/REVIEW without an audit record | Append precedes apply; append failure → `REJECT [AUDIT_UNAVAILABLE]` |
| After `SetKillSwitch` returns, nothing in scope is allowed, approved or committed | Same mutex; `gate()` on approve and commit |
| A reservation is counted until explicitly released, filled or its uncommitted lease lapses | `Account.Open`; committed reservations never expire |
| No float touches money | `fixed.Decimal` (int64 × 1e-4), parsed from text, saturating arithmetic |
| A saturated value breaches rather than passes | Policy thresholds capped at 1e13 currency units, far below saturation |
| The log cannot be edited, reordered or have records removed from the middle unnoticed | Hash chain + signature; engine refuses to start otherwise |

Tail truncation (dropping the newest records) is the one change a hash chain cannot detect on its own. Detecting it requires comparing against a head hash stored elsewhere; `GET /v1/audit/head` exists so an external system can record it.

## Time

Two clocks are kept apart on every quote: `event_time` (the venue's) and `receive_time` (ours). Staleness is measured from event time. A quote whose event time is ahead of local time by more than `max_clock_skew_ms` is not trusted for decisions, and one more than 5 s ahead is refused at ingest so it cannot shadow later quotes. All timestamps are UTC.

Quotes are deliberately not persisted. After a restart every order is rejected with `NO_MARKET_DATA` until the feed delivers a fresh quote.

## Durability

Each audit record is written to the OS before the response is sent, so it survives a process crash. Surviving power loss needs an fsync: by default the log is synced every 100 ms (up to 100 ms of acknowledged decisions could be lost with the machine), and `-audit-sync` syncs every record at a large latency cost (see [benchmarks](benchmarks.md)). A torn final line from a crash mid-write is truncated at startup; any other damage stops startup.
