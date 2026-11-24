# 0004. Timestamps outside (1970, 2100) are rejected, and expiry uses the 64-bit column

Status: accepted

## Context

While testing the ClickHouse adapter against a real server (26.8.45.6), a row
whose `event_ts` was in the year 2107 was accepted with HTTP 200 and was then
not in the table. No error was reported.

The schema at that point expired rows with
`TTL toDateTime(event_ts) + INTERVAL n DAY`. `toDateTime` is a 32-bit type
that ends in February 2106. In the recorded reproduction
([../evidence/clickhouse-timestamp-range.txt](../evidence/clickhouse-timestamp-range.txt),
table A) the year-2107 timestamp converts to 1970-11-24 and the year-2150 one
to 2014-04-24; both rows are then deleted as long expired, either at insert
or at the next merge. The partition key `toDate(event_ts)` is a 16-bit type
with its own limit: the year-2150 row is filed under partition 1970-12-26, and
a 1969 timestamp converts to 2149-06-06 (section C of the reproduction).

A market data store that acknowledges a write and then discards it violates
the one property this project is about. The in-memory store used by the
hermetic tests accepted the same rows and kept them, so those tests could not
have shown the problem. It was found only because the same contract suite
runs against the real server.

## Decision

Two changes, either of which alone would have prevented the loss.

1. **Expire on the stored 64-bit timestamp**: `TTL event_ts + INTERVAL n DAY`
   (and `recv_ts` for gaps). Table B in the reproduction: the year-2107 row
   survives a merge, and rows genuinely older than the retention are still
   removed. `TestClickHouseExpiryDoesNotWrap` writes around the adapter to
   check this on the real tables.
2. **Accept only timestamps strictly between 1970-01-01 and 2100-01-01 UTC**
   (`domain.MinTime`, `domain.MaxTime`).
   - The normalizer rejects events outside the range to `md.rejects` with
     reason `EVENT_TIME_OUT_OF_RANGE`, where the original payload can be
     reported.
   - Both store adapters refuse any batch containing such a row, as a
     permanent error, before writing anything. This is the backstop for
     writers that bypass the normalizer. It is part of the shared store
     contract test, so the in-memory store behaves as ClickHouse must.

The range check stays even with the 64-bit TTL because the daily partition key
still wraps outside 1970–2149 (table B files the year-2150 row under 1970),
and because a venue timestamp eighty years in the future is a broken clock: a
row that would never expire and that no realistic query range would return.

Retention is capped at five years (`storage.MaxRetention`) so that timestamp
plus retention stays far from any representable limit; longer history belongs
in the Parquet export.

## Alternatives considered

- **Clamp out-of-range timestamps.** That would rewrite the provider's event
  time, which the platform never does.
- **Partition by `toDate32`.** Would widen the partition range to 1900–2299.
  Not done: the range check already keeps every stored row inside what `Date`
  represents, and the accepted range would not change.
- **Drop the TTL and expire with a scheduled job.** Replaces a storage-engine
  guarantee with an operational one.

## Consequences

- A provider with a broken clock produces visible rejects instead of silent
  loss or misfiled rows.
- Data before 1970 or after 2099 cannot be stored. For live or recent market
  data that is not a restriction.
- Tables created by a build with the 32-bit TTL keep that TTL until altered
  (`ALTER TABLE … MODIFY TTL`); `Migrate` only creates missing tables. No such
  build was ever released.
