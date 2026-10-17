// Package domain defines the authoritative domain types for
// reconciliation-engine-svc (DATA-07, ZS-SVC-N-001 §4). This service
// compares two or more frozen populations under explicit, versioned
// match/tolerance/materiality rules, raises exceptions for whatever
// doesn't reconcile, and seals an immutable certification once every
// exception is resolved. It never edits ledger/subledger/source
// records and never invents corrective journals — remediation always
// happens back in the owning domain.
package domain

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

const (
	PrefixDefinition    = "rdf_"
	PrefixRun           = "rcr_"
	PrefixSnapshot      = "rps_"
	PrefixItem          = "rpi_"
	PrefixMatchResult   = "rmr_"
	PrefixException     = "rex_"
	PrefixCertification = "rct_"
)

type errorString string

func (e errorString) Error() string { return string(e) }

type IdempotentReplayError struct {
	ResourceID string
}

func (e *IdempotentReplayError) Error() string {
	return fmt.Sprintf("idempotent replay: resource %s already exists", e.ResourceID)
}

type IdempotencyClaim struct {
	OwnerScope    string
	PrincipalID   string
	Key           string
	Operation     string
	RequestSHA256 string
	ResourceID    string
}

const SellerScope = "seller"

// ── ReconciliationDefinition ────────────────────────────────────────────────

// ReconciliationDefinition is a reconciliation's stable identity — tenant,
// legal entity and name — and the rule set currently in force for it.
//
// Rules live in immutable rule versions (rules.go, A11): a version is created
// as a draft, approved by someone other than its creator, and governs runs
// from its effective date. The rule fields below are the EFFECTIVE version's —
// the latest approved version whose effective date has arrived — and Version
// is its number; with no effective version they are empty and Version is 0,
// and the definition cannot start a run. A run copies the effective version's
// rules onto itself, so a later version never reaches back into it.
//
// LegalEntityID is the entity the definition and every run of it belong to;
// authorization is evaluated against it (A10). It is fixed for the life of
// the definition.
type ReconciliationDefinition struct {
	DefinitionID      string           `json:"definition_id"`
	TenantID          string           `json:"tenant_id"`
	LegalEntityID     string           `json:"legal_entity_id"`
	Name              string           `json:"name"`
	Populations       []PopulationSpec `json:"populations,omitempty"`
	MatchKeyFields    []string         `json:"match_key_fields,omitempty"`
	BalanceDimensions []string         `json:"balance_dimensions,omitempty"`
	AmountScale       int              `json:"amount_scale"`
	Tolerance         TolerancePolicy  `json:"tolerance"`
	// Materiality, ExceptionPolicy and CertificationProfile are nil when no
	// rule version is effective.
	Materiality          *MaterialityPolicy    `json:"materiality,omitempty"`
	ExceptionPolicy      *ExceptionPolicy      `json:"exception_policy,omitempty"`
	CertificationProfile *CertificationProfile `json:"certification_profile,omitempty"`
	// Version is the effective rule version (0: none). RulesSHA256 and
	// EffectiveFrom are that version's; LatestRuleVersion is the newest version
	// in any state, which a new version must be based on.
	Version           int64     `json:"version"`
	RulesSHA256       string    `json:"rules_sha256,omitempty"`
	EffectiveFrom     string    `json:"effective_from,omitempty"`
	LatestRuleVersion int64     `json:"latest_rule_version"`
	CreatedAt         time.Time `json:"created_at"`
	CreatedBy         string    `json:"created_by"`
	UpdatedAt         time.Time `json:"updated_at"`
}

// Legacy reports a definition created before migration 000004: it has no
// legal entity and no population sources, so it can be neither authorized
// against an entity nor run.
func (d ReconciliationDefinition) Legacy() bool { return d.LegalEntityID == "" }

// Runnable reports why the definition cannot start a run, if it cannot.
func (d ReconciliationDefinition) Runnable() error {
	if d.Legacy() {
		return ErrLegacyRecord
	}
	if d.Version == 0 || d.Materiality == nil || d.ExceptionPolicy == nil || d.CertificationProfile == nil {
		return ErrNoEffectiveRules
	}
	return nil
}

// RuleSet returns the effective rules as a rule set (zero if none).
func (d ReconciliationDefinition) RuleSet() RuleSet {
	r := RuleSet{Populations: d.Populations, MatchKeyFields: d.MatchKeyFields, BalanceDimensions: d.BalanceDimensions,
		AmountScale: d.AmountScale, Tolerance: d.Tolerance}
	if d.Materiality != nil {
		r.Materiality = *d.Materiality
	}
	if d.ExceptionPolicy != nil {
		r.ExceptionPolicy = *d.ExceptionPolicy
	}
	if d.CertificationProfile != nil {
		r.CertificationProfile = *d.CertificationProfile
	}
	return r
}

// DefineReconciliationRequest creates a definition and its first rule version,
// as a draft awaiting approval. It never changes an existing definition: a
// name already defined in the entity is refused (ErrDefinitionExists); new
// rules for it are a new rule version.
type DefineReconciliationRequest struct {
	LegalEntityID string `json:"legal_entity_id"`
	Name          string `json:"name"`
	RulesInput
	EffectiveFrom string `json:"effective_from,omitempty"`
}

func (r DefineReconciliationRequest) Validate() error {
	if !ValidUUID(r.LegalEntityID) {
		return fmt.Errorf("%w: legal_entity_id must be a UUID", ErrInvalidArgument)
	}
	if strings.TrimSpace(r.Name) == "" || len(r.Name) > 256 {
		return fmt.Errorf("%w: name is required (at most 256 characters)", ErrInvalidArgument)
	}
	if err := ValidateEffectiveFrom(r.EffectiveFrom); err != nil {
		return err
	}
	_, err := r.RulesInput.RuleSet()
	return err
}

// ── ReconciliationRun / PopulationSnapshot ──────────────────────────────────

type RunStatus string

const (
	RunPlanned        RunStatus = "Planned"
	RunRunning        RunStatus = "Running"
	RunExceptionsOpen RunStatus = "ExceptionsOpen"
	RunReperformed    RunStatus = "Reperformed"
	RunCertified      RunStatus = "Certified"
	RunFailed         RunStatus = "Failed"
	RunSuperseded     RunStatus = "Superseded"
)

// ReconciliationRun freezes the definition's rules onto itself at StartRun
// time — the tolerance policy, match key, balance dimensions, materiality and
// definition version never change after that (B16). Status is the lifecycle;
// ResultState and CertificationState are the other two state dimensions
// (state.go). A Failed run carries why it could not determine a result.
type ReconciliationRun struct {
	RunID             string `json:"run_id"`
	TenantID          string `json:"tenant_id"`
	LegalEntityID     string `json:"legal_entity_id"`
	DefinitionID      string `json:"definition_id"`
	DefinitionVersion int64  `json:"definition_version"`
	// RulesSHA256 is the digest of the rule version the run pinned (A11);
	// empty on runs started before migration 000008.
	RulesSHA256          string                `json:"rules_sha256,omitempty"`
	Status               RunStatus             `json:"status"`
	ResultState          ResultState           `json:"result_state"`
	CertificationState   CertificationState    `json:"certification_state"`
	FailureCode          *FailureCode          `json:"failure_code,omitempty"`
	FailureReason        *string               `json:"failure_reason,omitempty"`
	PeriodID             string                `json:"period_id,omitempty"`
	Scope                map[string]string     `json:"scope,omitempty"`
	AmountScale          int                   `json:"amount_scale"`
	MatchKeyFields       []string              `json:"match_key_fields,omitempty"`
	BalanceDimensions    []string              `json:"balance_dimensions,omitempty"`
	Tolerance            *TolerancePolicy      `json:"tolerance,omitempty"`
	Materiality          *MaterialityPolicy    `json:"materiality,omitempty"`
	ExceptionPolicy      *ExceptionPolicy      `json:"exception_policy,omitempty"`
	CertificationProfile *CertificationProfile `json:"certification_profile,omitempty"`
	// Assessment is derived on read by GetRun (never stored): what the result
	// rests on, including every accepted risk.
	Assessment        *RunAssessment `json:"assessment,omitempty"`
	SupersedesRunID   *string        `json:"supersedes_run_id,omitempty"`
	SupersededByRunID *string        `json:"superseded_by_run_id,omitempty"`
	SupersededReason  *string        `json:"superseded_reason,omitempty"`
	SupersededBy      *string        `json:"superseded_by,omitempty"`
	SupersededAt      *time.Time     `json:"superseded_at,omitempty"`
	StartedAt         time.Time      `json:"started_at"`
	StartedBy         string         `json:"started_by"`
	CertifiedAt       *time.Time     `json:"certified_at,omitempty"`
	CertifiedBy       string         `json:"certified_by,omitempty"`
}

// PopulationSnapshot is one frozen side of a run's comparison (e.g.
// "Ledger" vs "Bank"), read from its owning service through the control
// population contract. Immutable from the moment StartRun creates it (or a
// reperformance freezes the next generation of it). It pins what
// ZS-CONTROL-001 names for a snapshot (side, source, query/spec, watermark,
// row count, per-currency control totals, hash) plus the source's own declared
// totals it was verified against. Generation 1 is the run's initial freeze;
// each reperformance adds the next generation and leaves the earlier ones
// standing (A8). The optional fields are empty only on snapshots frozen before
// migration 000004.
type PopulationSnapshot struct {
	SnapshotID      string `json:"snapshot_id"`
	TenantID        string `json:"tenant_id"`
	RunID           string `json:"run_id"`
	Side            string `json:"side"`
	Generation      int    `json:"generation"`
	SourceSystem    string `json:"source_system"`
	SpecRef         string `json:"spec_ref,omitempty"`
	SourceWatermark string `json:"source_watermark,omitempty"`
	ItemCount       int64  `json:"item_count"`
	AmountScale     *int   `json:"amount_scale,omitempty"`
	// TotalAmountMinorUnits is set only on snapshots frozen before migration
	// 000005, where it added every currency together. Use ControlTotals.
	TotalAmountMinorUnits *int64            `json:"total_amount_minor_units,omitempty"`
	ControlTotals         []CurrencyTotal   `json:"control_totals,omitempty"`
	DeclaredRowCount      *int64            `json:"declared_row_count,omitempty"`
	DeclaredTotals        map[string]string `json:"declared_totals,omitempty"`
	ContentHash           string            `json:"content_hash"`
	CreatedAt             time.Time         `json:"created_at"`
}

type PopulationItem struct {
	ItemID           string            `json:"item_id"`
	SnapshotID       string            `json:"snapshot_id"`
	SourceRecordID   string            `json:"source_record_id,omitempty"`
	RefID            string            `json:"ref_id"`
	AmountMinorUnits int64             `json:"amount_minor_units"`
	Currency         string            `json:"currency,omitempty"`
	OccurredAt       time.Time         `json:"occurred_at"`
	Dimensions       map[string]string `json:"dimensions,omitempty"`
}

// PopulationItemInput is one item to freeze, converted from a source record.
type PopulationItemInput struct {
	SourceRecordID   string
	RefID            string
	AmountMinorUnits int64
	Currency         string
	OccurredAt       time.Time
	Dimensions       map[string]string
}

// StartRunRequest names the definition to run and the run's own scope. The
// populations are NOT in the request: they are read from the sources the
// definition names, for the definition's legal entity and this period (A1).
type StartRunRequest struct {
	DefinitionID string            `json:"definition_id"`
	PeriodID     string            `json:"period_id,omitempty"`
	Scope        map[string]string `json:"scope,omitempty"`
}

func (r StartRunRequest) Validate() error {
	if r.DefinitionID == "" {
		return fmt.Errorf("definition_id is required")
	}
	if r.PeriodID != "" && !periodRe.MatchString(r.PeriodID) {
		return fmt.Errorf("%w: period_id must be YYYY-MM", ErrInvalidArgument)
	}
	if err := ValidateParams(r.Scope); err != nil {
		return fmt.Errorf("scope: %w", err)
	}
	return nil
}

// ── MatchResult ──────────────────────────────────────────────────────────────

// MatchResult is one matched pair — append-only, never edited. A
// Manual match always carries a non-empty Reason and the authorizing
// principal (MatchedBy) — this is what makes a manual match auditable,
// per the doc's own named acceptance test.
type MatchResult struct {
	MatchResultID string    `json:"match_result_id"`
	TenantID      string    `json:"tenant_id"`
	RunID         string    `json:"run_id"`
	ItemAID       string    `json:"item_a_id"`
	ItemBID       string    `json:"item_b_id"`
	RefID         string    `json:"ref_id"`
	Manual        bool      `json:"manual"`
	Reason        string    `json:"reason,omitempty"`
	MatchedBy     string    `json:"matched_by"`
	MatchedAt     time.Time `json:"matched_at"`
}

type ManualMatchInput struct {
	ItemAID string `json:"item_a_id"`
	ItemBID string `json:"item_b_id"`
	Reason  string `json:"reason"`
}

type MatchRequest struct {
	ManualMatches []ManualMatchInput `json:"manual_matches,omitempty"`
}

func (r MatchRequest) Validate() error {
	for _, m := range r.ManualMatches {
		if m.ItemAID == "" || m.ItemBID == "" {
			return fmt.Errorf("manual match requires item_a_id and item_b_id")
		}
		if m.Reason == "" {
			return fmt.Errorf("manual match requires a reason")
		}
	}
	return nil
}

// ValidateManualMatch checks a manual pairing against the run's frozen rules.
// The two items must come from different populations and must reconcile under
// the run's pinned tolerance policy exactly as an automatic pair would: same
// currency, amount within that currency's tolerance, dates within the date
// tolerance. A manual match overrides only the match KEY; it records a reason
// and an authority, but it can never hide an amount, currency or timing
// difference (ZS-CONTROL-001 §10, scenario 06 — "narrative alone cannot force
// pass"). A pair that does not reconcile stays an exception.
func ValidateManualMatch(a, b MatchItem, p TolerancePolicy) error {
	if a.Side == b.Side {
		return ErrManualMatchSameSide
	}
	if reason, _ := Compare(a, b, p); reason != "" {
		return fmt.Errorf("%w: %s", ErrManualMatchOutsideTolerance, reason)
	}
	return nil
}

// ── ReconciliationException ─────────────────────────────────────────────────

type ExceptionStatus string

type ExceptionKind string

const (
	ExceptionKindItem    ExceptionKind = "ITEM"
	ExceptionKindBalance ExceptionKind = "BALANCE"
)

// ReconciliationException is raised for any item or balance that didn't
// reconcile. Its lifecycle and governance fields are in governance.go: it is
// resolved (CLOSED) only as the side effect of a manual match consuming the
// item it was raised for — never a bare "resolve" with no evidence — and it
// can be accepted as risk under a named authority, which leaves it unresolved.
//
// Kind ITEM is about one frozen item (unmatched, outside tolerance, currency
// or timing mismatch, missing key). Kind BALANCE is about a per-currency side
// total — of the whole population (Dimension "") or of one value of a
// declared balance dimension — that does not balance; it has no item, and its
// amount is the signed difference side A minus side B. Detail carries the
// observed values and the threshold that was applied. The governance fields
// are nil only on exceptions raised before migration 000007.
type ReconciliationException struct {
	ExceptionID       string          `json:"exception_id"`
	TenantID          string          `json:"tenant_id"`
	RunID             string          `json:"run_id"`
	Kind              ExceptionKind   `json:"kind"`
	ItemID            string          `json:"item_id,omitempty"`
	Side              string          `json:"side,omitempty"`
	RefID             string          `json:"ref_id,omitempty"`
	Currency          string          `json:"currency,omitempty"`
	Dimension         *string         `json:"dimension,omitempty"`
	DimensionValue    *string         `json:"dimension_value,omitempty"`
	Reason            string          `json:"reason"`
	AmountMinorUnits  int64           `json:"amount_minor_units"`
	Detail            json.RawMessage `json:"detail,omitempty"`
	Status            ExceptionStatus `json:"status"`
	RaisedAt          time.Time       `json:"raised_at"`
	RaisedBy          string          `json:"raised_by"`
	ResolvedAt        *time.Time      `json:"resolved_at,omitempty"`
	ResolvedByMatchID *string         `json:"resolved_by_match_id,omitempty"`

	Category      *string           `json:"category,omitempty"`
	Assertion     *string           `json:"assertion,omitempty"`
	Severity      *Severity         `json:"severity,omitempty"`
	Materiality   *MaterialityClass `json:"materiality,omitempty"`
	ExposureMinor *int64            `json:"exposure_minor_units,omitempty"`
	OwnerRole     *string           `json:"owner_role,omitempty"`
	Owner         *string           `json:"owner_principal_id,omitempty"`
	DueAt         *time.Time        `json:"due_at,omitempty"`
	AuthorityRef  *string           `json:"authority_ref,omitempty"`
	EvidenceRef   *string           `json:"evidence_ref,omitempty"`
	RootCauseCode *string           `json:"root_cause_code,omitempty"`
	RootCauseNote *string           `json:"root_cause_note,omitempty"`
	AcceptedBy    *string           `json:"accepted_by,omitempty"`
	AcceptedAt    *time.Time        `json:"accepted_at,omitempty"`
	// Derived on read, never stored (§7 attention signals, §20 aging).
	AgeDays   *int     `json:"age_days,omitempty"`
	Attention []string `json:"attention,omitempty"`
}

// RaiseExceptionRequest raises an exception by hand. The raiser names what
// kind of failure it is (category) and which assertion it impacts — a human
// finding has no rule to derive them from; severity is still derived from the
// item's exposure against the run's materiality, never chosen.
type RaiseExceptionRequest struct {
	ItemID    string `json:"item_id"`
	Reason    string `json:"reason"`
	Category  string `json:"category"`
	Assertion string `json:"assertion"`
}

func (r RaiseExceptionRequest) Validate() error {
	if r.ItemID == "" {
		return fmt.Errorf("item_id is required")
	}
	if r.Reason == "" {
		return fmt.Errorf("reason is required")
	}
	if !exceptionCategories[r.Category] {
		return fmt.Errorf("%w: category must be one of ZS-CONTROL-001 §21's categories", ErrInvalidArgument)
	}
	if !exceptionAssertions[r.Assertion] {
		return fmt.Errorf("%w: assertion must be one of ZS-CONTROL-001 §21's assertions", ErrInvalidArgument)
	}
	return nil
}

// ── Certification ────────────────────────────────────────────────────────────

// Certification is the sealed, immutable evidence produced by Certify
// — the run's frozen rule version, its final match/exception counts
// and totals, hashed into one addressable manifest.
type Certification struct {
	CertificationID   string `json:"certification_id"`
	TenantID          string `json:"tenant_id"`
	RunID             string `json:"run_id"`
	DefinitionVersion int64  `json:"definition_version"`
	MatchedCount      int64  `json:"matched_count"`
	ExceptionCount    int64  `json:"exception_count"`
	// TotalAssertedMinor is set only on certifications sealed before migration
	// 000005, where it added both sides and every currency together.
	TotalAssertedMinor *int64                     `json:"total_asserted_minor_units,omitempty"`
	ControlTotals      map[string][]CurrencyTotal `json:"control_totals,omitempty"`
	// ResultState and Assessment are set on certifications sealed from
	// migration 000007 on: a certification over accepted risk says so — the
	// result stays VARIANCE and every accepted exception is listed.
	ResultState    *ResultState   `json:"result_state,omitempty"`
	Assessment     *RunAssessment `json:"assessment,omitempty"`
	ManifestSHA256 string         `json:"manifest_sha256"`
	SealedAt       time.Time      `json:"sealed_at"`
	SealedBy       string         `json:"sealed_by"`
}

type SupersedeRunRequest struct {
	NewRunID string `json:"new_run_id"`
	Reason   string `json:"reason"`
}

func (r SupersedeRunRequest) Validate() error {
	if r.NewRunID == "" {
		return fmt.Errorf("new_run_id is required")
	}
	if r.Reason == "" {
		return fmt.Errorf("reason is required")
	}
	return nil
}

var (
	ErrDefinitionNotFound     = errorString("reconciliation definition not found")
	ErrRunNotFound            = errorString("reconciliation run not found")
	ErrItemNotFound           = errorString("population item not found")
	ErrRunNotOpen             = errorString("reconciliation run is not open for this operation")
	ErrOpenExceptionsRemain   = errorString("open exceptions remain; resolve them before certifying")
	ErrRunImmutable           = errorString("reconciliation run is certified and immutable")
	ErrExceptionAlreadyClosed = errorString("exception is already resolved")
	ErrIdempotencyKeyReused   = errorString("idempotency key was already used for a different request")

	ErrManualMatchSameSide         = errorString("manual match must pair items from different populations")
	ErrManualMatchOutsideTolerance = errorString("manual match amounts differ by more than the run's tolerance")
	ErrSupersedeSelf               = errorString("a run cannot supersede itself")

	ErrInvalidArgument              = errorString("invalid argument")
	ErrAmountNotRepresentable       = errorString("amount is not exactly representable at the definition's amount_scale")
	ErrAmountOverflow               = errorString("amount or total exceeds the representable range")
	ErrDefinitionChanged            = errorString("the definition changed while its populations were being read; start the run again")
	ErrLegacyRecord                 = errorString("record predates legal-entity scoping (migration 000004) and cannot be authorized or run")
	ErrSupersedeDifferentDefinition = errorString("a replacement run must use the same definition as the run it supersedes")
	ErrReplacementNotTerminal       = errorString("a replacement run must be Certified or Failed before it can supersede another run")
	ErrReplacementAlreadyUsed       = errorString("the replacement run already supersedes another run")
	ErrInvalidTransition            = errorString("invalid state transition")
	ErrRunNotEvaluated              = errorString("the run has not been matched yet; match it before raising exceptions or certifying")
	ErrRunNotEligible               = errorString("the run is not eligible for certification")
	ErrExceptionNotFound            = errorString("reconciliation exception not found")
	ErrNotExceptionOwner            = errorString("only the exception's assigned owner can work it; assign it first")
	ErrNotIndependent               = errorString("risk must be accepted by someone other than the exception's owner and its raiser")
	ErrNotIndependentCertifier      = errorString("a run must be certified by someone other than whoever started, matched or reperformed it")
)
