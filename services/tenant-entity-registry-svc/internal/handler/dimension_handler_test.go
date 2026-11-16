package handler_test

// REF-08 routes. partySvc also implements handler.DimensionService, so the OpenAPI parity test covers them in both
// directions.

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

const dimValueID = "0190a000-0000-7000-8000-0000000d0001"

type dimState struct {
	err       error
	noop      bool
	gotID     string
	gotTo     string
	gotNo     int
	gotRep    domain.ReparentDimensionValueRequest
	gotMap    domain.MapSourceObjectRequest
	gotAsOf   time.Time
	gotHist   bool
	gotType   string
	gotEntity string
	gotQ      domain.ResolveDimensionQuery
}

var dimStates = map[*partySvc]*dimState{}

func (p *partySvc) dm() *dimState {
	if dimStates[p] == nil {
		dimStates[p] = &dimState{}
	}
	return dimStates[p]
}

func (p *partySvc) CreateDimensionType(context.Context, domain.CreateDimensionTypeRequest) (*domain.DimensionType, error) {
	return &domain.DimensionType{}, p.dm().err
}
func (p *partySvc) ListDimensionTypes(context.Context) ([]domain.DimensionType, error) {
	return []domain.DimensionType{}, p.dm().err
}
func (p *partySvc) CreateDimensionValue(context.Context, domain.CreateDimensionValueRequest) (*registry.DimensionOutcome, error) {
	return &registry.DimensionOutcome{}, p.dm().err
}
func (p *partySvc) ReparentDimensionValue(_ context.Context, id string, req domain.ReparentDimensionValueRequest) (*registry.DimensionOutcome, error) {
	p.dm().gotID, p.dm().gotRep = id, req
	return &registry.DimensionOutcome{}, p.dm().err
}
func (p *partySvc) ApproveDimensionHierarchy(_ context.Context, id string, no int, _ domain.ApproveDimensionHierarchyRequest) (*registry.DimensionOutcome, error) {
	p.dm().gotID, p.dm().gotNo = id, no
	return &registry.DimensionOutcome{}, p.dm().err
}
func (p *partySvc) ChangeDimensionStatus(_ context.Context, id, to string, _ domain.DimensionStatusRequest) (*registry.DimensionOutcome, error) {
	p.dm().gotID, p.dm().gotTo = id, to
	return &registry.DimensionOutcome{}, p.dm().err
}
func (p *partySvc) MapSourceObject(_ context.Context, id string, req domain.MapSourceObjectRequest) (*registry.MappingOutcome, error) {
	p.dm().gotID, p.dm().gotMap = id, req
	return &registry.MappingOutcome{NoOp: p.dm().noop}, p.dm().err
}
func (p *partySvc) GetDimensionValue(_ context.Context, id string, asOf time.Time, history bool) (*domain.DimensionValueView, error) {
	p.dm().gotID, p.dm().gotAsOf, p.dm().gotHist = id, asOf, history
	return &domain.DimensionValueView{}, p.dm().err
}
func (p *partySvc) GetHierarchyAsOf(_ context.Context, typeCode, entity string, asOf time.Time) (*domain.HierarchyAsOf, error) {
	p.dm().gotType, p.dm().gotEntity, p.dm().gotAsOf = typeCode, entity, asOf
	return &domain.HierarchyAsOf{}, p.dm().err
}
func (p *partySvc) ListPostingEligibleValues(_ context.Context, typeCode, entity string, asOf time.Time) ([]registry.EligibleDimensionValue, error) {
	p.dm().gotType, p.dm().gotEntity, p.dm().gotAsOf = typeCode, entity, asOf
	return []registry.EligibleDimensionValue{}, p.dm().err
}
func (p *partySvc) ResolveDimension(_ context.Context, q domain.ResolveDimensionQuery) (*domain.DimensionResolution, error) {
	p.dm().gotQ = q
	return &domain.DimensionResolution{}, p.dm().err
}

func TestDimensionRoutes_AbsentWhenTheServiceOffersNone(t *testing.T) {
	assert.Equal(t, http.StatusNotFound, do(newRouter(&stubSvc{}), http.MethodPost, "/v1/dimension-types", `{"type_code":"AB"}`).Code)
}

func TestDimensionRoutes_Commands(t *testing.T) {
	p := &partySvc{}
	r := newPartyRouter(p)
	assert.Equal(t, http.StatusCreated, do(r, http.MethodPost, "/v1/dimension-types", `{"type_code":"REGION","name":"n","hierarchical":true}`).Code)
	assert.Equal(t, http.StatusOK, do(r, http.MethodGet, "/v1/dimension-types", "").Code)
	assert.Equal(t, http.StatusCreated, do(r, http.MethodPost, "/v1/dimension-values", `{"type_code":"REGION","code":"UK","label":"l","evidence_ref":"e"}`).Code)
	assert.Equal(t, http.StatusBadRequest, do(r, http.MethodPost, "/v1/dimension-values", "nope").Code)

	parent := "0190a000-0000-7000-8000-0000000d0002"
	assert.Equal(t, http.StatusCreated, do(r, http.MethodPost, "/v1/dimension-values/"+dimValueID+"/hierarchy", `{"expected_version":2,"parent_value_id":"`+parent+`","reason":"r","evidence_ref":"e"}`).Code)
	assert.Equal(t, dimValueID, p.dm().gotID)
	require.NotNil(t, p.dm().gotRep.ParentValueID)
	assert.Equal(t, parent, *p.dm().gotRep.ParentValueID)
	assert.Equal(t, http.StatusCreated, do(r, http.MethodPost, "/v1/dimension-values/"+dimValueID+"/hierarchy", `{"expected_version":2,"root":true,"reason":"r","evidence_ref":"e"}`).Code)
	assert.True(t, p.dm().gotRep.Root)

	assert.Equal(t, http.StatusOK, do(r, http.MethodPost, "/v1/dimension-values/"+dimValueID+"/hierarchy/3:approve", `{"expected_version":4,"evidence_ref":"e"}`).Code)
	assert.Equal(t, 3, p.dm().gotNo)
	assert.Equal(t, http.StatusBadRequest, do(r, http.MethodPost, "/v1/dimension-values/"+dimValueID+"/hierarchy/x:approve", `{"evidence_ref":"e"}`).Code)

	for path, to := range map[string]string{":activate": domain.DimActive, ":deactivate": domain.DimInactive} {
		assert.Equal(t, http.StatusOK, do(r, http.MethodPost, "/v1/dimension-values/"+dimValueID+path, `{"expected_version":1,"reason":"r","evidence_ref":"e","to_status":"ACTIVE"}`).Code, path)
		assert.Equal(t, to, p.dm().gotTo, "the route, not the body, names the target status")
	}

	assert.Equal(t, http.StatusCreated, do(r, http.MethodPost, "/v1/dimension-values/"+dimValueID+"/source-mappings", `{"source_domain":"PROJECT","source_object_id":"P-1","evidence_ref":"e"}`).Code)
	assert.Equal(t, "P-1", p.dm().gotMap.SourceObjectID)
	p.dm().noop = true
	assert.Equal(t, http.StatusOK, do(r, http.MethodPost, "/v1/dimension-values/"+dimValueID+"/source-mappings", `{"source_domain":"PROJECT","source_object_id":"P-1","evidence_ref":"e"}`).Code, "already mapped is 200")
}

func TestDimensionRoutes_Reads(t *testing.T) {
	p := &partySvc{}
	r := newPartyRouter(p)
	require.Equal(t, http.StatusOK, do(r, http.MethodGet, "/v1/dimension-values/"+dimValueID+"?as_of=2025-06-01&history=true", "").Code)
	assert.Equal(t, time.Date(2025, 6, 1, 0, 0, 0, 0, time.UTC), p.dm().gotAsOf)
	assert.True(t, p.dm().gotHist)
	assert.Equal(t, http.StatusBadRequest, do(r, http.MethodGet, "/v1/dimension-values/"+dimValueID+"?as_of=junk", "").Code)

	require.Equal(t, http.StatusOK, do(r, http.MethodGet, "/v1/dimension-types/REGION/hierarchy?as_of=2025-06-01&legal_entity_id=e1", "").Code)
	assert.Equal(t, "REGION", p.dm().gotType)
	assert.Equal(t, "e1", p.dm().gotEntity)
	require.Equal(t, http.StatusOK, do(r, http.MethodGet, "/v1/dimension-types/REGION/posting-eligible-values", "").Code)

	require.Equal(t, http.StatusOK, do(r, http.MethodGet, "/v1/dimensions:resolve?type_code=REGION&code=UK&legal_entity_id=e1&as_of=2025-06-01", "").Code)
	assert.Equal(t, domain.ResolveDimensionQuery{TypeCode: "REGION", Code: "UK", LegalEntityID: "e1", AsOf: time.Date(2025, 6, 1, 0, 0, 0, 0, time.UTC)}, p.dm().gotQ)
	require.Equal(t, http.StatusOK, do(r, http.MethodGet, "/v1/dimensions:resolve?type_code=PROJECT&source_domain=PROJECT&source_object_id=P-1&as_of=2025-06-01", "").Code)
	assert.Equal(t, "P-1", p.dm().gotQ.SourceObjectID)
	assert.Equal(t, http.StatusBadRequest, do(r, http.MethodGet, "/v1/dimensions:resolve?as_of=junk", "").Code)
}

func TestDimensionRoutes_TypedErrors(t *testing.T) {
	for _, tc := range []struct {
		err    error
		status int
		code   string
	}{
		{registry.ErrNotFound, http.StatusNotFound, "NOT_FOUND"},
		{registry.ErrSelfApproval, http.StatusForbidden, "SOD_DENIED"},
		{registry.ErrStateConflict, http.StatusConflict, "INVALID_TRANSITION"},
		{registry.ErrConflict, http.StatusConflict, "DUPLICATE_CANDIDATE"},
		{registry.ErrVersionConflict, http.StatusConflict, "VERSION_CONFLICT"},
		{registry.ErrInvalidInput, http.StatusBadRequest, "VALIDATION_FAILED"},
	} {
		p := &partySvc{}
		p.dm().err = tc.err
		rec := do(newPartyRouter(p), http.MethodPost, "/v1/dimension-values/"+dimValueID+":activate", `{"expected_version":1,"evidence_ref":"e"}`)
		assert.Equal(t, tc.status, rec.Code, tc.err.Error())
		assert.Contains(t, rec.Body.String(), tc.code, tc.err.Error())
	}
}
