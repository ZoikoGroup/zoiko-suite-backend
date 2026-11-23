package handler_test

// ORG-07 party routes: status codes per outcome, what reaches the service, and the routes' absence when the
// service does not offer parties.

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"zoiko.io/tenant-entity-registry-svc/internal/domain"
	"zoiko.io/tenant-entity-registry-svc/internal/handler"
	"zoiko.io/tenant-entity-registry-svc/internal/registry"
)

const partyID = "0190a000-0000-7000-8000-00000000aaaa"

type partySvc struct {
	*stubSvc
	create   *domain.CreatePartyResult
	fact     *registry.PartyFactOutcome
	identOut *registry.PartyIdentifierOutcome

	gotCreate  domain.CreatePartyRequest
	gotFact    domain.ProposeFactRequest
	gotRole    string
	gotReveal  bool
	gotKnownAt time.Time
	gotQuery   domain.PartySearchQuery
	gotScheme  domain.IdentifierScheme
	gotScope   string
	gotValue   string
}

func (p *partySvc) CreateParty(_ context.Context, req domain.CreatePartyRequest) (*domain.CreatePartyResult, error) {
	p.gotCreate = req
	return p.create, p.err
}

func (p *partySvc) GetParty(_ context.Context, id string, asOf, knownAt time.Time, reveal bool) (*domain.PartyView, error) {
	p.gotID, p.gotAsOf, p.gotKnownAt, p.gotReveal = id, asOf, knownAt, reveal
	return &domain.PartyView{Party: domain.Party{PartyID: id}}, p.err
}

func (p *partySvc) ProposePartyFact(_ context.Context, id string, req domain.ProposeFactRequest) (*registry.PartyFactOutcome, error) {
	p.gotID, p.gotFact = id, req
	return p.fact, p.err
}

func (p *partySvc) AddPartyRole(_ context.Context, id string, req domain.AddPartyRoleRequest) (*registry.PartyRoleOutcome, error) {
	p.gotID, p.gotCorrID = id, req.CorrelationID
	return &registry.PartyRoleOutcome{Role: domain.PartyRole{RoleType: req.RoleType}}, p.err
}

func (p *partySvc) RemovePartyRole(_ context.Context, id, roleID string, req domain.RemovePartyRoleRequest) (*registry.PartyRoleOutcome, error) {
	p.gotID, p.gotRole = id, roleID
	return &registry.PartyRoleOutcome{}, p.err
}

func (p *partySvc) LinkPartyIdentifier(_ context.Context, id string, _ domain.LinkIdentifierRequest) (*registry.PartyIdentifierOutcome, error) {
	p.gotID = id
	return p.identOut, p.err
}

func (p *partySvc) FindPartiesByIdentifier(_ context.Context, scheme domain.IdentifierScheme, scope, value string) ([]domain.PartySummary, error) {
	p.gotScheme, p.gotScope, p.gotValue = scheme, scope, value
	return []domain.PartySummary{{PartyID: partyID}}, p.err
}

func (p *partySvc) SearchParties(_ context.Context, q domain.PartySearchQuery) ([]domain.PartySummary, string, error) {
	p.gotQuery = q
	return []domain.PartySummary{}, "next", p.err
}

func (p *partySvc) GetResolutionCase(_ context.Context, id string) (*domain.ResolutionCase, error) {
	p.gotID = id
	return &domain.ResolutionCase{CaseID: id}, p.err
}

func newPartyRouter(p *partySvc) chi.Router {
	if p.stubSvc == nil {
		p.stubSvc = &stubSvc{}
	}
	r := chi.NewRouter()
	handler.RegisterRoutes(r, handler.New(p, zap.NewNop()))
	return r
}

const createBody = `{"create_request_id":"r1","party_kind":"LEGAL_ENTITY","source_class":"AUTHORITY_REGISTRY"}`

func TestPartyRoutes_AbsentWhenTheServiceOffersNoParties(t *testing.T) {
	rec := do(newRouter(&stubSvc{}), http.MethodPost, "/v1/parties", createBody)
	assert.Equal(t, http.StatusNotFound, rec.Code)
}

func TestCreateParty_StatusPerOutcome(t *testing.T) {
	for name, tc := range map[string]struct {
		res  *domain.CreatePartyResult
		want int
	}{
		"created":            {&domain.CreatePartyResult{Outcome: domain.OutcomeCreated}, http.StatusCreated},
		"replay of a create": {&domain.CreatePartyResult{Outcome: domain.OutcomeCreated, Replayed: true}, http.StatusOK},
		"matched existing":   {&domain.CreatePartyResult{Outcome: domain.OutcomeMatchedExisting}, http.StatusOK},
		"review required":    {&domain.CreatePartyResult{Outcome: domain.OutcomeReviewRequired}, http.StatusAccepted},
		"replayed review":    {&domain.CreatePartyResult{Outcome: domain.OutcomeReviewRequired, Replayed: true}, http.StatusAccepted},
	} {
		p := &partySvc{create: tc.res}
		rec := do(newPartyRouter(p), http.MethodPost, "/v1/parties", createBody)
		assert.Equal(t, tc.want, rec.Code, name)
		assert.Equal(t, "r1", p.gotCreate.CreateRequestID, name)
		assert.Equal(t, corrID, p.gotCreate.CorrelationID, "the envelope correlation id is used when the body names none")
	}
}

func TestCreateParty_RefusalsAreMapped(t *testing.T) {
	for _, tc := range []struct {
		err  error
		want int
		code string
	}{
		{registry.ErrCreateRequestMismatch, http.StatusConflict, "IDEMPOTENCY_MISMATCH"},
		{registry.ErrInvalidInput, http.StatusBadRequest, "VALIDATION_FAILED"},
		{registry.ErrUnauthorized, http.StatusForbidden, "AUTHORIZATION_DENIED"},
		{registry.ErrServiceUnavailable, http.StatusServiceUnavailable, ""},
	} {
		rec := do(newPartyRouter(&partySvc{stubSvc: &stubSvc{err: tc.err}}), http.MethodPost, "/v1/parties", createBody)
		assert.Equal(t, tc.want, rec.Code, tc.err.Error())
		if tc.code != "" {
			assert.Contains(t, rec.Body.String(), tc.code)
		}
	}
	rec := do(newPartyRouter(&partySvc{}), http.MethodPost, "/v1/parties", `not json`)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestGetParty_AsOfKnownAtAndReveal(t *testing.T) {
	p := &partySvc{}
	rec := do(newPartyRouter(p), http.MethodGet,
		"/v1/parties/"+partyID+"?as_of=2025-03-31&known_at=2025-04-01T10:00:00Z&reveal=true", "")
	require.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, partyID, p.gotID)
	assert.True(t, p.gotAsOf.Equal(time.Date(2025, 3, 31, 0, 0, 0, 0, time.UTC)))
	assert.True(t, p.gotKnownAt.Equal(time.Date(2025, 4, 1, 10, 0, 0, 0, time.UTC)))
	assert.True(t, p.gotReveal)

	p = &partySvc{}
	do(newPartyRouter(p), http.MethodGet, "/v1/parties/"+partyID, "")
	assert.False(t, p.gotReveal, "identifiers are masked unless revealing is asked for")
	assert.True(t, p.gotKnownAt.IsZero())

	for _, bad := range []string{"?as_of=last-tuesday", "?known_at=yesterday"} {
		p = &partySvc{}
		assert.Equal(t, http.StatusBadRequest, do(newPartyRouter(p), http.MethodGet, "/v1/parties/"+partyID+bad, "").Code, bad)
		assert.Empty(t, p.gotID, "the service is not called")
	}
	assert.Equal(t, http.StatusNotFound,
		do(newPartyRouter(&partySvc{stubSvc: &stubSvc{err: registry.ErrNotFound}}), http.MethodGet, "/v1/parties/"+partyID, "").Code)
}

func TestProposeFact_ColonRouteAndStatuses(t *testing.T) {
	body := `{"fact_type":"TRADING_NAME","value":"Nova","valid_from":"2025-01-01","source_class":"TENANT_ASSERTED","evidence_ref":"e","expected_master_version":3}`
	p := &partySvc{fact: &registry.PartyFactOutcome{}}
	rec := do(newPartyRouter(p), http.MethodPost, "/v1/parties/"+partyID+"/facts:propose", body)
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	assert.Equal(t, partyID, p.gotID)
	assert.Equal(t, int64(3), p.gotFact.ExpectedMasterVersion)
	assert.Equal(t, domain.FactTradingName, p.gotFact.FactType)

	p = &partySvc{fact: &registry.PartyFactOutcome{NoOp: true}}
	assert.Equal(t, http.StatusOK, do(newPartyRouter(p), http.MethodPost, "/v1/parties/"+partyID+"/facts:propose", body).Code, "restating a fact")

	p = &partySvc{stubSvc: &stubSvc{err: registry.ErrVersionConflict}}
	rec = do(newPartyRouter(p), http.MethodPost, "/v1/parties/"+partyID+"/facts:propose", body)
	assert.Equal(t, http.StatusConflict, rec.Code)
	assert.Contains(t, rec.Body.String(), "VERSION_CONFLICT")
}

func TestPartyRoles_AddAndRemove(t *testing.T) {
	p := &partySvc{}
	rec := do(newPartyRouter(p), http.MethodPost, "/v1/parties/"+partyID+"/roles",
		`{"role_type":"SUPPLIER","valid_from":"2025-01-01","evidence_ref":"c","expected_master_version":1}`)
	assert.Equal(t, http.StatusCreated, rec.Code)
	assert.Equal(t, partyID, p.gotID)

	p = &partySvc{}
	rec = do(newPartyRouter(p), http.MethodPost, "/v1/parties/"+partyID+"/roles/role-9/removal",
		`{"end_date":"2025-06-30","reason":"ended","expected_master_version":2}`)
	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, "role-9", p.gotRole)
}

func TestLinkIdentifier_StatusPerOutcome(t *testing.T) {
	for name, tc := range map[string]struct {
		out  *registry.PartyIdentifierOutcome
		want int
	}{
		"linked":          {&registry.PartyIdentifierOutcome{Identifier: &domain.PartyIdentifier{}}, http.StatusCreated},
		"already linked":  {&registry.PartyIdentifierOutcome{Identifier: &domain.PartyIdentifier{}, NoOp: true}, http.StatusOK},
		"held by another": {&registry.PartyIdentifierOutcome{Case: &domain.ResolutionCase{}}, http.StatusAccepted},
	} {
		rec := do(newPartyRouter(&partySvc{identOut: tc.out}), http.MethodPost, "/v1/parties/"+partyID+"/identifiers", `{}`)
		assert.Equal(t, tc.want, rec.Code, name)
	}
}

func TestFindByIdentifierAndSearch(t *testing.T) {
	p := &partySvc{}
	rec := do(newPartyRouter(p), http.MethodGet, "/v1/identifiers:find?scheme=VAT&scope=GB&value=GB123", "")
	require.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, domain.SchemeVAT, p.gotScheme)
	assert.Equal(t, "GB", p.gotScope)
	assert.Equal(t, "GB123", p.gotValue)
	assert.Contains(t, rec.Body.String(), `"items"`)

	p = &partySvc{}
	rec = do(newPartyRouter(p), http.MethodGet, "/v1/parties?q=nova&kind=LEGAL_ENTITY&role=SUPPLIER&status=ACTIVE&limit=5&cursor=abc", "")
	require.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, domain.PartySearchQuery{Text: "nova", Kind: domain.PartyLegalEntity, Role: domain.RoleSupplier,
		Status: domain.PartyActive, Limit: 5, After: "abc"}, p.gotQuery)
	assert.Contains(t, rec.Body.String(), `"next_cursor":"next"`)
	assert.Contains(t, rec.Body.String(), `"items":[]`)

	for _, bad := range []string{"limit=0", "limit=201", "limit=x"} {
		assert.Equal(t, http.StatusBadRequest, do(newPartyRouter(&partySvc{}), http.MethodGet, "/v1/parties?"+bad, "").Code, bad)
	}
	rec = do(newPartyRouter(&partySvc{stubSvc: &stubSvc{err: errors.New("db down")}}), http.MethodGet, "/v1/resolution-cases/c1", "")
	assert.Equal(t, http.StatusInternalServerError, rec.Code)
	assert.NotContains(t, rec.Body.String(), "db down")
}
