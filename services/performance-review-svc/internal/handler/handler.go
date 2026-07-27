package handler

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"go.uber.org/zap"

	"zoiko.io/performance-review-svc/internal/domain"
	svcmiddleware "zoiko.io/performance-review-svc/internal/middleware"
)

// ── Interface contracts ───────────────────────────────────────────────────────

type Store interface {
	// Cycle
	CreateCycle(ctx context.Context, c *domain.ReviewCycle) error
	GetCycle(ctx context.Context, id string) (*domain.ReviewCycle, error)
	ListCycles(ctx context.Context, legalEntityID, status string) ([]domain.ReviewCycle, error)
	UpdateCycleStatus(ctx context.Context, id, newStatus string) error
	// Review
	CreateReview(ctx context.Context, r *domain.PerformanceReview) error
	GetReview(ctx context.Context, id string) (*domain.PerformanceReview, error)
	ListReviews(ctx context.Context, cycleID, employeeID, status string) ([]domain.PerformanceReview, error)
	TransitionStatus(ctx context.Context, id, fromStatus, toStatus string) error
	SubmitSelfAssessment(ctx context.Context, id string, payload map[string]interface{}) error
	SubmitManagerEvaluation(ctx context.Context, id string, payload map[string]interface{}, rating float64) error
	CompleteReview(ctx context.Context, id, governanceDecisionID string) error
}

type Publisher interface {
	PublishReviewCycleCreated(ctx context.Context, correlationID string, c domain.ReviewCycle)
	PublishReviewCreated(ctx context.Context, correlationID string, r domain.PerformanceReview)
	PublishReviewCompleted(ctx context.Context, correlationID string, r domain.PerformanceReview)
}

type AuthZClient interface {
	CheckAllowed(ctx context.Context, principalID, legalEntityID, actionType string) error
}

// ── Action constants sent to authorization-svc ────────────────────────────────

const (
	actionReviewCycleCreate   = "REVIEW_CYCLE_CREATE"
	actionReviewCycleView     = "REVIEW_CYCLE_VIEW"
	actionReviewCycleUpdate   = "REVIEW_CYCLE_UPDATE"
	actionReviewCreate        = "REVIEW_CREATE"
	actionReviewView          = "REVIEW_VIEW"
	actionReviewSelfAssess    = "REVIEW_SELF_ASSESS"
	actionReviewManagerEval   = "REVIEW_MANAGER_EVALUATE"
	actionReviewApprove       = "REVIEW_APPROVE"
	actionReviewComplete      = "REVIEW_COMPLETE"
)

// ── Handler ───────────────────────────────────────────────────────────────────

type Handler struct {
	store     Store
	publisher Publisher
	authz     AuthZClient
	log       *zap.Logger
}

func New(store Store, publisher Publisher, authz AuthZClient, log *zap.Logger) *Handler {
	return &Handler{
		store:     store,
		publisher: publisher,
		authz:     authz,
		log:       log,
	}
}

func RegisterRoutes(r chi.Router, h *Handler) {
	r.Route("/v1/review-cycles", func(r chi.Router) {
		r.Post("/", h.CreateCycle)
		r.Get("/", h.ListCycles)
		r.Get("/{id}", h.GetCycle)
		r.Put("/{id}/status", h.UpdateCycleStatus)
	})
	r.Route("/v1/reviews", func(r chi.Router) {
		r.Post("/", h.CreateReview)
		r.Get("/", h.ListReviews)
		r.Get("/{id}", h.GetReview)
		r.Post("/{id}/self-assessment", h.SubmitSelfAssessment)
		r.Post("/{id}/manager-evaluation", h.SubmitManagerEvaluation)
		r.Post("/{id}/approve", h.ApproveReview)
		r.Post("/{id}/complete", h.CompleteReview)
	})
}

// ── POST /v1/review-cycles ───────────────────────────────────────────────────

func (h *Handler) CreateCycle(w http.ResponseWriter, r *http.Request) {
	var req domain.CreateCycleRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_json", err.Error())
		return
	}

	if req.LegalEntityID == "" || req.CycleName == "" || req.CycleType == "" || req.StartDate == "" || req.EndDate == "" {
		writeError(w, http.StatusBadRequest, "missing_fields", "legal_entity_id, cycle_name, cycle_type, start_date, end_date are required")
		return
	}

	validCycleTypes := map[string]bool{"ANNUAL": true, "SEMI_ANNUAL": true, "PROBATIONARY": true, "PROJECT_BASED": true}
	if !validCycleTypes[req.CycleType] {
		writeError(w, http.StatusBadRequest, "invalid_cycle_type", "cycle_type must be ANNUAL, SEMI_ANNUAL, PROBATIONARY, or PROJECT_BASED")
		return
	}

	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}

	if err := h.authz.CheckAllowed(r.Context(), principalID, req.LegalEntityID, actionReviewCycleCreate); err != nil {
		h.writeAuthzErr(w, err)
		return
	}

	tenantID := svcmiddleware.TenantFromContext(r.Context())
	now := time.Now().UTC()
	c := &domain.ReviewCycle{
		ReviewCycleID: uuid.NewString(),
		TenantID:      tenantID,
		LegalEntityID: req.LegalEntityID,
		CycleName:     req.CycleName,
		CycleType:     req.CycleType,
		StartDate:     req.StartDate,
		EndDate:       req.EndDate,
		CycleStatus:   "DRAFT",
		EffectiveFrom: now,
		CreatedAt:     now,
		UpdatedAt:     now,
	}

	if err := h.store.CreateCycle(r.Context(), c); err != nil {
		h.log.Error("failed to create review cycle", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store_unavailable", err.Error())
		return
	}

	h.publisher.PublishReviewCycleCreated(r.Context(), getCorrelationID(r), *c)
	writeJSON(w, http.StatusCreated, c)
}

// ── GET /v1/review-cycles ────────────────────────────────────────────────────

func (h *Handler) ListCycles(w http.ResponseWriter, r *http.Request) {
	legalEntityID := r.URL.Query().Get("legal_entity_id")
	status := r.URL.Query().Get("status")

	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	if legalEntityID == "" {
		writeError(w, http.StatusBadRequest, "missing_fields", "legal_entity_id is required")
		return
	}
	if err := h.authz.CheckAllowed(r.Context(), principalID, legalEntityID, actionReviewCycleView); err != nil {
		h.writeAuthzErr(w, err)
		return
	}

	list, err := h.store.ListCycles(r.Context(), legalEntityID, status)
	if err != nil {
		h.log.Error("failed to list review cycles", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store_unavailable", err.Error())
		return
	}
	if list == nil {
		list = []domain.ReviewCycle{}
	}
	writeJSON(w, http.StatusOK, list)
}

// ── GET /v1/review-cycles/{id} ───────────────────────────────────────────────

func (h *Handler) GetCycle(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}

	c, err := h.store.GetCycle(r.Context(), id)
	if errors.Is(err, domain.ErrCycleNotFound) {
		writeError(w, http.StatusNotFound, "cycle_not_found", "")
		return
	}
	if err != nil {
		h.log.Error("failed to get review cycle", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store_unavailable", err.Error())
		return
	}

	if err := h.authz.CheckAllowed(r.Context(), principalID, c.LegalEntityID, actionReviewCycleView); err != nil {
		h.writeAuthzErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, c)
}

// ── PUT /v1/review-cycles/{id}/status ────────────────────────────────────────

func (h *Handler) UpdateCycleStatus(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")

	var req domain.UpdateCycleStatusRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_json", err.Error())
		return
	}

	validStatuses := map[string]bool{"ACTIVE": true, "IN_EVALUATION": true, "COMPLETED": true, "ARCHIVED": true}
	if !validStatuses[req.Status] {
		writeError(w, http.StatusBadRequest, "invalid_status", "status must be ACTIVE, IN_EVALUATION, COMPLETED, or ARCHIVED")
		return
	}

	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}

	c, err := h.store.GetCycle(r.Context(), id)
	if errors.Is(err, domain.ErrCycleNotFound) {
		writeError(w, http.StatusNotFound, "cycle_not_found", "")
		return
	}
	if err != nil {
		h.log.Error("failed to get review cycle for update", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store_unavailable", err.Error())
		return
	}

	if err := h.authz.CheckAllowed(r.Context(), principalID, c.LegalEntityID, actionReviewCycleUpdate); err != nil {
		h.writeAuthzErr(w, err)
		return
	}

	if err := h.store.UpdateCycleStatus(r.Context(), id, req.Status); err != nil {
		h.log.Error("failed to update cycle status", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store_unavailable", err.Error())
		return
	}

	c.CycleStatus = req.Status
	c.UpdatedAt = time.Now().UTC()
	writeJSON(w, http.StatusOK, c)
}

// ── POST /v1/reviews ─────────────────────────────────────────────────────────

func (h *Handler) CreateReview(w http.ResponseWriter, r *http.Request) {
	var req domain.CreateReviewRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_json", err.Error())
		return
	}

	if req.LegalEntityID == "" || req.ReviewCycleID == "" || req.EmployeeID == "" || req.ReviewerPrincipalID == "" {
		writeError(w, http.StatusBadRequest, "missing_fields", "legal_entity_id, review_cycle_id, employee_id, reviewer_principal_id are required")
		return
	}

	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}

	if err := h.authz.CheckAllowed(r.Context(), principalID, req.LegalEntityID, actionReviewCreate); err != nil {
		h.writeAuthzErr(w, err)
		return
	}

	tenantID := svcmiddleware.TenantFromContext(r.Context())
	now := time.Now().UTC()
	rv := &domain.PerformanceReview{
		PerformanceReviewID: uuid.NewString(),
		TenantID:            tenantID,
		LegalEntityID:       req.LegalEntityID,
		ReviewCycleID:       req.ReviewCycleID,
		EmployeeID:          req.EmployeeID,
		ReviewerPrincipalID: req.ReviewerPrincipalID,
		ReviewStatus:        "SELF_ASSESSMENT_PENDING",
		IdempotencyKey:      req.IdempotencyKey,
		EffectiveFrom:       now,
		CreatedAt:           now,
		UpdatedAt:           now,
	}

	if err := h.store.CreateReview(r.Context(), rv); errors.Is(err, domain.ErrDuplicateIdempotencyKey) {
		writeError(w, http.StatusConflict, "duplicate_idempotency_key", err.Error())
		return
	} else if err != nil {
		h.log.Error("failed to create review", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store_unavailable", err.Error())
		return
	}

	h.publisher.PublishReviewCreated(r.Context(), getCorrelationID(r), *rv)
	writeJSON(w, http.StatusCreated, rv)
}

// ── GET /v1/reviews ──────────────────────────────────────────────────────────

func (h *Handler) ListReviews(w http.ResponseWriter, r *http.Request) {
	cycleID := r.URL.Query().Get("review_cycle_id")
	employeeID := r.URL.Query().Get("employee_id")
	status := r.URL.Query().Get("status")
	legalEntityID := r.URL.Query().Get("legal_entity_id")

	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	if legalEntityID == "" {
		writeError(w, http.StatusBadRequest, "missing_fields", "legal_entity_id is required")
		return
	}
	if err := h.authz.CheckAllowed(r.Context(), principalID, legalEntityID, actionReviewView); err != nil {
		h.writeAuthzErr(w, err)
		return
	}

	list, err := h.store.ListReviews(r.Context(), cycleID, employeeID, status)
	if err != nil {
		h.log.Error("failed to list reviews", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store_unavailable", err.Error())
		return
	}
	if list == nil {
		list = []domain.PerformanceReview{}
	}
	writeJSON(w, http.StatusOK, list)
}

// ── GET /v1/reviews/{id} ─────────────────────────────────────────────────────

func (h *Handler) GetReview(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}

	rv, err := h.store.GetReview(r.Context(), id)
	if errors.Is(err, domain.ErrReviewNotFound) {
		writeError(w, http.StatusNotFound, "review_not_found", "")
		return
	}
	if err != nil {
		h.log.Error("failed to get review", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store_unavailable", err.Error())
		return
	}

	if err := h.authz.CheckAllowed(r.Context(), principalID, rv.LegalEntityID, actionReviewView); err != nil {
		h.writeAuthzErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, rv)
}

// ── POST /v1/reviews/{id}/self-assessment ────────────────────────────────────

func (h *Handler) SubmitSelfAssessment(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")

	var req domain.SelfAssessmentRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_json", err.Error())
		return
	}

	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}

	rv, err := h.store.GetReview(r.Context(), id)
	if errors.Is(err, domain.ErrReviewNotFound) {
		writeError(w, http.StatusNotFound, "review_not_found", "")
		return
	}
	if err != nil {
		h.log.Error("failed to get review for self-assessment", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store_unavailable", err.Error())
		return
	}

	if err := h.authz.CheckAllowed(r.Context(), principalID, rv.LegalEntityID, actionReviewSelfAssess); err != nil {
		h.writeAuthzErr(w, err)
		return
	}

	if err := h.store.SubmitSelfAssessment(r.Context(), id, req.Payload); errors.Is(err, domain.ErrInvalidStatusTransition) {
		writeError(w, http.StatusConflict, "invalid_transition", fmt.Sprintf("review must be in SELF_ASSESSMENT_PENDING state; current: %s", rv.ReviewStatus))
		return
	} else if err != nil {
		h.log.Error("failed to submit self assessment", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store_unavailable", err.Error())
		return
	}

	rv.ReviewStatus = "MANAGER_REVIEW_PENDING"
	rv.SelfAssessmentPayload = req.Payload
	rv.UpdatedAt = time.Now().UTC()
	writeJSON(w, http.StatusOK, rv)
}

// ── POST /v1/reviews/{id}/manager-evaluation ─────────────────────────────────

func (h *Handler) SubmitManagerEvaluation(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")

	var req domain.ManagerEvaluationRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_json", err.Error())
		return
	}

	if req.OverallRating < 0 || req.OverallRating > 5 {
		writeError(w, http.StatusBadRequest, "invalid_rating", "overall_rating must be between 0.00 and 5.00")
		return
	}

	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}

	rv, err := h.store.GetReview(r.Context(), id)
	if errors.Is(err, domain.ErrReviewNotFound) {
		writeError(w, http.StatusNotFound, "review_not_found", "")
		return
	}
	if err != nil {
		h.log.Error("failed to get review for manager eval", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store_unavailable", err.Error())
		return
	}

	if err := h.authz.CheckAllowed(r.Context(), principalID, rv.LegalEntityID, actionReviewManagerEval); err != nil {
		h.writeAuthzErr(w, err)
		return
	}

	if err := h.store.SubmitManagerEvaluation(r.Context(), id, req.Payload, req.OverallRating); errors.Is(err, domain.ErrInvalidStatusTransition) {
		writeError(w, http.StatusConflict, "invalid_transition", fmt.Sprintf("review must be in MANAGER_REVIEW_PENDING state; current: %s", rv.ReviewStatus))
		return
	} else if err != nil {
		h.log.Error("failed to submit manager evaluation", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store_unavailable", err.Error())
		return
	}

	rv.ReviewStatus = "SUBMITTED"
	rv.ManagerEvalPayload = req.Payload
	rv.OverallRating = &req.OverallRating
	rv.UpdatedAt = time.Now().UTC()
	writeJSON(w, http.StatusOK, rv)
}

// ── POST /v1/reviews/{id}/approve ────────────────────────────────────────────

func (h *Handler) ApproveReview(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")

	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}

	rv, err := h.store.GetReview(r.Context(), id)
	if errors.Is(err, domain.ErrReviewNotFound) {
		writeError(w, http.StatusNotFound, "review_not_found", "")
		return
	}
	if err != nil {
		h.log.Error("failed to get review for approval", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store_unavailable", err.Error())
		return
	}

	if err := h.authz.CheckAllowed(r.Context(), principalID, rv.LegalEntityID, actionReviewApprove); err != nil {
		h.writeAuthzErr(w, err)
		return
	}

	if err := h.store.TransitionStatus(r.Context(), id, "SUBMITTED", "APPROVED"); errors.Is(err, domain.ErrInvalidStatusTransition) {
		writeError(w, http.StatusConflict, "invalid_transition", fmt.Sprintf("review must be in SUBMITTED state; current: %s", rv.ReviewStatus))
		return
	} else if err != nil {
		h.log.Error("failed to approve review", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store_unavailable", err.Error())
		return
	}

	rv.ReviewStatus = "APPROVED"
	rv.UpdatedAt = time.Now().UTC()
	writeJSON(w, http.StatusOK, rv)
}

// ── POST /v1/reviews/{id}/complete ───────────────────────────────────────────

func (h *Handler) CompleteReview(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")

	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}

	rv, err := h.store.GetReview(r.Context(), id)
	if errors.Is(err, domain.ErrReviewNotFound) {
		writeError(w, http.StatusNotFound, "review_not_found", "")
		return
	}
	if err != nil {
		h.log.Error("failed to get review for completion", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store_unavailable", err.Error())
		return
	}

	if err := h.authz.CheckAllowed(r.Context(), principalID, rv.LegalEntityID, actionReviewComplete); err != nil {
		h.writeAuthzErr(w, err)
		return
	}

	// Governance decision ID — in a full implementation this would be written to
	// governance-decision-log-svc first; here we generate it locally as a UUID
	// to maintain the immutable evidence link on the record.
	governanceDecisionID := uuid.NewString()

	if err := h.store.CompleteReview(r.Context(), id, governanceDecisionID); errors.Is(err, domain.ErrInvalidStatusTransition) {
		writeError(w, http.StatusConflict, "invalid_transition", fmt.Sprintf("review must be in APPROVED state; current: %s", rv.ReviewStatus))
		return
	} else if err != nil {
		h.log.Error("failed to complete review", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store_unavailable", err.Error())
		return
	}

	now := time.Now().UTC()
	rv.ReviewStatus = "COMPLETED"
	rv.GovernanceDecisionID = &governanceDecisionID
	rv.CompletedAt = &now
	rv.UpdatedAt = now

	h.publisher.PublishReviewCompleted(r.Context(), getCorrelationID(r), *rv)
	writeJSON(w, http.StatusOK, rv)
}

// ── Helpers ───────────────────────────────────────────────────────────────────

func (h *Handler) requirePrincipal(w http.ResponseWriter, r *http.Request) (string, bool) {
	principalID := r.Header.Get("X-Principal-Id")
	if principalID == "" {
		writeError(w, http.StatusUnauthorized, "identity_missing", string(domain.ErrIdentityMissing))
		return "", false
	}
	return principalID, true
}

func (h *Handler) writeAuthzErr(w http.ResponseWriter, err error) {
	if errors.Is(err, domain.ErrAuthorizationDenied) {
		writeError(w, http.StatusForbidden, "forbidden", err.Error())
	} else {
		writeError(w, http.StatusServiceUnavailable, "authz_unavailable", err.Error())
	}
}

func getCorrelationID(r *http.Request) string {
	cid := r.Header.Get("X-Correlation-ID")
	if cid == "" {
		return uuid.NewString()
	}
	return cid
}

func writeError(w http.ResponseWriter, status int, code, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{
		"error_code":    code,
		"error_message": msg,
	})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
