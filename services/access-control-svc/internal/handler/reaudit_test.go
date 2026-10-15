package handler_test

import (
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/google/uuid"

	"zoiko.io/access-control-svc/internal/domain"
)

// Tests for the 7 Oct 2026 re-audit fixes:
//
//	F1 a command's legal_entity_id is bound to the record's own entity
//	F2 an end date at grant time (§9 / §22 "effective dates")
//	F3 effective-dated revocation (§9 "immediate or effective-dated removal")
//	F4 a CRITICAL request needs a security approver (§9 "... depending on risk")
//	F5 toxic combinations across a tenant-wide and an entity role (§24)
//	F6 dormancy and inactive-subject flags (§24 "Dormancy", "Orphan detection")

func cmd(g *govRig, verb, caller, id string, body map[string]any) (int, []byte) {
	b := map[string]any{"legal_entity_id": "le-1", "reason": "checked", "correlation_id": uuid.NewString()}
	for k, v := range body {
		b[k] = v
	}
	rr := doReq(g.r, http.MethodPost, "/v1/iam/access-assignments/"+id+":"+verb, b, caller)
	return rr.Code, rr.Body.Bytes()
}

// ── F1 ──────────────────────────────────────────────────────────────────────

func TestAssignmentCommand_EntityBoundToTheRequest(t *testing.T) {
	g := newGovRig()
	role := g.seedRole("LEGAL_ENTITY", domain.RoleStatusActive, "payment.release")
	_, raw := requestAssignment(g, "admin-1", "user-7", role.RoleDefinitionID, uuid.NewString())
	a := decodeBody[domain.AssignmentRequest](t, raw)

	// A manager of le-2 names le-2 (where begin's ROLE_MANAGE check passes)
	// to reject a request that belongs to le-1.
	for _, verb := range []string{"reject", "approve", "cancel"} {
		caller := "approver-2"
		if verb == "cancel" {
			caller = "admin-1"
		}
		code, raw := cmd(g, verb, caller, a.RequestID, map[string]any{"legal_entity_id": "le-2"})
		if code != http.StatusBadRequest || govCode(raw) != "entity_mismatch" {
			t.Fatalf("%s with another entity: want 400 entity_mismatch, got %d %s", verb, code, raw)
		}
	}
	if g.gov.assignments[a.RequestID].Status != domain.AssignmentPendingApproval {
		t.Fatal("a refused command changed the request")
	}
	if len(g.asg.creates) != 0 {
		t.Fatal("nothing may be provisioned on an entity mismatch")
	}
	if n := len(g.base.refusals); n == 0 || g.base.refusals[n-1].ErrorCode != "entity_mismatch" {
		t.Fatalf("refusal not recorded: %+v", g.base.refusals)
	}
}

func TestCampaignCommands_EntityBoundToTheCampaign(t *testing.T) {
	g := newGovRig()
	role := g.seedRole("LEGAL_ENTITY", domain.RoleStatusActive, "supplier_invoice.create")
	g.asg.assignments[role.RoleDefinitionID] = []domain.AuthzAssignment{{PrincipalRoleAssignmentID: uuid.NewString(), PrincipalID: "user-7", RoleID: role.RoleDefinitionID, LegalEntityID: entity("le-1")}}
	_, raw := createCampaign(g, nil)
	c := decodeBody[domain.ReviewCampaign](t, raw)
	itemID := c.Items[0].ItemID

	rr := doReq(g.r, http.MethodPost, "/v1/access-review-campaigns/"+c.CampaignID+"/complete", map[string]any{
		"legal_entity_id": "le-2", "correlation_id": uuid.NewString(),
	}, "admin-1")
	if rr.Code != http.StatusBadRequest || govCode(rr.Body.Bytes()) != "entity_mismatch" {
		t.Fatalf("complete: want 400 entity_mismatch, got %d %s", rr.Code, rr.Body.String())
	}
	rr = doReq(g.r, http.MethodPost, "/v1/access-review-campaigns/"+c.CampaignID+"/items/"+itemID+"/reassign", map[string]any{
		"legal_entity_id": "le-2", "reviewer_principal_id": "mgr-9", "correlation_id": uuid.NewString(),
	}, "admin-1")
	if rr.Code != http.StatusBadRequest || govCode(rr.Body.Bytes()) != "entity_mismatch" {
		t.Fatalf("reassign: want 400 entity_mismatch, got %d %s", rr.Code, rr.Body.String())
	}
	rr = doReq(g.r, http.MethodPost, "/v1/access-review-campaigns/"+c.CampaignID+"/items/"+itemID+"/decide", map[string]any{
		"legal_entity_id": "le-2", "decision": "REVOKE", "reason": "left", "correlation_id": uuid.NewString(),
	}, "mgr-1")
	if rr.Code != http.StatusBadRequest || govCode(rr.Body.Bytes()) != "entity_mismatch" {
		t.Fatalf("decide: want 400 entity_mismatch, got %d %s", rr.Code, rr.Body.String())
	}
	if len(g.asg.revokes) != 0 || g.gov.campaigns[c.CampaignID].Status != domain.CampaignOpen {
		t.Fatal("a refused campaign command acted")
	}
}

// ── F2 ──────────────────────────────────────────────────────────────────────

func TestRequestAssignment_EndDateProvisionedAndRecorded(t *testing.T) {
	g := newGovRig()
	role := g.seedRole("LEGAL_ENTITY", domain.RoleStatusActive, "supplier_invoice.create")
	end := time.Now().Add(30 * 24 * time.Hour).UTC().Truncate(time.Second)
	rr := doReq(g.r, http.MethodPost, "/v1/iam/access-assignments/", map[string]any{
		"legal_entity_id": "le-1", "target_principal_id": "contractor-1", "role_definition_id": role.RoleDefinitionID,
		"justification": "fixed-term contract", "effective_to": end, "correlation_id": uuid.NewString(),
	}, "admin-1")
	if rr.Code != http.StatusCreated {
		t.Fatalf("want 201, got %d %s", rr.Code, rr.Body.String())
	}
	a := decodeBody[domain.AssignmentRequest](t, rr.Body.Bytes())
	if a.EffectiveTo == nil || !a.EffectiveTo.Equal(end) {
		t.Fatalf("end date not recorded: %+v", a.EffectiveTo)
	}
	if len(g.asg.creates) != 1 || g.asg.creates[0].effectiveTo == nil || !g.asg.creates[0].effectiveTo.Equal(end) {
		t.Fatalf("end date not provisioned into authorization-svc: %+v", g.asg.creates)
	}
}

func TestRequestAssignment_EndDateValidated(t *testing.T) {
	for name, body := range map[string]map[string]any{
		"in the past":        {"effective_to": time.Now().Add(-time.Hour)},
		"before the start":   {"effective_from": time.Now().Add(48 * time.Hour), "effective_to": time.Now().Add(24 * time.Hour)},
		"equal to the start": {"effective_from": time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC), "effective_to": time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC)},
	} {
		t.Run(name, func(t *testing.T) {
			g := newGovRig()
			role := g.seedRole("LEGAL_ENTITY", domain.RoleStatusActive, "supplier_invoice.create")
			b := map[string]any{
				"legal_entity_id": "le-1", "target_principal_id": "contractor-1", "role_definition_id": role.RoleDefinitionID,
				"justification": "x", "correlation_id": uuid.NewString(),
			}
			for k, v := range body {
				b[k] = v
			}
			rr := doReq(g.r, http.MethodPost, "/v1/iam/access-assignments/", b, "admin-1")
			if rr.Code != http.StatusBadRequest || govCode(rr.Body.Bytes()) != "invalid_effective_to" {
				t.Fatalf("want 400 invalid_effective_to, got %d %s", rr.Code, rr.Body.String())
			}
			if len(g.asg.creates) != 0 {
				t.Fatal("an invalid end date reached authorization-svc")
			}
		})
	}
}

func TestApproveAssignment_ElapsedWindowRefused(t *testing.T) {
	g := newGovRig()
	role := g.seedRole("LEGAL_ENTITY", domain.RoleStatusActive, "payment.release")
	_, raw := requestAssignment(g, "admin-1", "user-7", role.RoleDefinitionID, uuid.NewString())
	a := decodeBody[domain.AssignmentRequest](t, raw)
	past := time.Now().Add(-time.Minute)
	g.gov.assignments[a.RequestID].EffectiveTo = &past // the window ran out while it waited

	code, raw := cmd(g, "approve", "approver-2", a.RequestID, nil)
	if code != http.StatusConflict || govCode(raw) != "assignment_window_elapsed" {
		t.Fatalf("want 409 assignment_window_elapsed, got %d %s", code, raw)
	}
	if len(g.asg.creates) != 0 {
		t.Fatal("an elapsed assignment was provisioned")
	}
}

// ── F3 ──────────────────────────────────────────────────────────────────────

func provisioned(t *testing.T, g *govRig) domain.AssignmentRequest {
	t.Helper()
	role := g.seedRole("LEGAL_ENTITY", domain.RoleStatusActive, "supplier_invoice.create")
	_, raw := requestAssignment(g, "admin-1", "user-7", role.RoleDefinitionID, uuid.NewString())
	return decodeBody[domain.AssignmentRequest](t, raw)
}

func TestRevokeAssignment_EffectiveDatedSchedulesTheEnd(t *testing.T) {
	g := newGovRig()
	a := provisioned(t, g)
	at := time.Now().Add(14 * 24 * time.Hour).UTC().Truncate(time.Second)
	code, raw := cmd(g, "revoke", "admin-1", a.RequestID, map[string]any{"effective_at": at, "reason": "moves team on the 20th"})
	if code != http.StatusOK {
		t.Fatalf("want 200, got %d %s", code, raw)
	}
	out := decodeBody[domain.AssignmentRequest](t, raw)
	if out.Status != domain.AssignmentProvisioned || out.EffectiveTo == nil || !out.EffectiveTo.Equal(at) || out.RevokedByPrincipalID != "admin-1" {
		t.Fatalf("schedule not recorded: %+v", out)
	}
	if len(g.asg.schedules) != 1 || g.asg.schedules[0].assignmentID != a.AuthzAssignmentID || !g.asg.schedules[0].at.Equal(at) {
		t.Fatalf("end not scheduled in authorization-svc: %+v", g.asg.schedules)
	}
	if len(g.asg.revokes) != 0 {
		t.Fatal("an effective-dated revoke must not revoke now")
	}
	if g.gov.events[len(g.gov.events)-1] == "iam.assignment.revoked" {
		t.Fatal("iam.assignment.revoked must wait for the instant, or sessions end early")
	}
}

func TestRevokeAssignment_EffectiveAtValidated(t *testing.T) {
	g := newGovRig()
	a := provisioned(t, g)
	code, raw := cmd(g, "revoke", "admin-1", a.RequestID, map[string]any{"effective_at": time.Now().Add(-time.Hour)})
	if code != http.StatusBadRequest || govCode(raw) != "invalid_effective_at" {
		t.Fatalf("past instant: want 400 invalid_effective_at, got %d %s", code, raw)
	}
	end := time.Now().Add(24 * time.Hour)
	g.gov.assignments[a.RequestID].EffectiveTo = &end
	code, raw = cmd(g, "revoke", "admin-1", a.RequestID, map[string]any{"effective_at": end.Add(time.Hour)})
	if code != http.StatusConflict || govCode(raw) != "ends_sooner" {
		t.Fatalf("later than the existing end: want 409 ends_sooner, got %d %s", code, raw)
	}
	if len(g.asg.schedules) != 0 {
		t.Fatal("a refused schedule reached authorization-svc")
	}
}

func TestRevokeAssignment_ScheduleOnEndedAssignmentClosesIt(t *testing.T) {
	g := newGovRig()
	a := provisioned(t, g)
	g.asg.scheduleErr = fmt.Errorf("%w: returned 404", domain.ErrAuthzAssignmentAbsent)
	code, raw := cmd(g, "revoke", "admin-1", a.RequestID, map[string]any{"effective_at": time.Now().Add(time.Hour)})
	if code != http.StatusOK || decodeBody[domain.AssignmentRequest](t, raw).Status != domain.AssignmentRevoked {
		t.Fatalf("already ended there: want 200 REVOKED, got %d %s", code, raw)
	}
}

// ── F4 ──────────────────────────────────────────────────────────────────────

func TestApproveAssignment_CriticalNeedsSecurityApprover(t *testing.T) {
	g := newGovRig()
	role := g.seedRole("LEGAL_ENTITY", domain.RoleStatusActive, "payment.release") // CRITICAL in the rig
	_, raw := requestAssignment(g, "admin-1", "user-7", role.RoleDefinitionID, uuid.NewString())
	a := decodeBody[domain.AssignmentRequest](t, raw)
	if a.RiskTier != domain.RiskCritical {
		t.Fatalf("fixture: want CRITICAL, got %s", a.RiskTier)
	}

	g.authz.deny[domain.ActionApprovePrivileged] = true
	code, raw := cmd(g, "approve", "approver-2", a.RequestID, nil)
	if code != http.StatusForbidden || govCode(raw) != "security_approval_required" {
		t.Fatalf("manager-only approver: want 403 security_approval_required, got %d %s", code, raw)
	}
	if len(g.asg.creates) != 0 || g.gov.assignments[a.RequestID].Status != domain.AssignmentPendingApproval {
		t.Fatal("a CRITICAL grant was provisioned without security approval")
	}

	delete(g.authz.deny, domain.ActionApprovePrivileged)
	if code, raw := cmd(g, "approve", "security-1", a.RequestID, nil); code != http.StatusOK {
		t.Fatalf("security approver: want 200, got %d %s", code, raw)
	}
}

func TestApproveAssignment_HighNeedsNoSecurityApprover(t *testing.T) {
	g := newGovRig()
	g.tax.risk["journal.post"] = domain.RiskHigh
	role := g.seedRole("LEGAL_ENTITY", domain.RoleStatusActive, "journal.post")
	_, raw := requestAssignment(g, "admin-1", "user-7", role.RoleDefinitionID, uuid.NewString())
	a := decodeBody[domain.AssignmentRequest](t, raw)
	g.authz.deny[domain.ActionApprovePrivileged] = true
	if code, raw := cmd(g, "approve", "approver-2", a.RequestID, nil); code != http.StatusOK {
		t.Fatalf("HIGH is the manager tier: want 200, got %d %s", code, raw)
	}
	for _, act := range g.authz.asked {
		if act == domain.ActionApprovePrivileged {
			t.Fatal("security approval was asked for a HIGH request")
		}
	}
}

// ── F5 ──────────────────────────────────────────────────────────────────────

func TestCreateCampaign_ToxicCombinationAcrossTenantWideAndEntityRole(t *testing.T) {
	g := newGovRig()
	prep := g.seedRole("TENANT", domain.RoleStatusActive, "payment.prepare")
	rel := g.seedRole("LEGAL_ENTITY", domain.RoleStatusActive, "payment.release")
	// Tenant-wide assignments carry no entity; the preparer duty applies in
	// le-1 as much as anywhere.
	g.asg.assignments[prep.RoleDefinitionID] = []domain.AuthzAssignment{{PrincipalRoleAssignmentID: uuid.NewString(), PrincipalID: "user-7", RoleID: prep.RoleDefinitionID}}
	g.asg.assignments[rel.RoleDefinitionID] = []domain.AuthzAssignment{{PrincipalRoleAssignmentID: uuid.NewString(), PrincipalID: "user-7", RoleID: rel.RoleDefinitionID, LegalEntityID: entity("le-1")}}
	g.sod.decide = func(req domain.SoDCheckRequest) error {
		has := map[string]bool{}
		for _, a := range req.CandidateActions {
			has[a] = true
		}
		if has["payment.prepare"] && has["payment.release"] {
			return &domain.SoDConflictError{Conflicts: []domain.SoDConflict{{CandidateAction: "payment.prepare", ConflictsWith: "payment.release"}}}
		}
		return nil
	}
	code, raw := createCampaign(g, nil)
	if code != http.StatusCreated {
		t.Fatalf("want 201, got %d %s", code, raw)
	}
	c := decodeBody[domain.ReviewCampaign](t, raw)
	flagged := 0
	for _, it := range c.Items {
		if fmt.Sprint(it.Flags) == "[SOD_CONFLICT]" {
			flagged++
		}
	}
	if flagged != 2 {
		t.Fatalf("both halves of the tenant-wide + entity conflict must be flagged once each; items: %+v", c.Items)
	}
}

// ── F6 ──────────────────────────────────────────────────────────────────────

func TestCreateCampaign_DormantAndInactiveSubjectFlagged(t *testing.T) {
	g := newGovRig()
	crit := g.seedRole("LEGAL_ENTITY", domain.RoleStatusActive, "payment.release") // CRITICAL
	std := g.seedRole("LEGAL_ENTITY", domain.RoleStatusActive, "supplier_invoice.create")
	old := time.Now().AddDate(0, -6, 0)
	recent := time.Now().AddDate(0, 0, -3)
	g.asg.assignments[crit.RoleDefinitionID] = []domain.AuthzAssignment{
		// held six months, never used: dormant
		{PrincipalRoleAssignmentID: uuid.NewString(), PrincipalID: "user-1", RoleID: crit.RoleDefinitionID, LegalEntityID: entity("le-1"), EffectiveFrom: old, PrincipalStatus: "ACTIVE"},
		// held six months, used three days ago: not dormant
		{PrincipalRoleAssignmentID: uuid.NewString(), PrincipalID: "user-2", RoleID: crit.RoleDefinitionID, LegalEntityID: entity("le-1"), EffectiveFrom: old, LastGrantedAt: &recent, PrincipalStatus: "ACTIVE"},
		// granted yesterday: inside the window, not dormant
		{PrincipalRoleAssignmentID: uuid.NewString(), PrincipalID: "user-3", RoleID: crit.RoleDefinitionID, LegalEntityID: entity("le-1"), EffectiveFrom: time.Now().AddDate(0, 0, -1), PrincipalStatus: "ACTIVE"},
	}
	g.asg.assignments[std.RoleDefinitionID] = []domain.AuthzAssignment{
		// STANDARD and unused: never flagged dormant; but the subject is suspended
		{PrincipalRoleAssignmentID: uuid.NewString(), PrincipalID: "user-4", RoleID: std.RoleDefinitionID, LegalEntityID: entity("le-1"), EffectiveFrom: old, PrincipalStatus: "SUSPENDED"},
	}
	code, raw := createCampaign(g, nil)
	if code != http.StatusCreated {
		t.Fatalf("want 201, got %d %s", code, raw)
	}
	c := decodeBody[domain.ReviewCampaign](t, raw)
	if c.DormancyDays != domain.DefaultDormancyDays {
		t.Fatalf("dormancy window not recorded: %d", c.DormancyDays)
	}
	want := map[string]string{"user-1": "[DORMANT]", "user-2": "[]", "user-3": "[]", "user-4": "[SUBJECT_INACTIVE]"}
	for _, it := range c.Items {
		if got := fmt.Sprint(it.Flags); got != want[it.TargetPrincipalID] {
			t.Errorf("%s: flags %s, want %s", it.TargetPrincipalID, got, want[it.TargetPrincipalID])
		}
		if it.TargetPrincipalID == "user-4" && (it.RiskTier != domain.RiskHigh || it.SubjectStatus != "SUSPENDED") {
			t.Errorf("inactive subject must be raised to HIGH with its status as evidence: %+v", it)
		}
		if it.TargetPrincipalID == "user-2" && (it.LastGrantedAt == nil || !it.LastGrantedAt.Equal(recent)) {
			t.Errorf("last use not carried as evidence: %+v", it.LastGrantedAt)
		}
	}
}

func TestCreateCampaign_DormancyWindowValidated(t *testing.T) {
	g := newGovRig()
	code, raw := createCampaign(g, map[string]any{"dormancy_days": 5000})
	if code != http.StatusBadRequest || govCode(raw) != "invalid_dormancy_days" {
		t.Fatalf("want 400 invalid_dormancy_days, got %d %s", code, raw)
	}
}
