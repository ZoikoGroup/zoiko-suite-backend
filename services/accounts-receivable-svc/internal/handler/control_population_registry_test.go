package handler_test

import (
	"context"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"zoiko.io/accounts-receivable-svc/internal/domain"
	"zoiko.io/accounts-receivable-svc/internal/handler"
)

// Registered providers are dispatched by population name; the legacy store still serves `open-invoices` when nothing is
// registered, and an unknown name is still a 404. Authz (AR_CONTROL_POPULATION_READ), parameter validation and cursor
// handling are shared by every population.
func TestControlPopulation_RegisteredProviders(t *testing.T) {
	var gotQuery domain.ControlPopulationQuery
	handler.RegisterPopulation("cash-receipts", func(_ context.Context, q domain.ControlPopulationQuery) (*domain.ControlPopulationPage, error) {
		gotQuery = q
		return &domain.ControlPopulationPage{Watermark: "w:0"}, nil
	})
	defer handler.RegisterPopulation("cash-receipts", nil)

	s := newStubStore()
	s.popPage = &domain.ControlPopulationPage{Watermark: "legacy:0"}
	base := "/v1/control-populations/"
	q := "?legal_entity_id=" + popEntity

	rec := popRequest(t, base+"cash-receipts"+q+"&period_id=2026-02&limit=7", popTenantHdr, "p1", s, &stubAuthZ{})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.Contains(t, rec.Body.String(), `"watermark":"w:0"`)
	assert.Equal(t, popTenantHdr, gotQuery.TenantID)
	assert.Equal(t, popEntity, gotQuery.LegalEntityID)
	assert.Equal(t, 7, gotQuery.Limit)
	require.NotNil(t, gotQuery.CutOff)
	assert.Equal(t, "2026-02-28", gotQuery.CutOff.Format("2006-01-02"))
	assert.Equal(t, domain.ControlPopulationQuery{}, s.popQuery, "the legacy store is not consulted for a registered population")

	// shared guards apply to registered populations
	assert.Equal(t, http.StatusForbidden, popRequest(t, base+"cash-receipts"+q, popTenantHdr, "p1", s, &stubAuthZ{err: domain.ErrAuthorizationDenied}).Code)
	assert.Equal(t, http.StatusBadRequest, popRequest(t, base+"cash-receipts"+q+"&bogus=1", popTenantHdr, "p1", s, &stubAuthZ{}).Code)
	assert.Equal(t, http.StatusUnauthorized, popRequest(t, base+"cash-receipts"+q, "", "p1", s, &stubAuthZ{}).Code)

	// unregistered names stay 404; `open-invoices` stays the legacy store
	assert.Equal(t, http.StatusNotFound, popRequest(t, base+"credit-debit-notes"+q, popTenantHdr, "p1", s, &stubAuthZ{}).Code)
	rec = popRequest(t, base+"open-invoices"+q, popTenantHdr, "p1", s, &stubAuthZ{})
	require.Equal(t, http.StatusOK, rec.Code)
	assert.Contains(t, rec.Body.String(), `"watermark":"legacy:0"`)

	// a registered `open-invoices` replaces the legacy-only behaviour (the main.go union)
	handler.RegisterPopulation("open-invoices", func(context.Context, domain.ControlPopulationQuery) (*domain.ControlPopulationPage, error) {
		return nil, domain.ErrPopulationTooLarge
	})
	defer handler.RegisterPopulation("open-invoices", nil)
	assert.Equal(t, http.StatusUnprocessableEntity, popRequest(t, base+"open-invoices"+q, popTenantHdr, "p1", s, &stubAuthZ{}).Code)
}
