package domain

import "time"

// This file holds the ORG-02 (Tenant) and ORG-03 (Legal Entity) types added to
// close the named-command and as-of gaps in
// ZoikoSuite_Organization_Legal_Entity_Global_Reference_Data_Detailed_Service_
// Specifications §4.2 and §4.3. It is separate from types.go only so the
// original data-model §05.1 shapes stay legible as one block.

// ---------------------------------------------------------------------------
// ORG-03 — LegalEntityProfileVersion (bitemporal)
// ---------------------------------------------------------------------------

// LegalEntityProfileVersion is one effective-dated version of an entity's legal
// profile.
//
// The distinction that makes this type worth having: EffectiveFrom/EffectiveTo
// are BUSINESS time — when the fact was true in the world — while RecordedAt is
// RECORD time, when this platform learned it. A backdated name change has a
// RecordedAt later than its EffectiveFrom, and §9.2's requirement that as-of
// retrieval be "verified against correction and late-arriving-change
// scenarios" is precisely the case where the two disagree.
//
// SupersededAt marks a version corrected by a later one. The row is never
// rewritten or deleted: §8 NP6 requires that history resolve the ORIGINAL
// version, which is impossible if a correction overwrites what it corrects.
type LegalEntityProfileVersion struct {
	ProfileVersionID string `json:"profile_version_id"`
	TenantID         string `json:"tenant_id"`
	LegalEntityID    string `json:"legal_entity_id"`
	VersionNumber    int    `json:"version_number"`

	LegalName                   string  `json:"legal_name"`
	TradingName                 *string `json:"trading_name"`
	LegalFormCode               *string `json:"legal_form_code"`
	LegalFormSource             *string `json:"legal_form_source"`
	LegalFormLocalText          *string `json:"legal_form_local_text"`
	RegistrationNumber          *string `json:"registration_number"`
	RegistryAuthority           *string `json:"registry_authority"`
	RegisteredOffice            *string `json:"registered_office"`
	IncorporationJurisdictionID *string `json:"incorporation_jurisdiction_id"`
	DefaultCurrencyCode         *string `json:"default_currency_code"`

	EffectiveFrom time.Time  `json:"effective_from"`
	EffectiveTo   *time.Time `json:"effective_to"`

	RecordedAt   time.Time  `json:"recorded_at"`
	SupersededAt *time.Time `json:"superseded_at"`

	ChangeReason          ProfileChangeReason `json:"change_reason"`
	SourceEvidenceRef     *string             `json:"source_evidence_ref"`
	CreatedByPrincipalID  string              `json:"created_by_principal_id"`
	ApprovedByPrincipalID *string             `json:"approved_by_principal_id"`
}

// ProfileChangeReason names why a profile version exists. It is stored rather
// than inferred, because §4.3's evidence requirement is "verified fields,
// legal-form code/source, effective/recorded time, approver" — a reader of the
// history needs to know whether version 3 exists because the company renamed
// itself or because version 2 recorded the name wrongly. Those are different
// facts with different downstream consequences.
type ProfileChangeReason string

const (
	// ProfileChangeInitial is the version created alongside the entity.
	ProfileChangeInitial ProfileChangeReason = "INITIAL"
	// ProfileChangeInitialBackfill marks versions synthesised by migration
	// 000006 for entities that predate profile versioning. Distinct from
	// INITIAL so a reader can tell a real recorded creation from a
	// reconstruction.
	ProfileChangeInitialBackfill ProfileChangeReason = "INITIAL_BACKFILL"
	// ProfileChangeLegalNameChange — the entity renamed itself. The prior name
	// remains correct for the period it covers.
	ProfileChangeLegalNameChange ProfileChangeReason = "LEGAL_NAME_CHANGE"
	// ProfileChangeRegisteredOfficeChange — ChangeRegisteredOffice.
	ProfileChangeRegisteredOfficeChange ProfileChangeReason = "REGISTERED_OFFICE_CHANGE"
	// ProfileChangeAmendment — AmendLegalProfile, any other profile field.
	ProfileChangeAmendment ProfileChangeReason = "AMENDMENT"
	// ProfileChangeCorrection — the prior version was WRONG, not superseded.
	// The difference matters downstream: a correction means reports already
	// issued against the prior version were misstated.
	ProfileChangeCorrection ProfileChangeReason = "CORRECTION"
)

// ValidProfileChangeReason reports whether r is one this service writes.
func ValidProfileChangeReason(r ProfileChangeReason) bool {
	switch r {
	case ProfileChangeInitial, ProfileChangeInitialBackfill,
		ProfileChangeLegalNameChange, ProfileChangeRegisteredOfficeChange,
		ProfileChangeAmendment, ProfileChangeCorrection:
		return true
	}
	return false
}

// ---------------------------------------------------------------------------
// ORG-02 — Tenant lifecycle history and named commands
// ---------------------------------------------------------------------------

// TenantLifecycleEvent is one recorded lifecycle transition.
type TenantLifecycleEvent struct {
	LifecycleEventID string                `json:"lifecycle_event_id"`
	TenantID         string                `json:"tenant_id"`
	FromState        *TenantLifecycleState `json:"from_state"`
	ToState          TenantLifecycleState  `json:"to_state"`
	CommandName      TenantCommand         `json:"command_name"`
	Reason           string                `json:"reason"`

	ActorPrincipalID      string    `json:"actor_principal_id"`
	ApprovedByPrincipalID *string   `json:"approved_by_principal_id"`
	CorrelationID         *string   `json:"correlation_id"`
	OccurredAt            time.Time `json:"occurred_at"`
}

// TenantCommand is an ORG-02 §4.2 named command.
//
// The DoD gate these exist for is "No generic path bypasses named governance
// commands". A single generic TransitionTenantLifecycle(target) endpoint
// satisfies the state machine but not that gate: the evidence record cannot
// say which governance command was invoked, only where the tenant ended up,
// and Suspend-then-Resume is indistinguishable from a correction of a mistaken
// suspension.
type TenantCommand string

const (
	TenantCommandCreate              TenantCommand = "CreateTenant"
	TenantCommandActivate            TenantCommand = "ActivateTenant"
	TenantCommandSuspend             TenantCommand = "SuspendTenant"
	TenantCommandResume              TenantCommand = "ResumeTenant"
	TenantCommandInitiateTermination TenantCommand = "InitiateTermination"
	TenantCommandCompleteTermination TenantCommand = "CompleteTermination"
	TenantCommandChangeDefaultLocale TenantCommand = "ChangeDefaultLocale"
)

// TargetState returns the lifecycle state this command moves a tenant to, and
// the states it may legitimately be invoked from.
//
// Keeping the mapping here rather than in the service is what lets the handler
// reject an unknown command before any authorization or database work, and
// what lets a test enumerate the commands rather than restate them.
//
// ChangeDefaultLocale returns ok=false: it is a named ORG-02 command but not a
// lifecycle transition, and callers must not route it through the lifecycle
// path.
func (c TenantCommand) TargetState() (target TenantLifecycleState, from []TenantLifecycleState, ok bool) {
	switch c {
	case TenantCommandActivate:
		// ONBOARDING is the state ProvisionTenant leaves a tenant in.
		return TenantLifecycleActive, []TenantLifecycleState{TenantLifecycleOnboarding}, true
	case TenantCommandSuspend:
		return TenantLifecycleSuspended, []TenantLifecycleState{TenantLifecycleActive}, true
	case TenantCommandResume:
		return TenantLifecycleActive, []TenantLifecycleState{TenantLifecycleSuspended}, true
	case TenantCommandInitiateTermination:
		// Reachable from ACTIVE or SUSPENDED: a suspended tenant is exactly
		// the one most likely to be terminated, and forcing it through
		// ResumeTenant first would require reactivating a tenant in order to
		// end it.
		return TenantLifecycleOffboarding, []TenantLifecycleState{TenantLifecycleActive, TenantLifecycleSuspended}, true
	case TenantCommandCompleteTermination:
		return TenantLifecycleTerminated, []TenantLifecycleState{TenantLifecycleOffboarding}, true
	}
	return "", nil, false
}

// AuthzAction is the action this command is authorized against.
//
// Derived here rather than by lowercasing the command name at the call site,
// which produced TENANT_LIFECYCLE_SUSPENDTENANT once authorization-svc
// normalised it. That is a grant an operator has to transcribe by hand into an
// RBAC console, and the estate's own bundles are named PO_ISSUE, GL_JOURNAL_POST
// and so on — one word per concept. These produce TENANT_SUSPEND,
// TENANT_INITIATE_TERMINATION and the rest.
//
// Each command keeps its OWN action rather than sharing one
// "tenant lifecycle" permission, because the commands differ in severity:
// granting someone the ability to suspend a tenant during an incident should
// not also grant them the ability to terminate it.
func (c TenantCommand) AuthzAction() string {
	switch c {
	case TenantCommandActivate:
		return "activate"
	case TenantCommandSuspend:
		return "suspend"
	case TenantCommandResume:
		return "resume"
	case TenantCommandInitiateTermination:
		return "initiate-termination"
	case TenantCommandCompleteTermination:
		return "complete-termination"
	case TenantCommandChangeDefaultLocale:
		return "defaults.change"
	case TenantCommandCreate:
		return "provision"
	}
	// An unknown command never reaches authorization — TargetState() refuses it
	// first — but returning the raw name rather than "" means that if one ever
	// did, it would be evaluated against an action no grant matches and be
	// denied, rather than against the empty action.
	return string(c)
}

// RequiresMakerChecker reports whether §4.2's SoD rule applies to this command:
// "Tenant creation/termination and home-region changes require maker-checker
// for controlled environments."
//
// Suspension is deliberately NOT in this set. Suspending a tenant is a
// containment action — commonly taken during an incident — and requiring a
// second approver would mean a compromised tenant stays live until one is
// found. Resuming it, which restores access, is a different matter, but it is
// reachable only from SUSPENDED and so cannot be used to escalate.
func (c TenantCommand) RequiresMakerChecker() bool {
	switch c {
	case TenantCommandInitiateTermination, TenantCommandCompleteTermination:
		return true
	}
	return false
}

// ExecuteTenantCommandRequest is the body of POST /v1/tenants/{id}/commands/{command}.
type ExecuteTenantCommandRequest struct {
	// Reason is mandatory. §4.2 requires "lifecycle actor/reason" as evidence,
	// and a lifecycle history whose reason column is empty answers the
	// question "why is this tenant suspended?" with silence.
	Reason string `json:"reason"`
	// ExpectedVersion is the tenant's record_version as the caller last saw it.
	// Zero means "no expectation" and is accepted — the spec requires the
	// mechanism to exist and be honoured when supplied, and a hard requirement
	// would break every existing caller at once.
	ExpectedVersion int64 `json:"expected_version"`
	// ApprovedByPrincipalID is the second party for maker-checker commands.
	// It must differ from the acting principal; the database CHECK enforces
	// that independently of the service.
	ApprovedByPrincipalID string `json:"approved_by_principal_id"`
	CorrelationID         string `json:"correlation_id"`
}

// ChangeDefaultLocaleRequest is the body of the ChangeDefaultLocale command.
type ChangeDefaultLocaleRequest struct {
	PrimaryLocale   string `json:"primary_locale"`
	PrimaryTimezone string `json:"primary_timezone"`
	Reason          string `json:"reason"`
	ExpectedVersion int64  `json:"expected_version"`
	CorrelationID   string `json:"correlation_id"`
}

// TenantDefaults is the ORG-02 GetTenantDefaults read surface — the baseline
// tenancy configuration a consumer needs without fetching the whole tenant and
// without being handed its lifecycle state as though it were configuration.
type TenantDefaults struct {
	TenantID                     string `json:"tenant_id"`
	DefaultCurrencyCode          string `json:"default_currency_code"`
	PrimaryTimezone              string `json:"primary_timezone"`
	PrimaryLocale                string `json:"primary_locale"`
	DefaultDataResidencyPolicyID string `json:"default_data_residency_policy_id"`
	RecordVersion                int64  `json:"record_version"`
}

// TenantHostBinding maps a hostname to the tenant it belongs to.
type TenantHostBinding struct {
	HostBindingID        string    `json:"host_binding_id"`
	Hostname             string    `json:"hostname"`
	TenantID             string    `json:"tenant_id"`
	IsPrimary            bool      `json:"is_primary"`
	ActiveFlag           bool      `json:"active_flag"`
	CreatedAt            time.Time `json:"created_at"`
	CreatedByPrincipalID string    `json:"created_by_principal_id"`
}

// BindTenantHostRequest is the body of POST /v1/tenants/{id}/host-bindings.
type BindTenantHostRequest struct {
	Hostname      string `json:"hostname"`
	IsPrimary     bool   `json:"is_primary"`
	CorrelationID string `json:"correlation_id"`
}

// ResolvedTenantByHost is the ResolveTenantByHost result.
//
// It deliberately returns the tenant's status and lifecycle state alongside the
// id. The caller resolving a hostname is an ingress layer deciding whether to
// admit the request at all, and giving it only an id would force a second
// round trip to learn the tenant is suspended.
type ResolvedTenantByHost struct {
	Hostname       string               `json:"hostname"`
	TenantID       string               `json:"tenant_id"`
	TenantCode     string               `json:"tenant_code"`
	Status         TenantStatus         `json:"status"`
	LifecycleState TenantLifecycleState `json:"lifecycle_state"`
	IsPrimary      bool                 `json:"is_primary"`
}

// ---------------------------------------------------------------------------
// ORG-03 — named commands
// ---------------------------------------------------------------------------

// AmendLegalProfileRequest creates a new effective-dated profile version.
//
// Every profile field is a pointer: nil means "unchanged, carry forward from
// the version in force", which is what makes this an amendment rather than a
// replacement. A non-pointer field could not distinguish "set trading name to
// empty" from "do not touch the trading name", and ORG-03 profiles have
// optional fields where both are meaningful.
type AmendLegalProfileRequest struct {
	LegalName          *string `json:"legal_name"`
	TradingName        *string `json:"trading_name"`
	LegalFormCode      *string `json:"legal_form_code"`
	LegalFormSource    *string `json:"legal_form_source"`
	LegalFormLocalText *string `json:"legal_form_local_text"`
	RegistrationNumber *string `json:"registration_number"`
	RegistryAuthority  *string `json:"registry_authority"`
	// RegisteredOffice is a structured address held as opaque JSON. ORG-08 owns
	// address normalization; §1 is explicit that storing one here must not
	// become a second address authority, so this service records what it was
	// told and draws no conclusions from it.
	RegisteredOffice            *string `json:"registered_office"`
	IncorporationJurisdictionID *string `json:"incorporation_jurisdiction_id"`
	DefaultCurrencyCode         *string `json:"default_currency_code"`

	// EffectiveFrom is BUSINESS time and may legitimately be in the past — a
	// registry filing learned about a week late is effective from the filing
	// date, not from when this service heard about it.
	EffectiveFrom time.Time `json:"effective_from"`

	ChangeReason      ProfileChangeReason `json:"change_reason"`
	SourceEvidenceRef string              `json:"source_evidence_ref"`
	// ApprovedByPrincipalID is required for the changes §4.3 puts under SoD:
	// "Maker cannot approve legal-name/registry/jurisdiction change".
	ApprovedByPrincipalID string `json:"approved_by_principal_id"`
	ExpectedVersion       int64  `json:"expected_version"`
	CorrelationID         string `json:"correlation_id"`
}

// RequiresApproval reports whether this amendment touches a field §4.3 places
// under segregation of duties.
func (r AmendLegalProfileRequest) RequiresApproval() bool {
	return r.LegalName != nil ||
		r.RegistrationNumber != nil ||
		r.IncorporationJurisdictionID != nil
}

// ChangeLegalNameRequest is the narrow ChangeLegalName command.
type ChangeLegalNameRequest struct {
	LegalName             string    `json:"legal_name"`
	EffectiveFrom         time.Time `json:"effective_from"`
	SourceEvidenceRef     string    `json:"source_evidence_ref"`
	ApprovedByPrincipalID string    `json:"approved_by_principal_id"`
	ExpectedVersion       int64     `json:"expected_version"`
	CorrelationID         string    `json:"correlation_id"`
}

// ChangeRegisteredOfficeRequest is the narrow ChangeRegisteredOffice command.
type ChangeRegisteredOfficeRequest struct {
	RegisteredOffice      string    `json:"registered_office"`
	EffectiveFrom         time.Time `json:"effective_from"`
	SourceEvidenceRef     string    `json:"source_evidence_ref"`
	ApprovedByPrincipalID string    `json:"approved_by_principal_id"`
	ExpectedVersion       int64     `json:"expected_version"`
	CorrelationID         string    `json:"correlation_id"`
}

// EntityAsOf is the GetLegalEntityAsOf result: the entity's current identity
// paired with the profile version that was in force at the requested instant.
//
// Both are returned because they answer different questions and a caller
// reconstructing a historical report needs both. The entity carries identifiers
// that do not change (id, code, tenant); the profile carries the facts that do.
type EntityAsOf struct {
	LegalEntityID string                     `json:"legal_entity_id"`
	TenantID      string                     `json:"tenant_id"`
	EntityCode    string                     `json:"entity_code"`
	EntityType    EntityType                 `json:"entity_type"`
	EntityStatus  EntityStatus               `json:"entity_status"`
	AsOf          time.Time                  `json:"as_of"`
	Profile       *LegalEntityProfileVersion `json:"profile"`
}

// ---------------------------------------------------------------------------
// ORG-03 — registry conflict quarantine (§8 NP5)
// ---------------------------------------------------------------------------

// EntityRegistryConflict records a rejected attempt to claim a registry
// identity another active entity already holds in the same jurisdiction.
type EntityRegistryConflict struct {
	ConflictID            string                 `json:"conflict_id"`
	TenantID              string                 `json:"tenant_id"`
	RegistrationNumber    string                 `json:"registration_number"`
	JurisdictionID        string                 `json:"jurisdiction_id"`
	ExistingLegalEntityID string                 `json:"existing_legal_entity_id"`
	AttemptedPayload      map[string]any         `json:"attempted_payload"`
	Status                RegistryConflictStatus `json:"status"`

	ResolutionNote        *string    `json:"resolution_note"`
	ResolvedByPrincipalID *string    `json:"resolved_by_principal_id"`
	ResolvedAt            *time.Time `json:"resolved_at"`

	DetectedAt            time.Time `json:"detected_at"`
	DetectedByPrincipalID string    `json:"detected_by_principal_id"`
	CorrelationID         *string   `json:"correlation_id"`
}

// RegistryConflictStatus is the resolution state of a quarantined conflict.
type RegistryConflictStatus string

const (
	RegistryConflictOpen RegistryConflictStatus = "OPEN"
	// RegistryConflictResolvedDistinct — a human confirmed the two are
	// genuinely different entities that happen to share a registry number
	// (which happens across registry authorities within one jurisdiction).
	RegistryConflictResolvedDistinct RegistryConflictStatus = "RESOLVED_DISTINCT"
	// RegistryConflictResolvedDuplicate — confirmed the same entity. §1 is
	// explicit that "destructive merge is prohibited", so this status records
	// the conclusion; it does not itself merge anything.
	RegistryConflictResolvedDuplicate RegistryConflictStatus = "RESOLVED_DUPLICATE"
	RegistryConflictDismissed         RegistryConflictStatus = "DISMISSED"
)

// ValidRegistryConflictResolution reports whether s is a terminal status a
// resolver may set. OPEN is excluded: reopening a resolved conflict is not a
// resolution.
func ValidRegistryConflictResolution(s RegistryConflictStatus) bool {
	switch s {
	case RegistryConflictResolvedDistinct, RegistryConflictResolvedDuplicate, RegistryConflictDismissed:
		return true
	}
	return false
}

// ResolveRegistryConflictRequest is the body of the conflict resolution route.
type ResolveRegistryConflictRequest struct {
	Status         RegistryConflictStatus `json:"status"`
	ResolutionNote string                 `json:"resolution_note"`
	CorrelationID  string                 `json:"correlation_id"`
}
