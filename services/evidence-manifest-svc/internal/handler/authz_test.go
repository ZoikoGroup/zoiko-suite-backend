package handler_test

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-chi/chi/v5"
	"go.uber.org/zap"

	"zoiko.io/evidence-manifest-svc/internal/authz"
	"zoiko.io/evidence-manifest-svc/internal/domain"
	"zoiko.io/evidence-manifest-svc/internal/handler"
	svcmiddleware "zoiko.io/evidence-manifest-svc/internal/middleware"
)

// Authorization tests for evidence-manifest-svc (tracker row 87).
//
// Until this change the service had NO authorization at all — no
// internal/authz package, no check on any route. Tenant isolation (row 14)
// was already in place and holds, so this was never a cross-tenant hole. It
// was an intra-tenant privilege gap: any principal holding any valid
// envelope for the tenant could read any manifest in it.
//
// That matters here more than in most services because ListRecords returns
// record_snapshot — verbatim governance decisions, access decisions and
// workflow instances as they stood at generation time. That is the
// assembled evidence bundle itself, the artefact handed to an auditor or
// regulator, not metadata about it. "Any principal in the tenant" is the
// wrong audience for it.
//
// The two things these tests pin that a behavioural test would not:
//
//  1. Authorization is actually CONSULTED on every route (a granting stub
//     makes every existing test pass whether or not the call happens, so
//     only a DENYING stub proves the call is wired).
//  2. The failure posture is fail-closed in both directions — denied is
//     403, unreachable is 503. An authz client that returned nil on a
//     transport error would be worse than no authz at all.

func routerWithAuthz(t *testing.T, az handler.AuthzChecker) chi.Router {
	t.Helper()
	gov, acc, wf, pub := scopeTestSources()
	r := chi.NewRouter()
	r.Use(svcmiddleware.TenantContext())
	h := handler.New(newStubStore(), gov, acc, wf, &stubWorkflowHistory{}, pub, az, zap.NewNop())
	handler.RegisterRoutes(r, h)
	return r
}

func generateBody(t *testing.T, tenantID string) []byte {
	t.Helper()
	b, err := json.Marshal(domain.GenerateManifestRequest{
		TenantID:              tenantID,
		LegalEntityID:         "e1",
		ScenarioType:          domain.ScenarioAudit,
		GovernanceDecisionIDs: []string{"gd-1"},
	})
	if err != nil {
		t.Fatalf("marshal request: %v", err)
	}
	return b
}

// TestMissingPrincipal_Refused covers the fabricated-actor fix.
//
// actorFromHeader previously fell back to the literal string "unknown",
// so a request with no principal header SUCCEEDED and wrote "unknown" into
// evidence_manifests.requested_by — a NOT NULL column satisfied with no
// accountability, on an artefact whose entire purpose is attribution. Same
// anti-pattern as the "default-tenant" fabrication removed across 15
// services in Priority 1b, applied to the actor instead.
func TestMissingPrincipal_Refused(t *testing.T) {
	r := routerWithAuthz(t, &stubAuthz{})

	for _, tc := range []struct {
		name, method, path string
		body               []byte
	}{
		{"generate manifest", http.MethodPost, "/v1/evidence-manifests", generateBody(t, "t1")},
		{"get manifest", http.MethodGet, "/v1/evidence-manifests/some-id", nil},
		{"list records", http.MethodGet, "/v1/evidence-manifests/some-id/records", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var req *http.Request
			if tc.body != nil {
				req = httptest.NewRequest(tc.method, tc.path, bytes.NewReader(tc.body))
				req.Header.Set("Content-Type", "application/json")
			} else {
				req = httptest.NewRequest(tc.method, tc.path, nil)
			}
			req.Header.Set("X-Tenant-Id", "t1")
			// Deliberately NO principal header, in either spelling.
			w := httptest.NewRecorder()
			r.ServeHTTP(w, req)

			if w.Code != http.StatusUnauthorized {
				t.Fatalf("expected 401 with no principal header, got %d: %s", w.Code, w.Body.String())
			}
			if bytes.Contains(w.Body.Bytes(), []byte("unknown")) {
				t.Fatalf("response must never mention a fabricated \"unknown\" actor: %s", w.Body.String())
			}
		})
	}
}

// TestAuthzDenied_Refused proves the authorization call is actually wired on
// every route. With a granting stub every route behaves as before, so only a
// denying stub can distinguish "authorized" from "never asked".
func TestAuthzDenied_Refused(t *testing.T) {
	r := routerWithAuthz(t, &stubAuthz{err: authz.ErrAuthorizationDenied})

	for _, tc := range []struct {
		name, method, path string
		body               []byte
	}{
		{"generate manifest", http.MethodPost, "/v1/evidence-manifests", generateBody(t, "t1")},
		{"get manifest", http.MethodGet, "/v1/evidence-manifests/some-id", nil},
		{"list records", http.MethodGet, "/v1/evidence-manifests/some-id/records", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var req *http.Request
			if tc.body != nil {
				req = httptest.NewRequest(tc.method, tc.path, bytes.NewReader(tc.body))
				req.Header.Set("Content-Type", "application/json")
			} else {
				req = httptest.NewRequest(tc.method, tc.path, nil)
			}
			req.Header.Set("X-Tenant-Id", "t1")
			req.Header.Set("X-Principal-Id", "principal-denied")
			w := httptest.NewRecorder()
			r.ServeHTTP(w, req)

			// The two GET routes fetch the manifest before authorizing, so a
			// nonexistent id yields 404 before the authz check is reached —
			// that ordering is deliberate (authorize against the row's own
			// legal entity, never a caller-supplied scope). Either answer
			// proves the request did not succeed; what must never happen is
			// 200.
			if w.Code == http.StatusOK {
				t.Fatalf("ISOLATION FAILURE: route returned 200 while authorization was DENIED: %s", w.Body.String())
			}
			if tc.method == http.MethodPost && w.Code != http.StatusForbidden {
				t.Fatalf("expected 403 on a denied write, got %d: %s", w.Code, w.Body.String())
			}
		})
	}
}

// TestAuthzDenied_OnExistingManifest_Is403 is the sharper version of the
// above for reads: seed a real manifest with a granting client, then read it
// back with a denying one. Now the row exists, so a 404 cannot mask the
// result and the assertion is exactly 403.
func TestAuthzDenied_OnExistingManifest_Is403(t *testing.T) {
	gov, acc, wf, pub := scopeTestSources()
	store := newStubStore()

	granting := &stubAuthz{}
	r := chi.NewRouter()
	r.Use(svcmiddleware.TenantContext())
	h := handler.New(store, gov, acc, wf, &stubWorkflowHistory{}, pub, granting, zap.NewNop())
	handler.RegisterRoutes(r, h)

	seedReq := httptest.NewRequest(http.MethodPost, "/v1/evidence-manifests", bytes.NewReader(generateBody(t, "t1")))
	seedReq.Header.Set("Content-Type", "application/json")
	seedReq.Header.Set("X-Tenant-Id", "t1")
	seedReq.Header.Set("X-Principal-Id", "principal-seed")
	seedW := httptest.NewRecorder()
	r.ServeHTTP(seedW, seedReq)
	if seedW.Code != http.StatusCreated {
		t.Fatalf("seed manifest: expected 201, got %d — %s", seedW.Code, seedW.Body.String())
	}
	var m domain.EvidenceManifest
	if err := json.NewDecoder(seedW.Body).Decode(&m); err != nil {
		t.Fatalf("decode seeded manifest: %v", err)
	}

	// Same handler, same store, same tenant — only the decision changes.
	granting.err = authz.ErrAuthorizationDenied

	for _, path := range []string{
		"/v1/evidence-manifests/" + m.ManifestID,
		"/v1/evidence-manifests/" + m.ManifestID + "/records",
	} {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		req.Header.Set("X-Tenant-Id", "t1")
		req.Header.Set("X-Principal-Id", "principal-denied")
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)

		if w.Code != http.StatusForbidden {
			t.Fatalf("%s: expected 403 for a denied principal on an EXISTING manifest, got %d: %s",
				path, w.Code, w.Body.String())
		}
		if bytes.Contains(w.Body.Bytes(), []byte("record_snapshot")) {
			t.Fatalf("ISOLATION FAILURE: %s leaked evidence to a denied principal: %s", path, w.Body.String())
		}
	}
}

// TestAuthzUnavailable_FailsClosed covers the posture this change
// introduces, and is the reason it is a real behaviour change rather than a
// free addition: a read that works today starts returning 503 during an
// authorization-svc outage. That is correct for governed evidence (Doc 03
// §22 has evidence fail safe) but it must be deliberate, and it must be 503
// rather than a silent success.
func TestAuthzUnavailable_FailsClosed(t *testing.T) {
	r := routerWithAuthz(t, &stubAuthz{err: authz.ErrAuthzServiceUnavailable})

	req := httptest.NewRequest(http.MethodPost, "/v1/evidence-manifests", bytes.NewReader(generateBody(t, "t1")))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Tenant-Id", "t1")
	req.Header.Set("X-Principal-Id", "principal-test-01")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503 when authorization-svc is unreachable, got %d: %s", w.Code, w.Body.String())
	}
}
