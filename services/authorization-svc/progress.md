# authorization-svc — Progress

## Status: v1 built and verified end-to-end (2026-07-08)

Built from scratch per `docs/architecture/03-microservices.md` §8.3 and
`docs/architecture/04-data-model.md` §6.1 — the last unstarted Phase 1
Sovereign Spine service with a concrete spec (Workflow & Approvals Service
remains unassigned separately).

## What's implemented

- `Role` / `PermissionBundle` / `PrincipalRoleAssignment` admin CRUD —
  tenant-scoped roles, each owning one or more permission bundles (a JSON
  array of granted action codes), assigned to principals with
  entity-scoping and effective-dating. No hard-delete: assignments end via
  `effective_to`, roles deactivate via `active_flag`.
- `DelegatedAuthority` admin CRUD — one principal delegates their own
  effective grants to another, entity-scoped and effective-dated.
  `revocation_status` is a real one-way state machine (`ACTIVE` ->
  `REVOKED`), enforced in application code; a second revoke attempt
  correctly 409s rather than silently no-opping.
- `SoDRule` admin CRUD — Separation-of-Duties conflict pairs, optionally
  jurisdiction-scoped (validated synchronously against the real
  jurisdiction-rules-svc, fail-closed, same pattern as obligations-svc).
- **The evaluation engine** (`POST /v1/authorize`) — the actual value of
  this service. Layers, in order:
  1. **RBAC** — does the principal directly hold a role granting the
     action in this legal entity, right now (effective-dated)?
  2. **Delegated access** — if not directly granted, does the principal
     have an active, non-expired delegation from someone who holds it?
  3. **Separation of Duties** — if granted by either layer, does holding
     this action alongside anything else the principal already holds
     (RBAC ∪ delegated) violate an active SoD rule? SoD conflicts are
     checked across delegated grants too, not just direct RBAC grants —
     confirmed by manual testing (see below).
  - Every evaluation — grant or deny — is written to `access_decision_log`
    before the response returns (critical constraint: "no material action
    executes without an authorization decision artifact"). `decision_basis`
    always names which layer produced the outcome (e.g.
    `rbac:role=FINANCE_APPROVER`, `delegated:from=principal-x`,
    `sod:conflict_with=PAYMENT_INITIATE`, `no_grant`) — never a bare
    "denied" with no reason.
  - Fail-closed: any store error during evaluation returns 503 with
    nothing recorded, rather than guessing an outcome. "Cannot evaluate"
    and "evaluated and denied" are kept as distinct, never conflated.
- `GET /v1/access-decisions/{id}` — the "retrieve authorization rationale"
  capability.
- Real Kafka producer: `authorization.granted`, `authorization.denied`,
  `sod.violation.detected` (fired in addition to `authorization.denied`
  specifically when the denial reason was an SoD conflict).

## Deliberate v1 scope decisions (not oversights)

- **ABAC is not implemented.** ~~No attribute-condition rules exist anywhere
  in the architecture docs to encode — implementing it now would mean
  inventing business logic, not encoding a specified rule.~~ **Superseded
  2026-09-03** — see "Second pass" below. The premise was right about the
  RULES and was then read as an argument against the ENGINE. `abac_rules` +
  `internal/abac` now exist and ship with zero rules.
- **No consumed events in v1** (`role.assigned`, `authority.delegated`,
  `employment.changed`, `entity.scope.updated`). ~~None of these are
  actually published by any built service today~~ — **partly wrong, corrected
  2026-09-03**: `authority.delegated` (plus `authority.revoked` and
  `authority.expired`) IS published, by delegated-authority-svc, and is now
  consumed. The other three still have no producer and are still deliberately
  not consumed. See "Second pass" below.
- **"Validate entity scope" and "validate SoD conflicts" are not separate
  standalone endpoints.** Both are folded into `POST /v1/authorize` as
  internal layers — same simplification policy-svc made folding "validate
  threshold applicability" into `Evaluate`. The capabilities exist, just
  not as separate HTTP surface.
- **No Authorization Service calls itself, obviously** — this service
  doesn't call out to itself. No other service has been wired to call
  *into* authorization-svc yet either; that's the natural next integration
  step now that this exists (e.g. tenant-entity-registry-svc's
  `AUTHZ_SERVICE_URL` currently points back at itself as a stub).

## Verified (real infra, not mocks)

- `go build`/`go vet`/`go test` clean
- 8 store-layer integration tests against a real PostgreSQL instance:
  role idempotency + 409 conflict, RBAC grant resolution (including
  entity-scope isolation), role-assignment revocation actually ending a
  grant (and correctly 404ing on double-revoke), delegated-authority
  revocation as a one-way transition, delegation resolution through to
  the delegator's own grants, SoD conflict detection in both directions,
  and access-decision record/retrieve.
- Booted the real Docker image against the live platform compose stack
  (real Postgres, real Kafka, real jurisdiction-rules-svc) and drove a
  full real scenario over HTTP:
  1. Created a role, granted it `PAYMENT_APPROVE` + `PAYMENT_INITIATE`,
     assigned it to a principal — evaluated `PAYMENT_APPROVE` → `GRANTED`.
  2. Created an SoD rule pairing those two actions — re-evaluated the
     identical request → now correctly `DENIED` with
     `sod:conflict_with=PAYMENT_INITIATE`, proving the same request's
     outcome correctly flips once a conflict rule exists.
  3. Confirmed both `authorization.denied` and `sod.violation.detected`
     independently consumed off the real `zoiko.authorization.events`
     Kafka topic.
  4. Delegated the same principal's authority to a second principal and
     confirmed the delegate correctly inherits — and is correctly denied
     by the same SoD rule — proving SoD checks apply across delegated
     grants, not just direct RBAC.
  5. Confirmed rationale retrieval (`GET /v1/access-decisions/{id}`)
     returns the exact recorded decision.

## Bugs found and fixed during this build

- **`docker-compose.yml` had a duplicate `policy-svc:` YAML key** — an
  artifact of two earlier PRs both wiring policy-svc into compose
  independently. This made `docker compose` fail to parse the file *at
  all* (a hard YAML error, not a silent issue) for anyone touching the
  stack. Fixed by removing the stale duplicate (which still had the old
  broken `wget` healthcheck and was missing the Kafka env vars).
- **`authorization` is a reserved SQL keyword** (`CREATE SCHEMA ...
  AUTHORIZATION owner`) — a bare `CREATE DATABASE authorization` fails
  with a syntax error. Renamed the actual database to `authorization_svc`
  everywhere (config default, `docker-compose.yml`, `init-db.sh`) rather
  than quoting the identifier forever as a landmine for the next person.

## Not yet done

- No other service currently calls `POST /v1/authorize` — this is
  infrastructure now available for other services to adopt, not yet
  wired into anyone's write path.
- Workflow & Approvals Service remains the one unassigned Phase 1
  service; per its own spec it explicitly depends on this service
  (`authorization.denied` is a consumed event there).

---

# Second pass — 2026-09-03

Closing the gaps in the scorecard above. Everything below was measured on
PostgreSQL 16.15 through a purpose-created `NOSUPERUSER NOBYPASSRLS` role,
because that is the only configuration in which the row-security policies this
service depends on actually execute.

## The live bug: delegated access granted nothing at all

`PgStore.FindDelegatedActions` read `delegated_authorities` on the bare pool —
outside both `withRLS` and `withPlatformScope`. Migration `000006` had given
that table a policy with no `app.platform_scope` hatch, so a connection that
installs neither setting matches no rows at all: `current_setting` returns
NULL, the policy's `NULLIF` of it is NULL, and `tenant_id = NULL` is NULL,
never true.

Layer 2 of 4 therefore returned an empty action set for **every request** on
**every deployment where the policy binds** — compose (`DB_USER=zoiko_app`,
`NOSUPERUSER NOBYPASSRLS`) and Supabase (`app_authorization`) both.

It failed **closed**, which is why nothing broke visibly: a delegate was denied
with basis `no_grant`, indistinguishable from having no delegation.
`TestPgStore_FindDelegatedActions_ResolvesViaDelegator` passed throughout,
because it runs as the migration user and a superuser bypasses row security
unconditionally.

Measured, one ACTIVE / in-date / correctly-tenanted delegation present:

| connection state | rows |
|---|---|
| no `app.tenant_id`, no platform scope — **the shipped behaviour** | **0** |
| `app.tenant_id` installed | 1 |
| `app.platform_scope` only — no hatch to honour | **0** |

Both halves of the fix are load-bearing, and each was proven so by removing it
and watching the specific subtest fail:

1. Routing the query through `withRLS` / `withPlatformScope` fixes the
   **tenant-supplied** path.
2. Migration `000008`'s platform-scope hatch on `delegated_authorities` fixes
   the **tenantless** path.

On which of those is load-bearing today, precisely — because the first draft of
this note got it wrong. The canonical input-contract middleware
(`ZS_ENVELOPE_ENFORCEMENT`, default `write-strict`) treats `tenant_id` as
unconditionally mandatory and answers **401 before the handler runs**, so a
tenantless `POST /v1/authorize` does not reach the store on a default
deployment. Fix (1) is therefore the one that restores delegated access for
every caller that gets through. Fix (2) still matters: `observe` mode is a
documented migration state in which the branch IS reachable, and without it
`FindDelegatedActions`' own documented contract — an empty tenant evaluates
across tenants — silently returns nothing instead, which is exactly how the
original defect survived review. See "Found on the way: 75 of 97 authz clients
are refused" below, which is the same middleware seen from the other side.

Regression test:
`TestPgStore_RLS_FindDelegatedActions_ResolvesUnderOrdinaryRole`, both
subtests, in `internal/store/rls_delegation_test.go` — a file that exists
because a suite which only uses the migration connection proves nothing about
row security.

## Three more defects found while fixing that one

- **`ACTION_SUBSET` delegations conferred FULL authority.** `scope_type` has
  always accepted `'ACTION_SUBSET'` and `authority_limit_type` /
  `authority_limit_value` have always been stored, and nothing ever read any of
  them — the evaluation unioned the delegator's entire grant set regardless. A
  delegation recorded as restricted looked restricted in the register and was
  not. `delegated_actions` (000008) is now intersected with the delegator's
  **live** grants, so a delegation can never confer an action its delegator
  does not currently hold.
- **The own-object SoD check used the request BODY's tenant** while every other
  layer used the resolved scope. `CheckOwnObjectSoD`'s predicate is
  `tenant_id IS NULL OR tenant_id = NULLIF($2, empty)`, so a caller doing
  exactly what `resolveTenantScope` encourages — forwarding `X-Tenant-Id`,
  omitting `tenant_id` from the body — had its tenant's own-object rules
  silently skipped. It punished the best-behaved callers.
- **The tenantless delegation path resolved the delegator's roles
  cross-tenant.** Under platform scope every tenant's rows are visible, and the
  old code passed an empty tenant straight into a nested `FindGrantedActions` —
  so a delegation made in tenant A could resolve against the delegator's roles
  in tenant B. The same escalation `FindGrantedActions`' own comment documents,
  by a different route. `r.tenant_id = da.tenant_id` is now in the query.

## ABAC (the objection, answered)

The v1 note was right that no concrete attribute rule exists in the
architecture docs to encode, and that inventing one would be inventing business
logic. What it conflated was the **rule** with the **rule engine**. The spec
assigns this service the ABAC *decision logic*; that is a mechanism, and a
mechanism can be built without knowing a single rule.

- `abac_rules` (migration `000010`) is the table rules are declared in. It
  **ships with zero rows**, and `TestPgStore_ABACRules_ShipsEmpty` fails if a
  future migration seeds one.
- `internal/abac` is the evaluator: eleven comparison operators, two effects
  (`REQUIRE` / `FORBID`), and no attribute name or threshold anywhere in the
  package.
- **Deny-only.** A rule can remove an action the RBAC/delegation layers
  granted; it can never add one. Structural, not stylistic — see
  `domain.ABACRule`.
- Admin surface: `POST/GET /v1/admin/abac-rules` and
  `/{id}/retire|reactivate`. A platform-wide rule (no `tenant_id`) requires the
  distinct `ABAC_RULE_MANAGE_GLOBAL` grant at platform scope, exactly as a
  platform-wide SoD rule already does, and cannot be retired from any one
  tenant's console.
- An absent attribute **denies** a `REQUIRE` rule and **permits** a `FORBID`
  one. Otherwise any caller could evade a `REQUIRE` rule by omitting a JSON
  field.
- An operator or effect the evaluator cannot execute is refused at authoring
  time (400) and, if one reaches evaluation anyway, denies with basis
  `abac:rule_unevaluable=<code>` and an ERROR log — a condition nobody can
  evaluate has not been met.

This is the same shape `sod_rules` already has, and nobody calls that invented
business logic.

## Consumed events (the correction)

`authority.delegated` **does** have a producer: delegated-authority-svc
publishes it, plus `authority.revoked` and `authority.expired`, with a full
payload, on `zoiko.delegated-authority.events`. The authoritative owner of the
delegated-authority concept has been announcing every grant and revocation it
makes, to nobody.

`internal/events/consumer.go` now projects those three into this service's
`delegated_authorities` table. `role.assigned`, `employment.changed` and
`entity.scope.updated` still have no producer anywhere on the estate and are
still deliberately not consumed — `TestConsumedEventTypes` pins both halves of
that decision.

This is also what stops **tracker item 81** ("two delegation stores, one is
decorative") being true: delegated-authority-svc stays authoritative for the
lifecycle, and this table becomes the evaluation read-model `/v1/authorize`
resolves against. Projected rows carry `source_service` /
`source_delegation_id`, are upserted on the upstream id (Kafka redelivers, and
an INSERT per delivery would multiply one delegation into a duplicate grant),
and an upstream event can never revoke a locally-authored row. It does not by
itself *close* 81 — retiring this service's own delegation write API is a
cross-service decision that needs the console migrated first — but neither
store is decorative now.

## Performance — and where the 1.07s actually was

The scorecard reported 1.07s for one `/v1/authorize` call and attributed it to
missing caching and the synchronous decision-log insert. Measured, it was
neither. Three separate things, in descending order of size.

### 1. An 850ms synchronous call to a SIEM service that was not running

`internal/siem`'s package comment said: "This is deliberately fire-and-forget:
streaming is a monitoring side-channel, never a gate. A slow or unreachable
siem-integration-svc must never delay or fail the request that triggered the
security event."

That was a claim, not a fact. `Stream` did the exporter lookup and the POSTs
**inline, on the caller's goroutine, on the caller's request context**, with a
2s HTTP timeout. `/v1/authorize` calls it on **every DENIED decision** — the
platform's hottest endpoint, on the branch a probing or misconfigured caller
hits repeatedly.

And "not running" is the compose **default**: `docker-compose.yml` points
`SIEM_SERVICE_URL` at siem-integration-svc, which lives in
`docker-compose.phase6.yml`. Any stack without phase6 up pays this on every
denial.

Measured on `POST /v1/authorize` returning DENIED, everything else held
constant, n=8 sequential, warm-up discarded:

| | median |
|---|---|
| `SIEM_SERVICE_URL` empty (streaming off) | 11 ms |
| `SIEM_SERVICE_URL` set, service not running — **the compose default** | **850 ms** |
| after the fix, service still not running | **12 ms** |

`Stream` now enqueues onto a bounded queue and returns; four workers deliver on
their own goroutines with their own background context — deliberately not the
request context, which is cancelled as the response is written and would
otherwise cancel every event. A full queue **drops** and counts the drop rather
than blocking or growing without limit, and `Close` drains on shutdown so a
SIGTERM does not discard an accepted event. `TestStream_DoesNotBlockTheCaller`
and `TestStream_DoesNotUseTheRequestContext` pin both halves.

This is very likely the whole of the reported 1.07s.

### 2. The delegation lookup was 1 + N transactions

One transaction to list delegators, then a separate `FindGrantedActions` — its
own transaction — **per delegator**. Five delegators meant six transactions. It
is now one query in one transaction returning the same set, and that rewrite is
what fixed the RLS bug and the two over-grants as well.

### 3. Nothing was cached

`internal/cache` decorates the store with a short TTL (default 5s,
`AUTHZ_CACHE_TTL_SECONDS`, `0` a true off switch) over the five evaluation
reads, invalidating exactly on every write that passes through it — including
**every** tenant when a platform-wide SoD or ABAC rule is authored, since
scoping that to the author's tenant would leave every other tenant enforcing
the old rule set.

Measured honestly, same binary and database, only the TTL differing, n=30
sequential on a delegated grant:

| | median | mean |
|---|---|---|
| cache disabled | 13.21 ms | 17.69 ms |
| cache enabled | 7.95 ms | 14.61 ms |

About 5ms of database round-trips per call on a local instance with a small
dataset. Real, worth having, and an order of magnitude smaller than item 1 —
which is the point worth carrying forward: the gap list attributed the latency
to the database, and the database was never the expensive part.

### What is deliberately still synchronous

`RecordAccessDecision`. The scorecard lists it as a scaling risk and it is one,
but it is not movable: the response returns the `access_decision_id`, and the
critical constraint is that no material action executes without a decision
artifact. Making it async, batched or best-effort would mean answering GRANTED
for a request whose evidence may never land. The insert is ~4ms of the ~8ms
remaining, against a table now partitioned by month so it stays that way.
`TestCache_NeverCachesTheDecisionArtifact` pins that a cache hit never skips
it.

## access_decision_log retention

One row per evaluation, platform-wide, on a table `000001` correctly declared
append-only — which together describe unbounded growth with no sanctioned way
to shrink, paid as insert latency on every request forever.

Migration `000009` converts it to **monthly RANGE partitions**, atomically,
comparing row counts before dropping the original.

- Retention is `detach_access_decision_log_partitions_before(cutoff)`:
  `DETACH`, never `DELETE`. The rows survive in an ordinary table for an
  operator to archive and then drop deliberately, so append-only stays true.
- `access_decision_log_default` is **not optional**. A partitioned table with
  no partition for an inserted row rejects the insert, and here that is a 503
  on the platform's hottest path — the month after the last one anybody created
  would take authorization offline at midnight. The default partition catches
  those rows; `access_decision_log_retention_status` reports them.
- Every partition gets the parent's policy, because a partition inherits none
  and would otherwise be an unprotected copy of a protected table.
- The primary key becomes `(access_decision_id, decided_at)` — Postgres
  requires the partition key in it — so the id-only rationale read is served by
  an explicit index instead.

## Platform scope (tracker item 67)

Services with a platform-wide act had nowhere to put a scope and each invented
its own synthetic `legal_entity_id`; a grant seeded against one was invisible
to a check made against another, silently and fail-closed.

`legal_entity_id: "PLATFORM"` now resolves to `AUTHZ_PLATFORM_SCOPE_ENTITY_ID`
— one id, configured once, the same one `requirePlatformAction` already uses. A
sentinel rather than accepting an empty field: an omitted `legal_entity_id` is
far more often a caller bug, and it still answers 400. An unconfigured
deployment answers 400 too, rather than inventing an id.

`AUTHZ_PLATFORM_SCOPE_ENTITY_ID` was never set in compose, so platform-wide SoD
rules could not be authored on that stack at all. It is now set to the
`00000000-0000-0000-0000-00000000f001` that every calling service already
carries as `AUTHZ_PLATFORM_SCOPE_ID`.

## Ordinary database role

`DB_USER` in compose was the shared `zoiko_app`. It is now `app_authorization`,
which `create-app-roles.sh` has provisioned since `000007` landed — "flipping
DB_USER is the step that makes the policies bite", in that script's own words.
`zoiko_app` was already `NOSUPERUSER NOBYPASSRLS`, so the policies did apply;
what it was not is per-service, so a compromise of any one service reached
every other service's database.

## Found on the way: 75 of 97 authz clients are refused

Not caused by this pass, not fixed by it, and the largest thing found in it.

This service mounts the canonical input-contract middleware ahead of every
route. `ZS_ENVELOPE_ENFORCEMENT` defaults to `write-strict` and nothing in
`deployments/` sets it, so `POST /v1/authorize` is treated as a material write
and requires `X-Tenant-Id`, `X-Principal-Id`, `X-Legal-Entity-Id`,
`X-Request-Id`, `X-Source-Channel` and `Idempotency-Key`. The first two are
unconditionally mandatory and deliberately not expressible per service.

Measured against the running container, sending exactly what each client sends
in code:

| client | headers it sends | result |
|---|---|---|
| obligations-svc | Content-Type, X-Correlation-ID | **401** |
| jurisdiction-rules-svc | Content-Type | **401** |
| policy-svc | the full envelope | 200 |

Sweeping every non-test Go file that builds a request to `/v1/authorize`:
**22 conformant, 75 not** — including accounts-payable, general-ledger,
workflow-svc, every `payment-*` and `privacy-*` service,
tenant-entity-registry-svc and identity-context-svc. Those clients fail closed
on a non-200, so the writes they guard are denied and the reason surfaces as an
authorization failure rather than as a missing header.

**Deliberately not fixed here.** Both candidate fixes are decisions: relaxing
this service's own `ServicePolicy` (a `MaterialWrite` override — the case
`Policy.MaterialWrite`'s doc comment describes, though this endpoint does write
the decision artifact) weakens a control on the authorization path; migrating
75 clients is the doctrinally correct end state and is 75 services of edits.
**The gateway does not cover it** — checked, on three grounds. Service-to-service
authz calls never traverse it (all 99 `AUTHZ_SERVICE_URL` values dial
`authorization-svc:8089` directly; ForwardAuth is attached to Traefik routers,
i.e. browser traffic). Even on the gateway path its `authResponseHeaders` carry
only 4 of the 6 required fields — `X-Request-Id`, `X-Source-Channel` and
`Idempotency-Key` are absent, and `/verify` sets none of them, because those
come from the original client. And the local Traefik config carries no auth
middleware at all, as its own header says. Recorded as tracker row 82i and in
`known-gaps.md`.

This is also why the tenantless-caller paths in this service — the
`resolveTenantScope` no-tenant branch, the store's `$3 = ''` fallbacks,
migration 000008's platform-scope hatch — are not the live path while
enforcement is `write-strict`. They are still correct and still needed (observe
mode reaches them, and the store documents the contract), and the comments that
described them as the majority path have been corrected.

## Still open, and why

- **Tracker item 81 (two delegation stores)** — narrowed, not closed. Full
  consolidation means retiring this service's delegation write API, which needs
  the console migrated off it first. Cross-service decision.
- **Tenant fallback in `/v1/authorize`** — the body-then-nothing fallback
  stays, and the reason it was kept has turned out to be moot rather than
  wrong. It was kept so a caller with no `X-Tenant-Id` would not be refused;
  the canonical input-contract middleware already refuses exactly that caller
  one layer up (see "Found on the way" above), so the fallback protects nobody
  who currently reaches it. Left in place regardless: removing it is only
  meaningful once the envelope question above is settled, and doing it first
  would couple two unrelated decisions. Every use is still logged.
- **`internal/siem`'s inline-streaming shape in four other services** —
  gateway-auth-svc, identity-context-svc, key-management-svc and
  mtls-management-svc each vendor a copy of this client, and the 850ms defect
  was in the shared shape rather than in this service's use of it. Only
  authorization-svc's copy was changed; the others are presumably identical and
  were not touched.
- **Three unconsumed events** — `role.assigned`, `employment.changed` and
  `entity.scope.updated` have no producer. Still dead infrastructure if built.
- **Retention is not scheduled.** `AUTHZ_ACCESS_DECISION_RETENTION_MONTHS`
  declares the window (24 by default) and the SQL function exists and is
  tested; nothing calls it on a timer yet. The default partition and the
  three-month runway make that a growth question, not an outage one.

## Verified

- `go build` / `go vet` / `gofmt` clean; full `go test ./...` green.
- Store integration tests against real PostgreSQL 16.15, including a
  `NOSUPERUSER NOBYPASSRLS` role that asserts its own non-superuser status
  first — so a misconfigured instance fails loudly instead of making every
  isolation assertion vacuous.
- Migrations `000008`–`000010` applied, then their `down` files applied, then
  re-applied.
- Supabase mirrors `0034` / `0035` applied twice each (idempotent), and the
  regenerated `zoiko-suite-0034-0035.sql` delta applied to a fresh pre-0034
  database — ending with zero tables or partitions in `authorization_svc`
  lacking forced row security.
- Both negative controls recorded rather than assumed: removing the `000008`
  hatch fails exactly the tenantless delegation subtest and nothing else;
  restoring `req.TenantID` on the own-object check fails exactly its regression
  test.

---

# Third pass — 2026-09-08

Closing the console gap. The two passes above built and hardened the service;
neither asked what could reach it. Of fifteen routes, the Next.js console wired
**three** — `POST`/`GET /v1/admin/role-assignments` and `GET /v1/admin/sod-rules`
— and two of the twelve it did not wire had no read endpoint to wire.

## The evaluation engine had no console at all

`POST /v1/authorize` is the service. Roles, permission bundles, assignments,
delegations, SoD pairs and ABAC rules all exist to change what it answers, and
nothing in the console could ask it anything. An operator built a grant and then
waited for some other service to exercise it to find out whether it worked — and
when the answer was DENIED, there was nowhere to read why.

`GET /v1/access-decisions/{id}` was in the same position. Every evaluation is
written to the decision log before the response returns, *specifically* so a
refusal can be explained afterwards, and nothing could read one back. The
recording was load-bearing for an audit nobody could perform from the console.

Both are now wired, and `decision_basis` is translated rather than displayed.
All eight forms the service emits:

| stored basis | rendered as |
|---|---|
| `rbac:role=X` | Allowed by a role — held directly, in force now |
| `delegated:from=Y` | Allowed by a delegation — borrowed, and only while the lender holds it |
| `no_grant` | Not allowed — nothing grants it. A plain absence, not a block |
| `sod:conflict_with=Z` | Blocked — conflicting duties. Granted, then taken away |
| `sod:own_object_forbidden` | Blocked — it is their own work. This item only |
| `abac:forbidden=C` | Blocked — a condition forbids it, for this request |
| `abac:require_failed=C` | Blocked — a condition was not met (an absent attribute counts as not met) |
| `abac:rule_unevaluable=C` | Blocked — a broken rule, denying for **everyone** until fixed |

Each carries what to do about it, and none of them drops the stored string: the
raw basis and the raw outcome are both rendered beside the wording, because an
auditor cites what the service holds and not the console's paraphrase.

**The distinction the console now refuses to blur.** A denial is a 200 with a
basis: the service evaluated, answered, and recorded the artifact. A 503 is the
service declining to guess — nothing decided, nothing recorded, and every caller
asking that question refusing to write. Those render as visibly different
things, with the second one saying in its own words that it is *not* a no. The
service has always been careful about this; the console previously had no way to
be either way.

## Two write-only admin surfaces, now readable

Added, because they did not exist:

- **`GET /v1/admin/roles`** — `PgStore.ListRoles`, tenant-scoped through the
  explicit predicate *and* `withRLS`, retired roles last. This service could
  create a role, retire it, reactivate it and attach permission bundles to it,
  and never list what it held: the only way to learn a role's id was to have
  been the caller that created it, and `role_code` — the idempotent creation key
  — could not be checked for collision before writing.
- **`GET /v1/admin/delegated-authorities`** — `PgStore.ListDelegatedAuthorities`,
  `principal_id` matching either side, `active_only` applying the same three
  conditions the evaluation path applies so the register cannot report a
  delegation as live that `/v1/authorize` treats as expired.

Both pass straight through `internal/cache` uncached, like the other `List*`
methods: they are catalogue reads for a console, not evaluation reads, so
caching them would put staleness in the one place an operator looks to confirm a
write landed and save nothing.

`internal/handler/list_catalogue_test.go` — 13 tests. The guards first (both
401 without a principal and without a tenant, and neither reaches the store),
then that the scope comes from the verified header and not a query parameter,
then that retired and revoked rows are included by default, then `[]`-not-`null`
on empty, then 503 on a store failure.

## The ABAC surface had shipped with no console

Eleven comparisons, two effects, retire/reactivate, and a table that
deliberately ships empty — authorable only by hand-crafted HTTP. Which means the
one layer that can silently refuse an action for every principal on the platform
was the layer with no UI.

The console now lists conditions **as sentences** rather than as five columns.
`PAYMENT_APPROVE / REQUIRE / amount / lte / 10000` is five facts that mean
nothing apart and one thing together, and assembling them was exactly the work
the reader cannot be expected to do. It reads: *"PAYMENT_APPROVE is refused
unless amount is at most 10000. A request that does not mention amount at all is
refused too."*

The form states the two properties that are surprising and undiscoverable: a
condition can only ever take access away, so it is never the fix for somebody
being unable to work; and a REQUIRE condition on an attribute no caller sends
refuses its action for everybody, immediately. A platform-wide condition offers
no retire button rather than offering one that reliably 403s — a control that
always fails teaches an operator to ignore refusals.

## Also wired

- **Role enforcement.** `active_flag` is what actually stops a role granting
  anything, and nothing could set it from the console. Shown next to
  access-control-svc's own catalogue on purpose: a role marked retired upstream
  while this flag stays true is a retirement that is a label and not a control,
  which is the defect `SetRoleActive`'s comment describes at length.
- **Delegations.** Lend and withdraw. No field for who is lending — the service
  refuses any delegation whose delegator is not the caller, so an input could
  only ever produce a refusal, and offering one would imply the platform lets
  one person hand out another's access. Withdraw is offered only to the lender,
  and never on a projected row, because neither can succeed elsewhere.
- **`explainAuthorizationError`** rewritten: all 23 error codes the handler
  emits, transport failures without a port number, `envelope_incomplete` ahead
  of the field branches, and a status fallback. It no longer returns the
  service's raw phrasing — where no wording fits it quotes the service as a
  quotation rather than presenting it as the answer. Its doc comment carries the
  rule that matters: a failure here is not a denial, and denials do not come
  through this function at all.

## Verified

- `go build` / `go vet` clean; `gofmt` clean on the three files touched.
- `go test ./...` green, including the store integration tests against real
  PostgreSQL and the 13 new handler tests.
- Console: `tsc --noEmit` and `eslint` clean across `lib/api/authorization.ts`,
  `app/admin/access-control/` and `components/admin/access-control/`.

**Manually tested** — see the section below, added the same day. The claim
that originally stood here ("not manually tested") is superseded: the service
was brought up and driven over HTTP, which found three defects the static
verification above could not.

## Still open after this pass

Unchanged from the second pass, and none of it is console work:

- **~86 of 111 `/v1/authorize` callers are refused with 401.** Re-measured this
  pass: 111 non-test Go files build a request to the endpoint, 25 send
  `X-Source-Channel`. The second pass floated a `MaterialWrite` override on this
  service's own `ServicePolicy` as the cheap fix — **it does not work.**
  `tenant_id`, `actor_subject_id`, `request_id`, `correlation_id` and
  `source_channel` are unconditionally mandatory and explicitly not expressible
  per service, so the override drops only `idempotency_key` and
  `legal_entity_id`; obligations-svc, which sends `Content-Type` and
  `X-Correlation-ID`, still 401s. The real options are an `ExemptPaths` entry
  for `/v1/authorize` (scoped and reversible, but it removes the traceability
  contract from the authorization path) or migrating the callers (correct, and
  ~86 services of edits). Left as the recorded decision it already is.
- **Tracker item 81 (two delegation stores)** — narrowed further by this pass
  rather than closed: the console can now read and write this service's
  register, which is the opposite of retiring its write API. Still a
  cross-service decision.
- **Tenant fallback in `/v1/authorize`**, **`internal/siem`'s inline-streaming
  shape in four other services**, **three unconsumed events with no producer**,
  and **unscheduled `access_decision_log` retention** — all as recorded above.

## Manual testing — 2026-09-08, same day

The third pass above shipped with "not manually tested" recorded against it.
That is now closed: the service was built, brought up on the compose stack
against real PostgreSQL 16 and real Kafka, and driven over HTTP. It found three
defects that static verification could not have.

Connected as `app_authorization`, confirmed `rolsuper=f rolbypassrls=f` before
relying on any isolation result — a superuser bypasses row security
unconditionally and would have made every scoping assertion vacuous.

### The envelope refusal, measured rather than inferred

The second pass reported this by sweeping source; here it is from the wire.
`POST /v1/authorize` with no envelope, and with exactly the headers
obligations-svc sends:

```
{"error":"envelope_incomplete","detail":"canonical input contract violated:
 actor_subject_id, idempotency_key, legal_entity_id, request_id,
 source_channel, tenant_id", ...}          → 401, both cases
```

Which settles the `MaterialWrite`-override question empirically: four of the six
violations — `actor_subject_id`, `request_id`, `source_channel`, `tenant_id` —
are the unconditional set, so an override that drops `idempotency_key` and
`legal_entity_id` still leaves a 401. **The cheap fix does not work.** Recorded
against the item rather than left as a plausible next step for somebody to
spend a day on.

### Defect 1 — the console sent attributes the service cannot decode

`authorizeRequest.Attributes` is `map[string]string`. The console's
`AuthorizeInput.attributes` was `Record<string, unknown>` and the evaluation
form parsed the operator's JSON and passed it through, so any check carrying an
attribute answered:

```
{"error":"invalid_json","message":"json: cannot unmarshal number into
 Go struct field authorizeRequest.attributes of type string"}
```

Every attribute-bearing check from the console was broken — which is to say the
entire ABAC path, the one layer the console had just been built to expose. It
type-checked cleanly because `unknown` is assignable to anything, and no test
covered it because nothing exercised the endpoint.

Fixed: the client type is `Record<string, string>`, and the action converts
numbers and booleans to strings rather than demanding the operator quote them —
`{"amount": 5000}` is what anybody would type. A nested object or list is
refused, because no comparison could match one. Proven after the fix, against
the live `CAP_10K` rule (REQUIRE amount lte 10000):

| attributes sent | outcome |
|---|---|
| `{"amount":"5000"}` | GRANTED `rbac:role=INITIATOR_…` |
| `{"amount":"50000"}` | DENIED `abac:require_failed=CAP_10K` |
| `{"amount":5000}` (pre-fix shape) | 400 `invalid_json` |

Which also confirms the struct comment's claim that the operators parse both
sides numerically: `"5000"` orders under 10000, `"50000"` does not.

### Defect 2 — two writes sent an id the service discards

`createSoDRuleRequest` and `createABACRuleRequest` have no id field, and the
handlers build their params without one, so a client-supplied id is silently
dropped and the store generates its own. Verified by sending a known id and
reading back a different one.

Both console calls sent one anyway — `sod_rule_id` pre-existing, `abac_rule_id`
added by the third pass. Harmless in effect, and a false claim in the code: it
implied these writes are idempotent under a client-chosen id. They are not, and
the two differ from each other:

- ABAC dedups on `rule_code` — a repeat answers 409 `abac_rule_code_conflict`.
  Confirmed.
- SoD dedups on **nothing**. Posting the same conflict pair twice stores it
  twice; the register listed two identical HARD rules on the same two actions.
  Confirmed.

Both payloads corrected, and both facts written into the call sites.

### Defect 3 — `sod_rules` had an off switch nothing could reach

The one found by testing that was a real hole in the service, not the console.

`active_flag` has been on `sod_rules` since `000001` and `CheckSoDConflict` has
always filtered on it (`WHERE active_flag`), so it was always the intended way
to stop a rule denying. No route could set it. Every sibling object had a
lifecycle — roles retire/reactivate, assignments and delegations revoke,
`abac_rules` gained retire/reactivate with `000010` — and this one, alone, could
only be created.

That asymmetry sat on the object with the widest blast radius. An SoD rule
denies its action to **every** principal holding the pair, from the next
decision, tenant-wide; a rule authored by mistake had no remedy through the API
at all. The same defect `SetRoleActive`'s comment describes for roles, left
unfixed where it mattered most — and this pass had just given the console a
button to author them with.

Added: `PgStore.SetSoDRuleActive`, the cache passthrough invalidating `nsSoD`
(a cached conflict would keep denying an action just stopped), and
`POST /v1/admin/sod-rules/{sod_rule_id}/retire|reactivate`. Idempotent, no
delete — the rule stays readable because a denial recorded as
`sod:conflict_with=<action>` is only explainable while its rule can be looked
up. `tenant_id = $3` with no IS NULL branch, so a platform-wide rule reads as
absent from a tenant's scope and cannot be switched off by one of the tenants it
binds. 7 handler tests in `sod_lifecycle_test.go`.

Proven at the evaluation layer, not just at the route:

| step | outcome |
|---|---|
| before any conflict rule | GRANTED `rbac:role=SODTEST_ROLE_…` |
| conflict rule authored | DENIED `sod:conflict_with=SODTEST_B_…` |
| **retired via the new route** | **GRANTED** — the denial is gone |
| reactivated | DENIED again |
| retired twice | 200, 200 — idempotent |
| retired from another tenant | 404 `sod_rule_not_found` |
| retired with no principal | 401 |

### Everything else, exercised and correct

Both endpoints the third pass added: `GET /v1/admin/roles` and
`GET /v1/admin/delegated-authorities` — guards 401 with no principal and with no
tenant and the store is not reached; a hostile `?tenant_id=` does not override
the verified header (8 roles returned, all ours); a different tenant header
returns 0 roles and 0 delegations; `active_only=true` narrows; retired and
revoked rows are included by default.

The evaluation chain: `no_grant` → assign → `rbac:role=` → SoD →
`sod:conflict_with=` → ABAC `abac:require_failed=`, each recorded and each read
back by `GET /v1/access-decisions/{id}`. A decision fetched from another tenant
answers 404, never 403.

Delegations: create honours a supplied id (that request struct DOES have the
field); lending another principal's authority is refused 403
`delegator_must_be_caller`; the register lists it; revoke 200; a second revoke
409 `already_revoked`, which the console maps to its own `alreadyWithdrawn`
state rather than an error.

ABAC admin: create, retire, reactivate, duplicate code 409, and an operator the
evaluator cannot run refused 400 `unsupported_operator` at authoring time. One
divergence found and kept: the service ACCEPTS a presence operator carrying an
unused `attribute_value` (201), where the console refuses it. The console's
message no longer claims the service refuses it — it says the value would be
stored and never read, which is the actual reason to refuse.

Every error code the console has wording for was seen on the wire except
`store_unavailable`, `authz_unavailable` and the transport branches, which need
a dependency taken down to reach.

### Test data

The compose database is shared and already held fixtures from the 2026-09-03
session. Everything this pass created was retired afterwards — 0 active SoD
rules and 0 active attribute conditions left behind by it. The cleanup filter
initially caught the pre-existing 2026-09-03 `PAYMENTS` conflict rule as well;
it was reactivated, and being able to reactivate it at all is the new endpoint
earning its place — before this pass that retirement would have been
unrecoverable through the API.

Roles, bundles, assignments and the delegation rows created during testing were
left in place: none of them denies anything, all are additive, and the register
reads them as ordinary test fixtures alongside the existing ones.

---

# Fourth pass — 2026-09-08

Closing the bounded items from "Still open, and why". Three of them; the two
cross-service decisions and the producerless events are untouched and still
recorded above.

## The SIEM shape, in the four services that vendor it

The second pass fixed `internal/siem` here and recorded that
gateway-auth-svc, identity-context-svc, key-management-svc and
mtls-management-svc "are presumably identical and were not touched". They were
identical — verified by diff: all four bodies are byte-for-byte the same below
the package comment, so the 850ms defect was in the shared shape exactly as
suspected.

All four now carry this service's implementation: bounded queue, four workers
delivering on their own background context, drops counted and logged, `Close`
draining on shutdown. `defer siemClient.Close()` wired into each `main` —
none of the four had it. The seven regression tests are ported to each,
including the two that pin the properties that matter
(`TestStream_DoesNotBlockTheCaller`, `TestStream_DoesNotUseTheRequestContext`).

What the per-service doc comments claimed, and what was true:

- **gateway-auth-svc** documented the blocking honestly and pushed the burden
  onto callers — "Call this from a goroutine if the caller is on a
  latency-sensitive path". Neither of its two call sites did. Both
  `session_risk.*` and `tenant_context.denied` ran inline on `r.Context()`, so
  an absent siem-integration-svc added its connect time to the gateway's own
  auth verdict. Advice in a doc comment is not a control.
- **identity-context-svc** claimed streaming "is always called from a goroutine
  already off the P99<50ms hot path ... never inline in Resolve()". True of two
  of its three call sites. The third — `authenticator.go`'s
  `credential.digest_unusable` — was inline on the authentication request path.
  The claim was true of the hot path and false of the package.
- **key-management-svc** and **mtls-management-svc** made no claim and had
  three inline call sites each, on `key.rotated` / `key.disabled` and
  `certificate.issued` / `.rotated` / `.revoked`. Disabling a key and revoking
  a certificate are incident-response actions; paying a telemetry sink's
  connect timeout to perform one is the wrong way round.

The redundant `go func(){...}()` wrappers in identity-context-svc are left
alone: Stream is non-blocking now, so they cost one goroutine and nothing else,
and restructuring another service's call sites from here is a wider change than
this needs.

## Retention — which did not work at all, not merely "not scheduled"

Recorded above as "the SQL function exists and is tested; nothing calls it on a
timer yet". The scheduling was the smaller half of the problem.

**Neither helper could be called by the service that needs them.** Both are
`LANGUAGE plpgsql` with no SECURITY clause, so they execute as the CALLER;
`access_decision_log` is owned by the migration role, and the service connects
as `app_authorization`, which `create-app-roles.sh` deliberately grants USAGE
and DML and **no DDL**. Measured against the running stack as that role:

```
create_access_decision_log_partition('2027-03-01')
  ERROR: permission denied for schema public
  QUERY: CREATE TABLE access_decision_log_2027_03 PARTITION OF ...

detach_access_decision_log_partitions_before('2026-10-01')
  ERROR: must be owner of table access_decision_log
  CONTEXT: ALTER TABLE access_decision_log DETACH PARTITION ...
```

000009's tests passed throughout because they run on the migration connection,
which owns the table — the same blind spot 000008 was written to close for row
security. A suite that only uses the migration role proves nothing about the
role the service uses. So the partition runway could only ever have been
extended by hand, and when the last pre-created month elapsed every decision
would have landed in the default partition: caught by 000009's design rather
than an outage, but silently, with the runway gone.

**Migration 000011** makes both `SECURITY DEFINER` with `search_path` pinned to
`public, pg_temp`, revokes EXECUTE from PUBLIC and grants it to
`app_authorization` by name where that role exists. The bodies are unchanged —
they were already safe to run this way, since every identifier goes through
`format(%I)` and both parameters are DATE-typed. Applied, reverted, re-applied
against the live database; the down migration genuinely reverts (the service
role is denied again).

**`internal/retention`** is the sweeper. It runs immediately on start and then
on `AUTHZ_RETENTION_SWEEP_INTERVAL_HOURS` (default 24), and each sweep:

1. **Extends the runway** — the current month plus `AUTHZ_PARTITION_MONTHS_AHEAD`
   (default 3). The current month is included deliberately: the case that
   matters is a service starting into a month nobody pre-created, where inserts
   are already going to the default partition, and extending from "next month"
   would leave that unrepaired. A failure on one month logs and continues; the
   near months are the ones that keep the service answering.
2. **Detaches what aged out** — cutoff is the first of the month
   `AUTHZ_ACCESS_DECISION_RETENTION_MONTHS` back. `0` is the documented off
   switch and disables only this half; anything negative is read as "do not
   detach", never "detach more"; and a floor refuses any cutoff reaching the
   current month.

Create runs before detach, always, and a detach failure never prevents a
create: if only one half ever runs it should be the one that keeps the service
answering. One replica sweeps at a time, on a session-scoped advisory lock —
creating is idempotent but `DETACH` is not, and two replicas detaching the same
partition means one of them errors on a maintenance path, which trains an
operator to ignore errors. A replica that cannot get the lock skips; the work
is idempotent and time-based, so the next tick covers it.

Nothing here issues a DELETE or a DROP. Detached partitions are logged by name
and row count with the follow-up stated — they have left the parent table, not
the database, and somebody archives and drops them deliberately.

11 unit tests in `internal/retention`, and both halves proven live:

| | |
|---|---|
| runway extended on startup | `months_covered:4, through:2026-12-31` |
| new partition's row security | `relrowsecurity=t relforcerowsecurity=t`, 1 policy |
| detach, 12-month window, aged partition present | `partitions:[access_decision_log_2024_01] cutoff:2025-09-01` |
| the detached table afterwards | `relkind=r`, no longer a partition — DETACH, not DROP |

## Console, exercised through a running browser session

The third pass's console work was static-only. It has now been driven against
the live service: dev server up, logged in as the admin account, and
`/admin/access-control` fetched and inspected — 200, ~302KB, no error boundary.

Every new section rendered. The plain-English labels are rendering from live
records ("In force", "Retired", "Live", "Withdrawn"), the attribute conditions
are rendering as sentences from `describeABACRule` — "is refused unless amount
is at most 10000", "is refused when channel is exactly fax" — and the role
catalogue is showing the roles created over HTTP during this session's testing,
which is what proves `GET /v1/admin/roles` is wired end to end through the
console rather than merely compiling. All eleven ABAC operators are offered.
The enforcement controls appear in the counts the database implies: four
retired conditions produce four "Start applying it again" and no "Stop applying
it", because none is active.

`RoleCataloguePanel` reported "access-control-svc could not be reached", which
is correct — only authorization-svc was running, and that panel degrading to a
labelled empty state instead of breaking the page is the behaviour it was
written for.

**Still not exercised: the Server Actions behind the forms.** Turbopack does
not put action ids in the server-rendered HTML, so the forms need a JS browser
and no browser automation was available here. The segment that remains untested
is thin — FormData parsing between a button and `lib/api` — and every wire
contract underneath it was verified directly against the live service, the
attributes shape included, in both its broken and fixed forms.

---

# Fifth pass — 2026-09-08

The four passes above built the service, hardened it, gave it a console, and
closed the bounded operational items. None of them asked what
`permission_bundles` — the table that holds what a role actually permits —
could do. It turned out to be the one object left with no read, no off switch,
and a destructive write that reported itself as a creation.

## `permitted_actions` was write-only

A role could be created, given permission sets, retired, reactivated and
assigned to anybody, and **nothing in the service or the console could read
what it granted.** `GET /v1/admin/roles` returns `role_code`, `role_name`,
`role_scope_type`, `active_flag`, `created_at`, `created_by_principal_id` — and
no actions. The table's only other reader is `FindGrantedActions`, which
answers the different question "what may THIS principal do in THIS entity".

So the console listed role **labels**. `role_code` is a name an operator chose;
the bundle is the control. An operator could see that a role existed and was
being enforced, and had no way to learn whether it permitted one action or
forty, or which — the who-can-do-what map was unreadable from the one page
built to show it.

It also made the write beneath it unsafe to use. The upsert dedups on
`(role_id, bundle_code)`, so a `bundle_code` could not be checked for collision
before writing, which is the next item.

`GET /v1/admin/roles/{role_id}/permission-bundles` +
`PgStore.ListPermissionBundles`. Tenant-scoped through the role FK **and**
`withRLS`, matching `000007`'s policy exactly — this table has no `tenant_id`
of its own, and the role's ownership is checked rather than trusted, so naming
another tenant's `role_id` returns `[]` and not its actions. Retired sets are
included and ordered last, for the reason `ListRoles` keeps retired roles: a
retired set is why an action a role used to grant is gone. 200-with-`[]` rather
than 404 for an unknown role, so the endpoint is not an existence oracle for
other tenants' role ids. Uncached, like the other `List*` methods.

## An off switch nothing could reach — the third time this shape has appeared

`permission_bundles.active_flag` has been on the table since `000001` and
**both** evaluation reads join through it — `FindGrantedActions`
(`JOIN permission_bundles pb ON ... AND pb.active_flag`) and
`FindDelegatedActions`. So a false flag genuinely withdraws every action the
set granted, from the next decision, from direct holders **and** from anyone
who borrowed the role through a delegation. No route, store method or console
control could set it.

This is exactly the defect the third pass found on `sod_rules` and the second
pass's `SetRoleActive` comment describes for roles — a flag wired into the hot
path with nothing able to reach it — arriving a third time, on the granting
side rather than the denying side.

What the workarounds cost, which is why neither is a substitute:

- **Retiring the role** withdraws every set it holds at once and suspends it
  for every principal assigned it.
- **Reposting the set with a shorter action list** destroys the record of what
  was withdrawn, because the write overwrites `permitted_actions`.

Neither can take back one set and leave the rest.

Added: `PgStore.SetPermissionBundleActive`, the cache passthrough invalidating
the grant **and** delegation namespaces (a cached grant would keep granting an
action just withdrawn — the same reason `SetSoDRuleActive` invalidates `nsSoD`),
and `POST /v1/admin/permission-bundles/{permission_bundle_id}/retire|reactivate`.
Idempotent, no delete — the set stays readable because a grant recorded as
`rbac:role=<code>` is only explainable while the actions that role held can
still be looked up. Scoped through the role FK with no `IS NULL` branch, so a
set belonging to another tenant reads as absent and 404s rather than 403ing.

Proven at the evaluation layer, not at the route — because the flag was always
in the query and what was missing was any way to set it, so the only assertion
that means anything is that the action stops being granted:

| step | ACTION_KEEP | ACTION_DROP |
|---|---|---|
| both sets live | GRANTED | GRANTED |
| the `drop` set retired | **GRANTED** | **DENIED `no_grant`** |
| reactivated | GRANTED | GRANTED |

`ACTION_KEEP` surviving is the half that proves it withdraws ONE set. A
separate test proves the same retirement withdraws the action through a
**delegation** of the role, since `FindDelegatedActions` joins the same flag —
a switch that stopped the direct grant and left the borrowed one would be
82a's failure mode in reverse.

## A destructive write that reported itself as a creation

`ON CONFLICT (role_id, bundle_code) DO UPDATE SET permitted_actions =
EXCLUDED.permitted_actions` replaces an existing action set **wholesale**, and
the handler answered **201** either way. So a repost that narrowed or emptied
what a role grants — taking those actions off every principal holding it from
the next decision — was indistinguishable from having created something.
Combined with there being no way to LIST sets, an operator could not discover
the code was taken beforehand or that it had been replaced afterwards.

`CreatePermissionBundle` now returns `(bundle, created, error)` and the handler
answers **201 created / 200 replaced**. `(xmax = 0)` is the discriminator —
zero on the INSERT path, the locking transaction on the DO UPDATE path —
verified against PostgreSQL 16 before being relied on, not assumed.

The upsert itself is deliberately **kept**: callers re-provision sets from a
declarative catalogue (access-control-svc is the governed authoring layer in
front of this API), so `DO NOTHING` or a 409 would break replay. What was
missing was the caller being told which happened. Same created/replayed shape
`CreateRole` already answers with.

**Checked for breakage before changing it**, since 200 where 201 was expected
is exactly how a status-code change breaks a caller:
`access-control-svc`'s `AuthzAdminClient.post` tests `resp.StatusCode >= 300`,
so 200 passes as 201 did. No other service calls this route, and the console
authors sets through access-control-svc rather than directly.

## Found by driving it: `decision_basis` named a role once per bundle

The grant query returns one row per (assignment × bundle) and appended
`role_code` per **row**, so a role holding two sets produced
`rbac:role=FINANCE_APPROVER,FINANCE_APPROVER`. Measured on the wire, not
inferred — no existing test had a role with more than one bundle, which is why
four passes missed it.

That field is the audit record of **why** an action was allowed, and the
console renders it verbatim beside its paraphrase precisely so an auditor cites
what the service holds rather than the console's wording. A role repeated once
per set is noise in the one place that has to be exact, and it also leaked how
many sets matched, which is not what the field claims to report. Deduped;
`TestPgStore_FindGrantedActions_BasisNamesEachRoleOnce` pins it, and was
verified to fail on the old line (`rbac:role=BASIS_ONCE,BASIS_ONCE`).

## Console

The sets render **under their role**, as sentences rather than as a boolean
column — `describePermissionBundle`. "In force. ACTION_APPROVE — 1 action
anyone holding this role may take, and anyone who has been lent this role may
take too." The retired wording is the one that has to be unmistakable, since a
retired set looks identical to a live one in a table: "Not in force.
ACTION_DROP — 1 action the role would grant, and does not, because this set has
been switched off."

Three states are distinguished that a naive rendering conflates:

- **A role with no sets** is called out, not left blank. It is the quiet
  failure this surface exists to expose: listed, shown as enforced, grantable
  to anybody, and confers nothing.
- **An empty set** says it grants nothing rather than showing "0 actions".
- **A set that could not be READ** says so, because "this role permits nothing"
  and "we could not read what it permits" are opposite statements and the
  second must never render as the first on a page about who can do what. Each
  role's read is settled independently so one failure does not blank the
  catalogue.

`BundleEnforcementButton` states the blast radius on its own hint rather than
in a tooltip, because the distinction from the role's "Stop enforcing it"
button is the whole point and the two otherwise read the same: this takes one
set's actions away and leaves the role's others working.

`permission_bundle_not_found` added to `explainAuthorizationError`, including
why another tenant's set reads as absent.

`describeBundleWrite` was written for the 201/200 distinction and then
**removed**: the console authors sets through access-control-svc, so it had no
call site, and the fact is carried in `createPermissionBundle`'s doc comment
instead. An uncalled helper is decorative.

## Verified

- `go build` / `go vet` clean. On `gofmt`, precisely: the two files this pass
  created are clean outright, and the seven it edited are reported by
  `gofmt -l` **only** because they carry CRLF line endings, which they did
  before this pass — each is byte-for-byte gofmt-clean once normalised to LF,
  checked one by one rather than assumed. Line endings deliberately left alone:
  converting them would put a whole-file diff on seven files to change nothing
  a compiler or reader sees.
- `go test ./...` green, **including** the store integration tests against real
  PostgreSQL 16 — which the previous passes' "green" did not always mean, see
  below.
- 13 handler tests (`internal/handler/permission_bundle_test.go`) and 10 store
  integration tests (`internal/store/permission_bundle_test.go`).
- Driven over HTTP against the rebuilt image on the compose stack: 22
  assertions, 0 failures — the three new routes, the 201/200 split, cross-tenant
  reads returning `[]`, cross-tenant flips 404ing, idempotent double-retire, 401
  with no principal and with no tenant, and the evaluation table above.
- Console rendered against the live service at `/admin/access-control` — 200,
  ~206KB, no error boundary — with the sets, the sentences, the counts and both
  badge states rendering from live records.

## Two things this pass got wrong, recorded rather than quietly fixed

**The store tests were skipping, not passing.** `TEST_DATABASE_URL` was unset,
so every integration test in this service `t.Skip`ped and `go test ./...`
reported `ok`. Four passes' worth of "full `go test ./...` green" therefore did
not mean the store suite ran. It does now, and it does pass — but the claim and
the evidence had drifted apart, which is the same class of thing as a suite
that only uses the migration connection proving nothing about row security.

**Running it against the compose database erased that stack's fixtures.**
`setupTestDB` DROPs every table this service owns and re-runs the migrations.
Pointed at the compose `authorization_svc`, it destroyed the roles, bundles,
assignments, delegations, SoD rules, attribute conditions and
`access_decision_log` history that earlier sessions left there. The **schema**
survived intact — migrations re-applied, all 7 tables back to
`relforcerowsecurity=t`, partitions re-created Sep→Dec 2026 plus the default by
the retention sweeper on boot, `000011`'s SECURITY DEFINER functions present —
so the service kept working. The data did not.

`setupTestDB` now carries that warning in its doc comment. Deliberately no
heuristic guard on the DSN: every candidate (refuse a database named
`authorization_svc`, require a name suffix, check the host) either blocks the
legitimate throwaway case or gives false confidence against a URL shaped
slightly differently, and a stated contract is more honest than a check that
can be satisfied by accident.

Also fixed while in there: `000011` was missing from that migration list — the
same omission the file's own comment records for `000007`. It passed either way
because the test pool connects as the owner, and an owner can create and detach
partitions with or without the SECURITY DEFINER grant, which is precisely the
blind spot `000011` exists to close.

## Still open after this pass

Unchanged, and all of it either a cross-service decision or producerless:

- **~86 of 111 `/v1/authorize` callers refused with 401** (tracker 82i). The
  `MaterialWrite` override was measured not to work in the third pass; the real
  options remain an `ExemptPaths` entry or migrating the callers.
- **Tracker 81 (two delegation stores)** — this pass widened the console's use
  of this service's register again, which is the opposite of retiring its write
  API.
- **Tracker 79 (role assignment duplicated with identity-context-svc)** — Not
  Started, and the delegation projection trick does not transfer; see the row.
- **Tenant fallback in `/v1/authorize`**, **three unconsumed events with no
  producer**.
- **The Server Actions behind the console forms** are still not exercised by a
  JS browser — no browser automation available here. Every wire contract
  underneath them was verified directly against the live service, this pass's
  three routes included.

---

# Sixth pass — 2026-09-09

One thing, the one that was blocking everything else: **`POST /v1/authorize`
now answers its callers instead of refusing them.** Tracker 82i, open across
three passes as "needs a decision", is closed — and it turned out not to need
the decision it was recorded as needing, because the reasoning that ruled out
the cheap fix was wrong.

## The cheap fix was rejected on an inference, and the inference was wrong

The third pass measured the refusal from the wire and then reasoned about the
remedy:

> Which settles the `MaterialWrite`-override question empirically: four of the
> six violations — `actor_subject_id`, `request_id`, `source_channel`,
> `tenant_id` — are the unconditional set, so an override that drops
> `idempotency_key` and `legal_entity_id` still leaves a 401. **The cheap fix
> does not work.**

The measurement was right; the inference from it was not. It assumed
`MaterialWrite` only selects which *conditional* fields `Validate` demands. It
also decides whether a violation is **refused at all**:

```go
if err := policy.Validate(e, r); err != nil {
    if enforced(mode, policy.materialWrite(r)) {   // <-- here
        writeViolation(w, err)
        return
    }
    ...
}

func enforced(mode Mode, isWrite bool) bool {
    switch mode {
    case ModeStrict:      return true
    case ModeWriteStrict: return isWrite     // <-- non-writes are not enforced
    default:              return false
    }
}
```

Under `write-strict`, a request classified as a non-write is admitted whatever
`Validate` found. So the override does not have to satisfy the unconditional
five — it takes `/v1/authorize` out of enforcement entirely, in that mode, and
only in that mode. A day was nearly spent on the 86-service migration on the
strength of a conclusion that could have been checked by reading fifteen lines
of the middleware it was about.

Recorded rather than quietly corrected, because the shape is worth keeping: the
pass measured the symptom on real infrastructure and then reasoned about the
cure on paper. This pass measured the cure too — see Verified below.

## The fix

`internal/handler/envelope_policy.go` — new, 70 lines, mostly the argument:

```go
func EnvelopePolicy() svcenvelope.Policy {
	p := svcenvelope.ServicePolicy()
	p.MaterialWrite = MaterialWrite
	return p
}

func MaterialWrite(r *http.Request) bool {
	if r.Method == http.MethodPost && r.URL.Path == AuthorizePath {
		return false
	}
	switch r.Method {
	case http.MethodGet, http.MethodHead, http.MethodOptions:
		return false
	default:
		return true
	}
}
```

and `cmd/server/main.go` passes `handler.EnvelopePolicy()` where it passed
`svcenvelope.ServicePolicy()`.

Three things about where it lives. It is **not** in `internal/envelope/
contract.go`: `services/_contract/rollout.sh` regenerates that file (`cp` of
five named sources plus a generated `contract.go`), so an override there
survives until the next rollout and then silently does not. It is **not** in
`main.go` either, though that is where the wiring goes, because then the tests
would exercise a copy of the policy rather than the policy. It is in the
handler package, next to `RegisterRoutes`, because what it encodes — which of
this service's routes change state — is a property of the route table. Adding a
route puts the question in front of whoever adds it.

## Why this is the right answer and not merely the cheap one

Both prior write-ups recorded a real objection to the override: it "weakens a
control on the platform's authorization path", and "the endpoint DOES write the
decision artifact, so *it changes nothing* is not quite true of it either."
Taking those in turn.

**On the decision artifact.** `/v1/authorize` appends to
`access_decision_log`. That row is the audit record *of the question*, not
business state. Replay protection is not merely unnecessary for it, it is
backwards: two identical questions must produce two log rows, because the log
is how "who asked what, when" is answered. An `Idempotency-Key` on an
evaluation would suppress exactly the second entry the audit trail needs. So
the endpoint writes, but not material state — which is the distinction
`RequiredOnWrite` exists to draw, and `Policy.MaterialWrite`'s own doc comment
names the case: "Override where a service exposes a POST that changes nothing —
a search or evaluate endpoint."

**On weakening the control.** Measured, not asserted: the relaxation reaches
one route.

- Every `/v1/admin/*` write is still refused without a full envelope — all
  twelve of them, checked individually.
- Under `ModeStrict` the endpoint is refused again. `enforced` ignores the
  classification in that mode, so this is not a hole strict mode has to undo:
  flipping `ZS_ENVELOPE_ENFORCEMENT=strict` re-closes it with no code change.
- What the override *does* change about the strict end state is correct on its
  own terms: `/v1/authorize` under strict requires the five unconditional §4
  fields and no longer demands `Idempotency-Key` or `X-Legal-Entity-Id` —
  neither of which an evaluation has anything to do with, and the entity is in
  the body where the handler already validates it.
- It is not silent. A violating call is admitted with
  `X-Envelope-Contract: violated` on the response and a `WARN` from the
  reporter naming the missing fields. The log is the migration list.

So this is a migration state with a defined end, not a permanent exemption —
which is what `ExemptPaths` would have been, and why it was not used. That
field is for endpoints that *produce* the mandatory fields (gateway-auth-svc's
`/verify` derives tenant and principal from a signed token); `/v1/authorize`
consumes a tenant scope, so a blanket bypass would also have dropped
`request_id` and `correlation_id` from the one path where losing the trace
costs most.

## What this unblocks, and the part that is now live for the first time

86 of 111 callers were failing closed on a 401 they reported as an
authorization failure. They now get real decisions. That includes
accounts-payable, general-ledger, workflow-svc, every `payment-*` and
`privacy-*` service, tenant-entity-registry-svc and identity-context-svc.

It also makes live a set of paths that three passes have described as correct
but not reached: `resolveTenantScope`'s body-then-nothing fallback, the store's
`$3 = ''` tenantless fallbacks, and migration `000008`'s platform-scope hatch on
`delegated_authorities`. Those were built for exactly this caller — one that
arrives with no `X-Tenant-Id` — and the middleware was refusing it one layer
up, which is why 82a's fix (2) was previously reachable only in observe mode.
The body-tenant path is exercised below and resolves the correct tenant.

Note the consequence for the fallback item that has been carried as open since
the second pass: it was kept so a caller with no `X-Tenant-Id` would not be
refused, then recorded as protecting nobody because the middleware refused that
caller anyway. It now protects the callers it was written for. It stays, and the
reason is no longer moot.

## Verified

`go build`, `go vet`, `gofmt` clean on both changed files; full `go test ./...`
green.

Nine new tests in `internal/handler/envelope_policy_test.go`, which mount the
real middleware with `handler.EnvelopePolicy()` — the other handler tests
deliberately omit the middleware and send no headers, so none of them could
have caught this. They cover: the no-envelope call admitted and flagged; the
obligations-svc header shape admitted; a conformant caller admitted and *not*
flagged; all twelve admin writes still refused with `envelope_incomplete`;
strict mode still refusing; strict mode not demanding an idempotency key;
strict mode still demanding tenant; and the classifier itself, including
`POST /v1/authorize/something` which is not the evaluation endpoint.

**On the wire**, against the real service on :8089 — postgres 16 and Kafka up,
`DB_USER=app_authorization`, deliberately only those three containers:

| call | before | now |
|---|---|---|
| `POST /v1/authorize`, obligations-svc's exact headers | 401 | **200** `GRANTED` `rbac:role=TEST_ROLE`, flagged `violated` |
| `POST /v1/authorize`, no headers at all | 401 | **200** `GRANTED`, flagged `violated` |
| `POST /v1/authorize`, full envelope (policy-svc's shape) | 200 | **200**, and *not* flagged |
| `POST /v1/authorize`, action the principal does not hold | — | **DENIED** `no_grant` — fail-closed intact |
| `POST /v1/admin/roles` and the other 11 admin writes, no envelope | 401 | **401** `envelope_incomplete` |
| `GET /v1/admin/roles`, tenant + principal | 200 | **200** |

Tenant resolution on the newly-live path: the body-tenant calls wrote
`access_decision_log` rows carrying the correct
`tenant_id=11111111-…-111111111111` and `principal_id`, so the fallback
resolves rather than merely admitting.

And the strict end state, measured on a second container run from the same
image with `ZS_ENVELOPE_ENFORCEMENT=strict` (stopped afterwards):

| call under strict | result |
|---|---|
| `POST /v1/authorize`, no envelope | **401** — re-closes, as claimed |
| `POST /v1/authorize`, the five unconditional fields only | **200** `GRANTED` |
| `POST /v1/admin/roles`, the same five fields | **401**, missing exactly `idempotency_key, legal_entity_id` |

That last row is the write/non-write distinction doing its job on two routes of
the same service in the same request shape.

## Still open after this pass

82i is closed as a refusal. What remains of it is the migration it was
blocking, and it is now a cleanup rather than an outage:

- **86 callers still send no envelope.** They work, and every call is logged
  `WARN canonical input contract violated` with the missing fields, so the list
  is generated rather than swept for. The doctrinal end state is still to
  migrate them and set `ZS_ENVELOPE_ENFORCEMENT=strict`; until then the
  decision log records those calls without caller attribution.
  `services/policy-svc/internal/authz/client.go` is the reference
  implementation — it lifts the inbound envelope off the request context with
  `svcenvelope.FromContext(ctx)` and forwards it, so no client signature has to
  change. This is 86 services of small, identical edits, and out of scope here
  by instruction.
- **Tracker 81 (two delegation stores)**, **tracker 79 (role assignment
  duplicated with identity-context-svc)** — unchanged, cross-service decisions.
- **Three unconsumed events** — `role.assigned`, `employment.changed`,
  `entity.scope.updated` — still have no producer.
- **The Server Actions behind the console forms** are still not exercised by a
  JS browser; no browser automation here. Every wire contract underneath them
  has been verified directly.
- **The store integration suite did not run in this pass.** `TEST_DATABASE_URL`
  was left unset deliberately, because `setupTestDB` DROPs every table the
  service owns and the only database to hand was the compose stack's
  `authorization_svc` with the fixtures these wire tests depend on. `go test
  ./...` prints `ok` for `internal/store` regardless, which is why this is
  written down: that `ok` means "skipped", not "passed". Use a throwaway
  database.


# Seventh pass — 2026-09-09

## The blocker the sixth pass applied, and the caller it left in the cold

The sixth pass made every `/v1/admin/*` write require the §4 envelope. It did
not break the console — `lib/api/envelope.ts`/`client.ts` in the frontend and
`access-control-svc/internal/clients/authzadmin.go` already send the canonical
headers. It broke the one remaining caller that did not: the demo RBAC seeding
script.

`seed-demo-rbac.ps1` predates the enforcement, sends no envelope, and every one
of its writes (role create, 26 bundle creates, 4 role-assignments) was answered
`401 envelope_incomplete`. The script kept running, kept failing into its
catch blocks, and reported the result honestly at the end:

    Seeding demo RBAC via http://localhost:8089 — 102 of 82 actions missing: ...

So a fresh stack — this one included — had a console wired correctly and a
service answering its reads, with *nothing seeded behind it*: every gated
service write refused `DENIED / no_grant`, the exact "console can read and
write nothing" state the script's own header describes. This is why the
enforcement and the seeding could not coexist: each half was done, and the
join was missing.

## The fix

`deployments/scripts/seed-demo-rbac.ps1` now builds the §4 envelope in
`New-AuthzEnvelope` and `Invoke-Authz` attaches it with `-Headers`. The shape
mirrors what the console and access-control-svc send:

- every request: `X-Tenant-Id`, `X-Principal-Id`, `X-Legal-Entity-Id`,
  `X-Correlation-ID`, `X-Request-Id`, `X-Source-System=console-seed`,
  `X-Source-Channel=api`;
- material writes add `Idempotency-Key` (fresh GUID per call — the seeding
  stays idempotent at the domain level, on primary keys, exactly as before),
  `X-Occurred-At`, `X-Operation=admin_seed`;
- the `/v1/authorize` probes are classified as non-writes and omit
  `Idempotency-Key`, matching the sixth pass's classification of that route.

The probes now also send `X-Tenant-Id`, so `resolveTenantScope` evaluates
against the demo tenant instead of the global-only fallback — closer to what a
real caller sends. The decision *scope* is unaffected: `Authorize` reads
`legal_entity_id` from the body, not the header (verified in `handleAuthorize`),
so the platform-scope probes still evaluate on the platform identity.

## Verified — the run it has been failing since enforcement shipped

First run, against the live `authz-e2e` container on :8089 (postgres 16 +
Kafka up, no gateway): role `CONSOLE_DEMO_OPERATOR` created, all 26 bundles
`201`, all 4 assignments `201` (legal entity, tenant, platform scope, plus the
SoD approver), and the full verification loop green on all three scopes.

Second run, same volume: fast-path — "Already granted — holds all 82 actions on
… and the platform scope", with the whole verification suite re-run green:

    Done. The console can write to all 25 wired services.

That final line is the state every downstream service now actually sees via
`POST /v1/authorize`: `GRANTED (rbac:role=CONSOLE_DEMO_OPERATOR)` everywhere it
was previously `DENIED / no_grant`.

## Still open after this pass

The seed-path blocker is closed. Everything else is the same cross-service
list, unchanged:

- **86 callers still send no envelope** — the migration out of scope here.
- **Tracker 81 (two delegation stores)**, **tracker 79 (role assignment
  duplicated with identity-context-svc)** — unchanged, cross-service decisions.
- **Three unconsumed events** — `role.assigned`, `employment.changed`,
  `entity.scope.updated` — still have no producer.
- **`DELEGATION_ADMINISTER`** is enforced but deliberately not seeded: the
  script's header keeps it out of the demo bundle ("handing it to the demo
  principal would restore exactly the escalation this pass closed") and the
  console's `explainDelegationError` relies on a delegation administrator that
  a stock demo stack therefore cannot name. A deliberate gap, recorded — close
  it by seeding a separate delegation-admin bundle to a second principal when
  that principal exists to be realistic.
- **The store integration suite still does not run** — the same
  `TEST_DATABASE_URL` caveat as pass six, unchanged.
---

# Eighth pass — 2026-09-10

The pass that was asked to close the service end to end, so this one is
structured as an audit against Doc 03 §8.3 rather than around a single defect.
Five things were found. Two of them were errors in this file.

## The two things this file got wrong

**1. Three "unconsumed events with no producer" have producers.** Carried since
the first pass and repeated in every "Still open" list since, including the
seventh: `role.assigned`, `employment.changed` and `entity.scope.updated` "have
no producer anywhere on the estate", so consuming them "would still be dead
infrastructure".

That conclusion came from grepping the **spec's** event names. The platform
publishes all three concepts under concrete names, and a grep for those finds
them immediately:

| §8.3 name | what is actually published | topic |
|---|---|---|
| `role.assigned` | `role.created`, `role.updated`, `permission.bundle.updated` — access-control-svc | `zoiko.access-control.events` |
| `employment.changed` | `principal.status.changed` — identity-context-svc | `zoiko.identity.events` |
| `entity.scope.updated` | `entity.status.changed`, `entity.hierarchy.changed`, `entity.jurisdiction.changed` — tenant-entity-registry-svc | `zoiko.entity.events` |

This is the same shape as the sixth pass's `MaterialWrite` error, and worth
naming as such: a measurement was taken correctly and then the *inference* from
it was made on paper. There, a violation list was read as ruling out an override
without reading the fifteen lines of middleware that decide refusal. Here, a
spec's vocabulary was searched for instead of the producers' code being read.
Both times the answer was one grep away from the thing already being looked at.

The error is left in `internal/events/consumer.go`'s comment with the correction
underneath it rather than edited out, because the shape recurs.

**2. `employee.terminated` is the one candidate that genuinely cannot be
consumed, and the reason is worth recording** rather than leaving as a silent
omission. Both employee-master-svc and offboarding-severance-svc publish it, and
its payload names an `employee_id`. There is **no employee-to-principal mapping
anywhere on this estate** — employee-master-svc's schema carries no
`principal_id` or `user_id` column at all. So the event says somebody left and
this service cannot tell whose authority that is about. Consuming it on a guessed
join would end the wrong principal's grants, silently, on the platform's
authorization plane. That is an identity-mapping gap, not a missing consumer, and
it is now recorded in `known-gaps.md` as the former.

## Layer 0 — a suspended principal could do everything they held

§8.3's first sentence is "determines whether a principal may execute a specific
action". A principal identity-context-svc has SUSPENDED or DISABLED may execute
nothing, and this service had no way to know that: their grants resolved exactly
as before.

The obvious objection is that identity-context-svc already evicts SESSIONS on
`principal.status.changed`, so a suspended human cannot log in. True, and not the
same control. This endpoint is called east-west, service to service, 111 callers
deep, on requests whose identity envelope was resolved *before* the suspension,
and by scheduled and queued work that carries a principal and no session at all.
Session eviction closes the front door; nothing closed this one.

`principal_status_projection` (migration `000013`) is the read-model, and
`/v1/authorize` consults it as **layer 0**, before RBAC. Four decisions in it are
load-bearing:

- **A projection, not a revocation.** The tempting implementation is to end the
  principal's role assignments on suspension. SUSPENDED is reversible and
  `effective_to` is not — ending them would mean an unsuspension restored
  nothing and somebody would have to reconstruct by hand what the principal used
  to hold. Verified on the wire: after suspend → reinstate, the principal's five
  assignments were untouched and every grant resolved again.
- **Absence means ACTIVE.** The table ships empty and changes no outcome until a
  status event arrives — the same shape `abac_rules` ships in. This is the one
  fail-OPEN default in a service that fails closed everywhere else, and it is
  correct here: the alternative denies every principal on the platform until a
  status event happens to arrive for them, which is a total outage of the
  authorization plane on first deploy. The gate can only ever take access away
  from a principal an authoritative service has explicitly said is not active.
- **A missing table also answers ACTIVE.** `FindPrincipalStatus` matches
  SQLSTATE 42P01 and returns ACTIVE with a WARN. Without it, reverting `000013`
  on a live service would 503 every authorization call on the platform — a
  rollback that takes the authorization plane down is not a rollback. There is a
  test that drops the table and asserts both code paths stay inert.
- **The tenantless caller is gated too.** 86 of 111 callers send no
  `X-Tenant-Id`. A gate that only applied to the other 25 would be escapable by
  omitting a header, so a tenantless read runs under `app.platform_scope` and
  takes the **most restrictive** row — `ORDER BY (status <> 'ACTIVE') DESC`.
  Verified: with the principal suspended, both the tenant-scoped and the
  tenantless call answered `DENIED principal_status:SUSPENDED`.

The denial is **recorded and published** like every other outcome, through a
`recordAndAnswer` extracted for the purpose — a suspended principal being
refused is exactly the evidence §8.3 asks for. It is deliberately not prefixed
`sod:`, because that prefix is what makes the handler publish
`sod.violation.detected`, and a suspended principal is not a duty conflict.

## "Denials must be evidentially retrievable" was not true

§8.3 sets two evidence obligations. "Every decision logged with actor, action,
basis, and outcome" has held since `000001`. "Denials must be evidentially
retrievable" had not, and the gap was not in the schema — it was that
`FindAccessDecisionByID` was the only read.

Retrieval by primary key is retrieval only for somebody who already holds the
key, and a denial's key exists in exactly one place: the response handed to the
service that was refused. So auditing a denial began by reading the **calling**
service's logs to find a UUID to bring back to this one. The console's own
lookup box said so in its hint text: "take it from the answer a check gave you,
or from the service that was refused — the reference is in its logs."

`GET /v1/access-decisions` closes it — filterable by principal, outcome, action,
entity and date window, keyset-paginated newest-first.

**Keyset, not offset, and that is not a preference here.** This table takes one
row per authorization evaluation platform-wide. OFFSET makes the database walk
and discard every skipped row, and — worse — the log is append-only and
constantly appended to, so a row inserted during paging shifts every subsequent
offset and an auditor silently never sees one decision while seeing another
twice. There is an integration test that inserts a decision between page one and
page two and asserts page two is unmoved.

`access_decision_id` is the tie-breaker because `decided_at` is not unique: 111
services call this concurrently and two decisions can share a timestamp.

**Migration `000012` is an index SWAP, not an addition.** `000009`'s
`(tenant_id, decided_at DESC)` is a strict prefix of
`(tenant_id, decision_outcome, decided_at DESC)`, so the old one is dropped.
Index count matters more on this table than anywhere else on the platform —
every index is maintained on the hottest write in the estate — so adding a fifth
to buy an audit query would have charged every service's write latency for a
read that runs when an auditor asks. Replacing a prefix is count-neutral.

## The other three §8.3 inbound APIs had no surface

§8.3 lists five inbound APIs. Three were folded into `/v1/authorize` as internal
layers and recorded as a deliberate simplification on the grounds that "the
capabilities exist, just not as separate HTTP surface".

The capabilities do exist. What folding them cost is three questions that
`/v1/authorize` **structurally cannot answer**, each one asked *before* a
material action rather than about one:

- **`POST /v1/entity-scope/validate`** — "which of these companies may this
  principal act in?" Through `/v1/authorize` that is one call per (entity,
  action) pair *and*, because every evaluation records its artifact, N rows in
  `access_decision_log` for a question nobody acted on. A console listing twelve
  entities would write twelve decision artifacts to grey out four buttons.
- **`POST /v1/sod/validate`** — "would granting this role to this person breach
  separation of duties?" A breach is a **combination**, so `/v1/authorize` can
  only see one after it already exists. The only way to discover that a role
  must not go to somebody was to grant it and then watch every use of it be
  refused: the control working and the workflow failing, leaving a live
  assignment that confers nothing and no explanation until somebody read a
  decision log. It also checks the candidates against **each other**, because a
  bundle carrying both halves of a pair grants both to everyone who holds the
  role and then refuses them both.
- **`POST /v1/delegated-access/evaluate`** — "is this principal acting on their
  own authority or somebody else's?" `/v1/authorize` collapses both into one
  GRANTED and names RBAC as the basis when both apply, so a principal who holds
  an action *both* directly and by delegation reads as pure RBAC. For a
  four-eyes step that has to know whether the delegate or the delegator
  satisfied it, that is the whole question — `held_directly` is the field that
  answers it.

**None of the three records a decision artifact**, and that is the point rather
than an omission. `access_decision_log` means "one row per authorization of a
material act", and that meaning is what an auditor reads it for. All three
therefore require a verified principal and tenant, on the same footing as the
admin reads, and all three log what was asked — authenticated and logged rather
than unauthenticated and recorded as evidence of something that did not happen.

All three are also registered in `handler.evaluatePaths` so `MaterialWrite`
classifies them as non-writes. A question-answering POST missing from that map is
classified as a material write and its callers are refused 401 for want of an
`Idempotency-Key` on a question — which is precisely tracker 82i, undiagnosed for
three passes. The map exists so adding a route puts the question in front of
whoever adds it.

## A mistyped scope reported the authorization plane as down

Found while auditing the Postman collection, which documented it as something to
live with: *"A non-UUID legal_entity_id returns 503 store_unavailable, not 400 —
that is a service defect, not your mistake."*

`known-gaps.md` had it too, under **Resolved**, with the remedy assigned to the
callers: *"callers must therefore validate the scope themselves before asking"*.
That is the wrong place. The workaround has to be written 111 times, each copy
has to know which of this service's columns are `uuid`, and any caller that
forgets reports an outage instead of a typo.

Reproduced as the service's own role (`rolsuper=f rolbypassrls=f`), both halves:

```
SELECT count(*) FROM principal_role_assignments WHERE legal_entity_id = 'oops'::uuid;
  ERROR: invalid input syntax for type uuid: "oops"

SELECT set_config('app.tenant_id','acme',false);
SELECT count(*) FROM roles;
  ERROR: invalid input syntax for type uuid: "acme"
```

The second is the one that was easy to miss and is the worse of the two:
`withRLS` installs the raw value into `app.tenant_id` and the **policy** does the
`::uuid` cast, so a malformed tenant does not fail in a query this service wrote
— it fails inside row security, on a plain `SELECT`, on every table. The store
wraps it as `ErrStoreUnavailable` and the handler answers 503.

Now refused with `400 invalid_scope` naming the field, in three places that
between them cover every route taking a scope: `resolvePlatformScope` (the four
evaluate/validate routes), `resolveTenantScope` (the `/v1/authorize` header and
body-fallback paths, each naming the field the caller actually set), and
`requireTenant` (every admin read).

**`principal_id` is deliberately not validated**, and that is the half it would
be wrong to get wrong. It is `TEXT` in every table and compared as text, so a
malformed one is a valid comparison that matches nothing — validating it would
refuse the service-account ids this service has never required to be UUIDs.
There is a test asserting a non-UUID principal is still evaluated.

**A missing tenant is still 401, not 400.** "You did not identify your
organisation" and "the organisation you named is not a reference" are different
facts.

### What this exposed about the test suite

The change broke 69 assertions across ten handler test files, all of which sent
tenants like `"tenant-1"` and entities like `"le-1"`. Those tests only ever
passed because they use stub stores that never touch Postgres: **every one of
them was asserting behaviour against a value the real service answers 503 to.**
That is the same class of thing this file has recorded twice already — a suite
that only uses the migration connection proving nothing about row security, and
an `ok` that meant "skipped". Swept to real UUIDs, so the suite now exercises
inputs the service can actually accept.

## The Postman collection did not work at all

`docs/postman/ZoikoSuite_Authorization.postman_collection.json` describes itself
as "every endpoint on authorization-svc, ordered as a runnable flow". Neither
half was true.

**It was broken.** Every request sent only `Content-Type` and
`X-Correlation-ID`. The sixth pass made the envelope middleware enforce on every
`/v1/admin/*` write, so **all fourteen writes in the collection answered 401
`envelope_incomplete`**, and both decision reads omitted the `X-Principal-Id` /
`X-Tenant-Id` the handler requires and answered 401 as well. The seventh pass
found and fixed exactly this shape in `seed-demo-rbac.ps1`; the collection is
the same caller class and was missed.

**It was incomplete.** 9 of 31 routes. Missing: the entire ABAC surface, every
admin read, every retire/reactivate, and everything added this pass.

Fixed with a collection-level pre-request script — one definition that every
request inherits, rather than seven headers pasted onto each of 78 items — which
also encodes the write/non-write distinction: `Idempotency-Key` is attached to
material writes and withheld from the four evaluate/validate POSTs, because
replay protection on a question is backwards. Now 13 folders, 78 requests, and a
programmatic diff against `RegisterRoutes` confirms **31 routes in the service,
31 in the collection, none missing, none spurious**.

## Two silent-failure modes in the Kafka wiring

Found by driving the new consumer rather than by reading it.

**A consumer group joined, was assigned nothing, and said nothing.** On the
first run the lifecycle group reported `Stable`, one member, **zero partitions**,
and consumed nothing — while the service log said only "lifecycle consumer
started". A second run under a different group id was assigned all three
partitions immediately, from the same image against the same broker, which is
what identifies it as a join-time race rather than a misconfiguration.

Two independent causes, both fixed:

- kafka-go reports consumer-group problems **only** through `Logger` and
  `ErrorLogger`, both of which default to nil and **discard** them. The `Run`
  loop cannot help: it logs errors returned from `ReadMessage`, and this failure
  returns nothing at all — `ReadMessage` simply blocks forever on a member with
  no assignment. So a consumer doing nothing looked exactly like a consumer with
  nothing to do. `ErrorLogger` is now wired on both readers.
- `WatchPartitionChanges` defaults to **false**, so a member assigned nothing
  never re-checks: it heartbeats happily and the broker reports a healthy group
  forever. Now `true` with a 30s interval on both readers, which bounds a silent
  stall at half a minute instead of until somebody restarts the pod. Verified by
  re-running under the **same group id that had been stuck**: 3 partitions
  assigned.

On this service that failure mode means principal suspensions silently never
take effect — the one thing layer 0 exists to prevent.

## The console

`/admin/access-control` gains two panels, and `explainDecisionBasis` gains the
branch it needed: without it every layer-0 denial rendered as "Reason not
recognised", which is the least useful possible answer to "why was this person
refused" when the answer is "their account has been stood down". A basis form the
service emits and that function does not match falls through to a safe but
useless fallback, so the two have to move together.

- **The decision history** — filterable, paged, one row per check with the
  plain-English reason and a "Why?" disclosure that renders the full rationale
  through the same `AccessDecisionSummary` the by-reference lookup uses, so one
  decision cannot be explained two different ways depending on how it was found.
  The empty state says at length that an empty result is **not** "nothing
  happened": checks from services that do not yet identify their organisation
  are recorded without one and are deliberately unreadable from inside one, and
  somebody could otherwise close an investigation on that difference.
- **The pre-flight conflict check** — reports the two conflict shapes separately
  because the remedies differ (withdraw something the person holds, versus split
  the bundle), and states that it records nothing, in contrast to the evaluate
  form directly above it which records every question it asks.

The read/advisory actions live in a new `audit-actions.ts` rather than the
1243-line `actions.ts`: everything in that module authors something, and its
states and error handling are shaped by "did the write land".

## Verified

`go build`, `go vet`, `gofmt` clean. Full `go test ./...` green.

**The store integration suite ran for real** — 62 tests, **0 skipped**, against
PostgreSQL 16 in a throwaway database created for the purpose and dropped after.
This is the item pass five recorded as "that `ok` means skipped, not passed" and
pass six and seven carried forward unchanged. The compose stack's
`authorization_svc` was checked before and after and its fixtures are intact (5
roles, 1 SoD rule, 548 decisions) — `setupTestDB` DROPs every table it owns, and
pointing it at the compose database is what erased that stack's fixtures on
2026-09-08.

Migrations `000012`/`000013` applied to the live database, then `down`, then
re-applied; the `down` pair verified to leave the projection dropped, `000009`'s
index restored and the new one gone.

**Supabase mirror `0036` applied for real**, not read: against a purpose-built
fixture standing in for `0001`–`0035`, then applied a **second** time to prove
idempotence. End state asserted — new index present, old one dropped, index
propagated to the partition, forced row security on the projection, the partial
index carrying its `WHERE status <> 'ACTIVE'`, one policy, and
`PRIMARY KEY (tenant_id, principal_id)`. The schema-absent guard was tested on an
empty database and answers a NOTICE, not an error.

On the wire, against the live service (postgres + kafka + authorization-svc
only):

| call | result |
|---|---|
| `POST /v1/authorize`, no envelope | **200** GRANTED |
| `POST /v1/authorize`, malformed `legal_entity_id` | **400** `invalid_scope` (was 503) |
| `POST /v1/authorize`, malformed tenant | **400** `invalid_scope` (was 503) |
| `POST /v1/authorize`, non-UUID principal | **200** — evaluated, not refused |
| `POST /v1/authorize`, `legal_entity_id=PLATFORM` | **200** |
| `GET /v1/access-decisions` | **200** |
| `GET /v1/access-decisions?decision_outcome=DENIED` | **200** |
| `GET /v1/access-decisions`, malformed tenant | **400** |
| `GET /v1/access-decisions`, no tenant | **401** |
| `POST /v1/entity-scope/validate` (batch incl. PLATFORM) | **200** |
| `POST /v1/sod/validate` | **200**, conflict on both halves of a real rule |
| `POST /v1/delegated-access/evaluate` | **200**, `held_directly` correct |
| `POST /v1/sod/validate`, malformed entity | **400** |
| all 12 `/v1/admin/*` writes, no envelope | **401** `envelope_incomplete` |

Layer 0 driven by a **real Kafka event** on `zoiko.identity.events`:

| | |
|---|---|
| before | `GRANTED rbac:role=TEST_ROLE,CONSOLE_DEMO_OPERATOR` |
| after SUSPENDED, with tenant | `DENIED principal_status:SUSPENDED` |
| after SUSPENDED, **no** tenant (the 86-caller shape) | `DENIED principal_status:SUSPENDED` |
| after ACTIVE | `GRANTED` — same basis as before |
| assignments touched | **none** (5, unchanged) |

The grant-graph half, also on real events: `permission.bundle.updated` and
`entity.status.changed` each invalidated their tenant's cached grants;
`role.updated` with no tenant invalidated across every tenant at WARN;
`employee.terminated` was correctly ignored.

**The console driven in a real browser** — Chromium via Playwright, signed in as
the admin account. This closes the item five passes have carried as "the Server
Actions behind the console forms are still not exercised by a JS browser":
Turbopack does not put action ids in the server-rendered HTML, so the forms
cannot be submitted by curl and the segment between a button and `lib/api` had
never run. Sixteen checks, all passing — route renders (156KB, no error
boundary); the search action returns "25 checks, 25 refused. There are older
ones." with real rows, plain-English reasons and a "Why?" disclosure that adds
1368 characters of rationale; the inverted-window error path renders a readable
refusal; the pre-flight check reports the conflict, names both halves and states
the remedy; the person-without-company guard is handled; the evaluate form
returns a decision; zero page or console errors.

Worth recording about that script: its first version reported two failures
against features that work. Both assertions read `document.body.innerText` on a
route that is 150KB of prose about "permission checks", so the wait matched
static copy before the action had resolved. Scoped to the panel, both pass. A
test that fails for its own reasons is worse than no test, because the next
person spends their time on the wrong half.

## Still open after this pass

Nothing left inside this service. What remains is cross-service, and each item
is now blocked on something nameable rather than merely unfinished:

- **86 callers still send no envelope.** They work, every call is logged with the
  missing fields, and the doctrinal end state is to migrate them and set
  `ZS_ENVELOPE_ENFORCEMENT=strict`. Until then those decisions are recorded
  without caller attribution — and, note, are therefore invisible in the new
  audit read, which is tenant-scoped. `services/policy-svc/internal/authz/client.go`
  is the reference implementation. 86 services of small identical edits.
- **`employee.terminated` cannot be consumed** until something on the estate maps
  an `employee_id` to a `principal_id`. Not a missing consumer — a missing
  mapping. Recorded in `known-gaps.md`.
- **Tracker 81 (two delegation stores)** and **tracker 79 (role assignment
  duplicated with identity-context-svc)** — unchanged cross-service ownership
  decisions.
- **`DELEGATION_ADMINISTER` is enforced but deliberately not seeded** — as the
  seventh pass recorded. Close it by seeding a delegation-admin bundle to a
  second principal when one exists to be realistic.

---

# Ninth pass — 2026-09-10

Triggered by being asked what percentage of the service is implemented. Trying
to answer that with a number rather than a claim required a measurement, and
the measurement found two things the eighth pass had reported as done.

## What the measurement is

`FE client` and `Console reaches` are different questions, and conflating them
is how a percentage stops meaning anything. A route can have a typed client
function and no button. So: parse `RegisterRoutes` for the real route table,
parse `lib/api/authorization.ts` for which routes have a client, then grep the
console for whether anything actually CALLS that client.

Result, after the fixes below:

| | |
|---|---|
| Doc 03 §8.3 checklist | **25/25** |
| Backend route table | 28 routes |
| FE client covers | **28/28** |
| Console reaches | **27/28** |

The one route with no direct console UI is `POST /v1/admin/roles`, and that is
correct: access-control-svc owns role DEFINITIONS and forwards each write here,
so the console defines roles through `createRoleDefinition` and this route is
exercised on every one of those, one hop along. Calling it directly would
create a role in the evaluation plane with no definition behind it — enforced,
assignable, and absent from the register an auditor reads. `createRole` is
therefore the one deliberately-uncalled export in that module, and its doc
comment now says so, because an unused export where everything else is wired
reads as something somebody forgot.

## Two of the §8.3 routes had a client, a describer, and no UI

The eighth pass built `validateEntityScope` and `evaluateDelegatedAccess`, wrote
`describeEntityScope` and `describeDelegatedAccess` to render their answers, and
then built console panels for only two of the three pre-flight routes. Nothing
called either function. The pass reported the §8.3 surface as complete, and on
the service side it was — but two of the three questions it had just made
answerable could not be asked from the console.

`components/admin/access-control/ReachChecks.tsx` closes it, as one card with
two forms — they are the same question from two directions ("where can this
person reach, and on whose authority") and an operator looking into somebody's
access asks them together.

The entity-scope form answers a batch and renders one row per company **in the
order asked**, so a caller can line the answers against its own list. It refuses
a malformed entry itself rather than forwarding it, so the message can name
WHICH line was wrong — the service refuses the whole batch on the first bad one
and cannot say which of five it was.

The delegated-access form calls out the both-paths case explicitly rather than
leaving it in a paragraph: somebody holding an action in their own right AND by
delegation is exactly what a check on the outcome alone cannot see, and it is
the answer a four-eyes step needs.

## A real bug in the panels the eighth pass shipped

Found by driving the new forms, and it is the more interesting half of this
pass.

**React discards an uncontrolled input's value when the panel's shape changes
between statuses.** Measured, not inferred:

```
textarea BEFORE submit 1: "22222222-…-222222222222\nPLATFORM"
textarea AFTER  submit 1: "22222222-…-222222222222"          <-- second line gone
```

The consequence is worse than losing typed text. The next submit sends the
RESET value while showing the operator what they typed — so "tweak it and run it
again" silently re-ran the ORIGINAL question and presented the answer as if it
were the new one. The malformed-entry refusal never fired on a second submit for
exactly that reason: the bad line had been discarded before the form was
submitted.

Fixed by echoing the submission back as `defaultValue` on every field, on every
non-idle status — which is the fix `DecisionSearchState.filters` already carried
in `DecisionLogPanel` for its own filters. That precedent existed and was not
followed in the new panels, which is why this is recorded as a defect rather
than a refinement.

The form is read BEFORE the session check now, so a return on an expired session
also echoes what was typed: somebody signing back in should not lose their input
as well.

After the fix, the same sequence:

```
textarea AFTER  submit 1: "22222222-…-222222222222\nPLATFORM"   <-- retained
submit 2, malformed      : refusal banner present, results table gone
```

## The browser scripts were wrong twice more, in the same way

Three of the reach-panel assertions failed against features that work:

- an order check compared a result against the panel's own hint text, which says
  "Use PLATFORM for something that belongs to no single company" and appears
  before any result;
- a verdict check matched the hint "Borrowed authority is recorded per company",
  so the wait fired on static copy before the action had resolved;
- a table-header check was case-sensitive against headers the stylesheet
  uppercases.

Same root cause as the eighth pass's two: **asserting against page text that
contains prose about the thing being asserted.** The fix each time is to scope
to the element that actually changes — the table, the panel, the specific
headline — and to poll rather than match once. Recorded because it has now
happened five times in two passes, and a test that fails for its own reasons
costs more than no test: the next person spends their time on the wrong half.

## Verified

`go build`, `go vet` clean; full `go test ./...` green with the store suite
running against real PostgreSQL (62 tests, 0 skipped). Frontend `tsc --noEmit`
and `eslint` clean.

Both browser suites green: 16 checks on the decision-history and SoD panels, 11
on the two new reach panels, zero page or console errors in either.

The compose-managed service (not an ad-hoc container) verified to wire the
lifecycle consumer from `docker-compose.yml`'s own environment, with all three
partitions assigned.

## Still open after this pass

Unchanged from the eighth pass. Nothing inside the service; the remainder is
cross-service and each item is blocked on something nameable — the 86-caller
envelope migration, the employee-to-principal mapping that
`employee.terminated` needs, and tracker rows 79 and 81.
