# Full Architecture Gap Analysis — Original Spec vs. Built System

This is a consolidated record of every gap found between the original client-provided
architecture spec (`docs/original_doc/zoiko_suite_doc1.txt` through `doc6.txt`, the
6-document architecture series, plus the separate `doc7.txt` commercial/governance
operating standard) and the actual running codebase, as of 2026-08-17 (updated
2026-08-18 with the CARTA/SIEM/mTLS wiring pass and the Secret Vault investigation).

It supersedes nothing — `known-gaps.md` and `doc7-implementation-backlog.md` remain the
source of truth for the items they already track. This file adds what a full read of
Docs 02–06 (Diagram Pack, Microservices Spec, Data Model/ERD Pack, Security
Architecture, Engineering Build Blueprint) surfaced that those two files did not yet
cover, and consolidates everything into one place for prioritization.

Doc numbering reference (the 6-series; `doc7.txt` is separate and already fully covered
by `doc7-implementation-backlog.md`):
- Doc 01 — Sovereign Back-End Architecture (`zoiko_suite_doc1.txt`)
- Doc 02 — System Architecture Diagram Pack (`zoiko_suite_doc6.txt`)
- Doc 03 — Microservices Specification Pack (`zoiko_suite_doc4.txt`)
- Doc 04 — Data Model / ERD Pack (`zoiko_suite_doc3.txt`)
- Doc 05 — Security Architecture Specification (`zoiko_suite_doc5.txt`)
- Doc 06 — Engineering Build Blueprint (`zoiko_suite_doc2.txt`)

## Status legend
- **Fixed** — resolved and merged (this session, PR #108 unless noted)
- **Open** — confirmed gap, not yet started
- **Next up** — confirmed gap, explicitly queued for the next work session
- **Blocked** — not a code problem; needs a real business/legal/operational decision or real production data the spec itself says not to invent

---

## Already fixed (PR #108, 2026-08-17)

| # | Issue | Spec ref | Fix |
|---|---|---|---|
| F1 | Every service connected to Postgres as the superuser, unconditionally bypassing Row-Level Security on 55 services' tenant-isolation policies | Doc 01 §11.2, §17.1, §18.3 | New least-privilege `zoiko_app` runtime role across all 63+ databases (main + phase5/6/7 stacks); migrations still run as superuser, runtime traffic does not. Live-verified against a running instance: cross-tenant read blocked after the fix, was not before. |
| F2 | No circuit breakers on any of the 7 services that had retry-with-backoff but no breaker state | Doc 03 §17.7 | Added open/half-open/closed breaker state to the existing `retry_transport.go` pattern |
| F3 | No dead-letter routing on the 2 Kafka consumers that had TODO'd it | Doc 03 §17.7 | DLQ topic production wired in for both |
| F4 | procurement-workflow-svc could leave a real, externally-created purchase order orphaned (no local record) on a transient local DB write failure after the remote call succeeded | Doc 03 §17.8 (saga discipline, applied to this one flow) | Added compensating cancel call to purchase-order-svc on local-write failure |
| F5 | accounts-payable-svc's payment-request endpoint was only accidentally non-duplicating (via a status-machine side effect), not genuinely idempotent | Doc 03 §3.7 | Added a real client-facing idempotency key, DB-enforced |

## Already fixed (2026-08-18, uncommitted PR)

| # | Issue | Spec ref | Fix |
|---|---|---|---|
| F6 | `carta-svc` (continuous session-risk scoring) was fully built but nothing in the request path called it | Doc 05 §3.11 | Wired into gateway-auth-svc's per-request `Verify` handler; blocks on ISOLATE/DENY, logs+streams STEP_UP_MFA (not yet blocking — no step-up challenge flow exists downstream to redirect to). Tested for all 4 decision outcomes. |
| F7 | `siem-integration-svc` (security-event pipeline) was fully built but had zero producers anywhere | Doc 05 §13.2 | Wired into 5 services (gateway-auth-svc, authorization-svc, key-management-svc, mtls-management-svc, identity-context-svc) on real, already-existing security-relevant code paths (auth failures, CARTA flags, authorization denials, key rotation/disable, cert issuance/rotation/revocation, MFA/trust-posture events). Fire-and-forget, tenant-opt-in via exporter config, never gates the primary operation. |
| F8 | mTLS piloted on only 2 of ~70 inter-service authz callers | Doc 05 §10.7/§76 | Extended the same opt-in, off-by-default client capability to the remaining 68 callers (67 `CheckAllowed`-shaped + jurisdiction-rules-svc's `Authorize`-shaped client). `AuthzMTLSEnabled` defaults false everywhere — this makes the capability available platform-wide, it does not turn it on. Verified with a full build+vet+test sweep, zero regressions. |

## Investigated, not fixed (2026-08-18)

| # | Issue | Finding |
|---|---|---|
| I1 | `secret-vault-integration-svc` wired into only 3 of 87 services | Found and fixed a real pre-existing bug (a migration never wired into `init-db.sh`). Then found a genuine design blocker: the vault's broker never returns raw secret material to a caller, so no service can actually bootstrap a runtime credential through it yet — attempted a live integration on general-ledger-svc, confirmed it doesn't work, reverted rather than ship a fake wiring. This needs a vault-side API change before any service can be genuinely wired to it. See `known-gaps.md` for the full writeup. |

---

## Event & resilience patterns (Doc 03 §17, §19)

| # | Issue | Status |
|---|---|---|
| 1 | ~~No shared/enforced event envelope — every service hand-copies its own struct; `event_version` and jurisdiction context are missing from almost every event; `tenant_id`/`legal_entity_id`/`actor_id` present only when that call site happened to include them~~ | **Partially resolved (2026-08-19)** — no shared Go module exists to enforce this structurally, so each service still hand-copies its own envelope; what changed is that all 6 of Doc 03 §3.7's explicitly-named mandatory cases now have a real, verified, correctly-populated envelope: journal posting (general-ledger-svc, bdd5a58), payment initiation (accounts-payable-svc, f7f4dde), payroll execution (payroll-run-svc, ba4efc2), approval action handling (workflow-svc, 1413f10), contract execution (contract-lifecycle-svc, 6f82b34), filing submission (filing-tracker-svc, 2d38c41). Each required real per-event actor attribution sourced from that service's own domain model or already-verified request principal — not a mechanical copy-paste, since a wrong actor on a financial/legal event is worse than a missing one (Doc 01 §2.10). Two genuine, unrelated bugs were found and fixed along the way: contract-lifecycle-svc and filing-tracker-svc both had a deterministic `event_id` (`"evt-"+eventType+"-"+id`) that collides across every repeat event on the same aggregate — any dedup consumer using `ON CONFLICT (event_id) DO NOTHING` would have silently dropped the second real event. The remaining ~64 services still need the same treatment; this establishes the verified pattern to apply. |
| 2 | No transactional outbox platform-wide — only `commercial-account-svc` has it (pilot); every other service can silently drop an event on a crash between DB commit and Kafka publish | Open |
| 3 | No general-purpose saga/compensating-transaction mechanism — F4 above fixed one concrete instance, but there's no reusable coordinator for the next multi-service flow that needs it (e.g. Hire-to-Pay) | Open |
| 4 | ~~Idempotency inconsistent — DB-enforced in ~18 services, handler-only or entirely absent in ~15 others~~ | **Mostly resolved (2026-08-18)** — re-verified the ~15 services the earlier audit flagged: all but one already had real `ON CONFLICT`-based DB-level idempotency (the audit's grep missed it because it looked for a migration literally named `*idempotency*` rather than the SQL pattern itself). The one genuine gap: `workflow-svc`'s `CreateWorkflow` had a `correlation_id` column with no uniqueness constraint at all — a retried `POST /v1/workflows` created a full duplicate workflow instance, stage chain, and initial transition. Fixed with a partial unique index (migration `000003`) + `ON CONFLICT ... DO NOTHING` + fallback lookup, mirroring the pattern used everywhere else in this codebase. Live-verified against real Postgres: a retried call now returns the original instance/stages with `created=false` and exactly one row exists, not two. `identity-context-svc` was the other service with no `ON CONFLICT` hits, but minting a duplicate identity token isn't a correctness bug in the same sense doc §3.7's mandatory list (payment/payroll/journal/filing/contract/approval) targets, so it's correctly excluded. |

## Governance plane completeness (Doc 01 §07, Doc 03)

| # | Issue | Status |
|---|---|---|
| 5 | No single enforced governance pipeline exists — each domain service calls whichever governance engines it individually decided it needs, not a universal ordered sequence. Only authorization-svc is consulted nearly everywhere (69 of ~70 services) | Open |
| 6 | jurisdiction-rules-svc has no compliance calendar entity, despite it being a named "Owns" item and `jurisdiction.calendar.changed` being a declared (unemittable) event | Open |
| 7 | authorization-svc has no concept of a platform-scoped, non-tenant, non-entity resource — services with platform-wide reference data fake a synthetic `legal_entity_id` | Open — spec silence, not a codified violation |
| 8 | ~~authorization-svc: re-POSTing an existing role reports a 503 instead of 200/409~~ | **Resolved (pre-existing)** — `internal/store/pg_store.go`'s `CreateRole` already does `INSERT ... ON CONFLICT DO NOTHING` + a fallback lookup, returning 200 for an idempotent re-create and 409 only on a genuine name/scope conflict (`TestPgStore_CreateRole_IdempotencyAnd409` covers exactly this). The `known-gaps.md` entry predates this fix; this line is stale. |
| 9 | A DENIED governance decision mostly just returns a 403 — automatic conversion into an approval workflow only exists where workflow-svc integration was explicitly built | Open |

## Data model (Doc 04)

| # | Issue | Status |
|---|---|---|
| 10 | Missing entities with no equivalent anywhere: `UltimateBeneficialOwner`, `FiscalCalendar` (dangling FK column exists, no table), `TaxLogicSnapshot` (dangling FK in 2 services), `GrossToNetCalculationLog`, `NexusRecord`, `SchemaDependencyMap` + `compatibility_mode` | Open |
| 11 | No chart-of-accounts (`Account`) entity anywhere — general-ledger-svc's own migration comment admits this | Open |
| 12 | Obligation tracking duplicated across 3 services (`obligations-svc`, `obligation-tracking-svc`, `filing-tracker-svc`) with non-identical schemas — violates the spec's own "single authoritative owning service" doctrine (§2.1), not just a missing feature | Open |
| 13 | Identity/role assignment duplicated across `authorization-svc` and `identity-context-svc` | Open |
| 14 | No standalone `VendorProfile` entity — only scattered FK-shaped columns | Open |
| 15 | Document Vault missing `virus_scan_status` and `digital_signature_id` (§15.5 requires both) | Open |
| 16 | No field-level encryption or classification tagging on tax ID / bank reference / payroll columns anywhere outside `document-vault-svc` | Open |

## Security (Doc 05) — real capability exists, mostly unwired

| # | Issue | Status |
|---|---|---|
| 20 | `secret-vault-integration-svc`'s broker never returns raw secret material to a caller — no service can be genuinely wired to fetch a runtime credential through it until that API gap is closed. See "Investigated, not fixed" above. | Open — needs a vault-side design change, not just more wiring |
| 21 | `key-management-svc` models BYOK/HYOK but is metadata CRUD only — never actually used to encrypt/decrypt anything | Open |
| 22 | No confidential computing/TEE anywhere (spec calls for it on payroll/tax calculation logic) | Open |
| 23 | No PAM/break-glass/just-in-time elevation anywhere | Open |

## Engineering process (Doc 06)

| # | Issue | Status |
|---|---|---|
| 24 | Only 1 of the mandated 6 environments (local dev) is real — no staging, QA, integration, or shared-dev config exists anywhere | Open |
| 25 | CI has no security scanning, artifact signing, schema-compatibility gate, policy check, or deployment-approval rules | Open |
| 26 | Blue-green/canary deployment documented as a preference only — real k8s manifests are plain rolling-update | Open |
| 27 | No release evidence (manifest/approver/rollback reference), contract tests, load/performance tests, DR/restore tests, or enforced coverage threshold anywhere | Open |
| 28 | Infrastructure-as-Code (Terraform + k8s manifests) is real but covers only ~30 of 87 services — docker-compose remains how most of the estate actually runs | Open |

## Blocked — not code problems

| # | Issue | Why blocked |
|---|---|---|
| 29 | Numeric SLOs (availability/latency/RTO/RPO) — Doc 01 §15.2, Doc 03 §20.4 explicitly require these to be defined | Needs real measured production capacity and business-criticality sign-off, not invented |
| 30 | Merchant/tax/processor/invoice identity setup for ZoikoSuite's own billing | Needs a real business/legal decision (merchant-of-record agreement, tax registration, processor account) |
| 31 | Doc 7 §27 acceptance sign-off by named function owners (Product/Engineering/Finance/Security/Privacy/Legal/AI-ML/QA) | Process gate — cannot be closed by an engineering session; traceability matrix already built (`doc7-acceptance-checklist-traceability.md`) |
| 32 | Safe-degraded-mode behavior definition per service (Doc 03 §3.10/§22, Doc07 §32.2) | Needs its own per-service behavior audit across ~90 services, not a shared table/service |
| 33 | Doc07 §32's observability signal families wired into OTel/Prometheus | Needs its own cross-cutting instrumentation pass across every service |
| 34 | `source-authority-svc` real precedence data for actual connected systems (payroll, HR, billing connectors) | Operational knowledge this pass cannot invent — same doctrine as item 30 |

---

## Sources
- `docs/original_doc/zoiko_suite_doc1.txt` (Doc 01), `doc6.txt` (Doc 02), `doc4.txt` (Doc 03), `doc3.txt` (Doc 04), `doc5.txt` (Doc 05), `doc2.txt` (Doc 06), `doc7.txt` (separate operating standard)
- `docs/architecture/known-gaps.md` — prior estate-wide findings, cross-referenced not duplicated
- `docs/architecture/doc7-implementation-backlog.md` — Doc7-specific backlog, cross-referenced not duplicated
- `docs/architecture/doc7-acceptance-checklist-traceability.md` — §27 sign-off evidence trail
