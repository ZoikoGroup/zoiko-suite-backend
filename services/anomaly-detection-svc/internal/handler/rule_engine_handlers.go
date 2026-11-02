package handler

import (
	"net/http"

	"zoiko.io/anomaly-detection-svc/internal/domain"
)

// Rule-engine surface: /v1/anomalies.

// ruleListResponse / anomalyListResponse keep the original collection keys
// ("rules", "anomalies", "total") that existing clients of this surface use.
type ruleListResponse struct {
	Rules      []domain.AnomalyRule `json:"rules"`
	Total      int                  `json:"total"`
	NextCursor string               `json:"next_cursor,omitempty"`
}

type anomalyListResponse struct {
	Anomalies  []domain.AnomalyRecord `json:"anomalies"`
	Total      int                    `json:"total"`
	NextCursor string                 `json:"next_cursor,omitempty"`
}

// CreateRule handles POST /v1/anomalies/rules.
//
// Validates rule_name, domain_name, metric_type (required) and the numeric
// ranges; a zero z_score_cutoff defaults to 3.00. Writes one row to
// anomaly_detection_rules (active, version 1). Emits no event. Requires
// Idempotency-Key; a replay returns the original rule.
//
// Success: 201 + Location + ETag. Errors: 400 malformed body / missing
// Idempotency-Key, 401, 403 tenant mismatch or authz denied, 409 key reused
// with a different body, 422 validation, 503 authz unavailable.
func (h *Handler) CreateRule(w http.ResponseWriter, r *http.Request) {
	scope, ok := readScope(w, r)
	if !ok {
		return
	}
	idempotencyKey, ok := requireIdempotencyKey(w, r)
	if !ok {
		return
	}

	var request domain.CreateRuleRequest
	body, ok := h.decodeBody(w, r, scope, &request, false)
	if !ok {
		return
	}
	if !h.validate(w, r, &request) {
		return
	}
	if !h.authorize(w, r, scope, ActionRuleCreate) {
		return
	}

	command := newCommand(r, scope, idempotencyKey, body, nil)
	result, err := h.store.CreateRule(r.Context(), command, request)
	if err != nil {
		h.writeError(w, r, err)
		return
	}

	rule := result.Resource
	writeCommandResult(w, http.StatusCreated, result.Replayed, rule.Version, "/v1/anomalies/rules/"+rule.RuleID, rule)
}

// ListRules handles GET /v1/anomalies/rules.
//
// Optional filter ?domain_name=; cursor pagination via ?limit= (1..200,
// default 50) and ?cursor=. Returns only the caller tenant's rules.
//
// Success: 200 {"rules": [...], "total": n, "next_cursor": "..."}.
// Errors: 400 bad limit/cursor, 401, 403, 503.
func (h *Handler) ListRules(w http.ResponseWriter, r *http.Request) {
	scope, ok := readScope(w, r)
	if !ok {
		return
	}
	page, ok := h.readPage(w, r)
	if !ok {
		return
	}
	if !h.authorize(w, r, scope, ActionAnomalyRead) {
		return
	}

	filter := domain.RuleFilter{DomainName: r.URL.Query().Get("domain_name")}
	rules, nextCursor, err := h.store.ListRules(r.Context(), scope.TenantID, filter, page)
	if err != nil {
		h.writeError(w, r, err)
		return
	}
	if rules == nil {
		rules = []domain.AnomalyRule{}
	}
	writeJSON(w, http.StatusOK, ruleListResponse{Rules: rules, Total: len(rules), NextCursor: nextCursor})
}

// GetRule handles GET /v1/anomalies/rules/{ruleID}.
//
// The id must be arule-<uuid> (400 otherwise); another tenant's rule is 404.
//
// Success: 200 + ETag. Errors: 400, 401, 403, 404, 503.
func (h *Handler) GetRule(w http.ResponseWriter, r *http.Request) {
	scope, ok := readScope(w, r)
	if !ok {
		return
	}
	ruleID, ok := h.pathID(w, r, "ruleID", domain.PrefixAnomalyRule)
	if !ok {
		return
	}
	if !h.authorize(w, r, scope, ActionAnomalyRead) {
		return
	}

	rule, err := h.store.GetRule(r.Context(), scope.TenantID, ruleID)
	if err != nil {
		h.writeError(w, r, err)
		return
	}
	setETag(w, rule.Version)
	writeJSON(w, http.StatusOK, rule)
}

// DetectAnomaly handles POST /v1/anomalies/detect.
//
// Requires X-Legal-Entity-Id (records are legal-entity specific); a
// legal_entity_id in the body may only repeat it (403 otherwise). Validates
// domain_name, source_entity_id, value ranges and an optional rule_id
// (must be an active rule of this tenant → else 422). Computes the z-score
// and severity, writes an OPEN anomaly_records row and the AI.AnomalyDetected
// outbox event in one transaction. The record is advisory: nothing acts on it.
//
// Success: 201 + Location + ETag. Errors: 400, 401, 403, 409 key reuse,
// 422 validation / unknown rule, 503.
func (h *Handler) DetectAnomaly(w http.ResponseWriter, r *http.Request) {
	scope, ok := readScope(w, r)
	if !ok {
		return
	}
	idempotencyKey, ok := requireIdempotencyKey(w, r)
	if !ok {
		return
	}
	if scope.LegalEntityID == "" {
		writeProblem(w, r, http.StatusBadRequest, "legal_entity_required",
			"X-Legal-Entity-Id is required: anomaly records are legal-entity specific")
		return
	}

	var request domain.DetectAnomalyRequest
	body, ok := h.decodeBody(w, r, scope, &request, false)
	if !ok {
		return
	}
	if request.LegalEntityID != "" && request.LegalEntityID != scope.LegalEntityID {
		writeProblem(w, r, http.StatusForbidden, "legal_entity_mismatch",
			"legal_entity_id in the body does not match X-Legal-Entity-Id")
		return
	}
	if !h.validate(w, r, &request) {
		return
	}
	if !h.authorize(w, r, scope, ActionAnomalyDetect) {
		return
	}

	command := newCommand(r, scope, idempotencyKey, body, nil)
	result, err := h.store.DetectAnomaly(r.Context(), command, request)
	if err != nil {
		h.writeError(w, r, err)
		return
	}

	record := result.Resource
	writeCommandResult(w, http.StatusCreated, result.Replayed, record.Version, "/v1/anomalies/"+record.AnomalyID, record)
}

// ListAnomalies handles GET /v1/anomalies.
//
// Optional filters ?legal_entity_id=, ?domain_name=, ?severity=
// (LOW|MEDIUM|HIGH|CRITICAL), ?status= (record status); cursor pagination
// via ?limit= and ?cursor=. Newest first.
//
// Success: 200 {"anomalies": [...], "total": n, "next_cursor": "..."}.
// Errors: 400 bad enum/limit/cursor, 401, 403, 503.
func (h *Handler) ListAnomalies(w http.ResponseWriter, r *http.Request) {
	scope, ok := readScope(w, r)
	if !ok {
		return
	}
	severity, ok := queryEnum(w, r, "severity", func(v string) bool { return domain.Severity(v).IsValid() })
	if !ok {
		return
	}
	status, ok := queryEnum(w, r, "status", func(v string) bool { return domain.AnomalyStatus(v).IsValid() })
	if !ok {
		return
	}
	page, ok := h.readPage(w, r)
	if !ok {
		return
	}
	if !h.authorize(w, r, scope, ActionAnomalyRead) {
		return
	}

	filter := domain.AnomalyFilter{
		LegalEntityID: r.URL.Query().Get("legal_entity_id"),
		DomainName:    r.URL.Query().Get("domain_name"),
		Severity:      domain.Severity(severity),
		Status:        domain.AnomalyStatus(status),
	}
	records, nextCursor, err := h.store.ListAnomalies(r.Context(), scope.TenantID, filter, page)
	if err != nil {
		h.writeError(w, r, err)
		return
	}
	if records == nil {
		records = []domain.AnomalyRecord{}
	}
	writeJSON(w, http.StatusOK, anomalyListResponse{Anomalies: records, Total: len(records), NextCursor: nextCursor})
}

// GetAnomaly handles GET /v1/anomalies/{anomalyID}.
//
// The id must be anom-<uuid> (400 otherwise). Another tenant's record is
// reported as 404, never 403, so existence does not leak.
//
// Success: 200 + ETag. Errors: 400, 401, 403, 404, 503.
func (h *Handler) GetAnomaly(w http.ResponseWriter, r *http.Request) {
	scope, ok := readScope(w, r)
	if !ok {
		return
	}
	anomalyID, ok := h.pathID(w, r, "anomalyID", domain.PrefixAnomalyRecord)
	if !ok {
		return
	}
	if !h.authorize(w, r, scope, ActionAnomalyRead) {
		return
	}

	record, err := h.store.GetAnomaly(r.Context(), scope.TenantID, anomalyID)
	if err != nil {
		h.writeError(w, r, err)
		return
	}
	setETag(w, record.Version)
	writeJSON(w, http.StatusOK, record)
}

// UpdateAnomalyStatus handles POST /v1/anomalies/{anomalyID}/status.
//
// Requires If-Match (428 if missing, 412 if stale) and Idempotency-Key.
// Validates the target status and, for CONFIRMED_ANOMALY / FALSE_POSITIVE /
// RESOLVED, non-empty resolution_notes. The recorded investigator is the
// X-Principal-Id (a body investigated_by may only repeat it, else 403).
// State machine: OPEN → UNDER_INVESTIGATION | FALSE_POSITIVE;
// UNDER_INVESTIGATION → CONFIRMED_ANOMALY | FALSE_POSITIVE;
// CONFIRMED_ANOMALY → RESOLVED (409 otherwise). Dispositions must be made by
// someone other than the detector (403 sod_violation) and emit
// AI.AnomalyDispositioned via the outbox.
//
// Success: 200 + new ETag. Errors: 400, 401, 403, 404, 409, 412, 422, 428, 503.
func (h *Handler) UpdateAnomalyStatus(w http.ResponseWriter, r *http.Request) {
	scope, ok := readScope(w, r)
	if !ok {
		return
	}
	anomalyID, ok := h.pathID(w, r, "anomalyID", domain.PrefixAnomalyRecord)
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

	var request domain.UpdateStatusRequest
	body, ok := h.decodeBody(w, r, scope, &request, false)
	if !ok {
		return
	}
	if request.InvestigatedBy != "" && request.InvestigatedBy != scope.PrincipalID {
		h.writeError(w, r, domain.ErrActorMismatch.WithDetail(
			"investigated_by must be the authenticated X-Principal-Id (or omitted)"))
		return
	}
	if !h.validate(w, r, &request) {
		return
	}
	if !h.authorize(w, r, scope, ActionAnomalyAck) {
		return
	}

	command := newCommand(r, scope, idempotencyKey, body, &expectedVersion)
	result, err := h.store.UpdateAnomalyStatus(r.Context(), command, anomalyID, request)
	if err != nil {
		h.writeError(w, r, err)
		return
	}

	record := result.Resource
	writeCommandResult(w, http.StatusOK, result.Replayed, record.Version, "", record)
}
