package store_test

import (
	"errors"
	"testing"
	"time"

	"zoiko.io/analytical-data-platform-svc/internal/domain"
)

// DATA-04 Analytical Data Platform, against real Postgres as the
// NOSUPERUSER NOBYPASSRLS role.

// Happy path: CreateDatasetVersion -> BuildSnapshot -> PublishDatasetVersion.
func TestADP_HappyPath_CreateBuildPublish(t *testing.T) {
	f := newFixture(t)
	version := f.createValidatedVersion(orgA, "customer-360")
	if version.Status != domain.VersionValidated {
		t.Fatalf("version status = %s, want Validated", version.Status)
	}
	if version.VersionNumber != 1 {
		t.Fatalf("version number = %d, want 1", version.VersionNumber)
	}

	cert, err := f.s.PublishDatasetVersion(f.ctx, orgA, version.VersionID, "test-operator", f.claim("PublishDatasetVersion", version.VersionID))
	if err != nil {
		t.Fatalf("publish dataset version: %v", err)
	}
	if cert.SummarySHA256 == "" || len(cert.SummarySHA256) != 64 {
		t.Fatalf("summary_sha256 = %q", cert.SummarySHA256)
	}
	if cert.Summary.RowCount != 1000 {
		t.Fatalf("certified row count = %d, want 1000", cert.Summary.RowCount)
	}

	after, err := f.s.GetDatasetVersion(f.ctx, orgA, version.VersionID)
	if err != nil || after.Status != domain.VersionPublished || after.PublishedAt == nil {
		t.Fatalf("version after publish: %+v (err=%v)", after, err)
	}
}

// Negative path #1 (doc-named): published dataset cannot be silently
// rebuilt in place — BuildSnapshot and RebuildPartition are both refused
// once a version is Published; a rebuild must be a brand new version.
func TestADP_PublishedDatasetCannotBeRebuiltInPlace(t *testing.T) {
	f := newFixture(t)
	version := f.createValidatedVersion(orgA, "invoice-facts")
	snapshot, err := f.s.GetLatestSnapshot(f.ctx, orgA, version.VersionID)
	if err != nil {
		t.Fatalf("get latest snapshot: %v", err)
	}
	if _, err := f.s.PublishDatasetVersion(f.ctx, orgA, version.VersionID, "test-operator", f.claim("PublishDatasetVersion", version.VersionID)); err != nil {
		t.Fatalf("publish: %v", err)
	}

	if _, err := f.s.BuildSnapshot(f.ctx, orgA, domain.BuildSnapshotRequest{
		VersionID: version.VersionID, Watermark: "wm-2", RowCount: 2000, ContentHash: hashLike("rebuild-attempt"),
	}, "test-operator", f.claim("BuildSnapshot-after-publish", version.VersionID)); !errors.Is(err, domain.ErrVersionPublished) {
		t.Fatalf("rebuilt a snapshot on a published version: %v", err)
	}
	if _, err := f.s.RebuildPartition(f.ctx, orgA, domain.RebuildPartitionRequest{
		VersionID: version.VersionID, SnapshotID: snapshot.SnapshotID, PartitionKey: "2026-01", RowCount: 100, ContentHash: hashLike("partition-attempt"),
	}, "test-operator", f.claim("RebuildPartition-after-publish", version.VersionID)); !errors.Is(err, domain.ErrVersionPublished) {
		t.Fatalf("rebuilt a partition on a published version: %v", err)
	}

	// The correct path is a brand new version under the same dataset.
	v2, err := f.s.CreateDatasetVersion(f.ctx, orgA, domain.CreateDatasetVersionRequest{
		DatasetName: "invoice-facts", SchemaDefinition: map[string]interface{}{"columns": []string{"id"}},
		TransformationVersion: "test:v2", ResidencyRegion: "us-east", Classification: "internal", MaxStalenessSeconds: 3600,
	}, "test-operator", f.claim("CreateDatasetVersion-v2", "invoice-facts"))
	if err != nil {
		t.Fatalf("create v2: %v", err)
	}
	if v2.VersionNumber != 2 {
		t.Fatalf("v2 version number = %d, want 2", v2.VersionNumber)
	}
	if v2.VersionID == version.VersionID {
		t.Fatal("rebuild reused the published version's ID instead of creating a new one")
	}
}

// Negative path #2 (doc-named): a stale dataset declares a freshness
// breach — staleness is computed against the version's own declared
// bound, server-side, not client-asserted.
func TestADP_StaleDatasetDeclaresFreshnessBreach(t *testing.T) {
	f := newFixture(t)
	version := f.createValidatedVersion(orgA, "daily-summary")

	fresh, err := f.s.GetFreshness(f.ctx, orgA, version.VersionID, time.Now().UTC())
	if err != nil {
		t.Fatalf("get freshness (fresh): %v", err)
	}
	if fresh.IsStale {
		t.Fatalf("freshly-built snapshot reported stale: %+v", fresh)
	}

	// max_staleness_seconds is 3600 (1h) — ask "as of" 2 hours after now,
	// simulating time passing without a rebuild.
	stale, err := f.s.GetFreshness(f.ctx, orgA, version.VersionID, time.Now().UTC().Add(2*time.Hour))
	if err != nil {
		t.Fatalf("get freshness (stale): %v", err)
	}
	if !stale.IsStale {
		t.Fatalf("2h-old snapshot against a 1h staleness bound was not flagged stale: %+v", stale)
	}
	if stale.StalenessSeconds <= stale.MaxStalenessSeconds {
		t.Fatalf("staleness_seconds (%d) should exceed max_staleness_seconds (%d)", stale.StalenessSeconds, stale.MaxStalenessSeconds)
	}
}

// Negative path #3 (doc-named): cross-tenant query does not leak rows or
// metadata — enforced by RLS, not an application-level filter.
func TestADP_CrossTenantIsolation(t *testing.T) {
	f := newFixture(t)
	version := f.createValidatedVersion(orgA, "cross-tenant-check")

	if _, err := f.s.GetDatasetVersion(f.ctx, orgB, version.VersionID); !errors.Is(err, domain.ErrDatasetVersionNotFound) {
		t.Fatalf("cross-tenant version read: %v", err)
	}
	if _, err := f.s.GetLatestSnapshot(f.ctx, orgB, version.VersionID); !errors.Is(err, domain.ErrNoSnapshotYet) {
		t.Fatalf("cross-tenant snapshot read: %v", err)
	}
	own, err := f.s.GetDatasetVersion(f.ctx, orgA, version.VersionID)
	if err != nil || own.VersionID != version.VersionID {
		t.Fatalf("own-tenant read: %+v (err=%v)", own, err)
	}
}

// Idempotent replay: retrying BuildSnapshot with the same claim key must
// not create a second snapshot.
func TestADP_BuildSnapshot_IdempotentReplay(t *testing.T) {
	f := newFixture(t)
	v, err := f.s.CreateDatasetVersion(f.ctx, orgA, domain.CreateDatasetVersionRequest{
		DatasetName: "replay-check", SchemaDefinition: map[string]interface{}{}, TransformationVersion: "test:v1",
		ResidencyRegion: "us-east", Classification: "internal", MaxStalenessSeconds: 3600,
	}, "test-operator", f.claim("CreateDatasetVersion", "replay-check"))
	if err != nil {
		t.Fatalf("create version: %v", err)
	}
	req := domain.BuildSnapshotRequest{VersionID: v.VersionID, Watermark: "wm-1", RowCount: 500, ContentHash: hashLike("replay")}
	claim := f.claim("BuildSnapshot", v.VersionID)
	first, err := f.s.BuildSnapshot(f.ctx, orgA, req, "test-operator", claim)
	if err != nil {
		t.Fatalf("first build: %v", err)
	}
	_, err = f.s.BuildSnapshot(f.ctx, orgA, req, "test-operator", claim)
	var replay *domain.IdempotentReplayError
	if !errors.As(err, &replay) || replay.ResourceID != first.SnapshotID {
		t.Fatalf("replay of BuildSnapshot: %v", err)
	}
}

// DB-level negative control: a sealed data product certification is
// immutable at the database.
func TestADP_CertificationIsImmutableAtTheDatabase(t *testing.T) {
	f := newFixture(t)
	version := f.createValidatedVersion(orgA, "immutability-check")
	cert, err := f.s.PublishDatasetVersion(f.ctx, orgA, version.VersionID, "test-operator", f.claim("PublishDatasetVersion", version.VersionID))
	if err != nil {
		t.Fatalf("publish: %v", err)
	}

	tx, err := f.app.Begin(f.ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer tx.Rollback(f.ctx) //nolint:errcheck
	if _, err := tx.Exec(f.ctx, "SELECT set_config('app.tenant_id', $1, true)", orgA); err != nil {
		t.Fatalf("declare tenant: %v", err)
	}
	if _, err := tx.Exec(f.ctx, `UPDATE data_product_certifications SET summary_sha256 = repeat('0', 64) WHERE certification_id = $1`,
		cert.CertificationID); err == nil {
		t.Fatal("a raw UPDATE against a sealed certification was not rejected")
	}
}

// DB-level negative control: a Published version is immutable outright,
// not just protected on the fields BuildSnapshot happens to check.
func TestADP_PublishedVersionIsImmutableAtTheDatabase(t *testing.T) {
	f := newFixture(t)
	version := f.createValidatedVersion(orgA, "terminal-check")
	if _, err := f.s.PublishDatasetVersion(f.ctx, orgA, version.VersionID, "test-operator", f.claim("PublishDatasetVersion", version.VersionID)); err != nil {
		t.Fatalf("publish: %v", err)
	}

	tx, err := f.app.Begin(f.ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer tx.Rollback(f.ctx) //nolint:errcheck
	if _, err := tx.Exec(f.ctx, "SELECT set_config('app.tenant_id', $1, true)", orgA); err != nil {
		t.Fatalf("declare tenant: %v", err)
	}
	if _, err := tx.Exec(f.ctx, `UPDATE dataset_versions SET transformation_version = 'tampered' WHERE version_id = $1`,
		version.VersionID); err == nil {
		t.Fatal("a raw UPDATE touching an unrelated column on a published version was not rejected")
	}
}

// DeprecateDataset: Published -> Deprecated, then terminal.
func TestADP_DeprecateDataset(t *testing.T) {
	f := newFixture(t)
	version := f.createValidatedVersion(orgA, "deprecate-check")
	if _, err := f.s.PublishDatasetVersion(f.ctx, orgA, version.VersionID, "test-operator", f.claim("PublishDatasetVersion", version.VersionID)); err != nil {
		t.Fatalf("publish: %v", err)
	}
	deprecated, err := f.s.DeprecateDataset(f.ctx, orgA, version.VersionID, "superseded by v2", "test-operator", f.claim("DeprecateDataset", version.VersionID))
	if err != nil {
		t.Fatalf("deprecate: %v", err)
	}
	if deprecated.Status != domain.VersionDeprecated {
		t.Fatalf("status = %s, want Deprecated", deprecated.Status)
	}
	if _, err := f.s.DeprecateDataset(f.ctx, orgA, version.VersionID, "again", "test-operator", f.claim("DeprecateDataset-again", version.VersionID)); !errors.Is(err, domain.ErrVersionNotDeprecatable) {
		t.Fatalf("re-deprecated an already-deprecated version: %v", err)
	}
}
