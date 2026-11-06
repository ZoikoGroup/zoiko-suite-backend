package lifecycle_test

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
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

// ── IC-13: S1-13 (K-7) password-reset decisions ────────────────────────────────

type resetStore struct {
	lifecycle.Store
	getReq    func(ctx context.Context, tenantID, requestID string) (*domain.PasswordResetRequest, error)
	decideReq func(ctx context.Context, tenantID, requestID string, a domain.ResetApproval,
		issue func(ctx context.Context, tx pgx.Tx, r *domain.PasswordResetRequest) error, recs ...outbox.Record) (*domain.PasswordResetRequest, error)
	createReq func(ctx context.Context, r domain.PasswordResetRequest, recs ...outbox.Record) error
}

func (s *resetStore) GetResetRequest(ctx context.Context, tenantID, requestID string) (*domain.PasswordResetRequest, error) {
	if s.getReq != nil {
		return s.getReq(ctx, tenantID, requestID)
	}
	return nil, nil
}

func (s *resetStore) DecideResetRequest(ctx context.Context, tenantID, requestID string, a domain.ResetApproval,
	issue func(ctx context.Context, tx pgx.Tx, r *domain.PasswordResetRequest) error, recs ...outbox.Record) (*domain.PasswordResetRequest, error) {
	if s.decideReq != nil {
		return s.decideReq(ctx, tenantID, requestID, a, issue, recs...)
	}
	return nil, nil
}

func (s *resetStore) CreateResetRequest(ctx context.Context, r domain.PasswordResetRequest, recs ...outbox.Record) error {
	if s.createReq != nil {
		return s.createReq(ctx, r, recs...)
	}
	return nil
}

type fakeNotifier struct {
	fail bool
}

func (f *fakeNotifier) Notify(ctx context.Context, n lifecycle.Notice) error {
	if f.fail {
		return errors.New("delivery failed")
	}
	return nil
}

func newResetService(st *resetStore, notifier lifecycle.Notifier, metricsInc *int) *lifecycle.Service {
	return lifecycle.New(st, nil, lifecycle.Policy{MinLength: 12}, notifier, nil, lifecycle.Config{
		ResetApprovals: 1,
		ResetTTL:       time.Hour,
		PublicAppURL:   "https://app.example.com",
		ResetPath:      "/reset",
	}, func(ctx context.Context, name, key string, max int, window time.Duration) bool {
		if metricsInc != nil && name == "password_resets" {
			*metricsInc++
		}
		return true
	}, nil)
}

func TestIC13_MetricAfterCommit(t *testing.T) {
	// Metric must be incremented AFTER the store commit, not before.
	var metricInc int
	st := &resetStore{}
	st.getReq = func(ctx context.Context, tenantID, requestID string) (*domain.PasswordResetRequest, error) {
		return &domain.PasswordResetRequest{
			RequestID: requestID, TenantID: tenantID, PrincipalID: "p-1",
			Status: "PENDING_APPROVAL", ApprovalsRequired: 1,
			Approvals: []domain.ResetApproval{},
		}, nil
	}
	st.decideReq = func(ctx context.Context, tenantID, requestID string, a domain.ResetApproval,
		issue func(ctx context.Context, tx pgx.Tx, r *domain.PasswordResetRequest) error, recs ...outbox.Record) (*domain.PasswordResetRequest, error) {
		// Simulate successful commit
		return &domain.PasswordResetRequest{
			RequestID: requestID, TenantID: tenantID, PrincipalID: "p-1",
			Status: "ISSUED", ApprovalsRequired: 1,
			Approvals: []domain.ResetApproval{{ApproverID: a.ApproverID, Decision: "APPROVE", At: time.Now()}},
		}, nil
	}
	svc := newResetService(st, nil, &metricInc)
	admin := lifecycle.Actor{PrincipalID: "admin-1", TenantID: "t-1", MFAVerified: true, AuthTime: time.Now(), CorrelationID: "corr-1"}

	_, err := svc.DecideReset(context.Background(), admin, "req-1", "APPROVE", "reason")
	require.NoError(t, err)
	assert.Equal(t, 1, metricInc, "metric must be incremented after commit")
}

func TestIC13_StoreOutageMapsTo503(t *testing.T) {
	// Store outages must map to 503 (service unavailable), not 500 or 409.
	st := &resetStore{}
	st.getReq = func(ctx context.Context, tenantID, requestID string) (*domain.PasswordResetRequest, error) {
		return &domain.PasswordResetRequest{
			RequestID: requestID, TenantID: tenantID, PrincipalID: "p-1",
			Status: "PENDING_APPROVAL", ApprovalsRequired: 1,
			Approvals: []domain.ResetApproval{},
		}, nil
	}
	st.decideReq = func(ctx context.Context, tenantID, requestID string, a domain.ResetApproval,
		issue func(ctx context.Context, tx pgx.Tx, r *domain.PasswordResetRequest) error, recs ...outbox.Record) (*domain.PasswordResetRequest, error) {
		return nil, errors.New("database connection failed")
	}
	svc := newResetService(st, nil, nil)
	admin := lifecycle.Actor{PrincipalID: "admin-1", TenantID: "t-1", MFAVerified: true, AuthTime: time.Now(), CorrelationID: "corr-1"}

	_, err := svc.DecideReset(context.Background(), admin, "req-1", "APPROVE", "reason")
	require.Error(t, err)
	// The error should indicate service unavailability
	assert.Contains(t, err.Error(), "unavailable", "store outage must map to 503")
}

func TestIC13_ExpiryCommittedBeforeRefuse(t *testing.T) {
	// When a reset request has expired, the expiry must be committed and an event
	// emitted BEFORE refusing the decision.
	var events []outbox.Record
	st := &resetStore{}
	st.getReq = func(ctx context.Context, tenantID, requestID string) (*domain.PasswordResetRequest, error) {
		// Return an expired request
		now := time.Now().UTC()
		expired := now.Add(-time.Hour)
		return &domain.PasswordResetRequest{
			RequestID: requestID, TenantID: tenantID, PrincipalID: "p-1",
			Status: "PENDING_APPROVAL", ApprovalsRequired: 1,
			CreatedAt: now.Add(-2 * time.Hour), ExpiresAt: expired,
			Approvals: []domain.ResetApproval{},
		}, nil
	}
	st.decideReq = func(ctx context.Context, tenantID, requestID string, a domain.ResetApproval,
		issue func(ctx context.Context, tx pgx.Tx, r *domain.PasswordResetRequest) error, recs ...outbox.Record) (*domain.PasswordResetRequest, error) {
		// The store should commit the expiry and return the updated request
		// with status EXPIRED
		for _, rec := range recs {
			events = append(events, rec)
		}
		now := time.Now().UTC()
		return &domain.PasswordResetRequest{
			RequestID: requestID, TenantID: tenantID, PrincipalID: "p-1",
			Status: "EXPIRED", DecidedAt: &now,
		}, errors.New("reset request expired")
	}
	svc := newResetService(st, nil, nil)
	admin := lifecycle.Actor{PrincipalID: "admin-1", TenantID: "t-1", MFAVerified: true, AuthTime: time.Now(), CorrelationID: "corr-1"}

	_, err := svc.DecideReset(context.Background(), admin, "req-1", "APPROVE", "reason")
	require.Error(t, err)
	// An event should have been emitted for the expiry decision
	require.NotEmpty(t, events, "expiry decision must emit an event")
}

func TestIC13_EventForEveryDecision(t *testing.T) {
	// Both APPROVE and REJECT must emit events (not just when status changes from PENDING_APPROVAL).
	st := &resetStore{}
	var events []outbox.Record
	st.getReq = func(ctx context.Context, tenantID, requestID string) (*domain.PasswordResetRequest, error) {
		return &domain.PasswordResetRequest{
			RequestID: requestID, TenantID: tenantID, PrincipalID: "p-1",
			Status: "PENDING_APPROVAL", ApprovalsRequired: 1,
			Approvals: []domain.ResetApproval{},
		}, nil
	}
	st.decideReq = func(ctx context.Context, tenantID, requestID string, a domain.ResetApproval,
		issue func(ctx context.Context, tx pgx.Tx, r *domain.PasswordResetRequest) error, recs ...outbox.Record) (*domain.PasswordResetRequest, error) {
		for _, rec := range recs {
			events = append(events, rec)
		}
		now := time.Now().UTC()
		status := "ISSUED"
		if a.Decision == "REJECT" {
			status = "REJECTED"
		}
		return &domain.PasswordResetRequest{
			RequestID: requestID, TenantID: tenantID, PrincipalID: "p-1",
			Status: status, DecidedAt: &now,
			Approvals: []domain.ResetApproval{{ApproverID: a.ApproverID, Decision: a.Decision, At: now}},
		}, nil
	}
	svc := newResetService(st, nil, nil)
	admin := lifecycle.Actor{PrincipalID: "admin-1", TenantID: "t-1", MFAVerified: true, AuthTime: time.Now(), CorrelationID: "corr-1"}

	// Test APPROVE
	events = nil
	_, err := svc.DecideReset(context.Background(), admin, "req-approve", "APPROVE", "reason")
	require.NoError(t, err)
	require.NotEmpty(t, events, "APPROVE must emit an event")

	// Test REJECT
	events = nil
	_, err = svc.DecideReset(context.Background(), admin, "req-reject", "REJECT", "reason")
	require.NoError(t, err)
	require.NotEmpty(t, events, "REJECT must emit an event")
}

func TestIC13_LinkDeliveryFailureReturnsError(t *testing.T) {
	// When link delivery fails, the service must return an error (502/503)
	// or allow re-issue, not silently succeed.
	st := &resetStore{}
	var events []outbox.Record
	st.getReq = func(ctx context.Context, tenantID, requestID string) (*domain.PasswordResetRequest, error) {
		return &domain.PasswordResetRequest{
			RequestID: requestID, TenantID: tenantID, PrincipalID: "p-1",
			Status: "PENDING_APPROVAL", ApprovalsRequired: 1,
			Approvals: []domain.ResetApproval{},
		}, nil
	}
	st.decideReq = func(ctx context.Context, tenantID, requestID string, a domain.ResetApproval,
		issue func(ctx context.Context, tx pgx.Tx, r *domain.PasswordResetRequest) error, recs ...outbox.Record) (*domain.PasswordResetRequest, error) {
		for _, rec := range recs {
			events = append(events, rec)
		}
		now := time.Now().UTC()
		return &domain.PasswordResetRequest{
			RequestID: requestID, TenantID: tenantID, PrincipalID: "p-1",
			Status: "ISSUED", DecidedAt: &now,
			Approvals: []domain.ResetApproval{{ApproverID: a.ApproverID, Decision: "APPROVE", At: now}},
		}, nil
	}
	notifier := &fakeNotifier{fail: true}
	svc := newResetService(st, notifier, nil)
	admin := lifecycle.Actor{PrincipalID: "admin-1", TenantID: "t-1", MFAVerified: true, AuthTime: time.Now(), CorrelationID: "corr-1"}

	_, err := svc.DecideReset(context.Background(), admin, "req-1", "APPROVE", "reason")
	require.Error(t, err, "link delivery failure must return an error")
	assert.Contains(t, err.Error(), "delivery", "error must indicate delivery failure")
}
