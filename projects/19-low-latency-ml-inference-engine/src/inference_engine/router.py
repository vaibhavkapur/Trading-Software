"""Versioned model registry, routing, atomic reload, rollback, overload shedding, and telemetry."""

from __future__ import annotations

import hashlib
import threading
import time
from collections import Counter
from dataclasses import dataclass
from pathlib import Path
from typing import Any, Callable, Dict, List, Optional, Tuple

from .artifact import LoadedModelArtifact, load_verified_artifact
from .contracts import (
    BatchInferenceRequest,
    BatchInferenceResponse,
    EngineError,
    ErrorCode,
    InferenceRequest,
    InferenceResponse,
    StalenessPolicy,
    format_utc_timestamp,
    utc_now,
)
from .runtime_adapter import RuntimeAdapter, create_adapter


@dataclass(frozen=True)
class RoutingPolicy:
    """Declarative routing rules supporting primary, canary, and explicit version pinning."""

    primary_version: str
    canary_version: Optional[str] = None
    canary_weight: float = 0.0

    def validate(self, loaded_versions: set[str]) -> None:
        if self.primary_version not in loaded_versions:
            raise EngineError(
                ErrorCode.MODEL_VERSION_NOT_FOUND,
                f"Primary version {self.primary_version!r} is not loaded.",
            )
        if not (0.0 <= self.canary_weight <= 1.0):
            raise EngineError(
                ErrorCode.ARTIFACT_SCHEMA_ERROR,
                f"canary_weight must be in [0.0, 1.0], got {self.canary_weight}",
            )
        if self.canary_weight > 0.0:
            if not self.canary_version or self.canary_version not in loaded_versions:
                raise EngineError(
                    ErrorCode.MODEL_VERSION_NOT_FOUND,
                    f"Canary version {self.canary_version!r} is not loaded.",
                )


@dataclass
class _LoadedEntry:
    version: str
    bundle_dir: Path
    artifact: LoadedModelArtifact
    adapter: RuntimeAdapter
    loaded_at_utc: str


class InferenceRouter:
    """Thread-safe inference service with versioned rollout, atomic reload, rollback, and overload shedding."""

    def __init__(
        self,
        *,
        runtime: str = "cpp_native",
        max_in_flight: int = 16,
        max_batch_size: int = 256,
        order_execution_enabled: bool = False,
        verify_sha256: bool = True,
        verify_parity: bool = True,
        library_path: Optional[Path] = None,
        staleness_override: Optional[StalenessPolicy] = None,
        pre_score_hook: Optional[Callable[[], None]] = None,
    ) -> None:
        if max_in_flight <= 0:
            raise ValueError("max_in_flight must be positive")
        self._runtime = runtime
        self._max_in_flight = max_in_flight
        self._max_batch_size = max_batch_size
        self._order_execution_enabled = order_execution_enabled
        self._verify_sha256 = verify_sha256
        self._verify_parity = verify_parity
        self._library_path = library_path
        self._staleness_override = staleness_override
        self._pre_score_hook = pre_score_hook

        self._lock = threading.RLock()
        self._entries: Dict[str, _LoadedEntry] = {}
        self._policy: Optional[RoutingPolicy] = None
        self._previous_policies: List[RoutingPolicy] = []
        self._rollout_events: List[Dict[str, Any]] = []

        self._in_flight = 0
        self._in_flight_lock = threading.Lock()

        # Observability counters
        self._metrics_lock = threading.Lock()
        self._requests_by_version: Counter[str] = Counter()
        self._requests_by_route: Counter[str] = Counter()
        self._errors_by_code: Counter[str] = Counter()
        self._overload_rejections: int = 0
        self._reload_successes: int = 0
        self._reload_failures: int = 0
        self._rollbacks: int = 0

    @property
    def order_execution_enabled(self) -> bool:
        return self._order_execution_enabled

    def assert_order_execution_allowed(self) -> None:
        """Fail-closed guard ensuring real/paper order execution cannot occur unintentionally."""
        if not self._order_execution_enabled:
            raise EngineError(
                ErrorCode.EXECUTION_DISABLED,
                "Order execution is disabled by operational policy (inference-only mode).",
            )

    def load_version(
        self,
        bundle_dir: Path,
        *,
        activate_as_primary: bool = False,
    ) -> Dict[str, Any]:
        """Verify and stage a model version; atomically swap active routing if requested.

        On any verification or parity failure, active routing remains untouched.
        """
        t0 = time.perf_counter_ns()
        try:
            artifact = load_verified_artifact(
                Path(bundle_dir), verify_sha256=self._verify_sha256
            )
            adapter = create_adapter(
                artifact,
                runtime=self._runtime,
                library_path=self._library_path,
                verify_parity=self._verify_parity,
            )
        except Exception as exc:
            with self._metrics_lock:
                self._reload_failures += 1
                code = exc.code.value if isinstance(exc, EngineError) else ErrorCode.MODEL_RELOAD_FAILED.value
                self._errors_by_code[code] += 1
            with self._lock:
                self._rollout_events.append(
                    {
                        "timestamp_utc": format_utc_timestamp(utc_now()),
                        "action": "load_failed",
                        "bundle_dir": str(bundle_dir),
                        "error": str(exc),
                    }
                )
            if isinstance(exc, EngineError):
                raise
            raise EngineError(
                ErrorCode.MODEL_RELOAD_FAILED,
                f"Failed to load model bundle {bundle_dir}: {exc}",
            ) from exc

        version = artifact.manifest.model_version
        entry = _LoadedEntry(
            version=version,
            bundle_dir=Path(bundle_dir),
            artifact=artifact,
            adapter=adapter,
            loaded_at_utc=format_utc_timestamp(utc_now()),
        )

        old_adapter: Optional[RuntimeAdapter] = None
        with self._lock:
            if version in self._entries:
                old_adapter = self._entries[version].adapter
            self._entries[version] = entry
            if activate_as_primary or self._policy is None:
                if self._policy is not None:
                    self._previous_policies.append(self._policy)
                self._policy = RoutingPolicy(primary_version=version)
            self._rollout_events.append(
                {
                    "timestamp_utc": entry.loaded_at_utc,
                    "action": "load_and_activate" if activate_as_primary else "load_staged",
                    "model_id": artifact.manifest.model_id,
                    "model_version": version,
                    "artifact_sha256": artifact.bundle_sha256,
                    "runtime": adapter.runtime_name,
                }
            )
        if old_adapter is not None:
            old_adapter.close()

        with self._metrics_lock:
            self._reload_successes += 1

        load_ms = (time.perf_counter_ns() - t0) / 1_000_000.0
        return {
            "model_id": artifact.manifest.model_id,
            "model_version": version,
            "artifact_sha256": artifact.bundle_sha256,
            "runtime": adapter.runtime_name,
            "load_ms": round(load_ms, 3),
            "active_primary": self._policy.primary_version if self._policy else None,
        }

    def set_routing_policy(self, policy: RoutingPolicy) -> None:
        """Atomically update routing policy (e.g., canary split or primary promotion)."""
        with self._lock:
            policy.validate(set(self._entries.keys()))
            if self._policy is not None:
                self._previous_policies.append(self._policy)
            self._policy = policy
            self._rollout_events.append(
                {
                    "timestamp_utc": format_utc_timestamp(utc_now()),
                    "action": "routing_updated",
                    "primary_version": policy.primary_version,
                    "canary_version": policy.canary_version,
                    "canary_weight": policy.canary_weight,
                }
            )

    def rollback(self) -> RoutingPolicy:
        """Atomically revert to the previous routing policy / primary model version."""
        with self._lock:
            if not self._previous_policies:
                raise EngineError(
                    ErrorCode.MODEL_RELOAD_FAILED,
                    "No previous routing policy available to roll back to.",
                )
            target = self._previous_policies.pop()
            target.validate(set(self._entries.keys()))
            self._policy = target
            self._rollout_events.append(
                {
                    "timestamp_utc": format_utc_timestamp(utc_now()),
                    "action": "rollback",
                    "primary_version": target.primary_version,
                    "canary_version": target.canary_version,
                    "canary_weight": target.canary_weight,
                }
            )
        with self._metrics_lock:
            self._rollbacks += 1
        return target

    def _select_entry(
        self, requested_version: Optional[str], routing_key: str
    ) -> Tuple[_LoadedEntry, str]:
        with self._lock:
            if self._policy is None:
                raise EngineError(
                    ErrorCode.MODEL_VERSION_NOT_FOUND, "No model versions are loaded."
                )
            if requested_version is not None:
                entry = self._entries.get(requested_version)
                if entry is None:
                    raise EngineError(
                        ErrorCode.MODEL_VERSION_NOT_FOUND,
                        f"Requested model_version {requested_version!r} is not loaded.",
                        details={"loaded_versions": sorted(self._entries.keys())},
                    )
                return entry, "pinned"

            pol = self._policy
            if pol.canary_version and pol.canary_weight > 0.0:
                digest = hashlib.sha256(routing_key.encode("utf-8")).digest()
                bucket = int.from_bytes(digest[:4], "big") / float(0xFFFFFFFF)
                if bucket < pol.canary_weight:
                    return self._entries[pol.canary_version], "canary"
            return self._entries[pol.primary_version], "primary"

    def _acquire_slot(self) -> None:
        with self._in_flight_lock:
            if self._in_flight >= self._max_in_flight:
                with self._metrics_lock:
                    self._overload_rejections += 1
                    self._errors_by_code[ErrorCode.OVERLOAD_REJECTED.value] += 1
                raise EngineError(
                    ErrorCode.OVERLOAD_REJECTED,
                    f"Inference engine overloaded: max_in_flight={self._max_in_flight} reached.",
                    details={"max_in_flight": self._max_in_flight},
                )
            self._in_flight += 1

    def _release_slot(self) -> None:
        with self._in_flight_lock:
            self._in_flight = max(0, self._in_flight - 1)

    def predict(self, request: InferenceRequest) -> InferenceResponse:
        """Score a single InferenceRequest with overload protection and version observability."""
        self._acquire_slot()
        try:
            if self._pre_score_hook is not None:
                self._pre_score_hook()
            entry, route_tag = self._select_entry(
                request.model_version, request.request_id
            )
            resp = entry.adapter.score_request(
                request,
                staleness_override=self._staleness_override,
                route_tag=route_tag,
                order_execution_enabled=self._order_execution_enabled,
            )
            with self._metrics_lock:
                self._requests_by_version[resp.model_version] += 1
                self._requests_by_route[route_tag] += 1
            return resp
        except EngineError as exc:
            with self._metrics_lock:
                self._errors_by_code[exc.code.value] += 1
            raise
        finally:
            self._release_slot()

    def predict_batch(self, batch_request: BatchInferenceRequest) -> BatchInferenceResponse:
        """Score a batch of requests atomically under one routed model version."""
        self._acquire_slot()
        try:
            if self._pre_score_hook is not None:
                self._pre_score_hook()
            entry, route_tag = self._select_entry(
                batch_request.model_version, batch_request.batch_id
            )
            resp = entry.adapter.score_batch(
                batch_request,
                max_batch_size=self._max_batch_size,
                staleness_override=self._staleness_override,
                route_tag=route_tag,
                order_execution_enabled=self._order_execution_enabled,
            )
            with self._metrics_lock:
                self._requests_by_version[resp.model_version] += len(resp.responses)
                self._requests_by_route[route_tag] += len(resp.responses)
            return resp
        except EngineError as exc:
            with self._metrics_lock:
                self._errors_by_code[exc.code.value] += 1
            raise
        finally:
            self._release_slot()

    def health_snapshot(self) -> Dict[str, Any]:
        with self._lock:
            versions = {
                v: {
                    "model_id": e.artifact.manifest.model_id,
                    "artifact_sha256": e.artifact.bundle_sha256,
                    "runtime": e.adapter.runtime_name,
                    "loaded_at_utc": e.loaded_at_utc,
                }
                for v, e in sorted(self._entries.items())
            }
            policy = (
                {
                    "primary_version": self._policy.primary_version,
                    "canary_version": self._policy.canary_version,
                    "canary_weight": self._policy.canary_weight,
                }
                if self._policy
                else None
            )
        with self._in_flight_lock:
            in_flight = self._in_flight
        return {
            "status": "healthy" if policy is not None else "not_ready",
            "order_execution_enabled": self._order_execution_enabled,
            "runtime": self._runtime,
            "max_in_flight": self._max_in_flight,
            "current_in_flight": in_flight,
            "routing_policy": policy,
            "loaded_versions": versions,
        }

    def metrics_snapshot(self) -> Dict[str, Any]:
        with self._metrics_lock:
            metrics = {
                "requests_by_version": dict(self._requests_by_version),
                "requests_by_route": dict(self._requests_by_route),
                "errors_by_code": dict(self._errors_by_code),
                "overload_rejections": self._overload_rejections,
                "reload_successes": self._reload_successes,
                "reload_failures": self._reload_failures,
                "rollbacks": self._rollbacks,
            }
        with self._lock:
            metrics["rollout_events"] = list(self._rollout_events)
        return metrics

    def close(self) -> None:
        with self._lock:
            for entry in self._entries.values():
                entry.adapter.close()
            self._entries.clear()
            self._policy = None
