// BNK-01's own HTTP surface — ownership verification, metadata amendment,
// operational-status lifecycle and identifier-token rotation, added to
// treasury-svc's existing /v1/treasury/accounts routes rather than a new
// service.
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

const (
	actionVerifyOwnership   = "BANK_ACCOUNT_VERIFY"
	actionAmendAccount      = "BANK_ACCOUNT_MANAGE"
	actionCloseAccount      = "BANK_ACCOUNT_CLOSE"
	actionViewAccountMasked = "BANK_ACCOUNT_VIEW_MASKED"
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
	if acct.CreatedByPrincipalID != "" && acct.CreatedByPrincipalID == principalID {
		writeError(w, http.StatusForbidden, "self_verification_forbidden", string(domain.ErrSelfVerificationForbidden))
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
	h.publisher.PublishBankAccountOwnershipVerified(r.Context(), r.Header.Get("X-Correlation-ID"), principalID, *acct, *evidence)
	writeJSON(w, http.StatusOK, evidence)
}

// GetBankAccountByID handles GET /v1/treasury/accounts/{accountID} — the
// single-account read other Banking services (starting with BNK-02's
// bank_account_id validation) need and which previously didn't exist;
// only ListBankAccounts (legal-entity-scoped list) was available.
// bankAccountDetailResponse adds the derived IsOwnershipVerified fact
// (never a column — see migration 000003's own doc comment on
// domain.OwnershipEvidence) to the single-account read. Deliberately not
// added to domain.BankAccount itself: ListBankAccounts composes this type
// for every account in a legal entity (BNK-08 cash position, etc.), and
// those callers have no use for a per-account ownership-evidence query —
// this field is only computed on the single-account detail path.
type bankAccountDetailResponse struct {
	domain.BankAccount
	IsOwnershipVerified bool `json:"is_ownership_verified"`
}

func (h *Handler) GetBankAccountByID(w http.ResponseWriter, r *http.Request) {
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
	verified, err := h.store.IsOwnershipVerified(r.Context(), acct.TenantID, acct.BankAccountID)
	if err != nil {
		h.log.Error("GetBankAccountByID: IsOwnershipVerified failed — failing closed", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store_error", "cannot verify ownership status")
		return
	}
	writeJSON(w, http.StatusOK, bankAccountDetailResponse{BankAccount: *acct, IsOwnershipVerified: verified})
}

// GetBankAccountAsOf handles GET /v1/treasury/accounts/{accountID}/as-of
// — the historical-reconstruction read Invariant #1 requires ("bank
// account legal-entity ownership and operational status are ...
// historically reconstructable"). Query param at is an RFC3339
// timestamp; defaults to now if omitted (equivalent to the live row).
func (h *Handler) GetBankAccountAsOf(w http.ResponseWriter, r *http.Request) {
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
	asOf := time.Now().UTC()
	if raw := r.URL.Query().Get("at"); raw != "" {
		parsed, err := time.Parse(time.RFC3339, raw)
		if err != nil {
			writeError(w, http.StatusBadRequest, "invalid_field", "at must be an RFC3339 timestamp")
			return
		}
		asOf = parsed
	}
	entry, err := h.store.GetBankAccountAsOf(r.Context(), acct.TenantID, accountID, asOf)
	if err != nil {
		h.log.Error("GetBankAccountAsOf: store unavailable", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store_error", err.Error())
		return
	}
	if entry == nil {
		writeError(w, http.StatusNotFound, "no_history_as_of", "the account had no recorded state as of the given time")
		return
	}
	writeJSON(w, http.StatusOK, entry)
}

// bankAccountMaskedResponse is BNK-01's safe-default read (Invariant #2:
// "raw values never appear in ordinary logs, list APIs, search indexes or
// analytics"). Every identifier this service ever stores is already
// masked/tokenized at write time (RegisterBankAccountRequest only ever
// accepts MaskedAccountNumber, never a raw one) — there is no further
// un-masking to strip. What this response DOES omit, deliberately, versus
// the full GetBankAccountByID read: bank_identifier (routing/institution
// reference), created_by_principal_id, correlation_id and token_version —
// fields that identify who/how the account was set up or rotated, not
// needed by a caller that only wants to know the account exists and is
// usable.
type bankAccountMaskedResponse struct {
	BankAccountID       string `json:"bank_account_id"`
	LegalEntityID       string `json:"legal_entity_id"`
	AccountName         string `json:"account_name"`
	MaskedAccountNumber string `json:"masked_account_number"`
	CurrencyCode        string `json:"currency_code"`
	AccountStatus       string `json:"account_status"`
}

// GetBankAccountMasked handles GET /v1/treasury/accounts/{accountID}/masked
// — a distinct, lower-privilege read path from GetBankAccountByID
// (actionViewAccountMasked, not actionViewPositions), so a caller that
// only needs to confirm an account's identity/status doesn't need the
// broader positions-view permission. GetBankAccountByID's own
// authorization requirement is left unchanged here — tightening it to a
// stricter "sensitive read" permission is a separate policy decision, not
// made by this additive change.
func (h *Handler) GetBankAccountMasked(w http.ResponseWriter, r *http.Request) {
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	accountID := chi.URLParam(r, "accountID")
	acct, ok := h.fetchAccountForAuth(w, r, accountID)
	if !ok {
		return
	}
	if err := h.authz.CheckAllowed(r.Context(), principalID, acct.LegalEntityID, actionViewAccountMasked); err != nil {
		h.writeAuthzErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, bankAccountMaskedResponse{
		BankAccountID: acct.BankAccountID, LegalEntityID: acct.LegalEntityID, AccountName: acct.AccountName,
		MaskedAccountNumber: acct.MaskedAccountNumber, CurrencyCode: acct.CurrencyCode, AccountStatus: acct.AccountStatus,
	})
}

// availableActionsResponse names which of this account's own existing
// transition guards (CanAmendMetadata etc., domain/types.go) currently
// evaluate true for its status — a direct, mechanical read of guards that
// already exist, not new business logic. The doc lists GetAvailableActions
// as a query with no elaborating prose anywhere; this is the only
// non-invented interpretation available.
type availableActionsResponse struct {
	BankAccountID string   `json:"bank_account_id"`
	AccountStatus string   `json:"account_status"`
	Actions       []string `json:"available_actions"`
}

func (h *Handler) GetAvailableActions(w http.ResponseWriter, r *http.Request) {
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
	actions := []string{}
	if domain.CanAmendMetadata(acct.AccountStatus) {
		actions = append(actions, "amend-metadata")
	}
	if domain.CanChangeOperationalUse(acct.AccountStatus) {
		actions = append(actions, "change-operational-use")
	}
	if domain.CanSuspendAccount(acct.AccountStatus) {
		actions = append(actions, "suspend")
	}
	if domain.CanReactivateAccount(acct.AccountStatus) {
		actions = append(actions, "reactivate")
	}
	if domain.CanCloseAccount(acct.AccountStatus) {
		actions = append(actions, "close")
	}
	if domain.CanRotateAccountToken(acct.AccountStatus) {
		actions = append(actions, "rotate-token")
	}
	writeJSON(w, http.StatusOK, availableActionsResponse{BankAccountID: acct.BankAccountID, AccountStatus: acct.AccountStatus, Actions: actions})
}

// ListConnectionOptions handles GET
// /v1/treasury/accounts/{accountID}/connection-options — a thin, fail-
// closed read against banking-connector-svc's real BNK-02 connection
// records for this account. This service never owns connection data; see
// internal/clients/banking.go's own doc comment on why the route lives
// here rather than on banking-connector-svc despite that.
func (h *Handler) ListConnectionOptions(w http.ResponseWriter, r *http.Request) {
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
	if h.banking == nil {
		writeError(w, http.StatusServiceUnavailable, "banking_connector_not_configured", "banking-connector-svc integration is not configured")
		return
	}
	options, err := h.banking.ListConnectionOptions(r.Context(), acct.TenantID, acct.LegalEntityID, accountID, r.Header.Get("X-Correlation-ID"))
	if err != nil {
		h.log.Error("ListConnectionOptions: banking-connector-svc unavailable — failing closed", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "banking_connector_unavailable", err.Error())
		return
	}
	if options == nil {
		options = []domain.ConnectionOption{}
	}
	writeJSON(w, http.StatusOK, options)
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
	h.publisher.PublishBankAccountMetadataAmended(r.Context(), r.Header.Get("X-Correlation-ID"), principalID, *updated)
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
	h.publisher.PublishBankAccountOperationalUseChanged(r.Context(), r.Header.Get("X-Correlation-ID"), principalID, *updated)
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
	h.publisher.PublishBankAccountSuspended(r.Context(), r.Header.Get("X-Correlation-ID"), principalID, *updated)
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
	h.publisher.PublishBankAccountReactivated(r.Context(), r.Header.Get("X-Correlation-ID"), principalID, *updated)
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
	h.publisher.PublishBankAccountClosed(r.Context(), r.Header.Get("X-Correlation-ID"), principalID, *updated)
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
	h.publisher.PublishBankAccountTokenRotated(r.Context(), r.Header.Get("X-Correlation-ID"), principalID, *updated)
	writeJSON(w, http.StatusOK, updated)
}
