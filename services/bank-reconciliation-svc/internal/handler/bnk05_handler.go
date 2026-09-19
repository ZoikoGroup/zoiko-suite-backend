// BNK-05's maker-checker manual-match path: ProposeMatch (maker),
// ConfirmMatch (checker, must differ from the maker), RejectProposedMatch
// (checker sends a bad proposal back to EXCEPTION). Additive alongside
// the original single-actor MatchStatementLine in handler.go, which is
// unchanged for backward compatibility.
package handler

import (
	"errors"
	"net/http"

	"github.com/go-chi/chi/v5"
	"go.uber.org/zap"

	"zoiko.io/bank-reconciliation-svc/internal/domain"
)

// ── POST /v1/statement-lines/{statement_line_id}/propose-match ───────────────
//
// Same journal verification as MatchStatementLine (verifyJournalMatches) —
// a proposal must already be a real, deterministic match candidate before
// it can even be proposed; the checker step exists to catch a wrong
// proposal to the WRONG journal despite it independently verifying, not
// to catch an unverified one.
func (h *Handler) ProposeMatch(w http.ResponseWriter, r *http.Request) {
	var req domain.MatchStatementLineRequest
	if !decodeBody(w, r, &req) {
		return
	}
	if req.JournalID == "" && req.TransactionID == "" {
		writeError(w, http.StatusBadRequest, "missing_field", "journal_id or transaction_id")
		return
	}

	tenantID, ok := h.requireTenant(w, r)
	if !ok {
		return
	}
	statementLineID := chi.URLParam(r, "statement_line_id")
	l, err := h.store.GetStatementLine(r.Context(), statementLineID)
	if err != nil {
		h.writeStoreErr(w, "ProposeMatch", err)
		return
	}
	if l == nil {
		writeError(w, http.StatusNotFound, "statement_line_not_found", "")
		return
	}

	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	if err := h.authz.CheckAllowed(r.Context(), principalID, l.LegalEntityID, actionMatch); err != nil {
		h.writeAuthzErr(w, err)
		return
	}

	if h.banking != nil && req.TransactionID != "" {
		if err := h.verifyCanonicalMatch(r.Context(), *l, req.TransactionID); err != nil {
			switch {
			case errors.Is(err, domain.ErrCanonicalTransactionNotFound):
				writeError(w, http.StatusNotFound, "canonical_transaction_not_found", err.Error())
			case errors.Is(err, domain.ErrCanonicalVerificationFailed):
				writeError(w, http.StatusBadRequest, "canonical_verification_failed", err.Error())
			default:
				h.log.Error("banking connector verification unavailable", zap.Error(err))
				writeError(w, http.StatusServiceUnavailable, "banking_connector_unavailable", "")
			}
			return
		}
		if err := h.store.ProposeMatchWithCanonical(r.Context(), tenantID, statementLineID, req.TransactionID, principalID); err != nil {
			h.handleTransitionErr(w, err)
			return
		}
		l.Status = domain.StatementLineStatusPendingConfirmation
		l.ProposedTransactionID = &req.TransactionID
	} else {
		if req.JournalID == "" {
			writeError(w, http.StatusBadRequest, "missing_field", "journal_id is required if transaction_id is omitted")
			return
		}
		if err := h.verifyJournalMatches(r.Context(), *l, req.JournalID); err != nil {
			switch {
			case errors.Is(err, domain.ErrCashAccountUnknown):
				writeError(w, http.StatusUnprocessableEntity, "cash_account_unknown", err.Error())
			case errors.Is(err, domain.ErrLedgerVerificationFailed):
				writeError(w, http.StatusBadRequest, "ledger_verification_failed", err.Error())
			default:
				h.log.Error("ledger verification unavailable — failing closed", zap.Error(err))
				writeError(w, http.StatusServiceUnavailable, "ledger_service_unavailable", "")
			}
			return
		}
		if err := h.store.ProposeMatch(r.Context(), tenantID, statementLineID, req.JournalID, principalID); err != nil {
			h.handleTransitionErr(w, err)
			return
		}
		l.Status = domain.StatementLineStatusPendingConfirmation
		l.ProposedJournalID = &req.JournalID
	}

	l.ProposedByPrincipalID = &principalID
	writeJSON(w, http.StatusOK, l)
}

// ── POST /v1/statement-lines/{statement_line_id}/confirm-match ───────────────
//
// The checker step. Re-verifies the proposed journal against
// general-ledger-svc again (defense in depth: the journal could have been
// reversed or otherwise changed between propose and confirm) before
// finalizing — never trusts that the propose-time verification is still
// true.
func (h *Handler) ConfirmMatch(w http.ResponseWriter, r *http.Request) {
	tenantID, ok := h.requireTenant(w, r)
	if !ok {
		return
	}
	statementLineID := chi.URLParam(r, "statement_line_id")
	l, err := h.store.GetStatementLine(r.Context(), statementLineID)
	if err != nil {
		h.writeStoreErr(w, "ConfirmMatch", err)
		return
	}
	if l == nil {
		writeError(w, http.StatusNotFound, "statement_line_not_found", "")
		return
	}
	if l.Status != domain.StatementLineStatusPendingConfirmation || (l.ProposedJournalID == nil && l.ProposedTransactionID == nil) {
		writeError(w, http.StatusUnprocessableEntity, "invalid_transition", domain.ErrInvalidTransition.Error())
		return
	}

	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	if err := h.authz.CheckAllowed(r.Context(), principalID, l.LegalEntityID, actionMatch); err != nil {
		h.writeAuthzErr(w, err)
		return
	}

	if h.banking != nil && l.ProposedTransactionID != nil {
		if err := h.verifyCanonicalMatch(r.Context(), *l, *l.ProposedTransactionID); err != nil {
			switch {
			case errors.Is(err, domain.ErrCanonicalTransactionNotFound):
				writeError(w, http.StatusNotFound, "canonical_transaction_not_found", err.Error())
			case errors.Is(err, domain.ErrCanonicalVerificationFailed):
				writeError(w, http.StatusBadRequest, "canonical_verification_failed", err.Error())
			default:
				h.log.Error("banking connector verification unavailable", zap.Error(err))
				writeError(w, http.StatusServiceUnavailable, "banking_connector_unavailable", "")
			}
			return
		}
	} else if l.ProposedJournalID != nil {
		if err := h.verifyJournalMatches(r.Context(), *l, *l.ProposedJournalID); err != nil {
			switch {
			case errors.Is(err, domain.ErrCashAccountUnknown):
				writeError(w, http.StatusUnprocessableEntity, "cash_account_unknown", err.Error())
			case errors.Is(err, domain.ErrLedgerVerificationFailed):
				writeError(w, http.StatusBadRequest, "ledger_verification_failed", err.Error())
			default:
				h.log.Error("ledger verification unavailable — failing closed", zap.Error(err))
				writeError(w, http.StatusServiceUnavailable, "ledger_service_unavailable", "")
			}
			return
		}
	}

	confirmed, err := h.store.ConfirmMatch(r.Context(), tenantID, statementLineID, principalID)
	if err != nil {
		h.writeConfirmMatchErr(w, err)
		return
	}
	h.publisher.PublishReconciliationMatched(r.Context(), *confirmed)
	writeJSON(w, http.StatusOK, confirmed)
}

// ── POST /v1/statement-lines/{statement_line_id}/reject-match ────────────────
func (h *Handler) RejectProposedMatch(w http.ResponseWriter, r *http.Request) {
	var req domain.FlagExceptionRequest
	if !decodeBody(w, r, &req) {
		return
	}
	if req.Reason == "" {
		writeError(w, http.StatusBadRequest, "missing_field", "reason")
		return
	}

	tenantID, ok := h.requireTenant(w, r)
	if !ok {
		return
	}
	statementLineID := chi.URLParam(r, "statement_line_id")
	l, err := h.store.GetStatementLine(r.Context(), statementLineID)
	if err != nil {
		h.writeStoreErr(w, "RejectProposedMatch", err)
		return
	}
	if l == nil {
		writeError(w, http.StatusNotFound, "statement_line_not_found", "")
		return
	}

	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	if err := h.authz.CheckAllowed(r.Context(), principalID, l.LegalEntityID, actionMatch); err != nil {
		h.writeAuthzErr(w, err)
		return
	}

	if err := h.store.RejectProposedMatch(r.Context(), tenantID, statementLineID, req.Reason, principalID); err != nil {
		h.handleTransitionErr(w, err)
		return
	}

	l.Status = domain.StatementLineStatusException
	l.ExceptionReason = &req.Reason
	l.FlaggedByPrincipalID = &principalID
	l.ProposedJournalID = nil
	l.ProposedByPrincipalID = nil
	h.publisher.PublishReconciliationExceptionRaised(r.Context(), *l)
	writeJSON(w, http.StatusOK, l)
}

func (h *Handler) writeConfirmMatchErr(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, domain.ErrStatementLineNotFound):
		writeError(w, http.StatusNotFound, "statement_line_not_found", "")
	case errors.Is(err, domain.ErrMatchSelfConfirmation):
		writeError(w, http.StatusForbidden, "self_confirmation_forbidden", err.Error())
	case errors.Is(err, domain.ErrInvalidTransition):
		writeError(w, http.StatusUnprocessableEntity, "invalid_transition", err.Error())
	default:
		h.log.Error("ConfirmMatch: store unavailable", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store_unavailable", "")
	}
}
