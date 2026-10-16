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
	"fmt"
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

// ReconciliationDefinition is the versioned, seller-managed rule set a
// run is executed against — which two populations are compared (and where
// they are read from), match keys, the fixed-point amount scale, tolerance
// and materiality. A new StartRun copies the definition's CURRENT values
// onto the run itself (frozen there), so editing a definition never reaches
// back into a run already started against an earlier version.
//
// LegalEntityID is the entity the definition and every run of it belong to;
// authorization is evaluated against it (A10). It is fixed for the life of
// the definition.
type ReconciliationDefinition struct {
	DefinitionID                string           `json:"definition_id"`
	TenantID                    string           `json:"tenant_id"`
	LegalEntityID               string           `json:"legal_entity_id"`
	Name                        string           `json:"name"`
	Populations                 []PopulationSpec `json:"populations"`
	MatchKeyFields              []string         `json:"match_key_fields"`
	AmountScale                 int              `json:"amount_scale"`
	ToleranceAmountMinorUnits   int64            `json:"tolerance_amount_minor_units"`
	MaterialityAmountMinorUnits int64            `json:"materiality_amount_minor_units"`
	Version                     int64            `json:"version"`
	CreatedAt                   time.Time        `json:"created_at"`
	CreatedBy                   string           `json:"created_by"`
	UpdatedAt                   time.Time        `json:"updated_at"`
}

// Legacy reports a definition created before migration 000004: it has no
// legal entity and no population sources, so it can be neither authorized
// against an entity nor run.
func (d ReconciliationDefinition) Legacy() bool { return d.LegalEntityID == "" }

// DefineReconciliationRequest. Amounts (tolerance, materiality and every
// population amount) are integers in units of 10^-amount_scale.
// amount_scale is required, with no default, because the scale is part of
// what "balanced" means (ZS-SVC-Z-001 INV-09, TC-05).
type DefineReconciliationRequest struct {
	LegalEntityID               string           `json:"legal_entity_id"`
	Name                        string           `json:"name"`
	Populations                 []PopulationSpec `json:"populations"`
	MatchKeyFields              []string         `json:"match_key_fields"`
	AmountScale                 *int             `json:"amount_scale"`
	ToleranceAmountMinorUnits   int64            `json:"tolerance_amount_minor_units"`
	MaterialityAmountMinorUnits int64            `json:"materiality_amount_minor_units"`
}

func (r DefineReconciliationRequest) Validate() error {
	if !ValidUUID(r.LegalEntityID) {
		return fmt.Errorf("%w: legal_entity_id must be a UUID", ErrInvalidArgument)
	}
	if r.Name == "" {
		return fmt.Errorf("name is required")
	}
	if err := ValidatePopulationSpecs(r.Populations); err != nil {
		return err
	}
	if len(r.MatchKeyFields) == 0 {
		return fmt.Errorf("match_key_fields is required")
	}
	if r.AmountScale == nil {
		return fmt.Errorf("%w: amount_scale is required", ErrInvalidArgument)
	}
	if *r.AmountScale < 0 || *r.AmountScale > MaxAmountScale {
		return fmt.Errorf("%w: amount_scale must be between 0 and %d", ErrInvalidArgument, MaxAmountScale)
	}
	if r.ToleranceAmountMinorUnits < 0 {
		return fmt.Errorf("tolerance_amount_minor_units cannot be negative")
	}
	if r.MaterialityAmountMinorUnits < 0 {
		return fmt.Errorf("materiality_amount_minor_units cannot be negative")
	}
	return nil
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

// ReconciliationRun freezes the definition's rules onto itself at
// StartRun time — ToleranceAmountMinorUnits/MaterialityAmountMinorUnits/
// DefinitionVersion never change after that, and once Status reaches
// Certified the row becomes fully immutable (see the "tolerance cannot
// be widened during a certified run" acceptance test — enforced as a
// strict subset of "nothing about a certified run can change at all").
type ReconciliationRun struct {
	RunID                       string     `json:"run_id"`
	TenantID                    string     `json:"tenant_id"`
	LegalEntityID               string            `json:"legal_entity_id"`
	DefinitionID                string            `json:"definition_id"`
	DefinitionVersion           int64             `json:"definition_version"`
	Status                      RunStatus         `json:"status"`
	PeriodID                    string            `json:"period_id,omitempty"`
	Scope                       map[string]string `json:"scope,omitempty"`
	AmountScale                 int               `json:"amount_scale"`
	ToleranceAmountMinorUnits   int64             `json:"tolerance_amount_minor_units"`
	MaterialityAmountMinorUnits int64             `json:"materiality_amount_minor_units"`
	SupersedesRunID             *string           `json:"supersedes_run_id,omitempty"`
	SupersededByRunID           *string           `json:"superseded_by_run_id,omitempty"`
	SupersededReason            *string           `json:"superseded_reason,omitempty"`
	SupersededBy                *string           `json:"superseded_by,omitempty"`
	SupersededAt                *time.Time        `json:"superseded_at,omitempty"`
	StartedAt                   time.Time  `json:"started_at"`
	StartedBy                   string     `json:"started_by"`
	CertifiedAt                 *time.Time `json:"certified_at,omitempty"`
	CertifiedBy                 string     `json:"certified_by,omitempty"`
}

// PopulationSnapshot is one frozen side of a run's comparison (e.g.
// "Ledger" vs "Bank"), read from its owning service through the control
// population contract. Immutable from the moment StartRun creates it. It pins
// what ZS-CONTROL-001 names for a snapshot (side, source, query/spec,
// watermark, row count, per-currency control totals, hash) plus the source's
// own declared totals it was verified against. The optional fields are empty
// only on snapshots frozen before migration 000004.
type PopulationSnapshot struct {
	SnapshotID            string            `json:"snapshot_id"`
	TenantID              string            `json:"tenant_id"`
	RunID                 string            `json:"run_id"`
	Side                  string            `json:"side"`
	SourceSystem          string            `json:"source_system"`
	SpecRef               string            `json:"spec_ref,omitempty"`
	SourceWatermark       string            `json:"source_watermark,omitempty"`
	ItemCount             int64             `json:"item_count"`
	AmountScale           *int              `json:"amount_scale,omitempty"`
	TotalAmountMinorUnits int64             `json:"total_amount_minor_units"`
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
// The two items must come from different populations, and their amounts must
// agree within the run's pinned tolerance: a manual match records a reason and
// an authority, but it can never hide an amount difference (ZS-CONTROL-001 §10,
// scenario 06 — "narrative alone cannot force pass"). An out-of-tolerance pair
// stays an exception.
func ValidateManualMatch(sideA, sideB string, amountA, amountB, toleranceMinor int64) error {
	if sideA == sideB {
		return ErrManualMatchSameSide
	}
	diff := amountA - amountB
	if diff < 0 {
		diff = -diff
	}
	if diff > toleranceMinor {
		return fmt.Errorf("%w: difference %d exceeds tolerance %d", ErrManualMatchOutsideTolerance, diff, toleranceMinor)
	}
	return nil
}

// ── ReconciliationException ─────────────────────────────────────────────────

type ExceptionStatus string

const (
	ExceptionOpen     ExceptionStatus = "Open"
	ExceptionResolved ExceptionStatus = "Resolved"
)

// ReconciliationException is raised for any item that didn't reconcile
// — unmatched, or matched-by-key but outside tolerance. Its only
// legitimate transition is Open -> Resolved, and that transition only
// ever happens as the side effect of a manual match consuming the
// exact item it was raised for (see store.resolveExceptionsForItem) —
// never a bare "resolve" command with no evidence behind it.
type ReconciliationException struct {
	ExceptionID       string          `json:"exception_id"`
	TenantID          string          `json:"tenant_id"`
	RunID             string          `json:"run_id"`
	ItemID            string          `json:"item_id"`
	Side              string          `json:"side"`
	RefID             string          `json:"ref_id"`
	Reason            string          `json:"reason"`
	AmountMinorUnits  int64           `json:"amount_minor_units"`
	Status            ExceptionStatus `json:"status"`
	RaisedAt          time.Time       `json:"raised_at"`
	RaisedBy          string          `json:"raised_by"`
	ResolvedAt        *time.Time      `json:"resolved_at,omitempty"`
	ResolvedByMatchID *string         `json:"resolved_by_match_id,omitempty"`
}

type RaiseExceptionRequest struct {
	ItemID string `json:"item_id"`
	Reason string `json:"reason"`
}

func (r RaiseExceptionRequest) Validate() error {
	if r.ItemID == "" {
		return fmt.Errorf("item_id is required")
	}
	if r.Reason == "" {
		return fmt.Errorf("reason is required")
	}
	return nil
}

// ── Certification ────────────────────────────────────────────────────────────

// Certification is the sealed, immutable evidence produced by Certify
// — the run's frozen rule version, its final match/exception counts
// and totals, hashed into one addressable manifest.
type Certification struct {
	CertificationID    string    `json:"certification_id"`
	TenantID           string    `json:"tenant_id"`
	RunID              string    `json:"run_id"`
	DefinitionVersion  int64     `json:"definition_version"`
	MatchedCount       int64     `json:"matched_count"`
	ExceptionCount     int64     `json:"exception_count"`
	TotalAssertedMinor int64     `json:"total_asserted_minor_units"`
	ManifestSHA256     string    `json:"manifest_sha256"`
	SealedAt           time.Time `json:"sealed_at"`
	SealedBy           string    `json:"sealed_by"`
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
)
