# access-control-svc — implementation progress

Tracked against `docs/architecture/03-microservices.md` §9.4 and the
estate-wide standards (the canonical input contract ZS-ARCH-SVC-001 §4, the
event contract Doc 03 §19, Doc 04's data-model rules and Doc 05 §6.5 on local
decision caches). There is no dedicated `ZS-SVC-*` specification document for
this service — §9.4 is one line, and most of what this register has to get
right follows from that line plus the estate-wide invariants.

Last updated: 2026-10-06 (re-audit, then gap closure; see the two 6 Oct sections at the end).

---

## The scope decision, restated

§9.4: "maintains role catalogues, permission bundles, and policy-linked access
groupings."

authorization-svc already owns live RBAC and every other service's authz checks
resolve against it. Migrating that data out from under the whole platform is a
real architectural risk, not a weekend task; leaving this service as a
disconnected shadow catalogue would be worse, because a register nobody
enforces is a register that lies.

So this service is the governed **authoring** layer: creating a role or bundle
here makes a real synchronous call into authorization-svc's admin API, so the
definition is actually provisioned for enforcement. authorization-svc remains
the enforcement source of truth; this adds an authz-checked, idempotent,
correlation-tracked, event-publishing front door in front of what is otherwise
an unguarded admin API. *(Superseded 6 Oct 2026: per-principal assignments
are now requested, approved, revoked and reviewed here through a governed
workflow, and still held and enforced by authorization-svc. See the end.)*

---

## §9.4, clause by clause

| Clause | Status | Evidence |
|---|---|---|
| "Role catalogues" | **Done** | Tenant-scoped catalogue with status/scope/search filters and paging; `ACTIVE`/`RETIRED` constrained by CHECK and validated in the handler. audit §5, §8, §10. |
| "Permission bundles" | **Done** | Attached per role, edited and detached; one `bundle_code` per role, matching authorization-svc's own `(role_id, bundle_code)` key. audit §9, §15. |
| "Policy-linked" — the definition is actually enforced | **Done** | Every write provisions into authorization-svc *before* it records, and fails closed. Proved live: audit §9 creates a role, reads it back from `authorization_svc.roles`, retires it, and reads `active_flag` back as false. |
| Publishes `role.created` / `role.updated` / `permission.bundle.updated` | **Done** | Through a transactional outbox. audit §6, §11. |

## Estate-wide standards

| Standard | Status | Note |
|---|---|---|
| Canonical input contract §4 | **Done** | `internal/envelope` in write-strict mode. Reads gate on identity in the handler; writes gate in middleware ahead of any handler. audit §4. |
| INV-08 replay protection | **Done** | Unique `(tenant_id, correlation_id)`; 201 on a create, **200** on a replay. audit §8. |
| Tenant isolation | **Done** | Explicit `tenant_id` predicate on every query *and* `FORCE ROW LEVEL SECURITY` with `USING` + `WITH CHECK`. The store suite runs as a `NOSUPERUSER NOBYPASSRLS` role, so the policy assertions are made against a database that is enforcing it. audit §2, §15. |
| Doc 03 §19 event envelope | **Done** | `legal_entity_id` and `jurisdiction_context` deliberately absent rather than empty — a role definition is tenant-wide config and fabricating a scope would be a false provenance claim. |
| Doc 05 §6.5 bounded decision cache | **Done** | 5s TTL on GRANTED/DENIED decisions only; an unavailable authorization-svc is never cached, so one transient outage cannot become a standing permit for the life of the instance. |
| Observability | **Done** | Decision counters, outbox gauges, six alert rules, a scrape job, and a runbook section behind each alert. audit §12, §13. |

---

## Gaps closed in this pass (2026-09-22)

### 1. Every write was refused 403 — the action name matched nothing

`internal/handler/handler.go` asked authorization-svc for
`ACCESS_ROLE_MANAGE`. **That name appears nowhere else in this estate.**
`deployments/scripts/seed-demo-rbac.ps1` attaches `ACCESS_CONTROL_FULL` with
`permitted_actions ["ROLE_MANAGE"]` and says so in its own comment; the
console's copy tells the operator that a 403 means "you hold no ROLE_MANAGE
grant"; the live bundle in `authorization_svc.permission_bundles` grants
`ROLE_MANAGE`. The service was the only party asking for the other name.

The result: **every write on this service answered 403 while every read
worked** — which presents as an operator who has not been granted enough, not
as a service asking for an action nobody defines. authorization-svc's own
decision log records both sides:

```
2026-09-21 10:21:46  ROLE_MANAGE         GRANTED  rbac:role=CONSOLE_DEMO_OPERATOR
2026-09-22 06:17:59  ACCESS_ROLE_MANAGE  DENIED   no_grant
```

**Done** — the constant is `ROLE_MANAGE`, and it lives in
`internal/telemetry/domain.go` so the pre-created metric series and the check
cannot drift apart. `audit §7` asserts the name, asserts `ACCESS_ROLE_MANAGE`
appears nowhere, and asserts the estate has an *active* bundle granting it.

A dead `actionRoleView = "ACCESS_ROLE_VIEW"` constant sat beside it, unused,
implying a read check that was never made. Removed rather than wired: no bundle
anywhere grants a view action on this service, so enforcing one would close a
working page against every existing operator, and identity-context-svc reads
this catalogue as a **workload** where there is no principal to evaluate a
grant for. Reads stay authenticated and tenant-scoped by RLS, and that choice
is now written down instead of half-implemented.

### 2. `role.updated` could never revoke a session

identity-context-svc's `handleRoleUpdated` revokes the sessions of every
principal holding a role, because a role's permission bundles are **frozen into
the session envelope at resolve time** — so changing what a role grants leaves
every live envelope asserting the old grant until it expires. It reads
`payload.role_id`.

This service emitted `role_definition_id` and nothing else. The field that
consumer reads was **always empty**, so every `role.updated` was answered with
`role.updated names no role_id — cannot revoke` and dropped.

Retiring a role therefore reached authorization-svc's `active_flag` — new
authorize calls denied, correctly — while every session that already held the
role kept the actions it granted. The console's own copy says a retired role
"grants nothing to anyone from that moment"; for the sessions that mattered
most, it granted everything until they expired.

**Done** — the payload carries `role_id` (and keeps `role_definition_id`, so
consumers built against the shipped shape keep working). Pinned by
`TestRoleUpdated_PayloadCarriesRoleID`, by the store suite against real
Postgres, and by `audit §6` and `§11` — the latter reads `role_id` back out of
the outbox row that was actually written, and then out of Kafka.

A bundle change now also enqueues `role.updated` for its role. A bundle edit
*is* a change to what the role grants, and `permission.bundle.updated` is a
name that consumer's dispatch switch does not match at all — so on its own it
could never revoke anything, however the payload were shaped.

### 3. Events were fire-and-forget

All three were written straight to Kafka from the handler *after* the store
transaction had committed, with the error logged and discarded. A broker hiccup
during a retirement discarded the only notice that the role had changed, while
the register showed RETIRED, authorization-svc correctly refused new authorize
calls, and the operator was told it had worked.

**Done** — migration `000004` adds `event_outbox`; the event is enqueued in the
**same transaction** as the state change; `internal/outbox` drains it with
at-least-once delivery, retries, per-row `attempts`/`last_error`, and depth +
oldest-age gauges. Proved end to end in `audit §11`: the row is enqueued, the
relay publishes it, it is read back off the Kafka topic, and authorization-svc
is observed consuming it and invalidating its grant cache.

The relay crosses tenants through a **named** policy disjunct
(`app.outbox_relay`), not by running unscoped. Under `FORCE ROW LEVEL
SECURITY` an unscoped reader sees zero rows *silently*, which would present as
a relay that publishes nothing while reporting perfect health.

### 4. Two bundles could share a code on one role

authorization-svc identifies a bundle by `(role_id, bundle_code)` and its
attach endpoint is an upsert-**replace** on that pair. This table had no
matching constraint, so:

* creating a second bundle with the same code silently **replaced** the first
  one's permitted actions there, while both rows stayed ACTIVE here listing
  different actions; and
* detaching either one retired the single remote bundle they shared, so the
  other went on displaying as ACTIVE while granting nothing.

**Done** — unique `(tenant_id, role_definition_id, bundle_code)` in `000004`,
a `409 bundle_code_exists` checked *before* provisioning, and an audit check
that no duplicates exist. `RUNBOOK §4.6` carries the query that finds legacy
ones if the migration refuses to apply.

### 5. A duplicate role code was reported as a database outage

The `UNIQUE (tenant_id, role_code)` violation reached the caller as `503
store_unavailable` **with the raw Postgres SQLSTATE in the response body** —
a duplicate, which is the caller's to resolve in a second, presented as an
outage, which is nobody's. Worse, the violation happened *after* the role had
been provisioned into authorization-svc, so a refused create left a role there
that this register did not record: a grant nobody here could see, retire or
explain.

**Done** — `409 role_code_exists`, checked before provisioning (the unique
index remains the backstop under a race). `audit §8` asserts the status, the
code, that no SQLSTATE leaks, and that the refused create provisioned **no
orphan**.

The pre-check excludes the request's own `correlation_id`. Without that
exclusion a retry — same key, same code — reads as a new intent and answers
409, which destroys the idempotency the key exists to provide. That was caught
by the existing replay test the moment the check was added.

### 6. A malformed id answered 503 and leaked the database's error dialect

`role_definition_id` and `bundle_id` are UUID columns, so a non-UUID raised
`22P02` from Postgres rather than returning no rows, and that travelled all the
way out as:

```
503 {"error_code":"store_unavailable",
     "error_message":"ERROR: invalid input syntax for type uuid: \"not-a-uuid\" (SQLSTATE 22P02)"}
```

A database outage reported for a request that simply named nothing, sending
on-call to look at a healthy database — and handing any caller who probed it
the database's own error text.

**Done** — a UUID guard ahead of the query on every single-row path (404) and
on every collection filter (narrows to nothing). audit §10.

### 7. A replay was indistinguishable from a create

Both answered `201`. The console's client documents "201 is a new role; 200 is
a replay of one this correlation_id already created" and has a `replayed`
branch written for it — a branch that, because the service never sent a 200,
**had never once been reachable**. A caller retrying a write could not tell
whether it had just created something, which is the entire value of an
idempotency key.

**Done** — 201 on a create, 200 on a replay, and no second event enqueued on a
replay (a consumer seeing `role.created` twice for one role would act on a
creation that did not happen). audit §8; console spec
`a replayed create reports that nothing was written, not a second role`.

### 8. Readiness could not see the dependency that matters

`/readyz` pinged the database and nothing else. Every write calls
authorization-svc twice — authorize the caller, provision the definition — and
fails closed on both, so with it unreachable the pool was healthy, readiness
was green, the load balancer kept sending traffic, and **100% of writes
answered 503**.

**Done** — readiness reports `database` and `authorization-svc` separately,
because the two have opposite remedies. audit §3.

### 9. No domain telemetry, no alerts, no scrape job

Every interesting failure on this service produces an entirely ordinary
response: a write refused because the action name matches no grant is a 403
like any other, and a retirement that never reached the enforcement plane is a
503 that reads like a blip. `http_requests_total` cannot tell any of them from
normal operation.

**Done** — `internal/telemetry/domain.go` with four decision counters and four
outbox series, **every label combination pre-created at zero** (a series that
has never been observed is indistinguishable from one reading zero to an alert
expression, so a rule written to catch the first occurrence would stay silent
through exactly the event it exists for); a scrape job; six alert rules, each
checked by the audit to read a series that exists and point at a runbook
section that exists.

### 10. No contract or operational documents

No `openapi.yaml`, `asyncapi.yaml`, `RUNBOOK.md`, `RELEASE_CERTIFICATE.md`,
`progress.md` or audit script — the artifacts that let anyone other than the
author check any of it.

**Done** — all present. `scripts/audit.sh` re-proves the service against a
running stack, including the console's typecheck and Playwright specs.

### 11. The console had no e2e coverage

7,300 lines of console surface for this service and not one spec.

**Done** — `e2e/mock/access-control-service.mjs` (a hermetic stand-in modelling
the header enforcement, the error dialect, 201-vs-200, both 409s and the
404-not-503 behaviour) and `e2e/access-control.spec.ts`, 14 specs.

`ZOIKO_AUTHORIZATION_URL` is now pinned to a closed port in the Playwright
config. It was unset, so the console reached the **real** authorization-svc
whenever a developer happened to have the stack up, and the page rendered
differently depending on what was running on the machine — the spec asserting
that the page degrades cleanly without it would have passed or failed by
accident.

---

## Not done, and why

### The session-revocation chain is not closed end to end

`role.updated` now carries what identity-context-svc reads, and that was a
necessary fix. It is **not sufficient**: that service's Kafka reader is
constructed with a single topic (`KAFKA_EVENTS_TOPIC`, defaulting to
`zoiko.identity.events`), so it does not receive `zoiko.access-control.events`
at all — no payload shape would help.

Subscribing it is a change to identity-context-svc, which is a different
service with its own release certificate and its own consumer-contract tests.
Making it from here would widen this pass into a second service's correctness
and would be the wrong place to review it. **Recorded rather than done**, and
the fix is small: give that reader `GroupTopics` instead of `Topic`, the way
authorization-svc's lifecycle consumer already does — it subscribes to all
three topics and is verified consuming ours in audit §11.

The consequence while it is open: retiring a role stops *new* authorize calls
immediately (authorization-svc's `active_flag`, verified live) but does not end
sessions already holding it; those keep their frozen bundles until the envelope
expires. That is a narrower window than it was, because authorization-svc's
cached grant sources *are* invalidated on our event, but it is not zero.

### Reads are not authorized against a grant

Stated under gap 1 above. If the estate ever provisions a view action for this
service, wiring it is one call — but it must exempt workload callers, or
identity-context-svc's session resolution breaks.

### No workflow approval on a role definition

Defining or retiring a role is a single-actor operation here: it is authorized,
attributed, evented and audited, but no route submits it to workflow-svc for a
second human. Doc 03 §9.4 does not ask for one. For an estate where retiring a
role withdraws access from everyone holding it, a maker-checker step is worth
considering — it is a product decision, not a defect, and it is not smuggled in
here.

### ~~Refusals this service makes before calling authorization-svc leave no evidence row~~

Closed by migration 000006 (`refused_escalations`) and completed on 6 Oct: early
refusals (invalid_json, missing_fields) are now actually written (they were
refused by RLS), UPDATE_ROLE and DETACH_BUNDLE refusals are recorded, and the
table is append-only under FORCE RLS (000007).

---

## 6 Oct 2026 re-audit: two regressions and five gaps, all fixed

Live `scripts/audit.sh` before the fixes: **111 / 137**. After: **153 / 153**
(16 new checks: §9b guards, §15 000007 posture). Unit tests 65 (was 53), store
tests 21 (was 16).

| # | Finding | Fix |
|---|---|---|
| R1 | **Regression (28 Sep):** the SoD client sent `{role_code, bundle_code, permitted_actions, …}` to `/v1/sod/validate`, which takes `candidate_actions` and answers 400 without it. Every error became `403 sod_conflict`, so **no role or bundle could be created**. | Client speaks the real contract and reads `conflict_free` strictly. |
| R2 | **Regression (30 Sep, cross-service):** authorization-svc began gating its admin API on `iam.*` at tenant scope. Its 403 was reported here as `503 authz_admin_unavailable`, and the dev seed could no longer bootstrap. | 403 → `403 provisioning_forbidden` with authorization-svc's reason; the seed bootstraps the iam grants and has the approver make assignments (no self-grant). |
| 3 | SoD failed **open**: a 200 `conflict_free:false` would have counted as a pass. | Strict verdict; no verdict → `503 sod_unavailable`. |
| 4 | SoD checked only the bundle being written, never the role's other bundles; reactivation skipped it; role creation made a meaningless call. | Candidate set = role's other active bundles + this one; reactivation checked; role-create call removed. |
| 5 | Protected-action guard **never fired**: it fetched a route nothing serves, and an empty list meant "skip". The seeded table was unused, and lacked the `iam.*` actions. | Reads `protected_permissions` (fail closed, also on empty); 000007 adds 11 `iam.*` actions; case- and space-insensitive match; names the action. |
| 6 | `refused_escalations` ENABLE-only RLS, FOR ALL policy (a tenant could rewrite its refusals), and early refusals lost to RLS. | 000007: FORCE, SELECT + INSERT policies only, revoke UPDATE/DELETE; store fills the request tenant. |
| 7 | UPDATE_ROLE and DETACH_BUNDLE refusals left no evidence row. | Recorded. |

**Still open, outside this service:** authorization-svc's dev database has
**no SoD rules** (the §10.1 baseline is unseeded), so `sod_conflict` can only
fire on rules someone adds; the audit installs a tenant-scoped test rule.

---

## 6 Oct 2026, second pass: the unbuilt §5 / §9 / §24 scope built

Score on the audit's 10 §9 rows: **2 → 9 of 10** (35% → 95% weighted). Live
`audit.sh` **229 / 229** (was 153); `live_session_revocation_check.sh` 10 / 10.
The full write-up, with the five decisions this took, is in
`docs/audit_files/Identity, Scope & Foundation-audit-2026-09-23.md` (9/9,
"Gap closure, 6 October 2026 (second pass)").

| Area | What exists now |
|---|---|
| §5 taxonomy | `permission_definitions` (000008); every bundle action must be registered; `GET /v1/permission-definitions` |
| §9 templates, §9.1 archetypes | `role_templates` + append-only versions (000009), 21 archetypes; instantiate / upgrade; template bundles immutable through the bundle routes |
| §9 / §21 assignment requests | `POST /v1/iam/access-assignments` + `:approve`, `:reject`, `:cancel`, `:revoke` (000010); risk-based approval, independence, held-based SoD |
| §9 / §24 attestation | `/v1/access-review-campaigns` (000011): snapshot, orphan + toxic-combination flags, decisions, high-risk completion gate; `GET /v1/iam/access-reviews` |
| §19 revocation | `iam.assignment.granted` / `.revoked` feed identity-context-svc's role-holder projection (which had no feed, so `role.updated` ended no session); proven live |
| §10.1 baseline | authorization-svc 000019 (global rules) |
| Idempotency-Key | Honoured (000012 + `internal/idempotency`) |

**Still open:** group subjects and grant-time end dates on assignments
(authorization-svc's admin API has neither); automatic event-triggered reviews
and dormancy detection; the 668 legacy action names are not §5-shaped; a
reviewer revoking via a review decision needs `iam.assignment.revoke` in
authorization-svc.

---

## 7 Oct 2026 re-audit (third pass): the 6 Oct "9 of 10" re-scored, eight gaps closed

The code was read again against §9, §21, §22, §23 and §24, with no credit
taken from earlier passes. The 6 Oct score of **9 of 10 (90%, 95% weighted)**
did not survive: four rows had defects or missing clauses, giving **6 of 10
(60%, 80% weighted)** before today's fixes. After them: **9 of 10 (90%, 95%
weighted)**, and this time each claim is backed by a live check in `audit.sh`
§9d.

| # | Finding | Row | Fix |
|---|---|---|---|
| D1 | approve / reject / cancel / revoke and campaign reassign / complete checked `ROLE_MANAGE` on the **body's** `legal_entity_id`, never the record's own. A manager of entity B could reject or cancel entity A's requests, or close A's review campaign, by naming B. | Assignment request; review | The body's entity must equal the request's / campaign's (`400 entity_mismatch`, recorded). |
| D2 | Review snapshots read authorization-svc's assignment list, which stops at **500 rows** with no indication. A campaign over a role with more holders reviewed only part of them, and left the rest unremovable through review. | Review | authorization-svc: paged `limit`/`offset`; the client reads every page. |
| D3 | Toxic combinations were grouped by (principal, entity). A **tenant-wide** assignment has no entity, so a tenant-wide Payment Preparer and an entity Payment Releaser were never compared. | Review (§24) | Tenant-wide items join every entity group of the principal. Live: both halves flagged. |
| D4 | No end date at grant time (§9 / §22 "effective dates"). | Access assignment | `effective_to` on the request, provisioned into authorization-svc (new optional field there), in `iam.assignment.granted`, projected by identity-context-svc. |
| D5 | No effective-dated revocation (§9 "immediate **or effective-dated** removal"). | Revocation | `:revoke` with `effective_at` schedules the end there and here; it can only bring an end earlier (`409 ends_sooner`). |
| D6 | Nothing ever announced an assignment ending by date, so identity-context-svc never ended the sessions holding it. | Revocation | Expiry sweep (`internal/expiry`, every 30s) closes due rows (EXPIRED, or REVOKED when scheduled) and enqueues `iam.assignment.revoked` in the same transaction. Cross-tenant scan through a SELECT-only named policy (000013). |
| D7 | Approval authority did not depend on risk. Any independent `ROLE_MANAGE` holder approved CRITICAL grants. §9 asks for manager / data-owner / **security** approval "depending on risk". | Assignment request | CRITICAL also needs `iam.assignment.approve_privileged` (new, protected; `SECURITY_ACCESS_APPROVER` template). Otherwise `403 security_approval_required`. |
| D8 | Reviews detected no stale or unowned assignments (§24 "Dormancy", "Orphan detection"). | Review | `DORMANT` flag: HIGH/CRITICAL, held through the window, with no GRANTED decision on record (window per campaign, default 90 days). `SUBJECT_INACTIVE` flag: the subject is suspended or disabled; raised to HIGH. Evidence is stored on the item. |

| Row | 6 Oct claim | 7 Oct before fixes | After |
|---|---|---|---|
| System role template | ✅ | ✅ | ✅ |
| Tenant custom role / protected | ✅ | ✅ | ✅ |
| Access assignment | ⚠️ | ⚠️ (no end date, no groups) | ⚠️ (groups only) |
| Assignment request | ✅ | ⚠️ (D1, D7) | ✅ |
| Assignment review / attestation | ✅ | ⚠️ (D1, D2, D3, D8) | ✅ |
| Revocation invalidates cache / session | ✅ | ⚠️ (D5, D6) | ✅ |
| §9.1 archetypes | ✅ | ✅ | ✅ |
| §5 taxonomy | ✅ | ✅ | ✅ |
| §10.1 consulted | ✅ | ✅ | ✅ |
| §20 evidence | ✅ | ✅ | ✅ |
| **Score** | 9/10 (90%, 95%) | **6/10 (60%, 80%)** | **9/10 (90%, 95%)** |

**Still open:**

- **Group subjects** on assignments. No service in the estate holds groups or
  group membership. §22 assigns them to "Identity/IAM". This is the one ⚠️.
- **Event-triggered reviews start only when a caller asks.** A caller starts
  them and names the trigger. Nothing yet consumes manager-change or
  contractor-end events to start one automatically.
- `DORMANT` reads authorization-svc's decision log through the role code in
  `decision_basis`. A decision granted through several roles counts as use of
  each of them.

Changed outside this service: authorization-svc (`effective_to` on create,
scheduled revoke, paged list with usage and subject status). identity-context-svc
(projects `effective_to`). `seed-demo-rbac.ps1` (the IAM admin bundle holds
`iam.assignment.approve_privileged`). Migration **000013** is applied to the dev
`access_control` database, and its down/up round trip has been checked.
