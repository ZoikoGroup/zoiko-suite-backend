package handler

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"go.uber.org/zap"

	authzpkg "zoiko.io/commercial-account-svc/internal/authz"
	"zoiko.io/commercial-account-svc/internal/domain"
	svcmiddleware "zoiko.io/commercial-account-svc/internal/middleware"
	"zoiko.io/commercial-account-svc/internal/store"
)

// platformScopeID is the legal_entity_id passed to authorization-svc for
// actions that aren't scoped to one commercial account (catalog/plan
// administration) — authorization-svc rejects an empty legal_entity_id, and
// this is the same platform-wide scope ID convention used elsewhere in this
// codebase (e.g. policy-svc, configuration-feature-flag-svc's
// AUTHZ_PLATFORM_SCOPE_ID).
const platformScopeID = "00000000-0000-0000-0000-00000000f001"

// Action constants passed to authorization-svc as action_type.
const (
	CommercialAccountCreate = "COMMERCIAL_ACCOUNT_CREATE"
	CommercialAccountRead   = "COMMERCIAL_ACCOUNT_READ"
)

// AuthzChecker is the authorization-svc contract this handler depends on.
type AuthzChecker interface {
	CheckAllowed(ctx context.Context, principalID, legalEntityID, actionType string) error
}

// Handler has no event publisher: every write's event is enqueued by the
// store in the write's own transaction (the shared transactional outbox), and
// nothing publishes from a handler.
type Handler struct {
	store  store.Store
	authz  AuthzChecker
	logger *zap.Logger
}

func New(st store.Store, az AuthzChecker, logger *zap.Logger) *Handler {
	return &Handler{store: st, authz: az, logger: logger}
}

func (h *Handler) requirePrincipal(w http.ResponseWriter, r *http.Request) (string, bool) {
	principalID := r.Header.Get("X-Principal-Id")
	if principalID == "" {
		writeError(w, http.StatusUnauthorized, "X-Principal-Id header is required")
		return "", false
	}
	return principalID, true
}

// requireTenant returns the gateway-verified organization scope, and
// refuses the request when there is none.
//
// It is not applied as blanket middleware because this service serves
// genuinely tenant-less routes: the price catalog, plans and entitlement
// limits are platform administration (doc7 §U1 — an approved catalog is
// never edited in place, and every tenant reads the same one), and those
// handlers authorize at platformScopeID rather than at an organization. A
// blanket 401 would break them.
//
// declared is an organization the caller named itself — a URL path segment
// or a body field. Where one is present it may only AGREE with the
// verified header; disagreement is refused rather than silently resolved,
// so a caller asking about another organization gets an error instead of a
// quietly reinterpreted answer.
func (h *Handler) requireTenant(w http.ResponseWriter, r *http.Request, declared string) (string, bool) {
	tenantID := svcmiddleware.TenantFromContext(r.Context())
	if tenantID == "" {
		writeError(w, http.StatusUnauthorized,
			"X-Tenant-Id is required — the gateway sets it from a verified identity envelope")
		return "", false
	}
	if declared != "" && declared != tenantID {
		writeError(w, http.StatusForbidden,
			"organization in the request does not match the verified X-Tenant-Id")
		return "", false
	}
	return tenantID, true
}

// authorize checks the calling principal against authorization-svc for the
// given action, scoped to organizationID (authorization-svc's "legal_entity_id"
// scope parameter — this service passes its organization_id there, same
// AUTHZ_PLATFORM_SCOPE_ID-style convention used elsewhere in this platform).
func (h *Handler) authorize(w http.ResponseWriter, r *http.Request, principalID, organizationID, actionType string) bool {
	if err := h.authz.CheckAllowed(r.Context(), principalID, organizationID, actionType); err != nil {
		if errors.Is(err, authzpkg.ErrAuthorizationDenied) {
			writeError(w, http.StatusForbidden, "not authorized to perform this action")
		} else {
			writeError(w, http.StatusServiceUnavailable, "authorization service unavailable")
		}
		return false
	}
	return true
}

// Retired (SPEC_DEVIATIONS.md A3, ZS-SVC-Q-001): /v1/memberships and its
// CreateMembership/GetMembership/ListMemberships/DeactivateMembership HTTP
// handlers are gone. Nothing in the canonical COM-01-05 surface reads or
// writes the memberships table, and no real caller (this repo, the frontend,
// or another service) was found referencing these paths — see the A3
// closure note for the verification. The memberships table and its RLS
// policy (migration 000005) are left in place; store.Store still declares
// CreateMembership/GetMembership/ListMemberships/DeactivateMembership and
// PgStore still implements them, because internal/store/rls_test.go uses
// them to exercise that policy against a real Postgres role. They are simply
// no longer reachable over HTTP.
func RegisterRoutes(r chi.Router, h *Handler) {
	r.Route("/v1/commercial-accounts", func(r chi.Router) {
		r.Post("/", h.CreateCommercialAccount)
		r.Get("/{id}", h.GetCommercialAccount)
	})
}

func (h *Handler) CreateCommercialAccount(w http.ResponseWriter, r *http.Request) {
	var req domain.CreateCommercialAccountRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.OrganizationID == "" || req.LegalCustomerName == "" || req.BillingCurrencyCode == "" {
		writeError(w, http.StatusBadRequest, "organization_id, legal_customer_name, and billing_currency_code are required")
		return
	}

	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	if !h.authorize(w, r, principalID, req.OrganizationID, CommercialAccountCreate) {
		return
	}

	acct := &domain.CommercialAccount{
		CommercialAccountID:  uuid.NewString(),
		OrganizationID:       req.OrganizationID,
		LegalCustomerName:    req.LegalCustomerName,
		BillingCurrencyCode:  req.BillingCurrencyCode,
		ContactEmail:         req.ContactEmail,
		Status:               domain.CommercialAccountStatusActive,
		CreatedAt:            time.Now().UTC(),
		CreatedByPrincipalID: principalID,
	}
	if req.ContractReference != "" {
		acct.ContractReference = &req.ContractReference
	}
	if req.ProcessorCustomerRef != "" {
		acct.ProcessorCustomerRef = &req.ProcessorCustomerRef
	}

	if err := h.store.CreateCommercialAccount(r.Context(), acct); err != nil {
		if errors.Is(err, domain.ErrConflict) {
			writeError(w, http.StatusConflict, "organization already has a commercial account")
			return
		}
		h.logger.Error("create commercial account failed", zap.Error(err))
		writeError(w, http.StatusInternalServerError, "failed to create commercial account")
		return
	}

	writeJSON(w, http.StatusCreated, acct)
}

// GetCommercialAccount reads one account, scoped to the verified tenant.
//
// Authz is now gated on COMMERCIAL_ACCOUNT_READ. The account is fetched
// first (store is tenant-scoped so a foreign tenant's account is already
// indistinguishable from 404), then the check is made against the account's
// OWN organization_id — never against a caller-supplied scope — to avoid
// the namespace-mismatch risk documented in tracker row 84a.
func (h *Handler) GetCommercialAccount(w http.ResponseWriter, r *http.Request) {
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	if _, ok := h.requireTenant(w, r, ""); !ok {
		return
	}
	id := chi.URLParam(r, "id")
	acct, err := h.store.GetCommercialAccount(r.Context(), id)
	if err != nil {
		if errors.Is(err, domain.ErrCommercialAccountNotFound) {
			writeError(w, http.StatusNotFound, "commercial account not found")
			return
		}
		writeError(w, http.StatusInternalServerError, "failed to get commercial account")
		return
	}
	if !h.authorize(w, r, principalID, acct.OrganizationID, CommercialAccountRead) {
		return
	}
	writeJSON(w, http.StatusOK, acct)
}

func writeJSON(w http.ResponseWriter, status int, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

// HandleIdempotentReplay handles the common pattern of catching an
// IdempotentReplayError and returning the original resource. The fetch
// function should re-fetch the resource by replay.ResourceID.
func HandleIdempotentReplay(w http.ResponseWriter, r *http.Request, err error, fetch func(resourceID string) (any, error)) bool {
	var replay *domain.IdempotentReplayError
	if !errors.As(err, &replay) {
		return false
	}
	resource, gerr := fetch(replay.ResourceID)
	if gerr != nil {
		// If we can't fetch the original, fall through to normal error handling
		return false
	}
	w.Header().Set("Idempotent-Replayed", "true")
	writeJSON(w, http.StatusOK, resource)
	return true
}
