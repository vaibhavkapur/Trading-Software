from decimal import Decimal

import pytest

from tca.domain import Side
from tca.importer import ImportFormatError, parse_csv, parse_json, parse_rows
from tca.timeutil import parse_ts

ORDERS_HEADER = "order_id,symbol,side,qty,decision_ts,arrival_ts,broker,strategy\n"
GOOD_ORDER = "O1,XYZ,BUY,1000,2026-03-02T14:59:59Z,2026-03-02T15:00:00Z,B1,TWAP\n"
QUOTES_HEADER = "symbol,venue,bid,ask,ts,recv_ts\n"
FILLS_HEADER = "fill_id,order_id,child_id,venue,qty,price,fee,ts\n"
BARS_HEADER = "symbol,ts,interval_s,vwap,volume\n"


def codes(batch):
    return [(reject.row, reject.code) for reject in batch.rejects]


def test_valid_order_is_normalised():
    batch = parse_csv("orders", ORDERS_HEADER + GOOD_ORDER)
    assert batch.rows == 1 and not batch.rejects
    row, parsed = batch.records[0]
    assert row == 1
    assert parsed.side is Side.BUY  # case-insensitive
    assert parsed.qty == 1000
    assert parsed.arrival_ts == parse_ts("2026-03-02T15:00:00Z")
    assert parsed.arrival_ts - parsed.decision_ts == 1_000_000


def test_bad_rows_are_rejected_individually_and_good_rows_still_load():
    text = ORDERS_HEADER + "".join(
        [
            GOOD_ORDER,
            "O2,XYZ,hold,1000,2026-03-02T14:59:59Z,2026-03-02T15:00:00Z,B1,TWAP\n",
            "O3,XYZ,buy,0,2026-03-02T14:59:59Z,2026-03-02T15:00:00Z,B1,TWAP\n",
            "O4,XYZ,buy,10.5,2026-03-02T14:59:59Z,2026-03-02T15:00:00Z,B1,TWAP\n",
            "O5,XYZ,buy,100,2026-03-02T14:59:59,2026-03-02T15:00:00Z,B1,TWAP\n",
            "O6,XYZ,buy,100,2026-03-02T15:00:01Z,2026-03-02T15:00:00Z,B1,TWAP\n",
            ",XYZ,buy,100,2026-03-02T14:59:59Z,2026-03-02T15:00:00Z,B1,TWAP\n",
            "O8,XYZ,sell,100,2026-03-02T14:59:59Z,2026-03-02T15:00:00Z,B1,TWAP\n",
        ]
    )
    batch = parse_csv("orders", text)
    assert batch.rows == 8
    assert [record.order_id for _, record in batch.records] == ["O1", "O8"]
    assert codes(batch) == [
        (2, "BAD_SIDE"),
        (3, "OUT_OF_RANGE"),
        (4, "NOT_WHOLE"),
        (5, "BAD_TIMESTAMP"),
        (6, "DECISION_AFTER_ARRIVAL"),
        (7, "MISSING_FIELD"),
    ]


def test_quote_validation():
    text = QUOTES_HEADER + "".join(
        [
            "XYZ,V1,100.00,100.02,2026-03-02T15:00:00Z,2026-03-02T15:00:00.001Z\n",
            "XYZ,V1,100.00,100.00,2026-03-02T15:00:01Z,\n",  # locked is allowed, no recv_ts
            "XYZ,V1,100.03,100.02,2026-03-02T15:00:02Z,\n",  # crossed on one venue
            "XYZ,V1,0,100.02,2026-03-02T15:00:03Z,\n",
            "XYZ,V1,abc,100.02,2026-03-02T15:00:04Z,\n",
            "XYZ,V1,NaN,100.02,2026-03-02T15:00:05Z,\n",
        ]
    )
    batch = parse_csv("quotes", text)
    assert len(batch.records) == 2
    assert batch.records[0][1].recv_ts is not None and batch.records[1][1].recv_ts is None
    assert codes(batch) == [
        (3, "CROSSED_QUOTE"),
        (4, "NON_POSITIVE"),
        (5, "BAD_NUMBER"),
        (6, "BAD_NUMBER"),
    ]


def test_fill_fee_may_be_a_rebate_but_price_must_be_positive():
    text = FILLS_HEADER + "".join(
        [
            "F1,O1,C1,V1,100,100.05,-0.20,2026-03-02T15:00:05Z\n",
            "F2,O1,,V1,100,-1,0.30,2026-03-02T15:00:05Z\n",
        ]
    )
    batch = parse_csv("fills", text)
    assert batch.records[0][1].fee == Decimal("-0.20")
    assert codes(batch) == [(2, "NON_POSITIVE")]


def test_bars_must_align_to_their_interval():
    text = BARS_HEADER + "".join(
        [
            "XYZ,2026-03-02T15:00:00Z,60,100.04,12000\n",
            "XYZ,2026-03-02T15:00:30Z,60,100.04,12000\n",
            "XYZ,2026-03-02T15:01:00Z,60,100.04,-5\n",
            "XYZ,2026-03-02T15:02:00Z,60,100.04,0\n",
        ]
    )
    batch = parse_csv("bars", text)
    assert len(batch.records) == 2  # the zero-volume bar is valid
    assert codes(batch) == [(2, "BAR_MISALIGNED"), (3, "OUT_OF_RANGE")]


def test_optional_columns_may_be_absent_from_the_header():
    text = "symbol,venue,bid,ask,ts\nXYZ,V1,100.00,100.02,2026-03-02T15:00:00Z\n"
    assert parse_csv("quotes", text).records[0][1].recv_ts is None


def test_rows_with_the_wrong_number_of_fields_are_rejected():
    batch = parse_csv("orders", ORDERS_HEADER + "O1,XYZ,buy\n" + GOOD_ORDER.rstrip() + ",extra\n")
    assert codes(batch) == [(1, "MALFORMED_ROW"), (2, "MALFORMED_ROW")]


@pytest.mark.parametrize(
    ("dataset", "text", "message"),
    [
        ("trades", "a,b\n", "unknown dataset"),
        ("orders", "", "empty payload"),
        ("orders", "order_id,symbol\nO1,XYZ\n", "missing required columns"),
        ("quotes", "symbol,venue,bid,ask,ts,recieve_ts\n", "unknown columns: recieve_ts"),
        ("quotes", "symbol,venue,bid,ask,ts,ts\n", "duplicate column"),
    ],
)
def test_structural_problems_fail_the_whole_payload(dataset, text, message):
    with pytest.raises(ImportFormatError, match=message):
        parse_csv(dataset, text)


def test_header_only_csv_is_an_empty_batch():
    batch = parse_csv("orders", ORDERS_HEADER)
    assert batch.rows == 0 and not batch.records and not batch.rejects


def test_byte_order_mark_and_padded_header_are_tolerated():
    batch = parse_csv(
        "orders", "\ufeff" + ORDERS_HEADER.replace(",symbol", ", symbol") + GOOD_ORDER
    )
    assert len(batch.records) == 1


def test_json_prices_never_pass_through_binary_floats():
    body = (
        '{"rows": [{"fill_id": "F1", "order_id": "O1", "venue": "V1", "qty": 300,'
        ' "price": 100.10, "fee": 0.1, "ts": "2026-03-02T15:00:05Z"}]}'
    )
    record = parse_json("fills", body).records[0][1]
    assert record.price == Decimal("100.10") and str(record.price) == "100.10"
    assert record.fee == Decimal("0.1")
    assert record.qty == 300 and record.child_id is None


def test_json_accepts_a_bare_array_and_rejects_other_shapes():
    assert parse_json("orders", "[]").rows == 0
    with pytest.raises(ImportFormatError, match="not valid JSON"):
        parse_json("orders", "{not json")
    with pytest.raises(ImportFormatError, match="expected a JSON array"):
        parse_json("orders", '{"data": []}')


def test_json_rows_are_checked_for_shape_and_types():
    good = {
        "symbol": "XYZ",
        "venue": "V1",
        "bid": "100.00",
        "ask": "100.02",
        "ts": "2026-03-02T15:00:00Z",
    }
    batch = parse_rows("quotes", [good, "not an object", good | {"oops": 1}, good | {"bid": True}])
    assert len(batch.records) == 1
    assert codes(batch) == [(2, "MALFORMED_ROW"), (3, "MALFORMED_ROW"), (4, "BAD_NUMBER")]


def test_binary_float_inputs_use_their_shortest_repr():
    # Parquet double columns arrive as Python floats.
    row = {"symbol": "XYZ", "venue": "V1", "bid": 100.1, "ask": 100.2, "ts": "2026-03-02T15:00:00Z"}
    record = parse_rows("quotes", [row]).records[0][1]
    assert (record.bid, record.ask) == (Decimal("100.1"), Decimal("100.2"))
