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

	"zoiko.io/governance-decision-log-svc/internal/handler"
)

// openapi.yaml is checked with a real OpenAPI implementation, not only for
// route parity: a parity test passes for a document no client generator can
// read.
func loadSpec(t *testing.T) *openapi3.T {
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
// unserved.
func TestOpenAPI_MatchesTheRouter(t *testing.T) {
	doc := loadSpec(t)

	documented := map[string]bool{}
	for path, item := range doc.Paths.Map() {
		for method := range item.Operations() {
			documented[method+" "+path] = true
		}
	}

	r := chi.NewRouter()
	handler.RegisterRoutes(r, handler.New(&stubStore{}, testAuthz(), &stubPolicyClient{}, testAuthzScopeID, zap.NewNop()))
	served := map[string]bool{}
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
		t.Fatalf("openapi.yaml out of step with the router — undocumented: %v, unserved: %v", missing, extra)
	}
}
