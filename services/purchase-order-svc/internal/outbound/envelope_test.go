package outbound_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"go.uber.org/zap"

	"zoiko.io/purchase-order-svc/internal/middleware"
	"zoiko.io/purchase-order-svc/internal/procurementcase"
	"zoiko.io/purchase-order-svc/internal/supplier"
)

// Both read clients send the full envelope: tenant, principal, legal entity,
// X-Source-Channel, a fresh X-Request-Id and the inbound X-Correlation-ID.
func TestReadClients_SendFullEnvelope(t *testing.T) {
	var got []http.Header
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = append(got, r.Header.Clone())
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	ctx := middleware.WithCorrelationID(context.Background(), "corr-123")
	_, _ = procurementcase.NewHTTPClient(srv.URL, zap.NewNop()).VerifyApproved(ctx, "tenant-1", "user-1", "le-1", "case-1", 10, "USD")
	_, _ = supplier.NewHTTPClient(srv.URL, zap.NewNop()).Eligibility(ctx, "tenant-1", "user-1", "le-1", "SUP-1")

	if len(got) != 2 {
		t.Fatalf("expected 2 calls, got %d", len(got))
	}
	for i, h := range got {
		for k, want := range map[string]string{
			"X-Tenant-Id": "tenant-1", "X-Principal-Id": "user-1", "X-Legal-Entity-Id": "le-1",
			"X-Source-Channel": "system", "X-Correlation-Id": "corr-123",
		} {
			if h.Get(k) != want {
				t.Errorf("call %d: %s = %q, want %q", i, k, h.Get(k), want)
			}
		}
		if h.Get("X-Request-Id") == "" {
			t.Errorf("call %d: X-Request-Id missing", i)
		}
	}
	if got[0].Get("X-Request-Id") == got[1].Get("X-Request-Id") {
		t.Error("each call needs its own X-Request-Id")
	}
}

func TestEnvelope_NoInboundCorrelation_GeneratesOne(t *testing.T) {
	var h http.Header
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h = r.Header.Clone()
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()
	_, _ = supplier.NewHTTPClient(srv.URL, zap.NewNop()).Eligibility(context.Background(), "t", "", "le", "S")
	if h.Get("X-Correlation-Id") == "" {
		t.Fatal("a correlation id must always be sent")
	}
	if h.Get("X-Principal-Id") != "" {
		t.Fatal("no principal must not be invented")
	}
}
