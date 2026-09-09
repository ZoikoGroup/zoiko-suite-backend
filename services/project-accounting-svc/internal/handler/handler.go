// Package handler exposes project-accounting-svc's REST API.
package handler

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"go.uber.org/zap"

	"zoiko.io/project-accounting-svc/internal/domain"
	svcmiddleware "zoiko.io/project-accounting-svc/internal/middleware"
)

// Store is the persistence contract the handler depends on.
type Store interface {
	CreateProject(ctx context.Context, p *domain.Project) error
	GetProject(ctx context.Context, projectID string) (*domain.Project, error)
	ListProjects(ctx context.Context, legalEntityID string) ([]domain.Project, error)
	ApproveProject(ctx context.Context, projectID, principalID string, at time.Time) error
	ActivateProject(ctx context.Context, projectID string, at time.Time) error
	SuspendProject(ctx context.Context, projectID, principalID, reason string, at time.Time) error
	CloseProject(ctx context.Context, projectID, principalID, reason string, at time.Time) error
	ReopenProjectControlled(ctx context.Context, projectID, principalID, reason string, at time.Time) error
	LinkContract(ctx context.Context, projectID, contractRef string) error

	AddWorkPackage(ctx context.Context, w *domain.WorkPackage) error

	AmendFinancialProfile(ctx context.Context, projectID string, newVersion *domain.FinancialProfile) error
	GetCurrentFinancialProfile(ctx context.Context, projectID string) (*domain.FinancialProfile, error)
	GetFinancialProfileAsOf(ctx context.Context, projectID string, at time.Time) (*domain.FinancialProfile, error)

	// PRJ-02 (Project Cost Capture) — see internal/store/cost_entry_store.go's
	// own doc comments for the authority boundary these implement.
	CaptureProjectCost(ctx context.Context, e *domain.CostEntry) error
	GetCostEntry(ctx context.Context, entryID string) (*domain.CostEntry, error)
	ListCostEntries(ctx context.Context, projectID, wbsID string) ([]domain.CostEntry, error)
	ValidateProjectCost(ctx context.Context, entryID string, at time.Time) error
	MarkBillableEligibility(ctx context.Context, entryID string, billable, capitalizable bool) error
	CreateLinkedCostEntry(ctx context.Context, originalEntryID, principalID, reason string, isReversal bool, newEntryID string, amountOverride *float64, costCategory *string, billable, capitalizable *bool, at time.Time) (*domain.CostEntry, error)
	CertifyCostPopulation(ctx context.Context, projectID, principalID string, at time.Time) (*domain.CostCertification, error)
}

// Publisher is the event-publishing contract the handler depends on —
// the spec's own named Events: "ProjectCreated; ProjectApproved;
// ProjectActivated; ProjectFinancialProfileChanged; ProjectClosed;
// ProjectReopened."
type Publisher interface {
	PublishProjectCreated(ctx context.Context, correlationID, actorID string, pr domain.Project)
	PublishProjectApproved(ctx context.Context, correlationID, actorID string, pr domain.Project)
	PublishProjectActivated(ctx context.Context, correlationID, actorID string, pr domain.Project)
	PublishProjectFinancialProfileChanged(ctx context.Context, correlationID, actorID, tenantID, legalEntityID, projectID string)
	PublishProjectClosed(ctx context.Context, correlationID, actorID string, pr domain.Project)
	PublishProjectReopened(ctx context.Context, correlationID, actorID string, pr domain.Project)

	// PRJ-02 (Project Cost Capture) — the spec's own named Events:
	// "ProjectCostCaptured; ProjectCostReclassified; ProjectCostReversed;
	// ProjectCostPopulationCertified."
	PublishProjectCostCaptured(ctx context.Context, correlationID, actorID, tenantID string, e domain.CostEntry)
	PublishProjectCostReclassified(ctx context.Context, correlationID, actorID, tenantID string, e domain.CostEntry)
	PublishProjectCostReversed(ctx context.Context, correlationID, actorID, tenantID string, e domain.CostEntry)
	PublishProjectCostPopulationCertified(ctx context.Context, correlationID, actorID, tenantID, legalEntityID string, cert domain.CostCertification)
}

// AuthZClient is the authorization contract the handler depends on.
type AuthZClient interface {
	CheckAllowed(ctx context.Context, principalID, legalEntityID, actionType string) error
}

// Action types checked against authorization-svc — the spec's own
// Permissions field: "project.read; project.create; project.approve;
// project.financial.manage; project.close/reopen," mapped onto this
// platform's PROJECT_* namespace convention. actionProjectApprove is
// deliberately distinct from actionProjectCreate — the spec's own SoD:
// "Project creator cannot self-approve high-value/regulated project
// profile."
const (
	actionProjectRead            = "PROJECT_READ"
	actionProjectCreate          = "PROJECT_CREATE"
	actionProjectApprove         = "PROJECT_APPROVE"
	actionProjectFinancialManage = "PROJECT_FINANCIAL_MANAGE"
	actionProjectCloseReopen     = "PROJECT_CLOSE_REOPEN"

	// PRJ-02 (Project Cost Capture) actions — the spec's own Permissions
	// field: "project.cost.read; project.cost.capture;
	// project.cost.reclassify; project.cost.adjust; project.cost.certify."
	// actionProjectCostAdjust is deliberately distinct from
	// actionProjectCostCapture — the spec's own SoD: "project cost user
	// cannot modify source AP/payroll/inventory facts," and manual
	// adjustments require independent approval.
	actionProjectCostRead       = "PROJECT_COST_READ"
	actionProjectCostCapture    = "PROJECT_COST_CAPTURE"
	actionProjectCostReclassify = "PROJECT_COST_RECLASSIFY"
	actionProjectCostAdjust     = "PROJECT_COST_ADJUST"
	actionProjectCostCertify    = "PROJECT_COST_CERTIFY"
)

type Handler struct {
	store     Store
	publisher Publisher
	authz     AuthZClient
	log       *zap.Logger
}

func New(store Store, publisher Publisher, authz AuthZClient, log *zap.Logger) *Handler {
	return &Handler{store: store, publisher: publisher, authz: authz, log: log}
}

func RegisterRoutes(r chi.Router, h *Handler) {
	r.Route("/v1/projects", func(r chi.Router) {
		r.Post("/", h.CreateProject)
		r.Get("/", h.ListProjects)
		r.Get("/{id}", h.GetProject)
		r.Get("/{id}/as-of", h.GetProjectAsOf)
		r.Post("/{id}/approve", h.ApproveProject)
		r.Post("/{id}/activate", h.ActivateProject)
		r.Post("/{id}/suspend", h.SuspendProject)
		r.Post("/{id}/close", h.CloseProject)
		r.Post("/{id}/reopen", h.ReopenProjectControlled)
		r.Post("/{id}/link-contract", h.LinkContract)
		r.Post("/{id}/work-packages", h.AddWorkPackage)
		r.Get("/{id}/work-packages", h.GetWBS)
		r.Post("/{id}/financial-profile", h.AmendFinancialProfile)
		r.Get("/{id}/financial-profile", h.GetFinancialProfile)
		r.Get("/{id}/available-actions", h.GetAvailableActions)
		r.Post("/{id}/certify-costs", h.CertifyCostPopulation)
	})
	r.Route("/v1/cost-entries", func(r chi.Router) {
		r.Post("/", h.CaptureProjectCost)
		r.Post("/allocate", h.AllocateSharedCostToProject)
		r.Get("/", h.ListCostEntries)
		r.Get("/{id}", h.GetCostEntry)
		r.Post("/{id}/validate", h.ValidateProjectCost)
		r.Post("/{id}/mark-billable", h.MarkBillableEligibility)
		r.Post("/{id}/reclassify", h.ReclassifyProjectCost)
		r.Post("/{id}/reverse", h.ReverseProjectCost)
	})
}

// ── POST /v1/projects ────────────────────────────────────────────────────────

func (h *Handler) CreateProject(w http.ResponseWriter, r *http.Request) {
	var req domain.CreateProjectRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if req.LegalEntityID == "" || req.ProjectCode == "" || req.Name == "" {
		writeError(w, http.StatusBadRequest, "missing_fields", "legal_entity_id, project_code and name are required")
		return
	}
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	tenantID, ok := h.requireTenant(w, r)
	if !ok {
		return
	}
	if err := h.authz.CheckAllowed(r.Context(), principalID, req.LegalEntityID, actionProjectCreate); err != nil {
		h.writeAuthzErr(w, err)
		return
	}

	p := &domain.Project{
		ProjectID: uuid.NewString(), TenantID: tenantID, LegalEntityID: req.LegalEntityID,
		ProjectCode: req.ProjectCode, Name: req.Name, ProjectType: req.ProjectType,
		CustomerRef: req.CustomerRef, ManagerPrincipalID: req.ManagerPrincipalID, CostCenter: req.CostCenter,
		GroupReference: req.GroupReference, StartDate: req.StartDate, EndDate: req.EndDate,
		Status: domain.ProjectStatusDraft, CreatedAt: time.Now().UTC(), CreatedByPrincipalID: principalID,
	}
	if err := h.store.CreateProject(r.Context(), p); err != nil {
		if errors.Is(err, domain.ErrDuplicateProjectCode) {
			writeError(w, http.StatusUnprocessableEntity, "duplicate_project_code", err.Error())
			return
		}
		h.log.Error("failed to create project", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store_unavailable", err.Error())
		return
	}
	h.publisher.PublishProjectCreated(r.Context(), getCorrelationID(r), principalID, *p)
	writeJSON(w, http.StatusCreated, p)
}

// ── GET /v1/projects/{id}, GET /v1/projects ──────────────────────────────────

func (h *Handler) GetProject(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	if _, ok := h.requireTenant(w, r); !ok {
		return
	}
	p, err := h.store.GetProject(r.Context(), id)
	if err != nil {
		h.writeProjectErr(w, err)
		return
	}
	if err := h.authz.CheckAllowed(r.Context(), principalID, p.LegalEntityID, actionProjectRead); err != nil {
		h.writeAuthzErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, p)
}

func (h *Handler) ListProjects(w http.ResponseWriter, r *http.Request) {
	legalEntityID := r.URL.Query().Get("legal_entity_id")
	if legalEntityID == "" {
		writeError(w, http.StatusBadRequest, "missing_fields", "legal_entity_id is required")
		return
	}
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	if _, ok := h.requireTenant(w, r); !ok {
		return
	}
	if err := h.authz.CheckAllowed(r.Context(), principalID, legalEntityID, actionProjectRead); err != nil {
		h.writeAuthzErr(w, err)
		return
	}
	list, err := h.store.ListProjects(r.Context(), legalEntityID)
	if err != nil {
		h.log.Error("ListProjects: store unavailable", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store_unavailable", err.Error())
		return
	}
	if list == nil {
		list = []domain.Project{}
	}
	writeJSON(w, http.StatusOK, list)
}

// ── GET /v1/projects/{id}/as-of ───────────────────────────────────────────────

// GetProjectAsOf backs the spec's own query of the same name — proof
// that financial-profile changes are versioned, not rewritten.
func (h *Handler) GetProjectAsOf(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	atParam := r.URL.Query().Get("at")
	at := time.Now().UTC()
	if atParam != "" {
		parsed, err := time.Parse(time.RFC3339, atParam)
		if err != nil {
			writeError(w, http.StatusBadRequest, "invalid_at", "at must be RFC3339")
			return
		}
		at = parsed
	}
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	if _, ok := h.requireTenant(w, r); !ok {
		return
	}
	p, err := h.store.GetProject(r.Context(), id)
	if err != nil {
		h.writeProjectErr(w, err)
		return
	}
	if err := h.authz.CheckAllowed(r.Context(), principalID, p.LegalEntityID, actionProjectRead); err != nil {
		h.writeAuthzErr(w, err)
		return
	}
	profile, err := h.store.GetFinancialProfileAsOf(r.Context(), id, at)
	if err != nil {
		h.writeProjectErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"project_id": id, "as_of": at, "financial_profile": profile})
}

// ── POST /v1/projects/{id}/approve ────────────────────────────────────────────

// ApproveProject refuses self-approval — the spec's own SoD: "Project
// creator cannot self-approve high-value/regulated project profile." No
// materiality tiering exists in this platform, so refused universally.
func (h *Handler) ApproveProject(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	if _, ok := h.requireTenant(w, r); !ok {
		return
	}
	p, err := h.store.GetProject(r.Context(), id)
	if err != nil {
		h.writeProjectErr(w, err)
		return
	}
	if p.CreatedByPrincipalID == principalID {
		writeError(w, http.StatusForbidden, "self_approval_not_permitted", domain.ErrSelfApprovalNotPermittedProject.Error())
		return
	}
	if err := h.authz.CheckAllowed(r.Context(), principalID, p.LegalEntityID, actionProjectApprove); err != nil {
		h.writeAuthzErr(w, err)
		return
	}
	now := time.Now().UTC()
	if err := h.store.ApproveProject(r.Context(), id, principalID, now); err != nil {
		h.writeProjectErr(w, err)
		return
	}
	p.Status, p.ApprovedAt, p.ApprovedByPrincipalID = domain.ProjectStatusApproved, &now, &principalID
	h.publisher.PublishProjectApproved(r.Context(), getCorrelationID(r), principalID, *p)
	writeJSON(w, http.StatusOK, p)
}

// ── POST /v1/projects/{id}/activate ───────────────────────────────────────────

// ActivateProject refuses (negative path: "Missing policy/currency/book
// blocks activation") without a financial profile already assigned.
func (h *Handler) ActivateProject(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	if _, ok := h.requireTenant(w, r); !ok {
		return
	}
	p, err := h.store.GetProject(r.Context(), id)
	if err != nil {
		h.writeProjectErr(w, err)
		return
	}
	if err := h.authz.CheckAllowed(r.Context(), principalID, p.LegalEntityID, actionProjectFinancialManage); err != nil {
		h.writeAuthzErr(w, err)
		return
	}
	profile, err := h.store.GetCurrentFinancialProfile(r.Context(), id)
	if err != nil {
		h.writeProjectErr(w, err)
		return
	}
	if profile == nil {
		writeError(w, http.StatusUnprocessableEntity, "financial_profile_required", domain.ErrFinancialProfileRequiredForActivation.Error())
		return
	}
	now := time.Now().UTC()
	if err := h.store.ActivateProject(r.Context(), id, now); err != nil {
		h.writeProjectErr(w, err)
		return
	}
	p.Status, p.ActivatedAt = domain.ProjectStatusActive, &now
	h.publisher.PublishProjectActivated(r.Context(), getCorrelationID(r), principalID, *p)
	writeJSON(w, http.StatusOK, p)
}

// ── POST /v1/projects/{id}/suspend ────────────────────────────────────────────

func (h *Handler) SuspendProject(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	var req domain.SuspendProjectRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if req.Reason == "" {
		writeError(w, http.StatusBadRequest, "missing_field", domain.ErrReasonRequired.Error())
		return
	}
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	if _, ok := h.requireTenant(w, r); !ok {
		return
	}
	p, err := h.store.GetProject(r.Context(), id)
	if err != nil {
		h.writeProjectErr(w, err)
		return
	}
	if err := h.authz.CheckAllowed(r.Context(), principalID, p.LegalEntityID, actionProjectFinancialManage); err != nil {
		h.writeAuthzErr(w, err)
		return
	}
	now := time.Now().UTC()
	if err := h.store.SuspendProject(r.Context(), id, principalID, req.Reason, now); err != nil {
		h.writeProjectErr(w, err)
		return
	}
	p.Status, p.SuspendedAt, p.SuspendedByPrincipalID, p.SuspensionReason = domain.ProjectStatusSuspended, &now, &principalID, &req.Reason
	writeJSON(w, http.StatusOK, p)
}

// ── POST /v1/projects/{id}/close ──────────────────────────────────────────────

func (h *Handler) CloseProject(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	var req domain.CloseProjectRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if req.Reason == "" {
		writeError(w, http.StatusBadRequest, "missing_field", domain.ErrReasonRequired.Error())
		return
	}
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	if _, ok := h.requireTenant(w, r); !ok {
		return
	}
	p, err := h.store.GetProject(r.Context(), id)
	if err != nil {
		h.writeProjectErr(w, err)
		return
	}
	if err := h.authz.CheckAllowed(r.Context(), principalID, p.LegalEntityID, actionProjectCloseReopen); err != nil {
		h.writeAuthzErr(w, err)
		return
	}
	now := time.Now().UTC()
	if err := h.store.CloseProject(r.Context(), id, principalID, req.Reason, now); err != nil {
		h.writeProjectErr(w, err)
		return
	}
	p.Status, p.ClosedAt, p.ClosedByPrincipalID, p.CloseReason = domain.ProjectStatusClosed, &now, &principalID, &req.Reason
	h.publisher.PublishProjectClosed(r.Context(), getCorrelationID(r), principalID, *p)
	writeJSON(w, http.StatusOK, p)
}

// ── POST /v1/projects/{id}/reopen ─────────────────────────────────────────────

// ReopenProjectControlled refuses self-reopen — the spec's own SoD:
// "closed project reopen requires independent approval."
func (h *Handler) ReopenProjectControlled(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	var req domain.ReopenProjectRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if req.Reason == "" {
		writeError(w, http.StatusBadRequest, "missing_field", domain.ErrReasonRequired.Error())
		return
	}
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	if _, ok := h.requireTenant(w, r); !ok {
		return
	}
	p, err := h.store.GetProject(r.Context(), id)
	if err != nil {
		h.writeProjectErr(w, err)
		return
	}
	if err := h.authz.CheckAllowed(r.Context(), principalID, p.LegalEntityID, actionProjectCloseReopen); err != nil {
		h.writeAuthzErr(w, err)
		return
	}
	now := time.Now().UTC()
	if err := h.store.ReopenProjectControlled(r.Context(), id, principalID, req.Reason, now); err != nil {
		if errors.Is(err, domain.ErrSelfReopenNotPermitted) {
			writeError(w, http.StatusForbidden, "self_reopen_not_permitted", err.Error())
			return
		}
		h.writeProjectErr(w, err)
		return
	}
	p.Status, p.ReopenedAt, p.ReopenedByPrincipalID, p.ReopenReason = domain.ProjectStatusActive, &now, &principalID, &req.Reason
	h.publisher.PublishProjectReopened(r.Context(), getCorrelationID(r), principalID, *p)
	writeJSON(w, http.StatusOK, p)
}

// ── POST /v1/projects/{id}/link-contract ──────────────────────────────────────

func (h *Handler) LinkContract(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	var req domain.LinkContractRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if req.ContractRef == "" {
		writeError(w, http.StatusBadRequest, "missing_fields", "contract_ref is required")
		return
	}
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	if _, ok := h.requireTenant(w, r); !ok {
		return
	}
	p, err := h.store.GetProject(r.Context(), id)
	if err != nil {
		h.writeProjectErr(w, err)
		return
	}
	if err := h.authz.CheckAllowed(r.Context(), principalID, p.LegalEntityID, actionProjectFinancialManage); err != nil {
		h.writeAuthzErr(w, err)
		return
	}
	if err := h.store.LinkContract(r.Context(), id, req.ContractRef); err != nil {
		h.writeProjectErr(w, err)
		return
	}
	p.ContractRef = &req.ContractRef
	writeJSON(w, http.StatusOK, p)
}

// ── POST/GET /v1/projects/{id}/work-packages ──────────────────────────────────

func (h *Handler) AddWorkPackage(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	var req domain.AddWorkPackageRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if req.WBSCode == "" {
		writeError(w, http.StatusBadRequest, "missing_fields", "wbs_code is required")
		return
	}
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	if _, ok := h.requireTenant(w, r); !ok {
		return
	}
	p, err := h.store.GetProject(r.Context(), id)
	if err != nil {
		h.writeProjectErr(w, err)
		return
	}
	if err := h.authz.CheckAllowed(r.Context(), principalID, p.LegalEntityID, actionProjectFinancialManage); err != nil {
		h.writeAuthzErr(w, err)
		return
	}
	now := time.Now().UTC()
	wbs := &domain.WorkPackage{
		WBSID: uuid.NewString(), ProjectID: id, WBSCode: req.WBSCode, Description: req.Description,
		EffectiveFrom: now, CreatedAt: now, CreatedByPrincipalID: principalID,
	}
	if req.ParentWBSID != "" {
		wbs.ParentWBSID = &req.ParentWBSID
	}
	if err := h.store.AddWorkPackage(r.Context(), wbs); err != nil {
		if errors.Is(err, domain.ErrDuplicateWBSCode) {
			writeError(w, http.StatusUnprocessableEntity, "duplicate_wbs_code", err.Error())
			return
		}
		h.log.Error("failed to add work package", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store_unavailable", err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, wbs)
}

func (h *Handler) GetWBS(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	if _, ok := h.requireTenant(w, r); !ok {
		return
	}
	p, err := h.store.GetProject(r.Context(), id)
	if err != nil {
		h.writeProjectErr(w, err)
		return
	}
	if err := h.authz.CheckAllowed(r.Context(), principalID, p.LegalEntityID, actionProjectRead); err != nil {
		h.writeAuthzErr(w, err)
		return
	}
	list := p.WorkPackages
	if list == nil {
		list = []domain.WorkPackage{}
	}
	writeJSON(w, http.StatusOK, list)
}

// ── POST/GET /v1/projects/{id}/financial-profile ──────────────────────────────

// AmendFinancialProfile is the ONLY way a project's own recognition/
// billing parameters change — see migration 000001's doc comment on
// negative path #2. Refuses any effective_from that isn't strictly later
// than the moment this command runs.
func (h *Handler) AmendFinancialProfile(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	var req domain.AmendFinancialProfileRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	switch req.RecognitionMethod {
	case domain.RecognitionMethodPercentageOfCompletion, domain.RecognitionMethodCompletedContract, domain.RecognitionMethodTimeAndMaterials:
	default:
		writeError(w, http.StatusBadRequest, "invalid_recognition_method", "recognition_method must be one of PERCENTAGE_OF_COMPLETION, COMPLETED_CONTRACT, TIME_AND_MATERIALS")
		return
	}
	switch req.BillingType {
	case domain.BillingTypeFixedPrice, domain.BillingTypeTimeAndMaterials, domain.BillingTypeCostPlus:
	default:
		writeError(w, http.StatusBadRequest, "invalid_billing_type", "billing_type must be one of FIXED_PRICE, TIME_AND_MATERIALS, COST_PLUS")
		return
	}
	if req.Currency == "" {
		writeError(w, http.StatusBadRequest, "missing_fields", "currency is required")
		return
	}
	now := time.Now().UTC()
	effectiveFrom := now
	if req.EffectiveFrom != nil {
		effectiveFrom = *req.EffectiveFrom
	}
	if !effectiveFrom.After(now) {
		writeError(w, http.StatusUnprocessableEntity, "not_future_effective", domain.ErrFinancialProfileMustBeFutureEffective.Error())
		return
	}
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	tenantID, ok := h.requireTenant(w, r)
	if !ok {
		return
	}
	p, err := h.store.GetProject(r.Context(), id)
	if err != nil {
		h.writeProjectErr(w, err)
		return
	}
	if err := h.authz.CheckAllowed(r.Context(), principalID, p.LegalEntityID, actionProjectFinancialManage); err != nil {
		h.writeAuthzErr(w, err)
		return
	}
	newVersion := &domain.FinancialProfile{
		ProfileVersionID: uuid.NewString(), ProjectID: id, RecognitionMethod: req.RecognitionMethod, BillingType: req.BillingType,
		Currency: req.Currency, EffectiveFrom: effectiveFrom, CreatedAt: now, CreatedByPrincipalID: principalID,
	}
	if err := h.store.AmendFinancialProfile(r.Context(), id, newVersion); err != nil {
		h.log.Error("failed to amend financial profile", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store_unavailable", err.Error())
		return
	}
	h.publisher.PublishProjectFinancialProfileChanged(r.Context(), getCorrelationID(r), principalID, tenantID, p.LegalEntityID, id)
	writeJSON(w, http.StatusCreated, newVersion)
}

func (h *Handler) GetFinancialProfile(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	if _, ok := h.requireTenant(w, r); !ok {
		return
	}
	p, err := h.store.GetProject(r.Context(), id)
	if err != nil {
		h.writeProjectErr(w, err)
		return
	}
	if err := h.authz.CheckAllowed(r.Context(), principalID, p.LegalEntityID, actionProjectRead); err != nil {
		h.writeAuthzErr(w, err)
		return
	}
	profile, err := h.store.GetCurrentFinancialProfile(r.Context(), id)
	if err != nil {
		h.writeProjectErr(w, err)
		return
	}
	if profile == nil {
		writeError(w, http.StatusNotFound, "financial_profile_not_set", "")
		return
	}
	writeJSON(w, http.StatusOK, profile)
}

// ── GET /v1/projects/{id}/available-actions ───────────────────────────────────

func (h *Handler) GetAvailableActions(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	if _, ok := h.requireTenant(w, r); !ok {
		return
	}
	p, err := h.store.GetProject(r.Context(), id)
	if err != nil {
		h.writeProjectErr(w, err)
		return
	}
	if err := h.authz.CheckAllowed(r.Context(), principalID, p.LegalEntityID, actionProjectRead); err != nil {
		h.writeAuthzErr(w, err)
		return
	}
	var available []string
	switch p.Status {
	case domain.ProjectStatusDraft:
		available = []string{"approve"}
	case domain.ProjectStatusApproved:
		available = []string{"activate"}
	case domain.ProjectStatusActive:
		available = []string{"suspend", "close"}
	case domain.ProjectStatusSuspended:
		available = []string{"close"}
	case domain.ProjectStatusClosed:
		available = []string{"reopen"}
	}
	writeJSON(w, http.StatusOK, map[string]any{"current_status": p.Status, "available_actions": available})
}

// ── Helpers ──────────────────────────────────────────────────────────────────

func (h *Handler) requirePrincipal(w http.ResponseWriter, r *http.Request) (string, bool) {
	principalID := r.Header.Get("X-Principal-Id")
	if principalID == "" {
		writeError(w, http.StatusUnauthorized, "identity_missing", domain.ErrIdentityMissing.Error())
		return "", false
	}
	return principalID, true
}

func (h *Handler) requireTenant(w http.ResponseWriter, r *http.Request) (string, bool) {
	tenantID := svcmiddleware.TenantFromContext(r.Context())
	if tenantID == "" {
		writeError(w, http.StatusUnauthorized, "identity_missing", domain.ErrIdentityMissing.Error())
		return "", false
	}
	return tenantID, true
}

func (h *Handler) writeAuthzErr(w http.ResponseWriter, err error) {
	if errors.Is(err, domain.ErrAuthorizationDenied) {
		writeError(w, http.StatusForbidden, "forbidden", err.Error())
		return
	}
	h.log.Error("authorization check failed", zap.Error(err))
	writeError(w, http.StatusServiceUnavailable, "authz_unavailable", err.Error())
}

func (h *Handler) writeProjectErr(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, domain.ErrProjectNotFound):
		writeError(w, http.StatusNotFound, "project_not_found", "")
	case errors.Is(err, domain.ErrInvalidProjectTransition):
		writeError(w, http.StatusUnprocessableEntity, "invalid_transition", err.Error())
	default:
		h.log.Error("project accounting store unavailable", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store_unavailable", err.Error())
	}
}

func getCorrelationID(r *http.Request) string {
	cid := r.Header.Get("X-Correlation-ID")
	if cid == "" {
		return uuid.NewString()
	}
	return cid
}

func decodeJSON(w http.ResponseWriter, r *http.Request, v any) bool {
	if r.Body == nil || r.ContentLength == 0 {
		return true
	}
	if err := json.NewDecoder(r.Body).Decode(v); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_json", err.Error())
		return false
	}
	return true
}

func writeError(w http.ResponseWriter, status int, code, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"error_code": code, "error_message": msg})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
