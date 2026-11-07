<!-- Extracted from ZoikoSuite_Suivo_Chatbot_Document_08_Deployment_DevOps_Observability_and_Production_Readiness_Playbook.docx -->

ZOIKO GROUP  /  ZOIKOTECH INC.  /  ZOIKOSUITE

SUIVO  /  PUBLIC AI CHATBOT

Deployment, DevOps, Observability &Production Readiness Playbook

ENGINEERING DOCUMENT 08    |    VERSION 1.0    |    24 OCTOBER 2026

Controlled end-to-end operating model for deploying and maintaining the ZoikoSuite public chatbot: environment baselines, supply-chain integrity, independent artifact promotion, progressive delivery, SLOs and burn-rate alerts, privacy-preserving observability, model and knowledge release operations, incident command, disaster recovery, cost management, and signed go-live evidence.

| DOCUMENT STATUS PROPOSED FOR FOUNDER / GLOBAL CTO / PLATFORM-SRE APPROVAL. This is an implementation and operations specification—not evidence that Kubernetes, a particular cloud, an APM vendor, a model vendor, or specific SLOs are already deployed. Existing ZoikoSuite repositories and infrastructure must be inspected before adopting the illustrative designs. |
|---|

| Control | Decision / required record |
|---|---|
| ID and classification | ZS-SUIVO-ENG-008  /  v1.0  /  24 October 2026  /  Internal engineering / confidential |
| Release scope | Public, anonymous, information-only Suivo assistant; official CTAs; authoritative public knowledge only. |
| Document lineage | 01 Product/Wireframes; 02 Conversation/Prompts; 03 KB; 04 Architecture; 05 APIs/Events; 06 Security/Privacy; 07 QA/Acceptance. |
| Working name | Suivo; brand/legal clearance and approved logo/favicon are separate sign-off requirements. |
| Permanent assistant key | zoikosuite-public-ai; do not couple runtime identifiers to changeable display branding. |
| Ownership | Global CTO • Platform/SRE • AI/ML • API/Backend • Web • Knowledge Ops • Security/DPO • QA • Product/Commercial. |
| Evidence limitation | Actual code, IaC, diagrams, deployed services, cloud configuration and measured load/recovery data have not been independently verified. |

| SUBMISSION AND STOPPING RULE Approve Document 08 as the proposed operating baseline, assign owners and verify preconditions. Approval of this file is NOT permission to activate the production chatbot; all release gates and signed evidence remain mandatory. Only proceed to the next document after separate approval. |
|---|

## 01  Executive operational mandate and critical decisions

Suivo is a public guidance surface for ZoikoSuite, not a privileged financial assistant. Operate it as a least-privilege internet-facing service with deterministic authority for commercial facts and knowledge permissions. The model generates wording; it does not authorize tool calls, change claims, publish knowledge or choose network routes. Continuous operations must preserve that separation during deployment, outage, rollback and emergency intervention.

| Operational design decision | Required implementation consequence |
|---|---|
| Four independently governed artifacts | Deploy and roll back (1) binary/container, (2) policy/prompt bundle, (3) PUBLIC_APPROVED KB release, and (4) P1 commercial-claim registry with independent immutable versions. |
| Public/private segregation | No production service account, NAT egress, secret, tool or network route into tenant finance, HR, payroll, tax, bank or privileged platform services. |
| Fail-closed commercial truth | If a signed effective claim cannot be verified, the assistant abstains from price/eligibility/security/certification specifics; it never relies on remembered LLM wording. |
| Human accountable release | Separation of duties, named change owner, measured gates and documented rollback authority; unattended model or content self-promotion prohibited. |
| Distinct health and safety signals | A 200 response or fallback message is not by itself proof of factual availability, accessibility, or answer quality. |

## 02  Controlled authority, precedence and implementation caveats

| Predecessor / current authority | Operations control preserved |
|---|---|
| Documents 01–02 | Web launcher, typed dialogue states, 30 intent families, accessible escalation and safe refusal scripts. |
| Document 03 | PUBLIC_APPROVED corpus, signing, P1/P2/P3 hierarchy, source expiration, claim revocation and provenance. |
| Document 04 | Public-only containers, denied private business interfaces, independent deterministic policy, p95 latency proposal. |
| Document 05 | 13 proposed APIs, six event types, exactly-one active turn/session, typed outcomes, idempotent retries, safe DLQ. |
| Document 06 | 60 security tests, vendor/retention approvals, redaction, kill switch, incident notice decision gates. |
| Document 07 | 84 new QA cases in 12 families, non-waivable P0 blockers, staged release and acceptance evidence. |
| Production repo / IaC | Authoritative for what actually exists after CTO/SRE inventory; no assumed cloud, scheduler or vendor. |

Resolution rule: approved security/privacy restrictions and source/claim integrity outrank convenience and infrastructure availability. In any conflict between sample manifest settings and the deployed stack, record an ADR and adapt the implementation without weakening those invariants.

## 03  Day-zero discovery and environment verification

| Discovery item | Evidence required / pass criterion | Owner |
|---|---|---|
| Repo and build | Source repos, protected branches, dependency lockfiles, artifact/CI provenance, code owners. | Platform lead |
| Hosting / traffic | DNS, CDN, WAF, ingress, certificates, routing, load balancers, support CTA URLs. | Web/SRE |
| Compute and data | Cloud, runtime/scheduler, store, cache, network egress, secret management, queue, backups. | SRE |
| LLM and retrieval | Approved vendor contract, model identifier, embedding/index version, privacy settings, rate limits. | AI/AppSec |
| Commercial truth | Signed price/plan/market/feature/safety assurance registries; expiry and revocation API. | Product/KB Ops |
| Observability | Existing traces/metrics/logs, alert routing, on-call, consent and log-retention records. | SRE/DPO |
| Privacy / compliance | DPIA decision, processor terms, lawfulness, region/transfer, erasure and cookie stance. | DPO/Legal |
| Existing readiness | Measured traffic/load, incident history, 07 release evidence, recent pentest and availability. | QA/SRE |

| NO INFERRED PRODUCTION STATUS The references in this playbook to Kubernetes, GitOps, OTEL and example numeric gates are reference patterns. Engineering must confirm the operational platform, adapt documented controls to that platform, and capture a signed baseline inventory before a deployment ticket may be opened. |
|---|

## 04  Target release control plane and zone layout

Figure 1 — Proposed delivery, security and operational trust zones (implementation-independent control requirements).

| Zone | Permitted interfaces | Prohibited interfaces |
|---|---|---|
| Browser / edge | HTTPS first-party widget, public chat routes, signed public config, WAF and rate limit. | Sensitive cookies or arbitrary model URLs; customer sessions. |
| Public orchestration | Verified knowledge and signed claim reads; scoped model gateway; safe route registry. | Private ZoikoSuite microservices, operational action tools or tenant credentials. |
| Content control plane | Staff SSO + MFA, approval workflows, signed publish, immutable release ledger. | Unapproved import to runtime; model-initiated publish. |
| SRE control plane | CI/GitOps, segmented metrics, secrets, audit events, killswitch with dual control. | Persistent raw-user-message observability by default. |

## 05  Environment topology and promotion boundaries

| Environment | Purpose / data rule | Promotion gate |
|---|---|---|
| Local / CI | Synthetic fixtures only; mock model/provider credentials; ephemeral stores; no production secrets. | Unit, lint, policy-as-code, SBOM and contract checks. |
| Integration / dev | Deterministic mock services and feature flags; tagged synthetic content only. | API/event and idempotency assertions. |
| Staging / preprod | Production-equivalent edge/policy/runtime; synthetic test corpora, independent keys and restricted egress. | Security, 84-case quality regression, WCAG manual review, load and failover rehearsal. |
| Canary / production | Measured 1%, 10%, 50%, 100% gates (proposal), stable + candidate snapshots, isolated public grants. | Human sign-off, health/error-budget checks and instant abort. |
| Recovery / DR | Restore immutable releases and minimal approved mutable metadata. No raw session transcript resurrection. | Proof of provenance, restore drill and post-restore P0 tests. |

Staging must never be quietly granted access to production customer records. If production-like data are unavoidable for a different future workstream, require an independently approved privacy and access scope before any test data movement.

## 06  Four-artifact release manifest and atomic compatibility

| Artifact kind | Immutable identifier | Acceptance and rollback mechanism |
|---|---|---|
| Application | Container digest / build SHA | Signed SBOM, vulnerable-dependency policy; deploy prior known-good digest. |
| Prompt + policy | Signed prompt/policy release ID | Policy SHA, prompt hash, model compatibility/eval receipts; revert policy pointer. |
| Public knowledge | Signed KB manifest + source/chunk hashes | PUBLIC_APPROVED allowlist, publication/expiry; revoke then re-point index/release. |
| Commercial P1 claims | Effective-dated signed claim manifest | Market/time/plan/feature proofs, atomic revocation and fallback; never stale claims. |

| ATOMIC COMPATIBILITY INVARIANT Every served response must record a coherent release_tuple {app, policy, kb, claim_registry, model_profile}. An answer cannot combine a future policy with an incompatible schema or use a claim revoked after its retrieval. Pin the tuple at turn start, validate claim revocation at final response commit, and invalidate inflight turns when an emergency revocation occurs. |
|---|

Example: release record — illustrative (not a deployed format)

{  "release_id": "suivo-prod-YYYYMMDD-NN",  "git_commit": "<sha>",  "container_digest": "sha256:<digest>",  "prompt_policy_release": "pp-<signed-version>",  "public_kb_release": "kb-<signed-version>",  "p1_claims_release": "claims-<signed-version>",  "model_profile": "model-approved-<version>",  "schema_contract": "suivo-api-v1",  "rollout_stage": "canary-1pct",  "approvals": [    "release_owner",    "qa",    "security",    "sre"  ],  "manifest_signature": "<verify-before-apply>"}

## 07  Build and software-supply-chain controls

| Pipeline guard | Required evidence / negative test |
|---|---|
| Source review | Protected branch, peer review, CODEOWNERS, ticket mapping, privileged change separation. |
| Reproducibility | Locked dependencies, hermetic/isolated builder where practical, pinned toolchain and digest. |
| Scanning | SAST, dependency audit, container scan, secrets detection and license review; critical findings block. |
| Provenance | Signed build provenance and SBOM; release controller verifies trusted builder and artifact digest. |
| Deployment attestations | Artifact registry signature, policy bundle signature, KB release signature and claim-signature check. |
| Environment security | Short-lived workload identity, just-in-time deploy permissions, encrypted secrets and no static tokens in YAML. |
| Post-release accountability | Immutable change record links code, model/policy/KB versions and incident rollback pointer. |

Align with NIST SP 800-218 (final SSDF v1.1) and SLSA v1.2 build provenance. These are engineering reference standards; this document does not assert independent certification or attainment of a particular SLSA level.

## 08  CI/CD pipeline: mandatory jobs and approval gates

| Stage | Automated or manual gate | Stop condition |
|---|---|---|
| PR / merge | Lint, typecheck, dependency/security scans, secrets, contracts, policy, unit checks. | Any P0, fail or unreviewed bypass. |
| Candidate artifact | Build immutable digest, sign, create SBOM/provenance, record config compatibility. | Invalid signature or missing provenance. |
| Staging deploy | Promotion only of verified digests; canonical data sets and no private data routes. | Any private service reachable. |
| QA/AI eval | Doc07 golden and holdout suites, risk-segment rubrics, high-impact commercial claims. | Non-waivable P0 or failed owner threshold. |
| Preflight SRE | Restore and kill-switch, probes, resource quotas, alert pipeline and synthetic journeys. | No rollback owner or no live alert. |
| Canary ramp | Compare to baseline and SLO burns, P0 dashboards, manual gate on each increment. | Any invariant failure or invalid metric denominator. |
| Postrelease | 24h heightened watch (proposed), annotate traces, close evidence register. | Material regression triggers revert or safe static mode. |

## 09  Delivery strategy: progressive canary and reversible releases

Figure 2 — Proposed deployment ramp, human oversight and automatic stop conditions.

Canary percentages are suggested operating stages, not an implicit instruction to use a user-level randomized experiment with personal data. Sticky routing is derived from a privacy-approved non-identifying session bucket; routing must not leak state across policy versions.

| Ramp | Minimum observation / decision | Owner |
|---|---|---|
| 0% (dark launch) | Synthetic endpoint and health probes, zero unexpected egress; signed approval. | SRE/AppSec |
| 1% | At least one observation window and minimum sample size agreed from traffic; compare p95, errors, claim P0 signals. | SRE+QA |
| 10% | Check event DLQ, abstain/fallback, citation validity, origin-specific errors and model token cost. | SRE+AI |
| 50% | Inspect segment/locale/device health, multiwindow burns and approved commercial journeys. | SRE+Product |
| 100% | Only on signed evidence; 24h heightened watch, no alert silencing. | Release manager |

| LOW-TRAFFIC CANARY DISCIPLINE Percentages alone do not prove statistical safety. If traffic is too low to establish meaningful outcome confidence, do not auto-promote; extend observation or use synthetic probes and explicit human risk acceptance, without waiving P0 invariants. |
|---|

## 10  Health checks, startup and safe dependency semantics

| Endpoint / check | What it validates | What it must not do |
|---|---|---|
| GET /health/live (illustrative) | Process is responsive and event loop/worker is not terminally wedged. | Call third-party LLM or query commercial database, causing restart storms. |
| GET /health/ready | Can accept a public turn with required local policy/config validation and minimal service dependencies. | Report ready if signed safety config is absent or active revocation cannot be enforced. |
| GET /health/startup | Startup initialization, cache warmup and signature verification completed. | Assume a long model vendor call must finish before app can launch. |
| Synthetic /canary-journey (restricted) | Actual outside-in safe CTA, knowledge citation, abstain, and fallback exercise. | Use real customer financial records or index unpublished sources. |

When backed by Kubernetes, use separate startup/readiness/liveness semantics and calibrate probe thresholds. Readiness removes unhealthy instances from service; liveness should not restart healthy processes solely because a third-party model is slow. Health endpoint paths here are proposed until service contracts are approved.

## 11  Runtime resources, isolation and scaling controls

| Concern | Proposed operational guardrail |
|---|---|
| Workload identity | Workload-scoped short-lived identity; secrets from central manager; egress allowlist for approved provider and public source registry. |
| Capacity | Horizontal scale by real request rate/CPU/concurrency; per-pod concurrency bound and global model spend circuit breaker. |
| Resource controls | Requests/limits, node/zone spread when supported, disruption budgets for multiple replicas, controlled graceful shutdown. |
| One-active-turn invariant | Concurrency lock stored in shared service; duplication is impossible across replicas; lock releases on timeout/disconnect. |
| Backpressure | Bounded queue; 429 with retry guidance on quota; 503 safe fallback on dependency failure; no infinite retries. |
| Browser isolation | CSP and allowed origins, no arbitrary client-selected LLM provider or endpoint, secure TLS. |

## 12  SLO taxonomy, indicators and proposed baselines

Separate user-experience success from technical safety and from factual correctness. Defining availability as “any HTTP 200, including a refusal or fallback” risks concealing an inaccessible or commercially unusable chatbot.

| Indicator | Proposed target / rule | Measurement / exception |
|---|---|---|
| Safe API responsiveness | 99.9% monthly requests receive a correctly typed outcome within service deadline. | Exclude intentional abuse-block 429 from availability numerator and denominator only by approved metric policy; count unknown/timeout as bad. |
| Useful grounded resolution | Report as separate SLI: eligible informational intents resolved with approved, current evidence. | Segment by module, region, locale, release and claim risk; target determined after baseline. |
| End-to-end latency | p95 ≤ 8 seconds at the approved reference load, matching Doc07 proposal. | Report model wait, retrieval, claims and render separately. Baseline is unverified. |
| High-impact claim integrity | 0 fabricated, expired or revoked P1 claims; 100% high-impact fixture pass. | P0 invariant; synthetic + independent audited samples, not aggregate availability. |
| Private boundary | 0 unauthorised private service access or tenant content leakage. | P0 hard stop, network/IAM probes and privacy assurance. |
| KB revocation propagation | Safe immediate deny of revoked claim/source on response commit. | Synchronous policy check; background index purge follows separately. |
| Safety fallback usability | 100% of tested outages return an accessible, safe typed outcome or static support route. | Synthetic browser journeys and status reason distribution. |

| TARGETS NOT CONTRACTUAL All thresholds are proposed design objectives and must be calibrated by SRE and Product from traffic and approved business criticality. No customer-facing SLA, performance claim, or go-live assurance is implied by this document. |
|---|

## 13  Error budgets, burn-rate paging and alert hygiene

For a 99.9% monthly availability SLO, the nominal budget is 0.1% bad events over the month. Use multi-window burn-rate alerting adapted from Google SRE practice rather than one noisy short-window threshold. Proposed combinations require replay testing against historical or synthetic incident telemetry.

| Alert | Proposed condition (both windows) | Response / audience |
|---|---|---|
| Page: rapid burn | 14.4× error-budget burn across 1h AND 5m; min request volume established. | On-call immediately; incident commander if sustained. |
| Page: sustained burn | 6× burn across 6h AND 30m; valid denominator / no absent data. | On-call + SRE lead; freeze promotions. |
| Ticket: gradual burn | 1× burn across 3d AND 6h, with noise filtering. | SRE product improvement task and owner. |
| P0 factual / security | Any confirmed revoked P1 claim, private route reach, invalid signing, disallowed response. | Immediate stop; safety switch and incident case. |
| Synthetic journey broken | Outside-in chat/policy/claim fallback test fails on two independently routed probes. | Check platform and vendor; page if customer impact. |
| Privacy trace/metric leak | Any raw prompt/PII-like payload unexpectedly recorded by observability pipeline. | Privacy incident triage; restrict/export-delete per DPO. |

Alert correctness requirements: verify time alignment, service-level denominator, traffic segmentation, sufficient sample volume, deduplication, quiet-hours policy, no-data semantics, suppression authority and on-call escalation. Do not page on provider status alone if user-facing outcomes remain good; do not suppress P0 invariants.

## 14  Observability instrumentation and privacy-safe schema

| Signal | Approved example fields | Explicitly excluded by default |
|---|---|---|
| Trace | trace_id, parent span, request_id, release tuple hashes, service, outcome enum, error.type, latency stage. | Full user prompt, model instructions, raw retrieved document text, email or financial identifiers. |
| Model span | Profile ID, approved provider category, usage token counts, timeout type, cache status, request latency. | Input/output messages, source passages, secret prompt or provider credentials. |
| Knowledge span | Source release hash, public chunk IDs, retrieval count, citation validation outcome, revocation flag. | Full source bodies or PII-bearing retrieval queries. |
| Commercial checks | Claim ID, verdict APPROVED/EXPIRED/REVOKED/UNKNOWN, market key, effective-date validity. | Free-text customer names, billing account identifiers. |
| Metric | Request count, outcome labels, p50/p95/p99 latency, DLQ depth, cost units, canary stage. | Session or request IDs as metric labels (unbounded cardinality). |
| Audit | Actor role, signed change ID, before/after hashes, timestamp, approval record. | Raw conversation transcripts except independently approved minimal evidence. |

Adopt OpenTelemetry standard span conventions where applicable, but explicitly disable sensitive GenAI input/output capture; upstream instrumentation settings must be tested in staging. Generate low-cardinality metric dimensions (environment, region, endpoint family, release stage, outcome), not arbitrary free-text attributes.

## 15  Dashboard and runbook binding

| Dashboard / panel | Minimum panels | Owner |
|---|---|---|
| Executive service health | Availability vs objective, grounded-answer SLI, safety incidents, P0 claims, release changes. | SRE + Product |
| Latency waterfall | Edge, policy, KB, claim resolution, model, validation, client render p50/p95/p99. | Backend + SRE |
| Commercial integrity | Approved/unknown/revoked claim outcomes, coverage by market, stale-source rejections. | KB/Commercial |
| AI quality and drift | Golden/holdout eval pass, unsafe attempts, abstain rates, intent confusion, provider model ID. | AI QA |
| Production cost | Cost/1k useful answers, token in/out, provider retries, prompt/cache efficiency, budget headroom. | FinOps/AI |
| Operations queue | DLQ events, retries, queue wait, circuit breaker, active incidents, pending owner actions. | SRE |

Every critical panel must link to one runbook, on-call rotation, relevant release record, and validated synthetic probe. Monitoring software names and links remain to be filled by the actual operations team.

## 16  Runbook RB-01: model provider degraded or unavailable

1. Detect: elevated generation timeouts/503/429 or outside-in failures; inspect private-safe trace counts and provider status. Confirm impact to public intents.

2. Triage: distinguish vendor outage from local egress, token quota, DNS, policy or signed-claim failures. Capture incident/release IDs; do not export raw prompts.

3. Contain: engage model-unavailable switch; preserve deterministic routes, approved FAQs and explicit “I cannot confirm” responses; do not change permissions.

4. Recover: verify provider/model profile against approved allowlist; run at least one representative factual, P1 claim and refusal test before resuming canary.

5. Close: record duration, customer impact, vendor ticket, fallback behaviour, observations and follow-up to AI/SRE owners.

| Artifact | Minimum evidence |
|---|---|
| RB-01 evidence | Provider latency/error chart; active model profile; fallback switch receipt; QA replay result; incident record; owner/time of re-enable. |

## 17  Runbook RB-02: stale price, wrong claim or unsafe citation

1. Classify any fabricated, revoked or materially incorrect price, trial, market, certification or assurance statement as P0 pending verification.

2. Immediately revoke the identified claim/source ID at the authoritative registry; invalidate cached/inflight responses at final commit and disable affected intent if uncertain.

3. Search privacy-safe response receipts and public affected-release IDs; bound the affected market/time/turn population without collecting new unnecessary user data.

4. Restore only after signed claim re-publish, source-index verification, deterministic policy tests and a second approver; full app rollback is not a substitute.

5. Record incident, commercial/legal assessment, correction notice decision, evaluator updates and KB editorial root cause.

| INDEPENDENT ROLLBACK INVARIANT A code version rollback must not resurrect revoked claims, expired market availability, or removed knowledge. Source revocations must be monotonic across image/prompt/model changes, and the final response gate must consult current revocation state. |
|---|

## 18  Runbook RB-03: privacy incident or prompt injection

1. Stop the affected behaviour: disable offending route/prompt bundle/model or set public assistant to static-safe mode; isolate compromised key/source.

2. Preserve only proportionate forensic metadata with controlled evidence access; involve AppSec, DPO/Legal and incident command without circulating private content.

3. Assess whether any protected data was accessed, transmitted to model supplier, logged, shown to another user or linked to a person.

4. Investigate prompt/vector provenance, compromised access paths and impacted deployment windows; rotate/revoke credentials where necessary.

5. Apply formal notification decision tree with responsible counsel/DPO based on actual jurisdictions and statutory deadlines; do not assume a notification is always or never required.

6. Re-enable only when negative tests, outbound egress checks, telemetry redaction and signed independent approval pass.

## 19  Runbook RB-04: deployment rollback and safe-static mode

| Phase | Operator action | Required verification |
|---|---|---|
| Trigger | Failed P0 assertion, burn alert, crash loop or human release stop. Disable further promotion. | On-call and incident owner notified. |
| Immediate safety | Set verified server-side assistant-disable/safe-static switch with a signed change receipt. | Official routes remain safe, accessible and truthful. |
| Revert application | Promote known-good immutable digest using the approved deployment tool; avoid mutable “latest”. | Gateway policy and runtime contracts still match. |
| Revert policy/prompt | Roll back signed bundle pointer independently after compatibility and safety checks. | No elevated tool set; matched model profile. |
| Preserve revocation | Do not roll back active claim/source revocation; reconcile KB index publication state. | Critical claims still abstain as intended. |
| Postrollback QA | Synthetic tests: public chat, access denial, P1 fail-closed, source citation, accessible fallback. | Two-person release closeout and incident evidence. |

## 20  Backup, recovery, resilience and disaster recovery

| Data/asset | Recovery policy | Proposed RTO/RPO |
|---|---|---|
| Immutable artifacts | Replicate release tuple, signatures, manifest and SBOM across approved environments. | RPO = 0 for approved published artifacts (via immutable archive). |
| Commercial claims + revocations | Reconstruct from signed append-only ledger; replay revocations after point-in-time restore. | Safety correctness takes priority over availability. |
| Knowledge index | Rebuild from approved versioned public source corpus; validate source hashes, publication status. | RTO proposed ≤ 60 min for safe information functionality; verify by drill. |
| Public site fallback | CDN/static approved routes and a clear unavailable status; independent of model/KB store. | RTO proposed ≤ 15 min to safe-static public experience. |
| Ephemeral conversation state | Expire/drop sessions during major disaster; avoid restoring raw anonymous transcripts. | No strict chat-history RPO; explicitly user-facing reset. |
| Editorial approvals | Encrypted access-controlled snapshots with tested restore; retain immutable decision audit. | RPO / RTO must be DPO + operations approved. |

The numerical recovery values above are proposed targets, not observed capability. SRE must prove them with isolated restore and upstream dependency outage drills before launch; product/legal approve any business-facing commitment.

## 21  Operational model and incident command

| Priority | Definition and response | Command / communications |
|---|---|---|
| SEV-0 / P0 | Private service/data reach, financial-data exposure, wrongful material claim or broken safety switch. | Immediate on-call page and deployment halt; incident commander, security/DPO and CTO escalation. |
| SEV-1 | Significant outage, sustained major SLO burn, mass inaccessible chat with no safe path. | SRE lead with Product and Web; proactive status update through approved channels. |
| SEV-2 | Localized provider degradation, non-critical wrong module content, delayed noncritical event. | Owned incident ticket, timeboxed mitigation and regression. |
| SEV-3 | Cosmetic defect, low-priority operational improvement without safety impact. | Planned backlog item and standard change review. |

Incident lifecycle: detect → declare → appoint commander/scribe → assess exposure → contain → communicate → restore → verify → close → postmortem. Ensure chat safety status can be published even if normal SRE observability is unavailable. Roles should have deputies and an on-call backup.

## 22  Incident communications, audit and retention

| Item | Mandatory evidence / control |
|---|---|
| Incident ID | Creation and observed start, severity, declared impact, affected region/release. |
| Command and timeline | Named commander/scribe, decisions, timestamps, change receipts, stakeholder notifications. |
| Communications | Approved public status text; accurate “unavailable”/“limited” wording without unsupported assurances. |
| Security/privacy | DPO determination, forensics hold, restricted evidence access, notification evaluation, vendor engagement. |
| Closure | Containment test, rollback or fix proof, postmortem owner, root-cause tasks with due dates. |
| Retention | Documented schedules by data category and region; no blanket retention value invented in this playbook. |

## 23  Model operations and policy change management

| Change type | Required gate |
|---|---|
| Model identifier/version | Contract compatibility, P0 refusal/routing, quality regression, latency/cost, vendor privacy review. |
| Temperature / length / token budget | Re-run hallucination/claim and abuse tests; validate provider quota and cost headroom. |
| System prompt or safety instruction | Version and sign prompt; policy negative tests; model cannot override deterministic controls. |
| Model vendor or hosting region | Separate DPO/legal supplier and cross-border review, DPIA/transfer checks and network egress adjustment. |
| Model outage failover | No automatic switch to unapproved vendor/model; safe-static or preapproved fallback only. |
| Tool registry | V1 remains no operational tools; any tool enablement requires architectural revision and new security review. |

## 24  Knowledge and commercial release operations

| Lifecycle point | Publication guard | Emergency reaction |
|---|---|---|
| Import | Typed source inventory, owner, source classification, virus/content extraction policy. | Quarantine failed source. |
| Review | EDITOR → LEGAL/COMMERCIAL when needed → PUBLISHER, two-person sign for P1. | Lock contradictory assertions. |
| Index | Embed and chunk only signed PUBLIC_APPROVED sources; maintain hash and release evidence. | Do not let partial index represent approved full release. |
| Cutover | Atomic pointer update, version bounds and source tests; publish effective-at time. | Rollback KB pointer, but never replay revoked claims. |
| Expiration | Evaluate market/date and knowledge freshness on each material answer. | Fail closed to approved static link or abstain. |
| Revocation | Immediate deny in serving path, invalidate caches, background purge and audit. | Kill affected intent if revocation interface fails. |

## 25  Cost, capacity and vendor quota FinOps controls

| Metric / limit | Required budget control |
|---|---|
| Cost per 1,000 useful resolutions | Total model + embedding + retrieval + inference infra cost divided by verified useful grounded answers, not raw turns. |
| Token and context budget | Input/output caps, retrieval size cap, bounded conversation turns and lower-cost evaluated model choice. |
| Daily/monthly spending ceiling | Separate soft warning, hard breaker and incident escalation; fail to safe deterministic support routes. |
| Abuse and retry cost | Per-origin/client coarse abuse quotas, anonymous rate caps, backoff and no unbounded self-retry. |
| Vendor limits | Approved provider QPS/TPM rate baseline, key rotation and no provider switching to evade quotas. |
| Capacity forecast | Traffic, proportion of claim paths, regional mix, p95 latency, worst-case tokens and headroom assumptions. |

## 26  Configuration, feature flags and kill-switch governance

| Flag / control | Safety properties | Test |
|---|---|---|
| public_ai_enabled | Server verified, audited; false switches to approved static handoff without breaking site. | Both UI and API reject dynamic generation. |
| model_generation_enabled | Independent from rendering; disable provider calls while exposing official routes. | Provider call count stays zero. |
| knowledge_release_id | Only signed valid manifest; idempotent pointer update. | Invalid signature fails closed. |
| p1_claim_registry_id | Signed date/market-accurate claims; revocation authoritative. | Revoked price never restored by rollback. |
| max_concurrent_turns | Exactly one active turn per session, rate quota server-enforced. | Cross-instance race fails safely. |
| canary_percentage | Change-authorized, audited release/traffic control. | No unapproved ramp by automated model process. |

## 27  Privacy, regional operations and audit boundaries

Production telemetry must minimize content collection by default. Vendor log settings, DPA and subprocessor terms, data region, backups, cookie controls, deletion and retention must be approved by DPO/Legal. It is unacceptable to promise “zero retention” or “EU-only processing” without signed, verifiable provider and processing records.

| Privacy assurance | Operational test / evidence |
|---|---|
| Data path inventory | Document actual request, response, trace, logs, source query, human support CTA, provider and backup processors. |
| Pre-provider content minimization | Intercept/redact unnecessary financial, health, identity, banking and payroll data before outbound vendor call where technically appropriate. |
| Export / retention | Proof that vendor usage, error log, crash dump and APM trace do not retain full content by default. |
| Access governance | Restricted read access to security/operations artifacts, periodic review and break-glass audit. |
| Regional constraints | Egress allowlist and actual data location verified; no assumption inferred from user locale. |
| Erasure and legal hold | Testable deletion workflows aligned with lawful retention and incident preservation decisions. |

## 28  Quality gates mapped to Document 07

| Check family | Minimum production evidence |
|---|---|
| Functional UI / journeys | Desktop/mobile user flows, retries, errors, accessible disclosure, handoffs and valid routes. |
| Intent / AI evaluation | Golden and withheld sets, high-risk intent false-permit, independent judgment and scoring artifacts. |
| P1 claim and KB accuracy | Signed price/security/market assertions, time validity, citation revocation and abstention tests. |
| API and event contracts | Doc05 schema fixtures, idempotency/concurrency, DLQ/replay and safe error semantics. |
| Security and privacy | All mandatory Doc06 assertions, private endpoint egress denial, secret safety, privacy redaction. |
| Accessibility | WCAG 2.2 AA target; manual keyboard/screen reader/reflow and live-message behaviours. |
| Resilience / release | Canary SLO, provider outage, KB outage, kill switch, signed rollback, restore and on-call drills. |

No aggregate health score may override a P0 gate. Actual executed tests require timestamp, source build hashes, environment, result, owner, evidence and defect linkage. Do not treat this document or earlier standards language as a test execution report.

## 29  Production preflight checklist: 30 concrete verifications

| ID | Preflight assertion | Status / evidence |
|---|---|---|
| OPS-001 | Codebase/infrastructure discovery signed | NOT EXECUTED — attach build/receipt |
| OPS-002 | Current service owners/on-call and backup named | NOT EXECUTED — attach build/receipt |
| OPS-003 | Public/private egress denial proven | NOT EXECUTED — attach build/receipt |
| OPS-004 | DNS/TLS/WAF/certificate paths validated | NOT EXECUTED — attach build/receipt |
| OPS-005 | Stage/production secret separation proven | NOT EXECUTED — attach build/receipt |
| OPS-006 | Source and builder signatures enforced | NOT EXECUTED — attach build/receipt |
| OPS-007 | SBOM and critical vulnerability gates pass | NOT EXECUTED — attach build/receipt |
| OPS-008 | Signed 4-artifact release manifest available | NOT EXECUTED — attach build/receipt |
| OPS-009 | Schema/model-profile compatibility checked | NOT EXECUTED — attach build/receipt |
| OPS-010 | All P0 QA and security suites executed/passed | NOT EXECUTED — attach build/receipt |

| ID | Preflight assertion | Status / evidence |
|---|---|---|
| OPS-011 | P1 claim effective-date fixtures pass | NOT EXECUTED — attach build/receipt |
| OPS-012 | Source revocation at response-commit proven | NOT EXECUTED — attach build/receipt |
| OPS-013 | Canary route and rollback knob tested | NOT EXECUTED — attach build/receipt |
| OPS-014 | Kill switch tested by independent operator | NOT EXECUTED — attach build/receipt |
| OPS-015 | Safe-static page/CTA works when model fails | NOT EXECUTED — attach build/receipt |
| OPS-016 | One-active-turn race test across replicas passes | NOT EXECUTED — attach build/receipt |
| OPS-017 | Retry/idempotency/DLQ replay contract passes | NOT EXECUTED — attach build/receipt |
| OPS-018 | Readiness/startup/liveness probes tested | NOT EXECUTED — attach build/receipt |
| OPS-019 | Outside-in synthetic journeys healthy | NOT EXECUTED — attach build/receipt |
| OPS-020 | Real on-call receives test alerts | NOT EXECUTED — attach build/receipt |

| ID | Preflight assertion | Status / evidence |
|---|---|---|
| OPS-021 | SLO definition / denominator approved | NOT EXECUTED — attach build/receipt |
| OPS-022 | Burn-rate alert replay tested | NOT EXECUTED — attach build/receipt |
| OPS-023 | No sensitive GenAI attributes in traces | NOT EXECUTED — attach build/receipt |
| OPS-024 | Log/backup/processor retention approved | NOT EXECUTED — attach build/receipt |
| OPS-025 | DPIA/transfer/cookie decisions signed | NOT EXECUTED — attach build/receipt |
| OPS-026 | WCAG manual defects cleared | NOT EXECUTED — attach build/receipt |
| OPS-027 | Vendor limits, budget breaker and quotas tested | NOT EXECUTED — attach build/receipt |
| OPS-028 | Restore drill, RTO/RPO evidence captured | NOT EXECUTED — attach build/receipt |
| OPS-029 | Brand/IP, privacy notice and content approved | NOT EXECUTED — attach build/receipt |
| OPS-030 | CTO + QA + Security + SRE release signatures | NOT EXECUTED — attach build/receipt |

## 30  Operations acceptance test catalog — OPR-001 to OPR-048

### Release  /  8 tests

| Test ID | Execution scenario | Required result |
|---|---|---|
| OPR-001 | Unsigned app image is rejected | Artifact signature gate fails closed |
| OPR-002 | Unsigned prompt bundle is rejected | No model use without verified policy |
| OPR-003 | Unsigned KB release rejected | Knowledge resolver denies release |
| OPR-004 | Incompatible release tuple refused | No mixed-schema deploy |
| OPR-005 | Revoked claim remains denied across rollback | Response commit checks live revocation |
| OPR-006 | Canary stage cannot auto-ramp without approval | Human change gate required |
| OPR-007 | Canary p95 regression automatically halts | Ramp pauses and notifies |
| OPR-008 | Kill-switch flag survives edge cache | Old page cannot generate text |

### Platform  /  8 tests

| Test ID | Execution scenario | Required result |
|---|---|---|
| OPR-009 | Private finance hostname blocked at egress | Proof from pod/VM and mesh controls |
| OPR-010 | Public service token cannot call authenticated API | 403 and network deny |
| OPR-011 | Startup/readiness/liveness differentiated | No provider-induced restart loop |
| OPR-012 | Graceful termination handles inflight turn | Typed terminal outcome no duplicate |
| OPR-013 | Exactly-one active turn across replicas | Concurrent retry deduplicated |
| OPR-014 | Resource-exhaustion triggers bounded fallback | No runaway queue/provider calls |
| OPR-015 | Model egress allowlist blocks arbitrary host | Attack injection cannot change egress |
| OPR-016 | Event consumer replay preserves idempotency | DLQ duplicate does not create action |

### Observability  /  8 tests

| Test ID | Execution scenario | Required result |
|---|---|---|
| OPR-017 | Request trace includes release tuple hashes | No raw prompt data |
| OPR-018 | GenAI content attributes disabled | Trace and vendor collector checked |
| OPR-019 | High-cardinality labels rejected | No session IDs in metric keys |
| OPR-020 | Burn-rate rapid threshold pages human | Both windows verified |
| OPR-021 | Sustained burn alert triggers freeze | On-call acknowledges |
| OPR-022 | No-data behaviour and dedup tested | No silent missing alert |
| OPR-023 | Claim revocation metric correlates to release | Correct provenance and market |
| OPR-024 | Synthetic outage reports safe fallback | User-visible test checks accessibility |

### Incidents  /  8 tests

| Test ID | Execution scenario | Required result |
|---|---|---|
| OPR-025 | Provider 503 activates safe response | No fabricated model answers |
| OPR-026 | P1 registry unreachable forces abstain | No cached pricing |
| OPR-027 | KB source revoked mid-turn yields deny | Final commit gate catches |
| OPR-028 | Model vendor replacement blocked unless approved | No automatic vendor failover |
| OPR-029 | P0 incident freezes promotion | Change ticket locked |
| OPR-030 | Privacy leak detection escalates DPO | Forensic evidence access scoped |
| OPR-031 | Known-good rollback tested with claim monotonicity | No resurrection |
| OPR-032 | Static mode preserves valid support CTA | Accessible on mobile/desktop |

### Recovery  /  8 tests

| Test ID | Execution scenario | Required result |
|---|---|---|
| OPR-033 | Restore immutable releases from archive | Signature hashes match |
| OPR-034 | Index rebuild excludes revoked content | P0 KB checks pass |
| OPR-035 | Ephemeral sessions are invalidated safely | No user data resurrection |
| OPR-036 | Safe-static works without model/KB | Independent site route |
| OPR-037 | Restore drill meets ratified RTO | Evidence actual timestamps |
| OPR-038 | Backup/restore access constrained | Break-glass trace recorded |
| OPR-039 | Secrets rotation does not prolong outage | Health and egress tests |
| OPR-040 | Postrestore acceptance suite complete | QA + SRE sign |

### Quality/Cost  /  8 tests

| Test ID | Execution scenario | Required result |
|---|---|---|
| OPR-041 | Human QA gate blocks unsafe high-score model | No aggregate override |
| OPR-042 | Accessibility regression stops promotion | Manual evidence retained |
| OPR-043 | Anomalous token spend trips breaker | Preserve safe fallback |
| OPR-044 | Retry storm has bounded request and cost | Token budget enforced |
| OPR-045 | Wrong effective-date market claim is blocked | Signed claim validator |
| OPR-046 | Abstain rate shifts generate investigated alert | No fabricated cause |
| OPR-047 | Vendor/processor region change requires privacy signoff | Deployment held |
| OPR-048 | Release manifest creates immutable audit record | Traceability across all four artifacts |

## 31  Runbook-to-test and governance traceability

| Domain | Document 08 authority / tests | Predecessor binding |
|---|---|---|
| Risk and scope | §01–04, OPR-009/010 | 01 public scope, 04 denied business data |
| Artifact integrity | §06–09, OPR-001–008 | 03 source/claim signoff, 04 architecture, 05 contracts |
| Runtime/alerts | §10–15, OPR-011–024 | 04 reliability, 05 events, 07 p95 and quality |
| Incident/rollback | §16–22, OPR-025–040 | 06 incident security/retention, 07 P0 gates |
| Model/source ops | §23–27, OPR-005/026/029/047 | 02 prompts, 03 knowledge, 06 vendor/data risk |
| Launch acceptance | §28–35, OPS-001–030, OPR-041–048 | 07 84-case QA suites; release go/no-go |

## 32  Operating ownership, RACI and duty schedules

| Workstream | Responsible (R) | Accountable (A) | Consulted / approval |
|---|---|---|---|
| App/platform deployment | SRE / Platform | Global CTO | API Lead, QA, AppSec |
| Prompt/model profile | AI Engineering | AI Product Owner | QA, DPO for provider changes |
| Public KB publishing | Knowledge Ops | Product owner | Legal, Commercial, Security |
| P1 claim release | Commercial Ops | Commercial Director | Legal, Product, SRE |
| Security incidents | AppSec / SRE | Security executive | DPO, Legal, CTO |
| Data privacy/retention | DPO / Legal | Accountable data controller lead | SRE, vendor management |
| Go/no-go decision | Release manager | Global CTO | Product, QA, Security, SRE |

Minimum operating cadence (proposal): per deployment—change review, smoke and P0 checks; daily—alerts, cost, DLQ, claim failure review; weekly—drift/holdout and source freshness; monthly—access, incident/problem review, spend and SLO/error budget; quarterly—kill-switch and restore drills, provider/privacy/vendor posture review. Final cadence requires CTO/SRE ratification.

## 33  Release evidence pack schema and change-ticket contents

| Field | Evidence requirement |
|---|---|
| Release identity | Ticket ID, environment, owner, start/end time, previous and candidate release tuple SHA IDs. |
| Source and supply chain | Repo SHA, container digest, signed provenance, SBOM, scan report, approved deployment job. |
| AI/knowledge/claims | Policy+prompt hash, model profile, KB signed manifest, P1 signed claims/version, revocation ledger watermark. |
| Validation | QA suite 07 link, security suite 06 link, actual counts, baseline comparison, WCAG manual check. |
| Operational decision | Canary cohorts and observation windows, SLO snapshots, synthetic probes, alert tests, budget headroom. |
| Rollback | Prior verified compatible tuple, rollback operator and ETA measured by drill, safety switch receipt. |
| Approval | Named signatories, UTC timestamps, open decision dispositions, go/no-go outcome and monitoring owner. |

Machine-readable release evidence stub (illustrative JSON)

{  "change_id": "CHG-<id>",  "assistant_id": "zoikosuite-public-ai",  "environment": "production",  "four_artifact_releases": {    "app": "sha256:<hash>",    "policy": "<hash>",    "public_kb": "<hash>",    "p1_claims": "<hash>"  },  "evidence": {    "qa_suite": "<url-or-record-id>",    "security_suite": "<id>",    "canary_sli": "<id>",    "rollback_drill": "<id>"  },  "decisions": {    "cto": "PENDING",    "qa": "PENDING",    "security": "PENDING",    "sre": "PENDING"  },  "deploy_status": "NOT_STARTED"}

## 34  Unresolved decisions — accountable launch blockers

| ID / severity | Decision required before deployment | Owner |
|---|---|---|
| D08-01 / P0 | Confirm actual infrastructure provider, scheduler, deployment controller and network policy proof. | SRE / CTO |
| D08-02 / P0 | Approve hosting/model vendor, subprocessors, region/transfer, retention and privacy notices. | DPO / Legal |
| D08-03 / P0 | Define exact release-publishing/signing infrastructure for four artifacts and revocation ledger. | Platform / KB |
| D08-04 / P0 | Confirm signing/auth and compatibility of Doc05 route/event contracts and exact domain names. | API lead |
| D08-05 / P0 | Approve SLO definitions, load profile, incident command/on-call 24/7 coverage. | SRE / Product |
| D08-06 / P0 | Approve deployed kill switch, safe-static fallback and verified previous release. | SRE / Security |
| D08-07 / P0 | Pass source/time-specific commercial claims and active revocation at final commit. | Commercial / KB |
| D08-08 / P0 | Clear public assistant display name, correct brand files, human support links and legal notices. | Brand / Legal |
| D08-09 / P1 | Calibrate cost/token budgets and quota limits using representative traffic. | FinOps / AI |
| D08-10 / P1 | Approve regional operating hours and incident/public status communication script. | Operations |
| D08-11 / P1 | Approve backups/RTO/RPO from measured isolated restore drills. | SRE / CTO |
| D08-12 / P1 | Choose compatible monitor/APM tools and privacy-safe OpenTelemetry export configuration. | SRE / DPO |

## 35  Critical re-assessment: defects and refinements incorporated

| Initial operating risk | Refinement adopted in this controlled version | Validation |
|---|---|---|
| One release switch for code and pricing | Split four signed release artifacts; final-commit revocation check. | OPR-005/029/037 |
| Kubernetes health = AI correctness | Separate health, claim truth, knowledge relevance and safe fallbacks. | OPR-011,019,032 |
| Logs capture raw model prompts | Privacy-safe schema; sensitive content off by default. | OPR-017/018 |
| Percent-based canary gives false confidence | Sample thresholds and synthetic tests; human promotion and P0 veto. | OPR-006/007 |
| Generic availability counts safe refusals | Explicit safe API vs useful-grounded resolution SLIs. | SLI register §12 |
| Uncontrolled vendor auto-failover | Approved model profiles only; otherwise static safe path. | OPR-028 |
| Restoring old backups resurrects bad claims | Revocation ledger monotonic; source expiry revalidated. | OPR-037/038 |
| Unknown cloud/stack asserted as fact | Discovery checklist, illustrative manifests and ADR authority. | OPS-001 |

## 36  Final go/no-go decision record — approval template

| Release gate | Evidence owner / required outcome | Decision |
|---|---|---|
| G0 — Repo / contracts | CTO/Architecture: verified deployment topology, route allowlists, source signing and artifact owners. | PENDING |
| G1 — Sec/Privacy | AppSec/DPO: P0 boundary, supplier, retention and signed privacy controls passed. | PENDING |
| G2 — Product/AI QA | QA/Product: 84 scenarios + predecessor suites, citations, intent, claim and WCAG passed. | PENDING |
| G3 — Operational rehearsal | SRE: alert delivery, rollback, kill switch, vendor outage, DR, cost and canary procedure evidenced. | PENDING |
| G4 — Brand/Commercial | Product/Legal/Commercial: name, logo, plan facts, CTA routing and disclosures authorised. | PENDING |
| G5 — CTO production gate | Global CTO: all no-go blockers closed, accountable on-call appointed; signed launch decision. | PENDING |

| RELEASE CLASSIFICATION State remains DESIGN COMPLETE FOR REVIEW — NOT DEPLOYED / NOT TEST EXECUTED / NOT PRODUCTION AUTHORISED. Founder approval of this document authorizes its use as a controlled engineering baseline only. |
|---|

## Appendix A  Platform-neutral operational configuration example

Example: controlled deployment policy in YAML — illustrative; not a ready-to-run infrastructure manifest

apiVersion: zoiko.example/v1alpha1kind: PublicAssistantReleasemetadata:  name: zoikosuite-public-aispec:  exposure: public_anonymous  allowed_business_data_planes: []  model_tools_allowed: []  artifacts:    app_digest: 'sha256:<verified>'    prompt_policy: '<signed-hash>'    public_knowledge: '<signed-hash>'    commercial_claims: '<signed-hash>'  promotion:    stages: [staging, canary_1, canary_10, canary_50, production]    manual_approval_required: true    block_on_p0: true  safety:    allow_static_fallback: true    verify_claim_revocation_at_commit: true    deny_private_egress: true

For a Kubernetes implementation, the team may translate these requirements into Deployments, NetworkPolicies, workload identities, admission policies, probes, disruption budgets and progressive delivery resources. Such translation must be reviewed against the live infrastructure. Kubernetes does not itself provide model/content signing or semantic safety guarantees.

## Appendix B  Illustrative operations metric / alert schema

Example OpenTelemetry-aligned semantic conventions — omit content payload

{  "service.name": "suivo-public-orchestrator",  "deployment.environment.name": "production",  "suivo.release.app": "<hash>",  "suivo.release.policy": "<hash>",  "suivo.release.kb": "<hash>",  "suivo.release.claims": "<hash>",  "suivo.outcome": "GROUNDED|ABSTAIN|SAFE_FALLBACK|REFUSED",  "gen_ai.operation.name": "chat",  "gen_ai.request.model": "<model-profile>",  "gen_ai.usage.input_tokens": 0,  "gen_ai.usage.output_tokens": 0,  "error.type": "none",  "duration_ms": 0,  "_prohibited": [    "gen_ai.input.messages",    "gen_ai.output.messages",    "gen_ai.system_instructions",    "raw_prompt"  ]}

The _prohibited entry above is a configuration review checklist, not a real telemetry attribute. Forbid content capture at the exporter and upstream SDK layers, not simply in dashboard field selection. Do not log a stable anonymous conversation identifier unless the DPO has explicitly approved the need, protection and retention.

## Appendix C  Change, incident and recovery dry-run scripts

| Scenario | Test choreography | Receipt |
|---|---|---|
| P1 price revocation | Start test turn; revoke signed price before response commit; check abstention and no stale result in browser/cache. | Source ID / release tuple / revocation timestamp / typed outcome. |
| Model vendor unavailable | Inject 503/timeout and request public price, product comparison, and support path. | No unauthorised fallback facts; safe route and alarm. |
| Prompt-injection in source | Import mock PUBLIC_DENIED/malicious content into staging; query with prompt override. | No publication; denied tool and network checks. |
| Blue/green rollback | Promote candidate, force breach, roll back app/policy, re-check revocation watermark. | Signed prior tuple / health / QA / incident timeline. |
| Disaster restore | Delete/rebuild staging KB index and control state from approved releases in isolated environment. | Measured restore time; no revoked or customer data. |
| Alert validation | Simulate 14.4× 1h+5m and 6× 6h+30m burn with test metrics. | Page receipt, ack, dedup and SRE review. |

## Appendix D  Standards and controlling references

| Primary official source | Operational use / URL |
|---|---|
| Kubernetes Deployments and Rollback | Rolling and paused rollouts, undo, progress deadlines: https://kubernetes.io/docs/concepts/workloads/controllers/deployment/ |
| Kubernetes Probes | Startup vs readiness vs liveness semantics: https://kubernetes.io/docs/concepts/workloads/pods/probes/ |
| OpenTelemetry | OpenTelemetry semantic convention registry and GenAI content warning: https://opentelemetry.io/docs/specs/semconv/ |
| Google SRE Workbook | SLO/error-budget alerting: https://sre.google/workbook/alerting-on-slos/ |
| NIST SP 800-218 (final) | Secure Software Development Framework version 1.1: https://csrc.nist.gov/pubs/sp/800/218/final |
| SLSA v1.2 | Supply-chain signing/provenance: https://slsa.dev/spec/v1.2/ |
| W3C WCAG 2.2 | Accessibility gates: https://www.w3.org/TR/WCAG22/ |
| ZoikoSuite controlled docs | Engineering Documents 01–07; cite internal release ID in each change ticket, not this filename alone. |
