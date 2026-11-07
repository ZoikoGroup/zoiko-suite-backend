package handler_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"testing"

	"github.com/getkin/kin-openapi/openapi3"
	"github.com/go-chi/chi/v5"

	"zoiko.io/carta-svc/internal/authz"
	apicontract "zoiko.io/identity-envelope/apicontract"
)

// API-2 (ZS-API-001 §12): every refusal is RFC 9457 problem+json with a
// stable code. A writer that names no code gets the status's class
// (apicontract.CodeForStatus), never a blanket internal_error. 409/412/428 are
// not answered by these routes until API-3.
func TestAPI2_ErrorResponsesAreProblemDetails(t *testing.T) {
	send := func(h http.Handler, method, path, body string, edit func(*http.Request)) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		withEnvelope(req)
		req.Header.Set("X-Tenant-ID", "t1")
		req.Header.Set("X-Principal-Id", "principal-test-01")
		if edit != nil {
			edit(req)
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		return w
	}
	noTenant := func(r *http.Request) { r.Header.Del("X-Tenant-ID") }

	for _, tc := range []struct {
		name   string
		w      *httptest.ResponseRecorder
		status int
		code   string
	}{
		{"400 malformed body", send(newRouter(), http.MethodPost, "/v1/carta/evaluate", `{`, nil), 400, "invalid_request"},
		{"401 no tenant scope", send(newRouter(), http.MethodGet, "/v1/carta/assessments?legal_entity_id=entity-test", "", noTenant), 401, "missing_tenant_scope"},
		{"403 not permitted", send(newRouterWithAuthz(&stubAuthz{err: authz.ErrAuthorizationDenied}), http.MethodGet, "/v1/carta/assessments?legal_entity_id=entity-test", "", nil), 403, "authorization_denied"},
		{"404 unknown id", send(newRouter(), http.MethodGet, "/v1/carta/assessments/does-not-exist", "", nil), 404, "not_found"},
		{"503 authorization unavailable", send(newRouterWithAuthz(&stubAuthz{err: authz.ErrAuthzServiceUnavailable}), http.MethodGet, "/v1/carta/assessments?legal_entity_id=entity-test", "", nil), 503, "service_unavailable"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.w.Code != tc.status {
				t.Fatalf("status = %d, want %d: %s", tc.w.Code, tc.status, tc.w.Body.String())
			}
			if ct := tc.w.Header().Get("Content-Type"); ct != apicontract.ContentTypeProblem {
				t.Fatalf("Content-Type = %q, want %s: %s", ct, apicontract.ContentTypeProblem, tc.w.Body.String())
			}
			var p map[string]any
			if err := json.Unmarshal(tc.w.Body.Bytes(), &p); err != nil {
				t.Fatalf("body is not JSON: %s", tc.w.Body.String())
			}
			typ, _ := p["type"].(string)
			if p["code"] != tc.code || p["status"] != float64(tc.status) || !strings.HasPrefix(typ, apicontract.ProblemBase) || p["title"] == nil {
				t.Fatalf("problem = %v, want code %q", p, tc.code)
			}
			if tc.status >= 500 && p["retryable"] != true {
				t.Fatalf("a %d must be retryable: %v", tc.status, p)
			}
		})
	}
}

// An envelope refusal is a problem too, not the vendored middleware's legacy
// application/json body. Strict mode, so a read without the envelope is
// refused whichever write routes this service exempts.
func TestAPI2_EnvelopeRefusalIsAProblem(t *testing.T) {
	t.Setenv("ZS_ENVELOPE_ENFORCEMENT", "strict")
	w := httptest.NewRecorder()
	newRouter().ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/v1/carta/assessments", nil))
	if w.Code < 400 || w.Header().Get("Content-Type") != apicontract.ContentTypeProblem || w.Header().Get("X-Envelope-Contract") != "violated" {
		t.Fatalf("an envelope-less read in strict mode: want a problem refusal, got %d %q %s", w.Code, w.Header().Get("Content-Type"), w.Body.String())
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
// unserved.
func TestOpenAPI_MatchesTheRouter(t *testing.T) {
	documented := map[string]bool{}
	for path, item := range loadOpenAPI(t).Paths.Map() {
		for method := range item.Operations() {
			documented[method+" "+path] = true
		}
	}
	routes, ok := newRouter().(chi.Routes)
	if !ok {
		t.Fatal("handler.NewRouter no longer returns a chi router")
	}
	served := map[string]bool{}
	_ = chi.Walk(routes, func(method, route string, _ http.Handler, _ ...func(http.Handler) http.Handler) error {
		if route != "/" {
			route = strings.TrimSuffix(route, "/")
		}
		served[method+" "+route] = true
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
