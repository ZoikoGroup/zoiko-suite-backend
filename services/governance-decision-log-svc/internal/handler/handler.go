package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"go.uber.org/zap"

	"zoiko.io/governance-decision-log-svc/internal/domain"
)

// DecisionStore is the narrow interface the handler depends on.
// Allows the handler to be tested without a real database.
type DecisionStore interface {
	Insert(ctx context.Context, d domain.GovernanceDecision) (created bool, err error)
}

// Handler holds all HTTP handler methods.
type Handler struct {
	store DecisionStore
	log   *zap.Logger
}

// New constructs a Handler.
func New(store DecisionStore, log *zap.Logger) *Handler {
	return &Handler{store: store, log: log}
}

// RegisterRoutes mounts all routes on the given chi router.
// correlationIDMiddleware is applied at the router level so every response
// carries an X-Correlation-ID regardless of path — this makes the
// behaviour testable in unit tests that build their own router via this
// function (same convention as jurisdiction-rules-svc).
func RegisterRoutes(r chi.Router, h *Handler) {
	r.Use(correlationIDMiddleware)
	r.Post("/v1/decisions", h.CreateDecision)
}

func correlationIDMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if id := r.Header.Get("X-Correlation-ID"); id != "" {
			w.Header().Set("X-Correlation-ID", id)
		}
		next.ServeHTTP(w, r)
	})
}

// createDecisionRequest is the wire shape for POST /v1/decisions.
// DecidedAt is optional — if omitted, it defaults to server-receipt time
// (see CONTEXT.md: DecidedAt represents when the decision happened
// upstream, not when it was logged here, but callers may not always have
// a distinct timestamp to send).
type createDecisionRequest struct {
	DecisionID        string          `json:"decision_id"`
	TenantID          string          `json:"tenant_id"`
	LegalEntityID     string          `json:"legal_entity_id"`
	ActorID           string          `json:"actor_id"`
	ActionType        string          `json:"action_type"`
	Outcome           string          `json:"outcome"`
	RuleBasis         string          `json:"rule_basis"`
	EvaluationContext json.RawMessage `json:"evaluation_context,omitempty"`
	CorrelationID     string          `json:"correlation_id"`
	DecidedAt         *time.Time      `json:"decided_at,omitempty"`
}

// requiredFields lists the fields that must be non-empty. evaluation_context
// and decided_at are the only optional fields.
func (req createDecisionRequest) missingField() string {
	switch {
	case req.DecisionID == "":
		return "decision_id"
	case req.TenantID == "":
		return "tenant_id"
	case req.LegalEntityID == "":
		return "legal_entity_id"
	case req.ActorID == "":
		return "actor_id"
	case req.ActionType == "":
		return "action_type"
	case req.Outcome == "":
		return "outcome"
	case req.RuleBasis == "":
		return "rule_basis"
	case req.CorrelationID == "":
		return "correlation_id"
	default:
		return ""
	}
}

// CreateDecision handles POST /v1/decisions.
//
// Idempotent on decision_id: a repeat POST with the same decision_id
// returns 200 (already recorded) instead of creating a duplicate row.
// A first-time POST returns 201.
//
// Response:
//
//	201 → decision recorded for the first time
//	200 → decision_id already existed; no-op, not an error
//	400 → missing required field
//	503 → store unavailable
func (h *Handler) CreateDecision(w http.ResponseWriter, r *http.Request) {
	correlationID := r.Header.Get("X-Correlation-ID")

	var req createDecisionRequest
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

	decidedAt := time.Now().UTC()
	if req.DecidedAt != nil {
		decidedAt = req.DecidedAt.UTC()
	}

	d := domain.GovernanceDecision{
		DecisionID:        req.DecisionID,
		TenantID:          req.TenantID,
		LegalEntityID:     req.LegalEntityID,
		ActorID:           req.ActorID,
		ActionType:        req.ActionType,
		Outcome:           req.Outcome,
		RuleBasis:         req.RuleBasis,
		EvaluationContext: req.EvaluationContext,
		CorrelationID:     req.CorrelationID,
		DecidedAt:         decidedAt,
	}

	created, err := h.store.Insert(r.Context(), d)
	if err != nil {
		h.log.Error("CreateDecision: store unavailable",
			zap.String("decision_id", d.DecisionID),
			zap.String("correlation_id", correlationID),
			zap.Error(err),
		)
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "store_unavailable"})
		return
	}

	status := http.StatusOK
	if created {
		status = http.StatusCreated
	}
	h.log.Info("governance decision recorded",
		zap.String("decision_id", d.DecisionID),
		zap.String("tenant_id", d.TenantID),
		zap.String("outcome", d.Outcome),
		zap.Bool("created", created),
		zap.String("correlation_id", correlationID),
	)
	writeJSON(w, status, d)
}

// writeJSON serialises v as JSON and writes it to w with the given status code.
func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		_ = err
	}
}
