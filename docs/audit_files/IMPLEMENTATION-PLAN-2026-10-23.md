# Implementation plan: every spec section into our existing services

**Date:** 23 October 2026 · **Owner:** Shashi · **Branch:** `shashi-changes` (backend), `domains-1-2-fe` (frontend)
**Progress file (update after every task):** `docs/audit_files/IMPLEMENTATION-PROGRESS.md`

This plan comes from the 23 Oct cross-check of 15 specification documents against the code. It
lists every gap, where each one is built, and the evidence needed before a task counts as done.
A reviewer re-runs that evidence after each session (see §8). A task that cannot show its
evidence is reopened.

---

## 1. How to work through this plan

1. Do the phases in order. Inside a phase, do the tasks in order unless a task says "after X".
2. Before a task, read its spec sections (§3 tells you how to get the text), the service's code,
   and the latest audit section for that service in `docs/audit_files/`.
3. After a task, fill in its evidence block in the progress file (§7). Set the status to DONE
   only when every "Done when" check passes.
4. If part of a task truly cannot be built here (§2 R14), set BLOCKED and write down what it
   needs. Build everything else in the task anyway.
5. Do not stop to ask about anything already decided in §5. If a new, genuine decision comes up,
   take the option that is safest and closest to the spec, record it under "Decisions taken" in
   the progress file, and carry on.

---

## 2. Ground rules (non-negotiable)

**R1 · Scope: only our services.** You may change:

| Group | Services |
|---|---|
| Identity, Scope & Foundation | identity-context-svc, tenant-entity-registry-svc, configuration-feature-flag-svc, secret-vault-integration-svc, gateway-auth-svc, search-indexer-svc, notification-svc, delegated-authority-svc, access-control-svc |
| Governance Platform | jurisdiction-rules-svc, governance-decision-log-svc, policy-svc, obligations-svc, authorization-svc, workflow-svc, evidence-requirements-svc |
| Governance supporting | schema-registry-svc, retention-registry-svc, `services/identity-envelope`, `services/search-client`, `services/_contract`, `deployments/gtrm` |
| Security & Trust | mtls-management-svc, siem-integration-svc, carta-svc, key-management-svc |
| Platform files | `deployments/` (compose, k8s, init-db, migrate.py, prometheus rules), `.github/workflows/`, `docs/` |

Every other service belongs to a teammate. Examples are currency-registry, fiscal-calendar,
accounting-period, payee-banking-identity, counterparty-management, org-structure,
exception-escalation, obligation-tracking, document-vault, privacy-*, commercial-account,
capability-registry, kill-switch-registry, treasury, general-ledger, evidence-manifest and
external-data-feed. **Do not edit them.** You may *call* their existing APIs. If a task needs a
change inside one, add it to "Cross-team follow-ups" in the progress file and carry on.

**R2 · No new microservices.** Where each spec section goes is decided in §4.

**R3 · Git.**
- Make one commit per task, or per service when a task spans several services.
- Stage files by explicit path. Never use `git add -A` or `git add .`, because other people's
  edits sit in this checkout.
- Never push, and never commit binaries (`*.exe`, `server`, `healthcheck`).
- Write commit messages in the repo's style (`feat(<svc>): …`, with a body saying what was wrong
  and what changed).

**R4 · Migrations.**
- Use the next free number in that service's `deployments/migrations/`. Some services have
  duplicate numbers, so `ls` the folder first.
- Always add a new file; never edit a migration that already exists. Every `.up.sql` needs a
  `.down.sql`.
- Tenant tables need `ENABLE` + `FORCE ROW LEVEL SECURITY`. Policies use
  `NULLIF(current_setting('app.tenant_id', true), '')::uuid`; a bare `::uuid` cast breaks on
  pooled connections.
- Every `GRANT` must be guarded (`DO $$ BEGIN IF EXISTS (SELECT 1 FROM pg_roles WHERE rolname = '…') THEN … END IF; END $$;`)
  or name a role that `deployments/scripts/create-app-roles.sh` creates. An unguarded grant
  aborts fresh-volume init for all 85 databases.
- A service that gets its first database (the Security & Trust four) also needs a database and
  role in `deployments/init-db.sh` and `create-app-roles.sh`, and a compose entry for its DSN.

**R5 · Permissions.** Every action a handler checks must be registered in access-control-svc:
- a new migration (000023 onward) with
  `INSERT INTO permission_definitions (action_name, naming, risk_tier, description)`, following
  the pattern in `000021_register_governance_platform_actions.up.sql`;
- a role template that carries the action;
- risk tier HIGH or CRITICAL for anything destructive or administrative.

An unregistered action can never be granted, so the endpoint answers 403 forever. That is how
`POST /v1/validate-action` broke.

**R6 · Every new or changed route:**
- keeps the identity-envelope gate already mounted in that service;
- checks authorization at the object it acts on (its entity), not at the caller's own entity;
- takes `Idempotency-Key` on mutations (ZS-API-001 §9). The replay key is tenant + principal +
  route template + key → request hash. Same key with a different body is 409;
- requires `If-Match` on mutations of an existing aggregate (428 when missing, 412 when stale)
  and returns `ETag` on reads of it;
- returns RFC 9457 problem details (task API-1);
- writes its events through the service's transactional outbox, in the same DB transaction as
  the state change;
- appears in `openapi.yaml`, which must be validated with a real parser (kin-openapi), and passes
  the service's route-parity test. Add that test where it is missing.

**R7 · Wiring.** New code must be constructed in `cmd/server/main.go` (or code it calls) and
reached by a route, consumer or ticker. Grep the callers. Code with no caller is not done.

**R8 · Background work.** Never run `go f(ctx)` with the request context, because it is cancelled
when the handler returns. Use `context.WithoutCancel` plus a timeout, or the outbox.

**R9 · Event names.** Use the wire names producers actually emit (grep the producers). Spec names
sometimes differ; where they do, emit both during a deprecation window and register both in
schema-registry.

**R10 · Tests.**
- Run against real Postgres (§3). Store and integration suites must **run, not skip**: record
  pass/fail/skip per package.
- Every negative-path or "minimum acceptance" line in a spec section you implement gets a test
  named after it (e.g. `TestORG04_RejectsOwnershipCycle`).
- Never point a test at the dev stack's Postgres on :5432. authorization-svc's store tests drop
  their database.

**R11 · Windows host.**
- Application Control randomly blocks `*.test.exe`. Compile with
  `go test -c -o <name>.exe ./internal/<pkg>` and run the renamed binary from the package folder,
  or run the suite in `docker run --rm -v "<repo>:/src" -w /src/services/<svc> golang:1.25-alpine go test ./...`.
- Do not write files with Python on Windows (it writes CRLF). `*.sh` must stay LF; check with
  `git ls-files --eol`.

**R12 · Docker.** Start only the target service and its `depends_on`
(`docker compose -f deployments/docker-compose.yml up -d <svc>`), never the whole file.
Rebuild with `--build`, because a plain `up` runs weeks-old images.

**R13 · Disk.** C: is almost full. If a build fails for space, run `go clean -cache`. Do not
prune Docker images or volumes.

**R14 · No stubs, no fake answers.** If a capability needs something that does not exist here
(cloud KMS, a SIEM vendor, an external IdP, an address-verification provider, sender DNS), build
the provider interface plus a real local implementation: a local KEK, a syslog/HTTPS sink,
deterministic validation rules. Mark only the production adapter BLOCKED. An endpoint that
invents an answer is worse than no endpoint.

**R15 · Spec deviations.** Where placement or behaviour differs from a spec (e.g. master data
living in tenant-entity-registry-svc instead of MDM-01…05 services), say so in that service's
`SPEC_DEVIATIONS.md`. Create the file if it is missing.

---

## 3. Environment

- **Backend:** `zoiko-suite-backend/` (Go, chi, pgx, kafka-go). Each service is its own module.
  The 16 Group 1/2 services import `zoiko.io/identity-envelope` through `replace ../identity-envelope`,
  so their Dockerfiles build from the repo root.
- **Frontend:** `zoiko-suite-frontend-platform/` (Next.js 16, which differs from older versions;
  read `node_modules/next/dist/docs/` before writing FE code).
- **Scratch test database:**
  ```bash
  docker run -d --name plan-pg -p 55432:5432 -e POSTGRES_PASSWORD=postgres postgres:16-alpine
  # one database per service:
  docker exec plan-pg psql -U postgres -c "CREATE DATABASE tenant_registry_test"
  export TEST_DATABASE_URL="postgres://postgres:postgres@localhost:55432/tenant_registry_test?sslmode=disable"
  ```
  RLS assertions must run as an app role, not the superuser. Copy configuration-feature-flag-svc's
  `appRolePool` test pattern.
- **Dev stack migrations:** `python deployments/migrate.py status`, then `up --db <name>`.
- **Spec text:** the `.docx` files are in `docs/architecture/original_doc/`. Extract them once into
  the git-ignored `scratch/` folder:
  ```bash
  mkdir -p scratch/specs
  for f in docs/architecture/original_doc/*.docx; do b=$(basename "$f" .docx); unzip -p "$f" word/document.xml | sed -e 's#</w:p>#\n#g' -e 's#<w:tab/># #g' -e 's#<[^>]*>##g' -e 's/&amp;/\&/g' -e 's/&lt;/</g' -e 's/&gt;/>/g' -e 's/&quot;/"/g' -e "s/&apos;/'/g" > "scratch/specs/$b.txt"; done
  ```
- **Audit history:** `docs/audit_files/Identity, Scope & Foundation-audit-2026-09-23.md`
  (Group 1), `Governance Platform-audit-2026-09-28.md` (Group 2), `Auth-Service-audit-2026-10-14.md`
  (IAM) and `Security & Trust.md`. The latest state of each service is at the end of each file.

**Spec index**

| ID | File (in `docs/architecture/original_doc/`) | Governs |
|---|---|---|
| ZS-SVC-A-001 | `ZoikoSuite_Governance_Control_Plane_Detailed_Service_Specifications.docx` | GOV-01…12, §16 envelope and error classes, §17 events |
| ZS-SVC-I-001 | `ZoikoSuite_Organization_Legal_Entity_Global_Reference_Data_Detailed_Service_Specifications.docx` | ORG-01…10 (§4.x), REF-01…10 |
| ZS-SVC-U-001 | `ZS-SVC-U-001_Enterprise_Master_Data_…docx` | MDM-01…05 (§9–18), API list near line 640 of the text |
| ZS-SVC-R-001 | `ZS-SVC-R-001_Workflow_Approval_Case_Obligation_Control_…docx` | WFC-01…05 |
| ZS-SVC-V-001 | `ZS-SVC-V-001_Policy_Rules_Decision_Jurisdictional_Applicability_Control_…docx` | PDC-01…05 |
| ZS-SVC-Y-001 | `ZS-SVC-Y-001_Notification_Communication_Delivery_Control_…docx` | NCD-01…05 |
| ZS-SVC-AA-001 | `ZS-SVC-AA-001_Platform_Configuration_Feature_Flag_…docx` | CFG-01…05 |
| ZS-SVC-AB-001 | `ZS-SVC-AB-001_Enterprise_Search_Indexing_…docx` | ESR-01…05 |
| ZS-IAM-001 | `ZoikoSuite_Global_Authorization_RBAC_ABAC_Segregation_of_Duties_Standard_…docx` | §1–32, scenarios A01…A30 |
| ZS-SEC-001 | `ZoikoSuite_Security_Privacy_Residency_Cryptographic_Architecture_Standard_…docx` | §1–30 |
| ZS-JUR-001 | `ZoikoSuite_Global_Jurisdiction_Pack_Regulatory_Rule_Architecture_Standard_…docx` | §1–30 |
| ZS-STATE-001 | `ZoikoSuite_Business_Object_State_Machine_Workflow_Approval_Catalogue.docx` | §1–24 |
| ZS-API-001 | `ZoikoSuite_Global_API_Command_Integration_Contract_Standard_…docx` | §1–30 |
| ZS-COMMS-EMAIL-001 | `ZoikoSuite_Email_Communications_System.docx` | Sections 00–15: 128 templates (§10/§11), 32 variants (§12), backlog (§13) |

---

## 4. Where each missing section is built (decided 23 Oct 2026)

| Spec section | Built in |
|---|---|
| GOV-02 runtime reads | tenant-entity-registry-svc (reads) + `deployments/gtrm` compiler commands (stays a compiler: decision doc §15) |
| GOV-07 Evidence ledger and verification | governance-decision-log-svc |
| GOV-08 Schema and contract registry lifecycle | schema-registry-svc |
| GOV-09 Retention and disposition, GOV-10 Legal hold | retention-registry-svc (legal-hold authority; document-vault becomes a consumer, cross-team) |
| GOV-11 Feature policy and capability exposure | configuration-feature-flag-svc |
| WFC-04 Cases, ZS-STATE §11 state-machine registry | workflow-svc |
| WFC-05 Obligation, deadline and escalation clocks | obligations-svc |
| ORG-01 identity gaps | identity-context-svc |
| ORG-04, ORG-05, ORG-07, ORG-08, ORG-09; MDM-01…05; REF-03, REF-06, REF-08, REF-09; `/v1/reference/{set}/{code}`; master imports | tenant-entity-registry-svc (new internal modules `ownership`, `orgunit`, `party`, `address`, `registration`, `reference`, `imports`) |
| REF-01 jurisdiction registry commands, REF-10 business calendars and payment terms, ZS-JUR `jurisdiction-decisions:resolve` | jurisdiction-rules-svc |
| REF-07 Accounting policy | policy-svc (policy type `ACCOUNTING_POLICY`) |
| ORG-06 remaining operations | authorization-svc (role assignments) |
| Email templates and trigger routing | notification-svc |
| Key custody, BYOK/HYOK, crypto inventory and algorithm registry, effective security policy | key-management-svc |
| SIEM forwarding, security exceptions, incident containment, security evidence | siem-integration-svc |
| Tokenization, masking, pseudonymization | secret-vault-integration-svc |

---

## 5. Decisions already taken (do not re-ask)

| Topic | Decision |
|---|---|
| Scope | Ours plus existing services only; no new microservices; teammates' services untouched |
| Email | All 128 templates + 32 state variants, plus trigger routing; triggers with no producer stay dormant and listed |
| Security | Service code + CI supply chain + backup/DR scripts and runbook |
| WFC-05 (old C1 hold) | In scope now: obligations-svc gets the full deadline-clock model |
| C8 single owner of obligations | obligations-svc is the owner; obligation-tracking-svc overlap is a cross-team follow-up |
| C3 event naming | Keep current wire names; add spec-canonical names alongside where they differ (R9) |
| X9 RFC 9457 | Required everywhere in our services (API-1/API-2) |
| P-09 `expected_version` | Mandatory, as `If-Match`, for every mutation of an existing aggregate (API-3) |
| P-03 policies and jurisdiction | Policies carry `jurisdiction_context` (V-001 PDC-02 applicability) |
| S3-D1 `X-Commercial-Plan`, `X-Org-Unit-Id` | Edge-set server context, stripped from clients; plan from commercial-account-svc |
| S3-D2 config snapshot signing | Sign with a key-management-svc key (local key until SEC-1 lands) |
| S4-D1 dynamic credentials | Broker returns per-lease credentials; Vault engine adapter BLOCKED until infra |
| S5-D2 CARTA unreachable | Fail closed on writes, open on reads (decided 21 Oct) |
| S7-D1 notification operating values | Ratify the values already in code |
| S7-D2 secure links | Signed, expiring capability link; S1+ content also needs a signed-in session |
| S7-D3 legal-hold authority | retention-registry-svc |
| S8-D1 revocation window | 5 s accepted; quantitative authority limits live in authorization-svc `authority_limits` |
| S9-D1 legacy action names | Add `resource.action` aliases in access-control; never rename callers' checks |
| GTRM | Stays a compiler (approved decision doc); runtime commands are compiler commands with provenance |
| Embedding provider (S6-D1), SIEM vendor, cloud KMS, external IdP vendor, address/LEI verification provider, sender DNS | BLOCKED on external choice; build interface + local implementation |
| P-01 SPEND_CONTROL / SIGNATORY_MATRIX / SOD_RULE payload semantics | BLOCKED on business input |

---

## 6. Tasks

Each task gives: **Where** · **Spec** · **Build** · **Done when**. Every task is also bound by §2.

### Phase 0: housekeeping

**P0-1 · Commit the pending authorization work**
- **Where:** authorization-svc (uncommitted 22 Oct changes, migrations 000034–000036) and
  access-control-svc 000022.
- **Build:** nothing new.
- **Done when:**
  - the authz store suite and handler tests pass on the scratch DB, with counts recorded;
  - `go vet` is clean;
  - one commit per service, staging only those services' files.

**P0-2 · Make `validate-action` grantable**
- **Where:** access-control-svc migration 000023.
- **Spec:** Doc 03 §8.2 (J-06).
- **Build:** register `JURISDICTION_RULE_VALIDATE` (STANDARD) and add it to the role template that
  carries the other `JURISDICTION_*` read actions.
- **Done when:**
  - the migration applies on a fresh DB and with `migrate.py up`;
  - a store test shows a principal with that template is granted the action;
  - `grep -rn JURISDICTION_RULE_VALIDATE services/access-control-svc` finds it.

**P0-3 · Stale scope comment**
- **Where:** `jurisdiction-rules-svc/internal/handler/resolve.go` lines 18–24.
- **Build:** the comment says `regulatory-calculations:execute` is not built, but it is
  (`tax.go`). Correct the comment. It will change again in JUR-1.

### Phase 1: API contract (ZS-API-001) in all our services

**API-1 · Shared contract package**
- **Where:** new package `zoiko.io/identity-envelope/apicontract`, in the module all 16 services
  already import.
- **Spec:** ZS-API-001 §6, §9, §10, §12, §15, §18, §26, §29.
- **Build:**
  - **Problem writer** (`application/problem+json`, fields `type`, `title`, `status`, `detail`,
    `instance`, `code`, `request_id`, `correlation_id`, `retryable`, `errors[]` with `pointer`,
    `code`, `detail`, plus optional `current_version`).
  - **Request-id middleware:** `Zoiko-Request-Id` on every response; echo a well-formed client
    value, otherwise generate one.
  - **Default cache header:** `Cache-Control: private, no-store`.
  - **ETag helpers:** an ETag from the aggregate version; `If-Match` parsing that answers 428
    `precondition_required` and 412 `stale_version`.
  - **Cursor helpers:** opaque, HMAC-signed, scoped to tenant + route + filter + sort;
    `page_size` defaults to 50, maximum 200; unknown `filter[...]` answers 400.
  - **`Location` helper.**
  - **Deprecation/Sunset helper:** RFC 9745 and RFC 8594 headers.
  - **Async-operation store:** an interface plus a pgx implementation, with states `queued`,
    `running`, `waiting_external`, `succeeded`, `failed`, `cancel_requested`, `cancelled`.
- **Done when:** the package has unit tests for every helper (including tampered and out-of-scope
  cursors, and 428/412), and `go vet` is clean.

**API-2 · Adopt problem details, request id and cache header in all 16 services and the Security & Trust four**
- **Build:**
  - Every error path returns problem details; the existing error codes become `code`.
  - Mount the request-id and cache middleware.
  - Add a shared `Problem` schema to each `openapi.yaml`, referenced by every 4xx/5xx response.
- **Done when:**
  - each service has a handler test asserting `Content-Type: application/problem+json` and a
    stable `code` for 400, 401, 403, 404, 409, 412, 428 and 503;
  - no handler writes a non-problem error body (grep each service's error writer);
  - every openapi is valid and parity tests pass.

**API-3 · ETag and If-Match on every aggregate mutation**
- **Build:** list the mutable aggregates per service in the progress file first (tenant, legal
  entity, flag, config change, secret policy, template, delegation, role, bundle, rule, pack,
  calendar, policy version, obligation, filing requirement, authority limit, SoD/ABAC rule,
  workflow definition, evidence requirement, schema, hold, rule set, key record, …).
- **Done when:** for each aggregate, GET returns `ETag`, and each mutation has three tests:
  missing `If-Match` → 428, stale → 412, correct → 2xx.

**API-4 · Cursor pagination on every list endpoint**
- **Build:** `filter[...]`, `sort` with `id` tie-break, `page_size`, `next_cursor`. Offset paging
  stays only on small static reference lists, and that must be documented in openapi.
- **Done when:** each list endpoint has tests for its first page, next page, tampered cursor (400)
  and unknown filter (400).

**API-5 · Async operations**
- **Where:**
  - search-indexer: rebuild and reindex;
  - jurisdiction-rules: pack compile and test-runs;
  - retention-registry: disposition execution (G-09);
  - tenant-entity-registry: master imports (ORG-9);
  - key-management: re-encryption (SEC-1).
- **Spec:** ZS-API-001 §18.
- **Build:**
  - `Prefer: respond-async` (or an always-async operation) → 202 + `Location: /v1/operations/{id}`;
  - `GET /v1/operations/{id}`;
  - cancel only before the point of no return;
  - an idempotency retry returns the same operation id.
- **Done when:** each service has tests for 202 + Location, polling to a terminal state, the same
  key giving the same operation id, and cancelling after the point of no return → 409.

**API-6 · Deprecation of renamed routes**
- **Build:** use the API-1 helper wherever a route is renamed (e.g. ST-3).
- **Done when:** the old route returns `Deprecation` and `Sunset` headers, as shown by a test.

### Phase 2: governance control plane (ZS-SVC-A-001)

**G-10 · Legal hold** (do this first: IAM-3 and G-09 depend on it)
- **Where:** retention-registry-svc.
- **Spec:** GOV-10 (§13 of the GOV doc, including its negative paths), ZS-DATA-GOV legal hold.
- **Build:**
  - commands: `IssueLegalHold` (matter, scope by subject ids / record classes / date range /
    custodians), `AmendHoldScope`, `AddCustodian`, `RemoveCustodian`, `ReleaseHold` (maker-checker:
    the issuer cannot release), `ReconcileHoldCoverage`;
  - queries: `EvaluateHoldCoverage`, `GetHold`, `ListHeldRecords`, `GetPropagationStatus` (which
    consuming services have acknowledged the hold);
  - events `legal_hold.issued`, `.amended` and `.released` through the outbox. Check the wire name
    identity-context-svc's existing GOV-10 projection consumes, and keep it working.
- **Done when:**
  - every op is routed, authorized and registered (R5);
  - tests cover each spec negative path, including that release by the issuer is refused and that
    a disposition over a held subject is refused (with G-09);
  - identity-context's projection test consumes the new event.

**G-09 · Retention and disposition**
- **Where:** retention-registry-svc.
- **Spec:** GOV-09.
- **Build:**
  - `PublishRetentionRule`: versioned and effective-dated, keyed by record class + jurisdiction,
    with the period taken from jurisdiction-rules `POST /v1/retention-rules:resolve`;
  - `ScheduleDisposition`;
  - `ApproveDisposition`: maker-checker;
  - `ExecuteDisposition`: an API-5 async operation. Emit `retention.disposition.instructed` to the
    owning service, record its confirmation, and issue a signed disposition certificate;
  - `ReperformDisposition`, `SuspendDisposition`;
  - queries `ResolveRetention`, `GetDispositionEligibility` (always checks G-10 holds),
    `ExplainRetentionDecision`, `ListDueDisposition`.
- **Done when:**
  - eligibility under an active hold is false and execution is refused;
  - the certificate verifies;
  - the approver cannot be the scheduler;
  - all of the above have tests.

**G-07 · Evidence ledger and verification**
- **Where:** governance-decision-log-svc.
- **Spec:** GOV-07.
- **Build:**
  - commands `AppendEvidence`, `SealEvidencePackage`, `VerifyEvidenceIntegrity`,
    `SupersedeEvidenceReference`, `ExportEvidencePackage`;
  - queries `GetEvidence`, `GetEvidenceChain`, `VerifyPackage`, `SearchBySubject` (privileged, with
    purpose required);
  - per-tenant hash chain (`prev_hash`);
  - a sealed package is a manifest of evidence ids + digests + Merkle root, signed (local key until
    SEC-1, then key-management);
  - export is a JSON bundle with a detached signature;
  - append-only triggers on the new tables.
- **Done when:**
  - tampering with one stored row makes `VerifyEvidenceIntegrity` fail (test);
  - a sealed package cannot be changed;
  - `SearchBySubject` without the privileged action → 403;
  - an exported bundle verifies offline (test).

**G-08 · Schema and contract registry lifecycle**
- **Where:** schema-registry-svc.
- **Spec:** GOV-08.
- **Build:**
  - `RegisterDraftSchema` → `RunCompatibilityCheck` (the existing `internal/compat`) →
    `ApproveSchema` (maker-checker) → `PublishSchema` → `DeprecateSchema` → `RetireSchema`;
  - queries `GetActiveSchema`, `CompareSchemas`, `ListConsumers` (with consumer registration),
    `GetCompatibilityReport`;
  - keep the existing `/{eventName}/versions` routes working.
- **Done when:**
  - publishing an incompatible schema without approval is refused;
  - a retired schema cannot be registered against;
  - the existing route tests still pass.

**G-11 · Feature policy and capability exposure**
- **Where:** configuration-feature-flag-svc.
- **Spec:** GOV-11; AA-001 CFG-03.
- **Build:**
  - commands `CreateFeaturePolicy`, `ApproveFeaturePolicy`, `AssignReleaseRing`,
    `EnableTenantFeature`, `DisableTenantFeature`, `EmergencyDisableFeature` (needs a live
    break-glass or emergency session from authorization-svc; closes S3-B1);
  - queries `EvaluateFeaturePolicy`, `ListEffectiveFeatures`, `ExplainFeatureDecision`;
  - a decision = entitlement (commercial-account-svc, read-only) ∧ release ring ∧ tenant override
    ∧ kill state (kill-switch-registry-svc `GET /check`, read-only).
- **Done when:**
  - a feature outside the tenant's entitlement evaluates false with reason `FEATURE_DISABLED`;
  - emergency disable without an emergency session → 403;
  - explain lists each input.

**G-02 · GTRM reads and compiler commands**
- **Where:** tenant-entity-registry-svc (reads), `deployments/gtrm/compiler` (commands).
- **Spec:** GOV-02; `docs/architecture/global-traffic-residency-manager-decision.md`.
- **Build:**
  - reads `ExplainRoutingDecision`, `GetTenantResidencyTopology` and `GetFailoverReadiness`, built
    from the compiled routing map + regions catalog the registry loads, with `ResolveTenantRegion`
    as the existing `ResolveRegion`;
  - compiler subcommands `activate-policy`, `schedule-change`, `failover initiate|complete|abort`
    and `mark-region-unavailable`. Each edits `routing-map.yaml` through the same validation gates,
    stamps provenance and writes an evidence record;
  - no runtime service.
- **Done when:**
  - every compiler gate test passes;
  - failover to a region not allowed by the tenant's residency is rejected;
  - `--check` passes on Windows.

**G-05 / G-03 / G-04 / G-12 · Named-operation parity**
- **Where:** policy-svc, authorization-svc.
- **Build:** for each named operation in GOV-03/04/05/12 with no route of its own:
  - **If a route already does it:** add `operationId` + `x-spec-operation: <Name>` in openapi.
  - **If not:** implement it. Expected ones: `EvaluateStaticConflict`, `EvaluateDynamicConflict`,
    `ExplainSoDDecision`, `ApproveCompensatingControl`, `RevokeCompensatingControl`,
    `EvaluateMakerChecker`, `ListEligibleCheckers`, `GetBreakGlassStatus`.
- **Done when:** every GOV-03/04/05/12 operation maps to a route in openapi, and new ones have
  tests.

### Phase 3: IAM (ZS-IAM-001, `Auth-Service-audit-2026-10-14.md`)

**IAM-1 · AU-12 per-service audience**
- **Build:** the gateway exchanges the session envelope for one whose audience is the target
  service (`idenv.Exchanger` / `ForwardTo` already exist), and each gate expects its own audience.
- **Done when:** an envelope minted for policy-svc is refused by workflow-svc (test + live probe).

**IAM-2 · AU-14 entitlement stage**
- **Build:**
  - authorization pipeline stage 3 asks commercial-account-svc (COM-03, cached) before candidate
    permissions, with deny reason `FEATURE_DISABLED`;
  - identity-context already calls `WithEntitlementResolver` when `COMMERCIAL_ACCOUNT_URL` is
    set (S1-C1). Set it in compose and k8s, and make it required outside `ENV=local`;
  - feed the config-flag plan input from commercial-account-svc (S3-C1).
- **Done when:** scenario A04 has a test and an unentitled feature is denied.

**IAM-3 · AU-15 legal-hold negative control** (after G-10)
- **Build:** authorization consumes hold events into a projection. Destructive actions on a held
  subject → deny `LEGAL_HOLD`.
- **Done when:** scenario A23 has a test.

**IAM-4 · AU-16 relationship and qualification predicates** (after ORG-4, ORG-5)
- **Build:** a PIP for engagement/matter membership and jurisdiction qualification, with attributes
  read from authoritative services (tenant-registry parties and org units, jurisdiction-rules),
  not from the caller.
- **Done when:** scenarios A24 and A25 have tests.

**IAM-5 · AU-18 authoritative FX** (after ORG-8)
- **Build:** authority limits convert with REF-03 rate sets (tenant-entity-registry `reference`)
  as of the decision time, and record the rate id in evidence. Remove every hard-coded rate.
- **Done when:** grep finds no literal rates, and a test pins the conversion to the rate set.

**IAM-6 · AU-19 observability**
- **Build:**
  - permit/deny/step-up counters, cache hit/miss, revocation-lag histogram and privileged-session
    gauges in authorization-svc;
  - MFA, lockout and replay metrics in identity-context;
  - alert rules in `deployments/prometheus-rules.yml`.
- **Done when:** a `/metrics` test shows the series, and promtool (or a YAML parse) validates the
  rules.

**IAM-7 · AU-20 Definition-of-Done artefacts**
- **Build:**
  - authorization-svc gets `asyncapi.yaml`, `RUNBOOK.md`, `RELEASE_CERTIFICATE.md` and
    `scripts/audit.sh`;
  - refresh identity-context's and gateway-auth's certificates and `audit.sh`.
- **Done when:** the files exist, asyncapi parses, and `audit.sh` runs against a live container.

**IAM-8 · AU-21 performance and revocation SLOs**
- **Build:** a load test of `/v1/authorize` (k6 or Go) and a revocation-propagation measurement.
- **Done when:** p50, p95 and p99 and the revocation lag are recorded in the progress file and
  compared with the IAM §29 targets.

**IAM-9 · AU-22 role-profile drift**
- **Build:** identity-context also consumes authorization-svc's role-assignment events (admin API,
  expiry sweeper, HR-exit revocations).
- **Done when:** a test shows a grant made directly in authz reaches the envelope `role_profile`.

**IAM-10 · AU-25 canonical event names**
- **Build:** emit the IAM §23 names (`identity.subject.*`, `identity.session.revoked`, …) alongside
  the current wire names, and register both in schema-registry (G-08).
- **Done when:** producer tests assert both names.

**IAM-11 · AU-26 `/v1/iam/*` aliases**
- **Build:** `/v1/iam/access-assignments` and `/v1/iam/delegations` as aliases of the admin routes.
- **Done when:** parity and alias tests pass.

**IAM-12 · Federation, SCIM and passkey acceptance**
- **Build:**
  - tests with a local mock OIDC IdP, a mock SAML IdP and a SCIM client covering scenarios A20,
    A21 and §17;
  - document how to set `FEDERATION_ENABLED=true` (the default stays false).
- **Done when:** the tests pass, and a live probe with federation enabled is recorded.

**IAM-13 · ORG-06 remaining operations**
- **Build:** `AssignBusinessRole`, `SuspendAssignment`, `ListPrincipalAssignments` and
  `GetAssignmentsAsOf` on authorization-svc role assignments: add a suspend state and an as-of
  query, or map them to existing routes.
- **Done when:** the as-of query reconstructs a past assignment set (test).

### Phase 4: workflow, obligations and state machines

**WF-1 · WFC-01…03: close workflow-svc's open rows**
- **Spec:** R-001 WFC-01/02/03; Governance audit "workflow-svc" sections (14, 19 and 21 Oct).
- **Build:** close the 25 partial rows: quorum races, RLS on six tables, the state model, common
  columns, per-type approve authority, and handler tests for the new paths.
- **Done when:** each row is re-scored in the audit file with evidence.

**WF-2 · WFC-04 cases**
- **Where:** workflow-svc.
- **Spec:** R-001 §6.
- **Build:**
  - case families from §6.2;
  - lifecycle OPEN → TRIAGED → IN_PROGRESS → WAITING_INTERNAL / WAITING_EXTERNAL → RESOLVED →
    CLOSED, plus REOPENED;
  - additive, versioned notes;
  - merge (`canonical_case_id` + aliases) and split (lineage kept);
  - confidentiality / legal-privilege labels enforced by authorization (restricted membership),
    not by the UI;
  - a closure gate: every mandatory child work item satisfied, waived or transferred;
  - links to domain records without mutating them;
  - `case.*` events;
  - obligations escalation can open a case.
- **Done when:** tests cover every §6.3 transition, refused closure with open mandatory work,
  merge/split lineage, and a restricted case being unreadable without membership.

**WF-3 · WFC-05 deadline clocks**
- **Where:** obligations-svc.
- **Spec:** R-001 §7.
- **Build:**
  - versioned obligation definitions (trigger, scope, due-date formula, evidence and satisfaction
    rules);
  - instances from trigger events (`event_id`, `event_time`, source version);
  - clock classes HARD_LEGAL / REGULATORY / CONTRACTUAL / INTERNAL_SLA / ADVISORY;
  - computation evidence: rule version, timezone, `calendar_version_id` from jurisdiction-rules
    calendars, business-day convention, offsets;
  - warning schedule;
  - lifecycle PLANNED, ACTIVE, DUE_SOON, DUE, OVERDUE, FULFILLED, WAIVED, NOT_APPLICABLE,
    SUPERSEDED;
  - satisfaction evidence; waiver and N/A with authority + reason + evidence;
  - supersession when a calendar is corrected (the old due date stays reconstructable);
  - escalation policies (priority, owners, a WF-2 case, a notification);
  - a deterministic DST rule;
  - also close the two ❌ rows: idempotent applicability decisions, and the single owner (C8).
- **Done when:**
  - a scheduler delay cannot suppress OVERDUE (test with a frozen clock);
  - a calendar correction produces a superseding clock and keeps the old one;
  - a waiver without authority → 403;
  - a DST-boundary test passes.

**WF-4 · Group 2 open rows that are inside our services**
- **Build:** after Phase 1 (which closes RFC 9457, Location and pagination rows), close the
  remaining ⚠️ rows in evidence-requirements (18), obligations (22), governance-decision-log (6),
  jurisdiction-rules (4) and policy (2), using each service's section of the Governance audit.
  Rows that need a teammate's service go to cross-team follow-ups.
- **Done when:** each row is re-scored in the audit file with evidence.

**ST-1 · ZS-STATE §11 state-machine definition registry**
- **Where:** workflow-svc.
- **Build:**
  - `StateMachineDefinition` (object_type, semver, states, terminal states, transitions with
    guards and required evidence);
  - immutable once published; migration between versions is an explicit command;
  - routes to publish, get and list definitions.
- **Done when:** a published definition cannot change, and a new version coexists with old
  objects (tests).

**ST-2 · Available actions and transition history on every lifecycle object in our services**
- **Spec:** ZS-STATE §12, §14.
- **Build:**
  - for each lifecycle object (list it in the progress file first):
    `GET …/{id}/available-actions` (server-evaluated for the caller, informative) and
    `GET …/{id}/transitions` (append-only: from/to, command, actor, versions before/after, reason,
    evidence refs, correlation);
  - record `state_machine_version` on each transition row;
  - publish each object's definition in ST-1.
- **Done when:** each object has the two routes and their tests, and the transition tables are
  append-only (trigger test).

**ST-3 · No generic status writes**
- **Build:**
  - replace identity-context `PUT /principals/{id}/status` with named commands
    (`POST /principals/{id}:suspend`, `:reactivate`, `:disable`), keeping the PUT with
    Deprecation/Sunset (API-6);
  - grep the other services for generic status PUT/PATCH and fix them the same way.
- **Done when:** no PUT/PATCH writes a status field in our services, except deprecated aliases.

### Phase 5: organization and reference data (ZS-SVC-I-001, ZS-SVC-U-001)

All in tenant-entity-registry-svc unless stated. Write the `SPEC_DEVIATIONS.md` entry first
(R15): MDM-01…05 implemented as modules of this service.

**ORG-1 · ORG-01 identity**
- **Where:** identity-context-svc.
- **Build:**
  - map `LinkIdentityProvider` and `DisableIdentity` to existing routes;
  - add `MergeDuplicateIdentity` (survivor id, alias, sessions ended, assignments re-pointed via
    events, history kept), `GetSubjectIdentity`, `ListIdentityLinks`, `GetAssuranceHistory`
    (store assurance changes) and `ListAuthenticationEvents` (login/MFA read model).
- **Done when:** every ORG-01 operation is routed and tested, and merge never reuses an id.

**ORG-2 · ORG-03 name parity**
- **Build:** add `operationId`s for `CreateLegalEntity` and `DeactivateLegalEntity` on the existing
  routes.

**ORG-3 · ORG-04 group ownership and control**
- **Build:**
  - `ProposeOwnershipRelationship`, `ApproveOwnershipRelationship` (the proposer cannot approve),
    `SupersedeRelationship`, `EndRelationship`;
  - queries `GetOwnershipGraph`, `GetGroupAsOf`, `ListEntityParents`, `ExplainControlChain`;
  - effective-dated, with cycle and overlap detection and evidence refs;
  - events `OwnershipRelationshipActivated`, `OwnershipRelationshipSuperseded`,
    `ControlRelationshipChanged`.
- **Done when:** the §4.4 minimum acceptance passes, each part with a test: cycle rejected,
  contradictory overlap rejected, group reconstructed as of a date, evidence preserved.

**ORG-4 · ORG-05 org units + MDM-02 org graphs**
- **Build:**
  - `CreateOrgUnit`, `ReparentOrgUnit`, `ChangeManager`, `ActivateOrgUnit`, `DeactivateOrgUnit`;
  - queries `GetOrgUnit`, `ListHierarchy`, `GetHierarchyAsOf`, `FindUnitsByManager`;
  - `GET /v1/org-graphs/{type}?as_of=`, where the hierarchy types (legal ownership, management,
    operational, reporting) are never mixed (MDM §11.2).
- **Done when:** reparenting into a cycle is rejected, and as-of returns the past tree (tests).

**ORG-5 · ORG-07 + MDM-01 + MDM-05 parties and golden record**
- **Build:**
  - `POST /v1/parties`, idempotent on `create_request_id`. It runs an exact-identifier and
    candidate match first, returns `REVIEW_REQUIRED` on ambiguity, and never creates a duplicate;
  - `GET /v1/parties/{id}?as_of=`, `POST /v1/parties/{id}/facts:propose`, party roles add/remove,
    external identifiers link/find, search;
  - `POST /v1/resolution:match`, `POST /v1/resolution/{case}:merge`,
    `POST /v1/resolution/{case}:split` (survivor id, alias/redirect lineage, no id reuse,
    downstream remediation events), `ExplainMergeHistory`;
  - stewardship cases;
  - a party of kind LEGAL_ENTITY links to the ORG-03 `legal_entity_id` (legal entities stay owned
    by ORG-03).
- **Done when:** MDM §10.1 create-flow steps 1–7 and §18.2 merge steps 1–6 each have a test, and
  merged ids still resolve.

**ORG-6 · ORG-08 + MDM-03 addresses and establishments**
- **Build:**
  - `CreateAddress` (structured, with a normalized comparison form; the source value is kept),
    `VerifyAddress` (pluggable verifier: local deterministic rules, external provider BLOCKED),
    `AmendAddress`, `CreateEstablishment`, `ChangeEstablishmentStatus`, `LinkAddressToParty`;
  - queries `GetAddress`, `GetEstablishment`, `ListEntityEstablishments`, `GetEstablishmentsAsOf`;
  - move the existing registered-office route onto this model, keeping the route.
- **Done when:** tests cover each operation, and the registered office still works.

**ORG-7 · ORG-09 + MDM-03 registrations, licences and identifiers**
- **Build:**
  - `CreateRegistration`, `VerifyRegistration`, `RenewRegistration`, `SuspendRegistration`,
    `ExpireRegistration`, `RevokeRegistration`;
  - queries `GetRegistration`, `FindByNumber` (extend the existing by-registry-number),
    `ListExpiringRegistrations`, `GetRegistrationHistory`;
  - `POST /v1/identifiers/{id}:verify`, with LEI alignment (MDM §13.3);
  - when a higher-authority source disagrees, open a conflict case (reuse registry-conflicts);
  - an expiry sweep emits events.
- **Done when:** the §14.1 pipeline steps are tested, and a conflicting higher-authority source
  opens a conflict instead of overwriting.

**ORG-8 · Reference sets (MDM-04, REF-03/06/08/09)**
- **Where:** tenant-entity-registry `reference` module.
- **Build:**
  - `GET /v1/reference/{set}/{code}?as_of=`, with versioned, effective-dated reference releases;
  - **REF-03 FX:** `IngestFXObservation`, `ApproveFXRateSet`, `SupersedeRateObservation`,
    `CloseRateSet`, `GetRateSet`, `ListRatesBySource`, `ExplainDerivedCrossRate`,
    `GetHistoricalRate`. Exact decimals and source-proven; currency codes are validated read-only
    against currency-registry-svc;
  - **REF-06 books:** `CreateAccountingBook`, `ActivateBook`, `AmendBookBasis`, `SuspendPosting`,
    `RetireBook`, `GetAccountingBook`, `ListEntityBooks`, `GetBookAsOf`, `ExplainBookBasis`;
  - **REF-08 dimensions:** `CreateDimensionType`, `CreateDimensionValue`, `ReparentDimensionValue`,
    `ActivateDimensionValue`, `DeactivateDimensionValue`, `MapSourceObject`, `GetDimensionValue`,
    `ResolveDimension`, `GetHierarchyAsOf`, `ListPostingEligibleValues`;
  - **REF-09 units:** `CreateUnit`, `ApproveConversion`, `SupersedeConversion`, `RetireUnit`,
    `GetUnit`, `ConvertQuantity` (exact rational arithmetic), `GetConversionVersion`,
    `ListUnitsByDimension`.
- **Done when:**
  - every listed operation is routed and tested;
  - cross-rate explain shows its source rates;
  - converting with a superseded conversion uses the version effective at the given time.

**ORG-9 · Master imports**
- **Build:** `POST /v1/master-imports` as an API-5 async operation: upload → validate → preview →
  approve → import → reconcile, with per-record errors kept and no partial silent import.
- **Done when:** a bad row blocks approval, reconciliation counts match, and a retry with the same
  key gives the same operation.

**REF-1 · REF-01 jurisdiction registry commands**
- **Where:** jurisdiction-rules-svc.
- **Build:** `ImportJurisdictionUpdate`, `AddInternalJurisdiction`, `AmendJurisdictionAlias`,
  `RetireJurisdiction`, `GetJurisdictionAsOf`, `MapExternalJurisdictionCode`.
- **Done when:** a retired jurisdiction is still readable as of the past and refused for new rules
  (tests).

**REF-2 · REF-07 accounting policy**
- **Where:** policy-svc, policy type `ACCOUNTING_POLICY`.
- **Build:** `DraftAccountingPolicy`, `ApprovePolicyVersion`, `ActivatePolicyVersion`,
  `SupersedePolicyVersion`, `EmergencySuspendPolicy`, `ResolveAccountingPolicy` and
  `ListApplicablePolicies` (by entity, book, jurisdiction and date).
- **Done when:** resolution as of a date returns the version effective then, and emergency suspend
  needs an emergency session (tests).

**REF-3 · REF-10 business calendars and payment terms**
- **Where:** jurisdiction-rules-svc.
- **Build:**
  - extend calendars with `ImportHolidayCalendar`, `IsBusinessDay` and `AddBusinessDays`;
  - payment terms: `ApprovePaymentTerm`, `SupersedePaymentTerm`, `RetirePaymentTerm`,
    `GetPaymentTerm` and `ExplainDueDate` (deterministic, calendar-versioned).
- **Done when:** due dates across holidays and month ends match fixed golden cases.

**JUR-1 · `POST /v1/jurisdiction-decisions:resolve`** (after ORG-5, ORG-6, ORG-7)
- **Where:** jurisdiction-rules-svc.
- **Spec:** ZS-JUR §9, §25.
- **Build:**
  - inputs: legal entity (ORG-03), establishments (ORG-6), registrations (ORG-7), counterparty party
    (ORG-5), plus transaction facts in the request;
  - output: regime and jurisdiction, with an evidence chain;
  - update the `resolve.go` scope note;
  - the external regulatory feed stays BLOCKED (external-data-feed-svc is a teammate's).
- **Done when:** missing facts give a 422 naming the fact, never a guess, and golden cases pass.

**G1-1 · Group 1 register items that are now unblocked**
- **Spec:** Identity audit "07/10 re-audit gaps" B, C and D. Verify each item is still open before
  fixing it.
- **Build:**
  - S7-C1: notification resolves external contacts and cross-tenant relationships from ORG-5
    parties;
  - S2-C1: fiscal-calendar id checked against fiscal-calendar-svc (read-only);
  - S1-B3: ingress-binding refresh from the registry;
  - S3-B3: `secret://` references resolved through the secret-vault broker;
  - S7-B1: privacy-decision-svc PRV-03 call;
  - S7-B2: notice package declared in document-vault `POST /v1/records`;
  - S7-B3: provider secrets leased from secret-vault;
  - S7-C2: legal basis from policy-svc, deadlines from WF-3;
  - S4-C1 and S4-C2: mtls-management URI SAN and CSR signing (with SEC-6);
  - S9-1 and S9-B, if still open.
- **Done when:** each item is re-scored in the Identity audit file with evidence.

### Phase 6: communications (ZS-SVC-Y-001, ZS-COMMS-EMAIL-001)

**EML-1 · All 128 templates + 32 variants**
- **Where:** notification-svc catalog. Follow the `DefaultSeedDefinitions` pattern in
  `internal/ledger/catalog.go`; split it into one file per family A–H.
- **Spec:** Email doc §10 (master register), §11 (content), §12 (variants), Appendix B (footer
  modules, prohibited patterns).
- **Build:** for each template, copy the subject, preheader, body, CTA, reference line and footer
  class exactly; set class (T0/S0/…), sender stream and reply mode from its header; set
  `RequiredVariables` from every `{{…}}`. Write both HTML and plain text, HTML-escaped.
- **Done when:** EML-2 passes with 128/128 and 32/32.

**EML-2 · Register parity test**
- **Build:**
  - extract the §10/§11/§12 register once into `internal/ledger/testdata/email_register.json`
    (id, class, trigger, recipient, reply, variables);
  - test catalog ↔ register both ways;
  - lint every template: no prohibited pattern, all variables declared, plain-text twin present,
    no S2/S3 variables in subjects.
- **Done when:** the test passes, and removing any template makes it fail.

**EML-3 · Trigger routing**
- **Build:**
  - a consumer for the 129 trigger events → intent and template selection (variants by §12
    conditions) → recipient resolution (NCD-02) → send through the NCD plane;
  - map spec trigger names to wire names (R9);
  - list every trigger with no producer in the progress file as dormant, naming the service that
    should produce it.
- **Done when:** each live trigger has an end-to-end test from event to rendered communication
  record, and a duplicate event delivers once.

**EML-4 · Event catalog** (after G-08)
- **Build:** register the 128 parent events in schema-registry with payload schemas, owner and
  consumers (email §13 P0-02).

**EML-5 · Email platform controls**
- **Spec:** sections 05–09.
- **Build:** one test per control:
  - message-class and preference precedence (§05);
  - identity, tenant and rendering contract (§06);
  - link controls: signed, expiring, no PII in the URL (§07);
  - sender, DMARC alignment and reply architecture config (§08);
  - accessibility and localization: alt text, `lang`, dark-mode-safe colours, governed locale
    fallback (§09).
- **Done when:** every control has a test. Implement any control that is missing.

**EML-6 · Email §13 backlog**
- **Build:** go through every P0/P1 item notification owns and either implement it or mark it
  BLOCKED with the external need (sender DNS, warm-up).

**NCD-1 · Notification 110-row matrix**
- **Build:** close the 7 partial and 3 unmet rows wherever their dependency now exists.

### Phase 7: security (ZS-SEC-001)

**SEC-1 · key-management-svc real custody**
- **Spec:** §10, §11, §12, §25, §26.
- **Build:**
  - Postgres persistence (R4: new DB, role, compose DSN);
  - `KeyRecord` with every §11.1 field;
  - a provider interface with a local AES-256-GCM KEK (the KEK itself from secret-vault). The Cloud
    KMS and Vault Transit adapters are BLOCKED on infrastructure;
  - lifecycle: generate, BYOK import (wrapped), activate (purpose, region, tenant scope,
    algorithm), use (encrypt/decrypt/sign APIs, authorized and logged), scheduled and on-demand
    rotation, re-encryption as an API-5 operation, suspend, revoke/destroy with dual control and a
    dependency assessment, archive, compromise;
  - HYOK: an external key reference with an availability probe, failing closed;
  - routes `POST /security/keys/{id}:rotate`, `:suspend`, `:revoke`;
  - §26 events;
  - maker-checker on disable (Security & Trust audit finding).
- **Done when:**
  - a suspended key refuses encrypt but still decrypts;
  - destroy needs a second approver;
  - re-encryption is resumable;
  - an unavailable HYOK key fails closed;
  - all of the above have tests.

**SEC-2 · Crypto agility (§23)**
- **Where:** key-management-svc.
- **Build:**
  - an algorithm registry with states Preferred / Approved / Transitional / Deprecated /
    Prohibited / Migration target;
  - `GET /security/crypto-inventory` covering keys, mtls certificates (mtls-management), envelope
    signing keys (identity-context JWKS), algorithms and libraries;
  - a versioned encryption envelope (algorithm id + key version in the ciphertext header), used by
    key-management and secret-vault.
- **Done when:** a Prohibited algorithm is refused for new keys, and old ciphertext still decrypts.

**SEC-3 · siem-integration-svc**
- **Spec:** §16, §22, §25.
- **Build:**
  - Postgres store + outbox;
  - an ingestion API with schema validation and redaction (the §16 "must not contain" list);
  - forwarders: RFC 5424 syslog over TLS, and HTTPS JSON (HEC- or Elastic-compatible), with retry,
    backoff and a dead-letter queue. Vendor-specific config is BLOCKED;
  - `POST /security/exceptions` (time-bound, approved);
  - `POST /security/incidents/{id}:contain`: runs an approved action set (end sessions via
    identity-context, revoke certificates via mtls-management, suspend keys via key-management);
  - `GET /security/evidence/{correlation_id}`.
- **Done when:**
  - a restart loses no event;
  - a secret in a payload is redacted before storage;
  - forwarding retries until a local syslog sink receives the event;
  - containment needs approval;
  - all of the above have tests.

**SEC-4 · Tokenization (§14)**
- **Where:** secret-vault-integration-svc.
- **Build:**
  - tokenize, detokenize and mask, each with a purpose and authorization check;
  - a token vault encrypted with SEC-1 keys;
  - deterministic tokens only for approved joinable fields;
  - pseudonymization mapping stored separately and restricted by purpose.
- **Done when:** detokenize without a permitted purpose → 403, masking never returns the raw value,
  and an audit event is written for each detokenize.

**SEC-5 · carta-svc**
- **Build:**
  - replace the two placeholder scoring inputs named in `Security & Trust.md` §11 with real
    signals;
  - add Postgres persistence;
  - add maker-checker where the audit asks for it.
- **Done when:** every scoring input comes from a real source (test), and the audit rows are
  re-scored.

**SEC-6 · mtls-management-svc**
- **Build:** a CSR-signing endpoint (the private key never leaves the workload; closes S4-C2), URI
  SAN `spiffe://zoiko/<env>/<service>` (closes S4-C1), Postgres persistence for certificates and
  revocations, and the remaining items in `Security & Trust.md` §14.
- **Done when:** issuance by CSR works end to end, and revocation survives a restart.

**SEC-7 · `GET /security/policies/effective`**
- **Where:** key-management-svc.
- **Build:** returns the effective crypto, residency and security policy for authorized diagnostic
  use (§25).

**SEC-8 · CI supply chain (§19)**
- **Where:** `.github/workflows/ci.yml`.
- **Build:** a govulncheck step per service; an SBOM per image (syft, SPDX JSON) uploaded as an
  artifact; image signing (cosign keyless with GitHub OIDC) plus a verify step.
- **Done when:** the workflow YAML parses, actionlint is clean, and the PR checks stay green
  (ask the user to paste the run if logs are not accessible).

**SEC-9 · Backup and disaster recovery (§18)**
- **Build:**
  - `deployments/scripts/backup.sh`: `pg_dump -Fc` per DB, a sha256 manifest, and encryption
    (age or gpg) with a key separate from app credentials;
  - `deployments/scripts/restore.sh`: restores into a scratch instance and runs verification
    queries;
  - `docs/architecture/runbooks/backup-dr.md`: RPO/RTO per data class, restore authorization, key
    recovery, ransomware steps.
- **Done when:** a recorded drill backs up the dev stack, restores to scratch, and verification
  passes. Paste the output into the progress file.

### Phase 8: frontend (`zoiko-suite-frontend-platform`, branch `domains-1-2-fe`)

**FE-0 · Commit the 23 Oct layout restyle**
- **Build:** stage only these files:
  - `app/admin/page.tsx`, `app/globals.css`;
  - `components/admin/layout/{AdminShell,GlobalSearch,NotificationBell,Sidebar,Topbar,UserMenu}.tsx`;
  - `components/admin/overview/KpiCardGrid.tsx`;
  - `components/admin/shared/PageHeader.tsx`, `components/admin/shared/Breadcrumbs.tsx`;
  - `components/ui/{Badge,Button,Card,StatCard}.tsx`, `components/ui/index.ts`;
  - `lib/constants.ts`.
- Leave the authorization console files (`app/admin/authorization/*`, `app/admin/account/`,
  `components/admin/authorization/kit/`, `lib/api/*`, `lib/authz-inbox.ts`, `lib/directory.ts`,
  `package*.json`) to their owner.
- **Done when:** `npx eslint` on those files is clean. `tsc` errors may remain only in the
  authorization console files.

**FE-1 · Console screens for every new backend capability**
- **Screens:**
  - tenant registry tabs: ownership graph, org units, parties and merges, addresses and
    establishments, registrations, reference data, imports;
  - evidence ledger (decision log);
  - schema lifecycle;
  - retention and legal holds;
  - feature policies;
  - cases;
  - deadline clocks;
  - key management;
  - security operations (SIEM, exceptions, incidents);
  - tokenization admin;
  - email template catalog browser.
- **Rules:**
  - real API calls through the existing `lib/api` clients;
  - plain-English rendering, never raw JSON as the only view;
  - honest empty and error states ("could not be read" is not "none");
  - sidebar labels unchanged except for new entries.
- **Done when:** each screen loads against a live service (screenshot or Playwright evidence).

**FE-2 · Security & Trust panels**
- **Build:** wire the four panels to the real client `lib/api/security-trust.ts`, which nothing
  imports today.

### Phase 9: close-out

**CLOSE-1 · Audit files**
- **Build:** append an "Implementation pass" section to each audit file, re-scoring only the rows
  you touched, and update the `SPEC_DEVIATIONS.md` files.

**CLOSE-2 · Postman collections**
- **Build:** update each touched service's collection for the envelope, `Idempotency-Key` and
  `If-Match`.

**CLOSE-3 · CI-equivalent run**
- **Build:** for every touched service, run `go mod verify`, build, vet and
  `go test -race ./...` in `golang:1.25` against `postgres:16-alpine`.
- **Done when:** every service is green, with counts recorded.

---

## 7. Evidence block (copy into the progress file for every task)

```
### <TASK-ID> — <title>
Status: TODO | IN PROGRESS | DONE | BLOCKED
Commits: <hash> <subject>
Migrations: <svc>/<NNNNNN_name> (up+down), applied fresh: yes/no, applied via migrate.py: yes/no
Permissions registered: access-control <migration> — <ACTION, …>
Routes: <METHOD path> → <handler>  (openapi validated with: <command>, result)
Wiring: <grep -n … cmd/server/main.go output>
Tests: <command> → <pkg>: <pass>/<fail>/<skip>   (store suites must show 0 skip)
Negative-path tests: <TestNames> ↔ <spec § line>
Live probe (if run): <request> → <status>
Spec rows closed: <doc §…>
Blocked / limits: <what, and what it needs>
```

---

## 8. How the reviewer checks your work

After each session the reviewer re-runs the evidence rather than trusting it:

1. Reads `git log` / `git show` for every commit listed, and checks that only allowed services
   (R1) were touched and nothing was staged by accident.
2. Builds and vets every touched service, and runs its suites on a separate scratch Postgres.
   The counts must match the progress file.
3. Greps the callers of every new constructor, handler and consumer from `cmd/server` (R7).
4. Validates every changed `openapi.yaml` with kin-openapi and runs the parity tests.
5. Reads every new migration: numbering, down file, RLS with NULLIF, FORCE RLS, guarded grants.
6. Checks that every new authorization action is registered in access-control (R5).
7. Re-runs the named-operation coverage scan over all 15 specs and compares it per section with
   the 23 Oct baseline. Target: no missing operation except those marked BLOCKED.
8. Probes the target service live (that service plus its dependencies only).
9. Breaks a guard or two on a scratch copy and expects a test to fail. A guard no test catches
   reopens the task.
10. Reopens any task whose evidence is missing, does not reproduce, or describes code with no
    caller.
