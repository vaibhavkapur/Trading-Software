"""The backtest engine: one deterministic loop over one totally ordered event queue.

    historical events -> simulated clock -> strategy -> order intent
        -> risk gate / order manager -> exchange simulator -> portfolio -> metrics

Each market event is queued twice: once at venue time, where the exchange simulator
matches resting orders against it, and once at availability time, where it is shown to
the strategy. The strategy therefore never holds a reference to data that has not been
published yet, and orders it sends can only be filled by market data that the exchange
sees after those orders arrive.
"""

from __future__ import annotations

import hashlib
import inspect
import json
import random
import time
from collections.abc import Mapping
from fractions import Fraction
from typing import Any

from ._version import FILL_MODEL_VERSION, __version__
from .config import BacktestConfig, ConfigError
from .data import Dataset
from .eventbus import (
    P_CORPORATE_ACTION,
    P_EXECUTION,
    P_MARKET_DATA,
    P_REQUEST,
    P_TIMER,
    P_VENUE,
    EventQueue,
    SimClock,
)
from .events import ActionType, Bar, CorporateAction, EventType, MarketEvent, Quote, Timer
from .exchange import CORPORATE_ACTION, ExchangeSim, LatencyModel, derive_rng
from .fees import FeeModel
from .fixedpoint import div_round_half_even, to_units
from .orders import (
    OrderCancel,
    OrderManager,
    OrderStatus,
    OrderType,
    OrderView,
    Side,
    SimFill,
    SimOrder,
    TimeInForce,
)
from .portfolio import Portfolio, PortfolioSnapshot, ReconciliationError
from .results import (
    BacktestResult,
    BacktestRun,
    RuntimeStats,
    compute_result_hash,
    segment_metrics,
)
from .risk import RUN_ENDED, WARMUP, AccountState, RiskGate, validate
from .strategy import Context, Strategy
from .timeutil import fmt_ts

_ARRIVE = "arrive"
_CANCEL = "cancel"


class LookaheadError(RuntimeError):
    """An event was about to be shown to the strategy before its availability time."""


class _History:
    """Published bars for one symbol, raw and restated for the splits seen so far."""

    __slots__ = ("_epochs", "_factors", "adjusted", "raw")

    def __init__(self) -> None:
        self.raw: list[Bar] = []
        self.adjusted: list[Bar] = []
        self._epochs: list[int] = []
        self._factors: list[Fraction] = [Fraction(1)]

    def append(self, bar: Bar) -> None:
        self.raw.append(bar)
        self.adjusted.append(bar)
        self._epochs.append(len(self._factors) - 1)

    def apply_split(self, num: int, den: int) -> None:
        self._factors.append(self._factors[-1] * Fraction(num, den))
        current = self._factors[-1]
        factors = self._factors
        # Always restate from the raw bars so rounding never compounds across splits.
        self.adjusted = [
            _restate(bar, factors[epoch] / current)
            for bar, epoch in zip(self.raw, self._epochs, strict=True)
        ]


def _restate(bar: Bar, factor: Fraction) -> Bar:
    """Scale prices by ``factor`` and volume by its inverse."""
    if factor == 1:
        return bar
    num, den = factor.numerator, factor.denominator
    return Bar(
        bar.ts_open,
        bar.ts_close,
        max(1, div_round_half_even(bar.open * num, den)),
        max(1, div_round_half_even(bar.high * num, den)),
        max(1, div_round_half_even(bar.low * num, den)),
        max(1, div_round_half_even(bar.close * num, den)),
        bar.volume * den // num,
    )


class _SimContext(Context):
    """The engine's implementation of the strategy-facing interface."""

    __slots__ = ("_engine",)

    def __init__(self, engine: Backtest) -> None:
        self._engine = engine

    @property
    def now(self) -> int:
        return self._engine._clock.now

    @property
    def symbols(self) -> tuple[str, ...]:
        return self._engine.dataset.symbols

    @property
    def cash(self) -> int:
        return self._engine._portfolio.cash

    @property
    def equity(self) -> int:
        return self._engine._portfolio.equity()

    @property
    def rng(self) -> random.Random:
        return self._engine._strategy_rng

    def submit_order(
        self,
        symbol: str,
        side: Side | str,
        qty: int,
        order_type: OrderType | str = OrderType.MARKET,
        limit_px: int | None = None,
        tif: TimeInForce | str = TimeInForce.GTC,
        tag: str | None = None,
    ) -> str:
        return self._engine._submit(symbol, side, qty, order_type, limit_px, tif, tag)

    def cancel_order(self, order_id: str) -> bool:
        return self._engine._request_cancel(order_id)

    def order(self, order_id: str) -> OrderView:
        return self._engine._oms.view(order_id)

    def open_orders(self, symbol: str | None = None) -> tuple[OrderView, ...]:
        return self._engine._oms.open_orders(symbol)

    def position(self, symbol: str) -> int:
        return self._engine._portfolio.position_qty(symbol)

    def mark(self, symbol: str) -> int | None:
        return self._engine._portfolio.marks.get(symbol)

    def history(self, symbol: str, n: int | None = None, adjusted: bool = True) -> tuple[Bar, ...]:
        history = self._engine._history.get(symbol)
        if history is None:
            return ()
        bars = history.adjusted if adjusted else history.raw
        if n is None:
            return tuple(bars)
        return tuple(bars[-n:]) if n > 0 else ()

    def last_quote(self, symbol: str) -> Quote | None:
        return self._engine._last_quote.get(symbol)

    def set_timer(self, ts: int, name: str = "") -> None:
        self._engine._set_timer(ts, name)

    def price(self, value: int | str | float) -> int:
        return to_units(value, self._engine.config.price_decimals)


class Backtest:
    """One run of one strategy over one dataset. Single use."""

    def __init__(
        self,
        dataset: Dataset,
        strategy: Strategy,
        config: BacktestConfig | None = None,
        *,
        strategy_info: Mapping[str, Any] | None = None,
    ) -> None:
        self.dataset = dataset
        self.strategy = strategy
        self.config = config or BacktestConfig()
        if dataset.price_decimals != self.config.price_decimals:
            raise ConfigError(
                f"dataset uses {dataset.price_decimals} price decimals but the config "
                f"uses {self.config.price_decimals}"
            )
        self._strategy_info = (
            dict(strategy_info) if strategy_info is not None else _describe(strategy)
        )

        execution = self.config.execution
        seed = self.config.seed
        static_ts = [e.ts_event for e in dataset.events]
        static_ts += [a.ts for a in dataset.corporate_actions]
        self._start_ts = min(static_ts, default=0)
        self._end_ts = max(
            max((e.ts for e in dataset.events), default=0),
            max((a.ts for a in dataset.corporate_actions), default=0),
        )

        self._clock = SimClock(self._start_ts)
        self._queue = EventQueue()
        fees = FeeModel(execution.fee_bps, execution.fee_per_unit)
        self._exchange = ExchangeSim(execution, fees, derive_rng(seed, "exchange.touch"))
        self._latency = LatencyModel(
            execution.latency_ns, execution.latency_jitter_ns, derive_rng(seed, "latency")
        )
        self._strategy_rng = derive_rng(seed, "strategy")
        self._risk = RiskGate(self.config.risk, fees)
        self._oms = OrderManager()
        self._portfolio = Portfolio(self.config.initial_cash, self._start_ts)
        self._context = _SimContext(self)

        self._history: dict[str, _History] = {}
        self._last_quote: dict[str, Quote] = {}
        self._last_data_ts: dict[str, int] = {}
        self._cancel_requested: set[str] = set()
        self._fills: list[SimFill] = []
        self._snapshots: list[PortfolioSnapshot] = []
        self._dirty = False
        self._accepting = True
        self._ran = False

    # -- the loop -------------------------------------------------------------------

    def run(self) -> BacktestResult:
        if self._ran:
            raise RuntimeError("a Backtest holds run state and can only be run once")
        self._ran = True

        items: list[tuple[int, int, Any]] = []
        for event in self.dataset.events:
            items.append((event.ts_event, P_VENUE, event))
            items.append((event.ts, P_MARKET_DATA, event))
        for action in self.dataset.corporate_actions:
            items.append((action.ts, P_CORPORATE_ACTION, action))
        queue, clock = self._queue, self._clock
        queue.load(items)

        handlers = {
            P_CORPORATE_ACTION: self._on_corporate_action,
            P_VENUE: self._on_venue,
            P_REQUEST: self._on_request,
            P_EXECUTION: self._on_execution,
            P_MARKET_DATA: self._on_market_data,
            P_TIMER: self._on_timer,
        }
        processed = 0
        now = clock.now
        started = time.perf_counter()
        try:
            self.strategy.on_start(self._context)
            while queue:
                ts, priority, _, payload = queue.pop()
                if ts != now:
                    if self._dirty:
                        self._snapshot(now)
                    clock.advance(ts)
                    now = ts
                handlers[priority](payload)
                processed += 1
            self._accepting = False
            self.strategy.on_end(self._context)
        except Exception as exc:
            exc.add_note(f"while processing simulated time {fmt_ts(clock.now)}")
            raise
        elapsed = time.perf_counter() - started
        if self._dirty or (processed and not self._snapshots):
            self._snapshot(now)
        return self._finish(processed, elapsed, started)

    # -- event handlers -------------------------------------------------------------

    def _on_corporate_action(self, action: CorporateAction) -> None:
        symbol = action.symbol
        now = self._clock.now
        if action.action_type is ActionType.SPLIT:
            self._portfolio.apply_split(symbol, action.ratio_num, action.ratio_den, now)
            history = self._history.get(symbol)
            if history is not None:
                history.apply_split(action.ratio_num, action.ratio_den)
            self._last_quote.pop(symbol, None)
            # Open orders are priced in pre-split terms, so none of them may survive.
            for order in self._oms.open_orders(symbol):
                if order.status is OrderStatus.PENDING:
                    self._exchange.cancel_on_arrival(order.id, CORPORATE_ACTION)
            for report in self._exchange.cancel_symbol(symbol, CORPORATE_ACTION, now):
                self._queue.push(now, P_EXECUTION, report)
        else:
            self._portfolio.apply_dividend(symbol, action.amount, now)
        self._dirty = True
        self.strategy.on_corporate_action(action, self._context)

    def _on_venue(self, event: MarketEvent) -> None:
        reports = self._exchange.on_market_event(event)
        if reports:
            now = self._clock.now
            push = self._queue.push
            for report in reports:
                push(now, P_EXECUTION, report)

    def _on_request(self, request: tuple[str, Any]) -> None:
        kind, payload = request
        now = self._clock.now
        if kind == _ARRIVE:
            self._oms.on_arrival(payload.id, now)
            reports = self._exchange.on_order_arrival(payload, now)
        else:
            reports = self._exchange.on_cancel_request(payload, now)
        for report in reports:
            self._queue.push(now, P_EXECUTION, report)

    def _on_execution(self, report: SimFill | OrderCancel | OrderView) -> None:
        if isinstance(report, SimFill):
            self._oms.on_fill(report)
            self._portfolio.apply_fill(report)
            self._fills.append(report)
            self._dirty = True
            self.strategy.on_fill(report, self._context)
        elif isinstance(report, OrderCancel):
            self.strategy.on_order_update(self._oms.on_cancel(report), self._context)
        else:
            self.strategy.on_order_update(report, self._context)

    def _on_market_data(self, event: MarketEvent) -> None:
        now = self._clock.now
        if event.ts > now or event.ts_event > now:
            raise LookaheadError(
                f"{event.symbol} event available at {event.ts} reached the strategy at {now}"
            )
        symbol = event.symbol
        payload = event.payload
        if event.event_type is EventType.BAR:
            history = self._history.get(symbol)
            if history is None:
                history = self._history[symbol] = _History()
            history.append(payload)
            self._portfolio.mark(symbol, payload.close)
        else:
            self._last_quote[symbol] = payload
            if payload.bid > 0 and payload.ask > 0:
                self._portfolio.mark(symbol, (payload.bid + payload.ask) // 2)
            elif payload.bid > 0 or payload.ask > 0:
                self._portfolio.mark(symbol, payload.bid or payload.ask)
        self._last_data_ts[symbol] = now
        self._dirty = True
        self.strategy.on_market_event(event, self._context)

    def _on_timer(self, timer: Timer) -> None:
        self.strategy.on_timer(timer, self._context)

    # -- requests from the strategy -------------------------------------------------

    def _submit(
        self,
        symbol: str,
        side: Side | str,
        qty: int,
        order_type: OrderType | str,
        limit_px: int | None,
        tif: TimeInForce | str,
        tag: str | None,
    ) -> str:
        now = self._clock.now
        portfolio = self._portfolio
        mark = portfolio.marks.get(symbol)
        order = SimOrder(
            id=self._oms.next_id(),
            symbol=symbol,
            side=Side(side),
            type=OrderType(order_type),
            qty=qty,
            limit_px=limit_px,
            submitted_at=now,
            tif=TimeInForce(tif),
            tag=tag,
            decision_px=mark,
        )
        trade_from = self.config.trade_from
        if not self._accepting:
            reason = RUN_ENDED
        elif (reason := validate(order)) is not None:
            pass
        elif trade_from is not None and now < trade_from:
            reason = WARMUP
        else:
            reason = self._risk.check(
                order,
                AccountState(
                    now=now,
                    cash=portfolio.cash,
                    position_qty=portfolio.position_qty(symbol),
                    mark=mark,
                    last_data_ts=self._last_data_ts.get(symbol),
                    open_orders=self._oms.open_orders(),
                ),
            )
        view = self._oms.register(order, reason)
        if reason is None:
            self._queue.push(now + self._latency.sample(), P_REQUEST, (_ARRIVE, order))
        elif self._accepting:
            self._queue.push(now, P_EXECUTION, view)
        return order.id

    def _request_cancel(self, order_id: str) -> bool:
        if not self._accepting or not self._oms.knows(order_id) or not self._oms.is_open(order_id):
            return False
        if order_id not in self._cancel_requested:
            self._cancel_requested.add(order_id)
            self._queue.push(
                self._clock.now + self._latency.sample(), P_REQUEST, (_CANCEL, order_id)
            )
        return True

    def _set_timer(self, ts: int, name: str) -> None:
        if ts < self._clock.now:
            raise ValueError(
                f"timer {name!r} at {fmt_ts(ts)} is in the past (now {fmt_ts(self._clock.now)})"
            )
        # Timers past the end of the data never fire, so a self-renewing timer terminates.
        if self._accepting and ts <= self._end_ts:
            self._queue.push(ts, P_TIMER, Timer(ts, name))

    # -- results --------------------------------------------------------------------

    def _snapshot(self, ts: int) -> None:
        self._snapshots.append(self._portfolio.snapshot(ts))
        self._dirty = False

    def _finish(self, processed: int, loop_seconds: float, started: float) -> BacktestResult:
        config = self.config
        orders = self._oms.all_orders()
        fills = tuple(self._fills)
        reconciliation = self._portfolio.reconcile(fills)
        _check_orders(orders, fills)

        snapshots = tuple(self._snapshots)
        metrics = segment_metrics(snapshots, fills, orders, config, start=config.trade_from)
        in_sample = out_of_sample = None
        if config.oos_start is not None:
            in_sample = segment_metrics(
                snapshots, fills, orders, config, start=config.trade_from, end=config.oos_start
            )
            out_of_sample = segment_metrics(
                snapshots, fills, orders, config, start=config.oos_start
            )

        config_hash = _hash_json(
            {
                "engine_version": __version__,
                "fill_model_version": FILL_MODEL_VERSION,
                "config": config.to_dict(),
                "strategy": self._strategy_info,
            }
        )
        data_version = self.dataset.data_version
        run_id = hashlib.sha256(f"{config_hash}|{data_version}|{config.seed}".encode()).hexdigest()[
            :16
        ]
        result_hash = compute_result_hash(
            orders, fills, self._portfolio.ledger, snapshots, config.price_decimals
        )
        runtime = RuntimeStats(processed, loop_seconds, time.perf_counter() - started)
        return BacktestResult(
            run=BacktestRun(run_id, config_hash, data_version, config.seed, metrics),
            config=config,
            strategy=self._strategy_info,
            data_summary=self.dataset.summary,
            orders=orders,
            fills=fills,
            ledger=tuple(self._portfolio.ledger),
            snapshots=snapshots,
            metrics=metrics,
            in_sample=in_sample,
            out_of_sample=out_of_sample,
            reconciliation=reconciliation,
            late_cancels=self._exchange.late_cancels,
            result_hash=result_hash,
            runtime=runtime,
        )


def run_backtest(
    dataset: Dataset,
    strategy: Strategy,
    config: BacktestConfig | None = None,
    *,
    strategy_info: Mapping[str, Any] | None = None,
) -> BacktestResult:
    return Backtest(dataset, strategy, config, strategy_info=strategy_info).run()


def _check_orders(orders: tuple[OrderView, ...], fills: tuple[SimFill, ...]) -> None:
    """Every order's filled quantity, notional and fees must equal the sum of its fills."""
    totals: dict[str, list[int]] = {}
    for fill in fills:
        entry = totals.setdefault(fill.order_id, [0, 0, 0])
        entry[0] += fill.qty
        entry[1] += fill.qty * fill.px
        entry[2] += fill.fee
    known = set()
    for order in orders:
        known.add(order.id)
        qty, notional, fees = totals.get(order.id, (0, 0, 0))
        if (qty, notional, fees) != (order.filled_qty, order.filled_notional, order.fees):
            raise ReconciliationError(f"order {order.id} does not match its fills")
        if order.status is OrderStatus.REJECTED:
            # Never reached the exchange, and its quantity may not even be a valid number.
            if qty:
                raise ReconciliationError(f"rejected order {order.id} has fills")
            continue
        if order.filled_qty > order.qty:
            raise ReconciliationError(f"order {order.id} is overfilled")
        if (order.status is OrderStatus.FILLED) != (order.filled_qty == order.qty):
            raise ReconciliationError(
                f"order {order.id} is {order.status} with {order.filled_qty}/{order.qty} filled"
            )
    if not known.issuperset(totals):
        raise ReconciliationError("a fill refers to an unknown order")


_PARAM_TYPES = (bool, int, float, str, type(None))


def _describe(strategy: Strategy) -> dict[str, Any]:
    """Identity of an in-process strategy: its class, parameters and source code."""
    cls = type(strategy)
    try:
        source = hashlib.sha256(inspect.getsource(cls).encode()).hexdigest()
    except (OSError, TypeError):
        source = None
    params = getattr(strategy, "params", None)
    if not isinstance(params, Mapping):
        # No declared parameters: use the plain public attributes set by the constructor,
        # so that two differently configured instances never share a config hash.
        params = {
            name: value
            for name, value in getattr(strategy, "__dict__", {}).items()
            if not name.startswith("_") and isinstance(value, _PARAM_TYPES)
        }
    return {
        "name": cls.__qualname__,
        "source_sha256": source,
        "params": {str(key): params[key] for key in sorted(params, key=str)},
    }


def _hash_json(value: Any) -> str:
    text = json.dumps(value, sort_keys=True, separators=(",", ":"), default=str)
    return "sha256:" + hashlib.sha256(text.encode()).hexdigest()
