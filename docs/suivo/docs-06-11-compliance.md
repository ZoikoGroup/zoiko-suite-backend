# Docs 06–11 compliance: ZoikoSuite Assistant R0

This record continues `docs-01-05-compliance.md` for Documents 06–11.

**Re-audit, 2026-10-30.** The previous version of this file called several items Built whose code had been moved to `_parked/` and did not compile, or did not exist at all. Every row below was checked against the code, and every test it names exists and passes (`go test ./...`, with `go test -json ./... | go run ./cmd/skipcheck`). Requirement-by-requirement QA-T mapping is in `qa-t-traceability.md`. The Doc 11 registers are in `handover-registers.md` and `threat-model.md`.

Status values:
- **Built:** implemented and tested.
- **Partial:** core behaviour is in place, but not every clause.
- **Not built:** engineering work still to do.
- **Blocked:** needs a decision, approval, people or staging.
- **Later:** belongs to a later release (R1+); R0 enforces only the guardrail.

Nothing here claims a gate is approved.

## Document 06: security, privacy, abuse prevention and trust

| Requirement | Status | Evidence |
|---|---|---|
| SEC-T016 / §08: encoding cannot bypass the safety layer (fullwidth, zero-width, homoglyphs, base64 instructions) | Built | `policy.SafetyFold` runs first in `Redact`; encoded instructions are treated as X06 injection. `policy.TestSafetyFoldDefeatsEncodingBypasses` |
| Doc 02 S07: an injected instruction is ignored, the permitted question still answered | Built | An "ignore your instructions" opener before an information question is stripped and recorded as `PROMPT_INJECTION`; prompt extraction and requests to act stay refused. `acceptance.TestDoc02S07_PromptInjectionContinues`, `acceptance.TestA19_UserPromptInjection` |
| §21 / COM-T053: contact details not kept | Built | Emails and phone numbers masked before retrieval and the model, without refusing the question; the reply says so. `policy.TestContactDetailsMaskedNotRefused`, `acceptance.TestContactDetailsNotKept` |
| SEC-P0-06 / §19: no provider request without approval; fail closed | Built | `PUBLIC_AI_ENVIRONMENT` is required (unset no longer means dev). Outside dev/test a remote provider needs `PUBLIC_AI_PROVIDER_APPROVAL_REF`, and `PUBLIC_AI_REGION` must be in `PUBLIC_AI_APPROVED_REGIONS`. `config.TestEnvironmentFailsClosed`, `config.TestProductionGates`, `acceptance.TestA40_*` |
| §08: approved policy hash | Built | The policy digest covers templates, boundary templates, redaction detectors, exception and intent rules, menu and CTAs. The service refuses to start unless it equals `PUBLIC_AI_POLICY_DIGEST` (required outside dev/test). `policy.TestPolicyDigestCoversRulesAndIsEnforced` |
| SEC-P0-09 / §13: token and spend limits, staged throttling | Built | Per-IP, per-session and in-flight limits. At 80 % of the daily/monthly ceiling, replies are shorter and never repaired. At 100 %, or at `PUBLIC_AI_SESSION_TOKEN_CAP`, only static content is served (`SPEND_CEILING`); a failed usage read also fails closed. Tokens spent on a rejected draft are still charged. `limits.TestSpendStages`, `limits.TestSpendCeilingStopsAnswering`, `httpapi.TestSessionTokenCapStages`, `limits.TestPerIPLimit`, `limits.TestPerSessionLimitAcrossIPs`, `limits.TestGlobalInFlightCap` |
| §17: session idle expiry in addition to the fixed maximum | Built | `PUBLIC_AI_SESSION_IDLE_TTL` (default 15 min); `last_seen_at` updated per completed turn (migration 006). `session.TestIdleExpiry` |
| §11 / SEC-T012: session grant bound to its origin | Built | A grant used from another origin is treated as expired. `httpapi.TestGrantBoundToOrigin`, `httpapi.TestCT003_OriginDenied`, `httpapi.TestMissingOriginDenied` |
| §21: tamper-evident operator actions | Built | Append-only `public_ai.control_audit` (migration 004, trigger). Flag changes are audited in the same transaction; promote, rollback, revoke and DLQ replay need `-actor`, `-approval` and `-why` and are refused if the audit fails; `kbctl audit` lists entries. `release.TestOperatorActionsAreAudited`, `flags.TestFlagChangesAreAudited`, storetest `ControlAudit`, `store.TestPostgresControlAuditAppendOnly` |
| §17 / SEC-T046: no indefinite visitor fingerprint | Built | Rate-limit buckets are dropped after 10 min idle; IPv6 is coarsened to /64; bucket keys are salted hashes with a random per-process salt, never addresses (`limits.TestIPv6Coarsened`, `limits.TestBucketKeysAreNotAddresses`) |
| §20: sensitive input not re-displayed | Built | The backend never echoes it (X03, `policy.TestTemplatesNeverEchoAndRender`); the widget replaces the bubble when the service flags it |
| §27: denied attempts monitored | Built | `public_ai_security_signals_total{class}` and 401/403 rate alerts in `deployments/prometheus-rules-public-ai.yml` |
| §11: Host allowlist | Built | `PUBLIC_AI_ALLOWED_HOSTS` is required outside dev/test; other Hosts are refused (`config.TestProductionGates`) |
| §03/§11: CSP and clickjacking headers | Partial | The backend sets `default-src 'none'; frame-ancestors 'none'` and HSTS. The Next console sets none; that is a console-owner decision |
| §12, §24: dependency scanning, SBOM, image scan, signing | Partial | CI job `public-ai-supply-chain`: govulncheck (fails on reachable findings), CycloneDX SBOM artifact, Trivy image scan (fails on fixable HIGH/CRITICAL). The 20 reachable findings were fixed by upgrading OpenTelemetry 1.45, x/net 0.60, x/text 0.42, gRPC 1.83.1 and the toolchain to go1.27.3; govulncheck now reports 0 reachable. Runtime image moved to alpine 3.22. Image signing and provenance need a registry and signing key. The Trivy step has only run in CI, not locally |
| §09: dual-signature release receipt | Partial | Promotion needs two distinct approvers and an independent promoter (`release.PromotionGate`), but approvers are names, not separate signatures |
| CODEOWNERS for policy and prompt files | Partial | `.github/CODEOWNERS` lists every protected path and its accountable role; the rules are commented out until real reviewer handles are named (H23) |
| Retention period, DPA, region, Legal/Brand name | Blocked | DEC-04, DEC-05, DEC-06 |

## Document 07: QA, evaluation and release gates

| Requirement | Status | Evidence |
|---|---|---|
| §21 verdicts: P0 failure blocks, P1 failure is NO-GO unless waived | Built | `PASS`, `NO_GO`, `BLOCK_RELEASE`; `evalrun -waivers` applies approved P1 waivers, never to P0 (`eval` tests in `doc07_test.go`) |
| Report binds revisions; intervals; repeats; baseline | Built | Report carries `-sha`, dataset SHA-256, prompt/policy versions, KB and claims releases, model profile; Wilson 95 % intervals per partition (`TestWilsonIntervals`); `-repeats` with flaky detection (`TestRepeatsAndFlakes`); intent accuracy and high-risk false permits (`TestIntentMetricsAndFalsePermit`); `-baseline` regressions are NO_GO (`TestBaselineRegression`); per-case result hash and pinned setup (`TestPinnedReleaseAndResultHash`) |
| Golden set in CI with its verdict | Built | CI step "Doc 07 gates" runs `evalrun -repeats 3` and uploads `eval-report.json`. `testdata/golden/golden-v1.json` (version golden-dev-v3): 40 cases, all pass |
| §10: no test disabled without a waiver | Built | `cmd/skipcheck` fails CI on any skipped test missing from `testdata/skip-allowlist.txt` (each entry needs a reason) |
| §34: machine-readable QA-T traceability | Built | `qa-t-traceability.md`: 71 Built, 9 Partial, 4 Blocked of 84 |
| QA-T042: off-contract model output rejected | Built | `model.TestGroqOffContractReplyIsAnEmptyDraft`; unsafe drafts are not repaired (`acceptance.TestA22_*`) |
| Multi-turn golden cases | Built | A case's `input.before` turns run first in the same conversation (open question and named market carry over); GD-M01 records refusal, GD-M02 demo "yes", GD-M03 session market |
| Holdout partition E, drift sentinel F, human grading | Blocked | Need independent authors and graders |
| Playwright in CI | Built (needs a secret) | Frontend job `assistant-e2e`; skipped when `BACKEND_REPO_TOKEN` is missing |

## Document 08: deployment, DevOps, observability and production readiness

| Requirement | Status | Evidence |
|---|---|---|
| §14: metrics with low-cardinality labels | Built | `/metrics`: HTTP, turns, model calls and tokens, 429s, events, release readiness, build info, CTAs offered, handoffs clicked, feedback, security signals. `httpapi.TestMetricsEndpoint` |
| §13: alerting | Built | `deployments/prometheus-rules-public-ai.yml` (symptom, burn-rate and absent-series rules on the PROPOSED 99.9 % objective) |
| §06: release tuple on every response | Built | Turn span and `policy_decision` log carry KB, claims and policy releases; `public_ai_build_info` |
| §11 / OPR-012: graceful shutdown | Built | On SIGTERM, readiness goes false, then a 5 s drain, then a shutdown timeout longer than the turn lease; compose `stop_grace_period: 45s`. `cmd/server` `TestShutdownOutlastsTurnLease` |
| §20 / CT-042: dead-letter replay | Built | `kbctl dlq-replay` rechecks schema and freshness, is audited, and uses exponential backoff. `events.TestDLQReplayRechecks` |
| Event identity | Built | UUIDv4 ids, a `source` per producer, `control_epoch` on kill-switch events (AsyncAPI updated) |
| §07: pinned image digests, SBOM, provenance, container scan | Partial | SBOM and image scan in CI (see Doc 06 §12); images are pinned by tag, not digest; no provenance attestation |
| §09/§10: synthetic canary; App C restore drill | Partial | `cmd/canary` runs the outside-in canary journey (readiness, config, session, cited answer, route-only CTA, abstain, private-data refusal, end) and exits 1 on any failure; its report has outcomes and latencies only (`canary.TestCanaryJourneyPasses`, `canary.TestCanaryFailsClosed`). `cmd/drill` DR-01..05. No restore drill |
| OPR-004: incompatible release tuple refused at load | Built | KB and claim manifests carry `compatible_policy_versions`; a release that excludes the running policy version is refused and the service stays not-ready. Outside dev/test a release must declare its range (`release.TestIncompatibleReleaseRefusedAtLoad`) |
| Prompt/policy as an independently rollable artifact | Partial | Ships in the app image; guarded by the digest gate |
| Alertmanager, paging, dashboards, staging load and soak | Blocked | Infrastructure and SRE |

## Document 09: administration, knowledge operations and handoff

| R0 guardrail | Status | Evidence |
|---|---|---|
| §11/§13: no invented confirmations or promises | Built | The validator rejects booked or forwarded claims, "will contact you", response times, case numbers and "data deleted". `validate.TestFalseConfirmationsRejected` |
| §12: data-rights requests route-only | Built | Erasure, export and access requests get a route-only reply that records nothing. `acceptance.TestDataRightsRequestsRouteOnly` |
| §05/§18: operator authority and reason recorded | Built | See Doc 06 §21 |
| Claim conflicts become owned cases | Built | Each conflict blocks the claim and names its owner (`owner_id` per claim type). storetest `ConflictCases`, `acceptance.TestKB003_*` |
| Overdue knowledge or claim review | Built | An article with no or overdue review is held; a claim past `review_due_at` counts as expired |
| §20: offers and clicks measured separately | Built | `public_ai_cta_offered_total`, `public_ai_handoff_clicked_total`, `public_ai_feedback_total` |
| Staff console, case management, live agent | Later | R1 |

## Document 10: commercial conversion, CRM and leads

| R0 guardrail | Status | Evidence |
|---|---|---|
| §21: no contact or profile collection in chat | Built | See Doc 06 contact row |
| App. B: `secure_lead_intake_enabled`, `crm_sync_enabled`, `booking_sync_enabled`, `third_party_tracking_enabled` | Built | The flags exist and are refused if set true. `flags.TestDoc10GatesAreHardOff` |
| Lead intake, CRM, scheduling | Later | Separately gated extension |

## Document 11: roadmap, WBS and handover

| Artefact | Status | Where |
|---|---|---|
| Handover checklist H01–H24 | Built (record) | `handover-registers.md` §1: 6 evidence ready, 1 gate ready, 9 partial, 8 open |
| RAID R-01..R-16, decisions DEC-01..16, change register | Built (record) | `handover-registers.md` §2–4 |
| Owner roster | Partial | `handover-registers.md` §5; functions from the RACI, names PENDING |
| STRIDE + OWASP LLM threat model (F01) | Built | `threat-model.md`, `owasp-llm-map.md` |
| Data inventory and retention map (F04) | Built | `data-inventory.md` (values PENDING DEC-06) |
| Runbooks, drills, ADRs | Built | `RUNBOOK.md` RB-01..05, `cmd/drill`, ADR-001..008 |
| Sign-offs (§24) | Blocked | All PENDING |

## Doc 02 conversation scripts (§10, §18, §23)

All ten scripts and the three behaviours that were parked now pass (`internal/acceptance/doc02_scripts_test.go`; nothing remains in `_parked/`):

| Script | Behaviour |
|---|---|
| S04 | After a records refusal, a company name or record number sent next is refused again and never repeated |
| S05 | "Can ZoikoSuite file payroll taxes in France, and what should I pay my contractor?": regulated advice (X04) leads, payroll (I18) is logged as the secondary intent |
| S07 | See Doc 06 above |
| S10 | "Can someone show me…" offers the demo route; "Yes" opens the official form, with no claim that anything was booked |
| §10 market | A country the visitor names is kept for the session (`sessions.market_code`) and used for later price and trial questions; a message that only names a country is acknowledged without implying coverage |
| §23 acronyms | SSO, SCIM, SoD, DPA, SOC 2 and MFA are expanded as general definitions, never as ZoikoSuite claims |
| §08 secondary topic | A second topic in one question is offered as one chip instead of being answered |

## Still not built

- Image signing, provenance and digest pinning (need a registry and key).
- Restore drill (the synthetic canary is `cmd/canary`).
- Holdout and human grading lanes; real reviewer handles; every sign-off.
