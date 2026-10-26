package paymentstatus

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"go.uber.org/zap"
)

// BNK-07 authorizes reads per principal, so the acting principal must reach it.
func TestGetStatusForwardsTenantAndPrincipal(t *testing.T) {
	var gotTenant, gotPrincipal, gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotTenant = r.Header.Get("X-Tenant-Id")
		gotPrincipal = r.Header.Get("X-Principal-Id")
		gotPath = r.URL.Path
		_ = json.NewEncoder(w).Encode(PaymentState{PaymentID: "pay-1", Status: "SETTLED"})
	}))
	defer srv.Close()

	c := NewHTTPClient(srv.URL, zap.NewNop())
	st, err := c.GetStatus(context.Background(), "tenant-1", "principal-1", "pay-1")
	if err != nil {
		t.Fatalf("GetStatus: %v", err)
	}
	if st.Status != "SETTLED" {
		t.Fatalf("status %q", st.Status)
	}
	if gotTenant != "tenant-1" || gotPrincipal != "principal-1" {
		t.Fatalf("headers tenant=%q principal=%q", gotTenant, gotPrincipal)
	}
	if gotPath != "/bnk07/payments/pay-1" {
		t.Fatalf("path %q", gotPath)
	}
}
