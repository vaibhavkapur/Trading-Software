import random

import pytest

from backtester.eventbus import (
    P_CORPORATE_ACTION,
    P_EXECUTION,
    P_MARKET_DATA,
    P_REQUEST,
    P_TIMER,
    P_VENUE,
    ClockError,
    EventQueue,
    SimClock,
)


def drain(queue: EventQueue) -> list:
    out = []
    while queue:
        ts, priority, _, payload = queue.pop()
        out.append((ts, priority, payload))
    return out


def test_events_come_out_in_timestamp_order():
    queue = EventQueue()
    queue.load([(30, P_MARKET_DATA, "c"), (10, P_MARKET_DATA, "a"), (20, P_MARKET_DATA, "b")])
    assert [p for _, _, p in drain(queue)] == ["a", "b", "c"]


def test_priority_fixes_the_causal_order_within_one_timestamp():
    queue = EventQueue()
    queue.load(
        [
            (5, P_TIMER, "timer"),
            (5, P_MARKET_DATA, "data"),
            (5, P_VENUE, "venue"),
            (5, P_CORPORATE_ACTION, "action"),
        ]
    )
    queue.push(5, P_EXECUTION, "execution")
    queue.push(5, P_REQUEST, "request")
    assert [p for _, _, p in drain(queue)] == [
        "action",
        "venue",
        "request",
        "execution",
        "data",
        "timer",
    ]


def test_equal_timestamp_and_priority_keeps_insertion_order():
    queue = EventQueue()
    queue.load([(1, P_MARKET_DATA, name) for name in "abc"])
    for name in "xyz":
        queue.push(1, P_MARKET_DATA, name)
    assert [p for _, _, p in drain(queue)] == list("abcxyz")


def test_pushed_events_merge_with_the_loaded_run():
    queue = EventQueue()
    queue.load([(t, P_MARKET_DATA, f"static{t}") for t in (10, 20, 30)])
    queue.push(25, P_TIMER, "dynamic25")
    queue.push(10, P_VENUE, "dynamic10")
    assert len(queue) == 5
    assert [p for _, _, p in drain(queue)] == [
        "dynamic10",
        "static10",
        "static20",
        "dynamic25",
        "static30",
    ]
    assert len(queue) == 0


def test_an_event_pushed_while_draining_is_ordered_among_what_remains():
    queue = EventQueue()
    queue.load([(1, P_MARKET_DATA, "first"), (1, P_MARKET_DATA, "second")])
    assert queue.pop()[3] == "first"
    queue.push(1, P_REQUEST, "request")  # earlier priority than the remaining data event
    assert [p for _, _, p in drain(queue)] == ["request", "second"]


def test_ordering_never_compares_payloads():
    class Opaque:  # defines no ordering; comparing two of these raises TypeError
        pass

    queue = EventQueue()
    queue.load([(1, P_MARKET_DATA, Opaque()) for _ in range(3)])
    for _ in range(3):
        queue.push(1, P_MARKET_DATA, Opaque())
    assert len(drain(queue)) == 6


def test_order_is_a_pure_function_of_the_inputs():
    rng = random.Random(3)
    items = [(rng.randrange(50), rng.randrange(6), i) for i in range(500)]
    pushes = [(rng.randrange(50), rng.randrange(6), 1_000 + i) for i in range(500)]

    def run() -> list:
        queue = EventQueue()
        queue.load(items)
        for ts, priority, payload in pushes:
            queue.push(ts, priority, payload)
        return drain(queue)

    first = run()
    assert first == run()
    assert [(ts, p) for ts, p, _ in first] == sorted((ts, p) for ts, p, _ in first)


def test_load_must_come_before_push_and_empty_pop_raises():
    queue = EventQueue()
    with pytest.raises(IndexError):
        queue.pop()
    queue.push(1, P_TIMER, "x")
    with pytest.raises(RuntimeError):
        queue.load([(1, P_TIMER, "y")])


def test_clock_only_moves_forward():
    clock = SimClock(100)
    clock.advance(100)
    clock.advance(150)
    assert clock.now == 150
    with pytest.raises(ClockError):
        clock.advance(149)
    assert clock.now == 150
