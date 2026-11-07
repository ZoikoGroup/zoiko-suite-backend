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

## STEP 13 — Assistant widget (frontend) + multi-turn carry-over (backend)

- Location chosen by the product owner: `zoiko-suite-frontend-platform` (staff admin console), branch
  `suivo/assistant-widget`. Note: Docs 01/11 place the visitor widget on the public website; in the
  console only signed-in staff see it. The widget talks only to the public API, so moving it to the
  public site is a contained change.
- Frontend: `lib/api/public-ai.ts` (server-only client; deliberately NOT the console client, so no
  X-Principal/X-Tenant identity headers ever reach the public assistant), `app/admin/assistant/actions.ts`
  (Server Actions; verify the console session; validate input), `components/admin/assistant/AssistantWidget.tsx`
  (launcher + non-modal panel, welcome + 4 starters, citations, single official CTA, clarify/refusal/
  error/retry/429/409 states, re-session on 401, end-chat deletes the session, grant kept in memory only),
  mounted in `app/admin/layout.tsx`; `.env.example` documents `ZOIKO_PUBLIC_AI_URL` / `_ORIGIN`.
- Backend: `/config` now returns `citation_links` (server-resolved, ADR-005 note); new migration
  000002 adds `sessions.pending_context` so a reply to a clarifying question keeps the original intent and
  slots (intent/slot values only, never text; wiped on delete); after two failed clarifications the
  assistant offers one route instead of a third question (Doc 02 §10).
- Found in the browser: the follow-up "In the United States, per month" lost the plan from "How much does
  the Growth plan cost?" and listed every plan — fixed by the carry-over above, re-verified in the browser.
- Verified in a real browser (console on :3000, backend dev mode on :18080): starter question answered with
  a source link and CTA; clarify → price from the verified claim only; private-invoice request refused with
  a support link; Escape closes and returns focus to the launcher; mobile 375 px full-screen sheet with no
  horizontal scroll; replies in a polite live region. Inline source links are 24 px tall (meets WCAG 2.2 AA
  2.5.8; the docs' 44 px target applies to buttons, which meet it).
- Checks: frontend `tsc --noEmit` and eslint clean; backend build, vet, all tests (incl. Postgres) pass.
- Not done: automated browser/a11y tests (the console repo has no test runner), screen-reader pass with
  NVDA/VoiceOver, 200%/400% zoom check, answer quality (the fake model is extractive; real model is DEC-05).

## STEP 13b — Public-website widget: built, then withdrawn

- A website version was built on `ZoikoSuite-NextJS` (branch `suivo/public-assistant`, never pushed) and
  verified in a browser. At the product owner's direction the widget lives **only** in
  `zoiko-suite-frontend-platform` (branch `suivo/assistant-widget`); `ZoikoSuite-NextJS` was returned to its
  `main` branch unchanged.
- Consequence to record for approvers: Docs 01/11 describe a public-website assistant for anonymous visitors.
  In the console only signed-in staff can use it. The backend is unchanged by this choice and still serves
  any allowlisted first-party origin, so a later move to the public site is a frontend-only change.

## STEP 13c — Answer coverage fixes found by probing the console widget

Trigger: in the console, "What plans are available?" asked for a country, and the follow-up "uk" got
"information is being reviewed". A probe of 24 typical visitor questions found more gaps. All fixed in
`zoiko-suite-backend` only; the console widget needed no change.

| Gap | Fix (document basis) |
|---|---|
| "hi" / "What is ZoikoSuite?" → "couldn't verify" | Greeting → welcome template (Doc 02 T01); intent-tagged page fallback when a question's words are too generic to rank (I01) |
| "What plans are available?" treated as a price question | Only price questions ("how much", "cost", "price"…) need a market and P1 claims; plan catalogue answered from the approved pricing page (Doc 02 I05) |
| "uk" after the plans answer lost context | Plans/trial answers carry intent and slots (never text) so a market reply returns that market's verified prices |
| Deliberate price conflict sat on GB | Conflict fixture moved to IE; GB gets consistent prices and a trial (fixtures are TEST-ONLY) |
| Thin knowledge: payroll, VAT, integrations, GDPR, multi-entity, AI, deployment… | Fixtures v2: 15 approved sources tagged to Doc 02 intents (still synthetic; no compliance or certification claims outside signed claims) |
| "How is data secured?" answered from the migration page | Intent-aware ranking boost; better stemming; small sections packed into coherent chunks with their headings |
| "Which countries do you support?" asked for a country | Lists every market whose availability is VERIFIED, verbatim from claims (Doc 02 I04) |
| Revoked page's question answered from an unrelated page | If the best match is revoked/expired, abstain instead of using a weaker page (Doc 03 §15) |
| "How does it compare to NetSuite?" | Route template: ZoikoSuite facts only (Doc 02 I26) |

- Probe after fixes: 24/24 questions get a grounded answer, a correct clarification, a route-only handoff
  or a policy-correct refusal; screenshot flow re-verified in the console (dark mode).
- Golden set v2: 37 cases (12 added from the probe), 37/37 pass. Drills 5/5. All 14 test packages pass.
- Still true: answers come from the extractive fake model (DEC-05), so wording is plain.

## Docs 01–05 gap closure

- Backend: `POST /turns/{turn_id}/feedback` (Doc 01 FR-09; enums only; own completed turns only; migration
  000003), `POST /handoffs` emitting `chat.handoff.clicked.v1` (Doc 05 §21; route-only), retention purge of
  expired/deleted sessions (`PUBLIC_AI_RETENTION`, dev default 24 h, DEC-06), W3C `traceparent` validation
  (Doc 05 §06), signed synonym lexicon in the KB release (Doc 03 §13), `api/asyncapi.yaml` (Doc 05 §20),
  OpenAPI updated, ADR-005 notes for the two extension endpoints.
- Console widget: per-answer "Was this helpful?" with reasons, Stop while thinking (S02), Get help link on
  errors (S08), retention notice (S09), official-link clicks recorded, mobile back button closes the sheet
  (W02), focus returns to the launcher after any close.
- Verified in the browser: synonym question answered; not-helpful + reason → "Thanks for the feedback"
  (backend 204); link click → backend 200 + `chat.handoff.clicked.v1` in the outbox log; backend down → error
  with Try again + Get help, Try again recovers after restart; 375 px back button closes the sheet and keeps
  the URL; Escape returns focus. Stop was not exercised live (the fake model answers instantly).
- Tests: feedback (own turn, enums, free text rejected, other session refused), handoff (approved URL,
  unknown route refused, event payload), traceparent, synonyms, store conformance for feedback/purge/
  append-events on memory and Postgres. All 14 packages pass; golden 37/37; drills 5/5.
- Record: `docs/suivo/docs-01-05-compliance.md`.

## Master implementation prompt gaps (2026-10-28)

- Checked the Suivo master implementation prompt against what was built. Its reference stack (Node/Fastify/React web component) is REPLACEABLE, so the discovered Go service and Next.js console stay; the delta is in ADR-006.
- Turn envelope: `answer_class` on every turn, plus optional `status_label` (governed registry vocabulary only), `market_code` and `clarification` {slot, question, options = markets + GENERAL_OVERVIEW}. Validated in `contract.Envelope.Validate` and documented in OpenAPI.
- "Show general overview" answers the pending question from market-free narrative (fixed per-intent overview query); it no longer falls through to the product overview.
- Prompts: L1-PROMPT-001, L2-RESPONSE-001 and the L3-EVIDENCE-001 wrapper are sent verbatim and pinned by hash (`model.PromptVersion`); L2-INTENT-001 is pinned for the provider classifier (R0 classifies deterministically).
- Validator: banned absolutes and action claims added (24/7, round-the-clock, works with every/all, I've submitted/created/sent/scheduled).
- Config: refuses to start outside dev/test without `PUBLIC_AI_RETENTION` from the signed policy. `GET /config` serves the `ui.*` microcopy registry and page-aware chips.
- Tests: S01–S10 multi-turn scripts over the HTTP API (`internal/acceptance/scripts_test.go`), envelope and retention tests. Full suite green.
- Docs: ADR-006, `conversation-assets.md` (T01–T30, S01–S10, banned absolutes, escalation matrix), `owasp-llm-map.md`.
- Console widget: S00–S09 reducer (`state.ts`, 7 `node --test` cases), status badge (text + icon), market qualifier, clarification chips, pre-send sensitive-data warning (blocks Send), privacy notice, Clear chat, unavailable panel with static routes, copy and chips from `/config`.
- Verified in the browser against the live dev backend: price question → country chips → United Kingdom → GB price with qualifier, source and one CTA; pasted password → warning, Send disabled; FixtureBooks → "Implemented" badge; Clear chat → S09 with retention notice.

## Widget fix-and-complete prompt (2026-10-28)

- Root cause of "I can't reach the assistant right now": no service was listening where the console calls it (127.0.0.1:18080). The backend worktree had been deleted, and the service's own default port (:8080) is held by local Docker (traefik). The API path itself was healthy (sessions 201, turns 200, replay, 422, 401, 403 all correct). Fix: dev mode now defaults to 127.0.0.1:18080.
- Duplicate bubbles: the widget's retry appended a new visitor bubble with a new client_turn_id. Fixed: one bubble per question; retry resends the same client_turn_id.
- Backend: MODEL_PROVIDER=mock (dev/test only, allowlisted model ids), synthetic seed in `data/seed` (every record `synthetic: true`; GB trials expired on purpose) served by default in dev, PUBLIC_AI_SEED_ON_START for Postgres, capability crosswalk validation, PLAN_AVAILABILITY claims, plans-and-trial questions answered from both claims, Other market option, market-named expired-trial abstention, `reason` and `secondary_intent_id` on every turn (UNAVAILABLE only for infrastructure), dev-only per-turn diagnostics log (stage + reason, never text), validator rejects the working name.
- Tests: `internal/acceptance/seed_test.go` (T1-T9, T11 and seed checks), `internal/isolation/boundary_test.go` (T12). Full suite green; golden 37/37.
- Widget: logo header (T0), retries without duplicates, S08 panel with official links and Try again, wording per failure type, privacy drawer with retention placeholder, empty-input hint, mobile focus trap. Vitest 10/10, node --test 7/7, Playwright 16/16 in Edge (light and dark).
- Not done: vector half of hybrid retrieval (lexical + aliases only, ADR-002; no embedding model approved).

## Branding, public mounting and a real public KB (2026-10-28)

- C1 diagnosis (seed dataset, before changes): release, pointers, access filters and locale were healthy (candidates before ACL = after ACL for every test question). Failures came from content and classification: 10 sources packed into 10 chunks with no coverage of Delegated Authority, banking connectors, Evidence or Intelligence; module names and live-dashboard questions fell to I29; answers began with the "SYNTHETIC seed data." preamble.
- Public KB: `data/public-kb` has 12 markdown articles (areas overview plus 11 dashboard areas), all DRAFT_PENDING_REVIEW, rewritten as public capability text with no counts, status or certification claims. Served only with KB_ALLOW_DRAFT (dev default, refused elsewhere), labelled "Draft content". Production builds with drafts fail; a runtime without the flag refuses a draft release. Ingest quarantines live-dashboard text (QUARANTINE_PRIVATE). Aliases cover AR, AP, GL, VAT, GST, SIEM, HRIS, eSignature, mTLS, delegation of authority, board resolutions, purchase order.
- Classification: live platform, tenant or dashboard questions are X01 with their own reply and chips, no model call, no CTA; a named area or alias never becomes I29 (falls to I02); recognised questions with no approved content get "I don't have approved information on that yet" with area chips.
- Retrieval reports candidates before and after access filters, top score and evidence ids; per-turn dev diagnostics log them with slots and claim verdict. Retrieval stays lexical (no embeddings, logged at start).
- Placeholder URLs (*.invalid and similar) are never exposed by /config: no CTA without a real route; sources show approved titles only.
- Connector questions without a status claim answer the capability and carry the governed "Unverified" label; signed-claim answers without a narrower status carry "VERIFIED".
- Tests: K1-K9 in `internal/acceptance/publickb_test.go`. Full suite green; golden 37/37.
- Frontend: widget moved to `components/assistant`, removed from /admin, mounted from the root layout on allowlisted public paths (/assistant now). Same-origin /public-ai/v1/* route with credentials omit and allowlisted headers. Z-tile branding per spec. Vitest 20/20, Playwright 19/19 (Edge, light and dark).
- Open decisions recorded in `docs/open-decisions.md` (taxonomy v2.1, public URLs, KB approval, retention, embeddings).

## Groq provider and the Suivo system prompt (2026-10-28)

- Inspection: neither zoiko-suite-backend (all branches) nor zoiko-suite-frontend-platform contained Groq, `openai/gpt-oss-120b`, `GROQ_API_KEY` or the greeting "I'm Suivo, ZoikoSuite's official public-facing assistant". Groq exists only in other local products (zoiko-billing-platform's Python chatbot, zoiko-logia). In this service, greetings are a template ("Hi! I'm the ZoikoSuite assistant…") and the output validator rejects any reply containing "Suivo", so the reported greeting cannot have come from this code.
- Added: Groq provider behind `model.Provider` (`internal/model/groq.go`), Suivo system prompt file embedded and hash-pinned, `BuildMessages` (system + user) with evidence metadata (source id, authority tier, approval, validity, markets), `MODEL_PROVIDER=groq` / `GROQ_API_KEY` / `GROQ_MODEL` config, ADR-007.
- Tests: Groq request shape, retry, error hygiene, timeout (model package); Tests 1-8 through the full pipeline with a local fake Groq server (acceptance); a live test that runs only with GROQ_API_KEY (not run: no key on this machine); frontend Test 7 (Playwright). Full suite green; golden 37/37.

## Four improvements: chunking, visitor-IP limits, CT map, tracing (2026-10-28)

- Chunking (Doc 03 §13):
  - one chunk per heading section, with the heading kept;
  - limits are 700 tokens maximum, 60 tokens overlap and 40 tokens minimum, estimated at 0.75 words per token (no tokenizer is approved yet; the estimate is written into the manifest's `chunk_policy`);
  - only a short preamble merges into the next section;
  - the public KB went from 12 chunks to about 24.
- Rate limiting by real visitor IP:
  - the per-IP bucket now uses the visitor's address when the TCP peer is in `PUBLIC_AI_TRUSTED_PROXIES` (default loopback);
  - `X-Forwarded-For` is read right to left, trusted hops are skipped, and entries left of the first untrusted address are ignored, so a visitor cannot choose their own bucket;
  - an untrusted peer's header is ignored;
  - the console's same-origin proxy forwards only the right-most `X-Forwarded-For` entry (`visitorIP`);
  - before this change every browser visitor shared one bucket, because all traffic arrives through that proxy;
  - deployment needs the console behind an edge that sets `X-Forwarded-For`, with the edge or console ranges listed in `PUBLIC_AI_TRUSTED_PROXIES`.
- Contract tests: `docs/suivo/contract-test-map.md` maps CT-001 to CT-052 using Doc 05 numbering. Four older test names use different numbers; the map lists them.
  - Added: CT-002 (missing or forged grant), CT-006 (unknown schema on a turn), CT-016 (invalid status enum), CT-046 (no public event-ingest route).
  - Result: 41 covered, 7 partial, 3 not applicable in R0 (020, 037, 051) and 1 gap (042, DLQ replay tool).
- OpenTelemetry (`internal/telemetry`, OTLP/HTTP, otel v1.38.0):
  - off unless `OTEL_EXPORTER_OTLP_ENDPOINT` is set;
  - spans: HTTP server (method, route, status, request id; continues a valid `traceparent`), `public_ai.turn` (outcome, reason, intent, KB release, prompt version, retrieval mode, count and public chunk ids, claim verdict, citation count) and the model call (`gen_ai.operation.name`, `gen_ai.request.model`, token usage, `error.type`);
  - the exporter is wrapped in an allowlist: any other attribute, every span event and every status description is dropped, so `gen_ai.input.messages` and similar keys cannot be exported;
  - tests check the filter, a full turn, and a real OTLP export to a local receiver (no visitor text on the wire);
  - the T12 boundary test now allows the three standard `OTEL_*` names.

## Running on Postgres and answer check (2026-10-28)

- Database per IMPLEMENTATION_PROMPT §5.1 and ADR-002:
  - setup: Postgres 16, database `public_ai`, its own `public_ai_app` role;
  - local container `public-ai-postgres-dev` on 127.0.0.1:55433;
  - the connection string is in the gitignored `services/public-ai-svc/.env.local`.
- Verified on that database:
  - migrations create all 10 `public_ai` tables;
  - `PUBLIC_AI_SEED_ON_START` signs and publishes the public KB and claims releases, and the pointers swap;
  - `TestPostgresConformance`, `TestPostgresMigrationsRoundTrip` and `TestPostgresRevocationsAppendOnly` pass;
  - turns store a request hash and the reply envelope only, and no visitor text was found in the stored rows.
- Answer check through the HTTP API on Postgres (mock model): 12 document questions (Doc 02 I01/I02, K1, K2, K4, K5, G04, G10, G16, G18, G21, G23).
  - Fixed: "ZoikoOne" written as one word was not recognised as another product, so the bot answered with ZoikoSuite modules. The cross-product rule now matches one-word, two-word and hyphenated forms of the Zoiko products named in the documents (One, Logia, Tech, Mobile, Telecom, Contracting). G23 now gives the CROSS_PRODUCT reply.

## Retriever interface and vector slot (2026-10-28)

- `internal/knowledge/retriever.go` adds:
  - `Retriever`, which the lexical index implements;
  - `Embedder`, the pinned embedding model interface;
  - `Hybrid`: BM25 plus vector recall with RRF fusion, filters before ranking, the stale/revoked verdict kept, and a keyword fallback when embedding fails.
- `release.Snapshot.Retriever` is used by the pipeline. `release.Runtime.Embedder` is the slot; it is unset in R0, so retrieval stays keyword-only, and `VectorError()` reports a failed release embed.
- Tests: `knowledge` (default retriever, vector recall, private chunk filtered, revoked kept, query fallback, bad vectors) and `release` (lexical default, hybrid with an embedder, lexical fallback without going not-ready). ADR-002 updated.

## Gap review against the documents (2026-10-28)

- Answer quality:
  - "What is ZoikoSuite?" now gets the approved platform overview from Doc 02 T02 (new draft article `00-overview.md`, intent I01) instead of the module list.
  - I01 now also catches "Explain ZoikoSuite to me", "What's ZoikoSuite?", "Zoiko Suite" written as two words, "describe…" and "new to…"; "Explain ZoikoSuite to me" was previously answered as out of scope.
  - I22 now catches "sign-in" with a hyphen and "account access"; I23 now catches "existing customer" and ticket phrasings.
  - New `policy.TestDoc02CatalogExamples` is generated from the Doc 02 catalog; all 30 intents classify correctly.
- Contract tests:
  - CT-014: the widget ignores a reply whose `turn_sequence` is older than one already shown;
  - CT-032: tests for forged, path, case-changed and empty citation anchors;
  - CT-039: test that unknown route keys and routes outside their window resolve to nothing;
  - CT-048: test that a kill switch thrown during generation withholds the answer.
  - Map is now 45 covered, 3 partial, 3 not applicable in R0, 1 gap.
- Compliance and traceability rows that were out of date are updated: chunking, OpenTelemetry, the Groq and Postgres stack, the CT mapping, and the automated accessibility checks.

## Support dead end and live-data reply (2026-10-28)

- Reported from the console (screenshot): "Active Microservices Directory" was refused (correct: the public assistant has no route to live platform data, INV-01 and Doc 03 §27). Its own "Get support" button sent "I need help from support", which was classified as I21 (help docs). With no guide article it ended in "I couldn't verify that" / "Which ZoikoSuite area…?".
- Fixes:
  - I23 now catches "help from support", "need support", "get support", "support team", "talk/speak/reach support";
  - a help or support intent (I21–I24) with no approved guide now gives the Doc 02 T16 support reply instead of the area question (G26);
  - the I23 reply now uses the Doc 02 T16 copy;
  - the live-data reply (X01) now tells the visitor to use the ZoikoSuite application they are signed in to, following Doc 02 T21.
- Test: `TestK5_LiveMicroservicesRefused` now also follows the "Get support" button through to the I23 support reply.
- Still open: no "Open support" button is shown, because the support route has only a placeholder URL (`*.invalid` routes are never exposed). It appears once Product supplies the real Support Center URL (`docs/open-decisions.md`).

## Live platform data: refusal kept, chatbot off /admin (2026-10-28)

- Product decision: keep refusing live platform data (Docs 01 FR-16, 03, 04, 06, 11). The authenticated copilot is deferred to R2 as OD-06 in `docs/open-decisions.md`. No new ADR.
- The X01 reply goes back to the fixed text "I can't see live platform data or account information. I can explain what ZoikoSuite's services and modules do, using public information.", which reverses the T21 rewording from earlier today. The chips are Explore modules and Get support; there is no CTA and no model call.
- Fixed: "domain status" was classified as I29 and sent to the model. The live-data rule now also matches domain, service, microservice, cluster and platform status, health, readiness and uptime, plus "domains monitored", "operational status", "needs attention" and "services ready".
- New test `acceptance.TestLivePlatformQuestionsAlwaysRefused` covers "What are the Active Microservices", "how many services are ready", "show cluster readiness", "domain status" and two variants: each gets X01, the fixed text, no CTA or citations, no digit, and no model call.
- Frontend:
  - the widget is active only on `/assistant` (removed from `/admin`);
  - ESLint `no-restricted-imports` and `import-boundary.test.tsx` fail if chatbot code imports dashboard, health, auth or session modules or references `getDomainHealth`; both were checked with a probe import;
  - a Playwright test checks that `/admin` shows no launcher and sends no assistant calls when signed in.

## Assistant shown on /admin again (2026-10-28)

- Product owner decision: the launcher returns on `/admin` and its pages. It is still the anonymous public assistant: it sends no cookies or Authorization, still refuses live platform data (X01), and the import-boundary lint rule and test stay. The mount test, the Playwright test, the widget README and OD-06 are updated.

## Answer review: modules, overview and Legal and Contracts (2026-10-28)

The answers were checked against the documents using console screenshots.

- "What modules does ZoikoSuite include?" listed the 11 dashboard areas, which is wrong per Doc 03 §5. It is now answered from a new draft article, `00-modules.md`, with:
  - the four module domains and ten canonical modules named in Doc 03 §5;
  - the Governance Platform and Platform Foundation tiers from Doc 02 T03;
  - the availability qualifier.
  - The dashboard-areas article is retitled "Areas in the ZoikoSuite console".
  - OD-01, ADR-007 and the taxonomy crosswalk no longer claim that the documents give no module names; stable IDs are still pending v2.1.
- "What is ZoikoSuite?" dropped the T02 qualifier "The precise capabilities available depend on market, plan and deployment status." The mock model now always keeps an availability qualifier from the passage it answers from (G01).
- "What does Legal and Contracts do?" left out approval-bound execution. Retrieval now adds the next section of the top article as supporting evidence (`knowledge.Index.Neighbor`, with every mandatory filter applied), and the mock adds that section's opening sentence. Groq receives the same evidence.
- Tests: K3 (modules per Doc 03 with the qualifier and no count), K10 (overview qualifier), K11 (Legal and Contracts complete), and Groq2 (cites `kb-core-modules`).

## Answer quality pass (2026-10-28)

A 47-question sweep covered every Doc 02 intent and every KB article. It found six kinds of weakness, each fixed at its cause:

1. **The wrong article answered.**
   - Generic verbs (support, handle, work, use, get, need, want, offer, provide) are now stopwords, so "Does it support HR?" no longer lands on Evidence ("the work they support").
   - An article whose title the question names ranks higher (×2), so "the compliance module" finds Compliance, not the module list.
   - Added the synonym "hr" → human resources.
2. **No source for an intent.** Doc 02 product copy was added as draft articles: T04 multi-entity (I03), T09 migration (I11) and T15 governance and approvals (I15).
3. **Boundary questions answered from unrelated text.** Doc 02 boundary replies now cover data residency (T11, I14), roadmap or unverified availability (T18, I25), accessibility (T30, I27) and responsible AI (I30, from the welcome disclosure). They are used only when no retrieved article is tagged for the intent. For example, "Where is data stored?" had been answered with the Integrations text.
4. **Classifier misses.**
   - "What can ZoikoSuite do?" is now I01.
   - "who can approve", "on behalf of" and "act for" are now I15.
   - The openers "how do I" and "how to" are no longer I21 phrases, so they can't outscore the topic ("How do I migrate…" is I11).
   - The console-areas article is no longer tagged I25 (roadmap).
5. **Caveats cut off.** A passage of three sentences or fewer is used whole, so endings such as "…a scoped review is needed before any timeline or compatibility commitment" are kept.
6. **Second section missing.** A next section that search had already found lower down is moved up to second place, and short sections are added whole (Compliance, Delegated Authority, Security and Trust).

- Test: `acceptance.TestAnswerQuality` has 20 questions, each with its expected source (or Doc 02 reply) and required phrases, and fails if an unrelated article is cited.
- Still open: Intelligence and Reporting has only a placeholder article (owner review), and the content is draft or synthetic until approved.

## Answer shaping and the model's answer-quality rules (2026-10-28)

- Dev composer (mock model):
  - a long list sentence such as "Lead: Label (items), …" is shaped to the question;
  - a general question gets a bulleted list with bold labels, and the caveat on its own line;
  - a question that names one group ("Does it handle payroll?", "HR", "treasury") gets only that group plus the caveat, with no extra sections;
  - shaping only reorders the article's own words (adding "include").
  - Tests: `acceptance.TestAnswerShaping` and `model.TestShape*`.
- Model contract (Groq): `model.ServerContract` now asks the model to answer the exact question in the first sentence, use only the records on the question's topic, keep every availability, scope or status qualifier, and use a short "- " list for three or more parallel items. The contract is included in the prompt-version hash (`SERVER-CONTRACT-002`), and the Doc 02 prompts are unchanged.

## Work moved to the main checkout; chatbot dev port is 8001 (2026-10-28)

- At the product owner's request, all chatbot work now lives in `C:\zoiko-suite-backend` (branch `Madhu`) instead of the `zoiko-suite-backend-r0` worktree (`suivo/r0-build`).
  - The worktree's 3 unmerged commits and its uncommitted changes were applied as one patch from the merge base `5ac641a2` (55 changed files, applied cleanly) plus 58 new files. All 113 files match the worktree exactly.
  - Nothing was committed or pushed. The gitignored `.env.local` (local Postgres) was copied over.
- Dev port: the chatbot backend now defaults to `127.0.0.1:8001`, as do the console proxy default, `.env.example`, the README, `scripts/run-dev-postgres.ps1` and `docker-compose.public-ai.yml`. Port 8000 stays with the main ZoikoSuite gateway (`GATEWAY_PORT`, `ZOIKO_GATEWAY_URL`). Playwright still starts its own backend on 18181.
- Verified from the main checkout: backend 18/18 packages, golden 37/37, Postgres tests, Vitest 40/40, Playwright 52/52, and the console (3000) reaching the chatbot (8001).

## Answers aligned to Doc 02's approved copy (2026-10-28)

- Fixed replies now use Doc 02 §12 visitor-facing copy word for word:
  - T01 welcome, T05 market-dependent pricing question, T07 enterprise (new, I09), T11, T16, T17 sign-in, T18;
  - T19 cross-product ("This assistant" for the uncleared working name);
  - T20 advice, T21 records and actions (X01 records and X02), T22 sensitive data, T23 security reporting;
  - T24 unavailable (the widget shows the reference), T25 off-topic, T26 ask for a person, T30.
  - Not changed: the X01 live-platform reply keeps the product owner's wording.
- `policy.TestRepliesUseDoc02Copy` reads Doc 02 at test time and fails if any of these 16 replies drifts.
- Knowledge articles gained Doc 02 product copy:
  - T08 APIs and connectors (integrations);
  - T12 jurisdictions are not universal (tax);
  - T13 workforce and payroll (new `00-workforce.md`);
  - T14 assurance and admissibility (evidence).
- Claim-gated facts stay claim-gated. T10 ("SOC 2/ISO not held") and T06 ("30-day trial") are not fixed text; an unverified certification now gets T10's due-diligence guidance instead (`TplAbstainCertification`).
- G12: "Do you / does it / can ZoikoSuite + verb" with no "my", "our" or "for me" is a capability question, not an action request (X02), so "Do you file taxes in Germany?" is I17 behind the regional coverage gate. "File my taxes" is still refused. The tax intent now also matches taxes, taxation, tax filing and tax returns.
- G14: admissibility and assurance questions are I16. They are answered only from a passage that addresses admissibility, otherwise with T14; they are never refused as advice and never answered from unrelated text.
- Tests: `TestDoc02CapabilityVersusAction`; TestAnswerQuality is updated (workforce T13, integrations T08); golden A08 now expects T01's "ZoikoSuite AI guide".
