# evidence-requirements-svc — Progress

**Status (5 Nov 2026):** ER-0 baseline and feature matrix committed. Suite green (handler 45, events 5, registry 1, store 18 skipped without TEST_DATABASE_URL). gofmt and go vet clean.

---

## Feature Matrix

| ID | Requirement | Route / Consumer | Test | Console Screen | Status |
|---|---|---|---|---|---|
| **Doc 03 §8.6 — Purpose & Owned Concerns** |
| ER-03-1 | Purpose: decide what evidence must exist before an action completes | POST /v1/evidence/evaluate | TestEvaluate_NoRequirementsDefined, TestEvaluate_Satisfied, TestEvaluate_Missing_NamesEachUnmetRequirement | Evidence Check tab | DONE |
| ER-03-2 | Owns evidence preconditions (catalogue) | GET/POST /v1/evidence-requirements, GET /v1/evidence-requirements/{id}, POST /v1/admin/evidence-requirements/{id}/end-date | TestCreateRequirement_*, TestEndDate_*, TestListRequirements_* | Requirements tab | DONE |
| ER-03-3 | Owns document requirements & verifies document evidence | POST /v1/evidence/evaluate → documentvault.VerifyDocument | TestEvaluate_UnverifiableDocument_DoesNotCount, TestEvaluate_DocumentServiceUnavailable_Returns503 | Check Evidence drawer | BUILT-THIS-PASS (ER-2) |
| ER-03-4 | Owns signature requirements | POST /v1/evidence/evaluate (SIGNATURE type) | TestER02_SignatureUnknownIsUnmet, TestER02_SignatureServiceDownIs503NoRow | Evidence types panel | BLOCKED: esignature-integration-svc exposes no scoped read (ER-2) |
| ER-03-5 | Owns supporting artifact rules (subtype, minimum_count) | POST /v1/evidence/evaluate (RequirementSpec) | TestEvaluate_MinimumCountEnforced, TestEvaluate_ArtifactSubtypeMismatch | Evidence types panel | DONE |
| ER-03-6 | Owns evidence sufficiency logic; fail-closed on malformed rule | POST /v1/evidence/evaluate | TestEvaluate_UnreadablePayload_FailsClosed | Check Evidence drawer | DONE |
| ER-03-7 | Critical constraint: no finalization path may skip required evidence states | (caller-side wiring) | E2E §4, §6, §11 | Gate pipeline panel | BLOCKED: callers outside domain (ER-8) |
| ER-03-8 | Published event: evidence.requirement.missing | Outbox → Kafka topic zoiko.evidence-requirements.events | TestPublishEvaluation_Missing_CarriesUnmetDetail, E2E §9 | Gate pipeline panel | DONE (wired), BUILT-THIS-PASS (live proof ER-7/8) |
| ER-03-9 | Published event: evidence.requirement.satisfied | Outbox → Kafka topic zoiko.evidence-requirements.events | TestPublishEvaluation_Satisfied, E2E §9 | Gate pipeline panel | DONE (wired), BUILT-THIS-PASS (live proof ER-7/8) |

| **Doc 04 §7.1 — EvidenceRequirement Attributes** |
| ER-04-1 | evidence_requirement_id (UUID, PK) | migration 000001 | TestPgStore_CreateRequirement_And_GetRequirement | — | DONE |
| ER-04-2 | tenant_id (UUID, NOT NULL) | migration 000001 | TestPgStore_GetRequirement_OtherTenant_ReadsAsAbsent | — | DONE |
| ER-04-3 | legal_entity_id (UUID, nullable — NULL = tenant-wide) | migration 000001 | TestPgStore_EffectiveRequirements_ScopeAndPointInTime | Requirements tab (entity filter) | DONE (decision D-ER-4 recorded) |
| ER-04-4 | domain_code (VARCHAR) | migration 000001 | TestPgStore_EffectiveRequirements_ScopeAndPointInTime | Requirements tab (domain filter) | DONE |
| ER-04-5 | action_type (VARCHAR) | migration 000001 | TestPgStore_EffectiveRequirements_ScopeAndPointInTime | Requirements tab (action filter) | DONE |
| ER-04-6 | evidence_type (VARCHAR) | migration 000001 | TestEvaluate_ArtifactSubtypeMismatch | Evidence types panel | DONE |
| ER-04-7 | requirement_payload (JSONB: minimum_count, artifact_subtype, description) | migration 000001 | TestCreateRequirement_PayloadMustMatchTheRuleShape, TestEvaluate_MinimumCountEnforced | Requirements drawer | DONE |
| ER-04-8 | effective_from (TIMESTAMPTZ) | migration 000001 | TestPgStore_EffectiveRequirements_ScopeAndPointInTime | Requirements tab (as_of) | DONE |
| ER-04-9 | effective_to (TIMESTAMPTZ, nullable) | migration 000001 | TestPgStore_EndDateRequirement_RetiresWithoutDeleting | Requirements drawer (end-date) | DONE |
| ER-04-10 | created_at, created_by_principal_id | migration 000001 | TestPgStore_CreateRequirement_And_GetRequirement | Requirements drawer | DONE |
| ER-04-11 | correlation_id (idempotency) | migration 000001 | TestPgStore_CreateRequirement_RetriedCorrelationID_IsIdempotent | — | DONE |
| ER-04-12 | Unique: (tenant_id, domain_code, action_type, evidence_type, effective_from) | migration 000001 | TestPgStore_CreateRequirement_RetriedCorrelationID_IsIdempotent | — | DONE |
| ER-04-13 | Retirement by effective end-dating only; no DELETE | migration 000001 + handler EndDateRequirement | TestEndDate_AlreadyRetired_Returns422, TestPgStore_EndDateRequirement_RetiresWithoutDeleting | Requirements drawer (end-date) | DONE |

| **Governance Audit 7/7 — 43 Scored Rows (19 Oct status alongside)** |
| GA-1 | Purpose implemented | — | TestEvaluate_NoRequirementsDefined | — | ✅ / DONE |
| GA-2 | Document requirements verified | — | TestEvaluate_UnverifiableDocument_DoesNotCount | — | ❌ / BUILT-THIS-PASS (ER-2) |
| GA-3 | Signature requirements owned | — | (stub) | — | ⚠️ / BLOCKED (ER-2) |
| GA-4 | Supporting artifact rules (subtype, min count) | — | TestEvaluate_MinimumCountEnforced, TestEvaluate_ArtifactSubtypeMismatch | — | ✅ / DONE |
| GA-5 | Sufficiency logic; malformed rule fails closed | — | TestEvaluate_UnreadablePayload_FailsClosed | — | ✅ / DONE |
| GA-6 | Sufficiency counts distinct artifacts | — | TestER02_SameManifestTwiceCountsOnce (new) | — | ❌ / BUILT-THIS-PASS (ER-2) |
| GA-7 | Non-document artifacts taken on caller's word | — | TestER02_UnconfiguredOwnerNeverCounts (new) | — | ⚠️ / BLOCKED: owners expose no scoped read (ER-2) |
| GA-8 | Document statuses counted | — | (not specified) | — | ❓ / D-ER-3 recorded |
| GA-9 | Critical constraint: no path skips evidence | — | E2E §4, §6, §11 | — | ❌ / BLOCKED (caller-side) |
| GA-10 | Callers reach gate under envelope | — | E2E §2, §3 | — | ❌ / BUILT-THIS-PASS (ER-5) |
| GA-11 | Callers treat only SATISFIED as pass | — | E2E §11 | — | ❌ / BLOCKED (caller-side) |
| GA-12 | NO_REQUIREMENTS_DEFINED semantics | — | TestEvaluate_NoRequirementsDefined | — | ❓ / D-ER-5 recorded |
| GA-13 | Event evidence.requirement.missing | — | TestPublishEvaluation_Missing_CarriesUnmetDetail | — | ✅ / DONE |
| GA-14 | Event evidence.requirement.satisfied | — | TestPublishEvaluation_Satisfied | — | ✅ / DONE |
| GA-15 | Event envelope (causation, jurisdiction, governing refs) | — | TestEvaluate_EventCarriesTheGoverningReferences | — | ⚠️ / BUILT-THIS-PASS (ER-6) |
| GA-16 | Outbox publication after commit | — | TestPgStore_RecordEvaluation_RoundTripAndIdempotency | — | ❌ / BUILT-THIS-PASS (ER-6/7) |
| GA-17 | Idempotency-Key mandatory; IDEMPOTENCY_MISMATCH | — | TestEvaluate_IdempotencyMismatch_Returns409, TestCreateRequirement_IdempotencyMismatch_Returns409 | — | ❌ / BUILT-THIS-PASS (ER-4) |
| GA-18 | Idempotent requirement create | — | TestCreateRequirement_Idempotent_ReplayReturns200 | — | ✅ / DONE |
| GA-19 | Catalogue mutation authorized; fail-closed | — | TestCreateRequirement_CallsAuthorization, TestCreateRequirement_AuthzUnavailable_FailsClosed | — | ✅ / DONE |
| GA-20 | Legal entity validated by server | — | TestEvaluate_LegalEntityMustExistInTheTenant | — | ❌ / BUILT-THIS-PASS (ER-1/ER-5) |
| GA-21 | Read contracts strongly scoped | — | TestReads_NeedAPrincipalAndAGrantAtTheEntity | — | ⚠️ / BUILT-THIS-PASS (ER-3/ER-4) |
| GA-22 | tenant_id from gateway, never trusted | — | E2E §3 | — | ⚠️ / BUILT-THIS-PASS (ER-5) |
| GA-23 | Canonical service input contract enforced | — | TestAPI2_EnvelopeRefusalIsAProblem | — | ✅ / DONE |
| GA-24 | Server-resolved context (tenant/entity) | — | TestEvaluate_AVerifiedPeerWorkloadIsTheCaller | — | ⚠️ / BUILT-THIS-PASS (ER-5) |
| GA-25 | Missing tenant → 401 | — | TestEvaluate_MissingTenant_Returns401 | — | ⚠️ / DONE |
| GA-26 | Workload identity accepted | — | TestEvaluate_AVerifiedPeerWorkloadIsTheCaller | — | ⚠️ / DONE |
| GA-27 | Stable error classes (RFC 9457) | — | TestAPI2_ErrorResponsesAreProblemDetails | — | ⚠️ / BUILT-THIS-PASS (ER-6) |
| GA-28 | evaluation_id returned | — | TestEvaluate_Satisfied | — | ✅ / DONE |
| GA-29 | EvidenceRequirement attributes complete | — | TestPgStore_CreateRequirement_And_GetRequirement | — | ✅ / DONE |
| GA-30 | Nullable legal_entity_id on requirement | — | TestPgStore_EffectiveRequirements_ScopeAndPointInTime | — | ❓ / D-ER-4 recorded |
| GA-31 | Retirement by end-dating; no delete | — | TestPgStore_EndDateRequirement_RetiresWithoutDeleting | — | ✅ / DONE |
| GA-32 | Effective interval validity | — | TestPgStore_EffectiveRequirements_ScopeAndPointInTime | — | ⚠️ / DONE |
| GA-33 | Requirement payload validated at create | — | TestCreateRequirement_PayloadMustMatchTheRuleShape | — | ⚠️ / DONE |
| GA-34 | Evaluation ledger append-only/immutable | — | TestPgStore_EvaluationLedgerIsAppendOnly | — | ⚠️ / DONE (trigger guard 000009) |
| GA-35 | Decision reproducible (governing versions retained) | — | TestEvaluate_EventCarriesTheGoverningReferences | — | ⚠️ / BUILT-THIS-PASS (ER-6) |
| GA-36 | RLS FORCE + NULLIF | — | TestPgStore_GetRequirement_OtherTenant_ReadsAsAbsent | — | ✅ / DONE |
| GA-37 | Explicit tenant predicate + non-superuser role | — | TestPgStore_GetRequirement_OtherTenant_ReadsAsAbsent | — | ✅ / DONE |
| GA-38 | Unknown/malformed id → 404 not 500 | — | TestPgStore_MalformedUUID_ReadsAsAbsent | — | ✅ / DONE |
| GA-39 | Malformed ids on write paths → 400 | — | TestPgStore_MalformedUUID_ReadsAsAbsent | — | ⚠️ / DONE |
| GA-40 | Request body cap (256 KiB) | — | (handler.go:1109) | — | ✅ / DONE |
| GA-41 | Health, readiness, metrics, tracing | — | (main.go healthcheck) | — | ✅ / DONE |
| GA-42 | Bounded authz decision cache | — | (authz/client.go) | — | ✅ / DONE |
| GA-43 | API endpoints in spec | — | TestOpenAPIMatchesTheRouter | — | ⚠️ / BUILT-THIS-PASS (ER-6) |

| **D2-7 Features (from domain1-domain2-features-and-requirements.md)** |
| D2-7-1 | evaluate: SATISFIED/MISSING; fails closed on malformed rule | POST /v1/evidence/evaluate | TestEvaluate_NoRequirementsDefined, TestEvaluate_Satisfied, TestEvaluate_Missing_NamesEachUnmetRequirement | Check Evidence tab | DONE |
| D2-7-2 | get evaluation | GET /v1/evidence/evaluations/{id} | TestGetEvaluation_NotFound_Returns404 | Evaluations drawer | DONE |
| D2-7-3 | requirement catalogue: list, get, create, end-date | GET/POST /v1/evidence-requirements, /v1/admin/evidence-requirements | TestCreateRequirement_*, TestEndDate_*, TestListRequirements_* | Requirements tab | DONE |
| D2-7-4 | supporting-artifact rules (subtype, min count) data-driven | RequirementSpec in payload | TestEvaluate_MinimumCountEnforced, TestEvaluate_ArtifactSubtypeMismatch | Evidence types panel | DONE |
| D2-7-5 | append-only decision ledger; outbox events | RecordEvaluation + Outbox | TestPublishEvaluation_Satisfied, TestPublishEvaluation_Missing_CarriesUnmetDetail | Gate pipeline panel | DONE (wired) |
| D2-7-6 | PBC requests (14 routes) | /v1/pbc/requests*, /v1/pbc/responses* | (pbc_store_test.go — skipped without DB) | PBC tab | BUILT-THIS-PASS (ER-4 API-3) |
| D2-7-7 | document verification against document-vault-svc | documentvault.VerifyDocument | TestEvaluate_UnverifiableDocument_DoesNotCount, TestEvaluate_DocumentServiceUnavailable_Returns503 | Check Evidence drawer | BUILT-THIS-PASS (ER-2) |
| D2-7-8 | signature verification | (stub) | TestER02_SignatureUnknownIsUnmet (new) | Evidence types panel | BLOCKED: esignature-integration-svc no scoped read |
| D2-7-9 | counting distinct artifacts | artifactKey deduplication | TestER02_SameManifestTwiceCountsOnce (new) | — | BUILT-THIS-PASS (ER-2) |
| D2-7-10 | non-document artifacts: empty reference_id counts; EVIDENCE_MANIFEST unchecked | checkArtifacts | TestER02_UnconfiguredOwnerNeverCounts (new) | — | BLOCKED: owners expose no scoped read |
| D2-7-11 | gate coverage for GL post, FC lock, VAT-GST, Corp Tax | (caller-side) | E2E §0, §4 | — | BLOCKED: caller-side (cross-team) |
| D2-7-12 | callers forward full envelope | (caller-side) | E2E §2, §3 | — | BLOCKED: caller-side (cross-team) |
| D2-7-13 | callers allow-list (only SATISFIED passes) | (caller-side) | E2E §11 | — | BLOCKED: caller-side (cross-team) |
| D2-7-14 | authorization on evaluate | EVIDENCE_EVALUATE | TestEvaluate_IsAuthorizedAtTheEntity | — | DONE |
| D2-7-15 | openapi.yaml | openapi.yaml | TestOpenAPIMatchesTheRouter, TestOpenAPIDocumentIsValid | — | BUILT-THIS-PASS (ER-6) |

| **PBC Routes (14 routes from openapi.yaml)** |
| PBC-1 | Create evidence request | POST /v1/pbc/requests | (store test skipped) | PBC tab (create drawer) | BUILT-THIS-PASS (ER-4) |
| PBC-2 | List open evidence requests | GET /v1/pbc/requests | (store test skipped) | PBC tab | BUILT-THIS-PASS (ER-3) |
| PBC-3 | Get evidence request | GET /v1/pbc/requests/{id} | (store test skipped) | PBC drawer | BUILT-THIS-PASS (ER-3) |
| PBC-4 | Send evidence request | POST /v1/pbc/requests/{id}/send | (store test skipped) | PBC drawer (send) | BUILT-THIS-PASS (ER-4) |
| PBC-5 | Reassign evidence request | POST /v1/pbc/requests/{id}/reassign | (store test skipped) | PBC drawer (reassign) | BUILT-THIS-PASS (ER-4) |
| PBC-6 | Extend due date | POST /v1/pbc/requests/{id}/extend-due-date | (store test skipped) | PBC drawer (extend) | BUILT-THIS-PASS (ER-4) |
| PBC-7 | View evidence request | POST /v1/pbc/requests/{id}/view | (store test skipped) | PBC drawer (view) | BUILT-THIS-PASS (ER-4) |
| PBC-8 | Submit response | POST /v1/pbc/requests/{id}/responses | (store test skipped) | PBC drawer (response) | BUILT-THIS-PASS (ER-4) |
| PBC-9 | Add note | POST /v1/pbc/requests/{id}/notes | (store test skipped) | PBC drawer (notes) | BUILT-THIS-PASS (ER-4) |
| PBC-10 | Request clarification | POST /v1/pbc/requests/{id}/clarify | (store test skipped) | PBC drawer (clarify) | BUILT-THIS-PASS (ER-4) |
| PBC-11 | Mark satisfied | POST /v1/pbc/requests/{id}/mark-satisfied | TestPgStore_MarkSatisfied_RefusesSelfEvaluation, TestPgStore_MarkSatisfied_RequiresCleanScan | PBC drawer (mark satisfied) | BUILT-THIS-PASS (ER-4) |
| PBC-12 | Close request | POST /v1/pbc/requests/{id}/close | (store test skipped) | PBC drawer (close) | BUILT-THIS-PASS (ER-4) |
| PBC-13 | Record scan result | POST /v1/pbc/responses/{id}/scan-result | (store test skipped) | PBC drawer (scan) | BUILT-THIS-PASS (ER-4) |
| PBC-14 | Acknowledge receipt | POST /v1/pbc/responses/{id}/acknowledge | (store test skipped) | PBC drawer (acknowledge) | BUILT-THIS-PASS (ER-4) |

| **context.md §11 Decisions** |
| D-ER-1 | Tier classification (Doc 03 §23 omits service) | — | — | — | RECORDED in SPEC_DEVIATIONS.md |
| D-ER-2 | Scan-result ownership (EVIDENCE_REQUEST_MANAGE interim) | — | TestPgStore_MarkSatisfied_RefusesSelfEvaluation | — | RECORDED in SPEC_DEVIATIONS.md |
| D-ER-3 | Which document and signature statuses count | — | — | — | RECORDED in SPEC_DEVIATIONS.md |
| D-ER-4 | Nullable legal_entity_id on requirement | migration 000001 | TestPgStore_EffectiveRequirements_ScopeAndPointInTime | — | DECIDED, BUILT (RECORDED) |
| D-ER-5 | NO_REQUIREMENTS_DEFINED caller contract (caller decides) | — | TestEvaluate_NoRequirementsDefined | — | DECIDED 21 Oct, RECORDED |
| D-ER-6 | Compose gate modes warn (evaluate, identity) and why | compose EVIDENCE_EVALUATE_AUTHZ=warn | — | — | RECORDED in SPEC_DEVIATIONS.md |

| **Plan Tasks (ER-0 … ER-9, ER-FE-1, ER-FE-2)** |
| ER-0 | Baseline and feature matrix (S0) | — | Suite counts recorded, matrix committed | — | **DONE (this commit)** |
| ER-1 | Grants by migration (R5) and demo seed | access-control migration + seed-demo-rbac.ps1 | TestR5_EvidenceActionsRegistered, TestR5_EvidenceTemplateGrantsEveryAction | — | PENDING |
| ER-2 | Verify manifest and signature references | internal/manifest/client.go, internal/esignature/client.go, handler.checkArtifacts | TestER02_ManifestOutsideTenantDoesNotCount, TestER02_ManifestUnknownIsUnmet, TestER02_ManifestServiceDownIs503NoRow, TestER02_SignatureUnknownIsUnmet, TestER02_SignatureServiceDownIs503NoRow, TestER02_SameManifestTwiceCountsOnce, TestER02_UnconfiguredOwnerNeverCounts | Evidence types panel | PENDING |
| ER-3 | Console read models (API-4 cursor paging) | GET /v1/evidence/evaluations, /v1/evidence-requirements, /v1/pbc/requests, /v1/evidence/summary | List tests: first page, next page, tampered cursor 400, foreign tenant cursor 400, unknown filter 400, RLS isolation | All tabs (registers) | PENDING |
| ER-4 | API-3 on mutable aggregates | record_version on requirements + PBC; ETag/If-Match on mutations | TestAPI3_EndDateNeedsIfMatch, TestAPI3_PBCCloseStaleIs412, etc. (3 per mutation) | All drawers (If-Match) | PENDING |
| ER-5 | Edge, identity and observability | Traefik labels, identity gate, revocation, metrics | E2E §2, §3; /metrics test | — | PENDING |
| ER-6 | Contract and docs (S2) | openapi.yaml, asyncapi.yaml, RUNBOOK.md, RELEASE_CERTIFICATE.md, SPEC_DEVIATIONS.md, scripts/audit.sh, Postman | kin-openapi, parity test, audit.sh live | — | PENDING |
| ER-7 | Live container (S3) | docker build, migrate, up -d, audit.sh live | progress.md totals | — | PENDING |
| ER-8 | Dependency E2E (S4) | deployments/scripts/e2e-evidence-requirements-deps.py | All mandatory checks PASS, stdout in progress.md | — | PENDING |
| ER-FE-1 | API client (lib/api/evidence.ts) | evaluation list, summary, cursor paging, PBC commands with ifMatch | tsc/eslint clean | Console tabs | PENDING |
| ER-FE-2 | /admin/evidence rebuilt in v2.0 | app/admin/evidence/page.tsx, components/admin/evidence/console/* | Headless Chrome screenshots (empty + error states) | All tabs rendered live | PENDING |
| ER-9 | Evidence and hand-back (S6) | IMPLEMENTATION-PLAN §7 evidence blocks, audit file updates, mutation check | Commit list, suite counts, E2E totals, BLOCKED items | — | PENDING |

---

## Suite Counts (5 Nov 2026)

| Package | Tests | Pass | Fail | Skip |
|---|---|---|---|---|
| internal/handler | 45 | 45 | 0 | 0 |
| internal/events | 5 | 5 | 0 | 0 |
| internal/registry | 1 | 1 | 0 | 0 |
| internal/store | 19 | 1 | 0 | 18 (TEST_DATABASE_URL not set) |

Store suite skip reason: `TEST_DATABASE_URL` not configured. The 1 passing test is `TestPgStore_EvaluationLedgerIsAppendOnly` which uses a unit-style check.

---

## Cross-team Follow-ups

- **general-ledger-svc**: add evidence gate before `POST /v1/journals/{journal_id}/post` (caller-side)
- **financial-close-svc**: add evidence gate before `POST /v1/close/periods/{id}/lock` (caller-side)
- **vat-gst-svc**: add evidence gate before return filing (caller-side)
- **corporate-tax-svc**: add evidence gate before return submission (caller-side)
- **board-resolutions-svc**: forward full identity envelope (X-Request-Id, X-Source-Channel, Idempotency-Key, X-Legal-Entity-Id) and treat only SATISFIED as pass
- **corporate-actions-svc**: forward full envelope; treat only SATISFIED as pass (currently deny-list fails open)
- **filing-preparation-svc**: forward full envelope; treat only SATISFIED as pass (currently deny-list fails open)
- **document-vault-svc**: grant DOCUMENT_READ to evidence-requirements-svc principal for evaluation callers
- **evidence-manifest-svc**: expose `GET /v1/evidence-manifests/{manifestID}` with tenant/entity scope for verification
- **esignature-integration-svc**: expose `GET /v1/esignature/envelopes/{id}` with scope for verification

---

## SPEC_DEVIATIONS Recorded (ER-0)

- D-ER-1: Tier classification — Doc 03 §23 omits this service; built to Tier 0 discipline but label unresolved
- D-ER-2: Scan-result ownership — EVIDENCE_REQUEST_MANAGE is interim; no dedicated scanner identity exists
- D-ER-3: Document/signature statuses that count — not specified; verify existence and scope only
- D-ER-4: Nullable legal_entity_id on EvidenceRequirement — NULL = tenant-wide; COALESCE to zero UUID for unique index
- D-ER-5: NO_REQUIREMENTS_DEFINED — distinct outcome, never SATISFIED; caller decides action
- D-ER-6: Compose gate modes warn — EVIDENCE_EVALUATE_AUTHZ=warn, IDENTITY_VERIFY=warn; callers outside governance domains send no bearer

---

## Verification Evidence (ER-0)

- `go build ./...` ✅
- `go vet ./...` ✅
- `gofmt -l` ✅ (no output)
- Handler suite: 45 pass, 0 fail, 0 skip
- Events suite: 5 pass, 0 fail, 0 skip
- Registry suite: 1 pass, 0 fail, 0 skip
- Store suite: 1 pass, 0 fail, 18 skip (TEST_DATABASE_URL not set)
- OpenAPI parity: `TestOpenAPIMatchesTheRouter` ✅, `TestOpenAPIDocumentIsValid` ✅
- Git status: clean (formatting changes from gofmt staged separately)

---

## Next: ER-1 — Grants by migration (R5) and demo seed