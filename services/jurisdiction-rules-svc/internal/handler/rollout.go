package handler

import (
	"errors"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"

	"zoiko.io/jurisdiction-rules-svc/internal/domain"
	"zoiko.io/jurisdiction-rules-svc/internal/store"
)

// ZS-JUR-001 Wave 8: jurisdiction rollout governance (s32, s35, s36, s37).
//
// The standard defines the architecture, not the substantive local law. Naming a
// jurisdiction in the portfolio does NOT mean certified rules exist: production
// support begins only when a rollout has met its Definition of Ready and Done
// with independent attestations, a qualified local expert has approved it, and a
// linked pack has a RELEASED version. This is governance only: no regulatory
// content is created here.

func registerRolloutRoutes(r chi.Router, h *Handler) {
	r.Post("/v1/admin/rollouts", h.CreateRollout)
	r.Get("/v1/admin/rollouts", h.ListRollouts)
	r.Post("/v1/admin/rollouts:seed-portfolio", h.SeedPortfolio)
	r.Get("/v1/admin/rollouts/{rollout_id}", h.GetRollout)
	r.Put("/v1/admin/rollouts/{rollout_id}/owner", h.SetRolloutOwner)
	r.Put("/v1/admin/rollouts/{rollout_id}/packs", h.LinkRolloutPacks)
	r.Post("/v1/admin/rollouts/{rollout_id}/attestations", h.AttestRollout)
	r.Post("/v1/admin/rollouts/{rollout_id}/expert-approvals", h.ExpertApproveRollout)
	r.Post("/v1/admin/rollouts/{rollout_id}/transition", h.TransitionRollout)
	r.Get("/v1/rollout-checklists", h.RolloutChecklists)
	r.Get("/v1/jurisdiction-support/{jurisdiction_code}", h.JurisdictionSupport)
}

func (h *Handler) writeRolloutError(w http.ResponseWriter, err error, corr string) {
	var blocked *domain.RolloutBlockedError
	switch {
	case errors.As(err, &blocked):
		writeJSON(w, http.StatusConflict, map[string]any{"error": "rollout_blocked", "message": blocked.Error(), "blockers": blocked.Blockers})
	case errors.Is(err, domain.ErrRolloutNotFound):
		writeError(w, http.StatusNotFound, "rollout_not_found", err.Error())
	default:
		// The database independence and gate triggers surface as a conflict or a segregation-of-duties refusal.
		if strings.Contains(err.Error(), "ck_rollout_independent") || strings.Contains(err.Error(), "cannot attest to or approve their own") {
			writeError(w, http.StatusForbidden, "segregation_of_duties", "the rollout owner cannot attest to or approve their own rollout")
			return
		}
		h.writeRegistryError(w, err, corr)
	}
}

type CreateRolloutRequest struct {
	FamilyRef        string  `json:"family_ref"`
	DisplayName      string  `json:"display_name"`
	Layer            string  `json:"layer"`
	JurisdictionCode *string `json:"jurisdiction_code"`
	ScopeNote        string  `json:"scope_note"`
	Owner            string  `json:"owner"`
	SupportOwner     *string `json:"support_owner"`
}

// CreateRollout adds one portfolio entry in PLANNED.
func (h *Handler) CreateRollout(w http.ResponseWriter, r *http.Request) {
	actor, ok := h.admin(w, r, "jurisdiction_rollout", "create")
	if !ok {
		return
	}
	var req CreateRolloutRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if field := firstBlank(requiredField{"family_ref", req.FamilyRef}, requiredField{"display_name", req.DisplayName},
		requiredField{"layer", req.Layer}, requiredField{"owner", req.Owner}); field != "" {
		writeMissingField(w, field)
		return
	}
	if !domain.ValidFamilyRef(req.FamilyRef) {
		writeError(w, http.StatusBadRequest, "invalid_family_ref", "family_ref must be lower-case letters, digits, dot, underscore or hyphen, up to 64 characters (for example gb or us.state.ca)")
		return
	}
	if !contains(domain.RolloutLayers, req.Layer) {
		writeError(w, http.StatusBadRequest, "invalid_layer", "layer must be one of "+strings.Join(domain.RolloutLayers, ", "))
		return
	}
	if (req.Layer == "COUNTRY" || req.Layer == "SUBDIVISION") && (req.JurisdictionCode == nil || strings.TrimSpace(*req.JurisdictionCode) == "") {
		writeError(w, http.StatusBadRequest, "jurisdiction_code_required", "a COUNTRY or SUBDIVISION rollout must name its jurisdiction_code")
		return
	}
	rec, err := h.registry.CreateRollout(r.Context(), store.CreateRolloutParams{FamilyRef: req.FamilyRef, DisplayName: req.DisplayName, Layer: req.Layer,
		ScopeNote: req.ScopeNote, Owner: req.Owner, CreatedBy: actor, JurisdictionCode: req.JurisdictionCode, SupportOwner: req.SupportOwner})
	if err != nil {
		h.writeRolloutError(w, err, r.Header.Get("X-Correlation-ID"))
		return
	}
	writeJSON(w, http.StatusCreated, rec)
}

// SeedPortfolio adds the s32 initial portfolio as PLANNED entries with no owner. Idempotent.
func (h *Handler) SeedPortfolio(w http.ResponseWriter, r *http.Request) {
	actor, ok := h.admin(w, r, "jurisdiction_rollout", "seed")
	if !ok {
		return
	}
	created, existing, err := h.registry.SeedPortfolio(r.Context(), actor)
	if err != nil {
		h.writeRolloutError(w, err, r.Header.Get("X-Correlation-ID"))
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"created": created, "already_present": existing,
		"note": "seeding states intent only: a PLANNED entry does not mean certified rules exist for that jurisdiction"})
}

func (h *Handler) ListRollouts(w http.ResponseWriter, r *http.Request) {
	if _, ok := h.admin(w, r, "jurisdiction_rollout", "view"); !ok {
		return
	}
	limit, offset, ok := h.parsePaging(w, r.URL.Query())
	if !ok {
		return
	}
	status := strings.ToUpper(r.URL.Query().Get("status"))
	if status != "" && !domain.ValidRolloutStatus(status) {
		writeError(w, http.StatusBadRequest, "invalid_status", "unknown rollout status")
		return
	}
	out, err := h.registry.ListRollouts(r.Context(), status, limit, offset)
	if err != nil {
		h.writeRolloutError(w, err, r.Header.Get("X-Correlation-ID"))
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"rollouts": out})
}

// GetRollout returns a rollout with its checklist state, expert decisions, packs, history and what blocks each next step.
func (h *Handler) GetRollout(w http.ResponseWriter, r *http.Request) {
	if _, ok := h.admin(w, r, "jurisdiction_rollout", "view"); !ok {
		return
	}
	d, err := h.registry.GetRollout(r.Context(), chi.URLParam(r, "rollout_id"))
	if err != nil {
		h.writeRolloutError(w, err, r.Header.Get("X-Correlation-ID"))
		return
	}
	writeJSON(w, http.StatusOK, d)
}

type SetRolloutOwnerRequest struct {
	Owner        string  `json:"owner"`
	SupportOwner *string `json:"support_owner"`
}

// SetRolloutOwner assigns the accountable owner (DOR_09: operational owner and support model).
func (h *Handler) SetRolloutOwner(w http.ResponseWriter, r *http.Request) {
	if _, ok := h.admin(w, r, "jurisdiction_rollout", "set_owner"); !ok {
		return
	}
	var req SetRolloutOwnerRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if strings.TrimSpace(req.Owner) == "" || req.Owner == domain.UnassignedOwner {
		writeMissingField(w, "owner")
		return
	}
	rec, err := h.registry.SetRolloutOwner(r.Context(), chi.URLParam(r, "rollout_id"), req.Owner, req.SupportOwner)
	if err != nil {
		h.writeRolloutError(w, err, r.Header.Get("X-Correlation-ID"))
		return
	}
	writeJSON(w, http.StatusOK, rec)
}

type LinkRolloutPacksRequest struct {
	PackRefs []string `json:"pack_refs"`
}

// LinkRolloutPacks links the packs that make up the rollout (additive, idempotent).
func (h *Handler) LinkRolloutPacks(w http.ResponseWriter, r *http.Request) {
	actor, ok := h.admin(w, r, "jurisdiction_rollout", "link_packs")
	if !ok {
		return
	}
	var req LinkRolloutPacksRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if len(req.PackRefs) == 0 || len(req.PackRefs) > 50 {
		writeError(w, http.StatusBadRequest, "invalid_pack_refs", "pack_refs needs 1 to 50 pack references")
		return
	}
	id := chi.URLParam(r, "rollout_id")
	if err := h.registry.LinkRolloutPacks(r.Context(), id, req.PackRefs, actor); err != nil {
		h.writeRolloutError(w, err, r.Header.Get("X-Correlation-ID"))
		return
	}
	d, err := h.registry.GetRollout(r.Context(), id)
	if err != nil {
		h.writeRolloutError(w, err, r.Header.Get("X-Correlation-ID"))
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"pack_refs": d.Packs})
}

type AttestRolloutRequest struct {
	// Checklist is READY (Definition of Ready, DOR_01..10) or DONE (Definition of Done, DOD_01..11).
	Checklist   string `json:"checklist"`
	ItemCode    string `json:"item_code"`
	Met         bool   `json:"met"`
	EvidenceRef string `json:"evidence_ref"`
}

// AttestRollout records one checklist answer. A met item needs an evidence reference and an attester who is
// not the rollout owner. The latest answer for an item is current; earlier answers stay as evidence.
func (h *Handler) AttestRollout(w http.ResponseWriter, r *http.Request) {
	actor, ok := h.admin(w, r, "jurisdiction_rollout", "attest")
	if !ok {
		return
	}
	var req AttestRolloutRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if !domain.ChecklistHasItem(req.Checklist, req.ItemCode) {
		writeError(w, http.StatusBadRequest, "invalid_item", "checklist must be READY or DONE and item_code one of its items (see GET /v1/rollout-checklists)")
		return
	}
	if req.Met && strings.TrimSpace(req.EvidenceRef) == "" {
		writeError(w, http.StatusBadRequest, "evidence_required", "a met item needs an evidence_ref (a document, report or record that shows it)")
		return
	}
	if err := h.registry.AddAttestation(r.Context(), chi.URLParam(r, "rollout_id"), req.Checklist, req.ItemCode, req.Met, strings.TrimSpace(req.EvidenceRef), actor); err != nil {
		h.writeRolloutError(w, err, r.Header.Get("X-Correlation-ID"))
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"checklist": req.Checklist, "item_code": req.ItemCode, "met": req.Met, "attested_by": actor})
}

type ExpertApprovalRequest struct {
	Qualification string `json:"qualification"`
	Scope         string `json:"scope"`
	Decision      string `json:"decision"`
	Notes         string `json:"notes"`
}

// ExpertApproveRollout records a qualified local expert, legal or tax decision.
func (h *Handler) ExpertApproveRollout(w http.ResponseWriter, r *http.Request) {
	actor, ok := h.admin(w, r, "jurisdiction_rollout", "expert_approve")
	if !ok {
		return
	}
	var req ExpertApprovalRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if field := firstBlank(requiredField{"qualification", req.Qualification}, requiredField{"scope", req.Scope}, requiredField{"decision", req.Decision}); field != "" {
		writeMissingField(w, field)
		return
	}
	if req.Decision != "APPROVE" && req.Decision != "REJECT" {
		writeError(w, http.StatusBadRequest, "invalid_decision", "decision must be APPROVE or REJECT")
		return
	}
	if err := h.registry.AddExpertApproval(r.Context(), chi.URLParam(r, "rollout_id"), actor, strings.TrimSpace(req.Qualification), strings.TrimSpace(req.Scope), req.Decision, req.Notes); err != nil {
		h.writeRolloutError(w, err, r.Header.Get("X-Correlation-ID"))
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"expert": actor, "decision": req.Decision})
}

type TransitionRolloutRequest struct {
	To     string `json:"to"`
	Reason string `json:"reason"`
}

// TransitionRollout moves a rollout. A refused move returns 409 with every blocker.
func (h *Handler) TransitionRollout(w http.ResponseWriter, r *http.Request) {
	actor, ok := h.admin(w, r, "jurisdiction_rollout", "transition")
	if !ok {
		return
	}
	var req TransitionRolloutRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if !domain.ValidRolloutStatus(req.To) || req.To == domain.RolloutPlanned {
		writeError(w, http.StatusBadRequest, "invalid_status", "to must be one of AUTHORING, READY, LAUNCHED, SUSPENDED, RETIRED")
		return
	}
	rec, err := h.registry.TransitionRollout(r.Context(), chi.URLParam(r, "rollout_id"), req.To, actor, strings.TrimSpace(req.Reason))
	if err != nil {
		h.writeRolloutError(w, err, r.Header.Get("X-Correlation-ID"))
		return
	}
	writeJSON(w, http.StatusOK, rec)
}

// RolloutChecklists lists the Definition of Ready and Definition of Done items.
func (h *Handler) RolloutChecklists(w http.ResponseWriter, r *http.Request) {
	if _, ok := h.admin(w, r, "jurisdiction_rollout", "view"); !ok {
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"definition_of_ready": domain.ReadyChecklist, "definition_of_done": domain.DoneChecklist})
}

// JurisdictionSupport says whether a jurisdiction is supported for production, and if not, exactly why not.
// It never guesses: absence of a launched, released rollout is an explicit "not supported" (JUR-NEG-22).
func (h *Handler) JurisdictionSupport(w http.ResponseWriter, r *http.Request) {
	if _, ok := h.admin(w, r, "jurisdiction_support", "view"); !ok {
		return
	}
	code := strings.TrimSpace(chi.URLParam(r, "jurisdiction_code"))
	if code == "" || len(code) > 32 {
		writeError(w, http.StatusBadRequest, "invalid_jurisdiction_code", "jurisdiction_code is required (at most 32 characters)")
		return
	}
	v, err := h.registry.JurisdictionSupport(r.Context(), code)
	if err != nil {
		h.writeRolloutError(w, err, r.Header.Get("X-Correlation-ID"))
		return
	}
	writeJSON(w, http.StatusOK, v)
}
