package handler_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sync"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"go.uber.org/zap"

	authzpkg "zoiko.io/privacy-purpose-registry-svc/internal/authz"
	"zoiko.io/privacy-purpose-registry-svc/internal/domain"
	"zoiko.io/privacy-purpose-registry-svc/internal/handler"
	"zoiko.io/privacy-purpose-registry-svc/internal/middleware"
)

const (
	tenantA  = "11111111-1111-1111-1111-111111111111"
	tenantB  = "22222222-2222-2222-2222-222222222222"
	platform = domain.PlatformScopeID

	maker   = "maker-01"
	checker = "checker-01"
	ops     = "ops-01"
)

// ── authorization stub ───────────────────────────────────────────────────────

type authzCall struct{ principal, scope, action string }

// stubAuthz grants by default. It records every check so tests can assert
// WHICH action and scope were asked about, and can be flipped mid-test to model
// a revoked grant or an unreachable authorization-svc.
type stubAuthz struct {
	mu          sync.Mutex
	deny        bool
	denyActions map[string]bool
	unavailable bool
	calls       []authzCall
}

func (a *stubAuthz) CheckAllowed(_ context.Context, principal, scope, action string) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.calls = append(a.calls, authzCall{principal, scope, action})
	if a.unavailable {
		return authzpkg.ErrAuthzServiceUnavailable
	}
	if a.deny || a.denyActions[action] {
		return authzpkg.ErrAuthorizationDenied
	}
	return nil
}

func (a *stubAuthz) last() authzCall {
	a.mu.Lock()
	defer a.mu.Unlock()
	if len(a.calls) == 0 {
		return authzCall{}
	}
	return a.calls[len(a.calls)-1]
}

func (a *stubAuthz) reset() { a.mu.Lock(); a.calls = nil; a.mu.Unlock() }

// ── router & requests ────────────────────────────────────────────────────────

func newRouter(st *stubStore, az *stubAuthz) chi.Router {
	h := handler.New(st, az, zap.NewNop())
	r := chi.NewRouter()
	r.Use(middleware.TenantContext())
	handler.RegisterRoutes(r, h)
	return r
}

// call issues a request. Mutating requests get a fresh Idempotency-Key and
// X-Correlation-ID unless a header override says otherwise; an override value
// of "" DELETES the header (so tests can prove a header is mandatory).
func call(r http.Handler, method, path string, body interface{}, tenant, principal string, hdr ...map[string]string) *httptest.ResponseRecorder {
	var buf bytes.Buffer
	if body != nil {
		_ = json.NewEncoder(&buf).Encode(body)
	}
	req := httptest.NewRequest(method, path, &buf)
	req.Header.Set("Content-Type", "application/json")
	if principal != "" {
		req.Header.Set("X-Principal-Id", principal)
	}
	if tenant != "" {
		req.Header.Set("X-Tenant-Id", tenant)
	}
	if method != http.MethodGet {
		req.Header.Set("X-Correlation-ID", "corr-"+uuid.NewString())
		req.Header.Set("Idempotency-Key", "idem-"+uuid.NewString())
	}
	for _, h := range hdr {
		for k, v := range h {
			if v == "" {
				req.Header.Del(k)
			} else {
				req.Header.Set(k, v)
			}
		}
	}
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

type apiError struct {
	Error     string                     `json:"error"`
	ErrorCode string                     `json:"error_code"`
	Findings  []domain.ValidationFinding `json:"findings"`
}

// wantErr asserts status and the stable §32 error_code of an error response.
func wantErr(t *testing.T, w *httptest.ResponseRecorder, status int, code string) apiError {
	t.Helper()
	if w.Code != status {
		t.Fatalf("want HTTP %d, got %d: %s", status, w.Code, w.Body.String())
	}
	var e apiError
	if err := json.Unmarshal(w.Body.Bytes(), &e); err != nil {
		t.Fatalf("error body is not JSON: %v: %s", err, w.Body.String())
	}
	if e.ErrorCode != code {
		t.Fatalf("want error_code %q, got %q (%s)", code, e.ErrorCode, e.Error)
	}
	if e.Error == "" {
		t.Fatalf("error response has no human-readable message: %s", w.Body.String())
	}
	return e
}

func wantOK(t *testing.T, w *httptest.ResponseRecorder, status int) {
	t.Helper()
	if w.Code != status {
		t.Fatalf("want HTTP %d, got %d: %s", status, w.Code, w.Body.String())
	}
}

func decode[T any](t *testing.T, w *httptest.ResponseRecorder) T {
	t.Helper()
	var v T
	if err := json.Unmarshal(w.Body.Bytes(), &v); err != nil {
		t.Fatalf("decode: %v: %s", err, w.Body.String())
	}
	return v
}

// ── fixtures ─────────────────────────────────────────────────────────────────

const (
	purposesPath   = "/privacy/purposes"
	activitiesPath = "/privacy/processing-activities"
)

// newPurpose creates and PUBLISHES a purpose in the given scope: created by
// maker, published by checker (segregation of duties). It carries a necessity
// rationale and a lawful basis, so it satisfies gates 3 and 4.
func newPurpose(t *testing.T, r http.Handler, scope string, mut ...func(*domain.CreatePurposeRequest)) *domain.PurposeVersion {
	t.Helper()
	req := domain.CreatePurposeRequest{
		Statement:          "Send transactional emails about invoice status",
		CompatibilityClass: "PRIMARY",
		LawfulBasisRefs:    []string{"basis-contract-performance"},
		NecessityRationale: "Only the email address is needed to deliver the notice",
	}
	for _, m := range mut {
		m(&req)
	}
	w := call(r, http.MethodPost, purposesPath, req, scope, maker)
	wantOK(t, w, http.StatusCreated)
	v := decode[domain.PurposeVersion](t, w)
	w = call(r, http.MethodPost, purposesPath+"/"+v.PurposeID+"/versions/"+v.PurposeVersionID+"/publish", nil, scope, checker)
	wantOK(t, w, http.StatusOK)
	return ptr(decode[domain.PurposeVersion](t, w))
}

func ptr[T any](v T) *T { return &v }

// goodActivity is a request that satisfies every §8.2 gate for purposeIDs.
func goodActivity(purposeIDs ...string) domain.CreateActivityRequest {
	return domain.CreateActivityRequest{
		PrivacyRole:             string(domain.RoleController),
		Owner:                   "head-of-billing",
		PrivacyOwner:            "dpo",
		PurposeIDs:              purposeIDs,
		SubjectClasses:          []string{"CUSTOMER"},
		DataCategories:          []string{"CONTACT_INFO"},
		Sources:                 []string{"signup-form"},
		Recipients:              []string{"INTERNAL_ONLY"},
		Jurisdictions:           []string{"GB"},
		RetentionRuleRefs:       []string{"retention-rule-7y"},
		NoticeConsentDependency: string(domain.NoticeConsentRequired),
		DPIATIAStatus:           string(domain.DPIATIAResolved),
	}
}

func newDraft(t *testing.T, r http.Handler, scope string, req domain.CreateActivityRequest) *domain.ProcessingActivityVersion {
	t.Helper()
	w := call(r, http.MethodPost, activitiesPath, req, scope, maker)
	wantOK(t, w, http.StatusCreated)
	return ptr(decode[domain.ProcessingActivityVersion](t, w))
}

func versionPath(v *domain.ProcessingActivityVersion, verb string) string {
	return activitiesPath + "/" + v.ActivityID + "/versions/" + v.ActivityVersionID + "/" + verb
}

// step posts a lifecycle verb as principal and decodes the resulting version.
func step(t *testing.T, r http.Handler, scope string, v *domain.ProcessingActivityVersion, verb, principal string, body interface{}, want int) *domain.ProcessingActivityVersion {
	t.Helper()
	w := call(r, http.MethodPost, versionPath(v, verb), body, scope, principal)
	wantOK(t, w, want)
	return ptr(decode[domain.ProcessingActivityVersion](t, w))
}

// toApproved drives a draft DRAFT -> VALIDATED -> SUBMITTED -> APPROVED.
func toApproved(t *testing.T, r http.Handler, scope string, v *domain.ProcessingActivityVersion) *domain.ProcessingActivityVersion {
	t.Helper()
	v = step(t, r, scope, v, "validate", maker, nil, http.StatusOK)
	if v.VersionStatus != domain.ActivityStatusValidated {
		t.Fatalf("expected VALIDATED, got %s (findings %+v)", v.VersionStatus, v.ValidationFindings)
	}
	step(t, r, scope, v, "submit", maker, nil, http.StatusOK)
	return step(t, r, scope, v, "approve", checker, nil, http.StatusOK)
}

// toActive drives a draft all the way to ACTIVE.
func toActive(t *testing.T, r http.Handler, scope string, v *domain.ProcessingActivityVersion) *domain.ProcessingActivityVersion {
	t.Helper()
	v = toApproved(t, r, scope, v)
	return step(t, r, scope, v, "activate", ops, nil, http.StatusOK)
}

// versionURL is GET .../versions/{id} for a version.
func versionURL(v *domain.ProcessingActivityVersion) string {
	return activitiesPath + "/" + v.ActivityID + "/versions/" + v.ActivityVersionID
}

// sameJSON reports whether two response bodies encode the same JSON value.
// An idempotent replay is read back from a JSONB column, which normalises key
// order and whitespace: the replay is the same object, not the same bytes.
func sameJSON(t *testing.T, a, b string) bool {
	t.Helper()
	var x, y any
	if err := json.Unmarshal([]byte(a), &x); err != nil {
		t.Fatalf("not JSON: %s", a)
	}
	if err := json.Unmarshal([]byte(b), &y); err != nil {
		t.Fatalf("not JSON: %s", b)
	}
	return reflect.DeepEqual(x, y)
}
