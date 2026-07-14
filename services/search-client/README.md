# search-client

Reusable Go package providing a tenant-scoped OpenSearch client for the
ZoikoSuite platform (architecture `01-backend.md §10.4`).

## Quick start

```go
import "zoiko.io/search-client/searchclient"

c, err := searchclient.New(searchclient.Config{
    Addresses: []string{os.Getenv("OPENSEARCH_ADDRESSES")},
})

// Call once at service startup (idempotent).
_ = c.EnsureIndex(ctx, searchclient.IndexObligations)

// Index a record.
err = c.Index(ctx, searchclient.IndexObligations, searchclient.Document{
    ID:            obligation.ObligationID,
    TenantID:      tenantID,       // resolved from LegalEntityID via tenant-svc
    LegalEntityID: obligation.LegalEntityID,
    Body: map[string]any{
        "obligation_code":     obligation.ObligationCode,
        "obligation_type":     obligation.ObligationType,
        "obligation_status":   obligation.ObligationStatus,
        "source_reference":    obligation.SourceReference,
        "responsible_function": obligation.ResponsibleFunction,
        "jurisdiction_id":     obligation.JurisdictionID,
        "due_date":            obligation.DueDate,
    },
})

// Search.
results, err := c.Search(ctx, searchclient.IndexObligations, searchclient.SearchQuery{
    TenantID: tenantID,     // REQUIRED — omitting returns ErrTenantIDRequired
    Keywords: "GST filing",
    Size:     20,
})
```

## Index naming convention

Indices are **domain-scoped, not tenant-scoped**:

| Constant | Index name | Consumer |
|---|---|---|
| `IndexObligations` | `zoiko-obligations` | `search-indexer-svc` (obligations-svc records) |
| `IndexAuditEvents` | `zoiko-audit` | Audit Event Store (Phase 2) |
| `IndexDocuments`   | `zoiko-documents`  | Document Vault Service (Phase 2) |

Every document carries `tenant_id` and `legal_entity_id` as `keyword`-type
fields (exact-match, not analyzed). Every query **must** supply `TenantID` —
`Search()` returns `ErrTenantIDRequired` immediately (no network call) if it
is empty. This is the platform's primary tenant isolation mechanism at the
search layer.

**Why not one index per tenant?**
Per-tenant indices create O(tenants × domains) index proliferation and make
cross-entity queries inside a tenant impossible. The shared-index + mandatory
`tenant_id` filter achieves the same isolation with predictable cluster
topology.

## Tenant ID resolution

`obligations-svc` records carry `legal_entity_id` but no `tenant_id`
(the obligations domain doesn't own tenant resolution). The indexer resolves
`tenant_id` from `legal_entity_id` via `tenant-entity-registry-svc` and
caches results in memory. See `search-indexer-svc` for the implementation.

## Configuration

| Env var | Default | Notes |
|---|---|---|
| `OPENSEARCH_ADDRESSES` | `http://opensearch:9200` | Comma-separated list |
| `OPENSEARCH_USERNAME` | *(empty)* | Dev: security plugin disabled |
| `OPENSEARCH_PASSWORD` | *(empty)* | Required in staging/prod |

## Running integration tests

```bash
# Start OpenSearch
docker compose -f deployments/docker-compose.yml up -d opensearch

# Run integration tests (requires OpenSearch on localhost:9200)
cd services/search-client
go test ./searchclient/... -v -tags=integration \
  -run TestPutGet_RoundTrip,TestSearch_TenantIsolation,TestSearch_EmptyTenantIDIsRejected
```

Expected output:
```
--- PASS: TestPutGet_RoundTrip (0.21s)
--- PASS: TestSearch_TenantIsolation (0.14s)
--- PASS: TestSearch_EmptyTenantIDIsRejected (0.00s)
PASS
```

## Phase 2.x follow-up: GTRM residency for RESTRICTED-tier data

> **Out of scope for this chunk — flagged for cross-team coordination.**
>
> For `RESTRICTED`-tier data (data classification §10.3), the search index
> may need to be pinned to a region-specific OpenSearch cluster to satisfy
> GTRM routing requirements (same constraint as object storage). This requires
> a decision on regional OS cluster topology and coordination with the GTRM
> owner. The `zoiko-documents` index will be the primary concern when Document
> Vault Service ships.

## Consuming this package in a new service

1. Add `zoiko.io/search-client` to your service's `go.mod` (or copy the
   package in as `internal/searchclient/` following the `internal/telemetry`
   copy-paste pattern).
2. Call `EnsureIndex` once at startup for each index your service needs.
3. Index your domain records using `Index()` with your record's primary key as
   `Document.ID` — this makes indexing idempotent.
4. Always supply `TenantID` in `SearchQuery` — the client enforces this.
