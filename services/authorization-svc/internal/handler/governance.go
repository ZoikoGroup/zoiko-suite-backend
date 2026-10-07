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
)

// Governance controls on the administration surface, decided by the specs:
//
//   - purpose / reason_code on privileged and destructive commands
//     (Governance Control Plane §16), recorded with the change (000025);
//   - maker-checker on privileged assignments (ZS-IAM-001 §9, A20; GOV-04
//     negative path #2; GOV-12 checker states);
//   - protected platform-admin permissions kept out of tenant custom roles
//     (ZS-IAM-001 §9);
//   - toxic combinations refused when a role change would create them for
//     the people who already hold it (GOV-04 "prevent conflicting access
//     assignments"; ZS-IAM-001 §24).

// CommandContractEnforce selects how a command missing its §16 fields is
// treated. Warn admits it and marks the response (X-Command-Contract:
// violated), the way the envelope's write-strict mode admits a violating
// evaluate call; Enforce refuses it with 400. Warn is the migration state:
// the console and access-control-svc send neither reason_code nor a
// delegation end date yet.
type CommandContractMode int

const (
	CommandContractWarn CommandContractMode = iota
	CommandContractEnforce
)

// HeaderCommandContract marks a command admitted in warn mode without the
// fields §16 requires.
const HeaderCommandContract = "X-Command-Contract"

// PermissionApprovePrivileged is the independent approval of a privileged
// assignment — the same permission access-control-svc requires of its
// security approver (its migration 000013).
const PermissionApprovePrivileged = "iam.assignment.approve_privileged"

// PendingApprovalWindow bounds how long a privileged assignment waits for its
// checker before it expires (GOV-12 "Expired").
const PendingApprovalWindow = 72 * time.Hour

// DefaultDelegationTerm is the end date given, in warn mode, to a delegation
// created without one: §11 makes the period "mandatory finite".
const DefaultDelegationTerm = 90 * 24 * time.Hour

// SetCommandContract sets the §16 enforcement mode. New leaves it at warn.
func (h *Handler) SetCommandContract(mode CommandContractMode) {
	h.commandContract = mode
}

// auditMiddleware puts the verified caller and correlation id on the request
// context; the store installs them on every transaction and the history
// trigger records them.
func auditMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx := domain.WithAudit(r.Context(), r.Header.Get("X-Principal-Id"), r.Header.Get("X-Correlation-ID"))
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// commandFields are the §16 fields a privileged or destructive command
// carries, plus expected_version for the protected updates.
type commandFields struct {
	ExpectedVersion int64  `json:"expected_version"`
	ReasonCode      string `json:"reason_code"`
	Purpose         string `json:"purpose"`
}

func (c commandFields) reason() string {
	code, purpose := strings.TrimSpace(c.ReasonCode), strings.TrimSpace(c.Purpose)
	switch {
	case code != "" && purpose != "":
		return code + ": " + purpose
	case code != "":
		return code
	default:
		return purpose
	}
}

// readCommand reads the optional body of a privileged or destructive command
// — {"expected_version": n, "reason_code": "...", "purpose": "..."} — applies
// the §16 reason rule, and puts the reason on the request context so the
// change records it. An empty body is valid JSON-wise. Writes a 400 and
// returns false on a malformed body, a negative version, or (enforce) no
// reason.
func (h *Handler) readCommand(w http.ResponseWriter, r **http.Request) (int64, bool) {
	var body commandFields
	if (*r).Body != nil {
		if err := json.NewDecoder((*r).Body).Decode(&body); err != nil && !errors.Is(err, io.EOF) {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid_json", "message": err.Error()})
			return 0, false
		}
	}
	if body.ExpectedVersion < 0 {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid_expected_version", "message": "expected_version must be positive"})
		return 0, false
	}
	if !h.applyReason(w, r, body.reason()) {
		return 0, false
	}
	return body.ExpectedVersion, true
}

// applyReason enforces §16's "purpose / reason_code required for privileged,
// destructive or emergency commands" per the contract mode, and records the
// reason on the request context.
func (h *Handler) applyReason(w http.ResponseWriter, r **http.Request, reason string) bool {
	if reason == "" {
		if h.commandContract == CommandContractEnforce {
			writeJSON(w, http.StatusBadRequest, map[string]string{
				"error":   "missing_field",
				"field":   "reason_code",
				"message": "reason_code or purpose is required for a privileged or destructive command",
			})
			return false
		}
		w.Header().Set(HeaderCommandContract, "violated")
		h.log.Warn("command admitted without reason_code (warn mode)",
			zap.String("path", (*r).URL.Path),
			zap.String("source_system", (*r).Header.Get("X-Source-System")),
			zap.String("correlation_id", (*r).Header.Get("X-Correlation-ID")))
		return true
	}
	*r = (*r).WithContext(domain.WithReason((*r).Context(), reason))
	return true
}

// roleActiveActions is every action the role's ACTIVE bundles grant.
func (h *Handler) roleActiveActions(ctx context.Context, roleID, tenantID string) ([]string, error) {
	bundles, err := h.store.ListPermissionBundles(ctx, roleID, tenantID)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, b := range bundles {
		if b.ActiveFlag {
			out = append(out, b.PermittedActions...)
		}
	}
	return dedupeSorted(out), nil
}

func anyPrivileged(actions []string) bool {
	for _, a := range actions {
		if domain.IsPrivilegedAction(a) {
			return true
		}
	}
	return false
}

func anyProtectedPlatform(actions []string) []string {
	var out []string
	for _, a := range actions {
		if domain.IsProtectedPlatformAction(a) {
			out = append(out, a)
		}
	}
	return out
}

// holderConflict is one holder of a role for whom a role change would create
// a segregation-of-duties conflict.
type holderConflict struct {
	PrincipalID string        `json:"principal_id"`
	Conflicts   []sodConflict `json:"conflicts"`
}

// refuseToxicForHolders refuses (409) a change that would give candidates to
// every current holder of roleID when, for any of them, that creates a static
// SoD conflict with what they already hold. Writes a 503 and returns true if
// it cannot be established.
//
// Static SoD was checked when a role was ASSIGNED (CreateRoleAssignment) but
// not when the role itself CHANGED, so adding payment.release to a role held
// by a preparer handed every holder the toxic pair the assignment check
// exists to prevent.
func (h *Handler) refuseToxicForHolders(w http.ResponseWriter, r *http.Request, roleID, tenantID string, candidates []string) bool {
	candidates = dedupeSorted(candidates)
	if len(candidates) == 0 {
		return false
	}
	holders, err := h.store.ListRoleAssignments(r.Context(), tenantID, "", roleID, false)
	if err != nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "store_unavailable"})
		return true
	}
	now := time.Now()
	seen := map[string]bool{}
	var found []holderConflict
	for _, a := range holders {
		if seen[a.PrincipalID] || (a.EffectiveTo != nil && !a.EffectiveTo.After(now)) ||
			a.ApprovalStatus == domain.ApprovalRejected || a.ApprovalStatus == domain.ApprovalExpired {
			continue
		}
		seen[a.PrincipalID] = true
		held, err := h.heldActionsInTenant(r, a.PrincipalID, tenantID)
		if err != nil {
			writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "store_unavailable"})
			return true
		}
		var heldList []string
		for act := range held {
			heldList = append(heldList, act)
		}
		conflicts, err := h.sodConflictsFor(r.Context(), dedupeSorted(heldList), candidates, tenantID)
		if err != nil {
			writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "store_unavailable"})
			return true
		}
		if len(conflicts) > 0 {
			found = append(found, holderConflict{PrincipalID: a.PrincipalID, Conflicts: conflicts})
		}
	}
	if len(found) == 0 {
		return false
	}
	writeJSON(w, http.StatusConflict, map[string]any{
		"error":   "sod_conflict",
		"message": "this change would give current holders of the role a segregation-of-duties conflict",
		"holders": found,
	})
	return true
}

// holdsPrivilegedApproval reports whether the caller may act as the
// independent approver of a privileged assignment at the ASSIGNMENT'S scope:
// the platform-scope grant for a platform-scope assignment, the grant at the
// assignment's legal entity for an entity assignment (a tenant-wide grant
// covers it), and the tenant grant for a tenant-wide one. access-control-svc
// checks its security approver at the request's entity; checking only the
// tenant here would park as PENDING a grant that approver had cleared.
func (h *Handler) holdsPrivilegedApproval(r *http.Request, principalID, tenantID, legalEntityID string) (bool, error) {
	scope, tenant := tenantID, tenantID
	switch {
	case h.isPlatformScopeEntity(legalEntityID):
		scope, tenant = h.platformScopeEntityID, ""
	case legalEntityID != "":
		scope = legalEntityID
	}
	actions, _, err := h.store.FindGrantedActions(r.Context(), principalID, scope, tenant)
	if err != nil {
		return false, err
	}
	return contains(actions, PermissionApprovePrivileged), nil
}

// assignmentDecider is the store capability behind approve / reject.
type assignmentDecider interface {
	DecideRoleAssignment(ctx context.Context, assignmentID, tenantID, decision, deciderPrincipalID string) (*domain.PrincipalRoleAssignment, error)
}

// ApproveRoleAssignment handles POST /v1/admin/role-assignments/{assignment_id}/approve.
// RejectRoleAssignment handles .../reject.
//
// The checker of a PENDING_APPROVAL privileged assignment (GOV-12): must hold
// iam.assignment.approve_privileged at the assignment's scope, must be neither
// the maker (assigned_by) nor the target (GOV-04 #2 "user cannot approve own
// access elevation"), and the window must not have passed. Approval re-runs
// the static SoD check, because what the target holds may have changed since
// the request.
//
// Response: 200 decided / 400 / 403 not independent or not entitled / 404 /
// 409 not pending, expired, or SoD conflict / 503.
func (h *Handler) ApproveRoleAssignment(w http.ResponseWriter, r *http.Request) {
	h.decideRoleAssignment(w, r, domain.ApprovalApproved)
}

func (h *Handler) RejectRoleAssignment(w http.ResponseWriter, r *http.Request) {
	h.decideRoleAssignment(w, r, domain.ApprovalRejected)
}

func (h *Handler) decideRoleAssignment(w http.ResponseWriter, r *http.Request, decision string) {
	assignmentID := chi.URLParam(r, "assignment_id")
	correlationID := r.Header.Get("X-Correlation-ID")

	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	tenantScope, ok := h.requireTenant(w, r)
	if !ok {
		return
	}
	if _, ok := h.readCommand(w, &r); !ok {
		return
	}

	finder, okF := h.store.(roleAssignmentFinder)
	decider, okD := h.store.(assignmentDecider)
	if !okF || !okD {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "store_unavailable"})
		return
	}
	pending, err := finder.FindRoleAssignmentByID(r.Context(), assignmentID, tenantScope)
	if err != nil {
		if errors.Is(err, domain.ErrRoleAssignmentNotFound) {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "role_assignment_not_found"})
			return
		}
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "store_unavailable"})
		return
	}
	if pending.ApprovalStatus != domain.ApprovalPending {
		writeJSON(w, http.StatusConflict, map[string]string{"error": "approval_not_pending", "approval_status": pending.ApprovalStatus})
		return
	}
	if principalID == pending.AssignedBy || principalID == pending.PrincipalID {
		writeJSON(w, http.StatusForbidden, map[string]string{
			"error":   "checker_not_independent",
			"message": "the approver must be neither the requester nor the principal receiving the access",
		})
		return
	}
	entity := ""
	if pending.LegalEntityID != nil {
		entity = *pending.LegalEntityID
	}
	entitled, err := h.holdsPrivilegedApproval(r, principalID, tenantScope, entity)
	if err != nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "store_unavailable"})
		return
	}
	if !entitled {
		writeJSON(w, http.StatusForbidden, map[string]string{
			"error":   "authorization_denied",
			"message": PermissionApprovePrivileged + " is required to decide a privileged assignment",
		})
		return
	}
	if decision == domain.ApprovalApproved {
		req := createAssignmentRequest{PrincipalID: pending.PrincipalID, RoleID: pending.RoleID, LegalEntityID: entity}
		if conflicts, ok := h.assignmentSoDConflicts(w, r, req, pending.RoleID, tenantScope); !ok {
			return
		} else if len(conflicts) > 0 {
			writeJSON(w, http.StatusConflict, map[string]any{
				"error": "sod_conflict", "message": "the assignment now conflicts with a segregation-of-duties rule for this principal",
				"conflicts": conflicts,
			})
			return
		}
	}

	decided, err := decider.DecideRoleAssignment(r.Context(), assignmentID, tenantScope, decision, principalID)
	switch {
	case errors.Is(err, domain.ErrApprovalExpired):
		writeJSON(w, http.StatusConflict, map[string]string{"error": "approval_expired", "message": err.Error()})
		return
	case errors.Is(err, domain.ErrApprovalNotPending):
		writeJSON(w, http.StatusConflict, map[string]string{"error": "approval_not_pending"})
		return
	case errors.Is(err, domain.ErrRoleAssignmentNotFound):
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "role_assignment_not_found"})
		return
	case err != nil:
		h.log.Error("decideRoleAssignment: store unavailable", zap.String("correlation_id", correlationID), zap.Error(err))
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "store_unavailable"})
		return
	}
	h.log.Info("privileged assignment decided",
		zap.String("assignment_id", assignmentID), zap.String("decision", decision),
		zap.String("checker", principalID), zap.String("maker", pending.AssignedBy),
		zap.String("correlation_id", correlationID))
	writeJSON(w, http.StatusOK, decided)
}
