package handler

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
)

// openapi.yaml is checked with a real OpenAPI implementation, not only for route parity: a parity test passes for a document that no
// client generator can read. No request is served; the router's registrations are set side by side with the document.
func loadKernelSpec(t *testing.T) *openapi3.T {
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

// Every route the kernel serves is documented, and nothing documented is unserved.
func TestKernelOpenAPIMatchesTheRouter(t *testing.T) {
	doc := loadKernelSpec(t)
	documented := map[string]bool{}
	for path, item := range doc.Paths.Map() {
		for method := range item.Operations() {
			documented[method+" "+path] = true
		}
	}

	r := chi.NewRouter()
	RegisterKernelRoutes(r, &KernelHandler{})
	routed := map[string]bool{}
	_ = chi.Walk(r, func(method, route string, _ http.Handler, _ ...func(http.Handler) http.Handler) error {
		routed[method+" "+strings.TrimSuffix(route, "/")] = true
		return nil
	})

	var undocumented, unrouted []string
	for op := range routed {
		if !documented[op] {
			undocumented = append(undocumented, op)
		}
	}
	for op := range documented {
		if !routed[op] {
			unrouted = append(unrouted, op)
		}
	}
	sort.Strings(undocumented)
	sort.Strings(unrouted)
	if len(undocumented) > 0 || len(unrouted) > 0 {
		t.Fatalf("openapi.yaml is out of step with the router\nrouted, not documented: %v\ndocumented, not routed: %v", undocumented, unrouted)
	}
	if len(routed) < 90 {
		t.Fatalf("only %d routes walked; the parity check is not seeing the router", len(routed))
	}
}

// Every operation states what it needs and how it can fail: the authorization action it checks (the rule packs list is the one
// read that checks only the rule-read action, still named), the shared failure responses, and a success response.
func TestKernelOpenAPIOperationsStateTheirContract(t *testing.T) {
	doc := loadKernelSpec(t)
	for path, item := range doc.Paths.Map() {
		for method, op := range item.Operations() {
			where := method + " " + path
			if op.Summary == "" || op.OperationID == "" || len(op.Tags) == 0 {
				t.Errorf("%s: a summary, an operation id and a tag are required", where)
			}
			acts, _ := op.Extensions["x-authorization-actions"].([]any)
			if len(acts) == 0 {
				t.Errorf("%s: no x-authorization-actions", where)
			}
			for _, code := range []string{"401", "403", "503"} {
				if op.Responses.Value(code) == nil {
					t.Errorf("%s: no %s response", where, code)
				}
			}
			ok := false
			for code := range op.Responses.Map() {
				ok = ok || strings.HasPrefix(code, "2")
			}
			if !ok {
				t.Errorf("%s: no success response", where)
			}
			if method == "POST" || method == "PUT" {
				if op.Responses.Value("422") == nil || op.Responses.Value("409") == nil {
					t.Errorf("%s: a command states its 409 and 422 failures", where)
				}
			}
		}
	}
	for _, tag := range doc.Tags {
		used := false
		for _, item := range doc.Paths.Map() {
			for _, op := range item.Operations() {
				for _, tg := range op.Tags {
					used = used || tg == tag.Name
				}
			}
		}
		if !used {
			t.Errorf("tag %s is declared and no operation uses it", tag.Name)
		}
	}
	for path, item := range doc.Paths.Map() {
		for _, op := range item.Operations() {
			for _, tg := range op.Tags {
				found := false
				for _, d := range doc.Tags {
					found = found || d.Name == tg
				}
				if !found {
					t.Errorf("%s: tag %s is not declared", path, tg)
				}
			}
		}
	}
}
