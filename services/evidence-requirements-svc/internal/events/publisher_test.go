// Package events_test asserts which evaluation outcomes actually reach the
// Kafka broker, and in what shape.
//
// This is the layer where the §8.6 event contract lives: exactly two event
// types exist (evidence.requirement.satisfied / .missing), and
// NO_REQUIREMENTS_DEFINED is neither of them, so it must emit nothing rather
// than mint a third event type that the spec does not name.
package events_test

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/segmentio/kafka-go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"zoiko.io/evidence-requirements-svc/internal/domain"
	"zoiko.io/evidence-requirements-svc/internal/events"
)

type fakeWriter struct {
	msgs []kafka.Message
	err  error
}

func (f *fakeWriter) WriteMessages(_ context.Context, msgs ...kafka.Message) error {
	f.msgs = append(f.msgs, msgs...)
	return f.err
}

type envelope struct {
	EventType     string          `json:"event_type"`
	SchemaVersion string          `json:"schema_version"`
	SourceService string          `json:"source_service"`
	CorrelationID string          `json:"correlation_id"`
	Payload       json.RawMessage `json:"payload"`
}

func evaluation(outcome domain.Outcome) domain.EvidenceEvaluation {
	return domain.EvidenceEvaluation{
		EvaluationID:  "eval-1",
		TenantID:      "tenant-1",
		LegalEntityID: "entity-1",
		DomainCode:    "FINANCE",
		ActionType:    "JOURNAL_POST",
		Outcome:       outcome,
		UnmetPayload:  json.RawMessage(`[{"evidence_requirement_id":"req-1","evidence_type":"SIGNATURE","reason":"missing"}]`),
		CorrelationID: "corr-1",
	}
}

func decodeOne(t *testing.T, w *fakeWriter) envelope {
	t.Helper()
	require.Len(t, w.msgs, 1)
	var env envelope
	require.NoError(t, json.Unmarshal(w.msgs[0].Value, &env))
	return env
}

func TestPublishEvaluation_Satisfied(t *testing.T) {
	w := &fakeWriter{}
	p := events.NewPublisher(zap.NewNop(), "zoiko.evidence-requirements.events", w)

	p.PublishEvaluation(context.Background(), evaluation(domain.OutcomeSatisfied))

	env := decodeOne(t, w)
	assert.Equal(t, "evidence.requirement.satisfied", env.EventType)
	assert.Equal(t, "evidence-requirements-svc", env.SourceService)
	assert.Equal(t, "1.0", env.SchemaVersion)
	assert.Equal(t, "corr-1", env.CorrelationID)
}

func TestPublishEvaluation_Missing_CarriesUnmetDetail(t *testing.T) {
	w := &fakeWriter{}
	p := events.NewPublisher(zap.NewNop(), "zoiko.evidence-requirements.events", w)

	p.PublishEvaluation(context.Background(), evaluation(domain.OutcomeMissing))

	env := decodeOne(t, w)
	assert.Equal(t, "evidence.requirement.missing", env.EventType)

	// A consumer reacting to a blocked finalization needs to know WHAT is
	// missing, not merely that something is.
	var payload struct {
		Outcome string `json:"outcome"`
		Unmet   []struct {
			EvidenceRequirementID string `json:"evidence_requirement_id"`
			Reason                string `json:"reason"`
		} `json:"unmet"`
	}
	require.NoError(t, json.Unmarshal(env.Payload, &payload))
	assert.Equal(t, "MISSING", payload.Outcome)
	require.Len(t, payload.Unmet, 1)
	assert.Equal(t, "req-1", payload.Unmet[0].EvidenceRequirementID)
	assert.Equal(t, "missing", payload.Unmet[0].Reason)
}

// The contract test: NO_REQUIREMENTS_DEFINED must not reach the broker at all.
// §8.6 names exactly two events and neither is true of this outcome; inventing
// a third would extend this service's public contract beyond the spec.
func TestPublishEvaluation_NoRequirementsDefined_EmitsNothing(t *testing.T) {
	w := &fakeWriter{}
	p := events.NewPublisher(zap.NewNop(), "zoiko.evidence-requirements.events", w)

	p.PublishEvaluation(context.Background(), evaluation(domain.OutcomeNoRequirementsDefined))

	assert.Empty(t, w.msgs, "an unconfigured gate must not emit a satisfied or missing event")
}

// A broker failure is logged, never propagated — the DB write is already
// committed and must not be rolled back over a publish failure. Same posture
// as every other producer on this platform.
func TestPublishEvaluation_WriteFailure_DoesNotPanic(t *testing.T) {
	w := &fakeWriter{err: errors.New("broker down")}
	p := events.NewPublisher(zap.NewNop(), "zoiko.evidence-requirements.events", w)

	assert.NotPanics(t, func() {
		p.PublishEvaluation(context.Background(), evaluation(domain.OutcomeMissing))
	})
}
