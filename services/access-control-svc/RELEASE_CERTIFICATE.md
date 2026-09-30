# Release Certificate — access-control-svc

**Date:** 2026-09-22
**Scope:** the service and the console surface that drives it, end to end.
**Result:** `scripts/audit.sh` — **158 checks, 0 failures (100%)**, against a
running stack.

This certifies what was verified, against what, and what was deliberately not
done.

---

## What this service is, and why the gaps mattered

The register of what roles exist and what each one permits. Doc 03 §9.4:
"maintains role catalogues, permission bundles, and policy-linked access
groupings."

It is not a shadow copy of authorization-svc's RBAC. It is the governed
**authoring** layer in front of that service's otherwise unguarded admin API:
creating a role or bundle here makes a real synchronous call into it, so a
definition recorded here has actually been provisioned for enforcement. Every
write provisions before it records and fails closed, which is why a 503 from
here always means *nothing was written*.

The code was in decent shape — 33 tests, a correct fail-closed posture, real
RLS with `FORCE` and `WITH CHECK`, a bounded decision cache with a documented
rationale. What it had were four defects of the same family, and the family is
the dangerous one: **each produced an entirely ordinary-looking response.**

### 1. Every write was refused 403

The handler asked authorization-svc for `ACCESS_ROLE_MANAGE`. That name exists
nowhere else in this estate. The seed grants `ROLE_MANAGE`; the console's own
copy tells the operator a 403 means "you hold no ROLE_MANAGE grant"; the live
bundle grants `ROLE_MANAGE`. The service was the only party asking for the
other name.

So every role and bundle write on this service was refused while every read
worked — which presents as an under-granted operator, not as a service asking
for an action nobody defines. authorization-svc's own decision log had both
sides of it, eleven hours apart:

```
2026-09-21 10:21:46  ROLE_MANAGE         GRANTED  rbac:role=CONSOLE_DEMO_OPERATOR
2026-09-22 06:17:59  ACCESS_ROLE_MANAGE  DENIED   no_grant
```

### 2. `role.updated` could never revoke a session

identity-context-svc revokes the sessions of everyone holding a role when that
role changes, because a role's permission bundles are frozen into the session
envelope at resolve time. Its handler reads `payload.role_id`. This service
emitted `role_definition_id` and nothing else, so that field was always empty
and every event was dropped with `role.updated names no role_id — cannot
revoke`.

Retiring a role therefore reached authorization-svc's `active_flag` — new
authorize calls denied, correctly — while every session already holding the
role kept every action it granted. The console's copy says a retired role
"grants nothing to anyone from that moment". For the sessions that mattered
most, it granted everything until they expired.

### 3. The events were fire-and-forget

All three were written to Kafka from the handler *after* the commit, with the
error logged and discarded. A broker hiccup during a retirement threw away the
only notice that the role had changed, while the register showed RETIRED and
the operator was told it had worked.

### 4. Two bundles could share a code on one role

authorization-svc identifies a bundle by `(role_id, bundle_code)` and its
attach endpoint is an upsert-**replace** on that pair. Nothing here enforced
the same key, so a second bundle with the same code silently replaced the
first's permitted actions there — both rows staying ACTIVE here, listing
different actions — and detaching either retired the single remote bundle they
shared, leaving the other displayed as ACTIVE while granting nothing.

Alongside those: a duplicate role code surfaced as `503 store_unavailable`
**with the raw Postgres SQLSTATE in the response body**, after the role had
already been provisioned remotely; a malformed id did the same; a replay
answered 201 like a create, so the console's `replayed` branch had never once
been reachable; readiness could not see authorization-svc; and there was no
domain telemetry, no alerts, no contract documents and no console test at all.

---

## Verification performed

Every figure below came from a run on 2026-09-22.

| Check | Result |
|---|---|
| `go build ./...` | clean |
| `go vet ./...` | clean |
| `gofmt` (line-ending independent) | clean |
| `go test ./...` | **53 tests**, 0 failing, 0 skipped (was 33) |
| `go test -tags=integration ./internal/store/` | **16 tests**, 0 failing, 0 skipped (was 10) |
| Console `tsc --noEmit` | clean |
| Playwright `e2e/access-control.spec.ts` | **14 passing** (was 0) |
| Playwright, whole console suite | **58 passing** |
| `scripts/audit.sh` | **158 checks, 0 failures** |

### Live, against the running stack

- Service on `:8137`, `/readyz` reporting both `database` and
  `authorization-svc`. The latter is a readiness dependency, not merely a
  runtime one: every write calls it twice and this service fails closed, so a
  green probe over a dead authorization-svc means 100% of writes answer 503
  while the load balancer keeps sending traffic.
- **The provisioning was proved, not asserted.** A role was created through the
  API and read back out of `authorization_svc.roles`; a bundle was attached and
  read back out of `authorization_svc.permission_bundles`; the role was retired
  and `active_flag` came back **false**; it was reactivated and came back true
  again. That is the property this service exists for, checked against the
  other service's own tables.
- **The outbox was proved end to end.** The event row was found in
  `event_outbox` in the transaction that made the change, with `role_id` in its
  payload; the relay published it; it was consumed back off
  `zoiko.access-control.events`; and **authorization-svc was observed
  consuming it** and logging `grant-graph event: cached grants invalidated`.
- A refused duplicate create was checked to have provisioned **no orphan** in
  authorization-svc — `SELECT count(*) FROM roles WHERE role_code = …` is 1,
  not 2.
- Live RLS read back from `pg_policy` rather than from the migration file: all
  three tables `FORCE`, all three policies carrying `WITH CHECK`, and the
  outbox relay admitted by a named `app.outbox_relay` disjunct rather than by
  running unscoped.
- Store suite run against a **`NOSUPERUSER NOBYPASSRLS`** role on embedded
  Postgres, so the isolation assertions were made against a database that was
  actually enforcing the policies. A superuser bypasses RLS unconditionally and
  would let every one of them pass with every policy dropped.

### Audit coverage, by section

| Section | Checks |
|---|---|
| 1. Static analysis | 3 |
| 2. Test suites | 6 |
| 3. Live health | 6 |
| 4. Envelope contract (§4) | 7 |
| 5. Route surface vs `openapi.yaml` | 15 |
| 6. Event contract vs `asyncapi.yaml` | 12 |
| 7. The authorization action name | 3 |
| 8. Write path, live | 9 |
| 9. Provisioning reached authorization-svc | 10 |
| 10. Read scoping and malformed ids | 12 |
| 11. Outbox, end to end | 9 |
| 12. Telemetry | 14 |
| 13. Alert rules | 14 |
| 14. Release artifacts | 6 |
| 15. Migrations & RLS | 11 |
| 16. Frontend | 21 |
| **Total** | **158** |

---

## What changed

| Area | Change |
|---|---|
| Authorization | `ROLE_MANAGE`, the name the estate actually grants. The dead `ACCESS_ROLE_VIEW` constant removed and the read posture written down instead of half-implemented. |
| Events | `role_id` in every payload; a bundle change also emits `role.updated`, because that is the only name the session-revoking consumer dispatches on. |
| Delivery | Transactional outbox (`event_outbox`, migration 000004) with a relay, at-least-once delivery, per-row `attempts`/`last_error`, and depth + oldest-age gauges. |
| Conflicts | `409 role_code_exists` / `409 bundle_code_exists`, both checked **before** provisioning so a refused write leaves no orphan. Unique index on `(tenant, role, bundle_code)`. |
| Not-found | UUID guards on every id path: `404`, not `503`, and no SQLSTATE in the body. |
| Idempotency | `201` on a create, `200` on a replay, no second event on a replay. The pre-check excludes the request's own correlation id, so a retry is still a replay. |
| Readiness | Reports `database` and `authorization-svc` separately. |
| Telemetry | Four decision counters, four outbox series, every label pre-created at zero; a scrape job and six alert rules, each pointing at a runbook section that exists. |
| Documents | `openapi.yaml`, `asyncapi.yaml`, `RUNBOOK.md`, `progress.md`, this certificate, `scripts/audit.sh`, `scripts/spec_query.py`. |
| Console | Conflict explanations added; 14 Playwright specs and a hermetic mock; `ZOIKO_AUTHORIZATION_URL` pinned to a closed port so the suite stops depending on what happens to be running on the machine. |

---

## Deliberately not done

**The session-revocation chain is not closed end to end, and this certificate
does not claim it is.** `role.updated` now carries `role_id`, which was
necessary — but identity-context-svc's Kafka reader is constructed with a
single topic (`zoiko.identity.events`) and does not subscribe to
`zoiko.access-control.events` at all, so no payload shape would reach it today.
Subscribing it is a change to a different service with its own release
certificate and its own consumer-contract tests, and making it from here would
widen this pass into that service's correctness. The fix is small and named in
`progress.md`: give that reader `GroupTopics` instead of `Topic`, exactly as
authorization-svc's lifecycle consumer already does.

While it is open, retiring a role stops *new* authorize calls immediately
(verified live) and invalidates authorization-svc's cached grant sources
(verified live), but does not end sessions already holding it.

**Reads are not gated on a grant.** No bundle in this estate provisions a view
action for this service, so enforcing one would close a working page against
every existing operator — and identity-context-svc reads this catalogue as a
*workload*, where there is no principal to evaluate a grant for. Reads stay
authenticated and tenant-scoped by row-level security.

**No workflow approval step.** Defining or retiring a role is a single-actor
operation: authorized, attributed, evented, audited, but not submitted to
workflow-svc for a second human. §9.4 does not ask for one; for an operation
that withdraws access from everyone holding a role, it is worth considering.
That is a product decision and it is not smuggled in here.

**Refusals made before calling authorization-svc leave no evidence row.** A
409, a malformed request or a missing field is counted and logged but has no
durable row in this service's own tables. Left consistent with the same gap
recorded against delegated-authority-svc rather than solved differently in one
place.

---

## Re-running the proof

```bash
cd deployments
docker start zoiko-postgres zoiko-kafka authorization-svc
docker compose -f docker-compose.access-control.yml up -d

cd ../services/access-control-svc
./scripts/audit.sh           # SKIP_FE=1 to skip the console section
```

Exits non-zero if anything fails.
