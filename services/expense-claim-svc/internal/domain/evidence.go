package domain

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"time"
)

// SubmissionSnapshot is exactly what the approver is shown for one
// submission version. It is written once to expense_claim_submissions and
// never changes.
type SubmissionSnapshot struct {
	VersionNo            int            `json:"version_no"`
	ClaimID              string         `json:"claim_id"`
	LegalEntityID        string         `json:"legal_entity_id"`
	ClaimantPrincipalID  string         `json:"claimant_principal_id"`
	Currency             string         `json:"currency"`
	BusinessPurpose      string         `json:"business_purpose"`
	ProjectCostCenter    string         `json:"project_cost_center"`
	PaymentPreferenceRef string         `json:"payment_preference_ref"`
	TotalAmount          float64        `json:"total_amount"`
	Lines                []SnapshotLine `json:"lines"`
	SubmittedBy          string         `json:"submitted_by"`
	SubmittedAt          time.Time      `json:"submitted_at"`
}

type SnapshotLine struct {
	LineID              string  `json:"line_id"`
	Merchant            string  `json:"merchant"`
	ExpenseDate         string  `json:"expense_date"`
	Amount              float64 `json:"amount"`
	Currency            string  `json:"currency"`
	Category            string  `json:"category"`
	ProjectCostCenter   string  `json:"project_cost_center"`
	ReceiptDocumentID   string  `json:"receipt_document_id"`
	ClaimTaxRecovery    bool    `json:"claim_tax_recovery"`
	Jurisdiction        string  `json:"jurisdiction"`
	TaxCategory         string  `json:"tax_category"`
	TaxDeterminationID  string  `json:"tax_determination_id"`
	TaxableAmount       float64 `json:"taxable_amount"`
	CalculatedTaxAmount float64 `json:"calculated_tax_amount"`
}

// BuildSnapshot freezes a claim and its active lines.
func BuildSnapshot(c *ExpenseClaim, lines []ExpenseLine, versionNo int, submittedBy string, at time.Time) SubmissionSnapshot {
	s := SubmissionSnapshot{
		VersionNo: versionNo, ClaimID: c.ClaimID, LegalEntityID: c.LegalEntityID,
		ClaimantPrincipalID: c.ClaimantPrincipalID, Currency: c.Currency, BusinessPurpose: c.BusinessPurpose,
		ProjectCostCenter: c.ProjectCostCenter, PaymentPreferenceRef: c.PaymentPreferenceRef,
		SubmittedBy: submittedBy, SubmittedAt: at.UTC(), Lines: make([]SnapshotLine, 0, len(lines)),
	}
	for _, l := range lines {
		s.TotalAmount += l.Amount
		s.Lines = append(s.Lines, SnapshotLine{
			LineID: l.LineID, Merchant: l.Merchant, ExpenseDate: l.ExpenseDate.UTC().Format(time.RFC3339), Amount: l.Amount,
			Currency: l.Currency, Category: l.Category, ProjectCostCenter: l.ProjectCostCenter,
			ReceiptDocumentID: l.ReceiptDocumentID, ClaimTaxRecovery: l.ClaimTaxRecovery, Jurisdiction: l.Jurisdiction,
			TaxCategory: l.TaxCategory, TaxDeterminationID: l.TaxDeterminationID, TaxableAmount: l.TaxableAmount,
			CalculatedTaxAmount: l.CalculatedTaxAmount,
		})
	}
	return s
}

// CanonicalJSON re-encodes any JSON document with sorted object keys, the
// form that is both stored (jsonb reorders keys anyway) and hashed.
func CanonicalJSON(raw []byte) ([]byte, error) {
	var v interface{}
	if err := json.Unmarshal(raw, &v); err != nil {
		return nil, err
	}
	return json.Marshal(v)
}

// SnapshotHash is the hex SHA-256 of the canonical snapshot JSON; it can be
// recomputed from the stored row to prove the evidence was not altered.
func SnapshotHash(raw []byte) (canonical []byte, hash string, err error) {
	canonical, err = CanonicalJSON(raw)
	if err != nil {
		return nil, "", err
	}
	sum := sha256.Sum256(canonical)
	return canonical, hex.EncodeToString(sum[:]), nil
}

// AccountingEventRequest mirrors services/_contract/accounting.AccountingEvent
// (the contract module cannot be imported into a per-service Docker build
// context, so the wire shape is reproduced here and pinned by a test).
type AccountingEventRequest struct {
	AccountingEventID    string          `json:"accounting_event_id"`
	TenantID             string          `json:"tenant_id"`
	LegalEntityID        string          `json:"legal_entity_id"`
	SourceDomain         string          `json:"source_domain"`
	SourceObjectTable    string          `json:"source_object_table"`
	SourceObjectID       string          `json:"source_object_id"`
	EventType            string          `json:"event_type"`
	OccurredAt           time.Time       `json:"occurred_at"`
	EffectiveDate        time.Time       `json:"effective_date"`
	AmountBasis          json.RawMessage `json:"amount_basis"`
	PostingPolicyVersion string          `json:"posting_policy_version"`
	Status               string          `json:"status"`
	CreatedAt            time.Time       `json:"created_at"`
}

// AccountingRequested wraps the contract event with the business fact that
// caused it. Consumers: the Accounting Kernel only (invariant #20).
type AccountingRequested struct {
	BusinessFact    string                 `json:"business_fact"`
	AccountingEvent AccountingEventRequest `json:"accounting_event"`
}

const PostingPolicyVersion = "ap07.expense-claim-approved.v1"

type accountingBasis struct {
	ClaimID             string                `json:"claim_id"`
	ClaimantPrincipalID string                `json:"claimant_principal_id"`
	Currency            string                `json:"currency"`
	TotalAmount         float64               `json:"total_amount"`
	TaxRecoverableTotal float64               `json:"tax_recoverable_total"`
	PolicyVersionID     string                `json:"policy_version_id"`
	Lines               []accountingBasisLine `json:"lines"`
}

type accountingBasisLine struct {
	LineID              string  `json:"line_id"`
	Category            string  `json:"category"`
	ProjectCostCenter   string  `json:"project_cost_center"`
	Amount              float64 `json:"amount"`
	TaxDeterminationID  string  `json:"tax_determination_id,omitempty"`
	TaxableAmount       float64 `json:"taxable_amount,omitempty"`
	CalculatedTaxAmount float64 `json:"calculated_tax_amount,omitempty"`
}

// NewAccountingRequested builds the expense/payable accounting fact for an
// approved claim. Tax recoverability is taken only from lines that carry a
// TAX determination id — never inferred.
func NewAccountingRequested(eventID string, c *ExpenseClaim, lines []ExpenseLine, now time.Time) (AccountingRequested, error) {
	b := accountingBasis{
		ClaimID: c.ClaimID, ClaimantPrincipalID: c.ClaimantPrincipalID, Currency: c.Currency, PolicyVersionID: c.PolicyVersionID,
		Lines: make([]accountingBasisLine, 0, len(lines)),
	}
	for _, l := range lines {
		b.TotalAmount += l.Amount
		bl := accountingBasisLine{LineID: l.LineID, Category: l.Category, ProjectCostCenter: l.ProjectCostCenter, Amount: l.Amount}
		if l.ClaimTaxRecovery && l.TaxDeterminationID != "" {
			bl.TaxDeterminationID, bl.TaxableAmount, bl.CalculatedTaxAmount = l.TaxDeterminationID, l.TaxableAmount, l.CalculatedTaxAmount
			b.TaxRecoverableTotal += l.CalculatedTaxAmount
		}
		b.Lines = append(b.Lines, bl)
	}
	raw, err := json.Marshal(b)
	if err != nil {
		return AccountingRequested{}, err
	}
	tenant := ""
	if c.TenantID != nil {
		tenant = *c.TenantID
	}
	return AccountingRequested{
		BusinessFact: "ExpenseClaimApproved",
		AccountingEvent: AccountingEventRequest{
			AccountingEventID: eventID, TenantID: tenant, LegalEntityID: c.LegalEntityID, SourceDomain: "AP",
			SourceObjectTable: "expense_claims", SourceObjectID: c.ClaimID, EventType: "EXPENSE_CLAIM_APPROVED",
			OccurredAt: now.UTC(), EffectiveDate: now.UTC().Truncate(24 * time.Hour), AmountBasis: raw,
			PostingPolicyVersion: PostingPolicyVersion, Status: "PENDING", CreatedAt: now.UTC(),
		},
	}, nil
}
