# Milestone M1 — Foundation and Design Report

## Deliverables Completed
1. **Foundation Repository & IP Audit**: Documented in [ADR-001-clean-room-contracts-and-repo-audit.md](../docs/adr/ADR-001-clean-room-contracts-and-repo-audit.md). Because personal portfolio repos do not have pinned commits or executed SAAT IP/licensing grants in this workspace, all domain contracts and core modules are implemented clean-room behind strict adapter boundaries.
2. **Target Scope Selection**:
   - **Asset Class**: US Equities (`USD`, cash equities: `AAPL`, `MSFT`, `NVDA`, `SPY`)
   - **Frequency**: Intraday L1 quote-driven (sub-minute to multi-minute)
   - **Broker Sandbox**: Deterministic `PaperBrokerAdapter` behind `BrokerAdapter` ABC
3. **Canonical Versioned Event Contracts** (`SCHEMA_VERSION = "1.0.0"`):
   - Defined in [events.py](../src/trading_platform/domain/events.py), [errors.py](../src/trading_platform/domain/errors.py), and [config.py](../src/trading_platform/domain/config.py).
   - Enforces timezone-aware UTC timestamps, exact `Decimal` monetary/quantity values, explicit ISO currencies, stable IDs, structured errors, and fail-closed `ExecutionPolicyConfig`.
4. **Architecture & Schema Documentation**:
   - [architecture.md](../docs/architecture.md), [api_and_schema.md](../docs/api_and_schema.md), [schema.sql](../docs/schema.sql), [ADR-003-storage-and-deployment-boundaries.md](../docs/adr/ADR-003-storage-and-deployment-boundaries.md).

## Verification Evidence
- Unit tests (`tests/unit/test_domain_and_ledger.py`): 5/5 passed (see [pytest_junit.xml](evidence/pytest_junit.xml)).
- CLI live gate check (`patp check-live-gate`): confirmed `live_allowed: false` by default.
