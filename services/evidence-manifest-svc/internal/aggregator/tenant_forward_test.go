package aggregator_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"go.uber.org/zap"

	"zoiko.io/evidence-manifest-svc/internal/aggregator"
	svcmiddleware "zoiko.io/evidence-manifest-svc/internal/middleware"
)

// These tests pin the tenant header forwarding, which was missing entirely.
//
// That was not merely a defence-in-depth gap: it left manifest generation
// NON-FUNCTIONAL for two of the three sources. governance-decision-log-svc
// answers 400 missing_tenant_id without the header, and workflow-svc answers
// 401 missing_tenant_scope (the latter directly because the Priority 1 row 6
// fix correctly stopped its by-id read falling back to an unscoped lookup).
// getByID maps any non-200 to ErrSourceUnavailable and collectRecords fails
// closed on the first source error, so the whole manifest failed — reported
// as "source unavailable", which reads as a downstream outage rather than a
// missing header.
//
// So these tests guard two things at once: that the tenant boundary extends
// across the service call, and that manifest generation works at all.

func TestGovernanceClient_ForwardsTenantHeader(t *testing.T) {
	var gotTenant string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotTenant = r.Header.Get("X-Tenant-Id")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"decision_id":"gd-1"}`))
	}))
	defer srv.Close()

	c := aggregator.NewGovernanceDecisionClient(srv.URL, zap.NewNop())
	ctx := svcmiddleware.WithTenant(context.Background(), "tenant-a")
	if _, err := c.GetByID(ctx, "gd-1"); err != nil {
		t.Fatalf("GetByID: %v", err)
	}
	if gotTenant != "tenant-a" {
		t.Fatalf("expected X-Tenant-Id to be forwarded as %q, got %q", "tenant-a", gotTenant)
	}
}

func TestGovernanceClient_List_ForwardsTenantHeader(t *testing.T) {
	var gotTenant string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotTenant = r.Header.Get("X-Tenant-Id")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`[]`))
	}))
	defer srv.Close()

	c := aggregator.NewGovernanceDecisionClient(srv.URL, zap.NewNop())
	ctx := svcmiddleware.WithTenant(context.Background(), "tenant-a")
	if _, err := c.ListByEntityAndDateRange(ctx, "e1", nil, nil); err != nil {
		t.Fatalf("ListByEntityAndDateRange: %v", err)
	}
	if gotTenant != "tenant-a" {
		t.Fatalf("expected X-Tenant-Id to be forwarded as %q, got %q", "tenant-a", gotTenant)
	}
}

func TestWorkflowAndAccessClients_ForwardTenantHeader(t *testing.T) {
	for _, tc := range []struct {
		name string
		call func(baseURL string, ctx context.Context) error
	}{
		{"workflow", func(baseURL string, ctx context.Context) error {
			_, err := aggregator.NewWorkflowClient(baseURL, zap.NewNop()).GetByID(ctx, "wf-1")
			return err
		}},
		{"access decision", func(baseURL string, ctx context.Context) error {
			_, err := aggregator.NewAccessDecisionClient(baseURL, zap.NewNop()).GetByID(ctx, "ad-1")
			return err
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var gotTenant string
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				gotTenant = r.Header.Get("X-Tenant-Id")
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"id":"x"}`))
			}))
			defer srv.Close()

			ctx := svcmiddleware.WithTenant(context.Background(), "tenant-a")
			if err := tc.call(srv.URL, ctx); err != nil {
				t.Fatalf("%s GetByID: %v", tc.name, err)
			}
			if gotTenant != "tenant-a" {
				t.Fatalf("%s: expected X-Tenant-Id %q, got %q", tc.name, "tenant-a", gotTenant)
			}
		})
	}
}

// TestClient_TenantlessContext_SendsNoHeader documents the deliberate
// choice not to invent a value when there is no verified tenant. The
// downstream service then applies its own fail-closed rule (400/401) rather
// than being handed a fabricated tenant that would satisfy it — which is
// the "default-tenant" mistake found across the connector services, one
// hop further out.
func TestClient_TenantlessContext_SendsNoHeader(t *testing.T) {
	sawHeader := true
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, sawHeader = r.Header["X-Tenant-Id"]
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"decision_id":"gd-1"}`))
	}))
	defer srv.Close()

	c := aggregator.NewGovernanceDecisionClient(srv.URL, zap.NewNop())
	if _, err := c.GetByID(context.Background(), "gd-1"); err != nil {
		t.Fatalf("GetByID: %v", err)
	}
	if sawHeader {
		t.Fatal("a tenant-less context must send NO X-Tenant-Id header, not an empty or invented one")
	}
}
