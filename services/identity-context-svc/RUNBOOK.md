# identity-context-svc — Operational Runbook

**Service:** GOV-01 — Tenant Context & Resolution
**Tier:** 0. Nothing on the platform authenticates while this is down.
**Dashboard:** Grafana → `identity-context-svc (GOV-01)` (uid `identity-context-gov01`)

---

## 1. What this service is, in one paragraph

It turns a credential into a signed `IdentityContextEnvelope` — the JWT every
other service on the platform trusts. Six dimensions must all resolve
(principal, tenant, legal entity, role profile, delegated authority, trust
posture) or it refuses. Partial envelopes are prohibited. Every resolution,
successful or refused, produces durable evidence.

Two things follow from that, and they drive most of this document:

- **A refusal here is the correct outcome, not an incident.** The failure mode
  to fear is the opposite — issuing an envelope that should not exist.
- **A silent success is worse than a loud failure.** Most of the alerts below
  fire on things that produce no errors and no latency.

---

## 2. First response

| Symptom | Start at |
|---|---|
| Nobody can log in anywhere | §3.1 |
| `/health` degraded | §3.2 |
| Outbox backlog alert | §4.1 |
| Outbox dead letters | §4.2 |
| Ingress/tenant mismatch alert | §5.1 |
| Residency denials after a deploy | §5.2 |
| Support context awaiting review | §6.1 |
| SoD unavailable | §6.2 |
| Disposition held back | §7.1 |

```bash
# The three commands worth running before anything else.
kubectl -n zoiko get pods -l app=identity-context-svc
kubectl -n zoiko exec deploy/identity-context-svc -- wget -qO- localhost:8080/health | jq
kubectl -n zoiko logs deploy/identity-context-svc --since=15m | grep -E 'ERROR|FATAL'
```

---

## 3. Availability

### 3.1 Total authentication outage

Check, in this order, because each rules out the next:

1. **Is the pod up?** Tier 0 fails fast on startup if Redis or Postgres are
   unreachable, so a `CrashLoopBackOff` usually means a dependency, not a bug.
   The fatal log line names which.

2. **Is the tenant registry answering?** It is a fail-closed dependency of
   Dimension 2, so a registry outage 503s every resolution. Since the readiness
   probe now includes it, this shows as `degraded` with
   `checks.tenant_registry: unreachable` rather than as a healthy pod serving
   503s.

   ```bash
   kubectl -n zoiko exec deploy/identity-context-svc -- \
     wget -qO- "$TENANT_REGISTRY_URL/health"
   ```

3. **Is every login 401 with `principal inactive or not found`?**
   This is the signature of an unscoped RLS connection. `principals` has
   `FORCE ROW LEVEL SECURITY` and the policy uses
   `current_setting('app.tenant_id', true)` with `missing_ok = true`, which
   returns NULL rather than raising — so an unscoped connection matches **zero
   rows** instead of erroring. Every login then fails while the principal is
   present and ACTIVE.

   ```sql
   -- Run as the SERVICE user, not a superuser: a superuser bypasses RLS and
   -- the query will succeed, telling you nothing.
   SELECT set_config('app.tenant_id', '<tenant>', false);
   SELECT principal_id, status FROM principals WHERE identity_provider_subject = '<sub>';
   ```

4. **Is `JWT_SIGNING_SECRET` consistent across replicas?** The IdP token is
   HS256 with a shared secret; a replica with a different one mints tokens the
   others reject, so failures are intermittent and proportional to replica
   count.

### 3.2 `/health` degraded

`checks` names the failing dependency. The two that are NOT fatal and will say
so:

- `outbox: unreadable` — a query problem, not availability. Postgres is checked
  separately.
- `outbox_dead_letter: N` — reported, never degrading. See §4.2.

---

## 4. Outbox

The outbox is the single most important thing to watch, because **a stopped
relay produces no errors and no latency**. Every request succeeds. The only
symptom is that governance events stop reaching the estate.

### 4.1 Backlog growing (`identity_context_outbox_pending`)

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
| `Unknown Topic Or Partition` | Topic missing and auto-create off broker-side | Create `zoiko.identity.events`. |
| `context deadline exceeded` | Broker slow, not down | Check broker health before touching this service. |
| *empty, and `pending` is rising* | **Relay is not running** | See below. |

A rising backlog with **no** `last_error` means nothing is even attempting
delivery. Check the relay started:

```bash
kubectl -n zoiko logs deploy/identity-context-svc | grep 'outbox relay started'
```

A restart is safe and is the correct first action — the relay is stateless and
`FOR UPDATE SKIP LOCKED` makes concurrent relays safe, so nothing is lost or
duplicated by restarting one.

**Do not delete rows to clear a backlog.** Every row is evidence of a
governance decision that already happened.

### 4.2 Dead letters (`identity_context_outbox_dead_letter`)

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

### 5.1 Ingress / tenant mismatch

A request arrived on tenant A's hostname bearing a token claiming tenant B.
**There is no legitimate request of this shape.** Two possible causes, and they
need very different responses:

1. **Routing misconfiguration** — an ingress rule or a load balancer serving one
   customer's hostname to another's backend. Check recent ingress changes first;
   this is by far the more common cause and it is a customer-visible outage for
   whoever owns that hostname.
2. **Somebody probing the boundary.** The log line is at ERROR and names both
   tenants:

   ```bash
   kubectl -n zoiko logs deploy/identity-context-svc | grep 'INGRESS/TENANT MISMATCH'
   ```

   It is also streamed to SIEM at CRITICAL as
   `identity.ingress_tenant_mismatch`.

Nothing was leaked either way — the check runs before any tenant-scoped query,
so the refusal happens before a cross-tenant read could occur.

**Do not "fix" this by setting `INGRESS_POLICY=observe`.** Observe mode still
refuses a mismatch; it only relaxes the unknown-hostname case. There is no
setting that permits a mismatch, deliberately.

### 5.2 Residency denials

An entity whose `data_residency_policy_id` is not in this region's
`ALLOWED_RESIDENCY_POLICIES`. After a deploy this almost always means the
allow-list was not updated for the region, not that anything is wrong:

```bash
kubectl -n zoiko get deploy identity-context-svc -o jsonpath='{.spec.template.spec.containers[0].env}' \
  | jq '.[] | select(.name=="ALLOWED_RESIDENCY_POLICIES" or .name=="DEPLOYMENT_REGION")'
```

Enforcement is **off** when the list is empty. That is the safe default for
rollout, and it is also a silent one — if you expect residency to be enforced,
confirm the list is populated rather than assuming the absence of denials means
it is working.

---

## 6. Break-glass (support contexts)

### 6.1 Grants awaiting review

`identity_context_support_contexts_unreviewed` counts elevations that **ended**
and were never reconciled. This is the half of break-glass that is normally
missing: the grant expires, nobody looks, and the control is decorative.

```sql
SELECT support_context_id, support_principal_id, approver_principal_id,
       reason_code, ticket_ref, granted_at, expires_at, revoked_at
  FROM support_contexts
 WHERE reviewed_at IS NULL
   AND (expires_at <= NOW() OR revoked_at IS NOT NULL)
 ORDER BY expires_at;
```

Reviewing is a human act — read the justification against the ticket, confirm
the access taken matches it, then record the review. There is deliberately no
auto-approve: an automatic review is not a review.

To see what was actually done under a grant:

```sql
SELECT session_context_id, principal_id, legal_entity_id, issued_at, ingress_source
  FROM session_contexts
 WHERE support_context_id = '<sup-...>'
 ORDER BY issued_at;
```

### 6.2 SoD unavailable

`identity_context_sod_checks_total{outcome="unavailable"}` rising means GOV-04
cannot be reached, so **no privileged command can be issued at all**. This is
correct — a conflict check that did not run is not a check that passed — but it
means break-glass is unavailable during an incident, which is exactly when it
is wanted.

```bash
kubectl -n zoiko exec deploy/identity-context-svc -- wget -qO- "$SOD_SERVICE_URL/health"
```

There is no bypass, and adding one would defeat the control. If GOV-04 is down
during a genuine emergency, that is an incident for the GOV-04 team.

### 6.3 Emergency revocation of an active grant

```bash
curl -X DELETE "$SVC/v1/context/support/<sup-id>" \
  -H "X-Tenant-Id: <customer-tenant>" \
  -H "X-Principal-Id: <your-principal>" \
  -H "Content-Type: application/json" \
  -d '{"reason":"revoked during incident review"}'
```

Guarded by `IDENTITY_SUPPORT_CONTEXT_REVOKE`, which is deliberately a weaker
grant than attach: whoever notices a problem must be able to stop it.

---

## 7. Retention

### 7.1 Disposition held back

`identity_context_disposition_held_total` rising means rows were due and blocked
by an active legal hold. **This is the control working**, and it is reported
separately precisely so a sweep that disposed nothing because everything was
held does not look like a quiet night.

```sql
SELECT hold_id, matter_ref, principal_id, issued_at
  FROM legal_hold_projection
 WHERE released_at IS NULL;
```

This service **cannot** issue or release a hold — the rows arrive from GOV-10's
events and there is no write path here. If a hold looks wrong, that is a GOV-10
question.

### 7.2 Running a sweep manually

The sweep runs on `RETENTION_SWEEP_INTERVAL_MINUTES`. There is no HTTP trigger,
deliberately: a disposition endpoint is a deletion endpoint. To force one,
restart the pod after setting a short interval, then set it back.

### 7.3 Disposition is redaction, not deletion

The row survives — it is the evidence a session existed. What the retention
period governs is the personal data, so `correlation_id`, `ingress_source`,
`risk_signal_source` and the scores are cleared and `disposed_at` is stamped.
`ExplainContextResolution` reports `RESOLVED_EVIDENCE_DISPOSED` for these rather
than presenting redacted fields as original values.

---

## 8. Load / NFR verification

The in-process budget is guarded in CI:

```bash
go test ./internal/context -run 'TestResolveLatency' -v
go test ./internal/context -bench=. -benchmem -run=^$
```

End-to-end against a running stack — this is the number the 50ms P99 target
refers to, since the CI test stubs out the registries and Postgres:

```bash
# 200 concurrent, 60s, against a seeded local stack.
hey -z 60s -c 200 -m POST \
  -H 'Content-Type: application/json' \
  -d '{"bearer_token":"'"$TOKEN"'","legal_entity_id":"'"$ENTITY"'","correlation_id":"load"}' \
  http://localhost:8080/v1/context/resolve
```

Acceptance: P99 < 50ms **per request**, zero 5xx, and — check this, it is the
one people forget — `identity_context_outbox_pending` returns to its baseline
afterwards. A load test that leaves a permanent backlog means the relay cannot
keep up with peak write rate, which no latency percentile will tell you.

### What 200 concurrent actually measures

**It measures the queue, not the service, and the two targets above cannot both
be read at that concurrency.** One replica at the compose limits
(`cpus: "0.5"`, `mem_limit: 256m`) saturates at roughly 100 resolutions/second.
By Little's law 200 concurrent at 100 req/s IS a two-second latency — 200
concurrent at P99 50ms would require 4,000 req/s from one container, or about
twenty CPUs. A run at that concurrency tells you the saturation point; it
cannot tell you whether a resolution is fast.

Measured 2026-09-18, in-network against this stack (load generator on the
compose network, so no Windows loopback in the path):

| concurrency | throughput | p50 | p99 | 5xx |
|---|---|---|---|---|
| 1 | 69 req/s | 12.9ms | **39.7ms** | 0 |
| 4 | 93 req/s | 18.2ms | 97.1ms | 0 |
| 16 | 101 req/s | 173.6ms | 291.9ms | 0 |
| 200 | 102 req/s | 1.90s | 2.40s | 0 |

**The 50ms P99 target is met at c=1: 39.7ms, end to end, with the tenant
registry, access-control-svc, Postgres, Redis and Kafka all real.** The CI
resolver test measures 0.6ms with those stubbed, so the dependencies this
profile exists to include cost about 39ms of the 50ms budget — most of the
headroom, and worth knowing before anyone adds another upstream call.

Raising the container to 4 CPUs moves c=16 from 101 to 184 req/s and c=1's p99
from 39.7ms to 29.0ms. Throughput is therefore only partly CPU-bound; single
resolution latency is dominated by the upstream round trips, not by this
service's own work.

**So: run c=1 to c=4 to check the latency target, and the 200-concurrent run to
find the saturation point and to confirm the outbox drains.** Do not read a
P99 off the 200-concurrent run and call it a regression.

### The outbox check is the one that has actually failed

After a 60s 200-concurrent run the backlog peaks around 9,000 events and must
return to zero. Measured: 9,052 → 0 in roughly 30 seconds, about 300 events/s.

Before 2026-09-18 it drained at **1.03 events/second** — a minute of load left
four hours of backlog — with zero errors, zero retries and a healthy-looking
service. See the release certificate's second addendum. If you ever see a drain
rate near 1/s again, look at the Kafka writer's `BatchTimeout` before anything
else.

---

## 9. Configuration that changes behaviour

| Variable | Effect if wrong |
|---|---|
| `INGRESS_POLICY` | `strict` refuses unbound hostnames. Turning it on before `tenant_ingress_bindings` is seeded refuses **everything**. |
| `ALLOWED_RESIDENCY_POLICIES` | Empty means enforcement **off**, silently. |
| `SOD_SERVICE_URL` | Required in staging/production; the service refuses to start without it. |
| `SUPPORT_CONTEXT_MAX_TTL_SECONDS` | The ceiling on break-glass. Raising it is a governance decision. |
| `SESSION_EVIDENCE_RETENTION_DAYS` | `0` means never dispose. Safe, and accumulates. |
| `DEPLOY_ENVIRONMENT` | Written to a CHECK-constrained column; an invalid value fails at startup, not at first request. **It is also what arms the authorization guard** — see below. |
| `AUTHZ_SERVICE_URL` | In `production`/`staging` a placeholder (unset, portless, loopback or a reserved domain) refuses to start. Anywhere else it selects a **permit-all stub**. |
| `AUTHZ_ENV` | Optional, and normally leave it unset. It defaults to `DEPLOY_ENVIRONMENT` and may only agree with it; naming a lower tier in production is refused at startup. |

**The one to get right is `DEPLOY_ENVIRONMENT=production`.** Every
authorization check this service makes is delegated to authorization-svc, and
the guard that refuses to run without a real one reads this variable. A
production pod left at the default tier builds the permit-all client and says
so in a warning log — which is the kind of evidence that is only ever read
afterwards. Confirm it at startup instead:

```bash
kubectl logs deploy/identity-context-svc | grep -i "authorization client"
# want: "using HTTP authorization client"  url=http://authorization-svc:8089
# NOT:  "using PERMIT-ALL authorization stub"
```

---

## 10. What NOT to do

- **Do not delete outbox rows** to clear a backlog. They are governance
  evidence for decisions that already happened.
- **Do not set `INGRESS_POLICY=observe` to stop mismatch alerts.** It does not
  permit mismatches and never did.
- **Do not grant yourself a support context.** The schema refuses
  self-approval, and working around it defeats the only control that makes
  break-glass auditable.
- **Do not run migrations on startup.** They go through golang-migrate in
  CI/CD. Migration 000007 replaces two RLS policies; an auto-run racing a
  second replica is how a tenant-isolation policy ends up missing.
- **Do not "fix" a residency denial by adding the policy to the allow-list**
  without checking which region the pod is in. The denial may be correct.
