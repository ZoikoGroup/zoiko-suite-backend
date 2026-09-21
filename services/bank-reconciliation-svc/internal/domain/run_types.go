package domain

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
	"time"
)

// ReconciliationRunStatus is the lifecycle of a reconciliation run.
type ReconciliationRunStatus string

const (
	RunStatusDraft                 ReconciliationRunStatus = "DRAFT"
	RunStatusRunning               ReconciliationRunStatus = "RUNNING"
	RunStatusExceptionsOpen        ReconciliationRunStatus = "EXCEPTIONS_OPEN"
	RunStatusReperformed           ReconciliationRunStatus = "REPERFORMED"
	RunStatusReadyForCertification ReconciliationRunStatus = "READY_FOR_CERTIFICATION"
	RunStatusCertified             ReconciliationRunStatus = "CERTIFIED"
	RunStatusFailed                ReconciliationRunStatus = "FAILED"
	RunStatusSuperseded            ReconciliationRunStatus = "SUPERSEDED"
)

type ReconciliationRun struct {
	RunID                  string                  `json:"run_id"`
	TenantID               string                  `json:"tenant_id"`
	LegalEntityID          string                  `json:"legal_entity_id"`
	BankAccountID          string                  `json:"bank_account_id"`
	StatementDate          string                  `json:"statement_date"`
	Status                 ReconciliationRunStatus `json:"status"`
	PolicyID               *string                 `json:"policy_id,omitempty"`
	PolicyVersion          *int                    `json:"policy_version,omitempty"`
	MaxUnmatchedCount      int                     `json:"max_unmatched_count"`
	MaxUnmatchedPct        float64                 `json:"max_unmatched_pct"`
	PopulationID           *string                 `json:"population_id,omitempty"`
	PopulationVersion      int                     `json:"population_version"`
	CertifiedByPrincipalID *string                 `json:"certified_by_principal_id,omitempty"`
	CertifiedAt            *time.Time              `json:"certified_at,omitempty"`
	SupersededByRunID      *string                 `json:"superseded_by_run_id,omitempty"`
	PriorRunID             *string                 `json:"prior_run_id,omitempty"`
	CreatedByPrincipalID   string                  `json:"created_by_principal_id"`
	CreatedAt              time.Time               `json:"created_at"`
	UpdatedAt              time.Time               `json:"updated_at"`
	CorrelationID          string                  `json:"correlation_id"`
}

type StartRunRequest struct {
	TenantID      string `json:"tenant_id"`
	LegalEntityID string `json:"legal_entity_id"`
	BankAccountID string `json:"bank_account_id"`
	StatementDate string `json:"statement_date"`
	CorrelationID string `json:"correlation_id"`
}

type ReconciliationPolicy struct {
	PolicyID             string    `json:"policy_id"`
	TenantID             string    `json:"tenant_id"`
	LegalEntityID        string    `json:"legal_entity_id"`
	PolicyVersion        int       `json:"policy_version"`
	MaxUnmatchedCount    int       `json:"max_unmatched_count"`
	MaxUnmatchedPct      float64   `json:"max_unmatched_pct"`
	MaxUnresolvedAmount  float64   `json:"max_unresolved_amount"`
	Currency             string    `json:"currency"`
	Rationale            string    `json:"rationale"`
	EffectiveFrom        string    `json:"effective_from"`
	EffectiveTo          *string   `json:"effective_to,omitempty"`
	CreatedByPrincipalID string    `json:"created_by_principal_id"`
	CreatedAt            time.Time `json:"created_at"`
}

type CreatePolicyRequest struct {
	TenantID            string  `json:"tenant_id"`
	LegalEntityID       string  `json:"legal_entity_id"`
	MaxUnmatchedCount   int     `json:"max_unmatched_count"`
	MaxUnmatchedPct     float64 `json:"max_unmatched_pct"`
	MaxUnresolvedAmount float64 `json:"max_unresolved_amount"`
	Currency            string  `json:"currency"`
	Rationale           string  `json:"rationale"`
	EffectiveFrom       string  `json:"effective_from"`
	CorrelationID       string  `json:"correlation_id"`
}

type ReconciliationPopulation struct {
	PopulationID        string     `json:"population_id"`
	RunID               string     `json:"run_id"`
	TenantID            string     `json:"tenant_id"`
	LegalEntityID       string     `json:"legal_entity_id"`
	BankAccountID       string     `json:"bank_account_id"`
	StatementDate       string     `json:"statement_date"`
	SnapshotVersion     int        `json:"snapshot_version"`
	LineCount           int        `json:"line_count"`
	TotalAmountCents    int64      `json:"total_amount_cents"`
	BankPopulationHash  string     `json:"bank_population_hash"`
	FrozenAt            time.Time  `json:"frozen_at"`
	FrozenByPrincipalID string     `json:"frozen_by_principal_id"`
	SourceWatermark     *time.Time `json:"source_watermark,omitempty"`
	PolicyID            *string    `json:"policy_id,omitempty"`
	CorrelationID       string     `json:"correlation_id"`
}

type PopulationLineItem struct {
	PopulationID    string     `json:"population_id"`
	TenantID        string     `json:"tenant_id"`
	StatementLineID string     `json:"statement_line_id"`
	SourceSystem    string     `json:"source_system"`
	SourceRecordID  string     `json:"source_record_id"`
	TransactionDate time.Time  `json:"transaction_date"`
	AmountCents     int64      `json:"amount_cents"`
	Currency        string     `json:"currency"`
	BankReference   string     `json:"bank_reference"`
	LineStatus      string     `json:"line_status"`
	Included        bool       `json:"included"`
	RowHash         string     `json:"row_hash"`
	SourceWatermark *time.Time `json:"source_watermark,omitempty"`
}

// HashPopulationLines computes a deterministic SHA-256 hash over the population.
// Items are sorted by statement_line_id before hashing to ensure
// canonical order regardless of insertion order.
func HashPopulationLines(items []PopulationLineItem) string {
	type hashItem struct {
		id     string
		fields string
	}
	hs := make([]hashItem, len(items))
	for i, item := range items {
		hs[i] = hashItem{
			id:     item.StatementLineID,
			fields: fmt.Sprintf("%s|%d|%s|%s|%v", item.SourceRecordID, item.AmountCents, item.Currency, item.LineStatus, item.Included),
		}
	}
	sort.Slice(hs, func(i, j int) bool { return hs[i].id < hs[j].id })
	h := sha256.New()
	for _, item := range hs {
		fmt.Fprintf(h, "%s:%s\n", item.id, item.fields)
	}
	return hex.EncodeToString(h.Sum(nil))
}

// AutoMatchCandidate is one caller-supplied line/journal (or line/canonical
// transaction) pairing for RunAutomaticMatching to attempt. Supplying the
// candidate journal/transaction — rather than having the server guess one
// — keeps the same "never guess" posture as the manual match path; what's
// automated here is the verification-and-match step, not the sourcing of
// candidates.
type AutoMatchCandidate struct {
	StatementLineID string `json:"statement_line_id"`
	JournalID       string `json:"journal_id,omitempty"`
	TransactionID   string `json:"transaction_id,omitempty"`
}

type RunAutomaticMatchingRequest struct {
	Candidates []AutoMatchCandidate `json:"candidates"`
}

// AutoMatchResult reports what happened to one candidate. A candidate is
// never silently dropped — it either matched or carries a Reason
// explaining why it didn't, so a caller can tell "verified and matched"
// apart from "skipped because it wasn't actually an UNMATCHED line in this
// run's frozen population" or "failed independent verification."
type AutoMatchResult struct {
	StatementLineID string `json:"statement_line_id"`
	Matched         bool   `json:"matched"`
	Reason          string `json:"reason,omitempty"`
}

type RunAutomaticMatchingResponse struct {
	RunID        string            `json:"run_id"`
	MatchedCount int               `json:"matched_count"`
	SkippedCount int               `json:"skipped_count"`
	Results      []AutoMatchResult `json:"results"`
}

// Sentinel errors for the run model.
var (
	ErrRunNotFound                 = errorString("reconciliation run not found")
	ErrRunAlreadyExists            = errorString("a reconciliation run already exists for this account and date")
	ErrRunInvalidTransition        = errorString("reconciliation run is not in a state that permits this action")
	ErrRunAlreadyCertified         = errorString("reconciliation run is already certified")
	ErrRunSuperseded               = errorString("reconciliation run has been superseded")
	ErrRunNotReadyForCertification = errorString("reconciliation run is not ready for certification")
	ErrRunPopulationNotFrozen      = errorString("reconciliation run population has not been frozen")
	ErrPolicyNotFound              = errorString("reconciliation policy not found")
	ErrPolicyWouldWidenActiveRun   = errorString("policy cannot be widened while a reconciliation run is active")
	ErrPopulationNotFound          = errorString("population snapshot not found")
	ErrMaterialResidualBlocked     = errorString("material unmatched residual exceeds policy threshold")
	// ErrRunSelfCertificationForbidden is the run-level SoD rule: a
	// principal who matched (or confirmed) any MATCHED line in the
	// frozen population cannot also certify the run — mirrors the
	// line-level ConfirmMatch self-confirmation block, applied at the
	// run/certification level.
	ErrRunSelfCertificationForbidden = errorString("the certifying principal matched at least one line in this population and cannot also certify the run")

	// ErrPeriodLocked/ErrCloseServiceUnavailable back CertifyRun's Wave 13
	// closed-period check (internal/close's client) — certification must
	// fail closed on either: a CLOSED/LOCKED period, or an unreachable/
	// ambiguous financial-close-svc response.
	ErrPeriodLocked          = errorString("the accounting period for this run's statement date is closed or locked")
	ErrCloseServiceUnavailable = errorString("financial-close-svc unavailable")
)
