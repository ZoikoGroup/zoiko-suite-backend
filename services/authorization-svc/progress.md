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

- **ABAC is not implemented.** No attribute-condition rules exist anywhere
  in the architecture docs to encode — implementing it now would mean
  inventing business logic, not encoding a specified rule. Same posture
  policy-svc took with `SPEND_CONTROL`/`SOD_RULE`/`SIGNATORY_MATRIX`
  evaluation. Revisit if concrete attribute rules are ever specified.
- **No consumed events in v1** (`role.assigned`, `authority.delegated`,
  `employment.changed`, `entity.scope.updated`). None of these are
  actually published by any built service today — building a consumer
  with no real producer would be dead infrastructure. Role/permission/
  delegation management happens via this service's own admin endpoints
  directly instead.
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
