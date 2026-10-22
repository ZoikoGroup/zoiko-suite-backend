package handler_test

import (
	"encoding/json"
	"net/http"
	"testing"

	"zoiko.io/jurisdiction-rules-svc/internal/authz"
	"zoiko.io/jurisdiction-rules-svc/internal/domain"
	"zoiko.io/jurisdiction-rules-svc/internal/handler"
)

// Governance audit J-06 / row 4: POST /v1/validate-action was documented and
// not routed. action_type is "<rule_domain>.<rule_code>" (decision 21 Oct
// 2026), resolved through the rule pack with the code required.

const validateJurisdiction = "11111111-1111-4111-8111-111111111111"

func validateBody(actionType string) map[string]any {
	return map[string]any{"action_type": actionType, "jurisdiction_id": validateJurisdiction, "entity_type": "COMPANY"}
}

func decodeValidate(t *testing.T, body []byte) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(body, &m); err != nil {
		t.Fatalf("decode: %v (%s)", err, body)
	}
	return m
}

func TestValidateAction_ResolvedRuleIsPermitted(t *testing.T) {
	st := &stubStore{rulePack: &domain.RulePack{
		Outcome: domain.RulePackResolved, ResolvedFrom: []string{validateJurisdiction}, PackDigest: "sha256:abc",
		Rules: []*domain.JurisdictionRule{
			{JurisdictionRuleID: "r-other", RuleDomain: "TAX", RuleCode: "REDUCED"},
			{JurisdictionRuleID: "r-std", RuleDomain: "TAX", RuleCode: "STD"},
		},
	}}
	authzMock := &mockAuthZ{}
	rr := postJSON(t, newTestRouterWithAuthz(st, authzMock), handler.ValidateActionPath, validateBody("TAX.STD"))
	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rr.Code, rr.Body.String())
	}
	got := decodeValidate(t, rr.Body.Bytes())
	rule, _ := got["rule"].(map[string]any)
	if got["permitted"] != true || rule["jurisdiction_rule_id"] != "r-std" || got["pack_digest"] != "sha256:abc" {
		t.Fatalf("response = %v", got)
	}
	q := st.rulePackQuery
	if q.JurisdictionID != validateJurisdiction || q.RuleDomain != "TAX" || len(q.RequiredCodes) != 1 || q.RequiredCodes[0] != "STD" {
		t.Fatalf("query = %+v", q)
	}
	if authzMock.resource != "jurisdiction_rule" || authzMock.action != "validate" ||
		authz.ActionType(authzMock.resource, authzMock.action) != "JURISDICTION_RULE_VALIDATE" {
		t.Fatalf("authorized as %s/%s", authzMock.resource, authzMock.action)
	}
}

func TestValidateAction_ConflictedIsNotPermitted(t *testing.T) {
	st := &stubStore{rulePack: &domain.RulePack{Outcome: domain.RulePackConflicted,
		Rules:     []*domain.JurisdictionRule{{JurisdictionRuleID: "r-1", RuleDomain: "TAX", RuleCode: "STD"}},
		Conflicts: []domain.RuleConflict{{RuleDomain: "TAX", RuleCode: "STD", RuleIDs: []string{"r-1", "r-2"}}}}}
	rr := postJSON(t, newTestRouter(st), handler.ValidateActionPath, validateBody("TAX.STD"))
	got := decodeValidate(t, rr.Body.Bytes())
	if rr.Code != http.StatusConflict || got["permitted"] != false || got["detail"] != "conflicted" || got["rule"] != nil {
		t.Fatalf("code %d body %v", rr.Code, got)
	}
}

// A conflict on another code of the domain does not block this action.
func TestValidateAction_ConflictOnAnotherCodeDoesNotBlock(t *testing.T) {
	st := &stubStore{rulePack: &domain.RulePack{Outcome: domain.RulePackConflicted,
		Rules:     []*domain.JurisdictionRule{{JurisdictionRuleID: "r-1", RuleDomain: "TAX", RuleCode: "STD"}},
		Conflicts: []domain.RuleConflict{{RuleDomain: "TAX", RuleCode: "REDUCED", RuleIDs: []string{"r-8", "r-9"}}}}}
	if rr := postJSON(t, newTestRouter(st), handler.ValidateActionPath, validateBody("TAX.STD")); rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rr.Code, rr.Body.String())
	}
}

func TestValidateAction_NoRuleIsInsufficient(t *testing.T) {
	st := &stubStore{rulePack: &domain.RulePack{Outcome: domain.RulePackInsufficient, Insufficient: []string{"STD"}}}
	rr := postJSON(t, newTestRouter(st), handler.ValidateActionPath, validateBody("TAX.STD"))
	got := decodeValidate(t, rr.Body.Bytes())
	if rr.Code != http.StatusConflict || got["detail"] != "insufficient" {
		t.Fatalf("code %d body %v", rr.Code, got)
	}
}

func TestValidateAction_RefusesMalformedInput(t *testing.T) {
	for name, body := range map[string]map[string]any{
		"no dot":         validateBody("TAXSTD"),
		"empty code":     validateBody("TAX."),
		"no entity type": {"action_type": "TAX.STD", "jurisdiction_id": validateJurisdiction},
		"unknown field":  {"action_type": "TAX.STD", "jurisdiction_id": validateJurisdiction, "entity_type": "COMPANY", "amount": 5},
		"payload array":  {"action_type": "TAX.STD", "jurisdiction_id": validateJurisdiction, "entity_type": "COMPANY", "payload": []int{1}},
	} {
		st := &stubStore{}
		if rr := postJSON(t, newTestRouter(st), handler.ValidateActionPath, body); rr.Code != http.StatusBadRequest {
			t.Errorf("%s: expected 400, got %d", name, rr.Code)
		}
		if st.storeCalled {
			t.Errorf("%s: the store was asked before the input was refused", name)
		}
	}
}

func TestValidateAction_AuthzAndStoreErrors(t *testing.T) {
	if rr := postJSON(t, newTestRouterWithAuthz(&stubStore{}, &mockAuthZ{err: authz.ErrUnauthorized}), handler.ValidateActionPath, validateBody("TAX.STD")); rr.Code != http.StatusForbidden {
		t.Errorf("denied: expected 403, got %d", rr.Code)
	}
	if rr := postJSONAs(t, newTestRouter(&stubStore{}), handler.ValidateActionPath, validateBody("TAX.STD"), ""); rr.Code != http.StatusUnauthorized {
		t.Errorf("no principal: expected 401, got %d", rr.Code)
	}
	st := &stubStore{rulePackErr: domain.ErrJurisdictionNotFound}
	if rr := postJSON(t, newTestRouter(st), handler.ValidateActionPath, validateBody("TAX.STD")); rr.Code != http.StatusNotFound {
		t.Errorf("unknown jurisdiction: expected 404, got %d", rr.Code)
	}
}
