package handler

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"go.uber.org/zap"

	"zoiko.io/decision-support-svc/internal/clients"
	"zoiko.io/decision-support-svc/internal/domain"
	svcmiddleware "zoiko.io/decision-support-svc/internal/middleware"
)

type Store interface {
	CreateRecommendation(ctx context.Context, r *domain.Recommendation) (created bool, err error)
	GetRecommendation(ctx context.Context, recommendationID string) (*domain.Recommendation, error)
	ListRecommendations(ctx context.Context, legalEntityID, subjectReference string) ([]domain.Recommendation, error)
}

type Publisher interface {
	PublishRecommended(ctx context.Context, r domain.Recommendation)
}

type AuthZClient interface {
	CheckAllowed(ctx context.Context, principalID, legalEntityID, actionType string) error
}

// HistorySource looks up prior governance decisions to ground a
// recommendation in real precedent.
type HistorySource interface {
	ListPriorDecisions(ctx context.Context, tenantID, legalEntityID, actionType string, limit int) ([]clients.PriorDecision, error)
}

const (
	actionRecommendationRequest = "DECISION_SUPPORT_REQUEST"
	actionRecommendationView    = "DECISION_SUPPORT_VIEW"

	historySampleSize = 30
)

type Handler struct {
	store     Store
	publisher Publisher
	authz     AuthZClient
	history   HistorySource
	log       *zap.Logger
}

func New(store Store, publisher Publisher, authz AuthZClient, history HistorySource, log *zap.Logger) *Handler {
	return &Handler{store: store, publisher: publisher, authz: authz, history: history, log: log}
}

func RegisterRoutes(r chi.Router, h *Handler) {
	r.Route("/v1/recommendations", func(r chi.Router) {
		r.Post("/", h.RequestRecommendation)
		r.Get("/", h.ListRecommendations)
		r.Get("/{recommendation_id}", h.GetRecommendation)
	})
}

// ── POST /v1/recommendations ──────────────────────────────────────────────────

// RequestRecommendation computes a recommendation from real historical
// precedent (governance-decision-log-svc), not an invented heuristic.
//
// Per doctrine §15 ("intelligence services fail without mutating source
// execution... reporting services fail degraded, not destructive"), if
// governance-decision-log-svc is unreachable or has no matching history,
// this returns a NO_HISTORY recommendation rather than a 503 — a
// recommendation is advisory only, so degrading gracefully is correct here
// in a way it would NOT be for a governed write like a spend check.
//
// Idempotent on (tenant_id, correlation_id).
func (h *Handler) RequestRecommendation(w http.ResponseWriter, r *http.Request) {
	var req domain.RequestRecommendationRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_json", err.Error())
		return
	}
	if req.LegalEntityID == "" || req.SubjectType == "" || req.SubjectReference == "" || req.ActionType == "" || req.CorrelationID == "" {
		writeError(w, http.StatusBadRequest, "missing_fields", "legal_entity_id, subject_type, subject_reference, action_type, correlation_id are required")
		return
	}

	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	if err := h.authz.CheckAllowed(r.Context(), principalID, req.LegalEntityID, actionRecommendationRequest); err != nil {
		h.writeAuthzErr(w, err)
		return
	}

	tenantID := svcmiddleware.TenantFromContext(r.Context())
	recommendedAction, confidence, rationale, sampled := h.computeRecommendation(r.Context(), tenantID, req.LegalEntityID, req.ActionType)

	rec := &domain.Recommendation{
		RecommendationID:       uuid.NewString(),
		TenantID:               tenantID,
		LegalEntityID:          req.LegalEntityID,
		SubjectType:            req.SubjectType,
		SubjectReference:       req.SubjectReference,
		ActionType:             req.ActionType,
		RecommendedAction:      recommendedAction,
		ConfidenceScore:        confidence,
		Rationale:              rationale,
		PriorDecisionsSampled:  sampled,
		RequestedByPrincipalID: principalID,
		CorrelationID:          req.CorrelationID,
		CreatedAt:              time.Now().UTC(),
	}

	created, err := h.store.CreateRecommendation(r.Context(), rec)
	if err != nil {
		h.log.Error("failed to record recommendation", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store_unavailable", err.Error())
		return
	}
	if created {
		h.publisher.PublishRecommended(r.Context(), *rec)
	}

	writeJSON(w, http.StatusCreated, rec)
}

// computeRecommendation samples up to historySampleSize prior governance
// decisions for the same legal entity + action type, and recommends
// whichever outcome was most frequent, with confidence = its share of the
// sample. Any failure to reach governance-decision-log-svc degrades to
// NO_HISTORY rather than failing the request.
func (h *Handler) computeRecommendation(ctx context.Context, tenantID, legalEntityID, actionType string) (domain.RecommendedAction, float64, string, int) {
	decisions, err := h.history.ListPriorDecisions(ctx, tenantID, legalEntityID, actionType, historySampleSize)
	if err != nil {
		h.log.Warn("governance-decision-log-svc unreachable, degrading to NO_HISTORY", zap.Error(err))
		return domain.RecommendedActionNoHistory, 0, "governance-decision-log-svc unreachable; no historical basis available", 0
	}
	if len(decisions) == 0 {
		return domain.RecommendedActionNoHistory, 0, "no prior decisions found for this legal entity and action type", 0
	}

	counts := map[string]int{}
	for _, d := range decisions {
		counts[d.Outcome]++
	}

	var topOutcome string
	var topCount int
	for outcome, count := range counts {
		if count > topCount {
			topOutcome, topCount = outcome, count
		}
	}

	total := len(decisions)
	confidence := float64(topCount) / float64(total)

	var recommended domain.RecommendedAction
	switch topOutcome {
	case "GRANTED":
		recommended = domain.RecommendedActionApprove
	case "DENIED":
		recommended = domain.RecommendedActionReject
	default:
		recommended = domain.RecommendedActionEscalate
	}

	rationale := formatRationale(topOutcome, topCount, total)
	return recommended, confidence, rationale, total
}

func formatRationale(topOutcome string, topCount, total int) string {
	return topOutcome + " in " + strconv.Itoa(topCount) + " of the last " + strconv.Itoa(total) + " decisions for this legal entity and action type"
}

// ── GET /v1/recommendations ───────────────────────────────────────────────────

func (h *Handler) ListRecommendations(w http.ResponseWriter, r *http.Request) {
	legalEntityID := r.URL.Query().Get("legal_entity_id")
	subjectReference := r.URL.Query().Get("subject_reference")

	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	if legalEntityID != "" {
		if err := h.authz.CheckAllowed(r.Context(), principalID, legalEntityID, actionRecommendationView); err != nil {
			h.writeAuthzErr(w, err)
			return
		}
	}

	list, err := h.store.ListRecommendations(r.Context(), legalEntityID, subjectReference)
	if err != nil {
		h.log.Error("failed to list recommendations", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store_unavailable", err.Error())
		return
	}
	if list == nil {
		list = []domain.Recommendation{}
	}
	writeJSON(w, http.StatusOK, list)
}

// ── GET /v1/recommendations/{recommendation_id} ───────────────────────────────

func (h *Handler) GetRecommendation(w http.ResponseWriter, r *http.Request) {
	recommendationID := chi.URLParam(r, "recommendation_id")

	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	rec, err := h.store.GetRecommendation(r.Context(), recommendationID)
	if err != nil {
		h.writeStoreErr(w, err)
		return
	}
	if err := h.authz.CheckAllowed(r.Context(), principalID, rec.LegalEntityID, actionRecommendationView); err != nil {
		h.writeAuthzErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, rec)
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
	if errors.Is(err, domain.ErrRecommendationNotFound) {
		writeError(w, http.StatusNotFound, "not_found", err.Error())
		return
	}
	h.log.Error("decision support store error", zap.Error(err))
	writeError(w, http.StatusServiceUnavailable, "store_unavailable", err.Error())
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
