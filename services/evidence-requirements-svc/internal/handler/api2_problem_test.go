package handler_test

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"zoiko.io/evidence-requirements-svc/internal/domain"
	svcenvelope "zoiko.io/evidence-requirements-svc/internal/envelope"
	"zoiko.io/evidence-requirements-svc/internal/handler"
	svcmiddleware "zoiko.io/evidence-requirements-svc/internal/middleware"
	apicontract "zoiko.io/identity-envelope/apicontract"
)

// API-2 (ZS-API-001 §12): every refusal is RFC 9457 problem+json with a
// stable code. 412/428 are not answered here until API-3.
func TestAPI2_ErrorResponsesAreProblemDetails(t *testing.T) {
	plain := func() http.Handler { return newRouter(&stubStore{}, &stubPublisher{}, &stubAuthz{}, &stubDocs{}) }
	noCorrelation := evaluateBody()
	noCorrelation.CorrelationID = ""
	denied := completionRouter(&stubStore{evalCreated: true},
		&recordingAuthz{deny: map[string]error{"EVIDENCE_EVALUATE": domain.ErrAuthorizationDenied}},
		&stubDocs{}, stubRegistry{}, handler.GateEnforce, handler.GateEnforce)
	replayed := &stubStore{evalCreated: false, evaluation: &domain.EvidenceEvaluation{
		EvaluationID: "eval-original", TenantID: testTenant, LegalEntityID: testEntity,
		DomainCode: "FINANCE", ActionType: "PAYMENT_RELEASE", Outcome: domain.OutcomeMissing,
		EvaluatedForPrincipalID: testPrincipal,
	}}

	for _, tc := range []struct {
		name   string
		rec    *httptest.ResponseRecorder
		status int
		code   string
	}{
		{"400 missing correlation id", do(t, plain(), http.MethodPost, "/v1/evidence/evaluate", noCorrelation, testTenant, testPrincipal), 400, ""},
		{"401 missing tenant", do(t, plain(), http.MethodPost, "/v1/evidence/evaluate", evaluateBody(), "", testPrincipal), 401, "missing_tenant"},
		{"403 not permitted at the entity", do(t, denied, http.MethodPost, "/v1/evidence/evaluate", evaluateBody(), testTenant, testPrincipal), 403, ""},
		{"404 unknown evaluation", do(t, plain(), http.MethodGet, "/v1/evidence/evaluations/eval-nope", nil, testTenant, testPrincipal), 404, ""},
		{"409 idempotency mismatch", do(t, newRouter(replayed, &stubPublisher{}, &stubAuthz{}, &stubDocs{}), http.MethodPost, "/v1/evidence/evaluate", evaluateBody(), testTenant, testPrincipal), 409, "idempotency_mismatch"},
		{"503 store unavailable", do(t, newRouter(&stubStore{effectErr: domain.ErrStoreUnavailable}, &stubPublisher{}, &stubAuthz{}, &stubDocs{}), http.MethodPost, "/v1/evidence/evaluate", evaluateBody(), testTenant, testPrincipal), 503, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, tc.status, tc.rec.Code, tc.rec.Body.String())
			p := problemOf(t, tc.rec)
			require.Equal(t, float64(tc.status), p["status"])
			require.True(t, strings.HasPrefix(p["type"].(string), apicontract.ProblemBase), p["type"])
			require.NotEmpty(t, p["code"])
			if tc.code != "" {
				require.Equal(t, tc.code, p["code"])
			}
			require.NotContains(t, p, "error", "the legacy error member must not survive")
		})
	}
}

// A write-strict envelope refusal is a problem too, not the vendored
// middleware's legacy {"error",...} body.
func TestAPI2_EnvelopeRefusalIsAProblem(t *testing.T) {
	r := chi.NewRouter()
	r.Use(svcmiddleware.TenantContext())
	r.Use(apicontract.EnvelopeRefusals)
	r.Use(svcenvelope.MiddlewareWithMode(svcenvelope.ServicePolicy(), svcenvelope.ModeWriteStrict, nil))
	handler.RegisterRoutes(r, handler.New(&stubStore{}, &stubAuthz{}, &stubDocs{}, zap.NewNop()))

	req := httptest.NewRequest(http.MethodPost, "/v1/evidence/evaluate", strings.NewReader(`{}`))
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	require.GreaterOrEqual(t, rec.Code, 400, rec.Body.String())
	p := problemOf(t, rec)
	require.Contains(t, []any{"identity_missing", "envelope_incomplete"}, p["code"])
}
