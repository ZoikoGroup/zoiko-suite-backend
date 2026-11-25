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

	"zoiko.io/data-quality-svc/internal/authz"
	"zoiko.io/data-quality-svc/internal/domain"
	"zoiko.io/data-quality-svc/internal/handler"
	"zoiko.io/data-quality-svc/internal/store"
)

// Handler-level tests. The store suite proves the database; these prove the
// HTTP layer in front of it: that a request with no verified tenant or
// principal never reaches the store, that authorization fails closed, that the
// tenant handed to the store is the header's and nothing else, and that store
// errors map to the status codes callers depend on.

const (
	tenantA = "11111111-1111-1111-1111-111111111111"
	hash64  = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
)

// recordingStore embeds the interface so only the methods a test exercises need
// bodies; anything else panics on a nil interface, which a test would surface.
type recordingStore struct {
	store.Store
	mu      sync.Mutex
	calls   int
	tenant  string
	claim   domain.IdempotencyClaim
	certReq domain.CertifyDQRequest
	err     error
}

func (s *recordingStore) note(tenant string, claim domain.IdempotencyClaim) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls++
	s.tenant = tenant
	s.claim = claim
}

func (s *recordingStore) RunDQ(_ context.Context, tenantID string, _ domain.RunDQRequest, _ string, claim domain.IdempotencyClaim) (*domain.DQRun, error) {
	s.note(tenantID, claim)
	if s.err != nil {
		return nil, s.err
	}
	return &domain.DQRun{RunID: "dqn_x", TenantID: tenantID}, nil
}

func (s *recordingStore) GetRun(_ context.Context, tenantID, runID string) (*domain.DQRun, error) {
	s.note(tenantID, domain.IdempotencyClaim{})
	if s.err != nil {
		return nil, s.err
	}
	return &domain.DQRun{RunID: runID, TenantID: tenantID}, nil
}

func (s *recordingStore) CertifyDQ(_ context.Context, tenantID, _, _ string, req domain.CertifyDQRequest, claim domain.IdempotencyClaim) (*domain.DQCertification, error) {
	s.note(tenantID, claim)
	s.certReq = req
	if s.err != nil {
		return nil, s.err
	}
	return &domain.DQCertification{CertificationID: "dqc_x", TenantID: tenantID}, nil
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

const runBody = `{"ruleset_version_id":"dqv_x","population_ref":"pop-1","population_row_count":10,"population_content_hash":"` + hash64 + `","rule_outcomes":[]}`

func TestNoTenant_RefusedBeforeAnythingElse(t *testing.T) {
	st := &recordingStore{}
	az := newAuthz(t, 200, "GRANTED")
	rec := do(t, newServer(t, st, az), "POST", "/v1/data-quality/runs", runBody, map[string]string{"X-Tenant-Id": ""})
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
	rec := do(t, newServer(t, st, az), "GET", "/v1/data-quality/runs/dqn_x", "", map[string]string{"X-Principal-Id": ""})
	if rec.Code != http.StatusUnauthorized && rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 401 (or 400 from the envelope)", rec.Code)
	}
	if st.calls != 0 {
		t.Fatalf("a request with no principal reached the store")
	}
}

func TestAuthzDenied_Is403_AndStoreUntouched(t *testing.T) {
	for _, tc := range []struct{ method, path, body string }{
		{"POST", "/v1/data-quality/runs", runBody},
		{"POST", "/v1/data-quality/runs/dqn_x:certify", ""},
		{"GET", "/v1/data-quality/runs/dqn_x", ""},
	} {
		st := &recordingStore{}
		az := newAuthz(t, 200, "DENIED")
		rec := do(t, newServer(t, st, az), tc.method, tc.path, tc.body, nil)
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
		rec := do(t, newServer(t, st, az), "POST", "/v1/data-quality/runs", runBody, nil)
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

	do(t, srv, "POST", "/v1/data-quality/runs", runBody, nil)
	if !strings.Contains(az.lastBody(), handler.ActionDQManage) {
		t.Errorf("write authorized %q, want %s", az.lastBody(), handler.ActionDQManage)
	}
	do(t, srv, "GET", "/v1/data-quality/runs/dqn_x", "", nil)
	if !strings.Contains(az.lastBody(), handler.ActionDQRead) {
		t.Errorf("read authorized %q, want %s", az.lastBody(), handler.ActionDQRead)
	}
}

// The tenant the store receives is the verified header's. There is no body
// field that can override it.
func TestTenantPassedToStore_IsTheHeaders(t *testing.T) {
	st := &recordingStore{}
	az := newAuthz(t, 200, "GRANTED")
	body := strings.Replace(runBody, `"rule_outcomes"`, `"tenant_id":"99999999-9999-9999-9999-999999999999","rule_outcomes"`, 1)
	rec := do(t, newServer(t, st, az), "POST", "/v1/data-quality/runs", body, nil)
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, body %s", rec.Code, rec.Body.String())
	}
	if st.tenant != tenantA {
		t.Fatalf("store got tenant %q, want the header's %q", st.tenant, tenantA)
	}
}

func TestIdempotencyClaim_CarriesPrincipalKeyAndOperation(t *testing.T) {
	st := &recordingStore{}
	az := newAuthz(t, 200, "GRANTED")
	do(t, newServer(t, st, az), "POST", "/v1/data-quality/runs/dqn_x:certify", "", map[string]string{"Idempotency-Key": "cert-key"})
	c := st.claim
	if c.PrincipalID != "tester" || c.Key != "cert-key" || c.Operation != "CertifyDQ" || c.ResourceID != "dqn_x" {
		t.Fatalf("claim = %+v", c)
	}
}

func TestStoreErrors_MapToStatus(t *testing.T) {
	cases := []struct {
		err  error
		want int
	}{
		{domain.ErrRunNotFound, http.StatusNotFound},
		{domain.ErrCertificationNotFound, http.StatusNotFound},
		{domain.ErrRunHasOpenIssues, http.StatusConflict},
		{domain.ErrRunAlreadyCertified, http.StatusConflict},
		{domain.ErrIdempotencyKeyReused, http.StatusConflict},
		{domain.ErrImmutableViolation, http.StatusConflict},
		// ZS-SVC-Z-001 INV-04 / INV-15
		{domain.ErrEvidenceIncomplete, http.StatusConflict},
		{domain.ErrEvidenceStale, http.StatusConflict},
		{domain.ErrCertificationBasisExists, http.StatusConflict},
		{domain.ErrUnknownRule, http.StatusBadRequest},
		{domain.ErrDuplicateRuleOutcome, http.StatusBadRequest},
	}
	for _, tc := range cases {
		st := &recordingStore{err: tc.err}
		az := newAuthz(t, 200, "GRANTED")
		rec := do(t, newServer(t, st, az), "POST", "/v1/data-quality/runs/dqn_x:certify", "", nil)
		if rec.Code != tc.want {
			t.Errorf("%v: status = %d, want %d", tc.err, rec.Code, tc.want)
		}
	}
}

func TestIdempotentReplay_Is200WithHeader(t *testing.T) {
	st := &recordingStore{err: &domain.IdempotentReplayError{ResourceID: "dqn_prev"}}
	az := newAuthz(t, 200, "GRANTED")
	rec := do(t, newServer(t, st, az), "POST", "/v1/data-quality/runs", runBody, nil)
	if rec.Code != http.StatusOK || rec.Header().Get("Idempotent-Replayed") != "true" {
		t.Fatalf("status %d header %q", rec.Code, rec.Header().Get("Idempotent-Replayed"))
	}
	if !strings.Contains(rec.Body.String(), "dqn_prev") {
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

// The optional certify body carries the lineage manifest reference. It must
// reach the store untouched, and it is part of the idempotency request hash so a
// retry that changes it cannot masquerade as a replay.
func TestCertify_LineageReferenceBodyReachesTheStore_AndIsHashedIntoTheClaim(t *testing.T) {
	st := &recordingStore{}
	az := newAuthz(t, 200, "GRANTED")
	body := `{"lineage_manifest_id":"dpm_aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa","lineage_manifest_sha256":"` + strings.Repeat("a", 64) + `"}`

	rec := do(t, newServer(t, st, az), "POST", "/v1/data-quality/runs/dqn_x:certify", body, nil)
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, body %s", rec.Code, rec.Body.String())
	}
	if st.certReq.LineageManifestID == nil || *st.certReq.LineageManifestID != "dpm_aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa" {
		t.Fatalf("store did not receive the lineage reference: %+v", st.certReq)
	}
	withBody := st.claim.RequestSHA256

	do(t, newServer(t, st, az), "POST", "/v1/data-quality/runs/dqn_x:certify", "", nil)
	if st.claim.RequestSHA256 == withBody {
		t.Fatal("the request hash ignores the body: changing the lineage reference would replay as the same request")
	}
	if st.certReq.LineageManifestID != nil {
		t.Fatalf("an empty body should carry no reference: %+v", st.certReq)
	}
}

func TestCertify_MalformedBody_Is400_AndStoreUntouched(t *testing.T) {
	st := &recordingStore{}
	az := newAuthz(t, 200, "GRANTED")
	rec := do(t, newServer(t, st, az), "POST", "/v1/data-quality/runs/dqn_x:certify", "{not json", nil)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
	if st.calls != 0 {
		t.Fatal("a malformed certify body reached the store")
	}
}
