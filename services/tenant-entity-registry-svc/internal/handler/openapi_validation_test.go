package handler_test

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/getkin/kin-openapi/openapi3"
	"github.com/getkin/kin-openapi/openapi3filter"
	"github.com/getkin/kin-openapi/routers/gorillamux"
)

// ORG §9.2 DoD gate 1: "OpenAPI/AsyncAPI/schema contracts pass compatibility
// gates and generated client/server validation".
//
// The route-parity tests prove every served path is declared. These go
// further, with a real OpenAPI implementation (kin-openapi):
//
//  1. openapi.yaml is a valid OpenAPI document — every $ref resolves and every
//     schema is well-formed, which a generated client depends on;
//  2. every route in the router's own table, driven through the real router,
//     answers with a status the document declares for it (bodies come from
//     test stubs with zero-valued enums, so they are validated against the
//     LIVE service instead — TestOpenAPI_LiveResponsesMatchTheSchemas);
//  3. with TER_LIVE_URL set, real responses from a running service are
//     validated against the declared schemas, bodies included.

func loadValidatedSpec(t *testing.T) *openapi3.T {
	t.Helper()
	_, thisFile, _, _ := runtime.Caller(0)
	loader := openapi3.NewLoader()
	doc, err := loader.LoadFromFile(filepath.Join(filepath.Dir(thisFile), "../../openapi.yaml"))
	if err != nil {
		t.Fatalf("openapi.yaml does not load: %v", err)
	}
	if err := doc.Validate(context.Background()); err != nil {
		t.Fatalf("openapi.yaml is not a valid OpenAPI document: %v", err)
	}
	return doc
}

func TestOpenAPI_DocumentIsValid(t *testing.T) {
	loadValidatedSpec(t)
}

func TestOpenAPI_EveryRouteAnswersAsDeclared(t *testing.T) {
	doc := loadValidatedSpec(t)
	doc.Servers = openapi3.Servers{{URL: "http://registry.test"}}
	router, err := gorillamux.NewRouter(doc)
	if err != nil {
		t.Fatalf("router from spec: %v", err)
	}
	r := newRouter(&stubSvc{})

	for _, rt := range allRoutes() {
		t.Run(rt.name, func(t *testing.T) {
			req := httptest.NewRequest(rt.method, "http://registry.test"+rt.path, strings.NewReader(rt.body))
			if rt.body != "" {
				req.Header.Set("Content-Type", "application/json")
			}
			req.Header.Set("X-Correlation-ID", corrID)
			rec := httptest.NewRecorder()
			r.ServeHTTP(rec, req)

			specRoute, params, err := router.FindRoute(req)
			if err != nil {
				t.Fatalf("%s %s is served but not declared: %v", rt.method, rt.path, err)
			}
			in := &openapi3filter.RequestValidationInput{
				Request: req, PathParams: params, Route: specRoute,
				Options: &openapi3filter.Options{AuthenticationFunc: openapi3filter.NoopAuthenticationFunc, ExcludeRequestBody: true},
			}
			out := &openapi3filter.ResponseValidationInput{
				RequestValidationInput: in,
				Status:                 rec.Code,
				Header:                 rec.Header(),
				Options:                &openapi3filter.Options{IncludeResponseStatus: true, ExcludeResponseBody: true},
			}
			out.SetBodyBytes(rec.Body.Bytes())
			if err := openapi3filter.ValidateResponse(context.Background(), out); err != nil {
				t.Errorf("%s %s answered %d, not as declared: %v", rt.method, rt.path, rec.Code, firstLine(err.Error()))
			}
		})
	}
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i > 0 {
		return s[:i]
	}
	return s
}

// TestOpenAPI_LiveResponsesMatchTheSchemas validates real response bodies.
// It runs only against a live service:
//
//	TER_LIVE_URL=http://localhost:8081 TER_LIVE_TENANT=<uuid> TER_LIVE_PRINCIPAL=<id> //	TER_LIVE_ENTITY=<uuid> go test ./internal/handler -run LiveResponses
func TestOpenAPI_LiveResponsesMatchTheSchemas(t *testing.T) {
	base := os.Getenv("TER_LIVE_URL")
	if base == "" {
		t.Skip("TER_LIVE_URL not set — live schema validation runs from scripts/audit.sh")
	}
	tenant, principal, entity := os.Getenv("TER_LIVE_TENANT"), os.Getenv("TER_LIVE_PRINCIPAL"), os.Getenv("TER_LIVE_ENTITY")
	doc := loadValidatedSpec(t)
	doc.Servers = openapi3.Servers{{URL: base}}
	router, err := gorillamux.NewRouter(doc)
	if err != nil {
		t.Fatalf("router from spec: %v", err)
	}
	paths := []string{
		"/v1/tenants/" + tenant,
		"/v1/tenants/" + tenant + "/defaults",
		"/v1/tenants/" + tenant + "/lifecycle-history",
		"/v1/tenants/" + tenant + "/entities",
		"/v1/tenants/" + tenant + "/residency-region",
		"/v1/residency-regions",
	}
	if entity != "" {
		paths = append(paths,
			"/v1/entities/"+entity,
			"/v1/entities/"+entity+"/versions",
			"/v1/entities/"+entity+"/status",
			"/v1/entities/"+entity+"/as-of?as_of="+time.Now().UTC().Format(time.RFC3339),
		)
	}
	for _, p := range paths {
		t.Run(p, func(t *testing.T) {
			req, _ := http.NewRequest(http.MethodGet, base+p, nil)
			req.Header.Set("X-Tenant-Id", tenant)
			req.Header.Set("X-Principal-Id", principal)
			req.Header.Set("X-Request-Id", "live-schema-"+strconv.FormatInt(time.Now().UnixNano(), 10))
			req.Header.Set("X-Correlation-ID", "live-schema")
			req.Header.Set("X-Source-Channel", "api")
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatalf("GET %s: %v", p, err)
			}
			defer resp.Body.Close()
			body, _ := io.ReadAll(resp.Body)

			specRoute, params, err := router.FindRoute(req)
			if err != nil {
				t.Fatalf("%s is not declared: %v", p, err)
			}
			in := &openapi3filter.RequestValidationInput{
				Request: req, PathParams: params, Route: specRoute,
				Options: &openapi3filter.Options{AuthenticationFunc: openapi3filter.NoopAuthenticationFunc},
			}
			out := &openapi3filter.ResponseValidationInput{
				RequestValidationInput: in, Status: resp.StatusCode, Header: resp.Header,
				Options: &openapi3filter.Options{IncludeResponseStatus: true},
			}
			out.SetBodyBytes(body)
			if err := openapi3filter.ValidateResponse(context.Background(), out); err != nil {
				t.Errorf("GET %s answered %d, not as declared: %v", p, resp.StatusCode, firstLine(err.Error()))
			}
		})
	}
}
