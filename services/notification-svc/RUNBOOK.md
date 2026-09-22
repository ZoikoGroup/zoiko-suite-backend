# notification-svc — Runbook

Operating this service: what its signals mean, what to do when one fires, and
which of its failure modes are silent.

**The thing to internalise first:** almost every interesting failure in this
service answers **2xx**. A FAILED delivery is a 201 by design; a rescheduled one
is a 201; a notification stranded in flight belongs to a request that succeeded
days earlier; and a lost event used to be a log line and nothing more. HTTP
metrics stay flat and green across all of it. Everything below keys on the
domain metrics and the register instead.

---

## 1. What it is

Governed delivery of notifications for workflows, deadlines, escalations,
approvals and status changes (03-microservices.md §9.7, ZS-SVC-Y-001).

| | |
|---|---|
| Port | 8133 |
| Database | `notification` (`app_notification` role) |
| Topic | `zoiko.notification.events` |
| Depends on | postgres, kafka, authorization-svc, identity-context-svc, an SMTP relay |
| Console | `/admin/notifications`, plus the topbar bell |

### What SENT means, per channel

This is the single most misread thing in the service, and ZS-SVC-Y-001 §0.4
forbids getting it wrong.

| Channel | SENT means | Read state |
|---|---|---|
| **IN_APP** | Genuinely delivered. The register row **is** the delivery. | `read_at` — real |
| **EMAIL** | A mail provider **accepted** the message. Not that it arrived, not that anyone read it. `provider_response` is the acceptance evidence. | Unknowable |
| **WEBHOOK** | Never SENT. No provider exists; §1.3 puts machine-to-machine exchange outside this service (XIC owns it). Records FAILED naming what is missing. | n/a |
| **SMS** | Withdrawn. Refused at the request boundary with 400. Historical rows still read back. | n/a |

### What PENDING means

`PENDING` has two meanings, and `next_attempt_at` separates them:

- **`next_attempt_at` set** — a transient failure is scheduled to be
  re-attempted. This is **not** a failure to alert on.
- **`next_attempt_at` NULL** — an attempt is **in flight right now**. If it is
  still that way fifteen minutes later, it is stranded — see §4.3.

---

## 2. Health and first checks

```bash
curl -fsS localhost:8133/healthz          # process up
curl -fsS localhost:8133/readyz           # database reachable — the probe compose uses
curl -fsS localhost:8133/metrics | grep notification_
```

`healthz` answers 200 with a dead database pool, which is why the container
healthcheck uses `readyz`.

### Startup lines worth reading

```
smtp credentials verified                     relay is reachable and accepts the credential
smtp credentials did not verify — ...         EMAIL will FAIL until fixed; IN_APP is unaffected
no email provider configured — EMAIL ...      NOTIFICATION_EMAIL_PROVIDER is unset
KAFKA_BROKERS is empty — events will be ...   dry run; the relay marks batches published
outbox relay started                          the event drain is running
delivery retry worker started                 the retry and stranded sweep are running
```

The SMTP verification is a **warning, not a fatal**, on purpose: refusing to
start would take IN_APP down with the mail relay, and IN_APP does not touch SMTP
at all. That coupling is precisely what §9.7 says must not exist.

---

## 3. The metrics, and what each one actually tells you

```
notification_deliveries_total{channel,status}            concluded deliveries
notification_delivery_attempts_total{channel,outcome,origin}  attempts (origin=request|retry)
notification_delivery_duration_seconds{channel}          one attempt against the provider
notification_retries_scheduled_total{channel}            transient failures rescheduled
notification_retries_exhausted_total                     gave up after the whole budget
notification_stranded_reclaimed_total                    notices the platform lost track of
notification_outbox_pending                              events committed, not yet on the bus
notification_outbox_oldest_age_seconds                   ← the alertable one
notification_outbox_publish_failures_total               drains that could not reach Kafka
```

Two of these should normally be **zero**, not merely low:

- `notification_stranded_reclaimed_total` — every increment is a notice this
  service accepted and then lost track of.
- `notification_retries_exhausted_total` — every increment is a governed notice
  the platform gave up on.

And the gap between `deliveries_total` and `delivery_attempts_total` is what the
mail relay is costing: one notification delivered on its fourth try is one
delivery and four attempts.

---

## 4. Incidents

### 4.1 `notification_outbox_oldest_age_seconds` is climbing

**What it means.** Events are being committed with their deliveries and not
reaching Kafka. Notices are still going out correctly and the register is
correct; what is broken is everything downstream that learns about them —
escalation chains waiting on `notification.failed`, audit sinks, anything
routing on `source_event_type`.

**Why age and not depth.** A backlog of ten that is three seconds old is a busy
service. A backlog of ten that is an hour old is a stopped relay, and that means
an hour of governed notices whose issue nobody has been told about while the
register shows every one of them concluded. Depth alone cannot tell those apart.

**Triage.**

```bash
docker logs notification-svc 2>&1 | grep -E "outbox drain failed|outbox relay"

docker exec -i zoiko-postgres psql -U postgres -d notification -tAc "
  SELECT count(*), min(created_at), max(attempts), max(last_error)
  FROM event_outbox WHERE published_at IS NULL;"
```

`last_error` is recorded **on the row**, precisely so a stuck event can be
diagnosed from the table without correlating against logs.

- Broker unreachable → fix Kafka. The relay retries on its own; nothing needs
  replaying by hand, because `published_at` is only set after a successful
  write.
- `Unknown Topic Or Partition` → the writer sets `AllowAutoTopicCreation`, so
  this means the broker has auto-creation off. Create
  `zoiko.notification.events`.
- Depth 0 but the gauge is stale → the relay goroutine is gone. Restart the
  service; the events are committed and will drain.

**Never** delete unpublished rows to clear the alert. They are the only record
that those notices concluded.

### 4.2 Notices are not going out at all

Work down, in this order:

```bash
# 1. Is anything being recorded?
docker exec -i zoiko-postgres psql -U postgres -d notification -tAc "
  SELECT status, count(*) FROM notifications
  WHERE created_at > now() - interval '1 hour' GROUP BY status;"

# 2. If they are FAILED — why?
docker exec -i zoiko-postgres psql -U postgres -d notification -tAc "
  SELECT channel, failure_reason, count(*) FROM notifications
  WHERE status='FAILED' AND created_at > now() - interval '1 hour'
  GROUP BY 1,2 ORDER BY 3 DESC;"
```

| `failure_reason` contains | Cause | Fix |
|---|---|---|
| `no email provider is configured` | `NOTIFICATION_EMAIL_PROVIDER` unset | Set it, with `SMTP_HOST` and `NOTIFICATION_EMAIL_FROM` |
| `recipient principal has no email address` | Settled fact about the recipient | Fix the principal in identity-context-svc; it will not retry |
| `identity-context-svc unavailable` | Transient | Restore the service; it retries on its own |
| `550` / `no such mailbox` | Settled refusal | Wrong address; no amount of retrying helps |
| `connection refused` / `4xx` | Transient | Relay down or greylisting; it retries |
| `webhook delivery is not this service's` | By design | XIC owns machine-to-machine exchange |

If nothing is being **recorded**, the caller is being refused: check for 400
`envelope_incomplete` (a missing §4 header) or 403 (no `NOTIFICATION_SEND`
grant) in the caller's logs, not this service's.

### 4.3 `notification_stranded_reclaimed_total` is rising

**What it means.** Notifications are being left in flight — `PENDING` with
nothing scheduled — and the sweep is rescuing them. Each one would otherwise
have been **never delivered, never failed, never retried**, and displayed as
PENDING, which reads as progress. Five such rows sat on the dev stack for six
days before the sweep existed.

A rescue is the system working. A rising **rate** means something upstream is
killing attempts mid-flight: restarts, OOM kills, or a provider slower than the
15s `WriteTimeout`.

```bash
docker exec -i zoiko-postgres psql -U postgres -d notification -tAc "
  SELECT notification_id, channel, created_at, delivery_attempts, last_attempt_at
  FROM notifications
  WHERE status='PENDING' AND next_attempt_at IS NULL
    AND COALESCE(last_attempt_at, created_at) < now() - interval '15 minutes'
  ORDER BY created_at LIMIT 20;"
```

**The trade, so nobody is surprised by it.** A stranded row may or may not have
reached the provider, and nothing on the row can distinguish those. The sweep
reschedules, so a recipient may get a notice twice. That is the right way to be
wrong here — the alternative guarantees some governed notices never arrive —
and `NOTIFICATION_STRANDED_AFTER` (default 15m, against a longest-possible
attempt of ~30s) is what keeps it to the genuine-crash case.

`NOTIFICATION_STRANDED_AFTER=0` is a **true off switch**, not "sweep everything
now". Setting it low is the one configuration that reliably duplicates live
sends.

### 4.4 Everything answers 403

The acting principal has no grant. Sending (`NOTIFICATION_SEND`) and reading
(`NOTIFICATION_VIEW`) are **separate** grants on a legal entity; holding one
does not imply the other.

```bash
docker exec -i zoiko-postgres psql -U postgres -d authorization_svc -tAc "
  SELECT r.role_code, p.action_type, ra.legal_entity_id
  FROM role_assignments ra
  JOIN roles r ON r.role_id = ra.role_id
  JOIN role_permissions rp ON rp.role_id = r.role_id
  JOIN permissions p ON p.permission_id = rp.permission_id
  WHERE ra.principal_id = '<principal>' AND p.action_type LIKE 'NOTIFICATION%';"
```

A 503 `authz_unavailable` is different: authorization could not be **verified**,
so the action was refused. That is a fail-closed refusal, not a denial — restore
authorization-svc.

### 4.5 Everything answers 400 `envelope_incomplete`

The caller is not sending the canonical §4 header set. Reads need
`X-Tenant-Id`, `X-Principal-Id`, `X-Request-Id`, `X-Source-Channel`; writes
additionally need `Idempotency-Key`. This is refused **before any handler runs**,
so nothing is recorded.

Every collection on this estate written before enforcement has this on every
write. `postman_collection.json` here mints the headers in a collection-level
pre-request script for exactly that reason.

---

## 5. Routine operations

### Seeding a grant for manual testing

```sql
-- In authorization_svc. Additive; denies nothing.
INSERT INTO roles (role_id, tenant_id, role_code, role_name)
VALUES (gen_random_uuid(), '<tenant>', 'NOTIFIER', 'Notification sender')
ON CONFLICT DO NOTHING;
-- then bind NOTIFICATION_SEND and NOTIFICATION_VIEW permissions to it and
-- assign it to the principal on the legal entity.
```

### Reading the register for one recipient

```sql
SELECT notification_id, channel, status, recipient_address,
       recipient_address_source, delivery_attempts, failure_reason,
       created_at, sent_at, read_at
FROM notifications
WHERE tenant_id = '<tenant>' AND recipient_principal_id = '<principal>'
ORDER BY created_at DESC LIMIT 50;
```

`recipient_address_source` is the answer to "did we send this to an address the
identity authority vouched for, or one the caller handed us" — the question that
matters in a dispute about a statutory notice.

### Local mail

Mailpit catches everything at <http://localhost:8025>. Nothing leaves the
machine, which is the point: with a real relay configured locally, running the
estate would email real people the first time anyone exercised an approval
workflow.

### Validating the NOT VALID constraints

Migrations 000002–000004 add every CHECK as `NOT VALID`: enforced on new writes,
not scanned against history. The history includes rows the service wrote while
it was wrong (a `PIGEON` channel, legacy SMS), and a migration that silently
rewrote those would be worth less than one that leaves an awkward row visible.

```sql
-- Count violations first. VALIDATE fails outright on any violating row.
SELECT count(*) FROM notifications WHERE channel NOT IN ('EMAIL','SMS','IN_APP','WEBHOOK');
ALTER TABLE notifications VALIDATE CONSTRAINT notifications_channel_known;
```

This stays a per-environment operator step, deliberately: a validating migration
would hard-fail on deploy against any environment whose backlog is not clean,
taking the service down to gain nothing.

---

## 6. Configuration

| Variable | Default | Notes |
|---|---|---|
| `PORT` | 8133 | |
| `KAFKA_BROKERS` | — | **Empty means "no bus"**: the relay dry-runs and marks batches published, rather than growing a backlog nothing will drain |
| `KAFKA_EVENTS_TOPIC` | `zoiko.notification.events` | |
| `AUTHZ_SERVICE_URL` | — | Fail-closed if unreachable |
| `IDENTITY_SERVICE_URL` | — | Recipient resolution. Without it, EMAIL has no address |
| `NOTIFICATION_EMAIL_PROVIDER` | *(empty)* | Empty = EMAIL records FAILED naming the missing provider. A deliberate default: a service that invented a mail server would send real mail from a laptop |
| `SMTP_VERIFY_ON_START` | true | Opens one session, sends nothing. Warns, never fatal |
| `SMTP_ALLOW_CLEARTEXT` | false | Required for a non-loopback relay like mailpit. Refused in production |
| `NOTIFICATION_RETRY_ENABLED` | true | False pins attempts to 1 — reported on the record, not silent |
| `NOTIFICATION_RETRY_MAX_ATTEMPTS` | 5 | |
| `NOTIFICATION_RETRY_BASE_DELAY` | 30s | |
| `NOTIFICATION_RETRY_MAX_DELAY` | 8m | |
| `NOTIFICATION_RETRY_INTERVAL` | 10s | How often the worker *looks*, not how long a notice waits |
| `NOTIFICATION_STRANDED_AFTER` | 15m | Must exceed the longest attempt (~30s). `0` is off, **not** "sweep now" |

---

## 7. Deploying

Migrations are globbed and applied in order. **000005 must be applied before the
new binary starts**: the enqueue runs inside the delivery transaction, so
without `event_outbox` every send fails at the INSERT.

Rolling back past 000005 requires draining the outbox first —
`SELECT count(*) FROM event_outbox WHERE published_at IS NULL` must be 0 — or
the drop discards events for notifications that DID conclude, which is the loss
the table exists to prevent, performed deliberately.

Two replicas are safe. The retry claim is the row itself (`next_attempt_at IS
NOT NULL` in the UPDATE predicate) and the outbox claim is `FOR UPDATE SKIP
LOCKED`; neither needs an advisory lock.

---

## 8. Known limits

- **The register grows without bound.** One row per notification forever,
  carrying every notice's subject, body and recipient address. No retention or
  partitioning. The rows *are* the evidence of what was sent to whom, so a purge
  is a governance decision, not a cleanup — and how long a notice's **body**
  should be kept needs a human before the mechanism does. Tracker row 97d.
- **Adoption, not capability, is the gap.** §9.7 gives this service "workflows,
  deadlines, escalations, approvals, and status changes". Wiring each producer
  is a per-workflow product decision — which service notifies whom, on what
  event, from which template. The path is proven; what is missing is the
  decisions. Tracker row 97c.
- **Port 8133 collides with `exception-escalation-svc`** in
  `docker-compose.phase5.yml`. Both bind host 8133, so they cannot run together,
  and a console pointed at `localhost:8133` reaches whichever is up. Port
  allocation is the backend's call to make; noted here so it is not diagnosed
  from scratch.
