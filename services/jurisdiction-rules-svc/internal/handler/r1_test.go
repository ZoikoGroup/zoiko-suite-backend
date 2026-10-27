package handler_test

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"zoiko.io/jurisdiction-rules-svc/internal/domain"
)

func createRuleBody(extra map[string]any) map[string]any {
	b := map[string]any{
		"rule_domain": "TAX", "rule_code": "VAT", "rule_name": "VAT",
		"effective_from": "2026-01-01T00:00:00Z", "rule_payload": map[string]any{},
	}
	for k, v := range extra {
		b[k] = v
	}
	return b
}

// Re-audit R-1: the three fields openapi documents on CreateRuleRequest were
// refused as unknown (400). They now reach the store.
func TestCreateRule_AcceptsTheDocumentedVersionAndPrecedenceFields(t *testing.T) {
	st := &stubStore{createdRule: &domain.JurisdictionRule{JurisdictionRuleID: "r-2"}, ruleWasCreated: true}
	h := newTestRouterWithAuthz(st, permitAll())
	pred := "11111111-1111-1111-1111-111111111111"

	rr := postJSON(t, h, "/v1/admin/jurisdictions/j-1/rules", createRuleBody(map[string]any{
		"precedence_level": 3, "supersedes_rule_id": pred, "rule_version": 2,
	}))
	if rr.Code != http.StatusCreated {
		t.Fatalf("want 201, got %d: %s", rr.Code, rr.Body.String())
	}
	a := st.createRuleArgs
	if a.PrecedenceLevel != 3 || a.SupersedesRuleID == nil || *a.SupersedesRuleID != pred || a.RuleVersion != 2 {
		t.Fatalf("fields not passed to the store: %+v", a)
	}
}

func TestCreateRule_RejectsBadVersionAndPrecedenceValues(t *testing.T) {
	cases := map[string]map[string]any{
		"invalid_precedence_level": {"precedence_level": -1},
		"invalid_supersedes":       {"supersedes_rule_id": "not-a-rule"},
		"invalid_rule_version":     {"rule_version": 0},
	}
	for code, extra := range cases {
		st := &stubStore{}
		rr := postJSON(t, newTestRouterWithAuthz(st, permitAll()), "/v1/admin/jurisdictions/j-1/rules", createRuleBody(extra))
		if rr.Code != http.StatusBadRequest || st.storeCalled {
			t.Errorf("%s: want 400 without a store call, got %d (store called=%v)", code, rr.Code, st.storeCalled)
			continue
		}
		if got := decodeError(t, rr)["code"]; got != code {
			t.Errorf("want code %s, got %v", code, got)
		}
	}
	over := &stubStore{}
	if rr := postJSON(t, newTestRouterWithAuthz(over, permitAll()), "/v1/admin/jurisdictions/j-1/rules",
		createRuleBody(map[string]any{"precedence_level": domain.MaxPrecedenceLevel + 1})); rr.Code != http.StatusBadRequest {
		t.Errorf("precedence above the maximum: want 400, got %d", rr.Code)
	}
	for err, want := range map[error]int{
		domain.ErrInvalidSupersedes:   http.StatusBadRequest,
		domain.ErrSupersedesTaken:     http.StatusConflict,
		domain.ErrRuleVersionMismatch: http.StatusConflict,
	} {
		rr := postJSON(t, newTestRouterWithAuthz(&stubStore{createRuleErr: err}, permitAll()), "/v1/admin/jurisdictions/j-1/rules", createRuleBody(nil))
		if rr.Code != want {
			t.Errorf("%v: want %d, got %d", err, want, rr.Code)
		}
	}
}

func postWithIfMatch(t *testing.T, h http.Handler, path, ifMatch string, body any) *httptest.ResponseRecorder {
	t.Helper()
	b, _ := json.Marshal(body)
	req := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(b))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Principal-Id", testPrincipal)
	req.Header.Set("If-Match", ifMatch)
	return executeRequest(h, req)
}

// Row 43: If-Match reaches the store as the expected row_version, a stale one
// is 412, and a malformed one never reaches the store.
func TestIfMatch_TransitionAndDrift(t *testing.T) {
	st := &stubStore{transitionedRule: &domain.JurisdictionRule{RuleStatus: "RETIRED", RowVersion: 8}, transitionDidRun: true}
	rr := postWithIfMatch(t, newTestRouterWithAuthz(st, permitAll()), "/v1/admin/rules/r-1/transition", `"7"`, map[string]any{"new_status": "RETIRED"})
	if rr.Code != http.StatusOK || st.transitionArgs.ExpectedRowVersion == nil || *st.transitionArgs.ExpectedRowVersion != 7 {
		t.Fatalf("If-Match \"7\" must reach the store: %d %+v", rr.Code, st.transitionArgs.ExpectedRowVersion)
	}
	if rr.Header().Get("ETag") != `"8"` {
		t.Errorf("the response must carry the new ETag, got %q", rr.Header().Get("ETag"))
	}

	stale := &stubStore{transitionErr: domain.ErrPreconditionFailed}
	rr = postWithIfMatch(t, newTestRouterWithAuthz(stale, permitAll()), "/v1/admin/rules/r-1/transition", `"1"`, map[string]any{"new_status": "RETIRED"})
	if rr.Code != http.StatusPreconditionFailed || decodeErrorAnyStatus(t, rr)["code"] != "precondition_failed" {
		t.Fatalf("stale If-Match: want 412 precondition_failed, got %d", rr.Code)
	}

	bad := &stubStore{}
	rr = postWithIfMatch(t, newTestRouterWithAuthz(bad, permitAll()), "/v1/admin/rules/r-1/drift", `"abc"`, map[string]any{"drift_state": "DRIFTED"})
	if rr.Code != http.StatusBadRequest || bad.storeCalled {
		t.Fatalf("malformed If-Match: want 400 without a store call, got %d", rr.Code)
	}

	drift := &stubStore{driftRule: &domain.JurisdictionRule{RowVersion: 4}, driftEvent: &domain.DriftEvent{}, driftChanged: true}
	rr = postWithIfMatch(t, newTestRouterWithAuthz(drift, permitAll()), "/v1/admin/rules/r-1/drift", `W/"3"`, map[string]any{"drift_state": "DRIFTED"})
	if rr.Code != http.StatusOK || drift.driftArgs.ExpectedRowVersion == nil || *drift.driftArgs.ExpectedRowVersion != 3 {
		t.Fatalf("weak If-Match W/\"3\" must reach the drift store: %d", rr.Code)
	}
}

func TestGetRule_ReturnsETag(t *testing.T) {
	st := &stubStore{ruleByID: &domain.JurisdictionRule{JurisdictionRuleID: "r-1", RowVersion: 5}}
	rr := executeRequest(newTestRouterWithAuthz(st, permitAll()), httptest.NewRequest(http.MethodGet, "/v1/rules/r-1", nil))
	if rr.Code != http.StatusOK || rr.Header().Get("ETag") != `"5"` {
		t.Fatalf("GET rule must carry ETag \"5\", got %d %q", rr.Code, rr.Header().Get("ETag"))
	}
}

func decodeErrorAnyStatus(t *testing.T, rr *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	if ct := rr.Header().Get("Content-Type"); ct != "application/problem+json" {
		t.Fatalf("expected Content-Type application/problem+json, got %q", ct)
	}
	var body map[string]any
	_ = json.NewDecoder(rr.Body).Decode(&body)
	return body
}
