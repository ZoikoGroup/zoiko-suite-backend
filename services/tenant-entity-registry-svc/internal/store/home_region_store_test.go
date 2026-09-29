package store_test

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"zoiko.io/tenant-entity-registry-svc/internal/domain"
	"zoiko.io/tenant-entity-registry-svc/internal/events"
	"zoiko.io/tenant-entity-registry-svc/internal/registry"
)

// Migration 000012 against a real Postgres: an approved ChangeHomeRegion
// re-points the default policy's region, bumps the tenant, records the
// decision evidence and enqueues the event — together.

func seedRegion(t *testing.T, f *orgFixture, active bool) string {
	t.Helper()
	id := uuid.New().String()
	_, err := f.pool.Exec(context.Background(), `
		INSERT INTO residency_regions (residency_region_id, region_code, region_name, cloud_provider,
		       country_code, sovereign_flag, active_flag, created_at, updated_at,
		       created_by_principal_id, updated_by_principal_id)
		VALUES ($1, $2, 'Drill region', 'gcp', 'GB', false, $3, now(), now(), 'seed', 'seed')`,
		id, "rg-"+id[:8], active)
	require.NoError(t, err)
	return id
}

func pendingHomeRegionApproval(t *testing.T, f *orgFixture) domain.ApprovalDecision {
	t.Helper()
	id := uuid.New().String()
	now := time.Now().UTC()
	require.NoError(t, f.s.CreateApprovalRequest(f.ctx, &domain.ApprovalRequest{
		ApprovalRequestID: id, TenantID: f.tenantID, SubjectType: domain.ApprovalSubjectTenantHomeRegion,
		SubjectID: f.tenantID, CommandName: string(domain.TenantCommandChangeHomeRegion),
		Payload: json.RawMessage(`{}`), ExpectedVersion: 1, PayloadFingerprint: "fp",
		Reason: "r", RequestedByPrincipalID: "p-maker", RequestedAt: now,
		ExpiresAt: now.Add(time.Hour), Status: domain.ApprovalPending,
	}))
	return domain.ApprovalDecision{ApprovalRequestID: id, TenantID: f.tenantID, DecidedByPrincipalID: "p-checker"}
}

func TestChangeHomeRegion_AppliesAtomicallyWithEvidence(t *testing.T) {
	f := newORGFixture(t)
	region := seedRegion(t, f, true)
	d := pendingHomeRegionApproval(t, f)
	ev, err := events.BuildRecord(events.RecordSpec{
		EventType: events.EventTenantHomeRegionChanged, TenantID: f.tenantID,
		ObjectID: f.tenantID, ObjectVersion: 2, Payload: map[string]any{"to_region_id": region},
	})
	require.NoError(t, err)

	res, err := f.s.ChangeHomeRegion(f.ctx, registry.HomeRegionChange{
		TenantID: f.tenantID, ResidencyRegionID: region, DecisionRef: "RES-DECISION-1",
		Reason: "EU residency", ActorID: "p-maker", ExpectedVersion: 1, Approval: d,
	}, ev)
	require.NoError(t, err)
	assert.EqualValues(t, 2, res.NewVersion)

	var got string
	require.NoError(t, f.pool.QueryRow(context.Background(),
		`SELECT residency_region_id::text FROM data_residency_policies WHERE data_residency_policy_id = $1`,
		f.policyID).Scan(&got))
	assert.Equal(t, region, got, "the home-region pointer moved")

	history, err := f.s.ListTenantLifecycleHistory(f.ctx, f.tenantID)
	require.NoError(t, err)
	require.NotEmpty(t, history)
	assert.Equal(t, domain.TenantCommandChangeHomeRegion, history[0].CommandName)
	require.NotNil(t, history[0].HomeRegionDecisionRef)
	assert.Equal(t, "RES-DECISION-1", *history[0].HomeRegionDecisionRef)
	assert.Equal(t, 1, outboxCount(t, f, events.EventTenantHomeRegionChanged))
}

func TestChangeHomeRegion_RefusesAnInactiveRegionAndChangesNothing(t *testing.T) {
	f := newORGFixture(t)
	region := seedRegion(t, f, false)
	d := pendingHomeRegionApproval(t, f)

	_, err := f.s.ChangeHomeRegion(f.ctx, registry.HomeRegionChange{
		TenantID: f.tenantID, ResidencyRegionID: region, DecisionRef: "RES-1",
		Reason: "r", ActorID: "p-maker", ExpectedVersion: 1, Approval: d,
	}, nil)
	require.ErrorIs(t, err, registry.ErrReferenceInvalid)

	a, err := f.s.GetApprovalRequest(f.ctx, d.ApprovalRequestID)
	require.NoError(t, err)
	assert.Equal(t, domain.ApprovalPending, a.Status, "the approval is not spent on a refused change")
}

// tlh_home_region_evidenced: the database itself refuses a ChangeHomeRegion
// lineage row without its decision reference.
func TestChangeHomeRegion_DatabaseRefusesUnevidencedLineage(t *testing.T) {
	f := newORGFixture(t)
	_, err := f.pool.Exec(context.Background(), `
		INSERT INTO tenant_lifecycle_history (lifecycle_event_id, tenant_id, to_state, command_name,
		       reason, actor_principal_id, occurred_at)
		VALUES ($1, $2, 'ACTIVE', 'ChangeHomeRegion', 'r', 'p', now())`,
		uuid.New().String(), f.tenantID)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "tlh_home_region_evidenced")
}
