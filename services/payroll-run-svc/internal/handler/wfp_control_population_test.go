package handler_test

import (
	"net/http"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"zoiko.io/payroll-run-svc/internal/domain"
	"zoiko.io/payroll-run-svc/internal/handler"
	"zoiko.io/payroll-run-svc/internal/middleware"
)

func TestControlPopulation_ServesTheWorkforcePayrollPopulationsUnderTheSameContract(t *testing.T) {
	for _, population := range []string{"wfp-results", "wfp-journals"} {
		s := newStubStore()
		s.cpPage = &domain.ControlPopulationPage{Records: []domain.ControlRecord{{RecordID: cpRunID, Reference: cpSlipID, Amount: "1.0000"}}}
		rr := cpGet(cpRouter(s, &stubAuthZ{}, true), cpBaseURL+population+okQuery("period_id=2026-09"), "p1")
		require.Equal(t, http.StatusOK, rr.Code, "%s: %s", population, rr.Body.String())
		assert.Equal(t, population, s.cpLast.Population)
		assert.Equal(t, "gross", s.cpLast.Measure)
		assert.Equal(t, "tenant-abc", s.cpLast.TenantID, "the tenant comes from the authenticated context")
		assert.Equal(t, cpEntity, s.cpLast.LegalEntityID)
		require.NotNil(t, s.cpLast.PeriodStart)
	}
}

func TestControlPopulation_NetInstructionsIsANetPayPopulationOnly(t *testing.T) {
	s := newStubStore()
	s.cpPage = &domain.ControlPopulationPage{}
	rr := cpGet(cpRouter(s, &stubAuthZ{}, true), cpBaseURL+"wfp-net-instructions"+okQuery(""), "p1")
	assert.Equal(t, http.StatusBadRequest, rr.Code, "measure=gross is refused for a net-pay population")
	assert.Equal(t, 0, s.cpCalls)
	rr = cpGet(cpRouter(s, &stubAuthZ{}, true), cpBaseURL+"wfp-net-instructions?legal_entity_id="+cpEntity+"&measure=net", "p1")
	assert.Equal(t, http.StatusOK, rr.Code)
	assert.Equal(t, 1, s.cpCalls)
}

func TestControlPopulation_WorkforcePayrollPopulationsAreAuthorizedLikeTheOthers(t *testing.T) {
	s := newStubStore()
	s.cpPage = &domain.ControlPopulationPage{}
	az := &recordingAuthZ{}
	r := chi.NewRouter()
	r.Use(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			next.ServeHTTP(w, req.WithContext(middleware.WithTenant(req.Context(), "tenant-abc")))
		})
	})
	handler.RegisterRoutes(r, handler.New(s, az, nil, nil, nil, testTaxRate, zap.NewNop()))
	rr := cpGet(r, cpBaseURL+"wfp-results"+okQuery(""), "p-9")
	require.Equal(t, http.StatusOK, rr.Code)
	assert.Equal(t, []string{"p-9", cpEntity, "PAYROLL_CONTROL_POPULATION_READ"}, []string{az.principal, az.entity, az.action})
	assert.Equal(t, http.StatusNotFound, cpGet(cpRouter(s, &stubAuthZ{}, true), cpBaseURL+"wfp-workers"+okQuery(""), "p1").Code, "no worker-level population exists")
	assert.Equal(t, http.StatusForbidden, cpGet(cpRouter(s, &stubAuthZ{err: domain.ErrAuthorizationDenied}, true), cpBaseURL+"wfp-journals"+okQuery(""), "p1").Code)
}
