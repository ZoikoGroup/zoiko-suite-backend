# notification-svc — Release Certificate

**Service:** notification-svc (port 8133)
**Spec:** ZS-SVC-Y-001 — Notification, Communication & Delivery Control;
03-microservices.md §9.7
**Certified:** 2026-09-22
**Verified against:** the running stack — PostgreSQL 16, Kafka, authorization-svc,
identity-context-svc, mailpit — not against stubs.

Re-run the proof at any time:

```bash
bash services/notification-svc/scripts/audit.sh          # 162 checks
RUN_E2E=1 bash services/notification-svc/scripts/audit.sh  # + the console suite
```

The script exits non-zero if anything fails, so CI can gate on it.

---

## 1. Result

| | |
|---|---|
| **scripts/audit.sh** | **163 / 163 checks — 100%** (with `RUN_E2E=1`; 162 without) |
| Go test suites | **165 tests**, 0 failed, **0 skipped** |
| Console e2e (Playwright) | **21 / 21** in `e2e/notifications.spec.ts` |
| `go build` / `go vet` / `gofmt` | clean |
| Console `tsc --noEmit` | clean |

**0 skipped is asserted, not eyeballed.** A suite that skipped is a suite that
verified nothing, and it reports `ok` either way — which is how a store
integration suite can sit green for months against no database at all.

---

## 2. What this release fixed

### 2.1 Every domain event was being lost silently on a broker hiccup — FIXED

**The defect.** Both producers — the handler and the retry worker — wrote to
Kafka *after* the delivery transaction had committed, with the error logged and
discarded. `Publisher.emit`'s entire answer to a broker refusal was one
`log.Error` and a `return`.

A broker hiccup at the moment a notice concluded therefore left: the delivery
correctly recorded, the caller correctly told 201, the register correctly
showing SENT — and **no consumer anywhere learning** that the notification went
out, or that it did not.

**Why it could not be noticed.** The loss is silent by construction: the one
thing that would have reported it is the event that was lost. An escalation
chain waiting on `notification.failed` before paging a human simply never
fires — nothing is in an error state, no retry is scheduled, no metric moves,
and the register shows a healthy row.

**The fix.** Migration `000005_event_outbox` plus `internal/outbox`.
`CompleteDelivery` takes the sealed event as a **required** argument and
enqueues it in the same transaction as the status transition, so a notification
that concluded always has its event and an event that exists always has its
conclusion. A background relay drains the outbox to Kafka and retries until the
write lands. `PublishSent` and `PublishFailed` are **removed**, not merely
unused — the audit greps for their absence, because a publisher that can still
be called after a commit is the defect waiting to be re-introduced.

**Proven:** 6 store integration tests against real PostgreSQL, 6 relay tests, and
live — 3 events enqueued with their deliveries, drained to
`zoiko.notification.events` on the broker, `notification_outbox_pending` at 0
and `notification_outbox_publish_failures_total` at 0.

### 2.2 The one route documented as authenticated was the only one that was not — FIXED

`GET /v1/notifications/templates` carried this comment:

> "It still requires a caller identity, so this is not an anonymous endpoint —
> the envelope middleware ahead of it refuses an unattributed request."

It did not. Enforcement runs in **write-strict** mode (the default), where a
read's envelope is parsed and *reported* and the request is then admitted.
Measured against the running service: `curl http://localhost:8133/v1/notifications/templates`
with no headers at all returned **200 and the whole catalogue**.

The disclosure is small — the catalogue is compiled into the binary, identical
for every tenant, holding no tenant data — which is exactly why it survived:
nothing downstream could go wrong in a way anyone would notice. A control that
reads as present and does nothing is worse than no control. The handler now
checks identity itself, like every other route. Verified live: **401**.

### 2.3 The console shipped mojibake in text users read — FIXED

`SendNotificationForm.tsx` had been saved once as cp1252 over UTF-8. Nine
corrupted sequences, two of them in rendered UI:

- the template picker's default option read `â€" none, write the subject and body below â€"`
- the submit button read `Sendingâ€¦` while a send was in flight

The file also carried a UTF-8 BOM. Both repaired; the audit now greps for the
byte sequence so it cannot come back.

### 2.4 No visibility into a service whose every failure answers 2xx — FIXED

A FAILED delivery is a 201 by design; so is a rescheduled one; a stranded
notification belongs to a request that succeeded days earlier. So
`http_requests_total{status_code="2xx"}` stays flat and green while the platform
delivers nothing, and **no number anywhere moved**.

Ten domain metrics added (`internal/telemetry/domain.go`), including the pair
that makes a stalled relay visible — `notification_outbox_pending` and
`notification_outbox_oldest_age_seconds`. Depth alone cannot distinguish a busy
service from a stopped relay; the age can.

### 2.5 The contract artefacts did not exist — ADDED, then corrected

`openapi.yaml`, `asyncapi.yaml`, `postman_collection.json`, `RUNBOOK.md`,
`context.md`, `scripts/audit.sh`, `scripts/spec_query.py`.

Worth recording: **the first draft of all three contract artefacts reproduced
this estate's recurring defect.** They documented five mandatory write headers
where the service demands six — `X-Legal-Entity-Id` is `RequiredOnWrite` here
(INV-02) — so a client built strictly from them would have had **every write
refused** with `400 envelope_incomplete`, for a reason that reads like an
authorization problem and is not. The audit caught it by driving the live
service, which is the point of driving the live service. All three corrected;
the Postman collection mints the full envelope in a collection-level pre-request
script, which cannot be left off one request the way a header list can.

The spec also documented one error body shape where the service emits **two**
(`error_code`/`error_message` from handlers, `error`/`detail`/`violations` from
the envelope). Both are now documented.

### 2.6 The console had no e2e coverage at all — ADDED

`e2e/mock/notification-service.mjs` (hermetic contract) and
`e2e/notifications.spec.ts` (21 tests), wired into `playwright.config.ts`.

This matters more here than on most surfaces, because the console is the only
place the 2xx distinctions become visible to a person. The spec asserts the four
readings are rendered differently, that a rescheduled delivery reads as
**RETRYING** rather than FAILED or PENDING, that an EMAIL send reports
**acceptance and not receipt**, that a WEBHOOK send is surfaced as FAILED
despite its 201, that the register read is entity-scoped **on the wire** while
the bell's is not, and that an unreadable inbox shows *unknown* rather than
zero — because "0" and "we could not find out" are different facts and only one
is safe to imply.

---

## 3. Controls verified live

| Control | Evidence |
|---|---|
| Canonical §4 envelope enforced | A write missing `Idempotency-Key` is `envelope_incomplete` and **leaves no record** — refused ahead of the handler |
| Identity fail-closed | No tenant → 401; no principal → 401; no headers at all → 401 |
| `NOTIFICATION_SEND` ≠ `NOTIFICATION_VIEW` | Both distinct actions; both bundled in authorization-svc; an ungranted entity read is 403 |
| Register read is authorized, not merely filtered | Reading another principal's inbox is `forbidden`; an ungranted entity register read is refused |
| Idempotency on (tenant, correlation_id) | A replay is 200 with the same id and **no second row** |
| Tenant isolation | `FORCE ROW LEVEL SECURITY` on both tables; another tenant's notification is **404, not 403** |
| Two distinct cross-tenant hatches | `app.platform_scope` (SELECT-only, retry) ≠ `app.outbox_relay` (relay); asserted distinct |
| Empty-GUC trap guarded | The outbox policy uses `NULLIF(current_setting(...), '')` |
| SMS refused at the boundary | 400 `unsupported_channel`, **no stored FAILED row** |
| A failed delivery does not collapse the workflow | A WEBHOOK send is **201** and recorded FAILED **with a reason** |
| Address provenance recorded | A caller-supplied address stores `REQUEST`; **no row** has an address without provenance |
| Read state is the recipient's own | A non-recipient gets 403; a repeat mark keeps the **first** read; an EMAIL mark is refused; **no** EMAIL row carries read state |
| The unread badge cannot report on colleagues | No principal parameter on the route |
| Exactly one event per conclusion | Only the two known types enqueued; every event keyed to a real notification |
| No message content on the bus | The stored envelope carries no `subject`, no `body`, no `recipient_address` |
| Nothing stranded | 0 in-flight beyond the threshold; 0 concluded rows with a retry scheduled |
| Non-UUID id is 404, not 503 | An outage status for a typo in a URL was the old behaviour |

---

## 4. Known limits, carried forward deliberately

These are **not** defects left unfixed; each is a decision with a reason.

1. **Nothing on the estate sends a notification.** §9.7 gives this service
   workflows, deadlines, escalations, approvals and status changes; none produce
   one today. Which service notifies whom, on what event, from which template is
   a product decision per workflow — picking one to wire would be inventing that
   decision rather than encoding it. The path is proven end to end; adoption is
   wiring, not discovery. *Tracker row 97c.*

2. **The register grows without bound.** One row per notification forever,
   carrying subject, body and recipient address. The rows *are* the evidence of
   what was sent to whom, so a purge is a governance decision; and how long a
   notice's **body** should be kept needs a human before the mechanism does.
   *Tracker row 97d.*

3. **`NOT VALID` constraints stay `NOT VALID`.** Enforced on every new write;
   the historical rows they would reject are the account of what this service
   did when it was wrong. `VALIDATE CONSTRAINT` remains a per-environment
   operator step, because a validating migration hard-fails on deploy against
   any environment whose backlog is not clean.

4. **Port 8133 collides with `exception-escalation-svc`** in
   `docker-compose.phase5.yml`. They cannot run together, and a console pointed
   at `localhost:8133` reaches whichever is up. Port allocation is the backend's
   call rather than something to resolve from inside one service. Recorded in
   RUNBOOK §8 so it is not diagnosed from scratch; the hermetic e2e suite pins
   its mock at 18133 and is unaffected.

5. **WEBHOOK is accepted by the handler and refused by the deliverer**, by
   design — §1.3 puts machine-to-machine delivery outside this service's remit.
   A WEBHOOK send therefore stores a FAILED row saying so.

---

## 5. Deploy note

**Migration 000005 must be applied before the new binary starts.** The enqueue
runs inside the delivery transaction, so without `event_outbox` every send fails
at the INSERT. Rolling back past it requires draining the outbox to empty first
(`SELECT count(*) FROM event_outbox WHERE published_at IS NULL` = 0), or the
drop discards events for notifications that **did** conclude — the exact loss
the table exists to prevent, performed deliberately.

Two replicas are safe: the retry claim is the row itself (`next_attempt_at IS
NOT NULL` in the UPDATE predicate) and the outbox claim is `FOR UPDATE SKIP
LOCKED`. Neither needs an advisory lock.
