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
	"zoiko.io/delegated-authority-svc/internal/telemetry"
)

type Store interface {
	CreateDelegation(ctx context.Context, d *domain.DelegationGrant) (created bool, err error)
	ExpireDue(ctx context.Context) ([]domain.DelegationGrant, error)
	GetDelegation(ctx context.Context, delegationID string) (*domain.DelegationGrant, error)
	ListDelegations(ctx context.Context, f domain.ListDelegationsFilter) ([]domain.DelegationGrant, error)
	RevokeDelegation(ctx context.Context, delegationID, revokedByPrincipalID string) (*domain.DelegationGrant, error)
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
	store   Store
	authz   AuthZClient
	log     *zap.Logger
	metrics *telemetry.Domain
}

// New builds the handler.
//
// There is no publisher argument any more, and its absence is the point: events
// are written by the store inside the same transaction as the state change they
// describe, then delivered by internal/outbox. A handler that published after
// the commit could — and on this service did — report a successful revocation
// whose notice was silently dropped.
//
// metrics may be nil in tests; every call site goes through the count* helpers
// below, which tolerate it. The counters describe DECISIONS rather than status
// codes, because almost everything interesting this service does is a
// well-formed 403 that no error-rate alert will ever see.
func New(store Store, authz AuthZClient, log *zap.Logger, metrics *telemetry.Domain) *Handler {
	return &Handler{store: store, authz: authz, log: log, metrics: metrics}
}

func (h *Handler) countGrant(outcome string) {
	if h.metrics != nil {
		h.metrics.Grants.WithLabelValues(outcome).Inc()
	}
}

func (h *Handler) countRevoke(outcome string) {
	if h.metrics != nil {
		h.metrics.Revocations.WithLabelValues(outcome).Inc()
	}
}

func (h *Handler) countRead(scope string) {
	if h.metrics != nil {
		h.metrics.RegisterReads.WithLabelValues(scope).Inc()
	}
}

// countAuthz records one authorization-svc decision.
//
// "unavailable" is a third outcome rather than a flavour of "denied" because
// the two demand opposite responses: a denial is a permissions question for a
// human, an unavailable dependency is an outage. Folded together — which is how
// a plain 403-vs-503 count leaves them — an authorization-svc outage looks like
// a sudden surge of people attempting things they are not allowed to do.
func (h *Handler) countAuthz(action string, err error) {
	if h.metrics == nil {
		return
	}
	switch {
	case err == nil:
		h.metrics.AuthZDecisions.WithLabelValues(action, telemetry.AuthZGranted).Inc()
	case errors.Is(err, domain.ErrAuthorizationDenied):
		h.metrics.AuthZDecisions.WithLabelValues(action, telemetry.AuthZDenied).Inc()
	default:
		h.metrics.AuthZDecisions.WithLabelValues(action, telemetry.AuthZUnavailable).Inc()
	}
}

// checkAllowed is CheckAllowed plus the counter, so no call site can add an
// authorization check and forget to make it visible.
func (h *Handler) checkAllowed(ctx context.Context, principalID, legalEntityID, actionType string) error {
	err := h.authz.CheckAllowed(ctx, principalID, legalEntityID, actionType)
	h.countAuthz(actionType, err)
	return err
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
		h.countGrant(telemetry.GrantTenantMissing)
		return
	}
	var req domain.CreateDelegationRequest
	if !decodeJSON(w, r, &req) {
		h.countGrant(telemetry.GrantInvalidRequest)
		return
	}
	if req.LegalEntityID == "" || req.DelegatorPrincipalID == "" || req.DelegatePrincipalID == "" || req.ActionType == "" || req.CorrelationID == "" {
		h.countGrant(telemetry.GrantInvalidRequest)
		writeError(w, http.StatusBadRequest, "missing_fields", "legal_entity_id, delegator_principal_id, delegate_principal_id, action_type, correlation_id are required")
		return
	}
	if req.DelegatorPrincipalID == req.DelegatePrincipalID {
		h.countGrant(telemetry.GrantDelegateIsDelegator)
		writeError(w, http.StatusBadRequest, "delegate_is_delegator", string(domain.ErrDelegateIsDelegator))
		return
	}
	if !req.EffectiveTo.After(req.EffectiveFrom) {
		h.countGrant(telemetry.GrantInvalidWindow)
		writeError(w, http.StatusBadRequest, "invalid_time_window", string(domain.ErrInvalidTimeWindow))
		return
	}

	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		h.countGrant(telemetry.GrantIdentityMissing)
		return
	}
	if err := h.checkAllowed(r.Context(), principalID, req.LegalEntityID, actionDelegationCreate); err != nil {
		if errors.Is(err, domain.ErrAuthorizationDenied) {
			h.countGrant(telemetry.GrantNoCreateGrant)
		} else {
			h.countGrant(telemetry.GrantAuthzUnavailable)
		}
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
			// Counted separately from delegator_mismatch on purpose. A mismatch
			// is usually somebody without the administer grant doing their job;
			// this one is an attempt to route another principal's authority to
			// the caller, and it is worth being able to alert on its rate
			// alone. Both answer 403, so nothing downstream of the status code
			// can tell them apart.
			h.countGrant(telemetry.GrantSelfDealing)
			writeError(w, http.StatusForbidden, "self_dealing", string(domain.ErrSelfDealing))
			return
		}
		if err := h.checkAllowed(r.Context(), principalID, req.LegalEntityID, actionDelegationAdminister); err != nil {
			if errors.Is(err, domain.ErrAuthorizationDenied) {
				h.countGrant(telemetry.GrantDelegatorMismatch)
				writeError(w, http.StatusForbidden, "delegator_mismatch", string(domain.ErrDelegatorMismatch))
				return
			}
			h.countGrant(telemetry.GrantAuthzUnavailable)
			h.writeAuthzErr(w, err)
			return
		}
	}

	// The core invariant: the delegator must actually hold what they're
	// trying to delegate.
	// Counted under a fixed label rather than under req.ActionType: the action
	// being delegated is caller-supplied, so using it as a metric label lets an
	// unauthenticated caller create unbounded series in Prometheus by varying
	// one JSON field. The action is in the access decision log, which is where
	// an auditor looks for it; the counter only needs to say that the core
	// invariant was exercised.
	if err := h.authz.CheckAllowed(r.Context(), req.DelegatorPrincipalID, req.LegalEntityID, req.ActionType); err != nil {
		h.countAuthz("DELEGATED_ACTION", err)
		if errors.Is(err, domain.ErrAuthorizationDenied) {
			h.countGrant(telemetry.GrantDelegatorLacksAuth)
			writeError(w, http.StatusForbidden, "delegator_lacks_authority", string(domain.ErrDelegatorLacksAuthority))
			return
		}
		h.countGrant(telemetry.GrantAuthzUnavailable)
		writeError(w, http.StatusServiceUnavailable, "authz_unavailable", err.Error())
		return
	}
	h.countAuthz("DELEGATED_ACTION", nil)

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
		h.countGrant(telemetry.GrantStoreUnavailable)
		h.log.Error("failed to create delegation", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store_unavailable", err.Error())
		return
	}

	// 201 for a grant that was written, 200 for a replay of one that already
	// was. This used to answer 201 unconditionally, and the store's idempotency
	// made that a lie the caller could not detect: a resubmitted form got
	// "Created" and the ORIGINAL grant's body, so the console reported a fresh
	// delegation of authority every time an operator double-clicked. Its own
	// client code had a branch for the 200 that no response ever took.
	//
	// The distinction is not cosmetic on this register. "You have just granted
	// someone your authority" and "this was granted days ago and nothing
	// changed" are different facts, and only one of them warrants a second look.
	if !created {
		h.countGrant(telemetry.GrantReplayed)
		writeJSON(w, http.StatusOK, d)
		return
	}
	h.countGrant(telemetry.GrantCreated)
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
		if err := h.checkAllowed(r.Context(), principalID, f.LegalEntityID, actionDelegationView); err != nil {
			h.writeAuthzErr(w, err)
			return
		}
		h.countRead(telemetry.ReadScopeEntity)
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
		h.countRead(telemetry.ReadScopeSelf)
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
	if err := h.checkAllowed(r.Context(), principalID, d.LegalEntityID, actionDelegationView); err != nil {
		h.writeAuthzErr(w, err)
		return
	}
	h.countRead(telemetry.ReadScopeEntity)
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
		h.countRevoke(telemetry.RevokeUnavailable)
		return
	}

	// Sweep BEFORE reading the row, exactly as the two read paths do.
	//
	// Expiry here is lazy: a grant past its window keeps status ACTIVE in the
	// table until some read observes the lapse. Revoke was the one path that
	// skipped the sweep, so revoking a delegation that had already run out
	// wrote REVOKED, revoked_at and revoked_by over a grant that no longer
	// conferred anything — the register then said a named principal withdrew an
	// authority at a time when nobody withdrew anything, and published
	// authority.revoked instead of authority.expired.
	//
	// On a register whose only job is recording who did what, attributing an
	// act to someone who did not perform it is the specific failure to avoid.
	// After the sweep the row reads EXPIRED and the revoke correctly answers
	// 409: already terminal, nothing to withdraw.
	h.sweepExpired(r.Context())

	d, err := h.store.GetDelegation(r.Context(), delegationID)
	if err != nil {
		h.countRevokeStoreErr(err)
		h.writeStoreErr(w, err)
		return
	}
	if err := h.checkAllowed(r.Context(), principalID, d.LegalEntityID, actionDelegationRevoke); err != nil {
		if errors.Is(err, domain.ErrAuthorizationDenied) {
			h.countRevoke(telemetry.RevokeForbidden)
		} else {
			h.countRevoke(telemetry.RevokeUnavailable)
		}
		h.writeAuthzErr(w, err)
		return
	}

	updated, err := h.store.RevokeDelegation(r.Context(), delegationID, principalID)
	if err != nil {
		h.countRevokeStoreErr(err)
		h.writeStoreErr(w, err)
		return
	}

	// No publish here. RevokeDelegation enqueued authority.revoked in the same
	// transaction as the status change; internal/outbox delivers it.
	h.countRevoke(telemetry.RevokeRevoked)
	writeJSON(w, http.StatusOK, updated)
}

func (h *Handler) countRevokeStoreErr(err error) {
	switch {
	case errors.Is(err, domain.ErrDelegationNotFound):
		h.countRevoke(telemetry.RevokeNotFound)
	case errors.Is(err, domain.ErrInvalidTransition):
		// The grant is REVOKED or EXPIRED already. Counted apart from an error
		// because it is neither a fault nor a refusal: it is a correct answer
		// that the caller is late, and a rising rate of it means two operators
		// are chasing the same delegation.
		h.countRevoke(telemetry.RevokeAlreadyTerminal)
	default:
		h.countRevoke(telemetry.RevokeUnavailable)
	}
}

// ── Helpers ────────────────────────────────────────────────────────────────

// sweepExpired lazily flips any due delegations to EXPIRED. Errors are logged,
// not surfaced — a failed sweep must not block the read it is piggybacking on.
//
// authority.expired is enqueued by ExpireDue inside the sweep's own
// transaction. That atomicity matters more here than on the other two events:
// the flip happens exactly once, so an event published separately and lost
// could never be regenerated — the next sweep finds no ACTIVE row left to flip.
func (h *Handler) sweepExpired(ctx context.Context) {
	expired, err := h.store.ExpireDue(ctx)
	if err != nil {
		h.log.Error("failed to sweep expired delegations", zap.Error(err))
		return
	}
	if h.metrics != nil && len(expired) > 0 {
		h.metrics.Expiries.Add(float64(len(expired)))
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
