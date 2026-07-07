# Known Architecture & Test Coverage Gaps

## Test coverage gap: SQL-vs-stub verification
TestFindRules_SupersededRuleReturnedForHistoricalQuery (jurisdiction-
rules-svc) verifies stubStore's Go reimplementation of date-interval
filtering, not the real SQL query in pg_store.go. No integration-test
infrastructure (testcontainers-go, docker-compose, or a CI Postgres
service container) exists anywhere in services/ or ci.yml today.
tenant-entity-registry-svc's pg_store.go additionally has ZERO test
coverage of any kind.

Fix in progress: internal/store/pg_store_test.go (env-guarded via
TEST_DATABASE_URL, skips locally, runs in CI) + Postgres service
container added to ci.yml, scoped initially to jurisdiction-rules-svc.

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

## Follow-up (separate tracked issue, not yet filed)
Backfill integration tests for tenant-entity-registry-svc's pg_store.go —
still zero test coverage of any kind, and its RLS-enforcing SQL is
untested against a real database in CI.
