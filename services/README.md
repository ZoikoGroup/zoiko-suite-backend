# zoiko-suite

Go microservices monorepo — one `go.mod` per service, no shared build tool.

| Service | Port | Status |
| --- | --- | --- |
| `identity-context-svc` | 8080 | HTTP server wired; principal store is Postgres-backed |
| `tenant-entity-registry-svc` | 8081 | Real Postgres + Row-Level Security for tenant isolation |
| `jurisdiction-rules-svc` | 8082 | Real Postgres-backed read API |
| `governance-decision-log-svc` | 8083 | Append-only evidence store for governance decisions (`POST`/`GET /v1/decisions`) |
| `audit-event-store-svc` | — | Kafka consumer + store interface only; no HTTP server yet |