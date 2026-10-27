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

## STEPs 3–4 — Sessions, config and turns API

- Files: `internal/contract` (wire types + validation, envelope schema check), `internal/session`
  (grant issue/verify, SHA-256 at rest), `internal/orchestrator` (Answerer interface + AbstainAll
  placeholder), `internal/httpapi/handlers.go`, `api/openapi.yaml` (full R0 contract), `cmd/server`
  (Postgres or in-memory store, migrations on start).
- Behaviour: POST /sessions requires Idempotency-Key; replay returns the same session_id/expiry with a
  rotated grant (the grant is never stored, so it cannot be returned twice — ADR-005 dev default);
  DELETE ends the session, repeat delete of own session is 204, a foreign grant gets 404 (no
  existence leak); GET /config with ETag/304. POST /turns: grant → session, strict schema (unknown
  fields 400), NFC + control-char stripping, session_id must equal grant subject (403), Idempotency-Key
  must equal client_turn_id (400), atomic single active turn (409 + Retry-After), replay is
  byte-identical with no second orchestrator call, same key + different payload 422, the orchestrator
  runs on a context detached from client disconnect, and any orchestrator output that fails the
  response schema is replaced by UNAVAILABLE before it is sent.
- Store change: deleted sessions keep their grant hash marked deleted, so repeat DELETE is 204 while
  the grant cannot start turns or be rotated.
- Tests: CT-001, CT-002 (replay + grant rotation + mismatch), CT-009/010 replay without second call,
  CT-011 turn conflict, idempotency mismatch, header mismatch, scope denied, extra fields, long message,
  expired session, delete semantics, config ETag, contract validation, NFC normalization, envelope
  schema (ANSWERED requires provenance, non-route actions rejected).
- Result: build ok, vet ok, all tests pass (memory + Postgres), gofmt clean.

## STEP 5 — Sanitize, redaction, intercepts, intent rules

- Files: `internal/policy` (`redact.go`, `classify.go`, `templates.go`).
- Sensitive-input interception before any model call: payment cards (Luhn-checked), IBAN (mod-97),
  US SSN, UK NINO, labelled tax IDs, API keys/tokens/private keys, passwords, one-time codes, bank
  account numbers → masked as `[REDACTED:<kind>]` and exception X03. Templates never echo the message.
- Exception classes X01–X08 by deterministic rules, highest risk first:
  X03 > X06 > X05 > X02 > X01 > X04 > X08 > X07 (X09 is set by the claim stage, X10 by the limiter).
- Intent rules I01–I30 with scored phrase matching and a fixed tie order; unmatched → I29.
- Slots from the visitor's own words only (market, plan word, billing cadence, entity band,
  integration name); never inferred from IP.
- Templates: neutral DEV DRAFT wording for exceptions, abstention, clarification, unavailability and
  route-only intents (I08/I22/I23/I24/I28) that never claim a booking, ticket or callback; unresolved
  placeholders fail rendering. Copy needs Legal/Product review (Doc 06 App. B).
- Tests: detection for each kind, no false positives on plan sizes/years/IDs failing checksums,
  exception precedence (incl. injection + private data, sensitive + injection), negative cases for
  ordinary product questions, 13 intent samples, slot extraction, template rendering.
- Result: build ok, vet ok, all tests pass, gofmt clean.
