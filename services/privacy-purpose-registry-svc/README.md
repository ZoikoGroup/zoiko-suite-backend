# privacy-purpose-registry-svc (PRV-01)

**Default Port:** `8151`
**Classification:** Interlinked Service
**Specification:** `ZS-SVC-W-001` (§8, §9, §18, §18.1, §19, §20.3, §23.1, §24, §31, §32)
**Role:** Registry of lawful processing purposes and processing activities (GDPR Article 30 RoPA): the legal and operational baseline for privacy decisions, consent tracking and data transfers.

> **Status: NOT production ready.** The service itself is remediated and verified against PostgreSQL, but three things outside it block a safe rollout — see [Dependency-blocked items](#dependency-blocked-items). The authoritative audit write-up is the PRV-01 section of `docs/audit_files/PRIVACY.md`.

---

## Architecture & Responsibilities

`privacy-purpose-registry-svc` governs the creation, review, activation and retirement of:

1. **Lawful purposes** — purpose statement, compatibility class, lawful-basis references, special-condition references and a necessity/minimization rationale. A published version is immutable (database trigger).
2. **Processing activities (RoPA)** — controller/processor role, business owner **and** privacy owner, purposes, subject classes, data categories, sources, recipients, jurisdictions, retention/transfer/processor references, notice/consent dependency and DPIA/TIA status. Content is immutable once a version leaves `DRAFT`; amending means a new version.

### Downstream dependencies

- `privacy-consent-svc` (8152): checks that a purpose is registered and `PUBLISHED` before recording consent.
- `privacy-decision-svc` (8153): checks that an activity is `ACTIVE` and its purposes `PUBLISHED`.
- `privacy-transfer-svc` (8155): validates `purpose_activity_refs` against registered activities.

All three call `GET` routes of this service. See [Dependency-blocked items](#dependency-blocked-items).

---

## Request pre-flight (every route)

Authentication and authorization strictly precede any idempotent replay.

| Step | Check | Refusal |
|---|---|---|
| 1 | `X-Principal-Id` present | `401 AUTHENTICATION_FAILED` |
| 2 | `X-Tenant-Id` present — a missing tenant is **never** platform scope | `401 TENANT_SCOPE_REQUIRED` |
| 3 | `authorization-svc` grant for the route's action in the verified scope | `403 AUTHORIZATION_DENIED`, `503 AUTHORIZATION_SERVICE_UNAVAILABLE` (fail closed) |
| 4 | mutating: `X-Correlation-ID` (echoed in the response) | `400 CORRELATION_ID_REQUIRED` |
| 5 | mutating: `Idempotency-Key`, then replay lookup | `400 IDEMPOTENCY_KEY_REQUIRED`; same key with a different method/path/body `409 IDEMPOTENCY_MISMATCH`; lookup failure `503` |

An idempotent replay returns the original response with `Idempotency-Replay: true`. Records are scoped to (tenant or platform plane, principal, key). **The claim, the change, its audit row, its outbox event and the stored replay response are committed in one transaction** (`store.RunIdempotent`; every store call the operation makes joins it): if any step — including saving the idempotency record — fails, nothing persists, so a retry re-executes cleanly, and concurrent identical requests execute exactly once. A refused operation (404/409/422 …) stores nothing and does not consume the key. Replay bodies are read from a `JSONB` column, so they are JSON-equivalent to the original, not byte-identical.

The `authorization-svc` client caches GRANTED/DENIED decisions for up to **5 seconds**, so a revocation is honoured within that window.

## Scope model: tenants and the platform plane

- A tenant id in `X-Tenant-Id` addresses that tenant's records.
- The **platform plane** (Zoiko as independent controller, §23.1/§24) is addressed **only** by sending the platform-scope id `00000000-0000-0000-0000-00000000f001` in `X-Tenant-Id`. It is also the scope passed to the authorization check.
- A tenant may mutate only its own records; the platform plane only platform records. Otherwise `403 PLATFORM_SCOPE_REQUIRED`. A tenant's record is invisible from the platform plane (`404`).
- Platform records are **readable** by every authorized tenant; tenant records only by their owner.
- Enforced again in PostgreSQL (migration 000006): a write to a `tenant_id IS NULL` row needs the transaction-local capability `app.platform_scope = 'true'`, set by the store only for the sentinel. With no tenant context nothing is writable; a platform context cannot write a tenant row; there is no DELETE policy, and a trigger rejects DELETE even for a superuser.
- A body `tenant_id` that disagrees with the verified header is `403 TENANT_SCOPE_MISMATCH`.

---

## Routes and the authorization action each requires

### Purposes

| Path | Method | Action | Description |
|---|---|---|---|
| `/privacy/purposes` | `POST` | `PRIVACY_PURPOSE_CREATE` | Create purpose and initial draft version |
| `/privacy/purposes` | `GET` | `PRIVACY_PURPOSE_READ` | List the currently published purposes: one row per purpose, the published version **in force now** (the version `GET /privacy/purposes/{id}` resolves with no parameters). Superseded versions, published versions whose `effective_from` is still in the future, and drafts are not listed. |
| `/privacy/purposes/{id}?as_of=&known_as_of=` | `GET` | `PRIVACY_PURPOSE_READ` | Resolve the published version in force |
| `/privacy/purposes/{id}/versions` | `POST` | `PRIVACY_PURPOSE_CREATE` | Create successor draft from a parent version |
| `/privacy/purposes/{id}/versions/{vid}/publish` | `POST` | `PRIVACY_PURPOSE_PUBLISH` | Publish (maker-checker, immutable) |

### Processing activities

| Canonical path | Explicit-version path | Method | Action |
|---|---|---|---|
| `/privacy/processing-activities` | — | `POST` | `PRIVACY_ACTIVITY_CREATE` |
| `/privacy/processing-activities/{id}?as_of=&known_as_of=` | — | `GET` | `PRIVACY_ACTIVITY_READ` |
| `…/{id}/versions` | — | `POST` | `PRIVACY_ACTIVITY_CREATE` |
| — | `…/{id}/versions/{vid}` | `GET` | `PRIVACY_ACTIVITY_READ` |
| `…/{id}/audit` | — | `GET` | `PRIVACY_ACTIVITY_READ` — the activity's append-only `activity_lifecycle_audit` rows, oldest first (actor, from/to status, correlation id, reason). Tenant-scoped by row-level security; an unknown or other-tenant id returns an empty list, never a 404 |
| `…/{id}/validate` | `…/versions/{vid}/validate` | `POST` | `PRIVACY_ACTIVITY_VALIDATE` |
| `…/{id}/submit` | `…/versions/{vid}/submit` | `POST` | `PRIVACY_ACTIVITY_SUBMIT` |
| `…/{id}/approve` | `…/versions/{vid}/approve` | `POST` | `PRIVACY_ACTIVITY_APPROVE` (maker-checker) |
| `…/{id}/reject` | `…/versions/{vid}/reject` | `POST` | `PRIVACY_ACTIVITY_REJECT` (maker-checker, `reason` required) |
| `…/{id}/activate` | `…/versions/{vid}/activate` | `POST` | `PRIVACY_ACTIVITY_ACTIVATE` |
| `…/{id}/suspend` | `…/versions/{vid}/suspend` | `POST` | `PRIVACY_ACTIVITY_SUSPEND` |
| `…/{id}/resume` | `…/versions/{vid}/resume` | `POST` | `PRIVACY_ACTIVITY_RESUME` |
| `…/{id}/retire` | `…/versions/{vid}/retire` | `POST` | `PRIVACY_ACTIVITY_RETIRE` |

The canonical (no version segment) forms act on the latest version, resolved only after authentication and authorization.

### RoPA

| Path | Method | Action | Description |
|---|---|---|---|
| `/privacy/ropa?role=&jurisdiction=` | `GET` | `PRIVACY_ROPA_READ` | `ACTIVE` versions in force, filtered by role (`CONTROLLER`/`PROCESSOR`/`JOINT_CONTROLLER`) and jurisdiction |

`/healthz` and `/readyz` are unauthenticated. `/healthz` answers `200 {"status":"ok"}`; `/readyz` answers `200 {"status":"ready"}`, or `503` with the standard error envelope (`STORE_UNAVAILABLE`) when the database pool is missing or does not answer.

The three `*_READ` action names are this service's: §8.1 limits callers to privacy admins, approved domain owners, the change pipeline and compliance services but names no permission.

---

## Minimum Activation Gates (§8.2)

Evaluated at `validate` (DRAFT → VALIDATED) and **again at `activate`** against the live registries; any finding blocks, and activation answers `422` with every finding. §32 fixes each code's meaning but does not map gates to codes; this mapping is the service's.

| Gate | Requirement | Code | Field |
|---|---|---|---|
| 1 | Business owner and privacy owner | `PRV-003` | `owner`, `privacy_owner` |
| 2 | Role resolved | `PRV-003` | `privacy_role` |
| 3 | ≥1 purpose; each registered, published, in force, with a necessity/minimization rationale | `PRV-001` | `purpose_ids` |
| 4 | Each linked purpose has a lawful-basis reference | `PRV-005` | `purpose_ids` |
| 4 | Sensitive data ⇒ ≥1 linked purpose has a special-condition reference | `PRV-010` | `special_condition_refs` |
| 5 | Jurisdictions enumerated, in the registry (ISO 3166-1 alpha-2, `EU`, `EEA`, `GLOBAL`) | `PRV-004` | `jurisdictions` |
| 5 | Subject classes; data categories in the taxonomy | `PRV-010` | `subject_classes`, `data_categories` |
| 5 | Sources and recipients | `PRV-019` | `sources`, `recipients` |
| 6 | Retention references | `PRV-014` | `retention_rule_refs` |
| 6 | More than one jurisdiction ⇒ transfer references | `PRV-015` | `transfer_refs` |
| 6 | `PROCESSOR` role ⇒ processor-instruction reference | `PRV-017` | `processor_refs` |
| 7 | Notice/consent dependency explicit: `REQUIRED`, `NOT_REQUIRED`, `CONDITIONAL` | `PRV-006` | `notice_consent_dependency` |
| 8 | DPIA/TIA `RESOLVED`, `NOT_REQUIRED` or `REVIEW_REQUIRED` | `PRV-016` | `dpia_tia_status` |

Blank list entries are findings. Registry codes are stored trimmed and upper-case. References (lawful basis, retention, transfer, processor) are checked for presence only — see [Remaining limitations](#remaining-limitations).

## Activity lifecycle (§9, Figure 4)

```
DRAFT --validate(8 gates)--> VALIDATED --submit--> SUBMITTED --approve--> APPROVED --activate--> ACTIVE
  ^ (successor draft)                                  |                                  |   ^
  |                                                    +--reject(reason)--> REJECTED       |   |
  +---------------------------------------------------------------------------      suspend  resume
                                                                                           v   |
                                                                                       SUSPENDED
                                          ACTIVE / SUSPENDED --retire--> RETIRED
```

- `REJECTED` is terminal (PRV-I20); the fix loop is a **new** version via `POST …/{id}/versions` with `parent_version_id`.
- **Maker-checker:** the creator of a version cannot approve, reject or publish it (`403 SOD_DENIED`).
- **Activation** locks the activity, checks the target is `APPROVED` before changing anything, then retires every previously `ACTIVE` **or `SUSPENDED`** version of that activity at the same instant — each with its own `retired` event and audit row — and activates the target. A partial unique index (`uq_activity_versions_one_active`) makes two ACTIVE versions impossible even for a superuser.
- **Effective time:** activation takes effect at the **server's** instant, recorded as `effective_from`. A caller-supplied `effective_from` is accepted only as "now" within ±60 s; backdating or post-dating is `422 EFFECTIVE_TIME_INVALID`. Scheduled activation needs WFC timers (§2.1). Publishing a purpose clamps an earlier `effective_from` to the publish instant; a deliberate future date is kept.
- **Submit** is a real, audited transition but does not hand the version to a WFC approval workflow (not integrated); approval is a separate, separately-authorized, maker-checker-gated call.

## Bitemporality (§9.1, §20.3)

`GET …/{id}?as_of=T&known_as_of=K` returns the version that governed at `T` (`effective_from ≤ T < effective_to`) **and** that the platform had approved by `K`. Omit both for "what governs now". No governing version: `404 PRV-002` (purposes: `404 PRV-001`). A present-but-malformed `as_of`/`known_as_of` is `400 INVALID_AS_OF` (RFC 3339 required) — never silently replaced by "now". A retired version stops governing at `effective_to`. **Suspension is time-sliced.** The `version_status` of the version a read returns is its status **at the requested instants**, not today's: a version SUSPENDED now is reported ACTIVE for an `as_of` before the suspension (or after a resume) and SUSPENDED for an `as_of` inside it. Suspend/resume are the `SUSPEND`/`RESUME` rows of the append-only `activity_lifecycle_audit`; the service stamps `recorded_at` itself, so it is both the valid-time and the knowledge-time of the change. The last such row with `recorded_at ≤ as_of` **and** `≤ known_as_of` decides (intervals are half-open: `[suspend, resume)`); with none the version was ACTIVE. A suspension recorded after `known_as_of` is therefore not yet known. A read with both instants at/after the database clock (no `as_of`) returns the stored status unchanged. A SUSPENDED version with no lifecycle row at all (suspended before the audit table existed) cannot be sliced and stays SUSPENDED. A RETIRED version reports the status it had inside its window (ACTIVE/SUSPENDED), since it governed then. A suspended instant still resolves the version (`200`, `version_status: SUSPENDED`), exactly as a current read of a suspended activity does; callers (decision, transfer) refuse anything that is not ACTIVE. The specification (§9.1, §20.3, PRV-002) names no suspended-at-`as_of` status code, so the existing 200-with-status contract was kept rather than inventing a new one. No schema change was needed.

## Events — transactional outbox only

The handler publishes nothing. Each state change enqueues its event **inside its own transaction** (`event_outbox`), so a rolled-back change leaves no event and a committed one leaves exactly one. The relay is the only component that talks to Kafka: it retries with exponential backoff (cap 300 s), records each failure durably (`attempts`, `last_error`), requires full broker acknowledgement, and checks that its own bookkeeping updated exactly one row. Delivery is at-least-once; consumers must be idempotent (§19.1).

| Event | Trigger |
|---|---|
| `privacy.purpose.published` | purpose version published |
| `privacy.processing_activity.submitted` / `approved` / `rejected` | the transition |
| `privacy.processing_activity.activated` | activation |
| `privacy.processing_activity.retired` | retire, **and** each version superseded by an activation (`superseded_by_version_id` set) |
| `privacy.processing_activity.suspended` / `resumed` | the transition |

`validate` is audited but is not a §19 event. Activity-event payloads embed the full version (tenant, activity/version ids, effective window, role and purpose scope) plus `previous_status`, `policy_refs` (lawful-basis and special-condition refs of the linked published purposes plus the version's retention, transfer and processor refs) and `superseded_by_version_id` where applicable. Platform events carry an empty tenant.

## Audit evidence

Every lifecycle change also writes an append-only row to `activity_lifecycle_audit` (action, from/to status, **acting principal — mandatory**, correlation id, reason, superseding version, effective window) in the same transaction, so required actor evidence never depends on an event reaching the bus. Approver, rejecter and activator are also stored on the version row. UPDATE and DELETE on the audit table are rejected by trigger.

## Error contract (§32)

Every error is `{"error": "...", "error_code": "..."}`; an activation refusal adds `findings`. §32 codes keep their §32 meaning: `PRV-001` (unknown/unpublished purpose), `PRV-002` (no governing activity version), `PRV-020` (re-publishing an immutable published purpose), plus the finding codes above. The remaining codes are local because §32 names none for them: `AUTHENTICATION_FAILED`, `AUTHORIZATION_DENIED`, `AUTHORIZATION_SERVICE_UNAVAILABLE`, `TENANT_SCOPE_REQUIRED`, `TENANT_SCOPE_MISMATCH`, `PLATFORM_SCOPE_REQUIRED`, `IDEMPOTENCY_KEY_REQUIRED`, `IDEMPOTENCY_MISMATCH`, `CORRELATION_ID_REQUIRED`, `INVALID_TRANSITION`, `SOD_DENIED`, `INVALID_AS_OF`, `EFFECTIVE_TIME_INVALID`, `VALIDATION_FAILED`, `NOT_FOUND`, `METHOD_NOT_ALLOWED`, `STORE_UNAVAILABLE`, `INTERNAL_ERROR`. **Transport-level errors use the same envelope.** An unknown route is `404 NOT_FOUND`, a known path with an unsupported method is `405 METHOD_NOT_ALLOWED` (with an `Allow` header), a panic recovered in a handler is `500 INTERNAL_ERROR` (the panic value and stack go to the log, never to the caller), and a failed `/readyz` is `503 STORE_UNAVAILABLE`. The one boundary: a malformed request that Go's `net/http` server rejects before routing (invalid request line, over-large headers) gets `net/http`'s own plain-text response; that layer is not service code.

---

## Database migrations (`deployments/migrations`)

- `000001_initial_schema` — purposes, purpose versions, processing activities, activity versions.
- `000002_immutability` — triggers: published purposes and non-DRAFT activity content are immutable.
- `000003_add_rls` — row-level security (enabled and forced).
- `000004_activation_gates_and_idempotency` — `notice_consent_dependency`, `dpia_tia_status`, `purpose_registry_idempotency_keys`.
- `000005_fix_gaps` — one-ACTIVE unique index, per-operation RLS policies, approver/rejecter/activator columns, `event_outbox`.
- `000006_scope_audit_and_activation_integrity` — `privacy_owner`, `processor_refs`, `effective_to`, `necessity_rationale`, `special_condition_refs`; `activity_lifecycle_audit`; DELETE-rejecting triggers; platform-capability write policies; tightened `event_outbox` and idempotency policies.

The runtime must connect as a `NOSUPERUSER NOBYPASSRLS` role (compose uses `zoiko_app`); a superuser would bypass every RLS policy described above.

## Local verification

```bash
# Unit + integration tests. The integration tests need PostgreSQL and a superuser URL
# (TEST_DATABASE_URL); each test creates and drops its own database and connects as a
# NOSUPERUSER NOBYPASSRLS role. REQUIRE_DB_TESTS=1 turns an unreachable database into a failure.
REQUIRE_DB_TESTS=1 TEST_DATABASE_URL=postgres://postgres:...@localhost:55432/postgres?sslmode=disable go test ./...

go vet ./...
go build -o server ./cmd/server
go build -o healthcheck ./cmd/healthcheck

curl http://localhost:8151/healthz
curl http://localhost:8151/readyz
```

Latest run (database reachable): **95 top-level tests pass, none skipped** (domain 4, handler 62, store 29). If the database is unreachable the integration tests skip (a run without one proves nothing about RLS, the outbox, atomic idempotency or the migrations). If `TEST_DATABASE_URL` is unset the integration tests fall back to `postgres://postgres:pgsuper@localhost:55432/postgres`. The server aborts on startup if the database is unreachable.

---

## Dependency-blocked items

- **`privacy-consent-svc`, `privacy-decision-svc`, `privacy-transfer-svc`** call `GET /privacy/purposes/{id}` and `GET /privacy/processing-activities/{id}`. Their clients forward the calling request's `X-Tenant-Id` and, when it has one, its `X-Principal-Id` (a call with no principal is refused `401` here); the calls still return `401`/`403` until that principal holds `PRIVACY_PURPOSE_READ` / `PRIVACY_ACTIVITY_READ` in `authorization-svc`, and a service-to-service identity for callers without an end-user principal is not defined by the architecture. Neither is provisioned here; end-to-end behaviour was not verified against live services.
- **`authorization-svc`** must provision `PRIVACY_PURPOSE_READ`, `PRIVACY_ACTIVITY_READ`, `PRIVACY_ROPA_READ` and the platform-scope grant for platform-plane principals; until then reads fail closed with `403`. Platform-scope authorization against the live service was not verified.
- **Frontend** must send the new content fields (`privacy_owner`, `necessity_rationale`, `special_condition_refs`, `processor_refs`), may select the platform scope, and must stop sending a free `effective_from` on activation.
- **WFC (§2.1/§9.1)** owns workflow timers, approval orchestration and escalation; none is integrated.
- **PDC / DRC / PRV-05 catalogues** — jurisdiction-specific legal evaluation and the existence of referenced lawful-basis, retention, transfer and processor records are not verifiable yet.

## Remaining limitations

- *(fixed)* Suspension is now correctly time-sliced for `as_of` historical resolution (see Bitemporality).
- Reference *existence* is not verified, only presence (`CROSS-SERVICE / EXTERNAL DEPENDENCY`). §8.2 gate 4 asks for lawful-basis / special-condition references "from PDC" and gate 6 for retention/DRC references and transfer/processor dependencies to be *identified*; the catalogues that could confirm that a reference is approved, active or even exists (PDC, DRC/retention-registry, PRV-05) are not callable contracts this service may depend on, and duplicating their logic here would invent rules. Presence, non-blank entries and the gate-triggering conditions are enforced.
- `owner` (the accountable business owner) is required when a draft is **created** (`400 VALIDATION_FAILED`), so the gate-1 `owner` finding cannot occur for a version created through the API; `privacy_owner` is checked at validate and again at activate.
- Versions activated before migration 000006 have no `privacy_owner`.

## Spec points carried as clarifications

- Whether the necessity/minimization rationale (§8.2 gate 3) belongs to the purpose version (implemented) or the activity.
- `processor_refs` is this service's representation of §8.2 gate 6's "processor dependencies"; §7 does not name it.
- The gate-to-§32 code mapping and the `*_READ` action names are the service's.
