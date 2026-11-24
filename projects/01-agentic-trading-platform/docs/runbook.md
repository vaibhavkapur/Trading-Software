# Runbook

Everything here is paper trading against the built-in simulator.

## Run

```bash
make setup          # once
make run            # http://127.0.0.1:8000, UI at /, OpenAPI at /docs
```

In the dev environment the API key is `dev-local-key` (user `alice`). Paste it into the
UI's key field or send `Authorization: Bearer dev-local-key`.

## Configuration

Set through environment variables; see `src/atp/config.py` for the full list.

| Variable | Default | Notes |
|---|---|---|
| `ATP_ENV` | `dev` | Anything else requires `ATP_API_KEYS` and `ATP_APPROVAL_SECRET`, and removes `/v1/sim/faults` |
| `ATP_API_KEYS` | dev key | `key1:user1,key2:user2` |
| `ATP_APPROVAL_SECRET` | random per process in dev | Rotating it invalidates outstanding approvals |
| `ATP_DB_PATH` | `data/atp.sqlite3` | |
| `ATP_APPROVAL_TTL_SECONDS` | `120` | |
| `ATP_MARKET_SESSION` | `regular` | `always` lets the simulator trade outside NYSE hours; `make run` sets it |
| `ATP_MAX_QUOTE_AGE_SECONDS` | `5` | |
| `ATP_MAX_ORDER_NOTIONAL` / `ATP_MAX_POSITION_NOTIONAL` | `10000` / `25000` | |
| `ATP_PLANNER` | `auto` | `auto` uses Claude when `ANTHROPIC_API_KEY` is set, else the rule parser |
| `ATP_PLANNER_MODEL` | `claude-opus-5-5` | |

Because the dev approval secret is random per process, approvals issued before a restart
are rejected with `APPROVAL_SIGNATURE_INVALID` afterwards. Set `ATP_APPROVAL_SECRET` to
keep them valid across restarts.

## Reading a rejection

`order.reject_reason` and `risk_decision.violations[].code`:

| Code | Meaning | Action |
|---|---|---|
| `RISK:QUOTE_STALE`, `RISK:QUOTE_UNAVAILABLE`, `RISK:QUOTE_CLOCK_SKEW` | Market data is old, missing, or timestamped in the future | Check the feed, then create a new intent |
| `RISK:PORTFOLIO_UNAVAILABLE` | Broker unreachable for reads; exposure could not be checked | Restore the broker, create a new intent |
| `RISK:LIMIT_PRICE_BAND` | Limit price more than 5% from the mid | Price moved or fat finger; re-propose |
| `RISK:MAX_ORDER_NOTIONAL`, `RISK:MAX_POSITION_NOTIONAL`, `RISK:INSUFFICIENT_BUYING_POWER`, `RISK:INSUFFICIENT_POSITION` | Size or exposure limits | Reduce size |
| `RISK:DUPLICATE_ORDER` | Identical live proposal within 60s | Intentional? wait or change it |
| `RISK:MARKET_CLOSED` | Outside the configured session | |
| `APPROVAL_EXPIRED` | Submitted after the approval window | Re-propose and approve again |
| `PROPOSAL_CHANGED`, `ORDER_CHANGED`, `APPROVAL_SIGNATURE_INVALID`, `PROPOSAL_NOT_APPROVED` | Stored data no longer matches what was approved | Treat as an integrity incident: verify the audit chain and inspect the database |
| `BROKER:<reason>` | The broker itself refused the order | |

## Order stuck in `submitting`

The last dispatch could not be confirmed. Calling `POST /v1/orders/{id}/submit` again or
`GET /v1/orders/{id}` asks the broker whether it has the order and adopts its state. If
the broker never received it and the approval has expired, the order ends `rejected`.
See ADR 0001.

## Verify the audit log

```bash
curl -H "Authorization: Bearer $KEY" localhost:8000/v1/audit/verify
PYTHONPATH=src .venv/bin/python scripts/verify_audit.py data/atp.sqlite3   # offline
```

A break reports the first bad sequence number.

## Fault injection (dev only)

`POST /v1/sim/faults` with any of `quotes_stalled`, `quotes_down`, `quote_skew_seconds`,
`price_shock_pct`, `broker_disconnected`, `broker_order_entry_down`,
`broker_drop_next_ack`, `broker_fail_next_submits`. The UI has toggles for the common
ones, and `examples/failure_demo.sh` scripts a broker outage.

## Metrics

`GET /metrics` (Prometheus text format): `atp_proposal_to_approval_seconds`,
`atp_order_submissions_total{outcome}`, `atp_order_submit_retries_total`,
`atp_policy_rejections_total{reason,stage}`, `atp_quote_staleness_seconds`,
`atp_order_reconciliation_mismatch_total{kind}`.

## Reset

`make clean` removes the local database and reports.
