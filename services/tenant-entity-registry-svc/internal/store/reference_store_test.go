package store_test

// MDM-04 reference releases against a real Postgres: releases staged then published by someone else, in effective-date order;
// codes read as of a date or under one exact release from published releases only, with validity and supersession; and what
// the database holds whatever the service does.

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"zoiko.io/tenant-entity-registry-svc/internal/domain"
	"zoiko.io/tenant-entity-registry-svc/internal/registry"
)

const (
	refSteward  = "p-ref-steward"
	refReleaser = "p-ref-releaser"
)

func (f *orgFixture) refSet(t *testing.T, code string, allowCustom bool) *domain.ReferenceSet {
	t.Helper()
	out, err := f.s.CreateReferenceSet(f.ctx, domain.CreateReferenceSetRequest{SetCode: code, Name: code, SourceAuthority: "ISO", AllowCustom: allowCustom}, refSteward)
	require.NoError(t, err)
	return out
}

func refAdd(code, label string) domain.ReferenceEntryInput {
	return domain.ReferenceEntryInput{Code: code, ChangeType: domain.ChangeAdd, Label: label}
}

func refChange(code string, ct domain.ReferenceChange, label, replacedBy string) domain.ReferenceEntryInput {
	return domain.ReferenceEntryInput{Code: code, ChangeType: ct, Label: label, ReplacedByCode: replacedBy, Reason: "why"}
}

func stageReq(version, effective string, entries ...domain.ReferenceEntryInput) domain.StageReferenceReleaseRequest {
	return domain.StageReferenceReleaseRequest{ReleaseVersion: version, SourcePublishedAt: "2024-12-01T00:00:00Z", EffectiveFrom: effective,
		Compatibility: domain.CompatAutoAdopt, EvidenceRef: "retrieval-" + version, Entries: entries}
}

func (f *orgFixture) stageRelease(t *testing.T, set string, req domain.StageReferenceReleaseRequest) *domain.ReferenceRelease {
	t.Helper()
	out, err := f.s.StageReferenceRelease(f.ctx, set, req, refSteward)
	require.NoError(t, err)
	return out
}

func (f *orgFixture) publishRelease(t *testing.T, set, version string) *domain.ReferenceRelease {
	t.Helper()
	out, err := f.s.PublishReferenceRelease(f.ctx, set, version, domain.PublishReferenceReleaseRequest{EvidenceRef: "approval-" + version}, refReleaser)
	require.NoError(t, err)
	return out
}

func (f *orgFixture) resolveRef(set, code, asOf, release string, history bool) (*domain.ReferenceResolution, error) {
	day, _ := domain.ParseDate(asOf)
	return f.s.ResolveReference(f.ctx, domain.ReferenceQuery{SetCode: set, Code: code, AsOf: day, Release: release, History: history})
}

func TestReference_SetsAreUniqueImmutableAndPerTenant(t *testing.T) {
	a := newORGFixture(t)
	b := secondTenant(t, a)
	set := a.refSet(t, "ISO_3166_COUNTRY", false)
	assert.Empty(t, set.CurrentRelease)
	_, err := a.s.CreateReferenceSet(a.ctx, domain.CreateReferenceSetRequest{SetCode: "ISO_3166_COUNTRY", Name: "dup", SourceAuthority: "x"}, refSteward)
	require.ErrorIs(t, err, registry.ErrConflict)
	assert.Equal(t, 1, a.count(t, `SELECT count(*) FROM event_outbox WHERE event_type = 'reference.set.created' AND tenant_id = $1`, a.tenantID))
	require.Error(t, a.execTenant(t, `UPDATE reference_sets SET name = 'renamed' WHERE set_code = 'ISO_3166_COUNTRY'`), "a set is immutable")
	require.Error(t, a.execTenant(t, `DELETE FROM reference_sets WHERE set_code = 'ISO_3166_COUNTRY'`))

	got, err := b.s.GetReferenceSet(b.ctx, "ISO_3166_COUNTRY")
	require.NoError(t, err)
	assert.Nil(t, got)
	sets, err := b.s.ListReferenceSets(b.ctx)
	require.NoError(t, err)
	assert.Empty(t, sets)
	_, err = b.s.StageReferenceRelease(b.ctx, "ISO_3166_COUNTRY", stageReq("1", "2025-01-01", refAdd("GB", "UK")), refSteward)
	require.ErrorIs(t, err, registry.ErrNotFound)
	_, err = b.resolveRef("ISO_3166_COUNTRY", "GB", "2025-02-01", "", false)
	require.ErrorIs(t, err, registry.ErrNotFound)
	b.refSet(t, "ISO_3166_COUNTRY", false)
	_, err = b.resolveRef("ISO_3166_COUNTRY", "GB", "2025-02-01", "", false)
	require.ErrorIs(t, err, registry.ErrNotFound, "another tenant's releases are not read")
}

func TestReference_AReleaseGovernsNothingUntilSomeoneElsePublishesIt(t *testing.T) {
	f := newORGFixture(t)
	f.refSet(t, "ISO_3166_COUNTRY", false)
	rel := f.stageRelease(t, "ISO_3166_COUNTRY", stageReq("2025.1", "2025-01-01", refAdd("GB", "United Kingdom"), refAdd("FR", "France")))
	assert.Equal(t, domain.ReleaseStaged, rel.Status)
	assert.Equal(t, 2, rel.EntryCount)
	assert.Equal(t, 2, rel.Changes["ADD"])
	assert.Equal(t, "ISO", rel.SourceAuthority, "the set's authority when the release names none")
	assert.Equal(t, 1, f.count(t, `SELECT count(*) FROM event_outbox WHERE event_type = 'reference.release.staged' AND tenant_id = $1`, f.tenantID))

	_, err := f.resolveRef("ISO_3166_COUNTRY", "GB", "2025-06-01", "", false)
	require.ErrorIs(t, err, registry.ErrNotFound, "a staged release is not read")
	_, err = f.resolveRef("ISO_3166_COUNTRY", "GB", "2025-06-01", "2025.1", false)
	require.ErrorIs(t, err, registry.ErrNotFound, "nor can it be pinned")

	_, err = f.s.PublishReferenceRelease(f.ctx, "ISO_3166_COUNTRY", "2025.1", domain.PublishReferenceReleaseRequest{EvidenceRef: "e"}, refSteward)
	require.ErrorIs(t, err, registry.ErrSelfApproval, "the stager cannot publish")
	require.Error(t, f.execTenant(t, `UPDATE reference_releases SET status = 'PUBLISHED', released_by = staged_by, released_at = now(), release_evidence = 'e', publish_seq = 1 WHERE release_version = '2025.1'`),
		"the table refuses it too")
	_, err = f.s.PublishReferenceRelease(f.ctx, "ISO_3166_COUNTRY", "9", domain.PublishReferenceReleaseRequest{EvidenceRef: "e"}, refReleaser)
	require.ErrorIs(t, err, registry.ErrNotFound)
	_, err = f.s.PublishReferenceRelease(f.ctx, "NOPE", "1", domain.PublishReferenceReleaseRequest{EvidenceRef: "e"}, refReleaser)
	require.ErrorIs(t, err, registry.ErrNotFound)

	pub := f.publishRelease(t, "ISO_3166_COUNTRY", "2025.1")
	assert.Equal(t, domain.ReleasePublished, pub.Status)
	assert.Equal(t, 1, pub.PublishSeq)
	assert.Equal(t, refReleaser, pub.ReleasedBy)
	assert.Equal(t, "approval-2025.1", pub.ReleaseEvidence)
	assert.Equal(t, 1, f.count(t, `SELECT count(*) FROM event_outbox WHERE event_type = 'reference.release.published' AND tenant_id = $1`, f.tenantID))
	_, err = f.s.PublishReferenceRelease(f.ctx, "ISO_3166_COUNTRY", "2025.1", domain.PublishReferenceReleaseRequest{EvidenceRef: "e"}, refReleaser)
	require.ErrorIs(t, err, registry.ErrStateConflict, "published once")

	res, err := f.resolveRef("ISO_3166_COUNTRY", "GB", "2025-06-01", "", false)
	require.NoError(t, err)
	assert.Equal(t, domain.CodeActive, res.Status)
	assert.Equal(t, "United Kingdom", res.Label)
	assert.Equal(t, domain.OriginOfficial, res.Origin)
	assert.Equal(t, "2025.1", res.Release.ReleaseVersion)
	assert.Equal(t, "2025-01-01", res.ValidFrom)
	assert.Equal(t, "2024-12-01", res.Release.SourcePublishedAt.Format("2006-01-02"), "publication time is kept apart from the effective date")
	assert.Equal(t, domain.CompatAutoAdopt, res.Release.Compatibility)
	assert.Equal(t, "retrieval-2025.1", res.Release.EvidenceRef)
	_, err = f.resolveRef("ISO_3166_COUNTRY", "GB", "2024-12-31", "", false)
	require.ErrorIs(t, err, registry.ErrNotFound, "before the release took effect the code did not exist")
	_, err = f.resolveRef("ISO_3166_COUNTRY", "ZZ", "2025-06-01", "", false)
	require.ErrorIs(t, err, registry.ErrNotFound, "an unknown code is never guessed")
	_, err = f.resolveRef("NOPE", "GB", "2025-06-01", "", false)
	require.ErrorIs(t, err, registry.ErrNotFound)

	sets, err := f.s.ListReferenceSets(f.ctx)
	require.NoError(t, err)
	require.Len(t, sets, 1)
	assert.Equal(t, "2025.1", sets[0].CurrentRelease)

	// A published release is final.
	require.Error(t, f.execTenant(t, `UPDATE reference_releases SET compatibility = 'PRESERVE_OLD' WHERE release_version = '2025.1'`))
	require.Error(t, f.execTenant(t, `DELETE FROM reference_releases WHERE release_version = '2025.1'`))
	require.Error(t, f.execTenant(t, `UPDATE reference_entries SET label = 'x' WHERE code = 'GB'`), "entries are immutable")
	require.Error(t, f.execTenant(t, `DELETE FROM reference_entries WHERE code = 'GB'`))
	require.Error(t, f.execTenant(t, `INSERT INTO reference_entries (entry_id, tenant_id, set_id, release_id, code, change_type, label, origin)
		SELECT gen_random_uuid(), tenant_id, set_id, release_id, 'DE', 'ADD', 'Germany', 'OFFICIAL' FROM reference_releases WHERE release_version = '2025.1'`), "nothing is added to a published release")
}

func TestReference_CodesAreReadAsOfADateWithValidityAndSupersession(t *testing.T) {
	f := newORGFixture(t)
	const set = "ISO_3166_COUNTRY"
	f.refSet(t, set, false)
	f.stageRelease(t, set, stageReq("2025.1", "2025-01-01", refAdd("GB", "Great Britain"), refAdd("YU", "Yugoslavia"), refAdd("DE", "Germany")))
	f.publishRelease(t, set, "2025.1")
	corr := refChange("GB", domain.ChangeCorrect, "United Kingdom", "")
	corr.Metadata = map[string]any{"alpha3": "GBR"}
	f.stageRelease(t, set, stageReq("2025.2", "2025-06-01", corr, refAdd("RS", "Serbia"), refChange("YU", domain.ChangeSupersede, "", "RS")))
	f.publishRelease(t, set, "2025.2")
	f.stageRelease(t, set, stageReq("2026.1", "2026-01-01", refChange("DE", domain.ChangeRetire, "", "")))
	f.publishRelease(t, set, "2026.1")

	// Before the second release the original label governs; after it, the correction, and validity still starts at the ADD.
	r, err := f.resolveRef(set, "GB", "2025-03-01", "", false)
	require.NoError(t, err)
	assert.Equal(t, "Great Britain", r.Label)
	assert.Equal(t, "2025.1", r.Release.ReleaseVersion)
	r, err = f.resolveRef(set, "GB", "2025-06-01", "", false)
	require.NoError(t, err)
	assert.Equal(t, "United Kingdom", r.Label, "a release governs from its effective date, inclusive")
	assert.Equal(t, "2025.2", r.Release.ReleaseVersion)
	assert.Equal(t, "2025-01-01", r.ValidFrom)
	assert.Equal(t, "CORRECT", r.ChangeType)
	assert.Equal(t, "GBR", r.Metadata["alpha3"])
	r, err = f.resolveRef(set, "GB", "2025-05-31", "", false)
	require.NoError(t, err)
	assert.Equal(t, "Great Britain", r.Label, "the day before it takes effect")

	// Supersession is shown from both sides, and only from its effective date.
	r, err = f.resolveRef(set, "YU", "2025-03-01", "", false)
	require.NoError(t, err)
	assert.Equal(t, domain.CodeActive, r.Status)
	assert.Empty(t, r.SupersededBy)
	r, err = f.resolveRef(set, "YU", "2025-06-01", "", false)
	require.NoError(t, err)
	assert.Equal(t, domain.CodeSuperseded, r.Status)
	assert.Equal(t, "RS", r.SupersededBy)
	assert.Equal(t, "2025-06-01", r.ValidTo)
	assert.Equal(t, "Yugoslavia", r.Label, "a superseded code is still readable, as it was")
	r, err = f.resolveRef(set, "RS", "2025-12-31", "", false)
	require.NoError(t, err)
	assert.Equal(t, domain.CodeActive, r.Status)
	assert.Equal(t, []string{"YU"}, r.Supersedes)
	assert.Equal(t, "2025-06-01", r.ValidFrom)
	r, err = f.resolveRef(set, "RS", "2025-03-01", "", false)
	assert.ErrorIs(t, err, registry.ErrNotFound, "a code is not read before it was added")
	assert.Nil(t, r)

	// A retired code is returned as retired, with the date it stopped.
	r, err = f.resolveRef(set, "DE", "2026-06-01", "", false)
	require.NoError(t, err)
	assert.Equal(t, domain.CodeRetired, r.Status)
	assert.Equal(t, "2026-01-01", r.ValidTo)
	assert.Equal(t, "Germany", r.Label)
	assert.Empty(t, r.SupersededBy)
	r, err = f.resolveRef(set, "DE", "2025-12-31", "", false)
	require.NoError(t, err)
	assert.Equal(t, domain.CodeActive, r.Status)

	// History is opt-in and lists every published change, oldest first.
	r, err = f.resolveRef(set, "YU", "2030-01-01", "", true)
	require.NoError(t, err)
	require.Len(t, r.History, 2)
	assert.Equal(t, domain.ChangeAdd, r.History[0].ChangeType)
	assert.Equal(t, domain.ChangeSupersede, r.History[1].ChangeType)
	r, err = f.resolveRef(set, "YU", "2030-01-01", "", false)
	require.NoError(t, err)
	assert.Empty(t, r.History)

	// Pinned to one exact release the date is ignored: the release is what controls the interpretation.
	r, err = f.resolveRef(set, "YU", "2030-01-01", "2025.1", false)
	require.NoError(t, err)
	assert.Equal(t, domain.CodeActive, r.Status, "under release 2025.1 Yugoslavia is not yet superseded")
	assert.Equal(t, "2025.1", r.PinnedRelease)
	r, err = f.resolveRef(set, "YU", "2020-01-01", "2025.2", false)
	require.NoError(t, err)
	assert.Equal(t, domain.CodeSuperseded, r.Status, "a date before the release does not unpin it")
	_, err = f.resolveRef(set, "RS", "2030-01-01", "2025.1", false)
	require.ErrorIs(t, err, registry.ErrNotFound, "RS did not exist under 2025.1")
	_, err = f.resolveRef(set, "GB", "2030-01-01", "no-such", false)
	require.ErrorIs(t, err, registry.ErrNotFound)

	releases, err := f.s.ListReferenceReleases(f.ctx, set)
	require.NoError(t, err)
	require.Len(t, releases, 3)
	assert.Equal(t, []string{"2025.1", "2025.2", "2026.1"}, []string{releases[0].ReleaseVersion, releases[1].ReleaseVersion, releases[2].ReleaseVersion})
	assert.Equal(t, 3, releases[1].EntryCount)
	assert.Equal(t, map[string]int{"CORRECT": 1, "ADD": 1, "SUPERSEDE": 1}, releases[1].Changes)
	_, err = f.s.ListReferenceReleases(f.ctx, "NOPE")
	require.ErrorIs(t, err, registry.ErrNotFound)
}

func TestReference_ReleasesAreCheckedAndOrderedByEffectiveDate(t *testing.T) {
	f := newORGFixture(t)
	const set = "ISO_4217_CURRENCY"
	f.refSet(t, set, false)
	f.stageRelease(t, set, stageReq("1", "2025-01-01", refAdd("GBP", "Pound sterling")))
	f.publishRelease(t, set, "1")

	_, err := f.s.StageReferenceRelease(f.ctx, set, stageReq("1", "2025-02-01", refAdd("EUR", "Euro")), refSteward)
	require.ErrorIs(t, err, registry.ErrConflict, "a version is used once")
	_, err = f.s.StageReferenceRelease(f.ctx, set, stageReq("2", "2025-01-01", refAdd("EUR", "Euro")), refSteward)
	require.ErrorIs(t, err, registry.ErrStateConflict, "a release takes effect after every published one")
	_, err = f.s.StageReferenceRelease(f.ctx, set, stageReq("2", "2024-06-01", refAdd("EUR", "Euro")), refSteward)
	require.ErrorIs(t, err, registry.ErrStateConflict, "published history is never rewritten")

	for name, entries := range map[string][]domain.ReferenceEntryInput{
		"add existing":       {refAdd("GBP", "again")},
		"modify unknown":     {refChange("XXX", domain.ChangeCorrect, "x", "")},
		"retire unknown":     {refChange("XXX", domain.ChangeRetire, "", "")},
		"supersede unknown":  {refChange("GBP", domain.ChangeSupersede, "", "EUR")},
		"custom in official": {{Code: "MY1", ChangeType: domain.ChangeAdd, Label: "mine", Origin: domain.OriginCustom}},
	} {
		_, err := f.s.StageReferenceRelease(f.ctx, set, stageReq("bad-"+name[:3], "2025-03-01", entries...), refSteward)
		require.ErrorIs(t, err, registry.ErrInvalidInput, name)
	}
	assert.Equal(t, 1, f.count(t, `SELECT count(*) FROM reference_releases`), "a refused release leaves nothing behind")
	assert.Equal(t, 1, f.count(t, `SELECT count(*) FROM reference_entries`))
	require.Error(t, f.execTenant(t, `INSERT INTO reference_entries (entry_id, tenant_id, set_id, release_id, code, change_type, label, origin)
		SELECT gen_random_uuid(), tenant_id, set_id, release_id, 'MY1', 'ADD', 'mine', 'CUSTOM' FROM reference_releases LIMIT 1`), "the table refuses custom codes in a set that takes none")

	// Two releases staged together: the second cannot be published once the first has changed what it assumed.
	f.stageRelease(t, set, stageReq("2a", "2025-03-01", refAdd("EUR", "Euro")))
	f.stageRelease(t, set, stageReq("2b", "2025-04-01", refAdd("EUR", "Euro (again)")))
	f.publishRelease(t, set, "2a")
	_, err = f.s.PublishReferenceRelease(f.ctx, set, "2b", domain.PublishReferenceReleaseRequest{EvidenceRef: "e"}, refReleaser)
	require.ErrorIs(t, err, registry.ErrStateConflict, "the set changed since 2b was staged")
	res, err := f.resolveRef(set, "EUR", "2025-05-01", "", false)
	require.NoError(t, err)
	assert.Equal(t, "Euro", res.Label)

	// Same effective date: only one can be published.
	f.stageRelease(t, set, stageReq("3a", "2025-09-01", refAdd("USD", "US dollar")))
	f.stageRelease(t, set, stageReq("3b", "2025-09-01", refAdd("JPY", "Yen")))
	f.publishRelease(t, set, "3a")
	_, err = f.s.PublishReferenceRelease(f.ctx, set, "3b", domain.PublishReferenceReleaseRequest{EvidenceRef: "e"}, refReleaser)
	require.ErrorIs(t, err, registry.ErrStateConflict)
	_, err = f.resolveRef(set, "JPY", "2025-12-01", "", false)
	require.ErrorIs(t, err, registry.ErrNotFound)

	require.Error(t, f.execTenant(t, `UPDATE reference_releases SET status = 'PUBLISHED', released_by = 'someone', released_at = now(), release_evidence = 'e', publish_seq = 9 WHERE release_version = '3b'`),
		"the table keeps releases in effective-date order whatever the service does")
	require.Error(t, f.execTenant(t, `UPDATE reference_releases SET effective_from = '2030-01-01' WHERE release_version = '3b'`), "a staged release changes only by being published")
}

func TestReference_CustomCodesAreDistinguishedAndOriginIsStable(t *testing.T) {
	f := newORGFixture(t)
	const set = "COST_REASON"
	f.refSet(t, set, true)
	my := domain.ReferenceEntryInput{Code: "X-1", ChangeType: domain.ChangeAdd, Label: "Internal", Origin: domain.OriginCustom, Metadata: map[string]any{"owner": "finance"}}
	f.stageRelease(t, set, stageReq("1", "2025-01-01", refAdd("OFF-1", "Official"), my))
	f.publishRelease(t, set, "1")

	r, err := f.resolveRef(set, "X-1", "2025-02-01", "", false)
	require.NoError(t, err)
	assert.Equal(t, domain.OriginCustom, r.Origin)
	assert.Equal(t, "finance", r.Metadata["owner"])
	r, err = f.resolveRef(set, "OFF-1", "2025-02-01", "", false)
	require.NoError(t, err)
	assert.Equal(t, domain.OriginOfficial, r.Origin)
	assert.Empty(t, r.Metadata)

	flip := domain.ReferenceEntryInput{Code: "OFF-1", ChangeType: domain.ChangeCorrect, Label: "Official", Origin: domain.OriginCustom, Reason: "why"}
	_, err = f.s.StageReferenceRelease(f.ctx, set, stageReq("2", "2025-06-01", flip), refSteward)
	require.ErrorIs(t, err, registry.ErrInvalidInput, "a code's origin does not change")

	// Retiring a custom code keeps it custom, whatever the entry says.
	f.stageRelease(t, set, stageReq("2", "2025-06-01", refChange("X-1", domain.ChangeRetire, "", "")))
	f.publishRelease(t, set, "2")
	r, err = f.resolveRef(set, "X-1", "2025-07-01", "", false)
	require.NoError(t, err)
	assert.Equal(t, domain.CodeRetired, r.Status)
	assert.Equal(t, domain.OriginCustom, r.Origin)
	assert.Equal(t, "Internal", r.Label)
	assert.Equal(t, "finance", r.Metadata["owner"], "a retired code keeps its last metadata")
}

// Migration 000026 reverses cleanly while no reference set exists and applies again; with one it refuses.
func TestReference_MigrationDownAndUp(t *testing.T) {
	f := newORGFixture(t)
	ctx := context.Background()
	_, thisFile, _, _ := runtime.Caller(0)
	dir := filepath.Join(filepath.Dir(thisFile), "../../deployments/migrations")
	read := func(name string) string {
		b, err := os.ReadFile(filepath.Join(dir, name))
		require.NoError(t, err)
		return string(b)
	}
	exists := func(q string) bool {
		var n int
		require.NoError(t, f.pool.QueryRow(ctx, q).Scan(&n))
		return n > 0
	}
	const table = `SELECT count(*) FROM pg_tables WHERE tablename = 'reference_releases'`

	f.refSet(t, "ISO_3166_COUNTRY", false)
	_, err := f.pool.Exec(ctx, read("000026_reference_sets.down.sql"))
	require.Error(t, err, "down is refused while reference sets exist")
	assert.Contains(t, err.Error(), "000026 down refused")

	f2 := newORGFixture(t)
	_, err = f2.pool.Exec(ctx, read("000026_reference_sets.down.sql"))
	require.NoError(t, err)
	assert.False(t, exists(table))
	_, err = f2.pool.Exec(ctx, read("000026_reference_sets.up.sql"))
	require.NoError(t, err, "up applies again after down")
	assert.True(t, exists(table))
}
