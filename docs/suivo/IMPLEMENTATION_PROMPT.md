# SUIVO R0 — IMPLEMENTATION PROMPT (built from Documents 01–11)

This is the single build specification for the coding agent. It condenses Documents 01–11
(`docs/suivo/source-text/*.md`, all v1.0, 24 Oct 2026, all PROPOSED). When this file and a
source document disagree, the source document wins — stop and report the difference.

---

## 0. HOW YOU MUST WORK (read every time)

1. Do **one STEP per message** (Section 9). At the end of the step: build, vet, test, commit, report, **STOP**.
2. Read files with your read tool. **Never** write a script to scrape documents. **Never** write files with
   PowerShell `Set-Content`/`Out-File` (it corrupts characters) — use your file edit/write tool.
3. Never invent facts: no real prices, plans, trial lengths, certifications, markets, URLs, vendor names,
   SLAs or retention periods. All content fixtures are synthetic and marked `TEST-ONLY` (Section 8).
4. Never touch other services in `services/`. Never import another service's packages. Never connect to
   another service's database, Kafka topic or credentials.
5. Never commit secrets, `.env`, `docs/chatbot-suite-docs/`, `docs/suivo/source-text/`, `docs/suivo-madhu-backup/`.
6. Work on branch `suivo/r0-build` (create it from `main` in STEP 0). Commit after each step with message
   `feat(public-ai): STEP N - <title>`.
7. Report at the end of every step, exactly:
   - Files created/changed
   - Commands run + their real output (`go build ./...`, `go vet ./...`, `go test ./...`)
   - Requirement/test IDs covered (e.g. CT-011, SEC-T049, QA-T029)
   - Open decisions hit (e.g. DEC-05) and the dev default you used
   - The next step number
8. If a step needs a decision that is not in this file, use the **dev default** given here, record it in
   `docs/suivo/adr/`, and mark it `Status: Proposed — pending <role>`. Never mark anything Approved.

---

## 1. WHAT WE ARE BUILDING (scope = Release R0 only)

Source: Doc 11 §04, Doc 01 §01/§05.

**R0 "Public guidance"** — an anonymous, public website assistant for ZoikoSuite:
- Explains products/capabilities, pricing, trials, integrations, trust/security, using only approved public
  knowledge and signed claims; cites sources; asks at most one clarification; abstains when evidence is
  missing; refuses private/transactional requests; sends visitors to official routes (demo, pricing, support,
  trust) by link only.
- Stable technical ID: `zoikosuite-public-ai`. Product ID: `zoikosuite`. Audience: `PUBLIC_ANONYMOUS`.
  "Suivo" is an internal working name only — never use it as an identifier; display name comes from config.

**NOT in R0 (do not build):** staff admin console (Doc 09), CRM writes, lead intake, booking/calendar
(Doc 10), case/ticket creation, file upload, payments, sign-up, account access, open-web search, model tools,
fine-tuning, agent frameworks, authenticated copilot (R2).

**Immutable boundary (INV-01, SEC-P0-01, Doc 04 §01):** the public runtime has NO network route, credential,
token or code path to accounting, AP, AR, treasury, payroll, HR, tax, legal, banking, procurement, compliance
or tenant systems. The model authorises nothing.

---

## 2. REPOSITORY FACTS (verified) AND WHERE CODE GOES

Verified in this repo: Go 1.25 services under `services/`, each its own module `zoiko.io/<name>`, using
`github.com/go-chi/chi/v5`, `github.com/jackc/pgx/v5`, `github.com/segmentio/kafka-go`, `go.uber.org/zap`,
`github.com/google/uuid`; layout `cmd/server/main.go`, `cmd/healthcheck`, `internal/{config,handler,
middleware,store,events,outbox,health,telemetry,...}`, `deployments/migrations/NNNNNN_*.up.sql/.down.sql`;
CI in `.github/workflows/ci.yml` runs a per-service matrix (`go mod verify`, `go build`, `go vet`, tests)
with a Postgres 16 service container. No frontend code exists in this repo.

`services/assistant-rag-svc` already exists. **Read it only to copy conventions.** Do NOT reuse its
database, index, topics, credentials or runtime (Doc 06 §09: dedicated PUBLIC_APPROVED namespace, no shared
index).

**New service:** `services/public-ai-svc` (module `zoiko.io/public-ai-svc`). One well-factored runtime
service with internal packages is explicitly allowed (Doc 04 §05, Doc 05 §16: "do not multiply
microservices without evidence"). Internal operations from Doc 05 §16 are in-process packages, not HTTP
services.

```
services/public-ai-svc/
  cmd/server/main.go
  cmd/healthcheck/main.go
  cmd/kbctl/main.go            # offline CLI: ingest fixtures, build+sign release, promote, revoke
  internal/config/             # env config (Section 7)
  internal/httpapi/            # chi router, handlers for /public-ai/v1
  internal/problem/            # RFC 9457 problem+json
  internal/middleware/         # request id, origin allowlist, body limit, rate limit, session auth, recover
  internal/session/            # anonymous session + grant, TTL, one-active-turn lock
  internal/idempotency/        # turn/session dedupe
  internal/orchestrator/       # turn pipeline (Section 4)
  internal/policy/             # sanitize, redaction, X01–X10 intercepts, intent classification
  internal/claims/             # claims:resolve (P1)
  internal/knowledge/          # registry, ingestion state machine, chunking, retrieval, releases, revocation
  internal/routes/             # official route registry
  internal/model/              # gateway interface + fake provider (no real vendor until DEC-05)
  internal/validate/           # output validation (schema, claim refs, URLs, HTML, phrases)
  internal/events/ + outbox/   # CloudEvents envelope, transactional outbox, Kafka publisher
  internal/flags/              # kill switch + feature flags
  internal/telemetry/          # redacted logs, metrics, OTel attributes
  internal/isolation/          # isolation test (Section 6)
  deployments/migrations/
  testdata/fixtures/           # TEST-ONLY synthetic content
```

---

## 3. PUBLIC API CONTRACT (Doc 05 §06–§15, Doc 04 §10–§11)

Cross-cutting: base path `/public-ai/v1`; JSON UTF-8 strict schemas (`additionalProperties:false`); server
sets `X-Request-ID` on every response; W3C `traceparent` accepted only after validation; session header
`X-Public-Chat-Session` (opaque short-lived grant, never logged; no cookies in R0); `Cache-Control: no-store`
on sessions/turns; errors are `application/problem+json` (RFC 9457) with no stack/vendor/internal detail;
never accept a forwarded customer OAuth token; Origin must be in `PUBLIC_AI_ALLOWED_ORIGINS`.

| Method + path | Success | Notes |
|---|---|---|
| `POST /public-ai/v1/sessions` | 201 | `Idempotency-Key` required (dev default — Doc 05 §06 vs §07 conflict) |
| `POST /public-ai/v1/turns` | 200 | `Idempotency-Key` = `client_turn_id`; one active turn per session |
| `GET /public-ai/v1/turns/{turn_id}/events` | 404 in R0 | SSE is behind `stream_enabled=false`; implement later only if approved |
| `DELETE /public-ai/v1/sessions/{session_id}` | 204 | repeat delete of own session → 204; ends session + ephemeral state only |
| `GET /public-ai/v1/config` | 200 + ETag | |
| `GET /public-ai/v1/health/public` | 200/503 | generic; no provider/region names |
| `GET /health/live`, `/health/ready`, `/health/startup` | | live never calls model/DB; ready=false if signed config missing or revocation unenforceable (Doc 08 §10) |

**POST /sessions request:** `schema_version`("1.0"), `locale`(BCP 47, optional), `market_code`(ISO 3166-1
alpha-2, optional, allowlisted; never inferred from IP), `page_context` enum
`home|pricing|core_modules|demo|support|other`.
**Response:** `schema_version`, `session_id` (`pub_s_…`), `session_grant`, `expires_at` (RFC 3339 UTC),
`policy_version` (e.g. `pub-policy-1.0`), `capabilities{streaming:false, lead_capture:false, file_upload:false}`.

**POST /turns request:** required `schema_version` const "1.0", `session_id` (minLength 12, must equal grant
subject), `client_turn_id` (UUID), `message` (1–2000 chars, NFC, control chars stripped); optional `locale`,
`market_code` (`^[A-Z]{2}$`), `page_context` (enum above), `requested_response_mode` enum `[json]`.

**Turn response envelope (`zs-public-answer-v1`):**
`schema_version`, `turn_id` (`pub_t_…`), `request_id` (`req_…`), `turn_sequence` (int ≥1, monotonic),
`status` ∈ `ANSWERED|CLARIFY|REFUSED_ACTION|ABSTAIN|HANDOFF|UNAVAILABLE`, `intent_id` (I01–I30, X01–X10 or
`UNKNOWN`), `text` (≤5000 chars, plain text, escaped), `citations` (max 5 in schema; orchestrator emits ≤3 —
Doc 01/03) each `{source_version_id, claim_ids[], url_key}`, `actions` (max 3; orchestrator emits ≤1 primary)
each `{type:"OPEN_OFFICIAL_ROUTE", route_key}`, `knowledge_release_id`, `policy_version`, optional
`flags{requires_human}`. ANSWERED requires machine-verifiable provenance; no evidence → ABSTAIN + safe action.
Business outcomes (REFUSED_ACTION, ABSTAIN…) are HTTP 200, never errors.

**Idempotency / concurrency (Doc 05 §12):** dedupe scope `(product_id, session_id, client_turn_id)`.
Same key + same normalized payload → 200 replay of same `turn_id`, no second model call.
Same key + different payload → 422 `publicai.idempotency_mismatch`. Header ≠ `client_turn_id` → 400
`publicai.idempotency_header_mismatch`. Turn already running → 409 `publicai.turn_conflict` (+Retry-After).
Expired session → 401. Release revoked mid-turn → ABSTAIN.

**Error codes (Doc 05 §13):** 400 `invalid_request|unsupported_schema|idempotency_header_mismatch`;
401 `session_expired`; 403 `origin_denied|scope_denied`; 404 `not_found`; 409 `turn_conflict`;
413 `payload_too_large`; 415 `unsupported_media_type` (CT-005); 422 `validation_failed|idempotency_mismatch`;
429 `rate_limited` (+Retry-After); 502/503/504 `dependency_failed|service_unavailable|timeout`.
Problem fields: `type`, `title`, `status`, `detail`, `instance`, `code` (`publicai.<suffix>`), `request_id`,
`retryable`. Dev default for malformed `market_code` pattern → 422 `validation_failed`; valid but unsupported
market → 200 CLARIFY (Doc 05 CT-008 vs §09).

**Config response (§15):** `config_revision` (=ETag), `assistant_display_name` (neutral default
"ZoikoSuite assistant" until brand clearance DEC-04), `capabilities{streaming,file_upload:false,
lead_capture:false,authenticated:false}`, `supported_locales` (["en-US"]), `market_choices`, `public_routes`
(route_keys only), `privacy_notice_key`.

Write `services/public-ai-svc/api/openapi.yaml` (OpenAPI 3.1) matching the above.

---

## 4. TURN PIPELINE (Doc 05 §11/§18, Doc 06 §08, Doc 03 §14, Doc 04 §08) — fail closed

1. **Edge:** origin allowlist, content-type, body size, schema, grant→session binding, rate limit,
   idempotency, atomic one-active-turn lock. Ignore any browser-supplied role/tenant/source/approval fields.
2. **Sanitize + intercept (before anything else, before any model call):** detect and mask passwords, API
   keys, OTPs, card numbers, bank/IBAN, national/tax IDs, payroll data. If found → X03 SAFE intercept:
   do not echo, do not store the text, return the sensitive-data guard template + secure route.
3. **Policy / intent:** deterministic exception classes first (highest risk wins):
   X01 SAFETY_PRIVATE_ACTION, X02 SAFETY_ACTION_DENIED, X03 SAFETY_SENSITIVE_INPUT, X04 SAFETY_REGULATED_ADVICE,
   X05 SAFETY_SECURITY_ESCALATE, X06 SAFETY_PROMPT_INJECTION, X07 SAFETY_CROSS_PRODUCT,
   X08 SAFETY_CLAIM_VERIFICATION, X09 SAFETY_UNSUPPORTED_COVERAGE, X10 SAFETY_ABUSE_RATE.
   Then one primary intent I01–I30 (+ optional secondary): I01 PRODUCT_OVERVIEW, I02 CAPABILITY_DISCOVERY,
   I03 MULTI_ENTITY, I04 JURISDICTION_COVERAGE, I05 PRICING, I06 TRIAL, I07 SUBSCRIPTION_CHANGES,
   I08 DEMO_BOOKING, I09 ENTERPRISE_QUALIFICATION, I10 INTEGRATIONS, I11 MIGRATION, I12 SECURITY_ASSURANCE,
   I13 PRIVACY, I14 DATA_RESIDENCY, I15 GOVERNANCE, I16 EVIDENCE, I17 TAX_COMPLIANCE, I18 PAYROLL_WORKFORCE,
   I19 LEGAL_WORKFLOWS, I20 REPORTING_AI, I21 SELF_SERVICE_SUPPORT, I22 ACCOUNT_NAVIGATION,
   I23 CUSTOMER_SUPPORT, I24 HUMAN_HANDOFF, I25 AVAILABILITY_STATUS, I26 COMPARISON,
   I27 ACCESSIBILITY_FEEDBACK, I28 BILLING_INFO, I29 OUT_OF_SCOPE, I30 RESPONSIBLE_AI (Doc 02 §06).
   R0 classifier = deterministic rules (keywords/patterns) behind an interface; model-based classification
   is added only after DEC-05 and must still be checked by the rules.
4. **Slots + clarification (Doc 02 §09–§10):** market_code, plan_code, billing_cadence, entity_band
   (1/2–5/6+), integration_name… At most ONE clarifier; after two failed clarifications give one verified
   support/demo route. Never infer market/jurisdiction from IP. Never ask for account numbers, credentials,
   tax IDs, payroll files, etc.
5. **P1 claims first** (pricing, trial, plan eligibility, market availability, certification, residency,
   integrations status) via `claims.Resolve` (Section 5.3). Decision rules:
   VERIFIED → use only the exact typed value; INSUFFICIENT_EVIDENCE → ABSTAIN + CTA;
   CONTRADICTED → ABSTAIN + record conflict; REVOKED/EXPIRED → block + invalidate cache.
   Prices are never pasted into prompts as free text and never derived arithmetically (Doc 02 S08).
6. **Retrieval (P2/P3) in this mandatory order:** pinned release → classification filter
   (PUBLIC_APPROVED) → audience filter → effective/revoked filter → search → rank → answerability
   (`ANSWERABLE|CLARIFY|BLOCKED|NOT_FOUND`; model cannot override BLOCKED) → citation validation.
7. **Generation:** model gateway with `tools=[]`, no browsing, strict JSON output, evidence passed as quoted
   non-instructional data inside `<approved_public_evidence>` (Doc 02 L3-EVIDENCE-001), token/timeout budget.
8. **Output validation (deterministic):** schema; every claim reference ∈ resolved claims; every citation ∈
   retrieved evidence; strip/deny any URL, email or phone produced by the model (links come only from the
   route registry); escape HTML; reject markup/script → ABSTAIN; prohibited phrases ("guaranteed",
   "fully compliant", "100%", "unlimited", "SOC 2 certified", "I booked you", "ticket created"…) unless a
   signed claim allows the exact wording; ≤120 words before expand (Doc 01 §12). Repair once within
   budget, else safe fallback.
9. **Revocation check at commit** (Doc 08 §06): re-check release pointer + revocation watermark; if changed
   → ABSTAIN.
10. **Persist + outbox:** store turn metadata (no raw message unless approved — it is not), write redacted
    `chat.turn.completed.v1` to the outbox in the same transaction.

Degraded modes (Doc 06 §23): model down → UNAVAILABLE + official links (no alternate vendor); claim registry
stale → no numeric claims; index compromised → serve only signed static content; breaker open → 429.

---

## 5. KNOWLEDGE, CLAIMS, ROUTES (Doc 03, Doc 05 §17–§18, Doc 08 §06)

### 5.1 Storage
Postgres, dedicated schema `public_ai` with its own DB role/credentials (env `PUBLIC_AI_DATABASE_URL`).
Retrieval R0 = Postgres full-text search (lexical) + exact-ID lookup with mandatory metadata filters.
Vector search is NOT added in R0 (Doc 04/ADR: add only when a documented need is shown) — keep a
`Retriever` interface so it can be added later. Chunk size starting band 300–700 tokens, 40–80 overlap.

### 5.2 Source registry (Doc 03 §06) — every field mandatory, null never means approved
`source_id`, `source_version_id`, `source_type`, `origin_uri`, `product_id`='zoikosuite',
`audience`='PUBLIC_ANONYMOUS', `classification`='PUBLIC_APPROVED', `pii_state`='CLEAR', `authority_tier`
(P0–P4), `subject_type`, `owner_id`, `approver_ids`, `market_codes`, `locale`, `plan_codes`,
`capability_ids`, `approval_state`, `approval_ts`, `effective_from`, `effective_until`, `review_due_at`,
`expires_at`, `content_sha256`, `signing_ref`, `license`, `supersedes`, `revocation_reason`.
Chunks inherit all scope/validity/revocation fields (Doc 03 §07) and carry `chunk_id`, `source_version_id`,
`segment_number`, `document_anchor`, `display_title`, `approved_uri`, `plain_text`, `text_sha256`,
`retrieval_release_id`, `valid_from`, `valid_until`, `revoked_at`.

Ingestion state machine (Doc 03 §09): REGISTER → ACQUIRE → EXTRACT → NORMALIZE → SENSITIVE SCAN →
DEDUP/DIFF → TAXONOMY MAP → CLAIM VALIDATE → REVIEW → CHUNK (+ injection/poison scan) → STAGE & EVAL →
PROMOTE (atomic pointer swap) → REVOKE (tombstone + cache invalidation). Failure states as in Doc 03.
R0 ingests only local fixture files via `kbctl`; no crawler, no visitor URLs.

### 5.3 Claim ledger + resolver (Doc 03 §08, Doc 05 §17)
Claim fields: `claim_id`, `claim_revision`, `claim_type`, `subject_id`, `predicate`, `normalized_value`,
scope (market, plan, segment, locale — absent = unknown, never global), `status`
(ACTIVE|CONDITIONAL|TARGET|HOLD|RETIRED), `operationally_enabled`, `publicly_publishable`, `valid_from`,
`valid_until`, `authority_ref`, `evidence_source_version_ids`, `approval_refs`, `conflict_ids`.
Resolve request: `product_id`, `audience`, `claim_subject`, `claim_keys[]`, `market_code`, `currency`,
`billing_term`, `as_of`, `required_release`. Response: `decision`
(VERIFIED|INSUFFICIENT_EVIDENCE|CONTRADICTED|REVOKED|EXPIRED), `claim_release_id`, `effective_at`,
`expires_at`, `verified_claims[]`, `missing_claim_keys[]`, `reason_code`, `signature_ref`.
Two conflicting active values → CONTRADICTED (no majority voting). Readiness is never "certified".

### 5.4 Releases, signing, revocation (Doc 03 §11–§12, Doc 08 §06)
Immutable release manifest: `release_id`, `index_schema_version`, `source_versions[]`, `claim_revisions[]`,
chunk policy, `eval_run_id`, `approved_by`, `signed_at`, `revocation_watermark`, content hashes.
Four independently versioned artifacts: app (git SHA), prompt/policy bundle, PUBLIC_APPROVED KB, P1 claim
registry; turn pins `release_tuple{app, policy, kb, claims, model_profile}` at start.
Signing dev default (D08-03 open): Ed25519, private key from env `PUBLIC_AI_SIGNING_KEY` (dev key generated
locally, never committed); verify on load; unsigned/invalid → not ready.
Promotion = atomic pointer swap. Revocation = monotonic watermark; denies synchronously at retrieval and at
commit; **rollback never restores a revoked source or claim**.

### 5.5 Route registry (Doc 05 §15, Doc 02 §20)
Route keys: `pricing`, `demo`, `signup_or_trial` (only if trial eligibility claim VERIFIED), `support`,
`core_modules`, `trust`. Each: `route_key`, HTTPS URL, market, locale, effective interval, approval ref.
Missing/unpublished route → hide CTA. Reject non-HTTPS, `javascript:` and off-allowlist hosts. Real URLs
are NOT known (DEC-10/D10-01) — fixtures use `https://example.invalid/...`.

---

## 6. SECURITY, PRIVACY, OPERATIONS (Doc 06, Doc 08)

- **Isolation test (SEC-T049, F02):** `internal/isolation` test that fails if `go list -deps ./...` includes
  any `zoiko.io/` module other than `zoiko.io/public-ai-svc`, and if config contains any other service's
  DSN/topic. Add to CI.
- **Limits (Doc 06 §12–§13):** message ≤2000 chars; body size cap; nesting cap; per-IP and per-session token
  bucket; 1 in-flight turn per session; model timeout; output token ceiling; daily/monthly spend ceilings →
  auto throttle / kill. Numeric values are NOT set by the docs → put them in config with conservative dev
  defaults and record as pending CISO/SRE approval (D06-05).
- **Logging allowed (Doc 05 §24, Doc 06 §21):** request_id, turn_id, product_id, policy_version, intent_id,
  status, latency, error_code, kb_release_id, claim IDs/decision, rate-limit bucket, aggregate tokens/cost.
  **Forbidden:** raw messages, prompts, model responses, grants, secrets, source excerpts, financial/tax IDs.
  `PUBLIC_AI_LOG_RAW_PROMPT=false` is enforced in code, not just config. Test with a canary secret (SEC-T039).
- **OTel attributes (Doc 08 App. B):** `service.name=suivo-public-orchestrator`, `suivo.release.*`,
  `suivo.outcome`, `gen_ai.operation.name`, `gen_ai.request.model`, `gen_ai.usage.*`. Never
  `gen_ai.input.messages`/`output.messages`/`system_instructions`. No session IDs as metric labels.
- **Events (Doc 05 §21) — exactly these six, CloudEvents 1.0 JSON, via transactional outbox, topics
  `publicai.telemetry.v1` / `publicai.control.v1` / `kb.governance.v1`:** `chat.turn.completed.v1`,
  `chat.handoff.clicked.v1`, `kb.release.promoted.v1`, `kb.source.revoked.v1`,
  `publicai.kill_switch.changed.v1`, `chat.security.signal.v1`. Payloads exactly as Doc 05 §21 (no raw text,
  no email). Consumers idempotent; replay re-checks revocation epoch. The browser never publishes events.
- **Flags (Doc 04 §20, Doc 08 §26):** `public_ai_enabled`, `model_generation_enabled`, `stream_enabled`
  (false), `pricing_claims_enabled`, `trial_cta_enabled`, `per_intent_disable`, `kb_release_pointer`,
  `approved_fallback_mode`, `max_concurrent_turns`. Kill switch fails closed and emits
  `publicai.kill_switch.changed.v1`.
- **Handoff (Doc 09 §11, Doc 10):** R0 is ROUTE_ONLY. Never say a ticket, booking, callback or lead exists.
  `chat.handoff.clicked.v1` never means a case was created.
- **Deletion contract (Doc 06 §15):** DELETE ends session + ephemeral state; never claim audit/vendor
  records are erased.
- **Trust copy:** assistant identifies as automated; never implies accountant/tax adviser or private access.

---

## 7. CONFIGURATION (Doc 04 App. B) — `.env.example` only, no real values

```
PUBLIC_AI_ENABLED=false
PUBLIC_AI_HTTP_ADDR=:8080
PUBLIC_AI_DATABASE_URL=postgres://public_ai_app:CHANGE_ME@localhost:5432/public_ai?sslmode=disable
PUBLIC_AI_KAFKA_BROKERS=localhost:9092
PUBLIC_AI_ALLOWED_ORIGINS=http://localhost:3000
PUBLIC_AI_MODEL_PROFILE=fake-v0
PUBLIC_AI_KB_RELEASE=
PUBLIC_AI_POLICY_VERSION=pub-policy-1.0
PUBLIC_AI_TOKEN_BUDGET_PER_TURN=2000
PUBLIC_AI_MAX_ACTIVE_TURNS_PER_SESSION=1
PUBLIC_AI_SESSION_TTL=30m
PUBLIC_AI_IDEMPOTENCY_TTL=24h
PUBLIC_AI_LOG_RAW_PROMPT=false
PUBLIC_AI_PRODUCTION_CHAT_PII_CAPTURE=false
PUBLIC_AI_REGION=
PUBLIC_AI_SIGNING_PUBLIC_KEY=
PUBLIC_AI_SIGNING_KEY=
```
TTL, budget and rate values are dev defaults pending DEC-07 / DEC-08 / D06-05.

---

## 8. TEST FIXTURES AND TESTS

- Fixtures in `testdata/fixtures/`, every file headed `TEST-ONLY — synthetic, not ZoikoSuite fact`.
  Use currency `XTS` (ISO test code), plan codes `TEST_STARTER`/`TEST_GROWTH`, URLs `https://example.invalid/…`.
  Include: one ACTIVE price claim, one EXPIRED claim, one REVOKED source, two CONTRADICTING claims, one
  source containing a hidden prompt-injection line, one PUBLIC_DENIED/private document (must never be
  retrievable).
- Name tests after the source IDs: `TestCT011_TurnConflict`, `TestSECT049_NoPrivateImports`,
  `TestQAT029_...`. Contract tests CT-001–052 (Doc 05), security SEC-T001–060 (Doc 06), QA QA-T001–084
  (Doc 07), golden G01–G36 (Doc 02), KB-001–030 (Doc 03), A01–A40 (Doc 04). Implement the ones relevant to
  each step; keep a coverage table in `docs/suivo/test-traceability.md`.
- Use an embedded/test Postgres as the existing services do in CI.

---

## 9. BUILD STEPS (one per message; Doc 11 stages S0–S4, vertical slice first)

**STEP 0 — G0 discovery (no code).** Create branch `suivo/r0-build` from `main`. Read
`services/assistant-rag-svc`, `.github/workflows/ci.yml`, `deployments/`, `supabase/`. Write
`docs/suivo/g0-architecture-delta.md`: for each need (HTTP framework, DB, migrations, Kafka/outbox, config,
logging, health, CI, secrets, search, model provider, signing, widget host) classify
EXISTING / REUSABLE / MISSING / BLOCKED with repo path + next action (Doc 11 §07). Write ADRs in
`docs/suivo/adr/` for: ADR-001 service location & isolation, ADR-002 Postgres FTS retrieval (no vector in R0),
ADR-003 fake model provider until DEC-05, ADR-004 Ed25519 dev signing, ADR-005 resolved doc conflicts
(list every dev default from Sections 3–5). All `Status: Proposed`.

**STEP 1 — Service skeleton.** Module, chi server, config loader (Section 7), zap logging with redaction,
request-ID + recover + body-limit + origin middleware, RFC 9457 problem package, `/health/live|ready|startup`
and `/public-ai/v1/health/public`, `cmd/healthcheck`, isolation test, `.env.example`, add service to CI
matrix. Tests: problem format, origin denied 403, oversized body 413, wrong content-type 415, isolation.

**STEP 2 — Database migrations.** `public_ai` schema: sessions, turns, idempotency_keys, sources,
source_versions, chunks (with tsvector), claims, conflicts, releases, release_pointer, revocations,
routes, flags, abuse_counters, outbox. Up + down. Tests: migrate up/down cleanly.

**STEP 3 — Sessions + config.** POST/DELETE sessions, grant issue/verify (random, hashed at rest, never
logged), TTL, GET config with ETag. Tests: CT for 201, idempotent replay, 401 expired, delete twice → 204,
session of another grant → 403/404.

**STEP 4 — Turns API shell.** Request validation, idempotency rules, atomic one-active-turn lock,
`turn_sequence`, envelope builder; orchestrator returns ABSTAIN for everything. Tests: 409 turn_conflict,
422 idempotency_mismatch, 400 header mismatch, replay without second call, schema rejects extra fields.

**STEP 5 — Sanitize, redaction, intercepts, intent rules.** X01–X10 detectors, sensitive-input masking,
I01–I30 rule classifier with highest-risk-wins, slot extraction, one-clarifier rule, response templates
(Doc 02 §17 T01–T30 — use neutral wording; unresolved placeholders never rendered). Tests: secrets never
echoed/stored (QA-T043+), "pay my invoice" → REFUSED_ACTION, injection → X06, ledger/balance → X01.

**STEP 6 — Knowledge pipeline + `kbctl`.** Source registry, ingestion state machine, sensitive & injection
scan, chunking, release build + Ed25519 sign, verify, promote pointer, revoke with watermark, FTS retrieval
with mandatory filter order, answerability. Tests: private doc never retrieved, revoked source not
retrievable, rollback does not resurrect revoked source, expired source → ABSTAIN_STALE.

**STEP 7 — Claim resolver.** Typed claims, resolve decisions, conflict records, cache keyed by
release+watermark. Tests: VERIFIED exact value only; CONTRADICTED → ABSTAIN; EXPIRED/REVOKED blocked;
no claim without market → CLARIFY.

**STEP 8 — Model gateway + output validation.** `model.Provider` interface, `fake` provider (deterministic,
returns structured drafts from evidence), timeouts, budget, one repair, fallback; validator rules from
Section 4 step 8. Tests: model-produced URL stripped, unsupported claim rejected, HTML escaped, provider
timeout → UNAVAILABLE + official route.

**STEP 9 — Routes + full orchestrator = VERTICAL SLICE (Doc 11 §13).** Wire steps 1–9 of Section 4.
End-to-end tests must cover: grounded fixture answer with citation + official CTA; denied private-data
intent; price from VERIFIED claim; revocation mid-flight → ABSTAIN; provider offline → safe fallback.
Stop rules: any private doc retrieved, any model tool, any unsupported price/trial/security claim = FAIL.

**STEP 10 — Events + telemetry.** CloudEvents envelope, outbox writer in turn transaction, Kafka publisher
with retry/backoff/DLQ, the six events, redacted logs/metrics/OTel. Tests: no raw text in any event or log
(canary secret), duplicate event idempotent.

**STEP 11 — Abuse, spend, flags, kill switch.** Rate limits, spend ceilings, flags table + cache,
kill switch fail-closed + event, degraded modes. Tests: 429 with Retry-After, kill switch → UNAVAILABLE,
`pricing_claims_enabled=false` → no numbers.

**STEP 12 — Evaluation harness.** Golden fixture format per Doc 07 §06/App. A, partitions A–D synthetic,
runner `cmd/evalrun` producing a JSON report (pass/fail per ID, P0 failures block). Traceability table.

**STEP 13 — Widget (Doc 01 §06–§11, Doc 07 §15).** ASK ME FIRST for the frontend repository. If I say
"build it here", create `web/public-ai-widget/` as a framework-neutral custom element: launcher
("Chat with ZoikoSuite"), panel/bottom sheet, states S00–S09, ≤4 chips, composer (Enter/Shift+Enter),
citations, single CTA via route_key, clear/end, privacy notice, WCAG 2.2 AA (focus, live region, 320px,
200% zoom, reduced motion, 44×44 targets). No secrets, no transcript in localStorage.

**STEP 14 — Ops readiness.** Dockerfile, docker-compose entry, release manifest file for the four artifacts,
runbooks RB-01..RB-04 (Doc 08 §16–19) adapted to this service, rollback + kill-switch drill scripts.
Write `docs/suivo/r0-readiness.md` listing every gate (Doc 11 G0–G5) with status PENDING and the evidence
produced. Never mark a gate passed.

---

## 10. PERMANENT BLOCKERS (cannot be resolved by code — list them, don't work around them)

DEC-01 real hosting/gateway; DEC-02/A04 taxonomy v2.1 crosswalk; DEC-03/A05 approved price/trial/claims
data and owners; DEC-04 public name & logo; DEC-05 LLM/search vendor, model, region, DPA; DEC-06 retention
& cookies (DPIA); DEC-07 session/replay TTL; DEC-08 SLOs & error budget; DEC-09 signing/revocation owner;
DEC-10 real handoff destinations; DEC-16 who can GO/STOP/rollback. Proposed targets (not commitments):
p95 end-to-end ≤8 s, non-generative gate p95 ≤500 ms, 99.9% monthly, groundedness ≥98%, material claims
100%, intent ≥97%, WCAG 2.2 AA, rollback <15 min (Docs 04/07/08).

Production launch requires every Doc 11 non-waivable stop (§16) cleared and individual sign-offs from
CTO, Security, DPO, QA, SRE and the Founder. The agent never declares production readiness.
