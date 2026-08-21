// Applicability decision HTTP handlers — doc7 §E2.
//
// The header here used to read "no authz gate, matching this service's existing
// posture: obligations-svc has no authorization-svc client anywhere today", and
// that stopped being true when the gap-closing pass gave the service a client
// and gated obligation create, status update and filing-requirement add. This
// file was simply not revisited, so the strongest write in the service stayed
// the only ungated one: recording an applicability decision is the record of
// WHETHER a statutory obligation binds an entity, and filings, evidence and
// aging are all derived from it.
//
// Two things follow, both now enforced below:
//
//   - The write is authorized against the PARENT obligation's legal entity,
//     after a tenant-scoped lookup of that parent. The lookup runs first so an
//     obligation belonging to another tenant answers 404 without revealing
//     whether the caller holds a grant on it.
//   - Attribution comes from X-Principal-Id, never the request body. A
//     body-supplied decided_by is a decision that names whoever the caller
//     chose — the defect board-resolutions-svc shipped, where it defeated the
//     segregation-of-duties check outright.
package handler

import (
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"go.uber.org/zap"

	"zoiko.io/obligations-svc/internal/authz"
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
//	400 → missing required field, or attribution disagreeing with the caller
//	401 → no tenant scope, or no principal
//	403 → principal holds no APPLICABILITY_DECISION_RECORD grant on the entity
//	404 → obligation_id not found, or belongs to another tenant
//	503 → store unavailable
func (h *Handler) CreateApplicabilityDecision(w http.ResponseWriter, r *http.Request) {
	obligationID := chi.URLParam(r, "obligation_id")
	correlationID := r.Header.Get("X-Correlation-ID")

	if _, ok := h.requireTenant(w, r); !ok {
		return
	}
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}

	// decodeJSON, not a bare json.Decoder: this service already had the size cap
	// and the unknown-field check, and this route was simply not using them. An
	// uncapped body is a memory-exhaustion route into a service that is
	// otherwise careful, and silently discarding a misspelled field is how a
	// caller comes to believe it sent a value it did not.
	var req createApplicabilityDecisionRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if missing := req.missingField(); missing != "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "missing_field", "field": missing})
		return
	}

	// Both attribution fields must name the caller. created_by is who recorded
	// the row; decided_by is who reached the conclusion — and when a human is
	// named as the decider it has to be this human, or the record is a decision
	// attributed to a colleague by whoever sent the request. An automated
	// decision names decided_by_system instead and leaves decided_by empty,
	// which stays legitimate.
	if req.CreatedByPrincipalID != principalID {
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"error":  "created_by_mismatch",
			"detail": "created_by_principal_id must be the authenticated principal — attribution comes from X-Principal-Id, not the request body",
		})
		return
	}
	if req.DecidedByPrincipalID != "" && req.DecidedByPrincipalID != principalID {
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"error":  "decided_by_mismatch",
			"detail": "decided_by_principal_id must be the authenticated principal — a decision cannot be attributed to another principal by the request body; use decided_by_system for an automated decision",
		})
		return
	}

	// The parent obligation is read BEFORE authorization, and it is read
	// tenant-scoped: another tenant's obligation is not found. That order
	// matters — authorizing first would answer 403 for an obligation the caller
	// cannot see and 404 for one that does not exist, which turns the endpoint
	// into an existence oracle for other tenants' registers.
	parent, err := h.store.FindObligationByID(r.Context(), obligationID)
	if err != nil {
		h.writeStoreErr(w, "CreateApplicabilityDecision: parent lookup failed", err)
		return
	}

	// Entity-scoped, like every other write in this service: the authority to
	// decide applicability is held over a legal entity, not platform-wide.
	if !h.authorize(w, r, principalID, parent.LegalEntityID, authz.ActionApplicabilityDecide) {
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

	// Without this, a request carrying no tenant header reached the store, which
	// returned ErrTenantMissing, which the switch below folded into the default
	// branch — so the absence of an identity header reported itself as
	// "store_unavailable", a 503 blaming the database for a request that was
	// never scoped. The read is tenant-scoped through the parent obligation
	// lookup inside the store; this only makes the missing scope say so.
	if _, ok := h.requireTenant(w, r); !ok {
		return
	}
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
			h.writeStoreErr(w, "ListApplicabilityDecisions: store unavailable", err)
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

	// Same reason as ListApplicabilityDecisions: a missing tenant header was
	// reporting itself as a 503.
	if _, ok := h.requireTenant(w, r); !ok {
		return
	}
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
			h.writeStoreErr(w, "GetCurrentApplicability: store unavailable", err)
		}
		return
	}
	writeJSON(w, http.StatusOK, result)
}
