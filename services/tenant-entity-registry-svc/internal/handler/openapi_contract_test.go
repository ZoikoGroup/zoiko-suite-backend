package handler_test

// Contract parity: every route this service serves must appear in openapi.yaml,
// and every path openapi.yaml declares must be served.
//
// This exists because the drift it catches is invisible. Five workspace routes
// had been live since migration 000005 and were absent from openapi.yaml
// entirely — not deprecated, not undocumented on purpose, just missing, and
// nothing failed. A consumer generating a client from the contract got no
// workspace methods at all and had no way to know why.
//
// ORG §9.2's first Definition-of-Done gate is that the machine contract passes
// compatibility gates. A contract that silently omits a third of the surface
// cannot; this test is what makes that gate mean something here.

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

// openAPIDoc is the fragment of the document this test needs.
type openAPIDoc struct {
	Paths map[string]map[string]struct {
		OperationID string `yaml:"operationId"`
	} `yaml:"paths"`
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

// registeredRoutes returns every METHOD + chi pattern the router serves.
func registeredRoutes(t *testing.T) map[string]bool {
	t.Helper()
	mux, ok := newRouter(&stubSvc{}).(*chi.Mux)
	if !ok {
		t.Fatal("router is not a *chi.Mux")
	}
	out := map[string]bool{}
	err := chi.Walk(mux, func(method, route string, _ http.Handler, _ ...func(http.Handler) http.Handler) error {
		if len(route) > 1 {
			route = strings.TrimSuffix(route, "/")
		}
		out[method+" "+route] = true
		return nil
	})
	if err != nil {
		t.Fatalf("chi.Walk: %v", err)
	}
	return out
}

// operational returns the routes that belong in the API contract, excluding
// the operational endpoints. /healthz, /readyz and /metrics are declared in
// openapi.yaml as documentation for operators, but they are not part of the
// versioned API surface and are not what a generated client consumes.
func operational(route string) bool {
	for _, p := range []string{"/healthz", "/readyz", "/metrics"} {
		if strings.HasSuffix(route, " "+p) || strings.Contains(route, " "+p) {
			return true
		}
	}
	return false
}

func TestOpenAPI_DeclaresEveryServedRoute(t *testing.T) {
	doc := loadOpenAPI(t)

	declared := map[string]bool{}
	for path, methods := range doc.Paths {
		for method := range methods {
			declared[strings.ToUpper(method)+" "+path] = true
		}
	}

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

	var phantom []string
	for path, methods := range doc.Paths {
		for method := range methods {
			route := strings.ToUpper(method) + " " + path
			if operational(route) {
				continue
			}
			if !served[route] {
				phantom = append(phantom, route)
			}
		}
	}
	if len(phantom) > 0 {
		sort.Strings(phantom)
		// A declared-but-unserved route is the more damaging direction: a
		// consumer generates a client for it, calls it, and gets a 404 from a
		// service that never had it.
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
			where := strings.ToUpper(method) + " " + path
			if op.OperationID == "" {
				problems = append(problems, where+": no operationId")
				continue
			}
			// Generated clients name methods after operationId, so a duplicate
			// silently drops one of the two operations from the client.
			if prev, dup := seen[op.OperationID]; dup {
				problems = append(problems, op.OperationID+": used by both "+prev+" and "+where)
				continue
			}
			seen[op.OperationID] = where
		}
	}
	if len(problems) > 0 {
		sort.Strings(problems)
		t.Fatalf("operationId problems:\n  %s", strings.Join(problems, "\n  "))
	}
}

// TestOpenAPI_NamesEveryORGOperation checks the ORG-02/ORG-03 named commands
// and read surfaces are present under the spec's OWN names.
//
// The route-parity tests above would pass if every ORG operation were renamed
// to something unrecognisable, as long as the contract matched the router.
// This asserts the estate can find the spec's named operations by their spec
// names rather than by something that resembles them — the same check
// identity-context-svc's contract test makes for GOV-01.
func TestOpenAPI_NamesEveryORGOperation(t *testing.T) {
	doc := loadOpenAPI(t)

	present := map[string]bool{}
	for _, methods := range doc.Paths {
		for _, op := range methods {
			present[op.OperationID] = true
		}
	}

	// ORG-02 §4.2 and ORG-03 §4.3, as the spec names them. The five lifecycle
	// commands are reached through executeTenantCommand, whose `command` path
	// parameter enumerates them; that enum is asserted separately below.
	for _, want := range []string{
		// ORG-02 commands and reads.
		"provisionTenant",            // CreateTenant
		"executeTenantCommand",       // Activate/Suspend/Resume/Initiate/CompleteTermination
		"changeDefaultLocale",        // ChangeDefaultLocale
		"getTenant",                  // GetTenant
		"resolveTenantByHost",        // ResolveTenantByHost
		"listTenantLifecycleHistory", // ListTenantLifecycleHistory
		"getTenantDefaults",          // GetTenantDefaults

		// ORG-03 commands and reads.
		"createEntity",           // CreateLegalEntity
		"amendLegalProfile",      // AmendLegalProfile
		"changeRegisteredOffice", // ChangeRegisteredOffice
		"changeLegalName",        // ChangeLegalName
		"transitionEntityStatus", // DeactivateLegalEntity
		"getEntity",              // GetLegalEntity
		"findByRegistryNumber",   // FindByRegistryNumber
		"listEntityVersions",     // ListEntityVersions
		"getLegalEntityAsOf",     // GetLegalEntityAsOf
	} {
		if !present[want] {
			t.Errorf("openapi.yaml has no operation %q — an ORG-02/ORG-03 named operation is missing from the contract", want)
		}
	}
}

// TestOpenAPI_TenantCommandEnumListsEveryLifecycleCommand pins the enum on the
// command path parameter.
//
// Without this, executeTenantCommand could exist while declaring an enum that
// omits CompleteTermination, and a generated client would have no way to invoke
// it — the operation would be present and the command unreachable.
func TestOpenAPI_TenantCommandEnumListsEveryLifecycleCommand(t *testing.T) {
	_, thisFile, _, _ := runtime.Caller(0)
	raw, err := os.ReadFile(filepath.Join(filepath.Dir(thisFile), "../../openapi.yaml"))
	if err != nil {
		t.Fatalf("read openapi.yaml: %v", err)
	}

	var doc struct {
		Paths map[string]map[string]struct {
			OperationID string `yaml:"operationId"`
			Parameters  []struct {
				Name   string `yaml:"name"`
				Schema struct {
					Enum []string `yaml:"enum"`
				} `yaml:"schema"`
			} `yaml:"parameters"`
		} `yaml:"paths"`
	}
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("parse openapi.yaml: %v", err)
	}

	var enum []string
	for _, methods := range doc.Paths {
		for _, op := range methods {
			if op.OperationID != "executeTenantCommand" {
				continue
			}
			for _, p := range op.Parameters {
				if p.Name == "command" {
					enum = p.Schema.Enum
				}
			}
		}
	}
	if len(enum) == 0 {
		t.Fatal("executeTenantCommand declares no enum for its command parameter")
	}

	got := map[string]bool{}
	for _, c := range enum {
		got[c] = true
	}
	for _, want := range []string{
		"ActivateTenant", "SuspendTenant", "ResumeTenant",
		"InitiateTermination", "CompleteTermination",
	} {
		if !got[want] {
			t.Errorf("command enum omits ORG-02 named command %q", want)
		}
	}
}
