// COM-05 gap-remediation types, from the skeptical re-audit of ZS-SVC-Q-001
// §6/§7: CommercialEvidencePackage (D1), registered tax jurisdictions (D2),
// and chargeback/dispute tracking (D4). D3 (seller accounting-event mapping)
// needed no new domain type — it reuses BillingAccount.AccountingMappingKey
// (com05_billing.go) and the pre-existing outbox event shape.
package domain

import "time"

const (
	PrefixEvidencePackage = "cevp_"
	PrefixDispute         = "cdis_"
)

// ── D1: CommercialEvidencePackage ───────────────────────────────────────────

// EvidenceLine is one rated line's manifest entry: the price version content
// hash it was rated against, and — for a USAGE line — the exact usage
// statement status/total_quantity the invoice was issued from.
type EvidenceLine struct {
	LineNo             int     `json:"line_no"`
	Kind               string  `json:"kind"`
	PriceVersionID     string  `json:"price_version_id"`
	PriceVersionSHA256 *string `json:"price_version_sha256,omitempty"`
	StatementID        *string `json:"statement_id,omitempty"`
	StatementStatus    *string `json:"statement_status,omitempty"`
	StatementTotalQty  *string `json:"statement_total_quantity,omitempty"`
}

// EvidenceManifest is the sealed content of a CommercialEvidencePackage —
// everything needed to prove what an issued invoice was rated from, without
// re-querying the live subscription/pricebook/usage tables that may have
// moved on since.
type EvidenceManifest struct {
	InvoiceID             string         `json:"invoice_id"`
	InvoiceNumber         string         `json:"invoice_number"`
	SubscriptionID        string         `json:"subscription_id"`
	SubscriptionVersionID string         `json:"subscription_version_id"`
	TermNo                int            `json:"term_no"`
	Lines                 []EvidenceLine `json:"lines"`
}

// CommercialEvidencePackage is the immutable, sealed manifest tying one
// issued invoice to its exact source lineage (ZS-SVC-Q-001 §7). Sealed
// automatically at issue time, in the same transaction as the invoice
// itself — never a separate operator command.
type CommercialEvidencePackage struct {
	PackageID           string           `json:"package_id"`
	InvoiceID           string           `json:"invoice_id"`
	OrganizationID      string           `json:"organization_id"`
	Manifest            EvidenceManifest `json:"manifest"`
	ManifestSHA256      string           `json:"manifest_sha256"`
	SealedAt            time.Time        `json:"sealed_at"`
	SealedByPrincipalID string           `json:"sealed_by_principal_id"`
}

// ── D2: registered tax jurisdictions ────────────────────────────────────────

// TaxJurisdiction is one seller-registered (billing account, jurisdiction)
// fact — the same "explicit registration required" doctrine already used
// for currencies and meters, applied to the one field of
// GenerateInvoiceCandidateRequest that was previously a bare, unchecked
// caller assertion. RegisteredRateBasisPoints is nil when the seller has
// registered the jurisdiction as valid but leaves the rate itself to the
// caller's own supplied evidence for now.
type TaxJurisdiction struct {
	BillingAccountID          string    `json:"billing_account_id"`
	JurisdictionCode          string    `json:"jurisdiction_code"`
	RegisteredRateBasisPoints *int      `json:"registered_rate_basis_points,omitempty"`
	EffectiveFrom             time.Time `json:"effective_from"`
	CreatedAt                 time.Time `json:"created_at"`
	CreatedByPrincipalID      string    `json:"created_by_principal_id"`
}

func ValidateTaxJurisdiction(j *TaxJurisdiction) error {
	if strEmpty(j.BillingAccountID) {
		return invalid("billing_account_id", "is required")
	}
	if strEmpty(j.JurisdictionCode) {
		return invalid("jurisdiction_code", "is required")
	}
	if j.RegisteredRateBasisPoints != nil && (*j.RegisteredRateBasisPoints < 0 || *j.RegisteredRateBasisPoints > 10000) {
		return invalid("registered_rate_basis_points", "must be between 0 and 10000")
	}
	return nil
}

// ── D4: chargeback / dispute ────────────────────────────────────────────────

type DisputeStatus string

const (
	DisputeOpen     DisputeStatus = "OPEN"
	DisputeWon      DisputeStatus = "WON"
	DisputeLost     DisputeStatus = "LOST"
	DisputeResolved DisputeStatus = "RESOLVED"
)

// DisputeCase is purely orthogonal chargeback tracking (ZS-SVC-Q-001 §6):
// independent of invoice issuance and payment collection. Nothing in its
// own lifecycle trigger, or in any command that mutates it, ever writes to
// payment_attempts or platform_commercial_invoices — money actually moving
// as a result of a LOST dispute goes through the pre-existing RequestRefund/
// ApplyWriteOff commands, optionally cross-referenced via RelatedRefundID.
type DisputeCase struct {
	DisputeID                    string        `json:"dispute_id"`
	OrganizationID               string        `json:"organization_id"`
	InvoiceID                    string        `json:"invoice_id"`
	PaymentAttemptID             string        `json:"payment_attempt_id"`
	Status                       DisputeStatus `json:"status"`
	Reason                       string        `json:"reason"`
	OpenedAt                     time.Time     `json:"opened_at"`
	OpenedByPrincipalID          string        `json:"opened_by_principal_id"`
	OutcomeRecordedAt            *time.Time    `json:"outcome_recorded_at,omitempty"`
	OutcomeRecordedByPrincipalID *string       `json:"outcome_recorded_by_principal_id,omitempty"`
	ResolvedAt                   *time.Time    `json:"resolved_at,omitempty"`
	ResolvedByPrincipalID        *string       `json:"resolved_by_principal_id,omitempty"`
	ResolutionNotes              *string       `json:"resolution_notes,omitempty"`
	RelatedRefundID              *string       `json:"related_refund_id,omitempty"`
}

func ValidateOpenDispute(reason string) error {
	if strEmpty(reason) {
		return invalid("reason", "is required")
	}
	return nil
}

var (
	ErrDisputeCaseNotFound        = errorString("dispute case not found")
	ErrDisputeCaseInvalidState    = errorString("dispute case is not in a state that allows this action")
	ErrDisputeAttemptNotSucceeded = errorString("a dispute can only be opened against a SUCCEEDED payment attempt")
	ErrEvidencePackageNotFound    = errorString("evidence package not found")
)
