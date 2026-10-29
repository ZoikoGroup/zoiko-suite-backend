package domain

import (
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"strings"
	"time"

	"github.com/google/uuid"
)

type SourceSystem string
type DiscrepancyType string
type ResolutionRecommendation string
type SuggestionStatus string

const (
	SourceGeneralLedger  SourceSystem = "GENERAL_LEDGER"
	SourceBankStatement  SourceSystem = "BANK_STATEMENTS"
	SourceInvoices       SourceSystem = "INVOICES"
	SourcePayrollJournal SourceSystem = "PAYROLL_JOURNAL"
)

const (
	DiscrepancyAmountMismatch   DiscrepancyType = "AMOUNT_MISMATCH"
	DiscrepancyDateSkew         DiscrepancyType = "DATE_SKEW"
	DiscrepancyMissingReference DiscrepancyType = "MISSING_REFERENCE"
	DiscrepancyDuplicateEntry   DiscrepancyType = "DUPLICATE_ENTRY"
)

const (
	RecommendationAutoMatch        ResolutionRecommendation = "AUTO_MATCH"
	RecommendationWriteOff         ResolutionRecommendation = "WRITE_OFF"
	RecommendationTimingAdjustment ResolutionRecommendation = "TIMING_ADJUSTMENT"
	RecommendationManualReview     ResolutionRecommendation = "MANUAL_REVIEW"
	// RecommendationConfirmDuplicate asks a human to confirm that two
	// statement lines are one bank entry counted twice. Removing the copy
	// is done in bank-reconciliation-svc, by a human; never from here.
	RecommendationConfirmDuplicate ResolutionRecommendation = "CONFIRM_DUPLICATE"
)

const (
	StatusRecommended SuggestionStatus = "RECOMMENDED"
	StatusApproved    SuggestionStatus = "APPROVED"
	StatusRejected    SuggestionStatus = "REJECTED"
	StatusExecuted    SuggestionStatus = "EXECUTED"
)

// HeuristicRule names the fixed rule that produced a suggestion. It is the
// machine-readable "score basis" the data model requires: a reviewer can
// select and group by the rule that fired without parsing ResolutionNotes,
// and a value here is a named constant, not free text.
type HeuristicRule string

const (
	HeuristicMissingReferenceWriteOff     HeuristicRule = "MISSING_REFERENCE_WRITE_OFF"
	HeuristicMissingReferenceManualReview HeuristicRule = "MISSING_REFERENCE_MANUAL_REVIEW"
	HeuristicDuplicateExact               HeuristicRule = "DUPLICATE_EXACT"
	HeuristicDuplicateSuffixed            HeuristicRule = "DUPLICATE_SUFFIXED"
)

// GovernedTolerance is the financial-control-svc TolerancePolicy used for an
// analysis: the exact amount approved and the version it was approved under.
// Every suggestion records the version, so its score basis points at one
// retrievable policy rather than "whatever was current at the time".
type GovernedTolerance struct {
	Amount  *big.Rat
	Version int
}

// BankStatementLine is one UNMATCHED statement line as read from
// bank-reconciliation-svc, the authoritative owner of statement lines and
// their match state. This service never posts a line: it is fetched by run
// id (see internal/bankrec), so every suggestion is about a real record.
//
// Amount is an exact decimal string, never float64 — ZS-DATA-001 D06. See
// decimal.go.
type BankStatementLine struct {
	StatementLineID string
	LegalEntityID   string
	BankAccountID   string
	StatementDate   string // YYYY-MM-DD
	Amount          string
	CurrencyCode    string
	BankReference   string
	// GLCashAccountCode is the general-ledger-svc account that represents
	// this bank account — what a counterpart journal must post to (A2a).
	// "" only on a line bank-reconciliation-svc ingested before it recorded
	// one; such a line cannot be searched, or matched to a journal there.
	GLCashAccountCode string
}

// ReconciliationJob is one analysis of a bank-reconciliation run's
// UNMATCHED lines. Jobs analyzed before migration 000003 were built from
// caller-posted transactions; they carry no BankReconciliationRunID, and
// their MatchedCount / ReconciliationRate are what that analysis computed.
//
// A run-sourced job leaves MatchedCount and ReconciliationRate nil (omitted
// from JSON): every line it examines is UNMATCHED by definition, and match
// state is bank-reconciliation-svc's to report, so the honest values here
// are "not measured", not 0 and 0%.
type ReconciliationJob struct {
	ID                      string                     `json:"id"`
	TenantID                string                     `json:"tenant_id"`
	LegalEntityID           string                     `json:"legal_entity_id"`
	JobName                 string                     `json:"job_name"`
	BankReconciliationRunID string                     `json:"bank_reconciliation_run_id,omitempty"`
	SourceSystemA           SourceSystem               `json:"source_system_a"`
	SourceSystemB           SourceSystem               `json:"source_system_b,omitempty"`
	TotalProcessedCount     int                        `json:"total_processed_count"`
	MatchedCount            *int                       `json:"matched_count,omitempty"`
	UnmatchedCount          int                        `json:"unmatched_count"`
	ReconciliationRate      *float64                   `json:"reconciliation_rate,omitempty"` // 0.00 to 100.00%; legacy jobs only
	Status                  string                     `json:"status"`                        // COMPLETED, ARCHIVED
	AnalyzedAt              time.Time                  `json:"analyzed_at"`
	CreatedAt               time.Time                  `json:"created_at"`
	UnmatchedItems          []ReconciliationSuggestion `json:"unmatched_items,omitempty"`
}

// ReconciliationSuggestion is one advisory suggestion about an UNMATCHED
// bank-reconciliation statement line. The name and fields follow the
// canonical data model's ReconciliationSuggestion entity (04-data-model.md
// §16): reconciliation_suggestion_id is ID, source_record_a/b are the bank
// references, suggestion_status is the human decision state, and
// legal_entity_id is on the suggestion itself, not only on its job.
//
// For one release the JSON also carries the pre-rename names
// transaction_ref_a/b and resolution_status (see MarshalJSON). Callers can
// migrate without a hard break.
type ReconciliationSuggestion struct {
	ID            string `json:"id,omitempty"`
	TenantID      string `json:"tenant_id"`
	LegalEntityID string `json:"legal_entity_id"`
	JobID         string `json:"job_id"`
	// BankReconciliationRunID and StatementLineID point at the authoritative
	// record this suggestion is about in bank-reconciliation-svc. Empty only
	// on items from jobs analyzed before migration 000003.
	BankReconciliationRunID string `json:"bank_reconciliation_run_id,omitempty"`
	StatementLineID         string `json:"statement_line_id,omitempty"`
	// CounterpartStatementLineID is the second record a two-record
	// suggestion is about: on DUPLICATE_ENTRY, the line StatementLineID
	// probably duplicates. Empty on single-line suggestions.
	CounterpartStatementLineID string `json:"counterpart_statement_line_id,omitempty"`
	// CounterpartJournalID is the general-ledger-svc journal proposed as
	// this line's book side on an AUTO_MATCH or DATE_SKEW suggestion (A2a).
	// Proposed only: the match is made in bank-reconciliation-svc, by humans.
	CounterpartJournalID string `json:"counterpart_journal_id,omitempty"`
	// CandidateSearchOutcome is what the search for a counterpart found, on
	// every searched line; empty on a DUPLICATE_ENTRY copy (not searched)
	// and on rows written before migration 000008.
	CandidateSearchOutcome CandidateSearchOutcome `json:"candidate_search_outcome,omitempty"`
	// DateToleranceVersion is the governed candidate date tolerance policy
	// version the search used; zero exactly when CandidateSearchOutcome is
	// empty.
	DateToleranceVersion int             `json:"date_tolerance_version,omitempty"`
	SourceRecordA        string          `json:"source_record_a"`
	SourceRecordB        string          `json:"source_record_b,omitempty"`
	AmountA              string          `json:"amount_a"`
	AmountB              string          `json:"amount_b"`
	DiscrepancyAmount    string          `json:"discrepancy_amount"`
	DiscrepancyType      DiscrepancyType `json:"discrepancy_type"`
	// ConfidenceScore is NOT statistically or ML-derived — it is a fixed
	// label produced by AnalyzeUnmatchedStatementLines' hardcoded threshold
	// rules. HeuristicRule names the exact rule that fired, so the score has
	// a machine-readable basis instead of being an unexplained number.
	ConfidenceScore float64 `json:"confidence_score"`
	// HeuristicRule is the named rule that produced this suggestion. Empty
	// only on rows written before migration 000005.
	HeuristicRule HeuristicRule `json:"heuristic_rule,omitempty"`
	// ToleranceVersion is the version of the governed missing-reference
	// tolerance policy in force for the analysis. Duplicate detection does
	// not compare amounts against the tolerance, but the version still
	// identifies the policy under which the analysis ran. Zero only on rows
	// written before migration 000005.
	ToleranceVersion int                      `json:"tolerance_version,omitempty"`
	Recommendation   ResolutionRecommendation `json:"recommendation"`
	SuggestionStatus SuggestionStatus         `json:"suggestion_status"`
	ResolutionNotes  string                   `json:"resolution_notes,omitempty"`
	// AuthoritativeRef is the owning domain's record that carried out an
	// EXECUTED suggestion (A5). Set only on EXECUTED, and always on an
	// EXECUTED WRITE_OFF or TIMING_ADJUSTMENT executed after migration 000007.
	AuthoritativeRef *AuthoritativeRef `json:"authoritative_ref,omitempty"`
	CreatedAt        time.Time         `json:"created_at"`
	UpdatedAt        time.Time         `json:"updated_at"`
}

// MarshalJSON emits the canonical field names and, for one release, the
// pre-rename aliases. A consumer still reading resolution_status or
// transaction_ref_a/b keeps working; new consumers read suggestion_status
// and source_record_a/b. The aliases are derived from the same fields, so
// they cannot disagree. Remove them after one release (A9/B5).
func (s ReconciliationSuggestion) MarshalJSON() ([]byte, error) {
	type alias ReconciliationSuggestion
	return json.Marshal(struct {
		alias
		LegacyTransactionRefA  string           `json:"transaction_ref_a"`
		LegacyTransactionRefB  string           `json:"transaction_ref_b,omitempty"`
		LegacyResolutionStatus SuggestionStatus `json:"resolution_status"`
	}{
		alias:                  alias(s),
		LegacyTransactionRefA:  s.SourceRecordA,
		LegacyTransactionRefB:  s.SourceRecordB,
		LegacyResolutionStatus: s.SuggestionStatus,
	})
}

// AnalyzeReconciliationRequest names the bank-reconciliation run whose
// UNMATCHED statement lines are to be analyzed. The lines themselves are
// read from bank-reconciliation-svc, never accepted from the caller.
type AnalyzeReconciliationRequest struct {
	LegalEntityID           string `json:"legal_entity_id"`
	JobName                 string `json:"job_name"`
	BankReconciliationRunID string `json:"bank_reconciliation_run_id"`
}

type ApplyResolutionRequest struct {
	SuggestionStatus SuggestionStatus `json:"suggestion_status"` // APPROVED, REJECTED, EXECUTED
	ResolutionNotes  string           `json:"resolution_notes"`
	// AuthoritativeRef accompanies EXECUTED only; see ValidateExecutionRef
	// for when it is required.
	AuthoritativeRef *AuthoritativeRef `json:"authoritative_ref,omitempty"`
}

// UnmarshalJSON accepts the canonical suggestion_status and, for one release,
// the pre-rename resolution_status. A caller sending both names with
// different values is refused rather than having one silently chosen.
func (r *ApplyResolutionRequest) UnmarshalJSON(data []byte) error {
	var wire struct {
		SuggestionStatus SuggestionStatus  `json:"suggestion_status"`
		ResolutionStatus SuggestionStatus  `json:"resolution_status"`
		ResolutionNotes  string            `json:"resolution_notes"`
		AuthoritativeRef *AuthoritativeRef `json:"authoritative_ref"`
	}
	if err := json.Unmarshal(data, &wire); err != nil {
		return err
	}
	if wire.SuggestionStatus != "" && wire.ResolutionStatus != "" && wire.SuggestionStatus != wire.ResolutionStatus {
		return fmt.Errorf("suggestion_status and legacy resolution_status disagree")
	}
	r.SuggestionStatus = wire.SuggestionStatus
	if r.SuggestionStatus == "" {
		r.SuggestionStatus = wire.ResolutionStatus
	}
	r.ResolutionNotes = wire.ResolutionNotes
	r.AuthoritativeRef = wire.AuthoritativeRef
	return nil
}

var (
	ErrInvalidSuggestionStatus = errors.New("suggestion_status must be APPROVED, REJECTED or EXECUTED")
	ErrResolutionTransition    = errors.New("suggestion status transition not allowed")
)

// resolutionEdges is the human decision path over a recommendation. A decision
// is never silently overwritten: REJECTED and EXECUTED are final, and EXECUTED
// (the owning domain acted on the recommendation) is reachable only from an
// explicit APPROVED, never straight from the machine's RECOMMENDED.
var resolutionEdges = map[SuggestionStatus][]SuggestionStatus{
	StatusRecommended: {StatusApproved, StatusRejected},
	StatusApproved:    {StatusExecuted},
}

// ValidateResolutionTarget rejects a requested status a caller may not set.
func ValidateResolutionTarget(to SuggestionStatus) error {
	switch to {
	case StatusApproved, StatusRejected, StatusExecuted:
		return nil
	}
	return fmt.Errorf("%w: got %q", ErrInvalidSuggestionStatus, to)
}

// ValidateResolutionTransition checks a decision against the item's current status.
func ValidateResolutionTransition(from, to SuggestionStatus) error {
	if err := ValidateResolutionTarget(to); err != nil {
		return err
	}
	for _, allowed := range resolutionEdges[from] {
		if allowed == to {
			return nil
		}
	}
	return fmt.Errorf("%w: %s -> %s", ErrResolutionTransition, from, to)
}

func (r *AnalyzeReconciliationRequest) Validate() error {
	if r.LegalEntityID == "" {
		return fmt.Errorf("legal_entity_id is required")
	}
	if r.JobName == "" {
		return fmt.Errorf("job_name is required")
	}
	if r.BankReconciliationRunID == "" {
		return fmt.Errorf("bank_reconciliation_run_id is required")
	}
	// bank-reconciliation-svc's run_id is a UUID. Checked here so a malformed
	// id is the caller's 400, not a refusal relayed back from that service.
	if _, err := uuid.Parse(r.BankReconciliationRunID); err != nil {
		return fmt.Errorf("bank_reconciliation_run_id must be a UUID")
	}
	return nil
}

// Confidence-score labels for the recommendations AnalyzeUnmatchedStatementLines
// produces, paired one-for-one with the HeuristicRule constants. These are NOT
// a governed policy and carry no monetary meaning — they are fixed descriptive
// labels telling a human reviewer which fixed heuristic rule fired (see
// ConfidenceScore's own field comment). The
// monetary tolerance the heuristic compares against is not hardcoded: it is
// fetched from financial-control-svc's governed, versioned TolerancePolicy
// registry (ZS-SVC-Z-001 INV-09 — tolerance must be "explicit and
// versioned"). There is no package-level default; a caller with no
// configured policy must refuse rather than guess.
const (
	missingReferenceWriteOffConfidence     = 90.0
	missingReferenceManualReviewConfidence = 60.0

	// Duplicate-entry labels (duplicates.go): identical references are
	// stronger evidence than a reference related by a sequence suffix.
	duplicateExactReferenceConfidence    = 85.0
	duplicateSuffixedReferenceConfidence = 70.0
)

// MetricMissingReferenceTolerance is the metric name under which the
// small-amount write-off tolerance is expected to exist as a
// financial-control-svc TolerancePolicy row, scoped per (tenant,
// legal_entity).
//
// The amount-mismatch tolerance (RECONCILIATION_AMOUNT_MISMATCH_WRITEOFF)
// is no longer fetched: an amount mismatch needs a counterpart record, and
// bank-reconciliation-svc's UNMATCHED lines have none. Proposing a
// counterpart is A2a in SPEC_DEVIATIONS.md.
const MetricMissingReferenceTolerance = "RECONCILIATION_MISSING_REFERENCE_WRITEOFF"

// AnalyzeUnmatchedStatementLines produces one advisory suggestion per
// UNMATCHED statement line of a bank-reconciliation run.
//
// The population is single-sided: bank-reconciliation-svc already ran the
// match, and these are the lines it could not match. There is no "side B"
// to walk, so every line is examined exactly once and every line yields a
// suggestion — none is skipped and none is counted as matched here.
//
// Two rules, in order. First, lines that are probably the same bank entry
// recorded twice are found (duplicates.go); each copy gets a
// DUPLICATE_ENTRY suggestion naming the line it duplicates, and nothing
// else. Then every other line — including the entry each copy duplicates —
// gets the missing-reference suggestion below.
//
// "Intelligent" means fixed rules and one threshold comparison against a governed,
// versioned tolerance the caller fetched from financial-control-svc
// (internal/financialcontrol) — never a model. Every item carries
// SuggestionStatus RECOMMENDED: domain.StatusApproved/Rejected/Executed are
// only reached through Handler.ApplyResolution, by an authenticated
// principal holding RECONCILIATION_APPLY_RESOLUTION. Nothing here writes
// anything off or matches anything, in this service or in
// bank-reconciliation-svc (ZS-CONTROL-001 §2: AI may classify/prioritize; it
// cannot certify, waive, post or clear). A real match is still established
// through bank-reconciliation-svc's own propose/confirm flow, by a human.
//
// missingReferenceTolerance is the current MetricMissingReferenceTolerance for
// the run's legal entity, including the version that was actually used. All
// money is compared exactly as big.Rat (ZS-DATA-001 D06).
//
// search is the counterpart search (A2a, candidates.go). Every line that is
// not a duplicate copy is searched first: a single qualifying journal becomes
// an AUTO_MATCH (within the governed date tolerance) or DATE_SKEW (beyond it)
// proposal *instead of* the missing-reference suggestion, so every line is
// still the subject of exactly one suggestion. Any other result — several
// qualifying journals, none, no cash account — is recorded on the
// missing-reference suggestion, and several candidates force MANUAL_REVIEW:
// a line with book-side candidates is never recommended for write-off.
func AnalyzeUnmatchedStatementLines(lines []BankStatementLine, runID, tenantID string, missingReferenceTolerance *GovernedTolerance, search *CandidateSearch) ([]ReconciliationSuggestion, error) {
	if missingReferenceTolerance == nil || missingReferenceTolerance.Amount == nil {
		return nil, fmt.Errorf("the governed missing-reference tolerance is required")
	}
	if missingReferenceTolerance.Version <= 0 {
		return nil, fmt.Errorf("the governed missing-reference tolerance version is required")
	}
	if search == nil || search.Tolerance.Version <= 0 || search.Tolerance.Days < 0 || search.HorizonDays < search.Tolerance.Days {
		return nil, fmt.Errorf("a governed candidate date tolerance and a search horizon at least as wide are required")
	}
	tol := CanonicalAmount(missingReferenceTolerance.Amount)

	amounts := make([]*big.Rat, len(lines))
	for i, line := range lines {
		amount, err := ParseStorableAmount(line.Amount)
		if err != nil {
			return nil, fmt.Errorf("statement line %s: %w", line.StatementLineID, err)
		}
		amounts[i] = amount
	}

	// Duplicate detection runs first and is exclusive for the duplicate: a
	// line identified as a copy of another gets ONE suggestion,
	// DUPLICATE_ENTRY, and not also a write-off or review recommendation.
	// Writing off or investigating a copy is the wrong action, and two
	// suggestions about the same line could contradict each other. The
	// anchor — the entry that stays — still goes through the rules below:
	// once the copy is removed it is one real entry that is still UNMATCHED.
	// So every line is still the subject of exactly one suggestion.
	dupOf, dupsOf := findDuplicates(lines, amounts)

	// A copy is not searched: the entry it duplicates is, once.
	searched := make(map[int]bool, len(lines))
	for i := range lines {
		if _, isDup := dupOf[i]; !isDup {
			searched[i] = true
		}
	}
	found, err := searchCandidates(lines, amounts, searched, tenantID, search)
	if err != nil {
		return nil, err
	}

	items := make([]ReconciliationSuggestion, 0, len(lines))
	for i, line := range lines {
		amount := amounts[i]
		now := time.Now()

		if d, isDup := dupOf[i]; isDup {
			items = append(items, duplicateItem(line, lines[d.anchor], amount, amounts[d.anchor], d.exact, runID, tenantID, missingReferenceTolerance.Version, now))
			continue
		}

		dupNote := ""
		if dups := dupsOf[i]; len(dups) > 0 {
			ids := make([]string, len(dups))
			for k, j := range dups {
				ids[k] = lines[j].StatementLineID
			}
			dupNote = fmt.Sprintf("; judged as one entry — %d probable duplicate(s) of it in this run (%s) carry DUPLICATE_ENTRY suggestions instead",
				len(dups), strings.Join(ids, ", "))
		}

		ls := found[i]
		if ls.proposed != nil {
			item := candidateItem(line, amount, ls.outcome, *ls.proposed, runID, tenantID, missingReferenceTolerance.Version, search, now)
			item.ResolutionNotes += dupNote
			items = append(items, item)
			continue
		}

		// The tolerance is a magnitude: a large credit (negative amount) is
		// as material as a large debit and must never qualify as a "small
		// amount" write-off.
		magnitude := new(big.Rat).Abs(amount)
		mag := CanonicalAmount(magnitude)

		confScore := missingReferenceManualReviewConfidence
		rule := HeuristicMissingReferenceManualReview
		rec := RecommendationManualReview
		rationale := fmt.Sprintf(
			"heuristic rule: statement line %s is UNMATCHED in bank-reconciliation run %s; amount magnitude %s is at or above the %s governed small-amount tolerance, so manual review is recommended rather than write-off",
			line.StatementLineID, runID, mag, tol)
		if magnitude.Cmp(missingReferenceTolerance.Amount) < 0 {
			rec = RecommendationWriteOff
			confScore = missingReferenceWriteOffConfidence
			rule = HeuristicMissingReferenceWriteOff
			rationale = fmt.Sprintf(
				"heuristic rule: statement line %s is UNMATCHED in bank-reconciliation run %s; amount magnitude %s is below the %s governed small-amount write-off tolerance",
				line.StatementLineID, runID, mag, tol)
		}
		if ls.outcome == CandidateAmbiguous {
			// Book-side candidates exist, so this is never a write-off.
			rec = RecommendationManualReview
			confScore = candidateAmbiguousConfidence
			rule = HeuristicCandidateAmbiguous
		}
		rationale += dupNote + candidateNote(ls, line, search)

		items = append(items, ReconciliationSuggestion{
			TenantID:                tenantID,
			LegalEntityID:           line.LegalEntityID,
			BankReconciliationRunID: runID,
			StatementLineID:         line.StatementLineID,
			SourceRecordA:           line.BankReference,
			AmountA:                 CanonicalAmount(amount),
			AmountB:                 "0",
			DiscrepancyAmount:       mag,
			DiscrepancyType:         DiscrepancyMissingReference,
			ConfidenceScore:         confScore,
			HeuristicRule:           rule,
			ToleranceVersion:        missingReferenceTolerance.Version,
			CandidateSearchOutcome:  ls.outcome,
			DateToleranceVersion:    search.Tolerance.Version,
			Recommendation:          rec,
			SuggestionStatus:        StatusRecommended,
			ResolutionNotes:         rationale,
			CreatedAt:               now,
			UpdatedAt:               now,
		})
	}
	return items, nil
}

// duplicateItem is the DUPLICATE_ENTRY suggestion that dup is probably the
// same bank entry as anchor. It names both lines; the amounts are equal by
// the detection rule, so the discrepancy is 0.
func duplicateItem(dup, anchor BankStatementLine, dupAmount, anchorAmount *big.Rat, exact bool, runID, tenantID string, toleranceVersion int, now time.Time) ReconciliationSuggestion {
	confScore := duplicateSuffixedReferenceConfidence
	rule := HeuristicDuplicateSuffixed
	refRule := fmt.Sprintf("bank reference %q is %q plus a trailing sequence suffix", dup.BankReference, anchor.BankReference)
	if exact {
		confScore = duplicateExactReferenceConfidence
		rule = HeuristicDuplicateExact
		refRule = fmt.Sprintf("bank references %q and %q are identical ignoring case and whitespace", dup.BankReference, anchor.BankReference)
	}
	rationale := fmt.Sprintf(
		"heuristic rule: statement line %s probably duplicates statement line %s in bank-reconciliation run %s — same bank account and statement date (the run's scope), same amount %s %s, and %s. If confirmed it is one bank entry counted twice: this line is not written off or reviewed on its own; line %s carries the suggestion for the entry itself",
		dup.StatementLineID, anchor.StatementLineID, runID, CanonicalAmount(dupAmount), dup.CurrencyCode, refRule, anchor.StatementLineID)

	return ReconciliationSuggestion{
		TenantID:                   tenantID,
		LegalEntityID:              dup.LegalEntityID,
		BankReconciliationRunID:    runID,
		StatementLineID:            dup.StatementLineID,
		CounterpartStatementLineID: anchor.StatementLineID,
		SourceRecordA:              dup.BankReference,
		SourceRecordB:              anchor.BankReference,
		AmountA:                    CanonicalAmount(dupAmount),
		AmountB:                    CanonicalAmount(anchorAmount),
		DiscrepancyAmount:          "0",
		DiscrepancyType:            DiscrepancyDuplicateEntry,
		ConfidenceScore:            confScore,
		HeuristicRule:              rule,
		ToleranceVersion:           toleranceVersion,
		Recommendation:             RecommendationConfirmDuplicate,
		SuggestionStatus:           StatusRecommended,
		ResolutionNotes:            rationale,
		CreatedAt:                  now,
		UpdatedAt:                  now,
	}
}
