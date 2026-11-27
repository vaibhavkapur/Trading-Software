"""Integration tests for versioned routing, canary splits, reload, rollback, and observability."""

from __future__ import annotations

import unittest
from pathlib import Path

from inference_engine.artifact import load_verified_artifact
from inference_engine.contracts import EngineError, ErrorCode, InferenceRequest
from inference_engine.router import InferenceRouter, RoutingPolicy


PROJECT_ROOT = Path(__file__).resolve().parents[2]
MODELS_ROOT = PROJECT_ROOT / "data" / "fixtures" / "models" / "lob_alpha_mlp"
V1_DIR = MODELS_ROOT / "v1.0.0"
V2_DIR = MODELS_ROOT / "v2.0.0"


class TestRouterAndRollout(unittest.TestCase):
    def setUp(self) -> None:
        self.art_v1 = load_verified_artifact(V1_DIR)
        self.sample_features = self.art_v1.golden_vectors[0].raw_features
        self.ts = "2026-01-15T15:00:00.000Z"

    def _req(self, req_id: str, model_version: str | None = None) -> InferenceRequest:
        return InferenceRequest(
            request_id=req_id,
            instrument_id="SYN-EQUITY-A",
            currency="USD",
            event_ts_utc=self.ts,
            available_ts_utc=self.ts,
            inference_ts_utc=self.ts,
            features=self.sample_features,
            model_version=model_version,
        )

    def test_primary_canary_pinning_and_rollback(self) -> None:
        router = InferenceRouter(runtime="cpp_native", max_in_flight=8)
        try:
            router.load_version(V1_DIR, activate_as_primary=True)
            router.load_version(V2_DIR, activate_as_primary=False)

            # Default goes to v1.0.0
            r1 = router.predict(self._req("r-primary"))
            self.assertEqual(r1.model_version, "1.0.0")
            self.assertEqual(r1.route_tag, "primary")

            # Explicit pin goes to v2.0.0
            r_pin = router.predict(self._req("r-pin", model_version="2.0.0"))
            self.assertEqual(r_pin.model_version, "2.0.0")
            self.assertEqual(r_pin.route_tag, "pinned")

            # 50% canary routes deterministically across both versions
            router.set_routing_policy(
                RoutingPolicy(primary_version="1.0.0", canary_version="2.0.0", canary_weight=0.5)
            )
            seen_versions = set()
            for i in range(40):
                seen_versions.add(router.predict(self._req(f"canary-{i}")).model_version)
            self.assertEqual(seen_versions, {"1.0.0", "2.0.0"})

            # Promote v2.0.0 to primary
            router.set_routing_policy(RoutingPolicy(primary_version="2.0.0"))
            self.assertEqual(router.predict(self._req("after-promote")).model_version, "2.0.0")

            # Roll back twice -> back to v1.0.0 primary
            router.rollback()
            pol = router.rollback()
            self.assertEqual(pol.primary_version, "1.0.0")
            self.assertIsNone(pol.canary_version)
            self.assertEqual(router.predict(self._req("after-rollback")).model_version, "1.0.0")

            # Order execution must remain disabled by default
            with self.assertRaises(EngineError) as ctx:
                router.assert_order_execution_allowed()
            self.assertEqual(ctx.exception.code, ErrorCode.EXECUTION_DISABLED)

            health = router.health_snapshot()
            metrics = router.metrics_snapshot()
            self.assertEqual(health["status"], "healthy")
            self.assertEqual(metrics["rollbacks"], 2)
            self.assertIn("1.0.0", metrics["requests_by_version"])
            self.assertIn("2.0.0", metrics["requests_by_version"])
        finally:
            router.close()


if __name__ == "__main__":
    unittest.main()
