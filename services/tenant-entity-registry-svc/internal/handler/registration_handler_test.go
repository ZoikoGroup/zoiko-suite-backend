package handler_test

// ORG-09 registration routes. partySvc also implements handler.RegistrationService, so the OpenAPI parity test covers
// these routes in both directions.

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

const regID = "0190a000-0000-7000-8000-00000000eeee"

type regState struct {
	out       *registry.RegistrationOutcome
	err       error
	gotCr     domain.CreateRegistrationRequest
	gotEnd    domain.RegistrationEndRequest
	gotTo     domain.RegistrationStatus
	gotID     string
	gotReveal bool
	gotFind   [4]string
	gotList   domain.RegistrationSearchQuery
	gotDays   int
	gotAsOf   time.Time
}

var regStates = map[*partySvc]*regState{}

func (p *partySvc) r() *regState {
	if regStates[p] == nil {
		regStates[p] = &regState{}
	}
	return regStates[p]
}

func (p *partySvc) CreateRegistration(_ context.Context, req domain.CreateRegistrationRequest) (*registry.RegistrationOutcome, error) {
	p.r().gotCr = req
	return p.r().out, p.r().err
}
func (p *partySvc) VerifyRegistration(_ context.Context, id string, _ domain.VerifyRegistrationRequest) (*registry.RegistrationOutcome, error) {
	p.r().gotID = id
	return p.r().out, p.r().err
}
func (p *partySvc) RenewRegistration(_ context.Context, id string, _ domain.RenewRegistrationRequest) (*registry.RegistrationOutcome, error) {
	p.r().gotID = id
	return p.r().out, p.r().err
}
func (p *partySvc) ChangeRegistrationStatus(_ context.Context, id string, to domain.RegistrationStatus, req domain.RegistrationEndRequest) (*registry.RegistrationOutcome, error) {
	p.r().gotID, p.r().gotTo, p.r().gotEnd = id, to, req
	return p.r().out, p.r().err
}
func (p *partySvc) GetRegistration(_ context.Context, id string, reveal bool) (*domain.Registration, error) {
	p.r().gotID, p.r().gotReveal = id, reveal
	return &domain.Registration{}, p.r().err
}
func (p *partySvc) GetRegistrationHistory(_ context.Context, id string) ([]domain.RegistrationTransition, error) {
	p.r().gotID = id
	return []domain.RegistrationTransition{}, p.r().err
}
func (p *partySvc) FindRegistrationsByNumber(_ context.Context, number string, typ domain.RegistrationType, authority, jurisdiction string, reveal bool) ([]domain.Registration, error) {
	p.r().gotFind, p.r().gotReveal = [4]string{number, string(typ), authority, jurisdiction}, reveal
	return []domain.Registration{}, p.r().err
}
func (p *partySvc) ListRegistrations(_ context.Context, q domain.RegistrationSearchQuery, reveal bool) ([]domain.Registration, string, error) {
	p.r().gotList, p.r().gotReveal = q, reveal
	return []domain.Registration{}, "next", p.r().err
}
func (p *partySvc) ListExpiringRegistrations(_ context.Context, days int, asOf time.Time, reveal bool) ([]domain.Registration, error) {
	p.r().gotDays, p.r().gotAsOf, p.r().gotReveal = days, asOf, reveal
	return []domain.Registration{}, p.r().err
}

const regBody = `{"registration_type":"LICENSE","issuing_authority":"FCA","jurisdiction":"GB","number":"123","holder_legal_entity_id":"e","effective_from":"2025-01-01","source_class":"AUTHORITY_DOCUMENT","evidence_ref":"e"}`

func TestRegistrationRoutes_AbsentWhenTheServiceOffersNoRegistrations(t *testing.T) {
	assert.Equal(t, http.StatusNotFound, do(newRouter(&stubSvc{}), http.MethodPost, "/v1/registrations", regBody).Code)
}

func TestRegistrationRoutes_CommandsAndStatusCodes(t *testing.T) {
	p := &partySvc{}
	p.r().out = &registry.RegistrationOutcome{}
	r := newPartyRouter(p)

	assert.Equal(t, http.StatusCreated, do(r, http.MethodPost, "/v1/registrations", regBody).Code)
	assert.Equal(t, domain.RegLicense, p.r().gotCr.RegistrationType)
	assert.NotEmpty(t, p.r().gotCr.CorrelationID)
	p.r().out = &registry.RegistrationOutcome{Replayed: true}
	assert.Equal(t, http.StatusOK, do(r, http.MethodPost, "/v1/registrations", regBody).Code)
	assert.Equal(t, http.StatusBadRequest, do(r, http.MethodPost, "/v1/registrations", "nope").Code)

	p.r().out = &registry.RegistrationOutcome{}
	for _, path := range []string{":verify", ":renew"} {
		assert.Equal(t, http.StatusOK, do(r, http.MethodPost, "/v1/registrations/"+regID+path, `{"expected_version":2,"evidence_ref":"e","new_expiry_date":"2030-01-01"}`).Code, path)
		assert.Equal(t, regID, p.r().gotID)
	}
	for path, to := range map[string]domain.RegistrationStatus{":suspend": domain.RegSuspended, ":expire": domain.RegExpired, ":revoke": domain.RegRevoked} {
		rec := do(r, http.MethodPost, "/v1/registrations/"+regID+path, `{"expected_version":4,"reason":"r","evidence_ref":"e","to":"ACTIVE"}`)
		assert.Equal(t, http.StatusOK, rec.Code, path)
		assert.Equal(t, to, p.r().gotTo, "the route, not the body, names the target status")
		assert.Equal(t, int64(4), p.r().gotEnd.ExpectedVersion)
	}
}

func TestRegistrationRoutes_Reads(t *testing.T) {
	p := &partySvc{}
	r := newPartyRouter(p)

	require.Equal(t, http.StatusOK, do(r, http.MethodGet, "/v1/registrations/"+regID+"?reveal=true", "").Code)
	assert.True(t, p.r().gotReveal)
	require.Equal(t, http.StatusOK, do(r, http.MethodGet, "/v1/registrations/"+regID, "").Code)
	assert.False(t, p.r().gotReveal, "masked by default")
	require.Equal(t, http.StatusOK, do(r, http.MethodGet, "/v1/registrations/"+regID+"/history", "").Code)

	require.Equal(t, http.StatusOK, do(r, http.MethodGet, "/v1/registrations:find?number=AB-1&type=LICENSE&authority=FCA&jurisdiction=GB", "").Code)
	assert.Equal(t, [4]string{"AB-1", "LICENSE", "FCA", "GB"}, p.r().gotFind)

	require.Equal(t, http.StatusOK, do(r, http.MethodGet, "/v1/registrations?holder_legal_entity_id=e&type=PERMIT&status=ACTIVE&jurisdiction=GB&limit=5&cursor=c", "").Code)
	assert.Equal(t, domain.RegistrationSearchQuery{HolderLegalEntityID: "e", Type: domain.RegPermit, Status: domain.RegActive, Jurisdiction: "GB", Limit: 5, After: "c"}, p.r().gotList)
	assert.Equal(t, http.StatusBadRequest, do(r, http.MethodGet, "/v1/registrations?limit=0", "").Code)

	require.Equal(t, http.StatusOK, do(r, http.MethodGet, "/v1/registrations/expiring", "").Code)
	assert.Equal(t, 30, p.r().gotDays, "a month by default")
	require.Equal(t, http.StatusOK, do(r, http.MethodGet, "/v1/registrations/expiring?within_days=90&as_of=2026-01-01", "").Code)
	assert.Equal(t, 90, p.r().gotDays)
	assert.Equal(t, time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC), p.r().gotAsOf)
	assert.Equal(t, http.StatusBadRequest, do(r, http.MethodGet, "/v1/registrations/expiring?within_days=-1", "").Code)
	assert.Equal(t, http.StatusBadRequest, do(r, http.MethodGet, "/v1/registrations/expiring?as_of=junk", "").Code)
}

func TestRegistrationRoutes_TypedErrors(t *testing.T) {
	for _, tc := range []struct {
		err    error
		status int
		code   string
	}{
		{registry.ErrNotFound, http.StatusNotFound, "NOT_FOUND"},
		{registry.ErrVersionConflict, http.StatusConflict, "VERSION_CONFLICT"},
		{registry.ErrStateConflict, http.StatusConflict, "INVALID_TRANSITION"},
		{registry.ErrSelfApproval, http.StatusForbidden, "SOD_DENIED"},
		{registry.ErrConflict, http.StatusConflict, "DUPLICATE_CANDIDATE"},
		{registry.ErrCreateRequestMismatch, http.StatusConflict, "IDEMPOTENCY_MISMATCH"},
		{registry.ErrUnauthorized, http.StatusForbidden, "AUTHORIZATION_DENIED"},
	} {
		p := &partySvc{}
		p.r().err = tc.err
		rec := do(newPartyRouter(p), http.MethodPost, "/v1/registrations/"+regID+":verify", `{"expected_version":1,"evidence_ref":"e"}`)
		assert.Equal(t, tc.status, rec.Code, tc.err.Error())
		assert.Contains(t, rec.Body.String(), tc.code, tc.err.Error())
	}
}
