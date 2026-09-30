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
// run is executed against — match keys, tolerance and materiality.
// Mutable in place like commercial_currencies/meter_definitions
// elsewhere in the platform: a new StartRun always copies the
// definition's CURRENT values onto the run itself (frozen there), so
// editing a definition never reaches back into a run already started
// against an earlier version.
type ReconciliationDefinition struct {
	DefinitionID                string    `json:"definition_id"`
	TenantID                    string    `json:"tenant_id"`
	Name                        string    `json:"name"`
	MatchKeyFields              []string  `json:"match_key_fields"`
	ToleranceAmountMinorUnits   int64     `json:"tolerance_amount_minor_units"`
	MaterialityAmountMinorUnits int64     `json:"materiality_amount_minor_units"`
	Version                     int64     `json:"version"`
	CreatedAt                   time.Time `json:"created_at"`
	CreatedBy                   string    `json:"created_by"`
	UpdatedAt                   time.Time `json:"updated_at"`
}

type DefineReconciliationRequest struct {
	Name                        string   `json:"name"`
	MatchKeyFields              []string `json:"match_key_fields"`
	ToleranceAmountMinorUnits   int64    `json:"tolerance_amount_minor_units"`
	MaterialityAmountMinorUnits int64    `json:"materiality_amount_minor_units"`
}

func (r DefineReconciliationRequest) Validate() error {
	if r.Name == "" {
		return fmt.Errorf("name is required")
	}
	if len(r.MatchKeyFields) == 0 {
		return fmt.Errorf("match_key_fields is required")
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
	DefinitionID                string     `json:"definition_id"`
	DefinitionVersion           int64      `json:"definition_version"`
	Status                      RunStatus  `json:"status"`
	ToleranceAmountMinorUnits   int64      `json:"tolerance_amount_minor_units"`
	MaterialityAmountMinorUnits int64      `json:"materiality_amount_minor_units"`
	SupersedesRunID             *string    `json:"supersedes_run_id,omitempty"`
	SupersededByRunID           *string    `json:"superseded_by_run_id,omitempty"`
	SupersededReason            *string    `json:"superseded_reason,omitempty"`
	StartedAt                   time.Time  `json:"started_at"`
	StartedBy                   string     `json:"started_by"`
	CertifiedAt                 *time.Time `json:"certified_at,omitempty"`
	CertifiedBy                 string     `json:"certified_by,omitempty"`
}

// PopulationSnapshot is one frozen side of a run's comparison (e.g.
// "Ledger" vs "Bank"). Immutable from the moment StartRun creates it —
// this is the "frozen populations" core control the doc names; this
// service never edits the source records a snapshot's items were
// copied from.
type PopulationSnapshot struct {
	SnapshotID            string    `json:"snapshot_id"`
	TenantID              string    `json:"tenant_id"`
	RunID                 string    `json:"run_id"`
	Side                  string    `json:"side"`
	SourceSystem          string    `json:"source_system"`
	ItemCount             int64     `json:"item_count"`
	TotalAmountMinorUnits int64     `json:"total_amount_minor_units"`
	ContentHash           string    `json:"content_hash"`
	CreatedAt             time.Time `json:"created_at"`
}

type PopulationItem struct {
	ItemID           string            `json:"item_id"`
	SnapshotID       string            `json:"snapshot_id"`
	RefID            string            `json:"ref_id"`
	AmountMinorUnits int64             `json:"amount_minor_units"`
	OccurredAt       time.Time         `json:"occurred_at"`
	Dimensions       map[string]string `json:"dimensions,omitempty"`
}

type PopulationInput struct {
	Side         string                `json:"side"`
	SourceSystem string                `json:"source_system"`
	Items        []PopulationItemInput `json:"items"`
}

type PopulationItemInput struct {
	RefID            string            `json:"ref_id"`
	AmountMinorUnits int64             `json:"amount_minor_units"`
	OccurredAt       time.Time         `json:"occurred_at"`
	Dimensions       map[string]string `json:"dimensions,omitempty"`
}

type StartRunRequest struct {
	DefinitionID string            `json:"definition_id"`
	Populations  []PopulationInput `json:"populations"`
}

func (r StartRunRequest) Validate() error {
	if r.DefinitionID == "" {
		return fmt.Errorf("definition_id is required")
	}
	if len(r.Populations) < 2 {
		return fmt.Errorf("at least two populations are required")
	}
	sides := map[string]bool{}
	for _, p := range r.Populations {
		if p.Side == "" {
			return fmt.Errorf("population side is required")
		}
		if sides[p.Side] {
			return fmt.Errorf("duplicate population side %q", p.Side)
		}
		sides[p.Side] = true
		for _, it := range p.Items {
			if it.RefID == "" {
				return fmt.Errorf("population item ref_id is required")
			}
			if it.OccurredAt.IsZero() {
				return fmt.Errorf("population item occurred_at is required")
			}
		}
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
)
