package handler_test

import (
	"encoding/json"
	"net/http"
	"testing"

	"zoiko.io/financial-close-svc/internal/domain"
)

func supersedeReq() domain.SupersedeAllocationRuleRequest {
	return domain.SupersedeAllocationRuleRequest{
		Name:              "IT shared cost allocation (revised)",
		SourceAccountCode: "5000-ITSharedCost",
		Drivers: []domain.AllocationDriver{
			{RecipientAccountCode: "6100-Sales", WeightPercentage: 50},
			{RecipientAccountCode: "6200-Ops", WeightPercentage: 50},
		},
	}
}

func TestSupersedeAllocationRule_MissingFields_Returns400(t *testing.T) {
	s := newStubStore()
	cl := &stubClients{}
	r := newRouter(s, &stubPublisher{}, &stubAuthZ{}, cl)
	ruleID := createApprovedAllocationRule(t, s, cl, r, evenDrivers())

	rr := doReq(r, http.MethodPost, "/v1/allocation-rules/"+ruleID+"/supersede", map[string]any{}, "preparer-1")
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d: %s", rr.Code, rr.Body.String())
	}
}

func TestSupersedeAllocationRule_NoCurrentRule_Returns404(t *testing.T) {
	s := newStubStore()
	cl := &stubClients{}
	r := newRouter(s, &stubPublisher{}, &stubAuthZ{}, cl)

	rr := doReq(r, http.MethodPost, "/v1/allocation-rules/nonexistent/supersede", supersedeReq(), "preparer-1")
	if rr.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d: %s", rr.Code, rr.Body.String())
	}
}

func TestSupersedeAllocationRule_StillDraft_Refused(t *testing.T) {
	s := newStubStore()
	cl := &stubClients{}
	r := newRouter(s, &stubPublisher{}, &stubAuthZ{}, cl)

	// Create but never approve — the rule's current version is DRAFT.
	if cl.accountStatuses == nil {
		cl.accountStatuses = map[string]string{}
	}
	req := domain.CreateAllocationRuleRequest{LegalEntityID: "le-1", Name: "x", SourceAccountCode: "5000", Drivers: evenDrivers()}
	rr := doReq(r, http.MethodPost, "/v1/allocation-rules/", req, "preparer-1")
	var rule domain.AllocationRule
	_ = json.NewDecoder(rr.Body).Decode(&rule)

	sup := doReq(r, http.MethodPost, "/v1/allocation-rules/"+rule.RuleID+"/supersede", supersedeReq(), "preparer-1")
	if sup.Code != http.StatusUnprocessableEntity {
		t.Fatalf("expected 422 superseding a DRAFT rule, got %d: %s", sup.Code, sup.Body.String())
	}
}

func TestSupersedeAllocationRule_ApprovedRule_CreatesNewDraftVersion(t *testing.T) {
	s := newStubStore()
	cl := &stubClients{}
	r := newRouter(s, &stubPublisher{}, &stubAuthZ{}, cl)
	ruleID := createApprovedAllocationRule(t, s, cl, r, evenDrivers())

	rr := doReq(r, http.MethodPost, "/v1/allocation-rules/"+ruleID+"/supersede", supersedeReq(), "preparer-1")
	if rr.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d: %s", rr.Code, rr.Body.String())
	}
	var newVersion domain.AllocationRule
	if err := json.NewDecoder(rr.Body).Decode(&newVersion); err != nil {
		t.Fatal(err)
	}
	if newVersion.RuleID != ruleID {
		t.Fatalf("expected the SAME rule_id, got %q want %q", newVersion.RuleID, ruleID)
	}
	if newVersion.Status != domain.AllocationRuleStatusDraft {
		t.Fatalf("expected the new version to start DRAFT, got %q", newVersion.Status)
	}
	if newVersion.Version != 2 {
		t.Fatalf("expected version 2, got %d", newVersion.Version)
	}

	// GetCurrentAllocationRule must now return the NEW version's content,
	// not the superseded one.
	get := doReq(r, http.MethodGet, "/v1/allocation-rules/"+ruleID, nil, "preparer-1")
	var current domain.AllocationRule
	_ = json.NewDecoder(get.Body).Decode(&current)
	if current.Name != "IT shared cost allocation (revised)" {
		t.Fatalf("expected the current rule to reflect the new version, got %+v", current)
	}
}

func TestSupersedeAllocationRule_TwiceInARow_SecondRefusedAsAlreadySuperseded(t *testing.T) {
	// Closes the real gap this command exists for: once superseded, a rule
	// cannot be superseded again without first being approved — there is
	// never a window where two versions are simultaneously current.
	s := newStubStore()
	cl := &stubClients{}
	r := newRouter(s, &stubPublisher{}, &stubAuthZ{}, cl)
	ruleID := createApprovedAllocationRule(t, s, cl, r, evenDrivers())

	first := doReq(r, http.MethodPost, "/v1/allocation-rules/"+ruleID+"/supersede", supersedeReq(), "preparer-1")
	if first.Code != http.StatusCreated {
		t.Fatalf("first supersede failed: %d %s", first.Code, first.Body.String())
	}

	// The new version is DRAFT, not APPROVED/ACTIVE — a second supersede
	// attempt must be refused.
	second := doReq(r, http.MethodPost, "/v1/allocation-rules/"+ruleID+"/supersede", supersedeReq(), "preparer-1")
	if second.Code != http.StatusUnprocessableEntity {
		t.Fatalf("expected 422, got %d: %s", second.Code, second.Body.String())
	}
}
