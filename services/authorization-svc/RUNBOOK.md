# authorization-svc — Operational Runbook

**Service:** IAM / AuthZ — policy decision point (ZS-IAM-001)
**Tier:** 0. Every material write on the platform asks this service first.
**Dashboard:** Grafana → `authorization-svc (IAM / AuthZ)` (uid `authorization-svc-iam`,
`deployments/grafana/provisioning/dashboards/authorization-svc.json`)
**Alerts:** group `authorization-svc` in `deployments/prometheus-rules.yml` (8 rules)
**Rewritten:** 10 Nov 2026. The 7 Nov version named tables, columns, metrics and
settings this service does not have (`event_outbox`, `OUTBOX_RELAY_MAX_ATTEMPTS`,
dead letters, `/health`, a permit-all stub). Everything below is checked against
the code.

---

## 1. What this service is

It answers `POST /v1/authorize` and `POST /internal/authorization/decisions` with
one pipeline (`internal/handler/canonical_decisions.go`, `evaluateDecisionLayers`):
workload binding → principal status → entity standing → RBAC → delegation → JIT /
break-glass / support session → static SoD → dynamic SoD (own object, unknown
participation, cooling window, self-approval, own elevation, prior rejecting
reviewer, related party) → ABAC → delegation ceiling → authority limits (monetary
with FX, quantity) → quorum → fact-hash revalidation → step-up → terminal resource
state. The first refusal wins. Every decision is written to `access_decision_log`
before the response; if that write fails the answer is **503** and nothing is
decided.

- **A refusal is the correct outcome, not an incident.** The failure to fear is a
  PERMIT that should have been a DENY.
- **Several controls are PEP-asserted:** quorum (`approved_by`, `required_approvals`),
  fact-hash revalidation (`approved_fact_hash`, `current_fact_hash`), the cooling
  window, related party and resource state are judged from request attributes the
  calling service supplies. A PEP that omits them is not checked on them, except
  where this service fails closed (an approved hash with no current hash, an
  amount with no currency against a limit).

---

## 2. First response

| Symptom | Start at |
|---|---|
| Every authorization answers 503 | §3.1 |
| Everything DENIED `no_grant` | §3.2 |
| `/readyz` not ready | §3.3 |
| `AuthorizationDecisionRecordFailures` | §3.1 |
| `AuthorizationOutboxBacklog` / `…Stuck` | §4 |
| `AuthorizationProjectionLagHigh` (revocations late) | §6 |
| `AuthorizationCacheWatermarkStale` | §7 |
| `AuthorizationPDPLatencyHigh` | §11 |
| `AuthorizationBreakGlassActive` | §8 |
| `AuthorizationSoDDenialSpike` | §9 |
| Denials not reaching SIEM | §5 |

```bash
# Local compose (container authorization-svc; plaintext port bound to loopback).
docker ps --filter name=authorization-svc
curl -s 127.0.0.1:8089/readyz            # {"status":"ok"} or db_unreachable
docker logs --since 15m authorization-svc 2>&1 | grep -E '"level":"(error|fatal)"'
```

---

## 3. Availability

### 3.1 Every request 503

1. **Did it start?** Startup is fatal on: Postgres unreachable (`db unreachable at
   startup`), an invalid `AUTHZ_FX_REFERENCE_RATES`, and, outside `ENV=local`, a
   missing `AUTHZ_ENTITY_REGISTRY_URL` or mTLS require mode without a listener /
   allow-list (`Config.Validate`). With `MTLS_ENABLED=true` it also needs
   mtls-management-svc to provision its server certificate.
2. **Can it write decisions?** `authorization_decision_record_failures_total`
   rising means the `access_decision_log` insert fails. Check Postgres write
   availability and the partitions (§10). The app role is `app_authorization`
   (NOSUPERUSER NOBYPASSRLS).
3. **Is it the registry?** A legal entity the projection has never seen is
   checked with tenant-entity-registry-svc; registry down → 503 for those
   entities only (`AUTHZ_ENTITY_REGISTRY_URL`).
4. **Is it the identity plane?** Admin, session and console routes verify the
   envelope against identity-context-svc's JWKS and introspect the session
   (`AUTHZ_IDENTITY_REVOCATION_FAIL=closed` by default) — identity down refuses
   those routes, never `/v1/authorize`.

### 3.2 Everything DENIED `no_grant`

The principal holds nothing in that scope. Read the decision:

```bash
curl -s "127.0.0.1:8089/v1/access-decisions?principal_id=<p>&limit=5" \
  -H 'X-Tenant-Id: <t>' -H 'X-Principal-Id: <admin with iam.policy.read>' ...
```

`decision_basis` names the layer. Then check the assignments
(`GET /v1/admin/role-assignments?principal_id=`): `approval_status` must be
`APPROVED` (not `PENDING_APPROVAL` / `SUSPENDED`) and the window open. An
assignment scoped to one legal entity does not grant in another; admin powers
are checked at **tenant** scope, so an administrator needs a tenant-wide
assignment (`legal_entity_id` empty).

### 3.3 `/readyz`

`/healthz` is liveness only. `/readyz` pings Postgres and answers
`{"status":"unavailable","reason":"db_unreachable"}` with 503 when it cannot.
Every `/metrics` scrape re-evaluates readiness (`readiness_up`).

---

## 4. Outbox (`outbox_events`, migration 000020)

Every produced event (decisions, IAM, rules, sessions, exceptions, workload
bindings, cache invalidation — `asyncapi.yaml`) is inserted in the same
transaction as its fact and published by the relay (`outbox relay started` in
the log). **A stopped relay produces no errors and no latency**: watch
`authorization_outbox_oldest_pending_seconds`.

```sql
-- authorization_svc database
SELECT count(*) AS pending, min(created_at) AS oldest, max(publish_attempts) AS worst
  FROM outbox_events WHERE published_at IS NULL;

SELECT event_type, publish_attempts, left(last_error, 200)
  FROM outbox_events
 WHERE published_at IS NULL AND last_error IS NOT NULL
 ORDER BY publish_attempts DESC LIMIT 20;
```

| `last_error` | Cause | Action |
|---|---|---|
| `connection refused` / `dial tcp` | Kafka down | Fix Kafka; the backlog drains itself. |
| `Unknown Topic Or Partition` | topic missing, auto-create off | Create `zoiko.authorization.events`. |
| empty while pending rises | relay not running | Restart the pod; the relay is stateless (`FOR UPDATE SKIP LOCKED`). |

There is **no dead-letter**: a failing row stays pending and is retried every
pass. `authorization_outbox_stuck` counts rows with 5 or more failed attempts.
**Never delete outbox rows** — each is evidence of something that happened.
Published rows are purged after `AUTHZ_OUTBOX_RETENTION_HOURS` (168) by the
retention sweep (`authz_purge_published_outbox`, 000031).

---

## 5. SIEM streaming

`authorization.denied`, SoD violations, unbound-workload attempts and session
step-up refusals are streamed to siem-integration-svc (`SIEM_SERVICE_URL`) by a
bounded, non-blocking queue (`internal/siem`). Unset URL = streaming off.

- `authorization_siem_dropped_total` rising: the queue filled because
  siem-integration-svc is slow or down. Decisions are unaffected; the events
  are still in Kafka via the outbox.
- Is it up? `curl -s http://siem-integration-svc:8141/healthz` (compose
  `docker-compose.phase6.yml`).

---

## 6. Revocation and projections

Three Kafka consumers keep the decision path current:

| Consumer (group) | Topic(s) | Projects |
|---|---|---|
| delegation (`authorization-svc-delegation-projector`) | `zoiko.delegated-authority.events` | `authority.*` → `delegated_authorities` |
| lifecycle (`authorization-svc-lifecycle`) | `zoiko.identity.events`, `zoiko.access-control.events`, `zoiko.entity.events` | principal status (layer 0), entity standing, employment exits, subject merges; grant-graph events invalidate the cache |
| invalidator (one group per replica) | the lifecycle topics + `zoiko.authorization.events` | drops this replica's cached grants |

`authorization_projection_lag_seconds{consumer}` is upstream `emitted_at` →
projected here. Measured locally on 10 Nov 2026: delegation ~0.23 s, lifecycle
~0.42 s mean. Late projections mean a suspended principal or revoked
delegation is still honoured until the event lands.

```bash
docker logs authorization-svc 2>&1 | grep -E 'lifecycle consumer started|delegation projection consumer started|principal status projected|delegation projected'
```

A change made through **this** service's admin API takes effect on the next
decision without Kafka: the version watermark (§7) moves inside the write.

---

## 7. Cache

Evaluation reads (grants, delegations, SoD, ABAC, principal status) are cached
for `AUTHZ_CACHE_TTL_SECONDS` (5). Every key includes the
`policy_version|assignment_version` watermark (`authz_versions`, 000028), bumped
by triggers on every config, assignment, delegation, principal-status and
entity-status write, and read every second. A watermark older than 3 s
**bypasses** the cache — it never serves stale grants.

- `authorization_watermark_age_seconds` > 3: reads of `authz_versions` are
  failing; decisions stay correct, latency rises.
- `authorization_cache_hits_total / (hits + misses)`: the hit ratio.
- Force a flush: `POST /v1/admin/authorization-cache/invalidate` (`iam.policy.publish`,
  §16 reason) — every replica's invalidator drops its own.

---

## 8. Privileged sessions

`authorization_sessions{kind,state}` and `authorization_sessions_expiring_soon`
are read every `AUTHZ_OPS_SAMPLE_SECONDS` (15). Any ACTIVE break-glass session
pages: confirm the incident, the approvals (quorum `AUTHZ_BREAK_GLASS_APPROVALS`),
and that a review follows. Caps: JIT 8 h, break-glass 2 h, support 4 h
(`AUTHZ_MAX_*`, refused not shortened). Issuing or approving JIT / break-glass
needs a TOTP step-up no older than `AUTHZ_SESSION_STEP_UP_MAX_AGE_SECONDS` (900)
from the verified envelope (`internal/handler/session_step_up.go`).

---

## 9. Segregation of duties and ABAC

- SoD and ABAC rule changes take effect only when someone other than the
  requester approves them (AU-17). A principal holding either action of an SoD
  rule cannot retire it.
- `AuthorizationSoDDenialSpike`: read `access_decision_log` for
  `decision_basis LIKE 'sod:%'`.
- ABAC is deny-only. An absent attribute denies a REQUIRE rule and permits a
  FORBID one; an operator the evaluator cannot run denies with
  `abac:rule_unevaluable=<code>` (and is refused at authoring, 400).
- A jurisdiction-scoped SoD rule is validated with jurisdiction-rules-svc
  (`JURISDICTION_RULES_URL`) and fails closed (503) when it cannot answer.
- **Do not "fix" a denial by retiring the rule.** Use a GOV-04 exception
  (`/v1/admin/sod-exceptions`) with a compensating control and an expiry.

---

## 10. Retention (`access_decision_log`)

Monthly partitions (000009). `internal/retention` runs at start and every
`AUTHZ_RETENTION_SWEEP_INTERVAL_HOURS` (24): it ensures the current month plus
`AUTHZ_PARTITION_MONTHS_AHEAD` (3), detaches partitions older than
`AUTHZ_ACCESS_DECISION_RETENTION_MONTHS` (24) — detach, **never** drop — and
purges published outbox rows and idempotency keys. Log lines: `retention
sweeper started`, `retention: …`.

```sql
SELECT * FROM access_decision_log_retention_status;
\df+ create_access_decision_log_partition
\df+ detach_access_decision_log_partitions_before
```

Rows for a month with no partition land in `access_decision_log_default`.
Archiving detached partitions is the operator's job.

---

## 11. Load and latency (IAM-8)

```bash
python deployments/scripts/e2e-authorization-scenarios.py --seed-only
python deployments/scripts/load-authorization.py --concurrency 200 --seconds 60
```

Measured 10 Nov 2026 on the local compose stack (one replica, `cpus: 0.5`,
`mem_limit: 256m`, Windows Docker VM with 2 GB): 200 concurrent → 321 decisions/s,
p50 578 ms, p95 1011 ms, p99 1376 ms, 0 errors; 50 concurrent → 341/s, p50 126 ms,
p99 297 ms. Throughput is CPU-bound at 0.5 CPU; higher concurrency only queues.
Revocation (admin revoke → next decision DENY) 9–35 ms. These are this
laptop stack's numbers, not a production SLO.

`authorization_evaluation_seconds` is the pipeline alone; compare it with
`http_request_duration_seconds{route="/v1/authorize"}` to separate PDP time
from queueing.

---

## 12. Configuration that changes behaviour

| Variable | Default | Effect |
|---|---|---|
| `AUTHZ_IDENTITY_ASSERTION` | `require` (`warn` in compose) | `warn` admits admin routes on `X-Principal-Id` alone, marked `X-Identity-Assertion: missing`. |
| `AUTHZ_EVALUATION_MTLS` | `require` outside local (`permit` in compose) | Evaluation routes need a client certificate whose CN is in `AUTHZ_PEP_ALLOWLIST`. |
| `AUTHZ_STEP_UP_UNKNOWN_AGE` | `step_up` (`permit` in compose) | Whether a step-up action with no stated authentication age steps up. |
| `AUTHZ_ENFORCE_TENANT_ON_AUTHORIZE` | `false` | `true` refuses a tenantless question about an entity the registry never mentioned. |
| `AUTHZ_COMMAND_CONTRACT` | `enforce` | Reason required on privileged/destructive commands; `warn` admits and marks. |
| `AUTHZ_FX_REFERENCE_RATES` | built-in GBP table | `GBP=1,USD=0.78,...`: the FX basis for authority limits; named in every converted decision (`FX_BASIS:…`, `FX_RATE:…`). Invalid → fatal at start. |
| `AUTHZ_CACHE_TTL_SECONDS` | 5 | `0` disables the cache. |
| `AUTHZ_BREAK_GLASS_APPROVALS` | 1 | 1–5 independent stepped-up approvers. |
| `AUTHZ_MAX_JIT_SECONDS` / `_BREAK_GLASS_` / `_SUPPORT_` | 8 h / 2 h / 4 h | Longer requests are refused. |
| `AUTHZ_OPS_SAMPLE_SECONDS` | 15 | Outbox and session gauge sample period. |
| `AUTHZ_ENTITY_REGISTRY_URL` | — | Required outside local. |
| `JURISDICTION_RULES_URL` | — | Jurisdiction-scoped SoD rule validation. |

Strict modes were each proven on the local stack on 10 Nov 2026 (identity
require, step-up unknown age, tenant enforce, evaluation mTLS require with an
allow-listed PEP); compose keeps the lax values so callers not yet migrated
keep working.

---

## 13. What NOT to do

- Do not delete `outbox_events` or `access_decision_log` rows.
- Do not run migrations from the service; apply them with
  `deployments/migrate.py` (filename-keyed: the duplicate 000037 / 000038
  prefixes are two different files, reconciled by 000041).
- Do not retire an SoD or ABAC rule to unblock someone; use an exception.
- Do not send `If-Match: *` from a console: it is the explicit unconditional
  form for machine callers that hold no version (access-control-svc's
  revocations), not a way round a 412.
