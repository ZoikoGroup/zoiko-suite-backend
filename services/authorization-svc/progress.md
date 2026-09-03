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
