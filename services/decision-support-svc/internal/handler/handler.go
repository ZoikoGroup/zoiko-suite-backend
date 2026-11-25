// Package handler is decision-support-svc's HTTP layer. Each handler does
// the same four things, in order:
//
//  1. read the caller (tenant + principal) and the request (path, query, body)
//  2. call one service.Service method
//  3. on error, render an RFC 9457 problem (problem.go)
//  4. on success, render the JSON body (and ETag for a single recommendation)
//
// Business rules live in internal/service, never here.
package handler

import (
	"context"
	"encoding/json"
	"net/http"

	"github.com/go-chi/chi/v5"
	"go.uber.org/zap"

	"zoiko.io/decision-support-svc/internal/domain"
)

// RecommendationService is the subset of *service.Service the handlers use.
type RecommendationService interface {
	CreateRecommendation(ctx context.Context, caller domain.CallerContext, idem domain.IdempotencyInput, req domain.CreateRecommendationRequest) (*domain.Recommendation, bool, error)
	GetRecommendation(ctx context.Context, caller domain.CallerContext, recommendationID string) (*domain.Recommendation, error)
	ListRecommendations(ctx context.Context, caller domain.CallerContext, filter domain.ListFilter) (*domain.Page, error)
	ListDispositions(ctx context.Context, caller domain.CallerContext, recommendationID string) ([]domain.Disposition, error)
	Decide(ctx context.Context, caller domain.CallerContext, idem domain.IdempotencyInput, cmd domain.DecisionCommand) (*domain.Recommendation, bool, error)
}

// Handler serves the /v1/recommendations API.
type Handler struct {
	service RecommendationService
	log     *zap.Logger
}

// New builds a Handler.
func New(service RecommendationService, log *zap.Logger) *Handler {
	return &Handler{service: service, log: log}
}

// RegisterRoutes mounts every route. ROUTES.md documents each one.
func RegisterRoutes(r chi.Router, h *Handler) {
	r.Route("/v1/recommendations", func(r chi.Router) {
		r.Post("/", h.CreateRecommendation)
		r.Get("/", h.ListRecommendations)
		r.Get("/{recommendation_id}", h.GetRecommendation)
		r.Get("/{recommendation_id}/dispositions", h.ListDispositions)
		r.Post("/{recommendation_id}/accept", h.AcceptRecommendation)
		r.Post("/{recommendation_id}/reject", h.RejectRecommendation)
		r.Post("/{recommendation_id}/expire", h.ExpireRecommendation)
		r.Post("/{recommendation_id}/supersede", h.SupersedeRecommendation)
	})
}

// CreateRecommendation handles POST /v1/recommendations.
//
// Validation: X-Tenant-Id and X-Principal-Id required (401); Idempotency-Key
// required (400); body must be a JSON object with legal_entity_id (UUID,
// equal to X-Legal-Entity-Id), subject_type, subject_reference and
// action_type (400); body tenant_id, if sent, must equal X-Tenant-Id (403);
// oversight_class O4 is refused (422).
// Writes: one recommendation in REVIEW_REQUIRED, its creation history entry,
// the decision_support.recommendation.proposed outbox event and the
// idempotency record — in one transaction.
// Success: 201 with the recommendation and its ETag. A replay of the same
// Idempotency-Key and body returns the original 201 body with
// Idempotent-Replayed: true; the same key with a different body is 422.
// Errors: 400, 401, 403 (tenant mismatch / authz denied), 422, 503.
func (h *Handler) CreateRecommendation(w http.ResponseWriter, r *http.Request) {
	caller, err := callerFromRequest(r)
	if err != nil {
		h.writeProblem(w, r, err)
		return
	}

	rawBody, err := readBody(r)
	if err != nil {
		h.writeProblem(w, r, err)
		return
	}
	idem, err := idempotencyFromRequest(r, caller.PrincipalID, rawBody)
	if err != nil {
		h.writeProblem(w, r, err)
		return
	}
	var req domain.CreateRecommendationRequest
	if err := decodeJSON(rawBody, &req, false); err != nil {
		h.writeProblem(w, r, err)
		return
	}

	rec, replayed, err := h.service.CreateRecommendation(r.Context(), caller, idem, req)
	if err != nil {
		h.writeProblem(w, r, err)
		return
	}

	writeRecommendation(w, http.StatusCreated, rec, replayed)
}

// ListRecommendations handles GET /v1/recommendations.
//
// Validation: X-Tenant-Id and X-Principal-Id required (401). Optional query:
// legal_entity_id (UUID), subject_type, subject_reference, action_type,
// status (one of the lifecycle states), limit (1-200, default 50) and cursor
// (from a previous page's next_cursor) — any malformed value is 400.
// Authorization: DECISION_SUPPORT_VIEW on legal_entity_id, or on PLATFORM
// scope when the list is not filtered by legal entity.
// Success: 200 {"items": [...], "next_cursor": "..." | null}, newest first.
// Errors: 400, 401, 403, 503.
func (h *Handler) ListRecommendations(w http.ResponseWriter, r *http.Request) {
	caller, err := callerFromRequest(r)
	if err != nil {
		h.writeProblem(w, r, err)
		return
	}

	filter, err := listFilterFromQuery(r)
	if err != nil {
		h.writeProblem(w, r, err)
		return
	}

	page, err := h.service.ListRecommendations(r.Context(), caller, filter)
	if err != nil {
		h.writeProblem(w, r, err)
		return
	}

	writeJSON(w, http.StatusOK, page)
}

// GetRecommendation handles GET /v1/recommendations/{recommendation_id}.
//
// Validation: X-Tenant-Id and X-Principal-Id required (401);
// recommendation_id must be a UUID (400).
// Authorization: DECISION_SUPPORT_VIEW on the recommendation's legal entity.
// Success: 200 with the recommendation and ETag: "<version>" — send it back
// as If-Match on a decision.
// Errors: 400, 401, 403, 404 (unknown id, or another tenant's), 503.
func (h *Handler) GetRecommendation(w http.ResponseWriter, r *http.Request) {
	caller, err := callerFromRequest(r)
	if err != nil {
		h.writeProblem(w, r, err)
		return
	}

	recommendationID, err := recommendationIDFromPath(r)
	if err != nil {
		h.writeProblem(w, r, err)
		return
	}

	rec, err := h.service.GetRecommendation(r.Context(), caller, recommendationID)
	if err != nil {
		h.writeProblem(w, r, err)
		return
	}

	writeRecommendation(w, http.StatusOK, rec, false)
}

// ListDispositions handles GET /v1/recommendations/{recommendation_id}/dispositions.
//
// Validation: X-Tenant-Id and X-Principal-Id required (401);
// recommendation_id must be a UUID (400).
// Authorization: DECISION_SUPPORT_VIEW on the recommendation's legal entity.
// Success: 200 {"items": [...]} — the append-only history, oldest first,
// starting with the creation entry (from_status absent, to_status
// REVIEW_REQUIRED).
// Errors: 400, 401, 403, 404, 503.
func (h *Handler) ListDispositions(w http.ResponseWriter, r *http.Request) {
	caller, err := callerFromRequest(r)
	if err != nil {
		h.writeProblem(w, r, err)
		return
	}

	recommendationID, err := recommendationIDFromPath(r)
	if err != nil {
		h.writeProblem(w, r, err)
		return
	}

	history, err := h.service.ListDispositions(r.Context(), caller, recommendationID)
	if err != nil {
		h.writeProblem(w, r, err)
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{"items": history})
}

// AcceptRecommendation handles POST /v1/recommendations/{recommendation_id}/accept.
//
// Records a human decision to accept the advice. It does NOT execute the
// recommended action: the governed action must still be submitted to its
// owning service, which re-authorizes it (AIG-04 §7.3, NP-38).
// Validation: caller (401), Idempotency-Key (400), recommendation_id UUID
// (400), If-Match required (428) and well-formed (400); optional body
// {reason, downstream_action_ref, approval_reference}; approval_reference
// is required for O3 (400).
// Rules: If-Match must equal the current version (412); status must be
// REVIEW_REQUIRED (409); the requester of an O2/O3 recommendation cannot
// accept it (403 segregation_of_duties); expires_at must not have passed
// (409 review_deadline_passed).
// Writes: status ACCEPTED + decided_by/decided_at, a history entry, the
// decision_support.recommendation.accepted event and a governance decision
// log record (both via the outbox), and the idempotency record.
// Success: 200 with the updated recommendation and its new ETag.
// Errors: 400, 401, 403, 404, 409, 412, 422 (key reused), 428, 503.
func (h *Handler) AcceptRecommendation(w http.ResponseWriter, r *http.Request) {
	h.decide(w, r, domain.StatusAccepted)
}

// RejectRecommendation handles POST /v1/recommendations/{recommendation_id}/reject.
//
// Records a human decision to reject the advice ("REJECTED -> no downstream
// authority"). Validation as for accept, plus body reason is required (400).
// Rules: If-Match (412), REVIEW_REQUIRED only (409), expires_at not passed
// (409). The requester may reject their own recommendation.
// Writes: status REJECTED, history entry, decision_support.recommendation.rejected
// event and governance decision log record (outbox), idempotency record.
// Success: 200 with the updated recommendation.
// Errors: 400, 401, 403, 404, 409, 412, 422, 428, 503.
func (h *Handler) RejectRecommendation(w http.ResponseWriter, r *http.Request) {
	h.decide(w, r, domain.StatusRejected)
}

// ExpireRecommendation handles POST /v1/recommendations/{recommendation_id}/expire.
//
// Closes the review window without a decision (NP-40 "Review deadline
// expires -> Escalate/expire; never auto-accept"). After expires_at no
// reason is needed; before it, body reason is required (400).
// Rules: If-Match (412), REVIEW_REQUIRED only (409).
// Writes: status EXPIRED, history entry, decision_support.recommendation.expired
// event (outbox), idempotency record.
// Success: 200 with the updated recommendation.
// Errors: 400, 401, 403, 404, 409, 412, 422, 428, 503.
func (h *Handler) ExpireRecommendation(w http.ResponseWriter, r *http.Request) {
	h.decide(w, r, domain.StatusExpired)
}

// SupersedeRecommendation handles POST /v1/recommendations/{recommendation_id}/supersede.
//
// Replaces this recommendation with a newer one ("SUPERSEDED -> replacement
// output/review linked"). Body: replacement_recommendation_id (UUID,
// required, 400) and optional reason.
// Rules: If-Match (412); this recommendation must be REVIEW_REQUIRED (409);
// the replacement must exist in the tenant (422 replacement_not_found), be
// a different recommendation for the same legal entity, subject and action
// (422 replacement_mismatched) and itself be REVIEW_REQUIRED (409).
// Writes: status SUPERSEDED + superseded_by link, the replacement's
// supersedes link (its version also increments), history entry,
// decision_support.recommendation.superseded event (outbox), idempotency record.
// Success: 200 with the superseded recommendation.
// Errors: 400, 401, 403, 404, 409, 412, 422, 428, 503.
func (h *Handler) SupersedeRecommendation(w http.ResponseWriter, r *http.Request) {
	h.decide(w, r, domain.StatusSuperseded)
}

// decide is the shared body of the four decision handlers: it differs only
// in the target status.
func (h *Handler) decide(w http.ResponseWriter, r *http.Request, target domain.Status) {
	caller, err := callerFromRequest(r)
	if err != nil {
		h.writeProblem(w, r, err)
		return
	}

	recommendationID, err := recommendationIDFromPath(r)
	if err != nil {
		h.writeProblem(w, r, err)
		return
	}

	rawBody, err := readBody(r)
	if err != nil {
		h.writeProblem(w, r, err)
		return
	}
	idem, err := idempotencyFromRequest(r, caller.PrincipalID, rawBody)
	if err != nil {
		h.writeProblem(w, r, err)
		return
	}
	var req domain.DecisionRequest
	if err := decodeJSON(rawBody, &req, true); err != nil {
		h.writeProblem(w, r, err)
		return
	}

	expectedVersion, err := expectedVersionFromRequest(r)
	if err != nil {
		h.writeProblem(w, r, err)
		return
	}

	rec, replayed, err := h.service.Decide(r.Context(), caller, idem, domain.DecisionCommand{
		RecommendationID: recommendationID,
		TargetStatus:     target,
		ExpectedVersion:  expectedVersion,
		Request:          req,
	})
	if err != nil {
		h.writeProblem(w, r, err)
		return
	}

	writeRecommendation(w, http.StatusOK, rec, replayed)
}

// ── response helpers ─────────────────────────────────────────────────────────

func writeRecommendation(w http.ResponseWriter, status int, rec *domain.Recommendation, replayed bool) {
	w.Header().Set("ETag", rec.ETag())
	if replayed {
		w.Header().Set("Idempotent-Replayed", "true")
	}
	writeJSON(w, status, rec)
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

func (h *Handler) logError(r *http.Request, message string, err error) {
	h.log.Error(message,
		zap.String("method", r.Method),
		zap.String("path", r.URL.Path),
		zap.String("correlation_id", r.Header.Get("X-Correlation-ID")),
		zap.Error(err),
	)
}
