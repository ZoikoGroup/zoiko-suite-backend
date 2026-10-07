package domain

import (
	"fmt"
	"strings"
	"time"
)

// BIZ-03 Template — TemplateDefinition owns identity/purpose/owner;
// TemplateVersion owns the actual governed content and its own
// lifecycle (Draft -> Review -> Approved -> Published ->
// Retired/Superseded). A definition can carry several locales, each
// version maturing independently, so the state machine lives on the
// version — see migration 000005's own comment for why.
type TemplateDefinition struct {
	TemplateID           string     `json:"template_id"`
	TenantID             string     `json:"tenant_id"`
	LegalEntityID        string     `json:"legal_entity_id"`
	Name                 string     `json:"name"`
	BusinessPurpose      string     `json:"business_purpose"`
	OwnerPrincipalID     string     `json:"owner_principal_id"`
	// IntentID is the communication intent this wording is for; fixed at creation
	// (migration 000021). Nil for a template bound to no intent.
	IntentID *string `json:"intent_id,omitempty"`
	Status               string     `json:"status"` // ACTIVE, RETIRED
	CreatedAt            time.Time  `json:"created_at"`
	RetiredAt            *time.Time `json:"retired_at,omitempty"`
	RetiredByPrincipalID *string    `json:"retired_by_principal_id,omitempty"`
}

// TemplateVersionStatus values. Draft -> Review -> Approved -> Published
// -> Retired/Superseded, exactly as the doc's own lifecycle line states.
const (
	TemplateVersionDraft      = "DRAFT"
	TemplateVersionReview     = "REVIEW"
	TemplateVersionApproved   = "APPROVED"
	TemplateVersionPublished  = "PUBLISHED"
	TemplateVersionRetired    = "RETIRED"
	TemplateVersionSuperseded = "SUPERSEDED"
)

type TemplateVersion struct {
	VersionID             string     `json:"version_id"`
	TemplateID            string     `json:"template_id"`
	TenantID              string     `json:"tenant_id"`
	LegalEntityID         string     `json:"legal_entity_id"`
	VersionNumber         int        `json:"version_number"`
	Locale                string     `json:"locale"`
	Content               string     `json:"content"`
	ContentHash           string     `json:"content_hash"`
	VariableSchema        []string   `json:"variable_schema"`
	// Subject is the reviewed subject text (placeholders only), frozen with the
	// version; nil on a version that leaves the subject to the caller (legacy).
	// SubjectVariables are the variables an author declared safe for a subject.
	Subject               *string    `json:"subject,omitempty"`
	SubjectVariables      []string   `json:"subject_variables"`
	BrandingMetadata      *string    `json:"branding_metadata,omitempty"`
	AccessibilityMetadata *string    `json:"accessibility_metadata,omitempty"`
	Status                string     `json:"status"`
	CreatedByPrincipalID  string     `json:"created_by_principal_id"`
	CreatedAt             time.Time  `json:"created_at"`
	ValidatedAt           *time.Time `json:"validated_at,omitempty"`
	ApprovedByPrincipalID *string    `json:"approved_by_principal_id,omitempty"`
	ApprovedAt            *time.Time `json:"approved_at,omitempty"`
	PublishedAt           *time.Time `json:"published_at,omitempty"`
	RetiredAt             *time.Time `json:"retired_at,omitempty"`
	SupersededByVersionID *string    `json:"superseded_by_version_id,omitempty"`
}

// CreateTemplateParams is CreateTemplate's input.
type CreateTemplateParams struct {
	TenantID         string
	LegalEntityID    string
	Name             string
	BusinessPurpose  string
	OwnerPrincipalID string
	// IntentID optionally binds the template to a communication intent, for good.
	IntentID string
	// CorrelationID is carried onto the template.created event.
	CorrelationID string
}

// CreateVersionParams is CreateVersion's input — a new DRAFT version for
// an existing, ACTIVE template definition.
type CreateVersionParams struct {
	TemplateID            string
	Locale                string
	Content               string
	VariableSchema        []string
	Subject               string
	SubjectVariables      []string
	BrandingMetadata      string
	AccessibilityMetadata string
	CreatedByPrincipalID  string
}

// ApproveVersionParams is ApproveTemplate's input.
type ApproveVersionParams struct {
	VersionID             string
	ApprovedByPrincipalID string
	CorrelationID         string
}

// PublishVersionParams is PublishTemplate's input.
type PublishVersionParams struct {
	VersionID              string
	PublishedByPrincipalID string
	CorrelationID          string
}

// RetireTemplateParams is RetireTemplate's input — retires the whole
// definition (blocks new versions) and every version of it that is
// still PUBLISHED, across every locale.
type RetireTemplateParams struct {
	TemplateID           string
	RetiredByPrincipalID string
	CorrelationID        string
}

// RenderPreviewParams is RenderPreview's input — renders a version's
// content against supplied variables regardless of its status, so an
// author or approver can see what a DRAFT/REVIEW version actually looks
// like before it is ever published.
type RenderPreviewParams struct {
	VersionID string
	// Placeholders fills a declared variable the caller did not supply with a visible
	// marker instead of refusing: an author checking wording needs no real data.
	// Never set on the send path.
	Placeholders bool
	Variables map[string]string
}

// RenderPreviewResult is RenderPreview's output.
type RenderPreviewResult struct {
	VersionID        string   `json:"version_id"`
	RenderedContent  string   `json:"rendered_content"`
	// RenderedSubject is set when the version carries a subject.
	RenderedSubject  string   `json:"rendered_subject,omitempty"`
	// PlaceholdersUsed names the variables filled with markers (preview only).
	PlaceholdersUsed []string `json:"placeholders_used,omitempty"`
	MissingVariables []string `json:"missing_variables,omitempty"`
}

// CompareVersionsResult is CompareVersions' output — both versions in
// full, plus the two things that actually differ between them in a way
// a reviewer cares about: whether the content itself changed, and which
// variables were added or removed from the schema.
type CompareVersionsResult struct {
	VersionA         TemplateVersion `json:"version_a"`
	VersionB         TemplateVersion `json:"version_b"`
	ContentChanged   bool            `json:"content_changed"`
	VariablesAdded   []string        `json:"variables_added,omitempty"`
	VariablesRemoved []string        `json:"variables_removed,omitempty"`
}

// LocaleSummary is one entry in ListLocales' output — one locale's
// latest version and, if any, which version currently governs it.
type LocaleSummary struct {
	Locale              string  `json:"locale"`
	LatestVersionID     string  `json:"latest_version_id"`
	LatestVersionNumber int     `json:"latest_version_number"`
	LatestStatus        string  `json:"latest_status"`
	PublishedVersionID  *string `json:"published_version_id,omitempty"`
}

var (
	ErrTemplateNotFound            = errorString("template not found")
	ErrTemplateVersionNotFound     = errorString("template version not found")
	ErrTemplateRetired             = errorString("template is retired; no new versions may be created")
	ErrTemplateAlreadyRetired      = errorString("template is already retired")
	ErrTemplateVersionNotDraft     = errorString("template version is not in DRAFT status")
	ErrTemplateVersionNotReview    = errorString("template version is not in REVIEW status")
	ErrTemplateVersionNotApproved  = errorString("template version is not in APPROVED status")
	ErrTemplateVersionSelfApproval = errorString("the principal who created a template version cannot also approve it")
	ErrTemplateContentInvalid      = errorString("template content failed validation")
	ErrTemplateLocaleRequired      = errorString("locale is required")
	// ErrTemplateVersionsBelongToDifferentTemplates backs CompareVersions —
	// comparing versions across two different templates is not a
	// meaningful diff and is refused rather than silently allowed.
	ErrTemplateVersionsBelongToDifferentTemplates = errorString("the two versions belong to different templates")
)

// ErrTemplateVariablesMissing backs RenderPreview — refusing a partial
// render beats producing one with a blank field, same posture as
// internal/templates.ErrMissingVariables.
type ErrTemplateVariablesMissing struct {
	VersionID string
	Missing   []string
}

func (e ErrTemplateVariablesMissing) Error() string {
	return fmt.Sprintf("version %s requires variables: %s", e.VersionID, strings.Join(e.Missing, ", "))
}

// ErrTemplateVariablesUnexpected backs RenderPreview and the governed-template
// send: a variable the version's schema does not declare is refused rather than
// silently ignored (ZS-SVC-Y-001 NP-07). An ignored variable is how a caller
// believes a value reached the recipient when it did not, and how a variable
// name starts doing something its template author never reviewed.
type ErrTemplateVariablesUnexpected struct {
	VersionID  string
	Unexpected []string
}

func (e ErrTemplateVariablesUnexpected) Error() string {
	return fmt.Sprintf("version %s does not declare variables: %s", e.VersionID, strings.Join(e.Unexpected, ", "))
}
