package envelope

import (
	"encoding/json"
	"errors"
	"net/http"
	"testing"
	"time"
)

func TestNewTransitionCommandFromEnvelope(t *testing.T) {
	req := fullEnvelope(http.MethodPost, "/v1/ar/invoices/inv-001:issue")
	env := Parse(req)

	payload := &TransitionPayload{
		ReasonCode:   "CUSTOMER_REQUEST",
		Narrative:    "customer billing cycle finalized",
		EvidenceRefs: []string{"doc-4"},
	}

	cmd, err := NewTransitionCommand(env, "customer_invoice", "inv-001", "issue_invoice", payload)
	if err != nil {
		t.Fatalf("unexpected error creating TransitionCommand: %v", err)
	}

	// Verify Tenant & Identity
	if cmd.TenantID != "tenant-01" {
		t.Errorf("cmd.TenantID = %q, want tenant-01", cmd.TenantID)
	}
	if cmd.ActorSubjectID != "user-01" {
		t.Errorf("cmd.ActorSubjectID = %q, want user-01", cmd.ActorSubjectID)
	}
	if cmd.LegalEntityID == nil || *cmd.LegalEntityID != "entity-01" {
		t.Errorf("cmd.LegalEntityID = %v, want entity-01", cmd.LegalEntityID)
	}

	// Verify Object & Transition
	if cmd.ObjectType != "customer_invoice" {
		t.Errorf("cmd.ObjectType = %q, want customer_invoice", cmd.ObjectType)
	}
	if cmd.ObjectID != "inv-001" {
		t.Errorf("cmd.ObjectID = %q, want inv-001", cmd.ObjectID)
	}
	if cmd.Transition != "issue_invoice" {
		t.Errorf("cmd.Transition = %q, want issue_invoice", cmd.Transition)
	}

	// Verify ExpectedVersion & Idempotency
	if cmd.ExpectedObjectVersion != 7 {
		t.Errorf("cmd.ExpectedObjectVersion = %d, want 7", cmd.ExpectedObjectVersion)
	}
	if cmd.IdempotencyKey != "idem-01" {
		t.Errorf("cmd.IdempotencyKey = %q, want idem-01", cmd.IdempotencyKey)
	}

	// Verify Workflow & Approval references
	if cmd.WorkflowInstanceID == nil || *cmd.WorkflowInstanceID != "wf-01" {
		t.Errorf("cmd.WorkflowInstanceID = %v, want wf-01", cmd.WorkflowInstanceID)
	}
	if cmd.ApprovalRequestID == nil || *cmd.ApprovalRequestID != "appr-01" {
		t.Errorf("cmd.ApprovalRequestID = %v, want appr-01", cmd.ApprovalRequestID)
	}

	// Verify Governed Reason Code
	if cmd.ReasonFamily == nil || *cmd.ReasonFamily != ReasonFamilyCancel {
		t.Errorf("cmd.ReasonFamily = %v, want %q", cmd.ReasonFamily, ReasonFamilyCancel)
	}
	if cmd.ReasonCode == nil || *cmd.ReasonCode != ReasonCancelCustomerRequest {
		t.Errorf("cmd.ReasonCode = %v, want %q", cmd.ReasonCode, ReasonCancelCustomerRequest)
	}
	if cmd.Narrative == nil || *cmd.Narrative != "customer billing cycle finalized" {
		t.Errorf("cmd.Narrative = %v, want 'customer billing cycle finalized'", cmd.Narrative)
	}

	// Verify Merged Evidence References (doc-1, doc-2, doc-3 from header + doc-4 from payload = 4 items)
	if len(cmd.EvidenceRefs) != 4 {
		t.Errorf("len(cmd.EvidenceRefs) = %d, want 4 (%v)", len(cmd.EvidenceRefs), cmd.EvidenceRefs)
	}
}

func TestTransitionCommandValidation(t *testing.T) {
	validEnv := Parse(fullEnvelope(http.MethodPost, "/v1/test"))

	// Missing TenantID
	badEnv := validEnv
	badEnv.TenantID = ""
	_, err := NewTransitionCommand(badEnv, "invoice", "inv-1", "issue", nil)
	if !errors.Is(err, ErrMissingTenantID) {
		t.Errorf("expected ErrMissingTenantID, got %v", err)
	}

	// Missing Actor (both user and workload empty)
	badEnv = validEnv
	badEnv.ActorSubjectID = ""
	badEnv.WorkloadID = ""
	_, err = NewTransitionCommand(badEnv, "invoice", "inv-1", "issue", nil)
	if !errors.Is(err, ErrMissingActorID) {
		t.Errorf("expected ErrMissingActorID, got %v", err)
	}

	// Workload-only actor should pass
	workloadEnv := validEnv
	workloadEnv.ActorSubjectID = ""
	workloadEnv.WorkloadID = "workload-cron-01"
	cmd, err := NewTransitionCommand(workloadEnv, "invoice", "inv-1", "issue", nil)
	if err != nil {
		t.Fatalf("workload identity should be valid, got: %v", err)
	}
	if cmd.WorkloadSubjectID == nil || *cmd.WorkloadSubjectID != "workload-cron-01" {
		t.Errorf("WorkloadSubjectID = %v, want workload-cron-01", cmd.WorkloadSubjectID)
	}

	// Missing ObjectType
	_, err = NewTransitionCommand(validEnv, "", "inv-1", "issue", nil)
	if !errors.Is(err, ErrMissingObjectType) {
		t.Errorf("expected ErrMissingObjectType, got %v", err)
	}

	// Missing ObjectID
	_, err = NewTransitionCommand(validEnv, "invoice", "", "issue", nil)
	if !errors.Is(err, ErrMissingObjectID) {
		t.Errorf("expected ErrMissingObjectID, got %v", err)
	}

	// Missing Transition
	_, err = NewTransitionCommand(validEnv, "invoice", "inv-1", "", nil)
	if !errors.Is(err, ErrMissingTransition) {
		t.Errorf("expected ErrMissingTransition, got %v", err)
	}

	// Missing IdempotencyKey
	badEnv = validEnv
	badEnv.IdempotencyKey = ""
	_, err = NewTransitionCommand(badEnv, "invoice", "inv-1", "issue", nil)
	if !errors.Is(err, ErrMissingIdempotencyKey) {
		t.Errorf("expected ErrMissingIdempotencyKey, got %v", err)
	}

	// Missing ExpectedVersion
	badEnv = validEnv
	badEnv.ExpectedVersion = ""
	_, err = NewTransitionCommand(badEnv, "invoice", "inv-1", "issue", nil)
	if !errors.Is(err, ErrInvalidExpectedVersion) {
		t.Errorf("expected ErrInvalidExpectedVersion, got %v", err)
	}

	// Negative ExpectedVersion
	badEnv = validEnv
	badEnv.ExpectedVersion = "-1"
	_, err = NewTransitionCommand(badEnv, "invoice", "inv-1", "issue", nil)
	if !errors.Is(err, ErrInvalidExpectedVersion) {
		t.Errorf("expected ErrInvalidExpectedVersion, got %v", err)
	}

	// Invalid Ungoverned Reason Code in Payload
	badPayload := &TransitionPayload{ReasonCode: "ungoverned_free_text"}
	_, err = NewTransitionCommand(validEnv, "invoice", "inv-1", "issue", badPayload)
	if err == nil {
		t.Error("expected error for ungoverned reason code, got nil")
	}
}

func TestTransitionResultStructure(t *testing.T) {
	now := time.Now().UTC()
	wfID := "wf-123"
	apprRef := "appr-456"

	res := TransitionResult{
		ObjectID:           "inv-001",
		ObjectType:         "customer_invoice",
		PriorState:         "APPROVED",
		CurrentState:       "ISSUED",
		ObjectVersion:      8,
		Transition:         "issue_invoice",
		OccurredAt:         now,
		TransitionID:       "trans-001",
		WorkflowInstanceID: &wfID,
		ApprovalReference:  &apprRef,
	}

	data, err := json.Marshal(res)
	if err != nil {
		t.Fatalf("json.Marshal failed: %v", err)
	}

	var parsed TransitionResult
	if err := json.Unmarshal(data, &parsed); err != nil {
		t.Fatalf("json.Unmarshal failed: %v", err)
	}

	if parsed.CurrentState != "ISSUED" || parsed.ObjectVersion != 8 {
		t.Errorf("TransitionResult fields corrupted during serialization: %+v", parsed)
	}
}

func TestTransitionHistoryRecordStructure(t *testing.T) {
	now := time.Now().UTC()
	leID := "le-01"
	reason := "POSTING_ERROR"

	rec := TransitionHistoryRecord{
		TransitionID:        "trans-99",
		TenantID:            "tenant-01",
		LegalEntityID:       &leID,
		ObjectType:          "journal",
		ObjectID:            "j-001",
		StateDimension:      "lifecycle_state",
		FromState:           "POSTED",
		ToState:             "REVERSED",
		TransitionName:      "reverse_journal",
		ObjectVersionBefore: 3,
		ObjectVersionAfter:  4,
		ActorSubjectID:      "user-02",
		ReasonCode:          &reason,
		CorrelationID:       "corr-99",
		OccurredAt:          now,
	}

	data, err := json.Marshal(rec)
	if err != nil {
		t.Fatalf("json.Marshal failed: %v", err)
	}

	var parsed TransitionHistoryRecord
	if err := json.Unmarshal(data, &parsed); err != nil {
		t.Fatalf("json.Unmarshal failed: %v", err)
	}

	if parsed.TransitionName != "reverse_journal" || parsed.ObjectVersionAfter != 4 {
		t.Errorf("TransitionHistoryRecord fields corrupted during serialization: %+v", parsed)
	}
}
