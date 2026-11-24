-- Storage for the optional ClickHouse sink (see src/stream-api/src/clickhouse.rs).
-- Timestamps arrive as microseconds since the Unix epoch (UTC) and are kept as
-- Int64; the DateTime64 columns are derived so the insert format stays trivial.
-- `event_ts` is venue time, `recv_ts` is this service's local receive time:
-- they come from different clocks and must not be subtracted to claim latency.

CREATE DATABASE IF NOT EXISTS lob;

CREATE TABLE IF NOT EXISTS lob.book_deltas
(
    event_ts    Int64,
    symbol      LowCardinality(String),
    seq         UInt64,
    side        Enum8('BID' = 1, 'ASK' = 2),
    price_ticks Int64,
    quantity    UInt64,
    action      Enum8('SET' = 1, 'DELETE' = 2),
    event_time  DateTime64(6, 'UTC') MATERIALIZED fromUnixTimestamp64Micro(event_ts)
)
ENGINE = MergeTree
PARTITION BY toDate(event_time)
ORDER BY (symbol, seq);

CREATE TABLE IF NOT EXISTS lob.book_metrics
(
    recv_ts         Int64,
    event_ts        Int64,
    symbol          LowCardinality(String),
    seq             UInt64,
    spread_ticks    Nullable(Int64),
    mid_ticks_x2    Nullable(Int64),
    microprice      Nullable(Float64),
    imbalance       Nullable(Float64),
    depth_imbalance Nullable(Float64),
    bid_depth       UInt64,
    ask_depth       UInt64,
    ofi             Int64,
    recv_time       DateTime64(6, 'UTC') MATERIALIZED fromUnixTimestamp64Micro(recv_ts)
)
ENGINE = MergeTree
PARTITION BY toDate(recv_time)
ORDER BY (symbol, recv_ts);

CREATE TABLE IF NOT EXISTS lob.feed_events
(
    recv_ts   Int64,
    symbol    LowCardinality(String),
    kind      LowCardinality(String),
    seq       Nullable(UInt64),
    value_ms  Nullable(Int64),
    detail    String,
    recv_time DateTime64(6, 'UTC') MATERIALIZED fromUnixTimestamp64Micro(recv_ts)
)
ENGINE = MergeTree
PARTITION BY toDate(recv_time)
ORDER BY (recv_ts, symbol);
