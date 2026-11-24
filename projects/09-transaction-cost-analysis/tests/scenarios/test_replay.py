"""Replay: the committed fixture and the committed report are reproducible byte for byte.

If one of these fails after an intentional change, regenerate with ``make fixtures`` (the
simulated inputs) or ``make golden`` (the report), and review the diff before committing.
"""

from __future__ import annotations

import hashlib
import json
from collections import Counter

from builders import FIXTURE_DIR, GOLDEN_REPORT_DIR

from tca.__main__ import main
from tca.report import build_bundle
from tca.simulator import DEFAULT_SEED, render_all, simulate

# Every data-quality problem the simulator plants must surface in at least one order.
PLANTED_FLAGS = {
    "ARRIVAL_QUOTE_MISSING",  # GLOBX has no quotes for the first five minutes
    "ARRIVAL_QUOTE_STALE",  # NOVA quote outage 15:10-15:13
    "FILL_QUOTE_STALE",
    "ARRIVAL_CLOCK_SKEW",  # ALPHA's clock runs 1.8s ahead 15:30-15:35
    "FILL_CLOCK_SKEW",
    "VWAP_BARS_PARTIAL",  # ACME bars missing 15:20-15:24
    "VWAP_BARS_COARSE",
    "PARTIAL_FILL",
    "NO_FILLS",
}


def test_the_simulator_is_deterministic():
    assert render_all(simulate(DEFAULT_SEED)) == render_all(simulate(DEFAULT_SEED))
    assert render_all(simulate(DEFAULT_SEED + 1)) != render_all(simulate(DEFAULT_SEED))


def test_the_committed_fixture_is_exactly_what_the_simulator_produces():
    for name, content in render_all(simulate(DEFAULT_SEED)).items():
        assert (FIXTURE_DIR / name).read_text(encoding="utf-8") == content, name


def test_the_fixture_exercises_every_planted_problem_and_nothing_else(fixture_analysis):
    counts = Counter(flag for result in fixture_analysis.results for flag in result.flags)
    assert PLANTED_FLAGS <= set(counts)
    # The simulator never produces impossible order records; those flags would mean a
    # simulator bug, and they would silently shrink the trusted sample.
    assert not {"OVERFILLED", "FILL_BEFORE_ARRIVAL", "VWAP_WINDOW_INVALID"} & set(counts)
    assert not any(flag.endswith("QUOTE_CROSSED") for flag in counts)
    assert fixture_analysis.orphan_fills == 0
    filled = [result for result in fixture_analysis.results if result.fills]
    trusted = [result for result in filled if result.trusted]
    assert 0 < len(trusted) < len(filled)  # damage is present but does not dominate


def test_the_committed_report_is_reproduced_byte_for_byte(fixture_analysis):
    bundle = build_bundle(fixture_analysis)
    committed = {path.name for path in GOLDEN_REPORT_DIR.iterdir()}
    assert committed == set(bundle)
    for name, content in bundle.items():
        assert (GOLDEN_REPORT_DIR / name).read_text(encoding="utf-8") == content, name


def test_the_cli_reproduces_the_committed_report_and_its_hashes_hold(tmp_path):
    out = tmp_path / "report"
    assert main(["report", "--data", str(FIXTURE_DIR), "--out", str(out)]) == 0
    manifest = json.loads((GOLDEN_REPORT_DIR / "manifest.json").read_text())
    for name, info in manifest["files"].items():
        data = (out / name).read_bytes()
        assert data == (GOLDEN_REPORT_DIR / name).read_bytes(), name
        assert info["sha256"] == f"sha256:{hashlib.sha256(data).hexdigest()}", name
    assert (out / "manifest.json").read_bytes() == (
        GOLDEN_REPORT_DIR / "manifest.json"
    ).read_bytes()


def test_the_committed_report_describes_the_committed_fixture(fixture_analysis):
    manifest = json.loads((GOLDEN_REPORT_DIR / "manifest.json").read_text())
    assert manifest["dataset"]["version"] == fixture_analysis.dataset.version
    assert manifest["dataset"]["rows"] == fixture_analysis.dataset.counts
    assert manifest["methodology"]["id"] == fixture_analysis.methodology.id
