# ADR 0001: Numeric Boundary Between Analytic Pricing (`float64`) and Order/Risk Accounting (`Decimal`), and Stdlib `math` vs. SciPy Kernels

- **Status:** Accepted
- **Date:** 2026-11-24
- **Rule/Model Versions:** `bsm-1.0.0`, `risk-rules-1.0.0`

## Context

Options infrastructure sits across two incompatible numerical domains:

1. **Continuous quantitative analytics** (Black–Scholes–Merton CDF/PDF, safeguarded Newton/bisection IV inversion, Cox–Ross–Rubinstein binomial trees, and 2D scenario grids) require transcendental functions (`exp`, `log`, `sqrt`, `erf`) and fast scalar iterations.
2. **Order, margin, and payoff accounting** (strike widths, tick-aligned limit prices, multi-leg net debit/credit, maximum loss estimates, and position average cost) must never suffer binary floating-point representation drift (e.g., `0.1 + 0.2 != 0.3` causing failed tick validation or penny discrepancies on defined-risk spread bounds).

Additionally, we had to decide whether to require NumPy/SciPy for scalar BSM/IV calls or build the core analytic kernel directly on Python's C-backed `math` standard library (`math.erf`, `math.exp`, `math.log`, `math.sqrt`).

## Decision

1. **Strict Numeric Boundary:**
   - All contract strikes, tick sizes, quotes (`bid`, `ask`, `underlying_px`), position `avg_price`, spread `net_debit_credit`, `max_loss_estimate`, `max_gain_estimate`, `breakeven_spot`, and order fills are represented and computed in fixed-point `Decimal` with explicit `ROUND_HALF_UP` quantization at currency/tick boundaries.
   - Transcendental pricing (`black_scholes.py`, `iv_solver.py`, `binomial.py`) operates in IEEE 754 `float64` and is explicitly guarded by put-call parity checks (`< 1e-11` residual) and central finite-difference Greek verification tests.
2. **Pure Stdlib `math` Analytic Kernel (Zero Heavy Native Runtime Dependency):**
   - `norm_cdf(x)` is implemented via `0.5 * (1.0 + math.erf(x / math.sqrt(2.0)))`, achieving full IEEE 754 float64 precision while avoiding per-call SciPy dispatch overhead for single-contract pricing and IV root-finding.
   - The pricing module exposes clean pure functions (`price_bsm`, `solve_implied_volatility`, `price_binomial_crr`) so a future vectorized NumPy or Rust FFI kernel can be swapped in behind the same interface without altering domain models or API schemas.
3. **Explicit Non-Goal Enforcement for American Contracts:**
   - European BSM is never silently presented as exact for American contracts. Instead, `ExerciseStyle.AMERICAN` emits structured warnings (`AMERICAN_PRICED_WITH_EUROPEAN_BSM`, `AMERICAN_SPREAD_EARLY_ASSIGNMENT`) and users/callers can invoke the CRR binomial tree (`model="BINOMIAL_CRR"`) to measure the early-exercise premium explicitly.

## Consequences

- **Positive:** Exact defined-risk spread invariant (`max_gain + max_loss == width * quantity * multiplier`) holds identically in `Decimal`; tick-size validation is exact; single-option BSM pricing runs in single-digit microseconds with zero C-compiler or SciPy wheel friction in CI/containers.
- **Trade-offs:** Large batch surface calibrations (tens of thousands of strikes per tick) would benefit from a vectorized NumPy or Rust pricing kernel in a future milestone; the current pure-Python CRR binomial tree is intended for on-demand single-contract verification rather than high-frequency surface fitting.
