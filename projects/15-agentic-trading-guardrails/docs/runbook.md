# Operational Runbook — Agentic Trading Guardrails

## 1. Emergency Trading Halt (Kill Switch)

To immediately halt all agentic write actions (`order:propose` and `order:submit`) while preserving read-only audit and market access:

1. Obtain a short-lived `policy:admin` token for `admin-risk`.
2. Publish a new policy version with `"kill_switch_active": true` via `POST /v1/policies`.
3. Verify `GET /healthz` reflects the new `policy_version`.
4. Confirm in `GET /v1/audit?category=POLICY_UPDATE` that the halt event is recorded in the hash chain.

Because deterministic policy is re-evaluated at execution time, even previously signed approval grants are immediately blocked.

## 2. Shadow-Testing a Policy Change Before Rollout

Before tightening or loosening limits:

1. Send the candidate `PolicyBundle` and representative `EvalInput` samples to `POST /v1/policies/simulate`.
2. Inspect `comparisons[].changed` to see every request whose `ALLOW`/`DENY` outcome would flip under the candidate bundle.
3. Publish via `POST /v1/policies` only after confirming zero unexpected diffs.

## 3. Verifying Audit Log Integrity

- Run `GET /v1/audit/verify` (or check `chain_verified` in `GET /healthz` and the Audit UI at `/`).
- If verification returns HTTP `409 Conflict`, treat the audit file (`data/audit.jsonl`) as tampered or truncated:
  1. Activate the kill switch immediately.
  2. Snapshot the affected `data/` volume for forensic inspection.
  3. Restore from verified immutable backup storage before resuming paper execution.

## 4. Handling Stale Market Data or Broker Disconnects

- **Symptom:** Write requests return HTTP `403` (`market quote is stale`) or HTTP `503` (`paper broker adapter disconnected`).
- **Expected behavior:** Fail-closed. No orders are queued or guessed using stale prices.
- **Recovery:** Restore connectivity/freshness on the paper feed; verify `GET /v1/tools/call` (`market.quote`) returns current `venue_time_utc`, then allow agents to create fresh proposals.

## 5. Identity Revocation & Key Rotation

- **Compromised agent or token:** Call `Identity.RevokeIdentity(agentID)` or `Identity.RevokeToken(jti)` — verification checks live registry state and denies immediately even before JWT `exp`.
- **Signing key rotation:** Call `Identity.RotateKey()`. New tokens use the new `kid`; `/.well-known/jwks.json` publishes both active and retained public keys so in-flight tokens expire cleanly without downtime.
