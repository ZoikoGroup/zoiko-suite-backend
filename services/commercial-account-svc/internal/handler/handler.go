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
	"zoiko.io/commercial-account-svc/internal/events"
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
	MembershipCreate        = "MEMBERSHIP_CREATE"
	MembershipDeactivate    = "MEMBERSHIP_DEACTIVATE"
)

// AuthzChecker is the authorization-svc contract this handler depends on.
type AuthzChecker interface {
	CheckAllowed(ctx context.Context, principalID, legalEntityID, actionType string) error
}

type Handler struct {
	store     store.Store
	publisher events.Publisher
	authz     AuthzChecker
	logger    *zap.Logger
}

func New(st store.Store, pub events.Publisher, az AuthzChecker, logger *zap.Logger) *Handler {
	return &Handler{store: st, publisher: pub, authz: az, logger: logger}
}

func (h *Handler) requirePrincipal(w http.ResponseWriter, r *http.Request) (string, bool) {
	principalID := r.Header.Get("X-Principal-Id")
	if principalID == "" {
		writeError(w, http.StatusUnauthorized, "X-Principal-Id header is required")
		return "", false
	}
	return principalID, true
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

func RegisterRoutes(r chi.Router, h *Handler) {
	r.Route("/v1/commercial-accounts", func(r chi.Router) {
		r.Post("/", h.CreateCommercialAccount)
		r.Get("/{id}", h.GetCommercialAccount)
	})
	r.Route("/v1/memberships", func(r chi.Router) {
		r.Post("/", h.CreateMembership)
		r.Get("/{id}", h.GetMembership)
		r.Delete("/{id}", h.DeactivateMembership)
	})
	r.Get("/v1/organizations/{organizationID}/memberships", h.ListMemberships)
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

	_ = h.publisher.Publish(r.Context(), events.PublishParams{
		EventType: "commercial_account.created", EntityID: acct.CommercialAccountID, TenantID: acct.OrganizationID,
		ActorID: principalID, CorrelationID: r.Header.Get("X-Correlation-ID"), Payload: acct,
	})
	writeJSON(w, http.StatusCreated, acct)
}

func (h *Handler) GetCommercialAccount(w http.ResponseWriter, r *http.Request) {
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
	writeJSON(w, http.StatusOK, acct)
}

func (h *Handler) CreateMembership(w http.ResponseWriter, r *http.Request) {
	var req domain.CreateMembershipRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.PrincipalID == "" || req.OrganizationID == "" {
		writeError(w, http.StatusBadRequest, "principal_id and organization_id are required")
		return
	}

	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	if !h.authorize(w, r, principalID, req.OrganizationID, MembershipCreate) {
		return
	}

	m := &domain.Membership{
		MembershipID:         uuid.NewString(),
		PrincipalID:          req.PrincipalID,
		OrganizationID:       req.OrganizationID,
		Status:               domain.MembershipStatusActive,
		EffectiveFrom:        time.Now().UTC(),
		CreatedAt:            time.Now().UTC(),
		CreatedByPrincipalID: principalID,
	}
	if req.WorkspaceID != "" {
		m.WorkspaceID = &req.WorkspaceID
	}
	if req.LegalEntityID != "" {
		m.LegalEntityID = &req.LegalEntityID
	}

	if err := h.store.CreateMembership(r.Context(), m); err != nil {
		h.logger.Error("create membership failed", zap.Error(err))
		writeError(w, http.StatusInternalServerError, "failed to create membership")
		return
	}

	_ = h.publisher.Publish(r.Context(), events.PublishParams{
		EventType: "membership.created", EntityID: m.MembershipID, TenantID: m.OrganizationID,
		ActorID: principalID, CorrelationID: r.Header.Get("X-Correlation-ID"), Payload: m,
	})
	writeJSON(w, http.StatusCreated, m)
}

func (h *Handler) GetMembership(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	m, err := h.store.GetMembership(r.Context(), id)
	if err != nil {
		if errors.Is(err, domain.ErrMembershipNotFound) {
			writeError(w, http.StatusNotFound, "membership not found")
			return
		}
		writeError(w, http.StatusInternalServerError, "failed to get membership")
		return
	}
	writeJSON(w, http.StatusOK, m)
}

func (h *Handler) ListMemberships(w http.ResponseWriter, r *http.Request) {
	organizationID := chi.URLParam(r, "organizationID")
	memberships, err := h.store.ListMembershipsByOrganization(r.Context(), organizationID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to list memberships")
		return
	}
	if memberships == nil {
		memberships = []domain.Membership{}
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"memberships": memberships, "total": len(memberships)})
}

// DeactivateMembership ends a membership per doc7 §A6: access ends, but the
// row is never deleted — historical attribution (who was a member, when)
// must survive removal.
func (h *Handler) DeactivateMembership(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")

	existing, err := h.store.GetMembership(r.Context(), id)
	if err != nil {
		if errors.Is(err, domain.ErrMembershipNotFound) {
			writeError(w, http.StatusNotFound, "membership not found")
			return
		}
		writeError(w, http.StatusInternalServerError, "failed to fetch membership")
		return
	}

	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	if !h.authorize(w, r, principalID, existing.OrganizationID, MembershipDeactivate) {
		return
	}

	if err := h.store.DeactivateMembership(r.Context(), id, existing.OrganizationID); err != nil {
		if errors.Is(err, domain.ErrMembershipNotFound) {
			writeError(w, http.StatusConflict, "membership already deactivated or not found")
			return
		}
		writeError(w, http.StatusInternalServerError, "failed to deactivate membership")
		return
	}

	_ = h.publisher.Publish(r.Context(), events.PublishParams{
		EventType: "membership.deactivated", EntityID: id, TenantID: existing.OrganizationID,
		ActorID: principalID, CorrelationID: r.Header.Get("X-Correlation-ID"), Payload: nil,
	})
	writeJSON(w, http.StatusOK, map[string]string{"status": "deactivated"})
}

func writeJSON(w http.ResponseWriter, status int, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}
