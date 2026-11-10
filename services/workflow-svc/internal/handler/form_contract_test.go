package handler_test

import (
	"net/http"
	"os"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
)

// The BIZ-04 routes the router serves must match, exactly, the paths and methods in
// openapi.biz04-forms.yaml, so the contract cannot drift from the code silently.
func TestFormRoutes_MatchOpenAPIContract(t *testing.T) {
	served := map[string]bool{}
	err := chi.Walk(newTestRouter(&stubStore{}), func(method, route string, _ http.Handler, _ ...func(http.Handler) http.Handler) error {
		route = strings.TrimSuffix(route, "/")
		if strings.HasPrefix(route, "/v1/forms") || strings.HasPrefix(route, "/v1/form-submissions") {
			served[strings.ToLower(method)+" "+route] = true
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk routes: %v", err)
	}

	raw, err := os.ReadFile("../../openapi.biz04-forms.yaml")
	if err != nil {
		t.Fatalf("read contract: %v", err)
	}
	declared := map[string]bool{}
	pathRe := regexp.MustCompile(`^  (/v1/[^\s:]+):\s*$`)
	methodRe := regexp.MustCompile(`^    (get|post|put|patch|delete):\s*$`)
	cur := ""
	for _, line := range strings.Split(strings.ReplaceAll(string(raw), "\r", ""), "\n") {
		if m := pathRe.FindStringSubmatch(line); m != nil {
			cur = m[1]
			continue
		}
		if m := methodRe.FindStringSubmatch(line); m != nil && cur != "" {
			declared[m[1]+" "+cur] = true
		}
	}

	var onlyServed, onlyDeclared []string
	for k := range served {
		if !declared[k] {
			onlyServed = append(onlyServed, k)
		}
	}
	for k := range declared {
		if !served[k] {
			onlyDeclared = append(onlyDeclared, k)
		}
	}
	sort.Strings(onlyServed)
	sort.Strings(onlyDeclared)
	if len(served) == 0 || len(onlyServed) > 0 || len(onlyDeclared) > 0 {
		t.Fatalf("contract drift (served=%d declared=%d)\n  served but undocumented: %v\n  documented but not served: %v",
			len(served), len(declared), onlyServed, onlyDeclared)
	}
}
