# Runbook

Paper/simulated use only.

## Start, stop, reload

```bash
export RISK_ADMIN_TOKEN=... RISK_FEED_TOKEN=...
export RISK_OMS_WEBHOOKS=http://oms.internal/kill-switch     # optional, comma-separated
./bin/riskd -addr 127.0.0.1:8080 -policy examples/policy.json -data data [-audit-sync]
```

- Stop with SIGINT/SIGTERM; the log is synced and closed.
- Reload the policy with `kill -HUP <pid>` or `POST /v1/policy/reload`. An invalid file is refused and the running policy is kept. Bump `version` on every change.
- Never run two instances on one data directory.
- Back up `data/audit.log` and `data/audit.key` together. Losing the key means the existing log can no longer be verified, and the engine will refuse to start on it.

## Emergency stop

```bash
curl -X POST localhost:8080/v1/risk/kill-switches/global/activate \
  -H "Authorization: Bearer $RISK_ADMIN_TOKEN" -d '{"activated_by":"<you>","reason":"<why>"}'
```

Scopes: `global`, `accounts/{id}`, `symbols/{symbol}`. Effective for every evaluate, approve and commit as soon as the call returns. It does **not** cancel orders already live at the venue; the OMS must do that on receiving the webhook. Check `GET /v1/risk/kill-switches` for per-subscriber delivery status. Lift with `…/deactivate`.

If the response says `"durable": false`, the switch is on in memory but the audit log is failing: fix the disk before restarting, or re-activate after restart.

## What failures look like

| Symptom | Cause | Action |
| --- | --- | --- |
| Every order `NO_MARKET_DATA` | Just restarted, or the feed never connected, or `RISK_FEED_TOKEN` unset | Restore the feed; quotes are not persisted |
| Orders `STALE_QUOTE` | Feed stalled for longer than `quote_max_age_ms` | Restore the feed |
| Orders `QUOTE_CLOCK_SKEW` | Venue timestamps ahead of the local clock | Check NTP on this host |
| Orders `REVIEW [EXPOSURE_MARK_STALE]` | A held position's symbol has no fresh quote | Restore that symbol's feed, or approve manually |
| Orders `REJECT [AUDIT_UNAVAILABLE]`, 503 on commit/fill | Audit log cannot be written (disk full, permissions) | Free space; no state was changed, callers can retry |
| Commit returns 409 `RESERVATION_EXPIRED` | OMS took longer than `reservation_ttl_ms` between evaluate and commit | Do not send; re-evaluate with a new order_id |
| Commit returns 409 `KILL_SWITCH_ACTIVE` | A switch came on after the order was allowed | Do not send |
| `risk_unreserved_fills_total` rising | OMS sent or kept working orders the engine did not cover | Investigate the OMS; reconcile |
| `risk_state_drift_abs_qty` non-zero | Engine positions differ from the broker's | Find the missing fills; the engine does not self-correct |
| Process exits at start with an `audit:` error | The log fails verification | Do not delete it. Restore from backup and investigate |
| 403 on admin routes | `RISK_ADMIN_TOKEN` not set on the server | Set it and restart |

## Metrics

`risk_decision_duration_seconds` (histogram), `risk_decisions_total{decision}`, `risk_decision_reasons_total{decision,reason}`, `risk_duplicate_replays_total`, `risk_reservations_open`, `risk_kill_switches_active`, `risk_kill_switch_propagation_seconds` (histogram), `risk_kill_switch_propagation_failures_total`, `risk_state_drift_abs_qty{account}`, `risk_unreserved_fills_total`, `risk_audit_append_errors_total`, `risk_quotes_rejected_total{reason}`.

## Verifying the audit log

`GET /v1/audit/verify` (admin) re-checks every hash and signature. To inspect a decision's full trace: `jq -c 'select(.seq==42)' data/audit.log`. Record `GET /v1/audit/head` somewhere outside this machine periodically; it is the only defence against someone truncating the newest records.

## Known limits

Memory holds every order decided in the last 24 hours (roughly 1.4 KB each in the benchmark) plus every order still holding capacity. An `order_id` may be reused, and a fill for it will be refused as unknown, once it is older than that and has no live reservation.
