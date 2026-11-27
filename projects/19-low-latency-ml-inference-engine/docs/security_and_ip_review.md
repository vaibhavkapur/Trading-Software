# Security, Data Licensing, IP, and Deployment Approval Review

## 1. Linked Portfolio Foundations & IP Provenance

`plan.md` Section 2 references three personal portfolio repositories:
- `Quantum-Trading — 07-quantum-order-book-predictor`
- `Machine-Learning — Sagemaker-Fraud-Detector`
- `High-Frequency-Trading — 09-latency-performance-lab`

Per `plan.md` Section 2 ("Confirm access, licenses, background IP and SAAT ownership terms before copying or adapting any personal repository... If access or licensing is unresolved, implement against clean, documented contracts instead"):
- **Zero code or artifacts were copied** from those external repositories.
- All source code, schemas, C++20 kernels, tests, and synthetic fixtures in this repository were implemented cleanly from scratch against documented contracts.

## 2. Data Licensing & Entitlements

- All fixtures under `data/fixtures/` are synthetically generated (`seed=42`) by `src/inference_engine/reference_scorer.py`.
- Provenance and SHA-256 integrity hashes are recorded in `data/fixtures/dataset_manifest.json`.
- Before connecting any real historical or live market data feed, data vendor licensing, redistribution terms, and point-in-time entitlements must be formally verified and documented.

## 3. Security Controls Implemented

- **No untrusted deserialization**: Pickle, joblib, dill, marshal, and raw PyTorch checkpoint files are explicitly blocked by extension and manifest allowlist (`ErrorCode.UNTRUSTED_ARTIFACT_FORMAT`).
- **Cryptographic integrity**: `weights.bin` and `reference_golden.json` are verified against SHA-256 digests and exact byte sizes prior to initialization.
- **Path traversal prevention**: Manifest file references are resolved and asserted to reside strictly within the artifact bundle directory.
- **No secrets in repo**: `config/` contains only non-sensitive versioned parameters. Credentials for future artifact registries or model stores must be injected via environment/secret managers outside git.
- **Fail-closed execution guard**: Order execution is disabled by default (`order_execution_enabled=false`).

## 4. Deployment Approval Checklist (Pre-Sandbox / Pre-Production)

- [ ] Confirm SAAT asset class, strategy horizon, and target latency budget on target hardware.
- [ ] Confirm code ownership and IP assignment policy.
- [ ] Approve market data vendor entitlements for non-synthetic backtesting/paper runs.
- [ ] Security sign-off on artifact signing (e.g., detached cryptographic signatures on `manifest.json` in addition to SHA-256 content hashes).
- [ ] Independent risk, compliance, and broker review prior to any paper or real-capital order routing integration.
