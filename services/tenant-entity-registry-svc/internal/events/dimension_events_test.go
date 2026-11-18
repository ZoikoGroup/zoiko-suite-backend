package events_test

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"zoiko.io/tenant-entity-registry-svc/internal/domain"
	"zoiko.io/tenant-entity-registry-svc/internal/events"
)

func TestDimensionEvents(t *testing.T) {
	v := domain.DimensionValue{ValueID: "0190a000-0000-7000-8000-0000000d0001", TenantID: "0190a000-0000-7000-8000-0000000000aa",
		TypeID: "0190a000-0000-7000-8000-0000000d00aa", TypeCode: "COST_CENTER", LegalEntityID: "0190a000-0000-7000-8000-00000000dddd", Code: "CC100",
		Status: domain.DimActive, CurrentVersion: 2, RecordVersion: 3, CreatedBy: "p", CreatedAt: time.Now()}
	h1 := domain.HierarchyVersion{VersionNo: 1, ParentValueID: "p0", Label: "Sales", ValidFrom: "2026-01-01", EvidenceRef: "e1", ProposedBy: "p"}
	h2 := domain.HierarchyVersion{VersionNo: 2, ParentValueID: "p1", Label: "Sales EMEA", Status: domain.HierarchyApproved, ValidFrom: "2026-07-01", EvidenceRef: "e2",
		ProposedBy: "a", ApprovedBy: "b", Reason: "reorganisation"}

	typ, err := events.DimensionTypeCreatedRecord(domain.DimensionType{TypeID: v.TypeID, TenantID: v.TenantID, TypeCode: "COST_CENTER", Hierarchical: true, Controlled: true, CreatedBy: "p"}, "c")
	require.NoError(t, err)
	assert.Equal(t, events.EventDimensionTypeCreated, typ.EventType)
	assert.Contains(t, string(typ.Payload), `"controlled":true`)

	created, err := events.DimensionValueCreatedRecord(v, h1, "c")
	require.NoError(t, err)
	assert.Equal(t, events.EventDimensionValueCreated, created.EventType)
	assert.Contains(t, string(created.Payload), `"code":"CC100"`)
	assert.Contains(t, string(created.Payload), `"effective_date":"2026-01-01"`)

	for to, want := range map[string]string{domain.DimActive: events.EventDimensionValueActivated, domain.DimInactive: events.EventDimensionValueDeactivated} {
		rec, err := events.DimensionStatusRecord(v, domain.DimensionTransition{From: domain.DimDraft, To: to, EffectiveDate: "2026-03-01", Command: "X", EvidenceRef: "e", Actor: "p3", ExceptionUntil: "2026-03-20"}, "c")
		require.NoError(t, err)
		assert.Equal(t, want, rec.EventType)
		assert.Contains(t, string(rec.Payload), `"posting_exception_until":"2026-03-20"`, "a controlled exception is stated for the posting service")
	}

	for _, eventType := range []string{events.EventDimensionHierarchyProposed, events.EventDimensionHierarchyChanged} {
		rec, err := events.DimensionHierarchyRecord(eventType, v, h2, &h1, "b", "c")
		require.NoError(t, err)
		assert.Equal(t, eventType, rec.EventType)
		p := string(rec.Payload)
		assert.Contains(t, p, `"previous_parent_value_id":"p0"`)
		assert.Contains(t, p, `"previous_label":"Sales"`)
		assert.Contains(t, p, `"parent_value_id":"p1"`)
		assert.Contains(t, p, `"effective_date":"2026-07-01"`)
		assert.Contains(t, p, `"approved_by":"b"`)
	}

	m := domain.SourceMapping{MappingID: "m1", ValueID: v.ValueID, SourceDomain: "PROJECT", SourceObjectID: "P-1", VersionNo: 2, ValidFrom: "2026-02-01", EvidenceRef: "e"}
	prev := domain.SourceMapping{ValueID: "old"}
	mapped, err := events.DimensionSourceMappedRecord(v, m, &prev, "p", "c")
	require.NoError(t, err)
	assert.Equal(t, events.EventDimensionSourceMapped, mapped.EventType)
	assert.Contains(t, string(mapped.Payload), `"previous_value_id":"old"`)
	assert.Contains(t, string(mapped.Payload), `"mapping_version":2`)
	assert.Len(t, events.DimensionEventNames, 7)
}
