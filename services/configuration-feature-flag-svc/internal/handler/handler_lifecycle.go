package handler

import (
	"encoding/json"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"
	"go.uber.org/zap"

	"zoiko.io/configuration-feature-flag-svc/internal/domain"
	"zoiko.io/configuration-feature-flag-svc/internal/store"
	"zoiko.io/configuration-feature-flag-svc/internal/telemetry"
)

// HeaderCommercialPlan carries the caller's commercial plan as established by
// the gateway from the commercial account (COM). It is the only source plan
// eligibility reads (NP-08): a plan in the request body is a claim by the
// client, not a fact about it. The gateway must set it and strip any
// client-supplied value.
const HeaderCommercialPlan = "X-Commercial-Plan"

// HeaderOrgUnit carries the caller's organizational unit, established by the
// gateway from identity context. The ORG_UNIT precedence layer (INV-07) reads
// it; like every trusted header, the gateway must strip a client-sent value.
const HeaderOrgUnit = "X-Org-Unit-Id"

// trustedRegion is the caller's gateway-verified jurisdiction, used for
// residency (INV-26). The envelope carries it as X-Jurisdiction-Context.
func trustedRegion(r *http.Request) string {
	return strings.TrimSpace(r.Header.Get("X-Jurisdiction-Context"))
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if s := strings.TrimSpace(v); s != "" {
			return s
		}
	}
	return ""
}

// ── POST /v1/flags/{key}/retire ──────────────────────────────────────────────

type retireFlagRequest struct {
	Environment          string          `json:"environment"`
	TenantID             *string         `json:"tenant_id,omitempty"`
	FinalEnabled         *bool           `json:"final_enabled"`
	FinalRollout         *int            `json:"final_rollout,omitempty"`
	ConsumerScanEvidence json.RawMessage `json:"consumer_scan_evidence,omitempty"`
}

// RetireFlag moves a flag to RETIRED (INV-21): it is pinned to its declared
// final value and tombstoned, so the key cannot be written — or reused for
// other semantics (NP-20) — until its removal is verified. RETIRED is not
// REMOVED; evaluation keeps answering the final value.
//
// Response:
//
//	201 → the retirement record
//	400 → missing_field
//	403 → scope_not_allowed / authorization_denied
//	409 → flag_key_retired (already retired)
//	503 → store or authz unavailable
func (h *Handler) RetireFlag(w http.ResponseWriter, r *http.Request) {
	correlationID := r.Header.Get("X-Correlation-ID")
	key := chi.URLParam(r, "key")

	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	tenantScope, ok := h.requireTenant(w, r)
	if !ok {
		return
	}
	var req retireFlagRequest
	if !h.decodeJSON(w, r, &req, nil) {
		return
	}
	switch {
	case req.Environment == "":
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "missing_field", "field": "environment"})
		return
	case req.FinalEnabled == nil:
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "missing_field", "field": "final_enabled"})
		return
	}
	if h.refuseForeignTenant(w, req.TenantID, tenantScope) {
		return
	}
	action := governedAction("flag", req.TenantID)
	if outcome := h.authorizeGoverned(w, r, principalID, action); outcome != "" {
		h.metrics.GovernedWrites.WithLabelValues(action, outcome).Inc()
		return
	}
	rollout := 100
	if req.FinalRollout != nil {
		rollout = *req.FinalRollout
	}
	retired, err := h.store.RetireFlag(r.Context(), domain.RetireFlagParams{
		Key: key, Environment: req.Environment, TenantID: req.TenantID,
		FinalEnabled: *req.FinalEnabled, FinalRollout: rollout,
		ConsumerScanEvidence: req.ConsumerScanEvidence,
		CallerTenantID:       tenantScope, ActorPrincipalID: principalID, CorrelationID: correlationID,
	})
	if err != nil {
		h.governedRefusal(w, action, "RetireFlag", correlationID, err)
		return
	}
	h.metrics.GovernedWrites.WithLabelValues(action, telemetry.WriteCreated).Inc()
	h.log.Info("flag retired", zap.String("flag_key", key), zap.String("correlation_id", correlationID))
	writeJSON(w, http.StatusCreated, retired)
}

// ── POST /v1/flags/{key}/removal ─────────────────────────────────────────────

type flagRemovalRequest struct {
	Environment          string          `json:"environment"`
	ConsumerScanEvidence json.RawMessage `json:"consumer_scan_evidence"`
}

// MarkFlagRemoved records that a RETIRED flag has no remaining consumers and
// frees the key (INV-21: removal requires consumer/code dependency
// verification). The consumer-scan evidence is required. Removal releases a
// key platform-wide, so it takes the global flag grant.
//
// Response:
//
//	200 → the retirement record, now reusable
//	400 → missing_field / value_constraint_failed (no evidence)
//	403 → authorization_denied
//	409 → flag_not_retired
//	503 → store or authz unavailable
func (h *Handler) MarkFlagRemoved(w http.ResponseWriter, r *http.Request) {
	correlationID := r.Header.Get("X-Correlation-ID")
	key := chi.URLParam(r, "key")

	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	tenantScope, ok := h.requireTenant(w, r)
	if !ok {
		return
	}
	var req flagRemovalRequest
	if !h.decodeJSON(w, r, &req, nil) {
		return
	}
	if req.Environment == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "missing_field", "field": "environment"})
		return
	}
	if len(req.ConsumerScanEvidence) == 0 {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "missing_field", "field": "consumer_scan_evidence"})
		return
	}
	action := telemetry.ActionFlagGlobalWrite
	if outcome := h.authorizeGoverned(w, r, principalID, action); outcome != "" {
		h.metrics.GovernedWrites.WithLabelValues(action, outcome).Inc()
		return
	}
	removed, err := h.store.MarkFlagRemoved(r.Context(), store.MarkFlagRemovedParams{
		Key: key, Environment: req.Environment, ConsumerScanEvidence: req.ConsumerScanEvidence,
		CallerTenantID: tenantScope, ActorPrincipalID: principalID,
	})
	if err != nil {
		h.governedRefusal(w, action, "MarkFlagRemoved", correlationID, err)
		return
	}
	h.metrics.GovernedWrites.WithLabelValues(action, telemetry.WriteCreated).Inc()
	writeJSON(w, http.StatusOK, removed)
}
