package domain

// ORG-04 Corporate Group & Ownership (ZS-SVC-I-001 s4.4): effective-dated
// ownership, voting and control relationships between legal entities.
//
// This is not EntityHierarchy. A hierarchy row is a bare parent/child link; an
// OwnershipRelationship states how much, of what kind, on whose evidence, and
// who approved it. The service owns the relationship; consolidation consumes it
// and decides what it means for the consolidation method (s4.4 non-ownership).

import (
	"errors"
	"fmt"
	"strings"
	"time"
)

// RelationshipKind is what the relationship is: equity or control.
type RelationshipKind string

const (
	RelationshipOwnership RelationshipKind = "OWNERSHIP"
	RelationshipControl   RelationshipKind = "CONTROL"
)

// ControlType says how control is exercised when it is not (only) equity.
type ControlType string

const (
	ControlVoting      ControlType = "VOTING"
	ControlBoard       ControlType = "BOARD"
	ControlContractual ControlType = "CONTRACTUAL"
	ControlOther       ControlType = "OTHER"
)

// OwnershipStatus is the s4.4 lifecycle: Proposed -> Approved/Active ->
// Superseded/Ended. REJECTED is the refused branch of Proposed.
type OwnershipStatus string

const (
	OwnershipProposed   OwnershipStatus = "PROPOSED"
	OwnershipActive     OwnershipStatus = "ACTIVE"
	OwnershipSuperseded OwnershipStatus = "SUPERSEDED"
	OwnershipEnded      OwnershipStatus = "ENDED"
	OwnershipRejected   OwnershipStatus = "REJECTED"
)

// OwnershipRelationship is one effective-dated fact about how a child entity is
// owned or controlled by a parent entity.
type OwnershipRelationship struct {
	RelationshipID      string           `json:"relationship_id"`
	TenantID            string           `json:"tenant_id"`
	ParentLegalEntityID string           `json:"parent_legal_entity_id"`
	ChildLegalEntityID  string           `json:"child_legal_entity_id"`
	RelationshipKind    RelationshipKind `json:"relationship_kind"`
	OwnershipPct        *float64         `json:"ownership_pct,omitempty"`
	VotingPct           *float64         `json:"voting_pct,omitempty"`
	ControlType         *ControlType     `json:"control_type,omitempty"`
	BeneficialNotes     *string          `json:"beneficial_notes,omitempty"`

	ValidFrom  time.Time  `json:"valid_from"`
	ValidTo    *time.Time `json:"valid_to"` // nil = open-ended
	RecordedAt time.Time  `json:"recorded_at"`

	Status      OwnershipStatus `json:"status"`
	EvidenceRef string          `json:"evidence_ref"`
	Reason      string          `json:"reason"`

	ProposedByPrincipalID string     `json:"proposed_by_principal_id"`
	ProposedAt            time.Time  `json:"proposed_at"`
	DecidedByPrincipalID  *string    `json:"decided_by_principal_id,omitempty"`
	DecidedAt             *time.Time `json:"decided_at,omitempty"`
	DecisionNote          *string    `json:"decision_note,omitempty"`
	CorrelationID         string     `json:"correlation_id,omitempty"`

	// SupersedesRelationshipID names the ACTIVE relationship this one replaces
	// when it is approved (SupersedeRelationship).
	SupersedesRelationshipID *string `json:"supersedes_relationship_id,omitempty"`

	// Set only on an ENDED relationship (EndRelationship).
	EndedByPrincipalID *string    `json:"ended_by_principal_id,omitempty"`
	EndedAt            *time.Time `json:"ended_at,omitempty"`
	EndReason          *string    `json:"end_reason,omitempty"`
	EndEvidenceRef     *string    `json:"end_evidence_ref,omitempty"`

	// RecordVersion is the optimistic-concurrency version; events carry it as
	// object_version (ORG s7).
	RecordVersion int64 `json:"record_version"`
}

// ProposeOwnershipRequest is the ProposeOwnershipRelationship command.
type ProposeOwnershipRequest struct {
	ParentLegalEntityID string           `json:"parent_legal_entity_id"`
	ChildLegalEntityID  string           `json:"child_legal_entity_id"`
	RelationshipKind    RelationshipKind `json:"relationship_kind"`
	OwnershipPct        *float64         `json:"ownership_pct,omitempty"`
	VotingPct           *float64         `json:"voting_pct,omitempty"`
	ControlType         *ControlType     `json:"control_type,omitempty"`
	BeneficialNotes     string           `json:"beneficial_notes,omitempty"`
	ValidFrom           time.Time        `json:"valid_from"`
	ValidTo             *time.Time       `json:"valid_to,omitempty"`
	EvidenceRef         string           `json:"evidence_ref"`
	Reason              string           `json:"reason"`
	CorrelationID       string           `json:"correlation_id,omitempty"`
}

// OwnershipDecisionRequest is the body of an approval or a rejection. The
// decider is never in the body: it is the verified caller.
type OwnershipDecisionRequest struct {
	ExpectedVersion int64  `json:"expected_version"`
	DecisionNote    string `json:"decision_note,omitempty"`
	CorrelationID   string `json:"correlation_id,omitempty"`
}

// ErrInvalidOwnership is returned by Validate; the service wraps it as
// invalid input.
var ErrInvalidOwnership = errors.New("invalid ownership relationship")

func invalidOwnership(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrInvalidOwnership, fmt.Sprintf(format, args...))
}

// Validate checks everything that can be decided from the request alone. What
// needs the database (both entities exist and are operational, no cycle, no
// contradictory overlap) belongs to the service.
func (r ProposeOwnershipRequest) Validate() error {
	if strings.TrimSpace(r.ParentLegalEntityID) == "" || strings.TrimSpace(r.ChildLegalEntityID) == "" {
		return invalidOwnership("parent_legal_entity_id and child_legal_entity_id are required")
	}
	if r.ParentLegalEntityID == r.ChildLegalEntityID {
		return invalidOwnership("an entity cannot own or control itself")
	}
	switch r.RelationshipKind {
	case RelationshipOwnership:
		if r.OwnershipPct == nil {
			return invalidOwnership("an OWNERSHIP relationship requires ownership_pct")
		}
	case RelationshipControl:
		if r.ControlType == nil {
			return invalidOwnership("a CONTROL relationship requires control_type")
		}
	default:
		return invalidOwnership("relationship_kind must be OWNERSHIP or CONTROL")
	}
	if r.ControlType != nil {
		switch *r.ControlType {
		case ControlVoting, ControlBoard, ControlContractual, ControlOther:
		default:
			return invalidOwnership("control_type must be VOTING, BOARD, CONTRACTUAL or OTHER")
		}
	}
	if r.OwnershipPct != nil && (*r.OwnershipPct <= 0 || *r.OwnershipPct > 100) {
		return invalidOwnership("ownership_pct must be greater than 0 and at most 100")
	}
	if r.VotingPct != nil && (*r.VotingPct <= 0 || *r.VotingPct > 100) {
		return invalidOwnership("voting_pct must be greater than 0 and at most 100")
	}
	if r.ValidFrom.IsZero() {
		return invalidOwnership("valid_from is required")
	}
	if r.ValidTo != nil && !r.ValidTo.After(r.ValidFrom) {
		return invalidOwnership("valid_to must be after valid_from")
	}
	if strings.TrimSpace(r.EvidenceRef) == "" {
		return invalidOwnership("evidence_ref is required: a relationship without its source filing or contract cannot be approved")
	}
	if strings.TrimSpace(r.Reason) == "" {
		return invalidOwnership("reason is required")
	}
	return nil
}

// SupersedeOwnershipRequest is the SupersedeRelationship command: new terms for
// an ACTIVE relationship, effective from valid_from. Parent, child and kind
// cannot change (that would be a different relationship, proposed afresh).
type SupersedeOwnershipRequest struct {
	ExpectedVersion int64        `json:"expected_version"`
	OwnershipPct    *float64     `json:"ownership_pct,omitempty"`
	VotingPct       *float64     `json:"voting_pct,omitempty"`
	ControlType     *ControlType `json:"control_type,omitempty"`
	BeneficialNotes string       `json:"beneficial_notes,omitempty"`
	ValidFrom       time.Time    `json:"valid_from"`
	ValidTo         *time.Time   `json:"valid_to,omitempty"`
	EvidenceRef     string       `json:"evidence_ref"`
	Reason          string       `json:"reason"`
	CorrelationID   string       `json:"correlation_id,omitempty"`
}

// EndOwnershipRequest is the EndRelationship command.
type EndOwnershipRequest struct {
	ExpectedVersion int64     `json:"expected_version"`
	EndDate         time.Time `json:"end_date"`
	EvidenceRef     string    `json:"evidence_ref"`
	Reason          string    `json:"reason"`
	CorrelationID   string    `json:"correlation_id,omitempty"`
}

// Validate checks what can be decided from the request alone.
func (r EndOwnershipRequest) Validate() error {
	if r.EndDate.IsZero() {
		return invalidOwnership("end_date is required")
	}
	if strings.TrimSpace(r.EvidenceRef) == "" {
		return invalidOwnership("evidence_ref is required: an end needs its source as much as a start does")
	}
	if strings.TrimSpace(r.Reason) == "" {
		return invalidOwnership("reason is required")
	}
	return nil
}

// AsProposal is the superseding relationship expressed as the proposal it is,
// given the relationship it replaces. Parent, child and kind come from old.
func (r SupersedeOwnershipRequest) AsProposal(old OwnershipRelationship) ProposeOwnershipRequest {
	return ProposeOwnershipRequest{
		ParentLegalEntityID: old.ParentLegalEntityID, ChildLegalEntityID: old.ChildLegalEntityID,
		RelationshipKind: old.RelationshipKind, OwnershipPct: r.OwnershipPct, VotingPct: r.VotingPct,
		ControlType: r.ControlType, BeneficialNotes: r.BeneficialNotes, ValidFrom: r.ValidFrom, ValidTo: r.ValidTo,
		EvidenceRef: r.EvidenceRef, Reason: r.Reason, CorrelationID: r.CorrelationID,
	}
}
