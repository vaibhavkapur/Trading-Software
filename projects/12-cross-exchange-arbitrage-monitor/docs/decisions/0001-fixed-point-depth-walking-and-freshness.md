# ADR 0001: Fixed-Point Depth-Walking Arithmetic and Dual-Timestamp Freshness Gates

- **Status:** Accepted
- **Date:** 2026-11-24
- **Context:** Cross-Exchange Arbitrage Monitor (`12-cross-exchange-arbitrage-monitor`)

## Decision

1. **Exact Fixed-Point Arithmetic (`1e8` Scale, `int64` + `big.Int` Intermediates):**
   All prices, quantities, notionals, fees, FX rates, slippage costs, and basis-point edges are computed using `src/fixed.Decimal` (scaled by `10^8`) rather than IEEE-754 `float64`. Conservative directional rounding is applied at every boundary:
   - Bids round **down** to tick size; asks round **up** to tick size.
   - Quantities floor **down** to lot size.
   - Taker fees, FX spread costs, and slippage buffers round **up** (`roundUp = true`).

2. **Marginal-Profitability Depth Walking:**
   Instead of evaluating only top-of-book touch prices (which overstates edge on thin books and understates executable size on deep books), the edge calculator walks buy-venue asks ascending and sell-venue bids descending simultaneously, stopping at the exact slice where marginal net unit profit after taker fees, FX spread, and modeled slippage reaches zero, or when pre-funded venue inventory is exhausted.

3. **Separate Venue `asof` vs. Local `received_at` Freshness Gates:**
   Rather than trusting a single timestamp, each book records UTC `asof` (exchange timestamp) and `received_at` (local ingress time). Both must satisfy `max_quote_age_ms`, `|received_at - asof| <= max_clock_skew_ms`, and `|buy.asof - sell.asof| <= max_leg_skew_ms`.

## Trade-offs Considered

| Alternative | Pros | Cons | Why Rejected |
| --- | --- | --- | --- |
| `float64` arithmetic | Slightly faster raw CPU ops; less boilerplate | Binary representation drift (`0.1 + 0.2 != 0.3`), non-deterministic cross-architecture rounding in replay digests | Violates financial safety boundary and breaks bit-exact replay hashes |
| External decimal library (`shopspring/decimal`) | Feature-rich arbitrary precision | Heap allocations per op and external dependency management | Fixed `1e8` scale fits crypto satoshi precision cleanly with zero external dependencies |
| Top-of-book-only edge calculation | O(1) computation | Misses multi-level liquidity and hides depth-walking slippage | Cannot estimate realistic executable notional |
| Single timestamp freshness | Simpler config | Cannot distinguish network delay from exchange clock drift or asynchronous leg updates | Would generate false alerts on stale or skewed books |

## Consequences

- Deterministic replay produces identical SHA-256 digests across runs and architectures.
- Conservative rounding guarantees the monitor never fabricates phantom positive edge from tick/lot quantization.
- Benchmark verification confirms sub-10 microsecond evaluation latency per pair despite `big.Int` overflow-safe multiplication/division.
