"""Runtime settings, read from the environment. There are no credentials: the service has
no outbound connections and reads only the files and payloads it is handed."""

from __future__ import annotations

import os
from collections.abc import Mapping
from dataclasses import dataclass

from tca.benchmark import Methodology


@dataclass(frozen=True)
class Settings:
    host: str
    port: int
    db_path: str
    methodology: Methodology


def load_settings(env: Mapping[str, str] | None = None) -> Settings:
    env = os.environ if env is None else env
    defaults = Methodology()
    return Settings(
        host=env.get("TCA_HOST", "127.0.0.1"),
        port=int(env.get("TCA_PORT", "8009")),
        db_path=env.get("TCA_DB_PATH", ":memory:"),
        methodology=Methodology(
            max_quote_age_ms=int(env.get("TCA_MAX_QUOTE_AGE_MS", defaults.max_quote_age_ms)),
            max_clock_skew_ms=int(env.get("TCA_MAX_CLOCK_SKEW_MS", defaults.max_clock_skew_ms)),
            quote_clock=env.get("TCA_QUOTE_CLOCK", defaults.quote_clock),
        ),
    )
