package handler

// ORG-06 s4.6: SuspendAssignment (and its resume), ListAssignmentSuspensions
// and GetAssignmentsAsOf on business role assignments.
//
//	POST /v1/admin/role-assignments/{assignment_id}/suspend       iam.assignment.suspend
//	POST /v1/admin/role-assignments/{assignment_id}/resume        iam.assignment.resume
//	GET  /v1/admin/role-assignments/{assignment_id}/suspensions   iam.assignment.read
//	GET  /v1/admin/role-assignments/as-of                         iam.assignment.read
//
// A suspended assignment grants nothing at once, but is still held: it can be
// resumed, revoked or expire. See migration 000037.

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
)

// Permissions, registered in access-control-svc's permission_definitions.
const (
	PermissionAssignmentSuspend = "iam.assignment.suspend"
	PermissionAssignmentResume  = "iam.assignment.resume"
	permissionAssignmentRead    = "iam.assignment.read"
)

type assignmentSuspender interface {
	SuspendRoleAssignment(ctx context.Context, assignmentID, tenantID, actor, reason string) (*domain.PrincipalRoleAssignment, error)
	ResumeRoleAssignment(ctx context.Context, assignmentID, tenantID, actor, reason string) (*domain.PrincipalRoleAssignment, error)
}

type assignmentSuspensionLister interface {
	ListAssignmentSuspensions(ctx context.Context, assignmentID, tenantID string) ([]domain.AssignmentSuspension, error)
}

type assignmentsAsOfReader interface {
	QueryAssignmentsAsOf(ctx context.Context, q domain.AssignmentsAsOfQuery) ([]domain.PrincipalRoleAssignment, error)
}

// writeSuspensionError maps a store refusal to its response. ok is false when
// err is not one this surface knows, so the caller reports an outage.
func writeSuspensionError(w http.ResponseWriter, err error) bool {
	switch {
	case errors.Is(err, domain.ErrRoleAssignmentNotFound):
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "role_assignment_not_found"})
	case errors.Is(err, domain.ErrAssignmentNotSuspendable):
		writeJSON(w, http.StatusConflict, map[string]string{"error": "assignment_not_suspendable", "message": err.Error()})
	case errors.Is(err, domain.ErrAssignmentNotSuspended):
		writeJSON(w, http.StatusConflict, map[string]string{"error": "assignment_not_suspended", "message": err.Error()})
	case errors.Is(err, domain.ErrAssignmentEnded):
		writeJSON(w, http.StatusConflict, map[string]string{"error": "assignment_ended", "message": err.Error()})
	case errors.Is(err, domain.ErrSelfResume):
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "self_resume_not_allowed", "message": err.Error()})
	default:
		return false
	}
	return true
}

// SuspendRoleAssignment handles POST .../{assignment_id}/suspend.
func (h *Handler) SuspendRoleAssignment(w http.ResponseWriter, r *http.Request) {
	h.changeSuspension(w, r, true)
}

// ResumeRoleAssignment handles POST .../{assignment_id}/resume.
func (h *Handler) ResumeRoleAssignment(w http.ResponseWriter, r *http.Request) {
	h.changeSuspension(w, r, false)
}

func (h *Handler) changeSuspension(w http.ResponseWriter, r *http.Request, suspend bool) {
	assignmentID := chi.URLParam(r, "assignment_id")
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	tenantScope, ok := h.requireTenant(w, r)
	if !ok {
		return
	}
	permission := PermissionAssignmentResume
	if suspend {
		permission = PermissionAssignmentSuspend
	}
	if !h.requirePermission(w, r, principalID, tenantScope, permission) {
		return
	}

	var body commandFields
	if r.Body != nil {
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil && !errors.Is(err, io.EOF) {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid_json", "message": err.Error()})
			return
		}
	}
	reason := strings.TrimSpace(body.reason())
	// A suspension without a reason is evidence of nothing, so it is required
	// whatever the command-contract mode; a resume's reason is optional.
	if suspend && reason == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"error":   "missing_field",
			"field":   "reason_code",
			"message": "reason_code or purpose is required to suspend an assignment",
		})
		return
	}
	if !h.applyReason(w, &r, reason) {
		return
	}

	// Suspending or resuming a platform-scope assignment is a platform act,
	// as revoking one is.
	if h.platformScopeEntityID != "" {
		finder, ok := h.store.(roleAssignmentFinder)
		if !ok {
			writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "store_unavailable"})
			return
		}
		existing, err := finder.FindRoleAssignmentByID(r.Context(), assignmentID, tenantScope)
		if err != nil {
			if !writeSuspensionError(w, err) {
				h.log.Error("suspension: assignment lookup failed", zap.String("correlation_id", r.Header.Get("X-Correlation-ID")), zap.Error(err))
				writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "store_unavailable"})
			}
			return
		}
		if existing.LegalEntityID != nil && h.isPlatformScopeEntity(*existing.LegalEntityID) &&
			!h.requirePlatformAction(w, r, principalID, permission) {
			return
		}
	}

	suspender, ok := h.store.(assignmentSuspender)
	if !ok {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "store_unavailable"})
		return
	}
	var (
		a   *domain.PrincipalRoleAssignment
		err error
	)
	if suspend {
		a, err = suspender.SuspendRoleAssignment(r.Context(), assignmentID, tenantScope, principalID, reason)
	} else {
		a, err = suspender.ResumeRoleAssignment(r.Context(), assignmentID, tenantScope, principalID, reason)
	}
	if err != nil {
		if !writeSuspensionError(w, err) {
			h.log.Error("suspension: store failure", zap.String("correlation_id", r.Header.Get("X-Correlation-ID")), zap.Error(err))
			writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "store_unavailable"})
		}
		return
	}
	writeJSON(w, http.StatusOK, a)
}

// ListAssignmentSuspensions handles GET .../{assignment_id}/suspensions.
func (h *Handler) ListAssignmentSuspensions(w http.ResponseWriter, r *http.Request) {
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	tenantScope, ok := h.requireTenant(w, r)
	if !ok {
		return
	}
	if !h.requirePermission(w, r, principalID, tenantScope, permissionAssignmentRead) {
		return
	}
	lister, ok := h.store.(assignmentSuspensionLister)
	if !ok {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "store_unavailable"})
		return
	}
	out, err := lister.ListAssignmentSuspensions(r.Context(), chi.URLParam(r, "assignment_id"), tenantScope)
	if err != nil {
		if !writeSuspensionError(w, err) {
			h.log.Error("suspension list failed", zap.String("correlation_id", r.Header.Get("X-Correlation-ID")), zap.Error(err))
			writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "store_unavailable"})
		}
		return
	}
	writeJSON(w, http.StatusOK, out)
}

// AssignmentsAsOfResponse is the answer to GetAssignmentsAsOf.
type AssignmentsAsOfResponse struct {
	AsOf        time.Time                        `json:"as_of"`
	Assignments []domain.PrincipalRoleAssignment `json:"assignments"`
}

// GetAssignmentsAsOf handles GET /v1/admin/role-assignments/as-of.
//
// It answers "which assignments were AUTHORIZING at that instant": approved by
// then, inside their effective window and not suspended. An assignment counts
// from when it was approved, not from a backdated effective_from, because the
// question is what the principal could do, and nothing was granted before the
// grant existed.
func (h *Handler) GetAssignmentsAsOf(w http.ResponseWriter, r *http.Request) {
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	tenantScope, ok := h.requireTenant(w, r)
	if !ok {
		return
	}
	if !h.requirePermission(w, r, principalID, tenantScope, permissionAssignmentRead) {
		return
	}

	asOf := time.Now().UTC()
	if raw := strings.TrimSpace(r.URL.Query().Get("as_of")); raw != "" {
		t, err := time.Parse(time.RFC3339, raw)
		if err != nil {
			t, err = time.Parse("2006-01-02", raw)
		}
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{
				"error":   "invalid_as_of",
				"message": "as_of must be an RFC3339 timestamp or a YYYY-MM-DD date",
			})
			return
		}
		asOf = t.UTC()
	}

	reader, ok := h.store.(assignmentsAsOfReader)
	if !ok {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "store_unavailable"})
		return
	}
	out, err := reader.QueryAssignmentsAsOf(r.Context(), domain.AssignmentsAsOfQuery{
		TenantID: tenantScope, PrincipalID: strings.TrimSpace(r.URL.Query().Get("principal_id")), AsOf: asOf,
	})
	if err != nil {
		h.log.Error("GetAssignmentsAsOf failed", zap.String("correlation_id", r.Header.Get("X-Correlation-ID")), zap.Error(err))
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "store_unavailable"})
		return
	}
	if out == nil {
		out = []domain.PrincipalRoleAssignment{}
	}
	writeJSON(w, http.StatusOK, AssignmentsAsOfResponse{AsOf: asOf, Assignments: out})
}
