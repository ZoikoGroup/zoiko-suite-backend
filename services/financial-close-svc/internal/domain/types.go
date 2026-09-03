package domain

import "time"

type FiscalPeriod struct {
	FiscalPeriodID     string     `json:"fiscal_period_id"`
	TenantID           string     `json:"tenant_id"`
	LegalEntityID      string     `json:"legal_entity_id"`
	PeriodName         string     `json:"period_name"`
	PeriodStart        time.Time  `json:"period_start"`
	PeriodEnd          time.Time  `json:"period_end"`
	CloseStatus        string     `json:"close_status"` // OPEN, CLOSED, LOCKED
	CloseLockedAt      *time.Time `json:"close_locked_at,omitempty"`
	EvidenceDocumentID *string    `json:"evidence_document_id,omitempty"`
}

// PeriodReopenEvent is a permanent, append-only record that a LOCKED period
// was reopened, and why — ACC-14 invariant #6: "reopen is explicit, scoped,
// approved and evidenced." Never updated or deleted (database-enforced, see
// migration 000003).
type PeriodReopenEvent struct {
	ReopenEventID         string    `json:"reopen_event_id"`
	TenantID              string    `json:"tenant_id"`
	FiscalPeriodID        string    `json:"fiscal_period_id"`
	Reason                string    `json:"reason"`
	ReopenedByPrincipalID string    `json:"reopened_by_principal_id"`
	ReopenedAt            time.Time `json:"reopened_at"`
}

type ReopenPeriodRequest struct {
	Reason string `json:"reason"`
}

// SubledgerControlRun is ACC-06's real subledger-to-GL reconciliation
// record — a genuine balance comparison between a subledger's own total
// and its GL control account, not an existence-count of open items. See
// migration 000004's doc comment for why the run and its exception
// outcome are one row, not two separately-lifecycled objects.
type SubledgerControlRun struct {
	ControlRunID           string    `json:"control_run_id"`
	TenantID               string    `json:"tenant_id"`
	LegalEntityID          string    `json:"legal_entity_id"`
	FiscalPeriod           string    `json:"fiscal_period"`
	Subledger              string    `json:"subledger"` // AP | AR
	ControlAccountCode     string    `json:"control_account_code"`
	SubledgerTotalAmount   float64   `json:"subledger_total_amount"`
	GLControlBalanceAmount float64   `json:"gl_control_balance_amount"`
	DifferenceAmount       float64   `json:"difference_amount"`
	Status                 string    `json:"status"` // MATCHED | EXCEPTION
	RunAt                  time.Time `json:"run_at"`
	RunByPrincipalID       string    `json:"run_by_principal_id"`
}

type RunSubledgerControlRequest struct {
	LegalEntityID            string `json:"legal_entity_id"`
	FiscalPeriod             string `json:"fiscal_period"`
	Subledger                string `json:"subledger"` // AP | AR
	ControlAccountMappingKey string `json:"control_account_mapping_key"`
}

// Accrual schedule lifecycle states (ACC-07's state model, verbatim from
// spec: "Draft -> PendingApproval -> Approved -> Active ->
// Completed/Cancelled/Superseded").
const (
	AccrualStatusDraft           = "DRAFT"
	AccrualStatusPendingApproval = "PENDING_APPROVAL"
	AccrualStatusApproved        = "APPROVED"
	AccrualStatusActive          = "ACTIVE"
	AccrualStatusCompleted       = "COMPLETED"
	AccrualStatusCancelled       = "CANCELLED"
)

// AccrualSchedule is ACC-07's own authority — "AccrualSchedule, basis/
// evidence, recognition instances and reversal plan," explicitly never
// "Direct ledger writes": recognition posts through general-ledger-svc's
// own Create/Validate/Post journal lifecycle, the same as every other
// caller, rather than this service writing ledger tables itself.
type AccrualSchedule struct {
	ScheduleID        string  `json:"schedule_id"`
	TenantID          string  `json:"tenant_id"`
	LegalEntityID     string  `json:"legal_entity_id"`
	Description       string  `json:"description"`
	PolicyVersion     string  `json:"policy_version"`
	TotalAmount       float64 `json:"total_amount"`
	StartFiscalPeriod string  `json:"start_fiscal_period"` // 'YYYY-MM'
	PeriodCount       int     `json:"period_count"`
	DebitAccountCode  string  `json:"debit_account_code"`
	CreditAccountCode string  `json:"credit_account_code"`
	Status            string  `json:"status"`

	CreatedAt              time.Time  `json:"created_at"`
	CreatedByPrincipalID   string     `json:"created_by_principal_id"`
	SubmittedAt            *time.Time `json:"submitted_at,omitempty"`
	SubmittedByPrincipalID *string    `json:"submitted_by_principal_id,omitempty"`
	ApprovedAt             *time.Time `json:"approved_at,omitempty"`
	ApprovedByPrincipalID  *string    `json:"approved_by_principal_id,omitempty"`
	CancelledAt            *time.Time `json:"cancelled_at,omitempty"`
	CancelledByPrincipalID *string    `json:"cancelled_by_principal_id,omitempty"`
}

type CreateAccrualRequest struct {
	LegalEntityID     string  `json:"legal_entity_id"`
	Description       string  `json:"description"`
	PolicyVersion     string  `json:"policy_version"`
	TotalAmount       float64 `json:"total_amount"`
	StartFiscalPeriod string  `json:"start_fiscal_period"`
	PeriodCount       int     `json:"period_count"`
	DebitAccountCode  string  `json:"debit_account_code"`
	CreditAccountCode string  `json:"credit_account_code"`
}

type AmendFutureScheduleRequest struct {
	TotalAmount float64 `json:"total_amount"`
	PeriodCount int     `json:"period_count"`
}

// RecognitionInstance is ACC-07's permanent evidence that one period of an
// accrual schedule was recognized — see migration 000005's doc comment for
// why it is append-only and unique per (schedule_id, fiscal_period).
type RecognitionInstance struct {
	RecognitionInstanceID   string    `json:"recognition_instance_id"`
	TenantID                string    `json:"tenant_id"`
	ScheduleID              string    `json:"schedule_id"`
	FiscalPeriod            string    `json:"fiscal_period"`
	RecognizedAmount        float64   `json:"recognized_amount"`
	JournalID               string    `json:"journal_id"`
	RecognizedAt            time.Time `json:"recognized_at"`
	RecognizedByPrincipalID string    `json:"recognized_by_principal_id"`
}

type RunAccrualRecognitionRequest struct {
	FiscalPeriod string `json:"fiscal_period"`
}

// Prepayment schedule lifecycle states (ACC-08's state model, verbatim
// from spec: "Draft -> Approved -> Active ->
// Completed/Terminated/Superseded"). No PendingApproval step in this
// state model, unlike ACC-07 — Approve acts directly on DRAFT.
const (
	PrepaymentStatusDraft      = "DRAFT"
	PrepaymentStatusApproved   = "APPROVED"
	PrepaymentStatusActive     = "ACTIVE"
	PrepaymentStatusCompleted  = "COMPLETED"
	PrepaymentStatusTerminated = "TERMINATED"
)

const (
	TerminationTreatmentWriteOff           = "WRITE_OFF"
	TerminationTreatmentRecognizeRemaining = "RECOGNIZE_REMAINING"
)

// PrepaymentSchedule is ACC-08's own authority — "RecognitionSchedule,
// remaining balance, period recognition instances and schedule versions,"
// explicitly never "Direct ledger writes." Economically the mirror of
// ACC-07's AccrualSchedule: recognizes an already-paid prepaid asset into
// expense over time, rather than a not-yet-paid liability.
type PrepaymentSchedule struct {
	ScheduleID        string  `json:"schedule_id"`
	TenantID          string  `json:"tenant_id"`
	LegalEntityID     string  `json:"legal_entity_id"`
	Description       string  `json:"description"`
	TotalAmount       float64 `json:"total_amount"`
	StartFiscalPeriod string  `json:"start_fiscal_period"`
	PeriodCount       int     `json:"period_count"`
	DebitAccountCode  string  `json:"debit_account_code"`
	CreditAccountCode string  `json:"credit_account_code"`
	Status            string  `json:"status"`

	CreatedAt                 time.Time  `json:"created_at"`
	CreatedByPrincipalID      string     `json:"created_by_principal_id"`
	ApprovedAt                *time.Time `json:"approved_at,omitempty"`
	ApprovedByPrincipalID     *string    `json:"approved_by_principal_id,omitempty"`
	TerminatedAt              *time.Time `json:"terminated_at,omitempty"`
	TerminatedByPrincipalID   *string    `json:"terminated_by_principal_id,omitempty"`
	TerminationReason         *string    `json:"termination_reason,omitempty"`
	TerminationFinalTreatment *string    `json:"termination_final_treatment,omitempty"`
}

type CreatePrepaymentRequest struct {
	LegalEntityID     string  `json:"legal_entity_id"`
	Description       string  `json:"description"`
	TotalAmount       float64 `json:"total_amount"`
	StartFiscalPeriod string  `json:"start_fiscal_period"`
	PeriodCount       int     `json:"period_count"`
	DebitAccountCode  string  `json:"debit_account_code"`
	CreditAccountCode string  `json:"credit_account_code"`
}

type ModifyFutureScheduleRequest struct {
	TotalAmount float64 `json:"total_amount"`
	PeriodCount int     `json:"period_count"`
}

type RunPrepaymentRecognitionRequest struct {
	FiscalPeriod string `json:"fiscal_period"`
}

// TerminatePrepaymentRequest implements the spec's own negative-path
// requirement that a terminate WITHOUT a stated final-balance treatment be
// blocked — FinalBalanceTreatment has no default and is validated against
// the two named values, never silently assumed.
type TerminatePrepaymentRequest struct {
	Reason                string `json:"reason"`
	FinalBalanceTreatment string `json:"final_balance_treatment"` // WRITE_OFF | RECOGNIZE_REMAINING
	// FiscalPeriod is required only when FinalBalanceTreatment is
	// RECOGNIZE_REMAINING — the real accounting period the final
	// settlement journal posts into.
	FiscalPeriod string `json:"fiscal_period,omitempty"`
}

// PrepaymentRecognitionInstance mirrors ACC-07's RecognitionInstance —
// permanent evidence of one posted recognition. FiscalPeriod is either a
// real 'YYYY-MM' period or the literal "TERMINATION" for a final
// settlement entry (see migration 000006's doc comment) — a distinct key
// so a termination's final posting never collides with an ordinary
// periodic recognition.
type PrepaymentRecognitionInstance struct {
	RecognitionInstanceID   string    `json:"recognition_instance_id"`
	TenantID                string    `json:"tenant_id"`
	ScheduleID              string    `json:"schedule_id"`
	FiscalPeriod            string    `json:"fiscal_period"`
	RecognizedAmount        float64   `json:"recognized_amount"`
	JournalID               string    `json:"journal_id"`
	RecognizedAt            time.Time `json:"recognized_at"`
	RecognizedByPrincipalID string    `json:"recognized_by_principal_id"`
}

// terminationPseudoPeriod is the fixed fiscal_period value a termination's
// final settlement recognition is recorded under — never a real period, so
// it can never collide with an ordinary periodic recognition, and its
// UNIQUE(schedule_id, fiscal_period) constraint makes a replayed terminate
// call idempotent for free.
const TerminationPseudoPeriod = "TERMINATION"

// ACC-09 (Allocation Engine) — two independently-lifecycled state
// machines, verbatim from spec: "Rule: Draft→Approved→Active→Superseded;
// Run: Planned→Calculated→Posted/Failed."
const (
	AllocationRuleStatusDraft      = "DRAFT"
	AllocationRuleStatusApproved   = "APPROVED"
	AllocationRuleStatusActive     = "ACTIVE"
	AllocationRuleStatusSuperseded = "SUPERSEDED"

	AllocationRunStatusPlanned    = "PLANNED"
	AllocationRunStatusCalculated = "CALCULATED"
	AllocationRunStatusPosted     = "POSTED"
	AllocationRunStatusFailed     = "FAILED"
)

// AllocationDriver is one recipient's share of an AllocationRule version.
// WeightPercentage across all of a version's drivers must sum to exactly
// 100 — validated at approval, the spec's own negative path ("Drivers do
// not sum/cover source" must be blocked).
type AllocationDriver struct {
	RecipientAccountCode string  `json:"recipient_account_code"`
	WeightPercentage     float64 `json:"weight_percentage"`
}

// AllocationRule is ACC-09's own authority over "AllocationRuleVersion" —
// a stable logical rule_id carrying effective-dated versions (see
// migration 000007's doc comment), never mutated in place once approved.
// Changing drivers means creating a new version, which supersedes this
// one — the spec's own negative path ("Driver changes after run starts")
// is satisfied by there being no in-place edit path to begin with.
type AllocationRule struct {
	RuleVersionID         string             `json:"rule_version_id"`
	RuleID                string             `json:"rule_id"`
	Version               int                `json:"version"`
	TenantID              string             `json:"tenant_id"`
	LegalEntityID         string             `json:"legal_entity_id"`
	Name                  string             `json:"name"`
	SourceAccountCode     string             `json:"source_account_code"`
	Drivers               []AllocationDriver `json:"drivers"`
	Status                string             `json:"status"`
	CreatedAt             time.Time          `json:"created_at"`
	CreatedByPrincipalID  string             `json:"created_by_principal_id"`
	ApprovedAt            *time.Time         `json:"approved_at,omitempty"`
	ApprovedByPrincipalID *string            `json:"approved_by_principal_id,omitempty"`
	EffectiveTo           *time.Time         `json:"effective_to,omitempty"`
}

type CreateAllocationRuleRequest struct {
	LegalEntityID     string             `json:"legal_entity_id"`
	Name              string             `json:"name"`
	SourceAccountCode string             `json:"source_account_code"`
	Drivers           []AllocationDriver `json:"drivers"`
}

// AllocationResultLine is one recipient's computed share within a run —
// permanent calculation evidence (migration 000007), never mutated.
type AllocationResultLine struct {
	ResultLineID         string  `json:"result_line_id"`
	RunID                string  `json:"run_id"`
	RecipientAccountCode string  `json:"recipient_account_code"`
	AllocatedAmount      float64 `json:"allocated_amount"`
}

// AllocationRun is ACC-09's own authority over "AllocationRun, driver
// snapshot, result lines and calculation evidence" — explicitly never
// "Source population or ledger truth": SourceAmount is always READ from
// general-ledger-svc's own trial balance (ACC-15), never supplied by the
// caller, so this run can never assert a source truth GL itself doesn't
// hold.
type AllocationRun struct {
	RunID                string                 `json:"run_id"`
	TenantID             string                 `json:"tenant_id"`
	LegalEntityID        string                 `json:"legal_entity_id"`
	RuleID               string                 `json:"rule_id"`
	RuleVersionID        string                 `json:"rule_version_id"`
	FiscalPeriod         string                 `json:"fiscal_period"`
	SourceAccountCode    string                 `json:"source_account_code"`
	SourceAmount         float64                `json:"source_amount"`
	Status               string                 `json:"status"`
	JournalID            *string                `json:"journal_id,omitempty"`
	FailureReason        *string                `json:"failure_reason,omitempty"`
	CreatedAt            time.Time              `json:"created_at"`
	CreatedByPrincipalID string                 `json:"created_by_principal_id"`
	CalculatedAt         *time.Time             `json:"calculated_at,omitempty"`
	PostedAt             *time.Time             `json:"posted_at,omitempty"`
	ResultLines          []AllocationResultLine `json:"result_lines,omitempty"`
}

type ExecuteAllocationRequest struct {
	RuleID       string `json:"rule_id"`
	FiscalPeriod string `json:"fiscal_period"`
}

// AllocationJournalLine is one debit line of a multi-recipient allocation
// posting — shared between internal/clients (which builds the GL request)
// and internal/handler (which computes the amounts), so neither package
// needs to import the other's internals.
type AllocationJournalLine struct {
	AccountCode string
	Amount      float64
}

// JournalLineInput is a generic, mixed debit-or-credit journal line —
// ACC-10's FX revaluation posting needs both signs in one journal (some
// items move by a debit, others by a credit), unlike ACC-09's fixed
// all-debits-plus-one-credit shape, so it gets its own generic primitive
// rather than forcing that shape to fit.
type JournalLineInput struct {
	AccountCode  string
	DebitAmount  float64
	CreditAmount float64
}

// ACC-10 (Foreign Currency Revaluation) — verbatim from spec: "Planned →
// Calculated → Review → Approved → Posted; corrected/reversed via new
// run." PLANNED and CALCULATED collapse into REVIEW in this v1:
// StartRevaluation computes every item synchronously in one call (the
// wireframe has no separate "calculate" command), so there is no durable
// intermediate state between "just started" and "ready for review" worth
// exposing as its own API-visible status.
const (
	FXRevaluationStatusReview   = "REVIEW"
	FXRevaluationStatusApproved = "APPROVED"
	FXRevaluationStatusPosted   = "POSTED"
)

// AccountTypeAsset/AccountTypeLiability mirror general-ledger-svc's own
// ACC-01 domain.AccountType constants — duplicated rather than imported,
// same posture this package already takes with every other GL wire
// concept, since these two services share no Go module.
const (
	AccountTypeAsset     = "ASSET"
	AccountTypeLiability = "LIABILITY"
)

// FXRevaluationRun is ACC-10's own authority — "RevaluationRun, item
// calculations, rate references, resulting posting refs," explicitly
// never "FX reference master": ReversalOfRunID implements "corrected/
// reversed via new run" literally — there is no in-place reversal of a
// POSTED run, only a fresh run that negates a prior one's adjustments.
type FXRevaluationRun struct {
	RunID                 string              `json:"run_id"`
	TenantID              string              `json:"tenant_id"`
	LegalEntityID         string              `json:"legal_entity_id"`
	FiscalPeriod          string              `json:"fiscal_period"`
	FXGainLossAccountCode string              `json:"fx_gain_loss_account_code"`
	Status                string              `json:"status"`
	ReversalOfRunID       *string             `json:"reversal_of_run_id,omitempty"`
	JournalID             *string             `json:"journal_id,omitempty"`
	CreatedAt             time.Time           `json:"created_at"`
	CreatedByPrincipalID  string              `json:"created_by_principal_id"`
	ApprovedAt            *time.Time          `json:"approved_at,omitempty"`
	ApprovedByPrincipalID *string             `json:"approved_by_principal_id,omitempty"`
	PostedAt              *time.Time          `json:"posted_at,omitempty"`
	PostedByPrincipalID   *string             `json:"posted_by_principal_id,omitempty"`
	Items                 []FXRevaluationItem `json:"items,omitempty"`
}

// FXRevaluationItem is one monetary balance's revaluation — permanent
// calculation evidence (migration 000008), never mutated. BookAmount is
// always read from general-ledger-svc's own trial balance, never
// caller-declared; ForeignAmount and ClosingRate are caller-declared
// (this platform's ledger carries no currency-denominated face value of
// its own — see the ACC-10 findings section for why that is a named,
// deliberate platform gap rather than a fabricated number).
type FXRevaluationItem struct {
	ItemID           string  `json:"item_id"`
	RunID            string  `json:"run_id"`
	AccountCode      string  `json:"account_code"`
	AccountType      string  `json:"account_type"`
	CurrencyCode     string  `json:"currency_code"`
	ForeignAmount    float64 `json:"foreign_amount"`
	BookAmount       float64 `json:"book_amount"`
	ClosingRate      float64 `json:"closing_rate"`
	RevaluedAmount   float64 `json:"revalued_amount"`
	AdjustmentAmount float64 `json:"adjustment_amount"`
}

// RevaluationItemInput is the caller-declared scope of one item to
// revalue.
type RevaluationItemInput struct {
	AccountCode   string  `json:"account_code"`
	CurrencyCode  string  `json:"currency_code"`
	ForeignAmount float64 `json:"foreign_amount"`
}

// StartRevaluationRequest's RateSet maps currency_code -> closing_rate,
// this run's own caller-declared rate reference (never a shared master —
// see migration 000008's doc comment). Every currency named by an Items
// entry must have a matching RateSet entry, or the run is refused (the
// spec's own negative path, "Rate set missing one currency").
type StartRevaluationRequest struct {
	LegalEntityID         string                 `json:"legal_entity_id"`
	FiscalPeriod          string                 `json:"fiscal_period"`
	FXGainLossAccountCode string                 `json:"fx_gain_loss_account_code"`
	RateSet               map[string]float64     `json:"rate_set"`
	Items                 []RevaluationItemInput `json:"items"`
}

type ReversePriorRevaluationRequest struct {
	PriorRunID string `json:"prior_run_id"`
}

// ACC-17 (Opening Balance & Migration) — verbatim from spec: "Planned →
// Loaded → Validated → Approved → Posted → Reconciled → Certified;
// failed/quarantined as needed." Planned collapses into LOADED for the
// same reason ACC-10's Planned/Calculated did — CreateMigrationAccountingBatch
// loads every crosswalk entry synchronously, so there is no durable
// intermediate "just planned" state to expose.
const (
	MigrationBatchStatusLoaded      = "LOADED"
	MigrationBatchStatusValidated   = "VALIDATED"
	MigrationBatchStatusApproved    = "APPROVED"
	MigrationBatchStatusPosted      = "POSTED"
	MigrationBatchStatusReconciled  = "RECONCILED"
	MigrationBatchStatusCertified   = "CERTIFIED"
	MigrationBatchStatusQuarantined = "QUARANTINED"
)

// MigrationBatch is ACC-17's own authority — "MigrationAccountingBatch,
// opening journal proposals, source crosswalk and reconciliation/
// certification refs," explicitly never "Bypass of ACC-04/05": opening
// balances post through GL's real journal lifecycle and this service's
// real period status, exactly like every other capability.
type MigrationBatch struct {
	BatchID                string                    `json:"batch_id"`
	TenantID               string                    `json:"tenant_id"`
	LegalEntityID          string                    `json:"legal_entity_id"`
	FiscalPeriod           string                    `json:"fiscal_period"`
	SourceSystemName       string                    `json:"source_system_name"`
	SourceExtractHash      string                    `json:"source_extract_hash"`
	ExpectedRowCount       int                       `json:"expected_row_count"`
	ExpectedTotalDebits    float64                   `json:"expected_total_debits"`
	ExpectedTotalCredits   float64                   `json:"expected_total_credits"`
	Status                 string                    `json:"status"`
	QuarantineReason       *string                   `json:"quarantine_reason,omitempty"`
	JournalID              *string                   `json:"journal_id,omitempty"`
	CreatedAt              time.Time                 `json:"created_at"`
	CreatedByPrincipalID   string                    `json:"created_by_principal_id"`
	ValidatedAt            *time.Time                `json:"validated_at,omitempty"`
	ApprovedAt             *time.Time                `json:"approved_at,omitempty"`
	ApprovedByPrincipalID  *string                   `json:"approved_by_principal_id,omitempty"`
	PostedAt               *time.Time                `json:"posted_at,omitempty"`
	ReconciledAt           *time.Time                `json:"reconciled_at,omitempty"`
	CertifiedAt            *time.Time                `json:"certified_at,omitempty"`
	CertifiedByPrincipalID *string                   `json:"certified_by_principal_id,omitempty"`
	CertificationReason    *string                   `json:"certification_reason,omitempty"`
	Entries                []MigrationCrosswalkEntry `json:"entries,omitempty"`
}

// MigrationCrosswalkEntry is one source-to-target line — permanent
// evidence (migration 000009), never mutated.
type MigrationCrosswalkEntry struct {
	EntryID           string  `json:"entry_id"`
	BatchID           string  `json:"batch_id"`
	SourceReferenceID string  `json:"source_reference_id"`
	SourceAccountCode string  `json:"source_account_code"`
	TargetAccountCode string  `json:"target_account_code"`
	DebitAmount       float64 `json:"debit_amount"`
	CreditAmount      float64 `json:"credit_amount"`
}

type CreateMigrationBatchRequest struct {
	LegalEntityID     string `json:"legal_entity_id"`
	FiscalPeriod      string `json:"fiscal_period"`
	SourceSystemName  string `json:"source_system_name"`
	SourceExtractHash string `json:"source_extract_hash"`
	// ExpectedRowCount/ExpectedTotalDebits/ExpectedTotalCredits are the
	// SOURCE system's own declared control totals — independent of
	// whatever Entries actually arrives with, so ValidateOpeningBalances
	// can catch "source-target row counts match but values differ" (the
	// spec's own negative path) rather than only ever checking the batch
	// against itself.
	ExpectedRowCount     int                       `json:"expected_row_count"`
	ExpectedTotalDebits  float64                   `json:"expected_total_debits"`
	ExpectedTotalCredits float64                   `json:"expected_total_credits"`
	Entries              []MigrationCrosswalkEntry `json:"entries"`
}

type CertifyMigrationBatchRequest struct {
	Reason string `json:"reason"`
}

// ACC-16 (Signed Financial Snapshot) — verbatim from spec: "Draft →
// Sealed → Certified → Superseded; sealed content never edited in
// place."
const (
	SnapshotStatusDraft      = "DRAFT"
	SnapshotStatusSealed     = "SEALED"
	SnapshotStatusCertified  = "CERTIFIED"
	SnapshotStatusSuperseded = "SUPERSEDED"
)

// FinancialSnapshot is ACC-16's own authority — "FinancialSnapshot,
// manifest, content hash/signature, purpose, source references and
// supersession chain," explicitly never "Mutable live balances": Content
// is fixed at creation and never mutated afterward — there is no update
// endpoint at any stage, sealed or not (see migration 000010's doc
// comment).
type FinancialSnapshot struct {
	SnapshotID              string     `json:"snapshot_id"`
	TenantID                string     `json:"tenant_id"`
	LegalEntityID           string     `json:"legal_entity_id"`
	Purpose                 string     `json:"purpose"`
	Content                 string     `json:"content"`
	SourceReferences        string     `json:"source_references"`
	ContentHash             *string    `json:"content_hash,omitempty"`
	Signature               *string    `json:"signature,omitempty"`
	HasUnresolvedExceptions bool       `json:"has_unresolved_exceptions"`
	Status                  string     `json:"status"`
	SupersededBySnapshotID  *string    `json:"superseded_by_snapshot_id,omitempty"`
	CreatedAt               time.Time  `json:"created_at"`
	CreatedByPrincipalID    string     `json:"created_by_principal_id"`
	SealedAt                *time.Time `json:"sealed_at,omitempty"`
	CertifiedAt             *time.Time `json:"certified_at,omitempty"`
	CertifiedByPrincipalID  *string    `json:"certified_by_principal_id,omitempty"`
	CertificationReason     *string    `json:"certification_reason,omitempty"`
	SupersededAt            *time.Time `json:"superseded_at,omitempty"`
}

type CreateFinancialSnapshotRequest struct {
	LegalEntityID           string `json:"legal_entity_id"`
	Purpose                 string `json:"purpose"`
	Content                 string `json:"content"`
	SourceReferences        string `json:"source_references"`
	HasUnresolvedExceptions bool   `json:"has_unresolved_exceptions"`
}

type CertifySnapshotRequest struct {
	Reason string `json:"reason"`
}

var (
	ErrFinancialSnapshotNotFound = errorString("financial snapshot not found")
	ErrInvalidSnapshotTransition = errorString("financial snapshot is not in a status that allows this action")
	ErrSigningKeyUnavailable     = errorString("no valid signing key is available to seal this snapshot")
	// ErrCertifyWithUnresolvedException is returned when CertifySnapshot
	// is called on a snapshot whose has_unresolved_exceptions flag is
	// still true — the spec's own negative path, "Certify snapshot with
	// unresolved prohibited exception."
	ErrCertifyWithUnresolvedException = errorString("cannot certify a snapshot with unresolved exceptions")
)

// ACC-18 (Source-to-Report Traceability) — verbatim from spec:
// "Projection: Current / Rebuilding / Degraded; historical lineage
// immutable even if access policy changes."
const (
	LineageProjectionCurrent    = "CURRENT"
	LineageProjectionRebuilding = "REBUILDING"
	LineageProjectionDegraded   = "DEGRADED"
)

// LineageEdge is ACC-18's own authority — "AccountingLineageGraph/index
// and verification results," explicitly "does not own underlying
// business facts": an edge only ever RECORDS that a link exists (e.g.
// "this accrual recognition produced this GL journal"); it never
// re-derives, owns or duplicates the source records themselves. Never
// inferred — see migration 000011's doc comment.
type LineageEdge struct {
	EdgeID        string    `json:"edge_id"`
	TenantID      string    `json:"tenant_id"`
	LegalEntityID string    `json:"legal_entity_id"`
	FromType      string    `json:"from_type"`
	FromID        string    `json:"from_id"`
	ToType        string    `json:"to_type"`
	ToID          string    `json:"to_id"`
	RecordedAt    time.Time `json:"recorded_at"`
}

// LineageProjectionStatus is ACC-18's own tracked projection state per
// legal entity.
type LineageProjectionStatus struct {
	TenantID       string     `json:"tenant_id"`
	LegalEntityID  string     `json:"legal_entity_id"`
	Status         string     `json:"status"`
	DegradedReason *string    `json:"degraded_reason,omitempty"`
	LastRebuiltAt  *time.Time `json:"last_rebuilt_at,omitempty"`
}

// PostedJournalRef is one (from_type, from_id, journal_id) fact drawn
// from an already-built ACC capability's own posted records — the
// authoritative source VerifyLineageCompleteness and
// RebuildLineageProjection check recorded edges against, never a guess.
type PostedJournalRef struct {
	FromType  string `json:"from_type"`
	FromID    string `json:"from_id"`
	JournalID string `json:"journal_id"`
}

// LineageCompletenessReport is ACC-18's own VerifyLineageCompleteness
// result — Gaps lists every posted journal with NO recorded lineage
// edge, the spec's own negative path, "Missing journal-source link,"
// surfaced explicitly rather than silently ignored.
type LineageCompletenessReport struct {
	LegalEntityID string             `json:"legal_entity_id"`
	CheckedCount  int                `json:"checked_count"`
	Gaps          []PostedJournalRef `json:"gaps"`
	Complete      bool               `json:"complete"`
}

type CloseEvidence struct {
	EvidenceID       string    `json:"evidence_id"`
	TenantID         string    `json:"tenant_id"`
	FiscalPeriodID   string    `json:"fiscal_period_id"`
	TrialBalanceHash string    `json:"trial_balance_hash"`
	Signature        string    `json:"signature"`
	GeneratedAt      time.Time `json:"generated_at"`
}

type PeriodCreateRequest struct {
	LegalEntityID string    `json:"legal_entity_id"`
	PeriodName    string    `json:"period_name"`
	PeriodStart   time.Time `json:"period_start"`
	PeriodEnd     time.Time `json:"period_end"`
}

type PeriodLockResponse struct {
	FiscalPeriodID     string    `json:"fiscal_period_id"`
	PeriodName         string    `json:"period_name"`
	CloseStatus        string    `json:"close_status"`
	CloseLockedAt      time.Time `json:"close_locked_at"`
	EvidenceDocumentID string    `json:"evidence_document_id"`
	VerificationHash   string    `json:"verification_hash"`
}

type ReadinessCheckResponse struct {
	IsReady        bool     `json:"is_ready"`
	BlockingIssues []string `json:"blocking_issues"`
}

type errorString string

func (e errorString) Error() string { return string(e) }

var (
	ErrFiscalPeriodNotFound    = errorString("fiscal period not found")
	ErrPeriodAlreadyLocked     = errorString("fiscal period is already locked")
	ErrStoreUnavailable        = errorString("financial close store unavailable")
	ErrAuthorizationDenied     = errorString("authorization denied for financial close action")
	ErrAuthzServiceUnavailable = errorString("authorization-svc unavailable")
	ErrIdentityMissing         = errorString("caller identity missing")
	ErrReadinessChecksFailed   = errorString("period close blocked: unresolved balance sheet or ledger discrepancies")
	ErrGLServiceUnavailable    = errorString("general-ledger-svc unavailable")
	ErrAPServiceUnavailable    = errorString("accounts-payable-svc unavailable")
	ErrARServiceUnavailable    = errorString("accounts-receivable-svc unavailable")
	ErrVaultServiceUnavailable = errorString("document-vault-svc unavailable")

	// ErrLedgerPageTruncated is returned when the ledger answered with a full
	// page, so there may be journals this service never saw. A trial balance
	// compiled from a partial ledger is wrong, not merely incomplete, and this
	// one is about to be hashed and locked as the period's evidence — so the
	// close is refused rather than signed over an unknown.
	ErrLedgerPageTruncated = errorString("ledger returned a full page: the trial balance may be incomplete, so the close was refused")

	// ErrTenantScopeMissing is returned when a request carries no verified
	// tenant scope. Distinguished from a store outage so it answers 401 rather
	// than 503 — it is the caller's request that is wrong, not the database.
	ErrTenantScopeMissing = errorString("caller tenant scope missing")

	// ErrEvidenceNotRecorded is returned when the period was locked but its
	// close evidence could not be written. See the handler for why this is
	// surfaced rather than logged and swallowed.
	ErrEvidenceNotRecorded = errorString("period locked but close evidence could not be recorded")

	// ErrPeriodNotLocked is returned by ReopenFiscalPeriod when the period
	// is not currently LOCKED — reopen only ever makes sense as a LOCKED ->
	// OPEN transition, same doctrine as LockFiscalPeriod only accepting
	// OPEN -> LOCKED.
	ErrPeriodNotLocked = errorString("fiscal period is not currently locked")

	// ErrReopenReasonRequired is returned when a reopen request carries no
	// reason — ACC-14 invariant #6 requires reopen be "evidenced," and an
	// empty reason is not evidence.
	ErrReopenReasonRequired = errorString("reason is required to reopen a locked period")

	// ErrReopenEventNotRecorded is returned when the period was reopened but
	// its append-only reopen event could not be written — surfaced rather
	// than logged and swallowed, same posture as ErrEvidenceNotRecorded.
	ErrReopenEventNotRecorded = errorString("period reopened but the reopen event could not be recorded")

	ErrInvalidSubledger = errorString("subledger must be AP or AR")
	// ErrControlAccountMappingNotFound is returned when
	// control_account_mapping_key names no current mapping — ACC-06 must
	// never guess which GL account a subledger reconciles against.
	ErrControlAccountMappingNotFound = errorString("no current account mapping found for control_account_mapping_key")

	// ErrSubledgerPageTruncated is returned when AP or AR answered with a
	// full page of invoices, so the subledger total may be understated. Same
	// doctrine as ErrLedgerPageTruncated: a control run is permanent
	// evidence the moment it is recorded, so a total that might be wrong is
	// refused rather than persisted as if it were authoritative.
	ErrSubledgerPageTruncated = errorString("subledger returned a full page: the control total may be incomplete, so the run was refused")

	// ErrControlAccountBalanceNotFound is returned when the trial balance
	// general-ledger-svc compiled has no line for the resolved control
	// account. No balance is not zero — it means the account never posted
	// this period, and ACC-06 must not silently treat that as a match.
	ErrControlAccountBalanceNotFound = errorString("no trial balance line found for the resolved control account")

	ErrAccrualNotFound = errorString("accrual schedule not found")

	// ErrInvalidAccrualTransition is returned by every ACC-07 lifecycle
	// action for a schedule not currently in the one status that action
	// requires — Submit only from DRAFT, Approve only from
	// PENDING_APPROVAL, recognition only from APPROVED/ACTIVE, matching
	// the spec's own forward-only state model.
	ErrInvalidAccrualTransition = errorString("accrual schedule is not in a status that allows this action")

	ErrInvalidAccrualAmount = errorString("total_amount must be positive and period_count must be at least 1")

	// ErrRecognitionPeriodOutOfRange is returned when the requested
	// fiscal_period is not one of the schedule's own period_count periods
	// counting from start_fiscal_period — ACC-07 never posts a recognition
	// for a period the schedule was never defined over.
	ErrRecognitionPeriodOutOfRange = errorString("fiscal_period is not within this accrual schedule's period range")

	// ErrRecognitionPeriodLocked is returned when the target fiscal period
	// is LOCKED — the spec's own negative-path table: "Accrual posted into
	// hard-closed period" must be blocked, not silently posted into a
	// sealed close.
	ErrRecognitionPeriodLocked = errorString("cannot post an accrual recognition into a LOCKED fiscal period")

	// ErrAmendWouldDropRecognizedPeriods is returned when an amendment
	// would shrink period_count below the number of periods already
	// recognized — ACC-07 amends the FUTURE of a schedule only; recognized
	// history is permanent evidence and is never invalidated by a later
	// amendment.
	ErrAmendWouldDropRecognizedPeriods = errorString("period_count cannot be reduced below the number of periods already recognized")

	ErrJournalPostingFailed = errorString("general-ledger-svc rejected the accrual recognition journal")

	ErrPrepaymentNotFound          = errorString("prepayment schedule not found")
	ErrInvalidPrepaymentTransition = errorString("prepayment schedule is not in a status that allows this action")
	ErrInvalidPrepaymentAmount     = errorString("total_amount must be positive and period_count must be at least 1")
	ErrPrepaymentPeriodOutOfRange  = errorString("fiscal_period is not within this prepayment schedule's period range")
	ErrPrepaymentPeriodLocked      = errorString("cannot post a prepayment recognition into a LOCKED fiscal period")

	// ErrModifyWouldDropRecognizedPeriods mirrors ACC-07's
	// ErrAmendWouldDropRecognizedPeriods — the spec's own negative path
	// ("Backdate schedule change over recognized periods" must be
	// blocked): a schedule modification never invalidates already-posted
	// recognition history.
	ErrModifyWouldDropRecognizedPeriods = errorString("period_count cannot be reduced below the number of periods already recognized")

	// ErrFinalBalanceTreatmentRequired is returned when a terminate request
	// names no valid final_balance_treatment — the spec's own negative path
	// ("Terminate without final balance treatment" must be blocked), made a
	// real validation rather than a documented expectation.
	ErrFinalBalanceTreatmentRequired = errorString("final_balance_treatment must be WRITE_OFF or RECOGNIZE_REMAINING")

	ErrTerminationFiscalPeriodRequired = errorString("fiscal_period is required when final_balance_treatment is RECOGNIZE_REMAINING")

	ErrAllocationRuleNotFound = errorString("allocation rule not found")
	ErrAllocationRunNotFound  = errorString("allocation run not found")

	// ErrDriversDoNotSumTo100 is returned when an allocation rule's
	// drivers don't sum to exactly 100% — the spec's own negative path,
	// "Drivers do not sum/cover source," made a real, blocking validation
	// at approval rather than a documented expectation.
	ErrDriversDoNotSumTo100 = errorString("allocation drivers must sum to exactly 100 percent")

	ErrNoDriversDefined = errorString("an allocation rule must have at least one driver")

	// ErrRecipientAccountInvalid is returned when a driver's
	// recipient_account_code does not resolve to a real, ACTIVE
	// chart-registered account — the spec's own negative path, "Recipient
	// dimension invalid."
	ErrRecipientAccountInvalid = errorString("driver recipient_account_code is not a real, active account")

	ErrInvalidAllocationRuleTransition = errorString("allocation rule is not in a status that allows this action")
	ErrInvalidAllocationRunTransition  = errorString("allocation run is not in a status that allows this action")

	// ErrSourceBalanceNotFound is returned when general-ledger-svc's own
	// trial balance has no line for the rule's source_account_code — ACC-09
	// never invents a source amount; no balance is not zero.
	ErrSourceBalanceNotFound = errorString("no trial balance line found for the allocation rule's source account")

	ErrFXRevaluationRunNotFound = errorString("FX revaluation run not found")
	ErrNoRevaluationItems       = errorString("a revaluation run must include at least one item")

	// ErrRateMissingForCurrency is returned when an item's currency has no
	// matching entry in the run's rate_set — the spec's own negative path,
	// "Rate set missing one currency."
	ErrRateMissingForCurrency = errorString("rate_set has no closing rate for one or more item currencies")

	// ErrNonMonetaryItemIncluded is returned when an item's account is not
	// ASSET or LIABILITY — the spec's own negative path, "Non-monetary
	// item included." Revenue/expense/equity accounts carry historical
	// rates by accounting convention and are never revalued.
	ErrNonMonetaryItemIncluded = errorString("item account is not a monetary (ASSET or LIABILITY) account")

	ErrInvalidFXRevaluationTransition = errorString("FX revaluation run is not in a status that allows this action")

	// ErrRevaluationBookBalanceNotFound mirrors ACC-09's
	// ErrSourceBalanceNotFound — an item's account never posted this
	// period, so there is no real book amount to revalue.
	ErrRevaluationBookBalanceNotFound = errorString("no trial balance line found for a revaluation item's account")

	// ErrPriorRevaluationNotPosted is returned when
	// ReversePriorRevaluation names a run that never reached POSTED —
	// only a real, posted revaluation has an accounting consequence worth
	// reversing.
	ErrPriorRevaluationNotPosted = errorString("prior revaluation run is not POSTED and cannot be reversed")

	ErrMigrationBatchNotFound          = errorString("migration batch not found")
	ErrNoMigrationEntries              = errorString("a migration batch must include at least one crosswalk entry")
	ErrInvalidMigrationBatchTransition = errorString("migration batch is not in a status that allows this action")

	// ErrOpeningTBDoesNotBalance is returned when a batch's crosswalk
	// entries don't sum debits-equal-credits exactly — the spec's own
	// negative path, "Opening TB forced with suspense plug": refusing to
	// post an unbalanced opening position is what prevents a plug, rather
	// than silently absorbing the difference into any account.
	ErrOpeningTBDoesNotBalance = errorString("opening trial balance does not balance: debits must equal credits with no suspense plug")

	// ErrSuspenseAccountNotAllowed is returned when a crosswalk entry
	// targets an account whose code names it a suspense/clearing account
	// — the same negative path as ErrOpeningTBDoesNotBalance, caught even
	// when the entries happen to balance overall.
	ErrSuspenseAccountNotAllowed = errorString("crosswalk entries may not target a suspense account")

	ErrDuplicateSourceReference = errorString("source_reference_id is duplicated within this batch")

	// ErrControlTotalsMismatch is returned when the source system's own
	// declared row count or debit/credit totals don't match what was
	// actually loaded — the spec's own negative path, "source-target row
	// counts match but values differ" (and its inverse: counts differ
	// too).
	ErrControlTotalsMismatch = errorString("loaded entries do not match the source system's declared control totals")

	ErrMigrationTargetAccountInvalid = errorString("crosswalk entry target_account_code is not a real, active account")

	ErrMigrationPeriodLocked = errorString("cannot post opening balances into a LOCKED fiscal period")

	ErrReconciliationMismatch = errorString("posted journal balances do not match the batch's own crosswalk totals")
)
