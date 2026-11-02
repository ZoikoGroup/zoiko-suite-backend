# reconciliation-intelligence-svc: comparison, remaining deep-suite failures, and recommended plan

Written for: satyaprakash (service owner) and the Blocks 6–10 owner (vasudevareddy-zoiko). Status 2026-11-02, branch `fix/blocks-6-10-final` (uncommitted).

## 1. What is in the branch
- **Base:** satyaprakash's upstream service (origin/main, including A2a, A5 and `000007_force_rls` from PR #243). The Block 8 rewrite that vasudevareddy started was **not** ported, because it conflicts with A5 (`authoritative_ref` object vs a string, both using migration 000007).
- **Changes on top of upstream (each has tests):**
  1. `job_name` over 128 characters used to return 500 (VARCHAR(128) INSERT failure); it is now 400.
  2. `GET /v1/reconciliations` was unbounded (every job). It now pages with `limit` 1..200 (default 50) and `offset`, using LIMIT/OFFSET in SQL. The response adds `limit` and `offset`.
  3. An unknown `status` or `source_system_a` filter now returns 400 instead of an empty 200.
  4. A malformed item id used to come back as 404 **carrying the raw Postgres error** (`invalid input syntax for type uuid … SQLSTATE 22P02`). It is now 400, and store errors are logged, never returned (`domain.ErrItemNotFound`).
  5. A decision on an **ARCHIVED** job is now 409 (archived suggestions are the audit trail). It used to succeed.
  6. An analyze body over 64 KiB now returns 413 instead of 400.
  7. Migration `000009_rls_force_with_check.down` no longer removes the FORCE that `000007_force_rls` (main) set.
- **Tests:** 132 passed, 0 failed, 0 skipped against Postgres 16 as a NOSUPERUSER role.
- **Deep suite:** 19/93 both before and after fixes 4–6. Those three checks now get the correct status code (400, 409, 413) but still fail the suite's problem+json body assertion. The classification below was made before fixes 4–6, so three of its four "actual implementation defect" rows (malformed item id, decision on an archived job, body over 64 KiB) are now **fixed** and fail only on body format. Only the SoD defect remains.

## 2. The remaining deep-suite failures
The deep suite (`E:/zoiko-e2e/e2e_intel_recon.py`) was written for the unported rewrite, so most failures assert that rewrite's contract, not upstream's.

Current code = satyaprakash's upstream (origin/main) + 3 targeted fixes (job_name bound, bounded list, filter validation).

## Actual implementation defect: 4

- `body over 64 KiB -> 413` — Low: oversized body answers 400, not 413
- `SoD: analysis requester cannot APPROVE its own suggestion -> 403` — High: requester can approve its own suggestion (no segregation of duties)
- `apply malformed item id -> 400` — Medium: malformed item id leaks the raw Postgres error (SQLSTATE 22P02) in a 404 body
- `decision on an archived job -> 409` — Medium: suggestions on an ARCHIVED job can still be decided (archive does not freeze the audit record)

## Cascade (fails only because an earlier step failed): 9

- `SoD: analysis requester may REJECT (no effect on records) -> 200` — suggestion was already APPROVED by the failed SoD step
- `reviewer APPROVES suggestion -> 200` — suggestion was already APPROVED by the failed SoD step
- `EXECUTED is final -> 409` — depends on the EXECUTED step
- `malformed If-Match -> 400` — suggestion was already APPROVED by the failed SoD step
- `missing suggestion_status -> 400` — suggestion was already APPROVED by the failed SoD step
- `missing resolution_notes (reason required) -> 400` — suggestion was already APPROVED by the failed SoD step
- `authoritative_reference with APPROVED -> 400` — suggestion was already APPROVED by the failed SoD step
- `unknown field -> 400` — suggestion was already APPROVED by the failed SoD step
- `apply replay with same Idempotency-Key -> same 200, not 409` — suggestion was already APPROVED by the failed SoD step

## Design decision requiring owner approval: 5

- `same Idempotency-Key with a different body -> 422` — 409 (platform idempotency template) vs 422
- `stale If-Match -> 412` — optimistic concurrency (If-Match/ETag) on decisions
- `job version advanced by each decision` — job versioning
- `archive with stale If-Match -> 412` — optimistic concurrency on archive
- `archive an archived job -> 409` — archive idempotent (200) vs 409

## API contract difference (rewrite's contract vs upstream): 54

- `analyze run -> 201 job with suggestions` — no version/requested_by fields
- `body tenant_id equal to X-Tenant-Id accepted` — upstream rejects a body tenant_id (header is authoritative)
- `analyze without X-Tenant-Id -> 401` — status code correct; body is {error}, not RFC 9457 problem+json
- `analyze without X-Principal-Id -> 401` — status code correct; body is {error}, not RFC 9457 problem+json
- `analyze without Idempotency-Key -> 400` — status code correct; body is {error}, not RFC 9457 problem+json
- `analyze without X-Correlation-ID -> 400` — status code correct; body is {error}, not RFC 9457 problem+json
- `body tenant_id disagreeing with X-Tenant-Id -> 403` — upstream rejects a body tenant_id (header is authoritative)
- `malformed JSON -> 400` — status code correct; body is {error}, not RFC 9457 problem+json
- `empty body -> 400` — status code correct; body is {error}, not RFC 9457 problem+json
- `missing job_name -> 400` — status code correct; body is {error}, not RFC 9457 problem+json
- `missing legal_entity_id -> 400` — status code correct; body is {error}, not RFC 9457 problem+json
- `bank_reconciliation_run_id not a UUID -> 400` — status code correct; body is {error}, not RFC 9457 problem+json
- `job_name over 128 chars -> 400 (not a DB error)` — status code correct; body is {error}, not RFC 9457 problem+json
- `caller-supplied transactions refused (unknown field) -> 400` — status code correct; body is {error}, not RFC 9457 problem+json
- `unknown bank-reconciliation run -> 404` — status code correct; body is {error}, not RFC 9457 problem+json
- `superseded run (frozen population replaced) -> 409` — status code correct; body is {error}, not RFC 9457 problem+json
- `another legal entity's run -> 403` — status code correct; body is {error}, not RFC 9457 problem+json
- `no governed tolerance policy -> 424, never a default` — status code correct; body is {error}, not RFC 9457 problem+json
- `list without legal_entity_id -> 400` — status code correct; body is {error}, not RFC 9457 problem+json
- `list with unknown status -> 400` — status code correct; body is {error}, not RFC 9457 problem+json
- `list with unknown source_system_a -> 400` — status code correct; body is {error}, not RFC 9457 problem+json
- `list limit=0 -> 400` — status code correct; body is {error}, not RFC 9457 problem+json
- `list limit=500 (over max 200) -> 400` — status code correct; body is {error}, not RFC 9457 problem+json
- `list offset=-1 -> 400` — status code correct; body is {error}, not RFC 9457 problem+json
- `list without X-Tenant-Id -> 401` — status code correct; body is {error}, not RFC 9457 problem+json
- `list without X-Principal-Id -> 401` — status code correct; body is {error}, not RFC 9457 problem+json
- `get job -> 200 with suggestions and version` — no version/requested_by fields
- `get job of another tenant -> 404 (hidden)` — status code correct; body is {error}, not RFC 9457 problem+json
- `get unknown job id -> 404` — status code correct; body is {error}, not RFC 9457 problem+json
- `get malformed job id -> 400` — malformed id answers 404 instead of 400
- `get without X-Tenant-Id -> 401` — status code correct; body is {error}, not RFC 9457 problem+json
- `get without X-Principal-Id -> 401` — status code correct; body is {error}, not RFC 9457 problem+json
- `applying the same decision twice -> 409` — status code correct; body is {error}, not RFC 9457 problem+json
- `approving a REJECTED suggestion -> 409` — status code correct; body is {error}, not RFC 9457 problem+json
- `EXECUTED straight from RECOMMENDED -> 409` — status code correct; body is {error}, not RFC 9457 problem+json
- `conflicting suggestion_status / resolution_status -> 400` — status code correct; body is {error}, not RFC 9457 problem+json
- `unknown suggestion_status -> 400` — status code correct; body is {error}, not RFC 9457 problem+json
- `a human cannot set RECOMMENDED -> 400` — status code correct; body is {error}, not RFC 9457 problem+json
- `apply malformed JSON -> 400` — status code correct; body is {error}, not RFC 9457 problem+json
- `apply malformed job id -> 400` — malformed id answers 404 instead of 400
- `apply unknown suggestion -> 404` — status code correct; body is {error}, not RFC 9457 problem+json
- `apply unknown job -> 404` — status code correct; body is {error}, not RFC 9457 problem+json
- `apply to another tenant's job -> 404` — status code correct; body is {error}, not RFC 9457 problem+json
- `apply without Idempotency-Key -> 400` — status code correct; body is {error}, not RFC 9457 problem+json
- `apply without X-Principal-Id -> 401` — status code correct; body is {error}, not RFC 9457 problem+json
- `apply without X-Tenant-Id -> 401` — status code correct; body is {error}, not RFC 9457 problem+json
- `archive malformed id -> 400` — malformed id answers 404 instead of 400
- `archive unknown job -> 404` — status code correct; body is {error}, not RFC 9457 problem+json
- `archive another tenant's job -> 404` — status code correct; body is {error}, not RFC 9457 problem+json
- `archive without Idempotency-Key -> 400` — status code correct; body is {error}, not RFC 9457 problem+json
- `archive without X-Principal-Id -> 401` — status code correct; body is {error}, not RFC 9457 problem+json
- `archive without X-Tenant-Id -> 401` — status code correct; body is {error}, not RFC 9457 problem+json
- `archive job (If-Match "1") -> 200 ARCHIVED (soft delete)` — no version/requested_by fields
- `archived job still readable with its suggestions (audit trail)` — no version/requested_by fields

## Outdated test: 2

- `EXECUTED without authoritative_reference -> 400` — suite sends authoritative_reference (string); upstream A5 contract is authoritative_ref {object}
- `EXECUTED with the owning domain reference -> 200` — suite sends authoritative_reference (string); upstream A5 contract is authoritative_ref {object}

## Missing fixture/dependency: 0


TOTAL 74

## 3. Recommended plan (needs satyaprakash's agreement before implementation)

| # | Item | Type | Recommendation | Effort |
|---|---|---|---|---|
| 1 | Requester can APPROVE its own suggestion | **Defect (High, SoD)** | Store `requested_by` on the job (new migration, next free number). Refuse APPROVE/EXECUTED when principal == requester (403); REJECT stays allowed. | S–M |
| 2 | RFC 9457 problem+json errors | Contract | Adopt the platform `apicontract` problem writer, as carta, key-management and compliance-risk-scoring already do. Status codes are already right; only the body shape changes, which is breaking for clients that parse `{error}`. | M |
| 3 | Job/suggestion versioning + If-Match (412/428) | Owner decision | Add `version` to jobs and suggestions and enforce If-Match on decisions and archive; this prevents lost updates between two reviewers. | M |
| 4 | Malformed job id → 400 | Contract | Validate UUID path params (as the item id now is). | S |
| 5 | Archive an archived job → 409 vs 200 | Owner decision | Either is defensible; pick one and document it. | S |
| 6 | Body `tenant_id` | Contract | Keep upstream behaviour (header is authoritative); update the suite. | — |
| 7 | Idempotency mismatch 409 vs 422 | Owner decision | Keep 409 (platform template used by 30+ services); update the suite. | — |
| 8 | Suite sends `authoritative_reference` (string) | Outdated test | Update the suite to A5's `authoritative_ref` object. | S |

**Test plan:**
- Unit tests per item, with red/green proof.
- Postgres integration as the restricted role.
- Re-run `e2e_intel_recon.py` after updating its outdated and contract-difference expectations to the agreed contract.
- Live run through the gateway.
