# notification-svc — Progress

## Status: 5 Oct 2026 re-audit — six defects fixed and proven live

The 30 Sep audit scored 37 API and table rows. The re-audit also read the spec's NP and INV
matrices and the legacy code beside the plane. The NCD plane held up. The legacy surface did not.
All six defects are fixed. Store suite 128 pass, 1 skip (the legacy outbox RLS test refuses to run as superuser), 0 fail
against Postgres 16, NCD tests as the
`NOSUPERUSER NOBYPASSRLS` role. `scripts/ncd_live_check.py` 69/69 on three consecutive runs,
including a real marketing round-trip through Mailpit.

| # | Defect (as found) | Fix |
|---|---|---|
| 1 | `POST /v1/notifications/unsubscribe` was envelope-exempt and never verified its token. An anonymous `{tenant_id, email}` wrote an UNSUBSCRIBE. The legacy upsert (`ON CONFLICT DO UPDATE reason`) then rewrote a recorded HARD_BOUNCE as UNSUBSCRIBE, which the gate scopes to marketing, so transactional and security mail resumed to a dead address. **Proven live.** | `internal/unsubscribe`: AES-256-GCM token over (tenant, address), keyed by `NOTIFICATION_UNSUBSCRIBE_SECRET`. The receiver believes only the token: forged gets 403, absent gets 400, unconfigured gets 503. The upsert never weakens a reason (rank UNSUBSCRIBE < COMPLAINT < HARD_BOUNCE < ADMIN_SUPPRESSED). Negative control recorded. |
| 2 | `DELETE /v1/notifications/suppression/{email}` hard-deleted a suppression: one principal, no evidence. | Route and store method removed. Migration `000022` adds lift columns with the canonical CHECKs (evidence, plus a second principal for bounce/complaint/hold), refuses DELETE by trigger, and makes the uniqueness active-rows-only so a lifted row stays as history. `POST /v1/suppressions/{id}/lift` now reaches legacy rows. |
| 3 | The housekeeping worker deleted concluded `message_intents` older than 90 days. The FK cascade took renders, attempts and delivery events with them, with no legal-hold check (§8.4, §9.2, INV-28). | Purge step, option, stat and store methods removed. Migration `000023` makes all six evidence tables refuse DELETE. Retention is DRC's. Negative control recorded. |
| 4 | Marketing mail from the plane carried no unsubscribe link. The legacy header hard-coded `notify.zoiko.com` with the raw address in the URL, plus a `mailto:` nobody reads. | Both paths add the sealed RFC 8058 header, with its base from `NOTIFICATION_PUBLIC_BASE_URL`. Marketing that cannot carry a working link is refused before the provider (NCD-011 / `ErrUnsubscribeUnavailable`), never sent without one (INV-25). |
| 5 | The plane exported no metrics. No §13.1 dimension was observable. | `telemetry.NCD` through an `ncd.Metrics` port. Counters only where nothing can roll back: submits (state, latency) and callbacks (`rejected_<code>`, applied, duplicate…). Backlog gauges re-read from the DB every 15 s under platform scope: UNKNOWN count and age, queue depth and age, notices past deadline, pending DRC declarations. Label allow-list test (§13.3). The RLS negative control showed zeros. |
| 6 | No DKIM, and no reaction to broken sender authentication (§11.1, NP-55). | `internal/senderauth`: DKIM relaxed/relaxed signing (go-msgauth), covering List-Unsubscribe(-Post) per RFC 8058 §4. A monitor checks the selector key, DMARC and SPF. A definite break holds email as retryable before the relay. Resolver timeouts change nothing. Enabled by `NOTIFICATION_DKIM_DOMAIN/_SELECTOR/_PRIVATE_KEY`. **Proven live**: email went to RETRY_SCHEDULED with the NP-55 reason and Mailpit was unchanged. |

The live check also changed. It raced the worker (it read the attempt while still SUBMITTING;
53/57 on one run), its README and stand-ins were never committed, and it covered none of the
above. It now waits for a concluded attempt and adds 12 checks. `scripts/README-live-check.md`
and `scripts/live_check_stubs.py` make it reproducible.

**Deploy notes.** Apply `000022` and `000023`. Set `NOTIFICATION_UNSUBSCRIBE_SECRET` (32+ bytes)
and `NOTIFICATION_PUBLIC_BASE_URL` from the secret store, otherwise marketing email is refused.
Set the DKIM trio only where this service, not the provider, signs.

**Still open (not defects in this service):** nothing in the estate calls notification-svc
(INV-01 adoption). The console uses only the 6 legacy routes, with no NCD screens and no
acknowledgment UI. PRV and DRC do not exist. `ncd_exceptions` has no platform-read policy, so
open exceptions are not in the backlog gauges. 62 pre-existing files are not gofmt-clean (CI
does not check).

---

## Earlier status: ZS-SVC-Y-001 control plane implemented and verified live (2026-09-30)

The five canonical NCD services now live in this one service (`internal/ncd`, migrations
`000015`–`000021`), the way configuration-feature-flag-svc implemented AA-001. Group 1 audit 7/9
went from 12% to **95%** (35 of 37 rows; the two partials wait on PRV and DRC, which do not exist).
The full scoring, row by row, is in `docs/audit_files/Identity, Scope & Foundation-audit-2026-09-23.md`.

**Re-prove it:** `python scripts/ncd_live_check.py` against a running service (57 checks; header of
the script says what it needs), and the store suite with `TEST_DATABASE_URL` pointing at
`notification_test` — it resets the schema, applies every migration and connects as the
unprivileged `zoiko_ncd_app` role, because a superuser bypasses RLS and would hide exactly the
defects RLS exists to catch. Regenerate the contract with `python scripts/gen_openapi.py`.

**Shape.** `internal/ncd` is pure policy over two ports: `Store.InTx` hands out a tenant-scoped
`Tx` whose `Enqueue` is the only way to emit an event (so no fact is announced outside the
transaction that records it), and `Transport` submits through a certified binding (SMTP router,
or the in-app register). Handlers in `internal/handler/ncd_handler.go` fetch, authorize against the
stored object's legal entity, then act. The worker (`Service.Run`) moves jobs, turns stranded
SUBMITTING attempts into UNKNOWN, raises UNKNOWN past its deadline as an exception, runs notice
clocks and evaluates reputation; dispatch kicks it.

**The rules that are database rules, not conventions:** template and intent content immutable
(triggers); the §6.2 attempt graph (trigger); no new attempt of a communication while one is
UNKNOWN (trigger — INV-13 cannot be forgotten by any code path); evidence, plans, decisions and
acknowledgments append-only; suppressions never deleted, governed lifts need a second principal
(CHECK); SoD on intent activation and template approval (CHECK).

**Defects found on the way, all fixed:** the legacy send path consulted no suppression list at all
(`GatedDeliverer` now gates handler, resend and retry worker); the legacy provider webhook was
unauthenticated (HMAC now); the live `notification` DB was on the pre-merge migration lineage
(empty — rebuilt to 000021); `Idempotency-Key` was demanded and never read (middleware now); the dev
RBAC seed granted none of the four other actions this service authorizes; `openapi.yaml` and the
audit script were lost in the merge; and, in the new code, callback dedupe keyed without the tenant.

**§3.3 on the legacy register.** `status` in every API response is now the precise proposition
(`PROVIDER_ACCEPTED`, `DELIVERED_TO_INBOX`, `DELIVERY_UNKNOWN`, `RETRY_SCHEDULED`, …); the column is
unchanged and returned as `stored_status`; `?status=` accepts both vocabularies; the console and its
e2e mock follow.

**Still open:** the legacy `POST /v1/notifications` and `/events/ingest` paths remain for existing
callers and name no intent — gated and §3.3-correct, but INV-01 holds only once callers move to
`POST /v1/communications`. Provider callbacks for SMTP need a real adapter posting to
`/v1/provider-events/smtp-primary` with `NCD_CALLBACK_SECRET_SMTP_PRIMARY`; the legacy webhook needs
`NOTIFICATION_WEBHOOK_SECRET[_<PROVIDER>]` or it refuses everything (by design). `docker-compose.yml`
sets development defaults for both; a real deployment must set them from the secret store.

---

## Earlier status: complete and working, with no callers (2026-09-08)

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
