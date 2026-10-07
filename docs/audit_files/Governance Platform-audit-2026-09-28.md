# Group 2 — Governance Platform: documentation-compliance audit

**Date:** 28 September 2026
**Scope:** all seven services of Group 2, "Governance Platform — the non-bypassable spine", each
audited on its own: jurisdiction-rules-svc, governance-decision-log-svc, policy-svc,
obligations-svc, authorization-svc, workflow-svc, evidence-requirements-svc.
**Source of truth:** the original specifications in `docs/architecture/`: `03-microservices.md`
§08 (§8.1–§8.7, one section per service), `04-data-model.md`, and the `.docx` standards listed
below. The per-service `openapi.yaml`, `context.md` and `progress.md` files were treated as
**claims to verify**, not as the contract. The one exception is jurisdiction-rules-svc's
`openapi.yaml`, which was also diffed as a published contract. The reason is the same as in the
Group 1 audit: checking code against a contract written alongside it cannot catch a case where
both encode the same wrong assumption.

**Specifications used across the group:**

| Document | ID | Used for |
|---|---|---|
| `03-microservices.md` §08 Governance Platform Service Specifications | Doc 03 | Primary per-service spec (purpose, owns, inbound APIs, events, critical constraints) |
| `04-data-model.md` §7.1–7.3, §13 | Doc 04 | Entity attributes, effective-dating, compliance model |
| `ZoikoSuite_Governance_Control_Plane_Detailed_Service_Specifications.docx` | ZS-SVC-A-001 | §2 invariants, GOV-03 / 04 / 05 / 06 / 07 / 12, §16 envelope and error classes, §17 events, §18 data model |
| `ZS-SVC-V-001_Policy_Rules_Decision_Jurisdictional_Applicability_Control_…docx` | V-001 | PDC-01…05 (policy registry, jurisdiction applicability, evaluation, evidence, simulation) |
| `ZS-SVC-R-001_Workflow_Approval_Case_Obligation_Control_…docx` | R-001 | WFC-01…05 (definitions, orchestration, approval/SoD, case, obligation/deadline) |
| `ZoikoSuite_Global_Authorization_RBAC_ABAC_Segregation_of_Duties_Standard_…docx` | ZS-IAM-001 | Authorization pipeline, SoD, delegation, caching, evidence |
| `ZoikoSuite_Global_Jurisdiction_Pack_Regulatory_Rule_Architecture_Standard_…docx` | ZS-JUR-001 | Jurisdiction packs, rule immutability, precedence |
| `ZoikoSuite_Global_Data_Governance_Evidence_Lineage_Retention_Legal_Hold_Standard_…docx` | ZS-DATA-GOV-001 | Immutability, retention, legal hold |
| `ZoikoSuite_Enterprise_Domain_Event_Catalogue_Event_Contract_Standard_…docx` | Event Catalogue | Event naming, envelope, transactional outbox |
| `ZoikoSuite_Global_API_Command_Integration_Contract_Standard_…docx` | API standard | Status codes, idempotency, RFC 9457 errors, pagination, If-Match |

**Method (per service):**
1. Extract the documented contract: endpoints, schemas, status codes, auth, business rules,
   side effects and events.
2. Extract the implemented contract from the code: router, handlers, store, migrations,
   publishers and consumers, envelope policy, compose and gateway wiring.
3. Diff the two, then report.

The audit is **static and read-only**: no containers were started, no code was changed, and
no tests were run. The authorization-svc store tests would have wiped their target database.
The two authorization-svc privilege-escalation paths were checked twice in the code, once by
the service auditor and once again independently. Anything ambiguous or contradictory in the
documents is flagged **needs clarification (❓)** and left out of the score, not counted as a
pass or a fail.

**Scoring:** *Full match* = ✅ ÷ scored items. *Weighted* = (✅ + ½ ⚠️) ÷ scored items. Items
marked ❓ are not scored.

**Legend:** ✅ match · ⚠️ partial · ❌ missing · ❓ needs clarification

| # | Service | Port | Spec audited against | Full match | Weighted |
|---|---|---|---|---|---|
| 1 | jurisdiction-rules-svc | 8082 | Doc 03 §8.2 + own `openapi.yaml`; ZS-JUR-001; V-001 PDC-02 | 49% | **64%** |
| 2 | governance-decision-log-svc | 8083 | Doc 03 §8.7 + Doc 04 §7.1; GOV-07 (nearest section) | 34% | 59% |
| 3 | policy-svc | 8085 | Doc 03 §8.1; GOV-05; V-001 PDC-01…05 | 27% | 41% |
| 4 | obligations-svc | 8088 | Doc 03 §8.5 + Doc 04 §13 (R-001 WFC-05 unscored) | 45% | **65%** |
| 5 | authorization-svc | 8089 | Doc 03 §8.3; GOV-03 (+ GOV-04 / GOV-12); ZS-IAM-001 | 43% | 57% |
| 6 | workflow-svc | 8090 | Doc 03 §8.4; GOV-06; R-001 WFC-02 / WFC-03 | 34% | 49% |
| 7 | evidence-requirements-svc | 8130 | Doc 03 §8.6 + Doc 04 §7.1; GCP §1 / §2 / §16 | 37% | 58% |
| | **Group 2 total** | | 418 scored items | **39%** | **57%** |

| Service | ✅ | ⚠️ | ❌ | ❓ (unscored) | Scored |
|---|---|---|---|---|---|
| jurisdiction-rules-svc | 39 | 23 | 17 | 3 | 79 |
| governance-decision-log-svc | 16 | 23 | 8 | 5 | 47 |
| policy-svc | 15 | 15 | 25 | 2 | 55 |
| obligations-svc | 29 | 27 | 9 | 1 (+16 WFC-05 rows) | 65 |
| authorization-svc | 29 | 20 | 19 | 4 | 68 |
| workflow-svc | 21 | 18 | 22 | 2 | 61 |
| evidence-requirements-svc | 16 | 18 | 9 | 5 | 43 |
| **Total** | **165** | **144** | **109** | **22** | **418** |

**Read the percentages with the risk ranking below, not on their own.** authorization-svc
scores 57%, but two of its ❌ items let any tenant principal take over authorization for their
tenant and deny access across the platform. policy-svc's 41% is largely GOV-05 / V-001
lifecycle scope that v1 never attempted. Against Doc 03 §8.1 alone it scores 50%.

---

# Group-wide findings

These patterns recur across the group. Each one is also recorded in the section for every
affected service.

## Cross-cutting defects

| # | Pattern | Services affected | Effect |
|---|---|---|---|
| X1 | **The canonical-envelope contract is broken between callers and callees.** Every service runs the §4 envelope in `write-strict` mode (the default; compose overrides it for none of them). On every write it demands `X-Request-Id`, `X-Source-Channel`, `Idempotency-Key` and, in most services, `X-Legal-Entity-Id` (the decision log also demands `X-Purpose-Context`). Internal clients were written before enforcement and send only tenant, principal and correlation headers. | policy-svc → decision-log (**every** write refused, 400 `envelope_incomplete`); board-resolutions / corporate-actions / filing-preparation → evidence-requirements (**every** evaluate refused); decision-support / contract-lifecycle / evidence-manifest → decision-log reads (these pass only because write-strict admits incomplete reads); jurisdiction-rules `openapi.yaml` omits the headers entirely | The governance evidence chain does not work end to end (X2). The failures are silent because callers either swallow the error (policy-svc) or turn it into a generic 503. |
| X2 | **Governance evidence does not reach the ledger.** GOV §1 says material decisions create evidence "as part of completion semantics, not as asynchronous best effort". | policy-svc (every write refused, error only logged, Evaluate still answers 200); workflow-svc (writes no evidence at all); evidence-requirements-svc (gate unreachable; document verification gets 401 from document-vault); obligations-svc (no evidence writes) | No policy evaluation, workflow approval or evidence-gate result in this group produces a durable governance record today. |
| X3 | **No transactional outbox anywhere.** All seven services publish straight to Kafka after the DB commit, synchronously and on the request context. A failed publish is only logged, and no metric exists to alert on it. Idempotent replays deliberately do not republish. | All 7 | A broker outage, timeout or client disconnect after commit loses the event permanently, and nothing can detect it. This breaks GOV §2 invariant #10 and the Event Catalogue §6 rule against dual-writing without an outbox. |
| X4 | **`Idempotency-Key` is required and then never read.** The envelope refuses a write without it, but no handler or store uses its value. Deduplication runs on natural keys, `decision_id` or `correlation_id` instead. | All 7 | No service can detect "same key, different body" (API standard l.381, §16 `IDEMPOTENCY_MISMATCH`). Where dedup is on `correlation_id`, distinct requests in one business flow collapse into the first one: workflow-svc starts, evidence-requirements-svc evaluations, and evidence-requirements-svc requirement creates. In evidence-requirements-svc a SATISFIED result can therefore be replayed for a different action. In governance-decision-log-svc a changed body gets 200 and is shown content that was never stored. |
| X5 | **Identity headers are trusted exactly as sent.** `X-Tenant-Id` and `X-Principal-Id` are read raw. Each service's host port is published in `docker-compose.yml`, and the local Traefik routes (`traefik-dynamic/all-services.yml`) carry no ForwardAuth. | All 7 | Anyone who can reach a port directly can act as any principal in any tenant. Production safety rests entirely on GTRM ForwardAuth and network policy, and neither is documented per service. |
| X6 | **Outbound authorization calls carry no tenant or envelope.** | jurisdiction-rules-svc, obligations-svc, workflow-svc (and policy-svc Evaluate makes no authz call at all) | authorization-svc logs these decisions with a NULL tenant, so they do not appear in the tenant-scoped audit read (tracker row 82i). A tenantless `/v1/authorize` is also evaluated across tenants (authorization-svc row "Trusted tenant resolved before authorization"). |
| X7 | **No maker-checker on governing artefacts.** | policy-svc (the author of a version can activate it); jurisdiction-rules-svc (one principal can create a rule directly in ACTIVE); authorization-svc (no approval on role grants, and no permission check at all, see the authorization-svc section); workflow-svc (the initiator chooses the approvers) | GOV-05, ZS-JUR-001 §3, V-001 §18 and ZS-IAM-001 §9 all require independent approval. GOV-12 (maker-checker and break-glass) has **no owning service** in the estate. |
| X8 | **Historical decisions are not reproducible.** | policy-svc (no as-of resolution; `effective_from` / `effective_to` never gate); jurisdiction-rules-svc (status and dates mutated in place; queries filter on current status); authorization-svc (SoD and ABAC rules flipped in place, no versions); workflow-svc (no versioned definitions; instances not pinned) | This breaks GOV §2 invariants #4 and #11 and the Doc 03 §8.2 critical constraint. |
| X9 | **The error model does not follow the standard.** Every service returns ad-hoc `{"error":"snake_code"}` bodies. None uses RFC 9457 problem+json, and none uses the §16 stable classes (`AUTHORIZATION_DENIED`, `SOD_CONFLICT`, `IDEMPOTENCY_MISMATCH`, `CONCURRENCY_CONFLICT`, `POLICY_NOT_EFFECTIVE`, …). | All 7 | Estate-wide drift from the standard. Fixing it is a platform decision, not a per-service one. |
| X10 | **Malformed input is answered as an outage.** A non-UUID id or an over-length string reaches Postgres, and the cast or length error maps to 503 `store_unavailable`. | policy-svc (ids), workflow-svc (path id, tenant, legal entity), evidence-requirements-svc (write paths), jurisdiction-rules-svc (over-length strings), governance-decision-log-svc (negative `offset`), obligations-svc (caller-supplied id clash) | A caller's typo looks like an outage and is retried as one. |

## Checked and sound across the group

| Control | Result |
|---|---|
| RLS empty-GUC trap (`current_setting(...)::UUID` without NULLIF) | ✅ Not present. Every tenant-scoped service uses `NULLIF(current_setting(..., true), '')::uuid`, or a text column in governance-decision-log-svc. FORCE RLS runs under a non-owner, NOBYPASSRLS runtime role where tenant tables exist. jurisdiction-rules-svc is platform reference data with no tenant column, which is correct. |
| `go Publish(ctx)` with the request context | ✅ Not present in any of the 7. All publishing is synchronous. The residual request-context risk is part of X3. |
| Fail-closed on authorization outage | ✅ Every service that calls authorization-svc answers 403 on deny and 503 when it is unreachable. It never allows. The one exception is jurisdiction-rules-svc's permit-all stub fallback (see that section). |
| Kafka consumer stall (nil ErrorLogger, no partition watch) | ✅ The only group member with consumers is authorization-svc, and both diagnostics are wired there. The other six have no consumers. |
| Empty tenant answers 500 instead of 4xx | ✅ Not present. Missing tenants get 400 or 401, although the choice between 400 and 401 is inconsistent within some services. |

---

# Group 2 — top gaps ranked by risk

Ranking: auth and security first, then data integrity, then cosmetic doc drift. Paths are
relative to each service's directory.

### Auth / security

| Rank | Service | Gap | Where |
|---|---|---|---|
| 1 | authorization-svc | **Any tenant principal can grant themselves any permission.** Tenant-level admin writes check only that principal and tenant headers are present: create role, create or replace permission bundle, assign or revoke role, create, retire or reactivate SoD and ABAC rules. No permission is evaluated and there is no self-grant check. **Verified twice by reading the code.** | `internal/handler/handler.go:374, 528, 773-840, 842, 1105-1140, 1196, 1302` |
| 2 | authorization-svc | **A tenant user can reach platform scope.** Migration 000003 made `principal_role_assignments.legal_entity_id` nullable. `requirePlatformAction` calls `FindGrantedActions` with tenant `''`, and that query accepts `pra.legal_entity_id IS NULL`. So a tenant-wide assignment of a self-made role containing `SOD_RULE_MANAGE_GLOBAL` passes the platform check, and the holder can write SoD and ABAC rules that deny actions in **every** tenant. The comment at handler.go:293-295 saying otherwise is false. **Verified twice by reading the code.** | `internal/handler/handler.go:289-300`; `internal/store/pg_store.go:1414-1418, 1480-1484`; `deployments/migrations/000003_*` |
| 3 | authorization-svc | Tenantless `/v1/authorize` is admitted and evaluated **across all tenants**. About 86 callers send no tenant. | `internal/handler/handler.go:1699-1702`; `envelope_policy.go:88-99` |
| 4 | authorization-svc | **Suspensions and revocations can be lost permanently.** Consumers commit the offset before applying, and a failed DB apply is only logged, so the principal keeps full authority. | `internal/events/lifecycle_consumer.go:369-378`; `consumer.go:191-227` |
| 5 | governance-decision-log-svc | **Replay manifests are readable across tenants**: no tenant check, bare pool, and a table with no `tenant_id` or RLS. `POST /replay` has no authentication or authorization, and the replaying principal is taken from the request body into a permanent record. | `internal/handler/handler.go:380-471, 502-513`; `internal/store/pg_store.go:376-398`; migration 000005 |
| 6 | governance-decision-log-svc | **The GET endpoints do no authorization.** Anyone with a tenant header can read that tenant's whole decision log. The recorded `actor_id` comes from the body and is never bound to the caller. | `internal/handler/handler.go:248, 325, 502, 533` |
| 7 | policy-svc | **Evaluate takes the actor from the body and is never authorized.** The body value is written into the governance ledger as the decision's author. | `internal/handler/handler.go:803, 839-912, 853, 988`; `internal/decisionlog/client.go:143` |
| 8 | workflow-svc | **Escalate and cancel have no authorization.** Any principal in the tenant can cancel or escalate any workflow. | `internal/handler/handler.go:426-479` |
| 9 | workflow-svc | **The initiator chooses their own approvers**, and nothing checks at create that those approvers are eligible, which opens a collusion route around the self-approval ban. Delegation cannot work: a delegate always gets 403. | `internal/handler/handler.go:185-204`; `internal/store/pg_store.go:156-168` |
| 10 | jurisdiction-rules-svc | **Authorization can silently become permit-all.** The default `AUTHZ_SERVICE_URL` is on the placeholder list, which selects a stub that allows everything. The guard fires only when `ENV` is exactly `production` or `staging`, so `prod`, `uat` or an unset ENV allows every admin write. Compose sets the real URL, so this is not live on the stack. | `internal/config/config.go:92`; `internal/authz/client.go:283, 335, 353-356` |
| 11 | policy-svc | **Control-test and attestation reads need no principal, tenant or permission**, and those tables have no tenant_id. | `internal/handler/control_test_handler.go:124-134`; migration 000004 |
| 12 | obligations-svc | **Applicability reads need no login.** The legal entity is never reconciled with the tenant, so a grant on entity X lets a caller write X's obligations into any tenant. | `internal/handler/applicability_handler.go:210-277`; `internal/handler/handler.go:311`; `internal/envelope/contract.go:17` |
| 13 | evidence-requirements-svc | **Evaluate has no authorization** and trusts the body's `legal_entity_id`. Two callers (corporate-actions, filing-preparation) treat any outcome other than `MISSING`, including empty or unknown ones, as a pass, so they fail open. | `internal/handler/handler.go:101-131`; `corporate-actions-svc` and `filing-preparation-svc` `internal/evidencereq/client.go` |
| 14 | policy-svc, jurisdiction-rules-svc, authorization-svc | **No maker-checker** on policy activation, rule activation or role grants (X7). | policy `handler.go:542-550`; jurisdiction `handler.go:113-116, 677, 747`; authz `pg_store.go:622-667` |
| 15 | All 7 | Identity headers are trusted raw while host ports are published (X5). | compose `:771, :851, :1077, :1122, :1251, :2272`; `traefik-dynamic/all-services.yml:12-16` |

### Data integrity

| Rank | Service | Gap | Where |
|---|---|---|---|
| 16 | policy-svc → governance-decision-log-svc | **Every policy evaluation's evidence write is refused** with 400 `envelope_incomplete`, and policy-svc only logs the error. The client sends 3 of the 8 required headers, and for global policies it sends `X-Tenant-Id: GLOBAL`, which is not a UUID. The same service's authz client already forwards the full envelope correctly. | `policy-svc/internal/decisionlog/client.go:133-153`; `policy-svc/internal/handler/handler.go:994-1004`; `governance-decision-log-svc/internal/envelope/policy.go:185-246`, `contract.go:488-500` |
| 17 | evidence-requirements-svc | **No caller can pass the evidence gate.** All three wired callers send too few envelope headers, so write-strict refuses evaluate with 400, which each caller maps to 503. Document verification also fails, because the document-vault client sends no `X-Principal-Id` (401 → 503). 4 of the 7 finalization paths have no gate at all: GL post, financial-close lock, vat-gst and corporate-tax. | `board-resolutions-svc/internal/evidencereq/client.go:87-93`; `internal/envelope/contract.go:12-24`; `internal/documentvault/client.go:326-332` |
| 18 | policy-svc | **`effective_from` / `effective_to` are never checked.** A future-dated version decides immediately and an expired one keeps deciding. When two policies of the same type tie, the newest `effective_from` wins, which GOV-05 forbids. | `internal/store/pg_store.go:620-627`; `internal/handler/handler.go:900` |
| 19 | evidence-requirements-svc | **A SATISFIED result can be replayed for a different action.** Evaluations dedup on `correlation_id` and the response does not echo the action. One document offered twice meets `minimum_count: 2`. | `internal/store/pg_store.go:351, 364-371`; `internal/handler/handler.go:187-205, 238-241` |
| 20 | workflow-svc | **State changes read first and write later, with no lock, version or status predicate.** Concurrent approvals, or an approval racing a cancel, can both commit and publish contradictory events. | `internal/store/pg_store.go:304-409, 452-490` |
| 21 | jurisdiction-rules-svc | **The overlap check is check-then-act outside a transaction**, with no DB constraint behind it, so two live rules for the same code and period can both be created. | `internal/store/pg_store.go:861, 953, 1125` |
| 22 | All 7 | **No outbox**, and a lost event stays lost (X3). | e.g. decision-log `handler.go:294-300`; authz `handler.go:1920-1928`; workflow `handler.go:229, 396-407, 474` |
| 23 | All 7 | **`Idempotency-Key` is required but ignored** (X4). governance-decision-log-svc returns 200 for a changed body and echoes content that was never stored. workflow-svc and evidence-requirements-svc collapse distinct requests that share a correlation_id. | decision-log `handler.go:309`; workflow `pg_store.go:215-219`; evidence-req `pg_store.go:138-165` |
| 24 | jurisdiction-rules-svc, policy-svc, authorization-svc, workflow-svc | **Historical replay is not faithful** (X8). | jurisdiction `pg_store.go:710, 761, 792, 975`; policy `pg_store.go:606`; authz `pg_store.go:1155, 1252, 462` |
| 25 | obligations-svc, workflow-svc | **Nothing time-driven runs.** Overdue obligations stay OPEN forever unless someone POSTs OVERDUE, and no caller does. Workflows never escalate on their own, and an escalated workflow can never be approved. | obligations `pg_store.go:399-403`; workflow `pg_store.go:308, 422-434` |
| 26 | authorization-svc | No static SoD check at assignment time. Admin writes publish no events. Rules have no versions and there is no `expected_version`. The decision record lacks obligations, policy versions, resource and attribute digests, and the STEP_UP / REQUIRE_APPROVAL outcomes. | `internal/store/pg_store.go:622-667, 1866`; `internal/handler/handler.go:96-100, 1585-1589` |
| 27 | governance-decision-log-svc | The data model is short of Doc 04 §7.1: `action_subject_type` / `_id` are missing, `policy_version_id` exists only inside a string, and `decided_at` is caller-controlled with no bound. `TRUNCATE` is not blocked by the append-only trigger. | migrations 000001, 000003 |
| 28 | obligations-svc | "Obligations" has three owners with separate tables: obligations-svc, obligation-tracking-svc and filing-tracker-svc. obligation-tracking-svc emits the same `obligation.created` / `.updated` names. | tracker row 78; `obligation-tracking-svc/internal/handler/handler.go:121, 220` |

### Cosmetic / doc drift

| Rank | Gap | Services |
|---|---|---|
| 29 | Error bodies are not RFC 9457 and do not use the §16 stable classes (X9) | All 7 |
| 30 | Malformed ids or over-length strings return 503 instead of 400/404 (X10) | policy, workflow, evidence-requirements, jurisdiction-rules, decision-log, obligations |
| 31 | Event names follow Doc 03 (`policy.version.activated`), not GOV §17 (`PolicyActivated`), V-001 / R-001 (`pdc.*`, snake_case) or the Event Catalogue (`com.zoikosuite.*`) | All 7 |
| 32 | Undocumented surface: 22 `/v1/admin/*` routes (authorization-svc), 8 control-test routes (policy-svc), replay routes (decision-log), all 6 routes (evidence-requirements), `jurisdiction.created` / `.deactivated` events | authorization, policy, decision-log, evidence-requirements, jurisdiction-rules |
| 33 | Stale self-documentation: authorization-svc `progress.md` says "Nothing left inside this service"; policy-svc's "verified live 2026-07-07" predates enforcement; decision-log `CONTEXT.md:105-108` says it never calls authz, but it does; obligations-svc `progress.md` says there is no authz wiring and no compose entry, but both exist; evidence-requirements `context.md` retrofit targets are wrong; jurisdiction tracker row 96 has the wrong route count | authorization, policy, decision-log, obligations, evidence-requirements, jurisdiction-rules |
| 34 | Postman: no collection for jurisdiction-rules, decision-log, policy, obligations or workflow; the evidence-requirements collection is broken by the envelope | 6 of 7 |

---

# Needs clarification — group-level decisions

These questions came up in more than one service. Each answer moves a set of ❓ rows, and in
several cases turns a block of ❌ rows into "future scope". They are listed in order of how
many rows they unblock.

| # | Question | Affects | Why it matters |
|---|---|---|---|
| C1 | **Which spec binds each service?** Doc 03 §8.x (the v1 service specs), or the multi-service packs V-001 (PDC-01…05), R-001 (WFC-01…05) and ZS-JUR-001? | policy-svc (~12 ❌ rows), workflow-svc (~12 ❌ rows), obligations-svc (16 unscored WFC-05 rows; score 65% → 54% if binding), jurisdiction-rules-svc (rows on approval, bitemporal replay, versions, precedence and outcomes) | Decides whether the lifecycle, versioning, deadline and precedence rows are defects or roadmap. |
| C2 | **GOV-xx ownership.** GOV-04 (SoD exceptions and compensating controls), GOV-07 (Evidence Ledger), GOV-12 (maker-checker and break-glass). The decision log and the evidence-requirements gate have no GOV number. | authorization-svc, governance-decision-log-svc, evidence-requirements-svc | GOV-04's exception lifecycle and all of GOV-12 have no implementation anywhere in the estate. GOV-07 is probably owned by evidence-manifest-svc, document-vault-svc or audit-event-store-svc in Group 3. |
| C3 | **Which event naming is the wire contract?** Doc 03 dotted names, GOV §17 PascalCase, V-001 / R-001 names, or the Event Catalogue's `com.zoikosuite.<domain>.<aggregate>.<fact>` | All 7 | Every service currently follows Doc 03, and workflow-history-svc consumes those names. |
| C4 | **Are GOV-xx and V-001 / R-001 paths binding?** (`/internal/v1/gov05/commands/*`, `/v1/packages/*`, `/v1/approvals/{id}/decisions`, `/internal/authorization/decisions`) | policy, workflow, authorization | **Precedent:** the Group 1 audit ruled GOV-01's "Illustrative contract surface" column illustrative, not binding (23 Sep 2026). The same column heading appears in GOV-03 / 05 / 06. |
| C5 | **Is "evaluate" a material write?** The envelope treats `POST …/evaluate` as one and demands `Idempotency-Key` and `X-Legal-Entity-Id`. authorization-svc exempts `/v1/authorize`. | policy-svc, evidence-requirements-svc | Decides whether to change the services' envelope policy or fix every caller. |
| C6 | **Must evidence writes block the decision?** Doc 03 §3.9 and §8.1 say evidence failure must not block evaluation; V-001 §14.1 step 7 and GOV §1 say no material success without evidence. | policy-svc, governance-decision-log-svc, workflow-svc | policy-svc chose best-effort, which is also what makes X2 silent. |
| C7 | **Who owns role and permission-bundle master data?** authorization-svc (Doc 03 §8.3 "RBAC mappings") or access-control-svc (§9.4 "role catalogues")? Tracker row 79 is Not Started. | authorization-svc | Decides where gap 1 has to be fixed. |
| C8 | **Who owns obligations, filing calendars and entity-jurisdiction mappings?** obligations-svc vs obligation-tracking-svc vs filing-tracker-svc; jurisdiction-rules-svc (Doc 03 §8.2 "Owns") vs tenant-entity-registry-svc and obligations-svc (where the code puts them) | obligations-svc, jurisdiction-rules-svc | |
| C9 | **Does `NO_REQUIREMENTS_DEFINED` permit or block?** | evidence-requirements-svc and its callers | The catalog starts empty and every caller permits, so once X1 is fixed every gated action would pass with no evidence. |

---

# 1/7 — jurisdiction-rules-svc (:8082) vs Doc 03 §8.2, its own openapi.yaml, ZS-JUR-001 and V-001 PDC-02

**Contract extracted from:** `03-microservices.md` §8.2 Jurisdiction Rules Service (L509-568),
the primary per-service spec, together with §3.7 idempotency, §3.8 observability, §3.10 failure
modes and §05/§06 Tier-0. Also:
- `04-data-model.md` §7.3 (L1017-1022: "jurisdiction rules are effective-dated"; basis snapshots)
- `data_classification_audit.md` L14 and §2.11
- the service's own `openapi.yaml` v1.1.0 (921 lines), diffed as a published contract
- ZS-JUR-001 §3, §5-10, §15, §25-28
- V-001 §8 (bitemporal), §11-12 (PDC-02 resolution), §19-20
- Governance Control Plane §16-18. This service is not one of GOV-01…12, so only the shared
  sections apply.
- Event Catalogue §4-6 and the API standard
- `known-gaps.md`, `backend-completion-tracker.md` rows 4, 66, 82i, 84c, 85, 96, and
  `master-register-findings-2026-08-27.md` §1.4

**Code:** `services/jurisdiction-rules-svc/`. Files read:
- `cmd/server/main.go`
- `internal/{handler,store,authz,events,envelope,config,telemetry,health}`
- migrations 000001-000004
- `deployments/docker-compose.yml` L763-841, `traefik-dynamic/all-services.yml` L1-12 and
  L288-294, `scripts/seed-demo-rbac.ps1` L397-399, `scripts/create-app-roles.sh` L132

**Framing.** The code matches its own `openapi.yaml` closely: all 12 routes, request and
response schemas, 404/503 fail-closed behaviour, and authorization on every admin write. The
score is lost in three places:
- Doc 03 §8.2 scope that was never built: compliance calendar, entity mappings, the
  validate-action API, and consumers.
- Event delivery guarantees.
- The stricter ZS-JUR-001 and V-001 rules (approval, bitemporal replay, immutable versions,
  explicit precedence). Whether those two apply to this service is itself ❓ (C1).

## Implemented surface

Routes are mounted in `handler.go:182-200`, and health and metrics in `main.go:157-159`.

| Method | Path | Handler | Authz action |
|---|---|---|---|
| GET | /v1/jurisdictions | ListJurisdictions :281 | open |
| GET | /v1/jurisdictions/{id} | GetJurisdiction :228 | open |
| GET | /v1/jurisdictions/{id}/ancestors | GetAncestors :328 | open |
| GET | /v1/jurisdictions/{id}/rules | GetRules :369 | open |
| GET | /v1/jurisdictions/{id}/rule-pack | GetRulePack :428 | open |
| GET | /v1/rules/{id} | GetRule :460 | open |
| GET | /v1/rules/{id}/drift-events | GetDriftEvents :484 | open |
| POST | /v1/admin/jurisdictions | CreateJurisdiction :518 | JURISDICTION_CREATE |
| POST | /v1/admin/jurisdictions/{id}/deactivate | DeactivateJurisdiction :596 | JURISDICTION_DEACTIVATE |
| POST | /v1/admin/jurisdictions/{id}/rules | CreateRule :640 | JURISDICTION_RULE_CREATE |
| POST | /v1/admin/rules/{id}/transition | TransitionRuleStatus :747 | JURISDICTION_RULE_TRANSITION |
| POST | /v1/admin/rules/{id}/drift | RecordDrift :823 | JURISDICTION_RULE_RECORD_DRIFT |
| GET | /healthz, /readyz, /metrics | main.go:157-159 | open, envelope-exempt |

**Middleware** (`main.go:137-150`): RequestID, RealIP, Recoverer, otelchi, metrics,
correlation ID, Logger, then `envelope.Middleware`.
- The envelope runs in `write-strict` because `ZS_ENVELOPE_ENFORCEMENT` is unset.
- On every POST it requires X-Tenant-Id, an actor (X-Principal-Id or X-Workload-Id),
  X-Request-Id, X-Correlation-ID, X-Source-Channel and Idempotency-Key
  (`envelope/policy.go:185-227`).
- A request missing any of these gets 401 (tenant or actor missing) or 400 otherwise
  (`policy.go:298-305`).

**Events:** one topic, `zoiko.jurisdiction.events` (`config.go:90`). Types are defined at
`events/publisher.go:34-40`. There are no Kafka consumers: `KAFKA_GROUP_ID` is configured and
never read.

## Doc 03 §8.2 inbound APIs

| Item | Documented | Implemented | Status | Notes |
|---|---|---|---|---|
| Resolve jurisdiction set | 03 §8.2 L533 | `/ancestors` handler.go:188; `/rule-pack` :190 (store findChain pg_store.go:477) | ⚠️ | Resolves the ancestor chain of a jurisdiction id the caller already has. It does not work out which jurisdictions apply to an entity or transaction (the V-001 §11.1 inputs: legal entity, work location, transaction type). |
| Fetch runtime rule pack | 03 §8.2 L535 | handler.go:428, pg_store.go:752 | ✅ | |
| Get effective rule by date | 03 §8.2 L537 | `GET /rules?effective_at=` handler.go:369; pg_store.go:689 | ✅ | Half-open interval. DRAFT is excluded. |
| Validate action against jurisdiction requirements | 03 §8.2 L539 | — | ❌ | No endpoint and no logic. |

## Endpoints vs openapi.yaml

| Item | Documented | Implemented | Status | Notes |
|---|---|---|---|---|
| GET /v1/jurisdictions (type, active, limit ≤ 200, offset) | openapi L64-90 | handler.go:281; pg_store.go:405 | ✅ | |
| GET /v1/jurisdictions/{id}: 200 active, 404 unknown/malformed/inactive, 503 | openapi L92-114 | handler.go:228-265; pg_store.go:366 (22P02 → 404) | ✅ | The tenant-entity-registry validation contract holds. |
| GET ancestors (nearest first; root → `[]`) | openapi L116-137 | handler.go:328; pg_store.go:542 | ✅ | |
| GET rules: SUPERSEDED/RETIRED returned historically; works after deactivation | openapi L141-176 | pg_store.go:689 (FindByIDAny) | ✅ | |
| GET rule-pack: nearest wins, DRAFT/RETIRED excluded, inactive → 404 | openapi L178-214 | pg_store.go:752-800 | ✅ | Matches openapi. The historical-replay problem is covered under Business rules. |
| GET /v1/rules/{id} | openapi L216-234 | handler.go:460 | ✅ | |
| GET drift-events, newest first | openapi L236-263 | handler.go:484; pg_store.go:1076 | ✅ | |
| POST /v1/admin/jurisdictions: 201 / 200 replay / 409 | openapi L267-318 | handler.go:518-585; pg_store.go:553 | ✅ | |
| POST deactivate | openapi L320-343 | handler.go:596-625; pg_store.go:643 | ⚠️ | **Not idempotent.** Every repeat call rewrites `updated_by` and **re-publishes `jurisdiction.deactivated`** (handler.go:615). The handler's own comments say a replay must not re-emit (:571, :784), and Doc 03 §3.7 requires idempotent state changes. |
| POST rules (DRAFT/ACTIVE only; overlap → 409) | openapi L345-401 | handler.go:640-733 | ✅ | See the overlap race below. |
| POST transition (state machine; 409 invalid_transition) | openapi L403-449 | handler.go:747-805; pg_store.go:936 | ✅ | |
| POST drift → {rule, drift_event, changed} | openapi L451-486, L907 | handler.go:823-877; pg_store.go:1002 | ✅ | |
| /healthz, /readyz (DB ping), /metrics | openapi L490-525 | main.go:157-159; health.go:36-40 | ✅ | |

## Schemas

| Item | Documented | Implemented | Status | Notes |
|---|---|---|---|---|
| Request bodies `additionalProperties:false`; single JSON object; 256 KiB cap | openapi L834-905 | handler.go:1011-1033 (DisallowUnknownFields, trailing-data check), MaxBytesReader :87 | ✅ | |
| Request `maxLength` limits (code 32, type/authority 64, rule_code 128) | openapi L838-857 | not validated in handler.go:535-551 / :658-665; the DB columns are VARCHAR(32/64/128) (000001 L27-82) | ⚠️ | An over-length value hits Postgres 22001, falls through the `writeStoreError` default and returns **503 store_unavailable** (handler.go:937-942). openapi implies 400. A caller's typo looks like an outage. |
| Response schemas (Jurisdiction, JurisdictionRule, RulePack, DriftEvent, RecordDriftResponse) | openapi L673-921 | domain/types.go; column lists in pg_store.go | ✅ | Field names and nullability match. |
| Envelope headers required on writes (X-Tenant-Id, X-Request-Id, X-Source-Channel, Idempotency-Key) | **Absent from openapi**, which lists only X-Principal-Id and X-Correlation-ID (L556-569) | envelope/policy.go:185-227 via main.go:150 | ❌ | Mandatory inputs that openapi does not document. A client built from openapi gets `401 envelope_incomplete` on every admin write. The gateway injects X-Tenant-Id and X-Principal-Id, but not X-Request-Id, X-Source-Channel or Idempotency-Key (compose L840). |

## Status codes and errors

| Item | Documented | Implemented | Status | Notes |
|---|---|---|---|---|
| Admin 401 is `missing_principal` only | openapi L612-616 | The envelope refuses first, with 401/400 `envelope_incomplete` plus `violations[]` (envelope/middleware.go:146-160) | ❌ | The error shape and code are undocumented. |
| 413 on transition/drift | openapi documents it only on the create routes (L317, L400) | decodeJSON is shared by all of them (handler.go:1017-1021) | ⚠️ | Cosmetic omission from openapi. |
| 409 `cyclic_hierarchy` on create | openapi L309-316 | only checks parent == own id (pg_store.go) | ⚠️ | Unreachable: the id is generated by the server just above the check, and no re-parent endpoint exists. The documented code is dead. |
| 503 means "cannot answer", never "no"; authz fails closed | openapi L38-40, L638-653 | handler.go:910-916, :937-942 | ✅ | |
| Error contract: RFC 9457 problem+json with stable classes | API standard L81, L260, L471-489; GCP §16 L1219 | `{"error":"<snake_code>"}` handler.go:1087-1093 | ❌ | Group-wide X9. |

## Auth and authz

| Item | Documented | Implemented | Status | Notes |
|---|---|---|---|---|
| Admin routes require a principal; 401 without one; no "system" fallback | known-gaps L292-298; openapi L530-539 | handler.go:890-899 | ✅ | |
| Every admin write gets a positive authz decision; deny → 403, unreachable → 503 | openapi L33-37; known-gaps L273-290 | handler.go:525/604/648/755/831 → authz/client.go:162-274 | ✅ | Fails closed on non-200, an unreadable body and network errors. |
| Action strings are seeded | known-gaps L766-786 | seed-demo-rbac.ps1:397-399 (the 5 actions match ActionType, client.go:158) | ✅ | |
| X-Principal-Id is trusted only because the gateway sets it | openapi L530-539; compose L812-840 | handler.go:891 reads the raw header, with no inbound token or mTLS check | ⚠️ | Host port `8082:8082` is published (compose L771), and the local Traefik route `/jurisdiction-rules-svc` has **no gateway-auth** (all-services.yml L12, L288-294). Anyone who can reach either can impersonate any principal holding JURISDICTION_FULL. Group-wide X5. |
| Legacy `X-Actor-Principal-ID` | known-gaps L292-298 calls it "a header nothing sets" | still accepted as a fallback, handler.go:891 | ⚠️ | An undocumented second identity header. Combined with `X-Workload-Id`, which is enough for the envelope (policy.go:191), it lets a caller choose which principal is recorded in the audit columns. |
| "No domain service may silently fall back to a permit-all stub in production" | known-gaps L273-282; main.go:114-116 | config.go:92 defaults `AUTHZ_SERVICE_URL=http://authorization-svc`, which is on the placeholder list (client.go:283), so **StubAuthZClient permits everything** (client.go:68-75, :353-356). The guard fires only for ENV exactly `production` or `staging` (client.go:335; config.go:116) | ⚠️ | With ENV set to `prod`, `uat`, `preprod` or left unset (default `local`) and the URL left at its default, every admin write is permitted and only a WARN is logged. Compose sets the real URL, so this is not live on the stack. |
| Authz call carries caller context and the envelope | tracker 82i; known-gaps L1500-1520 | client.go:229-233 sends only Content-Type; the body is principal, scope and action | ⚠️ | Works now that authorization-svc treats `/v1/authorize` as a non-write. Decisions are logged with no tenant or caller attribution (X6). No resource id is sent, so authorization is per action, not per jurisdiction or rule. |
| Authz decision caching | not documented for this service | client.go:94 (5 s TTL, grants and denials) | ⚠️ | A revoked grant keeps working for up to 5 s. Undocumented. |
| Read endpoints unauthenticated vs the INTERNAL tier | openapi L30-33 ("public reference data") vs data_classification_audit L14 ("INTERNAL … requires valid tenant session context") and §2.11 (rules = INTERNAL) | handler.go:185-192 are open; the compose gateway route applies gateway-auth to `/jurisdiction` reads anyway (L840) | ❓ | The documents contradict each other, and the gateway contradicts openapi. |
| Maker-checker / approval before a rule governs | ZS-JUR-001 §3 ("cannot be promoted without … approval evidence"), §28; V-001 §18.1 steps 5-7 | CreateRule accepts `rule_status: ACTIVE` directly (handler.go:113-116, :677-685); a transition to ACTIVE needs one principal (:747-795) | ❌ | One principal can create and activate a governing rule with no second approval and no workflow or evidence reference. Doc 03 §8.2 does not require approval; ZS-JUR-001 and V-001 do (C1). |

## Business rules

| Item | Documented | Implemented | Status | Notes |
|---|---|---|---|---|
| Rule state machine DRAFT → ACTIVE → SUPERSEDED/RETIRED | openapi L403-449, L884 | handler.go:93-97; compare-and-set on the prior status (pg_store.go:975, `rule_status = ANY($4)`) | ✅ | Doc 03 does not define it; the service owns it (handler.go:89-92). |
| Creation only in DRAFT/ACTIVE; default DRAFT | openapi L868-876 | handler.go:675-685 | ✅ | |
| No two live rules with the same code and an overlapping period | openapi L349-355; ZS-JUR-001 §10 (two active rules overlapping is a blocker) | pg_store.go:1125, called at :861 and :953 | ⚠️ | **Check-then-act race.** The overlap SELECT runs on the pool, outside any transaction, before the INSERT or UPDATE, and no DB exclusion constraint backs it (migrations 000001-000004). Two concurrent creates or activations can both pass. |
| `effective_to` > `effective_from` | openapi L846, L862, L889 | handler.go:548, :670, :1076; pg_store.go (transition end date) | ✅ | |
| Natural-key idempotent create (200 replay / 409 mismatch) | openapi L271-275, L349 | pg_store.go:587, :877 with attribute comparison | ✅ | A rule replay compares only payload and name, not domain, `effective_to` or status. Minor. |
| Idempotency-Key semantics (same key replays; changed body → 409) | API standard L381; GCP §16; Doc 03 §3.7 | The middleware requires the header (policy.go:224); nothing reads its value | ⚠️ | Replay safety comes from natural keys and state checks, not from the key the client is forced to send. Two different keys with the same body are treated as one request. Group-wide X4. |
| Optimistic concurrency (If-Match / expected_version) | API standard L69, L359, L404; GCP §16 L1213 | none. Transition uses a prior-status compare-and-set (pg_store.go:975); drift uses FOR UPDATE (:1014) | ⚠️ | Protects the state machine, but a caller cannot assert which version it read. |
| No hard delete; end-date and deactivate instead | openapi L16-17; 000001 L17 | pg_store.go:643-660 | ✅ | |
| A deactivated jurisdiction stops resolving (404) | known-gaps L788-800 | pg_store.go:366-386 | ✅ | Deliberate and documented. |
| "Historical actions must always be explainable against the rule set active at the time of execution" | **03 §8.2 Critical Constraint** L565-566; 04 §7.3; openapi L18-20 | Rule status, `effective_to`, drift state and `updated_by` are **updated in place** (pg_store.go:975, :1034), and there is no status-history table. FindRules and FindRulePack filter on the *current* status (:710, :792). The rule pack returns 404 at any `effective_at` once the jurisdiction is inactive (:761) | ⚠️ | A rule activated after its `effective_from` appears effective for dates when it was still DRAFT. A RETIRED rule vanishes from historical packs (:792), even for dates when it governed. Historical packs for a later-deactivated jurisdiction cannot be rebuilt. `/rules` still covers raw per-jurisdiction replay. |
| Bitemporal replay (known_from/known_to, decision_known_at) | V-001 §8.1 (L290-325) | no known_at dimension; only created_at / updated_at | ❌ | Cannot answer "what did the platform know on date X". |
| Immutable released rule versions; rule_version; supersedes reference | ZS-JUR-001 §3 ("released pack versions are immutable"), §7 | no rule_version column; rows are mutated in place; no supersedes link (000001 L73-115) | ❌ | Depends on C1. |
| Precedence is explicit, never a silent "nearest wins" | V-001 §11.1 ("Prohibited: … silently selecting nearest/default package"), §12.1; ZS-JUR-001 §10 | FindRulePack `DISTINCT ON … ORDER BY c.depth ASC, effective_from DESC` (pg_store.go:752-800) is an implicit nearest-wins; there is no precedence metadata | ❌ | openapi documents the implicit rule (L184-186); the higher-order standards forbid it. |
| Resolution outcomes RESOLVED / AMBIGUOUS / INSUFFICIENT / CONFLICTED | V-001 §11.1, §12.2 | the pack always returns 200 with whatever it found (pg_store.go:806-830) | ❌ | An empty pack and a complete pack cannot be told apart. |
| `rule_payload` content: applicability metadata only | openapi L10-12 and 000001 L90-95 (OQ-1 Model B) vs ZS-JUR-001 §6 ("Rule Modules: rates, thresholds, formulas…") | normaliseRulePayload (handler.go:1044) checks only that it is an object | ❓ | The documents conflict, and the code enforces neither reading. Nothing stops rates from being stored. |
| Compliance calendar logic | 03 §8.2 L525; ZS-JUR-001 §15 | none | ❌ | Known open gap: known-gaps L227-236, tracker row 66. |
| Entity-jurisdiction applicability mappings | 03 §8.2 L521 ("Owns") | none. Assignments live in tenant-entity-registry-svc, which calls GET /v1/jurisdictions/{id} | ❌ | Ownership conflict (C8). |
| Legal drift detection (spot when external updates diverge) | 03 §8.2 L559-560 | manual `POST …/drift` only (handler.go:823); no feed consumer and no comparison | ⚠️ | Drift can be recorded, but it is never detected. |
| Drift history is append-only | 000001 L135-139; openapi L21-22 | the code only INSERTs (pg_store.go:1002-1073), but the compose DB role is granted `SELECT, INSERT, UPDATE, DELETE ON ALL TABLES` (create-app-roles.sh:132) | ⚠️ | Not enforced in the DB on the docker path. The supabase variant claims INSERT/SELECT only (supabase/migrations/0002 L169). |
| Record the rule basis used in every governed action | 03 §8.2 L553; 04 §7.3 | the pack returns `resolved_from` plus rule ids (domain/types.go:140-148) | ⚠️ | No pack digest or version is returned for callers to pin, and the service writes no evidence or decision-log record. The basis is only as good as what each caller stores. |

## Events

| Item | Documented | Implemented | Status | Notes |
|---|---|---|---|---|
| `jurisdiction.rule.updated` | 03 §8.2 L543 | publisher.go:37, :145; emitted at handler.go:713, :787 | ✅ | Also emitted for SUPERSEDED and RETIRED. |
| `jurisdiction.rule.activated` | 03 §8.2 L545 | publisher.go:38, :152; handler.go:719, :791 | ✅ | |
| `jurisdiction.calendar.changed` | 03 §8.2 L547 | deliberately absent (publisher.go:28-33) | ❌ | Depends on the compliance calendar. |
| `legal.drift.detected` | 03 §8.2 L549 | publisher.go:39, :159; handler.go:859-863 (only when the state changes and is not CURRENT) | ✅ | |
| `jurisdiction.created` / `jurisdiction.deactivated` | not in 03 §8.2; in openapi L291, L326; ZS-JUR-001 §26 names `jurisdiction.created` | publisher.go:35-36; handler.go:573, :615 | ⚠️ | Doc 03 drift: two events that exist in code are missing from the primary spec. |
| Transactional outbox; event loss not allowed | Event Catalogue §6 L389-429, L1890 (a dual write to DB and broker without an outbox is prohibited); V-001 §20.2 | direct `WriteMessages` after commit (publisher.go:231); a failure is only logged (handler.go:949-957); no outbox table | ❌ | A broker outage or timeout (5 s, main.go:236) after commit loses the event permanently and silently. Group-wide X3. |
| Publish context | `go Publish(ctx)` pattern | not a goroutine; synchronous on `r.Context()` (handler.go:574, :616, :714, :720, :788, :792, :861) | ⚠️ | The goroutine defect is absent. A client disconnect after commit still cancels the request context, the Kafka write fails, and the event is dropped. kafka-go is synchronous, so a nil ErrorLogger is irrelevant here. |
| Event envelope fields | Event Catalogue §4 (id, source, type, subject, time, classification, schemaversion, payloadhash, aggregateversion, residencyregion, publishedat) | publisher.go:62-73 (event_id, event_type, event_version, emitted_at, schema_version "1.0", source_service, jurisdiction_id, actor_id, correlation_id, payload) | ⚠️ | Missing: classification, payloadhash, aggregateversion, causation/request id and residency. Tenant and entity are correctly left out for platform data. |
| Event type and topic naming | Event Catalogue §5.1-5.2 (`com.zoikosuite.<domain>.<aggregate>.<fact>`, `zoikosuite.<domain>.<stream>.v1`) vs Doc 03 names | `jurisdiction.rule.updated` on `zoiko.jurisdiction.events` (config.go:90) | ❓ | The standards conflict; the code follows Doc 03 (C3). |
| Consume `entity.created` | 03 §8.2 L553 | no consumer (no kafka.Reader) | ❌ | |
| Consume `entity.jurisdiction.changed` | 03 §8.2 L555 | none | ❌ | |
| Consume external regulatory feed changes | 03 §8.2 L557 | none; only an `external_feed_reference` text column | ❌ | Drift detection depends on this. |
| Replays do not re-emit (create/transition/drift) | handler.go comments; openapi L295, L378 | handler.go:570-577, :711-724, :786-795, :859 | ✅ | Deactivate is the exception (above). |

## Data model

| Item | Documented | Implemented | Status | Notes |
|---|---|---|---|---|
| Tables: jurisdictions, jurisdiction_rules, drift events; effective-dated | 03 §8.2 Owns; 04 §7.3 | 000001 L23-154 | ✅ | |
| data_classification PUBLIC / INTERNAL | data_classification_audit §2.11 | 000003; pg_store defaults | ✅ | |
| No tenant_id / RLS (platform reference data) | tracker rows 4, 96; master register §1.4 | 000001 L18-19 | ✅ | The RLS empty-GUC and empty-tenant patterns do not apply. Malformed ids return 404, not 500 (22P02 → notFoundOr). |
| Audit columns created_by / updated_by / updated_at | known-gaps L292; 000002 | pg_store.go:643-660, :975, :1034 | ✅ | |
| Type, status and domain stored as VARCHAR data, not enums (OQ-3) | 000001 L9-10; openapi L13-15 | 000001 | ✅ | |

## NFRs, ops and documentation artefacts

| Item | Documented | Implemented | Status | Notes |
|---|---|---|---|---|
| Observability baseline: structured logs, OTel traces, probes, metrics, correlation ids | 03 §3.8 | main.go:54, :70, :140-143, :157-159; telemetry.go:87-99 | ✅ | |
| "Alertable failure states" | 03 §3.8 | no metric for publish failure; only a log line (handler.go:950) | ⚠️ | Silent event loss cannot be alerted on. |
| Port 8082 | input-contract-conformance L191; ownership | config.go:78; compose L771-773 | ✅ | |
| Defined failure mode (fail-closed) | 03 §3.10 | handler.go:910-916; openapi L38-40 | ✅ | |
| Server timeouts, body cap, production config guards | hardening | main.go:169-176; config.go:111-133 | ✅ | |
| Postman collection | `postman/` convention | none for this service | ❌ | Cosmetic. |
| Tracker row 96 says "21 GET / 5 POST" | backend-completion-tracker L354 | 7 GET + 5 POST (handler.go:186-199) | ⚠️ | Doc drift. |
| known-gaps: "Still open: seed-demo-rbac.ps1 does not grant the JURISDICTION_* actions" | known-gaps L271 | they are granted (seed-demo-rbac.ps1:397-399); known-gaps L766 records it as resolved | ⚠️ | A stale sentence that contradicts a later section. |

## What the service does do well

It is essentially complete against its own openapi:
- Strict request decoding, with unknown fields rejected and a 256 KiB cap.
- Malformed ids return 404, not 5xx.
- Every admin write is authorized and fails closed, with the action strings seeded.
- Creates are natural-key idempotent, and replays do not re-emit (except deactivate).
- The rule state machine is guarded by a compare-and-set on the prior status.
- No hard deletes.
- The full observability baseline is in place.

## Compliance

**39 of 79 scored items fully met — 49%** (partials at half: **63.9%**). 17 are ❌ and 3 are ❓.

**Top gaps by risk**

1. **The principal is taken from a raw header with no inbound check** (security).
   handler.go:891; port 8082 is published (compose L771); the local Traefik route has no
   gateway-auth (all-services.yml L12, L288).
2. **Permit-all stub fallback whenever ENV is not exactly `production` or `staging`**
   (security). config.go:92, client.go:283, :335, :353-356.
3. **No maker-checker on activation** (security). One principal can create a rule directly in
   ACTIVE or activate it (handler.go:113-116, :677, :747).
4. **The legacy `X-Actor-Principal-ID` is still accepted, and authz calls carry no caller
   context** (security, attribution). handler.go:891; client.go:229-233.
5. **No outbox; the publish runs on the request context; no metric** (data integrity).
   publisher.go:231, handler.go:949-957.
6. **Overlap check-then-act race** (data integrity). pg_store.go:861, :953, :1125, with no DB
   constraint behind it.
7. **Historical replay is not faithful, and there is no known_at or rule_version** (data
   integrity; breaks the §8.2 critical constraint). pg_store.go:710, :761, :792, :975.
8. **Implicit nearest-wins precedence, with no CONFLICTED or INSUFFICIENT outcome** (data
   integrity). pg_store.go:752-800.
9. **Deactivate is not idempotent, and drift history is not append-only in the DB** (data
   integrity). handler.go:615; create-app-roles.sh:132.
10. **Missing §8.2 scope** (breadth): compliance calendar and its event, entity mappings, the
    validate-action API, and all three consumed events.
11. **openapi drift** (contract):
    - mandatory envelope headers and `envelope_incomplete` responses are missing from openapi
      (policy.go:185-227 vs openapi L556-569), so a client built from openapi fails every write
    - over-length input returns 503 instead of 400 (handler.go:937)
    - `Idempotency-Key` is required but never used (policy.go:224)
    - errors are not RFC 9457 (handler.go:1087)
    - the event envelope has no classification or payloadhash (publisher.go:62)
    - the publish runs on the request context, so a client disconnect drops the event
      (handler.go:574 and siblings)
    - the ENV guard compares two literal strings only (client.go:335)
    - the documented 409 `cyclic_hierarchy` is dead; `jurisdiction.created` / `.deactivated`
      are missing from Doc 03; tracker row 96 has the wrong route count; known-gaps L271 is
      stale; there is no Postman collection

**Needs clarification**

1. **Read authentication and the INTERNAL tier.** openapi says public; data_classification
   says a tenant session is needed; the gateway authenticates reads anyway. Which one is the
   contract?
2. **Which spec binds this service?** Is it the PDC-02 service of V-001 and/or the rule
   registry of ZS-JUR-001? This decides whether the approval, bitemporal, version, precedence
   and outcome rows are defects or future scope (C1).
3. **`rule_payload` content**: applicability metadata only (OQ-1 Model B), or rates and
   thresholds (ZS-JUR-001 §6)?
4. **Event and topic naming**: Doc 03 or the Event Catalogue pattern (C3)?
5. **Ownership of entity-jurisdiction mappings and the compliance calendar** (C8).
6. **X-Tenant-Id on a tenantless service.** The envelope demands a tenant on every write for
   data that by design has none. Is an exemption or a sentinel intended?
7. **Inheritance from deactivated ancestors.** Their rules still enter a child's rule pack
   (findChain does not filter `active_flag`, pg_store.go:477), and no document says whether
   that is intended.

---

# 2/7 — governance-decision-log-svc (:8083) vs Doc 03 §8.7, Doc 04 §7.1 and GOV-07 (nearest section)

**Contract extracted from:** `03-microservices.md` §8.7 Governance Decision Log Service
(L763-786), the primary per-service spec. It covers Purpose, Owns, the
`governance.decision.recorded` event, and the Critical Constraint that decisions must be
queryable by "audit, actor, entity, action, rule basis, and time range". §5.1 (L214) and §06
(L382) list the service as Tier-0.

Also used:
- `04-data-model.md` §7.1 GovernanceDecision (L918-946) and the §7.2 ERD (L1000-1010).
- Governance Control Plane:
  - §1 (L42) "evidence … as part of completion semantics, not as asynchronous best effort"
  - §2 invariants #2, #3, #9, #10, #11
  - GOV-07 (L645-736), the closest section (see the mapping caveat below)
  - §16 envelope and error classes; §17; §18
- API standard (RFC 9457; L381 "same key, changed request → 409"; L456).
- Event Catalogue (§6 outbox; §5.1 naming).
- ZS-DATA-GOV-001 (L85, L102, L111: controlled immutability, retention, legal hold).
- `master-register-findings-2026-08-27.md` L593, `known-gaps.md` L825, L850, L1634, and tracker
  rows 14a, 65, 193.

**Code:** `services/governance-decision-log-svc/`. Files read:
- `cmd/server/main.go`
- `internal/{handler,store,events,envelope,middleware,domain,config}`
- migrations 000001-000006

Callers:
- `policy-svc/internal/decisionlog/client.go` and `policy-svc/internal/handler/handler.go:984-1003`
- `decision-support-svc/internal/clients/governancelog.go`
- `contract-lifecycle-svc/internal/governancelog/client.go`
- `evidence-manifest-svc/internal/aggregator/clients.go`

Wiring:
- `deployments/docker-compose.yml:843-887`
- `traefik-dynamic/all-services.yml:256-263`
- `deployments/gtrm/routing-map.yaml`, which has no entry for this service

`CONTEXT.md` and `PROGRESS.md` were treated as claims, not as the contract.

**Mapping caveat.** The master register (L593) records that this service cites the old Doc 03
and that **no GOV-xx section owns GovernanceDecision**. GOV-07 (Evidence Ledger) is the nearest
section but is mapped to evidence-manifest-svc. Rows that depend on GOV-07's hash, digest and
verification rules are marked ❓ (C2).

## Implemented surface

Routes are defined at `handler.go:113-121`; the probes at `main.go:164-166`.

| Route | Handler | Status codes |
|---|---|---|
| POST /v1/decisions | CreateDecision L192 | 201, 200 (replay), 400, 401, 403, 409, 413, 503 |
| GET /v1/decisions | ListDecisions L533 | 200, 400, 503 |
| GET /v1/decisions/{id} | GetDecision L325 | 200, 400, 404, 503 |
| POST /v1/decisions/{id}/replay | ReplayDecision L380 | 201, 400, 404, 501, 503 |
| GET /v1/decisions/{id}/replay-manifests | ListReplayManifests L502 | 200, 503 |
| GET /healthz, /readyz, /metrics | main.go:164-166 | — |

**Middleware** (`main.go:143-156`, `handler.go:114-115`): RequestID, RealIP, Recoverer,
otelchi, metrics, correlation-ID, logger, the §4 envelope, then TenantContext (which reads the
raw `X-Tenant-Id`).
- The envelope defaults to write-strict, and compose does not override it.
- **On writes it requires:** `X-Tenant-Id`, `X-Principal-Id` (or `X-Workload-Id`),
  `X-Request-Id`, `X-Correlation-ID`, `X-Source-Channel` (a closed vocabulary),
  `Idempotency-Key`, `X-Legal-Entity-Id` and `X-Purpose-Context`, plus `X-Source-System` when
  the channel is import or integration (`internal/envelope/policy.go:185-246`,
  `contract.go:488-500`).
- A missing tenant or actor gets 401. Any other missing field gets **400
  `envelope_incomplete`** (`policy.go:298-305`, `middleware.go:120-135`).
- Write authorization calls authorization-svc with action `GOVERNANCE_DECISION_RECORD`, scoped
  to `legal_entity_id` or the platform scope (`handler.go:89-106`, L235).

**Tables:**
- **`governance_decisions`** (000001, 000004): the primary key is `decision_id` VARCHAR(64) on
  its own. RLS is ENABLE plus FORCE, with USING / WITH CHECK on text equality and no `::UUID`
  cast (000002, 000006). A BEFORE UPDATE OR DELETE trigger blocks mutation (000003).
- **`replay_manifests`** (000005): has an append-only trigger, but **no tenant_id and no
  RLS**.

**Kafka:** produces `governance.decision.recorded` on `zoiko.governance.events` (`config.go:161`,
`publisher.go:491-504`). The publish is synchronous, runs after commit on the request context,
and a failure is logged and swallowed (`handler.go:294-300`). There are no consumers.

## Endpoints and ownership (Doc 03 §8.7)

| Item | Documented | Implemented | Status | Notes |
|---|---|---|---|---|
| Record a governance decision (write endpoint) | 03 §8.7 Purpose "captures every governance evaluation" | handler.go:116, 192-310 | ✅ | POST /v1/decisions. The docs do not specify a path. |
| Query by actor, entity, action, rule basis, time range | 03 §8.7 Critical Constraint | handler.go:533-597; pg_store.go:246-325; indexes in 000001 | ✅ | Params: `actor`, `entity`, `action`, `rule_basis`, `from`, `to`, `limit` (max 200), `offset`. |
| Query by "audit" | 03 §8.7 Critical Constraint (the first dimension listed) | none | ❓ | The docs don't say what "audit" means: an audit id, a link to an audit event, or just "queryable for audit". |
| GET /v1/decisions/{id} | not in 03 §8.7 | handler.go:118, 325-353 | ⚠️ | Undocumented endpoint, yet contract-lifecycle-svc depends on it (VerifyGranted). |
| Replay and replay-manifest endpoints | not in §8.7; the closest is GOV §2 invariant #11 (reproducibility) | handler.go:119-120, 380-513 | ⚠️ | Undocumented surface. Replay works only for `APPROVAL_THRESHOLD` (L409); every other action type gets 501. |
| Owns decision records | 03 §8.7 Owns | 000001 `governance_decisions` | ✅ | |
| Owns evaluation context | 03 §8.7 Owns | 000001 `evaluation_context JSONB` | ✅ | |
| Owns outcome records | 03 §8.7 Owns | 000001 `outcome` | ✅ | Free-text VARCHAR(32). No vocabulary is enforced, by design (types.go:6-7). |
| Owns rule references | 03 §8.7 Owns | 000001 `rule_basis` | ✅ | |
| Owns correlation IDs | 03 §8.7 Owns; GCP §16 "propagated end-to-end" | handler.go:146, 173, 253 | ⚠️ | The stored correlation_id comes from the **body** and is never checked against the envelope's `X-Correlation-ID` (L193), so the two can diverge. |

## Events

| Item | Documented | Implemented | Status | Notes |
|---|---|---|---|---|
| Event `governance.decision.recorded` | 03 §8.7 Published Events; Event Catalogue §5.1 naming | publisher.go:492 | ⚠️ | Matches Doc 03. Does not follow the catalogue pattern `com.zoikosuite.governance.decision.recorded` (C3). |
| Transactional outbox; publication only after commit | GCP §2 invariant #10; Event Catalogue L57/L60/§6 | handler.go:294-300; publisher.go:544 | ❌ | Publishes straight after commit with no outbox. A Kafka failure is only logged, so the event is lost and never retried. Group-wide X3. |
| Publish context | cross-cutting pattern | handler.go:294 (`r.Context()`, synchronous) | ⚠️ | Not the `go Publish(ctx)` bug. But a client disconnect or timeout cancels the write after the row has committed. The 5 s WriteTimeout (main.go:251) is on the request path. |
| Event envelope fields | Event Catalogue CloudEvents example L355 (causationid, payloadhash, schema); GCP §2 invariant #9 | publisher.go:427-439, 492-503 | ⚠️ | No causation_id, workflow_instance_id or payload hash. `jurisdiction_context` is filled with `rule_basis` (L500), a substitute value. |
| Event partition key | Event Catalogue §5 | publisher.go:543 (key = correlation_id) | ⚠️ | Not keyed by the aggregate (`decision_id`). |

## Data model (Doc 04 §7.1 GovernanceDecision)

| Item | Documented | Implemented | Status | Notes |
|---|---|---|---|---|
| `governance_decision_id` | 04 §7.1 | `decision_id` (000001:6) | ⚠️ | Renamed. |
| `principal_id` | 04 §7.1 | `actor_id` (000001) | ⚠️ | Renamed. |
| `action_subject_type` / `action_subject_id` | 04 §7.1 | absent (types.go:46-48 defers them to evaluation_context) | ❌ | Missing columns, and no caller writes them. |
| `policy_version_id` (nullable) | 04 §7.1; GCP §16 "policy_versions: exact versions used" | only encoded as the string `code:version` inside `rule_basis` (policyclient ParseRuleBasis; handler.go:417) | ⚠️ | Can't be queried as a column. Parsing breaks if the format ever drifts. |
| `jurisdiction_rule_basis` / `authorization_outcome` | 04 §7.1 | `rule_basis` / `outcome` | ⚠️ | Renamed and merged. |
| `workflow_instance_id` (nullable) | 04 §7.1 | 000004; handler.go:149, 254 | ✅ | `causation_id` is present too. No caller sends either. |
| `decision_timestamp` | 04 §7.1 | `decided_at`, supplied by the caller (handler.go:239-242) | ⚠️ | Callers can backdate or future-date it with no bound. The server's `stored_at` is never returned or selected (pg_store.go:194-197). |
| Immutable / append-only | 03 §8.7 "immutable evidence"; GOV-07 negative path #1 | 000003:16-18; 000005 trigger | ✅ | The trigger fires even for a superuser. |
| Immutability against TRUNCATE | GOV-07 "no general-purpose admin rewrite" | 000003 covers UPDATE OR DELETE only | ⚠️ | `TRUNCATE governance_decisions` is not blocked, and the down migration drops the trigger. |
| Integrity verification (canonical hash, digest chain) | GOV-07 server-derived "canonical hash; manifest digest"; §18 "content hash" | none | ❓ | No hash or chain exists. Whether GOV-07 binds this service is unresolved (C2). |
| `evidence_id` returned for a material decision | GCP §16 | the decision row is returned (handler.go:309) | ❓ | Unclear whether `decision_id` counts as the evidence_id. |
| Key scoping | 04 §7.1 (tenant-owned) | 000001:6 (PK decision_id, global) | ⚠️ | A global key namespace, mitigated by the cross-tenant collision check below. |
| Retention and legal-hold metadata | ZS-DATA-GOV-001 L102, L111 | none; CONTEXT.md:112-114 says "indefinitely" | ❓ | No record class, retention rule or hold applies. |

## Auth, tenant and envelope

| Item | Documented | Implemented | Status | Notes |
|---|---|---|---|---|
| Tenant is server-resolved, never trusted from a client header | GCP §2 invariant #2; §16 tenant_id "never trusted from arbitrary client header" | tenant.go:34-36; handler.go:219-233 | ⚠️ | A body/header mismatch gives 403, which is good. But the header itself is trusted raw. Host port 8083 is published (compose:851), the local Traefik route has no ForwardAuth (all-services.yml:256-263), and there is no GTRM route, so the service is internal-only by convention. Group-wide X5. |
| RLS isolation | 05-security; RLS traps | 000006:28-33; pg_store.go:83-101 | ✅ | FORCE plus WITH CHECK. The column is text, so the `::UUID` empty-GUC trap does not apply. An explicit `tenant_id = $n` is on every statement. Compose uses a non-superuser DB_USER (compose:857), but the config default is `postgres` (config.go:167). |
| **Tenant isolation of replay_manifests** | GCP §2 invariants #2, #3 | 000005 (no tenant_id, no RLS); pg_store.go:353-398 (bare pool); handler.go:502-513 (no tenant check) | ❌ | **Cross-tenant disclosure.** Any caller can read any tenant's replay manifests (policy_version_id, outcomes, principal) by decision_id, even with no X-Tenant-Id at all. |
| Empty tenant on read gets 4xx, not 500 | cross-cutting pattern | handler.go:329-331, 384-386, 536-538 | ✅ | Returns 400 `missing_tenant_id`, which is inconsistent with the 401 `tenant_scope_missing` that POST returns (L220-225). |
| Writes restricted to trusted, authorized callers | GOV-07 "append via trusted services" | handler.go:195-198, 235-237, 89-106 | ✅ | X-Principal-Id plus an authz check on `GOVERNANCE_DECISION_RECORD`. |
| **Replay authn, authz and actor attribution** | GCP §16 subject_id "authenticated server-resolved"; invariant #9 | handler.go:380-471 (no requirePrincipal, no authorize); L393, L456 | ❌ | Anyone with a tenant header can trigger a replay. `replayed_by_principal_id` is taken from the **body**, so a permanent, append-only manifest can be attributed to any principal. |
| **Read authorization** ("read/export strongly scoped"; sensitive reads fail closed) | GOV-07 Authorization; GCP §2 invariant #3 | handler.go:325, 533, 502 (no authz call) | ❌ | Anyone holding a tenant header can read that tenant's whole body of governance evidence. |
| The recorded actor is bound to the authenticated principal | GCP §2 invariant #9; §16 | handler.go:141, 165, 248 (body `actor_id`); the principal is used only for authz | ⚠️ | An authorized writer can record decisions in any actor's name. |
| Envelope fields required on write | GCP §16 (purpose/reason only for "privileged, destructive or emergency commands"; idempotency_key for stateful I2-I4) | contract.go:488-500; policy.go:185-246 | ⚠️ | Requiring **purpose_context** on an evidence append is documented neither in §16 nor in Doc 03 §8.7. None of the required headers are documented for this service. |
| **Caller writes are in contract (policy-svc)** | GCP §1 L42 "not asynchronous best effort"; invariant #9 | policy-svc `decisionlog/client.go:135-153` sends only X-Principal-Id, X-Tenant-Id and X-Correlation-ID; the error is swallowed at policy-svc `handler.go:994-1003` | ❌ | **Every write is refused** with 400 `envelope_incomplete` (missing request_id, source_channel, idempotency_key, legal_entity_id and purpose_context), or 401 if the actor is empty. policy-svc only logs the error, so **no policy evaluation reaches the ledger**. The client does not call `svcenvelope.ForwardTo`, and its body sends `"GLOBAL"` as the tenant sentinel (client.go:29). Group-wide X1 and X2. |
| Read callers are in contract | GCP §16 (request_id and correlation_id propagated) | decision-support governancelog.go:39; contract-lifecycle client.go (X-Tenant-Id only); evidence-manifest `forwardTenant` | ⚠️ | They pass only because write-strict admits non-compliant reads. Setting `ZS_ENVELOPE_ENFORCEMENT=strict` would break all three, contract activation included. |
| Fail closed on authz outage or deny | GCP §2 invariant #3; GOV-03 failure semantics | handler.go:95-105 | ✅ | 403 on deny, 503 when unavailable. |
| Tier-0 production guard on events | GCP §1 "no evidence afterthought" | main.go:231-236 | ✅ | Refuses to start in production or staging with no brokers configured. |

## Idempotency, errors and replay

| Item | Documented | Implemented | Status | Notes |
|---|---|---|---|---|
| Idempotency semantics | API standard L381 (same key, changed request → 409); GCP §16 `IDEMPOTENCY_MISMATCH` | Idempotency-Key is required (policy.go:220-227) but never read; dedup is on decision_id (pg_store.go:112-172); handler.go:309 echoes the request | ❌ | A repeat POST with the same decision_id and a **different body** returns 200 and shows the *new, unstored* content as though it had been recorded. There is no mismatch detection. Group-wide X4. |
| Cross-tenant decision_id collision | (derived) | pg_store.go:146-165; handler.go:260-275 | ✅ | Returns 409 `decision_id_conflict`. |
| Error format: RFC 9457 with stable classes | API standard §12 L471, L260; GCP §16 | handler.go:600-606; middleware.go:120-135 | ❌ | Ad-hoc `{"error": ...}` codes; none of the §16 classes. Group-wide X9. |
| Documented status codes | the handler's own doc comments | handler.go:186-191 lists 201/200/400/503, but the code also returns 401/403/409/413; L322 lists 400 | ⚠️ | The self-documentation has drifted. No external doc specifies status codes. |
| Historical decisions are reproducible | GCP §2 invariant #11 | handler.go:380-498; policyclient | ⚠️ | Only APPROVAL_THRESHOLD can be replayed. Replay is not idempotent (every call adds a manifest, and Idempotency-Key is ignored) and publishes no event. |
| Replay write idempotency | API standard L381; GCP §16 idempotency_key | handler.go:448-462 (a new UUID on every call) | ⚠️ | A retried replay creates duplicate permanent manifests. |
| List input validation | API standard (400 for invalid input) | handler.go:571-580; pg_store.go:293-301 | ⚠️ | A non-numeric limit or offset is silently ignored. A **negative offset** reaches Postgres and returns **503 store_unavailable** instead of 400. |

## NFRs and self-documentation

| Item | Documented | Implemented | Status | Notes |
|---|---|---|---|---|
| Tier-0 observability | 03 §3.8, §06 | main.go:73-85, 146-147, 163-166 | ✅ | OTel traces, Prometheus metrics, and readyz with a DB check. |
| Request size bound | NFR hygiene | handler.go:612-629 | ✅ | 256 KiB; 413 above that. |
| CONTEXT.md claims vs code | CONTEXT.md:105-108 ("must never itself require a governance check") | handler.go:235 calls authorization-svc | ⚠️ | Either a stale claim or an undocumented design reversal. CONTEXT.md:134 still describes a stubbed Kafka writer. |
| Governing spec mapping | master register L593 | the service cites 03 §8.7 only | ❓ | No GOV-xx section owns GovernanceDecision (C2). |
| Postman collection | `postman/` convention | none covers this service | — | Not scored. |

## What the service does do well

- The core write and query contract of Doc 03 §8.7 is met.
- The main table is append-only by a trigger that binds even a superuser.
- RLS is forced with WITH CHECK and immune to the `::UUID` trap.
- Writes are authorized and fail closed.
- A cross-tenant id collision returns 409 rather than leaking.
- A read with no tenant gets 400, not 500.
- The Tier-0 observability baseline is in place.
- The service refuses to start in production without Kafka.

## Compliance

**16 of 47 scored items fully met — 34%** (partials at half: **58.5%**). 8 are ❌ and 5 are ❓.

**Top gaps by risk**

1. **Replay manifests are readable across tenants** (security). handler.go:502-513 has no
   tenant check, pg_store.go:376-398 uses the bare pool, and migration 000005 gives the table no
   tenant_id and no RLS.
2. **Replay has no authn or authz, and its attribution can be forged** (security).
   handler.go:380-471; `replayed_by_principal_id` comes from the body (L393, L456) and lands in
   an immutable record.
3. **No read authorization on governance evidence** (security). handler.go:325, 533, 502 gate
   on the tenant header only.
4. **The tenant header is trusted raw while port 8083 is published** (security).
   tenant.go:34-36; compose:851; all-services.yml:256-263.
5. **`actor_id` is not bound to the authenticated principal** (security, attribution).
   handler.go:248 vs L195.
6. **Every policy-svc evidence write is refused and swallowed** (data integrity). The header
   requirement is documented nowhere for this service: policy-svc `decisionlog/client.go:135-153`
   vs `contract.go:488-500` and `policy.go:185-246`; swallowed at policy-svc
   `handler.go:994-1003`.
7. **A changed body on a repeat POST gets 200, and content that was never stored is echoed
   back** (data integrity). handler.go:309; the Idempotency-Key is never read.
8. **No outbox; a failed publish is only logged** (data integrity, breaks invariant #10).
   handler.go:294-300.
9. **Data-model shortfalls** (data integrity): action_subject_type/id missing,
   policy_version_id buried in a string, decided_at caller-controlled and unbounded.
10. **TRUNCATE is not blocked** (data integrity). 000003:16-18.
11. **Doc drift** (cosmetic): errors are not RFC 9457; the event name is not in the catalogue
    pattern; self-documented status codes have drifted; CONTEXT.md is stale; a negative offset
    returns 503; there is no Postman collection.

**Needs clarification**

1. What does the "audit" query dimension in 03 §8.7 refer to?
2. Which GOV-xx section owns GovernanceDecision? Do GOV-07's hash, digest and verification
   obligations apply here (C2)?
3. Is `decision_id` the §16 `evidence_id`?
4. Which retention or record class applies to governance decisions?
5. Should this service require `purpose_context` on append at all? §16 scopes purpose to
   privileged, destructive or emergency commands.
6. Must callers treat this write as blocking (GOV §1 "not best effort")? CONTEXT.md:170-185
   leaves the choice to callers, and policy-svc chose best-effort (C6).

---

# 3/7 — policy-svc (:8085) vs Doc 03 §8.1, GOV-05 and V-001 (PDC-01…05)

**Contract extracted from:**
- `03-microservices.md` §8.1 Policy Service (L428-508), the primary per-service spec, plus
  §3.9 (L167: decision cache and governance latency) and §06 Tier-0 (L362).
- `04-data-model.md` §7.1 Policy / PolicyVersion (L840-874).
- Governance Control Plane: §2 invariants (L47-75), **GOV-05 Policy & Rules Registry /
  Resolver** (L461-553), §16 envelope and error classes (L1196-1221), §17 events, §18 data model.
- V-001: §5 states, §6 invariants PDC-I-01…30, §7 data model, §8 temporal model, §19 API,
  §20 events and outbox.
- Tracker row 3 (L69) and row 82i, `doc7-acceptance-checklist-traceability.md` GOV-01/02
  (L39-40), and `input-contract-conformance.md` L224.

**Code:** `services/policy-svc/`. Files read:
- `cmd/server/main.go`
- `internal/handler/{handler.go,control_test_handler.go}`
- `internal/store/pg_store.go`
- `internal/decisionlog/client.go`, `internal/events/publisher.go`, `internal/authz/client.go`
- `internal/envelope/*`, `internal/middleware/tenant.go`
- migrations 000001-000005
- `deployments/docker-compose.yml` L843-968
- `governance-decision-log-svc/cmd/server/main.go:156` and its `internal/envelope/contract.go`

**Framing.** V-001 specifies a **five-service PDC suite**: registry, applicability, evaluation
engine, evidence, and simulation. policy-svc is a v1 registry plus evaluator. The PDC and GOV-05
lifecycle items are scored below, but whether policy-svc is meant to meet them is ❓ (C1).
- **Against Doc 03 §8.1 alone** (16 rows: ✅ 6, ⚠️ 4, ❌ 6) the service scores
  **(6 + 2) / 16 = 50%**.
- Most of the rest is the review → approve → schedule → suspend lifecycle, which v1 never
  attempted.

The two previously recorded defects both **still hold in the current code**: evidence writes
fail silently, and effective dates are not enforced.

## A. Endpoints

| Item | Documented | Implemented | Status | Notes |
|---|---|---|---|---|
| Get applicable policy set | 03 §8.1 Inbound APIs | `GET /v1/policies` handler.go:169, 726-775 | ✅ | Requires `policy_type`. Tenant comes from the header; a foreign `?tenant_id` gets 403 (755). |
| Evaluate policy against action context | 03 §8.1 | `POST /v1/policies/evaluate` handler.go:170, 839-912 | ⚠️ | Only APPROVAL_THRESHOLD is evaluated (903). Every other type gets 501 (907). |
| Retrieve policy version history | 03 §8.1 | `GET /v1/policies/{id}/versions` handler.go:173, 623; pg_store.go:421-460 | ✅ | Tenant-scoped (global plus own tenant) with RLS. The earlier unscoped-history gap is closed. |
| Validate threshold applicability | 03 §8.1 | none | ⚠️ | Folded into Evaluate (progress.md:90); there is no dedicated endpoint. |
| Create policy / version / activate | implied by GOV-05 commands; V-001 §19 `POST /v1/policies` | handler.go:168, 171, 172 | ⚠️ | REST-style paths, not the documented `/internal/v1/gov05/commands/*` or `/v1/packages/*` (C4). |
| GetPolicyVersion (exact version) | GOV-05 queries; V-001 §19 `GET /v1/packages/{version}` | `GET /v1/policy-versions/{id}` handler.go:174, 672 | ✅ | Tenant required (681). Scoped lookup with no unscoped fallback (pg_store.go:292-296). |
| ResolveEffectivePolicy as of a time | GOV-05 NFR "historical as-of resolution"; V-001 §8.1 decision_as_of / known_at | none (FindApplicableVersions takes no time parameter, pg_store.go:606) | ❌ | Can only resolve "whatever is ACTIVE now". |
| ComparePolicyVersions / ExplainPolicyResolution | GOV-05 queries | none | ❌ | |
| SubmitPolicyReview / ApprovePolicy / ScheduleActivation / EmergencySuspendPolicy / SupersedePolicy | GOV-05 commands | none | ❌ | Supersede happens only as a side effect of activation (pg_store.go:539). |
| Control-test / attestation routes (8 routes) | not in §8.1 or GOV-05; they come from doc7 §E3/§E6 (traceability GOV-02) | control_test_handler.go:21-29 | ⚠️ | Code with no matching entry in the service spec. |

## B. Auth, tenant and envelope

| Item | Documented | Implemented | Status | Notes |
|---|---|---|---|---|
| Actor comes from trusted identity, not from the body | GCP §2 invariant #2; §16 "subject_id server-resolved" | create, version and activate use the `X-Principal-Id` header (handler.go:90-99). **Evaluate takes the actor from the body field `evaluated_by_principal_id`** (803, 853, 988) | ❌ | Evaluate never reads the principal header. The body value is recorded as `actor_id` and forwarded as `X-Principal-Id` to the decision log (decisionlog/client.go:143), so the ledger's author is whatever the caller typed. |
| Evaluate is authorized | GCP §2 invariants #1, #3; V-001 §14.1 step 1 "Authenticate/authorize caller" | no `authorize` call in Evaluate (handler.go:839-912) | ❌ | Any caller carrying the envelope headers can evaluate. |
| Reads are authorized (list, history, get) | GCP §2 invariant #1; GOV-05 "Read-only; scoped" | tenant checked (627, 681, 739); no authz call | ⚠️ | Scoped by tenant, but no permission check. |
| Mutations are authorized per scope | GOV-05 authz | handler.go:247, 401-407, 542-548; global versions need the `_GLOBAL` actions; fails closed with 503 (135-158) | ✅ | |
| Tenant is never trusted from the client | GCP §2 invariant #2; §16 | the body/query tenant must equal `X-Tenant-Id` (124-133); the tenant header is read raw (middleware/tenant.go:27) | ⚠️ | Relies on the gateway; the local Traefik route has no auth middleware (X5). The body's legal entity is not checked against the header or the MDM. |
| Envelope contract enforced | ZS-ARCH-SVC-001 §4; GCP §16 | main.go:180 write-strict (no compose override, compose:935-954); contract at envelope/contract.go:14 | ⚠️ | `LegalEntityID: RequiredOnWrite` forces `X-Legal-Entity-Id` on `POST /v1/policies` (a platform-wide header row) and on global evaluations. Evaluate has no `MaterialWrite` override, so it must carry `Idempotency-Key` even though it is a read-type computation (C5). |
| Maker-checker / SoD on activation | GOV-05 SoD ("author cannot be sole approver; production activation requires approved artifact"); V-001 PDC-I-24 | ActivateVersion checks only POLICY_VERSION_ACTIVATE (handler.go:542-548); no creator ≠ activator check; no approval reference | ❌ | One principal holding both grants can author and activate a spend threshold. Group-wide X7. |
| Emergency suspension is privileged | GOV-05 authz | none | ❌ | |
| Control-test / attestation reads are scoped | GCP §2 invariant #1 | the GET handlers check no principal, tenant or authz (control_test_handler.go:124-134, ~230-260); the tables have no tenant_id (000004) | ❌ | Any caller can read any definition, execution, effectiveness record or attestation. Reads pass under write-strict. |
| RLS on tenant data | 05-security; tracker row 3 | 000005:22-33 ENABLE+FORCE with `NULLIF(current_setting(...,true),'')::uuid`; withRLS pg_store.go:57-71 | ✅ | The empty-GUC trap is avoided. |
| A malformed id gets 400/404, not 5xx | platform convention | a non-UUID `policy_id` or `version_id` reaches a uuid comparison, the store errors, and the caller gets **503** (pg_store.go:160-167, 305-309; handler.go:699) | ❌ | A malformed id reads like an outage (X10). |
| Request size cap | NFR | handler.go:1060-1077, 256 KiB, 413 | ✅ | |

## C. Business rules and versioning

| Item | Documented | Implemented | Status | Notes |
|---|---|---|---|---|
| **Effective dating gates applicability** | 000001:70-71 comment; GOV-05 negative path #1 ("Future policy cannot apply before effective time"); V-001 PDC-I-27 | FindApplicableVersions filters only `version_status='ACTIVE'` (pg_store.go:620-627); there is no `effective_from <= now` | ❌ | **Confirmed:** a future-dated ACTIVE version decides immediately. |
| **Expiry (`effective_to`) is honoured** | same | not filtered (pg_store.go:620-627) | ❌ | An expired version keeps deciding. |
| Valid temporal range | V-001 §10.1 step 4 "invalid temporal ranges" | only a non-zero `effective_from` is checked (handler.go:331-338); `effective_to` > `effective_from` is not checked | ❌ | |
| No "latest by timestamp" fallback; conflicts return CONFLICTED | GOV-05 failure semantics; V-001 PDC-I-08, §12.2 | ties are broken by `effective_from DESC` across distinct policies of the same type and tier (pg_store.go:602-605, 624-627); Evaluate takes `matches[0]` (handler.go:900) | ❌ | This is exactly the prohibited last-write-wins behaviour. |
| Overlapping conflicting rules are blocked | GOV-05 negative path #2 | only one ACTIVE per (policy, tenant, LE) (000001:103-109); overlap across policies of the same type is not detected | ❌ | |
| Released versions are immutable | GCP §2 invariant #4; V-001 PDC-I-02 | no UPDATE of `rule_payload` in code; a payload mismatch on the dedup key gets 409 (pg_store.go:375-381) | ⚠️ | Enforced only by the application; no DB trigger or grant stops an UPDATE. |
| Every policy version is preserved | 03 §8.1 evidence | no DELETE path; SUPERSEDED rows are kept (pg_store.go:539-547) | ✅ | |
| Effective-dated activation (actor, time) is preserved | 03 §8.1; 04 activated_by/at | 000002; stamped at pg_store.go:479-489 | ✅ | |
| Atomic supersede; one ACTIVE per scope | V-001 §19.1 | single transaction (pg_store.go:517-584) plus a partial unique index | ✅ | |
| State model | GOV-05 Draft / Review / Approved / Scheduled / Active / Suspended / Superseded / Retired | DRAFT / ACTIVE / SUPERSEDED only; RETIRED is declared (000001:75) but never reached | ❌ | |
| Explicit GLOBAL scope | doc7 §F1 | 000003 CHECK; domain.DeriveScopeType; a global version naming an LE is refused (handler.go:389-395) | ✅ | |
| Exact decimal comparison | V-001 PDC-I-11 | big.Rat (handler.go:922-942, 979) | ✅ | |
| Idempotent create | Doc 03 doctrine | policy_code dedup 200/201/409 (pg_store.go:173-222); version dedup (319-393) | ✅ | |
| Evaluation is safely repeatable | 03 §8.1 Idempotency; V-001 §19.1 | `decision_id` required (handler.go:860); the result is pure | ⚠️ | The spec also needs the same `decision_request_id` to reconcile to the original result. Nothing records it, and the evidence write fails, so there is nothing to reconcile against. |
| Activate with no body | handler comment "body is now optional" (handler.go:470) | `decodeJSON` on an empty body returns io.EOF, so the caller gets **400 invalid_json** (handler.go:503, 1067-1073) | ❌ | The documented optional-body call fails. |
| No applicable policy: fail closed with a stable error | GOV-05 failure; GCP §16 `POLICY_NOT_EFFECTIVE` | 404 `no_applicable_policy` (handler.go:894) | ⚠️ | Leaves the decision to the caller, which is consistent with V-001 §25.1, but the error code is not the canonical one. |
| Stable error classes | GCP §16 | ad-hoc strings (`authorization_denied`, `invalid_transition`, …) | ❌ | Group-wide X9. |
| Optimistic concurrency / expected_version | GCP §16; GOV-05 "expected_version where stateful" | none; `X-Expected-Version` is parsed but never used | ❌ | |

## D. Evidence and side effects

| Item | Documented | Implemented | Status | Notes |
|---|---|---|---|---|
| **Preserve the evaluation basis (decision-log write)** | 03 §8.1 evidence; GCP §2 invariant #9; V-001 PDC-I-17 and §14.1 step 7 ("material success cannot be returned without evidence correlation") | decisionlog/client.go:133-153 sends only Content-Type, X-Principal-Id, X-Tenant-Id and X-Correlation-ID. governance-decision-log-svc runs write-strict (its main.go:156) and requires X-Request-Id, X-Source-Channel, Idempotency-Key, X-Legal-Entity-Id and X-Purpose-Context (its contract.go). The error is logged and dropped, and **200 is still returned** (handler.go:994-1004) | ❌ | **Confirmed:** every evaluation's evidence write is refused with `envelope_incomplete`, and the caller is never told. Global scope also sends `X-Tenant-Id: GLOBAL`, which is not a UUID. The authz client in the **same service** forwards the full envelope correctly (authz/client.go:218-252), but the decision-log client was never updated to match. progress.md:206 "verified live 2026-07-07" predates enforcement. Group-wide X1 and X2. |
| evidence_id returned | GCP §16 | evaluateResponse has only result, policy_version_id and rule_basis (handler.go:811-815) | ❌ | |
| Evidence content: input fingerprint, evaluator version, selected rule IDs | V-001 PDC-I-17 | rule_basis is `code:version_id` only (handler.go:982) | ❌ | |
| Decision cache ("cache-accelerated") plus invalidation | 03 §8.1 scaling; §3.9 L167 | none | ❌ | No cache and no invalidation consumer. |

## E. Events

| Item | Documented | Implemented | Status | Notes |
|---|---|---|---|---|
| Published: policy.created / updated / version.activated / rule.retired | 03 §8.1 | publisher.go:246-298, topic `zoiko.policy.events` (compose:954) | ✅ | Names match Doc 03. Emitted only on a real transition (handler.go:291, 447, 577). |
| GOV-05 events: PolicyApproved / PolicyActivated / PolicySuspended / PolicySuperseded | GCP GOV-05; §17 | policy.version.activated ≈ PolicyActivated; policy.rule.retired ≈ PolicySuperseded; nothing for Approved or Suspended | ⚠️ | Names differ from Doc 03 (C3). |
| Outbox after commit; at-least-once delivery | GCP §2 invariant #10; V-001 §20.2 | synchronous `WriteMessages` after commit on the request context; a failure is only logged (handler.go:297-303, 449-455, 578-593; publisher.go:329) | ❌ | No outbox. A Kafka failure or client disconnect loses the event permanently, and nothing can detect it. `go Publish(ctx)` is **absent**. The ErrorLogger issue does not apply, because the service only produces. |
| Event envelope fields | Doc 03 §19 | publisher.go:194-206: no jurisdiction_context; rule.retired has no actor | ⚠️ | |
| Consumers of policy events exist | 03 §8.1 "All policy consumers" | nothing in `deployments/` or in any Go code subscribes to `zoiko.policy.events` | ❓ | The events go nowhere. Is that intended at this stage? |
| Consumed events: entity.created, role.updated, authority.delegated | 03 §8.1 | no Kafka reader (main.go builds only a Writer, 117-133) | ❌ | `KAFKA_GROUP_ID` is configured but never used. |

## F. Data model

| Item | Documented | Implemented | Status | Notes |
|---|---|---|---|---|
| Policy fields | 04 §7.1: policy_id, tenant_id, policy_code, policy_name, policy_domain, policy_status, versioning_mode | 000001:28-46 has no tenant_id, policy_status or versioning_mode; `policy_type` stands in for `policy_domain` | ⚠️ | A tenant-owned policy family cannot be expressed; every Policy row is platform-wide. |
| PolicyVersion fields | 04 §7.1: version_number, policy_payload, activation_status, activated_by/at | no version_number; the other fields exist under different names (rule_payload, version_status) | ⚠️ | |
| Source, rationale, artifact digest, known_from | GCP §18; V-001 §7, §8.1 | none | ❌ | |
| Policy types owned: signatory matrices, SoD rule sets, spend control | 03 §8.1 Owns | stored as free-form `rule_payload`; only APPROVAL_THRESHOLD can be evaluated | ⚠️ | |

## G. NFR and ops

| Item | Documented | Implemented | Status | Notes |
|---|---|---|---|---|
| Tier-0 timeouts, readiness, telemetry | 03 §06; §3.8 | main.go:66-80, 172-185, 190-197; readyz probes the DB (compose:963) | ✅ | |
| The evidence call must not block evaluation | 03 §3.9 | 2 s timeout (decisionlog/client.go:82) | ✅ | Required by §8.1, but it is also what makes the evidence failure silent (C6). |
| Postman collection | test assets | no policy-svc collection under `postman/` | ❓ | Not a documented obligation. |

## What the service does do well

- Mutations are authorized per scope and fail closed, with separate `_GLOBAL` actions for
  platform-wide versions.
- Version history is correctly tenant-scoped under forced RLS with NULLIF.
- Supersede is atomic, with a partial unique index guaranteeing one ACTIVE version per scope.
- Released payloads cannot be changed through the API.
- Threshold comparison uses exact decimals.
- Creates are idempotent.
- Events fire only on real transitions.

## Compliance

**15 of 55 scored items fully met — 27%** (partials at half: **40.9%**). 25 are ❌ and 2 are ❓.
**Against Doc 03 §8.1 alone: 50%.**

**Top gaps by risk**

1. **Evaluate records a caller-chosen actor into the governance ledger, with no authz**
   (security). handler.go:803, 853, 988; decisionlog/client.go:143.
2. **No maker-checker on activation** (security). The author can activate their own threshold
   (handler.go:542-550).
3. **Control-test and attestation reads are unauthenticated and cross-tenant** (security).
   control_test_handler.go:124-134; 000004.
4. **The envelope policy forces `X-Legal-Entity-Id` and `Idempotency-Key` onto global and
   evaluate calls** (security/contract). Callers invent values, and the header LE is never
   checked against the body LE (envelope/contract.go:14; main.go:180).
5. **Every decision-log write is refused and swallowed** (data integrity). There is zero
   durable evaluation evidence (decisionlog/client.go:133-153; handler.go:994-1004).
6. **`effective_from` and `effective_to` never gate** (data integrity). Future-dated and
   expired versions both decide (pg_store.go:620-627).
7. **Latest-by-timestamp tie-break across same-type policies**, which GOV-05 forbids (data
   integrity). pg_store.go:624-627; handler.go:900.
8. **No outbox; the topic has no consumer; no cache invalidation** (data integrity).
   handler.go:297, 449, 578.
9. **No as-of or historical resolution** (data integrity). pg_store.go:606.
10. **A malformed UUID returns 503; activate with no body returns 400** despite the documented
    optional body (contract). pg_store.go:160-167; handler.go:503, 1067.
11. **Doc drift** (cosmetic): routes differ from GOV-05 and V-001; error codes are not the §16
    classes; Policy lacks tenant_id, policy_status and version_number; the control-test routes
    are undocumented; progress.md's "decision log verified live" is stale.

**Needs clarification**

1. **Q1.** Is V-001 (PDC-01…05) the target contract for policy-svc, or for a future PDC suite?
   This decides whether the as-of, compare/explain, lifecycle-command, temporal-range, conflict,
   overlap, state-model, expected_version and evidence-content rows are defects or roadmap (C1).
2. **Q2.** Event names: Doc 03 `policy.*`, GCP §17 `PolicyActivated`, or V-001
   `pdc.release.activated` (C3)?
3. **Q3.** Should Evaluate get a `MaterialWrite` override, as authorization-svc has for
   `/v1/authorize`, so that `Idempotency-Key` and `X-Legal-Entity-Id` are not demanded (C5)?
4. **Q4.** Doc 03 §3.9/§8.1 says evidence failure must not block evaluation, but V-001 §14.1
   step 7 forbids success without evidence correlation. Which wins for APPROVAL_THRESHOLD (C6)?
5. **Q5.** Doc 04 puts `tenant_id` on Policy, but the code (000005 comment) treats Policy as
   platform-wide. Which is intended?
6. **Q6.** Is it intended that nothing consumes `zoiko.policy.events` yet?

---

# 4/7 — obligations-svc (:8088) vs Doc 03 §8.5 and Doc 04 §13 (R-001 WFC-05 recorded, unscored)

**Contract extracted from:**
- `03-microservices.md` §8.5 Obligations Service (L702-735), the primary per-service spec, plus:
  - doctrine §3.3 / 3.4 / 3.7 / 3.8 / 3.10 (L105-183)
  - §5.1, §06 Tier-0, the §07 template
  - §19 event contract rules (L1725-1750)
  - §12.3 Obligation Tracking (L1310-1322), read only to rule out overlap
- `04-data-model.md` §13 Compliance & Obligations Model (L1916-2032) and §18.3.
- R-001 §1, §2 (R-INV-03/04/14-19/22/23), §7 WFC-05 (L455-519), §9, §10, §11, and NP-12/37/38.
- Governance Control Plane GOV-03 (L87, 292-323, 1277). There, "obligations" means
  **policy-decision obligations attached to a Permit/Deny result**, in the XACML sense. That is
  a different concept from obligations-svc's records.
- V-001 (L88, 190-191, 224, 855, 1404), which gives WFC ownership of obligation clocks.
- API standard (status codes, idempotency, If-Match, RFC 9457, pagination) and the Event
  Catalogue (naming, outbox).
- `known-gaps.md` L225-235, 843-918, 1476-1490; tracker rows 65a, 78 and 82i; doc7 GOV-01.

**Code:** `services/obligations-svc/`. Files read:
- `cmd/server/main.go`
- `internal/handler/{handler.go,applicability_handler.go}`
- `internal/store/{pg_store.go,applicability_store.go}`
- `internal/events/publisher.go`
- `internal/authz/client.go`, `internal/jurisdiction/client.go`, `internal/middleware/tenant.go`
- `internal/envelope/*`, `internal/config`, `internal/health`, `internal/telemetry`
- migrations 000001-000004 and `pg_store_test.go`
- `deployments/docker-compose.yml` L1067-1112, `traefik-dynamic/all-services.yml`,
  `scripts/create-app-roles.sh` L55 and L92, `kubernetes/manifests/15-app-obligations.yaml`
- There is no Postman collection for this service. `ZoikoSuite_Phase5_LegalTaxCompliance`
  targets :8121, which is obligation-tracking-svc.

**Framing.** Section A is scored against Doc 03 §8.5, Doc 04 §13, the Doc 03 doctrine and §19,
and the API and event standards. R-001's WFC-05 "Obligation, Deadline & Escalation Service" is
much larger: clock classes, computed deadlines, durable overdue timers, and evidence gates on
FULFILLED/WAIVED. Whether obligations-svc *is* WFC-05 is ❓ (C1), so those 16 rows appear in
Section B, unscored. **If WFC-05 is binding, the combined score falls from 65.4% to 54.3%.**

## Implemented surface

| Method + path | Handler | Auth | Side effects |
|---|---|---|---|
| POST /v1/obligations | handler.go:274 | principal + tenant; authz `OBLIGATION_CREATE` on the body's legal entity | INSERT, dedup on (tenant, obligation_code); jurisdiction validated fail-closed; `obligation.created` after commit |
| GET /v1/obligations | handler.go:427 | principal + tenant; no authz | filters; offset paging (100 default, 500 max) |
| GET /v1/obligations/{id} | handler.go:391 | principal + tenant; no authz | — |
| POST /v1/obligations/{id}/status | handler.go:512 | principal + tenant; authz `OBLIGATION_STATUS_UPDATE` | row-locked transition; `obligation.updated`, plus `.overdue` or `.closed`, after commit |
| POST /v1/obligations/{id}/filing-requirements | handler.go:622 | principal + tenant; authz `FILING_REQUIREMENT_CREATE` | INSERT (status PENDING); no event |
| GET /v1/obligations/{id}/filing-requirements | handler.go:687 | principal + tenant | unbounded list |
| POST /v1/obligations/{id}/applicability-decisions | applicability_handler.go:104 | tenant + principal; authz `APPLICABILITY_DECISION_RECORD` | append-only INSERT (straight to the pool); no event |
| GET /v1/obligations/{id}/applicability-decisions | applicability_handler.go:210 | **tenant only** | unbounded list |
| GET /v1/obligations/{id}/applicability | applicability_handler.go:251 | **tenant only** | UNASSESSED when no decision row exists |
| GET /healthz, /readyz, /metrics | main.go:162-164 | none | readyz pings the DB |

**Middleware** (`main.go:140-154`): RequestID, RealIP, Recoverer, otelchi, metrics,
correlation-ID, TenantContext, Logger, then the §4 envelope (write-strict by default). There is
no Kafka consumer and no background job. The topic is `zoiko.obligations.events`
(`config.go:92`).

## Section A — Ownership and binding (Doc 03 §8.5)

| Item | Documented | Implemented | Status | Notes |
|---|---|---|---|---|
| Owns obligation **definitions** | 03 §8.5 Owns | 000001:24 (a single `obligations` table) | ⚠️ | One flat record, with no split between definition and instance and no versioned definition. |
| Owns obligation state | 03 §8.5 | pg_store.go:399-403, 409-482 | ✅ | OPEN → IN_PROGRESS / OVERDUE / CLOSED; CLOSED is terminal. |
| Owns due dates | 03 §8.5; 04 §13.1 | 000001:51 `due_date TIMESTAMPTZ NOT NULL`; handler.go:236 | ✅ | Supplied by the caller; nothing computes it (see B2). |
| Obligation-to-entity **mappings** | 03 §8.5 | 000001:28 (one `legal_entity_id`) | ⚠️ | No mapping table; an obligation binds to exactly one entity. |
| Obligation-to-workflow mappings | 03 §8.5 | none | ❌ | No column, table or API links an obligation to a workflow instance. |
| Source-origin refs / atomic linking | 03 §8.5 Critical Enhancement; 04 §13.3 | handler.go:228-246 (required); 000001:35-61 NOT NULL | ✅ | `obligation_source_type/id` and `source_reference`. No source *version* is stored, which 04 L1762 requires for contract obligations. |
| Entity-bound | 03 §8.5 Critical Constraint; 04 §13.3 | handler.go:224; 000001:28 NOT NULL | ✅ | |
| Jurisdiction-bound and validated | 03 §8.5 | handler.go:315-331; jurisdiction/client.go:61-90 | ✅ | Fails closed (503 when unreachable). Does not check that the jurisdiction is active. |
| **Legal entity exists and belongs to the caller's tenant** | 03 §8.5 entity-bound; known-gaps.md:879-894 (OPEN) | handler.go:311 (authz only) | ❌ | A principal with a grant on entity X can write X-scoped obligations into **any tenant** just by changing `X-Tenant-Id`. |
| Single owner of obligation truth | 03 §3.3; 04 §2.1; tracker row 78 (Not Started) | separate `obligations` tables in obligations-svc, obligation-tracking-svc and filing-tracker-svc; obligation-tracking-svc handler.go:121 and 220 emit **`obligation.created` / `obligation.updated`** | ❌ | §12.3 specifies `contract.obligation.*`. Two producers share one event type on different topics, which invites consumer confusion (C8). |

## Section A — Events

| Item | Documented | Implemented | Status | Notes |
|---|---|---|---|---|
| Publishes `obligation.created` | 03 §8.5 | handler.go:363-372; publisher.go:85-98 | ✅ | First insert only; a replay does not re-emit. |
| Publishes `obligation.updated` | 03 §8.5 | handler.go:555-561; publisher.go:103-109 | ⚠️ | Fires only on status transitions. There is no attribute-update API, so "updated" never covers data changes. |
| Publishes `obligation.overdue` | 03 §8.5 | handler.go:563-570; publisher.go:113-118 | ✅ | Emitted on an actual transition to OVERDUE. |
| **Overdue detection** (the due date passing) | 03 §8.5 (owns due dates and publishes overdue) | none; progress.md says "No built-in scheduler" | ⚠️ | A past-due obligation stays OPEN **forever** unless an external caller POSTs OVERDUE, and no such caller exists in the estate. See B7 for the stricter WFC-05 rule. |
| Publishes `obligation.closed` | 03 §8.5 | handler.go:571-578; pg_store.go:462 stamps closed_at | ✅ | |
| Event envelope minimum fields | 03 §19 (L1727-1747) | publisher.go:27-51, 137-157 | ✅ | name, version, timestamp, tenant (from ctx), entity, jurisdiction, actor, correlation, source, schema version. |
| Event type and channel naming | Event Catalogue §5.1-5.2 | publisher.go:86 etc.; config.go:92 `zoiko.obligations.events` | ⚠️ | Estate-wide cosmetic drift; the wire names match 03 §8.5 (C3). |
| Partition key = hash(tenant, aggregate) | Event Catalogue §5.3 | publisher.go:163, key = obligation_id | ⚠️ | Per-aggregate ordering still holds. Minor. |
| Transactional outbox | Event Catalogue §6 (L389-392); R-001 R-INV-23 | handler.go:348-372, 548-580 (published after commit, straight to the broker) | ❌ | A Kafka failure, or the request context being cancelled after commit, loses the event permanently, leaving only a log line. Group-wide X3. |
| Alertable failure states | 03 §3.8 | telemetry.go:64-76 (HTTP and readiness metrics only) | ⚠️ | No publish-failure counter, so event loss cannot be alerted on. |
| Consumed events | 03 §07 template; §8.5 lists none | none | ✅ | No consumer, so no ErrorLogger-stall or single-topic risk. |
| No `go Publish(ctx)` pattern | platform pattern | handler.go:365, 555 (synchronous) | ✅ | Synchronous on the request context; the residual risk is the outbox row. |

## Section A — Idempotency

| Item | Documented | Implemented | Status | Notes |
|---|---|---|---|---|
| Idempotent obligation create | 03 §3.7; API standard L381 | pg_store.go:225-298 | ⚠️ | Dedup is on (tenant, code), but the conflict check compares only entity, jurisdiction, type and due_date (L287-290). A replay with a different severity, source_reference or responsible_function silently returns 200 with the *old* record. |
| Idempotent filing-requirement create | 03 §3.7 (names "filing submission") | pg_store.go:514-553 | ❌ | No dedup: a retry creates a duplicate. A reused caller-supplied id hits PK 23505 and gets 503. |
| Idempotent applicability decision | 03 §3.7 | applicability_store.go:67-99 | ❌ | Same as above; duplicate decisions are appended. |
| Idempotent status update | 03 §3.7 | pg_store.go:440-445 | ✅ | A same-status request gets 200 and emits no event. |
| `Idempotency-Key` semantics (same key replays; changed body → 409) | API standard L375-385; envelope INV-08 | envelope/policy.go:220-227 checks presence only; the value is read nowhere | ❌ | Required on writes, then ignored. Group-wide X4. |
| Caller-supplied obligation_id collision | API standard 409 | pg_store.go:212-234 (the ON CONFLICT target is (tenant, code) only) | ⚠️ | A PK clash hits 23505 and gets 503. The 503-versus-201 difference reveals whether that id exists in another tenant. |

## Section A — Tenant, auth and envelope

| Item | Documented | Implemented | Status | Notes |
|---|---|---|---|---|
| Tenant isolation: obligations and filings | 04 tenant rule; R-INV-22 | pg_store.go:46-82 (explicit predicate plus `set_config`); 000003:63-76 FORCE RLS; 000004:32-42 NULLIF | ✅ | The app role is NOSUPERUSER, NOBYPASSRLS and not the owner (create-app-roles.sh:55, 92), so RLS is live. The empty-GUC trap is fixed. |
| Tenant isolation: applicability_decisions | R-INV-22; known-gaps.md:896-918 (OPEN) | applicability_store.go:68, 105, 139 (tenant-scoped parent lookup), then straight to the pool at L87, 115, 152; 000004:26-28 | ⚠️ | The cross-tenant read leak in known-gaps is now closed at the app level by the parent pre-check, so that entry is partly stale. There is still no tenant_id column and no RLS. |
| Missing tenant → 401, not 500/503 | platform pattern | handler.go:82-89, 165-166; applicability_handler.go:221, 258 | ✅ | |
| Malformed id → 404/400, not 503 | API standard | pg_store.go:30-36, 198-202, 434-435 | ✅ | |
| Principal required on obligation routes | API standard 401 | handler.go:277, 394, 430, 516, 626, 690 | ✅ | |
| **Principal required on applicability reads** | API standard 401; R-001 §11.1 | applicability_handler.go:210-243, 251-277 (no `requirePrincipal`); the envelope admits GETs (middleware.go:102-108) | ❌ | `facts_used` and the decider's identity can be read without authentication, given only a tenant header. |
| Identity headers are verified | 05-security; gateway doctrine | tenant.go:37-46 and handler.go:69-76 trust the raw headers; the local Traefik route has no auth (all-services.yml:12-16, 344-351) | ⚠️ | Production relies on GTRM ForwardAuth; direct access to port 8088 (compose:1077-1078) bypasses it. Group-wide X5. |
| Write authorization: per action, fail-closed | 03 §3.2, §3.10; known-gaps:843-859 | handler.go:92-105; authz/client.go:48-53, 84-135 | ✅ | Four distinct actions; GRANTED and DENIED are parsed correctly; any other answer gets 503. |
| Read authorization (object-level entity scope) | R-001 §11.1 | handler.go:391-478, 687-707 (no authz) | ⚠️ | Any principal in the tenant can read every entity's obligations. |
| Attribution from the verified principal | known-gaps:872-874 | handler.go:296-307, 345; applicability_handler.go:130-149 | ✅ | |
| Authz call carries the caller's envelope | tracker row 82i | authz/client.go:94-101 (Content-Type and correlation only) | ⚠️ | Admitted since the 82i fix, but decisions are logged with a NULL tenant: unattributed, and invisible in `GET /v1/access-decisions` (X6). |
| `X-Legal-Entity-Id` reconciled with the body's `legal_entity_id` | canonical input contract §4 (INV-02); contract.go:17 | none | ❌ | The header is required but never compared, and authz uses the body value. |
| Canonical input envelope enforced | ZS-ARCH-SVC-001 §4 | main.go:154; envelope/middleware.go:63-100 | ✅ | Write-strict by default. |
| Envelope tenant and entity resolution | envelope/resolver.go (vendored) | `NewResolver` (resolver.go:99) is never called | ⚠️ | Dead code: tenant and entity are never resolved against the registry. |

## Section A — State, data model and applicability

| Item | Documented | Implemented | Status | Notes |
|---|---|---|---|---|
| Status state machine is concurrency-safe | known-gaps:875-876 | pg_store.go:422-477 (`FOR UPDATE`, one transaction) | ✅ | |
| Invalid status value → 400 | the handler's own contract (handler.go:508); API standard 400/422 | pg_store.go:447-456 → `ErrInvalidTransition` → 409 (handler.go:171-172) | ⚠️ | `"FOO"` gets 409 invalid_transition. |
| Status and timestamp invariants in the DB | 04 §13.3 | 000003:96-108 CHECKs (NOT VALID) | ✅ | |
| Obligation fields per 04 §13.1 | 04 §13.1 | 000001:24-70; domain/types.go:29-77 | ✅ | All 12 fields plus audit columns. |
| FilingRequirement fields per 04 §13.1 | 04 §13.1 | 000001:83-95 | ✅ | |
| filing_status lifecycle | 04 §13.1 (field listed) | pg_store.go:530 hard-codes PENDING; there is no update route | ⚠️ | The status can never change. |
| ComplianceStatus / ExceptionCase / EscalationRecord | 04 §13.1-13.2 | not built (progress.md: "Deliberate v1 scope") | ❓ | §8.5 does not assign their ownership. |
| Applicability is versioned; UNASSESSED ≠ NOT_APPLICABLE | doc7 §E2; checklist GOV-01; V-001 PDC-I-12 | 000002:28-80; applicability_store.go:154-161 | ✅ | |
| The applicability decision value is validated | 000002:21-42 comment; doc7 §E2 | applicability_handler.go:68-91 checks non-empty only; no CHECK constraint | ⚠️ | Any string is stored, including `UNASSESSED`, which the design says must never be stored. |
| Each decision is attributed to exactly one of human or system | 000002:60-61 "exactly one" | the CHECK at L72-74 is at-least-one; the handler allows both | ⚠️ | |

## Section A — API standard, NFRs and artefacts

| Item | Documented | Implemented | Status | Notes |
|---|---|---|---|---|
| Error body is RFC 9457 problem+json | API standard L260, 471-489 | handler.go:709-716 `{"error":...}` | ⚠️ | Estate-wide drift (X9). |
| 201 includes a Location header | API standard L443 | handler.go:379, 674; applicability_handler.go:202 | ⚠️ | No Location header. |
| Pagination (cursor; default 50, max 200) | API standard L556-570 | handler.go:131-160 uses offset with 100/500; pg_store.go:561 and applicability_store.go:109 are unbounded | ⚠️ | |
| If-Match optimistic concurrency on the mutable aggregate | API standard L69, 359 | none; no row_version column | ⚠️ | The row lock prevents lost updates but not stale-intent writes. |
| Observability baseline | 03 §3.8 | main.go:53-79, 140-164; health.go:27-51 | ✅ | zap, OTel, /healthz, /readyz, /metrics, correlation ID. |
| Failure mode declared | 03 §3.10 | jurisdiction/client.go:61-90; authz/client.go:84-135 | ✅ | Fail-closed on both dependencies. |
| Request hardening | code only, undocumented | handler.go:107-129; main.go:174-181 | ✅ | 256 KiB cap, unknown fields rejected, server timeouts. |
| Tier-0 deployable | 03 §06 | compose:1067-1112; k8s 15-app-obligations.yaml | ✅ | |
| progress.md is accurate | claims | progress.md:40-56, 70-75 | ⚠️ | **Stale.** It says there is no authz wiring (there is: authz/client.go) and no compose entry (there is: compose:1070), and it omits tenant scoping, applicability and the envelope. |
| Gateway route | gen-gateway-routes | all-services.yml:344-351, 713-716, 978-981 | ✅ | Local only, no auth. |
| Postman collection | none documented | none in `postman/` | ⚠️ | |
| Store tests are safe | known pattern: store tests wipe their DB | pg_store_test.go:22-24 (silent skip), L44 `DROP TABLE ... CASCADE` | ⚠️ | Destroys whatever database TEST_DATABASE_URL points at. |
| Outbound URL built from body input | security hygiene | jurisdiction/client.go:62 uses `Sprintf` without escaping | ⚠️ | Bounded, because the UUID column rejects non-UUIDs later. |
| Compose declares runtime dependencies | compose convention | compose:1096-1102 (postgres, kafka and authz only) | ⚠️ | jurisdiction-svc is missing, so every create gets 503 until it is up. |

## Section B — R-001 WFC-05 (Obligation, Deadline & Escalation) — ❓ unscored

These rows are ❓ because it is not settled whether obligations-svc is the WFC-05 implementation
(C1). The *If binding* column shows the status each row would get.

| Item | Documented | Implemented | Status | If binding |
|---|---|---|---|---|
| B1 Clock class HARD_LEGAL / REGULATORY / CONTRACTUAL / INTERNAL_SLA / ADVISORY (R-INV-15) | R-001 §7.1 L466; §2 L164 | none (`severity_level` is free text) | ❓ | ❌ |
| B2 Deadline computed with rule/calendar version, timezone and business-day provenance | R-INV-16; §7.1 L470; §7.3 | due_date is supplied by the caller (handler.go:215) | ❓ | ❌ |
| B3 Recalculation supersedes; history can be reconstructed | R-INV-17; §7.1 Supersession | none | ❓ | ❌ |
| B4 Lifecycle PLANNED / ACTIVE / DUE_SOON / DUE / OVERDUE / FULFILLED / WAIVED / NOT_APPLICABLE / SUPERSEDED | §7.2 L482-512 | OPEN / IN_PROGRESS / OVERDUE / CLOSED (pg_store.go:399-403) | ❓ | ❌ |
| B5 FULFILLED requires an evidence ref or an approved exemption reason | R-INV-14 | CLOSED needs only a status string (handler.go:482-484) | ❓ | ❌ |
| B6 WAIVED / NOT_APPLICABLE need authority plus evidence | §7.1 L476; §7.2 | applicability decisions exist (source rule, facts, actor) but are not linked to obligation state | ❓ | ⚠️ |
| B7 OVERDUE is computed; scheduler delay cannot suppress it; durable timers | §7.2 L497; "Scheduler is source of truth" L55-57; NP-37 | none (progress.md) | ❓ | ❌ |
| B8 Warning and escalation schedule; DUE_SOON; escalation ≠ fulfilment | §7.1 L472; §7.4; R-INV-19 | none | ❓ | ❌ |
| B9 `POST /v1/obligations/evaluate`, `POST /v1/obligations/{id}/satisfy` | §10.1 L644-649 | not routed (handler.go:183-192) | ❓ | ❌ |
| B10 Events obligation_created, deadline_clock_started, obligation_due_soon, obligation_due, obligation_overdue, obligation_fulfilled, obligation_waived | §10.2 L667-668 | obligation.created / updated / overdue / closed only | ❓ | ❌ |
| B11 Trigger evidence (event_id + event_time + source version) | §7.1 L468; NP-12 | source_reference text only | ❓ | ❌ |
| B12 Business state and outbox written atomically | R-INV-23 | see the Section A outbox row | ❓ | ❌ |
| B13 Common columns: row_version, correlation_id, plane, data_class, residency_region, updated_by | §9.1 L590 | none of these (000001) | ❓ | ❌ |
| B14 Envelope minimum: plane, causation_id, subject version, payload hash, idempotency ref | §10.3 L670 | partial (publisher.go:27-51) | ❓ | ⚠️ |
| B15 Satisfied-after-overdue keeps the overdue period | NP-38 | the status is overwritten and there is no history table; only closed_at is kept | ❓ | ⚠️ |
| B16 Overdue-obligation metric by clock class, jurisdiction and owner | §12 L734 | none | ❓ | ❌ |

If WFC-05 is binding, the combined score is (29 + 0.5 × 30) / 81 = **54.3%** (✅ 29, ⚠️ 30,
❌ 22).

## What the service does do well

- Tenant isolation on obligations and filings is solid: FORCE RLS with the NULLIF fix, and a
  non-owner NOBYPASSRLS role.
- Write authorization is per action and fails closed; jurisdiction validation fails closed too.
- The §19 event envelope is complete.
- Status transitions are concurrency-safe under a row lock.
- An empty tenant gets 401 and a malformed id gets 404.
- Obligations are always entity- and jurisdiction-bound, with a mandatory source origin.
- There is no `go Publish(ctx)` and no consumer, so neither the stall nor the single-topic risk
  exists.

## Compliance

**29 of 65 scored items fully met — 45%** (partials at half: **65.4%**). 9 are ❌ and 1 is ❓.
The 16 WFC-05 rows are unscored; if they are binding, the weighted score is **54.3%**.

**Top gaps by risk**

1. **Applicability reads are unauthenticated** (security). There is no `requirePrincipal`, and
   write-strict admits GETs (applicability_handler.go:210-277).
2. **The legal entity is never reconciled with the tenant** (security, integrity). Neither the
   registry nor `X-Legal-Entity-Id` is checked, so cross-tenant writes are possible
   (handler.go:311; contract.go:17; known-gaps:879 still open).
3. **Identity headers are trusted raw while the port is exposed directly** (security).
   tenant.go:37-46; handler.go:69-76; compose:1077-1078.
4. **Authz calls carry no tenant envelope**, so decisions are logged unattributed (security,
   audit). authz/client.go:94-101; tracker 82i.
5. **No object-level (entity) read authorization** (security). handler.go:391-478.
6. **No transactional outbox and no publish-failure metric** (data integrity).
   handler.go:363-372, 554-580; telemetry.go:64-76.
7. **Overdue is never computed** (data integrity). Past-due obligations stay OPEN
   indefinitely, and `obligation.overdue` depends on a caller that does not exist
   (pg_store.go:399-403).
8. **Idempotency** (data integrity). `Idempotency-Key` is ignored (envelope/policy.go:224);
   filings and applicability decisions have no dedup (pg_store.go:514;
   applicability_store.go:67); the obligation dedup compares only 4 attributes
   (pg_store.go:287-290).
9. **Three services own "obligations"**, two of them with the same event names (data
   integrity). Tracker row 78; obligation-tracking-svc handler.go:121, 220.
10. **applicability_decisions has no tenant_id or RLS; the decision value and the "exactly
    one" actor rule are not enforced** (data integrity). 000002:42, 72-74;
    applicability_handler.go:68-91.
11. **Lower-risk drift:**
    - no obligation-to-workflow mapping
    - filing_status stuck at PENDING
    - an invalid status gets 409 instead of 400
    - pagination, error format and the Location header differ from the API standard
    - the envelope resolver is dead code
    - pg_store_test.go:44 drops tables on its target DB
    - progress.md and part of known-gaps are stale
    - compose is missing the jurisdiction-svc depends_on

**Needs clarification**

1. **Is obligations-svc the WFC-05 "Obligation, Deadline & Escalation Service" of R-001?**
   ownership.txt lists obligations-svc and workflow-svc separately. If it is, all of Section B
   is missing (C1).
2. **GOV-03 "obligations"** (conditions attached to a Permit/Deny result) are judged a
   *different concept*. It is unconfirmed whether the "feature/jurisdiction obligations" at
   L305 are ever meant to come from obligations-svc.
3. **ComplianceStatus / ExceptionCase / EscalationRecord** (04 §13.1): which service owns them?
4. **Applicability vocabulary:** doc7 §E2 uses APPLICABLE / NOT_APPLICABLE / UNCERTAIN /
   UNASSESSED; V-001 PDC-02 uses RESOLVED / AMBIGUOUS / INSUFFICIENT / CONFLICTED /
   NOT_APPLICABLE. Should obligations-svc record decisions itself or consume PDC-02 results?
5. **Scope of `obligation.updated`:** status changes only, or attribute edits too? The latter
   needs an update API that does not exist.
6. **Filing calendar and due-date ownership:** jurisdiction-rules-svc or obligations-svc
   (known-gaps:225-235; C8)?

---

# 5/7 — authorization-svc (:8089) vs Doc 03 §8.3, GOV-03 (+ GOV-04 / GOV-12) and ZS-IAM-001

**Contract extracted from:**
- `03-microservices.md` §8.3 Authorization Service (L570-637), the primary per-service spec,
  plus §3.7 idempotency (L129-147), §3.9 governance latency (L165-167), §06 Tier-0 (L362),
  §9.3 Delegated Authority (L823-837) and §9.4 Access Control (L839-850).
- Governance Control Plane (ZS-SVC-A-001):
  - §2 invariants #1, #2, #3 (fail-closed), #4 (immutable versions), #6, #9 and #10 (outbox)
  - §3 authority matrix
  - **GOV-03 Authorization — RBAC/ABAC** (L291-369), GOV-04 SoD (L370-460) and GOV-12
    Maker-Checker & Break-Glass (L1103-1195)
  - §16 envelopes and error classes, §17 events, and §18 data model, including the
    AuthorizationDecision record (L1277)
- ZS-IAM-001 (Global Authorization / RBAC / ABAC / SoD Standard):
  - §0, §7 decision pipeline, §8.1/8.2 canonical request and decision
  - §9 roles and assignments, §10 SoD, §11 delegation
  - §19 caching, §20 evidence, §21 APIs, §23 events, §32 anti-patterns
- `known-gaps.md` and `backend-completion-tracker.md` (rows 5, 67, 79, 81, 82a-82j, and the
  L333 note).

**Code:** `services/authorization-svc/`. Files read:
- `cmd/server/main.go`
- `internal/handler/{handler.go,validation.go,access_decisions.go,envelope_policy.go}`
- `internal/envelope/*`, `internal/store/pg_store.go`, `internal/cache/store.go`
- `internal/events/{publisher.go,consumer.go,lifecycle_consumer.go}`, `internal/config/config.go`
- migrations 000001-000013
- `deployments/docker-compose.yml` (L1114-), `traefik-dynamic/all-services.yml`
- `postman/postman/ZoikoSuite_Authorization.postman_collection.json`
- `progress.md`, read as claims

No tests were run: the store tests wipe their target DB.

**Framing.** This is the largest service in the group (~11k lines of non-test Go), and most of
its decision engine is sound:
- deny-by-default
- fail-closed on every store error
- the decision recorded before the response
- SoD and ABAC both deny-only

The critical findings are in the **administration surface**: the routes that create roles,
bundles, assignments and rules. The weighted score of 57.4% **understates the risk**, because
two ❌ items together let any tenant principal take over authorization for their own tenant and
deny access across the platform.

> **Verification of the two escalation paths (28 Sep 2026).** The service auditor found both,
> and they were then re-checked independently in the code:
> - `CreateRole` (handler.go:374) and `CreateRoleAssignment` (handler.go:773) call only
>   `requirePrincipal` and `requireTenant` before writing. No permission is evaluated.
> - Migration `000003_nullable_legal_entity_for_tenant_scope` made
>   `principal_role_assignments.legal_entity_id` nullable.
> - `requirePlatformAction` (handler.go:272-300) calls
>   `FindGrantedActions(ctx, principal, platformScopeEntityID, "")` with an empty tenant.
> - The query (pg_store.go:1414-1418) matches
>   `(pra.legal_entity_id = $2 OR pra.legal_entity_id IS NULL) AND ($3 = '' OR r.tenant_id::text = $3)`,
>   so any tenant-wide assignment satisfies the platform check.
>
> Confirmed statically, not executed.

## Doc 03 §8.3 inbound APIs and GOV-03 queries/commands

| Item | Documented | Implemented | Status | Notes |
|---|---|---|---|---|
| Evaluate action authorization | §8.3 Inbound APIs; GOV-03 EvaluateAuthorization | `POST /v1/authorize` handler.go:157, 1705 | ✅ | The path differs from GOV-03's `/internal/v1/gov03/evaluateAuthorization`, which that spec labels illustrative (C4). |
| Validate entity scope | §8.3 | `POST /v1/entity-scope/validate` validation.go:78, 153 | ✅ | Batches of up to 100 entities (validation.go:141). |
| Validate SoD conflicts | §8.3 | `POST /v1/sod/validate` validation.go:79, 357 | ✅ | Advisory only. Nothing on the write path calls it (see Assignment-time SoD below). |
| Evaluate delegated access | §8.3 | `POST /v1/delegated-access/evaluate` validation.go:80, 543 | ✅ | |
| Retrieve authorization rationale | §8.3 | `GET /v1/access-decisions` and `GET /v1/access-decisions/{id}` (access_decisions.go:17, 73; handler.go:170-171, 2189) | ✅ | |
| Explanation access restricted to prevent policy leakage | GOV-03 Authorization/permissions (L321) | access_decisions.go:76-79; validation.go:122-127, 156-183 | ⚠️ | Any principal with the tenant header can list every decision in the tenant, including the SoD rule behind each denial, and can enumerate any other principal's `permitted_actions`. Only the principal and tenant headers are checked; there is no read permission. |
| ListAvailableActions | GOV-03 Queries (L313); ZS-IAM-001 §21 `available-actions` | partly: entity-scope/validate with no `action_type` returns PermittedActions (validation.go:122-127) | ⚠️ | Not a dedicated query. It ignores ABAC and SoD, so it can list actions that `/v1/authorize` would deny. That breaks GOV-03 negative path #4 ("AvailableActions equals backend-authorized actions"). |
| InvalidateAuthorizationCache command | GOV-03 Commands (L311) | none | ❌ | Invalidation happens only as a side effect of local writes and consumed events (cache/store.go:261, 579). |
| RecomputeSubjectEffectiveAccess | GOV-03 Commands (L311) | none | ❌ | |
| Admin API surface documented | §8.3 lists 5 inbound APIs only | 22 `/v1/admin/*` routes (handler.go:134-155) | ⚠️ | Code with no contract: request and response schemas, status codes and required permissions are all undocumented. |

## Decision contract

| Item | Documented | Implemented | Status | Notes |
|---|---|---|---|---|
| Decision carries obligations, reason_codes, policy_set_version, expires_at | GOV-03 purpose (L292); ZS-IAM-001 §8.2 (L338-340), §8 PDP row | `authorizeResponse` (handler.go:1585-1589) has only `decision_outcome`, `decision_basis` and `access_decision_id` | ❌ | There are no obligations anywhere in the service. |
| Outcomes PERMIT / DENY / STEP_UP / REQUIRE_APPROVAL | ZS-IAM-001 §7 stage 9 (L305) | only GRANTED and DENIED (access_decisions.go:22) | ❌ | GRANTED vs PERMIT is also a naming drift. |
| Stable error classes (AUTHORIZATION_DENIED, SOD_CONFLICT, IDEMPOTENCY_MISMATCH, CONCURRENCY_CONFLICT…) | §16 (L1220) | ad-hoc snake_case codes such as `authorization_denied`, `store_unavailable`, `tenant_scope_mismatch` (handler.go:188-330) | ❌ | SoD shows up only as a `sod:` basis prefix inside a 200 response. Group-wide X9. |
| Deny by default (no grant → DENY) | ZS-IAM-001 §7 (L312) | handler.go:1797-1799 | ✅ | |
| Fail closed when the store is unavailable | invariant #3 (L55); GOV-03 Failure (L331); ZS-IAM-001 §19 | handler.go:1754-1759, 1766-1777, 1783-1788, 1805-1809, 1834-1838 and 1858-1862 return 503 and never allow | ✅ | |
| No decision without an artifact (a failed record means refuse) | §8.3 Critical Constraint (L636) | handler.go:1900-1918; requirePlatformAction handler.go:307-322 | ✅ | The insert result is never cached (cache/store.go:14-31). |
| Authorization never overrides SoD; deny wins | invariant #6 (L61); GOV-03 SoD (L325) | SoD runs after the grant layers and can only deny (handler.go:1800-1815) | ✅ | |
| ABAC predicates; a role alone cannot bypass ABAC | §8.3 Owns; GOV-03 negative path #1 | handler.go:1856-1882; internal/abac/evaluator.go; migration 000010 | ✅ | Deny-only. A REQUIRE rule denies when the attribute is missing. The table ships empty. |
| Suspension as a negative control | ZS-IAM-001 §7 stage 6 (L297) | layer 0 at handler.go:1754-1764; pg_store.go:2140-2189 | ✅ | Works only if the consumer actually applied the event (see Consumers below). |

## Segregation of duties, self-grant and admin authorization

| Item | Documented | Implemented | Status | Notes |
|---|---|---|---|---|
| Static SoD at action time | GOV-04 EvaluateStaticConflict; ZS-IAM-001 §10.1 | CheckSoDConflict pg_store.go:1645 | ✅ | |
| Dynamic (own-object) SoD | GOV-04 EvaluateDynamicConflict; ZS-IAM-001 §10.2 | handler.go:1821-1840; pg_store.go:1706 | ⚠️ | The object's owner (`resource_owner_principal_id`) is optional and supplied by the caller, so a caller that omits it skips the check. Invariant #2 and ZS-IAM-001 §7 stage 2 say resource ownership is resolved by the server. workflow-svc never sends it. |
| Static SoD enforced when a role is assigned | ZS-IAM-001 §10 ("deny an assignment"); ZS-SVC-A-001 §19 negative path #5 | CreateRoleAssignment handler.go:773-840 and pg_store.go:622-667 run no SoD check | ❌ | A conflicting pair can be assigned and is caught only later, when the action is evaluated. |
| No self-grant of a protected privilege | ZS-IAM-001 §10.1 "Access Administrator / Own Privileged Grant Approver" (Critical); §10.2 "Own access elevation"; §32 | handler.go:773-840 never compares `req.PrincipalID` with the caller | ❌ | A caller can assign any role to themselves. |
| **Authorization on admin and grant endpoints** (who may manage roles, grants and rules) | ZS-IAM-001 §9 ("Assignment request … approval"), §21 (`POST /v1/iam/access-assignments` "governed workflow"), Appendix A (`iam.role.manage`, `iam.assignment.grant`, `iam.sod_rule.publish`) | tenant-level CreateRole handler.go:374; CreatePermissionBundle :528; CreateRoleAssignment :773; RevokeRoleAssignment :842; CreateSoDRule (tenant) :1105-1140; RetireSoDRule :1196; CreateABACRule (tenant) :1302-1356; retire/reactivate for role, bundle and ABAC | ❌ | **Critical.** Every tenant-level admin write checks only that `X-Principal-Id` and `X-Tenant-Id` are present and match the owning tenant; **no permission is evaluated**. Any principal can grant themselves any action, or retire the SoD rule that blocks them. Tracker L333 says the service "cannot call itself", but `requirePlatformAction` (handler.go:272) already shows it can check its own store. |
| **Tenant custom roles cannot hold protected platform-admin permissions** | ZS-IAM-001 §9 "Tenant custom role" (L352) | a bundle accepts any action strings (handler.go:528-560); an assignment accepts any `legal_entity_id` or none (handler.go:814-823); the platform check (handler.go:300) calls FindGrantedActions with tenant `''` (pg_store.go:1414-1418, 1480-1484) | ❌ | **Critical escalation, confirmed in the code:** (1) a tenant user creates a TENANT-scoped role whose bundle is `[SOD_RULE_MANAGE_GLOBAL]`; (2) assigns it to themselves with no `legal_entity_id`; (3) `pra.legal_entity_id IS NULL` then matches the platform-scope entity. The user can now author **platform-wide** SoD and ABAC rules, which amounts to a platform-wide denial of service. The comment at handler.go:293-295 ("NOT satisfied by any tenant-level role") is false. |
| Maker-checker on role and assignment changes | ZS-IAM-001 §9, §21; GOV-12 | none | ❌ | There is no assignment approval, either in this service or via workflow-svc, however GOV-12 ownership is eventually settled (C2). Group-wide X7. |
| purpose / reason_code on privileged or destructive commands | §16 (L1216) | the retire and revoke routes take no body | ❌ | |
| expected_version on protected updates | §16 (L1214) | none on retire/reactivate or bundle replace (handler.go:450, 706; pg_store.go:462) | ❌ | A bundle upsert overwrites `permitted_actions` wholesale. |
| Versioned, immutable policy artifacts (SoD and ABAC rules) | invariant #4 (L57); GOV-04 "Policy lifecycle versioned"; ZS-IAM-001 §8 PAP | `active_flag` is flipped in place (pg_store.go:1155, 1252); no versions | ❌ | Historical decisions cannot be reproduced against the rule version that was in force (invariant #11; X8). |
| Idempotency on state-changing APIs | §3.7 (L131); §16 idempotency_key; GOV-03/04 "Idempotency-Key" | the envelope requires the header on writes (envelope/middleware.go:70-99), but nothing stores or replays it. CreateRoleAssignment mints a new UUID on every call (pg_store.go:633-635); CreateSoDRule and CreateABACRule are plain inserts | ⚠️ | A retried call creates duplicate grants and rules. CreateRole is idempotent on (tenant, role_code). Group-wide X4. |
| Repeat evaluation has no conflicting side effects | §8.3 Idempotency (L623) | each call appends one log row and one event, by design (envelope_policy.go:53-60) | ✅ | |

## Tenant scope and caller identity

| Item | Documented | Implemented | Status | Notes |
|---|---|---|---|---|
| Tenant scoping of admin writes | invariants #1, #2; §16 tenant_id | refuseForeignTenant handler.go:247-257; role-owner check at handler.go:476-479, 575-583, 802-808 | ✅ | |
| A cross-tenant resource ID is denied without disclosure | GOV-03 negative path #3 | 404, not 403 (handler.go:465-479, 575-583) | ✅ | |
| **Trusted tenant resolved before authorization** | invariant #1 (L50) | tenantless `/v1/authorize` is admitted under write-strict (envelope_policy.go:88-99) and **evaluated across tenants** (handler.go:1699-1702; pg_store.go:1414-1418) | ❌ | With no tenant, a tenant-wide assignment in tenant A matches a legal entity in tenant B. Tracker row 82i and progress.md L1915 count 86 callers that send no envelope. |
| A client-supplied tenant is never authoritative | invariant #2 (L52); §16; ZS-IAM-001 §32 | falls back to the body's `tenant_id` (handler.go:1686-1697) | ⚠️ | The header wins when both are present and a mismatch is refused, but the fallback is still a client claim. |
| subject_id and tenant are authenticated and server-resolved | §16 (L1204-1207); ZS-IAM-001 §16 | `X-Principal-Id` and `X-Tenant-Id` are read raw (envelope.go:152-174; handler.go:184-221); port 8089 is published (compose:1122); the dev Traefik route has no auth (all-services.yml:12-16) | ⚠️ | Estate-wide pattern that depends on GTRM ForwardAuth in production; mTLS is off by default (main.go:199-229). X5. |
| Only internal PEPs call the evaluation endpoint | GOV-03 Authorization (L321); ZS-IAM-001 §21 "Internal only" | Authorize does not call requirePrincipal and does not check the workload (handler.go:1705-1730) | ⚠️ | Anyone who can reach the service can probe any principal's grants, and each probe writes a log row. |
| RLS on tenant tables uses NULLIF | Doc 04 §2.2; the RLS empty-GUC trap | 000004, 000005, 000006, 000007, 000008, 000009, 000010 and 000013 all use `NULLIF(current_setting(...), '')::uuid` | ✅ | The handler predicates are explicit too. |
| A malformed or empty tenant gets 400/401, not 500/503 | estate pattern | validTenantScope handler.go:230-245; resolvePlatformScope validation.go:633-680 | ✅ | |

## Caching

| Item | Documented | Implemented | Status | Notes |
|---|---|---|---|---|
| Short, risk-based cache TTL | ZS-IAM-001 §19; §3.9 | 5 s by default, 0 disables it (config.go:306; cache/store.go:33-46) | ✅ | |
| Cache key includes assignment and policy versions | ZS-IAM-001 §19 Key; §32 | generation counters per namespace and tenant (cache/store.go:193, 261) | ⚠️ | The key has no version component, but invalidation compensates within a replica. |
| Revocation invalidates cached decisions | GOV-03 negative path #2; ZS-IAM-001 §19 | local invalidation on revoke (cache/store.go:456, 472); across replicas it relies on access-control-svc events or the TTL (main.go:305-318) | ⚠️ | The service's own admin writes announce nothing, so other replicas stay stale for up to the TTL. |
| Tier-0 latency: local decision cache | §3.9 (L167); §8.3 Scaling (L627-634) | cache/store.go; SIEM is asynchronous (siem/client.go; tracker 82j) | ✅ | No numeric latency budget is documented to test against. |
| Horizontally scalable | §8.3 Scaling | stateless apart from the per-replica cache | ✅ | |

## Evidence and decision log

| Item | Documented | Implemented | Status | Notes |
|---|---|---|---|---|
| Every decision logged with actor, action, basis and outcome | §8.3 Evidence (L618) | RecordAccessDecision pg_store.go:1864-1896 | ✅ | |
| Denials are evidentially retrievable | §8.3 Evidence (L620) | ListAccessDecisions access_decisions.go:73 | ⚠️ | Tenantless decisions are invisible to every reader (access_decisions.go:54-58), and today that covers most callers (X6). |
| Decision record fields: resource, attributes digest, obligations, policy versions, session assurance, scope | GOV-03 Evidence (L333); §18 AuthorizationDecision (L1277); ZS-IAM-001 §20 | stored columns: principal, legal_entity, action, outcome, basis, correlation, tenant (pg_store.go:1866) | ❌ | Resource type/id, attributes digest, obligations, policy digest and assurance are all missing. |
| Delegated actions record on_behalf_of / delegation_id | ZS-IAM-001 §11 attribution rule (L511) | only the basis string `delegated:from=<id>` (validation.go:120) | ⚠️ | No structured field and no delegation_id. |
| Platform-scope admin acts produce a decision artifact | §8.3 Critical Constraint | handler.go:307-322 | ✅ | TenantID is empty on that record. |
| The decision log is immutable | ZS-IAM-001 §32 ("Deleting authorization history"); GOV-03 "Decision is immutable fact" | enforced by convention only (000001:14-15); no trigger and no REVOKE UPDATE/DELETE | ⚠️ | Retention detaches partitions rather than deleting rows (000009). |
| Retention and partitioning of the decision log | ZS-IAM-001 §20 retention | 000009, 000011; retention/runner.go; main.go:374-395 | ✅ | |

## Events and consumers

| Item | Documented | Implemented | Status | Notes |
|---|---|---|---|---|
| Publishes authorization.granted, authorization.denied, sod.violation.detected | §8.3 Published Events (L599-605) | publisher.go:62-97, topic `zoiko.authorization.events` | ✅ | Names match §8.3. GOV-03 uses PascalCase (AuthorizationDenied) (C3). |
| Outbox publication after commit; no lost events | invariant #10 (L73); §3.4 | synchronous `WriteMessages` after the insert commits; on failure the event is logged and dropped (handler.go:1920-1928, 1983-1986; publisher.go:125-128) | ❌ | No outbox, so a Kafka outage silently loses `authorization.denied` and `sod.violation.detected`. It is not `go Publish(ctx)`. X3. |
| Event envelope carries tenant_id | Doc 03 §19; Event Catalogue | the envelope struct has no tenant_id (publisher.go:25-36), and its comment (L20-23) wrongly says decisions have no tenant | ⚠️ | The tenant has been recorded since 000005, but it is not propagated. |
| Policy, cache and assignment events | GOV-03 Events (L327: AuthorizationPolicyVersionChanged, AuthorizationCacheInvalidated); ZS-IAM-001 §23 (iam.assignment.granted/revoked, iam.sod_policy.published, iam.role.published) | no admin write publishes anything; the publisher interface has only the three decision events (handler.go:96-100) | ❌ | Role grants and revocations are invisible to identity-context-svc and to audit. |
| Consume role.assigned | §8.3 Consumed (L609) | not consumed | ❓ | Nothing in the estate produces it, and this service itself writes the assignments. |
| Consume authority.delegated (and revoked / expired) | §8.3 (L611); §9.3 | consumer.go:89-93, 274-280; main.go:246-294 | ✅ | |
| Consume employment.changed | §8.3 (L613) | substituted with `principal.status.changed` (lifecycle_consumer.go:143-145); `employee.terminated` is deliberately not consumed (compose comment) | ⚠️ | A reasonable substitution, but undocumented, and termination never reaches this service. |
| Consume entity.scope.updated | §8.3 (L615) | substituted with entity.status / hierarchy / jurisdiction.changed and entity.updated, all of which only invalidate the cache (lifecycle_consumer.go:160-168, 424) | ⚠️ | A DORMANT or DISSOLVED entity is not denied. |
| Consumer diagnostics (ErrorLogger, partition watch) | estate pattern (kafka-go silent stall) | main.go:280-293, 334-370, 432-438 | ✅ | |
| **Consumers are at-least-once and idempotent; no apply is lost** | invariant #10 "consumers assume at-least-once"; §3.7 | `ReadMessage` auto-commits before `Handle`; a failed store write is logged and dropped (consumer.go:191-227; lifecycle_consumer.go:369-378) | ❌ | If the DB is down when a suspension or upstream revocation arrives, **the principal keeps full authority permanently**. The code's own log line says "authority is unchanged until it is replayed". Dedup is in memory only (consumer.go:158-189). |

## Delegation

| Item | Documented | Implemented | Status | Notes |
|---|---|---|---|---|
| Delegation has a mandatory finite period | ZS-IAM-001 §11 effective_to (L499) | `effective_to` is optional (handler.go:896) | ⚠️ | |
| Delegation never exceeds the delegator | §9.3 Critical Constraint (L837); ZS-IAM-001 §11 | intersected with live grants (FindDelegatedActions pg_store.go:1553-1644) | ✅ | |
| Only the delegator may delegate or revoke | ZS-IAM-001 §11, §32 | handler.go:977-983, 1035-1041 | ✅ | |
| No re-delegation; protected privileges cannot be delegated | ZS-IAM-001 §11 constraints (L509) | partly: delegation resolves only the delegator's direct RBAC grants, so re-delegation is structurally impossible (pg_store.go:1553-1644); no action is ever marked non-delegable | ⚠️ | Protected privileges such as `SOD_RULE_MANAGE_GLOBAL` can be delegated. |
| Delegation approval_reference and reason | ZS-IAM-001 §11 (L503-505) | no fields for either (handler.go:880-897) | ❌ | |

## Cross-cutting checks and ownership

| Item | Documented | Implemented | Status | Notes |
|---|---|---|---|---|
| `go Publish(ctx)` with the request context | estate pattern | not present; the publish is synchronous (handler.go:1920-1928); SIEM uses background workers with their own context (tracker 82j) | ✅ | |
| Action names callers use but nothing grants | estate pattern | no seed in migrations 000001-000013; ABAC ships empty; every caller's action is `no_grant` until someone seeds it | ⚠️ | Fails closed, as intended. Seeding is manual, and it is only possible at all because admin writes are unauthorized. `DELEGATION_ADMINISTER` is enforced but deliberately not seeded (progress.md L1927). workflow-svc's `WORKFLOW_APPROVE` is referenced nowhere else in the estate. |
| Fail-open on dependency error | invariant #3 | none on the evaluation path, and jurisdiction validation fails closed (handler.go:1147-1158). But a **missing** `principal_status_projection` table answers ACTIVE (pg_store.go:2180-2185), and a lost consumer apply leaves the principal ACTIVE | ⚠️ | The missing-table case is a deliberate fail-open, added so that migration 000013 can be reverted. |
| RBAC mappings owned here vs the role catalogue in access-control-svc | §8.3 Owns "RBAC mappings" vs §9.4 "role catalogues, permission bundles"; GOV-03 "role/assignment master facts may be owned by IAM administration plane" | full role and bundle CRUD here (handler.go:134-141) | ❓ | Two role and bundle stores; tracker row 79 is Not Started (C7). |
| GOV-04 SoD authority (exceptions, compensating controls, PublishSoDPolicy, ExplainSoDDecision) | GOV-04 (L370-460) | only a rule table and its evaluation here; no service in the estate has exceptions or compensating controls (a grep of sod_rule / SoDRule finds only authorization-svc and policy-svc) | ❓ | Ownership is ambiguous. If this service is GOV-04, the whole exception lifecycle is ❌ (C2). |
| GOV-12 maker-checker and break-glass | GOV-12 (L1103-1195) | not in this service; break-glass references exist only in identity-context-svc, configuration-feature-flag-svc and secret-vault-integration-svc | ❓ | GOV-12 has no evident owner (C2). |

## What the service does do well

- Deny-by-default and fail-closed on every store error.
- No decision is returned without a recorded artifact.
- Deny wins, and SoD and ABAC can only deny.
- Suspension is layer 0.
- RLS uses NULLIF throughout, and a malformed tenant gets 400.
- Cross-tenant resource IDs get 404 without disclosure.
- The Kafka ErrorLogger and the partition watch are wired.
- The delegator must be the caller, and a delegate never gets more than the delegator holds.
- Decision-log retention runs by partition detachment.
- It is the only group member whose consumers carry the stall diagnostics.

## Compliance

**29 of 68 scored items fully met — 43%** (partials at half: **57.4%**). 19 are ❌ and 4 are ❓.
**The percentage understates the risk:** the two critical ❌ rows are the most severe findings
in the group.

**Top gaps by risk**

1. **Any tenant principal can grant themselves anything** (security, critical). Tenant-level
   admin writes evaluate no permission, and there is no self-assignment check:
   handler.go:374, 528, 773-840, 842, 1105-1140, 1196, 1302.
2. **A tenant user can reach platform scope** (security, critical). A tenant-wide assignment
   (`legal_entity_id` NULL) satisfies `requirePlatformAction`, so that user can author
   platform-wide SoD and ABAC rules that deny actions in every tenant: pg_store.go:1414-1418,
   1480-1484; handler.go:289-300; migration 000003.
3. **Any user can switch off the SoD control that blocks them** (security). handler.go:1196-1265,
   1447-1495.
4. **Tenantless `/v1/authorize` is evaluated across tenants** (security). handler.go:1699-1702;
   envelope_policy.go:88-99.
5. **Suspensions and revocations are lost on a DB blip** (security). Offsets are committed before
   apply: lifecycle_consumer.go:369-378; consumer.go:191-227.
6. **Explanation and scope reads are open to any tenant principal, and `/v1/authorize` itself is
   unauthenticated** (security). access_decisions.go:76-79; validation.go:122-127;
   handler.go:1705.
7. **Decision events are lost on a Kafka error** (data integrity). handler.go:1920-1928.
8. **No static SoD check at assignment time** (data integrity). pg_store.go:622-667.
9. **Admin writes publish no events, and the decision event envelope has no tenant_id** (data
   integrity). handler.go:96-100; publisher.go:25-36.
10. **Rules are not versioned; there is no `expected_version`; the bundle upsert overwrites
    `permitted_actions`; idempotency keys are never honoured** (data integrity).
    pg_store.go:462, 633, 1155, 1252.
11. **The decision record lacks the GOV-03 and §18 fields** (data integrity): obligations,
    policy versions, resource, attributes digest, and the STEP_UP / REQUIRE_APPROVAL outcomes.
12. **Doc drift** (cosmetic): the error vocabulary does not match §16; 22 admin routes are
    undocumented; consumed-event names were substituted without updating the doc. progress.md
    L1912 says "Nothing left inside this service", which contradicts gaps 1-3, and the comment
    at handler.go:293-295 is false.

**Needs clarification**

1. **C1 — GOV-04.** Is authorization-svc the GOV-04 SoD authority? If so, the exception and
   compensating-control lifecycle is missing entirely, and no other service owns it.
2. **C2 — GOV-12.** Which service owns maker-checker and break-glass? Nothing in the estate
   implements CheckerDecision or BreakGlassGrant.
3. **C3 — `role.assigned`.** It is listed as consumed, but this service writes assignments
   itself and nothing produces the event. Should it be dropped from §8.3, or produced by
   someone?
4. **C4 — role and bundle master data.** This service (§8.3 "RBAC mappings") or
   access-control-svc (§9.4)? Tracker row 79 is Not Started, and the answer decides where
   gap 1 has to be fixed (group C7).
5. **C5 — event naming.** §8.3 uses `authorization.granted`, GOV-03 uses `AuthorizationDenied`
   (and lists no granted event), and ZS-IAM-001 §23 uses `iam.*`. Which is the wire contract?
6. **C6 — paths.** GOV-03 uses `/internal/v1/gov03/...` and ZS-IAM-001 §21 uses
   `POST /internal/authorization/decisions`, while the code uses `/v1/authorize`. Is either
   binding (group C4)?

## Gap closure, 7 October 2026

Every in-service gap was fixed against the documents (Doc 03 §8.3, GOV-03 / 04 / 12,
ZS-IAM-001). The ❓ items were decided from those documents: this service is the GOV-04
SoD authority (exceptions with compensating controls, never self-approved, always
expiring); it holds maker-checker for privileged assignments (GOV-12, §9, A20); and
access-control-svc authors the role catalogue, which this service enforces. The detail
is in `services/authorization-svc/progress.md` ("Governance Platform audit closure") and
`ADMIN-API.md`. Uncommitted; migrations 000020–000027 must be applied before the build.

| | ✅ | ⚠️ | ❌ | Full | Weighted |
|---|---|---|---|---|---|
| 28 Sep audit | 29 | 20 | 19 | 42.6% | 57.4% |
| **7 Oct, after fixes** | **58** | **10** | **0** | **85.3%** | **92.6%** |

All 19 ❌ are closed. The 10 ⚠️ left each wait on something outside the service:

- the console sending `reason` (then `AUTHZ_COMMAND_CONTRACT=enforce`), which covers 2 rows;
- the ~86 tenantless `/v1/authorize` callers sending `X-Tenant-Id`, which covers 3 rows;
- the mTLS / gateway identity rollout, which covers 2 rows;
- an employee-to-principal mapping (`employment.changed`);
- per-service grant seeding;
- a version component in the cache key (invalidation already broadcasts).

The integration suites found one regression in this pass, and it is fixed:
`GET /v1/access-decisions` answered 503 once the evidence columns were added. Both
store suites now pass on a fully migrated database (13/13 as the app role, 66/66 as the
owner).

---

# 6/7 — workflow-svc (:8090) vs Doc 03 §8.4, GOV-06 and R-001 WFC-02 / WFC-03

**Contract extracted from:**
- `03-microservices.md` §8.4 Workflow & Approvals Service (L639-701), the primary per-service
  spec.
- Governance Control Plane: §2 invariants (L47-75), **GOV-06 Workflow & Approval
  Orchestration** (L554-644), GOV-12 Maker-Checker (L1103-1195) where it touches approvals, §16
  envelopes and errors, §17 events, §18 data model.
- R-001 (Workflow, Approval, Case & Obligation Control):
  - §2 R-INV-01…30
  - §4 WFC-02 orchestrator (L278-341) and §5 WFC-03 approval/SoD (L345-405)
  - §8.3 concurrency, §9 data model, §10 API and events, §11 security
- Tracker rows 6, 14a, 65 and 82i, and `input-contract-conformance.md` L89.
- `progress.md`, read as claims.

**Code:** `services/workflow-svc/`. Files read:
- `cmd/server/main.go`
- `internal/handler/handler.go`, `internal/store/pg_store.go`
- `internal/authz/client.go`, `internal/events/publisher.go`
- `internal/domain/types.go`, `internal/middleware/tenant.go`
- `internal/envelope/*`, `internal/config/config.go`
- migrations 000001-000004
- `deployments/docker-compose.yml:1244-1270`, `traefik-dynamic/all-services.yml:536-543`,
  `init-db-phase5.sh:88`

Cross-checked against the `/v1/authorize` contract in
`authorization-svc/internal/handler/handler.go:1507-1530, 1705-1790`, and against
workflow-history-svc, the only consumer of `zoiko.workflow.events`. There is no Postman
collection.

**Framing.** The service covers Doc 03 §8.4's five capabilities (create, next approver, action,
escalate, cancel) and publishes the five event names that doc lists. Its self-approval ban holds
at both create and decision time, and its authorization-svc check fails closed. Tenant isolation
on `workflow_instances` holds.

Almost every ❌ is something GOV-06 and R-001 specify and v1 does not have:
- versioned definitions
- server-resolved routes
- delegation and quorum
- deadlines and timers
- subject binding
- an outbox
- evidence writes

R-001 splits this area across **five services** (WFC-01…05). Whether workflow-svc is meant to be
WFC-02 plus WFC-03, or only the §8.4 v1, is ❓ (C1).

## Implemented surface

Routes (`handler.go:54-63`):
- `POST /v1/workflows`
- `GET /v1/workflows/{id}`
- `GET /v1/workflows/{id}/next-approver`
- `POST /v1/workflows/{id}/actions` (APPROVE | REJECT)
- `POST /v1/workflows/{id}/escalate`
- `POST /v1/workflows/{id}/cancel`
- `/healthz`, `/readyz`, `/metrics` (`main.go:140-143`)

**Middleware** (`main.go:121-135`): RequestID → RealIP → Recoverer → otelchi → metrics →
correlationID → Logger → TenantContext (X-Tenant-Id) → canonical envelope.
- correlationID fills X-Correlation-ID from the request id when it is missing.
- The envelope runs write-strict by default, with `LegalEntityID: RequiredOnWrite` and
  Idempotency-Key required on writes.

**State:** an instance moves `PENDING → APPROVED | REJECTED | ESCALATED | CANCELLED`, and
`ESCALATED → CANCELLED` only. Stages are an ordered list of `approver_principal_id` supplied by
the caller.

**Tables:**
- `workflow_instances`: RLS with NULLIF, FORCE.
- `workflow_stages`: no RLS.
- `workflow_transitions`: no RLS.

There are no background jobs, no timers, no Kafka consumers and no outbox. The producer topic is
`zoiko.workflow.events` (`config.go:228`), written by a synchronous `kafka.Writer` with no
message key.

## Commands and queries

| Item | Documented | Implemented | Status | Notes |
|---|---|---|---|---|
| Create approval workflow | 03 §8.4 Inbound APIs; R-001 §10.1 `POST /v1/workflows` | handler.go:57, 161-240 | ✅ | 201 created / 200 replay / 400 / 401 / 403 / 503. |
| Resolve next approver | 03 §8.4; GOV-06 GetRequiredRoute | handler.go:59, 274-288; pg_store.go:170-186 | ⚠️ | A terminal workflow returns 404 `workflow_not_found` (pg_store.go:175-176), which looks the same as a missing id. Returns only the current stage, not the route. |
| Submit approval action | 03 §8.4; GOV-06 Approve/Reject | handler.go:60, 319-419 | ✅ | APPROVE / REJECT only. |
| Escalate pending workflow (endpoint) | 03 §8.4; GOV-06 Escalate | handler.go:61, 426-449; pg_store.go:422-434 | ✅ | The endpoint exists. Its authorization gap and its semantics gap are recorded below. |
| Cancel workflow | 03 §8.4; GOV-06 CancelWorkflow; R-001 §10.1 | handler.go:62, 456-479; pg_store.go:438-450 | ✅ | PENDING / ESCALATED → CANCELLED; idempotent. |
| GetWorkflow | GOV-06 queries; R-001 §10.1 | handler.go:58, 247-266 | ✅ | Returns the instance and its stages; transitions are not exposed. |
| ListPendingApprovals | GOV-06 queries | — | ❌ | No list endpoint of any kind. |
| GetApprovalEvidence | GOV-06 queries | — | ❌ | `workflow_transitions` is written but cannot be read through the API. |
| RequestChanges decision | GOV-06 commands; R-001 §5.1/5.2 CHANGES_REQUESTED | handler.go:341 | ❌ | Only APPROVE / REJECT are accepted. |
| Expire / approval expiry | GOV-06 Expire, WorkflowExpired; R-INV-11; R-001 §5.1 Expiry | — | ❌ | No expiry fields, no timers, no Expired state. |
| Subject binding and material-change invalidation (RevalidateApproval, ApprovalInvalidated) | GOV-06 SoD row, negative path #1; R-INV-07, R-INV-10; R-001 §5.3 | createWorkflowRequest handler.go:132-137 | ❌ | No subject_id, version or hash, so invalidation is impossible. |
| Path and command shape | 03 §8.4 names capabilities only; R-001 `/v1/workflows/{id}/commands/cancel`, `/v1/approvals/{id}/decisions`; GOV-06 `/internal/v1/gov06/commands/*` | handler.go:57-62 | ❓ | Three documents give three different surfaces (C4). |

## Definitions, routes and approver authority

| Item | Documented | Implemented | Status | Notes |
|---|---|---|---|---|
| Owns versioned, immutable workflow definitions; each instance pinned to a version | 03 §8.4 Owns; §2 invariants #4, #11; R-INV-01/02; GOV-06 negative path #4 | migration 000001 (no definition tables); domain/types.go:154-175 (no version field) | ❌ | No definition entity and no process_version_id. `workflow_type` is a free string. X8. |
| Route and candidate approvers are server-resolved | GOV-06 "Server-resolved: route; quorum; due dates; delegation; signing authority"; R-001 §5.1 "Policy returns requirements" | handler.go:185-194; domain/types.go:177-181 | ❌ | **The initiator chooses their own approvers**, and nothing checks at create that they are eligible. |
| Initiator may not be an approver (at create) | GOV-06 SoD; R-INV-08; GOV-12 | handler.go:199-204 | ✅ | 400 `initiator_cannot_be_approver`. |
| No self-approval (at decision time) | GOV-06 SoD; R-INV-08 | handler.go:358-361 | ✅ | 403 `self_approval_not_allowed`. No `self_approval_blocked` event (R-001 §10.2). |
| Approver authority checked with authorization-svc, fail-closed | 03 §8.4 Critical Constraint; GOV-06 Authorization | handler.go:363-373; authz/client.go:71-104 | ✅ | Deny → 403; unreachable or non-200 → 503. |
| Authz call carries tenant scope and the canonical envelope | §16 tenant_id; §2 invariant #1; tracker row 82i | authz/client.go:55-59, 77-81 | ❌ | The body has no `tenant_id` and no headers are forwarded, so authorization-svc sees a tenantless request (`resolveTenantScope`, authz handler.go:1737). These decisions are logged with a NULL tenant and are invisible to the tenant-scoped audit read. X6. |
| Underlying approval authority, not just a workflow permission | GOV-06 "workflow action + underlying approval authority" | authz/client.go:65-69 | ⚠️ | One platform-wide `WORKFLOW_APPROVE` action for every workflow_type. No other file in the estate references `WORKFLOW_APPROVE`, so a fresh deployment denies every approval until someone seeds the grant by hand. |
| Independence and relationship SoD beyond requester ≠ approver | R-INV-09; R-001 §5.1 Independence; §11.3 self-approval through delegation | authz/client.go:55-59 | ⚠️ | authorization-svc accepts `resource_owner_principal_id` for own-object SoD (authz handler.go:1515-1523), but workflow-svc never sends it, so only the local equality check applies. |
| Approver eligibility re-evaluated at decision time | R-001 §11.1; GOV-06 "revalidated at execution" | handler.go:363 (on every action) | ✅ | |
| **Authorization on escalate and cancel** | GOV-06 "system transitions restricted"; R-001 §10.1 cancel "actor authorization" | handler.go:426-479 | ❌ | **Any principal holding a tenant header can cancel or escalate any workflow in that tenant.** There is no authz call and no initiator or role check. |
| Delegation paths | 03 §8.4 Owns; GOV-06; R-001 §4.2 Delegation; negative path #2 | pg_store.go:156-168, 320-341 | ❌ | The stage match requires `approver_principal_id == caller`, so a delegate always gets 403 `wrong_approver`. |
| Quorum and parallel approvals | GOV-06 server-resolved quorum; R-001 §5.1, §8.3 | pg_store.go:367-381 | ❌ | Strictly sequential, one approver per stage. |
| Well-formed chain (the same approver in two stages) | GOV-06 "Missing approver => pending exception" | pg_store.go:157 (`LIMIT 1` with no ORDER BY); handler.go:189-204 (no duplicate check) | ❌ | If X is listed at stages 1 and 3, the stage-3 lookup can return stage 1 (already APPROVED) and treat the call as an idempotent replay. Stage 3 then never advances and the workflow is **stuck for good**. progress.md:51-54 acknowledges this; nothing enforces it. |
| Required source inputs | GOV-06: business object ref and version, requested transition, subject fingerprint, amount / risk / jurisdiction | handler.go:132-137 | ❌ | Only tenant, legal_entity_id, workflow_type and stages. |

## Deadlines, escalation and concurrency

| Item | Documented | Implemented | Status | Notes |
|---|---|---|---|---|
| Approval deadlines, SLA timers and timer recovery | 03 §8.4 Owns "approval deadlines", "escalation logic"; GOV-06 NFR "timer recovery"; R-001 §4.4 | — (main.go starts no goroutines or jobs) | ❌ | No due_at, no next_action_at, no reconciler. Escalation is manual only, and untouched instances stay PENDING forever. |
| Escalation semantics | R-001 §4.2 "Escalation can change queue/priority/owner but cannot silently complete" | pg_store.go:308-312, 422-434 | ⚠️ | **ESCALATED is a dead end.** SubmitAction accepts only PENDING, so an escalated workflow can never be approved, only cancelled. There is no escalation target and no change of owner. |
| A timeout never counts as approval | GOV-06 failure semantics; negative path #3 | — | ✅ | Holds trivially, because there are no timeouts. |
| Durable long-running state and stuck-instance recovery | GOV-06 NFR; R-001 §4.4, R-INV-26 | — | ❌ | |
| A duplicate approval is idempotent (sequential replay) | 03 §8.4 Idempotency Requirement; GOV-06 | pg_store.go:320-333; handler.go:394 | ✅ | A replay gets 200, with no new transition and no re-publish. |
| **Concurrency control / expected_version** | R-001 §8.3; §16 expected_version; 03 §8.4 idempotency | pg_store.go:304-342 (read outside the transaction), 355-360, 383-389 (`WHERE ... id` only), 423-433, 464-470 | ❌ | Check-then-act with no row lock and no status predicate. Two concurrent identical approvals can both commit, writing duplicate transitions and a duplicate `approval.granted`. A cancel racing a final approval can commit both: the last write wins and both events are published. |
| Idempotency-Key honoured; IDEMPOTENCY_MISMATCH | GOV-06 commands "Idempotency-Key"; §16; R-001 idempotency_record | envelope/policy.go:220-227 requires the header; dedup is on correlation_id (pg_store.go:215-219; migration 000003) | ❌ | The key is demanded and then ignored. Two distinct workflows started under one business correlation_id collapse: the second silently returns the first (200). A replay with a different body goes undetected. X4. |
| correlation_id is mandatory (caller-supplied, end to end) | §16; ZS-ARCH-SVC-001 §4 | main.go:126, 175-183 (runs before the envelope at 135) | ⚠️ | The service generates a correlation_id from the request id before validation, so the mandatory check can never fail. |

## Evidence

| Item | Documented | Implemented | Status | Notes |
|---|---|---|---|---|
| Preserve every workflow transition | 03 §8.4 Evidence Obligations | pg_store.go:253, 401, 481 (in the same transaction as the state change) | ✅ | |
| Transition log is append-only | 03 §8.4; R-INV-21/25 | migration 000001:71 (a comment only) | ⚠️ | No trigger and no REVOKE UPDATE/DELETE. Stage rows, which hold the decisions, are mutable. |
| Preserve approver identity and outcome | 03 §8.4 | pg_store.go:355-360, 401 | ✅ | acted_by comes from the verified header. |
| Preserve rationale where required | 03 §8.4 | pg_store.go:357, 396 | ⚠️ | The user's rationale is stored only on the mutable stage row; the transition gets a synthetic `"stage N APPROVED by X"`. Rationale is never required, not even for REJECT. |
| Evidence and decision-log write; evidence_id returned | §16 evidence_id; GOV-06 Dependencies "evidence"; §2 invariant #9 | — | ❌ | No call to governance-decision-log or any evidence service, and no evidence_id in responses. X2. |
| Evidence lineage: object version, route version, authority snapshot, delegation | GOV-06 Evidence/lineage; §18 | — | ❌ | No authority snapshot; the authz decision id is not stored. |

## Events

| Item | Documented | Implemented | Status | Notes |
|---|---|---|---|---|
| Published event names | 03 §8.4: workflow.started, approval.granted, approval.rejected, workflow.escalated, workflow.completed | publisher.go:58-104 | ✅ | All five are present, and workflow-history-svc consumes them. |
| Event names per GOV-06 / §17 / R-001 | GOV-06: WorkflowStarted, ApprovalRecorded, WorkflowApproved, WorkflowRejected, ApprovalInvalidated, WorkflowExpired; R-001 §10.2 snake_case | publisher.go | ❓ | Three incompatible catalogues (C3). |
| Outbox; publication after the authoritative commit | §2 invariant #10; R-INV-23; R-001 §4.3 | handler.go:229-231, 396-407, 444-446, 474-476 | ❌ | Direct Kafka write after commit, with failures only logged. An idempotent replay never re-publishes, so a lost event is **lost permanently and cannot be detected**. X3. |
| Publish context | request-context publish pattern | handler.go:229, 396 (synchronous, `r.Context()`) | ⚠️ | No `go Publish` (good), but a client disconnect after commit cancels the publish and drops the event. |
| Cancellation event | R-001 §10.2 workflow_cancelled; 03 lists none | handler.go:474 | ⚠️ | Cancel emits `workflow.completed` with status CANCELLED. |
| Event envelope minimum | R-001 §10.3; §2 invariant #9 | publisher.go:25-37, 141-147 | ⚠️ | event_id travels only as a Kafka header. Missing: causation_id, subject ref, process_version_id, plane, payload hash, evidence refs. Jurisdiction is always empty. |
| Per-aggregate ordering | R-001 §8.3 (implied) | main.go:109-114 (no Key; LeastBytes balancer) | ⚠️ | With more than one partition, `approval.granted` can arrive before `workflow.started`. The default BatchTimeout adds up to ~1 s to each synchronous write. |
| Consumed events | 03 §8.4: authorization.denied, policy.updated, authority.delegated | — (progress.md:37-47 defers them) | ❌ | No consumer code. |

## Tenant, identity and state model

| Item | Documented | Implemented | Status | Notes |
|---|---|---|---|---|
| Tenant required before data access | §2 invariant #1; R-INV-22 | handler.go:75-85 | ✅ | 401 `missing_tenant_scope`. |
| A client-supplied tenant is not authoritative | §2 invariant #2 | handler.go:110-119, 182 | ✅ | A body tenant that differs from the header gets 403. |
| RLS with NULLIF (empty-GUC trap) | 04-data-model; tracker row 6 | migration 000004:15-20; pg_store.go:58-72 | ✅ | FORCE RLS; the runtime role `zoiko_app` is NOBYPASSRLS (init-db-phase5.sh:88). |
| Isolation at the persistence boundary for stages and transitions | R-INV-22 "again at persistence boundary" | pg_store.go:131, 158, 179 (pool, no transaction or GUC); no RLS on those two tables | ⚠️ | Isolation depends on the app layer calling FindWorkflowByID first. |
| Legal entity validated by the server | §2 invariant #2; §16 entity_id "server-validated" | handler.go:143, 209 | ⚠️ | The body's `legal_entity_id` is compared neither with X-Legal-Entity-Id nor with the registry. |
| A malformed id gets 400/404, not 5xx | 500-vs-404 pattern | pg_store.go:101, 109-114 | ⚠️ | A non-UUID path id, tenant header or legal_entity_id fails the uuid cast and maps to 503 `store_unavailable` (X10). |
| Stable error classes | §16 | handler.go:175-491 | ⚠️ | Ad-hoc snake_case codes (`self_approval_not_allowed`, `wrong_approver`, `invalid_transition`). X9. |
| Actor comes from verified identity, not the body | §16 subject_id "server-resolved"; R-001 §11.3 approval spoofing | handler.go:96-106, 206-211, 328 | ✅ | |
| Gateway authentication in front of the service | §16; tracker 82i | traefik-dynamic/all-services.yml:536-543 | ⚠️ | The local route has only stripPrefix, so X-Tenant-Id and X-Principal-Id can be spoofed there; compose also publishes 8090 directly (compose:1251-1252). X5. |
| Instance state model | GOV-06: Pending / InProgress / Approved / Rejected / Cancelled / Expired / Invalidated | domain/types.go:163-165 | ⚠️ | ESCALATED is undocumented; InProgress, Expired and Invalidated are missing. |
| Workflow state and approval state are not collapsed | R-INV-03 | workflow_status carries both | ⚠️ | One status column holds both the execution state and the approval outcome. |
| Approval does not execute the business transition | §2 invariant #5 | only events are published | ✅ | |
| Common columns | R-001 §9.1: row_version, updated_at, created_by, plane, data_class, residency_region | migration 000001:22-83 | ❌ | None of them are present. |
| Observability (traces, metrics) | 03 §3.8 | main.go:64-80, 124-125, 142 | ✅ | |
| Health and readiness probes | platform baseline | main.go:140-141; health.go | ✅ | |

## What the service does do well

- The self-approval ban holds twice: at create (initiator ≠ approver) and at decision time.
- Approver authority is re-checked with authorization-svc on every action and fails closed.
- The actor always comes from the verified header.
- Tenant scope is required, and a body naming a foreign tenant gets 403.
- `workflow_instances` has forced RLS with NULLIF under a NOBYPASSRLS role.
- Every transition is written in the same transaction as the state change.
- Sequential duplicate approvals are idempotent.
- Approval never executes the business transition itself.
- The five Doc 03 events are produced and consumed.

## Compliance

**21 of 61 scored items fully met — 34%** (partials at half: **49.2%**). 22 are ❌ and 2 are ❓.

**Top gaps by risk**

1. **Escalate and cancel have no authorization** (security). Any tenant principal can cancel or
   escalate any workflow (handler.go:426-479).
2. **The initiator chooses the approvers** (security). There is no server-resolved route and no
   eligibility check at create, so the self-approval ban can be bypassed by naming a colluding
   approver (handler.go:185-204).
3. **The authz call is tenantless and carries no envelope** (security, audit). Decisions are
   logged with a NULL tenant (authz/client.go:55-81; tracker 82i).
4. **A single `WORKFLOW_APPROVE` action, granted nowhere in the estate** (security). It does not
   reflect the underlying business authority, and every approval is denied until someone seeds
   it by hand (authz/client.go:69).
5. **Own-object SoD is not passed to authorization-svc** (security). authz/client.go:55-59.
6. **Delegation cannot work** (security, function). pg_store.go:156-168.
7. **Check-then-act races** (data integrity). Concurrent approve + approve, or approve + cancel,
   both commit and publish contradictory events (pg_store.go:304-409, 452-490).
8. **No outbox; events are lost permanently on a Kafka failure or client disconnect** (data
   integrity). handler.go:229, 396-407, 474.
9. **Idempotency is keyed on correlation_id, not Idempotency-Key** (data integrity). Distinct
   starts in one saga collapse into one (pg_store.go:215-219; migration 000003).
10. **A duplicate approver across stages strands the workflow** (data integrity). pg_store.go:157.
11. **ESCALATED is a dead end, and there are no deadlines or timers** (data integrity).
    pg_store.go:308, 422-434.
12. **No versioned definitions, subject binding or material-change invalidation** (data
    integrity). Migration 000001; handler.go:132-137.
13. **No evidence or decision-log write** (data integrity). Rationale lives only on the mutable
    stage row, and "append-only" transitions are append-only by comment only (pg_store.go:396;
    migration 000001:71).
14. **Doc drift** (cosmetic):
    - malformed UUIDs return 503 instead of 400/404 (pg_store.go:101-114)
    - error codes are ad hoc rather than the §16 classes (handler.go:175-491)
    - cancel emits `workflow.completed` (handler.go:474)
    - ESCALATED is undocumented (domain/types.go:163-165)
    - the envelope lacks the R-001 §10.3 fields (publisher.go:25-37)
    - there is no Kafka message key, so per-instance ordering is not guaranteed
      (main.go:109-114)

**Needs clarification**

1. **API surface.** Doc 03 §8.4 lists capabilities only; R-001 §10.1 gives
   `/v1/workflows/{id}/commands/cancel` plus a separate `/v1/approvals/{id}/decisions`; GOV-06
   gives `/internal/v1/gov06/commands/*`. Which is the contract (C4)?
2. **Event names.** Doc 03 uses dotted names (implemented and consumed), GOV-06/§17 uses
   PascalCase with a different set, and R-001 §10.2 uses snake_case, including
   workflow_cancelled and self_approval_blocked (C3).
3. **Scope vs WFC-01…05.** Is workflow-svc meant to be WFC-02 + WFC-03, or only the §8.4 v1?
   This decides whether the list/evidence queries, RequestChanges, expiry, subject binding,
   definitions, quorum, deadlines, source inputs, common columns and durable-state rows are
   gaps or out of scope (C1).
4. **Consumed events.** Nothing documents how the service should react to
   authorization.denied or policy.updated. That behaviour must be specified before it can be
   built.
5. **Service accounts as approvers.** R-INV-08 says service accounts cannot satisfy human
   independence, but the handler accepts any X-Principal-Id, and the docs don't say how this
   service should tell a workload principal from a human.

---

# 7/7 — evidence-requirements-svc (:8130) vs Doc 03 §8.6, Doc 04 §7.1 and the Governance Control Plane invariants

**Contract extracted from:**
- `03-microservices.md` §8.6 Evidence Requirements Service (L737-762), the primary per-service
  spec. It covers purpose, five owned concerns, two events and one critical constraint, and
  lists **no endpoints, schemas or status codes**.
- `04-data-model.md` §7.1 EvidenceRequirement (L946-962) and the relationship at L1012.
- `01-backend.md:603` (Evidence Requirements Engine: document preconditions, signature
  requirements, certification checks, prior-approval validation).
- Governance Control Plane:
  - §1 "No evidence afterthought" (L41-42)
  - §2 invariants #1–#11 (L47-72)
  - GOV-07 (L645-736)
  - §16 envelope and error classes (L1196-1222), §17 events, §18 data model
  - negative test #13 (L1346)
- ZS-DATA-GOV-001. Its "GOV-07" at L135 is *data-quality populations*, a different numbering
  scheme.
- `ownership.txt`: this service is Governance Platform 7/7. document-vault-svc,
  evidence-manifest-svc and audit-event-store-svc sit in Group 3, "Evidence, Audit & Utility".
- `backend-completion-tracker.md:160` and `master-register-findings-2026-08-27.md:594`.

Because §8.6 documents no endpoints, most of the contract here comes from the Control Plane
invariants and §16. `context.md` (§4 API, §5 semantics, §9 retrofit, §11 decisions) and
`progress.md` were read as claims.

**Code:** `services/evidence-requirements-svc/`. Files read:
- `cmd/server/main.go`
- `internal/{handler,domain,store,events,authz,documentvault,envelope,middleware,config}`
- migrations 000001-000003
- `deployments/docker-compose.yml:2269-2305`, `traefik-dynamic/all-services.yml:224-231`,
  `kubernetes/manifests/24-app-evidence-requirements.yaml`
- `postman/postman/ZoikoSuite_EvidenceRequirements.postman_collection.json`

The estate was grepped for callers:
- `board-resolutions-svc`, `corporate-actions-svc` and `filing-preparation-svc`, each through
  its `internal/evidencereq/client.go` and handler.
- The dependency `document-vault-svc/internal/handler/handler.go` was also checked.

**Framing.** The service is well built on tenant isolation and catalogue authorization. Most of
its failures are in **how the gate is wired to its callers and dependencies**. As deployed:
- no caller can pass the gate;
- document verification cannot succeed against the real document-vault;
- 4 of the 7 finalization paths have no gate at all;
- two of the three callers that are wired would fail open if the gate did work.

## Implemented surface

Router: `handler.go:80-94`; probes: `main.go:189-192`.

| Method | Path | Auth in code | Statuses |
|---|---|---|---|
| POST | /v1/evidence/evaluate | tenant header + X-Principal-Id only, **no authz** | 200; 400 missing_field / invalid_json / missing_tenant; 401 identity_missing; 413; 503 store / document_service_unavailable; 500 |
| GET | /v1/evidence/evaluations/{evaluation_id} | tenant header only | 200; 404; 400 missing_tenant; 503 |
| GET | /v1/evidence-requirements | tenant header only | 200; 400 invalid_field; 403 tenant_scope_mismatch; 503 |
| GET | /v1/evidence-requirements/{id} | tenant header only | 200; 404; 503 |
| POST | /v1/admin/evidence-requirements | authz `EVIDENCE_REQUIREMENT_CREATE` | 201 new / 200 replay; 400; 401; 403 tenant_scope_mismatch / authorization_denied; 503 |
| POST | /v1/admin/evidence-requirements/{id}/end-date | authz `EVIDENCE_REQUIREMENT_RETIRE` | 200; 400 reason; 404; 422 already_retired; 403; 503 |
| GET | /healthz, /readyz, /metrics | none (envelope-exempt) | |

**Middleware** (`main.go:165-183`): RequestID, RealIP, Recoverer, otelchi, metrics, correlation-id
echo, TenantContext (`X-Tenant-Id`), Logger, then the canonical envelope in default
**write-strict** mode (`internal/envelope/middleware.go:345-353`).
- The envelope policy sets `LegalEntityID: RequiredOnWrite` and requires Idempotency-Key on every
  non-GET (`internal/envelope/contract.go:12-24`, `policy.go:218-227`).
- There is no `MaterialWrite` override, so `POST /v1/evidence/evaluate` counts as a material
  write.
- There is no rate limiting.

**Kafka:** a single producer on `zoiko.evidence-requirements.events` (`config.go:454`). The wire
event types, `evidence.requirement.satisfied` and `evidence.requirement.missing`
(`publisher.go:88, 94`), match §8.6 word for word. **Nothing in the estate consumes them**, and
the service has no consumer of its own.

## Purpose and sufficiency (Doc 03 §8.6)

| Item | Documented | Implemented | Status | Notes |
|---|---|---|---|---|
| Purpose: decide what evidence must exist before an action completes | 03 §8.6 L739-740 | handler.go:101-206 (evaluate plus the catalogue) | ✅ | |
| **Owns document requirements, and verifies document evidence** | 03 §8.6 L746; context §11.2 (verify) | documentvault/client.go:326-332 sends only `X-Tenant-Id` | ❌ | document-vault's `GetDocument` needs `X-Principal-Id` (document-vault-svc handler.go:191; 401 at L529-531) plus a `DOCUMENT_READ` grant (L204). A 401 maps to `ErrDocumentServiceUnavailable` (client.go:345-348), so **every evaluate that includes a SUPPORTING_DOCUMENT gets 503** (handler.go:146-151). The verification path cannot work against the real dependency; the unit tests pass only because they stub it. |
| Owns signature requirements | 03 §8.6 L748; 01-backend L603 | a free-form `evidence_type` (handler.go:231-236) | ⚠️ | A signature counts as present whenever the caller says so. Nothing verifies it, and there is no e-signature service; context §10 records this as accepted. |
| Supporting artifact rules (subtype, minimum count) | 03 §8.6 L750 | domain/types.go:83-93; handler.go:301-321 | ✅ | Data-driven, with no jurisdiction branch, which meets the doctrine. |
| Evidence sufficiency logic; fail-closed on a malformed rule | 03 §8.6 L752; GCP §2 invariant #3 | handler.go:268-337; an unreadable payload counts as unmet (L285-297) | ✅ | |
| **Sufficiency counts distinct artifacts** | implied by §8.6 "sufficiency" | handler.go:238-241 appends the memoised result again when a `reference_id` repeats | ❌ | One real document offered twice satisfies `minimum_count: 2`, which bypasses the gate's integrity check. |
| Non-document artifacts | §8.6 constraint; context §11.2 | handler.go:231-236 sets `counts: true` without checking, even when `reference_id` is empty | ⚠️ | An empty `reference_id` still counts. filing-preparation-svc submits `EVIDENCE_MANIFEST` refs (its handler.go:266-270), and these are never checked against evidence-manifest-svc. |
| Which document statuses (superseded, revoked, draft) count as valid evidence | not specified | not checked (documentvault/client.go:268-271 comment) | ❓ | |

## The gate as wired into the estate

| Item | Documented | Implemented | Status | Notes |
|---|---|---|---|---|
| **"No finalization path may skip required evidence states"** | 03 §8.6 Critical Constraint L760-761 | callers exist for board-resolutions pass (handler.go:541), corporate-actions execute (handler.go:305) and filing-preparation validate (handler.go:273) | ❌ | **general-ledger post, financial-close lock, vat-gst and corporate-tax have no gate** (no `evidencereq` package or env var in any of them). context §9.1 names GL /post and FC /lock as the v1 targets, and those are exactly the two that were not wired. |
| **Callers can reach the gate under the envelope contract** | ZS-ARCH-SVC-001 §4 via envelope/policy.go:183-232 | all three caller clients send only `Content-Type`, `X-Tenant-Id`, `X-Principal-Id` and `X-Correlation-ID` (board-resolutions-svc evidencereq/client.go:87-93; the same in corporate-actions and filing-preparation) | ❌ | No `X-Request-Id`, `X-Source-Channel`, `Idempotency-Key` or `X-Legal-Entity-Id`, so write-strict returns 400 envelope_incomplete. Each caller maps any non-200 to `ErrServiceUnavailable`, so **every gated pass, execute and validate is refused**. It fails closed, but the gate as a whole is unusable. Compose sets no `ZS_ENVELOPE_ENFORCEMENT` for 8130. X1. |
| **Callers treat only SATISFIED (and agreed outcomes) as a pass** | §8.6 constraint | corporate-actions evidencereq/client.go ends with `if res.Outcome == "MISSING" {…} return nil`, and filing-preparation does the same | ❌ | A deny-list, so it fails open: an empty or unknown outcome passes. board-resolutions was already fixed to an allow-list. The code is on the caller side, but it is the gate's wiring. |
| Whether NO_REQUIREMENTS_DEFINED blocks or permits | context §11.4 "STILL OPEN" | all three callers permit it | ❓ | The catalogue starts empty, so once the envelope issue is fixed, every gated action will pass with no evidence (C9). |

## Events

| Item | Documented | Implemented | Status | Notes |
|---|---|---|---|---|
| Event `evidence.requirement.missing` | 03 §8.6 L756 | events/publisher.go:92-94 | ✅ | The wire name matches; nothing consumes it. |
| Event `evidence.requirement.satisfied` | 03 §8.6 L758 | events/publisher.go:87-88 | ✅ | NO_REQUIREMENTS_DEFINED emits nothing (L95-100), by design. |
| Event envelope (Doc 03 §19 / GCP §16: causation, jurisdiction, policy versions) | GCP §16 L1196-1220 | publisher.go:283-295 | ⚠️ | No `causation_id`, no jurisdiction context, no requirement or version references. |
| Outbox publication after commit; at-least-once delivery | GCP §2 invariant #10 (L69); §1 "not asynchronous best effort" | handler.go:180-188 publishes synchronously after commit; publisher.go:383-389 only logs failures | ❌ | No outbox, even though the comment at publisher.go:309-311 claims "an outbox pattern handles redelivery". It publishes on the request context, so a client disconnect cancels the write, and a replay deliberately does not republish (handler.go:189-197). A lost event is **lost for good and undetectable**. X3. |

## Idempotency

| Item | Documented | Implemented | Status | Notes |
|---|---|---|---|---|
| **`idempotency_key` mandatory for stateful commands; IDEMPOTENCY_MISMATCH class** | GCP §16 L1212, L1221 | pg_store.go:351, 364-371 dedups evaluations on `(tenant_id, correlation_id)`; handler.go:187-205 | ❌ | The envelope requires `Idempotency-Key` and then **ignores** it. `correlation_id` is the end-to-end business trace (§16 L1200), and callers pass the inbound `X-Correlation-ID` (board-resolutions handler.go:530-533). A second evaluation in the same flow, even for a different action, legal entity or set of artifacts, gets **the first evaluation's outcome** back with 200. The response does not echo `domain_code` / `action_type`, so the caller cannot tell. **A SATISFIED result can be replayed across actions.** X4. |
| Idempotent requirement create | GCP §16 | pg_store.go:138, 153-165; handler.go:544-548 | ❌ | When two *different* requirements are created under one correlation_id, the second is silently dropped and answered 200 with the first row, so a bulk seed from one workflow loses rows. |

## Auth, tenant and envelope

| Item | Documented | Implemented | Status | Notes |
|---|---|---|---|---|
| Catalogue mutation is authorized, with no self-authorization; fail-closed | GCP §2 invariant #3; context §4 | handler.go:515-518, 592-595; authz/client.go:231-252 | ✅ | An unreachable authz service gives 503, and the result is not cached (authz/client.go:132-136). |
| **Legal entity is validated by the server, not taken from the client** | GCP §2 invariant #2 (L53); §16 L1206 | evaluate takes `legal_entity_id` from the body (handler.go:107) with no authz, no comparison with `X-Legal-Entity-Id` and no registry check | ❌ | Any principal in the tenant can write ledger records and trigger events for any legal entity. It is not even validated as a UUID (see the write-path row below). |
| Read contracts are "strongly scoped" | GOV-07 authz row (L669) | the GET routes check only the tenant header (handler.go:358-373, 384-462) | ⚠️ | No per-entity authz on the catalogue or on the evaluation ledger, which contains the asserted artifact refs. |
| tenant_id is resolved by GOV-01 and never trusted from an arbitrary header | GCP §16 L1204; §2 invariant #1 | middleware/tenant.go:35-44 trusts `X-Tenant-Id` | ⚠️ | Compose publishes `8130:8130` (L2272), and the Traefik route carries no ForwardAuth (all-services.yml:12-16), so the headers can be spoofed. X5. |
| Canonical service input contract enforced | ZS-ARCH-SVC-001 §4 | main.go:183 | ✅ | |
| Server-resolved context (tenant and entity are operable) | GCP §2 invariants #1-2; §16 | envelope/resolver.go:99-315 exists, but main.go never calls `NewResolver` | ⚠️ | Dead code: the envelope is parsed but never resolved. |
| A missing tenant gets 401 | envelope policy.go:291-305 | the handler returns **400** missing_tenant (handler.go:637-644) | ⚠️ | The same fault gets 400 on reads and 401 on writes; document-vault uses 401. |
| Workload identity accepted (`subject_id / workload_id`) | GCP §16 L1202 | the envelope accepts `X-Workload-Id` (envelope.go:141), but the handler requires `X-Principal-Id` (handler.go:651-658) | ⚠️ | A service caller with only a workload id gets 401. |
| Stable error classes | GCP §16 L1221 | lowercase ad-hoc codes (handler.go:108-692) | ⚠️ | X9. |
| evidence_id returned for a material decision | GCP §16 L1218 | `evaluation_id` in `EvaluateResponse` (domain/types.go:159-165) | ✅ | |

## Data model and ledger integrity

| Item | Documented | Implemented | Status | Notes |
|---|---|---|---|---|
| EvidenceRequirement attributes | 04 §7.1 L946-962 | migration 000001:15-53 | ✅ | Every listed attribute is present, with provenance columns added on top. |
| Nullable `legal_entity_id` on EvidenceRequirement | not in 04 §7.1; required by the doctrine | 000001:25; pg_store.go:258 | ❓ | context §11.1 says it "still wants confirmation". |
| Retirement by effective end-dating; no delete | 04 §7.3; GCP §2 invariant #4 | pg_store.go:290-334 (an atomic guarded UPDATE); no DELETE route | ✅ | 422 when already retired. |
| Effective interval validity | 04 §7.3 | handler.go:520-523, 597-600 | ⚠️ | Accepts `effective_to < effective_from`, a retroactive `effective_to` and a back-dated `effective_from`. A retroactive retirement changes what "was in force" for audit queries. |
| Requirement payload validated when authored | §8.6 sufficiency | handler.go:525-532 stores any JSON, including a non-object | ⚠️ | A malformed rule is accepted with 201 and then blocks every evaluation (evaluate fails closed). It should be refused with 400 at create. |
| Evaluation ledger is append-only / immutable | GCP §1 L42; context §6.2 | the code never updates it; 000001:101-126 has no trigger, REVOKE or rule | ⚠️ | Immutability rests on convention only; the table role can UPDATE and DELETE. |
| Decision is reproducible (governing versions retained) | GCP §2 invariants #9, #11 (L67-71) | handler.go:168-179 stores the unmet list and the asserted artifacts | ⚠️ | The IDs of the *satisfied* requirements, and the catalogue as it stood at `as_of`, are not recorded, so a SATISFIED record cannot prove which rules it was tested against. |
| RLS: FORCE, WITH CHECK, NULLIF on an empty GUC | platform RLS standard | 000002:31-53; 000003:21-38 | ✅ | The empty-GUC trap is fixed. |
| Explicit tenant predicate on every query, plus a non-superuser role | 03 §3 doctrine | pg_store.go:184, 222, 255, 296, 414; compose `DB_USER app_evidence_requirements` (L2283) | ✅ | The comment at pg_store.go:15-22 saying the pool connects as a superuser is stale. |
| An unknown or malformed id gets 404, not 500/503 | platform pattern | pg_store.go:104-113, 192-199, 422-427 | ✅ | Applies to GET requirement and GET evaluation. |
| Malformed ids on write paths | platform pattern | handler.go:107, 134-139, 180-184 (evaluate) and 537-541 (create) | ⚠️ | A non-UUID `legal_entity_id` or tenant causes 22P02 in the INSERT, which is not mapped, so the caller gets **503 store_unavailable** instead of 400 (X10). |

## NFRs and self-documentation

| Item | Documented | Implemented | Status | Notes |
|---|---|---|---|---|
| Request body cap | platform hardening | handler.go:679-696 | ✅ | 413 at 256 KiB. |
| Health, readiness, metrics and tracing | 03 §3.8 | main.go:74-86, 189-192; compose healthcheck on /readyz | ✅ | |
| Bounded authz decision cache | Doc 05 §6.5 | authz/client.go:69, 123-177 | ✅ | 5 s TTL. |
| API endpoints specified in the primary spec | 03 §8.6 lists none | 6 routes exist | ⚠️ | All six routes and the three-valued outcome exist in code but not in the docs; only `context.md §4` describes them. |
| context.md §4 list semantics | context.md §4 | handler.go:389-424 | ⚠️ | The doc says `tenant_id` is required and `as_of` defaults to now. In the code, the header is the scope, `?tenant_id` can only match it, and an omitted `as_of` returns the full history. |
| context and progress claims vs the estate | context §9 / §9.1; pg_store.go:15-22; 000002 comment | — | ⚠️ | context says "zero references in services/" and plans a GL/FC retrofit. In fact 3 other services are wired, and GL and FC are not. The "connects as superuser" comments are stale now that compose uses an app role. |
| Postman collection is usable | postman convention | the collection sends only `X-Tenant-Id` / `X-Principal-Id` | ⚠️ | Every POST gets 400 envelope_incomplete under write-strict, the same known issue as other pre-enforcement collections. |
| Tier classification (Tier 0 or not) | context §11.3; 03 §23 omits it | Tier-0 pool sizing in main.go:89-100 | ❓ | |
| GOV-07 ownership | Control Plane §10, L645-736 | not implemented here | ❓ | GOV-07 is the Evidence *Ledger*: EvidenceObject, Manifest, Package, IntegrityVerification, hashing, sealing and `/internal/v1/gov07/*` commands. This service implements none of it and never claims to. It is the §8.6 *requirements* gate, which has **no GOV-xx number** in the Control Plane spec (C2). |

## What the service does do well

- Tenant isolation is thorough: FORCE RLS with WITH CHECK and NULLIF, an explicit tenant
  predicate on every query, and a non-superuser role.
- Catalogue writes are authorized per action and fail closed, with no caching of failures.
- Sufficiency is data-driven, and a malformed rule fails closed when evaluated.
- Unknown ids on GET return 404.
- Retirement is by guarded end-dating, never by delete.
- The wire event names match §8.6 exactly.
- There is no `go Publish(ctx)` goroutine.
- An evaluation id is returned for every decision.

## Compliance

**16 of 43 scored items fully met — 37%** (partials at half: **58.1%**). 9 are ❌ and 5 are ❓.

**Top gaps by risk**

1. **The gate is unreachable by its callers** (security, function). The three caller clients
   send no envelope headers, write-strict refuses evaluate, and every evidence-gated
   finalization fails with 503 (board-resolutions-svc evidencereq/client.go:87-93;
   internal/envelope/contract.go:12-24).
2. **A SATISFIED result can be replayed across actions** (security, integrity). Evaluations
   dedup on the business `correlation_id` rather than `Idempotency-Key`, and the response has
   no action echo (pg_store.go:351, 364-371; handler.go:187-205).
3. **Evaluate is unauthorized and trusts the body's `legal_entity_id`** (security).
   handler.go:101-131.
4. **Callers check the outcome against a deny-list, so they fail open** (security).
   corporate-actions-svc and filing-preparation-svc internal/evidencereq/client.go.
5. **Document verification is broken against the real document-vault** (function). No
   `X-Principal-Id` means 401, which becomes 503 on every document-backed evaluation
   (documentvault/client.go:326-332).
6. **Headers are trusted while the port is directly exposed, and the resolver is never wired**
   (security). middleware/tenant.go:35-44; main.go has no `NewResolver`.
7. **Four of the seven finalization paths have no gate** (data integrity): GL post,
   financial-close lock, vat-gst and corporate-tax.
8. **A duplicate reference double-counts toward `minimum_count`, and unverified non-document
   artifacts count, even with an empty `reference_id`** (data integrity). handler.go:231-241.
9. **No outbox; the publish runs on the request context; replays never republish** (data
   integrity). handler.go:187-188; publisher.go:309-311, 383-389.
10. **Requirement create silently drops distinct rows that share a correlation_id** (data
    integrity). pg_store.go:138-165.
11. **Ledger integrity** (data integrity): the ledger is not immutable in the DB; SATISFIED
    records omit the requirement set they were tested against; retroactive or inverted
    effective dates are accepted; a malformed payload is accepted at create; non-UUID ids on
    writes return 503.
12. **Doc drift** (cosmetic): the 6 routes are undocumented in the primary spec; error codes
    are not the §16 classes; a missing tenant gets 400 in the handler but 401 from the envelope;
    context.md is stale; the Postman collection is broken by the envelope.

**Needs clarification**

1. **GOV-07 ownership.** GOV-07 (Evidence Ledger & Verification) is not this service's scope;
   the likely owner is evidence-manifest-svc, document-vault-svc or audit-event-store-svc in
   Group 3. Neither the §8.6 requirements gate nor the §16 "storage failure blocks
   evidence-before-commit" dependency (L675, test #13 L1346) is assigned to anyone. Someone
   needs to say which service GOV-07 maps to, and whether the gate should consult it, for
   example to verify manifest refs (C2).
2. Does **NO_REQUIREMENTS_DEFINED** permit or block (context §11.4, still open)? All callers
   currently permit it, and the catalogue is empty by default (C9).
3. Is a nullable **`legal_entity_id`** on EvidenceRequirement accepted (04 §7.1 omits it;
   context §11.1)?
4. Is the service **Tier 0** (context §11.3; 03 §23 omits it)?
5. Which **document statuses** count as valid evidence? No spec section defines this.
6. Is evaluate a **"material write"**? The envelope treats it as one and demands
   Idempotency-Key and a legal-entity header, while the code comments call it a precondition
   query. The answer decides whether to override `MaterialWrite` or fix the callers (C5).
