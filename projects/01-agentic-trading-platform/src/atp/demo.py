"""Terminal walkthrough on simulated data: `python -m atp.demo` (or `make demo`).

Runs in-process against a throwaway database with a fixed clock and seed, so the
output is identical on every run. All activity is PAPER activity.
"""

from __future__ import annotations

from datetime import UTC, datetime

from .clock import FixedClock
from .config import Settings
from .ids import SequentialIds
from .oms import BrokerUnavailableError
from .planner import RuleBasedPlanner
from .service import ServiceError, TradingService

USER = "alice"
START = datetime(2026, 1, 5, 15, 0, 0, tzinfo=UTC)  # Monday 10:00 New York


def heading(text: str) -> None:
    print(f"\n\033[1m== {text} ==\033[0m")


def approve(svc: TradingService, text: str):
    print(f'  intent    "{text}"')
    _, proposal = svc.create_intent(USER, text)
    price = f"limit {proposal.limit_price}" if proposal.limit_price else "market"
    print(
        f"  proposal  {proposal.side.value.upper()} {proposal.quantity} {proposal.symbol} @ {price}"
        f"   {proposal.proposal_hash[:23]}..."
    )
    decision = svc.evaluate(USER, proposal.id)
    codes = ", ".join(v.code for v in decision.violations) or "none"
    print(f"  risk      passed={decision.passed}  violations: {codes}")
    if not decision.passed:
        return proposal, None
    approval, order = svc.decide(USER, proposal.id, proposal.proposal_hash, "approve")
    print(f"  approval  by {approval.approver_id}, expires {approval.expires_at:%H:%M:%S} UTC")
    return proposal, order


def show(order) -> None:
    extra = f"  reason={order.reject_reason}" if order.reject_reason else ""
    fill = f" @ {order.avg_fill_price}" if order.avg_fill_price else ""
    print(
        f"  order     {order.state.value}  filled {order.filled_qty}/{order.quantity}{fill}{extra}"
    )


def main() -> None:
    clock = FixedClock(START)
    svc = TradingService(
        Settings(db_path=":memory:", approval_secret=b"demo-only", planner="rules"),
        clock=clock,
        new_id=SequentialIds(),
        planner=RuleBasedPlanner(),
    )
    print("PAPER TRADING DEMO: simulated prices, simulated broker, no real money.")
    print(f"Simulated clock starts {START:%Y-%m-%d %H:%M} UTC; planner = rules (offline).")

    heading("1. Happy path")
    proposal, order = approve(svc, "Buy $500 of SPY using a limit order")
    show(svc.submit_order(USER, order.client_order_id))
    happy_cid = proposal.correlation_id

    heading("2. Duplicate submit resolves to one broker order")
    for _ in range(3):
        svc.submit_order(USER, order.client_order_id)
    n = len(svc.db.all("SELECT 1 FROM broker_orders"))
    print(f"  4 submits of {order.client_order_id} -> {n} broker order")

    heading("3. Policy gate blocks an oversized trade")
    clock.advance(5)
    proposal, order = approve(svc, "Buy 5000 shares of NVDA")
    try:
        svc.decide(USER, proposal.id, proposal.proposal_hash, "approve")
    except ServiceError as exc:
        print(f"  approve   refused: {exc.code}")

    heading("4. Order changed after approval never executes")
    clock.advance(5)
    proposal, order = approve(svc, "Buy 4 shares of QQQ")
    svc.db.execute("UPDATE proposals SET quantity = 400 WHERE id = ?", (proposal.id,))
    print("  tamper    quantity edited 4 -> 400 directly in the database")
    show(svc.submit_order(USER, order.client_order_id))

    heading("5. Expired approval")
    clock.advance(5)
    _, order = approve(svc, "Buy 10 shares of AAPL")
    clock.advance(svc.settings.approval_ttl_seconds + 1)
    print(f"  wait      {svc.settings.approval_ttl_seconds + 1}s")
    show(svc.submit_order(USER, order.client_order_id))

    heading("6. Stale market data")
    _, order = approve(svc, "Buy 10 shares of MSFT")
    svc.market_data.stall()
    clock.advance(30)
    print("  fault     quote feed stalled for 30s")
    show(svc.submit_order(USER, order.client_order_id))
    svc.market_data.resume()

    heading("7. Broker outage: lost acknowledgement, then recovery")
    _, order = approve(svc, "Buy 20 shares of TLT")
    svc.broker.faults.drop_next_ack = True
    svc.oms.max_submit_attempts = 1
    print("  fault     broker books the order but the reply is lost")
    try:
        svc.submit_order(USER, order.client_order_id)
    except BrokerUnavailableError as exc:
        print(f"  submit    503 retryable; local state = {exc.order.state.value}, nothing assumed")
    show(svc.submit_order(USER, order.client_order_id))
    n = len(svc.db.all("SELECT 1 FROM broker_orders WHERE symbol = 'TLT'"))
    print(f"  result    retry reconciled with the broker instead of resending -> {n} TLT order")

    heading("Portfolio (paper)")
    portfolio = svc.portfolio(USER)
    print(f"  cash      {portfolio['cash']}")
    for p in portfolio["positions"]:
        print(f"  position  {p.symbol:5} {p.quantity:>4} @ {p.avg_cost}")

    heading(f"Audit trail for the happy path ({happy_cid})")
    for e in svc.audit.by_correlation(happy_cid):
        print(f"  {e['seq']:>3}  {e['ts'][11:19]}  {e['actor']:<14} {e['event_type']}")
    ok, _ = svc.audit.verify_chain()
    print(f"\n  {len(svc.audit.all())} audit events, hash chain intact: {ok}")

    heading("Metrics")
    for line in svc.metrics.render().decode().splitlines():
        if "_created" not in line and line.startswith(
            (
                "atp_order_submissions_total",
                "atp_policy_rejections_total",
                "atp_order_submit_retries_total",
                "atp_order_reconciliation",
            )
        ):
            print("  " + line)


if __name__ == "__main__":
    main()
