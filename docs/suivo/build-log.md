# Suivo R0 build log

Each entry records what a build step changed and the verified result of
`go build ./...`, `go vet ./...`, `go test ./...` and `gofmt -l .` in
`services/public-ai-svc`. Postgres-backed tests run when `TEST_DATABASE_URL` is set.

## STEP 1 — Service skeleton

- Files: `go.mod`, `cmd/server`, `cmd/healthcheck`, `internal/config`, `internal/problem`,
  `internal/middleware`, `internal/httpapi`, `internal/isolation`, `api/openapi.yaml`, `.env.example`;
  `.github/workflows/ci.yml` (adds `public-ai-svc` to the matrix and to the `TEST_DATABASE_URL` list).
- Repaired from the earlier OpenCode attempt: that commit had corrupted `ci.yml` for every service
  (rewrote `[` in shell tests, changed triggers, mojibake, BOM) and left the service code uncommitted;
  its isolation test could never fail.
- Controls: server-issued `X-Request-ID`; first-party Origin allowlist (missing Origin denied) with CORS
  preflight; body cap → 413; non-JSON → 415; RFC 9457 problems from the Doc 05 §13 catalogue;
  `no-store`; security headers; access log with allowlisted fields only; config refuses
  `PUBLIC_AI_LOG_RAW_PROMPT=true`, PII capture, wildcard origins and >1 active turn per session.
- Tests: CT-003 origin denied, CT-004 oversized body, CT-005 content type, preflight, request id,
  readiness vs liveness, generic public health, SSE 404 in R0, SEC-T049 isolation (module-wide, plus a
  test proving a cross-service import is rejected), config guards, problem format.
- Result: build ok, vet ok, all tests pass, gofmt clean.

## STEP 2 — Database migrations and store

- Files: `deployments/migrations/000001_initial_schema.{up,down}.sql` + embed package,
  `internal/store` (`Store` interface, `Memory`, `Postgres`, `Migrate`/`MigrateDown`),
  `internal/store/storetest` (conformance suite run against both implementations).
- Schema `public_ai`: sessions (grant stored only as SHA-256), session_idempotency, turns (exact
  response bytes for byte-identical replay; never the raw message), releases (signed, immutable),
  release_pointer (atomic swap), revocations (append-only, enforced by trigger), conflicts, flags,
  usage_counters, outbox.
- Design change recorded in ADR-002: knowledge lives in signed release bundles rather than editable
  source/chunk tables (Doc 03 §11 forbids in-place edits).
- Tests: memory + Postgres conformance (sessions, grant rotation, delete, turn begin/replay/mismatch/
  conflict/abort/expired lock, sequence, releases immutability + pointer kind check, monotonic
  revocations, flags, usage, outbox dedupe/publish), migrations up/down/up, revocation append-only.
- Found by the real-Postgres run: JSONB re-formats stored responses; switched to BYTEA.
- Result: build ok, vet ok, all tests pass (Postgres 16 via TEST_DATABASE_URL), gofmt clean.
