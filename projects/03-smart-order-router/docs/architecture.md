# Architecture

Everything is **simulated / paper**. There is no connectivity to any real venue, broker or market-data feed.

## Shape

One Go process, standard library only. The five services named in the README are packages with narrow
interfaces, wired together by `engine` on a single-threaded, simulated-time event loop.

```text
            POST /parents                        POST /sim/venues/{v}/faults
                 |                                          |
           +-----v------+                                   |
           |  api       |  mutex + wall-clock pacing        |
           +-----+------+  (live engine + baseline shadow)  |
                 |                                          |
+----------------v------------------------------------------v-----------------+
| engine  (event heap ordered by simulated time, then sequence number)        |
|                                                                             |
|  slice timer -> risk.CollarPx -> marketview.Snapshot --+                    |
|                                                        v                    |
|                                        planner.Plan(Input) -> RouteDecision |
|                                                        |                    |
|                risk.CheckChild -> oms.NewChild (reserve qty) -> send        |
|                                                        |                    |
|      +-------------------------+-----------------------+----+               |
|      v                         v                            v               |
|  venuesim ALPHA           venuesim BRAVO              venuesim CHARLIE      |
|      |   quotes (latency)      |                            |               |
|      +--> marketview <---------+----------------------------+               |
|      |   exec reports (latency, may be lost / duplicated / late)            |
|      +--> oms.ApplyReport -> marketview.RecordOutcome -> replan residual    |
|                                                                             |
|  tca.Build(parent, children, fills, market VWAP)      event log (JSONL)     |
+-----------------------------------------------------------------------------+
```

| Package | README service | Responsibility |
|---|---|---|
| `src/domain` | — | Canonical schemas: `ParentOrder`, `ChildOrder`, `VenueQuote`, `Fill`, `RouteDecision`. |
| `src/marketview` | `market-view` | Normalized quotes, venue status (`OK/NO_QUOTE/STALE/SKEWED/DOWN/TRIPPED`), circuit breaker, latency and fill-ratio estimates, immutable snapshots. |
| `src/planner` | `route-planner` | Pure function `Plan(Input) -> RouteDecision` with an explanation trace. |
| `src/risk` | (pre-trade risk) | Parent and child limits, price collar, kill switch. Independent of routing. |
| `src/oms` | `child-oms` | Parent/child state machines, quantity reservation, idempotent report handling. |
| `src/venuesim` | `venue-sim` | Seeded books, IOC matching, latency, rejects, and injectable faults. |
| `src/tca` | `tca` | Arrival price, market VWAP, implementation shortfall, effective spread, venue stats, baseline comparison. |
| `src/engine` | — | Scheduling (TWAP / LIQUIDITY), replanning, timeouts, reconciliation, event log. |
| `src/scenario`, `src/replay`, `src/api`, `src/cmd/sor` | — | Scripted runs, decision replay, HTTP API, CLI. |

## Units

Integers only on the execution path (`domain`, `planner`, `risk`, `oms`, `venuesim`, `engine`).

- Price: ticks (1 tick = 0.01). Money (fees, costs): mills (1 tick = 100 mills). Quantity: shares in lot multiples.
- Time: Unix milliseconds UTC. Venue event time (`asof_ms`, `venue_ms`) is distinct from local receive time (`recv_ms`).
- Floats appear only in `tca` (derived bps and averages, computed from exact integer sums) and in the simulator's
  fault probabilities.

## Order flow

1. **Accept.** `SubmitParent` takes the consolidated mid from the market view as the arrival price and runs
   `risk.CheckParent`. Rejected parents are stored with a reason code for audit.
2. **Schedule.** `TWAP` slices every `interval_seconds` (default 10s). It is a catch-up schedule: slice *i* targets
   the scheduled cumulative quantity minus what is filled or reserved, so shortfalls roll forward. `LIQUIDITY`
   fires every second and takes a participation-capped share of top-of-book on every healthy venue.
3. **Plan.** `planner.Plan` ranks every (venue, level) by all-in cost in mills per share:
   `±price + taker fee + latency penalty + fill-quality penalty`, takes greedily subject to the participation cap,
   per-venue child cap, lot size and the risk price collar, and emits one IOC limit child per venue. Ties break on
   size then venue name, so the result is deterministic.
4. **Send.** Each child passes `risk.CheckChild`, reserves its quantity in the OMS, and travels to the venue with
   that venue's latency. The venue matches it IOC against its book *as of arrival* — so books that moved give
   partial or zero fills.
5. **Report.** One terminal report per child returns after the venue latency. The OMS applies it idempotently.
   Outcome and round-trip time feed venue health.
6. **Replan.** When a whole wave has resolved with residual quantity, the engine re-routes it after
   `replan_delay_ms`, at most `max_replans_per_slice` times, excluding venues that rejected or timed out in that
   slice. Anything left rolls into the next TWAP slice.
7. **Finish.** `FILLED` on the last fill; `EXPIRED` at the deadline once nothing is in flight; `CANCELLED` by the
   kill switch or a cancel request.

## Invariants (all asserted in tests)

- `parent.filled + parent.reserved <= parent.target` at all times. A child reserves its full quantity on creation;
  the reservation is released only by a fill or a *confirmed* terminal report.
- A child whose report did not arrive within `child_timeout_ms` becomes `TIMED_OUT` and **keeps its reservation**.
  Its true outcome arrives late, or is recovered by an order-status reconciliation when the venue link returns.
- A report that would overfill a child or parent is refused atomically and engages the kill switch.
- Fill ids are de-duplicated per venue; duplicate reports are counted and ignored.
- The simulated market (price path, generated depth, tape) is independent of our order flow; orders only deplete
  displayed depth temporarily. Router and baseline therefore see the identical market.

## Failure handling

| Condition | Detection | Response |
|---|---|---|
| Link down | Session state (`DOWN`) | Venue excluded immediately; sends fail fast locally; reports in transit are lost and reconciled on reconnect. |
| Market data silent | `now - asof_ms > stale_ms` (`STALE`) | Venue excluded until fresh quotes arrive. |
| Venue clock ahead | `asof_ms > recv_ms + skew_tolerance_ms` (`SKEWED`) | Venue excluded. A clock that is behind looks `STALE`. |
| Out-of-order / duplicate quotes | Venue sequence number | Dropped. |
| Rejects / timeouts | Consecutive failures >= `breaker_threshold` (`TRIPPED`) | Excluded for `breaker_cooldown_ms`, then half-open: one failure re-trips, one success closes. |
| Slow venue | Round-trip EWMA; timeouts | Latency penalty in the cost model; timeouts feed the breaker. |
| Duplicate execution reports | Terminal child / seen fill id | Ignored. |
| No healthy venue | No consolidated mid | No routing; new parents are rejected `NO_MARKET_DATA`. |

## Replay

The engine appends every decision and order event to a JSON-lines log. A `route_decision` event contains the
complete `planner.Input` — the frozen snapshot, policy, collar, exclusions — and the decision made.
`sor replay` re-runs `planner.Plan` on each recorded input and requires byte-identical decisions. Separately, a
whole run is reproducible: the same scenario file yields a byte-identical event log.

## Known limitations

- One symbol per engine. Children are IOC marketable limits only (no passive posting, no maker rebates).
- No permanent market impact: our flow depletes displayed depth, which refills within a few seconds.
- State is in memory. A restart loses orders; there is no persistence or recovery yet.
- The baseline shadow in the API server doubles simulation work per request.
