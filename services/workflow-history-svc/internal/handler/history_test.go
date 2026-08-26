package handler_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"go.uber.org/zap"

	"zoiko.io/workflow-history-svc/internal/authz"
	"zoiko.io/workflow-history-svc/internal/handler"
	"zoiko.io/workflow-history-svc/internal/store"
)

// Handler tests for workflow-history-svc (tracker rows 94 + a Priority 1c
// finding).
//
// This service had no handler tests at all, which is part of how the defect
// below survived: the read API was never exercised above the store.
//
// The defect was NOT a missing authorization check — it was a cross-tenant
// read. Both routes took tenant_id from the QUERY STRING and nothing in the
// service ever read X-Tenant-Id. Because the store then fed that value into
// set_config('app.tenant_id', ...) and the RLS policy reads it back with
// USING (tenant_id = current_setting('app.tenant_id', true)), the policy was
// SATISFIED rather than bypassed: Postgres faithfully returned the rows of
// whichever tenant the caller named in the URL.
//
// That is the same security-theater shape as the five Priority 1b business
// services, but strictly worse. There, a header-less caller landed in one
// shared synthetic "default-tenant" bucket. Here a caller chose its victim by
// editing a query parameter.

// fakeReadStore records what tenant it was asked for, so the tests can assert
// on the value that would reach set_config — not merely on the HTTP status.
// A store double that ignored the tenant could not distinguish "scoped
// correctly" from "scoped to whatever the caller asked for", which is exactly
// the distinction under test.
type fakeReadStore struct {
	events        []store.WorkflowHistoryEvent
	gotTenantID   string
	gotFilter     store.QueryFilter
	filterCalled  bool
	instanceCalls int
}

func (f *fakeReadStore) ListByInstance(_ context.Context, tenantID, instanceID string) ([]store.WorkflowHistoryEvent, error) {
	f.gotTenantID = tenantID
	f.instanceCalls++
	var out []store.WorkflowHistoryEvent
	for _, e := range f.events {
		if e.TenantID == tenantID && e.WorkflowInstanceID == instanceID {
			out = append(out, e)
		}
	}
	return out, nil
}

func (f *fakeReadStore) ListByFilter(_ context.Context, filter store.QueryFilter) ([]store.WorkflowHistoryEvent, error) {
	f.gotFilter = filter
	f.filterCalled = true
	var out []store.WorkflowHistoryEvent
	for _, e := range f.events {
		if e.TenantID == filter.TenantID && e.LegalEntityID == filter.LegalEntityID {
			out = append(out, e)
		}
	}
	return out, nil
}

func (f *fakeReadStore) GetTenantContext(_ context.Context, _ string) (store.TenantContext, bool, error) {
	return store.TenantContext{}, false, nil
}

type stubAuthz struct {
	err error
}

func (s *stubAuthz) CheckAllowed(_ context.Context, _, _, _ string) error { return s.err }

func newRouter(rs store.ReadStore, az handler.AuthzChecker) http.Handler {
	h := handler.New(rs, az, zap.NewNop())
	r := chi.NewRouter()
	r.Get("/v1/workflows/history", h.GetCrossWorkflowHistory)
	r.Get("/v1/workflows/{workflow_instance_id}/history", h.GetInstanceHistory)
	return r
}

func seedEvents() []store.WorkflowHistoryEvent {
	return []store.WorkflowHistoryEvent{
		{
			EventID: "e1", WorkflowInstanceID: "wf-a", EventType: "workflow.started",
			TenantID: "tenant-a", LegalEntityID: "le-a",
			Payload: json.RawMessage(`{"secret":"tenant-a-approval-detail"}`), RecordedAt: time.Now(),
		},
		{
			EventID: "e2", WorkflowInstanceID: "wf-b", EventType: "workflow.started",
			TenantID: "tenant-b", LegalEntityID: "le-b",
			Payload: json.RawMessage(`{"secret":"tenant-b-detail"}`), RecordedAt: time.Now(),
		},
	}
}

// TestQueryTenantIDCannotOverrideVerifiedTenant is the regression test for the
// cross-tenant read. Tenant B asks for tenant A's instance by naming
// tenant-a in the query string — the exact request that used to work.
func TestQueryTenantIDCannotOverrideVerifiedTenant(t *testing.T) {
	fake := &fakeReadStore{events: seedEvents()}
	r := newRouter(fake, &stubAuthz{})

	req := httptest.NewRequest(http.MethodGet, "/v1/workflows/wf-a/history?tenant_id=tenant-a", nil)
	req.Header.Set("X-Tenant-Id", "tenant-b")
	req.Header.Set("X-Principal-Id", "principal-b")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusForbidden {
		t.Fatalf("expected 403 when the query tenant_id disagrees with the verified header, got %d: %s",
			w.Code, w.Body.String())
	}
	if bytes.Contains(w.Body.Bytes(), []byte("tenant-a-approval-detail")) {
		t.Fatalf("ISOLATION FAILURE: response leaked tenant-a's payload: %s", w.Body.String())
	}
	// The store must not even have been consulted — the refusal happens before
	// any value reaches set_config.
	if fake.instanceCalls != 0 {
		t.Fatalf("store was queried %d times on a refused request; the mismatch must be caught before the store",
			fake.instanceCalls)
	}
}

// TestVerifiedTenantIsWhatReachesTheStore is the positive half, and it asserts
// on the VALUE rather than the status code. A 200 alone would not distinguish
// "used the header" from "used the query parameter".
func TestVerifiedTenantIsWhatReachesTheStore(t *testing.T) {
	fake := &fakeReadStore{events: seedEvents()}
	r := newRouter(fake, &stubAuthz{})

	// No tenant_id in the query at all — the header is the only source.
	req := httptest.NewRequest(http.MethodGet, "/v1/workflows/wf-a/history", nil)
	req.Header.Set("X-Tenant-Id", "tenant-a")
	req.Header.Set("X-Principal-Id", "principal-a")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	if fake.gotTenantID != "tenant-a" {
		t.Fatalf("store received tenant %q, expected the verified header value tenant-a", fake.gotTenantID)
	}
}

func TestMissingTenantHeader_Refused(t *testing.T) {
	fake := &fakeReadStore{events: seedEvents()}
	r := newRouter(fake, &stubAuthz{})

	for _, tc := range []struct {
		name, path string
	}{
		{"instance history", "/v1/workflows/wf-a/history?tenant_id=tenant-a"},
		{"cross-workflow history", "/v1/workflows/history?tenant_id=tenant-a&legal_entity_id=le-a&from=2026-01-01T00:00:00Z&to=2026-12-31T00:00:00Z"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, tc.path, nil)
			req.Header.Set("X-Principal-Id", "principal-a")
			// Deliberately NO X-Tenant-Id — previously the query parameter
			// alone was enough, which is the whole defect.
			w := httptest.NewRecorder()
			r.ServeHTTP(w, req)

			if w.Code != http.StatusUnauthorized {
				t.Fatalf("expected 401 with no X-Tenant-Id, got %d: %s", w.Code, w.Body.String())
			}
			if bytes.Contains(w.Body.Bytes(), []byte("tenant-a-approval-detail")) {
				t.Fatalf("ISOLATION FAILURE: a header-less request returned data: %s", w.Body.String())
			}
		})
	}
}

func TestMissingPrincipal_Refused(t *testing.T) {
	fake := &fakeReadStore{events: seedEvents()}
	r := newRouter(fake, &stubAuthz{})

	req := httptest.NewRequest(http.MethodGet, "/v1/workflows/wf-a/history", nil)
	req.Header.Set("X-Tenant-Id", "tenant-a")
	// Deliberately NO X-Principal-Id.
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 with no principal, got %d: %s", w.Code, w.Body.String())
	}
}

func TestAuthzDenied_Refused(t *testing.T) {
	fake := &fakeReadStore{events: seedEvents()}
	r := newRouter(fake, &stubAuthz{err: authz.ErrAuthorizationDenied})

	for _, tc := range []struct {
		name, path string
	}{
		{"instance history", "/v1/workflows/wf-a/history"},
		{"cross-workflow history", "/v1/workflows/history?legal_entity_id=le-a&from=2026-01-01T00:00:00Z&to=2026-12-31T00:00:00Z"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, tc.path, nil)
			req.Header.Set("X-Tenant-Id", "tenant-a")
			req.Header.Set("X-Principal-Id", "principal-denied")
			w := httptest.NewRecorder()
			r.ServeHTTP(w, req)

			if w.Code != http.StatusForbidden {
				t.Fatalf("expected 403 when authorization is DENIED, got %d: %s", w.Code, w.Body.String())
			}
			if bytes.Contains(w.Body.Bytes(), []byte("tenant-a-approval-detail")) {
				t.Fatalf("a denied principal received the payload: %s", w.Body.String())
			}
		})
	}
}

func TestAuthzUnavailable_FailsClosed(t *testing.T) {
	fake := &fakeReadStore{events: seedEvents()}
	r := newRouter(fake, &stubAuthz{err: authz.ErrAuthzServiceUnavailable})

	req := httptest.NewRequest(http.MethodGet, "/v1/workflows/wf-a/history", nil)
	req.Header.Set("X-Tenant-Id", "tenant-a")
	req.Header.Set("X-Principal-Id", "principal-a")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503 when authorization-svc is unreachable, got %d: %s", w.Code, w.Body.String())
	}
}

// TestCrossWorkflowUsesVerifiedTenant covers the same substitution on the
// filter route, asserting on the QueryFilter the store actually received.
func TestCrossWorkflowUsesVerifiedTenant(t *testing.T) {
	fake := &fakeReadStore{events: seedEvents()}
	r := newRouter(fake, &stubAuthz{})

	req := httptest.NewRequest(http.MethodGet,
		"/v1/workflows/history?legal_entity_id=le-a&from=2026-01-01T00:00:00Z&to=2026-12-31T00:00:00Z", nil)
	req.Header.Set("X-Tenant-Id", "tenant-a")
	req.Header.Set("X-Principal-Id", "principal-a")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	if !fake.filterCalled {
		t.Fatal("expected the filter query to reach the store")
	}
	if fake.gotFilter.TenantID != "tenant-a" {
		t.Fatalf("filter carried tenant %q, expected the verified header value tenant-a", fake.gotFilter.TenantID)
	}
}
