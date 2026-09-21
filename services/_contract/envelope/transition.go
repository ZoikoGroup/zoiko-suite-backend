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
