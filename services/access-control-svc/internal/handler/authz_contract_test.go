package handler_test

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/google/uuid"

	"zoiko.io/access-control-svc/internal/domain"
)

// The authorization-svc integration after its 7 Oct governance pass:
// approval references and §16 purposes travel with every provisioning call, a
// review's REVOKE is executed by this service's identity (gap S9-B), and an
// assignment authorization-svc parks PENDING is never recorded as provisioned.

func TestProvisioning_CarriesApprovalReferenceAndPurpose(t *testing.T) {
	g := newGovRig()
	role := g.seedRole("LEGAL_ENTITY", domain.RoleStatusActive, "supplier_invoice.create")
	code, raw := requestAssignment(g, "admin-1", "user-7", role.RoleDefinitionID, uuid.NewString())
	if code != http.StatusCreated {
		t.Fatalf("want 201, got %d %s", code, raw)
	}
	a := decodeBody[domain.AssignmentRequest](t, raw)
	s := g.asg.creates[0].scope
	if s.ApprovalReference != a.RequestID || s.Purpose != "joins AP team" {
		t.Fatalf("provisioning scope = %+v; want approval_reference = request id and the justification as purpose", s)
	}

	code, raw = decideAssignment(g, "revoke", "admin-1", a.RequestID, true)
	if code != http.StatusOK {
		t.Fatalf("revoke: %d %s", code, raw)
	}
	rs := g.asg.revokeScope[len(g.asg.revokeScope)-1]
	if rs.Purpose != "checked" || rs.ApprovalReference != a.RequestID {
		t.Errorf("revoke scope = %+v; want the reason as purpose", rs)
	}
}

// S9-B: a REVOKE review decision is executed as the service identity, not the
// reviewer, with the reviewer recorded in the purpose.
func TestReviewRevoke_ExecutedAsServiceIdentity(t *testing.T) {
	g := newGovRig()
	high := g.seedRole("LEGAL_ENTITY", domain.RoleStatusActive, "payment.release")
	asg := uuid.NewString()
	g.asg.assignments[high.RoleDefinitionID] = []domain.AuthzAssignment{{PrincipalRoleAssignmentID: asg, PrincipalID: "user-7", RoleID: high.RoleDefinitionID}}
	_, raw := createCampaign(g, nil)
	c := decodeBody[domain.ReviewCampaign](t, raw)
	rr := doReq(g.r, http.MethodPost, "/v1/access-review-campaigns/"+c.CampaignID+"/items/"+c.Items[0].ItemID+"/decide", map[string]any{
		"decision": "REVOKE", "reason": "left treasury", "correlation_id": uuid.NewString(),
	}, "mgr-1")
	if rr.Code != http.StatusOK {
		t.Fatalf("review revoke: %d %s", rr.Code, rr.Body.String())
	}
	s := g.asg.revokeScope[len(g.asg.revokeScope)-1]
	if s.PrincipalID != "svc-access-control" || s.ReasonCode != "ACCESS_REVIEW_REVOKE" ||
		!strings.Contains(s.Purpose, "by mgr-1") || !strings.Contains(s.Purpose, "left treasury") {
		t.Fatalf("revocation scope = %+v; want the service identity, with the reviewer and reason in the purpose", s)
	}
}

// authorization-svc parked the grant PENDING: not provisioned here, withdrawn
// there, reported 409.
func TestApprove_AuthzPendingIsNotProvisioned(t *testing.T) {
	g := newGovRig()
	role := g.seedRole("LEGAL_ENTITY", domain.RoleStatusActive, "payment.release")
	_, raw := requestAssignment(g, "admin-1", "user-7", role.RoleDefinitionID, uuid.NewString())
	a := decodeBody[domain.AssignmentRequest](t, raw)
	g.asg.createErr = domain.ErrAuthzApprovalPending
	code, raw := decideAssignment(g, "approve", "approver-2", a.RequestID, true)
	if code != http.StatusConflict || govCode(raw) != "authz_approval_pending" {
		t.Fatalf("want 409 authz_approval_pending, got %d %s", code, raw)
	}
	if len(g.asg.revokes) != 1 || g.asg.revokes[0] != a.RequestID || g.asg.revokeScope[0].ReasonCode != "PROVISIONING_WITHDRAWN" {
		t.Errorf("the pending grant must be withdrawn there: %v %+v", g.asg.revokes, g.asg.revokeScope)
	}
	got, _ := g.gov.GetAssignmentRequest(context.Background(), a.RequestID)
	if got != nil && got.Status != domain.AssignmentPendingApproval {
		t.Errorf("request status = %s; it must stay PENDING_APPROVAL", got.Status)
	}
}
