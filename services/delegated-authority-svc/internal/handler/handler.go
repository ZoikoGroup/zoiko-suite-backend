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

	"zoiko.io/delegated-authority-svc/internal/domain"
	svcmiddleware "zoiko.io/delegated-authority-svc/internal/middleware"
)

type Store interface {
	CreateDelegation(ctx context.Context, d *domain.DelegationGrant) (created bool, err error)
	ExpireDue(ctx context.Context) ([]domain.DelegationGrant, error)
	GetDelegation(ctx context.Context, delegationID string) (*domain.DelegationGrant, error)
	ListDelegations(ctx context.Context, legalEntityID, delegatorPrincipalID, delegatePrincipalID, status string) ([]domain.DelegationGrant, error)
	RevokeDelegation(ctx context.Context, delegationID, revokedByPrincipalID string) (*domain.DelegationGrant, error)
}

type Publisher interface {
	PublishDelegated(ctx context.Context, d domain.DelegationGrant)
	PublishRevoked(ctx context.Context, d domain.DelegationGrant)
	PublishExpired(ctx context.Context, d domain.DelegationGrant)
}

// AuthZClient is used twice, for two different purposes: (1) the normal
// gate on whether the caller may manage delegations at all, and (2) the
// platform's core delegation invariant — whether the delegator actually
// holds the authority being delegated.
type AuthZClient interface {
	CheckAllowed(ctx context.Context, principalID, legalEntityID, actionType string) error
}

const (
	actionDelegationCreate = "DELEGATION_CREATE"
	actionDelegationView   = "DELEGATION_VIEW"
	actionDelegationRevoke = "DELEGATION_REVOKE"
)

type Handler struct {
	store     Store
	publisher Publisher
	authz     AuthZClient
	log       *zap.Logger
}

func New(store Store, publisher Publisher, authz AuthZClient, log *zap.Logger) *Handler {
	return &Handler{store: store, publisher: publisher, authz: authz, log: log}
}

func RegisterRoutes(r chi.Router, h *Handler) {
	r.Route("/v1/delegations", func(r chi.Router) {
		r.Post("/", h.CreateDelegation)
		r.Get("/", h.ListDelegations)
		r.Get("/{delegation_id}", h.GetDelegation)
		r.Post("/{delegation_id}/revoke", h.RevokeDelegation)
	})
}

// ── POST /v1/delegations ──────────────────────────────────────────────────────

// CreateDelegation enforces the platform's core delegation invariant:
// delegated authority must never exceed the delegator's own authority. That
// is checked via a real synchronous call to authorization-svc, evaluating
// whether the DELEGATOR (not the caller) holds a GRANTED decision for the
// exact action_type being delegated on the target legal entity — never
// trusted from the request body.
//
// Idempotent on (tenant_id, correlation_id).
func (h *Handler) CreateDelegation(w http.ResponseWriter, r *http.Request) {
	var req domain.CreateDelegationRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_json", err.Error())
		return
	}
	if req.LegalEntityID == "" || req.DelegatorPrincipalID == "" || req.DelegatePrincipalID == "" || req.ActionType == "" || req.CorrelationID == "" {
		writeError(w, http.StatusBadRequest, "missing_fields", "legal_entity_id, delegator_principal_id, delegate_principal_id, action_type, correlation_id are required")
		return
	}
	if !req.EffectiveTo.After(req.EffectiveFrom) {
		writeError(w, http.StatusBadRequest, "invalid_time_window", string(domain.ErrInvalidTimeWindow))
		return
	}

	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	if err := h.authz.CheckAllowed(r.Context(), principalID, req.LegalEntityID, actionDelegationCreate); err != nil {
		h.writeAuthzErr(w, err)
		return
	}

	// The core invariant: the delegator must actually hold what they're
	// trying to delegate.
	if err := h.authz.CheckAllowed(r.Context(), req.DelegatorPrincipalID, req.LegalEntityID, req.ActionType); err != nil {
		if errors.Is(err, domain.ErrAuthorizationDenied) {
			writeError(w, http.StatusForbidden, "delegator_lacks_authority", string(domain.ErrDelegatorLacksAuthority))
			return
		}
		writeError(w, http.StatusServiceUnavailable, "authz_unavailable", err.Error())
		return
	}

	now := time.Now().UTC()
	d := &domain.DelegationGrant{
		DelegationID:         uuid.NewString(),
		TenantID:             svcmiddleware.TenantFromContext(r.Context()),
		LegalEntityID:        req.LegalEntityID,
		DelegatorPrincipalID: req.DelegatorPrincipalID,
		DelegatePrincipalID:  req.DelegatePrincipalID,
		ActionType:           req.ActionType,
		EffectiveFrom:        req.EffectiveFrom,
		EffectiveTo:          req.EffectiveTo,
		Status:               domain.DelegationStatusActive,
		CreatedByPrincipalID: principalID,
		CorrelationID:        req.CorrelationID,
		CreatedAt:            now,
		UpdatedAt:            now,
	}

	created, err := h.store.CreateDelegation(r.Context(), d)
	if err != nil {
		h.log.Error("failed to create delegation", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store_unavailable", err.Error())
		return
	}
	if created {
		h.publisher.PublishDelegated(r.Context(), *d)
	}

	writeJSON(w, http.StatusCreated, d)
}

// ── GET /v1/delegations ────────────────────────────────────────────────────────

func (h *Handler) ListDelegations(w http.ResponseWriter, r *http.Request) {
	legalEntityID := r.URL.Query().Get("legal_entity_id")
	delegatorPrincipalID := r.URL.Query().Get("delegator_principal_id")
	delegatePrincipalID := r.URL.Query().Get("delegate_principal_id")
	status := r.URL.Query().Get("status")

	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	if legalEntityID != "" {
		if err := h.authz.CheckAllowed(r.Context(), principalID, legalEntityID, actionDelegationView); err != nil {
			h.writeAuthzErr(w, err)
			return
		}
	}

	h.sweepExpired(r.Context())

	list, err := h.store.ListDelegations(r.Context(), legalEntityID, delegatorPrincipalID, delegatePrincipalID, status)
	if err != nil {
		h.log.Error("failed to list delegations", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store_unavailable", err.Error())
		return
	}
	if list == nil {
		list = []domain.DelegationGrant{}
	}
	writeJSON(w, http.StatusOK, list)
}

// ── GET /v1/delegations/{delegation_id} ───────────────────────────────────────

func (h *Handler) GetDelegation(w http.ResponseWriter, r *http.Request) {
	delegationID := chi.URLParam(r, "delegation_id")

	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}

	h.sweepExpired(r.Context())

	d, err := h.store.GetDelegation(r.Context(), delegationID)
	if err != nil {
		h.writeStoreErr(w, err)
		return
	}
	if err := h.authz.CheckAllowed(r.Context(), principalID, d.LegalEntityID, actionDelegationView); err != nil {
		h.writeAuthzErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, d)
}

// ── POST /v1/delegations/{delegation_id}/revoke ───────────────────────────────

func (h *Handler) RevokeDelegation(w http.ResponseWriter, r *http.Request) {
	delegationID := chi.URLParam(r, "delegation_id")

	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	d, err := h.store.GetDelegation(r.Context(), delegationID)
	if err != nil {
		h.writeStoreErr(w, err)
		return
	}
	if err := h.authz.CheckAllowed(r.Context(), principalID, d.LegalEntityID, actionDelegationRevoke); err != nil {
		h.writeAuthzErr(w, err)
		return
	}

	updated, err := h.store.RevokeDelegation(r.Context(), delegationID, principalID)
	if err != nil {
		h.writeStoreErr(w, err)
		return
	}

	h.publisher.PublishRevoked(r.Context(), *updated)
	writeJSON(w, http.StatusOK, updated)
}

// ── Helpers ────────────────────────────────────────────────────────────────

// sweepExpired lazily flips any due delegations to EXPIRED and publishes
// authority.expired for each one that flipped. Errors are logged, not
// surfaced — a failed sweep must not block the read it's piggybacking on.
func (h *Handler) sweepExpired(ctx context.Context) {
	expired, err := h.store.ExpireDue(ctx)
	if err != nil {
		h.log.Error("failed to sweep expired delegations", zap.Error(err))
		return
	}
	for _, d := range expired {
		h.publisher.PublishExpired(ctx, d)
	}
}

func (h *Handler) requirePrincipal(w http.ResponseWriter, r *http.Request) (string, bool) {
	principalID := r.Header.Get("X-Principal-Id")
	if principalID == "" {
		writeError(w, http.StatusUnauthorized, "identity_missing", string(domain.ErrIdentityMissing))
		return "", false
	}
	return principalID, true
}

func (h *Handler) writeAuthzErr(w http.ResponseWriter, err error) {
	if errors.Is(err, domain.ErrAuthorizationDenied) {
		writeError(w, http.StatusForbidden, "forbidden", err.Error())
	} else {
		writeError(w, http.StatusServiceUnavailable, "authz_unavailable", err.Error())
	}
}

func (h *Handler) writeStoreErr(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, domain.ErrDelegationNotFound):
		writeError(w, http.StatusNotFound, "not_found", err.Error())
	case errors.Is(err, domain.ErrInvalidTransition):
		writeError(w, http.StatusConflict, "invalid_transition", err.Error())
	default:
		h.log.Error("delegated authority store error", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store_unavailable", err.Error())
	}
}

func writeError(w http.ResponseWriter, status int, code, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{
		"error_code":    code,
		"error_message": msg,
	})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
