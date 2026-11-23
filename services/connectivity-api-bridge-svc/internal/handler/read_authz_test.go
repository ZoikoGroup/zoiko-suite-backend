package handler

import (
	"net/http"
	"testing"
)

// Every read of connector, connection, registry and provider-profile data must
// carry a principal (401), be authorized (403 on denial) and fail closed when
// authorization-svc gives no decision (503).

func TestBridgeReads_RequirePrincipalAndAuthorization(t *testing.T) {
	h := newHarness(t)
	b := h.activeBridge(t, "tenant-a")
	paths := []string{
		"/v1/bridges",
		"/v1/bridges/" + b.BridgeID,
		"/v1/bridges/" + b.BridgeID + "/logs",
		"/v1/bridges/" + b.BridgeID + "/changes",
		"/v1/bridges/" + b.BridgeID + "/approvals",
		"/v1/bridges/" + b.BridgeID + "/profile-status",
		"/v1/bridges/" + b.BridgeID + "/health",
		"/v1/bridges/" + b.BridgeID + "/health/transitions",
		"/v1/bridges/" + b.BridgeID + "/checkpoints",
		"/v1/api-bridge/connections",
		"/v1/api-bridge/connections/" + b.BridgeID,
	}
	for _, p := range paths {
		if w := h.call("GET", p, "tenant-a", "", nil); w.Code != http.StatusUnauthorized {
			t.Errorf("GET %s without principal: %d, want 401 (%s)", p, w.Code, w.Body.String())
		}
		if w := h.call("GET", p, "tenant-a", "reader", nil); w.Code != http.StatusOK {
			t.Errorf("GET %s granted: %d, want 200 (%s)", p, w.Code, w.Body.String())
		}
	}
	h.authz.deny("outsider", ActionRead)
	for _, p := range paths {
		if w := h.call("GET", p, "tenant-a", "outsider", nil); w.Code != http.StatusForbidden {
			t.Errorf("GET %s denied: %d, want 403 (%s)", p, w.Code, w.Body.String())
		}
	}
	h.authz.mu.Lock()
	h.authz.down = true
	h.authz.mu.Unlock()
	for _, p := range paths {
		if w := h.call("GET", p, "tenant-a", "reader-2", nil); w.Code != http.StatusServiceUnavailable {
			t.Errorf("GET %s with authz down: %d, want 503 (%s)", p, w.Code, w.Body.String())
		}
	}
}

func TestBridgeRead_ForeignConnectorIs404BeforeAuthorization(t *testing.T) {
	h := newHarness(t)
	b := h.activeBridge(t, "tenant-a")
	h.authz.mu.Lock()
	h.authz.actions = nil
	h.authz.mu.Unlock()
	if w := h.call("GET", "/v1/bridges/"+b.BridgeID, "tenant-b", "reader", nil); w.Code != http.StatusNotFound {
		t.Fatalf("foreign connector: %d, want 404 (%s)", w.Code, w.Body.String())
	}
	h.authz.mu.Lock()
	defer h.authz.mu.Unlock()
	if len(h.authz.actions) != 0 {
		t.Fatalf("authorization asked about a connector that is not the caller's: %v", h.authz.actions)
	}
}

func TestProfileReads_RequirePrincipalAndAuthorization(t *testing.T) {
	h := newHarness(t)
	for _, p := range []string{"/v1/external-profiles", "/v1/external-profiles/bank-x/versions/2"} {
		if w := h.call("GET", p, "tenant-a", "", nil); w.Code != http.StatusUnauthorized {
			t.Errorf("GET %s without principal: %d, want 401", p, w.Code)
		}
		if w := h.call("GET", p, "tenant-a", "reader", nil); w.Code != http.StatusOK {
			t.Errorf("GET %s granted: %d, want 200 (%s)", p, w.Code, w.Body.String())
		}
	}
	h.authz.deny("outsider", ActionProfileRead)
	for _, p := range []string{"/v1/external-profiles", "/v1/external-profiles/bank-x/versions/2"} {
		if w := h.call("GET", p, "tenant-a", "outsider", nil); w.Code != http.StatusForbidden {
			t.Errorf("GET %s denied: %d, want 403", p, w.Code)
		}
	}
}

func TestConnectionReads_RequirePrincipalAndAuthorization(t *testing.T) {
	h := newConnHarness(t)
	c := h.create(t, "tenant-a", "SANDBOX", "acct-1")
	paths := []string{
		"/v1/integration-connections",
		"/v1/integration-connections/" + c.ConnectionID,
		"/v1/integration-connections/" + c.ConnectionID + "/checks",
		"/v1/integration-connections/" + c.ConnectionID + "/certifications",
		"/v1/integration-connections/" + c.ConnectionID + "/events",
		"/v1/integration-connections/" + c.ConnectionID + "/requests",
	}
	for _, p := range paths {
		if w := h.c("GET", p, "tenant-a", "", nil); w.Code != http.StatusUnauthorized {
			t.Errorf("GET %s without principal: %d, want 401", p, w.Code)
		}
		if w := h.c("GET", p, "tenant-a", "reader", nil); w.Code != http.StatusOK {
			t.Errorf("GET %s granted: %d, want 200 (%s)", p, w.Code, w.Body.String())
		}
	}
	h.authz.deny("outsider", ActionConnRead)
	for _, p := range paths {
		if w := h.c("GET", p, "tenant-a", "outsider", nil); w.Code != http.StatusForbidden {
			t.Errorf("GET %s denied: %d, want 403", p, w.Code)
		}
	}
}

func TestRegistryReads_RequirePrincipalAndAuthorization(t *testing.T) {
	h := newRegHarness(t)
	h.connector(t, "bank-x")
	v := h.publishedOverHTTP(t, "bank-x", "1.0.0")
	paths := []string{
		"/v1/connectors",
		"/v1/connectors/bank-x",
		"/v1/connectors/bank-x/compatibility",
		"/v1/connectors/bank-x/versions",
		"/v1/contract-versions/" + v.ContractVersionID,
	}
	if len(v.MappingVersions) > 0 {
		paths = append(paths, "/v1/mapping-versions/"+v.MappingVersions[0].MappingVersionID)
	}
	for _, p := range paths {
		if w := h.call("GET", p, "tenant-a", "", nil); w.Code != http.StatusUnauthorized {
			t.Errorf("GET %s without principal: %d, want 401", p, w.Code)
		}
		if w := h.call("GET", p, "tenant-a", "reader", nil); w.Code != http.StatusOK {
			t.Errorf("GET %s granted: %d, want 200 (%s)", p, w.Code, w.Body.String())
		}
	}
	h.authz.deny("outsider", ActionRegistryRead)
	for _, p := range paths {
		if w := h.call("GET", p, "tenant-a", "outsider", nil); w.Code != http.StatusForbidden {
			t.Errorf("GET %s denied: %d, want 403", p, w.Code)
		}
	}
}
