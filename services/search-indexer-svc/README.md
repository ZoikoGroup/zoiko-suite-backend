# search-indexer-svc

Polls obligations-svc and upserts records into OpenSearch, providing the
first real working integration of the ZoikoSuite search layer
(`01-backend.md §10.4`).

## Responsibilities

- Polls `GET /v1/obligations` on `obligations-svc` at a configurable interval.
- Resolves `tenant_id` from `legal_entity_id` via `tenant-entity-registry-svc`
  (with an in-memory cache to minimise upstream calls).
- Upserts each obligation into the `zoiko-obligations` OpenSearch index
  using `obligation_id` as the document ID (idempotent).
- Serves `/healthz`, `/readyz`, and `/metrics` (Prometheus).

This service is a **read-only consumer** — it never writes to obligations-svc
or its database.

## Environment variables

| Variable | Default | Notes |
|---|---|---|
| `PORT` | `8094` | HTTP port |
| `OBLIGATIONS_SVC_URL` | `http://obligations-svc:8088` | Base URL of obligations-svc |
| `TENANT_SVC_URL` | `http://tenant-svc:8081` | Base URL of tenant-entity-registry-svc |
| `OPENSEARCH_ADDRESSES` | `http://opensearch:9200` | Comma-separated list of OS nodes |
| `OPENSEARCH_USERNAME` | *(empty)* | Required in staging/prod |
| `OPENSEARCH_PASSWORD` | *(empty)* | Required in staging/prod |
| `SYNC_INTERVAL` | `60s` | How often to poll obligations-svc |
| `OTEL_EXPORTER_OTLP_ENDPOINT` | *(empty)* | Trace export endpoint |

## How it works

```
obligations-svc GET /v1/obligations
        │
        ▼ for each obligation
tenant-svc GET /v1/entities/{legal_entity_id}  (cached in memory)
        │
        ▼
OpenSearch: upsert into zoiko-obligations
        (obligation_id is the _id — idempotent)
```

## Adding a second consumer (audit, documents)

1. Create a new syncer struct in `internal/sync/` (e.g. `audit_syncer.go`)
   following the same `Start(ctx)` / `runCycle` pattern.
2. Add its config to `cmd/server/main.go` and start it as a goroutine.
3. Use `searchclient.IndexAuditEvents` or `searchclient.IndexDocuments` as
   the index name.

No changes to `search-client` are needed — just a new syncer.

## Metrics

| Metric | Labels | Notes |
|---|---|---|
| `search_indexer_obligations_indexed_total` | `status={ok,error,skip_no_tenant}` | Upsert outcomes |
| `search_indexer_sync_errors_total` | — | Full cycle errors (fetch failed, etc.) |

## Index naming & tenant isolation

See `services/search-client/README.md` for the full index-naming convention
and the GTRM / residency follow-up note for RESTRICTED-tier data.
