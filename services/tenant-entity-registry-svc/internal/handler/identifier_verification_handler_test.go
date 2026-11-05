package handler_test

// Identifier verification routes. partySvc also implements handler.IdentifierVerificationService, so the OpenAPI parity
// test covers them in both directions.

import (
	"context"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"zoiko.io/tenant-entity-registry-svc/internal/domain"
	"zoiko.io/tenant-entity-registry-svc/internal/registry"
)

const identID = "0190a000-0000-7000-8000-00000000ffff"

type ivState struct {
	out   *registry.IdentifierVerifyOutcome
	err   error
	gotID string
	got   domain.VerifyIdentifierRequest
}

var ivStates = map[*partySvc]*ivState{}

func (p *partySvc) iv() *ivState {
	if ivStates[p] == nil {
		ivStates[p] = &ivState{out: &registry.IdentifierVerifyOutcome{}}
	}
	return ivStates[p]
}

func (p *partySvc) VerifyPartyIdentifier(_ context.Context, id string, req domain.VerifyIdentifierRequest) (*registry.IdentifierVerifyOutcome, error) {
	p.iv().gotID, p.iv().got = id, req
	return p.iv().out, p.iv().err
}
func (p *partySvc) ListIdentifierVerifications(_ context.Context, id string) ([]domain.IdentifierObservation, error) {
	p.iv().gotID = id
	return []domain.IdentifierObservation{}, p.iv().err
}

const verifyBody = `{"expected_master_version":3,"result":"MISMATCH","source_class":"AUTHORITY_REGISTRY","source_ref":"Companies House","source_version":"2026-06",` +
	`"exchange_ref":"xic-1","observed_at":"2026-06-01T10:00:00Z","authority_status":"ISSUED","evidence_ref":"resp-1"}`

func TestIdentifierVerificationRoutes_AbsentWhenTheServiceOffersNone(t *testing.T) {
	assert.Equal(t, http.StatusNotFound, do(newRouter(&stubSvc{}), http.MethodPost, "/v1/identifiers/"+identID+":verify", verifyBody).Code)
}

func TestIdentifierVerificationRoutes_VerifyPassesTheObservationThrough(t *testing.T) {
	p := &partySvc{}
	r := newPartyRouter(p)
	rec := do(r, http.MethodPost, "/v1/identifiers/"+identID+":verify", verifyBody)
	require.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, identID, p.iv().gotID)
	g := p.iv().got
	assert.Equal(t, int64(3), g.ExpectedMasterVersion)
	assert.Equal(t, domain.ObsMismatch, g.Result)
	assert.Equal(t, domain.SourceAuthorityRegistry, g.SourceClass)
	assert.Equal(t, "Companies House", g.SourceRef)
	assert.Equal(t, "xic-1", g.ExchangeRef)
	assert.Equal(t, "2026-06-01T10:00:00Z", g.ObservedAt.UTC().Format("2006-01-02T15:04:05Z"))
	assert.NotEmpty(t, g.CorrelationID, "the request correlation id is filled in")
	assert.Equal(t, http.StatusBadRequest, do(r, http.MethodPost, "/v1/identifiers/"+identID+":verify", "nope").Code)

	require.Equal(t, http.StatusOK, do(r, http.MethodGet, "/v1/identifiers/"+identID+"/verifications", "").Code)
}

func TestIdentifierVerificationRoutes_TypedErrors(t *testing.T) {
	for _, tc := range []struct {
		err    error
		status int
		code   string
	}{
		{registry.ErrNotFound, http.StatusNotFound, "NOT_FOUND"},
		{registry.ErrVersionConflict, http.StatusConflict, "VERSION_CONFLICT"},
		{registry.ErrStateConflict, http.StatusConflict, "INVALID_TRANSITION"},
		{registry.ErrUnauthorized, http.StatusForbidden, "AUTHORIZATION_DENIED"},
		{registry.ErrServiceUnavailable, http.StatusServiceUnavailable, "DEPENDENCY_UNAVAILABLE"},
	} {
		p := &partySvc{}
		p.iv().err = tc.err
		rec := do(newPartyRouter(p), http.MethodPost, "/v1/identifiers/"+identID+":verify", verifyBody)
		assert.Equal(t, tc.status, rec.Code, tc.err.Error())
		assert.Contains(t, rec.Body.String(), tc.code, tc.err.Error())
	}
}
