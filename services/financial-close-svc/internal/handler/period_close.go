package handler

import (
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"zoiko.io/financial-close-svc/internal/domain"
)

// Period close state machine commands (ACC-14, ZS-SVC-B-001 §17). Hard close
// and reclose live with the close's readiness and evidence logic
// (closeWithEvidence); the transitions here carry no evidence of their own
// beyond the history row every transition writes.

// StartSoftClose — POST /v1/close/periods/{id}/soft-close: OPEN →
// SOFT_CLOSE. Ordinary posting stops; close journals continue.
// PERIOD_CLOSE_INITIATE.
func (h *Handler) StartSoftClose(w http.ResponseWriter, r *http.Request) {
	h.simpleTransition(w, r, domain.PeriodOpen, domain.PeriodSoftClose, "soft-closed", "period.soft_closed")
}

// EnterCloseReview — POST /v1/close/periods/{id}/close-review: SOFT_CLOSE →
// CLOSE_REVIEW. Only designated close journals from here; next is hard
// close. PERIOD_CLOSE_INITIATE.
func (h *Handler) EnterCloseReview(w http.ResponseWriter, r *http.Request) {
	h.simpleTransition(w, r, domain.PeriodSoftClose, domain.PeriodCloseReview, "close-review-started", "period.close_review_started")
}

func (h *Handler) simpleTransition(w http.ResponseWriter, r *http.Request, from, to, fact, legacyType string) {
	id := chi.URLParam(r, "id")
	var req domain.PeriodTransitionRequest
	if r.ContentLength != 0 && !decodeJSON(w, r, &req) {
		return
	}
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	if _, ok := h.requireTenant(w, r); !ok {
		return
	}
	fp, err := h.store.GetFiscalPeriod(r.Context(), id)
	if err != nil {
		h.writeStoreErr(w, err, "period_not_found")
		return
	}
	if err := h.authz.CheckAllowed(r.Context(), principalID, fp.LegalEntityID, actionCloseInitiate); err != nil {
		h.writeAuthzErr(w, err)
		return
	}
	correlationID := correlationIDOf(r)
	updated, err := h.store.ApplyPeriodTransition(r.Context(), id, []string{from}, domain.PeriodUpdate{
		To: to, PrincipalID: principalID, Reason: strings.TrimSpace(req.Reason), At: time.Now().UTC(),
	}, fact, legacyType, correlationID, principalID)
	if err != nil {
		h.writeTransitionErr(w, err, updated, from)
		return
	}
	writeJSON(w, http.StatusOK, updated)
}

// RequestReopen — POST /v1/close/periods/{id}/reopen-requests: ask for a
// closed period to be reopened until reopen_until, with a reason. Nothing
// reopens until a different principal approves. PERIOD_REOPEN.
func (h *Handler) RequestReopen(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	var req domain.RequestReopenRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	req.Reason = strings.TrimSpace(req.Reason)
	if req.Reason == "" {
		writeError(w, http.StatusBadRequest, "missing_fields", string(domain.ErrReopenReasonRequired))
		return
	}
	now := time.Now().UTC()
	if !req.ReopenUntil.After(now) || req.ReopenUntil.Sub(now) > domain.MaxReopenWindow {
		writeError(w, http.StatusBadRequest, "invalid_reopen_window", string(domain.ErrReopenWindowInvalid))
		return
	}
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	tenantID, ok := h.requireTenant(w, r)
	if !ok {
		return
	}
	fp, err := h.store.GetFiscalPeriod(r.Context(), id)
	if err != nil {
		h.writeStoreErr(w, err, "period_not_found")
		return
	}
	if err := h.authz.CheckAllowed(r.Context(), principalID, fp.LegalEntityID, actionPeriodReopen); err != nil {
		h.writeAuthzErr(w, err)
		return
	}
	rr := &domain.ReopenRequest{
		RequestID: uuid.NewString(), TenantID: tenantID, FiscalPeriodID: id, RequestedByPrincipalID: principalID,
		Reason: req.Reason, ReopenUntil: req.ReopenUntil.UTC(), Status: domain.ReopenPending, CreatedAt: now,
	}
	correlationID := correlationIDOf(r)
	if err := h.store.CreateReopenRequest(r.Context(), rr, correlationID, principalID); err != nil {
		switch {
		case errors.Is(err, domain.ErrReopenRequestPending):
			writeError(w, http.StatusConflict, "reopen_request_pending", string(domain.ErrReopenRequestPending))
		case errors.Is(err, domain.ErrInvalidPeriodTransition):
			writeError(w, http.StatusConflict, "invalid_period_transition",
				fmt.Sprintf("only a HARD_CLOSED or RECLOSED period can be reopened; this one is %s", fp.CloseStatus))
		default:
			h.writeStoreErr(w, err, "period_not_found")
		}
		return
	}
	writeJSON(w, http.StatusCreated, rr)
}

// ApproveReopen — POST /v1/close/reopen-requests/{request_id}/approve: a
// principal other than the requester reopens the period until the requested
// time. PERIOD_REOPEN_APPROVE.
func (h *Handler) ApproveReopen(w http.ResponseWriter, r *http.Request) {
	h.decideReopen(w, r, true)
}

// RejectReopen — POST /v1/close/reopen-requests/{request_id}/reject: the
// request is closed and the period stays closed. A reason is required.
// PERIOD_REOPEN_APPROVE, and not by the requester.
func (h *Handler) RejectReopen(w http.ResponseWriter, r *http.Request) {
	h.decideReopen(w, r, false)
}

func (h *Handler) decideReopen(w http.ResponseWriter, r *http.Request, approve bool) {
	requestID := chi.URLParam(r, "request_id")
	var req domain.DecideReopenRequest
	if r.ContentLength != 0 && !decodeJSON(w, r, &req) {
		return
	}
	req.Reason = strings.TrimSpace(req.Reason)
	if !approve && req.Reason == "" {
		writeError(w, http.StatusBadRequest, "missing_fields", "reason is required to reject a reopen request")
		return
	}
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	if _, ok := h.requireTenant(w, r); !ok {
		return
	}
	rr, err := h.store.GetReopenRequest(r.Context(), requestID)
	if errors.Is(err, domain.ErrReopenRequestNotFound) {
		writeError(w, http.StatusNotFound, "reopen_request_not_found", "")
		return
	}
	if err != nil {
		h.writeStoreErr(w, err, "reopen_request_not_found")
		return
	}
	fp, err := h.store.GetFiscalPeriod(r.Context(), rr.FiscalPeriodID)
	if err != nil {
		h.writeStoreErr(w, err, "period_not_found")
		return
	}
	if err := h.authz.CheckAllowed(r.Context(), principalID, fp.LegalEntityID, actionPeriodReopenApprove); err != nil {
		h.writeAuthzErr(w, err)
		return
	}
	// Checked here for a clear answer; the store and the table enforce it too.
	if rr.RequestedByPrincipalID == principalID {
		writeError(w, http.StatusForbidden, "self_approval_forbidden", string(domain.ErrReopenSelfApproval))
		return
	}

	now := time.Now().UTC()
	correlationID := correlationIDOf(r)
	if !approve {
		decided, err := h.store.RejectReopenRequest(r.Context(), requestID, principalID, req.Reason, now, correlationID, principalID)
		if err != nil {
			h.writeReopenDecisionErr(w, err)
			return
		}
		writeJSON(w, http.StatusOK, decided)
		return
	}
	decided, reopened, err := h.store.ApproveReopenRequest(r.Context(), requestID, principalID, req.Reason, now, correlationID, principalID)
	if err != nil {
		h.writeReopenDecisionErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"reopen_request": decided, "period": reopened})
}

func (h *Handler) writeReopenDecisionErr(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, domain.ErrReopenRequestNotFound):
		writeError(w, http.StatusConflict, "reopen_request_not_pending", string(domain.ErrReopenRequestNotFound))
	case errors.Is(err, domain.ErrReopenSelfApproval):
		writeError(w, http.StatusForbidden, "self_approval_forbidden", string(domain.ErrReopenSelfApproval))
	case errors.Is(err, domain.ErrReopenWindowInvalid):
		writeError(w, http.StatusConflict, "reopen_window_passed",
			"the requested reopen window has already ended; request a new one")
	case errors.Is(err, domain.ErrInvalidPeriodTransition):
		writeError(w, http.StatusConflict, "invalid_period_transition", "the period is no longer closed")
	default:
		h.writeStoreErr(w, err, "reopen_request_not_found")
	}
}

// GetCloseHistory — GET /v1/close/periods/{id}/history: every transition
// and reopen request, oldest first. PERIOD_CLOSE_VIEW.
func (h *Handler) GetCloseHistory(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	fp, ok := h.viewablePeriod(w, r, id)
	if !ok {
		return
	}
	hist, err := h.store.GetCloseHistory(r.Context(), fp.FiscalPeriodID)
	if err != nil {
		h.writeStoreErr(w, err, "period_not_found")
		return
	}
	writeJSON(w, http.StatusOK, hist)
}

// GetAvailableCloseActions — GET /v1/close/periods/{id}/available-actions:
// which close commands the period's state allows now (not whether the
// caller holds the permission for them, which authorization decides).
func (h *Handler) GetAvailableCloseActions(w http.ResponseWriter, r *http.Request) {
	fp, ok := h.viewablePeriod(w, r, chi.URLParam(r, "id"))
	if !ok {
		return
	}
	now := time.Now().UTC()
	writeJSON(w, http.StatusOK, domain.AvailableCloseActions{
		FiscalPeriodID: fp.FiscalPeriodID, PeriodState: fp.CloseStatus, PostingPolicy: fp.PostingPolicy(now),
		Actions: availableActions(fp.CloseStatus),
	})
}

func availableActions(state string) []string {
	switch state {
	case domain.PeriodOpen:
		return []string{"soft-close"}
	case domain.PeriodSoftClose:
		return []string{"close-review"}
	case domain.PeriodCloseReview:
		return []string{"hard-close"}
	case domain.PeriodHardClosed, domain.PeriodReclosed:
		return []string{"reopen-request"}
	case domain.PeriodAuthorizedReopen:
		return []string{"reclose"}
	}
	return []string{}
}

func (h *Handler) viewablePeriod(w http.ResponseWriter, r *http.Request, id string) (*domain.FiscalPeriod, bool) {
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return nil, false
	}
	if _, ok := h.requireTenant(w, r); !ok {
		return nil, false
	}
	fp, err := h.store.GetFiscalPeriod(r.Context(), id)
	if err != nil {
		h.writeStoreErr(w, err, "period_not_found")
		return nil, false
	}
	if err := h.authz.CheckAllowed(r.Context(), principalID, fp.LegalEntityID, actionCloseView); err != nil {
		h.writeAuthzErr(w, err)
		return nil, false
	}
	return fp, true
}

// writeTransitionErr answers a refused transition with the state the period
// is actually in, so a caller can tell "out of order" from "someone else
// moved it first".
func (h *Handler) writeTransitionErr(w http.ResponseWriter, err error, current *domain.FiscalPeriod, needed string) {
	if errors.Is(err, domain.ErrInvalidPeriodTransition) {
		state := "unknown"
		if current != nil {
			state = current.CloseStatus
		}
		writeError(w, http.StatusConflict, "invalid_period_transition",
			fmt.Sprintf("%s; the period is %s and this command needs %s", domain.ErrInvalidPeriodTransition, state, needed))
		return
	}
	h.writeStoreErr(w, err, "period_not_found")
}

func correlationIDOf(r *http.Request) string {
	if id := r.Header.Get("X-Correlation-ID"); id != "" {
		return id
	}
	return uuid.NewString()
}
