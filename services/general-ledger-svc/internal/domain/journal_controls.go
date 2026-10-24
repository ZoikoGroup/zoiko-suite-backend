package domain

// Controlled vocabularies of the accounting kernel's journal model (standard §9, §11). They are closed on purpose: a
// purpose or a correction kind that anyone can invent cannot be reported on or reviewed.

var journalPurposes = map[string]bool{
	"ACCRUAL": true, "PREPAYMENT": true, "RECLASSIFICATION": true, "CORRECTION": true, "ESTIMATE_CHANGE": true, "POLICY_CHANGE": true,
	"OPENING_BALANCE": true, "MIGRATION": true, "CLOSE_ADJUSTMENT": true, "ALLOCATION": true, "REVALUATION": true, "RECONCILIATION_ADJUSTMENT": true, "OTHER": true,
}

// ValidJournalPurpose reports whether p is a controlled manual-journal purpose.
func ValidJournalPurpose(p string) bool { return journalPurposes[p] }

// JournalPurposes lists the accepted purposes, for error messages and the console.
func JournalPurposes() []string {
	return []string{"ACCRUAL", "PREPAYMENT", "RECLASSIFICATION", "CORRECTION", "ESTIMATE_CHANGE", "POLICY_CHANGE", "OPENING_BALANCE", "MIGRATION", "CLOSE_ADJUSTMENT", "ALLOCATION", "REVALUATION", "RECONCILIATION_ADJUSTMENT", "OTHER"}
}

// PurposeRequired reports whether a journal of this type must carry a controlled purpose: the adjusting types, where free
// text alone is not enough (§9). A STANDARD journal or a reversal is already explained by its source or its original.
func PurposeRequired(t JournalType) bool {
	switch t {
	case JournalTypeAdjustment, JournalTypeAccrual, JournalTypeReclass, JournalTypeOpening, JournalTypeClosing:
		return true
	}
	return false
}

var correctionKinds = map[string]bool{
	"ERROR_CORRECTION": true, "ESTIMATE_CHANGE": true, "POLICY_CHANGE": true, "SOURCE_DOCUMENT_CORRECTION": true,
	"COMMERCIAL_ADJUSTMENT": true, "PERIOD_REOPEN_CORRECTION": true, "MIGRATION_CORRECTION": true,
}

// ValidCorrectionKind reports whether k names a kind of correction. The kernel keeps error corrections, estimate changes
// and policy changes apart because they carry different accounting and disclosure consequences (§11, IAS 8).
func ValidCorrectionKind(k string) bool { return correctionKinds[k] }

// RecurringLine is one line of a recurring journal template.
type RecurringLine struct {
	AccountCode  string     `json:"account_code"`
	DebitAmount  float64    `json:"debit_amount,omitempty"`
	CreditAmount float64    `json:"credit_amount,omitempty"`
	Description  string     `json:"description,omitempty"`
	Dimensions   Dimensions `json:"dimensions,omitempty"`
}

// RecurringTemplate is a recurring journal template (standard §9). It is never itself posted: generating it creates a new
// draft journal with its own identity, which then follows the ordinary approval and posting path.
type RecurringTemplate struct {
	TemplateID      string          `json:"template_id"`
	TenantID        string          `json:"tenant_id"`
	LegalEntityID   string          `json:"legal_entity_id"`
	Name            string          `json:"name"`
	JournalType     JournalType     `json:"journal_type"`
	JournalPurpose  string          `json:"journal_purpose"`
	CurrencyCode    string          `json:"currency_code"`
	BookID          *string         `json:"book_id,omitempty"`
	Description     string          `json:"description"`
	Lines           []RecurringLine `json:"lines"`
	Frequency       string          `json:"frequency"`
	NextRunDate     Date            `json:"next_run_date"`
	EndDate         *Date           `json:"end_date,omitempty"`
	Status          string          `json:"status"`
	CreatedBy       string          `json:"created_by"`
	CreatedAt       string          `json:"created_at"`
	LastGeneratedAt *string         `json:"last_generated_at,omitempty"`
}

// ErrRecurring* are the refusals of the recurring-journal commands.
var (
	ErrTemplateNotFound     = errorString("recurring journal template not found")
	ErrTemplateNotActive    = errorString("the recurring journal template is not active")
	ErrTemplateOutOfOrder   = errorString("the recurring journal template has runs before that date still to generate")
	ErrTemplateExists       = errorString("a recurring journal template with that name already exists for the entity")
	ErrNotWithdrawable      = errorString("only a draft journal can be withdrawn")
)

// ErrReconciliationCaseNotOpen is returned when a reconciliation adjustment names a case that is settled or accepted.
var ErrReconciliationCaseNotOpen = errorString("the reconciliation case is not open: only a case in warning or break can be settled by an adjustment")

// AccountControl is what the chart says about an account that a manual journal must respect beyond the restriction check:
// the area that owns it and whether its lines need a counterparty.
type AccountControl struct {
	ControlType          string
	CounterpartyRequired bool
}
