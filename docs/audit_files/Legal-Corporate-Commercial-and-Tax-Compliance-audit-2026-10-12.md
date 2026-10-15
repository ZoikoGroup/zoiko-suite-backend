# Domain 6 (Legal, Corporate & Commercial) + Domain 7 (Tax & Compliance) — documentation compliance audit

Date: 12 Oct 2026 · Scope: 20 services (11 + 9) · Method: static read of code against docs. No tests were run and nothing was exercised live; every "cross-tenant" finding below is established from SQL + deployment config, not reproduced.

## 0. What "the documentation" is for these services

No document defines HTTP endpoints (method + path), request/response schemas or status codes for any of these 20 services. The contract that exists is:

| Source | What it gives | Services covered |
| --- | --- | --- |
| `docs/architecture/03-microservices.md` §12–§13 (Doc 03) | Purpose, published event names, one-line critical constraint | all 20 |
| `ZoikoSuite_Legal_Corporate_Governance_Contract_Lifecycle_…docx` (ZS-SVC-O-001) | Named commands/queries, states, SoD, canonical events, LEG-CTRL-001…040, negative tests T01…T50 | LEG-04 board-resolutions, LEG-05 contract-lifecycle, LEG-06 clause-template, LEG-07 obligation-tracking |
| `ZoikoSuite_Procurement_Expenses_Accounts_Payable_…docx` (ZS-SVC-D-001) | Same, plus "illustrative" `/internal/v1/apNN/...` surfaces | AP-02 purchase-request, AP-03 purchase-order |
| `ZoikoSuite_Tax_EInvoicing_Regulatory_Compliance_…docx` (ZS-SVC-F-001) | Same | TAX-02 tax-rules, TAX-03 tax-determination, TAX-04 vat-gst, TAX-05 withholding-tax, TAX-06 corporate-tax, TAX-09 filing-preparation, TAX-10/12 filing-tracker |
| `ZoikoSuite_Global_Service_Requirements_Input_Contract_Catalogue.docx` §4, §9 | Common request envelope; per-service required inputs | all (§4); spec-mapped services (§9) |

So "endpoint compliance" below means **named command/query coverage**, and "schema" means **§9 required inputs and canonical-event payload fields**. Status codes are scored only where a doc implies one (401 for a missing tenant, §4).

Services with **no detailed spec**, scored against Doc 03 alone (their % is therefore not comparable with spec-backed services — a thin contract is easy to meet): corporate-actions, counterparty-management, spend-controls, vendor-due-diligence, procurement-workflow, compliance-status, exception-escalation.

Scoring: ✅ = 1, ⚠️ = 0.5, ❌ = 0. ❓ (needs clarification) rows are excluded from the denominator.

## 1. Results at a glance

| # | Service | Port (doc → code) | Doc basis | Score |
| --- | --- | --- | --- | --- |
| 1 | contract-lifecycle-svc | 8119 → 8119 | LEG-05 + Doc 03 §12.1 | 10.5 / 24 = **44%** |
| 2 | clause-template-svc | 8120 → 8120 | LEG-06 + §12.2 | 13.5 / 23 = **59%** |
| 3 | obligation-tracking-svc | 8121 → 8121 | LEG-07 + §12.3 | 9 / 20 = **45%** |
| 4 | board-resolutions-svc | 8122 → 8122 | LEG-04 + §12.4 | 9 / 19 = **47%** |
| 5 | corporate-actions-svc | 8123 → 8123 | §12.5 only | 2.5 / 12 = **21%** |
| 6 | counterparty-management-svc | 8124 → 8124 | §12.6 only | 5 / 12 = **42%** |
| 7 | purchase-request-svc | 8100 → 8100 | AP-02 + §12.8 | 16.5 / 18 = **92%** |
| 8 | purchase-order-svc | 8129 → 8129 | AP-03 + §12.9 | 14 / 18 = **78%** |
| 9 | spend-controls-svc | 8131 → 8131 | §12.12 only | 8 / 11 = **73%** |
| 10 | vendor-due-diligence-svc | **8132 → 8135** | §12.10 only | 7.5 / 11 = **68%** |
| 11 | procurement-workflow-svc | 8134 → 8134 | §12.7 only | 8 / 10 = **80%** |
| 12 | tax-rules-svc | 8125 → 8125 | TAX-02 + §13.1 | 2.5 / 14 = **18%** |
| 13 | tax-determination-svc | 8126 → 8126 | TAX-03 + §13.2 | 6.5 / 18 = **36%** |
| 14 | vat-gst-svc | 8127 → 8127 | TAX-04 + §13.3 | 4 / 11 = **36%** |
| 15 | corporate-tax-svc | 8128 → 8128 | TAX-06 + §13.4 | 2.5 / 11 = **23%** |
| 16 | withholding-tax-svc | 8129 → 8129 | TAX-05 + §13.5 | 4.5 / 12 = **38%** |
| 17 | filing-preparation-svc | 8130 → 8130 | TAX-09 + §13.6 | 4.5 / 11 = **41%** |
| 18 | filing-tracker-svc | 8131 → 8131 | TAX-10/12 + §13.8 | 5 / 12 = **42%** |
| 19 | compliance-status-svc | 8132 → 8132 | §13.7 only | 4.5 / 8 = **56%** |
| 20 | exception-escalation-svc | 8133 → 8133 | §13.9 only | 4.5 / 8 = **56%** |
| | **Overall** | | | **142 / 283 = 50%** |

By domain: Legal & corporate (1–6) **43%** · Procurement (7–11) **78%** · Tax & compliance (12–20) **38%**.

## 2. Top gaps, ranked by risk

### Security / authorization

| Rank | Gap | Services | Evidence |
| --- | --- | --- | --- |
| S1 | **Cross-tenant read and write.** Store SQL filters by id only (`WHERE action_id = $1`, list has no tenant predicate) and relies on RLS. `deployments/docker-compose.yml` connects these services as `${DB_USER:-postgres}` and `.env`/`.env.local` do not set `DB_USER`, so they run as superuser, which bypasses RLS (including FORCE). Any tenant can GET/list/PUT another tenant's rows by id. | corporate-actions, counterparty-management (counterparty table: tax IDs, emails, phones), tax-rules, corporate-tax | `corporate-actions-svc/internal/store/pg_store.go:113-194`, `counterparty-management-svc/internal/store/pg_store.go:102,138,185,217`, `tax-rules-svc/internal/store/pg_store.go:98,131,177`, `corporate-tax-svc/internal/store/pg_store.go:111,151,202` |
| S2 | **Compounding tax effect of S1.** tax-determination fetches `GET /v1/tax-rules?status=ACTIVE` and takes `Rules[0]`. Under S1 that list spans tenants, so a determination can apply another tenant's rate. | tax-determination ← tax-rules | `tax-determination-svc/internal/rules/client.go:72-73` |
| S3 | **Status field writable through PUT.** `UpdateAction` copies `req.Status` and the UPDATE has no state guard, so a caller can set `EXECUTED`/`BOARD_APPROVED` without the execute path's SoD and evidence check, or edit an executed action in place. Same pattern (status via PUT) in tax-rules and counterparty-management. | corporate-actions (critical), tax-rules, counterparty-management | `corporate-actions-svc/internal/handler/handler.go` UpdateAction; `store/pg_store.go:192-196` |
| S4 | **8 audit-finding write endpoints have no authorization check**: misstatements, materiality, communicate, management-response, remediation, scope-limitations (record and resolve), reopen. | exception-escalation | `exception-escalation-svc/internal/handler/finding_handler.go` (2 `CheckAllowed` calls across 13 handlers) |
| S5 | **Audit attribution taken from the request body.** `created_by`, `terminated_by`, `amended_by`, `renewed_by`, `signed_by`, `executed_by`, `filed_by` are read from JSON rather than the verified `X-Principal-Id`, so the audit trail can name anyone. board-resolutions shows the fix (`requireSelfAttribution`). | contract-lifecycle, clause-template, obligation-tracking, corporate-actions, counterparty-management, vat-gst | e.g. `contract-lifecycle-svc/internal/handler/handler.go:115` |
| S6 | **Voter population and quorum chosen by the caller.** OpenVoting takes `voter_principal_ids` and `quorum_threshold` from the body. They are frozen once open, but nothing sources them from the director/shareholder register as-of, so whoever opens voting decides who may vote (LEG-CTRL-005, T09). | board-resolutions | `board-resolutions-svc/internal/store/pg_store.go:439-495` |
| S7 | **Execution recorded without signer authority or document-digest binding** (LEG-CTRL-004/016, T25, T49). | contract-lifecycle | `store/pg_store.go:504-552` |

### Data integrity

| Rank | Gap | Services |
| --- | --- | --- |
| D1 | **Tax fails open to 0%.** Any error from tax-rules (unreachable, non-200, no matching rule) yields a `trule-fallback` rule at 0% and a recorded determination (invariant 3: missing rule must block). The rule is also not effective-dated to the supply date (`status=ACTIVE` only), and overlaps resolve to `Rules[0]`. | tax-determination (`handler.go:271-277`, `rules/client.go:48-57`) |
| D2 | **Tax rates edited in place.** PUT rewrites `tax_rate_percentage`/exemptions/status on an ACTIVE rule, with no certification, source citation, maker-checker, overlap check or version history (TAX-02 negative path "operator edits rate in production table"). | tax-rules |
| D3 | **Filed, submitted or remitted records editable in place** (INV-07, T23): PUT has no status guard. | vat-gst (FILED returns), corporate-tax (SUBMITTED/ASSESSED), withholding-tax (REMITTED), filing-preparation (READY_FOR_SUBMISSION), corporate-actions (EXECUTED) |
| D4 | **Finalize skips validation.** A DRAFT filing goes straight to READY_FOR_SUBMISSION without Validate, so the evidence-requirements check never runs; the code's own comment says no finalization path may skip it. | filing-preparation (`store/pg_store.go` Finalize) |
| D5 | **Caller-supplied rates and self-asserted treaty relief.** Withholding rate comes from the request (`TAX_RULES_SERVICE_URL` is configured but never called). `tax_treaty_exemption=true` plus any non-empty certificate string zeroes withholding, with no validity/expiry check and no override approval (T11, T12). corporate-tax also takes `tax_rate_percent` from the caller. | withholding-tax, corporate-tax |
| D6 | **One approved requisition can back unlimited POs of any size.** `POST /v1/purchase-orders` checks only that the PR is APPROVED; it does not convert it, cap the PO total at the PR amount, or enforce uniqueness (`idx_purchase_orders_request` is non-unique). | purchase-order |
| D7 | **Executed contract amended without approval.** Amend/Renew on an EFFECTIVE contract rewrite title, value and term (a version snapshot is kept), with no review or approval cycle (LEG-CTRL-011, T13, T15). | contract-lifecycle |
| D8 | **Stub sanctions screening marks vendors VERIFIED.** Screening is an exact-match lookup against a hard-coded name denylist; a pass pushes `VERIFIED` into counterparty-management. | vendor-due-diligence (`handler.go:91-99, 330-349`) |
| D9 | **Spend never released and double-counted.** Consumptions are never released on reject/cancel, and purchase-request keys each check `pr:{id}:v{version}:{cat}`, so amend-and-resubmit consumes the same spend again. | spend-controls ↔ purchase-request |
| D10 | **Events lost on broker failure.** 18 of 20 services publish after commit, swallow the error and have no outbox. Only purchase-request and purchase-order use a transactional outbox. | all except PR, PO |
| D11 | **float64 money and rates** in all 20 services (INV-04; TAX-03 is C0 "exact decimal"). DB columns are NUMERIC in most, so values are rounded in Go. | all |
| D12 | **Idempotency-Key required but not honoured.** The envelope rejects writes without the header, but 15 services never deduplicate on it, so a retried create makes a second row. | all except PR, PO, spend-controls, vendor-DD, procurement-workflow (correlation-based) |

### Operational / doc drift

| Rank | Gap |
| --- | --- |
| O1 | **Host-port collisions** if `docker-compose.yml` and `docker-compose.phase5.yml` run together: 8129 (purchase-order vs withholding-tax) and 8131 (spend-controls vs filing-tracker). The ownership PDF lists these same duplicates. |
| O2 | vendor-due-diligence listens on **8135**; the ownership PDF says 8132 (which is compliance-status). |
| O3 | Event names diverge from Doc 03 (dotted) and from the specs (PascalCase) in 13 services. See each table. |
| O4 | Undocumented surfaces: exception-escalation also serves `/v1/deadlines` (13 ops), `/v1/audit-findings` (13), `/v1/cases` + `/v1/tasks` (16); counterparty-management serves a CRM (`/v1/relationships`, `/v1/opportunities`, `/v1/pipeline`); clause-template duplicates `/v1/templates` as `/v1/clause-templates` (without approve). |
| O5 | counterparty-management has two migrations numbered `000002` (`000002_add_crm_relationship`, `000002_force_rls`). |
| O6 | `purchase-order-svc/progress.md` and `context.md` still describe the v1 ISSUED→CLOSED machine with no CANCELLED status; the code has 8 states. |
| O7 | Stable error vocabularies (LEGAL_CONTEXT_MISSING…, TAX_RULE_NOT_FOUND…) are not used anywhere; responses are `{"error": "<free text>"}`. PR and PO have their own stable codes. |

## 3. Needs clarification (not scored)

| # | Question | Why it blocks a verdict |
| --- | --- | --- |
| C1 | Which spec governs **corporate-actions-svc**? Doc 03 says "officer changes, filings, board-authorized entity events" (LEG-01/02); the code models MERGER, SHARE_ISSUANCE, DIVIDEND… (LEG-03 territory). | Determines whether cap-table arithmetic (T06/T07), officer register and filing attempts are required. |
| C2 | Is **counterparty-management-svc** a party master? LEG-CTRL-001 forbids duplicating ORG-07 party identity; §9 has no row for it. | Decides whether its name/registration/tax-ID fields are allowed at all. |
| C3 | **vat-gst-svc**: TAX-04 (line-level indirect tax calculation, recovery, reverse charge) or a VAT return store? Doc 03 events (`vat.calculated`, `gst.calculated`) say calculation; the code is returns. | Scored ❌ against TAX-04 + Doc 03 as written; if the intent is returns, it should be re-specced under TAX-09. |
| C4 | **corporate-tax-svc**: TAX-06 provision (current/deferred), Doc 03 "estimated tax positions", or returns (as coded)? | Same as C3. |
| C5 | **filing-tracker-svc**: TAX-10 (submission adapter), TAX-12 (obligation calendar), or both? | Scored against both where Doc 03 overlaps. |
| C6 | Spec surfaces `/internal/v1/apNN/...` are labelled "illustrative". Are the `/v1/purchase-requests` and `/v1/purchase-orders` paths acceptable? | Path shape not scored. |
| C7 | No doc gives HTTP status codes or response schemas. Is there an OpenAPI baseline these should be measured against? | Status-code compliance not scored beyond §4's 401. |
| C8 | compliance-status, exception-escalation, spend-controls, vendor-DD and procurement-workflow have no §9 or detailed-spec row (input-contract-conformance G-5 already notes compliance-status). | Their scores measure only Doc 03's purpose + events. |

## 4. Platform controls common to all 20 (not re-scored per service)

| Control | Status |
| --- | --- |
| §4 envelope: tenant/actor/request/correlation/source-channel mandatory; Idempotency-Key and X-Legal-Entity-Id mandatory on writes (`internal/envelope`, write-strict default) | ✅ wired in all 20 |
| Missing `X-Tenant-Id` → 401 (no "default" tenant) | ✅ |
| authz: writes call authorization-svc and fail closed (403 deny / 503 unavailable) | ✅ in all 20, except the gaps listed in S4 |
| `X-Legal-Entity-Id` (envelope-verified) vs body `legal_entity_id` (used for authz): never compared, so the envelope's cross-tenant entity check covers the header, not the entity actually authorized and stored | ⚠️ in services 1–6, 12–20 |
| Purpose context on sensitive writes | ✅ counterparty, VDD, tax-determination, vat-gst, corporate-tax, withholding |

---

## 5. Per-service detail

Each table is the diff (step 3) and the report row (step 4). "Implemented" lists the route or code path; "Documented" names the doc clause.

### 5.1 contract-lifecycle-svc — 8119 — LEG-05

Routes (12): `POST/GET /v1/contracts`, `GET/PUT /v1/contracts/{id}`, `POST …/{id}/submit-review | approve | send-for-signature | record-execution | amend | renew | terminate`, `GET …/{id}/versions`.

| Item | Documented | Implemented | Status | Notes |
| --- | --- | --- | --- | --- |
| CreateContract | LEG-05 cmd; §9 inputs incl. governing law, template, expiry | POST /v1/contracts → 201 DRAFT | ⚠️ | No governing-law, template/clause-set or jurisdiction fields; `legal_entity_id` not required in body |
| CreateVersion | edit only before approval (§7.1) | PUT only in DRAFT/REVIEW, row-locked, snapshot | ✅ | |
| SubmitReview | cmd | DRAFT→REVIEW | ✅ | |
| ApproveContract | GOV approval, self-approval blocked | governance decision verified GRANTED + submitter≠approver (handler and store) | ✅ | |
| SendForSignature | binds exact approved digest (LEG-CTRL-016) | state flag only, no LEG-08 envelope or digest | ⚠️ | LEG-08 does not exist |
| RecordExecution | signer authority as-of (LEG-CTRL-004); digest match (T25) | `signed_by` from body; no authority/digest check | ❌ | S7 |
| Amend | new version, re-approval (LEG-CTRL-011, T13) | rewrites EFFECTIVE contract fields, snapshot only, no approval | ❌ | D7 |
| Renew | controlled event | extends term, no approval | ⚠️ | |
| Terminate | preserves surviving obligations (§7.1, T16) | APPROVED+ only; does not touch obligations | ✅ | |
| Assign/Novate | cmd | — | ❌ | missing |
| Queries | GetContract, GetVersion, CompareVersions, ListParties, GetApprovalTrail, GetExecutedArtifact | GET by id, list, versions list | ⚠️ | 2 of 6 |
| Orthogonal status | §2.2 one-status prohibited | `status` + `signature_status` (migration 000003) | ✅ | |
| Doc 03 constraint | "no contract active without governed approval and signatory validation" | approval yes, signatory no | ⚠️ | |
| Doc 03 events | drafted, submitted, executed, renewal.due, expired | created, submitted_for_review, executed; no renewal.due/expired (no scheduler; EXPIRED unreachable) | ⚠️ | |
| Spec events | Created, Approved, Executed, Amended, Renewed, Terminated | all six as dotted lower-case; payload lacks aggregate_version, evidence_ref, jurisdiction | ⚠️ | |
| Event delivery | outbox (DoD) | post-commit Kafka, error swallowed | ❌ | D10 |
| Actor attribution | actor from verified context (§2.1) | body fields | ❌ | S5 |
| Read authorization | need-to-know | tenant scope only | ⚠️ | |
| Tenant isolation | INV-01 | explicit `tenant_id` predicate | ✅ | |
| Idempotency | §2.1 | header required, not deduped | ❌ | D12 |
| expected_version | §2.1 | not consumed | ❌ | |
| Money | INV-04 | float64 (NUMERIC in DB) | ⚠️ | |
| Stable errors | LEGAL_CONTEXT_MISSING… | free text | ❌ | |
| Counterparty identity | LEG-CTRL-001 canonical ORG id | `counterparty_id` unvalidated | ❌ | |

### 5.2 clause-template-svc — 8120 — LEG-06

Routes (23): `/v1/clauses` (CRUD, submit-legal-review, approve, activate, retire, supersede, versions), `/v1/templates` (CRUD, approve), `/v1/clause-templates` (alias, no approve), `/v1/deviation-rules` (create, list, get, approve).

| Item | Documented | Implemented | Status | Notes |
| --- | --- | --- | --- | --- |
| CreateClause | §9: text, type, jurisdiction, language, owner, period | title, category, body, jurisdiction, dates | ⚠️ | no language/owner |
| CreateClauseVersion | Draft/Review only | PUT guarded DRAFT/LEGAL_REVIEW | ✅ | |
| Legal Review | Draft→Legal Review | submit-legal-review | ✅ | |
| ApproveClause maker-checker | §8 | submitter≠approver (handler + store) | ✅ | |
| Activate | Approved→Active | guarded | ✅ | |
| RetireClause | cmd | Active only | ✅ | |
| Supersede | linked successor | `superseded_by` not validated (may be missing, unapproved or self) | ⚠️ | |
| CreateTemplate | cmd | ✅ | ✅ | |
| ApproveTemplate | maker-checker; missing clause blocks assembly | no SoD; referenced `clause_ids` not checked as approved/active | ❌ | |
| UpdateTemplate | | DRAFT only | ✅ | |
| Deviation rules | ApproveDeviationRule | create + approve with proposer≠approver | ✅ | |
| AI promotion (T17) | LLM text never approved without human review | `authored_by_ai` caller-asserted; approver can be any principal incl. a workload | ⚠️ | |
| Clause text hash | evidence | none | ❌ | |
| Queries | FindApprovedClause, GetClauseAsOf, Compare, GetPlaybook, ListDeviationRules | list/get/versions; deviation list | ❌ | 1 of 5 |
| Doc 03 events | clause.template.updated, template.approved | clause.updated / template.updated, template.approved | ⚠️ | |
| Spec events | ClauseVersionApproved, ClauseRetired, TemplateApproved, DeviationRuleChanged | all four (dotted) | ✅ | |
| Event delivery | outbox | swallowed | ❌ | |
| Actor attribution | | clause/template `created_by` from body; deviation uses principal | ⚠️ | |
| Undocumented alias | — | `/v1/clause-templates` | ⚠️ | O4 |
| Tenant isolation | | explicit predicate | ✅ | |
| Idempotency | | not deduped | ❌ | |
| Read authorization | | tenant only | ⚠️ | |
| Stable errors | | free text | ❌ | |

### 5.3 obligation-tracking-svc — 8121 — LEG-07

Routes (13): `/v1/obligations` CRUD + validate, schedule, mark-due, mark-in-progress, complete, waive, record-breach, dispute, supersede.

| Item | Documented | Implemented | Status | Notes |
| --- | --- | --- | --- | --- |
| CreateObligation | cmd | CONTRACTUAL only (statutory goes to obligations-svc) | ✅ | |
| Source = executed LEG-05 version | required input | `source_id` free text; no contract/version check | ❌ | |
| AI candidate gate (T20) | extraction cannot activate | `extracted_by_ai` → CANDIDATE → validate | ⚠️ | flag is caller-asserted, so omitting it skips the gate |
| Due-date provenance (LEG-CTRL-013/014) | clause + trigger + method | trigger + method required; `source_clause_id` optional; no version | ⚠️ | Doc 03 constraint "clause and version" |
| Due-date edit after schedule | | blocked (CANDIDATE/PLANNED only) | ✅ | |
| MarkDue / InProgress | | manual transitions | ✅ | |
| Complete | completion evidence | no evidence field | ❌ | |
| Waive (T22) | distinct from satisfied; documented authority | distinct state; `waiver_authority_reference` required but not verified | ⚠️ | |
| RecordBreach | no automatic financial effect | fact only | ✅ | |
| Dispute / Supersede | | ✅ | ✅ | |
| Business calendar (REF-10) | | none | ❌ | REF-10 missing |
| Overdue detection | Doc 03 contract.obligation.overdue | no scheduler or route | ❌ | |
| Queries | ListObligations, GetDueSchedule, GetEvidence, GetContractObligationMap, GetOverdueCritical | list/get only | ❌ | |
| Doc 03 events | contract.obligation.created, .overdue | obligation.created; no overdue | ❌ | |
| Spec events | Activated, Due, Satisfied, Breached, Waived | all present | ✅ | |
| Event delivery | | swallowed | ❌ | |
| Actor attribution | | `created_by` body | ⚠️ | |
| Tenant isolation | | explicit | ✅ | |
| Idempotency | | not deduped | ❌ | |
| Stable errors | | free text | ❌ | |

### 5.4 board-resolutions-svc — 8122 — LEG-04

Routes (13): `/v1/meetings` (create, list, get); `/v1/resolutions` (create, list, get, open-voting, vote, close-voting, supersede, roster, votes, quorum).

| Item | Documented | Implemented | Status | Notes |
| --- | --- | --- | --- | --- |
| CreateResolution | cmd | ✅, self-attribution enforced | ✅ | |
| Meeting record | input | meetings CRUD-lite | ✅ | |
| Frozen entitlement population (LEG-CTRL-005, T09) | from directors/shareholders as-of | roster + quorum from caller body, frozen at open | ❌ | S6 |
| CastVote | eligible voters only; no fabrication | roster-only, one vote, voter = principal | ✅ | |
| Conflicts | preserved | not modelled (abstain only) | ❌ | |
| CloseVoting | quorum, evidence | quorum tally + closer≠creator + evidence-requirements-svc | ✅ | |
| Text change after voting (T11) | invalidates votes | no edit endpoint exists | ✅ | |
| ApproveMinutes | cmd | — | ❌ | |
| ExecuteResolution | Passed→Executed→Effective | — | ❌ | no Executed/Effective state |
| SupersedeResolution | | PASSED only; `superseded_by` unvalidated | ⚠️ | |
| Draft state | Draft→Proposed | starts PROPOSED | ⚠️ | |
| Queries | GetResolution, GetVoterPopulation, GetVoteLedger, GetQuorumEvidence, GetResolutionEvidence | first four | ⚠️ | |
| Doc 03 events | drafted, approved, executed | created, passed; no executed | ❌ | |
| Spec events | Proposed, QuorumSatisfied, Passed, Failed, Executed | passed, failed | ⚠️ | |
| Event delivery | | WithoutCancel ctx, logged, no outbox | ⚠️ | |
| Read authorization | | principal + tenant required, no authz call | ⚠️ | |
| Tenant isolation | | explicit + FORCE RLS (000002) | ✅ | |
| Idempotency | | not deduped | ❌ | |
| Stable errors (QUORUM_OR_ELIGIBILITY_FAILED…) | | free text | ❌ | |

### 5.5 corporate-actions-svc — 8123 — Doc 03 §12.5 only

Routes (5): `/v1/corporate-actions` create, list, get, PUT, `/{id}/execute`.

| Item | Documented | Implemented | Status | Notes |
| --- | --- | --- | --- | --- |
| Tenant isolation | INV-01 | id-only SQL; superuser in main compose | ❌ | S1 |
| Governed state change | "governed corporate actions" | PUT accepts `status`; no guard | ❌ | S3 |
| Execute controls | | executor≠creator + evidence-requirements | ✅ | |
| Board authorization basis | board-authorized events; LEG-CTRL-006/030 | does not require BOARD_APPROVED or verify `resolution_id` is PASSED | ❌ | |
| Purpose: officer changes, filings | Doc 03 | M&A/share/dividend/name types only | ❌ | see C1 |
| Share-event arithmetic | LEG-03 (if applicable) | none | ❓ | C1 |
| Doc 03 events | corporate.action.initiated, .completed, entity.filing.submitted | corporate_action.created/.updated/.executed | ❌ | |
| Event delivery | | swallowed | ❌ | |
| Actor attribution | | `created_by`, `executed_by` body | ❌ | |
| Error mapping | | every create failure → 500 | ⚠️ | |
| Idempotency | | not deduped | ❌ | |
| Money | | float64 | ⚠️ | |
| Read authorization | | none | ⚠️ | |

### 5.6 counterparty-management-svc — 8124 — Doc 03 §12.6 only

Routes: `/v1/counterparties` (create, list, get, PUT, `/{id}/compliance`) + undocumented CRM (`/v1/relationships` 14 ops, `/v1/opportunities` 3, `/v1/pipeline`).

| Item | Documented | Implemented | Status | Notes |
| --- | --- | --- | --- | --- |
| Tenant isolation (counterparties) | | id-only SQL; superuser bypasses FORCE | ❌ | S1, PII |
| Create counterparty | owns counterparties | ✅ | ✅ | |
| Risk metadata | owns risk; risk.changed | PUT can set status/risk freely; emits counterparty.updated | ⚠️ | |
| Validation state | owns validation state | `compliance_status` free text, no enum | ⚠️ | |
| Doc 03 events | created, validated, risk.changed | created, compliance_updated, updated | ⚠️ | |
| Party-master duplication | LEG-CTRL-001 | holds name, registration no., tax ID | ❓ | C2 |
| CRM surface | — | relationships/opportunities, tenant-scoped, authz'd | ⚠️ | O4 |
| Purpose context on writes | §4 sensitive | required | ✅ | |
| Event delivery | | swallowed | ❌ | |
| Actor attribution | | `created_by` body | ❌ | |
| Idempotency | | not deduped | ❌ | |
| Migration numbering | | two `000002_*` | ⚠️ | O5 |
| Read authorization | | none on counterparty GET/list | ⚠️ | |

### 5.7 purchase-request-svc — 8100 — AP-02

Routes (12): `/v1/purchase-requests` create, list, get, approval-status, available-actions, history, amend, submit, approve, reject, cancel, convert-to-purchase-order.

| Item | Documented | Implemented | Status | Notes |
| --- | --- | --- | --- | --- |
| Commands (7) | Create, Amend, Submit, Approve, Reject, Cancel, ConvertToPurchaseOrder | all | ✅ | |
| Queries (5) | Get, List, GetApprovalStatus, GetAvailableActions, GetRequisitionHistory | all | ✅ | |
| Paths | illustrative `/internal/v1/ap02/...` | `/v1/purchase-requests/...` | ❓ | C6 |
| Self-approval above threshold | neg #1 | maker-checker + independent of submitter/amender | ✅ | |
| Material amend invalidates approval | neg #2 | APPROVED→re-earn + `approval_invalidated` event | ✅ | |
| Budget check, controlled category | neg #3 | fail-closed spend-controls call | ✅ | |
| Cancelled → PO blocked | neg #4 | convert only from APPROVED | ✅ | |
| Expiry | state model | lazy expiry + worker (000007) | ✅ | |
| Idempotency | | Idempotency-Key replay (`Idempotent-Replay`) | ✅ | |
| expected_version | | header/body honoured | ✅ | |
| Outbox | DoD #5 | transactional outbox | ✅ | |
| Spec events | Created, Submitted, Approved, Rejected, Converted | all (+Cancelled, Expired, Amended) | ✅ | |
| Doc 03 events | purchase.request.created/approved/rejected | all | ✅ | |
| Tenant isolation | | explicit + body tenant mismatch refused | ✅ | |
| Read authorization | | `authorizeRead` | ✅ | |
| Stable errors | §16 | coded errors (own vocabulary) | ✅ | |
| Money | INV-04 | float64 + Round2 | ⚠️ | |
| Budget consumption lifecycle | | double-count on resubmit, never released | ⚠️ | D9 |
| Supplier eligibility (server-resolved) | AP-02 derived context | `preferred_supplier_ref` unvalidated | ⚠️ | |

### 5.8 purchase-order-svc — 8129 — AP-03

Routes (19): `/v1/purchase-orders` (issue, draft, list) and `/{id}` (get, amendments, revisions, revision/{n}, history, open-quantity, receipt-status, invoice-status, available-actions, amend, submit, approve, issue, hold, release-hold, cancel, close, lines/{line}/progress).

| Item | Documented | Implemented | Status | Notes |
| --- | --- | --- | --- | --- |
| Commands (9) | Create … Close | all | ✅ | |
| Queries (6) | Get, GetPORevision, GetOpenQuantity, GetReceiptStatus, GetInvoiceStatus, GetAvailableActions | all | ✅ | |
| Preparer ≠ approver | SoD | preparer and submitter excluded | ✅ | |
| Issued PO revised, not overwritten | neg #1 | new revision rows | ✅ | |
| Re-approval after material change | neg #2 | RequiresReapproval | ✅ | |
| Held supplier blocks issue | neg #3 | AP-01 eligibility + exception permission | ✅ | |
| Supplier mandatory | required input | `supplier_ref` optional; check skipped when absent | ⚠️ | |
| Approved basis consumed once | AP-03 basis; AP-02 conversion | PR checked APPROVED only; no conversion, cap or uniqueness | ❌ | D6 |
| Receipt/invoice > remaining | neg #4 | `ErrProgressExceedsOrder` | ✅ | |
| Cancel with progress | | blocked | ✅ | |
| Idempotency / version / outbox | | all three | ✅ | |
| Spec events | Created, Approved, Issued, Amended, Held, Closed, Cancelled | all | ✅ | |
| Doc 03 events | issued, amended, closed | all | ✅ | |
| Tenant isolation | | explicit | ✅ | |
| Read authorization | | tenant only (by design comment) | ⚠️ | |
| Money | | float64 | ⚠️ | |
| Service docs | progress.md / context.md | stale (v1 machine) | ⚠️ | O6 |
| Port | 8129 | collides with withholding-tax | ❌ | O1 |

### 5.9 spend-controls-svc — 8131 — Doc 03 §12.12

Routes (5): `/v1/spend-policies` (create, list, deactivate, usage), `/v1/spend-checks`, `/v1/spend-consumptions`.

| Item | Documented | Implemented | Status | Notes |
| --- | --- | --- | --- | --- |
| Policy management | owns enforcement logic | create, list, deactivate, usage | ✅ | |
| Enforcement | | atomic evaluate with policy row lock | ✅ | |
| Replay | | correlation-id replay, no duplicate events | ✅ | |
| Authorization | | writes and reads authz'd | ✅ | |
| Doc 03 events | spend.threshold.breached, spend.block.applied | both | ✅ | |
| Currency | | mismatch refused (no FX) | ✅ | |
| Consumption tracking | "consumption tracking" | never released; resubmits double-count | ❌ | D9 |
| Event delivery | | direct Kafka, no outbox | ⚠️ | |
| Tenant isolation | | explicit | ✅ | |
| Money | | float64 | ⚠️ | |
| Port | 8131 | collides with filing-tracker | ❌ | O1 |

### 5.10 vendor-due-diligence-svc — 8135 (doc: 8132) — Doc 03 §12.10

Routes (3): `/v1/vendor-checks` create, list, get.

| Item | Documented | Implemented | Status | Notes |
| --- | --- | --- | --- | --- |
| Check lifecycle | owns validation | STARTED→COMPLETED/FAILED | ✅ | |
| Sanctions/risk state | owns sanctions/risk | hard-coded exact-name denylist; pass → VERIFIED | ❌ | D8 |
| Onboarding checks | "onboarding checks" | sanctions only | ❌ | |
| Evidence | due-diligence evidence | evidence row per check | ✅ | |
| Counterparty sync | | best-effort push, failure only logged | ⚠️ | |
| counterparty_id | | not validated | ⚠️ | |
| Doc 03 events | vendor.dd.started, completed, failed | all | ✅ | |
| Replay | | correlation replay | ✅ | |
| Authorization | | writes and reads | ✅ | |
| Tenant isolation | | explicit | ✅ | |
| Port | 8132 | 8135 | ⚠️ | O2 |

### 5.11 procurement-workflow-svc — 8134 — Doc 03 §12.7

Routes (6): `/v1/procurement-cases` create, list, get, approve, reject, issue-order.

| Item | Documented | Implemented | Status | Notes |
| --- | --- | --- | --- | --- |
| Orchestration | owns orchestration | case → spend check → approve → PO via purchase-order-svc | ✅ | |
| Approval SoD | | requester≠approver; reject | ✅ | |
| PO issuance | | idempotent via correlation | ✅ | |
| Doc 03 events | requested, approval.started, completed | all | ✅ | |
| Governed spend routing | | spend-controls yes; vendor DD/supplier eligibility not consulted | ⚠️ | |
| Decision events | | approve/reject emit nothing | ⚠️ | |
| Tenant isolation | | explicit | ✅ | |
| Idempotency | | correlation-based | ✅ | |
| Error hygiene | | raw downstream `err.Error()` returned to caller | ⚠️ | |
| Money | | float64 | ⚠️ | |

### 5.12 tax-rules-svc — 8125 — TAX-02

Routes (4): `/v1/tax-rules` create, list, get, PUT.

| Item | Documented | Implemented | Status | Notes |
| --- | --- | --- | --- | --- |
| Tenant isolation | | id-only SQL, superuser | ❌ | S1, S2 |
| Runtime cannot modify contents | §TAX-02 | PUT edits rate/exemptions/status | ❌ | D2 |
| Certification/signature/citation | evidence | none | ❌ | |
| Author/reviewer/releaser separation | SoD | none | ❌ | |
| Bind/Activate/Suspend/Promote/Rollback | cmds | status via PUT | ❌ | |
| Overlap quarantine | failure semantics | none | ❌ | |
| GetRulePackAsOf | query | status filter only | ❌ | |
| Provenance/versions/compatibility | queries | none | ❌ | |
| Doc 03 events | tax.rule.updated, tax.rule.activated | tax_rule.created/.updated | ❌ | |
| Spec events | Bound, Activated, Suspended, Superseded | none | ❌ | |
| Basic CRUD | | create/list/get | ✅ | |
| Write authorization | | authz (tenant as scope) | ✅ | |
| Rate type | exact decimal | float64 | ⚠️ | |
| Event delivery | | swallowed | ❌ | |

### 5.13 tax-determination-svc — 8126 — TAX-03

Routes (4): `/v1/tax-determinations` determine, list, get, `/{id}/override`.

| Item | Documented | Implemented | Status | Notes |
| --- | --- | --- | --- | --- |
| §9.J inputs | seller/buyer, supply location/date, classification, B2B/B2C, exemption | required/validated (migration 000003) | ✅ | |
| Registrations server-resolved | | tenant-entity-registry, effective-dated | ✅ | |
| Jurisdiction verification | | fail-closed | ✅ | |
| Missing rule → Blocked (inv. 3) | | 0% fallback on any rule error | ❌ | D1 |
| As-of rule resolution | historic replay | ACTIVE-only lookup | ❌ | |
| Overlapping versions | quarantine | `Rules[0]` | ❌ | |
| Override (SoD, supersede) | independent approval; never overwrite consumed | in-place amount overwrite; authz only | ❌ | |
| Rule snapshot | evidence | `tax_logic_snapshot_id` | ✅ | |
| Place of supply | server-derived | caller-asserted, recorded as such | ⚠️ | |
| Rounding | server-resolved | none (REF-07 missing) | ❌ | |
| Commands | Determine, Redetermine, Explain, Supersede, ValidateForIssue | Determine only | ❌ | |
| Queries | Get, Breakdown, Explanation, AsOf, RuleTrace | get/list; snapshot is a partial trace | ⚠️ | |
| Doc 03 events | tax.determined, tax.determination.failed | tax_determination.calculated/.overridden | ❌ | |
| Spec events | Determined, Validated, Superseded, ExceptionRaised | calculated only | ❌ | |
| Exact decimals | C0, inv. 4 | float64 | ❌ | |
| Event delivery | | swallowed | ❌ | |
| Tenant isolation | | explicit | ✅ | |
| Idempotency per transaction | | non-unique (tenant, transaction_id) index | ⚠️ | |

### 5.14 vat-gst-svc — 8127 — TAX-04

Routes (5): `/v1/vat-returns` create, list, get, PUT, `/{id}/file`.

| Item | Documented | Implemented | Status | Notes |
| --- | --- | --- | --- | --- |
| Scope | indirect-tax calculation for transactional flows | period VAT returns | ❌ | C3 |
| CalculateIndirectTax, AssessRecovery, AssessReverseCharge, Reclassify, Supersede | cmds | none | ❌ | |
| Derived amounts | from TAX-03 results / ledger | caller-typed totals | ❌ | |
| Filed return immutable (T23) | | PUT unguarded on FILED | ❌ | D3 |
| File | | duplicate filing blocked; `filed_by` body | ⚠️ | |
| One return per period | | unique, 409 | ✅ | |
| Doc 03 events | vat.calculated, gst.calculated, indirect.tax.exception.detected | vat_return.* | ❌ | |
| Tenant isolation | | explicit | ✅ | |
| Authorization | | writes | ✅ | |
| Money | | float64 | ⚠️ | |
| Event delivery | | swallowed | ❌ | |

### 5.15 corporate-tax-svc — 8128 — TAX-06

Routes (6): `/v1/corporate-tax-returns` create, list, get, PUT, submit, assess.

| Item | Documented | Implemented | Status | Notes |
| --- | --- | --- | --- | --- |
| Scope | provision (TAX-06) / estimate (Doc 03) | returns | ❌ | C4 |
| Rates from certified rules | inv. 2 | `tax_rate_percent` from caller | ❌ | D5 |
| Immutability after submit | | PUT unguarded | ❌ | D3 |
| Preparer ≠ approver | SoD | no approve step | ❌ | |
| Temporary differences / deferred tax | ownership | none | ❌ | |
| Submit / Assess lifecycle | | authz'd | ✅ | |
| Doc 03 events | corporate.tax.estimated | corporate_tax_return.* | ❌ | |
| Tenant isolation | | id-only SQL, superuser | ❌ | S1 |
| Write authorization | | yes | ✅ | |
| Event delivery | | swallowed | ❌ | |
| Money | | float64 | ⚠️ | |

### 5.16 withholding-tax-svc — 8129 — TAX-05

Routes (7): `/v1/withholding-tax` calculate, create, list, get, PUT, remit, cancel.

| Item | Documented | Implemented | Status | Notes |
| --- | --- | --- | --- | --- |
| DetermineWithholding | cmd | `/calculate`, authz'd | ✅ | |
| Certified rate | rule pack | caller-supplied; tax-rules URL unused | ❌ | D5 |
| Treaty validation / override approval (T11, T12) | | flag + any cert string → 0 | ❌ | D5 |
| Remitted immutable | | PUT unguarded | ❌ | D3 |
| Remit / cancel guards | | no double remit; no cancel after remit | ✅ | |
| Certificate issue, treaty trace | cmd/query | none | ❌ | |
| Doc 03 event | withholding.tax.calculated | withholding_tax.calculated | ⚠️ | |
| Spec events | Determined, LiabilityCreated, Remitted, CertificateIssued | calculated, created, remitted | ⚠️ | |
| Tenant isolation | | explicit | ✅ | |
| Money | | float64 | ⚠️ | |
| Event delivery | | swallowed | ❌ | |
| Port | 8129 | collides with purchase-order | ❌ | O1 |

### 5.17 filing-preparation-svc — 8130 — TAX-09

Routes (6): `/v1/filing-preparation/drafts` create, list, get, PUT, validate, finalize.

| Item | Documented | Implemented | Status | Notes |
| --- | --- | --- | --- | --- |
| Create / read | | ✅ | ✅ | |
| Evidence completeness | Doc 03 | Validate → evidence-requirements-svc | ✅ | |
| No finalize without validation | | DRAFT→READY allowed | ❌ | D4 |
| Population change invalidates approval | | PUT unguarded after READY | ❌ | D3 |
| Preparer ≠ approver | SoD | none | ❌ | |
| Frozen population, schema version, line derivation | | none | ❌ | |
| Amend / Supersede | | none | ❌ | |
| Doc 03 events | filing.prepared, filing.blocked, filing.ready.for.submission | all | ✅ | |
| Tenant isolation | | RLS-only; phase5 role is NOBYPASSRLS | ⚠️ | breaks if run as superuser |
| Write authorization | | yes | ✅ | |
| Event delivery | | swallowed | ❌ | |

### 5.18 filing-tracker-svc — 8131 — TAX-10 / TAX-12

Routes (7): `/v1/filing-tracker/requirements` create, list, get, PUT, submit, confirm, mark-overdue.

| Item | Documented | Implemented | Status | Notes |
| --- | --- | --- | --- | --- |
| Requirement create/read | | ✅ | ✅ | |
| Rule-derived due date (TAX-12) | | caller-supplied | ❌ | |
| Submit approved return only | TAX-10 input | no link to READY draft | ❌ | |
| Durable attempt, Pending/Unknown, Rejected | TAX-10 states | Submitted/Confirmed/Overdue only | ❌ | |
| Authority confirmation evidence | | caller-asserted reference | ⚠️ | |
| Overdue | | manual endpoint, no scheduler | ⚠️ | |
| Doc 03 events | filing.due, submitted, confirmed, overdue | all | ✅ | |
| Update scope | | notes only | ✅ | |
| Authorization | | writes yes, reads no | ⚠️ | |
| Tenant isolation | | RLS-only (phase5 role) | ⚠️ | |
| Port | 8131 | collides with spend-controls | ❌ | O1 |
| Event delivery | | swallowed | ❌ | |

### 5.19 compliance-status-svc — 8132 — Doc 03 §13.7

Routes (6): `/v1/compliance-status` evaluate, list, get, gaps (create, list, resolve).

| Item | Documented | Implemented | Status | Notes |
| --- | --- | --- | --- | --- |
| Evaluate | explainable status | authz'd | ✅ | |
| Basis | "for obligations, controls, readiness" | counts supplied by caller, not read from obligation/control services | ❌ | |
| Gaps | gap.detected | create/list/resolve | ✅ | |
| Doc 03 events | compliance.status.changed, compliance.gap.detected | both (+gap.resolved) | ✅ | |
| "changed" semantics | | emitted on every evaluate even if unchanged | ⚠️ | |
| Tenant isolation | | RLS-only (phase5 role) | ⚠️ | |
| Event delivery | | swallowed | ❌ | |
| Read authorization | | partial | ⚠️ | |

### 5.20 exception-escalation-svc — 8133 — Doc 03 §13.9

Routes: `/v1/exception-escalation` (exceptions create/list/get/escalate/resolve, escalations list) + undocumented `/v1/deadlines` (13), `/v1/audit-findings` (13), `/v1/cases` (3), `/v1/tasks` (13).

| Item | Documented | Implemented | Status | Notes |
| --- | --- | --- | --- | --- |
| Exception cases | owns cases | create/list/get | ✅ | |
| Escalation routing | owns routing | manual role + reason; no routing rules | ⚠️ | |
| Close SoD | | creator≠resolver | ✅ | |
| Doc 03 events | exception.created, .escalated, .closed | all | ✅ | |
| Scope | exceptions only | deadlines, audit findings, tasks/cases also hosted | ⚠️ | O4 |
| Audit-finding authorization | | 8 writes unguarded | ❌ | S4 |
| Tenant isolation | | RLS-only; finding_store queries lack tenant predicate | ⚠️ | |
| Event delivery | | direct Kafka, no outbox | ❌ | |
