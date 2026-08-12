// Applicability decision HTTP handlers — doc7 §E2.
//
// No authz gate here, matching this service's existing posture: obligations-
// svc has no authorization-svc client anywhere today (a pre-existing gap
// tracked separately in docs/architecture/doc7-implementation-backlog.md,
// out of scope for this addition). CreatedByPrincipalID is accepted from
// the request body, same as CreateObligation/CreateFilingRequirement.
package handler

import (
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"go.uber.org/zap"

	"zoiko.io/obligations-svc/internal/domain"
)

// RegisterApplicabilityRoutes mounts applicability-decision routes on the
// given chi router. Call after RegisterRoutes so its correlationIDMiddleware
// also covers these routes.
func RegisterApplicabilityRoutes(r chi.Router, h *Handler) {
	r.Post("/v1/obligations/{obligation_id}/applicability-decisions", h.CreateApplicabilityDecision)
	r.Get("/v1/obligations/{obligation_id}/applicability-decisions", h.ListApplicabilityDecisions)
	r.Get("/v1/obligations/{obligation_id}/applicability", h.GetCurrentApplicability)
}

// ── POST /v1/obligations/{obligation_id}/applicability-decisions ──────────

type createApplicabilityDecisionRequest struct {
	ApplicabilityDecisionID string          `json:"applicability_decision_id,omitempty"`
	JurisdictionCode        string          `json:"jurisdiction_code"`
	EntityRef               string          `json:"entity_ref"`
	ActivityRef             string          `json:"activity_ref,omitempty"`
	ProductProcessRef       string          `json:"product_process_ref,omitempty"`
	Decision                string          `json:"decision"`
	SourceRuleRef           string          `json:"source_rule_ref"`
	SourceRuleVersion       string          `json:"source_rule_version"`
	EffectiveFrom           time.Time       `json:"effective_from"`
	EffectiveTo             *time.Time      `json:"effective_to,omitempty"`
	FactsUsed               json.RawMessage `json:"facts_used,omitempty"`
	Confidence              *float64        `json:"confidence,omitempty"`
	UncertaintyNotes        string          `json:"uncertainty_notes,omitempty"`
	DecidedByPrincipalID    string          `json:"decided_by_principal_id,omitempty"`
	DecidedBySystem         string          `json:"decided_by_system,omitempty"`
	ReviewRequired          bool            `json:"review_required,omitempty"`
	CreatedByPrincipalID    string          `json:"created_by_principal_id"`
}

func (req createApplicabilityDecisionRequest) missingField() string {
	switch {
	case req.JurisdictionCode == "":
		return "jurisdiction_code"
	case req.EntityRef == "":
		return "entity_ref"
	case req.Decision == "":
		return "decision"
	case req.SourceRuleRef == "":
		return "source_rule_ref"
	case req.SourceRuleVersion == "":
		return "source_rule_version"
	case req.EffectiveFrom.IsZero():
		return "effective_from"
	case req.CreatedByPrincipalID == "":
		return "created_by_principal_id"
	// doc7 §E2's "actor/system" — a decision must be attributable to
	// exactly one of a human or an automated rule engine.
	case req.DecidedByPrincipalID == "" && req.DecidedBySystem == "":
		return "decided_by_principal_id or decided_by_system"
	default:
		return ""
	}
}

// CreateApplicabilityDecision handles
// POST /v1/obligations/{obligation_id}/applicability-decisions.
//
// Response:
//
//	201 → decision recorded
//	400 → missing required field
//	404 → obligation_id not found
//	503 → store unavailable
func (h *Handler) CreateApplicabilityDecision(w http.ResponseWriter, r *http.Request) {
	obligationID := chi.URLParam(r, "obligation_id")
	correlationID := r.Header.Get("X-Correlation-ID")

	var req createApplicabilityDecisionRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid_json", "message": err.Error()})
		return
	}
	if missing := req.missingField(); missing != "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "missing_field", "field": missing})
		return
	}

	d, err := h.store.CreateApplicabilityDecision(r.Context(), domain.CreateApplicabilityDecisionParams{
		ApplicabilityDecisionID: req.ApplicabilityDecisionID,
		ObligationID:            obligationID,
		JurisdictionCode:        req.JurisdictionCode,
		EntityRef:               req.EntityRef,
		ActivityRef:             req.ActivityRef,
		ProductProcessRef:       req.ProductProcessRef,
		Decision:                req.Decision,
		SourceRuleRef:           req.SourceRuleRef,
		SourceRuleVersion:       req.SourceRuleVersion,
		EffectiveFrom:           req.EffectiveFrom,
		EffectiveTo:             req.EffectiveTo,
		FactsUsed:               req.FactsUsed,
		Confidence:              req.Confidence,
		UncertaintyNotes:        req.UncertaintyNotes,
		DecidedByPrincipalID:    req.DecidedByPrincipalID,
		DecidedBySystem:         req.DecidedBySystem,
		ReviewRequired:          req.ReviewRequired,
		CreatedByPrincipalID:    req.CreatedByPrincipalID,
	})
	if err != nil {
		switch {
		case errors.Is(err, domain.ErrObligationNotFound):
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "obligation_not_found", "obligation_id": obligationID})
		default:
			h.log.Error("CreateApplicabilityDecision: store unavailable",
				zap.String("obligation_id", obligationID),
				zap.String("correlation_id", correlationID),
				zap.Error(err),
			)
			writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "store_unavailable"})
		}
		return
	}
	writeJSON(w, http.StatusCreated, d)
}

// ── GET /v1/obligations/{obligation_id}/applicability-decisions ───────────

// ListApplicabilityDecisions handles
// GET /v1/obligations/{obligation_id}/applicability-decisions?jurisdiction_code=&entity_ref=
// — the full versioned decision history for a scope, per doc7 §E2.
func (h *Handler) ListApplicabilityDecisions(w http.ResponseWriter, r *http.Request) {
	obligationID := chi.URLParam(r, "obligation_id")
	jurisdictionCode := r.URL.Query().Get("jurisdiction_code")
	entityRef := r.URL.Query().Get("entity_ref")
	if jurisdictionCode == "" || entityRef == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "missing_field", "field": "jurisdiction_code and entity_ref query params are required"})
		return
	}

	results, err := h.store.ListApplicabilityDecisions(r.Context(), obligationID, jurisdictionCode, entityRef)
	if err != nil {
		switch {
		case errors.Is(err, domain.ErrObligationNotFound):
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "obligation_not_found", "obligation_id": obligationID})
		default:
			writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "store_unavailable"})
		}
		return
	}
	if results == nil {
		results = []*domain.ApplicabilityDecision{}
	}
	writeJSON(w, http.StatusOK, results)
}

// ── GET /v1/obligations/{obligation_id}/applicability ─────────────────────

// GetCurrentApplicability handles
// GET /v1/obligations/{obligation_id}/applicability?jurisdiction_code=&entity_ref=
// — doc7 §E2's core answer: UNASSESSED when no decision has ever been made
// for this scope, never silently defaulted to NOT_APPLICABLE.
func (h *Handler) GetCurrentApplicability(w http.ResponseWriter, r *http.Request) {
	obligationID := chi.URLParam(r, "obligation_id")
	jurisdictionCode := r.URL.Query().Get("jurisdiction_code")
	entityRef := r.URL.Query().Get("entity_ref")
	if jurisdictionCode == "" || entityRef == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "missing_field", "field": "jurisdiction_code and entity_ref query params are required"})
		return
	}

	result, err := h.store.FindCurrentApplicability(r.Context(), obligationID, jurisdictionCode, entityRef)
	if err != nil {
		switch {
		case errors.Is(err, domain.ErrObligationNotFound):
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "obligation_not_found", "obligation_id": obligationID})
		default:
			writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "store_unavailable"})
		}
		return
	}
	writeJSON(w, http.StatusOK, result)
}
