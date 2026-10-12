package handler

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"go.uber.org/zap"

	"zoiko.io/delegated-authority-svc/internal/domain"
	svcmiddleware "zoiko.io/delegated-authority-svc/internal/middleware"
	"zoiko.io/delegated-authority-svc/internal/store"
	"zoiko.io/delegated-authority-svc/internal/telemetry"
)

// ── POST /v1/delegations ──────────────────────────────────────────────────────

// CreateDelegation records a delegation of authority (ORG-06
// DelegateAuthority).
//
// A delegator delegating their OWN authority is the approval, so the grant is
// ACTIVE at once (approval DELEGATOR_SELF). Anything else — an administrator
// creating a grant on someone else's behalf, or a delegator asking for
// propose=true — is PROPOSED, and confers nothing until the delegator, or an
// administrator who is neither its maker nor its delegate, activates it
// (maker-checker; ORG-06 "Proposed → Active").
//
// The grant must not exceed the delegator: they must hold the action, a
// monetary ceiling must be within their own authority limit (negative case
// 11), and the delegate must not end up holding a conflicting duty (SoD).
// Every refusal of an attempted escalation is recorded durably.
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
	// Identity first, so every refusal recorded below names a real caller.
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		h.countGrant(telemetry.GrantIdentityMissing)
		return
	}
	if req.LegalEntityID == "" || req.DelegatorPrincipalID == "" || req.DelegatePrincipalID == "" || req.ActionType == "" ||
		req.CorrelationID == "" || strings.TrimSpace(req.Reason) == "" {
		h.countGrant(telemetry.GrantInvalidRequest)
		writeError(w, http.StatusBadRequest, "missing_fields",
			"legal_entity_id, delegator_principal_id, delegate_principal_id, action_type, correlation_id and reason are required")
		return
	}
	refuse := func(reason string) { h.recordRefusedEscalation(r.Context(), reason, &req, principalID, r) }

	if req.DelegatorPrincipalID == req.DelegatePrincipalID {
		h.countGrant(telemetry.GrantDelegateIsDelegator)
		refuse("delegate_is_delegator")
		writeError(w, http.StatusBadRequest, "delegate_is_delegator", string(domain.ErrDelegateIsDelegator))
		return
	}
	if !req.EffectiveTo.After(req.EffectiveFrom) {
		h.countGrant(telemetry.GrantInvalidWindow)
		refuse("invalid_window")
		writeError(w, http.StatusBadRequest, "invalid_time_window", string(domain.ErrInvalidTimeWindow))
		return
	}
	if !req.EffectiveTo.After(time.Now()) {
		h.countGrant(telemetry.GrantInvalidWindow)
		refuse("invalid_window")
		writeError(w, http.StatusBadRequest, "invalid_time_window", "effective_to must be in the future")
		return
	}
	if err := domain.ValidateLimit(req.AuthorityLimitCents, req.AuthorityLimitCurrency, req.AuthorityLimitQuantity); err != nil {
		h.countGrant("invalid_limit")
		refuse("invalid_limit")
		writeError(w, http.StatusBadRequest, "invalid_limit", err.Error())
		return
	}

	if err := h.checkAllowed(r.Context(), principalID, req.LegalEntityID, actionDelegationCreate); err != nil {
		if errors.Is(err, domain.ErrAuthorizationDenied) {
			h.countGrant(telemetry.GrantNoCreateGrant)
			refuse("no_create_grant")
		} else {
			h.countGrant(telemetry.GrantAuthzUnavailable)
		}
		h.writeAuthzErr(w, err)
		return
	}

	// Bind the delegator to the caller: a principal may only give away
	// authority that is theirs to give. Administering delegations between
	// other people is allowed under DELEGATION_ADMINISTER — as a proposal —
	// but routing someone else's authority to yourself never is.
	onBehalf := req.DelegatorPrincipalID != principalID
	if onBehalf {
		if req.DelegatePrincipalID == principalID {
			h.countGrant(telemetry.GrantSelfDealing)
			refuse("self_dealing")
			writeError(w, http.StatusForbidden, "self_dealing", string(domain.ErrSelfDealing))
			return
		}
		if err := h.checkAllowed(r.Context(), principalID, req.LegalEntityID, actionDelegationAdminister); err != nil {
			if errors.Is(err, domain.ErrAuthorizationDenied) {
				h.countGrant(telemetry.GrantDelegatorMismatch)
				refuse("delegator_mismatch")
				writeError(w, http.StatusForbidden, "delegator_mismatch", string(domain.ErrDelegatorMismatch))
				return
			}
			h.countGrant(telemetry.GrantAuthzUnavailable)
			h.writeAuthzErr(w, err)
			return
		}
	}

	g := grantFacts{
		tenantID: svcmiddleware.TenantFromContext(r.Context()), legalEntityID: req.LegalEntityID,
		delegator: req.DelegatorPrincipalID, delegate: req.DelegatePrincipalID, actionType: req.ActionType,
		from: req.EffectiveFrom, to: req.EffectiveTo, cents: req.AuthorityLimitCents, currency: req.AuthorityLimitCurrency,
	}
	if code, status, err := h.authorizeGrant(r.Context(), g); err != nil {
		if code != "" {
			refuse(code)
		}
		h.countGrant(code)
		writeError(w, status, nonEmpty(code, "authz_unavailable"), err.Error())
		return
	}
	if err := h.store.CheckOverlap(r.Context(), g.tenantID, g.legalEntityID, g.delegate, g.actionType, g.from, g.to, req.CorrelationID, ""); err != nil {
		if errors.Is(err, domain.ErrOverlapConflict) {
			h.countGrant(telemetry.GrantOverlapConflict)
			refuse("overlap_conflict")
			writeError(w, http.StatusConflict, "overlap_conflict", err.Error())
			return
		}
		h.countGrant(telemetry.GrantStoreUnavailable)
		h.writeStoreErr(w, err)
		return
	}

	now := time.Now().UTC()
	d := &domain.DelegationGrant{
		DelegationID: uuid.NewString(), TenantID: g.tenantID, LegalEntityID: req.LegalEntityID,
		DelegatorPrincipalID: req.DelegatorPrincipalID, DelegatePrincipalID: req.DelegatePrincipalID,
		ActionType: req.ActionType, EffectiveFrom: req.EffectiveFrom, EffectiveTo: req.EffectiveTo,
		Status: domain.DelegationStatusProposed, CreatedByPrincipalID: principalID, CorrelationID: req.CorrelationID,
		CreatedAt: now, UpdatedAt: now, AuthorityLimitCents: req.AuthorityLimitCents,
		AuthorityLimitCurrency: req.AuthorityLimitCurrency, AuthorityLimitQuantity: req.AuthorityLimitQuantity,
		Reason: strings.TrimSpace(req.Reason),
	}
	if !onBehalf && !req.Propose {
		method := domain.ApprovalDelegatorSelf
		d.Status, d.ApprovedByPrincipalID, d.ApprovedAt, d.ApprovalMethod = domain.DelegationStatusActive, &principalID, &now, &method
	}

	created, err := h.store.CreateDelegation(r.Context(), d)
	if err != nil {
		if errors.Is(err, domain.ErrCorrelationReused) {
			h.countGrant(telemetry.GrantInvalidRequest)
			writeError(w, http.StatusConflict, "correlation_reused", err.Error())
			return
		}
		if errors.Is(err, domain.ErrOverlapConflict) {
			h.countGrant(telemetry.GrantOverlapConflict)
			refuse("overlap_conflict")
			writeError(w, http.StatusConflict, "overlap_conflict", err.Error())
			return
		}
		h.countGrant(telemetry.GrantStoreUnavailable)
		h.log.Error("failed to create delegation", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "store_unavailable", err.Error())
		return
	}
	// 201 for a grant written now, 200 for a replay of one that already was:
	// "you have just granted authority" and "this was granted days ago" are
	// different facts.
	if !created {
		h.countGrant(telemetry.GrantReplayed)
		writeJSON(w, http.StatusOK, d)
		return
	}
	if d.Status == domain.DelegationStatusProposed {
		h.countGrant("proposed")
	} else {
		h.countGrant(telemetry.GrantCreated)
	}
	writeJSON(w, http.StatusCreated, d)
}

// grantFacts is what the "does not exceed the delegator" checks need.
type grantFacts struct {
	tenantID, legalEntityID, delegator, delegate, actionType string
	from, to                                                 time.Time
	cents                                                    *int64
	currency                                                 *string
}

// authorizeGrant is the core invariant, run on every path that makes or keeps
// a grant ACTIVE — create, activate, resume and extend — because the
// delegator's authority, limit and the delegate's duties can all change after
// the grant was first made. It returns the refusal code, the HTTP status and
// the error; an empty code means the authority could not be consulted.
func (h *Handler) authorizeGrant(ctx context.Context, g grantFacts) (string, int, error) {
	// The delegator must hold the action in their own right: authority held
	// only by delegation confers nothing when passed on (authorization-svc
	// resolves a delegation through the delegator's own roles), so it is
	// refused here rather than recorded as a grant that grants nothing.
	if err := h.authz.CheckHeldInOwnRight(ctx, g.delegator, g.legalEntityID, g.actionType); err != nil {
		h.countAuthz("DELEGATED_ACTION", err)
		if errors.Is(err, domain.ErrDelegatorAuthorityDelegated) {
			return "delegator_authority_delegated", http.StatusForbidden, err
		}
		if errors.Is(err, domain.ErrAuthorizationDenied) {
			return "delegator_lacks_authority", http.StatusForbidden, domain.ErrDelegatorLacksAuthority
		}
		return "", http.StatusServiceUnavailable, err
	}
	h.countAuthz("DELEGATED_ACTION", nil)

	// ORG-06 negative case 11: a monetary ceiling is delegated only if the
	// delegator's own authority limit covers it.
	if g.cents != nil && g.currency != nil {
		amount := domain.FormatMinorUnits(*g.cents, *g.currency)
		if err := h.authz.CheckAllowedAtLimit(ctx, g.delegator, g.legalEntityID, g.actionType, amount, *g.currency); err != nil {
			if errors.Is(err, domain.ErrAuthorizationDenied) {
				return "delegator_exceeds_limit", http.StatusForbidden, domain.ErrDelegatorExceedsLimit
			}
			return "", http.StatusServiceUnavailable, err
		}
	}

	// "Cannot delegate around SoD": the delegate must not end up holding a
	// conflicting pair. Unavailable refuses — never permits.
	if h.sod == nil {
		return "", http.StatusServiceUnavailable, errors.New("segregation-of-duties engine not configured")
	}
	if err := h.sod.CheckConflict(ctx, g.tenantID, g.legalEntityID, g.delegator, g.delegate, g.actionType); err != nil {
		if errors.Is(err, domain.ErrSODConflict) {
			return "sod_conflict", http.StatusForbidden, err
		}
		h.log.Error("sod check unavailable", zap.Error(err))
		return "", http.StatusServiceUnavailable, err
	}
	return "", 0, nil
}

func nonEmpty(s, def string) string {
	if s == "" {
		return def
	}
	return s
}

// ── lifecycle transitions ─────────────────────────────────────────────────────

// expectedVersion reads the version the caller last saw, from ?expected_version
// or If-Match. ORG-06: "all protected changes require authoritative current
// version" — so it is required, and a malformed value is an error rather than
// silently "no check", which is what it used to be.
func expectedVersion(w http.ResponseWriter, r *http.Request) (int64, bool) {
	v := r.URL.Query().Get("expected_version")
	if v == "" {
		v = strings.Trim(strings.TrimPrefix(r.Header.Get("If-Match"), "W/"), `"`)
	}
	if v == "" {
		writeError(w, http.StatusPreconditionRequired, "version_required", string(domain.ErrVersionRequired))
		return 0, false
	}
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil || n <= 0 {
		writeError(w, http.StatusBadRequest, "invalid_expected_version", "expected_version must be a positive integer")
		return 0, false
	}
	return n, true
}

// transitionPrelude does what every transition starts with: tenant, version,
// body, identity, and the grant as it stands now.
func (h *Handler) transitionPrelude(w http.ResponseWriter, r *http.Request, needReason bool) (*domain.DelegationGrant, string, int64, string, bool) {
	if _, ok := h.requireTenant(w, r); !ok {
		return nil, "", 0, "", false
	}
	version, ok := expectedVersion(w, r)
	if !ok {
		return nil, "", 0, "", false
	}
	var body domain.TransitionRequest
	if r.ContentLength != 0 {
		if !decodeJSON(w, r, &body) {
			return nil, "", 0, "", false
		}
	}
	reason := strings.TrimSpace(body.Reason)
	if needReason && reason == "" {
		writeError(w, http.StatusBadRequest, "reason_required", string(domain.ErrReasonRequired))
		return nil, "", 0, "", false
	}
	principalID, ok := h.requirePrincipal(w, r)
	if !ok {
		return nil, "", 0, "", false
	}
	// A transition acts on the grant as it is NOW: one past its window is
	// EXPIRED, not something to activate, suspend, resume or revoke.
	h.sweepExpired(r.Context())
	d, err := h.store.GetDelegation(r.Context(), chi.URLParam(r, "delegation_id"))
	if err != nil {
		h.writeStoreErr(w, err)
		return nil, "", 0, "", false
	}
	return d, principalID, version, reason, true
}

func (h *Handler) apply(w http.ResponseWriter, r *http.Request, d *domain.DelegationGrant, kind store.TransitionKind, in domain.TransitionInput) bool {
	updated, err := h.store.Transition(r.Context(), d.DelegationID, kind, in)
	if err != nil {
		h.writeStoreErr(w, err)
		return false
	}
	writeJSON(w, http.StatusOK, updated)
	return true
}

// keepWithinDelegator re-runs the core invariant and the overlap check for a
// grant about to be (or stay) ACTIVE over [from, to).
func (h *Handler) keepWithinDelegator(w http.ResponseWriter, r *http.Request, d *domain.DelegationGrant, caller string, to time.Time) bool {
	g := grantFacts{tenantID: d.TenantID, legalEntityID: d.LegalEntityID, delegator: d.DelegatorPrincipalID,
		delegate: d.DelegatePrincipalID, actionType: d.ActionType, from: d.EffectiveFrom, to: to,
		cents: d.AuthorityLimitCents, currency: d.AuthorityLimitCurrency}
	if code, status, err := h.authorizeGrant(r.Context(), g); err != nil {
		if code != "" {
			h.recordRefusedEscalation(r.Context(), code, requestOf(d, to), caller, r)
		}
		writeError(w, status, nonEmpty(code, "authz_unavailable"), err.Error())
		return false
	}
	if err := h.store.CheckOverlap(r.Context(), d.TenantID, d.LegalEntityID, d.DelegatePrincipalID, d.ActionType, d.EffectiveFrom, to, "", d.DelegationID); err != nil {
		if errors.Is(err, domain.ErrOverlapConflict) {
			h.recordRefusedEscalation(r.Context(), "overlap_conflict", requestOf(d, to), caller, r)
			writeError(w, http.StatusConflict, "overlap_conflict", err.Error())
			return false
		}
		h.writeStoreErr(w, err)
		return false
	}
	return true
}

// segregatedFromMaker refuses the maker of an on-behalf grant acting alone to
// widen it. Such a grant needed an independent approver to become ACTIVE;
// without this its maker could extend it, or resume it after somebody else
// suspended it, holding nothing but DELEGATION_ADMINISTER — approving their
// own proposal after all (ORG-06 "self-approval prohibited"). The delegator,
// whose authority it is, is not held to this; callers check that first.
func (h *Handler) segregatedFromMaker(w http.ResponseWriter, r *http.Request, d *domain.DelegationGrant, caller string) bool {
	if caller != d.CreatedByPrincipalID {
		return true
	}
	h.recordRefusedEscalation(r.Context(), "approval_not_segregated", requestOf(d, d.EffectiveTo), caller, r)
	writeError(w, http.StatusForbidden, "approval_not_segregated", string(domain.ErrApprovalNotSegregated))
	return false
}

func requestOf(d *domain.DelegationGrant, to time.Time) *domain.CreateDelegationRequest {
	return &domain.CreateDelegationRequest{LegalEntityID: d.LegalEntityID, DelegatorPrincipalID: d.DelegatorPrincipalID,
		DelegatePrincipalID: d.DelegatePrincipalID, ActionType: d.ActionType, EffectiveFrom: d.EffectiveFrom, EffectiveTo: to,
		CorrelationID: d.CorrelationID}
}

// ActivateDelegation approves a PROPOSED grant. The approver is the delegator
// (DELEGATOR_APPROVAL) or an administrator who is neither the maker nor the
// delegate (ADMINISTRATOR_APPROVAL); the database enforces the same rule.
// POST /v1/delegations/{id}/activate?expected_version=N
func (h *Handler) ActivateDelegation(w http.ResponseWriter, r *http.Request) {
	d, caller, version, reason, ok := h.transitionPrelude(w, r, false)
	if !ok {
		return
	}
	method := domain.ApprovalDelegator
	if caller != d.DelegatorPrincipalID {
		if caller == d.CreatedByPrincipalID || caller == d.DelegatePrincipalID {
			h.recordRefusedEscalation(r.Context(), "approval_not_segregated", requestOf(d, d.EffectiveTo), caller, r)
			writeError(w, http.StatusForbidden, "approval_not_segregated", string(domain.ErrApprovalNotSegregated))
			return
		}
		if err := h.checkAllowed(r.Context(), caller, d.LegalEntityID, actionDelegationAdminister); err != nil {
			h.writeAuthzErr(w, err)
			return
		}
		method = domain.ApprovalAdministrator
	}
	if d.Status != domain.DelegationStatusProposed {
		writeError(w, http.StatusConflict, "invalid_transition", "only a PROPOSED delegation can be activated")
		return
	}
	if !h.keepWithinDelegator(w, r, d, caller, d.EffectiveTo) {
		return
	}
	h.apply(w, r, d, store.Activate, domain.TransitionInput{ExpectedVersion: version, ActorPrincipalID: caller,
		Reason: reason, ApprovalMethod: method})
}

// SuspendDelegation takes an ACTIVE grant out of effect until resumed. The
// delegator, or a holder of DELEGATION_REVOKE on the entity, may suspend.
// POST /v1/delegations/{id}/suspend?expected_version=N  {"reason": "..."}
func (h *Handler) SuspendDelegation(w http.ResponseWriter, r *http.Request) {
	d, caller, version, reason, ok := h.transitionPrelude(w, r, true)
	if !ok {
		return
	}
	if caller != d.DelegatorPrincipalID {
		if err := h.checkAllowed(r.Context(), caller, d.LegalEntityID, actionDelegationRevoke); err != nil {
			h.writeAuthzErr(w, err)
			return
		}
	}
	h.apply(w, r, d, store.Suspend, domain.TransitionInput{ExpectedVersion: version, ActorPrincipalID: caller, Reason: reason})
}

// ResumeDelegation returns a SUSPENDED grant to ACTIVE, after re-checking that
// it still does not exceed the delegator. The delegator, or a holder of
// DELEGATION_ADMINISTER who is not the delegate, may resume.
// POST /v1/delegations/{id}/resume?expected_version=N
func (h *Handler) ResumeDelegation(w http.ResponseWriter, r *http.Request) {
	d, caller, version, reason, ok := h.transitionPrelude(w, r, false)
	if !ok {
		return
	}
	if caller != d.DelegatorPrincipalID {
		if caller == d.DelegatePrincipalID {
			writeError(w, http.StatusForbidden, "self_dealing", "the delegate may not resume their own delegation")
			return
		}
		if !h.segregatedFromMaker(w, r, d, caller) {
			return
		}
		if err := h.checkAllowed(r.Context(), caller, d.LegalEntityID, actionDelegationAdminister); err != nil {
			h.writeAuthzErr(w, err)
			return
		}
	}
	if d.Status != domain.DelegationStatusSuspended {
		writeError(w, http.StatusConflict, "invalid_transition", "only a SUSPENDED delegation can be resumed")
		return
	}
	if !h.keepWithinDelegator(w, r, d, caller, d.EffectiveTo) {
		return
	}
	h.apply(w, r, d, store.Resume, domain.TransitionInput{ExpectedVersion: version, ActorPrincipalID: caller, Reason: reason})
}

// RevokeDelegation ends a PROPOSED, ACTIVE or SUSPENDED grant for good, with a
// stated reason (§4.6 evidence: revocation reason).
// POST /v1/delegations/{id}/revoke?expected_version=N  {"reason": "..."}
func (h *Handler) RevokeDelegation(w http.ResponseWriter, r *http.Request) {
	d, caller, version, reason, ok := h.transitionPrelude(w, r, true)
	if !ok {
		return
	}
	if err := h.checkAllowed(r.Context(), caller, d.LegalEntityID, actionDelegationRevoke); err != nil {
		if errors.Is(err, domain.ErrAuthorizationDenied) {
			h.countRevoke(telemetry.RevokeForbidden)
		} else {
			h.countRevoke(telemetry.RevokeUnavailable)
		}
		h.writeAuthzErr(w, err)
		return
	}
	updated, err := h.store.Transition(r.Context(), d.DelegationID, store.Revoke,
		domain.TransitionInput{ExpectedVersion: version, ActorPrincipalID: caller, Reason: reason})
	if err != nil {
		h.countRevokeStoreErr(err)
		h.writeStoreErr(w, err)
		return
	}
	h.countRevoke(telemetry.RevokeRevoked)
	writeJSON(w, http.StatusOK, updated)
}

// ExtendDelegation moves an ACTIVE grant's effective_to later (ORG-06
// ExtendDelegation), keeping its identity and evidence chain. The extended
// window must still not exceed the delegator, must not overlap another grant
// — the grant itself excluded, by id — and a grant whose window has already
// ended is not revived (no grace extension).
// POST /v1/delegations/{id}/extend?expected_version=N
// {"new_effective_to": "...", "correlation_id": "...", "reason": "..."}
func (h *Handler) ExtendDelegation(w http.ResponseWriter, r *http.Request) {
	if _, ok := h.requireTenant(w, r); !ok {
		return
	}
	version, ok := expectedVersion(w, r)
	if !ok {
		return
	}
	var req domain.ExtendDelegationRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if req.NewEffectiveTo.IsZero() || req.CorrelationID == "" || strings.TrimSpace(req.Reason) == "" {
		writeError(w, http.StatusBadRequest, "missing_fields", "new_effective_to, correlation_id and reason are required")
		return
	}
	caller, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	h.sweepExpired(r.Context())
	d, err := h.store.GetDelegation(r.Context(), chi.URLParam(r, "delegation_id"))
	if err != nil {
		h.writeStoreErr(w, err)
		return
	}
	if caller != d.DelegatorPrincipalID {
		if caller == d.DelegatePrincipalID {
			writeError(w, http.StatusForbidden, "self_dealing", "the delegate may not extend their own delegation")
			return
		}
		if !h.segregatedFromMaker(w, r, d, caller) {
			return
		}
		if err := h.checkAllowed(r.Context(), caller, d.LegalEntityID, actionDelegationAdminister); err != nil {
			if errors.Is(err, domain.ErrAuthorizationDenied) {
				writeError(w, http.StatusForbidden, "forbidden", "only the delegator or an administrator may extend a delegation")
			} else {
				h.writeAuthzErr(w, err)
			}
			return
		}
	}
	if d.Status != domain.DelegationStatusActive {
		writeError(w, http.StatusConflict, "cannot_extend", string(domain.ErrCannotExtend))
		return
	}
	if !req.NewEffectiveTo.After(d.EffectiveTo) {
		writeError(w, http.StatusBadRequest, "invalid_time_window", "new_effective_to must be after current effective_to")
		return
	}
	if !h.keepWithinDelegator(w, r, d, caller, req.NewEffectiveTo) {
		return
	}
	if h.apply(w, r, d, store.Extend, domain.TransitionInput{ExpectedVersion: version, ActorPrincipalID: caller,
		Reason: strings.TrimSpace(req.Reason), NewEffectiveTo: req.NewEffectiveTo}) {
		h.countGrant(telemetry.GrantExtended)
	}
}
