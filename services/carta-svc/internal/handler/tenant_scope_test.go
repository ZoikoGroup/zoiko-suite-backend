package handler_test

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// Tenant-scope tests for carta-svc (tracker row 8g).
//
// CARTA here is Continuous Adaptive Risk and Trust Assessment — a zero-trust
// access-scoring engine, not the cap-table company. domain.EvaluateAccess is
// a deterministic scoring function (device trust 30%, location 25%, resource
// sensitivity 25%, off-hours 20%) mapping a score to ALLOW / STEP_UP_MFA /
// ISOLATE / DENY.
//
// The store is correctly written: SaveAssessment stamps the tenant, both
// reads compare it, and subject_id narrows WITHIN the tenant rather than
// replacing it (unlike the self-disabling filters found across the connector
// services). The single defect was a getTenant helper returning
// "default-tenant" when X-Tenant-ID was absent — so the correct store
// enforced a synthetic tenant faithfully, and every header-less caller
// shared one bucket of access-decision telemetry.
//
// Why that bucket matters: an assessment records who requested access to
// what, from which IP, on what device, at what hour, and what was decided —
// with RiskFactors naming the reasoning in plain text. Because the scoring
// is deterministic, another tenant's assessments read as a map of how to
// pass their access checks: which subjects are on weak devices, which IPs
// they trust, and where their allow/deny boundary sits.
//
// Read exposure only — the store has no update or delete, so unlike
// esignature-integration-svc or mtls-management-svc there is no
// write-forgery angle here.
//
// No RLS dimension: this service has no migrations and no Postgres, so the
// handler plus the in-memory store is the entire boundary.

func evaluate(t *testing.T, r http.Handler, tenantID, subjectID string, deviceTrust int, knownLocation bool) string {
	t.Helper()
	body, _ := json.Marshal(map[string]interface{}{
		"legal_entity_id": "le-1",
		"context": map[string]interface{}{
			"subject_id":           subjectID,
			"subject_type":         "USER",
			"device_trust_level":   deviceTrust,
			"ip_address":           "203.0.113.10",
			"is_known_location":    knownLocation,
			"resource_sensitivity": "RESTRICTED",
			"action_requested":     "READ",
			"time_of_day_hour":     3,
		},
	})
	req := httptest.NewRequest(http.MethodPost, "/v1/carta/evaluate", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Tenant-ID", tenantID)
	req.Header.Set("X-Principal-Id", "principal-test-01")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusCreated {
		t.Fatalf("evaluate for %s: expected 201, got %d — %s", tenantID, w.Code, w.Body.String())
	}
	var asm struct {
		ID       string `json:"id"`
		TenantID string `json:"tenant_id"`
	}
	if err := json.NewDecoder(w.Body).Decode(&asm); err != nil {
		t.Fatalf("decode assessment: %v", err)
	}
	if asm.TenantID != tenantID {
		t.Fatalf("assessment attributed to %q, expected %q", asm.TenantID, tenantID)
	}
	return asm.ID
}

func TestMissingTenantHeader_Refused(t *testing.T) {
	r := newRouter()

	evalBody, _ := json.Marshal(map[string]interface{}{
		"legal_entity_id": "le-1",
		"context": map[string]interface{}{
			"subject_id": "user-1", "subject_type": "USER",
			"device_trust_level": 90, "is_known_location": true,
			"resource_sensitivity": "LOW", "action_requested": "READ",
			"time_of_day_hour": 12,
		},
	})

	for _, tc := range []struct {
		name, method, path string
		body               []byte
	}{
		{"evaluate access", http.MethodPost, "/v1/carta/evaluate", evalBody},
		{"list assessments", http.MethodGet, "/v1/carta/assessments", nil},
		{"get assessment", http.MethodGet, "/v1/carta/assessments/some-id", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var req *http.Request
			if tc.body != nil {
				req = httptest.NewRequest(tc.method, tc.path, bytes.NewReader(tc.body))
				req.Header.Set("Content-Type", "application/json")
			} else {
				req = httptest.NewRequest(tc.method, tc.path, nil)
			}
			// Deliberately NO X-Tenant-ID.
			w := httptest.NewRecorder()
			r.ServeHTTP(w, req)

			if w.Code != http.StatusUnauthorized {
				t.Fatalf("expected 401 with no X-Tenant-ID, got %d: %s", w.Code, w.Body.String())
			}
			if bytes.Contains(w.Body.Bytes(), []byte("default-tenant")) {
				t.Fatalf("response must never mention a fabricated default tenant: %s", w.Body.String())
			}
		})
	}
}

// TestForeignTenant_CannotReadAssessment covers the by-id read. It must
// answer 404, not 403 — a distinct forbidden would confirm to a prober that
// another tenant's assessment id exists.
func TestForeignTenant_CannotReadAssessment(t *testing.T) {
	r := newRouter()

	// A deliberately weak request, so the assessment carries RiskFactors
	// naming exactly why it scored badly — the part that reads as a map of
	// how to pass tenant-a's checks.
	asmID := evaluate(t, r, "tenant-a", "tenant-a-exec", 20, false)

	req := httptest.NewRequest(http.MethodGet, "/v1/carta/assessments/"+asmID, nil)
	req.Header.Set("X-Tenant-ID", "tenant-b")
	req.Header.Set("X-Principal-Id", "principal-test-01")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusNotFound {
		t.Fatalf("expected 404 for another tenant's assessment, got %d: %s", w.Code, w.Body.String())
	}
	for _, needle := range []string{"tenant-a-exec", "Untrusted device", "unknown IP"} {
		if bytes.Contains(w.Body.Bytes(), []byte(needle)) {
			t.Fatalf("ISOLATION FAILURE: tenant-b's response leaked %q: %s", needle, w.Body.String())
		}
	}

	// Sanity: tenant-a still reads its own, and the risk reasoning is there
	// (proving the assertions above are testing real content, not an
	// assessment that happens to carry no RiskFactors at all).
	ownReq := httptest.NewRequest(http.MethodGet, "/v1/carta/assessments/"+asmID, nil)
	ownReq.Header.Set("X-Tenant-ID", "tenant-a")
	ownReq.Header.Set("X-Principal-Id", "principal-test-01")
	ownW := httptest.NewRecorder()
	r.ServeHTTP(ownW, ownReq)
	if ownW.Code != http.StatusOK {
		t.Fatalf("tenant-a must still read its own assessment, got %d: %s", ownW.Code, ownW.Body.String())
	}
	if !bytes.Contains(ownW.Body.Bytes(), []byte("Untrusted device")) {
		t.Fatalf("expected tenant-a's own assessment to carry RiskFactors, got: %s", ownW.Body.String())
	}
}

// TestForeignTenant_CannotListOthersAssessments covers the unfiltered list —
// the call that would have returned every tenant's access telemetry.
func TestForeignTenant_CannotListOthersAssessments(t *testing.T) {
	r := newRouter()

	evaluate(t, r, "tenant-a", "tenant-a-exec", 20, false)
	evaluate(t, r, "tenant-b", "tenant-b-analyst", 95, true)

	req := httptest.NewRequest(http.MethodGet, "/v1/carta/assessments?legal_entity_id=le-1", nil)
	req.Header.Set("X-Tenant-ID", "tenant-b")
	req.Header.Set("X-Principal-Id", "principal-test-01")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	if bytes.Contains(w.Body.Bytes(), []byte("tenant-a-exec")) {
		t.Fatalf("ISOLATION FAILURE: tenant-b's list contained tenant-a's subject: %s", w.Body.String())
	}

	var resp struct {
		Count int `json:"count"`
	}
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Count != 1 {
		t.Fatalf("expected tenant-b to see exactly its own 1 assessment, got %d", resp.Count)
	}
}

// TestSubjectFilter_NarrowsWithinTenant_DoesNotReplaceIt guards the store's
// filter ordering. subject_id is an optional narrowing INSIDE the tenant; if
// it were ever rewritten as the only predicate (the self-disabling shape
// found across the six connector services), supplying another tenant's
// subject_id would return their assessments.
func TestSubjectFilter_NarrowsWithinTenant_DoesNotReplaceIt(t *testing.T) {
	r := newRouter()

	evaluate(t, r, "tenant-a", "shared-subject-id", 20, false)
	evaluate(t, r, "tenant-b", "shared-subject-id", 95, true)

	// Both tenants assessed a subject with the SAME id. Filtering by it as
	// tenant-b must return only tenant-b's row.
	req := httptest.NewRequest(http.MethodGet, "/v1/carta/assessments?legal_entity_id=le-1&subject_id=shared-subject-id", nil)
	req.Header.Set("X-Tenant-ID", "tenant-b")
	req.Header.Set("X-Principal-Id", "principal-test-01")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	var resp struct {
		Count int `json:"count"`
		Data  []struct {
			TenantID   string  `json:"tenant_id"`
			TrustScore float64 `json:"trust_score"`
		} `json:"data"`
	}
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Count != 1 {
		t.Fatalf("expected 1 assessment for tenant-b, got %d: %+v", resp.Count, resp.Data)
	}
	if resp.Data[0].TenantID != "tenant-b" {
		t.Fatalf("ISOLATION FAILURE: subject filter crossed tenants, got %q", resp.Data[0].TenantID)
	}
}

// TestTwoHeaderlessCallersDoNotShareANamespace is the direct regression test
// for the old behaviour: both callers became "default-tenant", so the second
// could read the first's access telemetry.
func TestTwoHeaderlessCallersDoNotShareANamespace(t *testing.T) {
	r := newRouter()

	body, _ := json.Marshal(map[string]interface{}{
		"legal_entity_id": "le-1",
		"context": map[string]interface{}{
			"subject_id": "shared-bucket-subject", "subject_type": "USER",
			"device_trust_level": 20, "is_known_location": false,
			"resource_sensitivity": "RESTRICTED", "action_requested": "READ",
			"time_of_day_hour": 3,
		},
	})
	req := httptest.NewRequest(http.MethodPost, "/v1/carta/evaluate", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("header-less evaluate must be refused, got %d: %s", w.Code, w.Body.String())
	}

	listReq := httptest.NewRequest(http.MethodGet, "/v1/carta/assessments?legal_entity_id=le-1", nil)
	listW := httptest.NewRecorder()
	r.ServeHTTP(listW, listReq)
	if listW.Code != http.StatusUnauthorized {
		t.Fatalf("header-less list must be refused, got %d: %s", listW.Code, listW.Body.String())
	}
	if bytes.Contains(listW.Body.Bytes(), []byte("shared-bucket-subject")) {
		t.Fatalf("ISOLATION FAILURE: a header-less caller saw another header-less caller's assessment: %s", listW.Body.String())
	}
}
