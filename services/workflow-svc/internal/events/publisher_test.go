// Package events_test asserts the event envelope actually carries the
// fields Doc 03 §19 requires (event_version, tenant_id, legal_entity_id,
// actor_id). Approval action handling is Doc 03 §3.7's #6 named mandatory
// case, and before this fix none of approval.granted/rejected/
// workflow.escalated/workflow.completed carried any actor at all.
package events_test

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/segmentio/kafka-go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"zoiko.io/workflow-svc/internal/domain"
	"zoiko.io/workflow-svc/internal/events"
)

type fakeWriter struct {
	msgs []kafka.Message
}

func (f *fakeWriter) WriteMessages(_ context.Context, msgs ...kafka.Message) error {
	f.msgs = append(f.msgs, msgs...)
	return nil
}

type envelope struct {
	EventType     string `json:"event_type"`
	EventVersion  string `json:"event_version"`
	SourceService string `json:"source_service"`
	CorrelationID string `json:"correlation_id"`
	TenantID      string `json:"tenant_id"`
	LegalEntityID string `json:"legal_entity_id"`
	ActorID       string `json:"actor_id"`
}

func decodeOne(t *testing.T, w *fakeWriter) envelope {
	t.Helper()
	require.Len(t, w.msgs, 1)
	var env envelope
	require.NoError(t, json.Unmarshal(w.msgs[0].Value, &env))
	return env
}

func instance() domain.WorkflowInstance {
	return domain.WorkflowInstance{
		WorkflowInstanceID: "wf-1",
		TenantID:           "tenant-1",
		LegalEntityID:      "entity-1",
		WorkflowType:       "PURCHASE_APPROVAL",
		WorkflowStatus:     "PENDING",
		InitiatedBy:        "initiator-1",
		CorrelationID:      "corr-1",
	}
}

func TestPublishWorkflowStarted_EnvelopeCarriesInitiator(t *testing.T) {
	w := &fakeWriter{}
	p := events.NewPublisher(zap.NewNop(), "zoiko.workflow.events", w)

	require.NoError(t, p.PublishWorkflowStarted(context.Background(), instance()))

	env := decodeOne(t, w)
	assert.Equal(t, "workflow.started", env.EventType)
	assert.Equal(t, "1.0", env.EventVersion)
	assert.Equal(t, "tenant-1", env.TenantID)
	assert.Equal(t, "entity-1", env.LegalEntityID)
	assert.Equal(t, "initiator-1", env.ActorID)
}

func TestPublishApprovalGranted_EnvelopeCarriesApprovingActor(t *testing.T) {
	w := &fakeWriter{}
	p := events.NewPublisher(zap.NewNop(), "zoiko.workflow.events", w)
	inst := instance()
	stage := domain.WorkflowStage{WorkflowStageID: "stage-1", StageOrder: 1, ApproverPrincipalID: "approver-1"}

	require.NoError(t, p.PublishApprovalGranted(context.Background(), inst, stage, "approver-1"))

	env := decodeOne(t, w)
	assert.Equal(t, "approval.granted", env.EventType)
	assert.Equal(t, "tenant-1", env.TenantID)
	assert.Equal(t, "entity-1", env.LegalEntityID)
	assert.Equal(t, "approver-1", env.ActorID)
}

// TestPublishWorkflowCompleted_EnvelopeCarriesFinalActor_NotInitiator proves
// the actor attributed to workflow.completed is whoever caused the terminal
// transition (the final approver, or whoever cancelled) — not the
// workflow's original initiator, which was the only actor-shaped field
// available on WorkflowInstance before this fix.
func TestPublishWorkflowCompleted_EnvelopeCarriesFinalActor_NotInitiator(t *testing.T) {
	w := &fakeWriter{}
	p := events.NewPublisher(zap.NewNop(), "zoiko.workflow.events", w)
	inst := instance()
	inst.WorkflowStatus = "APPROVED"

	require.NoError(t, p.PublishWorkflowCompleted(context.Background(), inst, "final-approver-1"))

	env := decodeOne(t, w)
	assert.Equal(t, "workflow.completed", env.EventType)
	assert.Equal(t, "final-approver-1", env.ActorID)
	assert.NotEqual(t, inst.InitiatedBy, env.ActorID)
}

func TestPublishWorkflowEscalated_EnvelopeCarriesEscalatingActor(t *testing.T) {
	w := &fakeWriter{}
	p := events.NewPublisher(zap.NewNop(), "zoiko.workflow.events", w)

	require.NoError(t, p.PublishWorkflowEscalated(context.Background(), instance(), "escalator-1"))

	env := decodeOne(t, w)
	assert.Equal(t, "escalator-1", env.ActorID)
}

func TestPublishApprovalRejected_EnvelopeCarriesRejectingActor(t *testing.T) {
	w := &fakeWriter{}
	p := events.NewPublisher(zap.NewNop(), "zoiko.workflow.events", w)
	stage := domain.WorkflowStage{WorkflowStageID: "stage-1", StageOrder: 1, ApproverPrincipalID: "approver-1"}

	require.NoError(t, p.PublishApprovalRejected(context.Background(), instance(), stage, "approver-1"))

	env := decodeOne(t, w)
	assert.Equal(t, "approval.rejected", env.EventType)
	assert.Equal(t, "approver-1", env.ActorID)
}

func TestPublish_WriteFailure_ReturnsError(t *testing.T) {
	w := &failingWriter{}
	p := events.NewPublisher(zap.NewNop(), "zoiko.workflow.events", w)

	err := p.PublishWorkflowStarted(context.Background(), instance())
	assert.Error(t, err)
}

type failingWriter struct{}

func (f *failingWriter) WriteMessages(_ context.Context, _ ...kafka.Message) error {
	return assert.AnError
}
