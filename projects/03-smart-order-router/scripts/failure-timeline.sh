#!/bin/sh
# Print the failure-related events from a router event log (JSON lines):
# injected faults, child timeouts/rejects, and reconciliations.
# Usage: scripts/failure-timeline.sh demo-events.jsonl
set -eu
log="${1:?usage: $0 <events.jsonl>}"
echo
echo "--- failure timeline (SIMULATED) from $log ---"
t0="$(head -n 1 "$log" | sed -E 's/^\{"ts_ms":([0-9]+).*/\1/')"
echo "(times are simulated seconds since the first logged event)"
grep -E '"kind":"(fault|child_timeout|reconcile|kill_switch|anomaly)"|"child_status":"REJECTED"' "$log" |
	sed -E 's/^\{"ts_ms":([0-9]+),"kind":"([a-z_]+)","data":(.*)\}$/\1 \2 \3/' |
	awk -v t0="$t0" '{printf "+%7.1fs  %-14s %s\n", ($1-t0)/1000, $2, substr($0, index($0,$3))}' |
	cut -c1-150
