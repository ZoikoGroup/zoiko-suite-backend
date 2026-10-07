package handler

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"go.uber.org/zap"

	"zoiko.io/authorization-svc/internal/domain"
)

// GOV-04 compensating-control exceptions (000027) and ListConflictingPermissions.
//
// Permissions follow the Authorization Standard Appendix A IAM family: the
// rule administrator (iam.sod_rule.manage) requests and revokes; approving a
// compensating control — "restricted and independently reviewed" (GOV-04) —
// is iam.sod_rule.publish; reading the register is iam.sod_rule.read.
const (
	PermissionSoDRuleManage  = "iam.sod_rule.manage"
	PermissionSoDRulePublish = "iam.sod_rule.publish"
	PermissionSoDRuleRead    = "iam.sod_rule.read"
)

// sodExceptionStore is the store capability behind the exception lifecycle.
type sodExceptionStore interface {
	CreateSoDException(ctx context.Context, p domain.CreateSoDExceptionParams) (*domain.SoDException, error)
	FindSoDException(ctx context.Context, id, tenantID string) (*domain.SoDException, error)
	ListSoDExceptions(ctx context.Context, tenantID, principalID, status string) ([]domain.SoDException, error)
	TransitionSoDException(ctx context.Context, id, tenantID, from, to, actor string) (*domain.SoDException, error)
}

// sodExceptionFinder is the evaluation read: the exception, if any, that lets
// a principal hold this pair right now.
type sodExceptionFinder interface {
	FindActiveSoDException(ctx context.Context, principalID, tenantID, actionA, actionB string) (*domain.SoDException, error)
}

type createSoDExceptionRequest struct {
	SoDRuleID           string     `json:"sod_rule_id"`
	PrincipalID         string     `json:"principal_id"`
	CompensatingControl string     `json:"compensating_control"`
	Reason              string     `json:"reason"`
	EffectiveFrom       *time.Time `json:"effective_from,omitempty"`
	ExpiresAt           *time.Time `json:"expires_at"`
}

func (h *Handler) sodExceptions(w http.ResponseWriter) (sodExceptionStore, bool) {
	s, ok := h.store.(sodExceptionStore)
	if !ok {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "store_unavailable"})
	}
	return s, ok
}

// RequestSoDException handles POST /v1/admin/sod-exceptions — GOV-04
// "compensating-control reference". REQUESTED until independently approved;
// expires_at is mandatory ("exceptions ... always expire").
//
// Response: 201 / 400 / 401 / 403 / 404 rule / 503.
func (h *Handler) RequestSoDException(w http.ResponseWriter, r *http.Request) {
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	tenantScope, ok := h.requireTenant(w, r)
	if !ok {
		return
	}
	if !h.requirePermission(w, r, principalID, tenantScope, PermissionSoDRuleManage) {
		return
	}
	var req createSoDExceptionRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid_json", "message": err.Error()})
		return
	}
	for field, v := range map[string]string{"sod_rule_id": req.SoDRuleID, "principal_id": req.PrincipalID,
		"compensating_control": req.CompensatingControl, "reason": req.Reason} {
		if strings.TrimSpace(v) == "" {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "missing_field", "field": field})
			return
		}
	}
	if !validScope(req.SoDRuleID) {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid_field", "field": "sod_rule_id", "message": "sod_rule_id must be a UUID"})
		return
	}
	if req.ExpiresAt == nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "missing_field", "field": "expires_at",
			"message": "an SoD exception must expire (GOV-04)"})
		return
	}
	from := time.Now()
	if req.EffectiveFrom != nil {
		from = *req.EffectiveFrom
	}
	if !req.ExpiresAt.After(from) || !req.ExpiresAt.After(time.Now()) {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid_expires_at", "message": "expires_at must be in the future and after effective_from"})
		return
	}
	store, ok := h.sodExceptions(w)
	if !ok {
		return
	}
	e, err := store.CreateSoDException(r.Context(), domain.CreateSoDExceptionParams{
		TenantID: tenantScope, SoDRuleID: req.SoDRuleID, PrincipalID: strings.TrimSpace(req.PrincipalID),
		CompensatingControl: strings.TrimSpace(req.CompensatingControl), Reason: strings.TrimSpace(req.Reason),
		RequestedBy: principalID, EffectiveFrom: req.EffectiveFrom, ExpiresAt: *req.ExpiresAt,
	})
	if errors.Is(err, domain.ErrSoDRuleNotFound) {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "sod_rule_not_found"})
		return
	}
	if err != nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "store_unavailable"})
		return
	}
	writeJSON(w, http.StatusCreated, e)
}

// ListSoDExceptions handles GET /v1/admin/sod-exceptions?principal_id=&status=.
func (h *Handler) ListSoDExceptions(w http.ResponseWriter, r *http.Request) {
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	tenantScope, ok := h.requireTenant(w, r)
	if !ok {
		return
	}
	if !h.requirePermission(w, r, principalID, tenantScope, PermissionSoDRuleRead) {
		return
	}
	store, ok := h.sodExceptions(w)
	if !ok {
		return
	}
	q := r.URL.Query()
	list, err := store.ListSoDExceptions(r.Context(), tenantScope, strings.TrimSpace(q.Get("principal_id")), strings.ToUpper(strings.TrimSpace(q.Get("status"))))
	if err != nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "store_unavailable"})
		return
	}
	writeJSON(w, http.StatusOK, list)
}

// ApproveSoDException / RejectSoDException handle
// POST /v1/admin/sod-exceptions/{id}/approve|reject. RevokeSoDException
// handles .../revoke.
func (h *Handler) ApproveSoDException(w http.ResponseWriter, r *http.Request) {
	h.transitionSoDException(w, r, domain.SoDExceptionRequested, domain.SoDExceptionApproved, PermissionSoDRulePublish)
}

func (h *Handler) RejectSoDException(w http.ResponseWriter, r *http.Request) {
	h.transitionSoDException(w, r, domain.SoDExceptionRequested, domain.SoDExceptionRejected, PermissionSoDRulePublish)
}

func (h *Handler) RevokeSoDException(w http.ResponseWriter, r *http.Request) {
	h.transitionSoDException(w, r, domain.SoDExceptionApproved, domain.SoDExceptionRevoked, PermissionSoDRuleManage)
}

// transitionSoDException: "exceptions cannot be self-approved" — the decider
// of a request is neither its requester nor its subject.
//
// Response: 200 / 400 / 401 / 403 / 404 / 409 wrong state / 503.
func (h *Handler) transitionSoDException(w http.ResponseWriter, r *http.Request, from, to, permission string) {
	id := chi.URLParam(r, "sod_exception_id")
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	tenantScope, ok := h.requireTenant(w, r)
	if !ok {
		return
	}
	if !h.requirePermission(w, r, principalID, tenantScope, permission) {
		return
	}
	if _, ok := h.readCommand(w, &r); !ok {
		return
	}
	store, ok := h.sodExceptions(w)
	if !ok {
		return
	}
	if !validScope(id) {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "sod_exception_not_found"})
		return
	}
	existing, err := store.FindSoDException(r.Context(), id, tenantScope)
	if errors.Is(err, domain.ErrSoDExceptionNotFound) {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "sod_exception_not_found"})
		return
	}
	if err != nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "store_unavailable"})
		return
	}
	if from == domain.SoDExceptionRequested && (principalID == existing.RequestedBy || principalID == existing.PrincipalID) {
		writeJSON(w, http.StatusForbidden, map[string]string{
			"error":   "checker_not_independent",
			"message": "an SoD exception cannot be decided by its requester or its subject",
		})
		return
	}
	e, err := store.TransitionSoDException(r.Context(), id, tenantScope, from, to, principalID)
	switch {
	case errors.Is(err, domain.ErrSoDExceptionState):
		writeJSON(w, http.StatusConflict, map[string]string{"error": "invalid_state", "status": existing.Status})
		return
	case errors.Is(err, domain.ErrSoDExceptionNotFound):
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "sod_exception_not_found"})
		return
	case err != nil:
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "store_unavailable"})
		return
	}
	h.log.Info("sod exception transitioned", zap.String("sod_exception_id", id), zap.String("from", from), zap.String("to", to),
		zap.String("actor", principalID), zap.String("correlation_id", r.Header.Get("X-Correlation-ID")))
	writeJSON(w, http.StatusOK, e)
}

// ListConflictingPermissions handles GET /v1/sod/conflicting-permissions?action_type=X
// — GOV-04's query: the actions that conflict with X under the SoD rules in
// force for the caller's tenant (its own and platform-wide).
func (h *Handler) ListConflictingPermissions(w http.ResponseWriter, r *http.Request) {
	if _, ok := h.requirePrincipal(w, r); !ok {
		return
	}
	tenantScope, ok := h.requireTenant(w, r)
	if !ok {
		return
	}
	action := strings.TrimSpace(r.URL.Query().Get("action_type"))
	if action == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "missing_field", "field": "action_type"})
		return
	}
	rules, err := h.store.ListSoDRules(r.Context(), tenantScope)
	if err != nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "store_unavailable"})
		return
	}
	type conflict struct {
		ConflictsWith string `json:"conflicts_with"`
		SoDRuleID     string `json:"sod_rule_id"`
		ConflictType  string `json:"conflict_type"`
		PlatformWide  bool   `json:"platform_wide"`
	}
	out := []conflict{}
	for _, rule := range rules {
		if !rule.ActiveFlag {
			continue
		}
		other := ""
		switch action {
		case rule.ActionA:
			other = rule.ActionB
		case rule.ActionB:
			other = rule.ActionA
		default:
			continue
		}
		out = append(out, conflict{ConflictsWith: other, SoDRuleID: rule.SoDRuleID, ConflictType: rule.ConflictType, PlatformWide: rule.TenantID == nil})
	}
	writeJSON(w, http.StatusOK, map[string]any{"action_type": action, "conflicts": out})
}
