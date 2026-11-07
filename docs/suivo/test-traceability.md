# Suivo R0 — test traceability (dev build)

Maps document requirements and test IDs to the automated tests in `services/public-ai-svc`.
"Partial" means the behaviour is tested but not every case the document lists; "Not covered" items
need work or a human/manual lane. Nothing here claims a Doc 07 gate threshold has been met: thresholds
apply to the approved holdout set, real content and the approved model, none of which exist yet.

| Requirement / test IDs | Covered by | Status |
|---|---|---|
| INV-01, SEC-P0-01, SEC-T049 (no private-system path) | `isolation.TestSECT049_NoOtherZoikoModules`, `TestForbiddenDepsRejectsOtherServices` | Covered (code dependency level; network egress policy needs infra) |
| Doc 05 CT-001 to CT-052 | See `contract-test-map.md` (one row per CT, with Doc 05 numbering) | 45 covered, 3 partial, 3 N/A in R0, 1 gap |
| Doc 05 §07 delete semantics | `httpapi.TestDeleteSession` | Covered |
| Doc 05 §10 envelope schema | `contract.TestEnvelopeValidate`, `httpapi.TestInvalidOrchestratorOutputIsContained`, `e2e` | Covered |
| Doc 03 §09 ingestion states | `release.TestIngestionStates`, `TestRoutesRejectUnsafeURLs` | Covered |
| Doc 03 §11–12, RB-02, OPR-005 revocation + rollback | `release.TestRevocationSurvivesRollback`, `TestRevokedSinceAtCommit`, `orchestrator.TestRevocationMidFlightAbstains`, `TestRevokedSourceIsNotUsed`, Postgres append-only trigger test | Covered |
| Doc 05 §17 claim decisions | `release.TestClaimDecisions`, `orchestrator.TestConflictingPricesAbstain`, golden C02–C04 | Covered |
| J04 / AT-03 SOC 2 never claimed | `orchestrator.TestSOC2NeverClaimedCertified`, golden C01 | Covered |
| FR-03 pricing from dated market source | `TestSlicePriceFromVerifiedClaim`, `TestPriceWithoutMarketClarifies`, golden A02, B01–B02 | Covered |
| X01–X08 exceptions | `policy.TestExceptions`, golden C05–C07, C09, D01, D05–D06 | Covered |
| QA-T043.. sensitive input | `policy.TestRedactSensitiveInput`, `TestRedactNoFalsePositives`, `TestSensitiveInputNeverEchoed`, golden D03 | Covered |
| SEC-T039 telemetry canary | `e2e.TestEndToEndConversation`, live smoke run (0 leaks) | Covered |
| T02 / LLM01 indirect injection in KB | ingestion quarantine + golden D02 | Covered |
| Output handling (LLM05) | `validate.TestCheck`, `TestModelLinksAreRemoved`, `orchestrator.TestOutputValidation` | Covered |
| Provider outage / timeout (RB-01) | `TestProviderFailuresFallBackSafely`, gateway deadline test | Covered |
| Kill switch, flags, static fallback | `TestKillSwitchAndFlags`, `flags` via kbctl run | Covered |
| Rate, concurrency, spend limits (T10) | `limits.*` | Covered (thresholds unapproved) |
| Events (six only, redacted, outbox, DLQ) | `events.*`, `e2e` | Covered |
| Golden set partitions A–D | `eval.TestGoldenSet` (37 cases, dev set v2) | Covered; partition E holdout and F drift sentinel **not covered** (need independent authors / production) |
| Semantic rubric + human grading lanes (Doc 07 §08) | — | Not covered (harness is deterministic lane only) |
| WCAG 2.2 AA (QA-T050–056) | Playwright in Edge: axe (no serious or critical issues, light and dark), keyboard-only flow, Tab order, Escape focus return, 44 px targets, contrast, 320 px and 200 % zoom header, reduced motion | Partial: no manual screen-reader pass |
| Load / p95 latency (QA-T057–063) | — | Not covered: needs staging + agreed reference load |
| Doc 09 admin console, Doc 10 CRM/leads | — | Out of R0 scope |
