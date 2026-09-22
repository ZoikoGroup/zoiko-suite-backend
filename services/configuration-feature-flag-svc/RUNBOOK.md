# configuration-feature-flag-svc — Runbook

Operational guide for the service that owns runtime configuration values and
environment-aware feature flags.

---

## 1. What this service is, and what it is not

**Owns:** versioned, effective-dated runtime configuration values and feature
flags, scoped to `(key, environment, tenant_id)`.

**Does not own:** authorization decisions, policy evaluation, or evidence
requirements. It is *not* one of the seven Governance Control Plane engines and
is not on the non-bypassable governance path.

The constraint from `03-microservices.md` §9.6, verbatim:

> Configuration may tune service behavior, but must never be used to bypass
> governance doctrine.

That is satisfied **by scope, not by code here**: this service only stores and
serves values. It never calls Policy, Authorization or Evidence Requirements to
make a decision. But it cannot stop a careless *consumer* from wiring a flag as
a bypass — so whenever a new consumer of this service appears, that is the thing
to check in review.

### The two properties every operator needs to know

**Nothing is ever overwritten.** A change is a new row plus an end-dated
predecessor, in one transaction. There is no `UPDATE` of a value and no
`DELETE`. If you need to know what a value was last Tuesday, it is still there.

**A missing record is not a default.** This service never guesses. A consumer
that treats a `404` on a feature flag as "enabled" has a safety bug that no
change here can prevent.

---

## 2. Vital signs

| Where | What |
| --- | --- |
| `GET /healthz` | Liveness. Answers whenever the process is up. Checks nothing on purpose. |
| `GET /readyz` | Readiness. Names **each** component: `database` and `authorization-svc`. |
| `GET /metrics` | Prometheus. Re-evaluates readiness on every scrape. |

```bash
curl -s localhost:8086/readyz | python -m json.tool
```

A healthy answer names both components:

```json
{ "status": "READY", "components": { "database": "ok", "authorization-svc": "ok" } }
```

### The metrics that matter

| Series | Reads as |
| --- | --- |
| `configuration_config_writes_total{outcome}` | Every `POST /v1/config` by outcome. |
| `configuration_flag_writes_total{outcome}` | Every `POST /v1/flags` by outcome. |
| `configuration_authz_decisions_total{action,outcome}` | `granted` / `denied` / `unavailable`, per action. |
| `configuration_global_scope_writes_total{resource,outcome}` | Writes to the environment-wide default. |
| `configuration_outbox_pending` | Unpublished event backlog. |
| `configuration_outbox_oldest_age_seconds` | **The one to watch.** See 4.1. |
| `configuration_outbox_published_total{event_type}` | Events handed to Kafka. |
| `configuration_outbox_failures_total` | Relay drains that failed. |
| `readiness_up` | 1 when the last readiness evaluation passed. |

Every label value is pre-created at zero at startup. That is deliberate: a
series that has never been observed and one reading zero are indistinguishable
to an alert expression, so a rule written to catch the *first* occurrence of
something would otherwise stay silent through exactly the event it exists for.

---

## 3. The distinctions that cause misdiagnosis

### 3.1 `201` and `200` are both success, and they mean opposite things

- `201` — a real transition. A new version exists, and an event was enqueued.
- `200` — the submitted value already equalled the effective one. **Nothing was
  written and no event was emitted.**

An operator reporting "I saved it and nothing happened" has almost always got a
`200`. That is the service working correctly. Check
`configuration_config_writes_total{outcome="no_change"}`.

### 3.2 A `404` names a scope, not a setting

The single-key `GET`s match `(key, environment, tenant_id)` **exactly** and do
not fall back from a tenant miss to the global default. So:

```
GET /v1/config/cutoff.hour?environment=prod&tenant_id=<T>  → 404
```

does **not** mean `cutoff.hour` is unset. It means tenant `T` has no value of
its own; there may well be an environment-wide default in force for it. Check
the list route, which returns the caller's tenant *plus* the globals.

### 3.3 Reads work while writes are dead

Reads touch only Postgres. Writes additionally call `authorization-svc` and fail
closed. So the characteristic outage of this service is: **every page loads,
every value displays, and every change is refused** — which reads as a broken
console. `/readyz` and `configuration_authz_decisions_total{outcome="unavailable"}`
are what tell you otherwise.

---

## 4. Alert response

### 4.1 ConfigurationOutboxStalled / BacklogGrowing / PublishFailing

**What is actually wrong:** configuration changes are being recorded but not
delivered. Consumers are still acting on values this service has already
replaced.

**Why it is worse than it sounds:** the value a consumer is holding is *valid* —
just superseded. No request fails, no latency moves, nothing downstream can
detect it. These alerts are the only evidence that exists.

**Triage:**

```bash
# How far behind, and why
docker exec -i zoiko-postgres psql -U postgres -d configuration_feature_flag -c \
  "SELECT event_type, count(*), min(created_at), max(attempts), max(last_error)
     FROM event_outbox WHERE published_at IS NULL GROUP BY event_type;"

# Is the broker reachable at all?
docker exec -i zoiko-kafka bash -lc \
  "/opt/kafka/bin/kafka-topics.sh --bootstrap-server localhost:9094 --list" | grep configuration
```

- `last_error` naming the broker → Kafka problem. The relay retries on its own
  once the broker returns; nothing needs replaying by hand.
- Backlog large but `attempts` at 0 → the relay is not running. Check the
  service logs for `outbox relay started`.
- `attempts` climbing with no `last_error` → look at the relay's own log lines
  (`outbox drain failed`).

**Do not** delete rows from `event_outbox` to clear an alert. Every row is a
change some consumer has not been told about; dropping them makes the
inconsistency permanent *and* invisible.

### 4.2 ConfigurationWritesUniformlyDenied

**What it means:** more than five denials and not one grant in fifteen minutes.

This is rarely a user problem. On this service the shape of "many denials, zero
grants" is an **action-name mismatch**: the service asking `authorization-svc`
for a name nothing in the estate provisions. Every write 403s while every read
works, and it looks exactly like an under-granted operator. `access-control-svc`
shipped in precisely this state.

**Check the two lists agree:**

```bash
# What this service asks for
grep -n 'Action.*= "' services/configuration-feature-flag-svc/internal/telemetry/domain.go

# What the estate actually grants
docker exec -i zoiko-postgres psql -U postgres -d authorization_svc -tAc \
  "SELECT bundle_code, permitted_actions FROM permission_bundles WHERE active_flag;"
```

All four must be granted: `CONFIGURATION_WRITE`, `CONFIGURATION_GLOBAL_WRITE`,
`FEATURE_FLAG_WRITE`, `FEATURE_FLAG_GLOBAL_WRITE`. They are seeded by
`deployments/scripts/seed-demo-rbac.ps1` in the `CONFIG_FULL` bundle, on the
**platform scope** (`AUTHZ_PLATFORM_SCOPE_ID`), not on a legal entity — a grant
made on the entity is invisible to every check this service makes.

If only the `_GLOBAL_` ones are denied, that is not a fault: it means the
principal may change its own organisation's settings but not the default for
everyone. That is the intended separation.

### 4.3 ConfigurationAuthzUnavailable

**What it means:** no authorization decision can be obtained, so every write is
refused `503 authz_unavailable`. This is the fail-closed posture working.

```bash
curl -s localhost:8086/readyz | python -m json.tool     # names authorization-svc
curl -s localhost:8089/healthz                          # is it actually up?
```

Restarting *this* service will not help — the dependency is somebody else's. If
`authorization-svc` is healthy and this still fires, check
`AUTHZ_SERVICE_URL` and, if the mTLS pilot is on, `AUTHZ_MTLS_URL` and the
client identity from `mtls-management-svc`.

One non-obvious cause: **`AUTHZ_PLATFORM_SCOPE_ID` unset.** Every write
authorizes against it as the `legal_entity_id` and `authorization-svc` rejects an
empty one outright, so an unset variable does not disable the check — it fails
every write with an error that reads as an outage. The service now refuses to
start without it outside `local`, but a service started before that guard
existed can still be running in this state.

### 4.4 ConfigurationGlobalScopeWriteBurst

**What it means:** more than ten environment-wide defaults changed in ten
minutes.

Not necessarily wrong — a planned rollout looks like this — but each one takes
effect for **every tenant that has not set its own value**, so it is worth
confirming it was intended.

```bash
docker exec -i zoiko-postgres psql -U postgres -d configuration_feature_flag -c \
  "SELECT key, environment, created_by_principal_id, effective_from
     FROM config_entries
    WHERE tenant_id IS NULL AND effective_from > now() - interval '30 minutes'
    ORDER BY effective_from DESC;"
```

To undo one, write the previous value back — do not delete the row. The history
is the record.

### 4.5 ConfigurationReadinessFailing

`/readyz` names the failed component. Two possibilities, opposite remedies:

- `database` — this service's problem. Check the pool, check Postgres.
- `authorization-svc` — see 4.3. Restarting this service achieves nothing.

---

## 5. Common tasks

### Read what a service would actually see

```bash
TEN=11111111-1111-1111-1111-111111111111
curl -s "localhost:8086/v1/config/payroll.batch_size?environment=prod&tenant_id=$TEN" \
  -H "X-Tenant-Id:$TEN" -H "X-Principal-Id:$ME" \
  -H "X-Request-Id:$(uuidgen)" -H "X-Source-Channel:system"
```

Drop `tenant_id` to ask about the environment-wide default instead. The two are
different questions and neither answers the other.

### Reconstruct the history of a value

```sql
SELECT value, effective_from, effective_to, created_by_principal_id
  FROM config_entries
 WHERE key = 'payroll.batch_size' AND environment = 'prod'
 ORDER BY effective_from;
```

The row with `effective_to IS NULL` is the one in force.

### Verify the one-effective-row invariant

This should always return nothing. If it does not, the partial unique index is
missing or was created non-uniquely:

```sql
SELECT key, environment, COALESCE(tenant_id, '00000000-0000-0000-0000-000000000000'::uuid), count(*)
  FROM config_entries WHERE effective_to IS NULL
 GROUP BY 1,2,3 HAVING count(*) > 1;
```

---

## 6. Configuration

| Variable | Default | Notes |
| --- | --- | --- |
| `PORT` | `8086` | |
| `ENV` | `local` | Outside `local`, missing settings become fatal rather than defaulted. |
| `DB_*` | — | `DB_SSLMODE` defaults to `require`. |
| `AUTHZ_SERVICE_URL` | `http://authorization-svc` | Refused at startup if it looks like a placeholder outside development. |
| `AUTHZ_PLATFORM_SCOPE_ID` | *(none)* | **Required outside `local`.** See 4.3. |
| `KAFKA_BROKERS` | `localhost:9092` | Empty is allowed in `local` only; fatal in staging/production, because a deployment silently publishing nothing is the failure events exist to prevent. |
| `KAFKA_EVENTS_TOPIC` | `zoiko.configuration.events` | |
| `ZS_ENVELOPE_ENFORCEMENT` | write-strict | ZS-ARCH-SVC-001 §4. |

---

## 7. Re-proving the service

`scripts/audit.sh` re-runs every claim in this runbook against the running
stack rather than against stubs, and exits non-zero if anything fails.

```bash
cd services/configuration-feature-flag-svc
B=http://localhost:8086 bash scripts/audit.sh          # full
SKIP_FE=1 bash scripts/audit.sh                        # backend only
```

It needs the service on `:8086`, Postgres as `zoiko-postgres`,
`authorization-svc` on `:8089`, Kafka as `zoiko-kafka`, a Go toolchain, and the
console's `node_modules` for the frontend section.

---

## 8. Known-sharp edges

**The store test suite drops tables.** Two tests reach the store-unavailable
path by dropping `config_entries` / `feature_flags`. They now restore the schema
in `t.Cleanup`, but never point `TEST_DATABASE_URL` at a database that is
serving anything — this service's own history records a live demo losing
`feature_flags` to exactly that.

**A trailing newline in a pasted URL** breaks exact-match filters and route
matching, producing a misleading empty list or a bare Go `404 page not found`
rather than this service's JSON error shape. Not a service fault, but worth
ruling out before suspecting one.
