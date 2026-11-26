package handler

import (
	"net/http"

	"zoiko.io/anomaly-detection-svc/internal/domain"
)

// AI-04 RunDetection and DetectionRun reads: /v1/anomaly-governance/runs.

// RunDetection handles POST /v1/anomaly-governance/runs.
//
// Validates domain_name, model_provider, model_version, feature_set_version,
// observed_drift_bp (0..10000) and 1..500 signals (source_entity_ref,
// severity, anomaly_score_bp 0..10000). Any signal requested_action other
// than REVIEW is refused with 422 advisory_boundary_violation: an anomaly
// score can never trigger a protected action.
//
// The named model must be registered (422) and Active (409). When
// observed_drift_bp > max_drift_bp the model's drift_policy applies: BLOCK →
// 409 drift_threshold_exceeded; DISABLE → 409 and the model is Disabled;
// ESCALATE → accepted with drift_breached=true and every signal
// ReviewPending. Otherwise a signal is ReviewPending when its score reaches
// review_threshold_bp, else Detected.
//
// Writes the detection_runs row, one anomaly_signals row per signal and one
// AI.AnomalyDetected outbox event per signal, in one transaction.
//
// Success: 201 + Location, body = run with its signals. Errors: 400, 401,
// 403, 409 (key reuse, model disabled, drift), 422, 503.
func (h *Handler) RunDetection(w http.ResponseWriter, r *http.Request) {
	scope, ok := readScope(w, r)
	if !ok {
		return
	}
	idempotencyKey, ok := requireIdempotencyKey(w, r)
	if !ok {
		return
	}

	var request domain.RunDetectionRequest
	body, ok := h.decodeBody(w, r, scope, &request, false)
	if !ok {
		return
	}
	if !h.validate(w, r, &request) {
		return
	}
	if !h.authorize(w, r, scope, ActionAnomalyDetect) {
		return
	}

	command := newCommand(r, scope, idempotencyKey, body, nil)
	result, err := h.store.RunDetection(r.Context(), command, request)
	if err != nil {
		h.writeError(w, r, err)
		return
	}

	run := result.Resource
	writeCommandResult(w, http.StatusCreated, result.Replayed, 0, "/v1/anomaly-governance/runs/"+run.RunID, run)
}

// GetDetectionRun handles GET /v1/anomaly-governance/runs/{runID}.
//
// Returns the immutable run (frozen model version, thresholds, policy
// version, publisher, feature-set version, observed drift) with its signals.
//
// Success: 200. Errors: 400 bad id, 401, 403, 404, 503.
func (h *Handler) GetDetectionRun(w http.ResponseWriter, r *http.Request) {
	scope, ok := readScope(w, r)
	if !ok {
		return
	}
	runID, ok := h.pathID(w, r, "runID", domain.PrefixDetectionRun)
	if !ok {
		return
	}
	if !h.authorize(w, r, scope, ActionGovernedRead) {
		return
	}

	run, err := h.store.GetDetectionRun(r.Context(), scope.TenantID, runID)
	if err != nil {
		h.writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, run)
}

// GetSignalsByRun handles GET /v1/anomaly-governance/runs/{runID}/signals.
//
// Lists the signals of one run in creation order. An unknown (or another
// tenant's) run is 404, not an empty list.
//
// Success: 200 {"data": [...], "count": n}. Errors: 400, 401, 403, 404, 503.
func (h *Handler) GetSignalsByRun(w http.ResponseWriter, r *http.Request) {
	scope, ok := readScope(w, r)
	if !ok {
		return
	}
	runID, ok := h.pathID(w, r, "runID", domain.PrefixDetectionRun)
	if !ok {
		return
	}
	if !h.authorize(w, r, scope, ActionGovernedRead) {
		return
	}

	signals, err := h.store.GetSignalsByRun(r.Context(), scope.TenantID, runID)
	if err != nil {
		h.writeError(w, r, err)
		return
	}
	writeList(w, signals, "")
}
