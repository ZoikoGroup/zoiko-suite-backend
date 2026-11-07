# authorization-svc — Operational Runbook

**Service:** IAM / AuthZ — Evaluation Engine
**Tier:** 0. Nothing on the platform authorizes while this is down.
**Dashboard:** Grafana → `authorization-svc (IAM)` (uid `authorization-iam`)

---

## 1. What this service is, in one paragraph

It answers `POST /v1/authorize` — the single point every service on the
platform calls before a material write. Four layers, in order: RBAC (direct
grants), delegated authority, Separation of Duties, ABAC conditions. Every
evaluation — grant or deny — is written to `access_decision_log` before the
response returns, with a `decision_basis` that names the layer that produced
the outcome. Fail-closed: any store error returns 503 with nothing recorded.

Two things follow from that, and they drive most of this document:

- **A refusal here is the correct outcome, not an incident.** The failure mode
  to fear is the opposite — permitting an action that should be denied.
- **A silent success is worse than a loud failure.** Most of the alerts below
  fire on things that produce no errors and no latency.

---

## 2. First response

| Symptom | Start at |
|---|---|
| Every authorization returns 503 | §3.1 |
| Every authorization returns 403 DENIED with `no_grant` | §3.2 |
| `/health` degraded | §3.3 |
| Outbox backlog alert | §4.1 |
| SoD unavailable alert | §5.1 |
| Authorization denied events not reaching SIEM | §6.1 |
| Delegated access not working | §7.1 |
| Cache not invalidating | §8.1 |

```bash
# The three commands worth running before anything else.
kubectl -n zoiko get pods -l app=authorization-svc
kubectl -n zoiko exec deploy/authorization-svc -- wget -qO- localhost:8089/health | jq
kubectl -n zoiko logs deploy/authorization-svc --since=15m | grep -E 'ERROR|FATAL'
```

---

## 3. Availability

### 3.1 Total authorization outage (every request 503)

Check, in this order, because each rules out the next:

1. **Is the pod up?** Tier 0 fails fast on startup if Postgres or Kafka are
   unreachable, so a `CrashLoopBackOff` usually means a dependency, not a bug.
   The fatal log line names which.

2. **Is Postgres reachable and RLS bound?** This service connects as
   `app_authorization` (NOSUPERUSER NOBYPASSRLS). If the connection installs
   neither `app.tenant_id` nor `app.platform_scope`, every query matches
   **zero rows** and every evaluation returns 503 "store unavailable". The
   readiness probe (`/readyz`) includes a tenant-scoped query and will show
   `degraded`.

   ```bash
   kubectl -n zoiko exec deploy/authorization-svc -- \
     wget -qO- localhost:8089/readyz
   ```

3. **Is the outbox relay running?** A stopped relay produces no errors and no
   latency — every request succeeds. The only symptom is that governance events
   stop reaching the estate. Check the relay started:

   ```bash
   kubectl -n zoiko logs deploy/authorization-svc | grep 'outbox relay started'
   ```

   A restart is safe and is the correct first action — the relay is stateless
   and `FOR UPDATE SKIP LOCKED` makes concurrent relays safe.

4. **Is `AUTHZ_SERVICE_URL` set and real?** In `production`/`staging` the
   service refuses to start with a placeholder (unset, portless, loopback or a
   reserved domain). A production pod left at the default tier builds the
   permit-all client and says so in a warning log. Confirm it at startup:

   ```bash
   kubectl logs deploy/authorization-svc | grep -i "authorization client"
   # want: "using HTTP authorization client"  url=http://authorization-svc:8089
   # NOT:  "using PERMIT-ALL authorization stub"
   ```

### 3.2 Every authorization returns 403 DENIED with `no_grant`

This means the evaluation engine is running but finding no grants. Check:

1. **Is the cache stale?** `AUTHZ_CACHE_TTL_SECONDS` (default 5s) caches the
   five evaluation reads. A cache hit never skips the decision artifact
   (`RecordAccessDecision`), but a stale cache means the principal's current
   grants are not seen.

   ```bash
   kubectl -n zoiko exec deploy/authorization-svc -- \
     wget -qO- localhost:8089/metrics | grep 'authorization_cache'
   ```

   If the cache metrics show high hit rate but denials, the cache may need
   invalidation. The watermark (`authz_versions.assignment_version`) bumps on
   every assignment write — check it moved.

2. **Are the grants actually in the database?** Run as the SERVICE user,
   not a superuser — a superuser bypasses RLS and the query will succeed,
   telling you nothing.

   ```sql
   -- Run as app_authorization
   SELECT set_config('app.tenant_id', '<tenant>', false);
   SELECT * FROM principal_role_assignments
     WHERE principal_id = '<principal>'
       AND effective_from <= NOW() AND effective_to > NOW()
       AND NOT (suspended_at IS NOT NULL AND (resumed_at IS NULL OR suspended_at > resumed_at));
   ```

3. **Is the tenant registry answering?** It is a fail-closed dependency for
   entity scope resolution. A registry outage makes every request with a
   legal entity 503.

### 3.3 `/health` degraded

`checks` names the failing dependency. The ones that are NOT fatal and will
say so:

- `outbox: unreadable` — a query problem, not availability. Postgres is checked
  separately.
- `outbox_dead_letter: N` — reported, never degrading. See §4.2.

---

## 4. Outbox

The outbox is the single most important thing to watch, because **a stopped
relay produces no errors and no latency**. Every request succeeds. The only
symptom is that governance events stop reaching the estate.

### 4.1 Backlog growing (`authorization_outbox_pending`)

A spike during a broker restart is the outbox doing its job — events are
durable in Postgres and will be delivered. A line that only goes up is not.

```sql
-- How far behind, and since when.
SELECT count(*) AS pending,
       min(created_at) AS oldest,
       max(attempts)   AS worst_attempts
  FROM event_outbox
 WHERE published_at IS NULL;

-- What the relay last complained about.
SELECT event_type, attempts, next_attempt_at, left(last_error, 200)
  FROM event_outbox
 WHERE published_at IS NULL AND last_error IS NOT NULL
 ORDER BY attempts DESC
 LIMIT 20;
```

Read the result:

| `last_error` | Cause | Action |
|---|---|---|
| `dial tcp ... connect: connection refused` | Broker down | Fix Kafka. The backlog drains on its own. |
| `Unknown Topic Or Partition` | Topic missing and auto-create off broker-side | Create `zoiko.authorization.events`. |
| `context deadline exceeded` | Broker slow, not down | Check broker health before touching this service. |
| *empty, and `pending` is rising* | **Relay is not running** | See below. |

A rising backlog with **no** `last_error` means nothing is even attempting
delivery. Check the relay started:

```bash
kubectl -n zoiko logs deploy/authorization-svc | grep 'outbox relay started'
```

A restart is safe and is the correct first action — the relay is stateless and
`FOR UPDATE SKIP LOCKED` makes concurrent relays safe, so nothing is lost or
duplicated by restarting one.

**Do not delete rows to clear a backlog.** Every row is evidence of an
authorization decision that already happened.

### 4.2 Dead letters (`authorization_outbox_dead_letter`)

Events that exhausted `OUTBOX_RELAY_MAX_ATTEMPTS`. They are **not deleted** —
they stay with `last_error` set, which is how you find them. Nothing drains
automatically; this needs a human, and a restart will not fix one.

```sql
SELECT event_id, event_type, tenant_id, attempts, left(last_error, 300)
  FROM event_outbox
 WHERE published_at IS NULL AND attempts >= 12;
```

Once the cause is fixed, reset them to be retried:

```sql
-- Deliberately explicit about which rows. Never a blanket UPDATE.
UPDATE event_outbox
   SET attempts = 0, next_attempt_at = NOW(), last_error = NULL
 WHERE event_id IN ('evt-...', 'evt-...');
```

---

## 5. Security refusals

### 5.1 SoD unavailable (`authorization_sod_checks_total{outcome="unavailable"}`)

This means `jurisdiction-rules-svc` cannot be reached for a jurisdiction-scoped
SoD rule. **No privileged command can be issued at all** while this is down.
This is correct — a conflict check that did not run is not a check that passed
— but it means SoD is unavailable during an incident, which is exactly when it
is wanted.

```bash
kubectl -n zoiko exec deploy/authorization-svc -- wget -qO- "$JURISDICTION_SERVICE_URL/health"
```

There is no bypass, and adding one would defeat the control. If
jurisdiction-rules-svc is down during a genuine emergency, that is an incident
for the jurisdiction team.

---

## 6. SIEM streaming

### 6.1 Authorization denied events not reaching SIEM

`authorization.denied` and `sod.violation.detected` are streamed to
siem-integration-svc over HTTP. The client (`internal/siem`) is now
non-blocking: a bounded queue, four workers delivering on their own background
context, drops counted and logged, `Close` draining on shutdown.

If events are not appearing in SIEM:

1. **Is the queue full?** Check the drop counter:

   ```bash
   kubectl -n zoiko exec deploy/authorization-svc -- \
     wget -qO- localhost:8089/metrics | grep 'authorization_siem_queue_drops_total'
   ```

   A non-zero value means the siem-integration-svc is slow or down and the
   queue filled. Check the SIEM service.

2. **Is the SIEM service up?**

   ```bash
   kubectl -n zoiko exec deploy/authorization-svc -- \
     wget -qO- "$SIEM_SERVICE_URL/health"
   ```

3. **Are the events being produced?** Check the outbox:

   ```sql
   SELECT count(*) FROM event_outbox
    WHERE event_type IN ('authorization.denied', 'sod.violation.detected')
      AND published_at IS NOT NULL
      AND created_at > NOW() - INTERVAL '5 minutes';
   ```

---

## 7. Delegated authority

### 7.1 Delegated access not working

Delegated authority comes from `delegated-authority-svc` via Kafka
(`authority.delegated`, `authority.revoked`, `authority.expired` on
`zoiko.delegated-authority.events`). The consumer projects these into
`delegated_authorities` table.

Check:

1. **Is the Kafka consumer running?**

   ```bash
   kubectl -n zoiko logs deploy/authorization-svc | grep 'delegated-authority consumer'
   ```

2. **Are the events reaching the outbox?**

   ```sql
   SELECT count(*) FROM event_outbox
    WHERE event_type = 'authority.ended'
      AND created_at > NOW() - INTERVAL '5 minutes';
   ```

3. **Is the projection correct?** The delegator's grants must be live at the
   time of evaluation — a delegation can never confer an action its delegator
   does not currently hold.

   ```sql
   -- As app_authorization
   SELECT set_config('app.tenant_id', '<tenant>', false);
   SELECT * FROM delegated_authorities
    WHERE delegate_principal_id = '<delegate>'
      AND effective_from <= NOW() AND effective_to > NOW()
      AND revocation_status = 'ACTIVE';
   ```

---

## 8. Cache invalidation

### 8.1 Cache not invalidating

The evaluation reads are cached with TTL `AUTHZ_CACHE_TTL_SECONDS` (default 5s).
The watermark (`authz_versions.assignment_version`) bumps on every assignment
write and is used for cache invalidation.

If cache hits show stale data:

1. **Is the watermark moving?** Check the version:

   ```sql
   SELECT assignment_version FROM authz_versions;
   ```

   It should increment on every assignment write (grant, suspend, resume,
   revoke, expire, schedule-end).

2. **Is the cache tier invalidating on the watermark?** The cache decorator
   (`internal/cache`) checks the watermark before serving a cached read.

3. **Are the writes going through the cache?** Only writes that pass through
   the service's store methods bump the watermark. Raw SQL bypasses it.

---

## 9. ABAC (Attribute-Based Access Control)

### 9.1 ABAC rules silently refusing

`abac_rules` ships with zero rows. If rules are added, they are **deny-only**:
they can remove an action the RBAC/delegation layers granted; they can never
add one.

If a rule denies an action unexpectedly:

1. **Is the attribute present in the request?** An absent attribute **denies** a
   `REQUIRE` rule and **permits** a `FORBID` one. The operator cannot evade a
   `REQUIRE` rule by omitting a JSON field.

2. **Is the operator supported?** An operator or effect the evaluator cannot
   execute is refused at authoring time (400) and, if one reaches evaluation
   anyway, denies with basis `abac:rule_unevaluable=<code>` and an ERROR log.

3. **Is the rule active and in scope?** Platform-wide rules require
   `ABAC_RULE_MANAGE_GLOBAL` at platform scope and cannot be retired from any
   one tenant's console.

---

## 10. Retention (`access_decision_log`)

### 10.1 Partitions not being created

The `access_decision_log` table is partitioned by month (migration 000009).
`internal/retention` runs immediately on start and then every
`AUTHZ_RETENTION_SWEEP_INTERVAL_HOURS` (default 24), extending the runway
(current month + `AUTHZ_PARTITION_MONTHS_AHEAD`, default 3) and detaching
aged partitions.

If partitions are missing:

1. **Is the sweeper running?**

   ```bash
   kubectl -n zoiko logs deploy/authorization-svc | grep 'retention sweep'
   ```

2. **Does `app_authorization` have EXECUTE on the helper functions?**
   Migration 000011 made them `SECURITY DEFINER` and granted EXECUTE to
   `app_authorization`.

   ```sql
   \df+ create_access_decision_log_partition
   \df+ detach_access_decision_log_partitions_before
   ```

   Both should show `SECURITY DEFINER` and grant to `app_authorization`.

3. **Is the default partition catching rows?** If a month elapses without a
   partition, rows go to `access_decision_log_default` — check the retention
   status:

   ```sql
   SELECT * FROM access_decision_log_retention_status;
   ```

---

## 11. Configuration that changes behaviour

| Variable | Effect if wrong |
|---|---|
| `AUTHZ_SERVICE_URL` | In `production`/`staging` a placeholder (unset, portless, loopback or a reserved domain) refuses to start. Anywhere else it selects a **permit-all stub**. |
| `AUTHZ_ENV` | Optional; defaults to `DEPLOY_ENVIRONMENT`. Must agree with it; a downgrade in production is refused at startup. |
| `JURISDICTION_SERVICE_URL` | Required in staging/production; the service refuses to start without it. |
| `AUTHZ_CACHE_TTL_SECONDS` | `0` disables the cache. Default 5s. Higher = more stale reads; lower = more DB load. |
| `AUTHZ_PARTITION_MONTHS_AHEAD` | Runway for partition creation. Default 3. If 0, a service starting into a new month has no partition. |
| `AUTHZ_ACCESS_DECISION_RETENTION_MONTHS` | Retention window. `0` = off (detach disabled). Default 24. Negative = "do not detach". |
| `AUTHZ_RETENTION_SWEEP_INTERVAL_HOURS` | Sweep interval. Default 24. |
| `OUTBOX_RELAY_MAX_ATTEMPTS` | Dead-letter threshold. Default 12. |
| `SIEM_SERVICE_URL` | If unset, streaming is off. If set but service down, queue fills and drops. |

**The one to get right is `AUTHZ_SERVICE_URL` + `DEPLOY_ENVIRONMENT=production`.**
Every authorization check this service makes is delegated to itself (the
permit-all stub is only for non-production tiers). A production pod left at
the default tier builds the permit-all client and says so in a warning log.
Confirm it at startup:

```bash
kubectl logs deploy/authorization-svc | grep -i "authorization client"
# want: "using HTTP authorization client"  url=http://authorization-svc:8089
# NOT:  "using PERMIT-ALL authorization stub"
```

---

## 12. What NOT to do

- **Do not delete outbox rows** to clear a backlog. They are governance
  evidence for decisions that already happened.
- **Do not run migrations on startup.** They go through golang-migrate in
  CI/CD. Migration 000009 replaces the RLS policies on a partitioned table;
  an auto-run racing a second replica is how a tenant-isolation policy ends
  up missing.
- **Do not "fix" a SoD denial by removing the rule.** The rule is the control.
  If it was authored by mistake, use `POST /v1/admin/sod-rules/{id}/retire`
  (added in 2026-09-08 fourth pass) — it is idempotent and the rule stays
  readable because a denial recorded as `sod:conflict_with=<action>` is only
  explainable while its rule can be looked up.
- **Do not grant yourself a delegated authority.** The schema refuses a
  delegation whose delegator is not the caller.
- **Do not bypass the cache invalidation.** The watermark is the only
  mechanism; manual cache eviction is not provided.
- **Do not set `AUTHZ_ENV` independently of `DEPLOY_ENVIRONMENT`.** It must
  agree; a downgrade in production is refused at startup.

---

## 13. Load / NFR verification

The in-process budget is guarded in CI:

```bash
go test ./internal/handler -run 'TestAuthorizeLatency' -v
go test ./internal/handler -bench=. -benchmem -run=^$
```

End-to-end against a running stack — this is the number the P99 target
refers to, since the CI test stubs out the registries and Postgres:

```bash
# 200 concurrent, 60s, against a seeded local stack.
hey -z 60s -c 200 -m POST \
  -H 'Content-Type: application/json' \
  -H 'X-Tenant-Id: <tenant>' -H 'X-Principal-Id: <principal>' \
  -H 'X-Legal-Entity-Id: <entity>' -H 'Idempotency-Key: <key>' \
  -d '{"principal_id":"<principal>","legal_entity_id":"<entity>","action_type":"payment.approve"}' \
  http://localhost:8089/v1/authorize
```

**Acceptance:** P99 < target ms per request, zero 5xx, and — check this —
`authorization_outbox_pending` returns to its baseline afterwards. A load test
that leaves a permanent backlog means the relay cannot keep up with peak write
rate, which no latency percentile will tell you.

---

## 14. Second factor (TOTP) and step-up

Authorization-svc refuses to issue or approve a JIT or break-glass session
unless the caller's envelope carries `mfa_verified` with an `auth_time` at
most 15 minutes old. This is enforced in `internal/handler/step_up.go`.

---

## 15. Federation and SCIM

Off unless the operator turns it on. With `FEDERATION_ENABLED=false` (the
default) none of the `/v1/federation/*` or `/scim/v2/*` routes are mounted
(404), and the console's Federation tab says so.