# privacy-decision-svc (PRV-03)

Port: `8153`  
Specification: `ZS-SVC-W-001` §§12, 13, 13.1, 13.2, 18, 18.1, 19, 24, 32 (the specification, not this README, is the source of truth)  
Classification: Interlinked (calls `privacy-purpose-registry-svc`, `privacy-consent-svc`, `retention-registry-svc`, `privacy-transfer-svc`)

> **Status.** The service-owned gaps from the latest audit are fixed in code and tests; a number of behaviours depend on capabilities that **other services do not provide** and are listed under *Blocked external dependencies*. **Production readiness: NOT PRODUCTION READY — BLOCKED BY EXTERNAL DEPENDENCY** (service-to-service identity, purpose-registry/PDC capabilities, and a data-migration mapping if legacy rows exist). No compliance percentage is claimed here.

---

## Overview

`privacy-decision-svc` returns runtime purpose-binding decisions (`PERMIT`, `RESTRICT`, `BLOCK`, `REVIEW_REQUIRED`, `INDETERMINATE`) for proposed personal-data operations and persists each one as append-only evidence. Every unknown, unrecognised or unavailable input resolves to a non-`PERMIT` result (Invariant I13).

---

## API

- `POST /privacy/decisions` — evaluate and persist (alias `POST /v1/privacy/decisions`).
- `GET /privacy/decisions/{id}` — read stored evidence (alias `/v1/...`). Cross-tenant ids return `404`.
- `GET /healthz`, `GET /readyz`.

### Request headers (POST)

| Header | Required | Failure |
|---|---|---|
| `X-Principal-Id` | yes | `401 AUTHENTICATION_FAILED` |
| `X-Tenant-Id` | yes | `400 TENANT_SCOPE_REQUIRED` |
| `X-Correlation-ID` | yes | `400 CORRELATION_ID_REQUIRED` |
| `Idempotency-Key` | yes | `400 IDEMPOTENCY_KEY_REQUIRED` |

`GET` requires `X-Principal-Id` and `X-Tenant-Id`. `X-Correlation-ID` is **echoed in the response header** on every POST response (first response, replay) once it has been validated, and on GET when supplied. A replay echoes the correlation id of the *current* request; the stored decision keeps the original.

### Error contract

All errors are `{"error": "<message>", "error_code": "<code>"}`.

| Condition | Status | `error_code` |
|---|---|---|
| Missing principal | 401 | `AUTHENTICATION_FAILED` |
| Missing tenant | 400 | `TENANT_SCOPE_REQUIRED` |
| Body `tenant_id` ≠ `X-Tenant-Id` | 403 | `TENANT_SCOPE_MISMATCH` |
| Missing correlation id | 400 | `CORRELATION_ID_REQUIRED` |
| Missing idempotency key | 400 | `IDEMPOTENCY_KEY_REQUIRED` |
| Unreadable/invalid body, missing `subject_ref`/`processing_activity_id`/`purpose_id`, unknown `proposed_operation` | 400 | `VALIDATION_FAILED` |
| Same principal+key reused with a different payload | 409 | `PRV-020` (immutable-evidence conflict) |
| Idempotency lookup failed, or the atomic decision+idempotency commit failed (nothing persisted; safe to retry) | 503 | `STORE_UNAVAILABLE` |
| Response could not be rendered | 500 | `INTERNAL_ERROR` |
| Decision not found (or other tenant) | 404 | `NOT_FOUND` |

The specification does not define transport status codes; these are service decisions. The earlier "idempotency record saved after commit → `IDEMPOTENCY_RECORD_NOT_SAVED` / `IDEMPOTENCY_SAVE_FAILED`" outcome **no longer exists**: decision and idempotency record are written in one transaction.

### Idempotency (§18.1)

- Scope is **(tenant, principal, `Idempotency-Key`)**. The specification does not state the scope; it was chosen because §18.1 requires an actor on every decision and a replay must never return a decision attributed to a different principal. (Needs-clarification item.)
- The key is claimed, the decision inserted and the replayable response stored **in a single transaction**. If any step fails nothing is persisted, so a retry re-evaluates and produces exactly one decision. Concurrent requests with the same key produce exactly one decision; the others replay it.
- Replay returns the original body with `Idempotency-Replay: true`.

### Response (§18)

```json
{
  "decision_id": "uuid",
  "result": "PERMIT | RESTRICT | BLOCK | REVIEW_REQUIRED | INDETERMINATE",
  "constraints": [],
  "reason_codes": ["PRV-xxx: NAME"],
  "policy_refs": ["…"],
  "evidence_ref": "prv-dec:<decision_id>",
  "expires_at": "RFC3339 or omitted",
  "context_hash": "v2:<64 hex>",
  "input_fingerprint": "v2:<64 hex>",
  "activity_version_id": "…", "purpose_version_id": "…",
  "consent_receipt_id": "…", "notice_version_id": "…", "legal_hold_id": "…", "transfer_decision_id": "…",
  "decided_at": "RFC3339", "actor_principal_id": "…", "correlation_id": "…"
}
```

Formats (the specification does not prescribe them; this is what the code emits and the database stores):

- `evidence_ref` = `prv-dec:<decision_id>`. (Earlier documentation showed a `urn:zoiko:privacy:decision:evidence:…` form that the code never produced.)
- `input_fingerprint` = `context_hash` = `v2:` + SHA-256 hex of the normalized input. Decisions written before this scheme carry a bare 64-hex digest over a narrower input set; they are left as stored evidence. (Earlier documentation showed `sha256:…`.)
- `reason_codes` entries are `PRV-nnn: NAME`.

### Input fingerprint (§13.2)

Covers tenant, subject, activity and **requested** activity/purpose version ids, purpose, operation, secondary purpose, free-text secondary purpose, subject/data/recipient context, `consent_check`, `legal_hold_check`, `transfer_check` and the de-identification control. Enum-like values are trimmed and upper-cased; category and flag lists are de-duplicated and sorted. A reflection test fails if a request field is added without being covered.

### `expires_at`

The specification defines no default lifetime, so none is invented. `expires_at` is the **earliest expiry of the upstream evidence the decision relied on**: the consent receipt's `expires_at` (when consent was required) and the transfer decision's `expires_at` (when a transfer was evaluated). If neither exists the field is omitted. It is persisted, returned on GET and preserved on replay. A consent receipt already expired yields `BLOCK` `PRV-006`. A default TTL is a needs-clarification item.

---

## Evaluation sequence

1. **Activity (exact version).** With `activity_version_id` the registry's exact-version route is used; an unknown, non-`ACTIVE` or other-activity version → `BLOCK` `PRV-002`; a registry reply for a different version → `INDETERMINATE`. Without it the current `ACTIVE` version is used. Registry error → `INDETERMINATE` `PRV-019`.
2. **Purpose binding.** Purpose must be bound to the activity, else `BLOCK` `PRV-009`; must be `PUBLISHED`, else `BLOCK` `PRV-001`. The registry only resolves the current purpose version, so a requested `purpose_version_id` that is not the resolvable one → `INDETERMINATE` (never silently replaced by the current one).
3. **Secondary purpose (I14, N02).** Free-text `proposed_secondary_purpose` → `BLOCK` `PRV-001` and is never stored as a purpose id. A registered `secondary_purpose_id` must be `PUBLISHED` (else `BLOCK` `PRV-001`) and bound to the activity (else `BLOCK` `PRV-009`); even then the result is at least `REVIEW_REQUIRED` `PRV-009`, because the compatibility/policy evaluation (PDC) is not available.
4. **Policy refs.** Lawful-basis and special-condition refs of the purpose version → `policy_refs`; none → `INDETERMINATE` `PRV-005`.
5. **Jurisdiction (N12).** Missing/`UNKNOWN` → `INDETERMINATE` `PRV-004`; not covered by the activity (or `GLOBAL`) → `BLOCK` `PRV-004`.
6. **Registered scope (I13, I15).** The caller's declarations are checked against the *registered* activity:
   - `subject_class` must be supplied (else `INDETERMINATE`), `UNKNOWN` → `REVIEW_REQUIRED`, and must be in the activity's `subject_classes` (else `BLOCK` `PRV-009`); an activity with no registered classes → `INDETERMINATE`.
   - Requested `data_categories`/`data_category` must be a subset of the activity's `data_categories` (else `BLOCK` `PRV-010`); no registered categories → `INDETERMINATE`; categories outside the known taxonomy → `REVIEW_REQUIRED`.
   - Sensitivity is derived from the **registered ∪ requested** categories plus caller flags; a caller can add review but never remove it (a sensitive registered category omitted by the caller still yields `REVIEW_REQUIRED` `PRV-010`).
   - Unsupported `age_band`, `classification`, `source`, `sensitivity_flags` or `recipient_type` → `INDETERMINATE`; `UNKNOWN` age band/classification → `REVIEW_REQUIRED`.
7. **Assessment.** DPIA/TIA `RESOLVED`/`NOT_REQUIRED` pass; `REVIEW_REQUIRED`/`UNDER_REVIEW` → `REVIEW_REQUIRED` `PRV-016`; anything else → `INDETERMINATE`.
8. **De-identification (§13.1).** `ANONYMIZE` or classification `ANONYMOUS` without `deidentification_control_ref` → `BLOCK` `PRV-010`.
9. **Consent.** Required when `consent_check.required` or the activity's `notice_consent_dependency` is `REQUIRED` **or `CONDITIONAL`** (the condition is policy-owned, so it fails closed); `NOT_REQUIRED` skips; any other value → `INDETERMINATE`. `EMPLOYEE` subjects → `REVIEW_REQUIRED` `PRV-010` (N10 — the service cannot read a PDC flag, so consent-dependent employee processing is always reviewed). Status `GRANTED` requires receipt evidence (else `INDETERMINATE`) and, if the caller asserted a `consent_check.receipt_id`, it must be the latest (else `BLOCK` `PRV-006`); `WITHDRAWN` → `BLOCK` `PRV-007`; everything else → `BLOCK` `PRV-006`.
10. **Legal hold / retention (I19, N25).** The caller supplies `legal_hold_check.record_class` (this service never derives one; the earlier use of `retention_rule_refs` as record classes was removed). `DELETE` and `ANONYMIZE` without it → `INDETERMINATE` (`PRV-014`, `PRV-019`). With it: registry error / empty reply / inconsistent state (hold matched but not blocked) → `INDETERMINATE`; `blocked` → `BLOCK` `PRV-014` for every operation except `RETAIN` (a hold requires retention; the `legal_hold_id` is recorded and processing continues).
11. **Transfer (§13 step 4).** `EXPORT`, foreign `DISCLOSE` or any `transfer_check` triggers it; no `transfer_check` → `BLOCK` `PRV-015`. Result `AUTHORIZED` → continue; `CONDITIONAL` → `RESTRICT` with a `RECIPIENT_LIMITATION` constraint; `BLOCKED` → `BLOCK` `PRV-015`; `REVIEW_REQUIRED` → `REVIEW_REQUIRED` `PRV-016`; empty/unknown result, empty reply, downstream error or no configured client → `INDETERMINATE`.
12. **Review gate.** `TRAIN_MODEL` (§13.1/N50/PRV-C19), a registered secondary purpose, a `MINOR` subject class/age band, `RESTRICTED` classification, or sensitive data → `REVIEW_REQUIRED` (`PRV-009` / `PRV-010`). **`TRAIN_MODEL` is never auto-permitted**: the registry exposes no model-training attribute or permitted-operation list, and the specification requires an explicit, human-approved policy, so a "separately registered and permitted" training purpose cannot be verified here.
13. Otherwise `RESTRICT` (constraints present) or `PERMIT`.

---

## Blocked external dependencies (not implemented here on purpose)

| Item | Missing capability | Owner |
|---|---|---|
| Verified dedicated training purpose for `TRAIN_MODEL`; human-approved AI policy (PRV-C19) | purpose registry has no model-training/permitted-operations attribute; no PDC | purpose-registry / PDC |
| Secondary-purpose compatibility evaluation (I14) | `compatibility_class` is free-form data, no evaluation engine | PDC / purpose-registry |
| Exact **historical purpose-version** resolution (I29) | registry has `as_of` lookup only, no exact purpose-version route | purpose-registry |
| As-of (effective/knowledge-time) consent, hold and transfer state | those services expose current state only | consent / retention / transfer |
| Retention-period conformance for `RETAIN`/`DELETE` (DRC) | retention-registry resolve returns hold blocks only | retention-registry / DRC |
| Employee/imbalance policy flag (N10), conditional consent rule | PDC | PDC |
| Service-to-service identity / downstream RBAC grants | platform decision (`docs/architecture/05-security.md` §12.4 lists goals, no mechanism); callers forward the end-user principal | platform / authorization-svc |
| Legacy `privacy_decisions` rows with `tenant_id IS NULL` | authoritative tenant mapping (see Migrations) | operator |

Needs clarification: authorization-svc permission for `POST`/`GET` (the specification names none, so none is enforced); default `expires_at` lifetime; identifier formats; idempotency principal scope; drift between the local data-category taxonomy (`internal/domain/taxonomy.go`, copied from the registry because no endpoint exposes it) and the registry's.

Not service-owned: the admin workbench frontend (must send `X-Correlation-ID`); provisioning of PDC.

---

## Migrations

- `000001`–`000004`: base table, append-only trigger, RLS, §12.1/§13.2 columns, idempotency table.
- `000005_strict_tenant_isolation`: **never deletes or rewrites a row.** Adds `CHECK (tenant_id IS NOT NULL) NOT VALID` (rejects every new NULL-tenant row), replaces the policy with the strict `tenant_id::text = NULLIF(current_setting('app.tenant_id', true), '')`, adds `policy_refs`/`evidence_ref`/`expires_at`, re-asserts `FORCE ROW LEVEL SECURITY`. If no NULL-tenant row exists it also sets `tenant_id NOT NULL` and validates the constraint. If legacy NULL-tenant rows exist they are **left in place** (append-only evidence), are invisible to the application role under strict RLS, a `WARNING` with the count is raised, and `tenant_id` stays nullable: **BLOCKED — DATA MIGRATION REQUIRES AUTHORITATIVE TENANT MAPPING.** No tenant is guessed. (An earlier version ran `DELETE … WHERE tenant_id IS NULL`, which conflicted with the append-only trigger.)
- `000006_idempotency_principal_scope`: adds `principal_id` to `decision_idempotency_keys` (backfilled from the stored response's `actor_principal_id`; rows without one stay `''` and are inert) and makes the key `(tenant_id, principal_id, idempotency_key)`.

---

## Events

`privacy.decision.evaluated` is **not** emitted. §19 lists no `privacy.decision.*` event, decisions are durable evidence in `privacy_decisions` (§13.2), and nothing in the repository consumes such an event. The Kafka publisher was removed; the `KAFKA_*` configuration keys remain parsed but unused.

---

## Running tests

```bash
go test ./...          # DB-backed tests SKIP without TEST_DATABASE_URL
go vet ./... && go build ./...
```

Real PostgreSQL (a throwaway database per test; the app connects as a `NOSUPERUSER NOBYPASSRLS` role):

```powershell
$env:TEST_DATABASE_URL="postgres://postgres:pgsuper@localhost:55432/postgres?sslmode=disable"
$env:REQUIRE_DB_TESTS="1"   # fail instead of skip when no database
go test -count=1 -v ./...
```

Latest run from this change (64 top-level tests: domain 5, handler 44, store 13, consent client 2): **with** the database 64 pass, 0 skipped; **without** it 49 pass and 15 are skipped (13 store + 2 handler DB tests) — a run without a database does **not** prove tenant isolation, atomicity or the migrations. Not performed: Docker rebuild, run against the live compose stack, frontend check, live downstream calls (downstream services are stubbed).
