# Unsupported Features & Explicit Non-Goals (MVP Scope)

Per `plan.md` Sections 1 and 8, the following capabilities are **explicitly unsupported** at MVP scope:

1. **Unapproved Production / Real-Capital Trading:** Live execution is disabled by default and rejected by `config/policy.json` (`deny_live_execution: true`). Real-capital trading requires separate legal, compliance, broker certification, and risk approval.
2. **Cross-Broker Arbitrage & Smart Order Routing (SOR) Execution:** The gateway routes each order to an explicitly specified `broker_id`. Multi-venue order splitting, latency arbitrage, and smart routing decisions belong to an upstream SOR layer.
3. **Real External Broker Network Connections:** `sandbox-alpha` and `sandbox-beta` are deterministic local sandbox simulators exercising distinct REST/OAuth vs Async/HMAC/Replay semantics. Connecting real third-party brokerage HTTP/FIX endpoints requires confirmed broker contracts, credentials, and network egress approval.
4. **Order Replace / Amend (`Cancel/Replace`):** Not exposed as a gateway endpoint in MVP; callers must cancel and submit a new canonical `client_order_id` after terminal cancel confirmation.
5. **Options, Futures, Multi-Leg Spreads, and Short-Locate Workflows:** Canonical schema defines `OPTION`/`FX` enum placeholders, but MVP instrument mappings cover `EQUITY`, `ETF`, and spot `CRYPTO` only.
6. **Live PostgreSQL Driver Binding:** `docs/schema.sql` provides the exact production PostgreSQL DDL aligned with `src/storage.Store`, while local reproducible execution runs zero-dependency `JournalStore` / `MemoryStore`.
7. **Distributed Consensus / Multi-Region Active-Active Clustering:** MVP intentionally runs a single-process gateway with durable write-ahead persistence (`plan.md` Section 3: *"start with the simplest process boundaries that satisfy the workload"*).
