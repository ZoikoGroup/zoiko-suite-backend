package handler_test

import (
	"context"
	"errors"
	"net/http"
	"testing"

	"github.com/google/uuid"

	"zoiko.io/access-control-svc/internal/domain"
	"zoiko.io/access-control-svc/internal/handler"
	"zoiko.io/access-control-svc/internal/middleware"
)

// S9-C2: the subject-link registry and the reviews HR events open.

type stubLinkStore struct {
	links map[string]*domain.SubjectLink
}

func (s *stubLinkStore) UpsertSubjectLink(_ context.Context, l *domain.SubjectLink) error {
	cp := *l
	s.links[l.EmployeeID] = &cp
	return nil
}
func (s *stubLinkStore) ListSubjectLinks(context.Context) ([]domain.SubjectLink, error) {
	out := []domain.SubjectLink{}
	for _, l := range s.links {
		out = append(out, *l)
	}
	return out, nil
}
func (s *stubLinkStore) FindSubjectLink(_ context.Context, emp string) (*domain.SubjectLink, error) {
	return s.links[emp], nil
}
func (s *stubLinkStore) ObserveSubject(_ context.Context, emp, _, _ string) (*domain.SubjectLink, error) {
	return s.links[emp], nil
}

func TestLinkSubject_RequiresRoleManage(t *testing.T) {
	g := newGovRig()
	body := map[string]any{"legal_entity_id": "le-1", "employee_id": "E-1", "principal_id": "user-1", "correlation_id": uuid.NewString()}
	g.authz.deny["ROLE_MANAGE"] = true
	if rr := doReq(g.r, http.MethodPost, "/v1/iam/subject-links", body, "user-9"); rr.Code != http.StatusForbidden {
		t.Fatalf("without ROLE_MANAGE: %d %s", rr.Code, rr.Body.String())
	}
	delete(g.authz.deny, "ROLE_MANAGE")
	if rr := doReq(g.r, http.MethodPost, "/v1/iam/subject-links", body, "admin-1"); rr.Code != http.StatusOK {
		t.Fatalf("link: %d %s", rr.Code, rr.Body.String())
	}
	if l := g.links.links["E-1"]; l == nil || l.PrincipalID != "user-1" || l.LinkedByPrincipalID != "admin-1" {
		t.Fatalf("link = %+v", l)
	}
}

// The review covers only the subject, is opened by the service identity, and
// is assigned to the trigger's reviewer; a redelivery replays it.
func TestTriggerReview_NarrowsToSubject(t *testing.T) {
	g := newGovRig()
	role := g.seedRole("LEGAL_ENTITY", domain.RoleStatusActive, "supplier_invoice.create")
	g.asg.assignments[role.RoleDefinitionID] = []domain.AuthzAssignment{
		{PrincipalRoleAssignmentID: uuid.NewString(), PrincipalID: "user-7", RoleID: role.RoleDefinitionID},
		{PrincipalRoleAssignmentID: uuid.NewString(), PrincipalID: "user-8", RoleID: role.RoleDefinitionID},
	}
	ctx := middleware.WithTenant(context.Background(), "tenant-abc")
	tr := domain.ReviewTrigger{TenantID: "tenant-abc", LegalEntityID: "le-1", SubjectPrincipalID: "user-7",
		ReviewerPrincipalID: "mgr-2", Reason: "mover: manager changed", CorrelationID: "hr-ev-1"}
	c, created, err := g.gv.TriggerReview(ctx, tr)
	if err != nil || !created {
		t.Fatalf("trigger: %v created=%v", err, created)
	}
	if c.ReviewType != "EVENT_TRIGGERED" || c.CreatedByPrincipalID != "svc-access-control" || c.TriggerReason != tr.Reason {
		t.Errorf("campaign = %+v", c)
	}
	if len(c.Items) != 1 || c.Items[0].TargetPrincipalID != "user-7" || c.Items[0].ReviewerPrincipalID != "mgr-2" {
		t.Fatalf("items = %+v; want only user-7, reviewed by mgr-2", c.Items)
	}
	if _, created, err := g.gv.TriggerReview(ctx, tr); err != nil || created {
		t.Errorf("redelivery: created=%v %v; want a replay", created, err)
	}

	// A subject holding nothing needs no review, and none is recorded.
	tr.SubjectPrincipalID, tr.CorrelationID = "user-404", "hr-ev-2"
	if c, _, err := g.gv.TriggerReview(ctx, tr); err != nil || c != nil {
		t.Errorf("empty review: %+v %v", c, err)
	}
}

// A manager who is the subject never reviews themself: the configured
// reviewer takes the item.
func TestTriggerReview_SubjectIsReviewerEscalates(t *testing.T) {
	g := newGovRig()
	role := g.seedRole("LEGAL_ENTITY", domain.RoleStatusActive, "supplier_invoice.create")
	g.asg.assignments[role.RoleDefinitionID] = []domain.AuthzAssignment{{PrincipalRoleAssignmentID: uuid.NewString(), PrincipalID: "mgr-2", RoleID: role.RoleDefinitionID}}
	ctx := middleware.WithTenant(context.Background(), "tenant-abc")
	c, _, err := g.gv.TriggerReview(ctx, domain.ReviewTrigger{TenantID: "tenant-abc", LegalEntityID: "le-1", SubjectPrincipalID: "mgr-2",
		ReviewerPrincipalID: "mgr-2", Reason: "leaver", CorrelationID: "hr-ev-3"})
	if err != nil || c.Items[0].ReviewerPrincipalID != "sec-1" {
		t.Fatalf("self-review not escalated: %+v %v", c, err)
	}
}

func TestTriggerReview_DisabledWithoutReviewer(t *testing.T) {
	g := newGovRig()
	g.gv.SetSubjectLinks(g.links, "", 0)
	_, _, err := g.gv.TriggerReview(middleware.WithTenant(context.Background(), "tenant-abc"), domain.ReviewTrigger{TenantID: "tenant-abc", SubjectPrincipalID: "u", CorrelationID: "x"})
	if !errors.Is(err, handler.ErrEventReviewsDisabled) || handler.IsTransient(err) {
		t.Fatalf("err = %v", err)
	}
	if !handler.IsTransient(domain.ErrAuthzServiceUnavailable) || handler.IsTransient(domain.ErrEscalationReviewerReq) {
		t.Error("IsTransient must retry outages and not refusals")
	}
}
