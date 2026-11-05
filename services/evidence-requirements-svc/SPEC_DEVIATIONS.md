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
**Spec:** `AUTH-PLANE-PLAN-2026-11-02.md` "Global constraints": the service enforces
identity and authorization. The callers outside the governance domains
(board-resolutions-svc, corporate-actions-svc, filing-preparation-svc) do not yet send
an identity envelope, and their principals are not yet granted EVIDENCE_EVALUATE.

**Built:** compose runs `EVIDENCE_EVALUATE_AUTHZ=warn` and `IDENTITY_VERIFY=warn`
(both default to enforce/require in code). What warn admits, exactly
(identity-envelope `gate.go`, pinned by `TestGateWarnStillRefusesBadEnvelope` there and
by `TestER05_TamperedOrExpiredEnvelopeIs401EvenInWarnMode` here):
- a request with **no** envelope is admitted as asserted and marked
  `X-Identity-Assertion: missing`;
- a request whose envelope is **present but invalid** (bad signature, expired, wrong
  audience) is **401** in both modes, and so is one whose session identity-context
  reports revoked (`TestER05_RevokedSessionIsRefused`; live: E2E §2);
- with evaluate in warn, a principal without EVIDENCE_EVALUATE is admitted **and** the
  refusal is recorded in authorization-svc's decision log (E2E §10).
Reads, catalogue writes and PBC commands are always enforced.

**Status:** BUILT — RECORDED. A migration state: switch both to enforce once the three
callers forward the envelope and their principals are granted (cross-team follow-ups).

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

## D-ER-21: API-3 — which commands need If-Match
**Spec:** `IMPLEMENTATION-PLAN` §5 API-3; plan ER-4.

**Built:** migration 000010 adds `record_version` to `evidence_requirements` and
`evidence_requests`, served as the ETag `"v<n>"` on every read and every command's
response. The rule applied:
- **If-Match required** on every manager command that changes an existing aggregate:
  requirement end-date; PBC send, reassign, extend-due-date, clarify, mark-satisfied,
  close. Missing → 428 `precondition_required`; stale → 412 `precondition_failed` with
  `current_version` and the current ETag; malformed → 400; `*` → unconditional.
  The version is part of the same compare-and-set UPDATE (no read-then-write).
- **No If-Match** on the contributor's and the scanner's own actions on an open
  request — submit response, view, add note, acknowledge receipt, scan result. They
  append rows (responses, notes, receipts) or update a response, not edit the request.
  Submit and view do move the request's status, so they bump `record_version`, and a
  manager holding the ETag from before the response is refused with 412.

Tests: `TestAPI3_*` (handler, three cases per command) and
`TestAPI3_EndDateRequirementIsConditionalOnTheVersion`,
`TestAPI3_PBCCommandsBumpTheVersionAndRefuseStaleOnes` (real Postgres).

**Status:** BUILT — RECORDED.

---

## D-ER-22: API-4 on the console lists (breaking change to two list routes)
**Spec:** `IMPLEMENTATION-PLAN` §5 API-4; plan ER-3.

**Built:** `GET /v1/evidence-requirements`, `GET /v1/evidence/evaluations` and
`GET /v1/pbc/requests` take `filter[name]=value`, `page_size` (1..200, default 50) and an
opaque HMAC-signed `cursor` bound to the tenant, the route and the filter set
(identity-envelope/apicontract, as in identity-context-svc). Response
`{items, next_cursor, page_size}`. Unknown filter → 400 `unknown_filter`; a tampered
cursor, or one replayed by another tenant or with other filters → 400 `invalid_cursor`.
A bare pre-API-4 filter (`?legal_entity_id=`) is refused with 400 rather than ignored,
so a caller never receives an unfiltered list it believes is filtered.

Deviation: the two existing lists changed shape (array → page). The only caller is the
admin console, rebuilt in the same pass; no other service reads them.

Key: `CURSOR_SIGNING_KEY_HEX` (≥ 32 bytes), required outside local/dev; local/dev use a
per-process random key, so a cursor does not survive a restart.

**Status:** BUILT — RECORDED.

---

## D-ER-23: Evidence summary
**Spec:** plan ER-3 / ER-FE-2 (console KPI row).

**Built:** `GET /v1/evidence/summary` (EVIDENCE_REQUIREMENT_READ at PLATFORM) returns, in
one transaction: active requirements; evaluations by outcome over 30 days; distinct
(domain, action) pairs evaluated with NO_REQUIREMENTS_DEFINED, and the 20 most frequent;
open and overdue PBC requests; active requirements by evidence type and by domain; and
outbox rows unpublished for more than 5 minutes. The outbox count is **platform-wide**
(the outbox has no tenant column) and is read through the SECURITY DEFINER
`outbox_unpublished_count()` (000011). If any count cannot be read the route is 503 —
never a partial summary, so the console shows "unavailable", not 0.

**Status:** BUILT — RECORDED.

---

## D-ER-24: Which evidence types are verified
**Spec:** §8.6 owns document and signature requirements and supporting artifact rules.

**Built:** `SUPPORTING_DOCUMENT` is verified with document-vault-svc, `EVIDENCE_MANIFEST`
with evidence-manifest-svc (`GET /v1/evidence-manifests/{id}`), `SIGNATURE` with
esignature-integration-svc (`GET /v1/esignature/envelopes/{id}`): existence in the
caller's tenant and legal entity (D-ER-3: no status is required). Not found / other
entity → does not count, reason in `unmet`. Owner unreachable, refusing (any non-200 /
404) or unconfigured → 503, nothing recorded. Every other evidence type
(APPROVAL_RECORD, …) counts on the caller's word when its reference is not empty.

Live status (5 Nov 2026): EVIDENCE_MANIFEST references currently always fail closed,
because evidence-manifest-svc authorizes every read on `EVIDENCE_MANIFEST_READ`, which
access-control does not register (cross-team follow-up). SIGNATURE could not be run live:
esignature-integration-svc runs in the phase7 stack.

**Status:** BUILT — RECORDED.

---

## D-ER-25: Compose Port Binding Loopback-Only in Warn Mode
**Spec:** Security best practice.

**Built:** The compose port mapping is `127.0.0.1:8130:8130` (loopback only) while
`EVIDENCE_EVALUATE_AUTHZ=warn` and `IDENTITY_VERIFY=warn`. This prevents external
access to the service when enforcement is relaxed.

**Status:** BUILT — RECORDED.

---

## D-ER-26: The outbox is append-only for the runtime role (migration 000011)
**Spec:** Event Catalogue §6 (transactional outbox); R4 (RLS on every table).

**Found:** 000008 put `FOR ALL USING (false)` on `outbox`. For the runtime role
(`app_evidence_requirements`: NOSUPERUSER, NOBYPASSRLS, not the owner) that refuses INSERT
too, so every SATISFIED/MISSING evaluation failed with "new row violates row-level security
policy for table outbox" and was answered 503. Verified live before the fix.

**Built:** policy `outbox_append_only FOR INSERT WITH CHECK (true)`; no SELECT, UPDATE or
DELETE policy, so the role cannot read or rewrite events; the worker keeps using the
SECURITY DEFINER claim/mark functions; `outbox_unpublished_count()` for the backlog.
Grants guarded on the role existing. Test: `TestOutbox_AppRoleAppendsButCannotRead`.

**Status:** BUILT — RECORDED.

---

## D-ER-27: The PBC register is the managers' register
**Spec:** GA-21 "read contracts strongly scoped"; plan R6 (authorization at the object).

**Built:** `GET /v1/pbc/requests` is authorized under `EVIDENCE_REQUEST_MANAGE` (read gate
mode) at `filter[legal_entity_id]`, PLATFORM when unfiltered — no PBC read action exists in
access-control. `GET /v1/pbc/requests/{id}` is unchanged in this pass and is still
authorized by tenant only (recorded as an open follow-up, not fixed here).

**Status:** BUILT — RECORDED.

---

## D-ER-28: Edge route on websecure with `gateway-auth@file`
**Spec:** plan ER-5 (Traefik labels with ForwardAuth, copied from jurisdiction-svc).

**Built:** router `evidence-requirements`, `PathPrefix(/evidence-requirements)`, stripped,
middlewares `gateway-auth@file,evidence-requirements-strip`, entrypoint **websecure**
(`https://127.0.0.1:8443`). Two deliberate differences from jurisdiction-svc: the web
entrypoint redirects every request to websecure before routing, so a router on web alone
is unreachable; and `gateway-auth@file` (emitted by the GTRM compiler) does not depend on
jurisdiction-svc's container being up to define the middleware. Live (E2E §3): no bearer
→ 401 `X-Auth-Denial-Reason: no_token`; forged X-Tenant-Id / X-Principal-Id next to an
envelope are replaced. A write through the edge also needs carta-svc (gateway-auth fails
closed on material writes when its risk engine is configured but silent).

**Status:** BUILT — RECORDED.

---

*End of SPEC_DEVIATIONS.md — all deviations recorded at build time, per the doctrine
"When in doubt about ownership or contract shape, stop and ask — do not improvise
architecture."*