package domain

import (
	"strings"
	"time"
)

// ThresholdStatus is the maker-checker lifecycle of a threshold rule.
//
//	DRAFT --(approve, by a different principal)--> ACTIVE
//	ACTIVE --(another rule for the same category is approved)--> SUPERSEDED
//	DRAFT | ACTIVE --(retire)--> RETIRED   (terminal)
//
// Only DRAFT rules may be edited. Rules are never deleted.
type ThresholdStatus string

const (
	ThresholdDraft      ThresholdStatus = "DRAFT"
	ThresholdActive     ThresholdStatus = "ACTIVE"
	ThresholdSuperseded ThresholdStatus = "SUPERSEDED"
	ThresholdRetired    ThresholdStatus = "RETIRED"
)

// IsValid reports whether s is a known threshold status.
func (s ThresholdStatus) IsValid() bool {
	switch s {
	case ThresholdDraft, ThresholdActive, ThresholdSuperseded, ThresholdRetired:
		return true
	}
	return false
}

// DateLayout is the wire and storage format for effective dates.
const DateLayout = "2006-01-02"

// RiskThresholdRule is one versioned band ladder for one category.
//
// Effective dating decides which approved rule bands a score on a given
// date: the rule with effective_from <= date < effective_to (effective_to
// NULL = open-ended). Status only describes the rule's own lifecycle.
type RiskThresholdRule struct {
	ID                  string          `json:"id"`
	TenantID            string          `json:"tenant_id"`
	RuleName            string          `json:"rule_name"`
	RiskCategory        RiskCategory    `json:"risk_category"`
	ModerateThreshold   Hundredths      `json:"moderate_threshold"`
	HighThreshold       Hundredths      `json:"high_threshold"`
	CriticalThreshold   Hundredths      `json:"critical_threshold"`
	NotificationChannel string          `json:"notification_channel"`
	Status              ThresholdStatus `json:"status"`
	Version             int             `json:"version"`
	EffectiveFrom       string          `json:"effective_from"`
	EffectiveTo         *string         `json:"effective_to"`
	CreatedBy           string          `json:"created_by"`
	UpdatedBy           string          `json:"updated_by"`
	ApprovedBy          *string         `json:"approved_by"`
	ApprovedAt          *time.Time      `json:"approved_at"`
	RetiredBy           *string         `json:"retired_by"`
	RetiredAt           *time.Time      `json:"retired_at"`
	SupersedesRuleID    *string         `json:"supersedes_rule_id"`
	CreatedAt           time.Time       `json:"created_at"`
	UpdatedAt           time.Time       `json:"updated_at"`
}

// Cutoffs returns the rule's band ladder.
func (r *RiskThresholdRule) Cutoffs() BandCutoffs {
	return BandCutoffs{Moderate: r.ModerateThreshold, High: r.HighThreshold, Critical: r.CriticalThreshold}
}

// CanEdit: only drafts are editable; an approved ladder is evidence for the
// scores it banded and must not change underneath them.
func (r *RiskThresholdRule) CanEdit() *Error {
	if r.Status != ThresholdDraft {
		return InvalidTransition("threshold rule", string(r.Status), "edited")
	}
	return nil
}

// CanApprove checks the approve transition, the maker-checker rule and the
// no-backdating rule. today is the approval date (UTC).
func (r *RiskThresholdRule) CanApprove(approverID string, today string) *Error {
	if r.Status != ThresholdDraft {
		return InvalidTransition("threshold rule", string(r.Status), "approved")
	}
	if approverID == r.CreatedBy || approverID == r.UpdatedBy {
		return Forbidden("segregation_of_duties",
			"a threshold rule must be approved by a principal other than its author or last editor")
	}
	if r.EffectiveFrom < today {
		return Unprocessable("effective_from_in_past",
			"effective_from "+r.EffectiveFrom+" is before today ("+today+"); thresholds cannot be backdated — edit the draft")
	}
	return nil
}

// CanRetire checks the retire transition.
func (r *RiskThresholdRule) CanRetire() *Error {
	if r.Status != ThresholdDraft && r.Status != ThresholdActive {
		return InvalidTransition("threshold rule", string(r.Status), "retired")
	}
	return nil
}

// ThresholdRuleInput is the body of POST and PUT /v1/risk-scores/thresholds.
type ThresholdRuleInput struct {
	TenantID            string      `json:"tenant_id,omitempty"`
	RuleName            string      `json:"rule_name"`
	RiskCategory        string      `json:"risk_category"`
	ModerateThreshold   *Hundredths `json:"moderate_threshold"`
	HighThreshold       *Hundredths `json:"high_threshold"`
	CriticalThreshold   *Hundredths `json:"critical_threshold"`
	NotificationChannel string      `json:"notification_channel,omitempty"`
	EffectiveFrom       string      `json:"effective_from"`
}

// DefaultNotificationChannel is used when the body leaves it blank.
const DefaultNotificationChannel = "GOVERNANCE_DESK"

// Normalize trims strings and fills the notification channel default.
func (in *ThresholdRuleInput) Normalize() {
	in.RuleName = strings.TrimSpace(in.RuleName)
	in.RiskCategory = strings.ToUpper(strings.TrimSpace(in.RiskCategory))
	in.NotificationChannel = strings.TrimSpace(in.NotificationChannel)
	in.EffectiveFrom = strings.TrimSpace(in.EffectiveFrom)
	if in.NotificationChannel == "" {
		in.NotificationChannel = DefaultNotificationChannel
	}
}

// Validate checks every field. All three thresholds are required: the
// 000001-era handler silently defaulted missing ones to 60/80, which made a
// typo in a field name create a live rule with values nobody chose.
func (in *ThresholdRuleInput) Validate() []FieldError {
	var problems []FieldError
	add := func(field, detail string) { problems = append(problems, FieldError{Field: field, Detail: detail}) }

	if in.RuleName == "" {
		add("rule_name", "is required")
	} else if len(in.RuleName) > MaxNameLength {
		add("rule_name", "must be at most 128 characters")
	}

	if !RiskCategory(in.RiskCategory).IsThresholdCategory() {
		add("risk_category", "must be COMPOSITE or one of REGULATORY_OBLIGATIONS, POLICY_VIOLATIONS, "+
			"AUDIT_EXCEPTIONS, DATA_PRIVACY, TAX_COMPLIANCE")
	}

	if len(in.NotificationChannel) > MaxIdentifierLength {
		add("notification_channel", "must be at most 64 characters")
	}

	if in.EffectiveFrom == "" {
		add("effective_from", "is required (YYYY-MM-DD)")
	} else if _, err := time.Parse(DateLayout, in.EffectiveFrom); err != nil {
		add("effective_from", "must be a date in YYYY-MM-DD format")
	}

	missingThreshold := false
	if in.ModerateThreshold == nil {
		add("moderate_threshold", "is required")
		missingThreshold = true
	}
	if in.HighThreshold == nil {
		add("high_threshold", "is required")
		missingThreshold = true
	}
	if in.CriticalThreshold == nil {
		add("critical_threshold", "is required")
		missingThreshold = true
	}
	if !missingThreshold {
		problems = append(problems, in.Cutoffs().Validate()...)
	}

	return problems
}

// Cutoffs returns the ladder from a validated input.
func (in *ThresholdRuleInput) Cutoffs() BandCutoffs {
	return BandCutoffs{Moderate: *in.ModerateThreshold, High: *in.HighThreshold, Critical: *in.CriticalThreshold}
}

// ThresholdTransitionRequest is the optional body of approve and retire.
// It carries nothing but an optional tenant_id, which must agree with the
// X-Tenant-Id header when present.
type ThresholdTransitionRequest struct {
	TenantID string `json:"tenant_id,omitempty"`
}
