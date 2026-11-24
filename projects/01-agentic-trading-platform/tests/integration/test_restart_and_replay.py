"""State survives restarts, and a fixed clock + seed replays byte-for-byte."""

from conftest import SESSION_OPEN_UTC, USER, approved_order, make_service

from atp.clock import FixedClock
from atp.domain import OrderState


def test_approved_order_survives_restart(tmp_path):
    path = str(tmp_path / "atp.sqlite3")
    clock = FixedClock(SESSION_OPEN_UTC)
    first = make_service(clock, db_path=path)
    _, order = approved_order(first)
    first.db.close()

    second = make_service(clock, db_path=path)
    second.new_id._counters.update(brk=100, evt=100, rsk=100)  # avoid sequential-id reuse
    done = second.submit_order(USER, order.client_order_id)
    assert done.state is OrderState.FILLED
    assert second.audit.verify_chain() == (True, None)


def test_restart_mid_dispatch_reconciles_instead_of_resending(tmp_path):
    """Crash after the broker booked the order but before we recorded the reply."""
    path = str(tmp_path / "atp.sqlite3")
    clock = FixedClock(SESSION_OPEN_UTC)
    first = make_service(clock, db_path=path, max_submit_attempts=1)
    _, order = approved_order(first)
    first.broker.faults.drop_next_ack = True
    try:
        first.submit_order(USER, order.client_order_id)
    except Exception:
        pass
    first.db.close()

    second = make_service(clock, db_path=path)
    second.new_id._counters.update(brk=100, evt=100, rsk=100)
    assert second.store.order(order.client_order_id).state is OrderState.SUBMITTING
    done = second.submit_order(USER, order.client_order_id)
    assert done.state is OrderState.FILLED
    assert len(second.db.all("SELECT * FROM broker_orders")) == 1
    assert second.broker.get_account(USER).position_qty("SPY") == 3


def test_approvals_do_not_survive_a_secret_rotation(tmp_path):
    path = str(tmp_path / "atp.sqlite3")
    clock = FixedClock(SESSION_OPEN_UTC)
    first = make_service(clock, db_path=path)
    _, order = approved_order(first)
    first.db.close()

    from atp.config import Settings
    from atp.ids import SequentialIds
    from atp.planner import RuleBasedPlanner
    from atp.service import TradingService

    ids = SequentialIds()
    ids._counters.update(evt=100, rsk=100)
    rotated = TradingService(
        Settings(db_path=path, approval_secret=b"rotated", planner="rules"),
        clock=clock,
        new_id=ids,
        planner=RuleBasedPlanner(),
    )
    result = rotated.submit_order(USER, order.client_order_id)
    assert result.reject_reason == "APPROVAL_SIGNATURE_INVALID"
    assert rotated.db.all("SELECT * FROM broker_orders") == []


def _scripted_run() -> list[dict]:
    clock = FixedClock(SESSION_OPEN_UTC)
    svc = make_service(clock)
    _, o1 = approved_order(svc, "Buy $500 of SPY using a limit order")
    clock.advance(3)
    svc.submit_order(USER, o1.client_order_id)
    clock.advance(20)
    _, o2 = approved_order(svc, "Sell 2 shares of SPY at market")
    svc.broker.faults.drop_next_ack = True
    svc.submit_order(USER, o2.client_order_id)
    clock.advance(5)
    _, p3 = svc.create_intent(USER, "Buy 5000 shares of QQQ")
    svc.evaluate(USER, p3.id)
    _, o4 = approved_order(svc, "Buy 10 shares of AAPL")
    svc.market_data.stall()
    clock.advance(60)
    svc.submit_order(USER, o4.client_order_id)
    assert svc.audit.verify_chain() == (True, None)
    return svc.audit.all()


def test_replay_is_deterministic():
    first, second = _scripted_run(), _scripted_run()
    assert len(first) > 40
    assert first == second  # includes prices, timestamps, IDs and the hash chain
