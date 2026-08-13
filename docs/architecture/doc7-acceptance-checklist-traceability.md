# doc7 §27 Mandatory Production Acceptance Checklist — Traceability Matrix

Maps every criterion in `docs/original_doc/zoiko_suite_doc7.txt` §27 to
what currently exists in the codebase, as of 2026-08-13 (through Chunk 11
of `docs/architecture/doc7-implementation-backlog.md`).

**What this document is**: an engineering evidence trail — for each
criterion, what's built, what's tested, what's live-verified, and what
is still missing. It is the input the named function owners (Product,
Engineering, Finance, Security, Privacy, Legal/Compliance, Data
Governance, AI/ML, QA) need to actually perform the sign-off doc7 §35
requires.

**What this document is NOT**: a sign-off. Engineering readiness and
governance/business/legal approval are different questions — several
criteria below are "mechanism ready" while the actual approval decision
(a plan catalog, a legal claim, a security review) is explicitly a human
act this document cannot perform or substitute for. No criterion below
is marked PASS; only READY FOR REVIEW, PARTIAL, or NOT STARTED — the
signature itself belongs to Chunk 12 items 41-42, done by the actual
function owners, not by this matrix.

**Caveat on criterion IDs**: doc7's source PDF renders §27 as a two-column
table; the plain-text extraction separated the ID column from the
criterion-text column, so the mapping below is reconstructed by
preserved reading order (each ID and each paragraph appear in the same
top-to-bottom sequence in the raw extraction). This is very likely
correct but has not been cross-checked against the original PDF layout —
worth a quick visual confirmation against the source document before
this matrix is treated as authoritative for sign-off purposes.

| ID | Criterion | Status | Evidence |
|---|---|---|---|
| COM-01 | Final pricing/plan/catalog/limits/add-ons/trial posture are approved and one catalog version drives web, app, Zoiko One, processor, contracts, support and renewal. | **Mechanism ready, approval pending** | `price_catalogs`/`plans`/`entitlement_limits`/`evaluation_programs` exist in commercial-account-svc (Chunk 6) with one-active-version-at-a-time semantics. No actual catalog has been approved yet — doc7 §33 itself notes "final plan names/prices/limits are not established in this standard." This is a Product/Finance decision, not an engineering gap. |
| COM-02 | Every workspace has billing_classification and billing_source; non-commercial classes cannot create live Zoiko charges. | **Done** | `workspaces.billing_classification`/`billing_source` are NOT NULL columns in tenant-entity-registry-svc (Chunk 5); `ValidBillingClassifications` enforces the doc7 §T value set. |
| COM-03 | Standalone and Zoiko One entitlements cannot double charge the same scope/period. | **Done, live-verified** | Structural: a partial unique index enforces one non-terminal `commercial_subscriptions` row per `commercial_account_id` (Chunk 6); `billing_source_transfers` atomically cancel-old/create-new (Chunk 8). Live-verified a second concurrent subscription attempt 409s even after a transfer. |
| PROD-01 | Capability, entitlement, market, release, integration, execution and public-claim states are separately modeled and tested. | **Done, live-verified** | capability-registry-svc (Chunk 7) models capability/market-release/integration-capability/release/claim as five separate tables per doc7 §7's own warning against collapsing them; commercial-account-svc models entitlement separately (Chunk 6); execution/authority state lives in ai-governance-svc's `automation_actions` (Chunk 9) and authorization-svc. Live-verified all 4 capability-resolution reason-code paths plus the two commercial/execution planes never being called from within capability-registry-svc itself. |
| DATA-01 | System-of-record/field authority matrix is approved for every production integration; source conflict paths exist. | **Not started** | `source_authority_map`/`normalized_fact` (backlog item 36) explicitly deferred — no per-integration field-authority matrix or conflict-resolution path exists yet anywhere in the codebase. |
| GOV-01 | Obligation/control/evidence/status semantics are versioned, scoped and reproducible; no generic "compliant" state exists without defined evaluation. | **Done, live-verified** | obligations-svc's `obligation_status` state machine + append-only `applicability_decisions` (Chunk 10, versioned, scoped by jurisdiction/entity, UNASSESSED never conflated with NOT_APPLICABLE); policy-svc's `policy_versions`. No table or field anywhere in the codebase stores a bare "compliant"/"non-compliant" flag. |
| GOV-02 | Control design, operating effectiveness, evidence freshness, exceptions and attestations are distinct. | **Done, live-verified** | policy-svc's `control_test_definitions` (design status) vs. `control_test_executions` (operating effectiveness, with `exceptions_noted`) vs. `attestations` (signed assertions, separately revocable) — three independent tables (Chunk 10). Live-verified a control showing TESTED design status with an independently INEFFECTIVE latest execution result. |
| AI-01 | AI risk classes, source/evidence rules, review triggers, tool allowlist and autonomous-action policies are approved and evaluated. | **Mechanism ready, content pending** | ai-governance-svc's `action_risk_classifications` and `automation_policies` (allowlist per tenant/role/risk-class/tool) exist and are enforced fail-closed (Chunk 9, live-verified an unallowlisted action 403s). No actual production risk-class taxonomy or allowlist entries have been populated/approved yet — that population is a QA/AI-ML function-owner action, not an engineering gap. |
| AI-02 | High-impact actions require deterministic preconditions, authority, idempotency, required approval, postcondition verification and audit. | **Partial** | `automation_actions` (Chunk 9) has an idempotency key, maker-checker approval (self-approval blocked, live-verified), and an audit trail. Precondition/postcondition verification is recorded as caller-supplied data on the action record but is NOT independently verified by ai-governance-svc itself — by design, this service is "record-keeping/gate-checking... never executes models or automations itself," so actual postcondition proof is the calling service's responsibility and has not been end-to-end verified against a real automation caller yet. |
| AI-03 | Provider/model/prompt/tool changes pass evaluation and production authorization; tenant data is not used for training by default. | **Done for training-default; partial for full evaluation pipeline** | `model_provider_registrations` defaults to NO_TRAINING (Chunk 9, live-verified an unapproved data class and an unregistered provider both blocked). `policy_change_approvals` gates provider/model changes with maker-checker. A full "evaluation" pipeline (e.g. automated eval-suite results feeding the approval) is not modeled — only the approval gate itself. |
| SEC-01 | Tenant isolation, MFA/step-up, privileged access, break-glass, secrets, SSO/SCIM where released and access review pass Security review. | **Pre-existing, out of scope for this backlog** | Owned by authorization-svc/identity-context-svc, built before Chunks 5-11 of this backlog. `tenant_isolation_test.go` exists in several services. A formal Security-function review of this criterion has not been run as part of this backlog effort — that review belongs to the Security function owner directly, not to further doc7-backlog engineering. |
| PRIV-01 | Purpose, data class, retention, legal hold, residency/transfer, DSR and provider processing rules are implemented for launch markets. | **Not started** | `retention_policy`/`legal_hold` objects (backlog item 33) explicitly deferred — don't exist anywhere in the codebase. Cannot honestly be marked ready. |
| INT-01 | Imports, webhooks and material mutations are idempotent; connection failures do not silently alter governance truth. | **Partial** | Idempotency is a consistently-applied pattern in every service built or extended in this backlog (caller-supplied idempotency keys with `ON CONFLICT DO NOTHING`, e.g. `commercial_usage_meter_events.usage_event_id`, `automation_actions.idempotency_key`), and one real idempotency-type bug was caught and fixed during live verification (Chunk 6). Not independently audited across the full ~80-90 service fleet, most of which predates this backlog. |
| INT-02 | No product or connector writes directly into another product's core tables; all shared behavior uses approved contracts/events/APIs. | **Assumed true by convention, not independently audited** | Every service built in this backlog owns its own database and exposes only HTTP APIs + Kafka events to the outside — no cross-service SQL. This is architectural convention across the whole codebase, not something newly verified end-to-end for the ~80 pre-existing services in this pass. |
| AUD-01 | Material state changes emit durable immutable/replayable audit events; integrity and historical replay tests pass. | **Partial** | Append-only audit trails are a consistent pattern (`releases`, `subscription_status_events`, `kill_switch_events`, `control_test_executions`) and audit-event-store-svc exists pre-existing. The specific `replay_manifest` mechanism for replaying historical decisions against point-in-time policy/source versions (backlog item 34) is explicitly Not Started — so "replay tests pass" cannot be claimed yet. |
| EVID-01 | Evidence provenance, hashing/version, source rights, access class, retention and review state are enforced. | **Pre-existing, not verified by this backlog effort** | `evidence_manifest` and `document_vault` databases/services predate this backlog's Chunks 5-11. Not re-verified as part of this pass. |
| REP-01 | Executive metrics are defined, versioned, source-traceable and labeled so operational intelligence is not misrepresented as financial/legal assurance. | **Not started** | `report_metric_definition` (backlog item 35) explicitly deferred — no versioned executive-metric object exists yet. |
| QA-01 | Staging/sandbox uses synthetic or approved de-identified data and non-production provider credentials/endpoints. | **Out of scope for this backlog** | An environment/ops-configuration concern, not a data-model or API gap this backlog's chunks address. |
| QA-02 | Negative tests cover cross-tenant access, stale sources, conflicting facts, rule changes, duplicate events, replay storms, missing evidence, AI uncertainty, approval bypass and double charging. | **Partial** | Individually covered by specific tests built in this backlog: cross-tenant isolation (`tenant_isolation_test.go`), duplicate events (usage-event/idempotency-key dedup tests), approval bypass (self-approval-blocking tests in ai-governance-svc), double charging (Chunk 8's transfer/subscription tests). Not assembled as one deliberate negative-test suite spanning every category listed, and "replay storms"/"AI uncertainty" specifically have no dedicated test today. |
| OPS-01 | Backups, restore, regional failover where applicable, observability, incident response, provider disablement and action kill switches are tested. | **Partial** | Action kill switches now exist and are live-verified end-to-end (kill-switch-registry-svc, Chunk 11) — a direct, concrete close of part of this criterion. Backups/restore/regional-failover/observability signal-wiring (backlog item 39) remain Not Started; those are infrastructure/ops concerns and cross-cutting instrumentation work respectively, outside a single chunk's scope. |
| LEGAL-01 | Terms, privacy/DPA, market claims, professional boundaries, compliance wording, data transfers and public capability claims are approved. | **Mechanism ready, approval pending** | `capability_claims` (Chunk 7) is the structural gate for public claims — wording_owner + approver required fields, never auto-generated from roadmap state. No actual claims have been submitted/approved through it yet; that's a Legal/Compliance function-owner action. |
| A11Y-01 | User-facing launch surfaces meet the approved accessibility gate and do not introduce security/privacy bypasses. | **Not started** | No frontend/UX work exists in this backend-only backlog. Out of scope for Chunks 5-11; needs its own frontend accessibility pass. |
| GO-01 | Product, Commercial, Engineering, Finance, Security, Privacy, Legal/Compliance, Data Governance, AI/ML and QA sign the go-live record. | **Not started — this is Chunk 12 itself** | The actual signature step (backlog items 41-42) — cannot be performed by this document. This matrix is the input those ten function owners need to make that decision. |

## Summary

- **Done / live-verified**: COM-02, COM-03, PROD-01, GOV-01, GOV-02 (5)
- **Mechanism ready, human approval/content pending**: COM-01, AI-01, LEGAL-01 (3)
- **Partial**: AI-02, AI-03, INT-01, AUD-01, QA-02, OPS-01 (6)
- **Pre-existing / not independently re-verified by this backlog**: SEC-01, INT-02, EVID-01 (3)
- **Not started**: DATA-01, PRIV-01, REP-01, A11Y-01 (4)
- **Out of scope for this (backend) backlog**: QA-01 (1)
- **The sign-off itself**: GO-01 (1)

23 criteria total. Of the 22 that are engineering-addressable, roughly a
third (COM-02, COM-03, PROD-01, GOV-01, GOV-02) are genuinely done and
live-verified. The largest remaining gaps are all previously identified
in `doc7-implementation-backlog.md` as deferred cross-cutting work
(DATA-01/PRIV-01/REP-01 map directly to backlog items 36/33/35) or
explicitly out of this backlog's backend scope (A11Y-01, QA-01, SEC-01
review, EVID-01 re-verification).
