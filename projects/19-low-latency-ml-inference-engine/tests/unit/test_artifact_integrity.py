"""Unit tests for safe artifact bundling, SHA-256 verification, and untrusted format rejection."""

from __future__ import annotations

import json
import shutil
import tempfile
import unittest
from pathlib import Path

from inference_engine.artifact import load_verified_artifact
from inference_engine.contracts import EngineError, ErrorCode


PROJECT_ROOT = Path(__file__).resolve().parents[2]
V1_BUNDLE = PROJECT_ROOT / "data" / "fixtures" / "models" / "lob_alpha_mlp" / "v1.0.0"


class TestArtifactIntegrity(unittest.TestCase):
    def test_load_verified_v1_bundle(self) -> None:
        art = load_verified_artifact(V1_BUNDLE, verify_sha256=True)
        self.assertEqual(art.manifest.model_id, "lob_alpha_mlp")
        self.assertEqual(art.manifest.model_version, "1.0.0")
        self.assertEqual(len(art.manifest.features), 8)
        self.assertGreaterEqual(len(art.golden_vectors), 10)
        self.assertEqual(len(art.bundle_sha256), 64)

    def test_reject_untrusted_pickle_file_directly(self) -> None:
        with tempfile.TemporaryDirectory() as td:
            pkl = Path(td) / "evil_model.pkl"
            pkl.write_bytes(b"not-a-safe-artifact")
            with self.assertRaises(EngineError) as ctx:
                load_verified_artifact(pkl)
            self.assertEqual(ctx.exception.code, ErrorCode.UNTRUSTED_ARTIFACT_FORMAT)

    def test_reject_bundle_containing_pickle_file(self) -> None:
        with tempfile.TemporaryDirectory() as td:
            copy_dir = Path(td) / "v1_with_pkl"
            shutil.copytree(V1_BUNDLE, copy_dir)
            (copy_dir / "sidecar.joblib").write_bytes(b"untrusted")
            with self.assertRaises(EngineError) as ctx:
                load_verified_artifact(copy_dir)
            self.assertEqual(ctx.exception.code, ErrorCode.UNTRUSTED_ARTIFACT_FORMAT)

    def test_reject_tampered_weights_sha256(self) -> None:
        with tempfile.TemporaryDirectory() as td:
            copy_dir = Path(td) / "v1_tampered"
            shutil.copytree(V1_BUNDLE, copy_dir)
            wpath = copy_dir / "weights.bin"
            data = bytearray(wpath.read_bytes())
            data[0] ^= 0xFF
            wpath.write_bytes(bytes(data))
            with self.assertRaises(EngineError) as ctx:
                load_verified_artifact(copy_dir, verify_sha256=True)
            self.assertEqual(ctx.exception.code, ErrorCode.ARTIFACT_INTEGRITY_ERROR)

    def test_reject_disallowed_serialization_format_in_manifest(self) -> None:
        with tempfile.TemporaryDirectory() as td:
            copy_dir = Path(td) / "v1_bad_fmt"
            shutil.copytree(V1_BUNDLE, copy_dir)
            mpath = copy_dir / "manifest.json"
            mdata = json.loads(mpath.read_text(encoding="utf-8"))
            mdata["serialization_format"] = "python_pickle_v4"
            mpath.write_text(json.dumps(mdata), encoding="utf-8")
            with self.assertRaises(EngineError) as ctx:
                load_verified_artifact(copy_dir)
            self.assertEqual(ctx.exception.code, ErrorCode.UNTRUSTED_ARTIFACT_FORMAT)

    def test_reject_path_traversal_in_manifest(self) -> None:
        with tempfile.TemporaryDirectory() as td:
            copy_dir = Path(td) / "v1_traversal"
            shutil.copytree(V1_BUNDLE, copy_dir)
            mpath = copy_dir / "manifest.json"
            mdata = json.loads(mpath.read_text(encoding="utf-8"))
            mdata["weights_file"] = "../outside.bin"
            mpath.write_text(json.dumps(mdata), encoding="utf-8")
            with self.assertRaises(EngineError) as ctx:
                load_verified_artifact(copy_dir)
            self.assertEqual(ctx.exception.code, ErrorCode.ARTIFACT_INTEGRITY_ERROR)


if __name__ == "__main__":
    unittest.main()
