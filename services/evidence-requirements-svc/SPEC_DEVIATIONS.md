# evidence-requirements-svc — Specification Deviations

This file records every place where the implementation diverges from the authoritative
specification documents (`03-microservices.md`, `04-data-model.md`, `AUTH-PLANE-PLAN`,
`IMPLEMENTATION-PLAN`). Each deviation is numbered D-ER-N, recorded at the time the
decision was made, and tagged as **BUILT** (code already reflects it) or **OPEN** (needs
human confirmation before rows exist).

---

## D-ER-1: Tier Classification
**Spec:** `03-microservices.md` §23 enumerates Tier 0 as "Identity, authorization, policy,
jurisdiction, tenant/entity, workflow, audit event store, secret vault integration" and
**does not name evidence-requirements-svc**, even though §5.1 places it in the
Governance Platform group alongside five services that *are* Tier 0.

**Built:** This service is built to Tier 0 discipline (non-bypassable, fail-closed,
evidential) because its own §8.6 critical constraint demands it. The tier label itself
needs a human decision.

**Status:** BUILT — code does not depend on the label; OPEN for confirmation.

---

## D-ER-2: Scan-Result Ownership
**Spec:** No specification defines who may write `malware_scan_status` on a PBC response.
The `evidence-manifest-svc` does not own it (manifests are post-hoc assembly). No
dedicated "scanner" identity exists in the platform yet.

**Built:** The `MarkSatisfied` guard uses `EVIDENCE_REQUEST_MANAGE` as the interim
authorization action (see `pbc.go:requireManageAuthz`). A dedicated `EVIDENCE_SCANNER`
action and identity should be created when the scanner service exists.

**Status:** BUILT — RECORDED.

---

## D-ER-3: Document and Signature Statuses That Count
**Spec:** `document-vault-svc` owns document lifecycle (statuses like UPLOADED,
SCANNED, SIGNED, ARCHIVED). `esignature-integration-svc` owns envelope lifecycle. No
spec section says which statuses count as valid evidence.

**Built:** This service verifies **existence + tenant/legal-entity scope only**. It does
not gate on status. A document that exists and is in scope counts toward its
requirement regardless of its status. This is deliberate: asserting a status here
would be scope invention.

**Status:** BUILT — RECORDED.

---

## D-ER-4: Nullable `legal_entity_id` on `EvidenceRequirement`
**Spec:** `04-data-model.md` §7.1 lists `tenant_id` but **no `legal_entity_id`**, while
`.agents/rules/doctrine.md` requires *"every material record carries tenant_id,
legal_entity_id, and effective_from/effective_to."* A direct conflict.

**Built:** `legal_entity_id` is nullable. NULL = tenant-wide requirement. The
`EffectiveRequirements` query resolves both entity-scoped rows for the entity in
question, plus every tenant-wide row. The unique index uses `COALESCE(legal_entity_id, '00000000-0000-0000-0000-000000000000')` because Postgres treats NULLs as distinct in a unique index and would otherwise permit unlimited duplicate tenant-wide rows.

**Status:** BUILT — DECIDED, RECORDED. Expensive to reverse after rows exist.

---

## D-ER-5: `NO_REQUIREMENTS_DEFINED` Caller Contract
**Spec:** §8.6 names two outcomes: `evidence.requirement.satisfied` and
`evidence.requirement.missing`. It is silent on the case where the catalogue has no
effective rows for the requested `(domain_code, action_type)`.

**Built:** The evaluation returns three distinct outcomes:
- `SATISFIED` — all requirements met → publishes `evidence.requirement.satisfied`
- `MISSING` — one or more unmet → publishes `evidence.requirement.missing`
- `NO_REQUIREMENTS_DEFINED` — zero effective rows → **publishes no event**

`NO_REQUIREMENTS_DEFINED` is **never** reported as `SATISFIED`. The calling service
decides whether its action may proceed without configured requirements. This
distinction was drawn deliberately in reaction to the `tax-determination-svc`
defect (returning a synthetic `"ZERO-TAX"` rule at 0% when the rules service was
unreachable or had no match).

**Status:** BUILT — DECIDED 21 Oct 2026, RECORDED.

---

## D-ER-6: Compose Gate Modes Warn
**Spec:** `AUTH-PLANE-PLAN-2026-11-02.md` "Global constraints" states the service must
enforce identity and authorization gates. However, the callers outside the governance
domains (board-resolutions-svc, corporate-actions-svc, filing-preparation-svc) do not
yet send a full identity envelope, and their principals are not yet granted the
required actions.

**Built:** The compose stack runs with:
- `EVIDENCE_EVALUATE_AUTHZ=warn` — authorization failures are logged and admitted
- `IDENTITY_VERIFY=warn` — missing/invalid envelopes are logged and admitted

Both default to `enforce` in code; the warn mode is set via compose environment
variables. This is a migration state, not a resting state. The callers must be
updated to send full envelopes and have their principals granted before switching to
`enforce`.

**Status:** BUILT — RECORDED.

---

## D-ER-7: `evidence-manifest-svc` and `esignature-integration-svc` Optional Locally
**Spec:** The requirement to verify EVIDENCE_MANIFEST and SIGNATURE artifacts is
absolute per §8.6.

**Built:** Outside local/dev, `EVIDENCE_MANIFEST_SERVICE_URL` and `ESIGNATURE_SERVICE_URL`
are **required** (container startup fails if unset). Locally, they are optional; when
unset, the handler fails closed with 503 for those evidence types (never silently
counts them). This allows local development without running all seven services.

**Status:** BUILT — RECORDED.

---

## D-ER-8: Evaluation is Synchronous Request/Response
**Spec:** `03-microservices.md` §4 says services consume events. But §8.6 requires
evaluation to be a **blocking precondition** on a caller's in-flight transaction.

**Built:** Evaluation is synchronous HTTP (POST /v1/evidence/evaluate). An async
consumer cannot gate a finalization the way §8.6 requires. This matches how every
built service performs cross-service verification today (e.g., `accounts-receivable-svc`
→ `general-ledger-svc`). No Kafka consumer is implemented for v1.

**Status:** BUILT — RECORDED.

---

## D-ER-9: Only Two Events Published
**Spec:** §8.6 names exactly two events: `evidence.requirement.satisfied` and
`evidence.requirement.missing`.

**Built:** No other events are published (no `evidence.requirement.created`,
`evidence.requirement.retired`, etc.). The evaluation ledger (`evidence_evaluations`)
serves as the audit trail for catalogue changes.

**Status:** BUILT — RECORDED.

---

## D-ER-10: PBC `MarkSatisfied` Uses `EVIDENCE_REQUEST_MANAGE` Interim
**Spec:** AUD-05 (PBC requests) does not define a dedicated scanner identity.

**Built:** The `MarkSatisfied` endpoint (POST /v1/pbc/requests/{id}/mark-satisfied)
is authorized on `EVIDENCE_REQUEST_MANAGE` at the request's legal entity. The guard
also refuses self-evaluation (submitter cannot mark own response satisfied) and
requires the latest response's artifact to be scanned CLEAN. A dedicated
`EVIDENCE_SCANNER` action should be created when the scanner service exists.

**Status:** BUILT — RECORDED (related to D-ER-2).

---

## D-ER-11: `NO_REQUIREMENTS_DEFINED` Publishes No Event
**Spec:** §8.6 only names two events.

**Built:** When the outcome is `NO_REQUIREMENTS_DEFINED`, no outbox event is written.
This is deliberate: an empty catalogue is a legitimate data state, not a determination
that should appear in the event stream.

**Status:** BUILT — RECORDED.

---

## D-ER-12: Requirement Payload Validation at Create Time
**Spec:** `04-data-model.md` §7.1 says `requirement_payload` is JSONB with
`minimum_count`, `artifact_subtype`, `description`. No schema validation is specified.

**Built:** The create endpoint validates that the payload is an object containing only
known members (`minimum_count` ≥ 0 integer, `artifact_subtype` string, `description`
string). Unknown members or wrong types → 400. This prevents a malformed rule from
silently blocking or permitting actions later.

**Status:** BUILT — RECORDED.

---

## D-ER-13: Artifact Deduplication by Canonical Reference
**Spec:** Not specified.

**Built:** `checkArtifacts` memoizes verification results per canonical reference
(`artifactKey` = trimmed, lowercased UUID). The same document/manifest/envelope
offered twice (even with different casing or whitespace) counts once toward
`minimum_count`. This prevents a caller from satisfying `minimum_count: 2` by
submitting the same artifact twice.

**Status:** BUILT — RECORDED.

---

## D-ER-14: Empty `reference_id` Never Counts
**Spec:** Not specified.

**Built:** An artifact with an empty or whitespace-only `reference_id` is rejected
with `counts: false` and never satisfies any `minimum_count`. This prevents a caller
from satisfying a requirement by submitting empty references.

**Status:** BUILT — RECORDED.

---

## D-ER-15: Unconfigured Owner Service = 503, Never Silent Count
**Spec:** §8.6 says evidence requirements must be verified.

**Built:** If `EVIDENCE_MANIFEST_SERVICE_URL` or `ESIGNATURE_SERVICE_URL` is unset
(outside local/dev), container startup fails. If unset locally, the handler returns
503 for that evidence type. **Never** does an unconfigured owner service result in
the artifact silently counting toward satisfaction.

**Status:** BUILT — RECORDED.

---

## D-ER-16: Legal Entity Validated Against Registry
**Spec:** `03-microservices.md` §8.6 and `AUTH-PLANE-PLAN` require the legal entity to
exist in the caller's tenant.

**Built:** The `EntityChecker` (internal/registry) calls
`tenant-entity-registry-svc GET /v1/entities/{id}` to confirm existence and tenant
membership. Fail-closed on registry unavailability (503). The check runs on every
evaluation and every requirement create.

**Status:** BUILT — RECORDED.

---

## D-ER-17: Effective-Dated Catalogue Resolution
**Spec:** `04-data-model.md` §7.1 defines `effective_from` and `effective_to`.

**Built:** `EffectiveRequirements` returns requirements where `effective_from <= as_of`
AND (`effective_to IS NULL` OR `effective_to > as_of`). The `as_of` parameter
defaults to the evaluation time. An `as_of` of zero value means "no effective-date
filter — return all including retired", which is what an auditor reviewing history
needs. Entity resolution: entity-scoped rows for the entity + tenant-wide rows (NULL
legal_entity_id).

**Status:** BUILT — RECORDED.

---

## D-ER-18: Outbox Events Published After Commit
**Spec:** `03-microservices.md` §17.6 and `IMPLEMENTATION-PLAN` §5 require transactional
outbox.

**Built:** `RecordEvaluation` writes the evaluation row and the outbox event in the
same database transaction (000008). The outbox worker publishes to Kafka
asynchronously. Key: `<tenant>/<evaluation_id>`.

**Status:** BUILT — RECORDED.

---

## D-ER-19: Idempotency on `Idempotency-Key`, Not `correlation_id`
**Spec:** `IMPLEMENTATION-PLAN` §5 and `AUTH-PLANE-PLAN` require idempotency.

**Built:** All mutations (requirement create, end-date, PBC mutations) use the
`Idempotency-Key` header. The `correlation_id` field in the request body is for
business correlation, not replay protection. Replay with same `Idempotency-Key` and
identical body → 200 with original response. Replay with same key but different
body → 409 `idempotency_mismatch`.

**Status:** BUILT — RECORDED.

---

## D-ER-20: Evaluation Ledger Append-Only
**Spec:** `03-microservices.md` §17.6 "Every Material Service Must Be Evidential".

**Built:** `evidence_evaluations` table has a trigger guard (000009) that refuses any
UPDATE or DELETE. The table is append-only. `unmet_payload` and
`present_artifacts_payload` freeze the decision inputs at evaluation time.

**Status:** BUILT — RECORDED.

---

## D-ER-21: ETag/If-Match on All Mutations (API-3)
**Spec:** `IMPLEMENTATION-PLAN` §5 API-3.

**Built:** Migration 000010 adds `record_version` to `evidence_requirements` and
`evidence_requests`. All 13 mutation endpoints (1 end-date + 12 PBC) read the
`If-Match` header and include it in the UPDATE's WHERE clause as
`record_version = $ifMatch`. Stale version → 412 with `version_mismatch` code.
Response includes `ETag` header with current version.

**Status:** BUILT — RECORDED.

---

## D-ER-22: API-4 Cursor Pagination on All Lists
**Spec:** `IMPLEMENTATION-PLAN` §5 API-4.

**Built:** Four list endpoints use opaque, HMAC-signed cursor tokens:
- GET /v1/evidence-requirements/cursor
- GET /v1/evidence/evaluations
- GET /v1/pbc/requests/cursor
- GET /v1/evidence/summary (single page, no cursor)

Cursors encode tenant_id, route, filter, sort, offset, and last_id. Tampered/foreign
cursors → 400.

**Status:** BUILT — RECORDED.

---

## D-ER-23: Evidence Summary Endpoint
**Spec:** `IMPLEMENTATION-PLAN` §5 console read models.

**Built:** GET /v1/evidence/summary returns aggregate counts:
- `active_requirements`
- `evaluations_satisfied_30d`
- `evaluations_missing_30d`
- `actions_with_no_requirements_30d`
- `open_pbc_requests`
- `overdue_pbc_requests`
- `unpublished_outbox_older_than_5_minutes`

**Status:** BUILT — RECORDED.

---

## D-ER-24: `SUPPORTING_DOCUMENT` Verified, Others Taken on Caller's Word
**Spec:** §8.6 owns "document requirements" and "supporting artifact rules".

**Built:** Only `SUPPORTING_DOCUMENT` is verified against `document-vault-svc`.
`EVIDENCE_MANIFEST` is verified against `evidence-manifest-svc` (when configured).
`SIGNATURE` is verified against `esignature-integration-svc` (when configured).
All other evidence types (APPROVAL_RECORD, etc.) are taken on the caller's word in v1.
When their owner services exist, they become verifiable the same way.

**Status:** BUILT — RECORDED.

---

## D-ER-25: Compose Port Binding Loopback-Only in Warn Mode
**Spec:** Security best practice.

**Built:** The compose port mapping is `127.0.0.1:8130:8130` (loopback only) while
`EVIDENCE_EVALUATE_AUTHZ=warn` and `IDENTITY_VERIFY=warn`. This prevents external
access to the service when enforcement is relaxed.

**Status:** BUILT — RECORDED.

---

*End of SPEC_DEVIATIONS.md — all deviations recorded at build time, per the doctrine
"When in doubt about ownership or contract shape, stop and ask — do not improvise
architecture."*