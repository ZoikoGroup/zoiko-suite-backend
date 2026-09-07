package handler

// Tests for the two register endpoints, and for the cross-tenant reads this
// service used to perform.
//
// Both list endpoints are new. Before them this service could create a retention
// policy and resolve one record class at a time, and nothing could ask what rules
// a tenant was operating under or what holds were freezing its records — an
// operator's only route to a hold was already knowing its id.
//
// GET /v1/legal-holds/{id} had no principal check, no tenant scoping and no
// authorization, so anything that could reach the port could read any hold in any
// tenant by id. A hold names the authority that ordered the freeze, the matter,
// and the custodians holding the evidence.

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"zoiko.io/retention-registry-svc/internal/domain"
)

// seedHolds puts one ACTIVE hold in each of two tenants, plus one platform-wide
// hold that applies to both.
func seedHolds(st *stubStore) {
	tA, tB := tenantA, tenantB
	classA, classB := "FINANCIAL_LEDGER", "HR_RECORD"
	st.holds = append(st.holds,
		domain.LegalHold{
			LegalHoldID: "hold-a", ScopeDescription: "Tenant A matter", Authority: "HMRC enquiry",
			TenantID: &tA, RecordClass: &classA, HoldStatus: "ACTIVE",
		},
		domain.LegalHold{
			LegalHoldID: "hold-b", ScopeDescription: "Tenant B matter", Authority: "SEC subpoena",
			TenantID: &tB, RecordClass: &classB, HoldStatus: "ACTIVE",
		},
		domain.LegalHold{
			LegalHoldID: "hold-platform", ScopeDescription: "Platform-wide freeze", Authority: "Board directive",
			TenantID: nil, HoldStatus: "ACTIVE",
		},
	)
}

func TestListLegalHolds_ScopesToCallerTenantAndIncludesPlatformWide(t *testing.T) {
	h, st, _ := newTestHandler()
	seedHolds(st)
	r := newTestRouter(h)

	w := httptest.NewRecorder()
	r.ServeHTTP(w, buildRequestAs(http.MethodGet, "/v1/legal-holds/", nil, tenantA))
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d - %s", w.Code, w.Body.String())
	}

	var holds []domain.LegalHold
	if err := json.NewDecoder(w.Body).Decode(&holds); err != nil {
		t.Fatalf("decode register: %v", err)
	}

	got := map[string]bool{}
	for _, hld := range holds {
		got[hld.LegalHoldID] = true
	}
	if got["hold-b"] {
		t.Fatal("CROSS-TENANT READ: tenant A's register contained tenant B's hold, disclosing the authority that ordered it")
	}
	if !got["hold-a"] {
		t.Error("tenant A cannot see its own hold")
	}
	// A platform-wide hold freezes this tenant's records too. Hiding it would
	// show an empty register to a tenant whose data is in fact frozen.
	if !got["hold-platform"] {
		t.Error("platform-wide hold missing; a tenant would read 'nothing frozen' while frozen")
	}
}

func TestListLegalHolds_NoTenantScopeRefused(t *testing.T) {
	h, st, _ := newTestHandler()
	seedHolds(st)
	r := newTestRouter(h)

	w := httptest.NewRecorder()
	r.ServeHTTP(w, buildRequestAs(http.MethodGet, "/v1/legal-holds/", nil, ""))
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 with no X-Tenant-Id, got %d - defaulting it would make a dropped header an unscoped read", w.Code)
	}
}

func TestListLegalHolds_StatusFilter(t *testing.T) {
	h, st, _ := newTestHandler()
	seedHolds(st)
	st.holds[0].HoldStatus = "RELEASED"
	r := newTestRouter(h)

	w := httptest.NewRecorder()
	r.ServeHTTP(w, buildRequestAs(http.MethodGet, "/v1/legal-holds/?hold_status=ACTIVE", nil, tenantA))
	var holds []domain.LegalHold
	_ = json.NewDecoder(w.Body).Decode(&holds)
	for _, hld := range holds {
		if hld.HoldStatus != "ACTIVE" {
			t.Fatalf("status filter returned a %s hold", hld.HoldStatus)
		}
	}
}

func TestListLegalHolds_UnknownStatusRejected(t *testing.T) {
	// Refused rather than ignored or silently matching nothing: the first reads
	// as "no filter applied", the second as "this tenant has no holds".
	h, _, _ := newTestHandler()
	r := newTestRouter(h)

	w := httptest.NewRecorder()
	r.ServeHTTP(w, buildRequestAs(http.MethodGet, "/v1/legal-holds/?hold_status=FROZEN", nil, tenantA))
	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for an unknown hold_status, got %d", w.Code)
	}
}

func TestGetLegalHold_CannotReadAnotherTenantsHold(t *testing.T) {
	h, st, _ := newTestHandler()
	seedHolds(st)
	r := newTestRouter(h)

	w := httptest.NewRecorder()
	r.ServeHTTP(w, buildRequestAs(http.MethodGet, "/v1/legal-holds/hold-b", nil, tenantA))
	if w.Code != http.StatusNotFound {
		t.Fatalf("expected 404 reading tenant B's hold as tenant A, got %d - %s", w.Code, w.Body.String())
	}
	// 404 rather than 403 deliberately: a forbidden confirms the id exists.
	if bytes.Contains(w.Body.Bytes(), []byte("SEC subpoena")) {
		t.Fatal("the refusal leaked the hold's authority")
	}
}

func TestGetLegalHold_NoTenantScopeRefused(t *testing.T) {
	h, st, _ := newTestHandler()
	seedHolds(st)
	r := newTestRouter(h)

	w := httptest.NewRecorder()
	r.ServeHTTP(w, buildRequestAs(http.MethodGet, "/v1/legal-holds/hold-a", nil, ""))
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 with no tenant scope, got %d", w.Code)
	}
}

func TestGetLegalHold_PlatformWideIsReadableByAnyTenant(t *testing.T) {
	h, st, _ := newTestHandler()
	seedHolds(st)
	r := newTestRouter(h)

	w := httptest.NewRecorder()
	r.ServeHTTP(w, buildRequestAs(http.MethodGet, "/v1/legal-holds/hold-platform", nil, tenantB))
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 reading a platform-wide hold as tenant B, got %d - %s", w.Code, w.Body.String())
	}
}

func TestReleaseLegalHold_CannotReleaseAnotherTenantsHold(t *testing.T) {
	// The consequential one: releasing a hold unblocks deletion of records
	// something ordered frozen.
	h, st, _ := newTestHandler()
	seedHolds(st)
	r := newTestRouter(h)

	w := httptest.NewRecorder()
	r.ServeHTTP(w, buildRequestAs(http.MethodPost, "/v1/legal-holds/hold-b/release",
		map[string]any{"release_approved_by_principal_id": "legal-counsel-1"}, tenantA))
	if w.Code != http.StatusNotFound {
		t.Fatalf("expected 404 releasing tenant B's hold as tenant A, got %d - %s", w.Code, w.Body.String())
	}
	for _, hld := range st.holds {
		if hld.LegalHoldID == "hold-b" && hld.HoldStatus != "ACTIVE" {
			t.Fatalf("tenant B's hold is now %s - another tenant released it", hld.HoldStatus)
		}
	}
}

func TestListRetentionPolicies_ScopesToCallerTenant(t *testing.T) {
	h, st, _ := newTestHandler()
	tA, tB := tenantA, tenantB
	st.policies = append(st.policies,
		domain.RetentionPolicy{RetentionPolicyID: "pol-a", RecordClass: "FINANCIAL_LEDGER", TenantID: &tA, PolicyStatus: "ACTIVE", MinRetentionDays: 2555},
		domain.RetentionPolicy{RetentionPolicyID: "pol-b", RecordClass: "HR_RECORD", TenantID: &tB, PolicyStatus: "ACTIVE", MinRetentionDays: 365},
		domain.RetentionPolicy{RetentionPolicyID: "pol-global", RecordClass: "AUDIT_EVENT", TenantID: nil, PolicyStatus: "ACTIVE", MinRetentionDays: 3650},
	)
	r := newTestRouter(h)

	w := httptest.NewRecorder()
	r.ServeHTTP(w, buildRequestAs(http.MethodGet, "/v1/retention-policies/", nil, tenantA))
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d - %s", w.Code, w.Body.String())
	}
	var policies []domain.RetentionPolicy
	_ = json.NewDecoder(w.Body).Decode(&policies)

	got := map[string]bool{}
	for _, p := range policies {
		got[p.RetentionPolicyID] = true
	}
	if got["pol-b"] {
		t.Fatal("CROSS-TENANT READ: tenant A saw tenant B's retention policy")
	}
	if !got["pol-a"] || !got["pol-global"] {
		t.Errorf("expected tenant A's own policy and the platform-wide one, got %v", got)
	}
}

func TestListRetentionPolicies_BadPageParamsRefused(t *testing.T) {
	// Refused, not clamped: a caller who asked for 5000 and silently received
	// 500 would read a truncated register as a complete one.
	h, _, _ := newTestHandler()
	r := newTestRouter(h)

	for _, q := range []string{"?limit=5000", "?limit=0", "?offset=-1", "?limit=abc"} {
		w := httptest.NewRecorder()
		r.ServeHTTP(w, buildRequestAs(http.MethodGet, "/v1/retention-policies/"+q, nil, tenantA))
		if w.Code != http.StatusBadRequest {
			t.Errorf("%s: expected 400, got %d", q, w.Code)
		}
	}
}
