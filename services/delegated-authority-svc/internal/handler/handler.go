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
	"zoiko.io/delegated-authority-svc/internal/store"
	"zoiko.io/delegated-authority-svc/internal/telemetry"
)

type Store interface {
	CreateDelegation(ctx context.Context, d *domain.DelegationGrant) (created bool, err error)
	ExpireDue(ctx context.Context) ([]domain.DelegationGrant, error)
	GetDelegation(ctx context.Context, delegationID string) (*domain.DelegationGrant, error)
	ListDelegations(ctx context.Context, f domain.ListDelegationsFilter) ([]domain.DelegationGrant, error)
	// Transition applies a protected lifecycle change (activate, suspend,
	// resume, revoke, extend) against the version the caller read.
	Transition(ctx context.Context, delegationID string, kind store.TransitionKind, in domain.TransitionInput) (*domain.DelegationGrant, error)
	// ListEffectiveAsOf reconstructs the grants effective at a past instant.
	ListEffectiveAsOf(ctx context.Context, f domain.ListDelegationsFilter, asOf time.Time) ([]domain.DelegationGrant, error)

	// CheckOverlap returns an error if an ACTIVE delegation already exists
	// for the same (tenant, legal_entity, delegate, action_type) with an
	// overlapping time window. Used for pre-insert validation to give a
	// clear 409 rather than a raw DB error.
	// excludeCorrelationID passes an idempotent replay; excludeDelegationID
	// passes the grant itself when it is extended, activated or resumed.
	CheckOverlap(ctx context.Context, tenantID, legalEntityID, delegatePrincipalID, actionType string, effectiveFrom, effectiveTo time.Time, excludeCorrelationID, excludeDelegationID string) error

	// RecordRefusedEscalation records a refused escalation attempt for audit.
	// ORG-06 §4.2: "Every refused escalation attempt leaves durable evidence."
	RecordRefusedEscalation(ctx context.Context, r *domain.RefusedEscalation) error

	// ExplainDelegationChain returns the chain of delegations from a starting
	// principal to a target principal for a specific action_type on a legal
	// entity. Each step shows who delegated to whom, the action type, and the
	// time window. Used for audit and debugging.
	ExplainDelegationChain(ctx context.Context, tenantID, legalEntityID, startPrincipalID, targetPrincipalID, actionType string) ([]domain.DelegationChainStep, error)
}

// AuthZClient is used twice, for two different purposes: (1) the normal
// gate on whether the caller may manage delegations at all, and (2) the
// platform's core delegation invariant — whether the delegator actually
// holds the authority being delegated.
type AuthZClient interface {
	CheckAllowed(ctx context.Context, principalID, legalEntityID, actionType string) error
	// CheckAllowedAtLimit asks whether the principal may perform the action at
	// the given monetary ceiling, with an authority limit required.
	CheckAllowedAtLimit(ctx context.Context, principalID, legalEntityID, actionType, amount, currency string) error
	// CheckHeldInOwnRight is CheckAllowed that also refuses, with
	// ErrDelegatorAuthorityDelegated, an action held only by delegation.
	CheckHeldInOwnRight(ctx context.Context, principalID, legalEntityID, actionType string) error
}

// SoDClient checks whether a proposed delegation would create a
// segregation-of-duties conflict. This is the ORG-06 §4.6 invariant
// "Cannot delegate around SoD" — authorization alone cannot see a duties
// conflict, so we must consult the SoD engine in authorization-svc.
type SoDClient interface {
	// CheckConflict returns an error if the proposed delegation (delegator
	// granting action_type to delegate) would create a SoD conflict, given
	// the delegate's existing grants. The legal_entity_id scopes the check.
	CheckConflict(ctx context.Context, tenantID, legalEntityID, delegatorPrincipalID, delegatePrincipalID, actionType string) error
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
	sod     SoDClient
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
func New(store Store, authz AuthZClient, sod SoDClient, log *zap.Logger, metrics *telemetry.Domain) *Handler {
	return &Handler{store: store, authz: authz, sod: sod, log: log, metrics: metrics}
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
		r.Post("/{delegation_id}/extend", h.ExtendDelegation)
		r.Post("/{delegation_id}/activate", h.ActivateDelegation)
		r.Post("/{delegation_id}/suspend", h.SuspendDelegation)
		r.Post("/{delegation_id}/resume", h.ResumeDelegation)
		r.Get("/explain", h.ExplainDelegationChain)
	})
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
	case "", string(domain.DelegationStatusProposed), string(domain.DelegationStatusActive), string(domain.DelegationStatusSuspended),
		string(domain.DelegationStatusRevoked), string(domain.DelegationStatusExpired):
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

	// GetEffectiveDelegations as of an instant: ?as_of=<RFC 3339>. Answered
	// from the append-only history, so a grant revoked since is still shown
	// as effective then.
	if v := r.URL.Query().Get("as_of"); v != "" {
		asOf, err := time.Parse(time.RFC3339, v)
		if err != nil {
			writeError(w, http.StatusBadRequest, "invalid_as_of", "as_of must be an RFC 3339 timestamp")
			return
		}
		if f.Status != "" {
			writeError(w, http.StatusBadRequest, "invalid_filter", "as_of answers what was effective then; it cannot be combined with status")
			return
		}
		list, err := h.store.ListEffectiveAsOf(r.Context(), f, asOf)
		if err != nil {
			h.writeStoreErr(w, err)
			return
		}
		if list == nil {
			list = []domain.DelegationGrant{}
		}
		writeJSON(w, http.StatusOK, list)
		return
	}

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
	// A party to the grant may read it, as the list already allows; anyone
	// else needs DELEGATION_VIEW on its entity. A refused read is answered
	// exactly like a missing one. It used to be 403 for an id that exists and
	// 404 for one that does not, so any caller in the tenant could learn which
	// delegation ids were real.
	if principalID == d.DelegatorPrincipalID || principalID == d.DelegatePrincipalID || principalID == d.CreatedByPrincipalID {
		h.countRead(telemetry.ReadScopeSelf)
		writeJSON(w, http.StatusOK, d)
		return
	}
	if err := h.checkAllowed(r.Context(), principalID, d.LegalEntityID, actionDelegationView); err != nil {
		if errors.Is(err, domain.ErrAuthorizationDenied) {
			writeError(w, http.StatusNotFound, "not_found", string(domain.ErrDelegationNotFound))
			return
		}
		h.writeAuthzErr(w, err)
		return
	}
	h.countRead(telemetry.ReadScopeEntity)
	writeJSON(w, http.StatusOK, d)
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

// ExplainDelegationChain returns the delegation chain from a start principal
// to a target principal for a specific action_type on a legal entity.
// GET /v1/delegations/explain?legal_entity_id=...&start_principal_id=...&target_principal_id=...&action_type=...
func (h *Handler) ExplainDelegationChain(w http.ResponseWriter, r *http.Request) {
	if _, ok := h.requireTenant(w, r); !ok {
		return
	}

	legalEntityID := r.URL.Query().Get("legal_entity_id")
	startPrincipalID := r.URL.Query().Get("start_principal_id")
	targetPrincipalID := r.URL.Query().Get("target_principal_id")
	actionType := r.URL.Query().Get("action_type")

	if legalEntityID == "" || startPrincipalID == "" || targetPrincipalID == "" || actionType == "" {
		writeError(w, http.StatusBadRequest, "missing_fields", "legal_entity_id, start_principal_id, target_principal_id, action_type are required")
		return
	}

	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}

	// Check authorization - requires DELEGATION_VIEW on the entity
	if err := h.checkAllowed(r.Context(), principalID, legalEntityID, actionDelegationView); err != nil {
		if errors.Is(err, domain.ErrAuthorizationDenied) {
			writeError(w, http.StatusForbidden, "forbidden", "DELEGATION_VIEW required")
		} else {
			h.writeAuthzErr(w, err)
		}
		return
	}

	tenantID := svcmiddleware.TenantFromContext(r.Context())
	chain, err := h.store.ExplainDelegationChain(r.Context(), tenantID, legalEntityID, startPrincipalID, targetPrincipalID, actionType)
	if err != nil {
		h.writeStoreErr(w, err)
		return
	}

	if chain == nil {
		chain = []domain.DelegationChainStep{}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"chain": chain,
		"found": len(chain) > 0,
	})
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

// recordRefusedEscalation durably records a refused attempt to grant
// authority (ORG-06 §4.6 evidence; GOV-07). The envelope fields come from the
// request itself. A failed write is logged and counted, never silently lost
// behind the refusal the caller still receives.
func (h *Handler) recordRefusedEscalation(ctx context.Context, reason string, req *domain.CreateDelegationRequest, principalID string, r *http.Request) {
	if h.store == nil {
		return
	}
	refusal := &domain.RefusedEscalation{
		RefusedID:            uuid.NewString(),
		TenantID:             svcmiddleware.TenantFromContext(ctx),
		LegalEntityID:        req.LegalEntityID,
		CallerPrincipalID:    principalID,
		DelegatorPrincipalID: req.DelegatorPrincipalID,
		DelegatePrincipalID:  req.DelegatePrincipalID,
		ActionType:           req.ActionType,
		EffectiveFrom:        req.EffectiveFrom,
		EffectiveTo:          req.EffectiveTo,
		RefusalReason:        reason,
		CorrelationID:        req.CorrelationID,
		IdempotencyKey:       r.Header.Get("Idempotency-Key"),
		RequestID:            r.Header.Get("X-Request-Id"),
		SourceChannel:        r.Header.Get("X-Source-Channel"),
		RefusedAt:            time.Now().UTC(),
	}
	if err := h.store.RecordRefusedEscalation(ctx, refusal); err != nil {
		h.log.Error("failed to record refused escalation", zap.Error(err), zap.String("reason", reason))
		if h.metrics != nil {
			h.metrics.RefusedEscalations.WithLabelValues("record_failed").Inc()
		}
	}
	if h.metrics != nil {
		// A fixed vocabulary (the database CHECK lists it), so a bounded label.
		h.metrics.RefusedEscalations.WithLabelValues(reason).Inc()
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
	case errors.Is(err, domain.ErrVersionRequired):
		writeError(w, http.StatusPreconditionRequired, "version_required", err.Error())
	case errors.Is(err, domain.ErrVersionMismatch):
		writeError(w, http.StatusConflict, "version_mismatch", err.Error())
	case errors.Is(err, domain.ErrLapsed):
		writeError(w, http.StatusConflict, "lapsed", err.Error())
	case errors.Is(err, domain.ErrCannotExtend):
		writeError(w, http.StatusConflict, "cannot_extend", err.Error())
	case errors.Is(err, domain.ErrOverlapConflict):
		writeError(w, http.StatusConflict, "overlap_conflict", err.Error())
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
