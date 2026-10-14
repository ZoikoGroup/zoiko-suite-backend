package envelope_test

import (
	"errors"
	"testing"

	"zoiko.io/workflow-svc/internal/envelope"
)

func TestNewTransitionCommand_Valid(t *testing.T) {
	env := envelope.Envelope{
		TenantID:           "t-1",
		ActorSubjectID:     "p-1",
		RequestID:          "req-1",
		IdempotencyKey:     "idem-1",
		SourceChannel:      envelope.ChannelWeb,
		CorrelationID:      "corr-1",
		ExpectedVersion:    "5",
		LegalEntityID:      "le-1",
		WorkloadID:         "wl-1",
		CausationID:        "cause-1",
		WorkflowInstanceID: "wf-1",
		ApprovalReference:  "appr-1",
		EvidenceRefs:       []string{"ev-1", "ev-2"},
		EffectiveAt:        nil,
	}

	payload := &envelope.TransitionPayload{
		ReasonCode:   "CANCEL:CUSTOMER_REQUEST",
		Narrative:    "customer cancelled",
		EvidenceRefs: []string{"ev-3"},
	}

	cmd, err := envelope.NewTransitionCommand(env, "workflow_instance", "wf-123", "cancel", payload)
	if err != nil {
		t.Fatalf("NewTransitionCommand failed: %v", err)
	}

	if cmd.TenantID != "t-1" {
		t.Errorf("TenantID = %q, want %q", cmd.TenantID, "t-1")
	}
	if cmd.ObjectType != "workflow_instance" {
		t.Errorf("ObjectType = %q, want %q", cmd.ObjectType, "workflow_instance")
	}
	if cmd.ObjectID != "wf-123" {
		t.Errorf("ObjectID = %q, want %q", cmd.ObjectID, "wf-123")
	}
	if cmd.Transition != "cancel" {
		t.Errorf("Transition = %q, want %q", cmd.Transition, "cancel")
	}
	if cmd.IdempotencyKey != "idem-1" {
		t.Errorf("IdempotencyKey = %q, want %q", cmd.IdempotencyKey, "idem-1")
	}
	if cmd.ActorSubjectID != "p-1" {
		t.Errorf("ActorSubjectID = %q, want %q", cmd.ActorSubjectID, "p-1")
	}
	if cmd.SourceChannel != envelope.ChannelWeb {
		t.Errorf("SourceChannel = %q, want %q", cmd.SourceChannel, envelope.ChannelWeb)
	}
	if cmd.CorrelationID != "corr-1" {
		t.Errorf("CorrelationID = %q, want %q", cmd.CorrelationID, "corr-1")
	}
	if cmd.ExpectedObjectVersion != 5 {
		t.Errorf("ExpectedObjectVersion = %d, want 5", cmd.ExpectedObjectVersion)
	}
	if cmd.LegalEntityID == nil || *cmd.LegalEntityID != "le-1" {
		t.Errorf("LegalEntityID = %v, want le-1", cmd.LegalEntityID)
	}
	if cmd.WorkloadSubjectID == nil || *cmd.WorkloadSubjectID != "wl-1" {
		t.Errorf("WorkloadSubjectID = %v, want wl-1", cmd.WorkloadSubjectID)
	}
	if cmd.CausationID == nil || *cmd.CausationID != "cause-1" {
		t.Errorf("CausationID = %v, want cause-1", cmd.CausationID)
	}
	if cmd.WorkflowInstanceID == nil || *cmd.WorkflowInstanceID != "wf-1" {
		t.Errorf("WorkflowInstanceID = %v, want wf-1", cmd.WorkflowInstanceID)
	}
	if cmd.ApprovalRequestID == nil || *cmd.ApprovalRequestID != "appr-1" {
		t.Errorf("ApprovalRequestID = %v, want appr-1", cmd.ApprovalRequestID)
	}
	if cmd.ReasonFamily == nil || *cmd.ReasonFamily != envelope.ReasonFamilyCancel {
		t.Errorf("ReasonFamily = %v, want CANCEL", cmd.ReasonFamily)
	}
	if cmd.ReasonCode == nil || *cmd.ReasonCode != envelope.ReasonCancelCustomerRequest {
		t.Errorf("ReasonCode = %v, want CUSTOMER_REQUEST", cmd.ReasonCode)
	}
	if cmd.Narrative == nil || *cmd.Narrative != "customer cancelled" {
		t.Errorf("Narrative = %v, want customer cancelled", cmd.Narrative)
	}
	// Evidence refs should be deduplicated
	evidenceSet := make(map[string]bool)
	for _, ref := range cmd.EvidenceRefs {
		evidenceSet[ref] = true
	}
	if len(evidenceSet) != 3 || !evidenceSet["ev-1"] || !evidenceSet["ev-2"] || !evidenceSet["ev-3"] {
		t.Errorf("EvidenceRefs = %v, want [ev-1 ev-2 ev-3]", cmd.EvidenceRefs)
	}
}

func TestNewTransitionCommand_MissingExpectedVersion(t *testing.T) {
	env := envelope.Envelope{
		TenantID:        "t-1",
		ActorSubjectID:  "p-1",
		RequestID:       "req-1",
		IdempotencyKey:  "idem-1",
		SourceChannel:   envelope.ChannelWeb,
		CorrelationID:   "corr-1",
		ExpectedVersion: "", // missing
	}

	_, err := envelope.NewTransitionCommand(env, "workflow_instance", "wf-123", "cancel", nil)
	if err == nil {
		t.Errorf("expected error for missing ExpectedVersion")
	}
}

func TestNewTransitionCommand_InvalidExpectedVersion(t *testing.T) {
	env := envelope.Envelope{
		TenantID:        "t-1",
		ActorSubjectID:  "p-1",
		RequestID:       "req-1",
		IdempotencyKey:  "idem-1",
		SourceChannel:   envelope.ChannelWeb,
		CorrelationID:   "corr-1",
		ExpectedVersion: "not-a-number",
	}

	_, err := envelope.NewTransitionCommand(env, "workflow_instance", "wf-123", "cancel", nil)
	if err == nil {
		t.Errorf("expected error for invalid ExpectedVersion")
	}
}

func TestNewTransitionCommand_NegativeVersion(t *testing.T) {
	env := envelope.Envelope{
		TenantID:        "t-1",
		ActorSubjectID:  "p-1",
		RequestID:       "req-1",
		IdempotencyKey:  "idem-1",
		SourceChannel:   envelope.ChannelWeb,
		CorrelationID:   "corr-1",
		ExpectedVersion: "-1",
	}

	_, err := envelope.NewTransitionCommand(env, "workflow_instance", "wf-123", "cancel", nil)
	if err == nil {
		t.Errorf("expected error for negative version")
	}
}

func TestNewTransitionCommand_InvalidSourceChannel(t *testing.T) {
	env := envelope.Envelope{
		TenantID:        "t-1",
		ActorSubjectID:  "p-1",
		RequestID:       "req-1",
		IdempotencyKey:  "idem-1",
		SourceChannel:   envelope.SourceChannel("invalid"),
		CorrelationID:   "corr-1",
		ExpectedVersion: "1",
	}

	_, err := envelope.NewTransitionCommand(env, "workflow_instance", "wf-123", "cancel", nil)
	if err == nil {
		t.Errorf("expected error for invalid source channel")
	}
}

func TestTransitionCommand_Validate(t *testing.T) {
	tests := []struct {
		name    string
		cmd     envelope.TransitionCommand
		wantErr bool
	}{
		{
			name: "valid",
			cmd: envelope.TransitionCommand{
				TenantID:              "t-1",
				ActorSubjectID:        "p-1",
				ObjectType:            "workflow_instance",
				ObjectID:              "wf-1",
				Transition:            "cancel",
				IdempotencyKey:        "idem-1",
				ExpectedObjectVersion: 1,
				SourceChannel:         envelope.ChannelWeb,
			},
			wantErr: false,
		},
		{
			name: "missing tenant",
			cmd: envelope.TransitionCommand{
				ActorSubjectID:        "p-1",
				ObjectType:            "workflow_instance",
				ObjectID:              "wf-1",
				Transition:            "cancel",
				IdempotencyKey:        "idem-1",
				ExpectedObjectVersion: 1,
				SourceChannel:         envelope.ChannelWeb,
			},
			wantErr: true,
		},
		{
			name: "missing actor and workload",
			cmd: envelope.TransitionCommand{
				TenantID:              "t-1",
				ObjectType:            "workflow_instance",
				ObjectID:              "wf-1",
				Transition:            "cancel",
				IdempotencyKey:        "idem-1",
				ExpectedObjectVersion: 1,
				SourceChannel:         envelope.ChannelWeb,
			},
			wantErr: true,
		},
		{
			name: "missing object type",
			cmd: envelope.TransitionCommand{
				TenantID:              "t-1",
				ActorSubjectID:        "p-1",
				ObjectID:              "wf-1",
				Transition:            "cancel",
				IdempotencyKey:        "idem-1",
				ExpectedObjectVersion: 1,
				SourceChannel:         envelope.ChannelWeb,
			},
			wantErr: true,
		},
		{
			name: "missing idempotency key",
			cmd: envelope.TransitionCommand{
				TenantID:              "t-1",
				ActorSubjectID:        "p-1",
				ObjectType:            "workflow_instance",
				ObjectID:              "wf-1",
				Transition:            "cancel",
				ExpectedObjectVersion: 1,
				SourceChannel:         envelope.ChannelWeb,
			},
			wantErr: true,
		},
		{
			name: "negative version",
			cmd: envelope.TransitionCommand{
				TenantID:              "t-1",
				ActorSubjectID:        "p-1",
				ObjectType:            "workflow_instance",
				ObjectID:              "wf-1",
				Transition:            "cancel",
				IdempotencyKey:        "idem-1",
				ExpectedObjectVersion: -1,
				SourceChannel:         envelope.ChannelWeb,
			},
			wantErr: true,
		},
		{
			name: "invalid source channel",
			cmd: envelope.TransitionCommand{
				TenantID:              "t-1",
				ActorSubjectID:        "p-1",
				ObjectType:            "workflow_instance",
				ObjectID:              "wf-1",
				Transition:            "cancel",
				IdempotencyKey:        "idem-1",
				ExpectedObjectVersion: 1,
				SourceChannel:         envelope.SourceChannel("invalid"),
			},
			wantErr: true,
		},
	}

	for _, tt := range tests {
		err := tt.cmd.Validate()
		if tt.wantErr && err == nil {
			t.Errorf("%s: expected error, got nil", tt.name)
		}
		if !tt.wantErr && err != nil {
			t.Errorf("%s: unexpected error: %v", tt.name, err)
		}
	}
}

func TestValidateTransitionEdge(t *testing.T) {
	tests := []struct {
		name       string
		objectType string
		fromState  string
		toState    string
		wantErr    bool
	}{
		{"PENDING -> APPROVED", "workflow_instance", "PENDING", "APPROVED", false},
		{"PENDING -> REJECTED", "workflow_instance", "PENDING", "REJECTED", false},
		{"PENDING -> ESCALATED", "workflow_instance", "PENDING", "ESCALATED", false},
		{"PENDING -> CANCELLED", "workflow_instance", "PENDING", "CANCELLED", false},
		{"PENDING -> INVALIDATED", "workflow_instance", "PENDING", "INVALIDATED", false},
		{"ESCALATED -> APPROVED", "workflow_instance", "ESCALATED", "APPROVED", false},
		{"ESCALATED -> REJECTED", "workflow_instance", "ESCALATED", "REJECTED", false},
		{"ESCALATED -> CANCELLED", "workflow_instance", "ESCALATED", "CANCELLED", false},
		{"ESCALATED -> INVALIDATED", "workflow_instance", "ESCALATED", "INVALIDATED", false},
		{"APPROVED -> INVALIDATED", "workflow_instance", "APPROVED", "INVALIDATED", false},
		{"APPROVED -> APPROVED (terminal)", "workflow_instance", "APPROVED", "APPROVED", true},
		{"REJECTED -> APPROVED (terminal)", "workflow_instance", "REJECTED", "APPROVED", true},
		{"CANCELLED -> INVALIDATED (terminal)", "workflow_instance", "CANCELLED", "INVALIDATED", true},
		{"INVALIDATED -> CANCELLED (terminal)", "workflow_instance", "INVALIDATED", "CANCELLED", true},
		{"unknown object type", "unknown_object", "PENDING", "APPROVED", true},
	}

	for _, tt := range tests {
		err := envelope.ValidateTransitionEdge(tt.objectType, tt.fromState, tt.toState)
		if tt.wantErr && err == nil {
			t.Errorf("%s: expected error, got nil", tt.name)
		}
		if !tt.wantErr && err != nil {
			t.Errorf("%s: unexpected error: %v", tt.name, err)
		}
	}
}

func TestValidateTransitionEdge_CaseInsensitive(t *testing.T) {
	err := envelope.ValidateTransitionEdge("WORKFLOW_INSTANCE", "pending", "approved")
	if err != nil {
		t.Errorf("case insensitive failed: %v", err)
	}
}

func TestWorkflowTransitionTarget(t *testing.T) {
	tests := []struct {
		transition string
		wantState  string
		wantOK     bool
	}{
		{"invalidate", "INVALIDATED", true},
		{"invalidate_workflow", "INVALIDATED", true},
		{"cancel", "CANCELLED", true},
		{"cancel_workflow", "CANCELLED", true},
		{"escalate", "ESCALATED", true},
		{"escalate_workflow", "ESCALATED", true},
		{"approve", "APPROVED", true},
		{"approve_workflow", "APPROVED", true},
		{"reject", "REJECTED", true},
		{"reject_workflow", "REJECTED", true},
		{"unknown", "", false},
	}

	for _, tt := range tests {
		state, ok := envelope.WorkflowTransitionTarget(tt.transition)
		if ok != tt.wantOK {
			t.Errorf("%s: ok = %v, want %v", tt.transition, ok, tt.wantOK)
		}
		if ok && state != tt.wantState {
			t.Errorf("%s: state = %q, want %q", tt.transition, state, tt.wantState)
		}
	}
}

func TestIllegalEdgeError(t *testing.T) {
	err := &envelope.IllegalEdgeError{
		ObjectType:     "workflow_instance",
		FromState:      "APPROVED",
		ToState:        "REJECTED",
		ReasonFamily:   envelope.ReasonFamilyReject,
		ReasonCode:     envelope.ReasonRejectPolicyNotMet,
		ExceptionClass: envelope.ExceptionClassBusinessRule,
	}

	if err.Error() == "" {
		t.Errorf("Error() should not be empty")
	}

	if !errors.Is(err, envelope.ErrIllegalTransitionEdge) {
		t.Errorf("Is(ErrIllegalTransitionEdge) should be true")
	}
}

func TestPinnedStateMachineEdges(t *testing.T) {
	// Verify the pinned state machine has expected structure
	edges, ok := envelope.PinnedStateMachineEdges["workflow_instance"]
	if !ok {
		t.Fatal("workflow_instance not in PinnedStateMachineEdges")
	}

	pendingTargets := edges["PENDING"]
	expected := []string{"APPROVED", "REJECTED", "ESCALATED", "CANCELLED", "INVALIDATED"}
	if len(pendingTargets) != len(expected) {
		t.Errorf("PENDING has %d targets, want %d", len(pendingTargets), len(expected))
	}

	for _, target := range expected {
		found := false
		for _, t := range pendingTargets {
			if t == target {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("PENDING missing target %q", target)
		}
	}
}
