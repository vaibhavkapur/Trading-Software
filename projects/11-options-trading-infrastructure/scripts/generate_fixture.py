"""Generate the deterministic replay fixture at examples/fixtures/xyz_chain_2027_01_15.json."""

from __future__ import annotations

from pathlib import Path

from options_infra.instruments.simulator import (
    DEFAULT_ASOF,
    generate_default_universe,
    save_fixture_file,
)


def main() -> None:
    root = Path(__file__).resolve().parent.parent
    out = root / "examples" / "fixtures" / "xyz_chain_2027_01_15.json"
    contracts, quotes = generate_default_universe(asof=DEFAULT_ASOF)
    save_fixture_file(
        out,
        contracts=contracts,
        quotes=quotes,
        metadata={
            "source": "SIMULATED_FEED",
            "asof": DEFAULT_ASOF.isoformat(),
            "description": "Deterministic replay fixture for XYZ (European) and ABC (American)",
        },
    )
    print(f"Wrote {len(contracts)} contracts and {len(quotes)} quotes to {out}")


if __name__ == "__main__":
    main()
