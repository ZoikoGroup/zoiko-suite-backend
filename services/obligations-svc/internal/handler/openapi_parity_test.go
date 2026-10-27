package handler_test

import (
	"bufio"
	"net/http"
	"os"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	"go.uber.org/zap"

	"zoiko.io/obligations-svc/internal/handler"
)

// TestOpenAPIMatchesTheRouter keeps openapi.yaml and the router in step: every
// routed operation is documented and every documented operation is routed.
// (Validity of the document itself is checked with kin-openapi; route parity
// alone once passed for a document that was not valid OpenAPI.)
func TestOpenAPIMatchesTheRouter(t *testing.T) {
	f, err := os.Open("../../openapi.yaml")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	pathRe := regexp.MustCompile(`^  (/\S+):\s*$`)
	opRe := regexp.MustCompile(`^    (get|post|put|patch|delete):`)
	documented := map[string]bool{}
	current := ""
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimRight(sc.Text(), "\r")
		if m := pathRe.FindStringSubmatch(line); m != nil {
			current = m[1]
			continue
		}
		if strings.HasPrefix(line, "components:") {
			current = ""
		}
		if m := opRe.FindStringSubmatch(line); m != nil && current != "" {
			if current == "/healthz" || current == "/readyz" {
				continue // mounted in main.go
			}
			documented[strings.ToUpper(m[1])+" "+current] = true
		}
	}

	r := chi.NewRouter()
	h := handler.New(&stubStore{}, &stubValidator{}, &stubAuthz{}, zap.NewNop())
	handler.RegisterRoutes(r, h)
	handler.RegisterApplicabilityRoutes(r, h)
	routed := map[string]bool{}
	_ = chi.Walk(r, func(method, route string, _ http.Handler, _ ...func(http.Handler) http.Handler) error {
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
	if len(missingDoc) > 0 || len(missingRoute) > 0 {
		t.Fatalf("openapi.yaml out of step with the router\nrouted, not documented: %v\ndocumented, not routed: %v", missingDoc, missingRoute)
	}
	if len(routed) < 20 {
		t.Fatalf("only %d routes walked; the parity check is not seeing the router", len(routed))
	}
}
