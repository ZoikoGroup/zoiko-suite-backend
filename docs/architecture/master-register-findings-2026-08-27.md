# Master Engineering Register — Findings Against the Codebase

**Date:** 2026-08-27
**Source:** 44-document "ZoikoSuite Master Wireframe & Engineering Standards Register" (dated 24 Aug 2026), delivered as `.docx` files in `docs/original_doc/`, plus 3 supplementary documents outside the numbered register. All 47 content documents were read in full (converted to text via pandoc; originals are `.docx`, untracked per `.gitignore`).

**Purpose of this document:** consolidate findings from a 9-agent parallel review of every document against the current codebase and `backend-completion-tracker.md`. This is a findings record, not a tracker — items below should be triaged into the tracker (new rows, or corrections to existing ones) as a separate step.

**How to read this:** severity-ordered, not document-ordered. The register itself is a huge scope expansion — most of its 44 documents describe service domains that are partially or entirely unbuilt (this is normal for a 24-Aug delivery against an ~86-service platform still mid-build). The interesting findings are the ones below: places where the new spec reveals something is **actively wrong**, not just **not yet built**.

---

## 0. The pattern worth naming: fabricated success

Three unrelated services were found, independently, to report a positive outcome that did not actually happen. This is a distinct and more serious category than a missing tenant check — a missing check makes a request *fail*; these make a false claim *succeed*.

| Service | What it fabricates | Evidence |
|---|---|---|
| `tax-authority-interface-svc` | A tax authority acknowledgement | `SubmitTaxFiling` hardcodes `AckReference: "TAX-ACK-991823"` — a string literal. No real transmission, no durable attempt record, no pending/unknown state, no callback. Every filing gets the same fake ack regardless of whether anything was sent. |
| `reconciliation-intelligence-svc` | A reconciliation match/write-off decision | `PerformIntelligentReconciliation` uses hardcoded magic-number thresholds (`discAmount < 50.0 → write-off`) and fixed confidence scores (85.0/60.0/90.0) baked into Go code — not versioned, governed rules. |
| `reporting-orchestration-svc` | A completed report | `OrchestratReportRun`'s own code comment: *"Simulates the cross-service data aggregation... In production this would fan-out requests to data source services."* Looks up a hardcoded row count by report type, always returns `COMPLETED`. |

The new spec names these exact anti-patterns explicitly and independently for each service ("transport success is not filing acceptance"; "operator manually sets report state COMPLETE during incident → no bypass, state derived from execution evidence"; tolerance/rule versioning requirements). None of these were caught by the tenant-isolation or authorization sweeps, because the routes work, return 200, and have correct tenant scoping — the problem is what they return is false.

**Recommendation:** treat this as its own tracker priority, ranked above the domain-coverage gaps below. A fabricated regulatory acknowledgement in particular has real compliance exposure if anyone downstream trusts it.

---

## 1. Findings that change or resolve open tracker questions

### 1.1 `policy-svc`'s Priority 1c candidate hits are false positives — resolved, no fix needed
Doc `ZS-SVC-V-001` (Policy/Rules/Decision/Jurisdictional Applicability), §10.1/§11.1/§25, requires tenant_id be sourced from auth context and any caller-supplied value only *compared*, never trusted. Read directly: every `req.TenantID` occurrence in `policy-svc/internal/handler/handler.go` passes through `refuseForeignTenant(w, req.TenantID, verifiedTenant)` — 403 on disagreement, never an override. This is exactly the doc's mandated pattern, and matches the precedent already established for `ai-governance-svc`. **No code change indicated.** Close the Priority 1c candidate row for this service.

### 1.2 `policy-svc.Evaluate` has a real correctness bug — FIXED (2026-08-28)
Same document, §13.2/§14.2 (PDC-I-11): "binary floating-point is not used for material monetary... calculations." `policy-svc`'s `Evaluate` implemented `APPROVAL_THRESHOLD` via a Go `float64` comparison against a monetary value.

Fix: added `decodeExactAmount`, which decodes `threshold_amount`/`amount` via `json.Decoder.UseNumber()` and parses the literal decimal text with `big.Rat.SetString` — no binary-floating-point rounding step exists in the comparison at all. Regression test `TestEvaluate_LargeAmount_PrecisionNotLost` demonstrates a real, reproducible divergence at large magnitudes: `10000000000000000` and `10000000000000001` are the identical float64 bit pattern (adjacent doubles are >1 apart at that scale), so the old code reported `WITHIN_THRESHOLD` when the exact decimal answer is `APPROVAL_REQUIRED`. Negative control (reverting to a float64 comparison) confirmed the new test fails with exactly that wrong outcome, then confirmed clean again on restore.

**Scope note, not fixed here:** every financial service in this platform (`general-ledger-svc`, `accounts-payable-svc`, `accounts-receivable-svc`, ...) stores `Amount` as `float64` — this is the estate-wide convention, not an isolated bug. This fix guarantees only that policy-svc's own comparison introduces no *additional* binary-floating-point error; it does not make money exact end to end, since the value arriving over the wire may already have been produced by an upstream float64-typed field. Making money exact platform-wide would be its own estate-wide type migration, out of scope here.

### 1.3 `workflow-history-svc`'s fix is confirmed complete and doctrinally correct
Doc `ZS-SVC-R-001` (Workflow/Approval/Case/Obligation Control), §11.1/§11.3/R-INV-22, requires tenant resolved from verified request context, checked at API boundary *and* enforced again by RLS at persistence — "cross-tenant identifier substitution" is named as a threat with the exact control shape already implemented (`requireTenant` + RLS via `set_config`). The doc's own language ("rejected at service boundary and again at persistence boundary") mirrors the tracker's own phrasing ("the policy was SATISFIED, not bypassed"). No further action needed on the fix itself — see §2.3 below for new schema obligations surfaced alongside it.

### 1.4 `jurisdiction-rules-svc`'s platform-wide (no `tenant_id`) design is confirmed correct, twice over
`ZS-JUR-001` §"Non-Bypassable Jurisdiction-Pack Invariants" and `ZS-SVC-U-001` §11.2 both independently state jurisdiction/reference facts are MDM-owned, platform-wide artifacts, not tenant data. Matches tracker rows 4/96 exactly.

### 1.5 `search-indexer-svc` row 65a's diagnosis is confirmed by three independent documents
`ZS-SVC-AB-001` §5.1 states the canonical ingestion contract explicitly: durable domain events only, keyed by `tenant + source_ref + source_version`, tenant from the trusted event envelope — no HTTP-polling ingestion mode exists in the spec at all. `ZoikoSuite_Data_Search_Analytics_AI` and the older `ZS-ARCH-SVC-001` baseline ("Search is not authority") independently state the same doctrine. This closes any ambiguity: the fix for row 65a is event subscription, not a header fix, not a new privileged cross-tenant lookup endpoint.

---

## 2. New security/correctness findings (not previously on the tracker)

### 2.1 `accounts-receivable-svc` — CORRECTED, false positive (2026-08-28)
Originally reported as having zero action-level authorization on writes. **Verified directly and this is wrong** — the sixth grep-based false positive of this shape in the workstream. `CreateInvoice`, `SendInvoice`, `MarkOverdue`, and `ReceivePayment` all call `h.authz.CheckAllowed(...)` with real action constants (`AR_INVOICE_ISSUE`, `AR_INVOICE_SEND`, `AR_MARK_OVERDUE`, `AR_PAYMENT_RECEIVE`) — `SCREAMING_SNAKE_CASE`, same platform convention as every other service, not the new spec's dotted-lowercase (`invoice.approve`) or a literal `authorize(` call, which is what the original grep was looking for. The deny path is genuinely tested (`TestCreateInvoice_AuthorizationDenied_Returns403` and three more). Reads (`ListInvoices`/`GetInvoice`) are tenant-scoped only, with an explicit code comment stating this is "the settled posture of the two services either side of this one in the Finance domain — general-ledger-svc... and bank-reconciliation-svc. There is deliberately no AR_INVOICE_VIEW action." **No code change needed. Close this item.**

### 2.2 `banking-connector-svc` stores and returns bank account numbers and IBANs in plaintext, and one route has zero tenant check

**Tenant-check half — CORRECTED, false positive (2026-08-28).** The seventh grep-based false positive of this shape in the workstream. `GetConnectionByID`'s handler indeed takes a bare `id` with no explicit tenant parameter passed at the call site — misleading on its own — but `pg_store.go`'s `GetConnectionByID` pulls the tenant from context via `middleware.GetTenantID(ctx)` and enforces `AND tenant_id = $2` in SQL, matching the Priority 1b fix (`02421f7`) already applied to this service. `TestGetConnectionByID_ForeignTenant_NotFound` in `tenant_scope_test.go` already pins this. **No code change needed for the tenant-check claim.**

**Masking half — CONFIRMED and FIXED (2026-08-28, commit pending).** `AccountNumber`/`IBAN` were returned raw from `GetConnectionByID` and `ListConnections`, with no masking anywhere — `ZS-SVC-E-001` invariant #2 requires masking/tokenization of these fields in list/read APIs. Worse, `populateAliases` actively widened exposure by copying `AccountNumber` into `IBAN` whenever `IBAN` was empty. Fix: added `maskDigits`/`maskSensitiveFields` in `internal/handler/handler.go`, applied after `populateAliases` in `GetConnectionByID` and `ListConnections` only — everything but the last 4 characters becomes `*`. `CreateConnection`'s own response is deliberately left unmasked: it echoes the value back to the same caller who just submitted it, so masking there adds no security benefit while risking client breakage. The published `banking.connection.created` event still carries the unmasked struct as payload (Create's internal `conn`, before masking is applied) — that is a separate, wider question (event consumers across the platform, some of which may have a legitimate reconciliation need for the real number) that was not addressed here and should get its own review before deciding whether to mask event payloads too. Regression tests added in `masking_test.go`; negative control (temporarily removing the masking call) confirmed the new tests fail with the expected leak message, then confirmed clean again on restore.

Invariant #3/#4 (consent/credential-reference model — OAuth scopes/expiry/revocation vs. the current static `CONNECTED`/`DISCONNECTED` status) and the `BankAccount`/`BankConnection` struct-conflation finding are unrelated to the masking/tenant items above and remain open — out of scope for this pass (feature build, not a fabrication/security fix).

### 2.3 `evidence-manifest-svc` never checks legal-hold state before exporting — RE-SCOPED, needs a product decision before implementation (2026-08-28)

**Original citation was the wrong invariant.** DRC-I10 ("legal hold blocks ordinary disposition for every resolved target") governs *disposition* — delete/anonymize — not export. Legal hold's actual purpose is to *preserve* evidence, which cuts the other way for AUDIT/REGULATOR/LEGAL_DISCOVERY/COMPLIANCE_REVIEW manifests: a hold is a reason evidence must remain producible, not a reason to block producing it. The provision that actually applies is `ZS-SVC-S-001` Appendix C, "Evidence Manifest Minimum": the manifest's required `Policy` field group is "classification, retention schedule/version, **active holds/restrictions**" — a disclosure requirement (the manifest must record what holds apply to its contents), not a block-on-export requirement.

**Why this isn't implemented yet.** Disclosing hold status requires calling `retention-registry-svc`'s `/v1/retention/resolve`, which takes a mandatory `record_class` (plus `entity_ref`/`tenant_id`) and matches holds using nil-as-wildcard semantics (`compatibleStr` in `pg_store.go`: a hold's own `record_class`/`entity_ref` of `nil` matches any query, but a query naming the wrong `record_class` will silently miss a hold scoped to the real one). `evidence-manifest-svc`'s source records are governance decisions, access decisions, and workflow instances — none of these map to any `record_class` retention-registry-svc knows about, and no such mapping exists anywhere else in the codebase (`grep -rn record_class` across all services' domain packages returns nothing outside retention-registry-svc itself). Inventing a placeholder `record_class` to make the call succeed would produce exactly the "fabricated success" defect shape this fixing pass exists to remove: a check that runs, returns an answer, and gives false confidence while actually verifying nothing, because it can only ever match holds nobody scoped to that made-up class.

**What would need to be decided first**, by whoever owns the evidence/retention domain: either (a) retention-registry-svc gains a new query mode — resolve by `entity_ref` alone, ignoring `record_class` — which is a cross-service contract change to a service outside this one, or (b) evidence-manifest-svc's three source types are given a real, specified mapping onto retention record classes. Both are legitimate feature work, not a bug fix in this service alone. No code changed for this item.

### 2.4 `retention-registry-svc`'s `GetLegalHold` has zero authorization (self-documented gap from earlier work) — FIXED (2026-08-28)
Confirmed by the QA/data-governance doc review — the code comment already said this was deliberately deferred rather than invented. Fix: added a `LegalHoldRead` action constant and a fetch-then-authorize check (same order as `ReleaseLegalHold` — the store already collapses a foreign-tenant hold into the same not-found error as a genuinely absent one, so authorizing first against a caller-supplied scope would let a caller probe for holds outside a grant they do hold). Tests added in `get_legal_hold_authz_test.go` (authorized read, denied read, missing-principal 401); negative control (bypassing the new authorize gate) confirmed the denied-read test fails with the expected message, then confirmed clean again on restore.

### 2.5 Platform-wide action-naming convention mismatch — REVIEWED, no change (2026-08-28)
Every numbered spec document mandates dotted-lowercase action names (`invoice.approve`, `journal.post`, `period.close.approve`). Every service in the codebase uses `SCREAMING_SNAKE_CASE` (`AP_INVOICE_APPROVE`, `TAX_INTERFACE_CREATE`). Internally consistent, so functionally harmless today — but it means no service's action strings match what any of the 44 docs literally specify, which will matter the moment anyone tries to audit "what permission does X require" against the documentation.

**Decision (2026-08-28): leave as-is.** Renaming would touch every service's action constants, every SoD rule row, every test asserting a specific action string, and every `authorization-svc` permission bundle already in the database — an estate-wide rename for a cosmetic-naming fix, not something to fold into ad-hoc edits. If this is ever undertaken, it should be its own tracked initiative with a migration plan, same posture as §5's event-envelope/API-contract migrations.

### 2.6 `authorization-svc` has no dynamic Segregation-of-Duties layer at all — FIXED (2026-08-28)
`ZS-IAM-001` §10.2 requires dynamic SoD checks — e.g., a preparer cannot approve their own object (`resource.preparer_id == subject_id AND action == approve → DENY`). `authorization-svc` only checked static action-pairs held simultaneously by a principal; it had no relationship/ownership predicate evaluation and no `resource_attributes.preparer_id`-equivalent input at all.

Fix: added `resource_owner_principal_id` (optional) to `/v1/authorize`'s request — the first resource-attribute input this endpoint has ever accepted. When supplied and equal to `principal_id`, and a data-declared rule marks `action_type` own-object-forbidden, the decision is DENIED with basis `sod:own_object_forbidden`. The rule itself needed no new table or column: `conflict_type` is already documented as data-only, so `domain.ConflictTypeOwnObjectForbidden` ("OWN_OBJECT_FORBIDDEN") is expressed as a self-referential `sod_rules` row (`action_a == action_b`) — a new `CheckOwnObjectSoD` store query answers this distinct question, since the existing `CheckSoDConflict` query structurally cannot see it (it excludes the candidate action from the caller's held-actions set before searching). Own-object denials now share the `sod:` basis prefix with static conflicts, so they get the same elevated SIEM severity and the same `sod.violation.detected` event. Omitting the new field preserves today's behavior exactly — no own-object check is attempted. Tests added in `own_object_sod_test.go` (denied, different-owner-not-blocked, field-omitted-skips-check, store-failure-fails-closed) plus a real-DB integration test in `pg_store_test.go`; negative control (bypassing the new gating condition) confirmed the denial test fails with the expected message, then confirmed clean again on restore.

Full attribute-condition ABAC beyond this one own-object case remains out of scope — no other attribute-condition rules exist anywhere in the architecture docs to encode.

### 2.7 `authorization-svc`'s fail-closed posture on PDP-unavailable — DECIDED, no change (2026-08-28)
`ZS-IAM-001` §7/§19 states an unavailable policy source "results in DENY" (a recorded decision). Current code returns a bare `503` without recording a DENIED decision, on the reasoning that "cannot evaluate" and "evaluated and denied" are distinct outcomes callers should be able to tell apart. Functionally similar effect (caller can't proceed either way) but the letter of the two approaches differs.

**Decision (2026-08-28): keep the current 503 behavior.** Switching to a recorded DENIED would be a real behavior change for every caller of `/v1/authorize` — a lookup failure would start looking identical to "you lack permission" rather than "we couldn't check," in every one of the roughly 86 services calling this endpoint. Not undertaken.

---

## 3. New obligations with zero tracker presence — entirely new priority tiers, not gap-fills

These aren't bugs in existing code — they're domains the new register specifies that have no implementation footprint at all. Listed for backlog/roadmap triage, not as fix rows.

- **Data residency** (`ZS-SEC-001` §7) — a full `ResidencyPolicy` model, 5-tier data classification, cross-region transfer gating. Zero codebase presence.
- **Crypto-agility/PQC readiness, supply-chain security (SBOM/artifact signing), BYOK/HYOK key custody** (`ZS-SEC-001` §12/§19/§23) — all named as baseline invariants, not optional.
- **AP-07 Expense Claim** — confirmed absent estate-wide; no `expense-*-svc` exists anywhere.
- **Assets, Inventory & Project Accounting** (`ZS-SVC-G-001`) — an entire 12-service domain (fixed assets, depreciation, inventory movement/valuation, project cost capture) with zero corresponding service anywhere in the 86-service repo.
- **Most of Financial/Statutory/Regulatory Reporting** (`ZS-SVC-H-001`) — no XBRL/iXBRL, no Statutory Pack, no Financial Statements certification, no Regulatory Submission, no tamper-evident export/evidence package. `reporting-orchestration-svc` (see §0 above) only weakly resembles two of the seven required sub-services, and what exists is a stub.
- **Privacy/Consent/Data Rights** (`ZS-SVC-W-001`) — FULLY BUILT (2026-08-29), v1 scope: see §3.1–§3.5 below. All five services now exist: PRV-01 (`privacy-purpose-registry-svc`), PRV-02 (`privacy-consent-svc`), PRV-03 (`privacy-decision-svc`), PRV-04 (`privacy-rights-svc`), PRV-05 (`privacy-transfer-svc`). None of the five is deployed against a live stack, and each has a documented, deliberate gap where the full spec requires a PDC (jurisdiction-specific legal-rule) engine that doesn't exist anywhere in this codebase — see each service's own section for exactly what that means for it.
- **Full AP-01/04/08/09/10/11** (Supplier Financial Profile, Goods Receipt, Payables, Payment Proposal/Authorization/Run) — only requisition/PO/invoice/matching exist; the payment side of AP is unbuilt.
- **FP&A domain** (`ZS-SVC-J-001`) — `forecasting-svc` is a standalone statistics microservice; none of Budget/Forecast-with-approval/Scenario/Variance/KPI-Registry/Management-Accounts/Board-Pack exist as governed objects.
- **Audit & Assurance engagement domain** (`ZS-SVC-K-001`) — no engagement/risk-assessment/sampling/workpaper/sign-off service exists; `audit-event-store-svc` is correctly-scoped infrastructure this domain would consume, not an implementation of it.
- **AI Governance breadth** (`ZS-SVC-X-001`) — `ai-governance-svc` implements a slice (registry + allowlist); the full model-version pinning, RAG retrieval controls, human-oversight state machine, and evaluation/kill-switch pipeline (60 negative-path scenarios, 30 invariants) don't exist.
- **Notification/Communications breadth** (`ZS-SVC-Y-001` + the separate `Email_Communications_System` doc) — `notification-svc` is a flat Channel/Status struct; no template registry, no suppression/preference model, no delivery-evidence tiers (sent vs delivered vs acknowledged vs legally served), no durable-attempt/idempotency model. The Email doc is architecturally far more mature than what's built and should be the primary reference when this gets built.
- **Enterprise Search query/authorization side** (`ZS-SVC-AB-001`) — beyond the already-known ingestion bug (row 65a), there is **no query service at all** — `search-indexer-svc` is ingestion-only, no ESR-03/04/05 (query gateway, re-authorization/redaction, index lifecycle) exists.
- **Platform Commercial Billing** (`ZS-SVC-Q-001` COM-05) — `commercial-account-svc` covers subscriptions/entitlements but has no invoicing/payment-collection/dunning-execution/reconciliation service.
- **Corporate governance breadth** (`ZS-SVC-O-001`) — `corporate-actions-svc` and `counterparty-management-svc` have **zero** tracker coverage despite the doc imposing real invariants (ownership reconciliation, quorum/entitlement freezing) — never audited for tenant isolation at all, unlike every other service touched this session.
- **Business Operations & Content Services domain** (`ZS-SVC-P-001`) — document management, CRM, task/case, catalog, deadline services: no tracker rows reference any of these by name.

### 3.1 `privacy-purpose-registry-svc` — new service, PRV-01 of ZS-SVC-W-001 (2026-08-28)

Built as a real, working v1 — not a scaffold — of PRV-01 ("Processing Activity, Purpose & Lawful-Basis Registry"), the first of the five services `ZS-SVC-W-001` specifies and the one its own §35 eight-wave sequence names as the first buildable service (PRV-02 consent, PRV-03 runtime purpose-binding decisions, PRV-04 data rights/DSR, and PRV-05 transfer/processor control all depend on this registry existing first).

**What's real:**
- Two independently-versioned registries (`purposes`/`purpose_versions`, `processing_activities`/`processing_activity_versions`), same stable-identity + immutable-once-published shape used throughout this platform.
- The full processing-activity lifecycle from Figure 4: `DRAFT -> VALIDATED -> SUBMITTED -> APPROVED -> ACTIVE`, with a real reject/fix-loop branch (`SUBMITTED -> REJECTED`, dead end — the fix is a new version via `supersedes_version_id`, never a resurrection) and `SUSPENDED`/`RETIRED` branches. Every transition is atomically WHERE-guarded at the store layer and authorized against its own action.
- Real structural validation (PRV-001/004/019-coded findings): an activity referencing an unpublished or nonexistent purpose fails validation and stays DRAFT (PRV-I13 — never a silent PERMIT), verified end-to-end through the handler.
- Database-enforced immutability: published purpose versions (PRV-I06) and DRAFT-exited activity-version content (PRV-I20) are both protected by `BEFORE UPDATE` triggers, not just application code — same doctrine as `evidence-manifest-svc`'s `reject_mutation()`.
- Full RLS tenant isolation, including the nullable-tenant-is-platform-wide convention this platform uses elsewhere (`retention-registry-svc`'s `retention_policies`/`legal_holds`).
- `GET .../{id}?as_of=` historical resolution and `GET /privacy/ropa` (the role/jurisdiction-filtered processing inventory §9.1 names).
- Handler test suite covers the full lifecycle, illegal-transition rejection, tenant-mismatch refusal, authorization denial, and the reject/fix-loop; two negative controls (immutability guard, APPROVED-gate on activation) were run and confirmed to genuinely fail before being restored.

**What's deliberately NOT built, and why (see `internal/domain`'s package doc comment for the full statement):**
- **Validate is structural only.** It does not evaluate jurisdiction-specific legal correctness — `ZS-SVC-W-001` §0 is explicit that "jurisdiction-specific production rules require approved PDC packages," and no such package or service exists anywhere in this codebase. Faking a legal-correctness check would have been exactly the fabricated-success shape this whole workstream exists to remove.
- **No real workflow-svc orchestration behind Submit/Approve/Reject.** These are real, authorized, audited transitions — SUBMITTED is never silently treated as APPROVED — but there is no governed WFC engine wired in. This is an honest, simpler approval gate, not a fabricated one.
- **`lawful_basis_refs`/`retention_rule_refs`/`transfer_refs` are opaque, unvalidated references** — same doctrine as `retention_policies.record_class`: recorded, not resolved against any registry, because none exists yet.
- All five ZS-SVC-W-001 services now exist — see §3.5 for PRV-05, the last one built.

Registered in `docker-compose.yml`/`init-db.sh` (port 8151, db `privacy_purpose_registry`) but not started or smoke-tested against a live stack in this session, consistent with this session's minimal-Docker-footprint discipline — build/vet/test all pass against the in-memory stub store; RLS/immutability were verified by reading the migration SQL against this platform's proven patterns, not by running a live Postgres instance.

### 3.2 `privacy-consent-svc` — new service, PRV-02 of ZS-SVC-W-001 (2026-08-28)

Built as a real, working v1 of PRV-02 ("Notice, Consent & Preference Evidence Service"), the second of ZS-SVC-W-001's five services, directly following §3.1's PRV-01.

**What's real:**
- `notices`/`notice_versions` (`DRAFT -> APPROVED -> PUBLISHED -> WITHDRAWN`, with `SUPERSEDED` as an atomic side effect of publishing a successor — never a directly-taken transition, and verified to actually demote the prior version, not just resolve around it).
- Four genuinely append-only evidence tables — `presentation_receipts`, `consent_receipts`, `withdrawal_receipts`, `preference_assertions` — each with a database trigger blocking every `UPDATE`/`DELETE`, no exceptions. There is no "status" column on any of them; "current consent status" for `(subject_ref, purpose_id)` is a **derived read** (`ResolveConsentStatus`) computed from the latest receipt plus any withdrawal referencing it, never a stored, overwritable field. This is PRV-I09/I10/I11 taken literally, not just documented.
- **A real cross-service integration, not a stub**: every `ConsentReceipt`'s `purpose_id` is checked with a live HTTP call to `privacy-purpose-registry-svc`'s `GET /privacy/purposes/{id}` before the receipt is written. A purpose that doesn't resolve to a currently published version is rejected (422), and the purpose registry being unreachable fails the request closed (503) rather than silently accepting an unvalidated purpose.
- `PreferenceAssertion` is schema-independent from `ConsentReceipt` — verified by a dedicated test (`TestPreference_NeverImpliesConsent`) that an `ENABLED` preference does not change a consent resolution from `NOT_REQUESTED`.
- 12 handler tests plus two verified negative controls: removing the double-withdrawal guard (confirmed the withdrawal test fails exactly as expected) and removing the notice publish-requires-`APPROVED` gate (confirmed the same).

**A real bug found and fixed during this build, affecting PRV-01 too:** `ResolveNoticeAsOf`/`ResolvePurposeAsOf`/`ResolveActivityAsOf` originally ordered candidates by `effective_from DESC` alone. Two versions created back-to-back can receive an **identical** wall-clock timestamp — confirmed directly (not theorized): under Windows' `time.Now()` resolution, two versions published moments apart in the same test process produced the exact same `effective_from` value, and resolution picked between them arbitrarily depending on Go map iteration order, intermittently returning the SUPERSEDED version instead of the current one. Fixed in both services by adding a Postgres `BIGSERIAL sequence_no` column (tiebreaker only, never returned in API responses) as a secondary, collision-proof sort key, and by adding an equivalent monotonic counter to both services' in-memory test stubs. Confirmed fixed with 10 consecutive clean test runs after the change, versus intermittent failures (roughly 1 in 2) before it.

**Deliberately NOT built:** PRV-03's runtime decision endpoint, PDC-backed lawful-basis evaluation (same reasoning as PRV-01 — no such package exists), and any relationship between `PreferenceAssertion` and consent/lawful basis (PRV-I12 forbids exactly that).

Registered in `docker-compose.yml`/`init-db.sh` (port 8152, db `privacy_consent`, depends on `privacy-purpose-registry-svc`) but not started against a live stack this session — same discipline and same verification method as §3.1.

### 3.3 `privacy-decision-svc` — new service, PRV-03 of ZS-SVC-W-001 (2026-08-28)

Built as a real, working (deliberately partial) v1 of PRV-03 ("Purpose Binding & Runtime Data-Use Decision Service"), the third of ZS-SVC-W-001's five services.

**Why it's deliberately partial.** The full spec (§12/§13) names five outcomes — `PERMIT`, `RESTRICT`, `BLOCK`, `REVIEW_REQUIRED`, `INDETERMINATE` — and a rich input contract including sensitivity flags, recipient/destination, and a PDC jurisdiction/policy package resolved "at effective + knowledge time." `RESTRICT` (machine-enforceable minimization/redaction/recipient-limitation constraints) and `REVIEW_REQUIRED` (routing to a qualified human reviewer) both require exactly the PDC rules engine that PRV-01's own package doc comment already flagged as nonexistent anywhere in this codebase. Building them would mean fabricating the business rules PDC is supposed to own — the same defect shape this fixing pass exists to remove. Both values are defined in the wire contract (`domain.ResultRestrict`/`domain.ResultReviewRequired`) so a future version can start returning them without a breaking change, but this version never produces them.

**What v1 DOES check — real, not fabricated:**
- The processing activity resolves to `ACTIVE` in PRV-01, and the purpose resolves to `PUBLISHED` in PRV-01 — both via live HTTP calls, not caller-supplied claims.
- **PRV-C01, purpose limitation, enforced directly from PRV-01's own schema**: the proposed `purpose_id` must actually be one of the resolved activity's registered `purpose_ids` (`ProcessingActivityVersion.PurposeIDs`). This wasn't in the original plan — it fell out of actually reading PRV-01's response shape while building the client, and it's a genuine compliance check (a purpose that's individually valid but was never bound to the activity in question is now correctly blocked, with its own reason code and regression test).
- Two checks are **opt-in and caller-declared, never inferred**: if the caller declares `consent_check.required`, PRV-02's `ResolveConsentStatus` must resolve `GRANTED`; if the caller supplies its own real `legal_hold_check.record_class`, retention-registry-svc must report no active hold. This service does not infer whether a purpose requires consent (PRV-01's `lawful_basis_refs` is documented as opaque, not a signal this service may interpret) and does not invent a `record_class` — repeating evidence-manifest-svc's §2.3 finding here would have been the same mistake twice.
- Any unreachable dependency fails the **whole decision** closed as `INDETERMINATE` (§12.2: "Material processing fails closed") — verified with a negative control that confirmed a naive fail-open change silently downgrades to a different wrong answer rather than the required one.
- Every decision is recorded as a permanent, append-only evidence row (§13.2 "decision durability") capturing the exact resolved `activity_version_id`/`purpose_version_id` — not just the caller's input IDs — so a past decision stays reproducible regardless of what PRV-01/PRV-02 report later.

13 handler tests plus two verified negative controls (fail-closed-on-unreachable-dependency, purpose-limitation enforcement).

Registered in `docker-compose.yml`/`init-db.sh` (port 8153, db `privacy_decision`, depends on `privacy-purpose-registry-svc` + `privacy-consent-svc` + `retention-registry-svc`) but not started against a live stack this session — same discipline and same verification method as §3.1/§3.2.

### 3.4 `privacy-rights-svc` — new service, PRV-04 of ZS-SVC-W-001 (2026-08-29)

Built as a real, working (deliberately partial) v1 of PRV-04 ("Data Rights, Complaint & Disclosure Control Service"), the fourth of ZS-SVC-W-001's five services.

**Why it's deliberately partial.** §14.1 draws its own explicit ownership line: "PRV-04 owns the privacy meaning and evidence of a rights request. WFC [workflow-svc] owns long-running orchestration, tasks, deadlines, approvals and escalations. Domain adapters perform search, correction, restriction, export or erasure under explicit instructions and return evidence." This service builds only the first half. It does not orchestrate tasks/deadlines/approvals, and it does not perform search/correction/export/erasure itself.

**A real integration question was investigated and deliberately declined.** The spec's own canonical API table (§18) shows `POST /privacy/rights-requests` returning a `wfc_process_ref`, implying PRV-04 creates a `workflow-svc` instance at intake. Before building that, `workflow-svc`'s actual `CreateWorkflow` handler was read directly: it requires the caller to name a concrete `approver_principal_id` for every stage up front, and separately enforces that the initiator cannot be an approver of their own workflow. At intake time, PRV-04 has no legitimate way to know who the correct privacy reviewer/approver is for an arbitrary jurisdiction/right-family combination — inventing one would be fabricating an organizational fact, the same defect shape already avoided elsewhere this session (evidence-manifest-svc's `record_class`, PRV-03's PDC rules). `wfc_process_ref` is instead an optional, caller-supplied field: whoever actually creates the real `workflow-svc` instance (a human process, or a future orchestrator with real approver-routing rules) may attach its reference via a dedicated endpoint, but this service never invents it.

**What v1 DOES build — real, not fabricated:**
- Case intake (`RightsRequest`) covering all of §14.2's right families (access, rectification, erasure, restriction, portability, objection/withdrawal, automated-decision challenge, complaint).
- Identity-assurance evidence, recorded as a caller-declared fact rather than performed by this service (§15.1's risk-proportionate verification is an organizational/legal judgment call, not something to automate without a real verification-provider integration, which doesn't exist here) — a FAILED attempt is still recorded as evidence and, critically, verified NOT to silently advance the case status.
- Discovery-manifest evidence attachment — domain services record what they found; this service never searches anything itself.
- **§15.2's DISCLOSURE GATE, enforced verbatim, not invented**: "Finding a record is only discovery. Disclosure requires identity assurance... [and] approved response assembly." A request can only close with outcome `FULFILLED` if identity has been verified AND at least one discovery manifest was recorded — enforced atomically inside the closing transaction itself (not just the handler layer), so a race between two callers cannot slip a `FULFILLED` closure past the gate. `REJECTED`/`WITHDRAWN` carry no such precondition, since a request can legitimately be rejected precisely because identity could never be verified.
- A closed request is permanently immutable at the database layer (a trigger rejects any further update once `status = CLOSED`).

14 handler tests plus two verified negative controls on the two DISCLOSURE GATE preconditions — the first attempt at the identity-verification control was itself caught as flawed (the test hadn't attached a discovery manifest either, so the *other* precondition masked whether the identity check was doing anything at all); the test was corrected to isolate each precondition before the negative control was re-run and confirmed genuine.

**Not modeled**: exemption/third-party review, redaction, and response-package assembly (§15.1's other control points) — these require case-specific legal judgment this service cannot supply.

Registered in `docker-compose.yml`/`init-db.sh` (port 8154, db `privacy_rights`, depends on `authorization-svc`) but not started against a live stack this session — same discipline and same verification method as §3.1–§3.3.

### 3.5 `privacy-transfer-svc` — new service, PRV-05 of ZS-SVC-W-001 (2026-08-29) — domain complete

Built as a real, working (deliberately partial) v1 of PRV-05 ("Processor, Subprocessor & Transfer Privacy Control"), the fifth and final service in the ZS-SVC-W-001 build.

**Why it's deliberately partial.** §16.1 defines `TransferMechanism` as "a governed mechanism ID/version from PDC/legal catalogue" — no PDC catalogue exists anywhere in this codebase, the same gap already documented on PRV-01/PRV-03. Rather than invent a legal-validity catalogue, `TransferMechanism` is its own directly-registered record: a real legal/compliance reviewer enters the mechanism (SCC/BCR/adequacy/derogation), its validity window and its evidence reference — the same trust model this platform already uses for SoD rules and notice content (a human records a real fact; the system enforces the machine-checkable invariant on top, here the validity-window expiry). Similarly, §17's "PDC rule says assessment required?" branch cannot be evaluated without a PDC rule to consult, so whether a DPIA/TIA is required is an opt-in, caller-declared input (`assessment_required`) — mirroring PRV-03's `ConsentCheckRequest`/`LegalHoldCheckRequest` pattern exactly.

**What's real, not fabricated:**
- Processor relationship and subprocessor inventory, matching §16.1's own field list, with `purpose_activity_refs` validated against a live call to PRV-01 (each must resolve to an ACTIVE processing activity) — not trusted as opaque strings.
- **§17.2's fail-closed rule encoded literally, not invented**: "If a required DPIA/TIA, transfer mechanism... approval is missing, expired or conflicted, PRV-05 returns BLOCKED or REVIEW_REQUIRED." This service maps that sentence directly: an expired or not-yet-effective mechanism → `BLOCKED`; a missing assessment (when declared required) → `REVIEW_REQUIRED`; a `REJECT` outcome → `BLOCKED`; a `REMEDIATE` outcome → `REVIEW_REQUIRED`; an `APPROVE` outcome past its own `review_trigger_at` (§17.1's explicit reassessment trigger) → `REVIEW_REQUIRED`. Unlike PRV-03, `REVIEW_REQUIRED` is genuinely reachable here — it required no invented business rule, only the spec's own stated fail-closed behavior.
- `CONDITIONAL` (the fourth `TransferAuthorization` value, requiring machine-enforceable transfer conditions this service has no rule engine to generate) is defined in the wire contract for forward compatibility but never produced — same reasoning as PRV-03's `RESTRICT`.
- Every decision is a permanent, append-only evidence row, same "decision durability" doctrine as PRV-03, capturing the resolved `assessment_id` when one was consulted.

16 handler tests plus two verified negative controls (mechanism-expiry check, assessment-expiry check).

**All five ZS-SVC-W-001 services now exist.** See §3.6 below — a live-stack smoke test was subsequently run against all five, which is what actually found the two real bugs recorded there.

Registered in `docker-compose.yml`/`init-db.sh` (port 8155, db `privacy_transfer`, depends on `authorization-svc` + `privacy-purpose-registry-svc`).

### 3.6 Live-stack smoke test of all five ZS-SVC-W-001 services (2026-08-29) — two real bugs found and fixed

Every claim in §3.1–§3.5 above about RLS/immutability/cross-service integration had been verified by code inspection, unit tests against in-memory stubs, and `docker compose config` — never by actually running the services. This section records what happened when they were actually booted (Postgres 16 + Kafka + `authorization-svc` + `retention-registry-svc` + all five privacy services) and exercised end to end.

**What worked, genuinely, against a real stack:** the full processing-activity lifecycle in PRV-01 (`DRAFT → VALIDATED → SUBMITTED → APPROVED → ACTIVE`); PRV-02 recording a real consent and PRV-03 resolving it via a live HTTP call, correctly capturing the exact `consent_receipt_id` as evidence on the decision; PRV-03 correctly `BLOCK`ing on `CONSENT_NOT_GRANTED` and on `PURPOSE_NOT_BOUND_TO_ACTIVITY` (PRV-C01) for a subject/purpose that genuinely didn't qualify; PRV-05 validating a `purpose_activity_refs` entry against a live PRV-01 call and correctly rejecting an unregistered one; PRV-05's transfer-decision evaluation returning `AUTHORIZED` for a valid relationship + mechanism; PRV-04's DISCLOSURE GATE correctly refusing to close a request `FULFILLED` with no identity verification. This is the real, working cross-service architecture the previous five sections describe — confirmed, not just asserted.

**Bug 1 — platform-wide, pre-existing, not introduced this session: malformed IDs surface as 503, not 404.** Requesting `GET /privacy/purposes/totally-made-up-purpose-id` (a syntactically invalid UUID) returned `503 store unavailable` instead of `404 not found` — Postgres rejects the cast with `SQLSTATE 22P02`, and every service's store layer was wrapping that as `ErrStoreUnavailable` alongside genuine connection failures. Checked directly against `retention-registry-svc` and `authorization-svc` (both pre-existing, untouched this session) and found identical behavior — this is a platform-wide pattern likely present in most of the ~86 services that take an ID path/body param, not something specific to the five built this session. **Fixed in the five privacy services only** (added an `isInvalidUUID` helper checking for SQLSTATE 22P02, wired into every single-row lookup to return the domain "not found" sentinel instead). The other ~80 services are unchanged — fixing those is a separate, estate-wide effort in the same category as the event-envelope/API-contract migrations in §5, not something to do opportunistically here.

**Bug 2 — introduced this session, in `privacy-rights-svc` and `privacy-transfer-svc`: NULL-scan crash on omitted optional fields.** Several fields (`Jurisdiction`, `RequesterRef`, `SubmittedVia` on `RightsRequest`; `ProcessingInstructions`, `ContractEvidenceRef` on `ProcessorRelationship`; `Purpose`, `DataScope`, `NotificationApprovalModel` on `Subprocessor`; `EvidenceRef`, `Conditions` on `TransferMechanism`; `ResidualRisk`, `EvidenceRef` on `TransferAssessment`) are modeled as plain `string` on the wire (empty rather than `null` when absent) but backed by nullable columns written via the existing `strPtrOrNil` helper — so omitting any of them at request time produced a hard `cannot scan NULL into *string` failure the instant that row was read back. `privacy-rights-svc` already had the correct pattern for two other fields (`noteScan`/`evidenceRefScan`) but hadn't applied it to `scanRequest`'s three fields; `privacy-transfer-svc` had the bug throughout. **Fixed** by consolidating into one `nullString` scanner type per service (implementing `sql.Scanner`, bridging SQL `NULL` to Go `""`) and applying it at every affected `Scan` call site.

Neither bug was caught by any of the 71 unit tests written across the five services, because the in-memory stub stores have no real UUID column type to reject a malformed string, and no real nullable-column semantics to violate — both defects are specifically the kind that only a real database can expose. All five services were rebuilt, redeployed, and the exact failing requests re-run to confirm both fixes live, then the full unit suites re-run to confirm no regression, before the containers used for this test were stopped.

### 3.7 `supplier-financial-profile-svc` — new service, AP-01 of the Procurement/Expenses/Accounts Payable baseline (2026-08-29)

Built as a real, working (deliberately partial) v1 of AP-01 ("Supplier Financial Profile"), the first service of a new domain, opened after all five ZS-SVC-W-001 privacy services were complete and verified (§3.1–§3.6). AP-01 is the foundation the unbuilt payment side of Accounts Payable (AP-09/10/11, Payment Proposal/Authorization/Run) will eventually need a governed supplier record to pay against.

**Why it's deliberately partial.** AP-01's own contract names "ORG-10 Banking Identity/Payee Master" as the authoritative owner of real bank/payee details — no such service exists anywhere in this codebase (`banking-connector-svc` is a different bounded context: bank statement ingestion for reconciliation, not payee identity). Rather than fabricate a resolution, `PayeeReference` is modeled as an opaque, caller-supplied string, never validated or resolved — the same doctrine already used for `lawful_basis_refs` (PRV-01) and `record_class` (`retention-registry-svc`). The spec's state model (`Draft → Active → OnHold/Suspended → Retired`) requires a `Draft → Active` transition but the spec's own command list has no explicit `Activate` command — added honestly as `ActivateSupplierFinancialProfile` rather than silently omitted or smuggled into `AmendSupplierFinancialProfile`.

**What's real, not fabricated:**
- Full profile lifecycle (`Draft/Active/OnHold/Suspended/Retired`) with state-machine-enforced transitions, matching the spec's model exactly.
- **Negative-path scenario #2 from AP-01's own acceptance table** ("supplier payment terms overlap effective periods" must be blocked) is enforced by a genuine Postgres `EXCLUDE USING gist` constraint on `payment_terms_periods` (`profile_id WITH =, tstzrange(effective_from, effective_to) WITH &&`, requiring `btree_gist`) — a database-layer, race-proof invariant, not an application-level check-then-insert a race could defeat.
- **A variant of negative-path scenario #3** ("bank/payee changer cannot authorize resulting payment without independent control") is enforced via a real propose/decide flow (`ProposeHighRiskChange` / `DecideHighRiskChange` on `payee_reference` and `payment_method_preference`) that calls `authorization-svc`'s dynamic own-object Segregation-of-Duties layer (`CheckAllowedOwnObject`, built earlier this session — see §2.6) with the proposer as `resource_owner_principal_id`. This is the first real caller of that feature outside authorization-svc's own tests, and it worked as designed on first integration.
- Every state transition and high-risk-change decision is a permanent, append-only `profile_change_events` row (immutability enforced by `BEFORE UPDATE/DELETE` triggers, since the runtime connects as Postgres superuser and bypasses GRANT/REVOKE).
- Scenarios **not** built: negative-path #1 (invoice/payee bank-detail mismatch) and #4 (inactive supplier used in new PO/invoice) both need the *other* side of the check — a live call from `invoice-approval-svc`/`purchase-order-svc` into this service — which is out of scope for AP-01 alone and not wired up here; this service exposes the state (`ACTIVE`/`RETIRED`, `PayeeReference`) those services would need to consult, but the consulting side wasn't built or integrated as part of this task.

14 handler tests plus two verified negative controls (payment-terms overlap, high-risk-change self-approval).

Registered in `docker-compose.yml`/`init-db.sh` (port 8156, db `supplier_financial_profile`, depends on `authorization-svc`).

### 3.8 `goods-service-receipt-svc` — new service, AP-04 of the Procurement/Expenses/Accounts Payable baseline (2026-08-29)

Built as a real, working (deliberately partial) v1 of AP-04 ("Goods/Service Receipt"), the second service in this domain. It is the missing three-way-match link between `purchase-order-svc` (AP-03, already built) and `invoice-approval-svc` (AP-05/06, already built).

**Why it's deliberately partial.** `purchase-order-svc` has no PO-line or quantity model at all — `domain.PurchaseOrder` is header-only (`total_amount`, `po_status`), with only two statuses (`ISSUED`, `CLOSED` — no `CANCELLED`). AP-04's own contract names "PO open quantity/amount" as server-resolved context and lists "receipt exceeds PO tolerance" and "receipt confirmed against cancelled PO" as required negative-path scenarios. Since no PO-line or received-quantity data exists upstream to match against, this service enforces the nearest real equivalent: an aggregate amount check against the PO's own `total_amount` (net of this service's own confirmed-and-not-yet-reversed receipts), and treats `purchase-order-svc`'s real `CLOSED` status as the blocking state, there being no `CANCELLED` status to check against. A caller-supplied `ToleranceExceptionRef` is required to confirm past that ceiling — the exception approval itself is out of this service's authority, the same "opaque caller-supplied reference" doctrine as `PayeeReference` (AP-01, §3.7) and `lawful_basis_refs` (PRV-01, §3.1). Verified directly against `purchase-order-svc`'s actual handler/domain code before writing a line of this service, not assumed.

**What's real, not fabricated:**
- Full receipt lifecycle (`Draft/PendingConfirmation/Confirmed/Rejected/PartiallyReversed/FullyReversed`), with the PO's real open/closed status live-checked at both create time and confirm time — a PO that closes in between is caught on the second check, not just the first.
- **Negative-path scenario #1** ("receipt exceeds PO tolerance without approval") is enforced for real against a live `SUM(amount - reversed_amount)` aggregate across every confirmed receipt for that PO, compared to the PO's real `total_amount` from `purchase-order-svc` — not a fabricated per-line check.
- **Negative-path scenario #2** ("receipt confirmed against cancelled PO") maps onto `purchase-order-svc`'s real `CLOSED` status, checked live via HTTP at confirm time, not cached from create time.
- **Negative-path scenario #3** ("confirmed receipt deleted to fix mismatch") is blocked unconditionally by a `BEFORE DELETE` trigger — no row in `goods_service_receipts` can ever be deleted, at any status. `ReverseReceipt` is the only sanctioned correction path, itself an append-only record (`receipt_reversals`) that accumulates up to (never past) the original amount.
- **Negative-path scenario #4** ("GRNI event emitted twice on replay") is enforced by riding `general-ledger-svc`'s own real idempotency contract: every GRNI journal post uses `receipt_id` as `correlation_id`, so a replayed request against the same journal returns the original rather than creating a duplicate — verified structurally in this service's own tests (the ledger stub asserts it is asked to post exactly once per receipt ID).
- The SoD line ("receiver/acceptor cannot self-certify sensitive services where policy requires independent acceptance") is enforced via `authorization-svc`'s dynamic own-object SoD layer — the same feature and calling pattern first exercised by `supplier-financial-profile-svc` (AP-01, §3.7), now proven reusable by a second, independently-built service.
- GRNI accounting posts a real journal entry to `general-ledger-svc` (a real, existing endpoint: create → validate → post) but is deliberately best-effort: any failure at any step (GL unreachable, fiscal period locked, GL-side rejection) is recorded as a visible `EXCEPTION` accounting event and never blocks or reverses the receipt confirmation that already succeeded — this is AP-04's own stated failure semantics ("accounting event failure leaves visible pending/exception state, never silent receipt deletion") applied literally. GRNI account codes are operator-configured (`GRNI_DEBIT_ACCOUNT_CODE`/`GRNI_CREDIT_ACCOUNT_CODE`) since no chart-of-accounts authority exists anywhere in this codebase to resolve them dynamically. There is no background retry scheduler in this codebase, so an `EXCEPTION` event requires manual operational follow-up — an honest gap, not a fabricated auto-retry.

17 handler tests plus two verified negative controls (over-tolerance-without-exception, service-acceptance self-certification).

Registered in `docker-compose.yml`/`init-db.sh` (port 8157, db `goods_service_receipt`, depends on `authorization-svc` + `purchase-order-svc` + `general-ledger-svc`).

### 3.9 `expense-claim-svc` — new service, AP-07 of the Procurement/Expenses/Accounts Payable baseline (2026-08-29)

Built as a real, working (deliberately partial) v1 of AP-07 ("Expense Claim"), the third service in this domain. Unlike AP-01 and AP-04, every one of AP-07's own named dependencies (Workforce/ORG, Policy, TAX, Documents) turned out to be a real, existing service — the scope decisions here are about integration depth, not missing peers, except for AP-08.

**What's real, not fabricated:**
- Claimant identity is verified against `employee-master-svc`'s real `GET /v1/employees/{id}` at `CreateExpenseClaim` time — must exist, belong to the stated legal entity, and be `ACTIVE` (not `ONBOARDING`/`SUSPENDED`/`TERMINATED`).
- Receipt evidence is verified against `document-vault-svc`'s real `GET /v1/documents/{id}` when a line attaches one — must exist, match tenant/legal entity, and not be `PURGE_PENDING`.
- **Negative-path scenario #2** ("same receipt used on two claims") is enforced by a genuine partial `UNIQUE` index on `expense_lines.receipt_document_id` (`WHERE receipt_document_id IS NOT NULL`) — a database invariant spanning every claim platform-wide, not an application check a race could defeat.
- **Negative-path scenario #4** ("tax reclaim inferred without TAX result" must be blocked) is enforced literally: every line declaring `ClaimTaxRecovery` gets a real `POST /v1/tax-determinations` call to `tax-determination-svc` at `SubmitExpenseClaim` time; `TaxableAmount`/`CalculatedTaxAmount` are set only from that call's own response, keyed by its real `DeterminationID`, and a failed call blocks the entire submission (`422`) rather than leaving an invented figure on the claim.
- Policy-svc's real `POST /v1/policies/evaluate` (`policy_type=APPROVAL_THRESHOLD` — the only type it has real evaluation logic for) is called against the claim total at submit time. A `404 no_applicable_policy` (no threshold configured) is treated as `WITHIN_THRESHOLD`, per policy-svc's own documented stance that it will not guess fail-open/fail-closed for an unconfigured policy — a judgment call, documented in code rather than silently assumed.
- **Negative-path scenario #1** ("claimant approves own expense") is blocked via `authorization-svc`'s dynamic own-object SoD layer — the third independent reuse of that feature this session (after AP-01 and AP-04), now clearly a load-bearing, general-purpose platform capability rather than a one-off.
- **Negative-path scenario #3** ("expense over receipt threshold approved without evidence") is enforced at `ApproveExpenseClaim`: any line above `RECEIPT_REQUIRED_THRESHOLD` with no attached receipt blocks approval outright, unless `RecordExpensePolicyException` (itself gated behind a stronger `EXPENSE_EXCEPTION_APPROVE` authority and the same SoD check) has explicitly waived it for that claim — the literal mapping of AP-07's own SoD line ("approver cannot override policy/receipt requirement outside delegated exception authority").
- A claim policy-svc flags `APPROVAL_REQUIRED` needs that same stronger `EXPENSE_EXCEPTION_APPROVE` authority to approve at all, not the ordinary `EXPENSE_APPROVE` — policy escalation is enforced by which authorization action is checked, not by application-level logic duplicating policy-svc's own decision.
- Every claim transition is a permanent, append-only `expense_claim_events` row; expense lines and the claim itself are immutable once the claim leaves `DRAFT`/`RETURNED` (enforced by `BEFORE INSERT/UPDATE/DELETE` triggers, since the runtime connects as Postgres superuser and bypasses GRANT/REVOKE).

**Honest gap: AP-08 "Payables."** `accounts-payable-svc` exists but its entire domain is vendor-invoice-shaped (`vendor_id`, `CreateInvoice`/`ApproveInvoice`) with no claimant/reimbursement concept anywhere in it. Routing an approved expense claim through it as a fabricated "vendor invoice" would be exactly the kind of invented integration this codebase's discipline forbids. `ApproveExpenseClaim` instead emits `ExpenseClaimPayableRequested` honestly, with no real consumer yet — the same class of gap as AP-01's ORG-10 and AP-04's PO-line model.

**Two consolidations of the spec's own state model, not gaps in a peer:** the spec names both "Submitted" and "PendingApproval" as distinct states with only one command (`SubmitExpenseClaim`) to reach either — collapsed into one `PENDING_APPROVAL` status, the same kind of honest consolidation as AP-01's added `Activate` command. And "Approved → Reimbursable → Closed" only ever reaches `REIMBURSABLE` here — `Closed` would require a real payables consumer to report the reimbursement settled, which doesn't exist (see AP-08 above).

18 handler tests plus two verified negative controls (over-threshold-without-exception, self-approval).

Registered in `docker-compose.yml`/`init-db.sh` (port 8158, db `expense_claim`, depends on `authorization-svc` + `employee-master-svc` + `document-vault-svc` + `tax-determination-svc` + `policy-svc`).

### 3.10 `payment-proposal-svc` — new service, AP-09 of the Procurement/Expenses/Accounts Payable baseline (2026-08-29)

Built as a real, working (deliberately partial) v1 of AP-09 ("Payment Proposal"), the fourth service in this domain and the first to draw its "eligible payable population" from two other services built this session (`accounts-payable-svc`'s APPROVED invoices, `expense-claim-svc`'s REIMBURSABLE claims) plus `supplier-financial-profile-svc` (AP-01) for payee context — a genuine cross-service pipeline, not a standalone service with invented upstream data.

**Why it's deliberately partial.** AP-09's own command list (`CreatePaymentProposal`, `AddEligiblePayable`, `RemovePayable`, `RecalculatePaymentProposal`, `SubmitProposalForReview`, `FreezePaymentProposal`, `CancelPaymentProposal`) has no `Authorize`/`Reject` command — those belong to AP-10 ("Payment Authorization"), a distinct, unbuilt service. This service therefore only ever reaches `FROZEN`; `AUTHORIZED`/`REJECTED` are kept in the status `CHECK` constraint for forward compatibility (the same treatment AP-07 gave an unused `APPROVED` value) but this service never writes them. AP-09's own SoD line ("proposal preparer cannot be sole authorizer where maker-checker applies") has nothing to bite on with no Authorize command here, so the closest real analogue this service owns — `FreezePaymentProposal`, the last checkpoint before authorization would occur — requires a principal distinct from the proposal's own preparer instead, via `authorization-svc`'s dynamic own-object SoD layer: **the fourth independent reuse of that feature this session**, after AP-01, AP-04 and AP-07.

**What's real, not fabricated:**
- `accounts-payable-svc`'s real `GET /v1/invoices/{id}` and `expense-claim-svc`'s real `GET /ap07/expense-claims/{id}` are both called to verify a payable genuinely exists, belongs to the caller's tenant/legal entity, and is in an eligible status (`APPROVED` / `REIMBURSABLE`) before it can be selected. `expense-claim-svc` has no stored total on a claim — this service sums lines client-side exactly the way `expense-claim-svc`'s own handler does, rather than inventing a total field that doesn't exist upstream.
- **Negative-path scenario #1** ("held payable force-added without exception"): `accounts-payable-svc` has no hold/disputed concept of its own at all (checked directly) — the only real hold signal anywhere upstream is `supplier-financial-profile-svc`'s own `ON_HOLD` status. An `AP_INVOICE` item whose supplier profile is `ON_HOLD` is rejected unless the caller supplies a non-empty `ExceptionRef`, which itself requires the stronger `PAYMENT_PROPOSAL_EXCEPTION_RESOLVE` authority rather than ordinary `PAYMENT_PROPOSAL_MANAGE`. `FreezePaymentProposal` re-checks this live, in case a supplier went on hold after the item was originally added without an exception.
- **Negative-path scenario #2** ("duplicate/in-flight payable added to second proposal") is enforced by a genuine partial `UNIQUE` index on `(payable_source, payable_id) WHERE is_active` — a database invariant, not an application check a race could defeat. `RemovePayable` and `CancelPaymentProposal`'s cascade both work by flipping `is_active` to `FALSE` (append-only doctrine: rows are never deleted), which is also what frees a payable for reselection into a different proposal.
- **Negative-path scenario #3** ("proposal amount changes after freeze but approval retained"): once `FROZEN`, a `BEFORE UPDATE` trigger permits only the eventual `CANCELLED`/`AUTHORIZED`/`REJECTED` transition and blocks every other column from changing; proposal items are immutable outright past insertion except the same one-way `is_active` flip. Since AP-10 doesn't exist to have an authorization to invalidate, this service owns the half of the invariant it can: nothing about a frozen proposal's composition or totals can silently change under it.
- **Negative-path scenario #4** ("payment uses stale payee identity version"): `supplier-financial-profile-svc` has no version/revision column (checked directly in its migration and domain type) — `updated_at` is the only available staleness signal. Every `AP_INVOICE` item captures it as `PayeeSnapshotAt` when added; `FreezePaymentProposal` re-fetches the live profile and blocks (`409`) if it has changed since, rather than freezing against a payee identity that may already be wrong. `EXPENSE_CLAIM` items carry no such risk — a claim's payment preference is captured once and immutable the moment it leaves `DRAFT`/`RETURNED` (AP-07's own trigger).
- `tax-determination-svc`'s real endpoint computes withholding for an `AP_INVOICE` item when the caller declares `apply_withholding` — the same "real call, never inferred" doctrine as AP-07's tax-recovery lines, reused here for withholding instead of reclaim.
- `GetFingerprint` computes a real SHA-256 digest over the proposal's own composition and amounts — the "subject fingerprint" AP-09's own contract names it owns. There was no existing "fingerprint" pattern in this codebase to follow (the only prior art is TLS certificate fingerprints, an unrelated concept, and `tax-determination-svc`'s content-addressed rule snapshot, which is the same underlying principle) — this is a new, real computation, not a placeholder.

**Honest gap: BNK account/cash availability.** Neither `bank-reconciliation-svc` nor `banking-connector-svc` exposes a live balance/availability check anywhere (confirmed directly in both services' code) — `PayingBankAccountRef` is an opaque, caller-supplied reference, never resolved or balance-checked, the same doctrine as AP-01's `PayeeReference`.

16 handler tests plus two verified negative controls (self-freeze SoD denial, duplicate-payable-across-proposals rejection).

Registered in `docker-compose.yml`/`init-db.sh` (port 8159, db `payment_proposal`, depends on `authorization-svc` + `accounts-payable-svc` + `expense-claim-svc` + `supplier-financial-profile-svc` + `tax-determination-svc`).

### 3.11 `payment-authorization-svc` — new service, AP-10 of the Procurement/Expenses/Accounts Payable baseline (2026-08-31)

Built as a real, working v1 of AP-10 ("Payment Authorization"), the fifth service in this domain and the **first real consumer of AP-09's own `FROZEN` state and `GetFingerprint` endpoint** — both built in the previous session specifically so a real downstream authorization step could exist. This closes the authorization half of the AP-09→AP-10 handoff the domain has been building toward since AP-09 was scoped.

**What's real, not fabricated:**
- `payment-proposal-svc`'s real `GET /ap09/proposals/{id}` and `GET /ap09/proposals/{id}/fingerprint` are called to verify a proposal is genuinely `FROZEN` before authorization can be requested against it, and its fingerprint is captured as `ProposalFingerprint` at request time.
- **Negative-path scenario #1** ("payee bank details changed after approval"): AP-09 already snapshots each `AP_INVOICE` item's payee `updated_at` at its own freeze time, but that guarantee is only as fresh as AP-09's freeze moment. This service copies those snapshots into its own `authorization_payee_snapshots` (its own authoritative record, per its own contract) at request time, then independently re-verifies them live against `supplier-financial-profile-svc` at **both** `ApprovePayment` and `ConsumePaymentAuthorization` — two further, later checkpoints than AP-09 ever had the chance to check. Any mismatch (fingerprint or payee identity) moves the authorization to a genuinely reachable `INVALIDATED` state, not just a blocked request — the literal reading of the state model's own words, "any protected-field mismatch invalidates."
- **Negative-path scenario #2** ("signer exceeds delegated limit") reuses `policy-svc`'s real `APPROVAL_THRESHOLD` evaluation against the authorization's net amount — the same integration AP-07 and AP-09 already use — as AP-10's own "delegated signing limit": a signer without the stronger `PAYMENT_AUTHORIZE_HIGHVALUE` authority cannot approve a payment policy-svc flags `APPROVAL_REQUIRED`.
- **Negative-path scenario #3** ("proposal maker self-authorizes where prohibited") is enforced via `authorization-svc`'s dynamic own-object SoD layer, with the proposal's own preparer (fetched from AP-09) as resource owner — **the fifth independent reuse of that feature this session**, after AP-01, AP-04, AP-07, and AP-09.
- **Negative-path scenario #4** ("authorization token replayed after consumption") is enforced redundantly at two layers, mirroring every other service's immutability discipline this session: the store's own `WHERE status = 'APPROVED'` guard on `ConsumeAuthorization`, plus the migration's `BEFORE UPDATE` trigger making every terminal status (`CONSUMED` included) permanently immutable. Verified directly: disabling the handler-level guard alone left the negative-control test passing, because the store's own guard caught the replay independently — genuine two-layer redundancy, not a single point of failure disguised as two checks.
- A bonus real invariant beyond the four named scenarios: a frozen proposal can have at most one active (`PENDING`/`APPROVED`) authorization request in flight at a time, enforced by a partial unique index — the same technique as AP-09's own active-payable uniqueness.

**Two honest gaps, not fabricated:**
- `ExpirePaymentAuthorization` has no automatic trigger — there is no background scheduler anywhere in this codebase (the same class of gap already documented for AP-04's GRNI retry and AP-01's high-risk-change flow). It is a real, callable command an operator or a future scheduled job can invoke, never an invented automatic timer this service doesn't actually run.
- `ConsumePaymentAuthorization` has no real caller yet: AP-11 ("Payment Run"), the service that would actually execute a payment and consume its authorization, does not exist in this codebase. It is still implemented fully and honestly — including negative-path #4's replay protection — so it is ready the moment AP-11 exists, rather than left half-built.

15 handler tests plus two verified negative controls (self-approval SoD denial, consumption-replay rejection — the latter confirmed to be enforced independently at both the handler and store layers).

Registered in `docker-compose.yml`/`init-db.sh` (port 8160, db `payment_authorization`, depends on `authorization-svc` + `payment-proposal-svc` + `supplier-financial-profile-svc` + `policy-svc`).

### 3.12 `payment-run-svc` — new service, AP-11 of the Procurement/Expenses/Accounts Payable baseline (2026-08-31) — the domain's central honest gap

Built as a real, working v1 of AP-11 ("Payment Run"), the sixth service in this domain. **This is the one service in the whole AP-01→AP-11 build where the gap is not a field-level omission but the literal, stated purpose of the service itself, and it was confirmed with the user before writing any code, not discovered after.**

**The gap, stated plainly.** AP-11's purpose per its own spec: "Orchestrate authorized payable instructions into controlled payment runs and hand them to Banking for external initiation/status." A direct search of this entire codebase — `banking-connector-svc`, `bank-reconciliation-svc`, `treasury-svc` — found no real endpoint anywhere that initiates an outbound payment to an external bank/provider, and no real webhook/callback receiver for a provider's status response. `treasury-svc`'s own `POST /v1/treasury/transfers` ("InitiateTransfer") looked promising but only inserts a row and moves balances between two of its own internal ledger rows — it never calls anything external. Every previous gap in this domain (AP-01's ORG-10, AP-09's bank-balance check, AP-10's `ConsumePaymentAuthorization` caller) was a supporting piece; this is AP-11's entire reason for existing. Presented to the user directly, with three options (build the honest partial slice, skip to AP-12, or stop the domain here) — the first was chosen.

**What that means concretely:** `SubmitPaymentRun` does not call any bank — it records that submission was attempted and nothing more, collapsing the spec's `Submitted → Pending/Unknown` into one `SUBMITTED` status since there is no real provider response to distinguish them by. `ReconcilePaymentRunStatus` is not a webhook receiver — there is no real callback to receive — it is a command an operator calls with a caller-attested `ExternalStatus` and `ProviderEventRef`, the same "record a real external fact a human observed" doctrine already used for AP-01's high-risk-change approvals.

**What IS real, not fabricated, despite the gap:**
- **This is the FIRST real caller of `payment-authorization-svc`'s `ConsumeAuthorization`** anywhere in this codebase — a gap AP-10 itself documented two services ago as having no real caller yet. `LockPaymentRun` calls AP-10's real `ValidateAuthorization` (live re-check) then `ConsumeAuthorization` for every instruction in the run; a mid-sequence failure moves the whole run to `EXCEPTION` rather than silently leaving some authorizations consumed and others not (verified directly: a forced consumption failure moves the run to `EXCEPTION`, confirmed via the test suite).
- **Negative-path scenario #4** ("cross-tenant payable included in run") is enforced for real: every authorization named in `CreatePaymentRun` is fetched live from AP-10, and its own `LegalEntityID`/`TenantID` must match the run's, or the whole create is rejected.
- **Negative-path scenario #1** ("payment run replayed after timeout") is enforced by a real idempotency-key check: a repeat `SubmitPaymentRun` call on an already-`SUBMITTED` run with the *same* key is a no-op returning current state; a *different* key is rejected outright.
- **Negative-path scenario #2** ("provider callback forged/replayed"), reinterpreted honestly for a service with no real callback to verify a signature on: a genuine database uniqueness constraint on `(instruction_id, provider_event_ref)` makes a repeat `ReconcilePaymentRunStatus` call idempotent, never double-applied.
- **Negative-path scenario #3** ("run marks payment settled from initiation response") is enforced structurally, not by a runtime check that could be forgotten: `SubmitPaymentRun`'s own method signature has no path to any status beyond `SUBMITTED` — only a later, separate `ReconcilePaymentRunStatus` call can ever move a run to `ACCEPTED`/`REJECTED`/`PARTIALLY_ACCEPTED`/`SETTLED`.
- A bonus real invariant beyond the four named scenarios: an authorization consumed into one run instruction can never be included in another, via a genuine unique index (AP-10's authorizations are already single-use, but this is AP-11's own defense-in-depth layer of the same guarantee).

**Scope note, not a gap:** unlike every other service in this domain, this one does **not** call `authorization-svc`'s dynamic own-object SoD layer. AP-11's own SoD line ("run operator cannot alter authorized fields; unauthorized re-initiation prohibited") is a data-immutability/idempotency requirement, not a maker/checker person-pair rule — there is nothing here for that feature to check, and forcing a sixth reuse where the spec doesn't call for one would itself have been a small fabrication.

14 handler tests plus two verified negative controls (idempotency-key-mismatch rejection, replayed-provider-event idempotency).

Registered in `docker-compose.yml`/`init-db.sh` (port 8161, db `payment_run`, depends on `authorization-svc` + `payment-authorization-svc`).

### 3.13 `payment-initiation-adapter-svc` — new service, BNK-06 of the Banking, Cash & Treasury baseline (2026-08-31)

Built after the user asked directly "how will banks be added, and who adds them" following §3.12's central gap — the Banking spec (a separate document, `ZoikoSuite_Banking_Cash_Treasury_Detailed_Service_Specifications_v1_0.docx`, not read until this question was asked) turned out to define exactly the two services AP-11 named as dependencies: BNK-06 (this one) and BNK-07 (§3.14). Both were scoped and built in direct response.

**The Provider Adapter boundary.** The Banking spec itself names "Provider Adapter" as a dependency of BNK-06 but never gives it its own numbered section or contract anywhere in the spec set — confirming it as the genuinely pluggable point where a real bank/PSP integration (a specific bank's API, a SWIFT gateway, a payment processor's SDK, real credentials) belongs. This service defines that boundary as a real Go interface (`provideradapter.Client`) and ships a real, working, clearly-labeled `StubProviderAdapter` behind it — deterministic (its outcome is derived from the request's own `PaymentReference` via a documented `SIMULATE_TIMEOUT`/`SIMULATE_REJECT` prefix convention, not random), and never disguised as a real bank connection. Swapping in a real provider means implementing this same interface against a real API; nothing else in this service would need to change.

**What's real, not fabricated, on this side of that boundary:**
- **"Attempt record created after external call"** is structurally impossible: `PrepareAttempt` persists a `PREPARED` row *before* `SubmitAttempt` is ever allowed to call the provider adapter — there is no code path that calls the adapter without a durable record already committed.
- **"Provider timeout triggers new payment ID"** is structurally impossible: `RetrySameAttempt` re-submits the *same* `AttemptID`/`IdempotencyKey` row — a genuine database unique index on `idempotency_key` backs this. A timeout moves the attempt to `PENDING_UNKNOWN`, never `REJECTED` — verified directly: forcing the outcome mapping to treat timeout as rejection broke the negative-control test immediately.
- **"Payment amount changed after authorization"**: once `PREPARED`, an attempt's authorized fields (amount, currency, payer/payee references, authorization fingerprint) are immutable via a database trigger.
- **"Payment sent from suspended/unverified account"** has no real account-status source to check against (AP-09/AP-10 already treat bank account references as opaque) — this service requires the caller to explicitly assert `PayerAccountVerified=true` at prepare time, a real caller-attested gate, not a fabricated automatic check.
- `PrepareAttempt` is itself idempotent on `idempotency_key`: a repeat call returns the existing attempt (`200`), never a duplicate — verified directly as a negative control.

13 handler tests plus two verified negative controls (duplicate-idempotency-key returns existing rather than creating a second row; a corrupted timeout-outcome mapping is caught immediately by the test suite).

Registered in `docker-compose.yml`/`init-db.sh` (port 8162, db `payment_initiation_adapter`, depends on `authorization-svc` only — no real Banking dependency to depend on yet).

### 3.14 `payment-status-svc` — new service, BNK-07 of the Banking, Cash & Treasury baseline (2026-08-31)

Built alongside BNK-06 as its status/finality counterpart. **Unlike BNK-06, this service's central capability IS fully real**: a genuine HMAC-SHA256 signature-verified webhook receiver. There is no real bank/PSP sending callbacks in this environment, but the *receiving* side's security, ordering and idempotency logic is exactly what a real integration with a real provider (Stripe, Adyen, a bank's own webhook feed — all of which use this same signed-payload pattern) would need, and none of it is faked. Swapping in a real provider means pointing its webhook configuration at `POST /bnk07/webhooks/provider-callback` and sharing its real signing secret via `WEBHOOK_SHARED_SECRET`; nothing about this service's own logic would need to change.

**What's real, not fabricated:**
- `ProcessProviderCallback` verifies an HMAC-SHA256 signature (the `X-Webhook-Signature` header, computed over the exact raw request body, never the re-parsed/re-serialized JSON) against a configured shared secret before touching anything — the literal fix for negative-path scenario #1 ("forged callback accepted"). Verified directly: a callback signed with the wrong secret is rejected with `401`, confirmed as a negative control.
- **Negative-path scenario #2** ("out-of-order status regresses settled payment"): once a payment reaches a governed final state (`SETTLED`, `REJECTED`, `CANCELLED`), no callback or statement link can ever change its status again — enforced by a database trigger with a distinct SQLSTATE (`ZK002`) the store maps precisely. Only the distinct, explicit `RecordReturn` command may move a `SETTLED` payment to `RETURNED` — matching the spec's own words, "governed final states cannot regress without explicit return/reversal semantics." Verified directly: a late callback reporting `PENDING` against an already-`SETTLED` payment is silently ignored (`applied: false`), confirmed as a negative control.
- **Negative-path scenario #3** ("duplicate callback creates duplicate accounting effect") is enforced by a genuine database uniqueness constraint on `(payment_id, provider_event_ref)` — a repeat callback for the same real-world event is idempotent, never double-applied.
- **Negative-path scenario #4** ("provider says settled but statement evidence conflicts and is ignored"): `LinkStatementConfirmation` compares its caller-supplied statement status against the current canonical status; a mismatch raises a real, blocking conflict record rather than silently overwriting either side. Only `ResolveStatusConflict` — requiring the stronger `PAYMENT_FINALITY_CONFIRM` action, matching the spec's own SoD line ("manual finality override requires exceptional controlled workflow and evidence") — can close it.

**Honest gap:** `PollPaymentStatus` has no real provider to poll (the same reason as BNK-06's Provider Adapter boundary — no real bank/PSP integration exists in this codebase), so it is a caller-attested command sharing the same apply-status logic as the real webhook, just tagged `MANUAL_POLL` instead of `PROVIDER_CALLBACK` and requiring an authorized principal instead of a signature — the same "record a real external fact a human observed" doctrine used throughout this session.

11 handler tests plus two verified negative controls (forged-signature rejection, settled-payment regression blocking).

Registered in `docker-compose.yml`/`init-db.sh` (port 8163, db `payment_status`, depends on `authorization-svc` only).

**Update, same day:** the wiring described above as a next step is now done — see §3.15.

---

### 3.15 `payment-run-svc` (AP-11) wired to BNK-06/BNK-07 — closes §3.12's and §3.14's documented gap (2026-09-01)

`SubmitPaymentRun` now really hands every instruction to Banking before transitioning the run itself, and a new `PollInstructionStatus` reconciles from BNK-07's real canonical status — the two pieces §3.12 and §3.14 both explicitly scoped as "not yet done."

**What's real, not fabricated:**
- `SubmitPaymentRun` calls BNK-06's `PrepareAttempt`+`SubmitAttempt` (via a new `internal/provideradapter` client) for every instruction, then registers the canonical execution record with BNK-07's `RecordPaymentStatus` (via a new `internal/paymentstatus` client) — the first real caller of either service anywhere in this codebase. This happens **before** the run itself transitions from `LOCKED` to `SUBMITTED`, mirroring `LockPaymentRun`'s own "do the real work first, only then transition; any failure raises `EXCEPTION` rather than a silent partial state" discipline. Verified directly: with BNK-06 stubbed to fail, the run is confirmed to move to `EXCEPTION` rather than reaching `SUBMITTED` — disabling that guard and re-running the test showed the run wrongly reaching `SUBMITTED` at `200`, confirmed as a negative control, then restored.
- The correlation to both services is durable, not just passed through: two new columns on `run_instructions` (`provider_attempt_id`, `bnk07_payment_id`, migration `000004_provider_refs`) are written exactly once and are immutable afterward — enforced by the same database trigger pattern used for every other authorized field on this table.
- `PollInstructionStatus` (new endpoint, `POST /ap11/instructions/{instructionID}/poll`) queries BNK-07's real `GetPaymentStatus` for the instruction's own `bnk07_payment_id` and reconciles the run from that real answer, sharing the same authorization/idempotent-apply/aggregate-recompute path as the existing `ReconcilePaymentRunStatus` (both now call a common `applyExternalStatus`). A poll while BNK-07 still reports `PREPARED`/`SUBMITTED`/`PENDING` correctly reports `applied: false` rather than fabricating progress — verified directly: forcing the "no news yet" branch to always report news caused the test to genuinely fail, confirmed as a negative control, then restored.
- `ReconcilePaymentRunStatus` (the old caller-attested command) is deliberately kept, not removed — a manual-override path for an operator recording a real external fact BNK-07 didn't itself capture, the same "record a real external fact a human observed" doctrine used throughout this session.
- The derived `ProviderEventRef` for a poll (`"<bnk07_payment_id>:<status>"`) is deterministic, so re-polling the same real status twice is idempotent through the exact same database uniqueness constraint `ReconcilePaymentRunStatus` already relies on — no separate idempotency mechanism was needed.
- `PayerAccountVerified` is asserted `true` when calling BNK-06 — not a new fabrication; AP-09/AP-10 already treat the paying bank account reference as opaque (no real account-status source exists anywhere in this codebase), and this is the exact same caller-attestation gate BNK-06's own package doc defines for that reason.

**What remains the one honest gap, now one layer further down:** BNK-06's own Provider Adapter behind `SubmitAttempt` is a documented stub (§3.13) — no actual bank/PSP network call exists anywhere in this codebase. Wiring AP-11 through BNK-06/BNK-07 for real moves the domain's one remaining honest gap down to exactly that boundary, and no further.

4 new handler tests (real Banking handoff, handoff-failure → `EXCEPTION`, real-status poll settling a run, poll-with-no-news not applying), plus the two negative controls above. Full existing AP-11 suite (17 tests total) still green.

`docker-compose.yml` updated: `payment-run-svc` now depends on `payment-initiation-adapter-svc`/`payment-status-svc` (`service_healthy`) and carries their base URLs (`PAYMENT_INITIATION_ADAPTER_URL`, `PAYMENT_STATUS_SERVICE_URL`). `docker compose config --quiet` validated clean.

---

## 4. Confirmations (no action needed, listed so they aren't re-litigated)

- `tenant-entity-registry-svc`'s schema matches `ZS-SVC-I-001`'s canonical Tenant→LegalEntity→OrgUnit model almost field-for-field; its RLS posture (explicit WHERE-clause enforcement, RLS as defense-in-depth only, because the runtime connects as Postgres superuser) exceeds the doc's minimum.
- `accounts-payable-svc` (unlike `accounts-receivable-svc`) does call authorize checks on all mutating routes, scoped by `legal_entity_id` — matches tracker item 82's documented finding that some flows use `legal_entity_id` scope rather than `tenant_id`, and the new IAM doc confirms `legal_entity_id` is the documented *primary* scope for financial data, not a workaround.
- `configuration-feature-flag-svc`'s `TenantID *string` (nil = global default) design matches `ZS-SVC-AA-001`'s CFG-02 model exactly, including the immutable-version/no-UPDATE-DELETE pattern.
- `retention-registry-svc`'s policy/hold model (never deletes anything itself) matches `ZS-SVC-S-001` DRC-03's stated authority exactly.
- The event envelope currently in use — see §5 below — was deliberately built against the *old* Doc 03 §19 and is confirmed as the old, superseded shape.

---

## 5. The one platform-wide breaking change: event envelope migration

`ZS-EVENT-001` replaces the current event envelope with a CloudEvents-1.0.2-aligned shape. This is not additive — it's a rename/restructure across every producer and consumer in the estate (~86 services):

| Current (`internal/events/publisher.go`, e.g. `accounts-payable-svc`) | New (`ZS-EVENT-001` §4) |
|---|---|
| `EventType` | `type` (pattern: `com.zoikosuite.<domain>.<aggregate>.<past-tense-fact>`) |
| `EmittedAt` | `time` |
| `TenantID` | `tenantid` |
| `LegalEntityID` | `legalentityid` |
| *(none)* | `specversion`, `id`, `source`, `subject`, `aggregateid`, `aggregateversion`, `causationid`, `dataschema`, `payloadhash`, `residencyregion`, `classification` |

Similarly, `ZS-API-001` replaces the current ad-hoc JSON error bodies (`{"error":"...", "message":"..."}`) with RFC 9457 `application/problem+json`, and replaces plain REST routes with a `POST .../actions/{verb}` command pattern plus mandatory `Idempotency-Key` and `If-Match`/ETag concurrency. Neither of these has any footprint in the current codebase.

**These two are estate-wide migrations, not per-service fixes.** Recommend treating them as their own initiative with explicit sequencing, not folding into the existing tenant-isolation/authorization tracker.

---

## 6. Document → service mapping reference

For future lookups. "—" means no corresponding service was found in `services/`.

| # | Doc ID | Title | Service(s) |
|---|---|---|---|
| 1 | ZS-DATA-001 | Canonical Data Model | (platform-wide reference) |
| 2 | ZS-ACC-KERNEL-001 | Accounting Kernel | general-ledger-svc (partial) |
| 3 | ZS-STATE-001 | State Machine/Workflow/Approval Catalogue | workflow-svc (partial) |
| 4 | ZS-API-001 | API/Command/Integration Contract | (platform-wide, unimplemented) |
| 5 | ZS-EVENT-001 | Domain Event Catalogue | (platform-wide, superseded envelope in use) |
| 6 | ZS-IAM-001 | Authorization/RBAC/ABAC/SoD | authorization-svc |
| 7 | ZS-JUR-001 | Jurisdiction Pack | jurisdiction-rules-svc |
| 8 | ZS-CONTROL-001 | Financial Reconciliation Control | reconciliation-intelligence-svc, bank-reconciliation-svc |
| 9 | ZS-SEC-001 | Security/Privacy/Residency/Crypto | key-management-svc, secret-vault-integration-svc (partial) |
| 10 | ZS-QA-001 | Verification/Certification/Acceptance | (methodology, not a service) |
| 11 | ZS-MIG-001 | Migration/Opening Balance/Cutover | — |
| 12 | ZS-NFR-001 | NFR/SLO/Resilience | (platform-wide) |
| 13 | ZS-OPS-001 | Operational Runbook | — |
| 14 | ZS-DATA-GOV-001 | Data Governance/Evidence/Retention/Legal Hold | evidence-manifest-svc, retention-registry-svc, audit-event-store-svc |
| 15 | ZS-SVC-SPEC-001 | Service Spec Template/Standard | (template for all ZS-SVC-*-001 docs) |
| 16 | ZS-SVC-A-001 | Governance & Control Plane | identity-context-svc, authorization-svc, policy-svc, workflow-svc, schema-registry-svc, configuration-feature-flag-svc, retention-registry-svc |
| 17 | ZS-SVC-B-001 | Accounting & Record-to-Report | general-ledger-svc, financial-close-svc |
| 18 | ZS-SVC-C-001 | Revenue/Billing/AR | accounts-receivable-svc |
| 19 | ZS-SVC-D-001 | Procurement/Expenses/AP | accounts-payable-svc, purchase-order-svc, purchase-request-svc, invoice-approval-svc |
| 20 | ZS-SVC-E-001 | Banking/Cash/Treasury | banking-connector-svc, treasury-svc |
| 21 | ZS-SVC-F-001 | Tax/E-Invoicing/Regulatory | tax-determination-svc, tax-rules-svc, corporate-tax-svc, vat-gst-svc, withholding-tax-svc, filing-preparation-svc, filing-tracker-svc, tax-authority-interface-svc |
| 22 | ZS-SVC-G-001 | Assets/Inventory/Project Accounting | — (entire domain) |
| 23 | ZS-SVC-H-001 | Financial/Statutory/Regulatory Reporting | reporting-orchestration-svc (stub only) |
| 24 | ZS-SVC-I-001 | Organization/Legal Entity/Reference Data | tenant-entity-registry-svc |
| 25 | ZS-SVC-J-001 | Planning/Forecasting/Performance | forecasting-svc (narrow) |
| 26 | ZS-SVC-K-001 | Audit & Assurance | — (engagement domain; audit-event-store-svc is infra only) |
| 27 | ZS-SVC-L-001 | Workforce & Payroll | payroll-run-svc, payroll-tax-svc, payroll-exceptions-svc, employee-master-svc, compensation-svc, benefits-svc, leave-absence-svc, employment-contracts-svc, performance-review-svc |
| 28 | ZS-SVC-M-001 | Integration & Interoperability | external-data-feed-svc, connectivity-api-bridge-svc |
| 29 | ZS-SVC-N-001 | Data/Search/Analytics/AI | search-indexer-svc, carta-svc (not covered by this doc) |
| 30 | ZS-SVC-O-001 | Legal/Corporate Governance/Contracts | contract-lifecycle-svc, clause-template-svc, corporate-actions-svc (uncovered), counterparty-management-svc (uncovered) |
| 31 | ZS-SVC-P-001 | Business Operations/Content Services | — |
| 32 | ZS-SVC-Q-001 | Commercial Billing/Subscription/Entitlement | commercial-account-svc |
| 33 | ZS-SVC-R-001 | Workflow/Approval/Case/Obligation Control | workflow-svc, workflow-history-svc, obligations-svc, obligation-tracking-svc, filing-tracker-svc |
| 34 | ZS-SVC-S-001 | Document/Record/Retention/Legal Hold | retention-registry-svc, evidence-manifest-svc |
| 35 | ZS-SVC-T-001 | External Integration/Connector Control | banking-connector-svc, connectivity-api-bridge-svc, esignature-integration-svc, external-data-feed-svc, hris-connector-svc, tax-authority-interface-svc |
| 36 | ZS-SVC-U-001 | Enterprise Master Data/Party | tenant-entity-registry-svc |
| 37 | ZS-SVC-V-001 | Policy/Rules/Decision/Jurisdictional Applicability | policy-svc, jurisdiction-rules-svc |
| 38 | ZS-SVC-W-001 | Privacy/Consent/Data Rights | — (entire domain) |
| 39 | ZS-SVC-X-001 | AI Governance | ai-governance-svc (narrow) |
| 40 | ZS-SVC-Y-001 | Notification/Communication/Delivery | notification-svc (narrow) |
| 41 | ZS-SVC-Z-001 | Data Quality/Reconciliation/Lineage | reconciliation-intelligence-svc (see §0 — stub logic) |
| 42 | ZS-SVC-AA-001 | Config/Feature Flag/Change Control | configuration-feature-flag-svc |
| 43 | ZS-SVC-AB-001 | Search/Indexing/Secure Retrieval | search-indexer-svc (ingestion only, broken — row 65a) |
| 44 | ZS-SVC-AC-001 | Governed Reporting/Analytics/Export | reporting-orchestration-svc (see §0 — simulated) |
| extra | ZS-COM-PFE-001 | Commercial Plan/Feature Entitlement Allocation | commercial-account-svc (supplementary to #32) |
| extra | ZS-COMMS-EMAIL-001 | Email Communications System v2.0 | notification-svc (supplementary to #40, email channel detail) |
| extra | ZS-ARCH-SVC-001 | Global Service Requirements Input Contract Catalogue v2.0 | (platform-wide, older baseline predating the numbered register) |

---

## 7. Recommended next steps

1. **Triage §2 (new security/correctness findings) into the tracker as new rows.** These are real, scoped, fixable — same shape as the Priority 2b work.
2. **Decide on §0 (fabricated success) as its own priority tier**, ranked above domain-coverage gaps — it's a correctness/trust issue, not a missing-feature issue.
3. **Close/annotate §1 items** (policy-svc false positive, workflow-history-svc fix confirmed, jurisdiction-rules-svc confirmed, row 65a diagnosis confirmed) — no code change, just tracker bookkeeping.
4. **§3 and §6 are backlog/roadmap material**, not tracker fix rows — hand to whoever owns prioritization across the 44-document register as a distinct planning exercise from the tenant-isolation/authorization work.
5. **§5 (event envelope + API contract migration) needs its own initiative** given the estate-wide blast radius — not something to fold into existing work opportunistically.
