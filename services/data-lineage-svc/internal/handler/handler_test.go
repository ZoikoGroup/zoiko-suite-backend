package handler_test

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"go.uber.org/zap"

	"zoiko.io/data-lineage-svc/internal/authz"
	"zoiko.io/data-lineage-svc/internal/domain"
	"zoiko.io/data-lineage-svc/internal/handler"
	"zoiko.io/data-lineage-svc/internal/store"
)

// Handler-level tests. The store suite proves the database; these prove the
// HTTP layer in front of it: that a request with no verified tenant or
// principal never reaches the store, that authorization fails closed, that the
// tenant handed to the store is the header's and nothing else, and that store
// errors map to the status codes callers depend on.

const (
	tenantA   = "11111111-1111-1111-1111-111111111111"
	writePath = "/v1/data-lineage/manifests/ent_1:seal"
	readPath  = "/v1/data-lineage/entities/ent_1"
)

// recordingStore embeds the interface so only the methods a test exercises need
// bodies; anything else panics on a nil interface, which a test would surface.
type recordingStore struct {
	store.Store
	mu     sync.Mutex
	calls  int
	tenant string
	claim  domain.IdempotencyClaim
	err    error
}

func (s *recordingStore) note(tenant string, claim domain.IdempotencyClaim) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls++
	s.tenant = tenant
	s.claim = claim
}

func (s *recordingStore) SealManifest(_ context.Context, tenantID, entityID, _ string, claim domain.IdempotencyClaim) (*domain.ProvenanceManifest, error) {
	s.note(tenantID, claim)
	if s.err != nil {
		return nil, s.err
	}
	return &domain.ProvenanceManifest{ManifestID: "pm_x", TenantID: tenantID, EntityID: entityID}, nil
}

func (s *recordingStore) GetEntity(_ context.Context, tenantID, entityID string) (*domain.LineageEntity, error) {
	s.note(tenantID, domain.IdempotencyClaim{})
	if s.err != nil {
		return nil, s.err
	}
	return &domain.LineageEntity{EntityID: entityID, TenantID: tenantID}, nil
}

// authzStub answers /v1/authorize and records the body of every call, so a test
// can assert which action was checked.
type authzStub struct {
	*httptest.Server
	mu     sync.Mutex
	bodies []string
}

func newAuthz(t *testing.T, status int, outcome string) *authzStub {
	t.Helper()
	a := &authzStub{}
	a.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		a.mu.Lock()
		a.bodies = append(a.bodies, string(b))
		a.mu.Unlock()
		w.WriteHeader(status)
		_, _ = w.Write([]byte(`{"decision_outcome":"` + outcome + `"}`))
	}))
	t.Cleanup(a.Close)
	return a
}

func (a *authzStub) lastBody() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	if len(a.bodies) == 0 {
		return ""
	}
	return a.bodies[len(a.bodies)-1]
}

func newServer(t *testing.T, st store.Store, az *authzStub) http.Handler {
	t.Helper()
	h := handler.NewHandler(st, authz.NewClient(az.URL, zap.NewNop()), zap.NewNop())
	return handler.NewRouter(h)
}

// do sends a request carrying the full canonical envelope, minus whatever the
// caller overrides. An empty override value removes the header.
func do(t *testing.T, srv http.Handler, method, path, body string, override map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	hdr := map[string]string{
		"X-Tenant-Id":            tenantA,
		"X-Principal-Id":         "tester",
		"X-Correlation-ID":       "corr-1",
		"X-Purpose-Context":      "audit",
		"X-Request-Id":           "req-1",
		"X-Source-Channel":       "api",
		"X-Timezone":             "UTC",
		"X-Jurisdiction-Context": "GB",
		"Content-Type":           "application/json",
		"Idempotency-Key":        "key-1",
	}
	for k, v := range override {
		hdr[k] = v
	}
	for k, v := range hdr {
		if v != "" {
			req.Header.Set(k, v)
		}
	}
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	return rec
}

func TestNoTenant_RefusedBeforeAnythingElse(t *testing.T) {
	st := &recordingStore{}
	az := newAuthz(t, 200, "GRANTED")
	rec := do(t, newServer(t, st, az), "GET", readPath, "", map[string]string{"X-Tenant-Id": ""})
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
	if st.calls != 0 || az.lastBody() != "" {
		t.Fatalf("a request with no tenant reached the store (%d) or authz", st.calls)
	}
}

func TestNoPrincipal_Refused(t *testing.T) {
	st := &recordingStore{}
	az := newAuthz(t, 200, "GRANTED")
	rec := do(t, newServer(t, st, az), "GET", readPath, "", map[string]string{"X-Principal-Id": ""})
	if rec.Code != http.StatusUnauthorized && rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 401 (or 400 from the envelope)", rec.Code)
	}
	if st.calls != 0 {
		t.Fatalf("a request with no principal reached the store")
	}
}

func TestAuthzDenied_Is403_AndStoreUntouched(t *testing.T) {
	for _, tc := range []struct{ method, path string }{{"POST", writePath}, {"GET", readPath}} {
		st := &recordingStore{}
		az := newAuthz(t, 200, "DENIED")
		rec := do(t, newServer(t, st, az), tc.method, tc.path, "", nil)
		if rec.Code != http.StatusForbidden {
			t.Errorf("%s %s: status = %d, want 403", tc.method, tc.path, rec.Code)
		}
		if st.calls != 0 {
			t.Errorf("%s %s: denied request reached the store", tc.method, tc.path)
		}
	}
}

// Authorization that cannot be reached must never read as permission.
func TestAuthzUnavailable_FailsClosed(t *testing.T) {
	for _, status := range []int{500, 404, 503} {
		st := &recordingStore{}
		az := newAuthz(t, status, "GRANTED") // body says GRANTED; the status must still win
		rec := do(t, newServer(t, st, az), "GET", readPath, "", nil)
		if rec.Code != http.StatusServiceUnavailable {
			t.Errorf("authz status %d: got %d, want 503", status, rec.Code)
		}
		if st.calls != 0 {
			t.Errorf("authz status %d: request reached the store", status)
		}
	}
}

func TestActions_WriteChecksManage_ReadChecksRead(t *testing.T) {
	st := &recordingStore{}
	az := newAuthz(t, 200, "GRANTED")
	srv := newServer(t, st, az)
	do(t, srv, "POST", writePath, "", nil)
	if !strings.Contains(az.lastBody(), handler.ActionLineageManage) {
		t.Errorf("write authorized %q, want %s", az.lastBody(), handler.ActionLineageManage)
	}
	do(t, srv, "GET", readPath, "", nil)
	if !strings.Contains(az.lastBody(), handler.ActionLineageRead) {
		t.Errorf("read authorized %q, want %s", az.lastBody(), handler.ActionLineageRead)
	}
}

// The tenant the store receives is the verified header's. A query parameter
// must not be able to choose it.
func TestTenantPassedToStore_IsTheHeaders(t *testing.T) {
	st := &recordingStore{}
	az := newAuthz(t, 200, "GRANTED")
	rec := do(t, newServer(t, st, az), "GET", readPath+"?tenant_id=99999999-9999-9999-9999-999999999999", "", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body %s", rec.Code, rec.Body.String())
	}
	if st.tenant != tenantA {
		t.Fatalf("store got tenant %q, want the header's %q", st.tenant, tenantA)
	}
}

func TestIdempotencyClaim_CarriesPrincipalKeyAndOperation(t *testing.T) {
	st := &recordingStore{}
	az := newAuthz(t, 200, "GRANTED")
	do(t, newServer(t, st, az), "POST", writePath, "", map[string]string{"Idempotency-Key": "the-key"})
	c := st.claim
	if c.PrincipalID != "tester" || c.Key != "the-key" || c.Operation != "SealManifest" || c.ResourceID != "ent_1" {
		t.Fatalf("claim = %+v", c)
	}
}

func TestStoreErrors_MapToStatus(t *testing.T) {
	cases := []struct {
		err  error
		want int
	}{
		{domain.ErrLineageEntityNotFound, http.StatusNotFound},
		{domain.ErrProvenanceManifestNotFound, http.StatusNotFound},
		{domain.ErrMissingTransformationVersion, http.StatusConflict},
		{domain.ErrLineageEdgeAlreadySuperseded, http.StatusConflict},
		{domain.ErrIdempotencyKeyReused, http.StatusConflict},
		// ZS-SVC-Z-001 DQC-04 / NP-31..NP-34
		{domain.ErrNoInputLineage, http.StatusConflict},
		{domain.ErrMissingRunProvenance, http.StatusConflict},
		{domain.ErrMissingVersionPin, http.StatusConflict},
		{domain.ErrRunProvenanceConflict, http.StatusConflict},
	}
	for _, tc := range cases {
		st := &recordingStore{err: tc.err}
		az := newAuthz(t, 200, "GRANTED")
		rec := do(t, newServer(t, st, az), "POST", writePath, "", nil)
		if rec.Code != tc.want {
			t.Errorf("%v: status = %d, want %d", tc.err, rec.Code, tc.want)
		}
	}
}

func TestIdempotentReplay_Is200WithHeader(t *testing.T) {
	st := &recordingStore{err: &domain.IdempotentReplayError{ResourceID: "res_prev"}}
	az := newAuthz(t, 200, "GRANTED")
	rec := do(t, newServer(t, st, az), "POST", writePath, "", nil)
	if rec.Code != http.StatusOK || rec.Header().Get("Idempotent-Replayed") != "true" {
		t.Fatalf("status %d header %q", rec.Code, rec.Header().Get("Idempotent-Replayed"))
	}
	if !strings.Contains(rec.Body.String(), "res_prev") {
		t.Fatalf("replay must return the original resource id: %s", rec.Body.String())
	}
}

func TestHealthz_NeedsNoTenantOrAuthz(t *testing.T) {
	st := &recordingStore{}
	az := newAuthz(t, 500, "DENIED")
	rec := do(t, newServer(t, st, az), "GET", "/healthz", "", map[string]string{"X-Tenant-Id": "", "X-Principal-Id": ""})
	if rec.Code != http.StatusOK {
		t.Fatalf("healthz = %d, want 200", rec.Code)
	}
}
