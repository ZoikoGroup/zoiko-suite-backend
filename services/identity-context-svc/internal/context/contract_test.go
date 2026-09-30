package context_test

import (
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	"gopkg.in/yaml.v3"

	identityctx "zoiko.io/identity-context-svc/internal/context"
)

// DoD gate 1: "Machine contract published and compatibility certified."
//
// A published contract only certifies anything if something checks it against
// the code. These tests walk the chi router and the OpenAPI document and
// assert they describe the same service — in BOTH directions, because the two
// failure modes are different and both real:
//
//   - a route with no spec entry is an undocumented endpoint, which a client
//     cannot call and a reviewer cannot audit;
//   - a spec entry with no route is a documented endpoint that 404s, which is
//     worse, because a client will build against it.
//
// They also pin the GOV-01 operationIds, so this service implements the
// spec's NAMED surface rather than something that merely resembles it.

type openAPIDoc struct {
	Paths map[string]map[string]struct {
		OperationID string `yaml:"operationId"`
	} `yaml:"paths"`
	Components struct {
		Schemas map[string]any `yaml:"schemas"`
	} `yaml:"components"`
}

func loadSpec(t *testing.T) openAPIDoc {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "..", "openapi.yaml"))
	require.NoError(t, err, "openapi.yaml must be present — it is the published contract")

	var doc openAPIDoc
	require.NoError(t, yaml.Unmarshal(raw, &doc))
	require.NotEmpty(t, doc.Paths)
	return doc
}

// routerPaths walks the mounted router and returns "METHOD /path" entries with
// chi's {param} syntax normalised to the OpenAPI spelling.
func routerPaths(t *testing.T) map[string]bool {
	t.Helper()

	f := defaultFixture()
	sessions := newMockSessionCache()
	f.sessions = sessions
	h := identityctx.NewHandler(f.build(), nil, sessions, f.principals, &permittingAuthz{}, zap.NewNop())

	r := chi.NewRouter()
	identityctx.RegisterRoutes(r, h)

	out := map[string]bool{}
	err := chi.Walk(r, func(method, route string, _ http.Handler, _ ...func(http.Handler) http.Handler) error {
		// chi renders wildcards with a trailing slash on some subtrees.
		route = strings.TrimSuffix(route, "/")
		if route == "" {
			route = "/"
		}
		out[method+" "+route] = true
		return nil
	})
	require.NoError(t, err)
	return out
}

// chiParam rewrites chi's {sessionContextID} to the spec's
// {sessionContextId}. The two differ only in case, which is exactly the sort
// of divergence a human comparing two lists by eye does not notice.
var chiParam = regexp.MustCompile(`\{([A-Za-z]+)\}`)

func normaliseParams(route string) string {
	return chiParam.ReplaceAllStringFunc(route, func(m string) string {
		return strings.ToLower(m)
	})
}

func TestEveryRouteIsInThePublishedContract(t *testing.T) {
	doc := loadSpec(t)

	specPaths := map[string]bool{}
	for path, methods := range doc.Paths {
		for method := range methods {
			specPaths[normaliseParams(strings.ToUpper(method)+" "+path)] = true
		}
	}

	routes := routerPaths(t)
	// Guard against a vacuous pass: if the walk returned nothing, the
	// comparison below would trivially succeed and this test would certify a
	// contract it never checked.
	require.GreaterOrEqual(t, len(routes), 12,
		"the route walk found too few routes to be checking anything")

	var undocumented []string
	for route := range routes {
		if !specPaths[normaliseParams(route)] {
			undocumented = append(undocumented, route)
		}
	}
	sort.Strings(undocumented)

	assert.Empty(t, undocumented,
		"these routes are served but not published — a client cannot call them and a reviewer cannot audit them")
}

func TestEveryContractPathIsServed(t *testing.T) {
	doc := loadSpec(t)
	served := routerPaths(t)

	normalisedServed := map[string]bool{}
	for r := range served {
		normalisedServed[normaliseParams(r)] = true
	}

	// Mounted outside RegisterRoutes in cmd/server, so they are correctly in
	// the contract but not in this router. Named explicitly rather than
	// pattern-matched, so adding a new one is a deliberate act.
	mountedElsewhere := map[string]bool{
		"GET /health":                true,
		"GET /.well-known/jwks.json": true,
	}

	var unserved []string
	for path, methods := range doc.Paths {
		for method := range methods {
			key := normaliseParams(strings.ToUpper(method) + " " + path)
			if mountedElsewhere[key] || normalisedServed[key] {
				continue
			}
			unserved = append(unserved, key)
		}
	}
	sort.Strings(unserved)

	assert.Empty(t, unserved,
		"these paths are published but 404 — a client will build against them")
}

// TestGov01OperationIdsArePublished pins the spec's NAMED surface.
//
// GOV-01's contract names six operations. Implementing something that behaves
// similarly under a different name would satisfy a behavioural test and still
// leave the estate unable to assert this service implements GOV-01.
func TestGov01OperationIdsArePublished(t *testing.T) {
	doc := loadSpec(t)

	found := map[string]bool{}
	for _, methods := range doc.Paths {
		for _, op := range methods {
			if op.OperationID != "" {
				found[op.OperationID] = true
			}
		}
	}

	for _, want := range []string{
		// Queries
		"resolveContext",           // ResolveTenantContext
		"getSessionContext",        // GetEffectiveContext
		"explainContextResolution", // ExplainContextResolution
		// Commands
		"refreshTenantContextCache", // RefreshTenantContextCache
		"invalidateTenantContext",   // InvalidateTenantContext
		"attachSupportContext",      // AttachSupportContext (privileged)
	} {
		assert.True(t, found[want], "GOV-01 operation %q is not published in the contract", want)
	}
}

// TestStableErrorCatalogueIsPublished: a client branches on the code, so the
// enumeration has to be in the contract or the code is not stable in any sense
// the client can rely on.
func TestStableErrorCatalogueIsPublished(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "openapi.yaml"))
	require.NoError(t, err)
	spec := string(raw)

	for _, code := range []string{
		"CONTEXT_UNRESOLVED",
		"RESIDENCY_DENIED",
		"SOD_CONFLICT",
		"BREAK_GLASS_REQUIRED",
		"UPSTREAM_UNAVAILABLE",
	} {
		assert.Contains(t, spec, code, "stable error class %q must be published", code)
	}
}

func TestEverySchemaReferenceResolves(t *testing.T) {
	doc := loadSpec(t)
	raw, err := os.ReadFile(filepath.Join("..", "..", "openapi.yaml"))
	require.NoError(t, err)

	refPattern := regexp.MustCompile(`#/components/schemas/([A-Za-z0-9_]+)`)
	var dangling []string
	for _, m := range refPattern.FindAllStringSubmatch(string(raw), -1) {
		if _, ok := doc.Components.Schemas[m[1]]; !ok {
			dangling = append(dangling, m[1])
		}
	}
	sort.Strings(dangling)

	// A dangling $ref makes the whole document unusable to a generator, and
	// the failure surfaces in whichever client tried to generate from it
	// rather than here.
	assert.Empty(t, dangling, "unresolved schema references")
}
