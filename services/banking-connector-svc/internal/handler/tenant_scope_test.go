package handler

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"zoiko.io/banking-connector-svc/internal/domain"
)

// createConnectionAs POSTs a connection as the given tenant and returns
// the created connection_id.
func createConnectionAs(t *testing.T, r http.Handler, tenantID, legalEntityID string) string {
	t.Helper()

	body, _ := json.Marshal(domain.CreateConnectionRequest{
		LegalEntityID: legalEntityID,
		BankName:      "Test Bank",
		BIC:           "TESTUS33XXX",
		AccountNumber: "GB00TEST1234567890",
		Currency:      "USD",
	})
	req := httptest.NewRequest("POST", "/v1/banking/connections", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Tenant-Id", tenantID)
	req.Header.Set("X-Principal-Id", "principal-01")

	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusCreated {
		t.Fatalf("create connection as %s: expected 201, got %d: %s", tenantID, w.Code, w.Body.String())
	}

	var created domain.BankConnection
	if err := json.Unmarshal(w.Body.Bytes(), &created); err != nil {
		t.Fatalf("unmarshal created connection: %v", err)
	}
	if created.ConnectionID == "" {
		t.Fatalf("created connection has no connection_id: %s", w.Body.String())
	}
	return created.ConnectionID
}

// TestMissingTenantHeader_Refused proves the fabricated-identity fix.
//
// The middleware used to substitute the literal string "default-tenant"
// when X-Tenant-Id was absent, so a header-less request SUCCEEDED under a
// tenant that does not exist — and every header-less caller shared that
// one bucket, able to read each other's bank data. Omitting the header was
// the easier request to make, which made the insecure path the path of
// least resistance.
func TestMissingTenantHeader_Refused(t *testing.T) {
	r, _ := setupTestRouter(t)

	body, _ := json.Marshal(domain.CreateConnectionRequest{
		LegalEntityID: "le-101",
		BankName:      "Test Bank",
		AccountNumber: "GB00TEST1234567890",
		Currency:      "USD",
	})

	for _, tc := range []struct {
		name, method, path string
		body               []byte
	}{
		{"create connection", "POST", "/v1/banking/connections", body},
		{"list connections", "GET", "/v1/banking/connections", nil},
		{"get connection", "GET", "/v1/banking/connections/some-id", nil},
		{"list statements", "GET", "/v1/banking/connections/some-id/statements", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var req *http.Request
			if tc.body != nil {
				req = httptest.NewRequest(tc.method, tc.path, bytes.NewReader(tc.body))
				req.Header.Set("Content-Type", "application/json")
			} else {
				req = httptest.NewRequest(tc.method, tc.path, nil)
			}
			// Deliberately NO X-Tenant-Id.
			req.Header.Set("X-Principal-Id", "principal-01")

			w := httptest.NewRecorder()
			r.ServeHTTP(w, req)

			if w.Code != http.StatusUnauthorized {
				t.Fatalf("expected 401 with no X-Tenant-Id, got %d: %s", w.Code, w.Body.String())
			}
			if bytes.Contains(w.Body.Bytes(), []byte("default-tenant")) {
				t.Fatalf("response must never mention a fabricated default tenant: %s", w.Body.String())
			}
		})
	}
}

// TestGetConnectionByID_ForeignTenant_NotFound proves the read-scoping
// fix. GetConnectionByID's query was `WHERE connection_id = $1` alone, so
// any caller holding another tenant's connection_id could read its
// bank_name, bic and account_number.
//
// 404 rather than 403 is deliberate: a distinct forbidden status would
// confirm that another tenant's connection_id exists.
func TestGetConnectionByID_ForeignTenant_NotFound(t *testing.T) {
	r, _ := setupTestRouter(t)

	connID := createConnectionAs(t, r, "tenant-a", "le-a")

	// Tenant B asks for tenant A's connection by id.
	req := httptest.NewRequest("GET", "/v1/banking/connections/"+connID, nil)
	req.Header.Set("X-Tenant-Id", "tenant-b")
	req.Header.Set("X-Principal-Id", "principal-01")

	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusNotFound {
		t.Fatalf("ISOLATION FAILURE: tenant B read tenant A's bank connection — expected 404, got %d: %s", w.Code, w.Body.String())
	}

	// Sanity: tenant A can still read its own.
	own := httptest.NewRequest("GET", "/v1/banking/connections/"+connID, nil)
	own.Header.Set("X-Tenant-Id", "tenant-a")
	own.Header.Set("X-Principal-Id", "principal-01")

	ownW := httptest.NewRecorder()
	r.ServeHTTP(ownW, own)
	if ownW.Code != http.StatusOK {
		t.Fatalf("tenant A must still read its own connection, got %d: %s", ownW.Code, ownW.Body.String())
	}
}

// TestListConnections_OmittedFilter_DoesNotLeakOtherTenants proves the
// self-disabling-filter fix.
//
// The query matched when the legal_entity_id parameter was the empty
// string OR equalled the column, so omitting legal_entity_id — the
// shorter, easier request — disabled the only filter present and returned
// EVERY tenant's bank connections. The legal-entity dimension is
// legitimately optional; the tenant dimension never is.
func TestListConnections_OmittedFilter_DoesNotLeakOtherTenants(t *testing.T) {
	r, _ := setupTestRouter(t)

	createConnectionAs(t, r, "tenant-a", "le-a")
	createConnectionAs(t, r, "tenant-b", "le-b")

	// Tenant B lists with NO legal_entity_id filter at all.
	req := httptest.NewRequest("GET", "/v1/banking/connections", nil)
	req.Header.Set("X-Tenant-Id", "tenant-b")
	req.Header.Set("X-Principal-Id", "principal-01")

	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	// The list route returns an envelope, not a bare array — and it repeats
	// the same rows under both "connections" and "accounts" (the route is
	// registered at both paths for compatibility). Check both, so a leak
	// through either key is caught.
	var got struct {
		Connections []domain.BankConnection `json:"connections"`
		Accounts    []domain.BankConnection `json:"accounts"`
		Total       int                     `json:"total"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal list: %v (body=%s)", err, w.Body.String())
	}

	for key, list := range map[string][]domain.BankConnection{
		"connections": got.Connections,
		"accounts":    got.Accounts,
	} {
		for _, c := range list {
			if c.TenantID != "tenant-b" {
				t.Fatalf("ISOLATION FAILURE: tenant B's unfiltered list returned tenant %q's connection %s under %q", c.TenantID, c.ConnectionID, key)
			}
		}
		if len(list) != 1 {
			t.Fatalf("%s: expected exactly tenant B's own 1 connection, got %d: %+v", key, len(list), list)
		}
	}
	if got.Total != 1 {
		t.Fatalf("expected total=1 for tenant B, got %d", got.Total)
	}
}
