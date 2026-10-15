package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"

	"go.uber.org/zap"
)

// SoDEvaluatePath answers a maker-checker segregation question — the GOV-04
// contract identity-context-svc calls before AttachSupportContext and
// UpdatePrincipalStatus. No GOV-04 service exists on this estate; this service
// already owns the SoD rules, so it answers here (decision 5 Oct 2026).
//
// It is a different question from /v1/sod/validate. That one asks "would
// holding these actions together conflict"; this one asks "may THIS maker do
// THIS action, approved by THIS checker, about THIS subject". Before it
// existed identity-context-svc called a route nothing served and, with
// SOD_SERVICE_URL empty, fell back to a permit-everything stub.
const SoDEvaluatePath = "/v1/sod/evaluate"

// sodRuleVersion names the rule set that decided, so the evidence a caller
// records is reproducible. The static rules are data (sod_rules); the
// structural maker-checker rules below are this code.
const sodRuleVersion = "authorization-svc/sod-evaluate/v1"

type sodEvaluateRequest struct {
	TenantID           string `json:"tenant_id"`
	ActionType         string `json:"action_type"`
	MakerPrincipalID   string `json:"maker_principal_id"`
	CheckerPrincipalID string `json:"checker_principal_id,omitempty"`
	SubjectPrincipalID string `json:"subject_principal_id,omitempty"`
	CorrelationID      string `json:"correlation_id,omitempty"`
}

type sodEvaluateResponse struct {
	Result      string `json:"result"` // NO_CONFLICT | CONFLICT
	RuleVersion string `json:"rule_version"`
	ConflictID  string `json:"conflict_id,omitempty"`
	Reason      string `json:"reason,omitempty"`
}

type tenantActionsFinder interface {
	FindGrantedActionsInTenant(ctx context.Context, principalID, tenantID string) ([]string, error)
}

// EvaluateSoD handles POST /v1/sod/evaluate.
//
// CONFLICT, in order of how plain the violation is:
//   - maker_is_checker: one person made and approved the act;
//   - checker_is_subject: the approver approved something about themselves;
//   - maker_is_subject: the maker acted on their own identity or access;
//   - held_duty_conflict: the maker or the checker holds, anywhere in the
//     tenant, a duty the tenant's sod_rules say conflicts with this action.
//
// Every store failure is 503, never NO_CONFLICT: the caller fails closed on
// it, and a check that did not run must not read as one that passed.
func (h *Handler) EvaluateSoD(w http.ResponseWriter, r *http.Request) {
	correlationID := r.Header.Get("X-Correlation-ID")
	if _, ok := h.requirePrincipal(w, r); !ok {
		return
	}
	var req sodEvaluateRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid_json", "message": err.Error()})
		return
	}
	req.ActionType = strings.TrimSpace(req.ActionType)
	req.MakerPrincipalID = strings.TrimSpace(req.MakerPrincipalID)
	req.CheckerPrincipalID = strings.TrimSpace(req.CheckerPrincipalID)
	req.SubjectPrincipalID = strings.TrimSpace(req.SubjectPrincipalID)
	if req.ActionType == "" || req.MakerPrincipalID == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "missing_field", "message": "action_type and maker_principal_id are required"})
		return
	}
	tenantScope, ok := h.requireTenant(w, r)
	if !ok {
		return
	}
	if h.refuseForeignTenant(w, req.TenantID, tenantScope) {
		return
	}

	conflict := func(id, reason string) {
		writeJSON(w, http.StatusOK, sodEvaluateResponse{Result: "CONFLICT", RuleVersion: sodRuleVersion, ConflictID: id, Reason: reason})
	}
	switch {
	case req.CheckerPrincipalID != "" && strings.EqualFold(req.CheckerPrincipalID, req.MakerPrincipalID):
		conflict("maker_is_checker", "the principal performing the action also approved it")
		return
	case req.CheckerPrincipalID != "" && req.SubjectPrincipalID != "" && strings.EqualFold(req.CheckerPrincipalID, req.SubjectPrincipalID):
		conflict("checker_is_subject", "the approver is the principal the action is about")
		return
	case req.SubjectPrincipalID != "" && strings.EqualFold(req.SubjectPrincipalID, req.MakerPrincipalID):
		conflict("maker_is_subject", "the maker is acting on their own identity or access")
		return
	}

	finder, ok := h.store.(tenantActionsFinder)
	if !ok {
		h.log.Error("EvaluateSoD: store cannot resolve tenant-wide holdings", zap.String("correlation_id", correlationID))
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "store_unavailable"})
		return
	}
	for _, who := range []struct{ role, principal string }{{"maker", req.MakerPrincipalID}, {"checker", req.CheckerPrincipalID}} {
		if who.principal == "" {
			continue
		}
		held, err := finder.FindGrantedActionsInTenant(r.Context(), who.principal, tenantScope)
		if err != nil {
			h.log.Error("EvaluateSoD: store unavailable (holdings)", zap.String("correlation_id", correlationID), zap.Error(err))
			writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "store_unavailable"})
			return
		}
		others := removeAll(held, req.ActionType)
		if len(others) == 0 {
			continue
		}
		conflicting, has, err := h.store.CheckSoDConflict(r.Context(), others, req.ActionType, tenantScope)
		if err != nil {
			h.log.Error("EvaluateSoD: store unavailable (sod rules)", zap.String("correlation_id", correlationID), zap.Error(err))
			writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "store_unavailable"})
			return
		}
		if has {
			conflict("held_duty_conflict", "the "+who.role+" holds "+conflicting+", which conflicts with "+req.ActionType)
			return
		}
	}
	writeJSON(w, http.StatusOK, sodEvaluateResponse{Result: "NO_CONFLICT", RuleVersion: sodRuleVersion})
}
