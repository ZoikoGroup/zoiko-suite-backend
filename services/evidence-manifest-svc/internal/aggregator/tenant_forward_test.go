package aggregator_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

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

// These tests pin the X-Principal-Id forwarding, which was ALSO missing
// entirely — found while wiring WorkflowHistoryClient.ListByEntityAndDateRange.
// workflow-history-svc requires X-Principal-Id on every route, including
// the per-instance history endpoint ListByInstanceID already called, so
// that call was silently 401ing against a real workflow-history-svc the
// whole time — masked because the only tests covering it used a stub that
// never checked for the header. See aggregator.forwardIdentity's doc
// comment.

func TestWorkflowHistoryClient_ListByInstanceID_ForwardsPrincipalHeader(t *testing.T) {
	var gotPrincipal string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPrincipal = r.Header.Get("X-Principal-Id")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`[]`))
	}))
	defer srv.Close()

	c := aggregator.NewWorkflowHistoryClient(srv.URL, zap.NewNop())
	ctx := svcmiddleware.WithPrincipal(context.Background(), "principal-a")
	if _, err := c.ListByInstanceID(ctx, "wf-1"); err != nil {
		t.Fatalf("ListByInstanceID: %v", err)
	}
	if gotPrincipal != "principal-a" {
		t.Fatalf("expected X-Principal-Id to be forwarded as %q, got %q", "principal-a", gotPrincipal)
	}
}

func TestAllClients_ForwardPrincipalHeader(t *testing.T) {
	for _, tc := range []struct {
		name string
		call func(baseURL string, ctx context.Context) error
	}{
		{"governance GetByID", func(baseURL string, ctx context.Context) error {
			_, err := aggregator.NewGovernanceDecisionClient(baseURL, zap.NewNop()).GetByID(ctx, "gd-1")
			return err
		}},
		{"governance List", func(baseURL string, ctx context.Context) error {
			_, err := aggregator.NewGovernanceDecisionClient(baseURL, zap.NewNop()).ListByEntityAndDateRange(ctx, "e1", nil, nil)
			return err
		}},
		{"workflow", func(baseURL string, ctx context.Context) error {
			_, err := aggregator.NewWorkflowClient(baseURL, zap.NewNop()).GetByID(ctx, "wf-1")
			return err
		}},
		{"access decision", func(baseURL string, ctx context.Context) error {
			_, err := aggregator.NewAccessDecisionClient(baseURL, zap.NewNop()).GetByID(ctx, "ad-1")
			return err
		}},
		{"workflow history cross-workflow", func(baseURL string, ctx context.Context) error {
			_, err := aggregator.NewWorkflowHistoryClient(baseURL, zap.NewNop()).ListByEntityAndDateRange(ctx, "e1", time.Now(), time.Now())
			return err
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var gotPrincipal string
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				gotPrincipal = r.Header.Get("X-Principal-Id")
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`[]`))
			}))
			defer srv.Close()

			ctx := svcmiddleware.WithPrincipal(context.Background(), "principal-a")
			if err := tc.call(srv.URL, ctx); err != nil {
				t.Fatalf("%s: %v", tc.name, err)
			}
			if gotPrincipal != "principal-a" {
				t.Fatalf("%s: expected X-Principal-Id %q, got %q", tc.name, "principal-a", gotPrincipal)
			}
		})
	}
}

// TestWorkflowHistoryClient_ListByEntityAndDateRange_SendsRequiredParams
// pins the cross-workflow query shape (legal_entity_id, from, to all
// required, unlike GovernanceDecisionClient's optional from/to) and the
// event_id-per-record decoding, mirroring ListByInstanceID's own shape.
func TestWorkflowHistoryClient_ListByEntityAndDateRange_SendsRequiredParams(t *testing.T) {
	var gotEntity, gotFrom, gotTo string
	from := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	to := time.Date(2026, 1, 31, 0, 0, 0, 0, time.UTC)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotEntity = r.URL.Query().Get("legal_entity_id")
		gotFrom = r.URL.Query().Get("from")
		gotTo = r.URL.Query().Get("to")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`[{"event_id":"evt-1"},{"event_id":"evt-2"}]`))
	}))
	defer srv.Close()

	c := aggregator.NewWorkflowHistoryClient(srv.URL, zap.NewNop())
	ctx := svcmiddleware.WithTenant(context.Background(), "tenant-a")
	recs, err := c.ListByEntityAndDateRange(ctx, "entity-1", from, to)
	if err != nil {
		t.Fatalf("ListByEntityAndDateRange: %v", err)
	}
	if gotEntity != "entity-1" {
		t.Fatalf("expected legal_entity_id=entity-1, got %q", gotEntity)
	}
	if gotFrom != from.Format(time.RFC3339) || gotTo != to.Format(time.RFC3339) {
		t.Fatalf("expected from/to %s/%s, got %s/%s", from.Format(time.RFC3339), to.Format(time.RFC3339), gotFrom, gotTo)
	}
	if len(recs) != 2 || recs[0].SourceRecordID != "evt-1" || recs[1].SourceRecordID != "evt-2" {
		t.Fatalf("expected 2 records evt-1/evt-2, got %+v", recs)
	}
}
