# Low-Latency ML Inference Engine

A Python reference scorer and a C++20 kernel run the same model on synthetic limit order book features. The engine verifies model weights and reference predictions before loading a version. Its router supports canary traffic, rollback, and rejection of stale inputs. The demo produces predictions with order execution disabled.

## Getting started

The demo requires Python 3.9 or later, Make, and a C++20 compiler.

From the repository root, enter the project directory and run the following commands.

```bash
cd projects/19-low-latency-ml-inference-engine
# 1. Build C++20 native kernel & benchmark binary, generate deterministic synthetic fixtures and models
./scripts/setup.sh

# 2. Run full unit, integration, and failure-scenario test suite + parity verification
./scripts/validate.sh

# 3. Run end-to-end rollout, canary, rollback, and fail-safe demo
./scripts/demo.sh

# 4. Run full reproducible pipeline (setup + tests + benchmarks + reports)
./scripts/reproduce.sh
```

## Project layout

```text
19-low-latency-ml-inference-engine/
  plan.md
  README.md
  pyproject.toml
  config/
    engine_config.json
    benchmark_targets.json
  docs/
    architecture.md
    assumptions_and_limitations.md
    runbook.md
    security_and_ip_review.md
    adrs/
      001-safe-artifact-contract.md
      002-cpp20-native-adapter-and-onnx-boundary.md
      003-rollout-staleness-and-safe-failure.md
  data/fixtures/
    dataset_manifest.json
    synthetic_lob_features.jsonl
    models/lob_alpha_mlp/{v1.0.0,v2.0.0}/
  src/
    inference_engine/
    native/
  tests/
    unit/
    integration/
    scenarios/
  scripts/
    setup.sh
    demo.sh
    reproduce.sh
    validate.sh
  reports/
```

## Documentation and evidence

- Architecture & API contracts: [docs/architecture.md](docs/architecture.md)
- Assumptions, non-goals & unsupported features: [docs/assumptions_and_limitations.md](docs/assumptions_and_limitations.md)
- Operator runbook & safe fallback: [docs/runbook.md](docs/runbook.md)
- Security, data licensing & IP review: [docs/security_and_ip_review.md](docs/security_and_ip_review.md)
- Benchmark evidence: [reports/m3_benchmark_report.md](reports/m3_benchmark_report.md)
- Parity evidence: [reports/m2_parity_validation.json](reports/m2_parity_validation.json)
- Hardening & rollout evidence: [reports/m4_hardening_acceptance.json](reports/m4_hardening_acceptance.json)
