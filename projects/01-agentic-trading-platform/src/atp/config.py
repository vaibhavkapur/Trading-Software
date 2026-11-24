"""Runtime settings. Everything is injected through the environment; no secrets in code."""

from __future__ import annotations

import os
import secrets
from dataclasses import dataclass, field
from decimal import Decimal

DEFAULT_UNIVERSE: dict[str, str] = {
    # symbol -> simulated base price. Invented fixture values, not real market prices.
    "SPY": "125.00",
    "QQQ": "108.00",
    "AAPL": "47.50",
    "MSFT": "102.50",
    "NVDA": "30.00",
    "TLT": "23.00",
}

DEV_API_KEY = "dev-local-key"


@dataclass(frozen=True)
class Settings:
    env: str = "dev"
    db_path: str = "data/atp.sqlite3"
    # API key -> user id. Keys are bearer tokens; compare in constant time.
    api_keys: dict[str, str] = field(default_factory=lambda: {DEV_API_KEY: "alice"})
    approval_secret: bytes = field(default_factory=lambda: secrets.token_bytes(32))
    approval_ttl_seconds: int = 120

    # Simulator
    universe: dict[str, str] = field(default_factory=lambda: dict(DEFAULT_UNIVERSE))
    sim_seed: int = 7
    starting_cash: Decimal = Decimal("100000.00")
    sim_max_fill_per_match: int | None = None  # set to exercise partial fills

    # Risk policy thresholds (versioned together as `policy_version`)
    policy_version: str = "2026.11.0"
    max_quote_age_seconds: Decimal = Decimal("5")
    max_clock_skew_seconds: Decimal = Decimal("2")
    max_order_notional: Decimal = Decimal("10000")
    max_position_notional: Decimal = Decimal("25000")
    limit_price_band_pct: Decimal = Decimal("5")
    market_order_collar_bps: Decimal = Decimal("50")
    duplicate_window_seconds: int = 60
    market_session: str = "regular"  # "regular" (NYSE cash hours) or "always"

    # OMS
    max_submit_attempts: int = 3

    # Planner: "auto" (Claude when credentials exist, else rules), "rules", or "claude"
    planner: str = "auto"
    planner_model: str = "claude-opus-5-5"

    @classmethod
    def from_env(cls) -> Settings:
        env = os.environ.get("ATP_ENV", "dev")
        kwargs: dict = {"env": env}

        raw_keys = os.environ.get("ATP_API_KEYS")
        if raw_keys:
            keys = {}
            for pair in raw_keys.split(","):
                key, _, user = pair.strip().partition(":")
                if not key or not user:
                    raise ValueError("ATP_API_KEYS must look like 'key1:user1,key2:user2'")
                keys[key] = user
            kwargs["api_keys"] = keys
        elif env != "dev":
            raise ValueError("ATP_API_KEYS is required outside the dev environment")

        secret = os.environ.get("ATP_APPROVAL_SECRET")
        if secret:
            kwargs["approval_secret"] = secret.encode()
        elif env != "dev":
            raise ValueError("ATP_APPROVAL_SECRET is required outside the dev environment")

        simple = {
            "ATP_DB_PATH": ("db_path", str),
            "ATP_APPROVAL_TTL_SECONDS": ("approval_ttl_seconds", int),
            "ATP_SIM_SEED": ("sim_seed", int),
            "ATP_STARTING_CASH": ("starting_cash", Decimal),
            "ATP_SIM_MAX_FILL_PER_MATCH": ("sim_max_fill_per_match", int),
            "ATP_MAX_QUOTE_AGE_SECONDS": ("max_quote_age_seconds", Decimal),
            "ATP_MAX_ORDER_NOTIONAL": ("max_order_notional", Decimal),
            "ATP_MAX_POSITION_NOTIONAL": ("max_position_notional", Decimal),
            "ATP_LIMIT_PRICE_BAND_PCT": ("limit_price_band_pct", Decimal),
            "ATP_DUPLICATE_WINDOW_SECONDS": ("duplicate_window_seconds", int),
            "ATP_MARKET_SESSION": ("market_session", str),
            "ATP_MAX_SUBMIT_ATTEMPTS": ("max_submit_attempts", int),
            "ATP_PLANNER": ("planner", str),
            "ATP_PLANNER_MODEL": ("planner_model", str),
        }
        for var, (name, cast) in simple.items():
            if var in os.environ:
                kwargs[name] = cast(os.environ[var])

        settings = cls(**kwargs)
        if settings.market_session not in ("regular", "always"):
            raise ValueError("ATP_MARKET_SESSION must be 'regular' or 'always'")
        if settings.planner not in ("auto", "rules", "claude"):
            raise ValueError("ATP_PLANNER must be 'auto', 'rules' or 'claude'")
        return settings
