package handler_test

import (
	"net/http"
	"sort"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
)

// Every routed operation is documented in openapi.yaml and every documented
// operation is routed (both mounts: /v1/delegations and /v1/iam/delegations).
func TestOpenAPIMatchesTheRouter(t *testing.T) {
	doc := loadOpenAPI(t)
	documented := map[string]bool{}
	for path, item := range doc.Paths.Map() {
		if path == "/healthz" || path == "/readyz" || path == "/metrics" {
			continue
		}
		for method := range item.Operations() {
			documented[strings.ToUpper(method)+" "+strings.TrimSuffix(path, "/")] = true
		}
	}
	routed := map[string]bool{}
	_ = chi.Walk(newRouter(newStubStore(), &stubAuthZ{}, nil), func(method, route string, _ http.Handler, _ ...func(http.Handler) http.Handler) error {
		routed[method+" "+strings.TrimSuffix(route, "/")] = true
		return nil
	})
	var missingDoc, missingRoute []string
	for op := range routed {
		if !documented[op] {
			missingDoc = append(missingDoc, op)
		}
	}
	for op := range documented {
		if !routed[op] {
			missingRoute = append(missingRoute, op)
		}
	}
	sort.Strings(missingDoc)
	sort.Strings(missingRoute)
	if len(missingDoc) > 0 || len(missingRoute) > 0 || len(routed) < 18 {
		t.Fatalf("openapi.yaml out of step with the router (%d routed)\nrouted, not documented: %v\ndocumented, not routed: %v", len(routed), missingDoc, missingRoute)
	}
}
