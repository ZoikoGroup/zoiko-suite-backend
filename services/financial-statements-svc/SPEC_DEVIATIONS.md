# Spec deviations — financial-statements-svc (REP-01, ZS-SVC-H-001 §4.1)

This is v1. It implements a real, durable, SoD-enforced workflow over a
genuinely certified source, but it is narrower than the full spec in named
ways. Read this before relying on anything here for a real filing.

## A. Not implemented / weaker than the spec

- **A1. ACC-16's `Content` field has no platform-wide schema.** financial-close-svc's
  `FinancialSnapshot.Content` is a free-form string; nothing in the platform
  defines what goes inside it, and no producer (general-ledger-svc or
  otherwise) has ever emitted a real one — the only existing test fixture is
  the literal string `"x"`. `internal/domain.SnapshotContent` is the schema
  *this service* requires and documents, not a platform-agreed one. Until a
  real producer emits this shape, every statement set in this service must be
  built from a hand-crafted snapshot.
- **A2. Only the Balance Sheet and Income Statement are produced.** A real
  indirect-method Cash Flow Statement needs opening AND closing balances plus
  a transaction classification engine — a materially separate feature, not
  attempted here (the spec's own "Mandatory engineering control" that "cash
  flow construction method and mapping are versioned" is therefore
  unaddressed).
- **A3. PresentationMappingUse is one fixed category → statement-section map**
  (`domain.MappingVersionV1`), not a per-tenant/per-framework/per-jurisdiction
  versioned registry. Every account's statement placement is derived solely
  from its `category` (ASSET/LIABILITY/EQUITY/REVENUE/EXPENSE); there is no
  way to override where one specific account line is reported.
- **A4. No aggregation.** One StatementFact per source account. A real
  statement groups many accounts into one reported line (e.g. all cash
  accounts summed into "Cash and cash equivalents"); this version reports
  every account individually.
- **A5. DisclosureInstance (narrative disclosures/notes) is not built.** This
  service produces primary statements only.
- **A6. ComparativePresentation is two-period, pairwise only.** A set can be
  read side by side with one explicitly named prior set (see A7 and B8). Not
  built: N-period trends (three or more columns), restated/re-presented
  prior-period figures, any presentation formatting or export (PDF, XBRL,
  spreadsheet), and comparatives stored as part of a set — a comparative is
  computed on read and never certified or published itself.
- **A7. `GetComparatives` exists, with limits.**
  `GET /v1/statement-sets/{id}/comparatives?prior_set_id=<id>` pairs both
  sets' facts per (statement type, account code), with explicit nulls for an
  account present in only one period, and an exact-decimal `change`
  (current − prior, `big.Rat`) only when both sides exist. The caller must
  name the prior set: the service does not find "the previous period"
  itself, because `reporting_period` is a free-form label and this service
  has no fiscal calendar to order periods by. Because of A4, lines pair per
  source account, not per aggregated reported line.
- **A8. ACC-18 (source-to-report lineage) is not integrated as a graph.**
  `ExplainReportedFact` names the source snapshot ID and its content hash —
  real, verifiable provenance — but does not walk an actual lineage graph
  back to individual journal entries in general-ledger-svc.
- **A9. No numeric SLOs measured.** `SLO.md` states targets; none were load
  tested.
- **A10. No mTLS by default** — `AuthzMTLSEnabled` exists and is wired, off
  by default, matching every other new service's pilot posture.

## B. Interpretations and additions (decisions a reviewer may want to reverse)

- **B1. `CreateStatementSet` verifies `snapshot.legal_entity_id` matches the
  request's `legal_entity_id`.** The spec does not say this explicitly; it is
  a direct reading of "certified output must bind to ACC-16 Signed Financial
  Snapshot" — binding to a snapshot from a *different* legal entity would
  defeat that control silently.
- **B2. `REQUEST_CHANGES` (IN_REVIEW → DRAFT) is added.** The spec's own state
  model ("Draft → Validated → InReview → Approved → Certified →
  Published/Superseded") names no reviewer-rejection path. Without one, a
  reviewer who finds a problem has no way to send the set back — the
  workflow would otherwise be a dead end for any review that isn't a
  rubber-stamp approval.
- **B3. SoD enforces `preparer != certifier` and, additionally,**
  **`preparer != approver`.** The spec only states the first ("Preparer
  cannot independently certify material statutory statements"). The second
  is this service's own four-eyes addition for the APPROVE command, not
  spec-mandated — a reviewer may want to relax it if a single-reviewer
  workflow is intended for non-material entities.
- **B4. "Preparer" is defined as the most recent refresher, falling back to**
  **the creator** (`FinancialStatementSet.Preparer()`), not just whoever
  created the row. Refreshing a draft with new facts is authorship; treating
  only the original creator as "the preparer" would let someone who entirely
  reshaped the content before submission still certify their own work.
- **B5. At most one non-superseded statement set per**
  **(tenant, legal_entity, reporting_period, framework)**, enforced by a
  partial unique index. Not spec-mandated in those words, but a direct
  reading of "certified output is immutable; a correction is always a new
  set" — without this constraint nothing stops two independently-progressing
  drafts for what is supposed to be one real-world report.
- **B6. `ValidateTieOut` only checks Assets = Liabilities + Equity.** This is
  the one tie-out this version's narrower scope (A2) makes well-defined.
  Cross-casts between statements, and any check that depends on the Cash
  Flow Statement, are out of scope until A2 is addressed.
- **B7. Amounts are exact-decimal strings (`big.Rat` arithmetic), never**
  **`float64`** — ZS-DATA-001 D06 (binary floating point prohibited for
  monetary values), the same rule this platform's other accounting services
  (e.g. financial-control-svc) already follow.
- **B8. `GetComparatives` refuses comparisons it cannot stand behind.** The
  prior set must be CERTIFIED or PUBLISHED (422 `SOURCE_UNVERIFIED`; a
  SUPERSEDED prior is refused too, naming its replacement) — the same
  fail-closed posture as B1. It must match the current set's legal entity,
  framework, presentation currency and mapping version, and cover a
  different reporting period (422 `CONTEXT_INVALID`). The *current* set may
  be in any state, since preparing a draft against last period's certified
  figures is the main use. A line in only one period gets a null `change`,
  not one computed against zero: an absent line is not a zero balance.
