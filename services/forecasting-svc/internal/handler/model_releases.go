package handler

import (
	"net/http"

	"github.com/go-chi/chi/v5"

	"zoiko.io/forecasting-svc/internal/domain"
)

// ─────────────────────────────────────────────────────────────────────────────
// AI-05 model release handlers.
// ─────────────────────────────────────────────────────────────────────────────

func writeModelRelease(w http.ResponseWriter, status int, release *domain.ForecastModelRelease) {
	w.Header().Set("ETag", etagFor(release.Version))
	respondJSON(w, status, release)
}

// RegisterModelRelease handles POST /v1/forecast-assist/model-releases.
//
// Body: domain_name, model_provider, model_version (all required).
// Writes: forecast_model_releases in status PendingEvaluation. The release
// cannot be used until a different principal approves its evaluation.
// Event: none (configuration record; no catalogue event defined).
// Success: 201 + ETag. Errors: 400 malformed/empty body, 401, 403 authz,
// 409 already registered / key reused, 422 validation.
func (h *Handler) RegisterModelRelease(w http.ResponseWriter, r *http.Request) {
	actorID, ok := h.requireActor(w, r)
	if !ok {
		return
	}
	rawBody, ok := readBody(w, r, true)
	if !ok {
		return
	}
	var req domain.RegisterModelReleaseRequest
	if !decodeJSON(w, r, rawBody, &req) || !h.validate(w, r, &req) {
		return
	}
	if !h.authorize(w, r, actorID, authorizationScope(r, ""), ActionModelReleaseRegister) {
		return
	}

	cmd := buildCommand(r, actorID, "model_release.register", "", rawBody, 0)
	release, err := h.store.RegisterModelRelease(r.Context(), cmd, req)
	if replay, isReplay := asReplay(w, err); isReplay {
		release, err = h.store.GetModelRelease(r.Context(), cmd.TenantID, replay.ResourceID)
	}
	if err != nil {
		h.writeError(w, r, err)
		return
	}
	w.Header().Set("Location", "/v1/forecast-assist/model-releases/"+release.ReleaseID)
	writeModelRelease(w, http.StatusCreated, release)
}

// ListModelReleases handles GET /v1/forecast-assist/model-releases.
//
// Query: status (PendingEvaluation|Approved), limit, cursor.
// Success: 200 {data, count, next_cursor}. Errors: 400, 401, 403.
func (h *Handler) ListModelReleases(w http.ResponseWriter, r *http.Request) {
	actorID, ok := h.requireActor(w, r)
	if !ok {
		return
	}
	filter := domain.ModelReleaseListFilter{Status: domain.ModelReleaseStatus(r.URL.Query().Get("status"))}
	if filter.Status != "" && !filter.Status.Valid() {
		invalidQuery(w, r, "status must be PendingEvaluation or Approved")
		return
	}
	page, ok := parsePageRequest(w, r)
	if !ok {
		return
	}
	filter.Page = page
	if !h.authorize(w, r, actorID, authorizationScope(r, ""), ActionAssistRead) {
		return
	}
	releases, nextCursor, err := h.store.ListModelReleases(r.Context(), tenantOf(r), filter)
	if err != nil {
		h.writeError(w, r, err)
		return
	}
	respondJSON(w, http.StatusOK, listResponse{Data: releases, Count: len(releases), NextCursor: nextCursor})
}

// GetModelRelease handles GET /v1/forecast-assist/model-releases/{releaseID}.
//
// Success: 200 + ETag. Errors: 400 bad id (must be fmr_<uuid>), 401, 403, 404.
func (h *Handler) GetModelRelease(w http.ResponseWriter, r *http.Request) {
	actorID, ok := h.requireActor(w, r)
	if !ok {
		return
	}
	releaseID := chi.URLParam(r, "releaseID")
	if !requirePrefixedPathParam(w, r, "releaseID", releaseID, domain.PrefixForecastModelRelease) {
		return
	}
	if !h.authorize(w, r, actorID, authorizationScope(r, ""), ActionAssistRead) {
		return
	}
	release, err := h.store.GetModelRelease(r.Context(), tenantOf(r), releaseID)
	if err != nil {
		h.writeError(w, r, err)
		return
	}
	writeModelRelease(w, http.StatusOK, release)
}

// ApproveModelRelease handles POST /v1/forecast-assist/model-releases/{releaseID}:approve.
//
// Requires If-Match. Body: evaluation_reference (required, the evaluation
// evidence). PendingEvaluation -> Approved. Segregation of duties: the
// registering principal cannot approve (403 segregation_of_duties).
// Success: 200 + ETag. Errors: 400, 401, 403, 404, 409 already approved,
// 412 stale If-Match, 422, 428 If-Match missing.
func (h *Handler) ApproveModelRelease(w http.ResponseWriter, r *http.Request) {
	actorID, ok := h.requireActor(w, r)
	if !ok {
		return
	}
	releaseID := chi.URLParam(r, "releaseID")
	if !requirePrefixedPathParam(w, r, "releaseID", releaseID, domain.PrefixForecastModelRelease) {
		return
	}
	expectedVersion, ok := requireIfMatch(w, r)
	if !ok {
		return
	}
	rawBody, ok := readBody(w, r, true)
	if !ok {
		return
	}
	var req domain.ApproveModelReleaseRequest
	if !decodeJSON(w, r, rawBody, &req) || !h.validate(w, r, &req) {
		return
	}
	if !h.authorize(w, r, actorID, authorizationScope(r, ""), ActionModelReleaseApprove) {
		return
	}

	cmd := buildCommand(r, actorID, "model_release.approve", releaseID, rawBody, expectedVersion)
	release, err := h.store.ApproveModelRelease(r.Context(), cmd, releaseID, req)
	if replay, isReplay := asReplay(w, err); isReplay {
		release, err = h.store.GetModelRelease(r.Context(), cmd.TenantID, replay.ResourceID)
	}
	if err != nil {
		h.writeError(w, r, err)
		return
	}
	writeModelRelease(w, http.StatusOK, release)
}
