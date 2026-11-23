# Governance Decision Log Service — Progress Log

Status tracker for this service. Update this file whenever meaningful
work happens (spec changes, decisions made, code milestones). Keep
entries dated and short — this is a log, not a design doc (see
CONTEXT.md for the spec/context).

## Current status

**11 Nov 2026 (evening) — every open item closed and verified live
(uncommitted).** The three decision-log readers call :8450 over mTLS (F07);
the F11 decisions are recorded and built; the :8450 listener rotates its
certificate and checks the CRL; sealing runs on a schedule. Suite 213 pass /
0 fail / 0 skip (store suite on Postgres); dependency e2e 121/121 in
`IDENTITY_VERIFY=require`; scheduled-seal e2e 4/4; `scripts/audit.sh` 43/0;
4 mutants killed. Next: commit, then an independent VC-1 pass on the rows this
session changed (see "Next steps").

## S0 Baseline Matrix

| Feature Row | Route / Consumer | Test | Console | Status |
|-------------|------------------|------|---------|--------|
| D2-2.F01 | POST /v1/decisions (+ register fields, X-Causation-Id) | TestCreateDecision_*, TestCreateDecision_RegisterFieldsNormalisedStoredAndEvented, e2e "record:*" | New Decision, Record Outcome | 🔵 Built + live |
| D2-2.F02 | GET /v1/decisions (facets, q, total_count, offset+cursor), GET /v1/decisions/{id}, GET /v1/decisions/summary | TestListDecisions_*, TestList_RegisterFacetsTotalsSearchAndCursor, TestSummary_CountsWindowsAndFacets | KPIs, panels, register, drawer | 🔵 Built + live |
| D2-2.F03 | POST …/{id}/replay, GET …/{id}/replay-manifests, GET /v1/replay-manifests | TestReplayDecision_*, TestReplayDecision_ReadsPolicySvcActionContext, TestListReplayManifests_TenantWideWithDecisionReference, e2e "replay:*" | Replays tab, drawer Evidence pane | 🔵 Built + live (policy-svc) |
| D2-2.F04 | Outbox → Kafka `zoiko.governance.events` | TestOutbox*, e2e "kafka:*" | Audit Trail pane | 🔵 Proven against the local broker |
| D2-2.F05 | jurisdiction_context, width guards, RFC 9457 | TestCreateDecision_400_* | — | 🔵 |
| D2-2.F07 | Readers on mTLS: contract-lifecycle-svc (svc-contract-lifecycle), decision-support-svc (svc-decision-support), evidence-manifest-svc (signed-in user) | governancelog client tests (3 services), e2e "F07:*" + "F07 live:*" (contract approve/deny, recommendation from precedent, manifest snapshot) | — | 🔵 Built + live |
| D2-2.F06 | Read authorization (GOVERNANCE_DECISION_READ), compose default enforce | TestReadRoutes_401_403_503, e2e "read authz enforced" | all reads | 🔵 enforce |
| D2-2.F08 | Identity: compose `IDENTITY_VERIFY=require`; envelope (JWKS) or peer workload over mTLS :8450 (IDENTITY_PEERS); authz calls over mTLS; certificate rotation + CRL check | TestProvision_*, TestServerRequiresAVerifiedClientCertificate, TestRotation_*, TestRevocation_*, mtlsclient tests; e2e 121/121 in require (revoked peer refused) | console sends the envelope | 🔵 Built + live (require) |
| D2-2.F09 | G-07: hash chain (DB trigger), verify, seal (RFC 6962 Merkle), package verify, export bundle; scheduled sealing (EVIDENCE_SEAL_INTERVAL) | ledger tests (RFC 6962 vectors), TestLedger_*, TestSealer_*, TestRunOnce_*, e2e "offline:*", --scheduled-seal 4/4 | Evidence chain tab | 🔵 Built + live |
| D2-2.F10 | workflow-svc ledger relay over mTLS as svc-workflow | e2e "workflow relay: *" (live, require mode) | Register (workflow filter) | 🔵 Built + live |
| D2-2.F11 | Decisions 11 Nov: EV-REGULATED / COMPLIANCE_EVIDENCE; `?correlation=` (audit); decision_id = evidence_id; GOV-07 owns; purpose on reads; C3 canonical names alongside | TestList_AuditDimensionIsTheCorrelationID, TestC3_*, TestOutbox_WireAndCanonicalNamesQueuedTogether, e2e "F11:*", "C3/R9:*" | — | 🔵 Built + live |
| D2-2.F12 | Cursor paging (API-4) | TestList_RegisterFacetsTotalsSearchAndCursor | — | 🔵 |
| D2-2.F13 | Dependency E2E + console v2.0 + UI E2E | deployments/scripts/e2e-governance-decision-log-deps.py (121/121, 11 Nov evening), tests/e2e/console/governance.spec.ts (5/5, 11 Nov), scripts/audit.sh (43/0) | /admin/governance | 🔵 |

## Roadmap — three phases, three branches, three PRs into `main`

Each phase branches off `main` fresh (not off the previous phase's
branch) — phase N+1 doesn't start until phase N's PR has merged.
Every phase's "done" bar includes a real-Postgres (and, for phase 3,
real-Docker) end-to-end check — `go build` passing is never sufficient
on its own. Full technical detail for each phase is in CONTEXT.md
("Build plan — three phases, three branches, three PRs into main").

| Phase | Branch (suggested) | Scope | Status |
| --- | --- | --- | --- |
| 1 | `feat/governance-decision-log-svc-write-path` | New service scaffold (cmd/config/handler/store/domain/health), migration, `POST /v1/decisions`, idempotent on `decision_id`, real `PgStore` wired into `main.go` | **Merged to `main`** (PR #14) |
| 2 | `feat/governance-decision-log-svc-query-surface` (actually landed on `shashi-changes`) | `GET /v1/decisions/{id}` + `GET /v1/decisions` with all 5 filters (actor, entity, action, rule_basis, time range), composable; handler unit tests + real Postgres integration tests | **Pushed, PR pending** |
| 3 | `feat/governance-decision-log-svc-close-loop` (actually landed on `shashi-changes`) | Publish `governance.decision.recorded` (stub-Kafka convention), add service to CI matrix + `TEST_DATABASE_URL` condition, Dockerfile, `services/README.md` entry | **Pushed, PR pending** |

## Log

### 2026-07-06 — Spec drafted
- Assembled initial service spec into `CONTEXT.md` from
  `docs/architecture/01-backend.md`, `03-microservices.md` §8.7, and
  `04-data-model.md` §7.1–7.3.
- Flagged an open cross-service design question: whether writes to this
  service should be fail-closed or fail-safe-async from the caller's
  perspective.

### 2026-07-06 — Spec finalized against concrete build briefs
- Received concrete, code-referencing task briefs for all three phases
  (write path, query surface, close-the-loop). Reconciled them against
  the draft spec and the canonical `04-data-model.md` entity:
  - Adopted the simplified MVP schema (`decision_id`, `tenant_id`,
    `legal_entity_id`, `actor_id`, `action_type`, `outcome`,
    `rule_basis`, `evaluation_context` JSONB, `correlation_id`,
    `decided_at`) as a deliberate deviation from the full
    `GovernanceDecision` entity — recorded the field mapping in
    CONTEXT.md so it isn't mistaken for an oversight later.
  - Resolved the idempotency-key question: `decision_id`, mirroring
    `audit-event-store-svc`'s `event_id` / `ON CONFLICT DO NOTHING`
    pattern exactly.
  - Resolved the fail-closed-vs-async open question as **out of scope
    for this service** — it's a decision for Authorization Service (the
    caller), not for the Decision Log Service's own contract.
  - Finalized config (`PORT=8083`, confirmed unused — 8080/8081/8082
    are already taken by the other three services), pgxpool Tier 0
    sizing, and the event-envelope approach (mirror existing publishers'
    envelope shape exactly, carry §19's required fields inside
    `payload` rather than inventing new envelope fields).
  - Confirmed `services/audit-event-store-svc` has no Dockerfile yet as
    of this writing — Phase 3 will need to write one from scratch
    unless a teammate ships one first (re-check at that time).
  - Read-API auth model question (who may call `GET /v1/decisions`) is
    still open but not blocking — none of the three briefs require an
    answer yet since no Authorization Service integration exists to
    enforce it against. Revisit before this service is exposed outside
    trusted internal callers.
- No code written yet — this was an analysis/recording pass only.

### 2026-07-06 — Phase 1 (write path) built and verified
- Scaffolded full service: `cmd/server/main.go`, `internal/{config,domain,store,handler,health}`,
  migration `000001_initial_schema.{up,down}.sql`, `go.mod`/`go.sum`.
- `POST /v1/decisions`: 201 on first insert, 200 (no overwrite) on replay
  of the same `decision_id`, 400 on missing field/bad JSON, 503 on store
  failure. `GET` endpoints deferred to Phase 2 as planned; `store.FindByID`
  exists internally already for Phase 2 to reuse.
- 5 handler unit tests (mocked store) + 3 Postgres integration tests
  (env-guarded via `TEST_DATABASE_URL`) — all passing.
- No local Go toolchain on this machine — build/vet/test all run inside
  `golang:1.25` Docker containers instead (same approach CI uses).
- Verified live end-to-end: booted the real binary against a real
  `postgres:16-alpine` container, confirmed `/healthz` + `/readyz` return
  200, POSTed a decision (201), re-POSTed the same `decision_id` with a
  different payload (200, not 201), confirmed via direct SQL query exactly
  one row exists with the **original** outcome preserved (no overwrite).
  Test containers torn down after verification.
- Not yet committed/pushed — sitting as uncommitted files pending review.

### 2026-07-06 — Manual Postman smoke test against `gdl-test-server`
- Re-verified Phase 1 via Postman against the `gdl-test-server` container
  (`go run ./cmd/server`, source bind-mounted, backed by `gdl-test-postgres`):
  `GET /healthz` → 200, `POST /v1/decisions` → 201 first time / 200 on
  replay of the same `decision_id` (idempotency confirmed live, not just
  in tests).
- Hit a stale-process gotcha worth remembering: `/healthz` initially
  returned Go's default 404 because the container's `go run` process had
  been started before `/healthz` was added to `main.go` — `go run`
  compiles once at container startup and does not hot-reload on source
  changes. Fixed with `docker restart gdl-test-server`. **Any future code
  change to this service requires a container restart to take effect.**

### 2026-07-07 — Phase 2 (query surface) built and verified
- Branched `feat/governance-decision-log-svc-query-surface` off updated
  `main` (post Phase-1 merge, PR #14).
- Added `store.PgStore.List` (all 5 filters — `actor_id`, `legal_entity_id`,
  `action_type`, `rule_basis`, `decided_at` time range — compose with AND,
  default limit 50/max 200, newest-first), refactored `FindByID` to share
  a `decisionColumns`/`scanDecision` helper with `List`.
- Added `GET /v1/decisions/{decision_id}` (200/404/503) and
  `GET /v1/decisions?actor=&entity=&action=&rule_basis=&from=&to=&limit=&offset=`
  (200 with all filters composable, 400 on invalid `from`/`to`, 503 on
  store failure) to the handler, mirroring `jurisdiction-rules-svc`'s
  `ListJurisdictions`/`GetRules` filter-parsing pattern.
- 8 new handler unit tests (mocked store) + 8 new Postgres integration
  tests (one per filter in isolation, one composing three filters
  together, one asserting empty-not-error on no match) — all passing,
  run inside `golang:1.25` (still no local Go toolchain).
- Verified live end-to-end: booted the real binary against a real
  `postgres:16-alpine` container, POSTed two decisions, confirmed
  `GET /v1/decisions/{id}` returns 200 for a known id and 404 for an
  unknown one, confirmed unfiltered `GET /v1/decisions` returns all rows,
  confirmed single-filter and composed multi-filter (`entity` +
  `rule_basis`) queries narrow correctly with AND semantics, confirmed
  `from` time-range filtering, and confirmed `from=<garbage>` returns 400.
  Test containers torn down after verification.
- Committed to `feat/governance-decision-log-svc-query-surface`
  (`238a2b2`), separate from the manual Postman pass below.

### 2026-07-07 — Manual Postman smoke test of Phase 2, committed to `shashi-changes`
- Re-verified Phase 2 live via Postman against a fresh `gdl-test-server`
  + `gdl-test-postgres` pair: `POST /v1/decisions` → 201, `GET
  /v1/decisions/{decision_id}` → 200 for the created id, 404 for an
  unknown id, `GET /v1/decisions` (unfiltered and filtered) → 200 with
  correct results. Test containers torn down after verification.
- Cherry-picked the Phase 2 commit (`238a2b2`) from
  `feat/governance-decision-log-svc-query-surface` onto `shashi-changes`
  (now `2092b32`) to match Phase 1's actual landing branch, and pushed.
- PR into `main` not yet opened — title/description/URL handed off for
  manual creation (`gh` CLI is unauthenticated on this machine).

### 2026-07-07 — Phase 3 (close the loop) built and verified
- Added `internal/events/publisher.go`: `Publisher.PublishDecisionRecorded`
  emits `governance.decision.recorded` (stub — logs the full envelope,
  does not write to Kafka yet), mirroring `identity-context-svc`'s and
  `tenant-entity-registry-svc`'s envelope shape exactly. Payload includes
  `tenant_id`, `legal_entity_id`, `actor_id`, `jurisdiction_context`
  (populated from `rule_basis`, per CONTEXT.md), plus the remaining
  decision fields.
- Wired the publisher into `Handler` (new `EventPublisher` dependency)
  and `main.go`. `CreateDecision` now publishes only on a genuine first
  insert (`created == true`) — an idempotent replay must not re-emit the
  event. A publish failure is logged but does not fail the HTTP request
  (event delivery is a stubbed, non-blocking concern).
- Added `governance-decision-log-svc` to `.github/workflows/ci.yml`'s
  `matrix.service` list and to the `TEST_DATABASE_URL` conditional
  (alongside `jurisdiction-rules-svc` / `identity-context-svc`).
- Wrote a multi-stage `Dockerfile` + `.dockerignore`, mirroring the
  structure of `audit-event-store-svc`'s Dockerfile (found on its
  as-yet-unmerged `feat/audit-event-store-context-resolved` branch —
  still absent from `main` as of this writing): `golang:1.25-alpine`
  builder → `gcr.io/distroless/static-debian12:nonroot` runtime,
  statically linked, non-root, `EXPOSE 8083`.
- Added a service list table to `services/README.md` (previously just a
  one-line header) covering all 5 existing services, not only this one.
- 3 new handler unit tests: publish fires exactly once on 201, does not
  fire on a 200 replay, and a publish error doesn't change the response
  status. All existing tests still pass (29 total: unit + Postgres
  integration).
- Verified live end-to-end against the **actual built Docker image**
  (not `go run`): `docker build` succeeded, container booted against a
  real `postgres:16-alpine` container, `/healthz` and `/readyz` returned
  200, `POST /v1/decisions` returned 201 and the container logs showed
  the `governance.decision.recorded` envelope emitted exactly once,
  `GET /v1/decisions/{id}` returned 200, and a replayed POST returned
  200 with no second "event emitted" log line. All test containers,
  network, and the built image removed after verification.
- Committed and pushed to `shashi-changes`.

### 2026-07-07 — Manual Postman session against `gdl-test-server` (post Phase 3)
- Spun up `gdl-test-postgres` + `gdl-test-server` (`go run ./cmd/server`,
  bind-mounted source) again for a live Postman session covering the
  query surface: `GET /healthz` → 200, `GET /v1/decisions?from=<garbage>`
  → 400 (invalid timestamp correctly rejected), `GET
  /v1/decisions?entity=entity-A&rule_basis=policy-v3-sod` → 200 with an
  empty result (correct — no decisions had been posted yet in this
  session). No `POST` was sent in this pass, so no
  `governance.decision.recorded` event fired; the Phase 3 event-emission
  behavior remains verified by the earlier real-Docker-image pass above,
  not by this session.
- Test containers left running at the user's request for further manual
  testing (not yet torn down as of this log entry). Later torn down.

### 2026-07-07 — CONTEXT.md alignment audit: ErrStoreUnavailable now actually wired
- Line-by-line audit against CONTEXT.md found the implementation fully
  spec-compliant with one real gap: `domain.ErrStoreUnavailable` was
  declared (referencing "jurisdiction-rules-svc's fail-closed
  precedent") but `pg_store.go` never actually returned it — `Insert`,
  `FindByID`, and `List` all wrapped raw pgx errors with a plain
  `fmt.Errorf` instead. Functionally harmless (the handler already
  treats any non-`ErrDecisionNotFound` error as 503), but the sentinel
  was dead outside tests and callers couldn't `errors.Is` their way to
  "store is down" the way the doctrine comment promised.
- Fixed: all three methods now wrap genuine failures with
  `fmt.Errorf("%w: ...", domain.ErrStoreUnavailable, err)`, mirroring
  `jurisdiction-rules-svc`'s pattern exactly, plus added `s.log.Error`
  calls on each failure path (previously silent at the store layer).
- Added `TestPgStore_ErrorsWrapErrStoreUnavailable`: drops the table
  mid-test to force a real Postgres failure distinct from "no rows",
  then asserts `errors.Is(err, domain.ErrStoreUnavailable)` for
  `Insert`, `FindByID`, and `List` individually.
- Verified: full suite (30 tests: unit + Postgres integration) passes
  inside `golang:1.25`, including the new test.
- Remaining known deviations from the doc are process-only, not code:
  work landed on `shashi-changes` instead of three separate
  per-phase branches, and the Phase 2+3 PR into `main` is still open,
  not yet merged.

### 2026-07-07 — Closing out active development on this service
- All 3 build phases (write path, query surface, close the loop) are
  code-complete, fully aligned with `CONTEXT.md`, and verified live
  (real Postgres, real built Docker image, manual Postman). 30 tests
  passing. Nothing left to build against the current spec.
- Handed off: PR title/URL/description for `shashi-changes` → `main`
  (covering Phase 2, Phase 3, and the `ErrStoreUnavailable` alignment
  fix) given to the user to open and merge manually.
- Only remaining open items are explicitly deferred, not blocking:
  the fail-closed-vs-async delivery question and the read-API auth
  model question, both to be revisited when Authorization Service
  integration begins (a service that does not exist yet). Real Kafka
  wiring (currently stubbed — logs instead of producing) is also
  deferred, matching the same stub convention used by
  `identity-context-svc` and `tenant-entity-registry-svc`.
- Active development on this service pauses here. Next: moving to a
  new service in Phase 1 "Sovereign Spine" (see
  `docs/architecture/06-blueprint.md`).

### 2026-11-11 — Gaps closed, local E2E complete

What was wrong, and what replaced it:
- **000016 (G-07) could not be applied anywhere**: it referenced a `tenants`
  table this DB does not have, UPDATEd rows with the append-only trigger live,
  concatenated bytea with text, and chained in caller-supplied `decided_at`
  order. Rewritten (never recorded as applied anywhere): `chain_seq` /
  `prev_hash` / `curr_hash` set by a BEFORE INSERT trigger under a per-tenant
  advisory lock (no caller can supply hashes; concurrent appends serialise);
  `curr_hash = sha256(prev || gdl_canonical(row))` where the canonical form is
  the row's jsonb text; backfill in storage order; `evidence_packages`
  (append-only, RLS forced) with an RFC 6962 Merkle root and a package hash
  chaining packages. Also the register fields (title, summary, domain,
  priority, tags, related_obligation_id) and the data-driven
  `decision_domains` / `decision_domain_rules` / `decision_outcome_categories`.
- **VerifyChain checked only hash lengths; SealPackage authorized on the string
  "WORKFLOW_LEDGER_PRINCIPAL_ID" as an action, ignored the caller's package id,
  hashed `prev || last_decision_id` (not a Merkle tree) and counted leaves by
  string-comparing ids; GetPackage authorized on the tenant id as a legal
  entity; ExportChain wrote 200 before reading.** All rebuilt: full link +
  package re-computation, `GOVERNANCE_EVIDENCE_SEAL` on the platform scope,
  idempotent seal (Idempotency-Key), chain verified before sealing (409
  CHAIN_BROKEN), bundle built before the first byte.
- **List**: `total_count` was the page length; the cursor dropped microseconds
  (rows skipped/repeated) and was never issued on page one. Now a real count,
  RFC3339Nano keyset cursor on every page, stable `decided_at, decision_id`
  order, and facets (domain, status, priority, jurisdiction, causation,
  workflow, q incl. `GD-<year>-<n>`).
- **New routes**: `GET /v1/decisions/summary`, `GET /v1/decisions/{id}/evidence`,
  `GET /v1/replay-manifests`, `GET /v1/evidence/packages`,
  `GET /v1/evidence/packages/{id}/verify`.
- **Found by the live e2e**: replay read `amount` only at the top level, but
  policy-svc's relay nests it under `action_context` (every policy decision
  answered 400); the replay-history column list split `COALESCE(x, '')`.
  Both fixed with regression tests.
- Compose `DECISION_LOG_READ_AUTHZ` now `${…:-enforce}`; seed grants READ,
  REPLAY and GOVERNANCE_EVIDENCE_SEAL; four alert rules
  (`governance-decision-log` group, promtool-checked); `scripts/audit.sh` and
  the dependency e2e rewritten (both previously checked tables, metrics and
  statuses that do not exist).

Verified 11 Nov: unit + store suites (incl. tamper and full-rewrite detection
as the runtime role) green in golang:1.26; `e2e-governance-decision-log-deps.py`
64/64 (Kafka delivery, policy-svc relay as svc-policy, replay manifest,
offline re-verification of every link and Merkle root in the bundle);
`scripts/audit.sh` 40/0 (Prometheus scrape SKIPPED: not on the local stack);
console UI e2e 5/5 plus a 24-step drive (record, follow-up, filters, drawer
panes, verify, seal, package verify, both downloads, full record).

### 2026-11-11 (later) — X5: identity gate ready for `require`

- **mTLS listener** (`MTLS_ENABLED`, :8450): the platform CA client certificate
  is required; the identity gate admits `IDENTITY_PEERS` (default
  `policy-svc,workflow-svc`) as peer workloads vouching for X-Principal-Id.
  One workload identity serves the listener and the calls to
  authorization-svc's :8449 (`AUTHZ_MTLS_ENABLED`, now on in compose).
- **Fixed**: provisioning sent no bootstrap token (register M-01), so
  `AUTHZ_MTLS_ENABLED=true` could never start; and over mTLS the authz hop
  forwarded the caller's `X-Workload-Id` (policy-svc) on this service's
  certificate → `workload_identity_mismatch` 403 → 503 on every peer write
  (hidden in warn mode by the 5 s decision cache). The hop now names this
  service.
- compose: bootstrap tokens for governance-decision-log-svc, policy-svc and
  workflow-svc; port 8450; `IDENTITY_PEERS`.
- Verified: e2e 73/73 (warn), `--require` 9/9 (headers alone 401, verified
  envelope 200, tampered 401, no client cert refused at the handshake,
  policy-svc peer 201 as svc-policy, non-peer certificate 401); audit.sh
  41/0 with Prometheus scraping and the four rules healthy; console UI 5/5.
- Under `require`, policy-svc's relay (plain HTTP, no envelope) is refused
  401 and retries durably — it must call :8450 with its certificate first
  (policy-svc's change). Compose stays `warn` until then.

### 2026-11-11 (latest) — VC-1 closed (independence waived by the user)

- All 13 rows ✅. The user waived the independent pass for the 9 rows built in
  the verifying session. Re-run for it: suites 215/0/0 (+ readers, policy,
  workflow, identity-envelope), migrations up/down/up, 6 mutants killed
  (revocation lookup, seal-without-verify, correlation predicate, manifest
  paging, canonical alias, reader principal), live e2e 128/128, scheduled seal
  4/4, audit.sh live 40/40, governance.spec.ts 5/5.
- Writers' production grants: authorization-svc 000043 (svc-policy,
  svc-workflow; svc-access-control too); R5: access-control 000050/000051.
- Console: Trend Over Time y-axis labels were clipped (negative left margin)
  and the register's last column overflowed at 1536 px; both fixed.

### 2026-11-11 (late) — event schemas registered, reader grants by migration, console re-run

- **Schemas (R9).** `schemas/governance.decision.recorded.v1.json` and
  `.replayed.v1.json` are the envelope contracts; TestR9_* holds them to what
  the producer emits (both names, nulls included) and to the registry's shape
  rules. `deployments/scripts/register-governance-decision-log-schemas.py`
  registers all four names (wire + canonical) with fixed Idempotency-Keys and
  reads each back; run twice on the stack: version 1 each, unchanged.
- **Reader grants by migration (AC-2).** authorization-svc 000042 provisions
  `SVC_DECISION_LOG_READER` (READ only) for `svc-contract-lifecycle` and
  `svc-decision-support` in every tenant, and in each new tenant on its first
  role (trigger), once; `seed-demo-rbac.ps1` now only verifies it.
- **Console.** governance.spec.ts 5/5 against the built console on :3100
  (OIDC sign-in; `deployments/identity/oidc-clients.local.json` now also
  registers the :3100 callback the console tests use).
- Verified: GDL suite 215/0/0 (DB); authorization store suite 151/0/1;
  dependency e2e 128/128 (+ "R9:*" registry and live-event checks).

### 2026-11-11 (evening) — F07 readers, F11 decisions, :8450 hardening, scheduled sealing

- **F07 — the three readers.** Each now calls `https://governance-decision-log-svc:8450`
  with its own certificate (`identity-envelope/mtlsclient`, own bootstrap
  token; keygen loop and `IDENTITY_PEERS` extended) and the full read envelope
  with a purpose. contract-lifecycle-svc and decision-support-svc read as
  `svc-contract-lifecycle` / `svc-decision-support` (READ granted by
  `seed-demo-rbac.ps1`, role SVC_DECISION_LOG_READER); evidence-manifest-svc
  vouches for the signed-in user. Found while doing it: decision-support and
  evidence-manifest decoded the list as a bare array, so every recommendation
  silently degraded to NO_HISTORY and every range manifest failed; both now
  decode the page, and evidence-manifest follows `next_cursor` (fails closed
  past 100 pages). Their builds moved to the repository-root context (go.mod
  replaces identity-envelope); phase5 compose updated too. No stack needs
  `DECISION_LOG_IDENTITY_VERIFY=warn` any more.
- **F11 — decisions (user, 11 Nov):** retention class EV-REGULATED + record
  class COMPLIANCE_EVIDENCE (on decisions, events, bundle); the "audit"
  dimension is `?correlation=` (000017 index); `decision_id` is the GOV-07
  evidence id; GOV-07 owns GovernanceDecision (plan section 4, 23 Oct);
  purpose on reads (already built); C3: both event names during the
  deprecation window (`events.WithCanonicalAlias`, schema_version 1.3).
- **Hardening.** `mtls.RotatingIdentity` (listener + authz client read the
  certificate per handshake; renewal at 2/3 of lifetime) and
  `mtls.RevocationChecker` (CRL from mtls-management-svc, fail closed,
  `MTLS_REVOCATION_CHECK=off` only with ENV=local). `internal/sealer` seals
  every tenant each `EVIDENCE_SEAL_INTERVAL` (1h) as
  `svc-governance-decision-log` after re-verifying the chain; 000017 adds the
  read-only `app.evidence_sealer` RLS marker. Alerts
  GovernanceLedgerChainBroken / GovernanceLedgerSealingFailing (promtool: 85
  rules OK).
- Verified 11 Nov: suites in golang:1.26 — governance-decision-log-svc 213/0/0
  (DB, runtime role; 4 consecutive clean runs; one earlier run had a one-off
  failure of TestPgStore_List_FilterByAction/RuleBasis that did not
  reproduce in 6 later runs), identity-envelope, contract-lifecycle-svc,
  decision-support-svc, evidence-manifest-svc all ok; 000016 + 000017
  up/down/up on a scratch DB, access-control 000049 up/down/up; mutants killed:
  revocation lookup, seal-without-verify, correlation predicate, manifest
  paging. Live (14 containers, require mode): dependency e2e **121/121** (incl.
  contract approval refused on a DENIED decision and allowed on a GRANTED one,
  a recommendation from 3+ precedents, a manifest snapshotting the entity's
  decisions, a revoked peer refused at the :8450 handshake within the CRL
  refresh while another peer keeps working, the canonical alias on Kafka);
  `--scheduled-seal` 4/4 with a 20 s interval; `scripts/audit.sh` 43/0 with
  Prometheus loading all 6 rules. Console UI spec not re-run (no console code
  changed).

### 2026-11-11 (final) — callers on mTLS, `require` on, workflow relay live

- **identity-envelope/mtlsclient** (shared): a service's outbound workload
  identity — certificate from mtls-management-svc with the service's own
  bootstrap token, background provisioning with retry, renewal at 2/3 of the
  lifetime, server verified against the platform CA and by hostname.
- **policy-svc**: the evidence relay calls `GOVERNANCE_DECISION_LOG_MTLS_URL`
  (https://governance-decision-log-svc:8450) with its certificate.
- **workflow-svc**: the ledger relay does the same; its DB was 6 migrations
  behind (000018/000019 were already present untracked — verified object by
  object and recorded; 000008/12/17/20 applied).
- **access-control-svc 000049**: `GOVERNANCE_EVIDENCE_SEAL` (HIGH) registered;
  template GOVERNANCE_EVIDENCE_STEWARD v1 (READ, REPLAY, SEAL).
- **Seed**: WORKFLOW_FULL bundle (none existed: no demo user could approve) and
  svc-workflow's ledger grant.
- **Compose**: governance-decision-log-svc `IDENTITY_VERIFY=require`.
- Console: no change needed (it already sends the envelope); the three
  overview governance panels now pass the full session identity too.
- Verified: dependency e2e **79/79 in require** (policy relay + replay,
  workflow relay → decision in the chain, envelope / peer / refusals, Kafka,
  seal, offline bundle check); console UI 5/5; audit live sections green
  (host test binaries blocked by App Control — suites green in golang:1.26:
  identity-envelope, governance-decision-log-svc incl. DB, policy-svc,
  workflow-svc, access-control-svc).

## Next steps

- [ ] Commit and open the PR (nothing committed yet).
- [x] GDL-2: workflow-svc's ledger relay live (e2e "workflow relay: *").
- [x] contract-lifecycle-svc, decision-support-svc, evidence-manifest-svc on
      :8450 with their own certificates (D2-2.F07, 11 Nov evening).
- [x] Production hardening: rotation + CRL on :8450, GOVERNANCE_EVIDENCE_SEAL
      in access-control's taxonomy (000049), scheduled sealing, retention class.
- [x] VC-1 on F01-F04, F07-F09, F11, F13 (independence waived by the user).
- [x] Production grants by migration for svc-contract-lifecycle and
      svc-decision-support READ (authorization-svc 000042); writers
      svc-policy / svc-workflow by 000043.
- [x] Both event names registered in schema-registry-svc (R9).
