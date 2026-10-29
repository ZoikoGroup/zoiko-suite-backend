# Doc 05 contract tests CT-001 to CT-052: where each one is tested

This maps every contract test in Doc 05 §27–28 to the automated tests that cover it. The backend is
`services/public-ai-svc` and the frontend is `zoiko-suite-frontend-platform`.

**Numbering note.** Four older backend tests were named before this map existed, and their numbers
do not match Doc 05:

| Old test name | Doc 05 test it actually covers |
|---|---|
| `TestCT003_OriginDenied` | CT-004 |
| `TestCT004_OversizedBody` | CT-007 |
| `TestCT009_TurnReplayNoSecondCall` | CT-010 |
| `TestCT011_TurnConflict` | CT-012 |

The table below always uses the Doc 05 numbers. Tests added for this map are named `TestDoc05CTnnn_…`.

Status values:

- **Covered:** the expected result in Doc 05 is asserted.
- **Partial:** the core behaviour is asserted, but not every clause.
- **N/A in R0:** the feature does not exist in R0, and a test proves it is off.
- **Gap:** not tested yet.

## §27 Public surface

| CT | Doc 05 expectation | Tests | Status |
|---|---|---|---|
| CT-001 | 201, opaque subject, safe capabilities | `httpapi.TestCT001_CreateSession` | Covered |
| CT-002 | Missing/invalid grant → 401/403, no disclosure | `httpapi.TestDoc05CT002_MissingOrInvalidGrant`, `TestDeleteSession` (no grant), `TestCT002_SessionReplay` (rotated-out grant) | Covered |
| CT-003 | Cross-session turn denied | `httpapi.TestTurnSessionMustMatchGrant` | Covered |
| CT-004 | Unapproved Origin → 403, no CORS grant | `httpapi.TestCT003_OriginDenied`, `TestMissingOriginDenied`, `TestPreflightAllowedOrigin` | Covered |
| CT-005 | Bad Content-Type → 415 | `httpapi.TestCT005_WrongContentType` | Covered |
| CT-006 | Unknown schema_version → 400 unsupported_schema | `httpapi.TestDoc05CT006_UnknownSchemaVersion`, `TestSessionValidation`, `contract.TestTurnRequestValidation` | Covered |
| CT-007 | Oversize → 413, no model call | `httpapi.TestCT004_OversizedBody`, `contract.TestTurnRequestValidation` (2,000 chars) | Covered |
| CT-008 | Malformed market_code → 400, coverage never inferred | `contract.TestTurnRequestValidation` ("lower market"), `acceptance.TestG04`, `TestT4b_OtherCountryNotCovered` | Covered |
| CT-009 | Unknown page_context rejected; never a DOM fetch | `contract.TestTurnRequestValidation` ("bad page"; enum in `contract.PageContexts`) | Covered |
| CT-010 | Duplicate key → same turn, one model charge | `httpapi.TestCT009_TurnReplayNoSecondCall`, `storetest` | Covered |
| CT-011 | Same key, different payload → 422 | `httpapi.TestIdempotencyMismatch`, `TestIdempotencyHeaderMismatch` | Covered |
| CT-012 | Concurrent turn same session → 409 | `httpapi.TestCT011_TurnConflict` | Covered |
| CT-013 | Expired session → 401, no old result | `httpapi.TestExpiredSession` | Covered |
| CT-014 | Out-of-order final ignored by client | Server: one active turn per session (`TestCT011_TurnConflict`). Client: `AssistantWidget.test.tsx` "ignores a reply whose turn_sequence is older…", `state.test.mjs` "no late output after cancel or close" | Covered |
| CT-015 | Typed ANSWERED envelope with valid citations | `httpapi.TestTurnHappyPathEnvelope`, `contract.TestEnvelopeValidate`, `e2e.TestEndToEndConversation` | Covered |
| CT-016 | Invalid status enum → safe unavailable | `contract.TestDoc05CT016_InvalidStatusEnum`, `httpapi.TestInvalidOrchestratorOutputIsContained` | Covered |
| CT-017 | Overlength output rejected/repaired once, never raw | `contract.TestEnvelopeValidate` (>5,000), `validate.TestTruncatesToWholeSentences`, `orchestrator.TestOutputValidation` | Covered |
| CT-018 | Unsafe action URL → route_key allowlist | `orchestrator.TestOutputValidation` (FaultInjectURL), `validate.TestModelLinksAreRemoved`, `release.TestRoutesRejectUnsafeURLs`, `contract.TestEnvelopeValidate` (POST_FORM) | Covered |
| CT-019 | SSE disabled → deterministic refusal | `httpapi.TestTurnEventsDisabledInR0`, `TestCT001_CreateSession` (streaming=false) | Covered |
| CT-020 | Stream grant cross-session | Covered by CT-019: no stream endpoint exists in R0 | N/A in R0 |
| CT-021 | DELETE own session twice → 204, then blocked | `httpapi.TestDeleteSession` | Covered |
| CT-022 | DELETE other session → generic 404 | `httpapi.TestDeleteSession` | Covered |
| CT-023 | Config ETag, no secrets, 304 | `httpapi.TestConfigETag`, frontend `boundary.test.tsx` | Covered |
| CT-024 | Health in outage is generic | `httpapi.TestPublicHealthIsGeneric`, `TestReadinessReflectsDependencies` | Covered |

## §28 Integration, events and safety

| CT | Doc 05 expectation | Tests | Status |
|---|---|---|---|
| CT-025 | P1 price missing → ABSTAIN | `acceptance.TestT3_UnitedKingdomExpiredTrialAbstains`, `TestG04`, `orchestrator.TestExpiredEvidenceAbstains` | Covered |
| CT-026 | Contradicted price → claim block and governance case | `orchestrator.TestConflictingPricesAbstain`, `acceptance.TestG30` | Partial: blocked and abstained; no governance case is opened (no case system in R0) |
| CT-027 | Expired trust attestation → no certified claim | `orchestrator.TestSOC2NeverClaimedCertified`, `acceptance.TestG10`, `TestG24`, `TestK6` | Covered |
| CT-028 | Market unavailable → clarify/abstain | `acceptance.TestG06`, `TestT4b_OtherCountryNotCovered`, `TestS01` | Covered |
| CT-029 | Trial gate false → CTA hidden | `orchestrator.TestTrialRouteOnlyWhereVerified`, `claims.TestTrialRouteGatedByClaim` | Covered |
| CT-030 | Private chunk filtered before retrieval | `knowledge.TestPrivateAndPoisonedNeverRetrieved` | Covered |
| CT-031 | Release revoked during generation | `orchestrator.TestRevocationMidFlightAbstains`, `release.TestRevokedSinceAtCommit`, `acceptance.TestT11` | Covered |
| CT-032 | Malformed citation anchor rejected | `validate.TestCheck` (anchor injection, path, case, empty), `orchestrator.TestOutputValidation` (FaultUnknownRef), `acceptance.TestGroq8_Failures` | Covered |
| CT-033 | Model tool instruction → no tool call | `validate.TestGatewayDeadlineAndNoTools`, `model.TestGroqRequestShapeAndParse` (no tools sent) | Covered |
| CT-034 | Injection in approved document ignored | `acceptance.TestG22_InjectionInRetrievedArticle`, `knowledge.TestPrivateAndPoisonedNeverRetrieved` | Covered |
| CT-035 | Bank records request → REFUSED_ACTION | `acceptance.TestG16`, `TestT6`, `orchestrator.TestPrivateDataAndActionsRefused` | Covered |
| CT-036 | Payroll ID entered → intercepted, no plaintext telemetry | `policy.TestRedactSensitiveInput`, `acceptance.TestT8`, `orchestrator.TestSensitiveInputNeverEchoed`, frontend `state.test.mjs` pre-send warning | Covered |
| CT-037 | Commercial service timeout → ABSTAIN/UNAVAILABLE | None. R0 reads claims from the signed local release, so there is no commercial service call | N/A in R0 |
| CT-038 | Model timeout → safe UNAVAILABLE, retry | `acceptance.TestGroq8_Failures`, `TestG27`, `orchestrator.TestProviderFailuresFallBackSafely`, frontend "T10 outage" | Covered |
| CT-039 | Route resolution fails → CTA hidden | `release.TestRouteResolutionFailsClosed` (unknown key, URL as key, outside effective window), `orchestrator.TestDemoIsRouteOnly`, `release.TestRoutesRejectUnsafeURLs` | Covered |
| CT-040 | Duplicate event → effect once | `events.TestRelayDeliversOnceAndDivertsToDLQ` (producer side) | Partial: R0 has no consumer, so consumer dedupe is untested |
| CT-041 | Out-of-order events: older release not applied | `release.TestRevocationSurvivesRollback` | Partial |
| CT-042 | DLQ replay rechecks auth and freshness | None. R0 has no DLQ replay tool | Gap |
| CT-043 | Unsigned KB release rejected | `release.TestSignatureEnforced` | Covered |
| CT-044 | Revocation with stale cache → claims denied | `release.TestRevokedSinceAtCommit`, `acceptance.TestT11`, `TestK8` | Covered |
| CT-045 | Event with raw chat text rejected | `events.TestEnvelopeShapeAndRedaction` (exactly 5 allowlisted fields), `TestOnlySixEventTypes`, `acceptance.TestT7` | Covered by construction: payloads are typed, so raw text cannot be added. No DLP alert exists |
| CT-046 | Browser posts internal event → blocked | `httpapi.TestDoc05CT046_BrowserCannotPostEvents`, frontend `boundary.test.tsx` (path allowlist) | Covered at the gateway. Broker ACLs need infrastructure |
| CT-047 | Broker outage → outbox retains event | `events.TestRelayDeliversOnceAndDivertsToDLQ` (flaky publisher) | Covered |
| CT-048 | Kill switch mid-flight | `orchestrator.TestKillSwitchMidFlightWithholdsAnswer`, `TestKillSwitchAndFlags`, `limits.TestSpendCeilingStopsAnswering` | Covered |
| CT-049 | No messages, grants or tokens in telemetry | `telemetry.TestTurnSpansCarryNoMessageText`, `TestOTLPExportOverTheWire`, `TestFilterDropsContent`, `acceptance.TestT7`, `TestT8`, `model.TestGroqErrorsNeverLeakKeyOrBody`, `limits` (grant hashed) | Covered (logs, events and OTLP traces) |
| CT-050 | Cross-product query → product boundary held | `acceptance.TestG23_ZoikoOneFeatures`, `isolation.TestSECT049_NoOtherZoikoModules` | Covered |
| CT-051 | CMS webhook replay denied | None. R0 has no CMS webhook | N/A in R0 |
| CT-052 | Accessibility on retry and handoff | Frontend `assistant.spec.ts` "K11 axe", "K11 keyboard only", "T10 outage"; `ui.spec.ts` axe, Tab order, S08 retry | Covered |

## Summary

| Status | Count | CTs |
|---|---|---|
| Covered | 45 | All CTs not listed in the other rows (CT-046 at the gateway) |
| Partial | 3 | 026, 040, 041 |
| N/A in R0 | 3 | 020, 037, 051 |
| Gap | 1 | 042 |

Most Partial items need a part of the system that R0 does not have: an event consumer, a case
system, a DLQ tool, or broker ACLs.
