package handler

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"zoiko.io/access-control-svc/internal/domain"
	"zoiko.io/access-control-svc/internal/telemetry"
)

// Event-triggered reviews (Authorization Standard §24; audit gap S9-C2).
//
// HR events name employees; access is held by principals; nothing on the
// estate maps one to the other. This file serves the administered mapping
// (POST /v1/iam/subject-links, ROLE_MANAGE) and TriggerReview, which the
// HR-event consumer (internal/hrevents) calls to open an EVENT_TRIGGERED
// review of ONE subject's assignments. The review is created by this
// service's own identity; its items are decided by people, exactly as any
// other campaign's are — an HR event opens a review, it never revokes.

// SubjectLinkStore holds the employee-to-principal links (000015).
type SubjectLinkStore interface {
	UpsertSubjectLink(ctx context.Context, l *domain.SubjectLink) error
	ListSubjectLinks(ctx context.Context) ([]domain.SubjectLink, error)
	FindSubjectLink(ctx context.Context, employeeID string) (*domain.SubjectLink, error)
	ObserveSubject(ctx context.Context, employeeID, managerEmployeeID, status string) (*domain.SubjectLink, error)
}

// SetSubjectLinks enables the subject-link routes and event-triggered
// reviews. reviewer is the default reviewer of an event review (and the
// escalation reviewer when the subject's manager is the subject); due is how
// long the review stays open.
func (g *Gov) SetSubjectLinks(s SubjectLinkStore, reviewer string, due time.Duration) {
	g.links, g.eventReviewer, g.eventReviewDue = s, reviewer, due
}

func registerSubjectLinkRoutes(r chi.Router, g *Gov) {
	r.Post("/v1/iam/subject-links", g.LinkSubject)
	r.Get("/v1/iam/subject-links", g.ListSubjectLinks)
}

// LinkSubject handles POST /v1/iam/subject-links.
func (g *Gov) LinkSubject(w http.ResponseWriter, r *http.Request) {
	const op = telemetry.GovLinkSubject
	var req domain.LinkSubjectRequest
	if err := decode(r, &req); err != nil {
		g.refuse(w, r, op, "", req.LegalEntityID, "", req.CorrelationID, http.StatusBadRequest, "invalid_json", err.Error(), req)
		return
	}
	if req.LegalEntityID == "" || strings.TrimSpace(req.EmployeeID) == "" || strings.TrimSpace(req.PrincipalID) == "" || req.CorrelationID == "" {
		g.refuse(w, r, op, "", req.LegalEntityID, "", req.CorrelationID, http.StatusBadRequest, "missing_fields",
			"legal_entity_id, employee_id, principal_id and correlation_id are required", req)
		return
	}
	principalID, tenantID, ok := g.begin(w, r, op, req.LegalEntityID, req.CorrelationID, req)
	if !ok {
		return
	}
	l := &domain.SubjectLink{EmployeeID: strings.TrimSpace(req.EmployeeID), PrincipalID: strings.TrimSpace(req.PrincipalID),
		LegalEntityID: req.LegalEntityID, LinkedByPrincipalID: principalID, CorrelationID: req.CorrelationID}
	if err := g.links.UpsertSubjectLink(r.Context(), l); err != nil {
		g.fail(w, r, op, tenantID, req.LegalEntityID, principalID, req.CorrelationID, err, req)
		return
	}
	g.count(op, telemetry.WriteUpdated)
	writeJSON(w, http.StatusOK, l)
}

func (g *Gov) ListSubjectLinks(w http.ResponseWriter, r *http.Request) {
	if _, ok := g.reader(w, r); !ok {
		return
	}
	list, err := g.links.ListSubjectLinks(r.Context())
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, "store_unavailable", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, list)
}

// ErrEventReviewsDisabled: no service identity or default reviewer is
// configured, so an HR event cannot open a review.
var ErrEventReviewsDisabled = errors.New("event-triggered reviews need ACS_SERVICE_PRINCIPAL_ID and ACS_EVENT_REVIEW_DEFAULT_REVIEWER")

// TriggerReview opens an EVENT_TRIGGERED review of one subject's assignments.
// ctx must carry the trigger's tenant. Idempotent on t.CorrelationID. Returns
// (nil, false, nil) when the subject holds nothing to review.
func (g *Gov) TriggerReview(ctx context.Context, t domain.ReviewTrigger) (*domain.ReviewCampaign, bool, error) {
	if g.servicePrincipalID == "" || g.eventReviewer == "" {
		return nil, false, ErrEventReviewsDisabled
	}
	reviewer := t.ReviewerPrincipalID
	if reviewer == "" {
		reviewer = g.eventReviewer
	}
	escalation := ""
	if reviewer != g.eventReviewer {
		escalation = g.eventReviewer
	}
	due := g.eventReviewDue
	if due <= 0 {
		due = 7 * 24 * time.Hour
	}
	name := "Event review: " + t.Reason
	if len(name) > 200 {
		name = name[:200]
	}
	return g.buildCampaign(ctx, t.TenantID, g.servicePrincipalID, domain.CreateCampaignRequest{
		LegalEntityID: t.LegalEntityID, CampaignName: name, ReviewType: "EVENT_TRIGGERED", TriggerReason: t.Reason,
		DefaultReviewerPrincipalID: reviewer, EscalationReviewerPrincipalID: escalation, SubjectPrincipalID: t.SubjectPrincipalID,
		DueAt: time.Now().Add(due), DormancyDays: domain.DefaultDormancyDays, CorrelationID: t.CorrelationID,
	}, true)
}

// IsTransient reports whether a governance failure is an outage (answered
// 5xx) rather than a refusal: the HR-event consumer retries the first and
// records the second.
func IsTransient(err error) bool {
	if errors.Is(err, ErrEventReviewsDisabled) {
		return false
	}
	status, _ := classify(err)
	return status >= 500
}
