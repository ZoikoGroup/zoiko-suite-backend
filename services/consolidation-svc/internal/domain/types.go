package domain

import "time"

type ConsolidationRun struct {
	ConsolidationRunID string     `json:"consolidation_run_id"`
	TenantID           string     `json:"tenant_id"`
	GroupLegalEntityID string     `json:"group_legal_entity_id"`
	FiscalPeriod       string     `json:"fiscal_period"`
	TargetCurrency     string     `json:"target_currency"`
	Status             string     `json:"status"` // RUNNING, COMPLETED, FAILED
	ExceptionCount     int        `json:"exception_count"`
	StartedAt          time.Time  `json:"started_at"`
	CompletedAt        *time.Time `json:"completed_at,omitempty"`
}

type BalanceSnapshot struct {
	BalanceSnapshotID   string    `json:"balance_snapshot_id"`
	TenantID            string    `json:"tenant_id"`
	ConsolidationRunID  string    `json:"consolidation_run_id"`
	LegalEntityID       string    `json:"legal_entity_id"`
	FiscalPeriod        string    `json:"fiscal_period"`
	AccountCode         string    `json:"account_code"`
	ConsolidatedBalance float64   `json:"consolidated_balance"`
	CurrencyCode        string    `json:"currency_code"`
	SnapshotSignature   string    `json:"snapshot_signature"`
	GeneratedAt         time.Time `json:"generated_at"`
}

// BalanceContribution is the ACC-13 entity-to-group provenance record this
// domain was missing: one row per (run, account_code, source child entity),
// recorded BEFORE elimination — exactly what that child's own trial balance
// reported, so a later reader can answer "which entities make up this group
// line" without re-running the whole consolidation.
type BalanceContribution struct {
	BalanceContributionID string    `json:"balance_contribution_id"`
	TenantID              string    `json:"tenant_id"`
	ConsolidationRunID    string    `json:"consolidation_run_id"`
	AccountCode           string    `json:"account_code"`
	SourceLegalEntityID   string    `json:"source_legal_entity_id"`
	GrossAmount           float64   `json:"gross_amount"`
	GeneratedAt           time.Time `json:"generated_at"`
}

type StartConsolidationRequest struct {
	GroupLegalEntityID  string   `json:"group_legal_entity_id"`
	ChildLegalEntityIDs []string `json:"child_legal_entity_ids"`
	FiscalPeriod        string   `json:"fiscal_period"`
	TargetCurrency      string   `json:"target_currency"`
}

type ConsolidationRunResponse struct {
	ConsolidationRunID string            `json:"consolidation_run_id"`
	GroupLegalEntityID string            `json:"group_legal_entity_id"`
	FiscalPeriod       string            `json:"fiscal_period"`
	Status             string            `json:"status"`
	ExceptionCount     int               `json:"exception_count"`
	StartedAt          time.Time         `json:"started_at"`
	Snapshots          []BalanceSnapshot `json:"snapshots,omitempty"`
}

// ConsolidationAdjustment is ACC-12's own authority — "owns Consolidation
// adjustment lifecycle. Must never own: Entity statutory ledgers." Fuller
// ownership: "ConsolidationAdjustment, elimination rule version, approval
// state and consolidation-book journal ref." State model (verbatim):
// "Draft → PendingApproval → Approved → Posted → Reversed/Superseded."
//
// Distinct from the automatic elimination StartRun already performs
// (handler.go's eliminateMatchedEntry): that path only ever adjusts the
// in-memory snapshot math for one run, is never itself an approved,
// posted, reversible accounting fact, and posts nothing to any ledger.
// A ConsolidationAdjustment is a governed, human-approved journal that
// actually posts to general-ledger-svc under the group entity — see
// migration 000003's doc comment.
type ConsolidationAdjustment struct {
	ConsolidationAdjustmentID string  `json:"consolidation_adjustment_id"`
	TenantID                  string  `json:"tenant_id"`
	GroupLegalEntityID        string  `json:"group_legal_entity_id"`
	FiscalPeriod              string  `json:"fiscal_period"`
	AdjustmentType            string  `json:"adjustment_type"` // ELIMINATION | MANUAL
	Description               string  `json:"description"`
	Status                    string  `json:"status"`
	Lines                     []ConsolidationAdjustmentLine `json:"lines"`

	ConsolidationBookJournalID *string `json:"consolidation_book_journal_id,omitempty"`

	CreatedAt            time.Time  `json:"created_at"`
	CreatedByPrincipalID string     `json:"created_by_principal_id"`
	ApprovedAt           *time.Time `json:"approved_at,omitempty"`
	ApprovedByPrincipalID *string   `json:"approved_by_principal_id,omitempty"`
	PostedAt             *time.Time `json:"posted_at,omitempty"`
	PostedByPrincipalID  *string    `json:"posted_by_principal_id,omitempty"`
	ReversedAt           *time.Time `json:"reversed_at,omitempty"`
	ReversedByPrincipalID *string   `json:"reversed_by_principal_id,omitempty"`
	ReversalReason       *string    `json:"reversal_reason,omitempty"`

	// SupersededByAdjustmentID is set only when Reverse was called WITH a
	// replacement adjustment named — the spec's own "Reversed/Superseded"
	// grouping. Nil means a plain reversal with no replacement.
	SupersededByAdjustmentID *string `json:"superseded_by_adjustment_id,omitempty"`
}

type ConsolidationAdjustmentLine struct {
	AccountCode  string  `json:"account_code"`
	DebitAmount  float64 `json:"debit_amount,omitempty"`
	CreditAmount float64 `json:"credit_amount,omitempty"`
}

const (
	AdjustmentTypeElimination = "ELIMINATION"
	AdjustmentTypeManual      = "MANUAL"
)

const (
	AdjustmentStatusPendingApproval = "PENDING_APPROVAL"
	AdjustmentStatusApproved        = "APPROVED"
	AdjustmentStatusPosted          = "POSTED"
	AdjustmentStatusReversed        = "REVERSED"
)

type CreateEliminationProposalRequest struct {
	GroupLegalEntityID string                        `json:"group_legal_entity_id"`
	FiscalPeriod       string                        `json:"fiscal_period"`
	AdjustmentType     string                        `json:"adjustment_type"`
	Description        string                        `json:"description"`
	Lines              []ConsolidationAdjustmentLine `json:"lines"`
}

type ReverseConsolidationAdjustmentRequest struct {
	Reason                   string  `json:"reason"`
	SupersededByAdjustmentID *string `json:"superseded_by_adjustment_id,omitempty"`
}

type errorString string

func (e errorString) Error() string { return string(e) }

var (
	ErrRunNotFound             = errorString("consolidation run not found")
	ErrGLServiceUnavailable    = errorString("general-ledger-svc unavailable")
	ErrIntercompanyUnavailable = errorString("intercompany-accounting-svc unavailable")
	ErrAuthorizationDenied     = errorString("authorization denied for consolidation action")
	ErrAuthzServiceUnavailable = errorString("authorization-svc unavailable")
	ErrIdentityMissing         = errorString("caller identity missing")
	ErrStoreUnavailable        = errorString("consolidation store unavailable")

	// ── ACC-12 Elimination & Consolidation Adjustments ──────────────────

	ErrAdjustmentNotFound = errorString("consolidation adjustment not found")

	// ErrAdjustmentTargetsStatutoryBook is the spec's own negative path
	// "Adjustment targets statutory book": group_legal_entity_id must be
	// an entity this tenant has actually run a consolidation FOR as a
	// group entity (a real, checkable fact via ListRuns) — never an
	// arbitrary legal entity a caller could name, which would let an
	// "adjustment" quietly rewrite a child's own statutory ledger instead
	// of the consolidation-only book.
	ErrAdjustmentTargetsStatutoryBook = errorString("group_legal_entity_id has never been used as a consolidation group entity for this tenant")

	// ErrInvalidAdjustmentTransition covers every ACC-12 lifecycle command
	// called against an adjustment not in the one status it requires.
	ErrInvalidAdjustmentTransition = errorString("consolidation adjustment is not in a status that allows this action")

	// ErrSelfApprovalNotPermitted is the spec's own negative path "Top-side
	// journal self-approved" — the same maker/checker posture ACC-03
	// applies to journal approval.
	ErrSelfApprovalNotPermitted = errorString("the principal who created this adjustment may not also approve it")

	// ErrEliminationExceedsMatchedBalance is the spec's own negative path
	// "Elimination exceeds matched reciprocal balance": an ELIMINATION
	// adjustment's own lines must never exceed the real MATCHED
	// intercompany balance it is eliminating.
	ErrEliminationExceedsMatchedBalance = errorString("elimination adjustment amount exceeds the matched intercompany reciprocal balance")

	// ErrReversalRequiresSupersession is the spec's own negative path
	// "Reverse after snapshot without supersession": once a group/period
	// has a real BalanceSnapshot on record, a POSTED adjustment can only
	// be reversed by naming its replacement, never bare.
	ErrReversalRequiresSupersession = errorString("a snapshot already exists for this group/period; reversal requires naming a superseding adjustment")

	ErrReasonRequired = errorString("reason is required to reverse a consolidation adjustment")
)
