package handler

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"

	"github.com/go-chi/chi/v5"
	"go.uber.org/zap"

	"zoiko.io/workflow-svc/internal/authz"
	"zoiko.io/workflow-svc/internal/documentvault"
	"zoiko.io/workflow-svc/internal/domain"
	"zoiko.io/workflow-svc/internal/evidence"
	svcmiddleware "zoiko.io/workflow-svc/internal/middleware"
)

// WorkflowStore is the narrow interface the handler depends on.
type WorkflowStore interface {
	CreateWorkflow(ctx context.Context, params domain.CreateWorkflowParams) (instance *domain.WorkflowInstance, stages []*domain.WorkflowStage, created bool, err error)
	FindWorkflowByID(ctx context.Context, workflowInstanceID string) (*domain.WorkflowInstance, error)
	FindStagesByWorkflowID(ctx context.Context, workflowInstanceID string) ([]*domain.WorkflowStage, error)
	FindCurrentStage(ctx context.Context, workflowInstanceID string) (*domain.WorkflowStage, error)
	SubmitAction(ctx context.Context, params domain.SubmitActionParams) (*domain.WorkflowInstance, *domain.WorkflowStage, bool, error)
	EscalateWorkflow(ctx context.Context, workflowInstanceID, actorPrincipalID string) (*domain.WorkflowInstance, bool, error)
	CancelWorkflow(ctx context.Context, workflowInstanceID, actorPrincipalID string) (*domain.WorkflowInstance, bool, error)
	CreateAuditEngagement(ctx context.Context, params domain.CreateAuditEngagementParams) (*domain.AuditEngagement, bool, error)
	GetAuditEngagement(ctx context.Context, tenantID, engagementID string) (*domain.AuditEngagement, error)
	SubmitAuditEngagementAcceptance(ctx context.Context, params domain.SubmitAuditEngagementAcceptanceParams) (*domain.AuditEngagement, bool, error)
	TransitionAuditEngagement(ctx context.Context, params domain.TransitionAuditEngagementParams) (*domain.AuditEngagement, bool, error)
	AmendAuditEngagementScope(ctx context.Context, params domain.AmendAuditEngagementScopeParams) (*domain.AuditEngagement, bool, error)
	GetAuditEngagementCompletionGates(ctx context.Context, tenantID, engagementID, stage string) ([]domain.CompletionGate, error)

	// AUD-02 Planning & Risk Assessment — see internal/store/audit_plan_store.go's
	// own doc comments for the enforcement mechanisms.
	CreateAuditPlan(ctx context.Context, params domain.CreateAuditPlanParams) (*domain.AuditPlan, bool, error)
	GetAuditPlan(ctx context.Context, tenantID, planID string) (*domain.AuditPlan, error)
	GetAuditPlanByEngagement(ctx context.Context, tenantID, engagementID string) (*domain.AuditPlan, error)
	RecordMateriality(ctx context.Context, params domain.RecordMaterialityParams) (*domain.MaterialityRecord, error)
	IdentifyRisk(ctx context.Context, params domain.IdentifyRiskParams) (*domain.RiskAssessment, bool, error)
	GetRiskAssessment(ctx context.Context, tenantID, riskID string) (*domain.RiskAssessment, error)
	ListRisksByPlan(ctx context.Context, tenantID, planID string) ([]*domain.RiskAssessment, error)
	LinkAssertion(ctx context.Context, params domain.LinkAssertionParams) error
	DesignAuditResponse(ctx context.Context, params domain.DesignAuditResponseParams) error
	AssessRisk(ctx context.Context, params domain.AssessRiskParams) (*domain.RiskAssessment, bool, error)
	MarkSignificantRisk(ctx context.Context, params domain.MarkSignificantRiskParams) (*domain.RiskAssessment, error)
	ApprovePlan(ctx context.Context, params domain.ApprovePlanParams) (*domain.AuditPlan, bool, error)
	GetAuditEngagementFieldworkGates(ctx context.Context, tenantID, engagementID string) (planApproved bool, noUnresolvedHighRisk bool, err error)

	// AUD-07 Workpaper — see internal/store/workpaper_store.go's own doc
	// comments for the enforcement mechanisms.
	CreateWorkpaper(ctx context.Context, params domain.CreateWorkpaperParams) (*domain.Workpaper, bool, error)
	GetWorkpaper(ctx context.Context, tenantID, workpaperID string) (*domain.Workpaper, error)
	ListWorkpapersByEngagement(ctx context.Context, tenantID, engagementID string) ([]*domain.Workpaper, error)
	RecordProcedure(ctx context.Context, params domain.RecordProcedureParams) error
	RecordResult(ctx context.Context, params domain.RecordResultParams) error
	RecordConclusion(ctx context.Context, params domain.RecordConclusionParams) error
	AddWorkpaperCrossReference(ctx context.Context, params domain.AddWorkpaperCrossReferenceParams) error
	LinkWorkpaperEvidence(ctx context.Context, params domain.LinkWorkpaperEvidenceParams) error
	MarkWorkpaperPrepared(ctx context.Context, params domain.MarkWorkpaperPreparedParams) (*domain.Workpaper, bool, error)
	LockWorkpaper(ctx context.Context, params domain.LockWorkpaperParams) (*domain.Workpaper, bool, error)
	AddPostLockAddendum(ctx context.Context, params domain.AddPostLockAddendumParams) (*domain.WorkpaperAddendum, error)
	GetAuditEngagementRequiredWorkpapersLocked(ctx context.Context, tenantID, engagementID string) (bool, error)

	// AUD-09 Review & Sign-Off — see internal/store/audit_review_store.go's
	// own doc comments for the enforcement mechanisms.
	OpenReview(ctx context.Context, params domain.OpenReviewParams) (*domain.ReviewScope, bool, error)
	GetReviewScope(ctx context.Context, tenantID, reviewScopeID string) (*domain.ReviewScope, error)
	AssignReviewer(ctx context.Context, params domain.AssignReviewerParams) (*domain.ReviewAssignment, error)
	RaiseReviewNote(ctx context.Context, params domain.RaiseReviewNoteParams) (*domain.ReviewNote, error)
	RespondToReviewNote(ctx context.Context, params domain.RespondToReviewNoteParams) (*domain.ReviewNote, error)
	ResolveReviewNote(ctx context.Context, params domain.ResolveReviewNoteParams) (*domain.ReviewNote, error)
	SignOff(ctx context.Context, params domain.SignOffParams) (*domain.SignOff, error)
	WithdrawSignOff(ctx context.Context, params domain.WithdrawSignOffParams) (*domain.SignOff, error)
	StartQualityReview(ctx context.Context, params domain.StartQualityReviewParams) (*domain.QualityReviewRecord, bool, error)
	CompleteQualityReview(ctx context.Context, params domain.CompleteQualityReviewParams) (*domain.QualityReviewRecord, bool, error)
	GetAuditEngagementReportGates(ctx context.Context, tenantID, engagementID string) (allSignOffsValid bool, noUnresolvedMandatoryNotes bool, err error)

	// BIZ-04 Form Definition & Submission — see
	// internal/store/form_store.go's own doc comments for the enforcement
	// mechanisms.
	CreateForm(ctx context.Context, params domain.CreateFormParams) (*domain.FormDefinition, bool, error)
	GetForm(ctx context.Context, tenantID, formID string) (*domain.FormDefinition, error)
	PublishForm(ctx context.Context, params domain.PublishFormParams) (*domain.FormDefinition, error)
	RetireForm(ctx context.Context, params domain.RetireFormParams) (*domain.FormDefinition, error)
	SaveDraft(ctx context.Context, params domain.SaveDraftParams) (*domain.FormSubmission, error)
	SubmitForm(ctx context.Context, params domain.SubmitFormParams) (*domain.FormSubmission, error)
	ValidateSubmission(ctx context.Context, params domain.ValidateSubmissionParams) (*domain.FormSubmission, error)
	GetSubmission(ctx context.Context, tenantID, submissionID string) (*domain.FormSubmission, error)
}

// EventPublisher is the narrow interface the handler depends on. actorID on
// every method except PublishWorkflowStarted (which already has
// w.InitiatedBy) is the already-verified req.ActorPrincipalID from that
// request — the real principal who took the action, not necessarily the
// same as stage.ApproverPrincipalID (the stage's assigned approver, which
// CheckApprovalAllowed already confirmed matches, but the envelope's actor
// should name who the request says acted, per Doc 03 §19).
type EventPublisher interface {
	PublishWorkflowStarted(ctx context.Context, w domain.WorkflowInstance) error
	PublishApprovalGranted(ctx context.Context, w domain.WorkflowInstance, stage domain.WorkflowStage, actorID string) error
	PublishApprovalRejected(ctx context.Context, w domain.WorkflowInstance, stage domain.WorkflowStage, actorID string) error
	PublishWorkflowEscalated(ctx context.Context, w domain.WorkflowInstance, actorID string) error
	PublishWorkflowCompleted(ctx context.Context, w domain.WorkflowInstance, actorID string) error
	PublishAuditEngagementEvent(ctx context.Context, eventType string, engagement domain.AuditEngagement, actorID, correlationID string) error

	// BIZ-04 Form — see internal/events/publisher.go's own doc comments.
	PublishFormPublished(ctx context.Context, f domain.FormDefinition, actorID, correlationID string) error
	PublishFormEvent(ctx context.Context, eventType string, s domain.FormSubmission, actorID, correlationID string) error
}

// DocumentVaultClient verifies evidence references without making workflow-svc
// an evidence owner. The returned version is the immutable vault version that
// is pinned to the acceptance record.
type DocumentVaultClient interface {
	VerifyDocument(ctx context.Context, tenantID, legalEntityID, documentID, actorID, correlationID string) (int, error)
}

// EvidenceClient is AUD-07's own dependency on AUD-06 — see
// internal/evidence/client.go's own doc comment. Optional: a nil client
// means LinkEvidence never surfaces a contradiction flag (evidence-check
// disabled), used by tests that don't exercise AUD-06 integration.
type EvidenceClient interface {
	GetContradictionFlag(ctx context.Context, tenantID, evidenceID, actorID, correlationID string) (bool, error)
}

type Handler struct {
	store     WorkflowStore
	publisher EventPublisher
	authz     authz.Client
	documents DocumentVaultClient
	evidence  EvidenceClient
	log       *zap.Logger
}

func New(store WorkflowStore, publisher EventPublisher, authzClient authz.Client, documents documentvault.Client, log *zap.Logger) *Handler {
	return &Handler{store: store, publisher: publisher, authz: authzClient, documents: documents, log: log}
}

// WithEvidenceClient attaches the optional AUD-06 evidence client — an
// optional-dependency builder method, same pattern as
// WithLedgerClient/WithPeriodChecker elsewhere in this build, so New(...)'s
// required parameter list stays unchanged.
func (h *Handler) WithEvidenceClient(client evidence.Client) *Handler {
	h.evidence = client
	return h
}

func RegisterRoutes(r chi.Router, h *Handler) {
	r.Use(correlationIDMiddleware)

	r.Post("/v1/workflows", h.CreateWorkflow)
	r.Get("/v1/workflows/{workflow_instance_id}", h.GetWorkflow)
	r.Get("/v1/workflows/{workflow_instance_id}/next-approver", h.GetNextApprover)
	r.Post("/v1/workflows/{workflow_instance_id}/actions", h.SubmitAction)
	r.Post("/v1/workflows/{workflow_instance_id}/escalate", h.EscalateWorkflow)
	r.Post("/v1/workflows/{workflow_instance_id}/cancel", h.CancelWorkflow)
	r.Route("/v1/audit/engagements", func(r chi.Router) {
		r.Post("/", h.CreateAuditEngagement)
		r.Get("/{engagement_id}", h.GetAuditEngagement)
		r.Post("/{engagement_id}/submit-acceptance", h.SubmitAuditEngagementAcceptance)
		r.Post("/{engagement_id}/acceptance-decision", h.RecordAuditAcceptanceDecision)
		r.Post("/{engagement_id}/activate", h.ActivateAuditEngagement)
		r.Post("/{engagement_id}/withdraw", h.WithdrawAuditEngagement)
		r.Post("/{engagement_id}/amend-scope", h.AmendAuditEngagementScope)
		r.Get("/{engagement_id}/completion-gates", h.GetCompletionGates)
		r.Post("/{engagement_id}/mark-fieldwork-complete", h.MarkFieldworkComplete)
		r.Post("/{engagement_id}/enter-completion-review", h.EnterCompletionReview)
		r.Post("/{engagement_id}/mark-report-ready", h.MarkReportReady)
		r.Post("/{engagement_id}/close", h.CloseEngagement)
		r.Post("/{engagement_id}/plan", h.CreateAuditPlan)
		r.Get("/{engagement_id}/plan", h.GetAuditPlan)
	})
	r.Route("/v1/audit/plans", func(r chi.Router) {
		r.Post("/{plan_id}/materiality", h.RecordMateriality)
		r.Post("/{plan_id}/risks", h.IdentifyRisk)
		r.Get("/{plan_id}/risks", h.ListRisks)
		r.Post("/{plan_id}/approve", h.ApprovePlan)
	})
	r.Route("/v1/audit/risks", func(r chi.Router) {
		r.Post("/{risk_id}/assertions", h.LinkAssertion)
		r.Post("/{risk_id}/response", h.DesignAuditResponse)
		r.Post("/{risk_id}/assess", h.AssessRisk)
		r.Post("/{risk_id}/mark-significant", h.MarkSignificantRisk)
	})
	r.Route("/v1/audit/engagements/{engagement_id}/workpapers", func(r chi.Router) {
		r.Post("/", h.CreateWorkpaper)
	})
	r.Route("/v1/audit/workpapers", func(r chi.Router) {
		r.Get("/{workpaper_id}", h.GetWorkpaper)
		r.Post("/{workpaper_id}/procedures", h.RecordProcedure)
		r.Post("/{workpaper_id}/results", h.RecordResult)
		r.Post("/{workpaper_id}/conclusions", h.RecordConclusion)
		r.Post("/{workpaper_id}/cross-references", h.AddCrossReference)
		r.Post("/{workpaper_id}/evidence-links", h.LinkEvidence)
		r.Post("/{workpaper_id}/mark-prepared", h.MarkWorkpaperPrepared)
		r.Post("/{workpaper_id}/lock", h.LockWorkpaper)
		r.Post("/{workpaper_id}/addenda", h.AddPostLockAddendum)
	})
	r.Route("/v1/audit/engagements/{engagement_id}/reviews", func(r chi.Router) {
		r.Post("/", h.OpenReview)
		r.Post("/quality", h.StartQualityReview)
		r.Get("/release-gates", h.EvaluateReleaseGates)
	})
	r.Route("/v1/audit/review-scopes", func(r chi.Router) {
		r.Post("/{review_scope_id}/reviewers", h.AssignReviewer)
		r.Post("/{review_scope_id}/notes", h.RaiseReviewNote)
		r.Post("/{review_scope_id}/sign-off", h.SignOff)
	})
	r.Route("/v1/audit/review-notes", func(r chi.Router) {
		r.Post("/{review_note_id}/respond", h.RespondToReviewNote)
		r.Post("/{review_note_id}/resolve", h.ResolveReviewNote)
	})
	r.Post("/v1/audit/sign-offs/{sign_off_id}/withdraw", h.WithdrawSignOff)
	r.Post("/v1/audit/quality-reviews/{quality_review_id}/complete", h.CompleteQualityReview)

	r.Route("/v1/forms", func(r chi.Router) {
		r.Post("/", h.CreateForm)
		r.Get("/{form_id}", h.GetForm)
		r.Post("/{form_id}/publish", h.PublishForm)
		r.Post("/{form_id}/retire", h.RetireForm)
		r.Post("/{form_id}/drafts", h.SaveDraft)
	})
	r.Route("/v1/form-submissions", func(r chi.Router) {
		r.Get("/{submission_id}", h.GetSubmission)
		r.Post("/{submission_id}/drafts", h.SaveDraft)
		r.Post("/{submission_id}/submit", h.SubmitForm)
		r.Post("/{submission_id}/validate", h.ValidateSubmission)
	})
}

// requireTenant reads the caller's verified tenant scope, set into
// context by svcmiddleware.TenantContext from X-Tenant-Id, rejecting the
// request if absent.
//
// Before this fix, every method routing through FindWorkflowByID (which
// is all of them) fell back to an UNSCOPED lookup when the header was
// simply omitted — the exact "filter that disables itself when absent"
// shape already found in document-vault-svc. Omitting the header, the
// easier request to make, was strictly more permissive than supplying a
// real one.
func (h *Handler) requireTenant(w http.ResponseWriter, r *http.Request) (string, bool) {
	tenantID := svcmiddleware.TenantFromContext(r.Context())
	if tenantID == "" {
		writeJSON(w, http.StatusUnauthorized, map[string]string{
			"error":   "missing_tenant_scope",
			"message": "X-Tenant-Id is required — the gateway sets it from a verified identity envelope",
		})
		return "", false
	}
	return tenantID, true
}

// requirePrincipal reads the caller's verified principal from the
// X-Principal-Id header the gateway sets from a verified identity
// envelope, rejecting the request if absent.
//
// Before this fix, initiated_by / actor_principal_id came straight from
// the request body on every route — any caller could submit an approval
// or rejection as any principal_id, and if that principal happened to
// hold the right role, authorization-svc's CheckApprovalAllowed would
// have no way to tell the claim was forged.
func (h *Handler) requirePrincipal(w http.ResponseWriter, r *http.Request) (string, bool) {
	principalID := r.Header.Get("X-Principal-Id")
	if principalID == "" {
		writeJSON(w, http.StatusUnauthorized, map[string]string{
			"error":   "missing_principal",
			"message": "X-Principal-Id is required — the gateway sets it from a verified identity envelope",
		})
		return "", false
	}
	return principalID, true
}

// refuseForeignTenant reports whether claimed names a tenant other than
// verifiedTenant, writing a 403 if so.
func (h *Handler) refuseForeignTenant(w http.ResponseWriter, claimed, verifiedTenant string) bool {
	if claimed != "" && claimed != verifiedTenant {
		writeJSON(w, http.StatusForbidden, map[string]string{
			"error":   "tenant_scope_mismatch",
			"message": "request tenant_id does not match the caller's verified tenant scope",
		})
		return true
	}
	return false
}

func correlationIDMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if id := r.Header.Get("X-Correlation-ID"); id != "" {
			w.Header().Set("X-Correlation-ID", id)
		}
		next.ServeHTTP(w, r)
	})
}

// ── POST /v1/workflows ───────────────────────────────────────────────────────

type createWorkflowRequest struct {
	TenantID      string                            `json:"tenant_id"`
	LegalEntityID string                            `json:"legal_entity_id"`
	WorkflowType  string                            `json:"workflow_type"`
	Stages        []domain.CreateWorkflowStageInput `json:"stages"`
}

func (req createWorkflowRequest) missingField() string {
	switch {
	case req.TenantID == "":
		return "tenant_id"
	case req.LegalEntityID == "":
		return "legal_entity_id"
	case req.WorkflowType == "":
		return "workflow_type"
	default:
		return ""
	}
}

type workflowResponse struct {
	*domain.WorkflowInstance
	Stages []*domain.WorkflowStage `json:"stages"`
}

// CreateWorkflow handles POST /v1/workflows.
//
// Response: 201 created / 200 idempotent replay (same X-Correlation-ID as
// an existing instance) / 400 missing field or no stages / 503 unavailable.
func (h *Handler) CreateWorkflow(w http.ResponseWriter, r *http.Request) {
	correlationID := r.Header.Get("X-Correlation-ID")

	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	tenantScope, ok := h.requireTenant(w, r)
	if !ok {
		return
	}

	var req createWorkflowRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid_json", "message": err.Error()})
		return
	}
	if missing := req.missingField(); missing != "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "missing_field", "field": missing})
		return
	}
	if h.refuseForeignTenant(w, req.TenantID, tenantScope) {
		return
	}
	if len(req.Stages) == 0 {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "no_stages"})
		return
	}
	for _, st := range req.Stages {
		if st.ApproverPrincipalID == "" {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "missing_field", "field": "stages[].approver_principal_id"})
			return
		}
	}
	// Segregation of Duties (docs/original_doc/zoiko_suite_doc1.txt §12.3):
	// the initiator of a workflow may not be listed as an approver in any
	// of its own stages. This is a validation error on the caller-supplied
	// workflow definition, not an authz decision.
	for _, st := range req.Stages {
		if st.ApproverPrincipalID == principalID {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "initiator_cannot_be_approver", "field": "stages[].approver_principal_id"})
			return
		}
	}

	instance, stages, created, err := h.store.CreateWorkflow(r.Context(), domain.CreateWorkflowParams{
		// initiated_by is always the verified caller, never the request
		// body — see requirePrincipal's doc comment.
		TenantID: req.TenantID, LegalEntityID: req.LegalEntityID, WorkflowType: req.WorkflowType,
		InitiatedBy: principalID, CorrelationID: correlationID, Stages: req.Stages,
	})
	if err != nil {
		if errors.Is(err, domain.ErrNoStages) {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "no_stages"})
			return
		}
		h.log.Error("CreateWorkflow: store unavailable", zap.String("correlation_id", correlationID), zap.Error(err))
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "store_unavailable"})
		return
	}

	// A retried call (created=false) resolves to the workflow the ORIGINAL
	// call already started and already published workflow.started for —
	// publishing it again here would be a duplicate business event for a
	// workflow that was never actually re-initiated.
	status := http.StatusOK
	if created {
		status = http.StatusCreated
		if pubErr := h.publisher.PublishWorkflowStarted(r.Context(), *instance); pubErr != nil {
			h.log.Error("CreateWorkflow: failed to publish workflow.started", zap.String("correlation_id", correlationID), zap.Error(pubErr))
		}
		h.log.Info("workflow started",
			zap.String("workflow_instance_id", instance.WorkflowInstanceID),
			zap.String("workflow_type", instance.WorkflowType),
			zap.Int("stage_count", len(stages)),
			zap.String("correlation_id", correlationID),
		)
	}
	writeJSON(w, status, workflowResponse{WorkflowInstance: instance, Stages: stages})
}

// ── GET /v1/workflows/{id} ───────────────────────────────────────────────────

// GetWorkflow handles GET /v1/workflows/{workflow_instance_id}.
//
// Response: 200 instance + stages / 404 not found / 503 unavailable.
func (h *Handler) GetWorkflow(w http.ResponseWriter, r *http.Request) {
	workflowInstanceID := chi.URLParam(r, "workflow_instance_id")
	correlationID := r.Header.Get("X-Correlation-ID")

	if _, ok := h.requireTenant(w, r); !ok {
		return
	}

	instance, err := h.store.FindWorkflowByID(r.Context(), workflowInstanceID)
	if err != nil {
		writeStoreErr(w, h.log, err, correlationID, "GetWorkflow")
		return
	}
	stages, err := h.store.FindStagesByWorkflowID(r.Context(), workflowInstanceID)
	if err != nil {
		writeStoreErr(w, h.log, err, correlationID, "GetWorkflow")
		return
	}
	writeJSON(w, http.StatusOK, workflowResponse{WorkflowInstance: instance, Stages: stages})
}

// ── GET /v1/workflows/{id}/next-approver ─────────────────────────────────────

// GetNextApprover handles GET /v1/workflows/{workflow_instance_id}/next-approver
// — the "resolve next approver" capability.
//
// Response: 200 the current stage / 404 not found or workflow already terminal / 503 unavailable.
func (h *Handler) GetNextApprover(w http.ResponseWriter, r *http.Request) {
	workflowInstanceID := chi.URLParam(r, "workflow_instance_id")
	correlationID := r.Header.Get("X-Correlation-ID")

	if _, ok := h.requireTenant(w, r); !ok {
		return
	}

	stage, err := h.store.FindCurrentStage(r.Context(), workflowInstanceID)
	if err != nil {
		writeStoreErr(w, h.log, err, correlationID, "GetNextApprover")
		return
	}
	writeJSON(w, http.StatusOK, stage)
}

// ── POST /v1/workflows/{id}/actions ──────────────────────────────────────────

type submitActionRequest struct {
	Action    string  `json:"action"`
	Rationale *string `json:"rationale,omitempty"`
	// CausationID is optional: the event/decision that caused this specific
	// action, when the caller knows it.
	CausationID *string `json:"causation_id,omitempty"`
}

// SubmitAction handles POST /v1/workflows/{workflow_instance_id}/actions.
//
// Confirms via authorization-svc that the actor is authorized to submit an
// approval action before touching the workflow — "approval workflows
// extend authorization, they do not replace it." Fails closed if
// authorization-svc is unreachable or denies.
//
// Idempotent: resubmitting the identical action on an already-resolved
// stage is a no-op (200, no re-publish) — doctrine requirement ("duplicate
// approval submission must not create double-state transition").
//
// Response:
//
//	200 → action applied (or idempotent no-op)
//	400 → missing/invalid field
//	403 → not authorized to approve (from authorization-svc), or wrong approver for this stage
//	404 → workflow not found
//	409 → workflow not PENDING, or stage already resolved to the opposite outcome
//	503 → store or authorization-svc unavailable
func (h *Handler) SubmitAction(w http.ResponseWriter, r *http.Request) {
	workflowInstanceID := chi.URLParam(r, "workflow_instance_id")
	correlationID := r.Header.Get("X-Correlation-ID")

	// actor_principal_id used to come straight from the request body: any
	// caller could submit an approval as any principal, and if that
	// principal happened to hold the right role, CheckApprovalAllowed
	// below had no way to tell the claim was forged. It is now always the
	// verified caller.
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	if _, ok := h.requireTenant(w, r); !ok {
		return
	}

	var req submitActionRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid_json", "message": err.Error()})
		return
	}
	if req.Action != "APPROVE" && req.Action != "REJECT" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid_field", "field": "action", "message": "must be APPROVE or REJECT"})
		return
	}

	instanceForAuthzCheck, err := h.store.FindWorkflowByID(r.Context(), workflowInstanceID)
	if err != nil {
		writeStoreErr(w, h.log, err, correlationID, "SubmitAction")
		return
	}

	// Segregation of Duties (docs/original_doc/zoiko_suite_doc1.txt §12.3),
	// defense-in-depth: the principal who initiated the workflow may never
	// submit an approve/reject action on it, regardless of whether they are
	// (incorrectly) recorded as an assigned approver for the current stage.
	// This covers instances created before CreateWorkflow's validation
	// existed, or via any path that bypasses it.
	if principalID == instanceForAuthzCheck.InitiatedBy {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "self_approval_not_allowed", "message": domain.ErrSelfApprovalNotAllowed.Error()})
		return
	}

	if err := h.authz.CheckApprovalAllowed(r.Context(), principalID, instanceForAuthzCheck.LegalEntityID); err != nil {
		switch {
		case errors.Is(err, domain.ErrAuthorizationDenied):
			writeJSON(w, http.StatusForbidden, map[string]string{"error": "authorization_denied"})
		default:
			h.log.Error("SubmitAction: authorization-svc unavailable — failing closed",
				zap.String("correlation_id", correlationID), zap.Error(err))
			writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "authorization_service_unavailable"})
		}
		return
	}

	instance, stage, transitioned, err := h.store.SubmitAction(r.Context(), domain.SubmitActionParams{
		WorkflowInstanceID: workflowInstanceID, ActorPrincipalID: principalID, Action: req.Action,
		Rationale: req.Rationale, CausationID: req.CausationID,
	})
	if err != nil {
		switch {
		case errors.Is(err, domain.ErrWorkflowNotFound):
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "workflow_not_found"})
		case errors.Is(err, domain.ErrWrongApprover):
			writeJSON(w, http.StatusForbidden, map[string]string{"error": "wrong_approver"})
		case errors.Is(err, domain.ErrInvalidTransition):
			writeJSON(w, http.StatusConflict, map[string]string{"error": "invalid_transition"})
		default:
			h.log.Error("SubmitAction: store unavailable", zap.String("correlation_id", correlationID), zap.Error(err))
			writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "store_unavailable"})
		}
		return
	}

	if transitioned {
		if req.Action == "APPROVE" {
			if pubErr := h.publisher.PublishApprovalGranted(r.Context(), *instance, *stage, principalID); pubErr != nil {
				h.log.Error("SubmitAction: failed to publish approval.granted", zap.String("correlation_id", correlationID), zap.Error(pubErr))
			}
		} else {
			if pubErr := h.publisher.PublishApprovalRejected(r.Context(), *instance, *stage, principalID); pubErr != nil {
				h.log.Error("SubmitAction: failed to publish approval.rejected", zap.String("correlation_id", correlationID), zap.Error(pubErr))
			}
		}
		if instance.WorkflowStatus == "APPROVED" || instance.WorkflowStatus == "REJECTED" {
			if pubErr := h.publisher.PublishWorkflowCompleted(r.Context(), *instance, principalID); pubErr != nil {
				h.log.Error("SubmitAction: failed to publish workflow.completed", zap.String("correlation_id", correlationID), zap.Error(pubErr))
			}
		}
	}

	h.log.Info("workflow action submitted",
		zap.String("workflow_instance_id", workflowInstanceID),
		zap.String("action", req.Action),
		zap.Bool("transitioned", transitioned),
		zap.String("workflow_status", instance.WorkflowStatus),
		zap.String("correlation_id", correlationID),
	)
	writeJSON(w, http.StatusOK, workflowResponse{WorkflowInstance: instance, Stages: []*domain.WorkflowStage{stage}})
}

// ── POST /v1/workflows/{id}/escalate ─────────────────────────────────────────

// EscalateWorkflow handles POST /v1/workflows/{workflow_instance_id}/escalate.
//
// Response: 200 escalated (or idempotent no-op) / 401 no verified principal or tenant / 404 not found / 409 illegal transition / 503 unavailable.
func (h *Handler) EscalateWorkflow(w http.ResponseWriter, r *http.Request) {
	workflowInstanceID := chi.URLParam(r, "workflow_instance_id")
	correlationID := r.Header.Get("X-Correlation-ID")

	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	if _, ok := h.requireTenant(w, r); !ok {
		return
	}

	instance, transitioned, err := h.store.EscalateWorkflow(r.Context(), workflowInstanceID, principalID)
	if err != nil {
		writeStoreErr(w, h.log, err, correlationID, "EscalateWorkflow")
		return
	}
	if transitioned {
		if pubErr := h.publisher.PublishWorkflowEscalated(r.Context(), *instance, principalID); pubErr != nil {
			h.log.Error("EscalateWorkflow: failed to publish workflow.escalated", zap.String("correlation_id", correlationID), zap.Error(pubErr))
		}
	}
	writeJSON(w, http.StatusOK, instance)
}

// ── POST /v1/workflows/{id}/cancel ───────────────────────────────────────────

// CancelWorkflow handles POST /v1/workflows/{workflow_instance_id}/cancel.
//
// Response: 200 cancelled (or idempotent no-op) / 401 no verified principal or tenant / 404 not found / 409 illegal transition (already terminal) / 503 unavailable.
func (h *Handler) CancelWorkflow(w http.ResponseWriter, r *http.Request) {
	workflowInstanceID := chi.URLParam(r, "workflow_instance_id")
	correlationID := r.Header.Get("X-Correlation-ID")

	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	if _, ok := h.requireTenant(w, r); !ok {
		return
	}

	instance, transitioned, err := h.store.CancelWorkflow(r.Context(), workflowInstanceID, principalID)
	if err != nil {
		writeStoreErr(w, h.log, err, correlationID, "CancelWorkflow")
		return
	}
	if transitioned {
		if pubErr := h.publisher.PublishWorkflowCompleted(r.Context(), *instance, principalID); pubErr != nil {
			h.log.Error("CancelWorkflow: failed to publish workflow.completed", zap.String("correlation_id", correlationID), zap.Error(pubErr))
		}
	}
	writeJSON(w, http.StatusOK, instance)
}

// ── helpers ──────────────────────────────────────────────────────────────────

func writeStoreErr(w http.ResponseWriter, log *zap.Logger, err error, correlationID, op string) {
	switch {
	case errors.Is(err, domain.ErrWorkflowNotFound):
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "workflow_not_found"})
	case errors.Is(err, domain.ErrInvalidTransition):
		writeJSON(w, http.StatusConflict, map[string]string{"error": "invalid_transition"})
	default:
		log.Error(op+": store unavailable", zap.String("correlation_id", correlationID), zap.Error(err))
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "store_unavailable"})
	}
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		_ = err
	}
}
