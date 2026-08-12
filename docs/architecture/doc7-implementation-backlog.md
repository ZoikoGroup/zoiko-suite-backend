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

| # | Item | Spec ref | Status | Notes |
|---|---|---|---|---|
| 6 | `price_catalog` / `catalog_version_id` — plans, prices, limits, add-ons, market scope | §B1 | Not Started | |
| 7 | `entitlement_service` + `subscription_item` (metric_type, entitlement_set_id, limits) | §B2 | Not Started | |
| 8 | `commercial_subscription` — plan, interval, status, renewal/cancel, processor refs | §N1 | Not Started | |
| 9 | `EVALUATION_PROGRAM` object for trials — no free trial exists today without one | §B3 | Not Started | |
| 10 | Upgrade/downgrade workflow — quote/preview, effective_at, consent, resulting entitlement version | §B4–B5 | Not Started | |
| 11 | `contract_entitlement_overlay` for bespoke enterprise terms | §B6 | Not Started | |
| 12 | `commercial_usage_meter` — separate Plane 1 ledger with dedupe/aggregation before anything becomes billable | §B7, §N3 | Not Started | |

## Chunk 7 — Capability & Release Registries (net-new, Plane 1)

| # | Item | Spec ref | Status | Notes |
|---|---|---|---|---|
| 13 | `capability_registry` — does the capability exist at all | §C | Not Started | |
| 14 | `market_release_registry` — jurisdiction/entity/language gating | §C, §Q1 | Not Started | |
| 15 | `integration_capability_registry` — connector/provider certification status | §C | Not Started | |
| 16 | `claim_registry` — what marketing/sales may say is available, linked to release evidence | §C2 | Not Started | |
| 17 | `release_registry` — GA / BETA / PILOT / INTERNAL / DISABLED / INCIDENT_RESTRICTED state | §C | Not Started | |
| 18 | Capability-resolution endpoint returning structured reason codes (enabled / unavailable / requires-upgrade / market-blocked / provider-unavailable / incident-restricted) | §C1 | Not Started | |

## Chunk 8 — Zoiko One Billing & Double-Charge Prevention (net-new, Plane 1)

| # | Item | Spec ref | Status | Notes |
|---|---|---|---|---|
| 19 | Double-charge prevention — period-overlap checks, transfer records, split-billing exceptions | §P3, P0-blocker #5 | Not Started | |
| 20 | `transfer` record for standalone ↔ Zoiko One migration | §P3 | Not Started | |
| 21 | Failed-payment/dunning state machine — PAST_DUE → RESTRICTED → SUSPENDED, idempotent restore-on-retry | §O1–O3 | Not Started | |
| 22 | Merchant/tax/processor/invoice identity setup for ZoikoSuite's own billing | P0-blocker #6 | Not Started | |

## Chunk 9 — AI Governance & Automation Policy (net-new, Plane 5)

| # | Item | Spec ref | Status | Notes |
|---|---|---|---|---|
| 23 | AI run/recommendation object — model/prompt/tool version, source/evidence refs, confidence, audit ID | §G1 | Not Started | |
| 24 | Action-risk classification + human-review-trigger taxonomy | §G2, §AI-01 | Not Started | |
| 25 | `automation_action` object — preconditions, approvals, idempotency, postcondition verification, rollback | §G2, §G7 | Not Started | |
| 26 | Autonomous-action allowlist per tenant/role/risk-class/tool | §G7 | Not Started | |
| 27 | Provider/model registry — training-use posture, retention, region, DPA verification | §G6 | Not Started | |
| 28 | Maker-checker / self-approval blocking for AI-proposed policy or control changes | §G3 | Not Started | |

## Chunk 10 — Governance/Evidence Plane Gaps (extend existing services)

| # | Item | Spec ref | Status | Notes |
|---|---|---|---|---|
| 29 | `control_test_definition` / `control_test_execution` — separate design-status from operating-effectiveness (currently collapsed in policy-svc) | §E3, §I3 | Not Started | |
| 30 | `attestation` object — signed/attributed assertions with expiry/revocation (doesn't exist under any name today) | §E6 | Not Started | |
| 31 | `applicability_decision` — versioned, with confidence/uncertainty and UNASSESSED/APPLICABLE/UNCERTAIN states (closest today is `access_decision_log`, which is generic, not obligation-specific) | §E2, §29 | Not Started | |
| 32 | Transactional outbox pattern — every service currently publishes Kafka events directly with no outbox table; a crash between DB commit and publish can silently drop an event | §L1–L2, §I5 | Not Started | |
| 33 | `retention_policy` / `legal_hold` objects — don't exist anywhere; needed before any real data-deletion path is safe | §J1, §J3 | Not Started | |
| 34 | `replay_manifest` for historical decision replay against the source/policy versions active at the time | §I5, §29 | Not Started | |
| 35 | `report_metric_definition` — versioned formula/scope/owner for every executive metric | §M1 | Not Started | |
| 36 | `source_authority_map` / `normalized_fact` — field-level source-of-truth precedence for connected systems | §D1–D3 | Not Started | |

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
