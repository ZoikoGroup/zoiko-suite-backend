package handler

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"go.uber.org/zap"

	"zoiko.io/org-structure-svc/internal/domain"
	svcmiddleware "zoiko.io/org-structure-svc/internal/middleware"
)

// ORG-05 org-unit commands and queries (ZS-SVC-I-001 section 4.5).

const (
	actionOrgUnitCreate     = "ORG_UNIT_CREATE"
	actionOrgUnitView       = "ORG_UNIT_VIEW"
	actionOrgUnitActivate   = "ORG_UNIT_ACTIVATE"
	actionOrgUnitDeactivate = "ORG_UNIT_DEACTIVATE"
	actionOrgUnitReparent   = "ORG_UNIT_REPARENT"
	actionOrgUnitChangeMgr  = "ORG_UNIT_CHANGE_MANAGER"
	actionOrgUnitApprove    = "ORG_UNIT_APPROVE_STRUCTURE"
)

func registerOrgUnitRoutes(r chi.Router, h *Handler) {
	r.Post("/units", h.CreateOrgUnit)
	r.Get("/units", h.ListOrgUnits)
	r.Get("/units/{id}", h.GetOrgUnit)
	r.Post("/units/{id}/activate", h.ActivateOrgUnit)
	r.Post("/units/{id}/deactivate", h.DeactivateOrgUnit)
	r.Post("/units/{id}/reparent", h.ReparentOrgUnit)
	r.Post("/units/{id}/change-manager", h.ChangeOrgUnitManager)
	r.Get("/hierarchy", h.GetHierarchy)
	r.Get("/units/{id}/reparent-impact", h.GetReparentImpact)
	r.Get("/change-requests", h.ListChangeRequests)
	r.Get("/change-requests/{id}", h.GetChangeRequest)
	r.Post("/change-requests/{id}/approve", h.ApproveChangeRequest)
	r.Post("/change-requests/{id}/reject", h.RejectChangeRequest)
}

func validDate(s string) bool {
	_, err := time.Parse("2006-01-02", s)
	return err == nil
}

func (h *Handler) CreateOrgUnit(w http.ResponseWriter, r *http.Request) {
	var req domain.CreateOrgUnitRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_json", err.Error())
		return
	}
	if req.LegalEntityID == "" || req.Code == "" || req.Name == "" {
		writeError(w, http.StatusBadRequest, "missing_fields", "legal_entity_id, code, name are required")
		return
	}
	if !domain.IsValidUnitType(req.UnitType) {
		writeError(w, http.StatusBadRequest, "invalid_unit_type", "unit_type must be one of DEPARTMENT, BUSINESS_UNIT, FUNCTION, DIVISION, TEAM")
		return
	}
	if !validDate(req.EffectiveFrom) {
		writeError(w, http.StatusBadRequest, "invalid_effective_from", "effective_from must be a date, YYYY-MM-DD")
		return
	}
	if req.CorrelationID == "" {
		writeError(w, http.StatusBadRequest, "missing_fields", "correlation_id is required")
		return
	}

	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	if err := h.authz.CheckAllowed(r.Context(), principalID, req.LegalEntityID, actionOrgUnitCreate); err != nil {
		h.writeAuthzErr(w, err)
		return
	}
	if req.ManagerEmployeeID != nil {
		if !h.resolveManager(w, r, principalID, *req.ManagerEmployeeID) {
			return
		}
	}

	unit, created, err := h.store.CreateOrgUnit(r.Context(), &req, principalID)
	if err != nil {
		h.writeOrgUnitErr(w, err, "create")
		return
	}
	if !created {
		writeJSON(w, http.StatusOK, unit)
		return
	}

	h.publisher.PublishOrgStructureChanged(r.Context(), getCorrelationID(r), unit.TenantID, unit.LegalEntityID, principalID, "ORG_UNIT_CREATED", unit.UnitID)
	writeJSON(w, http.StatusCreated, unit)
}

func (h *Handler) GetOrgUnit(w http.ResponseWriter, r *http.Request) {
	asOf := r.URL.Query().Get("as_of")
	if asOf != "" && !validDate(asOf) {
		writeError(w, http.StatusBadRequest, "invalid_as_of", "as_of must be a date, YYYY-MM-DD")
		return
	}
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}

	unit, err := h.store.GetOrgUnit(r.Context(), chi.URLParam(r, "id"), asOf)
	if err != nil {
		h.writeOrgUnitErr(w, err, "get")
		return
	}
	if err := h.authz.CheckAllowed(r.Context(), principalID, unit.LegalEntityID, actionOrgUnitView); err != nil {
		h.writeAuthzErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, unit)
}

func (h *Handler) ListOrgUnits(w http.ResponseWriter, r *http.Request) {
	legalEntityID := r.URL.Query().Get("legal_entity_id")
	asOf := r.URL.Query().Get("as_of")
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	if asOf != "" && !validDate(asOf) {
		writeError(w, http.StatusBadRequest, "invalid_as_of", "as_of must be a date, YYYY-MM-DD")
		return
	}
	if managerID := r.URL.Query().Get("manager_employee_id"); managerID != "" {
		h.findUnitsByManager(w, r, principalID, managerID, legalEntityID, asOf)
		return
	}
	if legalEntityID == "" {
		writeError(w, http.StatusBadRequest, "missing_fields", "legal_entity_id is required")
		return
	}
	if err := h.authz.CheckAllowed(r.Context(), principalID, legalEntityID, actionOrgUnitView); err != nil {
		h.writeAuthzErr(w, err)
		return
	}

	list, err := h.store.ListOrgUnits(r.Context(), legalEntityID, asOf)
	if err != nil {
		h.writeOrgUnitErr(w, err, "list")
		return
	}
	writeJSON(w, http.StatusOK, list)
}

func (h *Handler) ActivateOrgUnit(w http.ResponseWriter, r *http.Request) {
	h.transitionOrgUnit(w, r, domain.UnitCommandActivate, actionOrgUnitActivate, "ORG_UNIT_ACTIVATED")
}

func (h *Handler) DeactivateOrgUnit(w http.ResponseWriter, r *http.Request) {
	h.transitionOrgUnit(w, r, domain.UnitCommandDeactivate, actionOrgUnitDeactivate, "ORG_UNIT_DEACTIVATED")
}

// transitionOrgUnit runs a lifecycle command. If-Match carries the version the
// caller read; a mutation of an existing unit without it is refused (428).
func (h *Handler) transitionOrgUnit(w http.ResponseWriter, r *http.Request, command, action, eventType string) {
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	version, ok := parseIfMatch(w, r)
	if !ok {
		return
	}

	unitID := chi.URLParam(r, "id")
	current, err := h.store.GetOrgUnit(r.Context(), unitID, "")
	if err != nil {
		h.writeOrgUnitErr(w, err, "transition")
		return
	}
	if err := h.authz.CheckAllowed(r.Context(), principalID, current.LegalEntityID, action); err != nil {
		h.writeAuthzErr(w, err)
		return
	}

	unit, err := h.store.TransitionOrgUnit(r.Context(), unitID, command, version)
	if err != nil {
		h.writeOrgUnitErr(w, err, "transition")
		return
	}

	h.publisher.PublishOrgStructureChanged(r.Context(), getCorrelationID(r), unit.TenantID, unit.LegalEntityID, principalID, eventType, unit.UnitID)
	writeJSON(w, http.StatusOK, unit)
}

// parseIfMatch reads the expected record_version from If-Match, which may be
// quoted (ETag style) or bare.
func parseIfMatch(w http.ResponseWriter, r *http.Request) (int64, bool) {
	raw := strings.Trim(strings.TrimSpace(r.Header.Get("If-Match")), `"`)
	if raw == "" {
		writeError(w, http.StatusPreconditionRequired, "if_match_required", "If-Match with the unit's record_version is required")
		return 0, false
	}
	v, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || v < 1 {
		writeError(w, http.StatusBadRequest, "invalid_if_match", "If-Match must be the unit's record_version")
		return 0, false
	}
	return v, true
}

func (h *Handler) writeOrgUnitErr(w http.ResponseWriter, err error, op string) {
	switch {
	case errors.Is(err, domain.ErrOrgUnitNotFound):
		writeError(w, http.StatusNotFound, "org_unit_not_found", "")
	case errors.Is(err, domain.ErrOrgUnitCodeTaken):
		writeError(w, http.StatusConflict, "code_taken", err.Error())
	case errors.Is(err, domain.ErrParentInvalid):
		writeError(w, http.StatusUnprocessableEntity, "parent_invalid", err.Error())
	case errors.Is(err, domain.ErrParentNotActive):
		writeError(w, http.StatusConflict, "parent_not_active", err.Error())
	case errors.Is(err, domain.ErrInvalidTransition):
		writeError(w, http.StatusConflict, "invalid_transition", err.Error())
	case errors.Is(err, domain.ErrHasActiveChildren):
		writeError(w, http.StatusConflict, "has_active_children", err.Error())
	case errors.Is(err, domain.ErrHierarchyLoop):
		writeError(w, http.StatusUnprocessableEntity, "hierarchy_loop", err.Error())
	case errors.Is(err, domain.ErrManagerNotFound):
		writeError(w, http.StatusBadRequest, "manager_invalid", err.Error())
	case errors.Is(err, domain.ErrNoManagerChange):
		writeError(w, http.StatusConflict, "no_change", err.Error())
	case errors.Is(err, domain.ErrEmployeeValidationFailed):
		writeError(w, http.StatusServiceUnavailable, "employee_validation_failed", err.Error())
	case errors.Is(err, domain.ErrChangeRequestNotFound):
		writeError(w, http.StatusNotFound, "change_request_not_found", "")
	case errors.Is(err, domain.ErrChangeRequestDecided):
		writeError(w, http.StatusConflict, "change_request_decided", err.Error())
	case errors.Is(err, domain.ErrSelfApproval):
		writeError(w, http.StatusForbidden, "sod_denied", err.Error())
	case errors.Is(err, domain.ErrChangePending):
		writeError(w, http.StatusConflict, "change_pending", err.Error())
	case errors.Is(err, domain.ErrNoHierarchyChange):
		writeError(w, http.StatusConflict, "no_change", err.Error())
	case errors.Is(err, domain.ErrEffectiveDateInvalid):
		writeError(w, http.StatusUnprocessableEntity, "invalid_effective_date", err.Error())
	case errors.Is(err, domain.ErrVersionConflict):
		writeError(w, http.StatusConflict, "version_conflict", err.Error())
	case errors.Is(err, domain.ErrIdentityMissing):
		writeError(w, http.StatusUnauthorized, "identity_missing", err.Error())
	default:
		h.log.Error("org unit "+op+" failed", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store_unavailable", err.Error())
	}
}

// ReparentOrgUnit moves a unit under a new parent from effective_at on. The
// unit's history is kept: the old placement is closed, not edited.
func (h *Handler) ReparentOrgUnit(w http.ResponseWriter, r *http.Request) {
	var req domain.ReparentOrgUnitRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_json", err.Error())
		return
	}
	if !validDate(req.EffectiveAt) {
		writeError(w, http.StatusBadRequest, "invalid_effective_at", "effective_at must be a date, YYYY-MM-DD")
		return
	}
	if strings.TrimSpace(req.Reason) == "" {
		writeError(w, http.StatusBadRequest, "missing_fields", "reason is required: a structural change keeps its reason as evidence")
		return
	}

	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	version, ok := parseIfMatch(w, r)
	if !ok {
		return
	}

	unitID := chi.URLParam(r, "id")
	current, err := h.store.GetOrgUnit(r.Context(), unitID, "")
	if err != nil {
		h.writeOrgUnitErr(w, err, "reparent")
		return
	}
	if err := h.authz.CheckAllowed(r.Context(), principalID, current.LegalEntityID, actionOrgUnitReparent); err != nil {
		h.writeAuthzErr(w, err)
		return
	}

	// A move that carries financial mappings is not applied by its maker: it
	// becomes a change request that a different principal must approve.
	impact, err := h.store.ReparentImpact(r.Context(), unitID, req.EffectiveAt)
	if err != nil {
		h.writeOrgUnitErr(w, err, "reparent")
		return
	}
	if impact.RequiresApproval {
		h.proposeReparent(w, r, unitID, &req, version, principalID, *impact)
		return
	}

	unit, err := h.store.ReparentOrgUnit(r.Context(), unitID, &req, version, principalID)
	if err != nil {
		h.writeOrgUnitErr(w, err, "reparent")
		return
	}

	h.publisher.PublishOrgStructureChanged(r.Context(), getCorrelationID(r), unit.TenantID, unit.LegalEntityID, principalID, "ORG_UNIT_REPARENTED", unit.UnitID)
	writeJSON(w, http.StatusOK, unit)
}

// resolveManager confirms the manager is a real, current employee. The manager
// identity is resolved server-side and fails closed: if employee-master-svc
// cannot be reached, nothing is written.
func (h *Handler) resolveManager(w http.ResponseWriter, r *http.Request, principalID, managerID string) bool {
	if strings.TrimSpace(managerID) == "" {
		writeError(w, http.StatusBadRequest, "missing_fields", "manager_employee_id is required")
		return false
	}
	if h.employee == nil {
		writeError(w, http.StatusServiceUnavailable, "employee_validation_failed", domain.ErrEmployeeValidationFailed.Error())
		return false
	}
	_, err := h.employee.ValidateEmployee(r.Context(), svcmiddleware.TenantFromContext(r.Context()), principalID, managerID)
	if err == nil {
		return true
	}
	if errors.Is(err, domain.ErrEmployeeNotFound) {
		writeError(w, http.StatusBadRequest, "manager_invalid", string(domain.ErrManagerNotFound))
		return false
	}
	h.log.Error("manager validation failed", zap.Error(err))
	writeError(w, http.StatusServiceUnavailable, "employee_validation_failed", domain.ErrEmployeeValidationFailed.Error())
	return false
}

// ChangeOrgUnitManager names a new manager from effective_at on; the previous
// manager stays in history.
func (h *Handler) ChangeOrgUnitManager(w http.ResponseWriter, r *http.Request) {
	var req domain.ChangeManagerRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_json", err.Error())
		return
	}
	if strings.TrimSpace(req.ManagerEmployeeID) == "" {
		writeError(w, http.StatusBadRequest, "missing_fields", "manager_employee_id is required")
		return
	}
	if !validDate(req.EffectiveAt) {
		writeError(w, http.StatusBadRequest, "invalid_effective_at", "effective_at must be a date, YYYY-MM-DD")
		return
	}
	if strings.TrimSpace(req.Reason) == "" {
		writeError(w, http.StatusBadRequest, "missing_fields", "reason is required: a structural change keeps its reason as evidence")
		return
	}

	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	version, ok := parseIfMatch(w, r)
	if !ok {
		return
	}

	unitID := chi.URLParam(r, "id")
	current, err := h.store.GetOrgUnit(r.Context(), unitID, "")
	if err != nil {
		h.writeOrgUnitErr(w, err, "change manager")
		return
	}
	if err := h.authz.CheckAllowed(r.Context(), principalID, current.LegalEntityID, actionOrgUnitChangeMgr); err != nil {
		h.writeAuthzErr(w, err)
		return
	}
	// Only after authorization, so a caller without rights learns nothing
	// about which employee ids exist.
	if !h.resolveManager(w, r, principalID, req.ManagerEmployeeID) {
		return
	}

	unit, err := h.store.ChangeOrgUnitManager(r.Context(), unitID, &req, version, principalID)
	if err != nil {
		h.writeOrgUnitErr(w, err, "change manager")
		return
	}

	h.publisher.PublishOrgStructureChanged(r.Context(), getCorrelationID(r), unit.TenantID, unit.LegalEntityID, principalID, "ORG_UNIT_MANAGER_CHANGED", unit.UnitID)
	writeJSON(w, http.StatusOK, unit)
}

// findUnitsByManager lists the units an employee manages. Units can sit in
// several legal entities, so view access is checked per entity: entities the
// caller may not view are left out, and a caller who may view none of them
// gets 403 rather than an empty list that would hide the refusal.
func (h *Handler) findUnitsByManager(w http.ResponseWriter, r *http.Request, principalID, managerID, legalEntityID, asOf string) {
	units, err := h.store.FindUnitsByManager(r.Context(), managerID, asOf)
	if err != nil {
		h.writeOrgUnitErr(w, err, "find by manager")
		return
	}

	allowed := map[string]bool{}
	out := []domain.OrgUnit{}
	denied := false
	for _, u := range units {
		if legalEntityID != "" && u.LegalEntityID != legalEntityID {
			continue
		}
		ok, seen := allowed[u.LegalEntityID]
		if !seen {
			err := h.authz.CheckAllowed(r.Context(), principalID, u.LegalEntityID, actionOrgUnitView)
			switch {
			case err == nil:
				ok = true
			case errors.Is(err, domain.ErrAuthorizationDenied):
				denied = true
			default:
				h.writeAuthzErr(w, err)
				return
			}
			allowed[u.LegalEntityID] = ok
		}
		if ok {
			out = append(out, u)
		}
	}
	if len(out) == 0 && denied {
		writeError(w, http.StatusForbidden, "forbidden", string(domain.ErrAuthorizationDenied))
		return
	}
	writeJSON(w, http.StatusOK, out)
}
