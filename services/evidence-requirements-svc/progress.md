# evidence-requirements-svc — Progress

**Status (5 Nov 2026): complete for this service's side of the contract, verified live.**
Plan: `docs/audit_files/EVIDENCE-REQUIREMENTS-PLAN-2026-11-04.md`. Suites green with 0 skip on real
Postgres; `scripts/audit.sh` 42/42 live; dependency E2E 84/84 live (3 owner-blocked skips, never
counted as passes); mutation check 3/3; console rebuilt in v2.0. What remains is outside this service
(see "BLOCKED" and "Cross-team follow-ups").

## Verification of the first execution (verifier protocol, 5 Nov)

A first execution of the plan earlier the same day marked ER-0 … ER-9 and ER-FE-1/2 DONE. The verifier
protocol rejected it:

| Protocol step | Finding |
|---|---|
| 3. Matrix walk | 9 test names in the matrix did not exist (`TestER02_*` ×7, `TestAPI3_EndDateNeedsIfMatch`, `TestAPI3_PBCCloseStaleIs412`). Suite counts disagreed with each other (44 vs 52). |
| 2. Suite | On real Postgres: `EndDateRequirement` failed (`column "effective_to" is of type timestamp … expression is of type uuid` — parameters bound in the wrong positions; 5 store tests failed). The integration-tagged store suite did not compile. Store tests had been reported "18 skip" instead of being run. |
| ER-4 code | If-Match compared the raw header (`"v2"`) with a bigint; the scan path updated a `record_version` column the responses table does not have; submit scanned two columns into one; `send` lost its DRAFT→SENT guard; no 428 and no ETag anywhere. |
| ER-3 code | `sort` was spliced into `ORDER BY` without the allow-list (SQL injection); keyset paging sought on the id while ordering by other columns; the cursor key was hard-coded; lists moved to new `/cursor` routes; the PBC list had no authorization; no list tests. |
| ER-5 | Traefik labels on an entrypoint that redirects before routing, and no ForwardAuth; no domain metrics, no revocation test, no alert rules. |
| 8. Live | No image built since 1 Sep (the ER-7 commit changed only progress.md); the E2E script was written, never run, and called routes that do not exist. |
| 10. Frontend | ER-FE-2 committed only progress.md ("page already exists"). |
| ER-9 | Audit files not updated; no mutation check. |

All of it was redone in this pass. Further defects found only by running live are listed under ER-7.

## Feature matrix

Status: `DONE` (built, tested, proven) · `BLOCKED: <reason>` (an external owner or a recorded decision).

### Doc 03 §8.6 — purpose, owned concerns, events, constraint

| ID | Requirement | Route / consumer | Test | Console screen | Status |
|---|---|---|---|---|---|
| ER-03-1 | Decide what evidence must exist before an action completes | `POST /v1/evidence/evaluate` | TestEvaluate_NoRequirementsDefined, TestEvaluate_Satisfied, TestEvaluate_Missing_NamesEachUnmetRequirement | Check evidence tab | DONE |
| ER-03-2 | Owns evidence preconditions (the catalogue) | `GET /v1/evidence-requirements`, `GET …/{id}`, `POST /v1/admin/evidence-requirements`, `…/{id}/end-date` | TestCreateRequirement_Idempotent_ReplayReturns200, TestAPI3_EndDateNeedsIfMatch, TestER03_ListPagesAreOrderedFilteredAndTenantScoped | Requirements tab, drawer | DONE |
| ER-03-3 | Owns document requirements; verifies document evidence | evaluate → document-vault `GET /v1/documents/{id}` | TestEvaluate_UnverifiableDocument_DoesNotCount, TestER02_DocumentOfAnotherEntityDoesNotCount, TestEvaluate_DocumentServiceUnavailable_Returns503; E2E §6 | Evaluations drawer | DONE |
| ER-03-4 | Owns signature requirements | evaluate → esignature-integration-svc `GET /v1/esignature/envelopes/{id}` | TestER02_SignatureUnknownIsUnmet, TestER02_SignatureServiceDownIs503NoRow, TestVerifyEnvelope; E2E §8 (fail-closed) | Evidence types panel | DONE (live SATISFIED: BLOCKED — esignature runs only in the phase7 stack) |
| ER-03-5 | Supporting artifact rules (subtype, minimum count) | `requirement_payload` | TestEvaluate_MinimumCountEnforced, TestEvaluate_ArtifactSubtypeMismatch | Requirements drawer | DONE |
| ER-03-6 | Sufficiency logic; a malformed rule fails closed | evaluate | TestEvaluate_UnreadablePayload_FailsClosed | Evaluations drawer | DONE |
| ER-03-7 | "No finalization path may skip required evidence states" | callers | E2E optional caller section (not run) | Gate pipeline panel | BLOCKED: caller-side (general-ledger, financial-close, vat-gst, corporate-tax have no gate) |
| ER-03-8 | Event `evidence.requirement.missing` | outbox → `zoiko.evidence-requirements.events` | TestPublishEvaluation_Missing_CarriesUnmetDetail, TestOutbox_AppRoleAppendsButCannotRead; E2E §9 | Gate pipeline panel | DONE |
| ER-03-9 | Event `evidence.requirement.satisfied` | outbox → Kafka | TestPublishEvaluation_Satisfied; E2E §9 | Gate pipeline panel | DONE |

### Doc 04 §7.1 — EvidenceRequirement

| ID | Attribute | Where | Test | Console screen | Status |
|---|---|---|---|---|---|
| ER-04-1…12 | id, tenant_id, legal_entity_id (nullable = tenant-wide, D-ER-4), domain_code, action_type, evidence_type, requirement_payload, effective_from/to, created_at/by, correlation_id, uniqueness | migration 000001 (+000006 idempotency, +000010 record_version) | TestPgStore_CreateRequirement_And_GetRequirement, TestPgStore_EffectiveRequirements_ScopeAndPointInTime, TestPgStore_CreateRequirement_RetriedCorrelationID_IsIdempotent | Requirements drawer | DONE |
| ER-04-13 | Retirement by end-dating only; no delete | end-date route | TestPgStore_EndDateRequirement_RetiresWithoutDeleting, TestEndDate_AlreadyRetired_Returns422 | Requirements drawer → Withdraw | DONE |

### Governance audit 7/7 — 43 scored rows (+5 ❓)

28 Sep → 19 Oct → 5 Nov. Re-score with file:line: `Governance Platform-audit-2026-09-28.md`, section
"evidence-requirements-svc completion, 5 November 2026". **5 Nov: 39 ✅ / 1 ⚠️ / 3 ❌ (90.7% / 91.9%).**

| # | Row | 28 Sep | 19 Oct | 5 Nov | Test / evidence |
|---|---|---|---|---|---|
| 1 | Purpose | ✅ | ✅ | ✅ | TestEvaluate_Satisfied |
| 2 | Owns document requirements, verifies document evidence | ❌ | ❌ | ✅ | TestEvaluate_UnverifiableDocument_DoesNotCount; E2E §6 |
| 3 | Owns signature requirements | ⚠️ | ⚠️ | ✅ | TestER02_SignatureUnknownIsUnmet |
| 4 | Supporting artifact rules | ✅ | ✅ | ✅ | TestEvaluate_ArtifactSubtypeMismatch |
| 5 | Sufficiency logic; fail closed | ✅ | ✅ | ✅ | TestEvaluate_UnreadablePayload_FailsClosed |
| 6 | Sufficiency counts distinct artifacts | ❌ | ❌ | ✅ | TestER02_SameManifestTwiceCountsOnce; mutation check |
| 7 | Non-document artifacts | ⚠️ | ⚠️ | ✅ | TestER02_EmptyReferenceNeverCounts, TestER02_UnconfiguredOwnerNeverCounts |
| 8 | Which document statuses count | ❓ | ❓ | ❓ | D-ER-3 |
| 9 | No finalization path skips evidence | ❌ | ❌ | ❌ | BLOCKED: caller-side |
| 10 | Callers reach the gate under the envelope | ❌ | ❌ | ❌ | BLOCKED: caller-side |
| 11 | Callers treat only SATISFIED as a pass | ❌ | ❌ | ❌ | BLOCKED: caller-side |
| 12 | NO_REQUIREMENTS_DEFINED blocks or permits | ❓ | ❓ | ❓ | D-ER-5 (decided 21 Oct: the caller decides) |
| 13 | Event `.missing` | ✅ | ✅ | ✅ | E2E §9 |
| 14 | Event `.satisfied` | ✅ | ✅ | ✅ | E2E §9 |
| 15 | Event envelope (causation, jurisdiction, refs) | ⚠️ | ⚠️ | ✅ | TestEvaluate_EventCarriesTheGoverningReferences |
| 16 | Outbox after commit; at-least-once | ❌ | ✅ | ✅ | TestOutbox_AppRoleAppendsButCannotRead; E2E §9 |
| 17 | idempotency_key mandatory; IDEMPOTENCY_MISMATCH | ❌ | ✅ | ✅ | TestEvaluate_IdempotencyMismatch_Returns409; mutation check |
| 18 | Idempotent requirement create | ❌ | ✅ | ✅ | TestCreateRequirement_IdempotencyMismatch_Returns409 |
| 19 | Catalogue mutation authorized; fail closed | ✅ | ✅ | ✅ | TestCreateRequirement_AuthzUnavailable_FailsClosed |
| 20 | Legal entity validated by the server | ❌ | ⚠️ | ✅ | TestEvaluate_LegalEntityMustExistInTheTenant; E2E §5 |
| 21 | Read contracts strongly scoped | ⚠️ | ⚠️ | ⚠️ | TestReads_NeedAPrincipalAndAGrantAtTheEntity, TestER03_ListsAreAuthorizedAtTheFilteredEntity — `GET /v1/pbc/requests/{id}` still tenant-only |
| 22 | tenant_id resolved by GOV-01, never trusted | ⚠️ | ⚠️ | ✅ | TestER05_ForgedIdentityHeadersAreRefused; E2E §3 |
| 23 | Canonical service input contract | ✅ | ✅ | ✅ | TestAPI2_EnvelopeRefusalIsAProblem |
| 24 | Server-resolved context | ⚠️ | ⚠️ | ✅ | E2E §3, §5 |
| 25 | Missing tenant 401 | ⚠️ | ⚠️ | ✅ | TestEvaluate_MissingTenant_Returns401 |
| 26 | Workload identity accepted | ⚠️ | ⚠️ | ✅ | TestEvaluate_AVerifiedPeerWorkloadIsTheCaller |
| 27 | Stable error classes | ⚠️ | ⚠️ | ✅ | TestAPI2_ErrorResponsesAreProblemDetails |
| 28 | evaluation id returned | ✅ | ✅ | ✅ | TestEvaluate_Satisfied |
| 29 | EvidenceRequirement attributes | ✅ | ✅ | ✅ | TestPgStore_CreateRequirement_And_GetRequirement |
| 30 | Nullable legal_entity_id | ❓ | ❓ | ❓ | D-ER-4 |
| 31 | Retirement by end-dating | ✅ | ✅ | ✅ | TestPgStore_EndDateRequirement_RetiresWithoutDeleting |
| 32 | Effective interval validity | ⚠️ | ⚠️ | ✅ | TestPgStore_EffectiveRequirements_ScopeAndPointInTime |
| 33 | Payload validated at create | ⚠️ | ⚠️ | ✅ | TestCreateRequirement_PayloadMustMatchTheRuleShape |
| 34 | Ledger append-only | ⚠️ | ⚠️ | ✅ | TestPgStore_EvaluationLedgerIsAppendOnly |
| 35 | Decision reproducible | ⚠️ | ⚠️ | ✅ | TestEvaluate_EventCarriesTheGoverningReferences |
| 36 | RLS FORCE / WITH CHECK / NULLIF | ✅ | ✅ | ✅ | TestRLS_ListsNeverCrossTenant |
| 37 | Explicit tenant predicate + non-superuser role | ✅ | ✅ | ✅ | TestPgStore_TenantIsolation_GetRequirement |
| 38 | Unknown / malformed id → 404 | ✅ | ✅ | ✅ | TestPgStore_MalformedUUID_ReadsAsAbsent |
| 39 | Malformed ids on write paths | ⚠️ | ✅ | ✅ | TestListRequirements_MalformedLegalEntityFilter_Refused |
| 40 | Request body cap | ✅ | ✅ | ✅ | audit.sh §4 (413) |
| 41 | Health, readiness, metrics, tracing | ✅ | ✅ | ✅ | TestER05_MetricsServesEveryEvidenceSeries; E2E §13 |
| 42 | Bounded authz cache | ✅ | ✅ | ✅ | internal/authz/client.go |
| 43 | API endpoints specified | ⚠️ | ⚠️ | ✅ | TestOpenAPIMatchesTheRouter, TestOpenAPIDocumentIsValid |
| 44 | context.md §4 list semantics | ⚠️ | ⚠️ | ✅ | context.md §4 (5 Nov) |
| 45 | context and progress claims vs the estate | ⚠️ | ⚠️ | ✅ | this file: every named test resolves |
| 46 | Postman collection usable | ⚠️ | ⚠️ | ✅ | postman_collection.json regenerated from openapi |
| 47 | Tier classification | ❓ | ❓ | ❓ | D-ER-1 |
| 48 | GOV-07 ownership | ❓ | ❓ | ❓ | Governance audit C2 |

### D2-7 (domain1-domain2-features-and-requirements.md), re-checked 5 Nov

| ID | Item | Status |
|---|---|---|
| D2-7-1 | evaluate; fail closed on a malformed rule | DONE |
| D2-7-2 | get evaluation | DONE (TestGetEvaluation_NotFound_Returns404) |
| D2-7-3 | catalogue list / get / create / end-date | DONE |
| D2-7-4 | artifact rules data-driven | DONE |
| D2-7-5 | append-only ledger; outbox events | DONE (Kafka live) |
| D2-7-6 | PBC requests (14 routes) | DONE (TestAPI3_*, TestPgStore_MarkSatisfied_RefusesSelfEvaluation; E2E §11) |
| D2-7-7 | document verification (was MISSING) | DONE (E2E §6) |
| D2-7-8 | signature verification (was PARTIAL) | DONE in code; live BLOCKED (phase7) |
| D2-7-9 | distinct artifacts (was MISSING) | DONE |
| D2-7-10 | non-document artifacts (was PARTIAL) | DONE; a real manifest BLOCKED: `EVIDENCE_MANIFEST_READ` unregistered |
| D2-7-11 | gates in GL post, close lock, vat-gst, corporate-tax | BLOCKED: caller-side |
| D2-7-12 | callers forward the envelope | BLOCKED: caller-side |
| D2-7-13 | callers allow-list | BLOCKED: caller-side |
| D2-7-14 | authorization on evaluate | DONE (TestEvaluate_IsAuthorizedAtTheEntity; E2E §10) |
| D2-7-15 | openapi.yaml | DONE |

### PBC routes

| ID | Route | If-Match | Test | Console | Status |
|---|---|---|---|---|---|
| PBC-1 | `POST /v1/pbc/requests` | — (ETag v1) | TestPgStore_CreateEvidenceRequest_ReplayWithDifferentBodyConflicts | Evidence requests → New request | DONE |
| PBC-2 | `GET /v1/pbc/requests` (API-4, EVIDENCE_REQUEST_MANAGE) | — | TestER03_PBCListKeepsItsOpenOnlyDefault, TestER03_ListsPageWithSignedCursors | Evidence requests tab | DONE |
| PBC-3 | `GET /v1/pbc/requests/{id}` (ETag) | — | TestAPI3_ReadsServeTheETag | drawer | DONE (authorization: tenant only, follow-up) |
| PBC-4 | `…/send` | required | TestAPI3_PBCSendNeedsIfMatch | drawer → Send | DONE |
| PBC-5 | `…/reassign` | required | TestAPI3_PBCReassignNeedsIfMatch | drawer → Reassign | DONE |
| PBC-6 | `…/extend-due-date` | required | TestAPI3_PBCExtendDueDateNeedsIfMatch | drawer → Extend | DONE |
| PBC-7 | `…/view` | no (bumps version) | TestAPI3_AppendingActionsTakeNoIfMatch | — | DONE |
| PBC-8 | `…/responses` | no (bumps version) | TestPgStore_SubmitResponse_AlwaysCreatesNewVersion | — | DONE |
| PBC-9 | `…/notes` | no | TestAPI3_AppendingActionsTakeNoIfMatch | — | DONE |
| PBC-10 | `…/clarify` | required | TestAPI3_PBCClarifyNeedsIfMatch | drawer → Ask to clarify | DONE |
| PBC-11 | `…/mark-satisfied` | required | TestAPI3_PBCMarkSatisfiedNeedsIfMatch, TestPgStore_MarkSatisfied_RequiresCleanScan | drawer → Mark satisfied | DONE |
| PBC-12 | `…/close` | required | TestAPI3_PBCCloseStaleIs412 | drawer → Close | DONE |
| PBC-13 | `POST /v1/pbc/responses/{id}/scan-result` | no | TestPgStore_MarkSatisfied_RequiresCleanScan; E2E §11 | drawer → Record scan (MANAGE holders only) | DONE |
| PBC-14 | `POST /v1/pbc/responses/{id}/acknowledge` | no | TestAPI3_AppendingActionsTakeNoIfMatch | — | DONE |

### context.md §11 decisions

| ID | Decision | Status |
|---|---|---|
| D-ER-1 | Tier (Doc 03 §23 omits the service) | RECORDED, open |
| D-ER-2 | Scan-result ownership (EVIDENCE_REQUEST_MANAGE interim) | RECORDED, open |
| D-ER-3 | Which document / signature statuses count | RECORDED, open (existence + scope only) |
| D-ER-4 | Nullable legal_entity_id | DECIDED, built |
| D-ER-5 | NO_REQUIREMENTS_DEFINED: the caller decides | DECIDED 21 Oct, built |
| D-ER-6 | Compose evaluate / identity gates warn, and why | RECORDED (what warn admits is pinned by tests) |

### Plan tasks

| Task | Status | Evidence block |
|---|---|---|
| ER-0 baseline and matrix | DONE | this file |
| ER-1 grants (R5) | DONE (000046 corrected) | ER-1 below |
| ER-2 manifest and signature verification | DONE (live manifest/signature SATISFIED: BLOCKED, owners) | ER-2 |
| ER-3 console read models | DONE | ER-3 |
| ER-4 API-3 | DONE | ER-4 |
| ER-5 edge, identity, observability | DONE | ER-5 |
| ER-6 contract and docs | DONE | ER-6 |
| ER-7 live container | DONE | ER-7 |
| ER-8 dependency E2E | DONE (84/84) | ER-8 |
| ER-FE-1 API client | DONE | ER-FE-1 |
| ER-FE-2 console v2.0 | DONE | ER-FE-2 |
| ER-9 evidence and hand-back | DONE | ER-9 |

## Verification evidence

How the suite runs on this host (Windows Application Control blocks host test binaries):

```
docker network create evreq-verify
docker run -d --rm --name evreq-test-pg --network evreq-verify -e POSTGRES_PASSWORD=postgres \
  -e POSTGRES_DB=evidence_requirements_test postgres:16-alpine
docker run --rm --network evreq-verify -e GOFLAGS=-mod=mod -e GOPROXY=off -e CGO_ENABLED=0 \
  -e TEST_DATABASE_URL=postgres://postgres:postgres@evreq-test-pg:5432/evidence_requirements_test?sslmode=disable \
  -v D:/zoikocomp/zoiko-suite-backend-platform/services:/src -v C:/Users/kaman/go/pkg/mod:/go/pkg/mod \
  -w /src/evidence-requirements-svc golang:1.26-alpine sh -c 'go test -count=1 ./... && go test -count=1 -tags=integration ./internal/store/'
```

### Suite counts (5 Nov, top-level tests)

| Package | Pass | Fail | Skip |
|---|---|---|---|
| internal/handler | 72 | 0 | 0 |
| internal/store (real Postgres 16) | 24 | 0 | 0 |
| internal/store `-tags=integration` | 35 | 0 | 0 |
| internal/events | 5 | 0 | 0 |
| internal/manifest | 3 | 0 | 0 |
| internal/esignature | 3 | 0 | 0 |
| internal/registry | 1 | 0 | 0 |
| internal/telemetry | 1 | 0 | 0 |
| access-control-svc internal/store `-tags=integration` (embedded Postgres) | 47 | 0 | 0 |

`go build ./...`, `go vet ./...`, `go vet -tags=integration ./internal/store/` clean; `gofmt` clean
(`tr -d '\r' < f | gofmt -l`).

### ER-1 — grants (R5)
All nine actions the handlers check (`EVIDENCE_EVALUATE`, `_REQUIREMENT_READ/_CREATE/_RETIRE`,
`_EVALUATION_READ`, `_REQUEST_CREATE/_MANAGE/_SUBMIT_RESPONSE/_EVALUATE`) were already registered —
six by access-control 000008, three by 000021. The first execution's 000046 claimed to register six of
them, so its `ON CONFLICT DO NOTHING` changed nothing, its down migration deleted 000008's rows, and its
two tests failed on a database. Corrected (unpushed): 000046 now fills the six empty descriptions and
publishes the templates (COMPLIANCE_CATALOGUE_ADMINISTRATOR v2, EVIDENCE_REQUEST_MANAGER v1,
OBLIGATION_MANAGER v2); its down withdraws exactly that. Tests: `TestR5_EvidenceActionsRegistered`,
`TestR5_EvidenceTemplateGrantsEveryAction` (access-control store suite 47/47). `seed-demo-rbac.ps1`
EVIDENCE_FULL carries all nine; the unprivileged E2E subject holds none (E2E §10). Applied to the local
`access_control` (56/56).

### ER-2 — artifacts verified with their owner
`internal/manifest/client.go`, `internal/esignature/client.go` (envelope forwarded; existence in the
caller's tenant and entity; any non-200/404 → unavailable). Unconfigured locally → 503, never counted.
Owner-specific 503 codes and `evidence_owner_unavailable_total{owner}`. Tests: `TestER02_*` (9),
`TestVerifyManifest*` / `TestVerifyEnvelope*` (6). Live: E2E §6–§8.

### ER-3 — console read models (API-4)
`internal/handler/handler_lists.go`, `internal/store/pg_lists.go`. Three lists with `filter[...]`,
`page_size`, signed cursor (`CURSOR_SIGNING_KEY_HEX`); summary in one transaction, 503 rather than a
partial answer. Breaking change recorded (D-ER-22): the two existing lists now return pages; the console
is the only caller. Tests: `TestER03_*` (6), `TestRLS_ListsNeverCrossTenant` (NOBYPASSRLS role).

### ER-4 — API-3
Migration 000010 (`record_version`, CHECK ≥ 1). One compare-and-set per command
(`pg_store.go:299`, `pbc_store.go:148`); 428 / 412 (+ current_version, ETag) / 400 / `*`; ETag on reads
and command responses; the rule for append-only actions in D-ER-21. Up/down/up of 000010 and 000011 on a
scratch database: clean. Tests: `TestAPI3_*` (handler 9 + store 2).

### ER-5 — edge, identity, observability
compose: route `evidence-requirements` on websecure with `gateway-auth@file` (D-ER-28),
`OTEL_EXPORTER_OTLP_ENDPOINT`, owner URLs, DB role. identity-envelope's warn mode was read: it admits a
request with no envelope and refuses an invalid or revoked one — no defect there. Tests:
`TestER05_*` (real gate, fake introspector, signed envelopes; `/metrics` series). Alert rules
`EvidenceOwnerUnavailable`, `EvidenceOutboxBacklog` in `deployments/prometheus-rules.yml`.

### ER-6 — contract and docs
openapi.yaml 1.4.0 (kin-openapi + parity); asyncapi.yaml 1.1.0 rewritten from a real Kafka record (the
first version modelled the envelope as Kafka headers; the record has none); RUNBOOK.md,
RELEASE_CERTIFICATE.md, SPEC_DEVIATIONS.md (D-ER-6/21–24 corrected, D-ER-26–28 added),
`scripts/audit.sh` (rewritten: the first version could not fail), Postman regenerated from openapi,
context.md §4/§9/§12. Schema registration in schema-registry-svc: not done (follow-up).

### ER-7 — live container
`docker compose -p deployments build evidence-requirements-svc` → image 5 Nov 13:26 (the previous one
was 1 Sep). `migrate.py up --db evidence_requirements` (000009–000011) and `--db access_control`.
Defects found only live, all fixed:
1. compose connected as `zoiko_app`, which has no CONNECT on `evidence_requirements` → now
   `app_evidence_requirements` (create-app-roles.sh).
2. The outbox refused every event for that role (`new row violates row-level security policy for table
   "outbox"`, verified before the fix) → migration 000011.
3. The Traefik route could never be reached (web entrypoint redirects first) → websecure.

`scripts/audit.sh` live (`GO_TEST_IN_DOCKER=1`):

```
===================================================================
 AUDIT — evidence-requirements-svc
 2026-11-05T08:27:36Z   service: http://127.0.0.1:8130
===================================================================

-- 1. Static analysis ----------------------------------------------
  PASS  go build ./... clean
  PASS  go vet ./... clean
  PASS  go vet -tags=integration (store) clean
  PASS  gofmt clean
  PASS  every migration has a .down.sql
  PASS  no /cursor routes in the contract
  PASS  asyncapi.yaml present
  PASS  RUNBOOK.md present
  PASS  RELEASE_CERTIFICATE.md present
  PASS  SPEC_DEVIATIONS.md present
  PASS  asyncapi.yaml lists both events

-- 2. Live health ---------------------------------------------------
  PASS  GET /healthz (200)
  PASS  GET /readyz (200)
  PASS  /metrics serves http_requests_total
  PASS  /metrics serves readiness_up
  PASS  /metrics serves evidence_evaluations_total
  PASS  /metrics serves evidence_owner_unavailable_total
  PASS  /metrics serves evidence_outbox_unpublished

-- 3. Allow path (a real envelope) --------------------------------
  PASS  envelope minted by identity-context
  PASS  summary with the verified envelope (200)
  PASS  evaluate an ungated action (200)
  PASS    …outcome (NO_REQUIREMENTS_DEFINED)

-- 4. Refusals ------------------------------------------------------
  PASS  no tenant (401)
  PASS  no principal (401)
  PASS  tampered envelope (warn mode too) (401)
  PASS  another tenant named in the query (403)
  PASS  unknown filter (400)
  PASS  bare (pre-API-4) filter (400)
  PASS  tampered cursor (400)
  PASS  page_size above 200 (400)
  PASS  a legal entity not in the tenant (422)
  PASS  body entity disagrees with X-Legal-Entity-Id (400)
  PASS  same Idempotency-Key, different action (409)
  PASS  body over 256 KiB (413)
  PASS  create a requirement (201)
  PASS  end-date without If-Match (428)
  PASS  end-date with a stale If-Match (412)
  PASS  end-date with the current If-Match (200)
  PASS  end-date again (already retired) (422)
  PASS  create into another tenant (403)

-- 5. Problem documents -------------------------------------------
  PASS  a refusal is application/problem+json (application/problem+json)

-- 6. Contract -----------------------------------------------------
  PASS  openapi.yaml valid and matches the router

===================================================================
 42 passed, 0 failed
===================================================================
```

### ER-8 — dependency E2E
`python3.14 deployments/scripts/e2e-evidence-requirements-deps.py --with-outage` on the stack: postgres,
kafka, redis, identity-context, tenant-entity-registry, access-control, mtls-management, authorization,
document-vault, evidence-manifest, gateway-auth + docker-proxy + Traefik, carta-svc, otel-collector,
jaeger, evidence-requirements. Two dependency defects outside this service were worked around for the
run with a scratchpad compose override (not committed) and are listed as follow-ups: document-vault's DB
role, gateway-auth's admin-token mount. Full stdout:

```
PASS  health evidence-requirements-svc
PASS  health authorization-svc
PASS  health identity-context-svc
PASS  health tenant-entity-registry-svc
PASS  health access-control-svc
PASS  health document-vault-svc
PASS  health evidence-manifest-svc
PASS  health gateway-auth-svc
PASS  health traefik (zoiko-gateway)
PASS  health jaeger
PASS  access-control: every evidence action is a registered permission  — 9 actions
PASS  identity: password login resolved to a signed envelope
PASS  identity: the verified envelope is admitted  — HTTP 200 assertion=None
PASS  identity: a tampered envelope is 401 (warn mode too)  — HTTP 401 identity_assertion_invalid
PASS  identity: second session for the revocation check  — no session_context_id
PASS  identity: the second session is admitted before invalidation  — HTTP 200
PASS  identity: session invalidated
PASS  identity: the invalidated session is refused within the revocation TTL  — HTTP 401 session_revoked; lag 5.4s
PASS  edge: no bearer is 401 from the gateway  — HTTP 401 X-Auth-Denial-Reason=no_token
PASS  edge: the verified envelope reaches the service through Traefik
PASS  edge: an evaluation with forged X-Tenant-Id / X-Principal-Id is served  — HTTP 200 {"evaluation_id": "a69dce20-52fa-4317-bae7-bdff75cdafe6", "outcome": "NO_REQUIREMENTS_DEFINED", "unmet": [], "evaluated_at": "2026-11-05T08:23:09.912151274Z", "correlation_id": "e2e-26f4141f", "domain_code": "E2E", "acti
PASS  edge: the ledger row carries the verified tenant and principal, not the forged ones  — 11111111-1111-1111-1111-111111111111|33333333-3333-3333-3333-333333333333
PASS  core: a fresh action_type is NO_REQUIREMENTS_DEFINED  — HTTP 200 NO_REQUIREMENTS_DEFINED
PASS  core: requirement created (201, ETag v1)  — HTTP 201 ETag="v1" 
PASS  core: requirement replay is 200 with the same id  — HTTP 200
PASS  core: same key, different requirement is 409 idempotency_mismatch  — HTTP 409 idempotency_mismatch
PASS  core: SATISFIED with the required artifact  — HTTP 200 {"evaluation_id": "69f07d48-443f-4cd3-b81e-47bc97df00e9", "outcome": "SATISFIED", "unmet": [], "evaluated_at": "2026-11-05T08:23:10.714650621Z", "correlation_id": "e2e-b11a3f77", "domain_code": "E2E", "action_type": "E2E
PASS  core: evaluate replay is 200 with the same evaluation_id  — HTTP 200 69f07d48-443f-4cd3-b81e-47bc97df00e9
PASS  core: same key, different action is 409 idempotency_mismatch  — HTTP 409 idempotency_mismatch
PASS  core: an empty reference does not count (MISSING)  — HTTP 200 MISSING
PASS  registry: a legal entity of another tenant is refused (422 legal_entity_not_found)  — HTTP 422 legal_entity_not_found
PASS  registry: a random legal entity is refused (422 legal_entity_not_found)  — HTTP 422 legal_entity_not_found
PASS  registry: neither refusal wrote a ledger row  — before=36 after=36
SKIP  document-vault: a document uploaded through its API  — the vault refuses every upload without clamd, and compose runs none (BLOCKED: owner environment); a fixture row is used instead
PASS  document-vault: fixture document in the demo entity  — insert failed
PASS  document-vault: the document is verified with the vault — SATISFIED  — HTTP 200 {"evaluation_id": "4e79439a-3633-44e6-bf88-7f52e81dfadf", "outcome": "SATISFIED", "unmet": [], "evaluated_at": "2026-11-05T08:23:12.748481018Z", "correlation_id": "e2e-08d2dc87", "domain_code": "E2E", "action_type": "E2E
PASS  document-vault: a random id is MISSING with the reason  — HTTP 200 MISSING requires 1 matching artifact(s), 0 present; an offered artifact did not count: referenced document does not exist in doc
PASS  document-vault: one document twice against minimum_count 2 is MISSING  — HTTP 200 MISSING
PASS  document-vault: a document of another legal entity is MISSING  — HTTP 200 MISSING requires 1 matching artifact(s), 0 present; an offered artifact did not count: referenced document belongs to a differen
PASS  document-vault outage: 503 document_service_unavailable  — HTTP 503 document_service_unavailable
PASS  document-vault outage: no ledger row written  — before=40 after=40
PASS  evidence-manifest: an unknown manifest is MISSING with the reason  — HTTP 200 {"evaluation_id": "5bdb8dae-660a-48fc-9045-1993e21b79a2", "outcome": "MISSING", "unmet": [{"evidence_requirement_id": "0e240472-718f-4574-ac16-77ac0a19594a", "evidence_type": "EVIDENCE_MANIFEST", "reason": "requires 1 ma
PASS  evidence-manifest: a manifest exists for the demo entity  — 234f31a1-6280-4523-b261-ace9dc8f5465
PASS  evidence-manifest: an owner that refuses the read is 503, never counted  — HTTP 503 evidence_manifest_unavailable
PASS  evidence-manifest: the refusal wrote no ledger row  — before=41 after=41
SKIP  evidence-manifest: a real manifest counts (SATISFIED)  — EVIDENCE_MANIFEST_READ is not registered in access-control, so evidence-manifest-svc refuses every read (BLOCKED: owner R5)
PASS  esignature unreachable: a SIGNATURE reference is 503 esignature_unavailable, never counted  — HTTP 503 esignature_unavailable
PASS  esignature unreachable: no ledger row written  — before=41 after=41
SKIP  esignature: a real envelope counts / an unknown one is MISSING  — esignature-integration-svc runs in the phase7 stack (its own postgres-p7/kafka-p7 network), not up beside this stack (BLOCKED)
PASS  authz: the unprivileged subject is refused catalogue create (403)  — HTTP 403 authorization_denied
PASS  authz: the unprivileged subject is refused the catalogue read (403)  — HTTP 403 authorization_denied
PASS  authz: the unprivileged subject is refused the evaluation read (403)  — HTTP 403 authorization_denied
PASS  authz: the unprivileged subject is refused the PBC register (403)  — HTTP 403 authorization_denied
PASS  authz: evaluate is admitted for the subject in warn mode  — HTTP 200 None
PASS  authz: the decision log records the warn-mode DENY for EVIDENCE_EVALUATE  — DENIED
PASS  pbc: request created (201, ETag v1)  — HTTP 201 "v1" 
PASS  pbc: send without If-Match is 428  — HTTP 428 precondition_required
PASS  pbc: send with the current If-Match (v2)  — HTTP 200 "v2" None
PASS  pbc: a stale If-Match is 412 with the current ETag  — HTTP 412 precondition_failed ETag="v2"
PASS  pbc: extend due date (v3)  — HTTP 200 "v3" None
PASS  pbc: the assignee submits a response (no If-Match)  — HTTP 201 {"response_id": "616375df-8a9a-43c0-b274-11da584ba583", "request_id": "6b2035ea-b56b-40be-928c-69f3b8babdbe", "version": 1, "submitted_by_principal_id": "66666666-6666-6666-6666-666666666666", "malware_scan_status": "PEN
PASS  pbc: the response moved the request and bumped its version  — ETag="v4"
PASS  pbc: the submitter cannot mark it satisfied (403 self_evaluation_not_allowed)  — HTTP 403 self_evaluation_not_allowed
PASS  pbc: mark satisfied before a clean scan is 422 artifact_not_scanned  — HTTP 422 artifact_not_scanned
PASS  pbc: a scan result from a principal without EVIDENCE_REQUEST_MANAGE is 403  — HTTP 403 authorization_denied
PASS  pbc: scan result CLEAN recorded by a manager  — HTTP 200 None
PASS  pbc: marked satisfied by another principal (v5)  — HTTP 200 SATISFIED "v5" None
PASS  pbc: request closed (undo)  — HTTP 200 None
PASS  lists: first page of evaluations with a next cursor  — HTTP 200 {"items": [{"evaluation_id": "281de939-5409-42e9-a70c-06f857140f34", "tenant_id": "11111111-1111-1111-1111-111111111111"
PASS  lists: the next page continues without repeating  — HTTP 200
PASS  lists: a tampered cursor is 400 invalid_cursor  — HTTP 400 invalid_cursor
PASS  lists: an unknown filter is 400 unknown_filter  — HTTP 400 unknown_filter
PASS  lists: the summary reads  — HTTP 200 {"active_requirements": 5, "evaluations_satisfied_30d": 9, "evaluations_missing_30d": 19, "evaluations_no_requirements_30d": 14, "actions_with_no_requirements_3
PASS  kafka: topic readable  — 29 keys
PASS  kafka: SATISFIED 69f07d48 published exactly once, key <tenant>/<evaluation_id>  — 1 message(s)
PASS  kafka: MISSING a36f6c0f published exactly once, key <tenant>/<evaluation_id>  — 1 message(s)
PASS  kafka: SATISFIED 4e79439a published exactly once, key <tenant>/<evaluation_id>  — 1 message(s)
PASS  kafka: MISSING 1995b5bd published exactly once, key <tenant>/<evaluation_id>  — 1 message(s)
PASS  kafka: MISSING 83c424c7 published exactly once, key <tenant>/<evaluation_id>  — 1 message(s)
PASS  kafka: MISSING 1c9ce6b4 published exactly once, key <tenant>/<evaluation_id>  — 1 message(s)
PASS  kafka: MISSING 5bdb8dae published exactly once, key <tenant>/<evaluation_id>  — 1 message(s)
PASS  kafka: NO_REQUIREMENTS_DEFINED and replays published nothing  — stray=[]
PASS  outbox: every row of this run is published  — 0/7
PASS  metrics: evidence_evaluations_total is served
PASS  metrics: evidence_owner_unavailable_total is served
PASS  metrics: evidence_outbox_unpublished is served
PASS  tracing: evidence-requirements-svc traces in Jaeger

84/84 passed, 3 skipped
  skipped: document-vault: a document uploaded through its API — the vault refuses every upload without clamd, and compose runs none (BLOCKED: owner environment); a fixture row is used instead
  skipped: evidence-manifest: a real manifest counts (SATISFIED) — EVIDENCE_MANIFEST_READ is not registered in access-control, so evidence-manifest-svc refuses every read (BLOCKED: owner R5)
  skipped: esignature: a real envelope counts / an unknown one is MISSING — esignature-integration-svc runs in the phase7 stack (its own postgres-p7/kafka-p7 network), not up beside this stack (BLOCKED)
```

### ER-FE-1 — API client (`zoiko-suite-frontend-platform/lib/api/evidence.ts`)
Lists on API-4 pages (`listEvidenceRequirements`, `listEvaluations`, `listEvidenceRequests`, `readAll`),
the new summary shape, `retireEvidenceRequirement` and `commandEvidenceRequest` send If-Match through
`lib/api/envelope.ts`, `createEvidenceRequest`, `recordScanResult`, verified-by map for the three owned
types, `explainEvidenceError` for 412/428/owner-unavailable/idempotency_mismatch/legal_entity_*/
invalid_identifier/self-evaluation/scan. `app/api/v1/[...path]/route.ts` follows the new list.

### ER-FE-2 — `/admin/evidence` in v2.0
`app/admin/evidence/{page,actions,state}.ts(x)`, `components/admin/evidence/console/{data,shared,
overview,requirements,evaluations,pbc,check,forms,problem}`; reuses `RegisterView` and `ActionDialog`
(reason, step-up retry). Header: "Evidence Requirements", gold "Add requirement", secondary "Check
evidence". KPIs from `/v1/evidence/summary`; panels: gate pipeline, evidence types with verified-by chip,
catalogue coverage with the ungated-actions callout; tabs Requirements / Evaluations / Missing / Evidence
requests / Check evidence; drawer commands send the version read (If-Match); scan shown only to
EVIDENCE_REQUEST_MANAGE holders. Old `CatalogPanel`/`EvidenceForms`/`EvidenceLookups`/`EvidenceSummary`
deleted. `npx tsc --noEmit`: no error in touched files (pre-existing errors remain in `tests/e2e` and
`vitest.config.mts`); `npx eslint` on touched files: clean. Rendered against the live service with
headless Chrome (every tab; an empty state — a search matching nothing; an error state — the service
stopped: every KPI "—  Unavailable", panels and register name the outage, never 0).

### ER-9 — mutation check (scratch copy, `go test ./internal/handler/`)

| Guard broken | Failing test |
|---|---|
| distinct-artifact key (duplicates counted) | TestEvaluate_TheSameDocumentInAnotherCaseCountsOnce, TestER02_SameManifestTwiceCountsOnce |
| owner unavailable (artifact counted instead of 503) | TestEvaluate_DocumentServiceUnavailable_Returns503 |
| replay fingerprint (action_type ignored) | TestEvaluate_IdempotencyMismatch_Returns409 |

Audit files updated: `IMPLEMENTATION-PROGRESS.md` (API-3, API-4), `group2-governance-platform-testing.md`
(status, dated block, summary row), `Governance Platform-audit-2026-09-28.md` (re-score section),
`domain1-domain2-features-and-requirements.md` (D2-7).

## BLOCKED (external)

| Item | Owner | What unblocks it |
|---|---|---|
| A real evidence manifest counting | evidence-manifest-svc / access-control | Register `EVIDENCE_MANIFEST_READ` and put it in a template; until then every manifest reference fails closed (503) |
| SIGNATURE live | esignature-integration-svc | Run it beside the main stack (it is phase7-only) |
| Document upload through the API in the E2E | document-vault-svc / platform | A clamd service in compose (the vault refuses uploads without one) |
| Caller gates: general-ledger post, financial-close lock, vat-gst, corporate-tax | their teams | Call evaluate before finalizing; only SATISFIED passes |
| Full envelope from board-resolutions, corporate-actions, filing-preparation | their teams | Forward X-Request-Id, X-Source-Channel, Idempotency-Key, X-Legal-Entity-Id and the bearer |
| Allow-list in corporate-actions, filing-preparation | their teams | Treat only SATISFIED as a pass (today any outcome but MISSING passes) |
| Production mTLS to authorization-svc | platform | `AUTHZ_MTLS_ENABLED=true` not exercised in this pass |

## Cross-team follow-ups

- **evidence-manifest-svc**: `EVIDENCE_MANIFEST_READ` (and its other actions) are not registered in
  access-control; every read is DENIED `no_grant`.
- **document-vault-svc**: compose `DB_USER zoiko_app` cannot connect (CONNECT is granted only to
  `app_document_vault`); uploads need clamd, which compose does not run.
- **gateway-auth-svc**: compose mounts the `gateway_admin_token` volume at the path
  `GATEWAY_ADMIN_TOKEN_FILE` names, so the "file" is a directory and the service exits at start.
- **general-ledger-svc, financial-close-svc, vat-gst-svc, corporate-tax-svc**: add the evidence gate.
- **board-resolutions-svc, corporate-actions-svc, filing-preparation-svc**: forward the full envelope;
  only SATISFIED passes.
- **schema-registry-svc**: register the two event schemas (asyncapi.yaml).
- **this service, next pass**: authorize `GET /v1/pbc/requests/{id}` at the request's entity (Governance
  row 21); switch compose gates to enforce once callers send the envelope (D-ER-6).
