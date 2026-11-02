# Zoiko Suite Blocks 6–10: end-to-end backend acceptance report (2026-11-02)

**Who this is for:** the Blocks 6–10 owner (vasudevareddy-zoiko), the owners of the affected services, and reviewers of branch `fix/blocks-6-10-final`.

## Branch and working state

| Item | Value |
|---|---|
| Worktree | `E:/zwork/backend-final` |
| Branch | `fix/blocks-6-10-final` |
| Base | origin/main `f4a9d9f5` |
| Committed so far | `37d5b869`, `b53d29ef` (pushed earlier; PR #245 closed at the owner's request, branch kept) |
| Work in this pass | **uncommitted**. Nothing was pushed, no PR was opened, `main` was not touched. |
| Other sessions | No backups, worktrees or volumes were removed. The frontend session's containers and repo were not touched. |

Mock and stub results are kept separate from real results. No production URL exists, so **nothing here is production-verified**.

**Earlier report, now superseded:** `docs/architecture/blocks-6-10-verification-2026-11-02.md`. Its §3 lists the defects fixed before this pass, and this report does not repeat them.

---

## 1. Summary

| Measure | Result |
|---|---|
| Services in scope | 37, plus intercompany-accounting (fix only) |
| **Accepted**: backend meets every acceptance criterion, with no open backend defect | **26** |
| **Partial**: backend works but has an internal gap | **3**: reporting-orchestration, mtls-management, connectivity-api-bridge |
| **Blocked**: waits on an external provider or a platform decision | **8**: siem-integration, carta, key-management, banking-connector, hris-connector, tax-authority-interface, esignature-integration, external-data-feed |
| Full 100% criteria (also frontend verified end-to-end and merged) | **0 / 37**. The frontend is not aligned, and nothing is merged. |
| Full-stack Docker verification of the main compose | **Not declared.** ai-governance migrations still fail on a fresh database (§7, B1). |
| Tests run in this pass | All suites listed in §5 pass, with 0 failures. Skips are listed with their reasons. |
| Critical/High defects found and fixed in this pass | 8 (§3) |
| Open Critical/High items | 1 Critical (ai-governance, outside the 37); High: external providers, frontend alignment, carta principal check, KMS crypto, SIEM delivery, Block 9 audit events (§7) |

**Is the end-to-end acceptance bar met?** **No.** These blockers remain:
- The backend of 26 services is accepted.
- reconciliation-intelligence reaches 93/93. It moves to Accepted on the condition that satyaprakash signs off on the contract changes in §3.1.
- The external providers are not connected.
- Block 9 emits no audit events.
- The frontend has not been aligned with the backend contracts.
- ai-governance stops the main stack from initialising on a fresh database.

---

## 2. 37-service matrix

**Legend:** ✔ evidence recorded · ◐ partial · ✖ not met · — not applicable.

**Column meanings:**
- **Tests:** unit tests plus DB-backed tests on Postgres 16 as a NOSUPERUSER role.
- **Mig:** upgrade from main, rollback, re-apply and clean install all succeed.
- **Sec:** tenant isolation, a real 403 from authz, a 403 for the wrong legal entity, and FORCE RLS.
- **Container:** the real container suites and the gateway.
- **Events:** outbox → Kafka → audit-event-store, with stored records counted in the audit DB.
- **FE:** frontend verified end-to-end.

| # | Service | Tests | Mig | Sec | Container | Events | FE | Provider | Status |
|---|---|---|---|---|---|---|---|---|---|
| 6.1 | contract-lifecycle | ✔ | ✔ | ✔ | ✔ legal 229/229 | ✔ 11/11 | ◐ | — | **Accepted** |
| 6.2 | clause-template | ✔ | ✔ | ✔ | ✔ | ✔ 19/19 | ◐ | — | **Accepted** |
| 6.3 | obligation-tracking | ✔ | ✔ | ✔ | ✔ | ✔ 20/20 | ◐ | — | **Accepted** |
| 6.4 | board-resolutions | ✔ | ✔ | ✔ | ✔ | ✔ 12/12 (fixed) | ◐ | — | **Accepted** |
| 6.5 | corporate-actions | ✔ | ✔ | ✔ | ✔ | ✔ 4/4 | ◐ | — | **Accepted** |
| 6.6 | counterparty-management | ✔ | ✔ | ✔ | ✔ | ✔ 15/15 | ◐ | — | **Accepted** |
| 6.7 | purchase-request | ✔ integ 80 | ✔ | ✔ | ✔ proc 134/134 | ✔ 15/15 | ◐ | — | **Accepted** |
| 6.8 | purchase-order | ✔ integ 102 | ✔ | ✔ | ✔ | ✔ 18/18 | ◐ | — | **Accepted** |
| 6.9 | spend-controls | ✔ | ✔ | ✔ | ✔ | ✔ 4/4 | ◐ | — | **Accepted** |
| 6.10 | vendor-due-diligence | ✔ | ✔ | ✔ | ✔ | ✔ 4/4 | ◐ | — | **Accepted** |
| 6.11 | procurement-workflow | ✔ | ✔ | ✔ | ✔ | ✔ 6/6 | ◐ | — | **Accepted** |
| 7.1 | tax-rules | ✔ | ✔ | ✔ | ✔ tax 269/269 | ✔ 5/5 (fixed) | ◐ | — | **Accepted** |
| 7.2 | tax-determination | ✔ | ✔ | ✔ | ✔ | ✔ 5/5 | ◐ | — | **Accepted** |
| 7.3 | vat-gst | ✔ | ✔ | ✔ | ✔ | ✔ 4/4 (fixed) | ◐ | — | **Accepted** |
| 7.4 | corporate-tax | ✔ | ✔ | ✔ | ✔ | ✔ 6/6 | ◐ | — | **Accepted** |
| 7.5 | withholding-tax | ✔ | ✔ | ✔ | ✔ | ✔ 8/8 (fixed) | ◐ | — | **Accepted** |
| 7.6 | filing-preparation | ✔ | ✔ | ✔ | ✔ | ✔ 6/6 | ◐ | — | **Accepted** |
| 7.7 | filing-tracker | ✔ | ✔ | ✔ | ✔ | ✔ 8/8 | ◐ | — | **Accepted** |
| 7.8 | compliance-status | ✔ | ✔ | ✔ | ✔ | ✔ 5/5 | ◐ | — | **Accepted** |
| 7.9 | exception-escalation | ✔ | ✔ | ✔ | ✔ | ✔ 24/24 | ◐ | — | **Accepted** |
| 8.1 | anomaly-detection | ✔ | ✔ | ✔ | ✔ 14/14 | ✔ 1/1 (fixed) | ✖ | — | **Accepted** |
| 8.2 | forecasting | ✔ | ✔ | ✔ | ✔ | ✔ 1/1 (fixed) | ✖ | — | **Accepted** |
| 8.3 | compliance-risk-scoring | ✔ | ✔ | ✔ | ✔ | ✔ 1/1 (fixed) | ✖ | — | **Accepted** |
| 8.4 | reconciliation-intelligence | ✔ unit 138, integ 165 | ✔ (000011) | ✔ SoD, tenant predicate | ✔ **deep 93/93** (was 19/93) | ✔ 16/16 | ✖ | — | **Accepted, pending owner sign-off on contract (§3.1)** |
| 8.5 | reporting-orchestration | ✔ | ✔ | ✔ | ✔ deep 154/154 | ✔ 66/66 (fixed) | ✖ | — | **Partial**: report runs recorded `NOT_IMPLEMENTED` |
| 8.6 | decision-support | ✔ | ✔ | ✔ | ✔ | ✔ 1/1 | ✖ | — | **Accepted** |
| 8.7 | migration-integrity | ✔ integ 83 | ✔ | ✔ | ✔ deep 81/81 | ✔ 19/19 | ✖ | — | **Accepted** |
| 9.1 | mtls-management | ✔ | ✔ | ✔ | ✔ | ✖ no topic | ✖ | — (internal CA) | **Partial**: no audit events |
| 9.2 | siem-integration | ✔ | ✔ | ✔ | ✔ | ✖ no topic | ✖ | ✖ SIEM | **Blocked** |
| 9.3 | carta | ✔ | ✔ | ◐ `/evaluate` has no principal check | ✔ | ✖ no topic | ✖ | — | **Blocked** (mTLS identity decision) |
| 9.4 | key-management | ✔ | ✔ | ✔ | ✔ | ✖ no topic | ✖ | ✖ KMS | **Blocked** (metadata only) |
| 10.1 | connectivity-api-bridge | ✔ | ✔ | ✔ | ✔ | ✔ 1/1 | ✖ | ◐ no real connector exercised | **Partial** |
| 10.2 | banking-connector | ✔ integ 95 | ✔ | ✔ | ✔ | ✔ 1/1 | ✖ | ✖ bank APIs | **Blocked** |
| 10.3 | hris-connector | ✔ | ✔ | ✔ | ✔ | ✔ 1/1 (fixed) | ✖ | ✖ HRIS | **Blocked** |
| 10.4 | tax-authority-interface | ✔ integ 115 | ✔ | ✔ | ✔ | ✔ 1/1 (fixed) | ✖ | ✖ HMRC | **Blocked** |
| 10.5 | esignature-integration | ✔ | ✔ | ✔ | ✔ 8/8 | — fails closed, so no events (fixed field) | ✖ | ✖ DocuSign/Adobe | **Blocked** |
| 10.6 | external-data-feed | ✔ | ✔ | ✔ | ✔ | ✔ 1/1 (fixed) | ✖ | ✖ market data | **Blocked** |
| — | intercompany-accounting (fix only) | ✔ integ 149 | — | ✔ list authz | — | — | — | — | Fix verified |

The Events column counts events stored against events published. "(fixed)" marks a service whose events were dead-lettered by the audit consumer before this pass (§3.2).

---

## 3. Defects found and fixed in this pass

### 3.1 reconciliation-intelligence (deep suite 19/93 → 93/93)

**Owner:** satyaprakash. Each change below follows a cited platform standard. The owner still needs to review them, because together they change the API contract. The frontend impact is in `docs/architecture/blocks-8-10-frontend-contract-handoff.md` §4.

| # | Defect | Sev | Fix | Evidence |
|---|---|---|---|---|
| R-1 | The analysis requester could approve or execute its own suggestion. This breaks the Global Authorization/SoD rule "requestor self-approval … DENY". | High | `requested_by` is recorded on the job. APPROVED/EXECUTED by the requester → 403 `segregation_of_duties`; REJECTED is still allowed. The check runs **in the store transaction on the locked job row**, not only in the handler. | `TestSoD_RequesterCannotApproveOrExecute_MayReject`, `TestPgStore_SoDAndVersionAtDataBoundary` (integration, Postgres) |
| R-2 | Concurrent decisions could overwrite each other; there was no aggregate version. | High | Job `version` (+1 per decision or archive). `ETag: "N"`; `If-Match` → 412 `version_mismatch` / 400 `invalid_if_match` (Contract Standard §10). | `TestIfMatch_*`; `TestPgStore_ConcurrentDecisions_OneWins`: two parallel reviewers, exactly one wins |
| R-3 | Job and item queries relied on RLS alone. With a superuser connection, a decision reached **another tenant's job** (found by the new integration test). | High | `tenant_id = $n` is added to every job and item query and update, as defence in depth. | Cross-tenant apply and archive return `ErrJobNotFound` at the store |
| R-4 | A missing `suggestion_status` was read as APPROVED. | High | Now required; 400 if missing. | `TestApply_BodyRules` |
| R-5 | A manual decision could be recorded without a reason (ZS-SVC-Z-001 INV-18). | Medium | `resolution_notes` is required. The machine rationale is still kept. | `TestApply_BodyRules`, `fabricated_intelligence_test` |
| R-6 | Error bodies were `{error}`, not RFC 9457. | Medium | New `internal/problem` package. Every handler and middleware error is now `application/problem+json`; the legacy `error` and `message` fields are kept. | `problemCode` assertions in all new tests |
| R-7 | Archive returned only a message. The decider was not recorded. | Medium | Archive returns the job, with `archived_at`/`archived_by`, and emits a `job.archived` outbox event. Each decision records `decided_by`/`decided_at`. | `TestArchive_ReturnsJob_ThenReadOnly`; exactly one archived event in the outbox |
| R-8 | Unknown body fields were accepted. A body `tenant_id` was ignored. Path IDs were not validated. List rows could carry suggestions. | Low | DisallowUnknownFields; body tenant must equal the header (403); UUID path check (400); list omits `unmatched_items`. | `TestApply_BodyRules`, `TestAnalyze_BodyTenantMustRepeatHeader`, `TestList_OmitsSuggestions` |

**Schema:** migration `000011_job_version_requester_decisions` adds `requested_by`, `version` (CHECK ≥ 1), `archived_at`/`archived_by` (CHECK: set only when ARCHIVED), and `decided_by`/`decided_at`. Upgrade, rollback, re-apply and clean install all pass.

**Tests changed (none weakened):**
- Five existing handler tests now use an independent reviewer and send a reason. Their assertions are unchanged.
- The test that asserted "an empty note is accepted" now asserts that INV-18 refuses it. It still asserts that the machine rationale survives a decision.
- The external e2e harness `E:/zoiko-e2e/e2e_intel_recon.py` (backup `.bak-20261102`) now expects 409 for idempotency-key misuse, per Contract Standard §9/§11. It also sends an `authoritative_ref` on EXECUTED. Its step count is unchanged.

### 3.2 Event path (outbox → Kafka → audit-event-store)

| # | Defect | Sev | Service(s) | Fix | Evidence |
|---|---|---|---|---|---|
| E-1 | The audit consumer requires `schema_version`. **12 producers did not send it, so every one of their events was dead-lettered** and never reached the audit trail. | High | board-resolutions, tax-rules, vat-gst, withholding-tax, forecasting, compliance-risk-scoring, reporting-orchestration, hris-connector, tax-authority-interface, external-data-feed, anomaly-detection, esignature-integration | `schema_version: "1.0"` added beside `event_version` (additive) | Live: all published events stored; new `schema_version_test.go` in forecasting and compliance-risk-scoring |
| E-2 | 17 Blocks 6–10 topics were missing from the audit consumer's `KAFKA_TOPICS`. | High | compose (audit-event-store-svc) | Topics added in `deployments/docker-compose.yml` | Events from those topics are stored live |
| E-3 | The consumer did not watch partition changes. A topic created after the consumer started was never read. | Medium | audit-event-store-svc | `WatchPartitionChanges: true`, 5 s interval | Live run. A deterministic replay was not possible (§8). |
| E-4 | A failed DLQ publish was not retried, so the message was lost. | High | audit-event-store-svc | Up to 6 attempts with the runner backoff | `dlq_retry_test.go` (red before the fix, green after) |
| E-5 | The event ID ignored the body `event_id`, so dedupe used Kafka coordinates and a replay produced duplicates. | Medium | audit-event-store-svc | Order: header `X-Event-ID` → body `event_id` → body `id` → coordinates | Extended `extractEventID` test |

E-3 to E-5 change **audit-event-store-svc**, which is outside the 37. Its owners (rohithyadav7 / satyaprakash / Shashi) need to agree before these changes merge.

---

## 4. Security, authorization and RLS evidence

| Check | Result | How it was checked |
|---|---|---|
| RLS: tenant tables with ENABLE + FORCE + a policy | **203/203** | Real container DB, catalogue query (earlier pass; no RLS migrations changed since, except recon 000011, which adds columns only) |
| DB roles in use | 35/37 dedicated `app_<db>`, 2 `zoiko_app`, **0 superuser** | `pg_stat_activity` in the container stack |
| Cross-tenant read or write → 404 | Pass for all 37 | Block 8–10 functional 218/218, plus the legal, tax and procurement suites |
| Denied principal: no write succeeds, real 403 | 20/20 (Blocks 6–7) | `deny_sweep.py` against the policy stub |
| Wrong legal entity → 403 | 14/14 (Blocks 8–10) | `le_isolation.py` |
| SoD (recon) | Requester approve/execute → 403, at both the handler and the store | §3.1 |
| Concurrency (recon) | Exactly one of two parallel decisions wins | §3.1 |
| Integration suites as a NOSUPERUSER/NOBYPASSRLS role | **7 suites, 789 passed, 0 failed, 0 skipped** | §5 |
| Static: SQL built with `fmt.Sprintf` | **0** in the 37 + intercompany | `E:/zaudit/secscan_final.json` |
| Static: UPDATE/DELETE without an explicit `tenant_id` predicate | **90 statements in 15 services**, which rely on FORCE RLS alone (§7, M1) | same file |

**Authorization caveat:** the authz decisions came from a policy-driven stub (`authz_policy.py`: deny by principal, action or entity; not allow-all). The **real authorization-svc policies for the 37 services' actions were not exercised** (§7).

---

## 5. Test results (every number below comes from a run in this pass unless marked "earlier")

| Layer | Suite | Result |
|---|---|---|
| 1 Unit | `go test ./... -count=1 -v`, 37 + intercompany + audit-event-store, no DB env | **2,350 passed, 0 failed, 553 skipped**. The skips are DB-gated tests with no `TEST_DATABASE_URL`; the next row covers them. |
| 1–2 Unit + DB | Same suites with `TEST_DATABASE_URL` → fresh Postgres DB per service (`scan.py`) | **39/39 modules build and vet clean. 0 failures.** First pass: 2,931 passed and 14 skipped. Two suites first failed on harness setup, not on product defects. compliance-risk-scoring (7) and reporting-orchestration (17) expect a DB with every migration already applied, plus a NOSUPERUSER role, and were given an empty DB as superuser. Re-run that way: **compliance-risk-scoring 30/30**. **reporting-orchestration 38/38**, which also needs the app role to own the tables for its rolled-back DDL negative controls. The 14 skips were RLS tests waiting for `TEST_APP_DATABASE_URL`. Re-run with a NOSUPERUSER NOBYPASSRLS role: **carta 48/48, key-management 64/64, connectivity-api-bridge 845/845**. purchase-order and purchase-request are covered by the integration row: 102 and 80, 0 skips. Only skip left: connectivity `TestDump`, a developer dump utility gated on `REGISTRYTEST_DUMP`. banking-connector's 3 "migration failures" were also harness-only: `001_init.sql` and `002_add_rls.sql` have no `.up` suffix and were not applied. In full order, all 9 apply cleanly. |
| 2 Integration (`-tags integration`) | purchase-order 102 · purchase-request 80 · reconciliation-intelligence 165 · migration-integrity 83 · banking-connector 95 · tax-authority-interface 115 · intercompany-accounting 149 | **789 passed, 0 failed, 0 skipped** |
| 2 Migrations | recon 000011: upgrade from main, rollback, re-apply, clean | **pass** (34/34 services with new migrations, earlier) |
| 1 Build/vet/format | the 14 changed services: build, vet, test, gofmt on changed files | **14/14 clean** |
| 3 Service contract | recon deep suite (real container) | **93/93** (was 19/93) |
| 3 Service contract | legal 229/229 · tax 269/269 · procurement 134/134 · reporting 154/154 · migration-integrity 81/81 | all pass (container, this pass) |
| 4 Service-to-service | Block 8–10 functional 218/218 · persistence 15/15 · deny sweep 20/20 · legal-entity 14/14 | earlier pass, same stack |
| 5 Events | 32 services' published events counted in the audit DB | **all stored**. Block 9 (4) has no topic; esignature fails closed, so it emits nothing. |
| 6 Gateway | Traefik probes / round trips | 74/74 / 61/61 (earlier) |
| 7 Deployment | `zverify` container stack | **43/43 health-checked containers healthy**; gateway and 2 stubs up |

---

## 6. Deployment verification (Blocks 8–10)

- **Stack:** `E:/zaudit/zverify/docker-compose.json`, generated from this branch's main compose by `E:/zaudit/gen_verify.py`.
  - Project `zverify`, containers `zv-*`, host ports 29101+, Postgres 15433, gateway 18180, audit store 29200.
  - Kafka and the real audit-event-store are included. **No port, container or volume is shared with other sessions.**
- **Images:** the repo's Dockerfiles for all 37, plus the audit store.
- **Databases:** the repo's `init-db.sh` and `create-app-roles.sh`, **with the database list filtered**, because of B1.
- **Stubs:**
  - authz uses the policy-driven stub.
  - Out-of-scope upstreams (bank-reconciliation, GL, governance) use fixtures.
  - The reporting archive URL points at the fixture stub.

---

## 7. Open defects, blockers and decisions

| # | Item | Sev | Owner / blocker |
|---|---|---|---|
| B1 | **ai-governance**: `000005_transactional_outbox` and `000006_aig05` both create `ai_incidents`, with different shapes. The code uses the 000006 shape, which has no `tenant_id`. On a fresh DB the main stack's Postgres init fails, so **full-stack Docker cannot be declared verified**. Not changed here: choosing which definition wins is a schema decision for its owners. | Critical | rohithyadav7 / satyaprakash |
| B2 | **Block 9 has no audit events** (mtls, siem, carta, key-management): certificate, key and trust lifecycle changes never reach the audit trail. | High | Block 9 design: add an outbox and topic |
| B3 | carta `/v1/carta/evaluate` needs only a tenant (no principal, no authz) | High | mTLS service-identity decision |
| B4 | key-management performs no cryptography (metadata only) | High | KMS architecture decision |
| B5 | siem-integration has no external delivery | High | SIEM provider decision |
| B6 | Block 10 has no real provider adapters. They fail closed (503) and never return simulated success. | High | Credentials and sandboxes (`docs/architecture/blocks-6-10-external-integrations-checklist.md`) |
| B7 | The frontend still uses wrong paths and payloads; it has not been verified end-to-end | High | Frontend session (handoff doc, §4 added in this pass) |
| B8 | Real authorization-svc policies are not exercised | Medium | Seed policies for the 37 services' actions |
| M1 | 90 UPDATE/DELETE statements in 15 services filter by primary key only. That is safe under FORCE RLS with a NOSUPERUSER role (verified), but there is no defence in depth. Outbox relays (cross-tenant by design) and tax-authority provider profiles (platform-level) are expected. | Medium | Each service owner. Recommendation: add tenant predicates the way recon now does |
| M2 | Replay hazard: events stored before E-5 under coordinate IDs are duplicated when history is replayed. The test audit DB holds 680 rows but only 564 distinct payload hashes. | Medium | audit-event-store owners: one-off dedupe before enabling E-5 on existing data |
| M3 | reporting-orchestration report runs are `NOT_IMPLEMENTED` | Medium | Source-service data contracts |
| M4 | reconciliation-intelligence has two migrations numbered `000007` (pre-existing on main) | Low | Owner. Harmless with the file-name loader. |
| M5 | Outside the 37: evidence-requirements and document-vault connect as `zoiko_app` to DBs that admit only their own role. 12 services connect as superuser. | High | Their owners (unchanged from the earlier report) |

---

## 8. Limits of this verification

- **Partition-watch race (E-3):** it could not be reproduced on demand after the fix. Kafka was recreated with its volume removed, but the running producers auto-created the topics again at once, before the consumer started. The fix is verified by configuration and by the live run, not by a red/green test.
- **Production:** no production URL exists, and nothing here is production-verified.
- **External providers:** none was called. Every adapter fails closed.

---

## 9. Changes in this pass (uncommitted)

- **reconciliation-intelligence:**
  - `internal/problem` (new)
  - envelope, tenant and idempotency middleware
  - domain types
  - store (Postgres and memory)
  - handler
  - migration 000011
  - new tests `governance_rules_test.go` and `governance_pg_test.go`; 5 tests updated
- **12 producers:** `internal/events/publisher.go` adds `schema_version`; plus 2 new tests.
- **audit-event-store-svc:** `internal/kafka/runner.go` (partition watch, DLQ retry, event-ID order) and its tests.
- **deployments/docker-compose.yml:** 17 audit-consumer topics.
- **docs:**
  - `docs/architecture/blocks-8-10-frontend-contract-handoff.md` (§4 added)
  - this report

Outside the repo:
- `E:/zoiko-e2e/e2e_intel_recon.py`
- `E:/zoiko-e2e/e2e_intel_migration.py` (container name now configurable)
- `E:/zaudit/*` helper scripts

## 10. Re-running

```
# integration suites (fresh DBs, NOSUPERUSER app role)
bash E:/zaudit/integ_run.sh                         # -> E:/zaudit/integ_results.txt
# migration lifecycle
python E:/zaudit/migtest.py reconciliation-intelligence-svc
# unit + DB-backed tests for all services
SCAN_ROOT=E:/zwork/backend-final/services SCAN_OUT=scan_final.json python E:/zaudit/scan.py <svc...>
bash E:/zaudit/rerun3.sh   # compliance-risk-scoring / reporting-orchestration / banking with migrated DB + NOSUPERUSER role
# reporting-orchestration's store tests need the NOSUPERUSER role to OWN the tables (apply migrations as that role)
# carta / key-management / connectivity: also set TEST_APP_DATABASE_URL (NOSUPERUSER NOBYPASSRLS) to run their RLS tests
# static security scan output
E:/zaudit/secscan_final.json
# event path
python E:/zaudit/event_reconcile.py                 # -> E:/zaudit/event_path_final.json
# container stack
docker compose -f E:/zaudit/zverify/docker-compose.json -p zverify up -d
```

## 11. Production readiness

**Not production ready.**
- **Backend logic, isolation and persistence:** these are at acceptance level for 26 services. Recon becomes the 27th once its owner signs off on the §3.1 contract changes.
- **Release gates that remain:**
  - B1: a fresh-environment bootstrap must work.
  - B2: every service must have an audit trail.
  - B6: real provider adapters.
  - B7: frontend alignment.
  - B8: real authorization policies.
  - Owner review of the cross-team changes:
    - recon contract, owner satyaprakash
    - audit-event-store consumer, owners rohithyadav7 / satyaprakash / Shashi
