package handler_test

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"zoiko.io/purchase-order-svc/internal/domain"
)

// readPaths are every GET route, for an order that exists.
func readPaths(id string) []string {
	base := "/v1/purchase-orders/" + id
	return []string{
		"/v1/purchase-orders/",
		base, base + "/amendments", base + "/revisions", base + "/revisions/1", base + "/history",
		base + "/open-quantity", base + "/receipt-status", base + "/invoice-status", base + "/available-actions",
	}
}

func (e *env) getWith(path string, headers map[string]string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodGet, path, nil)
	req.Header.Set("X-Tenant-Id", tenantA)
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	e.router.ServeHTTP(rec, req)
	return rec
}

func TestReads_NoPrincipal_401(t *testing.T) {
	e := newEnv()
	o := e.createDraft(t)
	for _, p := range readPaths(o.PurchaseOrderID) {
		rec := e.getWith(p, nil)
		if rec.Code != http.StatusUnauthorized || codeOf(t, rec) != "IDENTITY_MISSING" {
			t.Errorf("GET %s without a principal: %d %s, want 401 IDENTITY_MISSING", p, rec.Code, rec.Body.String())
		}
	}
}

func TestReads_AuthzDenied_403_AndChecksPORead(t *testing.T) {
	e := newEnv()
	o := e.createDraft(t)
	e.authz.err, e.authz.denyOnly = domain.ErrAuthorizationDenied, "PO_READ"
	for _, p := range readPaths(o.PurchaseOrderID) {
		rec := e.getWith(p, map[string]string{"X-Principal-Id": "outsider"})
		if rec.Code != http.StatusForbidden || codeOf(t, rec) != "AUTHORIZATION_DENIED" {
			t.Errorf("GET %s denied PO_READ: %d %s, want 403", p, rec.Code, rec.Body.String())
		}
	}
	if !e.authz.saw("PO_READ") {
		t.Fatal("reads must be authorized with PO_READ")
	}
}

func TestReads_AuthzUnavailable_503(t *testing.T) {
	e := newEnv()
	o := e.createDraft(t)
	e.authz.err, e.authz.denyOnly = errors.New("authorization-svc down"), "PO_READ"
	for _, p := range readPaths(o.PurchaseOrderID) {
		if rec := e.getWith(p, map[string]string{"X-Principal-Id": "reader"}); rec.Code != http.StatusServiceUnavailable {
			t.Errorf("GET %s with authz unreachable: %d, want 503 (fail closed)", p, rec.Code)
		}
	}
}

func TestReads_PrincipalAllowed_200(t *testing.T) {
	e := newEnv()
	o := e.createDraft(t)
	for _, p := range readPaths(o.PurchaseOrderID) {
		if rec := e.getWith(p, map[string]string{"X-Principal-Id": "reader"}); rec.Code != http.StatusOK {
			t.Errorf("GET %s by an authorized reader: %d %s", p, rec.Code, rec.Body.String())
		}
	}
}

// Documents the machine-read path that accounts-payable-svc and
// goods-service-receipt-svc depend on: no principal plus
// X-Source-Channel: system is served (tenant-scoped, no PO_READ check). The
// declaration is not verifiable; see readCaller. Still tenant-scoped: another
// tenant's order is a 404.
func TestReads_DeclaredSystemCall_ServedWithoutPrincipal_StillTenantScoped(t *testing.T) {
	e := newEnv()
	o := e.createDraft(t)
	if rec := e.getWith("/v1/purchase-orders/"+o.PurchaseOrderID, map[string]string{"X-Source-Channel": "system"}); rec.Code != http.StatusOK {
		t.Fatalf("declared system read: %d %s", rec.Code, rec.Body.String())
	}
	if e.authz.saw("PO_READ") {
		t.Error("a principal-less system read has no principal to authorize")
	}
	req := httptest.NewRequest(http.MethodGet, "/v1/purchase-orders/"+o.PurchaseOrderID, nil)
	req.Header.Set("X-Tenant-Id", tenantB)
	req.Header.Set("X-Source-Channel", "system")
	rec := httptest.NewRecorder()
	e.router.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("system read of another tenant's order: %d, want 404", rec.Code)
	}
}
