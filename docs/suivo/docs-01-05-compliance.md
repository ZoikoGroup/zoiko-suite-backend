# Suivo R0 — compliance with Documents 01–05

What each document requires, what is built, and the evidence. Code lives in
`zoiko-suite-backend/services/public-ai-svc` (branch `suivo/r0-build`) and the console widget in
`zoiko-suite-frontend-platform` (branch `suivo/assistant-widget`). All data is TEST-ONLY synthetic and the
model is the fake extractive provider, so **"Built" means the mechanism exists and is tested, not that it is
approved for production.** Status values: **Built**, **Partial**, **Blocked** (needs a decision or approval),
**Deviation** (built differently, recorded in an ADR).

## Document 01 — Product requirements, journeys, wireframes (ZS-AI-PUB-001)

| Requirement | Status | Evidence |
|---|---|---|
| Public-only boundary: no tenant data, no actions, prompt never an authorisation grant | Built | `isolation` test (SEC-T049); X01/X02 refusals; no tools in the model gateway |
| FR-01 welcome, 4 quick actions, type immediately | Built | Widget welcome + 4 starters; greeting → T01 welcome |
| FR-02 capabilities mapped to approved taxonomy and public routes | Built | Taxonomy-checked ingestion (`capability_ids`); `core_modules` route |
| FR-03 pricing/trial from dated, market-specific source; no invented prices | Built | P1 claims only, verbatim; numbers not in evidence rejected; golden A02, A11, B01–B02, C02–C04 |
| FR-04 demo qualification → owned HTTPS route, no sensitive data | Built (route-only) | I08 route template; no form data collected |
| FR-05 trust Q&A distinguishing readiness vs certification | Built | SOC 2 HOLD never stated; ISO from claim; golden C01, A05 |
| FR-06 integrations: confirmed methods only | Built | INTEGRATION_STATUS claims; built/beta/planned labels in source |
| FR-07 support routing + human path | Built (route-only) | I21/I23/I24 templates; never claims a ticket |
| FR-08 1–3 verified links; no answer without evidence | Built | ≤3 citations from what was used; ABSTAIN otherwise |
| FR-09 session controls + feedback | Built | Minimize/reopen, end chat (DELETE), **useful / not useful + reason** (`POST /turns/{id}/feedback`) |
| FR-10 PII redaction, injection defence, abuse/rate controls | Built | `policy.Redact`, X06, limiter, spend ceilings |
| FR-11 WCAG 2.2 AA, en-US, localisation gated | Partial | Automated: axe, keyboard-only, Tab order, focus return, contrast, target sizes, zoom and narrow widths (Playwright). Manual screen-reader pass not done |
| FR-12 typed events, trace IDs, redacted diagnostics | Built | Six CloudEvents; request id; `traceparent` validated |
| FR-13–FR-16 (P1/P2) | Not in R0 | Out of scope by design |
| States S00–S09 | Built | S02 thinking + **Stop**; S08 error with Try again + **Get help** + reference; S09 feedback + **retention notice** ("Ending the chat clears this conversation") |
| W02 mobile: back button closes sheet first | Built | `popstate` handling, verified at 375 px |
| Answer contract: ≤120 words, ≤1 primary CTA, disclosure | Built | `validate.truncateWords`; one action; "AI assistant. Product information only." |
| Journeys J01–J04 | Built | J01 plans/entities, J02 trial (no payment data), J03 private invoice refused, J04 SOC 2 not claimed |
| p95 ≤3 s start / ≤10 s complete, 99.9 % availability | Blocked | Needs staging and an approved model (DEC-05, DEC-08) |
| Gates G0–G7 | Blocked | All PENDING (see `r0-readiness.md`) |
| Placement on the public website | Deviation | Product-owner decision: console only (build log STEP 13b) |

## Document 02 — Conversation design, prompts, intent catalog (ZS-AI-PUB-002)

| Requirement | Status | Evidence |
|---|---|---|
| Decision pipeline: sanitize → policy → classify → slots → retrieve → compose → validate → deliver | Built | `orchestrator/pipeline.go` |
| States C00–C12 | Built | Clarify, evidence, answer, uncertainty, handoff, refusal, failure paths all tested |
| Intent catalog I01–I30 | Built | Rule classifier; probe of 24 typical questions all handled; golden 37 cases |
| Exceptions X01–X10 before generation, highest risk wins | Built | `policy.TestExceptions`; X10 via limiter |
| Slots from the visitor's words, never IP | Built | `TestSlots` |
| Clarification: ≤1 clarifier, market change invalidates evidence, give up after two | Built | Carry-over of intent/slots (never text); `TestClarificationCarriesPlan`, `TestClarificationGivesUpAfterTwo` |
| Prompt layers L0–L5; evidence wrapped as non-instructional data | Built | `model.BuildPrompt`; escaping test |
| Response envelope / answer classes | Deviation | Doc 05 envelope is master; Doc 02 answer classes mapped (ADR-005) |
| Tone: no superlatives/guarantees without a scoped source | Built | Prohibited-phrase validator |
| Templates T01–T30, no unresolved placeholders | Partial | Exception, route-only, welcome, comparison, abstain/clarify templates; wording is a dev draft pending Legal/Product |
| Escalation matrix: verified route IDs only | Built | Routes only from the signed registry; unknown routes hidden |
| Microcopy, page chips | Partial | Microcopy built; page-aware chips not used in the console (no public pages) |
| i18n en-US, no currency conversion, no inferred jurisdiction | Built | Locale filter; market only from the visitor |
| Release bars (intent ≥95 %, groundedness ≥95 %, pricing 100 %, safe abstention ≥98 %) | Partial | Golden dev set 37/37 passes; independent holdout not authored |
| Analytics events (§27) | Deviation | Doc 05's six events are the event contract; feedback stored as enums, not a seventh event |

## Document 03 — Knowledge base architecture and source governance (ZS-AI-PUB-003)

| Requirement | Status | Evidence |
|---|---|---|
| Planes: data / control / response; response plane never calls private systems | Built | Separate stack, signed releases, isolation test |
| Source precedence P0–P4 + blocked sources | Built | `authority_tier`; private/draft/off-site sources quarantined |
| Source registry: every field mandatory, null never approved | Built | `REJECT_UNREGISTERED` (caught missing `subject_type` in fixtures) |
| Chunk record inherits scope, validity, revocation | Built | `knowledge.Chunk` |
| Claim ledger + statuses ACTIVE/CONDITIONAL/TARGET/HOLD/RETIRED | Built | `claims.Resolve`; only ACTIVE answerable |
| Ingestion state machine 00–12 with failure states | Built | `TestIngestionStates` |
| Two-person approval for P1 | Built | Claims need two approval refs; P1 sources two approvers; no self-approval |
| Immutable signed releases, atomic promotion, rollback keeps revocations | Built | Ed25519; pointer swap; `TestRevocationSurvivesRollback`, drill DR-03 |
| Chunking 300–700 tokens, structured-first, hybrid search | Partial | One chunk per section, 700-token cap with 60 overlap (estimated tokens); claims first; BM25 + intent boost; `Retriever` interface and `Hybrid` vector slot built, off until an embedding model is approved (ADR-002) |
| **Signed synonym lexicon** ("AR" → accounts receivable) | Built | `synonyms` in the KB release; `TestSynonymLexicon` |
| Fixed retrieval order (validate → intercept → decompose → filter → claims → search → conflict → compose → validate → trace) | Built | Pipeline order; best match revoked/expired → abstain |
| Evidence gates: P1 over P3, typed values, CLAIM_CONFLICT without voting | Built | `TestClaimDecisions`, conflict record written |
| Abstention codes | Built | Mapped to ABSTAIN/CLARIFY templates (ADR-005) |
| Revocation ≤5 min, price propagation ≤15 min | Built | 5 s refresh + ledger re-check at commit |
| Real content: v2.1 crosswalk, price book, claims register, DPIA | Blocked | DEC-02, DEC-03, DEC-06 |

## Document 04 — Solution architecture and technical implementation (ZS-AI-PUB-004)

| Requirement | Status | Evidence |
|---|---|---|
| Invariant: no route/credential to private data planes | Built | Own compose stack + network; isolation test |
| Zones Z0–Z6 | Built (dev) | Edge middleware, orchestrator, read-only KB plane, `kbctl` staff plane; production network policy not verified |
| Components (widget, API, orchestrator, policy, claims, model gateway, routes, session store, KB publisher, telemetry) | Built | One service with internal packages (allowed by §05) |
| Explicitly denied: tools, web search, fine-tuning, CRM writes, HTML output | Built | `tools=[]`; markup rejected; no CRM code |
| Request lifecycle EDGE → … → RESPOND | Built | Middleware + pipeline |
| Conversation states incl. PROVIDER_UNAVAILABLE | Built | Drill DR-04 |
| Endpoints, envelope `zs-public-answer-v1`, errors | Built | `api/openapi.yaml`, contract tests |
| Entities (session, turn, evidence, policy decision, route, model invocation, abuse counter, release) | Built | `store` schema + migrations 001–003 |
| Feature flags + environment variables | Built | `flags`, `.env.example` |
| Stack choice (framework, search, LLM, cloud) | Partial / Blocked | Go + Postgres 16 (`public_ai` schema, own role); Groq `openai/gpt-oss-120b` provider built (ADR-007) pending vendor/DPA approval (DEC-05); cloud not chosen |
| Targets: turn availability 99.9 %, gate p95 ≤500 ms, answer p95 ≤8 s, rollback <15 min | Partial | Rollback drill passes; latency/availability need staging |
| OpenTelemetry | Built | OTLP/HTTP exporter, off unless an endpoint is set; allowlisted span attributes only, no content (`internal/telemetry`) |

## Document 05 — API integration, service contracts, event architecture (ZS-SUIVO-ENG-005)

| Requirement | Status | Evidence |
|---|---|---|
| `/public-ai/v1` base path, TLS-ready, strict JSON, `X-Request-ID`, `no-store` | Built | Middleware tests |
| **`traceparent` propagated only after validation** | Built | `middleware.TraceID`; `TestTraceparentValidation` |
| Session header grant, never logged; no cookies | Built | SHA-256 at rest; log canary test |
| Endpoints: sessions, turns, events (404 in R0), delete, config, health | Built | CT tests |
| Idempotency rules (replay, mismatch 422, header mismatch 400, 409 conflict) | Built | CT-009/010/011 |
| RFC 9457 errors incl. 415 | Built | `problem` package |
| Config: routes from signed allowlist, hide unpublished CTA | Built | + `citation_links` extension (ADR-005) |
| Internal operations (claims:resolve, knowledge:retrieve, routes, releases, model:generate) | Built (in-process) | Allowed by §16 |
| Synchronous turn pipeline incl. validation, repair once, outbox | Built | Pipeline + `CompleteTurn` transaction |
| CloudEvents envelope, transactional outbox, at-least-once, DLQ | Built | `events` tests |
| Six events | Built | **`chat.handoff.clicked.v1` now emitted** via `POST /handoffs` when a visitor opens an official link |
| **AsyncAPI 3.0 channel document** | Built | `api/asyncapi.yaml` |
| No arbitrary inbound webhooks in v1 | Built | None exist |
| Retention / deletion contract | Built (dev default) | DELETE wipes session context and turns; **purge of expired sessions** every 10 min, `PUBLIC_AI_RETENTION` 24 h pending DEC-06 |
| Contract tests CT-001–052 | Partial | Mapped 1:1 in `contract-test-map.md`: 45 covered, 3 partial, 3 N/A in R0, 1 gap (CT-042) |
| Gates G0–G5 | Blocked | All PENDING |

## What still blocks production (cannot be solved in code)

DEC-01 hosting/gateway · DEC-02 taxonomy v2.1 · DEC-03 real price/trial/claim data and owners · DEC-04 public
name and logo · **DEC-05 AI model vendor (answers stay extractive until then)** · DEC-06 retention, cookies,
DPIA · DEC-07 TTLs · DEC-08 SLOs · DEC-09 signing ownership · DEC-10 real handoff URLs · DEC-16 GO/STOP
authority · Legal/Product review of all template wording · public-website placement decision.
