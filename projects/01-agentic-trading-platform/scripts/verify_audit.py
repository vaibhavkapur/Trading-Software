"""Verify the audit hash chain of a database file, offline.

Usage: PYTHONPATH=src python scripts/verify_audit.py data/atp.sqlite3
Exit status 0 when the chain is intact, 1 when an event was altered or removed.
"""

import sys

from atp.audit import AuditLog
from atp.clock import SystemClock
from atp.db import Database
from atp.ids import random_id


def main() -> int:
    if len(sys.argv) != 2:
        print(__doc__)
        return 2
    audit = AuditLog(Database(sys.argv[1]), SystemClock(), random_id)
    ok, first_bad_seq = audit.verify_chain()
    count = len(audit.all())
    if ok:
        print(f"OK: {count} events, chain intact")
        return 0
    print(f"TAMPERED: chain breaks at seq {first_bad_seq} ({count} events)")
    return 1


if __name__ == "__main__":
    sys.exit(main())
