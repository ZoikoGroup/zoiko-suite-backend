package handler

import (
	"bytes"
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"go.uber.org/zap"

	"zoiko.io/jurisdiction-rules-svc/internal/domain"
)

// ValidateActionPath is Doc 03 §8.2's "validate-action" API. It was documented
// in openapi.yaml and never routed: a client generated from the spec got 404
// (Governance audit J-06, row 4).
const ValidateActionPath = "/v1/validate-action"

type validateActionRequest struct {
	// ActionType names the rule the action rests on: "<rule_domain>.<rule_code>",
	// matched exactly (decision J-06, 21 Oct 2026), e.g. "TAX.STD".
	ActionType     string `json:"action_type"`
	JurisdictionID string `json:"jurisdiction_id"`
	// EntityType is carried for the caller's record; rules have no entity-type
	// applicability to filter on.
	EntityType  string          `json:"entity_type"`
	EffectiveAt *time.Time      `json:"effective_at"`
	Payload     json.RawMessage `json:"payload,omitempty"`
}

type validateActionResponse struct {
	Permitted    bool                     `json:"permitted"`
	Detail       string                   `json:"detail,omitempty"`
	ActionType   string                   `json:"action_type"`
	EntityType   string                   `json:"entity_type"`
	Rule         *domain.JurisdictionRule `json:"rule,omitempty"`
	ResolvedFrom []string                 `json:"resolved_from,omitempty"`
	Conflicts    []domain.RuleConflict    `json:"conflicts,omitempty"`
	Insufficient []string                 `json:"insufficient,omitempty"`
	EffectiveAt  time.Time                `json:"effective_at"`
	// PackDigest pins the rule basis; the caller records it with its action.
	PackDigest string `json:"pack_digest"`
}

// splitActionType parses "<rule_domain>.<rule_code>". The domain holds no dot;
// the code is everything after the first one.
func splitActionType(s string) (ruleDomain, ruleCode string, ok bool) {
	ruleDomain, ruleCode, found := strings.Cut(strings.TrimSpace(s), ".")
	if !found || ruleDomain == "" || ruleCode == "" {
		return "", "", false
	}
	return ruleDomain, ruleCode, true
}

// ValidateAction handles POST /v1/validate-action: the synchronous check a
// service makes before a governed action. It resolves the jurisdiction's rule
// pack for the action's rule (rule_domain, required_codes=[rule_code]) at
// effective_at and answers with the one rule that governs it.
//
//	200 → permitted, with the rule, the chain it resolved from and pack_digest
//	409 → not permitted: detail conflicted (equal-precedence rules overlap) or
//	      insufficient (no rule anywhere in the chain)
//	400 → malformed body or action_type
//	401 / 403 → no caller identity / JURISDICTION_RULE_VALIDATE denied
//	404 → unknown or inactive jurisdiction
//	503 → authorization or store unavailable
func (h *Handler) ValidateAction(w http.ResponseWriter, r *http.Request) {
	correlationID := r.Header.Get("X-Correlation-ID")
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	if err := h.checkAuthz(r, principalID, "jurisdiction_rule", "validate"); err != nil {
		h.writeAuthzError(w, r, err)
		return
	}

	var req validateActionRequest
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		writeError(w, r, http.StatusBadRequest, "invalid_json", err.Error())
		return
	}
	switch {
	case strings.TrimSpace(req.JurisdictionID) == "":
		writeError(w, r, http.StatusBadRequest, "missing_field", "jurisdiction_id is required")
		return
	case strings.TrimSpace(req.EntityType) == "":
		writeError(w, r, http.StatusBadRequest, "missing_field", "entity_type is required")
		return
	case len(req.Payload) > 0 && !bytes.HasPrefix(bytes.TrimSpace(req.Payload), []byte("{")):
		writeError(w, r, http.StatusBadRequest, "invalid_field", "payload must be a JSON object")
		return
	}
	ruleDomain, ruleCode, ok := splitActionType(req.ActionType)
	if !ok {
		writeError(w, r, http.StatusBadRequest, "invalid_action_type",
			"action_type must be <rule_domain>.<rule_code>, e.g. TAX.STD")
		return
	}
	at := time.Now().UTC()
	if req.EffectiveAt != nil {
		at = req.EffectiveAt.UTC()
	}

	pack, err := h.store.ResolveRulePack(r.Context(), domain.RulePackQuery{
		JurisdictionID: req.JurisdictionID,
		RuleDomain:     ruleDomain,
		At:             at,
		RequiredCodes:  []string{ruleCode},
	})
	if err != nil {
		h.writeStoreError(w, r, err, correlationID)
		return
	}

	resp := validateActionResponse{ActionType: req.ActionType, EntityType: req.EntityType, EffectiveAt: at,
		ResolvedFrom: pack.ResolvedFrom, PackDigest: pack.PackDigest}
	for _, c := range pack.Conflicts {
		if c.RuleCode == ruleCode {
			resp.Conflicts = append(resp.Conflicts, c)
		}
	}
	for _, rule := range pack.Rules {
		if rule.RuleCode == ruleCode {
			resp.Rule = rule
			break
		}
	}
	status := http.StatusOK
	switch {
	case len(resp.Conflicts) > 0:
		// An equal-precedence overlap: no caller may act on it without a
		// human resolution, even if one of the rules is in the pack.
		resp.Detail, resp.Rule, status = "conflicted", nil, http.StatusConflict
	case resp.Rule == nil:
		resp.Detail, resp.Insufficient, status = "insufficient", []string{ruleDomain + "." + ruleCode}, http.StatusConflict
	default:
		resp.Permitted = true
	}

	h.log.Info("ValidateAction",
		zap.String("action_type", req.ActionType), zap.String("jurisdiction_id", req.JurisdictionID),
		zap.Bool("permitted", resp.Permitted), zap.String("detail", resp.Detail),
		zap.String("pack_digest", resp.PackDigest), zap.String("principal_id", principalID),
		zap.String("correlation_id", correlationID))
	writeJSON(w, status, resp)
}

