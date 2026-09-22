# notification-svc — design context

Why this service is built the way it is. The runbook says how to operate it and
the OpenAPI says what it answers; this says what was decided and what the
alternative cost.

Spec: **ZS-SVC-Y-001** (Notification, Communication & Delivery Control), and
**03-microservices.md §9.7**.

---

## 1. The one thing that shapes everything else

**Almost every interesting failure in this service answers 2xx.**

- A FAILED delivery is a **201**, deliberately. §9.7 requires that notification
  failure must not collapse the source operational workflow: a payroll run that
  finalized correctly cannot be told it failed because an employee has no
  address on file.
- A transiently failed delivery is **also a 201**, with `status: PENDING` and a
  schedule on it. Neither sent nor failed.
- A replayed send is a **200** with the original notification.
- A notification stranded in flight belongs to a request that **succeeded days
  earlier**.
- An event that never reached the bus used to be a **log line and nothing
  more**.

Three consequences run through the whole design:

1. **Callers must read `status`, never the HTTP code.** The OpenAPI says so at
   the top, the console says so on the page, and the Postman collection says so
   in its description.
2. **The domain metrics in `internal/telemetry/domain.go` are not decoration.**
   `http_requests_total{status_code="2xx"}` is flat and green across every
   failure above. Before those counters existed, no number anywhere moved when
   the platform stopped delivering.
3. **The audit asserts on the database and the metrics**, not on response
   codes. A smoke test that checks for 200s proves almost nothing here.

---

## 2. What SENT is worth, per channel

ZS-SVC-Y-001 §0.4 forbids treating a provider's acceptance as proof that a
person received, read or was legally served with a notice. That single sentence
decides a surprising amount.

| Channel | SENT means | Why |
|---|---|---|
| **IN_APP** | Genuinely delivered | The register row **is** the delivery. The recipient reads it back from the same register and no third party stands between the claim and the fact. The only channel that can honestly claim receipt, and the only one with read state. |
| **EMAIL** | A provider **accepted** it | `provider_response` is the acceptance evidence. Deliberately not named `delivered_at` or `receipt` — a field called `provider_response` cannot be misread as the stronger claim. |
| **WEBHOOK** | Never SENT | No provider exists, by design: §1.3 puts machine-to-machine exchange outside this service (XIC owns it). It records FAILED naming what is missing, rather than reporting a send. |
| **SMS** | Withdrawn | It was accepted, resolved a recipient, and then failed every send — the one channel advertising a capability the platform does not have. Now refused at the request boundary with the same 400 an unknown channel gets. |

**SMS is a withdrawal, not a judgement.** Restoring it means adding a provider
to `internal/deliver` and putting `"SMS"` back in `supportedChannels`. Existing
SMS rows are untouched and still render: the schema's channel CHECK still
permits the value, because the register is the account of what this service
did — including what it did wrongly.

---

## 3. The transactional outbox (migration 000005)

### What was wrong

Both producers — the handler and the retry worker — wrote to Kafka **after**
the delivery transaction had committed, with the error logged and discarded.
`Publisher.emit`'s entire answer to a broker refusal was

```go
p.log.Error("failed to publish event", ...)
```

and then it returned.

So a broker hiccup at the moment a notice concluded left: the delivery
correctly recorded, the caller correctly told 201, the register correctly
showing SENT — and **no consumer anywhere ever learning** that the notification
went out or that it did not.

### Why it is worse here than elsewhere

configuration-feature-flag-svc had the same shape (its 000003, which this
mirrors). There, a lost `config.updated` leaves a consumer serving a superseded
but valid value. Here, a lost `notification.failed` leaves a person who was
never told something they were entitled to be told, and a platform that
believes they were.

And the loss is **silent by construction**: the one thing that would have
reported it is the event that was lost. An escalation chain waiting on
`notification.failed` before paging a human simply never fires — nothing is in
an error state, no retry is scheduled, no metric moves, and the register shows
a healthy row.

### The design

`CompleteDelivery` takes the sealed event as a **required** argument and
enqueues it in the same transaction as the status transition. "Conclude a
delivery and tell nobody" is not a state a caller can reach by forgetting an
argument, which is precisely how the old shape failed.

`internal/events` is split to match: `Sent`/`Failed` **seal** an envelope and
are called inside the transaction; `Publish` hands sealed envelopes to Kafka
and is called from `internal/outbox`'s relay, where a failure is a retry.
`PublishSent` and `PublishFailed` are **gone**, not merely unused — a publisher
that can still be called after a commit is the defect waiting to be
re-introduced, and the audit greps for their absence.

### The trade, stated

Because the enqueue shares the transaction, a failure to enqueue fails the
whole conclusion — and by then the provider has already accepted the message.
The row stays PENDING in flight, the sweep reclaims it, and the recipient may
get a second copy.

That is the worse-looking outcome and still the right one. The enqueue can only
fail if `event_outbox` is missing or its CHECK rejects the event type — deploy
faults that affect every send equally and want to be loud. Committing the
conclusion and dropping the event is the failure that is silent, permanent, and
indistinguishable from success.

### Exactly one event per notification

Nothing is emitted on creation, and nothing on a rescheduled retry. A
notification awaiting another attempt has **not failed**, and emitting a failure
a later attempt reverses would have consumers act on an outcome that did not
happen. One event, at the SENT or FAILED transition, with `delivery_attempts`
saying how much work it took.

This is why `ScheduleRetry` takes no event while `CompleteDelivery` requires
one.

---

## 4. Row-level security, and two distinct hatches

Both tables are `FORCE ROW LEVEL SECURITY`. Postgres exempts a table's owner
from RLS unless FORCE is declared, and these services connect as the owner — so
000001's policy applied to precisely nothing until 000002 forced it. A policy
that silently does nothing is worse than no policy, because it reads as a
control that is present.

There are **two** cross-tenant hatches, and they are deliberately different
flags:

| Flag | Table | Grants | For |
|---|---|---|---|
| `app.platform_scope` | `notifications` | **SELECT only** | The retry worker discovering due and stranded work |
| `app.outbox_relay` | `event_outbox` | SELECT **and** UPDATE | The relay draining every tenant's backlog and marking it published |

Reusing one name would have silently widened the retry hatch from read to write
across every tenant's notification bodies. The audit asserts they are distinct.

The retry hatch is narrow in a second way that RLS itself cannot express: RLS
cannot restrict columns, so the policy does expose message bodies to a
connection that sets the flag. What bounds it is the **caller** —
`FindDueRetries` projects `notification_id` and `tenant_id` and nothing else,
then drops platform scope and re-enters per tenant to read the message. No
message content crosses the hatch.

Both policies use `NULLIF(current_setting(...), '')`. Postgres keeps a custom
GUC in the SESSION after a transaction-local SET is reset, with `''` as its
value, so on any pooled connection that has already served a request these read
as the empty string rather than NULL.

---

## 5. The two meanings of PENDING

No `RETRYING` status was added. `PENDING` already means "delivery has not
concluded", and `next_attempt_at` separates the cases:

```
PENDING + next_attempt_at set  → will be attempted again
PENDING + next_attempt_at NULL → in flight right now
SENT                           → a provider accepted it
FAILED                         → terminal
```

A `RETRYING` status would have meant widening
`notifications_status_known`, updating every consumer's vocabulary, and teaching
the console a fourth state — and would have made FAILED **ambiguous** for as
long as the rollout took, since a FAILED row would mean either "terminally
failed" or "failed once, from an older binary that had no RETRYING".

The console renders the second meaning as `RETRYING` anyway, as a *reading*
rather than a status, because showing a raw `PENDING` next to a failure reason
reads as a contradiction.

---

## 6. The stranded sweep

"In flight right now" is invisible to the retry path: `FindDueRetries` requires
`next_attempt_at IS NOT NULL`. So a notification whose attempt never reported an
outcome sat there **permanently** — never delivered, never failed, never
re-attempted, and displayed as PENDING, which reads as progress.

`RunOnce`'s own comment said a claimed row left "PENDING with nothing scheduled
... is what the sweep below is for". **There was no sweep**, in that file or
anywhere else. Three places in the service described the failure mode and none
built the remedy. Measured on the dev database 2026-09-08: five notifications
from six days earlier, `delivery_attempts = 0`, never attempted at all.

The sweep **only ever schedules**. Delivery goes through the ordinary due path,
so exactly one code path sends and one decides what an outcome means. Reclaimed
rows keep their attempt count, so being stranded buys no fresh retry budget.

`COALESCE(last_attempt_at, created_at)` is the in-flight clock — a row stranded
before its first attempt has no `last_attempt_at` at all, which was the whole of
the real case. Proven by negative control: dropping the COALESCE fails exactly
the never-attempted tests and nothing else.

**The threshold is the safety property.** A row being attempted right now is
indistinguishable from an abandoned one except by how long it has looked that
way. `NOTIFICATION_STRANDED_AFTER` defaults to 15 minutes against a
longest-possible attempt of ~30s (SMTP timeout 10s, `WriteTimeout` 15s). `0` is
a **true off switch**, never "sweep everything now" — which is the one setting
that would duplicate every live send. Negative values read as off for the same
reason.

---

## 7. Authorization

Two grants, and they are separate:

- `NOTIFICATION_SEND` — checked against the body's `legal_entity_id` on a send.
- `NOTIFICATION_VIEW` — checked against the entity on a register read, and
  against the notification's **own** entity on a single read, so a caller
  cannot name an entity they hold a grant on to read a notice belonging to
  another.

**`GET /v1/notifications/` is two different reads.** With `legal_entity_id` it
is the entity's register, authorized against it; without, it is the caller's own
inbox, with the recipient filter forced to the calling principal and any other
`recipient_principal_id` refused rather than silently rewritten.

The authorization used to be **conditional on the filter being present**, so
omitting it — the easier request to make — returned every notification in the
tenant, across every legal entity, with subjects and bodies, to a principal
holding no grant at all. A read is authorized by who is asking, never by which
query parameters they happened to send.

**Read state is not a `NOTIFICATION_VIEW` action.** That grant lets an
administrator read the register, and reading the register is not the recipient
reading their notice — an administrator opening the audit view must not clear
somebody else's unread badge. The only principal who can mark a notification
read is the one it was addressed to, and the store enforces it in the statement.

**`MarkRead` keeps the FIRST read** (`COALESCE`). Inboxes re-issue the mark on
every render, and without it "when did they first see this" — the only question
`read_at` can answer — would decay into "when did they last look".

### The catalogue route, which was not what it said it was

`GET /v1/notifications/templates` documented itself as authenticated:

> "It still requires a caller identity, so this is not an anonymous endpoint —
> the envelope middleware ahead of it refuses an unattributed request."

It did not. Enforcement runs in **write-strict** mode (the default), where a
read's envelope is parsed and *reported* and the request is then admitted.
Measured against the running service on 2026-09-22, a bare

```
curl http://localhost:8133/v1/notifications/templates
```

with no headers at all returned 200 and the whole catalogue. The one route on
this service documented as authenticated was the one route that was not.

The disclosure is small — the catalogue is compiled into the binary, identical
for every tenant, and holds no tenant data — which is precisely why it survived:
nothing downstream could go wrong in a way anyone would notice. It now checks
identity in the handler, like every other route.

---

## 8. Recipient address as evidence

The register records **where a message actually went** and **who vouched for
that address**, because "which address did we use" is the whole question when
somebody says they never got a notice.

- `recipient_address` is a **snapshot**, never recomputed. Resolving it again at
  read time answers "where would we send this today"; the question a delivery
  register exists to answer is "where did we actually send it", and those differ
  precisely when it matters.
- `recipient_address_source` is `IDENTITY_CONTEXT` or `REQUEST`. §0.4 names
  "mandatory notices being sent to an unverified or stale free-text address with
  no recipient provenance" as a thing this control plane exists to prevent, and
  provenance is only a control if it is recorded.

The caller-supplied override exists because not every recipient is an
established principal: `registration_received` goes to someone whose
organization has not been approved yet. The console marks such an address
**caller-supplied** in the register.

A `CHECK` enforces `(recipient_address IS NULL) = (recipient_address_source IS
NULL)` — an address with no provenance is reachable only by a bug, since the two
columns are written by the same code path.

---

## 9. Things deliberately left alone

**`NOT VALID` constraints stay `NOT VALID`.** Every CHECK in 000002–000004 is
enforced on new writes and skips the scan that would reject the table over rows
already written. Those rows are the audit trail of what this service actually
did — including a `PIGEON` channel from when a typo became a stored FAILED
delivery. A migration that quietly rewrites them is worth less than one that
leaves an awkward row visible. Validation stays a per-environment operator step:
`VALIDATE CONSTRAINT` scans and fails outright on any violating row, so a
validating migration would hard-fail on deploy against any environment whose
backlog is not clean.

**The register grows without bound.** One row per notification forever, carrying
every notice's subject, body and recipient address. The rows *are* the evidence
of what was sent to whom, so a purge is a governance decision rather than a
cleanup — and the data-minimisation question (how long a notice's **body**
should be kept) needs a human before the mechanism does. Probably monthly
partitions with DETACH, not DELETE, once volume justifies it. Tracker row 97d.

**Nothing on the estate sends a notification.** §9.7 gives this service
"workflows, deadlines, escalations, approvals, and status changes"; none of
those produce a notification today. Which service notifies whom, on what event,
from which template is a **product decision per workflow**, and picking one to
wire would be inventing that decision rather than encoding it. The path is
proven end to end; adoption is wiring rather than discovery. Tracker row 97c.

**Port 8133 collides with `exception-escalation-svc`** in
`docker-compose.phase5.yml`. Both bind host 8133, so they cannot run together,
and a console pointed at `localhost:8133` reaches whichever is up. Port
allocation is the backend's call to make rather than something to resolve from
inside one service; it is recorded in the runbook so it is not diagnosed from
scratch. The hermetic e2e suite pins its mock at 18133 and is unaffected.

---

## 10. Where the proof lives

| | |
|---|---|
| `scripts/audit.sh` | 163 checks against the **running** stack (162 without `RUN_E2E=1`). Re-runnable; exits non-zero on any failure. |
| Go suites | 165 tests, including store integration against real PostgreSQL 16 via `notification_test` |
| `zoiko-suite-frontend-platform/e2e/notifications.spec.ts` | 21 Playwright tests over the console and the bell |
| `zoiko-suite-frontend-platform/e2e/mock/notification-service.mjs` | The hermetic contract the console is tested against |

The store suite refuses to run against a database whose name does not contain
"test" (`requireThrowawayDatabase`). It DROPs `notifications`, and those rows are
the record of which notices went out; authorization-svc's equivalent suite had
no such guard and erased that stack's fixtures on 2026-09-08.
