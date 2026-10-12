package handler_test

import (
	"context"
	"net/http"
	"sort"
	"testing"
	"time"

	"github.com/google/uuid"

	"zoiko.io/access-control-svc/internal/domain"
)

// S9-C1: group subjects. A group assignment fans out to one governed request
// per member; nothing about membership bypasses SoD or approval.

type stubGroupStore struct {
	gov      *stubGovStore
	groups   map[string]*domain.Group
	assigned map[string]*domain.GroupAssignment
}

func newStubGroupStore(gov *stubGovStore) *stubGroupStore {
	return &stubGroupStore{gov: gov, groups: map[string]*domain.Group{}, assigned: map[string]*domain.GroupAssignment{}}
}

func (s *stubGroupStore) CreateGroup(_ context.Context, g *domain.Group) (bool, error) {
	for _, x := range s.groups {
		if x.CorrelationID == g.CorrelationID {
			*g = *x
			return false, nil
		}
		if x.GroupCode == g.GroupCode {
			return false, domain.ErrGroupCodeExists
		}
	}
	cp := *g
	s.groups[g.GroupID] = &cp
	return true, nil
}
func (s *stubGroupStore) GetGroup(_ context.Context, id string) (*domain.Group, error) {
	g, ok := s.groups[id]
	if !ok {
		return nil, domain.ErrGroupNotFound
	}
	cp := *g
	cp.Members = append([]domain.GroupMember(nil), g.Members...)
	return &cp, nil
}
func (s *stubGroupStore) ListGroups(context.Context) ([]domain.Group, error) {
	out := []domain.Group{}
	for _, g := range s.groups {
		out = append(out, *g)
	}
	return out, nil
}
func (s *stubGroupStore) AddGroupMember(_ context.Context, id, principal, actor string) (*domain.GroupMember, error) {
	g, ok := s.groups[id]
	if !ok {
		return nil, domain.ErrGroupNotFound
	}
	for _, m := range g.Members {
		if m.PrincipalID == principal {
			return nil, domain.ErrGroupMemberExists
		}
	}
	m := domain.GroupMember{PrincipalID: principal, AddedByPrincipalID: actor, AddedAt: time.Now().UTC()}
	g.Members = append(g.Members, m)
	return &m, nil
}
func (s *stubGroupStore) RemoveGroupMember(_ context.Context, id, principal, _, _ string) error {
	g := s.groups[id]
	for i, m := range g.Members {
		if m.PrincipalID == principal {
			g.Members = append(g.Members[:i], g.Members[i+1:]...)
			return nil
		}
	}
	return domain.ErrGroupMemberNotFound
}
func (s *stubGroupStore) FindGroupAssignmentByCorrelation(_ context.Context, cid string) (*domain.GroupAssignment, error) {
	for _, a := range s.assigned {
		if a.CorrelationID == cid {
			cp := *a
			return &cp, nil
		}
	}
	return nil, nil
}
func (s *stubGroupStore) CreateGroupAssignment(_ context.Context, a *domain.GroupAssignment) (bool, error) {
	cp := *a
	s.assigned[a.GroupAssignmentID] = &cp
	return true, nil
}
func (s *stubGroupStore) GetGroupAssignment(_ context.Context, id string) (*domain.GroupAssignment, error) {
	a, ok := s.assigned[id]
	if !ok {
		return nil, domain.ErrGroupAssignmentNotFound
	}
	cp := *a
	return &cp, nil
}
func (s *stubGroupStore) ListGroupAssignments(_ context.Context, groupID string, activeOnly bool) ([]domain.GroupAssignment, error) {
	out := []domain.GroupAssignment{}
	for _, a := range s.assigned {
		if a.GroupID == groupID && (!activeOnly || a.Status == domain.GroupAssignmentActive) {
			out = append(out, *a)
		}
	}
	return out, nil
}
func (s *stubGroupStore) MarkGroupAssignmentRevoked(_ context.Context, id, by, reason string) (*domain.GroupAssignment, error) {
	a := s.assigned[id]
	if a.Status != domain.GroupAssignmentActive {
		return nil, domain.ErrGroupAssignmentRevoked
	}
	a.Status, a.RevokedByPrincipalID, a.RevocationReason = domain.GroupAssignmentRevoked, by, reason
	cp := *a
	return &cp, nil
}
func (s *stubGroupStore) ListGroupAssignmentRequests(_ context.Context, gaID, principal string) ([]domain.AssignmentRequest, error) {
	out := []domain.AssignmentRequest{}
	for _, a := range s.gov.assignments {
		if a.GroupAssignmentID == gaID && (principal == "" || a.TargetPrincipalID == principal) {
			out = append(out, *a)
		}
	}
	return out, nil
}

// ── helpers ──────────────────────────────────────────────────────────────────

func createGroup(t *testing.T, g *govRig, members ...string) string {
	t.Helper()
	rr := doReq(g.r, http.MethodPost, "/v1/iam/groups/", map[string]any{
		"legal_entity_id": "le-1", "group_code": "AP_TEAM_" + uuid.NewString()[:6], "group_name": "AP team", "correlation_id": uuid.NewString(),
	}, "admin-1")
	if rr.Code != http.StatusCreated {
		t.Fatalf("create group: %d %s", rr.Code, rr.Body.String())
	}
	id := decodeBody[domain.Group](t, rr.Body.Bytes()).GroupID
	for _, m := range members {
		if _, err := g.grp.AddGroupMember(context.Background(), id, m, "admin-1"); err != nil {
			t.Fatal(err)
		}
	}
	return id
}

func assignGroup(t *testing.T, g *govRig, groupID, roleID string) domain.GroupAssignment {
	t.Helper()
	rr := doReq(g.r, http.MethodPost, "/v1/iam/groups/"+groupID+"/assignments", map[string]any{
		"legal_entity_id": "le-1", "role_definition_id": roleID, "justification": "AP team access", "correlation_id": uuid.NewString(),
	}, "admin-1")
	if rr.Code != http.StatusCreated {
		t.Fatalf("assign group: %d %s", rr.Code, rr.Body.String())
	}
	return decodeBody[domain.GroupAssignment](t, rr.Body.Bytes())
}

func outcomes(ga domain.GroupAssignment) map[string]domain.GroupMemberOutcome {
	out := map[string]domain.GroupMemberOutcome{}
	for _, o := range ga.Members {
		out[o.PrincipalID] = o
	}
	return out
}

// ── tests ────────────────────────────────────────────────────────────────────

// Each member passes the individual pipeline: a STANDARD role provisions,
// the requester's own membership needs an independent approver, and a member
// whose existing access conflicts is refused alone.
func TestGroupAssignment_FansOutPerMemberThroughPolicy(t *testing.T) {
	g := newGovRig()
	role := g.seedRole("LEGAL_ENTITY", domain.RoleStatusActive, "supplier_invoice.create")
	g.sod.decide = func(r domain.SoDCheckRequest) error {
		if r.SubjectPrincipalID == "user-2" {
			return domain.ErrSoDConflict
		}
		return nil
	}
	gid := createGroup(t, g, "user-1", "user-2", "admin-1")
	ga := assignGroup(t, g, gid, role.RoleDefinitionID)
	o := outcomes(ga)

	if o["user-1"].Status != domain.AssignmentProvisioned {
		t.Errorf("user-1 = %+v; want PROVISIONED", o["user-1"])
	}
	if o["user-2"].Error != "sod_conflict" || o["user-2"].RequestID != "" {
		t.Errorf("user-2 = %+v; want refused sod_conflict with no request", o["user-2"])
	}
	if o["admin-1"].Status != domain.AssignmentPendingApproval {
		t.Errorf("admin-1 = %+v; the requester's own membership must wait for an independent approver", o["admin-1"])
	}
	if len(g.asg.creates) != 1 || g.asg.creates[0].principal != "user-1" {
		t.Fatalf("provisioned = %+v; only user-1 may reach authorization-svc", g.asg.creates)
	}
	a, _ := g.gov.GetAssignmentRequest(context.Background(), o["user-1"].RequestID)
	if a.GroupAssignmentID != ga.GroupAssignmentID || g.asg.creates[0].scope.ApprovalReference != a.RequestID {
		t.Errorf("member request not linked to its group assignment: %+v", a)
	}
}

// A20: a protected / CRITICAL role reached through a group enters the approval
// path for every member; nothing is provisioned on submission.
func TestGroupAssignment_CriticalRoleEntersApprovalPerMember(t *testing.T) {
	g := newGovRig()
	role := g.seedRole("LEGAL_ENTITY", domain.RoleStatusActive, "payment.release")
	gid := createGroup(t, g, "user-1", "user-2")
	ga := assignGroup(t, g, gid, role.RoleDefinitionID)
	for p, o := range outcomes(ga) {
		if o.Status != domain.AssignmentPendingApproval {
			t.Errorf("%s = %+v; want PENDING_APPROVAL", p, o)
		}
	}
	if len(g.asg.creates) != 0 {
		t.Fatalf("a CRITICAL role was provisioned through a group without approval: %+v", g.asg.creates)
	}
}

// Join fans the ACTIVE group assignments out to the new member; leave ends
// what membership gave them, in authorization-svc and here.
func TestGroupMember_JoinFansOutAndLeaveRevokes(t *testing.T) {
	g := newGovRig()
	role := g.seedRole("LEGAL_ENTITY", domain.RoleStatusActive, "supplier_invoice.create")
	gid := createGroup(t, g, "user-1")
	ga := assignGroup(t, g, gid, role.RoleDefinitionID)

	rr := doReq(g.r, http.MethodPost, "/v1/iam/groups/"+gid+"/members", map[string]any{
		"legal_entity_id": "le-1", "principal_id": "user-3", "correlation_id": uuid.NewString(),
	}, "admin-1")
	if rr.Code != http.StatusCreated {
		t.Fatalf("join: %d %s", rr.Code, rr.Body.String())
	}
	if n := len(g.asg.creates); n != 2 || g.asg.creates[1].principal != "user-3" {
		t.Fatalf("join must provision the new member: %+v", g.asg.creates)
	}
	reqs, _ := g.grp.ListGroupAssignmentRequests(context.Background(), ga.GroupAssignmentID, "user-3")
	if len(reqs) != 1 || reqs[0].Status != domain.AssignmentProvisioned {
		t.Fatalf("user-3 requests = %+v", reqs)
	}

	rr = doReq(g.r, http.MethodPost, "/v1/iam/groups/"+gid+"/members/user-3:remove", map[string]any{
		"legal_entity_id": "le-1", "reason": "moved to treasury", "correlation_id": uuid.NewString(),
	}, "admin-1")
	if rr.Code != http.StatusOK {
		t.Fatalf("leave: %d %s", rr.Code, rr.Body.String())
	}
	if len(g.asg.revokes) != 1 || g.asg.revokes[0] != reqs[0].AuthzAssignmentID || g.asg.revokeScope[0].ReasonCode != "GROUP_MEMBERSHIP_ENDED" {
		t.Fatalf("leave must revoke the member's grant there: %v %+v", g.asg.revokes, g.asg.revokeScope)
	}
	a, _ := g.gov.GetAssignmentRequest(context.Background(), reqs[0].RequestID)
	if a.Status != domain.AssignmentRevoked {
		t.Errorf("request = %s; want REVOKED", a.Status)
	}
	grp, _ := g.grp.GetGroup(context.Background(), gid)
	if len(grp.Members) != 1 || grp.Members[0].PrincipalID != "user-1" {
		t.Errorf("members = %+v; user-3 must be gone and user-1 untouched", grp.Members)
	}
}

// Revoking a group assignment cancels pending member requests, revokes the
// provisioned ones, then closes it; a second revoke is a conflict.
func TestGroupAssignment_RevokeEndsEveryMember(t *testing.T) {
	g := newGovRig()
	role := g.seedRole("LEGAL_ENTITY", domain.RoleStatusActive, "supplier_invoice.create")
	gid := createGroup(t, g, "user-1", "admin-1")
	ga := assignGroup(t, g, gid, role.RoleDefinitionID)
	revoke := func() (int, []byte) {
		rr := doReq(g.r, http.MethodPost, "/v1/iam/group-assignments/"+ga.GroupAssignmentID+":revoke", map[string]any{
			"legal_entity_id": "le-1", "reason": "team disbanded", "correlation_id": uuid.NewString(),
		}, "admin-1")
		return rr.Code, rr.Body.Bytes()
	}
	code, raw := revoke()
	if code != http.StatusOK {
		t.Fatalf("revoke: %d %s", code, raw)
	}
	out := decodeBody[domain.GroupAssignment](t, raw)
	if out.Status != domain.GroupAssignmentRevoked {
		t.Errorf("status = %s", out.Status)
	}
	var got []string
	for _, o := range out.Members {
		got = append(got, o.PrincipalID+"="+o.Status)
	}
	sort.Strings(got)
	if len(got) != 2 || got[0] != "admin-1="+domain.AssignmentCancelled || got[1] != "user-1="+domain.AssignmentRevoked {
		t.Fatalf("ended = %v; want admin-1 cancelled, user-1 revoked", got)
	}
	if code, raw = revoke(); code != http.StatusConflict || govCode(raw) != "group_assignment_revoked" {
		t.Errorf("second revoke: %d %s", code, raw)
	}
}

// A ROLE_MANAGE holder of another entity cannot administer this entity's
// group by naming their own entity in the body.
func TestGroup_CommandsBoundToGroupEntity(t *testing.T) {
	g := newGovRig()
	role := g.seedRole("LEGAL_ENTITY", domain.RoleStatusActive, "supplier_invoice.create")
	gid := createGroup(t, g, "user-1")
	rr := doReq(g.r, http.MethodPost, "/v1/iam/groups/"+gid+"/assignments", map[string]any{
		"legal_entity_id": "le-2", "role_definition_id": role.RoleDefinitionID, "justification": "x", "correlation_id": uuid.NewString(),
	}, "admin-1")
	if rr.Code != http.StatusBadRequest || govCode(rr.Body.Bytes()) != "entity_mismatch" {
		t.Fatalf("want 400 entity_mismatch, got %d %s", rr.Code, rr.Body.String())
	}
	rr = doReq(g.r, http.MethodPost, "/v1/iam/groups/"+gid+"/members", map[string]any{
		"legal_entity_id": "le-2", "principal_id": "user-9", "correlation_id": uuid.NewString(),
	}, "admin-1")
	if rr.Code != http.StatusBadRequest || len(g.asg.creates) != 0 {
		t.Fatalf("member add across entities: %d %s", rr.Code, rr.Body.String())
	}
}

// Without ROLE_MANAGE nothing about a group can be changed.
func TestGroup_RequiresRoleManage(t *testing.T) {
	g := newGovRig()
	g.authz.deny["ROLE_MANAGE"] = true
	rr := doReq(g.r, http.MethodPost, "/v1/iam/groups/", map[string]any{
		"legal_entity_id": "le-1", "group_code": "X", "group_name": "X", "correlation_id": uuid.NewString(),
	}, "user-1")
	if rr.Code != http.StatusForbidden {
		t.Fatalf("want 403, got %d %s", rr.Code, rr.Body.String())
	}
}

// Sync re-runs the fan-out idempotently: members already served replay.
func TestGroupAssignment_SyncIsIdempotent(t *testing.T) {
	g := newGovRig()
	role := g.seedRole("LEGAL_ENTITY", domain.RoleStatusActive, "supplier_invoice.create")
	gid := createGroup(t, g, "user-1", "user-2")
	ga := assignGroup(t, g, gid, role.RoleDefinitionID)
	rr := doReq(g.r, http.MethodPost, "/v1/iam/group-assignments/"+ga.GroupAssignmentID+":sync", map[string]any{
		"legal_entity_id": "le-1", "correlation_id": uuid.NewString(),
	}, "admin-1")
	if rr.Code != http.StatusOK {
		t.Fatalf("sync: %d %s", rr.Code, rr.Body.String())
	}
	if len(g.asg.creates) != 2 || len(g.gov.assignments) != 2 {
		t.Fatalf("sync doubled the fan-out: %d creates, %d requests", len(g.asg.creates), len(g.gov.assignments))
	}
}
