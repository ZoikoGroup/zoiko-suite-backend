<!-- Extracted from ZoikoSuite_Suivo_Document_10.docx -->

ZOIKO GROUP  /  ZOIKOTECH INC.  /  ZOIKOSUITE

SUIVO  /  PUBLIC AI ASSISTANT

Commercial Conversion, CRM Integration& Lead Management Specification

ENGINEERING DOCUMENT 10  |  VERSION 1.0  |  24 OCTOBER 2026

The controlled blueprint for converting public ZoikoSuite enquiries into verified, consent-aware commercial outcomes: customer segmentation, plan guidance, official demo and trial routes, secure lead intake, CRM adapter contracts, booking lifecycle, qualification, deduplication, privacy, analytics, operational exceptions and testable launch gates.

| DOCUMENT STATUS — PROPOSED IMPLEMENTATION BASELINE FOR FOUNDER / GLOBAL CTO / PRODUCT / GLOBAL COMMERCIAL / SALES OPERATIONS / LEGAL-DPO / SECURITY / BACKEND / QA APPROVAL. This document is a target engineering specification. It is NOT proof that ZoikoSuite has a particular CRM, calendar vendor, lead workflow, billing system, live webhook, response-time SLA, country coverage or approved conversion tracking deployment. |
|---|

| Controlled field | Decision or boundary |
|---|---|
| Document ID | ZS-SUIVO-ENG-010 / v1.0 / Internal / controlled |
| Series lineage | Documents 01–09 are preceding architectural and QA baselines. This document adds new CRM/scheduling contracts ONLY as a separately gated extension. |
| Assistant identity | Internal working display name Suivo; stable public assistant ID zoikosuite-public-ai. External name usage remains subject to brand/IP sign-off. |
| Runtime boundary | Anonymous public chatbot -> official first-party destinations. No embedded CRM credentials, payment capture, private ledgers or customer-support case authority. |
| Primary audiences | Business owner; accountant/bookkeeper; agent; payroll bureau; enterprise finance; authorised partner, consistent with the Demo Page stakeholder model. |
| Commercial state | Public current snapshot: Starter, Growth, Enterprise; eligible Starter/Growth trial messaging; all claims must be resolved from signed market-specific records at runtime. |
| US/UK language | American English for global ZoikoSuite public site; localise UK statutory/privacy communications where required. No implied country-specific product coverage. |

| MANDATORY SEPARATION Marketing consent is NOT required merely because a visitor asks for a demo (the secure intake still needs a lawful basis and a privacy notice). A demo enquiry is not a booking; a booking is not a paid subscription; a trial referral is not an account or payment authorisation. |
|---|

## 01  Executive decision and critique-driven design corrections

Approve a progressive commercial pathway with explicit confirmations at every trust boundary: INTENT_DISCOVERED -> ROUTE_OFFERED -> SECURE_FORM_SUBMITTED -> LEAD_ACCEPTED -> BOOKING_CONFIRMED (optional) -> CRM_RECONCILED (eventual) -> HUMAN_QUALIFIED. The public assistant never writes directly to customer or CRM objects. An approved secure first-party intake may perform a server-to-server CRM write after separate security and legal gates.

| Common failure | Refined, testable guardrail |
|---|---|
| Lead auto-created when user says “book a demo” | Only secure intake submission creates a lead; chatbot action is an official route/link unless an approved signed transfer is used. |
| Calendar slot appears as confirmed before provider ACK | Show “booking confirmed” only with provider event ID and durable status from verified integration. |
| Demo submission doubles as newsletter opt-in | Purpose-bound form processing and separate unchecked marketing choice; lawful-basis review per region and contact type. |
| Latest price copied from chatbot prompt | Claim registry/approved price book resolves plan, currency, trial, effective window, market and disclaimers deterministically. |
| Duplicate submission spawns multiple leads | Per-submission idempotency + normalized identity/dedupe matching + reversible CRM merge review. |
| Existing customer gets sent to sales | Detect support/login intent; route to secure support; never use CRM follow-up for protected account inquiries. |
| Booking provider webhook trusted blindly | Verify signature, timestamp, event replay, resource correlation and vendor-issued identity. |
| Conversion tracked with cookies by default | Use privacy-safe server counters; load non-essential tracking only after valid visitor preference and legal approval. |
| Trial auto-converts to paid from AI CTA | Paid activation is an independent authenticated billing agreement with terms and affirmative acceptance. |

## 02  Scope, boundaries and precedence

| In scope — this document | Out of scope / separately approved |
|---|---|
| Visitor qualification prompts; product fit; official CTA route resolver; secure lead form; CRM adapter; optional scheduler; state receipts; CRM stages; sales queues; opt-out/suppression; analytics; incidents. | Chatbot payments, account signup in chat, tenant account linking, autonomous scheduling, automated bank/tax/payroll transactions, in-product financial assistant, arbitrary CRM tools, scraping visitor identity. |
| Propose typed interfaces and reference flows; verify against actual first-party codebase, CRM and scheduler before implement. | No vendor or webhook is declared currently deployed; do not create production credentials from examples. |

Precedence: (a) security, privacy and consent controls in Document 06; (b) service boundary and API/event contracts in Documents 04–05; (c) knowledge/claim authority in Document 03; (d) staff handoff and case semantics in Document 09; (e) QA and promotion controls in Documents 07–08; (f) this document for commercial-only states. Where proposed flows conflict, fail closed and record a new ADR.

## 03  Observed public ZoikoSuite journeys and live-evidence caveats

| Public source | Observed as of 24 October 2026 | Engineering treatment |
|---|---|---|
| /pricing | Starter, Growth and Enterprise shown; Starter/Growth trial messaging; annual only when market rate approved. | Link to signed commercial claim authority; NEVER hard-code the displayed snapshot. |
| /book-demo | Multi-step demo flow: You & business -> Priorities -> Schedule; optional phone; no signup/payment. | Prefer active first-party secure route once Product approves canonical mapping. |
| /core-modules | Four domains, ten canonical modules, shared governance and platform foundations. | Use stable taxonomy from Doc03, not free-form sidebar categories. |
| /trust-center | Specific statements about certifications, service promises and assurance status. | Keep trust claims evidence-gated; no fake badge or uptime promise. |

| ROUTE CONFLICT TO RECONCILE Earlier Demo Page specification referenced /demo, while the public site also exposes /book-demo. Engineering must verify redirects, SEO/canonical tags, route ownership, analytics events, and official locale mapping. The chatbot must use the signed route registry; do not presume one link is canonical until Product validates it. |
|---|

## 04  Commercial service architecture and trust zones

Figure 1 — Proposed conversion interfaces and denied trust-boundary crossings.

| Domain owner | System of truth and permission |
|---|---|
| Suivo public runtime | Anonymous intent, conversation UI and route recommendations; read-only signed public claims; no contact-field collection by default. |
| Route and commercial claim registry | Official CTA keys and current plan/trial facts, approved URL redirects, market eligibility and route disabled state. |
| Secure lead intake service | Visitor-provided contact details, notices, lawful-basis/marketing preferences, abuse controls, request receipt and durable outbox. |
| CRM integration adapter | Mapped public intake data only; service-authenticated scoped upsert; retries, dedupe links and vendor response identifiers. |
| Scheduling provider adapter | Verified availability on destination, create/cancel/reschedule events, signed webhook validation, reconcile provider receipt. |
| Sales operations console | Ownership, qualification, outreach suppression, audit and resolution in authenticated staff channel (Doc09 patterns). |

## 05  Stakeholders, buyer personas and preferred journeys

| Segment | Typical needs / fit signal | Recommended next step |
|---|---|---|
| Small business | 1 legal entity; core accounting, AR/AP, reconciliation; initial deployment. | Starter/Growth comparison if verified; eligible trial route when allowed. |
| Growing finance team | Several entities, approvals, budgets, automation, integrations. | Growth fit guidance; verified trial or qualified specialist demo. |
| Accountant / bookkeeper | Client separation, permissions, evidence, reporting, multi-client workflows. | Tailored demo and product specialist handoff. |
| Agent / payroll bureau | Delegated permission and multi-client operations, market-dependent payroll. | Specialist demo; never promise market or filings without claim gate. |
| Enterprise finance | Consolidation, intercompany, SSO/SCIM, complex migration and audit. | Enterprise demo route + trust diligence path. |
| Partner / integrator | API contract, integrations, partner or referral relationships. | Solutions/partnership enquiry with approved classification. |

## 06  Commercial intent catalog, priority and eligibility

| Intent ID / priority | Detection / required evidence | Permitted outcome |
|---|---|---|
| COM-01 / P0 / pricing | Explicit user asks pricing; market and currency only if provided; claim release valid. | Grounded plan comparison and pricing route; abstain when market unavailable. |
| COM-02 / P0 / trial | Visitor interested in trial; plan and market eligibility checked. | Official Starter/Growth trial route only if enabled. |
| COM-03 / P0 / enterprise demo | Enterprise team or advanced governance needs; verified route. | Open approved secure demo destination. |
| COM-04 / P0 / contact sales | Wants sales follow-up; signed route key. | Offer secure contact form; no silent CRM write. |
| COM-05 / P0 / reschedule | Visitor has existing booking and asks to change. | Direct to secure provider-managed reschedule route; no identity in public chat. |
| COM-06 / P0 / support | Already customer / billing / login / account problem. | Support route, not sales lead; no protected account lookup. |
| COM-07 / P1 / integrations | Named connector or migration inquiry. | Evidence-qualified answer; specialist demo if unclear. |
| COM-08 / P1 / trust | Security docs, regulatory or SLA requests. | Trust center / diligence channel; no false certifications. |
| COM-09 / P1 / comparison | Competitor/plan fit question. | Source-grounded differences; no unverifiable competitor claims. |
| COM-10 / P1 / partner | Integration/referral/agency partner interest. | Partnership route with appropriate contact-purpose category. |

## 07  Visitor experience — conversion decision ladder

The public chatbot offers at most ONE primary CTA and ONE optional secondary CTA per response. A visitor may ask questions without providing a name, email or phone. Do not trigger unsolicited forms or repeated lead prompts. Context preserved across turns must stay bounded and must not become a prospect profile without lawful intake.

| Visitor state | UI treatment | Success criteria |
|---|---|---|
| DISCOVERY | Concise answer with relevant approved public citation and optional capability link. | Question answered; no conversion pressure. |
| CONSIDERATION | Compare verified plans; clarify entity count or required governance; no invented feature availability. | Visitor understands fit and limits. |
| INTENT_HIGH | One qualified “Book a tailored demo” or “Explore 30-day trial” CTA. | A valid official destination is offered. |
| ROUTE_SELECTED | Explain leaving chatbot, never imply lead/ticket/booking created. | Click opens valid secure first-party route. |
| SUBMITTED / ACCEPTED | Only secure destination displays validation outcome and authoritative receipt. | Persistent intake reference; no duplicate. |
| SCHEDULED | Provider-confirmed receipt with timezone and reschedule policy. | Actual booking event persisted. |

## 08  Approved customer-facing copy and explicit prohibition

| Situation | Approved sample language / contract |
|---|---|
| Enquiry route only | “I can help you get to the right team. Open our secure demo form to send your request.” |
| Submitted lead (secure page) | “Thanks — your request was received. Reference: {submission_reference}. We will use your details for this enquiry as described in our privacy notice.” |
| Booking confirmed | “Your demo is confirmed for {local_time} ({iana_zone}). Confirmation: {booking_reference}. Manage it using the link in your booking confirmation.” |
| Trial eligible | “Starter and Growth may offer a 30-day trial where enabled. Review the current terms and activate it on the official signup page.” |
| Plan no evidence | “I can explain the plan structure, but I can’t verify the price or availability for your market right now. The pricing page has the latest published details.” |
| CRM outage | “Your request was received” ONLY after local durable intake receipt. Never promise routing to sales while delivery is unconfirmed. |
| Prohibited phrases | “I booked you”, “I opened your account”, “You are automatically enrolled”, “We guarantee SOC 2”, “An agent will call today”, absent verified evidence. |

## 09  Secure demo page field mapping and UX constraints

| Field | Required / source | Handling |
|---|---|---|
| first_name, last_name | Required on secure /book-demo form as presently shown. | Trim, Unicode normalize, length bounds; no chat collection. |
| work_email, organisation | Required per approved form flow. | Validate reasonable syntax; preserve original; avoid domain-existence proof as hard blocker. |
| role, country, company_size | Required/optional as form config states. | Controlled enumerations, “other / prefer not to say” where business permits. |
| phone | Optional on current public demo. | Never demand it; valid E.164 when provided. |
| priorities, entities, integrations | Second step; only minimum necessary for useful session. | No financial statements, bank numbers, tax IDs, payroll records or credentials. |
| timezone, appointment | Schedule step, provider is authoritative. | IANA TZ; DST-safe instant + offset; schedule only after confirmation. |
| privacy notice, marketing choice | Purpose notice mandatory; separate optional choice if offered. | Version, timestamp, language; no prechecked marketing option. |

## 10  Lead submission and booking state machines

Figure 2 — Conversion receipts are distinct and independently auditable.

| Entity / state | Valid transitions / notes |
|---|---|
| Intent: DISCOVERED -> CTA_OFFERED -> CTA_CLICKED | Analytics only; do not store email or create lead. |
| Intake: DRAFT -> VALIDATED -> ACCEPTED -> CRM_PENDING -> CRM_SYNCED | ACCEPTED only after durable first-party write; 422 rejects incomplete data; CRM_PENDING is honest retryable state. |
| Intake: VALIDATED -> REJECTED_ABUSE / REJECTED_VALIDATION | No lead; user gets repair path or rate-control notice, not false success. |
| Booking: STARTED -> HOLD_PENDING -> CONFIRMED / EXPIRED / FAILED | Only provider ACK with event ID confirms; slot display not guarantee. |
| Booking: CONFIRMED -> RESCHEDULED / CANCELLED | Prior event retained in immutable history; same lead association; no phantom new booking. |
| CRM: UPSERT_PENDING -> SYNCED / RETRYING / DEAD_LETTER / RECONCILED | Retry via idempotent external reference; alert owner if stuck; preserve durable intake. |

## 11  Reference data model and entity identity

| Object | Minimum fields and identity rules |
|---|---|
| LeadSubmission | submission_id UUID; public_reference; accepted_at UTC; source=suivo_or_web; purpose_id; form_version; locale/market; request_hash; pii_encrypted; dedupe_key; data_owner; retention_policy_id. |
| ConsentEvidence | evidence_id; notice_version; form_version; choice_status for marketing only; lawful_basis assessment reference; collected_at; channel; withdrawal_at; proven source; no inferred acceptance. |
| LeadJourney | journey_id; immutable submission_id; stage; server-side segment / routing reason; owner; stage change event and actor; TTL/retention policy. |
| BookingBinding | booking_binding_id; submission_id; provider_event_id; status; starts_at_utc; timezone; provider_ref; webhook_verification_ref; cancel/reschedule event IDs. |
| CrmBinding | submission_id; provider_name; external_contact_id/lead_id; sync_status; outbox_event_id; retry_count; last_ack_at; last_error_code. |
| Attribution | UTM source/medium/campaign only if lawful and available; first_party_utm_source; consent_bucket; no device-fingerprint key; not part of lead identity. |

## 12  Dedupe, organisation reconciliation and identity safety

Idempotency and entity resolution are different operations. Idempotency prevents replay of the same secure submission (submission_id/key scoped to first-party intake, TTL and request hash). Dedupe compares potentially related contacts/companies under a separately controlled rule set. Do not treat an email domain as proof of employer identity; do not automatically overwrite a customer record or merge multiple people.

| Condition | Deterministic outcome |
|---|---|
| Same idempotency key + same payload | Return original receipt/reference (200/201 per contract), not a duplicate lead or booking. |
| Same key + materially different payload | 409 IDEMPOTENCY_CONFLICT; do not replace accepted record. |
| Same email, different form months later | New submission activity linked to existing permissible contact; original consents remain purpose- and time-bound. |
| Same organisation / shared email | Do not force merge; flag CRM duplicate review queue with minimal metadata. |
| Lead matches existing customer | Route to account team where lawful; never surface CRM match or private status in public chat. |
| Person requests deletion or objects | Follow DPO workflow; suppress marketing and propagate lawful deletion/retention policy to adapters. |

## 13  CRM selection and integration pattern

Vendor-neutral decision: CRM is a replaceable downstream system, not the public AI authority. The first release can operate in ROUTE_ONLY mode with a secure form and manual sales queue. Enable CRM_SYNC mode only after chosen vendor, API contract, service credential scopes, processor terms, rate limits, regional transfer analysis, rollback/exports and support ownership are approved.

| Pattern | Engineering requirement | Vendor reference / evidence |
|---|---|---|
| Push to CRM via adapter | Server-side with short-lived credential; configurable mapping; outbox event; no client-side tokens. | HubSpot Forms authenticated submission / CRM objects API are examples, not selected vendors. |
| Webhook-based confirmation | Signature verification and replay window; never use signature alone as business authorization. | Calendly webhooks signature guidance is reference. |
| CRM duplicate protection | Design around vendor duplicate rules; handle 409/422/adapter-specific errors and reconciliation. | Salesforce Web-to-Lead duplicate behaviour illustrates risk. |
| Bidirectional lifecycle | Only approved CRM stages may return into Sales Ops; public chatbot cannot query CRM or show sales notes. | Scoped read adapter + separate staff IAM. |

## 14  API interface registry — proposed, not deployed

| Route / event | Caller -> owner | Purpose and invariant |
|---|---|---|
| GET /public/v1/conversion/routes/{route_key} | Suivo backend -> signed route resolver | Read-only; active key, market eligibility, official URL and TTL. |
| POST /secure/v1/leads | Secure first-party form -> intake API | TLS + CSRF/origin controls, bot controls, form schema, accepted receipt. Not callable by LLM. |
| GET /secure/v1/leads/{reference} | Secure form authenticated session -> intake | Returns redacted receipt status only; prevents enumeration and IDOR. |
| POST /internal/v1/crm/submissions:upsert | Trusted worker -> CRM adapter | Outbox-driven; service identity, dedupe, idempotency and mapping. |
| POST /internal/v1/bookings/events:ingest | Verified scheduler webhook -> adapter | Signature/timestamp/replay guarded and provider ID bound. |
| GET /internal/v1/commercial/queue | Authenticated Sales Ops -> workflow | Scoped lead queue; no open web or anonymous access. |
| POST /internal/v1/commercial/leads/{id}:assign | Sales Ops manager -> workflow | RBAC, optimistic concurrency, audit and SLA version. |
| POST /internal/v1/commercial/preferences:withdraw | Privacy-controlled service -> consent ledger | Suppression + outbound propagation; durable receipt. |

## 15  Secure lead intake request and response contract

Illustrative POST /secure/v1/leads request — synthetic data; NOT an implemented endpoint

{  "form_version": "zs-demo-2026-10",  "purpose_id": "REQUEST_DEMO",  "route_key": "demo_tailored",  "person": {    "first_name": "Taylor",    "last_name": "Example",    "work_email": "taylor@example.invalid",    "phone": null  },  "organisation": {    "name": "Example Co",    "country": "GB",    "size_band": "11_50"  },  "interest": {    "persona": "ENTERPRISE_FINANCE",    "modules": [      "accounting_gl",      "accounts_payable"    ],    "priority_tags": [      "multi_entity"    ]  },  "privacy": {    "notice_version": "2026-10",    "marketing_opt_in": false,    "preference_capture": "EXPLICIT_FORM_CHOICE"  },  "anti_abuse": {    "challenge_token": "[ephemeral secure form token]"  }}

Illustrative accepted response; 202 only when durable queue receipt exists

{  "submission_reference": "ZS-DEMO-EXAMPLE-01",  "status": "ACCEPTED",  "booking_status": "NOT_BOOKED",  "crm_status": "PENDING",  "accepted_at": "2026-10-24T11:00:00Z",  "next_step": "OPTIONAL_SCHEDULING",  "privacy_notice_version": "2026-10"}

Required transport: request_id and Idempotency-Key; server derives source, market eligibility and current plan claims; neither browser nor model may set approval or consent validity. Reject excessive payloads, unauthorised cross-origin calls, malformed enums and financial/credential strings in open comments. If submission durability is uncertain, return retryable error and never “received” confirmation.

## 16  CRM adapter normalisation, field-map ownership and failures

| Canonical field | Example downstream object / restriction |
|---|---|
| work_email, first/last name | Contact/Lead attributes; permitted processing and data minimisation. |
| company name / country / segment | Company association after matching review; not independently verified. |
| submission source, product, route_key | Immutable source = ZoikoSuite Suivo first-party secure form; never invent attribution. |
| sales intent, role, qualified notes | Strict approved enum and human-entered notes; do not ship AI conversation transcript. |
| marketing_preference | No unsubscribe override; carry proof/reference, never infer newsletter opt-in. |
| assigned owner / stage | Defined by CRM workflow; no anonymous or LLM update privileges. |
| source_reference and dedupe_key | Stable custom external ID for idempotent upsert. CRM mapping verified in sandbox. |

## 17  Booking, availability and calendar event contract

Scheduling is conditional on an actual vendor and approved flow. The chatbot does NOT run a calendar search or reserve slots itself in Phase 1. The approved secure scheduling page may show localized availability after explicit visitor action. Slots and timezones are provider-confirmed, not LLM-generated. On event webhook ingestion: cryptographic verification -> dedupe provider ID -> match submission -> update booking binding -> append event -> reconcile CRM.

| Booking state | Required evidence / user message |
|---|---|
| OFFERED | Official booking CTA; no implied reservation. |
| SLOT_SELECTED | Provider hold or selection only; never use CONFIRMED label yet. |
| CONFIRMED | Verified provider event ID, UTC instant, IANA timezone, invite status, confirmation destination. |
| RESCHEDULED | New provider event version; preserve prior event and audit trail; update CRM task once. |
| CANCELLED | Provider-corroborated event; timely release of staff calendar capacity; no lead deletion. |
| WEBHOOK_DELAYED | Status remains pending; reconciliation job queries provider-authorised API when available. |

## 18  Lead scoring, routing and human qualification

No AI-generated lead score may autonomously determine discriminatory eligibility, pricing, creditworthiness, account entitlements or deny service. For sales prioritization, use transparent, editable criteria (e.g. selected persona, entity band, self-declared role, explicit interest). Avoid sensitive traits, derived income, browser fingerprinting and unapproved geographic discrimination. A human may override routing with an audited reason.

| Proposed tier | Rule sketch (illustrative) | Suggested queue |
|---|---|---|
| T1 — enterprise | Explicit request for SSO/SCIM, consolidation, group operations, custom framework or 25+ users. | Enterprise Solutions (human review). |
| T2 — growth | Multi-entity, approvals, integrations or automation, no enterprise signal. | Mid-market Sales / product specialist. |
| T3 — starter / trial | Basic accounting and one entity, confirms interest in self-service. | Self-serve trial route or general sales. |
| TRUST | Formal diligence, procurement, vendor risk, residency request. | Trust & Security diligence queue. |
| SUPPORT | Existing account, password, billing dispute, transaction inquiry. | Secure support, not sales queue. |

## 19  Commercial lifecycle and staff stage definitions

| Lifecycle stage | Entry evidence | Exit or escalation rule |
|---|---|---|
| NEW | First-party receipt accepted. | Assigned or disqualified via approved policy. |
| ROUTED | Queue assignment confirmed. | Owner accepts within approved work calendar. |
| IN_REVIEW | Human has reviewed permitted lead fields. | QUALIFIED / NEEDS_INFO / DISQUALIFIED. |
| QUALIFIED | Valid interest and contact route; documented fit; no financial commitment. | Demo, opportunity, trial referral per approved process. |
| DEMO_CONFIRMED | Provider event receipt. | Completed, cancelled, no-show confirmed by legitimate source. |
| TRIAL_REFERRED | Visitor clicked official activation; not account creation. | SIGNED_UP only from authenticated signup system evidence. |
| CLOSED_LOST / DISQUALIFIED | Reason enum, actor and timestamp. | May reopen under policy; marketing opt-out persists. |

## 20  Consent, UK B2B electronic marketing and other markets

There are different legal purposes: responding to a requested demo; fulfilling a confirmed meeting; direct marketing; marketing analytics; and customer support. A request to contact sales does not automatically authorize promotional subscriptions. For UK B2B, PECR distinguishes corporate subscribers from sole traders/certain partnerships; UK GDPR still applies to personal data, and marketing objections must be honored. Legal/DPO must approve lawful basis, channel, market and wording; the engineering platform should implement the strictest safe default where contact classification is uncertain.

| Processing purpose | UI / control |
|---|---|
| Respond to enquiry | Short privacy notice; purpose stated; lawful basis documented, not automatically “consent”. |
| Marketing email/SMS | Separate clear choice when required; not prechecked; independent suppression; preference provenance. |
| Sales outbound phone | Apply CTPS/TPS or regional do-not-call screening as applicable; keep channel-specific scope. |
| Scheduling and reminders | Operational messaging linked to requested meeting; no ad campaign piggyback. |
| Advertising conversion pixels | Do not load non-essential tracking before legally valid preferences; independent event counters otherwise. |
| Withdraw / object | Enforce immediate suppression in dispatch path; downstream propagation; audited acknowledgement. |

| LEGAL SIGN-OFF IS A LAUNCH BLOCKER The UK ICO B2B marketing guidance says it is under review following the Data (Use and Access) Act. Do not ship a universal UK “B2B consent exemption”. UK, US and EU counsel must approve the notices, bases, opt-outs, telemarketing checks and processor arrangements applicable to actual launch markets. |
|---|

## 21  Privacy, minimisation, data residency and retention

| Control | Mandatory implementation |
|---|---|
| Default public chat | No email/contact/profile collection in-chat; signed route key may include non-PII context only after explicit action. |
| First-party secure form | Only necessary fields, encrypted in transit/at rest, data subject notice, anti-abuse and regional purpose tags. |
| Data export | CRM/data processor transfer assessed by DPO, documented subprocessors, regional controls and contract safeguards. |
| Retention | Per-record retention_policy_id; schedule and lawful hold approved; no invented “30 days” for CRM records. |
| Right to object / withdrawal | Suppression precedes marketing worker send; preference ledger append-only, sync to CRM, vendor retries monitored. |
| DSAR/erasure | Identity validation in separate secure channel; index cleanup, CRM sync, audit of permitted redactions and backup plan. |
| Logs and analytics | No full email, phone, free text or conversation transcript in logs; pseudonymous IDs and aggregate measures. |

## 22  Attribution, measurement and commercial analytics

| Metric ID | Definition / exclusions |
|---|---|
| M01 CTA impression | Rendered permitted, above-fold or in-view CTA. Not a lead. Denominator = eligible sessions. |
| M02 CTA click-through | Validated first-party route open divided by CTA impressions. Consent-respectful collection only. |
| M03 secure form start | First form interaction on approved destination. Not a submission. |
| M04 accepted lead | Unique durable LeadSubmission acceptance; exclude test/bot/replay duplicates. |
| M05 confirmed demo | Unique verified scheduler event in CONFIRMED state; reschedules do not count additional bookings. |
| M06 show / complete | Specialist outcome recorded in CRM, not assumed from schedule date. |
| M07 trial activation | Authenticated signup system confirms activation; never use trial CTA click as activation. |
| M08 marketing permissions | Explicit consent/objection records separated from sales enquiry counts. |
| M09 lead delivery latency | ACCEPTED -> CRM_SYNCED elapsed time; percentile + backlog; missing vendor events visible. |
| M10 outcome by cohort | Observed cohort window, market, route, channel; suppressed small cohorts to protect privacy. |

Define a canonical event contract carrying event_id, event_type, occurred_at UTC, event_schema_version, correlation_id, purpose_id, consent_processing_class and source_system. Never send raw email or free-text content to analytics platforms. Internal telemetry and third-party advertising pixels require separate configuration and lawful-basis analysis.

## 23  Event catalog, schema and delivery semantics

| Event type (proposed) | Producer -> consumer | Invariant |
|---|---|---|
| commercial.route.clicked.v1 | Web widget -> aggregate metrics | No lead or booking is created. |
| commercial.lead.accepted.v1 | Secure intake -> outbox, Sales Ops | Emitted only after durable commit; opaque submission ID. |
| commercial.crm.sync.requested.v1 | Outbox -> CRM adapter | At least once + idempotent consumer; no sensitive payload in broker. |
| commercial.crm.synced.v1 | CRM adapter -> commercial ledger | Vendor ACK and external ID persisted. |
| commercial.booking.confirmed.v1 | Scheduling adapter -> commercial ledger | Signature verified; valid event ID and matched submission. |
| commercial.booking.changed.v1 | Scheduling adapter -> CRM / Ops | Cancellation or reschedule once by provider revision. |
| commercial.marketing.withdrawn.v1 | Preference service -> suppression + CRM | Stop queued marketing before publish/dispatch. |
| commercial.lead.assigned.v1 | Staff operations -> CRM / notification | Owner grant, audit and routing policy revision. |

## 24  Reliability, outbox, dead letters and reconciliation

Perform durable intake commit and outbox write atomically. Background CRM dispatch uses backoff with jitter, dead-letter after the approved retry budget and owner alert. Never block a confirmed website lead receipt solely due to transient CRM downtime. Do not declare “sent to sales” until reconciled. If a provider acknowledgement is lost, query by external submission ID before retrying create. Maintain a compensating action for wrong attribution or improper marketing routing.

| Failure | Expected public outcome | Ops recovery |
|---|---|---|
| CRM unavailable | Accepted local request receives truthful reference if durable. | Retry outbox; monitor backlog; eventual status. |
| Scheduler timed out | “Not confirmed yet”; no booking assertion. | Provider reconcile by correlation before retry. |
| Duplicate lead webhook | No second lead, event or notification. | Idempotent bind; audit duplicate received. |
| Claim resolver stale | No specific quoted price/plan, display current official URL. | Block P1 claim; owner refresh and QA. |
| Marketing opt-out sync fails | Suppress in first-party dispatch now. | Retry vendor sync; alert DPO if overdue. |
| Ingestion poisoned with PII | Reject/quarantine payload; do not forward to CRM/model. | Privacy investigation and remediation. |

## 25  Security and fraud controls in commercial paths

| Attack class | Control and test requirement |
|---|---|
| Fake lead / spam / bot storm | Layered rate limits, honeypot/challenge on secure form, queue budget, unique sources, anti-enumeration. |
| PII exfiltration by chatbot | Model tools omit CRM/calendar; no CRM tokens in browser; network policy denies write path. |
| Link injection / phishing | Official CTA route registry and allowlisted origins; no LLM-selected arbitrary redirect. |
| Webhook impersonation | HMAC/signature and timestamp verification, replay window, delivery ID dedupe, source-identity correlation. |
| CRM account takeover | Dedicated scoped service principal, secret rotation, no browser-visible token, access audit. |
| Email header injection | Trusted email provider templates, escape all rendered fields, CRLF validation, no untrusted BCC or redirect. |
| Mass assignment | Server derives lead owner/stage; ignores client-supplied privileged values. |
| Insider data misuse | Role- and purpose-scoped staff access; field redaction, bulk export approval and audit. |

## 26  Operational communications and lead notification rules

| Message | Trigger | Required guard |
|---|---|---|
| Demo request receipt | LeadSubmission ACCEPTED after durable write. | Accurate reference; privacy notice; no false booking. |
| Booking confirmation | Provider CONFIRMED event after verified binding. | TZ details; event status; approved cancellation/reschedule route. |
| Demo reminder | Validated provider schedule and user preference. | No marketing content unless independently permitted. |
| Owner assignment | Sales Ops queue has accepted/assigned lead. | Staff-only; no customer data in public chat. |
| Sales follow-up | Assigned staff acts within approved market/channel policy. | Suppression, business contact classification, opt-out and legal basis. |
| Trial nurture | Only actual trial activation evidence and approved messaging permissions. | Never infer trial activation from CTA click. |

## 27  Access-control matrix and segregation of duties

| Role | Allowed responsibilities | Explicit denial |
|---|---|---|
| Commercial Content Editor | Draft offer explainer and links; request claim change. | Approve own claim or change active price directly. |
| Commercial Approver | Verify price book, market/time, plan trial eligibility. | Use public chatbot to override claim authority. |
| Sales Operations Agent | Read assigned lawful lead fields, log contact outcome. | Browse all chat transcripts, change privacy preferences. |
| Sales Manager | Manage approved queues, owner assignment and lawful stage rules. | Override privacy suppression or audit records. |
| CRM Integrations Engineer | Operate adapter maps, retry/incident controls in limited environment. | View live PII by default, approve own production deployment. |
| Privacy/DPO | Approve notices, processing, retention, subject rights, suppression audits. | Manipulate sales counts or silently broaden data purpose. |
| Security/SRE | Revoke integrations, rotate secrets, disable outbound processing in incidents. | Unlogged access to prospect data or indefinite consent bypass. |

## 28  Administration console extension — Document 09 compatibility

Extend the Doc09 authenticated staff console via an explicitly versioned Commercial Work Queue. Do NOT mix commercial leads with customer support tickets: distinct lifecycle, retention, permission, SLA policies and source-of-truth IDs. A Sales Ops staffer may see contact details only after a verified secure-form submission and a purpose-scoped role grant.

| Surface ID | Minimum UI controls |
|---|---|
| COM-ADM-01 / Lead Inbox | Assigned, unassigned, pending CRM, dedupe review; filters by purpose, market, stage, accepted_at. |
| COM-ADM-02 / Lead Detail | Receipt, contact fields (role-scoped), purposes/preferences, selected priorities, owner, activity timeline, CRM binding. |
| COM-ADM-03 / Booking Reconciliation | Provider event and status, unmatched webhook queue, reschedule/cancel lineage, repair action with audit. |
| COM-ADM-04 / Suppression & Privacy | Preferences, opt-out date, list sync state, rights ticket link, restricted DPO-only actions. |
| COM-ADM-05 / Funnel Quality | Aggregate route starts, form acceptance, actual bookings, CRM sync lag; cohort and consent banners. |
| COM-ADM-06 / Claim-to-CTA Registry | Approved destination keys and market/plan linkage; cannot publish unapproved claim. |

## 29  Commercial SLAs, service-level indicators and staffing

Commercial response times and working hours require actual signed Sales Ops policy. No numerical customer promise should be exposed by the chatbot until staffing and routing capacity are verified. SRE should adopt operational internal alerts for lead intake durability, CRM backlog and stale booking events; numeric targets below are initial engineering proposals, NOT external SLAs.

| SLO / proposed internal target | Measurement and gating |
|---|---|
| Intake accepted durability: >= 99.9% | Success commits / valid submissions, monthly, excluding deliberate 4xx; verify in staging load tests. |
| Duplicate write rate: 0 in idempotency scenarios | Same idempotency key and provider event ID never increase unique accepted entities. |
| Suppression enforce time: before next outbound marketing send | Hard gate on dispatch path; cannot be relaxed by vendor lag. |
| CRM sync backlog threshold: owner-defined | Alarm on queue age and dead letters; set absolute threshold after measured capacity. |
| Booking truth: 100% of confirmations provider-backed | No CONFIRMED without valid provider event ID and signature evidence. |
| Funnel analytic privacy: 0 unauthorized PII payloads | Schema tests, log scans and vendor export audits. |

## 30  Engineering work packages and build order

| Work package | Sequence / acceptance dependency |
|---|---|
| WP01 Product route and claim reconciliation | Resolve /demo vs /book-demo, market routing, signed pricing/trial registry and fallback links. |
| WP02 Secure form schema / UX | Single source of truth for demo fields; purpose notice/marketing option; mobile and WCAG validation. |
| WP03 Durable first-party intake | Validated API, transactional outbox, receipt and idempotency; test before CRM integration. |
| WP04 CRM mapping and sandbox adapter | Vendor selected and DPA reviewed; typed mapping/upsert/dedupe; failure and retry tests. |
| WP05 Booking/scheduler adapter | Approved provider and secure webhook; timezone/reschedule/cancel states; repair jobs. |
| WP06 Staff commercial queue and consent ledger | Granular roles, secure lead detail and suppression before outbound. |
| WP07 Metrics/observability and QA | Privacy-safe funnels, audit manifests, red-team, accessibility and trial/booking claim tests. |
| WP08 Controlled rollout | ROUTE_ONLY -> FORM_ACCEPTED -> CRM_SYNC -> SCHEDULER_SYNC by feature flag, with rollback. |

## 31  Test execution method and release blocking

Execute tests in CI against schema/contract mocks, sandbox vendor integrations and a fenced staging environment. Each test stores a stable test ID, owner, build revision, release tuple, expected outcome, actual result, trace/evidence link, severity and reviewed timestamp. Redact personal data. P0 failures (unconsented marketing, fake booking, silent lead creation, mispriced claims, leaked CRM access) are non-waivable. All schema examples below are scaffolding; engineering must own executable OpenAPI/AsyncAPI suites.

## 32  Acceptance-test catalog — 72 scenario-level tests

### CTA & route / 6 tests

| Test ID | Expected executable scenario | Gate |
|---|---|---|
| COM-T001 | Signed demo route offered only when route active for explicit market. | P0 |
| COM-T002 | Disabled market route returns safe public alternative with no claim. | P1 |
| COM-T003 | CTA click does not create lead or booking. | P0 |
| COM-T004 | Unapproved external redirect is blocked. | P0 |
| COM-T005 | No more than one primary commercial CTA in one bot response. | P1 |
| COM-T006 | Legacy /demo route and /book-demo mapping audited before release. | P1 |

### Pricing & trials / 6 tests

| Test ID | Expected executable scenario | Gate |
|---|---|---|
| COM-T007 | Current signed price/market/currency/term required for specific quote. | P0 |
| COM-T008 | Expired plan fact blocks quote even when cached model answer says yes. | P1 |
| COM-T009 | Trial text does not assert payment/account creation. | P0 |
| COM-T010 | Starter/Growth trial link appears only when commercially enabled. | P1 |
| COM-T011 | Enterprise plan does not display unapproved flat price or auto signup. | P0 |
| COM-T012 | Pricing release can be rolled back independently from runtime. | P1 |

### Lead intake / 6 tests

| Test ID | Expected executable scenario | Gate |
|---|---|---|
| COM-T013 | Accepted receipt only after durable commit and outbox write. | P1 |
| COM-T014 | Malformed email, bad enum or excessive size returns typed 422. | P1 |
| COM-T015 | Optional phone omission does not block demo intake. | P0 |
| COM-T016 | Client-supplied staff owner or consent approval field ignored or rejected. | P1 |
| COM-T017 | Form includes notice/version and purpose before submission. | P1 |
| COM-T018 | Sensitive account numbers in free text are rejected or safely quarantined. | P1 |

### Identity & dedupe / 6 tests

| Test ID | Expected executable scenario | Gate |
|---|---|---|
| COM-T019 | Exact idempotent replay returns prior receipt. | P1 |
| COM-T020 | Conflicting duplicate idempotency key returns 409. | P0 |
| COM-T021 | Duplicate CRM mapping does not create second contact. | P0 |
| COM-T022 | Same organisation does not silently merge unrelated people. | P0 |
| COM-T023 | Existing customer match is not disclosed in public chat. | P1 |
| COM-T024 | Dedupe review and merge is role-scoped and reversible/audited. | P1 |

### CRM integration / 6 tests

| Test ID | Expected executable scenario | Gate |
|---|---|---|
| COM-T025 | Worker uses approved narrow service principal, never browser token. | P1 |
| COM-T026 | CRM 429/5xx causes exponential backoff and durable retry. | P1 |
| COM-T027 | CRM outage after accepted intake leaves true receipt and pending status. | P1 |
| COM-T028 | Dead-letter can be replayed safely without duplicate. | P0 |
| COM-T029 | Vendor external ID reconciled to original submission. | P1 |
| COM-T030 | CRM field map rejects undeclared PII/free-text fields. | P1 |

### Bookings & timezone / 6 tests

| Test ID | Expected executable scenario | Gate |
|---|---|---|
| COM-T031 | Slot selection alone cannot produce CONFIRMED. | P0 |
| COM-T032 | Booking confirmation requires provider ID and verified status. | P0 |
| COM-T033 | UTC, IANA timezone and DST transition remain correct. | P1 |
| COM-T034 | Duplicate provider webhook does not double-count bookings. | P0 |
| COM-T035 | Cancelled booking removes future reminder and updates lifecycle. | P1 |
| COM-T036 | Late/out-of-order reschedule webhook reconciles with provider revision. | P1 |

### Privacy & marketing / 6 tests

| Test ID | Expected executable scenario | Gate |
|---|---|---|
| COM-T037 | Unchecked marketing option stays off in storage and downstream CRM. | P0 |
| COM-T038 | Demo reply remains possible independent of marketing opt-in where lawful. | P1 |
| COM-T039 | Opt-out suppresses outbound dispatch before next send. | P0 |
| COM-T040 | UK sole-trader uncertainty follows strict channel policy. | P1 |
| COM-T041 | Tracking pixel does not load without legally required permission. | P0 |
| COM-T042 | DSAR/erasure request goes through secure identity-checked workflow. | P1 |

### Sales Operations / 6 tests

| Test ID | Expected executable scenario | Gate |
|---|---|---|
| COM-T043 | Lead queue returns only staff-scoped permitted records. | P1 |
| COM-T044 | Role grants prevent cross-region bulk PII exports. | P1 |
| COM-T045 | Sales stage transitions record actor, timestamp and allowed reason. | P1 |
| COM-T046 | Lead owner changes do not reset audit or consent evidence. | P1 |
| COM-T047 | Support/login enquiries do not enter prospect sales queue. | P1 |
| COM-T048 | Commercial queue IDs cannot be used to enumerate helpdesk cases. | P0 |

### Security & abuse / 6 tests

| Test ID | Expected executable scenario | Gate |
|---|---|---|
| COM-T049 | Prompt injection cannot invoke CRM or scheduler commands. | P0 |
| COM-T050 | Bot/fraud limits work without blocking routine accessibility use. | P0 |
| COM-T051 | Webhook signature, replay ID and timestamp enforced. | P1 |
| COM-T052 | Open redirects and phishing URLs rejected. | P1 |
| COM-T053 | No secrets or full visitor transcript in model or analytics export. | P0 |
| COM-T054 | Browser CSRF/origin and form abuse tests pass. | P1 |

### Analytics & truth / 6 tests

| Test ID | Expected executable scenario | Gate |
|---|---|---|
| COM-T055 | CTA click, lead accepted, booking confirmed and trial activated are distinct. | P1 |
| COM-T056 | Funnel excludes test traffic, replays and duplicates. | P0 |
| COM-T057 | No raw email/IP/phone goes to analytics event payload. | P1 |
| COM-T058 | Attribution preserved only within privacy-approved scope. | P0 |
| COM-T059 | CRM sync latency starts at ACCEPTED and stops only on ACK. | P1 |
| COM-T060 | Quarter-end conversion cohort window and denominator documented. | P1 |

### Failures & recovery / 6 tests

| Test ID | Expected executable scenario | Gate |
|---|---|---|
| COM-T061 | Provider timeout produces truthful pending status and recovery link. | P1 |
| COM-T062 | Claim registry outage suppresses specific pricing commitments. | P1 |
| COM-T063 | Preference propagation incident halts marketing worker. | P1 |
| COM-T064 | Rejected intake never displays “received” to user. | P0 |
| COM-T065 | Outbox recovery after disaster replays without duplicate. | P0 |
| COM-T066 | Kill switch disables new CRM writes but preserves existing verified leads. | P1 |

### Accessibility & launch / 6 tests

| Test ID | Expected executable scenario | Gate |
|---|---|---|
| COM-T067 | Form/CTA keyboard-only completion and visible focus confirmed. | P1 |
| COM-T068 | Error messages attached to fields and announced to screen readers. | P1 |
| COM-T069 | Mobile at 320 CSS px has no loss of functionality. | P1 |
| COM-T070 | Date and timezone selections meet accessible label requirements. | P1 |
| COM-T071 | Each feature flag has reversible rollout and deterministic fallback. | P1 |
| COM-T072 | Go-live evidence pack has every owner sign-off and no P0 failures. | P1 |

## 33  Predecessor requirements crosswalk

| Controlling doc | Non-waivable compatibility tests |
|---|---|
| 01 / Public UX | CTA states, demo wireframe, audience roles, public-only boundary and accessibility. |
| 02 / Intents & prompt | Restricted intent intercept, deterministic commercial answers, no silent lead capture. |
| 03 / Knowledge & sources | Signed P1 claims, canonical ten modules, expiry/revocation and market logic. |
| 04 / Architecture | No private data routing, model with no CRM tools, separate first-party action boundary. |
| 05 / API & events | Correlation and idempotency, typed errors, replay-safe CloudEvents-compatible envelope. |
| 06 / Security & privacy | Purpose and suppression, data minimisation, vendor DPA, prompt injection and webhook authentication. |
| 07 / QA | All COM-T tests linked into golden suite; non-waivable P0 launch blocks. |
| 08 / Deployment | Four-artifact version tuple, progressive flags, CRM/booking rollback and incident runbooks. |
| 09 / Staff console | Separate support case vs sales lead, staff SSO/RBAC, audit and no false case receipts. |

## 34  Decision register — unresolved by this document

| ID | Accountable owner | Evidence / decision required |
|---|---|---|
| D10-01 | Sales/Product | Canonical /demo vs /book-demo and locale route registry |
| D10-02 | Commercial / Finance | Approved plan/price/trial release and market eligibility master |
| D10-03 | Sales Operations | CRM vendor, schema, entity dedupe and owner model |
| D10-04 | IT / SRE | Booking provider, timezone, event contract and operational owner |
| D10-05 | Legal / DPO | Jurisdiction-by-jurisdiction lawful basis and marketing channels |
| D10-06 | Privacy / Security | Processor terms, data residency, transfer and retention schedule |
| D10-07 | Product / UX | Approved secure form field sets, notice copy and conversion CTAs |
| D10-08 | Sales / Customer Success | Lead qualification criteria, queues, staffing and response policy |
| D10-09 | Security / SRE | Webhook signing, secrets rotation, replay/repair controls |
| D10-10 | Data / Legal | Analytics vs tracking consent and cohort policy |
| D10-11 | Security / AI | No-CRM-tool egress and private-network denial test evidence |
| D10-12 | SRE / QA | Outbox/DR, DLQ replay and CRM provider outage runbooks |
| D10-13 | Brand / IP | External Suivo display-name usage legal sign-off |
| D10-14 | Product / Finance | Trial end-state and authenticated billing acceptance flow |
| D10-15 | CRM Engineering | Idempotent external ID and duplicate-rule compatibility test |
| D10-16 | Founder / Global CTO | Authorize each rollout gate independent of drafting approval |

## 35  Critical reassessment — defects prevented before delivery

| Review challenge | Refinement incorporated in final version |
|---|---|
| Are we silently expanding Doc04/05? | All new write interfaces run only from secure first-party form/worker and require separate ADR; Suivo agent gains no write tool. |
| Does “accepted” confuse commercial and support cases? | Separate LeadSubmission record, support Case receipt, scheduler event, CRM binding and payment activation. |
| Are the trial/plan claims live? | Current website is a source reference; runtime must use signed, effective market-scoped claim release. |
| Does sales outreach rely on unchecked consent? | Purpose-based enquiry processing, marketing permissions separately captured and channel-specific legal screening. |
| Is a booked demo equal to revenue? | Conversion metrics distinguish route click, accepted lead, provider-confirmed meeting, attended meeting, trial activation and paid subscription. |
| Can operational failures fabricate success? | Outbox and truthful pending states, webhook checks, reconciliation and non-waivable acceptance tests. |
| Could vendor CRM dedupe destroy records? | Idempotency distinct from match/merge, independent review, stable external ID and audit evidence. |

## 36  Controlled rollout and release sign-off record

| Gate | Pass evidence | Approver |
|---|---|---|
| G10-0 / Route-only | Official route registry, commercial claim resolver and safe CTA UX validated. | Product + Brand + QA |
| G10-1 / Secure intake | Form privacy, typed schema, durable receipt, idempotency and abuse tests pass. | Product + Legal/DPO + Security |
| G10-2 / CRM sync | Vendor approval, field maps, consent/suppression, outbox/replay, RBAC and sandbox evidence. | Commercial + CRM owner + DPO + Security |
| G10-3 / Bookings | Provider signatures, timezone, cancellation, false booking negative tests pass. | Sales Ops + SRE + QA |
| G10-4 / Production | 72 COM-T plus predecessor critical suites, rollback drill, monitored canary, signed release tuple. | Founder / Global CTO + QA + SRE |

| APPROVAL EVIDENCE RECORD Approver: __________________  Role: __________________  Decision: APPROVE / APPROVE WITH CONDITIONS / REJECT  /  Date: __________  /  Open blocker references: __________. Formal document approval authorizes engineering specification work only. Production enablement is a separate evidence-based release decision. |
|---|

## 37  Appendix A — example event payload and policy-as-code

Event envelope example — no PII or raw conversation text

{  "specversion": "1.0",  "id": "evt_example_001",  "source": "urn:zoikosuite:secure-intake",  "type": "commercial.lead.accepted.v1",  "time": "2026-10-24T11:00:01Z",  "subject": "submission/opaque-id",  "datacontenttype": "application/json",  "data": {    "submission_id": "sub_example_001",    "purpose": "REQUEST_DEMO",    "source_route": "demo",    "outbox_id": "out_example_001",    "schema_version": "1.0",    "consent_evidence_id": "consent_example_001"  }}

Deterministic route and lead-creation policy sketch

{  "public_assistant": {    "tools": [      "PUBLIC_CLAIM_READ",      "APPROVED_ROUTE_READ"    ],    "crm_write": false,    "calendar_write": false,    "payment_write": false  },  "secure_intake": {    "allow_submission_only_if": [      "form_valid",      "abuse_passed",      "purpose_recorded",      "required_notice_shown"    ],    "receipt_requires": "DURABLE_COMMIT",    "crm_delivery": "OUTBOX_IDEMPOTENT"  },  "marketing_dispatch": {    "allow_only_if": [      "regional_channel_policy_passed",      "not_suppressed",      "required_permission_valid"    ]  }}

## 38  Appendix B — CRM field mapping, feature flags and errors

Proposed feature and vendor-neutral adapter configuration

{  "features": {    "commercial_routes_enabled": true,    "secure_lead_intake_enabled": false,    "crm_sync_enabled": false,    "booking_sync_enabled": false,    "third_party_tracking_enabled": false  },  "crm_adapter": {    "provider": "UNDECIDED",    "outbox_topic": "commercial.crm.sync.requested.v1",    "external_identity": "submission_id",    "pii_field_allowlist": [      "email",      "first_name",      "last_name",      "company",      "country"    ],    "consent_mode": "PURPOSE_BOUND"  },  "errors": [    "INVALID_FORM",    "IDEMPOTENCY_CONFLICT",    "RATE_LIMITED",    "CRM_UNAVAILABLE",    "BOOKING_UNCONFIRMED",    "CLAIM_STALE"  ]}

## 39  Appendix C — references and verified primary-source URLs

ZoikoSuite current pricing — https://zoikosuite.com/pricing — Plan structure, trial qualification, annual pricing caution and user-facing disclosures.

ZoikoSuite demo booking — https://zoikosuite.com/book-demo — Step structure, existing public fields, optional phone and no account/payment on demo.

ZoikoSuite core modules — https://www.zoikosuite.com/core-modules — Four domains / ten canonical modules.

ZoikoSuite Trust Center — https://zoikosuite.com/trust-center — Assurance claims and explicit certification status.

ICO UK B2B marketing — https://ico.org.uk/for-organisations/direct-marketing-and-privacy-and-electronic-communications/business-to-business-marketing/ — PECR + UK GDPR distinctions by contact type and marketing channel; review status noted.

ICO storage and access technologies — https://ico.org.uk/for-organisations/direct-marketing-and-privacy-and-electronic-communications/guidance-on-the-use-of-storage-and-access-technologies/ — 2026 final guidance governing tracking and storage/access technologies.

ICO consent — https://ico.org.uk/for-organisations/uk-gdpr-guidance-and-resources/lawful-basis/consent/ — Consent standards and withdrawal rights (updated August 2026).

HubSpot API objects — https://developers.hubspot.com/docs/api-reference/latest/crm/using-object-apis — Example of vendor CRM upsert, association and object model; NOT selected vendor.

HubSpot secure forms — https://developers.hubspot.com/docs/api-reference/legacy/marketing/forms/v3-legacy/submit-data-authenticated — Example of authenticated server-side form submission with consent options.

Salesforce duplicate rules — https://help.salesforce.com/s/articleView?id=000387310&language=en_US&type=1 — Potential Web-to-Lead duplicate blockage and need for explicit error handling.

Calendly webhook signatures — https://developer.calendly.com/api-docs/overview/webhooks/webhook-signatures — Example booking event source authentication; NOT selected vendor.

W3C WCAG 2.2 — https://www.w3.org/TR/WCAG22/ — Accessibility criteria for forms, feedback and responsive chat.

Sources were checked on 24 October 2026. Public pages are evidence snapshots; official product commercial releases and internal approvals take precedence for live promises. External systems listed are examples, not procurement decisions.

## 40  Appendix D — glossary, assumptions and next-series boundary

| Term | Precise meaning |
|---|---|
| ROUTE_ONLY | Chat offers official secure link; no CRM lead or booking created. |
| ACCEPTED | Secure first-party submission persisted with durable receipt, not a scheduler or CRM ACK. |
| CRM_SYNCED | Downstream CRM acknowledges upsert and external binding is persisted. |
| BOOKING_CONFIRMED | Provider-backed appointment with verified event identity and timezone. |
| TRIAL_REFERRED | Visitor clicked authorized trial path; NOT an account activation. |
| TRIAL_ACTIVATED | Authenticated signup/billing system returns successful activation event. |
| P1 claim | High-impact assertion that requires valid, signed product/commercial evidence. |
| Suppression | Prevent marketing sends following objection/withdrawal independent of vendor sync delay. |
| Outbox | Durable transactional event queue connecting secure intake to asynchronous CRM worker. |
| Document 11 | Next controlled engineering document only after separate approval of this Document 10. |

CLOSE OF DOCUMENT 10. For approval by Founder / Global CTO / Commercial Owner / Product / Legal-DPO / Security / QA. No Document 11 is issued as part of this submission.
