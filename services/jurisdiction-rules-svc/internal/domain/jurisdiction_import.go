package domain

// REF-01 ImportJurisdictionUpdate (ZS-SVC-I-001 s4.11): an externally sourced
// update to the jurisdiction registry, proposed as a batch of rows, approved by
// someone other than whoever proposed it, and applied in one transaction.
//
// "ISO/source updates maker-checker in production" and "Publisher/approver
// separation for externally sourced code-list updates": the registry does not
// change because a file arrived. "External source code+source version" is the
// idempotency key: one source version is imported once. "Source standard/
// version, import hash, effective dates": the batch keeps all of them.

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
)

// What a row of an import does to the registry.
const (
	ImportActionAdd    = "ADD"
	ImportActionUpdate = "UPDATE"
	ImportActionRetire = "RETIRE"
)

// Lifecycle of a batch.
const (
	ImportProposed = "PROPOSED"
	ImportApproved = "APPROVED"
	ImportRejected = "REJECTED"
)

// What approval did with a row.
const (
	OutcomeAdded     = "ADDED"
	OutcomeUpdated   = "UPDATED"
	OutcomeRetired   = "RETIRED"
	OutcomeUnchanged = "UNCHANGED"
)

// Bounds. Codes and types match the jurisdictions columns they end up in.
const (
	MaxImportItems     = 2000
	MaxImportCodeLen   = 32
	MaxImportTypeLen   = 64
	MaxImportNameLen   = 500
	MaxImportNoteLen   = 2000
	MaxImportSourceLen = 128
	MaxImportVersion   = 64
)

// JurisdictionRef names a jurisdiction in an import row: by its id, or by code
// and type. A code alone is never enough, because the same code can mean a
// country and a tax authority; the registry does not choose between them.
type JurisdictionRef struct {
	JurisdictionID string `json:"jurisdiction_id,omitempty"`
	Code           string `json:"code,omitempty"`
	Type           string `json:"type,omitempty"`
}

// Empty reports that nothing was named.
func (r JurisdictionRef) Empty() bool {
	return strings.TrimSpace(r.JurisdictionID) == "" && strings.TrimSpace(r.Code) == "" && strings.TrimSpace(r.Type) == ""
}

// Validate checks the reference is well formed: an id, or a code with its type.
func (r JurisdictionRef) validate(label string) []string {
	id, code, typ := strings.TrimSpace(r.JurisdictionID), strings.TrimSpace(r.Code), strings.TrimSpace(r.Type)
	switch {
	case id != "" && (code != "" || typ != ""):
		return []string{label + ": name it by jurisdiction_id or by code and type, not both"}
	case id != "":
		if _, err := uuid.Parse(id); err != nil {
			return []string{label + ": jurisdiction_id is not a UUID"}
		}
		return nil
	case code == "" || typ == "":
		return []string{label + ": a jurisdiction is named by jurisdiction_id, or by both code and type"}
	}
	var errs []string
	if len(code) > MaxImportCodeLen {
		errs = append(errs, fmt.Sprintf("%s: code is longer than %d characters", label, MaxImportCodeLen))
	}
	if len(typ) > MaxImportTypeLen {
		errs = append(errs, fmt.Sprintf("%s: type is longer than %d characters", label, MaxImportTypeLen))
	}
	return errs
}

// String is a reference as a message names it.
func (r JurisdictionRef) String() string {
	if id := strings.TrimSpace(r.JurisdictionID); id != "" {
		return "jurisdiction " + id
	}
	return fmt.Sprintf("%s %q", strings.TrimSpace(r.Type), strings.TrimSpace(r.Code))
}

// ImportItemInput is one row of an import as it is sent.
type ImportItemInput struct {
	Action string          `json:"action"`
	Target JurisdictionRef `json:"target"`

	// ADD and UPDATE: the attributes the source says the jurisdiction has.
	JurisdictionName string `json:"jurisdiction_name,omitempty"`
	AuthorityType    string `json:"authority_type,omitempty"`
	// Parent places the jurisdiction. On UPDATE, absent means "unchanged".
	Parent *JurisdictionRef `json:"parent,omitempty"`
	// EffectiveFrom is when the source says the row takes effect: required for
	// ADD, optional for UPDATE (default: when it is applied), and not accepted
	// for RETIRE, which takes effect when it is approved.
	EffectiveFrom *time.Time `json:"effective_from,omitempty"`

	// RETIRE: the jurisdiction that takes over, if any.
	Successor *JurisdictionRef `json:"successor,omitempty"`

	Note string `json:"note,omitempty"`
}

// Normalised trims every string, so the same row hashes the same however it was
// padded when it was sent.
func (i ImportItemInput) Normalised() ImportItemInput {
	t := strings.TrimSpace
	i.Action = strings.ToUpper(t(i.Action))
	i.Target = JurisdictionRef{JurisdictionID: t(i.Target.JurisdictionID), Code: t(i.Target.Code), Type: t(i.Target.Type)}
	i.JurisdictionName, i.AuthorityType, i.Note = t(i.JurisdictionName), t(i.AuthorityType), t(i.Note)
	norm := func(r *JurisdictionRef) *JurisdictionRef {
		if r == nil || r.Empty() {
			return nil
		}
		return &JurisdictionRef{JurisdictionID: t(r.JurisdictionID), Code: t(r.Code), Type: t(r.Type)}
	}
	i.Parent, i.Successor = norm(i.Parent), norm(i.Successor)
	if i.EffectiveFrom != nil {
		u := i.EffectiveFrom.UTC()
		i.EffectiveFrom = &u
	}
	return i
}

// Validate reports everything wrong with the row that can be known without
// looking at the registry. An empty result means the row is well formed, not
// that it can be applied: that depends on what the registry holds.
func (i ImportItemInput) Validate() []string {
	i = i.Normalised()
	var errs []string
	switch i.Action {
	case ImportActionAdd, ImportActionUpdate, ImportActionRetire:
	default:
		return []string{fmt.Sprintf("action must be %s, %s or %s", ImportActionAdd, ImportActionUpdate, ImportActionRetire)}
	}
	errs = append(errs, i.Target.validate("target")...)
	if i.Parent != nil {
		errs = append(errs, i.Parent.validate("parent")...)
	}
	if i.Successor != nil {
		errs = append(errs, i.Successor.validate("successor")...)
	}
	if len(i.JurisdictionName) > MaxImportNameLen {
		errs = append(errs, fmt.Sprintf("jurisdiction_name is longer than %d characters", MaxImportNameLen))
	}
	if len(i.AuthorityType) > MaxImportTypeLen {
		errs = append(errs, fmt.Sprintf("authority_type is longer than %d characters", MaxImportTypeLen))
	}
	if len(i.Note) > MaxImportNoteLen {
		errs = append(errs, fmt.Sprintf("note is longer than %d characters", MaxImportNoteLen))
	}

	switch i.Action {
	case ImportActionAdd:
		if i.Target.JurisdictionID != "" {
			errs = append(errs, "an ADD names the new jurisdiction by code and type; its id is assigned by the registry")
		}
		if i.JurisdictionName == "" {
			errs = append(errs, "an ADD needs jurisdiction_name")
		}
		if i.AuthorityType == "" {
			errs = append(errs, "an ADD needs authority_type")
		}
		if i.EffectiveFrom == nil || i.EffectiveFrom.IsZero() {
			errs = append(errs, "an ADD needs effective_from")
		}
		if i.Successor != nil {
			errs = append(errs, "successor applies only to a RETIRE")
		}
	case ImportActionUpdate:
		if i.JurisdictionName == "" && i.AuthorityType == "" && i.Parent == nil {
			errs = append(errs, "an UPDATE must change jurisdiction_name, authority_type or parent")
		}
		if i.Successor != nil {
			errs = append(errs, "successor applies only to a RETIRE")
		}
	case ImportActionRetire:
		if i.JurisdictionName != "" || i.AuthorityType != "" || i.Parent != nil || i.EffectiveFrom != nil {
			errs = append(errs, "a RETIRE takes only a target and, optionally, a successor and a note")
		}
	}
	return errs
}

// ImportItem is a stored row: what was sent, what validation said, and, once the
// batch is approved, what it did.
type ImportItem struct {
	ItemNo int `json:"item_no"`
	ImportItemInput
	ValidationErrors []string `json:"validation_errors"`
	Outcome          *string  `json:"outcome,omitempty"`
	JurisdictionID   *string  `json:"jurisdiction_id,omitempty"`
}

// ImportBatch is a proposed, approved or rejected source update.
type ImportBatch struct {
	BatchID        string `json:"batch_id"`
	SourceStandard string `json:"source_standard"`
	SourceVersion  string `json:"source_version"`
	ImportHash     string `json:"import_hash"`
	EvidenceRef    string `json:"evidence_ref"`
	Summary        string `json:"summary"`

	Status       string `json:"status"`
	ItemCount    int    `json:"item_count"`
	InvalidCount int    `json:"invalid_count"`

	ProposedBy string    `json:"proposed_by"`
	ProposedAt time.Time `json:"proposed_at"`

	DecidedBy      *string    `json:"decided_by,omitempty"`
	DecidedAt      *time.Time `json:"decided_at,omitempty"`
	DecisionReason *string    `json:"decision_reason,omitempty"`

	// Reconciliation: what approval did, in numbers that add up to ItemCount.
	Added     *int `json:"added_count,omitempty"`
	Updated   *int `json:"updated_count,omitempty"`
	Retired   *int `json:"retired_count,omitempty"`
	Unchanged *int `json:"unchanged_count,omitempty"`

	Items []ImportItem `json:"items,omitempty"`
}

// ProposeImportParams is the ImportJurisdictionUpdate command.
type ProposeImportParams struct {
	SourceStandard string
	SourceVersion  string
	EvidenceRef    string
	Summary        string
	Items          []ImportItemInput
	ActorID        string
}

// Validate checks the batch as a whole. A row that is wrong does not fail the
// batch here: it is kept, with its errors, so the whole file's problems are
// reported at once and the batch cannot be approved.
func (p ProposeImportParams) Validate() error {
	switch {
	case strings.TrimSpace(p.ActorID) == "":
		return fmt.Errorf("%w: an actor is required", ErrInvalidImport)
	case strings.TrimSpace(p.SourceStandard) == "" || len(strings.TrimSpace(p.SourceStandard)) > MaxImportSourceLen:
		return fmt.Errorf("%w: source_standard is required (at most %d characters)", ErrInvalidImport, MaxImportSourceLen)
	case strings.TrimSpace(p.SourceVersion) == "" || len(strings.TrimSpace(p.SourceVersion)) > MaxImportVersion:
		return fmt.Errorf("%w: source_version is required (at most %d characters)", ErrInvalidImport, MaxImportVersion)
	case strings.TrimSpace(p.EvidenceRef) == "":
		return fmt.Errorf("%w: evidence_ref is required: say where the source came from", ErrInvalidImport)
	case strings.TrimSpace(p.Summary) == "":
		return fmt.Errorf("%w: summary is required: say what this update is", ErrInvalidImport)
	case len(p.Items) == 0:
		return fmt.Errorf("%w: an import needs at least one row", ErrInvalidImport)
	case len(p.Items) > MaxImportItems:
		return fmt.Errorf("%w: an import carries at most %d rows", ErrInvalidImport, MaxImportItems)
	}
	return nil
}

// ImportHash is the SHA-256 of the normalised content of an import: the source
// standard, its version and every row in order. Two files with the same
// content hash the same however they were formatted, so "the same import"
// means the same content, and a different file for the same source version is
// recognisably different.
func ImportHash(sourceStandard, sourceVersion string, items []ImportItemInput) string {
	norm := make([]ImportItemInput, len(items))
	for i, it := range items {
		norm[i] = it.Normalised()
	}
	canonical, _ := json.Marshal(struct {
		Standard string            `json:"source_standard"`
		Version  string            `json:"source_version"`
		Items    []ImportItemInput `json:"items"`
	}{strings.TrimSpace(sourceStandard), strings.TrimSpace(sourceVersion), norm})
	sum := sha256.Sum256(canonical)
	return hex.EncodeToString(sum[:])
}

// DecideImportParams is an approval or a rejection of a proposed batch.
type DecideImportParams struct {
	BatchID string
	ActorID string
	// Reason is required to reject and optional to approve.
	Reason string
}

// AppliedChange is what one row of an approved import did to one jurisdiction.
// The store returns them so the handler can announce each in the same
// transaction, and a row that changed nothing returns none.
type AppliedChange struct {
	ItemNo  int
	Outcome string
	// Jurisdiction is the jurisdiction after the change; Before is what it was
	// (nil for an ADD).
	Jurisdiction *Jurisdiction
	Before       *Jurisdiction
	// ParentChanged: the jurisdiction moved in the hierarchy.
	ParentChanged bool
	// AttributesChanged: its name or authority type changed.
	AttributesChanged bool
}

// ImportItemError names the row an import could not apply and why.
type ImportItemError struct {
	ItemNo  int
	Message string
}

func (e *ImportItemError) Error() string {
	return fmt.Sprintf("import row %d: %s", e.ItemNo, e.Message)
}

// Is lets errors.Is(err, ErrImportItemFailed) match.
func (e *ImportItemError) Is(target error) bool { return target == ErrImportItemFailed }

var (
	// ErrInvalidImport: the batch as a whole is not acceptable.
	ErrInvalidImport = errorString("invalid jurisdiction import")
	// ErrImportNotFound: no such import batch.
	ErrImportNotFound = errorString("jurisdiction import not found")
	// ErrImportDecided: the batch was already approved or rejected.
	ErrImportDecided = errorString("the jurisdiction import has already been decided")
	// ErrImportVersionConflict: this source version was already imported, with
	// different content. The same content is a replay, not a conflict.
	ErrImportVersionConflict = errorString("this source version already has an import with different content")
	// ErrImportHasInvalidRows: a batch with a row that failed validation cannot
	// be approved.
	ErrImportHasInvalidRows = errorString("the jurisdiction import has rows that failed validation")
	// ErrImportItemFailed is the sentinel *ImportItemError matches: a row could
	// not be applied, so the whole batch was not.
	ErrImportItemFailed = errorString("a row of the jurisdiction import could not be applied")
)
