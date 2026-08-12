# ZSU-COM-OPS-001 (doc7) Implementation Backlog

Tracks the gap between `docs/original_doc/zoiko_suite_doc7.txt` (Commercial
Operations, Subscription & Governed Platform Standard, v1.0) and the current
backend. Source: a full canonical-object audit against the spec's §28
Minimum Engineering Data Model, run 2026-08-11.

**Status at audit time**: the governance/evidence plane (obligations,
policies, workflow, evidence, audit) is meaningfully built, though several
objects exist under different names/shapes than the spec defines. The
commercial/entitlement plane (Plane 1) and the AI/automation governance
plane (Plane 5) are entirely unimplemented — no services, tables, or types
exist for organization-vs-tenant separation, billing, pricing, entitlements,
capability/market-release registries, or AI governance. This is expected,
not a defect: doc7 itself is dated 2026-08-07, and every function's sign-off
in its Controlled Sign-Off Record (§35) is still "Pending."

Status values: `Not Started` / `In Progress` / `Blocked` / `Done`. Update in
place as work lands; link the PR in the Notes column.

---

## Chunk 5 — Commercial Account & Organization Model (net-new, Plane 1) — ✅ Done 2026-08-12

Resolution: rather than inventing a competing top-level object, `organization`
is satisfied by tenant-entity-registry-svc's existing `tenants` (= this
platform's `tenant_id`) — a platform-wide rename was rejected as
disproportionate (it would ripple across all ~80 services for no real
gain). `commercial_account` and `membership` are genuinely new concepts with
no prior owner, so they got a new service, per doc7 §3's own mandatory
Plane 1/Plane 2 separation. `workspace` is structurally a sibling of
`legal_entity`, so it extends tenant-entity-registry-svc rather than
duplicating it.

| # | Item | Spec ref | Status | Notes |
|---|---|---|---|---|
| 1 | `organization` as the top-level tenant object, distinct from today's `tenant_id` — persist `organization_id`, `commercial_account_id`, `workspace_id`, `legal_entity_id` separately | §A1 | Done | Satisfied by existing `tenants` table (tenant-entity-registry-svc); `commercial_account_id`/`workspace_id` now separately persisted per items 2–3 below |
| 2 | `commercial_account` object — legal customer name, billing/tax facts, contract/order refs, processor customer refs | §A4 | Done | New service `commercial-account-svc`; `commercial_accounts` table, one-per-organization unique constraint; live-verified create + 409-on-duplicate |
| 3 | `workspace` / `legal_entity` / `business_unit` as first-class objects (today `legal_entity_id` is only a scoping FK, not a real entity) | §A2 | Done | `workspace` added to tenant-entity-registry-svc (migration 000005); `legal_entity` already existed; `business_unit` is a plain label field on workspace, not a separate table — no real content to populate a full object yet |
| 4 | `billing_classification` + `billing_source` on every workspace (COMMERCIAL_STANDALONE / ZOIKO_ONE / LEGACY_MIGRATION / PILOT_NON_BILLABLE / INTERNAL / DEMO / SANDBOX / QA_AUTOMATION) | §T, §A5 | Done | Mandatory column, validated against the full enum, fail-closed on an unrecognized value; live-verified (INTERNAL workspace created, invalid classification rejected with 400) |
| 5 | Membership resolution: actor + org + workspace + entity + data-class + permission, deny-by-default on ambiguous scope | §A3 | Done | `memberships` table in commercial-account-svc — answers "does this principal belong to this org" (commercial question); authorization-svc's existing RBAC still answers "what may they do" (permission question) — kept separate, not duplicated. Deactivate-only, never delete (§A6); live-verified create/list/deactivate/double-deactivate-409 |

## Chunk 6 — Plans, Pricing & Entitlements (net-new, Plane 1)

Resolution: extends `commercial-account-svc` (Chunk 5) rather than a new
service — a subscription/plan/entitlement is meaningless without the
commercial_account it bills, and entitlement resolution needs both in one
transaction boundary. `entitlement_service` is satisfied by a
`ResolveEntitlement` method, not a separate service, since it has no
independent data of its own to own.

| # | Item | Spec ref | Status | Notes |
|---|---|---|---|---|
| 6 | `price_catalog` / `catalog_version_id` — plans, prices, limits, add-ons, market scope | §B1 | Done | `price_catalogs` + `plans` tables; plan_code/display_name are data, never switched on; live-verified create catalog → create plan → set limit |
| 7 | `entitlement_service` + `subscription_item` (metric_type, entitlement_set_id, limits) | §B2 | Done | `ResolveEntitlement(subscriptionID, metricType)` — plan limit, overridden by an active contract overlay if one exists; live-verified both PLAN and OVERLAY resolution paths |
| 8 | `commercial_subscription` — plan, interval, status, renewal/cancel, processor refs | §N1 | Done | Status values are doc7 §29's canonical state machine verbatim (EVALUATION/ACTIVE/PAST_DUE/RESTRICTED/SUSPENDED/CANCELED/TERMINATED); one non-terminal subscription per commercial account enforced by a partial unique index; live-verified |
| 9 | `EVALUATION_PROGRAM` object for trials — no free trial exists today without one | §B3 | Done | `evaluation_programs`, one per subscription; live-verified create (14-day trial, expires_at computed) + duplicate rejected 409 |
| 10 | Upgrade/downgrade workflow — quote/preview, effective_at, consent, resulting entitlement version | §B4–B5 | Done | `subscription_change_requests`: PREVIEWED → APPLIED, applied atomically in one transaction; a subscription is never repointed without a prior, inspectable preview row. Live-verified: preview leaves subscription unchanged, confirm repoints it, second confirm correctly 409s |
| 11 | `contract_entitlement_overlay` for bespoke enterprise terms | §B6 | Done | `contract_entitlement_overlays`, wins over the plan's own limit when active; live-verified override changes ResolveEntitlement's answer from PLAN/10 to OVERLAY/500 |
| 12 | `commercial_usage_meter` — separate Plane 1 ledger with dedupe/aggregation before anything becomes billable | §B7, §N3 | Done | `commercial_usage_meter_events`, caller-supplied idempotency key as primary key (TEXT, not UUID — a real bug caught during live verification and fixed before commit); live-verified retry does not double-count |

## Chunk 7 — Capability & Release Registries (net-new, Plane 1)

Resolution: new service `capability-registry-svc` — five deliberately
separate tables/objects, matching §7's own warning that these dimensions
must never collapse into one feature flag. Does NOT call out to
commercial-account-svc (entitlement) or policy-svc (security eligibility) —
those remain the caller's own separate checks.

| # | Item | Spec ref | Status | Notes |
|---|---|---|---|---|
| 13 | `capability_registry` — does the capability exist at all | §C | Done | `capabilities` table; capability_code is data, never switched on |
| 14 | `market_release_registry` — jurisdiction/entity/language gating | §C, §Q1 | Done | `market_releases`; state values are doc7 §29's "Market release" list verbatim; live-verified GA-in-UK vs no-release-in-DE |
| 15 | `integration_capability_registry` — connector/provider certification status | §C | Done | `integration_capabilities`; live-verified an uncertified provider blocks resolution |
| 16 | `claim_registry` — what marketing/sales may say is available, linked to release evidence | §C2 | Done | `capability_claims`; wording_owner + approver required fields, never auto-generated from roadmap state |
| 17 | `release_registry` — GA / BETA / PILOT / INTERNAL / DISABLED / INCIDENT_RESTRICTED state | §C | Done | `releases`, append-only (history never overwritten, per §32.1 kill-switch doctrine); live-verified INCIDENT_RESTRICTED overriding an already-GA market release |
| 18 | Capability-resolution endpoint returning structured reason codes (enabled / unavailable / requires-upgrade / market-blocked / provider-unavailable / incident-restricted) | §C1 | Done | `GET /v1/capability-resolution/{code}`; live-verified all 4 reason-code paths (ENABLED, MARKET_BLOCKED, INCIDENT_RESTRICTED, PROVIDER_UNAVAILABLE) plus CAPABILITY_UNKNOWN |

## Chunk 8 — Zoiko One Billing & Double-Charge Prevention (extends commercial-account-svc, Plane 1) — ✅ Done 2026-08-12

Resolution: extends commercial-account-svc rather than a new service —
dunning/transfers are tightly coupled to `commercial_subscriptions`, a
sibling concept of what the service already owns, not a cross-cutting new
bounded context. `billing_source` reuses tenant-entity-registry-svc's exact
vocabulary (DIRECT/ZOIKO_ONE_BUNDLE) rather than a second, competing enum.

| # | Item | Spec ref | Status | Notes |
|---|---|---|---|---|
| 19 | Double-charge prevention — period-overlap checks, transfer records, split-billing exceptions | §P3, P0-blocker #5 | Done | Structural, not just application-level: the existing "one non-terminal subscription per commercial_account_id" partial unique index (migration 000002) is what actually makes double-billing impossible; live-verified a second concurrent `CreateSubscription` on the same account 409s even after a transfer |
| 20 | `transfer` record for standalone ↔ Zoiko One migration | §P3 | Done | `billing_source_transfers` table + `POST /v1/billing-source-transfers`; atomically cancels the old subscription and creates the new one in one transaction, never a silent swap; live-verified DIRECT→ZOIKO_ONE_BUNDLE transfer leaves exactly one ACTIVE subscription |
| 21 | Failed-payment/dunning state machine — PAST_DUE → RESTRICTED → SUSPENDED, idempotent restore-on-retry | §O1–O3 | Done | `ValidSubscriptionStatusTransitions` map + `subscription_status_events` append-only audit trail; `POST /v1/subscriptions/{id}/status`; live-verified full ACTIVE→PAST_DUE→RESTRICTED→SUSPENDED→ACTIVE escalation/recovery, a same-status idempotent repeat that logs no extra event, and a rejected ACTIVE→RESTRICTED direct jump (409) |
| 22 | Merchant/tax/processor/invoice identity setup for ZoikoSuite's own billing | P0-blocker #6 | Blocked | Real business/legal setup (merchant-of-record agreement, tax registration, payment processor account) — not buildable as generic code per the "don't fabricate a signal with nothing real to populate it" doctrine; no code changes attempted |

Bug caught during live verification: the running container was still serving
the pre-Chunk-8 binary (missing `billing_source` from JSON responses) after
only a container *restart* — restart reuses the existing image, it doesn't
rebuild it. Fixed by `docker compose build commercial-account-svc` before
restarting; noted here since it's the same class of stale-artifact trap as
the pgx prepared-statement-cache issue from Chunk 6, but one layer further
out (image vs. connection cache).

## Chunk 9 — AI Governance & Automation Policy (net-new, Plane 5) — ✅ Done 2026-08-12

Resolution: new service `ai-governance-svc` — a record-keeping and
gate-checking layer only; it never runs models or executes automations
itself, per doc7 §11's own doctrine that "the deterministic policy layer...
tenant automation policy and required human approvals outrank model
preference." `AIRunType` and risk categories are quoted verbatim from §G1/§G2
rather than locally invented taxonomies.

| # | Item | Spec ref | Status | Notes |
|---|---|---|---|---|
| 23 | AI run/recommendation object — model/prompt/tool version, source/evidence refs, confidence, audit ID | §G1 | Done | `ai_runs` table, `POST/GET /v1/ai-runs`; live-verified a RECOMMEND run with source/evidence refs and confidence recorded |
| 24 | Action-risk classification + human-review-trigger taxonomy | §G2, §AI-01 | Done | `action_risk_classifications`; risk_category values are §G2's own enumerated list (MONEY/EMPLOYMENT/TAX_FILING/.../REGULATED_REPORTING) verbatim |
| 25 | `automation_action` object — preconditions, approvals, idempotency, postcondition verification, rollback | §G2, §G7 | Done | `automation_actions`, unique `(tenant_id, idempotency_key)`; live-verified a duplicate idempotency_key 409s |
| 26 | Autonomous-action allowlist per tenant/role/risk-class/tool | §G7 | Done | `automation_policies` + `GET /v1/automation-policies/resolve`; fail-closed by default — live-verified an unlisted action 403s (NOT_ALLOWLISTED) and an allowlisted one proceeds |
| 27 | Provider/model registry — training-use posture, retention, region, DPA verification | §G6 | Done | `model_provider_registrations`; defaults to `NO_TRAINING` per §G6 ("No default training use is authorized"); `GET .../verify?data_class=` live-verified blocking an unapproved data class and an unregistered provider |
| 28 | Maker-checker / self-approval blocking for AI-proposed policy or control changes | §G3 | Done | `policy_change_approvals` (for policy/control changes) and the decision step on `automation_actions` (for heightened-risk automation) both enforce `decider != proposer`; live-verified self-approval 403s on both objects, a different approver succeeds on both |

## Chunk 10 — Governance/Evidence Plane Gaps (extend existing services) — items 29–31 ✅ Done 2026-08-12, items 32–36 deferred

Resolution for 29–31: extended policy-svc (control tests + attestations —
both are governance-record concepts that already lives in this service's
domain, not a new bounded context) and obligations-svc (applicability
decisions — obligation-specific, extends the service that already owns
`obligations`). Items 32–36 are each cross-cutting across 4+ existing
services (outbox touches every service that publishes events; retention/
legal-hold touches every service that stores anything deletable; replay
manifests need policy-svc + governance-decision-log-svc + audit-event-
store-svc together; report metrics and source-authority mapping are
reporting/connected-systems concerns with no natural single owner). Building
any of them properly means a dedicated session scoped to that one item
across every affected service, not a shared slot in this chunk — flagged
here as the deliberate reason they're Not Started rather than attempted as
shallow, single-service stubs.

| # | Item | Spec ref | Status | Notes |
|---|---|---|---|---|
| 29 | `control_test_definition` / `control_test_execution` — separate design-status from operating-effectiveness (currently collapsed in policy-svc) | §E3, §I3 | Done | policy-svc migration 000004; `control_test_definitions` (immutable, DESIGN_STATUS) + `control_test_executions` (append-only, OPERATING_EFFECTIVENESS in `result`); `GET /v1/controls/{control_ref}/effectiveness` composes both as independent fields; live-verified a control that is TESTED (design exists) while its latest execution is INEFFECTIVE — the two never collapse into one status |
| 30 | `attestation` object — signed/attributed assertions with expiry/revocation (doesn't exist under any name today) | §E6 | Done | `attestations` table in the same migration; signer/role/period/evidence_refs/expiry + ACTIVE/CHALLENGED/REVOKED state; live-verified create → revoke → a second revoke attempt 409s (illegal transition, not silently re-applied) |
| 31 | `applicability_decision` — versioned, with confidence/uncertainty and UNASSESSED/APPLICABLE/UNCERTAIN states (closest today is `access_decision_log`, which is generic, not obligation-specific) | §E2, §29 | Done | obligations-svc migration 000002; append-only `applicability_decisions`; `GET .../applicability` returns UNASSESSED when no row exists for a scope — never coerced to NOT_APPLICABLE; live-verified three independent scopes on the same obligation resolving to UNASSESSED, APPLICABLE, and UNCERTAIN respectively, plus a decision missing both actor and system rejected with 400. Real bug caught during live verification: `facts_used` was typed `[]byte` in Go, which `encoding/json` silently base64-encodes instead of inlining — fixed to `json.RawMessage` before commit |
| 32 | Transactional outbox pattern — every service currently publishes Kafka events directly with no outbox table; a crash between DB commit and publish can silently drop an event | §L1–L2, §I5 | Not Started | Deferred — cross-cutting across every event-publishing service; needs its own session |
| 33 | `retention_policy` / `legal_hold` objects — don't exist anywhere; needed before any real data-deletion path is safe | §J1, §J3 | Not Started | Deferred — cross-cutting across every service that stores deletable data; needs its own session |
| 34 | `replay_manifest` for historical decision replay against the source/policy versions active at the time | §I5, §29 | Not Started | Deferred — needs policy-svc + governance-decision-log-svc + audit-event-store-svc together; needs its own session |
| 35 | `report_metric_definition` — versioned formula/scope/owner for every executive metric | §M1 | Not Started | Deferred — reporting concern with no natural single owning service yet; needs its own session |
| 36 | `source_authority_map` / `normalized_fact` — field-level source-of-truth precedence for connected systems | §D1–D3 | Not Started | Deferred — connected-systems concern with no natural single owning service yet; needs its own session |

## Chunk 11 — Cross-Cutting Reliability & Security Controls

| # | Item | Spec ref | Status | Notes |
|---|---|---|---|---|
| 37 | Kill switches — plane/domain/provider/tenant-scoped, for commercial charging, automation, model/provider use, obligation activation, imports/syncs, exports, public claims | §32.1 | Not Started | |
| 38 | Safe-degraded-mode definitions per service (stale integration → show timestamped last-known fact; evidence-store outage → fail closed on approvals) | §32.2 | Not Started | |
| 39 | Wire §32's observability signal families (commercial integrity, tenant/data integrity drift, governance-engine errors, AI/automation anomalies) into the existing OTel/Prometheus setup | §32 | Not Started | |
| 40 | Numeric SLOs (availability/latency/recovery/AI-quality) — intentionally not invented by the spec; needs real production measurement first | §32 | Not Started | |

## Chunk 12 — Production Acceptance Sign-Off (§27 checklist, process gate)

| # | Item | Spec ref | Status | Notes |
|---|---|---|---|---|
| 41 | Work through all 26 acceptance criteria (COM-01 through GO-01) with named function owners (Product, Engineering, Finance, Security, Privacy, Legal, AI/ML, QA) | §27 | Not Started | Process, not code |
| 42 | Controlled Sign-Off Record — every function currently shows "Pending" with no approver/date | §35 | Not Started | Gate for doc7 becoming the controlling production standard |

---

## Legend

- **Net-new (Chunks 5–9)**: no existing service or table to extend — these are new domains.
- **Extend (Chunks 10–11)**: existing services gain new objects/behavior.
- **Process (Chunk 12)**: organizational sign-off, not engineering work.

## Change log

- 2026-08-11 — Initial backlog created from full §28 data-model audit against the running codebase.
- 2026-08-12 — Chunk 5 (items 1–5) complete: new `commercial-account-svc` (commercial_accounts + memberships), `workspace` added to tenant-entity-registry-svc with mandatory billing_classification/billing_source. All 5 items build/vet/test clean and live-verified against real Postgres + authorization-svc.
- 2026-08-12 — Chunk 6 (items 6–12) complete: price_catalogs/plans/entitlement_limits, commercial_subscriptions, evaluation_programs, contract_entitlement_overlays, commercial_usage_meter_events, subscription_change_requests — all added to commercial-account-svc. Two real bugs caught during live verification and fixed before commit: usage_event_id was declared UUID but is a caller-supplied idempotency key (not guaranteed UUID-shaped); catalog/plan-admin actions were calling authorization-svc with an empty scope, which it rejects (fixed to use the platform-scope convention already established elsewhere in this codebase). All 7 items build/vet/test clean and live-verified end-to-end.
- 2026-08-12 — Chunk 7 (items 13–18) complete: new `capability-registry-svc` — capabilities, market_releases, integration_capabilities, releases (append-only), capability_claims, plus a capability-resolution endpoint checking all four in priority order. Built and live-verified cleanly on the first pass.
- 2026-08-12 — Chunk 8 (items 19–21) complete, item 22 Blocked: extended commercial-account-svc with billing_source on commercial_subscriptions, billing_source_transfers, and subscription_status_events (append-only dunning audit trail) via migration 000003. New endpoints: `POST /v1/subscriptions/{id}/status` (dunning transitions, fail-closed via ValidSubscriptionStatusTransitions, idempotent same-status no-op), `GET /v1/subscriptions/{id}/status-events`, `POST /v1/billing-source-transfers` (atomic cancel-old/create-new). Double-billing prevention is structural (existing partial unique index), not just application logic. Live-verified the full ACTIVE→PAST_DUE→RESTRICTED→SUSPENDED→ACTIVE cycle, an idempotent repeat logging no extra event, a rejected invalid transition (409), and a DIRECT→ZOIKO_ONE_BUNDLE transfer. Caught a stale-image trap during verification: a container *restart* does not pick up new code, only a rebuild does — fixed by rebuilding the image before restarting.
- 2026-08-12 — Chunk 9 (items 23–28) complete: new `ai-governance-svc` — ai_runs, action_risk_classifications, automation_policies (allowlist), automation_actions, model_provider_registrations, policy_change_approvals. Pure record-keeping/gate-checking layer, never executes models or automations itself. Built and live-verified cleanly on the first pass: an unallowlisted autonomous action 403s, an allowlisted one proceeds and requires maker-checker approval, self-approval is blocked on both automation-action decisions and policy-change approvals (403 for the proposer, 200 for a different approver), a duplicate idempotency_key 409s, and the model-provider verify endpoint correctly blocks an unapproved data class and an unregistered provider.
- 2026-08-12 — Chunk 10 (items 29–31) complete, items 32–36 explicitly deferred as follow-up (see Chunk 10 section for why each is cross-cutting): extended policy-svc with control_test_definitions/control_test_executions/attestations (migration 000004) and obligations-svc with applicability_decisions (migration 000002). Live-verified the doc7 §E3 payoff directly — a control showing TESTED design status with an independently INEFFECTIVE latest execution result — plus attestation revoke-then-re-revoke correctly 409ing, and three applicability scopes on one obligation resolving to UNASSESSED/APPLICABLE/UNCERTAIN respectively. One real bug caught and fixed before commit: obligations-svc's `facts_used` field was typed `[]byte`, which Go's `encoding/json` silently base64-encodes instead of serializing as inline JSON — changed to `json.RawMessage`.
