"""Unit tests for domain contracts, timestamp hygiene, and canonical preprocessing."""

from __future__ import annotations

import math
import unittest
from datetime import datetime, timezone

from inference_engine.contracts import (
    EngineError,
    ErrorCode,
    FeatureSpec,
    StalenessPolicy,
    parse_utc_timestamp,
)
from inference_engine.preprocessing import (
    extract_and_validate_raw_features,
    standardize_features,
    validate_timestamps,
)


class TestContractsAndPreprocessing(unittest.TestCase):
    def setUp(self) -> None:
        self.specs = (
            FeatureSpec(
                name="imbalance",
                unit="ratio",
                dtype="float64",
                mean=0.1,
                std=0.5,
                min_allowed=-1.0,
                max_allowed=1.0,
                clip_z=3.0,
            ),
            FeatureSpec(
                name="spread_bps",
                unit="bps",
                dtype="float64",
                mean=2.0,
                std=1.0,
                min_allowed=0.0,
                max_allowed=100.0,
                clip_z=4.0,
            ),
        )
        self.policy = StalenessPolicy(
            max_feature_age_ms=2000,
            max_clock_skew_ms=100,
            require_availability_not_after_inference=True,
        )

    def test_valid_extraction_and_standardization_with_clipping(self) -> None:
        raw = extract_and_validate_raw_features(
            {"imbalance": 0.6, "spread_bps": 10.0}, self.specs
        )
        self.assertEqual(raw, [0.6, 10.0])
        std = standardize_features(raw, self.specs)
        # (0.6 - 0.1) / 0.5 = 1.0; (10.0 - 2.0) / 1.0 = 8.0 -> clipped to 4.0
        self.assertAlmostEqual(std[0], 1.0, places=12)
        self.assertAlmostEqual(std[1], 4.0, places=12)

    def test_missing_feature_rejected(self) -> None:
        with self.assertRaises(EngineError) as ctx:
            extract_and_validate_raw_features({"imbalance": 0.2}, self.specs)
        self.assertEqual(ctx.exception.code, ErrorCode.MISSING_FEATURE)

    def test_bool_and_string_feature_rejected(self) -> None:
        with self.assertRaises(EngineError) as ctx:
            extract_and_validate_raw_features(
                {"imbalance": True, "spread_bps": 2.0}, self.specs
            )
        self.assertEqual(ctx.exception.code, ErrorCode.UNEXPECTED_FEATURE_TYPE)

        with self.assertRaises(EngineError) as ctx2:
            extract_and_validate_raw_features(
                {"imbalance": "0.1", "spread_bps": 2.0}, self.specs  # type: ignore[dict-item]
            )
        self.assertEqual(ctx2.exception.code, ErrorCode.UNEXPECTED_FEATURE_TYPE)

    def test_nan_and_inf_rejected(self) -> None:
        for bad in (float("nan"), float("inf"), float("-inf")):
            with self.assertRaises(EngineError) as ctx:
                extract_and_validate_raw_features(
                    {"imbalance": bad, "spread_bps": 2.0}, self.specs
                )
            self.assertEqual(ctx.exception.code, ErrorCode.NON_FINITE_FEATURE)

    def test_out_of_bounds_rejected(self) -> None:
        with self.assertRaises(EngineError) as ctx:
            extract_and_validate_raw_features(
                {"imbalance": 1.5, "spread_bps": 2.0}, self.specs
            )
        self.assertEqual(ctx.exception.code, ErrorCode.FEATURE_OUT_OF_BOUNDS)

    def test_timestamp_validation_fresh_stale_and_future(self) -> None:
        now = datetime(2026, 1, 15, 15, 0, 1, 0, tzinfo=timezone.utc)
        checked, age_ms = validate_timestamps(
            "2026-01-15T15:00:00.500Z",
            "2026-01-15T15:00:00.510Z",
            self.policy,
            inference_dt=now,
        )
        self.assertEqual(checked, now)
        self.assertAlmostEqual(age_ms, 500.0, places=3)

        # Stale feature (> 2000ms)
        with self.assertRaises(EngineError) as ctx_stale:
            validate_timestamps(
                "2026-01-15T14:59:55.000Z",
                "2026-01-15T14:59:55.010Z",
                self.policy,
                inference_dt=now,
            )
        self.assertEqual(ctx_stale.exception.code, ErrorCode.STALE_FEATURE)

        # Future availability timestamp (look-ahead leakage)
        with self.assertRaises(EngineError) as ctx_future:
            validate_timestamps(
                "2026-01-15T15:00:00.900Z",
                "2026-01-15T15:00:02.000Z",
                self.policy,
                inference_dt=now,
            )
        self.assertEqual(ctx_future.exception.code, ErrorCode.FUTURE_TIMESTAMP)

        # Availability before event
        with self.assertRaises(EngineError) as ctx_inv:
            validate_timestamps(
                "2026-01-15T15:00:00.900Z",
                "2026-01-15T15:00:00.800Z",
                self.policy,
                inference_dt=now,
            )
        self.assertEqual(ctx_inv.exception.code, ErrorCode.INVALID_TIMESTAMP)

    def test_naive_timestamp_rejected(self) -> None:
        with self.assertRaises(EngineError) as ctx:
            parse_utc_timestamp("2026-01-15T15:00:00")
        self.assertEqual(ctx.exception.code, ErrorCode.INVALID_TIMESTAMP)

    def test_invalid_feature_spec_std(self) -> None:
        bad = FeatureSpec("x", "u", "float64", 0.0, 0.0, -1.0, 1.0)
        with self.assertRaises(EngineError) as ctx:
            bad.validate_definition()
        self.assertEqual(ctx.exception.code, ErrorCode.ARTIFACT_SCHEMA_ERROR)
        self.assertTrue(math.isfinite(bad.mean))


if __name__ == "__main__":
    unittest.main()
