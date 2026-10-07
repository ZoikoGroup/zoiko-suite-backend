# notification-svc — Progress

## Status: complete and working, with no callers (2026-09-08)

Six routes, all wired to the Next.js console. Templates, retry with
exponential backoff and jitter, the stranded-delivery sweep added this pass,
forced row-level security, and both events Doc 03 §9.7 requires
(`notification.sent`, `notification.failed`).

The service is not the gap. **Nothing on the estate sends a notification** —
see "Still open" below.

## What was already here, and is good

Worth stating because this pass changed little of it, and because several
patterns here are ahead of the rest of the estate:

- **The store suite refuses to run against a database whose name does not
  contain "test"** (`requireThrowawayDatabase`), with an error naming
  `notification_test` explicitly. The suite DROPs `notifications`, and those
  rows are the record of which notices went out. authorization-svc's
  equivalent suite has no such guard and erased that stack's fixtures on
  2026-09-08 — this service had already solved it.
- **Migrations are globbed and sorted, not listed.** The comment says why: the
  list was left behind exactly once already when 000004 landed, and "a warning
  about a footgun is not a guard against it". It also strips a UTF-8 BOM,
  because 000003 shipped with one and psql tolerates what `pool.Exec` does not.
- **An unrecognised channel is a 400 at the boundary, not a stored FAILED
  row.** A caller's typo used to become permanent evidence of a delivery
  attempt no provider ever saw.
- **`SENT` means "a provider accepted it", never "it arrived"** — stated on
  `ProviderResponse` and honoured throughout, per ZS-SVC-Y-001 §0.4.
- **The recipient address is a snapshot with provenance**
  (`IDENTITY_CONTEXT` / `REQUEST`), never recomputed at read time, because
  "which address did we actually use" is the whole question when somebody says
  they never got a notice.
- **`MarkRead` keeps the FIRST read** (`COALESCE`) and only the recipient can
  set it — an administrator holding `NOTIFICATION_VIEW` over the entity is not
  the recipient reading their notice.
- **The retry claim is the row itself** (`next_attempt_at IS NOT NULL` in the
  UPDATE predicate), so no `SKIP LOCKED` and no advisory lock is needed for
  two replicas to poll the same second safely.
- **One cross-tenant read in the whole service**, under a SELECT-only
  platform-scope policy, projecting nothing but an id and a tenant, with every
  write that follows it re-scoped to the notification's own tenant.

---

# First pass — 2026-09-08

## The live bug: notices accepted and then never sent

PENDING with `next_attempt_at` NULL means "in flight right now" — the state
`ClaimRetry` creates deliberately, and `domain.Notification` documents. Nothing
in the service ever moved such a row again: `FindDueRetries` requires
`next_attempt_at IS NOT NULL`, so a send whose attempt never reported an
outcome sat there permanently. **Never delivered, never failed, never
re-attempted**, and displayed as PENDING, which reads as progress rather than
as a governed notice that silently did not go out.

Four ways in, all of them real:

1. The process dies between `CreateNotification` and the statement that
   concludes or reschedules the send.
2. `ScheduleRetry` fails. The handler answers 503 and returns, leaving the row
   it just created in flight.
3. `CompleteDelivery` fails, identically.
4. The request context is cancelled mid-attempt — which it can be, because the
   delivery call *and* the store write recording its outcome both ran on
   `r.Context()`, and the server's `WriteTimeout` is 15s.

**The service described this failure mode three times and never built the
remedy.** `RunOnce`'s shutdown branch said a claimed row is "PENDING with
nothing scheduled, which the sweep below is for" — there was no sweep, in that
file or anywhere else. `attempt`'s read-failure path logs a row being "stalled
PENDING with no schedule". And `domain.Notification` spells out both meanings
of the flag. Three descriptions, no mechanism.

Measured on the dev stack:

| | |
|---|---|
| stranded rows | **5** |
| created | 2026-09-02 (six days earlier) |
| `delivery_attempts` | **0** — never attempted at all |
| `last_attempt_at` | NULL |
| channel / recipient | EMAIL to a real address, real subjects |

## The fix

`Worker.SweepStranded`, plus `PgStore.FindStrandedDeliveries` and
`PgStore.ReviveStranded`.

**It only ever SCHEDULES.** Delivery still goes through the ordinary due path,
so exactly one code path sends and one decides what an outcome means. Reclaimed
rows keep their attempt count, so being stranded buys no fresh retry budget and
the sweep cannot become a resend loop.

**`COALESCE(last_attempt_at, created_at)` is the in-flight clock.** A row
stranded before its first attempt has no `last_attempt_at` at all — which was
the whole of the real case — and a predicate on `last_attempt_at` alone would
skip precisely those. Proven by negative control: dropping the COALESCE fails
exactly the never-attempted test.

**The threshold is the safety property.** A row being attempted right now is
indistinguishable from an abandoned one except by how long it has looked that
way, so reviving too eagerly sends the message twice.
`NOTIFICATION_STRANDED_AFTER` defaults to 15 minutes against a longest-possible
attempt of ~30s (SMTP timeout 10s, `WriteTimeout` 15s) — two orders of
magnitude of headroom. `0` is a **true off switch**, never "sweep everything
now", which is the one setting that would duplicate every live send; negative
values are read as off for the same reason.

**The duplicate-send trade, stated rather than hidden.** A stranded row may or
may not have reached the provider, and nothing on the row can distinguish
those. Rescheduling risks a second copy of a notice; not rescheduling
guarantees some governed notices never arrive. For this service the first is
the right way to be wrong. SENT rows are never touched.

Tenant posture matches the existing retry path exactly: platform-scope SELECT
only, a projection of nothing but id and tenant, and every write under the
notification's own tenant with the tenant installed on the context the same way
`attempt` installs it.

## Also fixed: the outcome of an attempt already made is no longer lost with the request

`CompleteDelivery`, `ScheduleRetry` and both event publishes ran on
`r.Context()`, which is cancelled when the response is written and sooner if
the client disconnects or the 15s `WriteTimeout` fires. Once the provider has
been called, what happened is a fact about the outside world, and the caller
hanging up does not un-send an email.

This is strand path 4 above, fixed at the source rather than repaired
afterwards — and it is what keeps the sweep's duplicate risk rare rather than
routine, because the worst case is a message that WAS delivered whose success
was never written, which the sweep would then reasonably re-send.

Now `context.WithoutCancel` with a 10s bound, and the tenant carried over
explicitly: the store reads the tenant from the context, so a bare
`context.Background()` would have none and be refused by row-level security.
Same shape as authorization-svc's `internal/siem` fix, which was the identical
mistake on a different path.

## Verified

- `go build` / `go vet` clean; `gofmt` clean on every file touched.
- `go test ./...` green, including the store integration tests against real
  PostgreSQL 16 via `notification_test`.
- 9 worker tests (`internal/retry/stranded_test.go`) and 10 store integration
  tests (`internal/store/stranded_test.go`), covering the threshold being in
  the past, zero and negative disabling the sweep, concluded and already-scheduled
  rows being left alone, a fresh in-flight row being refused, single-claim
  under contention, cross-tenant refusal, and the attempt count surviving.
- **Negative control recorded:** replacing the COALESCE clock with
  `last_attempt_at` alone fails exactly the never-attempted subtests and
  nothing else.
- **Proven end to end on the real stack.** Rebuilt, restarted, and the first
  tick reclaimed all five stranded rows and delivered all five:
  `"stranded deliveries reclaimed","count":5,"found":5` followed by five
  `"delivery succeeded on re-attempt"`. Confirmed in the register (zero PENDING
  rows remain) and in the mail catcher (five messages). Six days late, but
  delivered.
- Send path driven over HTTP with a full envelope, after seeding a
  `NOTIFICATION_SEND` grant in authorization-svc: IN_APP send → SENT with no
  schedule, the same `correlation_id` replaying to 200 and the *same*
  notification id, read state, unread count, 401 without a principal, and 400
  for both an unknown channel and the withdrawn SMS one.

## Measured, and left alone deliberately

**000002's `NOT VALID` constraints can now be validated.** Counted zero
violations across all six checkable constraints; the `PIGEON` row the previous
known-gaps entry described is gone, and the one remaining SMS row is legitimate
history that `channel_known` permits. **Not turned into a migration:**
`VALIDATE CONSTRAINT` scans and fails outright on any violating row, so a
validating migration would hard-fail on deploy against any environment whose
backlog is *not* clean — taking the service down to gain nothing, since
`NOT VALID` already enforces every new write. It stays a per-environment
operator step.

## Still open

- **Nothing on the estate sends a notification.** The largest gap, and none of
  it is in this service. Swept every non-test Go file and every deployment
  manifest for any env var matching `NOTIF`: the only hits are this service's
  own config, its DB role, and the two authz action codes it checks. There is
  no notification client in any of the other 103 services and no
  `NOTIFICATION_SERVICE_URL` anywhere. §9.7 gives this service "workflows,
  deadlines, escalations, approvals, and status changes"; none of those produce
  a notification today, so every notice the platform has sent was sent by hand.
  **Deliberately not fixed:** which service notifies whom, on what event, from
  which template is a product decision per workflow, and picking one to wire
  would be inventing that decision rather than encoding it — the same line
  tracker row 82e drew between an ABAC engine and an ABAC rule. The path is
  proven, so adoption is wiring rather than discovery. Tracker row 97c.
- **The register grows without bound.** One row per notification forever,
  carrying every notice's subject, body and recipient address. No retention,
  partitioning or archival. Volume is currently trivial (35 rows), and the rows
  ARE the evidence of what was sent to whom, so a purge is a governance
  decision rather than a cleanup. Same shape as `access_decision_log` before
  authorization-svc's migration 000009, and probably the same answer — monthly
  partitions with DETACH, not DELETE — once volume justifies it. The
  data-minimisation question (how long a notice's BODY should be kept) needs a
  human before the mechanism does. Tracker row 97d.
- **WEBHOOK is accepted by the handler and refused by the deliverer**, by
  design: ZS-SVC-Y-001 §1.3 puts machine-to-machine delivery outside this
  service's remit. Worth knowing that a WEBHOOK send therefore stores a FAILED
  row with that as the stated reason.
- **Test fixtures left in place:** the `NOTIFIER` role, its permission bundle
  and its assignment in authorization-svc, plus one IN_APP notification. All
  additive — none of them denies anything — and they make the send path
  testable, which it was not before.
