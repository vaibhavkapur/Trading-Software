#!/usr/bin/env python3
"""Measure sequential synthetic commands including both journal commits and TCP."""
import datetime
import json
import math
import os
from pathlib import Path
import platform
import sqlite3
import subprocess
import tempfile

ROOT = Path(__file__).resolve().parents[1]
SAMPLES = 100
RUNS = 5
WARMUP = 10


def output(*args):
    return subprocess.check_output(args, text=True).strip()


def cpu_model():
    if platform.system() == "Darwin":
        return output("sysctl", "-n", "machdep.cpu.brand_string")
    for line in Path("/proc/cpuinfo").read_text().splitlines():
        if line.startswith(("model name", "Hardware")):
            return line.partition(":")[2].strip()
    return platform.machine()


def percentile(values, fraction):
    return sorted(values)[math.ceil(len(values) * fraction) - 1]


def trial():
    with tempfile.TemporaryDirectory() as tmp:
        folder = Path(tmp)
        fixture = folder / "orders.txt"
        fixture.write_text("".join(f"new bench{i} SYNTH buy 1 10000\n" for i in range(WARMUP + SAMPLES)))
        lines = output(ROOT / "build/fix-gateway", folder / "state", "run", fixture).splitlines()
        events = [json.loads(line) for line in lines]
        result = events[-2]
        assert result["event"] == "reconciled" and result["fills"] == SAMPLES + WARMUP
        values = [r["elapsed_us"] for r in events if r["event"] == "command_processed"][WARMUP:]
        assert len(values) == SAMPLES
        return {"p50_us": percentile(values, .5), "p95_us": percentile(values, .95),
                "p99_us": percentile(values, .99), "samples_us": values}


if __name__ == "__main__":
    runs = [trial() for _ in range(RUNS)]
    values = [sample for run in runs for sample in run["samples_us"]]
    result = {"schema": 1, "utc": datetime.datetime.now(datetime.timezone.utc).isoformat(),
              "platform": platform.platform(), "cpu": cpu_model(), "logical_cpus": os.cpu_count(),
              "machine": platform.machine(), "quickfix": "1.16.0 + nonstop-startup.patch",
              "compiler": output("c++", "--version").splitlines()[0], "build": "CMake Release",
              "sqlite_python_runtime": sqlite3.sqlite_version,
              "workload": "One outstanding synthetic marketable limit buy, one share at USD 100.00. "
                          "Each new order produces an acknowledgment and a trade. The timer covers OMS staging, "
                          "both FIX TCP directions, simulator and OMS durable journal writes, and outbox publication. "
                          "Startup, command parsing, JSON output and final reconciliation are outside the timer.",
              "runs": RUNS, "warmup_per_run": WARMUP, "measured_per_run": SAMPLES,
              "percentile_method": "nearest rank", "p50_us": percentile(values, .5),
              "p95_us": percentile(values, .95), "p99_us": percentile(values, .99),
              "run_p95_range_us": [min(r["p95_us"] for r in runs), max(r["p95_us"] for r in runs)],
              "trials": runs}
    print(json.dumps(result, indent=2))
