package handler_test

// ORG-08 address routes: status codes, the typed error codes, and that the request reaches the service intact.
// partySvc also implements handler.AddressService, so the OpenAPI parity test covers these routes too.

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

const addressID = "0190a000-0000-7000-8000-00000000bbbb"

// addrState is what the stub answers and what it saw.
type addrState struct {
	out     *registry.AddressOutcome
	verify  *registry.VerifyOutcome
	link    *registry.AddressLinkOutcome
	err     error
	gotAsOf time.Time
	gotHist bool
	gotQ    domain.AddressSearchQuery
	gotCr   domain.CreateAddressRequest
	gotAm   domain.AmendAddressRequest
	gotVer  domain.VerifyAddressRequest
	gotLink domain.LinkAddressRequest
	gotIncl bool
}

var addrStates = map[*partySvc]*addrState{}

func (p *partySvc) a() *addrState {
	if addrStates[p] == nil {
		addrStates[p] = &addrState{}
	}
	return addrStates[p]
}

func (p *partySvc) CreateAddress(_ context.Context, req domain.CreateAddressRequest) (*registry.AddressOutcome, error) {
	p.a().gotCr = req
	return p.a().out, p.a().err
}
func (p *partySvc) AmendAddress(_ context.Context, _ string, req domain.AmendAddressRequest) (*registry.AddressOutcome, error) {
	p.a().gotAm = req
	return p.a().out, p.a().err
}
func (p *partySvc) VerifyAddress(_ context.Context, _ string, req domain.VerifyAddressRequest) (*registry.VerifyOutcome, error) {
	p.a().gotVer = req
	return p.a().verify, p.a().err
}
func (p *partySvc) LinkAddressToParty(_ context.Context, _ string, req domain.LinkAddressRequest) (*registry.AddressLinkOutcome, error) {
	p.a().gotLink = req
	return p.a().link, p.a().err
}
func (p *partySvc) GetAddress(_ context.Context, _ string, asOf time.Time, history bool) (*domain.AddressView, error) {
	p.a().gotAsOf, p.a().gotHist = asOf, history
	return &domain.AddressView{}, p.a().err
}
func (p *partySvc) SearchAddresses(_ context.Context, q domain.AddressSearchQuery) ([]domain.AddressSummary, string, error) {
	p.a().gotQ = q
	return []domain.AddressSummary{}, "next", p.a().err
}
func (p *partySvc) ListPartyAddresses(_ context.Context, _ string, asOf time.Time, includeEnded bool) ([]domain.PartyAddress, error) {
	p.a().gotAsOf, p.a().gotIncl = asOf, includeEnded
	return []domain.PartyAddress{}, p.a().err
}

const addrBody = `{"address_type":"BILLING","lines":["1 Main St"],"locality":"Leeds","country":"GB","source_class":"TENANT_ASSERTED","evidence_ref":"e"}`

func TestAddressRoutes_AbsentWhenTheServiceOffersNoAddresses(t *testing.T) {
	assert.Equal(t, http.StatusNotFound, do(newRouter(&stubSvc{}), http.MethodPost, "/v1/addresses", addrBody).Code)
}

func TestAddressRoutes_StatusCodes(t *testing.T) {
	p := &partySvc{}
	p.a().out = &registry.AddressOutcome{}
	r := newPartyRouter(p)
	assert.Equal(t, http.StatusCreated, do(r, http.MethodPost, "/v1/addresses", addrBody).Code)
	assert.Equal(t, "BILLING", string(p.a().gotCr.AddressType))
	assert.NotEmpty(t, p.a().gotCr.CorrelationID, "the request correlation id is filled in")

	p.a().out = &registry.AddressOutcome{Replayed: true}
	assert.Equal(t, http.StatusOK, do(r, http.MethodPost, "/v1/addresses", addrBody).Code, "a replay is 200")

	p.a().out = &registry.AddressOutcome{}
	assert.Equal(t, http.StatusCreated, do(r, http.MethodPost, "/v1/addresses/"+addressID+"/versions", `{"expected_version":1}`).Code)
	assert.Equal(t, 1, p.a().gotAm.ExpectedVersion)
	p.a().out = &registry.AddressOutcome{NoOp: true}
	assert.Equal(t, http.StatusOK, do(r, http.MethodPost, "/v1/addresses/"+addressID+"/versions", `{"expected_version":1}`).Code)

	p.a().verify = &registry.VerifyOutcome{}
	assert.Equal(t, http.StatusOK, do(r, http.MethodPost, "/v1/addresses/"+addressID+":verify", `{"expected_version":2,"verifier":"LOCAL_RULES"}`).Code)
	assert.Equal(t, 2, p.a().gotVer.ExpectedVersion)
	assert.Equal(t, "LOCAL_RULES", p.a().gotVer.Verifier)

	p.a().link = &registry.AddressLinkOutcome{}
	assert.Equal(t, http.StatusCreated, do(r, http.MethodPost, "/v1/parties/"+partyID+"/addresses", `{"address_id":"a","link_type":"BILLING"}`).Code)
	p.a().link = &registry.AddressLinkOutcome{NoOp: true}
	assert.Equal(t, http.StatusOK, do(r, http.MethodPost, "/v1/parties/"+partyID+"/addresses", `{"address_id":"a","link_type":"BILLING"}`).Code)
}

func TestAddressRoutes_ReadParameters(t *testing.T) {
	p := &partySvc{}
	r := newPartyRouter(p)
	require.Equal(t, http.StatusOK, do(r, http.MethodGet, "/v1/addresses/"+addressID+"?as_of=2025-06-01&history=true", "").Code)
	assert.Equal(t, time.Date(2025, 6, 1, 0, 0, 0, 0, time.UTC), p.a().gotAsOf)
	assert.True(t, p.a().gotHist)
	assert.Equal(t, http.StatusBadRequest, do(r, http.MethodGet, "/v1/addresses/"+addressID+"?as_of=junk", "").Code)

	require.Equal(t, http.StatusOK, do(r, http.MethodGet, "/v1/addresses?q=high+street&country=GB&postal_code=SW1&type=BILLING&limit=5&cursor=c", "").Code)
	assert.Equal(t, domain.AddressSearchQuery{Text: "high street", Country: "GB", PostalCode: "SW1", Type: domain.AddressBilling, Limit: 5, After: "c"}, p.a().gotQ)
	assert.Equal(t, http.StatusBadRequest, do(r, http.MethodGet, "/v1/addresses?limit=0", "").Code)
	assert.Equal(t, http.StatusBadRequest, do(r, http.MethodGet, "/v1/addresses?limit=abc", "").Code)

	require.Equal(t, http.StatusOK, do(r, http.MethodGet, "/v1/parties/"+partyID+"/addresses?include_ended=true", "").Code)
	assert.True(t, p.a().gotIncl)
	assert.Equal(t, http.StatusBadRequest, do(r, http.MethodPost, "/v1/addresses", "not json").Code)
}

func TestAddressRoutes_TypedErrors(t *testing.T) {
	for _, tc := range []struct {
		err    error
		status int
		code   string
	}{
		{registry.ErrNotFound, http.StatusNotFound, "NOT_FOUND"},
		{registry.ErrVersionConflict, http.StatusConflict, "VERSION_CONFLICT"},
		{registry.ErrVersionRequired, http.StatusBadRequest, "VALIDATION_FAILED"},
		{registry.ErrCreateRequestMismatch, http.StatusConflict, "IDEMPOTENCY_MISMATCH"},
		{registry.ErrSourceUnverified, http.StatusBadRequest, "SOURCE_UNVERIFIED"},
		{registry.ErrServiceUnavailable, http.StatusServiceUnavailable, "DEPENDENCY_UNAVAILABLE"},
		{registry.ErrUnauthorized, http.StatusForbidden, "AUTHORIZATION_DENIED"},
	} {
		p := &partySvc{}
		p.a().err = tc.err
		rec := do(newPartyRouter(p), http.MethodPost, "/v1/addresses", addrBody)
		assert.Equal(t, tc.status, rec.Code, tc.err.Error())
		assert.Contains(t, rec.Body.String(), tc.code, tc.err.Error())
	}
}
