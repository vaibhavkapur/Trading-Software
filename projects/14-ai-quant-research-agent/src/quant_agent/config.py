"""Runtime configuration for the AI Quant Research Agent.

All execution is strictly paper/simulated by default. Secrets and tokens are
injected via environment variables and never embedded in logs or artifacts.
"""

from __future__ import annotations

import os
from dataclasses import dataclass, field
from pathlib import Path


@dataclass(frozen=True)
class Settings:
    """Immutable service configuration."""

    data_dir: Path = field(default_factory=lambda: Path(os.getenv("QUANT_AGENT_DATA_DIR", ".data")))
    mode: str = field(default_factory=lambda: os.getenv("QUANT_AGENT_MODE", "PAPER_SIMULATED"))
    code_revision: str = field(
        default_factory=lambda: os.getenv("QUANT_AGENT_CODE_REV", "0.1.0-local")
    )
    validator_version: str = "1.0.0"
    planner_version: str = "1.0.0"
    reporter_version: str = "1.0.0"
    max_workers: int = field(default_factory=lambda: int(os.getenv("QUANT_AGENT_MAX_WORKERS", "2")))
    researcher_token: str = field(
        default_factory=lambda: os.getenv("QUANT_AGENT_RESEARCHER_TOKEN", "dev-researcher-token")
    )
    reviewer_token: str = field(
        default_factory=lambda: os.getenv("QUANT_AGENT_REVIEWER_TOKEN", "dev-reviewer-token")
    )
    require_auth: bool = field(
        default_factory=lambda: os.getenv("QUANT_AGENT_REQUIRE_AUTH", "false").lower() == "true"
    )
    max_clock_skew_seconds: int = 300
    max_staleness_days: int = 7

    def __post_init__(self) -> None:
        if self.mode != "PAPER_SIMULATED":
            raise ValueError(
                f"Unsupported mode {self.mode!r}: only 'PAPER_SIMULATED' is permitted. "
                "Live capital connectivity is explicitly disabled."
            )

    @property
    def db_path(self) -> Path:
        return self.data_dir / "registry.db"

    @property
    def artifacts_dir(self) -> Path:
        return self.data_dir / "artifacts"
