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
| F01 | List / get (fail-closed) / ancestors | `GET /v1/jurisdictions`, `GET …/{id}`, `GET …/{id}/ancestors` | 🔵 (API-4 paging; e2e 13 Nov) |
| F01 | AddInternalJurisdiction | `POST /v1/admin/jurisdictions` (Idempotency-Key, event `jurisdiction.created`) | 🔵 (e2e: create, replay, event on Kafka) |
| F01 | RetireJurisdiction | `POST /v1/admin/jurisdictions/{id}/deactivate` (If-Match, reason + evidence, `jurisdiction.deactivated`) | 🔵 (If-Match, unit of work) |
| F07 | GetJurisdictionAsOf · GetHierarchy | `GET …/{id}/as-of`, `GET …/{id}/hierarchy` | 🔵 |
| F07 | AmendJurisdictionAlias · end alias · list aliases | `POST/GET …/{id}/aliases`, `POST …/aliases/{alias_id}/end` | 🔵 |
| F07 | ResolveJurisdiction · MapExternalJurisdictionCode | `GET /v1/jurisdiction-resolutions`, `GET /v1/jurisdiction-code-mappings` | 🔵 |
| F07 | ImportJurisdictionUpdate (maker-checker) | `POST/GET /v1/admin/jurisdiction-imports`, `GET …/{batch}`, `POST …/approve`, `…/reject` | 🔵 |
| F07 | Events JurisdictionAdded/Updated/Retired/HierarchyChanged | `jurisdiction.created`, `.updated`, `.deactivated`, `.hierarchy.changed` | 🔵 |

### 2.2 Effective-dated rules (Doc 03 §8.2)

| ID | Operation | Route | Status |
|---|---|---|---|
| F02 | Rules of a jurisdiction (as of, known_at) · get rule (ETag) · status history | `GET …/{id}/rules`, `GET /v1/rules/{id}`, `GET …/status-history` | 🔵 (ETag on the rule read) |
| F02 | Create rule (DRAFT) · transition (creator ≠ activator) | `POST /v1/admin/jurisdictions/{id}/rules`, `POST /v1/admin/rules/{id}/transition` | 🔵 (author refused before workflow-svc is asked; e2e 403) |
| F06 | Activation approval confirmed with workflow-svc (J-02) | `X-Approval-Reference` → `internal/approval` | 🔵 (e2e live: unknown, PENDING, REJECTED, other rule's approval → 403; APPROVED → 200 ACTIVE) |
| F03 | Runtime rule pack with `pack_digest` | `GET …/{id}/rule-pack` | 🔵 |
| F04 | Manual legal drift · drift history | `POST /v1/admin/rules/{id}/drift`, `GET …/drift-events`, `legal.drift.detected` | 🔵 |
| F05 | Automatic drift from a regulatory feed (J-03/J-04) | consumer of a regulatory source-change event (`internal/consumer/feed.go`) | 🔵 consumer (off by default; e2e live with a synthetic event) · ⛔ producer (external-data-feed-svc) |
| F12 | Validate action (J-06) | `POST /v1/validate-action` | 🔵 |

### 2.3 Entity applicability (Doc 03 §8.2 "entity-jurisdiction applicability mappings")

| ID | Operation | Route / consumer | Status |
|---|---|---|---|
| F22 | Projection from `entity.created` / `entity.jurisdiction.changed` | Kafka consumer (`internal/consumer/entity.go`) | 🔵 |
| F22 | Read | `GET /v1/entities/{legal_entity_id}/jurisdictions` | 🔵 |
| F22 | J-10 backfill from tenant-entity-registry for events past Kafka retention | `POST /v1/admin/entity-applicability:backfill` | 🔵 (e2e: restored, rerun no-op) |
| F22 | Tenant isolation at the database (RLS + FORCE) | 000032 | 🔵 (probe-role tests; e2e) |

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
| — | API-5 async operations for compile and test runs (EST-5) | `Prefer: respond-async` → 202 + `/v1/operations/{id}`, `POST …/cancel` | 🔵 (e2e: compile succeeded async) |

### 2.5 Runtime resolution (ZS-JUR waves 2, 4–6; PDC-02)

| ID | Operation | Route | Status |
|---|---|---|---|
| F11 | Rule resolution with decision evidence · decision read · coverage · cache invalidate | `POST /v1/rule-resolutions:resolve`, `GET /v1/rule-decisions/{id}`, `GET /v1/pack-coverage`, `POST /v1/admin/resolver-cache:invalidate` | 🔵 |
| F13 | Jurisdiction determination (JUR-1) | `POST /v1/jurisdiction-decisions:resolve` | 🔵 (ORG-6/ORG-7 inputs pending) |
| F10 | Obligation rules + due-date calculation | `GET /v1/obligation-rules`, `…/{id}`, `POST /v1/admin/obligation-rules`, `PUT …/provenance`, `POST …/publish`, `POST /v1/obligation-calculations:calculate` | 🔵 |
| F20 | Registrations, licences, schemes, thresholds | `POST /v1/registration-obligations:resolve`, `/v1/registration-identifiers:validate`, `/v1/licences:evaluate`, `/v1/scheme-elections:evaluate`, `/v1/threshold-monitors:evaluate` | 🔵 |
| F20 | Tax calculation | `POST /v1/regulatory-calculations:execute` | 🔵 · events `regulatory-calculation.completed/.failed` 🔵 |
| F20 | Payroll parameters · retention · accounting mappings · submission profiles | `POST /v1/payroll-statutory-parameters:resolve`, `/v1/retention-rules:resolve`, `/v1/accounting-mappings:resolve`, `/v1/submission-profiles:resolve` | 🔵 |
| F21 | Regulatory submissions lifecycle | `POST /v1/regulatory-submissions`, `GET …/{id}`, `POST …/{id}/events` | 🔵 · tenant isolation 🔵 (000032) |
| — | Decision evidence tenant isolation | `rule_decision_evidence` (000032, operator cross-tenant read) | 🔵 |

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
| — | §26 rule lifecycle events `regulatory-rule.versioned/.effective/.expired` | effectivity watcher (`internal/events/effectivity.go`, 000035) | 🔵 (e2e: `.effective` announced) |

### 2.8 Cross-cutting controls

| ID | Control | Status |
|---|---|---|
| F23 / J-11 | Registry events written in the same transaction as the write | 🔵 (request unit of work; 503 `event_not_recorded` aborts the write) |
| F23 / J-12 | `aggregate_version` on registry events | 🔵 (e2e: on the Kafka envelope) |
| F23 / J-15 | Outbox claim with `FOR UPDATE SKIP LOCKED` | 🔵 (two-relay test; live relay fixed 13 Nov) |
| F23 / J-16 | Parked outbox events: list + requeue (authorized, evidenced) | 🔵 (e2e; 4 real parked events requeued live) |
| F24 / J-01 | No published service port; signed identity required | 🔵 (`127.0.0.1:8082`; admin writes need an envelope or mTLS peer; reads stay `warn`) |
| F25 | Idempotency-Key replay, ETag/If-Match, bitemporal replay, append-only triggers, RFC 9457 | 🔵 (If-Match on 14 aggregates, 000034) |
| F27 | API-4 cursor paging on list routes | 🔵 (signed, tenant-bound cursors) |
| F28 | Router ↔ openapi parity + kin-openapi validity | 🔵 (`TestOpenAPIMatchesTheRouter`, `TestOpenAPIDocumentIsValid`) |
| R4 | RLS + FORCE on tenant data | 🔵 (000032 on five tables incl. async_operations 000036) |
| R5 | Every checked action registered in access-control | 🔵 (access-control 000052: six new actions, nine templates) |
| R10 | Negative paths named after their spec id | 🔵 (see §4; 17 🧭, 29 🟡) |
| JR-FE-1 | Console summary route `GET /v1/jurisdiction-summary` | 🔵 |
| S2 | asyncapi.yaml, RUNBOOK.md, RELEASE_CERTIFICATE.md, scripts/audit.sh, Postman | 🔵 (all present; Postman folders 04–07) |
| S4 | `deployments/scripts/e2e-jurisdiction-rules-deps.py` | 🔵 (82/82 on 13 Nov with workflow-svc and the feed consumer; 59 + 2 SKIP with `--no-workflow`) |

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

Every spec negative path has a test named after its id. Pure-logic proofs run in `internal/domain`,
`internal/resolver` and `internal/store` (`negative_paths_index_test.go`); end-to-end proofs in
`internal/registryit` carry the id in their own name, so each runs once.

| Id | Scenario (short) | Test | Status |
|---|---|---|---|
| JUR-NEG-01 | Old rate before new effective date | `TestJURNEG01_TransactionBeforeANewRateUsesTheOldRule` (domain) | 🔵 |
| JUR-NEG-02 | Overlap without precedence | `TestJURNEG02_OverlapWithoutPrecedenceIsRefused` (domain) | 🔵 |
| JUR-NEG-03 | Digest mismatch | `TestJURNEG03_DigestMismatchIsRefused` (domain), `TestJURNEG04_RuntimeRefusesAnUntrustworthyPack` (resolver) | 🔵 |
| JUR-NEG-04 | Unsigned pack | `TestJURNEG04_UnsignedPackFailsClosed` (domain), `TestJURNEG04_RuntimeRefusesAnUntrustworthyPack` (resolver) | 🔵 |
| JUR-NEG-05 | Client jurisdiction not authoritative | `TestJURNEG05_AClientClaimIsNotAuthoritative` (domain) | 🔵 |
| JUR-NEG-06 | Source URL changes after release | `TestJURNEG06_SupersedingASourcePreservesIt` (registryit) | 🔵 |
| JUR-NEG-07 | Past return recalculated | `TestJURNEG07_HistoricalRecalculationUsesTheHistoricalVersion` (resolver) | 🔵 |
| JUR-NEG-08 | Schema invalid after cutoff | `TestJURNEG08_27_SubmissionProfiles_AreResolvedFromTheVerifiedPackWithDependencyChecks` (registryit) | 🔵 |
| JUR-NEG-09 | E-invoice rejected | `TestJURNEG09_RejectedEInvoiceIsNotAccepted` (domain) | 🔵 |
| JUR-NEG-10 | Duplicate filing callback | `TestJURNEG10_Submissions_RegisterRefuseAndLifecycle` (registryit) | 🔵 |
| JUR-NEG-11 | Late/out-of-order callback | `TestJURNEG11_LateCallbackCannotRegress` (domain) | 🔵 |
| JUR-NEG-12 | Tenant shortens retention | `TestJURNEG12_Retention_IsAnsweredFromTheVerifiedPackAndNeverShortenedByATenant` (registryit) | 🔵 |
| JUR-NEG-13 | Holiday calendar amended | `TestJURNEG13_HolidayAmendmentIsANewVersion` (domain) | 🔵 |
| JUR-NEG-14 | Hotfix lacks second approver | `TestJURNEG14_Hotfix_EnhancedReviewRingExceptionAndRetrospective` (registryit) | 🔵 |
| JUR-NEG-15 | Dependency unavailable in a region | `TestJURNEG15_Release_DependenciesMustBeReleased_AndDeployedInTheSameScope` (registryit) | 🔵 |
| JUR-NEG-16 | Subdivision rule vs national rule | `TestJURNEG16_SubdivisionRuleCollidingWithItsCountryIsRefused` (registryit, new) | 🔵 |
| JUR-NEG-17 | Exemption without registration evidence | — | 🧭 no exemption rule family exists; the wave docs leave exemptions (with reverse charge, place of supply) to a Tax Engineering decision (`jurisdiction-pack-wave7-consistency.md`). Licence and scheme answers already name their `evidence_required`. |
| JUR-NEG-18 | AI change without certification | `TestJURNEG18_UnapprovedChangeCannotBeReleased` (domain) | 🔵 |
| JUR-NEG-19 | Rollback after processing | `TestJURNEG19_Rollback_…`, `TestJURNEG19_Remediation_…` (registryit) | 🔵 |
| JUR-NEG-20 | Same version, different content | `TestJURNEG20_Compile_ChangedInputsUnderSameVersionAreRefused` (registryit) | 🔵 |
| JUR-NEG-21 | Future-dated rule deployed early | `TestJURNEG21_FutureDatedRuleAppliesOnlyFromItsEffectiveDate` (domain, new) | 🔵 |
| JUR-NEG-22 | Unsupported jurisdiction | `TestJURNEG22_UnsupportedJurisdictionIsExplicit` (domain) | 🔵 |
| JUR-NEG-23 | Guidance claims mandatory effect | `TestJURNEG23_GuidanceNeedsAnApprovedInterpretation` (domain) | 🔵 |
| JUR-NEG-24 | Two regimes for one transaction | `TestJURNEG24_CrossPackAmbiguityIsBlocked` (resolver) | 🔵 |
| JUR-NEG-25 | Float arithmetic | `TestJURNEG25_FloatArithmeticIsRefused` (domain) | 🔵 |
| JUR-NEG-26 | Due date on a holiday | `TestJURNEG26_DueDateOnAHolidayIsAdjusted` (domain) | 🔵 |
| JUR-NEG-27 | New code list mid-cycle | `TestJURNEG08_27_SubmissionProfiles_…` (registryit) | 🔵 |
| JUR-NEG-28 | Pack withdrawn | `TestJURNEG28_Release_PublishWithdrawBlock_AreRecordedAndGuarded` (registryit) | 🔵 |
| JUR-NEG-29 | Service runs a stale pack | `TestJURNEG29_OpsMetrics_…`, `TestJURNEG29_Resolver_UsesOnlyWhatIsDeployedToItsRingAndRegion` (registryit) | 🟡 skew is reported and each resolver serves only its ring and region; blocking a regulated action on skew needs a consistency policy nobody has defined (wave 7 "Not built": services do not report the version they run) |
| JUR-NEG-30 | Retroactive legislation | `TestJURNEG30_Remediation_ManualCaseAndNothingToRemediate` (registryit) + pinned replay (JUR-NEG-07) | 🔵 |
| REF-01 #22 | Unknown code | `TestREF01NP22_UnknownCodeIsRefusedNotGuessed` (store), `TestJUR_ValidateAction_UnknownJurisdictionIs404` (handler) | 🔵 |
| REF-01 #23 | Retired/renamed code | `TestREF01NP23_RetiredCodeKeepsItsHistory` (store) | 🔵 |
| REF-10 #40 | Holiday added after invoice | `TestREF10NP40_PaymentTerm_…`, `TestREF10NP40_BusinessCalendar_…` (registryit) | 🔵 |
| REF-10 #41 | Basis date unresolvable | `TestREF10NP41_UnresolvableBasisDateIsRefused` (domain), `TestREF10NP41_PaymentTerm_DueDateInputRefusals` (registryit) | 🔵 |
| PDC-02 NP-11 | Missing legal_entity_id | `TestPDC02NP11_JurisdictionDecision_RequestValidationAndRouteWiring` (registryit) | 🔵 |
| PDC-02 NP-12 | Two plausible jurisdictions | `TestPDC02NP12_TwoPlausibleJurisdictionsNeedReview` (domain) | 🔵 |
| PDC-02 NP-13 | UI locale ≠ legal jurisdiction | `TestPDC02NP13_LocaleOrClaimIsNotALegalFact` (domain) | 🔵 |
| PDC-02 NP-14 | Current hierarchy for a past period | `TestPDC02NP14_HistoricalPeriodUsesTheAsOfHierarchy` (store) | 🔵 |
| PDC-02 NP-15 | Branch mistaken for an entity | — | n/a here: legal personality is tenant-entity-registry's fact (MDM); this service reads assignments only |
| PDC-02 NP-16 | Tenant overrides a mandatory rule | — | n/a by construction: no tenant override path exists (s28 elections not built), so nothing can override a pack rule |
| PDC-02 NP-17 | Same-rank incompatible mandates | `TestPDC02NP17_SameRankIncompatibleMandatesAreConflicted` (resolver), `TestValidateAction_ConflictedIsNotPermitted` (handler) | 🔵 |
| PDC-02 NP-18 | No match for a mandatory family | `TestPDC02NP18_NoMatchNeverFallsBack` (domain), `TestValidateAction_NoRuleIsInsufficient` (handler) | 🔵 |
| PDC-02 NP-19 | Fixed UTC offset for a DST cutoff | `TestPDC02NP19_DeadlinesUseTheAuthorityTimeZone` (domain) | 🔵 |
| PDC-02 NP-20 | Retroactive source received late | `TestPDC02NP20_KnownAtReplaysWhatWasKnown` (store) | 🔵 |

PDC-02 NP-01…NP-10 are PDC-01 (policy package registry) scenarios, owned by policy-svc.

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

**13 Nov 2026, after step 16.** Application Control blocks host test binaries, so the suites ran in a
`golang:1.26-alpine` container (900 MB cap) against fresh scratch databases on the dev Postgres.

| Suite | Pass | Fail | Skip |
|---|---|---|---|
| unit (all packages) | 569 | 0 | 85 (the store DB tests, run below) |
| store (`jur_store_it`) | 92 | 0 | 0 |
| registryit (`jur_reg_it`, async operations through the `jur_rls_probe` role) | 143 | 0 | 0 |
| openapi / asyncapi contract tests | ✔ | — | — |
| live e2e `deployments/scripts/e2e-jurisdiction-rules-deps.py` (with workflow-svc and the feed consumer, step 17) | 82 | 0 | 0 |
| live e2e `--no-workflow` (workflow-svc stopped: fail-closed 503) | 59 | 0 | 2 (the workflow and feed sections) |
| live `scripts/audit.sh` (`SKIP_SUITES=1`, the suites ran above) | 24 | 0 | — |

## 6. Evidence log

| Step | What landed | Commit | Proof |
|---|---|---|---|
| 1 JR-0 | `JUR_IT_DSN` for registryit; hollow negative suite rewritten (4 tests, 21 cases) | 29fb166b | baseline in §5 |
| 2 J-11 | request unit of work; savepoint per statement; held response; outbox failure aborts with 503 `event_not_recorded` | 5c8cf652 | `registryit/unit_of_work_*` |
| 3 R4 | 000032 RLS + FORCE on five tenant tables; operator cross-tenant read for decision evidence | be1dfe7f | `tenant_isolation_*` through `jur_rls_probe`; e2e "RLS" checks |
| R5 | access-control 000052: six actions, nine role templates | 529ce58e | `registered_actions_test.go`; e2e "nine templates" |
| 4 J-12/15/16 | `aggregate_version` on events; claim `FOR UPDATE SKIP LOCKED` under advisory lock; parked list + requeue (000033) | fa65733c, af349005 | `TestJ15_TwoRelaysPublishEachEventOnce`, `TestJ16_*`; e2e Kafka envelope + requeue |
| 5 API-3 | `row_version` on 14 aggregates (000034); ETag; If-Match under row lock | af349005 | `TestAPI3_*`; e2e 412 then ETag 2 |
| 6 API-4 | signed, tenant-bound cursors on every list route | b7e8c903 | `paging_*`; e2e paging, foreign cursor 400 |
| 7 §26 | calculation events; rule `.versioned/.effective/.expired` watcher (000035) | e950eebf | `rule_effectivity_*`; e2e `.effective` |
| 8 J-10 | backfill from tenant-entity-registry | 65d03d38 | `backfill_*`; e2e restore + rerun no-op |
| 9 R10 | every negative path named after its id (§4) | 319f7c44 | `negative_paths_index_test.go` per package |
| 10 JR-FE-1 | `GET /v1/jurisdiction-summary` | 03e739ae | `summary_*`; e2e summary read |
| 11 API-5 | async compile / test runs, poll, cancel (000036) | 64fee24d | `TestAPI5_*`; e2e async compile succeeded |
| 12 JR-6 | `regulatory.source.changed` consumer (producer ⛔) | 589729eb | `feed_consumer_*` |
| 13 J-01 | loopback port; `IDENTITY_VERIFY_ADMIN=require` | 858eda1f | `TestJ01_AdminWritesNeedAVerifiedIdentity`; e2e 401 for headers-only and forged basis |
| 14 S2 | openapi 1.2.0, asyncapi, RUNBOOK, deviations, audit.sh, Postman | 0a9a8fbf | `TestOpenAPI*`, `TestAsyncAPI*` |
| 15 S3/S4 | live run found four defects, each fixed with a test that fails on the old code: relay set the topic twice so nothing was ever published (pre-existing); async operations written without their tenant; 000005 never applied on old-lineage databases (000037); the author's own activation answered 503 when workflow-svc was down | c7ab467e, 1ba898ff, c66d811e, 75769129 | e2e 59/59; audit 24/24; 4 events parked by the relay bug requeued through J-16, outbox 0 pending / 0 parked |
| 16 S6 | mutation check | — | J-01 gate accepting any basis → `TestJ01_AdminWritesNeedAVerifiedIdentity` fails (`204, want 401`); If-Match comparison disabled → `TestAPI3_SourceCommandsCheckIfMatch` fails (`expected 412, actual 200`). Both restored. |
| 17 S4 | live: workflow-svc approval outcomes (J-02), effectivity through the API, the regulatory feed consumer (JR-6); compose passes `KAFKA_REGULATORY_FEED_TOPIC` from `JURISDICTION_REGULATORY_FEED_TOPIC` (empty = off) | — | e2e 82/82, audit 24/24; no defect found. Stack: postgres, kafka, redis, mtls-management, identity-context, tenant-entity-registry, access-control, authorization, workflow-svc (`--no-deps`, no document-vault), this service; governance-decision-log started afterwards so workflow-svc's 15 ledger records delivered (outbox 0) |

Step 17 replaced what steps 15–16 could not run live:
- J-02: an activation citing an unknown id, a PENDING or a REJECTED workflow of the rule, or an APPROVED
  workflow of another rule answers 403 `approval_not_valid` with the reason, and the rule stays DRAFT. One
  citing an APPROVED workflow of the rule, decided by the approver, answers 200 ACTIVE; the open
  status-history row holds the workflow id and the activator, and `jurisdiction.rule.activated` is recorded.
- The effectivity rule is activated through workflow-svc, not a database fixture (the fixture stays for
  `--no-workflow`).
- JR-6: with the consumer on, a `regulatory.source.changed` event published to the feed topic moves the
  ACTIVE rule citing the reference to `UNDER_REVIEW` (status and payload unchanged), records a drift event
  by `jurisdiction-rules-svc:regulatory-feed` quoting the authority and event id, and `legal.drift.detected`.
  After a person sets the drift back to CURRENT, the same event redelivered changes nothing
  (`consumed_events`). The event is synthetic: its real producer, external-data-feed-svc, does not publish
  it yet (F05 ⛔).
