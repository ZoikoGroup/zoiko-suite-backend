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

## STEPs 6–7 — Knowledge pipeline, kbctl and claim resolver

- Files: `internal/signing` (Ed25519, release files), `internal/knowledge` (registry/chunk types, ingestion
  state machine, BM25 index with mandatory filters), `internal/claims` (P1 resolver), `internal/release`
  (verified runtime with pinned snapshots, revocation ledger, publish/promote/revoke, public config),
  `internal/events` (the six CloudEvents), `internal/flags` (flags + kill switch, fail closed),
  `cmd/kbctl`, `testdata/fixtures` (TEST-ONLY: currency XTS, plans TEST_STARTER/TEST_GROWTH,
  URLs https://example.invalid/…).
- Ingestion (Doc 03 §09): REGISTER (every field mandatory) → ACQUIRE (https + allowlisted host) →
  classification/audience/PII → sensitive scan → taxonomy map → review (approved, no self-approval,
  P1 needs two approvers) → hidden-content/injection scan → dedup + content hash → chunk → STAGED.
  Fixture outcomes verified: poisoned → QUARANTINE_INDEX, internal → QUARANTINE_PRIVATE,
  draft → HOLD_APPROVAL, off-allowlist → QUARANTINE_FETCH; `javascript:` route rejected.
- Retrieval filter order: release → classification → audience → revoked → effective window → market →
  locale → BM25 → answerability (ANSWERABLE / NOT_FOUND / STALE / REVOKED).
- Claims (Doc 05 §17): VERIFIED only for ACTIVE, publishable, enabled, in-window, unrevoked claims with
  approved wording and provenance; CONTRADICTED on disagreeing active values (no majority vote);
  EXPIRED/REVOKED blocked; market-scoped subjects without a market → MARKET_REQUIRED (clarify);
  SOC 2 readiness (HOLD) is never stated as certification; ACTIVE claims need two approvals.
- Runtime: releases verified on load (invalid signature → not ready, no snapshot); revocations are
  copy-on-write and apply across rollback; commit-time check reads the ledger directly.
- Routes: `signup_or_trial` offered only where a TRIAL claim is VERIFIED for the market.
- kbctl exercised against Postgres: build/sign/publish/promote/revoke/flag; outbox received
  kb.release.promoted.v1 ×2, kb.source.revoked.v1, publicai.kill_switch.changed.v1;
  `stream_enabled=true` refused.
- Result: build ok, vet ok, all tests pass, gofmt clean.

## STEPs 8–11 — Model gateway, output validation, vertical slice, events, limits, kill switch

- Files: `internal/model` (gateway: tools hard-coded off, deadline, one transient retry, layered prompt
  with evidence escaped as non-instructional data; deterministic `Fake` provider with fault modes),
  `internal/validate` (post-generation gate), `internal/orchestrator/pipeline.go` (full pipeline),
  `internal/fixtureenv` (in-process stack on TEST-ONLY fixtures), `internal/events/relay.go`
  (outbox → Kafka/log, at-least-once, DLQ after 10 attempts), `internal/limits`, `cmd/server`
  (Postgres mode + local dev mode on fixtures), `internal/e2e`.
- Pipeline order: kill switch → verified snapshot (none → UNAVAILABLE, never model memory) →
  redaction + X-exceptions (templates, security signal) → per-intent disable → route-only intents →
  P1 claims (MARKET_REQUIRED → CLARIFY; CONTRADICTED → ABSTAIN + conflict record; REVOKED/EXPIRED →
  ABSTAIN; INSUFFICIENT → ABSTAIN) → retrieval (STALE/REVOKED → ABSTAIN; NOT_FOUND → ABSTAIN or
  out-of-scope for I29) → generation (claims are used verbatim) → validation (one repair) → ≤3
  citations from what was actually used → revocation re-check at commit → ≤1 official CTA.
- Validation: unknown claim/evidence refs, markup/script, instruction leakage, prohibited commitment
  phrases (unless a signed claim contains them), and ANY number not present in the supplied claims or
  evidence are rejected; model URLs/emails/phones removed; ≤120 words at sentence boundaries.
- Degraded modes: provider error/timeout → UNAVAILABLE + route; `approved_fallback_mode=static` and
  `model_generation_enabled=false` serve signed claim statements only; `pricing_claims_enabled=false`
  → no numbers; `public_ai_enabled=false` → UNAVAILABLE and readiness false.
- Limits: per-IP (IPv6 /64) and per-session token buckets, global in-flight cap → 429 + Retry-After;
  daily/monthly token ceilings stop the model path (fail closed if usage unreadable).
- Events: chat.turn.completed.v1 and chat.security.signal.v1 written in the turn transaction;
  chat.handoff.clicked.v1 is defined but not emitted in R0 (no click endpoint in the Doc 05 contract).
- Tests: vertical slice (grounded answer, verified price, clarify, conflict + record, SOC 2 never
  certified, ISO verified, private data/actions refused, injection + signal, sensitive input never
  echoed, provider error/timeout/transient, 5 misbehaving-model faults, revocation mid-flight, revoked
  source, kill switch, pricing off, static mode, per-intent disable, trial CTA gating, demo route-only,
  expired evidence, no knowledge), validator unit tests, gateway deadline + prompt escaping, relay
  delivery/no-resend/DLQ, six event types only, limiter tests, spend ceiling, HTTP e2e incl. SEC-T039
  (no visitor text or secrets in any event).
- Live smoke test: server in dev mode answered 4 sample questions correctly over HTTP; server log
  contained 0 occurrences of visitor text or grants.
- Result: build ok, vet ok, all tests pass (incl. Postgres), gofmt clean.

## STEP 12 — Evaluation harness

- Files: `internal/eval` (dataset loader, deterministic grader, report), `cmd/evalrun`,
  `testdata/golden/golden-v1.json` (25 TEST-ONLY cases, partitions A canonical, B clarification,
  C abstention/refusal, D adversarial; each with priority and requirement refs),
  `docs/suivo/test-traceability.md`, service `README.md`.
- Grader checks outcome, intent, required/forbidden text, required/forbidden claim citations,
  official-route-only actions, citation counts, and the universal ANSWERED-needs-provenance rule.
  Any P0 failure → BLOCK_RELEASE and exit code 1. A test proves the grader catches a fabricated
  certification. `eval.TestGoldenSet` runs the set in CI.
- Found by the eval: generic phrases ("can zoikosuite") outweighed the integration intent so a
  verified integration claim was not cited; removed them from intent scoring.
- Result: golden-dev-v1 25/25 passed, 0 P0 failures. build ok, vet ok, all tests pass, gofmt clean.
- Not covered (needs people, the widget or staging): holdout partition E, drift sentinel F, semantic
  and human grading lanes, WCAG, load/latency.

## STEP 14 — Ops readiness

- Files: `services/public-ai-svc/Dockerfile` (+ `.dockerignore`), `deployments/docker-compose.public-ai.yml`,
  `internal/release/manifest.go` + `kbctl manifest`, `internal/policy/bundle.go` (prompt/policy digest),
  `cmd/drill`, `RUNBOOK.md`, `docs/suivo/r0-readiness.md`; `fixtureenv.NewWithStore`; server logs its git SHA.
- Container: multi-stage, non-root (uid 10001), runs with a read-only root filesystem, all capabilities
  dropped, healthcheck; image 14.7 MB; contains no fixtures, keys or secrets.
- Compose: standalone stack with its own Postgres and network, port bound to 127.0.0.1, signed releases
  mounted read-only. Verified: `/health/ready` 503 before releases → `kbctl publish -promote` inside the
  container → 200 → price question answered from the verified claim; container healthy; 0 visitor text in logs.
- Release manifest: signed record of the four artifacts (app commit + image digest, prompt/policy
  digest, knowledge release + digest, claim release + digest), assurance runs (eval summary + report
  hash) and approvals all PENDING with decision NO_GO_UNTIL_SIGNED. Generated successfully.
- Drills (Doc 08 RB-01/02/04 + kill switch): DR-01 kill switch, DR-02 claim revocation, DR-03 rollback
  without resurrection, DR-04 provider outage/timeout, DR-05 safe-static — 5/5 pass on the memory store
  and 5/5 on Postgres.
- Readiness record lists every Doc 11 gate G0–G5 as PENDING with evidence and remaining needs.
- Result: build ok, vet ok, all tests pass (incl. Postgres), gofmt clean.
