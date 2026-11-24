#!/usr/bin/env bash
# Terminal walkthrough of the failure paths against a running `make demo`.
# Everything here is SIMULATED: it drives the built-in simulator's fault hooks.
set -euo pipefail
BASE="${LOB_URL:-http://127.0.0.1:8080}"
SYMBOL="${1:-XYZ}"

health() {
  curl -fsS "$BASE/v1/books/$SYMBOL/health" | python3 -c '
import json, sys
h = json.load(sys.stdin)
keys = ("state", "last_seq", "stale", "stale_ms", "gap_count", "recovery_count",
        "last_recovery_ms", "duplicate_count", "integrity", "crossed_events")
print("   " + "  ".join(f"{k}={h[k]}" for k in keys))'
}
fault() { curl -fsS -X POST "$BASE/v1/admin/faults" -H 'content-type: application/json' -d "$1" >/dev/null; }
step() { printf '\n== %s\n' "$1"; }

step "[simulated] baseline"; health
step "[simulated] drop 400 deltas -> sequence gap -> snapshot resync"
fault '{"fault":"drop","count":400}'; sleep 0.4; health; sleep 2; health
step "[simulated] duplicate delivery -> ignored"
fault '{"fault":"duplicate"}'; sleep 1; health
step "[simulated] venue publishes a crossed book -> flagged, then clears"
fault '{"fault":"cross"}'; sleep 0.1; health; sleep 2; health
step "[simulated] venue goes silent for 4s -> stale"
fault '{"fault":"stall","ms":4000}'; sleep 3; health; sleep 2.5; health
step "feed-level counters"
curl -fsS "$BASE/metrics" | grep -E '^lob_(sequence_gaps|recoveries|duplicates|crossed_events)_total|^lob_gap_recovery_seconds_(count|sum)'
