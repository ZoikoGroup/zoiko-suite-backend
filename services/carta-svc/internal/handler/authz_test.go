package handler_test

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"zoiko.io/carta-svc/internal/authz"
)

// Authorization tests for carta-svc (tracker row 93).
//
// Priority 1b gave this service a verified TENANT. That left an intra-tenant
// gap: any principal holding any valid envelope for the tenant could read
// every access assessment in it. Since domain.EvaluateAccess is a
// deterministic scoring function and RiskFactors names the reasoning in plain
// text, those records read as a map of how to pass the tenant's access
// checks — which subjects are on weak devices, which IPs it trusts, where the
// ALLOW boundary sits.
//
// Two of three routes are guarded. POST /evaluate is NOT, deliberately, and
// TestEvaluateStillWorksWithoutPrincipal is the guard rail on that decision.

func evaluateAs(t *testing.T, r http.Handler, tenantID, subjectID string) string {
	t.Helper()
	body, _ := json.Marshal(map[string]interface{}{
		"legal_entity_id": "le-1",
		"context": map[string]interface{}{
			"subject_id":           subjectID,
			"subject_type":         "USER",
			"device_trust_level":   20,
			"ip_address":           "203.0.113.10",
			"is_known_location":    false,
			"resource_sensitivity": "RESTRICTED",
			"action_requested":     "READ",
			"time_of_day_hour":     3,
		},
	})
	req := httptest.NewRequest(http.MethodPost, "/v1/carta/evaluate", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Tenant-ID", tenantID)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusCreated {
		t.Fatalf("evaluate: expected 201, got %d — %s", w.Code, w.Body.String())
	}
	var asm struct {
		ID string `json:"id"`
	}
	if err := json.NewDecoder(w.Body).Decode(&asm); err != nil {
		t.Fatalf("decode assessment: %v", err)
	}
	return asm.ID
}

// TestEvaluateStillWorksWithoutPrincipal is the most important test in this
// file, because it protects a deliberate ABSENCE.
//
// gateway-auth-svc calls POST /v1/carta/evaluate with X-Tenant-ID and nothing
// else, passing the principal it is asking ABOUT as context.subject_id in the
// body. It is called during authentication, to decide whether an access
// request should be allowed, stepped up, isolated or denied.
//
// Requiring the subject to prove it is authorized before the platform will
// decide whether it is trustworthy inverts the dependency — the answer would
// be needed in order to ask the question. If someone later adds
// requirePrincipal here "for consistency", this test fails instead of the
// platform's authentication path silently breaking.
func TestEvaluateStillWorksWithoutPrincipal(t *testing.T) {
	r := newRouter()

	body, _ := json.Marshal(map[string]interface{}{
		"legal_entity_id": "le-1",
		"context": map[string]interface{}{
			"subject_id": "subject-being-scored", "subject_type": "USER",
			"device_trust_level": 90, "is_known_location": true,
			"resource_sensitivity": "LOW", "action_requested": "READ",
			"time_of_day_hour": 12,
		},
	})
	req := httptest.NewRequest(http.MethodPost, "/v1/carta/evaluate", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Tenant-ID", "t1")
	// Deliberately NO X-Principal-Id — this is how gateway-auth-svc calls it.
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusCreated {
		t.Fatalf("POST /evaluate must work with a tenant header alone (gateway-auth-svc calls it during authentication), got %d: %s",
			w.Code, w.Body.String())
	}
}

// TestMissingPrincipal_RefusedOnReads covers the two guarded routes.
func TestMissingPrincipal_RefusedOnReads(t *testing.T) {
	r := newRouter()
	asmID := evaluateAs(t, r, "t1", "subject-1")

	for _, tc := range []struct {
		name, path string
	}{
		{"list assessments", "/v1/carta/assessments?legal_entity_id=le-1"},
		{"get assessment", "/v1/carta/assessments/" + asmID},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, tc.path, nil)
			req.Header.Set("X-Tenant-ID", "t1")
			// Deliberately NO X-Principal-Id.
			w := httptest.NewRecorder()
			r.ServeHTTP(w, req)

			if w.Code != http.StatusUnauthorized {
				t.Fatalf("expected 401 with no principal, got %d: %s", w.Code, w.Body.String())
			}
		})
	}
}

// TestAuthzDenied_RefusedOnReads proves the check is reached. The assessment
// is created with a granting stub and the decision flipped afterwards, so the
// row genuinely exists and a 404 cannot masquerade as a refusal.
func TestAuthzDenied_RefusedOnReads(t *testing.T) {
	az := &stubAuthz{}
	r := newRouterWithAuthz(az)
	asmID := evaluateAs(t, r, "t1", "subject-1")

	az.err = authz.ErrAuthorizationDenied

	for _, tc := range []struct {
		name, path string
	}{
		{"list assessments", "/v1/carta/assessments?legal_entity_id=le-1"},
		{"get assessment", "/v1/carta/assessments/" + asmID},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, tc.path, nil)
			req.Header.Set("X-Tenant-ID", "t1")
			req.Header.Set("X-Principal-Id", "principal-denied")
			w := httptest.NewRecorder()
			r.ServeHTTP(w, req)

			if w.Code != http.StatusForbidden {
				t.Fatalf("expected 403 when authorization is DENIED, got %d: %s", w.Code, w.Body.String())
			}
			if bytes.Contains(w.Body.Bytes(), []byte("Untrusted device")) {
				t.Fatalf("ISOLATION FAILURE: a denied principal received the risk reasoning: %s", w.Body.String())
			}
		})
	}
}

// TestEvaluateUnaffectedByAuthzDenial is the other half of the guard rail: a
// denying authorization service must not break the authentication path, since
// /evaluate never consults it.
func TestEvaluateUnaffectedByAuthzDenial(t *testing.T) {
	r := newRouterWithAuthz(&stubAuthz{err: authz.ErrAuthorizationDenied})

	body, _ := json.Marshal(map[string]interface{}{
		"legal_entity_id": "le-1",
		"context": map[string]interface{}{
			"subject_id": "subject-1", "subject_type": "USER",
			"device_trust_level": 90, "is_known_location": true,
			"resource_sensitivity": "LOW", "action_requested": "READ",
			"time_of_day_hour": 12,
		},
	})
	req := httptest.NewRequest(http.MethodPost, "/v1/carta/evaluate", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Tenant-ID", "t1")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusCreated {
		t.Fatalf("/evaluate must not depend on authorization-svc, got %d: %s", w.Code, w.Body.String())
	}
}

// TestAuthzUnavailable_FailsClosedOnReads pins the posture on the guarded
// routes: unreachable authorization-svc is a 503, not a silent success.
func TestAuthzUnavailable_FailsClosedOnReads(t *testing.T) {
	r := newRouterWithAuthz(&stubAuthz{err: authz.ErrAuthzServiceUnavailable})

	req := httptest.NewRequest(http.MethodGet, "/v1/carta/assessments?legal_entity_id=le-1", nil)
	req.Header.Set("X-Tenant-ID", "t1")
	req.Header.Set("X-Principal-Id", "principal-test-01")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503 when authorization-svc is unreachable, got %d: %s", w.Code, w.Body.String())
	}
}

// TestListRequiresLegalEntity pins the API tightening.
func TestListRequiresLegalEntity(t *testing.T) {
	r := newRouter()
	evaluateAs(t, r, "t1", "subject-1")

	req := httptest.NewRequest(http.MethodGet, "/v1/carta/assessments", nil)
	req.Header.Set("X-Tenant-ID", "t1")
	req.Header.Set("X-Principal-Id", "principal-test-01")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 listing with no legal_entity_id, got %d: %s", w.Code, w.Body.String())
	}
	if bytes.Contains(w.Body.Bytes(), []byte("subject-1")) {
		t.Fatalf("a rejected listing must not return assessments: %s", w.Body.String())
	}
}
