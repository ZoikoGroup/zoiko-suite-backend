package handler

import (
	"net/http"

	"zoiko.io/anomaly-detection-svc/internal/domain"
)

// AI-04 AnomalySignal review and ReviewDisposition reads:
// /v1/anomaly-governance/signals and /v1/anomaly-governance/dispositions.

// ListSignals handles GET /v1/anomaly-governance/signals.
//
// The spec's "signals by severity/source/model" surface. Optional filters:
// ?severity= (LOW|MEDIUM|HIGH|CRITICAL), ?source_entity_ref=, ?domain_name=,
// ?model_provider=, ?model_version=, ?status= (Detected|ReviewPending|
// Confirmed|Dismissed|Escalated). Cursor pagination via ?limit= and ?cursor=.
//
// Success: 200 {"data": [...], "count": n, "next_cursor": "..."}.
// Errors: 400 bad enum/limit/cursor, 401, 403, 503.
func (h *Handler) ListSignals(w http.ResponseWriter, r *http.Request) {
	scope, ok := readScope(w, r)
	if !ok {
		return
	}
	severity, ok := queryEnum(w, r, "severity", func(v string) bool { return domain.Severity(v).IsValid() })
	if !ok {
		return
	}
	status, ok := queryEnum(w, r, "status", func(v string) bool { return domain.SignalStatus(v).IsValid() })
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

	query := r.URL.Query()
	filter := domain.SignalFilter{
		Severity:        domain.Severity(severity),
		SourceEntityRef: query.Get("source_entity_ref"),
		DomainName:      query.Get("domain_name"),
		ModelProvider:   query.Get("model_provider"),
		ModelVersion:    query.Get("model_version"),
		Status:          domain.SignalStatus(status),
	}
	signals, nextCursor, err := h.store.ListSignals(r.Context(), scope.TenantID, filter, page)
	if err != nil {
		h.writeError(w, r, err)
		return
	}
	writeList(w, signals, nextCursor)
}

// GetGovernedSignal handles GET /v1/anomaly-governance/signals/{signalID}.
//
// Returns the signal with its frozen model/feature-set provenance, content
// hash, status and version (as ETag). Dismissed signals stay readable as
// evidence.
//
// Success: 200 + ETag. Errors: 400, 401, 403, 404, 503.
func (h *Handler) GetGovernedSignal(w http.ResponseWriter, r *http.Request) {
	scope, ok := readScope(w, r)
	if !ok {
		return
	}
	signalID, ok := h.pathID(w, r, "signalID", domain.PrefixAnomalySignal)
	if !ok {
		return
	}
	if !h.authorize(w, r, scope, ActionGovernedRead) {
		return
	}

	signal, err := h.store.GetGovernedSignal(r.Context(), scope.TenantID, signalID)
	if err != nil {
		h.writeError(w, r, err)
		return
	}
	setETag(w, signal.Version)
	writeJSON(w, http.StatusOK, signal)
}

// AcknowledgeSignal handles POST /v1/anomaly-governance/signals/{signalID}:acknowledge.
//
// Moves Detected → ReviewPending (409 from any other status). Requires
// If-Match (428 / 412) and Idempotency-Key; the body is optional ({}).
// Emits no event (the spec names none for acknowledgement).
//
// Success: 200 + new ETag. Errors: 400, 401, 403, 404, 409, 412, 428, 503.
func (h *Handler) AcknowledgeSignal(w http.ResponseWriter, r *http.Request) {
	scope, ok := readScope(w, r)
	if !ok {
		return
	}
	signalID, ok := h.pathID(w, r, "signalID", domain.PrefixAnomalySignal)
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

	var request domain.AcknowledgeSignalRequest
	body, ok := h.decodeBody(w, r, scope, &request, true)
	if !ok {
		return
	}
	if !h.validate(w, r, &request) {
		return
	}
	if !h.authorize(w, r, scope, ActionAnomalyAck) {
		return
	}

	command := newCommand(r, scope, idempotencyKey, body, &expectedVersion)
	result, err := h.store.AcknowledgeSignal(r.Context(), command, signalID, request)
	if err != nil {
		h.writeError(w, r, err)
		return
	}

	signal := result.Resource
	writeCommandResult(w, http.StatusOK, result.Replayed, signal.Version, "", signal)
}

// EscalateForReview handles POST /v1/anomaly-governance/signals/{signalID}:escalate.
//
// Moves ReviewPending → Escalated (terminal here; 409 from any other
// status). Requires If-Match and Idempotency-Key. Validates reason
// (required) and evidence_refs; a requested_action other than REVIEW is
// 422 advisory_boundary_violation. Writes the review_dispositions row
// (reason_code ESCALATED_FOR_REVIEW) and AI.AnomalyDispositioned outbox
// event in the same transaction.
//
// Success: 200 + new ETag. Errors: 400, 401, 403, 404, 409, 412, 422, 428, 503.
func (h *Handler) EscalateForReview(w http.ResponseWriter, r *http.Request) {
	scope, ok := readScope(w, r)
	if !ok {
		return
	}
	signalID, ok := h.pathID(w, r, "signalID", domain.PrefixAnomalySignal)
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

	var request domain.EscalateForReviewRequest
	body, ok := h.decodeBody(w, r, scope, &request, false)
	if !ok {
		return
	}
	if !h.validate(w, r, &request) {
		return
	}
	if !h.authorize(w, r, scope, ActionAnomalyAck) {
		return
	}

	command := newCommand(r, scope, idempotencyKey, body, &expectedVersion)
	result, err := h.store.EscalateForReview(r.Context(), command, signalID, request)
	if err != nil {
		h.writeError(w, r, err)
		return
	}

	signal := result.Resource
	writeCommandResult(w, http.StatusOK, result.Replayed, signal.Version, "", signal)
}

// CloseSignal handles POST /v1/anomaly-governance/signals/{signalID}:close.
//
// Moves ReviewPending → Confirmed | Dismissed (409 from any other status).
// Requires If-Match and Idempotency-Key. Validates outcome, a reason_code
// allowed for that outcome, non-empty notes and evidence_refs; a
// requested_action such as FREEZE_PAYMENT or WRITE_OFF is 422
// advisory_boundary_violation. Segregation of duties: the closer must differ
// from the run submitter and the model publisher (403 sod_violation).
// Writes the review_dispositions row and AI.AnomalyDispositioned outbox
// event in the same transaction — and nothing else: no payment, write-off
// or freeze is ever performed here.
//
// Success: 200 + new ETag. Errors: 400, 401, 403, 404, 409, 412, 422, 428, 503.
func (h *Handler) CloseSignal(w http.ResponseWriter, r *http.Request) {
	scope, ok := readScope(w, r)
	if !ok {
		return
	}
	signalID, ok := h.pathID(w, r, "signalID", domain.PrefixAnomalySignal)
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

	var request domain.CloseSignalRequest
	body, ok := h.decodeBody(w, r, scope, &request, false)
	if !ok {
		return
	}
	if !h.validate(w, r, &request) {
		return
	}
	if !h.authorize(w, r, scope, ActionAnomalyDisposition) {
		return
	}

	command := newCommand(r, scope, idempotencyKey, body, &expectedVersion)
	result, err := h.store.CloseSignal(r.Context(), command, signalID, request)
	if err != nil {
		h.writeError(w, r, err)
		return
	}

	signal := result.Resource
	writeCommandResult(w, http.StatusOK, result.Replayed, signal.Version, "", signal)
}

// GetDisposition handles GET /v1/anomaly-governance/signals/{signalID}/disposition.
//
// Returns the signal's append-only review disposition (outcome, reason_code,
// notes, evidence_refs, actor, decided_at). 404 anomaly_signal_not_found for
// an unknown signal; 404 disposition_not_found while it is undecided.
//
// Success: 200. Errors: 400, 401, 403, 404, 503.
func (h *Handler) GetDisposition(w http.ResponseWriter, r *http.Request) {
	scope, ok := readScope(w, r)
	if !ok {
		return
	}
	signalID, ok := h.pathID(w, r, "signalID", domain.PrefixAnomalySignal)
	if !ok {
		return
	}
	if !h.authorize(w, r, scope, ActionGovernedRead) {
		return
	}

	disposition, err := h.store.GetDisposition(r.Context(), scope.TenantID, signalID)
	if err != nil {
		h.writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, disposition)
}

// ListDispositions handles GET /v1/anomaly-governance/dispositions.
//
// The spec's "dispositions" read surface. Optional ?outcome= (Confirmed |
// Dismissed | Escalated); cursor pagination via ?limit= and ?cursor=.
//
// Success: 200 {"data": [...], "count": n, "next_cursor": "..."}.
// Errors: 400, 401, 403, 503.
func (h *Handler) ListDispositions(w http.ResponseWriter, r *http.Request) {
	scope, ok := readScope(w, r)
	if !ok {
		return
	}
	outcome, ok := queryEnum(w, r, "outcome", func(v string) bool {
		status := domain.SignalStatus(v)
		return status == domain.SignalConfirmed || status == domain.SignalDismissed || status == domain.SignalEscalated
	})
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

	filter := domain.DispositionFilter{Outcome: domain.SignalStatus(outcome)}
	dispositions, nextCursor, err := h.store.ListDispositions(r.Context(), scope.TenantID, filter, page)
	if err != nil {
		h.writeError(w, r, err)
		return
	}
	writeList(w, dispositions, nextCursor)
}
