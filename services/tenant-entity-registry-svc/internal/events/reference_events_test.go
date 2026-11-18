package events_test

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"zoiko.io/tenant-entity-registry-svc/internal/domain"
	"zoiko.io/tenant-entity-registry-svc/internal/events"
)

func TestReferenceEvents(t *testing.T) {
	const tenant, setID = "0190a000-0000-7000-8000-0000000000aa", "0190a000-0000-7000-8000-0000000f0001"
	set := domain.ReferenceSet{SetID: setID, TenantID: tenant, SetCode: "ISO_3166_COUNTRY", SourceAuthority: "ISO", AllowCustom: true, CreatedBy: "p", CreatedAt: time.Now()}
	created, err := events.ReferenceSetCreatedRecord(set, "c")
	require.NoError(t, err)
	assert.Equal(t, events.EventReferenceSetCreated, created.EventType)
	assert.Contains(t, string(created.Payload), `"set_code":"ISO_3166_COUNTRY"`)
	assert.Contains(t, string(created.Payload), `"allow_custom":true`)

	rel := domain.ReferenceRelease{ReleaseID: "0190a000-0000-7000-8000-0000000f0002", SetCode: "ISO_3166_COUNTRY", ReleaseVersion: "2026.1", Status: domain.ReleasePublished,
		EffectiveFrom: "2026-02-01", Compatibility: domain.CompatRequiresMigration, SourceAuthority: "ISO", EvidenceRef: "stage-ev", StagedBy: "a", ReleasedBy: "b",
		ReleaseEvidence: "publish-ev", EntryCount: 3, Changes: map[string]int{"ADD": 2, "RETIRE": 1}}
	for _, eventType := range []string{events.EventReferenceReleaseStaged, events.EventReferenceReleasePublished} {
		rec, err := events.ReferenceReleaseRecord(eventType, tenant, setID, rel, "b", "c")
		require.NoError(t, err)
		assert.Equal(t, eventType, rec.EventType)
		p := string(rec.Payload)
		assert.Contains(t, p, `"reference_set_id":"`+setID+`"`)
		assert.Contains(t, p, `"release_version":"2026.1"`)
		assert.Contains(t, p, `"effective_from":"2026-02-01"`)
		assert.Contains(t, p, `"compatibility":"REQUIRES_MIGRATION"`, "a consumer decides adoption from this")
		assert.Contains(t, p, `"entry_count":3`)
		assert.NotContains(t, p, "United Kingdom", "the changed codes are read from the registry, not carried")
	}
	assert.Len(t, events.ReferenceEventNames, 3)
}
