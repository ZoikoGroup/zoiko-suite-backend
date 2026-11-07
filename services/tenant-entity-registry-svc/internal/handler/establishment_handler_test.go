package handler_test

// ORG-08 establishment routes. partySvc also implements handler.EstablishmentService, so the OpenAPI parity test
// covers these routes in both directions.

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

const (
	estID       = "0190a000-0000-7000-8000-00000000cccc"
	estEntityID = "0190a000-0000-7000-8000-00000000dddd"
)

type estState struct {
	out     *registry.EstablishmentOutcome
	err     error
	gotCr   domain.CreateEstablishmentRequest
	gotChg  domain.ChangeEstablishmentStatusRequest
	gotID   string
	gotHist bool
	gotAsOf time.Time
	gotStat domain.EstablishmentStatus
	asOfHit bool
	incl    bool
}

var estStates = map[*partySvc]*estState{}

func (p *partySvc) e() *estState {
	if estStates[p] == nil {
		estStates[p] = &estState{}
	}
	return estStates[p]
}

func (p *partySvc) CreateEstablishment(_ context.Context, req domain.CreateEstablishmentRequest) (*registry.EstablishmentOutcome, error) {
	p.e().gotCr = req
	return p.e().out, p.e().err
}
func (p *partySvc) ChangeEstablishmentStatus(_ context.Context, id string, req domain.ChangeEstablishmentStatusRequest) (*registry.EstablishmentOutcome, error) {
	p.e().gotID, p.e().gotChg = id, req
	return p.e().out, p.e().err
}
func (p *partySvc) GetEstablishment(_ context.Context, id string, asOf time.Time, history bool) (*domain.EstablishmentView, error) {
	p.e().gotID, p.e().gotAsOf, p.e().gotHist = id, asOf, history
	return &domain.EstablishmentView{}, p.e().err
}
func (p *partySvc) ListEntityEstablishments(_ context.Context, _ string, status domain.EstablishmentStatus) ([]domain.Establishment, error) {
	p.e().gotStat = status
	return []domain.Establishment{}, p.e().err
}
func (p *partySvc) GetEstablishmentsAsOf(_ context.Context, _ string, asOf time.Time) ([]domain.EstablishmentView, error) {
	p.e().asOfHit, p.e().gotAsOf = true, asOf
	return []domain.EstablishmentView{}, p.e().err
}
func (p *partySvc) ListEntityAddresses(_ context.Context, _ string, asOf time.Time, includeEnded bool) ([]domain.EntityAddress, error) {
	p.e().gotAsOf, p.e().incl = asOf, includeEnded
	return []domain.EntityAddress{}, p.e().err
}

const estBody = `{"legal_entity_id":"` + estEntityID + `","establishment_type":"BRANCH","name":"Leeds","address_id":"a","source_class":"AUTHORITY_DOCUMENT","evidence_ref":"e"}`

func TestEstablishmentRoutes_AbsentWhenTheServiceOffersNoEstablishments(t *testing.T) {
	assert.Equal(t, http.StatusNotFound, do(newRouter(&stubSvc{}), http.MethodPost, "/v1/establishments", estBody).Code)
}

func TestEstablishmentRoutes_CreateAndStatusCommands(t *testing.T) {
	p := &partySvc{}
	p.e().out = &registry.EstablishmentOutcome{}
	r := newPartyRouter(p)

	assert.Equal(t, http.StatusCreated, do(r, http.MethodPost, "/v1/establishments", estBody).Code)
	assert.Equal(t, domain.EstBranch, p.e().gotCr.EstablishmentType)
	assert.NotEmpty(t, p.e().gotCr.CorrelationID)
	p.e().out = &registry.EstablishmentOutcome{Replayed: true}
	assert.Equal(t, http.StatusOK, do(r, http.MethodPost, "/v1/establishments", estBody).Code)

	p.e().out = &registry.EstablishmentOutcome{}
	for path, to := range map[string]domain.EstablishmentStatus{
		":activate": domain.EstActive, ":deactivate": domain.EstInactive, ":close": domain.EstClosed,
	} {
		rec := do(r, http.MethodPost, "/v1/establishments/"+estID+path, `{"expected_version":3,"evidence_ref":"e","reason":"r"}`)
		assert.Equal(t, http.StatusOK, rec.Code, path)
		assert.Equal(t, to, p.e().gotChg.To, "the route, not the body, names the target status")
		assert.Equal(t, estID, p.e().gotID)
		assert.Equal(t, int64(3), p.e().gotChg.ExpectedVersion)
	}
	// A body cannot choose its own target.
	do(r, http.MethodPost, "/v1/establishments/"+estID+":close", `{"expected_version":1,"evidence_ref":"e","reason":"r","to":"ACTIVE","To":"ACTIVE"}`)
	assert.Equal(t, domain.EstClosed, p.e().gotChg.To)
	assert.Equal(t, http.StatusBadRequest, do(r, http.MethodPost, "/v1/establishments", "nope").Code)
}

func TestEstablishmentRoutes_Reads(t *testing.T) {
	p := &partySvc{}
	r := newPartyRouter(p)
	require.Equal(t, http.StatusOK, do(r, http.MethodGet, "/v1/establishments/"+estID+"?as_of=2025-06-01&history=true", "").Code)
	assert.Equal(t, time.Date(2025, 6, 1, 0, 0, 0, 0, time.UTC), p.e().gotAsOf)
	assert.True(t, p.e().gotHist)
	assert.Equal(t, http.StatusBadRequest, do(r, http.MethodGet, "/v1/establishments/"+estID+"?as_of=junk", "").Code)

	require.Equal(t, http.StatusOK, do(r, http.MethodGet, "/v1/entities/"+estEntityID+"/establishments?status=ACTIVE", "").Code)
	assert.Equal(t, domain.EstActive, p.e().gotStat)
	assert.False(t, p.e().asOfHit, "no as_of means the current-status list")

	require.Equal(t, http.StatusOK, do(r, http.MethodGet, "/v1/entities/"+estEntityID+"/establishments?as_of=2025-03-01", "").Code)
	assert.True(t, p.e().asOfHit, "as_of reconstructs the active set on that date")
	assert.Equal(t, http.StatusBadRequest, do(r, http.MethodGet, "/v1/entities/"+estEntityID+"/establishments?as_of=junk", "").Code)

	require.Equal(t, http.StatusOK, do(r, http.MethodGet, "/v1/entities/"+estEntityID+"/addresses?include_ended=true", "").Code)
	assert.True(t, p.e().incl)
}

func TestEstablishmentRoutes_TypedErrors(t *testing.T) {
	for _, tc := range []struct {
		err    error
		status int
		code   string
	}{
		{registry.ErrNotFound, http.StatusNotFound, "NOT_FOUND"},
		{registry.ErrVersionConflict, http.StatusConflict, "VERSION_CONFLICT"},
		{registry.ErrStateConflict, http.StatusConflict, "INVALID_TRANSITION"},
		{registry.ErrSelfApproval, http.StatusForbidden, "SOD_DENIED"},
		{registry.ErrCreateRequestMismatch, http.StatusConflict, "IDEMPOTENCY_MISMATCH"},
		{registry.ErrEntityNotOperational, http.StatusConflict, "INVALID_TRANSITION"},
	} {
		p := &partySvc{}
		p.e().err = tc.err
		rec := do(newPartyRouter(p), http.MethodPost, "/v1/establishments/"+estID+":activate", `{"expected_version":1,"evidence_ref":"e"}`)
		assert.Equal(t, tc.status, rec.Code, tc.err.Error())
		assert.Contains(t, rec.Body.String(), tc.code, tc.err.Error())
	}
}
