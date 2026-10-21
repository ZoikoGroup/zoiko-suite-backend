// Package domain defines the canonical types for privacy-purpose-registry-svc
// — PRV-01, "Processing Activity, Purpose & Lawful-Basis Registry", from
// ZS-SVC-W-001 (Privacy/Consent/Purpose/Data Rights Control).
//
// v1 scope, documented rather than hidden (same doctrine as
// evidence-manifest-svc's domain package doc comment):
//
//   - ZS-SVC-W-001 specifies five services, PRV-01 through PRV-05. This
//     service implements ONLY PRV-01, the registry. §35's eight-wave
//     sequence names PRV-01 as the first buildable service — PRV-02
//     (consent), PRV-03 (runtime purpose-binding decisions), PRV-04
//     (data rights/DSR), and PRV-05 (processor/transfer) all depend on a
//     working purpose+activity registry existing first, so building any
//     of them before this one would mean building on a foundation the
//     spec itself says doesn't exist yet.
//   - "Validate" performs deterministic STRUCTURAL checks only (are
//     required fields present, do referenced purposes exist and are they
//     published). It does NOT evaluate jurisdiction-specific legal
//     correctness — the spec's own §0 status line is explicit that
//     "jurisdiction-specific production rules require approved PDC
//     packages and qualified legal/privacy ownership," and no such
//     package or service exists anywhere in this codebase yet. Inventing
//     a fake legal-correctness check here would be exactly the
//     fabricated-success shape this codebase has spent this whole
//     workstream removing.
//   - "Submit" and "Approve"/"Reject" are real, authorized, audited state
//     transitions — but there is no real workflow-svc orchestration
//     wired in behind them (no WFC integration exists for this domain).
//     SUBMITTED is never silently treated as APPROVED; APPROVED is only
//     ever reached through an explicit, separately-authorized Approve
//     call. This is an honest, working approval gate, not a fabricated
//     one — it is just simpler than a full governed workflow engine.
//   - Lawful-basis and retention/transfer references (LawfulBasisRefs,
//     RetentionRuleRefs, TransferRefs) are stored as opaque, data-only
//     string references — same doctrine as retention-registry-svc's
//     record_class column: no registry backs their content yet, and this
//     service does not validate what they point to.
package domain

import (
	"strings"
	"time"
)

// ── Purpose ──────────────────────────────────────────────────────────────────

// PurposeVersionStatus is data only — new values are added via data, not a
// Go enum change, same doctrine as every other status column in this
// platform.
type PurposeVersionStatus string

const (
	PurposeStatusDraft     PurposeVersionStatus = "DRAFT"
	PurposeStatusPublished PurposeVersionStatus = "PUBLISHED"
)

// Purpose is the stable identity a PurposeVersion belongs to. TenantID nil
// means a platform-wide purpose (Zoiko acting as an independent
// controller — §23.1) rather than a tenant-instructed one; same nullable-
// scope doctrine as retention-registry-svc's retention_policies/legal_holds.
type Purpose struct {
	PurposeID            string    `json:"purpose_id"`
	TenantID             *string   `json:"tenant_id,omitempty"`
	CreatedAt            time.Time `json:"created_at"`
	CreatedByPrincipalID string    `json:"created_by_principal_id"`
}

// PurposeVersion is PRV-I05/I06: every purpose has a stable ID, owner-free
// content, and an effective-dated version; once PUBLISHED it is immutable
// (enforced at the database layer — see migration 000002's trigger).
// Amending a published purpose means creating a new version via
// SupersedesVersionID, never editing this row.
type PurposeVersion struct {
	PurposeVersionID   string   `json:"purpose_version_id"`
	PurposeID          string   `json:"purpose_id"`
	TenantID           *string  `json:"tenant_id,omitempty"`
	Statement          string   `json:"statement"`
	CompatibilityClass string   `json:"compatibility_class"` // data only, e.g. PRIMARY, SECONDARY_COMPATIBLE
	LawfulBasisRefs    []string `json:"lawful_basis_refs"`
	// NecessityRationale is §7's PurposeVersion "necessity evidence" and §8.2
	// gate 3's "necessity/minimization rationale". SpecialConditionRefs are
	// §8.2 gate 4's "special-category/other condition references". Both are
	// content: immutable once PUBLISHED (migration 000002's trigger), and both
	// are enforced at activity validation, not at purpose creation, the same
	// way lawful_basis_refs always has been.
	NecessityRationale     string               `json:"necessity_rationale,omitempty"`
	SpecialConditionRefs   []string             `json:"special_condition_refs"`
	VersionStatus          PurposeVersionStatus `json:"version_status"`
	EffectiveFrom          time.Time            `json:"effective_from"`
	SupersedesVersionID    *string              `json:"supersedes_version_id,omitempty"`
	PublishedByPrincipalID *string              `json:"published_by_principal_id,omitempty"`
	PublishedAt            *time.Time           `json:"published_at,omitempty"`
	CreatedAt              time.Time            `json:"created_at"`
	CreatedByPrincipalID   string               `json:"created_by_principal_id"`
}

// ── Processing Activity ──────────────────────────────────────────────────────

type ActivityVersionStatus string

const (
	ActivityStatusDraft     ActivityVersionStatus = "DRAFT"
	ActivityStatusValidated ActivityVersionStatus = "VALIDATED"
	ActivityStatusSubmitted ActivityVersionStatus = "SUBMITTED"
	ActivityStatusApproved  ActivityVersionStatus = "APPROVED"
	ActivityStatusActive    ActivityVersionStatus = "ACTIVE"
	ActivityStatusSuspended ActivityVersionStatus = "SUSPENDED"
	ActivityStatusRejected  ActivityVersionStatus = "REJECTED"
	ActivityStatusRetired   ActivityVersionStatus = "RETIRED"
)

// activityTransitions is the ONLY set of legal status transitions —
// Figure 4's lifecycle (DRAFT -> VALIDATE -> REVIEW -> APPROVED -> ACTIVE,
// with a reject/fix loop and SUSPENDED/RETIRED branches). REJECTED is a
// dead end by design: the spec's "fix loop" is a NEW version created from
// the rejected one (via CreateActivityVersion), never a resurrection of
// the rejected row itself — consistent with PRV-I20 (no destructive
// rewriting of a governed historical record).
var activityTransitions = map[ActivityVersionStatus][]ActivityVersionStatus{
	ActivityStatusDraft:     {ActivityStatusValidated},
	ActivityStatusValidated: {ActivityStatusSubmitted},
	ActivityStatusSubmitted: {ActivityStatusApproved, ActivityStatusRejected},
	ActivityStatusApproved:  {ActivityStatusActive},
	ActivityStatusActive:    {ActivityStatusSuspended, ActivityStatusRetired},
	ActivityStatusSuspended: {ActivityStatusActive, ActivityStatusRetired},
}

// ValidActivityTransition reports whether to is a legal next status from
// from. Used by both the handler (to return a clear 409 before touching
// the store) and the store (as the authoritative, atomic WHERE-guarded
// check — see PgStore's transition methods).
func ValidActivityTransition(from, to ActivityVersionStatus) bool {
	for _, allowed := range activityTransitions[from] {
		if allowed == to {
			return true
		}
	}
	return false
}

// PrivacyRole is data only (CONTROLLER | PROCESSOR | JOINT_CONTROLLER —
// §4.1's PrivacyRole concept).
type PrivacyRole string

const (
	RoleController      PrivacyRole = "CONTROLLER"
	RoleProcessor       PrivacyRole = "PROCESSOR"
	RoleJointController PrivacyRole = "JOINT_CONTROLLER"
)

func (r PrivacyRole) Valid() bool {
	switch r {
	case RoleController, RoleProcessor, RoleJointController:
		return true
	}
	return false
}

// ProcessingActivity is the stable identity — same nullable-tenant
// doctrine as Purpose.
type ProcessingActivity struct {
	ActivityID           string    `json:"activity_id"`
	TenantID             *string   `json:"tenant_id,omitempty"`
	CreatedAt            time.Time `json:"created_at"`
	CreatedByPrincipalID string    `json:"created_by_principal_id"`
}

// NoticeConsentDependency (§8.2 Gate 7: REQUIRED | NOT_REQUIRED | CONDITIONAL).
type NoticeConsentDependency string

const (
	NoticeConsentRequired    NoticeConsentDependency = "REQUIRED"
	NoticeConsentNotRequired NoticeConsentDependency = "NOT_REQUIRED"
	NoticeConsentConditional NoticeConsentDependency = "CONDITIONAL"
)

func (d NoticeConsentDependency) Valid() bool {
	switch d {
	case NoticeConsentRequired, NoticeConsentNotRequired, NoticeConsentConditional:
		return true
	}
	return false
}

// DPIATIAStatus (§8.2 Gate 8: RESOLVED | REVIEW_REQUIRED | NOT_REQUIRED).
type DPIATIAStatus string

const (
	DPIATIAResolved       DPIATIAStatus = "RESOLVED"
	DPIATIAReviewRequired DPIATIAStatus = "REVIEW_REQUIRED"
	DPIATIANotRequired    DPIATIAStatus = "NOT_REQUIRED"
)

func (s DPIATIAStatus) Valid() bool {
	switch s {
	case DPIATIAResolved, DPIATIAReviewRequired, DPIATIANotRequired:
		return true
	}
	return false
}

// ProcessingActivityVersion is the canonical data model's
// ProcessingActivityVersion (§7): the full record of what is processed,
// why, under what role, for whom, and where. Content fields are
// structurally immutable once the version leaves DRAFT (migration
// 000002 & 000004 triggers) — only VersionStatus (and the fields ValidationFindings/
// EffectiveFrom/lifecycle timestamps that the lifecycle actions
// themselves set) may change after that point. Amending content means
// creating a new version.
type ProcessingActivityVersion struct {
	ActivityVersionID string      `json:"activity_version_id"`
	ActivityID        string      `json:"activity_id"`
	TenantID          *string     `json:"tenant_id,omitempty"`
	PrivacyRole       PrivacyRole `json:"privacy_role"`
	Owner             string      `json:"owner"`         // accountable BUSINESS owner (§8.2 gate 1)
	PrivacyOwner      string      `json:"privacy_owner"` // accountable PRIVACY owner (§8.2 gate 1)
	PurposeIDs        []string    `json:"purpose_ids"`
	SubjectClasses    []string    `json:"subject_classes"`
	DataCategories    []string    `json:"data_categories"`
	Sources           []string    `json:"sources"`
	Recipients        []string    `json:"recipients"`
	Jurisdictions     []string    `json:"jurisdictions"`
	RetentionRuleRefs []string    `json:"retention_rule_refs"`
	TransferRefs      []string    `json:"transfer_refs"`
	// ProcessorRefs is the service-defined representation of §8.2 gate 6's
	// "processor dependencies": references to PRV-05 ProcessorRelationships
	// that carry the documented processing instruction. §7's field list does
	// not name this field — see README "Spec gaps carried as clarifications".
	ProcessorRefs           []string                `json:"processor_refs"`
	NoticeConsentDependency NoticeConsentDependency `json:"notice_consent_dependency,omitempty"`
	DPIATIAStatus           DPIATIAStatus           `json:"dpia_tia_status,omitempty"`
	VersionStatus           ActivityVersionStatus   `json:"version_status"`
	ValidationFindings      []ValidationFinding     `json:"validation_findings,omitempty"`
	RejectionReason         *string                 `json:"rejection_reason,omitempty"`
	// EffectiveFrom/EffectiveTo bound the VALID-time window (§20.3 "when a
	// privacy fact/rule became effective"). EffectiveFrom is the server's
	// activation instant; EffectiveTo is set when the version stops being the
	// governing one (retired, or superseded by a later activation). The
	// KNOWLEDGE-time half ("when ZoikoSuite learned/approved it") is
	// ApprovedAt — see ResolveActivityAsOf.
	EffectiveFrom          *time.Time `json:"effective_from,omitempty"`
	EffectiveTo            *time.Time `json:"effective_to,omitempty"`
	SupersedesVersionID    *string    `json:"supersedes_version_id,omitempty"`
	ApprovedByPrincipalID  *string    `json:"approved_by_principal_id,omitempty"`
	ApprovedAt             *time.Time `json:"approved_at,omitempty"`
	RejectedByPrincipalID  *string    `json:"rejected_by_principal_id,omitempty"`
	RejectedAt             *time.Time `json:"rejected_at,omitempty"`
	ActivatedByPrincipalID *string    `json:"activated_by_principal_id,omitempty"`
	ActivatedAt            *time.Time `json:"activated_at,omitempty"`
	CreatedAt              time.Time  `json:"created_at"`
	CreatedByPrincipalID   string     `json:"created_by_principal_id"`
}

// ── Recognized Jurisdictions & Data Categories Registry (§8.1) ───────────────

// IsValidJurisdiction reports whether j is a registered jurisdiction: an ISO
// 3166-1 alpha-2 country code or one of the recognised regimes (EU, EEA,
// GLOBAL) — see jurisdictions.go. Anything else (free text, malformed codes,
// or two letters that are not an assigned country) is rejected (§8.1). The
// comparison is case-insensitive; stored values are canonicalised by
// NormalizeCode.
func IsValidJurisdiction(j string) bool {
	j = NormalizeCode(j)
	if j == "" {
		return false
	}
	return isoAlpha2[j] || regimeJurisdictions[j]
}

var RecognizedDataCategories = map[string]bool{
	// Standard personal data categories
	"CONTACT_INFO": true, "IDENTIFIERS": true, "FINANCIAL": true,
	"TRANSACTIONAL": true, "EMPLOYMENT": true, "BEHAVIORAL": true,
	"TECHNICAL": true, "LOCATION": true, "COMMUNICATIONS": true,
	"USER_CONTENT": true, "ACCOUNT_CREDENTIALS": true,

	// Registered sensitive / special categories (§8.1, GDPR Art. 9, CPRA, HIPAA)
	"SPECIAL_CATEGORY": true, "HEALTH": true, "BIOMETRIC": true,
	"GENETIC": true, "RACIAL_ETHNIC": true, "POLITICAL_OPINIONS": true,
	"RELIGIOUS_PHILOSOPHICAL": true, "TRADE_UNION": true,
	"SEX_LIFE_ORIENTATION": true, "CRIMINAL_CONVICTIONS": true,
	"CHILD_DATA": true, "HIGH_RISK": true, "VULNERABLE_SUBJECTS": true,
}

// IsValidDataCategory checks whether a category is registered in the taxonomy.
// Arbitrary free-text values are blocked when registry validation is required (§8.1).
func IsValidDataCategory(cat string) bool {
	cat = strings.TrimSpace(strings.ToUpper(cat))
	return RecognizedDataCategories[cat]
}

func IsSensitiveCategory(cat string) bool {
	cat = strings.TrimSpace(strings.ToUpper(cat))
	switch cat {
	case "SPECIAL_CATEGORY", "HEALTH", "BIOMETRIC", "GENETIC", "RACIAL_ETHNIC",
		"POLITICAL_OPINIONS", "RELIGIOUS_PHILOSOPHICAL", "TRADE_UNION",
		"SEX_LIFE_ORIENTATION", "CRIMINAL_CONVICTIONS", "CHILD_DATA", "HIGH_RISK", "VULNERABLE_SUBJECTS":
		return true
	}
	return false
}

// ValidationFinding is one structural problem Validate found — never a
// silent PERMIT (PRV-I13): an activity with any finding stays DRAFT.
type ValidationFinding struct {
	Code    string `json:"code"` // stable code from §32's contract, e.g. PRV-001
	Field   string `json:"field"`
	Message string `json:"message"`
}

// IdempotencyRecord models an idempotency claim for material write operations (§18.1).
type IdempotencyRecord struct {
	IdempotencyKey string    `json:"idempotency_key"`
	TenantID       *string   `json:"tenant_id,omitempty"`
	PrincipalID    string    `json:"principal_id"`
	Operation      string    `json:"operation"`
	RequestHash    string    `json:"request_hash"`
	ResponseStatus int       `json:"response_status"`
	ResponseBody   []byte    `json:"response_body"`
	CreatedAt      time.Time `json:"created_at"`
}

// ── Request DTOs ─────────────────────────────────────────────────────────────

type CreatePurposeRequest struct {
	TenantID             string     `json:"tenant_id,omitempty"`
	Statement            string     `json:"statement"`
	CompatibilityClass   string     `json:"compatibility_class"`
	LawfulBasisRefs      []string   `json:"lawful_basis_refs"`
	NecessityRationale   string     `json:"necessity_rationale,omitempty"`
	SpecialConditionRefs []string   `json:"special_condition_refs"`
	EffectiveFrom        *time.Time `json:"effective_from,omitempty"`
}

type CreatePurposeVersionRequest struct {
	ParentVersionID      string     `json:"parent_version_id"`
	Statement            string     `json:"statement"`
	CompatibilityClass   string     `json:"compatibility_class"`
	LawfulBasisRefs      []string   `json:"lawful_basis_refs"`
	NecessityRationale   string     `json:"necessity_rationale,omitempty"`
	SpecialConditionRefs []string   `json:"special_condition_refs"`
	EffectiveFrom        *time.Time `json:"effective_from,omitempty"`
}

type CreateActivityRequest struct {
	TenantID                string   `json:"tenant_id,omitempty"`
	PrivacyRole             string   `json:"privacy_role"`
	Owner                   string   `json:"owner"`
	PrivacyOwner            string   `json:"privacy_owner"`
	PurposeIDs              []string `json:"purpose_ids"`
	SubjectClasses          []string `json:"subject_classes"`
	DataCategories          []string `json:"data_categories"`
	Sources                 []string `json:"sources"`
	Recipients              []string `json:"recipients"`
	Jurisdictions           []string `json:"jurisdictions"`
	RetentionRuleRefs       []string `json:"retention_rule_refs"`
	TransferRefs            []string `json:"transfer_refs"`
	ProcessorRefs           []string `json:"processor_refs"`
	NoticeConsentDependency string   `json:"notice_consent_dependency,omitempty"`
	DPIATIAStatus           string   `json:"dpia_tia_status,omitempty"`
}

type CreateActivityVersionRequest struct {
	ParentVersionID         string   `json:"parent_version_id"`
	PrivacyRole             string   `json:"privacy_role"`
	Owner                   string   `json:"owner"`
	PrivacyOwner            string   `json:"privacy_owner"`
	PurposeIDs              []string `json:"purpose_ids"`
	SubjectClasses          []string `json:"subject_classes"`
	DataCategories          []string `json:"data_categories"`
	Sources                 []string `json:"sources"`
	Recipients              []string `json:"recipients"`
	Jurisdictions           []string `json:"jurisdictions"`
	RetentionRuleRefs       []string `json:"retention_rule_refs"`
	TransferRefs            []string `json:"transfer_refs"`
	ProcessorRefs           []string `json:"processor_refs"`
	NoticeConsentDependency string   `json:"notice_consent_dependency,omitempty"`
	DPIATIAStatus           string   `json:"dpia_tia_status,omitempty"`
}

type RejectActivityRequest struct {
	Reason string `json:"reason"`
}

type ActivateActivityRequest struct {
	EffectiveFrom *time.Time `json:"effective_from,omitempty"`
}

// ── sentinel errors ──────────────────────────────────────────────────────────

type errorString string

func (e errorString) Error() string { return string(e) }

var (
	ErrPurposeNotFound         = errorString("purpose not found")
	ErrPurposeVersionNotFound  = errorString("purpose version not found")
	ErrPurposeAlreadyPublished = errorString("purpose version is already published")
	ErrActivityNotFound        = errorString("processing activity not found")
	ErrActivityVersionNotFound = errorString("processing activity version not found")
	ErrInvalidTransition       = errorString("invalid activity version status transition")
	ErrStoreUnavailable        = errorString("privacy-purpose-registry store unavailable")
	ErrSelfApprovalForbidden   = errorString("maker-checker segregation of duties violation: creator cannot approve or publish own object")
	ErrIdempotencyKeyReused    = errorString("idempotency key reused with different request payload")
	ErrEffectiveTimeInvalid    = errorString("effective_from must be omitted or equal to the server activation time (no backdating, no future-dating)")
)

// ActivityEventPayload is the payload of every privacy.processing_activity.*
// event (§19: "tenant, activity/version, effective time, purpose/role scope,
// policy refs"). It embeds the full version — so existing consumers reading
// the version fields keep working — and adds the fields §19 names that the
// version row does not carry on its own.
type ActivityEventPayload struct {
	ProcessingActivityVersion
	// PreviousStatus is the status the version left; empty for the first
	// state a version is announced in.
	PreviousStatus ActivityVersionStatus `json:"previous_status,omitempty"`
	// PolicyRefs is the governed policy surface the version operates under:
	// the lawful-basis and special-condition references of its linked
	// published purposes, plus its own retention, transfer and processor
	// references. Never nil, so consumers can range over it unconditionally.
	PolicyRefs []string `json:"policy_refs"`
	// SupersededByVersionID is set on the retired event emitted for a version
	// that stopped governing because a later version was activated.
	SupersededByVersionID *string `json:"superseded_by_version_id,omitempty"`
}
