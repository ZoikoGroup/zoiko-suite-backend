package handler

import (
	"net/http"

	"zoiko.io/anomaly-detection-svc/internal/domain"
)

// AI-04 AnomalyModel registry: /v1/anomaly-governance/models.

// RegisterAnomalyModel handles POST /v1/anomaly-governance/models.
//
// Validates domain_name, model_provider, model_version (required),
// review_threshold_bp and max_drift_bp (required, 0..10000) and drift_policy
// (BLOCK | ESCALATE | DISABLE, default BLOCK). Writes one Active
// anomaly_models row at version 1; the caller becomes its publisher
// (created_by / updated_by), which segregation of duties later uses. A
// second registration of the same domain/provider/model_version is 409 —
// change an existing model with PUT + If-Match. Emits no event.
//
// Success: 201 + Location + ETag. Errors: 400, 401, 403, 409, 422, 503.
func (h *Handler) RegisterAnomalyModel(w http.ResponseWriter, r *http.Request) {
	scope, ok := readScope(w, r)
	if !ok {
		return
	}
	idempotencyKey, ok := requireIdempotencyKey(w, r)
	if !ok {
		return
	}

	var request domain.RegisterAnomalyModelRequest
	body, ok := h.decodeBody(w, r, scope, &request, false)
	if !ok {
		return
	}
	if !h.validate(w, r, &request) {
		return
	}
	if !h.authorize(w, r, scope, ActionModelManage) {
		return
	}

	command := newCommand(r, scope, idempotencyKey, body, nil)
	result, err := h.store.RegisterAnomalyModel(r.Context(), command, request)
	if err != nil {
		h.writeError(w, r, err)
		return
	}

	model := result.Resource
	writeCommandResult(w, http.StatusCreated, result.Replayed, model.Version,
		"/v1/anomaly-governance/models/"+model.ModelID, model)
}

// ListAnomalyModels handles GET /v1/anomaly-governance/models.
//
// Optional filters ?domain_name= and ?status= (Active | Disabled); cursor
// pagination via ?limit= and ?cursor=.
//
// Success: 200 {"data": [...], "count": n, "next_cursor": "..."}.
// Errors: 400, 401, 403, 503.
func (h *Handler) ListAnomalyModels(w http.ResponseWriter, r *http.Request) {
	scope, ok := readScope(w, r)
	if !ok {
		return
	}
	status, ok := queryEnum(w, r, "status", func(v string) bool { return domain.ModelStatus(v).IsValid() })
	if !ok {
		return
	}
	page, ok := h.readPage(w, r)
	if !ok {
		return
	}
	if !h.authorize(w, r, scope, ActionGovernedRead) {
		return
	}

	filter := domain.ModelFilter{DomainName: r.URL.Query().Get("domain_name"), Status: domain.ModelStatus(status)}
	models, nextCursor, err := h.store.ListAnomalyModels(r.Context(), scope.TenantID, filter, page)
	if err != nil {
		h.writeError(w, r, err)
		return
	}
	writeList(w, models, nextCursor)
}

// GetAnomalyModel handles GET /v1/anomaly-governance/models/{modelID}.
//
// The id must be amd_<uuid>; another tenant's model is 404.
//
// Success: 200 + ETag. Errors: 400, 401, 403, 404, 503.
func (h *Handler) GetAnomalyModel(w http.ResponseWriter, r *http.Request) {
	scope, ok := readScope(w, r)
	if !ok {
		return
	}
	modelID, ok := h.pathID(w, r, "modelID", domain.PrefixAnomalyModel)
	if !ok {
		return
	}
	if !h.authorize(w, r, scope, ActionGovernedRead) {
		return
	}

	model, err := h.store.GetAnomalyModel(r.Context(), scope.TenantID, modelID)
	if err != nil {
		h.writeError(w, r, err)
		return
	}
	setETag(w, model.Version)
	writeJSON(w, http.StatusOK, model)
}

// UpdateAnomalyModel handles PUT /v1/anomaly-governance/models/{modelID}.
//
// Replaces the mutable configuration — review_threshold_bp, max_drift_bp,
// drift_policy, status (all required) — under optimistic concurrency:
// If-Match is required (428) and must equal the current version (412).
// Identity fields never change; register a new model_version instead. The
// caller becomes the model's publisher (updated_by). Increments version.
// Re-enabling a model Disabled by its drift policy is done here.
//
// Success: 200 + new ETag. Errors: 400, 401, 403, 404, 409, 412, 422, 428, 503.
func (h *Handler) UpdateAnomalyModel(w http.ResponseWriter, r *http.Request) {
	scope, ok := readScope(w, r)
	if !ok {
		return
	}
	modelID, ok := h.pathID(w, r, "modelID", domain.PrefixAnomalyModel)
	if !ok {
		return
	}
	expectedVersion, ok := requireIfMatch(w, r)
	if !ok {
		return
	}
	idempotencyKey, ok := requireIdempotencyKey(w, r)
	if !ok {
		return
	}

	var request domain.UpdateAnomalyModelRequest
	body, ok := h.decodeBody(w, r, scope, &request, false)
	if !ok {
		return
	}
	if !h.validate(w, r, &request) {
		return
	}
	if !h.authorize(w, r, scope, ActionModelManage) {
		return
	}

	command := newCommand(r, scope, idempotencyKey, body, &expectedVersion)
	result, err := h.store.UpdateAnomalyModel(r.Context(), command, modelID, request)
	if err != nil {
		h.writeError(w, r, err)
		return
	}

	model := result.Resource
	writeCommandResult(w, http.StatusOK, result.Replayed, model.Version, "", model)
}

// GetModelDriftMetrics handles GET /v1/anomaly-governance/models/{modelID}/drift-metrics.
//
// The spec's "drift metrics" read surface: run count, drift-breached runs,
// last / max / average observed drift against max_drift_bp, signal counts
// by status, and the false-positive rate (dismissed / closed) used for
// false-positive feedback monitoring.
//
// Success: 200. Errors: 400, 401, 403, 404, 503.
func (h *Handler) GetModelDriftMetrics(w http.ResponseWriter, r *http.Request) {
	scope, ok := readScope(w, r)
	if !ok {
		return
	}
	modelID, ok := h.pathID(w, r, "modelID", domain.PrefixAnomalyModel)
	if !ok {
		return
	}
	if !h.authorize(w, r, scope, ActionGovernedRead) {
		return
	}

	metrics, err := h.store.GetModelDriftMetrics(r.Context(), scope.TenantID, modelID)
	if err != nil {
		h.writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, metrics)
}
