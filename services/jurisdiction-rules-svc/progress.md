# jurisdiction-rules-svc · progress (D2-1, Jurisdictions & Rules)

Service: jurisdiction-rules-svc · port 8082 · DB `jurisdiction_rules` · compose service `jurisdiction-svc`
(container `jurisdiction-rules-svc`). Console page (later): `/admin/jurisdictions`.

This file replaces the 11 Nov version, whose matrix was not checked against the code: it claimed RLS + FORCE
on every table (there is none), a negative-path suite that never sent a request body or asserted a result,
and cursor paging that does not exist. Every status below was re-derived from the code on 12 Nov 2026.

Status legend (tracker): ✅ verified · 🔵 built, not verified · 🟡 partial · ❌ missing · ⛔ blocked on a named
external dependency · 🧭 waits on a named decision. The implementer never sets ✅.

## 1. Specification context (what binds this service)

| Source | What it gives this service |
|---|---|
| Doc 03 §8.2 (`docs/architecture/03-microservices.md`) | Owns jurisdiction profiles, rule libraries, effective-dated rule sets, entity-jurisdiction applicability, compliance calendar logic, external rule references, legal drift indicators. APIs: resolve jurisdiction set, fetch runtime rule pack, get effective rule by date, validate action. Events out: `jurisdiction.rule.updated`, `.rule.activated`, `.calendar.changed`, `legal.drift.detected`, `jurisdiction.created`, `.deactivated`. Events in: `entity.created`, `entity.jurisdiction.changed`, external regulatory feed changes. Evidence: rule basis of every governed action; historical rule state for replay. |
| ZS-JUR-001 (Global Jurisdiction Pack & Regulatory Rule Architecture Standard) | §3 15 invariants; §4 hierarchy; §5–6 pack manifest and components; §7 rule identity and temporal fields; §8 source register; §9 determination; §10 precedence/conflicts; §11–19 rule families (registration, tax, e-invoice, filing, calendar, payroll, retention, accounting mapping); §20 authoring workflow; §21 compiler/registry; §22 certification harness; §23 release states and rings; §24 hotfix; §25 runtime APIs; §26 canonical events; §27 persistence; §28 privilege separation; §29 observability; §31 change management; §34 JUR-NEG-01…30; §37 waves 0–8. |
| ZS-SVC-V-001 PDC-02 (Jurisdiction, Scope & Applicability Resolution) | Outcomes RESOLVED / AMBIGUOUS / INSUFFICIENT / CONFLICTED / NOT_APPLICABLE with evidence; no guessed jurisdiction; explicit precedence; NP-11…NP-20. |
| ZS-SVC-I/U-001 REF-01 (Jurisdiction Registry) | Commands ImportJurisdictionUpdate, AddInternalJurisdiction, AmendJurisdictionAlias, RetireJurisdiction; queries ResolveJurisdiction, GetHierarchy, GetJurisdictionAsOf, MapExternalJurisdictionCode; lifecycle Proposed → Active → Retired/Superseded; maker-checker on source imports; events JurisdictionAdded/Updated/Retired/HierarchyChanged; negative paths #22, #23. |
| REF-10 (Business Calendar & Payment Terms) | Commands ImportHolidayCalendar, CreateBusinessCalendar, ApprovePaymentTerm, SupersedePaymentTerm, RetirePaymentTerm; queries IsBusinessDay, AddBusinessDays, CalculateDueDate, GetPaymentTerm, ExplainDueDate; events BusinessCalendarUpdated, HolidayOccurrenceChanged, PaymentTermActivated, PaymentTermSuperseded; negative paths #40, #41. |
| Tracker D2-1 (`docs/audit_files/DOMAIN1-DOMAIN2-COMPLETION-TRACKER-2026-11-06.md`) | Feature rows F01–F29, tasks JR-0…JR-7 and JR-FE-1…3, open register J-01…J-17, rules R1–R15 (R4 migrations, R5 registrations, R6 route controls, R7 wiring, R10 tests, R14 no stubs). |
| Design notes | `docs/architecture/jurisdiction-pack-wave0…8*.md`, `jurisdiction-pack-decision-rule-parameters.md`. |

## 2. Feature inventory (every feature and operation)

### 2.1 Jurisdiction registry (REF-01, Doc 03 §8.2, ZS-JUR §4)

| ID | Operation | Route / consumer | Status |
|---|---|---|---|
| F01 | List / get (fail-closed) / ancestors | `GET /v1/jurisdictions`, `GET …/{id}`, `GET …/{id}/ancestors` | 🟡 reconfirm |
| F01 | AddInternalJurisdiction | `POST /v1/admin/jurisdictions` (Idempotency-Key, event `jurisdiction.created`) | 🟡 reconfirm |
| F01 | RetireJurisdiction | `POST /v1/admin/jurisdictions/{id}/deactivate` (If-Match, reason + evidence, `jurisdiction.deactivated`) | 🟡 reconfirm (J-13 If-Match built) |
| F07 | GetJurisdictionAsOf · GetHierarchy | `GET …/{id}/as-of`, `GET …/{id}/hierarchy` | 🔵 |
| F07 | AmendJurisdictionAlias · end alias · list aliases | `POST/GET …/{id}/aliases`, `POST …/aliases/{alias_id}/end` | 🔵 |
| F07 | ResolveJurisdiction · MapExternalJurisdictionCode | `GET /v1/jurisdiction-resolutions`, `GET /v1/jurisdiction-code-mappings` | 🔵 |
| F07 | ImportJurisdictionUpdate (maker-checker) | `POST/GET /v1/admin/jurisdiction-imports`, `GET …/{batch}`, `POST …/approve`, `…/reject` | 🔵 |
| F07 | Events JurisdictionAdded/Updated/Retired/HierarchyChanged | `jurisdiction.created`, `.updated`, `.deactivated`, `.hierarchy.changed` | 🔵 |

### 2.2 Effective-dated rules (Doc 03 §8.2)

| ID | Operation | Route | Status |
|---|---|---|---|
| F02 | Rules of a jurisdiction (as of, known_at) · get rule (ETag) · status history | `GET …/{id}/rules`, `GET /v1/rules/{id}`, `GET …/status-history` | 🟡 reconfirm |
| F02 | Create rule (DRAFT) · transition (creator ≠ activator) | `POST /v1/admin/jurisdictions/{id}/rules`, `POST /v1/admin/rules/{id}/transition` | 🟡 reconfirm |
| F06 | Activation approval confirmed with workflow-svc (J-02) | `X-Approval-Reference` → `internal/approval` | 🔵 |
| F03 | Runtime rule pack with `pack_digest` | `GET …/{id}/rule-pack` | 🟡 reconfirm |
| F04 | Manual legal drift · drift history | `POST /v1/admin/rules/{id}/drift`, `GET …/drift-events`, `legal.drift.detected` | 🟡 reconfirm |
| F05 | Automatic drift from a regulatory feed (J-03/J-04) | consumer of a regulatory source-change event | ❌ consumer · ⛔ producer (external-data-feed-svc) |
| F12 | Validate action (J-06) | `POST /v1/validate-action` | 🔵 |

### 2.3 Entity applicability (Doc 03 §8.2 "entity-jurisdiction applicability mappings")

| ID | Operation | Route / consumer | Status |
|---|---|---|---|
| F22 | Projection from `entity.created` / `entity.jurisdiction.changed` | Kafka consumer (`internal/consumer/entity.go`) | 🔵 |
| F22 | Read | `GET /v1/entities/{legal_entity_id}/jurisdictions` | 🔵 |
| F22 | J-10 backfill from tenant-entity-registry for events past Kafka retention | — | ❌ |
| F22 | Tenant isolation at the database (RLS + FORCE) | — | ❌ |

### 2.4 Pack registries, compiler, certification (ZS-JUR waves 0, 1, 3)

| ID | Operation | Route | Status |
|---|---|---|---|
| F14 | Regimes | `GET /v1/regimes`, `GET …/{regime}`, `POST /v1/admin/regimes` | 🔵 |
| F14 | Source register (capture, independent review, supersede) | `GET /v1/sources`, `GET …/{id}`, `POST /v1/admin/sources`, `…/review`, `…/supersede` | 🔵 |
| F14 | Interpretation records (approver ≠ author) | `GET /v1/interpretations/{id}`, `POST /v1/admin/interpretations`, `…/approve` | 🔵 |
| F14 | Packs and versions; submit for review | `GET /v1/packs`, `…/{ref}`, `…/versions`, `…/versions/{v}`; `POST /v1/admin/packs`, `…/versions`, `…/submit-review` | 🔵 |
| F14 | Rule provenance and typed parameters | `GET/PUT …/rules/{id}/provenance`, `GET/PUT …/rules/{id}/parameters` | 🔵 |
| F15 | Compile · sign · verify · artifact | `POST …/compile`, `…/sign`, `POST /v1/packs/{ref}/versions/{v}/verify`, `GET …/artifact` | 🔵 |
| F15 | Signing keys register / retire / revoke | `GET /v1/pack-signing-keys`, `POST /v1/admin/pack-signing-keys`, `…/retire`, `…/revoke` | 🔵 |
| F16 | Certification harness: test bundle, test runs, reviews, certify | `POST/GET …/test-bundle`, `…/test-runs`, `…/reviews`, `…/certify`, `GET …/certification` | 🔵 |
| — | API-5 async operations for compile and test runs (EST-5) | `Prefer: respond-async` → 202 + `/v1/operations/{id}` | ❌ |

### 2.5 Runtime resolution (ZS-JUR waves 2, 4–6; PDC-02)

| ID | Operation | Route | Status |
|---|---|---|---|
| F11 | Rule resolution with decision evidence · decision read · coverage · cache invalidate | `POST /v1/rule-resolutions:resolve`, `GET /v1/rule-decisions/{id}`, `GET /v1/pack-coverage`, `POST /v1/admin/resolver-cache:invalidate` | 🔵 |
| F13 | Jurisdiction determination (JUR-1) | `POST /v1/jurisdiction-decisions:resolve` | 🔵 (ORG-6/ORG-7 inputs pending) |
| F10 | Obligation rules + due-date calculation | `GET /v1/obligation-rules`, `…/{id}`, `POST /v1/admin/obligation-rules`, `PUT …/provenance`, `POST …/publish`, `POST /v1/obligation-calculations:calculate` | 🔵 |
| F20 | Registrations, licences, schemes, thresholds | `POST /v1/registration-obligations:resolve`, `/v1/registration-identifiers:validate`, `/v1/licences:evaluate`, `/v1/scheme-elections:evaluate`, `/v1/threshold-monitors:evaluate` | 🔵 |
| F20 | Tax calculation | `POST /v1/regulatory-calculations:execute` | 🔵 · events `regulatory-calculation.completed/.failed` ❌ |
| F20 | Payroll parameters · retention · accounting mappings · submission profiles | `POST /v1/payroll-statutory-parameters:resolve`, `/v1/retention-rules:resolve`, `/v1/accounting-mappings:resolve`, `/v1/submission-profiles:resolve` | 🔵 |
| F21 | Regulatory submissions lifecycle | `POST /v1/regulatory-submissions`, `GET …/{id}`, `POST …/{id}/events` | 🔵 · tenant isolation ❌ |
| — | Decision evidence tenant isolation | `rule_decision_evidence` | ❌ |

### 2.6 Calendars and payment terms (REF-10, ZS-JUR §15)

| ID | Operation | Route | Status |
|---|---|---|---|
| F08 | Calendars and versions (maker-checker publish), sources, holiday import, observed holidays | `GET /v1/calendars`, `…/{code}`, `…/versions`, `…/versions/{v}`; `POST /v1/admin/calendars`, `…/versions`, `…/publish`, `PUT …/sources`, `POST …/imports` | 🔵 |
| F08 | IsBusinessDay · AddBusinessDays | `GET …/is-business-day`, `GET …/add-business-days` | 🔵 |
| F09 | Payment terms: create, version, approve, supersede, retire | `POST /v1/admin/payment-terms`, `…/versions`, `…/approve`, `…/supersede`, `…/retire` | 🔵 |
| F09 | GetPaymentTerm · CalculateDueDate · ExplainDueDate | `GET /v1/payment-terms/{code}`, `…/versions`, `…/versions/{v}`, `…/due-date`, `…/explain-due-date` | 🔵 |

### 2.7 Release operations, rollouts, remediation (ZS-JUR waves 7, 8; §23, §24, §29, §31)

| ID | Operation | Route | Status |
|---|---|---|---|
| F17 | Publish · deploy (rings/regions) · rollback · supersede · withdraw · block · unblock | `POST /v1/admin/packs/{ref}/versions/{v}/publish|deploy|rollback|supersede|withdraw|block|unblock`, `GET …/deployments` | 🔵 |
| F17 | Emergency hotfix + retrospective | `POST …/hotfix`, `…/hotfix/retrospective`, `GET …/hotfix` | 🔵 |
| F17 | Rings and regions | `GET/POST /v1/deployment-rings`, `/v1/deployment-regions` | 🔵 |
| F17 | Source-change notices (intake, review, close) | `GET /v1/source-change-notices`, `…/{id}`, `POST /v1/admin/source-change-notices`, `…/review`, `…/close` | 🔵 |
| F17 | §29 operations metrics | `GET /v1/ops/metrics` | 🔵 |
| F18 | Rollout governance | `GET/POST /v1/admin/rollouts`, `GET …/{id}`, `…/attestations`, `…/expert-approvals`, `…/transition`, `PUT …/owner`, `PUT …/packs`, `POST /v1/admin/rollouts:seed-portfolio`, `GET /v1/rollout-checklists`, `GET /v1/jurisdiction-support/{code}` | 🔵 |
| F19 | Remediation cases | `GET/POST /v1/admin/remediation-cases`, `GET …/{id}`, `POST …/close`, `GET …/decisions`, `POST …/dispositions` | 🔵 |
| — | §26 rule lifecycle events `regulatory-rule.versioned/.effective/.expired` | effectivity watcher | ❌ |

### 2.8 Cross-cutting controls

| ID | Control | Status |
|---|---|---|
| F23 / J-11 | Registry events written in the same transaction as the write | ❌ (written after commit, through the pool) |
| F23 / J-12 | `aggregate_version` on registry events | ❌ |
| F23 / J-15 | Outbox claim with `FOR UPDATE SKIP LOCKED` | ❌ |
| F23 / J-16 | Parked outbox events: list + requeue (authorized, evidenced) | ❌ |
| F24 / J-01 | No published service port; signed identity required | 🟡 (gate mounted, `IDENTITY_VERIFY=warn`, `8082:8082` published) |
| F25 | Idempotency-Key replay, ETag/If-Match, bitemporal replay, append-only triggers, RFC 9457 | 🟡 reconfirm; If-Match covers only jurisdictions and rules |
| F27 | API-4 cursor paging on list routes | ❌ |
| F28 | Router ↔ openapi parity + kin-openapi validity | 🔵 (`TestOpenAPIMatchesTheRouter`, `TestOpenAPIDocumentIsValid`) |
| R4 | RLS + FORCE on tenant data | ❌ (no table has RLS; three hold tenant data) |
| R5 | Every checked action registered in access-control | 🔵 (83 actions, all registered; template coverage to confirm) |
| R10 | Negative paths named after their spec id | ❌ (ids in comments only; JUR-NEG-16, 17, 21, 29, 30 untested; 11 Nov suite hollow) |
| JR-FE-1 | Console summary route `GET /v1/jurisdiction-summary` | ❌ |
| S2 | asyncapi.yaml, RUNBOOK.md, RELEASE_CERTIFICATE.md, scripts/audit.sh, Postman | ❌ (only openapi.yaml, SPEC_DEVIATIONS.md) |
| S4 | `deployments/scripts/e2e-jurisdiction-rules-deps.py` | ❌ |

### 2.9 Decisions and blocked items (not buildable here)

| Item | Owner | Status |
|---|---|---|
| F26 · J-05 / C8: who owns entity applicability (projection vs owner) | Architecture, with tenant-entity-registry and obligations owners | 🧭 (built as a projection) |
| F26 · J-08: one precedence model (`precedence_level` vs registry `precedence`) | Architecture / Tax Engineering | 🧭 |
| F26 · J-09: public reads tier, `rule_payload` content, event naming, inherited rules of deactivated ancestors | Product / architecture | 🧭 |
| F05 producer: regulatory source-change event from external-data-feed-svc | external-data-feed-svc owner (teammate) | ⛔ |
| Production pack signing key, `RESOLVER_RING` / `RESOLVER_REGION` | Platform ops (SEC-1 key custody) | ⛔ |
| Tax families beyond flat/banded rates (place of supply, reverse charge, exemptions …) | Tax Engineering (wave docs) | 🧭 |
| Country rule content | Local expert certification (ZS-JUR §32) | 🧭 |

## 3. Ordered completion steps (backend)

1. **JR-0 baseline.** Make the registry integration suite runnable against an external scratch Postgres
   (`JUR_IT_DSN`), rewrite the hollow negative-path suite, run build · vet · unit · store · registryit with
   0 skips and record counts; check the dev DB migrations.
2. **Unit of work (J-11, R4).** One database transaction per request: every store call and every outbox row
   of a request commit together or not at all; the response is released only after the commit. The same
   transaction carries the caller's tenant (`app.tenant_id`) for RLS.
3. **Tenant isolation (R4).** RLS + FORCE on `entity_jurisdiction_applicability`; `tenant_id` + RLS + FORCE on
   `regulatory_submissions`, `regulatory_submission_events`, `rule_decision_evidence` (platform-scope read for
   remediation operators only).
4. **Outbox hygiene (JR-3).** `aggregate_version` on registry events (J-12); `FOR UPDATE SKIP LOCKED` claim and a
   two-worker test (J-15); parked-event list and requeue routes with authorization and evidence (J-16).
5. **Concurrency (F25, R6).** `row_version` on every mutable aggregate, `ETag` on its reads, `If-Match` (412) on
   its commands, checked under a row lock inside the request transaction.
6. **API-4 (JR-5).** Cursor paging on every list route (`page_size`, `cursor`, `filter[...]`; signed cursors
   bound to tenant, route and filters), keeping the bare-array answer for callers that send no paging
   parameter.
7. **Events (§26).** `regulatory-calculation.completed/.failed`; `regulatory-rule.versioned/.effective/.expired`
   (effectivity watcher with a durable watermark).
8. **J-10 backfill (JR-2).** Page tenant-entity-registry's entities and jurisdiction assignments and upsert the
   projection with synthetic event ids, version-guarded; admin route + test.
9. **Negative paths (R10).** One test named after each of JUR-NEG-01…30, REF-01 #22/#23, REF-10 #40/#41, and the
   PDC-02 NP-11…NP-20 cases that apply.
10. **Console summary (JR-FE-1 backend part).** `GET /v1/jurisdiction-summary` with the five KPIs, the pack
    pipeline counts and the coverage bars, all live.
11. **API-5 (EST-5).** Async compile and test runs: `Prefer: respond-async` → 202 + `Location`, operation
    polling and cancel.
12. **Drift consumer (JR-6).** Consumer for a regulatory source-change event (contract documented in
    asyncapi.yaml); matching ACTIVE rules move to `UNDER_REVIEW` with drift evidence; producer ⛔.
13. **J-01 (JR-4).** Loopback-bind the published port; admin mutations require a verified identity
    (`IDENTITY_VERIFY_ADMIN=require`); reads stay `warn` until the out-of-domain readers forward the envelope.
14. **S2 contract and docs.** openapi for every new route; asyncapi.yaml; RUNBOOK.md; RELEASE_CERTIFICATE.md;
    SPEC_DEVIATIONS.md; scripts/audit.sh; Postman collection.
15. **S3/S4 live.** Migrate the dev DB, rebuild the image, run audit.sh live, write and pass
    `deployments/scripts/e2e-jurisdiction-rules-deps.py`.
16. **S6.** Evidence blocks, mutation check (break two guards, a test fails for each), tracker rows → 🔵.

## 4. Negative-path matrix (R10)

Filled in by step 9. Each row: spec id → test name → status.

## 5. Test baseline

**12 Nov 2026, JR-0 (before any change).** Host Go 1.26.7, `GOTMPDIR` on D: (Application Control blocked a test
binary once; the rerun passed). Store and registry suites ran against scratch databases `jur_store_it` and
`jur_reg_it` on the dev Postgres (`registryit` now takes `JUR_IT_DSN`; embedded Postgres stays the default).

| Suite | Command | Pass | Fail | Skip |
|---|---|---|---|---|
| build + vet | `go build ./... && go vet ./...` | ✔ | — | — |
| unit (all packages) | `go test -count=1 ./...` | 504 | 7 | 81 (the store DB tests, run below) |
| store | `TEST_DATABASE_URL=… REQUIRE_DB_TESTS=1 go test -p 1 ./internal/store/` | 88 | 0 | 0 |
| registryit | `JUR_IT_DSN=… go test -tags=integration ./internal/registryit/` | 115 | 0 | 0 |

The 7 failures were the 11 Nov `TestJUR_ValidateAction_NegativePaths`, written against field names the
route does not have and never run. The suite is rewritten (`internal/handler/negative_paths_test.go`): every
case sends a real body and asserts status, `application/problem+json` and the problem code; 4 tests,
21 cases, all pass. Dev DB: `migrate.py status --db jurisdiction_rules` → 36/36 applied.

## 6. Evidence log

One block per step, appended as each step lands.
