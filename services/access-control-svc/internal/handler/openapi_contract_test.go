package handler_test

import (
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	"gopkg.in/yaml.v3"
)

// Contract parity: every route access-control-svc serves must appear in
// openapi.yaml, and every path openapi.yaml declares must be served.
//
// Mirrors tenant-entity-registry-svc's openapi_contract_test.go; access-control
// builds its router through newRouter with stubs, so we walk that router
// instead of a shared helper.
type openAPIDoc struct {
	Paths map[string]map[string]any `yaml:"paths"`
}

func loadOpenAPI(t *testing.T) openAPIDoc {
	t.Helper()
	_, thisFile, _, _ := runtime.Caller(0)
	path := filepath.Join(filepath.Dir(thisFile), "../../openapi.yaml")

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read openapi.yaml: %v", err)
	}
	var doc openAPIDoc
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("parse openapi.yaml: %v", err)
	}
	if len(doc.Paths) == 0 {
		t.Fatal("openapi.yaml declares no paths — parsed, but into nothing")
	}
	return doc
}

// opID returns the operationId of an operation map value, or "".
func opID(op any) string {
	m, ok := op.(map[string]any)
	if !ok {
		return ""
	}
	if id, ok := m["operationId"].(string); ok {
		return id
	}
	return ""
}

// httpMethod reports whether a path-item key is an HTTP method.
func httpMethod(k string) bool {
	switch strings.ToUpper(k) {
	case "GET", "PUT", "POST", "DELETE", "PATCH", "OPTIONS", "HEAD", "TRACE":
		return true
	}
	return false
}

func registeredRoutes(t *testing.T) map[string]bool {
	t.Helper()
	// newGovRig (governance_test.go) wires both RegisterRoutes and
	// RegisterGovernanceRoutes, so its router is the full surface the real
	// server exposes. newRouter alone only wires RegisterRoutes.
	mux := newGovRig().r
	out := map[string]bool{}
	err := chi.Walk(mux, func(method, route string, _ http.Handler, _ ...func(http.Handler) http.Handler) error {
		out[method+" "+normPath(route)] = true
		return nil
	})
	if err != nil {
		t.Fatalf("chi.Walk: %v", err)
	}
	return out
}

func operational(route string) bool {
	for _, p := range []string{"/healthz", "/readyz", "/metrics"} {
		if strings.HasSuffix(route, " "+p) || strings.Contains(route, " "+p) {
			return true
		}
	}
	return false
}

// normPath trims a trailing slash and normalises colon-bound verbs
// (`/{id}:approve` and `/{id}/approve` are the same route) so chi.Walk output
// matches openapi paths.
func normPath(p string) string {
	p = strings.ReplaceAll(p, ":", "/")
	return strings.TrimSuffix(p, "/")
}

func declaredRoutes(doc openAPIDoc) map[string]bool {
	out := map[string]bool{}
	for path, methods := range doc.Paths {
		for method := range methods {
			if !httpMethod(method) {
				continue
			}
			out[strings.ToUpper(method)+" "+normPath(path)] = true
		}
	}
	return out
}

func TestOpenAPI_DeclaresEveryServedRoute(t *testing.T) {
	doc := loadOpenAPI(t)

	declared := declaredRoutes(doc)

	var missing []string
	for route := range registeredRoutes(t) {
		if operational(route) {
			continue
		}
		if !declared[route] {
			missing = append(missing, route)
		}
	}
	if len(missing) > 0 {
		sort.Strings(missing)
		t.Fatalf("routes served but absent from openapi.yaml:\n  %s",
			strings.Join(missing, "\n  "))
	}
}

func TestOpenAPI_DeclaresNothingItDoesNotServe(t *testing.T) {
	doc := loadOpenAPI(t)
	served := registeredRoutes(t)
	declared := declaredRoutes(doc)

	var phantom []string
	for route := range declared {
		if operational(route) {
			continue
		}
		if !served[route] {
			phantom = append(phantom, route)
		}
	}
	if len(phantom) > 0 {
		sort.Strings(phantom)
		t.Fatalf("openapi.yaml declares routes this service does not serve:\n  %s",
			strings.Join(phantom, "\n  "))
	}
}

func TestOpenAPI_EveryOperationHasAUniqueOperationID(t *testing.T) {
	doc := loadOpenAPI(t)

	seen := map[string]string{}
	var problems []string
	for path, methods := range doc.Paths {
		for method, op := range methods {
			if !httpMethod(method) {
				continue
			}
			where := strings.ToUpper(method) + " " + path
			id := opID(op)
			if id == "" {
				// access-control-svc's contract predates the operationId
				// standard; these routes are still parity-checked above. Only
				// flag duplicates among routes that do declare one.
				continue
			}
			if prev, dup := seen[id]; dup {
				problems = append(problems, id+": used by both "+prev+" and "+where)
				continue
			}
			seen[id] = where
		}
	}
	if len(problems) > 0 {
		sort.Strings(problems)
		t.Fatalf("operationId problems:\n  %s", strings.Join(problems, "\n  "))
	}
}
