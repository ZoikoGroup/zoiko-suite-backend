package handler

import (
	"errors"
	"net/http"
	"strings"

	"go.uber.org/zap"

	"zoiko.io/access-control-svc/internal/domain"
)

// INT-10 identity federation reaches access control through two doors, both here.
//
//   - POST /v1/iam/access-assignments with origin=FEDERATION is the ordinary governed request, held to
//     baseline access (see submitAssignment): a mapping can never open an approval path.
//   - GET /v1/iam/federation/role-eligibility tells the identity service, before a mapping is ever approved,
//     whether a role could be the target of one. The identity service never decides what is privileged; this
//     service owns the risk taxonomy and answers from it.

// RoleEligibility handles GET /v1/iam/federation/role-eligibility?role_definition_id=.
//
// Eligible means the role is ACTIVE and every action in it is STANDARD risk. An action the taxonomy does not
// know is HIGH (see roleRisk), so an unclassified role is not eligible either. A taxonomy outage is a 503, never
// "eligible".
func (g *Gov) RoleEligibility(w http.ResponseWriter, r *http.Request) {
	if _, ok := g.reader(w, r); !ok {
		return
	}
	roleID := strings.TrimSpace(r.URL.Query().Get("role_definition_id"))
	if roleID == "" {
		writeError(w, r, http.StatusBadRequest, "missing_fields", "role_definition_id is required")
		return
	}
	role, err := g.h.store.GetRole(r.Context(), roleID)
	if err != nil {
		if errors.Is(err, domain.ErrRoleNotFound) {
			writeError(w, r, http.StatusNotFound, "not_found", err.Error())
			return
		}
		writeError(w, r, http.StatusServiceUnavailable, "store_unavailable", err.Error())
		return
	}
	out := domain.FederationRoleEligibility{RoleDefinitionID: role.RoleDefinitionID}
	if role.Status != domain.RoleStatusActive {
		out.RiskTier, out.Reason = "", "the role is not active"
		writeJSON(w, http.StatusOK, out)
		return
	}
	actions, err := g.roleActions(r.Context(), role.RoleDefinitionID)
	if err == nil {
		var risk string
		var drivers []string
		risk, drivers, err = g.h.roleRisk(r.Context(), actions)
		if err == nil {
			out.RiskTier, out.Drivers = risk, drivers
			out.Eligible = risk == domain.RiskStandard
			if out.Eligible {
				out.Reason = "baseline role: every action is standard risk"
			} else {
				out.Reason = "the role grants privileged actions (" + strings.Join(drivers, ", ") + ")"
			}
			writeJSON(w, http.StatusOK, out)
			return
		}
	}
	status, code := classify(err)
	g.h.log.Error("role eligibility could not be decided", zap.Error(err))
	writeError(w, r, status, code, "the role's risk could not be determined")
}
