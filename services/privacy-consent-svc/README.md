# privacy-consent-svc (PRV-02)

Notice, Consent & Preference Evidence Service — `ZS-SVC-W-001` §10-11, §18, §19.
Port `8152`.

This README reflects the actual implementation, not an aspirational one.
Where a documented requirement is only partially met, or depends on another
service, that is stated plainly below rather than in a separate drift
document discovered later.

> **Status: not production ready as an integrated system.** The service's own
> behaviour is covered by tests (see [Verification](#verification)), but three
> things outside it block a safe rollout — see
> [External dependencies](#external-dependencies). The audit write-up is the
> PRV-02 section of `docs/audit_files/PRIVACY.md`.

## Scope

Depends on `privacy-purpose-registry-svc` (PRV-01) for purpose validity:
every `ConsentReceipt.purpose_id` is checked against a real HTTP call, never
trusted as an opaque string. Does not itself enforce runtime data-use
decisions (PRV-03's job) — it is the evidence store PRV-03 queries.

## Endpoints

### Notices

| Method | Path | Notes |
|---|---|---|
| POST | `/privacy/notices` | Create draft identity + version 1 |
| GET | `/privacy/notices/{id}?as_of=` | Resolve the version effective at a time. Tenant-scoped (`X-Tenant-Id` required), no principal required — a published notice is meant to be shown to a subject before any session exists |
| POST | `/privacy/notices/{id}/versions` | Successor draft |
| POST | `/privacy/notices/{id}/approve`, `/publish`, `/withdraw` (canonical, latest version) | The latest version is resolved **after** authorization — see [Request ordering](#request-ordering) |
| POST | `/privacy/notices/{id}/versions/{vid}/approve`, `/publish`, `/withdraw` (explicit version) | |
| POST | `/privacy/notices/{id}/presentation-receipts` (canonical) / `.../versions/{vid}/presentation-receipts` | Evidence a notice version was shown to a subject |

### Consent

| Method | Path | Notes |
|---|---|---|
| POST | `/privacy/consents` | Record a grant/deny receipt |
| GET | `/privacy/consents?subject_ref=&purpose_id=` | Resolve current status. **Authenticated and authorized** (`PRIVACY_CONSENT_READ`) |
| POST | `/privacy/consents/{id}/withdraw` | Subject-initiated terminal act |
| POST | `/privacy/consents/{id}/invalidate` | **Platform**-initiated terminal act (`PRIVACY_CONSENT_INVALIDATE`) — a compromised capture channel or confirmed fraud, never a subject-initiated act. Distinct from withdrawal per §6's state list |

### Preferences

| Method | Path | Notes |
|---|---|---|
| POST | `/privacy/preferences` | |
| GET | `/privacy/preferences?subject_ref=&channel_or_purpose=` | **Authenticated and authorized** (`PRIVACY_PREFERENCE_READ`) |

## Response contract (§18)

§18's critical fields are returned **alongside** the names this service has
always used, with identical values, so existing consumers keep working.

| Route | §18 field | Existing field |
|---|---|---|
| `POST /privacy/consents` | `receipt_id` | `consent_receipt_id` |
| | `purpose_id`, `scope` (when a `consent_request` was supplied) | `purpose_id`, `consent_request.scope` |
| | `notice_version` | `notice_version_id` |
| | `status` (`GRANTED` or `DENIED` — the receipt's own captured outcome) | `action` |
| | `evidence_hash` | `receipt_integrity_hash` |
| `POST /privacy/consents/{id}/withdraw` | `withdrawal_id` | `withdrawal_receipt_id` |
| | `effective_at` (the instant the withdrawal was recorded) | `created_at` |
| | `propagation_correlation` | `propagation_correlation_id` |

`status` on a receipt is **not** the subject's current consent status, which
can later become `WITHDRAWN`, `EXPIRED` or `INVALIDATED`; that is the derived
read `GET /privacy/consents`, whose embedded `latest_receipt` has the same
shape.

## Request ordering

Every route runs, in order: principal (`401`), tenant (`401`), required
request context — `X-Correlation-ID` (`400`) — then the `authorization-svc`
grant (`403`/`503`, fail closed), then the idempotency lookup, and only then
any resource lookup. A caller who is not authorized therefore learns nothing
about whether a notice or receipt exists: for the canonical latest-version
routes and for withdraw/invalidate an existing and a nonexistent id answer
identically (`403`). A receipt the caller's tenant does not own — including
one with no tenant — answers `404`, never `403`.

## Auth

Every mutating route requires `X-Principal-Id`, a verified `X-Tenant-Id`,
`X-Correlation-ID` and `Idempotency-Key` (§18.1 — all four mandatory), plus a
fail-closed `authorization-svc` grant for its own action.
`GetConsentStatus` and `GetPreference` require the same principal + tenant +
grant, because they answer "what did this subject assert". `GetNotice`
requires a verified tenant but no principal, matching the documented ordering
(a notice precedes any consent decision, so it must be readable before a
session exists) — it is tenant-scoped so a notice id cannot be read across
tenants.

A caller sending no `X-Tenant-Id` is refused (401) rather than defaulted to
a platform-wide write.

## Lifecycle states

Notice: `DRAFT → APPROVED → PUBLISHED → WITHDRAWN`, with `SUPERSEDED` as the
side effect of publishing a successor (never a direct action). A superseded
version's `effective_to` is set to its successor's `effective_from` in the
same transaction, closing the §10.1 "effective window."

Consent: derived read, never stored — `NOT_REQUESTED | GRANTED | DENIED |
WITHDRAWN | EXPIRED | INVALIDATED`. It is computed from the **latest**
receipt for (subject, purpose). Precedence when more than one applies:
`INVALIDATED > WITHDRAWN > EXPIRED >` the receipt's own action. `EXPIRED` is
derived from an optional `expires_at`; `INVALIDATED` comes from a
`ConsentInvalidation` record, never the same table or actor as a
`WithdrawalReceipt`.

### Replay versus re-consent (PRV-N04)

A `POST /privacy/consents` returns the **original** receipt (no new evidence,
no event) only while that receipt is still the live outcome of the same
request: it is the latest receipt for the pair, has the same action and
notice version, and still derives to `GRANTED`/`DENIED`. After an
invalidation, an expiry, a withdrawal, or a later opposite decision, the same
request writes a **new** receipt that can become `GRANTED`. Earlier receipts
are never touched.

## Evidence

- `ConsentRequest` (§10.1) is a real row in `consent_requests`, one per
  receipt — scope, data/recipient/technology context, vendor, storage
  lifetime, jurisdiction rule reference, required status, notice version,
  policy package.
- **Receipt integrity (§10.1).** `receipt_integrity_hash` is a SHA-256, scheme
  `v2` (values are prefixed `v2:`), computed once at insert over a canonical,
  length-prefixed encoding of: subject, purpose, notice version, action,
  capture channel, actor, capture time; proxy (flag, representative subject,
  authority, evidence); affirmative-action type and evidence; age band;
  offline-import provenance (flag, source, capture method, evidence reference,
  `imported_at`); `expires_at`; and the whole `ConsentRequest`. Timestamps are
  normalised to UTC microseconds so the digest recomputed from the stored row
  equals the stored digest. Receipt id, tenant, correlation id and the hash
  itself are deliberately outside it. Receipts written before this version
  carry a bare 64-hex digest over a narrower field set (`v1-legacy`); they are
  not rewritten. **No new algorithm, key or HMAC was introduced:** ZS-SVC-W-001
  §33 OD-06 ("consent receipt cryptographic integrity scheme and long-term
  evidence verification policy") is still an *open* decision.
- Age-band evidence (`age_band`) and **offline/imported consent** (§11.1).
  When `is_offline_import` is true the request must carry all four of
  `import_source`, `import_capture_method`, `import_evidence_ref` and
  `imported_at` (the consent's own original time, never in the future) —
  otherwise `400 IMPORT_PROVENANCE_REQUIRED`. Online consent is unaffected.
- `WithdrawalReceipt.propagation_correlation_id` is minted once, at the first
  (non-replay) withdrawal (§10.1).
- `NoticeVersion.published_by_principal_id` / `published_at` record who
  actually published a version — distinct from maker and checker.

## Idempotency & correlation (§18.1)

`Idempotency-Key` and `X-Correlation-ID` are mandatory on every mutating
route (400 if missing), backed by `consent_idempotency_keys`
(RLS-protected). A replay with the same key and body returns the original
response with `Idempotency-Replay: true`; a different body under the same key
returns `409 IDEMPOTENCY_MISMATCH`.

It fails safe:

- **Lookup failure** → `503 STORE_UNAVAILABLE`, and the request is **not
  executed**. It no longer proceeds without replay protection.
- **Save failure after the operation committed** → `500
  IDEMPOTENCY_RECORD_NOT_SAVED` with `committed: true` and the committed
  `result`. It is no longer ignored. A retry with the same key will **not** be
  recognised as a replay; consent and withdrawal/invalidation are naturally
  idempotent (PRV-N04/N05), other creates are not — see
  [Remaining limitations](#remaining-limitations).

## Events & delivery

`privacy.notice.published` and `privacy.consent.changed` are enqueued by the
**store**, inside the same transaction as the evidence row they report
(`internal/outbox`, `event_outbox`) — never after commit. A background relay
drains the outbox with capped exponential backoff until the broker
acknowledges (`RequiredAcks: RequireAll`).

### `privacy.consent.changed` payload (§19)

Emitted for grant, deny, withdraw and invalidate (never for a replay):

| Field | Meaning |
|---|---|
| `subject_ref`, `purpose_id`, `scope` | the subject and purpose; `scope` from the receipt's `ConsentRequest` when present |
| `change_type` | `GRANTED` \| `DENIED` \| `WITHDRAWN` \| `INVALIDATED` |
| `old_state`, `new_state` | the **derived** consent status of the (subject, purpose) pair immediately before and after, both read inside the recording transaction. They can be equal — withdrawing an older receipt does not change what a newer one governs |
| `receipt_id`; `withdrawal_id` + `propagation_correlation` (withdraw); `invalidation_id` (invalidate) | the evidence reference |
| `effective_time` | capture time of a grant/deny — for an offline import its original `imported_at`, never a fabricated current time; the recording time of a withdrawal/invalidation |
| `receipt` / `withdrawal` / `invalidation` | the raw evidence row |

## Tenant isolation

Every evidence table is `FORCE ROW LEVEL SECURITY`, scoped by `app.tenant_id`
set from the gateway-verified `X-Tenant-Id` on every transaction. The runtime
role is `zoiko_app` (not a superuser). **Migration `000006` removed the
`tenant_id IS NULL OR …` exception** that `000003`/`000005` had written into
every policy: a tenant-owned row is now visible to, and writable by, its own
tenant and no one else, including a session with no tenant, and **no
platform-scope exception was added** (§24's "separate governed plane" does not
exist in this service). Every table also gets a `tenant_id IS NOT NULL` CHECK
(`NOT VALID`, so new rows cannot bypass tenant assignment) and `event_outbox`
no longer matches an empty tenant against an empty session.

**Legacy rows:** a row whose `tenant_id` is NULL becomes invisible to the
application role (it is not deleted — evidence is append-only). The migration
raises a `WARNING` with the count per table; giving those rows an owner is a
data decision it does not make.

## Authorization scope

`authz/client.go` forwards the caller's own verified tenant (from request
context) as `X-Tenant-Id` to `authorization-svc` — never a hard-coded value.

## External dependencies

These are **not** implemented by this service, and none is claimed as working.

1. **`privacy-purpose-registry-svc` — consent recording is currently refused.**
   `RecordConsent` calls the registry's `GET /privacy/purposes/{id}` with
   `X-Tenant-Id` only. The registry's read routes now require `X-Principal-Id`
   and a grant (the remediated registry in this repository; whether that
   build is deployed anywhere was not checked), so against that registry the
   call returns `401`, which the
   client reports as "unavailable" and `RecordConsent` answers `503
   UPSTREAM_UNAVAILABLE`. (Pinned by `internal/purposeregistry/client_test.go`.)
2. **`privacy-decision-svc` — consent lookups are refused.** Its consent client
   calls `GET /privacy/consents` with `X-Tenant-Id` only. This service requires a
   principal and `PRIVACY_CONSENT_READ`, so every lookup returns `401` (pinned
   by `TestGap11_…` in `internal/handler/audit_gaps_test.go`). The requirement
   is deliberately **not** relaxed.

   Both 1 and 2 need a **service-to-service identity**. The architecture names
   it as a goal (`docs/architecture/05-security.md` §12.4: "authenticated
   workload identity", "traceable service-principal identity") but defines no
   mechanism — no header scheme, no service-principal provisioning — so none was
   invented here. It is a platform decision, and the callers must be changed to
   present it.
3. **Permission grants are not provisioned by this repository.** The actions
   this service checks — `PRIVACY_NOTICE_CREATE`, `_APPROVE`, `_PUBLISH`,
   `_WITHDRAW`, `PRIVACY_NOTICE_PRESENTATION_RECORD`, `PRIVACY_CONSENT_RECORD`,
   `_WITHDRAW`, `_INVALIDATE`, `PRIVACY_CONSENT_READ`, `PRIVACY_PREFERENCE_SET`,
   `PRIVACY_PREFERENCE_READ` — are evaluated by `authorization-svc`, whose
   grants are runtime data created through its admin API. No migration, seed or
   script in the repository grants any of them (the only RBAC seed script,
   `scratch/seed_transfer_rbac.ps1`, covers privacy-transfer-svc actions for a
   fixed test principal). Earlier documentation said `PRIVACY_FULL` contained
   them; that was not verifiable and has been removed. Until an operator
   grants them, the routes fail closed with `403`.
4. **Workforce consent (§11.1) is not implemented here, by design.** The spec
   says "flag contexts **where policy says** consent may be inappropriate or
   invalid due to imbalance; require alternate approved basis or review"; the
   decision-time rule (PRV-N10: "employee consent used where **PDC** flags
   imbalance concern → REVIEW/BLOCK") belongs to privacy-decision-svc with the
   PDC policy. Nothing in this service can know a PDC flag, and inventing a
   flag or an "alternate basis" here would fabricate a control. This service
   makes no claim to implement it.
5. **§2.1 WFC boundary.** Notice approve/publish/withdraw are handled entirely
   within this service (maker-checker enforced locally), while §2.1 lists
   "workflow timers, maker-checker, approvals and escalation" as WFC's
   authority. No WFC integration exists in this codebase.
6. **Representative authority verification (§11.1)** — `is_proxy` requires the
   representative subject and authority references to be *present*; nothing
   verifies the authority reference. No verification service exists to call.
7. **Dark-pattern UI rules (§10.2)** are UI-layer concerns this backend cannot
   enforce; it only guarantees the API does not pre-select or mislabel.

## Remaining limitations

- The idempotency record is saved **after** the operation commits, not in the
  same transaction. A crash between the two (or a save failure, now reported)
  leaves a retry unprotected for operations that are not naturally idempotent
  (e.g. creating a notice).
- `PreferenceAssertion` has no effective time separate from its recording time
  (§10.1 names one).
- `scope` appears on a consent response only when the caller supplied a
  `consent_request`.
- Receipts and legacy rows written before migration `000006` / the `v2`
  digest are described above; they are not migrated.

## Verification

```bash
# Unit + integration tests. The integration tests need PostgreSQL and a
# SUPERUSER url (TEST_DATABASE_URL); each test creates and drops its own
# database and connects as a NOSUPERUSER NOBYPASSRLS role.
# REQUIRE_DB_TESTS=1 turns a missing database into a FAILURE rather than a skip.
REQUIRE_DB_TESTS=1 TEST_DATABASE_URL='postgres://postgres:...@localhost:55432/postgres?sslmode=disable' go test ./...
go vet ./...
go build ./...
```

With the database: 114 top-level tests pass, none skipped (+ subtests).
**Without** `TEST_DATABASE_URL`: 88 pass and **26 skip** — the row-level
security, outbox, notice-binding and store-level evidence tests are among the
skipped, so a run without the database does **not** prove tenant isolation.

## Migrations

- `000001_initial_schema` — notices, consent/withdrawal/presentation receipts, preferences.
- `000002_immutability` — append-only triggers on every evidence table.
- `000003_add_rls` — tenant isolation (superseded in part by `000006`).
- `000004_idempotency_and_evidence_extensions` — proxy/affirmative-action fields, `consent_idempotency_keys`.
- `000005_security_and_evidence_gaps` — notice publisher evidence + effective-window end; `consent_requests`; `consent_invalidations`; integrity, age-band, offline-import and expiry columns; `withdrawal_receipts.propagation_correlation_id`; `event_outbox`.
- `000006_strict_tenant_isolation` — removes the `tenant_id IS NULL OR` exception from every policy, adds `tenant_id IS NOT NULL` CHECKs (`NOT VALID`), tightens the `event_outbox` policy, warns about legacy NULL-tenant rows.
