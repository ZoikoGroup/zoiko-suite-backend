package handler_test

// REF-09 routes. partySvc also implements handler.UnitService, so the OpenAPI parity test covers them in both directions.

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"zoiko.io/tenant-entity-registry-svc/internal/domain"
	"zoiko.io/tenant-entity-registry-svc/internal/registry"
)

type unitState struct {
	err        error
	gotCode    string
	gotNo      int
	gotSup     bool
	gotPropose domain.ProposeConversionRequest
	gotRetire  domain.RetireUnitRequest
	gotAsOf    time.Time
	gotHist    bool
	gotQ       domain.ConvertQuantityQuery
}

var unitStates = map[*partySvc]*unitState{}

func (p *partySvc) um() *unitState {
	if unitStates[p] == nil {
		unitStates[p] = &unitState{}
	}
	return unitStates[p]
}

func (p *partySvc) CreateQuantityDimension(context.Context, domain.CreateQuantityDimensionRequest) (*registry.DimensionWithBase, error) {
	return &registry.DimensionWithBase{}, p.um().err
}
func (p *partySvc) ListQuantityDimensions(context.Context) ([]domain.QuantityDimension, error) {
	return []domain.QuantityDimension{}, p.um().err
}
func (p *partySvc) CreateUnit(context.Context, domain.CreateUnitRequest) (*registry.UnitOutcome, error) {
	return &registry.UnitOutcome{}, p.um().err
}
func (p *partySvc) ProposeConversion(_ context.Context, code string, supersede bool, req domain.ProposeConversionRequest) (*registry.UnitOutcome, error) {
	p.um().gotCode, p.um().gotSup, p.um().gotPropose = code, supersede, req
	return &registry.UnitOutcome{}, p.um().err
}
func (p *partySvc) ApproveConversion(_ context.Context, code string, no int, _ domain.ApproveConversionRequest) (*registry.UnitOutcome, error) {
	p.um().gotCode, p.um().gotNo = code, no
	return &registry.UnitOutcome{}, p.um().err
}
func (p *partySvc) RetireUnit(_ context.Context, code string, req domain.RetireUnitRequest) (*registry.UnitOutcome, error) {
	p.um().gotCode, p.um().gotRetire = code, req
	return &registry.UnitOutcome{}, p.um().err
}
func (p *partySvc) GetUnit(_ context.Context, code string, asOf time.Time, history bool) (*domain.UnitView, error) {
	p.um().gotCode, p.um().gotAsOf, p.um().gotHist = code, asOf, history
	return &domain.UnitView{}, p.um().err
}
func (p *partySvc) GetConversionVersion(_ context.Context, code string, no int) (*domain.ConversionVersion, error) {
	p.um().gotCode, p.um().gotNo = code, no
	return &domain.ConversionVersion{}, p.um().err
}
func (p *partySvc) ListUnitsByDimension(_ context.Context, code string) ([]domain.Unit, error) {
	p.um().gotCode = code
	return []domain.Unit{}, p.um().err
}
func (p *partySvc) ConvertQuantity(_ context.Context, q domain.ConvertQuantityQuery) (*domain.ConversionResult, error) {
	p.um().gotQ = q
	return &domain.ConversionResult{}, p.um().err
}

func TestUnitRoutes_AbsentWhenTheServiceOffersNone(t *testing.T) {
	assert.Equal(t, http.StatusNotFound, do(newRouter(&stubSvc{}), http.MethodPost, "/v1/units", `{"unit_code":"KM"}`).Code)
}

func TestUnitRoutes_Commands(t *testing.T) {
	p := &partySvc{}
	r := newPartyRouter(p)
	assert.Equal(t, http.StatusCreated, do(r, http.MethodPost, "/v1/quantity-dimensions", `{"dimension_code":"LENGTH","name":"n","base_unit":{"unit_code":"M","name":"Metre","symbol":"m"}}`).Code)
	assert.Equal(t, http.StatusOK, do(r, http.MethodGet, "/v1/quantity-dimensions", "").Code)
	assert.Equal(t, http.StatusBadRequest, do(r, http.MethodPost, "/v1/quantity-dimensions", "nope").Code)
	assert.Equal(t, http.StatusCreated, do(r, http.MethodPost, "/v1/units", `{"dimension_code":"LENGTH","unit_code":"KM","name":"Kilometre","symbol":"km"}`).Code)

	body := `{"expected_version":1,"factor":"1000","source_class":"STANDARD","source_ref":"ISO 80000","evidence_ref":"e"}`
	assert.Equal(t, http.StatusCreated, do(r, http.MethodPost, "/v1/units/KM/conversions", body).Code)
	assert.Equal(t, "KM", p.um().gotCode)
	assert.False(t, p.um().gotSup, "the plain route is the first conversion")
	assert.Equal(t, "1000", p.um().gotPropose.Factor)
	assert.Equal(t, http.StatusCreated, do(r, http.MethodPost, "/v1/units/KM/conversions:supersede", body).Code)
	assert.True(t, p.um().gotSup, "the route, not the body, says it replaces the active conversion")

	assert.Equal(t, http.StatusOK, do(r, http.MethodPost, "/v1/units/KM/conversions/3:approve", `{"expected_version":4,"evidence_ref":"e"}`).Code)
	assert.Equal(t, 3, p.um().gotNo)
	assert.Equal(t, http.StatusBadRequest, do(r, http.MethodPost, "/v1/units/KM/conversions/x:approve", `{"evidence_ref":"e"}`).Code)

	assert.Equal(t, http.StatusOK, do(r, http.MethodPost, "/v1/units/KM:retire", `{"expected_version":2,"reason":"r","evidence_ref":"e"}`).Code)
	assert.Equal(t, "KM", p.um().gotCode)
	assert.Equal(t, "r", p.um().gotRetire.Reason)
}

func TestUnitRoutes_Reads(t *testing.T) {
	p := &partySvc{}
	r := newPartyRouter(p)
	require.Equal(t, http.StatusOK, do(r, http.MethodGet, "/v1/units/KM?as_of=2025-06-01&history=true", "").Code)
	assert.Equal(t, time.Date(2025, 6, 1, 0, 0, 0, 0, time.UTC), p.um().gotAsOf)
	assert.True(t, p.um().gotHist)
	assert.Equal(t, http.StatusBadRequest, do(r, http.MethodGet, "/v1/units/KM?as_of=junk", "").Code)

	require.Equal(t, http.StatusOK, do(r, http.MethodGet, "/v1/units/KM/conversions/2", "").Code)
	assert.Equal(t, 2, p.um().gotNo)
	assert.Equal(t, http.StatusBadRequest, do(r, http.MethodGet, "/v1/units/KM/conversions/0", "").Code)
	require.Equal(t, http.StatusOK, do(r, http.MethodGet, "/v1/quantity-dimensions/LENGTH/units", "").Code)
	assert.Equal(t, "LENGTH", p.um().gotCode)

	require.Equal(t, http.StatusOK, do(r, http.MethodGet, "/v1/units:convert?quantity=2.5&from=KM&to=M&as_of=2025-06-01", "").Code)
	assert.Equal(t, domain.ConvertQuantityQuery{Quantity: "2.5", From: "KM", To: "M", AsOf: time.Date(2025, 6, 1, 0, 0, 0, 0, time.UTC)}, p.um().gotQ)
	assert.Equal(t, http.StatusBadRequest, do(r, http.MethodGet, "/v1/units:convert?as_of=junk", "").Code)
}

func TestUnitRoutes_TypedErrors(t *testing.T) {
	for _, tc := range []struct {
		err    error
		status int
		code   string
	}{
		{registry.ErrDimensionMismatch, http.StatusUnprocessableEntity, "DIMENSION_MISMATCH"},
		{registry.ErrConversionMissing, http.StatusNotFound, "CONVERSION_MISSING"},
		{registry.ErrNotFound, http.StatusNotFound, "NOT_FOUND"},
		{registry.ErrSelfApproval, http.StatusForbidden, "SOD_DENIED"},
		{registry.ErrStateConflict, http.StatusConflict, "INVALID_TRANSITION"},
		{registry.ErrConflict, http.StatusConflict, "DUPLICATE_CANDIDATE"},
		{registry.ErrVersionConflict, http.StatusConflict, "VERSION_CONFLICT"},
		{registry.ErrInvalidInput, http.StatusBadRequest, "VALIDATION_FAILED"},
	} {
		p := &partySvc{}
		p.um().err = tc.err
		rec := do(newPartyRouter(p), http.MethodGet, "/v1/units:convert?quantity=1&from=KM&to=M", "")
		assert.Equal(t, tc.status, rec.Code, tc.err.Error())
		assert.Contains(t, rec.Body.String(), tc.code, tc.err.Error())
	}
}
