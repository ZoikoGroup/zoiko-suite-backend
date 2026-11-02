// Package handler is the HTTP layer: it parses requests, builds the caller
// context from the envelope headers and delegates every route to exactly one
// method of internal/service. No business rule lives here.
package handler

import (
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"go.uber.org/zap"
	"zoiko.io/compliance-risk-scoring-svc/internal/domain"
	svcenvelope "zoiko.io/compliance-risk-scoring-svc/internal/envelope"
	"zoiko.io/compliance-risk-scoring-svc/internal/health"
	customMiddleware "zoiko.io/compliance-risk-scoring-svc/internal/middleware"
	"zoiko.io/compliance-risk-scoring-svc/internal/problem"
	"zoiko.io/compliance-risk-scoring-svc/internal/service"
	"zoiko.io/compliance-risk-scoring-svc/internal/store"
)

// Handler serves every route of the service.
type Handler struct {
	service *service.Service
	ready   health.Pinger
	logger  *zap.Logger
}

// NewHandler builds a Handler. ready decides /readyz (normally the store).
func NewHandler(svc *service.Service, ready health.Pinger, logger *zap.Logger) *Handler {
	return &Handler{service: svc, ready: ready, logger: logger}
}

// NewRouter wires middleware and routes.
//
// Middleware order (house convention): RequestID, RealIP, Recoverer, tenant
// gate, canonical input envelope. /healthz and /readyz are exempted by both
// the tenant gate and the envelope policy, so probes need no headers.
func NewRouter(h *Handler) http.Handler {
	r := chi.NewRouter()

	r.Use(middleware.RequestID)
	r.Use(middleware.RealIP)
	r.Use(middleware.Recoverer)
	r.Use(customMiddleware.TenantMiddleware)
	r.Use(svcenvelope.Middleware(svcenvelope.ServicePolicy(), svcenvelope.DefaultReporter()))

	r.Get("/healthz", health.Handler())
	r.Get("/readyz", health.ReadyHandler(h.ready))

	r.NotFound(func(w http.ResponseWriter, r *http.Request) {
		problem.Write(w, r, http.StatusNotFound, "route_not_found", "no route matches "+r.Method+" "+r.URL.Path, nil)
	})
	r.MethodNotAllowed(func(w http.ResponseWriter, r *http.Request) {
		problem.Write(w, r, http.StatusMethodNotAllowed, "method_not_allowed", r.Method+" is not allowed on "+r.URL.Path, nil)
	})

	r.Route("/v1/risk-scores", func(r chi.Router) {
		// Scores
		r.Post("/calculate", h.CalculateRiskScore)
		r.Get("/", h.ListRiskScores)
		r.Get("/model", h.GetScoringModel)
		r.Get("/subjects/{subjectType}/{subjectId}/history", h.GetSubjectHistory)

		// Threshold rules (static paths win over /{id} in chi)
		r.Get("/thresholds", h.ListThresholdRules)
		r.Post("/thresholds", h.CreateThresholdRule)
		r.Get("/thresholds/effective", h.GetEffectiveThresholds)
		r.Get("/thresholds/{id}", h.GetThresholdRule)
		r.Put("/thresholds/{id}", h.UpdateThresholdRule)
		r.Post("/thresholds/{id}/approve", h.ApproveThresholdRule)
		r.Post("/thresholds/{id}/retire", h.RetireThresholdRule)

		r.Get("/{id}", h.GetRiskScore)
		r.Post("/{id}/review", h.ReviewRiskScore)
		r.Post("/{id}/archive", h.ArchiveRiskScore)
		// DELETE is kept for clients of the 000001-era API. It archives —
		// scores are never deleted.
		r.Delete("/{id}", h.ArchiveRiskScore)
	})

	return r
}

// ---------------------------------------------------------------------------
// Scores
// ---------------------------------------------------------------------------

// CalculateRiskScore handles POST /v1/risk-scores/calculate.
func (h *Handler) CalculateRiskScore(w http.ResponseWriter, r *http.Request) {
	var request domain.CalculateRiskScoreRequest
	cmd, err := readCommand(w, r, &request, false)
	if err != nil {
		h.writeError(w, r, err)
		return
	}
	result, err := h.service.CalculateScore(r.Context(), cmd, request)
	if err != nil {
		h.writeError(w, r, err)
		return
	}
	writeCommandResult(w, result)
}

// ListRiskScores handles GET /v1/risk-scores.
func (h *Handler) ListRiskScores(w http.ResponseWriter, r *http.Request) {
	query, err := queryContext(r)
	if err != nil {
		h.writeError(w, r, err)
		return
	}
	filter, err := assessmentFilter(r)
	if err != nil {
		h.writeError(w, r, err)
		return
	}
	page, err := pageRequest(r)
	if err != nil {
		h.writeError(w, r, err)
		return
	}
	result, err := h.service.ListScores(r.Context(), query, filter, page)
	if err != nil {
		h.writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, result)
}

// GetRiskScore handles GET /v1/risk-scores/{id}.
func (h *Handler) GetRiskScore(w http.ResponseWriter, r *http.Request) {
	query, err := queryContext(r)
	if err != nil {
		h.writeError(w, r, err)
		return
	}
	id, err := uuidParam(r, "id")
	if err != nil {
		h.writeError(w, r, err)
		return
	}
	assessment, err := h.service.GetScore(r.Context(), query, id)
	if err != nil {
		h.writeError(w, r, err)
		return
	}
	w.Header().Set("ETag", service.ETagForVersion(assessment.Version))
	writeJSON(w, http.StatusOK, assessment)
}

// GetSubjectHistory handles GET /v1/risk-scores/subjects/{subjectType}/{subjectId}/history.
func (h *Handler) GetSubjectHistory(w http.ResponseWriter, r *http.Request) {
	query, err := queryContext(r)
	if err != nil {
		h.writeError(w, r, err)
		return
	}
	subjectType, subjectID, err := subjectParams(r)
	if err != nil {
		h.writeError(w, r, err)
		return
	}
	page, err := pageRequest(r)
	if err != nil {
		h.writeError(w, r, err)
		return
	}
	history, err := h.service.GetSubjectHistory(r.Context(), query, subjectType, subjectID, page)
	if err != nil {
		h.writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, history)
}

// ReviewRiskScore handles POST /v1/risk-scores/{id}/review.
func (h *Handler) ReviewRiskScore(w http.ResponseWriter, r *http.Request) {
	id, err := uuidParam(r, "id")
	if err != nil {
		h.writeError(w, r, err)
		return
	}
	var request domain.ReviewRiskScoreRequest
	cmd, err := readCommand(w, r, &request, false)
	if err != nil {
		h.writeError(w, r, err)
		return
	}
	result, err := h.service.ReviewScore(r.Context(), cmd, id, request)
	if err != nil {
		h.writeError(w, r, err)
		return
	}
	writeCommandResult(w, result)
}

// ArchiveRiskScore handles POST /v1/risk-scores/{id}/archive and the legacy
// DELETE /v1/risk-scores/{id}. The body ({"reason": "..."}) is optional.
func (h *Handler) ArchiveRiskScore(w http.ResponseWriter, r *http.Request) {
	id, err := uuidParam(r, "id")
	if err != nil {
		h.writeError(w, r, err)
		return
	}
	var request domain.ArchiveRiskScoreRequest
	cmd, err := readCommand(w, r, &request, true)
	if err != nil {
		h.writeError(w, r, err)
		return
	}
	result, err := h.service.ArchiveScore(r.Context(), cmd, id, request)
	if err != nil {
		h.writeError(w, r, err)
		return
	}
	writeCommandResult(w, result)
}

// GetScoringModel handles GET /v1/risk-scores/model: the factor weights,
// saturation points and default bands every score is computed with, so a
// stored factor breakdown can be re-derived by hand.
func (h *Handler) GetScoringModel(w http.ResponseWriter, r *http.Request) {
	if _, err := queryContext(r); err != nil {
		h.writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, domain.CurrentModel)
}

// ---------------------------------------------------------------------------
// Threshold rules
// ---------------------------------------------------------------------------

// CreateThresholdRule handles POST /v1/risk-scores/thresholds.
func (h *Handler) CreateThresholdRule(w http.ResponseWriter, r *http.Request) {
	var input domain.ThresholdRuleInput
	cmd, err := readCommand(w, r, &input, false)
	if err != nil {
		h.writeError(w, r, err)
		return
	}
	result, err := h.service.CreateThresholdRule(r.Context(), cmd, input)
	if err != nil {
		h.writeError(w, r, err)
		return
	}
	writeCommandResult(w, result)
}

// ListThresholdRules handles GET /v1/risk-scores/thresholds.
func (h *Handler) ListThresholdRules(w http.ResponseWriter, r *http.Request) {
	query, err := queryContext(r)
	if err != nil {
		h.writeError(w, r, err)
		return
	}
	status, err := enumQuery(r, "status", func(v string) bool { return domain.ThresholdStatus(v).IsValid() },
		"DRAFT, ACTIVE, SUPERSEDED, RETIRED")
	if err != nil {
		h.writeError(w, r, err)
		return
	}
	category, err := enumQuery(r, "risk_category", func(v string) bool { return domain.RiskCategory(v).IsThresholdCategory() },
		"COMPOSITE, REGULATORY_OBLIGATIONS, POLICY_VIOLATIONS, AUDIT_EXCEPTIONS, DATA_PRIVACY, TAX_COMPLIANCE")
	if err != nil {
		h.writeError(w, r, err)
		return
	}
	page, err := pageRequest(r)
	if err != nil {
		h.writeError(w, r, err)
		return
	}
	result, err := h.service.ListThresholdRules(r.Context(), query, store.ThresholdFilter{Status: status, RiskCategory: category}, page)
	if err != nil {
		h.writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, result)
}

// GetEffectiveThresholds handles GET /v1/risk-scores/thresholds/effective?as_of=YYYY-MM-DD.
func (h *Handler) GetEffectiveThresholds(w http.ResponseWriter, r *http.Request) {
	query, err := queryContext(r)
	if err != nil {
		h.writeError(w, r, err)
		return
	}
	asOf, err := dateQuery(r, "as_of")
	if err != nil {
		h.writeError(w, r, err)
		return
	}
	result, err := h.service.GetEffectiveThresholds(r.Context(), query, asOf)
	if err != nil {
		h.writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, result)
}

// GetThresholdRule handles GET /v1/risk-scores/thresholds/{id}.
func (h *Handler) GetThresholdRule(w http.ResponseWriter, r *http.Request) {
	query, err := queryContext(r)
	if err != nil {
		h.writeError(w, r, err)
		return
	}
	id, err := uuidParam(r, "id")
	if err != nil {
		h.writeError(w, r, err)
		return
	}
	rule, err := h.service.GetThresholdRule(r.Context(), query, id)
	if err != nil {
		h.writeError(w, r, err)
		return
	}
	w.Header().Set("ETag", service.ETagForVersion(rule.Version))
	writeJSON(w, http.StatusOK, rule)
}

// UpdateThresholdRule handles PUT /v1/risk-scores/thresholds/{id} (DRAFT
// only, If-Match required).
func (h *Handler) UpdateThresholdRule(w http.ResponseWriter, r *http.Request) {
	id, err := uuidParam(r, "id")
	if err != nil {
		h.writeError(w, r, err)
		return
	}
	var input domain.ThresholdRuleInput
	cmd, err := readCommand(w, r, &input, false)
	if err != nil {
		h.writeError(w, r, err)
		return
	}
	result, err := h.service.UpdateThresholdRule(r.Context(), cmd, id, input)
	if err != nil {
		h.writeError(w, r, err)
		return
	}
	writeCommandResult(w, result)
}

// ApproveThresholdRule handles POST /v1/risk-scores/thresholds/{id}/approve.
func (h *Handler) ApproveThresholdRule(w http.ResponseWriter, r *http.Request) {
	id, err := uuidParam(r, "id")
	if err != nil {
		h.writeError(w, r, err)
		return
	}
	var request domain.ThresholdTransitionRequest
	cmd, err := readCommand(w, r, &request, true)
	if err != nil {
		h.writeError(w, r, err)
		return
	}
	result, err := h.service.ApproveThresholdRule(r.Context(), cmd, id, request)
	if err != nil {
		h.writeError(w, r, err)
		return
	}
	writeCommandResult(w, result)
}

// RetireThresholdRule handles POST /v1/risk-scores/thresholds/{id}/retire.
func (h *Handler) RetireThresholdRule(w http.ResponseWriter, r *http.Request) {
	id, err := uuidParam(r, "id")
	if err != nil {
		h.writeError(w, r, err)
		return
	}
	var request domain.ThresholdTransitionRequest
	cmd, err := readCommand(w, r, &request, true)
	if err != nil {
		h.writeError(w, r, err)
		return
	}
	result, err := h.service.RetireThresholdRule(r.Context(), cmd, id, request)
	if err != nil {
		h.writeError(w, r, err)
		return
	}
	writeCommandResult(w, result)
}

// ---------------------------------------------------------------------------
// Query parsing helpers specific to these routes
// ---------------------------------------------------------------------------

func assessmentFilter(r *http.Request) (store.AssessmentFilter, error) {
	var filter store.AssessmentFilter
	var err error

	filter.LegalEntityID = strings.TrimSpace(r.URL.Query().Get("legal_entity_id"))
	if len(filter.LegalEntityID) > domain.MaxIdentifierLength {
		return filter, domain.BadRequest("invalid_filter", "legal_entity_id must be at most 64 characters")
	}
	if filter.RiskTier, err = enumQuery(r, "risk_tier", func(v string) bool { return domain.RiskTier(v).IsValid() },
		"LOW, MODERATE, HIGH, CRITICAL"); err != nil {
		return filter, err
	}
	if filter.Status, err = enumQuery(r, "status", func(v string) bool { return domain.ScoreStatus(v).IsValid() },
		"ACTIVE, SUPERSEDED, ARCHIVED"); err != nil {
		return filter, err
	}
	if filter.ReviewStatus, err = enumQuery(r, "review_status", isReviewStatus,
		"NOT_REQUIRED, PENDING_REVIEW, CONFIRMED, DISMISSED, ESCALATED"); err != nil {
		return filter, err
	}
	if filter.SubjectType, err = enumQuery(r, "subject_type", func(v string) bool { return domain.SubjectType(v).IsValid() },
		"LEGAL_ENTITY, OBLIGATION, FILING, CONTROL"); err != nil {
		return filter, err
	}
	filter.SubjectID = strings.TrimSpace(r.URL.Query().Get("subject_id"))
	if len(filter.SubjectID) > domain.MaxSubjectIDLength {
		return filter, domain.BadRequest("invalid_filter", "subject_id must be at most 128 characters")
	}
	return filter, nil
}

func isReviewStatus(value string) bool {
	switch domain.ReviewStatus(value) {
	case domain.ReviewNotRequired, domain.ReviewPending, domain.ReviewConfirmed, domain.ReviewDismissed, domain.ReviewEscalated:
		return true
	}
	return false
}

func subjectParams(r *http.Request) (domain.SubjectType, string, error) {
	subjectType := domain.SubjectType(strings.ToUpper(chi.URLParam(r, "subjectType")))
	if !subjectType.IsValid() {
		return "", "", domain.BadRequest("invalid_subject_type", "subjectType must be one of LEGAL_ENTITY, OBLIGATION, FILING, CONTROL")
	}
	subjectID := chi.URLParam(r, "subjectId")
	if subjectID == "" || len(subjectID) > domain.MaxSubjectIDLength {
		return "", "", domain.BadRequest("invalid_subject_id", "subjectId must be 1..128 characters")
	}
	return subjectType, subjectID, nil
}
