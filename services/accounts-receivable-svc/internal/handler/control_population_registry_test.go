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

// A population that declares its own parameters (aged-unapplied-receipts) requires each of them, rejects period_id and every
// undeclared name, and maps a provider's malformed-parameter error to 400.
func TestControlPopulation_PopulationSpecificParams(t *testing.T) {
	var got domain.ControlPopulationQuery
	handler.RegisterPopulationWithParams("aged-unapplied-receipts", func(_ context.Context, q domain.ControlPopulationQuery) (*domain.ControlPopulationPage, error) {
		got = q
		if q.Params["unapplied_before"] == "bad" {
			return nil, domain.ErrInvalidPopulationParam
		}
		return &domain.ControlPopulationPage{Watermark: "w:0"}, nil
	}, "unapplied_before")
	defer handler.RegisterPopulationWithParams("aged-unapplied-receipts", nil)

	s := newStubStore()
	base := "/v1/control-populations/aged-unapplied-receipts?legal_entity_id=" + popEntity

	rec := popRequest(t, base+"&unapplied_before=2026-09-01", popTenantHdr, "p1", s, &stubAuthZ{})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.Equal(t, "2026-09-01", got.Params["unapplied_before"])

	assert.Equal(t, http.StatusBadRequest, popRequest(t, base, popTenantHdr, "p1", s, &stubAuthZ{}).Code, "missing parameter")
	assert.Equal(t, http.StatusBadRequest, popRequest(t, base+"&unapplied_before=2026-09-01&period_id=2026-09", popTenantHdr, "p1", s, &stubAuthZ{}).Code, "period_id")
	assert.Equal(t, http.StatusBadRequest, popRequest(t, base+"&unapplied_before=2026-09-01&bogus=1", popTenantHdr, "p1", s, &stubAuthZ{}).Code, "unknown")
	assert.Equal(t, http.StatusBadRequest, popRequest(t, base+"&unapplied_before=a&unapplied_before=b", popTenantHdr, "p1", s, &stubAuthZ{}).Code, "repeated")
	assert.Equal(t, http.StatusBadRequest, popRequest(t, base+"&unapplied_before=bad", popTenantHdr, "p1", s, &stubAuthZ{}).Code, "provider rejects value")
}

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
