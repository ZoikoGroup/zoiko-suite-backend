package store_test

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"zoiko.io/tenant-entity-registry-svc/internal/domain"
	"zoiko.io/tenant-entity-registry-svc/internal/registry"
)

func TestUnit_ConversionIsExactAndRoundedOnlyAtTheEnd(t *testing.T) {
	f := newORGFixture(t)
	f.quantityDim(t, "LENGTH", "M", 6)
	f.standardUnit(t, "LENGTH", "KM", "1000", 6)
	mi := f.standardUnit(t, "LENGTH", "MI", "1.609344", 4)
	f.standardUnit(t, "LENGTH", "THIRD", "1/3", 2)
	assert.Equal(t, domain.UnitActive, mi.Status, "a STANDARD conversion takes effect at once and activates the unit")
	today := time.Now().UTC()

	// 1 mile is 1.609344 metres here, so 0.1 mile is exactly 0.1609344 m, which no float holds.
	res, err := f.convert(t, "0.1", "MI", "M", today)
	require.NoError(t, err)
	assert.Zero(t, ratOf(t, res.Exact).Cmp(ratOf(t, "0.1609344")), "exact: %s", res.Exact)
	assert.Equal(t, "0.160934", res.Value, "rounded to the target unit decimals (metre keeps 6)")
	assert.Equal(t, "M", res.ToUnit)
	assert.Equal(t, 6, res.Decimals)
	assert.Equal(t, "LENGTH", res.Dimension)
	assert.Equal(t, domain.UnitRounding, res.Rounding)

	res, err = f.convert(t, "0.1", "MI", "KM", today)
	require.NoError(t, err)
	assert.Zero(t, ratOf(t, res.Exact).Cmp(ratOf(t, "0.0001609344")))
	assert.Equal(t, "0.000161", res.Value, "KM keeps 6 decimals; the exact fraction is returned beside the rounded figure")

	// A third is held as a fraction: three of them are exactly one metre.
	res, err = f.convert(t, "3", "THIRD", "M", today)
	require.NoError(t, err)
	assert.Equal(t, "1", res.Exact)
	assert.Equal(t, "1.000000", res.Value)
	res, err = f.convert(t, "1", "M", "THIRD", today)
	require.NoError(t, err)
	assert.Equal(t, "3", res.Exact)
	res, err = f.convert(t, "1", "THIRD", "KM", today)
	require.NoError(t, err)
	assert.Equal(t, "1/3000", res.Exact, "never a recurring decimal in the exact figure")
	assert.Equal(t, "0.000333", res.Value)

	// The base converts to itself; a negative quantity is an adjustment and rounds away from zero.
	res, err = f.convert(t, "-2.5", "M", "M", today)
	require.NoError(t, err)
	assert.Equal(t, "-2.500000", res.Value)
	assert.True(t, res.From.IsBase)
	assert.Equal(t, "1", res.From.Factor)
	assert.Zero(t, res.From.VersionNo)
	res, err = f.convert(t, "-0.5", "THIRD", "THIRD", today)
	require.NoError(t, err)
	assert.Equal(t, "-0.50", res.Value)

	// The result names the version applied to each unit so the calling domain can retain it.
	res, err = f.convert(t, "1", "MI", "KM", today)
	require.NoError(t, err)
	assert.Equal(t, 1, res.From.VersionNo)
	assert.Equal(t, "MI", res.From.UnitCode)
	assert.Equal(t, 1, res.To.VersionNo)
	assert.Equal(t, "ISO 80000", res.From.SourceRef)
	assert.Equal(t, "1.609344", res.From.FactorDecimal)
	assert.Equal(t, "2025-01-01", res.From.ValidFrom)
	assert.Equal(t, 0, f.count(t, `SELECT count(*) FROM conversion_versions WHERE factor_num <> trunc(factor_num) OR factor_den <> trunc(factor_den)`))

	_, err = f.convert(t, "x", "KM", "M", today)
	require.ErrorIs(t, err, registry.ErrInvalidInput)
	_, err = f.convert(t, "1", "NOPE", "M", today)
	require.ErrorIs(t, err, registry.ErrNotFound)
	_, err = f.convert(t, "1", "KM", "NOPE", today)
	require.ErrorIs(t, err, registry.ErrNotFound)
}

func TestUnit_DimensionalityIsCheckedBeforeAnyArithmetic(t *testing.T) {
	f := newORGFixture(t)
	f.quantityDim(t, "LENGTH", "M", 6)
	f.quantityDim(t, "MASS", "KG", 6)
	f.standardUnit(t, "LENGTH", "KM", "1000", 6)
	f.standardUnit(t, "MASS", "T", "1000", 6)
	draft := f.draftUnit(t, "MASS", "LB", 4)
	require.NotNil(t, draft)

	_, err := f.convert(t, "1", "KM", "T", time.Now().UTC())
	require.ErrorIs(t, err, registry.ErrDimensionMismatch)
	assert.Contains(t, err.Error(), "LENGTH")
	assert.Contains(t, err.Error(), "MASS")
	// The mismatch wins even when a unit could not be converted anyway: dimensionality is first.
	_, err = f.convert(t, "1", "KM", "LB", time.Now().UTC())
	require.ErrorIs(t, err, registry.ErrDimensionMismatch)
	_, err = f.convert(t, "1", "M", "KG", time.Now().UTC())
	require.ErrorIs(t, err, registry.ErrDimensionMismatch, "the base units of two dimensions never convert")
	// Within a dimension a unit with no conversion is a different, explicit refusal.
	_, err = f.convert(t, "1", "LB", "T", time.Now().UTC())
	require.ErrorIs(t, err, registry.ErrConversionMissing)

	// Nothing in the schema can relate two dimensions: a conversion is to the unit's own base only.
	require.Error(t, f.execTenant(t, `INSERT INTO conversion_versions (conversion_id, tenant_id, unit_id, version_no, factor_num, factor_den, source_class, source_ref, status, valid_from, evidence_ref, proposed_by, approved_by, approved_at)
		SELECT gen_random_uuid(), tenant_id, unit_id, 1, 1, 1, 'STANDARD', 's', 'ACTIVE', '2025-01-01', 'e', 'x', 'x', now() FROM units WHERE unit_code = 'M'`), "a base unit takes no conversion")
}

func TestUnit_ConversionVersionsArePinnedAndReproducible(t *testing.T) {
	f := newORGFixture(t)
	f.quantityDim(t, "VOLUME", "L", 6)
	gal := f.draftUnit(t, "VOLUME", "GAL", 4)
	today := time.Now().UTC()
	from := dayFromNow(10)
	later := today.AddDate(0, 0, 10)

	first, err := f.s.ProposeConversion(f.ctx, "GAL", false, conversionReq("3.785", domain.ConvStandard, "2025-01-01", gal.Unit.RecordVersion), unitSteward, false)
	require.NoError(t, err)
	assert.Equal(t, 1, first.Conversion.VersionNo)
	assert.Equal(t, domain.ConvActive, first.Conversion.Status)
	assert.Equal(t, 1, f.count(t, `SELECT count(*) FROM event_outbox WHERE event_type = 'unit.conversion.activated' AND tenant_id = $1`, f.tenantID))
	v := first.Unit.RecordVersion

	_, err = f.s.ProposeConversion(f.ctx, "GAL", false, conversionReq("3.8", domain.ConvStandard, "", v), unitSteward, false)
	require.ErrorIs(t, err, registry.ErrStateConflict, "an active conversion is superseded, not proposed again")
	_, err = f.s.ProposeConversion(f.ctx, "GAL", true, conversionReq("3.785", domain.ConvStandard, from, v), unitSteward, false)
	require.ErrorIs(t, err, registry.ErrInvalidInput, "the same factor and class is no change")
	_, err = f.s.ProposeConversion(f.ctx, "GAL", true, conversionReq("3.8", domain.ConvStandard, dayFromNow(-1), v), unitSteward, false)
	require.ErrorIs(t, err, registry.ErrInvalidInput, "a change never starts in the past")
	_, err = f.s.ProposeConversion(f.ctx, "GAL", true, conversionReq("3.8", domain.ConvStandard, "", 0), unitSteward, false)
	require.ErrorIs(t, err, registry.ErrVersionRequired)
	_, err = f.s.ProposeConversion(f.ctx, "GAL", true, conversionReq("3.8", domain.ConvStandard, "", v+9), unitSteward, false)
	require.ErrorIs(t, err, registry.ErrVersionConflict)
	_, err = f.s.ProposeConversion(f.ctx, "NOPE", true, conversionReq("3.8", domain.ConvStandard, "", 1), unitSteward, false)
	require.ErrorIs(t, err, registry.ErrNotFound)
	_, err = f.s.ProposeConversion(f.ctx, "L", false, conversionReq("2", domain.ConvStandard, "", 1), unitSteward, false)
	require.ErrorIs(t, err, registry.ErrInvalidInput, "the base unit converts to itself")

	sup, err := f.s.ProposeConversion(f.ctx, "GAL", true, conversionReq("3.8", domain.ConvStandard, from, v), unitSteward, false)
	require.NoError(t, err)
	assert.Equal(t, 2, sup.Conversion.VersionNo)
	assert.Equal(t, 1, sup.Conversion.SupersedesVersion)
	require.NotNil(t, sup.Previous)
	assert.Equal(t, domain.ConvSuperseded, sup.Previous.Status)
	assert.Equal(t, from, sup.Previous.ValidTo, "the old version ends where the new one begins")
	assert.Equal(t, 1, f.count(t, `SELECT count(*) FROM event_outbox WHERE event_type = 'unit.conversion.superseded' AND tenant_id = $1`, f.tenantID))

	// Before the change a result applies version 1; on and after it, version 2. Both stay readable.
	res, err := f.convert(t, "10", "GAL", "L", today)
	require.NoError(t, err)
	assert.Equal(t, 1, res.From.VersionNo)
	assert.Equal(t, "37.850000", res.Value)
	res, err = f.convert(t, "10", "GAL", "L", later)
	require.NoError(t, err)
	assert.Equal(t, 2, res.From.VersionNo)
	assert.Equal(t, "38.000000", res.Value)
	res, err = f.convert(t, "10", "GAL", "L", later.AddDate(0, 0, -1))
	require.NoError(t, err)
	assert.Equal(t, 1, res.From.VersionNo, "the day before the change still applies the old version")
	_, err = f.convert(t, "10", "GAL", "L", time.Date(2024, 12, 31, 0, 0, 0, 0, time.UTC))
	require.ErrorIs(t, err, registry.ErrConversionMissing, "before any conversion took effect there is no answer, never a guess")

	v1, err := f.s.GetConversionVersion(f.ctx, "GAL", 1)
	require.NoError(t, err)
	require.NotNil(t, v1)
	assert.Equal(t, "757/200", v1.Factor, "3.785 is held as a reduced fraction")
	missing, err := f.s.GetConversionVersion(f.ctx, "GAL", 9)
	require.NoError(t, err)
	assert.Nil(t, missing)
	none, err := f.s.GetConversionVersion(f.ctx, "NOPE", 1)
	require.NoError(t, err)
	assert.Nil(t, none)

	view, err := f.s.GetUnit(f.ctx, "GAL", later, true)
	require.NoError(t, err)
	require.NotNil(t, view)
	require.NotNil(t, view.Conversion)
	assert.Equal(t, 2, view.Conversion.VersionNo)
	assert.True(t, view.Usable)
	assert.Len(t, view.Versions, 2)
	view, err = f.s.GetUnit(f.ctx, "GAL", time.Date(2024, 6, 1, 0, 0, 0, 0, time.UTC), false)
	require.NoError(t, err)
	assert.False(t, view.Usable)
	assert.Contains(t, view.Reason, "no approved conversion")
	assert.Nil(t, view.Conversion)
	absent, err := f.s.GetUnit(f.ctx, "NOPE", today, false)
	require.NoError(t, err)
	assert.Nil(t, absent)

	// The database holds what the service promises.
	require.Error(t, f.execTenant(t, `UPDATE conversion_versions SET factor_num = 7 WHERE version_no = 1`), "a version is immutable; supersede it")
	require.Error(t, f.execTenant(t, `UPDATE conversion_versions SET valid_to = NULL WHERE version_no = 1`), "a superseded version never reopens")
	require.Error(t, f.execTenant(t, `UPDATE conversion_versions SET status = 'ACTIVE' WHERE version_no = 1`))
	require.Error(t, f.execTenant(t, `DELETE FROM conversion_versions WHERE version_no = 1`))
	require.Error(t, f.execTenant(t, `UPDATE conversion_versions SET status = 'PENDING', approved_by = NULL, approved_at = NULL WHERE version_no = 2`), "an approved conversion is never reopened")
	require.Error(t, f.execTenant(t, `UPDATE conversion_versions SET factor_den = 0 WHERE version_no = 2`))
	assert.Equal(t, 2, f.count(t, `SELECT count(*) FROM event_outbox WHERE event_type = 'unit.conversion.activated' AND tenant_id = $1`, f.tenantID), "both versions announced")
}

func TestUnit_ANonStandardConversionNeedsAnIndependentApprover(t *testing.T) {
	f := newORGFixture(t)
	f.quantityDim(t, "COUNT", "EA", 0)
	box := f.draftUnit(t, "COUNT", "BOX", 0)

	_, err := f.s.ProposeConversion(f.ctx, "BOX", false, conversionReq("12", domain.ConvOverride, "", box.Unit.RecordVersion), unitSteward, false)
	require.NoError(t, err)
	pend, err := f.s.GetUnit(f.ctx, "BOX", time.Now().UTC(), true)
	require.NoError(t, err)
	require.NotNil(t, pend.Pending)
	assert.Equal(t, domain.ConvPending, pend.Pending.Status)
	assert.False(t, pend.Usable, "nothing converts until it is approved")
	assert.Equal(t, domain.UnitDraft, pend.Unit.Status)
	assert.Equal(t, 1, f.count(t, `SELECT count(*) FROM event_outbox WHERE event_type = 'unit.conversion.proposed' AND tenant_id = $1`, f.tenantID))
	assert.Equal(t, 0, f.count(t, `SELECT count(*) FROM event_outbox WHERE event_type = 'unit.conversion.activated' AND tenant_id = $1`, f.tenantID))
	_, err = f.convert(t, "2", "BOX", "EA", time.Now().UTC())
	require.ErrorIs(t, err, registry.ErrConversionMissing, "a pending conversion is not in force")
	v := pend.Unit.RecordVersion

	_, err = f.s.ProposeConversion(f.ctx, "BOX", false, conversionReq("10", domain.ConvOrganisation, "", v), unitReviewer, false)
	require.ErrorIs(t, err, registry.ErrStateConflict, "one conversion awaits approval at a time")

	_, err = f.s.ApproveConversion(f.ctx, "BOX", 1, domain.ApproveConversionRequest{ExpectedVersion: v, EvidenceRef: "e"}, unitSteward, false)
	require.ErrorIs(t, err, registry.ErrSelfApproval, "the proposer cannot approve")
	_, err = f.s.ApproveConversion(f.ctx, "BOX", 1, domain.ApproveConversionRequest{EvidenceRef: "e"}, unitReviewer, false)
	require.ErrorIs(t, err, registry.ErrVersionRequired)
	_, err = f.s.ApproveConversion(f.ctx, "BOX", 1, domain.ApproveConversionRequest{ExpectedVersion: v + 5, EvidenceRef: "e"}, unitReviewer, false)
	require.ErrorIs(t, err, registry.ErrVersionConflict)
	_, err = f.s.ApproveConversion(f.ctx, "BOX", 7, domain.ApproveConversionRequest{ExpectedVersion: v, EvidenceRef: "e"}, unitReviewer, false)
	require.ErrorIs(t, err, registry.ErrNotFound)
	_, err = f.s.ApproveConversion(f.ctx, "NOPE", 1, domain.ApproveConversionRequest{ExpectedVersion: v, EvidenceRef: "e"}, unitReviewer, false)
	require.ErrorIs(t, err, registry.ErrNotFound)

	// The table refuses it too, whoever calls it.
	require.Error(t, f.execTenant(t, `UPDATE conversion_versions SET status = 'ACTIVE', approved_by = proposed_by, approved_at = now() WHERE unit_id IN (SELECT unit_id FROM units WHERE unit_code = 'BOX')`),
		"a non-standard conversion is not self-approved")

	ok, err := f.s.ApproveConversion(f.ctx, "BOX", 1, domain.ApproveConversionRequest{ExpectedVersion: v, EvidenceRef: "e"}, unitReviewer, false)
	require.NoError(t, err)
	assert.Equal(t, domain.ConvActive, ok.Conversion.Status)
	assert.Equal(t, unitReviewer, ok.Conversion.ApprovedBy)
	assert.Equal(t, domain.UnitActive, ok.Unit.Status)
	assert.Equal(t, 1, f.count(t, `SELECT count(*) FROM event_outbox WHERE event_type = 'unit.conversion.activated' AND tenant_id = $1`, f.tenantID))
	_, err = f.s.ApproveConversion(f.ctx, "BOX", 1, domain.ApproveConversionRequest{ExpectedVersion: ok.Unit.RecordVersion, EvidenceRef: "e"}, unitReviewer, false)
	require.ErrorIs(t, err, registry.ErrStateConflict, "approved once")

	res, err := f.convert(t, "2", "BOX", "EA", time.Now().UTC())
	require.NoError(t, err)
	assert.Equal(t, "24", res.Value, "a unit with 0 decimals keeps whole numbers")
	assert.Equal(t, domain.ConvOverride, res.From.SourceClass)

	// Replacing a non-standard conversion is independently approved as well, and the old one stays in force until then.
	v = ok.Unit.RecordVersion
	from := dayFromNow(5)
	sup, err := f.s.ProposeConversion(f.ctx, "BOX", true, conversionReq("10", domain.ConvOrganisation, from, v), unitSteward, false)
	require.NoError(t, err)
	assert.Equal(t, domain.ConvPending, sup.Conversion.Status)
	assert.Equal(t, 1, sup.Conversion.SupersedesVersion)
	res, err = f.convert(t, "2", "BOX", "EA", time.Now().UTC().AddDate(0, 0, 6))
	require.NoError(t, err)
	assert.Equal(t, 1, res.From.VersionNo, "the active version keeps applying until the replacement is approved")
	_, err = f.s.ApproveConversion(f.ctx, "BOX", 2, domain.ApproveConversionRequest{ExpectedVersion: sup.Unit.RecordVersion, EvidenceRef: "e"}, unitSteward, false)
	require.ErrorIs(t, err, registry.ErrSelfApproval)
	done, err := f.s.ApproveConversion(f.ctx, "BOX", 2, domain.ApproveConversionRequest{ExpectedVersion: sup.Unit.RecordVersion, EvidenceRef: "e"}, unitReviewer, false)
	require.NoError(t, err)
	require.NotNil(t, done.Previous)
	assert.Equal(t, domain.ConvSuperseded, done.Previous.Status)
	assert.Equal(t, from, done.Previous.ValidTo)
	assert.Equal(t, 1, f.count(t, `SELECT count(*) FROM event_outbox WHERE event_type = 'unit.conversion.superseded' AND tenant_id = $1`, f.tenantID))
	res, err = f.convert(t, "2", "BOX", "EA", time.Now().UTC().AddDate(0, 0, 6))
	require.NoError(t, err)
	assert.Equal(t, 2, res.From.VersionNo)
	assert.Equal(t, "20", res.Value)

	_, err = f.s.ProposeConversion(f.ctx, "BOX", true, conversionReq("9", domain.ConvStandard, dayFromNow(9), 0), unitSteward, true)
	require.NoError(t, err, "a standard replacement takes effect directly")
}

func TestUnit_RetirementRejectsNewUseAndKeepsHistory(t *testing.T) {
	f := newORGFixture(t)
	f.quantityDim(t, "LENGTH", "M", 6)
	km := f.standardUnit(t, "LENGTH", "KM", "1000", 6)
	now := time.Now().UTC()

	_, err := f.s.RetireUnit(f.ctx, "M", domain.RetireUnitRequest{ExpectedVersion: 1, Reason: "r", EvidenceRef: "e"}, unitSteward, false)
	require.ErrorIs(t, err, registry.ErrStateConflict, "the base cannot go while a unit still converts through it")
	_, err = f.s.RetireUnit(f.ctx, "KM", domain.RetireUnitRequest{Reason: "r", EvidenceRef: "e"}, unitSteward, false)
	require.ErrorIs(t, err, registry.ErrVersionRequired)
	_, err = f.s.RetireUnit(f.ctx, "KM", domain.RetireUnitRequest{ExpectedVersion: km.RecordVersion + 3, Reason: "r", EvidenceRef: "e"}, unitSteward, false)
	require.ErrorIs(t, err, registry.ErrVersionConflict)
	_, err = f.s.RetireUnit(f.ctx, "NOPE", domain.RetireUnitRequest{ExpectedVersion: 1, Reason: "r", EvidenceRef: "e"}, unitSteward, false)
	require.ErrorIs(t, err, registry.ErrNotFound)

	out, err := f.s.RetireUnit(f.ctx, "KM", domain.RetireUnitRequest{ExpectedVersion: km.RecordVersion, Reason: "no longer used", EvidenceRef: "memo"}, unitSteward, false)
	require.NoError(t, err)
	assert.Equal(t, domain.UnitRetired, out.Unit.Status)
	assert.Equal(t, now.Format("2006-01-02"), out.Unit.RetiredOn)
	assert.Equal(t, unitSteward, out.Unit.RetiredBy)
	assert.Equal(t, 1, f.count(t, `SELECT count(*) FROM event_outbox WHERE event_type = 'unit.retired' AND tenant_id = $1`, f.tenantID))

	_, err = f.s.RetireUnit(f.ctx, "KM", domain.RetireUnitRequest{ExpectedVersion: out.Unit.RecordVersion, Reason: "again", EvidenceRef: "e"}, unitSteward, false)
	require.ErrorIs(t, err, registry.ErrStateConflict)
	_, err = f.s.ProposeConversion(f.ctx, "KM", true, conversionReq("999", domain.ConvStandard, dayFromNow(3), out.Unit.RecordVersion), unitSteward, false)
	require.ErrorIs(t, err, registry.ErrStateConflict, "a retired unit takes no new conversion")

	_, err = f.convert(t, "1", "KM", "M", now)
	require.ErrorIs(t, err, registry.ErrReferenceInvalid, "new use is rejected")
	_, err = f.convert(t, "1", "M", "KM", now)
	require.ErrorIs(t, err, registry.ErrReferenceInvalid, "either side")
	res, err := f.convert(t, "1", "KM", "M", now.AddDate(0, 0, -1))
	require.NoError(t, err, "history is kept: a past date still converts")
	assert.Equal(t, "1000.000000", res.Value)
	view, err := f.s.GetUnit(f.ctx, "KM", now, false)
	require.NoError(t, err)
	assert.False(t, view.Usable)
	assert.Contains(t, view.Reason, "retired")

	require.Error(t, f.execTenant(t, `UPDATE units SET status = 'ACTIVE', retired_on = NULL, retired_by = NULL, retired_at = NULL, retire_reason = NULL WHERE unit_code = 'KM'`), "a retired unit never returns")
	require.Error(t, f.execTenant(t, `UPDATE units SET status = 'RETIRED' WHERE unit_code = 'M'`), "retirement needs its date, actor and reason")

	// With nothing converting through it, the base can retire too.
	baseView, err := f.s.GetUnit(f.ctx, "M", now, false)
	require.NoError(t, err)
	_, err = f.s.RetireUnit(f.ctx, "M", domain.RetireUnitRequest{ExpectedVersion: baseView.Unit.RecordVersion, Reason: "dimension closed", EvidenceRef: "e"}, unitSteward, false)
	require.NoError(t, err)
}

func TestUnit_TenantsNeverSeeEachOther(t *testing.T) {
	a := newORGFixture(t)
	b := secondTenant(t, a)
	a.quantityDim(t, "LENGTH", "M", 6)
	a.standardUnit(t, "LENGTH", "KM", "1000", 6)

	view, err := b.s.GetUnit(b.ctx, "KM", time.Now().UTC(), false)
	require.NoError(t, err)
	assert.Nil(t, view)
	_, err = b.convert(t, "1", "KM", "M", time.Now().UTC())
	require.ErrorIs(t, err, registry.ErrNotFound)
	_, err = b.s.RetireUnit(b.ctx, "KM", domain.RetireUnitRequest{ExpectedVersion: 1, Reason: "r", EvidenceRef: "e"}, unitSteward, false)
	require.ErrorIs(t, err, registry.ErrNotFound)
	_, err = b.s.CreateUnit(b.ctx, domain.CreateUnitRequest{DimensionCode: "LENGTH", UnitFields: domain.UnitFields{UnitCode: "X", Name: "x", Symbol: "x"}}, unitSteward)
	require.ErrorIs(t, err, registry.ErrInvalidInput, "another tenant's dimension is no dimension here")
	dims, err := b.s.ListQuantityDimensions(b.ctx)
	require.NoError(t, err)
	assert.Empty(t, dims)
	b.quantityDim(t, "LENGTH", "M", 6)
	_, err = b.s.GetConversionVersion(b.ctx, "KM", 1)
	require.NoError(t, err, "the same code in this tenant is a different unit")
	got, _ := b.s.GetConversionVersion(b.ctx, "KM", 1)
	assert.Nil(t, got)
}

// Migration 000025 reverses cleanly while no dimension exists and applies again; with one it refuses.
func TestUnit_MigrationDownAndUp(t *testing.T) {
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
	const table = `SELECT count(*) FROM pg_tables WHERE tablename = 'conversion_versions'`

	f.quantityDim(t, "LENGTH", "M", 6)
	_, err := f.pool.Exec(ctx, read("000025_units_of_measure.down.sql"))
	require.Error(t, err, "down is refused while quantity dimensions exist")
	assert.Contains(t, err.Error(), "000025 down refused")

	f2 := newORGFixture(t)
	_, err = f2.pool.Exec(ctx, read("000025_units_of_measure.down.sql"))
	require.NoError(t, err)
	assert.False(t, exists(table))
	_, err = f2.pool.Exec(ctx, read("000025_units_of_measure.up.sql"))
	require.NoError(t, err, "up applies again after down")
	assert.True(t, exists(table))
}
