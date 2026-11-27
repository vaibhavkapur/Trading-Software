# ADR-001: Safe Model Artifact and Preprocessing Contract

- **Status**: Accepted
- **Milestone**: M1 — Foundation and design

## Context

`plan.md` explicitly prohibits untrusted serialized model artifacts and requires reproducible predictions with preprocessing parity across Python and optimized native runtimes. Standard Python serialization formats (`pickle`, `joblib`, `dill`, PyTorch `.pt` checkpoints) execute arbitrary code on load and couple preprocessing tightly to training code.

## Decision

1. Every model version is packaged as an immutable directory bundle containing:
   - `manifest.json`: schema-versioned (`1.0.0`) declarative specification of `features` (names, units, dtype, allowed min/max bounds, training mean/std, z-score clip), `layers` (shapes and activations), `output` (unit, task type, absolute parity tolerance), `staleness_policy`, and SHA-256 digests + byte sizes.
   - `weights.bin`: contiguous little-endian IEEE-754 `float64` weight/bias buffer with exact byte-length validation and finite-value checks.
   - `reference_golden.json`: golden input/output vectors scored by the Python reference implementation at export time.
2. `artifact.py` enforces an allowlist (`safe_ieee754_bin_v1`), rejects any path or bundle containing disallowed extensions (`.pkl`, `.pickle`, `.joblib`, `.dill`, `.pt`, `.pth`, `.ckpt`, `.marshal`), prevents manifest path traversal, and verifies SHA-256 digests before model initialization.
3. Preprocessing parameters live inside `manifest.json` so training-time statistics cannot drift from serving-time standardization.

## Consequences

- **Positive**: Zero arbitrary code execution risk during artifact loading; bit-level deterministic loading in both Python and C++20; preprocessing parity is structurally enforced.
- **Trade-off**: Only architectures expressible via the declarative `LayerSpec` schema (currently `mlp_v1`, `linear_v1`) are supported without extending the schema or enabling the optional ONNX Runtime adapter.
