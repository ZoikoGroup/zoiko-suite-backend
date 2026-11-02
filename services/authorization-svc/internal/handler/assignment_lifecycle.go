package handler

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"go.uber.org/zap"
	"zoiko.io/identity-envelope/apicontract"

	"zoiko.io/authorization-svc/internal/domain"
)

// The ORG-06 role half (IAM-13), on this service's role assignments:
//
//	AssignBusinessRole        POST /v1/admin/role-assignments
//	ListPrincipalAssignments  GET  /v1/admin/role-assignments?principal_id=
//	SuspendAssignment         POST /v1/admin/role-assignments/{id}/suspend, /resume
//	GetAssignmentsAsOf        GET  /v1/admin/role-assignments?as_of=<RFC 3339>
//
// The first two already existed. Suspension and the as-of read rest on
// migration 000038: a suspended_at the grant queries exclude, and an
// append-only history of every assignment state.

// assignmentSuspender is the store capability behind suspend / resume.
type assignmentSuspender interface {
	SuspendRoleAssignment(ctx context.Context, assignmentID, tenantID, suspendedBy, reason string) (*domain.PrincipalRoleAssignment, error)
	ResumeRoleAssignment(ctx context.Context, assignmentID, tenantID string) (*domain.PrincipalRoleAssignment, error)
}

// assignmentHistoryReader is the store capability behind ?as_of=.
type assignmentHistoryReader interface {
	FindRoleAssignmentsAsOf(ctx context.Context, tenantID, principalID string, asOf time.Time) ([]domain.PrincipalRoleAssignment, error)
}

// optionalExpectedVersion is expectedVersion for the aggregates whose callers
// predate API-3 — role assignments and workload bindings. The console and
// access-control-svc send no version on these yet, so none means
// unconditional (0) instead of 428; one that is sent is honoured exactly as on
// the other aggregates, and a stale one is refused.
func optionalExpectedVersion(w http.ResponseWriter, r *http.Request, bodyVersion int64) (int64, bool) {
	if _, _, present, _ := apicontract.IfMatchVersion(r); !present && bodyVersion == 0 {
		return 0, true
	}
	return expectedVersion(w, r, bodyVersion)
}

// assignmentForMutation reads the assignment a mutation acts on and checks
// the caller's expected version against it. Writes the 404 / 412 / 503.
func (h *Handler) assignmentForMutation(w http.ResponseWriter, r *http.Request, assignmentID, tenantScope string, expected int64) (*domain.PrincipalRoleAssignment, bool) {
	finder, ok := h.store.(roleAssignmentFinder)
	if !ok {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "store_unavailable"})
		return nil, false
	}
	existing, err := finder.FindRoleAssignmentByID(r.Context(), assignmentID, tenantScope)
	if err != nil {
		if errors.Is(err, domain.ErrRoleAssignmentNotFound) {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "role_assignment_not_found"})
			return nil, false
		}
		h.log.Error("assignment lookup failed", zap.String("assignment_id", assignmentID), zap.Error(err))
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "store_unavailable"})
		return nil, false
	}
	if expected > 0 && expected != existing.Version {
		writeVersionConflict(w, r)
		return nil, false
	}
	return existing, true
}

// writeAssignmentTransitionErr answers a suspend / resume the store refused.
func (h *Handler) writeAssignmentTransitionErr(w http.ResponseWriter, op string, err error, message string) {
	switch {
	case errors.Is(err, domain.ErrRoleAssignmentNotFound):
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "role_assignment_not_found"})
	case errors.Is(err, domain.ErrInvalidTransition):
		writeJSON(w, http.StatusConflict, map[string]string{"error": "invalid_transition", "message": message})
	default:
		h.log.Error(op+": store unavailable", zap.Error(err))
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "store_unavailable"})
	}
}

// SuspendRoleAssignment handles POST /v1/admin/role-assignments/{assignment_id}/suspend.
//
// The assignment stops granting at once and stays on record; /resume lifts
// it. Withdrawing access, so it takes what revoking takes: iam.assignment.revoke,
// and a platform act for an assignment on the platform-scope entity. The §16
// reason is recorded as the suspension reason.
//
// Response: 200 suspended / 400 / 403 / 404 / 409 not approved, already
// suspended or ended / 412 stale If-Match / 503.
func (h *Handler) SuspendRoleAssignment(w http.ResponseWriter, r *http.Request) {
	assignmentID := chi.URLParam(r, "assignment_id")
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	tenantScope, ok := h.requireTenant(w, r)
	if !ok {
		return
	}
	if !h.requirePermission(w, r, principalID, tenantScope, "iam.assignment.revoke") {
		return
	}
	bodyVersion, ok := h.readCommand(w, &r)
	if !ok {
		return
	}
	expected, ok := optionalExpectedVersion(w, r, bodyVersion)
	if !ok {
		return
	}
	suspender, ok := h.store.(assignmentSuspender)
	if !ok {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "store_unavailable"})
		return
	}
	existing, ok := h.assignmentForMutation(w, r, assignmentID, tenantScope, expected)
	if !ok {
		return
	}
	if existing.LegalEntityID != nil && h.isPlatformScopeEntity(*existing.LegalEntityID) &&
		!h.requirePlatformAction(w, r, principalID, "iam.assignment.revoke") {
		return
	}
	_, _, reason := domain.AuditFrom(r.Context())
	a, err := suspender.SuspendRoleAssignment(r.Context(), assignmentID, tenantScope, principalID, reason)
	if err != nil {
		h.writeAssignmentTransitionErr(w, "SuspendRoleAssignment", err,
			"only an approved assignment that has not ended and is not already suspended can be suspended")
		return
	}
	writeJSONWithETag(w, http.StatusOK, a, a.Version)
}

// ResumeRoleAssignment handles POST /v1/admin/role-assignments/{assignment_id}/resume.
//
// Lifting a suspension restores access, so it takes what granting takes:
// iam.assignment.grant, never on one's own assignment, a platform act on the
// platform-scope entity, iam.assignment.approve_privileged for a role carrying
// IAM or platform administration, and no segregation-of-duties conflict with
// what the principal holds now — which may have changed while it was
// suspended.
//
// Response: 200 resumed / 400 / 403 / 404 / 409 not suspended, ended or SoD
// conflict / 412 stale If-Match / 503.
func (h *Handler) ResumeRoleAssignment(w http.ResponseWriter, r *http.Request) {
	assignmentID := chi.URLParam(r, "assignment_id")
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	tenantScope, ok := h.requireTenant(w, r)
	if !ok {
		return
	}
	if !h.requirePermission(w, r, principalID, tenantScope, "iam.assignment.grant") {
		return
	}
	bodyVersion, ok := h.readCommand(w, &r)
	if !ok {
		return
	}
	expected, ok := optionalExpectedVersion(w, r, bodyVersion)
	if !ok {
		return
	}
	suspender, ok := h.store.(assignmentSuspender)
	if !ok {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "store_unavailable"})
		return
	}
	existing, ok := h.assignmentForMutation(w, r, assignmentID, tenantScope, expected)
	if !ok {
		return
	}
	if existing.PrincipalID == principalID {
		writeJSON(w, http.StatusForbidden, map[string]string{
			"error":   "self_grant_not_allowed",
			"message": "a principal cannot lift the suspension of their own assignment",
		})
		return
	}
	if existing.SuspendedAt == nil {
		writeJSON(w, http.StatusConflict, map[string]string{"error": "invalid_transition", "message": "the assignment is not suspended"})
		return
	}
	entity := ""
	if existing.LegalEntityID != nil {
		entity = *existing.LegalEntityID
	}
	if h.isPlatformScopeEntity(entity) && !h.requirePlatformAction(w, r, principalID, "iam.assignment.grant") {
		return
	}
	roleActions, err := h.roleActiveActions(r.Context(), existing.RoleID, tenantScope)
	if err != nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "store_unavailable"})
		return
	}
	if anyPrivileged(roleActions) {
		entitled, err := h.holdsPrivilegedApproval(r, principalID, tenantScope, entity)
		if err != nil {
			writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "store_unavailable"})
			return
		}
		if !entitled {
			writeJSON(w, http.StatusForbidden, map[string]string{
				"error":   "authorization_denied",
				"message": PermissionApprovePrivileged + " is required to resume a privileged assignment",
			})
			return
		}
	}
	req := createAssignmentRequest{PrincipalID: existing.PrincipalID, RoleID: existing.RoleID, LegalEntityID: entity}
	if conflicts, ok := h.assignmentSoDConflicts(w, r, req, existing.RoleID, tenantScope); !ok {
		return
	} else if len(conflicts) > 0 {
		writeJSON(w, http.StatusConflict, map[string]any{
			"error": "sod_conflict", "message": "the assignment now conflicts with a segregation-of-duties rule for this principal",
			"conflicts": conflicts,
		})
		return
	}
	a, err := suspender.ResumeRoleAssignment(r.Context(), assignmentID, tenantScope)
	if err != nil {
		h.writeAssignmentTransitionErr(w, "ResumeRoleAssignment", err, "the assignment is not suspended, or has ended")
		return
	}
	writeJSONWithETag(w, http.StatusOK, a, a.Version)
}

// listRoleAssignmentsAsOf answers GET /v1/admin/role-assignments?as_of= —
// the assignments that granted at that instant, reconstructed from the
// history (GetAssignmentsAsOf). principal_id and role_id narrow it as on the
// current list. A future as_of is refused: the history records what was, not
// what will be.
func (h *Handler) listRoleAssignmentsAsOf(w http.ResponseWriter, r *http.Request, tenantScope, principalFilter, roleFilter, asOfRaw string) {
	asOf, err := time.Parse(time.RFC3339, asOfRaw)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid_as_of", "message": "as_of must be RFC 3339, e.g. 2026-03-01T00:00:00Z"})
		return
	}
	if asOf.After(time.Now()) {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid_as_of", "message": "as_of must not be in the future"})
		return
	}
	reader, ok := h.store.(assignmentHistoryReader)
	if !ok {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "store_unavailable"})
		return
	}
	held, err := reader.FindRoleAssignmentsAsOf(r.Context(), tenantScope, principalFilter, asOf.UTC())
	if err != nil {
		h.log.Error("ListRoleAssignments as_of: store unavailable", zap.Error(err))
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "store_unavailable"})
		return
	}
	out := make([]domain.PrincipalRoleAssignment, 0, len(held))
	for _, a := range held {
		if roleFilter == "" || strings.EqualFold(a.RoleID, roleFilter) {
			out = append(out, a)
		}
	}
	writeJSON(w, http.StatusOK, out)
}
