package handler

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"

	"github.com/go-chi/chi/v5"

	"zoiko.io/data-ingestion-svc/internal/domain"
	customMiddleware "zoiko.io/data-ingestion-svc/internal/middleware"
)

// Governed release of run-level quarantine (INT-04; ZS-SVC-Z-001 DQC-02, NP-16). Asking needs the permission to manage
// ingestion; deciding needs its own, the same one that decides a release of an imported item, and the decider must be
// neither the requester nor (for an approval) whoever quarantined the batch.
const ActionRunQuarantineRelease = "DATA_IMPORT_QUARANTINE_RELEASE"

func registerRunReleaseRoutes(r chi.Router, h *Handler) {
	r.Post("/runs/{runID}/quarantine-items/{quarantineID}:request-release", h.requestRunRelease)
	r.Post("/runs/{runID}/quarantine-items/{quarantineID}:approve-release", func(w http.ResponseWriter, r *http.Request) { h.decideRunRelease(w, r, true) })
	r.Post("/runs/{runID}/quarantine-items/{quarantineID}:refuse-release", func(w http.ResponseWriter, r *http.Request) { h.decideRunRelease(w, r, false) })
	r.Get("/runs/{runID}/quarantine-items/{quarantineID}/release", h.runReleaseLedger)
}

// strictJSON decodes a body refusing unknown fields, and returns its hash for the idempotency claim.
func strictJSON(r *http.Request, v any) (string, error) {
	raw, err := io.ReadAll(r.Body)
	if err != nil {
		return "", err
	}
	if len(bytes.TrimSpace(raw)) == 0 {
		return "", errors.New("a request body is required")
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return "", err
	}
	if dec.More() {
		return "", errors.New("unexpected data after the JSON body")
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:]), nil
}

func (h *Handler) requestRunRelease(w http.ResponseWriter, r *http.Request) {
	tenantID := customMiddleware.GetTenantID(r.Context())
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	if !h.authorize(w, r, principalID, tenantID, ActionIngestionManage) {
		return
	}
	var req domain.RunReleaseRequest
	hash, err := strictJSON(r, &req)
	if err != nil {
		h.respondError(w, http.StatusBadRequest, "invalid request body: "+err.Error())
		return
	}
	runID, qid := chi.URLParam(r, "runID"), chi.URLParam(r, "quarantineID")
	claim := h.idempotencyClaim(principalID, "RequestRunRelease", qid, hash, r.Header.Get("Idempotency-Key"))
	if err := h.store.RequestRunRelease(r.Context(), tenantID, runID, qid, principalID, req, claim); err != nil {
		h.respondFromError(w, err)
		return
	}
	h.respondJSON(w, http.StatusAccepted, map[string]any{"quarantine_id": qid, "release_state": domain.RunReleasePending})
}

func (h *Handler) decideRunRelease(w http.ResponseWriter, r *http.Request, release bool) {
	tenantID := customMiddleware.GetTenantID(r.Context())
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	if !h.authorize(w, r, principalID, tenantID, ActionRunQuarantineRelease) {
		return
	}
	var dec domain.RunReleaseDecision
	hash, err := strictJSON(r, &dec)
	if err != nil {
		h.respondError(w, http.StatusBadRequest, "invalid request body: "+err.Error())
		return
	}
	runID, qid := chi.URLParam(r, "runID"), chi.URLParam(r, "quarantineID")
	op := "RefuseRunRelease"
	if release {
		op = "ApproveRunRelease"
	}
	claim := h.idempotencyClaim(principalID, op, qid, hash, r.Header.Get("Idempotency-Key"))
	if err := h.store.DecideRunRelease(r.Context(), tenantID, runID, qid, principalID, release, dec, claim); err != nil {
		h.respondFromError(w, err)
		return
	}
	state := domain.RunQuarantined
	if release {
		state = domain.RunReleaseApprovedS
	}
	h.respondJSON(w, http.StatusOK, map[string]any{"quarantine_id": qid, "release_state": state})
}

func (h *Handler) runReleaseLedger(w http.ResponseWriter, r *http.Request) {
	tenantID := customMiddleware.GetTenantID(r.Context())
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	if !h.authorize(w, r, principalID, tenantID, ActionIngestionRead) {
		return
	}
	entries, state, err := h.store.RunQuarantineLedger(r.Context(), tenantID, chi.URLParam(r, "runID"), chi.URLParam(r, "quarantineID"))
	if err != nil {
		h.respondFromError(w, err)
		return
	}
	h.respondJSON(w, http.StatusOK, map[string]any{"release_state": state, "ledger": entries})
}

// runReleaseError maps the errors this feature adds. It reports whether it handled err.
func (h *Handler) runReleaseError(w http.ResponseWriter, err error) bool {
	switch {
	case errors.Is(err, domain.ErrRunReleaseInvalid):
		h.respondError(w, http.StatusBadRequest, err.Error())
	case errors.Is(err, domain.ErrRunReleaseNotIndependent):
		h.respondError(w, http.StatusForbidden, err.Error())
	case errors.Is(err, domain.ErrRunReleaseOpen), errors.Is(err, domain.ErrRunNoOpenRelease), errors.Is(err, domain.ErrRunReleaseAlreadyReleased),
		errors.Is(err, domain.ErrRunReleaseRequired), errors.Is(err, domain.ErrRunReleaseNotApproved), errors.Is(err, domain.ErrRunReleaseNotACorrection):
		h.respondError(w, http.StatusConflict, err.Error())
	default:
		return false
	}
	return true
}
