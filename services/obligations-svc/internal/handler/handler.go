package handler

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"go.uber.org/zap"

	"zoiko.io/obligations-svc/internal/domain"
	"zoiko.io/obligations-svc/internal/jurisdiction"
)

// ObligationStore is the narrow interface the handler depends on.
// Allows the handler to be tested without a real database.
type ObligationStore interface {
	CreateObligation(ctx context.Context, params domain.CreateObligationParams) (*domain.Obligation, bool, error)
	FindObligationByID(ctx context.Context, obligationID string) (*domain.Obligation, error)
	ListObligations(ctx context.Context, filter domain.ListObligationsFilter) ([]*domain.Obligation, error)
	UpdateObligationStatus(ctx context.Context, obligationID, newStatus string) (*domain.Obligation, bool, error)
	CreateFilingRequirement(ctx context.Context, params domain.CreateFilingRequirementParams) (*domain.FilingRequirement, error)
	ListFilingRequirements(ctx context.Context, obligationID string) ([]*domain.FilingRequirement, error)
}

// EventPublisher is the narrow interface the handler depends on for
// publishing domain events. Allows the handler to be tested without a real
// event backbone. Mirrors policy-svc's pattern.
type EventPublisher interface {
	PublishObligationCreated(ctx context.Context, o domain.Obligation, correlationID string) error
	PublishObligationUpdated(ctx context.Context, o domain.Obligation, correlationID string) error
	PublishObligationOverdue(ctx context.Context, o domain.Obligation, correlationID string) error
	PublishObligationClosed(ctx context.Context, o domain.Obligation, correlationID string) error
}

// Handler holds all HTTP handler methods.
type Handler struct {
	store                 ObligationStore
	publisher             EventPublisher
	jurisdictionValidator jurisdiction.Validator
	log                   *zap.Logger
}

// New constructs a Handler.
func New(store ObligationStore, publisher EventPublisher, jurisdictionValidator jurisdiction.Validator, log *zap.Logger) *Handler {
	return &Handler{store: store, publisher: publisher, jurisdictionValidator: jurisdictionValidator, log: log}
}

// RegisterRoutes mounts all routes on the given chi router.
// correlationIDMiddleware is applied at the router level so every response
// carries an X-Correlation-ID regardless of path (same convention as
// policy-svc and jurisdiction-rules-svc).
func RegisterRoutes(r chi.Router, h *Handler) {
	r.Use(correlationIDMiddleware)

	r.Post("/v1/obligations", h.CreateObligation)
	r.Get("/v1/obligations", h.ListObligations)
	r.Get("/v1/obligations/{obligation_id}", h.GetObligation)
	r.Post("/v1/obligations/{obligation_id}/status", h.UpdateObligationStatus)
	r.Post("/v1/obligations/{obligation_id}/filing-requirements", h.CreateFilingRequirement)
	r.Get("/v1/obligations/{obligation_id}/filing-requirements", h.ListFilingRequirements)
}

func correlationIDMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if id := r.Header.Get("X-Correlation-ID"); id != "" {
			w.Header().Set("X-Correlation-ID", id)
		}
		next.ServeHTTP(w, r)
	})
}

// ── POST /v1/obligations ─────────────────────────────────────────────────────

// createObligationRequest is the wire shape for POST /v1/obligations.
// ObligationID is optional — callers may supply their own idempotency key.
type createObligationRequest struct {
	ObligationID         string    `json:"obligation_id,omitempty"`
	LegalEntityID        string    `json:"legal_entity_id"`
	JurisdictionID       string    `json:"jurisdiction_id"`
	ObligationSourceType string    `json:"obligation_source_type"`
	ObligationSourceID   string    `json:"obligation_source_id"`
	ObligationCode       string    `json:"obligation_code"`
	ObligationType       string    `json:"obligation_type"`
	DueDate              time.Time `json:"due_date"`
	SeverityLevel        string    `json:"severity_level"`
	ResponsibleFunction  string    `json:"responsible_function"`
	SourceReference      string    `json:"source_reference"`
	CreatedByPrincipalID string    `json:"created_by_principal_id"`
}

func (req createObligationRequest) missingField() string {
	switch {
	case req.LegalEntityID == "":
		return "legal_entity_id"
	case req.JurisdictionID == "":
		return "jurisdiction_id"
	case req.ObligationSourceType == "":
		return "obligation_source_type"
	case req.ObligationSourceID == "":
		return "obligation_source_id"
	case req.ObligationCode == "":
		return "obligation_code"
	case req.ObligationType == "":
		return "obligation_type"
	case req.DueDate.IsZero():
		return "due_date"
	case req.SeverityLevel == "":
		return "severity_level"
	case req.ResponsibleFunction == "":
		return "responsible_function"
	case req.SourceReference == "":
		// Atomic Linking — every obligation must point to its originating
		// source. Enforced here, not just at the DB NOT NULL level, so the
		// caller gets a clear 400 instead of a generic 503/constraint error.
		return "source_reference"
	case req.CreatedByPrincipalID == "":
		return "created_by_principal_id"
	default:
		return ""
	}
}

// CreateObligation handles POST /v1/obligations.
//
// Validates jurisdiction_id against jurisdiction-rules-svc before
// persisting — critical constraint (03-microservices.md §8.5): every
// obligation must be jurisdiction-bound. Fails closed: an unreachable
// jurisdiction-rules-svc rejects the write (503), it does not silently
// accept an unvalidated jurisdiction_id.
//
// Idempotent on obligation_code: a repeat POST with identical attributes
// returns 200 instead of creating a duplicate row. A repeat POST with the
// same code but different attributes returns 409.
//
// Response:
//
//	201 → obligation created for the first time
//	200 → obligation_code already existed with identical attributes; no-op
//	400 → missing required field
//	404 → jurisdiction_id does not exist
//	409 → obligation_code already exists with differing attributes
//	503 → store or jurisdiction-rules-svc unavailable
func (h *Handler) CreateObligation(w http.ResponseWriter, r *http.Request) {
	correlationID := r.Header.Get("X-Correlation-ID")

	var req createObligationRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"error":   "invalid_json",
			"message": err.Error(),
		})
		return
	}
	if missing := req.missingField(); missing != "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"error": "missing_field",
			"field": missing,
		})
		return
	}

	if err := h.jurisdictionValidator.ValidateExists(r.Context(), req.JurisdictionID); err != nil {
		switch {
		case errors.Is(err, domain.ErrJurisdictionNotFound):
			writeJSON(w, http.StatusNotFound, map[string]string{
				"error":           "jurisdiction_not_found",
				"jurisdiction_id": req.JurisdictionID,
			})
		default:
			h.log.Error("CreateObligation: jurisdiction validation failed",
				zap.String("jurisdiction_id", req.JurisdictionID),
				zap.String("correlation_id", correlationID),
				zap.Error(err),
			)
			writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "jurisdiction_service_unavailable"})
		}
		return
	}

	params := domain.CreateObligationParams{
		ObligationID:         req.ObligationID,
		LegalEntityID:        req.LegalEntityID,
		JurisdictionID:       req.JurisdictionID,
		ObligationSourceType: req.ObligationSourceType,
		ObligationSourceID:   req.ObligationSourceID,
		ObligationCode:       req.ObligationCode,
		ObligationType:       req.ObligationType,
		DueDate:              req.DueDate,
		SeverityLevel:        req.SeverityLevel,
		ResponsibleFunction:  req.ResponsibleFunction,
		SourceReference:      req.SourceReference,
		CreatedByPrincipalID: req.CreatedByPrincipalID,
	}

	o, created, err := h.store.CreateObligation(r.Context(), params)
	if err != nil {
		switch {
		case errors.Is(err, domain.ErrConflict):
			writeJSON(w, http.StatusConflict, map[string]string{
				"error":           "obligation_conflict",
				"obligation_code": req.ObligationCode,
			})
		default:
			h.log.Error("CreateObligation: store unavailable",
				zap.String("obligation_code", req.ObligationCode),
				zap.String("correlation_id", correlationID),
				zap.Error(err),
			)
			writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "store_unavailable"})
		}
		return
	}

	status := http.StatusOK
	if created {
		status = http.StatusCreated
		if pubErr := h.publisher.PublishObligationCreated(r.Context(), *o, correlationID); pubErr != nil {
			h.log.Error("CreateObligation: failed to publish obligation.created",
				zap.String("obligation_id", o.ObligationID),
				zap.String("correlation_id", correlationID),
				zap.Error(pubErr),
			)
		}
	}
	h.log.Info("obligation created",
		zap.String("obligation_id", o.ObligationID),
		zap.String("obligation_code", o.ObligationCode),
		zap.Bool("created", created),
		zap.String("correlation_id", correlationID),
	)
	writeJSON(w, status, o)
}

// ── GET /v1/obligations/{obligation_id} ──────────────────────────────────────

// GetObligation handles GET /v1/obligations/{obligation_id}.
//
// Response:
//
//	200 → the Obligation
//	404 → obligation_id not found
//	503 → store unavailable
func (h *Handler) GetObligation(w http.ResponseWriter, r *http.Request) {
	obligationID := chi.URLParam(r, "obligation_id")
	correlationID := r.Header.Get("X-Correlation-ID")

	o, err := h.store.FindObligationByID(r.Context(), obligationID)
	if err != nil {
		switch {
		case errors.Is(err, domain.ErrObligationNotFound):
			writeJSON(w, http.StatusNotFound, map[string]string{
				"error":         "obligation_not_found",
				"obligation_id": obligationID,
			})
		default:
			h.log.Error("GetObligation: store unavailable",
				zap.String("obligation_id", obligationID),
				zap.String("correlation_id", correlationID),
				zap.Error(err),
			)
			writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "store_unavailable"})
		}
		return
	}
	writeJSON(w, http.StatusOK, o)
}

// ── GET /v1/obligations ──────────────────────────────────────────────────────

// ListObligations handles GET /v1/obligations.
//
// Query parameters (all optional, combined with AND):
//
//	legal_entity_id
//	jurisdiction_id
//	obligation_type
//	status
//	due_before   RFC3339 timestamp
//	due_after    RFC3339 timestamp
//
// Response:
//
//	200 → JSON array of Obligation (may be empty)
//	400 → due_before/due_after not valid RFC3339
//	503 → store unavailable
func (h *Handler) ListObligations(w http.ResponseWriter, r *http.Request) {
	correlationID := r.Header.Get("X-Correlation-ID")
	q := r.URL.Query()

	filter := domain.ListObligationsFilter{
		LegalEntityID:  q.Get("legal_entity_id"),
		JurisdictionID: q.Get("jurisdiction_id"),
		ObligationType: q.Get("obligation_type"),
		Status:         q.Get("status"),
	}

	if v := q.Get("due_before"); v != "" {
		t, err := time.Parse(time.RFC3339, v)
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid_field", "field": "due_before"})
			return
		}
		filter.DueBefore = &t
	}
	if v := q.Get("due_after"); v != "" {
		t, err := time.Parse(time.RFC3339, v)
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid_field", "field": "due_after"})
			return
		}
		filter.DueAfter = &t
	}

	results, err := h.store.ListObligations(r.Context(), filter)
	if err != nil {
		h.log.Error("ListObligations: store unavailable",
			zap.String("correlation_id", correlationID),
			zap.Error(err),
		)
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "store_unavailable"})
		return
	}

	// Always return an array — never null.
	if results == nil {
		results = []*domain.Obligation{}
	}
	writeJSON(w, http.StatusOK, results)
}

// ── POST /v1/obligations/{obligation_id}/status ─────────────────────────────

type updateStatusRequest struct {
	ObligationStatus string `json:"obligation_status"`
}

// UpdateObligationStatus handles POST /v1/obligations/{obligation_id}/status.
//
// Transitions the obligation's status per the legal state machine: OPEN can
// move to IN_PROGRESS, OVERDUE, or CLOSED; IN_PROGRESS can move to OVERDUE
// or CLOSED; OVERDUE can move to CLOSED; CLOSED is terminal. Idempotent:
// requesting the status the obligation is already in returns 200 unchanged,
// not an error.
//
// Publishes obligation.updated on every real transition, plus
// obligation.overdue or obligation.closed when the new status is OVERDUE or
// CLOSED respectively.
//
// v1 scope note: there is no built-in scheduler that calls this endpoint
// automatically when due_date passes — an external caller (cron job,
// orchestrator) is expected to detect overdue obligations (e.g. via
// GET /v1/obligations?status=OPEN&due_before=<now>) and drive the
// OPEN/IN_PROGRESS -> OVERDUE transition itself. Building a scheduler is
// out of scope for this service.
//
// Response:
//
//	200 → transitioned (or already in that status — idempotent no-op)
//	400 → missing/invalid obligation_status
//	404 → obligation_id not found
//	409 → illegal transition
//	503 → store unavailable
func (h *Handler) UpdateObligationStatus(w http.ResponseWriter, r *http.Request) {
	obligationID := chi.URLParam(r, "obligation_id")
	correlationID := r.Header.Get("X-Correlation-ID")

	var req updateStatusRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"error":   "invalid_json",
			"message": err.Error(),
		})
		return
	}
	if req.ObligationStatus == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"error": "missing_field",
			"field": "obligation_status",
		})
		return
	}

	updated, transitioned, err := h.store.UpdateObligationStatus(r.Context(), obligationID, req.ObligationStatus)
	if err != nil {
		switch {
		case errors.Is(err, domain.ErrObligationNotFound):
			writeJSON(w, http.StatusNotFound, map[string]string{
				"error":         "obligation_not_found",
				"obligation_id": obligationID,
			})
		case errors.Is(err, domain.ErrInvalidTransition):
			writeJSON(w, http.StatusConflict, map[string]string{
				"error":         "invalid_transition",
				"obligation_id": obligationID,
			})
		default:
			h.log.Error("UpdateObligationStatus: store unavailable",
				zap.String("obligation_id", obligationID),
				zap.String("correlation_id", correlationID),
				zap.Error(err),
			)
			writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "store_unavailable"})
		}
		return
	}

	if transitioned {
		if pubErr := h.publisher.PublishObligationUpdated(r.Context(), *updated, correlationID); pubErr != nil {
			h.log.Error("UpdateObligationStatus: failed to publish obligation.updated",
				zap.String("obligation_id", updated.ObligationID),
				zap.String("correlation_id", correlationID),
				zap.Error(pubErr),
			)
		}
		switch updated.ObligationStatus {
		case "OVERDUE":
			if pubErr := h.publisher.PublishObligationOverdue(r.Context(), *updated, correlationID); pubErr != nil {
				h.log.Error("UpdateObligationStatus: failed to publish obligation.overdue",
					zap.String("obligation_id", updated.ObligationID),
					zap.String("correlation_id", correlationID),
					zap.Error(pubErr),
				)
			}
		case "CLOSED":
			if pubErr := h.publisher.PublishObligationClosed(r.Context(), *updated, correlationID); pubErr != nil {
				h.log.Error("UpdateObligationStatus: failed to publish obligation.closed",
					zap.String("obligation_id", updated.ObligationID),
					zap.String("correlation_id", correlationID),
					zap.Error(pubErr),
				)
			}
		}
	}

	h.log.Info("obligation status updated",
		zap.String("obligation_id", obligationID),
		zap.String("new_status", req.ObligationStatus),
		zap.Bool("transitioned", transitioned),
		zap.String("correlation_id", correlationID),
	)
	writeJSON(w, http.StatusOK, updated)
}

// ── POST /v1/obligations/{obligation_id}/filing-requirements ───────────────

type createFilingRequirementRequest struct {
	FilingRequirementID string `json:"filing_requirement_id,omitempty"`
	FilingType          string `json:"filing_type"`
	FilingAuthority     string `json:"filing_authority"`
	SubmissionChannel   string `json:"submission_channel"`
}

func (req createFilingRequirementRequest) missingField() string {
	switch {
	case req.FilingType == "":
		return "filing_type"
	case req.FilingAuthority == "":
		return "filing_authority"
	case req.SubmissionChannel == "":
		return "submission_channel"
	default:
		return ""
	}
}

// CreateFilingRequirement handles
// POST /v1/obligations/{obligation_id}/filing-requirements.
//
// Response:
//
//	201 → filing requirement created
//	400 → missing required field
//	404 → obligation_id not found
//	503 → store unavailable
func (h *Handler) CreateFilingRequirement(w http.ResponseWriter, r *http.Request) {
	obligationID := chi.URLParam(r, "obligation_id")
	correlationID := r.Header.Get("X-Correlation-ID")

	var req createFilingRequirementRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"error":   "invalid_json",
			"message": err.Error(),
		})
		return
	}
	if missing := req.missingField(); missing != "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"error": "missing_field",
			"field": missing,
		})
		return
	}

	f, err := h.store.CreateFilingRequirement(r.Context(), domain.CreateFilingRequirementParams{
		FilingRequirementID: req.FilingRequirementID,
		ObligationID:        obligationID,
		FilingType:          req.FilingType,
		FilingAuthority:     req.FilingAuthority,
		SubmissionChannel:   req.SubmissionChannel,
	})
	if err != nil {
		switch {
		case errors.Is(err, domain.ErrObligationNotFound):
			writeJSON(w, http.StatusNotFound, map[string]string{
				"error":         "obligation_not_found",
				"obligation_id": obligationID,
			})
		default:
			h.log.Error("CreateFilingRequirement: store unavailable",
				zap.String("obligation_id", obligationID),
				zap.String("correlation_id", correlationID),
				zap.Error(err),
			)
			writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "store_unavailable"})
		}
		return
	}

	h.log.Info("filing requirement created",
		zap.String("obligation_id", obligationID),
		zap.String("filing_requirement_id", f.FilingRequirementID),
		zap.String("correlation_id", correlationID),
	)
	writeJSON(w, http.StatusCreated, f)
}

// ── GET /v1/obligations/{obligation_id}/filing-requirements ────────────────

// ListFilingRequirements handles
// GET /v1/obligations/{obligation_id}/filing-requirements.
//
// Response:
//
//	200 → JSON array of FilingRequirement (may be empty)
//	404 → obligation_id not found
//	503 → store unavailable
func (h *Handler) ListFilingRequirements(w http.ResponseWriter, r *http.Request) {
	obligationID := chi.URLParam(r, "obligation_id")
	correlationID := r.Header.Get("X-Correlation-ID")

	results, err := h.store.ListFilingRequirements(r.Context(), obligationID)
	if err != nil {
		switch {
		case errors.Is(err, domain.ErrObligationNotFound):
			writeJSON(w, http.StatusNotFound, map[string]string{
				"error":         "obligation_not_found",
				"obligation_id": obligationID,
			})
		default:
			h.log.Error("ListFilingRequirements: store unavailable",
				zap.String("obligation_id", obligationID),
				zap.String("correlation_id", correlationID),
				zap.Error(err),
			)
			writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "store_unavailable"})
		}
		return
	}

	if results == nil {
		results = []*domain.FilingRequirement{}
	}
	writeJSON(w, http.StatusOK, results)
}

// writeJSON serialises v as JSON and writes it to w with the given status code.
func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		_ = err
	}
}
