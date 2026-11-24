# Test report

Run on 2026-11-23 with `make test-all` (gofmt, `go vet`, then all three suites with the race detector) on darwin/arm64, Go 1.27.1.

**Result: 55 tests, 55 passed, 0 failed, 0 skipped. No data races reported.** `make demo` also exits 0.

| Suite | Command | Tests | Result |
| --- | --- | --- | --- |
| Unit | `make test` | 31 | pass |
| Integration | `make integration-test` | 12 | pass |
| Scenario + replay | `make scenario-test` | 12 | pass |

Statement coverage of the library packages (everything under `src/` except the three `cmd` programs), all suites combined: **92.6%** (`go test -coverpkg=… ./tests/...`).

## What each suite establishes

- **Unit** — fixed-point parsing and saturation; policy validation (13 broken documents each refused); quote freshness, skew and collar at their exact boundaries; audit chain verification, five kinds of tampering, torn-write recovery; position and realized P&L accounting; every limit at the largest passing and smallest failing value.
- **Integration** — the HTTP API end to end, including the README's protocol sketch verbatim; malformed input; auth on every privileged route; 300 concurrent clients against one notional limit; reserve/release churn; restart recovery compared field by field; refusal to start on a forged log or invalid policy; policy reload; kill-switch webhook with a subscriber that fails twice; reconciliation and metrics output.
- **Scenario** — no policy; audit log outage; feed loss and recovery; stale mark on another symbol; clock skew in both directions; duplicate delivery; kill switch at evaluate, approve and commit; kill switch under concurrent load; lease expiry across a restart; unreachable OMS; order retention; and a recorded 44-step session that must reproduce a pinned transcript and a bit-identical audit log.

## Not covered by tests

The three `cmd` programs (`riskd`, `demo`, `bench`) have no unit tests; `riskd` was checked by hand with curl and `demo` runs in CI. The Dockerfile and compose file have never been built. There is no long-running soak test, no fuzzing, and no test on Linux yet (CI is configured for `ubuntu-latest` but has not run, as this directory is not yet a git repository). Power-loss durability is argued in the architecture doc, not tested.

## Individual results

```text
--- PASS: TestReadmeProtocolSketch (0.02s)
--- PASS: TestMalformedRequestsAreRejectedWithEvidence (0.51s)
--- PASS: TestPrivilegedRoutesFailClosed (0.02s)
--- PASS: TestConcurrentOrdersCannotOversubscribe (0.21s)
--- PASS: TestReserveReleaseChurnNeverExceedsLimit (1.68s)
--- PASS: TestRestartRebuildsStateFromAuditLog (0.03s)
--- PASS: TestEngineRefusesToStartOnTamperedLogOrBadPolicy (0.01s)
--- PASS: TestPolicyReloadIsAtomicAndVersioned (0.01s)
--- PASS: TestKillSwitchPropagatesToOMSWithRetries (0.16s)
--- PASS: TestReconciliationDriftAndMetrics (0.01s)
--- PASS: TestQuoteIngestOverHTTP (0.01s)
--- PASS: TestReservationLifecycleOverHTTP (0.03s)
ok  	riskengine/tests/integration	4.126s
--- PASS: TestNoPolicyMeansNoTrading (0.01s)
--- PASS: TestAuditOutageFailsClosed (0.01s)
--- PASS: TestMarketDataLossAndRecovery (0.01s)
--- PASS: TestStaleMarkOnAnotherSymbolForcesReview (0.01s)
--- PASS: TestClockSkew (0.01s)
--- PASS: TestDuplicateDelivery (0.01s)
--- PASS: TestKillSwitchBlocksEveryEntryPoint (0.01s)
--- PASS: TestKillSwitchIsImmediateUnderLoad (0.07s)
--- PASS: TestReservationLeaseExpiry (0.02s)
--- PASS: TestOMSUnreachableDoesNotWeakenKillSwitch (0.06s)
--- PASS: TestFinishedOrdersAreForgottenAfterRetention (0.02s)
--- PASS: TestRecordedSessionReplaysIdentically (0.12s)
ok  	riskengine/tests/scenarios	1.594s
--- PASS: TestAuditChainVerifies (0.01s)
--- PASS: TestAuditDetectsTampering (0.05s)
--- PASS: TestAuditTailTruncationChangesHead (0.01s)
--- PASS: TestAuditRecoversFromTornFinalWrite (0.01s)
--- PASS: TestSigningKeyIsCreatedPrivateAndReused (0.00s)
--- PASS: TestDecimalParseAndFormat (0.00s)
--- PASS: TestDecimalRejectsAnythingThatWouldRound (0.00s)
--- PASS: TestDecimalJSONNeverUsesFloat (0.00s)
--- PASS: TestArithmeticSaturatesInsteadOfWrapping (0.00s)
--- PASS: TestOrderSizeBoundaries (0.01s)
--- PASS: TestValidationRejects (0.01s)
--- PASS: TestInstrumentRules (0.01s)
--- PASS: TestPriceCollarBoundaries (0.01s)
--- PASS: TestQuoteFreshnessBoundaries (0.01s)
--- PASS: TestRateLimitSlidingWindow (0.02s)
--- PASS: TestSymbolExposureBoundary (0.01s)
--- PASS: TestExposureIsWorstCaseAcrossOpenOrders (0.01s)
--- PASS: TestOverLimitAccountCanStillReduce (0.01s)
--- PASS: TestGrossExposureBoundary (0.01s)
--- PASS: TestDailyLossBoundaryAndRollover (0.02s)
--- PASS: TestReviewThresholdHoldsCapacity (0.01s)
--- PASS: TestSessionClosed (0.01s)
--- PASS: TestEveryDecisionIsExplained (0.01s)
--- PASS: TestFreshnessBoundaries (0.00s)
--- PASS: TestCollarBoundaries (0.00s)
--- PASS: TestQuoteIngestRefusesBadWrites (0.00s)
--- PASS: TestPolicyAccountOverridesOverlayDefaults (0.00s)
--- PASS: TestPolicyRejectsBrokenDocuments (0.00s)
--- PASS: TestSessionWindow (0.00s)
--- PASS: TestPositionAndRealizedPnL (0.05s)
--- PASS: TestFillLifecycle (0.01s)
ok  	riskengine/tests/unit	1.859s
```
