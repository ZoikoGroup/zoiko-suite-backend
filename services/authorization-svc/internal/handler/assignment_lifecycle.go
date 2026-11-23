package handler

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"time"

	"go.uber.org/zap"

	"zoiko.io/authorization-svc/internal/domain"
)

// The ORG-06 role half (IAM-13), on this service's role assignments:
//
//	AssignBusinessRole        POST /v1/admin/role-assignments
//	ListPrincipalAssignments  GET  /v1/admin/role-assignments?principal_id=
//	SuspendAssignment         POST /v1/admin/role-assignments/{id}/suspend, /resume
//	GetAssignmentsAsOf        GET  /v1/admin/role-assignments?as_of=<RFC 3339>
//	                          GET  /v1/admin/role-assignments/as-of
//
// Suspend and resume are in assignment_suspension.go (000037). This file holds
// the If-Match handling the assignment mutations share, and the ?as_of= read
// of the append-only history (000038).

// assignmentHistoryReader is the store capability behind ?as_of=.
type assignmentHistoryReader interface {
	FindRoleAssignmentsAsOf(ctx context.Context, tenantID, principalID string, asOf time.Time) ([]domain.PrincipalRoleAssignment, error)
}

// Role assignments and workload bindings were the last aggregates where
// If-Match was optional (optionalExpectedVersion, 2 Nov). Since 10 Nov 2026
// (AZ-1) they use expectedVersion like every other versioned aggregate: no
// If-Match and no expected_version is 428; "If-Match: *" is the explicit
// unconditional form.

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
