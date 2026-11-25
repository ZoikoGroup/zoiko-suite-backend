package handler

import (
	"errors"
	"net/http"

	"github.com/go-chi/chi/v5"

	"zoiko.io/data-quality-svc/internal/domain"
)

// The specification's /dqc/* routes (ZS-SVC-Z-001 section 11), alongside the service's own. They use the same
// permissions, the same store methods and the same rules as the routes they correspond to:
//
//	POST /dqc/observations     record that an asset was observed over a window at a source watermark
//	POST /dqc/validate         judge frozen evidence against an exact PUBLISHED contract (RunDQ, contract only)
//	POST /dqc/certifications   certify a run (CertifyDQ), naming the run in the body
//	POST /dqc/exceptions       raise an exception on a failing result (RaiseIssue)
//
// What each adds is the section 11 idempotency rule: the same observation, the same evidence, the same
// failing result, or the same certification basis returns the existing record instead of a second one.

func registerFacadeRoutes(r chi.Router, h *Handler) {
	r.Post("/dqc/observations", h.RecordObservation)
	r.Get("/dqc/observations/{observationID}", h.GetObservation)
	r.Post("/dqc/validate", h.DQCValidate)
	r.Post("/dqc/certifications", h.DQCCertify)
	r.Post("/dqc/exceptions", h.DQCRaiseException)
}

func createdOrExisting(deduplicated bool) int {
	if deduplicated {
		return http.StatusOK
	}
	return http.StatusCreated
}

func (h *Handler) RecordObservation(w http.ResponseWriter, r *http.Request) {
	tenant, principal, ok := h.gate(w, r, ActionDQManage)
	if !ok {
		return
	}
	var req domain.RecordObservationRequest
	hash, err := strictBody(r, &req, false)
	if err != nil {
		h.respondError(w, http.StatusBadRequest, "invalid request body: "+err.Error())
		return
	}
	claim := h.idempotencyClaim(principal, "RecordObservation", req.AssetID, hash, r.Header.Get("Idempotency-Key"))
	got, err := h.store.RecordObservation(r.Context(), tenant, req, principal, claim)
	if err != nil {
		h.respondFromError(w, err)
		return
	}
	h.respondJSON(w, createdOrExisting(got.Deduplicated), got)
}

func (h *Handler) GetObservation(w http.ResponseWriter, r *http.Request) {
	tenant, _, ok := h.gate(w, r, ActionDQRead)
	if !ok {
		return
	}
	got, err := h.store.GetObservation(r.Context(), tenant, chi.URLParam(r, "observationID"))
	if err != nil {
		h.respondFromError(w, err)
		return
	}
	h.respondJSON(w, http.StatusOK, got)
}

// DQCValidate is RunDQ with the contract required: validation evaluates against an exact DQC-01 contract.
func (h *Handler) DQCValidate(w http.ResponseWriter, r *http.Request) {
	tenant, principal, ok := h.gate(w, r, ActionDQManage)
	if !ok {
		return
	}
	var req domain.RunDQRequest
	hash, err := strictBody(r, &req, false)
	if err != nil {
		h.respondError(w, http.StatusBadRequest, "invalid request body: "+err.Error())
		return
	}
	if req.ContractAssetID == "" || req.ContractVersion < 1 || req.RuleSetVersionID != "" {
		h.respondError(w, http.StatusBadRequest, "validate is judged against an exact contract: give contract_asset_id and contract_version, and no ruleset_version_id")
		return
	}
	claim := h.idempotencyClaim(principal, "RunDQ", req.PopulationRef, hash, r.Header.Get("Idempotency-Key"))
	got, err := h.store.RunDQ(r.Context(), tenant, req, principal, claim)
	if err != nil {
		h.respondFromError(w, err)
		return
	}
	h.respondJSON(w, createdOrExisting(got.Deduplicated), got)
}

type dqcCertifyRequest struct {
	RunID string `json:"run_id"`
	domain.CertifyDQRequest
}

func (h *Handler) DQCCertify(w http.ResponseWriter, r *http.Request) {
	tenant, principal, ok := h.gate(w, r, ActionDQManage)
	if !ok {
		return
	}
	var req dqcCertifyRequest
	hash, err := strictBody(r, &req, false)
	if err != nil {
		h.respondError(w, http.StatusBadRequest, "invalid request body: "+err.Error())
		return
	}
	if req.RunID == "" {
		h.respondError(w, http.StatusBadRequest, "run_id is required")
		return
	}
	claim := h.idempotencyClaim(principal, "CertifyDQ", req.RunID, hash, r.Header.Get("Idempotency-Key"))
	got, err := h.store.CertifyDQ(r.Context(), tenant, req.RunID, principal, req.CertifyDQRequest, claim)
	if err != nil {
		h.respondFromError(w, err)
		return
	}
	h.respondJSON(w, http.StatusCreated, got)
}

func (h *Handler) DQCRaiseException(w http.ResponseWriter, r *http.Request) {
	tenant, principal, ok := h.gate(w, r, ActionDQManage)
	if !ok {
		return
	}
	var req domain.RaiseIssueRequest
	hash, err := strictBody(r, &req, false)
	if err != nil {
		h.respondError(w, http.StatusBadRequest, "invalid request body: "+err.Error())
		return
	}
	claim := h.idempotencyClaim(principal, "RaiseIssue", req.ResultID, hash, r.Header.Get("Idempotency-Key"))
	got, err := h.store.RaiseIssue(r.Context(), tenant, req, principal, claim)
	if err != nil {
		h.respondFromError(w, err)
		return
	}
	h.respondJSON(w, createdOrExisting(got.Deduplicated), got)
}

// facadeError maps the errors the /dqc/* routes add. It reports whether it handled err.
func (h *Handler) facadeError(w http.ResponseWriter, err error) bool {
	var basis *domain.BasisExistsError
	switch {
	case errors.As(err, &basis):
		// "Conflict returns existing certification or 409": the 409 names the existing certification.
		h.respondJSON(w, http.StatusConflict, map[string]any{"error": err.Error(), "code": "DQC-015", "existing_certification_id": basis.ExistingID})
	case errors.Is(err, domain.ErrObservationNotFound):
		h.respondError(w, http.StatusNotFound, err.Error())
	case errors.Is(err, domain.ErrObservationConflict), errors.Is(err, domain.ErrEvidenceAlreadyRecorded):
		h.respondError(w, http.StatusConflict, err.Error())
	default:
		return false
	}
	return true
}
