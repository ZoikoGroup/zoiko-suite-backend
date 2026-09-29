# Runbook — access-control-svc

The register of what roles exist and what each one permits, and the governed
front door that provisions both into authorization-svc.

Port **8137**. Database **`access_control`**. Topic
**`zoiko.access-control.events`**.

---

## 1. What this service is, and what it is not

Doc 03 §9.4: "maintains role catalogues, permission bundles, and policy-linked
access groupings."

**authorization-svc owns live RBAC.** Every other service's authz check
resolves against it, and this service does not shadow that data. It is the
authoring layer: creating a role or bundle here makes a synchronous call into
authorization-svc's admin API, so a definition recorded here has actually been
provisioned for enforcement. Per-principal role **assignments** are out of
scope and are not reachable on this API at all — they stay where they are.

The consequence that shapes every incident below: **this service is useless
without authorization-svc, and it is honest about that.** Every write calls it
twice — once to authorize the caller, once to provision — and fails closed on
both. A 503 from here means nothing was written.

### The one thing to understand before touching anything

**Every write provisions before it records.** A role is never listed here as
created unless authorization-svc accepted it. A retirement is never recorded
unless `roles.active_flag` was actually cleared there.

So when a write fails, the register is *correct*: it did not record a state the
platform is not enforcing. The failure is loud and the data is safe. The
opposite arrangement — record locally, reconcile later — was rejected, because
a governance register that disagrees with the enforcement it describes is worse
than no register.

---

## 2. Dependencies

| Dependency | Used for | Failure mode |
|---|---|---|
| Postgres `access_control` | The catalogue and the outbox | Everything 503s. Readiness fails and names `database`. |
| authorization-svc `:8089` | `/v1/authorize` on every write; `/v1/admin/*` to provision | Every WRITE 503s and records nothing. Reads keep working. Readiness fails and names `authorization-svc`. |
| Kafka `zoiko.access-control.events` | Event delivery from the outbox | Writes are unaffected — the event is already durable in `event_outbox`. The backlog grows and the relay retries. |

Reads deliberately survive an authorization-svc outage: identity-context-svc
reads the bundle collection on its session-resolution hot path and must not be
taken down by it.

---

## 3. Health and first look

```bash
curl -s localhost:8137/healthz                 # liveness: checks nothing, by design
curl -s localhost:8137/readyz | jq             # names the failing component
curl -s localhost:8137/metrics | grep access_control_
```

`/readyz` returns both components:

```json
{"status":"READY","components":{"database":"ok","authorization-svc":"ok"}}
```

A `NOT_READY` naming `authorization-svc` is **somebody else's outage**.
Restarting this service will not help and will only lose the in-memory
authorization decision cache.

### The decision counters

`http_requests_total` cannot diagnose anything on this service — every
interesting failure is an ordinary-looking 403 or 503. Use these instead:

| Series | Reads |
|---|---|
| `access_control_authz_decisions_total{outcome}` | granted / denied / unavailable on `/v1/authorize` |
| `access_control_authz_admin_calls_total{operation,outcome}` | provisioning into the admin API |
| `access_control_role_writes_total{outcome}` | created / replayed / conflict / forbidden / … |
| `access_control_bundle_writes_total{outcome}` | same, for bundles |
| `access_control_outbox_pending` | unpublished events |
| `access_control_outbox_oldest_age_seconds` | age of the oldest one — **this is the stall signal** |

Every label combination is pre-created at zero, so an alert written to catch
the *first* occurrence of something is not evaluating against absent data.

---

## 4. Incidents

### 4.1 Every write is refused 403, but reads work

**Alert:** `AccessControlWritesUniformlyDenied`

This is the shape an **action-name mismatch** takes, and it has happened. The
handler asked authorization-svc for `ACCESS_ROLE_MANAGE`; nothing in this
estate provisions that name. The seed
(`deployments/scripts/seed-demo-rbac.ps1`) attaches `ACCESS_CONTROL_FULL` with
`permitted_actions ["ROLE_MANAGE"]`, the console tells the operator a 403 means
"you hold no ROLE_MANAGE grant", and the live bundle grants `ROLE_MANAGE`. The
service was the only party asking for the other name, so every write was
refused while every read worked — which presents as an under-granted operator,
not as a broken service.

**Diagnose from authorization-svc's decision log, which records both sides:**

```sql
-- in the authorization_svc database
SELECT decided_at, principal_id, legal_entity_id, action_type,
       decision_outcome, decision_basis
FROM access_decision_log
WHERE action_type LIKE '%ROLE_MANAGE%'
ORDER BY decided_at DESC LIMIT 20;
```

`GRANTED  rbac:role=CONSOLE_DEMO_OPERATOR` for one name and
`DENIED  no_grant` for another, for the same principal on the same entity, is
the mismatch in one screen.

**Then check the two sides agree:**

```bash
# what this service asks for
grep -n 'ActionRoleManage' services/access-control-svc/internal/telemetry/domain.go

# what is actually granted
docker exec -i zoiko-postgres psql -U postgres -d authorization_svc -tAc \
  "SELECT bundle_code, permitted_actions, active_flag FROM permission_bundles
   WHERE bundle_code = 'ACCESS_CONTROL_FULL'"
```

**If the grant is simply missing** (a fresh stack), re-run the seed:
`deployments/scripts/seed-demo-rbac.ps1`. **If the names differ**, the service
is wrong — the estate's name is `ROLE_MANAGE` — and the constant is in
`internal/telemetry/domain.go`, referenced by the handler so the pre-created
metric series and the check cannot drift apart.

### 4.2 Provisioning into authorization-svc is failing

**Alert:** `AccessControlProvisioningFailing`

Writes are failing closed. Nothing is being recorded that is not enforced —
correct, and also total unavailability of role administration.

Two causes, which need opposite responses. **The service logs the response body
specifically so they can be told apart:**

```bash
docker logs access-control-svc 2>&1 | grep -i "authorization-svc admin API"
```

* **`returned 400` / `envelope_incomplete`** — this service sent a malformed
  admin request. It is the caller's bug, not an outage. Every admin call must
  carry the full §4 envelope (`X-Principal-Id`, `X-Tenant-Id`,
  `X-Legal-Entity-Id`, `X-Correlation-ID`, `X-Request-Id`,
  `X-Source-Channel`, `Idempotency-Key`); `internal/clients/authzadmin.go`
  sends all seven and takes them as a `clients.Scope` struct precisely so an
  omission is a compile error. Two of the three methods once passed empty
  strings for principal and tenant and could only ever answer 401.
* **`unreachable` / connection refused** — authorization-svc is down. Check its
  own health; nothing here will help.

There is no manual reconciliation step, because there is nothing to reconcile:
the failed writes wrote nothing.

### 4.3 The outbox has stalled

**Alerts:** `AccessControlOutboxStalled`, `AccessControlOutboxPublishFailing`

Writes still succeed and the register still reads correctly. What stops is the
rest of the estate being *told*: authorization-svc's grant cache is not
invalidated and may serve a withdrawn grant for up to its TTL.

```sql
-- in access_control
SELECT count(*) FILTER (WHERE published_at IS NULL) AS pending,
       min(created_at) FILTER (WHERE published_at IS NULL) AS oldest,
       max(attempts) AS max_attempts
FROM event_outbox;

-- why it is stuck, recorded on the rows themselves
SELECT outbox_id, event_type, attempts, last_error
FROM event_outbox
WHERE published_at IS NULL
ORDER BY created_at
LIMIT 10;
```

`last_error` is the answer nine times in ten. Usually the broker is unreachable
or the topic cannot be auto-created.

**Nothing is lost while this lasts.** Rows are marked published only after the
broker accepts them, inside the same transaction, so a crash mid-publish
re-delivers. Delivery is at-least-once and consumers deduplicate on `event_id`.

If the relay itself has died (pending growing, `attempts` NOT growing, no
`outbox drain failed` log lines), restart the service — `Run` is started in
`main` and a final drain runs on shutdown.

**A note on the RLS escape hatch.** The relay reads across tenants by
installing `app.outbox_relay`, admitted by an explicit disjunct in migration
000004's policy. If you find the relay publishing nothing *and reporting no
error at all*, check that setting is being installed — under
`FORCE ROW LEVEL SECURITY` an unscoped reader sees zero rows silently, which
looks exactly like an empty backlog.

### 4.4 A flood of 409s

**Alert:** `AccessControlDuplicateCodeStorm`

A conflict is the caller's to resolve, not an outage — but a sustained stream
usually means a client is minting a **new `correlation_id` on every retry**.
Each attempt then reads as a new intent, collides with its own first success,
and the caller cannot make progress.

The fix is on the client: generate the key once per intent and reuse it. A
replay of the same key answers **200** with the original record and writes
nothing — that is what the key is for, and it is how a retry is supposed to
resolve.

Find the offender:

```sql
SELECT role_code, count(DISTINCT correlation_id)
FROM role_definitions WHERE tenant_id = :tenant
GROUP BY role_code HAVING count(DISTINCT correlation_id) > 1;
```

### 4.5 Readiness failing

**Alert:** `AccessControlReadinessFailing`

`curl -s localhost:8137/readyz | jq .components` names the failure.

* `database` — this service's problem. Check the pool, the credentials and
  whether `zoiko_app` has `USAGE` on the `event_outbox` sequence (a
  `BIGSERIAL` needs it; without it every write fails, because the enqueue
  shares the write's transaction).
* `authorization-svc` — not this service's problem. Restarting here loses the
  decision cache and fixes nothing.

### 4.6 The register and the enforcement plane disagree

The symptom: a bundle shows ACTIVE here, and authorization-svc denies the
actions it names — or `authz_bundle_not_found` appears in the logs.

Compare the two directly:

```sql
-- here
SELECT role_definition_id, bundle_code, permitted_actions, active_flag
FROM permission_bundle_defs WHERE tenant_id = :tenant;
```
```sql
-- in authorization_svc
SELECT role_id, bundle_code, permitted_actions, active_flag
FROM permission_bundles WHERE role_id = :role_definition_id;
```

The ids line up: this service generates `role_definition_id` and provisions the
role under exactly that id, so `role_definition_id` here **is** `role_id`
there. Bundles join on `(role_id, bundle_code)`.

**The historical cause of this divergence was two local bundles sharing a code
on one role.** authorization-svc's attach endpoint is an upsert-REPLACE on
`(role_id, bundle_code)`, so the second create silently replaced the first
one's actions there while both kept showing ACTIVE here — and detaching either
retired the single remote bundle they both described. Migration 000004 added
the unique index that makes the local key match the remote one. To find any
legacy duplicates:

```sql
SELECT tenant_id, role_definition_id, bundle_code, count(*)
FROM permission_bundle_defs
GROUP BY 1,2,3 HAVING count(*) > 1;
```

If the migration refuses to apply, this query is why, and the duplicates need a
human to decide which one survives.

---

## 5. Running it

### The limited stack

**Do not** `docker compose up` the whole estate. Start this service and its
three infra peers only:

```bash
cd deployments
docker start zoiko-postgres zoiko-kafka authorization-svc
docker compose -f docker-compose.access-control.yml up -d
docker logs -f access-control-svc
```

`docker-compose.access-control.yml` exists because the full compose file has
pre-existing dependency cycles that stop it starting even one service, and
because starting ~90 containers to exercise one is not a debugging strategy.

The healthcheck uses exec-form `CMD`, not `CMD-SHELL`: the image is distroless
and has no `/bin/sh`, so a shell-form check marks the container unhealthy no
matter what the service answers.

### Migrations

```bash
for f in services/access-control-svc/deployments/migrations/*.up.sql; do
  docker exec -i zoiko-postgres psql -U postgres -d access_control -v ON_ERROR_STOP=1 < "$f"
done
# BIGSERIAL needs the sequence, not just the table
docker exec -i zoiko-postgres psql -U postgres -d access_control -c \
  "GRANT SELECT, INSERT, UPDATE, DELETE ON ALL TABLES IN SCHEMA public TO zoiko_app;
   GRANT USAGE, SELECT ON ALL SEQUENCES IN SCHEMA public TO zoiko_app;"
```

| Migration | What it does |
|---|---|
| `000001` | Initial schema, RLS enabled |
| `000002` | **FORCE** RLS, `WITH CHECK` on both policies, status CHECK constraint |
| `000003` | `updated_by_principal_id` on both tables |
| `000004` | `event_outbox` + its relay policy; unique `(tenant, role, bundle_code)`; read indexes |

`000002` is load-bearing: `ENABLE` alone exempts the table **owner**, which is
who runs migrations, so the policies never executed for anybody connecting that
way. And a `USING`-only policy governs what is VISIBLE, not what may be
WRITTEN — a caller could insert a row attributed to another tenant and then be
unable to see it.

### Configuration

| Variable | Default | Note |
|---|---|---|
| `PORT` | `8137` | |
| `DB_*` | see `internal/config` | `TEST_DATABASE_URL` overrides the whole DSN |
| `KAFKA_BROKERS` | `localhost:9092` | |
| `KAFKA_EVENTS_TOPIC` | `zoiko.access-control.events` | |
| `AUTHZ_SERVICE_URL` | `http://authorization-svc:8089` | Used for both authorize and admin calls |
| `AUTHZ_MTLS_ENABLED` | `false` | Opt-in to the material-path mTLS pilot |
| `ZS_ENVELOPE_ENFORCEMENT` | `write-strict` | `strict` gates reads too; `observe` gates nothing |

`ZS_ENVELOPE_ENFORCEMENT` deliberately falls back to `write-strict` on an
unrecognised value rather than to `observe`: a typo in a deployment variable
must not silently disable a control.

---

## 6. Verifying it

```bash
cd services/access-control-svc

go build ./... && go vet ./...
go test -count=1 ./...                                   # unit
TEST_DATABASE_URL=... go test -tags=integration ./internal/store/   # embedded Postgres + RLS

./scripts/audit.sh                                       # the whole thing, live
```

`scripts/audit.sh` is the re-runnable proof: static analysis, the test suites,
live health, the envelope contract, the route surface against `openapi.yaml`,
the event contract against `asyncapi.yaml`, the authorization action name, the
conflict and not-found behaviour, the outbox end to end through Kafka,
telemetry, alert rules, migrations and RLS read back from `pg_policy`, and the
console's typecheck and Playwright specs. It exits non-zero if anything fails.

`SKIP_FE=1` skips the console section when node_modules are not present.
