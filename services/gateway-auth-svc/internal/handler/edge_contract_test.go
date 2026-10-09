package handler_test

// Verify's answer only reaches a backend through Traefik's ForwardAuth
// authResponseHeaders list, which lives in two places outside this module: the
// compose "gateway-auth" middleware and the GTRM-compiled tenant routers.
// Traefik deletes each listed name from the client's request and copies this
// service's value in. A header Verify sets that a list omits is therefore not
// merely dropped — the client's own copy passes through as if verified. On
// 25 Sep the GTRM list omitted X-Legal-Entity-Id and four more, and every test
// in this module stayed green, because none of them read the edge config.

import (
	"net/http"
	"os"
	"regexp"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

const (
	composePath      = "../../../../deployments/docker-compose.yml"
	compiledGTRMPath = "../../../../deployments/gtrm/compiled-traefik.yml"
)

// Identity-class headers the gateway never sets. They are on both lists so
// that Traefik strips any client copy: no client can assert a workload
// identity or a support session.
var strippedByForwardAuth = []string{"X-Workload-Id", "X-Support-Context-Id"}

// §4 caller assertions. The envelope contract makes the owning service
// validate these, so the edge must let them through.
var callerAssertions = []string{
	"X-Purpose-Context", "X-Approval-Reference", "X-Evidence-Refs", "X-Causation-Id",
	"X-Workflow-Instance-Id", "X-Book-Id", "X-Source-Channel", "X-Expected-Version",
}

// headersVerifySets returns every X- header a successful Verify writes, taken
// from a real response rather than a hand-kept list, plus the two that are
// conditional and absent from a fresh, correlation-less request.
func headersVerifySets(t *testing.T) []string {
	t.Helper()
	rec := verifyWithRegistry(t, http.MethodGet, &stubRegistry{
		tenantBody: activeTenant, entityBody: ownEntity,
	})
	require.Equal(t, http.StatusOK, rec.Code)

	set := map[string]bool{"X-Tenant-Context-Stale": true, "X-Correlation-Id": true}
	for name := range rec.Header() {
		if strings.HasPrefix(name, "X-") {
			set[name] = true
		}
	}
	out := make([]string, 0, len(set))
	for name := range set {
		out = append(out, name)
	}
	return out
}

func composeAuthResponseHeaders(t *testing.T) map[string]bool {
	t.Helper()
	raw, err := os.ReadFile(composePath)
	require.NoError(t, err, "edge contract lives in the compose file")
	m := regexp.MustCompile(`gateway-auth\.forwardauth\.authResponseHeaders=([^"\s]+)`).FindSubmatch(raw)
	require.NotNil(t, m, "compose defines no gateway-auth authResponseHeaders")
	return canonicalSet(strings.Split(string(m[1]), ","))
}

// compiledAuthResponseHeaders reads the list under forwardAuth in the
// GTRM-compiled config: the "- X-..." items that follow authResponseHeaders.
func compiledAuthResponseHeaders(t *testing.T) map[string]bool {
	t.Helper()
	raw, err := os.ReadFile(compiledGTRMPath)
	require.NoError(t, err, "edge contract lives in the compiled GTRM config")
	var names []string
	in := false
	for _, line := range strings.Split(strings.ReplaceAll(string(raw), "\r\n", "\n"), "\n") {
		trimmed := strings.TrimSpace(line)
		switch {
		case trimmed == "authResponseHeaders:":
			in = true
		case in && strings.HasPrefix(trimmed, "- "):
			names = append(names, strings.TrimPrefix(trimmed, "- "))
		default:
			in = false
		}
	}
	require.NotEmpty(t, names, "compiled GTRM config has no authResponseHeaders")
	return canonicalSet(names)
}

// The token/hostname tenant check reads X-Zoiko-Resolved-Tenant-Id, which the
// GTRM ctx middleware sets. ForwardAuth sends /verify the request as it stands
// when ForwardAuth runs, so ctx must come first. It came after until 29 Sep,
// and the check compared nothing on every tenant host. And with
// trustForwardHeader on, a client's X-Forwarded-Method reaches Verify.
func TestEdgeContract_ResolvedTenantReachesVerify(t *testing.T) {
	raw, err := os.ReadFile(compiledGTRMPath)
	require.NoError(t, err)
	lines := strings.Split(strings.ReplaceAll(string(raw), "\r\n", "\n"), "\n")

	gated := 0
	for i, line := range lines {
		trimmed := strings.TrimSpace(line)
		if trimmed == "trustForwardHeader: true" {
			t.Errorf("compiled-traefik.yml:%d: ForwardAuth trusts inbound X-Forwarded-* headers", i+1)
		}
		if trimmed != "middlewares:" {
			continue
		}
		var chain []string
		for _, next := range lines[i+1:] {
			item := strings.TrimSpace(next)
			if !strings.HasPrefix(item, "- ") {
				break
			}
			chain = append(chain, strings.TrimPrefix(item, "- "))
		}
		auth, ctx := -1, -1
		for j, mw := range chain {
			switch {
			case mw == "gateway-auth":
				auth = j
			case strings.HasPrefix(mw, "gtrm-ctx-"):
				ctx = j
			}
		}
		if auth < 0 {
			continue
		}
		gated++
		if ctx < 0 || ctx > auth {
			t.Errorf("compiled-traefik.yml:%d: chain %v runs gateway-auth before the ctx middleware sets the resolved tenant", i+1, chain)
		}
	}
	require.NotZero(t, gated, "no gateway-auth router found in the compiled config")
}

func canonicalSet(names []string) map[string]bool {
	out := map[string]bool{}
	for _, n := range names {
		out[http.CanonicalHeaderKey(strings.TrimSpace(n))] = true
	}
	return out
}

func TestEdgeContract_ForwardAuthListsMatchVerify(t *testing.T) {
	lists := map[string]map[string]bool{
		"docker-compose.yml":    composeAuthResponseHeaders(t),
		"compiled-traefik.yml": compiledAuthResponseHeaders(t),
	}
	for file, listed := range lists {
		for _, h := range headersVerifySets(t) {
			if !listed[http.CanonicalHeaderKey(h)] {
				t.Errorf("%s: Verify sets %s but authResponseHeaders omits it — a client copy reaches the backend unreplaced", file, h)
			}
		}
		for _, h := range strippedByForwardAuth {
			if !listed[http.CanonicalHeaderKey(h)] {
				t.Errorf("%s: %s must be listed so Traefik strips the client's copy", file, h)
			}
		}
		for _, h := range callerAssertions {
			if listed[http.CanonicalHeaderKey(h)] {
				t.Errorf("%s: %s is a §4 caller assertion; listing it deletes it before the owning service sees it", file, h)
			}
		}
	}
}
