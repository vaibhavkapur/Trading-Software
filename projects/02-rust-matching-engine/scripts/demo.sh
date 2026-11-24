#!/usr/bin/env bash
# Terminal walkthrough of the matching engine. SIMULATED DATA ONLY.
set -euo pipefail
cd "$(dirname "$0")/.."

banner() { printf '\n\033[1m== %s ==\033[0m\n' "$*"; }
WORK="$(mktemp -d)"
trap 'rm -rf "$WORK"' EXIT

cargo build --locked -q
ME=target/debug/me

banner "SIMULATED / PAPER TRADING - no real venue, no real money"

banner "1. README example: BUY 100 @ 10025, SELL 40 @ 10025, CANCEL"
grep -v '^#' examples/session.jsonl | head -3 | tee "$WORK/readme.jsonl"
echo "--- events ---"
"$ME" run "$WORK/readme.jsonl"

banner "2. Full example session (priority, market sweep, replace, rejects, 2 symbols)"
"$ME" run examples/session.jsonl --emit-log "$WORK/session.commands.jsonl" > "$WORK/session.events.jsonl"
echo "$(grep -vc '^#' examples/session.jsonl) commands -> $(wc -l < "$WORK/session.events.jsonl" | tr -d ' ') events; trades:"
grep '"type":"TRADE"' "$WORK/session.events.jsonl"

banner "3. Deterministic replay: same sequenced log -> byte-identical events"
"$ME" replay "$WORK/session.commands.jsonl" > "$WORK/replay1.jsonl"
"$ME" replay "$WORK/session.commands.jsonl" > "$WORK/replay2.jsonl"
cmp "$WORK/session.events.jsonl" "$WORK/replay1.jsonl"
cmp "$WORK/replay1.jsonl" "$WORK/replay2.jsonl"
echo "live run == replay #1 == replay #2  ($(shasum -a 256 < "$WORK/replay1.jsonl" | cut -c1-16)...)"

banner "4. Seeded synthetic load: 20000 commands, live run vs. replay of its log"
"$ME" gen --seed 42 --count 20000 --symbols 4 > "$WORK/load.jsonl"
"$ME" run "$WORK/load.jsonl" --emit-log "$WORK/load.commands.jsonl" > "$WORK/load.events.jsonl"
"$ME" replay "$WORK/load.commands.jsonl" > "$WORK/load.replay.jsonl"
cmp "$WORK/load.events.jsonl" "$WORK/load.replay.jsonl"
echo "$(wc -l < "$WORK/load.events.jsonl" | tr -d ' ') events, $(grep -c '"type":"TRADE"' "$WORK/load.events.jsonl") trades, byte-identical on replay"

banner "Done (simulated). Next: 'make failure-demo', 'make bench'"
