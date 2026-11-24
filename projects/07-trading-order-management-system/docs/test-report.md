# Test report

Simulated / paper trading only. Every number here was produced by running the commands shown on the machine described; nothing is estimated. Where something was not run, it says so.

## Environment

| | |
|---|---|
| Date | 2026-11-24 |
| Machine | Apple M5, 10 cores, 16 GB, macOS 26.5.1 (darwin/arm64) |
| Go | 1.27.1 |
| PostgreSQL | 17.5, started by the test harness as a throwaway server (`src/devpg`), loopback TCP |
| Broker | The simulator in `src/brokersim`, reached over real HTTP in both directions |

## Results

| Suite | Command | Result |
|---|---|---|
| Lint | `make lint` (`gofmt -l`, `go vet ./...`) | clean |
| Unit | `make test` (`go test -race -count=1 ./tests/unit/...`) | 37 tests passed, 0 failed, 0 skipped |
| Integration | `make integration-test` (`-race`) | 73 tests passed, 0 failed, 0 skipped |
| Scenario | `make scenario-test` (`-race`) | 17 tests (19 with subtests) passed, 0 failed, 0 skipped |
| Demo | `make demo` | 52 of 52 checks passed, exit 0 |
| Repeats | `go test -count=5 ./tests/scenarios/...`, `go test -count=3 ./tests/integration/...` | all passed |

All three suites run under the race detector. The integration suite also builds `omsd` and `brokersim` with `-race` and runs them as separate processes.

Every database-backed test gets its own empty database and, when it ends, replays the event log and fails if the `orders`, `executions` or `positions` tables differ from it.

## Definition of done

| Requirement | Evidence |
|---|---|
| Repeated requests with identical idempotency keys do not create multiple orders | `TestIdempotency_RepeatedCreateReturnsTheFirstOrder` (byte-identical response), `TestIdempotency_ConcurrentCreatesWithOneKey` (concurrent requests, one order), `TestIdempotency_KeyReusedForADifferentRequest`, `TestIdempotency_EquivalentBodiesAreTheSameRequest`, `TestIdempotency_KeysAreScopedToThePrincipal`, `TestIdempotency_DuplicateClientOrderID`, `TestIdempotency_RiskRejectionIsReplayedToo`, `TestIdempotency_RepeatedCancelSendsOneCancel`, `TestIdempotency_RepeatedReplaceIsReplayed`. The benchmark replays every create and counts orders afterwards (2000 of 2000) |
| Duplicate fills do not double-count quantity or cash | `TestFills_RedeliveredWebhookIsCountedOnce`, `TestFills_SameExecutionUnderANewEventID`, `TestFills_SameExecutionIDWithDifferentTermsIsQuarantined`, `TestScenario_DuplicateDelivery`, `TestScenario_ConcurrentLoadWithFaults` (every webhook delivered twice, positions exact) |
| Total filled quantity never exceeds order quantity absent explicit corrections | `TestFills_OverfillIsQuarantinedUntilAnOperatorAcceptsIt`, `TestReconcile_OverfillFoundInSnapshotIsQuarantined`, `TestApply_OverfillNeedsAnExplicitAcceptance`, `TestProperty_RandomHistoriesKeepTheInvariants` (500 random histories; invariants checked after every event) |
| Restarting workers drains the outbox safely | `TestProcesses_KillAndRestartDrainsTheOutbox` (real `omsd` process killed with SIGKILL, restarted; 45 orders, 45 broker orders, 12 resubmissions deduplicated by the broker), `TestOutbox_WorkerDiesAfterTheBrokerAccepted`, `TestOutbox_LostResponseDoesNotDuplicateTheOrderAtTheBroker`, `TestOutbox_StaleWorkerCannotFinishAMessageItNoLongerOwns`, `TestOutbox_ManyOrdersTwoWorkersEachSubmittedOnce`, `TestOutbox_BackgroundWorkerDeliversAndSurvivesRestart`, `TestScenario_FlakyBrokerAndWorkerRestarts` |
| Reconciliation identifies discrepancies | The same process test (48 missing executions found and repaired after the restart, then a clean run), and the 13 `TestReconcile_*` tests: lost fill, missed status reports, execution mismatch (reported, not overwritten), phantom execution, missing order, order missing at the broker, state and quantity mismatches, overfill in the snapshot, grace window, snapshot unavailable, one run at a time, scheduled runs |
| No backward transition without a documented correction event | `TestTransitions_ApplyAndTheTableAgreeExactly`, `TestTransitions_ShapeOfTheTable`, `TestFills_CorrectionAndBust`, `TestFills_BustOfTheOnlyFillReturnsToAcknowledged` |
| Executed trades are not mutated to hide discrepancies | `TestFills_EventsAndExecutionsAreAppendOnly` (the database refuses UPDATE, DELETE and TRUNCATE), `TestReconcile_ExecutionMismatchIsReportedNotOverwritten` |
| The broker is not assumed to be exactly-once | Every outbox and delivery test above |

## Required scenarios

| Scenario | Tests |
|---|---|
| Duplicate delivery | `TestScenario_DuplicateDelivery`, `TestFills_RedeliveredWebhookIsCountedOnce` |
| Out-of-order delivery | `TestScenario_FillArrivesBeforeTheAcknowledgement`, `TestScenario_CancelConfirmationArrivesBeforeAnEarlierFill`, `TestScenario_CorrectionsArriveBeforeTheirOriginals`, `TestScenario_AnyDeliveryOrderConverges` (40 shuffles of one history, one final state), `TestScenario_TransientOverfillFromReorderingIsRecoveredByReconciliation` |
| Stale events | `TestScenario_StaleEvents` |
| Missing data | `TestScenario_LostWebhooksAreRecoveredByReconciliation`, `TestScenario_MalformedAndIncompleteReports`, `TestNormalizeWebhook` |
| Connection loss | `TestScenario_BrokerConnectionLoss`, `TestHTTPClient_NoResponseIsRetryable`, `TestOutbox_RetriesWithBackoffUntilTheBrokerAccepts`, `TestOutbox_DeadLetterThenRequeue` |
| Clock skew | `TestScenario_ClockSkew` (broker clock 10 minutes ahead and behind), `TestReconcile_LeavesRecentActivityAlone` |
| Rejected writes | `TestScenario_RejectedWrites` (the database refuses the write; the API and the webhook answer 503 and store nothing partial), `TestScenario_RejectedWritesInTheDispatcher`, `TestAPI_RiskUnavailableFailsClosed` |
| Load with faults | `TestScenario_ConcurrentLoadWithFaults`: 120 orders, 25 failed and 25 lost broker responses, every webhook duplicated, four webhook senders reordering; all orders filled exactly once |

Unit tests cover the calculations: the transition table, each decision function, order validation, fixed-point parsing and overflow, webhook signatures and normalization, the HTTP client's retry classification, backoff, API key parsing and permissions, risk limits, metric rendering, ids and the clock.

## Replay

`TestReplay_DeterministicGoldenHistory` runs a scripted session (orders, partial fills, a correction, a bust, a cancel, a replace, duplicates, an overfill, a reconciliation repair) on a frozen clock with sequential ids, twice, in separate databases. The two event logs must be identical to each other and to `tests/scenarios/testdata/replay.golden.jsonl` (42 events). After an intended behaviour change, `make golden` re-records it and the diff shows exactly what changed.

`TestReplay_FoldingEventsRebuildsEveryOrder` rebuilds every order from its events alone and compares with the table.

The property tests are seeded (seeds 1 to 300 and 1 to 500), so a failure names the seed that reproduces it.

## Security checks

| Check | Evidence |
|---|---|
| Unauthenticated and wrongly authenticated requests are refused | `TestAPI_AuthenticationFailsClosed`, `TestAuth_AuthenticateAndPermissions` |
| Roles and account scopes | `TestAPI_AccountScopesAndRoles` (another account's order answers 404; viewers cannot trade; traders cannot repair) |
| Webhooks need a valid signature; no secret means no webhooks | `TestAPI_WebhookAuthentication`, `TestWebhookSignature` |
| Processes refuse to start without their secrets or with weak ones, and name what is missing without echoing values | `TestProcesses_OmsdFailsClosedOnBadConfiguration`, `TestAuth_ParseKeysFailsClosed` |
| `omsd` refuses a broker that does not identify itself as simulated | Same test |
| No credential appears in the logs of either process at debug level | `TestProcesses_KillAndRestartDrainsTheOutbox` searches the output of both for every token, key, secret and the database URL |
| No credential is committed | `TestRepository_ContainsNoCredentials` scans every text file; `TestExamples_CredentialTemplatesAreNotUsable` checks that the example key file cannot be loaded as it stands and that `.env.example` has no values |

## Performance

### Method

`make bench` (`src/cmd/bench`) starts PostgreSQL, the simulated broker (auto mode: it acknowledges, then fills each order in two executions) and the OMS in one process, connected over loopback HTTP, and drives the public API. Each order is a 100-share limit order that fills completely, so one order is one create request, one broker submission and three webhooks: six committed transactions.

- **Commits are durable.** The throwaway PostgreSQL runs with `fsync=on`, `synchronous_commit=on` and, on macOS, `wal_sync_method=fsync_writethrough`, because a plain fsync on macOS does not flush the drive's cache. Runs made before that setting was added were faster and are not reported: they measured commits that were not durable.
- Request latencies are measured by the client. In paced runs they are measured from the moment the request was due, not from when a client was free to send it.
- Pipeline times are read back from the timestamps the OMS stored on each order (`created_at`, `submitted_at`, `acknowledged_at`, `closed_at`). They include the simulator's own delays: 1 ms before the acknowledgement and 1 ms before each fill.
- After the load, every create is sent again with the same key (replays), every order is read once, one reconciliation run compares everything with the broker, and the event log is replayed against the projections. A run counts only if every order filled, the order count is unchanged after the replays, reconciliation finds no drift and the replay finds no problems; otherwise the benchmark exits non-zero.

Limits of the method: the load generator, the simulated broker and the OMS share one process and one laptop, and PostgreSQL shares the machine. Each configuration below is a single run. These are a baseline for this machine, not a capacity plan.

### Unloaded: 50 orders per second

`go run ./src/cmd/bench -orders 1000 -rate 50` (16 clients, outbox concurrency 8, 4 webhook senders, one account and symbol)

| | p50 ms | p95 ms | p99 ms | max ms | errors |
|---|---:|---:|---:|---:|---:|
| `POST /v1/orders` (new) | 6.65 | 10.71 | 12.78 | 34.54 | 0 |
| `POST /v1/orders` (idempotent replay) | 0.34 | 2.21 | 6.22 | 45.34 | 0 |
| `GET /v1/orders/{id}` | 0.25 | 0.59 | 0.98 | 1.67 | 0 |
| accepted to first submission attempt | 12.39 | 19.20 | 23.12 | 36.83 | |
| submission to acknowledgement applied | 8.24 | 10.12 | 11.41 | 19.24 | |
| accepted to `FILLED` | 22.93 | 30.48 | 34.82 | 51.11 | |

1000 of 1000 orders filled. Replays: 16,568 per second. Reads: 52,781 per second. Reconciliation over all 1000 orders: 0.13 s, no drift. Event-log replay: 5,981 events in 0.01 s, no problems. Process memory at the end (OMS, simulated broker and load generator together; PostgreSQL excluded): heap 14.0 MB, 34.8 MB obtained from the OS.

A create is one durable commit, which is where its 6.65 ms goes. Reads and replays do not commit and take a fraction of a millisecond.

### Burst: one account and symbol

`go run ./src/cmd/bench -orders 2000` (clients send as fast as they can)

| | p50 ms | p95 ms | p99 ms | max ms | errors | per second |
|---|---:|---:|---:|---:|---:|---:|
| `POST /v1/orders` (new) | 8.22 | 18.25 | 40.78 | 76.13 | 0 | 1,514 |
| `POST /v1/orders` (idempotent replay) | 0.28 | 0.56 | 1.06 | 1.95 | 0 | 50,366 |
| `GET /v1/orders/{id}` | 0.29 | 0.68 | 1.03 | 1.64 | 0 | 47,326 |
| accepted to first submission attempt | 1,630 | 3,081 | 3,211 | 3,239 | | |
| submission to acknowledgement applied | 8,066 | 13,153 | 13,694 | 13,814 | | |
| accepted to `FILLED` | 9,733 | 16,262 | 16,945 | 17,085 | | |

2000 of 2000 orders filled, 18.42 s after the first request: **109 orders per second end to end**. Reconciliation: 0.21 s, no drift. Replay: 11,993 events in 0.03 s, no problems. Heap 20.1 MB, 47.0 MB from the OS.

The API accepted orders at 1,514 per second; the rest of the pipeline completed 109 per second. The pipeline times above are therefore queueing time, not processing time: orders waited in the outbox and then for their reports. Nothing was lost, duplicated or dead-lettered while the backlog drained.

### Burst: one account and symbol, more parallelism

`go run ./src/cmd/bench -orders 2000 -outbox-concurrency 32 -webhook-senders 32`

| | p50 ms | p95 ms | p99 ms | max ms | errors | per second |
|---|---:|---:|---:|---:|---:|---:|
| `POST /v1/orders` (new) | 21.72 | 34.81 | 65.36 | 99.18 | 0 | 752 |
| `POST /v1/orders` (idempotent replay) | 0.33 | 0.73 | 1.29 | 2.83 | 0 | 41,624 |
| `GET /v1/orders/{id}` | 0.26 | 0.51 | 0.71 | 0.95 | 0 | 55,320 |
| accepted to first submission attempt | 80.53 | 112.22 | 125.59 | 159.25 | | |
| submission to acknowledgement applied | 8,618 | 15,596 | 16,179 | 16,281 | | |
| accepted to `FILLED` | 8,778 | 15,720 | 16,278 | 16,396 | | |

2000 of 2000 orders filled, 19.17 s after the first request: **104 orders per second end to end**. Reconciliation: 0.23 s, no drift. Replay: 11,996 events in 0.03 s, no problems. Heap 26.7 MB, 55.2 MB from the OS.

Four times the workers moved the queue from the outbox (submissions now start within about 80 ms) to the reports, and completed no more orders per second. Worker count is not the limit here.

### Burst: eight positions, same parallelism

`go run ./src/cmd/bench -orders 2000 -spread -outbox-concurrency 32 -webhook-senders 32` (2 accounts x 4 symbols)

| | p50 ms | p95 ms | p99 ms | max ms | errors | per second |
|---|---:|---:|---:|---:|---:|---:|
| `POST /v1/orders` (new) | 17.73 | 31.00 | 62.20 | 86.21 | 0 | 882 |
| `POST /v1/orders` (idempotent replay) | 0.45 | 1.47 | 2.08 | 4.50 | 0 | 27,624 |
| `GET /v1/orders/{id}` | 0.40 | 0.98 | 1.74 | 3.60 | 0 | 33,617 |
| accepted to first submission attempt | 80.78 | 118.18 | 143.31 | 157.96 | | |
| submission to acknowledgement applied | 1,027 | 1,670 | 1,724 | 1,760 | | |
| accepted to `FILLED` | 1,118 | 1,750 | 1,822 | 1,859 | | |

2000 of 2000 orders filled, 4.14 s after the first request: **483 orders per second end to end**. Reconciliation: 0.22 s, no drift. Replay: 11,930 events in 0.03 s, no problems. Heap 29.1 MB, 55.2 MB from the OS.

### What the numbers say

- With durable commits the cost of an order is its six commits. Unloaded, that is about 23 ms from accepted to fully filled at the median.
- **One account and symbol is the worst case.** Every fill updates that position's row inside its own transaction, so those fills commit one after another. With identical settings, a single position completed 104 orders per second and eight positions completed 483. This is a known limit of keeping positions in the fill transaction; [ADR 0001](decisions/0001-event-log-and-outbox-in-postgres.md) records the trade-off and what to change.
- Reads and idempotent replays are cheap and are not affected by the write backlog.
- Not measured: more than one `omsd` process, a PostgreSQL on a separate host, a network between the components, sustained load for longer than about 20 seconds, order books larger than 2000 orders.

## Defects found during this work and fixed

| Defect | Fix |
|---|---|
| A replayed idempotent response was not byte-identical to the first one: the stored body was `JSONB`, which PostgreSQL normalises | Responses are stored as `BYTEA`. `TestIdempotency_RepeatedCreateReturnsTheFirstOrder` compares bytes |
| An outbox worker stopped during a claim could commit leases that nobody then worked, delaying those messages until the lease expired | A claim is not started once shutdown has begun and is not interrupted halfway |
| Reconciliation raised a second, redundant state-mismatch issue for an order whose overfill it had just quarantined | Orders with a report quarantined by the current run are not judged further in that run. `TestReconcile_OverfillFoundInSnapshotIsQuarantined` |
| `positions.updated_at` depended on the order in which concurrent transactions reached the row, so it could differ from a replay of the log. Quantities and cash were correct throughout. Found by the benchmark's replay check at 32 webhook senders | The timestamp is the latest one seen, which does not depend on order. `TestPosition_DoesNotDependOnTheOrderFillsAreBooked` fails without the fix |
| An execution could be booked whose notional or fee, added to the order's totals, did not fit in 64 bits | Such an execution is refused as an invalid report and quarantined |
| The benchmark claimed durable commits on macOS while using a sync method that does not flush the drive's cache | `fsync_writethrough` on macOS; all figures above were measured after the change |

## Not verified

- **The container image and `docker-compose.yml` were not built or started.** No Docker daemon was available. `docker compose config` parses the file, and confirms that it refuses to start when a secret is missing. The two binaries the image runs were tested as separate processes with the same environment variables the compose file sets, against the test PostgreSQL.
- **The CI workflow (`.github/workflows/ci.yml`) has not run.** Each `make` target it calls was run locally. The one path it uses that was not exercised here is running the suites against an external server through `OMS_TEST_DATABASE_URL`.
- **`scripts/gen-env.sh`** was exercised with a stand-in for `openssl` that printed a fixed placeholder, to check the file it writes, its permissions and its refusal to overwrite, without putting generated secrets on disk during testing. The real `openssl rand` call is one line and was not run.
- **Linux.** Everything above ran on macOS/arm64.
- **A real broker.** There is no adapter for one, by design.
