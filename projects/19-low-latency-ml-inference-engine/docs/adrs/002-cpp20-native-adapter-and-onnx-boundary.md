# ADR-002: C++20 Native Runtime Adapter and Isolated ONNX Boundary

- **Status**: Accepted
- **Milestone**: M2 & M3 — Core implementation and optimization

## Context

A low-latency financial inference engine requires predictable microsecond-scale latency and thread-safe concurrent execution while keeping a clean checkout reproducible without mandatory heavy third-party binary downloads.

## Decision

1. Establish `PythonReferenceAdapter` as the ground-truth numerical oracle first ("no optimization before correctness").
2. Implement a C++20 native inference kernel (`src/native/inference_kernel.cpp`) compiled with `-std=c++20 -O3` into a shared library (`libml_inference_kernel`) and standalone microbenchmark binary (`bench_native`):
   - Immutable `MlNativeModel` handle after creation enables lock-free concurrent inference across threads.
   - Stack-allocated `alignas(64)` scratch buffers eliminate heap allocations on the hot path for models up to width 256.
   - Preprocessing (`(x - mean) / std` with clipping, finite check, bounds check) runs in IEEE-754 `double` identically to Python, achieving `< 1e-12` absolute parity (well inside the `1e-9` contract).
3. Optimize the Python `CppNativeAdapter` ctypes boundary *after* profiling by introducing thread-local pre-allocated `ctypes.c_double` buffers (`use_preallocated_buffers=True`) and recording before/after latency in M3 benchmarks.
4. Keep `OptionalOnnxAdapter` behind the `RuntimeAdapter` ABC so ONNX Runtime can be plugged in when optional dependencies (`onnxruntime`, `numpy`) and verified `.onnx` artifacts are provisioned, without breaking clean offline checkouts.

## Consequences

- **Positive**: Zero external Python package dependencies for core tests and benchmarks; sub-microsecond pure C++20 kernel latency and low-tens-of-microseconds end-to-end Python API latency; strict numerical parity.
- **Trade-off**: Python `ctypes` call overhead dominates pure C++20 kernel time for single-item Python API calls; ultra-low-latency callers needing sub-microsecond latency should link `libml_inference_kernel` directly in C++/Rust or use batching.
