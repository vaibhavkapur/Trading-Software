"""Unit tests for EventDrivenBacktester causality, Decimal math, costs, and determinism."""

from __future__ import annotations

from datetime import date
from decimal import Decimal

from quant_agent.backtest.costs import TransactionCostModel
from quant_agent.backtest.engine import EventDrivenBacktester
from quant_agent.data.catalog import DatasetCatalog
from quant_agent.models.domain import MarketBar
from quant_agent.models.signals import TransactionCostAssumptions, WalkForwardConfig
from tests.conftest import make_valid_experiment, make_valid_hypothesis


def test_transaction_cost_model_fixed_point_and_adverse_slippage():
    model = TransactionCostModel(
        TransactionCostAssumptions(
            commission_bps=2.0,
            half_spread_bps=3.0,
            impact_bps_per_pct_adv=10.0,
        )
    )
    buy = model.estimate_fill(
        side="BUY",
        quantity=1000,
        reference_price=Decimal("100.00"),
        trailing_adv_shares=1_000_000,
    )
    sell = model.estimate_fill(
        side="SELL",
        quantity=1000,
        reference_price=Decimal("100.00"),
        trailing_adv_shares=1_000_000,
    )
    assert isinstance(buy.notional, Decimal)
    assert buy.fill_price > Decimal("100.00")
    assert sell.fill_price < Decimal("100.00")
    assert buy.total_cost == buy.commission + buy.spread_cost + buy.impact_cost
    assert buy.total_cost > Decimal("0.00")


def test_backtester_is_bit_for_bit_deterministic_and_strictly_causal(catalog: DatasetCatalog):
    hyp = make_valid_hypothesis()
    exp = make_valid_experiment(hyp, seed=777)
    exp.walk_forward = WalkForwardConfig(enabled=True, num_folds=3)

    bt = EventDrivenBacktester(catalog)
    out1, mt1 = bt.run(exp)
    out2, mt2 = bt.run(exp)

    assert out1.trades_digest == out2.trades_digest
    assert out1.test_metrics == out2.test_metrics
    assert mt1 == mt2
    assert len(out1.walk_forward_folds) == 3
    assert out1.test_metrics.num_trades > 0

    # Strict causality: every trade must execute strictly AFTER its signal_date
    for trade in out1.trades:
        assert trade.execution_date > trade.signal_date
        assert isinstance(trade.quantity, int)
        assert isinstance(trade.fill_price, Decimal)
        assert isinstance(trade.total_cost, Decimal)


def test_modifying_future_bars_does_not_affect_past_trades():
    """Causal invariance test: mutating 2024 test bars cannot alter 2021-2022 train trades."""
    cat_a = DatasetCatalog()
    cat_b = DatasetCatalog()

    # Perturb every 2024 bar in cat_b dramatically
    cutoff = date(2024, 1, 1)
    for sym, bars in cat_b._bars_cache["sim_us_liquid_equities_v1"].items():
        mutated: list[MarketBar] = []
        for b in bars:
            if b.trading_date >= cutoff:
                mutated.append(
                    b.model_copy(
                        update={
                            "open": b.open * Decimal("3"),
                            "high": b.high * Decimal("3"),
                            "low": b.low * Decimal("3"),
                            "close": b.close * Decimal("3"),
                            "volume": b.volume * 5,
                        }
                    )
                )
            else:
                mutated.append(b)
        cat_b._bars_cache["sim_us_liquid_equities_v1"][sym] = mutated

    hyp = make_valid_hypothesis()
    exp = make_valid_experiment(hyp, seed=42)

    out_a, _ = EventDrivenBacktester(cat_a).run(exp)
    out_b, _ = EventDrivenBacktester(cat_b).run(exp)

    # Train and Val splits (pre-2024) must be 100% identical!
    assert out_a.train_metrics == out_b.train_metrics
    assert out_a.val_metrics == out_b.val_metrics
    train_trades_a = [t for t in out_a.trades if t.split_name in ("train", "val")]
    train_trades_b = [t for t in out_b.trades if t.split_name in ("train", "val")]
    assert train_trades_a == train_trades_b


def test_higher_transaction_costs_reduce_net_pnl(catalog: DatasetCatalog):
    hyp = make_valid_hypothesis()
    exp_low = make_valid_experiment(hyp)
    exp_low.cost_assumptions = TransactionCostAssumptions(
        commission_bps=0.5, half_spread_bps=0.5, impact_bps_per_pct_adv=1.0
    )
    exp_high = make_valid_experiment(hyp)
    exp_high.cost_assumptions = TransactionCostAssumptions(
        commission_bps=15.0, half_spread_bps=20.0, impact_bps_per_pct_adv=40.0
    )

    bt = EventDrivenBacktester(catalog)
    out_low, _ = bt.run(exp_low)
    out_high, _ = bt.run(exp_high)

    assert (
        out_high.test_metrics.total_transaction_costs > out_low.test_metrics.total_transaction_costs
    )
    assert out_high.test_metrics.net_pnl < out_low.test_metrics.net_pnl
