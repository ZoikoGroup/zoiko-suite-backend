package handler

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"

	"github.com/go-chi/chi/v5"
	"go.uber.org/zap"

	"zoiko.io/clause-template-svc/internal/authz"
	"zoiko.io/clause-template-svc/internal/domain"
	"zoiko.io/clause-template-svc/internal/events"
	"zoiko.io/clause-template-svc/internal/middleware"
	"zoiko.io/clause-template-svc/internal/store"
)

// Action types passed to authorization-svc for each write operation.
const (
	actionClauseCreate       = "CLAUSE_CREATE"
	actionClauseUpdate       = "CLAUSE_UPDATE"
	actionClauseSubmitReview = "CLAUSE_SUBMIT_LEGAL_REVIEW"
	actionClauseApprove      = "CLAUSE_APPROVE"
	actionClauseActivate     = "CLAUSE_ACTIVATE"
	actionClauseRetire       = "CLAUSE_RETIRE"
	actionClauseSupersede    = "CLAUSE_SUPERSEDE"
	actionTemplateCreate     = "TEMPLATE_CREATE"
	actionTemplateUpdate     = "TEMPLATE_UPDATE"
	actionTemplateApprove    = "TEMPLATE_APPROVE"
	actionDeviationCreate    = "DEVIATION_RULE_CREATE"
	actionDeviationApprove   = "DEVIATION_RULE_APPROVE"
)

// AuthZClient checks whether a principal is allowed to perform an action
// against a legal entity. Implementations must fail closed: any error
// returned here must be treated as "not authorized".
type AuthZClient interface {
	CheckAllowed(ctx context.Context, principalID, legalEntityID, actionType string) error
}

type Handler struct {
	store     store.Store
	publisher events.Publisher
	authz     AuthZClient
	logger    *zap.Logger
}

func New(st store.Store, pub events.Publisher, az AuthZClient, logger *zap.Logger) *Handler {
	return &Handler{store: st, publisher: pub, authz: az, logger: logger}
}

var _ AuthZClient = (*authz.Client)(nil)

func RegisterRoutes(r chi.Router, h *Handler) {
	r.Route("/v1/clauses", func(r chi.Router) {
		r.Post("/", h.CreateClause)
		r.Get("/", h.ListClauses)
		r.Get("/{id}", h.GetClause)
		r.Put("/{id}", h.UpdateClause)
		r.Post("/{id}/submit-legal-review", h.SubmitClauseForLegalReview)
		r.Post("/{id}/approve", h.ApproveClause)
		r.Post("/{id}/activate", h.ActivateClause)
		r.Post("/{id}/retire", h.RetireClause)
		r.Post("/{id}/supersede", h.SupersedeClause)
		r.Get("/{id}/versions", h.ListClauseVersions)
	})

	r.Route("/v1/templates", func(r chi.Router) {
		r.Post("/", h.CreateTemplate)
		r.Get("/", h.ListTemplates)
		r.Get("/{id}", h.GetTemplate)
		r.Put("/{id}", h.UpdateTemplate)
		r.Post("/{id}/approve", h.ApproveTemplate)
	})

	r.Route("/v1/clause-templates", func(r chi.Router) {
		r.Post("/", h.CreateTemplate)
		r.Get("/", h.ListTemplates)
		r.Get("/{id}", h.GetTemplate)
		r.Put("/{id}", h.UpdateTemplate)
	})

	r.Route("/v1/deviation-rules", func(r chi.Router) {
		r.Post("/", h.CreateDeviationRule)
		r.Get("/", h.ListDeviationRules)
		r.Get("/{id}", h.GetDeviationRule)
		r.Post("/{id}/approve", h.ApproveDeviationRule)
	})
}

// --- Clause Handlers ---

func (h *Handler) CreateClause(w http.ResponseWriter, r *http.Request) {
	tenantID := middleware.GetTenantID(r.Context())

	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}

	var req domain.CreateClauseRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.Title == "" || req.Body == "" || req.Category == "" {
		writeError(w, http.StatusBadRequest, "title, body, and category are required")
		return
	}

	if err := h.authz.CheckAllowed(r.Context(), principalID, req.LegalEntityID, actionClauseCreate); err != nil {
		h.writeAuthzErr(w, err)
		return
	}

	c := &domain.Clause{
		TenantID:       tenantID,
		LegalEntityID:  req.LegalEntityID,
		Title:          req.Title,
		Category:       req.Category,
		Body:           req.Body,
		JurisdictionID: req.JurisdictionID,
		EffectiveFrom:  req.EffectiveFrom,
		EffectiveTo:    req.EffectiveTo,
		AuthoredByAI:   req.AuthoredByAI,
		CreatedBy:      req.CreatedBy,
	}

	if err := h.store.CreateClause(r.Context(), c); err != nil {
		h.writeLifecycleErr(w, "failed to create clause", err)
		return
	}

	_ = h.publisher.Publish(r.Context(), events.PublishParams{
		EventType: "clause.created", EntityID: c.ClauseID, TenantID: tenantID,
		LegalEntityID: c.LegalEntityID, Jurisdiction: c.JurisdictionID, ActorID: principalID,
		CorrelationID: r.Header.Get("X-Correlation-ID"), Payload: c,
	})
	writeJSON(w, http.StatusCreated, c)
}

func (h *Handler) GetClause(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	c, err := h.store.GetClause(r.Context(), id)
	if err != nil {
		h.writeLifecycleErr(w, "failed to get clause", err)
		return
	}
	writeJSON(w, http.StatusOK, c)
}

func (h *Handler) ListClauses(w http.ResponseWriter, r *http.Request) {
	legalEntityID := r.URL.Query().Get("legal_entity_id")
	category := r.URL.Query().Get("category")
	clauses, err := h.store.ListClauses(r.Context(), legalEntityID, category)
	if err != nil {
		h.writeLifecycleErr(w, "failed to list clauses", err)
		return
	}
	if clauses == nil {
		clauses = []domain.Clause{}
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"clauses": clauses, "total": len(clauses)})
}

func (h *Handler) UpdateClause(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	tenantID := middleware.GetTenantID(r.Context())

	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}

	existing, err := h.store.GetClause(r.Context(), id)
	if err != nil {
		h.writeLifecycleErr(w, "failed to fetch clause", err)
		return
	}

	if err := h.authz.CheckAllowed(r.Context(), principalID, existing.LegalEntityID, actionClauseUpdate); err != nil {
		h.writeAuthzErr(w, err)
		return
	}

	var req domain.UpdateClauseRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}

	if req.Title != "" {
		existing.Title = req.Title
	}
	if req.Category != "" {
		existing.Category = req.Category
	}
	if req.Body != "" {
		existing.Body = req.Body
	}
	if req.JurisdictionID != "" {
		existing.JurisdictionID = req.JurisdictionID
	}
	if req.EffectiveTo != nil {
		existing.EffectiveTo = req.EffectiveTo
	}

	if err := h.store.UpdateClause(r.Context(), existing, req.ChangeSummary); err != nil {
		h.writeLifecycleErr(w, "failed to update clause", err)
		return
	}

	_ = h.publisher.Publish(r.Context(), events.PublishParams{
		EventType: "clause.updated", EntityID: id, TenantID: tenantID,
		LegalEntityID: existing.LegalEntityID, Jurisdiction: existing.JurisdictionID, ActorID: principalID,
		CorrelationID: r.Header.Get("X-Correlation-ID"), Payload: existing,
	})
	writeJSON(w, http.StatusOK, existing)
}

func (h *Handler) SubmitClauseForLegalReview(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	tenantID := middleware.GetTenantID(r.Context())

	existing, err := h.store.GetClause(r.Context(), id)
	if err != nil {
		h.writeLifecycleErr(w, "failed to fetch clause", err)
		return
	}
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	if err := h.authz.CheckAllowed(r.Context(), principalID, existing.LegalEntityID, actionClauseSubmitReview); err != nil {
		h.writeAuthzErr(w, err)
		return
	}

	c, err := h.store.SubmitClauseForLegalReview(r.Context(), id, principalID)
	if err != nil {
		h.writeLifecycleErr(w, "failed to submit clause for legal review", err)
		return
	}
	_ = h.publisher.Publish(r.Context(), events.PublishParams{
		EventType: "clause.submitted_for_legal_review", EntityID: id, TenantID: tenantID,
		LegalEntityID: c.LegalEntityID, Jurisdiction: c.JurisdictionID, ActorID: principalID,
		CorrelationID: r.Header.Get("X-Correlation-ID"), Payload: c,
	})
	writeJSON(w, http.StatusOK, c)
}

// ApproveClause moves LEGAL_REVIEW -> APPROVED. Maker-checker (LEG-06 §8):
// the principal who submitted the clause for review may not approve it.
func (h *Handler) ApproveClause(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	tenantID := middleware.GetTenantID(r.Context())

	existing, err := h.store.GetClause(r.Context(), id)
	if err != nil {
		h.writeLifecycleErr(w, "failed to fetch clause", err)
		return
	}
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	if err := h.authz.CheckAllowed(r.Context(), principalID, existing.LegalEntityID, actionClauseApprove); err != nil {
		h.writeAuthzErr(w, err)
		return
	}
	if existing.SubmittedBy != nil && *existing.SubmittedBy == principalID {
		writeError(w, http.StatusForbidden, domain.ErrSelfApprovalNotAllowed.Error())
		return
	}

	c, err := h.store.ApproveClause(r.Context(), id, principalID)
	if err != nil {
		h.writeLifecycleErr(w, "failed to approve clause", err)
		return
	}
	_ = h.publisher.Publish(r.Context(), events.PublishParams{
		EventType: "clause.version_approved", EntityID: id, TenantID: tenantID,
		LegalEntityID: c.LegalEntityID, Jurisdiction: c.JurisdictionID, ActorID: principalID,
		CorrelationID: r.Header.Get("X-Correlation-ID"), Payload: c,
	})
	writeJSON(w, http.StatusOK, c)
}

func (h *Handler) ActivateClause(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	tenantID := middleware.GetTenantID(r.Context())

	existing, err := h.store.GetClause(r.Context(), id)
	if err != nil {
		h.writeLifecycleErr(w, "failed to fetch clause", err)
		return
	}
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	if err := h.authz.CheckAllowed(r.Context(), principalID, existing.LegalEntityID, actionClauseActivate); err != nil {
		h.writeAuthzErr(w, err)
		return
	}

	c, err := h.store.ActivateClause(r.Context(), id, principalID)
	if err != nil {
		h.writeLifecycleErr(w, "failed to activate clause", err)
		return
	}
	_ = h.publisher.Publish(r.Context(), events.PublishParams{
		EventType: "clause.activated", EntityID: id, TenantID: tenantID,
		LegalEntityID: c.LegalEntityID, Jurisdiction: c.JurisdictionID, ActorID: principalID,
		CorrelationID: r.Header.Get("X-Correlation-ID"), Payload: c,
	})
	writeJSON(w, http.StatusOK, c)
}

func (h *Handler) RetireClause(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	tenantID := middleware.GetTenantID(r.Context())

	existing, err := h.store.GetClause(r.Context(), id)
	if err != nil {
		h.writeLifecycleErr(w, "failed to fetch clause", err)
		return
	}
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	if err := h.authz.CheckAllowed(r.Context(), principalID, existing.LegalEntityID, actionClauseRetire); err != nil {
		h.writeAuthzErr(w, err)
		return
	}

	c, err := h.store.RetireClause(r.Context(), id, principalID)
	if err != nil {
		h.writeLifecycleErr(w, "failed to retire clause", err)
		return
	}
	_ = h.publisher.Publish(r.Context(), events.PublishParams{
		EventType: "clause.retired", EntityID: id, TenantID: tenantID,
		LegalEntityID: c.LegalEntityID, Jurisdiction: c.JurisdictionID, ActorID: principalID,
		CorrelationID: r.Header.Get("X-Correlation-ID"), Payload: c,
	})
	writeJSON(w, http.StatusOK, c)
}

func (h *Handler) SupersedeClause(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	tenantID := middleware.GetTenantID(r.Context())

	var req domain.SupersedeClauseRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.SupersededBy == "" {
		writeError(w, http.StatusBadRequest, "superseded_by is required")
		return
	}

	existing, err := h.store.GetClause(r.Context(), id)
	if err != nil {
		h.writeLifecycleErr(w, "failed to fetch clause", err)
		return
	}
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	if err := h.authz.CheckAllowed(r.Context(), principalID, existing.LegalEntityID, actionClauseSupersede); err != nil {
		h.writeAuthzErr(w, err)
		return
	}

	c, err := h.store.SupersedeClause(r.Context(), id, req.SupersededBy)
	if err != nil {
		h.writeLifecycleErr(w, "failed to supersede clause", err)
		return
	}
	_ = h.publisher.Publish(r.Context(), events.PublishParams{
		EventType: "clause.superseded", EntityID: id, TenantID: tenantID,
		LegalEntityID: c.LegalEntityID, Jurisdiction: c.JurisdictionID, ActorID: principalID,
		CorrelationID: r.Header.Get("X-Correlation-ID"), Payload: c,
	})
	writeJSON(w, http.StatusOK, c)
}

func (h *Handler) ListClauseVersions(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	versions, err := h.store.ListClauseVersions(r.Context(), id)
	if err != nil {
		h.writeLifecycleErr(w, "failed to list clause versions", err)
		return
	}
	if versions == nil {
		versions = []domain.ClauseVersion{}
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"versions": versions, "total": len(versions)})
}

// --- Deviation Rule Handlers ---

func (h *Handler) CreateDeviationRule(w http.ResponseWriter, r *http.Request) {
	tenantID := middleware.GetTenantID(r.Context())

	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}

	var req domain.CreateDeviationRuleRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.Description == "" || req.JurisdictionID == "" {
		writeError(w, http.StatusBadRequest, "description and jurisdiction_id are required")
		return
	}
	if !req.RiskClassification.IsValid() {
		writeError(w, http.StatusBadRequest, "risk_classification must be one of LOW, MEDIUM, HIGH, CRITICAL")
		return
	}

	if err := h.authz.CheckAllowed(r.Context(), principalID, req.LegalEntityID, actionDeviationCreate); err != nil {
		h.writeAuthzErr(w, err)
		return
	}

	d := &domain.DeviationRule{
		LegalEntityID:      req.LegalEntityID,
		ClauseID:           req.ClauseID,
		JurisdictionID:     req.JurisdictionID,
		RiskClassification: req.RiskClassification,
		Description:        req.Description,
		ProposedBy:         principalID,
	}
	if err := h.store.CreateDeviationRule(r.Context(), d); err != nil {
		h.writeLifecycleErr(w, "failed to create deviation rule", err)
		return
	}

	_ = h.publisher.Publish(r.Context(), events.PublishParams{
		EventType: "deviation_rule.created", EntityID: d.DeviationID, TenantID: tenantID,
		LegalEntityID: d.LegalEntityID, Jurisdiction: d.JurisdictionID, ActorID: principalID,
		CorrelationID: r.Header.Get("X-Correlation-ID"), Payload: d,
	})
	writeJSON(w, http.StatusCreated, d)
}

func (h *Handler) GetDeviationRule(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	d, err := h.store.GetDeviationRule(r.Context(), id)
	if err != nil {
		h.writeLifecycleErr(w, "failed to get deviation rule", err)
		return
	}
	writeJSON(w, http.StatusOK, d)
}

// ListDeviationRules is LEG-06's named GetPlaybook read surface: the set of
// deviation rules for a jurisdiction, optionally narrowed to APPROVED only.
func (h *Handler) ListDeviationRules(w http.ResponseWriter, r *http.Request) {
	legalEntityID := r.URL.Query().Get("legal_entity_id")
	jurisdictionID := r.URL.Query().Get("jurisdiction_id")
	status := r.URL.Query().Get("status")
	rules, err := h.store.ListDeviationRules(r.Context(), legalEntityID, jurisdictionID, status)
	if err != nil {
		h.writeLifecycleErr(w, "failed to list deviation rules", err)
		return
	}
	if rules == nil {
		rules = []domain.DeviationRule{}
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"deviation_rules": rules, "total": len(rules)})
}

// ApproveDeviationRule enforces LEG-06 §8.1's "clause deviations require
// risk/playbook classification and accountable approval": the proposer may
// not approve their own deviation.
func (h *Handler) ApproveDeviationRule(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	tenantID := middleware.GetTenantID(r.Context())

	existing, err := h.store.GetDeviationRule(r.Context(), id)
	if err != nil {
		h.writeLifecycleErr(w, "failed to fetch deviation rule", err)
		return
	}
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	if err := h.authz.CheckAllowed(r.Context(), principalID, existing.LegalEntityID, actionDeviationApprove); err != nil {
		h.writeAuthzErr(w, err)
		return
	}
	if existing.ProposedBy == principalID {
		writeError(w, http.StatusForbidden, domain.ErrSelfApprovalNotAllowed.Error())
		return
	}

	d, err := h.store.ApproveDeviationRule(r.Context(), id, principalID)
	if err != nil {
		h.writeLifecycleErr(w, "failed to approve deviation rule", err)
		return
	}
	_ = h.publisher.Publish(r.Context(), events.PublishParams{
		EventType: "deviation_rule.changed", EntityID: id, TenantID: tenantID,
		LegalEntityID: d.LegalEntityID, Jurisdiction: d.JurisdictionID, ActorID: principalID,
		CorrelationID: r.Header.Get("X-Correlation-ID"), Payload: d,
	})
	writeJSON(w, http.StatusOK, d)
}

// --- Template Handlers ---

func (h *Handler) CreateTemplate(w http.ResponseWriter, r *http.Request) {
	tenantID := middleware.GetTenantID(r.Context())

	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}

	var req domain.CreateTemplateRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.Title == "" || req.ContractType == "" {
		writeError(w, http.StatusBadRequest, "title and contract_type are required")
		return
	}

	if err := h.authz.CheckAllowed(r.Context(), principalID, req.LegalEntityID, actionTemplateCreate); err != nil {
		h.writeAuthzErr(w, err)
		return
	}

	t := &domain.ContractTemplate{
		TenantID:       tenantID,
		LegalEntityID:  req.LegalEntityID,
		Title:          req.Title,
		ContractType:   req.ContractType,
		Description:    req.Description,
		ClauseIDs:      req.ClauseIDs,
		JurisdictionID: req.JurisdictionID,
		EffectiveFrom:  req.EffectiveFrom,
		EffectiveTo:    req.EffectiveTo,
		CreatedBy:      req.CreatedBy,
	}

	if err := h.store.CreateTemplate(r.Context(), t); err != nil {
		h.writeLifecycleErr(w, "failed to create template", err)
		return
	}

	_ = h.publisher.Publish(r.Context(), events.PublishParams{
		EventType: "template.created", EntityID: t.TemplateID, TenantID: tenantID,
		LegalEntityID: t.LegalEntityID, Jurisdiction: t.JurisdictionID, ActorID: principalID,
		CorrelationID: r.Header.Get("X-Correlation-ID"), Payload: t,
	})
	writeJSON(w, http.StatusCreated, t)
}

func (h *Handler) GetTemplate(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	t, err := h.store.GetTemplate(r.Context(), id)
	if err != nil {
		h.writeLifecycleErr(w, "failed to get template", err)
		return
	}
	writeJSON(w, http.StatusOK, t)
}

func (h *Handler) ListTemplates(w http.ResponseWriter, r *http.Request) {
	legalEntityID := r.URL.Query().Get("legal_entity_id")
	contractType := r.URL.Query().Get("contract_type")
	templates, err := h.store.ListTemplates(r.Context(), legalEntityID, contractType)
	if err != nil {
		h.writeLifecycleErr(w, "failed to list templates", err)
		return
	}
	if templates == nil {
		templates = []domain.ContractTemplate{}
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"templates": templates, "total": len(templates)})
}

func (h *Handler) UpdateTemplate(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	tenantID := middleware.GetTenantID(r.Context())

	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}

	existing, err := h.store.GetTemplate(r.Context(), id)
	if err != nil {
		h.writeLifecycleErr(w, "failed to fetch template", err)
		return
	}

	if err := h.authz.CheckAllowed(r.Context(), principalID, existing.LegalEntityID, actionTemplateUpdate); err != nil {
		h.writeAuthzErr(w, err)
		return
	}

	var req domain.UpdateTemplateRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}

	if req.Title != "" {
		existing.Title = req.Title
	}
	if req.ContractType != "" {
		existing.ContractType = req.ContractType
	}
	if req.Description != "" {
		existing.Description = req.Description
	}
	if req.ClauseIDs != nil {
		existing.ClauseIDs = req.ClauseIDs
	}
	if req.JurisdictionID != "" {
		existing.JurisdictionID = req.JurisdictionID
	}
	if req.EffectiveTo != nil {
		existing.EffectiveTo = req.EffectiveTo
	}

	if err := h.store.UpdateTemplate(r.Context(), existing); err != nil {
		h.writeLifecycleErr(w, "failed to update template", err)
		return
	}

	_ = h.publisher.Publish(r.Context(), events.PublishParams{
		EventType: "template.updated", EntityID: id, TenantID: tenantID,
		LegalEntityID: existing.LegalEntityID, Jurisdiction: existing.JurisdictionID, ActorID: principalID,
		CorrelationID: r.Header.Get("X-Correlation-ID"), Payload: existing,
	})
	writeJSON(w, http.StatusOK, existing)
}

// ApproveTemplate moves DRAFT -> ACTIVE. TemplateApproved is a named
// canonical event (LEG-06 §8).
func (h *Handler) ApproveTemplate(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	tenantID := middleware.GetTenantID(r.Context())

	existing, err := h.store.GetTemplate(r.Context(), id)
	if err != nil {
		h.writeLifecycleErr(w, "failed to fetch template", err)
		return
	}
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	if err := h.authz.CheckAllowed(r.Context(), principalID, existing.LegalEntityID, actionTemplateApprove); err != nil {
		h.writeAuthzErr(w, err)
		return
	}

	t, err := h.store.ApproveTemplate(r.Context(), id, principalID)
	if err != nil {
		h.writeLifecycleErr(w, "failed to approve template", err)
		return
	}
	_ = h.publisher.Publish(r.Context(), events.PublishParams{
		EventType: "template.approved", EntityID: id, TenantID: tenantID,
		LegalEntityID: t.LegalEntityID, Jurisdiction: t.JurisdictionID, ActorID: principalID,
		CorrelationID: r.Header.Get("X-Correlation-ID"), Payload: t,
	})
	writeJSON(w, http.StatusOK, t)
}

// --- Helpers ---

func (h *Handler) requirePrincipal(w http.ResponseWriter, r *http.Request) (string, bool) {
	principalID := r.Header.Get("X-Principal-Id")
	if principalID == "" {
		writeError(w, http.StatusUnauthorized, "X-Principal-Id header is required")
		return "", false
	}
	return principalID, true
}

func (h *Handler) writeAuthzErr(w http.ResponseWriter, err error) {
	if errors.Is(err, authz.ErrAuthorizationDenied) {
		writeError(w, http.StatusForbidden, "action not authorized")
		return
	}
	writeError(w, http.StatusServiceUnavailable, "authorization service unavailable")
}

// writeLifecycleErr maps a store failure to the status it deserves, shared
// by every command in this handler.
func (h *Handler) writeLifecycleErr(w http.ResponseWriter, what string, err error) {
	switch {
	case errors.Is(err, domain.ErrTenantMissing):
		writeError(w, http.StatusUnauthorized, "tenant scope missing")
	case errors.Is(err, domain.ErrClauseNotFound):
		writeError(w, http.StatusNotFound, "clause not found")
	case errors.Is(err, domain.ErrTemplateNotFound):
		writeError(w, http.StatusNotFound, "template not found")
	case errors.Is(err, domain.ErrDeviationNotFound):
		writeError(w, http.StatusNotFound, "deviation rule not found")
	case errors.Is(err, domain.ErrWrongStatus):
		writeError(w, http.StatusConflict, domain.ErrWrongStatus.Error())
	case errors.Is(err, domain.ErrSelfApprovalNotAllowed):
		writeError(w, http.StatusForbidden, domain.ErrSelfApprovalNotAllowed.Error())
	default:
		h.logger.Error(what, zap.Error(err))
		writeError(w, http.StatusInternalServerError, what)
	}
}

func writeJSON(w http.ResponseWriter, status int, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}
