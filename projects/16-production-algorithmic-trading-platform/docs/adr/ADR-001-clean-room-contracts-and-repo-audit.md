# ADR-001: Clean-Room Canonical Contracts and Foundation Repository Audit

- **Status**: Accepted
- **Milestone**: M1 — Foundation and Design
- **Scope**: Asset class (`US_EQUITIES`), strategy horizon (intraday L1 quote-driven, sub-minute to multi-minute), execution venue (`PAPER_SANDBOX`).

## Context

`plan.md` Section 2 lists four existing personal portfolio repositories as potential foundations:
1. `High-Frequency-Trading-Engine`
2. `Trading-Software — 07-trading-order-management-system`
3. `Trading-Software — 05-pre-trade-risk-engine`
4. `Trading-Software — 08-market-data-platform`

`plan.md` explicitly requires:
> "Confirm access, licenses, background IP and SAAT ownership terms before copying or adapting any personal repository. A linked prototype is not proof of production readiness. If access or licensing is unresolved, implement against clean, documented contracts instead."

## Audit Findings

1. **Local Workspace Inspection**: None of the four external GitHub repositories are checked out locally with a pinned commit or executed SAAT IP assignment/license grant in this environment.
2. **Licensing & Background IP Status**: **Unresolved**. Copying source code from personal repositories without documented SAAT ownership/license clearance would create background-IP contamination risk.
3. **Production-Readiness Caveat**: Even after IP clearance, personal prototypes require deterministic idempotency, unknown-outcome reservation retention, double-entry accounting, and hash-chained audit logging before production consideration.

## Decision

Implement the MVP against **clean-room, versioned domain contracts** (`SCHEMA_VERSION = "1.0.0"`) with strict adapter boundaries (`BrokerAdapter`, `BaseStrategy`, `SQLiteStore` + PostgreSQL DDL) so that once SAAT legal/IP clearance is obtained, existing C++/Rust matching engines or venue gateways can be plugged in behind the adapter interface without altering risk, OMS, or accounting invariants.

## Consequences

- Zero unresolved background-IP dependencies in the codebase.
- Clean checkout runs deterministically with only Python 3.12 + Pydantic (`pytest` and `ruff` for dev).
- Future integration of external repositories requires pinning a commit hash, recording license/IP sign-off, and writing an adapter test suite against `BrokerAdapter` or `MarketDataGuard`.
