# RUNBOOK — delegated-authority-svc

The register of who may act for whom. Port **8136**.

Doc 03 §9.3: "maintains time-bound, scope-bound, approval-bound delegated
authority chains", under one hard constraint — **delegated authority must never
exceed the delegator's own authority**.

---

## 1. What breaks, and what it looks like

This service has an unusual property for an on-call reader: **almost everything
that goes wrong here produces no 5xx and no latency.** A delegation that
outlives its window, an escalation attempt that is correctly refused, an event
that never reaches the consumer that ends a delegate's session — all three look
like a perfectly healthy service on the golden signals. That is why the alert
rules in `deployments/prometheus-rules.yml` are written against decisions and
ages rather than error rates, and why this runbook leads with the quiet
failures.

| Symptom an operator reports | Look at | Section |
|---|---|---|
| "This delegation should have ended yesterday and is still active" | `delegated_authority_expiry_oldest_overdue_seconds` | [4.1](#41-expiry-is-not-happening) |
| "Someone got access they shouldn't have" | `delegated_authority_grants_total{outcome=~"delegator_mismatch\|self_dealing"}` | [4.2](#42-escalation-attempts) |
| "I revoked it but they're still logged in" | `delegated_authority_outbox_oldest_age_seconds` | [4.3](#43-revocation-not-reaching-the-consumer) |
| "Everything returns 503" | `delegated_authority_authz_decisions_total{outcome="unavailable"}` | [4.4](#44-authorization-svc-unreachable) |
| Pod never goes ready | `/readyz` | [4.5](#45-readiness-failing) |
| "Why is this a 403 and not an empty list?" | — | [5](#5-the-refusal-vocabulary) |

---

## 2. First response

```bash
curl -s localhost:8136/healthz                      # process up?
curl -s localhost:8136/readyz | jq                  # Postgres AND authorization-svc
curl -s localhost:8136/metrics | grep delegated_authority_expiry
```

`/healthz` answers 200 whenever the process is up and checks nothing, by
design. **`/readyz` is the one that means something**: it pings Postgres *and*
authorization-svc, because every route here calls the latter and this service
fails closed. With authorization-svc down, the pool can be perfectly healthy
while 100% of requests answer 503 — readiness used to report ready throughout
exactly that outage.

---

## 3. Minting a request by hand

Every route requires the canonical §4 envelope, so a bare `curl` is refused
before it reaches a handler. The headers are not decoration and cannot be
faked past gateway-auth-svc in a real deployment; this is for a local stack.

```bash
TENANT=11111111-1111-1111-1111-111111111111
ENTITY=22222222-2222-2222-2222-222222222222
ME=33333333-3333-3333-3333-333333333333

# Read your own involvement — no entity scope, no authorization call.
curl -s localhost:8136/v1/delegations/ \
  -H "X-Tenant-Id: $TENANT" \
  -H "X-Principal-Id: $ME" | jq

# Grant. Writes additionally need the entity and an idempotency key.
CORR=$(uuidgen)
curl -s -X POST localhost:8136/v1/delegations/ \
  -H "X-Tenant-Id: $TENANT" \
  -H "X-Principal-Id: $ME" \
  -H "X-Legal-Entity-Id: $ENTITY" \
  -H "Idempotency-Key: $CORR" \
  -H "Content-Type: application/json" \
  -d "{\"legal_entity_id\":\"$ENTITY\",
       \"delegator_principal_id\":\"$ME\",
       \"delegate_principal_id\":\"44444444-4444-4444-4444-444444444444\",
       \"action_type\":\"INVOICE_APPROVE\",
       \"effective_from\":\"$(date -u +%Y-%m-%dT%H:%M:%SZ)\",
       \"effective_to\":\"$(date -u -d '+1 hour' +%Y-%m-%dT%H:%M:%SZ)\",
       \"correlation_id\":\"$CORR\"}" | jq
```

If that grant answers **403 `delegator_lacks_authority`**, the service is
working: `$ME` does not hold `INVOICE_APPROVE` on `$ENTITY`, and a delegation
may not exceed the delegator's own authority. Seed the grant in
authorization-svc first — `deployments/scripts/seed-demo-rbac.ps1`.

---

## 4. Incidents

### 4.1 Expiry is not happening

**Alerts:** `DelegatedAuthorityExpirySweepStalled`,
`DelegatedAuthorityExpirySweepFailing`

**Why this matters more than it sounds.** A delegation past its `effective_to`
that has not been expired is authority nobody granted. The register still says
`ACTIVE`, the delegate can still act, and no `authority.expired` has been
published — so identity-context-svc has never been told to end their session.
Nothing 5xxs. Nothing is slow.

This is the defect the background sweeper was built to fix. Before it existed,
expiry piggybacked on reads, so **a tenant whose register nobody opened expired
nothing at all**, indefinitely. If you are reading this because expiry has
stopped, the question is which of the three supports has gone:

```bash
# 1. Is the sweeper running at all? It logs on start.
kubectl logs deploy/delegated-authority-svc | grep "expiry sweeper started"

# 2. Is it erroring?
curl -s localhost:8136/metrics | grep expiry_sweep_failures_total

# 3. How bad is the backlog, and how late are the ones it does catch?
curl -s localhost:8136/metrics | grep -E 'expiry_due_pending|expiry_oldest_overdue|expiry_lateness'
```

**If `expiry_sweep_failures_total` is climbing** — read the logs for the
underlying error. The commonest cause is the RLS exemption being missing:
the sweep crosses tenants by installing `app.expiry_sweeper`, which migration
`000004` admits in the policy. If `000004` has not been applied, or a down
migration removed it, the sweep sees no rows and silently expires nothing.
Confirm with:

```sql
SELECT polname, pg_get_expr(polqual, polrelid) AS using_expr
  FROM pg_policy
 WHERE polrelid = 'delegation_grants'::regclass;
-- The USING expression must mention app.expiry_sweeper.
```

**If failures are zero but the backlog is growing** — the sweeper is running and
finding nothing, which points at the database rather than the service. Check
whether the rows are actually due:

```sql
SELECT count(*), min(effective_to)
  FROM delegation_grants
 WHERE status = 'ACTIVE' AND effective_to < now();
```

**If `expiry_lateness_seconds` is high but the backlog is empty** — the sweep is
working and has caught up after an outage. The lateness histogram is measured
from each grant's `effective_to`, so a burst of very late expiries after a
restart is the mechanism recovering, not failing. It should settle within one
sweep interval (`EXPIRY_SWEEP_INTERVAL`, 30s by default).

**Manual sweep.** There is no admin endpoint for this, deliberately — a route
that expires delegations on demand is a route that can be pointed at the wrong
tenant. Restarting the service runs a pass immediately on startup.

### 4.2 Escalation attempts

**Alert:** `DelegatedAuthorityEscalationAttempts`

Two of this service's refusals are not mistakes, and they fire this alert at
**any** occurrence rather than on a rate — a rate threshold would mean
tolerating some amount of attempted privilege escalation.

- **`delegator_mismatch`** — a caller named somebody else in
  `delegator_principal_id` without holding `DELEGATION_ADMINISTER` on the
  entity. This is the one that made `DELEGATION_CREATE` a privilege-escalation
  primitive before it was closed: name a colleague as delegator, name yourself
  as delegate, and you have minted yourself their authority. Both of the checks
  that existed still passed, because the invariant they enforce was never the
  one being violated.
- **`self_dealing`** — an administrator, legitimately holding
  `DELEGATION_ADMINISTER`, routed another principal's authority to themselves.
  The same escalation by a longer route.

Both are refused correctly, so **nothing is broken and nothing needs fixing in
the service**. What this alert asks for is an answer to *who, and why*:

```bash
kubectl logs deploy/delegated-authority-svc | grep -E 'delegator_mismatch|self_dealing'
```

A single occurrence from an operator learning the console is ordinary. A
repeated one from the same principal, or one from a service account, is not.

### 4.3 Revocation not reaching the consumer

**Alert:** `DelegatedAuthorityOutboxStalled`

`authority.revoked` is the signal identity-context-svc acts on to end the
delegate's session. **The gap between an operator being told a revocation
succeeded and the delegate's session actually ending is the outbox backlog
age.** That is why this alert is at two minutes and not at an error rate.

The event is written in the same transaction as the status change, so it is
never lost — a grant that exists always has its event. What can go wrong is
delivery:

```bash
curl -s localhost:8136/metrics | grep -E 'outbox_pending|outbox_oldest_age|outbox_failures'
```

```sql
SELECT event_type, count(*), min(created_at)
  FROM delegation_outbox
 WHERE published_at IS NULL
 GROUP BY event_type;
```

Almost always Kafka: broker unreachable, topic missing, or the partition leader
unavailable. The relay retries indefinitely and the backlog drains on its own
once the broker returns — **do not delete outbox rows to clear the alert.** An
unpublished `authority.revoked` is a delegate still holding a withdrawn
authority; deleting it makes the symptom disappear and the problem permanent.

### 4.4 authorization-svc unreachable

**Alert:** `DelegatedAuthorityAuthzUnavailable`

Every grant, every revoke and every entity-scoped read calls authorization-svc
before doing anything, and this service **fails closed**. So an
authorization-svc outage presents here as every gated route answering 503.

The two outcomes are counted separately for a reason: `denied` is a permissions
problem and needs a grant; `unavailable` is a dependency outage and needs
authorization-svc. Conflating them sends you to the wrong service.

```bash
curl -s localhost:8136/readyz | jq        # names the failing dependency
curl -s authorization-svc:8089/healthz
```

Nothing was written during the outage. There is no reconciliation to do
afterwards — that is the point of failing closed.

One non-obvious cause, seen on sibling services: authorization-svc enforces the
same §4 envelope contract this service does, and answers **400
`envelope_incomplete`** without it. A non-200 is treated as unavailable here, so
a *missing header* on the outbound call presents as an outage rather than as a
contract error. If authorization-svc is demonstrably healthy and this service
still reports it unavailable, read authorization-svc's own logs for
`envelope_incomplete` before looking at the network.

### 4.5 Readiness failing

**Alert:** `DelegatedAuthorityReadinessFailing`

`/readyz` pings Postgres and authorization-svc. Both are readiness
dependencies, not merely runtime ones, for the reason in §2.

The probe is written against a `Pinger` interface rather than `*pgxpool.Pool`
concretely. That is not only for testing: `*pgxpool.Pool` **panics** on `Ping`
when it is nil, so a probe written against the concrete type turned "the pool
was never built" into a crash inside the health endpoint — the one handler that
has to survive everything else being broken.

---

## 5. The refusal vocabulary

Every refusal carries a stable `error_code`. Branch on the code, never the
message. The full list is in `openapi.yaml`; these are the ones that get
mistaken for bugs.

| Code | Status | Why it is correct |
|---|---|---|
| `delegator_lacks_authority` | 403 | The §9.3 invariant. The delegator does not hold what is being delegated, so it cannot be passed on. Grant it to the delegator first. |
| `delegator_mismatch` | 403 | The caller is not the delegator and lacks `DELEGATION_ADMINISTER`. See §4.2. |
| `self_dealing` | 403 | An administrator named themselves as delegate. See §4.2. |
| `forbidden` on an unscoped read | 403 | Asking after **another** principal's delegations with no `legal_entity_id`. Deliberately not an empty list: "you may not ask" and "there are none" are different answers and only one of them is reassuring. |
| `unknown_status` | 400 | A status filter outside `ACTIVE\|REVOKED\|EXPIRED`. Deliberately not an empty result — on a governance register, "nobody holds any delegated authority" and "you misspelled the filter" must not look identical. |
| `invalid_transition` | 409 | Already `REVOKED` or `EXPIRED`. Not a fault and not a refusal: a correct answer that the caller is late. A rising rate means two operators are chasing the same delegation. |
| `not_found` on a malformed id | 404 | A non-UUID reaches Postgres as `22P02`. It used to surface as **503**, sending whoever was on call to look at a healthy database. A string that cannot name a row is what 404 means. |
| `tenant_missing` vs `identity_missing` | 401 | Kept distinct on purpose. A forgotten tenant header and a request that never passed ForwardAuth are fixed in different places. |

---

## 6. Timestamps, and the one that is easy to misread

Three timestamps can look interchangeable and are not:

- **`effective_to`** — when the authority ends. A property of the grant, known
  when it is written.
- **`expired_at`** — set to `effective_to` when the grant lapses. **When the
  authority ended**, not when the sweep noticed.
- **`updated_at`** — when the row last changed. For an expiry, this *is* the
  observation time.

`expired_at` used to be set to `now()` at sweep time. A grant whose window
closed on a Friday and was next swept on Monday therefore asserted, in
evidence, that its authority ran all weekend — and `authority.expired` carried
the same wrong timestamp to every consumer. Doc 04 §6.3 requires these records
to stand as evidence, and evidence that misdates the end of an authority is
worse than absent, because nothing marks it as approximate.

Migration `000004` corrected the stored rows. **A consumer computing how long an
authority lasted must read `effective_to` or `expired_at`, never the event's
`emitted_at`** — the envelope timestamp is when the transition was recorded,
which may trail the end of the window by up to one sweep interval.

---

## 7. Configuration

| Variable | Default | Notes |
|---|---|---|
| `PORT` | `8136` | |
| `AUTHZ_SERVICE_URL` | `http://authorization-svc:8089` | Readiness dependency. |
| `EXPIRY_SWEEP_INTERVAL` | `30s` | Bounds how long a delegation can outlive its window. **There is deliberately no off switch** — see §4.1. An unparseable value falls back to the default rather than failing startup. |
| `ZS_ENVELOPE_ENFORCEMENT` | `write-strict` | Reads need tenant + actor; writes additionally need entity + idempotency key. |
| `KAFKA_EVENTS_TOPIC` | `zoiko.delegated-authority.events` | Consumers bind via `KAFKA_DELEGATION_TOPIC`. |
| `AUTHZ_MTLS_ENABLED` | `false` | Matches the siblings. |

---

## 8. Re-proving the service

```bash
cd services/delegated-authority-svc && bash scripts/audit.sh
```

Needs the service on `:8136`, `zoiko-postgres`, authorization-svc on `:8089`,
Prometheus on `:9090` for the alert-rule sections, and the console's
`node_modules` for the frontend section (`SKIP_FE=1` skips it). Exits non-zero
on any failure, so CI can gate on it.

The store suite needs `TEST_DATABASE_URL` pointed at a **scratch** database —
it drops its tables on every run — and at a **`NOSUPERUSER NOBYPASSRLS`** role.
The suite now refuses to run as a superuser, because a superuser bypasses
row-level security unconditionally and the isolation tests would pass with
every policy dropped.
