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
