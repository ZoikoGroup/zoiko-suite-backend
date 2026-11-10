# BIZ-07 Product & Service Catalog — Certification Status

**Service host:** inventory-management-svc (BIZ-07 is bolted onto this service; no separate service)
**Assessed against:** `ZoikoSuite_Business_Operations_Content_Services_Detailed_Service_Specifications_v1_0` (ZS-SVC-P-001) section 9
**Status: NOT CERTIFIED.** Core contract implemented; the implementation gate below is only partly evidenced.

A certificate listing only successes certifies nothing, so the last two sections say what was **not** verified.

## Contract coverage (spec section 9)

| Spec row | State | Where |
|---|---|---|
| Commands: CreateOffering, CreateVersion, ApproveOffering, Activate, Suspend, Retire, LinkMapping | Implemented | `internal/handler/catalog.go` |
| Queries: GetOffering, GetVersionAsOf, SearchCatalog, ListVariants, GetMappings | Implemented | same |
| Lifecycle Draft -> Approved -> Active -> Suspended/Retired -> Superseded | Implemented; activation supersedes prior ACTIVE in one transaction | `internal/store/catalog_store.go` |
| Authorization / SoD | Distinct actions: `CATALOG_READ`, `CATALOG_MANAGE`, `CATALOG_APPROVE`, `CATALOG_MAPPING_LINK` | handler |
| Events (5) | Published; contract in `asyncapi.biz07.yaml` | `internal/events/publisher.go` |
| API contract | `openapi.biz07.yaml` | this directory |
| Failure: missing required mapping blocks use | Enforced at activation (TAX + ACCOUNTING required) in the handler and by a database trigger (migration 000013) | `ActivateOffering` |
| Failure: historical transactions pin the version | Data layer supports it (immutable versions, `version-as-of`). `GET ./versions/{versionID}/usability` lets a consumer check a version before pinning it. accounts-receivable-svc now calls it at quote and sales-order approval when `catalog_version_ref` is `catalog:<offering_id>:<version_id>`; opaque refs are untouched. AP and inventory consumers do not yet | migration 000006, `catalog.go`, AR `internal/catalogref` |
| Dependency: REF-09 units | **Not met** — REF-09 does not exist; `unit` is free text | — |
| Boundary: not the SaaS price book (COM-01) | Respected; no price fields | — |

## Implementation gate (spec: "must pass ... before production certification")

| Gate | Evidence | State |
|---|---|---|
| Authority boundary | No price/plan fields in domain or routes (by inspection). No test asserts it. | Partial |
| Tenant isolation | RLS enabled and forced on all four catalog tables (migration 000006); store queries carry `tenant_id`. `TestPgStore_Catalog_TenantIsolation` passes on Postgres 16 as both a superuser and a non-superuser role (`NOSUPERUSER NOBYPASSRLS`, owning the tables): tenant B cannot read, search, map, activate, suspend or retire tenant A's offering, A's data is unchanged, and a raw count with B's tenant context and no `tenant_id` predicate sees none of A's rows in any of the four tables (the RLS probe, skipped automatically for a bypassing role). | Covered, including RLS |
| Classification / retention | Nothing catalog-specific found. | **Missing** |
| Negative paths | Duplicate SKU, content immutability, activate-requires-approved, append-only mappings, unknown version, version-as-of with none active (store tests); missing/partial/PRODUCT-only mappings (handler tests). | Mostly covered |
| Evidence lineage | Version rows record creator/approver/activator and reasons; mappings record linker. No test asserts downstream-usage references (spec lists them). | Partial |

## What was verified, and how

```
go build ./... && go vet ./...   clean
go test ./... -count=1           all packages pass with TEST_DATABASE_URL set to a throwaway
                                 postgres:16-alpine container (store suite 81 s); handler suite
                                 includes the 5 mapping-gate tests
catalog store tests              12/12 pass (11 existing + the tenant-isolation test)
docker build                     image builds (first attempt failed on a transient go mod download)
container smoke run              starts, connects to Postgres, /healthz and /readyz 200,
                                 catalog route without identity -> 401
```

Date: 2026-11-10. The Postgres container was disposable and has been removed.

## NOT verified

- Only the 13 catalog store tests were run as the non-superuser role; the rest of the store suite and the cross-service flows were not.
- The mapping gate was tested through handler tests with a stub store, not against a live database through the running service.
- The full docker-compose stack was not run: no real authorization-svc, Kafka or end-to-end flow.
- OpenAPI/AsyncAPI files parse as YAML but were not run through a linter. Event names (and envelope constants) are tied to the publisher by a test; payload fields are not.
- Authorization was tested only with a stub; real authorization-svc decisions for the `CATALOG_*` actions were not checked, nor that those actions are registered there.

## Open items to reach certification

1. ~~Run the store suite on Postgres and record the result.~~ Done, 12/12.
2. ~~Add a cross-tenant negative test.~~ Done, including an RLS probe under a non-superuser role.
3. Decide classification/retention for catalog records and implement or document it.
4. ~~Move the mapping-required rule into the database (trigger).~~ Done: migration 000013; tested via the store and a raw UPDATE.
5. ~~Validate `catalog_version_ref` in AR quotes/sales orders against an ACTIVE catalog version.~~ Done for AR quotes (ar02) and orders (ar03), opt-in by the `catalog:` ref format, fail-closed. Not done: AP, and the authorization-svc grant of `CATALOG_READ` to `system:ar-catalog` (deployment config, unverified).
6. Replace free-text `unit` with REF-09 validation once REF-09 exists.
7. ~~Add an event-name contract test for `asyncapi.biz07.yaml`.~~ Done: `internal/events/catalog_contract_test.go` (verified to fail on a renamed event).
8. Confirm the TAX + ACCOUNTING required-mapping set with the spec owner (the spec does not enumerate it).

## AR consumer (accounts-receivable-svc) — added after the sections above

- Format: `catalog_version_ref = catalog:<offering_id>:<version_id>` (UUIDs). Anything else is an opaque ref and is never sent to the catalog, so existing quotes and orders are unaffected. A `catalog:` ref that does not parse is rejected `400` when the document is written.
- Check: at `approveQuote` (ar02) and `approveOrder` (ar03), AR calls the usability endpoint as the system principal `system:ar-catalog`. Unknown or unusable version -> `422 CATALOG_VERSION_INVALID`. Catalog down, unwired or answering 401/403/5xx -> `503`, fail closed; the document stays pending.
- Verified: `internal/catalogref` unit tests; `TestCatalogVersionPinning_CheckedAtApproval` in ar02 and ar03 (integration tag, embedded Postgres); full AR unit suite and full ar02/ar03 integration suites pass.
- NOT verified: a live AR -> inventory call. Not run: the docker-compose stack. The `system:ar-catalog` principal needs `CATALOG_READ` on the legal entity in authorization-svc; that grant was not created or checked, and without it every `catalog:` approval will fail closed with 503.
