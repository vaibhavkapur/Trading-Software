# ADR-003: Versioned Rollout, Staleness Rejection, and Safe Failure Controls

- **Status**: Accepted
- **Milestone**: M4 — Hardening and delivery

## Context

In financial inference, silent numerical degradation, stale market features, look-ahead timestamps, mid-flight reload corruption, or unbounded queueing under load can cause severe downstream strategy errors.

## Decision

1. **Atomic reload with pre-activation gates**: `InferenceRouter.load_version` verifies SHA-256 integrity, builds the runtime adapter, and executes golden-vector parity checks *before* mutating the live routing map. Any failure leaves the currently active version serving uninterrupted.
2. **Deterministic canary & rollback**: Canary traffic splits use SHA-256 hashing over `request_id` for reproducible routing. Previous `RoutingPolicy` states are pushed onto a rollback stack so `rollback()` restores the prior version atomically.
3. **Staleness & point-in-time hygiene**: Requests must provide explicit UTC `event_ts_utc` and `available_ts_utc`. Stale features (`age > max_feature_age_ms`) and future/look-ahead timestamps (`available_ts_utc > inference_ts_utc + max_clock_skew_ms`) fail closed with `STALE_FEATURE` / `FUTURE_TIMESTAMP`.
4. **Overload shedding**: A non-blocking `max_in_flight` concurrency gate immediately rejects excess load with `OVERLOAD_REJECTED` rather than building unbounded latency queues.
5. **Execution disabled by default**: `order_execution_enabled=False` is stamped on every `InferenceResponse` and guarded by `assert_order_execution_allowed()`.

## Consequences

- **Positive**: Every failure mode is explicit, observable via `metrics_snapshot()`, and fail-closed.
- **Trade-off**: Callers must handle structured `EngineError` rejections cleanly and implement downstream safe-flat/no-trade behavior when inference rejects a tick.
