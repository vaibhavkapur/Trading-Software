# M3 Latency & Load Benchmark Report

> Measured on local hardware with synthetic LOB fixtures. Never extrapolate local benchmark results to live production SLAs without target-hardware validation.

## Environment

| Field | Value |
|---|---|
| Timestamp (UTC) | `2026-11-27T09:49:05.145Z` |
| OS / Arch | `Darwin 25.5.0 (arm64)` |
| CPU cores | `10` |
| Python | `3.9.6` |
| Model | `lob_alpha_mlp v1.0.0` |
| Artifact SHA-256 | `826c933443690346...` |
| Native kernel | `cpp20-native-kernel-1.0.0` |

## Cold Start (Load + SHA-256 Verify + Init + Parity + First Inference)

| Runtime | Load & Verify (us) | Adapter Init (us) | Golden Parity (us) | First Inference (us) | Total Cold Start (us) |
|---|---:|---:|---:|---:|---:|
| `python_reference` | 442.92 | 1.50 | 402.12 | 54.96 | 901.50 |
| `cpp_native` | 412.50 | 619.46 | 49.21 | 59.88 | 1141.04 |

## Warm Single-Request Inference (End-to-End Python API)

| Path | Samples | p50 (us) | p95 (us) | p99 (us) | Max (us) | Throughput (QPS) |
|---|---:|---:|---:|---:|---:|---:|
| `python_reference` | 5000 | 22.96 | 24.21 | 31.34 | 134.92 | 42704.4 |
| `cpp_native` (per-call alloc) | 5000 | 9.29 | 9.96 | 18.54 | 101.33 | 103091.8 |
| `cpp_native` (pre-allocated TLS) | 5000 | 8.83 | 9.33 | 9.62 | 92.62 | 109733.9 |

- **p50 speedup (`cpp_native` pre-allocated vs `python_reference`)**: `2.6x`
- **Pre-allocated TLS buffer p50 change vs per-call ctypes allocation**: `4.94%`

## Warm Batch Inference (Batch Size 64)

| Path | Batches | Batch p50 (us) | Batch p99 (us) | Per-Item p50 (us) | Per-Item p99 (us) | Items/sec |
|---|---:|---:|---:|---:|---:|---:|
| `python_reference` | 500 | 1464.69 | 1974.95 | 22.89 | 30.86 | 42740.3 |
| `cpp_native` | 500 | 485.62 | 884.74 | 7.59 | 13.82 | 127685.0 |

## Concurrent Load (Python ThreadPool -> C++20 Native Adapter)

- Threads: `4`, Total Requests: `4000`, Throughput: `91072.4 QPS`
- Latency: p50=`9.88 us`, p95=`13.04 us`, p99=`35.55 us`, max=`9328.25 us`

## Pure C++20 Native Kernel Benchmark (No Python Boundary Overhead)

- Cold start (read weights + init + first score): `43.79 us`

| Workload | Samples | p50 (us) | p95 (us) | p99 (us) | Max (us) |
|---|---:|---:|---:|---:|---:|
| Single inference | 10000 | 0.0830 | 0.0840 | 0.1250 | 0.1670 |
| Batch-64 per-item | 500 | 0.0716 | 0.0729 | 0.0729 | 0.0742 |
| Concurrent (4 threads, 21544017 QPS) | 10000 | 0.0830 | 0.2080 | 0.2090 | 8.0420 |

## Regression Gates

| Gate | Measured p99 (us) | Cap (us) | Passed |
|---|---:|---:|---|
| `cpp_native_single_p99` | 9.62 | 500.00 | `PASS` |
| `cpp_native_batch_per_item_p99` | 13.82 | 100.00 | `PASS` |
| `python_reference_single_p99` | 31.34 | 2000.00 | `PASS` |
