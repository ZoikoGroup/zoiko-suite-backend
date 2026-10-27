package handler

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"zoiko.io/payroll-run-svc/internal/domain"
	"zoiko.io/payroll-run-svc/internal/events"
	"zoiko.io/payroll-run-svc/internal/store"
)

// WFP-03 manual payroll inputs with maker-checker (WFP-NP-009). The handler only records proposals and
// decisions; an APPROVED input reaches payroll solely by being pulled into an input batch (buildFresh).

const (
	actionManualPropose = "WFP_MANUAL_INPUT_PROPOSE"
	actionManualDecide  = "WFP_MANUAL_INPUT_DECIDE"
)

// ManualInputStore is the persistence surface of manual inputs.
type ManualInputStore interface {
	ProposeManualInput(ctx context.Context, m *domain.ManualInput, mk store.ManualEventBuilder) error
	GetManualInput(ctx context.Context, id string) (*domain.ManualInput, error)
	DecideManualInput(ctx context.Context, id, decision, actor, note string, mk store.ManualEventBuilder) (*domain.ManualInput, error)
	ListManualInputs(ctx context.Context, legalEntityID, periodID, state string) ([]domain.ManualInput, error)
	ListApprovedManualInputs(ctx context.Context, legalEntityID, periodID string) ([]domain.ManualInput, error)
}

func RegisterWFPManualInputRoutes(r chi.Router, h *WFPInputHandler) {
	r.Route("/v1/payroll/wfp/manual-inputs", func(r chi.Router) {
		r.Post("/", h.ProposeManual)
		r.Get("/", h.ListManual)
		r.Get("/{id}", h.GetManual)
		r.Post("/{id}/approve", h.ApproveManual)
		r.Post("/{id}/reject", h.RejectManual)
		r.Post("/{id}/withdraw", h.WithdrawManual)
	})
}

func (h *WFPInputHandler) writeManualErr(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, domain.ErrManualInputNotFound):
		writeError(w, http.StatusNotFound, "not_found", err.Error())
	case errors.Is(err, domain.ErrManualInputNotPending):
		writeError(w, http.StatusConflict, "INVALID_STATE", err.Error())
	case errors.Is(err, domain.ErrManualFourEyes), errors.Is(err, domain.ErrManualOnlyMakerWithdraws):
		writeError(w, http.StatusForbidden, "FOUR_EYES_REQUIRED", err.Error())
	default:
		h.writeErr(w, err)
	}
}

func manualEventBuilder(r *http.Request, actor, tenant string) store.ManualEventBuilder {
	return func(ch store.ManualChange) (domain.OutboxEvent, error) {
		typ := map[string]string{
			"PROPOSE": domain.EventManualInputProposed, "APPROVE": domain.EventManualInputApproved,
			"REJECT": domain.EventManualInputRejected, "WITHDRAW": domain.EventManualInputWithdrawn,
		}[ch.Kind]
		m := ch.Input
		// No amount and no free text travel: the event is a reference to the record.
		body, err := events.BuildWFPEnvelope(typ, getCorrelationID(r), r.Header.Get("X-Causation-Id"), actor, tenant, m.LegalEntityID, "", map[string]any{
			"manual_id": m.ManualID, "legal_entity_id": m.LegalEntityID, "payroll_period_id": m.PayrollPeriodID, "worker_id": m.WorkerID,
			"input_code": m.InputCode, "category": m.Category, "currency": m.Currency, "state": m.State,
			"proposed_by": m.ProposedBy, "decided_by": m.DecidedBy,
		})
		return domain.OutboxEvent{TenantID: tenant, ObjectID: m.ManualID, EventType: typ, Payload: body}, err
	}
}

func (h *WFPInputHandler) ProposeManual(w http.ResponseWriter, r *http.Request) {
	var req domain.ProposeManualInputRequest
	if err := decodeStrict(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_json", err.Error())
		return
	}
	if err := req.Validate(); err != nil {
		writeError(w, http.StatusBadRequest, "validation_failed", err.Error())
		return
	}
	principal, tenant, ok := h.identity(w, r)
	entity := scopeEntity(r)
	if !ok || !h.authorize(w, r, principal, entity, actionManualPropose) {
		return
	}
	m := &domain.ManualInput{
		ManualID: uuid.NewString(), LegalEntityID: entity, PayrollPeriodID: strings.TrimSpace(req.PayrollPeriodID), WorkerID: strings.ToLower(req.WorkerID),
		InputCode: strings.TrimSpace(req.InputCode), Category: req.Category, Amount: req.Amount, Currency: req.Currency,
		EffectiveDate: req.EffectiveDate, Reason: strings.TrimSpace(req.Reason), EvidenceRef: strings.TrimSpace(req.EvidenceRef),
		ProposedBy: principal, CorrelationID: getCorrelationID(r),
	}
	if err := h.store.ProposeManualInput(r.Context(), m, manualEventBuilder(r, principal, tenant)); err != nil {
		h.writeManualErr(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, m)
}

func (h *WFPInputHandler) ListManual(w http.ResponseWriter, r *http.Request) {
	principal, _, ok := h.identity(w, r)
	entity := scopeEntity(r)
	if !ok || !h.authorize(w, r, principal, entity, actionInputView) {
		return
	}
	q := r.URL.Query()
	list, err := h.store.ListManualInputs(r.Context(), entity, q.Get("payroll_period_id"), q.Get("state"))
	if err != nil {
		h.writeManualErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"manual_inputs": list})
}

func (h *WFPInputHandler) GetManual(w http.ResponseWriter, r *http.Request) {
	principal, _, ok := h.identity(w, r)
	if !ok {
		return
	}
	m, err := h.store.GetManualInput(r.Context(), chi.URLParam(r, "id"))
	if err != nil {
		h.writeManualErr(w, err)
		return
	}
	if !h.authorize(w, r, principal, m.LegalEntityID, actionInputView) {
		return
	}
	writeJSON(w, http.StatusOK, m)
}

func (h *WFPInputHandler) decideManual(w http.ResponseWriter, r *http.Request, decision, action string) {
	var req domain.DecideManualInputRequest
	if r.ContentLength != 0 {
		if err := decodeStrict(r, &req); err != nil {
			writeError(w, http.StatusBadRequest, "invalid_json", err.Error())
			return
		}
	}
	principal, tenant, ok := h.identity(w, r)
	if !ok {
		return
	}
	id := chi.URLParam(r, "id")
	if _, err := uuid.Parse(id); err != nil {
		writeError(w, http.StatusBadRequest, "validation_failed", "id must be a UUID")
		return
	}
	cur, err := h.store.GetManualInput(r.Context(), id)
	if err != nil {
		h.writeManualErr(w, err)
		return
	}
	// Authorize against the entity the input belongs to, never a header the caller chose.
	if !h.authorize(w, r, principal, cur.LegalEntityID, action) {
		return
	}
	if decision == domain.ManualRejected && strings.TrimSpace(req.Note) == "" {
		writeError(w, http.StatusBadRequest, "validation_failed", "a rejection needs a note saying why")
		return
	}
	m, err := h.store.DecideManualInput(r.Context(), id, decision, principal, strings.TrimSpace(req.Note), manualEventBuilder(r, principal, tenant))
	if err != nil {
		h.writeManualErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, m)
}

func (h *WFPInputHandler) ApproveManual(w http.ResponseWriter, r *http.Request) {
	h.decideManual(w, r, domain.ManualApproved, actionManualDecide)
}

func (h *WFPInputHandler) RejectManual(w http.ResponseWriter, r *http.Request) {
	h.decideManual(w, r, domain.ManualRejected, actionManualDecide)
}

func (h *WFPInputHandler) WithdrawManual(w http.ResponseWriter, r *http.Request) {
	h.decideManual(w, r, domain.ManualWithdrawn, actionManualPropose)
}

// manualSources turns the approved manual inputs of an entity and period into per-worker sources so they are
// classified with everything else. A manual input for a worker the pull did not return is not dropped: its worker
// is added with an invalid context, so the input is QUARANTINED and stays visible.
func manualSources(inputs []domain.ManualInput, sources []domain.WorkerSource) []domain.WorkerSource {
	if len(inputs) == 0 {
		return sources
	}
	idx := make(map[string]int, len(sources))
	for i, s := range sources {
		idx[strings.ToLower(s.WorkerID)] = i
	}
	for _, m := range inputs {
		amt, err := strconv.ParseFloat(m.Amount, 64)
		if err != nil {
			continue // cannot happen for a stored NUMERIC; an amount is never guessed
		}
		approver := ""
		if m.DecidedBy != nil {
			approver = *m.DecidedBy
		}
		src := domain.SourceManualInput{ManualID: m.ManualID, Code: m.InputCode, Category: m.Category, Amount: amt, Currency: m.Currency,
			EffectiveDate: m.EffectiveDate, Reason: m.Reason, EvidenceRef: m.EvidenceRef, ApprovedBy: approver}
		i, ok := idx[strings.ToLower(m.WorkerID)]
		if !ok {
			sources = append(sources, domain.WorkerSource{WorkerID: m.WorkerID, Context: domain.WorkerContext{Valid: false, Reason: domain.InReasonWorkerContext}})
			i = len(sources) - 1
			idx[strings.ToLower(m.WorkerID)] = i
		}
		sources[i].Manuals = append(sources[i].Manuals, src)
	}
	return sources
}
