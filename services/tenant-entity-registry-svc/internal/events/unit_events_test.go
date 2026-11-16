package events_test

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"zoiko.io/tenant-entity-registry-svc/internal/domain"
	"zoiko.io/tenant-entity-registry-svc/internal/events"
)

func TestUnitEvents(t *testing.T) {
	u := domain.Unit{UnitID: "0190a000-0000-7000-8000-0000000e0001", TenantID: "0190a000-0000-7000-8000-0000000000aa", DimensionCode: "LENGTH", UnitCode: "MI",
		Name: "Mile", Symbol: "mi", Status: domain.UnitActive, Decimals: 4, RecordVersion: 3, CreatedBy: "p", CreatedAt: time.Now(),
		RetiredOn: "2026-09-01", RetireReason: "superseded by KM", RetiredBy: "r"}
	c1 := domain.ConversionVersion{VersionNo: 1, Factor: "12573/7812", SourceClass: domain.ConvStandard, SourceRef: "ISO 80000", Status: domain.ConvSuperseded,
		ValidFrom: "2026-01-01", ValidTo: "2026-07-01", EvidenceRef: "e1", ProposedBy: "p"}
	c2 := domain.ConversionVersion{VersionNo: 2, Factor: "1609344/1000000", SourceClass: domain.ConvOrganisation, SourceRef: "policy 7", Status: domain.ConvActive,
		ValidFrom: "2026-07-01", EvidenceRef: "e2", ProposedBy: "a", ApprovedBy: "b"}

	dim, err := events.QuantityDimensionCreatedRecord(domain.QuantityDimension{DimensionID: "d1", TenantID: u.TenantID, DimensionCode: "LENGTH", BaseUnitCode: "M", CreatedBy: "p"}, "c")
	require.NoError(t, err)
	assert.Equal(t, events.EventQuantityDimensionCreated, dim.EventType)
	assert.Contains(t, string(dim.Payload), `"base_unit_code":"M"`)

	created, err := events.UnitCreatedRecord(u, "c")
	require.NoError(t, err)
	assert.Equal(t, events.EventUnitCreated, created.EventType)
	assert.Contains(t, string(created.Payload), `"unit_code":"MI"`)
	assert.Contains(t, string(created.Payload), `"dimension_code":"LENGTH"`)

	for _, eventType := range []string{events.EventConversionProposed, events.EventConversionActivated, events.EventConversionSuperseded} {
		rec, err := events.ConversionRecord(eventType, u, c2, &c1, "b", "c")
		require.NoError(t, err)
		assert.Equal(t, eventType, rec.EventType)
		p := string(rec.Payload)
		assert.Contains(t, p, `"factor":"1609344/1000000"`, "the exact factor travels, not a float")
		assert.Contains(t, p, `"previous_factor":"12573/7812"`)
		assert.Contains(t, p, `"previous_version_no":1`)
		assert.Contains(t, p, `"valid_from":"2026-07-01"`)
		assert.Contains(t, p, `"approved_by":"b"`)
	}

	retired, err := events.UnitRetiredRecord(u, "ev-9", "c")
	require.NoError(t, err)
	assert.Equal(t, events.EventUnitRetired, retired.EventType)
	assert.Contains(t, string(retired.Payload), `"retired_on":"2026-09-01"`)

	assert.Len(t, events.UnitEventNames, 6)
}
