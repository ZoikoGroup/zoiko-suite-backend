package handler

import (
	"net/http"

	"github.com/go-chi/chi/v5"

	"zoiko.io/forecasting-svc/internal/domain"
	"zoiko.io/forecasting-svc/internal/store"
)

// ─────────────────────────────────────────────────────────────────────────────
// AI-05 Forecast Assist job handlers. Advisory only: none of these handlers can
// change a forecast.
// ─────────────────────────────────────────────────────────────────────────────

func writeAssistJob(w http.ResponseWriter, status int, job *domain.ForecastAssistJob) {
	w.Header().Set("ETag", etagFor(job.Version))
	respondJSON(w, status, job)
}

// suggestionCommand is the signature shared by the three suggestion commands.
type suggestionCommand func(r *http.Request, cmd store.Command) (*domain.ForecastAssistJob, error)

// runSuggestion is the common body of SuggestDrivers, SuggestRange and
// GenerateNarrative after the body has been decoded and validated.
//
// Authorization scope: the existing job's legal entity when appending,
// otherwise X-Legal-Entity-Id. If-Match is optional and, when sent, is checked
// against the existing job's version.
func (h *Handler) runSuggestion(w http.ResponseWriter, r *http.Request, actorID, operation string, rawBody []byte,
	jobContext domain.JobContext, command suggestionCommand) {
	scope := authorizationScope(r, "")
	if !jobContext.StartsNewJob() {
		existing, err := h.store.GetAssistJob(r.Context(), tenantOf(r), jobContext.JobID)
		if err != nil {
			h.writeError(w, r, err)
			return
		}
		scope = authorizationScope(r, existing.LegalEntityID)
	}
	if !h.authorize(w, r, actorID, scope, ActionAssistSuggest) {
		return
	}
	expectedVersion, ok := optionalIfMatch(w, r)
	if !ok {
		return
	}

	cmd := buildCommand(r, actorID, operation, jobContext.JobID, rawBody, expectedVersion)
	job, err := command(r, cmd)
	if replay, isReplay := asReplay(w, err); isReplay {
		job, err = h.store.GetAssistJob(r.Context(), cmd.TenantID, replay.ResourceID)
	}
	if err != nil {
		h.writeError(w, r, err)
		return
	}
	w.Header().Set("Location", "/v1/forecast-assist/jobs/"+job.JobID)
	writeAssistJob(w, http.StatusCreated, job)
}

// SuggestDrivers handles POST /v1/forecast-assist/suggest-drivers.
//
// Body: either a new-job context (domain_name, model_provider, model_version,
// plan_version, planning_purpose) or job_id of an open job; drivers[] (name,
// value, rationale) and evidence[] (at least one source_ref). A new job needs an
// Approved model release (else 422 model_release_not_registered/_not_evaluated).
// Writes: forecast_assist_jobs (new, status Generated), suggested_drivers,
// evidence_references. Event: AI.ForecastSuggestionGenerated.
// Success: 201 job + ETag. Errors: 400, 401, 403, 404 unknown job_id, 409 job
// not open / key reused, 412 stale If-Match, 422 validation.
func (h *Handler) SuggestDrivers(w http.ResponseWriter, r *http.Request) {
	actorID, ok := h.requireActor(w, r)
	if !ok {
		return
	}
	rawBody, ok := readBody(w, r, true)
	if !ok {
		return
	}
	var req domain.SuggestDriversRequest
	if !decodeJSON(w, r, rawBody, &req) || !h.validate(w, r, &req) {
		return
	}
	h.runSuggestion(w, r, actorID, "assist.suggest_drivers", rawBody, req.JobContext,
		func(r *http.Request, cmd store.Command) (*domain.ForecastAssistJob, error) {
			return h.store.SuggestDrivers(r.Context(), cmd, req)
		})
}

// SuggestRange handles POST /v1/forecast-assist/suggest-range.
//
// Same job rules as SuggestDrivers; body carries ranges[] (period_label,
// low_value <= high_value, confidence_note) and evidence[].
// Writes: suggested_ranges, evidence_references. Event: AI.ForecastSuggestionGenerated.
// Success: 201 job + ETag. Errors: 400, 401, 403, 404, 409, 412, 422.
func (h *Handler) SuggestRange(w http.ResponseWriter, r *http.Request) {
	actorID, ok := h.requireActor(w, r)
	if !ok {
		return
	}
	rawBody, ok := readBody(w, r, true)
	if !ok {
		return
	}
	var req domain.SuggestRangeRequest
	if !decodeJSON(w, r, rawBody, &req) || !h.validate(w, r, &req) {
		return
	}
	h.runSuggestion(w, r, actorID, "assist.suggest_range", rawBody, req.JobContext,
		func(r *http.Request, cmd store.Command) (*domain.ForecastAssistJob, error) {
			return h.store.SuggestRange(r.Context(), cmd, req)
		})
}

// GenerateNarrative handles POST /v1/forecast-assist/generate-narrative.
//
// Same job rules as SuggestDrivers; body carries narrative_text and evidence[].
// The narrative is write-once per job (409 narrative_already_generated).
// Event: AI.ForecastSuggestionGenerated.
// Success: 201 job + ETag. Errors: 400, 401, 403, 404, 409, 412, 422.
func (h *Handler) GenerateNarrative(w http.ResponseWriter, r *http.Request) {
	actorID, ok := h.requireActor(w, r)
	if !ok {
		return
	}
	rawBody, ok := readBody(w, r, true)
	if !ok {
		return
	}
	var req domain.GenerateNarrativeRequest
	if !decodeJSON(w, r, rawBody, &req) || !h.validate(w, r, &req) {
		return
	}
	h.runSuggestion(w, r, actorID, "assist.generate_narrative", rawBody, req.JobContext,
		func(r *http.Request, cmd store.Command) (*domain.ForecastAssistJob, error) {
			return h.store.GenerateNarrative(r.Context(), cmd, req)
		})
}

// loadJobForRequest validates {jobID}, loads the job in the caller's tenant and
// authorizes action against the job's legal entity.
// Errors: 400 bad id (must be faj_<uuid>), 404 (incl. other tenants), 403/503.
func (h *Handler) loadJobForRequest(w http.ResponseWriter, r *http.Request, actorID, action string) (*domain.ForecastAssistJob, bool) {
	jobID := chi.URLParam(r, "jobID")
	if !requirePrefixedPathParam(w, r, "jobID", jobID, domain.PrefixForecastAssistJob) {
		return nil, false
	}
	job, err := h.store.GetAssistJob(r.Context(), tenantOf(r), jobID)
	if err != nil {
		h.writeError(w, r, err)
		return nil, false
	}
	if !h.authorize(w, r, actorID, authorizationScope(r, job.LegalEntityID), action) {
		return nil, false
	}
	return job, true
}

// ListAssistJobs handles GET /v1/forecast-assist/jobs.
//
// Query: status (Queued|Generated|PlannerReview|Accepted|Rejected), limit, cursor.
// Success: 200 {data, count, next_cursor}. Errors: 400, 401, 403.
func (h *Handler) ListAssistJobs(w http.ResponseWriter, r *http.Request) {
	actorID, ok := h.requireActor(w, r)
	if !ok {
		return
	}
	filter := domain.AssistJobListFilter{Status: domain.AssistJobStatus(r.URL.Query().Get("status"))}
	if filter.Status != "" && !filter.Status.Valid() {
		invalidQuery(w, r, "status must be one of Queued, Generated, PlannerReview, Accepted, Rejected")
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
	jobs, nextCursor, err := h.store.ListAssistJobs(r.Context(), tenantOf(r), filter)
	if err != nil {
		h.writeError(w, r, err)
		return
	}
	respondJSON(w, http.StatusOK, listResponse{Data: jobs, Count: len(jobs), NextCursor: nextCursor})
}

// GetAssistJob handles GET /v1/forecast-assist/jobs/{jobID}.
//
// Success: 200 job + ETag. Errors: 400 bad id, 401, 403, 404.
func (h *Handler) GetAssistJob(w http.ResponseWriter, r *http.Request) {
	actorID, ok := h.requireActor(w, r)
	if !ok {
		return
	}
	job, ok := h.loadJobForRequest(w, r, actorID, ActionAssistRead)
	if !ok {
		return
	}
	writeAssistJob(w, http.StatusOK, job)
}

// StartPlannerReview handles POST /v1/forecast-assist/jobs/{jobID}:start-review.
//
// Generated -> PlannerReview; the job's suggestions are frozen from here.
// Requires If-Match. Event: none (not in the AI-05 event list).
// Success: 200 job + ETag. Errors: 400, 401, 403, 404, 409 illegal transition,
// 412, 428.
func (h *Handler) StartPlannerReview(w http.ResponseWriter, r *http.Request) {
	actorID, ok := h.requireActor(w, r)
	if !ok {
		return
	}
	job, ok := h.loadJobForRequest(w, r, actorID, ActionAssistDecide)
	if !ok {
		return
	}
	expectedVersion, ok := requireIfMatch(w, r)
	if !ok {
		return
	}
	rawBody, ok := readBody(w, r, false)
	if !ok {
		return
	}
	var ignoredBody map[string]interface{}
	if !decodeJSON(w, r, rawBody, &ignoredBody) {
		return
	}

	cmd := buildCommand(r, actorID, "assist.start_review", job.JobID, rawBody, expectedVersion)
	reviewed, err := h.store.StartPlannerReview(r.Context(), cmd, job.JobID)
	if replay, isReplay := asReplay(w, err); isReplay {
		reviewed, err = h.store.GetAssistJob(r.Context(), cmd.TenantID, replay.ResourceID)
	}
	if err != nil {
		h.writeError(w, r, err)
		return
	}
	writeAssistJob(w, http.StatusOK, reviewed)
}

// writeDecisionResult responds with {job, decision} and the job's ETag.
func writeDecisionResult(w http.ResponseWriter, result *domain.JobDecisionResult) {
	w.Header().Set("ETag", etagFor(result.Job.Version))
	respondJSON(w, http.StatusOK, result)
}

// AcceptSuggestion handles POST /v1/forecast-assist/jobs/{jobID}:accept.
//
// PlannerReview -> Accepted. Requires If-Match. Optional body:
// target_forecast_id (UUID of a forecast in this tenant; 422 if unknown), note.
// Segregation of duties: the job's creator cannot accept (403). The target
// forecast is NOT modified, whatever its status: the decision records a
// snapshot and AI.ForecastSuggestionAccepted (fin_candidate_required=true)
// asks FIN-02 to create a planner-reviewed candidate.
// Success: 200 {job, decision} + ETag. Errors: 400, 401, 403, 404, 409, 412,
// 422, 428.
func (h *Handler) AcceptSuggestion(w http.ResponseWriter, r *http.Request) {
	actorID, ok := h.requireActor(w, r)
	if !ok {
		return
	}
	job, ok := h.loadJobForRequest(w, r, actorID, ActionAssistDecide)
	if !ok {
		return
	}
	expectedVersion, ok := requireIfMatch(w, r)
	if !ok {
		return
	}
	rawBody, ok := readBody(w, r, false)
	if !ok {
		return
	}
	var req domain.AcceptSuggestionRequest
	if !decodeJSON(w, r, rawBody, &req) || !h.validate(w, r, &req) {
		return
	}

	cmd := buildCommand(r, actorID, "assist.accept", job.JobID, rawBody, expectedVersion)
	result, err := h.store.AcceptSuggestion(r.Context(), cmd, job.JobID, req)
	if replay, isReplay := asReplay(w, err); isReplay {
		result, err = h.store.GetJobDecisionResult(r.Context(), cmd.TenantID, replay.ResourceID)
	}
	if err != nil {
		h.writeError(w, r, err)
		return
	}
	writeDecisionResult(w, result)
}

// RejectSuggestion handles POST /v1/forecast-assist/jobs/{jobID}:reject.
//
// PlannerReview -> Rejected. Requires If-Match. Body: reason (required, 422 if
// missing). Event: none (AI-05 lists no rejection event).
// Success: 200 {job, decision} + ETag. Errors: 400, 401, 403, 404, 409, 412,
// 422, 428.
func (h *Handler) RejectSuggestion(w http.ResponseWriter, r *http.Request) {
	actorID, ok := h.requireActor(w, r)
	if !ok {
		return
	}
	job, ok := h.loadJobForRequest(w, r, actorID, ActionAssistDecide)
	if !ok {
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
	var req domain.RejectSuggestionRequest
	if !decodeJSON(w, r, rawBody, &req) || !h.validate(w, r, &req) {
		return
	}

	cmd := buildCommand(r, actorID, "assist.reject", job.JobID, rawBody, expectedVersion)
	result, err := h.store.RejectSuggestion(r.Context(), cmd, job.JobID, req)
	if replay, isReplay := asReplay(w, err); isReplay {
		result, err = h.store.GetJobDecisionResult(r.Context(), cmd.TenantID, replay.ResourceID)
	}
	if err != nil {
		h.writeError(w, r, err)
		return
	}
	writeDecisionResult(w, result)
}

// GetSuggestedDrivers handles GET /v1/forecast-assist/jobs/{jobID}/drivers.
//
// Success: 200 {data, count}. Errors: 400, 401, 403, 404 unknown job.
func (h *Handler) GetSuggestedDrivers(w http.ResponseWriter, r *http.Request) {
	actorID, ok := h.requireActor(w, r)
	if !ok {
		return
	}
	job, ok := h.loadJobForRequest(w, r, actorID, ActionAssistRead)
	if !ok {
		return
	}
	drivers, err := h.store.GetSuggestedDrivers(r.Context(), job.TenantID, job.JobID)
	if err != nil {
		h.writeError(w, r, err)
		return
	}
	respondJSON(w, http.StatusOK, listResponse{Data: drivers, Count: len(drivers)})
}

// GetSuggestedRanges handles GET /v1/forecast-assist/jobs/{jobID}/ranges.
//
// Success: 200 {data, count}. Errors: 400, 401, 403, 404 unknown job.
func (h *Handler) GetSuggestedRanges(w http.ResponseWriter, r *http.Request) {
	actorID, ok := h.requireActor(w, r)
	if !ok {
		return
	}
	job, ok := h.loadJobForRequest(w, r, actorID, ActionAssistRead)
	if !ok {
		return
	}
	ranges, err := h.store.GetSuggestedRanges(r.Context(), job.TenantID, job.JobID)
	if err != nil {
		h.writeError(w, r, err)
		return
	}
	respondJSON(w, http.StatusOK, listResponse{Data: ranges, Count: len(ranges)})
}

// GetEvidenceReferences handles GET /v1/forecast-assist/jobs/{jobID}/evidence.
//
// Success: 200 {data, count}. Errors: 400, 401, 403, 404 unknown job.
func (h *Handler) GetEvidenceReferences(w http.ResponseWriter, r *http.Request) {
	actorID, ok := h.requireActor(w, r)
	if !ok {
		return
	}
	job, ok := h.loadJobForRequest(w, r, actorID, ActionAssistRead)
	if !ok {
		return
	}
	evidence, err := h.store.GetEvidenceReferences(r.Context(), job.TenantID, job.JobID)
	if err != nil {
		h.writeError(w, r, err)
		return
	}
	respondJSON(w, http.StatusOK, listResponse{Data: evidence, Count: len(evidence)})
}

// GetPlannerDecision handles GET /v1/forecast-assist/jobs/{jobID}/decision.
//
// Success: 200 decision. Errors: 400, 401, 403, 404 unknown job or no decision
// yet (code planner_decision_not_found).
func (h *Handler) GetPlannerDecision(w http.ResponseWriter, r *http.Request) {
	actorID, ok := h.requireActor(w, r)
	if !ok {
		return
	}
	job, ok := h.loadJobForRequest(w, r, actorID, ActionAssistRead)
	if !ok {
		return
	}
	decision, err := h.store.GetPlannerDecision(r.Context(), job.TenantID, job.JobID)
	if err != nil {
		h.writeError(w, r, err)
		return
	}
	respondJSON(w, http.StatusOK, decision)
}
