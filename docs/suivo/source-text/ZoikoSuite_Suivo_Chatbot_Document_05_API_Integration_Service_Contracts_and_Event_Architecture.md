<!-- Extracted from ZoikoSuite_Suivo_Chatbot_Document_05_API_Integration_Service_Contracts_and_Event_Architecture.docx -->

ZOIKO GROUP  /  ZOIKOTECH INC.  /  ZOIKOSUITE

SUIVO  /  PUBLIC AI CHATBOT

API Integration, Service Contracts & Event Architecture

ENGINEERING DOCUMENT 05  •  VERSION 1.0  •  24 OCTOBER 2026

Contract-first public chatbot engineering handoff: machine-typed requests, responses, service ownership, canonical evidence sources, first-party route handoffs, event safety, reliability, contract verification and controlled rollout.

| APPROVAL STATUS PROPOSED / submitted for Founder, Global CTO, Product, Architecture, Security, Privacy, Data Governance and QA review. All routes, services and event topics are target contracts—not a claim of implemented endpoints. Implementation starts only after a repository inventory and explicit approvals. |
|---|

| Document control | Value / controlled interpretation |
|---|---|
| Document ID | ZS-SUIVO-ENG-005; series item 05 of the ZoikoSuite public chatbot controlled handoff. |
| Scope | PUBLIC_ANONYMOUS website chatbot; explanation, guided navigation, public support/demo/trial handoff only. |
| Authority | Docs 01-04 as submitted sequence; signed P0 policy, P1 business registries and P2 taxonomy/route registry take precedence over examples. |
| Audience | Backend, Frontend, API Platform, AI/ML, Knowledge Ops, QA, SRE, SecOps, Product and Legal. |
| Implementation status | Proposed logical reference design; endpoints, infrastructure, owners, live product entitlements and releases unverified. |
| Confidentiality | Internal controlled engineering specification; not a public API guarantee or compliance attestation. |

CRITICAL DESIGN RULE — The public chatbot may never receive a customer-business access token, direct query privileges for customer records, or tool access to ledger, payroll, tax, HR, banking, filing, payments, audit administration or tenancy management APIs.

## 01  Executive contract determination & critiques addressed

This document translates the requirements and target architecture in Docs 01–04 into implementable service boundaries. Its objective is to eliminate ambiguous responsibility, divergent schemas, unsafe retries, unverifiable commercial claims and surprise side effects between website, model, product registries and knowledge control plane.

| Earlier implementation risk | Refined contract decision | Acceptance evidence |
|---|---|---|
| LLM determines API calls or authority | Orchestrator owns a fixed capability matrix; zero privileged tools; route actions are signed allowlist values. | Negative IAM, firewall and tool-enumeration proofs. |
| Unclear idempotency/retry scope | One POST turn per client_turn_id + session; duplicate payload returns same committed result; mismatch blocks. | Replay and mismatch contract tests. |
| Conflicting pricing or country content | Signed P1 registry is definitive for amount, eligibility, geography and time; P3 copy cannot elevate a claim. | Contradiction and expiry fixtures. |
| Streaming leaks unverified text | Launch with validated JSON final answer; optional stream may emit operational progress but no unvalidated token deltas. | Network replay and UI stream tests. |
| Events treated as commands | CloudEvents-style internal signals are outbox-issued, deduplicated and cannot authorize business writes. | Consumer idempotency and privilege proof. |
| Design viewed as a deployed service inventory | All paths explicitly tagged PROPOSED until mapped to production code and tested. | As-built service mapping and ADR sign-off. |

## 02  Precedence, cross-document reconciliation & naming control

| Source | Authoritative contribution | Non-negotiable reconciliation |
|---|---|---|
| Doc 01 Product Requirements & Wireframes | Visitor journeys, public-only privacy scope and UI states. | No authenticated workflows smuggled into integrations. |
| Doc 02 Conversation/Prompt/Intent Catalog | I01–I30 intents, exception patterns, answer statuses and human handoff semantics. | Return typed intent_id; never interpret intent as authorization. |
| Doc 03 Knowledge Base/Source Governance | P0–P4 authority, source registry, claim ledger, taxonomy and signed KB release. | Claim gating deterministic, citations resolvable; revocation takes precedence. |
| Doc 04 Solution Architecture | Public service edge, chat orchestration, model gateway, core boundaries and 6 public endpoints. | Preserve existing path/field names; gaps become change-controlled ADRs. |
| ZoikoSuite live service/backend inventory | Actual route ownership, gateway, hosting, IDs, deployed service semantics. | NOT PRESENT in this series; must be obtained before binding actual production services. |

Stable assistant identity is platform-scoped; display name “Suivo” is an internal working brand and does not confer public trademark clearance. Product ID must be server-supplied as “zoikosuite”; NEVER use the display name as an IAM or tenancy boundary.

## 03  Scope, authority and prohibited endpoints

| Interaction | In v1 public scope? | Server-side enforcement |
|---|---|---|
| Product/module FAQs, supported use cases, public comparison | YES — approved content only | Approved P2/P3 release + P0 safety. |
| Verified current plan, trial and market claims | CONDITIONAL | P1 claim resolution with signed effective-date record. |
| Public demo/pricing/support/signup route | YES — navigation only | Signed route-key allowlist; no in-chat PII submissions. |
| Create CRM prospect/contact, booking or ticket | NO | Separate form/application contract and consent required. |
| Read bank, accounting, HR/payroll, tax, legal contracts or statements | NO | Network/identity deny; no tenant or customer bearer token. |
| Upload documents / execute tools / run filing or payment | NO | No file intake, tool privileges or transactional action schemas. |
| Read login-based subscription or support case status | NO | Official login/support route only. |

## 04  Target integration diagram and segregation

Figure 1. Proposed logical API and event integration. All customer finance, payroll, HR, tax and bank integrations remain denied.

The website widget is an untrusted client. Every model output is untrusted proposed copy. The API service independently validates authorization, data scope, evidence provenance, outbound URLs, reply structure and publication status.

## 05  Service catalog, logical owners & permitted connections

| Service / component | Owner (role) | Allowed callers / data | Prohibited access |
|---|---|---|---|
| Public widget / BFF adapter | Web Frontend | Origin-bound visitor session and read-only page enum. | No signed business data or API secrets. |
| Public API edge | API Platform | Browser to /public-ai/v1 only. | No internal KB staff edit endpoints. |
| Turn orchestration runtime | AI Platform | Session metadata, approved KB/claim services. | No finance, tax, HR, payroll credentials. |
| Public KB read service | Knowledge Ops | Released public chunk vectors and citation registry. | No draft, confidential or private index. |
| P1 claim-resolution service | Commercial/Product/Security | Signed typed fact registry and active market/time context. | No page text as authority. |
| Route-resolution service | Web Platform | Signed route_key->approved first-party destination. | No arbitrary user-provided redirect URL. |
| Approved model gateway | AI Platform / Sec | Minimized prompts+evidence, timeout and budget. | No HTTP tools or free network fetch. |
| Event outbox / message broker | Platform SRE | Redacted events from services/admin. | No chat transcript transport by default. |
| Knowledge control plane | Content/Legal/Sec | Staff identity and dual approvals only. | No browser or model-initiated releases. |
| Audit/observability sink | SRE/Security | Opaque IDs, decisions, timings, digest. | No raw prompts/secrets by default. |

## 06  Cross-service requirements — transport, versioning & headers

| Rule | Required contract / treatment |
|---|---|
| Transport | TLS 1.2+ minimum per approved enterprise gateway policy; require TLS 1.3 where supported; deny cleartext/mixed content. |
| API base path | /public-ai/v1 (PROPOSED). Internal services never publicly routable. |
| Versioning | Major wire contract in URI and schema_version body. Compatible additive fields only if clients tolerate them; breaking fields require /v2. |
| Request correlation | Server-issued X-Request-ID on every response. Propagate W3C traceparent only after inbound validation; never trust the client to set privilege. |
| Session binding | X-Public-Chat-Session opaque short-lived bearer grant, cookie strategy only after specific CSRF/CORS/consent security approval. |
| Retries | Idempotency-Key header required for side-effecting POST /turns and POST /sessions; documented dedupe TTL approved by Sec/SRE. |
| JSON | Content-Type: application/json; UTF-8; strict request/response schema, body size and nesting caps. |
| Caching | No-store on sessions/turns; ETag/short max-age on safe public config; do not cache cross-market answers. |
| Public errors | application/problem+json following RFC 9457; no stack trace, inference vendor or internal architecture in detail. |
| Standard provenance | OpenAPI 3.1-compatible description as Doc 04 baseline; evaluate v3.2.1 published 10 Sep 2026 in toolchain ADR before changing formal syntax. |

## 07  Public endpoint registry — stable contract skeleton

| Method / route — proposed | Response / idempotency | Capability and boundary |
|---|---|---|
| POST /public-ai/v1/sessions | 201; Idempotency-Key recommended/required by final spec | Issue anonymous session ID + scoped session grant; no customer sign-in. |
| POST /public-ai/v1/turns | 200 typed final; Idempotency-Key required | One active turn per session; deny oversized or unsafe input. |
| GET /public-ai/v1/turns/{turn_id}/events | 200 text/event-stream OPTIONAL | Session-bound progress/final; no raw LLM token streaming in v1. |
| DELETE /public-ai/v1/sessions/{session_id} | 204 (repeat own deletion ->204) | Clear governed ephemeral session state; never promise deletion of legally required aggregates. |
| GET /public-ai/v1/config | 200 + ETag | Public capabilities, safe route keys, notice and feature flags only. |
| GET /public-ai/v1/health/public | 200/503 coarse generic | Status only; no names of internal providers or regions. |

Engineering MUST NOT declare this registry deployed by reading this table. Route ownership, gateway prefix, code location and access policies require an as-built inventory in section 29.

## 08  Session creation — POST /sessions

| Field / format | Source & validation rule | Privacy / rejection condition |
|---|---|---|
| request.locale (BCP 47) | Optional; explicitly supported locale or server default. | No free-form user profile. |
| request.market_code (ISO 3166-1 alpha-2) | Optional unverified visitor choice; allow listed countries only. | Never infer entitlement from IP alone. |
| request.page_context | Optional enum home / pricing / core_modules / demo / support / other. | No raw URL, DOM or referrer capture. |
| response.session_id | Opaque random ID, not user/account ID; single public-assistant scope. | No tenant ID in browser or events. |
| response.session_grant | Short-lived cryptographic session proof, bound to session and origin/abuse controls. | Return through first-party HTTPS; never log grant. |
| response.expires_at | RFC 3339 UTC timestamp and renewal policy. | Expired grants fail; no silent account re-auth. |
| response.policy_version / capabilities | Resolved policy and allowed UI features only. | Never reveal admin settings or provider keys. |

Reference JSON — proposed session request

{  "schema_version":"1.0",  "locale":"en-US",  "market_code":"US",  "page_context":"pricing"}

Reference JSON — proposed session response

{  "schema_version":"1.0",  "session_id":"pub_s_opaque-example",  "session_grant":"opaque-sensitive-grant-placeholder",  "expires_at":"2026-10-24T13:00:00Z",  "policy_version":"pub-policy-1.0",  "capabilities":{"streaming":false,"lead_capture":false,"file_upload":false}}

On creation, the server binds session subject, issued-at, expiry, product ID and allowed public scope. Reuse a client-generated idempotency key to return the original session creation result only within documented deduplication TTL; rate limits still apply.

## 09  Submit turn — POST /turns request schema

| Field / format | Source & validation rule | Privacy / rejection condition |
|---|---|---|
| schema_version: string | Required fixed value “1.0” for this contract. | Reject unsupported major versions. |
| session_id: opaque | Required; must equal validated session-grant subject. | Do not accept another visitor session. |
| client_turn_id: UUID | Required stable ID for one visitor submission. | Mismatch to Idempotency-Key -> 400. |
| message: string | Required; Unicode NFC; 1-2000 chars proposed; strip forbidden controls. | Sensitive patterns -> intercepted per Doc 02, not stored as transcript. |
| locale: BCP47 | Optional permitted locale; server default and locale-aware copy. | Never used as policy bypass. |
| market_code: ISO alpha-2 | Optional explicit visitor choice; validated against supported list. | Unknown market -> CLARIFY, not infer rollout. |
| page_context: enum | Optional approved context enum. | Reject/ignore arbitrary page DOM values. |
| requested_response_mode | “json” for P0 launch; “sse” only behind signed feature gate. | No unvalidated streaming tokens. |
| Idempotency-Key header | Required; UUID string; equal client_turn_id by v1 convention. | Same key/different normalized payload -> 422. |
| X-Public-Chat-Session header | Required; cryptographically validated session grant. | Never accept forwarded customer OAuth token. |

Reference JSON — proposed turn request

{  "schema_version": "1.0",  "session_id": "pub_s_opaque-example",  "client_turn_id": "78f74a7d-752c-4716-869d-2bf49e36c403",  "message": "Is the Growth plan available in my country?",  "locale": "en-US",  "market_code": "US",  "page_context": "pricing",  "requested_response_mode": "json"}

## 10  Submit turn — canonical response envelope

| Field | Requirement and permitted values | Client validation |
|---|---|---|
| schema_version | Required “1.0”. | Reject unsupported major and show safe fallback. |
| turn_id / request_id | Required opaque IDs, server generated, unique. | Render-safe diagnostic reference only. |
| turn_sequence | Integer >= 1; monotonic within session. | Ignore stale late responses. |
| status | ANSWERED / CLARIFY / REFUSED_ACTION / ABSTAIN / HANDOFF / UNAVAILABLE. | UI mapping from Doc 02; never infer success from HTTP 200. |
| intent_id | I01–I30 or X-series controlled exception; UNKNOWN if no fit. | May be hidden from visitor; telemetry only. |
| text | Server-approved output <=5000 chars; plain text/allowlisted markdown only. | Escape HTML/URL; no executable markup. |
| citations | Array max 5 with released source_version_id + claim_ids + safe url_key. | Resolve href only through approved route/citation resolver. |
| actions | Array max 3; type OPEN_OFFICIAL_ROUTE; route_key from signed allowlist. | No open redirect, no action POST. |
| knowledge_release_id | Required published immutable release reference. | Observe for source-revocation invalidation. |
| policy_version | Required active approved policy revision. | Report mismatched stale response. |
| flags | Optional public booleans like requires_human; no internal risk score. | Never render secret or security reason. |

Reference JSON — verified answer envelope

{ "schema_version":"1.0", "turn_id":"pub_t_opaque-example", "request_id":"req_opaque-example", "turn_sequence":2, "status":"ABSTAIN", "intent_id":"I05", "text":"Plan availability varies by market. See the current pricing page for details.", "citations":[], "actions":[{"type":"OPEN_OFFICIAL_ROUTE","route_key":"pricing"}], "knowledge_release_id":"kb-release-approved-001", "policy_version":"pub-policy-1.0"}

When evidence or claim metadata is absent, the platform intentionally returns a valid ABSTAIN outcome with an approved safe action, rather than guessing. A response with status ANSWERED MUST contain sufficient approved provenance for factual content, including machine-verifiable source references.

## 11  Synchronous sequence for commercial and trust claims

Figure 2. Deterministic signed-claim gate; generation never substitutes for registry evidence.

| Step | Control / decision | Failure consequence |
|---|---|---|
| 1 Request validation | Origin, grant, schema, idempotency, rate limit, per-session lock. | Reject with structured problem or retryable 429. |
| 2 Intent classification | Doc 02 mapping and prohibited-action interception; policy first. | REFUSED_ACTION/HANDOFF if restricted. |
| 3 P1 structured claim resolution | Plan, market, term, effective date, security claim legal approval. | ABSTAIN; never quote unverified price/security status. |
| 4 P2 taxonomy and P3 KB | Retrieve within active public KB release and taxonomy registry. | CLARIFY/ABSTAIN; no fallback to unsanctioned public web. |
| 5 Model request | Only minimum verified evidence; bounded prompt, no tools. | UNAVAILABLE if provider fails. |
| 6 Deterministic validation | Schema, status/citations/claims/route, safety/revision, no unsupported numbers. | Repair once within budget or safe fallback. |
| 7 Persist and publish | Committed typed final response and redacted audit event, deduped outbox. | Do not show answer if validation failed. |

## 12  Concurrency, retries, locks and cancellation

| Case | Server contract | Expected HTTP / status |
|---|---|---|
| Same session, new turn while existing generation runs | Atomic single-active-turn lock; optional queue MUST be separately approved; simple P0 rejects. | 409 publicai.turn_conflict; Retry-After optional. |
| Same idempotency key, identical normalized request | Return previously committed response with same turn_id; no second model call. | 200, replay same envelope. |
| Same idempotency key, different request payload | Fingerprint mismatch; preserve original, never overwrite. | 422 publicai.idempotency_mismatch. |
| Client network timeout before commit | Retry same idempotency key; server can complete original once. | 200 same result, or safe transient problem. |
| Browser session expires mid-turn | Do not emit to different/expired session; discard any prohibited output. | 401/403, start new anon session. |
| Client cancels | Abort transport; server attempts provider cancellation; committed charges/telemetry recorded independently. | No implied refund or erase guarantee. |
| Knowledge release revoked mid-generation | Final validator rechecks active/revoked epoch and claim status. | ABSTAIN or 503; never emit stale claim. |

The dedupe key scope is (product_id, anonymous_session_id, client_turn_id). Key reuse across sessions is NOT a credential and MUST NOT expose earlier response content. Production TTL and secure storage are ratification items, not promises set by this document.

## 13  Transport errors, application outcomes and public UX

| HTTP | Problem type suffix | Safe UI behavior |
|---|---|---|
| 400 | invalid_request / unsupported_schema / idempotency_header_mismatch | Correct input or reload approved UI. |
| 401 | session_expired | Issue new anonymous grant; do not auto attach login. |
| 403 | origin_denied / scope_denied | Show generic unavailable; no authorization diagnostics. |
| 404 | not_found | Do not disclose others’ session/turn existence. |
| 409 | turn_conflict | Disable submit; ask visitor to complete/cancel existing turn. |
| 413 | payload_too_large | Show documented text limit; no hidden truncation. |
| 422 | validation_failed / idempotency_mismatch | Guide safe correction; never silently alter prior turn. |
| 429 | rate_limited | Honor Retry-After, backoff and safe FAQ route. |
| 502/503/504 | dependency_failed / service_unavailable / timeout | UNAVAILABLE-style message; no fabricated answer. |

Reference RFC 9457 error payload

{ "type":"https://developers.zoikosuite.com/problems/turn-conflict", "title":"A chat response is already in progress", "status":409, "detail":"Complete or cancel the current response before sending another.", "instance":"/public-ai/errors/req_opaque-example", "code":"publicai.turn_conflict", "request_id":"req_opaque-example", "retryable":true}

The type URI above is ILLUSTRATIVE and must resolve to real approved documentation before production. RFC 9457 status values describe transport errors; business outcomes such as REFUSED_ACTION or ABSTAIN are valid typed 200 envelopes after safe processing.

## 14  Optional SSE and browser compatibility contract

v1 SHOULD ship non-streaming JSON by default. If product approves progress streaming, return only enumerated status/progress phases (accepted, searching, validating, final); do not stream raw model tokens. A final event contains the identical validated answer envelope from POST /turns. This avoids citation-less or policy-unsafe partial replies.

| Transport/control | Contract / test |
|---|---|
| Feature gating | config.capabilities.streaming true only after Security/UX sign-off; kill switch independent. |
| Authentication | Use fetch-based streaming with scoped X-Public-Chat-Session header, not unauthenticated native EventSource. |
| Event shape | event: status + minimal approved JSON; event: final + fully verified answer envelope; event: error + public code. |
| Replay | Last-Event-ID only if implementation has authenticated replay; otherwise return clear terminal error. |
| Heartbeat | Comment heartbeat allowed; never transmit provider internal text, chain-of-thought or source chunks. |
| Cross-session | Turn IDs always session checked; expiry closes stream; no shared caches. |
| Accessibility | Progress announce rate limited, visible Cancel/Retry, reduced-motion aware; no focus stealing. |

## 15  Public configuration & official-route registry

| Field / format | Source & validation rule | Privacy / rejection condition |
|---|---|---|
| config_revision | Immutable published UI config revision and ETag. | Do not expose feature secrets or staff URLs. |
| assistant_display_name | Controlled brand metadata; may use Suivo only after legal clearance. | Fallback may use neutral product descriptor. |
| capabilities | streaming, file_upload=false, lead_capture=false, authenticated=false. | No hidden enabled privileged features. |
| supported_locales / market_choices | Reviewed lists; localize naming. | Selected market is visitor input, not verified entitlement. |
| public_routes | List of allowed route_key values; resolved server-side. | Reject arbitrary href or javascript: URLs. |
| privacy_notice_key | Approved public notice; versioned copy translation ID. | Consent not inferred from chat launch. |

| Route key | Expected approved first-party destination | Decision gate |
|---|---|---|
| pricing | Current /pricing equivalent resolved from signed route registry. | If missing/unpublished, hide CTA. |
| demo | Official /demo and schedule path. | No promise of scheduling completed. |
| signup_or_trial | Official market+plan eligible route. | Only if P1 trial eligibility proven. |
| support | Official /support-center or approved help entry. | No anonymous private ticket data. |
| core_modules | Official canonical public module overview. | Route remains separate from dashboard module IDs. |
| trust | Approved trust/security destination. | No extrapolated certification claims. |

## 16  Internal API inventory — not publicly addressable

| Proposed server-to-server operation | Trust / data | Mandatory behavior |
|---|---|---|
| POST /internal/public-ai/v1/claims:resolve | Orchestrator -> P1 signed claim service | Batch typed claims; require scope/product/market/effective date; return evidence or reason code. |
| POST /internal/public-ai/v1/knowledge:retrieve | Orchestrator -> released public KB | Public index only; release-pin; filters before ranking; citation IDs. |
| GET /internal/public-ai/v1/routes/{route_key} | Orchestrator -> public route registry | Signed public HTTPS destination, active and market rules. |
| GET /internal/public-ai/v1/releases/active | Orchestrator -> release-control cache | Policy/KB/claim/taxonomy revision consistency and revocation epoch. |
| POST /internal/public-ai/v1/model:generate | Orchestrator -> model gateway | Minimized approved evidence, no tools, budget and timeout. |
| POST /internal/kb/v1/sources/{source_id}/versions | Staff governance -> KB ingestion | Doc 03 action; staff identity, dual approval; no browser access. |
| POST /internal/kb/v1/releases/{release_id}:promote | Staff governance -> KB release control | Signed release and promotion gate; never model-generated. |

These logical operations may be implemented as in-process modules or private service endpoints; a separate network service is not necessary unless ownership, IAM, scale or deployment boundaries require it. Do not multiply microservices without evidence.

## 17  Typed commercial claim-resolution contract

Illustrative claims:resolve request

{ "product_id":"zoikosuite", "audience":"PUBLIC_ANONYMOUS", "claim_subject":"PLAN_ELIGIBILITY", "claim_keys":["plan:growth","trial_duration"], "market_code":"US", "currency":"USD", "billing_term":"monthly", "as_of":"2026-10-24T10:30:00Z", "required_release":"commercial-release-active"}

Illustrative claims:resolve response

{ "decision":"INSUFFICIENT_EVIDENCE", "claim_release_id":"commercial-release-active", "effective_at":"2026-10-24T00:00:00Z", "expires_at":"2026-10-24T23:59:59Z", "verified_claims":[], "missing_claim_keys":["trial_duration"], "reason_code":"CLAIM_NOT_PUBLISHED", "signature_ref":"signed-digest-reference"}

| Decision | Permitted behavior | Forbidden behavior |
|---|---|---|
| VERIFIED | May use exactly matched structured fact and its claim_id within effective market/term. | No embellishment, implied global inclusion or unapproved fee waiver. |
| INSUFFICIENT_EVIDENCE | ABSTAIN + approved pricing/demos CTA. | No stale marketing copy used as authoritative fallback. |
| CONTRADICTED | ABSTAIN; register source-conflict case for Knowledge Ops. | No cherry-picking more favorable claim. |
| REVOKED / EXPIRED | Block affected answer and invalidate caches. | No grace period on security certification claims. |

Claim verification enforces P0 policy before P1 resolution, and uses Doc 03 P2 taxonomy only for identifiers. Market, currency and billing term MUST come from approved constrained dimensions. Commercial record owners—not the chatbot—control price and entitlement publication.

## 18  Public knowledge retrieval and citation resolution

| Field / format | Source & validation rule | Privacy / rejection condition |
|---|---|---|
| required_release / audience | Pinned KB release; audience PUBLIC_ANONYMOUS enforced server-side. | Deny mixed/private/superseded chunks. |
| intent_id / taxonomy_ids | Doc 02 intent; Doc 03 stable identifiers only. | No user-invented private filters. |
| max_chunks / token budget | Server-configured cap and ranking policy. | No unrestricted document exfiltration. |
| retrieval_response | chunk_id, source_version_id, anchor, url_key, digest, rank, sensitivity flags. | Citation fields resolve only for approved published state. |
| answerability | ANSWERABLE / CLARIFY / BLOCKED / NOT_FOUND; evidence and reason. | No model override of BLOCKED decision. |
| revocation_epoch | Current signed invalidation generation. | Post-revocation response forbidden. |

Order is mandatory: signed release selection -> classification filter -> audience entitlement filter -> effective/revoked filter -> content retrieval -> ranking -> answerability -> quote-free paraphrase -> citation validation. Public passage rankings do not create policy authority.

## 19  Model gateway request/response and zero-tool policy

| Required gateway field | Expected validation | Ownership |
|---|---|---|
| policy_version + prompt_template_version | Approved prompt registry release; deny unapproved prompt ID. | AI Governance |
| intent_id, safety_state | Only allow approved public intents; intercept restricted intents before gateway. | Orchestrator |
| approved_evidence_refs | Signed KB/claim references; minimal quoted context with source boundaries. | Knowledge Ops |
| temperature / token budget / timeout | Bounded deployment profile; measured and cost controlled. | AI Platform / SRE |
| provider region and data handling | Approved subprocessor/region, logging disabled per contract. | Privacy/Security |
| tools=[] and no network browsing | Hard-coded and policy-validated, not just prompt text. | API Platform / Security |
| draft_answer structured payload | Candidate answer subject to validation, not sent straight to client. | Orchestrator |

Prompt injection in retrieved material is treated as untrusted data and never allowed to change service policy or API destinations. The model cannot issue cross-product routing commands; stable platform identifiers and route registry decide all public destinations.

## 20  Events — envelope, transport, delivery and privacy

Adopt CloudEvents 1.0.2-compatible structured JSON envelopes for proposed internal events, with AsyncAPI 3.0.0 documentation for channels/operations after a broker is selected. These standards document interoperability; they do not themselves confer authorisation to trigger business operations.

| CloudEvents field | Proposed rule | Rationale |
|---|---|---|
| specversion | “1.0” (CloudEvents v1.0.2 uses specversion 1.0). | Wire interoperability. |
| id | Producer-generated unique UUID/ULID; immutable per event. | Consumer dedupe. |
| source | Service identity URI e.g. urn:zoikosuite:publicai:orchestrator. | Trusted producer policy. |
| type | Exact versioned event name from Doc 04. | Schema routing. |
| subject | Opaque resource type/id or omitted if unnecessary. | Avoid PII/account IDs. |
| time | UTC RFC3339 timestamp. | Ordering and expiry validation. |
| datacontenttype | application/json. | Typed payloads. |
| data | Redacted business-safe event schema; no chat message, grant, prompt or financial records. | Data minimisation. |
| traceparent | Only validated trace context; sampling and privacy policy. | Observability without data exposure. |

Valid illustrative CloudEvents 1.0 envelope

{ "specversion":"1.0", "id":"b0febd3c-68a0-45ba-b7cf-f0bd7db7ff7b", "source":"urn:zoikosuite:publicai:orchestrator", "type":"chat.turn.completed.v1", "time":"2026-10-24T10:35:00Z", "datacontenttype":"application/json", "data":{   "request_id":"req_opaque-example",   "intent_id":"I05",   "status":"ABSTAIN",   "latency_bucket":"2s-5s",   "release_id":"kb-release-approved-001" }}

Delivery semantics: use a transactional outbox where event persistence and authoritative state change share a transaction; consumers are idempotent and expect at-least-once delivery, duplicates and out-of-order delivery. Ordering is guaranteed only where specifically documented by broker partition key.

## 21  Canonical event catalog, producers, consumers and guards

| Event / producer | Consumer(s) / schema-critical fields | Forbidden effect / handling |
|---|---|---|
| chat.turn.completed.v1 / orchestrator | Analytics and SRE: request_id, intent_id, status, latency_bucket, release_id. | Never triggers new chat or CRM write; no raw text. |
| chat.handoff.clicked.v1 / browser-edge | Privacy-safe conversion analytics: route_key, market_code_if_explicit, consent_flag. | Click not a booking; do not attach visitor email. |
| kb.release.promoted.v1 / governance | Read plane + cache: release_id, signer_refs, digest, effective_at. | Reject unsigned/unauthorised producer. |
| kb.source.revoked.v1 / governance | Read plane + edge: source_version_id, reason_code, effective_at. | Immediate deny before asynchronous purge completes. |
| publicai.kill_switch.changed.v1 / ops control | Edge/runtime: flag, actor_ref, approval_ref, environment. | Only authorized control plane; fail closed. |
| chat.security.signal.v1 / SecOps proposed P1 | SRE/Security: reason class, request_id, coarse severity. | Do not record malicious prompt body. |

Event schema IDs are example targets. A service owner MUST publish an AsyncAPI contract and JSON Schema version and retain producer/consumer compatibility reports before connecting a real broker.

## 22  Event topics, dead-letter queues and replay controls

| Design element | Proposed implementation contract |
|---|---|
| Topic segregation | Separate publicai.telemetry.v1, publicai.control.v1, and kb.governance.v1; no mixed broker namespace with financial domains by default. |
| Producer identity | Workload identity and authorization per topic; browser never publishes internal event directly. |
| Delivery guarantee | At least once; event_id-based dedupe with bounded retention; no exactly-once assumptions. |
| Ordering | Control events use monotonically increasing signed release/epoch numbers; clients reject older state. |
| Failure recovery | Exponential backoff + jitter; circuit breaker; dead-letter after configured attempts. |
| DLQ handling | Privileged operator review with payload redaction, replay approval, expiry and audit trail. |
| Replay | Replay must not re-enable revoked release or resurrect prior plan claims; validate current effective/revocation epoch. |
| Retention | Event TTL per DPO-approved matrix; telemetry aggregated rather than raw visitor content. |

## 23  Webhooks and third-party integration boundaries

Production v1 does NOT require arbitrary external inbound webhooks. Approved first-party CMS / commercial / trust systems may emit events through staff-controlled authenticated connectors only after source-provenance and service-ownership validation. Any future CRM, scheduler, support ticket vendor or third-party webhook integration is a separate controlled change with DPIA/security review.

| Future integration | Minimum prerequisites | Default until approved |
|---|---|---|
| CRM lead capture | Consent-purpose specification, validated customer-entry form, anti-abuse, DPA, explicit API method and owner. | CTA route only; no contact data in chatbot. |
| Calendar/demo scheduler | Slot authority, market/timezone, token protection, booking confirmation and PII agreements. | Open official demo route; do not claim booking. |
| Support ticketing | Consent, case creation authentication, data-retention and staff escalation rules. | Official support route; no private case status. |
| CMS publish webhook | Signed producer, replay timestamp/nonce, source version, content security scan. | Scheduled/polled approved ingestion fallback. |
| Price/market publication | Commercial owner signed release, effective dates and rollback procedures. | ABSTAIN when registry not verified. |

## 24  Privacy, IAM, audit and regulatory checkpoints

| Control | Enforcement & evidence |
|---|---|
| Data minimisation | Never log chat text, financial identifiers, access tokens, source excerpt payloads or model chain-of-thought by default. |
| PII interception | Detect obvious sensitive input, show interruption and redirect to secure first-party flow; no structured extraction of bank/tax/payroll details. |
| Anonymous identity | Session grant is NOT customer authentication; no customer account lookup, SSO proxy or token exchange. |
| Internal IAM | mTLS/workload identity as appropriate; least privilege per service, per topic and per endpoint; deny-by-default network controls. |
| Signed releases | Immutable digest/signer and revocation_epoch; differentiate product approvals from launch and trademark clearance. |
| Region & subprocessors | Record data flow, retention, provider DPA, hosting cell, cross-border transfer decision, deletion mechanism and DPIA determination. |
| Audit | Operator control changes and P1 source/release promotions logged with actor, reason, approval reference and trace, not chat content. |
| Abuse | Per-origin, per-IP coarse signals, per-session and spend-quota limits; mitigate bot flooding without building a personal profile. |

## 25  Observability, SLO and fault injection contract

| Indicator | Required measurement / action |
|---|---|
| Turn outcome | Answered, clarify, abstain, refused, handoff, unavailable counts and percentages by release and market (sufficient aggregation). |
| Latency | Edge validation, claim resolution, KB read, model generation, response validation, end-to-end distribution. |
| Registry freshness | Active KB/claim/taxonomy revisions, effective windows, revocation propagation and stale-read rejection counts. |
| Cost & quota | Provider input/output token counts and spend by service/version; no billing/user linkage in anonymous telemetry. |
| Reliability | API availability, timeouts, 429, circuit breaker openings, DLQ depth, event lag, failed webhook signatures. |
| Safety | Disallowed call attempts, tool access denials, suspicious payload classes, blocked claim assertions. |
| Release controls | Canary comparison; independent kill-switch test; policy rollback; model provider outage simulation. |

Proposed objectives, not service guarantees: contract schema 100% compliant in QA; unauthorized customer-data calls 0 permitted; unsupported high-impact answers 0 in golden tests; event consumers idempotent under duplicate replay; exact latency and budget thresholds must be ratified from benchmark results and actual traffic forecasts.

## 26  End-to-end release sequence and migration approach

| Phase / gate | Engineering action | Required exit evidence |
|---|---|---|
| P0 inventory | Map actual API gateway, ZoikoSuite web routes, infrastructure, repo components and named service owners. | As-built service inventory signed. |
| P1 contracts | Publish OpenAPI 3.1-compatible public contract, JSON Schemas and AsyncAPI event definitions; verify generator support. | Schema lint + consumer/provider tests. |
| P2 stubs | Mock public config, anon sessions, claims:resolve and KB read via deterministic fixtures. | Frontend/AI independent development passes. |
| P3 enforcement | Network IAM denies all private ZoikoSuite services; release validators; signed claims and routes. | Pen test, bad-path tests and logs. |
| P4 end-to-end QA | Golden G01–G36, KB-001–030, A01–A40 and current Doc 05 suites; simulate outages. | Repeatable CI evidence and approval. |
| P5 controlled beta | Internal/approved pilot with kill switch, sampled telemetry and on-call runbook. | No severe defects; cost/load budget approval. |
| P6 release | Phased enablement of first-party widget; rollback rehearsal and post-launch monitoring. | Founder/Product, CTO, Sec, DPO, QA, SRE sign-off. |

## 27  Contract tests — public surface (CT-001 to CT-024)

| Test | Stimulus | Mandatory observable outcome |
|---|---|---|
| CT-001 | Valid session create | 201, opaque subject and safe public capabilities only. |
| CT-002 | Missing/invalid session grant | 401 or 403; no information disclosure. |
| CT-003 | Cross-session turn submission | Deny mismatched session_id and grant. |
| CT-004 | Unapproved Origin header | 403; no CORS grant; server rejects regardless of browser. |
| CT-005 | Content-Type invalid | 415 or deterministic invalid-request problem per approved spec. |
| CT-006 | Unknown schema_version | 400 unsupported_schema; client fallback. |
| CT-007 | Message > configured size | 413; no silent truncation or model call. |
| CT-008 | Malformed market_code | 400 or CLARIFY per published schema, never infer coverage. |
| CT-009 | Unknown page_context | Reject or ignore based on versioned validation; never DOM fetch. |
| CT-010 | Valid duplicate Idempotency-Key | Same turn_id and final; one model charge. |
| CT-011 | Same key different payload | 422 mismatch; no overwrite. |
| CT-012 | Different key concurrent same session | 409; no competing generation. |

| Test | Stimulus | Mandatory observable outcome |
|---|---|---|
| CT-013 | Session expired | 401; no old result leaks. |
| CT-014 | Final arrives out of order | Client ignores old sequence; server keeps canonical order. |
| CT-015 | Typed ANSWERED envelope | Citations/claims validate and render from approved resolver. |
| CT-016 | Invalid status enum | Gateway rejects; user sees safe unavailable. |
| CT-017 | Overlength model output | Server rejects/repairs once; never raw render. |
| CT-018 | Unsafe action URL injected | No arbitrary URL; route_key allowlist enforced. |
| CT-019 | SSE disabled | Endpoint refuses/404/feature-disabled deterministically. |
| CT-020 | Stream grant cross-session | No access; no provider tokens emitted. |
| CT-021 | DELETE own session twice | 204 and idempotent repeat; new operations blocked. |
| CT-022 | DELETE other session | 403/404 generic; no presence disclosure. |
| CT-023 | Config ETag / feature gate | No secrets; safe cache; correct 304 behavior. |
| CT-024 | Health endpoint in outage | Generic response; no internal vendor details. |

## 28  Integration, event and safety tests (CT-025 to CT-052)

| Test | Stimulus | Mandatory observable outcome |
|---|---|---|
| CT-025 | P1 price missing | ABSTAIN; no precise price or unverified plan terms. |
| CT-026 | P1 price contradicted | Claim block and governance case. |
| CT-027 | Trust attestation expired | No certified claim; safe trust route. |
| CT-028 | Market unavailable | Clarify/abstain; no global-rollout assertions. |
| CT-029 | Trial plan gate false | Trial CTA hidden, explanation truthful. |
| CT-030 | KB chunk is private | Filter before retrieval; zero surfaced text. |
| CT-031 | KB release revoked during generation | Final validator blocks; cache invalidates. |
| CT-032 | Citation anchor malformed | Reject citation or answer; no forged link. |
| CT-033 | Model emits tool instruction | No tool invocation occurs; record safe signal. |
| CT-034 | Prompt injection in approved document | Ignored as authority; safe answer/no action. |
| CT-035 | User requests bank records | REFUSED_ACTION; no business API call. |
| CT-036 | User enters payroll ID | Sensitive prompt interception; no telemetry plaintext. |
| CT-037 | Commercial service timeout | ABSTAIN or UNAVAILABLE, never P3 fallback. |
| CT-038 | Model gateway timeout | Safe UNAVAILABLE and predictable client retry. |

| Test | Stimulus | Mandatory observable outcome |
|---|---|---|
| CT-039 | Route resolution fails | Hide CTA; never arbitrary redirect. |
| CT-040 | Event delivery duplicate | Consumer effect once, event_id dedupe. |
| CT-041 | Events out of order | Older release/epoch not applied. |
| CT-042 | Event DLQ replay | Authorization and freshness rechecked. |
| CT-043 | Unsigned KB release event | Reject, alert, no publish. |
| CT-044 | Revocation + cache stale | Deny claims synchronously; target propagation logged. |
| CT-045 | Event contains raw chat text | Schema rejection; DLP alert. |
| CT-046 | Browser posts internal event | Blocked at gateway and broker. |
| CT-047 | Broker outage during completed turn | Outbox retains event; answer state consistent. |
| CT-048 | Kill switch enabled mid-flight | New turns blocked, active turn final withheld if required. |
| CT-049 | Telemetry PII leak probe | No messages/grants/tokens in traces, logs, event bus. |
| CT-050 | Cross-product query | Product boundary honored; approved route only. |
| CT-051 | First-party CMS webhook replay | Signature/timestamp/nonce check denies duplicate. |
| CT-052 | Accessibility on retry/handoff | Keyboard, screen-reader and focus state verified. |

## 29  Engineering repo handoff, owners, evidence and open decisions

| Required artifact | Owner | Exit evidence / file example |
|---|---|---|
| As-built service/register map | CTO / API Platform | service-map.md, environment inventory, bounded-context owner approval. |
| Public OpenAPI specification | Backend / Frontend | openapi/public-ai-v1.yaml; schema lint & consumer/provider diffs. |
| Internal typed interface definitions | AI Platform / KB Ops | schemas/claims, knowledge, route and generation revisions. |
| AsyncAPI + CloudEvent schemas | Platform Events / Data Governance | asyncapi/publicai-events.yaml + broker ACL rules + tests. |
| P1 signed claim contracts | Commercial / CISO / Legal | current plan/market/trust registries and allowed claim types. |
| P2 taxonomy and route crosswalk | Product Architecture / Web | signed service/category v2.1 ID+route reconciliation. |
| Provider security / DPIA | Security / Privacy | approved region, DPA, retention, network egress proof. |
| CI contract & security tests | QA / SRE | CT-001–CT-052, prior suites, threat proof, SLO budget. |
| Incident and rollout pack | SRE / Release | dashboards, revocation drill, kill switch and rollback steps. |

| Open decision / release blocker | Required resolution | Owner and effect |
|---|---|---|
| D05-01 / P0: production repo/API topology | Confirm existing gateway, frameworks, path prefix and release pipeline. | CTO; no production service binding until approved. |
| D05-02 / P0: public official route registry | Route keys, host allowlist and live demo/support/signup URLs. | Web/Product; no unsafe CTA links. |
| D05-03 / P0: signed price/entitlement/market releases | Machine-readable P1 schema, legal claim templates, effective dates. | Commercial/CFO/Product; no exact pricing claims. |
| D05-04 / P0: trust and certification approvals | Permissible words and evidence under valid window. | CISO/Legal; no certification representations. |
| D05-05 / P0: KB source/taxonomy reconciliation | Authoritative service/category v2.1 + public KB release and signers. | Knowledge/Product; no general claim generation. |
| D05-06 / P0: IAM & region policies | Approved session grants, storage and vendor egress region. | Security/DPO; block deployment. |
| D05-07 / P1: SSE yes/no | UI benefit, cost and accessibility readiness. | Product/UX/SRE; default JSON-only. |
| D05-08 / P1: event broker selection | Existing platform broker, outbox pattern, topic ACL, DLQ. | Platform/SRE; no broker assumption. |
| D05-09 / P1: OpenAPI toolchain revision | Support 3.1.2 vs 3.2.1; ensure no Doc04 contract break. | API Platform; control version bump. |
| D05-10 / P1: session TTL and dedupe window | Measured abuse/privacy/cost tradeoffs. | Security/SRE/Privacy; no invented guarantees. |
| D05-11 / P1: retention and deletion | Data classification and legal requirements by operating region. | DPO/Legal; no false deletion claims. |
| D05-12 / P1: name/brand and asset permission | Current approved logo and Suivo clearance/gating. | Brand/Legal; neutral fallback if needed. |

## 30  Architectural critique, approvals and stopping rule

Critical review disposition: the reference design is internally consistent with prior documents when (a) P0/P1 facts are a hard precondition for high-impact claims; (b) session and request IDs are not identity credentials; (c) public chat receives no private-service connectivity; (d) the model has no tools; (e) final responses are server-validated before display; (f) revocation is checked at reply time; and (g) events are never an authorization channel. These properties require executable enforcement and independent review, not descriptive documentation alone.

| Sign-off gate | Approver | Evidence/status |
|---|---|---|
| G0 Scope and approved assistant identity | Founder / Product / Brand | PENDING; Doc 05 alone does not authorize deployment. |
| G1 Canonical commercial/taxonomy sources | Commercial / Product Architecture / Knowledge Ops | PENDING P1/P2 signed registries. |
| G2 API contracts and as-built architecture | Global CTO / API Platform / AI Lead | PENDING repo map, specs, compiled integration. |
| G3 Security, privacy, legal | CISO / DPO / Counsel | PENDING threat model, PIA, third-party and legal use. |
| G4 QA + event integrity | QA / Data Platform / SRE | PENDING CT-001–CT-052 plus prior suite evidence. |
| G5 Public go-live release | Release Manager / Founder | PENDING signoff, canary, rollback, operator drill. |

| CONTROLLED APPROVAL BOUNDARY Document 05 is for review and approval as an individual deliverable. Do not progress to Document 06 (Security, Privacy, Abuse Prevention & Trust Controls) until the Founder expressly approves this document. Proposed APIs/events require separate implementation and security sign-offs. |
|---|

## Appendix A  OpenAPI contract fragment for CI scaffolding

The following is an illustrative, parsable OpenAPI 3.1 baseline excerpt. Full schemas, constraints, auth handler behaviour and failure bodies MUST be expanded in the repository before release. Do not silently advance the major version to OpenAPI 3.2.x without contract-tooling compatibility evidence.

OpenAPI 3.1-compatible minimum scaffold

openapi: 3.1.0info:  title: ZoikoSuite Public AI API - proposed  version: 1.0.0paths:  /public-ai/v1/turns:    post:      operationId: submitPublicTurn      security:        - publicChatSession: []      parameters:        - in: header          name: Idempotency-Key          required: true          schema: {type: string, format: uuid}      requestBody:        required: true        content:          application/json:            schema: {$ref: '#/components/schemas/TurnRequest'}      responses:        '200': {description: Validated typed final response}        '409': {description: Concurrent session turn}        '422': {description: Idempotency fingerprint mismatch}        '429': {description: Rate limit}components:  securitySchemes:    publicChatSession:      type: apiKey      in: header      name: X-Public-Chat-Session  schemas:    TurnRequest:      type: object      additionalProperties: false      required: [schema_version,session_id,client_turn_id,message]      properties:        schema_version: {type: string, const: '1.0'}        session_id: {type: string, minLength: 12}        client_turn_id: {type: string, format: uuid}        message: {type: string, minLength: 1, maxLength: 2000}        locale: {type: string}        market_code: {type: string, pattern: '^[A-Z]{2}$'}        page_context: {type: string}        requested_response_mode: {type: string, enum: [json]}

## Appendix B  Event schema and example consumer rule

JSON Schema — chat.turn.completed.v1 data object (illustrative)

{ "$schema":"https://json-schema.org/draft/2020-12/schema", "type":"object", "additionalProperties":false, "required":["request_id","intent_id","status","latency_bucket","release_id"], "properties":{  "request_id":{"type":"string","maxLength":128},  "intent_id":{"type":"string","maxLength":32},  "status":{"enum":["ANSWERED","CLARIFY","REFUSED_ACTION","ABSTAIN","HANDOFF","UNAVAILABLE"]},  "latency_bucket":{"enum":["<2s","2s-5s","5s-15s",">15s"]},  "release_id":{"type":"string","maxLength":128} }}

Consumer rule (normative target): verify authenticated producer + schema_version + event_id uniqueness + freshness/epoch where applicable. On duplicate, acknowledge without side effect. On expired control events, quarantine and alert. Never deduce customer identity from telemetry fields.

## Appendix C  Source and standards references (checked 24 October 2026)

| Ref | Document / URL | Use and interpretive limit |
|---|---|---|
| INT-01 | ZoikoSuite Documents 01–04, 24 Oct 2026 | Controlled prior work; proposes design, does not verify deployed APIs. |
| INT-02 | https://spec.openapis.org/oas/v3.2.1.html | OpenAPI latest published spec v3.2.1 (10 Sep 2026); validate toolchain adoption. |
| INT-03 | https://spec.openapis.org/oas/v3.1.html | OpenAPI 3.1.2 known-compatible family for current Doc04 scaffold. |
| INT-04 | https://www.rfc-editor.org/rfc/rfc9457 | Problem Details for HTTP APIs (status and machine-readable codes). |
| INT-05 | https://github.com/cloudevents/spec/blob/main/README.md | CloudEvents core v1.0.2 release reference. |
| INT-06 | https://www.asyncapi.com/docs/reference/specification/v3.0.0 | AsyncAPI event-driven interface specification. |
| INT-07 | https://api-security.owasp.org/editions/2023/en/0x11-t10/ | OWASP API Security Top 10; authentication/authorization, SSRF and abuse. |
| INT-08 | https://datatracker.ietf.org/doc/html/draft-ietf-httpapi-idempotency-key-header | Idempotency-Key draft; NOT an RFC. Final contract policy explicitly local. |
| INT-09 | https://www.w3.org/TR/trace-context/ | W3C Trace Context for correlation; sanitize inbound traces. |
| INT-10 | https://www.w3.org/TR/WCAG22/ | WCAG 2.2 target for chat widget integration. |

## Appendix D  Implementation glossary and change control

| Term | Definition and non-implication |
|---|---|
| PUBLIC_ANONYMOUS | Only v1 capability scope; no access to business tenants or user accounts. |
| P0 / P1 / P2 / P3 / P4 | Knowledge authority tiers defined in Doc 03; rank is not data-access authorization. |
| Claim gate | Server-side signed structured-fact verification; model text never substitutes. |
| Typed envelope | Server-validated response object with status, citations/actions, revisions and IDs. |
| Idempotency | Duplicate retry equivalence for defined session/request fingerprint; not globally exactly-once delivery. |
| CloudEvent | Interoperable event envelope; does not imply trusted publisher. |
| Revocation epoch | Signed monotonic control value used to reject stale knowledge/config. |
| Internal service | Private/protected integration boundary; not an externally advertised public API. |
| Owner sign-off | Named approval evidence for scope, security, backend implementation and release. |

Version 1.0 / 24 October 2026 — first controlled Document 05. Every material revision must record reason, impacted Docs 01–04, contract breaking/nonbreaking decision, migrations, CI results, approvers and effective time. No unreviewed field, event, owner or privilege change is authorized by this specification.
