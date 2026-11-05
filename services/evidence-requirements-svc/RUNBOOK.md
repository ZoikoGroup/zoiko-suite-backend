# evidence-requirements-svc — Runbook

Port 8130. Decides whether the evidence required before an action may complete exists
(Doc 03 §8.6), records every determination in an append-only ledger, and publishes
`evidence.requirement.satisfied` / `.missing`. Also runs evidence requests (PBC).

## Configuration

| Variable | Required | Local default | Meaning |
|---|---|---|---|
| `ENV` | — | `local` | `local`/`dev`/`development`/`test` relax the start-up refusals below |
| `PORT` | — | 8130 | |
| `DB_HOST` `DB_PORT` `DB_NAME` | yes | `postgres` `5432` `evidence_requirements` | |
| `DB_USER` / `DB_PASSWORD` | yes | `app_evidence_requirements` / `postgres` | The per-service role (create-app-roles.sh): NOSUPERUSER, NOBYPASSRLS, CONNECT on this database only. `zoiko_app` cannot connect. |
| `KAFKA_BROKERS` `KAFKA_EVENTS_TOPIC` | yes | `kafka:9094` `zoiko.evidence-requirements.events` | |
| `AUTHZ_SERVICE_URL` | yes | `http://authorization-svc:8089` | Every gate; unreachable = 503 (fail closed) |
| `AUTHZ_MTLS_ENABLED` / `AUTHZ_MTLS_URL` | no | `false` | Production mTLS client to authorization-svc :8449 |
| `DOCUMENT_VAULT_SERVICE_URL` | yes | `http://document-vault-svc:8094` | SUPPORTING_DOCUMENT verification |
| `EVIDENCE_MANIFEST_SERVICE_URL` | outside local | `http://evidence-manifest-svc:8095` | EVIDENCE_MANIFEST verification; unset locally = those references are 503 |
| `ESIGNATURE_SERVICE_URL` | outside local | `http://host.docker.internal:8148` | SIGNATURE verification (phase7 stack); unset locally = 503 |
| `TENANT_REGISTRY_URL` | outside local | `http://tenant-entity-registry-svc:8081` | Legal entity of every evaluation/requirement |
| `CURSOR_SIGNING_KEY_HEX` | outside local | random per process | ≥ 32 bytes, hex. Signs list cursors; shared by every replica |
| `EVIDENCE_EVALUATE_AUTHZ` | — | `enforce` (compose: `warn`) | Gate mode for EVIDENCE_EVALUATE (D-ER-6) |
| `EVIDENCE_READ_AUTHZ` | — | `enforce` | Gate mode for the reads and lists |
| `IDENTITY_VERIFY` | — | `require` (compose: `warn`) | identity-envelope gate; an invalid envelope is 401 in both modes |
| `IDENTITY_JWKS_URL` `IDENTITY_INTROSPECT_URL` `IDENTITY_ISSUER` | yes | identity-context-svc | Envelope keys and session revocation (TTL 5 s) |
| `OTEL_EXPORTER_OTLP_ENDPOINT` | — | `http://otel-collector:4318` | Traces |

### Start-up refusals
Outside local/dev the process exits when any of these is unset:
`EVIDENCE_MANIFEST_SERVICE_URL`, `ESIGNATURE_SERVICE_URL`, `TENANT_REGISTRY_URL`,
`CURSOR_SIGNING_KEY_HEX` (or shorter than 32 bytes). It also exits when Postgres is
unreachable at start, or when the identity gate configuration is refused.

## Build, migrate, run (local)

```bash
cd deployments
docker compose -p deployments build evidence-requirements-svc          # root context
docker images | grep evidence-requirements                               # check the timestamp
python3.14 migrate.py up --db evidence_requirements                      # 000001–000011
docker compose -p deployments up -d --no-deps --no-build --force-recreate evidence-requirements-svc
powershell -File scripts/seed-demo-rbac.ps1                              # EVIDENCE_FULL bundle
```

Verify: `bash services/evidence-requirements-svc/scripts/audit.sh` (service behaviour) and
`python3.14 deployments/scripts/e2e-evidence-requirements-deps.py [--with-outage]` (every
dependency edge). Both exit with the number of failures.

Migrations 000007–000009 are not idempotent: on a scratch database, recreate it between
runs.

## Seeding the catalogue

A requirement is data, never code: `POST /v1/admin/evidence-requirements` with
`domain_code`, `action_type`, `evidence_type`, `requirement_payload`
(`{"minimum_count":2,"artifact_subtype":"BANK_STATEMENT","description":"…"}`) and
`legal_entity_id` (omit for tenant-wide, authorized at PLATFORM). Or the console:
/admin/evidence → Add requirement. Retire by end-dating (If-Match required), never delete.

## Operations

### An owner is down (503 `document_service_unavailable` / `evidence_manifest_unavailable` / `esignature_unavailable`)
The evaluation was refused and **nothing was recorded**; the caller retries with the same
Idempotency-Key. Metric: `evidence_owner_unavailable_total{owner}`; alert
`EvidenceOwnerUnavailable`. Restore the owner. A 403 from the owner is treated the same
way (the service cannot confirm the artifact): EVIDENCE_MANIFEST references currently
always land here because evidence-manifest-svc's `EVIDENCE_MANIFEST_READ` is not
registered in access-control.

### Outbox backlog
`evidence_outbox_unpublished` > 0 for 10 minutes (alert `EvidenceOutboxBacklog`):
decisions are recorded, their events are waiting. Check Kafka and the log line
`outbox: failed to publish`. The worker retries every 5 s; nothing is lost. Count by hand:
`SELECT outbox_unpublished_count();` (any role).

If evaluations answer 503 `store_unavailable` and the log shows `new row violates
row-level security policy for table "outbox"`, migration 000011 is not applied.

### Gate modes
Compose runs evaluate and identity in `warn` (D-ER-6). A refusal in warn is logged
(`authorization gate (warn mode): would be refused; admitted`) and recorded in
authorization-svc's decision log. Switch to `enforce`/`require` once the calling services
send the envelope and their principals hold EVIDENCE_EVALUATE.

### 412 / 428
428: the client sent no If-Match. 412: the record changed since the client read it; the
response carries the current ETag. Read again and retry.

### 409 `idempotency_mismatch`
The same Idempotency-Key was sent with a different body. The original answer stands; send
a new key for a new request.

### 400 `invalid_cursor`
A list cursor was tampered with, came from another tenant or another filter set, or was
signed by another key (a restart without `CURSOR_SIGNING_KEY_HEX`). Start from page one.

## Metrics
`evidence_evaluations_total{outcome}`, `evidence_owner_unavailable_total{owner}`,
`evidence_outbox_unpublished`, `http_requests_total`, `http_request_duration_seconds`,
`readiness_up`. Alerts: `deployments/prometheus-rules.yml`, group `evidence-requirements`.

## Rollback
Redeploy the previous image. Migrations 000010/000011 have down files; rolling back 000011
restores the outbox policy under which the runtime role cannot write events, so roll the
image back first and keep 000011 unless the previous image connected as a superuser.
