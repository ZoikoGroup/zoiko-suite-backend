package lifecycle_test

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"zoiko.io/identity-context-svc/internal/domain"
	"zoiko.io/identity-context-svc/internal/events"
	"zoiko.io/identity-context-svc/internal/lifecycle"
	"zoiko.io/identity-context-svc/internal/outbox"
)

// SetStatus is what PUT /v1/principals/{id}/status runs since 19 Oct 2026
// (Group 1 re-audit F-3). authorization-svc's principal-status projection
// consumes principal.status.changed, and ZS-IAM-001 names identity.subject.*,
// so a change emits both; a disable or suspension then ends every session.

// statusStore implements only what SetStatus calls; any other method panics
// through the nil embedded interface, which would fail the test loudly.
type statusStore struct {
	lifecycle.Store
	changed bool
	calls   int
	status  domain.PrincipalStatus
	recs    []outbox.Record
}

func (s *statusStore) SetPrincipalLifecycle(_ context.Context, _, _ string, status domain.PrincipalStatus,
	_, _, _ string, recs ...outbox.Record) (bool, error) {
	s.calls++
	s.status, s.recs = status, recs
	return s.changed, nil
}

type enderCall struct{ tenantID, principalID, reason, actor string }

func newService(st *statusStore, ended *[]enderCall) *lifecycle.Service {
	end := func(_ context.Context, tenantID, principalID, reason, actor, _ string) (int, error) {
		*ended = append(*ended, enderCall{tenantID, principalID, reason, actor})
		return 1, nil
	}
	return lifecycle.New(st, nil, lifecycle.Policy{MinLength: 12}, nil, end, lifecycle.Config{}, nil, nil)
}

func eventTypes(t *testing.T, recs []outbox.Record) []string {
	t.Helper()
	var out []string
	for _, r := range recs {
		out = append(out, r.EventType)
		var env map[string]any
		require.NoError(t, json.Unmarshal(r.Payload, &env))
		assert.Equal(t, "t-1", env["tenant_id"])
	}
	return out
}

var admin = lifecycle.Actor{PrincipalID: "admin-1", TenantID: "t-1", CorrelationID: "corr-1"}

func TestSetStatus_DisableEmitsBothNamesAndEndsSessions(t *testing.T) {
	st := &statusStore{changed: true}
	var ended []enderCall
	require.NoError(t, newService(st, &ended).SetStatus(context.Background(), admin, "p-1", domain.PrincipalStatusDisabled, "left"))

	assert.Equal(t, domain.PrincipalStatusDisabled, st.status)
	assert.Equal(t, []string{events.EventPrincipalStatusChanged, events.EventSubjectDisabled}, eventTypes(t, st.recs))
	require.Len(t, ended, 1, "a disable ends every session the principal holds")
	assert.Equal(t, enderCall{"t-1", "p-1", domain.SessionEndDeprovisioned, "admin-1"}, ended[0])
}

func TestSetStatus_SuspendEndsSessionsToo(t *testing.T) {
	st := &statusStore{changed: true}
	var ended []enderCall
	require.NoError(t, newService(st, &ended).SetStatus(context.Background(), admin, "p-1", domain.PrincipalStatusSuspended, "investigation"))
	assert.Equal(t, []string{events.EventPrincipalStatusChanged, events.EventSubjectDisabled}, eventTypes(t, st.recs))
	assert.Len(t, ended, 1)
}

func TestSetStatus_EnableEmitsEnabledAndEndsNothing(t *testing.T) {
	st := &statusStore{changed: true}
	var ended []enderCall
	require.NoError(t, newService(st, &ended).SetStatus(context.Background(), admin, "p-1", domain.PrincipalStatusActive, "back"))
	assert.Equal(t, []string{events.EventPrincipalStatusChanged, events.EventSubjectEnabled}, eventTypes(t, st.recs))
	assert.Empty(t, ended)
}

// Re-applying a status changes nothing, so there are no sessions to end.
func TestSetStatus_NoChangeEndsNothing(t *testing.T) {
	st := &statusStore{changed: false}
	var ended []enderCall
	require.NoError(t, newService(st, &ended).SetStatus(context.Background(), admin, "p-1", domain.PrincipalStatusDisabled, "again"))
	assert.Empty(t, ended)
}

func TestSetStatus_RefusesSelfDisableAndUnknownStatus(t *testing.T) {
	st := &statusStore{changed: true}
	var ended []enderCall
	svc := newService(st, &ended)

	err := svc.SetStatus(context.Background(), admin, "admin-1", domain.PrincipalStatusDisabled, "oops")
	require.ErrorIs(t, err, lifecycle.ErrForbidden)

	err = svc.SetStatus(context.Background(), admin, "p-1", domain.PrincipalStatus("DELETED"), "x")
	require.ErrorIs(t, err, lifecycle.ErrInvalid)

	assert.Zero(t, st.calls, "a refused change writes nothing")
	assert.Empty(t, ended)
}
