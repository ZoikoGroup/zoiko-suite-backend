package handler_test

// MDM-01 contact point routes. partySvc also implements handler.ContactPointService, so the OpenAPI parity test covers
// these routes in both directions.

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"zoiko.io/tenant-entity-registry-svc/internal/domain"
	"zoiko.io/tenant-entity-registry-svc/internal/registry"
)

const (
	cpID    = "0190a000-0000-7000-8000-00000000c0c0"
	cpParty = "0190a000-0000-7000-8000-00000000a0a0"
)

type cpState struct {
	out    *registry.ContactPointOutcome
	items  []domain.ContactPointItem
	err    error
	gotCr  domain.CreateContactPointRequest
	gotVer domain.VerifyContactPointRequest
	gotRet domain.RetireContactPointRequest
	gotID  string
	gotQ   domain.ContactPointQuery
}

var cpStates = map[*partySvc]*cpState{}

func (p *partySvc) c() *cpState {
	if cpStates[p] == nil {
		cpStates[p] = &cpState{}
	}
	return cpStates[p]
}

func (p *partySvc) CreateContactPoint(_ context.Context, partyID string, req domain.CreateContactPointRequest) (*registry.ContactPointOutcome, error) {
	p.c().gotID, p.c().gotCr = partyID, req
	return p.c().out, p.c().err
}
func (p *partySvc) VerifyContactPoint(_ context.Context, id string, req domain.VerifyContactPointRequest) (*registry.ContactPointOutcome, error) {
	p.c().gotID, p.c().gotVer = id, req
	return p.c().out, p.c().err
}
func (p *partySvc) RetireContactPoint(_ context.Context, id string, req domain.RetireContactPointRequest) (*registry.ContactPointOutcome, error) {
	p.c().gotID, p.c().gotRet = id, req
	return p.c().out, p.c().err
}
func (p *partySvc) ListPartyContactPoints(_ context.Context, partyID string, q domain.ContactPointQuery) ([]domain.ContactPointItem, error) {
	p.c().gotID, p.c().gotQ = partyID, q
	return p.c().items, p.c().err
}

const cpBody = `{"channel":"EMAIL","value":"ops@example.com","source_class":"TENANT_ASSERTED"}`

func TestContactPointRoutes_AbsentWhenTheServiceOffersNone(t *testing.T) {
	assert.Equal(t, http.StatusNotFound, do(newRouter(&stubSvc{}), http.MethodPost, "/v1/parties/"+cpParty+"/contact-points", cpBody).Code)
}

func TestContactPointRoutes_Commands(t *testing.T) {
	p := &partySvc{}
	p.c().out = &registry.ContactPointOutcome{}
	r := newPartyRouter(p)

	rec := do(r, http.MethodPost, "/v1/parties/"+cpParty+"/contact-points", cpBody)
	assert.Equal(t, http.StatusCreated, rec.Code)
	assert.Equal(t, cpParty, p.c().gotID)
	assert.Equal(t, "ops@example.com", p.c().gotCr.Value)
	assert.NotEmpty(t, p.c().gotCr.CorrelationID)
	assert.Equal(t, http.StatusBadRequest, do(r, http.MethodPost, "/v1/parties/"+cpParty+"/contact-points", "nope").Code)

	rec = do(r, http.MethodPost, "/v1/party-contact-points/"+cpID+":verify", `{"expected_version":2,"evidence_ref":"mail-roundtrip"}`)
	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, cpID, p.c().gotID)
	assert.Equal(t, int64(2), p.c().gotVer.ExpectedVersion)
	assert.Equal(t, "mail-roundtrip", p.c().gotVer.EvidenceRef)

	rec = do(r, http.MethodPost, "/v1/party-contact-points/"+cpID+":retire", `{"expected_version":3,"reason":"bounced"}`)
	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, "bounced", p.c().gotRet.Reason)
	assert.Equal(t, http.StatusBadRequest, do(r, http.MethodPost, "/v1/party-contact-points/"+cpID+":retire", "nope").Code)
}

func TestContactPointRoutes_ReadReturnsTheDocumentedShape(t *testing.T) {
	from := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	ver := from.Add(time.Hour)
	p := &partySvc{}
	p.c().items = []domain.ContactPointItem{{ContactPointID: cpID, PartyID: cpParty, Channel: domain.ContactEmail, Value: "ops@example.com",
		Purpose: domain.ContactBilling, Status: domain.ContactVerified, ValidFrom: from, SourceClass: domain.SourceTenantAsserted,
		VerifiedAt: &ver, EvidenceRef: "ev", RecordVersion: 2}}
	r := newPartyRouter(p)

	rec := do(r, http.MethodGet, "/v1/parties/"+cpParty+"/contact-points?channel=EMAIL&status=VERIFIED&as_of=2026-06-01T00:00:00Z", "")
	require.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, domain.ContactEmail, p.c().gotQ.Channel)
	assert.Equal(t, domain.ContactVerified, p.c().gotQ.Status)
	assert.Equal(t, time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC), p.c().gotQ.AsOf)

	var body struct {
		ContactPoints []map[string]any `json:"contact_points"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	require.Len(t, body.ContactPoints, 1)
	keys := map[string]bool{}
	for k := range body.ContactPoints[0] {
		keys[k] = true
	}
	assert.Equal(t, map[string]bool{"contact_point_id": true, "party_id": true, "channel": true, "value": true, "purpose": true, "status": true,
		"valid_from": true, "valid_to": true, "source_class": true, "verified_at": true, "evidence_ref": true, "record_version": true}, keys)
	assert.Nil(t, body.ContactPoints[0]["valid_to"])

	// No filter means every state, and as_of defaults to now.
	before := time.Now().UTC()
	require.Equal(t, http.StatusOK, do(r, http.MethodGet, "/v1/parties/"+cpParty+"/contact-points", "").Code)
	assert.Empty(t, p.c().gotQ.Status)
	assert.False(t, p.c().gotQ.AsOf.Before(before.Add(-time.Second)))
	assert.Equal(t, http.StatusBadRequest, do(r, http.MethodGet, "/v1/parties/"+cpParty+"/contact-points?as_of=junk", "").Code)

	// An empty result is an empty list.
	p2 := &partySvc{}
	rec = do(newPartyRouter(p2), http.MethodGet, "/v1/parties/"+cpParty+"/contact-points", "")
	require.Equal(t, http.StatusOK, rec.Code)
	assert.Contains(t, rec.Body.String(), `"contact_points"`)
}

func TestContactPointRoutes_TypedErrors(t *testing.T) {
	for _, tc := range []struct {
		err    error
		status int
		code   string
	}{
		{registry.ErrNotFound, http.StatusNotFound, "NOT_FOUND"},
		{registry.ErrUnauthorized, http.StatusForbidden, "AUTHORIZATION_DENIED"},
		{registry.ErrVersionConflict, http.StatusConflict, "VERSION_CONFLICT"},
		{registry.ErrStateConflict, http.StatusConflict, "INVALID_TRANSITION"},
		{registry.ErrSelfApproval, http.StatusForbidden, "SOD_DENIED"},
		{registry.ErrConflict, http.StatusConflict, "DUPLICATE_CANDIDATE"},
		{registry.ErrInvalidInput, http.StatusUnprocessableEntity, "VALIDATION_FAILED"},
	} {
		for _, call := range []struct{ method, path, body string }{
			{http.MethodPost, "/v1/party-contact-points/" + cpID + ":verify", `{"expected_version":1,"evidence_ref":"e"}`},
			{http.MethodPost, "/v1/party-contact-points/" + cpID + ":retire", `{"expected_version":1,"reason":"r"}`},
			{http.MethodPost, "/v1/parties/" + cpParty + "/contact-points", cpBody},
			{http.MethodGet, "/v1/parties/" + cpParty + "/contact-points", ""},
		} {
			p := &partySvc{}
			p.c().err = tc.err
			rec := do(newPartyRouter(p), call.method, call.path, call.body)
			if tc.err == registry.ErrInvalidInput {
				assert.Contains(t, []int{http.StatusBadRequest, http.StatusUnprocessableEntity}, rec.Code, tc.err.Error())
			} else {
				assert.Equal(t, tc.status, rec.Code, tc.err.Error()+" "+call.path)
			}
			assert.Contains(t, rec.Body.String(), tc.code, tc.err.Error())
		}
	}
}
