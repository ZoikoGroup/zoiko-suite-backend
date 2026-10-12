//go:build integration

package store_test

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"zoiko.io/access-control-svc/internal/domain"
	svcmiddleware "zoiko.io/access-control-svc/internal/middleware"
	"zoiko.io/access-control-svc/internal/store"
)

// Migration 000013: end dates, effective-dated revocation and the expiry
// sweep, through the NOBYPASSRLS app pool.

func provisionedAssignment(t *testing.T, s *store.PgStore, tenant string, end *time.Time) *domain.AssignmentRequest {
	t.Helper()
	ctx := svcmiddleware.WithTenant(context.Background(), tenant)
	roleID := seedRole(t, tenant, "EXP_"+uuid.NewString()[:8])
	start := time.Now().UTC().Add(-time.Hour)
	id := uuid.NewString()
	a := &domain.AssignmentRequest{RequestID: id, TenantID: tenant, TargetPrincipalID: "subject-" + id[:6],
		RoleDefinitionID: roleID, LegalEntityID: uuid.NewString(), EffectiveFrom: start, EffectiveTo: end, Justification: "j",
		RiskTier: domain.RiskStandard, Status: domain.AssignmentProvisioned, AuthzAssignmentID: id,
		RequestedByPrincipalID: "requester", CorrelationID: uuid.NewString(), CreatedAt: start, UpdatedAt: start}
	created, err := s.CreateAssignmentRequest(ctx, a, "requester")
	require.NoError(t, err)
	require.True(t, created)
	return a
}

// pushEnd moves a row's end into the past as the owner, standing in for time
// passing (the CHECK keeps effective_to after effective_from).
func pushEnd(t *testing.T, requestID string) {
	t.Helper()
	_, err := ownerPool.Exec(context.Background(),
		`UPDATE assignment_requests SET effective_to = now() - interval '1 second' WHERE request_id = $1`, requestID)
	require.NoError(t, err)
}

func TestEndDateIsStoredAndMustFollowTheStart(t *testing.T) {
	s := store.New(appPool)
	end := time.Now().UTC().Add(24 * time.Hour).Truncate(time.Microsecond)
	a := provisionedAssignment(t, s, tenantGov, &end)
	got, err := s.GetAssignmentRequest(svcmiddleware.WithTenant(context.Background(), tenantGov), a.RequestID)
	require.NoError(t, err)
	require.NotNil(t, got.EffectiveTo)
	require.True(t, got.EffectiveTo.Equal(end))

	_, err = ownerPool.Exec(context.Background(),
		`UPDATE assignment_requests SET effective_to = effective_from - interval '1 day' WHERE request_id = $1`, a.RequestID)
	require.ErrorContains(t, err, "assignment_requests_end_after_start")
}

func TestScheduledEndOnlyMovesEarlier(t *testing.T) {
	s := store.New(appPool)
	ctx := svcmiddleware.WithTenant(context.Background(), tenantGov)
	end := time.Now().UTC().Add(48 * time.Hour).Truncate(time.Microsecond)
	a := provisionedAssignment(t, s, tenantGov, &end)

	sooner := end.Add(-24 * time.Hour)
	out, err := s.ScheduleAssignmentEnd(ctx, a.RequestID, "admin", "moves team", sooner)
	require.NoError(t, err)
	require.Equal(t, domain.AssignmentProvisioned, out.Status, "stays provisioned until the instant")
	require.True(t, out.EffectiveTo.Equal(sooner))
	require.Equal(t, "admin", out.RevokedByPrincipalID)

	later := end.Add(24 * time.Hour)
	out, err = s.ScheduleAssignmentEnd(ctx, a.RequestID, "admin", "later", later)
	require.NoError(t, err)
	require.True(t, out.EffectiveTo.Equal(sooner), "a schedule never extends access")
}

func TestExpirySweepClosesDueRowsAcrossTenantsAndEnqueues(t *testing.T) {
	s := store.New(appPool)
	future := time.Now().UTC().Add(time.Hour)

	// Tenant gov: one end set at grant time, one scheduled by a revoke.
	granted := provisionedAssignment(t, s, tenantGov, &future)
	scheduled := provisionedAssignment(t, s, tenantGov, nil)
	_, err := s.ScheduleAssignmentEnd(svcmiddleware.WithTenant(context.Background(), tenantGov), scheduled.RequestID, "admin-9", "contract ended", future)
	require.NoError(t, err)
	// Tenant B: due as well; the sweep crosses tenants.
	other := provisionedAssignment(t, s, tenantB, &future)
	// Not due: must be left alone.
	notDue := provisionedAssignment(t, s, tenantGov, &future)

	for _, id := range []string{granted.RequestID, scheduled.RequestID, other.RequestID} {
		pushEnd(t, id)
	}

	n, err := s.ExpireDueAssignments(context.Background(), 100)
	require.NoError(t, err)
	require.GreaterOrEqual(t, n, 3)

	status := func(id string) (st, revokedBy string) {
		require.NoError(t, ownerPool.QueryRow(context.Background(),
			`SELECT status, COALESCE(revoked_by_principal_id, '') FROM assignment_requests WHERE request_id = $1`, id).Scan(&st, &revokedBy))
		return
	}
	st, _ := status(granted.RequestID)
	require.Equal(t, domain.AssignmentExpired, st, "an end set at grant time expires")
	st, by := status(scheduled.RequestID)
	require.Equal(t, domain.AssignmentRevoked, st, "an end scheduled by a revoke is a revocation")
	require.Equal(t, "admin-9", by)
	st, _ = status(other.RequestID)
	require.Equal(t, domain.AssignmentExpired, st)
	st, _ = status(notDue.RequestID)
	require.Equal(t, domain.AssignmentProvisioned, st)

	// Each closed row enqueued iam.assignment.revoked, in its own tenant,
	// naming the subject (identity-context-svc ends that principal's sessions).
	for _, a := range []*domain.AssignmentRequest{granted, scheduled, other} {
		var tenant, subject, actor string
		require.NoError(t, ownerPool.QueryRow(context.Background(), `
			SELECT tenant_id, payload->'payload'->>'principal_id', payload->>'actor_id' FROM event_outbox
			 WHERE event_type = 'iam.assignment.revoked' AND aggregate_key = $1`, a.RequestID).Scan(&tenant, &subject, &actor))
		require.Equal(t, a.TenantID, tenant)
		require.Equal(t, a.TargetPrincipalID, subject)
		require.NotEmpty(t, actor)
	}

	// A second pass finds nothing more for these rows.
	_, err = s.ExpireDueAssignments(context.Background(), 100)
	require.NoError(t, err)
	var events int
	require.NoError(t, ownerPool.QueryRow(context.Background(),
		`SELECT count(*) FROM event_outbox WHERE event_type = 'iam.assignment.revoked' AND aggregate_key = $1`, granted.RequestID).Scan(&events))
	require.Equal(t, 1, events, "an expiry is announced once")
}

// The sweep's disjunct is SELECT-only: naming it does not let the app write
// another tenant's rows.
func TestExpiryScanDisjunctCannotWrite(t *testing.T) {
	s := store.New(appPool)
	future := time.Now().UTC().Add(time.Hour)
	a := provisionedAssignment(t, s, tenantGov, &future)
	ctx := context.Background()
	tx, err := appPool.Begin(ctx)
	require.NoError(t, err)
	defer func() { _ = tx.Rollback(ctx) }()
	_, err = tx.Exec(ctx, "SELECT set_config('app.assignment_expiry', 'true', true)")
	require.NoError(t, err)
	var seen int
	require.NoError(t, tx.QueryRow(ctx, `SELECT count(*) FROM assignment_requests WHERE request_id = $1`, a.RequestID).Scan(&seen))
	require.Equal(t, 1, seen, "the scan can find it")
	tag, err := tx.Exec(ctx, `UPDATE assignment_requests SET status = 'CANCELLED' WHERE request_id = $1`, a.RequestID)
	require.NoError(t, err)
	require.Equal(t, int64(0), tag.RowsAffected(), "but not change it")
}
