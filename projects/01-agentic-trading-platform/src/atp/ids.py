"""ID generation. Random by default; a deterministic factory supports replay tests."""

from __future__ import annotations

import uuid
from collections import defaultdict
from collections.abc import Callable

IdFactory = Callable[[str], str]


def random_id(prefix: str) -> str:
    return f"{prefix}_{uuid.uuid4().hex}"


class SequentialIds:
    """Deterministic IDs (`ord_000001`, ...) so replays produce identical output."""

    def __init__(self) -> None:
        self._counters: dict[str, int] = defaultdict(int)

    def __call__(self, prefix: str) -> str:
        self._counters[prefix] += 1
        return f"{prefix}_{self._counters[prefix]:06d}"
