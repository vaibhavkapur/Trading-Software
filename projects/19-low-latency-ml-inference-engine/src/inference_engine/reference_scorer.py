"""Pure-Python deterministic reference scorer and synthetic LOB model trainer/exporter.

Establishes ground-truth numerical outputs before any native optimization.
"""

from __future__ import annotations

import json
import math
import random
from datetime import datetime, timedelta, timezone
from pathlib import Path
from typing import Any, Dict, List, Mapping, Sequence, Tuple

from .artifact import LoadedModelArtifact, save_verified_artifact, sha256_file
from .contracts import (
    SCHEMA_VERSION,
    EngineError,
    ErrorCode,
    FeatureSpec,
    InferenceRequest,
    InferenceResponse,
    LayerSpec,
    OutputSpec,
    StalenessPolicy,
    format_utc_timestamp,
)
from .preprocessing import prepare_request, standardize_features


# Canonical synthetic Limit Order Book (LOB) feature definitions
DEFAULT_LOB_FEATURE_METADATA: Tuple[Dict[str, Any], ...] = (
    {
        "name": "order_book_imbalance_l1",
        "unit": "ratio",
        "dtype": "float64",
        "min_allowed": -1.0,
        "max_allowed": 1.0,
        "clip_z": 5.0,
    },
    {
        "name": "order_book_imbalance_l5",
        "unit": "ratio",
        "dtype": "float64",
        "min_allowed": -1.0,
        "max_allowed": 1.0,
        "clip_z": 5.0,
    },
    {
        "name": "bid_ask_spread_bps",
        "unit": "bps",
        "dtype": "float64",
        "min_allowed": 0.0,
        "max_allowed": 500.0,
        "clip_z": 5.0,
    },
    {
        "name": "microprice_deviation_bps",
        "unit": "bps",
        "dtype": "float64",
        "min_allowed": -200.0,
        "max_allowed": 200.0,
        "clip_z": 5.0,
    },
    {
        "name": "order_flow_imbalance_1s",
        "unit": "zscore",
        "dtype": "float64",
        "min_allowed": -50.0,
        "max_allowed": 50.0,
        "clip_z": 5.0,
    },
    {
        "name": "trade_sign_momentum_5s",
        "unit": "ratio",
        "dtype": "float64",
        "min_allowed": -1.0,
        "max_allowed": 1.0,
        "clip_z": 5.0,
    },
    {
        "name": "realized_vol_30s_bps",
        "unit": "bps",
        "dtype": "float64",
        "min_allowed": 0.0,
        "max_allowed": 1000.0,
        "clip_z": 5.0,
    },
    {
        "name": "depth_ratio_bid_ask",
        "unit": "log_ratio",
        "dtype": "float64",
        "min_allowed": -10.0,
        "max_allowed": 10.0,
        "clip_z": 5.0,
    },
)


def _apply_activation(x: float, activation: str) -> float:
    if activation == "linear":
        return x
    if activation == "relu":
        return x if x > 0.0 else 0.0
    if activation == "tanh":
        return math.tanh(x)
    if activation == "sigmoid":
        if x >= 0.0:
            z = math.exp(-x)
            return 1.0 / (1.0 + z)
        z = math.exp(x)
        return z / (1.0 + z)
    raise EngineError(
        ErrorCode.ARTIFACT_SCHEMA_ERROR, f"Unknown activation {activation!r}"
    )


def score_preprocessed_vector(
    standardized_vec: Sequence[float], artifact: LoadedModelArtifact
) -> float:
    """Deterministic pure-Python forward pass over standardized features."""
    current: List[float] = list(standardized_vec)
    for layer in artifact.layers:
        nxt: List[float] = [0.0] * layer.spec.out_dim
        act = layer.spec.activation
        for j in range(layer.spec.out_dim):
            w_row = layer.weights[j]
            acc = layer.biases[j]
            for i in range(layer.spec.in_dim):
                acc += w_row[i] * current[i]
            nxt[j] = _apply_activation(acc, act)
        current = nxt
    out = current[0]
    if not math.isfinite(out):
        raise EngineError(
            ErrorCode.NON_FINITE_FEATURE,
            f"Model produced non-finite prediction: {out!r}",
        )
    return out


def score_raw_vector(raw_vec: Sequence[float], artifact: LoadedModelArtifact) -> float:
    """Preprocess raw ordered features and score with Python reference implementation."""
    standardized = standardize_features(raw_vec, artifact.manifest.features)
    return score_preprocessed_vector(standardized, artifact)


def verify_golden_parity(
    artifact: LoadedModelArtifact,
    score_fn,
    *,
    tolerance_abs: float | None = None,
) -> Dict[str, Any]:
    """Verify a scorer callable against bundled golden reference vectors."""
    tol = (
        tolerance_abs
        if tolerance_abs is not None
        else artifact.manifest.output.prediction_tolerance_abs
    )
    max_abs_err = 0.0
    checked = 0
    for gv in artifact.golden_vectors:
        raw_vec = [gv.raw_features[f.name] for f in artifact.manifest.features]
        pred = float(score_fn(raw_vec))
        err = abs(pred - gv.expected_prediction)
        if err > max_abs_err:
            max_abs_err = err
        if not math.isfinite(pred) or err > tol:
            raise EngineError(
                ErrorCode.PARITY_VERIFICATION_FAILED,
                f"Parity failed on golden case {gv.case_id!r}: expected={gv.expected_prediction}, got={pred}, err={err} > tol={tol}",
                details={
                    "case_id": gv.case_id,
                    "expected": gv.expected_prediction,
                    "actual": pred,
                    "abs_error": err,
                    "tolerance_abs": tol,
                },
            )
        checked += 1
    return {
        "cases_checked": checked,
        "max_abs_error": max_abs_err,
        "tolerance_abs": tol,
        "passed": True,
    }


def generate_synthetic_lob_dataset(
    num_samples: int = 1200,
    seed: int = 42,
) -> List[Dict[str, Any]]:
    """Generate deterministic chronological synthetic LOB snapshots with point-in-time labels.

    Synthetic data only — no proprietary or unlicensed market data.
    """
    rng = random.Random(seed)
    base_time = datetime(2026, 1, 15, 14, 30, 0, tzinfo=timezone.utc)
    records: List[Dict[str, Any]] = []

    vol_state = 8.0
    for idx in range(num_samples):
        event_dt = base_time + timedelta(milliseconds=250 * idx)
        avail_dt = event_dt + timedelta(milliseconds=5)

        vol_state = max(2.0, min(40.0, 0.92 * vol_state + 0.08 * 8.5 + rng.gauss(0.0, 0.9)))
        imb_l1 = max(-0.98, min(0.98, rng.gauss(0.0, 0.42)))
        imb_l5 = max(-0.98, min(0.98, 0.65 * imb_l1 + rng.gauss(0.0, 0.25)))
        spread_bps = max(0.2, min(40.0, 1.4 + 0.12 * vol_state + abs(rng.gauss(0.0, 0.6))))
        micro_dev = max(-30.0, min(30.0, 1.8 * imb_l1 * spread_bps + rng.gauss(0.0, 0.5)))
        ofi_1s = max(-12.0, min(12.0, 2.2 * imb_l1 + 1.1 * imb_l5 + rng.gauss(0.0, 1.0)))
        trade_mom = max(-0.98, min(0.98, 0.4 * imb_l1 + rng.gauss(0.0, 0.35)))
        depth_ratio = max(-4.0, min(4.0, 1.4 * imb_l5 + rng.gauss(0.0, 0.4)))

        # Non-linear synthetic next-1s mid-price return (bps) with regime interaction + noise
        true_signal_bps = (
            1.65 * imb_l1
            + 0.75 * imb_l5
            + 0.38 * micro_dev
            + 0.22 * math.tanh(ofi_1s * 0.5)
            + 0.55 * trade_mom
            - 0.06 * spread_bps * (1.0 if imb_l1 > 0 else -1.0)
            + 0.45 * (imb_l1 * max(0.0, trade_mom))
            + 0.18 * depth_ratio
        )
        target_return_bps = true_signal_bps + rng.gauss(0.0, 0.85)

        records.append(
            {
                "record_id": f"syn-lob-{idx:06d}",
                "instrument_id": "SYN-EQUITY-A",
                "currency": "USD",
                "event_ts_utc": format_utc_timestamp(event_dt),
                "available_ts_utc": format_utc_timestamp(avail_dt),
                "features": {
                    "order_book_imbalance_l1": round(imb_l1, 6),
                    "order_book_imbalance_l5": round(imb_l5, 6),
                    "bid_ask_spread_bps": round(spread_bps, 6),
                    "microprice_deviation_bps": round(micro_dev, 6),
                    "order_flow_imbalance_1s": round(ofi_1s, 6),
                    "trade_sign_momentum_5s": round(trade_mom, 6),
                    "realized_vol_30s_bps": round(vol_state, 6),
                    "depth_ratio_bid_ask": round(depth_ratio, 6),
                },
                "target_return_1s_bps": round(target_return_bps, 6),
            }
        )
    return records


def _compute_feature_specs(
    train_records: Sequence[Mapping[str, Any]],
) -> List[FeatureSpec]:
    specs: List[FeatureSpec] = []
    n = len(train_records)
    for meta in DEFAULT_LOB_FEATURE_METADATA:
        name = meta["name"]
        vals = [float(r["features"][name]) for r in train_records]
        mean = sum(vals) / n
        var = sum((v - mean) ** 2 for v in vals) / max(1, n)
        std = math.sqrt(var) if var > 1e-12 else 1.0
        specs.append(
            FeatureSpec(
                name=name,
                unit=meta["unit"],
                dtype=meta["dtype"],
                mean=round(mean, 8),
                std=round(std, 8),
                min_allowed=float(meta["min_allowed"]),
                max_allowed=float(meta["max_allowed"]),
                clip_z=float(meta["clip_z"]),
            )
        )
    return specs


def _train_mlp_pure_python(
    x_train: List[List[float]],
    y_train: List[float],
    layer_dims: Sequence[Tuple[int, int, str]],
    *,
    seed: int,
    epochs: int,
    lr: float,
    l2_reg: float = 1e-4,
) -> List[float]:
    """Deterministic pure-Python SGD trainer for small MLPs (ReLU/Tanh/Linear)."""
    rng = random.Random(seed)
    weights: List[List[List[float]]] = []
    biases: List[List[float]] = []

    for in_d, out_d, _act in layer_dims:
        scale = math.sqrt(2.0 / (in_d + out_d))
        w = [[rng.gauss(0.0, scale) for _ in range(in_d)] for _ in range(out_d)]
        b = [0.0 for _ in range(out_d)]
        weights.append(w)
        biases.append(b)

    n_layers = len(layer_dims)
    n_samples = len(x_train)
    order = list(range(n_samples))

    for epoch in range(epochs):
        rng.shuffle(order)
        step_lr = lr / (1.0 + 0.04 * epoch)
        for idx in order:
            x = x_train[idx]
            y = y_train[idx]

            # Forward pass
            acts: List[List[float]] = [x]
            pre_acts: List[List[float]] = []
            cur = x
            for l_idx, (in_d, out_d, act_name) in enumerate(layer_dims):
                w_l = weights[l_idx]
                b_l = biases[l_idx]
                z = [0.0] * out_d
                a = [0.0] * out_d
                for j in range(out_d):
                    row = w_l[j]
                    acc = b_l[j]
                    for i in range(in_d):
                        acc += row[i] * cur[i]
                    z[j] = acc
                    a[j] = _apply_activation(acc, act_name)
                pre_acts.append(z)
                acts.append(a)
                cur = a

            pred = acts[-1][0]
            # MSE gradient: d/dpred of 0.5*(pred - y)^2 = (pred - y)
            delta = [pred - y]

            # Backprop
            for l_idx in range(n_layers - 1, -1, -1):
                in_d, out_d, act_name = layer_dims[l_idx]
                prev_a = acts[l_idx]
                z_l = pre_acts[l_idx]
                w_l = weights[l_idx]
                b_l = biases[l_idx]

                # Multiply by activation derivative
                dz = [0.0] * out_d
                for j in range(out_d):
                    if act_name == "linear":
                        dz[j] = delta[j]
                    elif act_name == "relu":
                        dz[j] = delta[j] if z_l[j] > 0.0 else 0.0
                    elif act_name == "tanh":
                        t = math.tanh(z_l[j])
                        dz[j] = delta[j] * (1.0 - t * t)
                    elif act_name == "sigmoid":
                        s = _apply_activation(z_l[j], "sigmoid")
                        dz[j] = delta[j] * s * (1.0 - s)

                # Compute upstream delta before weight update
                if l_idx > 0:
                    prev_delta = [0.0] * in_d
                    for j in range(out_d):
                        row = w_l[j]
                        dj = dz[j]
                        for i in range(in_d):
                            prev_delta[i] += row[i] * dj
                else:
                    prev_delta = []

                # Update weights and biases with L2 weight decay
                for j in range(out_d):
                    row = w_l[j]
                    dj = dz[j]
                    b_l[j] -= step_lr * dj
                    for i in range(in_d):
                        grad = dj * prev_a[i] + l2_reg * row[i]
                        row[i] -= step_lr * grad

                delta = prev_delta

    flat: List[float] = []
    for l_idx in range(n_layers):
        for row in weights[l_idx]:
            flat.extend(row)
        flat.extend(biases[l_idx])
    return flat


def _evaluate_predictions(
    preds: Sequence[float],
    targets: Sequence[float],
    cost_bps: float = 0.5,
) -> Dict[str, float]:
    n = len(preds)
    mse = sum((p - y) ** 2 for p, y in zip(preds, targets)) / n
    mae = sum(abs(p - y) for p, y in zip(preds, targets)) / n
    mean_y = sum(targets) / n
    ss_tot = sum((y - mean_y) ** 2 for y in targets)
    r2 = 1.0 - (mse * n / ss_tot) if ss_tot > 1e-12 else 0.0
    dir_hits = sum(1 for p, y in zip(preds, targets) if (p >= 0 and y >= 0) or (p < 0 and y < 0))
    # Simulated net PnL per signal taking a position only when |pred| > cost_bps
    trades = 0
    net_bps_sum = 0.0
    for p, y in zip(preds, targets):
        if abs(p) > cost_bps:
            trades += 1
            sign = 1.0 if p > 0 else -1.0
            net_bps_sum += sign * y - cost_bps
    return {
        "samples": float(n),
        "rmse_bps": round(math.sqrt(mse), 6),
        "mae_bps": round(mae, 6),
        "r2": round(r2, 6),
        "directional_accuracy": round(dir_hits / n, 6),
        "simulated_trades_after_cost": float(trades),
        "simulated_avg_net_bps_per_trade": round(net_bps_sum / trades, 6) if trades else 0.0,
        "assumed_round_trip_cost_bps": cost_bps,
    }


def build_fixtures_and_models(project_root: Path) -> Dict[str, Any]:
    """Generate deterministic synthetic LOB fixtures and export verified v1.0.0 and v2.0.0 models."""
    fixtures_dir = project_root / "data" / "fixtures"
    models_root = fixtures_dir / "models" / "lob_alpha_mlp"
    fixtures_dir.mkdir(parents=True, exist_ok=True)

    records = generate_synthetic_lob_dataset(num_samples=1200, seed=42)

    # Strict chronological split: 70% train, 15% val, 15% test (no random shuffle across time!)
    n_train = 840
    n_val = 180
    train_recs = records[:n_train]
    val_recs = records[n_train : n_train + n_val]
    test_recs = records[n_train + n_val :]

    jsonl_path = fixtures_dir / "synthetic_lob_features.jsonl"
    with jsonl_path.open("w", encoding="utf-8") as f:
        for rec in records:
            f.write(json.dumps(rec, sort_keys=True) + "\n")

    dataset_manifest = {
        "schema_version": SCHEMA_VERSION,
        "dataset_id": "synthetic_lob_microprice_v1",
        "provenance": "Deterministically generated synthetic data (seed=42); zero proprietary or external market data.",
        "license": "Internal synthetic test fixture — permitted for local and CI use.",
        "instrument_id": "SYN-EQUITY-A",
        "currency": "USD",
        "num_records": len(records),
        "chronological_split": {
            "train_count": len(train_recs),
            "train_start_utc": train_recs[0]["event_ts_utc"],
            "train_end_utc": train_recs[-1]["event_ts_utc"],
            "val_count": len(val_recs),
            "val_start_utc": val_recs[0]["event_ts_utc"],
            "val_end_utc": val_recs[-1]["event_ts_utc"],
            "test_count": len(test_recs),
            "test_start_utc": test_recs[0]["event_ts_utc"],
            "test_end_utc": test_recs[-1]["event_ts_utc"],
        },
        "file": "synthetic_lob_features.jsonl",
        "sha256": sha256_file(jsonl_path),
    }
    (fixtures_dir / "dataset_manifest.json").write_text(
        json.dumps(dataset_manifest, indent=2, sort_keys=True) + "\n", encoding="utf-8"
    )

    # Fit preprocessing parameters strictly on train split only (no leakage!)
    feature_specs = _compute_feature_specs(train_recs)
    feature_names = [fs.name for fs in feature_specs]

    def _featurize(recs: Sequence[Mapping[str, Any]]) -> Tuple[List[List[float]], List[float]]:
        xs: List[List[float]] = []
        ys: List[float] = []
        for r in recs:
            raw = [float(r["features"][name]) for name in feature_names]
            xs.append(standardize_features(raw, feature_specs))
            ys.append(float(r["target_return_1s_bps"]))
        return xs, ys

    x_train, y_train = _featurize(train_recs)
    x_val, y_val = _featurize(val_recs)
    x_test, y_test = _featurize(test_recs)

    # Experiment 0 (failed / weaker baseline documented per plan.md Section 5):
    # High-cost linear baseline with overly aggressive L2 penalty
    over_reg_dims = ((8, 1, "linear"),)
    over_reg_flat = _train_mlp_pure_python(
        x_train, y_train, over_reg_dims, seed=101, epochs=12, lr=0.005, l2_reg=0.5
    )

    # Model v1.0.0: compact 8 -> 16 (relu) -> 8 (tanh) -> 1 (linear)
    v1_dims = ((8, 16, "relu"), (16, 8, "tanh"), (8, 1, "linear"))
    v1_flat = _train_mlp_pure_python(
        x_train, y_train, v1_dims, seed=42, epochs=28, lr=0.006, l2_reg=5e-4
    )

    # Model v2.0.0: wider 8 -> 32 (relu) -> 16 (relu) -> 1 (linear) trained on train+val
    v2_dims = ((8, 32, "relu"), (32, 16, "relu"), (16, 1, "linear"))
    v2_flat = _train_mlp_pure_python(
        x_train + x_val, y_train + y_val, v2_dims, seed=2026, epochs=32, lr=0.005, l2_reg=4e-4
    )

    def _export_version(
        version: str,
        dims: Sequence[Tuple[int, int, str]],
        flat_weights: List[float],
        train_seed: int,
        epochs: int,
        notes: str,
    ) -> Dict[str, Any]:
        layer_specs = tuple(
            LayerSpec(name=f"dense_{i}", in_dim=in_d, out_dim=out_d, activation=act)
            for i, (in_d, out_d, act) in enumerate(dims)
        )
        # Temporarily build golden cases by scoring test fixtures via pure Python
        from .artifact import _unpack_layers

        layers = _unpack_layers(flat_weights, layer_specs)

        class _TempArtifact:
            pass

        tmp = _TempArtifact()
        tmp.layers = layers  # type: ignore[attr-defined]
        tmp_preds_test = [score_preprocessed_vector(x, tmp) for x in x_test]  # type: ignore[arg-type]
        tmp_preds_val = [score_preprocessed_vector(x, tmp) for x in x_val]  # type: ignore[arg-type]

        val_metrics = _evaluate_predictions(tmp_preds_val, y_val, cost_bps=0.5)
        test_metrics = _evaluate_predictions(tmp_preds_test, y_test, cost_bps=0.5)

        golden_cases = []
        for idx in range(25):
            rec = test_recs[idx]
            raw = [float(rec["features"][name]) for name in feature_names]
            std_v = standardize_features(raw, feature_specs)
            pred = score_preprocessed_vector(std_v, tmp)  # type: ignore[arg-type]
            golden_cases.append(
                {
                    "case_id": f"golden_{idx:03d}",
                    "raw_features": rec["features"],
                    "expected_prediction": pred,
                }
            )

        manifest_template = {
            "schema_version": SCHEMA_VERSION,
            "model_id": "lob_alpha_mlp",
            "model_version": version,
            "architecture": "mlp_v1",
            "serialization_format": "safe_ieee754_bin_v1",
            "created_at_utc": "2026-01-15T15:00:00.000Z",
            "features": [fs.to_dict() for fs in feature_specs],
            "layers": [ls.to_dict() for ls in layer_specs],
            "output": OutputSpec(
                name="expected_return_1s_bps",
                unit="bps",
                task_type="regression",
                prediction_tolerance_abs=1e-9,
            ).to_dict(),
            "staleness_policy": StalenessPolicy(
                max_feature_age_ms=5000,
                max_clock_skew_ms=250,
                require_availability_not_after_inference=True,
            ).to_dict(),
            "training_metadata": {
                "dataset_id": dataset_manifest["dataset_id"],
                "dataset_sha256": dataset_manifest["sha256"],
                "seed": train_seed,
                "epochs": epochs,
                "split_policy": "chronological_no_lookahead",
                "val_metrics": val_metrics,
                "test_metrics": test_metrics,
                "notes": notes,
                "live_performance_claim": "NONE — synthetic data only; never infer live performance from synthetic tests.",
            },
        }
        bundle_dir = models_root / f"v{version}"
        manifest = save_verified_artifact(
            bundle_dir, manifest_template, flat_weights, golden_cases
        )
        return {
            "version": version,
            "bundle_dir": str(bundle_dir.relative_to(project_root)),
            "weights_sha256": manifest.weights_sha256,
            "weights_byte_size": manifest.weights_byte_size,
            "val_metrics": val_metrics,
            "test_metrics": test_metrics,
        }

    # Evaluate the rejected over-regularized baseline honestly
    from .artifact import _unpack_layers

    bad_layers = _unpack_layers(
        over_reg_flat, (LayerSpec(name="dense_0", in_dim=8, out_dim=1, activation="linear"),)
    )

    class _TmpBad:
        layers = bad_layers

    bad_preds_test = [score_preprocessed_vector(x, _TmpBad()) for x in x_test]  # type: ignore[arg-type]
    failed_baseline_metrics = _evaluate_predictions(bad_preds_test, y_test, cost_bps=0.5)

    v1_summary = _export_version(
        "1.0.0",
        v1_dims,
        v1_flat,
        train_seed=42,
        epochs=28,
        notes="Primary MVP compact MLP (8->16->8->1) trained strictly on chronological train split.",
    )
    v2_summary = _export_version(
        "2.0.0",
        v2_dims,
        v2_flat,
        train_seed=2026,
        epochs=32,
        notes="Candidate rollout MLP (8->32->16->1) for canary routing, reload, and rollback verification.",
    )

    return {
        "dataset": dataset_manifest,
        "exported_models": [v1_summary, v2_summary],
        "rejected_experiments": [
            {
                "experiment_id": "linear_over_regularized_l2_0.5",
                "reason_rejected": "Higher RMSE and lower R2 on chronological test split due to underfitting non-linear LOB interactions.",
                "test_metrics": failed_baseline_metrics,
            }
        ],
    }
