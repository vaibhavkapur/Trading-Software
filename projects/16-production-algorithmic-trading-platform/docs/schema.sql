-- PostgreSQL-compatible relational schema for Production Algorithmic Trading Platform
-- Mirrors src/trading_platform/storage/sqlite_store.py with explicit NUMERIC precision and TIMESTAMPTZ.

CREATE TABLE IF NOT EXISTS orders (
    client_order_id      TEXT PRIMARY KEY,
    idempotency_key      TEXT UNIQUE NOT NULL,
    payload_hash         TEXT NOT NULL,
    broker_order_id      TEXT,
    account_id           TEXT NOT NULL,
    strategy_id          TEXT NOT NULL,
    signal_id            TEXT,
    symbol               TEXT NOT NULL,
    side                 TEXT NOT NULL CHECK (side IN ('BUY', 'SELL')),
    order_type           TEXT NOT NULL CHECK (order_type IN ('MARKET', 'LIMIT')),
    time_in_force        TEXT NOT NULL CHECK (time_in_force IN ('DAY', 'IOC', 'GTC')),
    quantity             NUMERIC(20, 4) NOT NULL CHECK (quantity > 0),
    filled_quantity      NUMERIC(20, 4) NOT NULL DEFAULT 0,
    remaining_quantity   NUMERIC(20, 4) NOT NULL,
    limit_price          NUMERIC(20, 4),
    reference_price      NUMERIC(20, 4) NOT NULL,
    average_fill_price   NUMERIC(20, 4),
    currency             CHAR(3) NOT NULL DEFAULT 'USD',
    state                TEXT NOT NULL,
    execution_mode       TEXT NOT NULL,
    reject_reason        TEXT,
    risk_check_id        TEXT,
    created_at_utc       TIMESTAMPTZ NOT NULL,
    updated_at_utc       TIMESTAMPTZ NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_orders_state ON orders(state);
CREATE INDEX IF NOT EXISTS idx_orders_symbol ON orders(symbol);

CREATE TABLE IF NOT EXISTS order_transitions (
    transition_id        TEXT PRIMARY KEY,
    client_order_id      TEXT NOT NULL REFERENCES orders(client_order_id),
    from_state           TEXT,
    to_state             TEXT NOT NULL,
    reason               TEXT NOT NULL,
    actor                TEXT NOT NULL,
    occurred_at_utc      TIMESTAMPTZ NOT NULL,
    metadata_json        JSONB NOT NULL DEFAULT '{}'::jsonb
);

CREATE TABLE IF NOT EXISTS fills (
    fill_id              TEXT PRIMARY KEY,
    client_order_id      TEXT NOT NULL REFERENCES orders(client_order_id),
    broker_order_id      TEXT NOT NULL,
    account_id           TEXT NOT NULL,
    strategy_id          TEXT NOT NULL,
    symbol               TEXT NOT NULL,
    side                 TEXT NOT NULL CHECK (side IN ('BUY', 'SELL')),
    quantity             NUMERIC(20, 4) NOT NULL CHECK (quantity > 0),
    price                NUMERIC(20, 4) NOT NULL CHECK (price > 0),
    commission           NUMERIC(20, 4) NOT NULL DEFAULT 0,
    currency             CHAR(3) NOT NULL DEFAULT 'USD',
    venue                TEXT NOT NULL,
    executed_at_utc      TIMESTAMPTZ NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_fills_order ON fills(client_order_id);

CREATE TABLE IF NOT EXISTS risk_reservations (
    reservation_id       TEXT PRIMARY KEY,
    client_order_id      TEXT UNIQUE NOT NULL,
    account_id           TEXT NOT NULL,
    strategy_id          TEXT NOT NULL,
    symbol               TEXT NOT NULL,
    side                 TEXT NOT NULL,
    reserved_quantity    NUMERIC(20, 4) NOT NULL,
    reserved_notional    NUMERIC(20, 4) NOT NULL,
    currency             CHAR(3) NOT NULL DEFAULT 'USD',
    active               BOOLEAN NOT NULL,
    retained_reason      TEXT,
    created_at_utc       TIMESTAMPTZ NOT NULL,
    updated_at_utc       TIMESTAMPTZ NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_reservations_active ON risk_reservations(active);

CREATE TABLE IF NOT EXISTS kill_switches (
    scope                TEXT NOT NULL CHECK (scope IN ('GLOBAL', 'STRATEGY', 'SYMBOL')),
    target               TEXT NOT NULL,
    engaged              BOOLEAN NOT NULL,
    reason               TEXT NOT NULL,
    actor                TEXT NOT NULL,
    actor_role           TEXT NOT NULL,
    requires_dual_release BOOLEAN NOT NULL DEFAULT FALSE,
    updated_at_utc       TIMESTAMPTZ NOT NULL,
    PRIMARY KEY (scope, target)
);

CREATE TABLE IF NOT EXISTS ledger_entries (
    entry_id             TEXT PRIMARY KEY,
    idempotency_key      TEXT UNIQUE NOT NULL,
    account_id           TEXT NOT NULL,
    entry_type           TEXT NOT NULL,
    reference_id         TEXT NOT NULL,
    description          TEXT NOT NULL,
    posted_at_utc        TIMESTAMPTZ NOT NULL
);

CREATE TABLE IF NOT EXISTS ledger_lines (
    id                   BIGSERIAL PRIMARY KEY,
    entry_id             TEXT NOT NULL REFERENCES ledger_entries(entry_id),
    account_code         TEXT NOT NULL,
    currency             CHAR(3) NOT NULL DEFAULT 'USD',
    debit                NUMERIC(20, 4) NOT NULL DEFAULT 0,
    credit               NUMERIC(20, 4) NOT NULL DEFAULT 0,
    symbol               TEXT,
    memo                 TEXT NOT NULL DEFAULT ''
);

CREATE TABLE IF NOT EXISTS positions (
    account_id           TEXT NOT NULL,
    symbol               TEXT NOT NULL,
    currency             CHAR(3) NOT NULL DEFAULT 'USD',
    quantity             NUMERIC(20, 4) NOT NULL DEFAULT 0,
    average_cost         NUMERIC(20, 4) NOT NULL DEFAULT 0,
    realized_pnl         NUMERIC(20, 4) NOT NULL DEFAULT 0,
    total_commissions    NUMERIC(20, 4) NOT NULL DEFAULT 0,
    updated_at_utc       TIMESTAMPTZ NOT NULL,
    PRIMARY KEY (account_id, symbol)
);

CREATE TABLE IF NOT EXISTS reconciliation_reports (
    reconciliation_id    TEXT PRIMARY KEY,
    account_id           TEXT NOT NULL,
    run_at_utc           TIMESTAMPTZ NOT NULL,
    matched              BOOLEAN NOT NULL,
    report_json          JSONB NOT NULL
);

CREATE TABLE IF NOT EXISTS shadow_decisions (
    shadow_id            TEXT PRIMARY KEY,
    tick_id              TEXT NOT NULL,
    strategy_id          TEXT NOT NULL,
    symbol               TEXT NOT NULL,
    record_json          JSONB NOT NULL,
    recorded_at_utc      TIMESTAMPTZ NOT NULL
);

CREATE TABLE IF NOT EXISTS audit_events (
    sequence_no          BIGSERIAL PRIMARY KEY,
    event_id             TEXT UNIQUE NOT NULL,
    event_type           TEXT NOT NULL,
    actor                TEXT NOT NULL,
    actor_role           TEXT NOT NULL,
    entity_type          TEXT NOT NULL,
    entity_id            TEXT NOT NULL,
    payload_json         JSONB NOT NULL,
    occurred_at_utc      TIMESTAMPTZ NOT NULL,
    prev_hash            CHAR(64) NOT NULL,
    event_hash           CHAR(64) NOT NULL
);
