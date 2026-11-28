package handler

// R-001 WFC-05 deadline clocks over HTTP (tracker D2-4, OB-S6..OB-S12).
//
//	POST /v1/obligations/evaluate                                   definition + trigger -> obligation + clock
//	POST /v1/obligations/{id}/satisfy                               fulfil every running clock of the obligation
//	GET  /v1/obligations/summary                                    console KPIs
//	GET  /v1/obligations/{id}/available-actions                     ST-2 for the obligation
//	POST /v1/obligations/{id}/clocks                                start a clock (computed by jurisdiction-rules-svc)
//	GET  /v1/obligations/{id}/clocks                                list (API-4 cursor)
//	GET  /v1/obligations/{id}/clocks/{cid}                          read (effective state, ETag)
//	GET  /v1/obligations/{id}/clocks/{cid}/transitions | milestones | escalations | available-actions
//	POST /v1/obligations/{id}/clocks/{cid}/activate | satisfy | waive | not-applicable | recalculate
//	POST /v1/obligations/{id}/clocks/{cid}/status                   deprecated alias (ST-3)
//	POST/GET /v1/escalation-policies, GET …/{id}, POST …/{id}/retire
//
// A caller never sends a due instant: there is no field for it, and the JSON
// decoder refuses unknown fields (R-INV-15).

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"go.uber.org/zap"

	"zoiko.io/obligations-svc/internal/authz"
	"zoiko.io/obligations-svc/internal/domain"
	"zoiko.io/obligations-svc/internal/envelope"
	"zoiko.io/obligations-svc/internal/evidencereq"
	"zoiko.io/obligations-svc/internal/jurisdiction"
	"zoiko.io/obligations-svc/internal/store"
	"zoiko.io/obligations-svc/internal/workflowclient"
)

type ctxT = context.Context

// ClockStore is the clock persistence the handler needs.
type ClockStore interface {
	StartClock(ctx ctxT, p domain.StartClockParams) (*domain.Clock, bool, error)
	GetClock(ctx ctxT, clockID string) (*domain.Clock, error)
	ListClocks(ctx ctxT, f domain.ListClocksFilter) ([]*domain.Clock, error)
	LiveClocksForObligation(ctx ctxT, obligationID string) ([]*domain.Clock, error)
	ApplyClockCommand(ctx ctxT, p domain.ClockCommandParams) (*domain.Clock, error)
	SupersedeClock(ctx ctxT, oldClockID string, expectedVersion int, successor domain.StartClockParams) (*domain.Clock, *domain.Clock, bool, error)
	ListClockTransitions(ctx ctxT, clockID string) ([]*domain.ClockTransition, error)
	ListClockMilestones(ctx ctxT, clockID string) ([]*domain.Milestone, error)
	ListEscalationEvents(ctx ctxT, clockID string) ([]*domain.EscalationEvent, error)
	ClockCalculationRequest(ctx ctxT, tenantID, clockID string) (json.RawMessage, error)
	GetApplicabilityDecision(ctx ctxT, id string) (*domain.ApplicabilityDecision, error)
	CreateEscalationPolicy(ctx ctxT, code string, milestones []domain.MilestoneSpec, actor, correlationID string) (*domain.EscalationPolicy, error)
	GetEscalationPolicy(ctx ctxT, id string) (*domain.EscalationPolicy, error)
	ListEscalationPolicies(ctx ctxT, code string, limit int, afterCode string, afterVersion int) ([]*domain.EscalationPolicy, error)
	RetireEscalationPolicy(ctx ctxT, id string, expectedVersion int, actor string) (*domain.EscalationPolicy, error)
	Summary(ctx ctxT, now time.Time) (*domain.ObligationSummary, error)
}

// clockDeps are the collaborators of the clock routes.
type clockDeps struct {
	store      ClockStore
	calculator jurisdiction.Calculator
	workflow   workflowclient.Client
	now        func() time.Time
}

// WithClocks enables the WFC-05 clock routes. Without it they answer 503.
func (h *Handler) WithClocks(s ClockStore, calc jurisdiction.Calculator, wf workflowclient.Client) *Handler {
	h.clocks = &clockDeps{store: s, calculator: calc, workflow: wf, now: func() time.Time { return time.Now().UTC() }}
	return h
}

// WithClock overrides the clock's notion of now (tests).
func (h *Handler) WithClock(now func() time.Time) *Handler {
	if h.clocks != nil {
		h.clocks.now = now
	}
	return h
}

func registerClockRoutes(r chi.Router, h *Handler) {
	r.Post("/v1/obligations/evaluate", h.EvaluateObligation)
	r.Get("/v1/obligations/summary", h.ObligationSummary)
	r.Post("/v1/obligations/{obligation_id}/satisfy", h.SatisfyObligation)
	r.Get("/v1/obligations/{obligation_id}/available-actions", h.ObligationAvailableActions)
	r.Post("/v1/obligations/{obligation_id}/clocks", h.StartClock)
	r.Get("/v1/obligations/{obligation_id}/clocks", h.ListClocks)
	r.Get("/v1/obligations/{obligation_id}/clocks/{clock_id}", h.GetClock)
	r.Get("/v1/obligations/{obligation_id}/clocks/{clock_id}/transitions", h.ListClockTransitions)
	r.Get("/v1/obligations/{obligation_id}/clocks/{clock_id}/milestones", h.ListClockMilestones)
	r.Get("/v1/obligations/{obligation_id}/clocks/{clock_id}/escalations", h.ListClockEscalations)
	r.Get("/v1/obligations/{obligation_id}/clocks/{clock_id}/available-actions", h.ClockAvailableActions)
	r.Post("/v1/obligations/{obligation_id}/clocks/{clock_id}/activate", h.ActivateClock)
	r.Post("/v1/obligations/{obligation_id}/clocks/{clock_id}/satisfy", h.SatisfyClock)
	r.Post("/v1/obligations/{obligation_id}/clocks/{clock_id}/waive", h.WaiveClock)
	r.Post("/v1/obligations/{obligation_id}/clocks/{clock_id}/not-applicable", h.NotApplicableClock)
	r.Post("/v1/obligations/{obligation_id}/clocks/{clock_id}/recalculate", h.RecalculateClock)
	r.Post("/v1/obligations/{obligation_id}/clocks/{clock_id}/status", h.ClockStatusAlias)
	r.Post("/v1/escalation-policies", h.CreateEscalationPolicy)
	r.Get("/v1/escalation-policies", h.ListEscalationPolicies)
	r.Get("/v1/escalation-policies/{escalation_policy_id}", h.GetEscalationPolicy)
	r.Post("/v1/escalation-policies/{escalation_policy_id}/retire", h.RetireEscalationPolicy)
}

// ── shared plumbing ──────────────────────────────────────────────────────────

func (h *Handler) clocksReady(w http.ResponseWriter, r *http.Request) bool {
	if h.clocks == nil || h.clocks.store == nil {
		writeJSON(w, r, http.StatusServiceUnavailable, map[string]string{"error": "clocks_unavailable"})
		return false
	}
	return true
}

func idempotencyKeyOf(r *http.Request) string {
	if env, ok := envelope.FromContext(r.Context()); ok && env.IdempotencyKey != "" {
		return env.IdempotencyKey
	}
	return strings.TrimSpace(r.Header.Get("Idempotency-Key"))
}

func (h *Handler) requireIdempotencyKey(w http.ResponseWriter, r *http.Request) (string, bool) {
	k := idempotencyKeyOf(r)
	if k == "" {
		writeJSON(w, r, http.StatusBadRequest, map[string]string{"error": "idempotency_key_required",
			"detail": "send an Idempotency-Key header; it names this clock command"})
		return "", false
	}
	if len(k) > 200 {
		writeJSON(w, r, http.StatusBadRequest, map[string]string{"error": "invalid_idempotency_key", "detail": "Idempotency-Key is too long"})
		return "", false
	}
	return k, true
}

// writeClockErr maps clock errors; everything else goes to writeStoreErr.
func (h *Handler) writeClockErr(w http.ResponseWriter, r *http.Request, what string, err error) {
	switch {
	case errors.Is(err, domain.ErrClockNotFound):
		writeJSON(w, r, http.StatusNotFound, map[string]string{"error": "clock_not_found"})
	case errors.Is(err, domain.ErrClockVersionMismatch):
		writeJSON(w, r, http.StatusPreconditionFailed, map[string]string{"error": "version_mismatch", "detail": "the clock changed since it was read; reload"})
	case errors.Is(err, domain.ErrClockCommandNotAllowed):
		writeJSON(w, r, http.StatusConflict, map[string]string{"error": "invalid_transition", "detail": err.Error()})
	case errors.Is(err, domain.ErrClockKeyConflict):
		writeJSON(w, r, http.StatusConflict, map[string]string{"error": "idempotency_key_conflict", "detail": "this Idempotency-Key started a different clock"})
	case errors.Is(err, domain.ErrCalculationRefused):
		writeJSON(w, r, http.StatusUnprocessableEntity, map[string]string{"error": "calculation_refused", "detail": err.Error()})
	case errors.Is(err, domain.ErrEscalationPolicyNotFound):
		writeJSON(w, r, http.StatusNotFound, map[string]string{"error": "escalation_policy_not_found"})
	case errors.Is(err, domain.ErrApprovalRequired):
		writeJSON(w, r, http.StatusConflict, map[string]string{"error": "approval_required", "detail": err.Error()})
	case errors.Is(err, domain.ErrApplicabilityMismatch):
		writeJSON(w, r, http.StatusConflict, map[string]string{"error": "applicability_mismatch", "detail": err.Error()})
	case errors.Is(err, domain.ErrWorkflowUnavailable):
		writeJSON(w, r, http.StatusServiceUnavailable, map[string]string{"error": "workflow_unavailable"})
	case errors.Is(err, domain.ErrEvidenceUnavailable):
		writeJSON(w, r, http.StatusServiceUnavailable, map[string]string{"error": "evidence_requirements_unavailable"})
	case errors.Is(err, domain.ErrJurisdictionServiceUnavailable):
		writeJSON(w, r, http.StatusServiceUnavailable, map[string]string{"error": "jurisdiction_service_unavailable"})
	default:
		h.writeStoreErr(w, r, what, err)
	}
}

// loadClock reads a clock and confirms it belongs to the obligation in the
// path (a clock of another obligation is 404, never 403).
func (h *Handler) loadClock(w http.ResponseWriter, r *http.Request) (*domain.Clock, bool) {
	obligationID, clockID := chi.URLParam(r, "obligation_id"), chi.URLParam(r, "clock_id")
	c, err := h.clocks.store.GetClock(r.Context(), clockID)
	if err != nil {
		h.writeClockErr(w, r, "clock lookup failed", err)
		return nil, false
	}
	if c.ObligationID != obligationID {
		writeJSON(w, r, http.StatusNotFound, map[string]string{"error": "clock_not_found"})
		return nil, false
	}
	c.Resolve(h.clocks.now())
	return c, true
}

func writeClock(w http.ResponseWriter, r *http.Request, status int, c *domain.Clock) {
	w.Header().Set("ETag", strconv.Quote(strconv.Itoa(c.RowVersion)))
	writeJSON(w, r, status, c)
}

func parseTime(s string) (time.Time, bool) {
	t, err := time.Parse(time.RFC3339Nano, s)
	return t.UTC(), err == nil
}

// ── start ────────────────────────────────────────────────────────────────────

type triggerRequest struct {
	EventID             string  `json:"event_id"`
	EventType           string  `json:"event_type"`
	EventTime           string  `json:"event_time"`
	BusinessEventTime   *string `json:"business_event_time,omitempty"`
	SourceObjectType    string  `json:"source_object_type"`
	SourceObjectID      string  `json:"source_object_id"`
	SourceObjectVersion *string `json:"source_object_version,omitempty"`
}

type startClockRequest struct {
	ClockClass         string                 `json:"clock_class"`
	JurisdictionCode   string                 `json:"jurisdiction_code"`
	RuleObligationCode string                 `json:"rule_obligation_code"`
	Facts              domain.DueFacts        `json:"facts"`
	PackRef            string                 `json:"pack_ref,omitempty"`
	PackVersion        string                 `json:"pack_version,omitempty"`
	Trigger            triggerRequest         `json:"trigger"`
	Planned            bool                   `json:"planned,omitempty"`
	DueSoonDays        *int                   `json:"due_soon_days,omitempty"`
	EscalationPolicyID *string                `json:"escalation_policy_id,omitempty"`
	Milestones         []domain.MilestoneSpec `json:"milestones,omitempty"`
}

// validate names the first problem ("" if none) and returns the trigger.
func (req startClockRequest) validate(now time.Time) (domain.TriggerEvidence, string, string) {
	var tr domain.TriggerEvidence
	switch {
	case !domain.ClockClasses[domain.ClockClass(req.ClockClass)]:
		return tr, "clock_class", "clock_class must be HARD_LEGAL, REGULATORY, CONTRACTUAL, INTERNAL_SLA or ADVISORY"
	case strings.TrimSpace(req.JurisdictionCode) == "":
		return tr, "jurisdiction_code", "jurisdiction_code is required"
	case strings.TrimSpace(req.RuleObligationCode) == "":
		return tr, "rule_obligation_code", "rule_obligation_code (the jurisdiction rule's obligation code) is required"
	case req.Trigger.EventID == "" || req.Trigger.EventType == "" || req.Trigger.SourceObjectType == "" || req.Trigger.SourceObjectID == "":
		return tr, "trigger", "trigger.event_id, event_type, source_object_type and source_object_id are required (R-001 s7.1)"
	case req.DueSoonDays != nil && (*req.DueSoonDays < 0 || *req.DueSoonDays > 366):
		return tr, "due_soon_days", "due_soon_days must be 0-366"
	}
	et, ok := parseTime(req.Trigger.EventTime)
	if !ok {
		return tr, "trigger.event_time", "trigger.event_time must be an RFC3339 instant"
	}
	if et.After(now.Add(5 * time.Minute)) {
		return tr, "trigger.event_time", "trigger.event_time is in the future"
	}
	tr = domain.TriggerEvidence{EventID: req.Trigger.EventID, EventType: req.Trigger.EventType, EventTime: et,
		SourceObjectType: req.Trigger.SourceObjectType, SourceObjectID: req.Trigger.SourceObjectID,
		SourceObjectVersion: req.Trigger.SourceObjectVersion}
	if req.Trigger.BusinessEventTime != nil {
		bt, ok := parseTime(*req.Trigger.BusinessEventTime)
		if !ok {
			return tr, "trigger.business_event_time", "trigger.business_event_time must be an RFC3339 instant"
		}
		tr.BusinessEventTime = &bt
	}
	for _, m := range req.Milestones {
		if why := domain.ValidMilestone(m); why != "" {
			return tr, "milestones", why
		}
	}
	return tr, "", ""
}

func (req startClockRequest) calculationRequest() jurisdiction.CalculationRequest {
	return jurisdiction.CalculationRequest{Jurisdiction: req.JurisdictionCode, ObligationCode: req.RuleObligationCode,
		DueFacts: req.Facts, PackRef: req.PackRef, PackVersion: req.PackVersion}
}

// calculate asks jurisdiction-rules-svc and writes the refusal itself.
func (h *Handler) calculate(w http.ResponseWriter, r *http.Request, req jurisdiction.CalculationRequest, key string) (*domain.Calculation, bool) {
	calc, err := h.clocks.calculator.Calculate(r.Context(), req, key)
	switch {
	case err == nil:
	case errors.Is(err, domain.ErrAuthorizationDenied):
		writeJSON(w, r, http.StatusForbidden, map[string]string{"error": "calculation_forbidden",
			"detail": "jurisdiction-rules-svc refused the calculation for this principal (OBLIGATION_CALCULATION_CALCULATE)"})
		return nil, false
	case errors.Is(err, domain.ErrInvalidField):
		writeJSON(w, r, http.StatusBadRequest, map[string]string{"error": "invalid_facts", "detail": err.Error()})
		return nil, false
	case errors.Is(err, domain.ErrConflict):
		writeJSON(w, r, http.StatusConflict, map[string]string{"error": "idempotency_key_conflict", "detail": err.Error()})
		return nil, false
	default:
		h.log.Error("due-date calculation failed; failing closed", zap.Error(err))
		writeJSON(w, r, http.StatusServiceUnavailable, map[string]string{"error": "jurisdiction_service_unavailable"})
		return nil, false
	}
	if calc.Outcome != domain.CalculationOutcomeCalculated || calc.DueAt == nil {
		writeJSON(w, r, http.StatusUnprocessableEntity, map[string]any{"error": "calculation_refused", "outcome": calc.Outcome,
			"detail": "jurisdiction-rules-svc answered " + calc.Outcome + ": " + calc.Message, "decision_id": calc.DecisionID})
		return nil, false
	}
	return calc, true
}

func (h *Handler) startClockParams(r *http.Request, obligationID, key, principalID string, req startClockRequest, tr domain.TriggerEvidence, calc *domain.Calculation, raw []byte) domain.StartClockParams {
	dueSoonDays := 7
	if req.DueSoonDays != nil {
		dueSoonDays = *req.DueSoonDays
	}
	var dueSoon *time.Time
	if dueSoonDays > 0 {
		t := calc.DueAt.Add(-time.Duration(dueSoonDays) * 24 * time.Hour)
		dueSoon = &t
	}
	creq, _ := json.Marshal(req.calculationRequest())
	return domain.StartClockParams{ObligationID: obligationID, ClockKey: key, RequestHash: store.ComputeRequestHash(raw),
		ClockClass: domain.ClockClass(req.ClockClass), Planned: req.Planned, JurisdictionCode: req.JurisdictionCode,
		RuleObligationCode: req.RuleObligationCode, Calc: *calc, Trigger: tr, DueSoonAt: dueSoon,
		EscalationPolicyID: req.EscalationPolicyID, ExtraMilestones: req.Milestones, CalculationRequest: creq,
		Actor: principalID, CorrelationID: correlationOf(r), CausationID: tr.EventID}
}

// StartClock handles POST /v1/obligations/{obligation_id}/clocks.
func (h *Handler) StartClock(w http.ResponseWriter, r *http.Request) {
	if !h.clocksReady(w, r) {
		return
	}
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	if _, ok := h.requireTenant(w, r); !ok {
		return
	}
	key, ok := h.requireIdempotencyKey(w, r)
	if !ok {
		return
	}
	raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxRequestBytes))
	if err != nil {
		writeJSON(w, r, http.StatusRequestEntityTooLarge, map[string]string{"error": "request_too_large"})
		return
	}
	r.Body = io.NopCloser(bytes.NewReader(raw))
	var req startClockRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	tr, field, why := req.validate(h.clocks.now())
	if field != "" {
		writeJSON(w, r, http.StatusBadRequest, map[string]string{"error": "invalid_field", "field": field, "detail": why})
		return
	}
	obligationID := chi.URLParam(r, "obligation_id")
	o, err := h.store.FindObligationByID(r.Context(), obligationID)
	if err != nil {
		h.writeStoreErr(w, r, "StartClock: obligation lookup failed", err)
		return
	}
	if !h.authorize(w, r, principalID, o.LegalEntityID, authz.ActionClockCreate) {
		return
	}
	c, status, ok := h.startClockFor(w, r, o.ObligationID, key, principalID, req, tr, raw)
	if !ok {
		return
	}
	if status == http.StatusCreated {
		w.Header().Set("Location", "/v1/obligations/"+o.ObligationID+"/clocks/"+c.ClockID)
	}
	writeClock(w, r, status, c)
}

func (h *Handler) startClockFor(w http.ResponseWriter, r *http.Request, obligationID, key, principalID string, req startClockRequest, tr domain.TriggerEvidence, raw []byte) (*domain.Clock, int, bool) {
	calc, ok := h.calculate(w, r, req.calculationRequest(), "obligations-clock:"+key)
	if !ok {
		return nil, 0, false
	}
	c, created, err := h.clocks.store.StartClock(r.Context(), h.startClockParams(r, obligationID, key, principalID, req, tr, calc, raw))
	if err != nil {
		h.writeClockErr(w, r, "StartClock: store failed", err)
		return nil, 0, false
	}
	c.Resolve(h.clocks.now())
	if created {
		return c, http.StatusCreated, true
	}
	return c, http.StatusOK, true
}

// ── evaluate (definition + trigger -> obligation + clock) ────────────────────

type evaluateRequest struct {
	Obligation json.RawMessage   `json:"obligation"`
	Clock      startClockRequest `json:"clock"`
}

// EvaluateObligation handles POST /v1/obligations/evaluate: it raises the
// obligation from a catalogue definition (every rule of POST /v1/obligations
// applies: definition pinned, entity mapped and in the registry, jurisdiction
// validated, OBLIGATION_CREATE) and starts its clock from the trigger
// (OBLIGATION_CLOCK_CREATE). The obligation's due_date is the clock's local
// due date. Idempotent on the Idempotency-Key.
func (h *Handler) EvaluateObligation(w http.ResponseWriter, r *http.Request) {
	if !h.clocksReady(w, r) {
		return
	}
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	if _, ok := h.requireTenant(w, r); !ok {
		return
	}
	key, ok := h.requireIdempotencyKey(w, r)
	if !ok {
		return
	}
	var req evaluateRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	var ob map[string]any
	if err := json.Unmarshal(req.Obligation, &ob); err != nil || ob == nil {
		writeJSON(w, r, http.StatusBadRequest, map[string]string{"error": "invalid_field", "field": "obligation"})
		return
	}
	if def, _ := ob["obligation_definition_id"].(string); def == "" {
		writeJSON(w, r, http.StatusBadRequest, map[string]string{"error": "missing_field", "field": "obligation.obligation_definition_id",
			"detail": "evaluate raises an obligation from a catalogue definition"})
		return
	}
	if _, has := ob["due_date"]; has {
		writeJSON(w, r, http.StatusBadRequest, map[string]string{"error": "invalid_field", "field": "obligation.due_date",
			"detail": "the due date is computed by jurisdiction-rules-svc; it cannot be sent"})
		return
	}
	tr, field, why := req.Clock.validate(h.clocks.now())
	if field != "" {
		writeJSON(w, r, http.StatusBadRequest, map[string]string{"error": "invalid_field", "field": "clock." + field, "detail": why})
		return
	}
	calc, ok := h.calculate(w, r, req.Clock.calculationRequest(), "obligations-clock:evaluate:"+key)
	if !ok {
		return
	}
	ob["due_date"] = calc.DueAt.UTC().Format(time.RFC3339)
	if _, has := ob["created_by_principal_id"]; !has {
		ob["created_by_principal_id"] = principalID
	}
	obBody, _ := json.Marshal(ob)

	// Raise the obligation through the one create path, so every rule of
	// POST /v1/obligations holds here too.
	sub := r.Clone(r.Context())
	sub.Body = io.NopCloser(bytes.NewReader(obBody))
	sub.ContentLength = int64(len(obBody))
	rec := httptest.NewRecorder()
	h.CreateObligation(rec, sub)
	if rec.Code != http.StatusCreated && rec.Code != http.StatusOK {
		for k, v := range rec.Header() {
			w.Header()[k] = v
		}
		w.WriteHeader(rec.Code)
		_, _ = w.Write(rec.Body.Bytes())
		return
	}
	var o domain.Obligation
	if err := json.Unmarshal(rec.Body.Bytes(), &o); err != nil || o.ObligationID == "" {
		writeJSON(w, r, http.StatusServiceUnavailable, map[string]string{"error": "store_unavailable"})
		return
	}
	if !h.authorize(w, r, principalID, o.LegalEntityID, authz.ActionClockCreate) {
		return
	}
	clockRaw, _ := json.Marshal(req.Clock)
	c, _, err := h.clocks.store.StartClock(r.Context(), h.startClockParams(r, o.ObligationID, "evaluate:"+key, principalID, req.Clock, tr, calc, clockRaw))
	if err != nil {
		h.writeClockErr(w, r, "EvaluateObligation: clock start failed", err)
		return
	}
	c.Resolve(h.clocks.now())
	status := http.StatusOK
	if rec.Code == http.StatusCreated {
		status = http.StatusCreated
		w.Header().Set("Location", "/v1/obligations/"+o.ObligationID)
	}
	writeJSON(w, r, status, map[string]any{"obligation": o, "clock": c})
}

// ── reads ────────────────────────────────────────────────────────────────────

func (h *Handler) authorizeObligationRead(w http.ResponseWriter, r *http.Request, principalID string) (*domain.Obligation, bool) {
	o, err := h.store.FindObligationByID(r.Context(), chi.URLParam(r, "obligation_id"))
	if err != nil {
		h.writeStoreErr(w, r, "obligation lookup failed", err)
		return nil, false
	}
	if !h.authorize(w, r, principalID, o.LegalEntityID, authz.ActionObligationRead) {
		return nil, false
	}
	return o, true
}

// GetClock handles GET …/clocks/{clock_id}.
func (h *Handler) GetClock(w http.ResponseWriter, r *http.Request) {
	if !h.clocksReady(w, r) {
		return
	}
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	if _, ok := h.requireTenant(w, r); !ok {
		return
	}
	if _, ok := h.authorizeObligationRead(w, r, principalID); !ok {
		return
	}
	c, ok := h.loadClock(w, r)
	if !ok {
		return
	}
	writeClock(w, r, http.StatusOK, c)
}

// ListClocks handles GET …/clocks (API-4 keyset cursor).
func (h *Handler) ListClocks(w http.ResponseWriter, r *http.Request) {
	if !h.clocksReady(w, r) {
		return
	}
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	if _, ok := h.requireTenant(w, r); !ok {
		return
	}
	o, ok := h.authorizeObligationRead(w, r, principalID)
	if !ok {
		return
	}
	limit, offset, ok := parsePaging(w, r)
	if !ok {
		return
	}
	if offset != 0 {
		writeJSON(w, r, http.StatusBadRequest, map[string]string{"error": "invalid_offset", "detail": "clocks page by cursor only"})
		return
	}
	f := domain.ListClocksFilter{ObligationID: o.ObligationID, Limit: limit}
	q := r.URL.Query()
	if v := q.Get("clock_class"); v != "" {
		if !domain.ClockClasses[domain.ClockClass(v)] {
			writeJSON(w, r, http.StatusBadRequest, map[string]string{"error": "invalid_field", "field": "clock_class"})
			return
		}
		f.ClockClass = domain.ClockClass(v)
	}
	if v := q.Get("stored_state"); v != "" {
		if !domain.ClockStatuses[domain.ClockState(v)] {
			writeJSON(w, r, http.StatusBadRequest, map[string]string{"error": "invalid_field", "field": "stored_state"})
			return
		}
		f.StoredState = domain.ClockState(v)
	}
	if c := q.Get("cursor"); c != "" {
		at, id, ok := decodeCursor(c)
		if !ok {
			writeJSON(w, r, http.StatusBadRequest, map[string]string{"error": "invalid_field", "field": "cursor",
				"detail": "cursor must be a next_cursor this endpoint returned"})
			return
		}
		f.AfterCreatedAt, f.AfterID = &at, id
	}
	list, err := h.clocks.store.ListClocks(r.Context(), f)
	if err != nil {
		h.writeClockErr(w, r, "ListClocks failed", err)
		return
	}
	if list == nil {
		list = []*domain.Clock{}
	}
	now := h.clocks.now()
	for _, c := range list {
		c.Resolve(now)
	}
	if len(list) == limit {
		last := list[len(list)-1]
		next := encodeCursor(last.CreatedAt, last.ClockID)
		nq := r.URL.Query()
		nq.Set("cursor", next)
		w.Header().Set("X-Next-Cursor", next)
		w.Header().Set("Link", "<"+r.URL.Path+"?"+nq.Encode()+`>; rel="next"`)
	}
	writeJSON(w, r, http.StatusOK, list)
}

func (h *Handler) clockSubRead(w http.ResponseWriter, r *http.Request, fetch func(clockID string) (any, error)) {
	if !h.clocksReady(w, r) {
		return
	}
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	if _, ok := h.requireTenant(w, r); !ok {
		return
	}
	if _, ok := h.authorizeObligationRead(w, r, principalID); !ok {
		return
	}
	c, ok := h.loadClock(w, r)
	if !ok {
		return
	}
	out, err := fetch(c.ClockID)
	if err != nil {
		h.writeClockErr(w, r, "clock read failed", err)
		return
	}
	writeJSON(w, r, http.StatusOK, out)
}

// ListClockTransitions handles GET …/clocks/{clock_id}/transitions (ST-2).
func (h *Handler) ListClockTransitions(w http.ResponseWriter, r *http.Request) {
	h.clockSubRead(w, r, func(id string) (any, error) {
		l, err := h.clocks.store.ListClockTransitions(r.Context(), id)
		if l == nil {
			l = []*domain.ClockTransition{}
		}
		return l, err
	})
}

// ListClockMilestones handles GET …/clocks/{clock_id}/milestones.
func (h *Handler) ListClockMilestones(w http.ResponseWriter, r *http.Request) {
	h.clockSubRead(w, r, func(id string) (any, error) {
		l, err := h.clocks.store.ListClockMilestones(r.Context(), id)
		if l == nil {
			l = []*domain.Milestone{}
		}
		return l, err
	})
}

// ListClockEscalations handles GET …/clocks/{clock_id}/escalations.
func (h *Handler) ListClockEscalations(w http.ResponseWriter, r *http.Request) {
	h.clockSubRead(w, r, func(id string) (any, error) {
		l, err := h.clocks.store.ListEscalationEvents(r.Context(), id)
		if l == nil {
			l = []*domain.EscalationEvent{}
		}
		return l, err
	})
}

// availableAction is one command a caller could run now (ST-2).
type availableAction struct {
	Command  string `json:"command"`
	Method   string `json:"method"`
	Path     string `json:"path"`
	Action   string `json:"authorization_action"`
	Allowed  bool   `json:"allowed"`
	Reason   string `json:"reason,omitempty"`
	Requires string `json:"requires,omitempty"`
}

// ClockAvailableActions handles GET …/clocks/{clock_id}/available-actions:
// the named commands the clock's effective state allows, each checked
// against the caller's grant at the clock's entity.
func (h *Handler) ClockAvailableActions(w http.ResponseWriter, r *http.Request) {
	if !h.clocksReady(w, r) {
		return
	}
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	if _, ok := h.requireTenant(w, r); !ok {
		return
	}
	if _, ok := h.authorizeObligationRead(w, r, principalID); !ok {
		return
	}
	c, ok := h.loadClock(w, r)
	if !ok {
		return
	}
	base := "/v1/obligations/" + c.ObligationID + "/clocks/" + c.ClockID + "/"
	cmds := []struct{ cmd, action, requires string }{
		{domain.ClockCommandActivate, authz.ActionClockCreate, ""},
		{domain.ClockCommandSatisfy, authz.ActionClockSatisfy, "evidence_refs, or an approved evidence exemption"},
		{domain.ClockCommandWaive, authz.ActionClockWaive, "an APPROVED OBLIGATION_WAIVER workflow decided by another principal"},
		{domain.ClockCommandNotApplicable, authz.ActionClockNotApplicable, "an APPROVED OBLIGATION_NOT_APPLICABLE workflow and a NOT_APPLICABLE applicability decision"},
		{domain.ClockCommandRecalculate, authz.ActionClockRecalculate, "a reason"},
	}
	out := []availableAction{}
	for _, x := range cmds {
		if !domain.ClockCommandAllowed(x.cmd, c.ClockState) {
			continue
		}
		a := availableAction{Command: x.cmd, Method: http.MethodPost, Path: base + x.cmd, Action: x.action, Requires: x.requires, Allowed: true}
		if err := h.authz.CheckAllowed(r.Context(), principalID, c.LegalEntityID, r.Header.Get("X-Tenant-Id"), x.action, correlationOf(r)); err != nil {
			a.Allowed = false
			if errors.Is(err, domain.ErrAuthorizationDenied) {
				a.Reason = "not granted"
			} else {
				a.Reason = "authorization unavailable"
			}
		}
		out = append(out, a)
	}
	writeJSON(w, r, http.StatusOK, map[string]any{"clock_id": c.ClockID, "clock_state": c.ClockState, "row_version": c.RowVersion, "actions": out})
}

// ObligationAvailableActions handles GET /v1/obligations/{id}/available-actions.
func (h *Handler) ObligationAvailableActions(w http.ResponseWriter, r *http.Request) {
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	if _, ok := h.requireTenant(w, r); !ok {
		return
	}
	o, ok := h.authorizeObligationRead(w, r, principalID)
	if !ok {
		return
	}
	base := "/v1/obligations/" + o.ObligationID
	type cand struct{ command, method, path, action string }
	var cands []cand
	for _, next := range domain.ObligationTransitions[o.ObligationStatus] {
		cands = append(cands, cand{"status:" + next, http.MethodPost, base + "/status", authz.ActionObligationStatusUpdate})
	}
	if o.ObligationStatus != "CLOSED" {
		cands = append(cands,
			cand{"update", http.MethodPatch, base, authz.ActionObligationUpdate},
			cand{"add-filing-requirement", http.MethodPost, base + "/filing-requirements", authz.ActionFilingRequirementAdd},
			cand{"start-clock", http.MethodPost, base + "/clocks", authz.ActionClockCreate},
			cand{"satisfy", http.MethodPost, base + "/satisfy", authz.ActionClockSatisfy},
			cand{"record-applicability", http.MethodPost, base + "/applicability-decisions", authz.ActionApplicabilityDecide})
		if o.WorkflowID == nil {
			cands = append(cands, cand{"link-workflow", http.MethodPost, base + "/workflow", authz.ActionObligationStatusUpdate})
		} else {
			cands = append(cands, cand{"unlink-workflow", http.MethodDelete, base + "/workflow", authz.ActionObligationStatusUpdate})
		}
	}
	out := []availableAction{}
	for _, c := range cands {
		a := availableAction{Command: c.command, Method: c.method, Path: c.path, Action: c.action, Allowed: true}
		if err := h.authz.CheckAllowed(r.Context(), principalID, o.LegalEntityID, r.Header.Get("X-Tenant-Id"), c.action, correlationOf(r)); err != nil {
			a.Allowed = false
			if errors.Is(err, domain.ErrAuthorizationDenied) {
				a.Reason = "not granted"
			} else {
				a.Reason = "authorization unavailable"
			}
		}
		out = append(out, a)
	}
	writeJSON(w, r, http.StatusOK, map[string]any{"obligation_id": o.ObligationID, "obligation_status": o.ObligationStatus,
		"version": o.Version, "actions": out})
}

// ObligationSummary handles GET /v1/obligations/summary (console KPIs).
func (h *Handler) ObligationSummary(w http.ResponseWriter, r *http.Request) {
	if !h.clocksReady(w, r) {
		return
	}
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	if _, ok := h.requireTenant(w, r); !ok {
		return
	}
	// The summary spans the tenant's register, like the catalogue reads.
	if !h.authorize(w, r, principalID, authz.PlatformScope, authz.ActionObligationRead) {
		return
	}
	s, err := h.clocks.store.Summary(r.Context(), h.clocks.now())
	if err != nil {
		h.writeClockErr(w, r, "ObligationSummary failed", err)
		return
	}
	writeJSON(w, r, http.StatusOK, s)
}

// ── commands ─────────────────────────────────────────────────────────────────

// commandPrelude: principal, tenant, Idempotency-Key, If-Match (428 without),
// the clock, and the action's grant at the clock's entity.
func (h *Handler) commandPrelude(w http.ResponseWriter, r *http.Request, action string, bodyVersion int) (principalID string, c *domain.Clock, version int, ok bool) {
	if !h.clocksReady(w, r) {
		return "", nil, 0, false
	}
	if principalID, ok = h.requirePrincipal(w, r); !ok {
		return "", nil, 0, false
	}
	if _, ok = h.requireTenant(w, r); !ok {
		return "", nil, 0, false
	}
	if _, ok = h.requireIdempotencyKey(w, r); !ok {
		return "", nil, 0, false
	}
	v, present, ok := ifMatch(w, r, bodyVersion)
	if !ok {
		return "", nil, 0, false
	}
	if !present {
		preconditionRequired(w, r)
		return "", nil, 0, false
	}
	if c, ok = h.loadClock(w, r); !ok {
		return "", nil, 0, false
	}
	if !h.authorize(w, r, principalID, c.LegalEntityID, action) {
		return "", nil, 0, false
	}
	return principalID, c, v, true
}

// ActivateClock handles POST …/activate (PLANNED -> ACTIVE).
func (h *Handler) ActivateClock(w http.ResponseWriter, r *http.Request) {
	var body struct {
		ExpectedVersion int `json:"expected_version,omitempty"`
	}
	if r.ContentLength != 0 && !decodeJSON(w, r, &body) {
		return
	}
	principalID, c, v, ok := h.commandPrelude(w, r, authz.ActionClockCreate, body.ExpectedVersion)
	if !ok {
		return
	}
	out, err := h.clocks.store.ApplyClockCommand(r.Context(), domain.ClockCommandParams{ClockID: c.ClockID, Command: domain.ClockCommandActivate,
		ExpectedVersion: v, Actor: principalID, CorrelationID: correlationOf(r), Now: h.clocks.now()})
	if err != nil {
		h.writeClockErr(w, r, "ActivateClock failed", err)
		return
	}
	writeClock(w, r, http.StatusOK, out)
}

type satisfyRequest struct {
	EvidenceRefs          []string `json:"evidence_refs,omitempty"`
	EvidenceExemptReason  *string  `json:"evidence_exempt_reason,omitempty"`
	ApprovalReference     *string  `json:"approval_reference,omitempty"`
	DomainConfirmationRef *string  `json:"domain_confirmation_ref,omitempty"`
	SubjectState          *string  `json:"subject_state,omitempty"`
	ExpectedVersion       int      `json:"expected_version,omitempty"`
}

// parseEvidenceRefs reads "<EVIDENCE_TYPE>:<reference_id>[:<subtype>]".
func parseEvidenceRefs(refs []string) ([]evidencereq.Artifact, string) {
	out := make([]evidencereq.Artifact, 0, len(refs))
	for _, ref := range refs {
		parts := strings.SplitN(ref, ":", 3)
		if len(parts) < 2 || parts[0] == "" || parts[1] == "" {
			return nil, ref
		}
		a := evidencereq.Artifact{EvidenceType: parts[0], ReferenceID: parts[1]}
		if len(parts) == 3 {
			a.ArtifactSubtype = parts[2]
		}
		out = append(out, a)
	}
	return out, ""
}

// checkSatisfaction applies R-INV-14 to one obligation: evidence refs that
// evidence-requirements-svc accepts, or an approved evidence exemption. It
// writes the refusal itself and returns the evaluation it relied on.
func (h *Handler) checkSatisfaction(w http.ResponseWriter, r *http.Request, principalID string, o *domain.Obligation, subjectClockID string, req satisfyRequest) (evalID, outcome *string, ok bool) {
	if len(req.EvidenceRefs) == 0 && req.EvidenceExemptReason == nil {
		writeJSON(w, r, http.StatusConflict, map[string]string{"error": "evidence_missing",
			"detail": "FULFILLED needs evidence_refs or an approved evidence_exempt_reason (R-001 R-INV-14)"})
		return nil, nil, false
	}
	if len(req.EvidenceRefs) == 0 {
		if req.ApprovalReference == nil || *req.ApprovalReference == "" {
			writeJSON(w, r, http.StatusConflict, map[string]string{"error": "approval_required",
				"detail": "an evidence exemption needs an approved workflow (approval_reference)"})
			return nil, nil, false
		}
		if _, ok := h.verifyApproval(w, r, principalID, *req.ApprovalReference, subjectClockID, "EVIDENCE_EXEMPT", *req.EvidenceExemptReason, nil, "OBLIGATION_EVIDENCE_EXEMPTION"); !ok {
			return nil, nil, false
		}
		o := "EXEMPT"
		return nil, &o, true
	}
	arts, bad := parseEvidenceRefs(req.EvidenceRefs)
	if bad != "" {
		writeJSON(w, r, http.StatusBadRequest, map[string]string{"error": "invalid_field", "field": "evidence_refs",
			"detail": "each evidence ref is <EVIDENCE_TYPE>:<reference_id>[:<subtype>]; got " + bad})
		return nil, nil, false
	}
	domainCode, actionType := mapObligationToEvidence(o.ObligationType)
	if domainCode == "" {
		domainCode, actionType = "COMPLIANCE", "OBLIGATION_FULFIL_"+strings.ToUpper(o.ObligationType)
	}
	res, err := h.evidenceReq.Evaluate(r.Context(), evidencereq.EvaluateParams{LegalEntityID: o.LegalEntityID, DomainCode: domainCode,
		ActionType: actionType, CorrelationID: correlationOf(r), PresentArtifacts: arts})
	switch {
	case err == nil:
	case errors.Is(err, domain.ErrAuthorizationDenied):
		writeJSON(w, r, http.StatusForbidden, map[string]string{"error": "evidence_evaluation_forbidden"})
		return nil, nil, false
	default:
		h.log.Error("evidence evaluation failed; failing closed", zap.Error(err))
		writeJSON(w, r, http.StatusServiceUnavailable, map[string]string{"error": "evidence_requirements_unavailable"})
		return nil, nil, false
	}
	switch res.Outcome {
	case "SATISFIED":
	case "NO_REQUIREMENTS_DEFINED":
		h.log.Warn("clock fulfilled with evidence refs but no evidence requirement is defined (D-ER-5)",
			zap.String("obligation_id", o.ObligationID), zap.String("domain_code", domainCode), zap.String("action_type", actionType))
	default:
		writeJSON(w, r, http.StatusConflict, map[string]any{"error": "evidence_missing", "evaluation_id": res.EvaluationID,
			"outcome": res.Outcome, "unmet": res.Unmet, "domain_code": domainCode, "action_type": actionType})
		return nil, nil, false
	}
	id, out := res.EvaluationID, res.Outcome
	return &id, &out, true
}

// verifyApproval checks the approval workflow named by the caller: it is of
// the expected type, about this clock, approved for exactly this subject
// (verify-release), and decided by someone other than the caller.
func (h *Handler) verifyApproval(w http.ResponseWriter, r *http.Request, principalID, approvalRef, clockID, action, reasonCode string, evidence []string, workflowType string) (string, bool) {
	if h.clocks.workflow == nil {
		writeJSON(w, r, http.StatusServiceUnavailable, map[string]string{"error": "workflow_unavailable",
			"detail": "WORKFLOW_SERVICE_URL is not configured; approvals cannot be verified"})
		return "", false
	}
	a, err := h.clocks.workflow.VerifyApproval(r.Context(), approvalRef, domain.AuthoritySubjectFingerprint(clockID, action, reasonCode, evidence))
	if err != nil {
		h.writeClockErr(w, r, "approval verification failed", err)
		return "", false
	}
	switch {
	case a.WorkflowType != workflowType:
		writeJSON(w, r, http.StatusConflict, map[string]string{"error": "approval_required",
			"detail": "approval_reference must be a " + workflowType + " workflow"})
		return "", false
	case a.SubjectType != "OBLIGATION_CLOCK" || a.SubjectID != clockID:
		writeJSON(w, r, http.StatusConflict, map[string]string{"error": "approval_required",
			"detail": "the approval is not about this clock (subject_type OBLIGATION_CLOCK, subject_id = clock_id)"})
		return "", false
	}
	other := ""
	for _, d := range a.Deciders {
		if d == principalID {
			writeJSON(w, r, http.StatusConflict, map[string]string{"error": "approval_required",
				"detail": "the requester approved their own request; a different principal must approve"})
			return "", false
		}
		if other == "" {
			other = d
		}
	}
	if other == "" {
		writeJSON(w, r, http.StatusConflict, map[string]string{"error": "approval_required", "detail": "the workflow records no approver"})
		return "", false
	}
	return other, true
}

// SatisfyClock handles POST …/clocks/{clock_id}/satisfy.
func (h *Handler) SatisfyClock(w http.ResponseWriter, r *http.Request) {
	var req satisfyRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	principalID, c, v, ok := h.commandPrelude(w, r, authz.ActionClockSatisfy, req.ExpectedVersion)
	if !ok {
		return
	}
	if !domain.ClockCommandAllowed(domain.ClockCommandSatisfy, c.ClockState) {
		writeJSON(w, r, http.StatusConflict, map[string]string{"error": "invalid_transition", "detail": "a " + string(c.ClockState) + " clock cannot be fulfilled"})
		return
	}
	o, err := h.store.FindObligationByID(r.Context(), c.ObligationID)
	if err != nil {
		h.writeStoreErr(w, r, "SatisfyClock: obligation lookup failed", err)
		return
	}
	evalID, outcome, ok := h.checkSatisfaction(w, r, principalID, o, c.ClockID, req)
	if !ok {
		return
	}
	out, err := h.clocks.store.ApplyClockCommand(r.Context(), domain.ClockCommandParams{ClockID: c.ClockID, Command: domain.ClockCommandSatisfy,
		ExpectedVersion: v, Actor: principalID, CorrelationID: correlationOf(r), Now: h.clocks.now(),
		EvidenceRefs: req.EvidenceRefs, EvidenceExemptReason: req.EvidenceExemptReason, EvidenceEvaluationID: evalID, EvidenceOutcome: outcome,
		DomainConfirmationRef: req.DomainConfirmationRef, SubjectState: req.SubjectState, ApprovalReference: req.ApprovalReference})
	if err != nil {
		h.writeClockErr(w, r, "SatisfyClock failed", err)
		return
	}
	writeClock(w, r, http.StatusOK, out)
}

// SatisfyObligation handles POST /v1/obligations/{id}/satisfy: the evidence
// fulfils every running clock of the obligation (R-001 s10.1). If-Match is
// the obligation's version.
func (h *Handler) SatisfyObligation(w http.ResponseWriter, r *http.Request) {
	if !h.clocksReady(w, r) {
		return
	}
	var req satisfyRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	if _, ok := h.requireTenant(w, r); !ok {
		return
	}
	if _, ok := h.requireIdempotencyKey(w, r); !ok {
		return
	}
	v, present, ok := ifMatch(w, r, req.ExpectedVersion)
	if !ok {
		return
	}
	if !present {
		preconditionRequired(w, r)
		return
	}
	o, err := h.store.FindObligationByID(r.Context(), chi.URLParam(r, "obligation_id"))
	if err != nil {
		h.writeStoreErr(w, r, "SatisfyObligation: lookup failed", err)
		return
	}
	if o.Version != v {
		writeJSON(w, r, http.StatusPreconditionFailed, map[string]string{"error": "version_mismatch", "detail": "the obligation changed since it was read; reload"})
		return
	}
	if !h.authorize(w, r, principalID, o.LegalEntityID, authz.ActionClockSatisfy) {
		return
	}
	live, err := h.clocks.store.LiveClocksForObligation(r.Context(), o.ObligationID)
	if err != nil {
		h.writeClockErr(w, r, "SatisfyObligation: clocks failed", err)
		return
	}
	now := h.clocks.now()
	var running []*domain.Clock
	for _, c := range live {
		c.Resolve(now)
		if c.ClockState.Running() {
			running = append(running, c)
		}
	}
	if len(running) == 0 {
		writeJSON(w, r, http.StatusConflict, map[string]string{"error": "no_running_clock", "detail": "the obligation has no running deadline clock"})
		return
	}
	// One evaluation for the obligation; an exemption is approved per clock,
	// so it applies only when the obligation has exactly one running clock.
	if len(req.EvidenceRefs) == 0 && len(running) > 1 {
		writeJSON(w, r, http.StatusConflict, map[string]string{"error": "evidence_missing",
			"detail": "an evidence exemption is approved per clock; satisfy each clock on its own route"})
		return
	}
	evalID, outcome, ok := h.checkSatisfaction(w, r, principalID, o, running[0].ClockID, req)
	if !ok {
		return
	}
	out := make([]*domain.Clock, 0, len(running))
	for _, c := range running {
		done, err := h.clocks.store.ApplyClockCommand(r.Context(), domain.ClockCommandParams{ClockID: c.ClockID, Command: domain.ClockCommandSatisfy,
			ExpectedVersion: c.RowVersion, Actor: principalID, CorrelationID: correlationOf(r), Now: now,
			EvidenceRefs: req.EvidenceRefs, EvidenceExemptReason: req.EvidenceExemptReason, EvidenceEvaluationID: evalID, EvidenceOutcome: outcome,
			DomainConfirmationRef: req.DomainConfirmationRef, SubjectState: req.SubjectState, ApprovalReference: req.ApprovalReference})
		if err != nil {
			h.writeClockErr(w, r, "SatisfyObligation failed", err)
			return
		}
		out = append(out, done)
	}
	writeJSON(w, r, http.StatusOK, map[string]any{"obligation_id": o.ObligationID, "clocks": out})
}

type authorityRequest struct {
	ReasonCode              string   `json:"reason_code"`
	ReasonText              *string  `json:"reason_text,omitempty"`
	EvidenceRefs            []string `json:"evidence_refs"`
	ApprovalReference       string   `json:"approval_reference"`
	ApplicabilityDecisionID *string  `json:"applicability_decision_id,omitempty"`
	ExpectedVersion         int      `json:"expected_version,omitempty"`
}

func (h *Handler) authorityCommand(w http.ResponseWriter, r *http.Request, command string) {
	var req authorityRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	action, workflowType, authAction := authz.ActionClockWaive, "OBLIGATION_WAIVER", "WAIVE"
	if command == domain.ClockCommandNotApplicable {
		action, workflowType, authAction = authz.ActionClockNotApplicable, "OBLIGATION_NOT_APPLICABLE", "NOT_APPLICABLE"
	}
	principalID, c, v, ok := h.commandPrelude(w, r, action, req.ExpectedVersion)
	if !ok {
		return
	}
	switch {
	case strings.TrimSpace(req.ReasonCode) == "":
		writeJSON(w, r, http.StatusBadRequest, map[string]string{"error": "missing_field", "field": "reason_code"})
		return
	case len(req.EvidenceRefs) == 0:
		writeJSON(w, r, http.StatusBadRequest, map[string]string{"error": "missing_field", "field": "evidence_refs",
			"detail": "a waiver or not-applicable decision must carry its evidence (R-001 s7.1)"})
		return
	case strings.TrimSpace(req.ApprovalReference) == "":
		writeJSON(w, r, http.StatusConflict, map[string]string{"error": "approval_required",
			"detail": "approval_reference must name an APPROVED " + workflowType + " workflow decided by another principal"})
		return
	}
	if !domain.ClockCommandAllowed(command, c.ClockState) {
		writeJSON(w, r, http.StatusConflict, map[string]string{"error": "invalid_transition", "detail": "a " + string(c.ClockState) + " clock cannot take " + command})
		return
	}
	if command == domain.ClockCommandNotApplicable {
		if req.ApplicabilityDecisionID == nil || *req.ApplicabilityDecisionID == "" {
			writeJSON(w, r, http.StatusBadRequest, map[string]string{"error": "missing_field", "field": "applicability_decision_id"})
			return
		}
		d, err := h.clocks.store.GetApplicabilityDecision(r.Context(), *req.ApplicabilityDecisionID)
		if err != nil {
			h.writeStoreErr(w, r, "applicability lookup failed", err)
			return
		}
		if d.ObligationID != c.ObligationID || d.Decision != "NOT_APPLICABLE" {
			h.writeClockErr(w, r, "", domain.ErrApplicabilityMismatch)
			return
		}
	}
	approvedBy, ok := h.verifyApproval(w, r, principalID, req.ApprovalReference, c.ClockID, authAction, req.ReasonCode, req.EvidenceRefs, workflowType)
	if !ok {
		return
	}
	ref := req.ApprovalReference
	out, err := h.clocks.store.ApplyClockCommand(r.Context(), domain.ClockCommandParams{ClockID: c.ClockID, Command: command,
		ExpectedVersion: v, Actor: principalID, CorrelationID: correlationOf(r), Now: h.clocks.now(),
		ReasonCode: &req.ReasonCode, ReasonText: req.ReasonText, EvidenceRefs: req.EvidenceRefs, ApprovalReference: &ref,
		ApprovedBy: &approvedBy, ApplicabilityDecisionID: req.ApplicabilityDecisionID})
	if err != nil {
		h.writeClockErr(w, r, command+" failed", err)
		return
	}
	writeClock(w, r, http.StatusOK, out)
}

// WaiveClock handles POST …/clocks/{clock_id}/waive.
func (h *Handler) WaiveClock(w http.ResponseWriter, r *http.Request) {
	h.authorityCommand(w, r, domain.ClockCommandWaive)
}

// NotApplicableClock handles POST …/clocks/{clock_id}/not-applicable.
func (h *Handler) NotApplicableClock(w http.ResponseWriter, r *http.Request) {
	h.authorityCommand(w, r, domain.ClockCommandNotApplicable)
}

type recalculateRequest struct {
	ReasonCode      string  `json:"reason_code"`
	ReasonText      *string `json:"reason_text,omitempty"`
	ExpectedVersion int     `json:"expected_version,omitempty"`
}

// RecalculateClock handles POST …/clocks/{clock_id}/recalculate: the same
// question is asked again; if the due instant or calendar version changed the
// clock is superseded by a successor (R-INV-17), otherwise nothing changes.
func (h *Handler) RecalculateClock(w http.ResponseWriter, r *http.Request) {
	var req recalculateRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	principalID, c, v, ok := h.commandPrelude(w, r, authz.ActionClockRecalculate, req.ExpectedVersion)
	if !ok {
		return
	}
	if strings.TrimSpace(req.ReasonCode) == "" {
		writeJSON(w, r, http.StatusBadRequest, map[string]string{"error": "missing_field", "field": "reason_code"})
		return
	}
	if !domain.ClockCommandAllowed(domain.ClockCommandRecalculate, c.ClockState) {
		writeJSON(w, r, http.StatusConflict, map[string]string{"error": "invalid_transition", "detail": "a " + string(c.ClockState) + " clock cannot be recalculated"})
		return
	}
	raw, err := h.clocks.store.ClockCalculationRequest(r.Context(), "", c.ClockID)
	if err != nil {
		h.writeClockErr(w, r, "RecalculateClock: request lookup failed", err)
		return
	}
	var creq jurisdiction.CalculationRequest
	if err := json.Unmarshal(raw, &creq); err != nil || creq.Jurisdiction == "" {
		writeJSON(w, r, http.StatusConflict, map[string]string{"error": "not_recalculable",
			"detail": "the clock does not hold the facts it was calculated from"})
		return
	}
	key := idempotencyKeyOf(r)
	calc, ok := h.calculate(w, r, creq, "obligations-recalc:"+c.ClockID+":"+key)
	if !ok {
		return
	}
	reason := req.ReasonCode
	succ := domain.StartClockParams{ClockKey: "recalc:" + c.ClockID + ":" + key, RequestHash: store.ComputeRequestHash(raw),
		Calc: *calc, CalculationRequest: raw, SupersessionReason: &reason, Actor: principalID, CorrelationID: correlationOf(r)}
	old, next, superseded, err := h.clocks.store.SupersedeClock(r.Context(), c.ClockID, v, succ)
	if err != nil {
		h.writeClockErr(w, r, "RecalculateClock failed", err)
		return
	}
	now := h.clocks.now()
	old.Resolve(now)
	body := map[string]any{"superseded": superseded, "clock": old}
	if next != nil {
		next.Resolve(now)
		body["successor"] = next
	}
	w.Header().Set("ETag", strconv.Quote(strconv.Itoa(old.RowVersion)))
	writeJSON(w, r, http.StatusOK, body)
}

// ClockStatusAlias handles the deprecated POST …/clocks/{clock_id}/status
// (ST-3). A clock's state is computed or moved by named commands; the only
// target this alias still accepts is ACTIVE (activate). Every other target
// answers 409 naming the command to use.
func (h *Handler) ClockStatusAlias(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Deprecation", "true")
	w.Header().Set("Link", `</v1/obligations/{obligation_id}/clocks/{clock_id}/activate>; rel="successor-version"`)
	var body struct {
		ClockState      string `json:"clock_state"`
		ExpectedVersion int    `json:"expected_version,omitempty"`
	}
	raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxRequestBytes))
	if err != nil {
		writeJSON(w, r, http.StatusRequestEntityTooLarge, map[string]string{"error": "request_too_large"})
		return
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	if err := dec.Decode(&body); err != nil {
		writeJSON(w, r, http.StatusBadRequest, map[string]string{"error": "invalid_json"})
		return
	}
	commands := map[string]string{"FULFILLED": "satisfy", "WAIVED": "waive", "NOT_APPLICABLE": "not-applicable", "SUPERSEDED": "recalculate"}
	switch {
	case body.ClockState == string(domain.ClockStateActive):
		r.Body = io.NopCloser(bytes.NewReader([]byte(`{"expected_version":` + strconv.Itoa(body.ExpectedVersion) + `}`)))
		r.ContentLength = 1
		h.ActivateClock(w, r)
	case commands[body.ClockState] != "":
		writeJSON(w, r, http.StatusConflict, map[string]string{"error": "use_named_command",
			"detail": "POST …/clocks/{clock_id}/" + commands[body.ClockState] + " replaces this route for " + body.ClockState})
	default:
		writeJSON(w, r, http.StatusConflict, map[string]string{"error": "computed_state",
			"detail": "DUE_SOON, DUE and OVERDUE are computed from the clock's times and now; they cannot be set"})
	}
}

// ── escalation policies ──────────────────────────────────────────────────────

type escalationPolicyRequest struct {
	PolicyCode string                 `json:"policy_code"`
	Milestones []domain.MilestoneSpec `json:"milestones"`
}

// CreateEscalationPolicy handles POST /v1/escalation-policies (a new version
// of the code; version 1 for a new code).
func (h *Handler) CreateEscalationPolicy(w http.ResponseWriter, r *http.Request) {
	if !h.clocksReady(w, r) {
		return
	}
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	if _, ok := h.requireTenant(w, r); !ok {
		return
	}
	if _, ok := h.requireIdempotencyKey(w, r); !ok {
		return
	}
	var req escalationPolicyRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if !policyCodeRe(req.PolicyCode) {
		writeJSON(w, r, http.StatusBadRequest, map[string]string{"error": "invalid_field", "field": "policy_code",
			"detail": "policy_code is 2-64 characters of A-Z, 0-9 and _"})
		return
	}
	if len(req.Milestones) == 0 || len(req.Milestones) > 20 {
		writeJSON(w, r, http.StatusBadRequest, map[string]string{"error": "invalid_field", "field": "milestones", "detail": "1-20 milestones"})
		return
	}
	for _, m := range req.Milestones {
		if why := domain.ValidMilestone(m); why != "" {
			writeJSON(w, r, http.StatusBadRequest, map[string]string{"error": "invalid_field", "field": "milestones", "detail": why})
			return
		}
	}
	if !h.authorize(w, r, principalID, authz.PlatformScope, authz.ActionEscalationPolicyManage) {
		return
	}
	p, err := h.clocks.store.CreateEscalationPolicy(r.Context(), req.PolicyCode, req.Milestones, principalID, correlationOf(r))
	if err != nil {
		h.writeClockErr(w, r, "CreateEscalationPolicy failed", err)
		return
	}
	w.Header().Set("Location", "/v1/escalation-policies/"+p.EscalationPolicyID)
	w.Header().Set("ETag", strconv.Quote(strconv.Itoa(p.RowVersion)))
	writeJSON(w, r, http.StatusCreated, p)
}

func policyCodeRe(s string) bool {
	if len(s) < 2 || len(s) > 64 {
		return false
	}
	for _, ch := range s {
		if !(ch >= 'A' && ch <= 'Z' || ch >= '0' && ch <= '9' || ch == '_') {
			return false
		}
	}
	return true
}

// ListEscalationPolicies handles GET /v1/escalation-policies.
func (h *Handler) ListEscalationPolicies(w http.ResponseWriter, r *http.Request) {
	if !h.clocksReady(w, r) {
		return
	}
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	if _, ok := h.requireTenant(w, r); !ok {
		return
	}
	if !h.authorize(w, r, principalID, authz.PlatformScope, authz.ActionObligationDefinitionRead) {
		return
	}
	limit, _, ok := parsePaging(w, r)
	if !ok {
		return
	}
	afterCode, afterVersion := "", 0
	if c := r.URL.Query().Get("cursor"); c != "" {
		i := strings.LastIndex(c, ".")
		v, err := strconv.Atoi(c[i+1:])
		if i <= 0 || err != nil {
			writeJSON(w, r, http.StatusBadRequest, map[string]string{"error": "invalid_field", "field": "cursor"})
			return
		}
		afterCode, afterVersion = c[:i], v
	}
	list, err := h.clocks.store.ListEscalationPolicies(r.Context(), r.URL.Query().Get("policy_code"), limit, afterCode, afterVersion)
	if err != nil {
		h.writeClockErr(w, r, "ListEscalationPolicies failed", err)
		return
	}
	if list == nil {
		list = []*domain.EscalationPolicy{}
	}
	if len(list) == limit {
		last := list[len(list)-1]
		w.Header().Set("X-Next-Cursor", last.PolicyCode+"."+strconv.Itoa(last.Version))
	}
	writeJSON(w, r, http.StatusOK, list)
}

// GetEscalationPolicy handles GET /v1/escalation-policies/{id}.
func (h *Handler) GetEscalationPolicy(w http.ResponseWriter, r *http.Request) {
	if !h.clocksReady(w, r) {
		return
	}
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	if _, ok := h.requireTenant(w, r); !ok {
		return
	}
	if !h.authorize(w, r, principalID, authz.PlatformScope, authz.ActionObligationDefinitionRead) {
		return
	}
	p, err := h.clocks.store.GetEscalationPolicy(r.Context(), chi.URLParam(r, "escalation_policy_id"))
	if err != nil {
		h.writeClockErr(w, r, "GetEscalationPolicy failed", err)
		return
	}
	w.Header().Set("ETag", strconv.Quote(strconv.Itoa(p.RowVersion)))
	writeJSON(w, r, http.StatusOK, p)
}

// RetireEscalationPolicy handles POST /v1/escalation-policies/{id}/retire.
func (h *Handler) RetireEscalationPolicy(w http.ResponseWriter, r *http.Request) {
	if !h.clocksReady(w, r) {
		return
	}
	var body struct {
		ExpectedVersion int `json:"expected_version,omitempty"`
	}
	if r.ContentLength != 0 && !decodeJSON(w, r, &body) {
		return
	}
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	if _, ok := h.requireTenant(w, r); !ok {
		return
	}
	if _, ok := h.requireIdempotencyKey(w, r); !ok {
		return
	}
	v, present, ok := ifMatch(w, r, body.ExpectedVersion)
	if !ok {
		return
	}
	if !present {
		preconditionRequired(w, r)
		return
	}
	if !h.authorize(w, r, principalID, authz.PlatformScope, authz.ActionEscalationPolicyManage) {
		return
	}
	p, err := h.clocks.store.RetireEscalationPolicy(r.Context(), chi.URLParam(r, "escalation_policy_id"), v, principalID)
	if err != nil {
		h.writeClockErr(w, r, "RetireEscalationPolicy failed", err)
		return
	}
	w.Header().Set("ETag", strconv.Quote(strconv.Itoa(p.RowVersion)))
	writeJSON(w, r, http.StatusOK, p)
}
