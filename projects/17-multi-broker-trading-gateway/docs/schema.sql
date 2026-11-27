-- Multi-Broker Trading Gateway — PostgreSQL Production Schema (v1.0.0)
-- Matches the src/storage.Store contract with strict UTC timestamps, idempotency keys,
-- fill deduplication constraints, and immutable reconciliation audit records.

CREATE TABLE IF NOT EXISTS gateway_orders (
    order_id          TEXT PRIMARY KEY,
    client_order_id   TEXT NOT NULL UNIQUE,
    broker_id         TEXT NOT NULL,
    broker_order_id   TEXT,
    execution_mode    TEXT NOT NULL CHECK (execution_mode IN ('paper', 'sandbox', 'live')),
    instrument_id     TEXT NOT NULL,
    broker_symbol     TEXT NOT NULL,
    side              TEXT NOT NULL CHECK (side IN ('BUY', 'SELL')),
    order_type        TEXT NOT NULL CHECK (order_type IN ('MARKET', 'LIMIT', 'STOP_LIMIT', 'STOP')),
    time_in_force     TEXT NOT NULL CHECK (time_in_force IN ('DAY', 'GTC', 'IOC', 'FOK')),
    quantity          NUMERIC(20, 8) NOT NULL CHECK (quantity > 0),
    filled_quantity   NUMERIC(20, 8) NOT NULL DEFAULT 0 CHECK (filled_quantity >= 0),
    remaining_qty     NUMERIC(20, 8) NOT NULL CHECK (remaining_qty >= 0),
    limit_price       NUMERIC(20, 8),
    stop_price        NUMERIC(20, 8),
    avg_fill_price    NUMERIC(20, 8) NOT NULL DEFAULT 0,
    total_fees        NUMERIC(20, 8) NOT NULL DEFAULT 0,
    currency          TEXT NOT NULL,
    status            TEXT NOT NULL,
    reject_reason     TEXT,
    payload_hash      TEXT NOT NULL,
    reconciled        BOOLEAN NOT NULL DEFAULT FALSE,
    last_error_code   TEXT,
    created_at_utc    TIMESTAMPTZ NOT NULL,
    updated_at_utc    TIMESTAMPTZ NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_gateway_orders_broker_status
    ON gateway_orders (broker_id, status);

CREATE UNIQUE INDEX IF NOT EXISTS idx_gateway_orders_broker_oid
    ON gateway_orders (broker_id, broker_order_id)
    WHERE broker_order_id IS NOT NULL AND broker_order_id <> '';

CREATE TABLE IF NOT EXISTS gateway_fills (
    fill_id           TEXT PRIMARY KEY,
    event_id          TEXT,
    order_id          TEXT NOT NULL REFERENCES gateway_orders(order_id),
    client_order_id   TEXT NOT NULL,
    broker_id         TEXT NOT NULL,
    broker_order_id   TEXT NOT NULL,
    broker_fill_id    TEXT NOT NULL,
    instrument_id     TEXT NOT NULL,
    broker_symbol     TEXT NOT NULL,
    side              TEXT NOT NULL,
    sequence_no       BIGINT NOT NULL,
    quantity          NUMERIC(20, 8) NOT NULL CHECK (quantity > 0),
    price             NUMERIC(20, 8) NOT NULL CHECK (price > 0),
    fee               NUMERIC(20, 8) NOT NULL DEFAULT 0,
    currency          TEXT NOT NULL,
    executed_at_utc   TIMESTAMPTZ NOT NULL,
    received_at_utc   TIMESTAMPTZ NOT NULL,
    CONSTRAINT uq_broker_fill UNIQUE (broker_id, broker_fill_id)
);

CREATE INDEX IF NOT EXISTS idx_gateway_fills_order_seq
    ON gateway_fills (order_id, sequence_no, executed_at_utc);

CREATE TABLE IF NOT EXISTS webhook_dedup (
    broker_id         TEXT NOT NULL,
    event_id          TEXT NOT NULL,
    processed_at_utc  TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    PRIMARY KEY (broker_id, event_id)
);

CREATE TABLE IF NOT EXISTS broker_replay_cursors (
    broker_id         TEXT PRIMARY KEY,
    cursor_position   BIGINT NOT NULL DEFAULT 0,
    updated_at_utc    TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE TABLE IF NOT EXISTS reconciliation_runs (
    run_id              TEXT PRIMARY KEY,
    started_at_utc      TIMESTAMPTZ NOT NULL,
    completed_at_utc    TIMESTAMPTZ NOT NULL,
    brokers_checked     JSONB NOT NULL,
    orders_checked      INTEGER NOT NULL,
    unresolved_count    INTEGER NOT NULL,
    triggered_fail_safe BOOLEAN NOT NULL,
    findings_json       JSONB NOT NULL
);

CREATE TABLE IF NOT EXISTS operator_audit_log (
    audit_id          BIGSERIAL PRIMARY KEY,
    action            TEXT NOT NULL, -- HALT, RESUME, POLICY_CHANGE, CREDENTIAL_ROTATE
    operator_id       TEXT NOT NULL,
    reason            TEXT NOT NULL,
    created_at_utc    TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
