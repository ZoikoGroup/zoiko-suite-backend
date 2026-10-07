package domain

import (
	"errors"
	"time"
)

// DRC-05 Signature, Seal & Attestation Orchestrator (ZS-SVC-S-001 §7).
// Retrofits signature_envelopes (service.go) with the governed contract
// and orthogonal state dimensions the spec requires: a signature_profile
// an envelope is signed under, provider_attempts tracked independently
// of envelope status (so a timed-out call is resolved by reconciliation,
// never a blind retry), per-signer participant progress, and a sealed
// completion_evidence record once an envelope is SIGNED.

type AssuranceLevel string

const (
	AssuranceSimple    AssuranceLevel = "SES"
	AssuranceAdvanced  AssuranceLevel = "AES"
	AssuranceQualified AssuranceLevel = "QES"
)

func (a AssuranceLevel) Valid() bool {
	switch a {
	case AssuranceSimple, AssuranceAdvanced, AssuranceQualified:
		return true
	}
	return false
}

type IdentityRequirement string

const (
	IdentityEmailOnly IdentityRequirement = "EMAIL_ONLY"
	IdentityKBA       IdentityRequirement = "KBA"
	IdentityGovID     IdentityRequirement = "GOV_ID"
	IdentityVideoID   IdentityRequirement = "VIDEO_ID"
)

func (i IdentityRequirement) Valid() bool {
	switch i {
	case IdentityEmailOnly, IdentityKBA, IdentityGovID, IdentityVideoID:
		return true
	}
	return false
}

type ProfileStatus string

const (
	ProfileDraft   ProfileStatus = "DRAFT"
	ProfileActive  ProfileStatus = "ACTIVE"
	ProfileRetired ProfileStatus = "RETIRED"
)

// SignatureProfile is the governed contract an envelope is signed under —
// assurance level, jurisdiction and identity/witness requirements. DRAFT
// may be edited freely; once ACTIVE its terms are permanent.
type SignatureProfile struct {
	ProfileID            string              `json:"profile_id"`
	TenantID             string              `json:"tenant_id"`
	LegalEntityID        string              `json:"legal_entity_id"`
	AssuranceLevel       AssuranceLevel      `json:"assurance_level"`
	Jurisdiction         string              `json:"jurisdiction"`
	IdentityRequirement  IdentityRequirement `json:"identity_requirement"`
	WitnessRequired      bool                `json:"witness_required"`
	Status               ProfileStatus       `json:"status"`
	CreatedByPrincipalID string              `json:"created_by_principal_id"`
	CreatedAt            time.Time           `json:"created_at"`
	ActivatedAt          *time.Time          `json:"activated_at,omitempty"`
	RetiredAt            *time.Time          `json:"retired_at,omitempty"`
}

type CreateSignatureProfileRequest struct {
	LegalEntityID       string `json:"legal_entity_id"`
	AssuranceLevel      string `json:"assurance_level"`
	Jurisdiction        string `json:"jurisdiction"`
	IdentityRequirement string `json:"identity_requirement"`
	WitnessRequired     bool   `json:"witness_required"`
}

// ProviderAttemptOutcome is one outbound call's own result, tracked
// independently of the envelope's status. UNKNOWN can only be exited via
// ReconcileAttempt — never a bare retry of the original call.
type ProviderAttemptOutcome string

const (
	AttemptPending   ProviderAttemptOutcome = "PENDING"
	AttemptSucceeded ProviderAttemptOutcome = "SUCCEEDED"
	AttemptFailed    ProviderAttemptOutcome = "FAILED"
	AttemptUnknown   ProviderAttemptOutcome = "UNKNOWN"
)

func (o ProviderAttemptOutcome) Valid() bool {
	switch o {
	case AttemptPending, AttemptSucceeded, AttemptFailed, AttemptUnknown:
		return true
	}
	return false
}

type ProviderAttemptAction string

const (
	AttemptActionSend        ProviderAttemptAction = "SEND"
	AttemptActionVoid        ProviderAttemptAction = "VOID"
	AttemptActionCheckStatus ProviderAttemptAction = "CHECK_STATUS"
)

func (a ProviderAttemptAction) Valid() bool {
	switch a {
	case AttemptActionSend, AttemptActionVoid, AttemptActionCheckStatus:
		return true
	}
	return false
}

type ProviderAttempt struct {
	AttemptID             string                 `json:"attempt_id"`
	TenantID              string                 `json:"tenant_id"`
	EnvelopeID            string                 `json:"envelope_id"`
	IdempotencyKey        string                 `json:"idempotency_key"`
	Provider              string                 `json:"provider"`
	AttemptedAction       ProviderAttemptAction  `json:"attempted_action"`
	Outcome               ProviderAttemptOutcome `json:"outcome"`
	ProviderResponseRef   string                 `json:"provider_response_ref,omitempty"`
	ErrorDetail           string                 `json:"error_detail,omitempty"`
	AttemptedAt           time.Time              `json:"attempted_at"`
	ResolvedAt            *time.Time             `json:"resolved_at,omitempty"`
	ResolvedByPrincipalID *string                `json:"resolved_by_principal_id,omitempty"`
}

type RecordProviderAttemptRequest struct {
	IdempotencyKey      string `json:"idempotency_key"`
	Provider            string `json:"provider"`
	AttemptedAction     string `json:"attempted_action"`
	Outcome             string `json:"outcome"`
	ProviderResponseRef string `json:"provider_response_ref"`
	ErrorDetail         string `json:"error_detail"`
}

type ReconcileAttemptRequest struct {
	Outcome             string `json:"outcome"`
	ProviderResponseRef string `json:"provider_response_ref"`
}

// ParticipantState is one signer's own progress, independent of the
// envelope's overall status.
type ParticipantState string

const (
	ParticipantInvited  ParticipantState = "INVITED"
	ParticipantViewed   ParticipantState = "VIEWED"
	ParticipantSigned   ParticipantState = "SIGNED"
	ParticipantDeclined ParticipantState = "DECLINED"
)

type ParticipantRole string

const (
	RoleSigner   ParticipantRole = "SIGNER"
	RoleWitness  ParticipantRole = "WITNESS"
	RoleApprover ParticipantRole = "APPROVER"
	RoleCC       ParticipantRole = "CC"
)

func (r ParticipantRole) Valid() bool {
	switch r {
	case RoleSigner, RoleWitness, RoleApprover, RoleCC:
		return true
	}
	return false
}

type Participant struct {
	ParticipantID    string           `json:"participant_id"`
	TenantID         string           `json:"tenant_id"`
	EnvelopeID       string           `json:"envelope_id"`
	Email            string           `json:"email"`
	Name             string           `json:"name"`
	Role             ParticipantRole  `json:"role"`
	ParticipantState ParticipantState `json:"participant_state"`
	InvitedAt        time.Time        `json:"invited_at"`
	ViewedAt         *time.Time       `json:"viewed_at,omitempty"`
	SignedAt         *time.Time       `json:"signed_at,omitempty"`
	DeclinedAt       *time.Time       `json:"declined_at,omitempty"`
	DeclineReason    string           `json:"decline_reason,omitempty"`
}

type AddParticipantRequest struct {
	Email string `json:"email"`
	Name  string `json:"name"`
	Role  string `json:"role"`
}

// CompletionEvidence is the sealed, durable record of what the provider
// actually returned once an envelope is SIGNED — never mutated once
// written.
type CompletionEvidence struct {
	EvidenceID               string    `json:"evidence_id"`
	TenantID                 string    `json:"tenant_id"`
	EnvelopeID               string    `json:"envelope_id"`
	CompletionCertificateRef string    `json:"completion_certificate_ref"`
	CompletedArtifactHash    string    `json:"completed_artifact_hash"`
	SealedAt                 time.Time `json:"sealed_at"`
	SealedByPrincipalID      string    `json:"sealed_by_principal_id"`
}

type SealCompletionEvidenceRequest struct {
	CompletionCertificateRef string `json:"completion_certificate_ref"`
	CompletedArtifactHash    string `json:"completed_artifact_hash"`
}

type AmendEnvelopeRequest struct {
	NewEnvelope CreateEnvelopeRequest `json:"new_envelope"`
}

// ── errors ───────────────────────────────────────────────────────────────────

var (
	ErrSignatureProfileNotFound   = errors.New("signature profile not found")
	ErrInvalidAssuranceLevel      = errors.New("invalid assurance_level")
	ErrInvalidIdentityRequirement = errors.New("invalid identity_requirement")
	ErrSignatureProfileNotDraft   = errors.New("signature profile is not DRAFT")
	ErrSignatureProfileNotActive  = errors.New("signature profile is not ACTIVE")

	ErrEnvelopeAlreadyBoundToProfile = errors.New("this envelope is already bound to a signature profile")

	ErrProviderAttemptNotFound     = errors.New("provider attempt not found")
	ErrInvalidAttemptOutcome       = errors.New("invalid outcome")
	ErrInvalidAttemptAction        = errors.New("invalid attempted_action")
	ErrAttemptNotUnknown           = errors.New("provider attempt is not UNKNOWN; only an UNKNOWN attempt can be reconciled")
	ErrReconcileOutcomeMustBeFinal = errors.New("reconciled outcome must be SUCCEEDED or FAILED")

	ErrParticipantNotFound      = errors.New("participant not found")
	ErrInvalidParticipantRole   = errors.New("invalid role")
	ErrParticipantNotInvited    = errors.New("participant is not INVITED")
	ErrParticipantNotViewable   = errors.New("participant cannot be marked viewed from its current state")
	ErrParticipantNotSignable   = errors.New("participant cannot be marked signed from its current state")
	ErrParticipantNotDeclinable = errors.New("participant cannot be declined from its current state")

	ErrCompletionEvidenceExists = errors.New("completion evidence already sealed for this envelope")
	ErrEnvelopeNotSigned        = errors.New("envelope is not SIGNED; completion evidence can only be sealed for a signed envelope")

	ErrEnvelopeNotAmendable = errors.New("envelope is not in a voidable state and cannot be amended")
)
