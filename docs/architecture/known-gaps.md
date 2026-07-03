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

Follow-up (separate tracked issues, not yet filed): backfill
integration tests for tenant-entity-registry-svc and identity-context-svc
persistence layers.
