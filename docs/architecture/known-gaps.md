# Known Architecture & Test Coverage Gaps

## Open: jurisdiction-rules-svc owns no compliance calendar
03-microservices.md §8.2 lists "compliance calendar logic" among this
service's holdings and jurisdiction.calendar.changed among its published
events. Neither exists: there is no calendar entity in its schema, and the
event name is deliberately absent from internal/events rather than declared
for a signal that could never fire. Filing due dates and filing_requirements
currently live in obligations-svc, so the boundary question — does the
calendar belong here, there, or split — has to be settled before the entity
is built. The other three §8.2 events (jurisdiction.rule.updated,
jurisdiction.rule.activated, legal.drift.detected) are published.

## Open: authorization scope for platform-wide reference data
Jurisdiction data has no tenant_id and no owning legal entity, but
authorization-svc's POST /v1/authorize rejects an empty legal_entity_id with
400. jurisdiction-rules-svc therefore presents a single synthetic
platform-scope entity (AUTHZ_PLATFORM_SCOPE_ID) on every decision, and role
assignments granting JURISDICTION_* / JURISDICTION_RULE_* actions must use
that same id. This is a workaround for a missing concept: authorization-svc
has no notion of a platform-scoped, non-entity resource. Any other
platform-wide service will hit the same wall.
seed-demo-rbac.ps1 does not grant these actions.

## Resolved: jurisdiction-rules-svc authorized nothing
HTTPAuthZClient.Authorize was a TODO that logged a warning and returned nil,
so every admin mutation was permitted without an authorization decision. The
production-startup guard made this worse rather than better: it forced a
non-placeholder AUTHZ_SERVICE_URL in production/staging, which is exactly the
branch that selected the no-op client — the StubAuthZClient it was written to
prevent would at least have been labelled a stub. docker-compose.yml
compounded it by pointing AUTHZ_SERVICE_URL at jurisdiction-svc itself
("mock stub authz points back"). The client now calls
authorization-svc's real contract and fails closed on denial, non-200,
unreadable body, and network error; that URL is rejected as a placeholder;
and internal/authz/client_test.go asserts each failure mode denies.

Related, same service: admin routes took the actor from X-Actor-Principal-ID
— a header nothing in the platform sets, since the gateway's ForwardAuth
middleware publishes X-Principal-Id — and fell back to the literal string
"system" when absent. Every write was therefore attributed to the platform
itself. Admin routes now require a principal (401 without) and record it.

## Test coverage gap: SQL-vs-stub verification
TestFindRules_SupersededRuleReturnedForHistoricalQuery (jurisdiction-
rules-svc) verifies stubStore's Go reimplementation of date-interval
filtering, not the real SQL query in pg_store.go. No integration-test
infrastructure (testcontainers-go, docker-compose, or a CI Postgres
service container) exists anywhere in services/ or ci.yml today.
tenant-entity-registry-svc's pg_store.go previously had ZERO test
coverage of any kind (see "Resolved" section below).


Fix in progress: internal/store/pg_store_test.go (env-guarded via
TEST_DATABASE_URL, skips locally, runs in CI) + Postgres service
container added to ci.yml, scoped initially to jurisdiction-rules-svc.

Closed for jurisdiction-rules-svc (2026-08-05). The stub reimplementation
still exists — it is the handler's fake, and that is the right thing for a
handler test — but the SQL it used to stand in for is now covered directly
against Postgres: half-open interval filtering, DRAFT exclusion, superseded
rules staying visible to historical queries and disappearing from current
ones, ancestor-chain resolution including a deliberately cyclic hierarchy,
rule-pack inheritance and override precedence, overlap rejection, drift
history, pagination ordering, and malformed-UUID handling. Both store test
files now apply every file in deployments/migrations rather than naming two
of them inline, so a migration added later cannot be silently skipped by the
tests the way one was silently skipped by the running dev volume.

## Resolved: identity-context-svc had no database
identity-context-svc's principal/role/delegation store
(internal/principal/repository.go) was a pure stub — every method returned
empty results or a "not implemented" error, with no database behind it.
It has been replaced by internal/store/pg_store.go, a real pgxpool-backed
implementation, with migrations in
deployments/migrations/000001_initial_schema.up.sql and an integration
test suite (internal/store/pg_store_test.go) wired into ci.yml's
TEST_DATABASE_URL matrix alongside jurisdiction-rules-svc. The /health
endpoint's TODO for a DB ping is also now implemented.

Known limitation carried forward: PrincipalStore's methods (other than
FindByIDPSubject) do not carry tenant_id through the interface, so — unlike
tenant-entity-registry-svc — these tables have no Postgres Row-Level
Security policy; FindByIDPSubject enforces tenant scoping via an explicit
WHERE clause instead. Enabling RLS here would require widening
PrincipalStore (and its resolver call sites and test mocks) to carry
tenant_id on every method — a larger, separate change.

Also carried forward: principal_role_assignments and delegated_authorities
have no write path in this service by design — Access Control Service and
Delegated Authority Service own those objects per
docs/architecture/03-microservices.md §9.3–§9.4, and neither exists yet —
so those tables will read back empty until internal/events/consumer.go is
wired to populate them from upstream events (tracked separately as the
event-backbone gap).



## Resolved: tenant-entity-registry-svc had zero test coverage
internal/store/pg_store.go had no tests of any kind — including no
verification that its Postgres Row-Level Security policies actually
isolate tenants, despite that being this service's central architectural
guarantee. Added internal/store/pg_store_test.go (env-guarded via
TEST_DATABASE_URL, same convention as jurisdiction-rules-svc and
identity-context-svc), covering CreateTenant/GetTenantByID,
CreateEntity/GetEntityByID, and — the important one —
TestPgStore_RLS_TenantIsolation, which creates two tenants each with
their own entity and proves a query scoped to tenant A cannot see
tenant B's data. Wired into ci.yml's TEST_DATABASE_URL matrix alongside
the other three services.

Follow-up (separate, not yet filed): the tests above cover Tenant and
LegalEntity only. EntityHierarchy, EntityJurisdictionAssignment,
DataResidencyPolicy, and TaxIdentityBundle still have no integration
test coverage.
