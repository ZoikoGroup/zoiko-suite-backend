// Package events_test asserts the event envelope actually carries the
// fields Doc 03 §19 requires that this service has real data for
// (event_version, tenant_id, actor_id, correlation_id). legal_entity_id
// and jurisdiction are correctly omitted: neither domain.RoleDefinition
// nor domain.PermissionBundleDef is scoped to one legal entity — a role
// definition is tenant-wide config.
//
// It also pins the payload field CONSUMERS read, which is a different thing
// from the fields this service happens to put there. See
// TestRoleUpdated_PayloadCarriesRoleID.
package events_test

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/segmentio/kafka-go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"zoiko.io/access-control-svc/internal/domain"
	"zoiko.io/access-control-svc/internal/events"
)

type fakeWriter struct {
	msgs []kafka.Message
	err  error
}

func (f *fakeWriter) WriteMessages(_ context.Context, msgs ...kafka.Message) error {
	if f.err != nil {
		return f.err
	}
	f.msgs = append(f.msgs, msgs...)
	return nil
}

type envelope struct {
	EventID       string          `json:"event_id"`
	EventType     string          `json:"event_type"`
	EventVersion  string          `json:"event_version"`
	SchemaVersion string          `json:"schema_version"`
	SourceService string          `json:"source_service"`
	TenantID      string          `json:"tenant_id"`
	ActorID       string          `json:"actor_id"`
	CorrelationID string          `json:"correlation_id"`
	Payload       json.RawMessage `json:"payload"`
}

func decode(t *testing.T, body []byte) envelope {
	t.Helper()
	var env envelope
	require.NoError(t, json.Unmarshal(body, &env))
	return env
}

func TestRoleUpdated_EnvelopeCarriesActor(t *testing.T) {
	out, err := events.RoleUpdated(domain.RoleDefinition{
		RoleDefinitionID: "role-1", TenantID: "tenant-1", CorrelationID: "corr-1",
		RoleCode: "PO_OFFICER", Status: domain.RoleStatusActive,
	}, "updater-1")
	require.NoError(t, err)

	env := decode(t, out.Body)
	assert.Equal(t, "role.updated", env.EventType)
	assert.Equal(t, "1.0", env.EventVersion)
	assert.Equal(t, "1.0", env.SchemaVersion)
	assert.Equal(t, "access-control-svc", env.SourceService)
	assert.Equal(t, "tenant-1", env.TenantID)
	assert.Equal(t, "updater-1", env.ActorID)
	assert.Equal(t, "corr-1", env.CorrelationID)
	assert.NotEmpty(t, env.EventID)
	// The Kafka key partitions by the aggregate, so two changes to one role
	// stay ordered relative to each other.
	assert.Equal(t, "role-1", out.Key)
}

// TestRoleUpdated_PayloadCarriesRoleID pins the field the CONSUMER reads.
//
// identity-context-svc's handleRoleUpdated unmarshals {"role_id": "..."} and
// bails out with "role.updated names no role_id — cannot revoke" when it is
// empty. This service emitted only role_definition_id, so that branch was taken
// EVERY time: retiring a role reached authorization-svc's active_flag, and
// every session already holding that role kept the permission bundles frozen
// into its envelope, because the revocation was never triggered.
//
// A test on this service's own field names would have passed throughout. This
// one asserts the name the other side actually reads.
func TestRoleUpdated_PayloadCarriesRoleID(t *testing.T) {
	out, err := events.RoleUpdated(domain.RoleDefinition{
		RoleDefinitionID: "role-42", TenantID: "tenant-1", CorrelationID: "corr-1",
		Status: domain.RoleStatusRetired,
	}, "updater-1")
	require.NoError(t, err)

	var p struct {
		RoleID           string `json:"role_id"`
		RoleDefinitionID string `json:"role_definition_id"`
		Status           string `json:"status"`
	}
	require.NoError(t, json.Unmarshal(decode(t, out.Body).Payload, &p))

	assert.Equal(t, "role-42", p.RoleID,
		"identity-context-svc reads payload.role_id; empty here means no session is ever revoked for a role change")
	assert.Equal(t, "role-42", p.RoleDefinitionID,
		"the original field name is kept so consumers built against the shipped shape keep working")
	assert.Equal(t, "RETIRED", p.Status)
}

func TestRoleCreated_PayloadCarriesRoleID(t *testing.T) {
	out, err := events.RoleCreated(domain.RoleDefinition{
		RoleDefinitionID: "role-7", TenantID: "tenant-1", CorrelationID: "corr-1",
		RoleCode: "AP_CLERK", RoleScopeType: "TENANT", Status: domain.RoleStatusActive,
	}, "creator-1")
	require.NoError(t, err)

	var p map[string]any
	require.NoError(t, json.Unmarshal(decode(t, out.Body).Payload, &p))
	assert.Equal(t, "role-7", p["role_id"])
	assert.Equal(t, "AP_CLERK", p["role_code"])
}

func TestBundleUpdated_PayloadNamesItsRole(t *testing.T) {
	out, err := events.BundleUpdated(domain.PermissionBundleDef{
		BundleID: "bundle-1", RoleDefinitionID: "role-9", TenantID: "tenant-1",
		BundleCode: "PO_FULL", PermittedActions: []string{"PO_ISSUE"}, ActiveFlag: true,
		CorrelationID: "corr-2",
	}, "actor-1")
	require.NoError(t, err)

	var p struct {
		BundleID string   `json:"bundle_id"`
		RoleID   string   `json:"role_id"`
		Actions  []string `json:"permitted_actions"`
		Active   bool     `json:"active_flag"`
	}
	require.NoError(t, json.Unmarshal(decode(t, out.Body).Payload, &p))
	assert.Equal(t, "bundle-1", p.BundleID)
	assert.Equal(t, "role-9", p.RoleID, "a bundle event that cannot name its role forces the consumer into a second lookup")
	assert.Equal(t, []string{"PO_ISSUE"}, p.Actions)
	assert.True(t, p.Active)
}

func TestBuild_RepeatEventsOnSameAggregate_GetDistinctEventIDs(t *testing.T) {
	role := domain.RoleDefinition{RoleDefinitionID: "role-1", TenantID: "tenant-1", CorrelationID: "corr-x"}

	first, err := events.RoleCreated(role, "creator-1")
	require.NoError(t, err)
	second, err := events.RoleCreated(role, "creator-1")
	require.NoError(t, err)

	assert.NotEqual(t, decode(t, first.Body).EventID, decode(t, second.Body).EventID)
}

func TestPublish_BatchesInOneWrite(t *testing.T) {
	w := &fakeWriter{}
	p := events.NewPublisherWithWriter(zap.NewNop(), "zoiko.access-control.events", w)

	require.NoError(t, p.Publish(context.Background(), []kafka.Message{
		{Key: []byte("a"), Value: []byte(`{}`)},
		{Key: []byte("b"), Value: []byte(`{}`)},
	}))
	assert.Len(t, w.msgs, 2)
}

// TestPublish_ReturnsTheError is the whole reason the relay can be correct.
//
// The publish this replaced logged its error and returned nothing, so the
// caller could not tell a delivered event from a discarded one — and the
// handler, having already committed, had nothing it could have done about it
// anyway.
func TestPublish_ReturnsTheError(t *testing.T) {
	w := &fakeWriter{err: assertErr{}}
	p := events.NewPublisherWithWriter(zap.NewNop(), "zoiko.access-control.events", w)

	err := p.Publish(context.Background(), []kafka.Message{{Value: []byte(`{}`)}})
	require.Error(t, err, "a swallowed publish error is an event silently lost")
}

type assertErr struct{}

func (assertErr) Error() string { return "broker unavailable" }

func TestPublish_NilProducer_DoesNotPanic(t *testing.T) {
	p := events.NewPublisher(zap.NewNop(), "zoiko.access-control.events", nil)
	require.NoError(t, p.Publish(context.Background(), []kafka.Message{{Value: []byte(`{}`)}}))
}

func TestPublish_EmptyBatchIsANoOp(t *testing.T) {
	w := &fakeWriter{err: assertErr{}}
	p := events.NewPublisherWithWriter(zap.NewNop(), "zoiko.access-control.events", w)
	require.NoError(t, p.Publish(context.Background(), nil))
}
