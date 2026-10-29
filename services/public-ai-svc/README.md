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
PUBLIC_AI_ENABLED=true go run ./cmd/server          # PowerShell: $env:PUBLIC_AI_ENABLED='true'; go run ./cmd/server
```

This starts **local dev mode** on `127.0.0.1:8001` (where the console's `ZOIKO_PUBLIC_AI_URL`
points by default; `:8080` is often held by local Docker stacks). On start it builds the dataset,
signs it with a throwaway key, publishes it, moves the active release pointers and verifies the
signatures; `/health/ready` is green only after that. Defaults: `PUBLIC_AI_DATASET=public-kb` (the
public articles in [data/public-kb](data/public-kb/README.md), served as drafts with
`KB_ALLOW_DRAFT=true`, plus synthetic claims; `seed` is the older synthetic set), `MODEL_PROVIDER=mock` (deterministic,
extractive, no tools; the only provider in this build, dev/test only). Then:

```bash
curl -s -X POST localhost:8001/public-ai/v1/sessions -H "Origin: http://localhost:3000" \
  -H "Content-Type: application/json" -H "Idempotency-Key: demo-session-01" \
  -d '{"schema_version":"1.0","locale":"en-US"}'
```

Copy `session_id` and `session_grant` from the response, then:

```bash
curl -s -X POST localhost:8001/public-ai/v1/turns -H "Origin: http://localhost:3000" \
  -H "Content-Type: application/json" -H "X-Public-Chat-Session: <session_grant>" \
  -H "Idempotency-Key: 11111111-1111-4111-8111-111111111111" \
  -d '{"schema_version":"1.0","session_id":"<session_id>","client_turn_id":"11111111-1111-4111-8111-111111111111","message":"How much is the Growth plan per month in the United States?"}'
```

Expected: `"status":"ANSWERED"`, the synthetic Growth price statements with `[price]` placeholders,
`"market_code":"US"`, a citation and a `pricing` action. Each turn writes one dev-only
`turn diagnostics` log line (request_id, stage, reason; never the message).

### Use Groq instead of the mock model

```bash
# PowerShell: $env:MODEL_PROVIDER='groq'; $env:GROQ_API_KEY='<your key>'; $env:PUBLIC_AI_MODEL_TIMEOUT='15s'
MODEL_PROVIDER=groq GROQ_API_KEY=<your key> PUBLIC_AI_MODEL_TIMEOUT=15s PUBLIC_AI_ENABLED=true go run ./cmd/server
```

`GROQ_MODEL` defaults to `openai/gpt-oss-120b`. The key stays on the server and is never logged.
Only the compose step uses the model: greetings, refusals, clarifications and "no approved
information" replies are templates, and retrieval, claim checks, output validation and citations
run in code either way. Check the live path with
`GROQ_API_KEY=<your key> go test ./internal/acceptance -run TestGroqLive -v`.

With a database, `PUBLIC_AI_SEED_ON_START=true` (dev/test only) runs the same
migrate → seed → sign and publish → activate sequence against Postgres.

## How an answer is produced

Every `POST /public-ai/v1/turns` runs this fixed sequence. The model is called once, at compose,
with no tools; it never decides a fact.

| # | Step | What happens | File |
|---|---|---|---|
| 1 | Edge | Host and origin allowlist, JSON only, body size, schema, ≤2,000 chars NFC, session grant, Idempotency-Key = client_turn_id (replay / 422 mismatch), one active turn, rate and spend limits | `internal/middleware`, `internal/contract`, `internal/httpapi/handlers.go`, `internal/limits` |
| 2 | Sensitive data | Passwords, OTPs, cards, IBAN, SSN, NINO, tax and bank details, secrets: masked, never echoed, never sent to the model; X03 template | `internal/policy/redact.go` |
| 3 | Restricted intents | X01–X08 answered from pre-approved templates before any model call (X09 at claims, X10 at the edge limiter) | `internal/policy/classify.go`, `templates.go` |
| 4 | Classify | Deterministic rules → one primary intent I01–I30, at most one secondary (L2-INTENT-001 is pinned for a future provider classifier) | `internal/policy/classify.go` |
| 5 | Slots | Whitelisted slots only (market, plan, billing, integration…); a market or plan change recomputes everything | `internal/policy/classify.go`, `internal/orchestrator/pipeline.go` |
| 6 | Clarify | Only when a missing slot changes the truth; one question with market choices, Other and Show general overview; after two, one verified route | `internal/orchestrator/pipeline.go` |
| 7 | Claims | Typed signed claims first (price, trial, plans, market, certification, residency, integration): VERIFIED / INSUFFICIENT_EVIDENCE / CONTRADICTED / REVOKED / EXPIRED | `internal/claims`, `internal/release` |
| 8 | Retrieve | Public, release-pinned index; access filters before ranking; BM25 lexical with synonym/alias resolution (no vectors in R0, ADR-002) | `internal/knowledge` |
| 9 | Compose | Suivo system prompt (`internal/model/prompts/suivo_system_prompt.txt`) + L2/L3 layers pinned by hash; eligible records sent with their metadata as data; Groq (`openai/gpt-oss-120b`, strict JSON output, no tools) or the mock | `internal/model` |
| 10 | Validate | Grounded refs, numbers only from evidence, no URLs/emails/phones/markup, no "I booked/submitted", banned absolutes, 120 words | `internal/validate` |
| 11 | Resolve links | Route keys → HTTPS URLs from the signed route registry; a missing route hides the CTA | `internal/release` (`Route`, `PublicConfig`) |
| 12 | Deliver | Typed envelope: status, answer_class, status_label, market_code, clarification, reason; revocation re-checked at commit; privacy-safe events | `internal/contract`, `internal/httpapi`, `internal/events` |

Business outcomes (CLARIFY, ABSTAIN, REFUSED_ACTION) are HTTP 200. UNAVAILABLE is only for
infrastructure (kill switch, model failure or timeout, knowledge unavailable) and always carries a
`reason`, so the widget can word it.

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

## Tracing and rate limits

- **Traces** are off unless `OTEL_EXPORTER_OTLP_ENDPOINT` is set (OTLP/HTTP, for example `http://localhost:4318`). The standard `OTEL_*` variables choose the sampler. Spans carry ids, enums, counts and token usage only. An allowlist in `internal/telemetry` drops every other attribute, every span event and every status description, so prompts, answers and source text are never exported.
- **Per-IP limits** key on the visitor's address. `X-Forwarded-For` is believed only from peers in `PUBLIC_AI_TRUSTED_PROXIES` (default loopback, where the console's same-origin proxy runs). List your edge or load-balancer ranges there in each environment.

## Tests and evaluation

```bash
go test ./...                                         # Postgres tests run when TEST_DATABASE_URL is set
go run ./cmd/evalrun -dataset testdata/golden/golden-v1.json -out eval-report.json
```

`evalrun` exits 1 on any P0 failure (BLOCK_RELEASE). Coverage map: `docs/suivo/test-traceability.md`. Doc 05 CT-001 to CT-052: `docs/suivo/contract-test-map.md`.

## Open decisions (production blockers)

DEC-01 hosting/gateway · DEC-02 taxonomy v2.1 · DEC-03 real price/trial/claim data and owners ·
DEC-04 public name and logo · DEC-05 model vendor/region/DPA (only the fake model exists) ·
DEC-06 retention and cookies (DPIA) · DEC-07 session/replay TTLs · DEC-08 SLOs · DEC-09 signing and
revocation ownership · DEC-10 real handoff URLs · DEC-16 GO/STOP/rollback authority. Numeric limits
(rate, spend, TTL) are dev defaults pending CISO/SRE approval. Template wording is a dev draft pending
Legal/Product review.
