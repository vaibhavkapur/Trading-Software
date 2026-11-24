#!/usr/bin/env python3
"""Checks that the test suite notices deliberate bugs.

Each mutation below breaks one behaviour the README promises. The script
applies it, runs the tests, expects them to fail, and restores the file.
A mutation that survives means the tests do not cover that behaviour.

Usage: python3 scripts/mutation_check.py     (from the project root)
"""
import subprocess
import sys

MUTATIONS = [
    ("src/pipeline/reorder.go", "ts < r.wm }", "ts <= r.wm }",
     "an event exactly at the watermark is treated as late"),
    ("src/rules/detect.go", "int64(fw.Cancels)*100 < int64(r.MinCancelRatePct)*int64(fw.NewOrders)",
     "int64(fw.Cancels)*100 <= int64(r.MinCancelRatePct)*int64(fw.NewOrders)",
     "cancel-rate threshold is exclusive instead of inclusive"),
    ("src/rules/detect.go", "int64(fw.Messages) < int64(r.MinRatio)*trades",
     "int64(fw.Messages) <= int64(r.MinRatio)*trades",
     "order-to-trade threshold is exclusive instead of inclusive"),
    ("src/rules/layering.go", "cancelled*100 < int64(r.MinCancelPct)*resting",
     "cancelled*100 <= int64(r.MinCancelPct)*resting",
     "layering cancel threshold is exclusive instead of inclusive"),
    ("src/rules/layering.go", "len(layers) < r.MinLayerOrders || len(levels) < r.MinPriceLevels",
     "len(layers) < r.MinLayerOrders",
     "layering ignores the price-level requirement"),
    ("src/rules/wash.go", "len(list) < r.MinSelfTrades", "len(list) < r.MinSelfTrades-1",
     "one self-trade is enough for an alert"),
    ("src/rules/wash.go", "lo*100 < int64(r.MinOffsetPct)*hi", "false",
     "paired trades need not offset"),
    ("src/rules/book.go", "a.Status == domain.AlertOpen && gap <= b.cooldown", "gap <= b.cooldown",
     "detections are folded into alerts an analyst already picked up"),
    ("src/rules/book.go", "a.Status == domain.AlertOpen && gap <= b.cooldown", "a.Status == domain.AlertOpen",
     "suppression ignores the cooldown"),
    ("src/engine/engine.go", "if _, dup := e.offsets[ev.ID]; dup {", "if _, dup := e.offsets[ev.ID]; dup && false {",
     "redelivered events are processed again"),
    ("src/engine/engine.go", "\t\te.pendingOut = &rec\n", "",
     "an alert record that failed to write is dropped"),
    ("src/engine/engine.go", "\t\t\ttail = nil\n\t\t\treturn nil\n", "\t\t\treturn nil\n",
     "restart re-raises alerts that were already journalled"),
    ("src/engine/engine.go", "\tif ev.Late {\n\t\t\te.st.late++\n\t\t\treturn nil, nil\n\t\t}\n", "",
     "late events reach the detectors"),
    ("src/engine/engine.go", "ev.EventTime.After(now.Add(skew))", "false",
     "events from the future are accepted"),
    ("src/engine/engine.go", "\te.lastRecv = e.clock()\n\treturn e, nil", "\treturn e, nil",
     "a restart counts as idle time"),
    ("src/engine/engine.go", "if a.Status == domain.AlertInCase {\n\t\treturn domain.Case{}, fmt.Errorf(\"%w: alert %s is already under review in %v\", ErrConflict, alertID, a.CaseIDs)\n\t}\n\tnow := e.clock()\n\tc, err := e.cases.Create",
     "now := e.clock()\n\tc, err := e.cases.Create",
     "two open cases can be started on one alert"),
    ("src/cases/cases.go", "if rec.PrevHash != prevHash {", "if false {",
     "the audit chain is not checked link by link"),
    ("src/journal/journal.go", "\tif j.Fail != nil {\n\t\tif err := j.Fail(); err != nil {",
     "\tif j.Fail != nil {\n\t\tif err := j.Fail(); err != nil && false {",
     "journal write failures are swallowed"),
    ("src/api/api.go", "c, err := s.Engine.AddNote(who, r.PathValue(\"id\"), req.Text)",
     "c, err := s.Engine.AddNote(\"system\", r.PathValue(\"id\"), req.Text)",
     "notes are not attributed to the analyst who wrote them"),
    ("src/api/api.go", "\t\tif !same(bearer(r), want) {", "\t\tif false {",
     "ingest and admin routes accept any token"),
    ("src/normalize/normalize.go", "errors.New(\"ts must be RFC 3339 with an explicit offset, e.g. 2026-01-05T15:00:00.250Z\")\n\t}",
     "errors.New(\"ts must be RFC 3339 with an explicit offset, e.g. 2026-01-05T15:00:00.250Z\")\n\t}\n\tt = t.Add(time.Nanosecond)",
     "timestamps are shifted during normalization"),
]


def tests_pass():
    r = subprocess.run(["go", "test", "-count=1", "./tests/..."], capture_output=True, text=True)
    return r.returncode == 0, r.stdout + r.stderr


def main():
    ok, out = tests_pass()
    if not ok:
        print("the suite fails before any mutation; fix that first\n" + out)
        return 2
    survived = []
    for path, old, new, what in MUTATIONS:
        with open(path) as f:
            original = f.read()
        if original.count(old) != 1:
            print(f"SKIP     {what}: pattern found {original.count(old)} times in {path}")
            survived.append(what + " (pattern not applied)")
            continue
        try:
            with open(path, "w") as f:
                f.write(original.replace(old, new))
            ok, out = tests_pass()
        finally:
            with open(path, "w") as f:
                f.write(original)
        if ok:
            print(f"SURVIVED {what}")
            survived.append(what)
        else:
            failed = sorted({l.split()[2] for l in out.splitlines() if l.startswith("--- FAIL")})
            detail = ", ".join(failed[:3]) if failed else "build failure"
            print(f"killed   {what}  [{detail}{' ...' if len(failed) > 3 else ''}]")
    print(f"\n{len(MUTATIONS) - len(survived)} of {len(MUTATIONS)} mutations caught")
    return 1 if survived else 0


if __name__ == "__main__":
    sys.exit(main())
