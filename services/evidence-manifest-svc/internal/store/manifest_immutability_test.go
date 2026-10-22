package store_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"zoiko.io/evidence-manifest-svc/internal/domain"
	"zoiko.io/evidence-manifest-svc/internal/store"
)

// A generated (or failed) evidence manifest is immutable; only the PENDING ->
// GENERATED/FAILED lifecycle may change the row, and only once.

const entityA = "22222222-2222-2222-2222-222222222222"

func generated(t *testing.T, s *store.PgStore) *domain.EvidenceManifest {
	t.Helper()
	m := &domain.EvidenceManifest{TenantID: testTenant, LegalEntityID: entityA, ScenarioType: domain.ScenarioAudit, RequestedBy: "principal-1"}
	out, err := s.CompleteManifest(tenantCtx(testTenant), m, []domain.ManifestRecord{
		{SourceType: domain.SourceGovernanceDecision, SourceRecordID: "gd-1", RecordSnapshot: []byte(`{"a":1}`)}}, "corr-imm")
	require.NoError(t, err, "normal generation still works with the trigger in place")
	require.Equal(t, domain.StatusGenerated, out.Status)
	return out
}

func TestManifestRow_GeneratedIsImmutable_ReadsStillWork(t *testing.T) {
	pool := requireTestDB(t)
	s := store.New(pool, zap.NewNop())
	m := generated(t, s)
	ctx := context.Background()

	for name, stmt := range map[string]string{
		"rewrite the checksum":    `UPDATE evidence_manifests SET checksum_sha256 = 'forged' WHERE manifest_id = $1`,
		"flip the status":         `UPDATE evidence_manifests SET status = 'FAILED', failure_reason = 'x' WHERE manifest_id = $1`,
		"change the legal entity": `UPDATE evidence_manifests SET legal_entity_id = '33333333-3333-3333-3333-333333333333' WHERE manifest_id = $1`,
		"change the requester":    `UPDATE evidence_manifests SET requested_by = 'someone-else' WHERE manifest_id = $1`,
		"touch generated_at":      `UPDATE evidence_manifests SET generated_at = now() WHERE manifest_id = $1`,
		"delete the manifest":     `DELETE FROM evidence_manifests WHERE manifest_id = $1`,
	} {
		_, err := pool.Exec(ctx, stmt, m.ManifestID)
		assert.Error(t, err, "must be rejected: %s", name)
	}

	got, err := s.FindManifestByID(tenantCtx(testTenant), m.ManifestID)
	require.NoError(t, err)
	assert.Equal(t, domain.StatusGenerated, got.Status)
	require.NotNil(t, got.ChecksumSHA256)
	assert.Equal(t, *m.ChecksumSHA256, *got.ChecksumSHA256, "the checksum is untouched")
	recs, err := s.ListRecords(tenantCtx(testTenant), m.ManifestID)
	require.NoError(t, err)
	assert.Len(t, recs, 1)
}

func TestManifestRow_FailedIsTerminal(t *testing.T) {
	pool := requireTestDB(t)
	s := store.New(pool, zap.NewNop())
	failed, err := s.RecordFailedManifest(tenantCtx(testTenant),
		&domain.EvidenceManifest{TenantID: testTenant, LegalEntityID: entityA, ScenarioType: domain.ScenarioAudit, RequestedBy: "p"}, "source down")
	require.NoError(t, err, "a FAILED manifest is still recorded")
	_, err = pool.Exec(context.Background(), `UPDATE evidence_manifests SET status = 'GENERATED', checksum_sha256 = 'x' WHERE manifest_id = $1`, failed.ManifestID)
	assert.Error(t, err, "a failed manifest is never resurrected; a retry makes a new one")
	_, err = pool.Exec(context.Background(), `DELETE FROM evidence_manifests WHERE manifest_id = $1`, failed.ManifestID)
	assert.Error(t, err)
}

func TestManifestRow_PendingKeepsItsLifecycle_ButNotItsIdentity(t *testing.T) {
	pool := requireTestDB(t)
	s := store.New(pool, zap.NewNop())
	m := &domain.EvidenceManifest{TenantID: testTenant, LegalEntityID: entityA, ScenarioType: domain.ScenarioRegulator, RequestedBy: "principal-1"}
	require.NoError(t, s.CreateManifest(tenantCtx(testTenant), m))

	_, err := pool.Exec(context.Background(), `UPDATE evidence_manifests SET requested_by = 'forged' WHERE manifest_id = $1`, m.ManifestID)
	assert.Error(t, err, "attribution cannot be rewritten even while PENDING")

	fin, err := s.FinalizeGenerated(tenantCtx(testTenant), m.ManifestID, "chk")
	require.NoError(t, err, "the one permitted transition")
	assert.Equal(t, domain.StatusGenerated, fin.Status)
	_, err = s.FinalizeGenerated(tenantCtx(testTenant), m.ManifestID, "other")
	assert.Error(t, err, "and only once")
}

func TestManifestList_ForEntities_IsTenantScopedAndPaged(t *testing.T) {
	pool := requireTestDB(t)
	s := store.New(pool, zap.NewNop())
	generated(t, s)
	generated(t, s)

	ents, err := s.ListManifestLegalEntities(tenantCtx(testTenant))
	require.NoError(t, err)
	assert.Equal(t, []string{entityA}, ents)

	got, err := s.ListManifestsForEntities(tenantCtx(testTenant), []string{entityA}, 1, 0)
	require.NoError(t, err)
	assert.Len(t, got, 1, "limit applies after the entity restriction")
	none, err := s.ListManifestsForEntities(tenantCtx(testTenant), []string{"44444444-4444-4444-4444-444444444444"}, 10, 0)
	require.NoError(t, err)
	assert.Empty(t, none)

	otherEnts, err := s.ListManifestLegalEntities(tenantCtx("99999999-9999-9999-9999-999999999999"))
	require.NoError(t, err)
	assert.Empty(t, otherEnts, "another tenant sees none of this tenant's entities")
	otherList, err := s.ListManifestsForEntities(tenantCtx("99999999-9999-9999-9999-999999999999"), []string{entityA}, 10, 0)
	require.NoError(t, err)
	assert.Empty(t, otherList, "row-level security holds even when the entity id is named")
}
