# search-client

Reusable Go package providing a tenant-scoped OpenSearch client for the
ZoikoSuite platform (architecture `01-backend.md §10.4`). It is a library:
there is no HTTP server, no endpoints and no events. It reads **no
environment variables** — `Config` is passed in by the consumer.

It has two interfaces, split on purpose:

| Interface | Constructor | Audience |
|---|---|---|
| `Client` | `New(Config)` | A domain service that indexes its own records and searches them. Cannot repoint an alias or drop a generation. |
| `Engine` (embeds `Client`) | `NewEngine(Config)` | `search-indexer-svc`, the one service whose job is the index lifecycle (ZS-SVC-AB-001, ESR-02/03/05). |

## Quick start — `Client`

```go
import "zoiko.io/search-client/searchclient"

c, err := searchclient.New(searchclient.Config{
    Addresses: []string{os.Getenv("OPENSEARCH_ADDRESSES")},
})

// Call once at service startup. Idempotent, and safe when several replicas
// start together: losing the create race is success.
_ = c.EnsureIndex(ctx, searchclient.IndexObligations)

// Index a record.
err = c.Index(ctx, searchclient.IndexObligations, searchclient.Document{
    ID:            obligation.ObligationID,
    TenantID:      tenantID,       // resolved from LegalEntityID via tenant-svc
    LegalEntityID: obligation.LegalEntityID,
    Body: map[string]any{
        "obligation_code":     obligation.ObligationCode,
        "obligation_status":   obligation.ObligationStatus,
        "due_date":            obligation.DueDate,
    },
})

// Search.
results, err := c.Search(ctx, searchclient.IndexObligations, searchclient.SearchQuery{
    TenantID: tenantID,     // REQUIRED — omitting returns ErrTenantIDRequired
    Keywords: "GST filing",
    Size:     20,           // defaults to 20; no maximum is enforced here
})
```

- `Index` requires `ID`, `TenantID` and `LegalEntityID`, and always writes with
  `refresh=true` (the document is searchable when the call returns).
- `Search` returns `ErrTenantIDRequired` immediately, with no network call, when
  `TenantID` is empty.

## Index naming convention

Indices are **domain-scoped, not tenant-scoped**:

| Constant | Index name | Consumer |
|---|---|---|
| `IndexObligations` | `zoiko-obligations` | `search-indexer-svc` (obligations-svc records) |
| `IndexAuditEvents` | `zoiko-audit` | Audit Event Store (Phase 2) |
| `IndexDocuments`   | `zoiko-documents`  | Document Vault Service (Phase 2) |

Every document carries `tenant_id` and `legal_entity_id` as `keyword`-type
fields (exact-match, not analyzed). A shared index with a mandatory `tenant_id`
filter replaces per-tenant indices, which would multiply indices by
tenants × domains and make cross-entity queries inside a tenant impossible.

## Tenant scope is enforced on every query path

| Path | How |
|---|---|
| `Client.Search` | `SearchQuery.TenantID` is required; empty → `ErrTenantIDRequired`. |
| `Engine.ExecutePlan` | The plan must carry a **mandatory** filter on `tenant_id` (`TenantField`) naming **exactly one** tenant. Missing, empty, blank or multi-tenant → `ErrTenantIDRequired`. A tenant that appears only as a user filter does not count. |

Both fail before any request is sent.

## `Engine` — index lifecycle, projections, compiled queries

```go
e, err := searchclient.NewEngine(cfg)
```

**Generations and aliases (ESR-05).** A scope is served through an alias that
points at one concrete generation index.

| Method | Behaviour |
|---|---|
| `EnsureGeneration(physicalIndex, []FieldMapping)` | Creates a generation with a **strict** mapping: governance fields plus the contract's registered fields. A contract may not register a reserved governance field (`ReservedFields()`). Idempotent; losing a create race is success. |
| `ActivateGeneration(alias, physicalIndex)` | Repoints the alias in **one** atomic `_aliases` call and returns the previous target. |
| `ActiveGeneration(alias)` | The concrete index behind the alias, `""` if none. More than one target is `ErrAliasDrift`. |
| `DropIndex(physicalIndex)` | Removes a generation. **No guard** against dropping the active generation or a wildcard — see *Open clarifications*. |

Names: `Alias(scope)` and `GenerationIndex(scope, id)`.

**Projections (ESR-02).**

| Method | Behaviour |
|---|---|
| `IndexProjection` | Upsert, `refresh=wait_for`. |
| `CreateProjection` | Create-only (`op_type=create`); an existing document is `ErrProjectionExists`. For backfill, so replayed history never overwrites a live write. |
| `DeleteProjection`, `GetProjection` | Remove / read by id. |
| `CountProjections`, `CountMissing` | Live (non-tombstoned) count; count of documents lacking a field. |

Both writers share one validation: `DocID`, `TenantID`, `SourceType` and
`SourceID` are required. `LegalEntityID` is **not** required (ESR §2.1 does not
list it). Governance fields are merged over the domain body, so a source field
named `tenant_id` loses. A tombstone never carries a vector. An unregistered
field is refused by the strict mapping as `ErrStrictMappingRejected`.
`ProjectionDocID(tenant, sourceType, sourceID)` is the canonical, tenant-first
`_id`.

**Compiled queries (ESR-03).** `ExecutePlan(ctx, target, ExecutionPlan)` is the
only query shape the engine runs; there is no field for raw DSL.

- `MandatoryFilters` (ANDed) and `MandatoryMustNot` come from trusted context.
  Each must name a field and at least one non-blank value, otherwise
  `ErrMandatoryFilterEmpty` — an empty list never becomes "no constraint".
- `UserFilters` only narrow; an empty one is ignored.
- `target` must be a single concrete name: no wildcard, comma list, `_all`,
  hidden (`.`-prefixed), date-math or cluster-qualified name → `ErrInvalidTarget`.
  The package cannot tell an alias from a concrete generation index (the
  indexer uses both), so callers must pass **server-owned** names only.
- Result fields are deny-by-default: with no `SourceIncludes`, only governance
  lineage fields are returned.
- Text uses `multi_match` over `TextFields` only (`operator: and`); no text
  fields → `match_none`.
- Facets use `min_doc_count` and drop sub-minimum buckets again client-side
  (never below 1).
- Highlights use private-use sentinels (`HighlightOpen` / `HighlightClose`), not
  HTML. `StripHighlightMarkers(fragment)` returns the plain text **only**; the
  retrieval layer locates spans from the sentinels before stripping.
- `Result.Partial` is set when shards failed or the engine timed out, even on an
  HTTP 200. An alias with no generation is `ErrNoActiveGeneration`.
- Paging is `search_after` only; there is no offset. The sort ends with `_score`
  then `source_id`. `source_id` is unique within a tenant and source type, not
  across source types — see *Open clarifications*.
- Semantic / hybrid: set `Vector` and `K` (`Hybrid` adds the lexical clause). The
  eligibility filters apply inside the k-NN walk **and** in the outer query. The
  vector field is the reserved `embedding_vector`; build the generation with
  `VectorMapping(dimensions, spaceType)`.

`Ping(ctx)` reports cluster reachability, for readiness only.

## Configuration

`Config{Addresses, Username, Password}`. The package reads no environment;
consumers load these. Names used by `search-indexer-svc`:

| Env var | Default in `search-indexer-svc` | Default in `deployments/docker-compose.yml` |
|---|---|---|
| `OPENSEARCH_ADDRESSES` | `http://localhost:9200` | `http://opensearch:9200` |
| `OPENSEARCH_USERNAME` | *(empty)* | — |
| `OPENSEARCH_PASSWORD` | *(empty)* | — |

The two defaults differ and which is authoritative is an open clarification.
Username and password are empty in dev (security plugin disabled) and required
in staging/prod with TLS + auth.

## Tests

Unit tests (no cluster needed; the engine is faked over HTTP, so they prove
request shape and local validation, not real OpenSearch behaviour):

```bash
cd services/search-client
go test ./... -count=1
```

Integration tests need OpenSearch on `localhost:9200`:

```bash
docker compose -f deployments/docker-compose.yml up -d opensearch
cd services/search-client
go test ./searchclient/... -v -tags=integration \
  -run 'TestPutGet_RoundTrip|TestSearch_TenantIsolation|TestSearch_EmptyTenantIDIsRejected|TestIndex_EmptyTenantIDIsRejected'
```

Note `-run` takes a regular expression: alternatives are separated by `|`, **not**
commas (a comma list matches no test). Check what a pattern selects with
`go test -tags=integration -list '<pattern>' ./searchclient/`.

## Open clarifications

These are not decided by the ESR contract and are deliberately unchanged:

1. **Restriction epoch.** `IndexProjection` is an unconditional upsert; the caller
   refuses older epochs (INV-18 / NP-11).
2. **Legacy `Client` scope.** `EnsureIndex` / `Index` / `Search` use a dynamically
   mapped index and search `*` fields, unlike the strict `Engine` generations.
3. **Page-size cap.** Neither `Search` nor `ExecutePlan` caps `Size`.
4. **`DropIndex` guards.** Wildcard and active-generation protection are not in
   the library.
5. **Environment defaults.** See *Configuration*.
6. **Cursor tie-break.** ESR does not prescribe the final sort key.
7. **`refresh`.** `Index` uses `refresh=true`; ESR prescribes no policy.

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

1. Add `zoiko.io/search-client` to your service's `go.mod` (with
   `replace zoiko.io/search-client => ../search-client`), or copy the package in
   as `internal/searchclient/` following the `internal/telemetry` copy-paste
   pattern.
2. Call `EnsureIndex` once at startup for each index your service needs.
3. Index domain records with your record's primary key as `Document.ID` — this
   makes indexing idempotent.
4. Always supply a tenant: `TenantID` in `SearchQuery`, or a single-tenant
   mandatory `tenant_id` filter in an `ExecutionPlan`.
