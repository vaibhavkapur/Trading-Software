"""Deterministic event-driven backtester with strict causal bar-by-bar execution.

Causality guarantees:
- On trading day `t`, signals are computed strictly from bars up to index `t - feature_shift`
  (never peeking at bar `t+1` or future prices/volumes).
- Orders generated from bar `t` close are scheduled for execution at bar `t + lag_bars`
  (minimum `lag_bars >= 1`) using bar `t + lag_bars`'s OPEN price.
- All cash, equity, fill prices, notional, and transaction costs use fixed-point `Decimal`
  arithmetic with integer share quantities.
- Walk-forward evaluation folds are computed strictly forward in time.
"""

from __future__ import annotations

import hashlib
import json
import math
from dataclasses import dataclass
from datetime import date
from decimal import ROUND_DOWN, ROUND_HALF_UP, Decimal
from typing import Any

from quant_agent.backtest.costs import CENT, TransactionCostModel
from quant_agent.backtest.stats import (
    TRADING_DAYS_PER_YEAR,
    compute_is_vs_oos_degradation,
    compute_max_drawdown,
    compute_sharpe,
    compute_sortino,
    compute_t_stat_and_pvalue,
    evaluate_multiple_testing,
    sample_std,
    stationary_block_bootstrap,
)
from quant_agent.data.catalog import DatasetCatalog
from quant_agent.models.domain import (
    DateRange,
    Experiment,
    MarketBar,
    SplitMetrics,
    TradeRecord,
)
from quant_agent.models.signals import (
    DirectionMode,
    RebalanceFrequency,
    SignalDefinition,
    SignalType,
)


@dataclass
class BacktestExecutionOutput:
    train_metrics: SplitMetrics
    val_metrics: SplitMetrics
    test_metrics: SplitMetrics
    walk_forward_folds: list[SplitMetrics]
    is_vs_oos_degradation: float
    trades: list[TradeRecord]
    equity_curve: list[dict[str, Any]]
    trades_digest: str
    failure_cases: list[str]
    test_daily_returns: list[float]


def _compute_signal_score(
    history: list[MarketBar],
    signal_def: SignalDefinition,
) -> float | None:
    """Compute a strictly causal scalar signal from trailing bar history ending at t."""
    eff_end = len(history) - signal_def.feature_shift
    if eff_end <= signal_def.lookback_days:
        return None

    window = history[eff_end - signal_def.lookback_days : eff_end]
    closes = [float(b.close) for b in window]
    volumes = [float(b.volume) for b in window]
    short_w = signal_def.short_window

    if closes[-1] <= 0 or closes[0] <= 0:
        return None

    st = signal_def.signal_type

    if st in (SignalType.ABNORMAL_VOLUME_REVERSAL, SignalType.ABNORMAL_VOLUME_MOMENTUM):
        prior_vols = volumes[:-1]
        mean_vol = sum(prior_vols) / len(prior_vols)
        std_vol = sample_std(prior_vols)
        if std_vol <= 1e-9:
            return 0.0
        vol_z = (volumes[-1] - mean_vol) / std_vol
        day_ret = (closes[-1] - closes[-2]) / closes[-2]
        if vol_z < signal_def.zscore_threshold:
            return 0.0
        # Reversal bets against the abnormal-volume bar move; momentum follows it
        sign = -1.0 if st == SignalType.ABNORMAL_VOLUME_REVERSAL else 1.0
        return sign * math.copysign(vol_z, day_ret) if day_ret != 0.0 else 0.0

    if st == SignalType.CROSS_SECTIONAL_MOMENTUM:
        # Cumulative return over lookback skipping the most recent short_window days
        anchor_idx = max(0, len(closes) - short_w - 1)
        if closes[0] <= 0:
            return 0.0
        return (closes[anchor_idx] - closes[0]) / closes[0]

    if st == SignalType.MEAN_REVERSION_ZSCORE:
        mu = sum(closes) / len(closes)
        sd = sample_std(closes)
        if sd <= 1e-9:
            return 0.0
        z = (closes[-1] - mu) / sd
        if abs(z) < signal_def.zscore_threshold:
            return 0.0
        return -z

    if st == SignalType.VOLATILITY_BREAKOUT:
        rets = [(closes[i] - closes[i - 1]) / closes[i - 1] for i in range(1, len(closes))]
        sd = sample_std(rets[:-1]) if len(rets) > 2 else 0.0
        if sd <= 1e-9:
            return 0.0
        z = rets[-1] / sd
        if abs(z) < signal_def.zscore_threshold:
            return 0.0
        return z

    if st == SignalType.MOVING_AVERAGE_CROSSOVER:
        short_ma = sum(closes[-short_w:]) / short_w
        long_ma = sum(closes) / len(closes)
        if long_ma <= 1e-9:
            return 0.0
        return (short_ma - long_ma) / long_ma

    return 0.0


class EventDrivenBacktester:
    """Strictly causal, deterministic event-driven backtester."""

    def __init__(self, catalog: DatasetCatalog) -> None:
        self.catalog = catalog

    def run(
        self,
        experiment: Experiment,
        *,
        prior_trial_p_values: list[float] | None = None,
        prior_trial_sharpes: list[float] | None = None,
    ) -> tuple[BacktestExecutionOutput, Any]:
        """Run train, validation, test, and optional walk-forward splits."""
        bars_by_sym = self.catalog.load_bars(
            experiment.dataset_version_id,
            experiment.universe,
            start=experiment.train_range.start,
            end=experiment.test_range.end,
        )
        cost_model = TransactionCostModel(experiment.cost_assumptions)

        all_trades: list[TradeRecord] = []
        all_equity_points: list[dict[str, Any]] = []

        train_metrics, train_trades, train_eq, _ = self._simulate_split(
            experiment=experiment,
            bars_by_sym=bars_by_sym,
            cost_model=cost_model,
            split_name="train",
            date_range=experiment.train_range,
            bootstrap_seed=experiment.seed + 11,
        )
        val_metrics, val_trades, val_eq, _ = self._simulate_split(
            experiment=experiment,
            bars_by_sym=bars_by_sym,
            cost_model=cost_model,
            split_name="val",
            date_range=experiment.val_range,
            bootstrap_seed=experiment.seed + 23,
        )
        test_metrics, test_trades, test_eq, test_daily_returns = self._simulate_split(
            experiment=experiment,
            bars_by_sym=bars_by_sym,
            cost_model=cost_model,
            split_name="test",
            date_range=experiment.test_range,
            bootstrap_seed=experiment.seed + 37,
        )

        all_trades.extend(train_trades)
        all_trades.extend(val_trades)
        all_trades.extend(test_trades)
        all_equity_points.extend(train_eq)
        all_equity_points.extend(val_eq)
        all_equity_points.extend(test_eq)

        wf_folds: list[SplitMetrics] = []
        if experiment.walk_forward.enabled:
            wf_folds = self._run_walk_forward(experiment, bars_by_sym, cost_model)

        degradation = compute_is_vs_oos_degradation(
            train_metrics.sharpe_ratio, test_metrics.sharpe_ratio
        )

        # Multiple-testing statistics across all historical trials for this hypothesis
        all_pvals = list(prior_trial_p_values or []) + [test_metrics.p_value_two_sided]
        all_sharpes = list(prior_trial_sharpes or []) + [test_metrics.sharpe_ratio]
        mt_summary = evaluate_multiple_testing(
            daily_returns=test_daily_returns,
            raw_p_value=test_metrics.p_value_two_sided,
            all_trial_p_values=all_pvals,
            trial_sharpes=all_sharpes,
        )

        failure_cases = self._diagnose_failure_cases(
            train_metrics=train_metrics,
            val_metrics=val_metrics,
            test_metrics=test_metrics,
            degradation=degradation,
            mt_warning=mt_summary.warning_triggered,
        )

        trades_digest = self._compute_trades_digest(all_trades)

        output = BacktestExecutionOutput(
            train_metrics=train_metrics,
            val_metrics=val_metrics,
            test_metrics=test_metrics,
            walk_forward_folds=wf_folds,
            is_vs_oos_degradation=degradation,
            trades=all_trades,
            equity_curve=all_equity_points,
            trades_digest=trades_digest,
            failure_cases=failure_cases,
            test_daily_returns=test_daily_returns,
        )
        return output, mt_summary

    def _run_walk_forward(
        self,
        experiment: Experiment,
        bars_by_sym: dict[str, list[MarketBar]],
        cost_model: TransactionCostModel,
    ) -> list[SplitMetrics]:
        """Partition the validation+test horizon into sequential OOS walk-forward folds."""
        all_dates = sorted(
            {
                b.trading_date
                for bars in bars_by_sym.values()
                for b in bars
                if experiment.val_range.start <= b.trading_date <= experiment.test_range.end
            }
        )
        num_folds = experiment.walk_forward.num_folds
        if len(all_dates) < num_folds * 10:
            return []

        fold_size = len(all_dates) // num_folds
        folds: list[SplitMetrics] = []
        for idx in range(num_folds):
            start_i = idx * fold_size
            end_i = (idx + 1) * fold_size - 1 if idx < num_folds - 1 else len(all_dates) - 1
            fold_range = DateRange(start=all_dates[start_i], end=all_dates[end_i])
            metrics, _, _, _ = self._simulate_split(
                experiment=experiment,
                bars_by_sym=bars_by_sym,
                cost_model=cost_model,
                split_name=f"wf_fold_{idx + 1}",
                date_range=fold_range,
                bootstrap_seed=experiment.seed + 100 + idx,
            )
            folds.append(metrics)
        return folds

    def _simulate_split(
        self,
        *,
        experiment: Experiment,
        bars_by_sym: dict[str, list[MarketBar]],
        cost_model: TransactionCostModel,
        split_name: str,
        date_range: DateRange,
        bootstrap_seed: int,
    ) -> tuple[SplitMetrics, list[TradeRecord], list[dict[str, Any]], list[float]]:
        """Run bar-by-bar event loop on [date_range.start, date_range.end].

        Warmup bars strictly BEFORE `date_range.start` may be read only for
        lookback indicators (never traded, never leaking future bars).
        """
        symbols = sorted(experiment.universe)
        # Index bars by date per symbol
        bar_map: dict[str, dict[date, MarketBar]] = {
            sym: {b.trading_date: b for b in bars_by_sym[sym]} for sym in symbols
        }
        all_dates = sorted({d for m in bar_map.values() for d in m})
        split_dates = [d for d in all_dates if date_range.start <= d <= date_range.end]

        if not split_dates:
            empty = SplitMetrics(
                split_name=split_name,
                start_date=date_range.start,
                end_date=date_range.end,
                trading_days=0,
                num_trades=0,
                total_return=0.0,
                annualized_return=0.0,
                annualized_volatility=0.0,
                sharpe_ratio=0.0,
                sortino_ratio=0.0,
                max_drawdown=0.0,
                calmar_ratio=0.0,
                win_rate=0.0,
                profit_factor=0.0,
                turnover_annualized=0.0,
                gross_pnl=Decimal("0.00"),
                total_transaction_costs=Decimal("0.00"),
                net_pnl=Decimal("0.00"),
                t_statistic=0.0,
                p_value_two_sided=1.0,
                bootstrap_sharpe_ci_low=0.0,
                bootstrap_sharpe_ci_high=0.0,
                bootstrap_mean_return_ci_low=0.0,
                bootstrap_mean_return_ci_high=0.0,
            )
            return empty, [], [], []

        date_to_idx = {d: i for i, d in enumerate(all_dates)}
        cash = experiment.initial_capital
        positions: dict[str, int] = {sym: 0 for sym in symbols}
        pending_targets: dict[int, tuple[date, dict[str, float]]] = {}

        trades: list[TradeRecord] = []
        equity_points: list[dict[str, Any]] = []
        equity_floats: list[float] = []
        daily_returns: list[float] = []
        total_costs = Decimal("0.00")
        total_traded_notional = Decimal("0.00")
        prev_equity = experiment.initial_capital
        trade_seq = 0

        sig = experiment.signal_def
        horizon = experiment.horizon_days

        for split_pos, cur_date in enumerate(split_dates):
            global_idx = date_to_idx[cur_date]

            # STEP 1 (Market Open): Execute any pending orders scheduled for this bar's OPEN
            if global_idx in pending_targets:
                sig_date, target_weights = pending_targets.pop(global_idx)
                # Mark portfolio value at open for sizing
                open_equity = cash
                for sym in symbols:
                    b = bar_map[sym].get(cur_date)
                    if b is not None:
                        open_equity += Decimal(positions[sym]) * b.open

                for sym in symbols:
                    b = bar_map[sym].get(cur_date)
                    if b is None or b.open <= Decimal("0"):
                        continue
                    w = Decimal(str(target_weights.get(sym, 0.0)))
                    target_notional = open_equity * w
                    # Integer share quantity (fixed-point, rounded toward zero)
                    target_qty = int(
                        (target_notional / b.open).quantize(Decimal("1"), rounding=ROUND_DOWN)
                    )
                    delta_qty = target_qty - positions[sym]
                    if delta_qty == 0:
                        continue

                    side = "BUY" if delta_qty > 0 else "SELL"
                    abs_qty = abs(delta_qty)

                    # Trailing ADV strictly before today (no lookahead)
                    hist_vols = [
                        bar_map[sym][all_dates[j]].volume
                        for j in range(max(0, global_idx - 20), global_idx)
                        if all_dates[j] in bar_map[sym]
                    ]
                    adv = int(sum(hist_vols) / len(hist_vols)) if hist_vols else max(1, b.volume)

                    fill = cost_model.estimate_fill(
                        side=side,
                        quantity=abs_qty,
                        reference_price=b.open,
                        trailing_adv_shares=adv,
                    )

                    # Cash flow: BUY decreases cash by fill_price * qty + commission
                    # SELL increases cash by fill_price * qty - commission
                    # Note: fill_price already includes adverse spread + impact slippage
                    gross_cash_leg = (fill.fill_price * Decimal(abs_qty)).quantize(
                        CENT, rounding=ROUND_HALF_UP
                    )
                    if side == "BUY":
                        cash -= gross_cash_leg + fill.commission
                    else:
                        cash += gross_cash_leg - fill.commission

                    positions[sym] += delta_qty
                    total_costs += fill.total_cost
                    total_traded_notional += fill.notional

                    trade_seq += 1
                    trades.append(
                        TradeRecord(
                            trade_id=f"{split_name}_t{trade_seq:05d}",
                            symbol=sym,
                            side=side,
                            quantity=abs_qty,
                            signal_date=sig_date,
                            execution_date=cur_date,
                            event_time_utc=b.event_time_utc,
                            receive_time_utc=b.receive_time_utc,
                            reference_price=b.open,
                            fill_price=fill.fill_price,
                            notional=fill.notional,
                            commission=fill.commission,
                            spread_cost=fill.spread_cost,
                            impact_cost=fill.impact_cost,
                            total_cost=fill.total_cost,
                            split_name=split_name,
                        )
                    )

            # STEP 2 (Market Close): Accrue short borrow fee & mark-to-market equity
            short_notional = Decimal("0.00")
            mtm_value = Decimal("0.00")
            for sym in symbols:
                b = bar_map[sym].get(cur_date)
                if b is None:
                    continue
                pos_val = Decimal(positions[sym]) * b.close
                mtm_value += pos_val
                if positions[sym] < 0:
                    short_notional += abs(pos_val)

            borrow_fee = cost_model.daily_short_borrow_fee(short_notional)
            cash -= borrow_fee
            total_costs += borrow_fee

            equity = (cash + mtm_value).quantize(CENT, rounding=ROUND_HALF_UP)
            day_ret = (
                float((equity - prev_equity) / prev_equity) if prev_equity > Decimal("0") else 0.0
            )
            daily_returns.append(day_ret)
            equity_floats.append(float(equity))
            equity_points.append(
                {
                    "split": split_name,
                    "date": cur_date.isoformat(),
                    "equity": str(equity),
                    "daily_return": round(day_ret, 6),
                }
            )
            prev_equity = equity

            # STEP 3 (Post-Close Signal Generation): Compute strictly causal targets
            # scheduled for bar `global_idx + sig.lag_bars`
            rebalance_due = (
                sig.rebalance == RebalanceFrequency.DAILY and (split_pos % horizon == 0)
            ) or (sig.rebalance == RebalanceFrequency.WEEKLY and cur_date.weekday() == 4)
            exec_idx = global_idx + sig.lag_bars
            if rebalance_due and exec_idx < len(all_dates):
                scores: dict[str, float] = {}
                for sym in symbols:
                    # Gather strictly causal history up to and including `global_idx`
                    hist_start = max(0, global_idx - sig.lookback_days - sig.feature_shift - 2)
                    hist = [
                        bar_map[sym][all_dates[j]]
                        for j in range(hist_start, global_idx + 1)
                        if all_dates[j] in bar_map[sym]
                    ]
                    score = _compute_signal_score(hist, sig)
                    if score is not None and score != 0.0:
                        scores[sym] = score

                target_weights = self._scores_to_weights(scores, sig)
                pending_targets[exec_idx] = (cur_date, target_weights)

        # Compute summary metrics for this split
        n_days = len(daily_returns)
        net_pnl = (prev_equity - experiment.initial_capital).quantize(CENT, rounding=ROUND_HALF_UP)
        gross_pnl = (net_pnl + total_costs).quantize(CENT, rounding=ROUND_HALF_UP)
        total_return = float(net_pnl / experiment.initial_capital)
        ann_return = (
            ((1.0 + total_return) ** (TRADING_DAYS_PER_YEAR / n_days) - 1.0)
            if n_days > 0 and total_return > -0.99
            else -1.0
        )
        ann_vol = sample_std(daily_returns) * math.sqrt(TRADING_DAYS_PER_YEAR)
        sharpe = compute_sharpe(daily_returns)
        sortino = compute_sortino(daily_returns)
        max_dd = compute_max_drawdown(equity_floats)
        calmar = (ann_return / max_dd) if max_dd > 1e-9 else 0.0
        t_stat, p_val = compute_t_stat_and_pvalue(daily_returns)
        boot = stationary_block_bootstrap(
            daily_returns,
            num_resamples=250,
            avg_block_len=max(2, min(10, horizon)),
            seed=bootstrap_seed,
        )

        pos_days = sum(1 for r in daily_returns if r > 0)
        active_days = sum(1 for r in daily_returns if r != 0)
        win_rate = (pos_days / active_days) if active_days > 0 else 0.0
        gains = sum(r for r in daily_returns if r > 0)
        losses = abs(sum(r for r in daily_returns if r < 0))
        profit_factor = (gains / losses) if losses > 1e-12 else (2.0 if gains > 0 else 0.0)
        avg_equity = sum(equity_floats) / len(equity_floats) if equity_floats else 1.0
        turnover_ann = (float(total_traded_notional) / max(1.0, avg_equity)) * (
            TRADING_DAYS_PER_YEAR / max(1, n_days)
        )

        metrics = SplitMetrics(
            split_name=split_name,
            start_date=date_range.start,
            end_date=date_range.end,
            trading_days=n_days,
            num_trades=len(trades),
            total_return=round(total_return, 6),
            annualized_return=round(ann_return, 6),
            annualized_volatility=round(ann_vol, 6),
            sharpe_ratio=round(sharpe, 4),
            sortino_ratio=round(sortino, 4),
            max_drawdown=round(max_dd, 6),
            calmar_ratio=round(calmar, 4),
            win_rate=round(win_rate, 4),
            profit_factor=round(profit_factor, 4),
            turnover_annualized=round(turnover_ann, 4),
            gross_pnl=gross_pnl,
            total_transaction_costs=total_costs.quantize(CENT, rounding=ROUND_HALF_UP),
            net_pnl=net_pnl,
            t_statistic=round(t_stat, 4),
            p_value_two_sided=round(p_val, 6),
            bootstrap_sharpe_ci_low=boot.sharpe_low,
            bootstrap_sharpe_ci_high=boot.sharpe_high,
            bootstrap_mean_return_ci_low=boot.mean_return_low,
            bootstrap_mean_return_ci_high=boot.mean_return_high,
        )
        return metrics, trades, equity_points, daily_returns

    def _scores_to_weights(
        self, scores: dict[str, float], sig: SignalDefinition
    ) -> dict[str, float]:
        """Convert cross-sectional signal scores into bounded portfolio weights (gross <= 1.0)."""
        if not scores:
            return {}

        ranked = sorted(scores.items(), key=lambda kv: (-kv[1], kv[0]))
        max_pos = sig.max_positions

        if sig.direction == DirectionMode.LONG_ONLY:
            longs = [(s, sc) for s, sc in ranked if sc > 0][:max_pos]
            if not longs:
                return {}
            w = round(0.95 / len(longs), 6)
            return {s: w for s, _ in longs}

        # LONG_SHORT: balanced dollar-neutral up to 0.50 long / -0.50 short
        longs = [(s, sc) for s, sc in ranked if sc > 0][: max(1, max_pos // 2)]
        shorts = [
            (s, sc) for s, sc in sorted(scores.items(), key=lambda kv: (kv[1], kv[0])) if sc < 0
        ][: max(1, max_pos // 2)]
        weights: dict[str, float] = {}
        if longs:
            lw = round(0.45 / len(longs), 6)
            for s, _ in longs:
                weights[s] = lw
        if shorts:
            sw = round(-0.45 / len(shorts), 6)
            for s, _ in shorts:
                if s not in weights:
                    weights[s] = sw
        return weights

    def _diagnose_failure_cases(
        self,
        *,
        train_metrics: SplitMetrics,
        val_metrics: SplitMetrics,
        test_metrics: SplitMetrics,
        degradation: float,
        mt_warning: bool,
    ) -> list[str]:
        cases: list[str] = []
        if test_metrics.num_trades < 10:
            cases.append(
                f"Low OOS sample size: only {test_metrics.num_trades} trades "
                "executed in test holdout."
            )
        if test_metrics.sharpe_ratio <= 0.0:
            cases.append(
                f"Negative or zero OOS Sharpe ratio ({test_metrics.sharpe_ratio:.2f}) in holdout."
            )
        if degradation > 0.75:
            cases.append(
                f"Severe in-sample to out-of-sample degradation: train Sharpe "
                f"{train_metrics.sharpe_ratio:.2f} vs test Sharpe {test_metrics.sharpe_ratio:.2f} "
                f"(drop of {degradation:.2f})."
            )
        if test_metrics.max_drawdown > 0.15:
            cases.append(f"Elevated holdout max drawdown ({test_metrics.max_drawdown:.1%}).")
        if (
            test_metrics.total_transaction_costs > abs(test_metrics.gross_pnl)
            and test_metrics.num_trades > 0
        ):
            cases.append(
                f"Transaction costs (${test_metrics.total_transaction_costs}) exceeded gross PnL "
                f"(${test_metrics.gross_pnl}) in holdout."
            )
        if test_metrics.bootstrap_sharpe_ci_low < 0.0 < test_metrics.bootstrap_sharpe_ci_high:
            cases.append(
                f"95% block-bootstrap Sharpe CI [{test_metrics.bootstrap_sharpe_ci_low:.2f}, "
                f"{test_metrics.bootstrap_sharpe_ci_high:.2f}] spans zero."
            )
        if val_metrics.sharpe_ratio * test_metrics.sharpe_ratio < 0:
            cases.append(
                "Sign flip between validation Sharpe and test holdout Sharpe "
                "indicates regime instability."
            )
        if mt_warning:
            cases.append(
                "Multiple-testing or Deflated Sharpe threshold triggered; risk of selection bias."
            )
        return cases

    def _compute_trades_digest(self, trades: list[TradeRecord]) -> str:
        payload = [
            {
                "id": t.trade_id,
                "sym": t.symbol,
                "side": t.side,
                "qty": t.quantity,
                "sig": t.signal_date.isoformat(),
                "exec": t.execution_date.isoformat(),
                "fill": str(t.fill_price),
                "cost": str(t.total_cost),
                "split": t.split_name,
            }
            for t in trades
        ]
        canonical = json.dumps(payload, separators=(",", ":"))
        return hashlib.sha256(canonical.encode("utf-8")).hexdigest()
