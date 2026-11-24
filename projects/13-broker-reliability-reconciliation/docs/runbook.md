# Operational Incident Runbook — Broker Reliability & Reconciliation

> **Golden Rules (Non-Goals):**
> 1. **Never** overwrite or delete internal ledger history to force a match.
> 2. **Never** place automatic offsetting trades to mask a discrepancy.
> 3. **Never** treat pending/settling cash as final settled cash.

---

## 1. Late Fills (`late_fill`)

**Trigger:** A broker execution has `event_time <= cutoff`, but arrived after `cutoff` or with `receive_time - event_time > policy.late_fill_threshold_sec`.

### Triage Steps
1. Open the issue in the Operator UI (`GET /v1/reconciliation/issues/{id}`) and inspect `evidence.broker_data` (`event_time`, `receive_time`) and `evidence.deltas.arrival_delay_sec`.
2. Verify whether the fill is also present in the internal ledger (if absent, a companion `missing_ledger_fill` issue will be open — resolve that first).
3. Check `recon_late_fill_arrival_seconds` in Prometheus (`/metrics`) to determine whether this is an isolated exchange drop-copy delay or a systemic broker feed lag.
4. Confirm the execution against the broker's end-of-day settlement drop or FIX drop-copy archive.

### Approved Resolution Actions
- If the fill matches internal ledger state and was merely delayed in transit, resolve via:
  ```bash
  POST /v1/reconciliation/issues/{id}/resolve
  {"resolution":"late_broker_event","evidence":"Confirmed in broker EOD drop; latency spike logged in INC-xxx","actor":"<you>"}
  ```
- If systemic broker lag persists (>5 minutes), escalate to `escalated` and notify the broker connectivity desk.

---

## 2. Duplicate Fills (`duplicate_broker_fill` / `duplicate_ledger_fill`)

**Trigger:** The same `broker_exec_id` appears more than once in broker statements/webhooks or internal ledger records.

### Triage Steps
1. Inspect `evidence.deltas.duplicate_count` and compare timestamps/quantities across raw deliveries.
2. Check whether the duplicate inflated internal position or cash (`position_break` or `pending_cash_break` on the same symbol/account).
3. **For `duplicate_broker_fill`:** Verify the internal OMS deduplicated on `broker_exec_id` and did not double-book the fill.
4. **For `duplicate_ledger_fill`:** Immediately escalate (`POST .../state` -> `escalated`). Do **not** delete the original ledger row; prepare a compensating accounting journal entry proposal following dual-control approval.

### Approved Resolution Actions
- Benign broker webhook retransmission already ignored by OMS:
  `{"resolution":"duplicate_ignored","evidence":"Confirmed OMS idempotency kept single fill; position matches broker","actor":"<you>"}`
- Double-booked internal ledger entry:
  `{"resolution":"journal_adjustment_proposed","evidence":"Approved compensating journal JRN-xxx attached; original rows preserved","actor":"<you>"}`

---

## 3. Missing Orders (`missing_broker_order` / `missing_ledger_order`)

**Trigger:** An internal order is absent from broker order records, or a broker order has no matching internal `client_order_id` / `broker_order_id`.

### Triage Steps
1. **Check `missing_ledger_order` first (CRITICAL):** An order exists at the broker that our internal OMS does not know about!
   - Immediately determine if the order was entered manually via a broker GUI, drop-copy, or unauthorized channel.
   - Check if fills occurred (`missing_ledger_fill` or `position_break`) and notify risk/trading desk immediately.
2. **Check `missing_broker_order` (HIGH):**
   - Verify whether the order was rejected pre-routing, dropped during a FIX session disconnect, or mapped under a different `broker_order_id` alias.
   - Confirm order state with the broker before taking any remediation step; do **not** blindly resend an order that may already be resting at the venue.

### Approved Resolution Actions
- Confirmed pre-route reject / never left gateway: `{"resolution":"broker_ops_ticket_opened","evidence":"Verified gateway reject log; zero venue exposure","actor":"<you>"}`
- Manual broker-desk order backfilled via approved ops ticket: `{"resolution":"journal_adjustment_proposed","evidence":"Desk-authorized voice order logged in OPS-xxx","actor":"<you>"}`

---

## 4. Missing Fills & Attribute Mismatches (`missing_broker_fill`, `missing_ledger_fill`, `fill_mismatch`)

1. For `missing_ledger_fill` (CRITICAL), check lost webhook / FIX drop-copy gaps and reconcile against broker EOD trade file.
2. For `fill_mismatch`, inspect `evidence.deltas` (`price_delta`, `fee_delta`, `qty_delta`). Small fee differences (e.g. regulatory SEC/TAF fee adjustments) should be confirmed against broker fee schedule and recorded via an approved journal adjustment proposal.

---

## 5. Settled vs. Pending Cash Breaks (`settled_cash_break`, `pending_cash_break`)

- `settled_cash_break` is **CRITICAL**: pending/settling cash is intentionally excluded. Investigate wires, dividends, borrow fees, margin interest, or failed T+1 settlements.
- `pending_cash_break` is **MEDIUM**: verify whether unsettled T+0/T+1 trades or late fills explain the delta; track until settlement completes.

---

## 6. Broker Disconnects & Stale Snapshots (`stale_snapshot`, failed runs)

1. Check `/metrics` for `recon_connector_errors_total` and `recon_runs_total{status="failed"}`.
2. Inspect the failed run via `GET /v1/reconciliation/runs/{id}`.
3. Verify broker API credentials (via secret manager), network connectivity, and rate limits.
4. Once connectivity restores, verify `stale_snapshot` issues clear on the next fresh cutoff run before closing downstream alerts.
