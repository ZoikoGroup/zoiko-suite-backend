package handler

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
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
	ListDelegations(ctx context.Context, f domain.ListDelegationsFilter) ([]domain.DelegationGrant, error)
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

	// actionDelegationAdminister is what a caller needs to create a
	// delegation on SOMEONE ELSE'S behalf. DELEGATION_CREATE alone now only
	// lets a principal delegate their own authority.
	//
	// It is a separate action rather than a wider reading of
	// DELEGATION_CREATE because the two are not the same power: one hands
	// away authority you hold, the other moves authority between other
	// people. An administrator needs the second; almost nobody else should.
	actionDelegationAdminister = "DELEGATION_ADMINISTER"
)

// maxBodyBytes caps a request body. Without it an unbounded body is read
// straight into memory before any validation runs.
const maxBodyBytes = 256 << 10

const (
	defaultPageLimit = 100
	maxPageLimit     = 500
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
	if _, ok := h.requireTenant(w, r); !ok {
		return
	}
	var req domain.CreateDelegationRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if req.LegalEntityID == "" || req.DelegatorPrincipalID == "" || req.DelegatePrincipalID == "" || req.ActionType == "" || req.CorrelationID == "" {
		writeError(w, http.StatusBadRequest, "missing_fields", "legal_entity_id, delegator_principal_id, delegate_principal_id, action_type, correlation_id are required")
		return
	}
	if req.DelegatorPrincipalID == req.DelegatePrincipalID {
		writeError(w, http.StatusBadRequest, "delegate_is_delegator", string(domain.ErrDelegateIsDelegator))
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

	// Bind the delegator to the caller.
	//
	// Everything below this point used to run without ever asking who the
	// caller was in relation to the delegator. The delegator's identity came
	// from the request body and was never questioned -- only their authority
	// was verified -- so a principal holding DELEGATION_CREATE could name any
	// colleague as delegator, name themselves as delegate, and walk away with
	// that colleague's authority. The two checks that existed both passed:
	// the caller may create delegations, and the delegator does hold the
	// action. The invariant nobody wrote down is that a principal may only
	// give away authority that is theirs to give.
	if req.DelegatorPrincipalID != principalID {
		// Routing another principal's authority to yourself is the same
		// escalation by a longer route, so it is refused even for an
		// administrator. Administering delegations BETWEEN other people
		// remains allowed; being the beneficiary of one you created does not.
		if req.DelegatePrincipalID == principalID {
			writeError(w, http.StatusForbidden, "self_dealing", string(domain.ErrSelfDealing))
			return
		}
		if err := h.authz.CheckAllowed(r.Context(), principalID, req.LegalEntityID, actionDelegationAdminister); err != nil {
			if errors.Is(err, domain.ErrAuthorizationDenied) {
				writeError(w, http.StatusForbidden, "delegator_mismatch", string(domain.ErrDelegatorMismatch))
				return
			}
			h.writeAuthzErr(w, err)
			return
		}
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

// ListDelegations answers a register read.
//
// Authorization used to run ONLY when the caller supplied a legal_entity_id.
// Omitting it -- the shorter, easier request -- skipped the check entirely and
// returned every delegation in the tenant to a principal holding no grant at
// all: the complete map of who may act for whom, on what, and until when. On
// this register that map is the security model itself.
//
// Every read is now scoped one of two ways. With a legal entity, the caller
// must hold DELEGATION_VIEW on it. Without one, the answer is restricted to
// the delegations the caller is personally party to -- their own inbox, not
// the tenant's -- and asking after somebody else's by principal id is refused
// rather than quietly widened.
func (h *Handler) ListDelegations(w http.ResponseWriter, r *http.Request) {
	if _, ok := h.requireTenant(w, r); !ok {
		return
	}
	f := domain.ListDelegationsFilter{
		LegalEntityID:        r.URL.Query().Get("legal_entity_id"),
		DelegatorPrincipalID: r.URL.Query().Get("delegator_principal_id"),
		DelegatePrincipalID:  r.URL.Query().Get("delegate_principal_id"),
		Status:               r.URL.Query().Get("status"),
	}

	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}

	switch f.Status {
	case "", string(domain.DelegationStatusActive), string(domain.DelegationStatusRevoked), string(domain.DelegationStatusExpired):
	default:
		// A misspelled filter used to return an empty list, which on a
		// register of delegated authority reads as "nobody holds any" -- the
		// most reassuring possible answer, and a false one.
		writeError(w, http.StatusBadRequest, "unknown_status", string(domain.ErrUnknownStatus))
		return
	}

	limit, offset, ok := parsePaging(w, r)
	if !ok {
		return
	}
	f.Limit, f.Offset = limit, offset

	if f.LegalEntityID != "" {
		if err := h.authz.CheckAllowed(r.Context(), principalID, f.LegalEntityID, actionDelegationView); err != nil {
			h.writeAuthzErr(w, err)
			return
		}
	} else {
		// No entity scope: the caller may only see what they are party to.
		// Naming another principal here would be asking after someone else's
		// delegations without holding a grant on any entity, so it is refused
		// instead of silently narrowed to the caller.
		if (f.DelegatorPrincipalID != "" && f.DelegatorPrincipalID != principalID) ||
			(f.DelegatePrincipalID != "" && f.DelegatePrincipalID != principalID) {
			writeError(w, http.StatusForbidden, "forbidden",
				"reading another principal's delegations requires legal_entity_id and DELEGATION_VIEW on it")
			return
		}
		f.SelfPrincipalID = principalID
	}

	h.sweepExpired(r.Context())

	list, err := h.store.ListDelegations(r.Context(), f)
	if err != nil {
		h.writeStoreErr(w, err)
		return
	}
	if list == nil {
		list = []domain.DelegationGrant{}
	}
	writeJSON(w, http.StatusOK, list)
}

// ── GET /v1/delegations/{delegation_id} ───────────────────────────────────────

func (h *Handler) GetDelegation(w http.ResponseWriter, r *http.Request) {
	if _, ok := h.requireTenant(w, r); !ok {
		return
	}
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
	if _, ok := h.requireTenant(w, r); !ok {
		return
	}
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

// requireTenant refuses a request that carries no X-Tenant-Id. Without it a
// forgotten header reached the store and came back as store_unavailable -- a
// 503 that sends whoever is on call to look at Postgres over a missing header.
func (h *Handler) requireTenant(w http.ResponseWriter, r *http.Request) (string, bool) {
	tenantID := svcmiddleware.TenantFromContext(r.Context())
	if tenantID == "" {
		writeError(w, http.StatusUnauthorized, "tenant_missing", string(domain.ErrTenantMissing))
		return "", false
	}
	return tenantID, true
}

// decodeJSON caps the body and refuses unknown fields. A misspelled
// effective_to used to be discarded in silence, leaving the zero time -- so a
// caller who thought they had set an expiry got a delegation rejected for a
// window they believed they had supplied.
func decodeJSON(w http.ResponseWriter, r *http.Request, dst any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, maxBodyBytes)
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_json", err.Error())
		return false
	}
	return true
}

func parsePaging(w http.ResponseWriter, r *http.Request) (limit, offset int, ok bool) {
	limit, offset = defaultPageLimit, 0
	if v := r.URL.Query().Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > maxPageLimit {
			writeError(w, http.StatusBadRequest, "invalid_paging", string(domain.ErrInvalidPaging))
			return 0, 0, false
		}
		limit = n
	}
	if v := r.URL.Query().Get("offset"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 {
			writeError(w, http.StatusBadRequest, "invalid_paging", string(domain.ErrInvalidPaging))
			return 0, 0, false
		}
		offset = n
	}
	return limit, offset, true
}

func (h *Handler) writeStoreErr(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, domain.ErrTenantMissing):
		writeError(w, http.StatusUnauthorized, "tenant_missing", err.Error())
	case errors.Is(err, domain.ErrIdentityMissing):
		writeError(w, http.StatusUnauthorized, "identity_missing", err.Error())
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
