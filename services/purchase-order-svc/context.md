# Purchase Order Service — Context

Compiled from `docs/architecture/03-microservices.md` §12.9 and §12.8,
`docs/architecture/04-data-model.md` §14.1–14.3, and
`services/purchase-request-svc/internal/domain/types.go`'s own doc comment,
which explicitly names this service as the intended handoff target for an
APPROVED purchase request ("It does NOT own purchase orders — that's a
separate, not-yet-built service (Purchase Order Service) this one hands off
to via events."). This file has no independent authority — if it ever
disagrees with the source docs, the docs win.

## 1. What it is

**Service Class:** Commercial Ops / Finance-adjacent domain service.
**Tier:** 1 (Core Operational Execution) — extends the Phase 3 Finance
Engine's procurement chain (`purchase-request-svc` → **purchase-order-svc**
→ `accounts-payable-svc`), not part of the Tier 0 Governance Plane itself.
**Naming convention:** `-svc` suffix → `purchase-order-svc`.
**Port:** `8129` (see §9).

**Purpose** (`03-microservices.md` §12.9, verbatim):
> Owns purchase-order issuance, amendment, and fulfillment-linked state.

**Published Events** (§12.9, verbatim): `purchase.order.issued`,
`purchase.order.amended`, `purchase.order.closed`.

## 2. Doctrine context

Same platform-wide invariants as every other Phase 3 service
(`.agents/rules/doctrine.md`): no self-authorization (every mutation is
checked against `authorization-svc`'s `/v1/authorize`, fail-closed on
unreachability); every state-changing API is idempotent; no soft-delete
(status transitions + an append-only amendment ledger, never a destructive
UPDATE on `total_amount`); every material record carries `tenant_id` +
`legal_entity_id`.

**A specific doctrine choice made here, and why:** a full audit of the
platform (2026-07-23) found that all 10 Phase 5 services wire an
`authz.Client` into their handlers but never actually call `.Authorize(...)`
on any route — the client is dead code. This service is built the other
way deliberately: every mutating handler (`Issue`, `Amend`, `Close`) calls
`h.authz.CheckAllowed(...)` before touching the store, mirroring
`purchase-request-svc`'s and `bank-reconciliation-svc`'s actually-correct
pattern, not the Phase 5 anti-pattern. See `[[zoikosuite-known-issues]]`
(project memory) for the fuller finding.

## 3. Ownership boundary

**Owns:** `PurchaseOrder` (issuance, amendment, close) and
`PurchaseOrderAmendment` (an append-only version-lineage ledger, one row per
amendment — mirrors `employment-contracts-svc`'s `ContractAmendment`
pattern rather than destructively overwriting `total_amount` in place).

**Explicitly does not own:**
- `PurchaseRequest` — owned by `purchase-request-svc`. This service only
  *reads* an approved request (via `GET /v1/purchase-requests/{id}`) to
  verify a caller-supplied `purchase_request_id` before letting it seed a
  PO. It never writes to that service.
- `VendorProfile` — named in `04-data-model.md` §14.1
  (`PurchaseOrder.vendor_profile_id`) but the owning service (Vendor Due
  Diligence Service) doesn't exist yet (see the pending-services list in
  project memory `[[project-zoikosuite-overview]]`). `vendor_profile_id` is
  stored as a plain optional string, unvalidated — same documented-gap
  posture `accounts-payable-svc` took for its own "no vendor-master
  dependency" gap and `counterparty-management-svc` took for its
  unvalidated `jurisdiction_id`/`tax_id` fields. Not silently ignored:
  called out explicitly here and in §10.
- Fulfillment/receiving (goods-receipt matching) — `03-microservices.md`
  §12.9 says this service owns "fulfillment-linked state," which this v1
  reads narrowly as the terminal `CLOSED` status reachable via an explicit
  `POST /{id}/close` call, not a three-way-match receiving workflow. No
  spec section describes a Goods Receipt entity or service, so building one
  here would be scope invention, not implementation.

## 4. API surface

**Consumed events:** none. Verification of an upstream `purchase_request_id`
is done synchronously via HTTP GET at Issue time (§6), not via a Kafka
consumer — this matches every other Phase 3 service's cross-service
verification pattern (`accounts-receivable-svc` → `general-ledger-svc`,
`bank-reconciliation-svc` → `general-ledger-svc`), none of which consume
each other's Kafka events to auto-create records.

**Published events:** `purchase.order.issued`, `purchase.order.amended`,
`purchase.order.closed` (§12.9, verbatim) — envelope shape identical to
every other producer in this platform (see `internal/events/publisher.go`).

## 5. Evidence & idempotency

`POST /v1/purchase-orders` requires a caller-supplied `correlation_id` and
is idempotent on `(tenant_id, correlation_id)` — a genuine improvement over
most Phase 3/4/5 siblings, which the 2026-07-23 audit found store
`correlation_id` but never unique-constrain it. `INSERT ... ON CONFLICT
(tenant_id, correlation_id) DO NOTHING`, then re-select on conflict —
mirrors `governance-decision-log-svc`'s/`audit-event-store-svc`'s
append-only idempotent-insert pattern, applied here to a mutable-but-append-
history entity instead of a pure append-only one.

## 6. Concrete v1 implementation spec

### 6.1 Schema — two tables

**`purchase_orders`:**
- `purchase_order_id` — UUID, PK, server-generated
- `tenant_id`, `legal_entity_id` — UUID, NOT NULL
- `purchase_request_id` — UUID, nullable (per `04-data-model.md` §14.1: "PO
  may also be issued without a prior request")
- `vendor_profile_id` — UUID, nullable (see §3 — unvalidated in v1)
- `po_number` — VARCHAR, generated from a Postgres `SEQUENCE` as
  `PO-{:06d}`, unique per tenant
- `po_status` — VARCHAR: `ISSUED` → `CLOSED` (terminal). Amendment does
  **not** change status — see below.
- `total_amount`, `currency_code`
- `version` — INTEGER, starts at 1, incremented by each amendment
- `issued_by_principal_id`, `closed_by_principal_id` (nullable)
- `correlation_id` — NOT NULL, unique per tenant (§5)
- `created_at`, `issued_at`, `closed_at` (nullable)
- RLS enabled, `tenant_isolation_policy`, same as every Phase 3 sibling —
  defense-in-depth only: this platform connects as a Postgres superuser,
  which unconditionally bypasses RLS, so every store method also filters
  explicitly by `tenant_id` in its own SQL (the lesson
  `general-ledger-svc`'s and `tenant-entity-registry-svc`'s CI failures
  already taught this platform — see `internal/store/pg_store.go`'s package
  doc comment).

**`purchase_order_amendments`** (append-only, no UPDATE/DELETE path ever):
- `amendment_id` — UUID, PK
- `purchase_order_id` — FK
- `from_version`, `to_version`
- `previous_total_amount`, `new_total_amount`
- `reason` — TEXT, NOT NULL (an amendment with no stated reason isn't
  useful evidence, same rationale as `purchase-request-svc`'s required
  rejection reason)
- `amended_by_principal_id`, `amended_at`

### 6.2 Endpoints

| Method | Path | Purpose |
| --- | --- | --- |
| `POST` | `/v1/purchase-orders` | Issue a PO. Authz-gated (`PO_ISSUE`). Idempotent on `(tenant_id, correlation_id)`. If `purchase_request_id` is supplied, synchronously verifies it against `purchase-request-svc` (§6.3) — fail-closed. Publishes `purchase.order.issued`. |
| `GET` | `/v1/purchase-orders` | List, filterable by `tenant_id` (required), `legal_entity_id`, `status`. |
| `GET` | `/v1/purchase-orders/{purchase_order_id}` | Get one. |
| `POST` | `/v1/purchase-orders/{id}/amend` | Update `total_amount` + record a `PurchaseOrderAmendment` row, bump `version`. Authz-gated (`PO_AMEND`). Only legal while `po_status = ISSUED`. Publishes `purchase.order.amended`. |
| `POST` | `/v1/purchase-orders/{id}/close` | `ISSUED → CLOSED`, terminal, atomic `WHERE po_status = 'ISSUED'` CAS update. Authz-gated (`PO_CLOSE`). Publishes `purchase.order.closed`. |
| `GET` | `/healthz` | Liveness probe. |
| `GET` | `/readyz` | Readiness probe (DB connectivity). |

### 6.3 Cross-service verification of `purchase_request_id`

When `IssueOrderRequest.purchase_request_id` is set, this service calls
`GET {PURCHASE_REQUEST_SERVICE_URL}/v1/purchase-requests/{id}` (with
`X-Tenant-Id` set to the caller's tenant — `purchase-request-svc`'s own
`GetRequest` reads tenant scope from that header via its `TenantContext`
middleware, not a query param) and requires:
1. The request exists (`404` upstream → `ErrPurchaseRequestNotFound`, `422`
   here).
2. `tenant_id` and `legal_entity_id` on the returned request match the
   Issue request's own (`ErrPurchaseRequestMismatch`, `422`) — never trust
   a caller-supplied ID without checking it belongs to the same scope.
3. `status == "APPROVED"` (`ErrPurchaseRequestNotApproved`, `422`).

Any network error, timeout, or non-200/404 response →
`ErrPurchaseRequestServiceUnavailable`, `503`, fail-closed. This
deliberately follows `accounts-receivable-svc`'s/`bank-reconciliation-svc`'s
"verify against the real upstream record" pattern — the 2026-07-23 audit
flagged the opposite pattern (`corporate-actions-svc` trusting a
caller-supplied `resolution_id` with zero verification) as a real gap.

### 6.4 Failure mode

- Store unreachable → `503`.
- `authorization-svc` unreachable or denies → `503`/`403` respectively,
  never silently permitted.
- `purchase-request-svc` unreachable when a `purchase_request_id` was
  supplied → `503` (§6.3) — the PO is never issued against an unverifiable
  claim.
- Amend/Close on a non-`ISSUED` order → `422`, not a silent no-op.

## 7. Tech stack & model policy

Go, consistent with every service in this repo (see project memory
`[[project-zoikosuite-overview]]` on the real-vs-doctrine divergence: every
built Phase 3/4/5 service is Go despite `.agents/rules/tech-stack.md`
nominally calling for Node/TypeScript outside Tier 0 — this service follows
actual platform convention, not the on-paper rule, for consistency with its
direct sibling `purchase-request-svc`).

## 8. Port

**8129** — `docker-compose.yml` is contiguously populated `8080`–`8128`
with no gaps (confirmed by direct check, not assumed); `8129` is the next
free port.

## 9. Explicit non-goals for v1

- **No vendor-profile validation** (§3) — Vendor Due Diligence Service
  doesn't exist yet; `vendor_profile_id` is stored unvalidated.
- **No goods-receipt / three-way-match workflow** — `close` is a single
  explicit action, not a receiving process.
- **No `CANCELLED` status** — only the three events named in §12.9
  (`issued`/`amended`/`closed`) are in scope; a cancellation path would be
  scope invention without a corresponding spec event.
- **No Kafka consumer** — upstream `purchase_request_id` verification is
  synchronous HTTP (§6.3), matching every sibling Phase 3 service's
  cross-service-check pattern rather than event-driven auto-creation
  (which no Phase 3 service does today).

## 10. Build sequencing & environment note

Built directly against `purchase-request-svc`'s proven skeleton (chi +
pgxpool + RLS-with-explicit-filters + authz-gate + Kafka producer +
Dockerfile + tests), reusing its exact dependency set (`go.sum` copied
verbatim — checksums are dependency-version-specific, not module-name-
specific, so this is safe once `go.mod`'s `require` block matches).

**Important caveat, stated plainly rather than glossed over:** this
environment had no Go toolchain and no running Docker daemon available
during implementation, so `go build`/`go vet`/`go test`, `go mod tidy`, and
a Docker image build could **not** be run or verified locally. See
`progress.md` for exactly what has and hasn't been confirmed, and what
still needs a real CI run or a local machine with Go/Docker before this is
considered production-ready — mirroring this project's own hard-won lesson
from the Kubernetes `kind_run_log.md` fabricated-evidence incident
(`[[zoikosuite-known-issues]]`): don't claim verification that didn't
actually happen.
