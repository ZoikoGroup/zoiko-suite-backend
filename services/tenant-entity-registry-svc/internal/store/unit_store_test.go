package store_test

// REF-09 units of measure against a real Postgres: one base unit per dimension, exact rational conversions, dimensionality
// checked before arithmetic, versioned effective-dated conversions that stay reproducible, independent approval of
// non-standard conversions, retirement rules, and what the database holds whatever the service does.

import (
	"math/big"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"zoiko.io/tenant-entity-registry-svc/internal/domain"
	"zoiko.io/tenant-entity-registry-svc/internal/registry"
)

const (
	unitSteward  = "p-unit-steward"
	unitReviewer = "p-unit-reviewer"
)

func (f *orgFixture) quantityDim(t *testing.T, code, baseCode string, decimals int) *registry.DimensionWithBase {
	t.Helper()
	out, err := f.s.CreateQuantityDimension(f.ctx, domain.CreateQuantityDimensionRequest{DimensionCode: code, Name: code,
		BaseUnit: domain.UnitFields{UnitCode: baseCode, Name: baseCode, Symbol: baseCode, Decimals: &decimals}}, unitSteward)
	require.NoError(t, err)
	return out
}

func (f *orgFixture) draftUnit(t *testing.T, dim, code string, decimals int) *registry.UnitOutcome {
	t.Helper()
	out, err := f.s.CreateUnit(f.ctx, domain.CreateUnitRequest{DimensionCode: dim, UnitFields: domain.UnitFields{UnitCode: code, Name: code, Symbol: code, Decimals: &decimals}}, unitSteward)
	require.NoError(t, err)
	return out
}

func conversionReq(factor, class, from string, version int64) domain.ProposeConversionRequest {
	r := domain.ProposeConversionRequest{ExpectedVersion: version, Factor: factor, SourceClass: class, SourceRef: "ISO 80000", ValidFrom: from, EvidenceRef: "memo"}
	if class == domain.ConvOverride {
		r.Reason = "supplier pack differs"
	}
	return r
}

// standardUnit creates a unit and gives it a STANDARD conversion in force from 2025-01-01.
func (f *orgFixture) standardUnit(t *testing.T, dim, code, factor string, decimals int) *domain.Unit {
	t.Helper()
	u := f.draftUnit(t, dim, code, decimals)
	out, err := f.s.ProposeConversion(f.ctx, code, false, conversionReq(factor, domain.ConvStandard, "2025-01-01", u.Unit.RecordVersion), unitSteward, false)
	require.NoError(t, err)
	return &out.Unit
}

func (f *orgFixture) convert(t *testing.T, qty, from, to string, day time.Time) (*domain.ConversionResult, error) {
	t.Helper()
	return f.s.ConvertQuantity(f.ctx, domain.ConvertQuantityQuery{Quantity: qty, From: from, To: to, AsOf: day})
}

func ratOf(t *testing.T, s string) *big.Rat {
	t.Helper()
	r, ok := new(big.Rat).SetString(s)
	require.True(t, ok, s)
	return r
}

func TestUnit_ADimensionHasOneBaseUnitAndCodesAreUnique(t *testing.T) {
	f := newORGFixture(t)
	d := f.quantityDim(t, "LENGTH", "M", 6)
	assert.Equal(t, "M", d.Dimension.BaseUnitCode)
	assert.True(t, d.BaseUnit.IsBase)
	assert.Equal(t, domain.UnitActive, d.BaseUnit.Status, "a base unit converts to itself, so it is never a draft")
	assert.Equal(t, 1, f.count(t, `SELECT count(*) FROM event_outbox WHERE event_type = 'unit.dimension.created' AND tenant_id = $1`, f.tenantID))
	assert.Equal(t, 1, f.count(t, `SELECT count(*) FROM event_outbox WHERE event_type = 'unit.created' AND tenant_id = $1`, f.tenantID))

	_, err := f.s.CreateQuantityDimension(f.ctx, domain.CreateQuantityDimensionRequest{DimensionCode: "LENGTH", Name: "dup", BaseUnit: domain.UnitFields{UnitCode: "X", Name: "x", Symbol: "x"}}, unitSteward)
	require.ErrorIs(t, err, registry.ErrConflict, "dimension codes are unique")
	_, err = f.s.CreateQuantityDimension(f.ctx, domain.CreateQuantityDimensionRequest{DimensionCode: "MASS", Name: "m", BaseUnit: domain.UnitFields{UnitCode: "M", Name: "x", Symbol: "x"}}, unitSteward)
	require.ErrorIs(t, err, registry.ErrConflict, "unit codes are unique across the tenant")
	assert.Equal(t, 0, f.count(t, `SELECT count(*) FROM quantity_dimensions WHERE dimension_code = 'MASS'`), "a failed create leaves no half-made dimension")

	_, err = f.s.CreateUnit(f.ctx, domain.CreateUnitRequest{DimensionCode: "NOPE", UnitFields: domain.UnitFields{UnitCode: "Q", Name: "q", Symbol: "q"}}, unitSteward)
	require.ErrorIs(t, err, registry.ErrInvalidInput, "an unknown dimension")
	km := f.draftUnit(t, "LENGTH", "KM", 3)
	assert.Equal(t, domain.UnitDraft, km.Unit.Status)
	assert.False(t, km.Unit.IsBase)
	_, err = f.s.CreateUnit(f.ctx, domain.CreateUnitRequest{DimensionCode: "LENGTH", UnitFields: domain.UnitFields{UnitCode: "KM", Name: "dup", Symbol: "k"}}, unitSteward)
	require.ErrorIs(t, err, registry.ErrConflict)

	require.Error(t, f.execTenant(t, `INSERT INTO units (unit_id, tenant_id, dimension_id, unit_code, name, symbol, is_base, status, decimals, created_by)
		SELECT gen_random_uuid(), tenant_id, dimension_id, 'M2', 'n', 's', true, 'ACTIVE', 2, 'x' FROM quantity_dimensions WHERE dimension_code = 'LENGTH'`), "a second base unit")
	require.Error(t, f.execTenant(t, `UPDATE quantity_dimensions SET name = 'renamed' WHERE dimension_code = 'LENGTH'`), "a dimension is immutable")
	require.Error(t, f.execTenant(t, `DELETE FROM quantity_dimensions WHERE dimension_code = 'LENGTH'`))
	require.Error(t, f.execTenant(t, `DELETE FROM units WHERE unit_code = 'KM'`), "a unit is never deleted")
	require.Error(t, f.execTenant(t, `UPDATE units SET unit_code = 'KMX' WHERE unit_code = 'KM'`), "a unit code never changes")
	require.Error(t, f.execTenant(t, `UPDATE units SET is_base = true WHERE unit_code = 'KM'`), "a unit never becomes the base")

	dims, err := f.s.ListQuantityDimensions(f.ctx)
	require.NoError(t, err)
	require.Len(t, dims, 1)
	assert.Equal(t, "M", dims[0].BaseUnitCode)
	units, err := f.s.ListUnitsByDimension(f.ctx, "LENGTH")
	require.NoError(t, err)
	require.Len(t, units, 2)
	assert.Equal(t, "M", units[0].UnitCode, "the base is first")
	_, err = f.s.ListUnitsByDimension(f.ctx, "NOPE")
	require.ErrorIs(t, err, registry.ErrNotFound)
}
