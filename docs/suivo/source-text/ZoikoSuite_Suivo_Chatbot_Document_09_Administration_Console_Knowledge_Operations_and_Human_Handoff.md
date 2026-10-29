<!-- Extracted from ZoikoSuite_Suivo_Chatbot_Document_09_Administration_Console_Knowledge_Operations_and_Human_Handoff.docx -->

ZOIKO GROUP  /  ZOIKOTECH INC.  /  ZOIKOSUITE

SUIVO  /  PUBLIC AI ASSISTANT

Administration Console, Knowledge Operations& Human Handoff Specification

ENGINEERING DOCUMENT 09   |   VERSION 1.0   |   24 OCTOBER 2026

A controlled, implementation-oriented specification for staff administration of the Suivo public chatbot: user journeys, least-privilege authorisation, operational work queues, knowledge editorial and approval workflows, verified human handoff, commercial and source controls, privacy-safe quality analytics, audit evidence, incident intervention and executable acceptance tests.

| APPROVAL STATUS AND IMPORTANT LIMITATION FOR FOUNDER / GLOBAL CTO / PRODUCT / SECURITY / SUPPORT / KNOWLEDGE GOVERNANCE APPROVAL. This document describes target operations, not verified running staff consoles, CRM integrations, helpdesk licences, live case volumes, staffing levels, response-time commitments or repository code. All example routes, roles, SLAs, schemas and navigation are proposals requiring engineering and business owner confirmation. |
|---|

| Document control | Canonical position |
|---|---|
| Document ID | ZS-SUIVO-ENG-009 / v1.0 / Internal controlled engineering document |
| Product boundary | ZoikoSuite, public assistant key zoikosuite-public-ai; internal working display name Suivo; public name requires separate brand/IP clearance. |
| Lineage | 01 scope/wireframes; 02 intents; 03 knowledge; 04 architecture; 05 APIs/events; 06 security/privacy; 07 QA; 08 release/operations. |
| Scope | Authenticated STAFF console only, plus governed public-to-secure-channel handoff. Not a tenant customer dashboard or authenticated financial assistant. |
| Responsible groups | Founder/Product; Support/Customer Success; Knowledge Operations; Commercial; Legal/DPO; Security; AI/ML; Backend; SRE; QA/Accessibility. |
| Hard boundary | No direct customer finance, HR, payroll, tax, bank, legal, or protected ledger access from public chatbot or its admin console. |

| APPROVAL AND DELIVERY SEQUENCE This document defines the next engineering work package, subject to owner sign-off. Approving the document does not authorise go-live, introduce new public pricing promises, or relax the previous six non-waivable gates. Do not progress to Document 10 until separate approval of Document 09. |
|---|

## 01  Executive determination and critical refinement summary

Create a small, secure staff administration surface that joins three essential workflows: (a) manage and respond to verified human handoff cases, (b) create/review/release permitted public knowledge and claims, and (c) measure chatbot quality and intervene safely. The UI is a policy-enforcing operational client; it must not become a backdoor into ZoikoSuite customer systems.

| Critical defect prevented | Refined control / measurable impact |
|---|---|
| Chatbot click mislabelled as a human case | Case exists only after verified secure-channel acknowledgement; analytics clicks and route launches remain separate events. |
| Reviewer approves own high-impact content | Server-side ABAC + separation of duties; no sole self-approval for P1 claims and no self-promotion via role stacking. |
| Stale source shown during publication transition | Immutable versioned releases, time-bound validity and emergency revocation evaluated at response commit. |
| Agent reads raw public chat for convenience | Default handoff contains intent/category/source/consent only; transcript requires separate basis, explicit minimisation and approval. |
| SLA clock starts at public CTA click | SLA begins on accepted case creation per case-category policy, not when a chatbot response offers a link. |
| Powerful universal admin role | Split Support, Content, Commercial, Security and Release roles with step-up access and immutable evidence. |

## 02  Controlled references, definitions and precedence

Documents 01–08 remain controlling for public-chat states C00–C12, intents I01–I30, PUBLIC_APPROVED source status, high-impact P1 signed claims, six internal event types, typed public API envelopes, network isolation and QA/release gates. This document adds staff console behavior and secure handoff governance only; it does not redefine a previously approved public endpoint.

| Term | Binding definition |
|---|---|
| Handoff offered | Suivo displays an approved route_key; no request to support system is implied. |
| Handoff initiated | Visitor opens the destination; browser event records a link click, not ticket creation. |
| Verified case | A secure receiving form/service accepts submission and returns its own opaque case_id; only then show confirmation. |
| PUBLIC_APPROVED | Classification allowing a source version into anonymous retrieval, subject to independent release, expiry and revocation. |
| P1 material claim | Price, plan eligibility, market coverage, certification, data residency or similarly high-consequence published assurance; requires authoritative signed evidence. |
| Operator intervention | Authenticated staff action via scoped control-plane API; may not call the model as an authority or bypass security checks. |

## 03  Staff console information architecture and design rules

Figure 1 — Target low-density staff operations layout; shown data are purely illustrative.

Navigation is a controlled staff task taxonomy, not a replica of the ZoikoSuite customer-facing module sidebar. Use existing enterprise shell components only if verified, and do not import customer dashboard navigation or account entitlements.

| Navigation section | Screens / designated route keys |
|---|---|
| Operations | ADM-01 Command Center; ADM-02 Handoff Queue; ADM-03 Case Detail |
| Knowledge | ADM-04 Source Library; ADM-05 Draft/Source Editor; ADM-06 Review & Approvals; ADM-07 Release/Revocation |
| Assurance | ADM-08 Claims & Taxonomy (read-only for most roles); ADM-09 Conversation Quality; ADM-10 Audit & Evidence |
| Administration | ADM-11 Staff Access; ADM-12 Configuration & Incident Controls |

## 04  Responsive, interaction and accessibility design system

| Requirement | Staff UI rule / acceptance evidence |
|---|---|
| Layout | Desktop sidebar no wider than 240 px; main content with max 4 summary KPIs; no forced 12-card dashboards; breakpoint tested on 320px, tablet, 1440px. |
| Disclosure | Every operational view labels environment, role, data classification and freshness; destructive actions state impact before submit. |
| Tables | Meaningful headers, sortable columns with accessible labels, keyboard-operable row actions, pagination and empty/error states; no color-only statuses. |
| Dialogs | Accessible focus trapping/return; action-specific confirmations for publish, revoke, delete, override and kill-switch. |
| Status | Text status plus shape/icon; queue urgency has absolute due timestamp plus timezone, never only green/amber/red. |
| Feedback | Non-blocking success messages, error summaries and per-field validation; in-progress operations show lifecycle and final server receipt. |
| Usability | Task-specific command labels; avoid ambiguous “Approve” without source ID/version and affected markets. |
| Accessibility | WCAG 2.2 Level AA target; test keyboard order, skip links, 200%/400% zoom, high contrast, 1.4.3 contrast and 4.1.3 status messages. |

## 05  Identity, authentication, sessions and staff trust boundary

All staff functions require SSO or an approved first-party identity provider; MFA and phishing-resistant factors are required for high-impact privileged actions where supported. Staff claims carry principal_id, staff_org, roles, environment, conditional grants and expiry. Backends enforce grants independently of frontend menus.

| Control | Server-side implementation |
|---|---|
| Console perimeter | Admin origin and API gateway segregated from public browser; CSRF protection, strict origin policy and security headers. |
| Identity binding | Short-lived access tokens validated for audience and issuer; MFA freshness and device/session risk checks for publication or incident toggles. |
| Least privilege | Deny-by-default scoped permissions, object-level authorisation on cases and source versions, no inherited customer tenant access. |
| Privileged action | Step-up confirmation, reason code, change ticket, and dual approval where P1 claims or broad impacts are present. |
| Break-glass | Time-boxed emergency privileges, two-person retrospective review, automatic expiry, alerting; restricted to suspension/revocation, not content fabrication. |
| Audit | Every state-changing request persists actor, previous and new state, reason, target hash, approval reference, UTC timestamp and request ID. |

## 06  Staff role catalogue and separation-of-duties model

| Role ID | May perform | Expressly forbidden |
|---|---|---|
| R01 / Support Agent | View assigned or team-permitted cases; reply through secure case channel; apply approved dispositions. | Change public knowledge, claims, access grants, source signatures or case ownership outside scope. |
| R02 / Support Lead | Reassign and escalate cases; inspect team queues; resolve quality disputes. | Create claims or publish KB; read private financial records. |
| R03 / Knowledge Editor | Draft source revisions, attach evidence, request review and preflight checks. | Final approval of own material revision; publish directly. |
| R04 / Content Approver | Independently review low-risk help content and accept/reject with recorded grounds. | Approve own edits; approve P1 without specialist authority. |
| R05 / P1 Subject Owner | Attest pricing/coverage/security source truth for owned subject, market and effective window. | Single-handedly publish own high-impact claim. |
| R06 / KB Release Manager | Promote only approved signed bundles passing Doc07 tests; roll back under change control. | Edit claims, override revocation or become sole P1 approver. |
| R07 / Safety/SecOps | Inspect security signals, quarantine malicious sources, activate approved kill-switch. | Read unnecessary case PII; modify commercial prices. |
| R08 / Privacy/DPO | Approve data handling/retention and controlled subject-rights workflow; oversee audits. | Use privacy authority to change content truth or case facts. |
| R09 / IAM Administrator | Provision limited staff grants after approved request; revoke access; run access reviews. | Grant own elevated role or suppress own privilege audit. |
| R10 / QA Read-only | Review metrics, release evidence and redacted case samples in assigned scope. | Change records, read unnecessary PII, publish or trigger incidents. |

## 07  Explicit permission matrix and policy evaluation

| Operation | Required rights + independent control | Deny cases |
|---|---|---|
| cases.read / cases.assign | support.case.read own_scope; lead.assign for selected queues; purpose-bound data projection. | Out-of-scope case, deleted grant, cross-region denial. |
| cases.reply / resolve | support.case.write, accepted consent and verified channel; response logging. | Public chat only, no case receipt, unverified identity-sensitive reply. |
| sources.create / edit | kb.source.write and owner-bound source classification; all writes are new immutable versions. | Private, unidentified or cross-product source. |
| sources.approve | kb.source.approve, independent reviewer, evidence and current-policy attestation. | Author=self or role stacking conflict. |
| claims.approve / release | Subject owner attestation, separate release authority, two-person P1 approval + signed manifest. | Missing effective date, malformed terms, invalid evidence, expired approver grant. |
| releases.promote / rollback | release.deploy and verified QA results; change record; release tuple compatibility. | Outstanding P0, invalid signatures or revoked claim. |
| kill_switch / revoke | SecOps scoped emergency authority, recorded reason, alert and post-incident review. | Browser-origin call, unaudited arbitrary re-enable. |
| audit.export | audit.read with legal purpose and scope, short-lived signed export, DPO gate for personal data. | Bulk unauthorised export or user-level tracking. |

| PERMISSION IS NEVER MODEL-GENERATED Access, assignment, publication, case visibility, signed content status and incident overrides must be decided by deterministic IAM/ABAC services. LLM summaries and operator UI visibility have zero authorisation effect. |
|---|

## 08  ADM-01 Command Center — operational scope

| Region / widget | Content contract / controls |
|---|---|
| A / Status strip | Environment, runtime health, active release tuple; typed timestamp for latest event; no green status derived from a single 200. |
| B / Queue summary | Open cases, due soon, awaiting verification and P0 safety case counts; denominator and refresh timestamp. |
| C / Knowledge status | Sources pending, P1 expiring, blocked claims, current signed KB release and active revocation epoch. |
| D / Quality exceptions | Material-claim abstention trends, source citation failures, flagged response patterns; aggregated, no message content by default. |
| E / Primary action | Open My Queue; review assigned approvals; consult release health; disable unsafe source with permission. |
| F / Access | Only data explicitly permitted to role; read-only summary for release managers; no ability to click into protected case content. |

## 09  ADM-02 Handoff Queue — search, filters and SLA

| Control ID | Contract / behavior |
|---|---|
| Q01 Queues | My cases; Unassigned; Team; Escalated; Privacy/Security; Resolved. Role- and region-filtered on server. |
| Q02 Table | Case ID, category, priority, state, created_at, assigned_group, SLA due_at, consent state, channel, age. |
| Q03 Filters | Category, state, assigned group, urgency, submitted date, market when explicitly provided; pagination server-side. |
| Q04 Bulk controls | Only low-risk assignment / tagging as individually authorised; NO bulk close, PII exports or publication. |
| Q05 Empty/error | No cases = explicit empty state; 403 = denied; 429 = retry; incident fallback warns data may be stale. |
| Q06 SLA | Timers from accepted case creation according to support policy; stop/pause state policy documented, auditable and versioned. |
| Q07 Search | Search by opaque case number and safe metadata only; prevent free-form searches over hidden message bodies. |

## 10  ADM-03 Case Detail — privacy-aware operating wireframe

| Pane | Minimum content | Role/user action |
|---|---|---|
| Header | Opaque case ID, channel, ownership, priority, state, verified receipt, UTC timestamps and SLA. | Assign, escalate, resolve if permitted. |
| Summary | Visitor-selected category, intent ID, approved route key, optional market, originating page context. | Correct categorisation using reasoned edit. |
| Consent/evidence | Consent version, timestamp, secure-channel attestation, privacy notice, request source. | View status; not edit historical consent. |
| Communication | Only secure-channel messages collected after form consent and within authorised case scope. | Reply using templated channels; never display public-chat raw transcript by default. |
| Knowledge assistance | Approved articles and links matched by category, no model authority or auto-send. | Insert referenced public article into reply after review. |
| Audit / timeline | Receipt, assignment, escalations, due changes, reply and resolution with actor/time. | View evidence in role scope; immutable append-only. |
| Closure | Disposition taxonomy, customer-contact outcome and public FAQ improvement flag. | Resolve; do not assert service delivered without evidence. |

## 11  Public-to-human handoff: consent and evidence lifecycle

Figure 2 — End-to-end handoff without false ticket confirmation.

The default handoff mode is ROUTE_ONLY. An optional integrated case-creation mode may be activated only after the receiving service contract, customer disclosures, privacy basis, anti-abuse mechanisms and operational capacity are approved. The chatbot may never silently submit chat text, inferred identity or visitor contact details to a CRM/helpdesk.

| Transition | Source-of-truth event | User-visible statement |
|---|---|---|
| OFFERED → CLICKED | chat.handoff.clicked.v1 (Doc05), route validated at click time. | “Open secure support” (no case reference). |
| CLICKED → FORM_STARTED | Secure destination renders privacy notice and consent fields. | “You are leaving the assistant for secure support.” |
| FORM_STARTED → CASE_CREATED | Receivers return 201/accepted receipt + opaque case ID, idempotency key honored. | “Your request was received. Reference: [ID].” |
| CASE_CREATED → ASSIGNED | Receiving system emits verified assignment status or staff action. | Do not promise specific agent or response time unless live policy supports it. |
| CASE_CREATED → RESOLVED | Authorised human resolution with proof of contact/outcome. | Confirmation only in verified channel; AI does not close case. |

## 12  Handoff destinations, routing and unavailable paths

| Visitor reason | Verified ownership/destination | Data minimisation / failure |
|---|---|---|
| Enterprise sales / demo | Approved demo scheduler or Sales Ops secure form by route key. | Intent and optional business area; no booking until scheduler confirms. |
| Existing-customer support / login | Official secure support portal, login help or contact route; no account verification in public chat. | Collect identity on secure destination only. |
| Pricing/plan terms | Public pricing page; optional Commercial Ops enquiry if unavailable/complex. | Claim facts only if current signed P1 evidence exists. |
| Security / trust | Approved Trust Center/assurance request; authorised security team. | No unverified certification; redact attack details in public chat. |
| Privacy / rights | DPO/privacy channel with independent identity validation. | No transcript export or deletion claim in public chat. |
| Accessibility feedback | Approved accessibility contact route; ticket only if receiving endpoint confirms. | Capture page and issue description in secure route if approved. |
| Tax / payroll jurisdiction | Product specialist/Regional Legal secure enquiry where published. | No professional advice or inferred service availability. |
| Integration / migration | Solutions Architecture demo or secure enquiry. | Only voluntarily selected product category/region, no credentials. |

## 13  Case lifecycle, SLA policy and human response safety

| Case state | Permitted next state | Guard / evidence |
|---|---|---|
| NEW | TRIAGED; CANCELLED | Created only from confirmed secure intake. Required consent and classification. |
| TRIAGED | ASSIGNED; ESCALATED; CANCELLED | Queue ownership and due_at computed from policy. |
| ASSIGNED | WAITING_CUSTOMER; ESCALATED; RESOLVED | Agent scope, verified channel, substantive action recorded. |
| WAITING_CUSTOMER | ASSIGNED; RESOLVED | Reopening/resolution rules and SLA pause must follow published policy. |
| ESCALATED | ASSIGNED; RESOLVED | Supervisor or specialist acknowledgment; no dropped case. |
| RESOLVED | REOPENED; CLOSED | Disposition, optional QA review, reversible by authorised request. |
| REOPENED | ASSIGNED; ESCALATED | Preserve original case and full audit timeline. |
| CLOSED | REOPENED (policy permitting) | Retention and appeal rules; reopening does not erase closure event. |

SLA configuration must define calendar, timezone, holiday rules, business hours, priority, start/pause/stop and escalation. Do not insert invented response promises such as “within 2 hours” into public chat. Configure targets only after Support leadership approves staffing and service terms.

## 14  Handoff case data contract — minimum fields

| Field | Type / validation | Purpose / forbidden usage |
|---|---|---|
| case_id | Opaque ULID/UUID generated by case system | Public receipt reference; never tenant/account ID. |
| state, category, priority | Controlled enums, compatible status versions | Queue routing and evaluation; not LLM-defined. |
| source_session_ref | Optional irreversibly limited correlation reference | Not browser grant or raw full transcript; privacy-reviewed TTL. |
| origin_page, route_key | Allowlisted enums and approved routes | Navigation analytics; no arbitrary URL/referer. |
| intent_id, market_code | Allowlisted I01–I30 and optional explicit ISO country | Triage; neither field confirms entitlement. |
| consent_record | Version, timestamp, lawful basis/notice ref, channel evidence | Immutable intake attestation; no retroactive editing. |
| contact_ref | Opaque secure-channel contact reference | No contact details in public-ai analytics or cross-domain events. |
| assigned_team, assigned_staff | Scoped directory references | ABAC filter, assignment auditing. |
| created_at, due_at, updated_at | RFC 3339 UTC; SLA policy revision | Explain clock changes, evaluate fairness. |
| retention_class, hold_ref | DPO-approved policy key; optional legal-hold ref | No fixed retention number until approved. |

## 15  Knowledge operations console: inventory and governance

| Surface | Staff requirement / control |
|---|---|
| ADM-04 Source Library | Source/version list with owner, public classification, subject, market/locale, active release, effective/expiry, provenance status and current blockers. |
| ADM-05 Draft Editor | Edit only new revision; canonical ID mapping; citations/approved URLs; evidence attachments in staff-controlled storage, no direct content into production. |
| ADM-06 Review & Approvals | Two-panel diff and evidence; actor separation; approve/reject/changes requested with reason and hashed immutable record. |
| ADM-07 Release & Revocation | Compare signed release versions, KB dependency report, eval result, change ticket, deployment slot and live revocation controls. |
| ADM-08 Claims & Taxonomy | Read-only crosswalk of ten canonical modules and six public-facing domains; subject owner controls P1 claim approval, no duplicate taxonomy. |

## 16  Knowledge editorial workflow — controlled states

Figure 3 — Editorial maker-checker pipeline and instant block path.

| Status | Entry condition | Exit and rollback |
|---|---|---|
| DRAFT | Owner creates immutable revision from public admissible source candidate. | Submit to validation; may abandon draft, never mutate a released revision. |
| VALIDATION_FAILED | Parser/PII/taxonomy/unsafe URL/signature checks fail. | Return to draft with failure codes and evidence. |
| PENDING_REVIEW | All mechanical checks pass; reviewer distinct from editor. | Independent approval or changes requested. |
| APPROVED | Necessary subject owner approvals/evidence present. | Release manager signs manifest after staging/QA. |
| RELEASED | Signed public release active, applicable effective window. | Expire, supersede or revoke. No in-place edit. |
| SUSPENDED/REVOKED | Urgent policy, legal, privacy or trust concern; authoritative revocation epoch. | Deny retrieval immediately; new revision must re-run whole approval. |

## 17  Claim governance, temporal validity and taxonomy truth

Commercial and trust claims remain deterministic, typed records. The console must not let content editors replace, paraphrase or “fix” prices, trial duration, geographic eligibility, uptime terms or certification claims inside an ordinary FAQ article to bypass the P1 claim registry.

| High-risk field | Operational implementation |
|---|---|
| Claim identity | subject_type, claim_key, product_id, market, locale, plan, billing term, currency and applicability. |
| Authority | Signed claim revision and source evidence; designated subject owner; independent release authorisation. |
| Temporal | effective_from/until, review_due_at, expires_at, revocation epoch and time zone basis. |
| Contradiction | Block release if site copy, KB and claim registry disagree; show a conflict case with owner and resolution evidence. |
| Taxonomy | Canonical module ID is a foreign key; staff may add approved synonyms but never introduce undeclared public module categories. |
| Runtime | Claims resolved at response time, with emergency revocation checked before final answer commit. |
| Fallback | Safe abstention and verified sales/help link; never fill a missing value using a model guess. |

## 18  Review queue, approval rules and release controls

| Action | Mandatory precondition |
|---|---|
| Submit draft | Valid owner and purpose, source URL/hash, public scope and revision diff. |
| Routine article approval | Non-author reviewer; current public evidence; editorial QA and accessibility. |
| P1 approval | Subject owner + distinct approver; material evidence, market/effective date, finance/legal/security signoff depending on type. |
| Stage release | Clean PII scan, source classification, retrieval QA, citation resolver, prompt-injection corpus and taxonomy compatibility. |
| Promote release | Signed artifact, known app/policy/schema compatibility, Doc07 gate pass and change ticket; only designated release manager. |
| Emergency revoke | SecOps/content owner may block dangerous source/claim now; notify runtime, invalidate caches, audit, rollback to last safe snapshot. |
| Reinstate | Independent review and new signed release with incident closure; old revoked content never re-enabled by replay. |

## 19  Conversation quality, feedback and evaluation operations

| Capability | Data and operator control |
|---|---|
| Feedback ingest | Structured yes/no or bounded reason, consent-aware; do not automatically retain visitor prompt or identity. |
| Quality case | Aggregate failing intent, locale, model profile and response outcome; sampling must follow DPO-approved data policy. |
| Human annotation | Two-person blinded review for material-claim/safety cases; use synthetic/redacted examples by default. |
| Remediation queue | Link defect to source revision, approved prompt change or UI bug; owner and due date. |
| Drift reporting | Segment by module, market, intent, release tuple and reason; expose uncertainty and minimum sample. |
| Claims | P0 fabrications are emergency events, not low-priority “improvement opportunities”. |

## 20  Analytics dashboard — metric contracts and guardrails

| Metric | Definition / exclusions / next action |
|---|---|
| Eligible informative resolution | Grounded responses successfully answered ÷ eligible intents; abstentions shown separately, not hidden. |
| Verified case conversion | Confirmed secure-channel case receipts ÷ approved handoff starts, with suppression when correlation is unconsented. |
| Handoff offer / click rate | Offers and clicks are separate measures; zero implication of case creation. |
| Time to first human response | First authorised human message timestamp − case acceptance timestamp; separately segment working-hours policy. |
| P1 source violations | Count of expired, fabricated, contradictory or revoked P1 statements in independently sampled/pinned test cases; release-blocking. |
| Content freshness | Percent eligible sources with valid review/effective/expiry and retrieval success; status by owner/subject. |
| Case backlog / breach | Accepted open cases past approved SLA; show population and counting method. |
| Privacy/abuse | Coarse threat category and counts; avoid storing attack strings or visitor identifiers by default. |

## 21  Audit evidence, retention, deletion and export controls

Admin audit is independent of visitor transcript storage. Keep a privacy-safe, immutable activity ledger for privileged state changes; support-case records reside in the secure support service and follow approved retention and rights procedures. The administrative UI must not enable a broad raw-chat export.

| Event/action | Mandatory audit envelope |
|---|---|
| Case changed | actor_ref, case_ref, permitted_scope, old/new status, reason, change ID, UTC time, request_id. |
| Knowledge revised | source_version_id, prior hash, new hash, classification, owner, validation receipt, evidence refs. |
| P1 claim signed | claim ID/revision, independent approver IDs, authorisation grants, effective window and signature refs. |
| Release promoted | complete {app, policy, kb, claims, model_profile} tuple, evidence pack hash, rollout and approvers. |
| Access granted/removed | staff principal, role, scope, approver, expiry, just-in-time reason, prior grant hash. |
| Emergency intervention | incident ID, containment flag, epoch, actor, reason and second-person review outcome. |

| RETENTION CANNOT BE INVENTED The data retention schedule, audit-log access, law-enforcement process, subject-rights handling, processor/subprocessor terms, legal holds and international transfers are launch blockers until Privacy/Legal publish final values. UI copy must not imply “all data is erased” when different governed records exist. |
|---|

## 22  Configuration, feature controls and incident console

| Control | Safe implementation |
|---|---|
| Assistant read-only / disable | Explicit environment-specific switch with rationale, timestamp and authorised operator; safe public explanatory fallback. |
| Commercial claim quarantine | Block selected P1 subject/market/version immediately, invalidate cached results, reroute to safe non-specific response. |
| KB revocation | Source/version invalidation independent of background reindex; retrieval server evaluates active revocation epoch. |
| Release rollback | Restore last compatible signed tuple; do not restore revoked sources/claims or expired approvals. |
| Routing configuration | Changes limited to signed official HTTPS route registry; status/market checks; external destinations verified. |
| Model vendor degraded | Switch approved model profile or fail safely; no ungated direct provider calls or disclosure of internal endpoints. |
| Kill-switch controls | Two-person peer notice, audit evidence and incident linkage; read-plane fail-closed semantics. |

## 23  Proposed internal APIs and service ownership

| Private operation — proposed | Owner / contract boundary |
|---|---|
| GET /internal/admin/publicai/v1/overview | Admin BFF: role-filtered aggregates with release tuple and freshness. |
| GET /internal/admin/publicai/v1/cases | Support case adapter: pagination + ABAC; no public endpoint; fields minimised. |
| GET /internal/admin/publicai/v1/cases/{id} | Support case adapter: case detail with scoped secure-channel messages. |
| POST /internal/admin/publicai/v1/cases/{id}:assign | Case service: precondition If-Match; permission, reason and audit; conflict 409. |
| POST /internal/admin/publicai/v1/cases/{id}:transition | Case service: valid status machine and SLA policy; idempotency key. |
| GET /internal/admin/kb/v1/sources | Governance API: canonical registry, current revisions and validation/expiry. |
| POST /internal/kb/v1/sources/{id}/versions | Re-use Document05 proposed staff-governance operation; no duplicate public exposure. |
| POST /internal/kb/v1/releases/{id}:promote | Re-use Document05 private operation after signature + QA + release authorisation. |
| POST /internal/admin/kb/v1/sources/{id}:revoke | Governance plane: explicit signed revocation epoch and outbox event. |
| GET /internal/admin/publicai/v1/audit | Audit service: filtered immutable evidence, purpose-based export approval. |
| POST /internal/admin/publicai/v1/controls:apply | Ops control plane: scoped typed flag change; separate emergency/re-enable grants. |

## 24  Request contracts, errors and idempotent writes

All private APIs use existing organisation identity and permission standards once repository verified. Do not publish these endpoints through /public-ai/v1. Every write includes request_id, action_id, idempotency_key, expected_version or If-Match, documented body schema, actor scope and a durable audit/outbox result. Permission checks cannot be handled by frontend route guards alone.

| Failure | HTTP + UI behavior / audit |
|---|---|
| Unauthenticated/expired | 401; safe redirect to staff SSO; do not render cached restricted case. |
| Unauthorised scope | 403; no object existence leakage; audit denial class only. |
| Validation | 422 RFC 9457-style typed error + field path; never echo secrets. |
| Conflict / stale version | 409 or 412; show fresh diff and require user confirmation, never overwrite. |
| Duplicate action | 200/201 cached same outcome for idempotent retry; no duplicate case, approval or event. |
| Rate limit / outage | 429/503 + retry-after where appropriate; preserve draft locally only if privacy/security rules allow; never optimistic “published”. |
| Event backlog | Accepted write may show “pending delivery” with authoritative state; outbox retry, DLQ and incident workflow. |

## 25  Events and asynchronous guarantees — compatibility with Document 05

Preserve the six Document 05 event types exactly: chat.turn.completed.v1, chat.handoff.clicked.v1, kb.release.promoted.v1, kb.source.revoked.v1, publicai.kill_switch.changed.v1 and chat.security.signal.v1. Staff case creation/assignment events should remain internal to the receiving case domain unless approved as new versioned schemas; never silently overload chat.handoff.clicked.v1 to mean case creation.

| Event class | Consumer guarantee and privacy requirement |
|---|---|
| Handoff clicked | Anonymous analytics only; cannot create cases, imply receipt, or launch unsolicited outreach. |
| KB released/revoked | Signed monotonic release/epoch, reject out-of-order replay; runtime and CDN invalidation receipts. |
| Kill-switch | Authorised control-plane producer; event alone not source of truth, subscribers verify latest signed state. |
| Security signal | Minimal reason code and request ref, no attack string. Routed to SecOps. |
| Case-domain events — separate proposal | case.created.v1 and case.updated.v1 only after receiving service owners approve AsyncAPI schemas, identity and lawful basis. |

## 26  Workflow failure cases and compensating actions

| Failure / adversarial condition | System response / responsible actor |
|---|---|
| Visitor abandons secure form | Record only voluntary click under consent policy; no case and no “agent alerted” claim. |
| Form creates case but browser times out | Idempotent query/receipt recovery via secure channel; never create a second case without proof. |
| Agent loses assignment rights | Reject next case read/reply; clear cached case and warn; IAM audit. |
| Editor approves own price claim | Deny regardless of multiple roles; log policy rejection and independent reviewer requirement. |
| KB review passed then source revoked | Prevent promotion, revoke cached read access, invalidate affected claim answers. |
| Wrong region or market inferred | Ignore derived entitlement; answer only with explicitly applicable public evidence. |
| Model-generated poisoned article | Treat generated text as untrusted draft, inspect evidence; no automatic approval. |
| Ticket queue unavailable | Show official general support destination; avoid promise and retain no contact details in assistant. |
| Bulk exported case list | Only DPO- and scope-approved request, watermark evidence, expiration, rate and audit. |
| Release manager attempts re-enable revoked source | Requires new signed source revision and review, never one-click epoch rollback. |

## 27  Admin console detailed state design and copy

| State / screen | Required behavior / user message |
|---|---|
| Loading | Skeleton only for coarse non-sensitive data; progressive announcement and cancel navigations. |
| No permission | “You do not have access to this work item.” No inferred resource attributes. |
| No cases | “No cases match these filters.” Reset filters and view my queue. |
| No knowledge evidence | “This source needs verified evidence before it can be reviewed.” |
| Pending approval | “Pending independent review. This change is not live.” |
| Submitted but pending commit | “Change submitted for processing. Check its status before retrying.” |
| Published | “Release [opaque ID] is active.” Only after verified signed promotion receipt. |
| Revoked | “Source [ID] has been suspended from public retrieval.” Include committed epoch and incident ref. |
| Error | “The action could not be completed. No change was confirmed.” With request ID. |
| Case handoff | “Open secure support” before case; after receipt, “Request received — reference [ID].” |

## 28  Implementation work packages and sequencing

| Package / owners | Dependencies and deliverable | Exit evidence |
|---|---|---|
| WP-01 / Product + IAM | Staff navigation, role registry, access provisioning, ABAC policy in API. | Cross-role deny tests + 2-person controls. |
| WP-02 / Support + Backend | Receiving helpdesk inventory, verified secure handoff and case adapter. | Route-only + case receipt test, no false confirmation. |
| WP-03 / Knowledge Eng | Source inventory, draft/review/change set, source revocation. | Signed release and immediate revoke test. |
| WP-04 / Commercial + Legal | P1 claim approval, applicability and taxonomy reconciliation. | Two-person evidence and effective-dated test. |
| WP-05 / Ops + SecOps | Incident panel, kill switch, audit evidence and environment banner. | Exercise of fail-closed suspension and re-enable. |
| WP-06 / Product/QA | UX accessibility, quality analytics, feedback loop and test fixtures. | WCAG AA manual reviews and quality golden set. |
| WP-07 / SRE + DPO | Retention, monitoring, real SLAs, DR and release approval. | Runbooks with signed dry-run receipts. |

## 29  Acceptance test catalogue — 60 executable scenarios

### Identity & RBAC  /  6 tests

| Test ID | Stimulus / executable assertion |
|---|---|
| ADM-T001 | Agent sees only permitted assigned/team cases; cross-team ID returns 403. |
| ADM-T002 | KB editor cannot approve or promote own content using combined roles. |
| ADM-T003 | Revoked/expired staff grant blocks read and write immediately after token refresh/revocation. |
| ADM-T004 | Unauthorised direct API access rejected even when UI route hidden. |
| ADM-T005 | Privilege elevation requires approver and short-lived grant with audit. |
| ADM-T006 | Audit export denied without purpose-bound scope and approval. |

### Handoff & case receipts  /  6 tests

| Test ID | Stimulus / executable assertion |
|---|---|
| ADM-T007 | Link click alone does not create support case or show receipt. |
| ADM-T008 | Confirmed secure form response creates exactly one idempotent case and reference. |
| ADM-T009 | Abandoned form produces zero cases and zero callback promises. |
| ADM-T010 | Receiving system outage returns safe fallback with no false confirmation. |
| ADM-T011 | No raw conversation text or passwords travel in default handoff payload. |
| ADM-T012 | Case ID and messages inaccessible from wrong queue/region/role. |

### Case lifecycle & SLA  /  6 tests

| Test ID | Stimulus / executable assertion |
|---|---|
| ADM-T013 | Invalid state transition returns 409/422, retains original version. |
| ADM-T014 | Concurrent assignment requests honour If-Match and reject lost update. |
| ADM-T015 | SLA timer begins only on accepted case creation using approved calendar. |
| ADM-T016 | Pause and resume transitions preserve evidence and recompute deadlines. |
| ADM-T017 | Reopened case retains original case ID and all timeline events. |
| ADM-T018 | Agent reply not sent to wrong or unverified secure channel. |

### Knowledge editorial  /  6 tests

| Test ID | Stimulus / executable assertion |
|---|---|
| ADM-T019 | Draft is immutable after release and any edit creates new revision. |
| ADM-T020 | PII or private classification fails PUBLIC_APPROVED publishing gate. |
| ADM-T021 | Missing evidence/hash/owner prevents review and release. |
| ADM-T022 | Independent approver rejects own authored source revision. |
| ADM-T023 | Contradictory source cannot silently overwrite active truth. |
| ADM-T024 | Revoked source stops retrieval before background index purge. |

### Claims & releases  /  6 tests

| Test ID | Stimulus / executable assertion |
|---|---|
| ADM-T025 | P1 claim requires independent specialist signoff and signed manifest. |
| ADM-T026 | Expired market/plan claim cannot pass release and is never used in answers. |
| ADM-T027 | Canonical capability ID validated against approved ten-module registry. |
| ADM-T028 | Failed QA prohibits KB or P1 claim promotion. |
| ADM-T029 | Rollback preserves emergency revocation epoch and blocks outdated claim. |
| ADM-T030 | Production promotion records complete compatible release tuple. |

### Privacy & content safety  /  6 tests

| Test ID | Stimulus / executable assertion |
|---|---|
| ADM-T031 | No direct access path from staff console to private finance/HR/payroll data. |
| ADM-T032 | Support case PII is redacted from analytics, traces and LLM prompts. |
| ADM-T033 | Attachment scanning/quarantine works or uploads disabled by policy. |
| ADM-T034 | Rights request identity verification occurs only in secure approved channel. |
| ADM-T035 | Privacy export/deletion honours DPO-approved retention + legal hold states. |
| ADM-T036 | Prompt injection in source draft cannot alter approval or publish role. |

### UI & accessibility  /  6 tests

| Test ID | Stimulus / executable assertion |
|---|---|
| ADM-T037 | All 12 screens keyboard-operable in representative user journeys. |
| ADM-T038 | 400% zoom and 320px width avoid clipping or inaccessible commands. |
| ADM-T039 | All status/error messages announced per WCAG 4.1.3 as applicable. |
| ADM-T040 | SLA urgency text never relies solely on color. |
| ADM-T041 | Destructive confirmation returns focus safely and supports cancel. |
| ADM-T042 | Role-filtered empty/loading/error screens expose no restricted data. |

### Incidents & resilience  /  6 tests

| Test ID | Stimulus / executable assertion |
|---|---|
| ADM-T043 | Authorized source quarantine reaches read plane promptly under outage. |
| ADM-T044 | Kill switch change blocked from public origin and produces signed audit event. |
| ADM-T045 | Out-of-order control event cannot re-enable older release. |
| ADM-T046 | Re-enable requires independent review after emergency suspension. |
| ADM-T047 | DLQ replay dedupes and does not resurrect deleted/revoked content. |
| ADM-T048 | Staff app displays stale-data warning on API outage and does not claim saved changes. |

### Quality & analytics  /  6 tests

| Test ID | Stimulus / executable assertion |
|---|---|
| ADM-T049 | Handoff offer, click, case receipt and response measures remain separate. |
| ADM-T050 | Analytics exclude raw message bodies and unconsented identifiers. |
| ADM-T051 | Reported answer rate counts abstentions separately with valid denominators. |
| ADM-T052 | Sampling/redaction QA is role-scoped and documented. |
| ADM-T053 | P1 claim failure triggers P0 incident regardless of aggregate quality score. |
| ADM-T054 | Aggregate metrics link to release tuple and time window, with delayed events flagged. |

### Audit & governance  /  6 tests

| Test ID | Stimulus / executable assertion |
|---|---|
| ADM-T055 | Every state change links actor, approval, before/after, hash and UTC time. |
| ADM-T056 | Admin cannot delete or rewrite immutable audit rows. |
| ADM-T057 | Staff access quarterly review generates signed attestations; cadence proposed. |
| ADM-T058 | Unavailable approval basis leaves publication pending, never auto-approved. |
| ADM-T059 | Required retention owner + matrix exist before production launch. |
| ADM-T060 | Release gate includes security, support, QA, DPO and product owners or documented hold. |

## 30  Test fixtures and contract-level verification

| Fixture class | Examples / mandatory oracle |
|---|---|
| Persona matrix | 10 roles, no role, expired role, stacked roles, elevated emergency, cross-region viewer. |
| Case matrix | New, assigned, waiting, escalated, resolved, reopened; verified and unverified consent; stale version. |
| Knowledge matrix | Public clean, mixed/private, P1 signed/unsigned, future-effective, expired, revoked, contradictory. |
| Failure matrix | Helpdesk 503, delayed 201, lost receipt, network retry, DLQ replay, invalid signature, model unavailable. |
| Accessibility | VoiceOver/NVDA/TalkBack, keyboard only, 320px/400% zoom, high contrast, reduced motion. |
| Performance | Queue pagination by role, bounded read query, publish under load, revocation propagation measured. |

Every test stores build SHA, release tuple, anonymised fixture ID, expected/actual response, owner, reviewer, UTC timestamp and evidence artifact. Negative permission tests are release-blocking; aggregate test passing does not override a single P0.

## 31  Traceability to predecessor controls and audit evidence

| Controlling spec | New verification binding |
|---|---|
| Doc 01 — Product/UX | No extra public account functionality; staff console remains separate and responsive. |
| Doc 02 — Intents/Handoff | I24 human request and C09 handoff; copy and route behaviour do not imply accepted case. |
| Doc 03 — KB | Source registry, PUBLIC_APPROVED, P1 approvals, release signature, contradiction and revocation. |
| Doc 04 — Architecture | Hard network/data segregation; no customer ledger/HR/payroll endpoints. |
| Doc 05 — APIs/Events | Six established events; route_key, typed outcomes, idempotency, CloudEvents/AsyncAPI extension policy. |
| Doc 06 — Security/Privacy | DPO gate, no raw transcript default, ABAC, secure consent and audit. |
| Doc 07 — QA | Risk-weighted golden corpus, non-waivable P0 failures, release evidence. |
| Doc 08 — Deployment | Four separately signed artifact categories, revoke/kill-switch, compatible tuple and rollback. |

## 32  Open decisions / owner register — required before build or launch

| ID | Decision required | Accountable owner | Gate |
|---|---|---|---|
| OD-01 | Existing service desk vendor/API, supported case schema and webhook capability | Support Lead + Backend | Integration build |
| OD-02 | Route-only vs integrated case creation for launch and eligible markets | Product + Privacy | Public handoff build |
| OD-03 | Approved support staffing, queues, response SLAs, holidays/timezones | Support + Commercial | Case workflow go-live |
| OD-04 | Identity/SSO/MFA, directory IDs, role federation and admin origin | IAM + Security | Admin build |
| OD-05 | Approved public KB editor/corpus provider and asset storage | Knowledge Engineering | Content operations |
| OD-06 | Named P1 subject owners and binding legal/finance/security approvals | Commercial + Legal + Security | P1 release |
| OD-07 | Exact taxonomy mapping and service catalogue as-built check | Product Architecture | Knowledge publication |
| OD-08 | DPO privacy basis, consent scope, transcript policy and retention matrix | DPO + Legal | Any personal data transfer |
| OD-09 | Audit store, legal hold, export process and regional data plane | Platform + DPO | Operations go-live |
| OD-10 | Source revalidation cadence and conflict resolution owners | Content Ops + Product | KB release |
| OD-11 | Operational kill-switch grants and reversible incident approvals | SecOps + SRE | Production safety gate |
| OD-12 | Environment, API gateway and case adapter names in actual repo | Global CTO + Backend | Contract freeze |
| OD-13 | Approved brand assets / assistant public display clearance | Brand + Legal | Public deployment |
| OD-14 | Final supported locales, accessibility testing owners, translations | Product + Accessibility | Launch |

## 33  Critical reassessment and recommendations incorporated

| Review finding | Resolved by this version / remaining reservation |
|---|---|
| Admin console risked too much dashboard surface | 12 task-centred screens grouped in 4 navigation areas, not 12 unrelated large dashboards; avoid menu clutter. |
| Support workflow ambiguously coupled to anonymous chat | Route-only default, secure receiving system as source of truth, case-creation receipt contract and no raw transcript by default. |
| Content editors could bypass high-impact claims controls | P1 typed claim registry, two-person reviewer separation and signed manifest; ordinary FAQ cannot override. |
| Incidents could be reversed by stale event replay | Latest signed revocation epoch and denied re-enable until independent review. |
| Role stacking accidentally grants self-publishing | Attribute-based SoD check uses actor equality and action history rather than only role count. |
| Analytics conflated click with resolution | Four distinct steps for offer, click, accepted case and human response; privacy-safe denominators. |
| A public chatbot may be mistaken for authenticated assistance | UI and API segregation; staff console explicitly NOT a tenant dashboard or financial workspace. |
| Open details should not be silently invented | Fourteen decision gates and actual service-inventory task retained; proposed targets marked unverified. |

## 34  Implementation release gates and sign-off record

| Gate | Evidence required | Sign-off |
|---|---|---|
| G01 / Governance | Screen IA, roles, destinations, handoff mode and legal copy approved. | Founder / Product / Legal |
| G02 / Data & privacy | DPIA decision, vendor terms, retention, consent and regional controls documented. | Security / DPO / Legal |
| G03 / Contracts | Service endpoints verified against repo; case adapter, auth, KB and event schema contract tests passing. | Backend / IAM / Support |
| G04 / QA & accessibility | ADM-T001–T060, Doc07 regression, WCAG 2.2 AA manual and automated evidence. | QA / Accessibility |
| G05 / Release | Signed release tuple, outbox/kill-switch drills, monitoring, rollback and staffing acceptance. | SRE / SecOps / Operations |
| G06 / Product launch | Public brand clearance, route destinations, truthful copy and final commercial claim check. | Founder / Product / Brand |

| NO IMPLICIT PRODUCTION AUTHORISATION Technical approval of this document is conditional. Release occurs only after all applicable gates are evidenced. A missing service desk, undeclared SSO, unapproved retention or unavailable approver means disable that corresponding feature, not silently relax the safety boundary. |
|---|

## 35  Appendix A — illustrative typed case-creation receipt schema

Case creation receipt — submitted by SECURE destination, not the anonymous assistant

{  "$schema": "https://json-schema.org/draft/2020-12/schema",  "type": "object",  "required": [    "case_id",    "state",    "created_at",    "channel",    "consent_record_ref"  ],  "properties": {    "case_id": {      "type": "string",      "minLength": 12,      "maxLength": 128    },    "state": {      "const": "NEW"    },    "created_at": {      "type": "string",      "format": "date-time"    },    "channel": {      "enum": [        "SECURE_WEB_FORM",        "VERIFIED_SUPPORT_PORTAL"      ]    },    "consent_record_ref": {      "type": "string",      "minLength": 8    },    "request_id": {      "type": "string"    },    "next_step_route_key": {      "type": "string"    }  },  "additionalProperties": false}

Creation is accepted only when the secure receiving service confirms server-side persistence. Schema fields are illustrative until the helpdesk contract is authenticated and owner-approved.

## 36  Appendix B — example staff policy and source change

Policy-as-code rule — pseudocode, not deployed IAM syntax

permit(action, actor, object, context) when  actor.authenticated && actor.grant.active &&  actor.grant.environment == context.environment &&  scope_allowed(actor, object) &&  action in actor.grant.permissions &&  !(action in {APPROVE_SOURCE, APPROVE_P1} && object.author_id == actor.id) &&  !(action == PROMOTE_RELEASE && !signed_evidence_passed(object))else DENY; audit(opaque_request_id, actor_ref, resource_ref, reason_code).

Illustrative source review request

{  "source_version_id": "srcv_example-001",  "action": "REQUEST_REVIEW",  "classification": "PUBLIC_APPROVED",  "subject_type": "CAPABILITY",  "market_codes": [    "US"  ],  "owner_ref": "staff_opaque_A",  "evidence_ref": "evidence_approved_01",  "content_sha256": "sha256:<digest>",  "expected_source_revision": 3}

## 37  Appendix C — human handoff and case-state wire contracts

Route-only response envelope, compatible with Document 05 patterns

{  "type": "HANDOFF_OFFER",  "assistant_id": "zoikosuite-public-ai",  "intent_id": "I24",  "action": {    "kind": "OPEN_APPROVED_ROUTE",    "route_key": "OFFICIAL_SUPPORT_PORTAL"  },  "case_created": false,  "text": "You can contact our support team through the secure support page.",  "public_claims": []}

Illustrative status transition contract — admin only

{  "case_id": "HC-example-1043",  "expected_version": 5,  "transition": "ASSIGNED",  "assigned_team": "zoikosuite-support",  "reason_code": "TRIAGE_COMPLETE",  "change_ticket_ref": "ops-example-2026",  "idempotency_key": "<client-generated-opaque-key>"}

## 38  Appendix D — standard references and source governance

| Reference | Purpose and application |
|---|---|
| NIST AI RMF and NIST AI 600-1 | Govern, map, measure and manage generative-AI risks; human oversight, monitoring and documentation. https://www.nist.gov/itl/ai-risk-management-framework |
| OWASP LLM Top 10 2025 | Prompt injection, data disclosure, excessive agency, vector/retrieval risks and abuse. https://genai.owasp.org/llm-top-10/ |
| W3C WCAG 2.2 | Accessible admin forms, status updates, keyboard interfaces and responsive layouts. https://www.w3.org/TR/WCAG22/ |
| Atlassian Jira Service Management documentation | Reference pattern for work queues and SLA timers, not a selected ZoikoSuite service-desk vendor. https://support.atlassian.com/jira-service-management-cloud/ |
| Zendesk knowledge and handoff guidance | Reference pattern for focused AI-ready articles and handoff distinction, not evidence of an installed integration. https://support.zendesk.com/ |
| ZoikoSuite Suivo Documents 01–08 | Internal controlled references physically supplied with this engineering series; check current approved versions before development. |

Sources checked 24 October 2026. A competitor/vendor workflow is an external design reference only; it is not a representation that ZoikoSuite operates the same services. Final implementations must be reviewed by engineering, Security, Privacy and Product before launch.

| FOUNDER APPROVAL REQUESTED — DOCUMENT 09 ONLY Approve, request changes to, or reject this one controlled document. If approved, engineering may validate and implement its bounded staff-console work packages subject to named release gates; the next documentation item remains withheld until separate authorisation. |
|---|
