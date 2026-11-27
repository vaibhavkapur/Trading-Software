"""Low-Latency ML Inference Engine.

Provides versioned, integrity-verified model artifacts, canonical preprocessing,
a deterministic Python reference scorer, a compiled C++20 native runtime adapter,
versioned rollout/rollback routing with safe failure handling, and a reproducible
latency/load benchmark harness.
"""

from .contracts import (
    BatchInferenceRequest,
    BatchInferenceResponse,
    EngineError,
    ErrorCode,
    FeatureSpec,
    InferenceRequest,
    InferenceResponse,
    ModelManifest,
    OutputSpec,
    StalenessPolicy,
)

__all__ = [
    "BatchInferenceRequest",
    "BatchInferenceResponse",
    "EngineError",
    "ErrorCode",
    "FeatureSpec",
    "InferenceRequest",
    "InferenceResponse",
    "ModelManifest",
    "OutputSpec",
    "StalenessPolicy",
]
