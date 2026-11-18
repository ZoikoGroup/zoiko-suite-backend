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
	ProgramID            string         `json:"program_id,omitempty"`
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
	// ProgramID is the business program_id (MigrationProgram.ProgramID, not
	// its internal uuid) this batch belongs to. Optional for backward
	// compatibility: a promote call that omits it behaves exactly as before
	// this field existed (the resulting batch ref simply has no program).
	ProgramID         string                 `json:"program_id,omitempty"`
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

// ─── Migration Program ─────────────────────────────────────────────────────

// ProgramStatus is a MigrationProgram's own lifecycle, separate from any
// individual batch's status: a program stays OPEN/IN_PROGRESS across many
// validation jobs and promoted batches over the life of a migration
// project, and only reaches COMPLETE by an explicit transition (never
// inferred from "every batch so far happens to be reconciled" -- a program
// can be COMPLETE with batches still pending in a later wave, or still
// IN_PROGRESS after its first batch reconciles).
type ProgramStatus string

const (
	ProgramOpen       ProgramStatus = "OPEN"
	ProgramInProgress ProgramStatus = "IN_PROGRESS"
	ProgramComplete   ProgramStatus = "COMPLETE"
)

// validProgramTransitions is the only linear, forward-only path a program's
// status may take -- the same shape accounting-period-svc enforces for its
// own guarded lifecycle (OPEN -> SOFT_CLOSED -> ...): one step forward, no
// skipping, no going back.
var validProgramTransitions = map[ProgramStatus]ProgramStatus{
	ProgramOpen:       ProgramInProgress,
	ProgramInProgress: ProgramComplete,
}

var validCutoverStrategies = map[string]bool{"BIG_BANG": true, "PHASED": true, "PARALLEL_RUN": true}

// MigrationProgram is the top-level tracking object ZS-MIG-001 §4
// describes: "this is one migration project" (e.g. one customer's full
// onboarding), which can contain multiple batches over time. It aggregates
// the MigrationBatchRefs that name it (ProgramBatchSummary); it holds no
// posting state of its own.
type MigrationProgram struct {
	ID               string        `json:"id"`
	TenantID         string        `json:"tenant_id"`
	ProgramID        string        `json:"program_id"`
	SourceEstate     string        `json:"source_estate"`
	TargetScope      string        `json:"target_scope"`
	OwnerPrincipalID string        `json:"owner_principal_id"`
	CutoverStrategy  string        `json:"cutover_strategy"`
	Status           ProgramStatus `json:"status"`
	CreatedBy        string        `json:"created_by"`
	CreatedAt        time.Time     `json:"created_at"`
	UpdatedAt        time.Time     `json:"updated_at"`
}

// ProgramBatchSummary is how a program "aggregates/reports on its batches":
// how many batches it has, and a breakdown of their current promotion
// status, read live from migration_batch_refs rather than kept as a
// counter on the program row (so it can never drift from the batches it
// describes).
type ProgramBatchSummary struct {
	TotalBatches    int            `json:"total_batches"`
	StatusCounts    map[string]int `json:"status_counts"`
	ReconciledCount int            `json:"reconciled_count"`
}

// MigrationProgramView is a program plus its live batch summary -- the body
// of GET /v1/migrations/programs/{id}.
type MigrationProgramView struct {
	MigrationProgram
	Batches ProgramBatchSummary `json:"batches"`
}

// CreateProgramRequest is the body of POST /v1/migrations/programs.
type CreateProgramRequest struct {
	ProgramID        string `json:"program_id"`
	SourceEstate     string `json:"source_estate"`
	TargetScope      string `json:"target_scope"`
	OwnerPrincipalID string `json:"owner_principal_id"`
	CutoverStrategy  string `json:"cutover_strategy"`
}

func (r *CreateProgramRequest) Validate() error {
	if strings.TrimSpace(r.ProgramID) == "" {
		return fmt.Errorf("program_id is required")
	}
	if strings.TrimSpace(r.SourceEstate) == "" {
		return fmt.Errorf("source_estate is required")
	}
	if strings.TrimSpace(r.TargetScope) == "" {
		return fmt.Errorf("target_scope is required")
	}
	if strings.TrimSpace(r.OwnerPrincipalID) == "" {
		return fmt.Errorf("owner_principal_id is required")
	}
	if !validCutoverStrategies[strings.ToUpper(strings.TrimSpace(r.CutoverStrategy))] {
		return fmt.Errorf("cutover_strategy must be one of BIG_BANG, PHASED, PARALLEL_RUN")
	}
	return nil
}

// UpdateProgramStatusRequest is the body of
// POST /v1/migrations/programs/{id}/status.
type UpdateProgramStatusRequest struct {
	Status string `json:"status"`
}

func (r *UpdateProgramStatusRequest) Validate() error {
	switch ProgramStatus(strings.ToUpper(strings.TrimSpace(r.Status))) {
	case ProgramOpen, ProgramInProgress, ProgramComplete:
		return nil
	default:
		return fmt.Errorf("status must be one of OPEN, IN_PROGRESS, COMPLETE")
	}
}

// NextProgramStatus validates the requested transition against the
// program's current status and returns the new status to apply.
func NextProgramStatus(current ProgramStatus, requested ProgramStatus) (ProgramStatus, error) {
	if current == requested {
		return current, nil // idempotent replay of the same transition
	}
	if validProgramTransitions[current] == requested {
		return requested, nil
	}
	return "", fmt.Errorf("illegal program status transition %s -> %s", current, requested)
}

// ─── Source Extract ─────────────────────────────────────────────────────────

// ExtractAmountControl is one named control total carried on a
// SourceExtract -- the same shape PromoteCtrl already uses for a batch's
// control totals, reused here because it is the same concept one layer
// earlier: a control total the source side asserts about the extract,
// before any of it has been mapped into GL lines.
type ExtractAmountControl struct {
	Total string `json:"total"`
	Count int    `json:"count"`
	Ref   string `json:"reference,omitempty"`
}

// SourceExtract (ZS-MIG-001 §4, MIG-INV-03): a frozen, versioned landing of
// source data. Immutable once created -- there is no update endpoint for
// it anywhere in this service, and the database refuses UPDATE/DELETE/
// TRUNCATE on its table outright (migration 000010). A correction is a new
// row: same ExtractID, Version = previous max + 1.
type SourceExtract struct {
	ID              string                          `json:"id"`
	TenantID        string                          `json:"tenant_id"`
	LegalEntityID   string                          `json:"legal_entity_id"`
	ProgramID       string                          `json:"program_id,omitempty"`
	ExtractID       string                          `json:"extract_id"`
	Version         int                             `json:"version"`
	SourceWatermark string                          `json:"source_watermark"`
	GeneratedAt     time.Time                       `json:"generated_at"`
	ManifestHash    string                          `json:"manifest_hash"`
	RecordCount     int                             `json:"record_count"`
	Files           []string                        `json:"files"`
	AmountControls  map[string]ExtractAmountControl `json:"amount_controls,omitempty"`
	CreatedBy       string                          `json:"created_by"`
	CreatedAt       time.Time                       `json:"created_at"`
}

// RegisterExtractRequest is the body of POST /v1/migrations/extracts. If
// ExtractID already names an existing extract for this tenant, the store
// registers this as the next version (a correction, MIG-INV-03); if it is
// new (or omitted), a fresh ExtractID is minted at version 1.
type RegisterExtractRequest struct {
	ExtractID       string                          `json:"extract_id,omitempty"`
	LegalEntityID   string                          `json:"legal_entity_id"`
	ProgramID       string                          `json:"program_id,omitempty"`
	SourceWatermark string                          `json:"source_watermark"`
	GeneratedAt     time.Time                       `json:"generated_at"`
	ManifestHash    string                          `json:"manifest_hash"`
	RecordCount     int                             `json:"record_count"`
	Files           []string                        `json:"files"`
	AmountControls  map[string]ExtractAmountControl `json:"amount_controls,omitempty"`
}

func (r *RegisterExtractRequest) Validate() error {
	if strings.TrimSpace(r.LegalEntityID) == "" {
		return fmt.Errorf("legal_entity_id is required")
	}
	if strings.TrimSpace(r.SourceWatermark) == "" {
		return fmt.Errorf("source_watermark is required")
	}
	if r.GeneratedAt.IsZero() {
		return fmt.Errorf("generated_at is required")
	}
	if strings.TrimSpace(r.ManifestHash) == "" {
		return fmt.Errorf("manifest_hash is required")
	}
	if r.RecordCount < 0 {
		return fmt.Errorf("record_count cannot be negative")
	}
	return nil
}
