package handler

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"go.uber.org/zap"

	"zoiko.io/general-ledger-svc/internal/domain"
)

// scopedAccountStore is the part of the store that keeps entity- and book-scoped account versions (ACC-01 A1). A store without it keeps
// every account at tenant level, which is what every account was before scoped versions existed.
type scopedAccountStore interface {
	CreateAccountScopeVersion(ctx context.Context, tenantID, accountCode string, req domain.CreateAccountScopeVersionRequest, effective time.Time, principalID string) (*domain.AccountScopeVersion, error)
	ListAccountScopeVersions(ctx context.Context, tenantID, accountCode, entityID string) ([]domain.AccountScopeVersion, error)
	GetScopedAccountAsOf(ctx context.Context, tenantID, accountCode, entityID, bookID string, asOf time.Time) (*domain.ScopedAccount, error)
	ScopedAccountStatus(ctx context.Context, tenantID, code, entityID, bookRef string, on time.Time) (status string, overridden bool, err error)
}

func (h *Handler) scopedAccounts() (scopedAccountStore, bool) {
	s, ok := h.store.(scopedAccountStore)
	return s, ok
}

// CreateAccountScopeVersion records a version of an account for one legal entity, or one accounting book of it: its name, its place in
// the hierarchy and whether it is open for use there. The tenant version, and every other entity and book, are unchanged.
func (h *Handler) CreateAccountScopeVersion(w http.ResponseWriter, r *http.Request) {
	var req domain.CreateAccountScopeVersionRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	code := chi.URLParam(r, "account_code")
	if _, err := uuid.Parse(req.LegalEntityID); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_field", "legal_entity_id is a legal entity id")
		return
	}
	if req.AccountingBookID != "" {
		if _, err := uuid.Parse(req.AccountingBookID); err != nil {
			writeError(w, http.StatusBadRequest, "invalid_field", "accounting_book_id is an accounting book id")
			return
		}
	}
	if strings.TrimSpace(req.Reason) == "" {
		writeError(w, http.StatusBadRequest, "missing_field", "reason is required")
		return
	}
	if req.AccountName == nil && req.ParentAccountCode == nil && req.Status == nil {
		writeError(w, http.StatusBadRequest, "missing_field", "give at least one of account_name, parent_account_code and status")
		return
	}
	if req.Status != nil && *req.Status != "ACTIVE" && *req.Status != "INACTIVE" {
		writeError(w, http.StatusBadRequest, "invalid_field", "status is ACTIVE or INACTIVE")
		return
	}
	if req.AccountName != nil && strings.TrimSpace(*req.AccountName) == "" {
		writeError(w, http.StatusBadRequest, "invalid_field", "account_name cannot be empty")
		return
	}
	effective := time.Now().UTC().Truncate(24 * time.Hour)
	if req.EffectiveFrom != "" {
		d, err := time.Parse("2006-01-02", req.EffectiveFrom)
		if err != nil {
			writeError(w, http.StatusBadRequest, "invalid_field", "effective_from is a date, YYYY-MM-DD")
			return
		}
		effective = d
	}
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	tenantID, ok := h.requireTenant(w, r)
	if !ok {
		return
	}
	if err := h.authz.CheckAllowed(r.Context(), principalID, req.LegalEntityID, actionAmendAccount); err != nil {
		h.writeAuthzErr(w, err)
		return
	}
	st, ok := h.scopedAccounts()
	if !ok {
		writeError(w, http.StatusNotImplemented, "scoped_accounts_not_configured", "")
		return
	}
	v, err := st.CreateAccountScopeVersion(r.Context(), tenantID, code, req, effective, principalID)
	if err != nil {
		if errors.Is(err, domain.ErrAccountScopeBookMismatch) {
			writeError(w, http.StatusUnprocessableEntity, "book_entity_mismatch", err.Error())
			return
		}
		if h.writeAccountVersionErr(w, err) {
			return
		}
		h.log.Error("CreateAccountScopeVersion: store unavailable", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store_unavailable", "")
		return
	}
	writeJSON(w, http.StatusCreated, v)
}

// ListAccountScopeVersions lists an account scoped versions for one legal entity.
func (h *Handler) ListAccountScopeVersions(w http.ResponseWriter, r *http.Request) {
	entity := r.URL.Query().Get("legal_entity_id")
	if _, err := uuid.Parse(entity); err != nil {
		writeError(w, http.StatusBadRequest, "missing_field", "legal_entity_id is required")
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
	if err := h.authz.CheckAllowed(r.Context(), principalID, entity, actionViewAccount); err != nil {
		h.writeAuthzErr(w, err)
		return
	}
	st, ok := h.scopedAccounts()
	if !ok {
		writeError(w, http.StatusNotImplemented, "scoped_accounts_not_configured", "")
		return
	}
	list, err := st.ListAccountScopeVersions(r.Context(), tenantID, chi.URLParam(r, "account_code"), entity)
	if err != nil {
		h.log.Error("ListAccountScopeVersions: store unavailable", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store_unavailable", "")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"versions": list, "total": len(list)})
}

// scopedAccountStatus is the status the posting paths use for an account of an entity: the tenant status unless a scoped version of the
// entity or of the book sets another. ok is false when the store keeps no scoped versions.
func (h *Handler) scopedAccountStatus(ctx context.Context, tenantID, code, entityID, bookRef string, on time.Time) (status string, overridden bool, err error) {
	st, ok := h.scopedAccounts()
	if !ok || entityID == "" {
		return "", false, nil
	}
	return st.ScopedAccountStatus(ctx, tenantID, code, entityID, bookRef, on)
}
