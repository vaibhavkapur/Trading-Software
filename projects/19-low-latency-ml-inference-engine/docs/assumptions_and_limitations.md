# Assumptions, Known Limitations, and Unsupported Features

## Core Assumptions

1. **Synthetic data scope**: All fixtures (`data/fixtures/synthetic_lob_features.jsonl`) and trained model weights (`v1.0.0`, `v2.0.0`) are deterministically synthesized (`seed=42`). No live, proprietary, or unlicensed market data is used.
2. **Inference-only boundary**: `order_execution_enabled` is `false` in configuration and code defaults. Converting predictions into paper or live orders is outside this component's scope and requires separate risk, compliance, and broker approvals.
3. **Host clock synchronization**: Staleness checks compare UTC timestamps against `inference_ts_utc` (or local system UTC clock). Production deployment requires NTP/PTP clock synchronization within `max_clock_skew_ms` (default 250 ms).
4. **Local hardware benchmarks**: Latency numbers in `reports/m3_benchmark_report.md` measure this local machine only and are **not** SLA promises for production servers or colocation environments.

## Known Limitations

- **Python boundary overhead**: Single-request calls through the Python `InferenceRouter` incur Python object validation, timestamp parsing, and `ctypes` overhead (~10–40 us typical depending on host), whereas the underlying C++20 kernel runs in sub-microsecond time (`bench_native`). Callers requiring <5 us p99 should link `libml_inference_kernel` directly from a C++/Rust host process or batch requests.
- **Single-process in-memory registry**: The MVP runs in-process without a distributed control plane or network RPC layer.
- **Scalar regression/classification heads**: The current manifest validator restricts final layer `out_dim == 1`.

## Explicit List of Unsupported Features (MVP Scope)

- Network RPC server (gRPC / HTTP) — intentionally deferred until process boundaries and SAAT asset-class requirements are confirmed.
- GPU / CUDA / TensorRT execution providers.
- Arbitrary computation graphs (transformers, RNNs, custom ops) in the native C++20 kernel — use `OptionalOnnxAdapter` once ONNX Runtime and verified `.onnx` artifacts are approved.
- Tree ensembles (XGBoost / LightGBM) native parser.
- Automatic model promotion without operator approval.
- Live broker order routing or execution.
- Loading untrusted serialized formats (`.pkl`, `.pickle`, `.joblib`, `.dill`, `.pt`, `.pth`, `.ckpt`).
