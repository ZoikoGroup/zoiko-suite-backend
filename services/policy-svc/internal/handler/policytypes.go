package handler

import (
	"context"
	"net/http"
	"sort"
	"time"

	"github.com/go-chi/chi/v5"
	"zoiko.io/identity-envelope/govdecision"

	"zoiko.io/policy-svc/internal/authz"
	"zoiko.io/policy-svc/internal/domain"
	"zoiko.io/policy-svc/internal/policytype"
)

// familyStatusStore is the store's status-card read, optional like the
// register reads.
type familyStatusStore interface {
	FamilyStatuses(ctx context.Context) (map[string]*domain.FamilyStatus, error)
}

// Display states of a family card (ZS-GOV-POL-001 §9.1: "Not implemented"
// and "Configuration required" are blocking states, never green success).
const (
	DisplayEvaluable             = "EVALUABLE"
	DisplayConfigurationRequired = "CONFIGURATION_REQUIRED"
	DisplayDecidedByOwner        = "DECIDED_BY_OWNING_SERVICE"
	DisplayNotImplemented        = "NOT_IMPLEMENTED"
)

// policyTypeCard is one family: its catalogue entry and its status for the
// caller's tenant.
type policyTypeCard struct {
	policytype.Info
	DisplayState string `json:"display_state"`
	// Blocking is true when a governed action of this family cannot proceed
	// on policy-svc's word: not configured, or not decided here.
	Blocking bool                 `json:"blocking"`
	Message  string               `json:"message"`
	Status   *domain.FamilyStatus `json:"status"`
}

type policyTypesResponse struct {
	TenantID           string           `json:"tenant_id"`
	AsOf               time.Time        `json:"as_of"`
	ContractVersion    string           `json:"contract_version"`
	ReasonCodesVersion string           `json:"reason_codes_version"`
	Outcomes           []string         `json:"outcomes"`
	Items              []policyTypeCard `json:"items"`
}

// RegisterPolicyTypeRoutes mounts the family catalogue.
func RegisterPolicyTypeRoutes(r chi.Router, h *Handler) {
	r.Get("/v1/policy-types", h.ListPolicyTypes)
}

// ListPolicyTypes handles GET /v1/policy-types: one authoritative status card
// per policy family (ZS-GOV-POL-001 §9): owner, evaluation mode, the versions
// that govern with their effective dates, approval and activation trail, and
// the last day's decisions by outcome. A family type found in the data but
// unknown to this service is listed as NOT_IMPLEMENTED.
func (h *Handler) ListPolicyTypes(w http.ResponseWriter, r *http.Request) {
	tenantID, ok := h.requireTenant(w, r)
	if !ok {
		return
	}
	if _, ok := h.gate(w, r, h.opts.ReadAuthz, authz.ActionPolicyRead, nil); !ok {
		return
	}
	s, ok := h.store.(familyStatusStore)
	if !ok {
		writeError(w, r, http.StatusServiceUnavailable, codeUnavailable, "store_unavailable", "the store does not serve family status", nil)
		return
	}
	statuses, err := s.FamilyStatuses(r.Context())
	if err != nil {
		h.storeUnavailable(w, r, "ListPolicyTypes", err)
		return
	}
	resp := policyTypesResponse{
		TenantID: tenantID, AsOf: h.opts.Now().UTC(),
		ContractVersion: govdecision.ContractVersion, ReasonCodesVersion: govdecision.ReasonCodesVersion,
		Outcomes: govdecision.Outcomes,
	}
	seen := map[string]bool{}
	for _, info := range policytype.All() {
		seen[info.PolicyType] = true
		resp.Items = append(resp.Items, card(info, statuses[info.PolicyType]))
	}
	var unknown []string
	for t := range statuses {
		if !seen[t] {
			unknown = append(unknown, t)
		}
	}
	sort.Strings(unknown)
	for _, t := range unknown {
		resp.Items = append(resp.Items, card(policytype.Info{PolicyType: t, Mode: "UNSUPPORTED", EnforcedBy: []string{}}, statuses[t]))
	}
	writeJSON(w, http.StatusOK, resp)
}

func card(info policytype.Info, st *domain.FamilyStatus) policyTypeCard {
	if st == nil {
		st = &domain.FamilyStatus{PolicyType: info.PolicyType, VersionsByStatus: map[string]int64{},
			GoverningVersions: []domain.GoverningVersion{}, Decisions24h: map[string]int64{}}
	}
	if info.EnforcedBy == nil {
		info.EnforcedBy = []string{}
	}
	c := policyTypeCard{Info: info, Status: st}
	governs := st.VersionsByStatus[domain.StatusActive] > 0
	switch {
	case info.Mode == policytype.ModeReferenceOnly:
		c.DisplayState, c.Blocking = DisplayDecidedByOwner, true
		c.Message = "Not implemented here: " + info.OwningService + " decides this at runtime. Versions here are governed references only."
		if !governs {
			c.Message += " No reference version is active."
		}
	case info.Mode == "UNSUPPORTED":
		c.DisplayState, c.Blocking = DisplayNotImplemented, true
		c.Message = "Not implemented: no service evaluates this policy type; every request for it is refused."
	case !governs:
		c.DisplayState, c.Blocking = DisplayConfigurationRequired, true
		c.Message = "Configuration required: no active version, so every governed request is held (NOT_CONFIGURED)."
	default:
		c.DisplayState = DisplayEvaluable
		c.Message = "Evaluated by policy-svc under the active version(s) listed."
	}
	return c
}
