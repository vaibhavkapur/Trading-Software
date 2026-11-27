# Explicit List of Unsupported Features & Non-Goals

Per `plan.md` Sections 1 and 8, the following features are **explicitly unsupported** in this MVP prototype:

1. **No Default Real-Money / Live Execution**: Live broker order routing is disabled by default and blocked by `LiveModeBlockedError`. No real broker credentials or live venue connectors are bundled.
2. **No Unmeasured Latency or Availability Claims**: This prototype makes **zero microsecond latency**, **zero sub-millisecond SLA**, and **zero 99.99% availability claims**. Only measured local paper-sandbox baselines are reported in `reports/evidence/soak_and_benchmark.json`.
3. **No Live Performance Inference**: Simulated paper fills and synthetic price trajectories do not represent live alpha, queue position, market impact, or realized execution quality.
4. **Unsupported Instrument Types**: Options, futures, multi-leg spreads, FX forwards, fixed income, and crypto perpetuals are out of scope (MVP targets `US_EQUITIES` cash instruments in `USD`).
5. **Unsupported Order Types & Algos**: Pegged orders, iceberg/hidden orders, VWAP/TWAP/POV execution algos, and smart order routing across multiple venues are not implemented.
6. **No Real-Time Exchange Feed Connectors**: Direct ITCH/OUCH/FIX market data parsers and paid vendor integrations are excluded until data entitlements are contracted.
7. **No Automated Regulatory Reporting**: CAT, TRACE, MiFID II transaction reporting, and tax-lot accounting (FIFO/LIFO wash sales) are not implemented.
8. **No Production SSO / KMS**: The Operator Console uses local role headers for demonstration and testing; OIDC/mTLS and external secrets vaults are required prior to any shared sandbox or live deployment.
