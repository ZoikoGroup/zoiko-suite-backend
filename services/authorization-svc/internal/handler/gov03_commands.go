package handler

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"go.uber.org/zap"

	"zoiko.io/authorization-svc/internal/domain"
	"zoiko.io/authorization-svc/internal/events"
)

// The two GOV-03 administrative commands, which had no implementation:
// InvalidateAuthorizationCache and RecomputeSubjectEffectiveAccess.

// PermissionPolicyPublish is the Appendix A permission for publishing
// authorization policy; forcing every replica to re-read it is that act.
const PermissionPolicyPublish = "iam.policy.publish"

// tenantInvalidator is the cache capability (cache.Store) behind the commands.
type tenantInvalidator interface {
	InvalidateTenant(tenantID string)
}

// eventEmitter writes one event to the outbox.
type eventEmitter interface {
	EmitEvent(ctx context.Context, m domain.OutboxMessage) error
}

type invalidateCacheRequest struct {
	PrincipalID string `json:"principal_id,omitempty"`
	commandFields
}

// InvalidateAuthorizationCache handles POST /v1/admin/authorization-cache/invalidate
// (and GOV-03's illustrative /internal/v1/gov03/commands/invalidateAuthorizationCache).
//
// Drops this replica's cached reads for the caller's tenant and emits
// authorization.cache.invalidated (GOV-03 "AuthorizationCacheInvalidated")
// through the outbox, which every replica's invalidator consumes. The scope is
// the caller's verified tenant; principal_id is recorded as the subject of the
// invalidation, the tenant being the unit the cache can drop.
//
// Response: 200 / 400 / 401 / 403 / 503.
func (h *Handler) InvalidateAuthorizationCache(w http.ResponseWriter, r *http.Request) {
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	tenantScope, ok := h.requireTenant(w, r)
	if !ok {
		return
	}
	if !h.requirePermission(w, r, principalID, tenantScope, PermissionPolicyPublish) {
		return
	}
	var req invalidateCacheRequest
	if r.Body != nil {
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil && !errors.Is(err, io.EOF) {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid_json", "message": err.Error()})
			return
		}
	}
	if !h.applyReason(w, &r, req.reason()) {
		return
	}

	emitter, ok := h.store.(eventEmitter)
	if !ok {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "store_unavailable"})
		return
	}
	msg, err := events.NewEvent("authorization.cache.invalidated", r.Header.Get("X-Correlation-ID"), tenantScope, principalID,
		tenantScope, map[string]any{
			"tenant_id":      tenantScope,
			"principal_id":   strings.TrimSpace(req.PrincipalID),
			"invalidated_by": principalID,
			"reason":         req.reason(),
			"invalidated_at": time.Now().UTC(),
		})
	if err == nil {
		err = emitter.EmitEvent(r.Context(), msg)
	}
	if err != nil {
		h.log.Error("InvalidateAuthorizationCache: could not announce the invalidation", zap.Error(err))
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "store_unavailable"})
		return
	}
	if c, ok := h.store.(tenantInvalidator); ok {
		c.InvalidateTenant(tenantScope)
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"invalidated": true, "tenant_id": tenantScope, "principal_id": strings.TrimSpace(req.PrincipalID),
		"announced": "authorization.cache.invalidated",
	})
}

// effectiveAssignment is one assignment and what it confers now.
type effectiveAssignment struct {
	domain.PrincipalRoleAssignment
	Granting bool     `json:"granting"`
	Actions  []string `json:"actions"`
}

type effectiveAccessResponse struct {
	PrincipalID      string                `json:"principal_id"`
	TenantID         string                `json:"tenant_id"`
	EffectiveActions []string              `json:"effective_actions"`
	Assignments      []effectiveAssignment `json:"assignments"`
	ComputedAt       time.Time             `json:"computed_at"`
}

// RecomputeSubjectEffectiveAccess handles
// POST /v1/admin/subjects/{principal_id}/effective-access/recompute (and GOV-03's
// illustrative /internal/v1/gov03/commands/recomputeSubjectEffectiveAccess with
// principal_id in the body).
//
// Drops cached reads for the tenant and recomputes, from the store, what the
// subject holds: every assignment with whether it grants now (APPROVED, in its
// period, role active) and the actions its active bundles confer, and the
// union. Reading somebody's grant map is iam.assignment.read, as elsewhere.
//
// Response: 200 / 400 / 401 / 403 / 503.
func (h *Handler) RecomputeSubjectEffectiveAccess(w http.ResponseWriter, r *http.Request) {
	callerPrincipal, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	tenantScope, ok := h.requireTenant(w, r)
	if !ok {
		return
	}
	subject := chi.URLParam(r, "principal_id")
	if subject == "" {
		var body struct {
			PrincipalID string `json:"principal_id"`
		}
		if r.Body != nil {
			_ = json.NewDecoder(r.Body).Decode(&body)
		}
		subject = strings.TrimSpace(body.PrincipalID)
	}
	if subject == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "missing_field", "field": "principal_id"})
		return
	}
	if subject != callerPrincipal && !h.requirePermission(w, r, callerPrincipal, tenantScope, "iam.assignment.read") {
		return
	}
	if c, ok := h.store.(tenantInvalidator); ok {
		c.InvalidateTenant(tenantScope)
	}

	assignments, err := h.store.ListRoleAssignments(r.Context(), tenantScope, subject, "", false)
	if err != nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "store_unavailable"})
		return
	}
	roles := map[string]*domain.Role{}
	now := time.Now()
	resp := effectiveAccessResponse{PrincipalID: subject, TenantID: tenantScope, EffectiveActions: []string{},
		Assignments: []effectiveAssignment{}, ComputedAt: now.UTC()}
	var union []string
	for _, a := range assignments {
		actions, err := h.roleActiveActions(r.Context(), a.RoleID, tenantScope)
		if err != nil {
			writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "store_unavailable"})
			return
		}
		role, seen := roles[a.RoleID]
		if !seen {
			role, err = h.store.FindRoleByID(r.Context(), a.RoleID)
			if err != nil && !errors.Is(err, domain.ErrRoleNotFound) {
				writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "store_unavailable"})
				return
			}
			roles[a.RoleID] = role
		}
		granting := (a.ApprovalStatus == "" || a.ApprovalStatus == domain.ApprovalApproved) &&
			!a.EffectiveFrom.After(now) && (a.EffectiveTo == nil || a.EffectiveTo.After(now)) &&
			role != nil && role.ActiveFlag
		if granting {
			union = append(union, actions...)
		}
		resp.Assignments = append(resp.Assignments, effectiveAssignment{PrincipalRoleAssignment: a, Granting: granting, Actions: nonNil(actions)})
	}
	if u := dedupeSorted(union); len(u) > 0 {
		resp.EffectiveActions = u
	}
	writeJSON(w, http.StatusOK, resp)
}
