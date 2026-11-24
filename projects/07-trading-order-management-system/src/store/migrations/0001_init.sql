-- OMS schema, version 1.
--
-- Conventions
--   * Money is BIGINT micros (1e-6). Quantities are whole units.
--   * Every timestamp is timestamptz, written in UTC. Columns that hold the
--     broker's clock are named for it (event_time, executed_at, occurred_at);
--     recorded_at / received_at are always this system's receive time.
--   * order_events is the source of truth. orders, executions and positions
--     are projections of it, written in the same transaction, and
--     `omsd verify` proves they still match.

CREATE TABLE orders (
    id                     TEXT PRIMARY KEY,
    account_id             TEXT NOT NULL,
    client_order_id        TEXT NOT NULL,
    symbol                 TEXT NOT NULL,
    side                   TEXT NOT NULL CHECK (side IN ('BUY', 'SELL')),
    type                   TEXT NOT NULL CHECK (type IN ('LIMIT', 'MARKET')),
    time_in_force          TEXT NOT NULL CHECK (time_in_force IN ('DAY', 'GTC', 'IOC')),
    qty                    BIGINT NOT NULL CHECK (qty > 0),
    limit_price_micros     BIGINT CHECK (limit_price_micros > 0),
    state                  TEXT NOT NULL CHECK (state IN (
                               'NEW', 'VALIDATED', 'SUBMITTING', 'ACKNOWLEDGED', 'PARTIALLY_FILLED',
                               'FILLED', 'CANCELLED', 'REJECTED', 'EXPIRED')),
    version                BIGINT NOT NULL CHECK (version > 0),
    filled_qty             BIGINT NOT NULL DEFAULT 0 CHECK (filled_qty >= 0),
    filled_notional_micros BIGINT NOT NULL DEFAULT 0,
    fees_micros            BIGINT NOT NULL DEFAULT 0,
    overfill_accepted      BOOLEAN NOT NULL DEFAULT FALSE,
    broker_order_id        TEXT NOT NULL DEFAULT '',
    pending_cancel         BOOLEAN NOT NULL DEFAULT FALSE,
    pending_replace        JSONB,
    reject_reason          TEXT NOT NULL DEFAULT '',
    created_by             TEXT NOT NULL,
    created_at             TIMESTAMPTZ NOT NULL,
    updated_at             TIMESTAMPTZ NOT NULL,
    submitted_at           TIMESTAMPTZ,
    acknowledged_at        TIMESTAMPTZ,
    closed_at              TIMESTAMPTZ,

    -- A client order id is stable and unique per account.
    UNIQUE (account_id, client_order_id),
    -- Defence in depth for the aggregate's own rule: filled quantity cannot
    -- exceed order quantity unless an operator explicitly accepted an overfill.
    CONSTRAINT no_silent_overfill CHECK (filled_qty <= qty OR overfill_accepted),
    CONSTRAINT limit_price_matches_type CHECK ((type = 'LIMIT') = (limit_price_micros IS NOT NULL))
);

CREATE INDEX orders_account_created ON orders (account_id, created_at DESC, id);
CREATE INDEX orders_open ON orders (updated_at)
    WHERE state IN ('NEW', 'VALIDATED', 'SUBMITTING', 'ACKNOWLEDGED', 'PARTIALLY_FILLED');

CREATE TABLE order_events (
    seq          BIGSERIAL PRIMARY KEY,
    order_id     TEXT NOT NULL REFERENCES orders (id),
    version      BIGINT NOT NULL CHECK (version > 0),
    type         TEXT NOT NULL,
    source       TEXT NOT NULL CHECK (source IN ('API', 'OUTBOX', 'BROKER', 'RECONCILER', 'OPERATOR')),
    actor        TEXT NOT NULL DEFAULT '',
    causation_id TEXT NOT NULL DEFAULT '',
    state_after  TEXT NOT NULL,
    payload      JSONB NOT NULL,
    occurred_at  TIMESTAMPTZ NOT NULL, -- event time (the broker's clock for broker events)
    recorded_at  TIMESTAMPTZ NOT NULL, -- local receive time

    -- One writer per version: a concurrent writer that lost the race fails here.
    UNIQUE (order_id, version)
);

CREATE TABLE executions (
    id                 BIGSERIAL PRIMARY KEY,
    order_id           TEXT NOT NULL REFERENCES orders (id),
    event_seq          BIGINT NOT NULL REFERENCES order_events (seq),
    broker_exec_id     TEXT NOT NULL,
    kind               TEXT NOT NULL CHECK (kind IN ('FILL', 'CORRECT', 'BUST')),
    ref_broker_exec_id TEXT,
    qty                BIGINT NOT NULL CHECK (qty >= 0),
    price_micros       BIGINT NOT NULL CHECK (price_micros >= 0),
    fee_micros         BIGINT NOT NULL CHECK (fee_micros >= 0),
    -- What this record changed in the order's totals when it was booked.
    delta_qty             BIGINT NOT NULL,
    delta_notional_micros BIGINT NOT NULL,
    delta_fee_micros      BIGINT NOT NULL,
    accepted_overfill  BOOLEAN NOT NULL DEFAULT FALSE,
    source             TEXT NOT NULL,
    executed_at        TIMESTAMPTZ NOT NULL, -- broker execution time
    recorded_at        TIMESTAMPTZ NOT NULL, -- local receive time

    -- Fill deduplication: a broker execution id is booked at most once.
    UNIQUE (broker_exec_id),
    -- An execution can be corrected or busted at most once; a later correction
    -- must reference the correction, forming a chain.
    UNIQUE (ref_broker_exec_id)
);

CREATE INDEX executions_order ON executions (order_id, id);

-- order_events and executions are append-only. Trades are never edited or
-- deleted to make a discrepancy go away; a correction is a new row.
CREATE FUNCTION oms_forbid_mutation() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    RAISE EXCEPTION '% on % is not allowed: the table is append-only', TG_OP, TG_TABLE_NAME
        USING ERRCODE = 'integrity_constraint_violation';
END $$;

CREATE TRIGGER order_events_append_only BEFORE UPDATE OR DELETE ON order_events
    FOR EACH ROW EXECUTE FUNCTION oms_forbid_mutation();
CREATE TRIGGER order_events_no_truncate BEFORE TRUNCATE ON order_events
    FOR EACH STATEMENT EXECUTE FUNCTION oms_forbid_mutation();
CREATE TRIGGER executions_append_only BEFORE UPDATE OR DELETE ON executions
    FOR EACH ROW EXECUTE FUNCTION oms_forbid_mutation();
CREATE TRIGGER executions_no_truncate BEFORE TRUNCATE ON executions
    FOR EACH STATEMENT EXECUTE FUNCTION oms_forbid_mutation();

CREATE TABLE positions (
    account_id      TEXT NOT NULL,
    symbol          TEXT NOT NULL,
    net_qty         BIGINT NOT NULL,
    -- Cash paid (negative) or received (positive) for executions, before fees.
    net_cash_micros BIGINT NOT NULL,
    fees_micros     BIGINT NOT NULL,
    updated_at      TIMESTAMPTZ NOT NULL,
    PRIMARY KEY (account_id, symbol)
);

-- Transactional outbox: a row is written in the same transaction as the
-- order events that require a broker call, and delivered afterwards.
CREATE TABLE outbox (
    id           BIGSERIAL PRIMARY KEY,
    aggregate_id TEXT NOT NULL REFERENCES orders (id),
    event_type   TEXT NOT NULL CHECK (event_type IN ('SUBMIT_ORDER', 'CANCEL_ORDER', 'REPLACE_ORDER')),
    status       TEXT NOT NULL DEFAULT 'PENDING'
                 CHECK (status IN ('PENDING', 'DELIVERED', 'SKIPPED', 'REJECTED', 'DEAD')),
    attempts     INT NOT NULL DEFAULT 0,
    available_at TIMESTAMPTZ NOT NULL,
    locked_until TIMESTAMPTZ,
    locked_by    TEXT NOT NULL DEFAULT '',
    last_error   TEXT NOT NULL DEFAULT '',
    created_at   TIMESTAMPTZ NOT NULL,
    delivered_at TIMESTAMPTZ
);

CREATE INDEX outbox_pending ON outbox (aggregate_id, id) WHERE status = 'PENDING';

CREATE TABLE idempotency_keys (
    principal       TEXT NOT NULL,
    key             TEXT NOT NULL,
    operation       TEXT NOT NULL,
    request_hash    TEXT NOT NULL,
    order_id        TEXT,
    response_status INT NOT NULL DEFAULT 0, -- 0 while the first request is still in its transaction
    response_body   BYTEA,                  -- the first response, replayed byte for byte
    created_at      TIMESTAMPTZ NOT NULL,
    PRIMARY KEY (principal, key)
);

-- Inbox of everything the broker has told us, applied or not.
CREATE TABLE broker_events (
    id               BIGSERIAL PRIMARY KEY,
    event_id         TEXT NOT NULL UNIQUE,
    kind             TEXT NOT NULL,
    order_id         TEXT NOT NULL DEFAULT '',
    source           TEXT NOT NULL,
    payload          JSONB NOT NULL,
    outcome          TEXT NOT NULL DEFAULT 'PROCESSING',
    detail           TEXT NOT NULL DEFAULT '',
    deliveries       INT NOT NULL DEFAULT 1,
    event_time       TIMESTAMPTZ,          -- the broker's clock
    received_at      TIMESTAMPTZ NOT NULL, -- first local receipt
    last_received_at TIMESTAMPTZ NOT NULL  -- latest redelivery
);

CREATE INDEX broker_events_order ON broker_events (order_id, id);

CREATE TABLE reconciliation_runs (
    id                 TEXT PRIMARY KEY,
    trigger            TEXT NOT NULL,
    status             TEXT NOT NULL CHECK (status IN ('RUNNING', 'COMPLETED', 'FAILED')),
    error              TEXT NOT NULL DEFAULT '',
    started_at         TIMESTAMPTZ NOT NULL,
    finished_at        TIMESTAMPTZ,
    broker_as_of       TIMESTAMPTZ, -- the broker's clock when it took the snapshot
    orders_checked     INT NOT NULL DEFAULT 0,
    executions_checked INT NOT NULL DEFAULT 0,
    drift_found        INT NOT NULL DEFAULT 0,
    auto_repaired      INT NOT NULL DEFAULT 0,
    issues_opened      INT NOT NULL DEFAULT 0,
    issues_cleared     INT NOT NULL DEFAULT 0
);

-- The exception queue. An issue is opened by a reconciliation run or by the
-- ingest path when a broker report cannot be applied.
CREATE TABLE reconciliation_issues (
    id           TEXT PRIMARY KEY,
    run_id       TEXT REFERENCES reconciliation_runs (id),
    order_id     TEXT NOT NULL DEFAULT '',
    kind         TEXT NOT NULL,
    severity     TEXT NOT NULL CHECK (severity IN ('LOW', 'MEDIUM', 'HIGH', 'CRITICAL')),
    status       TEXT NOT NULL CHECK (status IN ('OPEN', 'RESOLVED')),
    fingerprint  TEXT NOT NULL,
    details      JSONB NOT NULL,
    occurrences  INT NOT NULL DEFAULT 1,
    detected_at  TIMESTAMPTZ NOT NULL,
    last_seen_at TIMESTAMPTZ NOT NULL,
    last_seen_run TEXT NOT NULL DEFAULT '',
    resolved_at  TIMESTAMPTZ,
    resolved_by  TEXT NOT NULL DEFAULT '',
    resolution   TEXT NOT NULL DEFAULT '',
    note         TEXT NOT NULL DEFAULT ''
);

-- The same discrepancy seen again updates the open issue instead of opening
-- another.
CREATE UNIQUE INDEX reconciliation_issues_open ON reconciliation_issues (fingerprint) WHERE status = 'OPEN';
CREATE INDEX reconciliation_issues_status ON reconciliation_issues (status, detected_at);
