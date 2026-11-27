"""Canonical feature validation, point-in-time staleness checks, and preprocessing."""

from __future__ import annotations

import math
from array import array
from datetime import datetime
from typing import List, Mapping, Optional, Sequence, Tuple

from .contracts import (
    EngineError,
    ErrorCode,
    FeatureSpec,
    InferenceRequest,
    ModelManifest,
    StalenessPolicy,
    parse_utc_timestamp,
    utc_now,
)


def validate_timestamps(
    event_ts_utc: str,
    available_ts_utc: str,
    policy: StalenessPolicy,
    *,
    inference_dt: Optional[datetime] = None,
) -> Tuple[datetime, float]:
    """Validate point-in-time timestamps and return (inference_dt, feature_age_ms).

    Enforces:
    1. All timestamps are valid UTC ISO-8601.
    2. event_ts_utc <= available_ts_utc (availability cannot precede the market event).
    3. available_ts_utc <= inference_dt + max_clock_skew_ms (no look-ahead leakage).
    4. inference_dt - event_ts_utc <= max_feature_age_ms (stale feature rejection).
    """
    event_dt = parse_utc_timestamp(event_ts_utc)
    avail_dt = parse_utc_timestamp(available_ts_utc)
    now_dt = inference_dt if inference_dt is not None else utc_now()

    if avail_dt < event_dt:
        raise EngineError(
            ErrorCode.INVALID_TIMESTAMP,
            f"available_ts_utc ({available_ts_utc}) precedes event_ts_utc ({event_ts_utc}).",
            details={
                "event_ts_utc": event_ts_utc,
                "available_ts_utc": available_ts_utc,
            },
        )

    skew_ms = (avail_dt - now_dt).total_seconds() * 1000.0
    if policy.require_availability_not_after_inference and skew_ms > policy.max_clock_skew_ms:
        raise EngineError(
            ErrorCode.FUTURE_TIMESTAMP,
            f"Feature available_ts_utc is {skew_ms:.2f}ms in the future (limit {policy.max_clock_skew_ms}ms).",
            details={
                "available_ts_utc": available_ts_utc,
                "skew_ms": round(skew_ms, 3),
                "max_clock_skew_ms": policy.max_clock_skew_ms,
            },
        )

    feature_age_ms = (now_dt - event_dt).total_seconds() * 1000.0
    if feature_age_ms < -policy.max_clock_skew_ms:
        raise EngineError(
            ErrorCode.FUTURE_TIMESTAMP,
            f"Feature event_ts_utc is {-feature_age_ms:.2f}ms in the future (limit {policy.max_clock_skew_ms}ms).",
            details={
                "event_ts_utc": event_ts_utc,
                "feature_age_ms": round(feature_age_ms, 3),
            },
        )

    if feature_age_ms > policy.max_feature_age_ms:
        raise EngineError(
            ErrorCode.STALE_FEATURE,
            f"Feature age {feature_age_ms:.2f}ms exceeds max_feature_age_ms={policy.max_feature_age_ms}ms.",
            details={
                "event_ts_utc": event_ts_utc,
                "feature_age_ms": round(feature_age_ms, 3),
                "max_feature_age_ms": policy.max_feature_age_ms,
            },
        )

    return now_dt, max(0.0, feature_age_ms)


def extract_and_validate_raw_features(
    features: Mapping[str, float],
    feature_specs: Sequence[FeatureSpec],
    *,
    out_buffer: Optional[List[float]] = None,
) -> List[float]:
    """Extract ordered raw float64 values while enforcing strict schema and finite bounds."""
    if not isinstance(features, Mapping):
        raise EngineError(
            ErrorCode.UNEXPECTED_FEATURE_TYPE,
            f"Features payload must be a mapping, got {type(features).__name__}.",
        )

    n = len(feature_specs)
    if out_buffer is not None and len(out_buffer) == n:
        raw = out_buffer
    else:
        raw = [0.0] * n

    for idx, spec in enumerate(feature_specs):
        if spec.name not in features:
            raise EngineError(
                ErrorCode.MISSING_FEATURE,
                f"Required feature {spec.name!r} is missing from request.",
                details={"missing_feature": spec.name},
            )
        val = features[spec.name]
        if isinstance(val, bool) or not isinstance(val, (int, float)):
            raise EngineError(
                ErrorCode.UNEXPECTED_FEATURE_TYPE,
                f"Feature {spec.name!r} must be numeric, got {type(val).__name__}.",
                details={"feature": spec.name, "type": type(val).__name__},
            )
        fval = float(val)
        if not math.isfinite(fval):
            raise EngineError(
                ErrorCode.NON_FINITE_FEATURE,
                f"Feature {spec.name!r} has non-finite value {val!r}.",
                details={"feature": spec.name, "value": str(val)},
            )
        if fval < spec.min_allowed or fval > spec.max_allowed:
            raise EngineError(
                ErrorCode.FEATURE_OUT_OF_BOUNDS,
                f"Feature {spec.name!r}={fval} outside allowed range [{spec.min_allowed}, {spec.max_allowed}].",
                details={
                    "feature": spec.name,
                    "value": fval,
                    "min_allowed": spec.min_allowed,
                    "max_allowed": spec.max_allowed,
                },
            )
        raw[idx] = fval

    return raw


def standardize_features(
    raw_values: Sequence[float],
    feature_specs: Sequence[FeatureSpec],
    *,
    out_buffer: Optional[List[float]] = None,
) -> List[float]:
    """Canonical z-score standardization with symmetric clipping.

    Must remain numerically identical to the C++20 native kernel preprocessing step.
    """
    n = len(feature_specs)
    if len(raw_values) != n:
        raise EngineError(
            ErrorCode.MISSING_FEATURE,
            f"Expected {n} features, received {len(raw_values)}.",
        )
    if out_buffer is not None and len(out_buffer) == n:
        out = out_buffer
    else:
        out = [0.0] * n

    for idx, spec in enumerate(feature_specs):
        z = (raw_values[idx] - spec.mean) / spec.std
        if z > spec.clip_z:
            z = spec.clip_z
        elif z < -spec.clip_z:
            z = -spec.clip_z
        out[idx] = z
    return out


def prepare_request(
    request: InferenceRequest,
    manifest: ModelManifest,
    *,
    staleness_override: Optional[StalenessPolicy] = None,
) -> Tuple[List[float], datetime, float]:
    """Validate timestamps + features for an InferenceRequest and return (raw_vec, inference_dt, age_ms)."""
    inf_dt = (
        parse_utc_timestamp(request.inference_ts_utc)
        if request.inference_ts_utc is not None
        else utc_now()
    )
    policy = staleness_override if staleness_override is not None else manifest.staleness_policy
    checked_dt, age_ms = validate_timestamps(
        request.event_ts_utc,
        request.available_ts_utc,
        policy,
        inference_dt=inf_dt,
    )
    raw = extract_and_validate_raw_features(request.features, manifest.features)
    return raw, checked_dt, age_ms


def pack_raw_batch(
    requests: Sequence[InferenceRequest],
    manifest: ModelManifest,
    *,
    staleness_override: Optional[StalenessPolicy] = None,
) -> Tuple[array, List[datetime], List[float]]:
    """Validate a batch of requests and pack raw float64 features contiguously."""
    buf = array("d")
    inf_dts: List[datetime] = []
    ages_ms: List[float] = []
    for req in requests:
        raw, inf_dt, age_ms = prepare_request(
            req, manifest, staleness_override=staleness_override
        )
        buf.extend(raw)
        inf_dts.append(inf_dt)
        ages_ms.append(age_ms)
    return buf, inf_dts, ages_ms
