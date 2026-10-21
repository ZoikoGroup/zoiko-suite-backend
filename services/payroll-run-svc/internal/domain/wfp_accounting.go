package domain

// ZS-SVC-L-001 WFP-05 — Payroll Journal Interface.
//
// WFP-05 never writes the ledger and never chooses an account. It turns an
// ACCEPTED payroll result version into a balanced posting PROPOSAL expressed in
// account-mapping KEYS (finance maps each key to an account code in
// general-ledger-svc), and submits it to the ledger's posting engine. Amounts stay
// exact decimal strings here; the ledger's API takes float64, so conversion
// happens once, at the edge, from a 4-decimal value.

import (
	"encoding/json"
	"fmt"
	"math/big"
	"strconv"
	"strings"
	"time"
)

const (
	BridgeEventCreated = "EVENT_CREATED"
	BridgeSubmitted    = "SUBMITTED"
	BridgePosted       = "POSTED"
	BridgeRejected     = "REJECTED"
	BridgeCorrected    = "CORRECTED"

	PurposeResult   = "PAYROLL_RESULT"
	PurposeReversal = "PAYROLL_REVERSAL"

	CorrectionReversed  = "REVERSED"
	CorrectionWithdrawn = "WITHDRAWN"

	ReconNotReconciled = "NOT_RECONCILED"
	ReconMatched       = "MATCHED"
	ReconMismatch      = "MISMATCH"

	// DefaultAccountingBook is the single book payroll posts to until the ledger
	// has a book field (decision 2026-10-14). It is carried for uniqueness and
	// lineage; it is NOT sent to the ledger.
	DefaultAccountingBook = "PRIMARY"

	// MaxPostingAttempts bounds transient retries of one subject; after that it
	// stays SUBMITTED with its attempts and last error visible until requeued.
	MaxPostingAttempts = 5
)

// Mapping keys. Finance configures key -> account code in general-ledger-svc;
// WFP holds no account code. A key with no mapping is a posting rejection.
const (
	KeyGrossExpense = "PAYROLL_GROSS_EXPENSE"
	KeyNetPayable   = "PAYROLL_NET_PAY_PAYABLE"
)

var (
	employeeKindOrder = []string{KindEmployeeDeduction, KindEmployeeTax, KindEmployeeSocial, KindEmployeePension, KindEmployeeBenefit}
	employerKindOrder = []string{KindEmployerTax, KindEmployerSocial, KindEmployerPension, KindEmployerBenefit, KindEmployerOther}
)

func kindSuffix(kind string) string {
	s := strings.TrimPrefix(strings.TrimPrefix(kind, "EMPLOYEE_"), "EMPLOYER_")
	return s
}

// EmployeePayableKey is the liability key for an employee-side deduction kind.
func EmployeePayableKey(kind string) string {
	return "PAYROLL_EMPLOYEE_" + kindSuffix(kind) + "_PAYABLE"
}

// EmployerExpenseKey / EmployerPayableKey are the cost and liability keys of an
// employer-side kind.
func EmployerExpenseKey(kind string) string {
	return "PAYROLL_EMPLOYER_" + kindSuffix(kind) + "_EXPENSE"
}
func EmployerPayableKey(kind string) string {
	return "PAYROLL_EMPLOYER_" + kindSuffix(kind) + "_PAYABLE"
}

// AllPayrollMappingKeys lists every key WFP-05 can emit, so finance can configure
// them all before the first posting.
func AllPayrollMappingKeys() []string {
	keys := []string{KeyGrossExpense, KeyNetPayable}
	for _, k := range employeeKindOrder {
		keys = append(keys, EmployeePayableKey(k))
	}
	for _, k := range employerKindOrder {
		keys = append(keys, EmployerExpenseKey(k), EmployerPayableKey(k))
	}
	return keys
}

// PostingLine is one line of the frozen proposal. Exactly one of Debit/Credit is
// non-zero.
type PostingLine struct {
	MappingKey  string `json:"mapping_key"`
	Debit       string `json:"debit"`
	Credit      string `json:"credit"`
	Description string `json:"description"`
}

// ResultPostingInput is what the stored result says: gross, net and the sum of
// each component kind. All exact.
type ResultPostingInput struct {
	Gross  *big.Rat
	Net    *big.Rat
	ByKind map[string]*big.Rat
}

// BuildPostingLines builds the balanced proposal for one accepted result:
//
//	Dr PAYROLL_GROSS_EXPENSE            gross
//	Dr PAYROLL_EMPLOYER_<K>_EXPENSE     employer cost of kind K
//	Cr PAYROLL_NET_PAY_PAYABLE          net pay
//	Cr PAYROLL_EMPLOYEE_<K>_PAYABLE     employee deductions of kind K
//	Cr PAYROLL_EMPLOYER_<K>_PAYABLE     employer liability of kind K
//
// Zero lines are omitted. The result must already reconcile (gross = net +
// employee deductions); if it does not, building fails instead of plugging.
func BuildPostingLines(in ResultPostingInput) ([]PostingLine, error) {
	zero := new(big.Rat)
	var lines []PostingLine
	debit, credit := new(big.Rat), new(big.Rat)
	addDebit := func(key string, amt *big.Rat, what string) {
		if amt == nil || amt.Cmp(zero) == 0 {
			return
		}
		lines = append(lines, PostingLine{MappingKey: key, Debit: FormatDecimal(amt), Credit: FormatDecimal(zero), Description: what})
		debit.Add(debit, amt)
	}
	addCredit := func(key string, amt *big.Rat, what string) {
		if amt == nil || amt.Cmp(zero) == 0 {
			return
		}
		lines = append(lines, PostingLine{MappingKey: key, Debit: FormatDecimal(zero), Credit: FormatDecimal(amt), Description: what})
		credit.Add(credit, amt)
	}

	addDebit(KeyGrossExpense, in.Gross, "Payroll gross pay")
	for _, k := range employerKindOrder {
		addDebit(EmployerExpenseKey(k), in.ByKind[k], "Employer cost "+kindSuffix(k))
	}
	addCredit(KeyNetPayable, in.Net, "Payroll net pay payable")
	for _, k := range employeeKindOrder {
		addCredit(EmployeePayableKey(k), in.ByKind[k], "Employee "+kindSuffix(k)+" payable")
	}
	for _, k := range employerKindOrder {
		addCredit(EmployerPayableKey(k), in.ByKind[k], "Employer "+kindSuffix(k)+" payable")
	}

	if len(lines) == 0 {
		return nil, ErrNothingToPost
	}
	if debit.Cmp(credit) != 0 {
		return nil, fmt.Errorf("%w: debits %s, credits %s", ErrUnbalancedPosting, FormatDecimal(debit), FormatDecimal(credit))
	}
	return lines, nil
}

// SumLines returns the exact debit and credit totals of a proposal.
func SumLines(lines []PostingLine) (debit, credit *big.Rat, err error) {
	debit, credit = new(big.Rat), new(big.Rat)
	for _, l := range lines {
		d, err := ParseDecimal(l.Debit)
		if err != nil {
			return nil, nil, err
		}
		c, err := ParseDecimal(l.Credit)
		if err != nil {
			return nil, nil, err
		}
		debit.Add(debit, d)
		credit.Add(credit, c)
	}
	return debit, credit, nil
}

// SourceEventID is the deterministic idempotency identity of one accounting
// subject. The ledger refuses a second posting for the same value, whichever
// replica or restart delivers it.
func SourceEventID(resultID, book, purpose string) string {
	return "wfp-payroll:" + resultID + ":" + book + ":" + purpose
}

// FiscalPeriodOf derives the ledger's "YYYY-MM" period from a pay date.
func FiscalPeriodOf(payDate string) string {
	if len(payDate) >= 7 {
		return payDate[:7]
	}
	return payDate
}

// ToLedgerAmount converts an exact 4-decimal string to the ledger's float64.
func ToLedgerAmount(s string) (float64, error) { return strconv.ParseFloat(s, 64) }

type AccountingBridge struct {
	BridgeID             string          `json:"bridge_id"`
	TenantID             string          `json:"tenant_id"`
	RunRefID             string          `json:"run_ref_id"`
	ResultID             string          `json:"result_id"`
	LegalEntityID        string          `json:"legal_entity_id"`
	AccountingBookID     string          `json:"accounting_book_id"`
	PostingPurpose       string          `json:"posting_purpose"`
	OriginalBridgeID     *string         `json:"original_bridge_id,omitempty"`
	SourceEventID        string          `json:"source_event_id"`
	FiscalPeriod         string          `json:"fiscal_period"`
	DocumentDate         string          `json:"document_date"`
	PostingDate          string          `json:"posting_date"`
	TransactionCurrency  string          `json:"transaction_currency"`
	Description          string          `json:"description"`
	Lines                []PostingLine   `json:"lines"`
	LineCount            int             `json:"line_count"`
	TotalDebit           string          `json:"total_debit"`
	TotalCredit          string          `json:"total_credit"`
	EvidenceFlags        []string        `json:"evidence_flags"`
	State                string          `json:"state"`
	CorrectionKind       *string         `json:"correction_kind,omitempty"`
	RejectionReason      *string         `json:"rejection_reason,omitempty"`
	ExecutionID          *string         `json:"execution_id,omitempty"`
	JournalID            *string         `json:"journal_id,omitempty"`
	Attempts             int             `json:"attempts"`
	LastError            *string         `json:"last_error,omitempty"`
	SubmittedBy          *string         `json:"submitted_by,omitempty"`
	SubmittedAt          *time.Time      `json:"submitted_at,omitempty"`
	PostedAt             *time.Time      `json:"posted_at,omitempty"`
	ReconciliationState  string          `json:"reconciliation_state"`
	ReconciledAt         *time.Time      `json:"reconciled_at,omitempty"`
	ReconciliationDetail json.RawMessage `json:"reconciliation_detail,omitempty"`
	Version              int             `json:"version"`
	CreatedBy            string          `json:"created_by"`
	CorrelationID        string          `json:"correlation_id"`
	CreatedAt            time.Time       `json:"created_at"`
	UpdatedAt            time.Time       `json:"updated_at"`
}

// glLine is the ledger's own line shape.
type glLine struct {
	MappingKey   string  `json:"mapping_key"`
	DebitAmount  float64 `json:"debit_amount,omitempty"`
	CreditAmount float64 `json:"credit_amount,omitempty"`
	Description  string  `json:"description,omitempty"`
}

// LedgerEventPayload renders the subject as the body of general-ledger-svc's
// POST /v1/postings/events. The proposal is the frozen one; only the float
// conversion happens here.
func (b *AccountingBridge) LedgerEventPayload() ([]byte, error) {
	lines := make([]glLine, 0, len(b.Lines))
	for _, l := range b.Lines {
		d, err := ToLedgerAmount(l.Debit)
		if err != nil {
			return nil, fmt.Errorf("line %s debit: %w", l.MappingKey, err)
		}
		c, err := ToLedgerAmount(l.Credit)
		if err != nil {
			return nil, fmt.Errorf("line %s credit: %w", l.MappingKey, err)
		}
		lines = append(lines, glLine{MappingKey: l.MappingKey, DebitAmount: d, CreditAmount: c, Description: l.Description})
	}
	return json.Marshal(map[string]any{
		"legal_entity_id": b.LegalEntityID, "fiscal_period": b.FiscalPeriod, "description": b.Description,
		"source_event_id": b.SourceEventID, "correlation_id": b.CorrelationID, "transaction_currency": b.TransactionCurrency,
		"document_date": b.DocumentDate, "posting_date": b.PostingDate, "lines": lines,
	})
}

type CreateAccountingEventRequest struct {
	RunRefID string `json:"run_ref_id"`
	// AccountingBookID is optional: the default book is used when omitted.
	AccountingBookID string `json:"accounting_book_id,omitempty"`
}

type AccountingTransitionRequest struct {
	ExpectedVersion int    `json:"expected_version"`
	Reason          string `json:"reason,omitempty"`
}

// ── events & errors ──────────────────────────────────────────────────────────

const (
	EventPayrollAccountingSubmitted = "payroll_accounting_submitted"
	EventPayrollAccountingPosted    = "payroll_accounting_posted"
	EventPayrollAccountingCorrected = "payroll_accounting_corrected"
)

var (
	ErrNothingToPost         = errorString("the payroll result has no amounts to post")
	ErrUnbalancedPosting     = errorString("the posting proposal does not balance")
	ErrBridgeNotFound        = errorString("payroll accounting subject not found")
	ErrBridgeVersionConflict = errorString("stale expected_version: the accounting subject changed since it was read")
	ErrResultNotPostable     = errorString("only an ACCEPTED payroll result can be posted")
	ErrSoDSamePrincipal      = errorString("separation of duties: the principal who received the payroll result cannot submit its posting")
	ErrBridgeNotSubmittable  = errorString("only an EVENT_CREATED or REJECTED subject can be submitted")
	ErrBridgeNotReversible   = errorString("only a POSTED subject whose result was superseded can be reversed")
	ErrBridgeNotWithdrawable = errorString("only an unposted subject whose result was superseded can be withdrawn")
	ErrBridgeNotPosted       = errorString("the subject has not posted")
	ErrLedgerUnavailable     = errorString("general-ledger-svc could not be reached")
)
