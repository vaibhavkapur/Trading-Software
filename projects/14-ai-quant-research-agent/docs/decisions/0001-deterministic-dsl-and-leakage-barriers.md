# ADR 0001: Constrained Signal DSL Over Arbitrary Code Execution

- **Status:** Accepted
- **Date:** 2026-11-24

## Context

An AI-assisted quant research agent needs to turn natural-language hypotheses into executable backtests. A naive design lets an LLM emit arbitrary Python/Pandas code and runs it inside a worker container. In quantitative research, arbitrary generated code introduces three severe failure modes:

1. **Subtle lookahead leakage:** A single `.shift(-1)`, full-sample normalization (`(x - x.mean()) / x.std()`), or post-hoc threshold tweak silently contaminates out-of-sample results while appearing plausible in code review.
2. **Non-determinism and unreproducible research:** Implicit library defaults, unseeded RNGs, or hidden external reads break bit-for-bit replay verification.
3. **Execution sandbox risk:** Arbitrary code execution expands the attack surface (filesystem/network exfiltration, resource exhaustion) and complicates auditing.

## Decision

For the MVP and core research pipeline, we enforce a **constrained declarative Signal DSL** (`SignalDefinition` + `SignalType` enum) paired with an allowlisted `DatasetCatalog` and a mandatory pre-execution `ExperimentValidator`:

- Signals must select from allowlisted, unit-tested causal primitives (`abnormal_volume_reversal`, `abnormal_volume_momentum`, `cross_sectional_momentum`, `mean_reversion_zscore`, `volatility_breakout`, `moving_average_crossover`).
- Any `custom_expression` or forbidden code token is rejected by `ExperimentValidator`.
- Execution lag (`lag_bars >= 1`), non-negative `feature_shift >= 0`, chronological train/val/test separation, and purge gaps (`>= max(purge_days, horizon_days)`) are enforced structurally before the backtester is invoked.
- The `EventDrivenBacktester` feeds each symbol strictly trailing history up to bar `t` and fills orders at bar `t + lag_bars` open using fixed-point `Decimal` math.

## Consequences

### Positive
- Eliminates an entire class of accidental and adversarial lookahead bugs by construction.
- Guarantees 100% bit-for-bit reproducibility (`trades_digest` + metric equality within `1e-9`) for any pinned `(config_hash, dataset_checksum, code_revision, seed)`.
- Makes every experiment diffable, hashable, and auditable in SQLite without executing untrusted code.

### Trade-offs & Mitigations
- **Expressiveness limit:** Novel factor formulas outside the allowlisted signal library cannot be expressed ad hoc via prompts.
- **Mitigation:** New signal primitives are added through reviewed, tested Python modules with causal invariance tests (verifying that mutating future bars does not alter past trades) before joining the allowlist.
