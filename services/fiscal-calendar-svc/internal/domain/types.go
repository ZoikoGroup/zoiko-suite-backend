package domain

import (
	"encoding/json"
	"time"
)

// CalendarStatus is the state of the calendar header. A calendar becomes
// ACTIVE when its first version is activated and is never deleted.
type CalendarStatus string

const (
	CalendarDraft  CalendarStatus = "DRAFT"
	CalendarActive CalendarStatus = "ACTIVE"
)

// VersionStatus is the lifecycle of a calendar version:
// DRAFT -> APPROVED -> ACTIVE -> SUPERSEDED.
type VersionStatus string

const (
	VersionDraft      VersionStatus = "DRAFT"
	VersionApproved   VersionStatus = "APPROVED"
	VersionActive     VersionStatus = "ACTIVE"
	VersionSuperseded VersionStatus = "SUPERSEDED"
)

// Valid reports whether s is a known version status.
func (s VersionStatus) Valid() bool {
	switch s {
	case VersionDraft, VersionApproved, VersionActive, VersionSuperseded:
		return true
	}
	return false
}

// InForce reports whether versions in this status occupy their effective
// interval (a superseded version still governs the dates it was in force).
func (s VersionStatus) InForce() bool { return s == VersionActive || s == VersionSuperseded }

// PlanStatus is the lifecycle of a CalendarTransitionPlan.
type PlanStatus string

const (
	PlanProposed PlanStatus = "PROPOSED"
	PlanApproved PlanStatus = "APPROVED"
	PlanRejected PlanStatus = "REJECTED"
)

// FiscalCalendar is the calendar header: identity and scope. The period
// definition lives on its versions.
type FiscalCalendar struct {
	CalendarID    string `json:"calendar_id"`
	TenantID      string `json:"tenant_id"`
	LegalEntityID string `json:"legal_entity_id"`
	Code          string `json:"code"`
	// Scope is an opaque book/basis label (for example a statutory or
	// management basis). It is data chosen by the tenant; this service never
	// interprets it and has no enumeration of values.
	Scope      string         `json:"scope"`
	Status     CalendarStatus `json:"status"`
	Version    int64          `json:"version"`
	CreatedBy  string         `json:"created_by"`
	CreatedAt  time.Time      `json:"created_at"`
	RecordedAt time.Time      `json:"recorded_at"`
}

// FiscalCalendarVersion is one effective-dated definition of a calendar. Once
// APPROVED its pattern and effective dates never change; a correction is a new
// version.
type FiscalCalendarVersion struct {
	VersionID            string        `json:"version_id"`
	CalendarID           string        `json:"calendar_id"`
	TenantID             string        `json:"tenant_id"`
	LegalEntityID        string        `json:"legal_entity_id"`
	Scope                string        `json:"scope"`
	VersionNo            int           `json:"version_no"`
	Pattern              Pattern       `json:"pattern"`
	FiscalYearStartMonth int           `json:"fiscal_year_start_month"`
	FiscalYearStartDay   int           `json:"fiscal_year_start_day"`
	EffectiveFrom        Date          `json:"effective_from"`
	EffectiveTo          *Date         `json:"effective_to"`
	Status               VersionStatus `json:"status"`
	ProposedBy           string        `json:"proposed_by"`
	ProposalReason       string        `json:"proposal_reason"`
	ApprovedBy           string        `json:"approved_by,omitempty"`
	ApprovalReason       string        `json:"approval_reason,omitempty"`
	ApprovedAt           *time.Time    `json:"approved_at,omitempty"`
	ActivatedBy          string        `json:"activated_by,omitempty"`
	ActivatedAt          *time.Time    `json:"activated_at,omitempty"`
	SupersededByVersion  string        `json:"superseded_by_version_id,omitempty"`
	RecordedAt           time.Time     `json:"recorded_at"`
	Version              int64         `json:"version"`
}

// Interval is the effective interval of the version.
func (v *FiscalCalendarVersion) Interval() Interval {
	return Interval{From: v.EffectiveFrom, To: v.EffectiveTo}
}

// CalendarTransitionPlan is the evidence that moving from one version to
// another was assessed: impact, whether posted/closed periods are affected and
// how old period keys map onto new ones. It never rewrites old periods; it
// records how consumers carry balances/reports across the boundary.
type CalendarTransitionPlan struct {
	PlanID               string          `json:"plan_id"`
	TenantID             string          `json:"tenant_id"`
	LegalEntityID        string          `json:"legal_entity_id"`
	CalendarID           string          `json:"calendar_id"`
	FromVersionID        string          `json:"from_version_id"`
	ToVersionID          string          `json:"to_version_id"`
	ImpactAssessment     json.RawMessage `json:"impact_assessment"`
	AffectsPostedPeriods bool            `json:"affects_posted_periods"`
	Mapping              json.RawMessage `json:"mapping"`
	Status               PlanStatus      `json:"status"`
	ProposedBy           string          `json:"proposed_by"`
	Reason               string          `json:"reason"`
	DecidedBy            string          `json:"decided_by,omitempty"`
	DecisionReason       string          `json:"decision_reason,omitempty"`
	DecidedAt            *time.Time      `json:"decided_at,omitempty"`
	Version              int64           `json:"version"`
	CreatedAt            time.Time       `json:"created_at"`
}

// StatusHistoryEntry is the append-only record of one version transition.
type StatusHistoryEntry struct {
	HistoryID        string
	VersionID        string
	FromStatus       VersionStatus // "" for creation
	ToStatus         VersionStatus
	Actor            string
	Reason           string
	RecordedAt       time.Time
	ResultingVersion int64
	CorrelationID    string
}

// ValidatePlanContent checks and canonicalises a plan's impact assessment and
// period mapping. Mapping is {old_period_key: [new_period_key, ...]}; a plan
// that declares posted-period impact must carry a non-empty mapping.
func ValidatePlanContent(impact, mapping json.RawMessage, affectsPosted bool) (json.RawMessage, json.RawMessage, error) {
	var imp map[string]any
	if err := json.Unmarshal(impact, &imp); err != nil || len(imp) == 0 {
		return nil, nil, Errf(CodeContextInvalid, "impact_assessment must be a non-empty JSON object")
	}
	var m map[string][]string
	if len(mapping) == 0 {
		mapping = json.RawMessage(`{}`)
	}
	if err := json.Unmarshal(mapping, &m); err != nil {
		return nil, nil, Errf(CodeContextInvalid, "mapping must be an object of old period key -> array of new period keys")
	}
	for old, news := range m {
		if old == "" || len(news) == 0 {
			return nil, nil, Errf(CodeContextInvalid, "mapping entries need a non-empty old period key and at least one new period key")
		}
		for _, n := range news {
			if n == "" {
				return nil, nil, Errf(CodeContextInvalid, "mapping contains an empty new period key for %q", old)
			}
		}
	}
	if affectsPosted && len(m) == 0 {
		return nil, nil, Errf(CodeContextInvalid, "a plan that affects posted periods must carry a non-empty period mapping")
	}
	ib, err := json.Marshal(imp)
	if err != nil {
		return nil, nil, err
	}
	mb, err := json.Marshal(m)
	if err != nil {
		return nil, nil, err
	}
	return ib, mb, nil
}
