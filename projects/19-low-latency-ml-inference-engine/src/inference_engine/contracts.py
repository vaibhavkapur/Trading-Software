"""Canonical domain contracts, schemas, timestamps, and structured errors."""

from __future__ import annotations

import math
from dataclasses import dataclass, field
from datetime import datetime, timezone
from enum import Enum
from typing import Any, Dict, List, Mapping, Optional, Sequence, Tuple


SCHEMA_VERSION = "1.0.0"
ALLOWED_SERIALIZATION_FORMATS = frozenset({"safe_ieee754_bin_v1"})
ALLOWED_ARCHITECTURES = frozenset({"mlp_v1", "linear_v1"})
ALLOWED_ACTIVATIONS = frozenset({"linear", "relu", "tanh", "sigmoid"})
UNTRUSTED_EXTENSIONS = frozenset(
    {".pkl", ".pickle", ".joblib", ".dill", ".pt", ".pth", ".ckpt", ".marshal"}
)


class ErrorCode(str, Enum):
    """Structured fail-safe error codes emitted by the inference engine."""

    MISSING_FEATURE = "MISSING_FEATURE"
    UNEXPECTED_FEATURE_TYPE = "UNEXPECTED_FEATURE_TYPE"
    NON_FINITE_FEATURE = "NON_FINITE_FEATURE"
    FEATURE_OUT_OF_BOUNDS = "FEATURE_OUT_OF_BOUNDS"
    STALE_FEATURE = "STALE_FEATURE"
    FUTURE_TIMESTAMP = "FUTURE_TIMESTAMP"
    INVALID_TIMESTAMP = "INVALID_TIMESTAMP"
    EMPTY_BATCH = "EMPTY_BATCH"
    BATCH_TOO_LARGE = "BATCH_TOO_LARGE"
    UNTRUSTED_ARTIFACT_FORMAT = "UNTRUSTED_ARTIFACT_FORMAT"
    ARTIFACT_INTEGRITY_ERROR = "ARTIFACT_INTEGRITY_ERROR"
    ARTIFACT_SCHEMA_ERROR = "ARTIFACT_SCHEMA_ERROR"
    PARITY_VERIFICATION_FAILED = "PARITY_VERIFICATION_FAILED"
    MODEL_VERSION_NOT_FOUND = "MODEL_VERSION_NOT_FOUND"
    MODEL_RELOAD_FAILED = "MODEL_RELOAD_FAILED"
    OVERLOAD_REJECTED = "OVERLOAD_REJECTED"
    RUNTIME_UNAVAILABLE = "RUNTIME_UNAVAILABLE"
    EXECUTION_DISABLED = "EXECUTION_DISABLED"
    LATENCY_REGRESSION = "LATENCY_REGRESSION"


class EngineError(Exception):
    """Structured domain error that guarantees fail-safe rejection."""

    def __init__(
        self,
        code: ErrorCode,
        message: str,
        *,
        details: Optional[Dict[str, Any]] = None,
    ) -> None:
        super().__init__(f"[{code.value}] {message}")
        self.code = code
        self.message = message
        self.details: Dict[str, Any] = details or {}

    def to_dict(self) -> Dict[str, Any]:
        return {
            "error_code": self.code.value,
            "message": self.message,
            "details": self.details,
        }


def parse_utc_timestamp(ts: str) -> datetime:
    """Parse an ISO-8601 UTC timestamp string strictly into timezone-aware UTC datetime."""
    if not isinstance(ts, str) or not ts.strip():
        raise EngineError(
            ErrorCode.INVALID_TIMESTAMP,
            "Timestamp must be a non-empty ISO-8601 UTC string.",
            details={"value": ts},
        )
    normalized = ts.strip()
    if normalized.endswith("Z"):
        normalized = normalized[:-1] + "+00:00"
    try:
        dt = datetime.fromisoformat(normalized)
    except ValueError as exc:
        raise EngineError(
            ErrorCode.INVALID_TIMESTAMP,
            f"Invalid ISO-8601 timestamp: {ts!r}",
            details={"value": ts},
        ) from exc
    if dt.tzinfo is None:
        raise EngineError(
            ErrorCode.INVALID_TIMESTAMP,
            "Timestamp must include explicit UTC timezone offset ('Z' or '+00:00').",
            details={"value": ts},
        )
    return dt.astimezone(timezone.utc)


def format_utc_timestamp(dt: datetime) -> str:
    """Format a datetime as canonical ISO-8601 UTC string with millisecond precision."""
    utc_dt = dt.astimezone(timezone.utc)
    return utc_dt.strftime("%Y-%m-%dT%H:%M:%S.%f")[:-3] + "Z"


def utc_now() -> datetime:
    return datetime.now(timezone.utc)


@dataclass(frozen=True)
class FeatureSpec:
    """Canonical specification and preprocessing parameters for one input feature."""

    name: str
    unit: str
    dtype: str
    mean: float
    std: float
    min_allowed: float
    max_allowed: float
    clip_z: float = 5.0

    def validate_definition(self) -> None:
        if not self.name or not isinstance(self.name, str):
            raise EngineError(
                ErrorCode.ARTIFACT_SCHEMA_ERROR, "Feature name must be a non-empty string."
            )
        if self.dtype != "float64":
            raise EngineError(
                ErrorCode.ARTIFACT_SCHEMA_ERROR,
                f"Unsupported feature dtype {self.dtype!r} for {self.name!r}; expected 'float64'.",
            )
        for label, val in (
            ("mean", self.mean),
            ("std", self.std),
            ("min_allowed", self.min_allowed),
            ("max_allowed", self.max_allowed),
            ("clip_z", self.clip_z),
        ):
            if not isinstance(val, (int, float)) or not math.isfinite(val):
                raise EngineError(
                    ErrorCode.ARTIFACT_SCHEMA_ERROR,
                    f"Feature {self.name!r} has non-finite {label}: {val!r}",
                )
        if self.std <= 0.0:
            raise EngineError(
                ErrorCode.ARTIFACT_SCHEMA_ERROR,
                f"Feature {self.name!r} std must be > 0, got {self.std}",
            )
        if self.min_allowed > self.max_allowed:
            raise EngineError(
                ErrorCode.ARTIFACT_SCHEMA_ERROR,
                f"Feature {self.name!r} min_allowed ({self.min_allowed}) > max_allowed ({self.max_allowed})",
            )
        if self.clip_z <= 0.0:
            raise EngineError(
                ErrorCode.ARTIFACT_SCHEMA_ERROR,
                f"Feature {self.name!r} clip_z must be > 0, got {self.clip_z}",
            )

    def to_dict(self) -> Dict[str, Any]:
        return {
            "name": self.name,
            "unit": self.unit,
            "dtype": self.dtype,
            "mean": self.mean,
            "std": self.std,
            "min_allowed": self.min_allowed,
            "max_allowed": self.max_allowed,
            "clip_z": self.clip_z,
        }

    @classmethod
    def from_dict(cls, data: Mapping[str, Any]) -> "FeatureSpec":
        try:
            spec = cls(
                name=str(data["name"]),
                unit=str(data["unit"]),
                dtype=str(data["dtype"]),
                mean=float(data["mean"]),
                std=float(data["std"]),
                min_allowed=float(data["min_allowed"]),
                max_allowed=float(data["max_allowed"]),
                clip_z=float(data.get("clip_z", 5.0)),
            )
        except (KeyError, TypeError, ValueError) as exc:
            raise EngineError(
                ErrorCode.ARTIFACT_SCHEMA_ERROR,
                f"Malformed FeatureSpec entry: {exc}",
            ) from exc
        spec.validate_definition()
        return spec


@dataclass(frozen=True)
class LayerSpec:
    """Dense layer specification inside a model manifest."""

    name: str
    in_dim: int
    out_dim: int
    activation: str

    def validate_definition(self) -> None:
        if self.in_dim <= 0 or self.out_dim <= 0:
            raise EngineError(
                ErrorCode.ARTIFACT_SCHEMA_ERROR,
                f"Layer {self.name!r} dimensions must be positive: ({self.in_dim}, {self.out_dim})",
            )
        if self.activation not in ALLOWED_ACTIVATIONS:
            raise EngineError(
                ErrorCode.ARTIFACT_SCHEMA_ERROR,
                f"Unsupported activation {self.activation!r} in layer {self.name!r}",
            )

    @property
    def num_weights(self) -> int:
        return self.in_dim * self.out_dim

    @property
    def num_biases(self) -> int:
        return self.out_dim

    @property
    def total_params(self) -> int:
        return self.num_weights + self.num_biases

    def to_dict(self) -> Dict[str, Any]:
        return {
            "name": self.name,
            "in_dim": self.in_dim,
            "out_dim": self.out_dim,
            "activation": self.activation,
        }

    @classmethod
    def from_dict(cls, data: Mapping[str, Any]) -> "LayerSpec":
        try:
            spec = cls(
                name=str(data["name"]),
                in_dim=int(data["in_dim"]),
                out_dim=int(data["out_dim"]),
                activation=str(data["activation"]),
            )
        except (KeyError, TypeError, ValueError) as exc:
            raise EngineError(
                ErrorCode.ARTIFACT_SCHEMA_ERROR, f"Malformed LayerSpec entry: {exc}"
            ) from exc
        spec.validate_definition()
        return spec


@dataclass(frozen=True)
class OutputSpec:
    """Canonical specification for model prediction output."""

    name: str
    unit: str
    task_type: str
    prediction_tolerance_abs: float = 1e-9

    def to_dict(self) -> Dict[str, Any]:
        return {
            "name": self.name,
            "unit": self.unit,
            "task_type": self.task_type,
            "prediction_tolerance_abs": self.prediction_tolerance_abs,
        }

    @classmethod
    def from_dict(cls, data: Mapping[str, Any]) -> "OutputSpec":
        try:
            tol = float(data.get("prediction_tolerance_abs", 1e-9))
            if not math.isfinite(tol) or tol <= 0.0:
                raise ValueError("prediction_tolerance_abs must be finite and > 0")
            return cls(
                name=str(data["name"]),
                unit=str(data["unit"]),
                task_type=str(data["task_type"]),
                prediction_tolerance_abs=tol,
            )
        except (KeyError, TypeError, ValueError) as exc:
            raise EngineError(
                ErrorCode.ARTIFACT_SCHEMA_ERROR, f"Malformed OutputSpec: {exc}"
            ) from exc


@dataclass(frozen=True)
class StalenessPolicy:
    """Point-in-time feature freshness and availability contract."""

    max_feature_age_ms: int = 5000
    max_clock_skew_ms: int = 250
    require_availability_not_after_inference: bool = True

    def to_dict(self) -> Dict[str, Any]:
        return {
            "max_feature_age_ms": self.max_feature_age_ms,
            "max_clock_skew_ms": self.max_clock_skew_ms,
            "require_availability_not_after_inference": self.require_availability_not_after_inference,
        }

    @classmethod
    def from_dict(cls, data: Mapping[str, Any]) -> "StalenessPolicy":
        try:
            max_age = int(data.get("max_feature_age_ms", 5000))
            max_skew = int(data.get("max_clock_skew_ms", 250))
            if max_age <= 0 or max_skew < 0:
                raise ValueError("Invalid staleness policy bounds")
            return cls(
                max_feature_age_ms=max_age,
                max_clock_skew_ms=max_skew,
                require_availability_not_after_inference=bool(
                    data.get("require_availability_not_after_inference", True)
                ),
            )
        except (TypeError, ValueError) as exc:
            raise EngineError(
                ErrorCode.ARTIFACT_SCHEMA_ERROR, f"Malformed StalenessPolicy: {exc}"
            ) from exc


@dataclass(frozen=True)
class ModelManifest:
    """Immutable, versioned contract for a verified model artifact bundle."""

    schema_version: str
    model_id: str
    model_version: str
    architecture: str
    serialization_format: str
    created_at_utc: str
    weights_file: str
    weights_sha256: str
    weights_byte_size: int
    golden_file: str
    golden_sha256: str
    features: Tuple[FeatureSpec, ...]
    layers: Tuple[LayerSpec, ...]
    output: OutputSpec
    staleness_policy: StalenessPolicy
    training_metadata: Dict[str, Any] = field(default_factory=dict)

    def validate(self) -> None:
        if self.schema_version != SCHEMA_VERSION:
            raise EngineError(
                ErrorCode.ARTIFACT_SCHEMA_ERROR,
                f"Unsupported manifest schema_version {self.schema_version!r}; expected {SCHEMA_VERSION!r}.",
            )
        if self.architecture not in ALLOWED_ARCHITECTURES:
            raise EngineError(
                ErrorCode.ARTIFACT_SCHEMA_ERROR,
                f"Unsupported architecture {self.architecture!r}.",
            )
        if self.serialization_format not in ALLOWED_SERIALIZATION_FORMATS:
            raise EngineError(
                ErrorCode.UNTRUSTED_ARTIFACT_FORMAT,
                f"Disallowed serialization_format {self.serialization_format!r}.",
            )
        parse_utc_timestamp(self.created_at_utc)
        if not self.features:
            raise EngineError(
                ErrorCode.ARTIFACT_SCHEMA_ERROR, "Manifest must define at least one feature."
            )
        seen_names = set()
        for f in self.features:
            f.validate_definition()
            if f.name in seen_names:
                raise EngineError(
                    ErrorCode.ARTIFACT_SCHEMA_ERROR,
                    f"Duplicate feature name {f.name!r} in manifest.",
                )
            seen_names.add(f.name)
        if not self.layers:
            raise EngineError(
                ErrorCode.ARTIFACT_SCHEMA_ERROR, "Manifest must define at least one layer."
            )
        prev_dim = len(self.features)
        expected_params = 0
        for layer in self.layers:
            layer.validate_definition()
            if layer.in_dim != prev_dim:
                raise EngineError(
                    ErrorCode.ARTIFACT_SCHEMA_ERROR,
                    f"Layer {layer.name!r} expects in_dim={layer.in_dim}, but previous dim is {prev_dim}.",
                )
            prev_dim = layer.out_dim
            expected_params += layer.total_params
        if prev_dim != 1:
            raise EngineError(
                ErrorCode.ARTIFACT_SCHEMA_ERROR,
                f"Final layer out_dim must be 1 for scalar financial signal, got {prev_dim}.",
            )
        expected_bytes = expected_params * 8  # IEEE-754 float64
        if self.weights_byte_size != expected_bytes:
            raise EngineError(
                ErrorCode.ARTIFACT_SCHEMA_ERROR,
                f"weights_byte_size mismatch: manifest says {self.weights_byte_size}, layer spec requires {expected_bytes}.",
            )

    @property
    def feature_names(self) -> Tuple[str, ...]:
        return tuple(f.name for f in self.features)

    def to_dict(self) -> Dict[str, Any]:
        return {
            "schema_version": self.schema_version,
            "model_id": self.model_id,
            "model_version": self.model_version,
            "architecture": self.architecture,
            "serialization_format": self.serialization_format,
            "created_at_utc": self.created_at_utc,
            "weights_file": self.weights_file,
            "weights_sha256": self.weights_sha256,
            "weights_byte_size": self.weights_byte_size,
            "golden_file": self.golden_file,
            "golden_sha256": self.golden_sha256,
            "features": [f.to_dict() for f in self.features],
            "layers": [layer.to_dict() for layer in self.layers],
            "output": self.output.to_dict(),
            "staleness_policy": self.staleness_policy.to_dict(),
            "training_metadata": self.training_metadata,
        }

    @classmethod
    def from_dict(cls, data: Mapping[str, Any]) -> "ModelManifest":
        try:
            manifest = cls(
                schema_version=str(data["schema_version"]),
                model_id=str(data["model_id"]),
                model_version=str(data["model_version"]),
                architecture=str(data["architecture"]),
                serialization_format=str(data["serialization_format"]),
                created_at_utc=str(data["created_at_utc"]),
                weights_file=str(data["weights_file"]),
                weights_sha256=str(data["weights_sha256"]),
                weights_byte_size=int(data["weights_byte_size"]),
                golden_file=str(data["golden_file"]),
                golden_sha256=str(data["golden_sha256"]),
                features=tuple(FeatureSpec.from_dict(f) for f in data["features"]),
                layers=tuple(LayerSpec.from_dict(l) for l in data["layers"]),
                output=OutputSpec.from_dict(data["output"]),
                staleness_policy=StalenessPolicy.from_dict(
                    data.get("staleness_policy", {})
                ),
                training_metadata=dict(data.get("training_metadata", {})),
            )
        except EngineError:
            raise
        except (KeyError, TypeError, ValueError) as exc:
            raise EngineError(
                ErrorCode.ARTIFACT_SCHEMA_ERROR, f"Malformed ModelManifest: {exc}"
            ) from exc
        manifest.validate()
        return manifest


@dataclass(frozen=True)
class InferenceRequest:
    """Single-record inference request with point-in-time UTC timestamps."""

    request_id: str
    instrument_id: str
    currency: str
    event_ts_utc: str
    available_ts_utc: str
    features: Mapping[str, float]
    model_version: Optional[str] = None
    inference_ts_utc: Optional[str] = None


@dataclass(frozen=True)
class InferenceResponse:
    """Observable, versioned prediction output with latency and provenance."""

    request_id: str
    instrument_id: str
    currency: str
    model_id: str
    model_version: str
    artifact_sha256: str
    runtime_name: str
    output_name: str
    output_unit: str
    prediction: float
    inference_ts_utc: str
    feature_age_ms: float
    latency_us: float
    order_execution_enabled: bool = False
    route_tag: str = "primary"

    def to_dict(self) -> Dict[str, Any]:
        return {
            "request_id": self.request_id,
            "instrument_id": self.instrument_id,
            "currency": self.currency,
            "model_id": self.model_id,
            "model_version": self.model_version,
            "artifact_sha256": self.artifact_sha256,
            "runtime_name": self.runtime_name,
            "output_name": self.output_name,
            "output_unit": self.output_unit,
            "prediction": self.prediction,
            "inference_ts_utc": self.inference_ts_utc,
            "feature_age_ms": self.feature_age_ms,
            "latency_us": self.latency_us,
            "order_execution_enabled": self.order_execution_enabled,
            "route_tag": self.route_tag,
        }


@dataclass(frozen=True)
class BatchInferenceRequest:
    """Batch inference request containing multiple point-in-time feature rows."""

    batch_id: str
    requests: Sequence[InferenceRequest]
    model_version: Optional[str] = None


@dataclass(frozen=True)
class BatchInferenceResponse:
    """Batch inference response preserving ordering and per-batch telemetry."""

    batch_id: str
    model_id: str
    model_version: str
    artifact_sha256: str
    runtime_name: str
    responses: Tuple[InferenceResponse, ...]
    batch_latency_us: float
    route_tag: str = "primary"
