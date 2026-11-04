# evidence-requirements-svc completion plan (Governance Platform 7/7)

> **For agentic workers:** REQUIRED SUB-SKILL: use superpowers:executing-plans to run this plan
> task by task. Steps use checkbox (`- [ ]`) syntax. This is **one service, one session**. When
> ER-9 is done, stop and hand back for verification (see "Verifier protocol"). Do not start
> another service.

**Goal:** Take evidence-requirements-svc to the same finished state as authorization-svc: every
spec feature built and wired, tested on real Postgres, proven live on the rebuilt container by a
dependency E2E script that crosses every edge, and operable from a v2.0 console page.

**Architecture:** Same seven-stage recipe as `AUTH-PLANE-PLAN-2026-11-02.md` ("The per-service
recipe (stages S0–S6)"): baseline and feature matrix, then backend gaps, contract and docs, live
container, dependency E2E, console, and finally evidence and hand-back. Backend is Go (chi, pgx,
kafka-go), module `zoiko.io/evidence-requirements-svc`, importing `zoiko.io/identity-envelope`.
Frontend is Next.js 16 server pages built from the console kit in `components/admin/console`.

**Tech stack:** Go 1.25+, Postgres 16, Kafka (`zoiko-kafka`, `apache/kafka`), Traefik
(`zoiko-gateway`), Docker Compose project `deployments`; Next.js 16 / TypeScript; Python 3.14 for
the E2E script.

**Spec sources (read before starting):**
- `docs/architecture/03-microservices.md` §8.6 (purpose, five owned concerns, two events, the
  critical constraint) and §5.1; `docs/architecture/04-data-model.md` §7.1 and §15.1.
- Governance Control Plane: §1, §2 invariants #1–#11, §16 envelope and error classes, §17 events,
  negative test #13 (line numbers in the audit section below).
- `docs/audit_files/Governance Platform-audit-2026-09-28.md`: "7/7 — evidence-requirements-svc"
  (43 scored rows, line ~2072) and the 19 Oct re-score (line ~3584). The 5 ❌ rows still open
  there are the core of this plan.
- `docs/audit_files/testing_shashi_progress/domain1-domain2-features-and-requirements.md` D2-7
  (line ~546). Some of its MISSING lines were closed on 21 Oct (`12ff3559`); ER-0 re-checks each.
- `docs/audit_files/testing_shashi_progress/group2-governance-platform-testing.md`, section
  "evidence-requirements-svc (Port 8130)": the 19 Oct live rig and its 42 probes.
- `docs/audit_files/IMPLEMENTATION-PLAN-2026-10-23.md` §2 (R1–R15), §5 (decisions: do not re-ask),
  §7 (evidence block).
- `services/evidence-requirements-svc/context.md` (§5 semantics, §11 decisions) and `progress.md`.
  Read both as claims, not as facts.

**Reference implementation (copy its shape, do not rebuild the kit):**
- E2E: `deployments/scripts/e2e-authorization-deps.py`. One check per dependency edge. Writes are
  undone where possible. Exit status = number of failed checks.
- Live audit script: `services/gateway-auth-svc/scripts/audit.sh` and identity-context's.
- Contract test: `internal/handler/openapi_parity_test.go` / `openapi_validation_test.go` (already
  in this service).
- Console: `app/admin/authorization/page.tsx`, `components/admin/authorization/console/*`
  (`register.tsx` `RegisterView`, `data.ts`, `overview.tsx`, `forms.tsx`), kit
  `components/admin/console/*`, envelope headers `lib/api/envelope.ts`, result shape
  `app/admin/authorization/state.ts`, step-up `app/admin/account/actions.ts`.

---

## Context: state on 4 Nov 2026 (read from code, not from progress.md)

**Built and wired** (`cmd/server/main.go`):
- Routes: evaluate, get evaluation, requirement list/get, admin create/end-date, 14 PBC routes.
- Identity gate `idenv.NewGateFromEnv`; compose `IDENTITY_VERIFY=warn`.
- Legal entity checked against tenant-entity-registry-svc (`internal/registry`).
- Authorization on evaluate (`EVIDENCE_EVALUATE`, compose `warn`), on reads (`enforce`) and on the
  admin and PBC routes.
- document-vault client forwards the envelope (`internal/documentvault/client.go:85-120`).
- Distinct artifacts by canonical key; empty `reference_id` refused (`handler.go:440-500`).
- Transactional outbox with a worker; Idempotency-Key fingerprints (409 on mismatch); RFC 9457
  problems.
- `openapi.yaml` validated, with a route-parity test.

**Open:**
1. **No live proof of the real document path.** The 200 path against the real document-vault-svc
   has never run. The 19 Oct rig blackholed it.
2. **No live proof of Kafka publication.** Outbox rows were only ever seen failing with Kafka down.
3. **The edge skips the gateway.** The Traefik route in `deployments/traefik-dynamic/all-services.yml`
   is local-only with no ForwardAuth. The compose block has no Traefik labels at all. Compare
   `jurisdiction-svc`, whose labels at compose line ~1055 carry `gateway-auth`.
4. **Unverified artifact types.** `EVIDENCE_MANIFEST` and `SIGNATURE` references count on the
   caller's word. Owners exist: `evidence-manifest-svc` (`GET /v1/evidence-manifests/{manifestID}`)
   and `esignature-integration-svc` (`GET /v1/esignature/envelopes/{id}`). Both are teammates'
   services: call them, never edit them.
5. **Grants.** `seed-demo-rbac.ps1` `EVIDENCE_FULL` carries only `EVIDENCE_REQUIREMENT_CREATE` and
   `_RETIRE`. Whether every action the handlers check is in a role template (R5) is unverified.
6. **No console read models.** There is no evaluation list, no summary and no cursor paging, so
   the console cannot show a register.
7. **API-3 is missing.** PBC requests are a mutable aggregate (send, reassign, extend, clarify,
   mark-satisfied, close) with no `record_version`, ETag or If-Match. A requirement's end-date has
   no If-Match either.
8. **Deliverables missing:** `asyncapi.yaml`, `RUNBOOK.md`, `RELEASE_CERTIFICATE.md`,
   `SPEC_DEVIATIONS.md`, `scripts/audit.sh`, a feature matrix, and
   `deployments/scripts/e2e-evidence-requirements-deps.py`.
9. **Old console page.** `/admin/evidence` (`app/admin/evidence/page.tsx`, `CatalogPanel`,
   `EvidenceForms`, `EvidenceLookups`) predates the v2.0 layout.
10. **Caller-side gaps (R1, teammates, not fixable here):**
    - general-ledger post, financial-close lock, vat-gst and corporate-tax have no gate;
    - board-resolutions, corporate-actions and filing-preparation send an incomplete envelope;
    - corporate-actions and filing-preparation pass any outcome other than MISSING.

---

## Rules learned from the gateway-auth-svc pass (the verifier rejected these on 4 Nov)

These are hard rules for this session:
- **A test named in the matrix must exist.** Before writing a test name into `progress.md`, run
  `grep -rn "func <Name>(" services/` and get a hit. The gateway matrix named five tests that exist
  nowhere.
- **A file named in the matrix must exist.** The gateway matrix pointed at a `SPEC_DEVIATIONS.md`
  that was never written.
- **A skipped check is not a PASS.** In the E2E script, a check that cannot run prints `SKIP`. It
  is counted separately and never added to the pass total. A mandatory edge may not be skipped.
- **The E2E must actually run, and its output goes into `progress.md` verbatim.** Writing a script
  is not proof. The gateway script crashes with `TypeError` at its fifth check (`call(...,
  envelope(jwt), headers=...)` passes `headers` twice). It logs in with a grant the console client
  does not have, and it health-checks port 8080 as "commercial-account".
- **DONE means the plan's stated behaviour, not a nearby one.** GW-5 asked the gateway to
  exchange the envelope for the target service's audience on `/verify`. What was built exchanges
  only for the gateway's own outbound calls.
- **A fallback that hides a failure is a defect.** Example: exchange fails, so the original
  envelope passes through. If you keep one, give it a metric and a test.

---

## Global constraints

Everything in IMPLEMENTATION-PLAN §2 and the AUTH-PLANE "Global constraints" applies, including
the Windows/host facts. These bite hardest here:

- **R1 scope.** You may edit:
  - `services/evidence-requirements-svc`;
  - `services/access-control-svc` (migrations only, for R5);
  - `services/authorization-svc` (only if a grant migration is needed);
  - `services/identity-envelope`, `deployments/`, `docs/`, and the frontend.

  document-vault, evidence-manifest, esignature-integration, general-ledger, financial-close,
  board-resolutions, corporate-actions, filing-preparation, vat-gst and corporate-tax belong to
  teammates: **call only**. Anything they need goes into "Cross-team follow-ups" in `progress.md`.
- **R3 git:**
  - Branch `shashi-changes` in both repos. One commit per task.
  - Stage files by explicit path. Never push. Never commit `*.exe` / `*.bin`.
  - Message format: `feat(evidence-requirements-svc): …` with a body, ending
    `Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>`.
- **R4 migrations.** The next free number here is **000010** (`ls` first). Each migration has a
  `.down.sql`. Use RLS + FORCE with the `NULLIF(current_setting('app.tenant_id', true), '')::uuid`
  policy, and guard every grant. Apply up/down/up on a scratch DB. Migrations 000007, 000008 and
  000009 are not idempotent, so recreate the scratch DB between runs.
- **R5.** Every action a handler checks must be registered in access-control **and** carried by a
  role template.
- **R6.** Every new or changed route keeps:
  - the envelope gate;
  - authorization at the object acted on;
  - Idempotency-Key;
  - If-Match/ETag on aggregate mutations (428 when missing, 412 when stale);
  - problem+json errors;
  - events through the outbox in the same transaction;
  - documentation in `openapi.yaml`, covered by the parity test.
- **R7.** Everything is reached from `main.go`. **R8:** no `go f(ctx)` with a request context.
  **R14:** no stubs and no invented answers.
- **Fail-closed semantics are the contract.** Do not weaken them. The four cases:

  | Case | Required behaviour |
  |---|---|
  | An owner (document-vault, evidence-manifest, esignature, registry, authz) is unreachable | 503, and **no evaluation row is written** |
  | An artifact is verified absent or out of scope | It does not count, and the reason is recorded in `unmet` |
  | `NO_REQUIREMENTS_DEFINED` | Stays distinct; never reported as SATISFIED |
  | A requirement payload is malformed | Unmet, never skipped |
- **Containers:** start only what a step needs. Stop and remove what you started when the step is
  done. Containers you did not start: ask before stopping them.

## Review focus

These are the inputs most likely to break a real user. Each has a pinned test in the owning task
and an E2E check.

1. **SATISFIED from evidence that was never proven.** Five artifacts must not count:
   - a manifest id from another tenant;
   - a signature envelope that does not exist;
   - one document offered twice against `minimum_count: 2`;
   - a document of another legal entity;
   - an empty reference.

   (ER-2, E2E §6–§8)
2. **A SATISFIED result replayed for a different action, entity or artifact set.** The answer must
   be 409 `idempotency_mismatch`, never the old result (E2E §4).
3. **An owner outage written down as a fact.** With document-vault, evidence-manifest or
   esignature down, the answer is 503 and the ledger has no new row (ER-2, E2E §6–§8).
4. **Event delivery.** Each SATISFIED or MISSING produces exactly one event on Kafka with key
   `<tenant>/<evaluation_id>`. A replay produces none. NO_REQUIREMENTS_DEFINED produces none
   (E2E §9).
5. **Forged identity through the edge.**
   - Via Traefik, a request with no bearer is 401 from the gateway.
   - A forged `X-Tenant-Id` is replaced by the verified tenant.
   - A tampered envelope is 401.
   - A session invalidated in identity-context is refused within the revocation TTL.

   (ER-5, E2E §2–§3)

---

## ER-0 · Baseline and feature matrix (S0)

- [ ] `git status -- services/evidence-requirements-svc` is clean, or every diff is explained and
  committed or reverted.
- [ ] Build, vet and gofmt pass (`tr -d '\r' < f | gofmt -l`).
- [ ] Run the full suite on a scratch Postgres (`TEST_DATABASE_URL`, `REQUIRE_DB_TESTS=1` if the
  harness honours it), plus the integration-tagged store suite (29 cases on 19 Oct). Record the
  pass/fail/skip count per package. Store suites: 0 skip.
- [ ] Rewrite `services/evidence-requirements-svc/progress.md` around a **feature matrix**.
  - Columns: `ID | requirement | route/consumer | test | console screen | status`.
  - Status values: `DONE` / `BUILT-THIS-PASS` / `BLOCKED: reason`.
  - One row per item from:
    - Doc 03 §8.6: the purpose, the five owned concerns, both events and the constraint;
    - Doc 04 §7.1 attributes;
    - every scored row of the Governance audit 7/7 (43 rows; keep the audit's status alongside);
    - every D2-7 bullet;
    - each PBC route;
    - the context.md §11 decisions;
    - every task in this plan.
- [ ] Re-check each D2-7 MISSING/PARTIAL line against the code. Mark the ones the 21 Oct pass
  (`12ff3559`) closed, citing file:line.
- **Done when:** suite counts are in progress.md and the matrix is committed.

## ER-1 · Grants by migration (R5) and demo seed

- [ ] List every action the handlers check (`grep -rhoE "EVIDENCE_[A-Z_]+" internal/handler
  internal/authz | sort -u`). Today there are eight base actions plus four PBC actions.
- [ ] Check each one against access-control `permission_definitions` (migrations 000008, 000021).
  Check that each is carried by a role template. Add any missing registration or template entry
  in a new access-control migration, numbered with the next free number (`ls` first).
- [ ] Store test: a principal instantiated from that template is granted each action.
- [ ] `seed-demo-rbac.ps1`: the demo admin must hold every evidence action for the E2E. One
  unprivileged subject must hold none. Remove seed lines only when a migration covers them.
- **Tests:** `TestR5_EvidenceActionsRegistered`, `TestR5_EvidenceTemplateGrantsEveryAction`.

## ER-2 · Verify manifest and signature references (closes the ⚠️ "non-document artifacts" row)

- **Files:**
  - new `internal/manifest/client.go` and `internal/esignature/client.go` (+ `_test.go`), shaped
    like `internal/documentvault/client.go`: forward the envelope, set principal, request id and
    channel;
  - `internal/handler/handler.go` (artifact checking around line 440);
  - `internal/config/config.go`, `cmd/server/main.go`, compose, k8s manifest.
- **Behaviour** (memoise results per reference within one evaluation):

  | Type | Checked against | Counts only when | Not found / out of scope | Unreachable |
  |---|---|---|---|---|
  | `EVIDENCE_MANIFEST` | evidence-manifest-svc `GET /v1/evidence-manifests/{id}` | it exists in the caller's tenant and legal entity | unmet, with the reason | 503, no row written |
  | `SIGNATURE` | esignature-integration-svc `GET /v1/esignature/envelopes/{id}` | it exists in scope | unmet, with the reason | 503, no row written |

  - Read each owner's handler first, to learn its auth headers and response fields. Do not edit
    them.
  - Which signature or document **statuses** count is not specified. Count existence and scope
    only, and record it in SPEC_DEVIATIONS as decision D-ER-3.
- **Config:**
  - `EVIDENCE_MANIFEST_SERVICE_URL` and `ESIGNATURE_SERVICE_URL`, both required outside
    `ENV=local`.
  - Unset locally means the type is verified as unavailable (503). It never silently counts.
  - Add a test for that.
- **Tests:**
  - `TestER02_ManifestOutsideTenantDoesNotCount`
  - `TestER02_ManifestUnknownIsUnmet`
  - `TestER02_ManifestServiceDownIs503NoRow`
  - `TestER02_SignatureUnknownIsUnmet`
  - `TestER02_SignatureServiceDownIs503NoRow`
  - `TestER02_SameManifestTwiceCountsOnce`
  - `TestER02_UnconfiguredOwnerNeverCounts`
- If an owner's API cannot answer "does this id exist in this tenant/entity", mark that type
  `BLOCKED: <owner> exposes no scoped read`. Leave it counting on the caller's word **only** if
  that is today's behaviour, and record it.

## ER-3 · Console read models (API-4 cursor paging)

- **Files:** `internal/handler/handler_lists.go` (+ test), `internal/store/pg_lists.go` (+ store
  test).
- **Routes** (all `GET`):
  - Cursor via `identity-envelope/apicontract/cursor.go`.
  - `page_size` default 50, max 200. An unknown `filter[...]` is 400.

  | Route | Filters | Action |
  |---|---|---|
  | `/v1/evidence/evaluations` | `filter[outcome]`, `filter[domain_code]`, `filter[action_type]`, `filter[legal_entity_id]`, `filter[since]` | `EVIDENCE_EVALUATION_READ` |
  | `/v1/evidence-requirements` (add cursor paging; keep `as_of`) | | |
  | `/v1/pbc/requests` (add cursor paging; keep the existing filters) | | |
  | `/v1/evidence/summary` | | `EVIDENCE_REQUIREMENT_READ` |

  The summary returns these counts:
  - active requirements;
  - evaluations by outcome over the last 30 days;
  - distinct (domain, action) pairs evaluated with `NO_REQUIREMENTS_DEFINED` in the last 30 days;
  - open PBC requests and overdue PBC requests;
  - unpublished outbox rows older than 5 minutes.
- **Tests per list:**
  - first page;
  - next page;
  - tampered cursor 400;
  - a cursor from another tenant 400;
  - unknown filter 400;
  - RLS isolation as the NOBYPASSRLS app role (`TestRLS_ListsNeverCrossTenant`).

## ER-4 · API-3 on the mutable aggregates

- **Migration 000010_record_version:** add `record_version bigint NOT NULL DEFAULT 1` to
  `evidence_requirements` and the PBC request table. Read `000004` for its name.
- **GET** of a requirement and of a PBC request returns `ETag: "<record_version>"`.
- **Every mutation of an existing aggregate:**
  - requirement end-date;
  - PBC send, reassign, extend-due-date, clarify, mark-satisfied and close.

  Each one:
  - requires `If-Match`: missing is 428 `precondition_required`, stale is 412
    `precondition_failed`;
  - bumps the version in the same statement;
  - returns the new ETag.
- Response submit, notes, view and acknowledge append rows; they do not mutate the request. State
  the rule you apply in `progress.md`.
- **Tests:** three per mutation (missing 428, stale 412, current 2xx), e.g.
  `TestAPI3_EndDateNeedsIfMatch`, `TestAPI3_PBCCloseStaleIs412`.

## ER-5 · Edge, identity and observability

- [ ] **Traefik labels with ForwardAuth.** Give `evidence-requirements-svc` labels in
  `deployments/docker-compose.yml`, copied from `jurisdiction-svc` (line ~1055):
  - router `evidence-requirements`, path prefix `/evidence-requirements`;
  - middlewares `gateway-auth,<strip>`.

  Keep the loopback-only port publish. If gateway-auth-svc's edge-contract tests
  (`TestEdgeContract_ForwardAuthListsMatchVerify` and the related tests) enumerate routers, extend
  them. Do not touch the local-only `all-services.yml` generator, except for a comment if needed.
- [ ] **Identity gate.** Read `services/identity-envelope` to learn exactly what `warn` admits.
  Then state, in `progress.md` and SPEC_DEVIATIONS, why compose stays `warn`: the callers in
  other domains send no bearer.
  - A tampered or expired envelope must be 401 in both modes. If `warn` admits a bad signature,
    that is a defect in identity-envelope (ours): fix it there, with a test.
- [ ] **Revocation.** A session invalidated in identity-context is refused (introspection) within
  the TTL. Add a handler test with a fake introspector; the E2E proves it live.
- [ ] **Telemetry.** Set `OTEL_EXPORTER_OTLP_ENDPOINT` explicitly in compose (the code default is
  `otel-collector:4318`). Add metrics, and a `/metrics` test that shows each series:
  - `evidence_evaluations_total{outcome}`;
  - `evidence_owner_unavailable_total{owner}`;
  - `evidence_outbox_unpublished` (gauge).

  Add alert rules to `deployments/prometheus-rules.yml` for owner unavailability and outbox
  backlog.

## ER-6 · Contract and docs (S2)

- [ ] `openapi.yaml` for every new or changed route (ER-2 problem codes, ER-3 lists and summary,
  ER-4 ETag/If-Match/428/412). Must pass kin-openapi validation and the parity test.
- [ ] `asyncapi.yaml`: `evidence.requirement.satisfied` and `.missing`, topic
  `zoiko.evidence-requirements.events`, key `<tenant>/<evaluation_id>`, and the envelope fields
  (causation, jurisdiction, governing refs). If schema-registry-svc's registration route exists,
  register both schemas; otherwise record it as a follow-up.
- [ ] `RUNBOOK.md`: config, start-up refusals, owner outages, outbox backlog, gate modes, how to
  seed the catalogue.
- [ ] `RELEASE_CERTIFICATE.md`, certified against this pass's evidence only.
- [ ] `SPEC_DEVIATIONS.md` with these decisions:
  - D-ER-1 tier (§23 omits the service);
  - D-ER-2 scan-result ownership (`EVIDENCE_REQUEST_MANAGE` is interim);
  - D-ER-3 which document and signature statuses count;
  - D-ER-4 nullable `legal_entity_id` on the requirement;
  - D-ER-5 the `NO_REQUIREMENTS_DEFINED` caller contract (decided 21 Oct: the caller decides);
  - D-ER-6 compose gate modes `warn` (evaluate, identity) and why.
- [ ] `scripts/audit.sh`, modelled on gateway-auth-svc's: static checks, unit, live health,
  allow path, every refusal, telemetry, contract parity.
- [ ] Postman collection: send the full envelope.
- [ ] Refresh the stale `context.md` §4/§9/§12 statements.

## ER-7 · Live container (S3)

- [ ] `docker compose -p deployments build evidence-requirements-svc` (root context). Check that
  `docker images` shows a fresh timestamp.
- [ ] Migrate first: `python3.14 deployments/migrate.py up --db evidence_requirements` (and
  `--db access_control` for ER-1). Then `up -d --no-deps --no-build --force-recreate
  evidence-requirements-svc`.
- [ ] Start only the dependencies the E2E needs:
  - postgres, kafka;
  - identity-svc, tenant-svc, authorization-svc (needs mtls-management-svc), access-control-svc;
  - document-vault-svc, evidence-manifest-svc, esignature-integration-svc;
  - gateway-auth-svc + gateway (Traefik);
  - otel-collector + jaeger.

  Run the seeds.
- [ ] Run `scripts/audit.sh` live. Paste the totals into `progress.md`.

## ER-8 · Dependency E2E (S4): `deployments/scripts/e2e-evidence-requirements-deps.py`

Model it on `e2e-authorization-deps.py`:
- `check(name, ok, detail)`; a separate `skip(name, reason)` that never counts as a pass;
- `call()` returns `(status, headers, body)`;
- `envelope(principal, write)` builds the canonical headers;
- `until()` for asynchronous edges;
- exit status = failures.

Every write is undone where the API allows it:
- requirements are end-dated;
- PBC requests are closed;
- documents are left in place, but named `e2e-…`.

Mandatory sections, one check per edge:

0. **Health:** evidence-requirements `/readyz`, authorization, identity, tenant registry,
   access-control, document-vault, evidence-manifest, esignature, gateway-auth, Traefik, Jaeger.
1. **access-control-svc:** `GET /v1/permission-definitions` lists every evidence action (ER-1).
2. **identity-context-svc:**
   - login (confidential client `zoiko-console`, as in `e2e-authorization-deps.py`) → resolve →
     envelope;
   - a call with the envelope is 200 and the gate reports verified;
   - a tampered signature is 401;
   - invalidate the session, then the call is refused within the TTL (measure and print the lag).
3. **Traefik + gateway-auth-svc:**
   - `http://127.0.0.1/evidence-requirements/...` with no bearer is 401 with
     `X-Auth-Denial-Reason: no_token`;
   - with the envelope it is 200;
   - a forged `X-Tenant-Id` / `X-Principal-Id` is replaced (the response or ledger row shows the
     verified tenant).
4. **Core evaluate and idempotency:**
   - with an empty catalogue for a fresh `action_type`: `NO_REQUIREMENTS_DEFINED`;
   - create a requirement: 201; replay: 200 with the same id; different body: 409;
   - evaluate replay: 200 with the same `evaluation_id`; same key, different action: 409.
5. **tenant-entity-registry-svc:** a legal entity outside the tenant is refused with the
   documented code. A random UUID is refused. Neither writes a ledger row.
6. **document-vault-svc (the path never proven):**
   - create a real document as the demo admin;
   - with a SUPPORTING_DOCUMENT requirement (`minimum_count: 1`), evaluate with it: **SATISFIED**;
   - a random id: MISSING, with the reason;
   - the same document twice against `minimum_count: 2`: MISSING;
   - a document of another entity, if the seed has one: MISSING.
   - Optional `--with-outage`: stop document-vault-svc, evaluate → 503 and the ledger count is
     unchanged; then start it again.
7. **evidence-manifest-svc:** a real manifest counts; an unknown id does not. If the owner's seed
   data makes this impossible, `skip` it with the reason and mark it BLOCKED in the matrix.
8. **esignature-integration-svc:** the same as §7 for an envelope id.
9. **Kafka:**
   - read `zoiko.evidence-requirements.events` with `docker exec zoiko-kafka
     /opt/kafka/bin/kafka-console-consumer.sh --bootstrap-server localhost:9092 --topic … --from-beginning
     --timeout-ms 15000 --property print.key=true` (check the listener name in compose first);
   - the SATISFIED and MISSING evaluations from §4/§6 each appear **exactly once**, keyed
     `<tenant>/<evaluation_id>`;
   - the replay added none; NO_REQUIREMENTS_DEFINED added none;
   - the outbox rows are `published = true` (`docker exec zoiko-postgres psql …`).
10. **authorization-svc:**
    - the unprivileged subject gets 403 on admin create, on reads, and on PBC commands;
    - with evaluate in `warn`, the subject's evaluate is admitted, **and** authorization-svc's
      decision log records a DENY for `EVIDENCE_EVALUATE` (this proves the edge is asked).
11. **PBC end to end:**
    - create → send → submit response → scan (MANAGE) → mark satisfied → close, with If-Match on
      each command;
    - a stale If-Match is 412;
    - the scan by a submitter is 403.
12. **Tracing:** `evidence-requirements-svc` is listed in Jaeger `/api/services`.

Optional (caller edges, teammates' services; `skip` when not running, never a pass):
- board-resolutions pass with a MISSING outcome is refused.
- corporate-actions execute with a MISSING outcome is refused. Expect the known fail-open on an
  unknown outcome, and print it as a finding for the cross-team list.

**Done when:** every mandatory check PASSES on the live stack, and the full stdout is pasted into
`progress.md`.

## ER-FE-1 · API client

- `lib/api/evidence.ts`: add the evaluation list, summary, cursor paging on requirements and PBC,
  the PBC commands with `ifMatch`, and end-date with `ifMatch`. All go through
  `lib/api/envelope.ts` (bearer, Idempotency-Key, If-Match).
- `explainEvidenceError` covers the new problem codes (412, 428, owner unavailable,
  `idempotency_mismatch`, `legal_entity_mismatch`, `invalid_identifier`).

## ER-FE-2 · `/admin/evidence` rebuilt in v2.0

- Replace `app/admin/evidence/page.tsx` with a server page shaped like
  `app/admin/authorization/page.tsx`:
  - `actions.ts` and `state.ts` reuse the authorization result shape;
  - add `components/admin/evidence/console/` with `data.ts`, `overview.tsx`, `requirements.tsx`,
    `evaluations.tsx`, `pbc.tsx`, `check.tsx` and `forms.tsx`;
  - reuse `RegisterView`; do not copy it;
  - delete the old `CatalogPanel` / `EvidenceForms` / `EvidenceLookups` once nothing imports them.
- **Header:** "Evidence Requirements", with:
  - gold primary "Add requirement";
  - secondary "Check evidence" (the evaluate form).
- **KPIs** (all from `/v1/evidence/summary`):
  - Active requirements
  - Satisfied (30 d)
  - Missing (30 d)
  - Actions with no requirements (30 d)
  - Open evidence requests (overdue as helper text)
- **Panels:**
  - **Gate pipeline:** Requested → Catalogue resolved → Artifacts verified → Decision recorded →
    Event published.
  - **Evidence types ordered list:** SUPPORTING_DOCUMENT, EVIDENCE_MANIFEST, SIGNATURE,
    APPROVAL_RECORD and others. Each shows its count of active requirements and a "verified by"
    chip (document vault / evidence manifest / e-signature / caller's word).
  - **Catalogue coverage bar list by domain:** an amber callout lists the actions evaluated with
    no requirements.
- **Tabs:**
  - Requirements
  - Evaluations (drawer: unmet list, the artifacts asserted, and the requirement set tested
    against)
  - Missing
  - Evidence requests (PBC)
  - Check evidence
- **Drawer commands:**
  - end-date: reason dialog + If-Match;
  - PBC send / reassign / extend / clarify / mark satisfied / close;
  - scan result: shown only to MANAGE holders;
  - every command uses the reason dialog and the step-up retry.
- **Footer:** "Required controls: evidence verified with its owner · a decision is never written
  when an owner is down · replays never change an answer · a reason on every change".
- **Checks:**
  - `npx tsc --noEmit` (no new errors in touched files) and `npx eslint <touched files>` clean.
  - Render every tab against the live service (headless Chrome screenshot), including one empty
    state and one error state.
  - Live data only. An unreadable source shows "unavailable", never 0.

## ER-9 · Evidence and hand-back (S6)

- [ ] An IMPLEMENTATION-PLAN §7 evidence block per task in `progress.md`: commands, counts,
  commit ids, E2E stdout.
- [ ] Update the audit files:
  - `IMPLEMENTATION-PROGRESS.md`: API-3 and API-4 rows for this service;
  - `group2-governance-platform-testing.md`: the evidence-requirements section status, a new
    dated block, and the summary table row;
  - `Governance Platform-audit-2026-09-28.md`: a new dated section "evidence-requirements-svc
    completion, <date>" that re-scores each of the 43 rows that moved, with file:line and test
    names;
  - D2-7 in `domain1-domain2-features-and-requirements.md`.
- [ ] **Mutation check** on a scratch copy. Break each guard below; a named test must fail for
  each. Restore afterwards.
  - the distinct-artifact key (count duplicates);
  - the owner-unavailable branch (count the artifact instead of 503);
  - the replay fingerprint (ignore `action_type`).
- [ ] Stop. Report the commit list and the E2E totals, and ask for verification. Do not start the
  console of another service.

---

## Verifier protocol (run by Claude after ER-9; nothing is taken from the summary)

1. **Commits.** `git log` / `git show --stat` for this pass: only R1 paths, explicit staging, no
   binaries, one commit per task.
2. **Suite.** Build, vet and the full suite on a scratch Postgres. Counts match `progress.md`;
   store suites have 0 skip.
3. **Matrix walk.** Every test name in the matrix resolves with `grep -rn "func <Name>("`. Every
   file named exists. Every row has a route or consumer, a test and a console screen, or a BLOCKED
   reason that names an external owner or a decision.
4. **Wiring.** Grep `main.go` callers for every new client, handler, store method and metric (R7).
5. **Contract.** kin-openapi and parity pass. asyncapi lists both events.
6. **Migrations.** 000010 (and any access-control migration): `.down.sql`, RLS/FORCE/NULLIF,
   guarded grants. Up/down/up on a scratch DB.
7. **R5.** Every checked action is registered and in a template.
8. **Live.** The image is rebuilt, migrations are applied, `e2e-evidence-requirements-deps.py` is
   re-run by the verifier with all mandatory checks PASS, and `scripts/audit.sh` is green.
9. **Mutation.** The three guards from ER-9 are re-broken; a test fails for each.
10. **Frontend.** tsc/eslint clean on touched files; each tab rendered live; empty and error states
    honest.

Any failure reopens the task.

## BLOCKED (build the interface and the local implementation; mark only what is truly external)

- **Caller retrofits (teammates):**
  - gates in general-ledger post, financial-close lock, vat-gst and corporate-tax;
  - the full envelope from board-resolutions, corporate-actions and filing-preparation;
  - an allow-list (only SATISFIED passes) in corporate-actions and filing-preparation.

  List these under "Cross-team follow-ups".
- **No consumer of the two events exists in the estate.** Kafka publication is proven by reading
  the topic, not by a consumer.
- **Decisions D-ER-1 … D-ER-4** (tier, scan ownership, statuses, nullable entity): record them,
  do not decide them.
- **The production mTLS client to authorization-svc** (`AuthzMTLSEnabled`): optional locally.
  Record whether it was exercised.

## Execution handoff

First step: commit this file (`docs: add evidence-requirements-svc completion plan`). Then run
ER-0 … ER-9 in order, in one session, with superpowers:executing-plans. Give the executing model
this file plus IMPLEMENTATION-PLAN §2/§5/§7 and the AUTH-PLANE "Global constraints". At the end,
return here for the verifier protocol.
