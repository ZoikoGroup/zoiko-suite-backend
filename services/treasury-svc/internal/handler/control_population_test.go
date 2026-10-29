package handler_test

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"zoiko.io/treasury-svc/internal/domain"
	"zoiko.io/treasury-svc/internal/handler"
	svcmiddleware "zoiko.io/treasury-svc/internal/middleware"
)

// popStub records the queries the control-population endpoints hand to the store.
type popStub struct {
	fxQueries  []domain.FXRateObservationsQuery
	ownQueries []domain.BankAccountOwnershipQuery
	tenants    []string
	page       *domain.ControlPopulationPage
	err        error
}

func (m *mockStore) QueryFXRateObservations(_ context.Context, tenantID string, q domain.FXRateObservationsQuery) (*domain.ControlPopulationPage, error) {
	m.pop.fxQueries = append(m.pop.fxQueries, q)
	m.pop.tenants = append(m.pop.tenants, tenantID)
	return m.popResult()
}

func (m *mockStore) QueryBankAccountOwnership(_ context.Context, tenantID string, q domain.BankAccountOwnershipQuery) (*domain.ControlPopulationPage, error) {
	m.pop.ownQueries = append(m.pop.ownQueries, q)
	m.pop.tenants = append(m.pop.tenants, tenantID)
	return m.popResult()
}

func (m *mockStore) popResult() (*domain.ControlPopulationPage, error) {
	if m.pop.err != nil {
		return nil, m.pop.err
	}
	if m.pop.page != nil {
		return m.pop.page, nil
	}
	return &domain.ControlPopulationPage{Watermark: "tr1:n=0;md5=x"}, nil
}

const (
	cpTenant = "tenant-cp-1"
	cpEntity = "le-cp-1"
	fxPath   = "/v1/control-populations/fx-rate-observations"
	ownPath  = "/v1/control-populations/bank-account-ownership"
)

type cpAuthz struct {
	allowed        bool
	err            error
	action, entity string
}

func (a *cpAuthz) CheckAllowed(_ context.Context, _, legalEntityID, action string) error {
	a.action, a.entity = action, legalEntityID
	if a.err != nil {
		return a.err
	}
	if !a.allowed {
		return domain.ErrAuthorizationDenied
	}
	return nil
}

func cpRouter(s *mockStore, az handler.AuthZClient) chi.Router {
	h := handler.New(s, az, &mockClients{}, &mockTransferClients{}, nil, zap.NewNop())
	r := chi.NewRouter()
	r.Use(svcmiddleware.TenantContext())
	handler.RegisterRoutes(r, h)
	return r
}

func cpGet(r http.Handler, path, tenant, principal string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodGet, path, nil)
	if tenant != "" {
		req.Header.Set("X-Tenant-Id", tenant)
	}
	if principal != "" {
		req.Header.Set("X-Principal-Id", principal)
	}
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	return rec
}

func TestControlPopulation_FXRateObservations_HappyPath(t *testing.T) {
	s := newMockStore()
	s.pop.page = &domain.ControlPopulationPage{
		Records: []domain.ControlPopulationRecord{{RecordID: "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa", Reference: "EUR/USD",
			Amount: "1.08000000", Currency: "XXX", Date: "2026-09-01", Attributes: map[string]string{"currency_pair": "EUR/USD"}}},
		NextRecordID: "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa", Watermark: "tr1:n=1;md5=x",
		DeclaredTotals: domain.ControlDeclaredTotals{RowCount: 1, Totals: map[string]string{"XXX": "1.08000000"}},
	}
	az := &cpAuthz{allowed: true}
	rec := cpGet(cpRouter(s, az), fxPath+"?legal_entity_id="+cpEntity+"&period_id=2026-09&limit=7", cpTenant, "p1")
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	var body struct {
		Records    []domain.ControlPopulationRecord `json:"records"`
		NextCursor string                           `json:"next_cursor"`
		Watermark  string                           `json:"watermark"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	assert.Equal(t, "tr1:n=1;md5=x", body.Watermark)
	assert.Len(t, body.Records, 1)
	assert.Equal(t, base64.RawURLEncoding.EncodeToString([]byte("aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa")), body.NextCursor)
	require.Len(t, s.pop.fxQueries, 1)
	assert.Equal(t, domain.FXRateObservationsQuery{PeriodID: "2026-09", Limit: 7}, s.pop.fxQueries[0])
	assert.Equal(t, []string{cpTenant}, s.pop.tenants)
	assert.Equal(t, "TREASURY_CONTROL_POPULATION_READ", az.action)
	assert.Equal(t, cpEntity, az.entity)

	// the cursor round-trips to the store as the decoded record id
	rec = cpGet(cpRouter(s, az), fxPath+"?legal_entity_id="+cpEntity+"&period_id=2026-09&cursor="+body.NextCursor, cpTenant, "p1")
	require.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa", s.pop.fxQueries[1].AfterRecordID)
}

func TestControlPopulation_BankAccountOwnership_HappyPathAndNoPeriod(t *testing.T) {
	s := newMockStore()
	az := &cpAuthz{allowed: true}
	r := cpRouter(s, az)
	rec := cpGet(r, ownPath+"?legal_entity_id="+cpEntity, cpTenant, "p1")
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	require.Len(t, s.pop.ownQueries, 1)
	assert.Equal(t, domain.BankAccountOwnershipQuery{LegalEntityID: cpEntity, Limit: 1000}, s.pop.ownQueries[0])
	// an empty population encodes as [] and "", never null
	var raw map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &raw))
	assert.Equal(t, "[]", string(raw["records"]))
	assert.Equal(t, `""`, string(raw["next_cursor"]))

	// a lifetime population: period_id is not defined for it
	assert.Equal(t, http.StatusBadRequest, cpGet(r, ownPath+"?legal_entity_id="+cpEntity+"&period_id=2026-09", cpTenant, "p1").Code)
}

func TestControlPopulation_RejectsMalformedRequests(t *testing.T) {
	cases := map[string]struct {
		path, tenant, principal string
		want                    int
	}{
		"no tenant":          {fxPath + "?legal_entity_id=" + cpEntity + "&period_id=2026-09", "", "p1", http.StatusUnauthorized},
		"no principal":       {fxPath + "?legal_entity_id=" + cpEntity + "&period_id=2026-09", cpTenant, "", http.StatusUnauthorized},
		"missing entity":     {fxPath + "?period_id=2026-09", cpTenant, "p1", http.StatusBadRequest},
		"bad entity":         {fxPath + "?legal_entity_id=-bad&period_id=2026-09", cpTenant, "p1", http.StatusBadRequest},
		"fx missing period":  {fxPath + "?legal_entity_id=" + cpEntity, cpTenant, "p1", http.StatusBadRequest},
		"fx bad period":      {fxPath + "?legal_entity_id=" + cpEntity + "&period_id=2026-13", cpTenant, "p1", http.StatusBadRequest},
		"fx date as period":  {fxPath + "?legal_entity_id=" + cpEntity + "&period_id=2026-09-01", cpTenant, "p1", http.StatusBadRequest},
		"unknown param":      {fxPath + "?legal_entity_id=" + cpEntity + "&period_id=2026-09&bogus=1", cpTenant, "p1", http.StatusBadRequest},
		"repeated param":     {fxPath + "?legal_entity_id=" + cpEntity + "&period_id=2026-09&limit=1&limit=2", cpTenant, "p1", http.StatusBadRequest},
		"limit zero":         {ownPath + "?legal_entity_id=" + cpEntity + "&limit=0", cpTenant, "p1", http.StatusBadRequest},
		"limit over max":     {ownPath + "?legal_entity_id=" + cpEntity + "&limit=5001", cpTenant, "p1", http.StatusBadRequest},
		"cursor not a uuid":  {ownPath + "?legal_entity_id=" + cpEntity + "&cursor=" + base64.RawURLEncoding.EncodeToString([]byte("nope")), cpTenant, "p1", http.StatusBadRequest},
		"cursor not base64":  {ownPath + "?legal_entity_id=" + cpEntity + "&cursor=***", cpTenant, "p1", http.StatusBadRequest},
		"unknown population": {"/v1/control-populations/nope?legal_entity_id=" + cpEntity, cpTenant, "p1", http.StatusNotFound},
	}
	for name, c := range cases {
		s := newMockStore()
		got := cpGet(cpRouter(s, &cpAuthz{allowed: true}), c.path, c.tenant, c.principal).Code
		assert.Equal(t, c.want, got, name)
		assert.Empty(t, s.pop.tenants, "%s: the store must not be consulted", name)
	}
}

func TestControlPopulation_AuthorizationAndStoreFailuresFailClosed(t *testing.T) {
	path := ownPath + "?legal_entity_id=" + cpEntity

	s := newMockStore()
	assert.Equal(t, http.StatusForbidden, cpGet(cpRouter(s, &cpAuthz{allowed: false}), path, cpTenant, "p1").Code)
	assert.Empty(t, s.pop.tenants, "the store is never consulted for a denied caller")

	s = newMockStore()
	assert.Equal(t, http.StatusServiceUnavailable, cpGet(cpRouter(s, &cpAuthz{err: domain.ErrAuthzServiceUnavailable}), path, cpTenant, "p1").Code)
	assert.Empty(t, s.pop.tenants)

	s = newMockStore()
	s.pop.err = domain.ErrControlPopulationTooLarge
	assert.Equal(t, http.StatusUnprocessableEntity, cpGet(cpRouter(s, &cpAuthz{allowed: true}), path, cpTenant, "p1").Code)

	s = newMockStore()
	s.pop.err = errors.New("db down")
	assert.Equal(t, http.StatusServiceUnavailable, cpGet(cpRouter(s, &cpAuthz{allowed: true}), path, cpTenant, "p1").Code)
}
