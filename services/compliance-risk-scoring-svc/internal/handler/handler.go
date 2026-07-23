package handler

import (
	"encoding/json"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"go.uber.org/zap"
	"zoiko.io/compliance-risk-scoring-svc/internal/authz"
	"zoiko.io/compliance-risk-scoring-svc/internal/domain"
	"zoiko.io/compliance-risk-scoring-svc/internal/events"
	"zoiko.io/compliance-risk-scoring-svc/internal/health"
	customMiddleware "zoiko.io/compliance-risk-scoring-svc/internal/middleware"
	"zoiko.io/compliance-risk-scoring-svc/internal/store"
)

type Handler struct {
	store     store.Store
	publisher *events.Publisher
	authz     *authz.Client
	logger    *zap.Logger
}

func NewHandler(s store.Store, p *events.Publisher, a *authz.Client, l *zap.Logger) *Handler {
	return &Handler{
		store:     s,
		publisher: p,
		authz:     a,
		logger:    l,
	}
}

func NewRouter(h *Handler) http.Handler {
	r := chi.NewRouter()

	r.Use(middleware.RequestID)
	r.Use(middleware.RealIP)
	r.Use(middleware.Logger)
	r.Use(middleware.Recoverer)
	r.Use(customMiddleware.TenantMiddleware)

	r.Get("/healthz", health.Handler())

	r.Route("/v1/risk-scores", func(r chi.Router) {
		r.Post("/calculate", h.CalculateRiskScore)
		r.Get("/", h.ListAssessments)
		r.Get("/thresholds", h.ListThresholdRules)
		r.Post("/thresholds", h.CreateThresholdRule)
		r.Get("/{id}", h.GetAssessmentByID)
		r.Delete("/{id}", h.ArchiveAssessment)
	})

	return r
}

func (h *Handler) CalculateRiskScore(w http.ResponseWriter, r *http.Request) {
	tenantID := customMiddleware.GetTenantID(r.Context())

	var req domain.CalculateRiskScoreRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		h.respondError(w, http.StatusBadRequest, "invalid request body")
		return
	}

	if err := req.Validate(); err != nil {
		h.respondError(w, http.StatusBadRequest, err.Error())
		return
	}

	compositeScore, tier, breakdowns := domain.ComputeRiskScore(&req, "", tenantID)

	assessment := &domain.RiskScoreAssessment{
		LegalEntityID:         req.LegalEntityID,
		AssessmentName:        req.AssessmentName,
		CompositeRiskScore:    compositeScore,
		RiskTier:              tier,
		OpenObligationsCount:  req.OpenObligationsCount,
		PolicyViolationsCount: req.PolicyViolationsCount,
		AuditExceptionsCount:  req.AuditExceptionsCount,
		PrivacyIncidentsCount: req.PrivacyIncidentsCount,
		TaxPenaltiesCount:     req.TaxPenaltiesCount,
		Status:                "ACTIVE",
	}

	if err := h.store.CreateAssessment(r.Context(), tenantID, assessment, breakdowns); err != nil {
		h.logger.Error("failed to save risk assessment", zap.Error(err))
		h.respondError(w, http.StatusInternalServerError, "failed to calculate risk score")
		return
	}

	_ = h.publisher.Publish(r.Context(), "risk_score.calculated", tenantID, assessment)

	if tier == domain.TierHigh || tier == domain.TierCritical {
		_ = h.publisher.Publish(r.Context(), "risk_threshold.exceeded", tenantID, map[string]interface{}{
			"assessment_id": assessment.ID,
			"risk_tier":     tier,
			"score":         compositeScore,
		})
	}

	h.respondJSON(w, http.StatusCreated, assessment)
}

func (h *Handler) ListAssessments(w http.ResponseWriter, r *http.Request) {
	tenantID := customMiddleware.GetTenantID(r.Context())
	legalEntityID := r.URL.Query().Get("legal_entity_id")
	tier := r.URL.Query().Get("risk_tier")

	assessments, err := h.store.ListAssessments(r.Context(), tenantID, legalEntityID, tier)
	if err != nil {
		h.logger.Error("failed to list assessments", zap.Error(err))
		h.respondError(w, http.StatusInternalServerError, "failed to query assessments")
		return
	}

	if assessments == nil {
		assessments = []domain.RiskScoreAssessment{}
	}

	h.respondJSON(w, http.StatusOK, map[string]interface{}{
		"data":  assessments,
		"count": len(assessments),
	})
}

func (h *Handler) GetAssessmentByID(w http.ResponseWriter, r *http.Request) {
	tenantID := customMiddleware.GetTenantID(r.Context())
	id := chi.URLParam(r, "id")

	assessment, err := h.store.GetAssessmentByID(r.Context(), tenantID, id)
	if err != nil {
		h.respondError(w, http.StatusNotFound, "risk assessment not found")
		return
	}

	h.respondJSON(w, http.StatusOK, assessment)
}

func (h *Handler) CreateThresholdRule(w http.ResponseWriter, r *http.Request) {
	tenantID := customMiddleware.GetTenantID(r.Context())

	var rule domain.RiskThresholdRule
	if err := json.NewDecoder(r.Body).Decode(&rule); err != nil {
		h.respondError(w, http.StatusBadRequest, "invalid request body")
		return
	}

	if rule.RuleName == "" || rule.RiskCategory == "" {
		h.respondError(w, http.StatusBadRequest, "rule_name and risk_category are required")
		return
	}

	if rule.HighThreshold == 0 {
		rule.HighThreshold = 60.0
	}
	if rule.CriticalThreshold == 0 {
		rule.CriticalThreshold = 80.0
	}
	if rule.NotificationChannel == "" {
		rule.NotificationChannel = "GOVERNANCE_DESK"
	}
	rule.IsActive = true

	if err := h.store.CreateThresholdRule(r.Context(), tenantID, &rule); err != nil {
		h.logger.Error("failed to create threshold rule", zap.Error(err))
		h.respondError(w, http.StatusInternalServerError, "failed to create threshold rule")
		return
	}

	h.respondJSON(w, http.StatusCreated, rule)
}

func (h *Handler) ListThresholdRules(w http.ResponseWriter, r *http.Request) {
	tenantID := customMiddleware.GetTenantID(r.Context())

	rules, err := h.store.ListThresholdRules(r.Context(), tenantID)
	if err != nil {
		h.logger.Error("failed to list threshold rules", zap.Error(err))
		h.respondError(w, http.StatusInternalServerError, "failed to query threshold rules")
		return
	}

	if rules == nil {
		rules = []domain.RiskThresholdRule{}
	}

	h.respondJSON(w, http.StatusOK, map[string]interface{}{
		"data":  rules,
		"count": len(rules),
	})
}

func (h *Handler) ArchiveAssessment(w http.ResponseWriter, r *http.Request) {
	tenantID := customMiddleware.GetTenantID(r.Context())
	id := chi.URLParam(r, "id")

	if err := h.store.ArchiveAssessment(r.Context(), tenantID, id); err != nil {
		h.respondError(w, http.StatusNotFound, "assessment not found")
		return
	}

	_ = h.publisher.Publish(r.Context(), "risk_assessment.archived", tenantID, map[string]string{"id": id, "status": "ARCHIVED"})

	h.respondJSON(w, http.StatusOK, map[string]string{
		"message": "risk assessment archived successfully",
		"id":      id,
	})
}

func (h *Handler) respondJSON(w http.ResponseWriter, code int, payload interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(payload)
}

func (h *Handler) respondError(w http.ResponseWriter, code int, message string) {
	h.respondJSON(w, code, map[string]string{"error": message})
}
