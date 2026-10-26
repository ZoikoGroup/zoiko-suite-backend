// Package envelope defines the canonical request and transition contracts for ZoikoSuite.
//
// This file implements the Canonical Transition Command, Transition Result, and
// Transition History contracts per ZS-STATE-001 (Business Object State Machine,
// Workflow & Approval Catalogue) §4 and Appendix A.
//
// Doctrine (ZS-STATE-001 §4):
// "Every authoritative transition command must carry enough context to prove what
// was requested, against which version, by whom, under which policy and with which
// reason/evidence. The owning service resolves all security- and policy-sensitive
// facts server-side."
package envelope

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Sentinel errors for transition command validation.
var (
	ErrMissingTenantID        = errors.New("transition command missing tenant_id")
	ErrMissingActorID         = errors.New("transition command missing actor_subject_id or workload_id")
	ErrMissingObjectType      = errors.New("transition command missing object_type")
	ErrMissingObjectID        = errors.New("transition command missing object_id")
	ErrMissingTransition      = errors.New("transition command missing transition name")
	ErrMissingIdempotencyKey  = errors.New("transition command missing idempotency_key")
	ErrInvalidExpectedVersion = errors.New("transition command requires non-negative expected_object_version")
	ErrInvalidSourceChannel   = errors.New("transition command invalid source_channel")
)

// TransitionCommand represents an explicit, governed transition invocation on a
// material business object per ZS-STATE-001 §4.
type TransitionCommand struct {
	CommandID             string        `json:"command_id"`
	TenantID              string        `json:"tenant_id"`
	LegalEntityID         *string       `json:"legal_entity_id,omitempty"`
	ObjectType            string        `json:"object_type"`
	ObjectID              string        `json:"object_id"`
	ExpectedObjectVersion int           `json:"expected_object_version"`
	Transition            string        `json:"transition"`
	IdempotencyKey        string        `json:"idempotency_key"`
	ActorSubjectID        string        `json:"actor_subject_id"`
	WorkloadSubjectID     *string       `json:"workload_subject_id,omitempty"`
	SourceChannel         SourceChannel `json:"source_channel"`
	EffectiveAt           *time.Time    `json:"effective_at,omitempty"`
	ReasonCode            *ReasonCode   `json:"reason_code,omitempty"`
	ReasonFamily          *ReasonFamily `json:"reason_family,omitempty"`
	Narrative             *string       `json:"narrative,omitempty"`
	EvidenceRefs          []string      `json:"evidence_refs,omitempty"`
	WorkflowInstanceID    *string       `json:"workflow_instance_id,omitempty"`
	ApprovalRequestID     *string       `json:"approval_request_id,omitempty"`
	CorrelationID         string        `json:"correlation_id"`
	CausationID           *string       `json:"causation_id,omitempty"`
}

// TransitionPayload represents the optional request body parameters that accompany
// a transition command.
type TransitionPayload struct {
	ReasonCode   string   `json:"reason_code,omitempty"`
	Narrative    string   `json:"narrative,omitempty"`
	EvidenceRefs []string `json:"evidence_refs,omitempty"`
}

// NewTransitionCommand constructs a strongly typed TransitionCommand by combining
// the verified request Envelope, target object metadata, transition verb, and optional payload.
func NewTransitionCommand(
	env Envelope,
	objectType string,
	objectID string,
	transition string,
	payload *TransitionPayload,
) (TransitionCommand, error) {
	cmd := TransitionCommand{
		CommandID:      env.RequestID,
		TenantID:       env.TenantID,
		ObjectType:     strings.TrimSpace(objectType),
		ObjectID:       strings.TrimSpace(objectID),
		Transition:     strings.TrimSpace(transition),
		IdempotencyKey: env.IdempotencyKey,
		ActorSubjectID: env.ActorSubjectID,
		SourceChannel:  env.SourceChannel,
		CorrelationID:  env.CorrelationID,
	}

	if env.LegalEntityID != "" {
		le := env.LegalEntityID
		cmd.LegalEntityID = &le
	}

	if env.WorkloadID != "" {
		wl := env.WorkloadID
		cmd.WorkloadSubjectID = &wl
	}

	if env.CausationID != "" {
		c := env.CausationID
		cmd.CausationID = &c
	}

	if env.WorkflowInstanceID != "" {
		wf := env.WorkflowInstanceID
		cmd.WorkflowInstanceID = &wf
	}

	if env.ApprovalReference != "" {
		appr := env.ApprovalReference
		cmd.ApprovalRequestID = &appr
	}

	if env.EffectiveAt != nil {
		cmd.EffectiveAt = env.EffectiveAt
	}

	// Parse ExpectedVersion from envelope header
	if env.ExpectedVersion == "" {
		return cmd, fmt.Errorf("%w: X-Expected-Version header is missing", ErrInvalidExpectedVersion)
	}
	ver, err := strconv.Atoi(env.ExpectedVersion)
	if err != nil || ver < 0 {
		return cmd, fmt.Errorf("%w: invalid version value %q", ErrInvalidExpectedVersion, env.ExpectedVersion)
	}
	cmd.ExpectedObjectVersion = ver

	// Merge evidence references from header and payload
	evidenceSet := make(map[string]bool)
	for _, ref := range env.EvidenceRefs {
		ref = strings.TrimSpace(ref)
		if ref != "" {
			evidenceSet[ref] = true
		}
	}

	if payload != nil {
		for _, ref := range payload.EvidenceRefs {
			ref = strings.TrimSpace(ref)
			if ref != "" {
				evidenceSet[ref] = true
			}
		}

		if strings.TrimSpace(payload.Narrative) != "" {
			narr := strings.TrimSpace(payload.Narrative)
			cmd.Narrative = &narr
		}

		// Parse and validate reason code if supplied
		if strings.TrimSpace(payload.ReasonCode) != "" {
			fam, code, parseErr := ParseReason(payload.ReasonCode)
			if parseErr != nil {
				return cmd, fmt.Errorf("transition command reason validation failed: %w", parseErr)
			}
			cmd.ReasonFamily = &fam
			cmd.ReasonCode = &code
		}
	}

	// Canonicalize evidence refs in deterministic slice
	if len(evidenceSet) > 0 {
		refs := make([]string, 0, len(evidenceSet))
		for ref := range evidenceSet {
			refs = append(refs, ref)
		}
		cmd.EvidenceRefs = refs
	}

	if err := cmd.Validate(); err != nil {
		return cmd, err
	}

	return cmd, nil
}

// Validate verifies that the TransitionCommand satisfies the mandatory invariants of ZS-STATE-001 §4.
func (c *TransitionCommand) Validate() error {
	if c.TenantID == "" {
		return ErrMissingTenantID
	}
	if c.ActorSubjectID == "" && (c.WorkloadSubjectID == nil || *c.WorkloadSubjectID == "") {
		return ErrMissingActorID
	}
	if c.ObjectType == "" {
		return ErrMissingObjectType
	}
	if c.ObjectID == "" {
		return ErrMissingObjectID
	}
	if c.Transition == "" {
		return ErrMissingTransition
	}
	if c.IdempotencyKey == "" {
		return ErrMissingIdempotencyKey
	}
	if c.ExpectedObjectVersion < 0 {
		return ErrInvalidExpectedVersion
	}
	if !c.SourceChannel.Valid() {
		return fmt.Errorf("%w: %q", ErrInvalidSourceChannel, c.SourceChannel)
	}
	return nil
}

// TransitionResult represents the canonical response returned upon successful
// execution of an authoritative state transition.
type TransitionResult struct {
	ObjectID           string     `json:"object_id"`
	ObjectType         string     `json:"object_type"`
	PriorState         string     `json:"prior_state"`
	CurrentState       string     `json:"current_state"`
	ObjectVersion      int        `json:"object_version"`
	Transition         string     `json:"transition"`
	OccurredAt         time.Time  `json:"occurred_at"`
	EffectiveAt        *time.Time `json:"effective_at,omitempty"`
	TransitionID       string     `json:"transition_id"`
	WorkflowInstanceID *string    `json:"workflow_instance_id,omitempty"`
	ApprovalReference  *string    `json:"approval_reference,omitempty"`
}

// TransitionHistoryRecord represents the canonical immutable transition audit record
// defined in ZS-STATE-001 Appendix A.
//
// Doctrine (ZS-STATE-001 §2 I-12):
// "Successful transitions append history; history is not overwritten."
type TransitionHistoryRecord struct {
	TransitionID        string     `json:"transition_id"`
	TenantID            string     `json:"tenant_id"`
	LegalEntityID       *string    `json:"legal_entity_id,omitempty"`
	ObjectType          string     `json:"object_type"`
	ObjectID            string     `json:"object_id"`
	StateDimension      string     `json:"state_dimension"` // e.g. "lifecycle_state", "approval_state"
	FromState           string     `json:"from_state"`
	ToState             string     `json:"to_state"`
	TransitionName      string     `json:"transition_name"`
	ObjectVersionBefore int        `json:"object_version_before"`
	ObjectVersionAfter  int        `json:"object_version_after"`
	ActorSubjectID      string     `json:"actor_subject_id"`
	WorkloadSubjectID   *string    `json:"workload_subject_id,omitempty"`
	ReasonCode          *string    `json:"reason_code,omitempty"`
	Narrative           *string    `json:"narrative,omitempty"`
	ApprovalRequestID   *string    `json:"approval_request_id,omitempty"`
	WorkflowInstanceID  *string    `json:"workflow_instance_id,omitempty"`
	PolicyVersion       *string    `json:"policy_version,omitempty"`
	RuleVersion         *string    `json:"rule_version,omitempty"`
	EvidenceRefs        []string   `json:"evidence_refs,omitempty"`
	CorrelationID       string     `json:"correlation_id"`
	CausationID         *string    `json:"causation_id,omitempty"`
	OccurredAt          time.Time  `json:"occurred_at"`
	EffectiveAt         *time.Time `json:"effective_at,omitempty"`
}

// PinnedStateMachineEdges maps object_type -> from_state -> set of allowed to_states.
// Per ZS-STATE-001 §4 (Evaluation-order step 4) and Invariant I-03 ("Allowed edge only:
// A transition must exist in the active state-machine definition and originate from the current state").
//
// Pinned state machine for workflow_instance (§6):
// - PENDING: may transition to APPROVED, REJECTED, ESCALATED, CANCELLED, INVALIDATED
// - ESCALATED: may transition to APPROVED, REJECTED, CANCELLED, INVALIDATED
// - APPROVED: may transition to INVALIDATED (per §6.1/§7 material edit after approval)
// - REJECTED, CANCELLED, INVALIDATED: terminal states (no allowed outbound transitions)
var PinnedStateMachineEdges = map[string]map[string][]string{
	"workflow_instance": {
		"PENDING":   {"APPROVED", "REJECTED", "ESCALATED", "CANCELLED", "INVALIDATED"},
		"ESCALATED": {"APPROVED", "REJECTED", "CANCELLED", "INVALIDATED"},
		"APPROVED":  {"INVALIDATED"},
	},
	"workflow": {
		"PENDING":   {"APPROVED", "REJECTED", "ESCALATED", "CANCELLED", "INVALIDATED"},
		"ESCALATED": {"APPROVED", "REJECTED", "CANCELLED", "INVALIDATED"},
		"APPROVED":  {"INVALIDATED"},
	},
}

// ErrIllegalTransitionEdge is the sentinel error when a transition edge does not exist
// in the pinned state-machine definition (ZS-STATE-001 §4 step 4, Invariant I-03).
var ErrIllegalTransitionEdge = errors.New("illegal state machine transition edge")

// IllegalEdgeError represents a rejection under ZS-STATE-001 §4 step 4 and §16.
type IllegalEdgeError struct {
	ObjectType     string         `json:"object_type"`
	FromState      string         `json:"from_state"`
	ToState        string         `json:"to_state"`
	ReasonFamily   ReasonFamily   `json:"reason_family"`
	ReasonCode     ReasonCode     `json:"reason_code"`
	ExceptionClass ExceptionClass `json:"exception_class"`
}

func (e *IllegalEdgeError) Error() string {
	return fmt.Sprintf("illegal state transition edge for %q: %s -> %s (exception_class: %s, reason: %s/%s)",
		e.ObjectType, e.FromState, e.ToState, e.ExceptionClass, e.ReasonFamily, e.ReasonCode)
}

func (e *IllegalEdgeError) Is(target error) bool {
	return target == ErrIllegalTransitionEdge
}

// ValidateTransitionEdge checks whether transitioning objectType from fromState to toState
// is permitted by the pinned state-machine definition (ZS-STATE-001 §4 evaluation order step 4).
func ValidateTransitionEdge(objectType, fromState, toState string) error {
	normType := strings.ToLower(strings.TrimSpace(objectType))
	edgesForType, ok := PinnedStateMachineEdges[normType]
	if !ok {
		return &IllegalEdgeError{
			ObjectType:     objectType,
			FromState:      fromState,
			ToState:        toState,
			ReasonFamily:   ReasonFamilyReject,
			ReasonCode:     ReasonRejectPolicyNotMet,
			ExceptionClass: ExceptionClassBusinessRule,
		}
	}

	normFrom := strings.ToUpper(strings.TrimSpace(fromState))
	normTo := strings.ToUpper(strings.TrimSpace(toState))

	allowedTargets, ok := edgesForType[normFrom]
	if !ok {
		// State has no outbound edges (e.g. terminal state)
		return &IllegalEdgeError{
			ObjectType:     objectType,
			FromState:      normFrom,
			ToState:        normTo,
			ReasonFamily:   ReasonFamilyReject,
			ReasonCode:     ReasonRejectPolicyNotMet,
			ExceptionClass: ExceptionClassBusinessRule,
		}
	}

	for _, target := range allowedTargets {
		if target == normTo {
			return nil
		}
	}

	return &IllegalEdgeError{
		ObjectType:     objectType,
		FromState:      normFrom,
		ToState:        normTo,
		ReasonFamily:   ReasonFamilyReject,
		ReasonCode:     ReasonRejectPolicyNotMet,
		ExceptionClass: ExceptionClassBusinessRule,
	}
}

// WorkflowTransitionTarget resolves a transition name to its target state for workflow_instance.
func WorkflowTransitionTarget(transition string) (string, bool) {
	switch strings.ToLower(strings.TrimSpace(transition)) {
	case "invalidate", "invalidate_workflow":
		return "INVALIDATED", true
	case "cancel", "cancel_workflow":
		return "CANCELLED", true
	case "escalate", "escalate_workflow":
		return "ESCALATED", true
	case "approve", "approve_workflow":
		return "APPROVED", true
	case "reject", "reject_workflow":
		return "REJECTED", true
	default:
		return "", false
	}
}
