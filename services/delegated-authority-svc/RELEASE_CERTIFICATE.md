# Release Certificate — delegated-authority-svc

**Date:** 2026-09-22
**Scope:** the service and the console surface that drives it, end to end.
**Result:** `scripts/audit.sh` — **122 checks, 0 failures (100%)**, against a
running stack.

This certifies what was verified, against what, and what was deliberately not
done.

---

## What this service is, and why the gap mattered

The register of who may act for whom. Doc 03 §9.3: "maintains time-bound,
scope-bound, approval-bound delegated authority chains", under one hard
constraint — **delegated authority must never exceed the delegator's own
authority**.

The state it was found in had one serious defect, and it is the kind this
service is least able to survive: **expiry depended on somebody looking.**

`ExpireDue` ran only from the three HTTP handlers, and it was scoped to the
tenant of whatever request happened to be passing through. Two consequences,
the second of which is the real one:

1. `expired_at` was set to `now()` — the instant of the sweep. A grant whose
   window closed on a Friday and whose register was next read on Monday
   recorded, in evidence, that its authority ended Monday morning. Doc 04 §6.3
   requires these records to stand as evidence; one that misdates the end of an
   authority by a weekend is worse than absent, because nothing marks it as
   approximate and `authority.expired` carried the same wrong timestamp to
   every consumer.

2. **A tenant whose register nobody opened expired nothing at all.** Not late —
   never. Its grants stayed `ACTIVE` past their window indefinitely, and
   because `authority.expired` is what identity-context-svc acts on to end the
   delegate's session, that session was never ended either. The tenant least
   likely to be watched was the one whose lapsed authority persisted longest,
   and the whole failure produced no 5xx, no latency and no log line.

The rest of the code was in good shape — 52 tests, a correct fail-closed
posture, the escalation refusals (`delegator_mismatch`, `self_dealing`) already
closed, malformed ids already answering 404 rather than 503. What was missing
was the enforcement of time, and every artifact by which anyone other than the
author could check any of it.

---

## Verification performed

Every figure below came from a run on 2026-09-22.

| Check | Result |
|---|---|
| `go build ./...` | clean |
| `go vet ./...` | clean |
| `gofmt` (line-ending independent) | clean |
| `go test ./...` | **75 tests**, 0 failing, 0 skipped (was 52) |
| Packages passing | 6 |
| Console `tsc --noEmit` | clean |
| Playwright `e2e/delegations.spec.ts` | **9 passing** |
| `scripts/audit.sh` | **122 checks, 0 failures** |

### Live, against the running stack

- Service on `:8136`, `/readyz` reporting both `database` and
  `authorization-svc` — the latter is a readiness dependency, not merely a
  runtime one, because every route calls it and this service fails closed.
- **The expiry fix was proved, not asserted.** A grant was seeded directly into
  a tenant that nothing in the test reads, with a window that had already
  closed. The background sweeper ended it with no request ever made against
  that tenant; `expired_at` came back equal to `effective_to` with **0 seconds
  of drift**; and `authority.expired` was found enqueued in the outbox. That is
  the check that would have caught the original defect — everything else about
  expiry is structural.
- Store suite run against a **`NOSUPERUSER NOBYPASSRLS`** role on a scratch
  database, so the row-level-security assertions were made against a database
  that was actually enforcing it.
- Live RLS policies read back from `pg_policy` rather than from the migration
  files, so a hand-edit or a down migration cannot pass this.

---

## Contract surface

| Route | Purpose |
|---|---|
| `POST /v1/delegations/` | Grant. Idempotent on `(tenant_id, correlation_id)`. |
| `GET /v1/delegations/` | Register read, in one of exactly two scopes. |
| `GET /v1/delegations/{id}` | One grant. |
| `POST /v1/delegations/{id}/revoke` | Terminal revocation. |
| `/healthz`, `/readyz`, `/metrics` | Operational. |

`openapi.yaml` documents all of them, and the audit checks the agreement in
both directions: every route the spec declares exists, and **every
`error_code` the handler can emit appears in the spec's enum** — 16 of them, so
a client branching on the code cannot meet one the contract never mentioned.

`asyncapi.yaml` documents the three §9.3 events. The audit asserts against the
code that `authority.expired` carries **no `actor_id`** — a lapse has no actor,
and naming the principal who happened to observe it would attribute an act to
someone who did not perform one, on a register whose entire purpose is
recording who did what.

---

## Defects found and fixed

| # | Defect | Pinned by |
|---|---|---|
| 1 | **Expiry waited for a reader.** Tenant-scoped, read-triggered; a tenant nobody read expired nothing, indefinitely, and `authority.expired` was never published to end the delegate's session. | audit §9, §10 (live), `TestExpireDueAllTenantsCrossesTenantBoundaries` |
| 2 | **`expired_at` recorded the observation, not the event.** Set to `now()` at sweep time rather than to `effective_to`, misdating the end of an authority by however long the gap was. Migration `000004` backfills the affected rows from a column that was always present in the same row. | `TestExpireDueRecordsWhenAuthorityEndedNotWhenObserved`; audit §10 asserts 0s drift live |
| 3 | **The store suite skipped silently** when `TEST_DATABASE_URL` was unset — reporting `ok` having verified nothing. Now fails under `CI` or `REQUIRE_DB_TESTS`. | audit §2 asserts 0 skipped |
| 4 | **The isolation tests could pass as a superuser.** Postgres exempts a superuser from row-level security unconditionally, and `FORCE ROW LEVEL SECURITY` forces it for the table *owner*, not for a superuser — so `TestCrossTenantIsolation` would have passed with every policy dropped. The suite now refuses to run as one and names the role to use. | `requireNotSuperuser` |
| 5 | **No scrape job and no alert rules**, on a service where every interesting failure produces no 5xx and no latency. Six alerts added, each checked to read a series that exists and to point at a runbook section that exists. | audit §13 |
| 6 | **None of the four release artifacts existed** — no `openapi.yaml`, `asyncapi.yaml`, `RUNBOOK.md` or `RELEASE_CERTIFICATE.md`, and no audit script. The service was improved, committed and unverifiable by anyone but its author. | audit §14 |

### Two defects in the audit script itself

Recorded because an audit that reports a false failure is worse than one that
does not run: it points at working code and costs someone a morning.

- Every `python` helper emitted **CRLF**. CR is not IFS whitespace, so each
  token the shell read back carried a trailing `\r` and every `grep` for it
  failed — five FAILs on checks whose subject was present in the very output
  being searched. The queries now live in `scripts/spec_query.py`, which writes
  LF only.
- The audit was invoked through `Q="python $SVC/scripts/spec_query.py"`, and
  `$SVC` contains a space on this machine. Unquoted expansion word-split the
  path and failed **fifteen** contract checks at once, all of them reporting
  the contract as missing content it plainly had. `Q` is now a function.

A third, smaller: the route-count grep matched `r.Header.Get("` — `"Heade"` +
`"r.Get("` — and reported a fifth route that does not exist.

---

## Artifacts added

- `internal/expiry/` — the background sweeper, with a lateness histogram
  measured from each grant's own `effective_to`. That metric is the one that
  distinguishes a sweeper doing its job from one expiring plenty of grants
  hours late, which looks identical on every other signal.
- `deployments/migrations/000004_expiry_sweeper.{up,down}.sql` — the
  cross-tenant RLS exemption, the index the sweep runs on, and the backfill.
- `openapi.yaml`, `asyncapi.yaml`, `RUNBOOK.md`, `progress.md`.
- `scripts/audit.sh` (122 checks) and `scripts/spec_query.py`.
- Four new domain metrics and six Prometheus alert rules; a scrape job in
  `deployments/prometheus.yml`.
- 23 new tests (52 → 75), including the first tests this service has had for
  the sweeper and for the cross-tenant sweep.

### On the console surface

The console page, server actions, API client and 9 Playwright specs already
existed and were in good shape. What this pass corrected was a **stale contract
comment**: `lib/api/delegations.ts` documented expiry as lazy and read-triggered
— accurate when written, wrong the moment the sweeper landed, and exactly the
kind of documentation the next reader trusts. The audit now asserts both that
the client documents enforced expiry *and* that it no longer claims the old
behaviour, so this cannot drift back silently.

---

## Not done, and deliberately

- **No workflow approval step.** §9.3 says "approval-bound"; what is bound here
  is *authorization*. No route submits a delegation to workflow-svc for a
  second human. Building one means choosing which action types need approval
  and at what seniority — a governance decision, not this service's. **This is
  the one §9.3 clause not fully discharged**, and it is the reason this
  certificate does not claim the spec is complete, only that everything
  claimed is verified.

- **No durable evidence row for refusals this service decides alone.** Doc 04
  §6.3 says denials matter evidentially. Refusals that reach authorization-svc
  are recorded there; `self_dealing` and `delegator_mismatch` — precisely the
  escalation attempts — exist only as a metric and a log line. Those are the
  refusals most worth having in evidence.

- **`Idempotency-Key` is required but not used to deduplicate.** Replay
  protection comes from `(tenant_id, correlation_id)`, a different key. A retry
  with a fresh correlation id and a repeated idempotency key creates a second
  grant. Same gap as tenant-entity-registry-svc.

- **Expiry cannot be switched off**, and an unparseable `EXPIRY_SWEEP_INTERVAL`
  falls back to the default rather than disabling the sweeper. Given what the
  missing sweep did, a config typo must not be able to reproduce it.

- **A second delegation surface exists elsewhere in the estate.**
  authorization-svc serves `/v1/admin/delegated-authorities`, rendered at
  `/admin/access-control`, separately from this service's `/v1/delegations/` at
  `/admin/delegations`. Doc 03 §9.3 and a comment in `docker-compose.yml` both
  make *this* service the authoritative owner of the concept. Reconciling them
  is a breaking change to another service's console and needs a team decision;
  it was not made unilaterally.

- **The alert rules have not been driven to `firing`.** They are asserted to
  parse, to read series that exist, and to point at runbook sections that
  exist — but unlike the GOV-01 certification, no alert was pushed through
  `inactive → pending → firing` against real data.

---

## One environment note

`deployments/init-db.sh` applies migrations **only on a fresh Postgres volume**
— the script documents this itself. Migrations `000003` (the outbox) and
`000004` therefore do not reach an existing stack on `docker compose up`, and
the symptom is the relay logging `relation "delegation_outbox" does not exist`
every 250ms while the service reports healthy and ready. Both were applied by
hand for this run. Anything gating on this service in an existing environment
must apply them explicitly.

---

## Re-proving this

```bash
cd services/delegated-authority-svc && bash scripts/audit.sh
```

Needs the service on `:8136`, `zoiko-postgres`, authorization-svc on `:8089`, a
Go toolchain, and the console's `node_modules` for §16 (`SKIP_FE=1` skips it,
leaving 102 checks). `TEST_DATABASE_URL` must point at a **scratch** database
owned by a **`NOSUPERUSER NOBYPASSRLS`** role.

Exits non-zero on any failure, so CI can gate on it.
