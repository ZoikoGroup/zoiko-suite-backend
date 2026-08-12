package handler_test

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

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
	req := httptest.NewRequest(http.MethodPost, "/v1/obligations/o-1/applicability-decisions", bytes.NewBufferString(body))
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
	req := httptest.NewRequest(http.MethodPost, "/v1/obligations/o-1/applicability-decisions", bytes.NewBufferString(body))
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 when neither decided_by_principal_id nor decided_by_system is set, got %d: %s", w.Code, w.Body.String())
	}
}

func TestCreateApplicabilityDecision_ObligationNotFound(t *testing.T) {
	store := &stubStore{applicabilityCreateErr: domain.ErrObligationNotFound}
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
	req := httptest.NewRequest(http.MethodPost, "/v1/obligations/does-not-exist/applicability-decisions", bytes.NewBufferString(body))
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

	req := httptest.NewRequest(http.MethodGet, "/v1/obligations/o-1/applicability?jurisdiction_code=IN-KA&entity_ref=le-1", nil)
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

	req := httptest.NewRequest(http.MethodGet, "/v1/obligations/o-1/applicability", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 when jurisdiction_code/entity_ref are omitted, got %d", w.Code)
	}
}
