package handler_test

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"zoiko.io/payroll-run-svc/internal/domain"
	"zoiko.io/payroll-run-svc/internal/handler"
	"zoiko.io/payroll-run-svc/internal/middleware"
)

func (s *stubStore) ControlPopulation(_ context.Context, q domain.ControlPopulationQuery) (*domain.ControlPopulationPage, error) {
	s.cpCalls++
	s.cpLast = q
	if s.cpErr != nil {
		return nil, s.cpErr
	}
	return s.cpPage, nil
}

const (
	cpEntity  = "11111111-1111-4111-8111-111111111111"
	cpSlipID  = "22222222-2222-4222-8222-222222222222"
	cpRunID   = "33333333-3333-4333-8333-333333333333"
	cpBaseURL = "/v1/control-populations/"
)

func cpRouter(s *stubStore, authz *stubAuthZ, withTenant bool) chi.Router {
	r := chi.NewRouter()
	if withTenant {
		r.Use(func(next http.Handler) http.Handler {
			return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
				next.ServeHTTP(w, req.WithContext(middleware.WithTenant(req.Context(), "tenant-abc")))
			})
		})
	}
	h := handler.New(s, &stubPublisher{}, authz, nil, nil, nil, testTaxRate, zap.NewNop())
	handler.RegisterRoutes(r, h)
	return r
}

func cpGet(r chi.Router, path, principal string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodGet, path, nil)
	if principal != "" {
		req.Header.Set("X-Principal-Id", principal)
	}
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)
	return rr
}

func okQuery(extra string) string {
	q := "?legal_entity_id=" + cpEntity + "&measure=gross"
	if extra != "" {
		q += "&" + extra
	}
	return q
}

func TestControlPopulation_Unauthenticated(t *testing.T) {
	s := newStubStore()
	// No tenant in context.
	rr := cpGet(cpRouter(s, &stubAuthZ{}, false), cpBaseURL+"pay-slips"+okQuery(""), "p1")
	assert.Equal(t, http.StatusUnauthorized, rr.Code)
	// No principal.
	rr = cpGet(cpRouter(s, &stubAuthZ{}, true), cpBaseURL+"pay-slips"+okQuery(""), "")
	assert.Equal(t, http.StatusUnauthorized, rr.Code)
	assert.Equal(t, 0, s.cpCalls)
}

func TestControlPopulation_BadInputs(t *testing.T) {
	badCursor := base64.RawURLEncoding.EncodeToString([]byte("not-a-uuid"))
	cases := []struct {
		name, path string
	}{
		{"missing legal_entity_id", "pay-slips?measure=gross"},
		{"empty legal_entity_id", "pay-slips?legal_entity_id=&measure=gross"},
		{"invalid legal_entity_id", "pay-slips?legal_entity_id=%27%3B--&measure=gross"},
		{"bad period", "pay-slips" + okQuery("period_id=2026-13")},
		{"bad period format", "pay-slips" + okQuery("period_id=2026-9")},
		{"bad period date", "pay-slips" + okQuery("period_id=2026-09-01")},
		{"unknown param", "pay-slips" + okQuery("status=COMPLETED")},
		{"repeated param", "pay-slips" + okQuery("limit=5&limit=6")},
		{"repeated legal_entity_id", "pay-slips" + okQuery("legal_entity_id="+cpEntity)},
		{"missing measure", "pay-slips?legal_entity_id=" + cpEntity},
		{"invalid measure", "pay-slips?legal_entity_id=" + cpEntity + "&measure=tax"},
		{"empty measure", "pay-slips?legal_entity_id=" + cpEntity + "&measure="},
		{"limit not int", "payroll-runs" + okQuery("limit=abc")},
		{"limit zero", "payroll-runs" + okQuery("limit=0")},
		{"limit negative", "payroll-runs" + okQuery("limit=-1")},
		{"limit too big", "payroll-runs" + okQuery("limit=5001")},
		{"cursor not base64", "payroll-runs" + okQuery("cursor=***")},
		{"cursor not a uuid", "payroll-runs" + okQuery("cursor="+badCursor)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := newStubStore()
			rr := cpGet(cpRouter(s, &stubAuthZ{}, true), cpBaseURL+tc.path, "p1")
			assert.Equal(t, http.StatusBadRequest, rr.Code, rr.Body.String())
			assert.Equal(t, 0, s.cpCalls, "store must not be called on a bad request")
		})
	}
}

func TestControlPopulation_UnknownPopulation404(t *testing.T) {
	s := newStubStore()
	rr := cpGet(cpRouter(s, &stubAuthZ{}, true), cpBaseURL+"employees"+okQuery(""), "p1")
	assert.Equal(t, http.StatusNotFound, rr.Code)
	assert.Equal(t, 0, s.cpCalls)
}

func TestControlPopulation_AuthzDeniedAndUnavailable(t *testing.T) {
	s := newStubStore()
	rr := cpGet(cpRouter(s, &stubAuthZ{err: domain.ErrAuthorizationDenied}, true), cpBaseURL+"pay-slips"+okQuery(""), "p1")
	assert.Equal(t, http.StatusForbidden, rr.Code)
	rr = cpGet(cpRouter(s, &stubAuthZ{err: domain.ErrAuthzServiceUnavailable}, true), cpBaseURL+"payroll-runs"+okQuery(""), "p1")
	assert.Equal(t, http.StatusServiceUnavailable, rr.Code)
	assert.Equal(t, 0, s.cpCalls, "store must not be called when authorization fails")
}

type recordingAuthZ struct{ principal, entity, action string }

func (a *recordingAuthZ) CheckAllowed(_ context.Context, p, e, act string) error {
	a.principal, a.entity, a.action = p, e, act
	return nil
}

func TestControlPopulation_AuthorizesAgainstEntityWithReadAction(t *testing.T) {
	s := newStubStore()
	s.cpPage = &domain.ControlPopulationPage{}
	az := &recordingAuthZ{}
	r := chi.NewRouter()
	r.Use(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			next.ServeHTTP(w, req.WithContext(middleware.WithTenant(req.Context(), "tenant-abc")))
		})
	})
	handler.RegisterRoutes(r, handler.New(s, &stubPublisher{}, az, nil, nil, nil, testTaxRate, zap.NewNop()))
	rr := cpGet(r, cpBaseURL+"pay-slips"+okQuery("period_id=2026-09&limit=7"), "principal-9")
	require.Equal(t, http.StatusOK, rr.Code)
	assert.Equal(t, "principal-9", az.principal)
	assert.Equal(t, cpEntity, az.entity)
	assert.Equal(t, "PAYROLL_CONTROL_POPULATION_READ", az.action)
	assert.Equal(t, "tenant-abc", s.cpLast.TenantID)
	assert.Equal(t, "gross", s.cpLast.Measure)
	assert.Equal(t, 7, s.cpLast.Limit)
	require.NotNil(t, s.cpLast.PeriodStart)
	assert.Equal(t, "2026-09-01", s.cpLast.PeriodStart.Format("2006-01-02"))
}

func TestControlPopulation_TooLarge422(t *testing.T) {
	s := newStubStore()
	s.cpErr = domain.ErrPopulationTooLarge
	rr := cpGet(cpRouter(s, &stubAuthZ{}, true), cpBaseURL+"pay-slips"+okQuery(""), "p1")
	assert.Equal(t, http.StatusUnprocessableEntity, rr.Code)
}

func TestControlPopulation_StoreFailure503(t *testing.T) {
	s := newStubStore()
	s.cpErr = domain.ErrStoreUnavailable
	rr := cpGet(cpRouter(s, &stubAuthZ{}, true), cpBaseURL+"pay-slips"+okQuery(""), "p1")
	assert.Equal(t, http.StatusServiceUnavailable, rr.Code)
}

func TestControlPopulation_PaySlipsHappyPath_NoNames(t *testing.T) {
	s := newStubStore()
	s.cpPage = &domain.ControlPopulationPage{
		Records: []domain.ControlRecord{{
			RecordID: cpSlipID, Reference: cpRunID, Amount: "1000.00", Currency: "USD", Date: "2026-09-25",
			Attributes: map[string]string{
				"run_id": cpRunID, "run_status": "COMPLETED", "is_shadow_run": "false", "employee_number": "E-001",
				"gross_pay": "1000.00", "tax_withheld": "200.00", "benefits_deductions": "50.00", "net_pay": "750.00",
			},
		}},
		NextCursor:     cpSlipID,
		Watermark:      "1:abc",
		DeclaredTotals: domain.DeclaredTotals{RowCount: 3, Totals: map[string]string{"USD": "3000.00"}},
	}
	rr := cpGet(cpRouter(s, &stubAuthZ{}, true), cpBaseURL+"pay-slips"+okQuery("period_id=2026-09"), "p1")
	require.Equal(t, http.StatusOK, rr.Code)

	var body map[string]any
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &body))
	assert.ElementsMatch(t, []string{"records", "next_cursor", "watermark", "declared_totals"}, keys(body))
	assert.Equal(t, base64.RawURLEncoding.EncodeToString([]byte(cpSlipID)), body["next_cursor"])
	assert.Equal(t, "1:abc", body["watermark"])

	rec := body["records"].([]any)[0].(map[string]any)
	assert.ElementsMatch(t, []string{"record_id", "reference", "amount", "currency", "date", "attributes"}, keys(rec))
	assert.IsType(t, "", rec["amount"], "amount must be a decimal STRING")
	attrs := rec["attributes"].(map[string]any)
	assert.ElementsMatch(t, []string{"run_id", "run_status", "is_shadow_run", "employee_number",
		"gross_pay", "tax_withheld", "benefits_deductions", "net_pay"}, keys(attrs))
	dt := body["declared_totals"].(map[string]any)
	assert.EqualValues(t, 3, dt["row_count"])
	assert.Equal(t, "3000.00", dt["totals"].(map[string]any)["USD"])

	assertNoNameFields(t, rr.Body.String())
}

func TestControlPopulation_PayrollRunsHappyPath_NoNames_EmptyLastPage(t *testing.T) {
	s := newStubStore()
	s.cpPage = &domain.ControlPopulationPage{
		Records: []domain.ControlRecord{{
			RecordID: cpRunID, Reference: cpRunID, Amount: "5000.00", Currency: "XXX", Date: "2026-09-25",
			Attributes: map[string]string{"status": "CALCULATED", "is_shadow_run": "true", "employee_count": "0",
				"snapshot_hash": "", "currency_note": "no_slips"},
		}},
		Watermark:      "1:def",
		DeclaredTotals: domain.DeclaredTotals{RowCount: 1, Totals: map[string]string{"XXX": "5000.00"}},
	}
	rr := cpGet(cpRouter(s, &stubAuthZ{}, true), cpBaseURL+"payroll-runs"+okQuery(""), "p1")
	require.Equal(t, http.StatusOK, rr.Code)
	var body map[string]any
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &body))
	assert.Equal(t, "", body["next_cursor"])
	assertNoNameFields(t, rr.Body.String())
	assert.Equal(t, domain.PopulationPayrollRuns, s.cpLast.Population)
}

func TestControlPopulation_EmptyPopulationRendersEmptyCollections(t *testing.T) {
	s := newStubStore()
	s.cpPage = &domain.ControlPopulationPage{Watermark: "0:x"}
	rr := cpGet(cpRouter(s, &stubAuthZ{}, true), cpBaseURL+"pay-slips"+okQuery(""), "p1")
	require.Equal(t, http.StatusOK, rr.Code)
	assert.Contains(t, rr.Body.String(), `"records":[]`)
	assert.Contains(t, rr.Body.String(), `"totals":{}`)
}

// The payload must not be able to carry any employee name, whatever the store
// returns for the whitelisted keys.
func assertNoNameFields(t *testing.T, raw string) {
	t.Helper()
	low := strings.ToLower(raw)
	for _, banned := range []string{"employee_name", "first_name", "last_name", "full_name", `"name"`, "email", "phone", "address", "iban", "bank_account"} {
		assert.NotContains(t, low, banned)
	}
}

func keys(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
