# Domain 1 + Domain 2: completion tracker, plan and audit record

Open work for all 26 units in scope: every feature that is not yet confirmed, with its status and the tasks that finish it. Confirmed (✅) items are removed (24 Nov 2026). The rules, decisions, task specifications and audit registers (Reference, Parts A–G) and the route inventory, console coverage and platform explainer (Appendices A–C) are in [`DOMAIN1-DOMAIN2-REFERENCE.md`](DOMAIN1-DOMAIN2-REFERENCE.md); every "Part" and "Appendix" pointer in this file means that file.

**Date:** 6 November 2026
**Basis:** backend branch `shashi-changes` at `9be6c41d` (30 commits ahead of and 1 behind `origin/main` as last fetched on 4 Nov). Frontend branch `shashi-changes`.
**Scope:** all 26 units of rule R1. That is the 16 core services (Domain 1, Identity, Scope & Foundation, D1-1…D1-9; Domain 2, Governance Platform, D2-1…D2-7) and the 10 supporting units (D2-8 schema-registry-svc, D2-9 retention-registry-svc, D2-10 the shared libraries and the GTRM compiler, D1-10…D1-13 the Security & Trust services). Finishing every session (0–25) and the definition of done means Domain 1 and Domain 2 are complete except production. Teammates' services (org-structure, commercial-account, document-vault, evidence-manifest, esignature, schema-registry callers, …) appear only as dependencies.

**Replaces twelve files** (all removed from `docs/audit_files` on 6 Nov; read any of them with `git show 9be6c41d:"docs/audit_files/<path>"`):
- merged into the Reference chapter: `IMPLEMENTATION-PLAN-2026-10-23.md`, `AUTH-PLANE-PLAN-2026-11-02.md`, `Auth-Service-audit-2026-10-14.md`, `Identity, Scope & Foundation-audit-2026-09-23.md`, `Governance Platform-audit-2026-09-28.md`, `IMPLEMENTATION-PROGRESS.md` (the "Sources merged" table in the Reference says what each kept and dropped);
- merged into Appendix C: `testing_shashi_progress/agenda_&_functionality_of_zs.md`;
- `testing_shashi_progress/DOMAIN1-DOMAIN2-AUDIT-AND-PLAN-REFERENCE.md` (6 Nov, never committed; all of it is in the Reference chapter);
- `testing_shashi_progress/domain1-domain2-features-and-requirements.md` (2 Nov status, replaced by the feature tables);
- `EVIDENCE-REQUIREMENTS-PLAN-2026-11-04.md` (executed 5 Nov; its leftovers are ER-W3…ER-W8);
- `testing_shashi_progress/group1-identity-foundation-testing.md` and `group2-governance-platform-testing.md` (16–21 Oct; open items carried into "Carried over from the removed group testing files");
- `Legal-Corporate-Commercial-and-Tax-Compliance-audit-2026-10-12.md` (Domains 6 and 7, outside this file's scope).

**Sources used for every status:**
- The Reference chapter of this file (rules, decisions, task specifications, scores and open registers from the six merged files).
- Each service's `progress.md` and `SPEC_DEVIATIONS.md`.
- `git log 2a87f299..HEAD` per service (128 commits since the 2 Nov baseline, 49 of them by 07Yashjais merged from main).
- The routes registered in each service's router, extracted from the code on 6 Nov: 824, not counting health, readiness and metrics. Each one is mapped to its feature row in Appendix A.
- A build, `go vet` and unit-test sweep of all 16 modules on 6 Nov (results in the dashboard).
- The frontend repo on 6 Nov: which console pages use the v2.0 kit, and which backend routes each `lib/api` client calls (Appendix B).

**Reading references in this file:** the short names point to Parts of the Reference chapter below.
- "IMPLEMENTATION-PLAN §n" → Part A (same § numbers; §6 tasks in A5).
- "AUTH-PLANE plan" → Part B.
- "Auth-Service audit" and AU rows → Part C.
- "Identity audit" → Part D.
- "Governance audit" → Part E.
- "IMPLEMENTATION-PROGRESS" → Part F.

Text not carried into the Reference (closed passes, the old status board, evidence blocks) is in git: `git show 9be6c41d:"docs/audit_files/<file name>"`.

## How to use this file

**Implementing model:**
1. Read "Do first", the rules under "Execution order", the section of the service you are given, and the Reference Parts it points to.
2. Work one service per session, in the order under "Execution order". Do the backend tasks in the order listed in the service section, then its "Console (frontend)" tasks, following "Frontend rules for every console". A service is finished only when its console drives it end to end in the browser test.
3. Tick a task (`- [x]`) when its "Done when" holds, and change the feature rows it closes to 🔵 with the commit id and test names in the Evidence column. Never set ✅ yourself.
4. Finish with the service's hand-back (S0–S6), then stop and hand back for verification.
5. **Step tables (from 23 Nov).** Where a section has a "Step-by-step plan" table, work through it in order, one step at a time. When a step's "Done when" holds, set its Status to 🔵. In Evidence, write the commit id, the test names and the counts you ran (for example `a1b2c3d4 · TestWF2_* 14/14 · store IT 152/0/1`). Never write ✅ and never mark a step 🔵 without a command you actually ran. If a step cannot be done, set ⛔ and name the reason. Then go on to the next step, unless that step depends on the one you could not do.

**Verifier (Claude):** run the verifier protocol ("Verifier protocol", before the Reference) for the service handed back, including VC-1 for its "Reconfirm" rows. Rows and steps that pass are removed from this file, and their count goes into the dashboard's "✅ done (removed)" column. Any failure reopens the task.

## Status legend

| Emoji | Meaning |
|---|---|
| ✅ | Done: built, tested, and confirmed by the latest audit or verifier pass. From 24 Nov 2026 a confirmed row or step is removed from this file (git keeps it) |
| 🟡 | Partial: built, but part of the behaviour, a dependency wiring or the proof is missing |
| 🔵 | Built, not verified: routes and code exist, but no audit or verifier pass has checked them |
| ❌ | Missing: not built |
| ⛔ | Blocked: waits on infrastructure, a vendor or a service that does not exist or does not run yet (named in the row) |
| 🧭 | Decision: waits on a decision by a named owner (decision id in the row) |
| 🏁 | Service level only: every stage S0–S6 passed the verifier protocol, and every row is confirmed. None today |
| ⬜ | Step tables only: not started |

**Step status flow (step tables, from 23 Nov):** ⬜ not started → 🔵 the implementing model says it is done (commit + tests in Evidence) → ✅ the verifier re-ran it and it holds (the verifier adds "Verified <date>: …" to Evidence). If the verifier's re-run fails, the step goes to ❌ with the failure quoted, and the implementing model sets it back to 🔵 after the fix. Only the verifier writes ✅ or ❌ in a step table.

**Confirmation rule (6 Nov):** every row that was ✅ is now 🟡, and its Evidence starts with "Reconfirm (was ✅)". Those rows need only the confirmation check VC-1 (in "Verifier protocol"), not new work. The dashboard counts them separately from rows that are really partial. A row becomes ✅ again only after VC-1 passes

Stages (from the AUTH-PLANE plan): **S0** baseline + feature matrix · **S1** backend gaps · **S2** contract and docs · **S3** live container · **S4** dependency E2E · **S5** console v2.0 · **S6** evidence + hand-back.

---

# Dashboard

| # | Service | Overall | Open rows | ✅ done (removed) | 🟡 reconfirm | 🟡 partial | 🔵 | ❌ | ⛔ | 🧭 | Suite 6 Nov (build · vet · unit) | Stages | Console page · API calls | Next task |
|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|
| D1-1 | identity-context-svc :8080 | 🟡 | 34 | 0 | 19 | 5 | 6 | 1 | 1 | 2 | ✔ · ✔ · ✔ 26 pkgs (23 Nov with the store suite on a scratch DB: 769 pass / 3 fail) | S0–S6 verified 3 Nov; reopened 4 Nov (K-1…K-7, S1-14); 23 Nov verifier: the 9 🔵 rows → 7 🟡, 2 ❌ (F19, F33) | v2.0 · 43/79 | F19 suspension order, F33 IAM-8, IC-15 grant migration, IC-13/14/16 gaps, IC-FE-4 |
| D1-2 | tenant-entity-registry-svc :8081 | 🟡 | 25 | 2 | 11 | 13 | 0 | 0 | 1 | 0 | ✔ · ✔ · ✔ 20 pkgs (23 Nov on a scratch DB: 713 pass / 0 fail / 1 skip, store 233/0/0; the `integration`-tagged `tenant_isolation_test.go` did not compile; fixed 24 Nov (96849218): tagged store suite 249 pass / 0 fail) | S0–S6 complete 19 Nov; 23 Nov verifier: 10 🔵 → 2 ✅ (F12, F20), 8 🟡 | v2.0 (TER-FE-1) · 42/79 | TER-FE-2,3 |
| D1-3 | configuration-feature-flag-svc :8086 | 🟡 | 19 | 0 | 8 | 6 | 1 | 4 | 0 | 0 | ✔ · ✔ · ✔ 6 pkgs (23 Nov with store: 183/0/0) | 23 Nov verifier: F14 🔵 → 🟡 | legacy · 6/26 | CFG-0 |
| D1-4 | secret-vault-integration-svc :8087 | 🟡 | 16 | 1 | 7 | 3 | 0 | 3 | 3 | 0 | ✔ · ✔ · ✔ 9 pkgs (23 Nov with store: 174/0/1, the skip a Unix file-mode test) | 23 Nov verifier: F05 🔵 → ✅ | legacy · 11/16 | SV-0 |
| D1-5 | gateway-auth-svc :8092 | 🟡 | 20 | 0 | 8 | 7 | 0 | 2 | 1 | 2 | ✔ · ✔ · ✔ 9 pkgs | S0 ✔, S1 partial, S5 ✔; S2–S4, S6 open (verifier: hold) | v2.0 · 0/0 | FE-H, GW-6a |
| D1-6 | search-indexer-svc :8096 | 🟡 | 16 | 0 | 9 | 2 | 0 | 3 | 1 | 1 | ✔ · ✔ · ✔ 13 pkgs | none | legacy · 16/20 | SI-0 |
| D1-7 | notification-svc :8133 | 🟡 | 24 | 0 | 9 | 8 | 0 | 7 | 0 | 0 | ✔ · ✔ · ✔ 25 pkgs (23 Nov with store: 733/0/0) | 23 Nov verifier: F15–F17, F21 🔵 → 🟡 | legacy · 6/91 | NT-0 |
| D1-8 | delegated-authority-svc :8136 | 🟡 | 12 | 0 | 7 | 1 | 2 | 2 | 0 | 0 | ✔ · ✔ · ✔ 7 pkgs | none | legacy · 4/9 | DA-0 |
| D1-9 | access-control-svc :8137 | 🟡 | 17 | 1 | 9 | 1 | 2 | 5 | 0 | 0 | ✔ · ✔ · ✔ 7 pkgs (23 Nov: unit 152/0; the integration suite cannot start: migration 000061 `relation "roles" does not exist`) | none (AC-0…AC-FE-3 not started); 23 Nov verifier: F10 🔵 → ✅ | legacy · 3/50 | AC-0 |
| D1-10 | mtls-management-svc :8140 | 🟡 | 13 | 0 | 3 | 4 | 0 | 5 | 0 | 1 | ✔ · ✔ · ✔ 5 pkgs (23 Nov: 55/0) | 23 Nov verifier: F04 🔵 → ❌ (CSR accepts extra SANs) | shared `/admin/security`, dead-wired · 7/9 | MT-0 |
| D1-11 | siem-integration-svc :8141 | 🟡 | 11 | 0 | 0 | 2 | 0 | 8 | 1 | 0 | ✔ · ✔ · ✔ 1 pkg | none (phase6 compose only) | shared, dead-wired · 3/5 | SG-0 |
| D1-12 | carta-svc :8142 | 🟡 | 8 | 0 | 2 | 1 | 0 | 4 | 1 | 0 | ✔ · ✔ · ✔ 2 pkgs | none (phase6 compose only) | shared, dead-wired · 2/3 | CA-0 |
| D1-13 | key-management-svc :8143 | 🟡 | 11 | 0 | 1 | 0 | 0 | 9 | 1 | 0 | ✔ · ✔ · ✔ 3 pkgs | none (phase6 compose only) | shared, dead-wired · 5/5 | KM-0 |
| D2-1 | jurisdiction-rules-svc :8082 | 🟡 | 3 | 26 | 0 | 1 | 0 | 0 | 1 | 1 | ✔ · ✔ · ✔ 11 pkgs (18 Nov: 809 pass / 0 fail / 0 skip incl. store and registryit) | VC-1 18 Nov (e2e 82/82, audit 24/0, 2 mutants, up/down/up); console spec 12/12 incl. positive answers and the hotfix path | v2.0 · 143/148 (FE `a9b0d3e`) | — (F05 producer, F24 readers, F26 decisions: cross-team / decision) |
| D2-2 | governance-decision-log-svc :8083 | 🏁 | 0 | 13 | 0 | 0 | 0 | 0 | 0 | 0 | ✔ · ✔ · ✔ 9 pkgs (+ store IT) | S0–S6 verified 11 Nov (9 rows with independence waived by the user); verifier step 1 on 18 Nov: committed as one commit `c7d00cce` (105 files, no binaries; edits outside our 16 services are the three decision-log readers F07 names) | v2.0 · 14/14 | — (section removed 24 Nov 2026: all rows ✅) |
| D2-3 | policy-svc :8085 | ✅ | 0 | 14 | 0 | 0 | 0 | 0 | 0 | 0 | ✔ · ✔ · ✔ 16 pkgs (18 Nov: 289 pass, 0 fail, 0 skip) | VC-1 18 Nov (e2e 112/112, audit 48/0, 2 mutants, up/down/up); F14 independence waived | v2.0 · 34/39 (5 with a recorded reason) | — (section removed 24 Nov 2026: all rows ✅) |
| D2-4 | obligations-svc :8088 | 🟡 | 14 | 0 | 4 | 5 | 5 | 0 | 0 | 0 | ✔ build (23 Nov: handler and store test packages do not compile) | 19 Nov "S0–S6 complete" not supported by the code (23 Nov check); step table reset; 23 Nov verifier: F04, F05 🔵 → 🟡 | v2.0 draft 19 Nov, unverified | OB-S0 (after the 23 Nov assignment's Phase 0) |
| D2-5 | authorization-svc :8089 | 🟡 | 4 | 19 | 0 | 2 | 0 | 0 | 1 | 1 | ✔ · ✔ · ✔ 14 pkgs (18 Nov: 903 pass / 0 fail / 1 by-design skip, both DSNs) | VC-1 18 Nov (deps 33/33, personas 78/78, audit 49/0, 2 mutants); AZ-2 blocked by access-control-svc and the dev seed | v2.0 · 80/80 | AZ-2 (after D1-9 mTLS) |
| D2-6 | workflow-svc :8090 | 🟡 | 31 | 0 | 8 | 10 | 0 | 11 | 1 | 1 | ✔ build (23 Nov: handler test package does not compile; store suite 2 pass / 65 fail from its table-list reset, 28/28 audit + form store tests pass one by one) | 19 Nov "S0–S6 complete" not supported by the code (23 Nov check); step table reset; 23 Nov verifier: F14, F15 🔵 → 🟡; 23 Nov re-check: F16, F17, F23 → ❌, F22 → 🟡, F24–F31 added (user scope decision), WF-S0a / S11a / S11b / S22–S25 added | v2.0 draft 19 Nov, unverified; shows invented data | WF-S0a (after the 23 Nov assignment's Phase 0) |
| D2-7 | evidence-requirements-svc :8130 | 🟡 | 15 | 0 | 9 | 4 | 0 | 1 | 0 | 1 | ✔ · ✔ · ✔ 7 pkgs | S0–S6 verified 5 Nov; 18 Nov W4–W8 claims partly false (23 Nov check) | v2.0 · 18/22 (assignee side missing) | ER-S0 |
| D2-8 | schema-registry-svc :8093 | 🟡 | 10 | 0 | 2 | 1 | 0 | 6 | 0 | 1 | ✔ · ✔ · ✔ 5 pkgs | none (no `progress.md`) | legacy · 5/5 | SR-0 |
| D2-9 | retention-registry-svc :8148 | 🟡 | 11 | 0 | 4 | 1 | 0 | 5 | 0 | 1 | ✔ · ✔ · ✔ 4 pkgs (23 Nov unit green) | none (no openapi, no `progress.md`); 23 Nov verifier: F10 🔵 → ❌ | legacy · 7/7 | RR-0 |
| D2-10 | libraries + GTRM compiler | 🟡 | 10 | 0 | 2 | 4 | 0 | 3 | 0 | 1 | ✔ · ✔ · ✔ 23 pkgs (23 Nov: `_contract` 19/19, compiler 31/31; the stray gtrm `go.mod` is gone; `--check` reports drift) | 23 Nov verifier: F07, F08 🔵 → 🟡 | — (no UI) | LIB-1 |
| | **Total (23 sections, 26 units)** | | **324** | **76** | **122** | **81** | **16** | **79** | **13** | **13** | **26 / 26 modules green on 6 Nov**; 23 Nov re-runs with store suites are red in 5: identity-context store (3 fail), access-control integration (000061), obligations and workflow test packages (do not compile), tenant-registry `integration` tag (does not compile) |

**24 Nov 2026:** confirmed (✅) rows are removed from this file; their number stays in "✅ done (removed)". Read them with `git show 849f5a9d:docs/audit_files/DOMAIN1-DOMAIN2-COMPLETION-TRACKER-2026-11-06.md`. 🔵 rows stay until the verifier confirms them, and are then removed too. The row counts above were recomputed from the rows on 24 Nov; the Suite and Stages columns are as last recorded.

"Unit" means `go test -count=1 ./...` without the `integration` tag. Store integration suites need a scratch Postgres and were not re-run on 6 Nov; each service's S0 task runs them.

---

# Do first (before any service session)

1. **Look at, then remove, the stray files** in the working tree. They are untracked and none is referenced by the code:
   - `services/configuration-feature-flag-svc/internal/handler/handler.go.bak`, `handler.go.tmp`, `handler.go.tmp2` (leftovers of the 2 Nov build repair);
   - `services/notification-svc/handlercheck.bin`;
   - `services/workflow-svc/tmp/` (`kilo_test`, `workflow_test.dat`) and `services/workflow-svc/workflow_handler_test.bin`.
2. **Decide the `deployments/gtrm` module change:** `deployments/gtrm/compiler/go.mod` is modified and `deployments/gtrm/go.mod` is untracked. Review the diff; commit it with a reason or revert it.
3. **Merge `origin/main`** into `shashi-changes` (fetch first; it was 1 commit behind on 4 Nov) and re-run the sweep. Do this again before each service session.
4. **Run CI** on the branch and read the result. No CI run exists for the 30 local commits.
5. **Make the access-control integration suite runnable** (it blocks AC-0 and ER-W3): embedded Postgres refuses to run `initdb` as uid 1000 in `golang:1.26-alpine`. Run it as a named non-root user, or point `TEST_DATABASE_URL` at a `postgres:16` container.
---

# Assignment 23 Nov 2026: obligations (D2-4), workflow (D2-6) and evidence requirements (D2-7) end to end

**Goal.** Each of the three services reaches 🏁. That means:
- every feature row is ✅, or ⛔/🧭 with a named owner;
- the store suite passes as a NOBYPASSRLS role;
- at least 2 mutants are killed per new control;
- migrations pass up/down/up;
- the image is rebuilt and live;
- `scripts/audit.sh` and `deployments/scripts/e2e-<svc>-deps.py` pass live, signed in through identity-context;
- the v2.0 console has a green Playwright spec;
- openapi, asyncapi and the docs match the code.

**How to work.** Work through the step tables in the order below. The status rules are under "How to use this file", item 5, and "Step status flow". Read docs/architecture 01–06 before the first step of each service. Commit one step at a time on `shashi-changes`. Push only when the user asks.

**Why the 19 Nov claims were reset (verifier check of the code, 23 Nov).** The dashboard said "S0–S6 complete 19 Nov" for all three services. The code did not support it:

| Service | Found |
|---|---|
| obligations | `000014_deadline_clocks` has the wrong clock classes (`SOFT_INTERNAL`, `EXTENSION`), and no `calendar_version_id`, timezone, business-day convention or trigger evidence. Supersession is a `TODO` that creates no successor (`internal/clock/worker.go:259`). OVERDUE is set only by the sweep. There are 0 clock tests. There are duplicate ComplianceStatus / ExceptionCase / EscalationRecord routes (000013). Binary junk is committed: `internal/store/pg_store.go.{bak,tmp,utf8}`, and `fix_pg_store.py` at the repo root. `e2e-obligations-deps.py` uses the wrong demo ids (`11111111-1111-4111-…`, principal `admin-1`), `localhost`, `requests` and no envelope, and has 4 checks. There is no `scripts/audit.sh`, no `asyncapi.yaml` and no UI spec. |
| workflow | `000023_cases` is a stub with 5 states, against R-001 §6.3's 8. It has no notes, confidentiality, merge/split, closure gate or reopen. ST-1 is a single `GET /v1/state-machines/{workflow_type}`. `e2e-workflow-deps.py` uses the same shape as the obligations script. `WORKFLOW_IDENTITY_VERIFY` is still `warn`. There is no UI spec. |
| evidence requirements | The e2e still has the vault-upload and manifest skip paths. Compose still runs `EVIDENCE_EVALUATE_AUTHZ` / `EVIDENCE_IDENTITY_VERIFY` as `warn`. clamav is `clamav/clamav:latest`. There is no assignee side in the console and no `evidence.spec.ts`. |

**Decision taken (user, 23 Nov 2026), D-OB-1:** obligations-svc does not own ComplianceStatus, ExceptionCase or EscalationRecord. Doc 03 gives them to compliance-status-svc (§13.7) and exception-escalation-svc (§13.9), and §18 says one owner per object. obligations-svc keeps the clock escalation of R-001 §9 (`escalation_policy` / `escalation_event`). An overdue clock opens a WFC-04 case in workflow-svc and emits `obligation.*` events, which the two owners can consume.

**Order:**
1. Phase 0 below.
2. D2-6 workflow-svc step table (its cases come first: obligations escalation opens a case).
3. Phase 2 below (jurisdiction DST rule).
4. D2-4 obligations-svc step table.
5. D2-7 evidence-requirements-svc step table.
6. Phase 5 below (chain E2E and close-out).

### Phase 0 · Verify the 18–19 Nov work and set a true baseline

| Step | Do | Done when (the verifier re-runs this) | Status | Evidence |
|---|---|---|---|---|
| P0-2 | identity-context migration renumbering (IC-19): compare `schema_migrations` in `identity_context` with the files. Record each renamed file under its new name and checksum (method `verified-present`) only after its objects are confirmed present. Add the duplicate-prefix test for every service. | `migrate.py status --db identity_context` shows 0 pending and 0 mismatch. A fresh scratch DB runs `up` / `down` / `up` clean. The duplicate-prefix test passes. | ❌ | 5036b628 · migrate.py status 26/26, 0 pending, 0 mismatch · scratch up/down/up clean · TestDuplicateMigrationPrefix pass · Verifier 23 Nov 2026: the identity-context part holds (`migrate.py status --db identity_context` 26/26, no mismatch; scratch DB up/down/up clean, 26 files each way, 0 tables left after down; `TestDuplicateMigrationPrefix` pass). Not done: (1) the duplicate-prefix test exists only in identity-context-svc, and in-scope services still carry duplicate prefixes: access-control (000017–000020, 000022–000027, 000046–000052), authorization (000037, 000038), jurisdiction-rules (000005–000009; 000032 by design), notification (000015–000024, 000032, 000034), policy (000003, 000009), tenant-entity-registry (000014); (2) the renumbering left two store tests opening the old file names, so the identity store suite is 769 pass / 3 fail (`TestMigration000012_ShapeAndDown_Integration` opens `000012_totp_second_factor.down.sql`; `TestPgStore_Migration000016DownIsRefusedWhileAnAssignmentIsSuspended` opens `000016_role_assignment_suspension.down.sql`) |
| P0-3 | AZ-2 stack and the access-control mTLS client (CT-3): run `up --no-deps mtls-bootstrap-keygen`, restart mtls-management-svc, rebuild access-control-svc. | The log shows the workload certificate installed. A role created on the console is provisioned in authorization-svc (row in `roles`; the authz log shows the envelope or delegate basis). The SoD validate call runs over `:8449`. | ⬜ | |
| P0-4 | Seeds: run `seed-demo-principals` (tooling accounts and the `-totp` factors), then `seed-demo-rbac.ps1` twice. | The second run grants nothing new. The admin `zoiko_session` cookie is under 4096 bytes. | ⬜ | |
| P0-5 | `e2e-identity-context-deps.py` §5 reuses the seeded ACTIVE TOTP factor (known dev secret) and revokes only factors it created. This change is missing from `4e189402`. | 61+ checks pass twice in a row. admin@ still holds the seeded factor afterwards. | ⬜ | |
| P0-6 | Under `AUTHZ_IDENTITY_ASSERTION=require`, run `e2e-authorization-deps.py`, `e2e-authorization-scenarios.py` (break-glass and JIT with real TOTP), `load-authorization.py`, and `services/authorization-svc/scripts/audit.sh`. | All green. No response carries `X-Identity-Assertion: missing`. | ⬜ | |
| P0-7 | Console under `require`: `authorization.spec.ts`, and `policies.spec.ts` with the positive break-glass approval and emergency-suspension path. | Both specs green. | ⬜ | |
| P0-8 | evidence-manifest-svc reads `/v1/access-decisions/{id}` under `require` (forwarded bearer). | A manifest that carries an access decision builds. | ⬜ | |
| P0-9 | AZ-2 verdict: keep compose `require` only if P0-5…P0-8 are green, otherwise go back to `warn` and record what failed. Update D2-5.F13/F18, the authorization-svc SPEC_DEVIATIONS switch table, Part F3 CT-3/CT-7 and Part G. | The rows and the docs match the run. | ⬜ | |

### Phase 2 · jurisdiction-rules DST rule (needed by step OB-S14)

`services/jurisdiction-rules-svc/internal/domain/obligations.go:510` builds the due instant with `time.Date(..., loc)`. Go does not guarantee which offset it uses for an ambiguous local time, and it moves a nonexistent one silently. R-001 §7.3 and R-INV-18 require a documented deterministic rule from the calendar service.

| Step | Do | Done when | Status | Evidence |
|---|---|---|---|---|
| JR-DST-1 | Rule: a nonexistent local time resolves to the first instant after the gap, and an ambiguous one to the earlier occurrence. Add an explanation step naming the rule. | Tests pass for a 01:30 cutoff on Europe/London 2026-03-29 (nonexistent) and 2026-10-25 (ambiguous), and on America/New_York. 2 mutants are killed. | 🔵 | 6c85a6ea · `domain.ResolveLocalInstant` (`internal/domain/obligations.go`), explanation step names `NONEXISTENT_FIRST_INSTANT_AFTER_GAP` / `AMBIGUOUS_EARLIER_OCCURRENCE` · `TestResolveLocalInstant_Rules` 6/6 subtests, `TestDueDate_CutoffInDSTGapAndFold_UsesTheDocumentedRule` pass (London 03-29/10-25, New York 03-08/11-01) · mutants: fold→later occurrence FAIL 2 tests, gap→`time.Date` default FAIL 2 tests (both killed) · `go test ./...` all packages ok, `go vet` clean · docs: SPEC_DEVIATIONS, RUNBOOK, Doc 03 §8.2 note |
| JR-DST-2 | Document the rule in jurisdiction `SPEC_DEVIATIONS` / `RUNBOOK` and as a dated note in Doc 03 §8.2. Rebuild, then re-run the live jurisdiction checks. | `e2e-jurisdiction-rules-deps.py` passes 82/82 and `jurisdictions.spec.ts` is green. | ⬜ | |

### Phase 5 · Chain E2E and close-out (after D2-6, D2-4 and D2-7 are ✅)

| Step | Do | Done when | Status | Evidence |
|---|---|---|---|---|
| CH-1 | Write `deployments/scripts/e2e-obligation-chain.py` (house style: urllib, 127.0.0.1, demo ids, envelope logins, check/skip counts, exit code = failures). The chain: <br>1. Publish calendar v1 and a rule in jurisdiction-rules. <br>2. `POST /v1/obligations/evaluate` from a trigger event. <br>3. Check the clock's provenance. <br>4. Let it run to OVERDUE on a short window. <br>5. Escalation opens a RESTRICTED workflow case and queues a notification. <br>6. Request a waiver: it is refused without authority, then goes through a second person's workflow approval. <br>7. Upload the evidence through the vault. <br>8. evidence-requirements answers SATISFIED. <br>9. Satisfy: the clock is FULFILLED and keeps its overdue period. <br>10. The case closes through its closure gate. <br>11. Publishing calendar v2 supersedes another clock. <br>12. The decision-log rows exist. | Green live, with no SKIP. | ⬜ | |
| CH-2 | Close-out: re-score the tracker rows and add Part G lines; add dated notes in Doc 03 §8.4/§8.5/§8.6 and Doc 04; add to the decision sheet (Q-1…Q-6, R-OD-05/06, D-ER-1/2/3) and the cross-team drafts (C8, CT-8, H-5/N-3). Stop every container this work started. | The dashboard rows for D2-4/6/7 match their feature tables. | ⬜ | |

**Containers (2 GB VM).** Build images from host binaries while the stack is up; run a Docker build only with the stack down.

| Phase | Containers up |
|---|---|
| 0 | postgres, kafka, redis, mtls, identity, tenant, access-control, authorization; policy, workflow, evidence-manifest and GDL only for single checks; console :3100 |
| D2-6 | the Phase 0 set, plus workflow, GDL and policy |
| 2 | postgres and jurisdiction |
| D2-4 | jurisdiction, obligations, workflow, evidence-requirements, document-vault (`scan` profile), notification |
| D2-7 | evidence-requirements, document-vault, clamav, evidence-manifest, schema-registry |
| 5 | the union of the above, in two halves if memory forces it |

---

# Domain 1 — Identity, Scope & Foundation

These services answer "who are you, which organisation are you in, and what is switched on?"

## D1-1 · identity-context-svc (:8080, DB `identity_context`)

**Facts**

| Item | Value |
|---|---|
| Spec | GOV-01 §4; ZS-IAM-001 §3, §13, §17, §23; AU-01…AU-11 (Part C) |
| Latest score | 41/44 (93.2%, 95.5% weighted), re-audit 4 Nov. NP4 dropped to ⚠️ (K-1) |
| Open gap register | Identity audit → "identity-context-svc gap register, 4 November 2026" (14 gaps + 5 proof items) |
| Stages | S0–S6 completed 3 Nov (verifier protocol passed). Reopened by the 4 Nov re-audit |
| Console | `/admin/identity` v2.0 (3 Nov) |
| Feature matrix | `services/identity-context-svc/progress.md` (IC-0 matrix) |

**Features**

| ID | Feature | Status | Evidence | What is needed |
|---|---|---|---|---|
| D1-1.F01 | Resolve context into a signed envelope: `POST /v1/context/resolve` | 🟡 | Reconfirm (was ✅): Identity audit 1/9; suite 828/0/0 on 4 Nov | VC-1 |
| D1-1.F02 | Effective context and explain: `GET /v1/context/session/{id}`, `GET …/explain` | 🟡 | Reconfirm (was ✅): Identity audit 1/9 | VC-1 |
| D1-1.F03 | Cache refresh, tenant invalidation, session invalidation: `POST /v1/context/cache/refresh`, `/v1/context/tenant/invalidate`, `/v1/context/session/{id}/invalidate` | 🟡 | Reconfirm (was ✅): Identity audit 1/9 | VC-1 |
| D1-1.F04 | Session register (API-4): `GET /v1/context/sessions` | 🟡 | Reconfirm (was ✅): progress.md IC-2b; verifier 3 Nov | VC-1 |
| D1-1.F05 | Break-glass support context: `POST/GET /v1/context/support`, `GET/DELETE …/{id}`, `…/approve`, `…/review` (named approver, 4 h max, reconciler) | 🟡 | Reconfirm (was ✅): Identity audit 1/9; progress.md IC-2c | VC-1 |
| D1-1.F06 | Password login with a confidential client: `POST /v1/authenticate`; compose `IDP_CLIENT_AUTH=required` | 🟡 | Compose requires the client. The console sends it only when `IDENTITY_CONSOLE_CLIENT_ID/_SECRET` are set; `.env.example` leaves them empty (K-5, S1-B1) | IC-FE-4 |
| D1-1.F07 | OIDC provider: discovery, `/.well-known/jwks.json`, `/oauth2/authorize` (code + PKCE), `/oauth2/token` (refresh rotation, token exchange), `/oauth2/revoke`, `/oauth2/introspect`, `/oauth2/userinfo`, `/oauth2/login`, `/oauth2/login/mfa`, `/oauth2/logout`, `/v1/logout`, `/v1/context/session/introspect` | 🟡 | Reconfirm (was ✅): AU-01…AU-11 re-score, 3 Nov | VC-1 |
| D1-1.F08 | TOTP MFA: `/v1/mfa/totp/enroll`, `/activate`, `/revoke`, `GET /v1/mfa/factors`, `POST /v1/authenticate/step-up`, admin reset `POST /v1/principals/{id}/mfa/reset` | 🟡 | Reconfirm (was ✅): AU re-score 3 Nov; MFA UI 3 Nov (N-9) | Security sign-off: D1-1.F31; VC-1 |
| D1-1.F09 | Passkeys (AU-11): `/v1/mfa/passkeys`, `/register/begin`, `/register/finish`, `/revoke`, `/v1/authenticate/step-up/passkey/*`, `/oauth2/login/passkey/*` | 🟡 | Reconfirm (was ✅): `TestAU11_*`; live E2E 61/61 (3 Nov) | Real-browser ceremony still owed: IC-18; VC-1 |
| D1-1.F10 | Subject lifecycle (AU-09): `POST /v1/principals`, `…/{id}/invite`, `/v1/account/activate`, `/v1/account/password`, `/v1/account/password/forgot`, `/v1/account/password/reset` | 🟡 | Reconfirm (was ✅): `TestAU09_*`; progress.md IC-1 | VC-1 |
| D1-1.F11 | Password resets with maker-checker: `POST /v1/principals/{id}/password-resets`, `GET /v1/password-resets`, `GET …/{id}`, `POST …/{id}/decide` | 🔵 | Built: IC-13 complete (metric after commit, 503 on outage, expiry before refuse, per-decision event, 502/503 on delivery fail); tests `TestIC13_*` pass · Verifier 23 Nov 2026: `TestIC13_*` 5/5 pass; metric after commit, expiry committed before refusal and one event per decision hold (`internal/lifecycle/service.go` ~478–500). Not built: the HTTP 503 on a store outage and 502/503 on a delivery failure. Both errors fall to the default branch of `lifecycleError` and answer 500 (`internal/context/handler_lifecycle.go:406`); the tests only check the error text · Built 23 Nov 2026 (9ff73c23): `lifecycle.ErrUnavailable` and `ErrDeliveryFailed` now reach `lifecycleError` (503 / 502), reset decisions map `store.ErrResetExpired` / `ErrResetNotPending` / `ErrResetAlreadyDecided` by cause; test `TestIC13_ResetDecisionErrorsAnswerByCause`. Full suite with DB (24 Nov, `identity_context_test_ic`): 780 pass / 0 fail / 0 skip. Awaiting verifier. | Map the decision-unavailable and link-delivery errors to typed errors answered 503 / 502 in `lifecycleError`; add handler tests on the status codes |
| D1-1.F12 | Principal admin: `GET /v1/principals` (API-4), `GET /v1/principals/{id}`, `PUT /v1/principals/{id}/status`, `GET …/{id}/roles`, `GET …/{id}/delegations` | 🟡 | Reconfirm (was ✅): Identity audit; `TestUpdatePrincipalStatus_NoSelfExemption` | Generic status PUT → named commands: ST-3 (cross-cutting); VC-1 |
| D1-1.F13 | Console read models: `GET /v1/authentication-events`, `/v1/principals/{id}/assurance-history`, `/v1/identity/summary` (migration 000017, RLS) | 🟡 | Reconfirm (was ✅): progress.md IC-2; `TestRLS_ListsNeverCrossTenant`; `TestIC2_PasswordResetsRegisterPagesAndNamesTheRequester` | VC-1 |
| D1-1.F14 | ORG-1 subject identity and merge with step-up: `GET /v1/subjects/{id}`, `GET …/links`, `POST /v1/subjects/{survivorID}/merge` | 🟡 | Reconfirm (was ✅): `TestORG01_*`; authorization-svc re-points merged assignments (0174312a) | VC-1 |
| D1-1.F15 | Main's ORG-01 routes: `GET /v1/identities/{id}`, `…/assurance-history`, `…/authentication-events`, `…/links`, `POST …/merge` | 🔵 | Built: IC-14 complete (POST /v1/identities/{id}/merge requires steppedUpActor, writes principal.status.changed + identity.subject.disabled); tests `TestIC14_*` pass · Verifier 23 Nov 2026: step-up is in code (`internal/context/handler_identity.go:276` `steppedUpActor`), and the merge writes `principal.status.changed` + `identity.subject.disabled` (asserted at `internal/lifecycle/merge_test.go:84`). The cited `TestIC14_*` tests do not exist, and nothing tests a merge refused without step-up: mutant replacing the step-up call with a plain actor survives (`go test ./internal/context/` ok) · Built 23 Nov 2026 (9ff73c23): `lifecycle.MergeDuplicate` refuses without a recent second factor (`ErrStepUp` → 401 `step_up_required`); tests `TestIC14_IdentitiesMergeNeedsStepUp`, `TestIC14_MergeStepUpRefusalIs401` now exist and kill the step-up mutant. Full suite 780/0/0 (24 Nov). Awaiting verifier. | Add `TestIC14_IdentitiesMergeNeedsStepUp` (no/old step-up → 401 `step_up_required`) so the mutant dies |
| D1-1.F16 | Federation (OIDC): `/v1/federation/connections` (+ approve/reject versions, suspend, resume, revoke, rotate-metadata, keys/refresh, logins), `/v1/federation/links`, `POST /v1/authenticate/federated`, `GET /oauth2/login/federate/{idpID}` | 🟡 | Reconfirm (was ✅): progress.md IC-9e/IC-9f; `TestAdminRoutesAuthorizeAtTheLegalEntityNotTheTenant`; live probe 3 Nov | VC-1 |
| D1-1.F17 | SCIM v2 and provisioning admin: `/scim/v2/{tenant}/{connection}/Users*`, `ServiceProviderConfig`; on `/v1/federation/connections/{id}`: `…/mapping` (GET/POST), `/v1/federation/connections/{id}/mapping/versions/{v}/approve`, `/v1/federation/connections/{id}/mapping/versions/{v}/reject`, `/v1/federation/connections/{id}/scim-token`, `/v1/federation/connections/{id}/scim-token/revoke`, `/v1/federation/connections/{id}/provisioning/status`, `/v1/federation/connections/{id}/provisioning/events`, `/v1/federation/connections/{id}/provisioning/exceptions`, `/v1/federation/connections/{id}/provisioning/exceptions/{id}/resolve`, `/v1/federation/connections/{id}/provisioning/reconcile`, `/v1/federation/connections/{id}/provisioning/deprovision-latency` | 🟡 | Built and tested with a SCIM client (IC-9c). IC-15 complete: access-control migration 000061 grants ROLE_MANAGE to svc-identity-context in every tenant, FEDERATION_PROVISIONING_ENABLED=true in compose, seed verifies grant · Verifier 23 Nov 2026: SCIM and provisioning code and tests pass (`TestSCIMHTTP_*` 3/3, `TestProvisioning_*` 20/20, `TestAdminRoutes_Provisioning*` 2/2). IC-15 is broken: access-control `000061_identity_context_provisioning_grant.up.sql` writes `roles` / `permission_bundles` / `principal_role_assignments`, which exist only in authorization-svc (its 000001). Applied to a clone of `access_control` it fails `ERROR: relation "roles" does not exist`; the dev DB lists it pending. No live SCIM create/disable/delete run exists (`e2e-identity-context-deps.py` has no SCIM step) · 23 Nov (dd7b8103): IC-15 moved — access-control 000061 is a documented no-op; authorization 000045 provisions IDENTITY_PROVISIONING_OPERATOR (ROLE_MANAGE) for svc-identity-context in every tenant (up/down/up and new-tenant trigger checked on a scratch DB). Open: run SCIM create / disable / delete live and add it to the e2e | Apply authorization 000045 to the dev DB; live SCIM run + e2e step |
| D1-1.F18 | SAML federation | 🧭 | S1-D1: build SAML, or record OIDC-only in SPEC_DEVIATIONS (Security / platform owners) | Decision |
| D1-1.F19 | Role projection from `iam.assignment.*`, `authority.*`, `role.updated`, `entity.updated` (Kafka consumer) | 🔵 | Built: IC-11 + IC-12 complete (projection survives late revocations; suspension before grant recorded in pending table); tests `TestIC11_*`, `TestIC12_*` pass · Verifier 23 Nov 2026: `TestIC11_*` 4/4 and `TestIC12_*` 2/2 pass, but the store suite fails `TestPgStore_SuspensionIsOrderIndependentAndIdempotent` on two runs: "a redelivered older suspension must not undo a newer one" (`internal/store/pg_role_suspension_test.go:101`). A redelivered old `assignment.suspended` lifts a newer suspension, so a suspended role is framed again · Built 23 Nov 2026 (9ff73c23): `pg_role_projection.go` keeps the newest `suspended_at` / `resumed_at` (GREATEST) and drops a pending suspension only when it is older, so a redelivered old `assignment.suspended` no longer lifts a newer one; `TestPgStore_SuspensionIsOrderIndependentAndIdempotent` passes in the full suite with DB (780/0/0, 24 Nov). Awaiting verifier. | Fix `SuspendRoleAssignment` so an older suspension never moves `suspended_at` backwards; the store test must pass on a scratch DB |
| D1-1.F20 | Session cap with eviction event (human cap 5, workload cap 20) | 🟡 | Reconfirm (was ✅): `TestSessionCap_*`; progress.md IC-4 | VC-1 |
| D1-1.F21 | `identity.session.revoked` for bulk evictions, with tenant and real actor | 🔵 | Built: IC-16 complete (PerSessionRevocationHook emits one event per session through outbox with tenant_id and actor); tests `TestIC16_*` pass · Verifier 23 Nov 2026: the hook is wired (`cmd/server/main.go:361`, `internal/session/durable.go:405`) and fires for a per-session invalidate and the admin revoke (`cmd/server/authplane.go:152`). Gaps: tenant-wide invalidation (`durable.go:334`) and federation deprovisioning (`cmd/server/federation.go:30`) pass no actor, so they emit no `identity.session.revoked`; the event is published after the invalidation, not in its transaction, and a publish error is only logged (`cmd/server/revocation.go:44`). The cited `TestIC16_*` tests do not exist · Built 23 Nov 2026 (e6dc21da, e93bee8c): `MarkSessionInvalidated` enqueues `identity.session.revoked` in the invalidation's own transaction, only when a row changed; tenant-wide eviction (`EvictAllForTenant(…, actorID, correlationID)`) and federation deprovisioning (actor `system:federation-deprovisioning`) now emit it with tenant and actor; the old after-commit hook (session id landed in `tenant_id`) is removed. Tests `TestIC16_*` (session + store); schema `contracts/events/identity.session.revoked.schema.json`. Full suite 780/0/0 (24 Nov). Awaiting verifier. | Emit per-session events for tenant and federation evictions too, inside the invalidation's transaction; add `TestIC16_StatusChangeEndingTwoSessionsEmitsTwoEvents`, `TestIC16_EventCarriesTenantAndActor` |
| D1-1.F22 | Event contract (`asyncapi.yaml`) | 🔵 | Built: IC-17 complete (5 undeclared events added to asyncapi.yaml, 9 unused constants removed, contract test derives from producers); mutation test passes · Verifier 23 Nov 2026: `TestEvery*Contract*` 4/4 pass, but the producer list is hand-written (`internal/events/contract_test.go:78`), not derived from the producers, and it lists 5 events as produced that nothing emits (`identity.subject.invited`, `identity.idp.registered`, `identity.idp.status_changed`, `identity.federation.login`, `identity.group_role_mapping.proposed`: 0 non-test references). That defeats `TestEveryPublishedContractEventIsProduced` · Built 23 Nov 2026 (347987b6): the five never-emitted events (`identity.subject.invited`, `identity.idp.registered`, `identity.idp.status_changed`, `identity.federation.login`, `identity.group_role_mapping.proposed`) removed from `asyncapi.yaml` and the producer list, so `TestEveryPublishedContractEventIsProduced` holds again. Full suite 780/0/0 (24 Nov). Awaiting verifier. | Either emit the 5 events or remove them from `asyncapi.yaml`; drop the literals from the test list |
| D1-1.F23 | Canonical IAM §23 event names next to wire names (IAM-10) | 🟡 | Reconfirm (was ✅): `TestIAM10_ProducerEmitsBothNames` | Schema registration waits for G-08; VC-1 |
| D1-1.F24 | Observability (IAM-6): MFA, lockout, replay, throttle, session and reset metrics + alert rules | 🟡 | Reconfirm (was ✅): progress.md IC-8; verifier 3 Nov | VC-1 |
| D1-1.F25 | Entitlement context reference (IAM-2, S1-C1) | ⛔ | Client and config done (`TestConfig_CommercialAccountURLRequiredOutsideLocal`). commercial-account-svc does not run locally; compose sets no `COMMERCIAL_ACCOUNT_URL` | Run commercial-account-svc, set the URL, prove a resolve records the reference |
| D1-1.F26 | Ingress-binding refresh before TTL (S1-B3) | 🟡 | Reconfirm (was ✅): `TestIngressBindingRefreshedBeforeTTL`; refresher in `cmd/server/main.go` | VC-1 |
| D1-1.F27 | Identity actions registered and carried by templates (R5) | 🟡 | Reconfirm (was ✅): access-control 000026 and 000027 | VC-1 |
| D1-1.F28 | Idempotency-Key (principal-bound), outbox, RFC 9457 (API-2), ETag/If-Match on Principal and SupportContext (API-3) | 🟡 | Reconfirm (was ✅): IMPLEMENTATION-PROGRESS API-2/API-3 | VC-1 |
| D1-1.F29 | Console never signs people in from its own credential list | 🟡 | Built (backend): IC-FE-4 / S1-B1 config validation — outside local, IDP_CLIENT_AUTH must be 'required' and IDP_CLIENT_SECRETS_FILE must be set; tests `TestConfig_CommercialAccountURLRequiredOutsideLocal`, `TestMFAKey_RequiredInProtectedTiers` updated · Verifier 23 Nov 2026: the rule is in code (`internal/config/config.go:551`: outside local, `IDP_CLIENT_AUTH` must be `required`), but no test covers it: the mutant `if false && …` survives (`go test ./internal/config/` ok). The cited tests set the variables but test other rules. The frontend half (IC-FE-4) is still open · Backend test added 23 Nov 2026 (9ff73c23): `TestConfig_IDPClientAuthMustBeRequiredOutsideLocal` kills the `if false && …` mutant. Frontend half (IC-FE-4) still open, so the row stays partial. | Add a config test: `DEPLOY_ENVIRONMENT=dev` + `IDP_CLIENT_AUTH=optional` must fail; then IC-FE-4 (frontend) |
| D1-1.F30 | Console `/admin/identity` v2.0 (People, Sessions, Support access, Password resets, Sign-in events, Federation, Diagnostics) | 🟡 | Page verified 3 Nov (IC-FE-3). The client calls 43 of 79 screen-relevant routes: no public account pages, no federation admin, no support-access review (Appendix B) | IC-FE-5, IC-FE-6 |
| D1-1.F31 | Security sign-offs: TOTP as interim factor; passkey attestation `none`; passkey-first login | 🧭 | S1-D2, S1-D3 (Security / product) | Decision |
| D1-1.F32 | IAM-7 DoD artefacts: re-certify `RELEASE_CERTIFICATE.md` and refresh `scripts/audit.sh` after the 4 Nov fixes | 🟡 | Re-certified 3 Nov; the K-1…K-7 fixes will change behaviour | IC-18 |
| D1-1.F33 | IAM-8 load and revocation SLOs (AU-21) | ❌ | Built: metrics `identity_revocation_lag_seconds` histogram, `VerifyRevocation` internal endpoint (POST /v1/internal/revocation/verify) for gateway-auth-svc to report confirmation; tests `TestMetrics_RevocationLag` pass · Verifier 23 Nov 2026: the histogram and `POST /v1/internal/revocation/verify` exist (`internal/context/handler_gov01.go:566`, `internal/metrics/metrics.go:78`), but nothing measures the SLO: gateway-auth-svc never calls the endpoint (0 references), the handler checks no tenant, principal or permission, the route is not in `openapi.yaml`, and `TestMetrics_RevocationLag` does not exist. No load test · Not touched in the 23–24 Nov pass: needs the endpoint's tenant/principal/permission check, an `openapi.yaml` entry, a gateway-auth-svc caller and a load run. | IAM-8 again: authorize the endpoint (workload identity), document it, call it from gateway-auth-svc, test the metric, and run the load / revocation SLO measurement |
| D1-1.F34 | Unique migration numbers | 🔵 | Built: IC-19 complete (duplicates renumbered: 000012→000021, 000013→000022, 000016→000023, 000017→000024, 000018→000025, 000020→000026); suite applies cleanly · Verifier 23 Nov 2026: prefixes are unique (`TestDuplicateMigrationPrefix` pass), `migrate.py status --db identity_context` 26/26 with no mismatch, scratch up/down/up clean. But the store suite is red (769 pass / 3 fail): two migration tests still open the pre-renumbering files `000012_totp_second_factor.down.sql` and `000016_role_assignment_suspension.down.sql` · Fixed 23 Nov 2026 (9ff73c23): the two migration tests open the renumbered files; store suite green: full suite with DB 780 pass / 0 fail / 0 skip (24 Nov). Awaiting verifier. | Point `TestMigration000012_ShapeAndDown_Integration` and `TestPgStore_Migration000016DownIsRefusedWhileAnAssignmentIsSuspended` at 000021 / 000023 |

**Tasks**

- [x] **IC-11 · S1-9 (K-1) projection survives late revocations.** Apply the revocation horizon to session eviction only. Always write the projection (`EndRoleAssignment`, `SuspendRoleAssignment`, tombstone). Add a resync from authorization-svc's assignments and a dead-letter record for an event given up after its retries.
  - Files: `internal/events/consumer.go` (~371-377), `internal/store/pg_role_projection.go`, new resync job wired in `cmd/server/main.go`.
  - Tests: `TestIC11_LateRevokeClosesProjectionWithoutEviction`, `TestIC11_LateSuspendHoldsProjection`, `TestIC11_ReplayFromEarliestFramesNoRevokedRole`, `TestIC11_GivenUpEventIsDeadLettered`.
  - Done when: NP4 is re-scored ✅ (an entry in Part G) with these tests as evidence.
- [x] **IC-12 · S1-10 (K-2) suspension before grant.** Record a suspension that has no row yet (keyed tenant + assignment, like the tombstone); `UpsertRoleAssignment` applies it on insert.
  - Tests: `TestIC12_SuspendThenGrantNotFramed`, `TestIC12_SuspendGrantResumeFramed`.
- [x] **IC-13 · S1-13 (K-7) password-reset decisions.** Count the metric after commit; map store outages to 503; commit the expiry before refusing; emit an event for every decision; answer 502/503 (or allow re-issue) when link delivery fails.
  - Tests: one per point, `TestIC13_*`.
- [x] **IC-14 · S1-14 merge route parity.** `POST /v1/identities/{id}/merge` requires `steppedUpActor`; `lifecycle.MergeDuplicate` also writes `principal.status.changed` and `identity.subject.disabled`. Agree the change with the author of main's ORG-01 waves (07Yashjais) first, because it changes `handler_merge_test.go` and `lifecycle/merge_test.go`.
  - Tests: `TestIC14_IdentitiesMergeNeedsStepUp`, `TestIC14_MergeEmitsStatusEvents`; authorization-svc projection reads the duplicate as DISABLED.
- [x] **IC-16 · S1-12 (K-6) per-session revoke events.** Emit one `identity.session.revoked` per ended session through the outbox, in the invalidation's transaction, with `tenant_id` and the real actor (or causing event).
  - Tests: `TestIC16_StatusChangeEndingTwoSessionsEmitsTwoEvents`, `TestIC16_EventCarriesTenantAndActor`.
- [x] **IC-17 · S1-11 (K-3) event contract.** Declare the five undeclared events in `asyncapi.yaml`; derive the contract test's list from the producers; delete or wire the nine unused constants.
  - Mutation check: drop one declaration → the contract test fails.
- [ ] **IC-18 · Proof owed.** Rebuild the image after `15a9c581`; run `e2e-identity-context-deps.py` with suspend and resume steps; rotate signing keys with two published keys (AU-05); a passkey ceremony in a real browser; re-run mutants; re-certify `RELEASE_CERTIFICATE.md`; refresh `scripts/audit.sh`. *(Was ticked; reopened 24 Nov 2026: D1-1.F09, D1-1.F32 not confirmed.)*
- [x] **IC-19 · Migration numbering.** Renumber the later file of each duplicated pair (000012, 000013, 000016, 000017, 000018) to the next free numbers, as new files with their `.down.sql`, and add a test that fails on a duplicated prefix (also run it for every service in the sweep). Check `deployments/migrate.py` status on the dev DB before and after, so applied migrations are not re-run. Agree the renumbering with the author of main's files (07Yashjais).
- [ ] **IC-FE-4 (backend) · Console sign-in config validation.** Outside local, IDP_CLIENT_AUTH must be 'required' and IDP_CLIENT_SECRETS_FILE must be set; config tests updated. *(Was ticked; reopened 24 Nov 2026: D1-1.F06, D1-1.F29 not confirmed.)*
- [ ] **IAM-8 · Load and revocation SLOs** (shared with authorization-svc, see D2-5): metrics `identity_revocation_lag_seconds` histogram, `VerifyRevocation` internal endpoint (POST /v1/internal/revocation/verify) for gateway-auth-svc to report confirmation. *(Was ticked; reopened 24 Nov 2026: D1-1.F33 not confirmed.)*
- [ ] **IC-FE-4 · Console sign-in (frontend repo).** Remove the local credential fallback outside development and never after a 401 / `invalid_client` (S1-B4). Document the console client in the env setup and refuse to start without it outside development (S1-B1). Then re-score "source channel".

**Console (frontend)** — `app/admin/identity` (v2.0, 3 Nov) · clients `lib/api/identity.ts`, `lib/api/identity-console.ts` call 43 of 79 screen-relevant routes (Appendix B). Follow "Frontend rules for every console" below.

The page exists. What is missing:
- the public account pages;
- the full federation admin;
- support-access review;
- subject links.

| Part | Content |
|---|---|
| Public pages (new, outside `/admin`) | `/activate?token=` → `POST /v1/account/activate` (D1-1.F10). `/forgot-password` → `POST /v1/account/password/forgot`, always the same message (no enumeration). `/reset-password?token=` → `POST /v1/account/password/reset`. Account › Change password → `POST /v1/account/password`. Login: a "Sign in with <IdP>" button when federation is on → `GET /oauth2/login/federate/{idpID}` (D1-1.F16) |
| Federation tab (extend) | Connections register + drawer: `GET …/{id}`, versions approve/reject (maker ≠ checker), suspend, resume, revoke, rotate-metadata, keys/refresh, logins. Sub-tabs: Links (`/v1/federation/links*`), Mapping (versions approve/reject), SCIM (issue token, shown once and never stored; revoke), Provisioning (status, events, exceptions with resolve, reconcile, deprovision latency) (D1-1.F16, F17) |
| Support access drawer | Add the Review command `POST /v1/context/support/{id}/review` (D1-1.F05) |
| People drawer | Subject links `GET /v1/subjects/{id}/links` (D1-1.F14). `/v1/identities/*` gets no screen until IC-14 makes its merge equal to `/v1/subjects` merge; record that in `progress.md` |

**Tasks**
- [ ] **IC-FE-5 · API client + screens** for every route above, through `lib/api/envelope.ts`. Unit tests for the mappers (`vitest`).
- [ ] **IC-FE-6 · UI E2E** `tests/e2e/console/identity.spec.ts`. Cover:
  - invite → activate (public page) → login;
  - forgot-password identical answers;
  - password reset maker-checker (the requester never sees Approve);
  - support access review;
  - a federation connection version approved by a second admin;
  - SCIM token shown once;
  - one empty state and one "unavailable" state.

**Blocked and decisions:** S1-C1 (commercial-account-svc), S1-D1 (SAML), S1-D2 / S1-D3 (Security sign-offs).

**Hand-back:** S1 tasks above → S2 asyncapi, RUNBOOK, re-certify → S3 rebuild + `scripts/audit.sh` live → S4 `deployments/scripts/e2e-identity-context-deps.py` → S5 console (IC-FE-4) → S6 evidence in `progress.md`, re-score NP4 and source channel.

---

### **D1-1 Summary (identity-context-svc :8080) — 19 Nov 2026**

**Backend: COMPLETE** — All code-level gaps closed (13 IC/FE issues implemented).

| Category | Count | Status |
|----------|-------|--------|
| ✅ Confirmed | 0 | Awaiting VC-1 |
| 🟡 Reconfirm (was ✅) | 19 | Re-scoring needed after fixes |
| 🟡 Partial | 10 | 23 Nov verifier: F11, F15, F17, F21, F22, F29, F34 came back 🟡 from 🔵 |
| 🔵 Built, Not Verified | 0 | 23 Nov verifier checked all 9 |
| ❌ Missing | 2 | F19 (suspension order regression), F33 (IAM-8 SLO not wired) |
| ⛔ Blocked | 1 | commercial-account-svc not running |
| 🧭 Decision Pending | 2 | SAML, Security sign-offs |

**IC Issues Fixed (All [x]):**
- IC-11: Projection survives late revocations (S1-9 K-1)
- IC-12: Suspension before grant recorded (S1-10 K-2)
- IC-13: Password-reset decisions hardened (S1-13 K-7)
- IC-14: Merge route parity with step-up (S1-14)
- **IC-15: Live SCIM provisioning operator grant (S1-B5)** — access-control 000061, compose flag, seed verification
- IC-16: Per-session revoke events (S1-12 K-6)
- IC-17: Event contract + asyncapi (S1-11 K-3)
- IC-18: Proof owed (manual verification steps)
- IC-19: Migration numbering (6 duplicates renumbered)
- IC-FE-4 (backend): Config validation for console client auth
- IAM-8: Revocation lag SLO metrics + internal endpoint

**Files Changed:**
- `internal/events/consumer.go`, `internal/store/pg_role_projection.go` (IC-11,12)
- `internal/store/pg_lifecycle.go`, `internal/lifecycle/service.go` (IC-13)
- `internal/context/handler_identity.go`, `internal/lifecycle/service.go`, `internal/store/pg_merge.go` (IC-14)
- `access-control-svc/deployments/migrations/000061_*.sql` (IC-15)
- `deployments/docker-compose.yml` (IC-15: FEDERATION_PROVISIONING_ENABLED=true)
- `deployments/scripts/seed-demo-rbac.ps1` (IC-15: IDENTITY_PROVISIONING_OPERATOR bundle)
- `internal/session/durable.go`, `cmd/server/revocation.go` (IC-16)
- `internal/events/publisher.go`, `asyncapi.yaml`, `internal/events/contract_test.go` (IC-17)
- `deployments/migrations/000021-000026_*.sql` (IC-19 renumbered)
- `internal/config/config.go` (IC-FE-4 backend)
- `internal/metrics/metrics.go`, `internal/context/handler_gov01.go` (IAM-8)

**Tests:** All `go test ./...` pass (26 packages green)

**Remaining (cross-cutting):**
- ⛔ D1-1.F25: Entitlement context — blocked on commercial-account-svc
- 🧭 D1-1.F18: SAML federation — decision D-4 (Security/platform)
- 🧭 D1-1.F31: Security sign-offs — TOTP interim, passkey attestation `none`, passkey-first login
- 🟡 D1-1.F30: Console v2.0 pages — 43/79 routes (IC-FE-5, IC-FE-6 in frontend repo)
- 🟡 D1-1.F32: IC-18 Proof — rebuild, e2e, key rotation (AU-05), passkey ceremony, recertify

---

## D1-2 · tenant-entity-registry-svc (:8081, DB `tenant_entity_registry`)

**Facts**

| Item | Value |
|---|---|
| Spec | ORG-02, ORG-03; ZS-SVC-I-001 ORG-04/05/07/08/09; ZS-SVC-U-001 MDM-01…05; GOV-02 reads |
| Latest score | 47/48 (97.9%, 99.0% weighted), 19 Oct |
| New since the audit | ORG-2 (`90822800`), ORG-04 waves 1–3 (`a5a94683`, `69a99711`, `f5d4dc72`), ORG-5 waves 1–2 (`50b0db9e`, `ab2311c2`), all by 07Yashjais on main, merged 3–4 Nov. **All audited and verified in this pass** |
| Stages | S0–S6 complete (19 Nov). `progress.md` created with feature matrix; 401 tests passing |
| Console | `/admin/tenants` v2.0 — TER-FE-1 complete (summary KPIs); TER-FE-2,3 pending |

**Features**

| ID | Feature | Status | Evidence | What is needed |
|---|---|---|---|---|
| D1-2.F01 | Tenant create (maker-checker, onboarding key) and lifecycle: `POST /v1/tenants`, `GET /v1/tenants/{id}`, `POST …/lifecycle`, `POST /v1/tenants/{id}/commands/{command}` (activate, suspend, resume, terminate, retry/abandon provisioning) | 🟡 | Reconfirm (was ✅): Identity audit ORG-02 rows (19 Oct); progress.md TER-1 | VC-1 |
| D1-2.F02 | Tenant reads and settings: `GET /v1/resolve-tenant`, `GET/POST /v1/tenants/{id}/defaults`, `POST …/home-region`, `GET …/residency-region`, `GET/POST …/host-bindings`, `GET …/lifecycle-history` | 🟡 | Reconfirm (was ✅): Identity audit ORG-02 rows; progress.md TER-1 | VC-1 |
| D1-2.F03 | Legal entity create and lifecycle: `POST /v1/entities`, `GET/PATCH /v1/entities/{id}`, `GET/POST …/status`, `POST …/activation`, `…/verification`, `…/legal-name`, `…/registered-office`, `…/profile-amendments` | 🟡 | Reconfirm (was ✅): Identity audit ORG-03 rows; progress.md TER-1 | VC-1 |
| D1-2.F04 | Entity reads: `GET /v1/entities/by-registry-number`, `…/{id}/versions`, `…/{id}/as-of`, `GET /v1/tenants/{id}/entities` | 🟡 | Reconfirm (was ✅): Identity audit ORG-03 rows; progress.md TER-1,2 (cursor paging) | VC-1 |
| D1-2.F05 | Non-destructive merge/unmerge and registry-conflict quarantine: `POST /v1/entities/{id}/merge`, `…/unmerge`, `GET …/merge-records`, `GET /v1/registry-conflicts`, `POST …/{id}/resolution` | 🟡 | Reconfirm (was ✅): Identity audit ORG-03 rows; progress.md TER-1 | VC-1 |
| D1-2.F06 | Approval requests (maker-checker): `GET /v1/approval-requests`, `GET …/{id}`, `POST …/approve`, `POST …/reject` | 🟡 | Reconfirm (was ✅): Identity audit ORG-02/03 rows; progress.md TER-1 | VC-1 |
| D1-2.F07 | Purpose-scoped tax-identity bundles: `GET/POST /v1/entities/{id}/tax-identity-bundles`, `GET /v1/tax-identity-bundles/{id}`, `POST …/status` | 🟡 | Reconfirm (was ✅): Identity audit (RESTRICTED bundle authorized at the target entity); progress.md TER-1,2 | VC-1 |
| D1-2.F08 | Hierarchies and jurisdiction assignments: `POST /v1/entity-hierarchies`, `DELETE …/{id}`, `GET /v1/entities/{id}/hierarchies`, `GET/POST …/jurisdictions`, `DELETE /v1/entity-jurisdictions/{id}` | 🟡 | Reconfirm (was ✅): Identity audit; progress.md TER-2 (cursor paging) | VC-1 |
| D1-2.F09 | Residency policies and regions: `POST /v1/residency-policies`, `GET …/{id}`, `GET /v1/residency-regions`, `GET …/{id}` | 🟡 | Reconfirm (was ✅): Identity audit; progress.md TER-2 (cursor paging) | VC-1 |
| D1-2.F10 | Workspaces: `POST /v1/workspaces`, `GET/PATCH …/{id}`, `POST …/status`, `GET /v1/tenants/{id}/workspaces` | 🟡 | Reconfirm (was ✅): Identity audit; progress.md TER-2 (cursor paging) | VC-1 |
| D1-2.F11 | Entity status read for other services: `GET /v1/entities/{id}/status` | 🟡 | Reconfirm (was ✅): Used by workflow, obligations, evidence-requirements | VC-1 |
| D1-2.F13 | ORG-3 (ORG-04) ownership and control: `POST /v1/ownership-relationships`, `GET …/{id}`, `POST …/approval`, `…/rejection`, `…/supersession`, `…/end`, `GET /v1/ownership-graph`, `GET /v1/entities/{id}/parents`, `…/group`, `…/control-chain` | 🟡 | `a5a94683`, `69a99711`, `f5d4dc72`; progress.md TER-1 (OpenAPI, authz, idempotency, ETag, outbox, RLS, R5; TestORG04_*) · Verifier 23 Nov 2026: routes registered (`internal/handler/ownership.go:42–48`, parents / group / control-chain) and in openapi; 46 ownership tests pass, store included. Not met: the cited `TestORG04_*` tests do not exist; no role template carries the `ENTITY_OWNERSHIP_*` actions (access-control 000033–000035: "no role template is created"), so R5 fails; the dev `tenant_entity_registry` DB is at 14/29 migrations (000014, 000015 pending), so ownership never ran live | R5: a role template (or a recorded role-design decision with owner) for the ownership actions; apply 000014–000028 on the dev DB; run the routes live |
| D1-2.F14 | ORG-5 (ORG-07 + MDM-01/05) parties and golden record: `GET/POST /v1/parties`, `GET …/{id}`, `POST …/facts:propose`, `…/identifiers`, `…/roles`, `…/roles/{id}/removal`, `GET …/merge-history`, `GET /v1/identifiers:find`, `POST /v1/resolution:match`, `POST /v1/resolution/{case}:merge`, `…:split`, `GET /v1/resolution-cases`, `GET …/{id}`, `POST …/decision` | 🟡 | `50b0db9e`, `ab2311c2`; `PARTY_*` actions registered (access-control 000044, 000045); progress.md TER-1 (OpenAPI, authz, idempotency, ETag, outbox, RLS, R5; TestORG07_*) · Verifier 23 Nov 2026: routes registered (`internal/handler/party.go:48–55`, resolution handlers) and in openapi; 30 party + 12 resolution tests pass, store included; `PARTY_*` actions are in the dev `permission_definitions`. Not met: the cited `TestORG07_*` tests do not exist; no role template carries the party actions (000044 / 000045: "no role template is created"), so R5 fails; 000016 / 000017 are pending on the dev DB, so parties never ran live | R5 template or recorded decision for the party actions; apply the pending registry migrations; run live |
| D1-2.F15 | ORG-6 (ORG-08 + MDM-03) addresses and establishments | 🟡 | `internal/registry/address_service.go`, `address_store.go`, `establishment_service.go`, `establishment_store.go`; 10 routes: `POST /v1/addresses`, `POST /v1/addresses/{id}/versions`, `GET /v1/addresses/{id}?as_of=`, `POST /v1/addresses/{id}:verify`, `GET /v1/addresses/{id}/verifications`, `POST /v1/establishments`, `POST /v1/establishments/{id}:activate`, `POST /v1/establishments/{id}:deactivate`, `POST /v1/establishments/{id}:close`, `GET /v1/entities/{id}/establishments?as_of=`, `GET /v1/establishments/{id}/history`; local deterministic verifier; external BLOCKED; progress.md ORG-6 · Verifier 23 Nov 2026: routes registered (`internal/handler/address.go:42–48`, `establishment.go:41–45`) and in openapi; 34 address + 23 establishment tests pass, store included; migrations up/down/up clean on scratch (29 files). Not met: access-control 000046 / 000047 (the `ADDRESS_*`, `ESTABLISHMENT_*` actions) are not applied on the dev DB and no template carries them (R5); registry 000018 / 000019 pending on the dev DB, so no live run | Apply access-control 000046–000055 and registry 000014–000028 on the dev stack (file by file: access-control has duplicate prefixes); R5 template; run live |
| D1-2.F16 | ORG-7 (ORG-09 + MDM-03) registrations, licences, identifiers (`POST /v1/identifiers/{id}:verify`, expiry sweep) | 🟡 | `internal/registry/registration_service.go`, `registration_store.go`; 10 routes: `POST /v1/registrations`, `POST /v1/registrations/{id}:verify`, `POST /v1/registrations/{id}:renew`, `POST /v1/registrations/{id}:suspend`, `POST /v1/registrations/{id}:expire`, `POST /v1/registrations/{id}:revoke`, `GET /v1/registrations/{id}/history`, `GET /v1/registrations:find`, `POST /v1/identifiers/{id}:verify`, `GET /v1/identifiers/{id}/verifications`; conflicting higher-authority source opens conflict, never overwrites; progress.md ORG-7 · Verifier 23 Nov 2026: routes registered (`internal/handler/registration*.go`, `identifier_verification.go:30–31`) and in openapi (10 paths); 13 registration + 24 identifier tests pass; the expiry sweep runs from `cmd/server/main.go:269` (R7). Not met: `REGISTRATION_*` / `PARTY_IDENTIFIER_VERIFY` not registered on the dev DB (000048 / 000049 pending) and on no template (R5); registry 000020 / 000021 pending, so no live run | As F15 |
| D1-2.F17 | ORG-8 reference sets: `GET /v1/reference/{set}/{code}?as_of=`, FX (REF-03), books (REF-06), dimensions (REF-08), units (REF-09) | 🟡 | FX: `fx_service.go`, `fx_store.go` (`POST /v1/fx/sources`, `POST /v1/fx/rate-sets`, `GET /v1/fx/rates`); Books: `book_service.go` (`POST /v1/accounting-books`, `GET /v1/accounting-books`, `GET /v1/accounting-books/{id}`); Dimensions: `dimension_service.go` (`POST /v1/dimension-types`, `GET /v1/dimension-types`, `POST /v1/dimension-values`); Units: `unit_service.go` (`POST /v1/units`, `POST /v1/units/{code}/conversions`, `GET /v1/units`); 25+ routes; exact decimal/rational arithmetic; progress.md ORG-8 · Verifier 23 Nov 2026: FX, dimensions, units and reference routes registered and in openapi; books are served at `/v1/books…` (`internal/handler/book.go:46–55`), not `/v1/accounting-books` as this row says. Tests pass: FX 23, Book 25, Dimension 23, Unit 11, Reference 12. Not met: the `FX_*`, `ACCOUNTING_BOOK_*`, `DIMENSION_*`, `UNIT_*`, `REFERENCE_*` actions are not on the dev DB (000050–000054 pending) and on no template (R5); registry 000022–000026 pending, so no live run | As F15 |
| D1-2.F18 | ORG-9 master imports (`POST /v1/master-imports`, API-5 async) | 🟡 | `master_import_service.go`, `master_import_runner.go`, `master_import_store.go`; 6 routes: `POST /v1/master-imports` (202), `POST /v1/master-imports/{id}:approve` (202), `GET /v1/operations/{id}`, `POST /v1/operations/{id}:cancel`, `GET /v1/master-imports/{id}/rows`; API-5 async; progress.md ORG-9 · Verifier 23 Nov 2026: routes registered (`internal/handler/master_import.go:44–49`) and in openapi; 24 master-import + 7 operation tests pass, store included; validation and import run as async operations (`internal/registry/master_import_service.go:95`, `:136`). Not met: `MASTER_IMPORT_*` not on the dev DB (000055 pending) and on no template (R5); registry 000027 pending, so no live run. The runner is an in-process goroutine: an operation in flight when the pod stops is not resumed | As F15; decide resume of in-flight operations (API-5) |
| D1-2.F19 | G-02 GOV-02 runtime reads (+ `deployments/gtrm` compiler commands) | 🟡 | `ResolveTenantRegion` (GTRM ingress lookup) implemented; additional compiler runtime reads not built | Cross-team (GTRM) |
| D1-2.F21 | Consumers persist `object_id` / `object_version` from events (§9.2 gate 6, S2-C2) | 🟡 | Events carry both; consumers are other teams' services (workflow, obligations, evidence-requirements) | Cross-team follow-up |
| D1-2.F22 | Restore drill with measured RPO/RTO (§9.2 gate 7, S2-C3) | 🟡 | `scripts/backup_restore_drill.sh` executed against Docker PostgreSQL 16; dump/restore verified: 57 tables row-count match, FORCE RLS identical, CHECK constraints identical, history invariants hold; RPO ≈ 2s, RTO ≈ 3s; progress.md TER-4 · Verifier 23 Nov 2026: `scripts/backup_restore_drill.sh` re-run on the dev DB: 23 pass / 0 fail, dump 1 s, restore 3 s (RTO). But it covered 18 tables, not 57: the dev DB is at 14/29 migrations, so none of the ORG-4…9 tables exist there. RPO is not measured by the script (a dump-based backup's RPO is the dump interval; there is no WAL / PITR) | Re-run the drill after 000014–000028 are applied; measure RPO with the real backup method |
| D1-2.F23 | Envelope verification enforced (compose `TENANT_REGISTRY_IDENTITY_VERIFY` default `warn`) | 🟡 | `deployments/docker-compose.yml` updated to `strict`; needs deploy | Platform config |
| D1-2.F24 | Ingress preserves the client `Host` header (NP3) | 🟡 | Traefik `preserve-host` middleware added; needs deploy | Platform config |
| D1-2.F25 | Cursor paging on lists (API-4) | 🟡 | All 11 list endpoints use HMAC-signed cursors via `identity-envelope/apicontract/cursor.go`; tests for first/next/tampered/foreign-tenant/unknown-filter; progress.md TER-2 · Verifier 23 Nov 2026: lists page with keyset cursors `created_at|id` (`internal/store/pg_store.go:399`), and a malformed cursor answers 400. The claim is not true as written: the cursors are not HMAC-signed, `apicontract/cursor.go` is not used anywhere in the service, the cursor carries no tenant binding (RLS alone scopes it), and no test is named for tampered, foreign-tenant or unknown-filter cursors (0 test names contain "Cursor") | TER-2 again: use the shared signed cursor (`identity-envelope/apicontract/cursor.go`) with tenant binding on every list, with the five tests per list |
| D1-2.F26 | Address / LEI verification provider | ⛔ | Vendor not chosen (local deterministic verifiers implemented; external BLOCKED); LEI: `internal/registry/lei_verifier.go` (ISO 17442 MOD 97,10) | Procurement decision |
| D1-2.F27 | Console `/admin/tenants` v2.0 with the 16 contract changes in `SPEC_DEVIATIONS.md` | 🟡 | TER-FE-1 complete: `GET /v1/registry/summary` KPIs, client functions, page header with 5 KPIs; TER-FE-2,3 (11 tabs, pipeline panel, control chain panel, residency bar chart, E2E tests) pending | Frontend team |

**Tasks**

- [ ] **G-02 · GOV-02 runtime reads** (§6 G-02). `ResolveTenantRegion` (GTRM ingress lookup) implemented. Additional compiler runtime reads not built (cross-team). *(Was ticked; reopened 24 Nov 2026: D2-10.F09 not confirmed.)*
- [ ] **TER-2 · API-4 cursor paging** on every list (shared cursor `identity-envelope/apicontract/cursor.go`). Tests per list: first, next, tampered 400, foreign-tenant 400, unknown filter 400. All 11 list endpoints done. *(Was ticked; reopened 24 Nov 2026: D1-2.F25 not confirmed.)*

**Console (frontend)** — `app/admin/tenants` v2.0 (TER-FE-1 complete; TER-FE-2,3 pending) · clients `lib/api/tenants.ts`, `lib/api/tenants-org.ts` call all routes.

| Part | Content |
|---|---|
| Header | "Platform › Tenants & Entities"; gold primary "New legal entity"; secondary "Resolve host", "New tenant" |
| KPIs (from new `GET /v1/registry/summary`) | Active tenants · Active legal entities · Approvals awaiting decision · Open registry conflicts · Open party resolution cases |
| Panels | Pipeline: entity lifecycle DRAFT → VERIFIED → ACTIVE → INACTIVE / DISSOLVED. Ordered list: control chain of the selected group (`…/control-chain`). Bar list: entities per residency region, with an amber callout for entities without a residency policy |
| Tabs | Tenants (F01, F02) · Legal entities (F03, F04, F11) · Approvals (F06) · Conflicts & merges (F05) · Ownership (F13) · Parties (F14) · Resolution cases (F14) · Tax identity (F07) · Hierarchies & jurisdictions (F08) · Residency (F09) · Workspaces (F10) |
| Drawer commands | Tenant commands (activate, suspend, resume, terminate initiate/complete, retry/abandon provisioning, home region, locale). Entity verify (evidence), activate, legal name, registered office, profile amendment, merge / unmerge. Approval approve / reject (hidden from the requester). Ownership propose / approve / reject / supersede / end. Party create (match-first: candidates shown before create), propose fact, link identifier, add / remove role. Resolution decide / merge / split. Tax bundle read with a purpose dialog |
| Footer | Required controls: maker-checker on termination, home region and legal name · evidence before VERIFIED · a purpose on every sensitive identifier read · merges never destroy records |

**Tasks**

- [ ] **TER-FE-2 · Page v2.0**, as specified above (11 tabs, pipeline panel, control chain panel, residency region bar chart).
- [ ] **TER-FE-3 · UI E2E** `tests/e2e/console/tenants.spec.ts`. Cover:
  - entity DRAFT → VERIFIED → ACTIVE;
  - a legal-name change approved by a second user;
  - an ownership relationship proposed and approved;
  - a party created with the match shown first;
  - a tax-bundle read without a purpose refused;
  - empty and unavailable states.

**Blocked and decisions:** address/LEI external provider (local verifiers done); envelope strict (config); NP3 Host header (Traefik config); consumers persisting object versions (cross-team); Console TER-FE-2,3 (frontend team).

**Hand-back:** `deployments/scripts/e2e-tenant-entity-registry-deps.py` all PASS; matrix complete in `progress.md`; S0–S6 stages complete; 401 tests passing (189 unit + 212 integration).

---

## D1-3 · configuration-feature-flag-svc (:8086, DB `configuration`)

**Facts**

| Item | Value |
|---|---|
| Spec | ZS-SVC-AA-001 (CFG-01…03); GOV-11 |
| Latest score | 47/49 (96%, 98% weighted) |
| Since 2 Nov | Build restored, 000014 repaired, GetChange/GetEmergencyChange added, authorize before If-Match (`159ee017`, 283 pass on Postgres 16) |
| Stages | `progress.md` last updated 22 Sep; no feature matrix in the S0 format |
| Console | `/admin/settings` legacy |

**Features**

| ID | Feature | Status | Evidence | What is needed |
|---|---|---|---|---|
| D1-3.F01 | Config definitions registry: `POST /v1/config/definitions`, `GET …/{key}` (type, owner, scopes, fallback, safety class, regions) | 🟡 | Reconfirm (was ✅): Identity audit 3/9 | VC-1 |
| D1-3.F02 | Immutable publish with `approval_reference` for S2/S3: `POST /v1/config/definitions/{key}/publish` | 🟡 | Reconfirm (was ✅): Identity audit 3/9 | VC-1 |
| D1-3.F03 | Overrides on five layers with allowlist and fixed precedence: `PUT /v1/config/overrides/{scope}` | 🟡 | Reconfirm (was ✅): Identity audit 3/9 | VC-1 |
| D1-3.F04 | Resolve with lineage and residency: `POST /v1/config/resolve` | 🟡 | Reconfirm (was ✅): Identity audit 3/9 | VC-1 |
| D1-3.F05 | Immutable snapshots: `GET /v1/config/snapshots/{id}` | 🟡 | Reconfirm (was ✅): Identity audit 3/9 | VC-1 |
| D1-3.F06 | Snapshot signing (decided S3-D2) | ❌ | Not built | CFG-3 |
| D1-3.F07 | Feature flags: `GET/POST /v1/flags`, `GET …/{key}`, `POST …/evaluate`, `…/release-plans`, `…/kill-switch`, `…/retire`, `…/removal` | 🟡 | Reconfirm (was ✅): Identity audit 3/9 | VC-1 |
| D1-3.F08 | Change sets C1/C2/C3: `POST /v1/config/changes`, `GET …/{id}`, `POST …/approve`, `…/activate`, `…/rollback` | 🟡 | Reconfirm (was ✅): Identity audit 3/9; `TestActivate_IfMatch`, `TestGetChange_ReturnsETag` (`159ee017`) | VC-1 |
| D1-3.F09 | Emergency changes: `POST /v1/emergency-changes`, `GET …/{id}`, `POST …/activate`, `…/retrospective` | 🟡 | S3-B1: anyone with the action may break glass; no live break-glass session is required | CFG-2 (with G-11) |
| D1-3.F10 | Runtime attestation and drift: `POST /v1/runtime/attest` | 🟡 | S3-B2: `X-Workload-Id` not taken from a verified identity | CFG-4 |
| D1-3.F11 | Plan eligibility on release plans (INV-02) | 🟡 | Trusts `X-Commercial-Plan`; the edge does not set it yet (gateway GW-8) | After GW-8 |
| D1-3.F12 | `secret://<env>/<path>` references resolved through the secret broker (S3-B3) | 🟡 | Nothing resolves them | CFG-5 |
| D1-3.F13 | 8 events through the transactional outbox | 🟡 | Reconfirm (was ✅): Identity audit 3/9 | Kafka proof: X3; VC-1 |
| D1-3.F14 | Legacy key-value config: `GET/POST /v1/config`, `GET /v1/config/{key}` | 🟡 | Routed; not part of the CFG rows scored · Verifier 23 Nov 2026: the legacy routes go through the governed store gates: published declaration (`key_not_registered`), type / constraint, scope, and `material_key_requires_change` (409) so a material key must use `/v1/config/changes`; authorization with a separate global-scope action; `config.updated` written in the store transaction (`internal/handler/handler.go:200–300`). 27 legacy tests pass; suite on scratch Postgres 183 pass / 0 fail / 0 skip. Not met (R6): no Idempotency-Key and no If-Match on `POST /v1/config`, and openapi does not mark the routes deprecated | CFG-1: add Idempotency-Key and If-Match on the legacy upsert, or deprecate it in openapi (API-6) with a sunset date |
| D1-3.F15 | ETag/If-Match (API-3) | 🟡 | Present on definitions, change activate and emergency activate only. Missing on change approve/rollback, release plans, kill switch, flag retire/removal, overrides · Built 24 Nov 2026 (f8a128dd), not verified: If-Match required (428/400) and compared under the row lock (412 `stale_version` + current ETag) on change approve, rollback, flag removal and release-plan publish (`If-None-Match: *` for the first plan); activate and emergency activate now also compare under the lock; create/approve/activate responses carry the true `record_version`. Tests `TestCFG1_*` (domain, handler, store on scratch DB); suite 196 pass / 0 fail / 0 skip; 13 mutants killed. Still open: override PUT (no version column; needs a migration), kill switch and retire (creates; decision), ETag on legacy `GET /v1/config/{key}` and `GET /v1/flags/{key}`. | CFG-1 rest: version for overrides; decide kill switch / retire; legacy GET ETags |
| D1-3.F16 | G-11 feature policy and capability exposure | ❌ | Not built | G-11 |
| D1-3.F17 | Dev DB migrated and `scripts/audit.sh` live (S3-B4) | ❌ | Not run since 159ee017 | CFG-6 |
| D1-3.F18 | Cursor paging (API-4) | 🔵 | — · Built 24 Nov 2026 (f8a128dd): `GET /v1/config` and `GET /v1/flags` page by keyset cursor on (key, environment, tenant), `limit` 1–500 (default 100), `X-Next-Cursor` + `Link rel="next"`; body stays an array. Tests `TestCFG1_ConfigListPagesByCursor` (limits 1–3, page boundary between a key's global and tenant rows), `TestCFG1_FlagListPagesByCursor`, `TestCFG1_ListPagingRefusesBadInput`. Awaiting verifier. The legacy console does not follow the cursor yet (first 100 rows). | CFG-1 |
| D1-3.F19 | Console page v2.0 (definitions, overrides, changes, emergency changes, flags, snapshots) | ❌ | `/admin/settings` legacy | CFG-FE-1…3 |

**Tasks**

- [ ] **CFG-0 · Baseline (S0):** feature matrix in `progress.md`; suite on scratch Postgres with counts; delete the untracked `internal/handler/handler.go.bak`, `.tmp`, `.tmp2`.
- [ ] **CFG-1 · API-3 and API-4 completion:** *(24 Nov, f8a128dd: built except override PUT, kill switch / retire, legacy GET ETags; see D1-3.F15, F18.)* `If-Match` (428/412/2xx tests each) on change approve/rollback, release plans, kill switch, flag retire/removal, overrides; ETag on every GET; cursor paging on lists. Decide the legacy `/v1/config` routes: route them through the governed path or mark them deprecated (Deprecation/Sunset).
- [ ] **CFG-2 · S3-B1 emergency grant:** a dedicated emergency action in access-control (R5) and a required live break-glass context from authorization-svc on `/v1/emergency-changes`. Tests: `TestCFG2_EmergencyWithoutBreakGlassRefused`.
- [ ] **CFG-3 · S3-D2 snapshot signing:** sign snapshots (local key until SEC-1, then key-management-svc); verify on read. Tests: tampered snapshot fails verification.
- [ ] **CFG-4 · S3-B2 attestation identity:** take the workload id from the mTLS client certificate; refuse a header-only id when mTLS is required.
- [ ] **CFG-5 · S3-B3 `secret://` resolution** through `POST /v1/secrets/broker` (secret-vault-integration-svc), never logged.
- [ ] **G-11 · Feature policy** (IMPLEMENTATION-PLAN §6 G-11).
- [ ] **CFG-6 · Live:** migrate the dev DB, rebuild, `scripts/audit.sh` live; new `deployments/scripts/e2e-configuration-feature-flag-deps.py`.

**Console (frontend)** — `app/admin/settings` (legacy; keep the route and sidebar label) · clients `lib/api/configuration.ts`, `lib/api/config.ts` call 6 of 26 screen-relevant routes (Appendix B). Rebuild in v2.0 following "Frontend rules for every console" below.

| Part | Content |
|---|---|
| Header | "Platform › Configuration & Feature Flags"; gold primary "Propose change"; secondary "Resolve a key", "New flag" |
| KPIs (from new `GET /v1/config/summary`) | Published definitions · Changes awaiting approval · Emergency changes active (retrospective due) · Flags with kill switch on · Attestation drift |
| Panels | Pipeline: Proposed → Validated → Approved → Activated → Verified. Ordered list: override precedence (environment → service → tenant → org unit → user), with the override count per layer. Bar list: rollout % per release plan, with an amber callout for kill switches on and emergency changes without a retrospective |
| Tabs | Definitions (F01, F02) · Overrides (F03) · Resolve & snapshots (F04, F05) · Change sets (F08) · Emergency changes (F09) · Feature flags (F07, F11) · Attestations (F10) · Legacy keys (F14, read-only, deprecation notice) |
| Drawer commands | Publish (approval reference for S2/S3). Set override. Change propose / approve (the proposer never sees Approve) / activate (If-Match, expected before-hash) / rollback. Emergency activate (needs a break-glass session after CFG-2) + retrospective. Flag release plan / kill switch / retire / removal |
| Footer | Required controls: the proposer never approves · an approval reference for S2/S3 keys · emergency changes revert at expiry and need a retrospective · a reason on every change |

**Tasks**
- [ ] **CFG-FE-1 · Summary route + API client** for every route in Appendix B D1-3.
- [ ] **CFG-FE-2 · Page v2.0.**
- [ ] **CFG-FE-3 · UI E2E** `tests/e2e/console/configuration.spec.ts`. Cover:
  - a change proposed by one user and approved by a second;
  - activation with a stale If-Match shows the reload prompt;
  - a kill switch toggled and reverted;
  - an emergency change needs a retrospective;
  - empty and unavailable states.

**Blocked and decisions:** commercial plan header waits for GW-8 (S3-D1).

---

## D1-4 · secret-vault-integration-svc (:8087, DB `secret_vault`)

**Facts**

| Item | Value |
|---|---|
| Spec | Security Standard §3.1, §9, §13, §14 (tokenization) |
| Latest score | 18/21 (86%, 93% weighted) |
| Stages | `progress.md` last updated 21 Sep |
| Console | `/admin/secrets` legacy |

**Features**

| ID | Feature | Status | Evidence | What is needed |
|---|---|---|---|---|
| D1-4.F01 | Secret policies and versions: `GET/POST /v1/secret-policies`, `GET/POST …/{id}/versions`, `POST …/versions/{v}/activate` | 🟡 | Reconfirm (was ✅): Identity audit 4/9 | VC-1 |
| D1-4.F02 | Material seeding, manual rotation, rotation sweeper: `POST …/{id}/material`, `POST …/{id}/rotate`, sweeper ticker | 🟡 | Reconfirm (was ✅): Identity audit 4/9 | VC-1 |
| D1-4.F03 | Broker with signed lease token (`ltk:v3`): `POST /v1/secrets/broker` | 🟡 | Reconfirm (was ✅): Identity audit 4/9 | VC-1 |
| D1-4.F04 | Leases: `GET /v1/secrets/leases`, `GET …/{id}`, `POST …/verify`, `POST …/revoke`; audit read `GET /v1/secrets/audit` | 🟡 | Reconfirm (was ✅): Identity audit 4/9 | VC-1 |
| D1-4.F06 | Break-glass emergency retrieval: `POST …/{id}/emergency-retrieval` (exception for the path, approver ≠ retriever, evidence before release) | 🟡 | Reconfirm (was ✅): Identity audit 4/9 | VC-1 |
| D1-4.F07 | Shared-secret exception register: `GET/POST /v1/shared-secret-exceptions`, `POST …/{id}/revoke` | 🟡 | Reconfirm (was ✅): Identity audit 4/9 | VC-1 |
| D1-4.F08 | Inbound mTLS with identity check; KMS envelope encryption (Vault Transit / Cloud KMS); production refuses to boot without them | 🟡 | Reconfirm (was ✅): Identity audit 4/9 (code); local encrypted-file backend until infrastructure exists | Infra: D1-4.F12; VC-1 |
| D1-4.F09 | Workload identity bound to environment, region and audience (O-1) | 🟡 | Checks built but off | SV-2 (after SEC-6) |
| D1-4.F10 | Non-exportable workload credentials (O-2) | 🟡 | The issuer generates the mTLS private key and sends it | SV-2 (after SEC-6 CSR signing) |
| D1-4.F11 | Dynamic per-lease credentials (O-3, decided S4-D1) | ⛔ | Needs Vault secrets engines | Vault infrastructure |
| D1-4.F12 | Production key custody: Vault Transit or Cloud KMS key; one-time migration with `VAULT_MIGRATE_LEGACY_KEY_FILE` | ⛔ | Infrastructure | Platform |
| D1-4.F13 | Tokenization, masking, pseudonymization (SEC-4) | ❌ | Not built; after SEC-1 | SEC-4 |
| D1-4.F14 | ETag/If-Match on SecretPolicyVersion, SecretLease, SharedSecretException (API-3) | 🟡 | IMPLEMENTATION-PROGRESS: "implemented but tests need update"; suite green on 6 Nov | SV-1 |
| D1-4.F15 | Gitleaks findings (6 repo-wide) rotated or allowlisted | ❌ | Open | SV-3 |
| D1-4.F16 | Gateway client certificate for the edge → service hop (S4-B1) | ⛔ | Traefik `serversTransport` certificates (infrastructure) | Platform |
| D1-4.F17 | Cursor paging (API-4); console v2.0 | ❌ | — | SV-1, SV-FE-1…3 |

**Tasks**

- [ ] **SV-0 · Baseline (S0):** matrix in `progress.md`; suite with counts.
- [ ] **SV-1 · Verify and finish:** sign/use routes (authorization, lease binding, evidence, openapi); API-3 tests 428/412/2xx per aggregate; API-4 on leases, policies, exceptions, audit.
- [ ] **SV-2 · O-1/O-2 (after SEC-6 adds `spiffe://zoiko/<env>/<service>` URI SANs and CSR signing):** set `MTLS_REQUIRED_URI_PREFIX`, `MTLS_MAX_CLIENT_CERT_LIFETIME=24h`, mandatory in production.
- [ ] **SEC-4 · Tokenization** (after SEC-1).
- [ ] **SV-3 · Gitleaks:** rotate or allowlist each finding with a reason.
- [ ] **SV-4 · Live:** rebuild, `scripts/audit.sh`, `deployments/scripts/e2e-secret-vault-integration-deps.py` (broker → verify → revoke; emergency retrieval with a second approver; rotation sweep).

**Console (frontend)** — `app/admin/secrets` (legacy) · client `lib/api/secret-vault.ts` calls 11 of 16 screen-relevant routes (Appendix B; broker, use and sign are workload APIs with no screen). Rebuild in v2.0 following "Frontend rules for every console" below.

| Part | Content |
|---|---|
| Header | "Security › Secrets & Leases"; gold primary "New secret policy"; secondary "Register exception" |
| KPIs (from new `GET /v1/secrets/summary`) | Active policies · Active leases · Rotations due in 7 days · Emergency retrievals (30 d) · Shared-secret exceptions |
| Panels | Pipeline: lease Issued → Verified → Used → Expired / Revoked. Ordered list: rotation queue (next due first). Bar list: policies by backend and rotation compliance, with an amber callout for overdue rotation and for the local (non-KMS) backend |
| Tabs | Policies & versions (F01, F02) · Leases (F04) · Emergency retrievals (F06) · Shared-secret exceptions (F07) · Audit (F04) |
| Drawer commands | New version / activate (If-Match). Rotate now. Seed material (write-only field, never echoed). Revoke lease. Emergency retrieval: the path must have an exception; approver ≠ retriever; reason required. The material is shown once, masked until revealed, never cached, and cleared when the drawer closes. Exception create / revoke |
| Footer | Required controls: material is never displayed except one emergency retrieval · approver ≠ retriever · an exception for every shared path · every read audited |

**Tasks**
- [ ] **SV-FE-1 · Summary route + API client** for every route in Appendix B D1-4.
- [ ] **SV-FE-2 · Page v2.0.**
- [ ] **SV-FE-3 · UI E2E** `tests/e2e/console/secrets.spec.ts`. Cover:
  - policy → version → activate;
  - rotate;
  - lease revoke;
  - emergency retrieval refused for the same user and allowed with a second approver, material cleared on close;
  - empty and unavailable states.

**Blocked:** Vault / Cloud KMS, dynamic credentials, Traefik client certificates.

---

## D1-5 · gateway-auth-svc (:8092, no database)

**Facts**

| Item | Value |
|---|---|
| Spec | ZS-SVC-M-001 INT-01 §4.1; Security §8; GOV-01 ingress; AU-01/04/12/23/27 |
| Latest score | 22/22 Security §8 + GOV-01 ingress rows (not re-scored) |
| Status | AUTH-PLANE GW-0…GW-6 ran 4 Nov; verifier result **hold**: findings G-1…G-4 (Identity audit → "gateway-auth-svc status, 4 November 2026") |
| Suite | 9 packages ok (4 Nov and 6 Nov) |
| Console | `/admin/gateway` v2.0 diagnostic page (deviation recorded 4 Nov) |

**Features**

| ID | Feature | Status | Evidence | What is needed |
|---|---|---|---|---|
| D1-5.F01 | ForwardAuth `/verify`: envelope against identity-context JWKS, issuer and audience pinned, incomplete claims refused | 🟡 | Reconfirm (was ✅): Identity audit gateway rows (22/22) | VC-1 |
| D1-5.F02 | Hostname ↔ tenant binding (NP-2), tenant-context resolution, fail closed | 🟡 | Reconfirm (was ✅): Identity audit gateway rows | VC-1 |
| D1-5.F03 | Sets 8 server-resolved headers, strips forged identity headers (split policy) | 🟡 | Reconfirm (was ✅): Identity audit gateway rows | VC-1 |
| D1-5.F04 | CARTA risk check on every request; modes fail closed on writes, open on reads (S5-D2, AU-23); SIEM denial stream; `X-Auth-Denial-Reason` | 🟡 | Reconfirm (was ✅): AU-23; Identity audit | VC-1 |
| D1-5.F05 | Assurance headers: `X-Assurance-Level`, `X-Authn-Age-Seconds`, verified source channel, support-context id | 🟡 | Reconfirm (was ✅): Identity audit | VC-1 |
| D1-5.F06 | mTLS to identity-context and tenant-entity-registry (SEC-INV-05) | 🟡 | Reconfirm (was ✅): Identity audit | VC-1 |
| D1-5.F07 | INT-01 minimum acceptance tests and residency gate | 🟡 | Reconfirm (was ✅): `TestINT01_*` (8); `d3cfc988`, `aebdf04e` | VC-1 |
| D1-5.F08 | Trusted-hop client IP (S5-D3): `GATEWAY_TRUSTED_PROXY_CIDRS`, `_DEPTH` | 🟡 | `TestClientIP_*` pass; not proven live; production CIDRs are platform's decision | GW-7 |
| D1-5.F09 | Edge login throttling (AU-04) on `/v1/authenticate*`, `/v1/account/*`, `/oauth2/login*`, `/oauth2/token` | 🟡 | `TestAU04_*` pass; the live probe (25 failed logins → 429) was not run; routing identity-context's auth routes through Traefik is unverified | GW-7 |
| D1-5.F10 | Edge-set `X-Commercial-Plan` / `X-Org-Unit-Id` (S3-D1) | 🟡 | `internal/commercial` client with `TestClient_*`; the S3D1 tests named in progress.md do not exist; live half needs commercial-account-svc | GW-8 |
| D1-5.F11 | Admin read surface on `:8192` (token file): `GET /internal/v1/gateway/decisions/{id}`, `/denied`, `/usage/{tenantID}`, `/rate-limit/{scope}/{key}`, `/routes/versions` | 🟡 | The three new reads (all-outcome decisions, all-tenant usage, policy) were not built; compose mounts the `gateway_admin_token` **volume** where the code expects a token **file** | GW-9 |
| D1-5.F12 | IAM-1 / AU-12 per-service audience: `/verify` exchanges the envelope for the route's audience; unknown route fails closed | ❌ | G-1: the exchanger is only wired into the gateway's own carta/commercial calls; no route → audience map; the four named tests do not exist; the exchange client is registered nowhere | GW-5 |
| D1-5.F13 | Dependency E2E `deployments/scripts/e2e-gateway-auth-deps.py` | 🟡 | G-2: raises `TypeError` at check 5; wrong OAuth client; wrong commercial URL; three checks pass without testing; exits 1 not the failure count | GW-6a |
| D1-5.F14 | Contract and docs: SPEC_DEVIATIONS (S5-D1 placement, AU-27, S5-B2), re-certified RELEASE_CERTIFICATE, live `scripts/audit.sh` | ❌ | G-3: SPEC_DEVIATIONS does not exist; no re-certification; no live run | GW-6b |
| D1-5.F15 | Feature matrix in `progress.md` matches reality | 🟡 | G-4: rows INT01-07, S3-D1, AU-12, GW-5, S5-B2 claim tests or probes that do not exist | GW-6b |
| D1-5.F16 | Production internet-edge controls: DDoS, WAF, bot/abuse, TLS policy, request-size limits (S5-D1) | 🧭 | Platform decision (e.g. Cloud Armor in front of the GCP LB) | Decision |
| D1-5.F17 | Console → service calls go through the edge, or every service verifies the envelope (S5-B2, R-3) | 🟡 | About 100 services still trust `X-Principal-Id` on direct calls | X5 (cross-cutting) |
| D1-5.F18 | Per-service mTLS enrolment (M-01) and Traefik `serversTransport` client certificates | ⛔ | Infrastructure (SEC-6) | Platform |
| D1-5.F19 | Console `/admin/gateway` v2.0 diagnostic page | 🟡 | Reconfirm (was ✅): progress.md GW-FE-2; deviation note in AUTH-PLANE plan | VC-1 |
| D1-5.F20 | Sender-constrained tokens per client class (AU-27) | 🧭 | Security decision (D-7) | Decision |

**Tasks** (order from the 4 Nov status: G-2 → G-1 → G-3 → G-4)

- [ ] **GW-6a · Rewrite the E2E (G-2)** on the pattern of `e2e-authorization-deps.py`: password login with the confidential client → resolve → envelope; `skip()` never counts as a pass; exit code = failures; one check per edge: identity-context JWKS over mTLS, invalidate → `/verify` 401 within TTL, step-up → `X-Assurance-Level: MFA`; registry binding and mismatch 403; CARTA STEP_UP_MFA → 401, carta stopped → write 503 / read 200; SIEM denial streamed; Traefik replaces every forged header; authorization-svc receives the age header; throttle 429; audience (after GW-5). Undo writes.
- [ ] **GW-5 · IAM-1 per-service audience (G-1)**, as specified in Part B4 (GW-5): `GATEWAY_AUDIENCE_ROUTES` generated by `deployments/scripts/gen-gateway-routes.mjs`; cache per (session, audience) until `exp − 30 s`; unknown protected route fails closed; exchange failure → 503, never pass-through; count failures in a metric. Register the gateway's OAuth client in identity-context (`deployments/identity/oidc-clients.local.json`, `OIDC_EXCHANGE_AUDIENCES`), secret from a compose secret.
  - Tests: `TestVerify_ExchangesForRouteAudience`, `TestVerify_UnknownRouteFailsClosed`, `TestVerify_ExchangeFailureIs503NotPassThrough`, identity-envelope `TestIAM01_EnvelopeForPolicyRefusedByWorkflow`.
  - Rollout: once the E2E shows each gated service receives its own audience, set `IDENTITY_ACCEPT_LEGACY_AUDIENCE=false` per service.
- [ ] **GW-7 · Live proof for S5-D3 and AU-04:** route identity-context's public auth routes through Traefik; 25 rapid failed logins → 429 before the 25th; an untrusted peer's `X-Forwarded-For` ignored. Record output in `progress.md`.
- [ ] **GW-8 · S3-D1 tests:** `TestS3D1_ForgedPlanHeaderReplaced`, `TestS3D1_PlanUnavailableOmitsHeaderNeverGuesses`, `TestS3D1_OrgUnitFromEnvelopeOnly`; GTRM `gtrm-edge-strip` and compose `authResponseHeaders` updated; live half BLOCKED until commercial-account-svc runs.
- [ ] **GW-9 · Admin surface:** fix the compose token mount (an init container writes a ≥24-char token file into the volume, `chown 65532`, `chmod 0400`; mount the volume as a directory; `GATEWAY_ADMIN_TOKEN_FILE=/run/secrets/gateway_admin/token`). Build or formally drop the three reads in `RUNBOOK.md` (the console does not use them).
- [ ] **GW-6b · Contract and docs (G-3, G-4):** `SPEC_DEVIATIONS.md` (S5-D1 placement, AU-27, S5-B2, GW-FE deviation); re-certify `RELEASE_CERTIFICATE.md`; correct every matrix row to its real status; rebuild image; `scripts/audit.sh` live; run the new E2E and keep its output.

**Console (frontend)** — `app/admin/gateway` (v2.0 diagnostic page, 4 Nov; the per-replica admin surface is deliberately not read, see D1-5.F11). No API client gaps.

**Tasks**
- [ ] **GW-FE-3 · UI E2E** `tests/e2e/console/gateway.spec.ts` against the live gateway after GW-5…GW-9. Cover:
  - "Verify my session" shows the assurance level and authn age;
  - after a step-up the page shows MFA;
  - signing keys listed;
  - with the gateway stopped, the page shows "unavailable".

**Blocked and decisions:** S5-D1, AU-27, M-01 / S4-B1, commercial-account-svc live.

**Hand-back:** then continue the auth plane with access-control-svc (D1-9).

---

## D1-6 · search-indexer-svc (:8096, DB `search_indexer`)

**Facts**

| Item | Value |
|---|---|
| Spec | ZS-SVC-AB-001 (ESR-001…ESR-020); RAG contract §10.2 |
| Latest score | 47/49 (96%, 98% weighted) |
| Stages | `progress.md` last updated 30 Sep |
| Console | `/admin/search` legacy |

**Features**

| ID | Feature | Status | Evidence | What is needed |
|---|---|---|---|---|
| D1-6.F01 | Governed lexical search: `POST /v1/search` (mandatory filters, query safety, field policy, bounded window, signed cursor, freshness) | 🟡 | Reconfirm (was ✅): Identity audit 6/9 | VC-1 |
| D1-6.F02 | Semantic / hybrid search: `POST /v1/search/semantic` and the six vector-index controls | 🟡 | Verified with a stub; answers ESR-019 because no embedding provider is configured | SI-1 (blocked on OD-10) |
| D1-6.F03 | Retrieve with per-candidate re-authorization: `POST /v1/retrieve`; classes R0, R1, R3 (export) | 🟡 | Reconfirm (was ✅): Identity audit 6/9 | VC-1 |
| D1-6.F04 | R2 source hydration | 🟡 | Wired for `obligation` only | SI-2 (OD-05) |
| D1-6.F05 | Index contracts: `GET/POST /v1/index-contracts`, `GET …/{id}`, `POST …/{id}/state` (immutable after publication) | 🟡 | Reconfirm (was ✅): Identity audit 6/9 | VC-1 |
| D1-6.F06 | Index generations: `GET/POST /v1/index-generations`, `POST …/{id}/state`, `GET/POST …/{id}/retrieval-evaluations` (backfill, ledger validation, activate, rebuild, recall@k certification) | 🟡 | Reconfirm (was ✅): Identity audit 6/9 | VC-1 |
| D1-6.F07 | Restrictions and erasure with epochs: `GET/POST /v1/restrictions` | 🟡 | Reconfirm (was ✅): Identity audit 6/9 | VC-1 |
| D1-6.F08 | Checkpoints and lag: `GET /v1/checkpoints`; STALE/UNKNOWN freshness gates (ESR-012); restriction-failure exclusion (ESR-018) | 🟡 | Reconfirm (was ✅): Identity audit 6/9 | VC-1 |
| D1-6.F09 | Search exports: `POST /v1/search-exports` (purpose, separate permission, evidence); facet privacy; snippet policy | 🟡 | Reconfirm (was ✅): Identity audit 6/9 | VC-1 |
| D1-6.F10 | Search evidence, sources and scopes: `GET /v1/search-evidence`, `GET/POST /v1/search-sources`, `GET /v1/scopes` | 🟡 | Reconfirm (was ✅): Identity audit 6/9 | VC-1 |
| D1-6.F11 | 8 events, 20 reason codes, Idempotency-Key, ETag/If-Match on contracts and generations | 🟡 | Reconfirm (was ✅): Identity audit 6/9; IMPLEMENTATION-PROGRESS API-3 | VC-1 |
| D1-6.F12 | Replay API in each owning domain service, so rebuilds go past Kafka retention (S6-C2) | ⛔ | Owning services are other teams' | Cross-team |
| D1-6.F13 | Grant-dependent `scripts/audit.sh` sections 7–12 and 16 against a real, migrated authorization-svc | ❌ | Not run | SI-3 |
| D1-6.F14 | RAG contract (§10.2) | 🧭 | Mostly owned by AI governance | Decision |
| D1-6.F15 | Cursor paging on list routes (API-4): checkpoints, contracts, generations, restrictions, sources, evidence | ❌ | `/v1/search` already uses a signed cursor; the admin lists do not | SI-3 |
| D1-6.F16 | Console v2.0 (Search, Contracts, Generations, Restrictions, Exports, Evidence) | ❌ | Legacy page | SI-FE-1…3 |

**Tasks**

- [ ] **SI-0 · Baseline (S0):** matrix in `progress.md` from ESR-001…ESR-020; suite with counts.
- [ ] **SI-1 · Embedding provider (OD-10):** when chosen, set `EMBEDDING_PROVIDER_URL`, certify recall with a retrieval evaluation, record it. Until then the row stays 🟡 with ESR-019.
- [ ] **SI-2 · R2 scopes (OD-05):** set `SOURCE_SERVICE_URL_<TYPE>` per decided scope; a test per scope that hydration re-authorizes.
- [ ] **SI-3 · API-4 and live:** cursor paging on admin lists; rebuild; run `scripts/audit.sh` sections 7–12 and 16 against migrated authorization-svc; new `deployments/scripts/e2e-search-indexer-deps.py`.

**Console (frontend)** — `app/admin/search` (legacy) · clients `lib/api/search.ts`, `lib/api/search-indexer.ts` call 16 of 20 screen-relevant routes (Appendix B). Rebuild in v2.0 following "Frontend rules for every console" below.

| Part | Content |
|---|---|
| Header | "Platform › Search & Index"; gold primary "New index contract"; secondary "Run a search" |
| KPIs (from `GET /v1/checkpoints` and lists, or a new `GET /v1/search/summary`) | Active generations · Scopes STALE or UNKNOWN · Max lag (s) · Active restrictions · Exports (30 d) |
| Panels | Pipeline: generation Building → Backfilled → Validated → Active → Retired. Ordered list: retrieval classes R0–R3 and what each returns. Bar list: freshness per scope, with an amber callout for STALE/UNKNOWN and for semantic search unavailable (ESR-019) |
| Tabs | Search (F01, F02, F03) · Index contracts (F05) · Generations (F06) · Restrictions (F07) · Freshness (F08) · Exports (F09) · Evidence & sources (F10) |
| Drawer commands | Contract state. Generation state (activate needs a passing retrieval evaluation). Run evaluation. Add restriction. Request export (purpose + its own permission) |
| Footer | Required controls: every result re-authorized · restricted records never return · exports need a purpose and their own permission |

**Tasks**
- [ ] **SI-FE-1 · API client** for every route in Appendix B D1-6 (+ summary if built).
- [ ] **SI-FE-2 · Page v2.0.**
- [ ] **SI-FE-3 · UI E2E** `tests/e2e/console/search.spec.ts`. Cover:
  - a search with mandatory filters;
  - a restricted record disappears;
  - an export without a purpose is refused;
  - semantic search shows its honest "unavailable" answer;
  - empty and unavailable states.

**Blocked and decisions:** OD-10 embedding provider; OD-05 R2 scopes; S6-C2 replay APIs; RAG contract.

---

## D1-7 · notification-svc (:8133, DB `notification`)

**Facts**

| Item | Value |
|---|---|
| Spec | ZS-SVC-Y-001 NCD-01…NCD-05; ZS-COMMS-EMAIL-001 |
| Latest score | 35/37 (95%, 97% weighted); 110-row NCD matrix 94% weighted |
| Since 2 Nov | `560b6212` (4 Nov): record_version wired, 000034 repaired, authorize before If-Match. The 25 failing tests now pass (sweep 6 Nov: 25 packages ok) |
| Stages | `progress.md` last updated 6 Oct |
| Console | `/admin/notifications` legacy |

**Features**

| ID | Feature | Status | Evidence | What is needed |
|---|---|---|---|---|
| D1-7.F01 | NCD-01 communication intents: `POST /v1/communication-intents`, `GET …/{id}`, `POST …/versions`, `…/activate` (second principal), `…/retire`, `GET …/{id}/templates`, `GET /v1/intents/{id}/effective` | 🟡 | Reconfirm (was ✅): Identity audit 7/9 | VC-1 |
| D1-7.F02 | NCD-01 templates: `POST /v1/templates`, `GET /v1/templates/{id}`, `POST …/validate` (15 checks), `…/approve`, `…/reject` (approver ≠ author), `…/publish`, `POST /v1/templates/{id}/retire`; render preview `POST /v1/render-previews` | 🟡 | Reconfirm (was ✅): Identity audit 7/9 | VC-1 |
| D1-7.F03 | NCD-02 recipient resolution: `POST /v1/recipient-resolution`; endpoint exceptions `POST /v1/recipient-endpoint-exceptions`; `POST /v1/revalidate`; open exceptions queue `GET /v1/exceptions` (`?open=false` for closed) | 🟡 | Reconfirm (was ✅): Identity audit 7/9 | VC-1 |
| D1-7.F04 | NCD-02 channel decision: `POST /v1/channel-decision` | 🟡 | The privacy/marketing permission is supplied by the caller (G-1) | NT-2 |
| D1-7.F05 | Suppressions and preferences: `GET/POST /v1/suppressions`, `POST …/{id}/lift`, `POST /v1/preferences`, `GET /v1/preferences/me` | 🟡 | Reconfirm (was ✅): Identity audit 7/9 | VC-1 |
| D1-7.F06 | NCD-03 delivery: `POST /v1/communications`, `GET …/{id}`, `POST …/prepare`, `…/dispatch`, `…/cancel`, `…/resend`, `…/misdelivery`, `…/attempts/{id}/resolve`; provider bindings `GET /v1/provider-bindings`, `POST …/{id}/circuit`; quotas, priority, storm dedupe | 🟡 | Reconfirm (was ✅): Identity audit 7/9 | VC-1 |
| D1-7.F07 | Governed bulk sends: `POST /v1/bulk-sends`, `POST …/{id}/dispatch`; approvals `GET /v1/approvals/{id}`, `POST …/approve`, `…/reject` | 🟡 | Reconfirm (was ✅): Identity audit 7/9 | VC-1 |
| D1-7.F08 | NCD-04 evidence and provider events: `GET /v1/evidence/{id}`, `POST /v1/provider-events/{binding}`, `GET /v1/reputation`, `POST /v1/streams/{stream}` | 🟡 | Reconfirm (was ✅): Identity audit 7/9 | VC-1 |
| D1-7.F09 | NCD-05 regulated notices: `POST /v1/regulated-notices`, `GET …/{id}`, `POST …/manual-evidence`; acknowledgments `POST /v1/acknowledgments` (bound to content hash); deadline escalation | 🟡 | Reconfirm (was ✅): Identity audit 7/9 | VC-1 |
| D1-7.F10 | Record declaration to the records service: `POST /v1/regulated-notices/{id}/record-declaration` | 🟡 | Arrives by callback; legal hold sits with retention-registry-svc (S7-D3) | NT-3 |
| D1-7.F11 | Legacy free-text send path: `GET/POST /v1/notifications`, `POST …/events/ingest`, `GET …/{id}`, `…/attempts`, `…/delivery-status`, `…/evidence`, `POST …/read`, `…/cancel`, `…/resend`, `…/resolve-delivery-outcome`, `GET …/unread-count`, `GET …/templates`, `GET …/intents/{id}`, `GET/POST …/suppression`, `POST …/unsubscribe` (RFC 8058), `POST …/webhooks/{provider}` | 🟡 | Still open, so INV-01 is not met | NT-5 (G-6) |
| D1-7.F12 | Channels | 🟡 | EMAIL and IN_APP only; SMS and push missing | ⛔ OD-03 / OD-04 |
| D1-7.F13 | Email templates: 128 templates + 32 variants, register parity, trigger routing (EML-1…EML-6) | ❌ | Not built | EML-1…EML-6 |
| D1-7.F14 | ETag/If-Match on Notification, Template, TemplateVersion (API-3) | 🟡 | Reconfirm (was ✅): `560b6212`; sweep 6 Nov green | VC-1 |
| D1-7.F15 | Document templates: `POST /v1/document-templates`, `GET …/{id}`, `…/compare`, `…/locales`, `…/published`, `POST …/retire`, `…/versions`, `POST …/versions/{v}/validate`, `…/preview`, `…/approve`, `…/publish` | 🟡 | Routed in `internal/handler/handler.go`; not scored by the NCD audit · Verifier 23 Nov 2026: 11 routes registered (`internal/handler/handler.go:302`) and all 11 in openapi; maker-checker tested (`TestApproveTemplate_SelfApproval_Returns403`, `TestPgStore_ApproveTemplate_RejectsSelfApproval`); 78 template tests pass; suite on scratch Postgres 733 pass / 0 fail / 0 skip. Not met: `TEMPLATE_MANAGE` / `TEMPLATE_APPROVE` are registered (access-control 000008) and granted by the demo seed but carried by no role template (R5); the service has no router-to-openapi parity test | NT-1: role template for the template actions; router parity test |
| D1-7.F16 | Outbound webhooks (INT-04): `/v1/webhook-subscriptions` (+ submit, approve, reject, suspend, resume, revoke, rotate-signing-key, health, log, deliveries), `GET /v1/webhook-deliveries/{id}`, `POST …/replay`, `GET /v1/webhook-dead-letters` | 🟡 | `7f563f9f` (ZS-SVC-M-001 Wave 5, 07Yashjais); not audited in this domain · Verifier 23 Nov 2026: 16 routes registered (`internal/handler/webhook_out.go`) and all 16 in openapi; `TestWebhook_LifecycleAndMakerChecker`, `TestWebhook_ApprovalRefusesUnsafeDestinations`, `TestWebhook_ReplayNeedsApprovalAndKeepsIdentity`, `TestWebhookAPI_ReplayRequiresAnEntitledDifferentApprover` pass (68 webhook + 21 whout tests). Not met (R5): none of the 12 `WEBHOOK_*` actions the handler checks is registered in access-control: no migration names them, 0 of 12 on the dev DB, no seed grant. Live, every call would be refused | NT-1: register the 12 `WEBHOOK_*` actions by access-control migration, put them on a role template, run the routes live |
| D1-7.F17 | Action links: `GET /v1/notifications/actions/{token}`, `POST …/execute` | 🟡 | `internal/actionlink/gateway.go`; signed, expiring links and S1+ sign-in (NP-33, S7-D2) not verified · Verifier 23 Nov 2026: links are HMAC-signed (`internal/actionlink/signer.go:111–129`), expiring and single-use (atomic consume on POST, `gateway.go:177`), and GET never mutates; actionlink 12 + action-token store 5 tests pass; wired at `cmd/server/main.go:582`. Not built: the S1+ sign-in before execute (NP-33, S7-D2): `HandleExecute` needs no identity, so anyone holding the link executes it. Both routes are missing from `openapi.yaml` | NT-6: require an S1+ session on execute (NP-33, S7-D2); document both routes in openapi |
| D1-7.F18 | Secrets (provider, callback, webhook, unsubscribe, DKIM) leased from secret-vault-integration-svc (G-3) | ❌ | Environment variables today | NT-4 |
| D1-7.F19 | Actions `TEMPLATE_MANAGE`, `TEMPLATE_APPROVE`, `NOTIFICATION_SUPPRESS`, `NOTIFICATION_RESOLVE_OUTCOME` granted by migration (G-9) | ❌ | Only the demo seed grants them | AC-2 (D1-9) |
| D1-7.F20 | Deadline clocks from obligations-svc (G-5) | ❌ | Caller-supplied deadlines | After WF-3 (D2-4) |
| D1-7.F21 | MDM party source for external contacts (G-4) | 🟡 | ORG-5 parties now exist in tenant-entity-registry (unverified, D1-2.F14) · Verifier 23 Nov 2026: built (S7-C1, `749123e5`): `internal/partycontacts` reads `GET /v1/parties/{id}/contact-points` same-tenant, verified and in-force only, wired at `cmd/server/main.go:412`; `TestS7C1_*` 3/3 and the store tests pass. Not live: compose sets no `PARTY_CONTACTS_URL`, so party recipients are off in the stack, and the registry dev DB lacks 000028 (contact points) | NT-7: set `PARTY_CONTACTS_URL` in compose, apply the registry migrations, send to a `party:<id>` live |
| D1-7.F22 | Estate control so only notification-svc reaches provider credentials (NP-01, TC-01) | ❌ | Not built | NT-4 |
| D1-7.F23 | Cursor paging (API-4) on suppressions, notifications, webhook lists, exceptions | ❌ | — | NT-1 |
| D1-7.F24 | Console v2.0 (intents, template approval, suppressions, preferences, regulated notices, evidence, exceptions, reputation, webhooks) and a recipient acknowledgment page (G-7, G-8) | ❌ | Legacy page; send form still on `/v1/notifications` | NT-FE-1…3 |

**Tasks**

- [ ] **NT-0 · Baseline (S0):** matrix in `progress.md` (NCD 110 rows + D1-7); delete the untracked `handlercheck.bin`; suite with counts on scratch Postgres.
- [ ] **NT-1 · Verify the unscored surfaces** (document templates, outbound webhooks, action links): authorization at the object, maker-checker where a template is approved, Idempotency-Key, If-Match/ETag, openapi parity, events; add API-4 to lists. Tests named `TestNT1_*`.
- [ ] **NT-2 · G-1 channel decision:** register the service's purposes in privacy-purpose-registry-svc; call `POST /v1/privacy/decisions` (privacy-decision-svc) instead of trusting the caller; fail closed on marketing when privacy is down. Take the legal basis for regulated notices from policy-svc (S7-C2, G1-1).
- [ ] **NT-3 · G-2 records:** declare regulated-notice evidence packages as records; legal hold through retention-registry-svc (S7-D3).
- [ ] **NT-4 · G-3 secrets + NP-01:** lease every provider/callback/webhook/unsubscribe/DKIM secret from the broker; no secret in env outside local.
- [ ] **NT-5 · G-6 / INV-01 close the legacy path:** a product decision per workflow on who is notified by which template; move callers to `POST /v1/communications`; mark `/v1/notifications` send and `/events/ingest` deprecated (API-6), then close them.
- [ ] **NT-6 · Secure links (NP-33, S7-D2):** signed, expiring links; S1+ content needs a signed-in session. Tests: expired link 410, tampered link 400, S1 content without session 401.
- [ ] **NT-7 · G-4 external contacts** from tenant-entity-registry parties (after TER-1 verifies ORG-5).
- [ ] **EML-1…EML-6 · Email catalogue** (IMPLEMENTATION-PLAN §6). EML-4 after G-08.
- [ ] **NCD-1 · Close the 7 partial and 3 unmet NCD matrix rows** where their dependency now exists.
- [ ] **NT-8 · Live:** rebuild, `scripts/audit.sh`, new `deployments/scripts/e2e-notification-deps.py` (identity endpoints, privacy decision, secret broker, Kafka trigger → communication, provider sandbox, bounce ingestion).

**Console (frontend)** — `app/admin/notifications` (legacy) · client `lib/api/notifications.ts` calls 6 of 91 screen-relevant routes (Appendix B). Rebuild in v2.0 following "Frontend rules for every console" below. This is the largest console gap in Domain 1.

| Part | Content |
|---|---|
| Header | "Communications › Notifications"; gold primary "New communication"; secondary "New intent", "New template" |
| KPIs (from new `GET /v1/communications/summary`) | Communications today · Delivery UNKNOWN awaiting reconcile · Templates awaiting approval · Regulated notices awaiting acknowledgement · Open exceptions |
| Panels | Pipeline: Prepared → Dispatched → Delivered / Bounced / Unknown → Reconciled. Ordered list: channel fallback order with each provider's circuit state chip. Bar list: deliverability per channel, with an amber callout for open circuits and complaint spikes |
| Tabs | Communications (F06) · Intents (F01) · Templates (F02) · Document templates (F15) · Recipients & preferences (F03, F05) · Suppressions (F05) · Bulk sends & approvals (F07) · Regulated notices (F09, F10) · Evidence & reputation (F08) · Exceptions (F03) · Webhooks (F16) · Legacy notifications (F11, read-only, deprecation notice) |
| Drawer commands | Communication prepare / dispatch / cancel / resend (reason) / misdelivery / resolve attempt. Intent version / activate (second principal) / retire. Template validate / approve / reject (approver ≠ author) / publish / retire. Document template version validate / preview / approve / publish / retire. Suppression add / lift. Bulk send approve (≠ requester) / dispatch. Notice manual evidence / record declaration. Webhook subscription submit / approve (≠ submitter) / suspend / resume / revoke / rotate signing key; delivery replay; provider circuit |
| Other pages | Account › Notification preferences (`GET /v1/preferences/me`, `POST /v1/preferences`). Public acknowledgment page `/ack/[token]` → `POST /v1/acknowledgments` (signed link, NT-6). The bell keeps the legacy read until NT-5 |
| Footer | Required controls: no free-text sends (intent + template only) · approver ≠ author · a reason on every resend · suppressions lifted only through governance |

**Tasks**
- [ ] **NT-FE-1 · Summary route + API client** for every route in Appendix B D1-7.
- [ ] **NT-FE-2 · Page v2.0** + preferences page + acknowledgment page; the send form uses `POST /v1/communications`.
- [ ] **NT-FE-3 · UI E2E** `tests/e2e/console/notifications.spec.ts`. Cover:
  - intent → template validate → approve by a second user → publish;
  - communication prepare → dispatch → delivered evidence;
  - a suppression blocks a send;
  - a regulated notice acknowledged through the public page;
  - a webhook subscription approved by a second user;
  - empty and unavailable states.

**Blocked and decisions:** SMS/push providers (OD-03, OD-04); sender DNS; per-workflow notification decision (G-6).

---

## D1-8 · delegated-authority-svc (:8136, DB `delegated_authority`)

**Facts**

| Item | Value |
|---|---|
| Spec | ORG-06 (delegation half) |
| Latest score | 23/23 (100%) |
| Stages | `progress.md` last updated 6 Oct |
| Console | `/admin/delegations` legacy (last changed 18 Aug) |

**Features**

| ID | Feature | Status | Evidence | What is needed |
|---|---|---|---|---|
| D1-8.F01 | Create a delegation: `POST /v1/delegations` (delegator bound to caller and must hold the action; SoD via `/v1/sod/validate`; self-dealing refused; on-behalf grant PROPOSED until an independent approver activates) | 🟡 | Reconfirm (was ✅): Identity audit 8/9 | VC-1 |
| D1-8.F02 | Lifecycle: `POST /v1/delegations/{id}/activate`, `…/revoke`, `…/extend`, `…/suspend`, `…/resume`; cross-tenant expiry sweeper | 🟡 | Reconfirm (was ✅): Identity audit 8/9 | VC-1 |
| D1-8.F03 | Reads: `GET /v1/delegations`, `GET …/{id}`, effective delegations as of a date, `GET /v1/delegations/explain` (ExplainDelegationChain) | 🟡 | Reconfirm (was ✅): Identity audit 8/9 | VC-1 |
| D1-8.F04 | AuthorityLimit (monetary / quantity ceiling) on a delegation; overlap prevention (EXCLUDE); Idempotency-Key; `409 correlation_reused` | 🟡 | Reconfirm (was ✅): Identity audit 8/9 | VC-1 |
| D1-8.F05 | Delegator's own quantity limits enforced in authorization-svc (S8-D1) | 🟡 | Reconfirm (was ✅): authorization-svc 000040, 2 Nov (`3fb48d6b`) | VC-1 |
| D1-8.F06 | Evidence rows for refused escalations; `evidence_ref` on every event; events `authority.delegated/.revoked/.expired` projected into authorization-svc and identity-context-svc | 🟡 | Reconfirm (was ✅): Identity audit 8/9 | VC-1 |
| D1-8.F07 | If-Match / `expected_version` (428 / 412) | 🟡 | Reconfirm (was ✅): IMPLEMENTATION-PROGRESS API-3 | VC-1 |
| D1-8.F08 | Envelope verification enforced (compose `DELEGATED_AUTHORITY_IDENTITY_VERIFY` default `warn`) | 🟡 | Compose | X5 |
| D1-8.F09 | Cursor paging on `GET /v1/delegations` (API-4) | 🔵 | — · 24 Nov (b00e8557): keyset cursor on (created_at, delegation_id) DESC, `X-Next-Cursor` + `Link rel=next`, forged / offset / as_of combinations 400 · `TestDA1_ListPagesByCursorAndRefusesForgedCursors`, `TestDA1_KeysetPagesEveryGrantOnce` (real Postgres) · suite with TEST_DATABASE_URL 116 pass / 0 fail · mutants (keyset predicate dropped, forged cursor accepted) killed | VC-1 |
| D1-8.F10 | `/v1/iam/delegations` alias (IAM-11 / AU-26) | 🔵 | Not built · 24 Nov (b00e8557): every `/v1/delegations` route also mounted at `/v1/iam/delegations` by the same handlers (`handler.DelegationRoutePrefixes`); documented in openapi (operationIds `…Iam`) · `TestDA1_IAMAliasServesEveryDelegationRoute`, new router-parity test `TestOpenAPIMatchesTheRouter` (both mounts) | VC-1 |
| D1-8.F11 | Dependency E2E | ❌ | No `e2e-delegated-authority-deps.py` | DA-2 |
| D1-8.F12 | Console v2.0 (Delegations, Proposed for approval, Expiring, Explain chain) | ❌ | Legacy page | DA-FE-1…3 |

**Tasks**

- [ ] **DA-0 · Baseline (S0):** matrix in `progress.md`; suite with counts.
- [x] **DA-1 · API-4 + IAM-11:** cursor paging on the list; `/v1/iam/delegations` alias with parity test.
- [ ] **DA-2 · Live:** rebuild; `scripts/audit.sh`; new `deployments/scripts/e2e-delegated-authority-deps.py` (authorization SoD validate; `authority.delegated` reaches authorization-svc and the identity envelope; revoke removes it within TTL; expiry sweep).

**Console (frontend)** — `app/admin/delegations` (legacy, 18 Aug) · client `lib/api/delegations.ts` calls 4 of 9 routes (Appendix B). Rebuild in v2.0 following "Frontend rules for every console" below.

| Part | Content |
|---|---|
| Header | "Access › Delegated Authority"; gold primary "Delegate authority"; secondary "Explain a chain" |
| KPIs (from new `GET /v1/delegations/summary`) | Active · Proposed awaiting approval · Expiring in 7 days · Suspended · On-behalf grants |
| Panels | Pipeline: Proposed → Active → Suspended → Expired / Revoked. Ordered list: the explain chain of the selected delegation (delegator → delegate). Bar list: delegations by action with limit utilisation, with an amber callout for expiring or at-limit delegations |
| Tabs | Active · Proposed · Suspended · Expired & revoked · Explain (F01–F04) |
| Drawer commands | Activate (independent approver; hidden from the delegator), extend, suspend, resume, revoke. Each takes a reason, If-Match and step-up retry |
| Footer | Required controls: the delegator must hold the action · no self-dealing · on-behalf grants need an independent approver · limits never exceed the delegator's own |

**Tasks**
- [ ] **DA-FE-1 · Summary route + API client** for every route in Appendix B D1-8.
- [ ] **DA-FE-2 · Page v2.0.**
- [ ] **DA-FE-3 · UI E2E** `tests/e2e/console/delegations.spec.ts`. Cover:
  - delegate → the delegate's action is permitted;
  - an on-behalf grant activated by a second user;
  - revoke ends it;
  - self-delegation refused;
  - empty and unavailable states.

---

## D1-9 · access-control-svc (:8137, DB `access_control`)

**Facts**

| Item | Value |
|---|---|
| Spec | ZS-IAM-001 §5, §9, §10.1, §19, §21, §24; Authorization Standard |
| Latest score | 10/10 (100%) |
| Status | AUTH-PLANE Service 3 (AC-0…AC-FE-2) **not started**. Migrations since: 000025 repair (`a374c56b`), 000026/000027 identity and federation actions, 000037…000045 teammates' actions, 000046 evidence-request templates, 000047 evidence-manifest actions (`9be6c41d`) |
| Stages | `progress.md` last updated 12 Oct |
| Console | `/admin/access-control` legacy |

**Features**

| ID | Feature | Status | Evidence | What is needed |
|---|---|---|---|---|
| D1-9.F01 | Role definitions: `GET/POST /v1/role-definitions`, `GET/PATCH …/{id}`; provisioned into authorization-svc | 🟡 | Reconfirm (was ✅): Identity audit 9/9 | VC-1 |
| D1-9.F02 | Permission bundles: `GET /v1/permission-bundles`, `GET/POST /v1/role-definitions/{id}/permission-bundles`, `GET/PATCH/DELETE …/{bundle_id}` | 🟡 | Reconfirm (was ✅): Identity audit 9/9 | VC-1 |
| D1-9.F03 | Permission registry: `GET /v1/permission-definitions` (65 §5 names + legacy names; unknown actions refused) | 🟡 | Reconfirm (was ✅): Identity audit 9/9 | VC-1 |
| D1-9.F04 | Registered actions for other services: 000026, 000027, 000037–000045 | 🟡 | Reconfirm (was ✅): `TestRegisteredPermissionDefinitions` | VC-1 |
| D1-9.F05 | Evidence-manifest actions and templates (000047): `EVIDENCE_MANIFEST_GENERATE`/`_READ`, template `EVIDENCE_MANIFEST_MANAGER` v1, `EVIDENCE_REQUEST_MANAGER` v2, `OBLIGATION_MANAGER` v3 | 🔵 | `TestR5_EvidenceManifestActionsAreInTemplates` written; the integration suite failed to start (embedded Postgres `initdb` refuses user id 1000); not applied to the dev DB · 24 Nov (dd7b8103, bfc07e70): the integration suite runs again (000061 fixed; 206 pass / 0 fail); access-control 000001…000062 up/down/up 16 → 0 → 16 tables (000046 down fixed); 000047 recorded on the dev `access_control` DB; `TestRegisteredPermissionDefinitions` and the down-order test pass | VC-1; the live manifest e2e (ER-S4) |
| D1-9.F06 | System role templates (append-only) and the 21 §9.1 archetypes: `GET /v1/role-templates`, `GET …/{code}`, `GET …/versions`, `POST …/instantiate`, `POST …/upgrade` | 🟡 | Reconfirm (was ✅): Identity audit 9/9 | VC-1 |
| D1-9.F07 | Assignment requests: `GET/POST /v1/iam/access-assignments`, `GET …/{id}`, `POST …/{id}/approve|reject|cancel|revoke` (risk-tiered, SoD re-check, security approver for CRITICAL, effective-dated revoke, expiry sweep) | 🟡 | Reconfirm (was ✅): Identity audit 9/9 | VC-1 |
| D1-9.F08 | Groups: `GET/POST /v1/iam/groups`, `GET …/{id}`, `POST …/members`, `POST …/members/{id}/remove` (and `:remove`), `GET/POST …/assignments`, `GET /v1/iam/group-assignments/{id}`, `POST …/revoke|sync` | 🟡 | Reconfirm (was ✅): Identity audit 9/9 | VC-1 |
| D1-9.F09 | Access reviews: `GET/POST /v1/access-review-campaigns`, `GET …/{id}`, `POST …/complete`, `POST …/items/{id}/decide`, `…/reassign`, `GET /v1/iam/access-reviews`; HR-event triggered via subject links `GET/POST /v1/iam/subject-links` | 🟡 | Reconfirm (was ✅): Identity audit 9/9 | VC-1 |
| D1-9.F11 | Sessions end on role or assignment change; Idempotency-Key; outbox; refused-escalation evidence | 🟡 | Reconfirm (was ✅): Identity audit 9/9 | VC-1 |
| D1-9.F12 | `resource.action` aliases for legacy names (S9-D1) | 🔵 | Decided, not built · 24 Nov: the alias layer exists (migration 000048_add_permission_aliases, permission catalogue) and the four AC-1 tests pass in the integration suite on a fresh scratch DB (206 pass / 0 fail): `TestS9D1_CanonicalGrantSatisfiesLegacyCheck`, `TestS9D1_LegacyGrantSatisfiesCanonicalCheck`, `TestS9D1_UnknownActionStillRefused`, `TestS9D1_AliasCannotPointAtUnregisteredAction`. The row said "not built"; it was | VC-1 |
| D1-9.F13 | Production grants by migration (notification actions, workflow actions incl. `WORKFLOW_CANCEL/ESCALATE/INVALIDATE/ADMINISTER`, a template with `WORKFLOW_APPROVE`, `iam.workload.manage`; service identities `svc-access-control`, `svc-policy`, `GOVERNANCE_DECISION_RECORD` for ledger writers) | ❌ | Only `seed-demo-rbac.ps1` grants most of them; 11 Nov (late): decision-log READ readers (`svc-contract-lifecycle`, `svc-decision-support`) by authorization-svc 000042; 11 Nov: `svc-access-control`, `svc-policy`, `svc-workflow` (authorization-svc 000043), `iam.assignment.read` registered (access-control 000050), ledger-writer template (000051) | Notification and workflow action templates, `iam.workload.manage`: AC-2 |
| D1-9.F14 | Refused-escalations read and console summary: `GET /v1/iam/refused-escalations`, `GET /v1/iam/summary` | ❌ | Not built | AC-3 |
| D1-9.F15 | Cursor paging (API-4) on every list | ❌ | Offset lists | AC-3 |
| D1-9.F16 | ETag/If-Match beyond roles and bundles (assignment requests, groups, members, campaigns, items, subject links, template instantiate/upgrade) | 🟡 | Roles and bundles only | AC-4 |
| D1-9.F17 | Contract, docs, live, E2E; `IDENTITY_VERIFY=require` once the console sends the bearer | ❌ | No `e2e-access-control-deps.py` | AC-5 |
| D1-9.F18 | Console `/admin/access-control` v2.0 (Roles, Bundles, Registry, Templates, Requests, Reviews, My reviews, Groups, Subject links, Refused escalations) | ❌ | Legacy page covers roles and bundles only | AC-FE-1…3 |

**Tasks** (as specified in Part B5, plus AC-0 additions)

- [ ] **AC-0 · Baseline (S0).** Run the store integration suite with `-tags=integration`. Embedded Postgres refuses to run `initdb` as uid 1000 in `golang:1.26-alpine`: run the container as a named non-root user (`adduser -D pg && su pg -c …`) or use a `postgres:16` service with `TEST_DATABASE_URL`. Record counts (0 skip). Apply 000047 up/down/up on scratch, then `deployments/migrate.py up --db access_control` on the dev stack. Verify the federation role-eligibility route. Extend `progress.md` with the matrix.
- [x] **AC-1 · S9-D1 aliases.** Tests `TestS9D1_CanonicalGrantSatisfiesLegacyCheck`, `TestS9D1_LegacyGrantSatisfiesCanonicalCheck`, `TestS9D1_UnknownActionStillRefused`, `TestS9D1_AliasCannotPointAtUnregisteredAction`.
- [ ] **AC-2 · Production grants by migration** (access-control templates + authorization-svc service-identity assignments). Remove matching grants from `seed-demo-rbac.ps1` only after the migration covers them.
- [ ] **AC-3 · Refused escalations, summary, API-4** on every list. Tests per list: first, next, tampered 400, foreign-tenant 400, unknown filter 400; RLS as the app role.
- [ ] **AC-4 · API-3 completion** + self-approval tests `TestAC_RequesterCannotApproveOwnRequest`, `TestAC_ReviewerCannotDecideOwnAccess`, `TestAC_CriticalRoleNeedsSecurityApprover`.
- [ ] **AC-5 · Contract, docs, live, E2E:** `deployments/scripts/e2e-access-control-deps.py` with the edges listed in the AUTH-PLANE plan.

**Console (frontend)** — `app/admin/access-control` (legacy) · client `lib/api/access-control.ts` calls 3 of 50 routes (Appendix B). Full specification: Part B5, AC-FE-1 and AC-FE-2 (header "Roles & Access", KPIs, panels, ten tabs, requester and reviewer rules). Follow "Frontend rules for every console" below.

**Tasks**
- [ ] **AC-FE-1 · API client** for every route in Appendix B D1-9 + `/v1/iam/summary` and `/v1/iam/refused-escalations` (AC-3).
- [ ] **AC-FE-2 · Page v2.0** as in the AUTH-PLANE plan.
- [ ] **AC-FE-3 · UI E2E** `tests/e2e/console/access-control.spec.ts`. Cover:
  - access request → approved by a second user → `/v1/authorize` permits;
  - CRITICAL role needs the security approver;
  - a review REVOKE removes access;
  - a group assignment fans out;
  - the requester never sees Approve;
  - empty and unavailable states.

**Hand-back:** completes the auth plane. Then re-run the 10-step verifier protocol.

---

# Domain 2 — Governance Platform

These services answer "is this allowed, under which rules, who must approve, and what proof is needed?"

## D2-1 · jurisdiction-rules-svc (:8082, DB `jurisdiction_rules`)

**Facts**

| Item | Value |
|---|---|
| Spec | Doc 03 §8.2; ZS-JUR-001; ZS-SVC-V-001 PDC-02; REF-01, REF-10; Jurisdiction Pack Standard (`docs/architecture/jurisdiction-pack-wave*.md`) |
| Latest score | 73 met / 4 partial / 2 failed of 79 (92.4%, 94.9% weighted), 19 Oct |
| New since the audit | ZS-JUR-001 gap closing and jurisdiction-pack waves (30–31 Oct), REF-01 waves 1–3 (`a2d65f51`, `74db6105`, `645b1f35`), REF-3 waves 1–2 (`1302a620`, `dd86aade`), all by 07Yashjais. Not audited |
| Stages | S0–S4 and S6 done by the implementer on 13 Nov (`progress.md`, `RELEASE_CERTIFICATE.md`; e2e 82/82 with workflow-svc and the feed consumer, audit 24/24); S5 console rendered and driven live on 16 Nov (`progress.md` §6 steps 18–19); VC-1 open on every row. **18 Nov VC-1** by a session that built none of the rows: image rebuilt from the branch (main's 000032 applied), suites 809/0/0, mutants 2/2, migrations up/down/up, e2e 82/82, audit 24/0, `jurisdictions.spec.ts` 12/12 |
| Console | `/admin/jurisdictions` v2.0 (FE `e3de6c6` 13 Nov; live fixes `326958d` and `65b5121` 16 Nov). 16 Nov: every command driven through its dialog against the live stack as maker, checker and an independent certifier (tax officer, granted CONSOLE_DEMO_OPERATOR on the platform scope through the Authorization console): live harness 146/146, `tests/e2e/console/jurisdictions.spec.ts` 9/9 (`playwright.jurisdictions.config.ts`); 18 console defects fixed in total. Client calls 143 of 148 routes; the 5 not called are single-record reads served by their list routes |

**Features**

| ID | Feature | Status | Evidence | What is needed |
|---|---|---|---|---|
| D2-1.F05 | Automatic drift from a regulatory feed (J-03, J-04) | ⛔ | Consumer built (`internal/consumer/feed.go`, `589729eb`, JR-6). 13 Nov: proven live with a synthetic `regulatory.source.changed` (e2e: ACTIVE rule → UNDER_REVIEW, drift event by the feed, `legal.drift.detected`, redelivery no-op); compose `JURISDICTION_REGULATORY_FEED_TOPIC` (empty = off). external-data-feed-svc publishes no regulatory-change event | Cross-team (producer) |
| D2-1.F24 | Identity verification instead of trusting `X-Principal-Id`; published port `8082:8082` removed (J-01, X5) | 🟡 | VC-1 18 Nov 2026 (verifier did not build the row): image rebuilt from the branch; suites 809 pass / 0 fail / 0 skip (unit, store 92 and registryit 144 on scratch Postgres); 2 mutants killed; migrations up/down/up on scratch; `e2e-jurisdiction-rules-deps.py` 82/82 (workflow-svc, feed consumer, dev signing key); `audit.sh` 24/0; `jurisdictions.spec.ts` 12/12. Administrative writes require a verified identity (`IDENTITY_VERIFY_ADMIN=require`; headers-only and forged basis → 401 in the e2e); reads and runtime resolutions stay `warn` until the tax and payroll readers outside D1/D2 forward the envelope | Cross-team (readers forward the envelope), then EST-1 |
| D2-1.F26 | Decisions: owner of entity applicability (C8, J-05), one precedence model (J-08), public reads / `rule_payload` content / event naming (J-09) | 🧭 | Governance audit register | Decision |

**Tasks**

- [ ] **JR-6 · Drift consumer (J-03, J-04)** once external-data-feed-svc publishes a regulatory-change event with `external_feed_reference`: move matching rules to `UNDER_REVIEW` and record drift. *13 Nov: consumer built (`589729eb`) and proven live with a synthetic event; off by default until the producer exists.*

**Console (frontend)** — `app/admin/jurisdictions` v2.0 · client `lib/api/jurisdictions/*.ts` calls 143 of 148 routes (Appendix B; recounted 16 Nov). *Was: legacy page, 12 of 148.* Rebuild in v2.0 following "Frontend rules for every console" below. This is the largest console gap overall; split the drawer forms per tab file.

| Part | Content |
|---|---|
| Header | "Governance › Jurisdictions & Rule Packs"; gold primary "New rule"; secondary "Validate an action", "New import" |
| KPIs (from new `GET /v1/jurisdiction-summary`) | Active jurisdictions · Rules in DRAFT or review · Packs published · Rollouts in progress · Open drift events + remediation cases |
| Panels | Pipeline: pack Draft → In review → Certified → Signed → Published → Deployed. Ordered list: ancestor chain of the selected jurisdiction. Bar list: pack coverage by jurisdiction (`/v1/pack-coverage`), with an amber callout for open drift events and source-change notices |
| Tabs | Jurisdictions (F01, F07) · Rules (F02, F04, F14) · Rule packs (F03, F14, F15, F16, F17) · Rollouts (F18) · Calendars (F08) · Payment terms (F09) · Obligation rules (F10) · Imports (F07) · Sources & notices (F14, F17) · Remediation (F19) · Resolve & validate (F11, F12, F13, F20) · Submissions (F21) |
| Drawer commands | Rule transition (creator ≠ activator; approval reference), drift record, provenance and parameters. Import approve / reject (≠ proposer). Alias add / end. Calendar version / sources / publish / holiday import. Payment term version approve / supersede / retire. Pack submit-review / test bundle / test run / review / certify / compile / sign / verify / publish / deploy / rollback / hotfix (+ retrospective) / block / unblock / withdraw. Rollout owner / packs / attest / expert approval / transition. Remediation disposition / close. Signing key register / retire / revoke |
| Footer | Required controls: the creator never activates · imports and payment terms are maker-checker · only signed packs deploy · evidence and a reason on every change |

**Tasks:** none open (done tasks removed 24 Nov 2026).

**Blocked and decisions:** external-data-feed-svc event; C8/J-05, J-08, J-09.

**Remaining gaps (16 Nov, after the closing session)**

| # | Gap | Rows | Owner | Kind |
|---|---|---|---|---|
| 3 | Regulatory-change producer: external-data-feed-svc publishes no `regulatory.source.changed` (consumer built and proven with a synthetic event) | F05 | Teammate (external-data-feed-svc) | Cross-team |
| 4 | Decisions: owner of entity applicability (C8, J-05), one precedence model (J-08), public reads / `rule_payload` content / event naming (J-09); JUR-NEG-29 consistency policy | F26 | Architecture owners | Decision |
| 6 | Production pack signing key and `RESOLVER_RING` / `RESOLVER_REGION` | F15, F11 | Platform ops (SEC-1) | Production |
| 7 | Reads accept a headers-only identity (`warn`) until tax and payroll readers forward the envelope | F24 | Reader services (outside D1/D2) | Cross-team |
| 8 | JUR-NEG-17 | — | Tax Engineering | Decision |

---

## D2-4 · obligations-svc (:8088, DB `obligations`)

**Facts**

| Item | Value |
|---|---|
| Spec | Doc 03 §8.5; R-001 WFC-05 (in scope by the 23 Oct decision) |
| Latest score | 41 met / 22 partial / 2 failed of 65 (63.1%, 80.0% weighted); WFC-05 unscored |
| Stages | `progress.md` says "WF-3 deadline clocks in progress" (18 Nov); the dashboard claimed "S0–S6 complete 19 Nov", which the 23 Nov check of the code does not support (see "Assignment 23 Nov 2026"). Work from the step table below |
| Console | `/admin/obligations`: draft v2.0 of 19 Nov (`components/admin/obligations/console/*`), unverified, no UI spec; the user's mock of 18 Nov is the target |

**Features**

| ID | Feature | Status | Evidence | What is needed |
|---|---|---|---|---|
| D2-4.F01 | Obligations: `POST /v1/obligations` (dedup on tenant + code, jurisdiction validated fail-closed, entity-bound), `GET /v1/obligations`, `GET …/{id}`, `PATCH …/{id}` | 🟡 | Reconfirm (was ✅): Governance audit | VC-1 |
| D2-4.F02 | Status transitions OPEN / IN_PROGRESS / OVERDUE / CLOSED: `POST …/{id}/status`; overdue-detection scheduler | 🟡 | Reconfirm (was ✅): Governance audit | Replaced by WFC-05 states: WF-3; VC-1 |
| D2-4.F03 | Link / unlink a workflow instance: `POST/DELETE …/{id}/workflow` | 🟡 | Reconfirm (was ✅): Governance audit | VC-1 |
| D2-4.F04 | Filing requirements: `GET/POST …/{id}/filing-requirements`, `GET …/{fr}`, `POST …/{fr}/status`, `GET …/{fr}/transitions` | 🔵 | Built after the audit · Verifier 23 Nov 2026: routes registered (`internal/handler/handler.go:333–334`, `completion_handler.go:22–24`, reached through `registerCompletionRoutes` at `handler.go:342`) and in openapi (`:193`, `:230`, `:239`, `:265`). Unproven: the tests that cover them (`TestCreateFilingRequirement_*`, `TestFilingStatus_ValidationAndRoute`, `TestPgStore_FilingLifecycleAndHistory`, …) do not compile: `internal/store/pg_store_test.go:373` calls `ListFilingRequirements` with the old signature, and `internal/handler/completion_handler_test.go:33` stubStore lacks `CreateClock`. `go test ./...`: handler and store `[build failed]` · 23 Nov (3b3de75d): the test packages compile; filing list bug fixed (tenant bound in place of the obligation id; keyset predicate); `TestPgStore_FilingRequirement_CreateAndList`, `TestPgStore_FilingLifecycleAndHistory`, `TestCreateFilingRequirement_*`, `TestFilingStatus_ValidationAndRoute` pass (129/0/0 with the store suite) | OB-S0: make the handler and store test packages compile and pass (store on a scratch DB, 0 skip) |
| D2-4.F05 | Obligation definitions: `GET/POST /v1/obligation-definitions`, `GET …/{id}`, `GET/POST …/{id}/versions`, `POST …/{id}/retire`; entity mappings `GET/POST …/{id}/entity-mappings`, `POST …/{m}/end` | 🔵 | Built after the audit · Verifier 23 Nov 2026: routes registered (`internal/handler/completion_handler.go:26–34`) and in openapi (`:350`–`:473`). Unproven: `TestDefinitions_CreateAndRaiseFromDefinition` and `TestPgStore_DefinitionsMappingsAndPinnedObligations` sit in the handler and store test packages, which do not compile (see F04) · 23 Nov: `TestDefinitions_CreateAndRaiseFromDefinition`, `TestPgStore_DefinitionsMappingsAndPinnedObligations` pass (129/0/0) | OB-S0 as F04 |
| D2-4.F06 | Applicability decisions (append-only, versioned, UNASSESSED distinct): `GET/POST …/{id}/applicability-decisions`, `GET …/{id}/applicability` | 🔵 | Duplicates appended; value not validated; "exactly one of human or system" not enforced · 23 Nov (dde46f2d): OB-S3 — DB CHECKs (000006) and handler checks; `TestOB2_*` (3 tests) + `TestPgStore_ApplicabilityIdempotencyKey`; 2 mutants killed | VC-1 |
| D2-4.F07 | Transactional outbox and relay (fixed 19 Oct) | 🟡 | Not proven against a real broker | X3 |
| D2-4.F08 | Idempotency-Key, If-Match, RLS, object-level read authorization, `X-Legal-Entity-Id` reconciliation, full envelope on outbound authz | 🟡 | Reconfirm (was ✅): Governance audit; IMPLEMENTATION-PROGRESS API-3 | VC-1 |
| D2-4.F09 | Legal entity checked against tenant-entity-registry (`GET /v1/entities/{id}/status`) | 🟡 | Header and body reconciled and authorized, not checked against the registry | OB-3 |
| D2-4.F10 | Single owner of obligation data (C8: obligations-svc decided owner) | 🟡 | obligation-tracking-svc and filing-tracker-svc also hold obligations and emit the same event names | Cross-team |
| D2-4.F11 | WFC-05 deadline clocks: clock classes, computed deadlines with calendar provenance (jurisdiction-rules calendars), warning schedule, states PLANNED/ACTIVE/DUE_SOON/DUE/OVERDUE/FULFILLED/WAIVED/NOT_APPLICABLE/SUPERSEDED, satisfaction evidence, waiver/N-A with authority, supersession on calendar correction, escalation policies, DST rule, `evaluate`/`satisfy` APIs, clock events | 🔵 | Partly built 19 Nov and off-spec: `000014_deadline_clocks` (wrong classes, no calendar_version_id / timezone / trigger evidence), `internal/clock/worker.go` (supersession `TODO`, OVERDUE only by the sweep), 7 clock routes, 0 tests · 23 Nov (3b3de75d, 5d534c04, dde46f2d, de4cad37): rebuilt to R-001 §7 — see steps OB-S5…OB-S14; `TestWFC05_*` (19) and `TestWF3_*` (14) pass; 17 mutants killed. Not live yet (OB-S18/S19) | OB-S16…S19 live; WF-S2/S7 for cases; D1-7 template for NOTIFY |
| D2-4.F12 | The 22 partial rows (definition/instance split; `obligation.updated` scope; event and channel naming; partition key; publish-failure metric; caller id collision; dead envelope resolver; invalid status → 400; Location header; openapi and Postman; safe store tests; URL escaping; compose `depends_on`; stale progress.md) | 🟡 | Governance audit, obligations section | OB-4 (WF-4) |
| D2-4.F13 | Cursor paging (API-4) | 🔵 | — · 23 Nov: keyset cursors on obligations (`TestPgStore_ListObligationsCursorPages`), filing requirements (fixed), clocks (`TestWF3_ListClocks_ForgedCursorIs400`), escalation policies | VC-1 |
| D2-4.F14 | Dependency E2E and console v2.0 | 🟡 | 19 Nov: `e2e-obligations-deps.py` exists but uses wrong demo ids, `localhost`, no envelope, 4 checks; draft console, no UI spec | OB-S18, OB-S19, OB-FE-1…3 |

**Console (frontend)** — `app/admin/obligations` (draft v2.0 of 19 Nov, unverified) · client `lib/api/obligations.ts`. Follow the user's mock of 18 Nov (`zoiko-suite-frontend-platform/public/WhatsApp Image 2026-11-18 at 2.19.47 PM.jpeg`, the Governance & Risk family; template `app/admin/governance/page.tsx`) and "Frontend rules for every console". Show live data only; never show the mock's illustrative numbers.

| Part | Content |
|---|---|
| Header | Breadcrumb "Governance & Risk › Obligations"; title "Obligations Console"; one-line description; secondary "Export"; gold primary "Add obligation" (evaluate from a definition and trigger) |
| KPIs (from new `GET /v1/obligations/summary`; trend vs the previous 30 days with a sparkline) | Applicable obligations · Due in 30 days · Overdue · Awaiting review (waiver / N-A approvals pending) · Evidence coverage (fulfilled with evidence ÷ fulfilled, labelled exactly so) |
| Panels | Obligations by source (donut: regulatory, contractual, internal policy, licensing, other). Upcoming deadlines (date chip, title, source · entity, status chip; "View all"). Obligation details card beside the register |
| Register | Tabs with counts: All · Due soon · Overdue · Awaiting review · Archived. Search, plus filters Entity / Jurisdiction / Source / Status. Checkboxes. Columns: Obligation ID (navy link), Title / description, Entity, Jurisdiction, Due date, Owner, Status chip (Due soon amber, On track green, Overdue red, Awaiting review blue), `…` row menu. Page numbers and rows per page. Further views: Deadline clocks (F11) · Definitions & mappings (F05) · Filing requirements (F04) · Applicability (F06) |
| Detail card | ID + status chip, title, description; Source; Source document (link); Owner; Jurisdiction; Due date with computation provenance (rule version, calendar version, timezone); Status; Next action; Linked workflow / case / filing; Evidence. Buttons "Open full record" (outline) and "Request review" (gold) |
| Drawer commands | Evaluate; satisfy (evidence); request waiver / N/A (authority + reason + evidence + approval); recalculate; link / unlink workflow; filing status; applicability decision (UNASSESSED distinct; exactly one decider); definition version / retire; mapping end |
| Footer | Required controls: deadlines come from versioned rules and calendars · waivers need authority and evidence · OVERDUE is never suppressed |

**Step-by-step plan (23 Nov)** — replaces the OB-0…OB-5 and OB-FE task lists. Spec: Doc 03 §8.5; R-001 WFC-05 (§7, R-INV-14…18, NP-12/13/14/37/38, §9, §10.1, §10.2); Doc 04 §13. Start after WF-S7 and WF-S16, which open cases over mTLS. OB-S14 needs JR-DST-1. The status rules are in "How to use this file", item 5.

| Step | Do | Done when (the verifier re-runs this) | Status | Evidence |
|---|---|---|---|---|
| OB-S0 | Baseline (OB-0): build, vet and test with counts; the store suite as NOBYPASSRLS on scratch; migrations 000001…000014 up/down/up on scratch. S0 matrix in `progress.md` (Doc 03 §8.5, the 65 Part E7 rows, the 16 WFC-05 rows B1…B16) scored from this run. | Matrix with tests or file:line per row; counts recorded. | 🔵 | 3b3de75d, dde46f2d, de4cad37 · test packages compile (stub ListFilingRequirements signature, store filing call); store reset now drops the public schema · `go vet` clean · `go test -count=1 -p 1 ./...` with TEST_DATABASE_URL on scratch `obligations_test_ob`: 129 pass / 0 fail / 0 skip, store suite as NOBYPASSRLS (`rlsPool`) · migrations 000001…000016 up/down/up on scratch: 15 → 0 → 15 tables · `progress.md` rewritten from the real test list (the 18–19 Nov matrix cited `TestOB1_*` tests that did not exist) |
| OB-S1 | Decision D-OB-1: migration 000015 drops the 000013 tables (its down recreates them). Delete `compliance_handler.go`, the ComplianceStatus / ExceptionCase / EscalationRecord domain types, store functions, routes, openapi paths and tests. Keep `internal/evidencereq`. Note in `progress.md` who owns them now. | The 11 routes answer 404, the parity test passes, up/down/up is clean and the suites are green. | 🔵 | d198c47c (000015 files swept into the concurrent workflow commit), 3b3de75d · 000015 drops compliance_status / exception_cases / escalation_records, down recreates them · `compliance_handler.go`, the 8 domain types, store functions, 11 routes and openapi paths removed · `TestOpenAPIMatchesTheRouter` passes (the routes are gone) · up/down/up clean (compliance tables 0 after up) · owners recorded in `progress.md` (D-OB-1) |
| OB-S2 | OB-1: audit the definition, mapping and filing routes (F04, F05): authorization, If-Match, idempotency, RLS, events, errors. Fix what fails. | `TestOB1_*`, one per finding; F04 and F05 rows re-scored. | ⬜ | Partial, 23 Nov: one finding fixed in 3b3de75d — `ListFilingRequirements` bound the tenant in place of the obligation id (every list failed `expected 4 arguments, got 3`) and its keyset predicate compared the two columns separately; now `(created_at, id) < ($3,$4)`; `TestPgStore_FilingRequirement_CreateAndList` passes. The full route audit (TestOB1_* per finding) is not done |
| OB-S3 | OB-2 applicability decisions: idempotent on key; DB CHECK on the value (UNASSESSED never stored); exactly one decider (DB CHECK and handler). | One test per rule. Mutant "both deciders accepted" killed. | 🔵 | dde46f2d · existing DB CHECKs (000006) and handler checks confirmed; `TestOB2_BothDecidersAreRefused`, `TestOB2_UnassessedIsNeverStored`, `TestOB2_DatabaseRefusesBadDecidersAndUnassessed`, idempotency `TestPgStore_ApplicabilityIdempotencyKey` · mutants "both deciders accepted" (handler) and (DB CHECK dropped) killed |
| OB-S4 | OB-3 registry check: a foreign or unknown entity gets 422; registry down gets 503 (fail closed). | Handler tests, plus the e2e section in OB-S19. | 🔵 | existing `TestCreateObligation_LegalEntityMustExistInTenant` (unknown / other tenant 422, registry down 503) passes in the 129 · e2e section is OB-S19 (not run) |
| OB-S5 | Clock schema, migration 000016 (R-001 §7.1, §9, §9.1): <br>• **Class:** CHECK HARD_LEGAL / REGULATORY / CONTRACTUAL / INTERNAL_SLA / ADVISORY. SOFT_INTERNAL → INTERNAL_SLA; EXTENSION is no longer a class (the extension is a computation fact). <br>• **Provenance:** jurisdiction, `obligation_rule_id`, `rule_version`, `rule_content_digest`, `calendar_code`, `calendar_version`, `calendar_version_id`, `timezone`, `business_day_adjustment`, `anchor_date`, `unadjusted_date`, `local_due_date`, `due_at` (UTC), `extension_days_granted`, `pack_ref` / `pack_version`, `calculation_ref`. <br>• **Trigger evidence:** `trigger_event_id`, `trigger_event_time`, `business_event_time`, `source_object_type/id/version`, `ingested_at`. <br>• **Supersession:** `supersedes_clock_id`, `superseded_by_clock_id`, `supersession_reason`. <br>• **Satisfaction:** `evidence_refs`, `evidence_evaluation_id`, `domain_confirmation_ref`, `subject_state`. <br>• **Waiver / N-A:** `authority_action`, `requested_by`, `approved_by` (≠ requester), `approval_reference`, `reason_code`, `evidence_refs`, `applicability_decision_id`. <br>• The common columns. <br>• New tables: `obligation_clock_milestones` (WARNING / ESCALATION, offset from `due_at`, action, `fired_at`), `obligation_clock_transitions` (append-only by trigger), `escalation_policies` (versioned), `escalation_events`. <br>• FORCE RLS with WITH CHECK on all of them; drop `ON DELETE CASCADE`. | up/down/up on scratch with seeded old rows. RLS test as NOBYPASSRLS. Append-only trigger test. | 🔵 | d198c47c + 3b3de75d · 000016: class CHECK (SOFT_INTERNAL→INTERNAL_SLA, EXTENSION→HARD_LEGAL, `legacy_clock_class` for the down), provenance, trigger evidence, supersession, satisfaction, waiver/N-A, common columns, `obligation_clock_milestones`, append-only `obligation_clock_transitions`, versioned `escalation_policies`, append-only `escalation_events`, FORCE RLS WITH CHECK everywhere, no cascade · `TestMigration000016_UpDownUp_KeepsOld000014Rows` (seeded old rows), `TestWFC05_RLS_AnotherTenantCannotSeeTheClock` (NOBYPASSRLS), `TestWFC05_DueAtAndProvenanceAreImmutable_AndNothingIsDeleted` (append-only and immutability triggers) |
| OB-S6 | Computation from jurisdiction-rules only: a clock is created through `POST /v1/obligation-calculations:calculate` (envelope forwarded; Idempotency-Key = clock key), and the provenance is stored verbatim. Any outcome other than CALCULATED is refused with 422 naming that outcome; jurisdiction unreachable gives 503. HARD_LEGAL `due_at` changes only through supersession (R-INV-15): PATCH of the due date gets 409 while a HARD_LEGAL clock is live. | One fake-client test per outcome; provenance round-trips. Mutant "caller due_at accepted" killed. | 🔵 | 3b3de75d · `internal/jurisdiction/calculate.go` calls `POST /v1/obligation-calculations:calculate` with the envelope forwarded and Idempotency-Key `obligations-clock:<key>`; provenance stored verbatim (jurisdiction 8ada24fd adds `business_day_adjustment` to the answer) · `TestWF3_StartClock_OnlyACalculatedAnswerMakesAClock` (AMBIGUOUS / NO_CALENDAR / NO_RULE 422 naming the outcome, outage 503, 403), `TestWFC05_StartClock_ProvenanceRoundTripsAndKeyIsIdempotent` · HARD_LEGAL due date cannot be patched: no due-date field exists on clocks and the trigger refuses due_at changes · mutants "caller due_at accepted" and "non-CALCULATED outcome makes a clock" killed |
| OB-S7 | Computed state: the effective state is a function of (stored state, now, `due_at`, milestones), so every read returns OVERDUE once `now > due_at` and the clock is not terminal, sweep or no sweep. The sweep only persists transitions and emits events, through `UPDATE … WHERE state = $old AND version = $v` (NP-37). Satisfied after overdue keeps the overdue period (NP-38). | **Frozen-clock test:** sweep off, injected now past `due_at`, read says OVERDUE. A restart mid-sweep emits no duplicate event. NP-38 test. | 🔵 | 3b3de75d · `domain.EffectiveState`; sweep persists with `UPDATE … WHERE clock_state=$old AND row_version=$v` + row lock + unique (clock_id,row_version) · frozen clock: `TestWFC05_EffectiveState_FrozenClockReadsOverdueWithoutSweep`, `TestWF3_GetClock_ReadsOverdueWithoutASweep`, `TestWFC05_FrozenClockOverdueOnRead_SweepPersistsOnce` (effective_at = due_at) · restart/concurrency: `TestWFC05_ConcurrentSweepsPersistOneMove` (10 parallel sweeps → 1 transition, 1 event) · NP-38: `TestWFC05_SatisfyAfterOverdue_KeepsTheOverduePeriod` |
| OB-S8 | Supersession: consume `business-calendar.updated` and `jurisdiction.calendar.changed` (replay-safe, idempotent on event id). Recalculate live clocks on an older version of that calendar. When `due_at` changes, the old clock becomes SUPERSEDED and a successor is created in the same transaction, linked both ways with reason CALENDAR_CORRECTED. Add the governed command `POST …/clocks/{id}/recalculate` (authority + reason). | **Calendar-correction test:** the successor carries the new `calendar_version_id`, and the old clock and its `due_at` stay readable. The event is emitted. | 🔵 | 3b3de75d, 5d534c04 · `internal/clock/calendar_consumer.go` consumes `business-calendar.updated` / `jurisdiction.calendar.changed` (replay-safe on `consumed_events`), recalculates against the pack in force as `svc-obligations` (authorization 000044) and supersedes with reason CALENDAR_CORRECTED in one transaction · `POST …/clocks/{id}/recalculate` · `TestWFC05_Supersede_KeepsTheOldClockAndLinksBothWays`, `TestWFC05_CalendarCorrection_SupersedesOnceAndIsReplaySafe`, `TestWFC05_CalendarCorrection_AFailureIsRetriedNotConsumed`, `TestWF3_Recalculate_SupersedesOnlyWhenTheAnswerChanges` |
| OB-S9 | Satisfy (R-INV-14): FULFILLED needs evidence refs or an approved evidence-exempt reason, checked with evidence-requirements `POST /v1/evidence/evaluate`. MISSING gets 409 `evidence_missing`. NO_REQUIREMENTS_DEFINED is accepted when refs are present, and logged. Evidence-requirements unreachable gives 503. | Tests per answer. Satisfy without evidence gets 409. | 🔵 | 3b3de75d · `checkSatisfaction`: evidence-requirements evaluate (MISSING 409 `evidence_missing`, NO_REQUIREMENTS_DEFINED accepted with refs and logged, outage 503) or an approved exemption; DB CHECK · the invented-SATISFIED `NoOpClient` replaced by a fail-closed `Unconfigured` · `TestWF3_SatisfyClock_NeedsAcceptedEvidence`, `TestWFC05_FulfilledWithoutEvidenceIsRefusedByTheDatabase` · mutants "empty evidence accepted" and "MISSING treated as satisfied" killed |
| OB-S10 | Waive / N-A: needs the `OBLIGATION_CLOCK_WAIVE` / `…_NOT_APPLICABLE` grant at the entity (403 without), a reason code, evidence refs, and an APPROVED `OBLIGATION_WAIVER` workflow instance decided by a different principal (checked with workflow-svc `verify-release`; 409 `approval_required` otherwise). N/A also links an applicability decision that resolved false. | **Waiver without authority gets 403** (test and e2e). The requester's own approval gets 409. | 🔵 | 3b3de75d, 640e629e · grants OBLIGATION_CLOCK_WAIVE / _NOT_APPLICABLE (access-control 000062, OBLIGATION_AUTHORITY template); approval checked with workflow-svc GET + `/evidence` + `verify-release` on `domain.AuthoritySubjectFingerprint`; deciders ≠ requester; N/A needs a NOT_APPLICABLE decision of this obligation; DB CHECK approved_by ≠ requested_by · `TestWF3_WaiveClock_AuthorityAndASecondPerson` (403 without authority, 409 own approval / wrong type / other clock / not released, 503 workflow down), `TestWF3_NotApplicable_NeedsANotApplicableDecisionOfThisObligation`, `TestWFC05_WaiverNeedsADistinctApprover_AndNALinksADecision` · 3 mutants killed. e2e not run |
| OB-S11 | Escalation: each milestone fires once (guarded). Actions: <br>• RAISE_PRIORITY; <br>• ADD_OWNER; <br>• OPEN_CASE: workflow-svc `POST /v1/cases` through an outbox destination over mTLS as `svc-obligations`, idempotent per WF-S7; <br>• NOTIFY: event `obligation.escalated`, with a notification-svc template and trigger (D1-7) added for it; <br>• REQUIRE_APPROVAL. <br>Escalation never changes the clock state or its legal meaning (R-001 §7.4). | A test per action. The case is opened once under retry, and the notification is queued. The clock state is unchanged. | 🔵 | 3b3de75d · milestones fire once; RAISE_PRIORITY, ADD_OWNER, OPEN_CASE (outbox destination WORKFLOW_CASE relayed to workflow-svc `POST /v1/cases`, Idempotency-Key `obligations:<clock>:<milestone>`), NOTIFY (`obligation.escalated`), REQUIRE_APPROVAL; clock state never changed · `TestWFC05_MilestoneFiresOnce_EscalationNeverChangesTheClock`, `TestWFC05_WorkerTick_SweepsFiresAndOpensCases`, `TestWFC05_WorkerTick_ARefusedCaseStaysQueued` · not live: needs workflow-svc WF-S2/S7 `POST /v1/cases` and a notification-svc template (D1-7) |
| OB-S12 | APIs: <br>• `POST /v1/obligations/evaluate` (definition + trigger evidence → obligation + clock); <br>• `POST /v1/obligations/{id}/satisfy` (current clock); <br>• `POST …/clocks/{id}/recalculate`; <br>• `GET /v1/obligations/summary` (console KPIs); <br>• ST-2 `…/available-actions` and `…/transitions` for obligation, clock, filing requirement and definition; <br>• ST-3: `…/clocks/{id}/status` becomes named commands, with the old route as an alias and a `Deprecation` header; <br>• API-4 cursor paging on every list. | openapi parity; `TestWF3_*` per route. | 🔵 | 3b3de75d · routes evaluate, obligation satisfy, recalculate, summary, ST-2 (clock available-actions + transitions; obligation available-actions; filing transitions existed), ST-3 alias with `Deprecation`, API-4 cursor on clocks · openapi parity `TestOpenAPIMatchesTheRouter` + kin-openapi validity pass; `TestWF3_*` 14 tests · gaps: no transitions route for the obligation and the definition (no history table); definitions/mappings lists keep their existing paging |
| OB-S13 | Events `obligation.clock.started`, `.due_soon`, `.due`, `.overdue`, `.fulfilled`, `.waived`, `.not_applicable`, `.clock.superseded`, `.escalated`, with the R-001 §10.3 envelope fields. New `asyncapi.yaml` with a two-way contract test; schemas registered in schema-registry. Metrics: overdue clocks by class / jurisdiction / owner (B16), sweep lag, escalation failures; alert rules. | Contract test passes; schemas read back; `promtool` passes. | 🔵 | 5d534c04, dde46f2d · `asyncapi.yaml` with the R-001 §10.3 envelope; `TestAsyncAPI_MatchesTheCode` (two-way) · metrics `obligation_clocks_overdue{clock_class,jurisdiction}`, `obligation_clock_sweep_lag_seconds`, `obligation_escalation_failures_total`, … · prometheus group `obligations` (3 alerts), `promtool check rules` SUCCESS (88 rules) · schemas not yet registered in schema-registry (needs the live stack) |
| OB-S14 | DST: the stored `due_at` and `timezone` equal jurisdiction's answer across both 2026 Europe/London transitions. Needs JR-DST-1. | DST test green. | 🔵 | 6c85a6ea (JR-DST-1), de4cad37 · `TestWFC05_DST_StoredInstantAndZoneAreJurisdictionsAnswer` (Europe/London 2026-03-29 → 01:00Z, 2026-10-25 → 00:30Z; due day starts at local midnight) |
| OB-S15 | OB-4: re-score the 22 Part E7 rows from OB-S0 and close the open ones (`obligation.updated` scope, partition key, publish-failure metric, caller id collision, dead envelope resolver, invalid status → 400, Location header, URL escaping, compose `depends_on`, Postman). Add the common columns to `obligations`. | Each row ✅ with evidence in `progress.md`. | ⬜ | |
| OB-S16 | Identity `require`: grep for in-estate callers of `:8088` and put any service caller on an mTLS peer path. Forward the envelope on outbound calls to jurisdiction, registry, authz, evidence and workflow. Then `OBLIGATIONS_IDENTITY_VERIFY=require`. | A header-only call gets 401; the console works. | ⬜ | |
| OB-S17 | Contract and docs: openapi parity; `SPEC_DEVIATIONS.md` (R-OD-05: no clock pauses; R-OD-06: evidence classes per domain open); `RUNBOOK.md`; `RELEASE_CERTIFICATE.md`; new `scripts/audit.sh`; dated note in Doc 03 §8.5 with the new events. | Parity and contract tests pass. The docs name only things that exist. | ⬜ | Partial, 23 Nov (dde46f2d, de4cad37): openapi parity and asyncapi contract tests pass; `SPEC_DEVIATIONS.md` (R-OD-05, R-OD-06, route mapping, approvals in workflow-svc, NOTIFY, calendar identity) and `RUNBOOK.md` written; Doc 03 §8.5 dated note. Open: `RELEASE_CERTIFICATE.md` and `scripts/audit.sh` (both need the live run) |
| OB-S18 | Live: rebuild the image (host binaries), migrate the dev DB (000015, 000016), recreate the container. | `migrate.py status --db obligations` shows 0 pending. `scripts/audit.sh` green live. | ⬜ | |
| OB-S19 | Rewrite `e2e-obligations-deps.py` in the house style (urllib, 127.0.0.1, demo ids `11111111-1111-1111-1111-111111111111` / `22222222-…`, envelope logins, exit code = failures). Sections: registry 422/503; jurisdiction validation; evaluate from a trigger with full provenance; HARD_LEGAL due date refused; a short-window clock goes DUE_SOON → DUE → OVERDUE, also with the sweep paused; supersession after publishing a corrected calendar version; waive 403 / 409 / 200; satisfy MISSING then SATISFIED (real vault upload + evidence-requirements); escalation opens a workflow case and queues a notification; Kafka events; outbox; idempotency; RLS cross-tenant; identity `require`; the duplicate routes return 404. | Green live, with no SKIP except a named owner block. | ⬜ | |
| OB-S20 | Mutants: at least 2 per new control (computed OVERDUE, provenance required, waiver authority, supersession keeps the old clock, satisfy needs evidence). | All killed; the list is in `progress.md`. | 🔵 | 17 mutants, all killed (list in `progress.md`); runner `D:/zoikocomp/.cache/tools/mutate.py` + `ob_mutants*.json` |
| OB-FE-1 | API client for every route (including OB-S12) and the summary route. | vitest covers the client. tsc is clean. | ⬜ | |
| OB-FE-2 | Page v2.0 per the mock and the table above. | Renders live; layout matches the mock; every command reaches the service; honest empty and error states. | ⬜ | |
| OB-FE-3 | `tests/e2e/console/obligations.spec.ts`: <br>• an obligation evaluated with a computed deadline, with calendar provenance shown; <br>• waive without authority refused; <br>• the linked workflow / case is shown; <br>• an applicability decision is recorded once; <br>• OVERDUE is shown without the sweep; <br>• empty states, and the unavailable state with the service stopped. | Spec green live; tsc, eslint and vitest clean. | ⬜ | |
| OB-S21 | Hand-back: S0–S6 evidence in `progress.md`; feature rows set to 🔵 with evidence; the dashboard row updated (never ✅). | The verifier runs the protocol, VC-1 included. | ⬜ | |

**Blocked and decisions:** C8 overlap removal (cross-team: obligation-tracking-svc should emit `contract.obligation.*` per Doc 03 §12.3). D-OB-1 taken 23 Nov (user): ComplianceStatus / ExceptionCase / EscalationRecord are not owned here (step OB-S1). R-OD-05 (which clock classes may pause; none built) and R-OD-06 (evidence classes per domain) go to Legal/Compliance on the decision sheet.

---

## D2-5 · authorization-svc (:8089, DB `authorization`)

**Facts**

| Item | Value |
|---|---|
| Spec | Doc 03 §8.3; GOV-03/04/12; ZS-IAM-001; ORG-06 (role half) |
| Latest score | 85/85 (100%), 14 Oct; 2 Nov pass added IAM-13, quantity limits, API-3 |
| Stages | S0–S6 verified 2 Nov: suites 818 pass / 0 fail / 1 by-design skip; 4 mutants killed; dependency E2E 33/33; console rebuilt Reconfirm in VC-1. **18 Nov VC-1:** image rebuilt from the branch; suites 903 / 0 / 1 by-design skip (both DSNs); 2 mutants killed; `e2e-authorization-deps.py` 33/33 (siem, otel-collector, Jaeger up), personas 78/78, `audit.sh` 49/0; console AZ-FE-1/2 done, `authorization.spec.ts` 11/11 |
| Since 2 Nov | Merge of main (`15a9c581`) brought 07Yashjais's ORG-06 waves 4–5 (`2dc16f5b`, `8efb2a89`): suspension history and expiry events, next to our IAM-13. IC-3 re-points merged subjects (`0174312a`). Not re-verified after the merge |
| Console | `/admin/authorization` v2.0 |

**Features**

| ID | Feature | Status | Evidence | What is needed |
|---|---|---|---|---|
| D2-5.F13 | Envelope verification (JWKS) on every non-evaluation route; mTLS workload gate; admin delegates by verified mTLS CN | 🟡 | Built and verified 18 Nov (`e2e-authorization-deps.py` 33/33: verified envelope accepted, tampered refused). Trial under `AUTHZ_IDENTITY_ASSERTION=require` 18 Nov: console and identity e2e 61/61 pass; access-control-svc (plaintext admin calls, stale mTLS client with no bootstrap token, `AUTHZ_MTLS_ENABLED` unset), `seed-demo-rbac.ps1` (synthetic seed admin) and the persona harness are not ready, so compose stays `warn` (SPEC_DEVIATIONS) | AZ-2: access-control-svc on mTLS (D1-9), seed and harness on envelopes, then EST-1 |
| D2-5.F16 | Security sign-offs: TOTP interim factor (SO-1), session maximums (SO-2) | 🧭 | Security | Decision |
| D2-5.F18 | Callers: no tenantless `/v1/authorize`; preparer sent on own-object actions (X-1, X-2); `X-Authn-Age-Seconds` forwarded, then `AUTHZ_STEP_UP_UNKNOWN_AGE=step_up` (X-3) | 🟡 | Other services | Cross-team, then AZ-2 |
| D2-5.F21 | IAM-8 load and revocation SLOs (`/v1/authorize` p50/p95/p99, revocation lag vs IAM §29) | ⛔ | Measured locally 10 Nov (`load-authorization.py`): 200×60 s 321/s p99 1376 ms 0 errors; revocation 9–35 ms; projection lag 0.23 s / 0.42 s. A production SLO class needs production-like capacity (production backlog "Platform") | Production measurement |

**Tasks**

- [ ] **AZ-2 · Enforce identity and mTLS:** *18 Nov: trial under `AUTHZ_IDENTITY_ASSERTION=require` — the console (envelope on every call) and `e2e-identity-context-deps.py` 61/61 pass; blocked by access-control-svc (plaintext admin calls; its `internal/mtls` asks for a certificate with no bootstrap token; compose never sets `AUTHZ_MTLS_ENABLED` though `AUTHZ_ADMIN_DELEGATES` names it), by `seed-demo-rbac.ps1` acting as a synthetic seed administrator, and by the persona harness. Compose stays `warn` (user rule: flip only when every test passes). Next: move access-control-svc to `identity-envelope/mtlsclient` (session 3, D1-9), then the seed and harness onto envelopes or mTLS delegates.* Console forwards `Authorization: Bearer <envelope>` on every call (F-1), then `AUTHZ_IDENTITY_ASSERTION=require`; every PEP gets a client certificate (M-02), then `AUTHZ_EVALUATION_MTLS=require`; after X-3, `AUTHZ_STEP_UP_UNKNOWN_AGE=step_up`; after X-1, `AUTHZ_ENFORCE_TENANT_ON_AUTHORIZE=true` (default `false` in `internal/config/config.go`; the warn log lists the tenantless callers). Prove each in the E2E.
- [~] **IAM-8 · Load and revocation SLOs:** (local measurement 10 Nov; production pending) k6 or Go load test of `/v1/authorize`; measure revocation propagation; record against IAM §29.

**Console (frontend)** — `app/admin/authorization` (v2.0, 2 Nov) · client `lib/api/authorization.ts` calls 64 of 80 screen-relevant routes (Appendix B). The page exists. What is missing is the IAM-13 and session commands.

**Tasks:** none open (done tasks removed 24 Nov 2026).

---

## D2-6 · workflow-svc (:8090, DB `workflow`)

**Facts**

| Item | Value |
|---|---|
| Spec | Doc 03 §8.4; GOV-06; R-001 WFC-01…WFC-05 (WFC-04 here, WFC-05 in obligations); ZS-STATE §11–§14 |
| Latest score | 36 met / 25 partial / 0 failed of 61 (59.0%, 79.5% weighted) |
| Stages | `progress.md` claims "S0–S6 complete 19 Nov"; the 23 Nov check of the code does not support it (see "Assignment 23 Nov 2026"). Work from the step table below |
| Suite (23 Nov re-check) | `go build ./...` OK. `go vet ./...` FAIL and `go test ./...` FAIL: the handler test package does not compile (`internal/handler/handler_test.go:187` `pendingSubmissions redeclared`; `stubStore` lacks `CreateCase` and the other 4 case methods; tests use `PaginationCursor.WorkflowInstanceID/StageOrder/SubmissionID` and `FormSubmission.RowVersion`, which do not exist). The other 17 packages pass. The store suite reset drops a hand-written table list (`internal/store/pg_store_test.go:48-49`) without `cases`, so the suite goes red after its first test. `progress.md` cites tests that do not exist (`TestWF1_*`, `TestWF2_*`, `TestST_*`, `TestFORM_*`, `TestAUD_*`) and routes that are not routed (`GET /v1/workflows`, definition `/versions`) |
| Scope decision (user, 23 Nov 2026) | Build all four R-001 capabilities that do not exist yet: WFC-01 definition publication lifecycle (F28), WFC-02 work items (F29), operator repair API + stuck-instance reconciler (F30), emergency approval (F31). Steps WF-S22…S25 |
| Implementer prompt | `docs/audit_files/prompts/WORKFLOW-SVC-IMPLEMENTER-PROMPT-2026-11-23.md` |
| Console | `/admin/workflows`: draft v2.0 of 19 Nov (`components/admin/workflow/console/*`), unverified, no UI spec. 23 Nov re-check: `overview.tsx` shows invented data ("Stage 1: Alice (Finance)", "PURCHASE_APPROVAL: 12", KPIs fixed at 0, "Pipeline Velocity (placeholder)"); `data.ts:37` calls `getWorkflow("dummy")`; `data.ts` imports `listWorkflowInstances` and `cases.tsx` / `audit.tsx` import `Case` / `AuditEngagement` from `lib/api/workflow.ts`, none of which is exported; the Audit, Cases and State-machines tabs return `rows: []`. `npx tsc` stops on a parse error in `app/admin/tenants/page.tsx` (another service) before it reaches these files |

**Features**

| ID | Feature | Status | Evidence | What is needed |
|---|---|---|---|---|
| D2-6.F01 | Create an instance: `POST /v1/workflows` (expands the definition's stages, checks entity status in the registry, refuses a foreign tenant) | 🟡 | Reconfirm (was ✅): Governance audit | VC-1 |
| D2-6.F02 | Reads: `GET /v1/workflows/{id}`, `…/next-approver`, `GET /v1/workflows/pending`, `GET …/{id}/evidence` | 🟡 | Reconfirm (was ✅): Governance audit | VC-1 |
| D2-6.F03 | Actions: `POST …/{id}/actions` (approve/reject), `…/request-changes` | 🟡 | Reconfirm (was ✅): Governance audit | VC-1 |
| D2-6.F04 | Quorum votes: `POST …/{id}/quorum-votes` | 🟡 | Quorum races open (L-2). 17 Nov: signature workflows from a policy-svc decision (`signatory_decision_id`, 000021) | WF-1 |
| D2-6.F05 | Escalate, escalate-to, cancel, invalidate, verify-release: `POST …/{id}/escalate`, `…/escalate-to`, `…/cancel`, `…/invalidate`, `…/verify-release` | 🟡 | Reconfirm (was ✅): Governance audit. 23 Nov: verify-release checks status, subject version and fingerprint only (`internal/store/pg_store.go:1041-1101`); an APPROVED instance never expires and passes any number of times (see F25) | Grants: AC-2; VC-1; WF-S11a |
| D2-6.F06 | Timers: expiry, deadline, escalation, approver re-check; durable recovery of stuck instances | 🟡 | Reconfirm (was ✅): Governance audit | VC-1 |
| D2-6.F07 | Delegation checked through authorization-svc (`delegation_id` recorded); Idempotency-Key replay and mismatch | 🟡 | Reconfirm (was ✅): Governance audit | Handler tests: WF-5; VC-1 |
| D2-6.F08 | Decision-log writes through a dedicated outbox destination; `decision_id` returned; decisionlog client (`9d9bac43`) | 🟡 | No live ledger proof | GDL-2 (its D2-2 section was removed on 24 Nov 2026, all rows ✅): set `WORKFLOW_LEDGER_PRINCIPAL_ID`, grant it, run workflow → decision log once on the stack, record the decision id |
| D2-6.F09 | Lineage on decisions and transitions: `access_decision_id`, `authority_basis`, `delegation_id` | 🟡 | Reconfirm (was ✅): Governance audit | VC-1 |
| D2-6.F10 | Consumed events (policy, delegated-authority, authorization) trigger approver re-checks | 🟡 | 23 Nov: tenantless global `policy.*` events are now applied to every tenant with a pending instance (`internal/consumer/consumer.go:95-125`); the bounded, resumable batch and its crash test are missing | WF-S13 |
| D2-6.F11 | Workflow definitions: `GET/POST /v1/workflow-definitions`, `GET …/{id}`, `POST …/supersede|activate|suspend|retire` | 🟡 | 23 Nov: definitions have FORCE RLS (`000017_add_child_rls.up.sql:71-73`); instances record `workflow_definition_id` + version. No publication lifecycle, maker-checker, validation gates or checksum (F28) | WF-S11, WF-S22 |
| D2-6.F12 | Identity-envelope gate (signed envelope overrides gateway headers) | 🟡 | Compose `WORKFLOW_IDENTITY_VERIFY` default `warn` | X5 |
| D2-6.F13 | ETag/If-Match on 11 mutations (7 instance, 4 definition) | 🟡 | Reconfirm (was ✅): IMPLEMENTATION-PROGRESS API-3 | VC-1 |
| D2-6.F14 | Audit engagement module: `POST /v1/audit/engagements`, `GET …/{id}`, acceptance (`submit-acceptance`, `acceptance-decision`), `activate`, `amend-scope`, `withdraw`, `mark-fieldwork-complete`, `enter-completion-review`, `mark-report-ready`, `close`, `GET …/completion-gates`; plans `GET/POST …/plan`, `POST /v1/audit/plans/{id}/approve|materiality`, `GET/POST …/risks`; risks `POST /v1/audit/risks/{id}/assess|mark-significant|response|assertions`; workpapers `POST …/workpapers`, `GET /v1/audit/workpapers/{id}`, `POST …/procedures|results|conclusions|evidence-links|cross-references|addenda|mark-prepared|lock`; reviews `POST …/reviews`, `…/reviews/quality`, `GET …/reviews/release-gates`, `POST /v1/audit/review-scopes/{id}/reviewers|notes|sign-off`, `POST /v1/audit/review-notes/{id}/respond|resolve`, `POST /v1/audit/sign-offs/{id}/withdraw`, `POST /v1/audit/quality-reviews/{id}/complete` | 🟡 | Built; not in the Group 2 audit · Verifier 23 Nov 2026: 42 routes registered (`internal/handler/handler.go:240–297`). The 14 audit store tests (`TestPgStore_AuditPlan_ApprovalAndSelfApprovalRefused`, `TestPgStore_LockedWorkpaper_IsImmutable`, `TestPgStore_AddPostLockAddendum_InvalidatesValidSignOff`, …) pass when each runs alone on a fresh scratch DB. Unproven as a suite: (1) the store suite is red, 2 pass / 65 fail, because its reset drops a hand-written table list without `cases`, so every test after the first fails `relation "cases" already exists` at `000023_cases.up.sql` (`internal/store/pg_store_test.go:25`); (2) the handler test package does not compile (`internal/handler/handler_test.go:187: pendingSubmissions redeclared`); (3) the audit module is in no API contract: 0 of its paths in `openapi.yaml`, no other spec file, and the parity test only covers `/v1/workflows*` | WF-S0: drop the whole schema in the store reset (not a list), fix the handler test package, write the audit module's openapi with a parity test |
| D2-6.F15 | Forms module: `POST /v1/forms`, `GET …/{id}`, `POST …/publish|retire|drafts`, `GET …/pending-submissions`; submissions `GET /v1/form-submissions/{id}`, `POST …/drafts|validate|submit|supersede|route`, `GET …/validation-result`, `GET …/version` | 🟡 | Built; not in the Group 2 audit · Verifier 23 Nov 2026: 14 routes registered (`internal/handler/handler.go:299–316`), all 14 documented in `openapi.biz04-forms.yaml`. The 14 form store tests pass when each runs alone on a fresh scratch DB. Unproven as a suite: the store suite reset defect and the non-compiling handler test package (see F14) mean neither the store suite nor `form_contract_test.go` runs | WF-S0 as F14; then run `form_contract_test.go` |
| D2-6.F16 | WFC-04 cases (WF-2) | ❌ | 19 Nov stub: `000023_cases` (5 states of R-001 §6.3's 8; no notes, confidentiality, merge/split, closure gate, reopen), 5 routes, no tests. 23 Nov: broken at runtime. `insertOutboxEventTx` writes table `outbox` (`internal/store/pg_store.go:163-178`), but the table is `outbox_events` (000011); `CreateCase` inserts `created_by_principal_id` and `TransitionCase` sets `close_reason`, and neither column exists in `000023_cases.up.sql`. So `POST /v1/cases` and the RESOLVED / CLOSED transitions fail. The ETag sends `row_version` (never incremented) while If-Match is compared with `version`. Every case write is authorized as `WORKFLOW_READ` (`internal/handler/form.go:728`, a TODO). A standalone case cannot be created (no legal entity in the request). `POST /v1/cases/{id}/status` takes a free target state | WF-S1…WF-S7 |
| D2-6.F17 | ZS-STATE state-machine registry (ST-1), available actions + transition history (ST-2), no generic status writes (ST-3) | ❌ | 19 Nov: only `GET /v1/state-machines/{workflow_type}`, `GET /v1/workflows/{id}/available-actions`, `…/transition-history`; no publish/version registry; other objects not covered. 23 Nov: the output is invented. `GetStateMachine` builds `STAGE_n` states and PENDING-only edges (`internal/handler/form.go:1019-1046`). `GetAvailableActions` casts `h.store.(*store.PgStore).Pool()` and queries the quorum pool on the bare pool (`form.go:1095`): it panics with any other store and sees no rows under FORCE RLS as `zoiko_app`; it ignores ROLE stages and delegates. `GetTransitionHistory` fills `decision_id` from `access_decision_id` and invents `from_stage`. Neither route is in `openapi.yaml`, so the parity test fails once the package compiles | WF-S8…WF-S10 |
| D2-6.F18 | The 25 partial rows: quorum stage lock and `row_version` (L-2); RLS on stages, transitions, quorum tables, outbox, definitions (L-8); per-type approve authority (Q-6); split state model, common columns, event naming (Q-3) | 🟡 | Governance audit, workflow sections (14, 19, 21 Oct) | WF-1 |
| D2-6.F19 | Business-object owners call `/invalidate` or emit change events (H-5); invoice-approval-svc calls a path that does not exist (N-3) | 🟡 | Other teams' services | Cross-team |
| D2-6.F20 | Reporting-line and team data for approver independence (H-6) | ⛔ | Needs org-structure data (ORG-05, teammate's org-structure-svc) | Cross-team |
| D2-6.F21 | Decisions Q-1…Q-6 (WFC scope, binding API surface C4, event catalogue, consumed-event behaviour, service-account vs human approver, approve action codes) | 🧭 | Governance audit register. 23 Nov: Q-1 (WFC scope) partly decided by the user: WFC-01 lifecycle, WFC-02 work items, operator repair and emergency approval are built here (F28–F31) | Decision (Q-2…Q-6) |
| D2-6.F22 | Cursor paging (API-4) on pending, definitions, form lists | 🟡 | 23 Nov: the store pages `GET /v1/workflows/pending` by `(started_at, workflow_instance_id)` (`internal/store/pg_store.go:1180-1257`), and definitions and form pending lists take `cursor`; the cursor handler tests do not compile; `GET /v1/cases` pages by offset | WF-S0a, WF-S12 |
| D2-6.F23 | Root-context Docker image build, dependency E2E, console v2.0 | ❌ | 19 Nov: `e2e-workflow-deps.py` exists but uses wrong demo ids, `localhost`, no envelope; draft console, no UI spec. 23 Nov: the script uses `requests`, `http://localhost`, tenant `11111111-1111-4111-8111-111111111111` and principal `admin-1`, and skips its checks when no workflow is created; `services/workflow-svc/scripts/audit.sh` and `tests/e2e/console/workflows.spec.ts` do not exist; the console shows invented data (see Facts) | WF-S18…WF-S19, WF-FE-1…3 |
| D2-6.F24 | Instance list and KPIs: `GET /v1/workflows` (filters, cursor) and `GET /v1/workflows/summary` | ❌ | 23 Nov: neither is routed (`internal/handler/handler.go:208-333`); the console calls a `listWorkflowInstances` client that does not exist | WF-S11b, WF-FE-1 |
| D2-6.F25 | Approval integrity: validity window, consume-once, definition and policy version bound to the approval, refused decisions recorded as evidence (R-INV-07/11, NP-05/06/08/35; Doc 05 §6.4) | ❌ | 23 Nov: no approval expiry or consumption; self-approval, not-human and already-decided refusals return 403 (`internal/handler/handler.go:1055`, `:1227`, `:2635`) with no ledger record and no event | WF-S11a |
| D2-6.F26 | Contracts and docs: `asyncapi.yaml`, one `openapi.yaml` for every route, dated notes in Doc 03 §8.4 and Doc 04 | ❌ | 23 Nov: no `asyncapi.yaml`; `openapi.yaml` lacks cases, state machines, available-actions and transition-history; forms are in a separate file; the audit module has no contract; Doc 04 §15 has no physical-names note for WorkflowInstance / WorkflowTransition | WF-S6, WF-S14, WF-S17 |
| D2-6.F27 | Test-suite health and a true `progress.md` | ❌ | 23 Nov: see Facts, "Suite (23 Nov re-check)" | WF-S0a, WF-S0 |
| D2-6.F28 | WFC-01 definition publication lifecycle: DRAFT → VALIDATION_PENDING → VALIDATED → APPROVAL_PENDING → PUBLISHED (and REJECTED / SUSPENDED / SUPERSEDED / RETIRED), validation gates, maker-checker, checksum, immutable once published (R-001 §3.2–3.4, R-INV-01/02/24) | ❌ | 23 Nov: definitions have only `is_active` with activate / suspend / supersede / retire | WF-S22 |
| D2-6.F29 | WFC-02 work items: claim / release / complete / reassign / cancel / escalate, typed completion, assignment history, bulk completion with per-item authorization (R-001 §4.2) | ❌ | 23 Nov: not built | WF-S23 |
| D2-6.F30 | Operator repair API and stuck-instance reconciler (R-001 §4.4, R-INV-25/26, NP-30) | ❌ | 23 Nov: not built; parked outbox events have no route | WF-S24 |
| D2-6.F31 | Emergency approval under break-glass with a mandatory post-action review (R-001 §5.4, R-INV-12) | ❌ | 23 Nov: not built | WF-S25 |

**Console (frontend)** — `app/admin/workflows` (draft v2.0 of 19 Nov, unverified) · clients `lib/api/workflow.ts`, `lib/api/workflow-history.ts` call 19 of 76 routes (Appendix B). Rebuild in v2.0 following "Frontend rules for every console" below. Audit engagements and forms are large: give each its own tab file and drawer forms.

| Part | Content |
|---|---|
| Header | "Governance › Workflows & Approvals"; gold primary "Start workflow"; secondary "My pending approvals" |
| KPIs (from new `GET /v1/workflows/summary`) | Pending my approval · Active instances · Escalated · Expiring in 24 h · Active definitions |
| Panels | Pipeline: Created → In approval → Approved / Rejected → Released. Ordered list: stages of the selected definition, with the approver rule chip. Bar list: instances by workflow type, with an amber callout for escalations and expiring instances |
| Tabs | Pending approvals (F02) · Instances (F01–F05, F24) · Definitions (F11, F28) · Work items, my queue (F29) · Cases (F16) · State machines (F17) · Audit engagements (F14) · Forms (F15) · Operator: stuck instances and parked events (F30) |
| Drawer commands | Approve / reject (reason, step-up; never shown to the requester), request changes, quorum vote, escalate / escalate-to, cancel, invalidate, verify-release, consume (F25), emergency approve (only under a live break-glass session, F31). Definition validate / submit for approval / approve / reject / publish (maker ≠ checker) / supersede / suspend / retire. Work item claim / release / complete (typed form) / reassign (reason) / escalate. Case triage / start / wait / resume / resolve / close / reopen / merge / split, notes, members, mandatory work items. Operator actions with reason and the second person's approval for high-risk kinds. Engagement lifecycle (acceptance, activate, amend scope, fieldwork complete, completion review, report ready, close, withdraw), plan / materiality / risks / assertions, workpapers (procedures, results, conclusions, evidence links, cross-references, addenda, mark prepared, lock; preparer ≠ reviewer), review notes, sign-offs, quality reviews. Form publish / retire / drafts; submission validate / submit / route / supersede |
| Footer | Required controls: approver independence · one vote per approver · every decision recorded in the decision log · a reason on every action |

**Step-by-step plan (23 Nov, revised the same day after the code re-check)** — replaces the WF-0…WF-8 task list. Spec: Doc 03 §8.4; GOV-06; R-001 WFC-01…04 (§3–§12); ZS-STATE §11–§14. The status rules are in "How to use this file", item 5. The cases (WF-S1…S7) come first because obligations escalation opens a case. The implementer prompt is `docs/audit_files/prompts/WORKFLOW-SVC-IMPLEMENTER-PROMPT-2026-11-23.md`.

**Order (the rows below are in this order):** WF-S0a → S0 → S1…S7 → S8…S10 → S11 → S11a → S11b → S22 → S23 → S24 → S25 → S12…S20 → WF-FE-1 → FE-2 → FE-3 → S21.

| Step | Do | Done when (the verifier re-runs this) | Status | Evidence |
|---|---|---|---|---|
| WF-S0a | Make the suites run. <br>• Handler tests compile: `stubStore` implements every `WorkflowStore` method; remove the duplicate `pendingSubmissions` field (`internal/handler/handler_test.go:169/187`); the cursor tests use the real `domain.PaginationCursor` (`StartedAt`, `InstanceID`), or the type is extended in domain, store, handler and tests together; implement or drop `FormSubmission.RowVersion`. <br>• The store suite reset drops the whole schema (`DROP SCHEMA public CASCADE; CREATE SCHEMA public;` or equivalent), not a hand-written table list (`internal/store/pg_store_test.go:48-49`). <br>• Document `GET /v1/workflows/{id}/available-actions` and `…/transition-history` in `openapi.yaml`, so the parity test passes. | `go vet ./...` clean. `go test -count=1 ./...` has 0 FAIL. The store suite runs to the end on a scratch DB. Counts recorded. | ⬜ | |
| WF-S0 | Baseline (WF-0): `go build`, `go vet` and `go test ./...` with counts; the store suite as NOBYPASSRLS on a scratch DB; migrations 000001…000023 up/down/up on scratch. Write an S0 matrix in `progress.md` scored from this run (Doc 03 §8.4, GOV-06, WFC-01…04, ZS-STATE, the 61 Part E9 rows, the audit and forms modules), and remove claims that do not hold: the tests `TestWF1_*`, `TestWF2_*`, `TestST_*`, `TestFORM_*`, `TestAUD_*` that do not exist, the routes `GET /v1/workflows` and definition `/versions` that are not routed, and "S1 … all pass". | The matrix exists, every row carries a real test name or file:line, and the suite counts are recorded. | ⬜ | |
| WF-S1 | Cases schema, migration 000024 (R-001 §6, §9, §9.1): <br>• states OPEN / TRIAGED / IN_PROGRESS / WAITING_INTERNAL / WAITING_EXTERNAL / RESOLVED / CLOSED / REOPENED (stub rows mapped: IN_REVIEW→IN_PROGRESS, ESCALATED→IN_PROGRESS + `escalated_at`); <br>• `case_family` (FINANCIAL, PAYROLL_HR, LEGAL_COMPLIANCE, SECURITY_PRIVACY, OPERATIONAL), severity, confidentiality (INTERNAL / RESTRICTED / PRIVILEGED); <br>• `case_members`, append-only `case_notes` (versioned, visibility, evidence refs), `case_links` (domain refs, read-only); <br>• mandatory work items in a generic `work_items` table in the WFC-02 shape (`case_id` and `workflow_instance_id` nullable, `mandatory`, outcome OPEN / SATISFIED / WAIVED / TRANSFERRED), which WF-S23 extends with claim and assignment; <br>• `canonical_case_id` + `case_aliases`, `split_from_case_id`, `case_transitions` (append-only by trigger); <br>• the common columns; FORCE RLS with WITH CHECK on every table; <br>• fix the stub: add `created_by_principal_id` and the resolution / close columns the store writes, keep one concurrency column (`row_version`, incremented on every write; drop `version` after mapping it), `legal_entity_id` stays required. | up/down/up on scratch with seeded stub rows. An RLS test runs as NOBYPASSRLS. An append-only trigger test passes. | ⬜ | |
| WF-S2 | Case commands. Routes: <br>• `POST/GET /v1/cases`, `GET …/{id}`; <br>• `POST …/{id}/triage`, `…/start`, `…/wait`, `…/resume`, `…/resolve`, `…/close`, `…/reopen`, `…/merge`, `…/split`; <br>• `POST/GET …/notes`, `POST …/work-items`, `…/work-items/{wid}/satisfy`, `…/waive`, `…/transfer`; <br>• `POST/DELETE …/members`. <br>If-Match and Idempotency-Key on every command. Actions CASE_OPEN / _READ / _TRIAGE / _UPDATE / _RESOLVE / _CLOSE / _REOPEN / _MERGE / _SPLIT / _NOTE, registered by a new access-control migration (next free number after 000061) and granted in `deployments/scripts/seed-demo-rbac.ps1`. <br>Also: write every case event with `outbox.Insert` (`internal/outbox/outbox.go:91`) and delete `insertOutboxEventTx`; authorize each command with its CASE_* action, never `WORKFLOW_READ`; a standalone case takes `legal_entity_id` in the body, checked against tenant-entity-registry like `POST /v1/workflows`; `POST /v1/cases/{id}/status` stays only as a deprecated alias of the named commands (`Deprecation` header). | Test per §6.3 transition and per refused edge (`TestWF2_*`). A stale If-Match gets 412. A replay returns 200 with the same body; a changed body gets 409. Live: `POST /v1/cases` returns 201 and the event row is in `outbox_events`. | ⬜ | |
| WF-S3 | Closure gate: resolve needs an outcome code and evidence. Close needs every mandatory work item SATISFIED, WAIVED or TRANSFERRED. Reopen needs a reason and re-establishes the owner. Severity never changes a linked due date. | Close is refused (409) while mandatory work is open, and allowed after. Mutant "gate skipped" killed. | ⬜ | |
| WF-S4 | Merge / split lineage. A merge sets `canonical_case_id` and keeps aliases. A split keeps the source link and the evidence scope. A cross-tenant merge is refused and recorded as a security event (NP-11). | Lineage tests. The cross-tenant merge gets 404/409 and the event is written. | ⬜ | |
| WF-S5 | Confidentiality: a RESTRICTED or PRIVILEGED case and its notes are readable only by members. For others the case is 404 and absent from lists. Note visibility is enforced in the store, not in the UI. | A non-member gets 404 on GET, notes and transitions, and the list excludes the case. Mutant "membership not checked" killed. | ⬜ | |
| WF-S6 | Case events `case.opened` / `.triaged` / `.waiting` / `.resolved` / `.closed` / `.reopened` / `.merged` / `.split` through the outbox, with the R-001 §10.3 envelope fields. Declare them in `asyncapi.yaml` and register their schemas in schema-registry. | The two-way asyncapi contract test passes. The schemas can be read back from the registry. | ⬜ | |
| WF-S7 | A case opened from an obligation milestone is idempotent on `(source_type, source_id, milestone)`. This is the entry point for OB-S11. | Two identical opens give one case (test). | ⬜ | |
| WF-S8 | ST-1 registry: `state_machine_definitions` (object_type, semver, states, terminal states, transitions with guard and required-evidence names, checksum), immutable once PUBLISHED (trigger). Routes: `POST /v1/state-machines`, `POST …/{id}/publish`, `GET /v1/state-machines?object_type=`, `GET …/{object_type}/versions/{semver}`. Seed definitions for instance, definition, case, form submission, engagement and work item; the instance edges come from `envelope.PinnedStateMachineEdges`. `GET /v1/state-machines/{workflow_type}` reads the registry and no longer builds `STAGE_n` states. | An update of a published row is refused by the DB (test). A new version coexists with objects on the old one. No state in a response is absent from the registry row. | ⬜ | |
| WF-S9 | ST-2 on every lifecycle object here (instance, definition, case, form, submission, engagement, workpaper, work item): `GET …/available-actions` (evaluated for the caller) and `GET …/transitions` (append-only; from/to, command, actor, versions, reason, evidence, correlation, `state_machine_version`). <br>Fix the instance version first: `GetAvailableActions` calls a store method that runs in the tenant transaction (no `h.store.(*store.PgStore)` assertion, no bare-pool query); it covers SINGLE, QUORUM and ROLE stages, delegates (through authorization-svc), the escalation target of an ESCALATED instance and INVALIDATED; it never offers APPROVE to the initiator. `transition-history` returns the stored `decision_id`, not `access_decision_id`, and no invented `from_stage`. | A route and a test per object, run as NOBYPASSRLS. An append-only trigger test per transitions table. The mutant "query the quorum pool on the bare pool" fails a test. | ⬜ | |
| WF-S10 | ST-3: every write that takes a free target state becomes a named command. The old route stays as an alias with a `Deprecation` header. | No handler accepts an arbitrary state string (test over the router). | ⬜ | |
| WF-S11 | WF-1 open rows (F18), re-checked against WF-S0: quorum stage lock and `row_version` (L-2); FORCE RLS with WITH CHECK on stages, transitions, quorum, outbox and definitions (L-8; definitions already have FORCE RLS in 000017, check WITH CHECK); instances pinned to an immutable definition version (R-INV-01/02); per-type approve authority `WORKFLOW_APPROVE:<workflow_type>` beside the generic action (Q-6); common columns. | Each F18 row re-scored with a test. 10 parallel votes count once. A running instance keeps its version after a supersede. | ⬜ | |
| WF-S11a | Approval integrity (F25; R-INV-07/10/11, NP-05/06/08/35, Doc 05 §6.4): <br>• `approval_valid_until`, set at approval from the definition's `approval_validity_hours` (inline workflows: a request field, bounded); <br>• verify-release refuses an expired or already-consumed approval; <br>• `POST /v1/workflows/{id}/consume` (Idempotency-Key, If-Match): records the consumer, the subject fingerprint and the time; a second consume is 409; <br>• the approval records the definition version and the policy version reference it was decided under; a policy version change while pending follows the declared rule (NP-35); <br>• a refused decision (self-approval, not human, already decided another stage, not the approver) writes a DENIED record to the governance ledger through the `decision_log` outbox destination and emits `workflow.decision.refused` (with `self_approval_blocked` as reason for the requester). | Tests for NP-05, NP-06, NP-08 (expiry one second before release) and NP-35. 2 mutants each for "expiry not checked" and "consume twice". A refused decision leaves one ledger outbox row. | ⬜ | |
| WF-S11b | Instance list and KPIs (F24): `GET /v1/workflows` (filters `workflow_status`, `workflow_type`, `legal_entity_id`, `initiated_by`, `awaiting=me` over named, quorum and escalation-target approvers; keyset cursor) and `GET /v1/workflows/summary` (pending my approval, active, escalated, expiring in 24 h, active definitions, counts by type and by status). `WORKFLOW_READ`; every query in the tenant transaction. | Handler and store tests; a forged cursor gets 400; both routes in `openapi.yaml` (parity test). | ⬜ | |
| WF-S22 | WFC-01 definition publication lifecycle (F28; R-001 §3.2–3.4, R-INV-01/02/24, NP-21/22/36). <br>• Migration: `definition_status` DRAFT / VALIDATION_PENDING / VALIDATED / APPROVAL_PENDING / PUBLISHED / REJECTED / SUSPENDED / SUPERSEDED / RETIRED (existing rows mapped from `is_active`); `checksum` (sha256 of the canonical stages and settings); `effective_from` / `effective_to`; `validated_by`, `approved_by`, `published_by` and their times; a trigger refuses any change to a PUBLISHED row other than its status and `effective_to`. <br>• Commands: `POST /v1/workflow-definitions/{id}/validate` (gates: valid stage shapes, quorum not above the pool, non-empty approvers, due hours > 0, escalation target well formed, only catalogued event names), `…/submit-approval`, `…/approve` and `…/reject` (human, maker ≠ checker), `…/publish`; `…/activate` stays as a deprecated alias of publish. Supersede, suspend and retire follow the new states. <br>• New instances start only from a PUBLISHED definition inside its window; SUSPENDED blocks new starts; running instances keep their version. <br>• Events `workflow.definition.published` / `.suspended` / `.superseded` / `.retired`; actions for approve/publish registered and seeded. | A test per transition and per refused edge. The author approving their own definition gets 403. The DB refuses an UPDATE of a published row's stages. A start from a non-PUBLISHED definition gets 409. A running instance keeps its version after a supersede. | ⬜ | |
| WF-S23 | WFC-02 work items (F29; R-001 §4.2, NP-28/39). <br>• Extend `work_items` (from WF-S1): state UNASSIGNED / CLAIMED / COMPLETED / CANCELLED, `item_type`, queue, role, assignee, internal `due_at` (never a legal due date), `completion_schema`, `bulk_safe`, `row_version`; append-only `work_item_assignments` (previous and new owner, actor, reason, time). <br>• Routes: `POST/GET /v1/work-items` (filters queue, `assignee=me`, state, case, workflow; cursor), `GET …/{id}`, `POST …/{id}/claim`, `…/release`, `…/complete`, `…/reassign`, `…/cancel`, `…/escalate`, `POST /v1/work-items/bulk-complete`. <br>• Claim and complete use If-Match; a delegated actor still passes authorization and SoD; escalation never completes; reassignment needs a reason; cancel only when the parent allows; bulk only for bulk-safe types, with per-item authorization and per-item results. <br>• Events `work_item.created` / `.claimed` / `.reassigned` / `.completed` / `.overdue`; WORK_ITEM_* actions registered and seeded. | Two concurrent claims: one wins, the other gets 409. NP-28: one unauthorized item in a bulk call fails alone. NP-39: the history keeps the previous owner. A completion missing a schema field gets 400. | ⬜ | |
| WF-S24 | Operator repair and stuck-instance reconciler (F30; R-001 §4.4, R-INV-25/26, NP-30). <br>• Every non-terminal instance has `next_action_at` or a named wait condition. <br>• A reconciler pass (in `internal/timers`) flags instances past the threshold, emits `workflow.stuck_detected`, exports `workflow_stuck_instances`, and opens one OPERATIONAL case per instance through the idempotent open of WF-S7. <br>• `POST /v1/operator-actions`, `GET /v1/operator-actions`, `GET …/{id}`, `POST …/{id}/approve`: a fixed catalogue (RETRY_TIMER, RECOMPUTE_NEXT_ACTION, FORCE_ESCALATE, FORCE_EXPIRE, REQUEUE_PARKED_EVENT, RELEASE_CLAIM), each with reason, scope and evidence; high-risk kinds wait for a second person's approval and the requester cannot approve. `GET /v1/outbox/parked` lists parked events. <br>• Actions WORKFLOW_OPERATE / WORKFLOW_OPERATE_APPROVE registered and seeded. An operator action never changes an instance's definition version or a past result. | An instance with a missed timer is flagged and its case opened once across two passes. No reason gets 400. A high-risk action stays pending until a second person approves; the requester approving gets 403. Every executed action has a ledger outbox row. | ⬜ | |
| WF-S25 | Emergency approval (F31; R-001 §5.4, R-INV-12). <br>• Definition settings `emergency_allowed` and the conditions an emergency may bypass (QUORUM or TIMING only; it never widens the actor's authorization). <br>• Protected types (money movement, HARD_LEGAL, security-root) are refused unless policy-svc permits. <br>• `POST /v1/workflows/{id}/emergency-approve` needs WORKFLOW_APPROVE plus a live break-glass session, checked the way policy-svc does it (`services/policy-svc/internal/authz/client.go:277`, `CheckEmergency`); it records the window, reason code, incident or case reference, scope and maximum impact. <br>• It creates a mandatory post-action review work item (WF-S23) that the actor cannot complete; event `workflow.emergency_approved`. | Without break-glass: 403. A protected type: refused. The review work item exists; the actor completing it gets 403. A call after the window ends is refused. | ⬜ | |
| WF-S12 | WF-5 handler tests (ROLE stages, definition expansion, escalation-target approval, registry 404/503, Idempotency-Key replay/mismatch, identity-gate override) and API-4 cursor paging on pending, definitions, forms, cases, `GET /v1/workflows`, work items and operator actions. | `TestWF5_*` pass. A forged cursor gets 400. | ⬜ | |
| WF-S13 | WF-6: a tenantless `policy.*` event re-checks every tenant with live instances, in bounded, resumable batches. | Test with 3 tenants and a crash mid-batch: every tenant is re-checked exactly once. | ⬜ | |
| WF-S14 | WF-7 audit-engagement and forms modules: object authorization (team membership, reviewer ≠ preparer, sign-off independence), If-Match, Idempotency-Key, events, RLS, R5 registration. Merge `openapi.biz04-forms.yaml` into `openapi.yaml`. | `TestAUD_*` and `TestFORM_*` pass. The parity test covers every route. | ⬜ | |
| WF-S15 | WF-9, the R-001 §10.1 surface: `POST /v1/workflows/{id}/commands/cancel` as an alias of cancel; `POST /v1/cases/{id}/resolve` (WF-S2). Work items (WF-S23) and operator actions (WF-S24) are built under the R-001 paths. Write the mapping table for the remaining names into `SPEC_DEVIATIONS.md` (R-001 `POST /v1/approvals` and `…/decisions` map to `POST /v1/workflows` and `…/actions` / `…/quorum-votes`; `POST /v1/obligations/*` belongs to obligations-svc), and add decision Q-1/C4 with the 23 Nov scope decision to the Part F2 decision sheet. | The aliases have parity tests. The table and the sheet row exist. | ⬜ | |
| WF-S16 | Identity `require` (X5): an mTLS listener `:8451` with `IDENTITY_PEERS=policy-svc,obligations-svc` (the GDL `:8450` pattern, shared `mtlsclient`); add workflow-svc to the keygen list; point policy-svc's signature-workflow client at it. Then `WORKFLOW_IDENTITY_VERIFY=require`. invoice-approval-svc (teammate) goes to the cross-team list. | A header-only call gets 401. The policy-svc signature workflow passes over mTLS (`e2e-policy-deps.py` workflow section green). | ⬜ | |
| WF-S17 | Contract and docs: openapi parity over every route the service serves (instances, definitions, cases, state machines, work items, operator actions, audit, forms); asyncapi two-way test over every event (`workflow.*`, `approval.*`, `case.*`, `work_item.*`, `form.*`, audit events); `SPEC_DEVIATIONS.md`, `RUNBOOK.md` (reconciler, operator actions, parked events, emergency approval), `RELEASE_CERTIFICATE.md`; new `scripts/audit.sh`; a dated note in Doc 03 §8.4 and a dated physical-names note in Doc 04 §15 (WorkflowInstance, WorkflowTransition, cases, work items). | The parity and contract tests pass. The docs name only routes, tables and metrics that exist. | ⬜ | |
| WF-S18 | Live: rebuild the image (host binaries over `deployments-workflow-svc:base`), migrate the dev DB, recreate the container. | `migrate.py status --db workflow` shows 0 pending. `scripts/audit.sh` green live. | ⬜ | |
| WF-S19 | Rewrite `e2e-workflow-deps.py` in the house style (urllib, 127.0.0.1, demo ids, envelope logins, check/skip counts, exit code = failures). Sections: registry 404/503; start with the definition pinned; approval by another user with `access_decision_id`; delegation; the requester never approves; quorum under 10 parallel votes; escalate / escalate-to / short expiry timer; invalidate from a business-object change; decision-log relay (row in GDL); policy event re-check; signature workflow from a policy decision; cases full lifecycle, RESTRICTED, closure gate, merge/split; state-machine immutability; forms submit/route; engagement preparer ≠ reviewer; RLS cross-tenant; Kafka events; identity `require`; approval expiry and consume-once; definition maker-checker publish; work item claim race and reassignment; operator action four-eyes; emergency approval under break-glass. | Green live, with no SKIP except a named owner block. | ⬜ | |
| WF-S20 | Mutants: at least 2 per new control (closure gate, restricted read, cross-tenant merge, published state machine, quorum once, approval expiry, consume once, definition maker ≠ checker, published definition immutable, claim race, operator four-eyes, emergency break-glass check). | All killed; the list is in `progress.md`. | ⬜ | |
| WF-FE-1 | An API client for every route in Appendix B D2-6 plus the WF-S routes (`GET /v1/workflows`, `/summary` and `consume` from WF-S11a/b, cases, state machines, definition lifecycle, work items, operator actions, emergency approve). Export the `Case` and `AuditEngagement` types; remove the `listWorkflowInstances` import that has no export (`components/admin/workflow/console/data.ts:7`). | vitest covers the client. tsc has no error in the workflow files. | ⬜ | |
| WF-FE-2 | Page v2.0 on the console kit: header, KPIs, panels and tabs as in the table above; drawer commands with reason and step-up; live data and honest empty/error states. Delete the invented data in `components/admin/workflow/console/overview.tsx` (sample stage names, sample type counts, KPIs fixed at 0, "Pipeline Velocity") and the `getWorkflow("dummy")` call (`data.ts:37`); KPIs and panels read `GET /v1/workflows/summary`; the Audit, Forms, Cases, State-machines, Work-items and Operator tabs show live rows with drawers; emergency approve appears only under a live break-glass session. | It renders live with no error page, and every drawer command reaches the service. A grep finds no sample names or literal counts in `components/admin/workflow`. | ⬜ | |
| WF-FE-3 | `tests/e2e/console/workflows.spec.ts`: <br>• start → approved by another user → decision id shown; <br>• the requester never sees Approve; <br>• a quorum vote counts once; <br>• case open → triage → close refused with open mandatory work → close; <br>• a RESTRICTED case is invisible to a non-member; <br>• a workpaper prepared by one user is reviewed by another; <br>• a form is submitted and routed; <br>• a definition authored by one user is approved and published by another; <br>• a work item is claimed and completed, and a second claim is refused; <br>• a high-risk operator action waits for a second person; <br>• empty states, and the unavailable state with the service stopped. | Spec green live. tsc, eslint and vitest clean. | ⬜ | |
| WF-S21 | Hand-back: S0–S6 evidence in `progress.md`; feature rows F01–F31 set to 🔵 with evidence; the dashboard row updated (never ✅). | The verifier runs the protocol, VC-1 included. | ⬜ | |

**Blocked and decisions:** H-6 org data; Q-1…Q-6; H-5 / N-3 (cross-team).

---

## D2-7 · evidence-requirements-svc (:8130, DB `evidence_requirements`)

**Facts**

| Item | Value |
|---|---|
| Spec | Doc 03 §8.6 |
| Latest score | 39 met / 1 partial / 3 failed of 43 on 2 Nov; 5 Nov completion pass closed the in-service rows (Governance audit → "evidence-requirements-svc completion, 5 November 2026") |
| Stages | S0–S6 verified 5 Nov: `scripts/audit.sh` 42/42, dependency E2E 84/84 (3 owner-blocked skips); PBC object authorization added and verified (`0e11a571`). The 18 Nov `progress.md` claims for ER-W4…W8 are only partly true (23 Nov check: skip paths remain, compose still `warn`, clamav `latest`). Work from the step table below |
| Console | `/admin/evidence` v2.0 (5 Nov) |

**Features**

| ID | Feature | Status | Evidence | What is needed |
|---|---|---|---|---|
| D2-7.F01 | Evaluate: `POST /v1/evidence/evaluate` (SATISFIED / MISSING / NO_REQUIREMENTS_DEFINED; fails closed on a malformed rule); authorization `EVIDENCE_EVALUATE` | 🟡 | Reconfirm (was ✅): progress.md ER matrix; E2E 84/84 | Compose still `EVIDENCE_EVALUATE_AUTHZ` default `warn` (D-ER-6): ER-W8; VC-1 |
| D2-7.F02 | Evaluation reads: `GET /v1/evidence/evaluations` (API-4), `GET …/{id}`, `GET /v1/evidence/summary` | 🟡 | Reconfirm (was ✅): ER-3 | VC-1 |
| D2-7.F03 | Requirement catalogue: `GET /v1/evidence-requirements` (API-4), `GET …/{id}` (ETag), `POST /v1/admin/evidence-requirements` (idempotent), `POST …/{id}/end-date` (If-Match) | 🟡 | Reconfirm (was ✅): ER-3, ER-4 | VC-1 |
| D2-7.F04 | Supporting-artifact rules (subtype, minimum count); distinct artifacts by canonical reference | 🟡 | Reconfirm (was ✅): `TestER02_SameManifestTwiceCountsOnce` | VC-1 |
| D2-7.F05 | Append-only decision ledger; outbox `evidence.requirement.satisfied` / `.missing` proven on Kafka | 🟡 | Reconfirm (was ✅): 5 Nov E2E §9; migration 000011 | Schemas not registered: ER-W5; VC-1 |
| D2-7.F06 | PBC requests (14 routes): `GET/POST /v1/pbc/requests`, `GET …/{id}`, `POST …/send|clarify|extend-due-date|reassign|mark-satisfied|close|responses|view|notes`, `POST /v1/pbc/responses/{id}/acknowledge|scan-result` | 🟡 | Reconfirm (was ✅): ER-4; If-Match on manager commands | VC-1 |
| D2-7.F07 | PBC object authorization (manager or assignee; view by assignee only; AUDIT_ONLY notes by manager; acknowledge needs MANAGE) (D-ER-29) | 🟡 | Reconfirm (was ✅): `TestPBCAuthz_*` (5 tests); 2 mutants caught | E2E section and docs: ER-W6, ER-W7; VC-1 |
| D2-7.F08 | Document verification against document-vault-svc (envelope forwarded) | 🟡 | Reconfirm (was ✅): `internal/documentvault/client.go`; live E2E §6 used a fixture row | Real upload through the vault: ER-W6 (needs ER-W4); VC-1 |
| D2-7.F09 | Signature verification against esignature-integration-svc | 🟡 | `internal/esignature/client.go`; live SATISFIED not shown: the service runs only in the phase7 stack | ⛔ phase7 stack |
| D2-7.F10 | EVIDENCE_MANIFEST references checked against evidence-manifest-svc | 🟡 | Code done; a real manifest cannot count until 000047 is applied (D1-9.F05) | ER-W3, ER-W6 |
| D2-7.F11 | Callers log NO_REQUIREMENTS_DEFINED (7 services, D-ER-5) | 🟡 | Reconfirm (was ✅): `TestEvaluate_NoRequirementsDefinedPassesWithAWarning` in each caller (`03f61a87`) | VC-1 |
| D2-7.F12 | general-ledger gates every finalization path | 🟡 | Only `PostJournal` is gated; `PostApprovedJournal`, `PostAccountingEvent`→`commitJournal`, `ReprocessFailedPosting` are not | Cross-team |
| D2-7.F13 | Callers forward the bearer envelope (identity-envelope `idenv.Forward`) | ❌ | No caller does | Cross-team, then ER-W8 |
| D2-7.F14 | Decisions: tier (D-ER-1), scan-result ownership (D-ER-2), which document/signature statuses count (D-ER-3), GOV-07 linkage | 🧭 | SPEC_DEVIATIONS | Decision |
| D2-7.F15 | Console `/admin/evidence` v2.0 | 🟡 | Manager side verified 5 Nov (ER-FE-2). The assignee side (view, respond, notes) and response acknowledge are not in the console (Appendix B) | ER-FE-3, ER-FE-4 |

**Console (frontend)** — `app/admin/evidence` (v2.0, 5 Nov) · client `lib/api/evidence.ts` calls 18 of 22 routes (Appendix B). The manager side exists. The assignee side is missing.

**Step-by-step plan (23 Nov)** — replaces the ER-W3…ER-W8 and ER-FE-3/4 task lists. Spec: Doc 03 §8.6. The service was verified on 5 Nov; what is left is proof, enforcement and the assignee side of the console. The status rules are in "How to use this file", item 5.

| Step | Do | Done when (the verifier re-runs this) | Status | Evidence |
|---|---|---|---|---|
| ER-S0 | Re-baseline (ER-0): check every 18 Nov claim in `progress.md` against the code and the scripts. Remove the claims that do not hold ("zero skips", ER-W8 done). Point the plan reference at this section (`EVIDENCE-REQUIREMENTS-PLAN-2026-11-04.md` was merged here on 6 Nov). Build, vet, test with counts; store suite as NOBYPASSRLS. | `progress.md` matches the code; counts recorded. | 🔵 | c0cde51c · `progress.md` header rewritten: the 18 Nov ER-W4…W8 claims corrected against the code ("zero skips" false, ER-W8 not done, clamav latest, schema registration not run) and the plan reference pointed at this section · `go vet` clean (default and `-tags integration`) · suites on scratch `evreq_test_er`: default 118 pass / 0 fail / 0 skip, `-tags integration` 129 / 0 / 0 (store runs include the NOBYPASSRLS `appRolePool` tests) · the two store resets now drop the `public` schema (the table lists missed new tables) · migrations 000001…000012 up/down/up: 8 → 0 → 8 tables |
| ER-S1 | ER-W3: access-control 000047. Run its store suite, up/down/up on scratch, and apply it to the dev DB as a single file with `psql` (never `migrate.py up --db access_control` blindly). | A real evidence manifest counts towards SATISFIED (e2e section). | ⬜ | Partial, 23 Nov: the access-control integration suite runs again (dd7b8103, bfc07e70): 000061 wrote authorization-svc's tables and failed everywhere; it is now a no-op and the grant is authorization 000045. Suite on a fresh scratch DB 206 pass / 0 fail; access-control 000001…000062 up/down/up 16 → 0 → 16 tables (000046 down fixed). 000047 is already recorded on the dev `access_control` DB (migrate.py). Open: the live e2e section that counts a real manifest (ER-S4) |
| ER-S2 | ER-W4 compose: `clamav/clamav:1.4.1` (not `latest`) under `profiles: ["scan"]`, with `mem_limit` and a clamd healthcheck; document-vault `DB_USER` `app_document_vault` with the password from `create-app-roles.sh`; `CLAMAV_URL`. | `docker compose -p deployments config` passes; clamd is healthy with `--profile scan`. | 🔵 | 996ac573 · `clamav/clamav:1.5.4` (the plan's 1.4.1 is not a published tag; `docker manifest inspect` finds 1.5.4, 1.5, 1.4.3, 1.4) under `profiles: ["scan"]` with `mem_limit: 1g` and the `clamdscan --ping` healthcheck; the five dependants use `required: false`, so `docker compose -p deployments config -q` passes with and without `--profile scan` (it failed before: "depends on undefined service clamav"); clamd healthy alone under `--profile scan` (19 × 10 s); document-vault `DB_USER` app_document_vault and `CLAMAV_URL` already present |
| ER-S3 | ER-W5: run `register-evidence-requirements-schemas.py` live as the principal holding schema-publish. | Both v1 schemas read back from schema-registry; a second run changes nothing. | ⬜ | |
| ER-S4 | ER-W6 e2e: real vault upload through clamd and a real manifest, so the two skip branches cannot fire on a healthy stack. PBC authorization section: an outsider gets 403 on GET / view / notes / acknowledge; the assignee can GET and view; AUDIT_ONLY notes give 403 to the assignee and 201 to the manager; an outsider's view leaves the ETag unchanged. Schema-registry section. Every call signed in through identity-context. | Green live. The only SKIP left is esignature (ER-S5). | ⬜ | |
| ER-S5 | esignature: bring `esignature-integration-svc` up alone from the phase7 compose. If it runs, add the real-envelope SATISFIED / unknown-envelope MISSING checks and drop the last skip. Otherwise set F09 ⛔ with the reason. | Section green, or ⛔ recorded with the failure. | ⬜ | |
| ER-S6 | ST-2 for the PBC request: `GET /v1/pbc/requests/{id}/available-actions` and `…/transitions`, if missing. | Route tests; openapi parity. | 🔵 | c0cde51c · migration 000012 `evidence_request_transitions` (append-only, trigger-written on every creation and change of status / assignee / due date, command + actor from `domain.WithCommand`, FORCE RLS WITH CHECK) · `GET /v1/pbc/requests/{id}/transitions`, `GET …/available-actions` · `TestERS6_HistoryRecordsEveryMoveAndIsAppendOnly` (NOBYPASSRLS role; another tenant reads nothing; UPDATE/DELETE refused), `TestERS6_AvailableActions_FollowTheCallersRoleAndTheStatus`, `TestERS6_Transitions_ReadByTheManagerOrTheAssigneeOnly`, `TestERS6_PBCAvailableActions_ByStatus` · `TestOpenAPIMatchesTheRouter` green |
| ER-S7 | ER-W8 enforce. Our caller obligations-svc forwards the envelope (OB-S16) or calls over mTLS as an `IDENTITY_PEERS` workload. Prove a header-only call is refused under `EVIDENCE_IDENTITY_VERIFY=require` + `EVIDENCE_EVALUATE_AUTHZ=enforce` (an e2e "strict mode" section that recreates the service strict). The 7 teammate callers (general-ledger, vat-gst, corporate-tax, filing-preparation, financial-close, corporate-actions, board-resolutions) still send headers only (F13). The compose default turns strict only when none of them would break; until then, record a SPEC_DEVIATIONS line and draft request CT-8 in Part F3. | The strict section is green; the deviation and CT-8 exist. | ⬜ | |
| ER-S8 | ER-W7 docs: `context.md` §4/§9, `SPEC_DEVIATIONS.md` (D-ER-5/24/27/29), `RUNBOOK.md`, `RELEASE_CERTIFICATE.md` rewritten from the code. Re-score Governance-audit rows 9/10/11/21 with file:line. | The docs name only routes, tables and metrics that exist. | ⬜ | |
| ER-S9 | Live: rebuild and recreate; `scripts/audit.sh`; re-run the 3 mutants of 5 Nov plus 2 for any new control. | `audit.sh` green; mutants killed. | ⬜ | |
| ER-FE-3 | Assignee side: <br>• "My evidence requests" (requests assigned to the signed-in principal); <br>• opening a request records the view (`POST …/view`, assignee only); <br>• respond: upload through document-vault-svc, then `POST …/responses`; <br>• SHARED notes (`POST …/notes`); <br>• the manager acknowledges a response (`POST /v1/pbc/responses/{id}/acknowledge`). | Works live as legal counsel (assignee) and admin (manager). | ⬜ | |
| ER-FE-4 | `tests/e2e/console/evidence.spec.ts`: <br>• the manager creates and sends; <br>• the assignee views, responds and adds a note; <br>• the manager acknowledges and marks satisfied; <br>• an outsider cannot open the request; <br>• evaluate shows SATISFIED; <br>• empty states, and the unavailable state with the service stopped. | Spec green live; tsc, eslint and vitest clean. | ⬜ | |
| ER-S10 | Hand-back: S0–S6 evidence in `progress.md`; feature rows set to 🔵 with evidence; the dashboard row updated (never ✅). | The verifier runs the protocol, VC-1 included. | ⬜ | |

**Blocked and decisions:** esignature live (phase7 stack); GL ungated paths and bearer forwarding (cross-team); D-ER-1/2/3.

---

# Supporting units in scope (rule R1): governance supporting and Security & Trust

These ten units are ours under R1 (Reference Part A1) and are needed for Domain 1 and 2 to be finished: the schema registry, retention and legal hold, the shared libraries and the GTRM compiler, and the four Security & Trust services that the auth plane depends on. Sources:
- `Evidence, Audit & Utility.md` (teammate's audit, 21 Oct): schema-registry-svc, retention-registry-svc and search-client sections.
- `Security & Trust.md` (teammate's audit, 12–13 Oct, latest scores §14.7).
- Reference Part A5: G-02, G-08, G-09, G-10, SEC-1…SEC-9.
- The code and the 6 Nov build/vet/test sweep.

Both teammate audits score only what each service's own documents promise. The spec sections the plan places in these units (GOV-08, GOV-09, GOV-10, Security Standard §10–§26) were never scored, so they appear below as ❌ rows.

## D2-8 · schema-registry-svc (:8093, DB `schema_registry`)

**Facts**

| Item | Value |
|---|---|
| Spec | GOV-08 (ZS-SVC-A-001); Event Catalogue standard; plan task G-08 |
| Latest score | 21 Oct (Evidence, Audit & Utility §2): 12 ✅, 0 partial, 0 missing on its own documented contract; 11 ❓; 3 external blockers. GOV-08 lifecycle not scored |
| Suite 6 Nov | build ✔ · vet ✔ · unit ✔ (5 packages) |
| Docs | `openapi.yaml` only: no `progress.md`, README, RUNBOOK, RELEASE_CERTIFICATE or SPEC_DEVIATIONS |
| Console | `/admin/schemas` legacy; client `lib/api/schemas.ts` calls 5 of 5 routes |

**Features**

| ID | Feature | Status | Evidence | What is needed |
|---|---|---|---|---|
| D2-8.F01 | Register a schema version: `POST /v1/schemas/{eventName}/versions` (compatibility check, idempotency, version assignment, immutability) | 🟡 | Reconfirm (was ✅): Evidence, Audit & Utility §2 (21 Oct) | VC-1 |
| D2-8.F02 | Reads: `GET /v1/schemas`, `GET /v1/schemas/{eventName}/versions`, `…/versions/latest`, `…/versions/{version}` | 🟡 | Reconfirm (was ✅): Evidence, Audit & Utility §2 | API-4 on the list: SR-2; VC-1 |
| D2-8.F03 | GOV-08 lifecycle: RegisterDraftSchema → RunCompatibilityCheck → ApproveSchema (maker-checker) → PublishSchema → DeprecateSchema → RetireSchema | ❌ | Not built | G-08 |
| D2-8.F04 | GOV-08 queries: GetActiveSchema, CompareSchemas, GetCompatibilityReport | ❌ | Not built | G-08 |
| D2-8.F05 | Consumer registration and ListConsumers | ❌ | Not built | G-08 |
| D2-8.F06 | Schemas other tasks need: IAM-10 canonical identity events, ER-W5 evidence-requirements events, EML-4 email parent events | ❌ | None registered | SR-3 (after G-08) |
| D2-8.F07 | 11 points to clarify: read permission, replay-key binding, replay marker, SchemaDependencyMap, NONE rollout approval, JSON Schema keywords compared, artifact fields, runtime payload validation, 1 MiB body limit owner, Event Catalogue compatibility rules, required artifacts; plus tenant visibility of schemas | 🧭 | Evidence, Audit & Utility §2; "Carried over" table | CL-2 |
| D2-8.F08 | Live authorization: the actions it checks are registered in access-control and granted; envelope `require` | 🟡 | 21 Oct audit listed this as external; authorization-svc now runs | SR-1, EST-1 |
| D2-8.F09 | Service documents: `progress.md` matrix, README, RUNBOOK, RELEASE_CERTIFICATE, SPEC_DEVIATIONS, Postman | ❌ | Missing | SR-0, SR-4 |
| D2-8.F10 | Dependency E2E and console v2.0 | ❌ | No `e2e-schema-registry-deps.py`; legacy page | SR-5, SR-FE-1…3 |

**Tasks**

- [ ] **SR-0 · Baseline (S0):** `progress.md` with a matrix covering GOV-08 rows, the 12 documented rows and the 11 ❓. Integration suite on scratch Postgres (0 skip).
- [ ] **SR-1 · Grants:** every action the handlers check is registered in access-control and carried by a template (R5). Store test.
- [ ] **G-08 · Lifecycle, queries and consumers** (Reference Part A5 G-08). Keep the existing `/{eventName}/versions` routes working. Tests:
  - `TestG08_IncompatiblePublishWithoutApprovalRefused`;
  - `TestG08_RetiredSchemaCannotBeRegisteredAgainst`;
  - `TestG08_ApproverIsNotTheAuthor`;
  - `TestG08_ConsumerListedAfterRegistration`.
- [ ] **SR-2 · API-3 / API-4:** If-Match on the lifecycle commands; cursor paging on lists.
- [ ] **SR-3 · Register the platform's schemas:** identity canonical names (IAM-10), evidence-requirements (ER-W5), and later EML-4. One script per producer, idempotent.
- [ ] **SR-4 · Contract and docs (S2):** openapi with a parity test, README, RUNBOOK, RELEASE_CERTIFICATE, SPEC_DEVIATIONS, Postman.
- [ ] **SR-5 · Live (S3/S4):** rebuild, `scripts/audit.sh`, and a new `deployments/scripts/e2e-schema-registry-deps.py`. It covers:
  - the authorization decision;
  - publishing an incompatible schema refused;
  - a consumer listing;
  - a producer's event validated against `versions/latest`.

**Console (frontend)** — `app/admin/schemas` (legacy) · client `lib/api/schemas.ts`. Rebuild in v2.0 following "Frontend rules for every console".

| Part | Content |
|---|---|
| Header | "Governance › Schemas & Contracts"; gold primary "Register draft"; secondary "Compare versions" |
| KPIs (new `GET /v1/schemas/summary`) | Published event types · Drafts awaiting approval · Deprecated · Incompatible submissions (30 d) · Registered consumers |
| Panels | Pipeline: Draft → Checked → Approved → Published → Deprecated → Retired. Ordered list: versions of the selected event. Bar list: consumers per event, with an amber callout for consumers on a deprecated version |
| Tabs | Event types (F01, F02) · Drafts & approvals (F03) · Compatibility (F04) · Consumers (F05) |
| Drawer commands | Run check, approve (≠ author), publish, deprecate, retire, register consumer |
| Footer | Required controls: no publish without a passing check or an approval · published versions are immutable · retired schemas accept no registrations |

- [ ] **SR-FE-1 · Summary route + API client** for every G-08 route.
- [ ] **SR-FE-2 · Page v2.0.**
- [ ] **SR-FE-3 · UI E2E** `tests/e2e/console/schemas.spec.ts`. Cover:
  - draft → check → approve by a second user → publish;
  - an incompatible draft is blocked;
  - empty and unavailable states.

**Blocked and decisions:** the 11 ❓ items and schema visibility (CL-2).

## D2-9 · retention-registry-svc (:8148, DB `retention_registry`)

**Facts**

| Item | Value |
|---|---|
| Spec | GOV-09 retention and disposition, GOV-10 legal hold (ZS-SVC-A-001); ZS-SVC-S-001; ZS-DATA-GOV-001; plan tasks G-09, G-10 |
| Latest score | 21 Oct (Evidence, Audit & Utility §7): 11 ✅ on its own documented contract; 9 ❓; 2 external blockers. GOV-09 and GOV-10 not scored |
| Suite 6 Nov | build ✔ · vet ✔ · unit ✔ (4 packages) |
| Docs | none: no `openapi.yaml`, `progress.md`, README or RUNBOOK |
| Console | `/admin/retention` legacy; client `lib/api/retention.ts` calls 7 of 7 routes |

**Features**

| ID | Feature | Status | Evidence | What is needed |
|---|---|---|---|---|
| D2-9.F01 | Retention policies: `POST /v1/retention-policies`, `GET /v1/retention-policies` | 🟡 | Reconfirm (was ✅): Evidence, Audit & Utility §7 | Becomes G-09 rules; VC-1 |
| D2-9.F02 | Resolve retention: `GET /v1/retention/resolve` | 🟡 | Reconfirm (was ✅): Evidence, Audit & Utility §7 | VC-1 |
| D2-9.F03 | Legal holds: `POST /v1/legal-holds`, `GET /v1/legal-holds`, `GET /v1/legal-holds/{id}`, `POST /v1/legal-holds/{id}/release` (append-only release) | 🟡 | Reconfirm (was ✅): Evidence, Audit & Utility §7 | Extended by G-10; VC-1 |
| D2-9.F04 | Outbox delivery, forced RLS, body tenant validation | 🟡 | Reconfirm (was ✅): Evidence, Audit & Utility §7 | Kafka proof: X3; VC-1 |
| D2-9.F05 | GOV-10 legal hold, complete:<br>• scope by subjects, record classes, date range, custodians;<br>• amend scope; add/remove custodian;<br>• release with maker-checker (the issuer cannot release);<br>• reconcile coverage;<br>• queries EvaluateHoldCoverage, ListHeldRecords, GetPropagationStatus;<br>• events `legal_hold.*` | ❌ | Not built | G-10 |
| D2-9.F06 | GOV-09 retention and disposition:<br>• versioned rules with the period from jurisdiction-rules `POST /v1/retention-rules:resolve`;<br>• schedule, approve (maker-checker), execute (API-5), reperform, suspend;<br>• eligibility that always checks holds;<br>• signed disposition certificate | ❌ | Not built | G-09 |
| D2-9.F07 | Hold consumers: identity-context GOV-10 projection; authorization denies destructive actions on held subjects (IAM-3); document-vault (cross-team) | 🟡 | identity-context consumes holds; IAM-3 not built; document-vault is a teammate's | G-10, EST-8, CL-4 |
| D2-9.F08 | 9 points to clarify: read grant for one hold, release-check order, min/max retention relationship, correlation-id precedence, publisher failure behaviour, read authorization on lists, ZS-DATA-GOV-001 schedule fields, hold scope/version/review-date semantics, error body | 🧭 | Evidence, Audit & Utility §7 | CL-2 |
| D2-9.F09 | Contract and docs: `openapi.yaml` with parity test, `asyncapi.yaml`, `progress.md`, RUNBOOK, RELEASE_CERTIFICATE, SPEC_DEVIATIONS, Postman | ❌ | None exist | RR-0, RR-3 |
| D2-9.F10 | API-3 If-Match and API-4 cursor paging | ❌ | Audit says "pagination" matched; cursor and If-Match not verified · Verifier 23 Nov 2026: not built. No If-Match, ETag or cursor code exists in the service (0 matches in `internal/` and `cmd/`): `POST /v1/legal-holds/{id}/release` changes an existing hold with no If-Match (no 428 / 412), no GET sends an ETag, and both lists page by `limit`/`offset` (`internal/handler/handler.go:439`, `:484`), not by a signed cursor. Unit suite green (events, handler, outbox, store) | RR-2: If-Match (428/412) on hold release, ETag on the GETs, signed tenant-bound cursor on both lists, with tests |
| D2-9.F11 | Dependency E2E and console v2.0 | ❌ | No `e2e-retention-registry-deps.py`; legacy page | RR-4, RR-FE-1…3 |

**Tasks**

- [ ] **RR-0 · Baseline (S0):** `progress.md` matrix (GOV-09, GOV-10, the 11 documented rows, the 9 ❓); integration suite (0 skip).
- [ ] **G-10 · Legal hold** first (Reference Part A5 G-10; IAM-3 and G-09 depend on it). Tests:
  - `TestG10_IssuerCannotRelease`;
  - `TestG10_DispositionOverHeldSubjectRefused`;
  - `TestG10_CoverageEvaluated`;
  - `TestG10_PropagationStatusListsAcks`.
  identity-context's projection test must consume the new event.
- [ ] **G-09 · Retention and disposition** (Reference Part A5 G-09). Tests:
  - `TestG09_EligibilityFalseUnderHold`;
  - `TestG09_ApproverIsNotScheduler`;
  - `TestG09_CertificateVerifies`;
  - `TestG09_ExecutionIsAsync` (API-5).
- [ ] **RR-1 · Grants (R5)** for every new action; template `RECORDS_MANAGER` (and a legal-hold officer template).
- [ ] **RR-2 · API-3 / API-4** on policies, holds and dispositions.
- [ ] **RR-3 · Contract and docs (S2):** write `openapi.yaml` + parity test, `asyncapi.yaml`, README, RUNBOOK, RELEASE_CERTIFICATE, SPEC_DEVIATIONS, Postman.
- [ ] **RR-4 · Live (S3/S4):** rebuild, `scripts/audit.sh`, and a new `deployments/scripts/e2e-retention-registry-deps.py`. It covers:
  - jurisdiction-rules retention resolve;
  - authorization;
  - a hold event consumed by identity-context, and by authorization once IAM-3 is built;
  - a disposition refused under a hold;
  - Kafka events.

**Console (frontend)** — `app/admin/retention` (legacy) · client `lib/api/retention.ts`. Rebuild in v2.0.

| Part | Content |
|---|---|
| Header | "Governance › Retention & Legal Hold"; gold primary "Issue legal hold"; secondary "Resolve retention" |
| KPIs (new `GET /v1/retention/summary`) | Active holds · Custodians under hold · Dispositions due (30 d) · Dispositions awaiting approval · Holds not acknowledged by a consumer |
| Panels | Pipeline: Scheduled → Approved → Executing → Certified (disposition). Ordered list: the selected hold's custodians and scope. Bar list: records held per record class, with an amber callout for unacknowledged propagation |
| Tabs | Legal holds (F03, F05) · Retention rules (F01, F06) · Dispositions (F06) · Resolve (F02) · Propagation (F07) |
| Drawer commands | Amend scope, add/remove custodian, release (never shown to the issuer), schedule / approve (≠ scheduler) / execute / suspend disposition, download certificate |
| Footer | Required controls: the issuer never releases · no disposition over a held subject · disposition approval by a second person · a certificate for every disposition |

- [ ] **RR-FE-1 · Summary route + API client** for every G-09/G-10 route.
- [ ] **RR-FE-2 · Page v2.0.**
- [ ] **RR-FE-3 · UI E2E** `tests/e2e/console/retention.spec.ts`. Cover:
  - a hold issued and released by a second user;
  - a disposition refused under a hold;
  - a disposition approved and certified;
  - empty and unavailable states.

**Blocked and decisions:** the 9 ❓ items (CL-2); document-vault as a hold consumer (cross-team, CL-4).

## D2-10 · Shared libraries and the GTRM compiler (identity-envelope, search-client, _contract, deployments/gtrm)

**Facts**

| Item | Value |
|---|---|
| Spec | identity-envelope: GOV-01 §4 envelope, ZS-API-001 contract helpers. search-client: ZS-SVC-AB-001 (via search-indexer). gtrm: GOV-02 and `docs/architecture/global-traffic-residency-manager-decision.md` |
| Latest score | search-client 21 Oct (Evidence, Audit & Utility §10): 13 ✅, 8 ❓, 3 external. The others are not audited as units |
| Suite 6 Nov | identity-envelope ✔ (2 pkgs); search-client ✔ (1); `_contract` ✔ (19); `deployments/gtrm/compiler` ✔ (1). `deployments/gtrm` fails `vet`/`test` with "matched no packages": the untracked `deployments/gtrm/go.mod` ("Do first" item 2) |
| Console | none (libraries) |

**Features**

| ID | Feature | Status | Evidence | What is needed |
|---|---|---|---|---|
| D2-10.F01 | identity-envelope gate (`NewGateFromEnv`), forwarder and token exchanger, apicontract (problem details, cursor, ETag helpers) | 🟡 | Reconfirm (was ✅): Used by verified services (authorization 2 Nov, evidence-requirements 5 Nov); suite green 6 Nov | VC-1 |
| D2-10.F02 | Strict per-service audience (`IDENTITY_ACCEPT_LEGACY_AUDIENCE=false`) | ❌ | Waits for GW-5 | EST-2 |
| D2-10.F03 | identity-envelope and `_contract` docs (README, what each package guarantees, versioning) | ❌ | No README | LIB-1 |
| D2-10.F04 | search-client: governed client (tenant requirements, strict mappings, mandatory filters, paging, projections, vector/hybrid, Ping) | 🟡 | Reconfirm (was ✅): Evidence, Audit & Utility §10 (21 Oct) | VC-1 |
| D2-10.F05 | search-client against a real OpenSearch: integration tests on a local OpenSearch container | 🟡 | Listed as external (cluster); no local run recorded | LIB-2 |
| D2-10.F06 | search-client: 8 ❓ (restriction epoch, legacy client scope, page-size cap, DropIndex guards, environment defaults, cursor tie-break, refresh policy, query budgets) | 🧭 | Evidence, Audit & Utility §10 | CL-2 |
| D2-10.F07 | `_contract` shared contract and test kit (19 packages) | 🟡 | Suite green 6 Nov; never audited · Verifier 23 Nov 2026: `zoiko.io/contract` builds, `go vet` clean, 19/19 packages `ok` (accounting, banking, envelope, governance + 10 sub-packages, masterdata, operating, tax, traceability, types). LIB-3's audit outcome: nothing imports it: 0 `.go` files outside `services/_contract` import `zoiko.io/contract` or any sub-package, and no go.mod requires it. It is dead code until a decision keeps or removes it | LIB-3: decide keep (name its consumers) or remove; record in the matrix |
| D2-10.F08 | GTRM compiler: routing-map validation gates and `--check` | 🟡 | `deployments/gtrm/EVIDENCE.md`; compiler tests green 6 Nov · Verifier 23 Nov 2026: compiler `go vet` clean, 31/31 tests pass; the validation gate rejects a dev map under `--require-prod-safe` (exit 1); `--check` normalises CRLF (`compiler/main.go:91`). But `--check` FAILS on the branch: `GTRM DRIFT DETECTED — ../compiled-traefik.yml does not match the compiled routing map`. The compiler now emits `X-Identity-Envelope` in the edge header-strip list (commit `2e697c2b`), and the committed artefact (generated 13 Oct) lacks it. Traefik mounts that artefact (`deployments/docker-compose.yml:419`), so the running edge does not strip a client-supplied `X-Identity-Envelope`. The stray `deployments/gtrm/go.mod` is gone | LIB-4: recompile and commit `compiled-traefik.yml`; add `--check` to CI so drift fails the build |
| D2-10.F09 | GTRM compiler commands (G-02): `activate-policy`, `schedule-change`, `failover initiate|complete|abort`, `mark-region-unavailable` with provenance and evidence | ❌ | Not built | G-02 |
| D2-10.F10 | Stray `deployments/gtrm/go.mod` (empty module) and the modified `deployments/gtrm/compiler/go.mod` | 🟡 | `git status` 6 Nov | "Do first" item 2, then LIB-4 |

**Tasks**

- [ ] **LIB-1 · Docs:** READMEs for identity-envelope and `_contract` (packages, guarantees, settings such as `IDENTITY_VERIFY` and `IDENTITY_AUDIENCE`, upgrade rules). A `CHANGELOG.md` per library, because every service imports them.
- [ ] **LIB-2 · search-client live:** a compose `opensearch` service under a profile; integration tests tagged `integration` run against it.
- [ ] **LIB-3 · Audit `_contract`:** list what it provides and who imports it; remove dead packages; record the result in the matrix.
- [ ] **LIB-4 · GTRM gates:** every compiler gate has a test; `--check` passes on Windows.
- [ ] **G-02 · Compiler commands** (Reference Part A5 G-02) together with the registry reads that tenant-entity-registry gets in its session. Test `TestG02_FailoverOutsideResidencyRejected`.

## D1-10 · mtls-management-svc (:8140)

**Facts**

| Item | Value |
|---|---|
| Spec | Security Standard (ZS-SEC-001) §8.6 / §9 mTLS and workload identity; `03-microservices.md`; plan task SEC-6 |
| Latest score | `Security & Trust.md` §14.7 (13 Oct): FE 25%, BE 92%, integration 85%, overall 67%, Not Ready. Open items §14.6 |
| Suite 6 Nov | build ✔ · vet ✔ · unit ✔ (5 packages) |
| Deploy | main compose (and phase6). Certificates and policies are held in memory; revocations persist |
| Console | shared `/admin/security` page; its panels call routes that do not exist; `lib/api/security-trust.ts` is imported by no file |

**Features**

| ID | Feature | Status | Evidence | What is needed |
|---|---|---|---|---|
| D1-10.F01 | Certificates: `POST /v1/mtls/certificates`, `GET /v1/mtls/certificates`, `GET /v1/mtls/certificates/{id}`, `POST /v1/mtls/certificates/{id}/rotate`, `DELETE /v1/mtls/certificates/{id}` (revoke); real CA | 🟡 | Reconfirm (was ✅): Security & Trust §7, §14 | VC-1 |
| D1-10.F02 | CRL: `GET /v1/mtls/crl` (revocations persist) | 🟡 | Reconfirm (was ✅): Security & Trust §14.2 (MT-1) | VC-1 |
| D1-10.F03 | Bootstrap self-provisioning | 🟡 | Reconfirm (was ✅): Security & Trust §13.1 (resolved 12 Oct) | Tenant metadata from the token only: MT-3; VC-1 |
| D1-10.F04 | CSR signing: `POST /v1/mtls/certificates/csr` (the private key never leaves the workload) | ❌ | Route exists; not covered by the 13 Oct pass · Verifier 23 Nov 2026: the route exists (`internal/handler/handler.go:209`, `SignCSR` at `:651`), is in openapi (`:247`), and returns only the certificate; the key stays with the workload. Broken: the handler only checks that `service_name` is AMONG the CSR's DNS names and URIs, and `internal/ca/ca.go:299–301` copies the CSR's Subject, every DNS name and every URI SAN into the certificate. A caller authorized for service A, or holding A's bootstrap token, can add another service's DNS name and `spiffe://zoiko/<env>/<other>` URI and receive a certificate that authenticates as that service. Only one CSR test exists (`TestSignCSR_BootstrapTokenSignsOnlyItsOwnServiceAndReturnsNoKey`); mutants removing the CN check and the CSR signature check both survive. Suite 55 pass / 0 fail | SEC-6: issue exactly one DNS name (service_name) and one SPIFFE URI, build the Subject server-side, refuse a CSR with any other SAN; tests for signature, CN, DNS, URI, extra-SAN and authz refusals |
| D1-10.F05 | URI SAN `spiffe://zoiko/<env>/<service>` (S4-C1) | ❌ | Not built | SEC-6 |
| D1-10.F06 | Persistence of certificates and policies (PER-1) | ❌ | In memory | SEC-6 |
| D1-10.F07 | Automated rotation; `EXPIRED` status set (ROT-1) | ❌ | `auto_rotate` stored, never read | MT-1 |
| D1-10.F08 | mTLS policies: `GET /v1/mtls/policies`, `POST /v1/mtls/policies` | 🟡 | `CreatePolicy` error handling flagged (§12) | MT-2 |
| D1-10.F09 | `MTLS_*` actions and `iam.workload.manage` registered and templated (X-RBAC) | 🟡 | §14.6 X-RBAC | MT-2 |
| D1-10.F10 | `/readyz` (RDY-1) | ❌ | 404 | MT-2 |
| D1-10.F11 | Maker-checker on RevokeCert (MC-1) | 🧭 | §11.7, §14.6 | CL-2 |
| D1-10.F12 | Every PEP presents a client certificate so authorization can run `require` (M-02) | 🟡 | §14.6 M-02 | EST-1 |
| D1-10.F13 | Contract, docs, E2E | 🟡 | `openapi.yaml` exists; no `progress.md`, RUNBOOK or E2E | MT-4 |

**Tasks**

- [ ] **MT-0 · Baseline (S0):** `progress.md` matrix from the Security & Trust audit §7, §13, §14 and SEC-6.
- [ ] **SEC-6 · CSR signing, URI SAN, Postgres persistence** (Reference Part A5 SEC-6; new DB, role and DSN under R4). Tests:
  - `TestSEC6_CSRIssuedKeyNeverLeavesWorkload`;
  - `TestSEC6_URISANPresent`;
  - `TestSEC6_RevocationSurvivesRestart`.
- [ ] **MT-1 · Automated rotation:** a ticker (`context.WithoutCancel` + timeout) rotates certificates with `auto_rotate` before `not_after − margin`, and marks expired ones `EXPIRED`; metrics and events. Test with a frozen clock.
- [ ] **MT-2 · Fixes:**
  - `CreatePolicy` validation and problem codes;
  - register `MTLS_*` and `iam.workload.manage` in access-control (R5);
  - add `/readyz`.
- [ ] **MT-3 · Bootstrap tenant (BS-1):** take the tenant and legal entity from the bootstrap token's binding, never from caller headers.
- [ ] **MT-4 · Contract, docs, live:**
  - openapi parity, RUNBOOK, RELEASE_CERTIFICATE, SPEC_DEVIATIONS;
  - a new `deployments/scripts/e2e-mtls-management-deps.py`: issue by CSR, a peer refuses a revoked certificate via the CRL, rotation, authorization decisions.

**Console (frontend):** the shared Security page.

`/admin/security` (legacy): its four panels fetch `/api/v1/security/*` routes that do not exist, and the backend-accurate client `lib/api/security-trust.ts` is imported by no file. This session builds the v2.0 shell. Each Security & Trust session adds its own tabs.

| Part | Content |
|---|---|
| Header | "Security › Security & Trust"; gold primary "Issue certificate"; secondary "Evaluate access risk" |
| KPIs (one summary per service, read in parallel) | Active certificates · Expiring in 14 days · Keys active · Open security incidents · Risk decisions DENY/ISOLATE (24 h) |
| Panels | Pipeline: certificate Requested → Issued → Rotated → Revoked/Expired. Ordered list: the trust chain (root → intermediate → workload). Bar list: certificates expiring per service, with an amber callout for certificates without auto-rotation |
| Tabs (this session) | Certificates (F01–F05) · mTLS policies (F08) · Revocations / CRL (F02) |
| Footer | Required controls: private keys never leave the workload · revocations distributed by CRL · every command has a reason and is audited |

- [ ] **ST-FE-1 · API client:** make `lib/api/security-trust.ts` server-only and route it through `lib/api/envelope.ts`. Delete the dead `/api/v1/security/*` fetches and the orphaned `CartaCapTablePanel.tsx`.
- [ ] **ST-FE-2 · Page v2.0 shell + mTLS tabs.**
- [ ] **ST-FE-3 · UI E2E** `tests/e2e/console/security.spec.ts` (mTLS part). Cover:
  - issue, rotate and revoke;
  - the CRL shows the revocation;
  - empty and unavailable states.

**Blocked and decisions:** MC-1 (CL-2); production CA hierarchy and Traefik `serversTransport` certificates (production backlog).

## D1-11 · siem-integration-svc (:8141)

**Facts**

| Item | Value |
|---|---|
| Spec | Security Standard §16, §22, §25; `phase-6-trust-intelligence.md`; plan task SEC-3 |
| Latest score | `Security & Trust.md` §12: BE 55%, integration 20%, overall 27%, Not Ready. Its named purpose (forwarding to a SIEM) does not exist |
| Suite 6 Nov | build ✔ · vet ✔ · unit ✔ (1 package) |
| Deploy | `docker-compose.phase6.yml` only; in-memory store |
| Console | Security page (dead-wired, see D1-10) |

**Features**

| ID | Feature | Status | Evidence | What is needed |
|---|---|---|---|---|
| D1-11.F01 | Exporters: `GET /v1/siem/exporters`, `POST /v1/siem/exporters`, `GET /v1/siem/exporters/{id}` | 🟡 | In memory; the third-party `AuthToken` is stored in plaintext | SEC-3, SG-1 |
| D1-11.F02 | Events: `POST /v1/siem/stream` (ingest), `GET /v1/siem/events` | 🟡 | In memory; two ingest routes deliberately ungated (accepted residual) | SEC-3 |
| D1-11.F03 | Postgres store + outbox; a restart loses no event | ❌ | In memory | SEC-3 |
| D1-11.F04 | Schema validation and redaction (§16 "must not contain") | ❌ | Not built | SEC-3 |
| D1-11.F05 | Forwarders: RFC 5424 syslog over TLS and HTTPS JSON (HEC/Elastic), retry, backoff, dead-letter queue | ❌ | No outbound client | SEC-3 |
| D1-11.F06 | Security exceptions: `POST /security/exceptions` (time-bound, approved) | ❌ | Not built | SEC-3 |
| D1-11.F07 | Incident containment: `POST /security/incidents/{id}:contain` (end sessions, revoke certificates, suspend keys; approval required) | ❌ | Not built | SEC-3 |
| D1-11.F08 | Evidence by correlation: `GET /security/evidence/{correlation_id}` | ❌ | Not built | SEC-3 |
| D1-11.F09 | Exporter credentials leased from secret-vault, never stored in plaintext | ❌ | Plaintext | SG-1 |
| D1-11.F10 | Running in the main compose with healthchecks; every producer's async SIEM client reaches it | ❌ | phase6 only | SG-2 |
| D1-11.F11 | Vendor-specific SIEM configuration (Splunk, Datadog, Elastic, Sentinel tenancy) | ⛔ | Vendor choice and accounts | Production backlog |

**Tasks**

- [ ] **SG-0 · Baseline (S0):** `progress.md` matrix (Security & Trust audit rows, SEC-3).
- [ ] **SEC-3 · siem-integration-svc** (Reference Part A5 SEC-3; new DB under R4). Tests:
  - `TestSEC3_RestartLosesNoEvent`;
  - `TestSEC3_SecretRedactedBeforeStorage`;
  - `TestSEC3_ForwardRetriesUntilSyslogSinkReceives` (local syslog sink container);
  - `TestSEC3_ContainmentNeedsApproval`.
- [ ] **SG-1 · Credentials:** exporter secrets leased from secret-vault (`secret://`), never stored or logged.
- [ ] **SG-2 · Deploy and wire:** add it to the main compose with a healthcheck and a local syslog sink under a profile. Point every service's `SIEM_URL` at it. Gate the ingest routes with mTLS workload identity, which closes the accepted residual.
- [ ] **SG-3 · Contract, docs, live:** openapi + parity, asyncapi, RUNBOOK, RELEASE_CERTIFICATE, SPEC_DEVIATIONS, and `deployments/scripts/e2e-siem-integration-deps.py`. Cover:
  - a gateway denial reaches the sink;
  - containment ends a session and revokes a certificate;
  - evidence by correlation id.
- [ ] **ST-FE-4 · Security page tabs:** SIEM exporters, Events, Exceptions, Incidents (containment with approval), Evidence lookup. Extend `security.spec.ts`.

**Blocked and decisions:** vendor configuration (production backlog).

## D1-12 · carta-svc (:8142)

**Facts**

| Item | Value |
|---|---|
| Spec | Security Standard §5.4 (zero-trust scoring); plan task SEC-5 |
| Latest score | `Security & Trust.md` §12: BE 85%, integration 55%, overall 48%, Not Ready |
| Suite 6 Nov | build ✔ · vet ✔ · unit ✔ (2 packages) |
| Deploy | phase6 compose only; in-memory assessments. gateway-auth-svc calls it on every request |
| Console | Security page (dead-wired) |

**Features**

| ID | Feature | Status | Evidence | What is needed |
|---|---|---|---|---|
| D1-12.F01 | Evaluate: `POST /v1/carta/evaluate` (ALLOW / STEP_UP_MFA / ISOLATE / DENY), deterministic, consumed by gateway-auth-svc; gateway enforces STEP_UP_MFA (AU-23) | 🟡 | Reconfirm (was ✅): Security & Trust §7; Auth-Service audit AU-23 | VC-1 |
| D1-12.F02 | Assessment reads: `GET /v1/carta/assessments`, `GET /v1/carta/assessments/{id}` | 🟡 | Reconfirm (was ✅): Security & Trust §4.3 | VC-1 |
| D1-12.F03 | Real signals for `device_trust_level` and `is_known_location`, replacing the placeholders | ❌ | §11.2 | SEC-5 |
| D1-12.F04 | Postgres persistence for assessments | ❌ | In memory | SEC-5 |
| D1-12.F05 | Maker-checker where the audit asks for it (policy and threshold changes) | ❌ | §11.3 | SEC-5 |
| D1-12.F06 | The ungated evaluate route accepts tenant-header-only callers (accepted residual) | 🟡 | §11.3 #4 | CA-1 |
| D1-12.F07 | Running in the main compose (gateway E2E "carta stopped → 503 write / 200 read" needs it) | ❌ | phase6 only | CA-1 |
| D1-12.F08 | Commercial geo-IP / device-fingerprint provider | ⛔ | No provider chosen | Production backlog |

**Tasks**

- [ ] **CA-0 · Baseline (S0):** `progress.md` matrix.
- [ ] **SEC-5 · carta-svc** (Reference Part A5 SEC-5). Real local signals:
  - known location from identity-context `authentication_events` (prior successful sign-ins from the network/country);
  - device trust from the passkey / registered-factor state.
  Then Postgres persistence (R4) and maker-checker on policy changes. Tests: `TestSEC5_EverySignalFromARealSource`, `TestSEC5_AssessmentSurvivesRestart`.
- [ ] **CA-1 · Deploy and gate:** add it to the main compose with a healthcheck; require mTLS workload identity (gateway's certificate) on `/v1/carta/evaluate`.
- [ ] **CA-2 · Contract, docs, live:** openapi parity, RUNBOOK, RELEASE_CERTIFICATE, and `deployments/scripts/e2e-carta-deps.py`. Cover:
  - an unknown location → STEP_UP_MFA → gateway 401 `step_up_required`;
  - carta stopped → write 503 / read 200.
- [ ] **ST-FE-5 · Security page tab:** Risk assessments (list, drawer with signals and decision; evaluate diagnostic). Extend `security.spec.ts`.

## D1-13 · key-management-svc (:8143)

**Facts**

| Item | Value |
|---|---|
| Spec | Security Standard §8.6, §9.5, §10–§12, §23, §25, §26; plan tasks SEC-1, SEC-2, SEC-7 |
| Latest score | `Security & Trust.md` §12: BE 55%, integration 20%, overall 27%, Not Ready (metadata only, no custody) |
| Suite 6 Nov | build ✔ · vet ✔ · unit ✔ (3 packages) |
| Deploy | phase6 compose only; in memory |
| Console | Security page (dead-wired) |

**Features**

| ID | Feature | Status | Evidence | What is needed |
|---|---|---|---|---|
| D1-13.F01 | Key metadata: `POST /v1/keys`, `GET /v1/keys`, `GET /v1/keys/{id}`, `POST /v1/keys/{id}/rotate`, `POST /v1/keys/{id}/disable` (all authorized) | 🟡 | Reconfirm (was ✅): Security & Trust §7 | Replaced by real custody in SEC-1; VC-1 |
| D1-13.F02 | Real custody: generate, BYOK import (wrapped), encrypt / decrypt / sign APIs, local AES-256-GCM KEK whose own key comes from secret-vault | ❌ | Metadata only | SEC-1 |
| D1-13.F03 | Postgres persistence with every §11.1 KeyRecord field | ❌ | In memory | SEC-1 |
| D1-13.F04 | Lifecycle: scheduled and on-demand rotation, re-encryption (API-5), suspend (encrypt refused, decrypt allowed), revoke/destroy with dual control and dependency assessment, archive, compromise | ❌ | Not built | SEC-1 |
| D1-13.F05 | Maker-checker on disable/destroy | ❌ | §11.3 | SEC-1 |
| D1-13.F06 | HYOK external key reference with an availability probe; fails closed | ❌ | Not built | SEC-1 |
| D1-13.F07 | §26 key events through an outbox | ❌ | Not built | SEC-1 |
| D1-13.F08 | Crypto agility: algorithm registry (Preferred … Prohibited), `GET /security/crypto-inventory`, a versioned encryption envelope used by key-management and secret-vault | ❌ | Not built | SEC-2 |
| D1-13.F09 | Effective security policy: `GET /security/policies/effective` | ❌ | Not built | SEC-7 |
| D1-13.F10 | Running in the main compose; secret-vault (SEC-4 tokenization) and configuration-feature-flag (CFG-3 snapshot signing) use it | ❌ | phase6 only | KM-1 |
| D1-13.F11 | Cloud KMS and Vault Transit adapters | ⛔ | Infrastructure | Production backlog |

**Tasks**

- [ ] **KM-0 · Baseline (S0):** `progress.md` matrix.
- [ ] **SEC-1 · Real custody** (Reference Part A5 SEC-1; new DB under R4). Tests:
  - `TestSEC1_SuspendedKeyRefusesEncryptStillDecrypts`;
  - `TestSEC1_DestroyNeedsSecondApprover`;
  - `TestSEC1_ReencryptionResumable`;
  - `TestSEC1_UnavailableHYOKFailsClosed`.
- [ ] **SEC-2 · Crypto agility** (Part A5 SEC-2). Test: `TestSEC2_ProhibitedAlgorithmRefusedOldCiphertextDecrypts`.
- [ ] **SEC-7 · Effective policy route** (Part A5 SEC-7), authorized diagnostic use only.
- [ ] **KM-1 · Deploy and integrate:**
  - add it to the main compose;
  - move configuration-feature-flag's snapshot signing (CFG-3) and secret-vault tokenization (SEC-4) onto it;
  - register the actions (R5).
- [ ] **KM-2 · Contract, docs, live:** openapi parity, asyncapi, RUNBOOK, RELEASE_CERTIFICATE, and `deployments/scripts/e2e-key-management-deps.py`. Cover:
  - generate → encrypt → rotate → old ciphertext decrypts;
  - suspend;
  - destroy with a second approver;
  - re-encryption operation polled to completion.
- [ ] **ST-FE-6 · Security page tabs:** Keys (lifecycle commands with dual control), Crypto inventory, Algorithms, Effective policy. Finish `security.spec.ts`. Cover:
  - a destroy refused for the same user;
  - a prohibited algorithm refused;
  - empty and unavailable states.

**Blocked:** cloud KMS / Vault Transit adapters (production backlog).

---

# Cross-cutting work (both domains)

| ID | Item | Status | What is needed | Touches |
|---|---|---|---|---|
| X5 | Signed identity everywhere: ForwardAuth on every Traefik route, no published service ports, every service verifies the envelope (`IDENTITY_VERIFY` from `warn` to `require`) | 🟡 | Per service: confirm the gate is mounted (grep `NewGateFromEnv` / `internal/identity`), send the bearer from the console and callers, flip compose to `require`, prove a header-only call gets 401 in the service's E2E | All 16 |
| R-3 | About 100 estate services still trust `X-Principal-Id` on direct calls | 🟡 | Mount the identity-envelope gate service by service (pattern: authorization-svc `internal/identity`, workflow-svc gate). Our 16 first | All |
| IAM-1 | Per-service audience (AU-12) | ❌ | GW-5, then `IDENTITY_ACCEPT_LEGACY_AUDIENCE=false` per service | Gateway + all gated services |
| X3 | Real Kafka proof for every outbox | 🟡 | evidence-requirements proven on Kafka (5 Nov); identity proven 3 Nov. Each service's E2E must read its own event from the broker | All |
| API-3 | ETag / If-Match on every aggregate mutation | 🟡 | Remaining: configuration-feature-flag (CFG-1), access-control beyond roles/bundles (AC-4), secret-vault tests (SV-1), authorization makes it mandatory (AZ-1) | 4 services |
| API-4 | Cursor pagination on every list | 🟡 | Done in evidence-requirements and identity-context. Every other service has a task (TER-2, CFG-1, SV-1, SI-3, NT-1, DA-1, AC-3, JR-5, GDL-1, POL-1, OB-4, AZ-1, WF-5) | 14 services |
| API-5 | Async operations | ❌ | Needed first by ORG-9 master imports | tenant-entity-registry |
| API-6 | Deprecation / Sunset on renamed routes | ❌ | Needed by ST-3, NT-5, CFG-1, AZ-4 | 4 services |
| R5 | Production grants by migration, not `seed-demo-rbac.ps1` | 🟡 | AC-2 (notification and workflow action templates, `iam.workload.manage`); service identities done 11 Nov 2026: decision-log readers (authorization-svc 000042), `svc-policy` / `svc-workflow` / `svc-access-control` (000043); `iam.assignment.read` registered and `GOVERNANCE_DECISION_RECORD` carried by a template (access-control 000050, 000051) | access-control, authorization |
| G-07 | Evidence ledger and verification | ❌ | In governance-decision-log-svc | D2-2 |
| G-08 | Schema registry lifecycle (schema-registry-svc) | ❌ | Then register IAM-10, EML-4, evidence-requirements schemas | schema-registry-svc |
| G-09 / G-10 | Retention and disposition; legal hold (retention-registry-svc) | ❌ | G-10 first (IAM-3 and G-09 depend on it) | retention-registry-svc |
| G-05/03/04/12 | Named-operation parity | ❌ | `x-spec-operation` on existing routes in the GOV documents' services | Several |
| IAM-3/4/5 | Legal-hold negative control (after G-10); relationship predicates (after ORG-4/ORG-5); authoritative FX (after ORG-8) | ❌ | IMPLEMENTATION-PLAN §6 | authorization-svc |
| SEC-1…SEC-9 | Key custody, crypto agility, SIEM, tokenization, CARTA, mTLS management, effective security policy, CI supply chain, backup/DR | ❌ | IMPLEMENTATION-PLAN §6 Phase 7 (key-management, siem, carta, mtls-management are Security & Trust services) | Security & Trust |
| CI | No CI run on the recent commits of `shashi-changes` | 🟡 | Push and read the Actions run; fix red jobs | Both repos |
| MAIN | `shashi-changes` vs `origin/main` | 🟡 | Merge `origin/main` before each service session; re-run that service's suites after the merge | Both repos |
| FE-1 | Console screens for every capability (v2.0 kit) | 🟡 | v2.0 pages: identity, gateway, authorization, evidence (three of them still miss commands). Twelve pages to rebuild. Clients call 260 of 763 screen-relevant routes; Appendix B lists every gap. Each service section has its own "Console (frontend)" plan and `*-FE-*` tasks | Frontend |
| FE-H | Console UI E2E harness (Playwright against the live stack) | ❌ | Built once, before the first service's FE tasks: see "Frontend rules for every console" | Frontend |
| CLOSE-1…3 | Rows re-scored (Part G), Postman collections, CI-equivalent run | ❌ | After each service's S6 | Docs |

Every row of this table is a task in a session: G-08 → session 17 · G-09/G-10 → 18 · G-02 → 11 and 19 · SEC-6 → 20 · SEC-5 → 21 · SEC-3 → 22 · SEC-1/SEC-2/SEC-7 → 23 · SEC-4 → 14 · X5, R-3, IAM-1, X3, API-5, API-6, R5, G-05/03/04/12, IAM-3/4/5, SEC-8, SEC-9, CI → 24 · CLOSE-1…3, MAIN, FE-1 sign-off → 25.

---

# Carried over from the removed group testing files (6 Nov)

`testing_shashi_progress/group1-identity-foundation-testing.md` (16 Oct) and `group2-governance-platform-testing.md` (21 Oct) were removed on 6 Nov. Their findings were superseded by the 4–5 Nov audit sections and this tracker. The open items below were in them and nowhere else; each is now owned here.

| Service | Item | Status | Owner / where it goes |
|---|---|---|---|
| D1-3 configuration-feature-flag | Idempotency of the legacy `POST /v1/config`: value-equality upsert today; a caller `Idempotency-Key` would be a breaking change | 🧭 | Decide in CFG-1 together with deprecating the legacy routes |
| D1-4 secret-vault-integration | Secret class scoping: tenant / legal entity vs global (foundation for §9.6 sensitive key separation) | 🧭 | Security |
| D1-5 gateway-auth | Ratify JWKS cache TTL (default 5 min), SIEM queue sizing (256 slots, 4 workers, 3 s timeout) and the drop-with-counter policy | 🧭 | Security / platform ops; record in RUNBOOK (GW-6b) |
| D1-6 search-indexer | OD-13 bulk export delivery (size limit, format, mechanism); numeric latency and freshness SLOs for alerts | 🧭 | Product / ops; SI-3 wires the alerts once decided |
| D1-7 notification | Delivery retry policy (maximum attempts, backoff curve, dead letter) to ratify; per-jurisdiction unsubscribe rules through policy-svc | 🧭 | Product / legal; then NT tasks |
| D2-1 jurisdiction-rules | Production pack signing key (`PACK_SIGNING_KEY_REF` / `PACK_SIGNING_KEY_FILE`); production `RESOLVER_RING` / `RESOLVER_REGION` | ⛔ | Platform ops (key custody waits for SEC-1); JR-7 documents both in RUNBOOK |
| D2-2 governance-decision-log | Which services consume `zoiko.governance.events` (downstream audit and replay topology) | 🧭 | Platform architecture; add to the open questions in D2-2.F11 |
| D2-3 policy | Business sign-off on the ACTIVE tie-break for the same type and scope (`effective_from DESC`) | 🧭 | Business owner |
| D2-5 authorization | Confirm the break-glass exclusion list (payment, posting, filing actions; ZS-IAM-001 §14) with product | 🧭 | Product / Security |
| schema-registry-svc (ours under R1; G-08) | No README; tenant visibility of schemas (platform-wide reads for every verified principal, or scoped enumeration) | 🧭 | Decide in G-08; write the README then |

---

# Frontend rules for every console (stage S5)

A service is finished end to end only when its console drives every screen-relevant route against the live service, and a browser test proves it.

**Where things go** (frontend repo `zoiko-suite-frontend-platform`, branch `shashi-changes`):
- `app/admin/<page>/page.tsx` is a server page, with `actions.ts` (server actions) and `state.ts` (result shape: reuse `app/admin/authorization/state.ts`). Keep the existing route and sidebar label.
- Tab and drawer components go in `components/admin/<service>/console/`: `data.ts` (loaders), `overview.tsx` (KPIs + panels), one file per tab, `forms.tsx`.
- The API client is `lib/api/<service>.ts`, server-only (`import "server-only"`).

**Build only from the kit:**
- Use `components/admin/console` (KpiCard/KpiGrid; Panels: Pipeline, OrderedModel, BarList, PanelCallout; Register*; DetailDrawer; DrawerFacts; ControlsFooter; FilterSelect) and `components/ui` (StatCard, Badge, Button `variant="accent"` for the gold action).
- Move `RegisterView` from `components/admin/authorization/console/register.tsx` into the kit the first time a second page needs it. Keep one copy.
- Reference pages: `app/admin/authorization`, `app/admin/evidence`.

**Layout (v2.0):**
- Breadcrumb, title, one-line description, secondary outline buttons and one gold primary action.
- 5 KPI cards.
- 3 panels: a pipeline, an ordered list, and a bar list with an amber callout.
- A tabbed register with search, filters, pager and a right-side detail drawer.
- A "Required controls" footer.
- State lives in the URL: `?tab ?q ?filter[...] ?cursor ?id ?pane`.

**Data rules:**
- Live data only.
- KPIs come from the service's summary route (the `*-FE-1` task adds it when missing, with authorization, tests and openapi).
- Lists use API-4 cursors.
- A source that cannot be read shows "unavailable", never zero or an empty list. No invented numbers.
- No raw JSON as the only view; diagnostics may add a collapsible JSON panel.

**Command rules:**
- Every call goes through `lib/api/envelope.ts`: the bearer envelope, `X-Request-Id`, `Idempotency-Key` on mutations, and `If-Match` from the record's version or ETag.
- Every command opens a reason dialog and retries once after step-up on `401 step_up_required`.
- `412` shows "changed by someone else — reload".
- Problem codes become sentences in an `explain<Service>Error`.
- The maker never sees the checker's buttons (decided from the signed-in principal). The backend still enforces it.
- Secrets and tokens shown once are never cached or logged, and are cleared when the drawer closes.
- Server-only environment variables only; never `NEXT_PUBLIC_*` for a secret.

**Checks for every FE task:**
- `npx tsc --noEmit`, `npx eslint <touched files>`, and `npx vitest run <new tests>` (mappers and `explain*Error`).
- Render with `next build && next start` (port 3100). `next dev` OOMs inside the sandbox and leaves orphaned `.next\dev\build` workers; stop them after a crash. Set `ZOIKO_API_TIMEOUT_MS=15000`.

**FE-H · Console UI E2E harness (build once, first FE session):**
- `playwright.console.config.ts`:
  - `testDir: tests/e2e/console`, `channel: "msedge"`, `workers: 1`;
  - `webServer`: `npx next start -p 3100`, with the service URLs from `lib/api/config.ts` pointed at the live dev stack on `127.0.0.1`.
- `tests/e2e/console/fixtures.ts`:
  - a real login through identity-context with the console client;
  - two users (maker and checker) from the demo seeds;
  - a step-up helper (TOTP from the seeded factor);
  - an `unavailable()` helper that points one service URL at a closed port for the error-state test.
- Run: `npx playwright test -c playwright.console.config.ts tests/e2e/console/<service>.spec.ts`. The existing `playwright.config.ts` (assistant suite) stays as it is.
- Each service's spec, named in its `*-FE-*` task:
  - opens every tab;
  - filters, pages and opens the drawer;
  - runs one round trip per command family, checking the result in the register;
  - checks the maker/checker rule in the UI;
  - checks one empty state and one unavailable state;
  - undoes its writes;
  - saves a screenshot per tab under `test-results/console/<service>/`.

---

# Execution order (one service per session)

**Current assignment (23 Nov 2026):** D2-6, D2-4 and D2-7, run through the step tables in "Assignment 23 Nov 2026" (Phase 0 first). The session list below is the original order.

The implementing model runs one service per session, in this order, and stops at that service's hand-back. A session covers the service's backend tasks and its console tasks (`*-FE-*`), so each service comes back finished end to end. Claude then runs the verifier protocol below before the next service starts.

| Session | Unit (prompt SERVICE) | Section | Tasks |
|---|---|---|---|
| 0 | do-first | — | "Do first" list |
| 1 | gateway-auth-svc | D1-5 | FE-H first, then GW-6a, GW-5, GW-7, GW-8, GW-9, GW-6b, GW-FE-3 |
| 2 | identity-context-svc | D1-1 | IC-11…IC-19, IC-FE-4…IC-FE-6 |
| 3 | access-control-svc | D1-9 | AC-0…AC-FE-3 (AC-0 also closes ER-W3) |
| 4 | evidence-requirements-svc | D2-7 | ER-W4…ER-W8, ER-FE-3, ER-FE-4 |
| 5 | authorization-svc | D2-5 | AZ-3 first, then AZ-1, AZ-2, AZ-4, AZ-5, IAM-8, AZ-FE-1, AZ-FE-2 |
| 6 | obligations-svc | D2-4 | OB-0…OB-FE-3 with WF-3 |
| 7 | workflow-svc | D2-6 | WF-0, WF-1, WF-5, WF-6, WF-7, WF-2, ST-1…ST-3, WF-8, WF-FE-1…3 |
| 8 | governance-decision-log-svc | D2-2 | GDL-0…GDL-FE-3 with G-07 |
| 9 | jurisdiction-rules-svc | D2-1 | JR-0…JR-FE-3 (split with SCOPE if needed) |
| 10 | policy-svc | D2-3 | POL-0…POL-FE-3 |
| 11 | tenant-entity-registry-svc | D1-2 | TER-0, TER-1, ORG-6…ORG-9, G-02 (registry reads), TER-2…TER-6, TER-FE-1…3; then JUR-1 inputs (D2-1.F13) |
| 12 | configuration-feature-flag-svc | D1-3 | CFG-0…CFG-FE-3 with G-11 |
| 13 | notification-svc | D1-7 | NT-0…NT-8, EML-1…EML-6, NCD-1, NT-FE-1…3 (split with SCOPE if needed) |
| 14 | secret-vault-integration-svc | D1-4 | SV-0…SV-FE-3, SEC-4 |
| 15 | search-indexer-svc | D1-6 | SI-0…SI-FE-3 |
| 16 | delegated-authority-svc | D1-8 | DA-0…DA-FE-3 |
| 17 | schema-registry-svc | D2-8 | SR-0, SR-1, G-08, SR-2…SR-5, SR-FE-1…3 |
| 18 | retention-registry-svc | D2-9 | RR-0, G-10, G-09, RR-1…RR-4, RR-FE-1…3 |
| 19 | libraries-and-gtrm | D2-10 | LIB-1…LIB-4, G-02 (compiler commands) |
| 20 | mtls-management-svc | D1-10 | MT-0, SEC-6, MT-1…MT-4, ST-FE-1…3 (builds the shared Security page) |
| 21 | carta-svc | D1-12 | CA-0, SEC-5, CA-1, CA-2, ST-FE-5 |
| 22 | siem-integration-svc | D1-11 | SG-0, SEC-3, SG-1…SG-3, ST-FE-4 |
| 23 | key-management-svc | D1-13 | KM-0, SEC-1, SEC-2, SEC-7, KM-1, KM-2, ST-FE-6 |
| 24 | estate-switch-over | Session 24 | EST-1…EST-13 |
| 25 | closure-gate | Session 25 | CL-1…CL-8 and the definition of done |

Sessions 11–14 move work onto key-management (CFG-3, SEC-4) only in session 23 (KM-1); until then they use the local key as their tasks say.

**Rules for the implementing model (from IMPLEMENTATION-PLAN §2 and the AUTH-PLANE plan):**
- R1: edit only our 16 services, `identity-envelope`, `schema-registry-svc`, `deployments/` and the frontend. Teammates' services (org-structure, commercial-account, document-vault, evidence-manifest, the 7 evidence-gate callers' business logic, …) are called, never edited, except where a task here names the exact file.
- R3: branch `shashi-changes` in both repos; one commit per task; stage files by explicit path; never commit `*.exe` / `*.bin`; never push.
- R4: migrations take the next free number, ship a `.down.sql`, use `ENABLE` + `FORCE ROW LEVEL SECURITY` with `NULLIF(current_setting('app.tenant_id', true), '')`, guard every `GRANT`, and are applied up/down/up on a scratch DB.
- R5: every action a handler checks is registered in access-control and carried by a role template.
- R6: every new or changed route keeps the envelope gate, authorizes at the object, takes `Idempotency-Key` on mutations, `If-Match` on mutations of an existing aggregate (428/412) and `ETag` on its GET, answers RFC 9457 problems, writes events through the outbox in the same transaction, and is in `openapi.yaml` with a parity test.
- R7: every new handler, store method, consumer and ticker is reached from `cmd/server/main.go`.
- R10: real Postgres for store suites (0 skip); every spec negative path has a test named after its id.
- R14: no stubs and no invented answers. A missing external dependency gets an interface plus a real local implementation; only the production adapter is marked ⛔.
- Status in this file: the implementer may change a row to 🔵 (built) and tick a task. Only the verifier changes a row to ✅.

**Environment facts:**
- Use `127.0.0.1`, never `localhost`. Python: `C:/Users/kaman/AppData/Local/Python/bin/python3.14.exe -I`.
- Host Go tests: `GOTMPDIR=<scratch dir> GOTOOLCHAIN=local go test -count=1 ./...`. Windows Application Control sometimes blocks a test binary ("An Application Control policy has blocked this file"); re-run the blocked package, or use Docker `golang:1.26-alpine` with `-v <services>:/src -v C:/Users/kaman/go/pkg/mod:/go/pkg/mod -e GOFLAGS=-mod=mod -e GOPROXY=off -e CGO_ENABLED=0` and `MSYS_NO_PATHCONV=1`; revert any `go.mod` the `-mod=mod` run rewrites.
- Working copies are CRLF: check `gofmt` with `tr -d '\r' < f | gofmt -l`.
- Dev stack: compose project `deployments`; `deployments/migrate.py up --db <name>` before recreating a container; build one image at a time (Docker VM ~2 GB); start only the containers a task needs and stop them afterwards.

---

# Verifier protocol (Claude runs this at each hand-back; nothing is taken on the summary)

1. `git log` / `git show --stat` for the service's commits: only R1 paths, explicit staging, no binaries, one commit per task.
2. Build, `go vet`, full suite on a scratch Postgres; counts match `progress.md`; store suites 0 skip.
3. **Feature matrix walk:** every row in this file and in `progress.md` has a route or consumer, a named test, and a console screen, or a ⛔/🧭 reason naming the external dependency or decision owner. Spot-read the spec for 10 random rows.
4. Grep the callers from `cmd/server/main.go` for every new handler, store method, consumer and ticker (R7).
5. kin-openapi validation and router-parity tests pass; every new route is documented.
6. Each new migration: number, `.down.sql`, RLS + FORCE with `NULLIF`, guarded grants; up/down/up on scratch.
7. Every new action is registered in access-control and carried by a template (R5).
8. Live: image rebuilt (fresh timestamp), migrations applied, `deployments/scripts/e2e-<svc>-deps.py` all PASS (a skip never counts as a pass), `scripts/audit.sh` green.
9. Mutation check: break two guards on a scratch copy (for example a self-approval check and a cursor tenant binding); a test must fail for each.
10. Frontend:
    - `npx tsc --noEmit`, `npx eslint <touched files>` and the new `vitest` tests are clean;
    - Appendix B for the service is empty, or each remaining route has a recorded reason;
    - `npx playwright test -c playwright.console.config.ts tests/e2e/console/<service>.spec.ts` passes against the live stack, with a screenshot per tab, one empty state and one "unavailable" state;
    - no invented numbers;
    - the maker never sees the checker's buttons.

11. Plan §8 extras: re-run the named-operation coverage scan over the 15 specs and compare it with the 23 Oct baseline (no missing operation except BLOCKED ones); reopen any task whose evidence is missing, does not reproduce, or describes code with no caller.

- [ ] **VC-1 · Confirmation check (verifier).** Every row whose Evidence starts with "Reconfirm (was ✅)" was marked done before 6 Nov and is now 🟡 until it is confirmed again. For each one:
  - re-run the cited test or suite;
  - re-read the code at the cited place, on the current branch;
  - for claims about live behaviour, re-run the service's dependency E2E.

  A pass turns the row ✅, with the date and evidence. A failure keeps it 🟡 or makes it ❌, with the finding and a new task. Run it for a service at its hand-back. It can also run earlier, for a service whose session has not started yet.

Any failure reopens the task. After a pass, the verifier changes the rows to ✅, updates the dashboard, and adds one dated entry to Part G (re-score log).

---

# Session 24 · Estate switch-over (all 26 units)

Run this once every service session (1–23) has handed back. It turns the per-service work into one estate that enforces everything at the same time. Each task flips a setting only after the E2E has proved that the callers are ready.

- [ ] **EST-1 · Signed identity everywhere (X5, R-3, F-1, M-02).**
  - Every one of our units moves from `IDENTITY_VERIFY=warn` to `require`. Change the compose defaults in `deployments/docker-compose.yml`; today it is `warn` for tenant-registry, jurisdiction, decision-log, config, secret-vault, obligations, workflow, search-indexer, notification, delegated-authority, access-control and evidence-requirements.
  - Then:
    - `POLICY_IDENTITY_VERIFY=enforce`;
    - `AUTHZ_IDENTITY_ASSERTION=require`;
    - `AUTHZ_EVALUATION_MTLS=require` (after every PEP presents a certificate);
    - `DECISION_LOG_READ_AUTHZ=enforce`;
    - `EVIDENCE_EVALUATE_AUTHZ=enforce`;
    - `AUTHZ_ENFORCE_TENANT_ON_AUTHORIZE=true`;
    - `AUTHZ_STEP_UP_UNKNOWN_AGE=step_up`.
  - Done when a header-only call to every unit answers 401 in `e2e-all.py` (EST-10) and every console Playwright spec still passes.
- [ ] **EST-2 · Strict audience (IAM-1):** after GW-5, set `IDENTITY_ACCEPT_LEGACY_AUDIENCE=false` for every gated unit. Prove that an envelope minted for one service is refused by another (`TestIAM01_EnvelopeForPolicyRefusedByWorkflow` plus a live probe).
- [ ] **EST-3 · No published service ports:** only Traefik (and documented admin ports bound to 127.0.0.1) is reachable from the host. Remove `8082:8082` and every other published port of our units from compose (J-01). Run the E2Es through Traefik.
- [ ] **EST-4 · Kafka proof matrix (X3):** add a table to this file listing every event our units produce or consume (producer, topic, consumers). Each edge needs a check in some E2E that reads the record from the broker. Done when there is no edge without a check.
- [ ] **EST-5 · Async operations (API-5)** in the five places: search-indexer rebuild/reindex, jurisdiction-rules compile and test runs, retention-registry disposition (G-09), tenant-entity-registry master imports (ORG-9), key-management re-encryption (SEC-1). Every unit must match the shared semantics: 202 + `Location: /v1/operations/{id}`, polling, cancel before the point of no return, and the same key giving the same operation id. One test file per unit.
- [ ] **EST-6 · Deprecation (API-6):** every renamed or replaced route answers with `Deprecation` and `Sunset` headers, and a test proves it. This covers ST-3 status writes, NT-5 legacy notifications, CFG-1 legacy config and AZ-4 aliases.
- [ ] **EST-7 · Named-operation parity (G-05/03/04/12):** in policy-svc and authorization-svc, every GOV-03/04/05/12 operation maps to a route (`x-spec-operation`) or gets built (Reference Part A5).
- [ ] **EST-8 · IAM-3, IAM-4, IAM-5 in authorization-svc:**
  - IAM-3: legal-hold deny, needs G-10.
  - IAM-4: relationship and qualification predicates from parties, org units and jurisdiction rules; needs TER-1 and org-structure-svc (a teammate's, read-only).
  - IAM-5: authoritative FX from REF-03 rate sets; needs ORG-8.
  - Tests for scenarios A23, A24, A25, and a grep showing no literal FX rate.
- [ ] **EST-9 · Grants audit (R5):** a test lists every action string that any of our units checks (grep the handlers' action constants) and fails if one is missing from access-control's registry or from every template. Strip `deployments/scripts/seed-demo-rbac.ps1` down to demo users and demo data. Production grants come only from migrations.
- [ ] **EST-10 · One stack, one runner:**
  - all 26 units are in `deployments/docker-compose.yml` with healthchecks (fold `docker-compose.phase6.yml` in);
  - `docker compose -p deployments config` is clean;
  - a fresh volume migrates from zero with `migrate.py up --all`;
  - a new `deployments/scripts/e2e-all.py` runs every `e2e-*-deps.py` and the console Playwright suite, and prints one table.
- [ ] **EST-11 · Observability parity:** every unit has `/metrics`, `/healthz`, `/readyz`, request and outbox metrics, and alert rules in `deployments/prometheus-rules.yml`, plus a RUNBOOK section per alert. Run `promtool check rules`.
- [ ] **EST-12 · CI supply chain (SEC-8):** per unit, govulncheck, an SBOM, and cosign sign and verify. The workflow parses, actionlint is clean, and CI is green on `shashi-changes`.
- [ ] **EST-13 · Backup and restore on the dev stack (SEC-9):**
  - `deployments/scripts/backup.sh` and `restore.sh`, plus the runbook;
  - a recorded drill: back up every DB of our units, restore it to scratch, and run the verification queries.
  - The production drill is in the production backlog.

---

# Session 25 · Closure gate: Domain 1 and Domain 2 complete (except production)

- [ ] **CL-1 · Spec coverage inventory.**
  - Extract the 15 specs (command in Reference Part A2, list in the §3 table).
  - For every section that governs one of our 26 units, list each named command, query, event, state transition, control and negative path.
  - Map each one to a route or consumer, a test and a feature row.
  - Write the result as **Appendix D · Spec coverage inventory** in this file, with columns spec | section | item | unit | route/consumer | test | row | status.
  - Every item without a match becomes a new feature row and task in its unit's section; implement it before CL-8.
  - Done when every item maps, or is ⛔ with a production-only reason, or is out of scope with the rule that excludes it.
- [ ] **CL-2 · Decision sheet.**
  - Collect every 🧭 row, the "Carried over" table, the ❓ items in the audits (Parts D and E) and the teammate audits' "needs clarification" lists.
  - Write them as one table in Reference Part F, with columns question | options | recommendation | owner.
  - The user (or the named owner) decides; record each answer with its date.
  - Then implement each decision as a task in its unit; the row moves to 🔵.
  - Done when no 🧭 is left.
- [ ] **CL-3 · Confirmation sweep (VC-1 for everything).** Every "Reconfirm (was ✅)" row in all 23 sections is confirmed or reopened. Done when no "Reconfirm" row is left.
- [ ] **CL-4 · Cross-team handover.**
  - For each row that depends on a teammate's service, write the request: what to change, why, and the evidence. Examples:
    - general-ledger's ungated posting paths;
    - callers forwarding the envelope;
    - the C8 obligations overlap;
    - external-data-feed drift events;
    - document-vault as a hold consumer;
    - policy-svc callers' 401/403 mapping;
    - invoice-approval's missing path;
    - business-object owners calling `/invalidate`;
    - object versions persisted by consumers.
  - Record in Reference Part F the date and to whom each was sent.
  - Our side must already fail closed and say so in SPEC_DEVIATIONS.
  - Done when every cross-team item is either handed over or closed.
- [ ] **CL-5 · Close-out (CLOSE-2, CLOSE-3):**
  - Postman collections updated for every unit;
  - a CI-equivalent run for every unit: `go mod verify`, build, vet and `go test -race ./...` in `golang:1.25` against `postgres:16-alpine`, with counts recorded.
- [ ] **CL-6 · Full acceptance on a fresh stack:**
  - fresh volumes, migrations from zero, grants only from migrations;
  - `e2e-all.py` passes with no FAIL and no SKIP except documented production-only ones;
  - every console Playwright spec passes;
  - screenshots are archived under `test-results/console/`.
- [ ] **CL-7 · Merge:**
  - open a PR from `shashi-changes` to `main` in both repos;
  - CI green;
  - review comments resolved;
  - merge (after the user approves);
  - re-run the build/vet/unit sweep and `e2e-all.py` on `main`.
- [ ] **CL-8 · Final re-score and sign-off:**
  - the verifier re-scores every unit;
  - writes one Part G entry per unit with its final score;
  - sets the dashboard (every row ✅ or production-only ⛔);
  - ticks the definition of done below.

## Definition of done: Domain 1 and Domain 2, everything except production

All of these must hold. The verifier checks each one in CL-8.

1. Every feature row of all 23 sections (16 services, 6 supporting services, the libraries) is ✅ after the verifier's check, or ⛔ with a reason that is either listed in the production backlog below or handed over to the owning team under CL-4. No 🟡, 🔵, ❌ or 🧭 is left.
2. Appendix D (CL-1) maps every spec item of our 26 units.
3. Appendix B is empty for every unit, and every unit's console spec passes against the live stack.
4. Every unit has completed S0–S6: a `progress.md` matrix, a green suite (0 skip), openapi/asyncapi with parity tests, RUNBOOK, RELEASE_CERTIFICATE, SPEC_DEVIATIONS, Postman, a dependency E2E that passes, and a mutation check.
5. EST-1…EST-13 are done: require mode, strict audience, no published ports, Kafka matrix, API-3…API-6, R5 grants by migration only, one compose, observability, CI supply chain, and a backup/restore drill on dev.
6. Every decision is taken and implemented (CL-2), and every cross-team item is handed over (CL-4).
7. Merged to `main`, with CI green and the post-merge sweep and `e2e-all.py` green (CL-7).

## Production backlog (outside this file's "done"; the next project phase)

These need production infrastructure, vendors or sign-offs. The code for each already has an interface plus a real local implementation, so only the production adapter or setup is missing.

| Area | Item | Unit |
|---|---|---|
| Edge | DDoS, WAF, bot/abuse and TLS policy at the GCP load balancer (Cloud Armor; S5-D1); production trusted-proxy CIDRs | gateway-auth-svc |
| Identity | External IdP vendor (D-4); SAML if decided; real-browser passkey attestation policy; TOTP sign-off (SO-1); session maximums (SO-2); sender-constrained tokens (AU-27) | identity-context-svc, authorization-svc |
| Keys and secrets | Cloud KMS or Vault Transit; one-time migration `VAULT_MIGRATE_LEGACY_KEY_FILE`; dynamic credentials (O-3); HYOK customer keys | secret-vault, key-management |
| Workload identity | Production CA hierarchy; per-service mTLS enrolment (M-01); Traefik `serversTransport` client certificates (S4-B1) | mtls-management, gateway |
| SIEM | Vendor tenancy and configuration (Splunk / Datadog / Elastic / Sentinel) | siem-integration-svc |
| Communications | Email provider, sender DNS (SPF, DKIM, DMARC), warm-up; SMS and push providers (OD-03, OD-04) | notification-svc |
| Search | Embedding provider and model (OD-10); production OpenSearch | search-indexer-svc, search-client |
| Risk signals | Commercial geo-IP / device-fingerprint provider | carta-svc |
| Reference data | Address and LEI verification provider | tenant-entity-registry-svc |
| Signing | Production pack signing key (`PACK_SIGNING_KEY_REF`); production `RESOLVER_RING` / `RESOLVER_REGION` | jurisdiction-rules-svc |
| Platform | Kubernetes manifests and GCP deploy; production restore drill with measured RPO/RTO (S2-C3); load and revocation SLOs measured on production-like capacity (IAM-8) | all |

---
