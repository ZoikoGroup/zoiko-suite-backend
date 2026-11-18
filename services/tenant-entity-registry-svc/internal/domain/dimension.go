package domain

// REF-08 Financial Dimension Registry (ZS-SVC-I-001 s4.18).
//
// A dimension value's identity (its id and code) never changes. Its label and its place in the hierarchy are
// effective-dated versions kept apart from that identity, so renaming or moving a node never changes who it is and a
// historical roll-up is reconstructable. A hierarchy never loops. An inactive value stays valid for history and rejects
// new postings unless an explicit, dated exception was recorded. This registry owns neither the operational source
// objects (a project, a location) nor the dimension values copied onto posted journals.

import (
	"fmt"
	"regexp"
	"strings"
	"time"
)

var (
	dimensionTypeCode = regexp.MustCompile(`^[A-Z][A-Z0-9_]{1,39}$`)
	dimensionCode     = regexp.MustCompile(`^[A-Z0-9][A-Z0-9_.-]{0,39}$`)
)

// Dimension value statuses: Draft, Active, Inactive.
const (
	DimDraft    = "DRAFT"
	DimActive   = "ACTIVE"
	DimInactive = "INACTIVE"
)

// Hierarchy version statuses. A version of a controlled type is PENDING until someone other than its proposer approves it.
const (
	HierarchyPending  = "PENDING"
	HierarchyApproved = "APPROVED"
)

// DimensionCanMoveTo reports whether the lifecycle allows from to to. INACTIVE is not final: a value can be activated
// again, because a dimension is reference data rather than a legal record.
func DimensionCanMoveTo(from, to string) bool {
	switch from {
	case DimDraft:
		return to == DimActive
	case DimActive:
		return to == DimInactive
	case DimInactive:
		return to == DimActive
	}
	return false
}

// DimensionType is a kind of dimension: cost centre, project, region.
type DimensionType struct {
	TypeID       string    `json:"type_id"`
	TenantID     string    `json:"tenant_id"`
	TypeCode     string    `json:"type_code"`
	Name         string    `json:"name"`
	Description  string    `json:"description,omitempty"`
	Hierarchical bool      `json:"hierarchical"`
	Controlled   bool      `json:"controlled"`
	SourceDomain string    `json:"source_domain,omitempty"`
	CreatedBy    string    `json:"created_by"`
	CreatedAt    time.Time `json:"created_at"`
}

// CreateDimensionTypeRequest is CreateDimensionType.
type CreateDimensionTypeRequest struct {
	TypeCode     string `json:"type_code"`
	Name         string `json:"name"`
	Description  string `json:"description,omitempty"`
	Hierarchical bool   `json:"hierarchical"`
	// Controlled marks a hierarchy that feeds reporting or control aggregation: a change to it is independently reviewed.
	Controlled    bool   `json:"controlled"`
	SourceDomain  string `json:"source_domain,omitempty"`
	CorrelationID string `json:"correlation_id,omitempty"`
}

// Validate checks the request.
func (r CreateDimensionTypeRequest) Validate() error {
	switch {
	case !dimensionTypeCode.MatchString(r.TypeCode):
		return fmt.Errorf("type_code must be upper case letters, digits and underscore, starting with a letter (2 to 40 characters)")
	case strings.TrimSpace(r.Name) == "" || len(r.Name) > 200:
		return fmt.Errorf("name is required (1 to 200 characters)")
	case r.Controlled && !r.Hierarchical:
		return fmt.Errorf("a controlled hierarchy needs a hierarchical type")
	case len(r.SourceDomain) > 40 || len(r.Description) > 1000:
		return fmt.Errorf("source_domain or description is too long")
	}
	return nil
}

// DimensionValue is the identity of a value: stable whatever its label or place in the hierarchy.
type DimensionValue struct {
	ValueID        string    `json:"value_id"`
	TenantID       string    `json:"tenant_id"`
	TypeID         string    `json:"type_id"`
	TypeCode       string    `json:"type_code"`
	LegalEntityID  string    `json:"legal_entity_id,omitempty"`
	Code           string    `json:"code"`
	Status         string    `json:"status"`
	CurrentVersion int       `json:"current_version"`
	RecordVersion  int64     `json:"record_version"`
	CreatedBy      string    `json:"created_by"`
	CreatedAt      time.Time `json:"created_at"`
}

// HierarchyVersion is the label and parent a value had over an interval.
type HierarchyVersion struct {
	VersionNo     int        `json:"version_no"`
	ParentValueID string     `json:"parent_value_id,omitempty"`
	ParentCode    string     `json:"parent_code,omitempty"`
	Label         string     `json:"label"`
	Status        string     `json:"status"`
	ValidFrom     string     `json:"valid_from"`
	ValidTo       string     `json:"valid_to,omitempty"`
	Reason        string     `json:"reason,omitempty"`
	EvidenceRef   string     `json:"evidence_ref"`
	ProposedBy    string     `json:"proposed_by"`
	ProposedAt    time.Time  `json:"proposed_at"`
	ApprovedBy    string     `json:"approved_by,omitempty"`
	ApprovedAt    *time.Time `json:"approved_at,omitempty"`
}

// DimensionTransition is one recorded move of a value's status.
type DimensionTransition struct {
	Seq            int       `json:"seq"`
	From           string    `json:"from_status,omitempty"`
	To             string    `json:"to_status"`
	EffectiveDate  string    `json:"effective_date"`
	Command        string    `json:"command"`
	Reason         string    `json:"reason,omitempty"`
	EvidenceRef    string    `json:"evidence_ref"`
	ExceptionUntil string    `json:"exception_until,omitempty"`
	Actor          string    `json:"actor"`
	VersionBefore  int64     `json:"version_before,omitempty"`
	VersionAfter   int64     `json:"version_after"`
	OccurredAt     time.Time `json:"occurred_at"`
}

// DimensionEligibility is whether a value may take a new posting on a date. Inactive values reject new postings unless
// an explicit controlled exception covers the date.
type DimensionEligibility struct {
	ValueID   string `json:"value_id"`
	AsOf      string `json:"as_of"`
	Eligible  bool   `json:"eligible"`
	Exception bool   `json:"exception,omitempty"`
	// Reason is empty when eligible without an exception: NOT_ACTIVE, INACTIVE or NOT_YET_EFFECTIVE.
	Reason string `json:"reason,omitempty"`
}

// EvaluateDimensionPosting is the posting-eligibility rule as a pure function of a value's status on the date and any
// controlled exception recorded on its deactivation.
func EvaluateDimensionPosting(valueID string, day time.Time, statusAsOf string, exceptionUntil *time.Time) DimensionEligibility {
	out := DimensionEligibility{ValueID: valueID, AsOf: day.UTC().Format("2006-01-02")}
	switch statusAsOf {
	case "":
		out.Reason = "NOT_YET_EFFECTIVE"
	case DimActive:
		out.Eligible = true
	case DimInactive:
		if exceptionUntil != nil && !day.UTC().After(*exceptionUntil) {
			out.Eligible, out.Exception = true, true
		} else {
			out.Reason = "INACTIVE"
		}
	default:
		out.Reason = "NOT_ACTIVE"
	}
	return out
}

// PathNode is one step of a value's roll-up path.
type PathNode struct {
	ValueID string `json:"value_id"`
	Code    string `json:"code"`
	Label   string `json:"label"`
}

// DimensionValueView is a value as it was on a date: its label and parent then, its status then, its roll-up path then and
// whether it could take a posting.
type DimensionValueView struct {
	Value       DimensionValue       `json:"value"`
	AsOf        string               `json:"as_of"`
	Hierarchy   *HierarchyVersion    `json:"hierarchy"`
	StatusAsOf  string               `json:"status_as_of"`
	Eligibility DimensionEligibility `json:"posting_eligibility"`
	// Path runs from the root to the value's parent, as the hierarchy stood on the date.
	Path []PathNode `json:"path"`
	// Pending is a proposed hierarchy change awaiting independent approval.
	Pending *HierarchyVersion     `json:"pending_change,omitempty"`
	History []HierarchyVersion    `json:"hierarchy_history,omitempty"`
	Status  []DimensionTransition `json:"status_history,omitempty"`
}

// HierarchyNode is one value in a hierarchy as it stood on a date.
type HierarchyNode struct {
	ValueID       string `json:"value_id"`
	Code          string `json:"code"`
	Label         string `json:"label"`
	LegalEntityID string `json:"legal_entity_id,omitempty"`
	ParentValueID string `json:"parent_value_id,omitempty"`
	Status        string `json:"status"`
	Depth         int    `json:"depth"`
}

// HierarchyAsOf is a dimension type's hierarchy on a date: every value that existed then, with its parent then.
type HierarchyAsOf struct {
	TypeCode string          `json:"type_code"`
	AsOf     string          `json:"as_of"`
	Nodes    []HierarchyNode `json:"nodes"`
}

// SourceMapping is one version of a mapping from an operational source object to a value.
type SourceMapping struct {
	MappingID      string    `json:"mapping_id"`
	TypeCode       string    `json:"type_code,omitempty"`
	ValueID        string    `json:"value_id"`
	SourceDomain   string    `json:"source_domain"`
	SourceObjectID string    `json:"source_object_id"`
	VersionNo      int       `json:"version_no"`
	ValidFrom      string    `json:"valid_from"`
	ValidTo        string    `json:"valid_to,omitempty"`
	EvidenceRef    string    `json:"evidence_ref"`
	RecordedBy     string    `json:"recorded_by"`
	RecordedAt     time.Time `json:"recorded_at"`
}

// DimensionResolution is the answer to ResolveDimension: the value, how it was found and whether it can take a posting.
type DimensionResolution struct {
	Via     string              `json:"via"` // CODE or SOURCE_MAPPING
	View    *DimensionValueView `json:"value,omitempty"`
	Mapping *SourceMapping      `json:"mapping,omitempty"`
}

// CreateDimensionValueRequest is CreateDimensionValue. The value starts DRAFT.
type CreateDimensionValueRequest struct {
	TypeCode string `json:"type_code"`
	// LegalEntityID scopes the value to one entity; empty means tenant-wide.
	LegalEntityID string `json:"legal_entity_id,omitempty"`
	Code          string `json:"code"`
	Label         string `json:"label"`
	ParentValueID string `json:"parent_value_id,omitempty"`
	ValidFrom     string `json:"valid_from,omitempty"`
	EvidenceRef   string `json:"evidence_ref"`
	CorrelationID string `json:"correlation_id,omitempty"`
}

// Validate checks the request.
func (r CreateDimensionValueRequest) Validate() error {
	switch {
	case !dimensionTypeCode.MatchString(r.TypeCode):
		return fmt.Errorf("type_code names a dimension type")
	case !dimensionCode.MatchString(r.Code):
		return fmt.Errorf("code must be upper case letters, digits, dot, dash or underscore (1 to 40 characters)")
	case strings.TrimSpace(r.Label) == "" || len(r.Label) > 200:
		return fmt.Errorf("label is required (1 to 200 characters)")
	case strings.TrimSpace(r.EvidenceRef) == "":
		return fmt.Errorf("evidence_ref is required")
	}
	if r.ValidFrom != "" {
		if _, err := ParseDate(r.ValidFrom); err != nil {
			return fmt.Errorf("valid_from %v", err)
		}
	}
	return nil
}

// ReparentDimensionValueRequest is ReparentDimensionValue: the next hierarchy version. It names a parent, or Root to make
// the value a root, and may also give a new label: renaming never changes the value's identity.
type ReparentDimensionValueRequest struct {
	ExpectedVersion int64   `json:"expected_version"`
	ParentValueID   *string `json:"parent_value_id,omitempty"`
	Root            bool    `json:"root,omitempty"`
	NewLabel        string  `json:"new_label,omitempty"`
	ValidFrom       string  `json:"valid_from,omitempty"`
	Reason          string  `json:"reason"`
	EvidenceRef     string  `json:"evidence_ref"`
	CorrelationID   string  `json:"correlation_id,omitempty"`
}

// Validate checks the request.
func (r ReparentDimensionValueRequest) Validate() error {
	hasParent := r.ParentValueID != nil && strings.TrimSpace(*r.ParentValueID) != ""
	switch {
	case hasParent == r.Root:
		return fmt.Errorf("name a new parent (parent_value_id) or make the value a root (root: true), not both and not neither")
	case strings.TrimSpace(r.Reason) == "":
		return fmt.Errorf("reason is required")
	case strings.TrimSpace(r.EvidenceRef) == "":
		return fmt.Errorf("evidence_ref is required")
	case len(r.NewLabel) > 200:
		return fmt.Errorf("new_label is too long")
	}
	if r.ValidFrom != "" {
		if _, err := ParseDate(r.ValidFrom); err != nil {
			return fmt.Errorf("valid_from %v", err)
		}
	}
	return nil
}

// ApproveDimensionHierarchyRequest approves a pending hierarchy change.
type ApproveDimensionHierarchyRequest struct {
	ExpectedVersion int64  `json:"expected_version"`
	EvidenceRef     string `json:"evidence_ref"`
	CorrelationID   string `json:"correlation_id,omitempty"`
}

// Validate checks the request.
func (r ApproveDimensionHierarchyRequest) Validate() error {
	if strings.TrimSpace(r.EvidenceRef) == "" {
		return fmt.Errorf("evidence_ref is required")
	}
	return nil
}

// DimensionStatusRequest is ActivateDimensionValue and DeactivateDimensionValue.
type DimensionStatusRequest struct {
	ExpectedVersion int64  `json:"expected_version"`
	Reason          string `json:"reason,omitempty"`
	EvidenceRef     string `json:"evidence_ref"`
	EffectiveDate   string `json:"effective_date,omitempty"`
	// ExceptionUntil, on a deactivation, is an explicit controlled exception: new postings are still accepted until that
	// date. It needs a reason, and is bounded by MaxPostingException.
	ExceptionUntil string `json:"exception_until,omitempty"`
	CorrelationID  string `json:"correlation_id,omitempty"`
}

// MaxPostingException bounds a controlled exception so it cannot become a standing way round a deactivation.
const MaxPostingException = 90 * 24 * time.Hour

// ValidateFor checks the request for a move to status to.
func (r DimensionStatusRequest) ValidateFor(to string) error {
	if strings.TrimSpace(r.EvidenceRef) == "" {
		return fmt.Errorf("evidence_ref is required")
	}
	if to == DimInactive && strings.TrimSpace(r.Reason) == "" {
		return fmt.Errorf("reason is required to deactivate a dimension value")
	}
	eff := time.Now().UTC()
	if r.EffectiveDate != "" {
		d, err := ParseDate(r.EffectiveDate)
		if err != nil {
			return fmt.Errorf("effective_date %v", err)
		}
		eff = d
	}
	if r.ExceptionUntil != "" {
		if to != DimInactive {
			return fmt.Errorf("exception_until applies only when deactivating")
		}
		u, err := ParseDate(r.ExceptionUntil)
		if err != nil {
			return fmt.Errorf("exception_until %v", err)
		}
		day := time.Date(eff.Year(), eff.Month(), eff.Day(), 0, 0, 0, 0, time.UTC)
		if !u.After(day) || u.Sub(day) > MaxPostingException {
			return fmt.Errorf("exception_until must be after the deactivation and within 90 days of it")
		}
	}
	return nil
}

// MapSourceObjectRequest is MapSourceObject: an operational source object (a project, a location) maps to the value from
// valid_from, replacing its previous mapping there.
type MapSourceObjectRequest struct {
	SourceDomain   string `json:"source_domain"`
	SourceObjectID string `json:"source_object_id"`
	ValidFrom      string `json:"valid_from,omitempty"`
	EvidenceRef    string `json:"evidence_ref"`
	CorrelationID  string `json:"correlation_id,omitempty"`
}

// Validate checks the request.
func (r MapSourceObjectRequest) Validate() error {
	switch {
	case strings.TrimSpace(r.SourceDomain) == "" || len(r.SourceDomain) > 40:
		return fmt.Errorf("source_domain is required (1 to 40 characters)")
	case strings.TrimSpace(r.SourceObjectID) == "" || len(r.SourceObjectID) > 200:
		return fmt.Errorf("source_object_id is required (1 to 200 characters)")
	case strings.TrimSpace(r.EvidenceRef) == "":
		return fmt.Errorf("evidence_ref is required")
	}
	if r.ValidFrom != "" {
		if _, err := ParseDate(r.ValidFrom); err != nil {
			return fmt.Errorf("valid_from %v", err)
		}
	}
	return nil
}

// ResolveDimensionQuery is ResolveDimension: by code within a type, or by an operational source object.
type ResolveDimensionQuery struct {
	TypeCode       string
	Code           string
	LegalEntityID  string
	SourceDomain   string
	SourceObjectID string
	AsOf           time.Time
}

// Validate checks the query names exactly one way to find a value.
func (q ResolveDimensionQuery) Validate() error {
	byCode := q.Code != ""
	bySource := q.SourceDomain != "" || q.SourceObjectID != ""
	switch {
	case byCode == bySource:
		return fmt.Errorf("resolve by code (type_code and code) or by source object (type_code, source_domain and source_object_id), not both and not neither")
	case q.TypeCode == "":
		return fmt.Errorf("type_code is required")
	case bySource && (q.SourceDomain == "" || q.SourceObjectID == ""):
		return fmt.Errorf("source_domain and source_object_id go together")
	}
	return nil
}

// HierarchyLoop reports whether making newParent the parent of value would close a loop in parentOf (value id to parent
// id, as the hierarchy stands on the date being checked). Reaching value by walking up from newParent is a loop; so is a
// chain that never ends.
func HierarchyLoop(parentOf map[string]string, value, newParent string) bool {
	if newParent == "" {
		return false
	}
	seen := map[string]bool{}
	for cur := newParent; cur != ""; cur = parentOf[cur] {
		if cur == value || seen[cur] {
			return true
		}
		seen[cur] = true
	}
	return false
}

// HierarchyDepth is how many parents a value has in parentOf.
func HierarchyDepth(parentOf map[string]string, value string) int {
	n := 0
	for cur := parentOf[value]; cur != "" && n <= len(parentOf); cur = parentOf[cur] {
		n++
	}
	return n
}
