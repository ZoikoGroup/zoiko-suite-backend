package authz

import (
	"testing"

	"go.uber.org/zap"
)

// The production guard used to be an exact-match list of two strings, so any
// URL merely SHAPED like a placeholder — a loopback port, a documentation
// domain — read as a real authorization service and the deployment came up
// behind a permit-all stub or a port nothing was listening on.
//
// The rule is deliberately a positive test for provably-local addresses. An
// unrecognised host is treated as real, because refusing one is a refusal to
// boot in production, which is worse than the failure being prevented.

func TestNonProductionURLsAreRefusedOutsideDevelopment(t *testing.T) {
	refused := []string{
		"http://localhost:8089",
		"http://127.0.0.1:8089",
		"http://[::1]:8089",
		"http://0.0.0.0:8089",
		"http://authz.example.com",
		"https://authz.example.org",
		"http://authz.example.net",
		"http://authz.invalid",
		"http://authz.test",
		"authorization-svc:8089", // no scheme — addresses nothing
	}
	for _, env := range []string{"production", "staging", "PRODUCTION"} {
		for _, u := range refused {
			if _, err := NewClient(env, u, zap.NewNop()); err == nil {
				t.Errorf("%s must refuse %q", env, u)
			}
		}
	}
}

// The other half, and the one that costs an outage if it regresses: a guard
// that refuses real addresses is a guard nobody can deploy behind. A private
// RFC 1918 address is an ordinary way to reach authorization-svc.
func TestDeployedURLsAreAcceptedInProduction(t *testing.T) {
	for _, u := range []string{
		"http://authorization-svc:8089",
		"http://authorization-svc.governance.svc.cluster.local:8089",
		"https://authz.internal.zoiko.io",
		"http://10.0.3.7:8089",
	} {
		c, err := NewClient("production", u, zap.NewNop())
		if err != nil {
			t.Errorf("production must accept %q: %v", u, err)
			continue
		}
		if c == nil {
			t.Errorf("production accepted %q but returned no client", u)
		}
	}
}

// Loopback is refused in production but must NOT collapse into the stub in
// development: a locally-run authorization-svc is addressed exactly this way,
// and ignoring it would make local authorization testing impossible while
// looking like it worked.
func TestLoopbackIsARealClientInDevelopment(t *testing.T) {
	c, err := NewClient("development", "http://localhost:8089", zap.NewNop())
	if err != nil {
		t.Fatalf("development must accept a loopback URL: %v", err)
	}
	if _, isStub := c.(*PermitAllClient); isStub {
		t.Fatal("a configured loopback URL in development is a real client, not the stub")
	}
}
