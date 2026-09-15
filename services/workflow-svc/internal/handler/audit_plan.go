package handler

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/go-chi/chi/v5"

	"zoiko.io/workflow-svc/internal/domain"
)

const (
	actionAuditPlanManage  = "AUDIT_PLAN_MANAGE"
	actionAuditPlanApprove = "AUDIT_PLAN_APPROVE"
	actionAuditPlanRead    = "AUDIT_PLAN_READ"
)

func (h *Handler) requireCorrelationID(w http.ResponseWriter, r *http.Request) (string, bool) {
	correlationID := r.Header.Get("X-Correlation-ID")
	if correlationID == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "missing_correlation_id"})
		return "", false
	}
	return correlationID, true
}

// CreateAuditPlan creates AUD-02's own AuditPlan container for an
// engagement that must already exist.
func (h *Handler) CreateAuditPlan(w http.ResponseWriter, r *http.Request) {
	actor, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	tenantID, ok := h.requireTenant(w, r)
	if !ok {
		return
	}
	correlationID, ok := h.requireCorrelationID(w, r)
	if !ok {
		return
	}
	engagement, err := h.store.GetAuditEngagement(r.Context(), tenantID, chi.URLParam(r, "engagement_id"))
	if err != nil {
		h.writeAuditEngagementErr(w, err)
		return
	}
	if err := h.authz.CheckAllowed(r.Context(), actor, engagement.LegalEntityID, actionAuditPlanManage); err != nil {
		h.writeAuditEngagementAuthzErr(w, err)
		return
	}
	plan, created, err := h.store.CreateAuditPlan(r.Context(), domain.CreateAuditPlanParams{
		EngagementID: engagement.EngagementID, TenantID: tenantID, CreatedByPrincipalID: actor, CorrelationID: correlationID,
	})
	if err != nil {
		h.writeAuditPlanErr(w, err)
		return
	}
	status := http.StatusOK
	if created {
		status = http.StatusCreated
	}
	writeJSON(w, status, plan)
}

func (h *Handler) GetAuditPlan(w http.ResponseWriter, r *http.Request) {
	tenantID, ok := h.requireTenant(w, r)
	if !ok {
		return
	}
	actor, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	plan, err := h.store.GetAuditPlanByEngagement(r.Context(), tenantID, chi.URLParam(r, "engagement_id"))
	if err != nil {
		h.writeAuditPlanErr(w, err)
		return
	}
	engagement, err := h.store.GetAuditEngagement(r.Context(), tenantID, plan.EngagementID)
	if err != nil {
		h.writeAuditEngagementErr(w, err)
		return
	}
	if err := h.authz.CheckAllowed(r.Context(), actor, engagement.LegalEntityID, actionAuditPlanRead); err != nil {
		h.writeAuditEngagementAuthzErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, plan)
}

type recordMaterialityRequest struct {
	OverallMateriality      float64  `json:"overall_materiality"`
	PerformanceMateriality  float64  `json:"performance_materiality"`
	ClearlyTrivialThreshold *float64 `json:"clearly_trivial_threshold,omitempty"`
	Rationale               string   `json:"rationale"`
}

// RecordMateriality is the real enforcement point for "materiality
// changes trigger dependency analysis" — see PgStore.RecordMateriality's
// own doc comment for the mechanism.
func (h *Handler) RecordMateriality(w http.ResponseWriter, r *http.Request) {
	actor, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	tenantID, ok := h.requireTenant(w, r)
	if !ok {
		return
	}
	planID := chi.URLParam(r, "plan_id")
	plan, err := h.store.GetAuditPlan(r.Context(), tenantID, planID)
	if err != nil {
		h.writeAuditPlanErr(w, err)
		return
	}
	engagement, err := h.store.GetAuditEngagement(r.Context(), tenantID, plan.EngagementID)
	if err != nil {
		h.writeAuditEngagementErr(w, err)
		return
	}
	if err := h.authz.CheckAllowed(r.Context(), actor, engagement.LegalEntityID, actionAuditPlanManage); err != nil {
		h.writeAuditEngagementAuthzErr(w, err)
		return
	}
	var req recordMaterialityRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid_json"})
		return
	}
	if req.Rationale == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "missing_field", "field": "rationale"})
		return
	}
	materiality, err := h.store.RecordMateriality(r.Context(), domain.RecordMaterialityParams{
		PlanID: planID, TenantID: tenantID, ActorPrincipalID: actor, CorrelationID: r.Header.Get("X-Correlation-ID"),
		OverallMateriality: req.OverallMateriality, PerformanceMateriality: req.PerformanceMateriality,
		ClearlyTrivialThreshold: req.ClearlyTrivialThreshold, Rationale: req.Rationale,
	})
	if err != nil {
		h.writeAuditPlanErr(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, materiality)
}

type identifyRiskRequest struct {
	Description string `json:"description"`
	RiskLevel   string `json:"risk_level"`
}

func (h *Handler) IdentifyRisk(w http.ResponseWriter, r *http.Request) {
	actor, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	tenantID, ok := h.requireTenant(w, r)
	if !ok {
		return
	}
	correlationID, ok := h.requireCorrelationID(w, r)
	if !ok {
		return
	}
	planID := chi.URLParam(r, "plan_id")
	plan, err := h.store.GetAuditPlan(r.Context(), tenantID, planID)
	if err != nil {
		h.writeAuditPlanErr(w, err)
		return
	}
	engagement, err := h.store.GetAuditEngagement(r.Context(), tenantID, plan.EngagementID)
	if err != nil {
		h.writeAuditEngagementErr(w, err)
		return
	}
	if err := h.authz.CheckAllowed(r.Context(), actor, engagement.LegalEntityID, actionAuditPlanManage); err != nil {
		h.writeAuditEngagementAuthzErr(w, err)
		return
	}
	var req identifyRiskRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid_json"})
		return
	}
	if req.Description == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "missing_field", "field": "description"})
		return
	}
	switch req.RiskLevel {
	case domain.RiskLevelLow, domain.RiskLevelModerate, domain.RiskLevelHigh:
	default:
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid_field", "field": "risk_level"})
		return
	}
	risk, created, err := h.store.IdentifyRisk(r.Context(), domain.IdentifyRiskParams{
		PlanID: planID, EngagementID: plan.EngagementID, TenantID: tenantID, Description: req.Description,
		RiskLevel: req.RiskLevel, CreatedByPrincipalID: actor, CorrelationID: correlationID,
	})
	if err != nil {
		h.writeAuditPlanErr(w, err)
		return
	}
	status := http.StatusOK
	if created {
		status = http.StatusCreated
	}
	writeJSON(w, status, risk)
}

func (h *Handler) ListRisks(w http.ResponseWriter, r *http.Request) {
	tenantID, ok := h.requireTenant(w, r)
	if !ok {
		return
	}
	actor, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	planID := chi.URLParam(r, "plan_id")
	plan, err := h.store.GetAuditPlan(r.Context(), tenantID, planID)
	if err != nil {
		h.writeAuditPlanErr(w, err)
		return
	}
	engagement, err := h.store.GetAuditEngagement(r.Context(), tenantID, plan.EngagementID)
	if err != nil {
		h.writeAuditEngagementErr(w, err)
		return
	}
	if err := h.authz.CheckAllowed(r.Context(), actor, engagement.LegalEntityID, actionAuditPlanRead); err != nil {
		h.writeAuditEngagementAuthzErr(w, err)
		return
	}
	risks, err := h.store.ListRisksByPlan(r.Context(), tenantID, planID)
	if err != nil {
		h.writeAuditPlanErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, risks)
}

func (h *Handler) getRiskForManage(w http.ResponseWriter, r *http.Request, riskID string) (*domain.RiskAssessment, bool) {
	tenantID, ok := h.requireTenant(w, r)
	if !ok {
		return nil, false
	}
	actor, ok := h.requirePrincipal(w, r)
	if !ok {
		return nil, false
	}
	risk, err := h.store.GetRiskAssessment(r.Context(), tenantID, riskID)
	if err != nil {
		h.writeAuditPlanErr(w, err)
		return nil, false
	}
	engagement, err := h.store.GetAuditEngagement(r.Context(), tenantID, risk.EngagementID)
	if err != nil {
		h.writeAuditEngagementErr(w, err)
		return nil, false
	}
	if err := h.authz.CheckAllowed(r.Context(), actor, engagement.LegalEntityID, actionAuditPlanManage); err != nil {
		h.writeAuditEngagementAuthzErr(w, err)
		return nil, false
	}
	return risk, true
}

type linkAssertionRequest struct {
	AssertionCode string `json:"assertion_code"`
}

func (h *Handler) LinkAssertion(w http.ResponseWriter, r *http.Request) {
	riskID := chi.URLParam(r, "risk_id")
	if _, ok := h.getRiskForManage(w, r, riskID); !ok {
		return
	}
	tenantID, _ := h.requireTenant(w, r)
	var req linkAssertionRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid_json"})
		return
	}
	if req.AssertionCode == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "missing_field", "field": "assertion_code"})
		return
	}
	if err := h.store.LinkAssertion(r.Context(), domain.LinkAssertionParams{RiskID: riskID, TenantID: tenantID, AssertionCode: req.AssertionCode}); err != nil {
		h.writeAuditPlanErr(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]string{"status": "linked"})
}

type designAuditResponseRequest struct {
	Description string `json:"description"`
}

func (h *Handler) DesignAuditResponse(w http.ResponseWriter, r *http.Request) {
	riskID := chi.URLParam(r, "risk_id")
	if _, ok := h.getRiskForManage(w, r, riskID); !ok {
		return
	}
	tenantID, _ := h.requireTenant(w, r)
	principalID, _ := h.requirePrincipal(w, r)
	var req designAuditResponseRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid_json"})
		return
	}
	if req.Description == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "missing_field", "field": "description"})
		return
	}
	if err := h.store.DesignAuditResponse(r.Context(), domain.DesignAuditResponseParams{
		RiskID: riskID, TenantID: tenantID, Description: req.Description, DesignedByPrincipalID: principalID,
	}); err != nil {
		h.writeAuditPlanErr(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]string{"status": "designed"})
}

// AssessRisk is the real, DB-enforced form of "every risk links to
// assertions/process and response" — see PgStore.AssessRisk's own doc
// comment for the CAS predicate.
func (h *Handler) AssessRisk(w http.ResponseWriter, r *http.Request) {
	riskID := chi.URLParam(r, "risk_id")
	if _, ok := h.getRiskForManage(w, r, riskID); !ok {
		return
	}
	tenantID, _ := h.requireTenant(w, r)
	principalID, _ := h.requirePrincipal(w, r)
	correlationID, ok := h.requireCorrelationID(w, r)
	if !ok {
		return
	}
	risk, _, err := h.store.AssessRisk(r.Context(), domain.AssessRiskParams{RiskID: riskID, TenantID: tenantID, ActorPrincipalID: principalID, CorrelationID: correlationID})
	if err != nil {
		h.writeAuditPlanErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, risk)
}

// MarkSignificantRisk is the only code path anywhere that can set
// is_significant=true, and it always attributes the caller's own
// principal — "significant-risk classification requires authorized
// human," enforced both here and by risk_significant_requires_actor's own
// CHECK constraint.
func (h *Handler) MarkSignificantRisk(w http.ResponseWriter, r *http.Request) {
	riskID := chi.URLParam(r, "risk_id")
	if _, ok := h.getRiskForManage(w, r, riskID); !ok {
		return
	}
	tenantID, _ := h.requireTenant(w, r)
	principalID, _ := h.requirePrincipal(w, r)
	correlationID, ok := h.requireCorrelationID(w, r)
	if !ok {
		return
	}
	risk, err := h.store.MarkSignificantRisk(r.Context(), domain.MarkSignificantRiskParams{RiskID: riskID, TenantID: tenantID, ActorPrincipalID: principalID, CorrelationID: correlationID})
	if err != nil {
		h.writeAuditPlanErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, risk)
}

// ApprovePlan refuses self-approval — the plan's own preparer cannot
// approve it, the same posture as every other maker/checker pair in this
// build (ErrAuditEngagementSelfAcceptance, ErrSelfApprovalNotAllowed).
func (h *Handler) ApprovePlan(w http.ResponseWriter, r *http.Request) {
	actor, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	tenantID, ok := h.requireTenant(w, r)
	if !ok {
		return
	}
	correlationID, ok := h.requireCorrelationID(w, r)
	if !ok {
		return
	}
	planID := chi.URLParam(r, "plan_id")
	plan, err := h.store.GetAuditPlan(r.Context(), tenantID, planID)
	if err != nil {
		h.writeAuditPlanErr(w, err)
		return
	}
	engagement, err := h.store.GetAuditEngagement(r.Context(), tenantID, plan.EngagementID)
	if err != nil {
		h.writeAuditEngagementErr(w, err)
		return
	}
	if err := h.authz.CheckAllowed(r.Context(), actor, engagement.LegalEntityID, actionAuditPlanApprove); err != nil {
		h.writeAuditEngagementAuthzErr(w, err)
		return
	}
	approved, _, err := h.store.ApprovePlan(r.Context(), domain.ApprovePlanParams{PlanID: planID, TenantID: tenantID, ActorPrincipalID: actor, CorrelationID: correlationID})
	if err != nil {
		h.writeAuditPlanErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, approved)
}

func (h *Handler) writeAuditPlanErr(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, domain.ErrAuditPlanNotFound):
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "audit_plan_not_found"})
	case errors.Is(err, domain.ErrAuditRiskNotFound):
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "risk_assessment_not_found"})
	case errors.Is(err, domain.ErrAuditPlanAlreadyExists):
		writeJSON(w, http.StatusConflict, map[string]string{"error": "audit_plan_already_exists"})
	case errors.Is(err, domain.ErrAuditPlanInvalidState):
		writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"error": "invalid_plan_state"})
	case errors.Is(err, domain.ErrAuditRiskInvalidState):
		writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"error": "invalid_risk_state"})
	case errors.Is(err, domain.ErrAuditRiskRequiresAssertionAndResponse):
		writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"error": "risk_requires_assertion_and_response"})
	case errors.Is(err, domain.ErrAuditPlanSelfApproval):
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "self_approval_not_allowed"})
	default:
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "store_unavailable"})
	}
}
