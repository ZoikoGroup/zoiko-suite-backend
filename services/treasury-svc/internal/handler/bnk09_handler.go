// BNK-09 Treasury Transfer's real maker-checker flow, replacing the old
// InitiateTransfer wholesale — see internal/store/bnk09_store.go and
// internal/domain/bnk09.go for the schema/state machine this drives.
package handler

import (
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"go.uber.org/zap"

	"zoiko.io/treasury-svc/internal/domain"
	svcmiddleware "zoiko.io/treasury-svc/internal/middleware"
)

func (h *Handler) writeTransferErr(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, domain.ErrTransferNotFound):
		writeError(w, http.StatusNotFound, "transfer_not_found", err.Error())
	case errors.Is(err, domain.ErrInvalidTransferTransition):
		writeError(w, http.StatusConflict, "invalid_transition", err.Error())
	case errors.Is(err, domain.ErrTransferSelfApproval):
		writeError(w, http.StatusForbidden, "self_approval_forbidden", err.Error())
	case errors.Is(err, domain.ErrTransferHashMismatch):
		writeError(w, http.StatusConflict, "protected_fields_changed", err.Error())
	case errors.Is(err, domain.ErrPaymentAdapterUnavailable), errors.Is(err, domain.ErrGLServiceUnavailable), errors.Is(err, domain.ErrIntercompanyServiceUnavailable):
		writeError(w, http.StatusServiceUnavailable, "dependency_unavailable", err.Error())
	case errors.Is(err, domain.ErrPaymentRejected):
		writeError(w, http.StatusUnprocessableEntity, "payment_rejected", err.Error())
	case errors.Is(err, domain.ErrOnlyMakerMayModifyTransfer):
		writeError(w, http.StatusForbidden, "only_maker_may_modify", err.Error())
	case errors.Is(err, domain.ErrInvalidResolution):
		writeError(w, http.StatusBadRequest, "invalid_resolution", err.Error())
	default:
		h.log.Error("treasury transfer operation failed", zap.Error(err))
		writeError(w, http.StatusInternalServerError, "transfer_operation_failed", err.Error())
	}
}

// CreateTreasuryTransfer handles POST /v1/treasury/transfers — BNK-09's
// maker entry point. Reuses InitiateTransferRequest's shape (unchanged on
// the wire) and the same dual-legal-entity authorization and
// fail-closed liquidity-threshold checks the old InitiateTransfer had,
// but writes a PENDING_APPROVAL treasury_transfers row instead of moving
// money directly — the real movement now happens in ExecuteTreasuryTransfer,
// only after a checker has approved.
func (h *Handler) CreateTreasuryTransfer(w http.ResponseWriter, r *http.Request) {
	var req domain.InitiateTransferRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_json", err.Error())
		return
	}
	if req.Amount <= 0 {
		writeError(w, http.StatusBadRequest, "invalid_amount", string(domain.ErrInvalidAmount))
		return
	}
	correlationID := r.Header.Get("X-Correlation-ID")
	if req.CorrelationID != "" {
		correlationID = req.CorrelationID
	}
	if correlationID == "" {
		writeError(w, http.StatusBadRequest, "missing_field", "correlation_id")
		return
	}

	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}

	srcAcct, err := h.store.GetBankAccount(r.Context(), req.SourceBankAccountID)
	if err != nil || srcAcct == nil {
		writeError(w, http.StatusNotFound, "source_account_not_found", "")
		return
	}
	if err := h.authz.CheckAllowed(r.Context(), principalID, srcAcct.LegalEntityID, actionInitiateTransfer); err != nil {
		h.writeAuthzErr(w, err)
		return
	}

	tgtAcct, err := h.store.GetBankAccount(r.Context(), req.TargetBankAccountID)
	if err != nil || tgtAcct == nil {
		writeError(w, http.StatusNotFound, "target_account_not_found", "")
		return
	}
	isCrossEntity := tgtAcct.LegalEntityID != srcAcct.LegalEntityID
	if isCrossEntity {
		if err := h.authz.CheckAllowed(r.Context(), principalID, tgtAcct.LegalEntityID, actionInitiateTransfer); err != nil {
			h.writeAuthzErr(w, err)
			return
		}
	}

	// BNK-01's own negative path ("unverified account used for payment")
	// — a treasury transfer is exactly the protected outbound use
	// IsOwnershipVerified exists to gate. Fail closed on either check.
	srcVerified, err := h.store.IsOwnershipVerified(r.Context(), srcAcct.TenantID, srcAcct.BankAccountID)
	if err != nil {
		h.log.Error("failed to check source account ownership verification — failing closed", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "ownership_check_failed", "cannot verify source account ownership before transfer")
		return
	}
	if !srcVerified {
		writeError(w, http.StatusPreconditionFailed, "source_account_unverified", "source bank account ownership has not been verified")
		return
	}
	tgtVerified, err := h.store.IsOwnershipVerified(r.Context(), tgtAcct.TenantID, tgtAcct.BankAccountID)
	if err != nil {
		h.log.Error("failed to check target account ownership verification — failing closed", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "ownership_check_failed", "cannot verify target account ownership before transfer")
		return
	}
	if !tgtVerified {
		writeError(w, http.StatusPreconditionFailed, "target_account_unverified", "target bank account ownership has not been verified")
		return
	}

	// Fail CLOSED on liquidity threshold errors, same as the old
	// InitiateTransfer — every other cross-service check in this
	// codebase fails closed on store error.
	bal, balErr := h.store.GetLatestCashBalance(r.Context(), req.SourceBankAccountID)
	if balErr != nil {
		h.log.Error("failed to read cash balance for threshold check — failing closed", zap.Error(balErr))
		writeError(w, http.StatusServiceUnavailable, "balance_check_failed", "cannot verify balance before transfer")
		return
	}
	if bal != nil {
		threshold, threshErr := h.store.GetLiquidityThreshold(r.Context(), srcAcct.LegalEntityID, req.CurrencyCode)
		if threshErr != nil {
			h.log.Error("failed to read liquidity threshold — failing closed", zap.Error(threshErr))
			writeError(w, http.StatusServiceUnavailable, "threshold_check_failed", "cannot verify liquidity threshold before transfer")
			return
		}
		if threshold != nil && bal.AvailableBalance-req.Amount < threshold.MinimumRequiredBalance {
			h.log.Warn("transfer blocked: threshold breach on source account", zap.String("account_id", req.SourceBankAccountID))
			writeError(w, http.StatusPreconditionFailed, "minimum_balance_breach", string(domain.ErrMinimumBalanceBreach))
			return
		}
	}

	transfer, created, err := h.store.CreateTreasuryTransfer(r.Context(), domain.CreateTreasuryTransferParams{
		TenantID: srcAcct.TenantID, SourceBankAccountID: req.SourceBankAccountID, TargetBankAccountID: req.TargetBankAccountID,
		Amount: req.Amount, CurrencyCode: req.CurrencyCode, IsCrossEntity: isCrossEntity,
		CorrelationID: correlationID, MakerPrincipalID: principalID, SaveAsDraft: req.SaveAsDraft,
	})
	if err != nil {
		h.writeTransferErr(w, err)
		return
	}
	status := http.StatusCreated
	if !created {
		status = http.StatusOK
	}
	writeJSON(w, status, transfer)
}

func (h *Handler) GetTreasuryTransfer(w http.ResponseWriter, r *http.Request) {
	tenantID := svcmiddleware.TenantFromContext(r.Context())
	transferID := chi.URLParam(r, "transferID")
	transfer, err := h.store.GetTreasuryTransfer(r.Context(), tenantID, transferID)
	if err != nil {
		h.writeTransferErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, transfer)
}

// ApproveTreasuryTransfer handles POST /v1/treasury/transfers/{id}/approve
// — the checker step. Self-approval and protected-field-hash mismatch are
// both real, store-enforced rejections (domain.ErrTransferSelfApproval,
// domain.ErrTransferHashMismatch), not just an app-layer suggestion.
func (h *Handler) ApproveTreasuryTransfer(w http.ResponseWriter, r *http.Request) {
	tenantID := svcmiddleware.TenantFromContext(r.Context())
	transferID := chi.URLParam(r, "transferID")
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	transfer, err := h.store.GetTreasuryTransfer(r.Context(), tenantID, transferID)
	if err != nil {
		h.writeTransferErr(w, err)
		return
	}
	if err := h.authz.CheckAllowed(r.Context(), principalID, transfer.TenantID, actionApproveTransfer); err != nil {
		h.writeAuthzErr(w, err)
		return
	}
	updated, err := h.store.ApproveTreasuryTransfer(r.Context(), domain.ApproveTreasuryTransferParams{
		TenantID: tenantID, TransferID: transferID, CheckerPrincipalID: principalID,
	})
	if err != nil {
		h.writeTransferErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, updated)
}

func (h *Handler) RejectTreasuryTransfer(w http.ResponseWriter, r *http.Request) {
	tenantID := svcmiddleware.TenantFromContext(r.Context())
	transferID := chi.URLParam(r, "transferID")
	var req struct {
		Reason string `json:"reason"`
	}
	_ = json.NewDecoder(r.Body).Decode(&req)
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	updated, err := h.store.RejectTreasuryTransfer(r.Context(), domain.RejectTreasuryTransferParams{
		TenantID: tenantID, TransferID: transferID, CheckerPrincipalID: principalID, Reason: req.Reason,
	})
	if err != nil {
		h.writeTransferErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, updated)
}

// ExecuteTreasuryTransfer handles POST /v1/treasury/transfers/{id}/execute
// — the resumable saga driver. It is safe to call repeatedly (a retry
// after a timeout, a crash mid-saga, or an explicit resume): each branch
// below only performs the one step the transfer's CURRENT status still
// needs, and every external call it makes is itself idempotent
// (payment-initiation-adapter-svc on transferID, general-ledger-svc on
// source_event_id=transferID, intercompany-accounting-svc on
// source_journal_id), so re-entering a step that already partially
// succeeded never double-executes its real-world consequence.
func (h *Handler) ExecuteTreasuryTransfer(w http.ResponseWriter, r *http.Request) {
	tenantID := svcmiddleware.TenantFromContext(r.Context())
	transferID := chi.URLParam(r, "transferID")
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	correlationID := r.Header.Get("X-Correlation-ID")

	transfer, err := h.store.GetTreasuryTransfer(r.Context(), tenantID, transferID)
	if err != nil {
		h.writeTransferErr(w, err)
		return
	}
	if transfer.Status == domain.TransferCompleted {
		writeJSON(w, http.StatusOK, transfer)
		return
	}
	if !domain.CanExecuteTransfer(transfer.Status) {
		h.writeTransferErr(w, domain.ErrInvalidTransferTransition)
		return
	}
	if err := h.authz.CheckAllowed(r.Context(), principalID, transfer.TenantID, actionExecuteTransfer); err != nil {
		h.writeAuthzErr(w, err)
		return
	}

	srcAcct, err := h.store.GetBankAccount(r.Context(), transfer.SourceBankAccountID)
	if err != nil || srcAcct == nil {
		writeError(w, http.StatusInternalServerError, "source_account_missing", "source bank account for this transfer could not be loaded")
		return
	}
	tgtAcct, err := h.store.GetBankAccount(r.Context(), transfer.TargetBankAccountID)
	if err != nil || tgtAcct == nil {
		writeError(w, http.StatusInternalServerError, "target_account_missing", "target bank account for this transfer could not be loaded")
		return
	}

	if transfer.Status == domain.TransferApproved {
		attemptID, err := h.transferClients.SubmitTreasuryPayment(r.Context(), tenantID, principalID, correlationID,
			srcAcct.LegalEntityID, transfer.TransferID, transfer.SourceBankAccountID, transfer.TargetBankAccountID, transfer.Amount, transfer.CurrencyCode)
		if err != nil {
			h.writeTransferErr(w, err)
			return
		}
		transfer, err = h.store.MarkTransferSubmitted(r.Context(), tenantID, transferID, attemptID)
		if err != nil {
			h.writeTransferErr(w, err)
			return
		}
	}

	if transfer.Status == domain.TransferSubmitted {
		if !transfer.IsCrossEntity {
			transfer, err = h.store.MarkTransferCompleted(r.Context(), tenantID, transferID)
			if err != nil {
				h.writeTransferErr(w, err)
				return
			}
			writeJSON(w, http.StatusOK, transfer)
			return
		}
		// Cross-entity: post the journal before pairing intercompany.
		// fiscal_period is the calendar month — treasury-svc has no
		// fiscal-calendar dependency of its own to resolve a real
		// accounting period, an honest simplification documented here
		// rather than fabricating a fiscal-calendar integration.
		fiscalPeriod := time.Now().UTC().Format("2006-01")
		journalID, err := h.transferClients.PostTreasuryTransferJournal(r.Context(), tenantID, principalID, correlationID,
			srcAcct.LegalEntityID, fiscalPeriod, transfer.TransferID, transfer.Amount)
		if err != nil {
			h.writeTransferErr(w, err)
			return
		}
		transfer, err = h.store.MarkTransferLedgerPosted(r.Context(), tenantID, transferID, journalID)
		if err != nil {
			h.writeTransferErr(w, err)
			return
		}
	}

	if transfer.Status == domain.TransferLedgerPosted {
		entryID, err := h.transferClients.PairTreasuryTransferIntercompany(r.Context(), tenantID, principalID, correlationID,
			srcAcct.LegalEntityID, tgtAcct.LegalEntityID, transfer.SourceJournalID, transfer.Amount, transfer.CurrencyCode)
		if err != nil {
			h.writeTransferErr(w, err)
			return
		}
		transfer, err = h.store.MarkTransferIntercompanyPaired(r.Context(), tenantID, transferID, entryID)
		if err != nil {
			h.writeTransferErr(w, err)
			return
		}
	}

	if transfer.Status == domain.TransferIntercompanyPaired {
		transfer, err = h.store.MarkTransferCompleted(r.Context(), tenantID, transferID)
		if err != nil {
			h.writeTransferErr(w, err)
			return
		}
	}

	writeJSON(w, http.StatusOK, transfer)
}

// AmendTreasuryTransfer handles POST /v1/treasury/transfers/{id}/amend —
// the maker's own DRAFT-only correction path. Reuses
// InitiateTransferRequest's shape (source/target account, amount,
// currency) since it's the same protected-field set CreateTreasuryTransfer
// itself accepts.
func (h *Handler) AmendTreasuryTransfer(w http.ResponseWriter, r *http.Request) {
	tenantID := svcmiddleware.TenantFromContext(r.Context())
	transferID := chi.URLParam(r, "transferID")
	var req domain.InitiateTransferRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_json", err.Error())
		return
	}
	if req.Amount <= 0 {
		writeError(w, http.StatusBadRequest, "invalid_amount", string(domain.ErrInvalidAmount))
		return
	}
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	transfer, err := h.store.GetTreasuryTransfer(r.Context(), tenantID, transferID)
	if err != nil {
		h.writeTransferErr(w, err)
		return
	}
	if err := h.authz.CheckAllowed(r.Context(), principalID, transfer.TenantID, actionModifyTransfer); err != nil {
		h.writeAuthzErr(w, err)
		return
	}
	updated, err := h.store.AmendTreasuryTransfer(r.Context(), domain.AmendTreasuryTransferParams{
		TenantID: tenantID, TransferID: transferID, SourceBankAccountID: req.SourceBankAccountID,
		TargetBankAccountID: req.TargetBankAccountID, Amount: req.Amount, CurrencyCode: req.CurrencyCode,
		ActorPrincipalID: principalID,
	})
	if err != nil {
		h.writeTransferErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, updated)
}

// SubmitTransferForApproval handles
// POST /v1/treasury/transfers/{id}/submit-for-approval — the maker's own
// DRAFT->PENDING_APPROVAL command. Distinct from ExecuteTreasuryTransfer's
// internal MarkTransferSubmitted (BNK-06 accepting the payment attempt,
// much later in the lifecycle) despite the similar name — the doc itself
// names both SubmitTransfer and the Submitted/Pending state as distinct
// concepts from CreateTreasuryTransfer/ApproveTreasuryTransfer.
func (h *Handler) SubmitTransferForApproval(w http.ResponseWriter, r *http.Request) {
	tenantID := svcmiddleware.TenantFromContext(r.Context())
	transferID := chi.URLParam(r, "transferID")
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	transfer, err := h.store.GetTreasuryTransfer(r.Context(), tenantID, transferID)
	if err != nil {
		h.writeTransferErr(w, err)
		return
	}
	if err := h.authz.CheckAllowed(r.Context(), principalID, transfer.TenantID, actionModifyTransfer); err != nil {
		h.writeAuthzErr(w, err)
		return
	}
	updated, err := h.store.SubmitTransferForApproval(r.Context(), domain.SubmitTransferForApprovalParams{
		TenantID: tenantID, TransferID: transferID, ActorPrincipalID: principalID,
	})
	if err != nil {
		h.writeTransferErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, updated)
}

// CancelBeforeSubmission handles POST /v1/treasury/transfers/{id}/cancel —
// the doc's own command name. Only the maker may cancel their own
// transfer, and only before the bank has ever seen it.
func (h *Handler) CancelBeforeSubmission(w http.ResponseWriter, r *http.Request) {
	tenantID := svcmiddleware.TenantFromContext(r.Context())
	transferID := chi.URLParam(r, "transferID")
	var req struct {
		Reason string `json:"reason"`
	}
	_ = json.NewDecoder(r.Body).Decode(&req)
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	transfer, err := h.store.GetTreasuryTransfer(r.Context(), tenantID, transferID)
	if err != nil {
		h.writeTransferErr(w, err)
		return
	}
	if err := h.authz.CheckAllowed(r.Context(), principalID, transfer.TenantID, actionModifyTransfer); err != nil {
		h.writeAuthzErr(w, err)
		return
	}
	updated, err := h.store.CancelBeforeSubmission(r.Context(), domain.CancelBeforeSubmissionParams{
		TenantID: tenantID, TransferID: transferID, Reason: req.Reason, ActorPrincipalID: principalID,
	})
	if err != nil {
		h.writeTransferErr(w, err)
		return
	}
	h.publisher.PublishTreasuryTransferCancelled(r.Context(), r.Header.Get("X-Correlation-ID"), principalID, *updated)
	writeJSON(w, http.StatusOK, updated)
}

// MarkTransferReturned handles
// POST /v1/treasury/transfers/{id}/mark-returned — an operator action (not
// maker-restricted) recording that the bank returned an already-submitted
// transfer unexecuted. This is the real trigger for the doc's
// TreasuryTransferReturned event; nothing in this codebase polls for
// returns automatically yet, so this is the explicit, evidenced entry
// point until such a consumer exists.
func (h *Handler) MarkTransferReturned(w http.ResponseWriter, r *http.Request) {
	tenantID := svcmiddleware.TenantFromContext(r.Context())
	transferID := chi.URLParam(r, "transferID")
	var req struct {
		Reason string `json:"reason"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Reason == "" {
		writeError(w, http.StatusBadRequest, "missing_field", "reason is required")
		return
	}
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	transfer, err := h.store.GetTreasuryTransfer(r.Context(), tenantID, transferID)
	if err != nil {
		h.writeTransferErr(w, err)
		return
	}
	if err := h.authz.CheckAllowed(r.Context(), principalID, transfer.TenantID, actionResolveTransfer); err != nil {
		h.writeAuthzErr(w, err)
		return
	}
	updated, err := h.store.MarkTransferReturned(r.Context(), domain.MarkTransferReturnedParams{
		TenantID: tenantID, TransferID: transferID, Reason: req.Reason, ActorPrincipalID: principalID,
	})
	if err != nil {
		h.writeTransferErr(w, err)
		return
	}
	h.publisher.PublishTreasuryTransferReturned(r.Context(), r.Header.Get("X-Correlation-ID"), principalID, *updated)
	writeJSON(w, http.StatusOK, updated)
}

// ResolveTreasuryTransfer handles
// POST /v1/treasury/transfers/{id}/resolve — the doc's own command name.
// resolution is "RESUBMIT" (back to PENDING_APPROVAL for a fresh
// maker-checker cycle) or "CANCEL" (terminal).
func (h *Handler) ResolveTreasuryTransfer(w http.ResponseWriter, r *http.Request) {
	tenantID := svcmiddleware.TenantFromContext(r.Context())
	transferID := chi.URLParam(r, "transferID")
	var req struct {
		Resolution string `json:"resolution"`
		Note       string `json:"note"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_json", err.Error())
		return
	}
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	transfer, err := h.store.GetTreasuryTransfer(r.Context(), tenantID, transferID)
	if err != nil {
		h.writeTransferErr(w, err)
		return
	}
	if err := h.authz.CheckAllowed(r.Context(), principalID, transfer.TenantID, actionResolveTransfer); err != nil {
		h.writeAuthzErr(w, err)
		return
	}
	updated, err := h.store.ResolveTreasuryTransfer(r.Context(), domain.ResolveTreasuryTransferParams{
		TenantID: tenantID, TransferID: transferID, Resolution: req.Resolution, Note: req.Note, ActorPrincipalID: principalID,
	})
	if err != nil {
		h.writeTransferErr(w, err)
		return
	}
	if updated.Status == domain.TransferCancelled {
		h.publisher.PublishTreasuryTransferCancelled(r.Context(), r.Header.Get("X-Correlation-ID"), principalID, *updated)
	}
	writeJSON(w, http.StatusOK, updated)
}
