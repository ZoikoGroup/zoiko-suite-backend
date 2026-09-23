package domain

import (
	"errors"
	"time"
)

// BIZ-06 CRM / Relationship — a second bolted-on domain in this
// service, beside Counterparty. Counterparty already owns external
// party identity (including a CUSTOMER type) — exactly the "ORG party
// identity" BIZ-06 is meant to reference, not own. Relationship is the
// real CRM entity; Interaction/Opportunity/ConsentRecord/
// CommercialObjectLink hang off it.
//
// Several commands here fill real gaps the doc's own command list
// leaves open — named states/events with nothing that reaches them —
// same class of gap as BIZ-04's missing RetireForm and BIZ-05's missing
// CreateCase:
//   - ConfirmPartyLink: the doc's own failure semantics require a
//     duplicate-party match to remain an unresolved candidate until ORG
//     linkage is confirmed, but no command does the confirming.
//   - ActivateRelationship / MarkDormant / CloseRelationship: the
//     lifecycle names Prospect->Active->Dormant->Closed and an event
//     RelationshipClosed, but no command reaches any of those states.
//   - RecordConsent: consent is a required input and GetConsentContext
//     is a named query, but no command writes it.
//
// Pipeline stages are a fixed, validated set — NOT a separately
// governed pipeline-definition entity. The doc says opportunity stages
// are "versioned per approved pipeline" but names no command that
// manages stage definitions, only UpdateStage (moving an opportunity
// along the pipeline). Building a full governed pipeline-config
// subsystem nothing names would be fabricating scope; "versioned" is
// satisfied honestly via OpportunityStageTransition's own full
// append-only history instead.

type PartyLinkStatus string

const (
	PartyLinkUnlinked  PartyLinkStatus = "UNLINKED"
	PartyLinkCandidate PartyLinkStatus = "CANDIDATE"
	PartyLinkConfirmed PartyLinkStatus = "CONFIRMED"
)

type RelationshipStatus string

const (
	RelationshipProspect RelationshipStatus = "PROSPECT"
	RelationshipActive   RelationshipStatus = "ACTIVE"
	RelationshipDormant  RelationshipStatus = "DORMANT"
	RelationshipClosed   RelationshipStatus = "CLOSED"
	RelationshipMerged   RelationshipStatus = "MERGED"
)

type Relationship struct {
	RelationshipID           string             `json:"relationship_id"`
	TenantID                 string             `json:"tenant_id"`
	LegalEntityID            string             `json:"legal_entity_id"`
	CounterpartyID           *string            `json:"counterparty_id,omitempty"`
	CandidateCounterpartyID  *string            `json:"candidate_counterparty_id,omitempty"`
	PartyLinkStatus          PartyLinkStatus    `json:"party_link_status"`
	Status                   RelationshipStatus `json:"status"`
	Source                   string             `json:"source,omitempty"`
	Channel                  string             `json:"channel,omitempty"`
	OwnerPrincipalID         string             `json:"owner_principal_id,omitempty"`
	MergedIntoRelationshipID *string            `json:"merged_into_relationship_id,omitempty"`
	CreatedBy                string             `json:"created_by"`
	CreatedAt                time.Time          `json:"created_at"`
	UpdatedAt                time.Time          `json:"updated_at"`
	ClosedBy                 string             `json:"closed_by,omitempty"`
	ClosedAt                 *time.Time         `json:"closed_at,omitempty"`
	ClosureReason            string             `json:"closure_reason,omitempty"`
}

type Interaction struct {
	InteractionID   string    `json:"interaction_id"`
	RelationshipID  string    `json:"relationship_id"`
	TenantID        string    `json:"tenant_id"`
	InteractionType string    `json:"interaction_type"`
	Channel         string    `json:"channel,omitempty"`
	Notes           string    `json:"notes,omitempty"`
	OccurredAt      time.Time `json:"occurred_at"`
	LoggedBy        string    `json:"logged_by"`
	CreatedAt       time.Time `json:"created_at"`
}

// OpportunityStage values — a fixed, validated set. See this file's own
// package doc comment on why this is not a separately governed
// pipeline-definition entity.
type OpportunityStage string

const (
	StageLead        OpportunityStage = "LEAD"
	StageQualified   OpportunityStage = "QUALIFIED"
	StageProposal    OpportunityStage = "PROPOSAL"
	StageNegotiation OpportunityStage = "NEGOTIATION"
	StageClosedWon   OpportunityStage = "CLOSED_WON"
	StageClosedLost  OpportunityStage = "CLOSED_LOST"
)

type OpportunityStatus string

const (
	OpportunityOpen   OpportunityStatus = "OPEN"
	OpportunityClosed OpportunityStatus = "CLOSED"
)

type Opportunity struct {
	OpportunityID    string            `json:"opportunity_id"`
	RelationshipID   string            `json:"relationship_id"`
	TenantID         string            `json:"tenant_id"`
	LegalEntityID    string            `json:"legal_entity_id"`
	Name             string            `json:"name"`
	Value            float64           `json:"value"`
	Currency         string            `json:"currency"`
	Stage            OpportunityStage  `json:"stage"`
	Status           OpportunityStatus `json:"status"`
	OwnerPrincipalID string            `json:"owner_principal_id,omitempty"`
	CreatedBy        string            `json:"created_by"`
	CreatedAt        time.Time         `json:"created_at"`
	UpdatedAt        time.Time         `json:"updated_at"`
	ClosedBy         string            `json:"closed_by,omitempty"`
	ClosedAt         *time.Time        `json:"closed_at,omitempty"`
	CloseReason      string            `json:"close_reason,omitempty"`
}

// OpportunityStageTransition is the append-only "versioned pipeline"
// record — one row per UpdateStage call.
type OpportunityStageTransition struct {
	TransitionID     string    `json:"transition_id"`
	OpportunityID    string    `json:"opportunity_id"`
	TenantID         string    `json:"tenant_id"`
	FromStage        string    `json:"from_stage"`
	ToStage          string    `json:"to_stage"`
	ActorPrincipalID string    `json:"actor_principal_id"`
	Reason           string    `json:"reason,omitempty"`
	OccurredAt       time.Time `json:"occurred_at"`
}

type ConsentStatus string

const (
	ConsentGranted   ConsentStatus = "GRANTED"
	ConsentWithdrawn ConsentStatus = "WITHDRAWN"
)

// ConsentRecord is append-only — each decision is a new row; the latest
// row per (relationship_id, consent_type) is the current answer.
type ConsentRecord struct {
	ConsentID      string        `json:"consent_id"`
	RelationshipID string        `json:"relationship_id"`
	TenantID       string        `json:"tenant_id"`
	ConsentType    string        `json:"consent_type"`
	ConsentStatus  ConsentStatus `json:"consent_status"`
	Basis          string        `json:"basis,omitempty"`
	RecordedBy     string        `json:"recorded_by"`
	RecordedAt     time.Time     `json:"recorded_at"`
}

// CommercialObjectLink is an opaque cross-service reference — no FK,
// same posture as document-vault-svc's document_links.
type CommercialObjectLink struct {
	LinkID           string    `json:"link_id"`
	RelationshipID   string    `json:"relationship_id"`
	TenantID         string    `json:"tenant_id"`
	LinkedObjectType string    `json:"linked_object_type"`
	LinkedObjectID   string    `json:"linked_object_id"`
	LinkedBy         string    `json:"linked_by"`
	LinkedAt         time.Time `json:"linked_at"`
}

// ── params ───────────────────────────────────────────────────────────────────

// CreateRelationshipParams's CandidateCounterpartyID (optional) records
// a tentative, unconfirmed party match — never auto-promoted. Leave
// both CounterpartyID-related fields empty to create an unlinked
// relationship (e.g. before any party identity is known at all).
type CreateRelationshipParams struct {
	TenantID, LegalEntityID           string
	CandidateCounterpartyID           string
	Source, Channel, OwnerPrincipalID string
	CreatedByPrincipalID              string
}

// ConfirmPartyLinkParams — a gap-fill; see this file's own doc comment.
type ConfirmPartyLinkParams struct {
	RelationshipID, TenantID, ActorPrincipalID, CounterpartyID string
}

type LogInteractionParams struct {
	RelationshipID, TenantID, InteractionType, Channel, Notes, LoggedByPrincipalID string
	OccurredAt                                                                     *time.Time
}

type CreateOpportunityParams struct {
	RelationshipID, TenantID, LegalEntityID, Name, Currency, OwnerPrincipalID string
	Value                                                                     float64
	CreatedByPrincipalID                                                      string
}

type UpdateStageParams struct {
	OpportunityID, TenantID, ActorPrincipalID string
	Stage                                     OpportunityStage
	Reason                                    string
}

// ── errors ───────────────────────────────────────────────────────────────────

var (
	ErrRelationshipNotFound     = errors.New("relationship not found")
	ErrRelationshipInvalidState = errors.New("relationship is not in a state that permits this action")
	ErrOpportunityNotFound      = errors.New("opportunity not found")
	ErrOpportunityInvalidState  = errors.New("opportunity is not in a state that permits this action")
	ErrPartyLinkNotCandidate    = errors.New("relationship has no candidate party link to confirm")
	ErrInvalidStage             = errors.New("invalid opportunity stage")
)
