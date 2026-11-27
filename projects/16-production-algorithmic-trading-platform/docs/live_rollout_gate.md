# Live Rollout Gate, Readiness Gaps & Security/Risk Review

## 1. Live Enablement Policy (`validate_live_enablement`)

Live capital trading is **hard-blocked by default** in `src/trading_platform/domain/config.py` (`ExecutionPolicyConfig.allow_live_execution = False`). Attempting to instantiate or run with `execution_mode = "LIVE"` raises `LiveModeBlockedError` unless **all** of the following are verified simultaneously:

1. Explicit policy opt-in (`allow_live_execution = true`).
2. Verified `venue_readiness_verified = true` (broker certification, FIX/REST conformance, drop-copy reconciliation tested).
3. Verified `regulatory_readiness_verified = true` (SEC Rule 15c3-5 / MiFID II RTS 6 / applicable jurisdiction controls).
4. Documented references to:
   - Independent Risk Review (`independent_risk_review_ref`)
   - Security & Secrets Review (`security_review_ref`)
   - Extended Controlled Paper Soak Report (`soak_report_ref`)
5. **Three distinct, unexpired written approvals** (strict separation of duties — duplicate `approver_id` is rejected):
   - `RISK_OFFICER`
   - `COMPLIANCE`
   - `CTO`

## 2. Independent Risk & Security Review Summary (M4 Prototype Assessment)

| Review Domain | Current MVP Status | Documented Readiness Gap for Real-Capital Live Rollout |
|---|---|---|
| **Pre-Trade Risk Controls** | Deterministic 10-rule engine + reserved exposure accounting + dual-release kill switch verified across all 8 fault scenarios. | Requires independent risk-team validation of stress limits, short-locate/borrow checks, and multi-currency FX haircuts. |
| **Execution & Broker Connectivity** | Clean `BrokerAdapter` interface with deterministic `PaperBrokerAdapter`. | No real broker/exchange FIX or authenticated REST adapter connected; requires certified sandbox integration + independent broker drop-copy feed. |
| **Persistence & HA** | Durable SQLite WAL locally + PostgreSQL-compatible DDL (`docs/schema.sql`). | Live deployment requires managed PostgreSQL with synchronous replication, automated point-in-time backups, and multi-AZ failover drill. |
| **Authentication & Secrets** | Header-based RBAC in local Operator Console; zero secrets in repo/config. | Live console requires mTLS/OIDC SSO, hardware/KMS-backed secrets manager, signed approval tokens, and network segmentation. |
| **Market Data Entitlements** | Synthetic/permitted fixtures only; explicit `FeedEntitlement` enforcement. | Live market data requires executed exchange/vendor agreements (CTA/UTP/SIP/direct feeds) and documented redistribution rights. |
| **Regulatory & Legal** | Explicitly non-compliant local prototype; live mode blocked by default. | Requires legal/compliance determination of broker-dealer / RIA / prop-trading obligations, CAT/OATS/MiFID reporting, and kill-switch audit retention. |
| **Performance & Availability** | Baseline local Python/SQLite soak measured honestly (`reports/evidence/soak_and_benchmark.json`). | No microsecond latency or HA SLA claimed; any target SLA requires production-hardware load testing and failover benchmarking. |
