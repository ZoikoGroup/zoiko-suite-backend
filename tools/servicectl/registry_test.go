package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The generated table is the thing most likely to go wrong, because it is
// derived from four compose files that disagreed with each other and with the
// console. These tests hold the properties the generator is supposed to
// guarantee, so a bad regeneration fails here rather than as one service
// intermittently failing to bind.

func TestRegistryLoads(t *testing.T) {
	reg, err := LoadRegistry()
	if err != nil {
		t.Fatalf("registry invalid: %v", err)
	}
	if got := len(reg.All()); got != 86 {
		t.Errorf("expected 86 services, got %d", got)
	}
	if got := len(reg.Pages()); got != 25 {
		t.Errorf("expected 25 console routes, got %d", got)
	}
}

// Sixteen ports were claimed twice across the compose files. LoadRegistry
// refuses a duplicate, so this asserts the generator produced none.
func TestPortsAreUnique(t *testing.T) {
	reg, err := LoadRegistry()
	if err != nil {
		t.Fatalf("registry invalid: %v", err)
	}
	seen := map[int]string{}
	for _, s := range reg.All() {
		if prev, dup := seen[s.Port]; dup {
			t.Errorf("port %d claimed by both %s and %s", s.Port, prev, s.Name)
		}
		seen[s.Port] = s.Name
	}
}

// Every service in the table must be a directory that actually builds, or the
// launcher offers to start something that cannot exist.
func TestEveryServiceHasAnEntrypoint(t *testing.T) {
	reg, err := LoadRegistry()
	if err != nil {
		t.Fatalf("registry invalid: %v", err)
	}
	root, err := backendRootFrom("..", "..")
	if err != nil {
		t.Skipf("cannot locate backend root: %v", err)
	}
	for _, s := range reg.All() {
		main := filepath.Join(root, "services", s.Name, "cmd", "server", "main.go")
		if _, err := os.Stat(main); err != nil {
			t.Errorf("%s: no cmd/server/main.go (%v)", s.Name, err)
		}
	}
}

// No cross-service URL may still name a Docker DNS host, because nothing
// resolves those once the containers are gone. This is the property that makes
// "no Docker" true rather than aspirational.
func TestNoDockerHostnamesRemain(t *testing.T) {
	reg, err := LoadRegistry()
	if err != nil {
		t.Fatalf("registry invalid: %v", err)
	}
	// Infrastructure the launcher does not run is allowed to keep a hostname:
	// it comes from the global env, where the operator sets a real address.
	allowedHosts := map[string]bool{"127.0.0.1": true, "localhost": true}
	for _, s := range reg.All() {
		for k, v := range s.Env {
			if !strings.Contains(v, "http://") && !strings.Contains(v, "https://") {
				continue
			}
			for _, host := range hostsIn(v) {
				if !allowedHosts[host] {
					t.Errorf("%s %s=%s still points at %q, which has no DNS without Docker",
						s.Name, k, v, host)
				}
			}
		}
	}
}

func hostsIn(v string) []string {
	var out []string
	for _, scheme := range []string{"http://", "https://"} {
		rest := v
		for {
			i := strings.Index(rest, scheme)
			if i < 0 {
				break
			}
			rest = rest[i+len(scheme):]
			end := len(rest)
			for j, c := range rest {
				if c == ':' || c == '/' || c == '?' {
					end = j
					break
				}
			}
			out = append(out, rest[:end])
		}
	}
	return out
}

// A variable named after a service must address that service. Fourteen compose
// values did not: six asked authorization-svc for jurisdiction rules, six
// pointed AUTHZ_SERVICE_URL at the tenant registry's port.
func TestServiceURLsAddressTheServiceTheyName(t *testing.T) {
	reg, err := LoadRegistry()
	if err != nil {
		t.Fatalf("registry invalid: %v", err)
	}
	port := func(name string) int {
		s, ok := reg.Get(name)
		if !ok {
			t.Fatalf("no such service %q", name)
		}
		return s.Port
	}
	expect := map[string]int{
		"AUTHZ_SERVICE_URL":      port("authorization-svc"),
		"JURISDICTION_RULES_URL": port("jurisdiction-rules-svc"),
		"EVIDENCE_MANIFEST_URL":  port("evidence-manifest-svc"),
		"EMPLOYEE_MASTER_URL":    port("employee-master-svc"),
		"TENANT_REGISTRY_URL":    port("tenant-entity-registry-svc"),
		"LEDGER_SERVICE_URL":     port("general-ledger-svc"),
		"GENERAL_LEDGER_URL":     port("general-ledger-svc"),
		"SIEM_SERVICE_URL":       port("siem-integration-svc"),
	}
	for _, s := range reg.All() {
		for k, want := range expect {
			v, set := s.Env[k]
			if !set {
				continue
			}
			if !strings.Contains(v, fmt.Sprintf(":%d", want)) {
				t.Errorf("%s %s=%s should address port %d", s.Name, k, v, want)
			}
		}
	}
}

func TestPageResolutionUsesWholeSegments(t *testing.T) {
	reg, err := LoadRegistry()
	if err != nil {
		t.Fatalf("registry invalid: %v", err)
	}

	// A dynamic segment resolves to its parent route: /admin/legal/[contractId]
	// is a real page in this console.
	svcs, matched := reg.ServicesForPage("/admin/legal/abc-123")
	if matched != "/admin/legal" {
		t.Errorf("nested legal route matched %q, want /admin/legal", matched)
	}
	if len(svcs) == 0 {
		t.Error("nested legal route resolved to no services")
	}

	// A longer route name that merely starts with a known one must NOT match --
	// the reason matching is segment-wise and not strings.HasPrefix.
	if _, matched := reg.ServicesForPage("/admin/legalese"); matched != "" {
		t.Errorf("/admin/legalese matched %q; it is not a route", matched)
	}

	if _, matched := reg.ServicesForPage("/admin/finance"); matched != "/admin/finance" {
		t.Errorf("exact route matched %q", matched)
	}
	if _, matched := reg.ServicesForPage("/login"); matched != "" {
		t.Errorf("/login matched %q; it has no services", matched)
	}
}

func TestResolveMixesNamesAndPaths(t *testing.T) {
	reg, err := LoadRegistry()
	if err != nil {
		t.Fatalf("registry invalid: %v", err)
	}
	names, unknown := reg.Resolve([]string{"/admin/finance", "policy-svc", "not-a-service", "/admin/nope"})
	if len(unknown) != 2 {
		t.Errorf("expected 2 unknown items, got %v", unknown)
	}
	var hasPolicy, hasLedger bool
	for _, n := range names {
		if n == "policy-svc" {
			hasPolicy = true
		}
		if n == "general-ledger-svc" {
			hasLedger = true
		}
	}
	if !hasPolicy {
		t.Error("explicit service name was dropped")
	}
	if !hasLedger {
		t.Error("/admin/finance did not expand to general-ledger-svc")
	}

	// Deduplication matters: /admin/obligations and /admin/jurisdictions both
	// read jurisdiction-rules-svc, and starting it twice is a port conflict with
	// itself.
	names, _ = reg.Resolve([]string{"/admin/obligations", "/admin/jurisdictions"})
	seen := map[string]int{}
	for _, n := range names {
		seen[n]++
	}
	for n, c := range seen {
		if c > 1 {
			t.Errorf("%s appears %d times after Resolve", n, c)
		}
	}
}

// backendRootFrom walks up from a starting point looking for services/.
func backendRootFrom(parts ...string) (string, error) {
	dir, err := filepath.Abs(filepath.Join(parts...))
	if err != nil {
		return "", err
	}
	for {
		if fi, err := os.Stat(filepath.Join(dir, "services")); err == nil && fi.IsDir() {
			return dir, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", os.ErrNotExist
		}
		dir = parent
	}
}
