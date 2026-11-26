# Blocks 6–10 (37 services): final verification report — 2026-11-02

Written for: the Blocks 6–10 owner (vasudevareddy-zoiko) and reviewers of branch `fix/blocks-6-10-final`.

**State:**
- **Worktree:** `E:/zwork/backend-final`
- **Branch:** `fix/blocks-6-10-final`
- **Base:** origin/main `f4a9d9f5`; the branch is 0 commits behind at the time of writing.
- **Commit status:** nothing is committed, pushed or merged. `main` was not touched.
- **Backups kept:**
  - `E:/zwork/backend` (`fix/blocks-6-10`)
  - `E:/zwork/backend810` (`fix/blocks-8-10`)
  - the main tree with its 188 WIP entries
  - removed migrations under `E:/zaudit/removed-migrations/`
- **Other sessions:** the frontend session's mock containers were never stopped or touched.

Mock and stub results are reported separately from real-container results. **No production URL exists and nothing here is production-verified.**

---

## 1. Headline

| Measure | Result |
|---|---|
| Services whose backend meets **every backend acceptance criterion**, with no open defect (excluding frontend, external providers and merge) | **26 / 37** |
| Services that meet the **full 100% acceptance criteria** (also frontend verified end-to-end, required external providers real, merged) | **0 / 37** |
| Backend completion estimate (8-dimension method, §8) | **≈ 92%** (was reported 85%; see §8 for why the number moved) |
| Real-container verification (repo Dockerfiles, compose env, per-service DB roles, Traefik) | **37 / 37 services up healthy; every suite green except the reconciliation-intelligence deep suite (contract dispute, §4)** |

**Why the earlier report said 85% and this one says ≈92%:** the earlier figure was an estimate taken before the migrations were proven, before the real containers ran, before live authz denial was tested on Blocks 6–7, and before eight further defects were fixed.

**About "37 vs 38" test counts:** 38 = the 37 in-scope services plus **intercompany-accounting-svc**. It is not one of your 37, but its list-authorization hole was fixed and tested here (Phase 2 finding F1).

**Correction to the 2026-11-02 morning status report:** it reported the Block 8–10 functional checks as "232/232". The correct total is **218/218**: 15 services × 14 checks, plus 8 for esignature. That was an arithmetic error in the report, not a change in results.

---

## 2. 37-service completion matrix

**Legend:**
- **Impl** — implementation meets the service contract.
- **Tests** — Go unit and integration tests pass (DB tests run against Postgres 16 as NOSUPERUSER, 0 skips).
- **Mig** — upgrade from main, rollback, re-apply and clean install all succeed.
- **Sec** — tenant isolation, real authz 403, legal-entity 403 and FORCE RLS, all verified live.
- **Native** — live checks against native processes with the policy authz stub.
- **Container** — the same checks against real containers, plus the gateway.
- **FE** — frontend integration verified end-to-end.
- **Provider** — real external provider, where the service needs one.
- **Backend done** — every backend criterion met with no open defect.

✔ = evidence recorded; ◐ = partial; ✖ = not met; — = not applicable.

| # | Service | Impl | Tests | Mig | Sec | Native | Container | FE | Provider | Backend done |
|---|---|---|---|---|---|---|---|---|---|---|
| 6.1 | contract-lifecycle | ✔ | ✔ | ✔ | ✔ | ✔ legal 229 | ✔ legal 229 | ◐ peer branch, not e2e | — | ✔ |
| 6.2 | clause-template | ✔ | ✔ | ✔ | ✔ | ✔ | ✔ | ◐ | — | ✔ |
| 6.3 | obligation-tracking | ✔ | ✔ | ✔ | ✔ | ✔ | ✔ | ◐ | — | ✔ |
| 6.4 | board-resolutions | ✔ | ✔ | ✔ | ✔ | ✔ | ✔ | ◐ | — | ✔ |
| 6.5 | corporate-actions | ✔ | ✔ | ✔ | ✔ | ✔ | ✔ | ◐ (create payload mismatch) | — | ✔ |
| 6.6 | counterparty-management | ✔ | ✔ | ✔ | ✔ | ✔ | ✔ | ◐ | — | ✔ |
| 6.7 | purchase-request | ✔ | ✔ 75 | ✔ | ✔ (outbox RLS fixed) | ✔ proc 134 | ✔ proc 134 | ◐ | — | ✔ |
| 6.8 | purchase-order | ✔ | ✔ 97 | ✔ | ✔ (outbox RLS fixed) | ✔ | ✔ | ◐ | — | ✔ |
| 6.9 | spend-controls | ✔ | ✔ | ✔ | ✔ | ✔ | ✔ | ◐ | — | ✔ |
| 6.10 | vendor-due-diligence | ✔ | ✔ | ✔ | ✔ | ✔ | ✔ | ◐ | — | ✔ |
| 6.11 | procurement-workflow | ✔ | ✔ | ✔ | ✔ | ✔ | ✔ | ◐ | — | ✔ |
| 7.1 | tax-rules | ✔ | ✔ | ✔ | ✔ | ✔ tax 269 | ✔ tax 269 | ◐ | — | ✔ |
| 7.2 | tax-determination | ✔ | ✔ | ✔ | ✔ | ✔ | ✔ | ◐ | — | ✔ |
| 7.3 | vat-gst | ✔ | ✔ | ✔ | ✔ | ✔ | ✔ | ◐ | — | ✔ |
| 7.4 | corporate-tax | ✔ (list envelope + GL port fixed) | ✔ | ✔ | ✔ | ✔ | ✔ | ◐ | — | ✔ |
| 7.5 | withholding-tax | ✔ | ✔ | ✔ | ✔ | ✔ | ✔ | ◐ | — | ✔ |
| 7.6 | filing-preparation | ✔ | ✔ | ✔ | ✔ | ✔ | ✔ | ◐ (port now 8176) | — | ✔ |
| 7.7 | filing-tracker | ✔ | ✔ | ✔ | ✔ | ✔ | ✔ | ◐ | — | ✔ |
| 7.8 | compliance-status | ✔ | ✔ | ✔ | ✔ | ✔ | ✔ | ◐ | — | ✔ |
| 7.9 | exception-escalation | ✔ | ✔ | ✔ | ✔ | ✔ | ✔ | ◐ | — | ✔ |
| 8.1 | anomaly-detection | ✔ | ✔ 38 | ✔ | ✔ | ✔ 14/14 | ✔ 14/14 | ✖ wrong paths | — | ✔ |
| 8.2 | forecasting | ✔ | ✔ 43 (rollback bug fixed) | ✔ | ✔ | ✔ | ✔ | ✖ | — | ✔ |
| 8.3 | compliance-risk-scoring | ✔ | ✔ 29 | ✔ | ✔ | ✔ | ✔ | ✖ | — | ✔ |
| 8.4 | reconciliation-intelligence | ◐ (SoD defect open) | ✔ 132 | ✔ | ◐ | ✔ deep 19/93 | ✔ deep 19/93 | ✖ | — | ✖ (owner decisions, §4) |
| 8.5 | reporting-orchestration | ◐ (report runs NOT_IMPLEMENTED) | ✔ 38 | ✔ (down added) | ✔ | ✔ deep 154/154 | ✔ deep 154/154 | ✖ | — | ✖ |
| 8.6 | decision-support | ✔ | ✔ 85 | ✔ | ✔ | ✔ | ✔ | ✖ (no client) | — | ✔ |
| 8.7 | migration-integrity | ✔ | ✔ 67 | ✔ | ✔ | ✔ deep 81/81 | ✔ deep 81/81 | ✖ | — | ✔ |
| 9.1 | mtls-management | ✔ (Postgres) | ✔ 102 | ✔ | ✔ | ✔ | ✔ | ✖ | — (internal CA) | ✔ |
| 9.2 | siem-integration | ◐ (no external SIEM delivery) | ✔ 60 | ✔ | ✔ | ✔ | ✔ | ✖ | ✖ | ✖ |
| 9.3 | carta | ✔ (Postgres) | ✔ 48 | ✔ | ◐ (`/evaluate` has no principal check, row 84d) | ✔ | ✔ | ✖ | — | ✖ |
| 9.4 | key-management | ◐ (metadata only, no crypto) | ✔ 64 | ✔ | ✔ | ✔ | ✔ | ✖ | ✖ KMS | ✖ |
| 10.1 | connectivity-api-bridge | ✔ | ✔ 845 (1 skip) | ✔ | ✔ | ✔ | ✔ (authz URL fixed) | ✖ | ◐ no real connector exercised | ✖ |
| 10.2 | banking-connector | ✔ (Dockerfile + residency fixed) | ✔ 81 | ✔ | ✔ | ✔ | ✔ | ✖ | ✖ bank APIs | ✖ |
| 10.3 | hris-connector | ✔ | ✔ 31 | ✔ | ✔ | ✔ | ✔ | ✖ | ✖ HRIS provider | ✖ |
| 10.4 | tax-authority-interface | ✔ (fail-closed) | ✔ 111/115 | ✔ | ✔ | ✔ | ✔ | ✖ | ✖ HMRC | ✖ |
| 10.5 | esignature-integration | ◐ (fail-closed, no adapter) | ✔ 58 | ✔ | ✔ | ✔ 8/8 | ✔ 8/8 | ✖ | ✖ DocuSign/Adobe | ✖ |
| 10.6 | external-data-feed | ◐ (no outbound client) | ✔ 25 | ✔ | ✔ | ✔ | ✔ | ✖ | ✖ market data | ✖ |

**Backend done: 26.** Blocks 6 and 7 account for all 20 of their services; Block 8 adds anomaly, forecasting, compliance-risk-scoring, decision-support and migration-integrity (5); Block 9 adds mtls-management (1).

**Not backend done: 11.**
- reconciliation-intelligence and reporting-orchestration
- siem-integration, carta and key-management
- all six Block 10 services, which need real provider adapters or a decision that none are in scope

---

## 3. Defects fixed on this branch (with test evidence)

### Fixed in this verification pass

| # | Service | Defect | Severity | Fix | Evidence |
|---|---|---|---|---|---|
| 1 | purchase-request, purchase-order | `outbox_events` had **no RLS**, the only unprotected tenant tables among the 37 | High | Migration `000008_outbox_rls` (ENABLE + FORCE, tenant policy plus explicit `app.outbox_relay` exception); both relays declare the flag | New tests run the relay **as the restricted role**. An unscoped session sees 0 rows; a cross-tenant insert is refused. Red/green: without the flag the relay test fails. Live procurement 134/134 |
| 2 | 10 Block 6–7 services | Branch `force_rls` migrations duplicated main's (PR #243); **their rollbacks would have removed main's FORCE RLS** | High | Removed the 10 redundant pairs (backed up) | Rollback is now schema-exact against main in those services |
| 3 | reconciliation-intelligence, compliance-risk-scoring | Down migrations also un-FORCEd tables that main forces | High | Down files leave main's FORCE in place | Schema comparison after rollback |
| 4 | forecasting | Rollback dropped columns the remaining trigger still reads, so **every update would fail after a rollback** | High | The down file restores the 000002 trigger function first | Schema comparison: function now identical |
| 5 | reporting-orchestration | `000003` had no down migration | Medium | Down migration added | Rollback is schema-exact (0 diff lines) |
| 6 | migration-integrity | Dockerfile copied prebuilt binaries, so **the image could not be built** (pre-existing on main) | High | Multi-stage build plus `.dockerignore`, context `services/` | Image builds; container healthy |
| 7 | banking-connector | Dockerfile lacked the sibling `eventing` module, so the image could not be built (pre-existing on main) | High | Same | Image builds; container healthy |
| 8 | reconciliation-intelligence, migration-integrity, banking-connector | Compose build contexts did not match the Dockerfiles | High | Contexts fixed | Images build |
| 9 | Block 10 (6 services) | Compose pointed authz at `host.docker.internal:8081` (copied from phase7), so **every request would 503 in the main stack** | Critical | `http://authorization-svc:8089`; vault URL likewise | Container run: 403 and 201 as expected |
| 10 | banking-connector, tax-authority-interface | `EVENT_RESIDENCY_REGION` unset, so **every banking create returned 500** in containers | Critical | Set (`${EVENT_RESIDENCY_REGION:-local}`) | Container functional 14/14 |
| 11 | reconciliation-intelligence | Required `EVENT_RESIDENCY_REGION` missing, so **the service would not start** | Critical | Set | Container healthy |
| 12 | reporting-orchestration | Audit-event-store URL defaulted to `:8080` (the service listens on 8084), so **audit exports always failed** | High | Explicit URL | Container deep suite 154/154 |
| 13 | anomaly-detection | Jurisdiction URL defaulted to `localhost:8125` | Medium | Explicit URL | Default-URL check: 0 issues |
| 14 | corporate-tax, vat-gst | `GENERAL_LEDGER_URL` pointed at port 8091; general-ledger listens on 8098 (from PR #243) | High | Compose and code default set to 8098 | Default-URL check: 0 issues |
| 15 | purchase-request | Compose connected as `zoiko_app`, but `create-app-roles.sh` revokes PUBLIC CONNECT, so **it could not connect on a fresh volume** (pre-existing on main) | Critical | Uses `app_purchase_request` | Container: connects as `app_purchase_request` |
| 16 | reconciliation-intelligence | Raw Postgres error (SQLSTATE 22P02) leaked in a 404 body; decisions allowed on ARCHIVED jobs; oversized body returned 400 instead of 413 | Medium | UUID validation and an error sentinel; 409 on archived; 413 | 3 new tests |
| 17 | 4 files | Not gofmt-clean | Low | gofmt (whitespace only) | — |

### Fixed earlier on this branch (2026-10-27 to 11-02)
- intercompany list authorization
- corporate-tax list envelope
- reconciliation-intelligence: job_name 500, unbounded list, filter validation
- idempotency on 18 services
- Block 9 moved from memory to Postgres
- esignature fake SENT replaced with fail-closed 503
- esignature route shadowing
- probes public on all 37
- outbox RLS (anomaly-detection, compliance-risk-scoring, reporting-orchestration)
- reporting cross-tenant authz cache leak
- the 16 host-port collisions
- 20 services added to main compose, and gateway routes for all 37
- migration loader for plain `NNN_name.sql` files

---

## 4. Remaining defects and open decisions

| # | Service | Item | Severity | Owner / blocker |
|---|---|---|---|---|
| R1 | reconciliation-intelligence | The analysis requester can **approve its own suggestion** (no SoD); needs a `requested_by` column | High | satyaprakash: schema change on his service |
| R2 | reconciliation-intelligence | 54 contract differences against the earlier rewrite: problem+json errors, versioning and If-Match, malformed-id 400, body `tenant_id`; plus 5 design decisions and 2 outdated tests | Medium | Agreement with satyaprakash (plan: `reconciliation-intelligence-plan-2026-11-02.md`) |
| R3 | carta | `/v1/carta/evaluate` needs only a tenant: no principal check and no authz (row 84d) | High | Needs mTLS service identity (platform decision) |
| R4 | key-management | No cryptographic operations; metadata only (row 82) | High | KMS architecture decision |
| R5 | siem-integration | Nothing is delivered to an external SIEM; `/stream` is tenant-header-only | High | Provider decision plus mTLS identity |
| R6 | reporting-orchestration | Report runs are `NOT_IMPLEMENTED` | Medium | Source-service data contracts |
| R7 | Block 10 | No real provider adapters (bank, HRIS, HMRC, e-signature, market data) | High | Credentials and sandboxes (`blocks-6-10-external-integrations-checklist.md`) |
| R8 | All 37 | Frontend still calls wrong paths and payloads for Blocks 8–10; Block 6–7 finance payload mismatches | High | Frontend session (`blocks-8-10-frontend-contract-handoff.md`) |
| R9 | Platform, outside the 37 | **ai-governance migrations fail on a fresh DB on main** (`000006_aig05` creates `ai_incidents` again), so **the main stack's Postgres cannot initialise from scratch** | Critical | ai-governance owners (satyaprakash / rohithyadav7) |
| R10 | Platform, outside the 37 | evidence-requirements and document-vault connect as `zoiko_app` but their databases admit only their app role, so they can't connect on a fresh volume; goods-service-receipt and payment-run connect as superuser (RLS inert) | Critical / High | Their owners |
| R11 | Platform, outside the 37 | 12 services still connect as the Postgres superuser in compose (payment-*, supplier-*, …) | High | Their owners |
| R12 | All | Kafka delivery not verified (outboxes hold events) | Medium | Needs a Kafka-backed run |
| R13 | All | Real authorization-svc policies not exercised; a policy-driven stub was used | Medium | Seed authorization-svc policies for the 37 actions |

---

## 5. External dependencies and decisions
See `blocks-6-10-external-integrations-checklist.md`. In short:
- **E-signature:** choose DocuSign or Adobe; sandbox; JWT key; webhook HMAC secret.
- **HMRC MTD:** Developer Hub app; sandbox VRN; fraud-prevention headers.
- **Market data:** choose a source (ECB is free); licences for Refinitiv or Bloomberg.
- **Key management:** choose a KMS and decide the crypto-boundary architecture.

None of these is marked complete.

---

## 6. Test results summary (all commands re-runnable; scripts in `E:/zaudit/`)

| Check | Native (processes, `zoiko_app`) | Real containers (`zverify` stack, per-service roles) |
|---|---|---|
| Go build + vet + tests, 37 + intercompany | **38/38 pass** (`final_go.sh`) | — |
| DB integration (per service, Postgres 16, NOSUPERUSER, `TEST_DATABASE_URL`) | All pass, 0 fail; 1 skip in connectivity-api-bridge (not identified) | — |
| Migrations: upgrade from main, rollback, re-apply, clean (34 services with new migrations) | **34/34**; rollback schema-exact **30/34**, the other 4 stricter-only (§3) | Fresh `init-db.sh` + `create-app-roles.sh` on 40 DBs; 37 app roles provisioned |
| Startup and health | 40/40 alive; `/healthz` + `/readyz` 200 without headers | **41/41 healthy** via the repo HEALTHCHECKs |
| Legal suite | 229/229 | **229/229** |
| Tax suite | 269/269 | **269/269** |
| Procurement suite | 134/134 | **134/134** |
| Intelligence deep suite | reporting 154/154 · migration 81/81 · recon 19/93 | reporting 154/154 · migration 81/81 · recon 19/93 |
| Block 8–10 functional (create, read, list, cross-tenant 404, deny 403, missing tenant, invalid body, unknown id, replay, key reuse) | **218/218** | **218/218** |
| Restart persistence | 15/15 (process kill) | **15/15** (`docker restart`) |
| Denied-principal sweep, Blocks 6–7 (no write succeeds; real 403 each) | **20/20** | **20/20** |
| Legal-entity denial, Blocks 8–10 | **14/14** | **14/14** |
| RLS: tenant tables with ENABLE + FORCE + policy | 201/203 before fix #1, 203/203 after | **203/203** |
| DB roles actually used | `zoiko_app` (NOSUPERUSER, NOBYPASSRLS) | **35 of 37 on dedicated `app_<db>`**, 2 on `zoiko_app`, **0 superuser** (`pg_stat_activity`) |
| Gateway (Traefik, generated routes): probes / round trips | 74/74 / 61/61 | **74/74 / 61/61** |

Stub usage, kept explicit:
- **Authorization:** both runs use a policy-driven stub (`authz_policy.py`: deny by principal, action or entity). It is not allow-all.
- **Out-of-scope dependencies** (bank-reconciliation, audit-event-store, retention, governance and others) are e2e fixtures.
- **Container run only:** evidence-requirements' tenant-registry lookup goes to the stub, as it did in the native run.

---

## 7. Real-container verification report
- **Stack:** `E:/zaudit/zverify/docker-compose.json`, generated from `docker compose config` of this branch's main compose by `E:/zaudit/gen_verify.py`.
  - Project `zverify`, containers `zv-*`, host ports 29101+, Postgres on 15433, gateway on 18180, private network.
  - **No container, port or volume shared with the frontend session's mock stack.**
- **Images:** the repo's own Dockerfiles for all 37 (`zaudit-real-*`). 3 failed to build before the fixes above; 37/37 build now.
- **Database:** the repo's `init-db.sh` and `create-app-roles.sh`, with the database list filtered to the 40 services under test. The filter is needed because of R9: the full list fails on main.
- **Defects found only by containers:** #6–#15 in §3. Five of them would have stopped services from starting or serving in a real deployment.
- **Not in the stack:** Kafka, the real authorization-svc, and the 70 other platform services.

---

## 8. Completion percentage and how it is calculated
- **Method:** each service scores 0, 0.5 or 1 on each of 8 dimensions, and the result is the average.
  - Dimensions: implementation, config/container, DB/migrations, authN/Z, tenant isolation, API contract (including frontend alignment), tests, live verification.
  - Per block: **B6 ≈ 94%, B7 ≈ 94%, B8 ≈ 92%, B9 ≈ 89%, B10 ≈ 88%.**
  - Weighted by service count: **≈ 92%**.
- **What holds it below 100%:**
  - API contract scores only 0.5 everywhere, because the frontend is not aligned or verified end-to-end.
  - Implementation is partial where a provider or feature is missing (§4 R1–R7).
- **Kept separate from that percentage:**
  - **0/37 merged**, and **0/37 at the full 100% acceptance criteria**.
  - **26/37 backend done.**

---

## 9. Proposed commit (for review; not committed)

**Two commits are suggested:**
```
fix(blocks-6-10): complete and verify 37 services — persistence, RLS, idempotency, probes, contracts

- Block 8: finish the intelligence rewrite (anomaly, forecasting, risk scoring, reporting, decision support,
  migration integrity); outbox RLS; reporting authz cache keyed by tenant; export build lease
- Block 9: mtls, siem, carta, key-management move from memory to Postgres (FORCE RLS, transactions, idempotency);
  siem tokens sealed at rest; mtls never stores or replays private keys
- Block 10: public probes, route-contract tests, idempotency; esignature fails closed without a provider;
  tax-authority adapter routes 503 when disabled; esignature route-shadowing fix
- Blocks 6-7: idempotency on 18 services, tax/legal/procurement e2e defects, corporate-tax list envelope,
  purchase-request/order outbox RLS, corporate-actions resolution verification
- intercompany: list authorized per source entity (was unfiltered without an entity filter)
- reconciliation-intelligence: job_name bound, bounded paged list, filter validation, no DB error leak,
  no decisions on archived jobs, 413 on oversized body
- migrations: renumbered above main, redundant force_rls removed, rollbacks fixed (forecasting trigger,
  reporting 000003 down, keep main's FORCE)

fix(deployments): main compose runs Blocks 6-10 for real

- add 20 phase-only services to docker-compose.yml with per-service app roles; host ports 8190-8205 resolve
  16 collisions (container ports unchanged; filing-preparation 8176, risk scoring 8177 as on main)
- gateway routes for all 37; ROUTES.md shows container and host ports
- init-db.sh / migrate.py apply plain NNN_name.sql migrations too
- Dockerfiles: migration-integrity and banking-connector build from source (were unbuildable)
- env: Block 10 authz/vault URLs, residency regions, reporting/anomaly/recon dependency URLs,
  GL port 8098, purchase-request app role
```
Diff: 237 tracked files changed (+10,713 / −9,727), 381 new files (104 test files, 93 migration files). No binaries, no credentials (scanned), all text files valid UTF-8.

**Docs added:**
- `blocks-8-10-frontend-contract-handoff.md`
- `blocks-6-10-external-integrations-checklist.md`
- `reconciliation-intelligence-plan-2026-11-02.md`
- this report

---

## 10. Services that genuinely meet the 100% acceptance criteria
**None (0/37).**
- The 26 "backend done" services fail the 100% bar only on frontend end-to-end verification and on merge status.
- The other 11 also need §4 items R1–R7.
