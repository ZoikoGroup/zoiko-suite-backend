# evidence-requirements-svc — Runbook

## Service Overview
**Port:** 8130  
**Database:** evidence_requirements (PostgreSQL)  
**Message Broker:** Kafka (topic: zoiko.evidence-requirements.events)  
**Dependencies:** postgres, kafka, authorization-svc, document-vault-svc, tenant-entity-registry-svc, identity-context-svc, evidence-manifest-svc (optional), esignature-integration-svc (optional)

## Startup Order
1. postgres (healthy)
2. kafka (healthy)
3. authorization-svc (healthy)
4. document-vault-svc (healthy)
5. tenant-entity-registry-svc (healthy)
6. evidence-requirements-svc

## Environment Variables

| Variable | Required | Default | Description |
|---|---|---|---|
| PORT | Yes | 8130 | HTTP listen port |
| DB_HOST | Yes | postgres | PostgreSQL host |
| DB_PORT | Yes | 5432 | PostgreSQL port |
| DB_NAME | Yes | evidence_requirements | Database name |
| DB_USER | Yes | zoiko_app | Database user |
| DB_PASSWORD | Yes | - | Database password |
| DB_SSLMODE | No | disable | PostgreSQL SSL mode |
| KAFKA_BROKERS | Yes | kafka:9094 | Kafka bootstrap servers |
| KAFKA_EVENTS_TOPIC | Yes | zoiko.evidence-requirements.events | Outbox event topic |
| AUTHZ_SERVICE_URL | Yes | http://authorization-svc:8089 | Authorization service URL |
| DOCUMENT_VAULT_SERVICE_URL | Yes | http://document-vault-svc:8094 | Document vault service URL |
| EVIDENCE_MANIFEST_SERVICE_URL | No* | - | Evidence manifest service URL |
| ESIGNATURE_SERVICE_URL | No* | - | Esignature service URL |
| TENANT_REGISTRY_URL | Yes | - | Tenant entity registry URL |
| EVIDENCE_EVALUATE_AUTHZ | No | enforce | Evaluate authz mode (enforce/warn) |
| EVIDENCE_READ_AUTHZ | No | enforce | Read authz mode (enforce/warn) |
| IDENTITY_VERIFY | No | warn | Identity verification mode |
| IDENTITY_JWKS_URL | No | http://identity-context-svc:8080/.well-known/jwks.json | JWKS URL |
| IDENTITY_INTROSPECT_URL | No | http://identity-context-svc:8080/v1/context/session/introspect | Session introspection URL |
| IDENTITY_ISSUER | No | identity-context-svc | JWT issuer |
| OTEL_EXPORTER_OTLP_ENDPOINT | No | http://otel-collector:4318 | OTLP exporter endpoint |
| CURSOR_SIGNING_KEY | No* | local-dev default | HMAC key for cursor signing |

*Required outside local/dev; unset locally means the evidence type is verified as unavailable (503).

## Health Checks
- **Liveness:** `GET /healthz` — process is up
- **Readiness:** `GET /readyz` — database connectivity verified
- **Metrics:** `GET /metrics` — Prometheus scrape endpoint

## Database Migrations
Migrations are in `deployments/migrations/` and run automatically on container start via the `migrate` tool. Key migrations:
- 000001: Initial schema (evidence_requirements, evidence_evaluations, outbox)
- 000004: PBC requests (evidence_requests, evidence_request_responses, etc.)
- 000005: Outbox table
- 000006: Idempotency constraint on correlation_id
- 000007: Evaluation ledger immutability trigger
- 000008: Transactional outbox
- 000009: Ledger guard trigger
- 000010: record_version for API-3 optimistic concurrency

## Common Operations

### Seed Demo RBAC
```bash
pwsh deployments/scripts/seed-demo-rbac.ps1
```
Grants EVIDENCE_FULL bundle to governance roles.

### View Active Requirements
```bash
curl -H "Authorization: Bearer <token>" \
  "http://localhost:8130/v1/evidence-requirements?page_size=50"
```

### Evaluate Evidence
```bash
curl -X POST -H "Authorization: Bearer <token>" \
  -H "Idempotency-Key: <uuid>" \
  -H "X-Legal-Entity-Id: <uuid>" \
  -H "Content-Type: application/json" \
  "http://localhost:8130/v1/evidence/evaluate" \
  -d '{"legal_entity_id":"<uuid>","domain_code":"FINANCE","action_type":"JOURNAL_POST","present_artifacts":[{"evidence_type":"SUPPORTING_DOCUMENT","reference_id":"<doc-id>"}]}'
```

### Retire a Requirement
```bash
curl -X POST -H "Authorization: Bearer <token>" \
  -H "Idempotency-Key: <uuid>" \
  -H "If-Match: <record_version>" \
  -H "Content-Type: application/json" \
  "http://localhost:8130/v1/admin/evidence-requirements/<req-id>/end-date" \
  -d '{"reason":"superseded by new policy","effective_to":"2026-12-31T23:59:59Z"}'
```

## Troubleshooting

### 503 on Evaluate — Document Vault Unavailable
**Symptom:** `POST /v1/evidence/evaluate` returns 503 with `store_unavailable` or `document_service_unavailable`
**Cause:** document-vault-svc is down or network partition
**Action:** Check document-vault-svc health; the evaluation fails closed (no row written)

### 503 on Evaluate — Identity Session Unavailable
**Symptom:** `POST /v1/evidence/evaluate` returns 503 with `identity_status_unavailable`
**Cause:** identity-context-svc session introspection failed
**Action:** Check identity-context-svc health; the request fails closed (AU-01)

### 412 Precondition Failed — Version Mismatch
**Symptom:** `POST /v1/admin/evidence-requirements/{id}/end-date` returns 412 with `version_mismatch`
**Cause:** The requirement was modified by another request since the client read it
**Action:** Client should re-read the requirement (GET), then retry with the new If-Match value

### 409 Idempotency Mismatch
**Symptom:** Reusing an Idempotency-Key with a different request body
**Action:** Generate a new Idempotency-Key for each unique evaluation

### 403 Tenant Scope Mismatch
**Symptom:** Caller sends X-Tenant-Id that doesn't match the gateway-verified tenant
**Action:** Ensure gateway-auth-svc is correctly setting X-Tenant-Id from the verified envelope

### Outbox Events Not Publishing
**Symptom:** `unpublished_outbox_older_than_5_minutes` > 0 in `/v1/evidence/summary`
**Cause:** Kafka broker unavailable or outbox worker stopped
**Action:** Check Kafka connectivity; restart evidence-requirements-svc to restart outbox worker

## Key Metrics to Watch
- `http_requests_total{route="/v1/evidence/evaluate",status_code="503"}` — evaluate failures
- `readiness_up` — should be 1
- `unpublished_outbox_older_than_5_minutes` — should be 0
- `http_request_duration_seconds{route="/v1/evidence/evaluate"}` — p99 < 500ms

## Logs
Structured JSON logs via zap. Key fields:
- `service`: "evidence-requirements-svc"
- `evaluation_id`: for evaluation traces
- `tenant_id`: tenant scope
- `correlation_id`: end-to-end trace

## Security
- Runs as non-root user in distroless container
- mTLS to authorization-svc configurable via `AUTHZ_MTLS_ENABLED`
- Identity envelope verification via identity-context-svc (AU-01, AU-27)
- Row-level security (RLS) on all tenant-scoped tables
- Explicit `tenant_id` filter in every query (defense in depth)

## Rollback Procedure
1. Stop the new container
2. Start the previous container image
3. Run down migrations if schema changed: `migrate -path deployments/migrations down 1`
4. Verify `/readyz` returns 200

## Contact
On-call: Platform Governance team  
Slack: #platform-governance  
Repo: zoiko-suite-backend-platform/services/evidence-requirements-svc