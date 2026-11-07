package handler_test

import (
	"context"
	"net/http"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"testing"

	"github.com/getkin/kin-openapi/openapi3"
	"github.com/go-chi/chi/v5"
	"go.uber.org/zap"

	"zoiko.io/authorization-svc/internal/handler"
	"zoiko.io/authorization-svc/internal/siem"
)

// openapi.yaml is checked with a real OpenAPI implementation, not only for
// route parity: a parity test passes for a document no client generator can
// read. Sandbox: no request is served — only the router's registrations are
// set side by side with the document (same pattern as
// governance-decision-log-svc and tenant-entity-registry-svc).
func loadAuthzSpec(t *testing.T) *openapi3.T {
	t.Helper()
	_, thisFile, _, _ := runtime.Caller(0)
	doc, err := openapi3.NewLoader().LoadFromFile(filepath.Join(filepath.Dir(thisFile), "../../openapi.yaml"))
	if err != nil {
		t.Fatalf("openapi.yaml does not load: %v", err)
	}
	if err := doc.Validate(context.Background()); err != nil {
		t.Fatalf("openapi.yaml is not a valid OpenAPI document: %v", err)
	}
	return doc
}

// Every route the router serves is documented, and nothing documented is
// unserved. The aliases (`/admin/v1/...` and `/v1/admin/...`, and the two
// available-actions spellings) are registered routes in their own right and
// are compared literally.
func TestOpenAPI_MatchesTheRouter(t *testing.T) {
	doc := loadAuthzSpec(t)

	documented := map[string]bool{}
	for path, item := range doc.Paths.Map() {
		for method := range item.Operations() {
			documented[method+" "+path] = true
		}
	}

	r := chi.NewRouter()
	handler.RegisterRoutes(r, handler.New(&stubStore{}, &stubPublisher{}, &stubValidator{},
		siem.New("", "authorization-svc", zap.NewNop()), "platform-scope-entity", false, zap.NewNop()))
	// The available-actions route is served under two spellings of the same
	// template. OpenAPI forbids the twin ("conflicting paths" — kin-openapi
	// enforces it), so the document carries the canonical spelling only; the
	// shorthand is not counted as undocumented.
	shorthand := map[string]bool{"GET /v1/{resource}/{id}/available-actions": true}

	served := map[string]bool{}
	_ = chi.Walk(r, func(method, route string, _ http.Handler, _ ...func(http.Handler) http.Handler) error {
		key := method + " " + strings.TrimSuffix(route, "/")
		if !shorthand[key] {
			served[key] = true
		}
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

// Every 4xx/5xx the contract publishes is RFC 9457 problem+json pointing at
// the Problem schema (or a schema built on it), and nothing else is: a request
// body or a success relabelled as a problem is as wrong as a legacy error
// (ZS-API-001 §12).
func TestOpenAPI_ProblemJSONOnlyOnErrors(t *testing.T) {
	doc := loadAuthzSpec(t)
	isProblem := func(ref *openapi3.SchemaRef) bool {
		if ref == nil {
			return false
		}
		if strings.HasSuffix(ref.Ref, "/Problem") {
			return true
		}
		if ref.Value != nil {
			for _, part := range ref.Value.AllOf {
				if strings.HasSuffix(part.Ref, "/Problem") {
					return true
				}
			}
		}
		return false
	}
	for path, item := range doc.Paths.Map() {
		for method, op := range item.Operations() {
			if op.RequestBody != nil && op.RequestBody.Value != nil &&
				op.RequestBody.Value.Content.Get("application/problem+json") != nil {
				t.Errorf("%s %s: a request body is not a problem", method, path)
			}
			for code, resp := range op.Responses.Map() {
				mt := resp.Value.Content.Get("application/problem+json")
				switch {
				case code[0] == '4' || code[0] == '5':
					if path == "/healthz" || path == "/readyz" {
						continue
					}
					if mt == nil || !isProblem(mt.Schema) {
						t.Errorf("%s %s %s must be application/problem+json with the Problem schema", method, path, code)
					}
				case mt != nil:
					t.Errorf("%s %s %s is a success, not a problem", method, path, code)
				}
			}
		}
	}
}
