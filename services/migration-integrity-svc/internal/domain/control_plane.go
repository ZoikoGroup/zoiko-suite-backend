package domain

import (
	"fmt"
	"strings"
	"time"
)

// This file is the "program/tracking layer" ZS-MIG-001 describes on top of
// the field-level integrity engine in types.go: a reference that ties one
// validation run (MigrationJob) to a real posting batch general-ledger-svc
// owns (MigrationBatchRef), a permanent cross-reference between legacy and
// canonical identifiers (IdentityCrosswalk, MIG-INV-14), and the certificate
// issued once a batch has both validated clean and posted/reconciled for
// real in general-ledger-svc (MigrationCertificate). None of this duplicates
// general-ledger-svc's own migration-batch state machine
// (internal/kernel/migration.go there) -- it references it.

// ─── Batch Ref ─────────────────────────────────────────────────────────────

type BatchRefStatus string

const (
	BatchRefPending    BatchRefStatus = "PENDING"
	BatchRefStaged     BatchRefStatus = "STAGED"
	BatchRefApproved   BatchRefStatus = "APPROVED"
	BatchRefPosted     BatchRefStatus = "POSTED"
	BatchRefReconciled BatchRefStatus = "RECONCILED"
	BatchRefFailed     BatchRefStatus = "FAILED"
)

// MigrationBatchRef ties this service's validation job to the real batch
// general-ledger-svc executes. GLBatchID/GLJournalID/GLJournalNumber are
// general-ledger-svc's own identifiers, copied back from its responses --
// never minted here.
type MigrationBatchRef struct {
	ID                   string         `json:"id"`
	TenantID             string         `json:"tenant_id"`
	LegalEntityID        string         `json:"legal_entity_id"`
	JobID                string         `json:"job_id"`
	BatchCode            string         `json:"batch_code"`
	GLBookID             string         `json:"gl_book_id"`
	GLBatchID            string         `json:"gl_batch_id,omitempty"`
	GLJournalID          string         `json:"gl_journal_id,omitempty"`
	GLJournalNumber      string         `json:"gl_journal_number,omitempty"`
	Status               BatchRefStatus `json:"status"`
	ReconciliationStatus string         `json:"reconciliation_status,omitempty"`
	FailureReason        string         `json:"failure_reason,omitempty"`
	CreatedBy            string         `json:"created_by"`
	CreatedAt            time.Time      `json:"created_at"`
	UpdatedAt            time.Time      `json:"updated_at"`
}

// PromoteBatchRequest is the body of POST /v1/migrations/{id}/promote. It
// carries the GL-shaped batch content the generic field-level validation in
// types.go knows nothing about (account mapping, debit/credit, the cutover
// window): the integrity job answers "is this extract trustworthy", this
// request answers "where does it post and under what authority".
type PromoteBatchRequest struct {
	BookID            string                 `json:"book_id"`
	BatchCode         string                 `json:"batch_code"`
	CutoverDate       string                 `json:"cutover_date"`
	SourceExtractRef  string                 `json:"source_extract_ref"`
	SourceExtractHash string                 `json:"source_extract_hash"`
	MappingRef        string                 `json:"mapping_ref"`
	SourceTotals      PromoteTotals          `json:"source_totals"`
	Lines             []PromoteLine          `json:"lines"`
	ControlTotals     map[string]PromoteCtrl `json:"control_totals,omitempty"`

	// AuthorityRef/WindowStart/WindowEnd approve the batch under a named
	// migration authority, inside a cutover window -- general-ledger-svc
	// requires both, and requires the approver differ from the preparer
	// (ErrSelfApproval); see PromoterPrincipalID.
	AuthorityRef string    `json:"authority_ref"`
	WindowStart  time.Time `json:"window_start"`
	WindowEnd    time.Time `json:"window_end"`

	// PreparerPrincipalID stages the batch in general-ledger-svc; the
	// caller promoting it (X-Principal-Id) approves it. They must differ,
	// matching general-ledger-svc's own self-approval rule.
	PreparerPrincipalID string `json:"preparer_principal_id"`
}

type PromoteTotals struct {
	Debit  string `json:"debit"`
	Credit string `json:"credit"`
}

type PromoteCtrl struct {
	Total string `json:"total"`
	Count int    `json:"count"`
	Ref   string `json:"reference"`
}

// PromoteLine is one opening line. SourceRef MUST already have a resolved
// IdentityCrosswalk entry (source_system/source_type/source_id ->
// canonical_id) before a batch naming it can be promoted -- see
// Handler.PromoteBatch's crosswalk-resolved check.
type PromoteLine struct {
	SourceRef           string            `json:"source_ref"`
	SourceType          string            `json:"source_type"`
	SourceAccount       string            `json:"source_account"`
	AccountCode         string            `json:"account_code"`
	Debit               string            `json:"debit"`
	Credit              string            `json:"credit"`
	CounterpartyID      string            `json:"counterparty_id,omitempty"`
	TransactionAmount   string            `json:"transaction_amount,omitempty"`
	TransactionCurrency string            `json:"transaction_currency,omitempty"`
	ExchangeRate        string            `json:"exchange_rate,omitempty"`
	Dimensions          map[string]string `json:"dimensions,omitempty"`
	Description         string            `json:"description,omitempty"`
}

func (r *PromoteBatchRequest) Validate() error {
	if strings.TrimSpace(r.BookID) == "" {
		return fmt.Errorf("book_id is required")
	}
	if strings.TrimSpace(r.BatchCode) == "" {
		return fmt.Errorf("batch_code is required")
	}
	if strings.TrimSpace(r.CutoverDate) == "" {
		return fmt.Errorf("cutover_date is required")
	}
	if strings.TrimSpace(r.SourceExtractRef) == "" {
		return fmt.Errorf("source_extract_ref is required")
	}
	if strings.TrimSpace(r.SourceExtractHash) == "" {
		return fmt.Errorf("source_extract_hash is required")
	}
	if strings.TrimSpace(r.MappingRef) == "" {
		return fmt.Errorf("mapping_ref is required")
	}
	if len(r.Lines) == 0 {
		return fmt.Errorf("lines cannot be empty")
	}
	if strings.TrimSpace(r.AuthorityRef) == "" {
		return fmt.Errorf("authority_ref is required")
	}
	if strings.TrimSpace(r.PreparerPrincipalID) == "" {
		return fmt.Errorf("preparer_principal_id is required")
	}
	if r.WindowStart.IsZero() || !r.WindowEnd.After(r.WindowStart) {
		return fmt.Errorf("window_end must be after window_start")
	}
	return nil
}

// ─── Identity Crosswalk ────────────────────────────────────────────────────

// IdentityCrosswalk is a permanent correspondence between a legacy source
// identifier and the platform's canonical identifier for the same
// real-world object (ZS-MIG-001 MIG-INV-14). It outlives the job that
// first established it: later extracts reuse the mapping rather than
// re-deciding it.
type IdentityCrosswalk struct {
	ID            string    `json:"id"`
	TenantID      string    `json:"tenant_id"`
	SourceSystem  string    `json:"source_system"`
	SourceType    string    `json:"source_type"`
	SourceID      string    `json:"source_id"`
	CanonicalID   string    `json:"canonical_id"`
	MatchMethod   string    `json:"match_method"`
	MatchEvidence string    `json:"match_evidence,omitempty"`
	MatchedBy     string    `json:"matched_by"`
	FirstJobID    string    `json:"first_job_id,omitempty"`
	CreatedAt     time.Time `json:"created_at"`
	UpdatedAt     time.Time `json:"updated_at"`
}

var validMatchMethods = map[string]bool{"EXACT": true, "DETERMINISTIC_RULE": true, "MANUAL": true, "FUZZY": true}

// UpsertCrosswalkRequest is the body of POST /v1/migrations/crosswalk.
type UpsertCrosswalkRequest struct {
	Entries []CrosswalkEntryInput `json:"entries"`
}

type CrosswalkEntryInput struct {
	SourceSystem  string `json:"source_system"`
	SourceType    string `json:"source_type"`
	SourceID      string `json:"source_id"`
	CanonicalID   string `json:"canonical_id"`
	MatchMethod   string `json:"match_method"`
	MatchEvidence string `json:"match_evidence,omitempty"`
	JobID         string `json:"job_id,omitempty"`
}

func (r *UpsertCrosswalkRequest) Validate() error {
	if len(r.Entries) == 0 {
		return fmt.Errorf("entries cannot be empty")
	}
	for i, e := range r.Entries {
		if strings.TrimSpace(e.SourceSystem) == "" || strings.TrimSpace(e.SourceType) == "" || strings.TrimSpace(e.SourceID) == "" {
			return fmt.Errorf("entries[%d]: source_system, source_type and source_id are required", i)
		}
		if strings.TrimSpace(e.CanonicalID) == "" {
			return fmt.Errorf("entries[%d]: canonical_id is required", i)
		}
		if !validMatchMethods[strings.ToUpper(strings.TrimSpace(e.MatchMethod))] {
			return fmt.Errorf("entries[%d]: match_method must be one of EXACT, DETERMINISTIC_RULE, MANUAL, FUZZY", i)
		}
	}
	return nil
}

// ─── Certificate ───────────────────────────────────────────────────────────

// MigrationCertificate is the evidence this service exists to issue: a
// named scope of batches loaded completely and correctly, backed by
// general-ledger-svc's own Post+Reconcile result (never asserted
// independently -- see the handler).
type MigrationCertificate struct {
	ID                     string    `json:"id"`
	TenantID               string    `json:"tenant_id"`
	LegalEntityID          string    `json:"legal_entity_id"`
	Scope                  string    `json:"scope"`
	BatchRefID             string    `json:"batch_ref_id"`
	JobID                  string    `json:"job_id"`
	UnresolvedExceptions   int       `json:"unresolved_exceptions"`
	GLBatchID              string    `json:"gl_batch_id"`
	GLJournalID            string    `json:"gl_journal_id"`
	GLJournalNumber        string    `json:"gl_journal_number"`
	GLReconciliationStatus string    `json:"gl_reconciliation_status"`
	GLEvidenceHash         string    `json:"gl_evidence_hash"`
	GLEvidenceVerified     bool      `json:"gl_evidence_verified"`
	SignerPrincipalID      string    `json:"signer_principal_id"`
	IssuedAt               time.Time `json:"issued_at"`
}

// IssueCertificateRequest is the body of POST /v1/migrations/{id}/certificate.
type IssueCertificateRequest struct {
	Scope string `json:"scope"`
}

func (r *IssueCertificateRequest) Validate() error {
	if strings.TrimSpace(r.Scope) == "" {
		return fmt.Errorf("scope is required")
	}
	return nil
}
