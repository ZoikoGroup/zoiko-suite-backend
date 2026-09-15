// BNK-01's own HTTP surface — ownership verification, metadata amendment,
// operational-status lifecycle and identifier-token rotation, added to
// treasury-svc's existing /v1/treasury/accounts routes rather than a new
// service.
package handler

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/go-chi/chi/v5"
	"go.uber.org/zap"

	"zoiko.io/treasury-svc/internal/domain"
	svcmiddleware "zoiko.io/treasury-svc/internal/middleware"
)

const (
	actionVerifyOwnership = "BANK_ACCOUNT_VERIFY"
	actionAmendAccount    = "BANK_ACCOUNT_MANAGE"
	actionCloseAccount    = "BANK_ACCOUNT_CLOSE"
)

func (h *Handler) fetchAccountForAuth(w http.ResponseWriter, r *http.Request, accountID string) (*domain.BankAccount, bool) {
	acct, err := h.store.GetBankAccount(r.Context(), accountID)
	if err != nil {
		h.log.Error("fetchAccountForAuth: store unavailable", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store_error", err.Error())
		return nil, false
	}
	if acct == nil {
		writeError(w, http.StatusNotFound, "bank_account_not_found", "")
		return nil, false
	}
	return acct, true
}

func (h *Handler) writeBankAccountErr(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, domain.ErrBankAccountNotFound):
		writeError(w, http.StatusNotFound, "bank_account_not_found", "")
	case errors.Is(err, domain.ErrInvalidTransition):
		writeError(w, http.StatusConflict, "invalid_transition", string(domain.ErrInvalidTransition))
	default:
		h.log.Error("bank account operation failed", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store_error", err.Error())
	}
}

type verifyOwnershipRequest struct {
	VerificationMethod string `json:"verification_method"`
	EvidenceRef        string `json:"evidence_ref"`
}

func (h *Handler) VerifyBankAccountOwnership(w http.ResponseWriter, r *http.Request) {
	var req verifyOwnershipRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_json", err.Error())
		return
	}
	if req.VerificationMethod == "" || req.EvidenceRef == "" {
		writeError(w, http.StatusBadRequest, "missing_field", "verification_method and evidence_ref are required")
		return
	}
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	accountID := chi.URLParam(r, "accountID")
	acct, ok := h.fetchAccountForAuth(w, r, accountID)
	if !ok {
		return
	}
	if err := h.authz.CheckAllowed(r.Context(), principalID, acct.LegalEntityID, actionVerifyOwnership); err != nil {
		h.writeAuthzErr(w, err)
		return
	}
	tenantID := svcmiddleware.TenantFromContext(r.Context())
	evidence, err := h.store.VerifyBankAccountOwnership(r.Context(), domain.VerifyOwnershipParams{
		BankAccountID: accountID, TenantID: tenantID, VerificationMethod: req.VerificationMethod,
		EvidenceRef: req.EvidenceRef, VerifiedByPrincipalID: principalID,
	})
	if err != nil {
		h.writeBankAccountErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, evidence)
}

func (h *Handler) GetOwnershipEvidence(w http.ResponseWriter, r *http.Request) {
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	accountID := chi.URLParam(r, "accountID")
	acct, ok := h.fetchAccountForAuth(w, r, accountID)
	if !ok {
		return
	}
	if err := h.authz.CheckAllowed(r.Context(), principalID, acct.LegalEntityID, actionViewPositions); err != nil {
		h.writeAuthzErr(w, err)
		return
	}
	tenantID := svcmiddleware.TenantFromContext(r.Context())
	evidence, err := h.store.ListOwnershipEvidence(r.Context(), tenantID, accountID)
	if err != nil {
		h.log.Error("GetOwnershipEvidence: store unavailable", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store_error", err.Error())
		return
	}
	if evidence == nil {
		evidence = []domain.OwnershipEvidence{}
	}
	writeJSON(w, http.StatusOK, evidence)
}

type amendMetadataRequest struct {
	AccountName    string `json:"account_name"`
	BranchRef      string `json:"branch_ref"`
	BankIdentifier string `json:"bank_identifier"`
	Country        string `json:"country"`
	AccountType    string `json:"account_type"`
}

func (h *Handler) AmendBankAccountMetadata(w http.ResponseWriter, r *http.Request) {
	var req amendMetadataRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_json", err.Error())
		return
	}
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	accountID := chi.URLParam(r, "accountID")
	acct, ok := h.fetchAccountForAuth(w, r, accountID)
	if !ok {
		return
	}
	if !domain.CanAmendMetadata(acct.AccountStatus) {
		writeError(w, http.StatusConflict, "invalid_transition", string(domain.ErrInvalidTransition))
		return
	}
	if err := h.authz.CheckAllowed(r.Context(), principalID, acct.LegalEntityID, actionAmendAccount); err != nil {
		h.writeAuthzErr(w, err)
		return
	}
	tenantID := svcmiddleware.TenantFromContext(r.Context())
	updated, err := h.store.AmendBankAccountMetadata(r.Context(), domain.AmendBankAccountMetadataParams{
		BankAccountID: accountID, TenantID: tenantID, AccountName: req.AccountName, BranchRef: req.BranchRef,
		BankIdentifier: req.BankIdentifier, Country: req.Country, AccountType: req.AccountType, ActorPrincipalID: principalID,
	})
	if err != nil {
		h.writeBankAccountErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, updated)
}

type changeOperationalUseRequest struct {
	RequestedOperationalUse string `json:"requested_operational_use"`
}

func (h *Handler) ChangeOperationalUse(w http.ResponseWriter, r *http.Request) {
	var req changeOperationalUseRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_json", err.Error())
		return
	}
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	accountID := chi.URLParam(r, "accountID")
	acct, ok := h.fetchAccountForAuth(w, r, accountID)
	if !ok {
		return
	}
	if !domain.CanChangeOperationalUse(acct.AccountStatus) {
		writeError(w, http.StatusConflict, "invalid_transition", string(domain.ErrInvalidTransition))
		return
	}
	if err := h.authz.CheckAllowed(r.Context(), principalID, acct.LegalEntityID, actionAmendAccount); err != nil {
		h.writeAuthzErr(w, err)
		return
	}
	tenantID := svcmiddleware.TenantFromContext(r.Context())
	updated, err := h.store.ChangeOperationalUse(r.Context(), domain.ChangeOperationalUseParams{
		BankAccountID: accountID, TenantID: tenantID, RequestedOperationalUse: req.RequestedOperationalUse, ActorPrincipalID: principalID,
	})
	if err != nil {
		h.writeBankAccountErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, updated)
}

type reasonRequest struct {
	Reason string `json:"reason"`
}

func (h *Handler) SuspendBankAccount(w http.ResponseWriter, r *http.Request) {
	var req reasonRequest
	_ = json.NewDecoder(r.Body).Decode(&req)
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	accountID := chi.URLParam(r, "accountID")
	acct, ok := h.fetchAccountForAuth(w, r, accountID)
	if !ok {
		return
	}
	if !domain.CanSuspendAccount(acct.AccountStatus) {
		writeError(w, http.StatusConflict, "invalid_transition", string(domain.ErrInvalidTransition))
		return
	}
	if err := h.authz.CheckAllowed(r.Context(), principalID, acct.LegalEntityID, actionAmendAccount); err != nil {
		h.writeAuthzErr(w, err)
		return
	}
	tenantID := svcmiddleware.TenantFromContext(r.Context())
	updated, err := h.store.SuspendBankAccount(r.Context(), domain.SuspendAccountParams{BankAccountID: accountID, TenantID: tenantID, Reason: req.Reason, ActorPrincipalID: principalID})
	if err != nil {
		h.writeBankAccountErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, updated)
}

func (h *Handler) ReactivateBankAccount(w http.ResponseWriter, r *http.Request) {
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	accountID := chi.URLParam(r, "accountID")
	acct, ok := h.fetchAccountForAuth(w, r, accountID)
	if !ok {
		return
	}
	if !domain.CanReactivateAccount(acct.AccountStatus) {
		writeError(w, http.StatusConflict, "invalid_transition", string(domain.ErrInvalidTransition))
		return
	}
	if err := h.authz.CheckAllowed(r.Context(), principalID, acct.LegalEntityID, actionAmendAccount); err != nil {
		h.writeAuthzErr(w, err)
		return
	}
	tenantID := svcmiddleware.TenantFromContext(r.Context())
	updated, err := h.store.ReactivateBankAccount(r.Context(), domain.ReactivateAccountParams{BankAccountID: accountID, TenantID: tenantID, ActorPrincipalID: principalID})
	if err != nil {
		h.writeBankAccountErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, updated)
}

func (h *Handler) CloseBankAccount(w http.ResponseWriter, r *http.Request) {
	var req reasonRequest
	_ = json.NewDecoder(r.Body).Decode(&req)
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	accountID := chi.URLParam(r, "accountID")
	acct, ok := h.fetchAccountForAuth(w, r, accountID)
	if !ok {
		return
	}
	if !domain.CanCloseAccount(acct.AccountStatus) {
		writeError(w, http.StatusConflict, "invalid_transition", string(domain.ErrInvalidTransition))
		return
	}
	if err := h.authz.CheckAllowed(r.Context(), principalID, acct.LegalEntityID, actionCloseAccount); err != nil {
		h.writeAuthzErr(w, err)
		return
	}
	tenantID := svcmiddleware.TenantFromContext(r.Context())
	updated, err := h.store.CloseBankAccount(r.Context(), domain.CloseAccountParams{BankAccountID: accountID, TenantID: tenantID, Reason: req.Reason, ActorPrincipalID: principalID})
	if err != nil {
		h.writeBankAccountErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, updated)
}

type rotateTokenRequest struct {
	NewMaskedAccountNumber string `json:"new_masked_account_number"`
	NewBankIdentifier      string `json:"new_bank_identifier"`
}

func (h *Handler) RotateAccountIdentifierToken(w http.ResponseWriter, r *http.Request) {
	var req rotateTokenRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_json", err.Error())
		return
	}
	if req.NewMaskedAccountNumber == "" {
		writeError(w, http.StatusBadRequest, "missing_field", "new_masked_account_number is required")
		return
	}
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	accountID := chi.URLParam(r, "accountID")
	acct, ok := h.fetchAccountForAuth(w, r, accountID)
	if !ok {
		return
	}
	if !domain.CanRotateAccountToken(acct.AccountStatus) {
		writeError(w, http.StatusConflict, "invalid_transition", string(domain.ErrInvalidTransition))
		return
	}
	if err := h.authz.CheckAllowed(r.Context(), principalID, acct.LegalEntityID, actionAmendAccount); err != nil {
		h.writeAuthzErr(w, err)
		return
	}
	newBankIdentifier := req.NewBankIdentifier
	if newBankIdentifier == "" {
		newBankIdentifier = acct.BankIdentifier
	}
	tenantID := svcmiddleware.TenantFromContext(r.Context())
	updated, err := h.store.RotateAccountIdentifierToken(r.Context(), domain.RotateAccountTokenParams{
		BankAccountID: accountID, TenantID: tenantID, NewMaskedAccountNumber: req.NewMaskedAccountNumber,
		NewBankIdentifier: newBankIdentifier, ActorPrincipalID: principalID,
	})
	if err != nil {
		h.writeBankAccountErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, updated)
}
