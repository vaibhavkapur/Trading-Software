"""Safe model artifact bundling, SHA-256 integrity verification, and loading.

Explicitly rejects untrusted serialization formats (pickle, joblib, torch checkpoints).
"""

from __future__ import annotations

import hashlib
import json
import math
import struct
from array import array
from dataclasses import dataclass
from pathlib import Path
from typing import Any, Dict, List, Mapping, Sequence, Tuple

from .contracts import (
    ALLOWED_SERIALIZATION_FORMATS,
    UNTRUSTED_EXTENSIONS,
    EngineError,
    ErrorCode,
    LayerSpec,
    ModelManifest,
)


def sha256_bytes(data: bytes) -> str:
    return hashlib.sha256(data).hexdigest()


def sha256_file(path: Path) -> str:
    h = hashlib.sha256()
    with path.open("rb") as f:
        while True:
            chunk = f.read(65536)
            if not chunk:
                break
            h.update(chunk)
    return h.hexdigest()


@dataclass(frozen=True)
class LayerParameters:
    """Unpacked read-only parameters for a single dense layer."""

    spec: LayerSpec
    weights: Tuple[Tuple[float, ...], ...]  # shape: [out_dim][in_dim]
    biases: Tuple[float, ...]  # shape: [out_dim]


@dataclass(frozen=True)
class GoldenVector:
    """Golden input/output parity fixture bundled inside a model artifact."""

    case_id: str
    raw_features: Dict[str, float]
    expected_prediction: float


@dataclass(frozen=True)
class LoadedModelArtifact:
    """Verified, immutable in-memory model bundle ready for scoring."""

    bundle_dir: Path
    manifest: ModelManifest
    bundle_sha256: str
    weights_flat: array  # contiguous float64 buffer matching weights.bin
    layers: Tuple[LayerParameters, ...]
    golden_vectors: Tuple[GoldenVector, ...]


def _check_for_untrusted_files(bundle_dir: Path) -> None:
    """Refuse any artifact directory containing untrusted serialized model files."""
    if not bundle_dir.exists():
        raise EngineError(
            ErrorCode.ARTIFACT_INTEGRITY_ERROR,
            f"Artifact path does not exist: {bundle_dir}",
        )
    if bundle_dir.is_file():
        suffix = bundle_dir.suffix.lower()
        if suffix in UNTRUSTED_EXTENSIONS:
            raise EngineError(
                ErrorCode.UNTRUSTED_ARTIFACT_FORMAT,
                f"Refusing untrusted serialized artifact file {bundle_dir.name!r} (extension {suffix!r}).",
                details={"path": str(bundle_dir), "extension": suffix},
            )
        raise EngineError(
            ErrorCode.ARTIFACT_SCHEMA_ERROR,
            f"Artifact bundle path must be a directory containing manifest.json, got file: {bundle_dir}",
        )

    for entry in bundle_dir.iterdir():
        suffix = entry.suffix.lower()
        if suffix in UNTRUSTED_EXTENSIONS:
            raise EngineError(
                ErrorCode.UNTRUSTED_ARTIFACT_FORMAT,
                f"Artifact directory contains disallowed untrusted file {entry.name!r}.",
                details={"file": entry.name, "extension": suffix},
            )


def _unpack_layers(
    flat: Sequence[float], layer_specs: Sequence[LayerSpec]
) -> Tuple[LayerParameters, ...]:
    offset = 0
    layers: List[LayerParameters] = []
    for spec in layer_specs:
        rows: List[Tuple[float, ...]] = []
        for _ in range(spec.out_dim):
            row = tuple(flat[offset : offset + spec.in_dim])
            offset += spec.in_dim
            rows.append(row)
        biases = tuple(flat[offset : offset + spec.out_dim])
        offset += spec.out_dim
        layers.append(LayerParameters(spec=spec, weights=tuple(rows), biases=biases))
    if offset != len(flat):
        raise EngineError(
            ErrorCode.ARTIFACT_INTEGRITY_ERROR,
            f"Weight buffer parameter count mismatch: consumed {offset}, total {len(flat)}.",
        )
    return tuple(layers)


def save_verified_artifact(
    bundle_dir: Path,
    manifest_template: Mapping[str, Any],
    flat_weights: Sequence[float],
    golden_cases: Sequence[Mapping[str, Any]],
) -> ModelManifest:
    """Write a safe IEEE-754 weights.bin, reference_golden.json, and SHA-256 manifest.json."""
    bundle_dir.mkdir(parents=True, exist_ok=True)

    for idx, w in enumerate(flat_weights):
        if not isinstance(w, (int, float)) or not math.isfinite(float(w)):
            raise EngineError(
                ErrorCode.ARTIFACT_INTEGRITY_ERROR,
                f"Refusing to save non-finite weight at index {idx}: {w!r}",
            )

    weights_bytes = struct.pack(f"<{len(flat_weights)}d", *(float(w) for w in flat_weights))
    weights_path = bundle_dir / "weights.bin"
    weights_path.write_bytes(weights_bytes)
    weights_digest = sha256_bytes(weights_bytes)

    golden_payload = {
        "schema_version": manifest_template["schema_version"],
        "model_id": manifest_template["model_id"],
        "model_version": manifest_template["model_version"],
        "cases": list(golden_cases),
    }
    golden_bytes = (json.dumps(golden_payload, indent=2, sort_keys=True) + "\n").encode("utf-8")
    golden_path = bundle_dir / "reference_golden.json"
    golden_path.write_bytes(golden_bytes)
    golden_digest = sha256_bytes(golden_bytes)

    full_manifest_dict = dict(manifest_template)
    full_manifest_dict.update(
        {
            "weights_file": "weights.bin",
            "weights_sha256": weights_digest,
            "weights_byte_size": len(weights_bytes),
            "golden_file": "reference_golden.json",
            "golden_sha256": golden_digest,
        }
    )
    manifest = ModelManifest.from_dict(full_manifest_dict)

    manifest_bytes = (
        json.dumps(manifest.to_dict(), indent=2, sort_keys=True) + "\n"
    ).encode("utf-8")
    (bundle_dir / "manifest.json").write_bytes(manifest_bytes)
    return manifest


def load_verified_artifact(
    bundle_dir: Path,
    *,
    verify_sha256: bool = True,
) -> LoadedModelArtifact:
    """Load and cryptographically verify a safe model artifact bundle."""
    bundle_path = Path(bundle_dir)
    _check_for_untrusted_files(bundle_path)

    manifest_path = bundle_path / "manifest.json"
    if not manifest_path.is_file():
        raise EngineError(
            ErrorCode.ARTIFACT_INTEGRITY_ERROR,
            f"Missing manifest.json in artifact bundle {bundle_path}.",
        )

    try:
        manifest_bytes = manifest_path.read_bytes()
        manifest_data = json.loads(manifest_bytes.decode("utf-8"))
    except (OSError, UnicodeDecodeError, json.JSONDecodeError) as exc:
        raise EngineError(
            ErrorCode.ARTIFACT_SCHEMA_ERROR,
            f"Failed to parse manifest.json: {exc}",
        ) from exc

    if not isinstance(manifest_data, dict):
        raise EngineError(
            ErrorCode.ARTIFACT_SCHEMA_ERROR, "manifest.json root must be a JSON object."
        )

    fmt = manifest_data.get("serialization_format")
    if fmt not in ALLOWED_SERIALIZATION_FORMATS:
        raise EngineError(
            ErrorCode.UNTRUSTED_ARTIFACT_FORMAT,
            f"Disallowed artifact serialization_format: {fmt!r}.",
        )

    manifest = ModelManifest.from_dict(manifest_data)

    # Prevent path traversal in manifest file references
    weights_path = (bundle_path / manifest.weights_file).resolve()
    golden_path = (bundle_path / manifest.golden_file).resolve()
    resolved_bundle = bundle_path.resolve()
    if resolved_bundle not in weights_path.parents or resolved_bundle not in golden_path.parents:
        raise EngineError(
            ErrorCode.ARTIFACT_INTEGRITY_ERROR,
            "Manifest references file outside bundle directory.",
        )

    if not weights_path.is_file():
        raise EngineError(
            ErrorCode.ARTIFACT_INTEGRITY_ERROR,
            f"Missing weights file {manifest.weights_file!r} in {bundle_path}.",
        )
    if not golden_path.is_file():
        raise EngineError(
            ErrorCode.ARTIFACT_INTEGRITY_ERROR,
            f"Missing golden reference file {manifest.golden_file!r} in {bundle_path}.",
        )

    weights_bytes = weights_path.read_bytes()
    if len(weights_bytes) != manifest.weights_byte_size:
        raise EngineError(
            ErrorCode.ARTIFACT_INTEGRITY_ERROR,
            f"weights.bin size mismatch: expected {manifest.weights_byte_size} bytes, got {len(weights_bytes)}.",
        )
    if verify_sha256:
        actual_w_hash = sha256_bytes(weights_bytes)
        if actual_w_hash != manifest.weights_sha256:
            raise EngineError(
                ErrorCode.ARTIFACT_INTEGRITY_ERROR,
                f"weights.bin SHA-256 mismatch: expected {manifest.weights_sha256}, got {actual_w_hash}.",
                details={
                    "expected_sha256": manifest.weights_sha256,
                    "actual_sha256": actual_w_hash,
                },
            )

    golden_bytes = golden_path.read_bytes()
    if verify_sha256:
        actual_g_hash = sha256_bytes(golden_bytes)
        if actual_g_hash != manifest.golden_sha256:
            raise EngineError(
                ErrorCode.ARTIFACT_INTEGRITY_ERROR,
                f"reference_golden.json SHA-256 mismatch: expected {manifest.golden_sha256}, got {actual_g_hash}.",
            )

    num_doubles = len(weights_bytes) // 8
    unpacked = struct.unpack(f"<{num_doubles}d", weights_bytes)
    for idx, val in enumerate(unpacked):
        if not math.isfinite(val):
            raise EngineError(
                ErrorCode.ARTIFACT_INTEGRITY_ERROR,
                f"Non-finite parameter detected in weights.bin at offset {idx}.",
            )

    weights_flat = array("d", unpacked)
    layers = _unpack_layers(weights_flat, manifest.layers)

    try:
        golden_json = json.loads(golden_bytes.decode("utf-8"))
        golden_cases_raw = golden_json["cases"]
        golden_vectors = tuple(
            GoldenVector(
                case_id=str(c["case_id"]),
                raw_features={str(k): float(v) for k, v in c["raw_features"].items()},
                expected_prediction=float(c["expected_prediction"]),
            )
            for c in golden_cases_raw
        )
    except (KeyError, TypeError, ValueError, json.JSONDecodeError) as exc:
        raise EngineError(
            ErrorCode.ARTIFACT_SCHEMA_ERROR,
            f"Malformed reference_golden.json: {exc}",
        ) from exc

    bundle_digest = sha256_bytes(manifest_bytes + weights_bytes + golden_bytes)
    return LoadedModelArtifact(
        bundle_dir=bundle_path,
        manifest=manifest,
        bundle_sha256=bundle_digest,
        weights_flat=weights_flat,
        layers=layers,
        golden_vectors=golden_vectors,
    )
