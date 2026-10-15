# Spec deviations — financial-statements-svc (REP-01, ZS-SVC-H-001 §4.1)

This is v1. It implements a real, durable, SoD-enforced workflow over a
genuinely certified source, but it is narrower than the full spec in named
ways. Read this before relying on anything here for a real filing.

## A. Not implemented / weaker than the spec

- **A1. ACC-16's `Content` field has no platform-wide schema.** financial-close-svc's
  `FinancialSnapshot.Content` is a free-form string; nothing in the platform
  defines what goes inside it. A producer now exists:
  financial-close-svc's `POST /v1/financial-snapshots/from-trial-balance`
  (commit 8083630e, branch satyaprakash-changes) compiles general-ledger-svc's
  trial balance + chart of accounts into a Balance Sheet-shaped `SnapshotContent`
  (one currency, no multi-book/consolidation source, no REP-02…07 statement
  types). `internal/domain.SnapshotContent` is the schema *this service*
  requires and documents, not a platform-agreed one. Until the platform agrees
  on a schema, statement sets built from other sources remain hand-crafted.
- **A2. Only the Balance Sheet and Income Statement are produced.** A real
  indirect-method Cash Flow Statement needs opening AND closing balances plus
  a transaction classification engine — a materially separate feature, not
  attempted here (the spec's own "Mandatory engineering control" that "cash
  flow construction method and mapping are versioned" is therefore
  unaddressed).
- **A3. PresentationMappingUse is a fixed, code-defined mapping, not the**
  **spec's registry.** The spec names PresentationMappingUse as the entity
  that maps accounts to report lines but defines no grouping rule; this
  service ships two hard-coded versions instead of a
  per-tenant/per-framework/per-jurisdiction versioned registry:
  - `v1-fixed-category-map` (`MappingVersionV1`): statement placement from
    `category` alone (ASSET/LIABILITY/EQUITY/REVENUE/EXPENSE), one line per
    source account.
  - `v2-category-map-name-aggregation` (`MappingVersionV2`, used for every
    set created from now on): the same placement, plus accounts on the same
    statement, in the same category, with **exactly** the same non-empty
    `account_name` become one line whose amount is their exact `big.Rat`
    sum. Category is part of the key so a balance can never move between
    tie-out buckets (B6 is unaffected); unnamed accounts never merge; names
    are compared byte-for-byte (`"Cash"`, `"Cash "` and `"cash"` are three
    lines). This is a placeholder step toward the real entity, not it:
    grouping by name is the only signal available, because the ACC-16
    snapshot carries no account hierarchy. general-ledger-svc's chart of
    accounts *does* have one (`ParentAccountID`), but threading it through
    financial-close-svc's snapshot producer into here is a three-service
    change not attempted. There is still no way to override where one
    specific account is reported, or to give a line a presentation label
    other than its accounts' shared name.

  A set records the mapping version it was created with and is built with
  that version for its whole life: sets created before v2 (including
  certified ones) keep one line per account on every refresh.
  Each aggregated fact stores every source account with its own balance
  (`source_accounts`, migration 000003); `ExplainReportedFact` returns all
  of them, flags `aggregated`, and refuses (422 `VALIDATION_FAILED`) a fact
  whose stored sources do not sum to its amount.
- **A4. Aggregation is name-based only — part of A3.** Since
  `MappingVersionV2`, same-named, same-category accounts are reported as one
  line (see A3 for the exact rule and its limits). A real statement groups
  *differently named* accounts into a labelled line (e.g. "Petty cash" and
  "Bank — current" into "Cash and cash equivalents"); that needs either the
  chart-of-accounts hierarchy or the PresentationMappingUse registry, and
  neither is available here (A3).
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
  has no fiscal calendar to order periods by. Lines pair at the sets' own
  mapping granularity: by account code under v1; by exact account name under
  v2 (same category first, then one leftover line per side as a
  reclassification), with each side's `*_source_account_codes` shown so a
  change in a line's composition is visible, not just its total.
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
  different reporting period (422 `CONTEXT_INVALID`). The mapping-version
  rule means a v2 set cannot be compared with a v1 prior: per-account and
  aggregated lines are different granularities, and re-presenting a
  certified v1 set's figures under v2 on the fly is not done. The first v2
  period therefore has no comparative until the prior period is itself
  superseded under v2. The *current* set may
  be in any state, since preparing a draft against last period's certified
  figures is the main use. A line in only one period gets a null `change`,
  not one computed against zero: an absent line is not a zero balance.
