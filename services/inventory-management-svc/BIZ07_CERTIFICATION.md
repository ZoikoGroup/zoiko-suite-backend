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
| Failure: missing required mapping blocks use | Enforced at activation (TAX + ACCOUNTING required) in the **handler only** | `ActivateOffering` |
| Failure: historical transactions pin the version | Data layer supports it (immutable versions, `version-as-of`); no consumer enforces it yet | migration 000006 |
| Dependency: REF-09 units | **Not met** — REF-09 does not exist; `unit` is free text | — |
| Boundary: not the SaaS price book (COM-01) | Respected; no price fields | — |

## Implementation gate (spec: "must pass ... before production certification")

| Gate | Evidence | State |
|---|---|---|
| Authority boundary | No price/plan fields in domain or routes (by inspection). No test asserts it. | Partial |
| Tenant isolation | RLS enabled and forced on all four catalog tables (migration 000006); store queries carry `tenant_id`. `TestPgStore_Catalog_TenantIsolation` passes on Postgres 16: tenant B cannot read, search, map, activate, suspend or retire tenant A's offering, and A's data is unchanged. Run as the Postgres superuser, which bypasses RLS, so this proves the query-level `tenant_id` filtering, not the RLS policies. | Covered (query level); RLS policies not proven |
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

- **RLS policies are unproven.** The tests connect as the Postgres superuser, which bypasses RLS. Re-run the isolation test under a non-superuser role to prove the policies themselves.
- The mapping gate was tested through handler tests with a stub store, not against a live database through the running service.
- The full docker-compose stack was not run: no real authorization-svc, Kafka or end-to-end flow.
- OpenAPI/AsyncAPI files parse as YAML but were not run through a linter, and no test ties the event names in `asyncapi.biz07.yaml` to the Go publisher.
- Authorization was tested only with a stub; real authorization-svc decisions for the `CATALOG_*` actions were not checked, nor that those actions are registered there.

## Open items to reach certification

1. ~~Run the store suite on Postgres and record the result.~~ Done, 12/12.
2. ~~Add a cross-tenant negative test.~~ Done at query level; re-run under a non-superuser role to prove RLS.
3. Decide classification/retention for catalog records and implement or document it.
4. Move the mapping-required rule into the database (trigger), so a direct write cannot bypass it.
5. Validate `catalog_version_ref` in AR quotes/sales orders against an ACTIVE catalog version.
6. Replace free-text `unit` with REF-09 validation once REF-09 exists.
7. Add an event-name contract test for `asyncapi.biz07.yaml`.
8. Confirm the TAX + ACCOUNTING required-mapping set with the spec owner (the spec does not enumerate it).
