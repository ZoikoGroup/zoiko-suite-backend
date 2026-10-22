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

	// IntercompanyAccountCodes are the accounts on which this group books
	// intercompany balances and activity: the lines of a pair's journals on
	// these accounts are its intercompany lines, and only those are
	// eliminated (SPEC_DEVIATIONS B9). The spec's "elimination mapping" has
	// no platform source yet, so it is declared per run, as
	// financial-control-svc's FIN-CTRL-014/015 declare ic_receivable_accounts
	// and ic_payable_accounts.
	IntercompanyAccountCodes []string `json:"intercompany_account_codes,omitempty"`
}

type ConsolidationRunResponse struct {
	ConsolidationRunID string            `json:"consolidation_run_id"`
	GroupLegalEntityID string            `json:"group_legal_entity_id"`
	FiscalPeriod       string            `json:"fiscal_period"`
	Status             string            `json:"status"`
	ExceptionCount     int               `json:"exception_count"`
	StartedAt          time.Time         `json:"started_at"`
	Snapshots          []BalanceSnapshot `json:"snapshots,omitempty"`
	Eliminations       []Elimination     `json:"eliminations,omitempty"`
}

// RunNotCompletedResponse is what StartRun answers when its final CompleteRun
// write failed. The run exists — it was created RUNNING before any of the work
// — so the response names the run and the state the store actually holds,
// rather than a 201 COMPLETED for a status that was never persisted (A14).
type RunNotCompletedResponse struct {
	ErrorCode          string `json:"error_code"`
	ErrorMessage       string `json:"error_message"`
	ConsolidationRunID string `json:"consolidation_run_id"`
	Status             string `json:"status"`
}

// ConsolidationAdjustment is ACC-12's own authority — "owns Consolidation
// adjustment lifecycle. Must never own: Entity statutory ledgers." Fuller
// ownership: "ConsolidationAdjustment, elimination rule version, approval
// state and consolidation-book journal ref." State model (verbatim):
// "Draft → PendingApproval → Approved → Posted → Reversed/Superseded."
//
// Distinct from the automatic elimination StartRun already performs
// (handler.go's eliminateInScope): that path adjusts the snapshot math for
// one run and records what it did (consolidation_eliminations), is never itself an approved,
// posted, reversible accounting fact, and posts nothing to any ledger.
// A ConsolidationAdjustment is a governed, human-approved journal that
// actually posts to general-ledger-svc under the group entity — see
// migration 000003's doc comment.
type ConsolidationAdjustment struct {
	ConsolidationAdjustmentID string                        `json:"consolidation_adjustment_id"`
	TenantID                  string                        `json:"tenant_id"`
	GroupLegalEntityID        string                        `json:"group_legal_entity_id"`
	FiscalPeriod              string                        `json:"fiscal_period"`
	AdjustmentType            string                        `json:"adjustment_type"` // ELIMINATION | MANUAL
	Description               string                        `json:"description"`
	Status                    string                        `json:"status"`
	Lines                     []ConsolidationAdjustmentLine `json:"lines"`

	ConsolidationBookJournalID *string `json:"consolidation_book_journal_id,omitempty"`

	CreatedAt             time.Time  `json:"created_at"`
	CreatedByPrincipalID  string     `json:"created_by_principal_id"`
	ApprovedAt            *time.Time `json:"approved_at,omitempty"`
	ApprovedByPrincipalID *string    `json:"approved_by_principal_id,omitempty"`
	PostedAt              *time.Time `json:"posted_at,omitempty"`
	PostedByPrincipalID   *string    `json:"posted_by_principal_id,omitempty"`
	ReversedAt            *time.Time `json:"reversed_at,omitempty"`
	ReversedByPrincipalID *string    `json:"reversed_by_principal_id,omitempty"`
	ReversalReason        *string    `json:"reversal_reason,omitempty"`

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

// Command names recorded in command_idempotency.command (migration 000008).
// Each names what was decided, not just which endpoint was called, so one key
// cannot replay a different command on the same target. They are domain
// constants because both the HTTP handler and the store need the exact text.
const (
	CommandStartRun          = "START_RUN"
	CommandCreateAdjustment  = "CREATE_ADJUSTMENT"
	CommandApproveAdjustment = "APPROVE_ADJUSTMENT"
	CommandPostAdjustment    = "POST_ADJUSTMENT"
	CommandReverseAdjustment = "REVERSE_ADJUSTMENT"
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

// AdjustmentTransition is one row of the append-only lifecycle log
// (consolidation_adjustment_transitions, migration 000006). FromStatus is
// nil for the creation step; Reason is set on a reversal; Details carries
// step-specific evidence (the consolidation-book journal id on POSTED, the
// superseding adjustment id on REVERSED).
type AdjustmentTransition struct {
	TransitionID              string         `json:"transition_id"`
	ConsolidationAdjustmentID string         `json:"consolidation_adjustment_id"`
	Sequence                  int64          `json:"sequence"`
	FromStatus                *string        `json:"from_status"`
	ToStatus                  string         `json:"to_status"`
	ActorPrincipalID          string         `json:"actor_principal_id"`
	OccurredAt                time.Time      `json:"occurred_at"`
	CorrelationID             string         `json:"correlation_id,omitempty"`
	Reason                    *string        `json:"reason,omitempty"`
	Details                   map[string]any `json:"details,omitempty"`
}

// AdjustmentHistory is GetAdjustmentHistory's answer: the adjustment's
// current identity and status plus every transition in the order it
// happened. Transitions is never null.
type AdjustmentHistory struct {
	ConsolidationAdjustmentID string                 `json:"consolidation_adjustment_id"`
	GroupLegalEntityID        string                 `json:"group_legal_entity_id"`
	FiscalPeriod              string                 `json:"fiscal_period"`
	AdjustmentType            string                 `json:"adjustment_type"`
	Status                    string                 `json:"status"`
	Transitions               []AdjustmentTransition `json:"transitions"`
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

	// ErrInvalidSupersession: the adjustment named as the replacement is
	// this same adjustment, belongs to a different group or period, or has
	// itself been reversed — so it replaces nothing.
	ErrInvalidSupersession = errorString("superseded_by_adjustment_id must name a different, unreversed adjustment for the same group and fiscal period")

	ErrReasonRequired = errorString("reason is required to reverse a consolidation adjustment")

	// ── Command idempotency (migration 000008) ──────────────────────────

	// ErrIdempotencyKeyRequired: every mutating command in this service
	// creates a run or moves an adjustment, so the caller must supply the
	// Idempotency-Key that lets a retry be recognised as one.
	ErrIdempotencyKeyRequired = errorString("an Idempotency-Key is required for this command")

	// ErrIdempotencyKeyConflict: the key was already used for a different
	// command or a different resource. Replaying it would return another
	// command's result, so it is refused rather than guessed at.
	ErrIdempotencyKeyConflict = errorString("idempotency key already used for a different command or resource")
)
