package handler_test

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-chi/chi/v5"

	"zoiko.io/obligations-svc/internal/domain"
)

func TestCreateApplicabilityDecision_Created(t *testing.T) {
	store := &stubStore{
		applicabilityDecision: &domain.ApplicabilityDecision{
			ApplicabilityDecisionID: "ad-1",
			ObligationID:            "o-1",
			Decision:                "APPLICABLE",
		},
	}
	r := newApplicabilityTestRouter(store)

	body := `{
		"jurisdiction_code": "IN-KA",
		"entity_ref": "le-1",
		"decision": "APPLICABLE",
		"source_rule_ref": "IN-GST-FILING-RULE-07",
		"source_rule_version": "v3",
		"effective_from": "2026-01-01T00:00:00Z",
		"decided_by_system": "obligation-rule-engine",
		"created_by_principal_id": "admin-1"
	}`
	req := withIdentity(httptest.NewRequest(http.MethodPost, "/v1/obligations/o-1/applicability-decisions", bytes.NewBufferString(body)))
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d: %s", w.Code, w.Body.String())
	}
}

// TestCreateApplicabilityDecision_RequiresActorOrSystem proves doc7 §E2's
// "actor/system" requirement is enforced: a decision with neither set must
// be rejected, not silently recorded as attributable to nobody.
func TestCreateApplicabilityDecision_RequiresActorOrSystem(t *testing.T) {
	r := newApplicabilityTestRouter(&stubStore{})

	body := `{
		"jurisdiction_code": "IN-KA",
		"entity_ref": "le-1",
		"decision": "APPLICABLE",
		"source_rule_ref": "IN-GST-FILING-RULE-07",
		"source_rule_version": "v3",
		"effective_from": "2026-01-01T00:00:00Z",
		"created_by_principal_id": "admin-1"
	}`
	req := withIdentity(httptest.NewRequest(http.MethodPost, "/v1/obligations/o-1/applicability-decisions", bytes.NewBufferString(body)))
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 when neither decided_by_principal_id nor decided_by_system is set, got %d: %s", w.Code, w.Body.String())
	}
}

func TestCreateApplicabilityDecision_ObligationNotFound(t *testing.T) {
	// findErr, not applicabilityCreateErr: the handler resolves the parent
	// obligation before it authorizes, so "not found" is now decided by the
	// lookup. Leaving this on the create error would have passed for the wrong
	// reason -- the handler would never have reached the create at all.
	store := &stubStore{findErr: domain.ErrObligationNotFound}
	r := newApplicabilityTestRouter(store)

	body := `{
		"jurisdiction_code": "IN-KA",
		"entity_ref": "le-1",
		"decision": "APPLICABLE",
		"source_rule_ref": "rule-1",
		"source_rule_version": "v1",
		"effective_from": "2026-01-01T00:00:00Z",
		"decided_by_system": "obligation-rule-engine",
		"created_by_principal_id": "admin-1"
	}`
	req := withIdentity(httptest.NewRequest(http.MethodPost, "/v1/obligations/does-not-exist/applicability-decisions", bytes.NewBufferString(body)))
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d: %s", w.Code, w.Body.String())
	}
}

// TestGetCurrentApplicability_UnassessedWhenNoDecisionExists is doc7 §E2's
// core guarantee: absence of any decision must read as UNASSESSED, never
// silently coerced into NOT_APPLICABLE.
func TestGetCurrentApplicability_UnassessedWhenNoDecisionExists(t *testing.T) {
	store := &stubStore{
		currentApplicability: &domain.CurrentApplicability{
			ObligationID:     "o-1",
			JurisdictionCode: "IN-KA",
			EntityRef:        "le-1",
			Status:           "UNASSESSED",
		},
	}
	r := newApplicabilityTestRouter(store)

	req := withIdentity(httptest.NewRequest(http.MethodGet, "/v1/obligations/o-1/applicability?jurisdiction_code=IN-KA&entity_ref=le-1", nil))
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	var got domain.CurrentApplicability
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("failed to unmarshal response: %v", err)
	}
	if got.Status != "UNASSESSED" {
		t.Fatalf("expected UNASSESSED for a scope with no decision, got %q", got.Status)
	}
	if got.Decision != nil {
		t.Fatalf("expected no decision object for an UNASSESSED status, got %+v", got.Decision)
	}
}

func TestGetCurrentApplicability_MissingQueryParams(t *testing.T) {
	r := newApplicabilityTestRouter(&stubStore{})

	req := withIdentity(httptest.NewRequest(http.MethodGet, "/v1/obligations/o-1/applicability", nil))
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 when jurisdiction_code/entity_ref are omitted, got %d", w.Code)
	}
}

// ── the gates added when the applicability write was closed ──────────────────
//
// Recording an applicability decision was the one ungated write left in this
// service after the others were closed. These tests exist so it cannot quietly
// become ungated again: each one asserts a refusal, because an authorization
// gate is only demonstrated by the request it turns away.

func applicabilityBody(createdBy, decidedBy, decidedBySystem string) string {
	return `{
		"jurisdiction_code": "IN-KA",
		"entity_ref": "le-1",
		"decision": "APPLICABLE",
		"source_rule_ref": "IN-GST-FILING-RULE-07",
		"source_rule_version": "v3",
		"effective_from": "2026-01-01T00:00:00Z",
		"decided_by_principal_id": "` + decidedBy + `",
		"decided_by_system": "` + decidedBySystem + `",
		"created_by_principal_id": "` + createdBy + `"
	}`
}

func postApplicability(t *testing.T, r chi.Router, body string, identity bool) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/v1/obligations/o-1/applicability-decisions", bytes.NewBufferString(body))
	if identity {
		req = withIdentity(req)
	}
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

func TestCreateApplicabilityDecision_RefusesWithoutIdentity(t *testing.T) {
	r := newApplicabilityTestRouter(&stubStore{})
	w := postApplicability(t, r, applicabilityBody(testPrincipal, "", "rule-engine"), false)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 for a request the gateway never identified, got %d: %s", w.Code, w.Body.String())
	}
}

func TestCreateApplicabilityDecision_RefusesTenantWithoutPrincipal(t *testing.T) {
	r := newApplicabilityTestRouter(&stubStore{})
	req := httptest.NewRequest(http.MethodPost, "/v1/obligations/o-1/applicability-decisions",
		bytes.NewBufferString(applicabilityBody(testPrincipal, "", "rule-engine")))
	req.Header.Set("X-Tenant-Id", testTenant) // tenant but no principal
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 when a tenant is scoped but no principal is named, got %d: %s", w.Code, w.Body.String())
	}
}

// The action string matters as much as the refusal: a gate that checks the
// wrong action is a gate on nothing. APPLICABILITY_DECISION_RECORD must also
// exist in seed-demo-rbac.ps1, or every caller is refused instead — the
// missing-bundle shape that had four separate instances on this platform.
func TestCreateApplicabilityDecision_DeniedIsForbiddenOnTheRightAction(t *testing.T) {
	a := &stubAuthz{err: domain.ErrAuthorizationDenied}
	r := newApplicabilityTestRouterAuthz(&stubStore{}, a)

	w := postApplicability(t, r, applicabilityBody(testPrincipal, "", "rule-engine"), true)
	if w.Code != http.StatusForbidden {
		t.Fatalf("expected 403 when the principal holds no grant, got %d: %s", w.Code, w.Body.String())
	}
	if len(a.calls) != 1 || a.calls[0] != "APPLICABILITY_DECISION_RECORD" {
		t.Fatalf("expected one check on APPLICABILITY_DECISION_RECORD, got %v", a.calls)
	}
}

// Ordering, not just presence: an obligation the caller cannot see must answer
// 404 and must never reach the authorization check. If it did, the pair of
// answers (403 for a real obligation, 404 for an imaginary one) would tell an
// outsider which obligation ids exist in another tenant's register.
func TestCreateApplicabilityDecision_UnknownParentIsNotAnOracle(t *testing.T) {
	a := &stubAuthz{err: domain.ErrAuthorizationDenied}
	r := newApplicabilityTestRouterAuthz(&stubStore{findErr: domain.ErrObligationNotFound}, a)

	w := postApplicability(t, r, applicabilityBody(testPrincipal, "", "rule-engine"), true)
	if w.Code != http.StatusNotFound {
		t.Fatalf("expected 404 for an obligation this tenant cannot see, got %d: %s", w.Code, w.Body.String())
	}
	if len(a.calls) != 0 {
		t.Fatalf("authorization must not run for an obligation that was never found, got calls %v", a.calls)
	}
}

func TestCreateApplicabilityDecision_RefusesForeignAttribution(t *testing.T) {
	for _, tc := range []struct {
		name, createdBy, decidedBy, wantErr string
	}{
		{"created_by names someone else", "someone-else", "", "created_by_mismatch"},
		{"decided_by names someone else", testPrincipal, "someone-else", "decided_by_mismatch"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newApplicabilityTestRouter(&stubStore{})
			w := postApplicability(t, r, applicabilityBody(tc.createdBy, tc.decidedBy, "rule-engine"), true)
			if w.Code != http.StatusBadRequest {
				t.Fatalf("expected 400, got %d: %s", w.Code, w.Body.String())
			}
			var body map[string]string
			if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
				t.Fatalf("unmarshal: %v", err)
			}
			if body["error"] != tc.wantErr {
				t.Fatalf("expected error %q, got %q", tc.wantErr, body["error"])
			}
		})
	}
}

// The automated case has to stay legitimate: doc7 §E2 allows a decision reached
// by a rule engine, which names decided_by_system and leaves decided_by empty.
// The attribution check must not turn that into a 400.
func TestCreateApplicabilityDecision_SystemDecisionStillAllowed(t *testing.T) {
	store := &stubStore{applicabilityDecision: &domain.ApplicabilityDecision{ApplicabilityDecisionID: "ad-1"}}
	r := newApplicabilityTestRouter(store)

	w := postApplicability(t, r, applicabilityBody(testPrincipal, "", "obligation-rule-engine"), true)
	if w.Code != http.StatusCreated {
		t.Fatalf("expected 201 for a system-attributed decision, got %d: %s", w.Code, w.Body.String())
	}
}

// A caller deciding in their own name is the ordinary human case.
func TestCreateApplicabilityDecision_SelfAttributedDecisionAllowed(t *testing.T) {
	store := &stubStore{applicabilityDecision: &domain.ApplicabilityDecision{ApplicabilityDecisionID: "ad-1"}}
	r := newApplicabilityTestRouter(store)

	w := postApplicability(t, r, applicabilityBody(testPrincipal, testPrincipal, ""), true)
	if w.Code != http.StatusCreated {
		t.Fatalf("expected 201 when the caller decides in their own name, got %d: %s", w.Code, w.Body.String())
	}
}

// Both reads used to answer 503 store_unavailable when no tenant header was
// present: the store returned ErrTenantMissing and the handler's switch folded
// it into the default branch, blaming the database for an unscoped request.
//
// THE STUB HERE SUCCEEDS ON PURPOSE. An earlier version of this test had the
// stub return ErrTenantMissing, and it passed with the handler's tenant check
// deleted — because the error mapping added in the same change ALSO answers 401
// for that error. It asserted the right status for the wrong reason and would
// not have noticed the gate going away. With a stub that would happily return
// rows, only the handler refusing the request can produce a 401, and the
// unscoped read shows up as the 200 it would be.
func TestApplicabilityReads_MissingTenantIsUnauthorizedNotUnavailable(t *testing.T) {
	for _, path := range []string{
		"/v1/obligations/o-1/applicability-decisions?jurisdiction_code=IN-KA&entity_ref=le-1",
		"/v1/obligations/o-1/applicability?jurisdiction_code=IN-KA&entity_ref=le-1",
	} {
		t.Run(path, func(t *testing.T) {
			r := newApplicabilityTestRouter(&stubStore{
				applicabilityList:    []*domain.ApplicabilityDecision{{ApplicabilityDecisionID: "ad-1"}},
				currentApplicability: &domain.CurrentApplicability{ObligationID: "o-1", Status: "APPLICABLE"},
			})
			req := httptest.NewRequest(http.MethodGet, path, nil) // deliberately no identity
			w := httptest.NewRecorder()
			r.ServeHTTP(w, req)
			if w.Code != http.StatusUnauthorized {
				t.Fatalf("expected 401 for an unscoped read, got %d: %s", w.Code, w.Body.String())
			}
		})
	}
}
