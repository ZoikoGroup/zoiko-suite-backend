# public-ai-svc — ZoikoSuite public AI assistant (Suivo, Release R0)

Anonymous, public-only website assistant built from engineering Documents 01–11
(`docs/suivo/IMPLEMENTATION_PROMPT.md`). Stable technical ID `zoikosuite-public-ai`;
"Suivo" is an internal working name only.

**Status: development build. Not production-ready.** Every document is PROPOSED, all content is
TEST-ONLY synthetic data, and the blockers in "Open decisions" below need named approvers.

## What it does (R0)

- Answers product, pricing, trial, integration and trust questions **only** from signed, approved
  public knowledge releases and a signed P1 claim registry, with citations and one official link.
- Prices, trials, certifications and market availability come **only** from VERIFIED claims and are
  stated verbatim; contradictions, expired or revoked evidence → it abstains.
- Refuses private-data requests, transactions, regulated advice and prompt injection; masks secrets
  before anything else runs; never claims a booking, ticket or callback.
- No access to any other ZoikoSuite service (enforced by `internal/isolation` in CI).

## Run it locally (no database)

```bash
cd services/public-ai-svc
PUBLIC_AI_ENABLED=true go run ./cmd/server
```

This starts **local dev mode**: in-memory store, TEST-ONLY fixtures signed with a throwaway key,
fake model. Then:

```bash
curl -s -X POST localhost:8080/public-ai/v1/sessions -H "Origin: http://localhost:3000" \
  -H "Content-Type: application/json" -H "Idempotency-Key: demo-session-01" \
  -d '{"schema_version":"1.0","locale":"en-US"}'
```

Copy `session_id` and `session_grant` from the response, then:

```bash
curl -s -X POST localhost:8080/public-ai/v1/turns -H "Origin: http://localhost:3000" \
  -H "Content-Type: application/json" -H "X-Public-Chat-Session: <session_grant>" \
  -H "Idempotency-Key: 11111111-1111-4111-8111-111111111111" \
  -d '{"schema_version":"1.0","session_id":"<session_id>","client_turn_id":"11111111-1111-4111-8111-111111111111","message":"How much is the Growth plan per month in the United States?"}'
```

Expected: `"status":"ANSWERED"`, text with `XTS 49.00`, a citation and a `pricing` action.

## Run it with Postgres

```bash
go run ./cmd/kbctl keygen            # put both values in your local .env (never commit it)
export PUBLIC_AI_DATABASE_URL=postgres://...   PUBLIC_AI_SIGNING_KEY=...   PUBLIC_AI_SIGNING_PUBLIC_KEY=...
go run ./cmd/kbctl build-kb -in testdata/fixtures/kb -id kb-dev-1 -out kb.release.json
go run ./cmd/kbctl build-claims -in testdata/fixtures/claims.json -id claims-dev-1 -out claims.release.json
go run ./cmd/kbctl publish -file kb.release.json -promote
go run ./cmd/kbctl publish -file claims.release.json -promote
PUBLIC_AI_ENABLED=true go run ./cmd/server
```

Operations: `kbctl promote` (also rollback), `kbctl revoke -kind source|claim -id ... -reason ...`,
`kbctl flag -name public_ai_enabled -value false -actor ... -approval ...` (kill switch).

## Run it in containers

```bash
docker compose -f deployments/docker-compose.public-ai.yml --env-file deployments/.env.public-ai up -d --build
docker exec public-ai-svc /app/kbctl publish -file /releases/kb.release.json -promote
docker exec public-ai-svc /app/kbctl publish -file /releases/claims.release.json -promote
```

The stack has its own Postgres and network (never joined to the main stack). `/health/ready` stays 503
until signed releases are published. Operations, rollback, kill switch and drills: [RUNBOOK.md](RUNBOOK.md).

## Tests and evaluation

```bash
go test ./...                                         # Postgres tests run when TEST_DATABASE_URL is set
go run ./cmd/evalrun -dataset testdata/golden/golden-v1.json -out eval-report.json
```

`evalrun` exits 1 on any P0 failure (BLOCK_RELEASE). Coverage map: `docs/suivo/test-traceability.md`.

## Open decisions (production blockers)

DEC-01 hosting/gateway · DEC-02 taxonomy v2.1 · DEC-03 real price/trial/claim data and owners ·
DEC-04 public name and logo · DEC-05 model vendor/region/DPA (only the fake model exists) ·
DEC-06 retention and cookies (DPIA) · DEC-07 session/replay TTLs · DEC-08 SLOs · DEC-09 signing and
revocation ownership · DEC-10 real handoff URLs · DEC-16 GO/STOP/rollback authority. Numeric limits
(rate, spend, TTL) are dev defaults pending CISO/SRE approval. Template wording is a dev draft pending
Legal/Product review.
