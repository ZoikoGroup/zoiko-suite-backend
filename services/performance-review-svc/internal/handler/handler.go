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

	"zoiko.io/performance-review-svc/internal/clients"
	"zoiko.io/performance-review-svc/internal/domain"
	svcmiddleware "zoiko.io/performance-review-svc/internal/middleware"
)

type Store interface {
	CreateCycle(ctx context.Context, c *domain.ReviewCycle) (created bool, err error)
	GetCycle(ctx context.Context, cycleID string) (*domain.ReviewCycle, error)
	ListCycles(ctx context.Context, legalEntityID, status string) ([]domain.ReviewCycle, error)
	CloseCycle(ctx context.Context, cycleID string) (*domain.ReviewCycle, error)

	CreateReview(ctx context.Context, r *domain.ReviewRecord) (created bool, err error)
	GetReview(ctx context.Context, reviewID string) (*domain.ReviewRecord, error)
	ListReviews(ctx context.Context, legalEntityID, cycleID, employeeID, status string) ([]domain.ReviewRecord, error)
	SubmitReview(ctx context.Context, reviewID string, rating int, comments string) (*domain.ReviewRecord, error)
	CompleteReview(ctx context.Context, reviewID string) (*domain.ReviewRecord, error)
}

type Publisher interface {
	PublishReviewCreated(ctx context.Context, r domain.ReviewRecord)
	PublishReviewCompleted(ctx context.Context, actorID string, r domain.ReviewRecord)
}

type AuthZClient interface {
	CheckAllowed(ctx context.Context, principalID, legalEntityID, actionType string) error
}

// EmployeeVerifier confirms a review record's subject employee actually
// exists in employee-master-svc before the record is created.
type EmployeeVerifier interface {
	GetEmployee(ctx context.Context, tenantID, principalID, employeeID string) (*clients.Employee, error)
}

const (
	actionCycleCreate    = "REVIEW_CYCLE_CREATE"
	actionCycleView      = "REVIEW_CYCLE_VIEW"
	actionCycleClose     = "REVIEW_CYCLE_CLOSE"
	actionReviewCreate   = "REVIEW_RECORD_CREATE"
	actionReviewView     = "REVIEW_RECORD_VIEW"
	actionReviewSubmit   = "REVIEW_RECORD_SUBMIT"
	actionReviewComplete = "REVIEW_RECORD_COMPLETE"
)

type Handler struct {
	store     Store
	publisher Publisher
	authz     AuthZClient
	employees EmployeeVerifier
	log       *zap.Logger
}

func New(store Store, publisher Publisher, authz AuthZClient, employees EmployeeVerifier, log *zap.Logger) *Handler {
	return &Handler{store: store, publisher: publisher, authz: authz, employees: employees, log: log}
}

func RegisterRoutes(r chi.Router, h *Handler) {
	r.Route("/v1/review-cycles", func(r chi.Router) {
		r.Post("/", h.CreateCycle)
		r.Get("/", h.ListCycles)
		r.Get("/{cycle_id}", h.GetCycle)
		r.Post("/{cycle_id}/close", h.CloseCycle)
	})
	r.Route("/v1/review-records", func(r chi.Router) {
		r.Post("/", h.CreateReview)
		r.Get("/", h.ListReviews)
		r.Get("/{review_id}", h.GetReview)
		r.Post("/{review_id}/submit", h.SubmitReview)
		r.Post("/{review_id}/complete", h.CompleteReview)
	})
}

// ── POST /v1/review-cycles ────────────────────────────────────────────────────

func (h *Handler) CreateCycle(w http.ResponseWriter, r *http.Request) {
	var req domain.CreateCycleRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_json", err.Error())
		return
	}
	if req.LegalEntityID == "" || req.CycleName == "" || req.PeriodStart == "" || req.PeriodEnd == "" || req.CorrelationID == "" {
		writeError(w, http.StatusBadRequest, "missing_fields", "legal_entity_id, cycle_name, period_start, period_end, correlation_id are required")
		return
	}

	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	if err := h.authz.CheckAllowed(r.Context(), principalID, req.LegalEntityID, actionCycleCreate); err != nil {
		h.writeAuthzErr(w, err)
		return
	}

	now := time.Now().UTC()
	cycle := &domain.ReviewCycle{
		CycleID:              uuid.NewString(),
		TenantID:             svcmiddleware.TenantFromContext(r.Context()),
		LegalEntityID:        req.LegalEntityID,
		CycleName:            req.CycleName,
		PeriodStart:          req.PeriodStart,
		PeriodEnd:            req.PeriodEnd,
		Status:               domain.CycleStatusOpen,
		CreatedByPrincipalID: principalID,
		CorrelationID:        req.CorrelationID,
		CreatedAt:            now,
		UpdatedAt:            now,
	}

	if _, err := h.store.CreateCycle(r.Context(), cycle); err != nil {
		h.log.Error("failed to create review cycle", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store_unavailable", err.Error())
		return
	}

	writeJSON(w, http.StatusCreated, cycle)
}

// ── GET /v1/review-cycles ─────────────────────────────────────────────────────

func (h *Handler) ListCycles(w http.ResponseWriter, r *http.Request) {
	legalEntityID := r.URL.Query().Get("legal_entity_id")
	status := r.URL.Query().Get("status")

	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	if legalEntityID != "" {
		if err := h.authz.CheckAllowed(r.Context(), principalID, legalEntityID, actionCycleView); err != nil {
			h.writeAuthzErr(w, err)
			return
		}
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

// ── GET /v1/review-cycles/{cycle_id} ──────────────────────────────────────────

func (h *Handler) GetCycle(w http.ResponseWriter, r *http.Request) {
	cycleID := chi.URLParam(r, "cycle_id")

	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	cycle, err := h.store.GetCycle(r.Context(), cycleID)
	if err != nil {
		h.writeStoreErr(w, err)
		return
	}
	if err := h.authz.CheckAllowed(r.Context(), principalID, cycle.LegalEntityID, actionCycleView); err != nil {
		h.writeAuthzErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, cycle)
}

// ── POST /v1/review-cycles/{cycle_id}/close ───────────────────────────────────

func (h *Handler) CloseCycle(w http.ResponseWriter, r *http.Request) {
	cycleID := chi.URLParam(r, "cycle_id")

	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	cycle, err := h.store.GetCycle(r.Context(), cycleID)
	if err != nil {
		h.writeStoreErr(w, err)
		return
	}
	if err := h.authz.CheckAllowed(r.Context(), principalID, cycle.LegalEntityID, actionCycleClose); err != nil {
		h.writeAuthzErr(w, err)
		return
	}

	updated, err := h.store.CloseCycle(r.Context(), cycleID)
	if err != nil {
		h.writeStoreErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, updated)
}

// ── POST /v1/review-records ───────────────────────────────────────────────────

// CreateReview requires the target cycle to be OPEN and the target employee
// to be confirmed real via a synchronous call to employee-master-svc — a
// review record is never created against an employee_id that hasn't been
// verified to exist.
//
// Idempotent on (tenant_id, correlation_id).
func (h *Handler) CreateReview(w http.ResponseWriter, r *http.Request) {
	var req domain.CreateReviewRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_json", err.Error())
		return
	}
	if req.LegalEntityID == "" || req.CycleID == "" || req.EmployeeID == "" || req.ReviewerPrincipalID == "" || req.CorrelationID == "" {
		writeError(w, http.StatusBadRequest, "missing_fields", "legal_entity_id, cycle_id, employee_id, reviewer_principal_id, correlation_id are required")
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

	cycle, err := h.store.GetCycle(r.Context(), req.CycleID)
	if err != nil {
		h.writeStoreErr(w, err)
		return
	}
	if cycle.Status != domain.CycleStatusOpen {
		writeError(w, http.StatusConflict, "cycle_not_open", string(domain.ErrCycleNotOpen))
		return
	}

	tenantID := svcmiddleware.TenantFromContext(r.Context())
	emp, err := h.employees.GetEmployee(r.Context(), tenantID, principalID, req.EmployeeID)
	if err != nil {
		h.log.Error("employee-master-svc check failed", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "employee_service_unavailable", err.Error())
		return
	}
	if emp == nil {
		writeError(w, http.StatusBadRequest, "employee_not_found", string(domain.ErrEmployeeNotFound))
		return
	}

	now := time.Now().UTC()
	review := &domain.ReviewRecord{
		ReviewID:             uuid.NewString(),
		TenantID:             tenantID,
		LegalEntityID:        req.LegalEntityID,
		CycleID:              req.CycleID,
		EmployeeID:           req.EmployeeID,
		ReviewerPrincipalID:  req.ReviewerPrincipalID,
		Status:               domain.ReviewStatusDraft,
		CreatedByPrincipalID: principalID,
		CorrelationID:        req.CorrelationID,
		CreatedAt:            now,
		UpdatedAt:            now,
	}

	created, err := h.store.CreateReview(r.Context(), review)
	if err != nil {
		h.log.Error("failed to create review record", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store_unavailable", err.Error())
		return
	}
	if created {
		h.publisher.PublishReviewCreated(r.Context(), *review)
	}

	writeJSON(w, http.StatusCreated, review)
}

// ── GET /v1/review-records ────────────────────────────────────────────────────

func (h *Handler) ListReviews(w http.ResponseWriter, r *http.Request) {
	legalEntityID := r.URL.Query().Get("legal_entity_id")
	cycleID := r.URL.Query().Get("cycle_id")
	employeeID := r.URL.Query().Get("employee_id")
	status := r.URL.Query().Get("status")

	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	if legalEntityID != "" {
		if err := h.authz.CheckAllowed(r.Context(), principalID, legalEntityID, actionReviewView); err != nil {
			h.writeAuthzErr(w, err)
			return
		}
	}

	list, err := h.store.ListReviews(r.Context(), legalEntityID, cycleID, employeeID, status)
	if err != nil {
		h.log.Error("failed to list review records", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store_unavailable", err.Error())
		return
	}
	if list == nil {
		list = []domain.ReviewRecord{}
	}
	writeJSON(w, http.StatusOK, list)
}

// ── GET /v1/review-records/{review_id} ────────────────────────────────────────

func (h *Handler) GetReview(w http.ResponseWriter, r *http.Request) {
	reviewID := chi.URLParam(r, "review_id")

	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	review, err := h.store.GetReview(r.Context(), reviewID)
	if err != nil {
		h.writeStoreErr(w, err)
		return
	}
	if err := h.authz.CheckAllowed(r.Context(), principalID, review.LegalEntityID, actionReviewView); err != nil {
		h.writeAuthzErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, review)
}

// ── POST /v1/review-records/{review_id}/submit ────────────────────────────────

func (h *Handler) SubmitReview(w http.ResponseWriter, r *http.Request) {
	reviewID := chi.URLParam(r, "review_id")

	var req domain.SubmitReviewRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_json", err.Error())
		return
	}
	if req.Rating < 1 || req.Rating > 5 {
		writeError(w, http.StatusBadRequest, "invalid_rating", "rating must be between 1 and 5")
		return
	}
	if req.Comments == "" {
		writeError(w, http.StatusBadRequest, "missing_fields", "comments is required")
		return
	}

	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	review, err := h.store.GetReview(r.Context(), reviewID)
	if err != nil {
		h.writeStoreErr(w, err)
		return
	}
	if err := h.authz.CheckAllowed(r.Context(), principalID, review.LegalEntityID, actionReviewSubmit); err != nil {
		h.writeAuthzErr(w, err)
		return
	}

	updated, err := h.store.SubmitReview(r.Context(), reviewID, req.Rating, req.Comments)
	if err != nil {
		h.writeStoreErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, updated)
}

// ── POST /v1/review-records/{review_id}/complete ──────────────────────────────

func (h *Handler) CompleteReview(w http.ResponseWriter, r *http.Request) {
	reviewID := chi.URLParam(r, "review_id")

	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	review, err := h.store.GetReview(r.Context(), reviewID)
	if err != nil {
		h.writeStoreErr(w, err)
		return
	}
	if err := h.authz.CheckAllowed(r.Context(), principalID, review.LegalEntityID, actionReviewComplete); err != nil {
		h.writeAuthzErr(w, err)
		return
	}

	updated, err := h.store.CompleteReview(r.Context(), reviewID)
	if err != nil {
		h.writeStoreErr(w, err)
		return
	}

	h.publisher.PublishReviewCompleted(r.Context(), principalID, *updated)
	writeJSON(w, http.StatusOK, updated)
}

// ── Helpers ────────────────────────────────────────────────────────────────

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

func (h *Handler) writeStoreErr(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, domain.ErrCycleNotFound), errors.Is(err, domain.ErrReviewNotFound):
		writeError(w, http.StatusNotFound, "not_found", err.Error())
	case errors.Is(err, domain.ErrInvalidTransition):
		writeError(w, http.StatusConflict, "invalid_transition", err.Error())
	default:
		h.log.Error("performance review store error", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store_unavailable", err.Error())
	}
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
