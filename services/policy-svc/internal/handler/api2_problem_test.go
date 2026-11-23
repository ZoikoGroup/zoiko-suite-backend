package handler_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"testing"

	"github.com/getkin/kin-openapi/openapi3"
	"github.com/go-chi/chi/v5"
	"go.uber.org/zap"

	apicontract "zoiko.io/identity-envelope/apicontract"
	"zoiko.io/policy-svc/internal/domain"
	svcenvelope "zoiko.io/policy-svc/internal/envelope"
	"zoiko.io/policy-svc/internal/handler"
	svcmiddleware "zoiko.io/policy-svc/internal/middleware"
)

// API-2 (ZS-API-001 §12): every refusal is RFC 9457 problem+json. code is
// the §16 class; reason_code keeps the specific refusal a detail message
// would otherwise hide. A stale version is 409 CONCURRENCY_CONFLICT until
// API-3 moves it to 412.
func TestAPI2_ErrorResponsesAreProblemDetails(t *testing.T) {
	unavailable := lifecycleStore(nil)
	unavailable.transitionErr = domain.ErrStoreUnavailable
	stale := lifecycleStore(nil)
	stale.transitionErr = domain.ErrConcurrencyConflict
	otherPolicy := &domain.PolicyVersion{PolicyVersionID: versionID, PolicyID: otherPolicyID, VersionStatus: domain.StatusApproved}
	missingField := `{"period_start":"2026-01-01T00:00:00Z"}`

	for _, tc := range []struct {
		name   string
		w      *httptest.ResponseRecorder
		status int
		code   string
		reason string
	}{
		{"400 control-test execution missing a field", newRig(nil, nil, nil).do(http.MethodPost, "/v1/control-test-definitions/"+versionID+"/executions", missingField), 400, "INVALID_REQUEST", "missing_field"},
		{"401 no principal", newRig(lifecycleStore(nil), nil, nil).do(http.MethodPost, lifecyclePath("activate"), "", ifm, noPrincipal), 401, "CONTEXT_UNRESOLVED", ""},
		{"403 denied", newRig(lifecycleStore(nil), nil, &stubAuthz{err: domain.ErrAuthorizationDenied}).do(http.MethodPost, lifecyclePath("activate"), "", ifm), 403, "AUTHORIZATION_DENIED", ""},
		{"404 version of another policy", newRig(lifecycleStore(otherPolicy), nil, nil).do(http.MethodPost, lifecyclePath("activate"), "", ifm), 404, "NOT_FOUND", "policy_version_not_found"},
		{"409 stale version", newRig(stale, nil, nil).do(http.MethodPost, lifecyclePath("activate"), "", ifm), 409, "CONCURRENCY_CONFLICT", "concurrency_conflict"},
		{"428 no If-Match header", newRig(lifecycleStore(nil), nil, nil).do(http.MethodPost, lifecyclePath("activate"), ""), 428, "precondition_required", ""},
		{"503 store unavailable", newRig(unavailable, nil, nil).do(http.MethodPost, lifecyclePath("activate"), "", ifm), 503, "SERVICE_UNAVAILABLE", "store_unavailable"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if ct := tc.w.Header().Get("Content-Type"); ct != apicontract.ContentTypeProblem {
				t.Fatalf("Content-Type = %q, want %s: %s", ct, apicontract.ContentTypeProblem, tc.w.Body.String())
			}
			p := expect(t, tc.w, tc.status, tc.code)
			typ, _ := p["type"].(string)
			if p["status"] != float64(tc.status) || !strings.HasPrefix(typ, apicontract.ProblemBase) || p["title"] == nil {
				t.Fatalf("problem = %v", p)
			}
			if tc.reason != "" && p["reason_code"] != tc.reason {
				t.Fatalf("reason_code = %v, want %s", p["reason_code"], tc.reason)
			}
			if _, legacy := p["error"]; legacy {
				t.Fatalf("legacy error member survived: %v", p)
			}
		})
	}
}

// A write-strict envelope refusal is a problem too, not the vendored
// middleware's legacy application/json body.
func TestAPI2_EnvelopeRefusalIsAProblem(t *testing.T) {
	r := chi.NewRouter()
	r.Use(svcmiddleware.TenantContext())
	r.Use(apicontract.EnvelopeRefusals)
	r.Use(svcenvelope.MiddlewareWithMode(svcenvelope.ServicePolicy(), svcenvelope.ModeWriteStrict, nil))
	handler.RegisterRoutes(r, handler.New(&stubStore{}, &stubCandidates{}, &stubAuthz{}, handler.Options{PlatformScopeID: testAuthzScopeID}, zap.NewNop()))
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/v1/policies", strings.NewReader(`{}`)))
	if w.Code < 400 || w.Header().Get("Content-Type") != apicontract.ContentTypeProblem {
		t.Fatalf("an envelope-less write: want a problem refusal, got %d %q %s", w.Code, w.Header().Get("Content-Type"), w.Body.String())
	}
}

func loadOpenAPI(t *testing.T) *openapi3.T {
	t.Helper()
	doc, err := openapi3.NewLoader().LoadFromFile("../../openapi.yaml")
	if err != nil {
		t.Fatalf("openapi.yaml does not load: %v", err)
	}
	if err := doc.Validate(context.Background()); err != nil {
		t.Fatalf("openapi.yaml is not a valid OpenAPI document: %v", err)
	}
	return doc
}

// Every route the router serves is documented, and nothing documented is
// unserved (the probes are mounted in cmd/server).
func TestOpenAPI_MatchesTheRouter(t *testing.T) {
	documented := map[string]bool{}
	for path, item := range loadOpenAPI(t).Paths.Map() {
		for method := range item.Operations() {
			documented[method+" "+path] = true
		}
	}
	r := chi.NewRouter()
	h := handler.New(&stubStore{}, &stubCandidates{}, &stubAuthz{}, handler.Options{PlatformScopeID: testAuthzScopeID}, zap.NewNop())
	handler.RegisterRoutes(r, h)
	handler.RegisterControlTestRoutes(r, h)
	served := map[string]bool{"GET /healthz": true, "GET /readyz": true}
	_ = chi.Walk(r, func(method, route string, _ http.Handler, _ ...func(http.Handler) http.Handler) error {
		served[method+" "+strings.TrimSuffix(route, "/")] = true
		return nil
	})
	var missing, extra []string
	for k := range served {
		if !documented[k] {
			missing = append(missing, k)
		}
	}
	for k := range documented {
		if !served[k] {
			extra = append(extra, k)
		}
	}
	sort.Strings(missing)
	sort.Strings(extra)
	if len(missing) > 0 || len(extra) > 0 {
		t.Fatalf("openapi.yaml out of step with the router - undocumented: %v, unserved: %v", missing, extra)
	}
}

// Errors are problem documents (ZS-API-001 §12); requests and successes are not.
func TestOpenAPI_ProblemJSONOnlyOnErrors(t *testing.T) {
	for path, item := range loadOpenAPI(t).Paths.Map() {
		for method, op := range item.Operations() {
			if op.RequestBody != nil && op.RequestBody.Value != nil &&
				op.RequestBody.Value.Content.Get("application/problem+json") != nil {
				t.Errorf("%s %s: a request body is not a problem", method, path)
			}
			for code, resp := range op.Responses.Map() {
				mt := resp.Value.Content.Get("application/problem+json")
				switch {
				case path == "/healthz" || path == "/readyz":
				case code[0] == '4' || code[0] == '5':
					if mt == nil || mt.Schema == nil || mt.Schema.Ref != "#/components/schemas/Problem" {
						t.Errorf("%s %s %s must be application/problem+json with the Problem schema", method, path, code)
					}
				case mt != nil:
					t.Errorf("%s %s %s is a success, not a problem", method, path, code)
				}
			}
		}
	}
}
