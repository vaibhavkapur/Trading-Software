"""Exercise process boundaries and the public OMS/result contract."""
import json
from pathlib import Path
import sqlite3
import subprocess
import sys
import tempfile

binary, fixture = sys.argv[1:]
with tempfile.TemporaryDirectory() as tmp:
    state = Path(tmp) / "state"

    def run(*args, success=True):
        p = subprocess.run([binary, state, *args], capture_output=True, text=True, timeout=15)
        if success:
            assert p.returncode == 0, p.stderr
            return [json.loads(line) for line in p.stdout.splitlines()]
        assert p.returncode == 1 and json.loads(p.stderr)["event"] == "halted", p

    result = run("run", fixture)
    first = result[-2]
    assert first["event"] == "reconciled" and first["pending"] == 0
    assert first["reports"] == 6 and first["fills"] == 1 and first["filled_shares"] == 7
    assert first["oms_next_send"] == first["venue_next_receive"]
    assert first["venue_next_send"] == first["oms_next_receive"]
    assert result[-1]["event"] == "logout" and not result[-1]["logged_on"]
    events = [r for r in result if r["event"] == "order_result"]
    assert len(events) == 6 and len({r["event_id"] for r in events}) == 6
    assert events[-1]["reason"] == "unknown original order"
    replay = run("run", fixture)
    assert replay[-2]["reports"] == 6 and replay[-2]["oms_next_send"] > first["oms_next_send"]
    assert [r for r in replay if r["event"] == "order_result"] == events
    run("new", "cross", "SYNTH", "sell", "8", "10000", success=False)
    run("new", "bad", "SYNTH", "buy", "0", "10000", success=False)
    assert run("status")[-2]["reports"] == 6
    for db in ["oms.sqlite", "venue.sqlite"]:
        with sqlite3.connect(state / db) as conn:
            assert conn.execute("PRAGMA integrity_check").fetchone() == ("ok",)
            assert conn.execute("SELECT count(*) FROM requests").fetchone() == (5,)
print("PASS CLI new/replace/cancel, explicit reject, durable results, rerun, invalid input and reconciliation")
