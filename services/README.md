# zoiko-suite Services

Go microservices monorepo — one `go.mod` per service, no shared build tool.

| Service | Port | Description |
| --- | --- | --- |
| `identity-context-svc` | 8080 | HTTP server wired; session resolution & principal store is Postgres-backed |
| `tenant-entity-registry-svc` | 8081 | Real Postgres + Row-Level Security for tenant isolation |
| `jurisdiction-rules-svc` | 8082 | Real Postgres-backed read API for jurisdiction configuration |
| `governance-decision-log-svc` | 8083 | Append-only evidence store for governance decisions (`POST`/`GET /v1/decisions`) |
| `audit-event-store-svc` | 8084 | Kafka consumer + HTTP server health probes (`/healthz` and `/readyz`) |
| `policy-svc` | 8085 | Policy/PolicyVersion CRUD + APPROVAL_THRESHOLD evaluation (`/v1/policies`, `/v1/policies/evaluate`) |
| `configuration-feature-flag-svc` | 8086 | Versioned config + feature flags (`/v1/config`, `/v1/flags`); event publishing stubbed |
| `secret-vault-integration-svc` | 8087 | Secret access broker — policy-gated, leased, rotation-aware (`/v1/secrets/broker`); local-file encrypted-at-rest backend for v1, real Vault/KMS client pending |
| `obligations-svc` | 8088 | Obligation/FilingRequirement CRUD (`/v1/obligations`), jurisdiction-bound with fail-closed validation against jurisdiction-rules-svc |
| `authorization-svc` | 8089 | Runtime access-decision engine — RBAC + delegated-authority + Separation-of-Duties evaluation (`/v1/authorize`), append-only decision evidence (`/v1/access-decisions`) |
| `workflow-svc` | 8090 | Multi-stage approval orchestration (`/v1/workflows`) — extends authorization-svc, doesn't replace it: every approval action is checked via `POST /v1/authorize` before it's applied |
| `gateway-auth-svc` | 8092 | API Gateway ForwardAuth target — verifies every gated request's signed identity envelope against identity-context-svc's JWKS (`/verify`) before Traefik forwards it to a backend; stateless, no DB |
| `schema-registry-svc` | 8093 | Centralized, version-controlled event payload schemas (`/v1/schemas`) — enforces backward-compatible evolution (breaking changes `409`); schema registration is gated through authorization-svc (`SCHEMA_PUBLISH`), fail-closed |
| `document-vault-svc` | 8094 | Phase 2 — versioned document storage with SHA-256 integrity (verified on every read), append-only access lineage, retention, and jurisdiction-aware residency checks against tenant-entity-registry-svc's GTRM resolution (`/v1/documents`); local encrypted-at-rest backend for v1 |
| `evidence-manifest-svc` | 8095 | Phase 2 — assembles structured evidence sets from governance-decision-log-svc, authorization-svc, and workflow-svc (`/v1/evidence-manifests`) for audit/regulator/legal-discovery scenarios; fails closed (whole manifest, never partial) if any source is unreachable |
| `workflow-history-svc` | 8097 | Phase 2 — durably consumes workflow events from `zoiko.workflow.events` and serves history (`/v1/workflows/{workflow_instance_id}/history`) |
| `general-ledger-svc` | 8098 | Phase 3 — authoritative owner of journalized financial postings (`/v1/journals`); Tri-Phase Commit States (PENDING → VALIDATED → FINALIZED → REVERSED), double-entry balance enforced at validation, no finalized journal ever hard-edited (corrections only via a new reversing journal); every mutating action gated through authorization-svc, real HTTP client from day one; Postgres RLS for tenant isolation |
| `accounts-payable-svc` | 8099 | Phase 3 — owns vendor invoice intake, liability-side invoice lifecycle, and payment readiness state (`/v1/invoices`); RECEIVED → VALIDATED → APPROVED → PAYMENT_REQUESTED, no way to reach payment initiation without passing through approval and validation first; every mutating action gated through authorization-svc, real HTTP client from day one; Postgres RLS + explicit `tenant_id` filters in every query (RLS alone is insufficient given this platform connects as a Postgres superuser — found via general-ledger-svc's CI failure) |

## Unified Local Platform Development Stack

You can boot the entire platform (all 5 microservices + PostgreSQL database with schemas pre-applied + Redis cache + single-broker Kafka) with a single command.

### 1. Prerequisite
Ensure Docker and Docker Desktop are running on your system.

### 2. Booting the Stack
From the repository root directory, run:
```powershell
docker compose -f deployments/docker-compose.yml up --build
```
Or to run in the background (detached mode):
```powershell
docker compose -f deployments/docker-compose.yml up -d --build
```

To view logs for the services:
```powershell
docker compose -f deployments/docker-compose.yml logs -f
```

---

## Local Verification & Usage Notes

### 1. Verify Service Probes
Confirm each container is running and healthy:
```bash
# identity-context-svc liveness check
curl http://localhost:8080/health

# tenant-entity-registry-svc liveness/readiness checks
curl http://localhost:8081/healthz
curl http://localhost:8081/readyz

# jurisdiction-rules-svc checks
curl http://localhost:8082/healthz
curl http://localhost:8082/readyz

# governance-decision-log-svc checks
curl http://localhost:8083/healthz
curl http://localhost:8083/readyz

# audit-event-store-svc checks (remapped container port to 8084)
curl http://localhost:8084/healthz
curl http://localhost:8084/readyz

# policy-svc checks
curl http://localhost:8085/healthz
curl http://localhost:8085/readyz
```

### 2. Produce a Test Kafka Event
You can publish a test event using `kcat` (or `kafkacat`) from your host machine to confirm message ingestion.

**On Bash (Linux/macOS/WSL):**
```bash
kcat -P -b localhost:9092 -t zoiko.identity.events \
     -H "X-Event-ID=test-evt-001" \
     <<< '{"event_type":"identity.context.resolved","emitted_at":"2026-07-06T08:00:00Z","schema_version":"1.0","source_service":"identity-context-svc","payload":{"principal_id":"u-001","tenant_id":"t-001","legal_entity_id":"e-001","session_context_id":"s-001","correlation_id":"c-001"}}'
```

**On Windows (PowerShell):**
```powershell
'{"event_type":"identity.context.resolved","emitted_at":"2026-07-06T08:00:00Z","schema_version":"1.0","source_service":"identity-context-svc","payload":{"principal_id":"u-001","tenant_id":"t-001","legal_entity_id":"e-001","session_context_id":"s-001","correlation_id":"c-001"}}' | kcat -P -b localhost:9092 -t zoiko.identity.events -H "X-Event-ID=test-evt-001"
```

### 3. Verify Ingested Data Landed
Check that `audit-event-store-svc` successfully consumed and saved the event.

**On Bash (Linux/macOS/WSL):**
```bash
psql "host=localhost dbname=audit_event_store user=postgres password=postgres sslmode=disable" \
     -c "SELECT event_id, event_type, tenant_id, stored_at FROM audit_events ORDER BY stored_at;"
```

**On Windows (PowerShell):**
```powershell
psql "host=localhost dbname=audit_event_store user=postgres password=postgres sslmode=disable" `
     -c "SELECT event_id, event_type, tenant_id, stored_at FROM audit_events ORDER BY stored_at;"
```

### 4. Shutdown and Cleanup
To tear down the container stack and remove the persistent DB volume:
```powershell
docker compose -f deployments/docker-compose.yml down -v
```
