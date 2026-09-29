package handler

import (
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"go.uber.org/zap"

	"zoiko.io/configuration-feature-flag-svc/internal/domain"
	"zoiko.io/configuration-feature-flag-svc/internal/telemetry"
)

// ── target-scoped authorization ──────────────────────────────────────────────

// authorizeTarget authorizes a route that acts on an existing change or
// emergency change by that target's scope, not the caller's. A global target
// (tenant_id NULL) needs the GLOBAL grant; a tenant target the tenant grant.
//
// These routes used to authorize CONFIGURATION_WRITE unconditionally, and the
// store refused only a *foreign* tenant's target — a global one passed. So a
// tenant-scoped writer could approve and activate a global change, or activate
// a global break-glass change: one organisation altering the configuration
// every other one reads. The same class as the 22 Sep global-write defect, on
// the routes that fix did not reach.
//
// Returns the action authorized (for the GovernedWrites series) and whether
// the caller may proceed; on false the response has been written.
func (h *Handler) authorizeTarget(w http.ResponseWriter, r *http.Request, principalID, callerTenant, kind, id string) (string, bool) {
	tenantID, err := h.store.TargetScope(r.Context(), kind, id)
	if err != nil {
		if errors.Is(err, domain.ErrChangeNotFound) {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "change_not_found", "id": id})
			return "", false
		}
		h.governedRefusal(w, "", "authorizeTarget", r.Header.Get("X-Correlation-ID"), err)
		return "", false
	}
	if tenantID != nil && *tenantID != callerTenant {
		writeJSON(w, http.StatusForbidden, map[string]string{
			"error":   "tenant_scope_mismatch",
			"message": "the target belongs to another tenant",
		})
		return "", false
	}
	action := governedAction("config", tenantID)
	if outcome := h.authorizeGoverned(w, r, principalID, action); outcome != "" {
		h.metrics.GovernedWrites.WithLabelValues(action, outcome).Inc()
		return action, false
	}
	return action, true
}

// ── POST /v1/flags/{key}/kill-switch ─────────────────────────────────────────

type killSwitchRequest struct {
	Environment  string    `json:"environment"`
	TenantID     *string   `json:"tenant_id,omitempty"`
	Reason       string    `json:"reason"`
	IncidentID   *string   `json:"incident_id,omitempty"`
	SafeBehavior string    `json:"safe_behavior"`
	ExpiresAt    time.Time `json:"expires_at"`
}

func (req killSwitchRequest) missingField() string {
	switch {
	case req.Environment == "":
		return "environment"
	case strings.TrimSpace(req.Reason) == "":
		return "reason"
	case req.SafeBehavior == "":
		return "safe_behavior"
	case req.ExpiresAt.IsZero():
		return "expires_at"
	default:
		return ""
	}
}

// ActivateKillSwitch forces a declared feature flag into one of its predefined
// safe behaviours (INV-16): DISABLE turns it off, DEGRADE_ONLY collapses its
// rollout to zero. Nothing else — a kill switch cannot set an arbitrary value.
// It is time-boxed (expires_at is mandatory and in the future), takes effect
// in the same transaction through a snapshot mint, and the expiry sweep lifts
// it the same way. The scope decides the grant like every flag write.
//
// Response:
//
//	201 → the kill switch
//	400 → missing_field / value_constraint_failed (behaviour, past expiry)
//	403 → scope_not_allowed / authorization_denied / not_a_feature_flag
//	409 → an active switch already covers this scope
//	503 → store or authz unavailable
func (h *Handler) ActivateKillSwitch(w http.ResponseWriter, r *http.Request) {
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
	var req killSwitchRequest
	if !h.decodeJSON(w, r, &req, nil) {
		return
	}
	if missing := req.missingField(); missing != "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "missing_field", "field": missing})
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

	ks, err := h.store.CreateKillSwitch(r.Context(), domain.CreateKillSwitchParams{
		FlagKey:          key,
		Environment:      req.Environment,
		TenantID:         req.TenantID,
		Reason:           req.Reason,
		IncidentID:       req.IncidentID,
		SafeBehavior:     strings.ToUpper(req.SafeBehavior),
		ExpiresAt:        req.ExpiresAt,
		CallerTenantID:   tenantScope,
		ActorPrincipalID: principalID,
		CorrelationID:    correlationID,
	})
	if err != nil {
		h.governedRefusal(w, action, "ActivateKillSwitch", correlationID, err)
		return
	}
	h.metrics.GovernedWrites.WithLabelValues(action, telemetry.WriteCreated).Inc()
	h.log.Info("kill switch activated",
		zap.String("flag_key", key),
		zap.String("kill_switch_id", ks.KillSwitchID),
		zap.String("safe_behavior", ks.SafeBehavior),
		zap.String("correlation_id", correlationID),
	)
	writeJSON(w, http.StatusCreated, ks)
}

// ── POST /v1/config/changes/{change_id}/rollback ─────────────────────────────

// RollbackChange proposes the change that restores a VERIFIED change's
// before-state (INV-17), computed from the target's immutable before
// snapshot. The result is a PROPOSED change of the target's class and approval
// requirement; it is approved and activated like any other, and activation
// marks the target ROLLED_BACK. Authorized by the target's scope.
//
// Response:
//
//	201 → the proposed rollback change
//	403 → tenant_scope_mismatch / authorization_denied
//	404 → change_not_found
//	409 → rollback_target_invalid (not VERIFIED, or already rolled back)
//	503 → store or authz unavailable
func (h *Handler) RollbackChange(w http.ResponseWriter, r *http.Request) {
	correlationID := r.Header.Get("X-Correlation-ID")
	changeID := chi.URLParam(r, "change_id")

	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	tenantScope, ok := h.requireTenant(w, r)
	if !ok {
		return
	}
	action, ok := h.authorizeTarget(w, r, principalID, tenantScope, "change", changeID)
	if !ok {
		return
	}

	proposed, err := h.store.RollbackChange(r.Context(), changeID, tenantScope, principalID, correlationID)
	if err != nil {
		h.governedRefusal(w, action, "RollbackChange", correlationID, err)
		return
	}
	h.metrics.GovernedWrites.WithLabelValues(action, telemetry.WriteCreated).Inc()
	h.log.Info("rollback proposed",
		zap.String("rollback_of", changeID),
		zap.String("change_id", proposed.ChangeID),
		zap.String("correlation_id", correlationID),
	)
	writeJSON(w, http.StatusCreated, proposed)
}

// ── POST /v1/emergency-changes/{emergency_change_id}/retrospective ───────────

type retrospectiveRequest struct {
	Reference string `json:"reference"`
}

// CloseEmergencyRetrospective records the mandatory retrospective review of an
// expired break-glass change (INV-15) and closes it. reference names the
// review record in the incident/WFC process; it is required.
//
// Response:
//
//	200 → the closed emergency change
//	400 → missing_field reference
//	403 → tenant_scope_mismatch / authorization_denied
//	404 → change_not_found
//	409 → retrospective_not_pending (not yet expired, or already closed)
//	503 → store or authz unavailable
func (h *Handler) CloseEmergencyRetrospective(w http.ResponseWriter, r *http.Request) {
	correlationID := r.Header.Get("X-Correlation-ID")
	id := chi.URLParam(r, "emergency_change_id")

	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	tenantScope, ok := h.requireTenant(w, r)
	if !ok {
		return
	}
	var req retrospectiveRequest
	if !h.decodeJSON(w, r, &req, nil) {
		return
	}
	if strings.TrimSpace(req.Reference) == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "missing_field", "field": "reference"})
		return
	}
	action, ok := h.authorizeTarget(w, r, principalID, tenantScope, "emergency", id)
	if !ok {
		return
	}

	closed, err := h.store.CloseEmergencyRetrospective(r.Context(), id, req.Reference, tenantScope, principalID)
	if err != nil {
		h.governedRefusal(w, action, "CloseEmergencyRetrospective", correlationID, err)
		return
	}
	h.metrics.GovernedWrites.WithLabelValues(action, telemetry.WriteCreated).Inc()
	writeJSON(w, http.StatusOK, closed)
}
