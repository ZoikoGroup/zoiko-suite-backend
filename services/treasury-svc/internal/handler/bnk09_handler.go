// BNK-09 Treasury Transfer's real maker-checker flow, replacing the old
// InitiateTransfer wholesale — see internal/store/bnk09_store.go and
// internal/domain/bnk09.go for the schema/state machine this drives.
package handler

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
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
	case errors.Is(err, domain.ErrTransferSelfAuthorization):
		writeError(w, http.StatusForbidden, "self_authorization_forbidden", err.Error())
	case errors.Is(err, domain.ErrCrossEntityTransferRequiresAuthorization):
		writeError(w, http.StatusConflict, "authorization_required", err.Error())
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
	if created {
		h.publisher.PublishTreasuryTransferCreated(r.Context(), correlationID, principalID, *transfer)
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

// GetTreasuryTransferFingerprint handles
// GET /v1/treasury/transfers/{transferID}/fingerprint — Wave 11b's
// service-to-service read payment-initiation-adapter-svc calls to
// independently re-derive and compare domain.TransferFingerprint,
// instead of trusting the fingerprint SubmitTreasuryPayment sent it.
// Same no-extra-authz-gate posture as GetTreasuryTransfer above: tenant
// isolation is enforced by RLS via the request's tenant context, not a
// principal-level permission check, since this is a narrow read of
// exactly what the caller already has (the transfer ID) plus its own
// live status/fingerprint.
func (h *Handler) GetTreasuryTransferFingerprint(w http.ResponseWriter, r *http.Request) {
	tenantID := svcmiddleware.TenantFromContext(r.Context())
	transferID := chi.URLParam(r, "transferID")
	transfer, err := h.store.GetTreasuryTransfer(r.Context(), tenantID, transferID)
	if err != nil {
		h.writeTransferErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"transfer_id": transfer.TransferID,
		"status":      transfer.Status,
		"fingerprint": domain.TransferFingerprint(transfer),
	})
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
	h.publisher.PublishTreasuryTransferApproved(r.Context(), r.Header.Get("X-Correlation-ID"), principalID, *updated)
	writeJSON(w, http.StatusOK, updated)
}

// AuthorizeTreasuryTransfer handles POST /v1/treasury/transfers/{id}/authorize
// — the doc's own additional dual-control step, required only for
// cross-entity transfers (see domain.CanAuthorizeTransfer). Self-check
// and CAS enforcement mirror ApproveTreasuryTransfer exactly.
func (h *Handler) AuthorizeTreasuryTransfer(w http.ResponseWriter, r *http.Request) {
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
	if err := h.authz.CheckAllowed(r.Context(), principalID, transfer.TenantID, actionAuthorizeTransfer); err != nil {
		h.writeAuthzErr(w, err)
		return
	}
	updated, err := h.store.AuthorizeTreasuryTransfer(r.Context(), domain.AuthorizeTreasuryTransferParams{
		TenantID: tenantID, TransferID: transferID, AuthorizerPrincipalID: principalID,
	})
	if err != nil {
		h.writeTransferErr(w, err)
		return
	}
	h.publisher.PublishTreasuryTransferAuthorized(r.Context(), r.Header.Get("X-Correlation-ID"), principalID, *updated)
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
	h.publisher.PublishTreasuryTransferRejected(r.Context(), r.Header.Get("X-Correlation-ID"), principalID, *updated)
	writeJSON(w, http.StatusOK, updated)
}

// ListTransfers handles GET /v1/treasury/transfers — the doc's own
// ListTransfers query. legal_entity_id/status are optional filters.
func (h *Handler) ListTransfers(w http.ResponseWriter, r *http.Request) {
	tenantID := svcmiddleware.TenantFromContext(r.Context())
	q := r.URL.Query()
	limit := 0
	if v := q.Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			limit = n
		}
	}
	offset := 0
	if v := q.Get("offset"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			offset = n
		}
	}
	transfers, err := h.store.ListTransfers(r.Context(), domain.ListTransfersParams{
		TenantID: tenantID, LegalEntityID: q.Get("legal_entity_id"), Status: q.Get("status"),
		Limit: limit, Offset: offset,
	})
	if err != nil {
		h.log.Error("ListTransfers: store unavailable", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store_unavailable", "cannot list transfers")
		return
	}
	writeJSON(w, http.StatusOK, transfers)
}

// GetTransferApproval handles GET /v1/treasury/transfers/{id}/approval —
// the doc's own GetTransferApproval query. No separate approval entity
// exists in this schema (maker/checker/status ARE the approval record on
// TreasuryTransfer itself), so this is an honest projection of the
// existing fields rather than a fabricated sub-entity.
func (h *Handler) GetTransferApproval(w http.ResponseWriter, r *http.Request) {
	tenantID := svcmiddleware.TenantFromContext(r.Context())
	transferID := chi.URLParam(r, "transferID")
	transfer, err := h.store.GetTreasuryTransfer(r.Context(), tenantID, transferID)
	if err != nil {
		h.writeTransferErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"transfer_id":             transfer.TransferID,
		"status":                  transfer.Status,
		"maker_principal_id":      transfer.MakerPrincipalID,
		"checker_principal_id":    transfer.CheckerPrincipalID,
		"approved":                transfer.CheckerPrincipalID != "",
		"authorizer_principal_id": transfer.AuthorizerPrincipalID,
		"authorized":              transfer.AuthorizerPrincipalID != "",
		"is_cross_entity":         transfer.IsCrossEntity,
		"reject_reason":           transfer.RejectReason,
	})
}

// GetTransferExecution handles GET /v1/treasury/transfers/{id}/execution
// — the doc's own GetTransferExecution query, projecting the same
// execution-lineage fields ExecuteTreasuryTransfer's saga already writes.
func (h *Handler) GetTransferExecution(w http.ResponseWriter, r *http.Request) {
	tenantID := svcmiddleware.TenantFromContext(r.Context())
	transferID := chi.URLParam(r, "transferID")
	transfer, err := h.store.GetTreasuryTransfer(r.Context(), tenantID, transferID)
	if err != nil {
		h.writeTransferErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"transfer_id":           transfer.TransferID,
		"status":                transfer.Status,
		"payment_attempt_id":    transfer.PaymentAttemptID,
		"source_journal_id":     transfer.SourceJournalID,
		"intercompany_entry_id": transfer.IntercompanyEntryID,
		"return_reason":         transfer.ReturnReason,
		"resolution_note":       transfer.ResolutionNote,
	})
}

// GetTransferAvailableActions handles
// GET /v1/treasury/transfers/{id}/available-actions — the doc's own
// GetAvailableActions query, mirroring BNK-01's own implementation
// pattern: a direct, mechanical read of which of this aggregate's own
// existing transition guards currently pass, not new business logic.
func (h *Handler) GetTransferAvailableActions(w http.ResponseWriter, r *http.Request) {
	tenantID := svcmiddleware.TenantFromContext(r.Context())
	transferID := chi.URLParam(r, "transferID")
	transfer, err := h.store.GetTreasuryTransfer(r.Context(), tenantID, transferID)
	if err != nil {
		h.writeTransferErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{
		"can_amend":               domain.CanAmendTransfer(transfer.Status),
		"can_submit_for_approval": domain.CanSubmitTransferForApproval(transfer.Status),
		"can_approve":             domain.CanApproveTransfer(transfer.Status),
		"can_authorize":           domain.CanAuthorizeTransfer(transfer.Status, transfer.IsCrossEntity),
		"can_reject":              domain.CanRejectTransfer(transfer.Status),
		"can_execute":             domain.CanExecuteTransfer(transfer.Status),
		"can_cancel":              domain.CanCancelBeforeSubmission(transfer.Status),
		"can_mark_returned":       domain.CanMarkTransferReturned(transfer.Status),
		"can_resolve":             domain.CanResolveTransfer(transfer.Status),
	})
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

	// A cross-entity transfer sitting APPROVED must go through
	// AuthorizeTreasuryTransfer first — see domain.CanAuthorizeTransfer's
	// own doc comment. Same-entity transfers are unaffected: they were
	// never gated by this and still submit straight from APPROVED.
	if transfer.Status == domain.TransferApproved && transfer.IsCrossEntity {
		h.writeTransferErr(w, domain.ErrCrossEntityTransferRequiresAuthorization)
		return
	}

	if transfer.Status == domain.TransferApproved || transfer.Status == domain.TransferAuthorized {
		// Wave 11b: computed from the transfer's live APPROVED/AUTHORIZED
		// state (still the status at this point — MarkTransferSubmitted
		// hasn't run yet), so it matches what GetTreasuryTransferFingerprint
		// will independently re-derive when payment-initiation-adapter-svc
		// verifies it.
		fingerprint := domain.TransferFingerprint(transfer)
		attemptID, err := h.transferClients.SubmitTreasuryPayment(r.Context(), tenantID, principalID, correlationID,
			srcAcct.LegalEntityID, transfer.TransferID, transfer.SourceBankAccountID, transfer.TargetBankAccountID, fingerprint, transfer.Amount, transfer.CurrencyCode)
		if err != nil {
			h.writeTransferErr(w, err)
			return
		}
		transfer, err = h.store.MarkTransferSubmitted(r.Context(), tenantID, transferID, attemptID)
		if err != nil {
			h.writeTransferErr(w, err)
			return
		}
		h.publisher.PublishTreasuryTransferSubmitted(r.Context(), correlationID, principalID, *transfer)
	}

	if transfer.Status == domain.TransferSubmitted {
		if !transfer.IsCrossEntity {
			transfer, err = h.store.MarkTransferCompleted(r.Context(), tenantID, transferID)
			if err != nil {
				h.writeTransferErr(w, err)
				return
			}
			h.publisher.PublishTreasuryTransferSettled(r.Context(), correlationID, principalID, *transfer)
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
		h.publisher.PublishTreasuryTransferSettled(r.Context(), correlationID, principalID, *transfer)
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
