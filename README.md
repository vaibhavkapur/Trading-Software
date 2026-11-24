# Trading Software

Fifteen independent projects cover the lifecycle of a simulated trade, from market data and order matching to risk checks, execution analysis, and broker reconciliation.

## Getting started

The Agentic Trading Platform demo requires Python 3.12 and Make. It walks a paper order through proposal, approval, and execution against a simulated broker.

```bash
git clone https://github.com/vaibhavkapur/Trading-Software.git
cd Trading-Software/projects/01-agentic-trading-platform
make setup
make demo
```

For another project, open its directory under `projects/` and follow its README. Each project has its own dependencies, tests, and demo commands. Run them from that project's directory.

## Projects

- [01 · Agentic Trading Platform](projects/01-agentic-trading-platform/) — Turns a natural-language request into a paper order with risk checks, approval, and an audit trail.
- [02 · Rust Matching Engine](projects/02-rust-matching-engine/) — Matches limit orders in price-time priority and replays command streams to reproduce exchange events.
- [03 · Multi-Venue Smart Order Router](projects/03-smart-order-router/) — Routes orders across simulated venues using price, fees, available depth, and venue health.
- [04 · Real-Time Limit Order Book](projects/04-real-time-limit-order-book/) — Reconstructs a level-2 order book from snapshots and updates, and visualizes gaps and stale quotes.
- [05 · Pre-Trade Risk Engine](projects/05-pre-trade-risk-engine/) — Checks proposed orders against exposure limits, reserves capacity, and records why each order passed or failed.
- [06 · Event-Driven Backtesting Engine](projects/06-event-driven-backtesting-engine/) — Replays market events to test strategies with simulated fills, fees, and slippage.
- [07 · Trading Order Management System](projects/07-trading-order-management-system/) — Tracks each order through retries, duplicate broker messages, partial fills, and reconciliation.
- [08 · Market Data Ingestion Platform](projects/08-market-data-platform/) — Normalizes market feeds, stores tick history, and serves historical queries and live subscriptions.
- [09 · Transaction Cost Analysis](projects/09-transaction-cost-analysis/) — Compares fills with quote and price benchmarks to measure spreads, slippage, and execution shortfall.
- [10 · Market Surveillance Engine](projects/10-market-surveillance-engine/) — Flags suspicious patterns in simulated order flow and preserves the evidence for case review.
- [11 · Options Trading Infrastructure](projects/11-options-trading-infrastructure/) — Prices options, aggregates portfolio Greeks, and previews defined-risk spreads before paper execution.
- [12 · Cross-Exchange Arbitrage Monitor](projects/12-cross-exchange-arbitrage-monitor/) — Compares venue quotes after accounting for fees, slippage, quote age, and transfer constraints.
- [13 · Broker Reliability & Reconciliation](projects/13-broker-reliability-reconciliation/) — Finds discrepancies between internal records and broker reports for orders, fills, cash, and positions.
- [14 · AI Quant Research Agent](projects/14-ai-quant-research-agent/) — Runs reproducible backtests for trading hypotheses with checks for look-ahead bias.
- [15 · Agentic Trading Guardrails](projects/15-agentic-trading-guardrails/) — Enforces delegated permissions and single-use approvals, and records policy decisions for paper orders.

To follow an order through the collection, start with matching and market data (02, 04, 08), then risk and routing (05, 07, 03), and finally execution analysis and agent controls (09, 01, 15).
