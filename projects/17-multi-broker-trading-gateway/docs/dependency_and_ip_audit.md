# Dependency, Licensing, and Background IP Audit

## 1. Linked Portfolio Foundations Reviewed

`plan.md` references three personal portfolio prototypes:
1. `Trading-Software — 07-trading-order-management-system`
2. `Trading-Software — 13-broker-reliability-reconciliation`
3. `Trading-Software — 03-smart-order-router`

## 2. Licensing & IP Disposition (Clean-Room Decision)

Per `plan.md` Section 2:
> *"Confirm access, licenses, background IP and SAAT ownership terms before copying or adapting any personal repository. A linked prototype is not proof of production readiness. If access or licensing is unresolved, implement against clean, documented contracts instead."*

- **Status:** Access, license transfer, and SAAT background IP ownership terms for external personal repositories have **not** been formally executed at MVP stage.
- **Action Taken:** Zero code was copied from any external personal repository. This implementation was authored 100% clean-room against documented domain contracts (`src/domain`, `src/broker`, `src/reconciler`) so it can integrate via clean REST/WebSocket or Go package adapters once upstream OMS/SOR ownership and interfaces are formally confirmed.

## 3. Third-Party Dependency Inventory

- **Runtime dependencies:** **Zero** external third-party Go modules (`go.mod` uses Go standard library exclusively, tested on Go `1.27.1`, targeting `go 1.24+`).
- **Supply-chain risk:** Minimized; offline reproducible builds with no transitive dependency licenses or CVE surface from third-party packages.
- **Future production dependencies (gated):** When moving from local prototype to hosted sandbox/production, a PostgreSQL driver (e.g., `pgx/v5`, MIT) and cloud KMS/Vault SDK (e.g., HashiCorp Vault / GCP Secret Manager, MPL-2.0 / Apache-2.0) will undergo standard legal & security review prior to adoption.

## 4. Data Provenance & Entitlements

All fixtures in `data/fixtures/` are 100% synthetic deterministic datasets documented in `data/fixtures/manifest.json` (`entitlement: internal-test-only`). No proprietary exchange or vendor market data is stored in this repository.
