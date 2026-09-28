# tenant-entity-registry-svc — Operational Runbook

ORG-02 (Tenant) and ORG-03 (Legal Entity). Port 8081. Objectives in `SLO.md`;
alerts in `deployments/alerts/tenant-entity-registry.rules.yml`, each of which
names the section below to open.

## 1. What this service is

The authority for tenants (the hard isolation boundary) and legal entities (the
accounting/legal anchor): lifecycle, home region, host bindings, effective-dated
legal profiles, registry-conflict quarantine and the approvals that govern all of
it. `GET /v1/resolve-tenant` is on the isolation path of every request in the
estate. Every write is authorized by authorization-svc and **fails closed** when
it is unreachable; every event is written to `event_outbox` inside its write's
transaction and delivered by an in-process relay.

## 2. First response

```bash
curl -s localhost:8081/healthz; curl -s localhost:8081/readyz
curl -s localhost:8081/metrics | grep -E '^(readiness_up|outbox_)'
scripts/audit.sh            # 58 live checks; anything FAIL is the lead
docker logs --since 10m tenant-entity-registry-svc | grep '"level":"error"'
```

## 3. Availability

### 3.1 Service not ready (`TenantRegistryNotReady`)
`/readyz` checks the database. Check `zoiko-postgres` health and the pool
(`db unreachable at startup` in the log means it never came up). **Boot
refusals are deliberate**: in staging/production the service refuses to start
with any dev compatibility flag set (`MAKER_CHECKER_LEGACY_BODY_APPROVER`,
`LEGACY_ENTITY_CREATE_ACTIVE`, `ONBOARDING_KEY_OPTIONAL`,
`LEGACY_PROVISIONING_INPUTS`, `EXPECTED_VERSION_OPTIONAL`), with the stub
jurisdiction validator, or without `COMMERCIAL_ACCOUNT_URL` /
`RESTRICTED_JURISDICTION_CODES`. The error names the variable; fix the
configuration, never the check.

### 3.2 Elevated 5xx (`TenantRegistryAvailabilityBurn`)
Every error body carries `error_code`. Group the log by it:
- `DEPENDENCY_UNAVAILABLE` (503) — authorization-svc, jurisdiction-rules-svc or
  commercial-account-svc is down. Correct fail-closed behaviour; restore the
  dependency. Do **not** switch to a stub.
- `INTERNAL_ERROR` (500) — a real defect; the log line `unhandled service error`
  carries the cause. Every 500 found so far was a schema/RLS mismatch
  (e.g. the 28 Sep tenant-create RLS scope bug): check the migration level first.

### 3.3 Latency (`TenantRegistryReadLatency`)
Reads are single-table, index-backed queries under RLS. Slow reads usually mean
pool exhaustion or a long transaction holding row locks (`FOR UPDATE` in status
transitions). `SELECT pid, state, now()-xact_start FROM pg_stat_activity WHERE datname='tenant_entity_registry'`.

## 4. Event delivery

### 4.1 Outbox backlog (`TenantRegistryOutboxBacklog`)
Events accumulate in Postgres when Kafka is unreachable and drain when it
returns — nothing is lost. Check the broker, then:
```sql
SELECT event_type, count(*), min(created_at), max(attempts)
  FROM event_outbox WHERE published_at IS NULL GROUP BY 1;
```
A backlog with `attempts = 0` means the relay is not running: restart the
service. Growing `attempts` means the broker refuses writes (topic, ACLs).

### 4.2 Dead letters (`TenantRegistryOutboxDeadLetters`)
An event exhausted its attempts. The fact it attests is committed; consumers are
missing it. Fix the cause, then reset it for redelivery (consumers dedupe on
`event_id`, so this is safe):
```sql
UPDATE event_outbox SET attempts = 0, next_attempt_at = now()
 WHERE published_at IS NULL AND attempts >= 8;
```

## 5. Refusals that are working as designed

### 5.1 Writes refused (`TenantRegistryAuthzRefusalsSpike`)
`AUTHORIZATION_DENIED` (403) with `basis: no_grant` in authorization-svc's log is
a missing grant. Actions are upper-snake: `TENANT_PROVISION` (platform scope),
`TENANT_SUSPEND`, `TENANT_HOME_REGION_CHANGE` (platform scope), `*_APPROVE` for
every checker, `APPROVAL_REQUEST_READ`, `ENTITY_REGISTRY_CONFLICT_READ`.

### 5.2 Codes an operator will see
| code | meaning | action |
|---|---|---|
| `VERSION_CONFLICT` | stale `expected_version` | client reloads and retries |
| `VALIDATION_FAILED` + "expected_version is required" | client sent no version | client fix; not an incident |
| `SOD_DENIED` | maker tried to approve own request | a different principal approves |
| `CONTEXT_INVALID` | host bound to another tenant (NP3), or no identity | check gateway host routing |
| `INVALID_TRANSITION` | tenant suspended/terminated, entity DRAFT | expected |
| `JURISDICTION_RESTRICTED` / `NOT_ENTITLED` | provisioning refused by policy / commercial hold | compliance / commercial |
| `IDEMPOTENCY_MISMATCH` | an Idempotency-Key reused for a different request | client bug |

## 6. Approvals

Every governed command (creation, termination, abandon, legal-name/registry/
jurisdiction/LEI changes, verification, merge/unmerge, conflict resolution,
home-region change) answers 202 with an `approval_request`; a **different**
principal releases it through `/v1/approval-requests/{id}/approve` with the
`payload_fingerprint` they reviewed. Pending requests expire after
`APPROVAL_TTL_HOURS` (168). A request whose subject moved goes `STALE` — refile.
```sql
SELECT subject_type, status, count(*) FROM approval_requests GROUP BY 1,2;
```

## 7. Reconciliation

Run after any restore, incident or manual SQL:
```bash
scripts/audit.sh                  # §13 profile-history coherence, §9 outbox, §3 RLS
```
```sql
-- entities whose as-of "now" could disagree with GetEntity (must be 0)
SELECT legal_entity_id FROM legal_entity_profile_versions
 WHERE effective_to IS NULL GROUP BY 1 HAVING count(DISTINCT effective_from) > 1;
-- tenants stuck mid-provisioning
SELECT tenant_id, provisioning_failure_reason FROM tenants WHERE lifecycle_state = 'FAILED_PROVISIONING';
```
FAILED_PROVISIONING tenants: `RetryProvisioning`, or `AbandonProvisioning`
(maker-checker; deactivates bindings and policies, deletes nothing).

## 8. Backup and restore

`scripts/backup_restore_drill.sh` dumps the database, restores it into a
throwaway copy and verifies row counts, FORCE RLS, every policy and named
capability, the control constraints and the history invariants, then drops the
copy (measured 4 s locally, 22/22, 28 Sep 2026). Production recovery is
point-in-time from the managed database; after it, run §7 and let the relay
re-deliver unpublished events. Never restore over a live database without
stopping the service — the relay would publish from a half-restored table.

## 9. Migrations

Migrations in `deployments/migrations` run only when Postgres first initialises
the volume. On an existing database apply new ones by hand, in order:
```bash
docker exec -i zoiko-postgres psql -U postgres -d tenant_entity_registry -v ON_ERROR_STOP=1 \
  < deployments/migrations/0000NN_name.up.sql
```
Every `up` has a `down`. The store test suite applies all of them from empty —
point `TEST_DATABASE_URL` at a **throwaway** database; the suite wipes it.

## 10. What not to do

- Do not set a dev compatibility flag to "unblock" production; each re-opens a closed control.
- Do not point `JURISDICTION_RULES_URL` at the default to silence a dependency outage — that selects the accept-everything stub.
- Do not hard-delete tenants, entities or profile versions: history is the product.
- Do not edit `event_outbox.payload`: it is the rendered event as decided.
