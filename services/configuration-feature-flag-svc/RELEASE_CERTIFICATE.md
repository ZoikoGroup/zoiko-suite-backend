# Release Certificate — configuration-feature-flag-svc

**Date:** 2026-09-22
**Scope:** the service and the console surface that drives it, end to end.
**Result:** `scripts/audit.sh` — **146 checks, 0 failures (100%)**, against a
running stack.

This certifies what was verified, against what, and what was deliberately not
done.

---

## What this service is, and why the gaps mattered

Runtime configuration values and environment-aware feature flags. Doc 03 §9.6,
which is the entire architecture-doc entry for it:

> Owns runtime configuration, rollout controls, and environment-aware feature
> flags.

It is **not** one of the seven Governance Control Plane engines and is not on
the non-bypassable governance path. Its critical constraint — "configuration may
tune service behavior, but must never be used to bypass governance doctrine" —
is satisfied by scope rather than by code here: this service only stores and
serves values, and never calls Policy, Authorization or Evidence Requirements to
make a decision.

The code was in reasonable shape before this pass: 36 tests, append-only
versioning with a partial unique index as a real concurrency backstop, RLS with
`FORCE` and `WITH CHECK` and a correct `NULLIF` guard on the session GUC, a
fail-closed authorization gate, a size-capped body reader, and a console that
already spoke plain English. Three earlier defects had already been found and
fixed — a body `tenant_id` that let a caller overwrite another tenant's values, a
list route that returned every tenant's configuration to any caller, and a lost
insert race reported as `503 store_unavailable`.

What remained were four defects of one family, and the family is the dangerous
one on a service like this: **each produced an entirely ordinary-looking
response, and the worst of them produced no response at all.**

### 1. A recorded change could be silently lost — and the loss was undetectable

`config.updated` and `feature_flag.updated` were published from the handler
*after* the store had already committed, with the error logged and discarded:

```go
if pubErr := h.publisher.PublishConfigUpdated(...); pubErr != nil {
    h.log.Error("failed to publish config.updated", ...)
}
writeJSON(w, http.StatusCreated, entry)
```

So a broker hiccup during a write left the new version correctly recorded, the
operator correctly told the change was saved, and **every consumer still reading
the value it superseded.**

On most services that is a tolerable trade. On this one it is the worst possible
failure shape, because of what the consumer is holding: not corrupt data, not
stale-looking data — a *perfectly valid configuration value that is simply no
longer the one in force*. No request fails. No latency moves. Nothing downstream
can detect it, because there is nothing to detect. The rollout percentage a
service enforces silently stops matching the one this console displays, and the
only way anyone finds out is by noticing that a feature never actually turned
on.

**Fixed** by a transactional outbox (migration `000003`). The event is written in
the *same transaction* as the version row, so a change that was recorded always
has its event and an event that exists always has its change. Delivery became a
separate, retryable problem owned by `internal/outbox` and made visible by four
metrics and three alerts. Because the enqueue shares the transaction, a failure
to record the event now **fails the write** — refusing a change nobody can be
told about is better than recording one silently.

### 2. Anyone who could set their own value could set everyone's

A config entry or flag written with no `tenant_id` is the **environment-wide
default**: it applies to every tenant that has not set its own value. Writing
one and writing your own organisation's value are not the same act, and both
authorized the same action:

```go
if !h.authorize(w, r, principalID, "", ActionConfigWrite) { return }
```

So a principal provisioned to manage one organisation's settings could change
what every *other* organisation reads, and the refusal that should have stopped
it never ran. RLS cannot catch this either — migration `000002`'s `WITH CHECK`
admits `tenant_id IS NULL` unconditionally, because a global row genuinely
belongs to no tenant.

**Fixed** by splitting the action on the scope being written:
`CONFIGURATION_GLOBAL_WRITE` and `FEATURE_FLAG_GLOBAL_WRITE` alongside the
originals, seeded into `CONFIG_FULL` in `seed-demo-rbac.ps1`. That seeding is not
a detail: an action name nothing in the estate grants refuses every write `403`
while every read works, which reads as an under-granted operator rather than as
a service asking for a name nobody defines — exactly how `access-control-svc`
shipped. The audit asserts both halves, that the service defines each action
*and* that an active bundle grants it.

The handler also had to be reordered: the scope lives in the body, so the body is
now decoded before the authorization call rather than after it.

### 3. Readiness was green while 100% of writes failed

Every write calls `authorization-svc` and fails closed. `/readyz` pinged only the
database, so with `authorization-svc` unreachable the container reported ready,
the orchestrator kept routing to it, and every write answered `503` — while every
read kept working perfectly.

That combination is what makes this outage so hard to place: the page loads, every
value displays, and only changes fail. It reads as a broken console.

**Fixed.** `/readyz` now names each component individually, because the two have
opposite remedies — a dead pool is this service's problem, an unreachable
`authorization-svc` is somebody else's and restarting this one achieves nothing.

### 4. None of the above was measurable

There were no domain metrics at all. Every interesting failure here is an
ordinary-looking response: a denial is a `403` like any other, an unobtainable
decision is a `503` that reads like a blip, and an idempotent no-op is a `200`
indistinguishable from a read. `http_requests_total` cannot tell any of them
apart.

**Fixed** with eight series, every label value pre-created at zero — a series
that has never been observed and one reading zero are indistinguishable to an
alert expression, so a rule written to catch the *first* occurrence of something
would otherwise stay silent through exactly the event it exists for. Seven alert
rules read them, and the audit asserts that every series an alert names actually
appears on `/metrics` and that every RUNBOOK section an alert cites exists.

---

## Smaller things found and fixed

- **`AUTHZ_PLATFORM_SCOPE_ID` defaulted to empty and was never validated.** Every
  write authorizes against it as the `legal_entity_id`, and `authorization-svc`
  rejects an empty one outright — so an unset variable did not disable the check,
  it failed every write with an error that reads as an outage. Now fatal at
  startup outside `local`.

- **The store test suite left the schema broken behind it.** Two tests reach the
  store-unavailable path by dropping a table, with no teardown. This service's
  own `progress.md` records a live demo losing `feature_flags` to exactly that,
  filed as a "lesson for next time" — so the lesson was written down and the
  teardown was not, and the next run would have done it again. Now restored in
  `t.Cleanup`, and the audit additionally refuses to run the suite against the
  serving database.

- **The event partition key was the correlation id.** Two changes to the same
  entry therefore landed on different partitions whenever they arrived on
  different requests, so a consumer replaying them could apply an older value
  after a newer one and serve the superseded value permanently. Now the aggregate
  id.

- **`scope_is_global` added to both event payloads.** A consumer had to infer
  scope from a null `tenant_id`, and "this changed for one organisation" versus
  "the default every organisation reads has changed" are very different facts —
  a consumer reading a missing tenant as "unknown" rather than "all" silently
  ignores the wider one.

- **Three gated-route tests asserted nothing about their payloads.** Their
  fixtures named `config_key`, `flag_key` and `updated_by_principal_id` — fields
  this service has never accepted — and passed anyway, because authorization ran
  before the body was parsed. Reordering the handler surfaced them.

- **`internal/authz/client.go` had two imports at the head of the stdlib block**,
  left by an earlier codemod, so `gofmt` had been failing on it silently.

- **Migration `000003` was not re-runnable** in its first draft: `CREATE TABLE IF
  NOT EXISTS` beside an unguarded `CREATE POLICY`, so a half-applied migration
  would have had to be unpicked by hand. Caught by the test harness re-applying
  migrations per test.

---

## What was verified, and against what

`scripts/audit.sh`, against the running stack — real Postgres, real Kafka, real
`authorization-svc`, the real console.

| Section | Checks |
| --- | --- |
| 1. Static analysis | `go build`, `go vet`, line-ending-independent `gofmt` |
| 2. Test suites | 81 tests, 0 failures, **0 skipped** — and asserted isolated from the serving database |
| 3. Live health | `/healthz`, `/readyz`, `/metrics`; readiness names both components |
| 4. Envelope | ZS-ARCH-SVC-001 §4 refusals, with structured violations |
| 5. Route surface | every route in both directions, every emitted error code documented |
| 6. Event contract | both events in asyncapi, in the publisher, and in the outbox CHECK constraint; partition keys; no direct publish from the handler |
| 7. Authorization | all four action names defined **and** granted by an active bundle |
| 8–10. Live behaviour | 201 vs 200, end-dating, one-effective-row, validation, cross-tenant refusal, exact-tuple 404 |
| 11. Outbox | enqueued on transition, **not** on a no-op, owned by the caller, drained, and present on Kafka |
| 12. Telemetry | every series, every pre-created label value |
| 13. Alerts | 7 rules; every series exists on `/metrics`; every RUNBOOK section exists |
| 14. Artifacts | openapi, asyncapi, RUNBOOK, this file, progress, context, audit |
| 15. Migrations & RLS | `FORCE` and `WITH CHECK` on all three tables, read from `pg_policy`; the empty-GUC guard; the relay's named exemption; every up has a down |
| 16. Frontend | typecheck, client routes, ten explained refusals, the 201/200 distinction, 16 e2e specs |

**Note on the store suite.** It runs as a purpose-created `NOSUPERUSER
NOBYPASSRLS` role. `TEST_DATABASE_URL` normally points at `postgres`, a
superuser, and a superuser bypasses row-level security unconditionally — `FORCE`
included — so an isolation assertion made over that connection would prove only
that the application predicate works, never that the policy does.

---

## Deliberately not done

- **No fallback from a tenant-specific miss to the global default.** The
  single-key `GET`s match the tuple exactly. This is the service's documented
  behaviour and the console explains it, but it is the single most misread
  response here, so it is called out rather than quietly changed. Adding
  inheritance would be a deliberate feature, not a fix.

- **No caller-supplied idempotency key on the write path.** Idempotency is by
  value equality. Adding a key later would be a breaking API change.

- **The relay is at-least-once, not exactly-once.** Chosen: a duplicate makes a
  consumer re-read a value it already has; a lost one leaves it acting on a value
  this service has already replaced.

- **No consumer of this service's events exists yet in the estate.** The outbox,
  the topic and the payloads are verified end to end onto Kafka, but nothing
  downstream consumes them, so the *consumer* half of the contract is unproven by
  construction. The critical constraint in §9.6 has to be re-checked in review
  the first time a real consumer appears — this service can stop itself being a
  governance backdoor, but it cannot stop a careless consumer wiring a flag as
  one.

- **The four `CONFIG_FULL` actions are seeded in one bundle**, because this seeds
  a development stack. In a real deployment "change my organisation's settings"
  and "change the default for everyone" are the pair you would want held by
  different people — the split now makes that possible, and the audit proves both
  are granted rather than assuming it.

---

## Re-proving this

```bash
cd services/configuration-feature-flag-svc
bash scripts/audit.sh              # full, including the console
SKIP_FE=1 bash scripts/audit.sh    # backend only
```

Exits non-zero if anything fails.
