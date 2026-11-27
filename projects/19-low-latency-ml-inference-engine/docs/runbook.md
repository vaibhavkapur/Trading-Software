# Operator Runbook & Safe Fallback Procedures

## 1. Pre-Deployment Verification

Before promoting any model bundle to a sandbox/paper environment:

```bash
./scripts/setup.sh
./scripts/validate.sh
```

Verify that:
- `reports/m2_parity_validation.json` reports `"all_passed": true` with `python_vs_cpp_max_abs_error <= tolerance_abs`.
- `reports/m3_benchmark_report.json` reports `"all_gates_passed": true`.
- `config/engine_config.json` has `"order_execution_enabled": false`.

## 2. Health and Observability Signals

Inspect `router.health_snapshot()` and `router.metrics_snapshot()`:

| Metric / Signal | Healthy Expectation | Operator Action on Anomaly |
|---|---|---|
| `health.status` | `"healthy"` | If `"not_ready"`, verify model bundle path and integrity logs. |
| `metrics.reload_failures` | `0` | Inspect `rollout_events` for `ARTIFACT_INTEGRITY_ERROR` or `PARITY_VERIFICATION_FAILED`; do **not** bypass SHA-256 checks. Active model remains unaffected. |
| `metrics.errors_by_code.STALE_FEATURE` | Near `0` | Check upstream market data feed latency and host NTP/PTP clock drift. Downstream strategy must remain flat/no-op on rejected ticks. |
| `metrics.errors_by_code.NON_FINITE_FEATURE` / `MISSING_FEATURE` | `0` | Halt upstream feature pipeline rollout and inspect feature store schema version. |
| `metrics.overload_rejections` | `0` | Caller rate exceeds `max_in_flight`. Shed non-critical batch load, increase batching, or scale out horizontally; never disable bounded concurrency. |
| Benchmark `p99` regression gate | `PASS` | Block rollout; profile CPU contention, thermal throttling, or allocator changes before proceeding. |

## 3. Versioned Rollout Procedure

1. **Stage**: Call `router.load_version(v_new_dir, activate_as_primary=False)`. This verifies SHA-256 checksums, schema, and golden parity without shifting traffic.
2. **Canary**: Call `router.set_routing_policy(RoutingPolicy(primary_version="1.0.0", canary_version="2.0.0", canary_weight=0.1))`. Observe `requests_by_version`, `errors_by_code`, and prediction distributions.
3. **Promote**: After operator review and sign-off, promote via `router.set_routing_policy(RoutingPolicy(primary_version="2.0.0"))`.

## 4. Emergency Rollback & Safe Fallback

- **Immediate rollback**: Call `router.rollback()` to atomically restore the previous `RoutingPolicy` in memory (sub-millisecond swap; no disk I/O).
- **Native runtime fallback**: If the C++20 shared library fails to load on a new host, operators may temporarily instantiate `InferenceRouter(runtime="python_reference")` for correctness-preserving fallback while rebuilding `src/native` (expect higher latency; disable latency-sensitive strategies accordingly).
- **Safe failure default**: Any unhandled rejection or degraded state must result in **no prediction / no order** (`order_execution_enabled=false`).
