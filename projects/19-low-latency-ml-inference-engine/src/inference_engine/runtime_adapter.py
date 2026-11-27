"""Runtime adapters: Python reference, C++20 native kernel, and optional ONNX boundary."""

from __future__ import annotations

import ctypes
import platform
import threading
import time
from abc import ABC, abstractmethod
from pathlib import Path
from typing import List, Optional, Sequence

from .artifact import LoadedModelArtifact
from .contracts import (
    BatchInferenceRequest,
    BatchInferenceResponse,
    EngineError,
    ErrorCode,
    InferenceRequest,
    InferenceResponse,
    StalenessPolicy,
    format_utc_timestamp,
)
from .preprocessing import pack_raw_batch, prepare_request
from .reference_scorer import score_raw_vector, verify_golden_parity


_ACT_TO_CODE = {
    "linear": 0,
    "relu": 1,
    "tanh": 2,
    "sigmoid": 3,
}


def default_native_library_path() -> Path:
    native_dir = Path(__file__).resolve().parent.parent / "native" / "build"
    ext = "dylib" if platform.system() == "Darwin" else "so"
    return native_dir / f"libml_inference_kernel.{ext}"


class RuntimeAdapter(ABC):
    """Abstract scoring adapter isolating runtime implementation from domain routing."""

    runtime_name: str

    def __init__(self, artifact: LoadedModelArtifact) -> None:
        self.artifact = artifact

    @abstractmethod
    def score_raw_one(self, raw_vec: Sequence[float]) -> float:
        """Score a single validated raw feature vector."""

    @abstractmethod
    def score_raw_batch(self, raw_flat: Sequence[float], batch_size: int) -> List[float]:
        """Score a contiguous row-major batch of validated raw features."""

    def score_request(
        self,
        request: InferenceRequest,
        *,
        staleness_override: Optional[StalenessPolicy] = None,
        route_tag: str = "primary",
        order_execution_enabled: bool = False,
    ) -> InferenceResponse:
        t0 = time.perf_counter_ns()
        raw_vec, inf_dt, age_ms = prepare_request(
            request, self.artifact.manifest, staleness_override=staleness_override
        )
        pred = self.score_raw_one(raw_vec)
        elapsed_us = (time.perf_counter_ns() - t0) / 1000.0
        manifest = self.artifact.manifest
        return InferenceResponse(
            request_id=request.request_id,
            instrument_id=request.instrument_id,
            currency=request.currency,
            model_id=manifest.model_id,
            model_version=manifest.model_version,
            artifact_sha256=self.artifact.bundle_sha256,
            runtime_name=self.runtime_name,
            output_name=manifest.output.name,
            output_unit=manifest.output.unit,
            prediction=pred,
            inference_ts_utc=format_utc_timestamp(inf_dt),
            feature_age_ms=round(age_ms, 3),
            latency_us=round(elapsed_us, 4),
            order_execution_enabled=order_execution_enabled,
            route_tag=route_tag,
        )

    def score_batch(
        self,
        batch_request: BatchInferenceRequest,
        *,
        max_batch_size: int = 256,
        staleness_override: Optional[StalenessPolicy] = None,
        route_tag: str = "primary",
        order_execution_enabled: bool = False,
    ) -> BatchInferenceResponse:
        if not batch_request.requests:
            raise EngineError(
                ErrorCode.EMPTY_BATCH, "Batch inference request must contain at least one item."
            )
        if len(batch_request.requests) > max_batch_size:
            raise EngineError(
                ErrorCode.BATCH_TOO_LARGE,
                f"Batch size {len(batch_request.requests)} exceeds max_batch_size={max_batch_size}.",
            )
        t0 = time.perf_counter_ns()
        raw_buf, inf_dts, ages_ms = pack_raw_batch(
            batch_request.requests,
            self.artifact.manifest,
            staleness_override=staleness_override,
        )
        preds = self.score_raw_batch(raw_buf, len(batch_request.requests))
        total_us = (time.perf_counter_ns() - t0) / 1000.0
        per_item_us = total_us / len(batch_request.requests)
        manifest = self.artifact.manifest
        responses = tuple(
            InferenceResponse(
                request_id=req.request_id,
                instrument_id=req.instrument_id,
                currency=req.currency,
                model_id=manifest.model_id,
                model_version=manifest.model_version,
                artifact_sha256=self.artifact.bundle_sha256,
                runtime_name=self.runtime_name,
                output_name=manifest.output.name,
                output_unit=manifest.output.unit,
                prediction=pred,
                inference_ts_utc=format_utc_timestamp(inf_dt),
                feature_age_ms=round(age_ms, 3),
                latency_us=round(per_item_us, 4),
                order_execution_enabled=order_execution_enabled,
                route_tag=route_tag,
            )
            for req, pred, inf_dt, age_ms in zip(
                batch_request.requests, preds, inf_dts, ages_ms
            )
        )
        return BatchInferenceResponse(
            batch_id=batch_request.batch_id,
            model_id=manifest.model_id,
            model_version=manifest.model_version,
            artifact_sha256=self.artifact.bundle_sha256,
            runtime_name=self.runtime_name,
            responses=responses,
            batch_latency_us=round(total_us, 4),
            route_tag=route_tag,
        )

    def verify_parity(self) -> dict:
        return verify_golden_parity(self.artifact, self.score_raw_one)

    def close(self) -> None:
        return None


class PythonReferenceAdapter(RuntimeAdapter):
    """Deterministic pure-Python reference scorer (ground-truth oracle)."""

    runtime_name = "python_reference"

    def score_raw_one(self, raw_vec: Sequence[float]) -> float:
        return score_raw_vector(raw_vec, self.artifact)

    def score_raw_batch(self, raw_flat: Sequence[float], batch_size: int) -> List[float]:
        nf = len(self.artifact.manifest.features)
        out: List[float] = [0.0] * batch_size
        for b in range(batch_size):
            start = b * nf
            out[b] = score_raw_vector(raw_flat[start : start + nf], self.artifact)
        return out


class CppNativeAdapter(RuntimeAdapter):
    """Optimized C++20 native scoring adapter via ctypes with pre-allocated thread-local buffers."""

    runtime_name = "cpp_native"

    def __init__(
        self,
        artifact: LoadedModelArtifact,
        *,
        library_path: Optional[Path] = None,
        use_preallocated_buffers: bool = True,
    ) -> None:
        super().__init__(artifact)
        self._lib_path = Path(library_path) if library_path else default_native_library_path()
        if not self._lib_path.is_file():
            raise EngineError(
                ErrorCode.RUNTIME_UNAVAILABLE,
                f"C++20 native library not found at {self._lib_path}. Run scripts/setup.sh to build it.",
                details={"library_path": str(self._lib_path)},
            )
        self._lib = self._load_cdll(self._lib_path)
        self._num_features = len(artifact.manifest.features)
        self._use_preallocated = use_preallocated_buffers
        self._tls = threading.local()
        self._handle = self._create_native_handle(artifact)
        self._closed = False

    @staticmethod
    def _load_cdll(lib_path: Path) -> ctypes.CDLL:
        lib = ctypes.CDLL(str(lib_path))
        c_double_p = ctypes.POINTER(ctypes.c_double)
        c_int32_p = ctypes.POINTER(ctypes.c_int32)

        lib.ml_engine_create_model.argtypes = [
            ctypes.c_int32,
            c_double_p,
            c_double_p,
            c_double_p,
            c_double_p,
            c_double_p,
            ctypes.c_int32,
            c_int32_p,
            c_int32_p,
            c_int32_p,
            c_double_p,
            ctypes.c_int64,
        ]
        lib.ml_engine_create_model.restype = ctypes.c_void_p

        lib.ml_engine_destroy_model.argtypes = [ctypes.c_void_p]
        lib.ml_engine_destroy_model.restype = None

        lib.ml_engine_score_one.argtypes = [
            ctypes.c_void_p,
            c_double_p,
            ctypes.c_int32,
            c_double_p,
        ]
        lib.ml_engine_score_one.restype = ctypes.c_int32

        lib.ml_engine_score_batch.argtypes = [
            ctypes.c_void_p,
            c_double_p,
            ctypes.c_int32,
            ctypes.c_int32,
            c_double_p,
        ]
        lib.ml_engine_score_batch.restype = ctypes.c_int32

        lib.ml_engine_version.argtypes = []
        lib.ml_engine_version.restype = ctypes.c_char_p
        return lib

    def _create_native_handle(self, artifact: LoadedModelArtifact) -> int:
        m = artifact.manifest
        nf = len(m.features)
        nl = len(m.layers)
        means = (ctypes.c_double * nf)(*(f.mean for f in m.features))
        stds = (ctypes.c_double * nf)(*(f.std for f in m.features))
        mins = (ctypes.c_double * nf)(*(f.min_allowed for f in m.features))
        maxs = (ctypes.c_double * nf)(*(f.max_allowed for f in m.features))
        clips = (ctypes.c_double * nf)(*(f.clip_z for f in m.features))

        in_dims = (ctypes.c_int32 * nl)(*(l.in_dim for l in m.layers))
        out_dims = (ctypes.c_int32 * nl)(*(l.out_dim for l in m.layers))
        acts = (ctypes.c_int32 * nl)(*(_ACT_TO_CODE[l.activation] for l in m.layers))

        n_params = len(artifact.weights_flat)
        weights_arr = (ctypes.c_double * n_params)(*artifact.weights_flat)

        handle = self._lib.ml_engine_create_model(
            nf,
            means,
            stds,
            mins,
            maxs,
            clips,
            nl,
            in_dims,
            out_dims,
            acts,
            weights_arr,
            n_params,
        )
        if not handle:
            raise EngineError(
                ErrorCode.ARTIFACT_INTEGRITY_ERROR,
                "C++20 native kernel rejected model parameters during initialization.",
            )
        return int(handle)

    @property
    def kernel_version(self) -> str:
        return self._lib.ml_engine_version().decode("utf-8")

    def _raise_for_status(self, status: int) -> None:
        if status == 0:
            return
        if status == -2:
            raise EngineError(
                ErrorCode.NON_FINITE_FEATURE,
                "C++20 native kernel rejected non-finite input feature.",
            )
        if status == -3:
            raise EngineError(
                ErrorCode.FEATURE_OUT_OF_BOUNDS,
                "C++20 native kernel rejected out-of-bounds input feature.",
            )
        if status == -4:
            raise EngineError(
                ErrorCode.NON_FINITE_FEATURE,
                "C++20 native kernel produced non-finite output.",
            )
        raise EngineError(
            ErrorCode.RUNTIME_UNAVAILABLE,
            f"C++20 native kernel returned error status {status}.",
        )

    def _get_single_buffers(self):
        if not self._use_preallocated:
            return (ctypes.c_double * self._num_features)(), ctypes.c_double(0.0)
        bufs = getattr(self._tls, "single_bufs", None)
        if bufs is None:
            bufs = ((ctypes.c_double * self._num_features)(), ctypes.c_double(0.0))
            self._tls.single_bufs = bufs
        return bufs

    def score_raw_one(self, raw_vec: Sequence[float]) -> float:
        if self._closed:
            raise EngineError(ErrorCode.RUNTIME_UNAVAILABLE, "Native adapter has been closed.")
        in_arr, out_val = self._get_single_buffers()
        for i in range(self._num_features):
            in_arr[i] = raw_vec[i]
        status = self._lib.ml_engine_score_one(
            self._handle, in_arr, self._num_features, ctypes.byref(out_val)
        )
        self._raise_for_status(status)
        return float(out_val.value)

    def score_raw_batch(self, raw_flat: Sequence[float], batch_size: int) -> List[float]:
        if self._closed:
            raise EngineError(ErrorCode.RUNTIME_UNAVAILABLE, "Native adapter has been closed.")
        total_in = batch_size * self._num_features
        if hasattr(raw_flat, "buffer_info"):
            addr, length = raw_flat.buffer_info()  # type: ignore[attr-defined]
            if length != total_in:
                raise EngineError(
                    ErrorCode.MISSING_FEATURE,
                    f"Batch buffer length {length} != expected {total_in}.",
                )
            in_ptr = ctypes.cast(addr, ctypes.POINTER(ctypes.c_double))
        else:
            in_arr = (ctypes.c_double * total_in)(*raw_flat)
            in_ptr = in_arr
        out_arr = (ctypes.c_double * batch_size)()
        status = self._lib.ml_engine_score_batch(
            self._handle, in_ptr, batch_size, self._num_features, out_arr
        )
        self._raise_for_status(status)
        return [float(out_arr[i]) for i in range(batch_size)]

    def close(self) -> None:
        if not self._closed and self._handle:
            self._lib.ml_engine_destroy_model(self._handle)
            self._handle = 0
            self._closed = True

    def __del__(self) -> None:
        try:
            self.close()
        except Exception:
            pass


class OptionalOnnxAdapter(RuntimeAdapter):
    """Isolated adapter boundary for optional ONNX Runtime deployments.

    Only active when optional `onnxruntime` and `numpy` packages are installed and an
    exported `.onnx` artifact with matching SHA-256 manifest is supplied.
    """

    runtime_name = "onnx_runtime"

    def __init__(self, artifact: LoadedModelArtifact, onnx_model_path: Path) -> None:
        super().__init__(artifact)
        try:
            import numpy as np  # type: ignore
            import onnxruntime as ort  # type: ignore
        except ImportError as exc:
            raise EngineError(
                ErrorCode.RUNTIME_UNAVAILABLE,
                "Optional ONNX Runtime dependencies ('onnxruntime', 'numpy') are not installed. "
                "Use 'cpp_native' or 'python_reference' runtime, or install extras '[onnx]'.",
            ) from exc
        if not Path(onnx_model_path).is_file():
            raise EngineError(
                ErrorCode.ARTIFACT_INTEGRITY_ERROR,
                f"ONNX model file not found: {onnx_model_path}",
            )
        self._np = np
        self._session = ort.InferenceSession(
            str(onnx_model_path), providers=["CPUExecutionProvider"]
        )
        self._input_name = self._session.get_inputs()[0].name

    def score_raw_one(self, raw_vec: Sequence[float]) -> float:
        arr = self._np.asarray([raw_vec], dtype=self._np.float64)
        out = self._session.run(None, {self._input_name: arr})[0]
        return float(out.ravel()[0])

    def score_raw_batch(self, raw_flat: Sequence[float], batch_size: int) -> List[float]:
        nf = len(self.artifact.manifest.features)
        arr = self._np.asarray(raw_flat, dtype=self._np.float64).reshape(batch_size, nf)
        out = self._session.run(None, {self._input_name: arr})[0]
        return [float(x) for x in out.ravel()]


def create_adapter(
    artifact: LoadedModelArtifact,
    runtime: str = "cpp_native",
    *,
    library_path: Optional[Path] = None,
    verify_parity: bool = True,
    use_preallocated_buffers: bool = True,
) -> RuntimeAdapter:
    """Factory creating and parity-verifying a runtime adapter."""
    if runtime == "python_reference":
        adapter: RuntimeAdapter = PythonReferenceAdapter(artifact)
    elif runtime == "cpp_native":
        adapter = CppNativeAdapter(
            artifact,
            library_path=library_path,
            use_preallocated_buffers=use_preallocated_buffers,
        )
    else:
        raise EngineError(
            ErrorCode.RUNTIME_UNAVAILABLE,
            f"Unsupported runtime {runtime!r}; choose 'cpp_native' or 'python_reference'.",
        )
    if verify_parity:
        adapter.verify_parity()
    return adapter
